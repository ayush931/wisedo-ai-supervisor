package oee

import (
	"errors"
	"math/big"
	"math/rand"
	"regexp"
	"testing"
	"time"

	"github.com/google/uuid"
)

var kt0 = time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)

func kWindow(d time.Duration) HalfOpenInterval {
	w, _ := NewHalfOpenInterval(kt0, kt0.Add(d))
	return w
}

func seg(from, to time.Duration, s TimelineState) CanonicalFactSegment {
	iv, _ := NewHalfOpenInterval(kt0.Add(from), kt0.Add(to))
	return CanonicalFactSegment{Interval: iv, State: s, EventID: string(s) + from.String()}
}

// oneHourRun is a fully running hour producing 3000 pieces at an ideal 1 s/piece.
func oneHourRun() KPICalculationInput {
	w := kWindow(time.Hour)
	return KPICalculationInput{
		Window:   w,
		Timeline: TimelineResolutionResult{Window: w, Segments: []CanonicalFactSegment{seg(0, time.Hour, StateRunning)}, Readiness: ReadinessDecision{Status: StatusReady}},
		CountContributions: []ProductCountContribution{
			{ContributionID: "c1", ProductID: "P", Interval: w, TotalCount: big.NewRat(3000, 1), Unit: "pcs"},
		},
		IdealCycles: []IdealCycleRevision{{ProductID: "P", Revision: 1, SecondsPerUnit: big.NewRat(1, 1), Unit: "s/unit"}},
		Policy:      DefaultKPIPolicy(uuid.MustParse("11111111-1111-4111-8111-111111111111")),
	}
}

func mustCalc(t *testing.T, in KPICalculationInput) KPICalculationResult {
	t.Helper()
	r, err := CalculateKPIs(in)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func kpiHasReason(r KPICalculationResult, code ReasonCode) bool {
	for _, c := range r.Readiness.ReasonCodes {
		if c == code {
			return true
		}
	}
	return false
}

// The digest binds versions, policy content and readiness, not only the formatted ratios.
func TestKPIDigestBindsVersionsPolicyAndReadiness(t *testing.T) {
	base := mustCalc(t, oneHourRun())
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(base.PolicyDigest) {
		t.Fatalf("policy digest is not a computed sha256: %q", base.PolicyDigest)
	}
	changes := map[string]func(*KPICalculationInput){
		"formula version": func(in *KPICalculationInput) { in.Policy.FormulaVersion = "9999-other" },
		"kernel version":  func(in *KPICalculationInput) { in.Policy.KernelVersion = "v9" },
		"policy version":  func(in *KPICalculationInput) { in.Policy.PolicyVersion = "9" },
		"rework rule":     func(in *KPICalculationInput) { in.Policy.ReworkCountsAsGood = true },
		"microstop rule":  func(in *KPICalculationInput) { in.Policy.MicrostopThresholdSec = big.NewRat(1, 1) },
		"cap rule":        func(in *KPICalculationInput) { in.Policy.PerformanceCapRule = CapFlagInvalid },
		"state buckets": func(in *KPICalculationInput) {
			in.Policy.StateBuckets = DefaultStateBuckets()
			in.Policy.StateBuckets[StateStandby] = BucketExcluded
		},
		"policy revision": func(in *KPICalculationInput) { in.Policy.PolicyRevisionID = uuid.New() },
		"timeline readiness": func(in *KPICalculationInput) {
			in.Timeline.Readiness = ReadinessDecision{Status: StatusNotReady, ReasonCodes: []ReasonCode{ReasonStaleSource}}
		},
	}
	for name, change := range changes {
		in := oneHourRun()
		change(&in)
		r := mustCalc(t, in)
		if r.CalculationDigest == base.CalculationDigest {
			t.Errorf("%s did not change the calculation digest", name)
		}
	}
	// A caller cannot name its own policy digest.
	in := oneHourRun()
	in.Policy.PolicyDigest = "caller-chosen"
	if r := mustCalc(t, in); r.PolicyDigest != base.PolicyDigest || r.CalculationDigest != base.CalculationDigest {
		t.Fatal("a caller policy digest changed the result")
	}
}

func TestKPIInputOrderAndZoneInvariance(t *testing.T) {
	in := oneHourRun()
	w := in.Window
	half := HalfOpenInterval{Start: w.Start, End: w.Start.Add(30 * time.Minute)}
	rest := HalfOpenInterval{Start: half.End, End: w.End}
	in.CountContributions = []ProductCountContribution{
		{ContributionID: "a", ProductID: "P", Interval: half, TotalCount: big.NewRat(1500, 1), Unit: "pcs"},
		{ContributionID: "b", ProductID: "Q", Interval: rest, TotalCount: big.NewRat(700, 1), Unit: "pcs"},
	}
	in.IdealCycles = []IdealCycleRevision{
		{ProductID: "P", Revision: 1, SecondsPerUnit: big.NewRat(1, 1), Unit: "s/unit"},
		{ProductID: "Q", Revision: 2, SecondsPerUnit: big.NewRat(2, 1), Unit: "s/unit"},
	}
	base := mustCalc(t, in)
	swapped := in
	swapped.CountContributions = []ProductCountContribution{in.CountContributions[1], in.CountContributions[0]}
	swapped.IdealCycles = []IdealCycleRevision{in.IdealCycles[1], in.IdealCycles[0]}
	if mustCalc(t, swapped).CalculationDigest != base.CalculationDigest {
		t.Fatal("input order changed the digest")
	}
	ist := time.FixedZone("IST", 19800)
	local := in
	local.Window = HalfOpenInterval{Start: w.Start.In(ist), End: w.End.In(ist)}
	local.Timeline.Window = local.Window
	if mustCalc(t, local).CalculationDigest != base.CalculationDigest {
		t.Fatal("the same instants in another zone changed the digest")
	}
	// The same revision twice is ambiguous: it never depends on which one sorts first.
	dup := oneHourRun()
	dup.IdealCycles = append(dup.IdealCycles, IdealCycleRevision{ProductID: "P", Revision: 1, SecondsPerUnit: big.NewRat(2, 1), Unit: "s/unit"})
	if r := mustCalc(t, dup); r.Readiness.Status != StatusNotReady || !kpiHasReason(r, ReasonConflictingFact) {
		t.Fatalf("duplicate ideal cycle revision: %+v", r.Readiness)
	}
}

func TestKPIQualityAccountingFollowsADR(t *testing.T) {
	counts := func(total, conforming, scrap, rework *big.Rat, reworkGood bool) KPICalculationResult {
		in := oneHourRun()
		in.CountContributions[0].TotalCount, in.CountContributions[0].ConformingCount = total, conforming
		in.CountContributions[0].ScrapCount, in.CountContributions[0].ReworkCount = scrap, rework
		in.Policy.ReworkCountsAsGood = reworkGood
		return mustCalc(t, in)
	}
	n := func(v int64) *big.Rat { return big.NewRat(v, 1) }
	// Total = Conforming + Scrap + Rework, with conforming derived when not given.
	if r := counts(n(1000), nil, n(100), n(200), false); r.GoodPieces.Cmp(n(700)) != 0 || r.Readiness.Status == StatusInvalid {
		t.Fatalf("rework excluded: good=%s %+v", r.GoodPieces.RatString(), r.Readiness)
	}
	if r := counts(n(1000), nil, n(100), n(200), true); r.GoodPieces.Cmp(n(900)) != 0 {
		t.Fatalf("rework counted: good=%s", r.GoodPieces.RatString())
	}
	if r := counts(n(1000), n(700), n(100), n(200), false); r.GoodPieces.Cmp(n(700)) != 0 || r.Readiness.Status == StatusInvalid {
		t.Fatalf("explicit conforming: good=%s %+v", r.GoodPieces.RatString(), r.Readiness)
	}
	for name, r := range map[string]KPICalculationResult{
		"negative scrap":         counts(n(1000), nil, n(-50), nil, false),
		"negative rework":        counts(n(1000), nil, nil, n(-1), false),
		"negative conforming":    counts(n(1000), n(-1), nil, nil, false),
		"components over total":  counts(n(1000), n(900), n(500), nil, false),
		"components under total": counts(n(1000), n(100), n(100), nil, false),
		"scrap above total":      counts(n(100), nil, n(150), nil, false),
	} {
		if r.Readiness.Status != StatusInvalid || !kpiHasReason(r, ReasonInvalidQualityCounts) {
			t.Errorf("%s: %+v", name, r.Readiness)
		}
	}
}

func TestKPIUnitsAreExactAndNeverGuessed(t *testing.T) {
	in := oneHourRun()
	in.IdealCycles[0].Unit, in.IdealCycles[0].SecondsPerUnit = "ms/unit", big.NewRat(1000, 1)
	in.CountContributions[0].Unit, in.CountContributions[0].TotalCount = "dozen", big.NewRat(250, 1)
	if r := mustCalc(t, in); r.TotalPieces.Cmp(big.NewRat(3000, 1)) != 0 || r.IdealRunSeconds.Cmp(big.NewRat(3000, 1)) != 0 || r.Readiness.Status != StatusReady {
		t.Fatalf("exact conversion: pieces=%s ideal=%s %+v", r.TotalPieces.RatString(), r.IdealRunSeconds.RatString(), r.Readiness)
	}
	for name, change := range map[string]func(*KPICalculationInput){
		"unknown cycle unit": func(in *KPICalculationInput) { in.IdealCycles[0].Unit = "units/min" },
		"empty cycle unit":   func(in *KPICalculationInput) { in.IdealCycles[0].Unit = "" },
		"unknown count unit": func(in *KPICalculationInput) { in.CountContributions[0].Unit = "kg" },
		"empty count unit":   func(in *KPICalculationInput) { in.CountContributions[0].Unit = "" },
	} {
		in := oneHourRun()
		change(&in)
		if r := mustCalc(t, in); r.Readiness.Status != StatusInvalid || !kpiHasReason(r, ReasonUnsupportedRule) {
			t.Errorf("%s: %+v", name, r.Readiness)
		}
	}
	if _, err := ConvertCycleTimeToSeconds(big.NewRat(1, 1), ""); !errors.Is(err, ErrUnsupportedUnit) {
		t.Fatalf("empty unit: %v", err)
	}
}

func TestKPIPolicyIsValidatedNotRepaired(t *testing.T) {
	for name, change := range map[string]func(*KPIPolicy){
		"rounding mode":  func(p *KPIPolicy) { p.RoundingMode = "BANKERS" },
		"rounding scale": func(p *KPIPolicy) { p.RoundingScale = 7 },
		"negative scale": func(p *KPIPolicy) { p.RoundingScale = -1 },
		"zero rule":      func(p *KPIPolicy) { p.ZeroDenominatorRule = "SKIP" },
		"cap rule":       func(p *KPIPolicy) { p.PerformanceCapRule = "CLAMP" },
		"microstop":      func(p *KPIPolicy) { p.MicrostopThresholdSec = big.NewRat(-1, 1) },
		"missing bucket": func(p *KPIPolicy) { p.StateBuckets = map[TimelineState]StateBucket{StateRunning: BucketRun} },
		"unknown bucket": func(p *KPIPolicy) { p.StateBuckets = DefaultStateBuckets(); p.StateBuckets[StateStandby] = "IDLE" },
		"unknown state":  func(p *KPIPolicy) { p.StateBuckets = DefaultStateBuckets(); p.StateBuckets["PRODUCING"] = BucketRun },
		"downtime as run": func(p *KPIPolicy) {
			p.StateBuckets = DefaultStateBuckets()
			p.StateBuckets[StateUnplannedDowntime] = BucketRun
		},
	} {
		in := oneHourRun()
		change(&in.Policy)
		if _, err := CalculateKPIs(in); !errors.Is(err, ErrInvalidPolicy) {
			t.Errorf("%s: got %v", name, err)
		}
	}
}

func TestKPIIdealCycleBindsOneEffectiveRevision(t *testing.T) {
	w := kWindow(2 * time.Hour)
	first, _ := NewHalfOpenInterval(kt0, kt0.Add(time.Hour))
	second, _ := NewHalfOpenInterval(kt0.Add(time.Hour), kt0.Add(2*time.Hour))
	in := KPICalculationInput{
		Window:   w,
		Timeline: TimelineResolutionResult{Window: w, Segments: []CanonicalFactSegment{seg(0, 2*time.Hour, StateRunning)}},
		IdealCycles: []IdealCycleRevision{
			{ProductID: "P", Revision: 1, SecondsPerUnit: big.NewRat(1, 1), Unit: "s/unit", EffectiveWindow: first},
			{ProductID: "P", Revision: 2, SecondsPerUnit: big.NewRat(7, 1), Unit: "s/unit", EffectiveWindow: second},
		},
		Policy: DefaultKPIPolicy(uuid.New()),
	}
	// Contributions inside one revision bind that revision.
	in.CountContributions = []ProductCountContribution{
		{ContributionID: "a", ProductID: "P", Interval: first, TotalCount: big.NewRat(100, 1), Unit: "pcs"},
		{ContributionID: "b", ProductID: "P", Interval: second, TotalCount: big.NewRat(100, 1), Unit: "pcs"},
	}
	if r := mustCalc(t, in); r.IdealRunSeconds.Cmp(big.NewRat(800, 1)) != 0 || r.Readiness.Status != StatusReady {
		t.Fatalf("per-revision binding: ideal=%s %+v", r.IdealRunSeconds.RatString(), r.Readiness)
	}
	// A contribution spanning the boundary is never bound to one revision for its whole count.
	in.CountContributions = []ProductCountContribution{{ContributionID: "span", ProductID: "P", Interval: w, TotalCount: big.NewRat(100, 1), Unit: "pcs"}}
	if r := mustCalc(t, in); r.Readiness.Status != StatusNotReady || !kpiHasReason(r, ReasonConflictingFact) || r.IdealRunSeconds.Sign() != 0 {
		t.Fatalf("spanning contribution: ideal=%s %+v", r.IdealRunSeconds.RatString(), r.Readiness)
	}
	// A half-set effective window is not "unbounded".
	half := in
	half.CountContributions = []ProductCountContribution{{ContributionID: "a", ProductID: "P", Interval: first, TotalCount: big.NewRat(1, 1), Unit: "pcs"}}
	half.IdealCycles = []IdealCycleRevision{{ProductID: "P", Revision: 1, SecondsPerUnit: big.NewRat(1, 1), Unit: "s/unit", EffectiveWindow: HalfOpenInterval{Start: kt0.Add(100 * time.Hour)}}}
	if r := mustCalc(t, half); r.Readiness.Status != StatusInvalid || !kpiHasReason(r, ReasonUnsupportedRule) {
		t.Fatalf("half-set window: %+v", r.Readiness)
	}
	// Two revisions whose windows both contain the interval conflict.
	overlap := half
	overlap.IdealCycles = []IdealCycleRevision{
		{ProductID: "P", Revision: 1, SecondsPerUnit: big.NewRat(1, 1), Unit: "s/unit", EffectiveWindow: w},
		{ProductID: "P", Revision: 2, SecondsPerUnit: big.NewRat(2, 1), Unit: "s/unit", EffectiveWindow: first},
	}
	if r := mustCalc(t, overlap); !kpiHasReason(r, ReasonConflictingFact) {
		t.Fatalf("overlapping revisions: %+v", r.Readiness)
	}
}

func TestKPIWindowContract(t *testing.T) {
	in := oneHourRun()
	in.Timeline.Window = kWindow(4 * time.Hour)
	if _, err := CalculateKPIs(in); !errors.Is(err, ErrInvalidKPIInput) {
		t.Fatalf("timeline window mismatch: %v", err)
	}
	in = oneHourRun()
	far, _ := NewHalfOpenInterval(kt0.Add(48*time.Hour), kt0.Add(49*time.Hour))
	in.CountContributions[0].Interval = far
	if _, err := CalculateKPIs(in); !errors.Is(err, ErrInvalidKPIInput) {
		t.Fatalf("contribution outside the window: %v", err)
	}
	in = oneHourRun()
	in.Timeline.Segments = []CanonicalFactSegment{seg(0, 2*time.Hour, StateRunning)}
	if _, err := CalculateKPIs(in); !errors.Is(err, ErrInvalidKPIInput) {
		t.Fatalf("segment outside the window: %v", err)
	}
	in = oneHourRun()
	in.Timeline.Segments = []CanonicalFactSegment{seg(0, 40*time.Minute, StateRunning), seg(30*time.Minute, time.Hour, StateRunning)}
	if _, err := CalculateKPIs(in); !errors.Is(err, ErrInvalidKPIInput) {
		t.Fatalf("overlapping segments: %v", err)
	}
	in = oneHourRun()
	in.Timeline.Segments = []CanonicalFactSegment{{Interval: in.Window, State: "PRODUCING"}}
	if _, err := CalculateKPIs(in); !errors.Is(err, ErrInvalidKPIInput) {
		t.Fatalf("unknown state: %v", err)
	}
	// Zero calendar time is INVALID with ZERO_DENOMINATOR (ADR 0005 Sec. 11).
	zero := KPICalculationInput{Window: HalfOpenInterval{Start: kt0, End: kt0}, Policy: DefaultKPIPolicy(uuid.New())}
	if r := mustCalc(t, zero); r.Readiness.Status != StatusInvalid || !kpiHasReason(r, ReasonZeroDenominator) {
		t.Fatalf("zero calendar: %+v", r.Readiness)
	}
	reversed := KPICalculationInput{Window: HalfOpenInterval{Start: kt0.Add(time.Hour), End: kt0}, Policy: DefaultKPIPolicy(uuid.New())}
	if _, err := CalculateKPIs(reversed); !errors.Is(err, ErrInvalidKPIInput) {
		t.Fatalf("reversed window: %v", err)
	}
}

// Microstops are a Performance loss (Loss 3), not an Availability loss; a stop is one event
// even when the timeline splits it into several segments.
func TestKPIMicrostopsArePerformanceLoss(t *testing.T) {
	in := oneHourRun()
	in.Timeline.Segments = []CanonicalFactSegment{seg(0, 30*time.Minute, StateRunning), seg(30*time.Minute, 34*time.Minute, StateUnplannedDowntime), seg(34*time.Minute, time.Hour, StateRunning)}
	r := mustCalc(t, in)
	if r.Availability.Formatted != "1.000000" || r.MicrostopCount != 1 || r.MicrostopSeconds.Cmp(big.NewRat(240, 1)) != 0 || r.StopSeconds.Sign() != 0 {
		t.Fatalf("4-minute minor stop: A=%s count=%d micro=%s stop=%s", r.Availability.Formatted, r.MicrostopCount, r.MicrostopSeconds.RatString(), r.StopSeconds.RatString())
	}
	if r.Performance.Formatted != "0.833333" { // 3000 ideal seconds over 3600 operating seconds
		t.Fatalf("performance carries the minor stop: %s", r.Performance.Formatted)
	}
	// Two adjacent 3-minute downtime segments are one 6-minute stop: an availability loss.
	in.Timeline.Segments = []CanonicalFactSegment{seg(0, 30*time.Minute, StateRunning), seg(30*time.Minute, 33*time.Minute, StateUnplannedDowntime), seg(33*time.Minute, 36*time.Minute, StateUnplannedDowntime), seg(36*time.Minute, time.Hour, StateRunning)}
	r = mustCalc(t, in)
	if r.MicrostopCount != 0 || r.StopSeconds.Cmp(big.NewRat(360, 1)) != 0 || r.Availability.Formatted != "0.900000" {
		t.Fatalf("split long stop: count=%d stop=%s A=%s", r.MicrostopCount, r.StopSeconds.RatString(), r.Availability.Formatted)
	}
}

func TestKPIRollupRequiresComparableChildren(t *testing.T) {
	a := mustCalc(t, oneHourRun())
	in := oneHourRun()
	in.CountContributions[0].TotalCount = big.NewRat(2000, 1)
	b := mustCalc(t, in)
	policy := oneHourRun().Policy
	if _, err := AggregateKPIResults([]KPICalculationResult{a, b}, policy); err != nil {
		t.Fatalf("comparable children: %v", err)
	}
	if _, err := AggregateKPIResults([]KPICalculationResult{a, a}, policy); !errors.Is(err, ErrIncompatibleChildren) {
		t.Fatalf("the same child twice: %v", err)
	}
	other := oneHourRun()
	other.Policy.ReworkCountsAsGood = true
	c := mustCalc(t, other)
	if _, err := AggregateKPIResults([]KPICalculationResult{a, c}, policy); !errors.Is(err, ErrIncompatibleChildren) {
		t.Fatalf("a child under another policy: %v", err)
	}
	if _, err := AggregateKPIResults([]KPICalculationResult{a, {}}, policy); !errors.Is(err, ErrIncompatibleChildren) {
		t.Fatalf("an empty child must not panic or pass: %v", err)
	}
}

// Seeded property tests: conservation and component relationships hold exactly, and
// shuffling the inputs never changes the digest.
func TestKPIProperties(t *testing.T) {
	states := []TimelineState{StateRunning, StateUnplannedDowntime, StateChangeover, StateStandby, StatePlannedBreak, StateGap}
	published := 0
	for seed := int64(1); seed <= 200; seed++ {
		rng := rand.New(rand.NewSource(seed))
		in := oneHourRun()
		var segs []CanonicalFactSegment
		cursor := time.Duration(0)
		for cursor < time.Hour {
			d := time.Duration(rng.Intn(900)+1) * time.Second
			if cursor+d > time.Hour {
				d = time.Hour - cursor
			}
			s := seg(cursor, cursor+d, states[rng.Intn(len(states))])
			s.IsGap = s.State == StateGap
			segs = append(segs, s)
			cursor += d
		}
		in.Timeline.Segments = segs
		in.CountContributions = nil
		for i := 0; i < rng.Intn(5)+1; i++ {
			total := int64(rng.Intn(500) + 1)
			scrap := int64(rng.Intn(int(total) + 1))
			in.CountContributions = append(in.CountContributions, ProductCountContribution{
				ContributionID: uuid.NewString(), ProductID: "P", Interval: in.Window,
				TotalCount: big.NewRat(total, 1), ScrapCount: big.NewRat(scrap, 1), Unit: "pcs",
			})
		}
		r := mustCalc(t, in)
		planned := new(big.Rat).Add(r.RunSeconds, r.StopSeconds)
		covered := new(big.Rat).Add(planned, new(big.Rat).Add(r.PlannedExclusionSeconds, r.GapSeconds))
		if r.PlannedProductionSeconds.Cmp(planned) != 0 || covered.Cmp(r.TotalCalendarSeconds) != 0 {
			t.Fatalf("seed %d: time is not conserved", seed)
		}
		if r.Availability.Value != nil && r.Performance.Value != nil && r.Quality.Value != nil {
			published++
			want := new(big.Rat).Mul(r.Availability.Value, new(big.Rat).Mul(r.Performance.Value, r.Quality.Value))
			if r.OEE.Value.Cmp(want) != 0 || r.TEEP.Value.Cmp(new(big.Rat).Mul(want, r.Loading.Value)) != 0 {
				t.Fatalf("seed %d: OEE != A*P*Q or TEEP != OEE*Loading", seed)
			}
		}
		shuffled := in
		shuffled.CountContributions = append([]ProductCountContribution(nil), in.CountContributions...)
		rng.Shuffle(len(shuffled.CountContributions), func(i, j int) {
			shuffled.CountContributions[i], shuffled.CountContributions[j] = shuffled.CountContributions[j], shuffled.CountContributions[i]
		})
		shuffled.Timeline.Segments = append([]CanonicalFactSegment(nil), segs...)
		rng.Shuffle(len(shuffled.Timeline.Segments), func(i, j int) {
			shuffled.Timeline.Segments[i], shuffled.Timeline.Segments[j] = shuffled.Timeline.Segments[j], shuffled.Timeline.Segments[i]
		})
		if mustCalc(t, shuffled).CalculationDigest != r.CalculationDigest {
			t.Fatalf("seed %d: input order changed the digest", seed)
		}
	}
	if published < 50 {
		t.Fatalf("only %d seeds published every factor; the OEE/TEEP identities are barely exercised", published)
	}
}

// Each version field of a result is bound by the digest on its own, not only through the
// policy digest.
func TestKPIDigestBindsEachResultVersionField(t *testing.T) {
	base := mustCalc(t, oneHourRun())
	for name, change := range map[string]func(*KPICalculationResult){
		"formula": func(r *KPICalculationResult) { r.FormulaVersion = "x" },
		"policy":  func(r *KPICalculationResult) { r.PolicyVersion = "x" },
		"kernel":  func(r *KPICalculationResult) { r.KernelVersion = "x" },
	} {
		r := base
		change(&r)
		if d, _ := ComputeKPICalculationDigest(r); d == base.CalculationDigest {
			t.Errorf("%s version is not bound", name)
		}
	}
}

// One revision number used twice is ambiguous even when the two windows do not overlap.
func TestKPIDuplicateRevisionNumberWithDisjointWindows(t *testing.T) {
	in := oneHourRun()
	first, _ := NewHalfOpenInterval(kt0, kt0.Add(30*time.Minute))
	second, _ := NewHalfOpenInterval(kt0.Add(30*time.Minute), kt0.Add(time.Hour))
	in.CountContributions[0].Interval = first
	in.IdealCycles = []IdealCycleRevision{
		{ProductID: "P", Revision: 3, SecondsPerUnit: big.NewRat(1, 1), Unit: "s/unit", EffectiveWindow: first},
		{ProductID: "P", Revision: 3, SecondsPerUnit: big.NewRat(2, 1), Unit: "s/unit", EffectiveWindow: second},
	}
	if r := mustCalc(t, in); !kpiHasReason(r, ReasonConflictingFact) {
		t.Fatalf("duplicate revision number: %+v", r.Readiness)
	}
}
