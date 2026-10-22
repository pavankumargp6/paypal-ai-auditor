package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/joho/godotenv"
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
	TotalCount    int     `json:"total_count"`
	FailedCount   int     `json:"failed_count"`
	AIInsight     string  `json:"ai_insight"`
}

var (
	recordsLock     sync.Mutex
	capturedRecords []PaymentRecord
	dataFile        = "captures.json"
)

func main() {
	_ = godotenv.Load()
	// Load existing records from disk (survives restarts)
	loadRecords()

	clientID := strings.TrimSpace(os.Getenv("PAYPAL_CLIENT_ID"))
	clientSecret := strings.TrimSpace(os.Getenv("PAYPAL_CLIENT_SECRET"))
	groqKey := strings.TrimSpace(os.Getenv("GROQ_API_KEY"))

	mux := http.NewServeMux()
	fs := http.FileServer(http.Dir("./static"))
	mux.Handle("/", fs)

	mux.HandleFunc("/api/create-order", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			sendJSONError(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()

		token, err := getPayPalToken(ctx, clientID, clientSecret)
		if err != nil {
			sendJSONError(w, "Auth Error: "+err.Error(), http.StatusUnauthorized)
			return
		}

		returnURL := "https://paypal-ai-auditor.onrender.com/?success=true"
		orderID, approveURL, err := createPayPalOrder(ctx, token, returnURL)
		if err != nil {
			sendJSONError(w, "Order Error: "+err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"id":         orderID,
			"approveURL": approveURL,
		})
	})

	mux.HandleFunc("/api/capture-order", func(w http.ResponseWriter, r *http.Request) {
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

		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()

		token, err := getPayPalToken(ctx, clientID, clientSecret)
		if err != nil {
			sendJSONError(w, "Auth Error: "+err.Error(), http.StatusUnauthorized)
			return
		}

		// Verify order is actually APPROVED
		isApproved, err := verifyPayPalOrder(ctx, token, reqBody.OrderID)
		if err != nil || !isApproved {
			sendJSONError(w, "Order is not approved by buyer yet.", http.StatusBadRequest)
			return
		}

		payment, err := capturePayPalOrder(ctx, token, reqBody.OrderID)
		if err != nil {
			sendJSONError(w, "Capture Error: "+err.Error(), http.StatusInternalServerError)
			return
		}

		// Duplicate Protection
		recordsLock.Lock()
		for _, existing := range capturedRecords {
			if existing.ID == payment.ID {
				recordsLock.Unlock()
				sendJSONError(w, "This payment has already been captured.", http.StatusConflict)
				return
			}
		}
		capturedRecords = append(capturedRecords, payment)
		saveRecords() // Persist to disk
		recordsLock.Unlock()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(payment)
	})

	mux.HandleFunc("/api/spend-intelligence", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()

		recordsLock.Lock()
		allRecords := append([]PaymentRecord{}, capturedRecords...)
		recordsLock.Unlock()

		allRecords = append(allRecords, getMockSpendRecords()...)
		summary := calculateSpendIntelligence(allRecords)
		summary.AIInsight = generateAIInsight(ctx, groqKey, summary)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(summary)
	})

	mux.HandleFunc("/api/transactions", func(w http.ResponseWriter, r *http.Request) {
		recordsLock.Lock()
		allRecords := append([]PaymentRecord{}, capturedRecords...)
		recordsLock.Unlock()
		allRecords = append(allRecords, getMockSpendRecords()...)

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

// --- Persistence Helpers ---
func loadRecords() {
	file, err := os.ReadFile(dataFile)
	if err == nil {
		json.Unmarshal(file, &capturedRecords)
	}
}

func saveRecords() {
	// Note: Must be called while recordsLock is held
	data, _ := json.MarshalIndent(capturedRecords, "", "  ")
	os.WriteFile(dataFile, data, 0644)
}

func sendJSONError(w http.ResponseWriter, msg string, code int) {
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// --- PayPal API Helpers ---
func getPayPalToken(ctx context.Context, clientID, secret string) (string, error) {
	apiURL := "https://api-m.sandbox.paypal.com/v1/oauth2/token"
	data := url.Values{}
	data.Set("grant_type", "client_credentials")
	req, _ := http.NewRequestWithContext(ctx, "POST", apiURL, strings.NewReader(data.Encode()))
	req.SetBasicAuth(clientID, secret)
	req.Header.Add("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("HTTP %d - %s", resp.StatusCode, string(body))
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
		"intent": "CAPTURE",
		"purchase_units": []map[string]interface{}{
			{"amount": map[string]string{"currency_code": "USD", "value": "25.00"}},
		},
		"application_context": map[string]string{
			"return_url": returnURL,
			"cancel_url": "https://paypal-ai-auditor.onrender.com/?cancel=true",
		},
	}
	jsonPayload, _ := json.Marshal(payload)
	req, _ := http.NewRequestWithContext(ctx, "POST", "https://api-m.sandbox.paypal.com/v2/checkout/orders", bytes.NewBuffer(jsonPayload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return "", "", fmt.Errorf("PayPal error: %s", string(body))
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

func verifyPayPalOrder(ctx context.Context, token, orderID string) (bool, error) {
	url := fmt.Sprintf("https://api-m.sandbox.paypal.com/v2/checkout/orders/%s", orderID)
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return false, fmt.Errorf("failed to fetch order")
	}

	var result struct {
		Status string `json:"status"`
	}
	json.NewDecoder(resp.Body).Decode(&result)

	return result.Status == "APPROVED", nil
}

func capturePayPalOrder(ctx context.Context, token, orderID string) (PaymentRecord, error) {
	url := fmt.Sprintf("https://api-m.sandbox.paypal.com/v2/checkout/orders/%s/capture", orderID)
	req, _ := http.NewRequestWithContext(ctx, "POST", url, nil)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return PaymentRecord{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return PaymentRecord{}, fmt.Errorf("capture failed: %s", string(body))
	}

	var paypalResult struct {
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
	body, _ := io.ReadAll(resp.Body)
	json.Unmarshal(body, &paypalResult)

	if len(paypalResult.PurchaseUnits) == 0 || len(paypalResult.PurchaseUnits[0].Payments.Captures) == 0 {
		return PaymentRecord{}, fmt.Errorf("no captures found")
	}

	capture := paypalResult.PurchaseUnits[0].Payments.Captures[0]

	if capture.Status != "COMPLETED" {
		return PaymentRecord{}, fmt.Errorf("capture status is %s, expected COMPLETED", capture.Status)
	}

	var amt float64
	fmt.Sscanf(capture.Amount.Value, "%f", &amt)

	return PaymentRecord{
		ID:       capture.ID,
		Amount:   amt,
		Status:   capture.Status,
		Currency: capture.Amount.CurrencyCode,
		Date:     time.Now().Format(time.RFC3339),
		IsLive:   true,
	}, nil
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
	var count, failed int
	for _, r := range records {
		if r.IsLive && r.Status == "COMPLETED" {
			total += r.Amount
			count++
		} else if r.Status == "DECLINED" || r.Status == "FAILED" {
			failed++
		}
	}
	return SpendSummary{
		TotalCaptured: total,
		TotalCount:    count,
		FailedCount:   failed,
	}
}

func generateAIInsight(ctx context.Context, apiKey string, summary SpendSummary) string {
	// FIX 2: Force AI to use evidence and distinguish failures
	prompt := fmt.Sprintf(`You are a merchant AI copilot. Analyze this exact payment data:
Live Captured Revenue: $%.2f
Successful Payments: %d
Failed Payments: %d

Based ONLY on these numbers, provide a 1-sentence actionable insight. If there are failures, suggest improving checkout. If there are successes and no failures, suggest retaining customers. Max 20 words.`, summary.TotalCaptured, summary.TotalCount, summary.FailedCount)

	payload := map[string]interface{}{"model": "openai/gpt-oss-20b", "messages": []map[string]string{{"role": "user", "content": prompt}}}
	jsonPayload, _ := json.Marshal(payload)

	client := &http.Client{Timeout: 10 * time.Second}
	req, _ := http.NewRequestWithContext(ctx, "POST", "https://api.groq.com/openai/v1/chat/completions", bytes.NewBuffer(jsonPayload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := client.Do(req)
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
