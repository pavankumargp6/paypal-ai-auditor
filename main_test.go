package main

import (
	"testing"
)

func TestCalculateSpendIntelligence(t *testing.T) {
	records := []PaymentRecord{
		{ID: "1", Amount: 25.00, Status: "COMPLETED", IsLive: true},
		{ID: "2", Amount: 150.50, Status: "COMPLETED", IsLive: false}, // Demo shouldn't count
		{ID: "3", Amount: 0.00, Status: "DECLINED", IsLive: true},
	}

	summary := calculateSpendIntelligence(records)

	if summary.TotalCaptured != 25.00 {
		t.Errorf("Expected total 25.00, got %.2f", summary.TotalCaptured)
	}
	if summary.TotalCount != 1 {
		t.Errorf("Expected 1 successful live payment, got %d", summary.TotalCount)
	}
	if summary.FailedCount != 1 {
		t.Errorf("Expected 1 failed payment, got %d", summary.FailedCount)
	}
}
