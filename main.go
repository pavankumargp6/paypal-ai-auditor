package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/joho/godotenv"
	_ "github.com/lib/pq"
)

// --- Structs ---
type PaymentRecord struct {
	ID       string  `json:"id"`
	Amount   float64 `json:"amount"`
	Status   string  `json:"status"`
	Currency string  `json:"currency"`
	Date     string  `json:"date"`
	IsLive   bool    `json:"is_live"`
}

type SpendSummary struct {
	TotalCaptured float64 `json:"total_captured"`
	LiveSuccess   int     `json:"live_success"`
	LiveFailed    int     `json:"live_failed"`
	SuccessRate   float64 `json:"success_rate"`
	AIInsight     string  `json:"ai_insight"`
}

var db *sql.DB
var fallbackFile = "fallback_captures.json"

func main() {
	_ = godotenv.Load()

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL != "" {
		var err error
		db, err = sql.Open("postgres", dbURL)
		if err != nil {
			fmt.Println("⚠️ Failed to connect to DB:", err)
		} else {
			_, err = db.Exec(`CREATE TABLE IF NOT EXISTS captures (
				id TEXT PRIMARY KEY, amount FLOAT, status TEXT, currency TEXT, date TEXT, is_live BOOLEAN
			)`)
			if err == nil {
				fmt.Println("✅ Database connected.")
				reconcileFallback() // Check for any failed DB writes from previous runs
			}
		}
	}

	clientID := strings.TrimSpace(os.Getenv("PAYPAL_CLIENT_ID"))
	clientSecret := strings.TrimSpace(os.Getenv("PAYPAL_CLIENT_SECRET"))
	groqKey := strings.TrimSpace(os.Getenv("GROQ_API_KEY"))

	mux := http.NewServeMux()
	fs := http.FileServer(http.Dir("./static"))
	mux.Handle("/", fs)

	mux.HandleFunc("/api/create-order", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			sendJSONError(w, "Method not allowed", 405)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()

		token, err := getPayPalToken(ctx, clientID, clientSecret)
		if err != nil {
			sendJSONError(w, "Auth Error: "+err.Error(), 401)
			return
		}

		returnURL := "https://paypal-ai-auditor.onrender.com/?success=true"
		orderID, approveURL, err := createPayPalOrder(ctx, token, returnURL)
		if err != nil {
			sendJSONError(w, "Order Error: "+err.Error(), 500)
			return
		}

		// Strict validation: Ensure approval URL exists
		if approveURL == "" {
			sendJSONError(w, "PayPal did not return an approval link.", 500)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"id": orderID, "approveURL": approveURL})
	})

	mux.HandleFunc("/api/capture-order", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			sendJSONError(w, "Method not allowed", 405)
			return
		}
		var reqBody struct {
			OrderID string `json:"orderID"`
		}
		if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
			sendJSONError(w, "Invalid body", 400)
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()

		token, err := getPayPalToken(ctx, clientID, clientSecret)
		if err != nil {
			sendJSONError(w, "Auth Error: "+err.Error(), 401)
			return
		}

		// Strict Approval Check
		status, err := verifyPayPalOrder(ctx, token, reqBody.OrderID)
		if err != nil || status != "APPROVED" {
			sendJSONError(w, fmt.Sprintf("Order status is %s. Must be APPROVED to capture.", status), 400)
			return
		}

		payment, err := capturePayPalOrder(ctx, token, reqBody.OrderID)
		if err != nil {
			sendJSONError(w, "Capture Error: "+err.Error(), 500)
			return
		}

		// Persistence with Fallback
		if db != nil {
			_, err = db.Exec(`INSERT INTO captures (id, amount, status, currency, date, is_live) 
				VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT (id) DO NOTHING`,
				payment.ID, payment.Amount, payment.Status, payment.Currency, payment.Date, payment.IsLive)

			if err != nil {
				fmt.Println("🚨 CRITICAL: DB insert failed! Saving to fallback.json:", err)
				saveToFallback(payment)
				// We still return 200 because the money was successfully captured
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]interface{}{"status": "success", "note": "Saved to fallback due to DB error"})
				return
			}
		} else {
			// If DB is nil (offline), save to fallback
			saveToFallback(payment)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(payment)
	})

	mux.HandleFunc("/api/spend-intelligence", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()

		liveRecords, _ := getRecordsFromDB()
		demoRecords := getMockSpendRecords()
		allRecords := append(liveRecords, demoRecords...)

		summary := calculateSpendIntelligence(allRecords)
		summary.AIInsight = generateAIInsight(ctx, groqKey, summary)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(summary)
	})

	mux.HandleFunc("/api/transactions", func(w http.ResponseWriter, r *http.Request) {
		liveRecords, _ := getRecordsFromDB()
		demoRecords := getMockSpendRecords()

		// Ensure we don't serve duplicates if fallback was just synced
		var allRecords []PaymentRecord
		seen := make(map[string]bool)
		for _, r := range append(liveRecords, demoRecords...) {
			if !seen[r.ID] {
				allRecords = append(allRecords, r)
				seen[r.ID] = true
			}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(allRecords)
	})

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	fmt.Println("🚀 Server starting on port " + port)
	http.ListenAndServe(":"+port, mux)
}

// --- Fallback Logic ---
func saveToFallback(p PaymentRecord) {
	f, err := os.OpenFile(fallbackFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	json.NewEncoder(f).Encode(p)
}

func reconcileFallback() {
	if _, err := os.Stat(fallbackFile); os.IsNotExist(err) {
		return
	}
	fmt.Println("🔄 Found fallback file. Syncing to DB...")

	file, err := os.ReadFile(fallbackFile)
	if err != nil {
		return
	}

	decoder := json.NewDecoder(bytes.NewReader(file))
	for decoder.More() {
		var p PaymentRecord
		if err := decoder.Decode(&p); err == nil {
			db.Exec(`INSERT INTO captures (id, amount, status, currency, date, is_live) 
				VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT (id) DO NOTHING`,
				p.ID, p.Amount, p.Status, p.Currency, p.Date, p.IsLive)
		}
	}
	os.Remove(fallbackFile)
	fmt.Println("✅ Fallback sync complete. File removed.")
}

// --- Data Fetching ---
func getRecordsFromDB() ([]PaymentRecord, error) {
	var records []PaymentRecord
	if db == nil {
		return records, nil
	}
	rows, err := db.Query("SELECT id, amount, status, currency, date, is_live FROM captures ORDER BY date DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var p PaymentRecord
		if err := rows.Scan(&p.ID, &p.Amount, &p.Status, &p.Currency, &p.Date, &p.IsLive); err == nil {
			records = append(records, p)
		}
	}
	return records, nil
}

// --- Verification & Capture ---
func verifyPayPalOrder(ctx context.Context, token, orderID string) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("https://api-m.sandbox.paypal.com/v2/checkout/orders/%s", orderID), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("failed to fetch order")
	}
	var result struct {
		Status string `json:"status"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	return result.Status, nil // Returns exactly what PayPal says (e.g. APPROVED, COMPLETED)
}

func capturePayPalOrder(ctx context.Context, token, orderID string) (PaymentRecord, error) {
	req, _ := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("https://api-m.sandbox.paypal.com/v2/checkout/orders/%s/capture", orderID), nil)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return PaymentRecord{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return PaymentRecord{}, fmt.Errorf("capture failed %d", resp.StatusCode)
	}

	var paypalResult struct {
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
	json.NewDecoder(resp.Body).Decode(&paypalResult)

	if len(paypalResult.PurchaseUnits) == 0 || len(paypalResult.PurchaseUnits[0].Payments.Captures) == 0 {
		return PaymentRecord{}, fmt.Errorf("no captures")
	}
	capture := paypalResult.PurchaseUnits[0].Payments.Captures[0]
	if capture.Status != "COMPLETED" {
		return PaymentRecord{}, fmt.Errorf("status %s", capture.Status)
	}

	var amt float64
	fmt.Sscanf(capture.Amount.Value, "%f", &amt)
	return PaymentRecord{ID: capture.ID, Amount: amt, Status: capture.Status, Currency: capture.Amount.CurrencyCode, Date: time.Now().Format(time.RFC3339), IsLive: true}, nil
}

// --- Helpers ---
func getPayPalToken(ctx context.Context, clientID, secret string) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, "POST", "https://api-m.sandbox.paypal.com/v1/oauth2/token", strings.NewReader("grant_type=client_credentials"))
	req.SetBasicAuth(clientID, secret)
	req.Header.Add("Content-Type", "application/x-www-form-urlencoded")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var result struct {
		AccessToken string `json:"access_token"`
	}
	body, _ := io.ReadAll(resp.Body)
	json.Unmarshal(body, &result)
	return result.AccessToken, nil
}

func createPayPalOrder(ctx context.Context, token, returnURL string) (string, string, error) {
	payload := map[string]interface{}{
		"intent":              "CAPTURE",
		"purchase_units":      []map[string]interface{}{{"amount": map[string]string{"currency_code": "USD", "value": "25.00"}}},
		"application_context": map[string]string{"return_url": returnURL, "cancel_url": "https://paypal-ai-auditor.onrender.com/?cancel=true"},
	}
	jsonPayload, _ := json.Marshal(payload)
	req, _ := http.NewRequestWithContext(ctx, "POST", "https://api-m.sandbox.paypal.com/v2/checkout/orders", bytes.NewBuffer(jsonPayload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return "", "", fmt.Errorf("PayPal error %d", resp.StatusCode)
	}

	var result struct {
		ID    string `json:"id"`
		Links []struct {
			Href string `json:"href"`
			Rel  string `json:"rel"`
		} `json:"links"`
	}
	json.NewDecoder(resp.Body).Decode(&result)

	var approveURL string
	for _, link := range result.Links {
		if link.Rel == "approve" || link.Rel == "payer-action" {
			approveURL = link.Href
			break
		}
	}
	return result.ID, approveURL, nil
}

func sendJSONError(w http.ResponseWriter, msg string, code int) {
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func getMockSpendRecords() []PaymentRecord {
	return []PaymentRecord{
		{ID: "DEMO-CAP-101", Amount: 25.00, Status: "COMPLETED", Currency: "USD", Date: "2026-10-01", IsLive: false},
		{ID: "DEMO-CAP-102", Amount: 150.50, Status: "COMPLETED", Currency: "USD", Date: "2026-10-05", IsLive: false},
		{ID: "DEMO-CAP-104", Amount: 0.00, Status: "DECLINED", Currency: "USD", Date: "2026-10-10", IsLive: false},
	}
}

func calculateSpendIntelligence(records []PaymentRecord) SpendSummary {
	var total float64
	var liveSuccess, liveFailed int

	for _, r := range records {
		if r.IsLive {
			if r.Status == "COMPLETED" {
				total += r.Amount
				liveSuccess++
			}
			// We do not store live failures in the captures table, so this is 0 for now
		} else if r.Status == "DECLINED" || r.Status == "FAILED" {
			liveFailed++ // Only counts synthetic failures right now
		}
	}

	successRate := 0.0
	if liveSuccess > 0 {
		successRate = (float64(liveSuccess) / float64(liveSuccess+liveFailed)) * 100
	}

	return SpendSummary{TotalCaptured: total, LiveSuccess: liveSuccess, LiveFailed: liveFailed, SuccessRate: successRate}
}

func generateAIInsight(ctx context.Context, apiKey string, summary SpendSummary) string {
	prompt := fmt.Sprintf(`You are a merchant AI copilot. Analyze this exact data:
Live Revenue: $%.2f
Live Successful Payments: %d
Synthetic Demo Failures: %d

Based ONLY on these facts, provide a 1-sentence actionable insight for a merchant. Max 20 words.`, summary.TotalCaptured, summary.LiveSuccess, summary.LiveFailed)

	payload := map[string]interface{}{"model": "openai/gpt-oss-20b", "messages": []map[string]string{{"role": "user", "content": prompt}}}
	jsonPayload, _ := json.Marshal(payload)
	req, _ := http.NewRequestWithContext(ctx, "POST", "https://api.groq.com/openai/v1/chat/completions", bytes.NewBuffer(jsonPayload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil || resp.StatusCode != 200 {
		return "AI insight currently unavailable."
	}
	defer resp.Body.Close()
	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	if len(result.Choices) == 0 {
		return "AI insight unavailable."
	}
	return strings.TrimSpace(result.Choices[0].Message.Content)
}
