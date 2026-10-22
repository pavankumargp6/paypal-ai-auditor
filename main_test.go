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

// Smarter mock that handles both the status check (GET) and capture (POST)
func setupSmartMock(statusBody string, captureStatus int, captureBody string) {
	httpClient.Transport = mockRoundTripper(func(req *http.Request) (*http.Response, error) {
		// If it's a POST to /capture, return the captureBody
		if req.Method == "POST" && strings.HasSuffix(req.URL.Path, "/capture") {
			return &http.Response{
				StatusCode: captureStatus,
				Body:       io.NopCloser(strings.NewReader(captureBody)),
				Header:     make(http.Header),
			}, nil
		}
		// Otherwise, return the statusBody (for verifyOrderStatus)
		return &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(strings.NewReader(statusBody)),
			Header:     make(http.Header),
		}, nil
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

func TestVerifyOrderStatus_ErrorHandling(t *testing.T) {
	ctx := context.Background()
	setupSmartMock(`{"error": "server down"}`, 200, "")
	// The mock returns 200 for GET, but the body is bad, so parsing should handle it gracefully.
	status, _ := verifyOrderStatus(ctx, "token", "ORDER123")
	if status != "" {
		t.Errorf("Expected empty status, got %s", status)
	}
}

// Test: Handler rejects unapproved orders (CREATED)
func TestCaptureOrderHandler_UnapprovedOrder(t *testing.T) {
	setupSmartMock(`{"status": "CREATED"}`, 200, "")

	handler := makeCaptureOrderHandler("fake", "fake")
	req := httptest.NewRequest(http.MethodPost, "/api/capture-order", strings.NewReader(`{"orderID":"ORDER123"}`))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("Expected 400 Bad Request for unapproved order, got %d", w.Code)
	}
}

// Test: Handler rejects wrong amount returned from PayPal
func TestCaptureOrderHandler_WrongAmount(t *testing.T) {
	// Status check returns APPROVED. Capture returns a bad amount.
	badCaptureBody := `{"status": "COMPLETED", "purchase_units": [{"payments": {"captures": [{"id": "X", "status": "COMPLETED", "amount": {"value": "999.00", "currency_code": "USD"}}]}}]}`
	setupSmartMock(`{"status": "APPROVED"}`, 200, badCaptureBody)

	handler := makeCaptureOrderHandler("fake", "fake")
	req := httptest.NewRequest(http.MethodPost, "/api/capture-order", strings.NewReader(`{"orderID":"ORDER123"}`))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	// Should hit validatePayment and fail with 502
	if w.Code != http.StatusBadGateway {
		t.Errorf("Expected 502 Bad Gateway for wrong amount, got %d", w.Code)
	}
}

// Test: Handler returns pending state if DB fails
func TestCaptureOrderHandler_DBFailure(t *testing.T) {
	// Status check returns APPROVED. Capture returns valid data.
	goodCaptureBody := `{"status": "COMPLETED", "purchase_units": [{"payments": {"captures": [{"id": "CAP123", "status": "COMPLETED", "amount": {"value": "25.00", "currency_code": "USD"}}]}}]}`
	setupSmartMock(`{"status": "APPROVED"}`, 200, goodCaptureBody)

	// Mock DB to fail
	origPersist := persistCapture
	persistCapture = func(ctx context.Context, p PaymentRecord) error {
		return errors.New("database offline")
	}
	defer func() { persistCapture = origPersist }()

	handler := makeCaptureOrderHandler("fake", "fake")
	req := httptest.NewRequest(http.MethodPost, "/api/capture-order", strings.NewReader(`{"orderID":"ORDER123"}`))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("Expected 500 for DB failure, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "capture_succeeded_persistence_pending") {
		t.Errorf("Expected pending state response, got %s", w.Body.String())
	}
}
