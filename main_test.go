package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type mockRoundTripper func(*http.Request) (*http.Response, error)

func (m mockRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) { return m(req) }

// readErrorBody simulates a response body whose Read always fails.
type readErrorBody struct{}

func (readErrorBody) Read(p []byte) (int, error) { return 0, errors.New("simulated read failure") }
func (readErrorBody) Close() error               { return nil }

func resp(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

// saveTransport registers a cleanup that restores the original transport.
func saveTransport(t *testing.T) {
	t.Helper()
	orig := httpClient.Transport
	t.Cleanup(func() { httpClient.Transport = orig })
}

// mockResponse installs a fixed-response transport and restores it after the test.
func mockResponse(t *testing.T, status int, body string) {
	t.Helper()
	saveTransport(t)
	httpClient.Transport = mockRoundTripper(func(req *http.Request) (*http.Response, error) {
		return resp(status, body), nil
	})
}

// mockError installs a transport that always returns a network error.
func mockError(t *testing.T, err error) {
	t.Helper()
	saveTransport(t)
	httpClient.Transport = mockRoundTripper(func(req *http.Request) (*http.Response, error) {
		return nil, err
	})
}

// mockReadError installs a transport whose body errors on Read.
func mockReadError(t *testing.T) {
	t.Helper()
	saveTransport(t)
	httpClient.Transport = mockRoundTripper(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: readErrorBody{}, Header: make(http.Header)}, nil
	})
}

// mockEndpoints sets up an endpoint-aware mock and restores the transport after the test.
func mockEndpoints(t *testing.T, verifyBody, captureBody, reconcileBody string) {
	t.Helper()
	saveTransport(t)
	setOrderLock(t, func(ctx context.Context, orderID string, fn func(context.Context) error) error { return fn(ctx) })
	getCount := 0
	httpClient.Transport = mockRoundTripper(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodPost && req.URL.Path == "/v1/oauth2/token" {
			return resp(http.StatusOK, `{"access_token":"fake-token","token_type":"Bearer"}`), nil
		}
		if req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/capture") {
			return resp(http.StatusOK, captureBody), nil
		}
		if req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/v2/checkout/orders/") {
			getCount++
			if getCount == 1 || reconcileBody == "" {
				return resp(http.StatusOK, verifyBody), nil
			}
			return resp(http.StatusOK, reconcileBody), nil
		}
		return resp(http.StatusNotFound, `{"error":"unexpected mock request"}`), nil
	})
}

// --- Validation ---

func TestValidatePayment(t *testing.T) {
	if err := validatePayment(PaymentRecord{AmountMinor: 2500, Currency: "USD"}); err != nil {
		t.Error("Valid failed")
	}
	if err := validatePayment(PaymentRecord{AmountMinor: 2400, Currency: "USD"}); err == nil {
		t.Error("Wrong amount passed")
	}
	if err := validatePayment(PaymentRecord{AmountMinor: 2500, Currency: "EUR"}); err == nil {
		t.Error("Wrong currency passed")
	}
}

func TestParseMinorUnits(t *testing.T) {
	tests := []struct {
		name     string
		value    string
		currency string
		want     int64
	}{
		{"one cent", "0.01", "USD", 1},
		{"whole amount", "25", "USD", 2500},
		{"grouping-sized amount", "1234.50", "USD", 123450},
		{"zero decimal JPY", "25", "JPY", 25},
		{"zero decimal HUF", "25", "HUF", 25},
		{"zero decimal TWD", "25", "TWD", 25},
		{"zero value", "0", "USD", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseMinorUnits(tt.value, tt.currency)
			if err != nil {
				t.Fatalf("parseMinorUnits(%q, %q): %v", tt.value, tt.currency, err)
			}
			if got != tt.want {
				t.Fatalf("got %d, want %d", got, tt.want)
			}
		})
	}
}

func TestParseMinorUnitsRejectsMalformedAmounts(t *testing.T) {
	tests := []struct {
		value    string
		currency string
	}{
		{"", "USD"},
		{".01", "USD"},
		{"25.", "USD"},
		{"25.001", "USD"},
		{"1e2", "USD"},
		{"-1", "USD"},
		{"+1", "USD"},
		{"1,000", "USD"},
		{"1.0", "JPY"},
		{"92233720368547759.99", "USD"},
	}
	for _, tt := range tests {
		if _, err := parseMinorUnits(tt.value, tt.currency); err == nil {
			t.Errorf("parseMinorUnits(%q, %q) unexpectedly succeeded", tt.value, tt.currency)
		}
	}
}

func TestFormatMinorUnits(t *testing.T) {
	tests := []struct {
		minor    int64
		currency string
		want     string
	}{
		{1, "USD", "0.01"},
		{123450, "USD", "1234.50"},
		{25, "JPY", "25"},
	}
	for _, tt := range tests {
		if got := formatMinorUnits(tt.minor, tt.currency); got != tt.want {
			t.Errorf("formatMinorUnits(%d, %q) = %q, want %q", tt.minor, tt.currency, got, tt.want)
		}
	}
}

func TestMoneyFieldsUseIntegerMinorUnits(t *testing.T) {
	types := []reflect.Type{reflect.TypeOf(PaymentRecord{}), reflect.TypeOf(SpendSummary{})}
	for _, typ := range types {
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			name := strings.ToLower(field.Name)
			if (strings.Contains(name, "amount") || strings.Contains(name, "totalcaptured")) && field.Type.Kind() == reflect.Float64 {
				t.Errorf("money field %s.%s uses float64", typ.Name(), field.Name)
			}
		}
	}
}

func TestDashboardUsesMinorUnitAPIFields(t *testing.T) {
	contents, err := os.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(contents)
	for _, fragment := range []string{
		`field: "amount_minor"`,
		`formatMinorCurrency(p.value, p.data.currency)`,
		`formatMinorCurrency(intel.total_captured_minor, 'USD')`,
		`BigInt(minor ?? "0")`,
		`style: "currency", currency`,
		`cellDataType: "text"`,
		`valueGetter: p => p.data.is_live === true ? "PayPal Sandbox (Live)" : "Synthetic Demo"`,
		`id="decline-rate"`,
		`intel.decline_rate_percent`,
		`intel.ai_insight_citations`,
		`"No data yet"`,
	} {
		if !strings.Contains(html, fragment) {
			t.Errorf("dashboard is missing minor-unit contract %q", fragment)
		}
	}
	if got := formatMinorUnits(17500, "USD"); got != "175.00" {
		t.Fatalf("dashboard revenue amount digits for 17500 minor units = %q, want 175.00 (Intl.NumberFormat supplies the USD symbol)", got)
	}
	if display := "$" + formatMinorUnits(17500, "USD"); display != "$175.00" {
		t.Fatalf("dashboard revenue display = %q, want $175.00", display)
	}
}

func TestValidateApprovalURL(t *testing.T) {
	cases := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{"valid sandbox www", "https://www.sandbox.paypal.com/checkoutnow?token=X", false},
		{"valid sandbox apex", "https://sandbox.paypal.com/checkoutnow?token=X", false},
		{"valid paypal www", "https://www.paypal.com/checkoutnow?token=X", false},
		{"valid paypal apex", "https://paypal.com/checkoutnow?token=X", false},
		{"empty", "", true},
		{"http not https", "http://www.sandbox.paypal.com/checkout", true},
		{"wrong host", "https://evil.example.com/checkout", true},
		{"lookalike host", "https://paypal.com.evil.example/checkout", true},
		{"malformed", "://not-a-url", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateApprovalURL(tc.url)
			if (err != nil) != tc.wantErr {
				t.Errorf("validateApprovalURL(%q) err=%v, wantErr=%v", tc.url, err, tc.wantErr)
			}
		})
	}
}

func TestPayPalReturnCancelURLs(t *testing.T) {
	returnURL, cancelURL, err := payPalReturnCancelURLs("")
	if err != nil {
		t.Fatal(err)
	}
	if returnURL != "http://localhost:8080/?success=true" || cancelURL != "http://localhost:8080/?cancel=true" {
		t.Fatalf("default callback URLs = %q / %q", returnURL, cancelURL)
	}
	returnURL, cancelURL, err = payPalReturnCancelURLs("https://merchant.example/app/")
	if err != nil {
		t.Fatal(err)
	}
	if returnURL != "https://merchant.example/app/?success=true" || cancelURL != "https://merchant.example/app/?cancel=true" {
		t.Fatalf("configured callback URLs = %q / %q", returnURL, cancelURL)
	}
}

// --- Helper: token ---

func TestGetPayPalToken_EmptyAccessToken(t *testing.T) {
	mockResponse(t, 200, `{"access_token":""}`)
	_, err := getPayPalToken(context.Background(), "id", "secret")
	if err == nil {
		t.Error("Expected error for empty access token")
	}
}

func TestGetPayPalToken_MalformedJSON(t *testing.T) {
	mockResponse(t, 200, `not-json`)
	_, err := getPayPalToken(context.Background(), "id", "secret")
	if err == nil {
		t.Error("Expected error for malformed JSON")
	}
}

func TestGetPayPalToken_Non2xxStatus(t *testing.T) {
	mockResponse(t, http.StatusUnauthorized, `{"error":"invalid_client"}`)
	_, err := getPayPalToken(context.Background(), "id", "secret")
	if err == nil {
		t.Error("Expected error for non-2xx status")
	}
	var apiErr *PayPalAPIError
	if !errors.As(err, &apiErr) || apiErr.ReasonCode != "invalid_client" {
		t.Fatalf("error = %v, want typed PayPal invalid_client error", err)
	}
}

func TestPayPalAPIErrorPreservesStructuredDetails(t *testing.T) {
	err := newPayPalAPIError(http.StatusUnprocessableEntity, []byte(`{"name":"UNPROCESSABLE_ENTITY","message":"Request failed","debug_id":"DBG123","details":[{"issue":"INSTRUMENT_DECLINED","description":"Try another funding source"}]}`))
	if err.Name != "UNPROCESSABLE_ENTITY" || err.Message != "Request failed" || err.DebugID != "DBG123" {
		t.Fatalf("structured error fields not preserved: %+v", err)
	}
	if err.ReasonCode != "INSTRUMENT_DECLINED" || err.ReasonDetail != "Try another funding source" {
		t.Fatalf("reason fields = %q/%q", err.ReasonCode, err.ReasonDetail)
	}
}

func TestPayPalAPIErrorUnclassifiedFallback(t *testing.T) {
	for _, tc := range []struct {
		name string
		body []byte
	}{
		{name: "empty body"},
		{name: "non-json body", body: []byte("upstream unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := newPayPalAPIError(http.StatusForbidden, tc.body)
			if err.HTTPStatus != http.StatusForbidden || err.ReasonCode != "UNCLASSIFIED" {
				t.Fatalf("error = %+v, want HTTP 403 UNCLASSIFIED", err)
			}
			if !strings.Contains(err.Error(), "HTTP 403") {
				t.Errorf("error string %q omits HTTP status", err.Error())
			}
		})
	}
}

func TestGetPayPalToken_NetworkFailure(t *testing.T) {
	mockError(t, errors.New("simulated network failure"))
	_, err := getPayPalToken(context.Background(), "id", "secret")
	if err == nil {
		t.Error("Expected network error")
	}
}

func TestGetPayPalToken_BodyReadFailure(t *testing.T) {
	mockReadError(t)
	_, err := getPayPalToken(context.Background(), "id", "secret")
	if err == nil {
		t.Error("Expected body read error")
	}
}

// --- Helper: create-order ---

func TestCreatePayPalOrder_MissingOrderID(t *testing.T) {
	mockResponse(t, 200, `{"links":[{"rel":"approve","href":"https://www.sandbox.paypal.com/checkoutnow?token=X"}]}`)
	_, _, err := createPayPalOrder(context.Background(), "token", "return")
	if err == nil {
		t.Error("Expected error for missing order ID")
	}
}

func TestCreatePayPalOrder_MissingApprovalURL(t *testing.T) {
	mockResponse(t, 200, `{"id":"ORDER1","links":[]}`)
	_, _, err := createPayPalOrder(context.Background(), "token", "return")
	if err == nil {
		t.Error("Expected error for missing approval URL")
	}
}

func TestCreatePayPalOrder_InvalidApprovalURL(t *testing.T) {
	mockResponse(t, 200, `{"id":"ORDER1","links":[{"rel":"approve","href":"http://evil.example/checkout"}]}`)
	_, _, err := createPayPalOrder(context.Background(), "token", "return")
	if err == nil {
		t.Error("Expected error for invalid approval URL (http + wrong host)")
	}
}

func TestCreatePayPalOrder_ValidApprovalURL(t *testing.T) {
	mockResponse(t, 200, `{"id":"ORDER1","links":[{"rel":"approve","href":"https://www.sandbox.paypal.com/checkoutnow?token=ABC"}]}`)
	id, url, err := createPayPalOrder(context.Background(), "token", "return")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if id != "ORDER1" {
		t.Errorf("Wrong ID: %s", id)
	}
	if !strings.Contains(url, "paypal.com") {
		t.Errorf("Unexpected URL: %s", url)
	}
}

func TestCreatePayPalOrder_NetworkFailure(t *testing.T) {
	mockError(t, errors.New("simulated network failure"))
	_, _, err := createPayPalOrder(context.Background(), "token", "return")
	if err == nil {
		t.Error("Expected network error")
	}
}

func TestCreatePayPalOrder_BodyReadFailure(t *testing.T) {
	mockReadError(t)
	_, _, err := createPayPalOrder(context.Background(), "token", "return")
	if err == nil {
		t.Error("Expected body read error")
	}
}

// --- Helper: verify ---

func TestVerifyOrderStatus_Non2xxStatus(t *testing.T) {
	mockResponse(t, http.StatusNotFound, `not found`)
	_, err := verifyOrderStatus(context.Background(), "token", "ORDER1")
	if err == nil {
		t.Error("Expected error for 404 status")
	}
}

func TestVerifyOrderStatus_MalformedJSON(t *testing.T) {
	mockResponse(t, 200, `not-json`)
	_, err := verifyOrderStatus(context.Background(), "token", "ORDER1")
	if err == nil {
		t.Error("Expected error on malformed JSON")
	}
}

func TestVerifyOrderStatus_EmptyStatus(t *testing.T) {
	mockResponse(t, 200, `{}`)
	status, err := verifyOrderStatus(context.Background(), "token", "ORDER1")
	if err == nil {
		t.Error("Expected error for empty status")
	}
	if status != "" {
		t.Errorf("Expected empty status, got %q", status)
	}
}

func TestVerifyOrderStatus_NetworkFailure(t *testing.T) {
	mockError(t, errors.New("simulated network failure"))
	_, err := verifyOrderStatus(context.Background(), "token", "ORDER1")
	if err == nil {
		t.Error("Expected network error")
	}
}

func TestVerifyOrderStatus_BodyReadFailure(t *testing.T) {
	mockReadError(t)
	_, err := verifyOrderStatus(context.Background(), "token", "ORDER1")
	if err == nil {
		t.Error("Expected body read error")
	}
}

// --- Helper: capture / reconcile ---

func TestCapturePayPalOrder_Non2xxStatus(t *testing.T) {
	mockResponse(t, http.StatusUnprocessableEntity, `{"error":"ORDER_NOT_APPROVED"}`)
	_, err := capturePayPalOrder(context.Background(), "token", "ORDER1")
	if err == nil {
		t.Error("Expected error for 422 status")
	}
}

func TestCapturePayPalOrder_Empty403IsTypedUnclassified(t *testing.T) {
	mockResponse(t, http.StatusForbidden, "")
	_, err := capturePayPalOrder(context.Background(), "token", "ORDER1")
	var apiErr *PayPalAPIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %v, want typed PayPal API error", err)
	}
	if apiErr.HTTPStatus != http.StatusForbidden || apiErr.ReasonCode != "UNCLASSIFIED" {
		t.Fatalf("error = %+v, want HTTP 403 UNCLASSIFIED", apiErr)
	}
}

func TestCapturePayPalOrder_NetworkFailure(t *testing.T) {
	mockError(t, errors.New("simulated network failure"))
	_, err := capturePayPalOrder(context.Background(), "token", "ORDER1")
	if err == nil {
		t.Error("Expected network error")
	}
}

func TestCapturePayPalOrder_BodyReadFailure(t *testing.T) {
	mockReadError(t)
	_, err := capturePayPalOrder(context.Background(), "token", "ORDER1")
	if err == nil {
		t.Error("Expected body read error")
	}
}

func TestReconcileCapture_Non2xxStatus(t *testing.T) {
	mockResponse(t, http.StatusInternalServerError, `error`)
	_, err := reconcileCapture(context.Background(), "token", "ORDER1")
	if err == nil {
		t.Error("Expected error for 500 status")
	}
}

func TestReconcileCapture_NetworkFailure(t *testing.T) {
	mockError(t, errors.New("simulated network failure"))
	_, err := reconcileCapture(context.Background(), "token", "ORDER1")
	if err == nil {
		t.Error("Expected network error")
	}
}

func TestReconcileCapture_BodyReadFailure(t *testing.T) {
	mockReadError(t)
	_, err := reconcileCapture(context.Background(), "token", "ORDER1")
	if err == nil {
		t.Error("Expected body read error")
	}
}

// --- Body parsing ---

func TestParseCaptureFromBody_MalformedJSON(t *testing.T) {
	_, err := parseCaptureFromBody([]byte(`not-json`))
	if err == nil {
		t.Error("Expected error on malformed capture JSON")
	}
}

func TestParseCaptureFromBody_NotCompleted(t *testing.T) {
	body := []byte(`{"status":"PENDING","purchase_units":[{"payments":{"captures":[{"id":"X","status":"PENDING","amount":{"value":"25.00","currency_code":"USD"}}]}}]}`)
	_, err := parseCaptureFromBody(body)
	if err == nil {
		t.Error("Expected error on non-COMPLETED capture")
	}
}

func TestParseCaptureUsesPayPalTimestamps(t *testing.T) {
	for _, tc := range []struct {
		name      string
		timestamp string
		field     string
	}{
		{name: "create_time preferred", timestamp: "2026-11-06T12:00:00Z", field: "create_time"},
		{name: "update_time fallback", timestamp: "2026-11-06T13:00:00+01:00", field: "update_time"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"purchase_units":[{"payments":{"captures":[{"id":"CAPTIME","status":"COMPLETED","%s":%q,"amount":{"value":"25.00","currency_code":"USD"}}]}}]}`, tc.field, tc.timestamp)
			payment, err := parseCaptureFromBody([]byte(body))
			if err != nil {
				t.Fatal(err)
			}
			if payment.Date != "2026-11-06T12:00:00Z" {
				t.Fatalf("capture date = %q, want PayPal timestamp normalized to UTC", payment.Date)
			}
		})
	}
}

func testInsightEvidence() InsightEvidence {
	return InsightEvidence{
		CapturedTotalMinor: 2500,
		CaptureCount:       1,
		Captures: []InsightCapture{{
			OrderID: "ORDER-1", CaptureID: "CAP-1", AmountMinor: 2500, Currency: "USD", Date: "2026-11-06T12:00:00Z",
		}},
	}
}

func TestBuildInsightEvidenceExcludesSyntheticAndUnmatchedRows(t *testing.T) {
	records := []PaymentRecord{
		{ID: "CAP-REAL", AmountMinor: 2500, Status: "COMPLETED", Currency: "USD", IsLive: true},
		{ID: "DEMO-CAP-101", AmountMinor: 99900, Status: "COMPLETED", Currency: "USD", IsLive: false},
		{ID: "CAP-PENDING", AmountMinor: 1000, Status: "PENDING", Currency: "USD", IsLive: true},
		{ID: "CAP-UNMATCHED", AmountMinor: 500, Status: "COMPLETED", Currency: "USD", IsLive: true},
	}
	got := buildInsightEvidence(records, map[string]string{
		"CAP-REAL": "ORDER-REAL", "DEMO-CAP-101": "ORDER-DEMO", "CAP-PENDING": "ORDER-PENDING",
	})
	if got.CaptureCount != 1 || got.CapturedTotalMinor != 2500 || len(got.Captures) != 1 {
		t.Fatalf("evidence = %+v, want one real completed capture", got)
	}
	if got.Captures[0].OrderID != "ORDER-REAL" || got.Captures[0].CaptureID != "CAP-REAL" {
		t.Fatalf("evidence citation = %+v, want real order/capture IDs", got.Captures[0])
	}
}

func TestCachedAIInsightUsesEvidenceHashStrictSchemaAndZeroTemperature(t *testing.T) {
	insightCache.Lock()
	insightCache.entries = make(map[[32]byte]*insightCacheEntry)
	insightCache.Unlock()
	t.Cleanup(func() {
		insightCache.Lock()
		insightCache.entries = make(map[[32]byte]*insightCacheEntry)
		insightCache.Unlock()
	})
	saveTransport(t)
	calls := 0
	httpClient.Transport = mockRoundTripper(func(req *http.Request) (*http.Response, error) {
		calls++
		var payload struct {
			Model       string  `json:"model"`
			Temperature float64 `json:"temperature"`
			Messages    []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
			ResponseFormat json.RawMessage `json:"response_format"`
		}
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			t.Errorf("decode Groq request: %v", err)
		} else {
			if payload.Model != "openai/gpt-oss-20b" || payload.Temperature != 0 {
				t.Errorf("model/temperature = %q/%v, want model and temperature 0", payload.Model, payload.Temperature)
			}
			if len(payload.Messages) != 2 || strings.Contains(payload.Messages[1].Content, "DEMO-CAP") || !strings.Contains(payload.Messages[1].Content, "CAP-1") {
				t.Errorf("Groq user evidence included unexpected data: %+v", payload.Messages)
			}
			var format map[string]any
			if err := json.Unmarshal(payload.ResponseFormat, &format); err != nil {
				t.Errorf("decode response_format: %v", err)
			} else {
				jsonSchema, _ := format["json_schema"].(map[string]any)
				if format["type"] != "json_schema" || jsonSchema["strict"] != true {
					t.Errorf("response format = %s, want strict json_schema", payload.ResponseFormat)
				}
				schema, _ := jsonSchema["schema"].(map[string]any)
				if schema["additionalProperties"] != false {
					t.Errorf("top-level schema must reject extra properties: %s", payload.ResponseFormat)
				}
				properties, _ := schema["properties"].(map[string]any)
				citations, _ := properties["citations"].(map[string]any)
				items, _ := citations["items"].(map[string]any)
				if items["additionalProperties"] != false {
					t.Errorf("citation schema must reject extra properties: %s", payload.ResponseFormat)
				}
			}
		}
		return resp(http.StatusOK, `{"choices":[{"message":{"content":"{\"insight\":\"Recent captures are consistent.\",\"citations\":[{\"order_id\":\"ORDER-1\",\"capture_id\":\"CAP-1\"}]}"}}]}`), nil
	})

	ctx := context.Background()
	evidence := testInsightEvidence()
	first := cachedAIInsight(ctx, "test-key", evidence)
	second := cachedAIInsight(ctx, "test-key", evidence)
	if first.Insight != "Recent captures are consistent." || second.Insight != first.Insight || len(first.Citations) != 1 {
		t.Fatalf("first/second structured insight = %+v / %+v", first, second)
	}
	if calls != 1 {
		t.Fatalf("Groq calls for equal evidence = %d, want 1", calls)
	}
	key, err := insightEvidenceHash(evidence)
	if err != nil {
		t.Fatal(err)
	}
	insightCache.Lock()
	entry := insightCache.entries[key]
	insightCache.Unlock()
	if entry == nil || !entry.expiresAt.After(time.Now()) || entry.expiresAt.After(time.Now().Add(time.Minute)) {
		t.Fatalf("cache entry expiry = %+v, want an expiry within 60 seconds", entry)
	}
	changed := testInsightEvidence()
	changed.Captures[0].AmountMinor++
	changed.CapturedTotalMinor++
	_ = cachedAIInsight(ctx, "test-key", changed)
	if calls != 2 {
		t.Fatalf("Groq calls after evidence changed = %d, want 2", calls)
	}
}

func TestNoRealInsightEvidenceReturnsNotEnoughDataWithoutGroq(t *testing.T) {
	saveTransport(t)
	calls := 0
	httpClient.Transport = mockRoundTripper(func(*http.Request) (*http.Response, error) {
		calls++
		return resp(http.StatusInternalServerError, `{}`), nil
	})
	got := cachedAIInsight(context.Background(), "test-key", InsightEvidence{})
	if got.Insight != "Not enough data yet" || len(got.Citations) != 0 || calls != 0 {
		t.Fatalf("empty evidence result=%+v Groq calls=%d, want no-data result and no request", got, calls)
	}
}

func TestInsightOutputRejectsMalformedAndUnknownCitations(t *testing.T) {
	evidence := testInsightEvidence()
	tests := []struct{ name, content string }{
		{name: "malformed JSON", content: `{not-json`},
		{name: "unknown field", content: `{"insight":"ok","citations":[{"order_id":"ORDER-1","capture_id":"CAP-1"}],"extra":true}`},
		{name: "trailing data", content: `{"insight":"ok","citations":[{"order_id":"ORDER-1","capture_id":"CAP-1"}]} {}`},
		{name: "empty citations", content: `{"insight":"ok","citations":[]}`},
		{name: "unknown IDs", content: `{"insight":"ok","citations":[{"order_id":"ORDER-OTHER","capture_id":"CAP-OTHER"}]}`},
		{name: "mismatched pair", content: `{"insight":"ok","citations":[{"order_id":"ORDER-1","capture_id":"CAP-OTHER"}]}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := validateInsightOutput(tc.content, evidence); err == nil {
				t.Fatal("validateInsightOutput accepted invalid output")
			}
		})
	}
}

func TestGenerateAIInsightRejectsCitationNotInEvidence(t *testing.T) {
	mockResponse(t, http.StatusOK, `{"choices":[{"message":{"content":"{\"insight\":\"A claim\",\"citations\":[{\"order_id\":\"ORDER-UNKNOWN\",\"capture_id\":\"CAP-UNKNOWN\"}]}"}}]}`)
	got := generateAIInsight(context.Background(), "test-key", testInsightEvidence())
	if got.Insight != "AI insight unavailable." || len(got.Citations) != 0 {
		t.Fatalf("unknown-citation result = %+v, want unavailable", got)
	}
}
func TestAttemptOutcomesPreservePayPalDeclineReasons(t *testing.T) {
	tests := []struct {
		issue   string
		outcome string
	}{
		{issue: "INSTRUMENT_DECLINED", outcome: "declined"},
		{issue: "PAYER_CANNOT_PAY", outcome: "declined"},
		{issue: "PAYMENT_DENIED", outcome: "declined"},
		{issue: "DUPLICATE_INVOICE_ID", outcome: "failed"},
		{issue: "ORDER_ALREADY_CAPTURED", outcome: "reconciled"},
		{issue: "UNCLASSIFIED", outcome: "failed"},
	}
	for _, tc := range tests {
		t.Run(tc.issue, func(t *testing.T) {
			err := &PayPalAPIError{HTTPStatus: http.StatusUnprocessableEntity, ReasonCode: tc.issue, ReasonDetail: "sandbox test response"}
			attempt := attemptFromPayPalError("capture", 2500, "USD", err)
			if attempt.Outcome != tc.outcome || attempt.ReasonCode != tc.issue || attempt.HTTPStatus == nil || *attempt.HTTPStatus != http.StatusUnprocessableEntity {
				t.Fatalf("attempt = %+v, want outcome %q with HTTP 422", attempt, tc.outcome)
			}
		})
	}
	emptyBody := attemptFromPayPalError("capture", 2500, "USD", newPayPalAPIError(http.StatusForbidden, nil))
	if emptyBody.Outcome != "failed" || emptyBody.ReasonCode != "UNCLASSIFIED" || emptyBody.HTTPStatus == nil || *emptyBody.HTTPStatus != http.StatusForbidden {
		t.Fatalf("empty 403 attempt = %+v, want failed UNCLASSIFIED HTTP 403", emptyBody)
	}
}

func TestDeclineRateNoDataAndCalculation(t *testing.T) {
	if rate, available := declineRate(0, 0); available || rate != 0 {
		t.Fatalf("empty decline rate = %v/%v, want unavailable", rate, available)
	}
	if rate, available := declineRate(2, 5); !available || rate != 40 {
		t.Fatalf("decline rate = %v/%v, want 40%% available", rate, available)
	}
	if !strings.Contains(declineRateQuery, "stage = 'capture'") || !strings.Contains(declineRateQuery, "is_synthetic = FALSE") || !strings.Contains(declineRateQuery, "INSTRUMENT_DECLINED") || !strings.Contains(declineRateQuery, "PAYER_CANNOT_PAY") || !strings.Contains(declineRateQuery, "PAYMENT_DENIED") || !strings.Contains(declineRateQuery, "ORDER_ALREADY_CAPTURED") || strings.Contains(declineRateQuery, "stage = 'reconcile'") {
		t.Fatalf("decline-rate query must count real capture attempts only: %s", declineRateQuery)
	}
}

func TestDeclineReasonDefinition(t *testing.T) {
	for _, code := range []string{"INSTRUMENT_DECLINED", "PAYER_CANNOT_PAY", "PAYMENT_DENIED"} {
		if !isDeclineReasonCode(code) {
			t.Errorf("%s should be a decline", code)
		}
	}
	for _, code := range []string{"ORDER_ALREADY_CAPTURED", "DUPLICATE_INVOICE_ID", "INSUFFICIENT_FUNDS", "UNCLASSIFIED", ""} {
		if isDeclineReasonCode(code) {
			t.Errorf("%s should not be a decline", code)
		}
	}
}

func TestDuplicateCaptureDetectorUsesCaptureEvidence(t *testing.T) {
	observations := []captureObservation{
		{OrderID: "ORDER-1", CaptureID: "CAP-1", AmountMinor: 2500, Currency: "USD", Outcome: "succeeded", Stage: "capture"},
		{OrderID: "ORDER-1", CaptureID: "CAP-2", AmountMinor: 2500, Currency: "USD", Outcome: "succeeded", Stage: "webhook"},
		{OrderID: "ORDER-2", CaptureID: "CAP-3", AmountMinor: 4000, Currency: "USD", Outcome: "failed", Stage: "capture"},
		{OrderID: "ORDER-2", CaptureID: "CAP-3", AmountMinor: 4000, Currency: "USD", Outcome: "succeeded", Stage: "webhook"},
	}
	findings := detectDuplicateCaptures(observations)
	if len(findings) != 2 {
		t.Fatalf("duplicate findings = %d, want order duplicate and repeated capture ID", len(findings))
	}
	var sawOrder, sawID bool
	for _, finding := range findings {
		if finding.Detector != "duplicate_capture" {
			t.Fatalf("detector = %q, want duplicate_capture", finding.Detector)
		}
		encoded, err := json.Marshal(finding.Evidence)
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case strings.Contains(finding.Fingerprint, "duplicate_capture:order:"):
			sawOrder = true
			if finding.AmountMinor != 2500 || !strings.Contains(string(encoded), `"CAP-1"`) || !strings.Contains(string(encoded), `"CAP-2"`) {
				t.Fatalf("order duplicate amount/evidence = %d/%s", finding.AmountMinor, encoded)
			}
		case strings.Contains(finding.Fingerprint, "duplicate_capture:capture:"):
			sawID = true
			if finding.AmountMinor != 0 || !strings.Contains(string(encoded), `"CAP-3"`) {
				t.Fatalf("repeated capture ID evidence = %d/%s", finding.AmountMinor, encoded)
			}
		}
	}
	if !sawOrder || !sawID {
		t.Fatalf("missing detector evidence: order=%v captureID=%v", sawOrder, sawID)
	}
}

func TestFeeMismatchDetectorUsesMinorUnitRoundingAndEvidence(t *testing.T) {
	expected, delta, mismatch := detectFeeMismatch(2500, 103, "USD")
	if expected != 103 || delta != 0 || mismatch {
		t.Fatalf("$25 expected fee = %d delta=%d mismatch=%v, want 103/0/false", expected, delta, mismatch)
	}
	_, delta, mismatch = detectFeeMismatch(2500, 104, "USD")
	if delta != 1 || mismatch {
		t.Fatalf("one-cent delta = %d mismatch=%v, want 1/false", delta, mismatch)
	}
	_, delta, mismatch = detectFeeMismatch(2500, 105, "USD")
	if delta != 2 || !mismatch {
		t.Fatalf("two-cent delta = %d mismatch=%v, want 2/true", delta, mismatch)
	}
	finding, ok := feeMismatchFinding("ORDER-FEE", "CAP-FEE", "USD", 2500, 105)
	if !ok || finding.Detector != "fee_mismatch" || finding.AmountMinor != 2 {
		t.Fatalf("fee finding = %+v, ok=%v", finding, ok)
	}
	evidence, err := json.Marshal(finding.Evidence)
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{`"capture_id":"CAP-FEE"`, `"expected_fee_minor":103`, `"actual_fee_minor":105`, `"delta_minor":2`} {
		if !strings.Contains(string(evidence), fragment) {
			t.Errorf("fee evidence %s missing %s", evidence, fragment)
		}
	}
}

func TestLeakageScoreQueryUsesRequiredStatuses(t *testing.T) {
	for _, fragment := range []string{
		"status = 'open'",
		"status IN ('open', 'acknowledged')",
		"status = 'actioned'",
		"SUM(amount_minor)",
	} {
		if !strings.Contains(leakageScoreQuery, fragment) {
			t.Errorf("leakage score query missing %q", fragment)
		}
	}
}

func TestFindingsActionsMigrationIsAdditiveAndHasStatusChecks(t *testing.T) {
	contents, err := os.ReadFile("migrations/004_findings_actions.sql")
	if err != nil {
		t.Fatal(err)
	}
	sqlText := strings.ToLower(string(contents))
	for _, fragment := range []string{
		"create table if not exists findings",
		"id bigserial primary key",
		"evidence jsonb not null",
		"findings_status_check",
		"('open', 'acknowledged', 'actioned', 'dismissed')",
		"create table if not exists actions",
		"result jsonb not null",
		"actions_status_check",
		"('proposed', 'approved', 'executed', 'failed', 'rejected')",
	} {
		if !strings.Contains(sqlText, fragment) {
			t.Errorf("migration 004 missing %q", fragment)
		}
	}
	if strings.Contains(sqlText, "drop table") || strings.Contains(sqlText, "alter table") {
		t.Fatal("migration 004 must be additive")
	}
}

func TestCIRunsFindingsActionsMigration(t *testing.T) {
	contents, err := os.ReadFile(".github/workflows/go-ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contents), "go run ./cmd/migrate ./migrations/004_findings_actions.sql") {
		t.Fatal("CI workflow must apply migration 004 before PostgreSQL integration tests")
	}
}

func TestWebhookCaptureEventsBecomeAttempts(t *testing.T) {
	tests := []struct {
		eventType string
		resource  string
		outcome   string
		reason    string
	}{
		{"PAYMENT.CAPTURE.COMPLETED", `{"id":"CAP-1","amount":{"value":"25.00","currency_code":"USD"},"supplementary_data":{"related_ids":{"order_id":"ORDER-1"}}}`, "succeeded", "PAYMENT.CAPTURE.COMPLETED"},
		{"PAYMENT.CAPTURE.DENIED", `{"id":"CAP-2","amount":{"value":"25.00","currency_code":"USD"},"status_details":{"reason":"INSTRUMENT_DECLINED"}}`, "declined", "INSTRUMENT_DECLINED"},
		{"PAYMENT.CAPTURE.DENIED", `{"id":"CAP-3","amount":{"value":"25.00","currency_code":"USD"},"status_details":{"reason":"OTHER_REASON"}}`, "failed", "OTHER_REASON"},
		{"PAYMENT.CAPTURE.REFUNDED", `{"id":"CAP-4","amount":{"value":"25.00","currency_code":"USD"}}`, "succeeded", "PAYMENT.CAPTURE.REFUNDED"},
	}
	for _, tc := range tests {
		t.Run(tc.eventType+"/"+tc.reason, func(t *testing.T) {
			event := paypalWebhookEvent{ID: "EVENT", EventType: tc.eventType, Resource: json.RawMessage(tc.resource)}
			attempt, supported, err := webhookAttempt(event)
			if err != nil || !supported {
				t.Fatalf("webhookAttempt supported=%v err=%v", supported, err)
			}
			if attempt.Stage != "webhook" || attempt.Outcome != tc.outcome || attempt.ReasonCode != tc.reason || attempt.AmountMinor != 2500 || attempt.Currency != "USD" {
				t.Fatalf("attempt = %+v", attempt)
			}
		})
	}
}

func TestAttemptsMigrationIsAdditiveAndHasRequiredColumns(t *testing.T) {
	contents, err := os.ReadFile("migrations/002_attempts.sql")
	if err != nil {
		t.Fatal(err)
	}
	sqlText := strings.ToLower(string(contents))
	for _, column := range []string{"paypal_order_id text", "capture_id text", "debug_id text", "stage text not null", "outcome text not null", "reason_code text,", "reason_detail text,", "http_status integer", "amount_minor bigint not null", "currency text not null", "is_synthetic boolean not null", "created_at timestamptz not null", "attempts_stage_check", "stage in ('create', 'approve', 'capture', 'reconcile', 'webhook')", "attempts_outcome_check", "attempts_http_status_check", "attempts_paypal_order_created_at_idx"} {
		if !strings.Contains(sqlText, column) {
			t.Errorf("migration 002 missing required column declaration %q", column)
		}
	}
	if strings.Contains(sqlText, "drop table") || strings.Contains(sqlText, "alter table") {
		t.Fatal("migration 002 must only add the attempts table and index")
	}
}

func TestWebhookReceiptMigrationIsAdditiveAndDeduplicated(t *testing.T) {
	contents, err := os.ReadFile("migrations/003_webhook_events.sql")
	if err != nil {
		t.Fatal(err)
	}
	sqlText := strings.ToLower(string(contents))
	for _, fragment := range []string{"create table if not exists webhook_events", "event_id text primary key", "event_type text not null", "resource_id text", "payload jsonb not null", "processed_at timestamptz", "process_error text", "webhook_events_received_at_idx", "webhook_events_resource_id_idx"} {
		if !strings.Contains(sqlText, fragment) {
			t.Errorf("webhook receipt migration missing %q", fragment)
		}
	}
	if strings.Contains(sqlText, "drop table") || strings.Contains(sqlText, "alter table") {
		t.Fatal("webhook receipt migration must be additive")
	}
}
func TestGroqErrorCodeIsShortAndRedacted(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "code", body: `{"error":{"code":"invalid_api_key","message":"do not log"}}`, want: "invalid_api_key"},
		{name: "type fallback", body: `{"error":{"type":"authentication_error"}}`, want: "authentication_error"},
		{name: "unclassified", body: `not-json`, want: "UNCLASSIFIED"},
		{name: "unsafe", body: `{"error":{"code":"token=secret"}}`, want: "UNCLASSIFIED"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := groqErrorCode([]byte(tc.body)); got != tc.want {
				t.Fatalf("groqErrorCode() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestGroqFailureLogsOnlyStatusAndErrorCode(t *testing.T) {
	mockResponse(t, http.StatusUnauthorized, `{"error":{"code":"invalid_api_key","message":"Bearer secret-value"}}`)
	previousOutput := log.Writer()
	var output bytes.Buffer
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(previousOutput) })
	if got := generateAIInsight(context.Background(), "never-log-this-key", testInsightEvidence()); got.Insight != "AI insight unavailable." {
		t.Fatalf("generateAIInsight() = %+v", got)
	}
	logged := output.String()
	if !strings.Contains(logged, "http_status=401 error_code=invalid_api_key") {
		t.Fatalf("diagnostic log = %q, want status and short code", logged)
	}
	for _, secret := range []string{"never-log-this-key", "secret-value", "Bearer", "message"} {
		if strings.Contains(logged, secret) {
			t.Fatalf("diagnostic log leaked %q: %q", secret, logged)
		}
	}
}

func TestAttemptLoggingFailureDoesNotBreakCapture(t *testing.T) {
	previousDB := db
	db = &sql.DB{}
	t.Cleanup(func() { db = previousDB })
	previousWriter := writePaymentAttempt
	logged := make(chan PaymentAttempt, 4)
	writePaymentAttempt = func(_ context.Context, _ *sql.DB, attempt PaymentAttempt) error {
		logged <- attempt
		return errors.New("simulated attempt table outage")
	}
	t.Cleanup(func() { writePaymentAttempt = previousWriter })

	completedOrder := `{"status":"COMPLETED","purchase_units":[{"payments":{"captures":[{"id":"CAPLOGGING","status":"COMPLETED","create_time":"2026-11-06T12:00:00Z","amount":{"value":"25.00","currency_code":"USD"}}]}}]}`
	mockEndpoints(t, `{"status":"APPROVED"}`, completedOrder, "")
	previousPersist := persistCapture
	persistCapture = func(context.Context, PaymentRecord) error { return nil }
	t.Cleanup(func() { persistCapture = previousPersist })

	req := httptest.NewRequest(http.MethodPost, "/api/capture-order", strings.NewReader(`{"orderID":"ORDER123"}`))
	w := httptest.NewRecorder()
	makeCaptureOrderHandler("client", "secret").ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("capture response = %d %s, want HTTP 200 despite attempt writer failure", w.Code, w.Body.String())
	}
	for i := 0; i < 2; i++ {
		select {
		case <-logged:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for best-effort attempt writer")
		}
	}
}

func setOrderLock(t *testing.T, lock func(context.Context, string, func(context.Context) error) error) {
	t.Helper()
	original := orderLock
	orderLock = lock
	t.Cleanup(func() { orderLock = original })
}

func TestCaptureOrderHandler_AlreadyCapturedResponseReconciles(t *testing.T) {
	saveTransport(t)
	setOrderLock(t, func(ctx context.Context, orderID string, fn func(context.Context) error) error { return fn(ctx) })
	completedOrder := `{"status":"COMPLETED","purchase_units":[{"payments":{"captures":[{"id":"CAPWINNER","status":"COMPLETED","create_time":"2026-11-06T12:00:00Z","amount":{"value":"25.00","currency_code":"USD"}}]}}]}`
	getCount := 0
	captureCalls := 0
	requestIDs := []string{}
	httpClient.Transport = mockRoundTripper(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodPost && req.URL.Path == "/v1/oauth2/token" {
			return resp(http.StatusOK, `{"access_token":"fake-token"}`), nil
		}
		if req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/v2/checkout/orders/") {
			getCount++
			if getCount == 1 {
				return resp(http.StatusOK, `{"status":"APPROVED"}`), nil
			}
			return resp(http.StatusOK, completedOrder), nil
		}
		if req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/capture") {
			captureCalls++
			requestIDs = append(requestIDs, req.Header.Get("PayPal-Request-Id"))
			return resp(http.StatusUnprocessableEntity, `{"name":"UNPROCESSABLE_ENTITY","details":[{"issue":"ORDER_ALREADY_CAPTURED"}]}`), nil
		}
		return resp(http.StatusNotFound, `{}`), nil
	})

	persistCalls := 0
	originalPersist := persistCapture
	persistCapture = func(ctx context.Context, payment PaymentRecord) error {
		persistCalls++
		return nil
	}
	t.Cleanup(func() { persistCapture = originalPersist })

	handler := makeCaptureOrderHandler("fake", "fake")
	req := httptest.NewRequest(http.MethodPost, "/api/capture-order", strings.NewReader(`{"orderID":"ORDER123"}`))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 after reconciliation, got %d: %s", w.Code, w.Body.String())
	}
	var payment PaymentRecord
	if err := json.Unmarshal(w.Body.Bytes(), &payment); err != nil {
		t.Fatalf("decode payment response: %v", err)
	}
	if payment.ID != "CAPWINNER" {
		t.Fatalf("reconciled capture ID = %q, want CAPWINNER", payment.ID)
	}
	if captureCalls != 1 || persistCalls != 1 {
		t.Fatalf("capture calls=%d, persist calls=%d; want one each", captureCalls, persistCalls)
	}
	if len(requestIDs) != 1 || requestIDs[0] != captureRequestID("ORDER123") {
		t.Fatalf("capture request IDs=%v, want [%q]", requestIDs, captureRequestID("ORDER123"))
	}
}

func TestCaptureOrderHandler_ConcurrentRequestsCaptureOnce(t *testing.T) {
	saveTransport(t)
	var orderMutex sync.Mutex
	setOrderLock(t, func(ctx context.Context, orderID string, fn func(context.Context) error) error {
		orderMutex.Lock()
		defer orderMutex.Unlock()
		return fn(ctx)
	})

	completedOrder := `{"status":"COMPLETED","purchase_units":[{"payments":{"captures":[{"id":"CAPWINNER","status":"COMPLETED","create_time":"2026-11-06T12:00:00Z","amount":{"value":"25.00","currency_code":"USD"}}]}}]}`
	var stateMu sync.Mutex
	orderCompleted := false
	captureCalls := 0
	requestIDs := []string{}
	httpClient.Transport = mockRoundTripper(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodPost && req.URL.Path == "/v1/oauth2/token" {
			return resp(http.StatusOK, `{"access_token":"fake-token"}`), nil
		}
		if req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/v2/checkout/orders/") {
			stateMu.Lock()
			completed := orderCompleted
			stateMu.Unlock()
			if completed {
				return resp(http.StatusOK, completedOrder), nil
			}
			return resp(http.StatusOK, `{"status":"APPROVED"}`), nil
		}
		if req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/capture") {
			stateMu.Lock()
			defer stateMu.Unlock()
			captureCalls++
			requestIDs = append(requestIDs, req.Header.Get("PayPal-Request-Id"))
			orderCompleted = true
			return resp(http.StatusOK, completedOrder), nil
		}
		return resp(http.StatusNotFound, `{}`), nil
	})

	rows := map[string]PaymentRecord{}
	var rowsMu sync.Mutex
	originalPersist := persistCapture
	persistCapture = func(ctx context.Context, payment PaymentRecord) error {
		rowsMu.Lock()
		defer rowsMu.Unlock()
		rows[payment.ID] = payment
		return nil
	}
	t.Cleanup(func() { persistCapture = originalPersist })

	const requestCount = 12
	type result struct {
		status int
		body   string
	}
	results := make([]result, requestCount)
	start := make(chan struct{})
	var wg sync.WaitGroup
	handler := makeCaptureOrderHandler("fake", "fake")
	for i := 0; i < requestCount; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			req := httptest.NewRequest(http.MethodPost, "/api/capture-order", strings.NewReader(`{"orderID":"ORDER123"}`))
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			results[i] = result{status: w.Code, body: w.Body.String()}
		}(i)
	}
	close(start)
	wg.Wait()

	for i, result := range results {
		if result.status != http.StatusOK {
			t.Errorf("request %d returned %d: %s", i, result.status, result.body)
			continue
		}
		var payment PaymentRecord
		if err := json.Unmarshal([]byte(result.body), &payment); err != nil {
			t.Errorf("request %d response JSON: %v", i, err)
			continue
		}
		if payment.ID != "CAPWINNER" || payment.AmountMinor != 2500 {
			t.Errorf("request %d returned payment %+v, want winning capture CAPWINNER/2500", i, payment)
		}
	}
	stateMu.Lock()
	gotCaptureCalls := captureCalls
	gotRequestIDs := append([]string(nil), requestIDs...)
	stateMu.Unlock()
	rowsMu.Lock()
	gotRows := len(rows)
	row, hasWinner := rows["CAPWINNER"]
	rowsMu.Unlock()
	if gotCaptureCalls != 1 {
		t.Errorf("PayPal capture calls = %d, want 1", gotCaptureCalls)
	}
	if len(gotRequestIDs) != 1 || gotRequestIDs[0] != captureRequestID("ORDER123") {
		t.Errorf("PayPal request IDs = %v, want [%q]", gotRequestIDs, captureRequestID("ORDER123"))
	}
	if gotRows != 1 || !hasWinner || row.AmountMinor != 2500 {
		t.Errorf("persisted rows = %d, winning row=%+v found=%t; want exactly one winner row", gotRows, row, hasWinner)
	}
}

// --- Handler-level tests ---

func TestCaptureOrderHandler_UnapprovedOrder(t *testing.T) {
	mockEndpoints(t, `{"status":"CREATED"}`, "", "")
	handler := makeCaptureOrderHandler("fake", "fake")
	req := httptest.NewRequest(http.MethodPost, "/api/capture-order", strings.NewReader(`{"orderID":"ORDER123"}`))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("Expected 400 for unapproved order, got %d, body: %s", w.Code, w.Body.String())
	}
}

func TestCaptureOrderHandler_WrongAmount(t *testing.T) {
	badCapture := `{"status":"COMPLETED","purchase_units":[{"payments":{"captures":[{"id":"X","status":"COMPLETED","create_time":"2026-11-06T12:00:00Z","amount":{"value":"999.00","currency_code":"USD"}}]}}]}`
	mockEndpoints(t, `{"status":"APPROVED"}`, badCapture, "")
	handler := makeCaptureOrderHandler("fake", "fake")
	req := httptest.NewRequest(http.MethodPost, "/api/capture-order", strings.NewReader(`{"orderID":"ORDER123"}`))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusBadGateway {
		t.Errorf("Expected 502 for wrong amount, got %d, body: %s", w.Code, w.Body.String())
	}
}

func TestLiveRecordsQueryUsesLegacyMinorFallback(t *testing.T) {
	if !strings.Contains(liveRecordsQuery, amountMinorFallbackExpression) {
		t.Fatalf("live-record query does not use legacy amount fallback: %s", liveRecordsQuery)
	}
}

func TestCaptureOrderHandler_PersistenceFailureRetryReconciles(t *testing.T) {
	saveTransport(t)
	setOrderLock(t, func(ctx context.Context, orderID string, fn func(context.Context) error) error { return fn(ctx) })
	completedOrder := `{"status":"COMPLETED","purchase_units":[{"payments":{"captures":[{"id":"CAPRETRY","status":"COMPLETED","create_time":"2026-11-06T12:00:00Z","amount":{"value":"25.00","currency_code":"USD"}}]}}]}`
	orderCompleted := false
	captureCalls := 0
	httpClient.Transport = mockRoundTripper(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodPost && req.URL.Path == "/v1/oauth2/token" {
			return resp(http.StatusOK, `{"access_token":"fake-token"}`), nil
		}
		if req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/v2/checkout/orders/") {
			if orderCompleted {
				return resp(http.StatusOK, completedOrder), nil
			}
			return resp(http.StatusOK, `{"status":"APPROVED"}`), nil
		}
		if req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/capture") {
			captureCalls++
			orderCompleted = true
			return resp(http.StatusOK, completedOrder), nil
		}
		return resp(http.StatusNotFound, `{}`), nil
	})

	persistCalls := 0
	rows := map[string]PaymentRecord{}
	originalPersist := persistCapture
	persistCapture = func(ctx context.Context, payment PaymentRecord) error {
		persistCalls++
		if persistCalls == 1 {
			return errors.New("simulated DB insert failure")
		}
		rows[payment.ID] = payment
		return nil
	}
	t.Cleanup(func() { persistCapture = originalPersist })

	handler := makeCaptureOrderHandler("fake", "fake")
	request := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/capture-order", strings.NewReader(`{"orderID":"ORDER123"}`))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		return w
	}
	first := request()
	if first.Code != http.StatusInternalServerError || !strings.Contains(first.Body.String(), "capture_succeeded_persistence_pending") {
		t.Fatalf("first request = %d %s, want persistence-pending 500", first.Code, first.Body.String())
	}
	second := request()
	if second.Code != http.StatusOK {
		t.Fatalf("retry = %d %s, want 200", second.Code, second.Body.String())
	}
	var payment PaymentRecord
	if err := json.Unmarshal(second.Body.Bytes(), &payment); err != nil {
		t.Fatalf("decode retry response: %v", err)
	}
	if payment.ID != "CAPRETRY" || captureCalls != 1 || persistCalls != 2 || len(rows) != 1 {
		t.Fatalf("retry payment=%+v captures=%d persists=%d rows=%d; want reconciled single capture and row", payment, captureCalls, persistCalls, len(rows))
	}
}

func TestCaptureOrderHandler_DBFailure(t *testing.T) {
	goodCapture := `{"status":"COMPLETED","purchase_units":[{"payments":{"captures":[{"id":"CAP123","status":"COMPLETED","create_time":"2026-11-06T12:00:00Z","amount":{"value":"25.00","currency_code":"USD"}}]}}]}`
	mockEndpoints(t, `{"status":"APPROVED"}`, goodCapture, "")

	orig := persistCapture
	persistCapture = func(ctx context.Context, p PaymentRecord) error { return errors.New("db offline") }
	defer func() { persistCapture = orig }()

	handler := makeCaptureOrderHandler("fake", "fake")
	req := httptest.NewRequest(http.MethodPost, "/api/capture-order", strings.NewReader(`{"orderID":"ORDER123"}`))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("Expected 500 for DB failure, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "capture_succeeded_persistence_pending") {
		t.Errorf("Expected pending state, got %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "CAP123") {
		t.Errorf("Expected capture_id in response, got %s", w.Body.String())
	}
}

func TestCaptureOrderHandler_RetryReconciles(t *testing.T) {
	reconcileBody := `{"status":"COMPLETED","purchase_units":[{"payments":{"captures":[{"id":"CAP123","status":"COMPLETED","create_time":"2026-11-06T12:00:00Z","amount":{"value":"25.00","currency_code":"USD"}}]}}]}`
	mockEndpoints(t, `{"status":"COMPLETED"}`, "", reconcileBody)

	persistCalled := 0
	orig := persistCapture
	persistCapture = func(ctx context.Context, p PaymentRecord) error {
		persistCalled++
		if p.ID != "CAP123" {
			return errors.New("wrong capture id")
		}
		return nil
	}
	defer func() { persistCapture = orig }()

	handler := makeCaptureOrderHandler("fake", "fake")
	req := httptest.NewRequest(http.MethodPost, "/api/capture-order", strings.NewReader(`{"orderID":"ORDER123"}`))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected 200 on retry reconcile, got %d, body: %s", w.Code, w.Body.String())
	}
	if persistCalled != 1 {
		t.Errorf("Expected persist to be called once, got %d", persistCalled)
	}
}

func TestPayPalWebhookHandlerVerifiesRawEventAndDeduplicates(t *testing.T) {
	saveTransport(t)
	rawEvent := []byte("{ \"id\":\"WH-EVENT-1\", \"event_type\":\"PAYMENT.CAPTURE.COMPLETED\",\"resource\":{\"id\":\"CAP-1\"} }")
	previousProcessor := processVerifiedWebhook
	seen := make(map[string]bool)
	processCalls := 0
	processVerifiedWebhook = func(_ context.Context, event paypalWebhookEvent, body []byte) (bool, error) {
		processCalls++
		if event.EventType != "PAYMENT.CAPTURE.COMPLETED" || !bytes.Equal(body, rawEvent) {
			t.Errorf("processed webhook event/body mismatch: type=%q body=%q", event.EventType, body)
		}
		duplicate := seen[event.ID]
		seen[event.ID] = true
		return duplicate, nil
	}
	t.Cleanup(func() { processVerifiedWebhook = previousProcessor })

	verifyCalls := 0
	httpClient.Transport = mockRoundTripper(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/v1/oauth2/token":
			return resp(http.StatusOK, `{"access_token":"fake-token"}`), nil
		case "/v1/notifications/verify-webhook-signature":
			verifyCalls++
			if req.Header.Get("Authorization") != "Bearer fake-token" {
				t.Errorf("verification authorization header = %q", req.Header.Get("Authorization"))
			}
			requestBytes, err := io.ReadAll(req.Body)
			if err != nil {
				t.Errorf("decode signature verification payload: %v", err)
			} else if !bytes.Contains(requestBytes, rawEvent) || !bytes.Contains(requestBytes, []byte(`"webhook_id":"WH-CONFIGURED"`)) || !bytes.Contains(requestBytes, []byte(`"transmission_id":"tx-1"`)) {
				t.Errorf("verification request did not preserve exact raw event bytes: %s", requestBytes)
			}
			return resp(http.StatusOK, `{"verification_status":"SUCCESS"}`), nil
		default:
			return resp(http.StatusNotFound, `{}`), nil
		}
	})

	handler := makePayPalWebhookHandler("fake-client", "fake-secret", "WH-CONFIGURED")
	request := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/webhooks/paypal", bytes.NewReader(rawEvent))
		req.Header.Set("PayPal-Auth-Algo", "SHA256withRSA")
		req.Header.Set("PayPal-Cert-Url", "https://api.sandbox.paypal.com/cert")
		req.Header.Set("PayPal-Transmission-Id", "tx-1")
		req.Header.Set("PayPal-Transmission-Sig", "signature")
		req.Header.Set("PayPal-Transmission-Time", "2026-11-06T12:00:00Z")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		return w
	}
	first := request()
	second := request()
	if first.Code != http.StatusOK || strings.Contains(first.Body.String(), `"duplicate":true`) {
		t.Fatalf("first webhook response = %d %s", first.Code, first.Body.String())
	}
	if second.Code != http.StatusOK || !strings.Contains(second.Body.String(), `"duplicate":true`) {
		t.Fatalf("duplicate webhook response = %d %s", second.Code, second.Body.String())
	}
	if verifyCalls != 2 || processCalls != 2 || len(seen) != 1 {
		t.Fatalf("signature verifications=%d process calls=%d receipts=%d; want two verifications/process calls and one receipt", verifyCalls, processCalls, len(seen))
	}
}

func TestPayPalWebhookHandlerRejectsTamperedBody(t *testing.T) {
	saveTransport(t)
	previousProcessor := processVerifiedWebhook
	processCalls := 0
	processVerifiedWebhook = func(context.Context, paypalWebhookEvent, []byte) (bool, error) {
		processCalls++
		return false, nil
	}
	t.Cleanup(func() { processVerifiedWebhook = previousProcessor })
	httpClient.Transport = mockRoundTripper(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/v1/oauth2/token" {
			return resp(http.StatusOK, `{"access_token":"fake-token"}`), nil
		}
		if req.URL.Path == "/v1/notifications/verify-webhook-signature" {
			return resp(http.StatusOK, `{"verification_status":"FAILURE"}`), nil
		}
		return resp(http.StatusNotFound, `{}`), nil
	})
	req := httptest.NewRequest(http.MethodPost, "/webhooks/paypal", strings.NewReader(`{"id":"WH-EVENT-2","event_type":"PAYMENT.CAPTURE.COMPLETED"}`))
	req.Header.Set("PayPal-Auth-Algo", "SHA256withRSA")
	req.Header.Set("PayPal-Cert-Url", "https://api.sandbox.paypal.com/cert")
	req.Header.Set("PayPal-Transmission-Id", "tx-2")
	req.Header.Set("PayPal-Transmission-Sig", "signature")
	req.Header.Set("PayPal-Transmission-Time", "2026-11-06T12:00:00Z")
	w := httptest.NewRecorder()
	makePayPalWebhookHandler("fake-client", "fake-secret", "WH-CONFIGURED").ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized || processCalls != 0 {
		t.Fatalf("tampered body response=%d processing=%d, want 401 and no receipt", w.Code, processCalls)
	}
}

func TestPayPalWebhookHandlerFailsClosedWithoutWebhookID(t *testing.T) {
	saveTransport(t)
	var transportCalls int
	httpClient.Transport = mockRoundTripper(func(req *http.Request) (*http.Response, error) {
		transportCalls++
		return resp(http.StatusOK, `{"access_token":"fake-token"}`), nil
	})
	req := validWebhookRequest(`{"id":"WH-NO-ID","event_type":"PAYMENT.CAPTURE.COMPLETED"}`)
	w := httptest.NewRecorder()
	makePayPalWebhookHandler("fake-client", "fake-secret", " ").ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable || transportCalls != 0 {
		t.Fatalf("missing webhook ID response=%d outbound calls=%d, want 503 and no PayPal calls", w.Code, transportCalls)
	}
}

func TestPayPalWebhookUnprocessedDuplicateReprocessedAfterFailure(t *testing.T) {
	saveTransport(t)
	previousProcessor := processVerifiedWebhook
	processedAtSet := false
	processCalls := 0
	processVerifiedWebhook = func(_ context.Context, event paypalWebhookEvent, _ []byte) (bool, error) {
		processCalls++
		if processedAtSet {
			return true, nil
		}
		if processCalls == 1 {
			return false, errors.New("simulated handler failure")
		}
		processedAtSet = true
		return true, nil
	}
	t.Cleanup(func() { processVerifiedWebhook = previousProcessor })
	httpClient.Transport = mockRoundTripper(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/v1/oauth2/token" {
			return resp(http.StatusOK, `{"access_token":"fake-token"}`), nil
		}
		if req.URL.Path == "/v1/notifications/verify-webhook-signature" {
			return resp(http.StatusOK, `{"verification_status":"SUCCESS"}`), nil
		}
		return resp(http.StatusNotFound, `{}`), nil
	})
	body := `{"id":"WH-RETRY","event_type":"PAYMENT.CAPTURE.COMPLETED"}`
	first := httptest.NewRecorder()
	makePayPalWebhookHandler("id", "secret", "WH-1").ServeHTTP(first, validWebhookRequest(body))
	if first.Code != http.StatusInternalServerError || processedAtSet {
		t.Fatalf("first delivery status=%d processed=%v, want 500 and unprocessed", first.Code, processedAtSet)
	}
	second := httptest.NewRecorder()
	makePayPalWebhookHandler("id", "secret", "WH-1").ServeHTTP(second, validWebhookRequest(body))
	if second.Code != http.StatusOK || !processedAtSet || processCalls != 2 {
		t.Fatalf("retry status=%d processed=%v calls=%d, want 200, processed, two calls", second.Code, processedAtSet, processCalls)
	}
}

func validWebhookRequest(body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/webhooks/paypal", strings.NewReader(body))
	req.Header.Set("PayPal-Auth-Algo", "SHA256withRSA")
	req.Header.Set("PayPal-Cert-Url", "https://api.sandbox.paypal.com/cert")
	req.Header.Set("PayPal-Transmission-Id", "tx-test")
	req.Header.Set("PayPal-Transmission-Sig", "signature")
	req.Header.Set("PayPal-Transmission-Time", "2026-11-06T12:00:00Z")
	return req
}
