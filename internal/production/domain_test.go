package production

import (
	"encoding/json"
	"errors"
	"math/big"
	"strings"
	"testing"
)

const (
	approvalID = "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d"
	orderID    = "abcdefab-cdef-4abc-8def-abcdefabcdef"
	assetID    = "22222222-2222-4222-8222-222222222222"
	base       = "2026-09-27T00:00:00Z"
)

func goldenPlan() Plan {
	return Plan{
		OrderID: orderID, AssetID: assetID, WindowStart: base, WindowEnd: "2026-09-27T00:15:00Z",
		PlanRevision: 1, ApprovalRef: approvalID, Unit: "count",
		RateSegments:  []RateSegment{{Start: base, End: "2026-09-27T00:15:00Z", TargetGoodUnitsPerSecond: "1"}},
		PlannedBreaks: []Interval{{Start: "2026-09-27T00:01:40Z", End: "2026-09-27T00:15:00Z"}},
	}
}

func TestGoldenExpectedAndActualShortfall(t *testing.T) {
	p := goldenPlan()
	start, end := p.WindowStart, p.WindowEnd
	expected, err := ExpectedGoodUnits(p, start, end)
	if err != nil || expected.Cmp(big.NewRat(100, 1)) != 0 {
		t.Fatalf("expected %v, err=%v", expected, err)
	}
	actual, err := SumGoodUnitsDeltas(p, start, end, []Delta{{
		OrderID: orderID, AssetID: assetID, IntervalStart: start, IntervalEnd: end, GoodUnits: "80", Unit: "count",
	}})
	if err != nil || actual.Cmp(big.NewRat(80, 1)) != 0 {
		t.Fatalf("actual %v, err=%v", actual, err)
	}
	if got := new(big.Rat).Sub(expected, actual); got.Cmp(big.NewRat(20, 1)) != 0 {
		t.Fatalf("shortfall %v", got)
	}
}

func TestPlanRequiresCompleteRateCoverageAndAllowsZero(t *testing.T) {
	p := goldenPlan()
	p.PlannedBreaks = []Interval{{Start: base, End: "2026-09-27T00:01:40Z"}}
	p.RateSegments = []RateSegment{{Start: "2026-09-27T00:01:40Z", End: "2026-09-27T00:15:00Z", TargetGoodUnitsPerSecond: "0"}}
	if err := p.Validate(); err != nil {
		t.Fatalf("zero rate is valid: %v", err)
	}
	got, err := ExpectedGoodUnits(p, base, p.WindowEnd)
	if err != nil || got.Sign() != 0 {
		t.Fatalf("zero rate: %v %v", got, err)
	}
	p.PlannedBreaks = nil
	if err := p.Validate(); err == nil {
		t.Fatal("rate gap was accepted")
	}
}

func TestExactRatesAndHalfEven(t *testing.T) {
	p := goldenPlan()
	p.PlannedBreaks = nil
	p.RateSegments = []RateSegment{
		{Start: base, End: "2026-09-27T00:00:01Z", TargetGoodUnitsPerSecond: "0.1"},
		{Start: "2026-09-27T00:00:01Z", End: p.WindowEnd, TargetGoodUnitsPerSecond: "0.2"},
	}
	got, err := ExpectedGoodUnits(p, base, p.WindowEnd)
	if err != nil || got.Cmp(big.NewRat(1799, 10)) != 0 {
		t.Fatalf("exact expected=%v err=%v", got, err)
	}
	for value, want := range map[string]string{"1/2000000": "0.000000", "3/2000000": "0.000002", "5/2000000": "0.000002", "7/2000000": "0.000004"} {
		r, _ := new(big.Rat).SetString(value)
		got, err := Decimal6HalfEven(r)
		if err != nil || got != want {
			t.Fatalf("round %s = %q, %v; want %s", value, got, err, want)
		}
	}
}

func TestContextWindowMustBeAlignedAndCoveredByPlan(t *testing.T) {
	p := goldenPlan()
	for _, window := range [][2]string{
		{base, "2026-09-27T00:10:00Z"},
		{"2026-09-27T00:01:00Z", "2026-09-27T00:16:00Z"},
		{p.WindowEnd, "2026-09-27T00:30:00Z"},
	} {
		if _, err := ExpectedGoodUnits(p, window[0], window[1]); !errors.Is(err, ErrInvalidInterval) && !errors.Is(err, ErrPlanDoesNotCoverWindow) {
			t.Fatalf("invalid or uncovered context window %v: err=%v", window, err)
		}
	}
	if _, err := SumGoodUnitsDeltas(p, base, "2026-09-27T00:10:00Z", nil); !errors.Is(err, ErrInvalidInterval) {
		t.Fatalf("unaligned context window for observed counts: err=%v", err)
	}
	if _, err := ExpectedGoodUnits(p, p.WindowEnd, "2026-09-27T00:30:00Z"); !errors.Is(err, ErrPlanDoesNotCoverWindow) {
		t.Fatalf("no plan intersection error=%v", err)
	}
	partial := goldenPlan()
	partial.PlannedBreaks = nil
	partial.WindowStart = "2026-09-27T00:05:00Z"
	partial.RateSegments = []RateSegment{{Start: partial.WindowStart, End: partial.WindowEnd, TargetGoodUnitsPerSecond: "1"}}
	if _, err := ExpectedGoodUnits(partial, base, partial.WindowEnd); !errors.Is(err, ErrPlanDoesNotCoverWindow) {
		t.Fatalf("partial expected plan coverage error=%v", err)
	}
	fullDelta := Delta{OrderID: orderID, AssetID: assetID, IntervalStart: base, IntervalEnd: p.WindowEnd, GoodUnits: "0", Unit: "count"}
	if _, err := SumGoodUnitsDeltas(partial, base, partial.WindowEnd, []Delta{fullDelta}); !errors.Is(err, ErrPlanDoesNotCoverWindow) {
		t.Fatalf("partial observed plan coverage error=%v", err)
	}
}

func TestDeltaCoverageAndBoundaries(t *testing.T) {
	p := goldenPlan()
	start, end := p.WindowStart, p.WindowEnd
	if got, err := SumGoodUnitsDeltas(p, start, end, nil); !errors.Is(err, ErrInvalidDelta) || got != nil {
		t.Fatalf("missing observed deltas became an invented zero: %v err=%v", got, err)
	}
	valid := Delta{OrderID: orderID, AssetID: assetID, IntervalStart: start, IntervalEnd: end, GoodUnits: "80", Unit: "count"}
	if _, err := SumGoodUnitsDeltas(p, start, end, []Delta{valid}); err != nil {
		t.Fatalf("single interval crossing planned break: %v", err)
	}
	firstEnd := "2026-09-27T00:00:50Z"
	secondStart := "2026-09-27T00:01:20Z"
	first := valid
	first.IntervalEnd, first.GoodUnits = firstEnd, "40"
	second := valid
	second.IntervalStart, second.GoodUnits = secondStart, "40"
	if _, err := SumGoodUnitsDeltas(p, start, end, []Delta{first, second}); !errors.Is(err, ErrInvalidDelta) {
		t.Fatalf("productive gap: err=%v", err)
	}
	second.IntervalStart = firstEnd
	if got, err := SumGoodUnitsDeltas(p, start, end, []Delta{second, first}); err != nil || got.Cmp(big.NewRat(80, 1)) != 0 {
		t.Fatalf("contiguous disordered deltas = %v, err=%v; want 80", got, err)
	}
	for name, mutate := range map[string]func(*Delta){
		"other order": func(d *Delta) { d.OrderID = "cccccccc-cccc-4ccc-8ccc-cccccccccccc" },
		"other asset": func(d *Delta) { d.AssetID = "dddddddd-dddd-4ddd-8ddd-dddddddddddd" },
	} {
		foreign := valid
		mutate(&foreign)
		if _, err := SumGoodUnitsDeltas(p, start, end, []Delta{foreign}); !errors.Is(err, ErrInvalidDelta) {
			t.Fatalf("%s delta counted toward this plan: err=%v", name, err)
		}
	}
	second.IntervalStart = firstEnd
	second.IntervalEnd = "2026-09-27T00:01:00Z"
	if _, err := SumGoodUnitsDeltas(p, start, end, []Delta{first, second}); !errors.Is(err, ErrInvalidDelta) {
		t.Fatalf("overlap: err=%v", err)
	}
	first.IntervalStart = "2026-09-26T23:59:59Z"
	first.IntervalEnd = end
	if _, err := SumGoodUnitsDeltas(p, start, end, []Delta{first}); !errors.Is(err, ErrInvalidDelta) {
		t.Fatalf("delta crossing request boundary: err=%v", err)
	}
}

func TestPlanValidationRejectsNoncanonicalAndUntrustedInputs(t *testing.T) {
	basePlan := goldenPlan()
	cases := map[string]func(*Plan){
		"bad id":                func(p *Plan) { p.OrderID = strings.ToUpper(p.OrderID) },
		"missing approval":      func(p *Plan) { p.ApprovalRef = "" },
		"free-text approval":    func(p *Plan) { p.ApprovalRef = "approval-17" },
		"uppercase approval":    func(p *Plan) { p.ApprovalRef = strings.ToUpper(approvalID) },
		"revision":              func(p *Plan) { p.PlanRevision = 0 },
		"seven-digit rate":      func(p *Plan) { p.RateSegments[0].TargetGoodUnitsPerSecond = "1.1234567" },
		"unparseable timestamp": func(p *Plan) { p.WindowStart = "2026-09-27 00:00:00" },
		"scientific rate":       func(p *Plan) { p.RateSegments[0].TargetGoodUnitsPerSecond = "1e2" },
		"negative rate":         func(p *Plan) { p.RateSegments[0].TargetGoodUnitsPerSecond = "-1" },
		"leading zero":          func(p *Plan) { p.RateSegments[0].TargetGoodUnitsPerSecond = "01" },
		"overlap breaks": func(p *Plan) {
			p.PlannedBreaks = []Interval{{Start: "2026-09-27T00:01:00Z", End: "2026-09-27T00:02:00Z"}, {Start: "2026-09-27T00:01:30Z", End: "2026-09-27T00:03:00Z"}}
		},
		"break outside window": func(p *Plan) {
			p.PlannedBreaks = []Interval{{Start: "2026-09-27T00:14:00Z", End: "2026-09-27T00:16:00Z"}}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			p := basePlan
			p.RateSegments = append([]RateSegment(nil), basePlan.RateSegments...)
			mutate(&p)
			if err := p.Validate(); !errors.Is(err, ErrInvalidPlan) {
				t.Fatalf("invalid plan: err=%v", err)
			}
		})
	}
}

func TestStrictJSONAndNilSliceEncoding(t *testing.T) {
	valid := `{"order_id":"` + orderID + `","asset_id":"` + assetID + `","window_start":"2026-09-27T00:00:00Z","window_end":"2026-09-27T00:15:00Z","plan_revision":1,"approval_ref":"` + approvalID + `","unit":"count","rate_segments":[{"start":"2026-09-27T00:00:00Z","end":"2026-09-27T00:15:00Z","target_good_units_per_second":"1"}],"planned_breaks":[{"start":"2026-09-27T00:01:40Z","end":"2026-09-27T00:15:00Z"}]}`
	for _, wire := range []string{
		strings.Replace(valid, `"plan_revision":1`, `"plan_revision":1,"plan_revision":1`, 1),
		strings.Replace(valid, `"planned_breaks":[{`, `"planned_breaks":[{"start":"x","start":"x",`, 1),
		strings.TrimSuffix(valid, "}") + `,"unknown":true}`,
		valid + `{}`,
	} {
		if _, err := DecodePlan([]byte(wire)); err == nil {
			t.Fatalf("accepted invalid JSON %s", wire)
		}
	}
	var direct Plan
	if json.Unmarshal([]byte(strings.Replace(valid, `"plan_revision":1`, `"plan_revision":1,"plan_revision":1`, 1)), &direct) == nil {
		t.Fatal("ordinary json.Unmarshal bypassed strict contract checks")
	}
	var invalidUTF8 = []byte(valid)
	invalidUTF8[len(invalidUTF8)-2] = 0xff
	if _, err := DecodePlan(invalidUTF8); err == nil {
		t.Fatal("invalid UTF-8 was accepted")
	}
	p := Plan{RateSegments: nil, PlannedBreaks: nil}
	encoded, err := json.Marshal(p)
	if err != nil || !strings.Contains(string(encoded), `"rate_segments":[]`) || !strings.Contains(string(encoded), `"planned_breaks":[]`) {
		t.Fatalf("nil arrays should be stable JSON: %s, %v", encoded, err)
	}
}

func TestDeltaStrictDecodeAndValidation(t *testing.T) {
	wire := `{"order_id":"` + orderID + `","asset_id":"` + assetID + `","interval_start":"2026-09-27T00:00:00Z","interval_end":"2026-09-27T00:00:01Z","good_units":"0.25","unit":"count"}`
	got, err := DecodeDelta([]byte(wire))
	if err != nil || got.GoodUnits != "0.25" {
		t.Fatalf("decode delta=%+v err=%v", got, err)
	}
	for _, bad := range []string{
		strings.Replace(wire, `"unit":"count"`, `"unit":"count","unit":"count"`, 1),
		strings.TrimSuffix(wire, "}") + `,"source":"untrusted"}`,
		strings.Replace(wire, `"good_units":"0.25"`, `"good_units":"1e2"`, 1),
		strings.Replace(wire, `"good_units":"0.25"`, `"good_units":"-1"`, 1),
	} {
		if _, err := DecodeDelta([]byte(bad)); err == nil {
			t.Fatalf("accepted invalid delta %s", bad)
		}
	}
}

const (
	tenantA = "0a0a0a0a-0a0a-4a0a-8a0a-0a0a0a0a0a0a"
	tenantB = "0b0b0b0b-0b0b-4b0b-8b0b-0b0b0b0b0b0b"
	siteA   = "1a1a1a1a-1a1a-4a1a-8a1a-1a1a1a1a1a1a"
)

func TestCanonicalPlanPayloadDigestIsExactAndScoped(t *testing.T) {
	body := goldenPlan().Body()
	canonical, digest, err := CanonicalPlanPayload(tenantA, siteA, body)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"schema_version":1,"tenant_id":"` + tenantA + `","site_id":"` + siteA + `","plan":{"order_id":"` + orderID + `","asset_id":"` + assetID + `","window_start":"2026-09-27T00:00:00Z","window_end":"2026-09-27T00:15:00Z","plan_revision":1,"unit":"count","rate_segments":[{"start":"2026-09-27T00:00:00Z","end":"2026-09-27T00:15:00Z","target_good_units_per_second":"1"}],"planned_breaks":[{"start":"2026-09-27T00:01:40Z","end":"2026-09-27T00:15:00Z"}]}}`
	if string(canonical) != want {
		t.Fatalf("canonical payload drifted:\n got %s\nwant %s", canonical, want)
	}
	if len(digest) != 64 || strings.ToLower(digest) != digest {
		t.Fatalf("digest is not lowercase SHA-256 hex: %q", digest)
	}
	// Repeated computation and an approval_ref change never alter the digest.
	withOtherApproval := goldenPlan()
	withOtherApproval.ApprovalRef = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	if _, again, err := CanonicalPlanPayload(tenantA, siteA, withOtherApproval.Body()); err != nil || again != digest {
		t.Fatal("approval_ref leaked into the approved payload digest")
	}
	// Tenant and site are part of the digest: an approval is never portable across scopes.
	if _, other, err := CanonicalPlanPayload(tenantB, siteA, body); err != nil || other == digest {
		t.Fatalf("digest does not bind tenant (err=%v)", err)
	}
	if _, other, err := CanonicalPlanPayload(tenantA, tenantB, body); err != nil || other == digest {
		t.Fatalf("digest does not bind site (err=%v)", err)
	}
	mutations := map[string]func(*PlanBody){
		"order":         func(p *PlanBody) { p.OrderID = "cccccccc-cccc-4ccc-8ccc-cccccccccccc" },
		"asset":         func(p *PlanBody) { p.AssetID = "dddddddd-dddd-4ddd-8ddd-dddddddddddd" },
		"plan revision": func(p *PlanBody) { p.PlanRevision = 2 },
		"rate":          func(p *PlanBody) { p.RateSegments[0].TargetGoodUnitsPerSecond = "1.5" },
		"break":         func(p *PlanBody) { p.PlannedBreaks[0].Start = "2026-09-27T00:01:41Z" },
		"window": func(p *PlanBody) {
			p.WindowEnd = "2026-09-27T00:30:00Z"
			p.RateSegments[0].End = "2026-09-27T00:30:00Z"
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			p := goldenPlan().Body()
			p.RateSegments = append([]RateSegment(nil), p.RateSegments...)
			p.PlannedBreaks = append([]Interval(nil), p.PlannedBreaks...)
			mutate(&p)
			_, changed, err := CanonicalPlanPayload(tenantA, siteA, p)
			if err != nil {
				t.Fatal(err)
			}
			if changed == digest {
				t.Fatal("material plan change kept the approved digest")
			}
		})
	}
	invalid := goldenPlan().Body()
	invalid.Unit = "kg"
	if _, _, err := CanonicalPlanPayload(tenantA, siteA, invalid); err == nil {
		t.Fatal("invalid plan produced a digest")
	}
	if _, _, err := CanonicalPlanPayload("not-a-tenant", siteA, body); err == nil {
		t.Fatal("noncanonical tenant produced a digest")
	}
}

func TestPlanBodyStrictDecodeRejectsApprovalRef(t *testing.T) {
	wire, err := json.Marshal(goldenPlan().Body())
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodePlanBody(wire)
	if err != nil || decoded.PlanRevision != 1 {
		t.Fatalf("decode body=%+v err=%v", decoded, err)
	}
	// A plan submitted for review cannot carry the approval it is asking for.
	withRef := strings.TrimSuffix(string(wire), "}") + `,"approval_ref":"` + approvalID + `"}`
	if _, err = DecodePlanBody([]byte(withRef)); err == nil {
		t.Fatal("review payload accepted an approval_ref")
	}
	gap := goldenPlan().Body()
	gap.PlannedBreaks = nil
	gap.RateSegments = []RateSegment{{Start: base, End: "2026-09-27T00:10:00Z", TargetGoodUnitsPerSecond: "1"}}
	if gap.Validate() == nil {
		t.Fatal("rate gap over planned production was treated as known")
	}
	zero := goldenPlan().Body()
	zero.RateSegments[0].TargetGoodUnitsPerSecond = "0"
	if zero.Validate() != nil {
		t.Fatal("explicit zero rate for planned nonproduction must be valid")
	}
	if got := goldenPlan().Body().WithApproval(approvalID); got.Validate() != nil || got.ApprovalRef != approvalID || got.Body().PlanRevision != 1 {
		t.Fatalf("round trip lost fields: %+v", got)
	}
}

func TestProductiveSecondsAndNoPlannedProduction(t *testing.T) {
	p := goldenPlan().Body()
	got, err := ProductiveSeconds(p, p.WindowStart, p.WindowEnd)
	if err != nil || got.Cmp(big.NewRat(100, 1)) != 0 {
		t.Fatalf("productive=%v err=%v", got, err)
	}
	allBreak := goldenPlan().Body()
	allBreak.PlannedBreaks = []Interval{{Start: base, End: "2026-09-27T00:15:00Z"}}
	if got, err = ProductiveSeconds(allBreak, base, "2026-09-27T00:15:00Z"); err != nil || got.Sign() != 0 {
		t.Fatalf("whole-window break must be zero productive seconds: %v err=%v", got, err)
	}
	if _, err = ProductiveSeconds(p, base, "2026-09-27T00:30:00Z"); err == nil {
		t.Fatal("non-15-minute window accepted")
	}
	if _, err = ProductiveSeconds(p, "2026-09-27T00:15:00Z", "2026-09-27T00:30:00Z"); !errors.Is(err, ErrPlanDoesNotCoverWindow) {
		t.Fatalf("uncovered window: %v", err)
	}
}

func TestExactDecimalNeverRounds(t *testing.T) {
	for value, want := range map[*big.Rat]string{
		big.NewRat(100, 1):        "100",
		big.NewRat(0, 1):          "0",
		big.NewRat(1, 8):          "0.125",
		big.NewRat(123456789, 10): "12345678.9",
		big.NewRat(1, 1000000000): "0.000000001",
	} {
		if got, err := ExactDecimal(value); err != nil || got != want {
			t.Errorf("ExactDecimal(%v)=%q,%v want %q", value, got, err, want)
		}
	}
	if _, err := ExactDecimal(big.NewRat(1, 3)); err == nil {
		t.Fatal("1/3 has no finite expansion and must not be rounded")
	}
	if _, err := ExactDecimal(big.NewRat(-1, 2)); err == nil {
		t.Fatal("negative accepted")
	}
}

func TestShortfallThresholdGoldenCases(t *testing.T) {
	for _, c := range []struct {
		expected, deficit int64
		want              bool
	}{
		{100, 20, true}, // golden: 80 actual -> deficit 20 >= max(5,10)
		{100, 5, false}, // corrected 95 -> deficit 5 < 10
		{100, 10, true}, // boundary is inclusive
		{20, 5, true},   // max(5, 2) = 5
		{20, 4, false},  // below the absolute minimum
		{1000, 99, false},
		{0, 0, false},
	} {
		if got := WindowQualifies(big.NewRat(c.expected, 1), big.NewRat(c.deficit, 1)); got != c.want {
			t.Errorf("expected=%d deficit=%d qualifies=%v want %v", c.expected, c.deficit, got, c.want)
		}
	}
	if WindowQualifies(nil, big.NewRat(5, 1)) || WindowQualifies(big.NewRat(100, 1), nil) {
		t.Fatal("unknown values must never qualify")
	}
}

func TestDecimalsAreCappedAtSixPlacesAndTimesNormalizeToUTC(t *testing.T) {
	p := goldenPlan().Body()
	p.RateSegments[0].TargetGoodUnitsPerSecond = "1.123456"
	if err := p.Validate(); err != nil {
		t.Fatalf("six fractional digits must be accepted: %v", err)
	}
	// The same instant spelled with an offset normalizes to the UTC form and digests equally.
	withOffset := goldenPlan().Body()
	withOffset.WindowStart = "2026-09-27T05:30:00+05:30"
	withOffset.RateSegments[0].Start = "2026-09-27T05:30:00+05:30"
	withOffset.RateSegments[0].End = "2026-09-27T05:45:00+05:30"
	withOffset.WindowEnd = "2026-09-27T05:45:00+05:30"
	withOffset.PlannedBreaks = []Interval{{Start: "2026-09-27T05:31:40+05:30", End: "2026-09-27T05:45:00+05:30"}}
	utc := goldenPlan().Body()
	utc.WindowStart = "2026-09-27T00:00:00Z"
	utc.WindowEnd = "2026-09-27T00:15:00Z"
	utc.RateSegments = []RateSegment{{Start: "2026-09-27T00:00:00Z", End: "2026-09-27T00:15:00Z", TargetGoodUnitsPerSecond: "1"}}
	utc.PlannedBreaks = []Interval{{Start: "2026-09-27T00:01:40Z", End: "2026-09-27T00:15:00Z"}}
	_, a, err := CanonicalPlanPayload(tenantA, siteA, withOffset)
	if err != nil {
		t.Fatal(err)
	}
	_, b, err := CanonicalPlanPayload(tenantA, siteA, utc)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatal("offset spelling of the same instant changed the digest")
	}
}

func TestNormalizePayloadTimes(t *testing.T) {
	delta := map[string]any{"interval_start": "2026-09-27T05:30:00+05:30", "interval_end": "2026-09-27T00:15:00Z", "good_units": "5"}
	NormalizePayloadTimes("production_delta", delta)
	if delta["interval_start"] != "2026-09-27T00:00:00Z" || delta["interval_end"] != "2026-09-27T00:15:00Z" || delta["good_units"] != "5" {
		t.Fatalf("delta: %v", delta)
	}
	plan := map[string]any{
		"window_start":   "2026-09-27T01:00:00+01:00",
		"window_end":     "not a time",
		"rate_segments":  []any{map[string]any{"start": "2026-09-27T00:00:00-02:00", "end": "2026-09-27T03:00:00Z"}},
		"planned_breaks": []any{map[string]any{"start": "2026-09-27T05:30:00+05:30", "end": 7}},
	}
	NormalizePayloadTimes("production_plan", plan)
	segments := plan["rate_segments"].([]any)[0].(map[string]any)
	breaks := plan["planned_breaks"].([]any)[0].(map[string]any)
	if plan["window_start"] != "2026-09-27T00:00:00Z" || plan["window_end"] != "not a time" ||
		segments["start"] != "2026-09-27T02:00:00Z" || breaks["start"] != "2026-09-27T00:00:00Z" || breaks["end"] != 7 {
		t.Fatalf("plan: %v", plan)
	}
	other := map[string]any{"interval_start": "2026-09-27T05:30:00+05:30"}
	NormalizePayloadTimes("machine_state", other)
	if other["interval_start"] != "2026-09-27T05:30:00+05:30" {
		t.Fatal("non-production payloads must be left as sent")
	}
}
