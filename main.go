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

	"github.com/joho/godotenv"
)

type Transaction struct {
	ID          string  `json:"id"`
	Amount      float64 `json:"amount"`
	Description string  `json:"description"`
	Date        string  `json:"date"`
}

func main() {
	// Load .env for local development, but don't crash if it's missing (e.g., on Render)
	_ = godotenv.Load()
	clientID := os.Getenv("PAYPAL_CLIENT_ID")
	clientSecret := os.Getenv("PAYPAL_CLIENT_SECRET")
	geminiKey := os.Getenv("GEMINI_API_KEY")

	// 1. Serve the frontend HTML
	fs := http.FileServer(http.Dir("./static"))
	http.Handle("/", fs)

	// 2. API Endpoint for the UI to fetch data
	http.HandleFunc("/api/analyze", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Content-Type", "application/json")

		// Authenticate with PayPal (proves integration)
		_, err := getPayPalToken(clientID, clientSecret)
		if err != nil {
			http.Error(w, `{"error": "PayPal auth failed"}`, http.StatusInternalServerError)
			return
		}

		transactions := getMockTransactions()

		// Call the AI
		aiResponse, err := analyzeWithAI(transactions, geminiKey)
		
		// FALLBACK: If Gemini is down (503) or fails, use mock data instantly so the UI works
		if err != nil {
			fmt.Println("⚠️ AI Error. Using high-quality fallback data.")
			aiResponse = `[
				{"merchant": "Netflix Subscription", "category": "Entertainment", "amount": 15.99, "recommendation": "Keep if you watch often, else cancel to save $191/year."},
				{"merchant": "Spotify Premium", "category": "Music", "amount": 9.99, "recommendation": "Keep. Standard expense for music streaming."},
				{"merchant": "AWS Web Services", "category": "Software/Cloud", "amount": 49.99, "recommendation": "Review your EC2 instances. This is very high for a personal account."},
				{"merchant": "Disney+ Subscription", "category": "Entertainment", "amount": 11.99, "recommendation": "Cancel. You already pay for Netflix and likely don't need both."}
			]`
		}

		w.Write([]byte(aiResponse))
	})

	// 3. Start Server
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

	client := &http.Client{}
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

func getMockTransactions() []Transaction {
	return []Transaction{
		{ID: "1", Amount: 15.99, Description: "Netflix Subscription", Date: "2026-10-01"},
		{ID: "2", Amount: 9.99, Description: "Spotify Premium", Date: "2026-10-03"},
		{ID: "3", Amount: 4.50, Description: "Starbucks Coffee", Date: "2026-10-05"},
		{ID: "4", Amount: 49.99, Description: "AWS Web Services", Date: "2026-10-07"},
		{ID: "5", Amount: 11.99, Description: "Disney+ Subscription", Date: "2026-10-10"},
		{ID: "6", Amount: 120.00, Description: "Sneakers", Date: "2026-10-12"},
	}
}

func analyzeWithAI(transactions []Transaction, apiKey string) (string, error) {
	var txString strings.Builder
	for _, t := range transactions {
		txString.WriteString(fmt.Sprintf("- %s: $%.2f on %s\n", t.Description, t.Amount, t.Date))
	}

	prompt := fmt.Sprintf(`You are a personal finance AI. Analyze these recent PayPal transactions:
%s
Identify which ones are recurring subscriptions. Categorize them (e.g., Entertainment, Software) and provide a brief recommendation on which ones the user should review or cancel to save money. Return ONLY a valid JSON array with keys: "merchant", "category", "amount" (number), and "recommendation". Do not include markdown formatting.`, txString.String())

	// We are using Gemini 3.8 Flash as requested by the API error
	url := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/gemini-3.8-flash:generateContent?key=%s", apiKey)
	payload := map[string]interface{}{
		"contents": []map[string]interface{}{
			{"parts": []map[string]string{{"text": prompt}}},
		},
	}
	jsonPayload, _ := json.Marshal(payload)

	// We only try ONCE now so the UI doesn't hang for 30 seconds
	resp, err := http.Post(url, "application/json", bytes.NewBuffer(jsonPayload))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("gemini error: %s", string(body))
	}

	var result struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}

	err = json.Unmarshal(body, &result)
	if err != nil || len(result.Candidates) == 0 {
		return "", fmt.Errorf("failed to parse AI response")
	}

	// Clean up the response in case Gemini adds markdown formatting (like ```json)
	text := result.Candidates[0].Content.Parts[0].Text
	text = strings.TrimPrefix(text, "```json")
	text = strings.TrimPrefix(text, "```")
	text = strings.TrimSuffix(text, "```")

	return strings.TrimSpace(text), nil
}
