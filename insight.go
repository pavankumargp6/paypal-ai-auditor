package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/lib/pq"
)

const maxInsightEvidenceCaptures = 50

var errInvalidInsightOutput = fmt.Errorf("invalid structured insight output")

type InsightCapture struct {
	OrderID     string `json:"order_id"`
	CaptureID   string `json:"capture_id"`
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
	Date        string `json:"date"`
}

type InsightEvidence struct {
	CapturedTotalMinor int64            `json:"captured_total_minor"`
	CaptureCount       int              `json:"capture_count"`
	Captures           []InsightCapture `json:"captures"`
}

type InsightCitation struct {
	OrderID   string `json:"order_id"`
	CaptureID string `json:"capture_id"`
}

type InsightResult struct {
	Insight   string            `json:"insight"`
	Citations []InsightCitation `json:"citations"`
}

func getInsightEvidence(ctx context.Context, records []PaymentRecord) (InsightEvidence, error) {
	eligible := make([]PaymentRecord, 0, len(records))
	for _, record := range records {
		if record.IsLive && strings.EqualFold(record.Status, "COMPLETED") {
			eligible = append(eligible, record)
			if len(eligible) == maxInsightEvidenceCaptures {
				break
			}
		}
	}
	if len(eligible) == 0 {
		return InsightEvidence{}, nil
	}
	if db == nil {
		return InsightEvidence{}, fmt.Errorf("database unavailable")
	}
	captureIDs := make([]string, 0, len(eligible))
	for _, record := range eligible {
		captureIDs = append(captureIDs, record.ID)
	}
	rows, err := db.QueryContext(ctx, `SELECT capture_id, paypal_order_id
		FROM attempts
		WHERE capture_id = ANY($1)
		  AND capture_id IS NOT NULL
		  AND paypal_order_id IS NOT NULL
		  AND paypal_order_id <> ''
		  AND is_synthetic = FALSE
		  AND stage IN ('capture', 'reconcile')
		  AND outcome IN ('succeeded', 'reconciled')
		ORDER BY created_at DESC`, pq.Array(captureIDs))
	if err != nil {
		return InsightEvidence{}, err
	}
	defer rows.Close()
	orderByCapture := make(map[string]string, len(captureIDs))
	for rows.Next() {
		var captureID, orderID string
		if err := rows.Scan(&captureID, &orderID); err != nil {
			return InsightEvidence{}, err
		}
		if _, exists := orderByCapture[captureID]; !exists {
			orderByCapture[captureID] = orderID
		}
	}
	if err := rows.Err(); err != nil {
		return InsightEvidence{}, err
	}
	return buildInsightEvidence(eligible, orderByCapture), nil
}

func buildInsightEvidence(records []PaymentRecord, orderByCapture map[string]string) InsightEvidence {
	evidence := InsightEvidence{Captures: make([]InsightCapture, 0)}
	seen := make(map[string]struct{})
	for _, record := range records {
		if !record.IsLive || !strings.EqualFold(record.Status, "COMPLETED") || strings.TrimSpace(record.ID) == "" {
			continue
		}
		orderID := strings.TrimSpace(orderByCapture[record.ID])
		if orderID == "" {
			continue
		}
		if _, exists := seen[record.ID]; exists {
			continue
		}
		seen[record.ID] = struct{}{}
		evidence.Captures = append(evidence.Captures, InsightCapture{
			OrderID: orderID, CaptureID: record.ID, AmountMinor: record.AmountMinor,
			Currency: record.Currency, Date: record.Date,
		})
		evidence.CapturedTotalMinor += record.AmountMinor
		if len(evidence.Captures) == maxInsightEvidenceCaptures {
			break
		}
	}
	// Keep evidence order stable so equal evidence hashes to the same cache key.
	sort.Slice(evidence.Captures, func(i, j int) bool {
		if evidence.Captures[i].Date == evidence.Captures[j].Date {
			return evidence.Captures[i].CaptureID < evidence.Captures[j].CaptureID
		}
		return evidence.Captures[i].Date < evidence.Captures[j].Date
	})
	evidence.CaptureCount = len(evidence.Captures)
	return evidence
}

func emptyInsight() InsightResult {
	return InsightResult{Insight: "Not enough data yet", Citations: []InsightCitation{}}
}

func unavailableInsight() InsightResult {
	return InsightResult{Insight: "AI insight unavailable.", Citations: []InsightCitation{}}
}

func cachedAIInsight(ctx context.Context, apiKey string, evidence InsightEvidence) InsightResult {
	if len(evidence.Captures) == 0 {
		return emptyInsight()
	}
	key, err := insightEvidenceHash(evidence)
	if err != nil {
		return unavailableInsight()
	}
	for {
		insightCache.Lock()
		now := time.Now()
		entry, exists := insightCache.entries[key]
		if exists && entry.ready != nil {
			ready := entry.ready
			insightCache.Unlock()
			select {
			case <-ready:
				continue
			case <-ctx.Done():
				return unavailableInsight()
			}
		}
		if exists && now.Before(entry.expiresAt) {
			result := cloneInsight(entry.result)
			insightCache.Unlock()
			return result
		}
		entry = &insightCacheEntry{ready: make(chan struct{})}
		insightCache.entries[key] = entry
		insightCache.Unlock()

		result := generateAIInsight(ctx, apiKey, evidence)
		insightCache.Lock()
		entry.result = cloneInsight(result)
		entry.expiresAt = time.Now().Add(60 * time.Second)
		close(entry.ready)
		entry.ready = nil
		pruneInsightCache(time.Now(), key)
		insightCache.Unlock()
		return result
	}
}

func cloneInsight(result InsightResult) InsightResult {
	result.Citations = append([]InsightCitation(nil), result.Citations...)
	return result
}

func insightEvidenceHash(evidence InsightEvidence) ([32]byte, error) {
	canonical := evidence
	canonical.Captures = append([]InsightCapture(nil), evidence.Captures...)
	sort.Slice(canonical.Captures, func(i, j int) bool {
		if canonical.Captures[i].CaptureID == canonical.Captures[j].CaptureID {
			return canonical.Captures[i].OrderID < canonical.Captures[j].OrderID
		}
		return canonical.Captures[i].CaptureID < canonical.Captures[j].CaptureID
	})
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(encoded), nil
}

func pruneInsightCache(now time.Time, keepKey [32]byte) {
	for key, entry := range insightCache.entries {
		if entry.ready == nil && now.After(entry.expiresAt) {
			delete(insightCache.entries, key)
		}
	}
	if len(insightCache.entries) <= 128 {
		return
	}
	var oldestKey [32]byte
	var oldest time.Time
	for key, entry := range insightCache.entries {
		if entry.ready == nil && key != keepKey && (oldest.IsZero() || entry.expiresAt.Before(oldest)) {
			oldestKey, oldest = key, entry.expiresAt
		}
	}
	if !oldest.IsZero() {
		delete(insightCache.entries, oldestKey)
	}
}

func insightJSONSchema() map[string]any {
	citationSchema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"order_id":   map[string]any{"type": "string"},
			"capture_id": map[string]any{"type": "string"},
		},
		"required":             []string{"order_id", "capture_id"},
		"additionalProperties": false,
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"insight":   map[string]any{"type": "string"},
			"citations": map[string]any{"type": "array", "items": citationSchema},
		},
		"required":             []string{"insight", "citations"},
		"additionalProperties": false,
	}
}

func generateAIInsight(ctx context.Context, apiKey string, evidence InsightEvidence) InsightResult {
	if len(evidence.Captures) == 0 {
		return emptyInsight()
	}
	evidenceJSON, err := json.Marshal(evidence)
	if err != nil {
		return unavailableInsight()
	}
	systemMessage := "Use only the supplied real completed PayPal capture evidence. Do not mention synthetic/demo rows, leads, or unsupported causal claims. Return one concise insight and cite every factual claim using exact order_id/capture_id pairs from the evidence. Never invent or alter IDs."
	userMessage := "Analyze this evidence and return a concise grounded insight: " + string(evidenceJSON)
	payload := map[string]any{
		"model":       "openai/gpt-oss-20b",
		"messages":    []map[string]string{{"role": "system", "content": systemMessage}, {"role": "user", "content": userMessage}},
		"temperature": 0,
		"response_format": map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name":   "revenue_insight",
				"strict": true,
				"schema": insightJSONSchema(),
			},
		},
	}
	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		return unavailableInsight()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.groq.com/openai/v1/chat/completions", bytes.NewReader(jsonPayload))
	if err != nil {
		return unavailableInsight()
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+apiKey)
	response, err := httpClient.Do(request)
	if err != nil {
		log.Printf("Groq insight request failed http_status=unavailable error_code=NETWORK_ERROR")
		return unavailableInsight()
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		log.Printf("Groq insight request failed http_status=%d error_code=BODY_READ_ERROR", response.StatusCode)
		return unavailableInsight()
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		log.Printf("Groq insight request failed http_status=%d error_code=%s", response.StatusCode, groqErrorCode(body))
		return unavailableInsight()
	}
	var responseBody struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &responseBody); err != nil || len(responseBody.Choices) == 0 {
		log.Printf("Groq insight request failed http_status=%d error_code=INVALID_RESPONSE", response.StatusCode)
		return unavailableInsight()
	}
	result, err := validateInsightOutput(responseBody.Choices[0].Message.Content, evidence)
	if err != nil {
		log.Printf("Groq insight request failed http_status=%d error_code=INVALID_CITATIONS_OR_SCHEMA", response.StatusCode)
		return unavailableInsight()
	}
	return result
}

func validateInsightOutput(content string, evidence InsightEvidence) (InsightResult, error) {
	decoder := json.NewDecoder(strings.NewReader(content))
	decoder.DisallowUnknownFields()
	var result InsightResult
	if err := decoder.Decode(&result); err != nil {
		return InsightResult{}, errInvalidInsightOutput
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return InsightResult{}, errInvalidInsightOutput
	}
	result.Insight = strings.TrimSpace(result.Insight)
	if result.Insight == "" || len(result.Citations) == 0 {
		return InsightResult{}, errInvalidInsightOutput
	}
	valid := make(map[string]struct{}, len(evidence.Captures))
	for _, capture := range evidence.Captures {
		valid[capture.OrderID+"\x00"+capture.CaptureID] = struct{}{}
	}
	seen := make(map[string]struct{}, len(result.Citations))
	for _, citation := range result.Citations {
		key := strings.TrimSpace(citation.OrderID) + "\x00" + strings.TrimSpace(citation.CaptureID)
		if _, ok := valid[key]; !ok {
			return InsightResult{}, errInvalidInsightOutput
		}
		if _, duplicate := seen[key]; duplicate {
			return InsightResult{}, errInvalidInsightOutput
		}
		seen[key] = struct{}{}
	}
	return result, nil
}
