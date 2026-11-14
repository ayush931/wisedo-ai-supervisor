package advisor

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/knowledge"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/recovery"
)

const (
	line     = "10000000-0000-4000-8000-000000000001"
	asset    = "10000000-0000-4000-8000-000000000002"
	order    = "10000000-0000-4000-8000-000000000003"
	op       = "10000000-0000-4000-8000-000000000004" // an operation the asset performs
	far      = "10000000-0000-4000-8000-000000000005" // unrelated to the case
	source   = "20000000-0000-4000-8000-000000000001"
	spanA    = "30000000-0000-4000-8000-00000000000a"
	spanB    = "30000000-0000-4000-8000-00000000000b"
	spanC    = "30000000-0000-4000-8000-00000000000c"
	claimA   = "40000000-0000-4000-8000-00000000000a" // asset cycle time (anchor)
	claimB   = "40000000-0000-4000-8000-00000000000b" // asset performs op (anchor relationship)
	claimC   = "40000000-0000-4000-8000-00000000000c" // op setup time (one hop)
	claimFar = "40000000-0000-4000-8000-00000000000f" // far cycle time (unrelated)
	claimD   = "40000000-0000-4000-8000-00000000000d" // feeder feeds the asset (incoming relationship)
	claimE   = "40000000-0000-4000-8000-00000000000e" // feeder cycle time (one hop, incoming)
	claimG   = "40000000-0000-4000-8000-000000000010" // op part of the line (second relationship to op)
	feeder   = "10000000-0000-4000-8000-000000000006"
	caseID   = "50000000-0000-4000-8000-000000000001"
	runID    = "60000000-0000-4000-8000-000000000001"
	machine  = "70000000-0000-4000-8000-000000000001"
)

func literal(v string) *knowledge.Literal {
	raw, _ := json.Marshal(v)
	return &knowledge.Literal{Type: "decimal", Value: raw, Unit: "s"}
}

// claim is in effect from before the case windows, with no end.
func claim(id, subject, predicate, object string, lit *knowledge.Literal, spans ...string) knowledge.SnapshotClaim {
	return knowledge.SnapshotClaim{ClaimInput: knowledge.ClaimInput{ClaimID: id, SubjectID: subject, Predicate: predicate, ObjectEntityID: object, Literal: lit, EffectiveFrom: "2026-01-01T00:00:00Z", SourceSpanIDs: spans}}
}

// effective sets claim id's validity in the input's snapshot.
func effective(in *Input, id, from string, to *string) {
	for i := range in.Snapshot.Contents.Claims {
		if in.Snapshot.Contents.Claims[i].ClaimID == id {
			in.Snapshot.Contents.Claims[i].EffectiveFrom, in.Snapshot.Contents.Claims[i].EffectiveTo = from, to
		}
	}
}

func ptr(s string) *string { return &s }

func measurements() []recovery.Measurement {
	return []recovery.Measurement{
		{ID: "80000000-0000-4000-8000-000000000001", ContextID: "90000000-0000-4000-8000-000000000001", MetricName: recovery.MetricExpected, Value: "75", Unit: "count", WindowStart: "2026-09-27T00:00:00Z", WindowEnd: "2026-09-27T00:15:00Z"},
		{ID: "80000000-0000-4000-8000-000000000002", ContextID: "90000000-0000-4000-8000-000000000001", MetricName: recovery.MetricDeficit, Value: "15", Unit: "count", WindowStart: "2026-09-27T00:00:00Z", WindowEnd: "2026-09-27T00:15:00Z"},
	}
}

// input is a READY case: an approved plan, a pinned snapshot with three related claims and one
// unrelated one, every passage available, and a stored investigation output with a hypothesis.
func input() Input {
	m := measurements()
	snap := &knowledge.Snapshot{SnapshotID: "a0000000-0000-4000-8000-000000000001", InventoryRevision: 4, CreatedAt: time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC), Contents: knowledge.SnapshotContents{
		Entities: []knowledge.SnapshotEntity{{EntityInput: knowledge.EntityInput{EntityID: asset, Name: "Press 1"}}, {EntityInput: knowledge.EntityInput{EntityID: op, Name: "Stamping"}}},
		Claims: []knowledge.SnapshotClaim{
			claim(claimFar, far, "cycle_time_seconds", "", literal("9"), spanA),
			claim(claimC, op, "setup_time_seconds", "", literal("300"), spanC),
			claim(claimB, asset, "performs", op, nil, spanB),
			claim(claimA, asset, "cycle_time_seconds", "", literal("60"), spanA),
		},
		Spans: []knowledge.SnapshotSpan{{SpanID: spanA, SourceID: source, Revision: 1, Start: 0, End: 10}, {SpanID: spanB, SourceID: source, Revision: 1, Start: 10, End: 20}, {SpanID: spanC, SourceID: source, Revision: 1, Start: 20, End: 30}},
	}}
	return Input{
		CaseID: caseID, CaseRevision: 3, LineID: line, OrderID: order, AssetID: asset,
		WindowStart: time.Date(2026, 9, 26, 23, 45, 0, 0, time.UTC), WindowEnd: time.Date(2026, 9, 27, 0, 15, 0, 0, time.UTC),
		Plan:         &PlanRefV1{PlanRevisionID: "b0000000-0000-4000-8000-000000000001", PlanRevision: 1, ApprovalID: "b0000000-0000-4000-8000-000000000002"},
		Measurements: m, Snapshot: snap,
		PassageText: map[string]string{spanA: "60 s cycle", spanB: "Press 1 st", spanC: "300 s setu"},
		Run: &Run{ID: runID, CaseRevision: 3, State: "completed", Status: recovery.StatusNeedsInput, Model: "fixture-model",
			Output: &recovery.RecoveryProposalV1{
				Hypotheses: []recovery.Hypothesis{{Text: "A stoppage on Press 1 may explain the deficit.", EvidenceIDs: []string{m[1].ID, machine}, ContradictingEvidenceIDs: []string{}}},
				Unknowns:   []recovery.Unknown{{Field: "stop_reason", Question: "What stopped the press?", Required: true}},
			},
			Captured: map[string]bool{m[0].ID: true, m[1].ID: true, machine: true, source: true}, Revoked: map[string]bool{}},
	}
}

func claimIDs(s SuggestionV1) []string {
	out := []string{}
	for _, f := range s.Facts {
		out = append(out, f.ClaimID)
	}
	return out
}

func TestBuild(t *testing.T) {
	for _, tc := range []struct {
		name       string
		edit       func(*Input)
		readiness  string
		reasons    []string
		facts      []string
		hypotheses int
		unknowns   int
	}{
		{"ready: related facts in claim order, server observations, cited hypothesis", func(*Input) {}, Ready, []string{}, []string{claimA, claimB, claimC}, 1, 1},
		{"WIS-629 no approved plan: NOT_READY and no statements", func(in *Input) { in.Plan, in.ContextNotEligible = nil, true },
			NotReady, []string{recovery.ReasonContextNotEligible, ReasonNoApprovedPlan}, []string{}, 0, 0},
		// Codex PR-228 finding 2: an earlier window no longer eligible, the plan still approved.
		{"a bound window not eligible: NOT_READY, the plan stays approved", func(in *Input) { in.ContextNotEligible = true },
			NotReady, []string{recovery.ReasonContextNotEligible}, []string{}, 0, 0},
		{"WIS-629 invented citation: every hypothesis and unknown is left out, facts stay", func(in *Input) {
			in.Run.Output.Hypotheses[0].EvidenceIDs = append(in.Run.Output.Hypotheses[0].EvidenceIDs, "ffffffff-0000-4000-8000-000000000000")
		}, Limited, []string{ReasonHypothesisCitationInvalid}, []string{claimA, claimB, claimC}, 0, 0},
		{"a captured source revoked since is not cited", func(in *Input) {
			in.Run.Output.Hypotheses[0].ContradictingEvidenceIDs = []string{source}
			in.Run.Revoked[source] = true
		}, Limited, []string{ReasonHypothesisCitationInvalid}, []string{claimA, claimB, claimC}, 0, 0},
		{"an unavailable passage leaves its fact out", func(in *Input) { delete(in.PassageText, spanC) }, Limited, []string{ReasonSourceUnavailable}, []string{claimA, claimB}, 1, 1},
		{"stale snapshot is labelled, still used", func(in *Input) { in.SnapshotStale = true }, Limited, []string{ReasonSnapshotStale}, []string{claimA, claimB, claimC}, 1, 1},
		{"no snapshot: observations only", func(in *Input) { in.Snapshot = nil }, Limited, []string{ReasonNoProcessSnapshot}, []string{}, 1, 1},
		{"pinned snapshot unreadable after a revocation: said so, no facts", func(in *Input) { in.Snapshot, in.SnapshotUnavailable = nil, true }, Limited, []string{ReasonSourceUnavailable}, []string{}, 1, 1},
		{"never investigated", func(in *Input) { in.Run = nil }, Limited, []string{ReasonInvestigationNotRun}, []string{claimA, claimB, claimC}, 0, 0},
		{"run stored no output: its reasons are shown", func(in *Input) {
			in.Run.Output, in.Run.Status, in.Run.Reasons = nil, recovery.StatusRefused, []string{recovery.ReasonSchemaInvalid}
		}, Limited, []string{ReasonInvestigationNoOutput, recovery.ReasonSchemaInvalid}, []string{claimA, claimB, claimC}, 0, 0},
		{"a run still going is not shown as failed", func(in *Input) { in.Run.Output, in.Run.State, in.Run.Status = nil, RunRunning, "" },
			Limited, []string{ReasonInvestigationRunning}, []string{claimA, claimB, claimC}, 0, 0},
		{"a cited machine-state record no longer current is not cited", func(in *Input) { in.Run.Revoked[machine] = true },
			Limited, []string{ReasonHypothesisCitationInvalid}, []string{claimA, claimB, claimC}, 0, 0},
		// Codex PR-228 finding 1: only claims in effect during the case windows
		// [2026-09-26T23:45Z, 2026-09-27T00:15Z) are approved facts.
		{"an expired claim is not a fact", func(in *Input) { effective(in, claimA, "2026-01-01T00:00:00Z", ptr("2026-09-26T23:45:00Z")) },
			Ready, []string{}, []string{claimB, claimC}, 1, 1},
		{"a future claim is not a fact", func(in *Input) { effective(in, claimA, "2026-09-27T00:15:00Z", nil) },
			Ready, []string{}, []string{claimB, claimC}, 1, 1},
		{"a claim in effect for part of the windows is a fact", func(in *Input) {
			effective(in, claimA, "2026-01-01T00:00:00Z", ptr("2026-09-26T23:46:00Z"))
			effective(in, claimC, "2026-09-27T00:14:00Z", nil)
		}, Ready, []string{}, []string{claimA, claimB, claimC}, 1, 1},
		{"a property reached through an expired relationship is not a fact", func(in *Input) {
			effective(in, claimB, "2026-01-01T00:00:00Z", ptr("2026-09-26T00:00:00Z"))
		}, Ready, []string{}, []string{claimA}, 1, 1},
		{"a property not in effect while its relationship is, is not a fact", func(in *Input) {
			effective(in, claimB, "2026-01-01T00:00:00Z", ptr("2026-09-27T00:00:00Z"))
			effective(in, claimC, "2026-09-27T00:00:00Z", nil)
		}, Ready, []string{}, []string{claimA, claimB}, 1, 1},
		{"no parseable case window: no fact, and the suggestion says why (rule 06)", func(in *Input) { in.WindowStart, in.WindowEnd = time.Time{}, time.Time{} },
			Limited, []string{ReasonCaseWindowUnknown}, []string{}, 1, 1},
		{"a property reached through an incoming relationship is a fact", func(in *Input) {
			in.Snapshot.Contents.Claims = append(in.Snapshot.Contents.Claims,
				claim(claimD, feeder, "feeds", asset, nil, spanB), claim(claimE, feeder, "cycle_time_seconds", "", literal("45"), spanA))
		}, Ready, []string{}, []string{claimA, claimB, claimC, claimD, claimE}, 1, 1},
		{"a property in effect only while the second of two relationships is, is a fact", func(in *Input) {
			in.Snapshot.Contents.Claims = append(in.Snapshot.Contents.Claims, claim(claimG, op, "part_of", line, nil, spanB))
			effective(in, claimB, "2026-01-01T00:00:00Z", ptr("2026-09-27T00:00:00Z"))
			effective(in, claimG, "2026-09-27T00:05:00Z", nil)
			effective(in, claimC, "2026-09-27T00:10:00Z", nil)
		}, Ready, []string{}, []string{claimA, claimB, claimC, claimG}, 1, 1},
		{"output from before the case was investigated again is labelled", func(in *Input) { in.Run.CaseRevision, in.Run.Reinvestigated = 2, true }, Limited, []string{ReasonInvestigationEarlier}, []string{claimA, claimB, claimC}, 1, 1},
		// WIS-631 regression: the run's own proposal moves the case on (proposed, awaiting approval);
		// that later case revision does not make the run that caused it earlier.
		{"a case moved on by the run's own proposal keeps it current", func(in *Input) { in.CaseRevision = 5 }, Ready, []string{}, []string{claimA, claimB, claimC}, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := input()
			tc.edit(&in)
			s := Build(in)
			if s.Readiness != tc.readiness || !slices.Equal(s.Reasons, tc.reasons) || !slices.Equal(claimIDs(s), tc.facts) || len(s.Hypotheses) != tc.hypotheses || len(s.Unknowns) != tc.unknowns {
				t.Fatalf("got %s %v facts %v hypotheses %d unknowns %d", s.Readiness, s.Reasons, claimIDs(s), len(s.Hypotheses), len(s.Unknowns))
			}
			if tc.readiness == NotReady && len(s.Observations) != 0 {
				t.Fatalf("NOT_READY carries observations: %+v", s.Observations)
			}
			if tc.readiness != NotReady && !slices.EqualFunc(s.Observations, in.Measurements, func(o ObservationV1, m recovery.Measurement) bool {
				return o.MeasurementID == m.ID && o.Value == m.Value
			}) {
				t.Fatalf("observations %+v", s.Observations)
			}
			if err := Validate(s, in); err != nil {
				t.Fatalf("Build returned a suggestion Validate refuses: %v", err)
			}
		})
	}
}

func TestBuildFactContent(t *testing.T) {
	s := Build(input())
	a := s.Facts[0]
	if a.EffectiveFrom != "2026-01-01T00:00:00Z" || a.EffectiveTo != nil || a.SubjectName != "Press 1" || a.Value == nil || *a.Value != "60" || a.Unit == nil || *a.Unit != "s" || !slices.Equal(a.SpanIDs, []string{spanA}) || s.Passages[0].SpanID != spanA || s.Passages[0].Text != "60 s cycle" || s.Passages[0].SourceID != source {
		t.Fatalf("fact %+v", a)
	}
	b := s.Facts[1]
	if b.ObjectID == nil || *b.ObjectID != op || *b.ObjectName != "Stamping" || b.Value != nil {
		t.Fatalf("relationship fact %+v", b)
	}
	if s.Snapshot == nil || s.Snapshot.CreatedAt != "2026-09-27T00:00:00Z" || s.Investigation == nil || s.Investigation.RunID != runID {
		t.Fatalf("refs %+v %+v", s.Snapshot, s.Investigation)
	}
}

func TestBuildTruncatesFacts(t *testing.T) {
	in := input()
	in.Snapshot.Contents.Claims = nil
	for i := range MaxFacts + 3 {
		in.Snapshot.Contents.Claims = append(in.Snapshot.Contents.Claims, claim(fmt.Sprintf("40000000-0000-4000-8000-%012d", 100+i), asset, "cycle_time_seconds", "", literal("60"), spanA))
	}
	s := Build(in)
	if len(s.Facts) != MaxFacts || !slices.Contains(s.Reasons, ReasonFactsTruncated) || s.Facts[0].ClaimID != "40000000-0000-4000-8000-000000000100" {
		t.Fatalf("got %d facts %v", len(s.Facts), s.Reasons)
	}
}

// WIS-629 defect: every fact embedded its passages, so 8 facts citing the same 4 whole-document
// spans carried 32 copies (~128 KB). Each cited span is now listed once and every fact's citation
// resolves to it.
func TestBuildListsEachCitedPassageOnce(t *testing.T) {
	in := input()
	in.Snapshot.Contents.Claims = nil
	for i := range 8 {
		in.Snapshot.Contents.Claims = append(in.Snapshot.Contents.Claims, claim(fmt.Sprintf("40000000-0000-4000-8000-%012d", 100+i), asset, "cycle_time_seconds", "", literal("60"), spanC, spanA, spanB))
	}
	s := Build(in)
	if s.Readiness != Ready || len(s.Facts) != 8 {
		t.Fatalf("got %s %v with %d facts", s.Readiness, s.Reasons, len(s.Facts))
	}
	listed := []string{}
	for _, p := range s.Passages {
		listed = append(listed, p.SpanID)
		if p.Text != in.PassageText[p.SpanID] || p.SourceID != source || p.Revision != 1 {
			t.Fatalf("passage %+v", p)
		}
	}
	if !slices.Equal(listed, []string{spanA, spanB, spanC}) {
		t.Fatalf("passages %v, want each cited span once", listed)
	}
	for _, f := range s.Facts {
		if !slices.Equal(f.SpanIDs, []string{spanA, spanB, spanC}) {
			t.Fatalf("fact %s cites %v", f.ClaimID, f.SpanIDs)
		}
	}
	body, _ := json.Marshal(s)
	if n := bytes.Count(body, []byte(`"60 s cycle"`)); n != 1 {
		t.Fatalf("passage text appears %d times in the body", n)
	}
}

// TestValidateRejects: a statement citing anything outside the input rejects the suggestion
// (WIS-629 acceptance: a suggestion citing an invented passage is rejected).
func TestValidateRejects(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*SuggestionV1)
	}{
		{"invented passage", func(s *SuggestionV1) { s.Facts[0].SpanIDs[0] = "ffffffff-0000-4000-8000-000000000000" }},
		{"a real span of another claim", func(s *SuggestionV1) { s.Facts[0].SpanIDs[0] = spanC }},
		{"passage text not the source's", func(s *SuggestionV1) { s.Passages[0].Text = "90 s cycle" }},
		// WIS-629 deduplicated passages: a cited span missing from the list, a passage listed twice
		// or listed with no fact citing it rejects, so no citation dangles and no text is smuggled.
		{"cited span not listed", func(s *SuggestionV1) { s.Passages = s.Passages[1:] }},
		{"passage listed twice", func(s *SuggestionV1) { s.Passages = append(s.Passages, s.Passages[0]) }},
		{"passage no fact cites", func(s *SuggestionV1) { s.Facts = s.Facts[1:] }},
		{"span cited twice by one fact", func(s *SuggestionV1) { s.Facts[0].SpanIDs = append(s.Facts[0].SpanIDs, s.Facts[0].SpanIDs[0]) }},
		{"invented claim", func(s *SuggestionV1) { s.Facts[0].ClaimID = "ffffffff-0000-4000-8000-000000000000" }},
		{"fact validity not the claim's", func(s *SuggestionV1) { s.Facts[0].EffectiveTo = ptr("2027-01-01T00:00:00Z") }},
		{"fact without a passage", func(s *SuggestionV1) { s.Facts[0].SpanIDs = nil }},
		{"observation value changed", func(s *SuggestionV1) { s.Observations[1].Value = "14" }},
		{"invented measurement", func(s *SuggestionV1) { s.Observations[0].MeasurementID = "ffffffff-0000-4000-8000-000000000000" }},
		{"hypothesis cites outside its run", func(s *SuggestionV1) { s.Hypotheses[0].EvidenceIDs = []string{"ffffffff-0000-4000-8000-000000000000"} }},
		{"other plan", func(s *SuggestionV1) {
			p := *s.Plan
			p.ApprovalID = "ffffffff-0000-4000-8000-000000000000"
			s.Plan = &p
		}},
		{"NOT_READY with statements", func(s *SuggestionV1) { s.Readiness = NotReady }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := input()
			s := Build(in)
			tc.edit(&s)
			if err := Validate(s, in); !errors.Is(err, ErrRejected) {
				t.Fatalf("Validate = %v, want ErrRejected", err)
			}
		})
	}
}

// TestValidateRejectsAgainstInput: Validate enforces the selection invariants against its input,
// not only the fields Build copies (Codex PR-228 follow-up): a fact for a claim not in effect
// during the case windows, and any statement while a bound window is not eligible, reject.
func TestValidateRejectsAgainstInput(t *testing.T) {
	for _, tc := range []struct {
		name  string
		edit  func(*SuggestionV1)
		input func(*Input)
	}{
		{"fact for a claim not in effect during the case windows", func(s *SuggestionV1) { s.Facts[0].EffectiveTo = ptr("2026-09-26T00:00:00Z") },
			func(in *Input) { effective(in, claimA, "2026-01-01T00:00:00Z", ptr("2026-09-26T00:00:00Z")) }},
		{"statements while a bound window is no longer eligible", func(*SuggestionV1) {}, func(in *Input) { in.ContextNotEligible = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := input()
			s := Build(in)
			tc.edit(&s)
			tc.input(&in)
			if err := Validate(s, in); !errors.Is(err, ErrRejected) {
				t.Fatalf("Validate = %v, want ErrRejected", err)
			}
		})
	}
}

// TestBuildRejectsWhatValidateRefuses: if assembly ever produced a citation outside its input,
// the suggestion is returned NOT_READY with nothing in it, never partially.
func TestBuildRejectsWhatValidateRefuses(t *testing.T) {
	in := input()
	in.Measurements = append(in.Measurements, recovery.Measurement{ID: "80000000-0000-4000-8000-000000000001", ContextID: "x", MetricName: "m", Value: "1"}) // duplicate ID, different value
	s := Build(in)
	if s.Readiness != NotReady || !slices.Equal(s.Reasons, []string{ReasonSuggestionRejected}) || len(s.Facts)+len(s.Observations)+len(s.Hypotheses) != 0 {
		t.Fatalf("got %s %v", s.Readiness, s.Reasons)
	}
}
