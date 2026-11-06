package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const maxWebhookBodyBytes = 1 << 20

type paypalWebhookEvent struct {
	ID        string          `json:"id"`
	EventType string          `json:"event_type"`
	Resource  json.RawMessage `json:"resource"`
}

type webhookProcessor func(context.Context, paypalWebhookEvent, []byte) (bool, error)

var processVerifiedWebhook = func(ctx context.Context, event paypalWebhookEvent, body []byte) (bool, error) {
	if db == nil {
		return false, errors.New("database unavailable")
	}
	return processWebhookReceipt(ctx, db, event, body)
}

type webhookExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

var insertWebhookAttempt = func(ctx context.Context, execer webhookExecer, attempt PaymentAttempt) error {
	_, err := execer.ExecContext(ctx, `INSERT INTO attempts
		(paypal_order_id, capture_id, debug_id, stage, outcome, reason_code, reason_detail, http_status, amount_minor, currency, is_synthetic)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		attempt.PayPalOrderID, attempt.CaptureID, attempt.DebugID, attempt.Stage, attempt.Outcome,
		nullIfEmpty(attempt.ReasonCode), nullIfEmpty(attempt.ReasonDetail), attempt.HTTPStatus,
		attempt.AmountMinor, attempt.Currency, attempt.IsSynthetic)
	return err
}

func processWebhookReceipt(ctx context.Context, targetDB *sql.DB, event paypalWebhookEvent, rawBody []byte) (bool, error) {
	if targetDB == nil {
		return false, errors.New("database unavailable")
	}
	result, err := targetDB.ExecContext(ctx, `INSERT INTO webhook_events (event_id, event_type, resource_id, payload)
		VALUES ($1, $2, $3, $4::jsonb) ON CONFLICT (event_id) DO NOTHING`, event.ID, event.EventType, webhookResourceID(event.Resource), string(rawBody))
	if err != nil {
		return false, err
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return false, err
	}

	tx, err := targetDB.BeginTx(ctx, nil)
	if err != nil {
		_ = persistWebhookProcessError(ctx, targetDB, event.ID, "receipt_transaction_failed")
		return false, err
	}
	defer tx.Rollback()
	var storedType string
	var storedPayload []byte
	var processedAt sql.NullTime
	if err := tx.QueryRowContext(ctx, `SELECT event_type, payload, processed_at FROM webhook_events WHERE event_id = $1 FOR UPDATE`, event.ID).
		Scan(&storedType, &storedPayload, &processedAt); err != nil {
		_ = tx.Rollback()
		_ = persistWebhookProcessError(ctx, targetDB, event.ID, "receipt_read_failed")
		return false, err
	}
	if processedAt.Valid {
		if err := tx.Commit(); err != nil {
			return false, err
		}
		return inserted == 0, nil
	}

	var storedEvent paypalWebhookEvent
	if err := json.Unmarshal(storedPayload, &storedEvent); err != nil {
		return false, recordWebhookProcessingError(ctx, targetDB, tx, event.ID, "stored_payload_invalid")
	}
	storedEvent.EventType = storedType
	if attempt, supported, err := webhookAttempt(storedEvent); err != nil {
		if updateErr := updateWebhookProcessError(ctx, tx, event.ID, safeWebhookError(err)); updateErr != nil {
			return false, errors.Join(err, updateErr)
		}
		if commitErr := tx.Commit(); commitErr != nil {
			return false, commitErr
		}
		return false, err
	} else if supported {
		if err := insertWebhookAttempt(ctx, tx, attempt); err != nil {
			_ = tx.Rollback()
			_ = persistWebhookProcessError(ctx, targetDB, event.ID, "attempt_persistence_failed")
			return false, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE webhook_events SET processed_at = NOW(), process_error = NULL WHERE event_id = $1`, event.ID); err != nil {
		_ = tx.Rollback()
		_ = persistWebhookProcessError(ctx, targetDB, event.ID, "receipt_update_failed")
		return false, err
	}
	if err := tx.Commit(); err != nil {
		_ = persistWebhookProcessError(ctx, targetDB, event.ID, "receipt_commit_failed")
		return false, err
	}
	return inserted == 0, nil
}

func webhookResourceID(resource json.RawMessage) string {
	var value struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(resource, &value)
	return value.ID
}

func webhookAttempt(event paypalWebhookEvent) (PaymentAttempt, bool, error) {
	switch event.EventType {
	case "PAYMENT.CAPTURE.COMPLETED", "PAYMENT.CAPTURE.DENIED", "PAYMENT.CAPTURE.REFUNDED":
	default:
		return PaymentAttempt{}, false, nil
	}
	var resource struct {
		ID     string `json:"id"`
		Amount struct {
			Value        string `json:"value"`
			CurrencyCode string `json:"currency_code"`
		} `json:"amount"`
		StatusDetails struct {
			Reason string `json:"reason"`
		} `json:"status_details"`
		SupplementaryData struct {
			RelatedIDs struct {
				OrderID string `json:"order_id"`
			} `json:"related_ids"`
		} `json:"supplementary_data"`
	}
	if err := json.Unmarshal(event.Resource, &resource); err != nil {
		return PaymentAttempt{}, false, fmt.Errorf("webhook capture resource is invalid")
	}
	if strings.TrimSpace(resource.ID) == "" || strings.TrimSpace(resource.Amount.CurrencyCode) == "" {
		return PaymentAttempt{}, false, errors.New("webhook capture is missing id or currency")
	}
	amountMinor, err := parseMinorUnits(resource.Amount.Value, resource.Amount.CurrencyCode)
	if err != nil {
		return PaymentAttempt{}, false, errors.New("webhook capture amount is invalid")
	}
	attempt := PaymentAttempt{
		PayPalOrderID: resource.SupplementaryData.RelatedIDs.OrderID,
		CaptureID:     resource.ID,
		Stage:         "webhook",
		Outcome:       "succeeded",
		AmountMinor:   amountMinor,
		Currency:      strings.ToUpper(resource.Amount.CurrencyCode),
	}
	switch event.EventType {
	case "PAYMENT.CAPTURE.COMPLETED":
		attempt.ReasonCode = "PAYMENT.CAPTURE.COMPLETED"
		attempt.ReasonDetail = "PayPal reported capture completed"
	case "PAYMENT.CAPTURE.REFUNDED":
		attempt.ReasonCode = "PAYMENT.CAPTURE.REFUNDED"
		attempt.ReasonDetail = "PayPal reported capture refunded"
	case "PAYMENT.CAPTURE.DENIED":
		attempt.ReasonCode = strings.ToUpper(strings.TrimSpace(resource.StatusDetails.Reason))
		if attempt.ReasonCode == "" {
			attempt.ReasonCode = "UNCLASSIFIED"
		}
		attempt.ReasonDetail = "PayPal reported capture denied"
		if isDeclineReasonCode(attempt.ReasonCode) {
			attempt.Outcome = "declined"
		} else {
			attempt.Outcome = "failed"
		}
	}
	return attempt, true, nil
}

func nullIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func safeWebhookError(err error) string {
	if err == nil {
		return "processing_failed"
	}
	message := strings.TrimSpace(err.Error())
	if message == "" {
		return "processing_failed"
	}
	if len(message) > 500 {
		message = message[:500]
	}
	return message
}

func updateWebhookProcessError(ctx context.Context, tx *sql.Tx, eventID, processError string) error {
	_, err := tx.ExecContext(ctx, `UPDATE webhook_events SET process_error = $2 WHERE event_id = $1`, eventID, processError)
	return err
}

func persistWebhookProcessError(ctx context.Context, targetDB *sql.DB, eventID, processError string) error {
	_, err := targetDB.ExecContext(ctx, `UPDATE webhook_events SET process_error = $2, processed_at = NULL WHERE event_id = $1`, eventID, processError)
	return err
}

func recordWebhookProcessingError(ctx context.Context, targetDB *sql.DB, tx *sql.Tx, eventID, processError string) error {
	if err := updateWebhookProcessError(ctx, tx, eventID, processError); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		_ = persistWebhookProcessError(ctx, targetDB, eventID, processError)
		return err
	}
	return errors.New(processError)
}

func makePayPalWebhookHandler(clientID, clientSecret, webhookID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			sendJSONError(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
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
		duplicate, err := processVerifiedWebhook(ctx, event, rawBody)
		if err != nil {
			sendJSONError(w, "Webhook event processing failed", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "duplicate": duplicate})
	}
}

func verifyPayPalWebhookSignature(ctx context.Context, token, webhookID string, headers http.Header, rawBody []byte) (bool, error) {
	if !json.Valid(rawBody) {
		return false, errors.New("invalid webhook JSON")
	}
	values := []string{
		headers.Get("PayPal-Auth-Algo"), headers.Get("PayPal-Cert-Url"), headers.Get("PayPal-Transmission-Id"),
		headers.Get("PayPal-Transmission-Sig"), headers.Get("PayPal-Transmission-Time"), webhookID,
	}
	for _, value := range values {
		if value == "" {
			return false, nil
		}
	}
	keys := []string{"auth_algo", "cert_url", "transmission_id", "transmission_sig", "transmission_time", "webhook_id"}
	var requestBody strings.Builder
	requestBody.WriteByte('{')
	for i, key := range keys {
		if i > 0 {
			requestBody.WriteByte(',')
		}
		encodedKey, _ := json.Marshal(key)
		encodedValue, _ := json.Marshal(values[i])
		requestBody.Write(encodedKey)
		requestBody.WriteByte(':')
		requestBody.Write(encodedValue)
	}
	requestBody.WriteString(`,"webhook_event":`)
	requestBody.Write(rawBody)
	requestBody.WriteByte('}')
	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api-m.sandbox.paypal.com/v1/notifications/verify-webhook-signature", strings.NewReader(requestBody.String()))
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
	return result.VerificationStatus == "SUCCESS", nil
}
