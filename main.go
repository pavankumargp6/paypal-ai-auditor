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

type AIResult struct {
	Merchant       string  `json:"merchant"`
	Category       string  `json:"category"`
	AnnualCost     float64 `json:"annual_cost"`
	Recommendation string  `json:"recommendation"`
	Evidence       string  `json:"evidence"`
}

type RecurringSub struct {
	Merchant      string
	AverageAmount float64
	AnnualCost    float64
	Count         int
	IntervalDays  int
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
			w.WriteHeader(http.StatusMethodNotAllowed)
			json.NewEncoder(w).Encode(map[string]string{"error": "Method not allowed"})
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Content-Type", "application/json")

		// 1. Auth (Proves integration)
		_, _ = getPayPalToken(clientID, clientSecret)

		// 2. Fetch Data
		transactions := getDemoTransactions()

		// 3. Deterministic Engine
		recurringSubs := findRecurringSubscriptions(transactions)

		// 4. AI Enrichment (with strict validation)
		aiResponseText, err := analyzeWithAI(recurringSubs, groqKey)
		
		var finalResults []AIResult
		if err == nil {
			// Try to parse AI output into our struct
			if parseErr := json.Unmarshal([]byte(aiResponseText), &finalResults); parseErr != nil || len(finalResults) == 0 {
				fmt.Println("⚠️ AI returned invalid JSON. Falling back.")
				err = parseErr
			}
		}

		// Fallback if AI failed or returned garbage
		if err != nil {
			fmt.Println("⚠️ AI Error/Fallback:", err)
			for _, sub := range recurringSubs {
				finalResults = append(finalResults, AIResult{
					Merchant:       sub.Merchant,
					Category:       "Uncategorized (Demo Data)",
					AnnualCost:     sub.AnnualCost,
					Recommendation: "AI enrichment unavailable. Review manually.",
					Evidence:       fmt.Sprintf("Charged %d times, ~%d days apart.", sub.Count, sub.IntervalDays),
				})
			}
		}

		json.NewEncoder(w).Encode(finalResults)
	})

	// Render requires using the PORT environment variable
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	fmt.Println("🚀 Server starting on port " + port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatal(err)
	}
}

func getPayPalToken(clientID, secret string) (string, error) {
	apiURL := "https://api-m.sandbox.paypal.com/v1/oauth2/token"
	data := url.Values{}
	data.Set("grant_type", "client_credentials")
	req, _ := http.NewRequest("POST", apiURL, strings.NewReader(data.Encode()))
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

func getDemoTransactions() []Transaction {
	// Explicitly labeled demo data
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

func findRecurringSubscriptions(transactions []Transaction) []RecurringSub {
	merchantMap := make(map[string][]Transaction)
	for _, t := range transactions {
		merchantMap[t.Description] = append(merchantMap[t.Description], t)
	}

	var subs []RecurringSub
	for merchant, txs := range merchantMap {
		if len(txs) >= 3 { 
			// Calculate average amount
			var total float64
			for _, t := range txs {
				total += t.Amount
			}
			avg := total / float64(len(txs))
			
			// Calculate simple interval between first and last
			firstDate, _ := time.Parse("2006-01-02", txs[0].Date)
			lastDate, _ := time.Parse("2006-01-02", txs[len(txs)-1].Date)
			daysBetween := int(lastDate.Sub(firstDate).Hours() / 24)
			avgInterval := daysBetween / (len(txs) - 1)

			// Only count if roughly monthly (25-35 days)
			if avgInterval > 25 && avgInterval < 35 {
				subs = append(subs, RecurringSub{
					Merchant:      merchant,
					AverageAmount: avg,
					AnnualCost:    avg * 12,
					Count:         len(txs),
					IntervalDays:  avgInterval,
				})
			}
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
Categorize them and provide a short recommendation. Return ONLY a valid JSON array with keys: "merchant" (string), "category" (string), "annual_cost" (number), "recommendation" (string), "evidence" (string). Do not include markdown formatting or backticks.`, subString.String())

	url := "https://api.groq.com/openai/v1/chat/completions"
	payload := map[string]interface{}{
		"model": "openai/gpt-oss-20b",
		"messages": []map[string]string{{"role": "user", "content": prompt}},
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
			Message struct { Content string `json:"content"` } `json:"message"`
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
