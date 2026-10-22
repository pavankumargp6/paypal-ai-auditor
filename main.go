package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
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
	expectedAmount   = 25.00
	expectedCurrency = "USD"
)

var (
	db           *sql.DB
	httpClient   = &http.Client{Timeout: 15 * time.Second}
	orderIDRegex = regexp.MustCompile(`^[A-Z0-9]{1,50}$`)

	allowedPayPalHosts = map[string]bool{
		"www.sandbox.paypal.com": true,
		"sandbox.paypal.com":     true,
		"www.paypal.com":         true,
		"paypal.com":             true,
	}

	persistCapture = func(ctx context.Context, p PaymentRecord) error {
		_, err := db.ExecContext(ctx, `INSERT INTO captures (id, amount, status, currency, date, is_live)
			VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT (id) DO NOTHING`,
			p.ID, p.Amount, p.Status, p.Currency, p.Date, p.IsLive)
		return err
	}
)

type PaymentRecord struct {
	ID       string  `json:"id"`
	Amount   float64 `json:"amount"`
	Status   string  `json:"status"`
	Currency string  `json:"currency"`
	Date     string  `json:"date"`
	IsLive   bool    `json:"is_live"`
}

type SpendSummary struct {
	TotalCaptured        float64 `json:"total_captured"`
	LiveSuccess          int     `json:"live_success"`
	DemoCompletedCount   int     `json:"demo_completed_count"`
	DeclineRateAvailable bool    `json:"decline_rate_available"`
	AIInsight            string  `json:"ai_insight"`
}

func validatePayment(p PaymentRecord) error {
	if p.Currency != expectedCurrency {
		return fmt.Errorf("invalid currency: expected %s, got %s", expectedCurrency, p.Currency)
	}
	if math.Abs(p.Amount-expectedAmount) > 0.001 {
		return fmt.Errorf("invalid amount: expected %.2f, got %.2f", expectedAmount, p.Amount)
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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("ping: %w", err)
	}
	_, err = db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS captures (
		id TEXT PRIMARY KEY, amount DOUBLE PRECISION NOT NULL, status TEXT NOT NULL,
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

		status, err := verifyOrderStatus(ctx, token, reqBody.OrderID)
		if err != nil {
			log.Printf("capture-order verify: %v", err)
			sendJSONError(w, "Failed to verify order status", http.StatusBadGateway)
			return
		}

		var payment PaymentRecord
		switch status {
		case "COMPLETED":
			payment, err = reconcileCapture(ctx, token, reqBody.OrderID)
			if err != nil {
				log.Printf("capture-order reconcile: %v", err)
				sendJSONError(w, "Reconcile failed", http.StatusBadGateway)
				return
			}
		case "APPROVED":
			payment, err = capturePayPalOrder(ctx, token, reqBody.OrderID)
			if err != nil {
				log.Printf("capture-order capture: %v", err)
				sendJSONError(w, "Capture failed", http.StatusBadGateway)
				return
			}
		default:
			log.Printf("capture-order: unexpected order status %q", status)
			sendJSONError(w, fmt.Sprintf("Order status is %s. Must be APPROVED.", status), http.StatusBadRequest)
			return
		}

		if err := validatePayment(payment); err != nil {
			log.Printf("capture-order validation: %v", err)
			sendJSONError(w, "Payment validation failed", http.StatusBadGateway)
			return
		}

		if err := persistCapture(ctx, payment); err != nil {
			correlationID := fmt.Sprintf("cap-%d", time.Now().UnixNano())
			log.Printf("[%s] capture-order persistence: %v", correlationID, err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"status":         "capture_succeeded_persistence_pending",
				"capture_id":     payment.ID,
				"amount":         payment.Amount,
				"correlation_id": correlationID,
			})
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(payment)
	}
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
	rows, err := db.QueryContext(ctx, `SELECT id, amount, status, currency, date, is_live FROM captures ORDER BY date DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []PaymentRecord
	for rows.Next() {
		var p PaymentRecord
		if err := rows.Scan(&p.ID, &p.Amount, &p.Status, &p.Currency, &p.Date, &p.IsLive); err != nil {
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
			{"amount": map[string]string{"currency_code": expectedCurrency, "value": fmt.Sprintf("%.2f", expectedAmount)}},
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

func capturePayPalOrder(ctx context.Context, token, orderID string) (PaymentRecord, error) {
	req, err := http.NewRequestWithContext(ctx, "POST",
		fmt.Sprintf("https://api-m.sandbox.paypal.com/v2/checkout/orders/%s/capture", orderID), nil)
	if err != nil {
		return PaymentRecord{}, fmt.Errorf("build capture request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

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
		return PaymentRecord{}, fmt.Errorf("capture request failed with status %d", resp.StatusCode)
	}
	return parseCaptureFromBody(body)
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
	amt, err := strconv.ParseFloat(capture.Amount.Value, 64)
	if err != nil {
		return PaymentRecord{}, fmt.Errorf("parse amount %q: %w", capture.Amount.Value, err)
	}
	if capture.Amount.CurrencyCode == "" {
		return PaymentRecord{}, fmt.Errorf("empty currency")
	}
	return PaymentRecord{
		ID:       capture.ID,
		Amount:   amt,
		Status:   capture.Status,
		Currency: capture.Amount.CurrencyCode,
		Date:     time.Now().UTC().Format(time.RFC3339),
		IsLive:   true,
	}, nil
}

func getDemoRecords() []PaymentRecord {
	return []PaymentRecord{
		{ID: "DEMO-CAP-101", Amount: 25.00, Status: "COMPLETED", Currency: "USD", Date: "2026-10-01T00:00:00Z", IsLive: false},
		{ID: "DEMO-CAP-102", Amount: 150.50, Status: "COMPLETED", Currency: "USD", Date: "2026-10-05T00:00:00Z", IsLive: false},
		{ID: "DEMO-CAP-104", Amount: 0.00, Status: "DECLINED", Currency: "USD", Date: "2026-10-10T00:00:00Z", IsLive: false},
	}
}

func calculateSummary(live, demo []PaymentRecord) SpendSummary {
	var liveTotal float64
	var liveSuccess, demoCompleted int
	for _, r := range live {
		if r.Status == "COMPLETED" {
			liveTotal += r.Amount
			liveSuccess++
		}
	}
	for _, r := range demo {
		if r.Status == "COMPLETED" {
			demoCompleted++
		}
	}
	return SpendSummary{TotalCaptured: liveTotal, LiveSuccess: liveSuccess, DemoCompletedCount: demoCompleted, DeclineRateAvailable: false}
}

func generateAIInsight(ctx context.Context, apiKey string, summary SpendSummary) string {
	prompt := fmt.Sprintf(`You are a merchant AI copilot. Analyze: Live Revenue $%.2f (%d payments), Demo %d. Decline rate NOT TRACKED. Give 1-sentence actionable insight. Max 20 words.`, summary.TotalCaptured, summary.LiveSuccess, summary.DemoCompletedCount)

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
