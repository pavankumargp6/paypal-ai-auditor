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
	"time"

	"github.com/joho/godotenv"
)

// --- Structs for Spend Intelligence ---
type PaymentRecord struct {
	ID       string  `json:"id"`
	Amount   float64 `json:"amount"`
	Status   string  `json:"status"`
	Currency string  `json:"currency"`
	Date     string  `json:"date"`
}

type SpendSummary struct {
	TotalCaptured float64 `json:"total_captured"`
	TotalCount    int     `json:"total_count"`
	FailedCount   int     `json:"failed_count"`
	AIInsight     string  `json:"ai_insight"`
}

// --- Main App ---
func main() {
	_ = godotenv.Load()
	clientID := os.Getenv("PAYPAL_CLIENT_ID")
	clientSecret := os.Getenv("PAYPAL_CLIENT_SECRET")
	groqKey := os.Getenv("GROQ_API_KEY")

	fs := http.FileServer(http.Dir("./static"))
	http.Handle("/", fs)

	// 1. Real PayPal Checkout Flow
	http.HandleFunc("/api/create-order", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			sendJSONError(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()

		token, err := getPayPalToken(ctx, clientID, clientSecret)
		if err != nil {
			sendJSONError(w, "PayPal auth failed", http.StatusUnauthorized)
			return
		}

		orderID, err := createPayPalOrder(ctx, token)
		if err != nil {
			sendJSONError(w, "Failed to create order: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"orderID": orderID})
	})

	http.HandleFunc("/api/capture-order", func(w http.ResponseWriter, r *http.Request) {
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
			sendJSONError(w, "PayPal auth failed", http.StatusUnauthorized)
			return
		}

		payment, err := capturePayPalOrder(ctx, token, reqBody.OrderID)
		if err != nil {
			sendJSONError(w, "Failed to capture order: "+err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(payment)
	})

	// 2. Spend Intelligence Analysis
	http.HandleFunc("/api/spend-intelligence", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			sendJSONError(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()

		// For the demo, we simulate the transactions this app has captured.
		// In production, you would fetch these from your database or PayPal API.
		records := getMockSpendRecords()
		summary := calculateSpendIntelligence(records)
		summary.AIInsight = generateAIInsight(ctx, groqKey, summary)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(summary)
	})

	// 3. Keep the Subscription Engine for the secondary demo
	http.HandleFunc("/api/analyze-subscriptions", func(w http.ResponseWriter, r *http.Request) {
		// (Your existing subscription logic goes here, returning the demo data)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(getMockSubscriptionsJSON()))
	})

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	fmt.Println("🚀 Server starting on port " + port)
	http.ListenAndServe(":"+port, nil)
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
	var result struct {
		AccessToken string `json:"access_token"`
	}
	body, _ := io.ReadAll(resp.Body)
	json.Unmarshal(body, &result)
	return result.AccessToken, nil
}

func createPayPalOrder(ctx context.Context, token string) (string, error) {
	payload := map[string]interface{}{
		"intent": "CAPTURE",
		"purchase_units": []map[string]interface{}{
			{"amount": map[string]string{"currency_code": "USD", "value": "25.00"}},
		},
	}
	jsonPayload, _ := json.Marshal(payload)
	req, _ := http.NewRequestWithContext(ctx, "POST", "https://api-m.sandbox.paypal.com/v2/checkout/orders", bytes.NewBuffer(jsonPayload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("PayPal error: %s", string(body))
	}

	var result struct {
		ID string `json:"id"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	return result.ID, nil
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
		return PaymentRecord{}, fmt.Errorf("Capture failed: %s", string(body))
	}

	// Parse the capture result
	var paypalResult struct {
		ID            string `json:"id"`
		Status        string `json:"status"`
		PurchaseUnits []struct {
			Payments struct {
				Captures []struct {
					ID     string `json:"id"`
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
	var amt float64
	fmt.Sscanf(capture.Amount.Value, "%f", &amt)

	return PaymentRecord{
		ID:       capture.ID,
		Amount:   amt,
		Status:   paypalResult.Status,
		Currency: capture.Amount.CurrencyCode,
		Date:     time.Now().Format(time.RFC3339),
	}, nil
}

// --- Spend Intelligence Engine ---
func getMockSpendRecords() []PaymentRecord {
	// Simulates historical payments to show the dashboard
	return []PaymentRecord{
		{ID: "CAP-101", Amount: 25.00, Status: "COMPLETED", Currency: "USD", Date: "2026-10-01"},
		{ID: "CAP-102", Amount: 150.50, Status: "COMPLETED", Currency: "USD", Date: "2026-10-05"},
		{ID: "CAP-103", Amount: 45.00, Status: "COMPLETED", Currency: "USD", Date: "2026-10-08"},
		{ID: "CAP-104", Amount: 0.00, Status: "DECLINED", Currency: "USD", Date: "2026-10-10"},
	}
}

func calculateSpendIntelligence(records []PaymentRecord) SpendSummary {
	var total float64
	var count, failed int
	for _, r := range records {
		if r.Status == "COMPLETED" {
			total += r.Amount
			count++
		} else {
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
	prompt := fmt.Sprintf(`You are a merchant AI copilot. Summarize this spend data for a dashboard:
Total Captured: $%.2f across %d successful payments. Failed payments: %d.
Return a 1-sentence actionable insight, max 15 words.`, summary.TotalCaptured, summary.TotalCount, summary.FailedCount)

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

func getMockSubscriptionsJSON() string {
	return `[{"merchant":"Netflix","category":"Video","annual_cost":191.88,"evidence":"Monthly (Charged 4 times)","recommendation":"Review plan","data_source":"Synthetic Demo"}]`
}
