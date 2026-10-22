package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
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

type RecurringSub struct {
	Merchant      string
	AverageAmount float64
	AnnualCost    float64
	Count         int
}

type AIResult struct {
	Merchant       string `json:"merchant"`
	Category       string `json:"category"`
	Recommendation string `json:"recommendation"`
	Evidence       string `json:"evidence"`
}

type FinalResult struct {
	Merchant       string  `json:"merchant"`
	Category       string  `json:"category"`
	AnnualCost     float64 `json:"annual_cost"`
	Recommendation string  `json:"recommendation"`
	Evidence       string  `json:"evidence"`
}

func main() {
	_ = godotenv.Load()
	clientID := os.Getenv("PAYPAL_CLIENT_ID")
	clientSecret := os.Getenv("PAYPAL_CLIENT_SECRET")
	groqKey := os.Getenv("GROQ_API_KEY")

	fs := http.FileServer(http.Dir("./static"))
	http.Handle("/", fs)

	http.HandleFunc("/api/analyze", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			sendJSONError(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Content-Type", "application/json")

		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()

		// 1. Auth (Proves integration, but we are using demo data for the hackathon)
		_, err := getPayPalToken(ctx, clientID, clientSecret)
		if err != nil {
			log.Println("⚠️ PayPal Auth failed, proceeding with labeled demo data.")
		} else {
			log.Println("✅ PayPal Authenticated (Demo Data mode active).")
		}

		// 2. Fetch Data
		transactions := getDemoTransactions()

		// 3. Deterministic Engine
		recurringSubs, err := findRecurringSubscriptions(transactions)
		if err != nil {
			sendJSONError(w, "Error analyzing transactions", http.StatusInternalServerError)
			return
		}

		if len(recurringSubs) == 0 {
			w.Write([]byte("[]"))
			return
		}

		// 4. AI Enrichment & Validation
		finalResults := enrichAndValidateAI(ctx, recurringSubs, groqKey)
		json.NewEncoder(w).Encode(finalResults)
	})

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	fmt.Println("🚀 Server starting on port " + port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatal(err)
	}
}

func sendJSONError(w http.ResponseWriter, msg string, code int) {
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func getPayPalToken(ctx context.Context, clientID, secret string) (string, error) {
	apiURL := "https://api-m.sandbox.paypal.com/v1/oauth2/token"
	data := url.Values{}
	data.Set("grant_type", "client_credentials")

	req, _ := http.NewRequestWithContext(ctx, "POST", apiURL, strings.NewReader(data.Encode()))
	req.SetBasicAuth(clientID, secret)
	req.Header.Add("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("auth failed")
	}

	var result struct {
		AccessToken string `json:"access_token"`
	}
	body, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, &result); err != nil {
		return "", err
	}
	return result.AccessToken, nil
}

func getDemoTransactions() []Transaction {
	// Explicitly labeled synthetic demo data to ensure safe, reproducible testing
	return []Transaction{
		{ID: "1", Amount: 15.99, Description: "Netflix", Date: "2026-05-01"},
		{ID: "2", Amount: 15.99, Description: "Netflix", Date: "2026-06-01"},
		{ID: "3", Amount: 15.99, Description: "Netflix", Date: "2026-07-01"},
		{ID: "4", Amount: 15.99, Description: "Netflix", Date: "2026-08-01"},

		{ID: "7", Amount: 9.99, Description: "Spotify", Date: "2026-05-03"},
		{ID: "8", Amount: 9.99, Description: "Spotify", Date: "2026-06-03"},
		{ID: "9", Amount: 9.99, Description: "Spotify", Date: "2026-07-03"},
		{ID: "10", Amount: 9.99, Description: "Spotify", Date: "2026-08-03"},

		// Irregular AWS (Should be flagged correctly or filtered depending on interval)
		{ID: "13", Amount: 45.00, Description: "AWS", Date: "2026-05-07"},
		{ID: "14", Amount: 49.99, Description: "AWS", Date: "2026-06-07"},
		{ID: "15", Amount: 52.50, Description: "AWS", Date: "2026-07-07"},
		{ID: "16", Amount: 48.00, Description: "AWS", Date: "2026-08-07"},

		{ID: "20", Amount: 120.00, Description: "Nike", Date: "2026-10-12"}, // One-off
	}
}

func findRecurringSubscriptions(transactions []Transaction) ([]RecurringSub, error) {
	merchantMap := make(map[string][]Transaction)

	// Normalize merchant names and group
	for _, t := range transactions {
		normName := strings.Title(strings.ToLower(strings.TrimSpace(t.Description)))
		merchantMap[normName] = append(merchantMap[normName], t)
	}

	var subs []RecurringSub
	for merchant, txs := range merchantMap {
		if len(txs) < 3 {
			continue
		}

		// Sort by date
		sort.Slice(txs, func(i, j int) bool {
			return txs[i].Date < txs[j].Date
		})

		isMonthly := true
		for i := 1; i < len(txs); i++ {
			t1, err1 := time.Parse("2006-01-02", txs[i-1].Date)
			t2, err2 := time.Parse("2006-01-02", txs[i].Date)
			if err1 != nil || err2 != nil {
				isMonthly = false
				break
			}

			days := int(t2.Sub(t1).Hours() / 24)
			// Allow 25-35 days for monthly tolerance
			if days < 25 || days > 35 {
				isMonthly = false
				break
			}
		}

		if isMonthly {
			var total float64
			for _, t := range txs {
				total += t.Amount
			}
			avg := total / float64(len(txs))
			subs = append(subs, RecurringSub{
				Merchant:      merchant,
				AverageAmount: avg,
				AnnualCost:    avg * 12,
				Count:         len(txs),
			})
		}
	}
	return subs, nil
}

func enrichAndValidateAI(ctx context.Context, subs []RecurringSub, apiKey string) []FinalResult {
	// 1. Prepare fallback
	var finalResults []FinalResult
	subMap := make(map[string]RecurringSub)
	for _, s := range subs {
		subMap[strings.ToLower(s.Merchant)] = s
	}

	// 2. Call AI
	var subString strings.Builder
	for _, s := range subs {
		subString.WriteString(fmt.Sprintf("- %s: $%.2f/month\n", s.Merchant, s.AverageAmount))
	}

	prompt := fmt.Sprintf(`Here are confirmed subscriptions: %s. Categorize them and provide a recommendation. Return ONLY JSON array with keys: "merchant", "category", "recommendation", "evidence".`, subString.String())

	payload := map[string]interface{}{
		"model":    "openai/gpt-oss-20b",
		"messages": []map[string]string{{"role": "user", "content": prompt}},
	}
	jsonPayload, _ := json.Marshal(payload)

	client := &http.Client{Timeout: 10 * time.Second}
	req, _ := http.NewRequestWithContext(ctx, "POST", "https://api.groq.com/openai/v1/chat/completions", bytes.NewBuffer(jsonPayload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := client.Do(req)
	if err == nil && resp.StatusCode == 200 {
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		var groqResp struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if json.Unmarshal(body, &groqResp) == nil && len(groqResp.Choices) > 0 {
			text := groqResp.Choices[0].Message.Content
			text = strings.TrimPrefix(text, "```json")
			text = strings.TrimPrefix(text, "```")
			text = strings.TrimSuffix(text, "```")

			var aiResults []AIResult
			if json.Unmarshal([]byte(text), &aiResults) == nil {
				// 3. Validate AI Output
				for _, ai := range aiResults {
					normMerchant := strings.ToLower(ai.Merchant)
					if sub, exists := subMap[normMerchant]; exists {
						finalResults = append(finalResults, FinalResult{
							Merchant:       sub.Merchant, // Use deterministic name
							Category:       ai.Category,
							AnnualCost:     sub.AnnualCost, // Use deterministic cost
							Recommendation: ai.Recommendation,
							Evidence:       ai.Evidence,
						})
						delete(subMap, normMerchant) // Mark as processed
					}
				}
			}
		}
	}

	// 4. Add any remaining subs that AI missed or if AI failed completely
	for _, sub := range subMap {
		finalResults = append(finalResults, FinalResult{
			Merchant:       sub.Merchant,
			Category:       "Uncategorized",
			AnnualCost:     sub.AnnualCost,
			Recommendation: "AI analysis unavailable for this item.",
			Evidence:       fmt.Sprintf("Charged %d times.", sub.Count),
		})
	}

	if finalResults == nil {
		return []FinalResult{}
	}
	return finalResults
}
