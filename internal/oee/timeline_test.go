package oee

import (
	"errors"
	"math/big"
	"testing"
	"time"
)

func TestOrderSegmentsAndOverlap(t *testing.T) {
	t0 := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	t1 := time.Date(2026, 9, 30, 0, 10, 0, 0, time.UTC)
	t2 := time.Date(2026, 9, 30, 0, 20, 0, 0, time.UTC)
	t3 := time.Date(2026, 9, 30, 0, 30, 0, 0, time.UTC)

	iv1, _ := NewHalfOpenInterval(t0, t1)
	iv2, _ := NewHalfOpenInterval(t1, t2)
	iv3, _ := NewHalfOpenInterval(t2, t3)

	seg1 := TimelineSegment{Interval: iv1, SourceKey: "key1"}
	seg2 := TimelineSegment{Interval: iv2, SourceKey: "key2"}
	seg3 := TimelineSegment{Interval: iv3, SourceKey: "key3"}

	// Disjoint/adjacent segments
	unordered := []TimelineSegment{seg3, seg1, seg2}
	ordered := OrderSegments(unordered)

	if ordered[0].SourceKey != "key1" || ordered[1].SourceKey != "key2" || ordered[2].SourceKey != "key3" {
		t.Fatalf("unexpected order: %v", ordered)
	}

	if HasOverlap(ordered) {
		t.Errorf("adjacent segments must not overlap")
	}

	// Add an overlapping segment
	ivOverlap, _ := NewHalfOpenInterval(time.Date(2026, 9, 30, 0, 5, 0, 0, time.UTC), t2)
	segOverlap := TimelineSegment{Interval: ivOverlap, SourceKey: "key_overlap"}
	withOverlap := []TimelineSegment{seg1, segOverlap}

	if !HasOverlap(withOverlap) {
		t.Errorf("expected overlap detection")
	}
}

func TestResolveTimeline_PolicyPrecedenceAndGaps(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	t1 := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	window, _ := NewHalfOpenInterval(t0, t1)

	// Policy: PlannedBreak beats Running, Running beats Standby
	policy := TimelinePolicy{
		PrecedenceOrder: []TimelineState{
			StatePlannedBreak,
			StateRunning,
			StateStandby,
		},
		IANATimezone: "UTC",
		// The 8:45-9:00 gap is 25% of the window; this policy tolerates it.
		MinCoverage: big.NewRat(3, 4),
	}

	// 8:00 to 8:30 Running
	t0_30 := t0.Add(30 * time.Minute)
	ivRun, _ := NewHalfOpenInterval(t0, t0_30)
	evRun := OwnerFactEvent{
		EventID:       "ev-run",
		SourceKey:     "plc",
		State:         StateRunning,
		Interval:      ivRun,
		EventAt:       t0,
		RecordedAt:    t0,
		Revision:      1,
		PayloadDigest: "d1",
	}

	// 8:15 to 8:45 PlannedBreak (overlaps Running from 8:15 to 8:30)
	t0_15 := t0.Add(15 * time.Minute)
	t0_45 := t0.Add(45 * time.Minute)
	ivBreak, _ := NewHalfOpenInterval(t0_15, t0_45)
	evBreak := OwnerFactEvent{
		EventID:       "ev-break",
		SourceKey:     "calendar",
		State:         StatePlannedBreak,
		Interval:      ivBreak,
		EventAt:       t0_15,
		RecordedAt:    t0_15,
		Revision:      1,
		PayloadDigest: "d2",
	}

	// From 8:45 to 9:00: NO events (GAP)
	res, err := ResolveTimeline(window, []OwnerFactEvent{evRun, evBreak}, policy)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Readiness.Status != StatusReady {
		t.Fatalf("expected StatusReady, got %v", res.Readiness.Status)
	}

	// Check segments:
	// [08:00, 08:15): RUNNING (15 min)
	// [08:15, 08:45): PLANNED_BREAK (30 min, wins precedence over RUNNING in 08:15-08:30)
	// [08:45, 09:00): GAP (15 min)
	if len(res.Segments) != 3 {
		t.Fatalf("expected 3 merged segments, got %d: %+v", len(res.Segments), res.Segments)
	}

	if res.Segments[0].State != StateRunning || !res.Segments[0].Interval.Start.Equal(t0) || !res.Segments[0].Interval.End.Equal(t0_15) {
		t.Errorf("seg 0 mismatch: %+v", res.Segments[0])
	}
	if res.Segments[1].State != StatePlannedBreak || !res.Segments[1].Interval.Start.Equal(t0_15) || !res.Segments[1].Interval.End.Equal(t0_45) {
		t.Errorf("seg 1 mismatch: %+v", res.Segments[1])
	}
	if res.Segments[2].State != StateGap || !res.Segments[2].IsGap || !res.Segments[2].Interval.Start.Equal(t0_45) || !res.Segments[2].Interval.End.Equal(t1) {
		t.Errorf("seg 2 mismatch: %+v", res.Segments[2])
	}

	// Check exact duration ratio: 45 min covered / 60 min total = 3/4
	expectedCoverage := big.NewRat(45*60, 60*60)
	if res.CoverageRatio.Cmp(expectedCoverage) != 0 {
		t.Errorf("expected coverage ratio %s, got %s", expectedCoverage.String(), res.CoverageRatio.String())
	}
}

func TestResolveTimeline_UnresolvableConflictReturnsNotReady(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	t1 := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	window, _ := NewHalfOpenInterval(t0, t1)

	// Two overlapping events of UNRANKED / identical precedence
	policy := TimelinePolicy{
		PrecedenceOrder: []TimelineState{StatePlannedBreak}, // StateRunning not ranked
		IANATimezone:    "UTC",
	}

	ev1 := OwnerFactEvent{
		EventID:       "ev1",
		State:         StateRunning,
		Interval:      window,
		Revision:      1,
		PayloadDigest: "d1",
	}
	ev2 := OwnerFactEvent{
		EventID:       "ev2",
		State:         StateUnplannedDowntime,
		Interval:      window,
		Revision:      1,
		PayloadDigest: "d2",
	}

	res, err := ResolveTimeline(window, []OwnerFactEvent{ev1, ev2}, policy)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Readiness.Status != StatusNotReady {
		t.Fatalf("expected NOT_READY on unresolvable overlap, got %s", res.Readiness.Status)
	}
	if len(res.Readiness.ReasonCodes) == 0 || res.Readiness.ReasonCodes[0] != ReasonConflictingFact {
		t.Fatalf("expected ReasonConflictingFact, got %v", res.Readiness.ReasonCodes)
	}
}

func TestComputeMonotonicCounterDeltas(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	t1 := t0.Add(15 * time.Minute)
	t2 := t0.Add(30 * time.Minute)
	t3 := t0.Add(45 * time.Minute)

	// 1. Normal monotonic progress: 100 -> 150 -> 220
	r1 := CounterReading{ReadingID: "r1", RawValue: big.NewRat(100, 1), OccurredAt: t0}
	r2 := CounterReading{ReadingID: "r2", RawValue: big.NewRat(150, 1), OccurredAt: t1}
	r3 := CounterReading{ReadingID: "r3", RawValue: big.NewRat(220, 1), OccurredAt: t2}

	deltas, err := ComputeMonotonicCounterDeltas([]CounterReading{r1, r2, r3})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(deltas) != 2 {
		t.Fatalf("expected 2 deltas, got %d", len(deltas))
	}
	if deltas[0].DeltaUnits.Cmp(big.NewRat(50, 1)) != 0 || deltas[1].DeltaUnits.Cmp(big.NewRat(70, 1)) != 0 {
		t.Errorf("delta mismatch: %+v, %+v", deltas[0].DeltaUnits, deltas[1].DeltaUnits)
	}

	// 2. Counter drop WITHOUT evidence -> fails closed
	rDrop := CounterReading{ReadingID: "r4", RawValue: big.NewRat(50, 1), OccurredAt: t3}
	_, err = ComputeMonotonicCounterDeltas([]CounterReading{r1, r2, r3, rDrop})
	if !errors.Is(err, ErrCounterDropNoEvidence) {
		t.Fatalf("counter drop without evidence: got %v, want ErrCounterDropNoEvidence", err)
	}

	// 3. Counter drop WITH explicit reset evidence -> succeeds
	rReset := CounterReading{
		ReadingID:        "r4-reset",
		RawValue:         big.NewRat(50, 1),
		OccurredAt:       t3,
		HasResetEvidence: true,
	}
	deltasReset, err := ComputeMonotonicCounterDeltas([]CounterReading{r3, rReset})
	if err != nil {
		t.Fatalf("unexpected error on reset with evidence: %v", err)
	}
	if len(deltasReset) != 1 || deltasReset[0].DeltaUnits.Cmp(big.NewRat(50, 1)) != 0 {
		t.Errorf("expected reset delta to be 50, got %v", deltasReset[0].DeltaUnits)
	}

	// 4. Counter drop WITH rollover evidence (threshold = 1000): 900 -> 50
	// Delta = (1000 - 900) + 50 = 150
	rPreRoll := CounterReading{ReadingID: "r-pre", RawValue: big.NewRat(900, 1), OccurredAt: t0}
	rRoll := CounterReading{
		ReadingID:           "r-post",
		RawValue:            big.NewRat(50, 1),
		OccurredAt:          t1,
		HasRolloverEvidence: true,
		RolloverThreshold:   big.NewRat(1000, 1),
	}
	deltasRoll, err := ComputeMonotonicCounterDeltas([]CounterReading{rPreRoll, rRoll})
	if err != nil {
		t.Fatalf("unexpected error on rollover: %v", err)
	}
	if len(deltasRoll) != 1 || deltasRoll[0].DeltaUnits.Cmp(big.NewRat(150, 1)) != 0 {
		t.Errorf("expected rollover delta 150, got %v", deltasRoll[0].DeltaUnits)
	}
}

func TestResolveTimelineWithExclusions(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	t1 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	window, _ := NewHalfOpenInterval(t0, t1)

	// Base operational fact: RUNNING from 8:00 to 12:00
	factRun := OwnerFactEvent{
		EventID:       "run-1",
		SourceKey:     "plc",
		State:         StateRunning,
		Interval:      window,
		EventAt:       t0,
		RecordedAt:    t0,
		Revision:      1,
		PayloadDigest: "d-run",
	}

	// Versioned calendar exclusions
	// 1. Changeover: 08:30 - 09:00
	t0_30 := t0.Add(30 * time.Minute)
	t1_00 := t0.Add(60 * time.Minute)
	ivChangeover, _ := NewHalfOpenInterval(t0_30, t1_00)

	// 2. Planned break: 10:00 - 10:30
	t2_00 := t0.Add(120 * time.Minute)
	t2_30 := t0.Add(150 * time.Minute)
	ivBreak, _ := NewHalfOpenInterval(t2_00, t2_30)

	// 3. Shutdown exclusion: 11:30 - 12:00
	t3_30 := t0.Add(210 * time.Minute)
	ivShutdown, _ := NewHalfOpenInterval(t3_30, t1)

	exclusions := CalendarExclusionPolicy{
		PolicyRevision: 1,
		Changeovers: []CalendarExclusion{
			{ExclusionID: "chg-1", Interval: ivChangeover, Revision: 1, RecordedAt: t0},
		},
		PlannedBreaks: []CalendarExclusion{
			{ExclusionID: "brk-1", Interval: ivBreak, Revision: 1, RecordedAt: t0},
		},
		ShutdownExclusions: []CalendarExclusion{
			{ExclusionID: "sht-1", Interval: ivShutdown, Revision: 1, RecordedAt: t0},
		},
	}

	policy := TimelinePolicy{
		PrecedenceOrder: []TimelineState{
			StateShutdownExclusion,
			StatePlannedBreak,
			StateChangeover,
			StateRunning,
		},
		IANATimezone: "UTC",
	}

	res, err := ResolveTimelineWithExclusions(window, []OwnerFactEvent{factRun}, exclusions, policy)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Readiness.Status != StatusReady {
		t.Fatalf("expected StatusReady, got %s", res.Readiness.Status)
	}

	// Check segments:
	// 08:00 - 08:30: RUNNING
	// 08:30 - 09:00: CHANGEOVER
	// 09:00 - 10:00: RUNNING
	// 10:00 - 10:30: PLANNED_BREAK
	// 10:30 - 11:30: RUNNING
	// 11:30 - 12:00: SHUTDOWN_EXCLUSION
	if len(res.Segments) != 6 {
		t.Fatalf("expected 6 segments, got %d: %+v", len(res.Segments), res.Segments)
	}

	expectedStates := []TimelineState{
		StateRunning,
		StateChangeover,
		StateRunning,
		StatePlannedBreak,
		StateRunning,
		StateShutdownExclusion,
	}

	for i, expected := range expectedStates {
		if res.Segments[i].State != expected {
			t.Errorf("seg %d expected %s, got %s", i, expected, res.Segments[i].State)
		}
	}
}

func TestBuildNewCalculationRevision_ImmutableHistory(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	t1 := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	window, _ := NewHalfOpenInterval(t0, t1)

	policy := TimelinePolicy{
		PrecedenceOrder: []TimelineState{StateRunning},
		IANATimezone:    "UTC",
	}

	exclusions := CalendarExclusionPolicy{PolicyRevision: 1}

	ev1 := OwnerFactEvent{
		EventID:       "ev-1",
		SourceKey:     "plc",
		State:         StateRunning,
		Interval:      window,
		EventAt:       t0,
		RecordedAt:    t0,
		Revision:      1,
		PayloadDigest: "d1",
	}

	// 1. Initial calculation: Rev 1
	calcRev1, err := BuildNewCalculationRevision(nil, "calc-1", window, []OwnerFactEvent{ev1}, exclusions, policy, "digest-v1", t0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calcRev1.Revision != 1 {
		t.Fatalf("expected revision 1, got %d", calcRev1.Revision)
	}

	// 2. Late arriving event creates Rev 2
	evLate := OwnerFactEvent{
		EventID:       "ev-late",
		SourceKey:     "operator",
		State:         StateRunning,
		Interval:      window,
		EventAt:       t0,
		RecordedAt:    t0.Add(30 * time.Minute), // late data arrival
		Revision:      2,
		PayloadDigest: "d2",
	}

	calcRev2, err := BuildNewCalculationRevision(&calcRev1, "calc-1", window, []OwnerFactEvent{evLate}, exclusions, policy, "digest-v2", t1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calcRev2.Revision != 2 {
		t.Fatalf("expected revision 2, got %d", calcRev2.Revision)
	}

	// Prior calculation record remains unmodified
	if calcRev1.Revision != 1 || calcRev1.DatasetDigest != "digest-v1" {
		t.Errorf("prior revision was mutated: %+v", calcRev1)
	}
}
