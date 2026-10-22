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

func resp(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

// Endpoint-aware mock: distinguishes GET (verify/reconcile) from POST (capture)
func mockEndpoints(verifyBody, captureBody, reconcileBody string) {
	httpClient.Transport = mockRoundTripper(func(req *http.Request) (*http.Response, error) {
		if req.Method == "POST" && strings.HasSuffix(req.URL.Path, "/capture") {
			return resp(200, captureBody), nil
		}
		// GET on /v2/checkout/orders/{id} — used by both verifyOrderStatus and reconcileCapture
		// The caller reads different fields, so we need to send a body that satisfies both.
		// If reconcileBody is set, it means we're testing the reconcile path — return it.
		// Otherwise return verifyBody (which is what status check reads).
		// Trick: return a body that has both `status` and `purchase_units` when reconcile is needed.
		if reconcileBody != "" {
			return resp(200, reconcileBody), nil
		}
		return resp(200, verifyBody), nil
	})
}

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

func TestVerifyOrderStatus_MalformedJSON(t *testing.T) {
	httpClient.Transport = mockRoundTripper(func(req *http.Request) (*http.Response, error) {
		return resp(200, `not-json`), nil
	})
	_, err := verifyOrderStatus(context.Background(), "token", "ORDER123")
	if err == nil {
		t.Error("Expected error on malformed JSON status response")
	}
}

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

func TestCaptureOrderHandler_UnapprovedOrder(t *testing.T) {
	mockEndpoints(`{"status":"CREATED"}`, "", "")
	handler := makeCaptureOrderHandler("fake", "fake")
	req := httptest.NewRequest(http.MethodPost, "/api/capture-order", strings.NewReader(`{"orderID":"ORDER123"}`))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("Expected 400 for unapproved order, got %d", w.Code)
	}
}

func TestCaptureOrderHandler_WrongAmount(t *testing.T) {
	badCapture := `{"status":"COMPLETED","purchase_units":[{"payments":{"captures":[{"id":"X","status":"COMPLETED","amount":{"value":"999.00","currency_code":"USD"}}]}}]}`
	mockEndpoints(`{"status":"APPROVED"}`, badCapture, "")
	handler := makeCaptureOrderHandler("fake", "fake")
	req := httptest.NewRequest(http.MethodPost, "/api/capture-order", strings.NewReader(`{"orderID":"ORDER123"}`))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusBadGateway {
		t.Errorf("Expected 502 for wrong amount, got %d", w.Code)
	}
}

// Test 1: DB fails on capture — must return pending state, not lose the payment
func TestCaptureOrderHandler_DBFailure(t *testing.T) {
	goodCapture := `{"status":"COMPLETED","purchase_units":[{"payments":{"captures":[{"id":"CAP123","status":"COMPLETED","amount":{"value":"25.00","currency_code":"USD"}}]}}]}`
	mockEndpoints(`{"status":"APPROVED"}`, goodCapture, "")

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

// Test 2: Retry after DB failure — PayPal is now COMPLETED, handler reconciles without double-capturing
func TestCaptureOrderHandler_RetryReconciles(t *testing.T) {
	// PayPal now reports COMPLETED (because the earlier POST already captured it)
	reconcileBody := `{"status":"COMPLETED","purchase_units":[{"payments":{"captures":[{"id":"CAP123","status":"COMPLETED","amount":{"value":"25.00","currency_code":"USD"}}]}}]}`
	mockEndpoints(`{"status":"COMPLETED"}`, "", reconcileBody)

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
