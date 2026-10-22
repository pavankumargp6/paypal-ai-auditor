package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
	if err := validatePayment(PaymentRecord{Amount: 25.00, Currency: "USD"}); err != nil {
		t.Error("Valid failed")
	}
	if err := validatePayment(PaymentRecord{Amount: 24.00, Currency: "USD"}); err == nil {
		t.Error("Wrong amount passed")
	}
	if err := validatePayment(PaymentRecord{Amount: 25.00, Currency: "EUR"}); err == nil {
		t.Error("Wrong currency passed")
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
	badCapture := `{"status":"COMPLETED","purchase_units":[{"payments":{"captures":[{"id":"X","status":"COMPLETED","amount":{"value":"999.00","currency_code":"USD"}}]}}]}`
	mockEndpoints(t, `{"status":"APPROVED"}`, badCapture, "")
	handler := makeCaptureOrderHandler("fake", "fake")
	req := httptest.NewRequest(http.MethodPost, "/api/capture-order", strings.NewReader(`{"orderID":"ORDER123"}`))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusBadGateway {
		t.Errorf("Expected 502 for wrong amount, got %d, body: %s", w.Code, w.Body.String())
	}
}

func TestCaptureOrderHandler_DBFailure(t *testing.T) {
	goodCapture := `{"status":"COMPLETED","purchase_units":[{"payments":{"captures":[{"id":"CAP123","status":"COMPLETED","amount":{"value":"25.00","currency_code":"USD"}}]}}]}`
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
	reconcileBody := `{"status":"COMPLETED","purchase_units":[{"payments":{"captures":[{"id":"CAP123","status":"COMPLETED","amount":{"value":"25.00","currency_code":"USD"}}]}}]}`
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
