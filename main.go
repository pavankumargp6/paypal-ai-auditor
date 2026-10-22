package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

type Transaction struct {
	ID          string  `json:"id"`
	Amount      float64 `json:"amount"`
	Description string  `json:"description"`
	Date        string  `json:"date"`
}

// Strict struct for AI output
type AIResult struct {
	Merchant       string  `json:"merchant"`
	Category       string  `json:"category"`
	AnnualCost     float64 `json:"annual_cost"`
	Recommendation string  `json:"recommendation"`
	Evidence       string  `json:"evidence"`
}

// Deterministic Engine struct
type RecurringSub struct {
	Merchant       string
	AverageAmount  float64
	AnnualCost     float64
	Count          int
	IntervalDays   int
}

func main() {
	_ = godotenv.Load()

	clientID := os.Getenv("PAYPAL_CLIENT_ID")
	clientSecret := os.Getenv("PAYPAL_CLIENT_SECRET")
	groqKey := os.Getenv("GROQ_API_KEY")

	fs := http.FileServer(http.Dir("./static"))
	http.Handle("/", fs)

	http.HandleFunc("/api/analyze", func(w http.ResponseWriter, r *http.Request) {
		// Phase 6: Security - Method check and CORS
		if r.Method != http.MethodGet {
			http.Error(w, `{"error": "Method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Content-Type", "application/json")

		// 1. Authenticate with PayPal
		_, err := getPayPalToken(clientID, clientSecret)
		if err != nil {
			fmt.Println("PayPal Auth Failed, using demo data.")
		} else {
			fmt.Println("✅ PayPal Authenticated.")
		}

		// 2. Fetch Data (Demo Dataset)
		transactions := getDemoTransactions()

		// 3. Deterministic Engine
		recurringSubs := findRecurringSubscriptions(transactions)

		// 4. AI Enrichment
		aiResponse, err := analyzeWithAI(recurringSubs, groqKey)
		if err != nil {
			fmt.Println("⚠️ AI Error:", err)
			// Fallback: return deterministic data mapped to the UI struct
			var fallback []AIResult
			for _, sub := range recurringSubs {
				fallback = append(fallback, AIResult{
					Merchant:       sub.Merchant,
					Category:       "Uncategorized",
					AnnualCost:     sub.AnnualCost,
					Recommendation: "AI enrichment unavailable. Review manually.",
					Evidence:       fmt.Sprintf("Charged %d times.", sub.Count),
				})
			}
			json.NewEncoder(w).Encode(fallback)
			return
		}

		w.Write([]byte(aiResponse))
	})

	fmt.Println("🚀 Server starting on http://localhost:8080")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		log.Fatal(err)
	}
}

func getPayPalToken(clientID, secret string) (string, error) {
	apiURL := "https://api-m.sandbox.paypal.com/v1/oauth2/token"
	data := url.Values{}
	data.Set("grant_type", "client_credentials")

	req, err := http.NewRequest("POST", apiURL, strings.NewReader(data.Encode()))
	if err != nil {
		return "", err
	}

	req.SetBasicAuth(clientID, secret)
	req.Header.Add("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("paypal API error: %s", string(body))
	}

	var result struct {
		AccessToken string `json:"access_token"`
	}
	json.Unmarshal(body, &result)
	return result.AccessToken, nil
}

func getDemoTransactions() []Transaction {
	return []Transaction{
		{ID: "1", Amount: 15.99, Description: "Netflix", Date: "2026-05-01"},
		{ID: "2", Amount: 15.99, Description: "Netflix", Date: "2026-06-01"},
		{ID: "3", Amount: 15.99, Description: "Netflix", Date: "2026-07-01"},
		{ID: "4", Amount: 15.99, Description: "Netflix", Date: "2026-08-01"},
		{ID: "5", Amount: 15.99, Description: "Netflix", Date: "2026-09-01"},
		{ID: "6", Amount: 15.99, Description: "Netflix", Date: "2026-10-01"},
		
		{ID: "7", Amount: 9.99, Description: "Spotify", Date: "2026-05-03"},
		{ID: "8", Amount: 9.99, Description: "Spotify", Date: "2026-06-03"},
		{ID: "9", Amount: 9.99, Description: "Spotify", Date: "2026-07-03"},
		{ID: "10", Amount: 9.99, Description: "Spotify", Date: "2026-08-03"},
		{ID: "11", Amount: 9.99, Description: "Spotify", Date: "2026-09-03"},
		{ID: "12", Amount: 9.99, Description: "Spotify", Date: "2026-10-03"},

		{ID: "13", Amount: 45.00, Description: "AWS", Date: "2026-05-07"},
		{ID: "14", Amount: 49.99, Description: "AWS", Date: "2026-06-07"},
		{ID: "15", Amount: 52.50, Description: "AWS", Date: "2026-07-07"},
		{ID: "16", Amount: 48.00, Description: "AWS", Date: "2026-08-07"},
		{ID: "17", Amount: 49.99, Description: "AWS", Date: "2026-09-07"},
		{ID: "18", Amount: 49.99, Description: "AWS", Date: "2026-10-07"},

		{ID: "19", Amount: 4.50, Description: "Starbucks", Date: "2026-10-05"},
		{ID: "20", Amount: 120.00, Description: "Nike", Date: "2026-10-12"},
	}
}

func findRecurringSubscriptions(transactions []Transaction) []RecurringSub {
	merchantMap := make(map[string][]Transaction)
	for _, t := range transactions {
		merchantMap[t.Description] = append(merchantMap[t.Description], t)
	}

	var subs []RecurringSub
	for merchant, txs := range merchantMap {
		if len(txs) >= 3 { // Must have at least 3 charges to be a sub
			var total float64
			for _, t := range txs {
				total += t.Amount
			}
			avg := total / float64(len(txs))
			
			// For demo purposes, assume monthly (12 payments/year)
			subs = append(subs, RecurringSub{
				Merchant:      merchant,
				AverageAmount: avg,
				AnnualCost:    avg * 12,
				Count:         len(txs),
				IntervalDays:  30,
			})
		}
	}
	return subs
}

func analyzeWithAI(subs []RecurringSub, apiKey string) (string, error) {
	var subString strings.Builder
	for _, s := range subs {
		subString.WriteString(fmt.Sprintf("- %s: $%.2f/month (Annual: $%.2f)\n", s.Merchant, s.AverageAmount, s.AnnualCost))
	}

	prompt := fmt.Sprintf(`You are a financial AI. Here are confirmed recurring subscriptions:
%s
Categorize them and provide a short recommendation. Return ONLY a JSON array with keys: "merchant" (string), "category" (string), "annual_cost" (number), "recommendation" (string), "evidence" (string).`, subString.String())

	url := "https://api.groq.com/openai/v1/chat/completions"
	payload := map[string]interface{}{
		"model": "openai/gpt-oss-20b",
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
		"temperature": 0.1,
	}
	jsonPayload, _ := json.Marshal(payload)

	client := &http.Client{Timeout: 10 * time.Second}
	req, _ := http.NewRequest("POST", url, bytes.NewBuffer(jsonPayload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := client.Do(req)
	if err != nil { return "", err }
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 { return "", fmt.Errorf("groq error: %s", string(body)) }

	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	json.Unmarshal(body, &result)
	if len(result.Choices) == 0 { return "", fmt.Errorf("empty response") }

	text := result.Choices[0].Message.Content
	text = strings.TrimPrefix(text, "```json")
	text = strings.TrimPrefix(text, "```")
	text = strings.TrimSuffix(text, "```")

	return strings.TrimSpace(text), nil
}
