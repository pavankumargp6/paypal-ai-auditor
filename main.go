package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
	_ "github.com/lib/pq"
)

const (
	orderAmount   = 25.00
	orderCurrency = "USD"
)

var (
	db         *sql.DB
	httpClient = &http.Client{Timeout: 15 * time.Second}
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
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("ping: %w", err)
	}
	_, err = db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS captures (
		id TEXT PRIMARY KEY,
		amount DOUBLE PRECISION NOT NULL,
		status TEXT NOT NULL,
		currency TEXT NOT NULL,
		date TEXT NOT NULL,
		is_live BOOLEAN NOT NULL
	)`)
	if err != nil {
		return fmt.Errorf("create table: %w", err)
	}
	log.Println("✅ Database connected and table ready")
	return nil
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
			log.Printf("create-order token error: %v", err)
			sendJSONError(w, "PayPal auth failed", http.StatusBadGateway)
			return
		}

		returnURL := "https://paypal-ai-auditor.onrender.com/?success=true"
		orderID, approveURL, err := createPayPalOrder(ctx, token, returnURL)
		if err != nil {
			log.Printf("create-order error: %v", err)
			sendJSONError(w, "Order creation failed: "+err.Error(), http.StatusBadGateway)
			return
		}
		if approveURL == "" {
			sendJSONError(w, "PayPal response missing approval link", http.StatusBadGateway)
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
			sendJSONError(w, "Invalid request body", http.StatusBadRequest)
			return
		}
		if reqBody.OrderID == "" {
			sendJSONError(w, "Missing orderID", http.StatusBadRequest)
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()

		token, err := getPayPalToken(ctx, clientID, clientSecret)
		if err != nil {
			log.Printf("capture token error: %v", err)
			sendJSONError(w, "PayPal auth failed", http.StatusBadGateway)
			return
		}

		payment, err := capturePayPalOrder(ctx, token, reqBody.OrderID)
		if err != nil {
			if strings.Contains(err.Error(), "ORDER_ALREADY_CAPTURED") {
				log.Printf("capture: order %s already captured, reconciling", reqBody.OrderID)
				payment, err = reconcileCapture(ctx, token, reqBody.OrderID)
				if err != nil {
					log.Printf("reconcile failed: %v", err)
					sendJSONError(w, "Reconcile failed: "+err.Error(), http.StatusBadGateway)
					return
				}
			} else {
				log.Printf("capture error: %v", err)
				sendJSONError(w, "Capture failed: "+err.Error(), http.StatusBadGateway)
				return
			}
		}

		if payment.Currency != orderCurrency {
			log.Printf("CRITICAL: currency mismatch: got %s, expected %s", payment.Currency, orderCurrency)
			sendJSONError(w, "Currency mismatch in PayPal response", http.StatusBadGateway)
			return
		}

		if err := persistCapture(ctx, payment); err != nil {
			correlationID := fmt.Sprintf("cap-%d", time.Now().UnixNano())
			log.Printf("[%s] CRITICAL: capture %s succeeded but persistence failed: %v", correlationID, payment.ID, err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"status":         "capture_succeeded_persistence_pending",
				"capture_id":     payment.ID,
				"amount":         payment.Amount,
				"currency":       payment.Currency,
				"correlation_id": correlationID,
				"message":        "Payment captured at PayPal but not saved. Please retry.",
			})
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(payment)
	}
}

func makeSpendHandler(groqKey string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			sendJSONError(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()

		liveRecords, err := getLiveRecords(ctx)
		if err != nil {
			log.Printf("spend: DB query failed: %v", err)
			sendJSONError(w, "Database query failed", http.StatusInternalServerError)
			return
		}

		demoRecords := getDemoRecords()
		summary := calculateSummary(liveRecords, demoRecords)
		summary.AIInsight = generateAIInsight(ctx, groqKey, summary)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(summary)
	}
}

func makeTransactionsHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			sendJSONError(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()

		liveRecords, err := getLiveRecords(ctx)
		if err != nil {
			log.Printf("transactions: DB query failed: %v", err)
			sendJSONError(w, "Database query failed", http.StatusInternalServerError)
			return
		}

		demoRecords := getDemoRecords()
		all := append(liveRecords, demoRecords...)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(all)
	}
}

func persistCapture(ctx context.Context, p PaymentRecord) error {
	_, err := db.ExecContext(ctx, `INSERT INTO captures (id, amount, status, currency, date, is_live)
		VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT (id) DO NOTHING`,
		p.ID, p.Amount, p.Status, p.Currency, p.Date, p.IsLive)
	return err
}

func getLiveRecords(ctx context.Context) ([]PaymentRecord, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT id, amount, status, currency, date, is_live FROM captures WHERE is_live = true ORDER BY date DESC`)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	var records []PaymentRecord
	for rows.Next() {
		var p PaymentRecord
		if err := rows.Scan(&p.ID, &p.Amount, &p.Status, &p.Currency, &p.Date, &p.IsLive); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		records = append(records, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows: %w", err)
	}
	return records, nil
}

func getPayPalToken(ctx context.Context, clientID, secret string) (string, error) {
	data := strings.NewReader("grant_type=client_credentials")
	req, err := http.NewRequestWithContext(ctx, "POST", "https://api-m.sandbox.paypal.com/v1/oauth2/token", data)
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

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("token status %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("decode token: %w", err)
	}
	if result.AccessToken == "" {
		return "", fmt.Errorf("empty access token")
	}
	return result.AccessToken, nil
}

func createPayPalOrder(ctx context.Context, token, returnURL string) (string, string, error) {
	payload := map[string]interface{}{
		"intent": "CAPTURE",
		"purchase_units": []map[string]interface{}{
			{"amount": map[string]string{"currency_code": orderCurrency, "value": fmt.Sprintf("%.2f", orderAmount)}},
		},
		"application_context": map[string]string{
			"return_url": returnURL,
			"cancel_url": "https://paypal-ai-auditor.onrender.com/?cancel=true",
		},
	}
	jsonPayload, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, "POST", "https://api-m.sandbox.paypal.com/v2/checkout/orders", bytes.NewReader(jsonPayload))
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

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return "", "", fmt.Errorf("order status %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		ID    string `json:"id"`
		Links []struct {
			Href string `json:"href"`
			Rel  string `json:"rel"`
		} `json:"links"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", "", fmt.Errorf("decode order: %w", err)
	}
	if result.ID == "" {
		return "", "", fmt.Errorf("missing order ID")
	}

	var approveURL string
	for _, link := range result.Links {
		if link.Rel == "approve" || link.Rel == "payer-action" {
			approveURL = link.Href
			break
		}
	}
	return result.ID, approveURL, nil
}

func capturePayPalOrder(ctx context.Context, token, orderID string) (PaymentRecord, error) {
	url := fmt.Sprintf("https://api-m.sandbox.paypal.com/v2/checkout/orders/%s/capture", orderID)
	req, err := http.NewRequestWithContext(ctx, "POST", url, nil)
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

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return PaymentRecord{}, fmt.Errorf("capture status %d: %s", resp.StatusCode, string(body))
	}

	return parseCaptureFromBody(body)
}

func reconcileCapture(ctx context.Context, token, orderID string) (PaymentRecord, error) {
	url := fmt.Sprintf("https://api-m.sandbox.paypal.com/v2/checkout/orders/%s", orderID)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return PaymentRecord{}, fmt.Errorf("build reconcile request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := httpClient.Do(req)
	if err != nil {
		return PaymentRecord{}, fmt.Errorf("reconcile request: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return PaymentRecord{}, fmt.Errorf("reconcile status %d: %s", resp.StatusCode, string(body))
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
		return PaymentRecord{}, fmt.Errorf("decode capture: %w", err)
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

	return SpendSummary{
		TotalCaptured:        liveTotal,
		LiveSuccess:          liveSuccess,
		DemoCompletedCount:   demoCompleted,
		DeclineRateAvailable: false,
	}
}

func generateAIInsight(ctx context.Context, apiKey string, summary SpendSummary) string {
	prompt := fmt.Sprintf(`You are a merchant AI copilot. Analyze this exact verified data:
- Live captured revenue: $%.2f (from %d completed PayPal Sandbox payments)
- Synthetic demo completed records: %d
- Decline rate: NOT TRACKED

Provide a 1-sentence actionable insight based ONLY on the live captured data. Max 20 words.`, summary.TotalCaptured, summary.LiveSuccess, summary.DemoCompletedCount)

	payload := map[string]interface{}{
		"model": "openai/gpt-oss-20b",
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
		"temperature": 0.1,
	}
	jsonPayload, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.groq.com/openai/v1/chat/completions", bytes.NewReader(jsonPayload))
	if err != nil {
		return "AI insight currently unavailable."
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := httpClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		return "AI insight currently unavailable."
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &result); err != nil || len(result.Choices) == 0 {
		return "AI insight unavailable."
	}
	return strings.TrimSpace(result.Choices[0].Message.Content)
}

func sendJSONError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
