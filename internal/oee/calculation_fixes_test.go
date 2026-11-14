package oee

import (
	"math/big"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Regression tests for the calculation defects found in review of the OEE kernel. Each test
// fails on the pre-fix code and passes after the fix.

func TestZeroDenominatorZeroPolicyEmitsFlaggedZero(t *testing.T) {
	policy := DefaultKPIPolicy(uuid.New())
	policy.ZeroDenominatorRule = ZeroDenomZero
	t0 := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	window, _ := NewHalfOpenInterval(t0, t0.Add(time.Hour))
	res, err := CalculateKPIs(KPICalculationInput{
		Window:   window,
		Timeline: TimelineResolutionResult{Window: window, Readiness: ReadinessDecision{Status: StatusReady}},
		Policy:   policy,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The explicit ZERO policy emits a zero value, but the result stays flagged and not ready.
	if res.Availability.Value == nil || res.Availability.Formatted != "0.000000" || !res.Availability.IsZeroDenom {
		t.Fatalf("ZERO policy: value=%v formatted=%q zero=%v", res.Availability.Value, res.Availability.Formatted, res.Availability.IsZeroDenom)
	}
	if res.Readiness.Status != StatusNotReady {
		t.Fatalf("ZERO policy must stay NOT_READY, got %s", res.Readiness.Status)
	}
}

func TestGoodCountIsDerivedPerContribution(t *testing.T) {
	policy := DefaultKPIPolicy(uuid.New())
	t0 := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	window, _ := NewHalfOpenInterval(t0, t0.Add(time.Hour))
	res, err := CalculateKPIs(KPICalculationInput{
		Window: window,
		Timeline: TimelineResolutionResult{
			Window:    window,
			Segments:  []CanonicalFactSegment{{Interval: window, State: StateRunning}},
			Readiness: ReadinessDecision{Status: StatusReady},
		},
		CountContributions: []ProductCountContribution{
			{ContributionID: "a", ProductID: "P", Interval: window, TotalCount: big.NewRat(100, 1), ScrapCount: big.NewRat(10, 1), Unit: "pcs"},
			{ContributionID: "b", ProductID: "P", Interval: window, TotalCount: big.NewRat(100, 1), ScrapCount: big.NewRat(5, 1), Unit: "pcs"},
		},
		Policy: policy,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Good = (100-10) + (100-5) = 185. Subtracting the running scrap total (15) from each
	// contribution gave 90 + 85 = 175.
	if res.GoodPieces.Cmp(big.NewRat(185, 1)) != 0 {
		t.Fatalf("derived good count = %s, want 185", res.GoodPieces.RatString())
	}
}

func TestScrapAboveTotalIsFlaggedNotClamped(t *testing.T) {
	policy := DefaultKPIPolicy(uuid.New())
	t0 := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	window, _ := NewHalfOpenInterval(t0, t0.Add(time.Hour))
	res, err := CalculateKPIs(KPICalculationInput{
		Window: window,
		Timeline: TimelineResolutionResult{
			Window:    window,
			Segments:  []CanonicalFactSegment{{Interval: window, State: StateRunning}},
			Readiness: ReadinessDecision{Status: StatusReady},
		},
		CountContributions: []ProductCountContribution{
			{ContributionID: "a", ProductID: "P", Interval: window, TotalCount: big.NewRat(100, 1), ScrapCount: big.NewRat(150, 1), Unit: "pcs"},
		},
		Policy: policy,
	})
	if err != nil {
		t.Fatal(err)
	}
	// A data error must stay visible: the raw negative value is kept and the result is INVALID.
	if res.GoodPieces.Cmp(big.NewRat(-50, 1)) != 0 {
		t.Fatalf("scrap above total was clamped: good=%s, want -50 retained", res.GoodPieces.RatString())
	}
	if res.Readiness.Status != StatusInvalid || !kpiHasReason(res, ReasonInvalidQualityCounts) {
		t.Fatalf("readiness=%s reasons=%v, want INVALID with INVALID_QUALITY_COUNTS", res.Readiness.Status, res.Readiness.ReasonCodes)
	}
}

// TestRollupRatiosUseExactFractionalSeconds: child 1 runs 1 s; child 2 is 0.5 s of unplanned
// downtime. Aggregated availability is run/planned = 1 / 1.5 = 0.666667. The pre-fix rollup
// rebuilt the timeline from Num().Int64(), which turns 1/2 s into 1 s, and reported 0.5.
func TestRollupRatiosUseExactFractionalSeconds(t *testing.T) {
	policy := DefaultKPIPolicy(uuid.New())
	t0 := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	w1, _ := NewHalfOpenInterval(t0, t0.Add(time.Second))
	child1, err := CalculateKPIs(KPICalculationInput{
		Window: w1,
		Timeline: TimelineResolutionResult{
			Window:    w1,
			Segments:  []CanonicalFactSegment{{Interval: w1, State: StateRunning}},
			Readiness: ReadinessDecision{Status: StatusReady},
		},
		Policy: policy,
	})
	if err != nil {
		t.Fatal(err)
	}
	t1 := t0.Add(time.Second)
	w2, _ := NewHalfOpenInterval(t1, t1.Add(500*time.Millisecond))
	child2, err := CalculateKPIs(KPICalculationInput{
		Window: w2,
		Timeline: TimelineResolutionResult{
			Window:    w2,
			Segments:  []CanonicalFactSegment{{Interval: w2, State: StateChangeover}},
			Readiness: ReadinessDecision{Status: StatusReady},
		},
		Policy: policy,
	})
	if err != nil {
		t.Fatal(err)
	}
	agg, err := AggregateKPIResults([]KPICalculationResult{child1, child2}, policy)
	if err != nil {
		t.Fatal(err)
	}
	if agg.RunSeconds.Cmp(big.NewRat(1, 1)) != 0 || agg.PlannedProductionSeconds.Cmp(big.NewRat(3, 2)) != 0 {
		t.Fatalf("exact sums: run=%s planned=%s", agg.RunSeconds.RatString(), agg.PlannedProductionSeconds.RatString())
	}
	if agg.Availability.Formatted != "0.666667" {
		t.Fatalf("aggregated availability = %s, want 0.666667 (truncated seconds gave 0.5)", agg.Availability.Formatted)
	}
}
