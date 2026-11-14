package oee

import (
	"math/big"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestGoldenReferenceVector_ADR0005 verifies the exact industry standard numerical example
// specified in ADR 0005 Section 9 (8-hour shift, 480 min total, 30 min breaks, 450 min planned,
// 60 min downtime, 390 min run time, 1.0s ideal cycle time, 19,500 total, 19,200 conforming, 300 scrap).
func TestGoldenReferenceVector_ADR0005(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	tEnd := t0.Add(8 * time.Hour) // 480 minutes total calendar time
	window, _ := NewHalfOpenInterval(t0, tEnd)

	// Timeline segments:
	// 08:00 - 08:30: Run (30 min)
	// 08:30 - 08:45: Planned Break (15 min) -> Exclusion
	// 08:45 - 12:00: Run (195 min)
	// 12:00 - 12:40: Motor Breakdown Unplanned Downtime (40 min)
	// 12:40 - 13:00: Product Changeover Stop (20 min)
	// 13:00 - 13:15: Planned Break (15 min) -> Exclusion
	// 13:15 - 16:00: Run (165 min)
	// Total Run Time = 30 + 195 + 165 = 390 min (23,400s)
	// Total Downtime = 40 + 20 = 60 min (3,600s)
	// Total Exclusions = 15 + 15 = 30 min (1,800s)
	// Planned Production Time = 390 + 60 = 450 min (27,000s)
	t0_30 := t0.Add(30 * time.Minute)
	t0_45 := t0.Add(45 * time.Minute)
	t4_00 := t0.Add(4 * time.Hour)
	t4_40 := t4_00.Add(40 * time.Minute)
	t5_00 := t4_00.Add(60 * time.Minute)
	t5_15 := t5_00.Add(15 * time.Minute)

	iv1, _ := NewHalfOpenInterval(t0, t0_30)
	ivBrk1, _ := NewHalfOpenInterval(t0_30, t0_45)
	iv2, _ := NewHalfOpenInterval(t0_45, t4_00)
	ivDown, _ := NewHalfOpenInterval(t4_00, t4_40)
	ivChg, _ := NewHalfOpenInterval(t4_40, t5_00)
	ivBrk2, _ := NewHalfOpenInterval(t5_00, t5_15)
	iv3, _ := NewHalfOpenInterval(t5_15, tEnd)

	timeline := TimelineResolutionResult{
		Segments: []CanonicalFactSegment{
			{Interval: iv1, State: StateRunning, EventID: "e1"},
			{Interval: ivBrk1, State: StatePlannedBreak, EventID: "b1"},
			{Interval: iv2, State: StateRunning, EventID: "e2"},
			{Interval: ivDown, State: StateUnplannedDowntime, EventID: "d1"},
			{Interval: ivChg, State: StateChangeover, EventID: "c1"},
			{Interval: ivBrk2, State: StatePlannedBreak, EventID: "b2"},
			{Interval: iv3, State: StateRunning, EventID: "e3"},
		},
		Window: window,
		Readiness: ReadinessDecision{
			Status:      StatusReady,
			ReasonCodes: []ReasonCode{},
		},
	}

	policyID := uuid.MustParse("00000000-0000-0000-0000-000000000226")
	policy := DefaultKPIPolicy(policyID)
	policy.RoundingScale = 6
	policy.RoundingMode = RoundHalfEven

	contributions := []ProductCountContribution{
		{
			ContributionID:  "c1",
			ProductID:       "BOTTLE-500ML",
			Interval:        window,
			TotalCount:      big.NewRat(19500, 1),
			ConformingCount: big.NewRat(19200, 1),
			ScrapCount:      big.NewRat(300, 1),
			ReworkCount:     big.NewRat(0, 1),
			Unit:            "pcs",
		},
	}

	cycles := []IdealCycleRevision{
		{
			ProductID:      "BOTTLE-500ML",
			Revision:       1,
			SecondsPerUnit: big.NewRat(1, 1), Unit: "s/unit", // 1.0 s/bottle
			EffectiveWindow: window,
		},
	}

	res, err := CalculateKPIs(KPICalculationInput{
		Window:             window,
		Timeline:           timeline,
		CountContributions: contributions,
		IdealCycles:        cycles,
		Policy:             policy,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// 1. Intermediate Seconds Assertions
	// Total Calendar = 480 min = 28,800 s
	if res.TotalCalendarSeconds.Cmp(big.NewRat(28800, 1)) != 0 {
		t.Errorf("expected 28800s calendar, got %v", res.TotalCalendarSeconds)
	}
	// Planned Exclusions = 30 min = 1,800 s
	if res.PlannedExclusionSeconds.Cmp(big.NewRat(1800, 1)) != 0 {
		t.Errorf("expected 1800s exclusions, got %v", res.PlannedExclusionSeconds)
	}
	// Planned Production Time = 450 min = 27,000 s
	if res.PlannedProductionSeconds.Cmp(big.NewRat(27000, 1)) != 0 {
		t.Errorf("expected 27000s PPT, got %v", res.PlannedProductionSeconds)
	}
	// Operating Run Time = 390 min = 23,400 s
	if res.RunSeconds.Cmp(big.NewRat(23400, 1)) != 0 {
		t.Errorf("expected 23400s Run Time, got %v", res.RunSeconds)
	}
	// Stop Time = 60 min = 3,600 s
	if res.StopSeconds.Cmp(big.NewRat(3600, 1)) != 0 {
		t.Errorf("expected 3600s Stop Time, got %v", res.StopSeconds)
	}
	// Ideal Run Time = 19,500 * 1.0s = 19,500 s
	if res.IdealRunSeconds.Cmp(big.NewRat(19500, 1)) != 0 {
		t.Errorf("expected 19500s Ideal Run, got %v", res.IdealRunSeconds)
	}

	// 2. Exact Rational & Formatted Factor Assertions (matching ADR 0005 Sec. 9)
	// Availability = 23,400 / 27,000 = 13 / 15 = 0.866667 (86.6667%)
	expectedAvailRat := big.NewRat(13, 15)
	if res.Availability.Value.Cmp(expectedAvailRat) != 0 {
		t.Errorf("availability rat mismatch: expected %v, got %v", expectedAvailRat, res.Availability.Value)
	}
	if res.Availability.Formatted != "0.866667" {
		t.Errorf("expected availability 0.866667, got %s", res.Availability.Formatted)
	}
	if res.Availability.Percentage != "86.666667%" {
		t.Errorf("expected availability percentage 86.666667%%, got %s", res.Availability.Percentage)
	}

	// Performance = 19,500 / 23,400 = 5 / 6 = 0.833333 (83.3333%)
	expectedPerfRat := big.NewRat(5, 6)
	if res.Performance.Value.Cmp(expectedPerfRat) != 0 {
		t.Errorf("performance rat mismatch: expected %v, got %v", expectedPerfRat, res.Performance.Value)
	}
	if res.Performance.Formatted != "0.833333" {
		t.Errorf("expected performance 0.833333, got %s", res.Performance.Formatted)
	}

	// Quality = 19,200 / 19,500 = 64 / 65 = 0.984615 (98.4615%)
	expectedQualRat := big.NewRat(64, 65)
	if res.Quality.Value.Cmp(expectedQualRat) != 0 {
		t.Errorf("quality rat mismatch: expected %v, got %v", expectedQualRat, res.Quality.Value)
	}
	if res.Quality.Formatted != "0.984615" {
		t.Errorf("expected quality 0.984615, got %s", res.Quality.Formatted)
	}

	// OEE = 13/15 * 5/6 * 64/65 = 64 / 90 = 32 / 45 = 0.711111 (71.1111%)
	expectedOEERat := big.NewRat(32, 45)
	if res.OEE.Value.Cmp(expectedOEERat) != 0 {
		t.Errorf("OEE rat mismatch: expected %v, got %v", expectedOEERat, res.OEE.Value)
	}
	if res.OEE.Formatted != "0.711111" {
		t.Errorf("expected OEE 0.711111, got %s", res.OEE.Formatted)
	}

	// Loading = 27,000 / 28,800 = 450 / 480 = 15 / 16 = 0.937500
	expectedLoadRat := big.NewRat(15, 16)
	if res.Loading.Value.Cmp(expectedLoadRat) != 0 {
		t.Errorf("loading rat mismatch: expected %v, got %v", expectedLoadRat, res.Loading.Value)
	}
	if res.Loading.Formatted != "0.937500" {
		t.Errorf("expected loading 0.937500, got %s", res.Loading.Formatted)
	}

	// TEEP = Loading * OEE = 15/16 * 32/45 = 2 / 3 = 0.666667 (66.6667%)
	expectedTEEPRat := big.NewRat(2, 3)
	if res.TEEP.Value.Cmp(expectedTEEPRat) != 0 {
		t.Errorf("TEEP rat mismatch: expected %v, got %v", expectedTEEPRat, res.TEEP.Value)
	}
	if res.TEEP.Formatted != "0.666667" {
		t.Errorf("expected TEEP 0.666667, got %s", res.TEEP.Formatted)
	}

	// 3. Status and Provenance Metadata
	if res.Readiness.Status != StatusReady {
		t.Errorf("expected READY, got %s (%v)", res.Readiness.Status, res.Readiness.ReasonCodes)
	}
	if res.FormulaVersion != "2026.1-nakajima-iso22400" {
		t.Errorf("missing formula version: %s", res.FormulaVersion)
	}
	if res.CalculationDigest == "" {
		t.Errorf("missing calculation digest")
	}
}

// TestZeroDenominatorNeverNaNOrInf verifies that zero Planned Production Time, zero Run Time,
// zero Total Count, and zero Calendar Time NEVER produce NaN or Infinity, but return safe
// zero rational representations and deterministic reason codes.
func TestZeroDenominatorNeverNaNOrInf(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	tEnd := t0.Add(4 * time.Hour)
	window, _ := NewHalfOpenInterval(t0, tEnd)

	policy := DefaultKPIPolicy(uuid.New())

	// Scenario A: Zero Planned Production Time (Entire shift is planned exclusion / break)
	timelineNoSchedule := TimelineResolutionResult{
		Segments: []CanonicalFactSegment{
			{Interval: window, State: StatePlannedBreak, EventID: "b-full"},
		},
		Window: window,
		Readiness: ReadinessDecision{
			Status:      StatusReady,
			ReasonCodes: []ReasonCode{},
		},
	}

	resA, err := CalculateKPIs(KPICalculationInput{
		Window:             window,
		Timeline:           timelineNoSchedule,
		CountContributions: nil,
		IdealCycles:        nil,
		Policy:             policy,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Availability must be zero denominator flagged NOT_READY, without panic, NaN, or Inf
	if !resA.Availability.IsZeroDenom {
		t.Errorf("expected Availability zero denom")
	}
	// ADR 0005 Sec. 3: a zero denominator publishes no number, never a fabricated 0%.
	if resA.Availability.Value != nil || resA.Availability.Formatted != "" || resA.Availability.Percentage != "" {
		t.Errorf("zero denominator published a value: value=%v formatted=%q", resA.Availability.Value, resA.Availability.Formatted)
	}
	if resA.Readiness.Status != StatusNotReady {
		t.Errorf("expected NOT_READY on zero schedule, got %s", resA.Readiness.Status)
	}

	hasMissingData := false
	hasZeroDenom := false
	for _, code := range resA.Readiness.ReasonCodes {
		if code == ReasonMissingData {
			hasMissingData = true
		}
		if code == ReasonZeroDenominator {
			hasZeroDenom = true
		}
	}
	if !hasMissingData || !hasZeroDenom {
		t.Errorf("expected MISSING_DATA and ZERO_DENOMINATOR in reasons: %v", resA.Readiness.ReasonCodes)
	}

	// Scenario B: Run Time is 0 (Entire shift was Unplanned Downtime)
	timelineAllDown := TimelineResolutionResult{
		Segments: []CanonicalFactSegment{
			{Interval: window, State: StateUnplannedDowntime, EventID: "down-full"},
		},
		Window: window,
		Readiness: ReadinessDecision{
			Status:      StatusReady,
			ReasonCodes: []ReasonCode{},
		},
	}

	resB, err := CalculateKPIs(KPICalculationInput{
		Window:             window,
		Timeline:           timelineAllDown,
		CountContributions: nil,
		IdealCycles:        nil,
		Policy:             policy,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Run Time = 0 -> Performance denominator is 0
	if !resB.Performance.IsZeroDenom {
		t.Errorf("expected Performance zero denom when run time is 0")
	}
	if resB.Performance.Value != nil || resB.Performance.Formatted != "" {
		t.Errorf("zero denominator published a value: formatted=%q", resB.Performance.Formatted)
	}
	if resB.OEE.Value != nil || resB.TEEP.Value != nil {
		t.Errorf("composite of an unpublished factor must be unpublished: OEE=%v TEEP=%v", resB.OEE.Value, resB.TEEP.Value)
	}
	if resB.Availability.Value.Sign() != 0 {
		t.Errorf("expected Availability = 0 when run time is 0")
	}
}

// TestMultiProductAndChangingIdealCycles verifies mixed product lines and dynamic ideal cycle changes
// effective at specific interval boundaries.
func TestMultiProductAndChangingIdealCycles(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	tMid := t0.Add(2 * time.Hour)
	tEnd := t0.Add(4 * time.Hour)
	window, _ := NewHalfOpenInterval(t0, tEnd)

	iv1, _ := NewHalfOpenInterval(t0, tMid)
	iv2, _ := NewHalfOpenInterval(tMid, tEnd)

	timeline := TimelineResolutionResult{
		Segments: []CanonicalFactSegment{
			{Interval: window, State: StateRunning, EventID: "run-4h"},
		},
		Window:    window,
		Readiness: ReadinessDecision{Status: StatusReady},
	}

	// Product A runs 08:00 - 10:00 (7,200s run time).
	// Ideal cycle Rev 1: 2.0s per unit. Produces 3,000 units -> Ideal Run = 6,000s
	// Product B runs 10:00 - 12:00 (7,200s run time).
	// Ideal cycle Rev 1: 1.5s per unit. Produces 4,000 units -> Ideal Run = 6,000s
	// Total Ideal Run Time = 6,000 + 6,000 = 12,000s
	// Total Run Time = 14,400s (4 hours)
	// Performance = 12,000 / 14,400 = 5 / 6 = 0.833333
	contributions := []ProductCountContribution{
		{
			ContributionID:  "c-prodA",
			ProductID:       "SKU-A",
			Interval:        iv1,
			TotalCount:      big.NewRat(3000, 1),
			ConformingCount: big.NewRat(2950, 1),
			ScrapCount:      big.NewRat(50, 1),
			Unit:            "pcs",
		},
		{
			ContributionID:  "c-prodB",
			ProductID:       "SKU-B",
			Interval:        iv2,
			TotalCount:      big.NewRat(4000, 1),
			ConformingCount: big.NewRat(3900, 1),
			ScrapCount:      big.NewRat(100, 1),
			Unit:            "pcs",
		},
	}

	cycles := []IdealCycleRevision{
		{
			ProductID:      "SKU-A",
			Revision:       1,
			SecondsPerUnit: big.NewRat(2, 1), Unit: "s/unit",
			EffectiveWindow: iv1,
		},
		{
			ProductID:      "SKU-B",
			Revision:       1,
			SecondsPerUnit: big.NewRat(3, 2), Unit: "s/unit", // 1.5s
			EffectiveWindow: iv2,
		},
	}

	policy := DefaultKPIPolicy(uuid.New())

	res, err := CalculateKPIs(KPICalculationInput{
		Window:             window,
		Timeline:           timeline,
		CountContributions: contributions,
		IdealCycles:        cycles,
		Policy:             policy,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if res.IdealRunSeconds.Cmp(big.NewRat(12000, 1)) != 0 {
		t.Errorf("expected 12000s ideal run, got %v", res.IdealRunSeconds)
	}
	expectedPerf := big.NewRat(5, 6)
	if res.Performance.Value.Cmp(expectedPerf) != 0 {
		t.Errorf("expected performance 5/6, got %v", res.Performance.Value)
	}
	if res.Performance.Formatted != "0.833333" {
		t.Errorf("expected formatted performance 0.833333, got %s", res.Performance.Formatted)
	}

	// Total pieces = 7,000, Good pieces = 6,850
	// Quality = 6,850 / 7,000 = 137 / 140 = 0.978571
	expectedQual := big.NewRat(137, 140)
	if res.Quality.Value.Cmp(expectedQual) != 0 {
		t.Errorf("expected quality 137/140, got %v", res.Quality.Value)
	}
	if res.Quality.Formatted != "0.978571" {
		t.Errorf("expected formatted quality 0.978571, got %s", res.Quality.Formatted)
	}
}

// TestAggregation_MathematicalRollupNotAveraged verifies that parallel asset or shift aggregation
// calculates the exact ratio over the union of numerators and denominators and NEVER naively
// averages percentages.
func TestAggregation_MathematicalRollupNotAveraged(t *testing.T) {
	policy := DefaultKPIPolicy(uuid.New())

	t0 := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	w4h, _ := NewHalfOpenInterval(t0, t0.Add(4*time.Hour))
	w8h, _ := NewHalfOpenInterval(t0, t0.Add(8*time.Hour))

	// Asset 1 (Short Shift - 4 Hours, 14,400s):
	// Run Time = 14,400s (100% Availability)
	// Ideal Run = 14,400s (100% Performance)
	// Good = 1,000, Total = 1,000 (100% Quality)
	// Child OEE = 100%
	tl1 := TimelineResolutionResult{
		Segments:  []CanonicalFactSegment{{Interval: w4h, State: StateRunning}},
		Window:    w4h,
		Readiness: ReadinessDecision{Status: StatusReady},
	}
	res1, err := CalculateKPIs(KPICalculationInput{
		Window:   w4h,
		Timeline: tl1,
		CountContributions: []ProductCountContribution{
			{
				ContributionID:  "c1",
				ProductID:       "P1",
				Interval:        w4h,
				TotalCount:      big.NewRat(1000, 1),
				ConformingCount: big.NewRat(1000, 1),
				Unit:            "pcs",
			},
		},
		IdealCycles: []IdealCycleRevision{
			{ProductID: "P1", SecondsPerUnit: big.NewRat(144, 10), Unit: "s/unit", EffectiveWindow: w4h}, // 14.4s * 1000 = 14400s
		},
		Policy: policy,
	})

	if err != nil {
		t.Fatal(err)
	}
	if res1.OEE.Formatted != "1.000000" {
		t.Fatalf("expected child 1 OEE 1.000000, got %s", res1.OEE.Formatted)
	}

	// Asset 2 (Long Shift - 8 Hours, 28,800s):
	// Run Time = 14,400s, Downtime = 14,400s -> Availability = 50%
	// Ideal Run = 7,200s -> Performance = 50%
	// Good = 500, Total = 1000 -> Quality = 50%
	// Child OEE = 0.5 * 0.5 * 0.5 = 12.5% = 0.125000
	tHalf := t0.Add(4 * time.Hour)
	tl2 := TimelineResolutionResult{
		Segments: []CanonicalFactSegment{
			{Interval: HalfOpenInterval{Start: t0, End: tHalf}, State: StateRunning},
			{Interval: HalfOpenInterval{Start: tHalf, End: t0.Add(8 * time.Hour)}, State: StateUnplannedDowntime},
		},
		Window:    w8h,
		Readiness: ReadinessDecision{Status: StatusReady},
	}
	res2, err := CalculateKPIs(KPICalculationInput{
		Window:   w8h,
		Timeline: tl2,
		CountContributions: []ProductCountContribution{
			{
				ContributionID:  "c2",
				ProductID:       "P2",
				Interval:        w8h,
				TotalCount:      big.NewRat(1000, 1),
				ConformingCount: big.NewRat(500, 1),
				ScrapCount:      big.NewRat(500, 1),
				Unit:            "pcs",
			},
		},
		IdealCycles: []IdealCycleRevision{
			{ProductID: "P2", SecondsPerUnit: big.NewRat(72, 10), Unit: "s/unit", EffectiveWindow: w8h}, // 7.2s * 1000 = 7200s
		},
		Policy: policy,
	})

	if err != nil {
		t.Fatal(err)
	}
	if res2.OEE.Formatted != "0.125000" {
		t.Fatalf("expected child 2 OEE 0.125000, got %s", res2.OEE.Formatted)
	}

	// If naively averaged: (1.0 + 0.125) / 2 = 56.25% (0.562500). THIS IS WRONG.
	// Correct mathematical rollup:
	// Total Run Time = 14,400 + 14,400 = 28,800s
	// Total Planned Time = 14,400 + 28,800 = 43,200s
	// Aggregated Availability = 28,800 / 43,200 = 2/3 = 0.666667
	// Aggregated Ideal Run = 14,400 + 7,200 = 21,600s
	// Aggregated Performance = 21,600 / 28,800 = 3/4 = 0.750000
	// Aggregated Quality = 1,500 / 2,000 = 3/4 = 0.750000
	// Aggregated OEE = 2/3 * 3/4 * 3/4 = 6/16 = 3/8 = 0.375000 (37.5000%)
	agg, err := AggregateKPIResults([]KPICalculationResult{res1, res2}, policy)
	if err != nil {
		t.Fatalf("unexpected aggregation error: %v", err)
	}

	if agg.Availability.Formatted != "0.666667" {
		t.Errorf("expected agg availability 0.666667, got %s", agg.Availability.Formatted)
	}
	if agg.Performance.Formatted != "0.750000" {
		t.Errorf("expected agg performance 0.750000, got %s", agg.Performance.Formatted)
	}
	if agg.Quality.Formatted != "0.750000" {
		t.Errorf("expected agg quality 0.750000, got %s", agg.Quality.Formatted)
	}
	if agg.OEE.Formatted != "0.375000" {
		t.Errorf("expected true mathematical OEE 0.375000, got %s", agg.OEE.Formatted)
	}
}

// TestValuesAboveExpectedBoundsRetainEvidence verifies that performance overshoot (e.g. 120%)
// is NEVER silently clamped to 1.0, retains raw values, and flags LIMITED/reason codes.
func TestValuesAboveExpectedBoundsRetainEvidence(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	tEnd := t0.Add(1 * time.Hour) // 3,600s
	window, _ := NewHalfOpenInterval(t0, tEnd)

	tl := TimelineResolutionResult{
		Segments:  []CanonicalFactSegment{{Interval: window, State: StateRunning}},
		Window:    window,
		Readiness: ReadinessDecision{Status: StatusReady},
	}

	// In 3,600s run time, machine produced 4,000 pieces with 1.0s ideal cycle time.
	// Ideal Run Time = 4,000s > Run Time = 3,600s
	// Performance = 4000 / 3600 = 10 / 9 = 1.111111 (111.1111% > 100%)
	policy := DefaultKPIPolicy(uuid.New())
	policy.PerformanceCapRule = CapAllowAndRetain

	res, err := CalculateKPIs(KPICalculationInput{
		Window:   window,
		Timeline: tl,
		CountContributions: []ProductCountContribution{
			{
				ContributionID:  "c1",
				ProductID:       "P1",
				Interval:        window,
				TotalCount:      big.NewRat(4000, 1),
				ConformingCount: big.NewRat(4000, 1),
				Unit:            "pcs",
			},
		},
		IdealCycles: []IdealCycleRevision{
			{ProductID: "P1", SecondsPerUnit: big.NewRat(1, 1), Unit: "s/unit", EffectiveWindow: window},
		},
		Policy: policy,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Performance MUST NOT be clamped to 1.000000
	if res.Performance.Formatted != "1.111111" {
		t.Errorf("expected unclamped performance 1.111111, got %s", res.Performance.Formatted)
	}
	if !res.Performance.IsOvershoot {
		t.Errorf("expected performance overshoot flag")
	}

	// Must be marked LIMITED with stable reason code
	if res.Readiness.Status != StatusLimited {
		t.Errorf("expected StatusLimited on performance overshoot, got %s", res.Readiness.Status)
	}

	hasOvershootReason := false
	for _, code := range res.Readiness.ReasonCodes {
		if code == ReasonPerformanceOvershoot {
			hasOvershootReason = true
		}
	}
	if !hasOvershootReason {
		t.Errorf("expected ReasonPerformanceOvershoot, got %v", res.Readiness.ReasonCodes)
	}
}

// TestDecimalRoundingInvariance verifies exact ties-to-even, half-up, floor, and ceiling
// modes across diverse rationals and scales, invariant to locale and timezone.
func TestDecimalRoundingInvariance(t *testing.T) {
	tests := []struct {
		val      *big.Rat
		scale    int
		mode     RoundingMode
		expected string
	}{
		// Banker's Rounding (HALF_EVEN): ties round to nearest even digit
		{big.NewRat(1, 2), 0, RoundHalfEven, "0"},    // 0.5 -> 0 (even)
		{big.NewRat(3, 2), 0, RoundHalfEven, "2"},    // 1.5 -> 2 (even)
		{big.NewRat(5, 2), 0, RoundHalfEven, "2"},    // 2.5 -> 2 (even)
		{big.NewRat(7, 2), 0, RoundHalfEven, "4"},    // 3.5 -> 4 (even)
		{big.NewRat(1, 8), 2, RoundHalfEven, "0.12"}, // 0.125 -> 0.12 (even)
		{big.NewRat(3, 8), 2, RoundHalfEven, "0.38"}, // 0.375 -> 0.38 (even)

		// HALF_UP: ties round away from zero
		{big.NewRat(1, 2), 0, RoundHalfUp, "1"},
		{big.NewRat(3, 2), 0, RoundHalfUp, "2"},
		{big.NewRat(1, 8), 2, RoundHalfUp, "0.13"}, // 0.125 -> 0.13

		// FLOOR: towards -infinity
		{big.NewRat(7, 4), 1, RoundFloor, "1.7"},   // 1.75 -> 1.7
		{big.NewRat(-7, 4), 1, RoundFloor, "-1.8"}, // -1.75 -> -1.8

		// CEILING: towards +infinity
		{big.NewRat(7, 4), 1, RoundCeiling, "1.8"},   // 1.75 -> 1.8
		{big.NewRat(-7, 4), 1, RoundCeiling, "-1.7"}, // -1.75 -> -1.7

		// Exact rationals at scale 6
		{big.NewRat(13, 15), 6, RoundHalfEven, "0.866667"},
		{big.NewRat(5, 6), 6, RoundHalfEven, "0.833333"},
		{big.NewRat(64, 65), 6, RoundHalfEven, "0.984615"},
		{big.NewRat(32, 45), 6, RoundHalfEven, "0.711111"},
		{big.NewRat(15, 16), 6, RoundHalfEven, "0.937500"},
		{big.NewRat(2, 3), 6, RoundHalfEven, "0.666667"},
	}

	for _, tc := range tests {
		got, err := FormatDecimal(tc.val, tc.scale, tc.mode)
		if err != nil {
			t.Errorf("error formatting %v: %v", tc.val, err)
			continue
		}
		if got != tc.expected {
			t.Errorf("val %v scale %d mode %s: expected %s, got %s", tc.val, tc.scale, tc.mode, tc.expected, got)
		}
	}
}
