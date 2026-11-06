package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const maxWebhookBodyBytes = 1 << 20

type paypalWebhookEvent struct {
	ID        string          `json:"id"`
	EventType string          `json:"event_type"`
	Raw       json.RawMessage `json:"-"`
}

var writeWebhookReceipt = func(ctx context.Context, eventID, eventType string, rawBody []byte) (bool, error) {
	if db == nil {
		return false, fmt.Errorf("database unavailable")
	}
	return insertWebhookReceipt(ctx, db, eventID, eventType, rawBody)
}

func insertWebhookReceipt(ctx context.Context, targetDB *sql.DB, eventID, eventType string, rawBody []byte) (bool, error) {
	if targetDB == nil {
		return false, fmt.Errorf("database unavailable")
	}
	result, err := targetDB.ExecContext(ctx, `INSERT INTO webhook_events (event_id, event_type, payload)
		VALUES ($1, $2, $3::jsonb) ON CONFLICT (event_id) DO NOTHING`, eventID, eventType, string(rawBody))
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return rows == 1, nil
}

func makePayPalWebhookHandler(clientID, clientSecret, webhookID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			sendJSONError(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Preserve and read the exact bytes PayPal signed before parsing the event.
		rawBody, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBodyBytes))
		if err != nil {
			sendJSONError(w, "Invalid webhook body", http.StatusRequestEntityTooLarge)
			return
		}
		if strings.TrimSpace(webhookID) == "" {
			sendJSONError(w, "Webhook verification is not configured", http.StatusServiceUnavailable)
			return
		}

		for _, header := range []string{"PayPal-Auth-Algo", "PayPal-Cert-Url", "PayPal-Transmission-Id", "PayPal-Transmission-Sig", "PayPal-Transmission-Time"} {
			if r.Header.Get(header) == "" {
				sendJSONError(w, "Invalid webhook signature", http.StatusUnauthorized)
				return
			}
		}
		var event paypalWebhookEvent
		if err := json.Unmarshal(rawBody, &event); err != nil || strings.TrimSpace(event.ID) == "" || strings.TrimSpace(event.EventType) == "" {
			sendJSONError(w, "Invalid webhook event", http.StatusBadRequest)
			return
		}

		ctx := r.Context()
		token, err := getPayPalToken(ctx, clientID, clientSecret)
		if err != nil {
			sendJSONError(w, "Webhook signature verification unavailable", http.StatusBadGateway)
			return
		}
		verified, err := verifyPayPalWebhookSignature(ctx, token, webhookID, r.Header, rawBody)
		if err != nil {
			sendJSONError(w, "Webhook signature verification unavailable", http.StatusBadGateway)
			return
		}
		if !verified {
			sendJSONError(w, "Invalid webhook signature", http.StatusUnauthorized)
			return
		}

		created, err := writeWebhookReceipt(ctx, event.ID, event.EventType, rawBody)
		if err != nil {
			sendJSONError(w, "Webhook receipt could not be stored", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "duplicate": !created})
	}
}

func verifyPayPalWebhookSignature(ctx context.Context, token, webhookID string, headers http.Header, rawBody []byte) (bool, error) {
	var event json.RawMessage
	if err := json.Unmarshal(rawBody, &event); err != nil {
		return false, err
	}
	payload := struct {
		AuthAlgo        string          `json:"auth_algo"`
		CertURL         string          `json:"cert_url"`
		TransmissionID  string          `json:"transmission_id"`
		TransmissionSig string          `json:"transmission_sig"`
		TransmissionAt  string          `json:"transmission_time"`
		WebhookID       string          `json:"webhook_id"`
		WebhookEvent    json.RawMessage `json:"webhook_event"`
	}{
		AuthAlgo:        headers.Get("PayPal-Auth-Algo"),
		CertURL:         headers.Get("PayPal-Cert-Url"),
		TransmissionID:  headers.Get("PayPal-Transmission-Id"),
		TransmissionSig: headers.Get("PayPal-Transmission-Sig"),
		TransmissionAt:  headers.Get("PayPal-Transmission-Time"),
		WebhookID:       webhookID,
		WebhookEvent:    event,
	}
	if payload.AuthAlgo == "" || payload.CertURL == "" || payload.TransmissionID == "" || payload.TransmissionSig == "" || payload.TransmissionAt == "" {
		return false, nil
	}
	requestBody, err := json.Marshal(payload)
	if err != nil {
		return false, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api-m.sandbox.paypal.com/v1/notifications/verify-webhook-signature", strings.NewReader(string(requestBody)))
	if err != nil {
		return false, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)

	response, err := httpClient.Do(request)
	if err != nil {
		return false, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxWebhookBodyBytes))
	if err != nil {
		return false, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return false, newPayPalAPIError(response.StatusCode, body)
	}
	var result struct {
		VerificationStatus string `json:"verification_status"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return false, err
	}
	return strings.EqualFold(result.VerificationStatus, "SUCCESS"), nil
}
