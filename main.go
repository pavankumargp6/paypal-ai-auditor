package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/joho/godotenv"
	_ "github.com/lib/pq"
)

const (
	expectedAmountMinor int64 = 2500
	expectedCurrency          = "USD"
)

const amountMinorFallbackExpression = `COALESCE(amount_minor, ROUND(amount::numeric * CASE
	WHEN UPPER(currency) IN ('JPY', 'HUF', 'TWD') THEN 1::numeric
	ELSE 100::numeric
END)::BIGINT)`

const liveRecordsQuery = `SELECT id, ` + amountMinorFallbackExpression + ` AS amount_minor, status, currency, date, is_live FROM captures ORDER BY date DESC`

const declineRateQuery = `SELECT COUNT(*) FILTER (WHERE reason_code IN ('INSTRUMENT_DECLINED', 'PAYER_CANNOT_PAY', 'PAYMENT_DENIED')), COUNT(*)
	FROM attempts
	WHERE stage = 'capture'
	  AND is_synthetic = FALSE
	  AND outcome IN ('succeeded', 'declined', 'failed')
	  AND reason_code IS DISTINCT FROM 'ORDER_ALREADY_CAPTURED'`

var errOrderAlreadyCaptured = errors.New("PayPal reports this order was already captured")

type PayPalErrorDetail struct {
	Issue       string `json:"issue"`
	Description string `json:"description"`
}

type PaymentAttempt struct {
	PayPalOrderID string
	CaptureID     string
	DebugID       string
	Stage         string
	Outcome       string
	ReasonCode    string
	ReasonDetail  string
	HTTPStatus    *int
	AmountMinor   int64
	Currency      string
	IsSynthetic   bool
}

// PayPalAPIError preserves structured response fields without retaining or logging
// the raw response body, which may contain data that should not be emitted.
type PayPalAPIError struct {
	HTTPStatus   int
	Name         string
	Message      string
	Details      []PayPalErrorDetail
	DebugID      string
	ReasonCode   string
	ReasonDetail string
}

func (e *PayPalAPIError) Error() string {
	message := fmt.Sprintf("PayPal API returned HTTP %d (%s): %s", e.HTTPStatus, e.ReasonCode, e.ReasonDetail)
	if e.DebugID != "" {
		message += " (debug_id=" + e.DebugID + ")"
	}
	return message
}

func newPayPalAPIError(status int, body []byte) *PayPalAPIError {
	var response struct {
		Name        string              `json:"name"`
		Message     string              `json:"message"`
		Error       string              `json:"error"`
		Description string              `json:"error_description"`
		Details     []PayPalErrorDetail `json:"details"`
		DebugID     string              `json:"debug_id"`
	}
	_ = json.Unmarshal(body, &response)
	if response.Name == "" {
		response.Name = response.Error
	}
	if response.Message == "" {
		response.Message = response.Description
	}
	apiErr := &PayPalAPIError{
		HTTPStatus: status,
		Name:       response.Name,
		Message:    response.Message,
		Details:    response.Details,
		DebugID:    response.DebugID,
		ReasonCode: "UNCLASSIFIED",
	}
	for _, detail := range response.Details {
		if detail.Issue != "" {
			apiErr.ReasonCode = detail.Issue
			apiErr.ReasonDetail = detail.Description
			break
		}
	}
	if apiErr.ReasonCode == "UNCLASSIFIED" && response.Name != "" {
		apiErr.ReasonCode = response.Name
	}
	if apiErr.ReasonDetail == "" {
		apiErr.ReasonDetail = response.Message
	}
	if apiErr.ReasonDetail == "" {
		apiErr.ReasonDetail = fmt.Sprintf("HTTP %d response contained no parseable PayPal error details", status)
	}
	return apiErr
}

type captureTransactionContextKey struct{}

const captureInsertSQL = `INSERT INTO captures (id, amount, amount_minor, status, currency, date, is_live)
	VALUES ($1, ($2::NUMERIC / CASE WHEN $4 IN ('JPY', 'HUF', 'TWD') THEN 1 ELSE 100 END)::DOUBLE PRECISION,
		$2, $3, $4, $5, $6) ON CONFLICT (id) DO NOTHING`

var (
	db           *sql.DB
	httpClient   = &http.Client{Timeout: 15 * time.Second}
	orderIDRegex = regexp.MustCompile(`^[A-Z0-9]{1,50}$`)
	orderLock    = withPostgresOrderAdvisoryLock

	allowedPayPalHosts = map[string]bool{
		"www.sandbox.paypal.com": true,
		"sandbox.paypal.com":     true,
		"www.paypal.com":         true,
		"paypal.com":             true,
	}

	insightCache = struct {
		sync.Mutex
		entries map[[32]byte]*insightCacheEntry
	}{entries: make(map[[32]byte]*insightCacheEntry)}

	writePaymentAttempt = func(ctx context.Context, targetDB *sql.DB, attempt PaymentAttempt) error {
		if targetDB == nil {
			return fmt.Errorf("database unavailable")
		}
		_, err := targetDB.ExecContext(ctx, `INSERT INTO attempts
			(paypal_order_id, capture_id, debug_id, stage, outcome, reason_code, reason_detail, http_status, amount_minor, currency, is_synthetic)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
			attempt.PayPalOrderID, attempt.CaptureID, attempt.DebugID, attempt.Stage, attempt.Outcome,
			attempt.ReasonCode, attempt.ReasonDetail, attempt.HTTPStatus, attempt.AmountMinor, attempt.Currency, attempt.IsSynthetic)
		return err
	}

	persistCapture = func(ctx context.Context, p PaymentRecord) error {
		args := []any{p.ID, p.AmountMinor, p.Status, p.Currency, p.Date, p.IsLive}
		if tx, ok := ctx.Value(captureTransactionContextKey{}).(*sql.Tx); ok {
			_, err := tx.ExecContext(ctx, captureInsertSQL, args...)
			return err
		}
		_, err := db.ExecContext(ctx, captureInsertSQL, args...)
		return err
	}
)

type PaymentRecord struct {
	ID          string `json:"id"`
	AmountMinor int64  `json:"amount_minor,string"`
	Status      string `json:"status"`
	Currency    string `json:"currency"`
	Date        string `json:"date"`
	IsLive      bool   `json:"is_live"`
}

type insightCacheEntry struct {
	result    InsightResult
	expiresAt time.Time
	ready     chan struct{}
}

type SpendSummary struct {
	TotalCapturedMinor   int64             `json:"total_captured_minor,string"`
	LiveSuccess          int               `json:"live_success"`
	DemoCompletedCount   int               `json:"demo_completed_count"`
	DeclineRateAvailable bool              `json:"decline_rate_available"`
	DeclineRatePercent   float64           `json:"decline_rate_percent"`
	AIInsight            string            `json:"ai_insight"`
	AIInsightCitations   []InsightCitation `json:"ai_insight_citations"`
}

func currencyExponent(currency string) int {
	switch strings.ToUpper(currency) {
	case "JPY", "HUF", "TWD":
		return 0
	default:
		return 2
	}
}

func validCurrency(currency string) bool {
	if len(currency) != 3 {
		return false
	}
	for _, r := range currency {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}

func parseMinorUnits(value, currency string) (int64, error) {
	exponent := currencyExponent(currency)
	wholePart, fractionalPart, hasDecimal := strings.Cut(value, ".")
	if wholePart == "" || !isDigits(wholePart) {
		return 0, fmt.Errorf("malformed amount %q", value)
	}
	if hasDecimal && (exponent == 0 || fractionalPart == "" || len(fractionalPart) > exponent || !isDigits(fractionalPart)) {
		return 0, fmt.Errorf("malformed amount %q for %s", value, currency)
	}
	if !hasDecimal {
		fractionalPart = ""
	}
	fractionalPart += strings.Repeat("0", exponent-len(fractionalPart))

	whole, err := strconv.ParseInt(wholePart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("amount %q is out of range: %w", value, err)
	}
	var fraction int64
	if fractionalPart != "" {
		fraction, err = strconv.ParseInt(fractionalPart, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("malformed amount %q: %w", value, err)
		}
	}

	scale := int64(1)
	for i := 0; i < exponent; i++ {
		scale *= 10
	}
	const maxInt64 = int64(1<<63 - 1)
	if whole > (maxInt64-fraction)/scale {
		return 0, fmt.Errorf("amount %q is out of range", value)
	}
	return whole*scale + fraction, nil
}

func isDigits(value string) bool {
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return value != ""
}

func formatMinorUnits(amountMinor int64, currency string) string {
	exponent := currencyExponent(currency)
	scale := int64(1)
	for i := 0; i < exponent; i++ {
		scale *= 10
	}
	whole := amountMinor / scale
	wholeString := strconv.FormatInt(whole, 10)
	fraction := amountMinor % scale
	if fraction < 0 {
		fraction = -fraction
	}
	if amountMinor < 0 && whole == 0 {
		wholeString = "-0"
	}
	if exponent == 0 {
		return wholeString
	}
	return fmt.Sprintf("%s.%0*d", wholeString, exponent, fraction)
}

func validatePayment(p PaymentRecord) error {
	if p.Currency != expectedCurrency {
		return fmt.Errorf("invalid currency: expected %s, got %s", expectedCurrency, p.Currency)
	}
	if p.AmountMinor != expectedAmountMinor {
		return fmt.Errorf("invalid amount: expected %d minor units, got %d", expectedAmountMinor, p.AmountMinor)
	}
	return nil
}

// validateApprovalURL enforces HTTPS and an explicit PayPal hostname allowlist.
func validateApprovalURL(rawURL string) error {
	if rawURL == "" {
		return fmt.Errorf("approval URL is empty")
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("malformed approval URL: %w", err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("approval URL must use HTTPS, got scheme %q", u.Scheme)
	}
	if !allowedPayPalHosts[u.Hostname()] {
		return fmt.Errorf("approval URL has unexpected host %q", u.Hostname())
	}
	return nil
}

func payPalReturnCancelURLs(base string) (string, string, error) {
	base = strings.TrimSpace(base)
	if base == "" {
		base = "http://localhost:8080"
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", "", fmt.Errorf("APP_BASE_URL must be an absolute HTTP or HTTPS URL")
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/"
	u.Fragment = ""
	query := u.Query()
	query.Set("success", "true")
	u.RawQuery = query.Encode()
	returnURL := u.String()
	query = u.Query()
	query.Del("success")
	query.Set("cancel", "true")
	u.RawQuery = query.Encode()
	return returnURL, u.String(), nil
}

func main() {
	_ = godotenv.Load()

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		log.Fatal("DATABASE_URL is required")
	}
	if err := initDB(dbURL); err != nil {
		log.Fatalf("DB init failed: %v", err)
	}
	defer db.Close()

	clientID := strings.TrimSpace(os.Getenv("PAYPAL_CLIENT_ID"))
	clientSecret := strings.TrimSpace(os.Getenv("PAYPAL_CLIENT_SECRET"))
	groqKey := strings.TrimSpace(os.Getenv("GROQ_API_KEY"))
	adminToken := strings.TrimSpace(os.Getenv("ADMIN_TOKEN"))
	if clientID == "" || clientSecret == "" {
		log.Fatal("PAYPAL_CLIENT_ID and PAYPAL_CLIENT_SECRET are required")
	}

	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.Dir("./static")))
	webhookID := strings.TrimSpace(os.Getenv("PAYPAL_WEBHOOK_ID"))
	registerAPIRoutes(mux, clientID, clientSecret, groqKey, adminToken, webhookID)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Printf("🚀 Server starting on port %s", port)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Fatal(err)
	}
}

func registerAPIRoutes(mux *http.ServeMux, clientID, clientSecret, groqKey, adminToken, webhookID string) {
	mux.Handle("/api/create-order", makeCreateOrderHandler(clientID, clientSecret))
	mux.Handle("/api/capture-order", makeCaptureOrderHandler(clientID, clientSecret))
	mux.HandleFunc("/api/spend-intelligence", makeSpendHandler(groqKey))
	mux.HandleFunc("/api/transactions", makeTransactionsHandler())
	mux.HandleFunc("/api/webhook-activity", makeWebhookActivityHandler())
	registerFindingRoutes(mux, adminToken)
	mux.HandleFunc("/api/actions/", makeApproveActionHandler(adminToken))
	mux.HandleFunc("/webhooks/paypal", makePayPalWebhookHandler(clientID, clientSecret, webhookID))
}

type webhookActivityResponse struct {
	ReceivedCount        int64      `json:"received_count"`
	ProcessedCount       int64      `json:"processed_count"`
	ProcessingErrorCount int64      `json:"processing_error_count"`
	LastEventType        string     `json:"last_event_type"`
	LastEventAt          *time.Time `json:"last_event_at"`
}

func makeWebhookActivityHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			sendJSONError(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		var activity webhookActivityResponse
		var lastEventType sql.NullString
		var lastEventAt sql.NullTime
		err := db.QueryRowContext(ctx, `SELECT
			COUNT(*),
			COUNT(*) FILTER (WHERE processed_at IS NOT NULL),
			COUNT(*) FILTER (WHERE process_error IS NOT NULL AND process_error <> ''),
			(SELECT event_type FROM webhook_events ORDER BY received_at DESC, event_id DESC LIMIT 1),
			(SELECT received_at FROM webhook_events ORDER BY received_at DESC, event_id DESC LIMIT 1)
			FROM webhook_events`).Scan(
			&activity.ReceivedCount,
			&activity.ProcessedCount,
			&activity.ProcessingErrorCount,
			&lastEventType,
			&lastEventAt,
		)
		if err != nil {
			sendJSONError(w, "Webhook activity query failed", http.StatusInternalServerError)
			return
		}
		if lastEventType.Valid {
			activity.LastEventType = lastEventType.String
		}
		if lastEventAt.Valid {
			lastEventAtValue := lastEventAt.Time
			activity.LastEventAt = &lastEventAtValue
		}
		writeJSON(w, http.StatusOK, activity)
	}
}

func initDB(dbURL string) error {
	var err error
	db, err = sql.Open("postgres", dbURL)
	if err != nil {
		return fmt.Errorf("sql.Open: %w", err)
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(5 * time.Minute)
	db.SetConnMaxIdleTime(time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("ping: %w", err)
	}
	_, err = db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS captures (
		id TEXT PRIMARY KEY, amount DOUBLE PRECISION NOT NULL, amount_minor BIGINT, status TEXT NOT NULL,
		currency TEXT NOT NULL, date TEXT NOT NULL, is_live BOOLEAN NOT NULL
	)`)
	return err
}

func makeCreateOrderHandler(clientID, clientSecret string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			sendJSONError(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()

		token, err := getPayPalToken(ctx, clientID, clientSecret)
		if err != nil {
			recordAttemptBestEffort(attemptFromPayPalError("create", expectedAmountMinor, expectedCurrency, err))
			log.Printf("create-order auth: %v", err)
			sendJSONError(w, "PayPal auth failed", http.StatusBadGateway)
			return
		}

		returnURL, cancelURL, err := payPalReturnCancelURLs(os.Getenv("APP_BASE_URL"))
		if err != nil {
			recordAttemptBestEffort(PaymentAttempt{Stage: "create", Outcome: "failed", ReasonCode: "APP_BASE_URL_INVALID", ReasonDetail: "PayPal callback URL configuration is invalid", AmountMinor: expectedAmountMinor, Currency: expectedCurrency})
			log.Printf("create-order callback URL: %v", err)
			sendJSONError(w, "Invalid APP_BASE_URL", http.StatusInternalServerError)
			return
		}
		orderID, approveURL, httpStatus, err := createPayPalOrderWithStatus(ctx, token, returnURL, cancelURL)
		if err != nil {
			recordAttemptBestEffort(attemptFromPayPalErrorWithStatus("create", expectedAmountMinor, expectedCurrency, httpStatus, err))
			log.Printf("create-order create: %v", err)
			sendJSONError(w, "Order creation failed", http.StatusBadGateway)
			return
		}
		attempt := successfulAttempt("create", "ORDER_CREATED", "PayPal order created", expectedAmountMinor, expectedCurrency, httpStatus)
		attempt.PayPalOrderID = orderID
		recordAttemptBestEffort(attempt)
		if approveURL == "" {
			log.Printf("create-order: missing approval link in PayPal response")
			sendJSONError(w, "Missing approval link", http.StatusBadGateway)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"id": orderID, "approveURL": approveURL})
	}
}

func makeCaptureOrderHandler(clientID, clientSecret string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			sendJSONError(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var reqBody struct {
			OrderID string `json:"orderID"`
		}
		if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
			log.Printf("capture-order decode: %v", err)
			sendJSONError(w, "Invalid request body", http.StatusBadRequest)
			return
		}
		if !orderIDRegex.MatchString(reqBody.OrderID) {
			log.Printf("capture-order: invalid order ID format")
			sendJSONError(w, "Invalid order ID format", http.StatusBadRequest)
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()

		token, err := getPayPalToken(ctx, clientID, clientSecret)
		if err != nil {
			recordOrderAttempt(reqBody.OrderID, attemptFromPayPalError("approve", expectedAmountMinor, expectedCurrency, err))
			log.Printf("capture-order auth: %v", err)
			sendJSONError(w, "PayPal auth failed", http.StatusBadGateway)
			return
		}

		payment, failureStatus, failureMessage, persistenceErr, flowErr := captureOrderWithLock(ctx, token, reqBody.OrderID)
		if failureStatus != 0 {
			log.Printf("capture-order: %v", flowErr)
			sendJSONError(w, failureMessage, failureStatus)
			return
		}
		if persistenceErr != nil {
			correlationID := fmt.Sprintf("cap-%d", time.Now().UnixNano())
			log.Printf("[%s] capture-order persistence: %v", correlationID, persistenceErr)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"status":         "capture_succeeded_persistence_pending",
				"capture_id":     payment.ID,
				"amount_minor":   strconv.FormatInt(payment.AmountMinor, 10),
				"correlation_id": correlationID,
			})
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(payment)
	}
}

func captureOrderWithLock(ctx context.Context, token, orderID string) (PaymentRecord, int, string, error, error) {
	var payment PaymentRecord
	var failureStatus int
	var failureMessage string
	var persistenceErr error

	runErr := orderLock(ctx, orderID, func(lockCtx context.Context) error {
		status, verifyHTTPStatus, err := verifyOrderStatusWithStatus(lockCtx, token, orderID)
		if err != nil {
			recordOrderAttempt(orderID, attemptFromPayPalErrorWithStatus("approve", expectedAmountMinor, expectedCurrency, verifyHTTPStatus, err))
			failureStatus = http.StatusBadGateway
			failureMessage = "Failed to verify order status"
			return err
		}

		switch status {
		case "COMPLETED":
			attempt := successfulAttempt("approve", "ORDER_ALREADY_CAPTURED", "PayPal order was already completed", expectedAmountMinor, expectedCurrency, verifyHTTPStatus)
			attempt.Outcome = "reconciled"
			recordOrderAttempt(orderID, attempt)
			var reconcileHTTPStatus int
			var reconcileErr error
			payment, reconcileHTTPStatus, reconcileErr = reconcileCaptureWithStatus(lockCtx, token, orderID)
			err = reconcileErr
			if err != nil {
				recordOrderAttempt(orderID, attemptFromPayPalErrorWithStatus("reconcile", expectedAmountMinor, expectedCurrency, reconcileHTTPStatus, err))
				failureStatus = http.StatusBadGateway
				failureMessage = "Reconcile failed"
				return err
			}
			captureAttempt := successfulAttempt("capture", "ORDER_ALREADY_CAPTURED", "Capture reconciled from the completed PayPal order", payment.AmountMinor, payment.Currency, reconcileHTTPStatus)
			captureAttempt.Outcome = "reconciled"
			captureAttempt.CaptureID = payment.ID
			recordOrderAttempt(orderID, captureAttempt)
		case "APPROVED":
			approveAttempt := successfulAttempt("approve", "ORDER_APPROVED", "PayPal order is approved", expectedAmountMinor, expectedCurrency, verifyHTTPStatus)
			recordOrderAttempt(orderID, approveAttempt)
			var captureHTTPStatus int
			var captureErr error
			payment, captureHTTPStatus, captureErr = capturePayPalOrderWithStatus(lockCtx, token, orderID)
			err = captureErr
			if errors.Is(err, errOrderAlreadyCaptured) {
				captureAttempt := attemptFromPayPalErrorWithStatus("capture", expectedAmountMinor, expectedCurrency, captureHTTPStatus, err)
				captureAttempt.Outcome = "reconciled"
				recordOrderAttempt(orderID, captureAttempt)
				var reconcileHTTPStatus int
				var reconcileErr error
				payment, reconcileHTTPStatus, reconcileErr = reconcileCaptureWithStatus(lockCtx, token, orderID)
				err = reconcileErr
				if err != nil {
					recordOrderAttempt(orderID, attemptFromPayPalErrorWithStatus("reconcile", expectedAmountMinor, expectedCurrency, reconcileHTTPStatus, err))
					failureStatus = http.StatusBadGateway
					failureMessage = "Reconcile failed after PayPal reported an already-captured order"
					return err
				}
				reconcileAttempt := successfulAttempt("reconcile", "CAPTURE_RECONCILED", "PayPal capture reconciled", payment.AmountMinor, payment.Currency, reconcileHTTPStatus)
				reconcileAttempt.CaptureID = payment.ID
				recordOrderAttempt(orderID, reconcileAttempt)
			} else if err != nil {
				recordOrderAttempt(orderID, attemptFromPayPalErrorWithStatus("capture", expectedAmountMinor, expectedCurrency, captureHTTPStatus, err))
				failureStatus = http.StatusBadGateway
				failureMessage = "Capture failed"
				return err
			} else {
				captureAttempt := successfulAttempt("capture", "CAPTURE_COMPLETED", "PayPal capture completed", payment.AmountMinor, payment.Currency, captureHTTPStatus)
				captureAttempt.CaptureID = payment.ID
				recordOrderAttempt(orderID, captureAttempt)
			}
		default:
			attempt := PaymentAttempt{Stage: "approve", Outcome: "failed", ReasonCode: "ORDER_NOT_APPROVED", ReasonDetail: "PayPal order status is " + status, HTTPStatus: &verifyHTTPStatus, AmountMinor: expectedAmountMinor, Currency: expectedCurrency}
			recordOrderAttempt(orderID, attempt)
			failureStatus = http.StatusBadRequest
			failureMessage = fmt.Sprintf("Order status is %s. Must be APPROVED.", status)
			return fmt.Errorf("unexpected order status %q", status)
		}

		if err := validatePayment(payment); err != nil {
			failureStatus = http.StatusBadGateway
			failureMessage = "Payment validation failed"
			return err
		}

		persistenceErr = persistCapture(lockCtx, payment)
		return nil
	})
	if runErr != nil && failureStatus == 0 {
		if payment.ID != "" {
			persistenceErr = runErr
		} else {
			failureStatus = http.StatusInternalServerError
			failureMessage = "Failed to acquire or release order capture lock"
		}
	}
	return payment, failureStatus, failureMessage, persistenceErr, runErr
}

func withPostgresOrderAdvisoryLock(ctx context.Context, orderID string, fn func(context.Context) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin capture lock transaction: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, orderID); err != nil {
		return fmt.Errorf("acquire capture advisory lock: %w", err)
	}
	lockedContext := context.WithValue(ctx, captureTransactionContextKey{}, tx)
	if err := fn(lockedContext); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("release capture advisory lock: %w", err)
	}
	return nil
}

func makeSpendHandler(groqKey string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		liveRecords, err := getLiveRecords(ctx)
		if err != nil {
			log.Printf("spend-intelligence db: %v", err)
			sendJSONError(w, "DB query failed", http.StatusInternalServerError)
			return
		}

		summary := calculateSummary(liveRecords, getDemoRecords())
		loadDeclineRate(ctx, &summary)
		evidence, evidenceErr := getInsightEvidence(ctx, liveRecords)
		if evidenceErr != nil {
			log.Printf("spend-intelligence evidence query failed")
			summary.AIInsight = "AI insight unavailable."
		} else {
			insight := cachedAIInsight(ctx, groqKey, evidence)
			summary.AIInsight = insight.Insight
			summary.AIInsightCitations = insight.Citations
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(summary)
	}
}

func loadDeclineRate(ctx context.Context, summary *SpendSummary) {
	if db == nil {
		return
	}
	var declined, total int64
	if err := db.QueryRowContext(ctx, declineRateQuery).Scan(&declined, &total); err != nil {
		return
	}
	summary.DeclineRatePercent, summary.DeclineRateAvailable = declineRate(declined, total)
}

func makeTransactionsHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		liveRecords, err := getLiveRecords(ctx)
		if err != nil {
			log.Printf("transactions db: %v", err)
			sendJSONError(w, "DB query failed", http.StatusInternalServerError)
			return
		}

		all := append(liveRecords, getDemoRecords()...)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(all)
	}
}

func getLiveRecords(ctx context.Context) ([]PaymentRecord, error) {
	rows, err := db.QueryContext(ctx, liveRecordsQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []PaymentRecord
	for rows.Next() {
		var p PaymentRecord
		if err := rows.Scan(&p.ID, &p.AmountMinor, &p.Status, &p.Currency, &p.Date, &p.IsLive); err != nil {
			return nil, err
		}
		records = append(records, p)
	}
	return records, rows.Err()
}

// --- HARDENED PAYPAL HELPERS ---

func getPayPalToken(ctx context.Context, clientID, secret string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "POST",
		"https://api-m.sandbox.paypal.com/v1/oauth2/token",
		strings.NewReader("grant_type=client_credentials"))
	if err != nil {
		return "", fmt.Errorf("build token request: %w", err)
	}
	req.SetBasicAuth(clientID, secret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("token request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read token body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("token request: %w", newPayPalAPIError(resp.StatusCode, body))
	}

	var result struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("decode token response: %w", err)
	}
	if result.AccessToken == "" {
		return "", fmt.Errorf("empty access token in response")
	}
	return result.AccessToken, nil
}

func createPayPalOrder(ctx context.Context, token, returnURL string, cancelURLs ...string) (string, string, error) {
	id, approvalURL, _, err := createPayPalOrderWithStatus(ctx, token, returnURL, cancelURLs...)
	return id, approvalURL, err
}

func createPayPalOrderWithStatus(ctx context.Context, token, returnURL string, cancelURLs ...string) (string, string, int, error) {
	cancelURL := returnURL
	if len(cancelURLs) != 0 {
		cancelURL = cancelURLs[0]
	}
	payload := map[string]interface{}{
		"intent": "CAPTURE",
		"purchase_units": []map[string]interface{}{
			{"amount": map[string]string{"currency_code": expectedCurrency, "value": formatMinorUnits(expectedAmountMinor, expectedCurrency)}},
		},
		"application_context": map[string]string{
			"return_url": returnURL,
			"cancel_url": cancelURL,
		},
	}
	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		return "", "", 0, fmt.Errorf("marshal order payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST",
		"https://api-m.sandbox.paypal.com/v2/checkout/orders", bytes.NewReader(jsonPayload))
	if err != nil {
		return "", "", 0, fmt.Errorf("build order request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", "", 0, fmt.Errorf("order request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", resp.StatusCode, fmt.Errorf("read order body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", resp.StatusCode, fmt.Errorf("order request: %w", newPayPalAPIError(resp.StatusCode, body))
	}

	var result struct {
		ID    string `json:"id"`
		Links []struct {
			Href string `json:"href"`
			Rel  string `json:"rel"`
		} `json:"links"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", "", resp.StatusCode, fmt.Errorf("decode order response: %w", err)
	}
	if result.ID == "" {
		return "", "", resp.StatusCode, fmt.Errorf("missing order ID in response")
	}

	var approveURL string
	for _, link := range result.Links {
		if link.Rel == "approve" || link.Rel == "payer-action" {
			approveURL = link.Href
			break
		}
	}
	if err := validateApprovalURL(approveURL); err != nil {
		return "", "", resp.StatusCode, fmt.Errorf("invalid approval URL: %w", err)
	}
	return result.ID, approveURL, resp.StatusCode, nil
}

func verifyOrderStatus(ctx context.Context, token, orderID string) (string, error) {
	status, _, err := verifyOrderStatusWithStatus(ctx, token, orderID)
	return status, err
}

func verifyOrderStatusWithStatus(ctx context.Context, token, orderID string) (string, int, error) {
	req, err := http.NewRequestWithContext(ctx, "GET",
		fmt.Sprintf("https://api-m.sandbox.paypal.com/v2/checkout/orders/%s", orderID), nil)
	if err != nil {
		return "", 0, fmt.Errorf("build verify request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("verify request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", resp.StatusCode, fmt.Errorf("read verify body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", resp.StatusCode, fmt.Errorf("verify request: %w", newPayPalAPIError(resp.StatusCode, body))
	}

	var result struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", resp.StatusCode, fmt.Errorf("malformed status response: %w", err)
	}
	if result.Status == "" {
		return "", resp.StatusCode, fmt.Errorf("empty order status in response")
	}
	return result.Status, resp.StatusCode, nil
}

func captureRequestID(orderID string) string {
	return "paypal-ai-auditor-capture-" + orderID
}

func capturePayPalOrder(ctx context.Context, token, orderID string) (PaymentRecord, error) {
	payment, _, err := capturePayPalOrderWithStatus(ctx, token, orderID)
	return payment, err
}

func capturePayPalOrderWithStatus(ctx context.Context, token, orderID string) (PaymentRecord, int, error) {
	req, err := http.NewRequestWithContext(ctx, "POST",
		fmt.Sprintf("https://api-m.sandbox.paypal.com/v2/checkout/orders/%s/capture", orderID), nil)
	if err != nil {
		return PaymentRecord{}, 0, fmt.Errorf("build capture request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("PayPal-Request-Id", captureRequestID(orderID))

	resp, err := httpClient.Do(req)
	if err != nil {
		return PaymentRecord{}, 0, fmt.Errorf("capture request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return PaymentRecord{}, resp.StatusCode, fmt.Errorf("read capture body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		apiErr := newPayPalAPIError(resp.StatusCode, body)
		if apiErr.ReasonCode == "ORDER_ALREADY_CAPTURED" {
			return PaymentRecord{}, resp.StatusCode, fmt.Errorf("%w: %w", errOrderAlreadyCaptured, apiErr)
		}
		return PaymentRecord{}, resp.StatusCode, fmt.Errorf("capture request: %w", apiErr)
	}
	payment, err := parseCaptureFromBody(body)
	return payment, resp.StatusCode, err
}

func reconcileCapture(ctx context.Context, token, orderID string) (PaymentRecord, error) {
	payment, _, err := reconcileCaptureWithStatus(ctx, token, orderID)
	return payment, err
}

func reconcileCaptureWithStatus(ctx context.Context, token, orderID string) (PaymentRecord, int, error) {
	req, err := http.NewRequestWithContext(ctx, "GET",
		fmt.Sprintf("https://api-m.sandbox.paypal.com/v2/checkout/orders/%s", orderID), nil)
	if err != nil {
		return PaymentRecord{}, 0, fmt.Errorf("build reconcile request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := httpClient.Do(req)
	if err != nil {
		return PaymentRecord{}, 0, fmt.Errorf("reconcile request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return PaymentRecord{}, resp.StatusCode, fmt.Errorf("read reconcile body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return PaymentRecord{}, resp.StatusCode, fmt.Errorf("reconcile request: %w", newPayPalAPIError(resp.StatusCode, body))
	}
	payment, err := parseCaptureFromBody(body)
	return payment, resp.StatusCode, err
}

func parseCaptureFromBody(body []byte) (PaymentRecord, error) {
	var result struct {
		Status        string `json:"status"`
		PurchaseUnits []struct {
			Payments struct {
				Captures []struct {
					ID         string `json:"id"`
					Status     string `json:"status"`
					CreateTime string `json:"create_time"`
					UpdateTime string `json:"update_time"`
					Amount     struct {
						Value        string `json:"value"`
						CurrencyCode string `json:"currency_code"`
					} `json:"amount"`
				} `json:"captures"`
			} `json:"payments"`
		} `json:"purchase_units"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return PaymentRecord{}, fmt.Errorf("malformed capture response: %w", err)
	}
	if len(result.PurchaseUnits) == 0 || len(result.PurchaseUnits[0].Payments.Captures) == 0 {
		return PaymentRecord{}, fmt.Errorf("no captures in response")
	}
	capture := result.PurchaseUnits[0].Payments.Captures[0]
	if capture.ID == "" {
		return PaymentRecord{}, fmt.Errorf("empty capture ID")
	}
	if capture.Status != "COMPLETED" {
		return PaymentRecord{}, fmt.Errorf("capture status %s, expected COMPLETED", capture.Status)
	}
	amountMinor, err := parseMinorUnits(capture.Amount.Value, capture.Amount.CurrencyCode)
	if err != nil {
		return PaymentRecord{}, err
	}
	if capture.Amount.CurrencyCode == "" {
		return PaymentRecord{}, fmt.Errorf("empty currency")
	}
	timestamp := capture.CreateTime
	if timestamp == "" {
		timestamp = capture.UpdateTime
	}
	if timestamp == "" {
		return PaymentRecord{}, fmt.Errorf("capture response has no create_time or update_time")
	}
	capturedAt, err := time.Parse(time.RFC3339Nano, timestamp)
	if err != nil {
		return PaymentRecord{}, fmt.Errorf("invalid capture timestamp: %w", err)
	}
	return PaymentRecord{
		ID:          capture.ID,
		AmountMinor: amountMinor,
		Status:      capture.Status,
		Currency:    capture.Amount.CurrencyCode,
		Date:        capturedAt.UTC().Format(time.RFC3339Nano),
		IsLive:      true,
	}, nil
}

func getDemoRecords() []PaymentRecord {
	return []PaymentRecord{
		{ID: "DEMO-CAP-101", AmountMinor: 2500, Status: "COMPLETED", Currency: "USD", Date: "2026-10-01T00:00:00Z", IsLive: false},
		{ID: "DEMO-CAP-102", AmountMinor: 15050, Status: "COMPLETED", Currency: "USD", Date: "2026-10-05T00:00:00Z", IsLive: false},
		{ID: "DEMO-CAP-104", AmountMinor: 0, Status: "DECLINED", Currency: "USD", Date: "2026-10-10T00:00:00Z", IsLive: false},
	}
}

func calculateSummary(live, demo []PaymentRecord) SpendSummary {
	var liveTotal int64
	var liveSuccess, demoCompleted int
	for _, r := range live {
		if r.Status == "COMPLETED" {
			liveTotal += r.AmountMinor
			liveSuccess++
		}
	}
	for _, r := range demo {
		if r.Status == "COMPLETED" {
			demoCompleted++
		}
	}
	return SpendSummary{TotalCapturedMinor: liveTotal, LiveSuccess: liveSuccess, DemoCompletedCount: demoCompleted, DeclineRateAvailable: false}
}

func recordAttemptBestEffort(attempt PaymentAttempt) {
	targetDB := db
	writer := writePaymentAttempt
	if targetDB == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := writer(ctx, targetDB, attempt); err != nil {
			log.Printf("payment attempt logging failed stage=%s outcome=%s", attempt.Stage, attempt.Outcome)
		}
	}()
}

func recordOrderAttempt(orderID string, attempt PaymentAttempt) {
	attempt.PayPalOrderID = orderID
	recordAttemptBestEffort(attempt)
}

func attemptFromPayPalError(stage string, amountMinor int64, currency string, err error) PaymentAttempt {
	attempt := PaymentAttempt{
		Stage:        stage,
		Outcome:      "failed",
		ReasonCode:   "UNCLASSIFIED",
		ReasonDetail: "PayPal request failed without structured error details",
		AmountMinor:  amountMinor,
		Currency:     currency,
	}
	var apiErr *PayPalAPIError
	if !errors.As(err, &apiErr) {
		return attempt
	}
	attempt.ReasonCode = apiErr.ReasonCode
	attempt.ReasonDetail = apiErr.ReasonDetail
	attempt.DebugID = apiErr.DebugID
	attempt.HTTPStatus = &apiErr.HTTPStatus
	if isDeclineReasonCode(apiErr.ReasonCode) {
		attempt.Outcome = "declined"
	} else if strings.EqualFold(apiErr.ReasonCode, "ORDER_ALREADY_CAPTURED") {
		attempt.Outcome = "reconciled"
	}
	return attempt
}

func isDeclineReasonCode(reasonCode string) bool {
	switch strings.ToUpper(strings.TrimSpace(reasonCode)) {
	case "INSTRUMENT_DECLINED", "PAYER_CANNOT_PAY", "PAYMENT_DENIED":
		return true
	default:
		return false
	}
}

func attemptFromPayPalErrorWithStatus(stage string, amountMinor int64, currency string, status int, err error) PaymentAttempt {
	attempt := attemptFromPayPalError(stage, amountMinor, currency, err)
	if attempt.HTTPStatus == nil && status > 0 {
		attempt.HTTPStatus = &status
	}
	return attempt
}

func successfulAttempt(stage, reason, detail string, amountMinor int64, currency string, status int) PaymentAttempt {
	return PaymentAttempt{
		Stage:        stage,
		Outcome:      "succeeded",
		ReasonCode:   reason,
		ReasonDetail: detail,
		HTTPStatus:   &status,
		AmountMinor:  amountMinor,
		Currency:     currency,
	}
}

func declineRate(declined, total int64) (float64, bool) {
	if total <= 0 {
		return 0, false
	}
	return float64(declined) * 100 / float64(total), true
}

func groqErrorCode(body []byte) string {
	var response struct {
		Error struct {
			Code string `json:"code"`
			Type string `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &response) != nil {
		return "UNCLASSIFIED"
	}
	code := strings.TrimSpace(response.Error.Code)
	if code == "" {
		code = strings.TrimSpace(response.Error.Type)
	}
	if code == "" || len(code) > 48 {
		return "UNCLASSIFIED"
	}
	for _, char := range code {
		if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '_' || char == '-' || char == '.') {
			return "UNCLASSIFIED"
		}
	}
	return code
}

func sendJSONError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
