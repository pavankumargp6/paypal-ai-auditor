package main

import "testing"

func TestCalculateSpendIntelligence(t *testing.T) {
	records := []PaymentRecord{
		{ID: "1", Amount: 25.00, Status: "COMPLETED"},
		{ID: "2", Amount: 150.50, Status: "COMPLETED"},
		{ID: "3", Amount: 0.00, Status: "DECLINED"},
	}

	summary := calculateSpendIntelligence(records)

	if summary.TotalCaptured != 175.50 {
		t.Errorf("Expected total 175.50, got %.2f", summary.TotalCaptured)
	}
	if summary.TotalCount != 2 {
		t.Errorf("Expected 2 successful, got %d", summary.TotalCount)
	}
	if summary.FailedCount != 1 {
		t.Errorf("Expected 1 failed, got %d", summary.FailedCount)
	}
}
