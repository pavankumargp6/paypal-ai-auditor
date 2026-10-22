package main

import "testing"

func TestCalculateSpendIntelligence(t *testing.T) {
	records := []PaymentRecord{
		{ID: "1", Amount: 25.00, Status: "COMPLETED", IsLive: true},
		{ID: "2", Amount: 150.50, Status: "COMPLETED", IsLive: false}, // Demo shouldn't count
		{ID: "3", Amount: 0.00, Status: "DECLINED", IsLive: true},
		{ID: "4", Amount: 0.00, Status: "FAILED", IsLive: true},
	}

	summary := calculateSpendIntelligence(records)

	if summary.TotalCaptured != 25.00 {
		t.Errorf("Expected total 25.00, got %.2f", summary.TotalCaptured)
	}
	if summary.LiveSuccess != 1 {
		t.Errorf("Expected 1 successful live payment, got %d", summary.LiveSuccess)
	}
	if summary.LiveFailed != 2 {
		t.Errorf("Expected 2 failed live payments, got %d", summary.LiveFailed)
	}
	// Success rate: 1 success / 3 total attempts = 33.33%
	expectedRate := float64(1) / float64(3) * 100
	if summary.SuccessRate != expectedRate {
		t.Errorf("Expected rate %.2f, got %.2f", expectedRate, summary.SuccessRate)
	}
}

func TestEmptyData(t *testing.T) {
	summary := calculateSpendIntelligence([]PaymentRecord{})
	if summary.TotalCaptured != 0 || summary.SuccessRate != 0 {
		t.Error("Expected zeroed summary for empty dataset")
	}
}
