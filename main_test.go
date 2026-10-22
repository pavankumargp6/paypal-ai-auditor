package main

import "testing"

func TestCalculateSummary_OnlyLiveCounts(t *testing.T) {
	live := []PaymentRecord{
		{ID: "L1", Amount: 25.00, Status: "COMPLETED", IsLive: true},
		{ID: "L2", Amount: 100.00, Status: "COMPLETED", IsLive: true},
		{ID: "L3", Amount: 0.00, Status: "DECLINED", IsLive: true},
	}
	demo := []PaymentRecord{
		{ID: "D1", Amount: 999.99, Status: "COMPLETED", IsLive: false},
		{ID: "D2", Amount: 0.00, Status: "DECLINED", IsLive: false},
	}

	summary := calculateSummary(live, demo)

	if summary.TotalCaptured != 125.00 {
		t.Errorf("Expected live total 125.00, got %.2f", summary.TotalCaptured)
	}
	if summary.LiveSuccess != 2 {
		t.Errorf("Expected 2 live successes, got %d", summary.LiveSuccess)
	}
	if summary.DemoCompletedCount != 1 {
		t.Errorf("Expected 1 demo completed, got %d", summary.DemoCompletedCount)
	}
}

func TestCalculateSummary_Empty(t *testing.T) {
	summary := calculateSummary([]PaymentRecord{}, []PaymentRecord{})

	if summary.TotalCaptured != 0 || summary.LiveSuccess != 0 || summary.DeclineRateAvailable {
		t.Error("Expected zeroed summary and unavailable decline rate for empty dataset")
	}
}

func TestParseCaptureFromBody_Valid(t *testing.T) {
	body := []byte(`{
		"status": "COMPLETED",
		"purchase_units": [{
			"payments": {
				"captures": [{
					"id": "95X58163J19414540",
					"status": "COMPLETED",
					"amount": {"value": "25.00", "currency_code": "USD"}
				}]
			}
		}]
	}`)

	rec, err := parseCaptureFromBody(body)
	if err != nil || rec.ID != "95X58163J19414540" || rec.Amount != 25.00 {
		t.Errorf("Parsing failed: %v, rec: %+v", err, rec)
	}
}
