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
	Interval      string // "Monthly", "Weekly", etc.
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
	DataSource     string  `json:"data_source"`
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

		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()

		// 1. Try to fetch real data from PayPal
		transactions, source, err := fetchPayPalTransactions(ctx, clientID, clientSecret)
		if err != nil || len(transactions) == 0 {
			log.Println("⚠️ PayPal API returned no data. Falling back to synthetic demo data.")
			transactions = getDemoTransactions()
			source = "Synthetic Demo Data"
		} else {
			log.Println("✅ Successfully fetched live PayPal Sandbox data.")
		}

		// 2. Deterministic Engine
		recurringSubs, err := findRecurringSubscriptions(transactions)
		if err != nil || len(recurringSubs) == 0 {
			w.Write([]byte("[]"))
			return
		}

		// 3. AI Enrichment & Validation
		finalResults := enrichAndValidateAI(ctx, recurringSubs, groqKey, source)
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

	client := &http.Client{Timeout: 10 * time.Second}
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
	json.Unmarshal(body, &result)
	return result.AccessToken, nil
}

// Phase 2: Implement a meaningful PayPal Data Flow
func fetchPayPalTransactions(ctx context.Context, clientID, secret string) ([]Transaction, string, error) {
	token, err := getPayPalToken(ctx, clientID, secret)
	if err != nil {
		return nil, "", err
	}

	// Fetch last 90 days of transactions
	endDate := time.Now().Format("2006-01-02T15:04:05-0700")
	startDate := time.Now().AddDate(0, -3, 0).Format("2006-01-02T15:04:05-0700")

	apiURL := fmt.Sprintf("https://api-m.sandbox.paypal.com/v1/reporting/transactions?start_date=%s&end_date=%s&fields=all", url.QueryEscape(startDate), url.QueryEscape(endDate))
	req, _ := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	req.Header.Add("Authorization", "Bearer "+token)
	req.Header.Add("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, "", fmt.Errorf("paypal reporting API error: %d", resp.StatusCode)
	}

	var paypalResp struct {
		TransactionDetails []struct {
			TransactionInfo struct {
				TransactionID     string `json:"transaction_id"`
				TransactionAmount struct {
					Value string `json:"value"`
				} `json:"transaction_amount"`
				TransactionInitiationDate string `json:"transaction_initiation_date"`
			} `json:"transaction_info"`
			PayerInfo struct {
				PayerName struct {
					GivenName string `json:"given_name"`
				} `json:"payer_name"`
			} `json:"payer_info"`
		} `json:"transaction_details"`
	}

	body, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, &paypalResp); err != nil {
		return nil, "", err
	}

	var txs []Transaction
	for _, t := range paypalResp.TransactionDetails {
		// Parse amount
		var amt float64
		fmt.Sscanf(t.TransactionInfo.TransactionAmount.Value, "%f", &amt)

		txs = append(txs, Transaction{
			ID:          t.TransactionInfo.TransactionID,
			Amount:      amt,
			Description: "PayPal Transaction",                             // Sandbox often doesn't have merchant names in this field
			Date:        t.TransactionInfo.TransactionInitiationDate[:10], // Just date part
		})
	}

	return txs, "Live PayPal Sandbox Data", nil
}

func getDemoTransactions() []Transaction {
	return []Transaction{
		{ID: "1", Amount: 15.99, Description: "Netflix", Date: "2026-05-01"},
		{ID: "2", Amount: 15.99, Description: "Netflix", Date: "2026-06-01"},
		{ID: "3", Amount: 15.99, Description: "Netflix", Date: "2026-07-01"},
		{ID: "4", Amount: 15.99, Description: "Netflix", Date: "2026-08-01"},

		{ID: "7", Amount: 9.99, Description: "Spotify", Date: "2026-05-03"},
		{ID: "8", Amount: 9.99, Description: "Spotify", Date: "2026-06-03"},
		{ID: "9", Amount: 9.99, Description: "Spotify", Date: "2026-07-03"},
		{ID: "10", Amount: 9.99, Description: "Spotify", Date: "2026-08-03"},

		{ID: "13", Amount: 45.00, Description: "AWS", Date: "2026-05-07"},
		{ID: "14", Amount: 49.99, Description: "AWS", Date: "2026-06-07"},
		{ID: "15", Amount: 52.50, Description: "AWS", Date: "2026-07-07"},
		{ID: "16", Amount: 48.00, Description: "AWS", Date: "2026-08-07"},
	}
}

// Phase 3: Harden the Deterministic Engine (Weekly, Monthly, Quarterly, Annual)
func findRecurringSubscriptions(transactions []Transaction) ([]RecurringSub, error) {
	merchantMap := make(map[string][]Transaction)
	for _, t := range transactions {
		normName := strings.Title(strings.ToLower(strings.TrimSpace(t.Description)))
		merchantMap[normName] = append(merchantMap[normName], t)
	}

	var subs []RecurringSub
	for merchant, txs := range merchantMap {
		if len(txs) < 3 {
			continue
		}

		sort.Slice(txs, func(i, j int) bool { return txs[i].Date < txs[j].Date })

		var intervals []int
		for i := 1; i < len(txs); i++ {
			t1, err1 := time.Parse("2006-01-02", txs[i-1].Date)
			t2, err2 := time.Parse("2006-01-02", txs[i].Date)
			if err1 == nil && err2 == nil {
				intervals = append(intervals, int(t2.Sub(t1).Hours()/24))
			}
		}

		if len(intervals) == 0 {
			continue
		}

		var totalDays int
		for _, d := range intervals {
			totalDays += d
		}
		avgDays := totalDays / len(intervals)

		var intervalName string
		var multiplier float64

		if avgDays >= 6 && avgDays <= 8 {
			intervalName = "Weekly"
			multiplier = 52.0
		} else if avgDays >= 28 && avgDays <= 31 {
			intervalName = "Monthly"
			multiplier = 12.0
		} else if avgDays >= 89 && avgDays <= 92 {
			intervalName = "Quarterly"
			multiplier = 4.0
		} else if avgDays >= 360 && avgDays <= 366 {
			intervalName = "Annually"
			multiplier = 1.0
		} else {
			continue // Irregular, skip
		}

		var total float64
		for _, t := range txs {
			total += t.Amount
		}
		avg := total / float64(len(txs))

		subs = append(subs, RecurringSub{
			Merchant:      merchant,
			AverageAmount: avg,
			AnnualCost:    avg * multiplier,
			Count:         len(txs),
			Interval:      intervalName,
		})
	}
	return subs, nil
}

func enrichAndValidateAI(ctx context.Context, subs []RecurringSub, apiKey, source string) []FinalResult {
	var finalResults []FinalResult
	subMap := make(map[string]RecurringSub)
	for _, s := range subs {
		subMap[strings.ToLower(s.Merchant)] = s
	}

	var subString strings.Builder
	for _, s := range subs {
		subString.WriteString(fmt.Sprintf("- %s: $%.2f/%s\n", s.Merchant, s.AverageAmount, s.Interval))
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
				for _, ai := range aiResults {
					normMerchant := strings.ToLower(ai.Merchant)
					if sub, exists := subMap[normMerchant]; exists {
						finalResults = append(finalResults, FinalResult{
							Merchant:       sub.Merchant,
							Category:       ai.Category,
							AnnualCost:     sub.AnnualCost,
							Recommendation: ai.Recommendation,
							Evidence:       fmt.Sprintf("%s (Charged %d times)", sub.Interval, sub.Count),
							DataSource:     source,
						})
						delete(subMap, normMerchant)
					}
				}
			}
		}
	}

	for _, sub := range subMap {
		finalResults = append(finalResults, FinalResult{
			Merchant:       sub.Merchant,
			Category:       "Uncategorized",
			AnnualCost:     sub.AnnualCost,
			Recommendation: "AI analysis unavailable.",
			Evidence:       fmt.Sprintf("%s (Charged %d times)", sub.Interval, sub.Count),
			DataSource:     source,
		})
	}

	if finalResults == nil {
		return []FinalResult{}
	}
	return finalResults
}
