package oee

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"math/rand"
	"testing"
	"time"
)

// WIS-226: DST, cross-midnight, exact boundary and leap-day golden cases for shift projection.
func TestProjectLocalShiftToUTC_Golden(t *testing.T) {
	utc := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	cases := []struct {
		name, zone, date, start, end string
		wantStart, wantEnd           string
		wantDuration                 time.Duration
	}{
		{"spring forward day shortens shift", "America/New_York", "2026-03-08", "00:00:00", "08:00:00",
			"2026-03-08T05:00:00Z", "2026-03-08T12:00:00Z", 7 * time.Hour},
		{"fall back day lengthens shift", "America/New_York", "2026-11-01", "00:00:00", "08:00:00",
			"2026-11-01T04:00:00Z", "2026-11-01T13:00:00Z", 9 * time.Hour},
		{"fold start picks first occurrence", "America/New_York", "2026-11-01", "01:30", "03:00",
			"2026-11-01T05:30:00Z", "2026-11-01T08:00:00Z", 150 * time.Minute},
		{"cross-midnight IST", "Asia/Kolkata", "2026-10-01", "22:00:00", "06:00:00",
			"2026-10-01T16:30:00Z", "2026-10-02T00:30:00Z", 8 * time.Hour},
		{"cross-midnight over spring forward", "Europe/Berlin", "2026-03-28", "22:00", "06:00",
			"2026-03-28T21:00:00Z", "2026-03-29T04:00:00Z", 7 * time.Hour},
		{"leap day shift", "Asia/Kolkata", "2028-02-29", "22:00", "06:00",
			"2028-02-29T16:30:00Z", "2028-03-01T00:30:00Z", 8 * time.Hour},
		{"exact midnight boundary, equal clocks is 24h", "UTC", "2028-02-28", "00:00", "00:00",
			"2028-02-28T00:00:00Z", "2028-02-29T00:00:00Z", 24 * time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			iv, err := ProjectLocalShiftToUTC(tc.zone, tc.date, tc.start, tc.end)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !iv.Start.Equal(utc(tc.wantStart)) || !iv.End.Equal(utc(tc.wantEnd)) {
				t.Fatalf("got [%s, %s), want [%s, %s)", iv.Start, iv.End, tc.wantStart, tc.wantEnd)
			}
			if iv.Duration() != tc.wantDuration {
				t.Fatalf("duration %s, want %s", iv.Duration(), tc.wantDuration)
			}
			if iv.Start.Location() != time.UTC || iv.End.Location() != time.UTC {
				t.Fatalf("interval is not UTC")
			}
		})
	}
}

func TestProjectLocalShiftToUTC_FailsClosed(t *testing.T) {
	cases := []struct {
		name, zone, date, start, end string
		want                         error
	}{
		{"start in spring-forward gap", "America/New_York", "2026-03-08", "02:30", "06:00", ErrLocalTimeInDSTGap},
		{"end in spring-forward gap", "America/New_York", "2026-03-07", "22:00", "02:15", ErrLocalTimeInDSTGap},
		{"no leap day in 2026", "UTC", "2026-02-29", "06:00", "14:00", ErrInvalidLocalTime},
		{"hour out of range", "UTC", "2026-10-01", "25:00", "06:00", ErrInvalidLocalTime},
		{"minute out of range", "UTC", "2026-10-01", "06:61", "07:00", ErrInvalidLocalTime},
		{"unknown zone", "Mars/Olympus", "2026-10-01", "06:00", "14:00", ErrInvalidTimezone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ProjectLocalShiftToUTC(tc.zone, tc.date, tc.start, tc.end)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

func TestResolveTimeline_LeapDayAndExactBoundaries(t *testing.T) {
	start := time.Date(2028, 2, 28, 0, 0, 0, 0, time.UTC)
	leap := time.Date(2028, 2, 29, 0, 0, 0, 0, time.UTC)
	end := time.Date(2028, 3, 1, 0, 0, 0, 0, time.UTC)
	window, _ := NewHalfOpenInterval(start, end)
	a, _ := NewHalfOpenInterval(start, leap)
	b, _ := NewHalfOpenInterval(leap, end)
	policy := TimelinePolicy{PrecedenceOrder: []TimelineState{StateRunning, StateUnplannedDowntime}}

	res, err := ResolveTimeline(window, []OwnerFactEvent{
		{EventID: "b", State: StateUnplannedDowntime, Interval: b, PayloadDigest: "b", Revision: 1},
		{EventID: "a", State: StateRunning, Interval: a, PayloadDigest: "a", Revision: 1},
	}, policy)
	if err != nil {
		t.Fatal(err)
	}
	if res.Readiness.Status != StatusReady {
		t.Fatalf("readiness %v", res.Readiness)
	}
	if len(res.Segments) != 2 || !res.Segments[0].Interval.End.Equal(leap) || !res.Segments[1].Interval.Start.Equal(leap) {
		t.Fatalf("touching half-open intervals must meet exactly at the leap day boundary: %+v", res.Segments)
	}
	if res.CoveredSeconds.Cmp(big.NewRat(48*3600, 1)) != 0 || res.GapSeconds.Sign() != 0 {
		t.Fatalf("covered %s gap %s, want 172800 and 0", res.CoveredSeconds, res.GapSeconds)
	}
}

func TestResolveTimeline_LimitsBeforeMaterialization(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	window, _ := NewHalfOpenInterval(t0, t0.Add(time.Hour))
	policy := TimelinePolicy{PrecedenceOrder: []TimelineState{StateRunning}, Limits: TimelineLimits{MaxFacts: 2, MaxWindow: time.Hour}}
	fact := func(id string) OwnerFactEvent {
		return OwnerFactEvent{EventID: id, State: StateRunning, Interval: window, PayloadDigest: id, Revision: 1}
	}

	if _, err := ResolveTimeline(window, []OwnerFactEvent{fact("a"), fact("b"), fact("c")}, policy); !errors.Is(err, ErrTimelineLimit) {
		t.Fatalf("fact limit: got %v", err)
	}
	// Exclusions count toward the limit before they are merged into the fact list.
	ex := CalendarExclusionPolicy{PlannedBreaks: []CalendarExclusion{{ExclusionID: "x1", Interval: window}, {ExclusionID: "x2", Interval: window}}}
	if _, err := ResolveTimelineWithExclusions(window, []OwnerFactEvent{fact("a")}, ex, policy); !errors.Is(err, ErrTimelineLimit) {
		t.Fatalf("exclusion limit: got %v", err)
	}
	long, _ := NewHalfOpenInterval(t0, t0.Add(2*time.Hour))
	if _, err := ResolveTimeline(long, nil, policy); !errors.Is(err, ErrTimelineLimit) {
		t.Fatalf("window limit: got %v", err)
	}
	huge, _ := NewHalfOpenInterval(t0, t0.Add(MaxTimelineWindow+time.Second))
	if _, err := ResolveTimeline(huge, nil, TimelinePolicy{}); !errors.Is(err, ErrTimelineLimit) {
		t.Fatalf("hard window ceiling: got %v", err)
	}
	raise := TimelinePolicy{Limits: TimelineLimits{MaxFacts: MaxTimelineFacts + 1}}
	if _, err := ResolveTimeline(window, nil, raise); !errors.Is(err, ErrInvalidPolicy) {
		t.Fatalf("a policy must not raise the ceiling: got %v", err)
	}
	if _, err := ResolveTimeline(HalfOpenInterval{Start: t0, End: t0}, nil, TimelinePolicy{}); !errors.Is(err, ErrInvalidInterval) {
		t.Fatalf("empty window: got %v", err)
	}
	readings := make([]CounterReading, MaxCounterReadings+1)
	if _, err := ComputeMonotonicCounterDeltas(readings); !errors.Is(err, ErrTimelineLimit) {
		t.Fatalf("counter limit: got %v", err)
	}
}

func TestResolveTimeline_StableFailClosedOutcomes(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	window, _ := NewHalfOpenInterval(t0, t0.Add(time.Hour))
	policy := TimelinePolicy{PrecedenceOrder: []TimelineState{StateRunning}}

	// Malformed facts are a contract error, distinct from a conflict.
	for name, f := range map[string]OwnerFactEvent{
		"empty id":          {State: StateRunning, Interval: window},
		"zero interval":     {EventID: "z", State: StateRunning},
		"reversed interval": {EventID: "r", State: StateRunning, Interval: HalfOpenInterval{Start: window.End, End: window.Start}},
	} {
		if _, err := ResolveTimeline(window, []OwnerFactEvent{f}, policy); !errors.Is(err, ErrMalformedFact) {
			t.Errorf("%s: got %v, want ErrMalformedFact", name, err)
		}
	}

	// A conflicting duplicate is NOT_READY / CONFLICTING_FACT.
	res, err := ResolveTimeline(window, []OwnerFactEvent{
		{EventID: "e", State: StateRunning, Interval: window, PayloadDigest: "1", Revision: 1},
		{EventID: "e", State: StateRunning, Interval: window, PayloadDigest: "2", Revision: 1},
	}, policy)
	if err != nil || res.Readiness.Status != StatusNotReady || res.Readiness.ReasonCodes[0] != ReasonConflictingFact {
		t.Fatalf("conflicting duplicate: %v %+v", err, res.Readiness)
	}

	// No facts at all is missing data, never a zero-coverage READY.
	res, err = ResolveTimeline(window, nil, policy)
	if err != nil || res.Readiness.Status != StatusNotReady || res.Readiness.ReasonCodes[0] != ReasonMissingData {
		t.Fatalf("empty window: %v %+v", err, res.Readiness)
	}
	if len(res.Segments) != 1 || !res.Segments[0].IsGap || res.CoverageRatio.Sign() != 0 {
		t.Fatalf("gap must stay visible: %+v", res.Segments)
	}
}

func TestResolveTimeline_EqualPrecedenceIdenticalFactsPickLowestEventID(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	window, _ := NewHalfOpenInterval(t0, t0.Add(time.Hour))
	early, _ := NewHalfOpenInterval(t0, t0.Add(40*time.Minute))
	late, _ := NewHalfOpenInterval(t0.Add(20*time.Minute), t0.Add(time.Hour))
	policy := TimelinePolicy{PrecedenceOrder: []TimelineState{StateRunning}}

	res, err := ResolveTimeline(window, []OwnerFactEvent{
		{EventID: "z-early", SourceKey: "plc", State: StateRunning, Interval: early, PayloadDigest: "same", Revision: 1},
		{EventID: "a-late", SourceKey: "mes", State: StateRunning, Interval: late, PayloadDigest: "same", Revision: 1},
	}, policy)
	if err != nil || res.Readiness.Status != StatusReady {
		t.Fatalf("%v %+v", err, res.Readiness)
	}
	// [0,20) only z-early; [20,60) both, a-late wins on event ID.
	if len(res.Segments) != 2 || res.Segments[0].EventID != "z-early" || res.Segments[1].EventID != "a-late" ||
		!res.Segments[1].Interval.Start.Equal(late.Start) {
		t.Fatalf("segments %+v", res.Segments)
	}
}

func TestBuildNewCalculationRevision_PureAndLineageBound(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	window, _ := NewHalfOpenInterval(t0, t0.Add(time.Hour))
	policy := TimelinePolicy{PrecedenceOrder: []TimelineState{StateRunning}}
	facts := []OwnerFactEvent{{EventID: "e", State: StateRunning, Interval: window, PayloadDigest: "d", Revision: 1}}

	if _, err := BuildNewCalculationRevision(nil, "c", window, facts, CalendarExclusionPolicy{}, policy, "ds", time.Time{}); !errors.Is(err, ErrMissingGeneratedAt) {
		t.Fatalf("zero generatedAt: got %v", err)
	}
	gen := time.Date(2026, 10, 1, 14, 0, 0, 0, time.FixedZone("IST", 19800))
	r1, err := BuildNewCalculationRevision(nil, "c", window, facts, CalendarExclusionPolicy{}, policy, "ds", gen)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := BuildNewCalculationRevision(nil, "c", window, facts, CalendarExclusionPolicy{}, policy, "ds", gen)
	b1, _ := json.Marshal(r1)
	b2, _ := json.Marshal(again)
	if string(b1) != string(b2) {
		t.Fatalf("same inputs must give byte-identical output:\n%s\n%s", b1, b2)
	}
	if !r1.GeneratedAtUTC.Equal(gen) || r1.GeneratedAtUTC.Location() != time.UTC {
		t.Fatalf("generatedAt must be the caller's instant in UTC, got %s", r1.GeneratedAtUTC)
	}

	other, _ := NewHalfOpenInterval(t0, t0.Add(2*time.Hour))
	if _, err := BuildNewCalculationRevision(&r1, "c", other, facts, CalendarExclusionPolicy{}, policy, "ds", gen); !errors.Is(err, ErrRevisionLineage) {
		t.Fatalf("window mismatch: got %v", err)
	}
	if _, err := BuildNewCalculationRevision(&r1, "other", window, facts, CalendarExclusionPolicy{}, policy, "ds", gen); !errors.Is(err, ErrRevisionLineage) {
		t.Fatalf("calculation mismatch: got %v", err)
	}
}

func TestComputeMonotonicCounterDeltas_DuplicatesAndAmbiguity(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	r := func(id string, v int64, at time.Duration) CounterReading {
		return CounterReading{ReadingID: id, CounterCode: "good", RawValue: big.NewRat(v, 1), OccurredAt: t0.Add(at)}
	}

	// Exact duplicate readings and out-of-order delivery give the same deltas.
	in := []CounterReading{r("c", 170, 2*time.Minute), r("a", 100, 0), r("b", 150, time.Minute), r("a", 100, 0)}
	deltas, err := ComputeMonotonicCounterDeltas(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(deltas) != 2 || deltas[0].DeltaUnits.Cmp(big.NewRat(50, 1)) != 0 || deltas[1].DeltaUnits.Cmp(big.NewRat(20, 1)) != 0 {
		t.Fatalf("deltas %+v", deltas)
	}

	// Same reading ID, different value: conflicting duplicate.
	if _, err := ComputeMonotonicCounterDeltas([]CounterReading{r("a", 100, 0), r("a", 101, 0)}); !errors.Is(err, ErrConflictingDuplicate) {
		t.Fatalf("conflicting reading id: got %v", err)
	}
	// Two readings at the same source time that disagree are never silently skipped.
	if _, err := ComputeMonotonicCounterDeltas([]CounterReading{r("a", 100, 0), r("b", 120, 0), r("c", 150, time.Minute)}); !errors.Is(err, ErrConflictingCounter) {
		t.Fatalf("same-time disagreement: got %v", err)
	}
	// Agreeing readings at the same source time collapse to one.
	deltas, err = ComputeMonotonicCounterDeltas([]CounterReading{r("a", 100, 0), r("b", 100, 0), r("c", 150, time.Minute)})
	if err != nil || len(deltas) != 1 || deltas[0].EvidenceID != "c" {
		t.Fatalf("same-time agreement: %v %+v", err, deltas)
	}
	// Mixed counters fail closed.
	mixed := r("b", 150, time.Minute)
	mixed.CounterCode = "scrap"
	if _, err := ComputeMonotonicCounterDeltas([]CounterReading{r("a", 100, 0), mixed}); !errors.Is(err, ErrMixedCounterCodes) {
		t.Fatalf("mixed counters: got %v", err)
	}
	// Rollover evidence inconsistent with the threshold fails closed.
	roll := r("b", 50, time.Minute)
	roll.HasRolloverEvidence, roll.RolloverThreshold = true, big.NewRat(800, 1)
	if _, err := ComputeMonotonicCounterDeltas([]CounterReading{r("a", 900, 0), roll}); !errors.Is(err, ErrInvalidRollover) {
		t.Fatalf("previous above threshold: got %v", err)
	}
	// A negative reading fails closed even when it is the only one.
	if _, err := ComputeMonotonicCounterDeltas([]CounterReading{r("a", -1, 0)}); !errors.Is(err, ErrNegativeCounter) {
		t.Fatalf("single negative: got %v", err)
	}
}

// Property tests: seeded, so a failure reproduces from the printed seed.

var propertyStates = []TimelineState{StateShutdownExclusion, StatePlannedBreak, StateChangeover, StateUnplannedDowntime, StateRunning}

func randomFacts(rng *rand.Rand, window HalfOpenInterval, n int) []OwnerFactEvent {
	span := int64(window.Duration() / time.Second)
	facts := make([]OwnerFactEvent, 0, n)
	for i := 0; i < n; i++ {
		// Start slightly before and end slightly after the window to exercise clipping.
		s := rng.Int63n(span+600) - 300
		d := rng.Int63n(span/4) + 1
		start := window.Start.Add(time.Duration(s) * time.Second)
		state := propertyStates[rng.Intn(len(propertyStates))]
		// Equal-state overlaps sometimes share a digest (resolvable) and sometimes do not (conflict).
		digest := fmt.Sprintf("%s-%d", state, rng.Intn(2))
		facts = append(facts, OwnerFactEvent{
			EventID:       fmt.Sprintf("ev-%03d", i),
			SourceKey:     fmt.Sprintf("src-%d", rng.Intn(3)),
			State:         state,
			Interval:      HalfOpenInterval{Start: start, End: start.Add(time.Duration(d) * time.Second)},
			Revision:      int64(rng.Intn(3) + 1),
			PayloadDigest: digest,
		})
	}
	return facts
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestResolveTimeline_Properties(t *testing.T) {
	t0 := time.Date(2028, 2, 28, 22, 0, 0, 0, time.UTC)
	window, _ := NewHalfOpenInterval(t0, t0.Add(8*time.Hour))
	policy := TimelinePolicy{PrecedenceOrder: propertyStates, MinCoverage: new(big.Rat)}
	ready := 0

	for seed := int64(1); seed <= 300; seed++ {
		rng := rand.New(rand.NewSource(seed))
		facts := randomFacts(rng, window, rng.Intn(12)+1)

		base, err := ResolveTimeline(window, facts, policy)
		if err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		want := mustJSON(t, base)

		// Ordering: any permutation gives byte-identical output.
		shuffled := append([]OwnerFactEvent(nil), facts...)
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		got, _ := ResolveTimeline(window, shuffled, policy)
		if g := mustJSON(t, got); g != want {
			t.Fatalf("seed %d: permutation changed output\nwant %s\ngot  %s", seed, want, g)
		}

		// Duplicate idempotency: replaying exact duplicates changes nothing.
		dup, _ := ResolveTimeline(window, append(append([]OwnerFactEvent(nil), facts...), facts...), policy)
		if g := mustJSON(t, dup); g != want {
			t.Fatalf("seed %d: exact duplicates changed output", seed)
		}

		if base.Readiness.Status != StatusReady {
			continue
		}
		ready++

		// Conservation: segments tile the window exactly, in order, without overlap,
		// and covered + gap equals the window duration.
		cursor := window.Start
		covered, gap := new(big.Rat), new(big.Rat)
		for i, seg := range base.Segments {
			if !seg.Interval.Start.Equal(cursor) || !seg.Interval.Start.Before(seg.Interval.End) {
				t.Fatalf("seed %d: segment %d does not continue the tiling at %s: %+v", seed, i, cursor, seg)
			}
			if i > 0 {
				prev := base.Segments[i-1]
				if prev.State == seg.State && prev.EventID == seg.EventID && prev.SourceKey == seg.SourceKey && prev.IsGap == seg.IsGap {
					t.Fatalf("seed %d: adjacent identical segments were not merged", seed)
				}
			}
			if seg.IsGap {
				gap.Add(gap, seg.Interval.DurationSeconds())
			} else {
				covered.Add(covered, seg.Interval.DurationSeconds())
			}
			cursor = seg.Interval.End
		}
		if !cursor.Equal(window.End) {
			t.Fatalf("seed %d: tiling ends at %s, want %s", seed, cursor, window.End)
		}
		if covered.Cmp(base.CoveredSeconds) != 0 || gap.Cmp(base.GapSeconds) != 0 {
			t.Fatalf("seed %d: segment totals %s/%s differ from reported %s/%s", seed, covered, gap, base.CoveredSeconds, base.GapSeconds)
		}
		if total := new(big.Rat).Add(covered, gap); total.Cmp(window.DurationSeconds()) != 0 {
			t.Fatalf("seed %d: covered+gap %s != window %s", seed, total, window.DurationSeconds())
		}

		// Every non-gap segment is backed by a fact that covers it and has the winning precedence.
		for _, seg := range base.Segments {
			if seg.IsGap {
				continue
			}
			best := len(propertyStates)
			for _, f := range facts {
				if sect, ok := window.Intersection(f.Interval); ok && !sect.Start.After(seg.Interval.Start) && !sect.End.Before(seg.Interval.End) {
					if r := policy.PrecedenceRank(f.State); r < best {
						best = r
					}
				}
			}
			if best == len(propertyStates) || propertyStates[best] != seg.State {
				t.Fatalf("seed %d: segment %+v is not the highest-precedence covering fact", seed, seg)
			}
		}
	}
	if ready < 50 {
		t.Fatalf("only %d READY cases; the generator no longer exercises conservation", ready)
	}
}

func TestComputeMonotonicCounterDeltas_Properties(t *testing.T) {
	t0 := time.Date(2028, 2, 29, 0, 0, 0, 0, time.UTC)
	for seed := int64(1); seed <= 200; seed++ {
		rng := rand.New(rand.NewSource(seed))
		n := rng.Intn(20) + 2
		value := int64(rng.Intn(1000))
		readings := make([]CounterReading, 0, n)
		for i := 0; i < n; i++ {
			readings = append(readings, CounterReading{
				ReadingID:   fmt.Sprintf("r-%03d", i),
				CounterCode: "good",
				RawValue:    big.NewRat(value, 1),
				OccurredAt:  t0.Add(time.Duration(i) * time.Minute),
			})
			value += int64(rng.Intn(50))
		}
		first, last := readings[0].RawValue, readings[n-1].RawValue

		shuffled := append([]CounterReading(nil), readings...)
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		shuffled = append(shuffled, readings[rng.Intn(n)]) // an exact replay

		a, err := ComputeMonotonicCounterDeltas(readings)
		if err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		b, err := ComputeMonotonicCounterDeltas(shuffled)
		if err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		if mustJSON(t, a) != mustJSON(t, b) {
			t.Fatalf("seed %d: order or replay changed deltas", seed)
		}
		sum := new(big.Rat)
		for i, d := range a {
			if d.DeltaUnits.Sign() < 0 {
				t.Fatalf("seed %d: negative delta", seed)
			}
			if i > 0 && !a[i-1].Interval.End.Equal(d.Interval.Start) {
				t.Fatalf("seed %d: delta intervals are not contiguous", seed)
			}
			sum.Add(sum, d.DeltaUnits)
		}
		if want := new(big.Rat).Sub(last, first); sum.Cmp(want) != 0 {
			t.Fatalf("seed %d: deltas sum to %s, want %s", seed, sum, want)
		}
	}
}

func TestResolveTimeline_CoverageThreshold(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	window, _ := NewHalfOpenInterval(t0, t0.Add(10*time.Hour))
	run := func(d time.Duration) []OwnerFactEvent {
		iv, _ := NewHalfOpenInterval(t0, t0.Add(d))
		return []OwnerFactEvent{{EventID: "r", State: StateRunning, Interval: iv, PayloadDigest: "r", Revision: 1}}
	}
	half := big.NewRat(1, 2)
	cases := []struct {
		name    string
		covered time.Duration
		min     *big.Rat
		want    Status
	}{
		{"one minute of ten hours, default policy", time.Minute, nil, StatusNotReady},
		{"any gap, default policy", 10*time.Hour - time.Second, nil, StatusNotReady},
		{"full coverage, default policy", 10 * time.Hour, nil, StatusReady},
		{"just below the declared minimum", 5*time.Hour - time.Second, half, StatusNotReady},
		{"exactly the declared minimum", 5 * time.Hour, half, StatusReady},
		{"above the declared minimum", 8 * time.Hour, half, StatusReady},
		{"zero minimum still needs some data", 0, new(big.Rat), StatusNotReady},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var facts []OwnerFactEvent
			if tc.covered > 0 {
				facts = run(tc.covered)
			}
			res, err := ResolveTimeline(window, facts, TimelinePolicy{PrecedenceOrder: []TimelineState{StateRunning}, MinCoverage: tc.min})
			if err != nil {
				t.Fatal(err)
			}
			if res.Readiness.Status != tc.want {
				t.Fatalf("status %s, want %s (coverage %s)", res.Readiness.Status, tc.want, res.CoverageRatio)
			}
			if tc.want == StatusNotReady && res.Readiness.ReasonCodes[0] != ReasonMissingData {
				t.Fatalf("reasons %v, want MISSING_DATA", res.Readiness.ReasonCodes)
			}
		})
	}
	for _, bad := range []*big.Rat{big.NewRat(-1, 10), big.NewRat(11, 10)} {
		if _, err := ResolveTimeline(window, nil, TimelinePolicy{MinCoverage: bad}); !errors.Is(err, ErrInvalidPolicy) {
			t.Fatalf("min_coverage %s: got %v", bad, err)
		}
	}
}

func TestDeduplicateOwnerFacts_RevisionsSourcesAndRedelivery(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	iv, _ := NewHalfOpenInterval(t0, t0.Add(time.Hour))
	base := OwnerFactEvent{EventID: "e", SourceKey: "plc", State: StateRunning, Interval: iv, EventAt: t0, RecordedAt: t0.Add(time.Minute), Revision: 1, PayloadDigest: "v1"}
	correction := base
	correction.Revision, correction.State, correction.PayloadDigest = 2, StateUnplannedDowntime, "v2"

	for _, order := range [][]OwnerFactEvent{{base, correction}, {correction, base}} {
		out, err := DeduplicateOwnerFacts(order)
		if err != nil || len(out) != 1 || out[0].Revision != 2 || out[0].State != StateUnplannedDowntime {
			t.Fatalf("the highest revision must win in any order: %v %+v", err, out)
		}
	}

	otherSource := base
	otherSource.SourceKey = "mes"
	if out, err := DeduplicateOwnerFacts([]OwnerFactEvent{base, otherSource}); err != nil || len(out) != 2 {
		t.Fatalf("event IDs are scoped by source: %v %+v", err, out)
	}

	sameRevision := base
	sameRevision.PayloadDigest = "other"
	if _, err := DeduplicateOwnerFacts([]OwnerFactEvent{base, sameRevision}); !errors.Is(err, ErrConflictingDuplicate) {
		t.Fatalf("same revision with different content: got %v", err)
	}
	movedEventTime := base
	movedEventTime.EventAt = t0.Add(time.Second)
	if _, err := DeduplicateOwnerFacts([]OwnerFactEvent{base, movedEventTime}); !errors.Is(err, ErrConflictingDuplicate) {
		t.Fatalf("same revision with different event time: got %v", err)
	}

	redelivered := base
	redelivered.RecordedAt = t0.Add(time.Hour)
	for _, order := range [][]OwnerFactEvent{{base, redelivered}, {redelivered, base}} {
		out, err := DeduplicateOwnerFacts(order)
		if err != nil || len(out) != 1 || !out[0].RecordedAt.Equal(base.RecordedAt) {
			t.Fatalf("a redelivery keeps the first receipt: %v %+v", err, out)
		}
	}

	for name, state := range map[string]TimelineState{"unknown": "PRODUCING", "output-only gap": StateGap, "empty": ""} {
		bad := base
		bad.State = state
		if _, err := DeduplicateOwnerFacts([]OwnerFactEvent{bad}); !errors.Is(err, ErrMalformedFact) {
			t.Errorf("%s state: got %v", name, err)
		}
	}
	unrevised := base
	unrevised.Revision = 0
	if _, err := DeduplicateOwnerFacts([]OwnerFactEvent{unrevised}); !errors.Is(err, ErrMalformedFact) {
		t.Fatalf("revision 0: got %v", err)
	}
}

func TestResolveTimeline_NormalizesToUTC(t *testing.T) {
	ist := time.FixedZone("IST", 19800)
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	policy := TimelinePolicy{PrecedenceOrder: []TimelineState{StateRunning}, MinCoverage: new(big.Rat)}
	build := func(loc *time.Location) string {
		window := HalfOpenInterval{Start: t0.In(loc), End: t0.Add(2 * time.Hour).In(loc)}
		fact := OwnerFactEvent{EventID: "e", State: StateRunning, Revision: 1, PayloadDigest: "d",
			Interval: HalfOpenInterval{Start: t0.Add(30 * time.Minute).In(loc), End: t0.Add(time.Hour).In(loc)},
			EventAt:  t0.In(loc), RecordedAt: t0.In(loc)}
		res, err := ResolveTimeline(window, []OwnerFactEvent{fact}, policy)
		if err != nil {
			t.Fatal(err)
		}
		return mustJSON(t, res)
	}
	if utc, local := build(time.UTC), build(ist); utc != local {
		t.Fatalf("the same instants in another zone changed the output:\n%s\n%s", utc, local)
	}
}

func TestResolveTimelineWithExclusions_RequiresRecordedAt(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	window, _ := NewHalfOpenInterval(t0, t0.Add(time.Hour))
	policy := TimelinePolicy{PrecedenceOrder: []TimelineState{StatePlannedBreak}}
	ex := CalendarExclusionPolicy{PlannedBreaks: []CalendarExclusion{{ExclusionID: "b", Interval: window, Revision: 1}}}
	if _, err := ResolveTimelineWithExclusions(window, nil, ex, policy); !errors.Is(err, ErrMalformedFact) {
		t.Fatalf("missing recorded_at: got %v", err)
	}
	ex.PlannedBreaks[0].RecordedAt = t0.Add(-24 * time.Hour)
	res, err := ResolveTimelineWithExclusions(window, nil, ex, policy)
	if err != nil || res.Readiness.Status != StatusReady || res.Segments[0].State != StatePlannedBreak {
		t.Fatalf("%v %+v", err, res)
	}
}

func TestComputeMonotonicCounterDeltas_ResetAndRolloverEvidence(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	r := func(id string, v int64, at time.Duration) CounterReading {
		return CounterReading{ReadingID: id, CounterCode: "good", RawValue: big.NewRat(v, 1), OccurredAt: t0.Add(at)}
	}
	// An evidenced reset counts from zero even when the raw value happens to be higher.
	reset := r("b", 150, time.Minute)
	reset.HasResetEvidence = true
	deltas, err := ComputeMonotonicCounterDeltas([]CounterReading{r("a", 100, 0), reset})
	if err != nil || len(deltas) != 1 || deltas[0].DeltaUnits.Cmp(big.NewRat(150, 1)) != 0 {
		t.Fatalf("reset above previous: %v %+v", err, deltas)
	}
	both := r("b", 50, time.Minute)
	both.HasResetEvidence, both.HasRolloverEvidence, both.RolloverThreshold = true, true, big.NewRat(1000, 1)
	if _, err := ComputeMonotonicCounterDeltas([]CounterReading{r("a", 900, 0), both}); !errors.Is(err, ErrInvalidRollover) {
		t.Fatalf("reset and rollover together: got %v", err)
	}
	noDrop := r("b", 950, time.Minute)
	noDrop.HasRolloverEvidence, noDrop.RolloverThreshold = true, big.NewRat(1000, 1)
	if _, err := ComputeMonotonicCounterDeltas([]CounterReading{r("a", 900, 0), noDrop}); !errors.Is(err, ErrInvalidRollover) {
		t.Fatalf("rollover without a drop: got %v", err)
	}
}

const (
	digestTenant = "0a0a0a0a-0a0a-4a0a-8a0a-0a0a0a0a0a0a"
	digestSite   = "1a1a1a1a-1a1a-4a1a-8a1a-1a1a1a1a1a1a"
)

// The canonical digest is the same for the same dataset and policy in any order or zone,
// and changes with any fact, revision, rule, tenant or site.
func TestTimelineDigestIsCanonicalAndScoped(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	window, _ := NewHalfOpenInterval(t0, t0.Add(time.Hour))
	a, _ := NewHalfOpenInterval(t0, t0.Add(40*time.Minute))
	b, _ := NewHalfOpenInterval(t0.Add(40*time.Minute), t0.Add(time.Hour))
	facts := []OwnerFactEvent{
		{EventID: "e1", SourceKey: "plc", State: StateRunning, Interval: a, RecordedAt: t0.Add(41 * time.Minute), Revision: 1, PayloadDigest: "a"},
		{EventID: "e2", SourceKey: "plc", State: StateUnplannedDowntime, Interval: b, RecordedAt: t0.Add(time.Hour), Revision: 1, PayloadDigest: "b"},
	}
	policy := TimelinePolicy{PrecedenceOrder: []TimelineState{StateUnplannedDowntime, StateRunning}}
	digest := func(fs []OwnerFactEvent, p TimelinePolicy, tenant, site string) string {
		t.Helper()
		r, err := ResolveTimeline(window, fs, p)
		if err != nil {
			t.Fatal(err)
		}
		d, err := TimelineDigest(tenant, site, r, p)
		if err != nil || len(d) != 64 {
			t.Fatalf("digest %q %v", d, err)
		}
		return d
	}
	base := digest(facts, policy, digestTenant, digestSite)
	if got := digest([]OwnerFactEvent{facts[1], facts[0]}, policy, digestTenant, digestSite); got != base {
		t.Fatal("fact order changed the digest")
	}
	ist := time.FixedZone("IST", 19800)
	local := append([]OwnerFactEvent(nil), facts...)
	for i := range local {
		local[i].Interval = HalfOpenInterval{Start: local[i].Interval.Start.In(ist), End: local[i].Interval.End.In(ist)}
	}
	if got := digest(local, policy, digestTenant, digestSite); got != base {
		t.Fatal("the same instants in another zone changed the digest")
	}
	revised := append([]OwnerFactEvent(nil), facts...)
	revised[1].Revision, revised[1].PayloadDigest = 2, "b2"
	other := "2a2a2a2a-2a2a-4a2a-8a2a-2a2a2a2a2a2a"
	tighter := policy
	tighter.Limits = TimelineLimits{MaxFacts: 10}
	for name, d := range map[string]string{
		"revision":   digest(revised, policy, digestTenant, digestSite),
		"tenant":     digest(facts, policy, other, digestSite),
		"site":       digest(facts, policy, digestTenant, other),
		"precedence": digest(facts, TimelinePolicy{PrecedenceOrder: []TimelineState{StateRunning, StateUnplannedDowntime}}, digestTenant, digestSite),
		"limits":     digest(facts, tighter, digestTenant, digestSite),
	} {
		if d == base {
			t.Errorf("%s did not change the digest", name)
		}
	}
	if _, err := TimelineDigest("not-a-uuid", digestSite, TimelineResolutionResult{}, policy); err == nil {
		t.Fatal("a non-canonical tenant was accepted")
	}
}

// With a silence limit, a source whose newest fact was recorded too long before the window
// ended is stale; calendar exclusions are exempt; a missing recorded time fails closed.
func TestResolveTimelineStaleSources(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	window, _ := NewHalfOpenInterval(t0, t0.Add(time.Hour))
	fact := func(recorded time.Time) []OwnerFactEvent {
		return []OwnerFactEvent{{EventID: "e", SourceKey: "plc", State: StateRunning, Interval: window, RecordedAt: recorded, Revision: 1, PayloadDigest: "d"}}
	}
	policy := TimelinePolicy{PrecedenceOrder: []TimelineState{StateRunning, StatePlannedBreak}, MaxSourceSilence: 5 * time.Minute}
	for name, tc := range map[string]struct {
		recorded time.Time
		stale    bool
	}{
		"recorded after the window": {t0.Add(2 * time.Hour), false},
		"exactly at the limit":      {t0.Add(55 * time.Minute), false},
		"a second past the limit":   {t0.Add(55*time.Minute - time.Second), true},
		"recorded at the start":     {t0, true},
		"no recorded time":          {time.Time{}, true},
	} {
		r, err := ResolveTimeline(window, fact(tc.recorded), policy)
		if err != nil {
			t.Fatal(err)
		}
		if got := r.Readiness.Status == StatusNotReady && len(r.Readiness.ReasonCodes) > 0 && r.Readiness.ReasonCodes[0] == ReasonStaleSource; got != tc.stale {
			t.Errorf("%s: %+v", name, r.Readiness)
		}
	}
	// Without the policy the check does not run.
	if r, _ := ResolveTimeline(window, fact(t0), TimelinePolicy{PrecedenceOrder: policy.PrecedenceOrder}); r.Readiness.Status != StatusReady {
		t.Fatalf("freshness checked without a policy: %+v", r.Readiness)
	}
	// A calendar exclusion recorded long ago is not a stale source.
	ex := CalendarExclusionPolicy{PlannedBreaks: []CalendarExclusion{{ExclusionID: "brk", Interval: window, Revision: 1, RecordedAt: t0.Add(-30 * 24 * time.Hour)}}}
	if r, err := ResolveTimelineWithExclusions(window, nil, ex, policy); err != nil || r.Readiness.Status != StatusReady {
		t.Fatalf("calendar exclusion treated as stale: %v %+v", err, r.Readiness)
	}
	if _, err := ResolveTimeline(window, nil, TimelinePolicy{MaxSourceSilence: -time.Second}); !errors.Is(err, ErrInvalidPolicy) {
		t.Fatalf("negative silence: %v", err)
	}
}

// TimelineDigest normalizes times itself, so a result built in another zone digests equally.
func TestTimelineDigestNormalizesZones(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	utc := TimelineResolutionResult{Window: HalfOpenInterval{Start: t0, End: t0.Add(time.Hour)}, Readiness: ReadinessDecision{Status: StatusNotReady, ReasonCodes: []ReasonCode{ReasonMissingData}}}
	ist := utc
	zone := time.FixedZone("IST", 19800)
	ist.Window = HalfOpenInterval{Start: t0.In(zone), End: t0.Add(time.Hour).In(zone)}
	a, _ := TimelineDigest(digestTenant, digestSite, utc, TimelinePolicy{})
	b, _ := TimelineDigest(digestTenant, digestSite, ist, TimelinePolicy{})
	if a == "" || a != b {
		t.Fatalf("zone changed the digest: %s %s", a, b)
	}
}
