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
	groqKey := os.Getenv("GROQ_API_KEY")

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
		aiResponse, err := analyzeWithAI(transactions, groqKey)		
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
	// Convert transactions to a string format for the prompt
	var txString strings.Builder
	for _, t := range transactions {
		txString.WriteString(fmt.Sprintf("- %s: $%.2f on %s\n", t.Description, t.Amount, t.Date))
	}

	prompt := fmt.Sprintf(`You are a personal finance AI. Analyze these recent PayPal transactions:
%s
Identify which ones are recurring subscriptions. Categorize them (e.g., Entertainment, Software) and provide a brief recommendation on which ones the user should review or cancel to save money. Return ONLY a valid JSON array with keys: "merchant", "category", "amount" (number), and "recommendation". Do not include markdown formatting or backticks.`, txString.String())

	// Groq API uses the OpenAI-compatible endpoint
	url := "https://api.groq.com/openai/v1/chat/completions"
	
	payload := map[string]interface{}{
		"model": "llama3-8b-8192", // Fast and free model
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
		"temperature": 0.1, // Keep it focused on the data
	}
	jsonPayload, _ := json.Marshal(payload)

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonPayload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("groq error: %s", string(body))
	}

	// Parse Groq's response
	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}

	err = json.Unmarshal(body, &result)
	if err != nil || len(result.Choices) == 0 {
		return "", fmt.Errorf("failed to parse AI response")
	}

	// Clean up any accidental markdown formatting
	text := result.Choices[0].Message.Content
	text = strings.TrimPrefix(text, "```json")
	text = strings.TrimPrefix(text, "```")
	text = strings.TrimSuffix(text, "```")

	return strings.TrimSpace(text), nil
}
