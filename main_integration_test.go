package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func openPostgresIntegrationDB(t *testing.T) *sql.DB {
	t.Helper()
	dbURL := strings.TrimSpace(os.Getenv("PAYPAL_AI_AUDITOR_TEST_DATABASE_URL"))
	if dbURL == "" {
		if strings.EqualFold(strings.TrimSpace(os.Getenv("CI")), "true") {
			t.Fatal("PAYPAL_AI_AUDITOR_TEST_DATABASE_URL is required in CI")
		}
		t.Skip("set PAYPAL_AI_AUDITOR_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	testDB, err := sql.Open("postgres", dbURL)
	if err != nil {
		t.Fatalf("open integration database: %v", err)
	}
	testDB.SetMaxOpenConns(8)
	testDB.SetMaxIdleConns(4)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := testDB.PingContext(ctx); err != nil {
		testDB.Close()
		t.Fatalf("ping integration database: %v", err)
	}
	t.Cleanup(func() { testDB.Close() })
	return testDB
}

func usePostgresIntegrationDB(t *testing.T) *sql.DB {
	t.Helper()
	testDB := openPostgresIntegrationDB(t)
	previousDB := db
	db = testDB
	t.Cleanup(func() { db = previousDB })
	return testDB
}

func TestPostgresAdvisoryLockSerializesSameOrder(t *testing.T) {
	usePostgresIntegrationDB(t)
	const workers = 8
	lockKey := fmt.Sprintf("paypal-ai-auditor-integration-%d", time.Now().UnixNano())
	start := make(chan struct{})
	errors := make(chan error, workers)
	var active atomic.Int32
	var maxActive atomic.Int32
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	for i := 0; i < workers; i++ {
		go func() {
			<-start
			err := withPostgresOrderAdvisoryLock(ctx, lockKey, func(context.Context) error {
				current := active.Add(1)
				for previous := maxActive.Load(); current > previous && !maxActive.CompareAndSwap(previous, current); previous = maxActive.Load() {
				}
				time.Sleep(40 * time.Millisecond)
				active.Add(-1)
				return nil
			})
			errors <- err
		}()
	}
	close(start)
	for i := 0; i < workers; i++ {
		if err := <-errors; err != nil {
			t.Errorf("advisory lock worker: %v", err)
		}
	}
	if got := maxActive.Load(); got != 1 {
		t.Fatalf("maximum concurrent lock holders=%d, want 1", got)
	}
}

func TestPostgresLegacyMinorFallback(t *testing.T) {
	testDB := openPostgresIntegrationDB(t)
	query := `SELECT ` + amountMinorFallbackExpression + ` FROM (
		SELECT NULL::BIGINT AS amount_minor, 1234.50::DOUBLE PRECISION AS amount, 'USD'::TEXT AS currency
	) AS legacy_capture`
	var got int64
	if err := testDB.QueryRow(query).Scan(&got); err != nil {
		t.Fatalf("evaluate legacy fallback: %v", err)
	}
	if got != 123450 {
		t.Fatalf("legacy fallback returned %d, want 123450", got)
	}
}

func TestPostgresLockPersistenceFailureRetryReconciles(t *testing.T) {
	usePostgresIntegrationDB(t)
	setOrderLock(t, withPostgresOrderAdvisoryLock)
	saveTransport(t)
	completedOrder := `{"status":"COMPLETED","purchase_units":[{"payments":{"captures":[{"id":"CAPDBRETRY","status":"COMPLETED","create_time":"2026-11-06T12:00:00Z","amount":{"value":"25.00","currency_code":"USD"}}]}}]}`
	orderCompleted := false
	captureCalls := 0
	httpClient.Transport = mockRoundTripper(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodPost && req.URL.Path == "/v1/oauth2/token" {
			return resp(http.StatusOK, `{"access_token":"fake-token"}`), nil
		}
		if req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/v2/checkout/orders/") {
			if orderCompleted {
				return resp(http.StatusOK, completedOrder), nil
			}
			return resp(http.StatusOK, `{"status":"APPROVED"}`), nil
		}
		if req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/capture") {
			captureCalls++
			orderCompleted = true
			return resp(http.StatusOK, completedOrder), nil
		}
		return resp(http.StatusNotFound, `{}`), nil
	})

	persistCalls := 0
	storedRows := map[string]PaymentRecord{}
	previousPersist := persistCapture
	persistCapture = func(ctx context.Context, payment PaymentRecord) error {
		persistCalls++
		if persistCalls == 1 {
			return errors.New("simulated persistence failure")
		}
		storedRows[payment.ID] = payment
		return nil
	}
	t.Cleanup(func() { persistCapture = previousPersist })

	handler := makeCaptureOrderHandler("fake", "fake")
	request := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/capture-order", strings.NewReader(`{"orderID":"ORDER123"}`))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		return w
	}
	first := request()
	if first.Code != http.StatusInternalServerError || !strings.Contains(first.Body.String(), "capture_succeeded_persistence_pending") {
		t.Fatalf("first response = %d %s; want persistence-pending 500", first.Code, first.Body.String())
	}
	second := request()
	if second.Code != http.StatusOK {
		t.Fatalf("retry response = %d %s; want 200", second.Code, second.Body.String())
	}
	var payment PaymentRecord
	if err := json.Unmarshal(second.Body.Bytes(), &payment); err != nil {
		t.Fatalf("decode retry response: %v", err)
	}
	if payment.ID != "CAPDBRETRY" || captureCalls != 1 || persistCalls != 2 || len(storedRows) != 1 {
		t.Fatalf("payment=%+v captures=%d persists=%d rows=%d; want one capture, retry persistence, and one row", payment, captureCalls, persistCalls, len(storedRows))
	}
}

func TestPostgresWebhookFailureCanBeRetriedAndProcessedOnce(t *testing.T) {
	testDB := openPostgresIntegrationDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	eventID, captureID, orderID := "integration-webhook-"+suffix, "INTEGRATION-CAPTURE-"+suffix, "INTEGRATION-ORDER-"+suffix
	payload := []byte(fmt.Sprintf(`{"id":%q,"event_type":"PAYMENT.CAPTURE.COMPLETED","resource":{"id":%q,"amount":{"value":"25.00","currency_code":"USD"},"supplementary_data":{"related_ids":{"order_id":%q}}}}`, eventID, captureID, orderID))
	var event paypalWebhookEvent
	if err := json.Unmarshal(payload, &event); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = testDB.ExecContext(context.Background(), `DELETE FROM attempts WHERE capture_id = $1`, captureID)
		_, _ = testDB.ExecContext(context.Background(), `DELETE FROM webhook_events WHERE event_id = $1`, eventID)
	})
	previousWriter := insertWebhookAttempt
	t.Cleanup(func() { insertWebhookAttempt = previousWriter })
	insertWebhookAttempt = func(context.Context, webhookExecer, PaymentAttempt) error {
		return errors.New("simulated attempt persistence failure")
	}
	_, err := processWebhookReceipt(ctx, testDB, event, payload)
	if err == nil {
		t.Fatal("first processing should fail when the attempt insert is rejected")
	}
	insertWebhookAttempt = previousWriter
	var processedAt sql.NullTime
	var processError sql.NullString
	if err := testDB.QueryRowContext(ctx, `SELECT processed_at, process_error FROM webhook_events WHERE event_id = $1`, eventID).Scan(&processedAt, &processError); err != nil {
		t.Fatalf("read unprocessed receipt: %v", err)
	}
	if processedAt.Valid || !processError.Valid {
		t.Fatalf("after failed processing processed_at=%v process_error=%v; want NULL timestamp and recorded error", processedAt, processError)
	}
	if _, err := processWebhookReceipt(ctx, testDB, event, payload); err != nil {
		t.Fatalf("retry unprocessed duplicate: %v", err)
	}
	if _, err := processWebhookReceipt(ctx, testDB, event, payload); err != nil {
		t.Fatalf("ack processed duplicate: %v", err)
	}
	if err := testDB.QueryRowContext(ctx, `SELECT processed_at, process_error FROM webhook_events WHERE event_id = $1`, eventID).Scan(&processedAt, &processError); err != nil {
		t.Fatalf("read processed receipt: %v", err)
	}
	if !processedAt.Valid || processError.Valid {
		t.Fatalf("after successful retry processed_at=%v process_error=%v; want timestamp and NULL error", processedAt, processError)
	}
	var attemptCount int
	if err := testDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM attempts WHERE capture_id = $1 AND stage = 'webhook'`, captureID).Scan(&attemptCount); err != nil {
		t.Fatalf("count webhook attempts: %v", err)
	}
	if attemptCount != 1 {
		t.Fatalf("webhook attempt count=%d, want exactly one", attemptCount)
	}
}

func TestPostgresInsightEvidenceExcludesSyntheticAttempts(t *testing.T) {
	testDB := usePostgresIntegrationDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	captureID := fmt.Sprintf("INSGHT%d", time.Now().UnixNano())
	orderID := fmt.Sprintf("ORDR%d", time.Now().UnixNano())
	date := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := testDB.ExecContext(ctx, `INSERT INTO captures (id, amount, amount_minor, status, currency, date, is_live)
		VALUES ($1, 25.00, 2500, 'COMPLETED', 'USD', $2, TRUE)`, captureID, date)
	if err != nil {
		t.Fatalf("insert live capture fixture: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testDB.ExecContext(context.Background(), `DELETE FROM attempts WHERE capture_id = $1`, captureID)
		_, _ = testDB.ExecContext(context.Background(), `DELETE FROM captures WHERE id = $1`, captureID)
	})
	_, err = testDB.ExecContext(ctx, `INSERT INTO attempts
		(paypal_order_id, capture_id, stage, outcome, amount_minor, currency, is_synthetic)
		VALUES ($1, $2, 'capture', 'succeeded', 2500, 'USD', TRUE)`, orderID, captureID)
	if err != nil {
		t.Fatalf("insert synthetic attempt fixture: %v", err)
	}
	records := []PaymentRecord{{ID: captureID, AmountMinor: 2500, Status: "COMPLETED", Currency: "USD", Date: date, IsLive: true}}
	evidence, err := getInsightEvidence(ctx, records)
	if err != nil {
		t.Fatalf("load evidence for synthetic attempt: %v", err)
	}
	if evidence.CaptureCount != 0 {
		t.Fatalf("synthetic attempt entered LLM evidence: %+v", evidence)
	}
	if _, err := testDB.ExecContext(ctx, `UPDATE attempts SET is_synthetic = FALSE WHERE capture_id = $1`, captureID); err != nil {
		t.Fatalf("mark fixture attempt real: %v", err)
	}
	evidence, err = getInsightEvidence(ctx, records)
	if err != nil {
		t.Fatalf("load evidence for real attempt: %v", err)
	}
	if evidence.CaptureCount != 1 || evidence.Captures[0].OrderID != orderID || evidence.Captures[0].CaptureID != captureID {
		t.Fatalf("real attempt evidence = %+v, want fixture order/capture", evidence)
	}
}
