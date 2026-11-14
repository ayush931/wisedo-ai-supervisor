package recovery

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/plantcontext"
)

// The WIS-276 deterministic fixture pack: two 15-minute windows, expected 75 units each,
// actual 60 and 60 (deficit 15/15, a valid two-window case); an approved line -> machine graph,
// machine stoppage evidence and a supervisor-confirmed maintenance assignee. Every ID is fixed.
const (
	caseID     = "00000000-0000-4000-8000-000000000276"
	lineID     = "00000000-0000-4000-8000-000000000001"
	orderID    = "00000000-0000-4000-8000-000000000002"
	ctxA       = "00000000-0000-4000-8000-00000000000a"
	ctxB       = "00000000-0000-4000-8000-00000000000b"
	deltaA     = "00000000-0000-4000-8000-0000000000d1"
	deltaB     = "00000000-0000-4000-8000-0000000000d2"
	stoppage   = "00000000-0000-4000-8000-0000000000f1"
	materials  = "00000000-0000-4000-8000-0000000000e1"
	assignee   = "00000000-0000-4000-8000-0000000000a1"
	proposalID = "00000000-0000-4000-8000-0000000000c1"
	processID  = "00000000-0000-4000-8000-0000000000b1"
)

var (
	now  = time.Date(2026, 10, 23, 6, 45, 0, 0, time.UTC)
	plan = plantcontext.PlanRef{PlanRevisionID: "00000000-0000-4000-8000-0000000000e9", PlanRevision: 3, ApprovalID: "00000000-0000-4000-8000-0000000000ea", CanonicalPayloadDigest: strings.Repeat("a", 64)}
)

func str(v string) *string { return &v }

func window(id, start, end string) plantcontext.Snapshot {
	planRef := plan
	return plantcontext.Snapshot{
		ContextID: id, Revision: 1, OrderID: orderID, AssetID: lineID, Window: plantcontext.Window{Start: start, End: end},
		ApprovedPlanRevision: &planRef, ProcessSnapshotID: processID, Readiness: plantcontext.ReadyState, ReasonCodes: []string{},
		ExpectedGoodUnits: str("75.000000"), ActualGoodUnits: str("60.000000"), DeficitGoodUnits: str("15.000000"),
		ExpectedExact: str("75"), ActualExact: str("60"), DeficitExact: str("15"), Unit: "count",
	}
}

func fixture() Input {
	current := plan
	return Input{
		Case:        CaseRef{ID: caseID, Revision: 2, State: "investigating", LineID: lineID, OrderID: orderID},
		Contexts:    []ContextView{{window(ctxA, "2026-10-23T06:00:00Z", "2026-10-23T06:15:00Z"), true}, {window(ctxB, "2026-10-23T06:15:00Z", "2026-10-23T06:30:00Z"), true}},
		CurrentPlan: &current,
		Evidence:    []Evidence{{deltaA, EvidenceProduction}, {deltaB, EvidenceProduction}, {stoppage, EvidenceMachineFault}, {materials, EvidenceMaterials}},
		Roster:      []Assignee{{ID: assignee, LineID: lineID, ConfirmedBy: "supervisor"}},
		ModeEnabled: true, PolicyVersion: 1, GraphVersion: 4, Now: now,
	}
}

// golden is the specialist output the fixture expects: exact deficits, a cautious hypothesis
// and an inspect/report proposal for the confirmed assignee.
func golden() RecoveryProposalV1 {
	return RecoveryProposalV1{
		ProposalID: proposalID, CaseID: caseID, CaseRevision: 2, ContextID: ctxB, ContextRevision: 1,
		Observations: []Observation{
			{MeasurementID: MeasurementID(ctxA, MetricDeficit), EvidenceIDs: []string{deltaA}, MetricName: MetricDeficit, ValueDecimal: "15.000000", Unit: "count", WindowStart: "2026-10-23T06:00:00Z", WindowEnd: "2026-10-23T06:15:00Z"},
			{MeasurementID: MeasurementID(ctxB, MetricDeficit), EvidenceIDs: []string{deltaB}, MetricName: MetricDeficit, ValueDecimal: "15.000000", Unit: "count", WindowStart: "2026-10-23T06:15:00Z", WindowEnd: "2026-10-23T06:30:00Z"},
		},
		Hypotheses: []Hypothesis{{Text: "Hypothesis: the recorded stoppage on the press may have contributed to the shortfall.", EvidenceIDs: []string{stoppage}, ContradictingEvidenceIDs: []string{}}},
		Unknowns:   []Unknown{{Field: "stoppage.cause", Question: "What caused the recorded stoppage?", Required: false}},
		Options: []Option{
			{Kind: OptionMaintenance, Text: "Ask maintenance to inspect the press and report findings.", EvidenceIDs: []string{stoppage}},
			{Kind: OptionClarify, Text: "Ask the shift lead whether the stoppage was planned.", EvidenceIDs: []string{}},
		},
		SelectedOption: OptionMaintenance,
		TaskPayload: &TaskPayload{Kind: TaskKind, AssetID: lineID, LineID: lineID, OrderID: orderID, AssigneeID: assignee, Template: TaskTemplate,
			Description: "Inspect the press under customer procedures and report findings. Do not adjust equipment, release quality holds or restart production.",
			DueAt:       now.Add(4 * time.Hour)},
		Versions:    Versions{Policy: 1, Plan: 3, Graph: 4, Model: "fixture-deterministic", Prompt: "investigate-v1", Tool: "tools-v1"},
		GeneratedAt: now, ExpiresAt: now.Add(30 * time.Minute),
		Rationale: "Two consecutive windows short by 15 units each; a stoppage is recorded on the press.",
	}
}

func encode(t *testing.T, p RecoveryProposalV1) []byte {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestMeasurementsSerializeExactContextValues(t *testing.T) {
	in := fixture()
	got := Measurements([]plantcontext.Snapshot{in.Contexts[0].Snapshot, in.Contexts[1].Snapshot})
	if len(got) != 6 {
		t.Fatalf("measurements = %d, want 6", len(got))
	}
	for _, m := range got {
		want := map[string]string{MetricExpected: "75.000000", MetricActual: "60.000000", MetricDeficit: "15.000000"}[m.MetricName]
		if m.Value != want || m.Unit != "count" || m.ID != MeasurementID(m.ContextID, m.MetricName) {
			t.Errorf("measurement %+v", m)
		}
	}
	// A window without actuals contributes no actual or deficit: missing is never zero.
	missing := window(ctxA, "2026-10-23T06:00:00Z", "2026-10-23T06:15:00Z")
	missing.ActualGoodUnits, missing.DeficitGoodUnits = nil, nil
	if got := Measurements([]plantcontext.Snapshot{missing}); len(got) != 1 || got[0].MetricName != MetricExpected {
		t.Fatalf("missing actuals serialized as %+v", got)
	}
}

// The golden fixture is proposed with its task; it is the only outcome with an action.
func TestGoldenFixtureIsProposedForApproval(t *testing.T) {
	in := fixture()
	if pre, ok := Precheck(in); !ok {
		t.Fatalf("preconditions: %+v", pre)
	}
	out := Judge(in, encode(t, golden()), nil)
	if out.Status != StatusProposed || !out.Action() || out.Proposal.TaskPayload.AssigneeID != assignee {
		t.Fatalf("golden outcome: %+v", out)
	}
}

// Fixtures that must stop before any provider call, as needs_input with the specific gap.
func TestPreconditionFixturesNeedInputWithoutAProviderCall(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Input)
		want []string
	}{
		{"counter reset (an invalid delta)", func(in *Input) {
			in.Contexts[1].Snapshot.Readiness, in.Contexts[1].Snapshot.ReasonCodes = plantcontext.NotReady, []string{"INVALID_DELTA"}
		}, []string{ReasonContextNotReady, "INVALID_DELTA"}},
		{"missing source", func(in *Input) {
			in.Contexts[0].Snapshot.Readiness, in.Contexts[0].Snapshot.ReasonCodes = plantcontext.NotReady, []string{"NO_HEARTBEAT", "SOURCE_NOT_APPROVED"}
		}, []string{ReasonContextNotReady, "NO_HEARTBEAT", "SOURCE_NOT_APPROVED"}},
		{"mixed unit", func(in *Input) { in.Contexts[1].Snapshot.Unit = "kg" }, []string{ReasonUnitMismatch}},
		{"stale plan", func(in *Input) {
			changed := plan
			changed.PlanRevision = 4
			in.CurrentPlan = &changed
		}, []string{ReasonPlanChanged}},
		{"no approved plan", func(in *Input) { in.CurrentPlan = nil }, []string{ReasonNoApprovedPlan}},
		{"context no longer eligible", func(in *Input) { in.Contexts[1].Eligible = false }, []string{ReasonContextNotEligible}},
		{"single window", func(in *Input) { in.Contexts = in.Contexts[1:] }, []string{ReasonTwoWindowsRequired}},
		{"windows not consecutive", func(in *Input) {
			in.Contexts[1].Snapshot.Window = plantcontext.Window{Start: "2026-10-23T06:30:00Z", End: "2026-10-23T06:45:00Z"}
		}, []string{ReasonNotConsecutive}},
		{"other line", func(in *Input) { in.Contexts[0].Snapshot.AssetID = assignee }, []string{ReasonIdentityMismatch}},
		{"below the shortfall threshold", func(in *Input) {
			in.Contexts[1].Snapshot.DeficitExact, in.Contexts[1].Snapshot.ActualExact = str("4"), str("71")
		}, []string{ReasonNotQualified}},
		{"case not investigating", func(in *Input) { in.Case.State = "awaiting_approval" }, []string{ReasonCaseNotInvestigating}},
		{"mode disabled", func(in *Input) { in.ModeEnabled = false }, []string{ReasonModeDisabled}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := fixture()
			tc.edit(&in)
			out, ok := Precheck(in)
			if ok || out.Status != StatusNeedsInput || out.Action() || !reflect.DeepEqual(out.Reasons, tc.want) {
				t.Fatalf("outcome %+v ok=%v, want needs_input %v", out, ok, tc.want)
			}
		})
	}
}

// Specialist outputs that must be refused whole, with no action.
func TestRefusedFixtures(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*RecoveryProposalV1, *Input)
		want []string
	}{
		{"fabricated evidence", func(p *RecoveryProposalV1, _ *Input) {
			p.Hypotheses[0].EvidenceIDs = []string{"00000000-0000-4000-8000-0000000000ff"}
		}, []string{RefuseForeignCitation}},
		{"unsupported numeric statement", func(p *RecoveryProposalV1, _ *Input) {
			p.Rationale = "Two windows short by 40 units each."
		}, []string{RefuseUnsupportedNumber}},
		{"percentage not served by the server", func(p *RecoveryProposalV1, _ *Input) {
			p.Hypotheses[0].Text = "Hypothesis: output fell by 20% after the stoppage."
		}, []string{RefuseUnsupportedNumber}},
		{"observation value differs from the server", func(p *RecoveryProposalV1, _ *Input) {
			p.Observations[0].ValueDecimal = "15"
		}, []string{RefuseNumericMismatch}},
		{"observation of an unknown measurement", func(p *RecoveryProposalV1, _ *Input) {
			p.Observations[0].MeasurementID = "00000000-0000-4000-8000-0000000000fe"
		}, []string{RefuseUnknownMeasurement}},
		{"source injection asks for an equipment adjustment", func(p *RecoveryProposalV1, _ *Input) {
			p.TaskPayload.Kind = "equipment_adjustment.create"
		}, []string{RefuseAction}},
		{"template other than inspect/report", func(p *RecoveryProposalV1, _ *Input) {
			p.TaskPayload.Template = "restart_line"
		}, []string{RefuseAction}},
		{"materials-only evidence disguised as maintenance", func(p *RecoveryProposalV1, _ *Input) {
			p.Options[0].EvidenceIDs = []string{materials}
		}, []string{RefuseNotMaintenance}},
		{"assignee not on the confirmed roster", func(p *RecoveryProposalV1, _ *Input) {
			p.TaskPayload.AssigneeID = "00000000-0000-4000-8000-0000000000a9"
		}, []string{RefuseAssignee}},
		{"no assignee recorded but maintenance selected", func(_ *RecoveryProposalV1, in *Input) { in.Roster = nil }, []string{RefuseNeedsClarify}},
		{"task for another order", func(p *RecoveryProposalV1, _ *Input) { p.TaskPayload.OrderID = assignee }, []string{RefuseTaskScope}},
		{"task with a clarify selection", func(p *RecoveryProposalV1, _ *Input) { p.SelectedOption = OptionClarify }, []string{RefuseTaskMismatch}},
		{"selection not offered", func(p *RecoveryProposalV1, _ *Input) {
			p.Options = p.Options[1:]
			p.SelectedOption, p.TaskPayload = OptionEscalate, nil
		}, []string{RefuseSelection}},
		{"unknown option kind", func(p *RecoveryProposalV1, _ *Input) { p.Options[1].Kind = "approve" }, []string{RefuseOptionKind}},
		{"another case", func(p *RecoveryProposalV1, _ *Input) { p.CaseID = ctxA }, []string{RefuseScope}},
		{"stale case revision", func(p *RecoveryProposalV1, _ *Input) { p.CaseRevision = 1 }, []string{RefuseScope}},
		{"too many options", func(p *RecoveryProposalV1, _ *Input) {
			p.Options = append(p.Options, Option{Kind: OptionDefer, Text: "Wait"}, Option{Kind: OptionEscalate, Text: "Escalate"})
		}, []string{RefuseBounds}},
		{"hypothesis without evidence", func(p *RecoveryProposalV1, _ *Input) { p.Hypotheses[0].EvidenceIDs = []string{} }, []string{RefuseUncited}},
		{"overlong hypothesis", func(p *RecoveryProposalV1, _ *Input) {
			p.Hypotheses[0].Text = strings.Repeat("h", MaxHypothesisText+1)
		}, []string{RefuseText}},
		{"executable task description", func(p *RecoveryProposalV1, _ *Input) {
			p.TaskPayload.Description = "=HYPERLINK(\"https://x.invalid\")"
		}, []string{RefuseText}},
		{"plan version not current", func(p *RecoveryProposalV1, _ *Input) { p.Versions.Plan = 2 }, []string{RefuseVersions}},
		{"generated in the future", func(p *RecoveryProposalV1, _ *Input) { p.GeneratedAt = now.Add(time.Minute) }, []string{RefuseTimes}},
		{"due before generation", func(p *RecoveryProposalV1, _ *Input) { p.TaskPayload.DueAt = now.Add(-time.Minute) }, []string{RefuseTimes}},
		// WIS-721: due times approvals would refuse at submit are refused here, so the case gets a
		// visible needs_input instead of retrying a submit that can never pass.
		{"due with sub-microsecond precision", func(p *RecoveryProposalV1, _ *Input) { p.TaskPayload.DueAt = now.Add(4*time.Hour + time.Nanosecond) }, []string{RefuseTimes}},
		{"due before an approval could expire", func(p *RecoveryProposalV1, _ *Input) { p.TaskPayload.DueAt = now.Add(500 * time.Millisecond) }, []string{RefuseTimes}},
		{"policy version not current", func(p *RecoveryProposalV1, _ *Input) { p.Versions.Policy = 2 }, []string{RefuseVersions}},
		{"graph version not current", func(p *RecoveryProposalV1, _ *Input) { p.Versions.Graph = 3 }, []string{RefuseVersions}},
		{"model version missing", func(p *RecoveryProposalV1, _ *Input) { p.Versions.Model = "" }, []string{RefuseVersions}},
		{"model version with a control character", func(p *RecoveryProposalV1, _ *Input) { p.Versions.Model = "model\x00v2" }, []string{RefuseVersions}},
		{"unknown field not an identifier", func(p *RecoveryProposalV1, _ *Input) { p.Unknowns[0].Field = "Stoppage Cause" }, []string{RefuseText}},
		{"unknown question empty", func(p *RecoveryProposalV1, _ *Input) { p.Unknowns[0].Question = " " }, []string{RefuseText}},
		{"unknown question overlong", func(p *RecoveryProposalV1, _ *Input) {
			p.Unknowns[0].Question = strings.Repeat("q", MaxQuestion+1)
		}, []string{RefuseText}},
		// Post-merge review of PR #145 (WIS-276): expiry was model-chosen and never checked
		// against server time or the rule 05 approval TTL.
		{"already expired", func(p *RecoveryProposalV1, _ *Input) {
			p.GeneratedAt, p.ExpiresAt = now.Add(-48*time.Hour), now.Add(-47*time.Hour)
		}, []string{RefuseTimes}},
		{"expires exactly now", func(p *RecoveryProposalV1, _ *Input) {
			p.GeneratedAt, p.ExpiresAt = now.Add(-30*time.Minute), now
		}, []string{RefuseTimes}},
		{"expiry beyond the approval TTL", func(p *RecoveryProposalV1, _ *Input) {
			p.ExpiresAt = now.Add(100 * 365 * 24 * time.Hour)
		}, []string{RefuseTimes}},
		{"expiry one second beyond the approval TTL", func(p *RecoveryProposalV1, _ *Input) {
			p.ExpiresAt = now.Add(30*time.Minute + time.Second)
		}, []string{RefuseTimes}},
		// PR #145 review: the due date was bounded only by the model's expiry and generation time.
		{"due in the past though after generation", func(p *RecoveryProposalV1, _ *Input) {
			p.GeneratedAt, p.ExpiresAt, p.TaskPayload.DueAt = now.Add(-10*time.Minute), now.Add(20*time.Minute), now.Add(-5*time.Minute)
		}, []string{RefuseTimes}},
		{"due beyond the server horizon", func(p *RecoveryProposalV1, _ *Input) {
			p.TaskPayload.DueAt = now.Add(MaxTaskDue + time.Second)
		}, []string{RefuseTimes}},
		// PR #145 review: numeric statements escaped the guard through separators, signs, other
		// unit words and unscanned unknown questions.
		{"thousands separator", func(p *RecoveryProposalV1, _ *Input) { p.Rationale = "Short by 1,015 units per window." }, []string{RefuseUnsupportedNumber}},
		{"comma-grouped measured value", func(p *RecoveryProposalV1, _ *Input) { p.Rationale = "Short by 15,000 units per window." }, []string{RefuseUnsupportedNumber}},
		{"signed number", func(p *RecoveryProposalV1, _ *Input) { p.Rationale = "Output changed by -15 units." }, []string{RefuseUnsupportedNumber}},
		{"unicode minus", func(p *RecoveryProposalV1, _ *Input) { p.Rationale = "Output changed by −15 units." }, []string{RefuseUnsupportedNumber}},
		{"other unit word", func(p *RecoveryProposalV1, _ *Input) { p.Rationale = "Output fell by 40 parts each window." }, []string{RefuseUnsupportedNumber}},
		{"bare number", func(p *RecoveryProposalV1, _ *Input) { p.Options[1].Text = "Ask the shift lead about the 40 rejects." }, []string{RefuseUnsupportedNumber}},
		{"non-ASCII digits", func(p *RecoveryProposalV1, _ *Input) { p.Rationale = "Short by ４０ units." }, []string{RefuseUnsupportedNumber}},
		{"measured value as a percentage", func(p *RecoveryProposalV1, _ *Input) { p.Rationale = "Output fell by 15 percent." }, []string{RefuseUnsupportedNumber}},
		{"measured value as percentage points", func(p *RecoveryProposalV1, _ *Input) { p.Rationale = "Output fell by 15 percentage points." }, []string{RefuseUnsupportedNumber}},
		{"en dash sign", func(p *RecoveryProposalV1, _ *Input) { p.Rationale = "Output changed by \u201315 units." }, []string{RefuseUnsupportedNumber}},
		{"number in an unknown question", func(p *RecoveryProposalV1, _ *Input) { p.Unknowns[0].Question = "Were 400 units scrapped?" }, []string{RefuseUnsupportedNumber}},
		{"number in the task description", func(p *RecoveryProposalV1, _ *Input) {
			p.TaskPayload.Description = "Inspect the press and report why 40 units were lost."
		}, []string{RefuseUnsupportedNumber}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in, p := fixture(), golden()
			tc.edit(&p, &in)
			out := Judge(in, encode(t, p), nil)
			if out.Status != StatusRefused || out.Action() || out.Proposal != nil {
				t.Fatalf("outcome %+v, want refused", out)
			}
			if !reflect.DeepEqual(out.Reasons, tc.want) {
				t.Fatalf("reasons %v, want exactly %v", out.Reasons, tc.want)
			}
		})
	}
}

// Valid outputs that select something other than a maintenance investigation carry no action.
func TestNonActionOutcomes(t *testing.T) {
	in := fixture()
	in.Roster = nil
	clarify := golden()
	clarify.SelectedOption, clarify.TaskPayload = OptionClarify, nil
	clarify.Options = []Option{{Kind: OptionClarify, Text: "Ask who is responsible for maintenance on this line.", EvidenceIDs: []string{}}}
	clarify.Unknowns = []Unknown{{Field: "assignee", Question: "Who is the responsible maintenance assignee?", Required: true}}
	if out := Judge(in, encode(t, clarify), nil); out.Status != StatusNeedsInput || out.Action() || out.Proposal == nil {
		t.Fatalf("no assignee -> clarify: %+v", out)
	}
	materialsOnly := golden()
	materialsOnly.SelectedOption, materialsOnly.TaskPayload = OptionEscalate, nil
	materialsOnly.Options = []Option{{Kind: OptionEscalate, Text: "Escalate a materials shortage to the shift lead.", EvidenceIDs: []string{materials}}}
	materialsOnly.Hypotheses[0].EvidenceIDs = []string{materials}
	if out := Judge(fixture(), encode(t, materialsOnly), nil); out.Status != StatusEscalated || out.Action() {
		t.Fatalf("materials-only -> escalate: %+v", out)
	}
	if out := Judge(fixture(), nil, errors.New("provider timeout")); out.Status != StatusEscalated || out.Action() || out.Reasons[0] != ReasonProviderFailed {
		t.Fatalf("provider failure: %+v", out)
	}
}

// Untrusted output with smuggled authority fields, duplicate keys or trailing data is refused
// before validation.
func TestStrictDecodingRefusesSmuggledFields(t *testing.T) {
	raw := encode(t, golden())
	for name, body := range map[string][]byte{
		"approved flag":  []byte(strings.Replace(string(raw), `{"proposal_id"`, `{"approved":true,"proposal_id"`, 1)),
		"tenant field":   []byte(strings.Replace(string(raw), `{"proposal_id"`, `{"tenant_id":"`+caseID+`","proposal_id"`, 1)),
		"duplicate key":  []byte(strings.Replace(string(raw), `{"proposal_id"`, `{"selected_option":"defer","proposal_id"`, 1)),
		"trailing value": append(append([]byte{}, raw...), []byte(`{}`)...),
	} {
		if out := Judge(fixture(), body, nil); out.Status != StatusRefused || out.Reasons[0] != ReasonSchemaInvalid {
			t.Errorf("%s: %+v", name, out)
		}
	}
}

// Measured values may be restated in free text, with or without a unit and in either decimal
// form; only numbers outside the server's measurements are refused.
func TestMeasuredNumbersInTextAreAccepted(t *testing.T) {
	for _, text := range []string{
		"Two consecutive windows short by 15 units each.",
		"Expected 75.000000 and actual 60 in each 15-minute window.",
		"The deficit was 15.",
	} {
		p := golden()
		p.Rationale = text
		if out := Judge(fixture(), encode(t, p), nil); out.Status != StatusProposed {
			t.Errorf("%q: %+v", text, out)
		}
	}
}

// PR #145 review: Judge validated against stale facts when the case, mode or context changed
// while the provider ran. It must re-run the preconditions and stop as needs_input.
func TestJudgeRechecksPreconditionsAfterTheProviderCall(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Input)
		want []string
	}{
		{"context became ineligible", func(in *Input) { in.Contexts[1].Eligible = false }, []string{ReasonContextNotEligible}},
		{"mode disabled", func(in *Input) { in.ModeEnabled = false }, []string{ReasonModeDisabled}},
		{"case left investigating", func(in *Input) { in.Case.State = "closed" }, []string{ReasonCaseNotInvestigating}},
		{"all three", func(in *Input) {
			in.Contexts[1].Eligible, in.ModeEnabled, in.Case.State = false, false, "closed"
		}, []string{ReasonCaseNotInvestigating, ReasonContextNotEligible, ReasonModeDisabled}},
		{"no contexts", func(in *Input) { in.Contexts = nil }, []string{ReasonTwoWindowsRequired}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := fixture()
			tc.edit(&in)
			out := Judge(in, encode(t, golden()), nil)
			if out.Status != StatusNeedsInput || out.Action() || out.Proposal != nil || !reflect.DeepEqual(out.Reasons, tc.want) {
				t.Fatalf("outcome %+v, want needs_input %v", out, tc.want)
			}
		})
	}
}

// PR #145 review: Validate indexed in.Contexts[-1] and panicked on an empty list.
func TestValidateRefusesEmptyContexts(t *testing.T) {
	in := fixture()
	in.Contexts = nil
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("Validate panicked: %v", r)
			}
		}()
		err = Validate(golden(), in)
	}()
	var r *Refusal
	if !errors.As(err, &r) || !reflect.DeepEqual(r.Reasons, []string{RefuseScope}) {
		t.Fatalf("err = %v, want refusal %s", err, RefuseScope)
	}
}

// PR #145 review: a maintenance selection that itself lists a required unknown was proposed for
// approval. It needs input first.
func TestRequiredUnknownBlocksMaintenanceProposal(t *testing.T) {
	p := golden()
	p.Unknowns[0].Required = true
	out := Judge(fixture(), encode(t, p), nil)
	if out.Status != StatusNeedsInput || out.Action() || !reflect.DeepEqual(out.Reasons, []string{ReasonRequiredUnknown}) {
		t.Fatalf("outcome %+v, want needs_input %s", out, ReasonRequiredUnknown)
	}
}

// PR #145 review: the proposal ID was model-chosen. approvals.Submit treats a non-empty ID as
// an edit of an existing proposal, so the outcome must never carry the model's ID.
func TestOutcomeDoesNotCarryTheModelProposalID(t *testing.T) {
	in := fixture()
	clarify := golden()
	clarify.SelectedOption, clarify.TaskPayload = OptionClarify, nil
	escalate := golden()
	escalate.SelectedOption, escalate.TaskPayload = OptionEscalate, nil
	escalate.Options = append(escalate.Options, Option{Kind: OptionEscalate, Text: "Escalate to the shift lead.", EvidenceIDs: []string{}})
	for name, p := range map[string]RecoveryProposalV1{"maintenance": golden(), "clarify": clarify, "escalate": escalate} {
		out := Judge(in, encode(t, p), nil)
		if out.Proposal == nil || out.Proposal.ProposalID != "" {
			t.Errorf("%s: proposal %+v, want a proposal with no proposal_id", name, out.Proposal)
		}
	}
}
