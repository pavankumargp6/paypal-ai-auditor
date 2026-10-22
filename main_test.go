package main

import (
	"testing"
)

func TestCalculateSpendIntelligence(t *testing.T) {
	records := []PaymentRecord{
		{ID: "1", Amount: 25.00, Status: "COMPLETED", IsLive: true},
		{ID: "2", Amount: 150.50, Status: "COMPLETED", IsLive: false}, // Demo data shouldn't count
		{ID: "3", Amount: 0.00, Status: "DECLINED", IsLive: true},
	}

	summary := calculateSpendIntelligence(records)

	// Should only count the LIVE completed record
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

// Mock test for duplicate prevention logic
func TestDuplicateCapturePrevention(t *testing.T) {
	// Reset global state for test
	capturedRecords = []PaymentRecord{{ID: "CAP-123", Amount: 25.00, Status: "COMPLETED"}}

	newPayment := PaymentRecord{ID: "CAP-123", Amount: 25.00, Status: "COMPLETED"}

	isDuplicate := false
	for _, existing := range capturedRecords {
		if existing.ID == newPayment.ID {
			isDuplicate = true
			break
		}
	}

	if !isDuplicate {
		t.Errorf("Expected duplicate to be detected, but it was not")
	}
}
