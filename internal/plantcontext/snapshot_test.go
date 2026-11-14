package plantcontext

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/production"
)

const (
	tenant   = "0a0a0a0a-0a0a-4a0a-8a0a-0a0a0a0a0a0a"
	site     = "1a1a1a1a-1a1a-4a1a-8a1a-1a1a1a1a1a1a"
	order    = "abcdefab-cdef-4abc-8def-abcdefabcdef"
	asset    = "22222222-2222-4222-8222-222222222222"
	source   = "5a5a5a5a-5a5a-4a5a-8a5a-5a5a5a5a5a5a"
	sourceB  = "6b6b6b6b-6b6b-4b6b-8b6b-6b6b6b6b6b6b"
	snapshot = "7c7c7c7c-7c7c-4c7c-8c7c-7c7c7c7c7c7c"
	approval = "8d8d8d8d-8d8d-4d8d-8d8d-8d8d8d8d8d8d"
	planRev  = "9e9e9e9e-9e9e-4e9e-8e9e-9e9e9e9e9e9e"
)

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

// goldenPlan: rate 1 unit/s over 30 minutes with 800-second planned breaks in each
// 15-minute window, so each window has 100 productive seconds and expects 100.
func goldenPlan() production.PlanBody {
	return production.PlanBody{
		OrderID: order, AssetID: asset, WindowStart: "2026-09-27T00:00:00Z", WindowEnd: "2026-09-27T00:30:00Z",
		PlanRevision: 1, Unit: "count",
		RateSegments:  []production.RateSegment{{Start: "2026-09-27T00:00:00Z", End: "2026-09-27T00:30:00Z", TargetGoodUnitsPerSecond: "1"}},
		PlannedBreaks: []production.Interval{{Start: "2026-09-27T00:01:40Z", End: "2026-09-27T00:15:00Z"}, {Start: "2026-09-27T00:16:40Z", End: "2026-09-27T00:30:00Z"}},
	}
}

func delta(sequence, revision int64, key, start, end, good string) DeltaRecord {
	payload, _ := json.Marshal(production.Delta{OrderID: order, AssetID: asset, IntervalStart: start, IntervalEnd: end, GoodUnits: good, Unit: "count"})
	return DeltaRecord{
		Ref:     RecordRef{SourceID: source, SourceSequence: sequence, Kind: "production_delta", ExternalKey: key, Revision: revision, SHA256: strings.Repeat("a", 63) + string(rune('0'+sequence%10))},
		Payload: payload, AssetEntityID: asset, OrderEntityID: order,
	}
}

func readySource(complete int64, heartbeat time.Time, through string) SourceInput {
	mark, _ := json.Marshal(watermarkPayload{CompleteThrough: through, ThroughSequence: complete - 1})
	return SourceInput{
		SourceID: source, SourceRevision: 2, MappingRevision: 2, AccessPolicyRevision: 1,
		Approved: true, ScopeAllowsDeltas: true, FreshnessSeconds: 120,
		Progress: &ProgressInput{
			CompleteSequence: complete, ClockQuality: "ok", LastHeartbeatReceivedAt: &heartbeat,
			Watermark: &WatermarkInput{Ref: RecordRef{SourceID: source, SourceSequence: complete, Kind: "watermark", ExternalKey: "wm", Revision: 1, SHA256: strings.Repeat("b", 64)}, Payload: mark},
		},
	}
}

// window1 returns the golden first window: expected 100, contiguous actual 80.
func window1() Inputs {
	return Inputs{
		OrderID: order, AssetID: asset, Window: Window{"2026-09-27T00:00:00Z", "2026-09-27T00:15:00Z"},
		ProcessSnapshotID: snapshot, ProcessSnapshotSHA256: strings.Repeat("c", 64),
		Plan:        &PlanInput{Ref: PlanRef{PlanRevisionID: planRev, PlanRevision: 1, ApprovalID: approval, CanonicalPayloadDigest: strings.Repeat("d", 64)}, Body: goldenPlan()},
		Deltas:      []DeltaRecord{delta(1, 1, "d-1", "2026-09-27T00:00:00Z", "2026-09-27T00:15:00Z", "80")},
		Sources:     map[string]SourceInput{source: readySource(2, at("2026-09-27T00:17:00Z"), "2026-09-27T00:15:00Z")},
		EvaluatedAt: at("2026-09-27T00:17:01Z"),
	}
}

func window2(good string) Inputs {
	in := window1()
	in.Window = Window{"2026-09-27T00:15:00Z", "2026-09-27T00:30:00Z"}
	in.Deltas = []DeltaRecord{delta(3, 1, "d-2", "2026-09-27T00:15:00Z", "2026-09-27T00:30:00Z", good)}
	in.Sources = map[string]SourceInput{source: readySource(4, at("2026-09-27T00:32:00Z"), "2026-09-27T00:30:00Z")}
	in.EvaluatedAt = at("2026-09-27T00:32:01Z")
	return in
}

func assemble(t *testing.T, in Inputs) Snapshot {
	t.Helper()
	s, err := Assemble(tenant, site, in)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func value(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

func TestGoldenWindowIsReadyWithExactShortfall(t *testing.T) {
	s := assemble(t, window1())
	if s.Readiness != ReadyState || len(s.ReasonCodes) != 0 {
		t.Fatalf("readiness=%s reasons=%v", s.Readiness, s.ReasonCodes)
	}
	if value(s.ExpectedGoodUnits) != "100.000000" || value(s.ActualGoodUnits) != "80.000000" || value(s.DeficitGoodUnits) != "20.000000" {
		t.Fatalf("expected=%s actual=%s deficit=%s", value(s.ExpectedGoodUnits), value(s.ActualGoodUnits), value(s.DeficitGoodUnits))
	}
	if value(s.ExpectedExact) != "100" || value(s.ActualExact) != "80" || value(s.DeficitExact) != "20" {
		t.Fatal("exact values not preserved")
	}
	if len(s.SourceRecordRefs) != 2 || s.SourceRecordRefs[0].Kind != "production_delta" || s.SourceRecordRefs[1].Kind != "watermark" ||
		len(s.MappingRevisionIDs) != 1 || len(s.SourcePolicyRevisionIDs) != 1 || s.ApprovedPlanRevision.ApprovalID != approval {
		t.Fatalf("provenance not pinned: %+v", s)
	}
	if s.Revision != 1 || s.FormulaVersion != FormulaVersion || s.ReadinessPolicyRevision != ReadinessPolicyRevision || s.Unit != "count" {
		t.Fatalf("versions missing: %+v", s)
	}
}

func TestDigestIgnoresCreationTimeButNotInputs(t *testing.T) {
	first := assemble(t, window1())
	later := window1()
	later.EvaluatedAt = at("2026-09-27T00:18:30Z")
	heartbeat := at("2026-09-27T00:18:00Z")
	later.Sources[source].Progress.LastHeartbeatReceivedAt = &heartbeat
	if again := assemble(t, later); again.Digest != first.Digest {
		t.Fatal("identical inputs at a different evaluation time changed the digest")
	}
	changes := map[string]func(*Inputs){
		"correction": func(in *Inputs) {
			in.Deltas = append(in.Deltas, delta(5, 2, "d-1", "2026-09-27T00:00:00Z", "2026-09-27T00:15:00Z", "95"))
			in.Sources = map[string]SourceInput{source: readySource(6, at("2026-09-27T00:17:00Z"), "2026-09-27T00:15:00Z")}
		},
		"plan revision": func(in *Inputs) { in.Plan.Ref.PlanRevision = 2 },
		"process snapshot": func(in *Inputs) {
			in.ProcessSnapshotSHA256 = strings.Repeat("e", 64)
		},
		"mapping revision": func(in *Inputs) {
			s := in.Sources[source]
			s.MappingRevision = 3
			in.Sources[source] = s
		},
		"policy revision": func(in *Inputs) {
			s := in.Sources[source]
			s.AccessPolicyRevision = 2
			in.Sources[source] = s
		},
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			in := window1()
			change(&in)
			if assemble(t, in).Digest == first.Digest {
				t.Fatal("changed input revision kept the digest")
			}
		})
	}
	if other := assemble(t, window1()); other.Digest != first.Digest {
		t.Fatal("assembling the same inputs twice changed the digest")
	}
	other := "3f3f3f3f-3f3f-4f3f-8f3f-3f3f3f3f3f3f"
	if s, err := Assemble(tenant, other, window1()); err != nil || s.Digest == first.Digest {
		t.Fatalf("digest does not bind the site (err=%v)", err)
	}
	if s, err := Assemble(other, site, window1()); err != nil || s.Digest == first.Digest {
		t.Fatalf("digest does not bind the tenant (err=%v)", err)
	}
}

func TestCorrectionReplacesContributionAndShortfallRule(t *testing.T) {
	corrected := window1()
	corrected.Deltas = append(corrected.Deltas, delta(5, 2, "d-1", "2026-09-27T00:00:00Z", "2026-09-27T00:15:00Z", "95"))
	corrected.Sources = map[string]SourceInput{source: readySource(6, at("2026-09-27T00:17:00Z"), "2026-09-27T00:15:00Z")}
	c := assemble(t, corrected)
	if c.Readiness != ReadyState || value(c.ActualExact) != "95" || value(c.DeficitExact) != "5" {
		t.Fatalf("correction: readiness=%s actual=%s deficit=%s reasons=%v", c.Readiness, value(c.ActualExact), value(c.DeficitExact), c.ReasonCodes)
	}
	first, second := assemble(t, window1()), assemble(t, window2("80"))
	if r := EvaluateShortfall(first, second); !r.Qualifies || r.RuleVersion != production.ShortfallRuleVersion {
		t.Fatalf("two golden windows must qualify: %+v", r)
	}
	if r := EvaluateShortfall(c, assemble(t, window2("80"))); r.Qualifies || r.Reason != "BELOW_THRESHOLD" {
		t.Fatalf("corrected 95 must not qualify: %+v", r)
	}
	if r := EvaluateShortfall(second, first); r.Qualifies || r.Reason != "NOT_CONSECUTIVE" {
		t.Fatalf("reversed windows qualified: %+v", r)
	}
	offline := window2("80")
	offline.Sources[source].Progress.LastHeartbeatReceivedAt = nil
	if r := EvaluateShortfall(first, assemble(t, offline)); r.Qualifies || r.Reason != "WINDOW_NOT_READY" {
		t.Fatalf("incomplete window qualified: %+v", r)
	}
	other := window2("80")
	other.AssetID = sourceB
	if r := EvaluateShortfall(first, assemble(t, other)); r.Qualifies || r.Reason != "DIFFERENT_SCOPE" {
		t.Fatalf("different asset qualified: %+v", r)
	}
}

func TestNotReadyReasons(t *testing.T) {
	cases := map[string]struct {
		mutate func(*Inputs)
		reason string
	}{
		"not elapsed":     {func(in *Inputs) { in.EvaluatedAt = at("2026-09-27T00:16:59Z") }, "WINDOW_NOT_ELAPSED"},
		"no plan":         {func(in *Inputs) { in.Plan, in.PlanReason = nil, "" }, "NO_APPROVED_PLAN"},
		"plan superseded": {func(in *Inputs) { in.Plan, in.PlanReason = nil, "PLAN_SUPERSEDED" }, "PLAN_SUPERSEDED"},
		"plan not covering": {func(in *Inputs) {
			in.Window = Window{"2026-09-27T00:30:00Z", "2026-09-27T00:45:00Z"}
			in.EvaluatedAt = at("2026-09-27T00:50:00Z")
		}, "PLAN_NOT_COVERING_WINDOW"},
		"no heartbeat": {func(in *Inputs) { in.Sources[source].Progress.LastHeartbeatReceivedAt = nil }, "NO_HEARTBEAT"},
		"stale": {func(in *Inputs) {
			hb := at("2026-09-27T00:12:59Z")
			in.Sources[source].Progress.LastHeartbeatReceivedAt = &hb
		}, "HEARTBEAT_STALE"},
		"clock suspect": {func(in *Inputs) { in.Sources[source].Progress.ClockQuality = "suspect" }, "CLOCK_SUSPECT"},
		"clock unknown": {func(in *Inputs) { in.Sources[source].Progress.ClockQuality = "unknown" }, "CLOCK_UNKNOWN"},
		"no watermark":  {func(in *Inputs) { in.Sources[source].Progress.Watermark = nil }, "NO_WATERMARK"},
		"watermark behind": {func(in *Inputs) {
			in.Sources = map[string]SourceInput{source: readySource(2, at("2026-09-27T00:17:00Z"), "2026-09-27T00:14:59Z")}
		}, "WATERMARK_BEHIND"},
		"watermark ahead of sequence": {func(in *Inputs) {
			in.Sources[source].Progress.Watermark.Payload = []byte(`{"complete_through":"2026-09-27T00:15:00Z","through_sequence":5}`)
		}, "WATERMARK_INVALID"},
		"incomplete sequence": {func(in *Inputs) { in.Sources[source].Progress.CompleteSequence = 0 }, "INCOMPLETE_SEQUENCE"},
		"not processed":       {func(in *Inputs) { s := in.Sources[source]; s.Progress = nil; in.Sources[source] = s }, "NOT_PROCESSED"},
		"source not approved": {func(in *Inputs) { s := in.Sources[source]; s.Approved = false; in.Sources[source] = s }, "SOURCE_NOT_APPROVED"},
		"source out of scope": {func(in *Inputs) { s := in.Sources[source]; s.ScopeAllowsDeltas = false; in.Sources[source] = s }, "SOURCE_OUT_OF_SCOPE"},
		"missing actuals":     {func(in *Inputs) { in.Deltas = nil }, "MISSING_ACTUALS"},
		"partial coverage": {func(in *Inputs) {
			in.Deltas = []DeltaRecord{delta(1, 1, "d-1", "2026-09-27T00:00:00Z", "2026-09-27T00:01:00Z", "60")}
		}, "MISSING_ACTUALS"},
		"boundary": {func(in *Inputs) {
			in.Deltas = []DeltaRecord{delta(1, 1, "d-1", "2026-09-27T00:00:00Z", "2026-09-27T00:20:00Z", "80")}
		}, "BOUNDARY_NOT_ALIGNED"},
		"overlap": {func(in *Inputs) {
			in.Deltas = []DeltaRecord{delta(1, 1, "a", "2026-09-27T00:00:00Z", "2026-09-27T00:10:00Z", "50"), delta(2, 1, "b", "2026-09-27T00:05:00Z", "2026-09-27T00:15:00Z", "30")}
			in.Sources = map[string]SourceInput{source: readySource(3, at("2026-09-27T00:17:00Z"), "2026-09-27T00:15:00Z")}
		}, "DELTA_OVERLAP"},
		"correction gap": {func(in *Inputs) {
			in.Deltas = append(in.Deltas, delta(2, 3, "d-1", "2026-09-27T00:00:00Z", "2026-09-27T00:15:00Z", "95"))
			in.Sources = map[string]SourceInput{source: readySource(3, at("2026-09-27T00:17:00Z"), "2026-09-27T00:15:00Z")}
		}, "CORRECTION_GAP"},
		"mapping mismatch": {func(in *Inputs) { in.Deltas[0].AssetEntityID = sourceB }, "MAPPING_MISMATCH"},
		"invalid delta":    {func(in *Inputs) { in.Deltas[0].Payload = []byte(`{"good_units":"1e2"}`) }, "INVALID_DELTA"},
		"two sources": {func(in *Inputs) {
			other := delta(1, 1, "x", "2026-09-27T00:00:00Z", "2026-09-27T00:01:00Z", "10")
			other.Ref.SourceID = sourceB
			in.Deltas = append(in.Deltas, other)
			s := readySource(2, at("2026-09-27T00:17:00Z"), "2026-09-27T00:15:00Z")
			s.SourceID = sourceB
			in.Sources[sourceB] = s
		}, "SOURCE_CONFLICT"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			in := window1()
			c.mutate(&in)
			s := assemble(t, in)
			found := false
			for _, code := range s.ReasonCodes {
				found = found || code == c.reason
			}
			if s.Readiness != NotReady || !found {
				t.Fatalf("readiness=%s reasons=%v want %s", s.Readiness, s.ReasonCodes, c.reason)
			}
			// Unknown actuals are never presented as zero.
			if s.ActualGoodUnits != nil || s.DeficitGoodUnits != nil {
				t.Fatalf("NOT_READY exposed actual=%s deficit=%s", value(s.ActualGoodUnits), value(s.DeficitGoodUnits))
			}
		})
	}
}

func TestNoPlannedProductionIsLimitedNotAnError(t *testing.T) {
	in := window1()
	in.Plan.Body.PlannedBreaks = []production.Interval{{Start: "2026-09-27T00:00:00Z", End: "2026-09-27T00:15:00Z"}, {Start: "2026-09-27T00:16:40Z", End: "2026-09-27T00:30:00Z"}}
	in.Deltas = nil
	s := assemble(t, in)
	if s.Readiness != LimitedState || strings.Join(s.ReasonCodes, ",") != "NO_PLANNED_PRODUCTION" || value(s.ExpectedExact) != "0" || s.ActualGoodUnits != nil {
		t.Fatalf("no planned production: %+v", s)
	}
	if r := EvaluateShortfall(s, assemble(t, window2("80"))); r.Qualifies {
		t.Fatal("a window without planned production qualified a shortfall")
	}
	in.EvaluatedAt = at("2026-09-27T00:16:00Z")
	if s = assemble(t, in); s.Readiness != NotReady {
		t.Fatalf("an unelapsed window must stay NOT_READY: %+v", s)
	}
}

func TestAssembleRejectsNonAlignedWindows(t *testing.T) {
	for _, w := range []Window{
		{"2026-09-27T00:01:00Z", "2026-09-27T00:16:00Z"},
		{"2026-09-27T00:00:00Z", "2026-09-27T00:30:00Z"},
	} {
		in := window1()
		in.Window = w
		if _, err := Assemble(tenant, site, in); err == nil {
			t.Errorf("window %+v accepted", w)
		}
	}
}

func TestOverProductionHasZeroDeficitNeverNegative(t *testing.T) {
	in := window1()
	in.Deltas = []DeltaRecord{delta(1, 1, "d-1", "2026-09-27T00:00:00Z", "2026-09-27T00:15:00Z", "120.5")}
	s := assemble(t, in)
	if s.Readiness != ReadyState || value(s.ActualExact) != "120.5" || value(s.DeficitExact) != "0" || value(s.DeficitGoodUnits) != "0.000000" {
		t.Fatalf("over-production: actual=%s deficit=%s", value(s.ActualExact), value(s.DeficitExact))
	}
}

func TestReviewFindingsFailClosed(t *testing.T) {
	// A source configured with a long freshness never stretches readiness past 120s.
	in := window1()
	s := in.Sources[source]
	s.FreshnessSeconds = 3600
	// The last heartbeat came 3 minutes before the window ended: older than the 120s cap even
	// though the source allows 3600s.
	old := at("2026-09-27T00:12:00Z")
	s.Progress.LastHeartbeatReceivedAt = &old
	in.Sources[source] = s
	if got := assemble(t, in); got.Readiness != NotReady || !strings.Contains(strings.Join(got.ReasonCodes, ","), "HEARTBEAT_STALE") {
		t.Fatalf("heartbeat 3 minutes before window end with freshness 3600: %s %v", got.Readiness, got.ReasonCodes)
	}
	// A quarantined correction blocks its chain instead of reviving revision 1.
	in = window1()
	quarantined := delta(5, 2, "d-1", "2026-09-27T00:00:00Z", "2026-09-27T00:15:00Z", "95")
	quarantined.Quarantine = "UNKNOWN_MAPPING"
	in.Deltas = append(in.Deltas, quarantined)
	// Completeness and the watermark cover the quarantined record: only the quarantine
	// itself can keep this window from READY with the stale revision-1 value.
	in.Sources = map[string]SourceInput{source: readySource(6, at("2026-09-27T00:17:00Z"), "2026-09-27T00:15:00Z")}
	if got := assemble(t, in); got.Readiness != NotReady || !strings.Contains(strings.Join(got.ReasonCodes, ","), "CORRECTION_QUARANTINED") || got.ActualGoodUnits != nil {
		t.Fatalf("quarantined correction: %s %v actual=%s", got.Readiness, got.ReasonCodes, value(got.ActualGoodUnits))
	}
	// A correction that moves a fact to another window removes it from this one.
	in = window1()
	in.Deltas = append(in.Deltas, delta(5, 2, "d-1", "2026-09-27T00:15:00Z", "2026-09-27T00:30:00Z", "80"))
	got := assemble(t, in)
	if len(got.DeltaRefs()) != 0 || !strings.Contains(strings.Join(got.ReasonCodes, ","), "MISSING_ACTUALS") {
		t.Fatalf("moved correction: refs=%v reasons=%v", got.DeltaRefs(), got.ReasonCodes)
	}
	// Windows under different process snapshots never combine into one shortfall.
	first, second := assemble(t, window1()), assemble(t, window2("80"))
	second.ProcessSnapshotID = "4d4d4d4d-4d4d-4d4d-8d4d-4d4d4d4d4d4d"
	if r := EvaluateShortfall(first, second); r.Qualifies || r.Reason != "DIFFERENT_PROCESS_SNAPSHOT" {
		t.Fatalf("different process snapshots: %+v", r)
	}
}

// An offset spelling of an aligned window is the same window: it normalizes to UTC and
// produces the same digest as the Z form.
func TestOffsetWindowNormalizesToUTCDigest(t *testing.T) {
	utc := window1()
	offset := window1()
	offset.Window = Window{"2026-09-27T05:30:00+05:30", "2026-09-27T05:45:00+05:30"}
	a := assemble(t, utc)
	b, err := Assemble(tenant, site, offset)
	if err != nil {
		t.Fatal(err)
	}
	if b.Window.Start != "2026-09-27T00:00:00Z" || b.Window.End != "2026-09-27T00:15:00Z" || a.Digest != b.Digest {
		t.Fatalf("offset window: start=%s end=%s digest equal=%v", b.Window.Start, b.Window.End, a.Digest == b.Digest)
	}
}

// record_id is derived from (source_id, source_sequence), so a context stored before it
// existed still matches the same records; any change to identity or content does not.
func TestSameRefsIgnoresOnlyTheDerivedRecordID(t *testing.T) {
	ref := RecordRef{RecordID: "11111111-1111-4111-8111-111111111111", SourceID: "s", SourceSequence: 3, Kind: "production_delta", ExternalKey: "k", Revision: 1, SHA256: "aa"}
	old := ref
	old.RecordID = ""
	if !sameRefs([]RecordRef{ref}, []RecordRef{old}) {
		t.Fatal("a pre-record_id context no longer matches its own records")
	}
	for name, change := range map[string]func(*RecordRef){
		"sequence": func(r *RecordRef) { r.SourceSequence++ },
		"source":   func(r *RecordRef) { r.SourceID = "t" },
		"content":  func(r *RecordRef) { r.SHA256 = "bb" },
		"revision": func(r *RecordRef) { r.Revision++ },
	} {
		other := ref
		change(&other)
		if sameRefs([]RecordRef{ref}, []RecordRef{other}) {
			t.Errorf("%s change still matched", name)
		}
	}
}

// Readiness and digest do not depend on when the context is requested: a source that was
// live at the window's end and then stopped heartbeating (the shift ended) still gives the
// same READY context an hour later.
func TestHeartbeatIsJudgedAtWindowEndNotRequestTime(t *testing.T) {
	soon := window1()
	late := window1()
	late.EvaluatedAt = at("2026-09-27T01:17:01Z")
	a, b := assemble(t, soon), assemble(t, late)
	if a.Readiness != ReadyState || b.Readiness != ReadyState || a.Digest != b.Digest {
		t.Fatalf("soon %s %s, late %s %s %v", a.Readiness, a.Digest, b.Readiness, b.Digest, b.ReasonCodes)
	}
	// Exactly at the limit before the window end is still live; one second earlier is not.
	edge := window1()
	hb := at("2026-09-27T00:13:00Z")
	edge.Sources[source].Progress.LastHeartbeatReceivedAt = &hb
	if got := assemble(t, edge); got.Readiness != ReadyState {
		t.Fatalf("heartbeat exactly 120s before the end: %s %v", got.Readiness, got.ReasonCodes)
	}
	hb = hb.Add(-time.Second)
	if got := assemble(t, edge); !strings.Contains(strings.Join(got.ReasonCodes, ","), "HEARTBEAT_STALE") {
		t.Fatalf("heartbeat 121s before the end: %s %v", got.Readiness, got.ReasonCodes)
	}
}
