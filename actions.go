package main

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const maxActionRequestBytes = 16 << 10

type actionProposalRequest struct {
	ActionType  string `json:"action_type"`
	Description string `json:"description"`
}

type actionResponse struct {
	ID         int64           `json:"id"`
	FindingID  int64           `json:"finding_id"`
	ActionType string          `json:"action_type"`
	Status     string          `json:"status"`
	Result     json.RawMessage `json:"result"`
	MoneyMoved bool            `json:"money_moved"`
}

type findingResponse struct {
	ID          int64           `json:"id"`
	Detector    string          `json:"detector"`
	Status      string          `json:"status"`
	AmountMinor int64           `json:"amount_minor,string"`
	Currency    string          `json:"currency"`
	Evidence    json.RawMessage `json:"evidence"`
	Explanation string          `json:"explanation,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
}

func registerFindingRoutes(mux *http.ServeMux, adminToken string) {
	mux.HandleFunc("/api/findings", makeFindingsHandler())
	mux.HandleFunc("/api/findings/detect", makeDetectFindingsHandler(adminToken))
	mux.HandleFunc("/api/findings/leakage-score", makeLeakageScoreHandler())
	mux.HandleFunc("/api/findings/seed-demo", makeSeedDemoFindingHandler())
	mux.HandleFunc("/api/findings/demo-approve", makeDemoApproveFindingHandler())
	mux.HandleFunc("/api/findings/", makeFindingActionHandler(adminToken))
	mux.HandleFunc("/api/leakage-score", makeLeakageScoreHandler())
}

func makeDetectFindingsHandler(adminToken string) http.HandlerFunc {
	return requireAdminToken(adminToken, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			sendJSONError(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		drafts, err := runDeterministicDetectors(ctx, db)
		if err != nil {
			sendJSONError(w, "Finding detection failed", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"detector": "deterministic", "findings_detected": len(drafts)})
	})
}

func requireAdminToken(adminToken string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.TrimSpace(adminToken) == "" {
			sendJSONError(w, "Admin actions are not configured", http.StatusServiceUnavailable)
			return
		}
		providedToken := r.Header.Get("X-Admin-Token")
		if subtle.ConstantTimeCompare([]byte(providedToken), []byte(adminToken)) != 1 {
			sendJSONError(w, "Admin token is missing or invalid", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func makeFindingsHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			sendJSONError(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		rows, err := db.QueryContext(ctx, `SELECT id, detector, status, amount_minor, currency, evidence, COALESCE(explanation, ''), created_at
			FROM findings ORDER BY created_at DESC, id DESC LIMIT 500`)
		if err != nil {
			log.Printf("findings query failed: %v", err)
			sendJSONError(w, "Findings query failed", http.StatusInternalServerError)
			return
		}
		defer rows.Close()
		findings := make([]findingResponse, 0)
		for rows.Next() {
			var finding findingResponse
			if err := rows.Scan(&finding.ID, &finding.Detector, &finding.Status, &finding.AmountMinor,
				&finding.Currency, &finding.Evidence, &finding.Explanation, &finding.CreatedAt); err != nil {
				log.Printf("findings row scan failed: %v", err)
				sendJSONError(w, "Findings query failed", http.StatusInternalServerError)
				return
			}
			findings = append(findings, finding)
		}
		if err := rows.Err(); err != nil {
			log.Printf("findings row iteration failed: %v", err)
			sendJSONError(w, "Findings query failed", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, findings)
	}
}

func makeLeakageScoreHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodPost {
			w.Header().Set("Allow", "GET, POST")
			sendJSONError(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		score, err := loadLeakageScore(ctx, db)
		if err != nil {
			sendJSONError(w, "Leakage score query failed", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, score)
	}
}

type seedDemoResponse struct {
	Created bool            `json:"created"`
	Finding findingResponse `json:"finding"`
}

func makeSeedDemoFindingHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			sendJSONError(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()

		const fingerprint = "synthetic-demo:duplicate_capture:v1"
		const evidence = `{"is_synthetic":true,"label":"Synthetic Demo","demo_key":"duplicate_capture_v1","reason":"multiple_successful_captures_for_order","paypal_order_id":"DEMO-ORDER-DUPLICATE","capture_ids":["DEMO-CAPTURE-1","DEMO-CAPTURE-2"],"captures":[{"capture_id":"DEMO-CAPTURE-1","amount_minor":2500,"currency":"USD"},{"capture_id":"DEMO-CAPTURE-2","amount_minor":2500,"currency":"USD"}],"monetary_excess_known":true}`
		const explanation = "Simulated duplicate-capture finding for the demo. The capture IDs and order are synthetic; no PayPal transaction was made."

		var response seedDemoResponse
		err := db.QueryRowContext(ctx, `INSERT INTO findings (fingerprint, detector, status, amount_minor, currency, evidence, explanation)
			VALUES ($1, 'duplicate_capture', 'open', 2500, 'USD', $2::jsonb, $3)
			ON CONFLICT (fingerprint) DO UPDATE SET
				status = 'open',
				created_at = NOW(),
				updated_at = NOW()
			RETURNING id, detector, status, amount_minor, currency, evidence, explanation, created_at, (xmax = 0)`,
			fingerprint, evidence, explanation).Scan(&response.Finding.ID, &response.Finding.Detector, &response.Finding.Status,
			&response.Finding.AmountMinor, &response.Finding.Currency, &response.Finding.Evidence,
			&response.Finding.Explanation, &response.Finding.CreatedAt, &response.Created)
		if err != nil {
			sendJSONError(w, "Demo finding could not be seeded", http.StatusInternalServerError)
			return
		}
		status := http.StatusOK
		if response.Created {
			status = http.StatusCreated
		}
		writeJSON(w, status, response)
	}
}

func makeDemoApproveFindingHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			sendJSONError(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			sendJSONError(w, "Demo action could not be recorded", http.StatusInternalServerError)
			return
		}
		defer tx.Rollback()

		var findingID int64
		var status string
		err = tx.QueryRowContext(ctx, `SELECT id, status FROM findings
			WHERE fingerprint = 'synthetic-demo:duplicate_capture:v1'
			  AND detector = 'duplicate_capture'
			  AND evidence->>'is_synthetic' = 'true'
			FOR UPDATE`).Scan(&findingID, &status)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				sendJSONError(w, "Demo finding not found", http.StatusNotFound)
			} else {
				sendJSONError(w, "Demo action could not be recorded", http.StatusInternalServerError)
			}
			return
		}
		if status != "open" && status != "acknowledged" {
			sendJSONError(w, "Demo finding is not actionable", http.StatusConflict)
			return
		}

		var response actionResponse
		err = tx.QueryRowContext(ctx, `INSERT INTO actions (finding_id, action_type, status, result)
			VALUES ($1, 'demo_record_only', 'executed',
				jsonb_build_object('events', jsonb_build_array(
					jsonb_build_object('status', 'proposed', 'at', NOW(), 'money_moved', FALSE),
					jsonb_build_object('status', 'approved', 'at', NOW(), 'money_moved', FALSE),
					jsonb_build_object('status', 'executed', 'at', NOW(), 'money_moved', FALSE)),
					'money_moved', FALSE, 'execution_mode', 'record_only'))
			RETURNING id, finding_id, action_type, status, result`, findingID).Scan(
			&response.ID, &response.FindingID, &response.ActionType, &response.Status, &response.Result)
		if err != nil {
			sendJSONError(w, "Demo action could not be recorded", http.StatusInternalServerError)
			return
		}
		result, err := tx.ExecContext(ctx, `UPDATE findings SET status = 'actioned', updated_at = NOW()
			WHERE id = $1 AND status IN ('open', 'acknowledged')`, findingID)
		if err != nil {
			sendJSONError(w, "Demo finding could not be actioned", http.StatusInternalServerError)
			return
		}
		updated, err := result.RowsAffected()
		if err != nil || updated != 1 {
			sendJSONError(w, "Demo finding is not actionable", http.StatusConflict)
			return
		}
		if err := tx.Commit(); err != nil {
			sendJSONError(w, "Demo action could not be recorded", http.StatusInternalServerError)
			return
		}
		response.MoneyMoved = false
		writeJSON(w, http.StatusOK, response)
	}
}

func makeFindingActionHandler(adminToken string) http.HandlerFunc {
	return requireAdminToken(adminToken, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			sendJSONError(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		findingID, ok := routeID(r.URL.Path, "/api/findings/", "/action")
		if !ok {
			sendJSONError(w, "Invalid finding ID", http.StatusBadRequest)
			return
		}
		var proposal actionProposalRequest
		if err := decodeActionRequest(w, r, &proposal); err != nil {
			sendJSONError(w, "Invalid action proposal", http.StatusBadRequest)
			return
		}
		proposal.ActionType = strings.TrimSpace(proposal.ActionType)
		proposal.Description = strings.TrimSpace(proposal.Description)
		if proposal.ActionType == "" || len(proposal.ActionType) > 100 || len(proposal.Description) > 2000 {
			sendJSONError(w, "Invalid action proposal", http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			sendJSONError(w, "Action could not be created", http.StatusInternalServerError)
			return
		}
		defer tx.Rollback()
		var status string
		if err := tx.QueryRowContext(ctx, `SELECT status FROM findings WHERE id = $1 FOR UPDATE`, findingID).Scan(&status); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				sendJSONError(w, "Finding not found", http.StatusNotFound)
			} else {
				sendJSONError(w, "Action could not be created", http.StatusInternalServerError)
			}
			return
		}
		if status != "open" && status != "acknowledged" {
			sendJSONError(w, "Finding is not actionable", http.StatusConflict)
			return
		}
		var response actionResponse
		err = tx.QueryRowContext(ctx, `INSERT INTO actions (finding_id, action_type, status, result)
			VALUES ($1, $2, 'proposed', jsonb_build_object(
				'events', jsonb_build_array(jsonb_build_object('status', 'proposed', 'at', NOW(), 'money_moved', FALSE,
				'action_type', $2::text, 'description', $3::text)),
				'money_moved', FALSE, 'execution_mode', 'record_only'))
			RETURNING id, finding_id, action_type, status, result`, findingID, proposal.ActionType, proposal.Description).
			Scan(&response.ID, &response.FindingID, &response.ActionType, &response.Status, &response.Result)
		if err != nil {
			sendJSONError(w, "Action could not be created", http.StatusInternalServerError)
			return
		}
		if err := tx.Commit(); err != nil {
			sendJSONError(w, "Action could not be created", http.StatusInternalServerError)
			return
		}
		response.MoneyMoved = false
		writeJSON(w, http.StatusCreated, response)
	})
}

func makeApproveActionHandler(adminToken string) http.HandlerFunc {
	return requireAdminToken(adminToken, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			sendJSONError(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		actionID, ok := routeID(r.URL.Path, "/api/actions/", "/approve")
		if !ok {
			sendJSONError(w, "Invalid action ID", http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			sendJSONError(w, "Action could not be approved", http.StatusInternalServerError)
			return
		}
		defer tx.Rollback()
		var status, actionType string
		var storedResult []byte
		err = tx.QueryRowContext(ctx, `SELECT status, action_type, result
			FROM actions WHERE id = $1 FOR UPDATE`, actionID).
			Scan(&status, &actionType, &storedResult)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				sendJSONError(w, "Action not found", http.StatusNotFound)
			} else {
				sendJSONError(w, "Action could not be approved", http.StatusInternalServerError)
			}
			return
		}
		if status != "proposed" {
			sendJSONError(w, "Action is not proposed", http.StatusConflict)
			return
		}
		moneyMoved, err := auditReportsMoneyMoved(storedResult)
		if err != nil {
			sendJSONError(w, "Action audit could not be verified", http.StatusInternalServerError)
			return
		}
		if moneyMoved {
			sendJSONError(w, "Action audit reports money moved; approval refused", http.StatusConflict)
			return
		}

		var response actionResponse
		err = tx.QueryRowContext(ctx, `UPDATE actions SET status = 'executed', updated_at = NOW(),
			result = jsonb_set(COALESCE(result, '{"events":[]}'::jsonb), '{events}',
				COALESCE(result->'events', '[]'::jsonb) || jsonb_build_array(
					jsonb_build_object('status', 'approved', 'at', NOW(), 'money_moved', FALSE),
					jsonb_build_object('status', 'executed', 'at', NOW(), 'money_moved', FALSE)), TRUE)
			WHERE id = $1 AND status = 'proposed'
			RETURNING id, finding_id, status, result`, actionID).
			Scan(&response.ID, &response.FindingID, &response.Status, &response.Result)
		if err != nil {
			sendJSONError(w, "Action could not be approved", http.StatusInternalServerError)
			return
		}
		findingResult, err := tx.ExecContext(ctx, `UPDATE findings SET status = 'actioned', updated_at = NOW()
			WHERE id = $1 AND status IN ('open', 'acknowledged')`, response.FindingID)
		if err != nil {
			sendJSONError(w, "Finding could not be actioned", http.StatusInternalServerError)
			return
		}
		updatedFindings, err := findingResult.RowsAffected()
		if err != nil {
			sendJSONError(w, "Finding could not be actioned", http.StatusInternalServerError)
			return
		}
		if updatedFindings != 1 {
			sendJSONError(w, "Finding is not actionable", http.StatusConflict)
			return
		}
		if err := tx.Commit(); err != nil {
			sendJSONError(w, "Action could not be approved", http.StatusInternalServerError)
			return
		}
		response.ActionType = actionType
		response.MoneyMoved = false
		writeJSON(w, http.StatusOK, response)
	})
}

func auditReportsMoneyMoved(result []byte) (bool, error) {
	var value any
	if err := json.Unmarshal(result, &value); err != nil {
		return false, err
	}
	return auditValueReportsMoneyMoved(value), nil
}

func auditValueReportsMoneyMoved(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if key == "money_moved" {
				if moved, ok := child.(bool); ok && moved {
					return true
				}
			}
			if auditValueReportsMoneyMoved(child) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if auditValueReportsMoneyMoved(child) {
				return true
			}
		}
	}
	return false
}

func routeID(path, prefix, suffix string) (int64, bool) {
	if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) {
		return 0, false
	}
	value := strings.TrimSuffix(strings.TrimPrefix(path, prefix), suffix)
	id, err := strconv.ParseInt(value, 10, 64)
	return id, err == nil && id > 0 && !strings.Contains(value, "/")
}

func decodeActionRequest(w http.ResponseWriter, r *http.Request, dest any) error {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxActionRequestBytes))
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dest); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("action request must contain a single JSON value")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
