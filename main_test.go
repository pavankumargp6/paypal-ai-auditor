package main

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type mockRoundTripper func(*http.Request) (*http.Response, error)

func (m mockRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return m(req)
}

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
	valid := "5O190127TN364715T"
	invalid := "malicious-id-with-slash/../"

	if !orderIDRegex.MatchString(valid) {
		t.Errorf("Expected %s to be valid", valid)
	}
	if orderIDRegex.MatchString(invalid) {
		t.Errorf("Expected %s to be invalid", invalid)
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

func TestParseCaptureFromBody_Valid(t *testing.T) {
	body := []byte(`{"status": "COMPLETED", "purchase_units": [{"payments": {"captures": [{"id": "X", "status": "COMPLETED", "amount": {"value": "25.00", "currency_code": "USD"}}]}}]}`)
	rec, err := parseCaptureFromBody(body)
	if err != nil || rec.Amount != 25.00 {
		t.Errorf("Parsing failed: %v", err)
	}
}

func TestVerifyOrderStatus(t *testing.T) {
	ctx := context.Background()

	setupMockClient(200, `{"status": "APPROVED"}`)
	status, err := verifyOrderStatus(ctx, "token", "ORDER123")
	if err != nil || status != "APPROVED" {
		t.Errorf("Expected APPROVED, got %s, err: %v", status, err)
	}

	setupMockClient(200, `{"status": "CREATED"}`)
	status, err = verifyOrderStatus(ctx, "token", "ORDER123")
	if err != nil || status != "CREATED" {
		t.Errorf("Expected CREATED, got %s", status)
	}
}

func TestCapturePayPalOrder_Incomplete(t *testing.T) {
	ctx := context.Background()

	setupMockClient(201, `{"status": "COMPLETED", "purchase_units": []}`)
	_, err := capturePayPalOrder(ctx, "token", "ORDER123")
	if err == nil {
		t.Error("Expected error for incomplete capture response")
	}
}
