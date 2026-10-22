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

// Smart mock: Handles GET (status check) and POST (capture) differently
func setupSmartMock(statusBody string, captureBody string) {
	httpClient.Transport = mockRoundTripper(func(req *http.Request) (*http.Response, error) {
		if req.Method == "POST" && strings.HasSuffix(req.URL.Path, "/capture") {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(captureBody)), Header: make(http.Header)}, nil
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(statusBody)), Header: make(http.Header)}, nil
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

func TestCaptureOrderHandler_UnapprovedOrder(t *testing.T) {
	setupSmartMock(`{"status": "CREATED"}`, "")
	handler := makeCaptureOrderHandler("fake", "fake")
	req := httptest.NewRequest(http.MethodPost, "/api/capture-order", strings.NewReader(`{"orderID":"ORDER123"}`))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("Expected 400 for unapproved order, got %d", w.Code)
	}
}

func TestCaptureOrderHandler_WrongAmount(t *testing.T) {
	badCapture := `{"status": "COMPLETED", "purchase_units": [{"payments": {"captures": [{"id": "X", "status": "COMPLETED", "amount": {"value": "999.00", "currency_code": "USD"}}]}}]}`
	setupSmartMock(`{"status": "APPROVED"}`, badCapture)

	handler := makeCaptureOrderHandler("fake", "fake")
	req := httptest.NewRequest(http.MethodPost, "/api/capture-order", strings.NewReader(`{"orderID":"ORDER123"}`))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Errorf("Expected 502 for wrong amount, got %d", w.Code)
	}
}

func TestCaptureOrderHandler_DBFailure(t *testing.T) {
	goodCapture := `{"status": "COMPLETED", "purchase_units": [{"payments": {"captures": [{"id": "CAP123", "status": "COMPLETED", "amount": {"value": "25.00", "currency_code": "USD"}}]}}]}`
	setupSmartMock(`{"status": "APPROVED"}`, goodCapture)

	origPersist := persistCapture
	persistCapture = func(ctx context.Context, p PaymentRecord) error { return errors.New("db offline") }
	defer func() { persistCapture = origPersist }()

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
}
