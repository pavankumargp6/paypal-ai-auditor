package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

const leakageScoreQuery = `SELECT
	COALESCE(SUM(amount_minor) FILTER (WHERE status = 'open' AND evidence->>'is_synthetic' IS DISTINCT FROM 'true'), 0) AS lost_minor,
	COALESCE(SUM(amount_minor) FILTER (WHERE status IN ('open', 'acknowledged') AND evidence->>'is_synthetic' IS DISTINCT FROM 'true'), 0) AS recoverable_minor,
	COALESCE(SUM(amount_minor) FILTER (WHERE status = 'actioned' AND evidence->>'is_synthetic' IS DISTINCT FROM 'true'), 0) AS recovered_minor,
	COALESCE(SUM(amount_minor) FILTER (WHERE status = 'open' AND evidence->>'is_synthetic' = 'true'), 0) AS simulated_lost_minor,
	COALESCE(SUM(amount_minor) FILTER (WHERE status IN ('open', 'acknowledged') AND evidence->>'is_synthetic' = 'true'), 0) AS simulated_recoverable_minor,
	COALESCE(SUM(amount_minor) FILTER (WHERE status = 'actioned' AND evidence->>'is_synthetic' = 'true'), 0) AS simulated_recovered_minor
FROM findings`

type findingDraft struct {
	Fingerprint string
	Detector    string
	AmountMinor int64
	Currency    string
	Evidence    any
}

type captureObservation struct {
	OrderID     string
	CaptureID   string
	AmountMinor int64
	Currency    string
	Outcome     string
	Stage       string
	CreatedAt   time.Time
}

type successfulCapture struct {
	CaptureID   string `json:"capture_id"`
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
}

func detectDuplicateCaptures(observations []captureObservation) []findingDraft {
	byCaptureID := make(map[string][]captureObservation)
	byOrder := make(map[string][]captureObservation)
	for _, observation := range observations {
		if observation.CaptureID == "" {
			continue
		}
		byCaptureID[observation.CaptureID] = append(byCaptureID[observation.CaptureID], observation)
		if observation.Outcome == "succeeded" && observation.OrderID != "" {
			byOrder[observation.OrderID] = append(byOrder[observation.OrderID], observation)
		}
	}

	findings := make([]findingDraft, 0)
	captureIDs := make([]string, 0, len(byCaptureID))
	for captureID := range byCaptureID {
		captureIDs = append(captureIDs, captureID)
	}
	sort.Strings(captureIDs)
	for _, captureID := range captureIDs {
		rows := byCaptureID[captureID]
		if len(rows) < 2 {
			continue
		}
		findings = append(findings, findingDraft{
			Fingerprint: "duplicate_capture:capture:" + fingerprintToken(captureID),
			Detector:    "duplicate_capture",
			AmountMinor: 0,
			Currency:    validFindingCurrency(rows[0].Currency),
			Evidence: map[string]any{
				"reason":                "capture_id_persisted_multiple_times",
				"capture_ids":           []string{captureID},
				"persisted_count":       len(rows),
				"amount_minor":          rows[0].AmountMinor,
				"monetary_excess_known": false,
			},
		})
	}

	orderIDs := make([]string, 0, len(byOrder))
	for orderID := range byOrder {
		orderIDs = append(orderIDs, orderID)
	}
	sort.Strings(orderIDs)
	for _, orderID := range orderIDs {
		rows := byOrder[orderID]
		byID := make(map[string]successfulCapture)
		for _, row := range rows {
			byID[row.CaptureID] = successfulCapture{CaptureID: row.CaptureID, AmountMinor: row.AmountMinor, Currency: strings.ToUpper(row.Currency)}
		}
		if len(byID) < 2 {
			continue
		}
		ids := make([]string, 0, len(byID))
		for id := range byID {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		captures := make([]successfulCapture, 0, len(ids))
		var extraAmount int64
		currency := validFindingCurrency(byID[ids[0]].Currency)
		for i, id := range ids {
			capture := byID[id]
			captures = append(captures, capture)
			if i > 0 && capture.Currency == currency && capture.AmountMinor > 0 && extraAmount <= int64(^uint64(0)>>1)-capture.AmountMinor {
				extraAmount += capture.AmountMinor
			}
		}
		findings = append(findings, findingDraft{
			Fingerprint: "duplicate_capture:order:" + fingerprintToken(orderID),
			Detector:    "duplicate_capture",
			AmountMinor: extraAmount,
			Currency:    currency,
			Evidence: map[string]any{
				"reason":          "multiple_successful_captures_for_order",
				"paypal_order_id": orderID,
				"capture_ids":     ids,
				"captures":        captures,
			},
		})
	}
	return findings
}

func detectFeeMismatch(amountMinor, actualFeeMinor int64, currency string) (expected int64, delta int64, mismatch bool) {
	currency = strings.ToUpper(currency)
	if amountMinor < 0 || actualFeeMinor < 0 || !validCurrency(currency) {
		return 0, 0, false
	}
	// Work entirely in minor units. The fixed 30 is 30 minor units; dividing
	// before multiplying avoids overflowing for large int64 amounts.
	base := (amountMinor/1000)*29 + ((amountMinor%1000)*29+500)/1000
	if base > int64(^uint64(0)>>1)-30 {
		return 0, 0, false
	}
	expected = base + 30
	if actualFeeMinor >= expected {
		delta = actualFeeMinor - expected
	} else {
		delta = expected - actualFeeMinor
	}
	return expected, delta, delta > 1
}

func feeMismatchFinding(orderID, captureID, currency string, amountMinor, actualFeeMinor int64) (findingDraft, bool) {
	expected, delta, mismatch := detectFeeMismatch(amountMinor, actualFeeMinor, currency)
	if !mismatch {
		return findingDraft{}, false
	}
	currency = strings.ToUpper(currency)
	return findingDraft{
		Fingerprint: "fee_mismatch:" + fingerprintToken(captureID),
		Detector:    "fee_mismatch",
		AmountMinor: delta,
		Currency:    validFindingCurrency(currency),
		Evidence: map[string]any{
			"paypal_order_id":    orderID,
			"capture_id":         captureID,
			"amount_minor":       amountMinor,
			"expected_fee_minor": expected,
			"actual_fee_minor":   actualFeeMinor,
			"delta_minor":        delta,
			"currency":           currency,
		},
	}, true
}

func detectFeeMismatches(ctx context.Context, targetDB *sql.DB) ([]findingDraft, error) {
	rows, err := targetDB.QueryContext(ctx, `SELECT DISTINCT ON (w.resource_id)
		COALESCE(a.paypal_order_id, ''), w.resource_id,
		a.amount_minor, a.currency,
		w.payload #>> '{resource,seller_receivable_breakdown,paypal_fee,value}' AS actual_fee,
		COALESCE(w.payload #>> '{resource,seller_receivable_breakdown,paypal_fee,currency_code}', a.currency) AS fee_currency
		FROM webhook_events w
	JOIN attempts a ON a.capture_id = w.resource_id
	WHERE w.event_type = 'PAYMENT.CAPTURE.COMPLETED'
	  AND w.processed_at IS NOT NULL
	  AND a.stage IN ('capture', 'webhook')
	  AND a.outcome = 'succeeded'
	  AND a.is_synthetic = FALSE
	  AND w.payload #>> '{resource,seller_receivable_breakdown,paypal_fee,value}' IS NOT NULL
	ORDER BY w.resource_id, w.received_at DESC, a.created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	findings := make([]findingDraft, 0)
	for rows.Next() {
		var orderID, captureID, currency, feeValue, feeCurrency string
		var amountMinor int64
		if err := rows.Scan(&orderID, &captureID, &amountMinor, &currency, &feeValue, &feeCurrency); err != nil {
			return nil, err
		}
		if feeCurrency == "" {
			feeCurrency = currency
		}
		if !strings.EqualFold(currency, feeCurrency) {
			continue
		}
		actualFeeMinor, err := parseMinorUnits(feeValue, feeCurrency)
		if err != nil {
			continue
		}
		if finding, ok := feeMismatchFinding(orderID, captureID, currency, amountMinor, actualFeeMinor); ok {
			findings = append(findings, finding)
		}
	}
	return findings, rows.Err()
}

func runDeterministicDetectors(ctx context.Context, targetDB *sql.DB) ([]findingDraft, error) {
	if targetDB == nil {
		return nil, errors.New("database unavailable")
	}
	rows, err := targetDB.QueryContext(ctx, `SELECT paypal_order_id, capture_id, amount_minor, currency, outcome, stage, created_at
		FROM attempts
		WHERE is_synthetic = FALSE AND capture_id IS NOT NULL
		  AND stage IN ('capture', 'webhook')`)
	if err != nil {
		return nil, err
	}
	observations := make([]captureObservation, 0)
	for rows.Next() {
		var observation captureObservation
		if err := rows.Scan(&observation.OrderID, &observation.CaptureID, &observation.AmountMinor,
			&observation.Currency, &observation.Outcome, &observation.Stage, &observation.CreatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		observations = append(observations, observation)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	drafts := detectDuplicateCaptures(observations)
	fees, err := detectFeeMismatches(ctx, targetDB)
	if err != nil {
		return nil, err
	}
	drafts = append(drafts, fees...)
	for _, draft := range drafts {
		evidence, err := json.Marshal(draft.Evidence)
		if err != nil {
			return nil, fmt.Errorf("marshal detector evidence: %w", err)
		}
		_, err = targetDB.ExecContext(ctx, `INSERT INTO findings (fingerprint, detector, amount_minor, currency, evidence)
			VALUES ($1, $2, $3, $4, $5::jsonb) ON CONFLICT (fingerprint) DO UPDATE
			SET amount_minor = EXCLUDED.amount_minor, evidence = EXCLUDED.evidence, updated_at = NOW()
			WHERE findings.status = 'open'`,
			draft.Fingerprint, draft.Detector, draft.AmountMinor, draft.Currency, string(evidence))
		if err != nil {
			return nil, err
		}
	}
	return drafts, nil
}

func fingerprintToken(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func validFindingCurrency(currency string) string {
	currency = strings.ToUpper(currency)
	if !validCurrency(currency) {
		return "USD"
	}
	return currency
}

type leakageScore struct {
	LostMinor                 int64 `json:"lost_minor,string"`
	RecoverableMinor          int64 `json:"recoverable_minor,string"`
	RecoveredMinor            int64 `json:"recovered_minor,string"`
	SimulatedLostMinor        int64 `json:"simulated_lost_minor,string"`
	SimulatedRecoverableMinor int64 `json:"simulated_recoverable_minor,string"`
	SimulatedRecoveredMinor   int64 `json:"simulated_recovered_minor,string"`
}

func loadLeakageScore(ctx context.Context, targetDB *sql.DB) (leakageScore, error) {
	var score leakageScore
	if targetDB == nil {
		return score, errors.New("database unavailable")
	}
	err := targetDB.QueryRowContext(ctx, leakageScoreQuery).Scan(&score.LostMinor, &score.RecoverableMinor, &score.RecoveredMinor,
		&score.SimulatedLostMinor, &score.SimulatedRecoverableMinor, &score.SimulatedRecoveredMinor)
	return score, err
}
