package main

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type mockRoundTripper func(*http.Request) (*http.Response, error)

func (m mockRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) { return m(req) }

func setupMockClient(statusCode int, body string) {
	httpClient.Transport = mockRoundTripper(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: statusCode,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     make(http.Header),
		}, nil
	})
}

func TestOrderIDValidation(t *testing.T) {
	if !orderIDRegex.MatchString("5O190127TN364715T") {
		t.Error("Valid ID failed")
	}
	if orderIDRegex.MatchString("malicious/../") {
		t.Error("Invalid ID passed")
	}
}

func TestCalculateSummary_LiveVsDemo(t *testing.T) {
	live := []PaymentRecord{{ID: "L1", Amount: 25.00, Status: "COMPLETED", IsLive: true}}
	demo := []PaymentRecord{{ID: "D1", Amount: 100.00, Status: "COMPLETED", IsLive: false}}
	summary := calculateSummary(live, demo)
	if summary.TotalCaptured != 25.00 {
		t.Errorf("Expected 25.00, got %.2f", summary.TotalCaptured)
	}
}

func TestValidatePayment(t *testing.T) {
	// 1. Valid Payment
	valid := PaymentRecord{Amount: 25.00, Currency: "USD"}
	if err := validatePayment(valid); err != nil {
		t.Errorf("Valid payment failed: %v", err)
	}

	// 2. Wrong Amount
	wrongAmt := PaymentRecord{Amount: 24.99, Currency: "USD"}
	if err := validatePayment(wrongAmt); err == nil {
		t.Error("Expected error for wrong amount")
	}

	// 3. Wrong Currency
	wrongCur := PaymentRecord{Amount: 25.00, Currency: "EUR"}
	if err := validatePayment(wrongCur); err == nil {
		t.Error("Expected error for wrong currency")
	}
}

func TestVerifyOrderStatus(t *testing.T) {
	ctx := context.Background()

	// 1. APPROVED (Valid for capture)
	setupMockClient(200, `{"status": "APPROVED"}`)
	status, _ := verifyOrderStatus(ctx, "token", "ORDER123")
	if status != "APPROVED" {
		t.Errorf("Expected APPROVED, got %s", status)
	}

	// 2. CREATED (Unapproved, must be rejected before capture)
	setupMockClient(200, `{"status": "CREATED"}`)
	status, _ = verifyOrderStatus(ctx, "token", "ORDER123")
	if status != "CREATED" {
		t.Errorf("Expected CREATED, got %s", status)
	}

	// 3. COMPLETED (Already captured, trigger reconciliation)
	setupMockClient(200, `{"status": "COMPLETED"}`)
	status, _ = verifyOrderStatus(ctx, "token", "ORDER123")
	if status != "COMPLETED" {
		t.Errorf("Expected COMPLETED, got %s", status)
	}
}

func TestParseCaptureFromBody(t *testing.T) {
	// 1. Valid Capture
	validBody := []byte(`{"status": "COMPLETED", "purchase_units": [{"payments": {"captures": [{"id": "X", "status": "COMPLETED", "amount": {"value": "25.00", "currency_code": "USD"}}]}}]}`)
	rec, err := parseCaptureFromBody(validBody)
	if err != nil || rec.Amount != 25.00 {
		t.Errorf("Parsing failed: %v", err)
	}

	// 2. Incomplete (No captures)
	incompleteBody := []byte(`{"status": "COMPLETED", "purchase_units": []}`)
	_, err = parseCaptureFromBody(incompleteBody)
	if err == nil {
		t.Error("Expected error for incomplete body")
	}

	// 3. Not COMPLETED status
	pendingBody := []byte(`{"status": "PENDING", "purchase_units": [{"payments": {"captures": [{"id": "X", "status": "PENDING", "amount": {"value": "25.00", "currency_code": "USD"}}]}}]}`)
	_, err = parseCaptureFromBody(pendingBody)
	if err == nil {
		t.Error("Expected error for non-COMPLETED status")
	}
}
