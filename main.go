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

var errOrderAlreadyCaptured = errors.New("PayPal reports this order was already captured")

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

type SpendSummary struct {
	TotalCapturedMinor   int64  `json:"total_captured_minor,string"`
	LiveSuccess          int    `json:"live_success"`
	DemoCompletedCount   int    `json:"demo_completed_count"`
	DeclineRateAvailable bool   `json:"decline_rate_available"`
	AIInsight            string `json:"ai_insight"`
}

func currencyExponent(currency string) int {
	switch strings.ToUpper(currency) {
	case "JPY", "HUF", "TWD":
		return 0
	default:
		return 2
	}
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
	if clientID == "" || clientSecret == "" {
		log.Fatal("PAYPAL_CLIENT_ID and PAYPAL_CLIENT_SECRET are required")
	}

	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.Dir("./static")))
	mux.HandleFunc("/api/create-order", makeCreateOrderHandler(clientID, clientSecret))
	mux.HandleFunc("/api/capture-order", makeCaptureOrderHandler(clientID, clientSecret))
	mux.HandleFunc("/api/spend-intelligence", makeSpendHandler(groqKey))
	mux.HandleFunc("/api/transactions", makeTransactionsHandler())

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Printf("🚀 Server starting on port %s", port)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Fatal(err)
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
			log.Printf("create-order auth: %v", err)
			sendJSONError(w, "PayPal auth failed", http.StatusBadGateway)
			return
		}

		returnURL := "https://paypal-ai-auditor.onrender.com/?success=true"
		orderID, approveURL, err := createPayPalOrder(ctx, token, returnURL)
		if err != nil {
			log.Printf("create-order create: %v", err)
			sendJSONError(w, "Order creation failed", http.StatusBadGateway)
			return
		}
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
		status, err := verifyOrderStatus(lockCtx, token, orderID)
		if err != nil {
			failureStatus = http.StatusBadGateway
			failureMessage = "Failed to verify order status"
			return err
		}

		switch status {
		case "COMPLETED":
			payment, err = reconcileCapture(lockCtx, token, orderID)
			if err != nil {
				failureStatus = http.StatusBadGateway
				failureMessage = "Reconcile failed"
				return err
			}
		case "APPROVED":
			payment, err = capturePayPalOrder(lockCtx, token, orderID)
			if errors.Is(err, errOrderAlreadyCaptured) {
				payment, err = reconcileCapture(lockCtx, token, orderID)
				if err != nil {
					failureStatus = http.StatusBadGateway
					failureMessage = "Reconcile failed after PayPal reported an already-captured order"
					return err
				}
			} else if err != nil {
				failureStatus = http.StatusBadGateway
				failureMessage = "Capture failed"
				return err
			}
		default:
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
		summary.AIInsight = generateAIInsight(ctx, groqKey, summary)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(summary)
	}
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
		return "", fmt.Errorf("token request failed with status %d", resp.StatusCode)
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

func createPayPalOrder(ctx context.Context, token, returnURL string) (string, string, error) {
	payload := map[string]interface{}{
		"intent": "CAPTURE",
		"purchase_units": []map[string]interface{}{
			{"amount": map[string]string{"currency_code": expectedCurrency, "value": formatMinorUnits(expectedAmountMinor, expectedCurrency)}},
		},
		"application_context": map[string]string{
			"return_url": returnURL,
			"cancel_url": "https://paypal-ai-auditor.onrender.com/?cancel=true",
		},
	}
	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		return "", "", fmt.Errorf("marshal order payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST",
		"https://api-m.sandbox.paypal.com/v2/checkout/orders", bytes.NewReader(jsonPayload))
	if err != nil {
		return "", "", fmt.Errorf("build order request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("order request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", fmt.Errorf("read order body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", fmt.Errorf("order request failed with status %d", resp.StatusCode)
	}

	var result struct {
		ID    string `json:"id"`
		Links []struct {
			Href string `json:"href"`
			Rel  string `json:"rel"`
		} `json:"links"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", "", fmt.Errorf("decode order response: %w", err)
	}
	if result.ID == "" {
		return "", "", fmt.Errorf("missing order ID in response")
	}

	var approveURL string
	for _, link := range result.Links {
		if link.Rel == "approve" || link.Rel == "payer-action" {
			approveURL = link.Href
			break
		}
	}
	if err := validateApprovalURL(approveURL); err != nil {
		return "", "", fmt.Errorf("invalid approval URL: %w", err)
	}
	return result.ID, approveURL, nil
}

func verifyOrderStatus(ctx context.Context, token, orderID string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET",
		fmt.Sprintf("https://api-m.sandbox.paypal.com/v2/checkout/orders/%s", orderID), nil)
	if err != nil {
		return "", fmt.Errorf("build verify request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("verify request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read verify body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("verify request failed with status %d", resp.StatusCode)
	}

	var result struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("malformed status response: %w", err)
	}
	if result.Status == "" {
		return "", fmt.Errorf("empty order status in response")
	}
	return result.Status, nil
}

func captureRequestID(orderID string) string {
	return "paypal-ai-auditor-capture-" + orderID
}

func capturePayPalOrder(ctx context.Context, token, orderID string) (PaymentRecord, error) {
	req, err := http.NewRequestWithContext(ctx, "POST",
		fmt.Sprintf("https://api-m.sandbox.paypal.com/v2/checkout/orders/%s/capture", orderID), nil)
	if err != nil {
		return PaymentRecord{}, fmt.Errorf("build capture request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("PayPal-Request-Id", captureRequestID(orderID))

	resp, err := httpClient.Do(req)
	if err != nil {
		return PaymentRecord{}, fmt.Errorf("capture request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return PaymentRecord{}, fmt.Errorf("read capture body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if paypalResponseHasIssue(body, "ORDER_ALREADY_CAPTURED") {
			return PaymentRecord{}, fmt.Errorf("%w (HTTP %d)", errOrderAlreadyCaptured, resp.StatusCode)
		}
		return PaymentRecord{}, fmt.Errorf("capture request failed with status %d", resp.StatusCode)
	}
	return parseCaptureFromBody(body)
}

func paypalResponseHasIssue(body []byte, issue string) bool {
	var response struct {
		Details []struct {
			Issue string `json:"issue"`
		} `json:"details"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return false
	}
	for _, detail := range response.Details {
		if detail.Issue == issue {
			return true
		}
	}
	return false
}

func reconcileCapture(ctx context.Context, token, orderID string) (PaymentRecord, error) {
	req, err := http.NewRequestWithContext(ctx, "GET",
		fmt.Sprintf("https://api-m.sandbox.paypal.com/v2/checkout/orders/%s", orderID), nil)
	if err != nil {
		return PaymentRecord{}, fmt.Errorf("build reconcile request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := httpClient.Do(req)
	if err != nil {
		return PaymentRecord{}, fmt.Errorf("reconcile request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return PaymentRecord{}, fmt.Errorf("read reconcile body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return PaymentRecord{}, fmt.Errorf("reconcile request failed with status %d", resp.StatusCode)
	}
	return parseCaptureFromBody(body)
}

func parseCaptureFromBody(body []byte) (PaymentRecord, error) {
	var result struct {
		Status        string `json:"status"`
		PurchaseUnits []struct {
			Payments struct {
				Captures []struct {
					ID     string `json:"id"`
					Status string `json:"status"`
					Amount struct {
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
	return PaymentRecord{
		ID:          capture.ID,
		AmountMinor: amountMinor,
		Status:      capture.Status,
		Currency:    capture.Amount.CurrencyCode,
		Date:        time.Now().UTC().Format(time.RFC3339),
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

func generateAIInsight(ctx context.Context, apiKey string, summary SpendSummary) string {
	prompt := fmt.Sprintf(`You are a merchant AI copilot. Analyze: Live Revenue $%s (%d payments), Demo %d. Decline rate NOT TRACKED. Give 1-sentence actionable insight. Max 20 words.`, formatMinorUnits(summary.TotalCapturedMinor, expectedCurrency), summary.LiveSuccess, summary.DemoCompletedCount)

	payload := map[string]interface{}{
		"model":       "openai/gpt-oss-20b",
		"messages":    []map[string]string{{"role": "user", "content": prompt}},
		"temperature": 0.1,
	}
	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		return "AI insight unavailable."
	}

	req, err := http.NewRequestWithContext(ctx, "POST",
		"https://api.groq.com/openai/v1/chat/completions", bytes.NewReader(jsonPayload))
	if err != nil {
		return "AI insight unavailable."
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := httpClient.Do(req)
	if err != nil {
		return "AI insight unavailable."
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "AI insight unavailable."
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "AI insight unavailable."
	}

	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "AI insight unavailable."
	}
	if len(result.Choices) == 0 {
		return "AI insight unavailable."
	}

	insight := strings.TrimSpace(result.Choices[0].Message.Content)
	if insight == "" {
		return "AI insight unavailable."
	}
	return insight
}

func sendJSONError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
