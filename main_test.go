package main

import (
	"context"
	"testing"
)

func TestFindRecurringSubscriptions(t *testing.T) {
	tests := []struct {
		name     string
		txs      []Transaction
		expected int
	}{
		{
			name:     "Empty Input",
			txs:      []Transaction{},
			expected: 0,
		},
		{
			name: "Irregular Purchases",
			txs: []Transaction{
				{Description: "Uber", Amount: 15, Date: "2026-01-01"},
				{Description: "Uber", Amount: 15, Date: "2026-02-15"},
				{Description: "Uber", Amount: 15, Date: "2026-04-20"},
			},
			expected: 0,
		},
		{
			name: "Valid Monthly Recurring",
			txs: []Transaction{
				{Description: "Netflix", Amount: 15.99, Date: "2026-01-01"},
				{Description: "Netflix", Amount: 15.99, Date: "2026-02-01"},
				{Description: "Netflix", Amount: 15.99, Date: "2026-03-01"},
			},
			expected: 1,
		},
		{
			name: "Variable Monthly Recurring",
			txs: []Transaction{
				{Description: "AWS", Amount: 45.00, Date: "2026-01-01"},
				{Description: "AWS", Amount: 50.00, Date: "2026-02-01"},
				{Description: "AWS", Amount: 48.00, Date: "2026-03-01"},
			},
			expected: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, _ := findRecurringSubscriptions(tc.txs)
			if len(result) != tc.expected {
				t.Errorf("Expected %d subs, got %d", tc.expected, len(result))
			}
		})
	}
}

func TestEnrichAndValidateAI_Empty(t *testing.T) {
	// If no recurring subs, it should safely return an empty array
	res := enrichAndValidateAI(context.Background(), []RecurringSub{}, "fake_key", "Test Source")
	if res == nil {
		t.Error("Expected empty array, got nil")
	}
	if len(res) != 0 {
		t.Errorf("Expected 0 results, got %d", len(res))
	}
}
