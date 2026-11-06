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

func TestPostgresConcurrentDuplicateWebhookPostsWriteOneAttempt(t *testing.T) {
	testDB := usePostgresIntegrationDB(t)
	saveTransport(t)
	httpClient.Transport = mockRoundTripper(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/v1/oauth2/token":
			return resp(http.StatusOK, `{"access_token":"integration-token"}`), nil
		case "/v1/notifications/verify-webhook-signature":
			return resp(http.StatusOK, `{"verification_status":"SUCCESS"}`), nil
		default:
			return resp(http.StatusNotFound, `{}`), nil
		}
	})

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	eventID, captureID, orderID := "integration-concurrent-webhook-"+suffix, "INTEGRATION-CONCURRENT-CAPTURE-"+suffix, "INTEGRATION-CONCURRENT-ORDER-"+suffix
	body := fmt.Sprintf(`{"id":%q,"event_type":"PAYMENT.CAPTURE.COMPLETED","resource":{"id":%q,"amount":{"value":"25.00","currency_code":"USD"},"supplementary_data":{"related_ids":{"order_id":%q}}}}`, eventID, captureID, orderID)
	t.Cleanup(func() {
		_, _ = testDB.ExecContext(context.Background(), `DELETE FROM attempts WHERE capture_id = $1`, captureID)
		_, _ = testDB.ExecContext(context.Background(), `DELETE FROM webhook_events WHERE event_id = $1`, eventID)
	})

	previousWriter := insertWebhookAttempt
	firstInsertEntered := make(chan struct{})
	releaseFirstInsert := make(chan struct{})
	var attemptCalls atomic.Int32
	insertWebhookAttempt = func(ctx context.Context, execer webhookExecer, attempt PaymentAttempt) error {
		if attemptCalls.Add(1) == 1 {
			close(firstInsertEntered)
			<-releaseFirstInsert
		}
		return previousWriter(ctx, execer, attempt)
	}
	t.Cleanup(func() {
		insertWebhookAttempt = previousWriter
		select {
		case <-releaseFirstInsert:
		default:
			close(releaseFirstInsert)
		}
	})

	handler := makePayPalWebhookHandler("client", "secret", "WH-INTEGRATION")
	responses := make(chan *httptest.ResponseRecorder, 2)
	deliver := func() {
		req := validWebhookRequest(body)
		req.URL.Path = "/webhooks/paypal"
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		responses <- w
	}
	go deliver()
	select {
	case <-firstInsertEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("first webhook did not reach attempt insertion")
	}
	go deliver()

	// Wait until the second request is visibly blocked on the first transaction's
	// SELECT ... FOR UPDATE before letting the first transaction commit.
	lockWaitCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		var waiting int
		err := testDB.QueryRowContext(lockWaitCtx, `SELECT COUNT(*) FROM pg_stat_activity
			WHERE pid <> pg_backend_pid() AND wait_event_type = 'Lock'
			AND query ILIKE '%SELECT event_type, payload, processed_at FROM webhook_events%'`).Scan(&waiting)
		if err != nil {
			t.Fatalf("check concurrent webhook row lock: %v", err)
		}
		if waiting > 0 {
			break
		}
		select {
		case <-lockWaitCtx.Done():
			t.Fatal("second webhook did not block on the receipt row lock")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if got := attemptCalls.Load(); got != 1 {
		t.Fatalf("attempt insert calls while first delivery holds the row lock = %d, want 1", got)
	}
	close(releaseFirstInsert)
	for i := 0; i < 2; i++ {
		select {
		case w := <-responses:
			if w.Code != http.StatusOK {
				t.Errorf("concurrent webhook response = %d %s, want 200", w.Code, w.Body.String())
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for concurrent webhook response")
		}
	}
	if got := attemptCalls.Load(); got != 1 {
		t.Fatalf("attempt insert calls = %d, want exactly one", got)
	}
	var attemptRows int
	if err := testDB.QueryRow(`SELECT COUNT(*) FROM attempts WHERE capture_id = $1 AND stage = 'webhook'`, captureID).Scan(&attemptRows); err != nil {
		t.Fatalf("count attempts from concurrent webhook posts: %v", err)
	}
	if attemptRows != 1 {
		t.Fatalf("attempt rows from two concurrent POSTs = %d, want exactly one", attemptRows)
	}
}

func TestPostgresDetectorsAndActionAudit(t *testing.T) {
	testDB := usePostgresIntegrationDB(t)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	orderID := "DETECT-ORDER-" + suffix
	capture1, capture2, repeatedID := "DETECT-CAP-A-"+suffix, "DETECT-CAP-B-"+suffix, "DETECT-CAP-X-"+suffix
	eventID := "DETECT-EVENT-" + suffix
	fingerprint := "action-test-" + suffix
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	t.Cleanup(func() {
		_, _ = testDB.ExecContext(context.Background(), `DELETE FROM actions WHERE finding_id IN (SELECT id FROM findings WHERE fingerprint = $1)`, fingerprint)
		_, _ = testDB.ExecContext(context.Background(), `DELETE FROM findings WHERE fingerprint = $1 OR fingerprint IN ($2, $3, $4)`,
			fingerprint, "duplicate_capture:order:"+fingerprintToken(orderID), "duplicate_capture:capture:"+fingerprintToken(repeatedID), "fee_mismatch:"+fingerprintToken(capture1))
		_, _ = testDB.ExecContext(context.Background(), `DELETE FROM attempts WHERE capture_id IN ($1, $2, $3)`, capture1, capture2, repeatedID)
		_, _ = testDB.ExecContext(context.Background(), `DELETE FROM webhook_events WHERE event_id = $1`, eventID)
	})

	for _, row := range []struct {
		orderID, captureID, outcome, stage string
	}{
		{orderID, capture1, "succeeded", "capture"},
		{orderID, capture2, "succeeded", "capture"},
		{"DETECT-REPEATED-" + suffix, repeatedID, "failed", "capture"},
		{"DETECT-REPEATED-" + suffix, repeatedID, "succeeded", "webhook"},
	} {
		if _, err := testDB.ExecContext(ctx, `INSERT INTO attempts
			(paypal_order_id, capture_id, stage, outcome, amount_minor, currency, is_synthetic)
			VALUES ($1, $2, $3, $4, 2500, 'USD', FALSE)`, row.orderID, row.captureID, row.stage, row.outcome); err != nil {
			t.Fatalf("insert detector attempt fixture: %v", err)
		}
	}
	payload := fmt.Sprintf(`{"id":%q,"event_type":"PAYMENT.CAPTURE.COMPLETED","resource":{"id":%q,"amount":{"value":"25.00","currency_code":"USD"},"seller_receivable_breakdown":{"paypal_fee":{"currency_code":"USD","value":"1.05"}}}}`, eventID, capture1)
	if _, err := testDB.ExecContext(ctx, `INSERT INTO webhook_events (event_id, event_type, resource_id, payload, processed_at)
		VALUES ($1, 'PAYMENT.CAPTURE.COMPLETED', $2, $3::jsonb, NOW())`, eventID, capture1, payload); err != nil {
		t.Fatalf("insert fee evidence fixture: %v", err)
	}
	drafts, err := runDeterministicDetectors(ctx, testDB)
	if err != nil {
		t.Fatalf("run deterministic detectors: %v", err)
	}
	for _, key := range []string{
		"duplicate_capture:order:" + fingerprintToken(orderID),
		"duplicate_capture:capture:" + fingerprintToken(repeatedID),
		"fee_mismatch:" + fingerprintToken(capture1),
	} {
		var id int64
		if err := testDB.QueryRowContext(ctx, `SELECT id FROM findings WHERE fingerprint = $1`, key).Scan(&id); err != nil {
			t.Fatalf("detector did not persist finding %s (drafts=%d): %v", key, len(drafts), err)
		}
	}
	var baseline leakageScore
	if err := testDB.QueryRowContext(ctx, leakageScoreQuery).Scan(&baseline.LostMinor, &baseline.RecoverableMinor, &baseline.RecoveredMinor); err != nil {
		t.Fatalf("read baseline leakage score: %v", err)
	}
	var findingID int64
	if err := testDB.QueryRowContext(ctx, `INSERT INTO findings (fingerprint, detector, amount_minor, currency, evidence)
		VALUES ($1, 'fee_mismatch', 200, 'USD', '{}'::jsonb) RETURNING id`, fingerprint).Scan(&findingID); err != nil {
		t.Fatalf("insert action finding: %v", err)
	}
	proposal := httptest.NewRecorder()
	proposalReq := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/findings/%d/action", findingID), strings.NewReader(`{"action_type":"record_recovery","description":"Record operator-confirmed recovery"}`))
	makeFindingActionHandler().ServeHTTP(proposal, proposalReq)
	if proposal.Code != http.StatusCreated {
		t.Fatalf("propose action response=%d %s, want 201", proposal.Code, proposal.Body.String())
	}
	var proposed actionResponse
	if err := json.Unmarshal(proposal.Body.Bytes(), &proposed); err != nil {
		t.Fatalf("decode proposed action: %v", err)
	}
	if proposed.Status != "proposed" || proposed.MoneyMoved || proposed.FindingID != findingID {
		t.Fatalf("proposal response = %+v", proposed)
	}
	approval := httptest.NewRecorder()
	approvalReq := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/actions/%d/approve", proposed.ID), nil)
	makeApproveActionHandler().ServeHTTP(approval, approvalReq)
	if approval.Code != http.StatusOK {
		t.Fatalf("approve action response=%d %s, want 200", approval.Code, approval.Body.String())
	}
	var executed actionResponse
	if err := json.Unmarshal(approval.Body.Bytes(), &executed); err != nil {
		t.Fatalf("decode executed action: %v", err)
	}
	if executed.Status != "executed" || executed.MoneyMoved || !strings.Contains(string(executed.Result), `"money_moved": false`) {
		t.Fatalf("executed action response = %+v", executed)
	}
	var findingStatus string
	if err := testDB.QueryRowContext(ctx, `SELECT status FROM findings WHERE id = $1`, findingID).Scan(&findingStatus); err != nil {
		t.Fatalf("read actioned finding: %v", err)
	}
	if findingStatus != "actioned" {
		t.Fatalf("finding status=%q, want actioned", findingStatus)
	}
	var after leakageScore
	if err := testDB.QueryRowContext(ctx, leakageScoreQuery).Scan(&after.LostMinor, &after.RecoverableMinor, &after.RecoveredMinor); err != nil {
		t.Fatalf("read final leakage score: %v", err)
	}
	if after.LostMinor != baseline.LostMinor || after.RecoverableMinor != baseline.RecoverableMinor || after.RecoveredMinor != baseline.RecoveredMinor+200 {
		t.Fatalf("leakage score before=%+v after=%+v; expected 200 minor units to move from open to actioned", baseline, after)
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
