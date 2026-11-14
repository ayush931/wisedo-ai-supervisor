//go:build integration

package agent_test

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/advisor"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/agent"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/approvals"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/evidence"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/recovery"
)

// TestWIS629AdvisorSuggestion drives GET /cases/{case}/suggestion on the real WIS-272 shortfall
// case, as the runtime role under forced RLS: approved fact from the pinned snapshot, server
// observations, the stored investigation hypothesis; deterministic (one per case); an invented
// citation in stored model output, a revoked passage source, a superseded context and a foreign
// tenant each have their stated outcome.
func TestWIS629AdvisorSuggestion(t *testing.T) {
	w := newWorldOpts(t, worldOpts{lineClaim: true})
	path := "/v1/sites/" + w.site + "/cases/" + w.caseID + "/suggestion"
	read := func(who string) (advisor.SuggestionV1, []byte) {
		t.Helper()
		code, body := w.get(t, who, path)
		if code != 200 {
			t.Fatalf("GET suggestion as %s: %d %s", who, code, body)
		}
		var s advisor.SuggestionV1
		if err := json.Unmarshal(body, &s); err != nil {
			t.Fatal(err)
		}
		return s, body
	}
	measured := recovery.Measurements(w.contexts)

	// Before any investigation: the approved fact and the observations, no hypothesis yet.
	s, _ := read("owner")
	if s.Readiness != advisor.Limited || !slices.Equal(s.Reasons, []string{advisor.ReasonInvestigationNotRun}) || s.Plan == nil || s.Snapshot == nil {
		t.Fatalf("before investigation: %s %v plan=%v snapshot=%v", s.Readiness, s.Reasons, s.Plan, s.Snapshot)
	}
	if len(s.Facts) != 1 || s.Facts[0].ClaimID != w.lineClaim || *s.Facts[0].Value != "60" || !slices.Equal(s.Facts[0].SpanIDs, []string{w.lineClaimSpan}) ||
		len(s.Passages) != 1 || s.Passages[0].SpanID != w.lineClaimSpan || s.Passages[0].Text != lineClaimText {
		t.Fatalf("facts: %+v passages: %+v", s.Facts, s.Passages)
	}
	if len(s.Observations) != len(measured) || len(measured) != 6 {
		t.Fatalf("observations %d, measurements %d", len(s.Observations), len(measured))
	}
	for i, o := range s.Observations {
		if o.MeasurementID != measured[i].ID || o.Value != measured[i].Value || o.Unit != measured[i].Unit {
			t.Fatalf("observation %d: %+v != %+v", i, o, measured[i])
		}
	}

	// Rule 03: another tenant's member gets the non-disclosing 404.
	if code, body := w.get(t, "outsider", path); code != 404 || !bytes.Contains(body, []byte(`"NOT_FOUND"`)) {
		t.Fatalf("outsider: %d %s", code, body)
	}

	// A scripted investigation stores a validated hypothesis; the suggestion shows it.
	rev := w.revision
	deficit := w.measurement(recovery.MetricDeficit)
	proposal := w.clarify(rev)
	proposal.Hypotheses = []recovery.Hypothesis{{Text: "A stoppage on the press may explain the deficit.", EvidenceIDs: []string{deficit.ID}, ContradictingEvidenceIDs: []string{}}}
	res := w.investigate(t, w.runtime(script(finalStep(t, w.result("success"), proposal)), nil), rev)
	if got := w.row(t, res.RunID); !got.artifact {
		t.Fatalf("the scripted run stored no artifact: %+v", got)
	}
	s, first := read("owner")
	if s.Readiness != advisor.Ready || len(s.Reasons) != 0 || len(s.Hypotheses) != 1 || s.Hypotheses[0].Text != proposal.Hypotheses[0].Text ||
		!slices.Equal(s.Hypotheses[0].EvidenceIDs, []string{deficit.ID}) || len(s.Unknowns) != 1 || s.Investigation == nil || s.Investigation.RunID != res.RunID || s.Investigation.Actual {
		t.Fatalf("after investigation: %s %v %+v %+v", s.Readiness, s.Reasons, s.Hypotheses, s.Investigation)
	}
	// One suggestion per case: assembled on read from the same records, it is the same bytes.
	if _, again := read("reviewer"); !bytes.Equal(first, again) {
		t.Fatalf("two reads differ:\n%s\n%s", first, again)
	}

	// WIS-631 regression: the run's own proposal moves the case on (proposed, awaiting approval);
	// that keeps its investigation current. Only a return to investigating makes it earlier.
	w.move(t, "investigating", "proposed")
	w.move(t, "proposed", "awaiting_approval")
	if moved, _ := read("owner"); moved.Readiness != advisor.Ready || len(moved.Hypotheses) == 0 {
		t.Fatalf("case moved on by its own proposal: %s %v", moved.Readiness, moved.Reasons)
	}
	w.move(t, "awaiting_approval", "needs_input") // e.g. a clarification is requested
	w.move(t, "needs_input", "investigating")
	if again, _ := read("owner"); again.Readiness != advisor.Limited || !slices.Equal(again.Reasons, []string{advisor.ReasonInvestigationEarlier}) {
		t.Fatalf("case investigated again: %s %v", again.Readiness, again.Reasons)
	}

	// WIS-629: stored model output citing something its run never captured is left out whole.
	invented := w.clarify(w.fresh(t))
	invented.ProposalID = ""
	invented.Hypotheses = []recovery.Hypothesis{{Text: "A stoppage may explain it.", EvidenceIDs: []string{deficit.ID, uuid.NewString()}, ContradictingEvidenceIDs: []string{}}}
	seedArtifactRun(t, w, invented)
	s, _ = read("owner")
	if !slices.Contains(s.Reasons, advisor.ReasonHypothesisCitationInvalid) || len(s.Hypotheses) != 0 || len(s.Unknowns) != 0 || len(s.Facts) != 1 || len(s.Observations) != 6 {
		t.Fatalf("invented citation: %s %v hypotheses=%d facts=%d", s.Readiness, s.Reasons, len(s.Hypotheses), len(s.Facts))
	}

	// Rule 06: once the SOP is revoked its passage is never returned and the fact is left out.
	es := evidence.Scope{TenantID: w.tenant, SiteID: w.site}
	head, err := w.ev.Read(w.ctx, w.ea, es, w.lineClaimSource, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.ev.Revoke(w.ctx, w.ea, es, w.lineClaimSource, "Withdrawn by the owner", uuid.NewString(), head.StateRevision); err != nil {
		t.Fatal(err)
	}
	s, body := read("owner")
	if len(s.Facts) != 0 || !slices.Contains(s.Reasons, advisor.ReasonSourceUnavailable) || bytes.Contains(body, []byte(lineClaimText)) {
		t.Fatalf("revoked source: %v facts=%d", s.Reasons, len(s.Facts))
	}

	// Codex PR-228 finding 2: a late record supersedes only the earlier window. The current window
	// stays eligible under the approved plan, yet the earlier window's measurements may not be
	// shown: the suggestion is NOT_READY with CONTEXT_NOT_ELIGIBLE and no statements (rule 06).
	w.sendLateDeltaTo(0)
	if err = w.store.InTx(w.ctx, agent.RunScope{TenantID: w.tenant, SiteID: w.site}, func(tx *sql.Tx) error {
		in, e := agent.LoadCaseInput(w.ctx, tx, agent.RunScope{TenantID: w.tenant, SiteID: w.site}, w.caseID, agent.LoaderOptions{})
		if e == nil && (len(in.Input.Contexts) != 2 || in.Input.Contexts[0].Eligible || !in.Input.Contexts[1].Eligible || in.Input.CurrentPlan == nil) {
			t.Fatalf("setup: want only the earlier window ineligible, got %+v plan=%v", in.Input.Contexts, in.Input.CurrentPlan)
		}
		return e
	}); err != nil {
		t.Fatal(err)
	}
	s, _ = read("owner")
	if s.Readiness != advisor.NotReady || !slices.Equal(s.Reasons, []string{recovery.ReasonContextNotEligible}) || s.Plan == nil || len(s.Facts)+len(s.Observations)+len(s.Hypotheses)+len(s.Unknowns) != 0 {
		t.Fatalf("earlier window ineligible: %s %v observations=%d", s.Readiness, s.Reasons, len(s.Observations))
	}

	// A late record supersedes the case's context: no plan is approved for what selection would
	// use now, so the suggestion is NOT_READY with no statements at all.
	w.sendLateDelta()
	s, _ = read("owner")
	if s.Readiness != advisor.NotReady || !slices.Contains(s.Reasons, advisor.ReasonNoApprovedPlan) || len(s.Facts)+len(s.Observations)+len(s.Hypotheses)+len(s.Unknowns) != 0 {
		t.Fatalf("superseded context: %s %v", s.Readiness, s.Reasons)
	}
}

// seedArtifactRun stores a terminal run at the world's current revision whose artifact is body,
// bypassing the Judge, as stored output from an older release or a tampered row would be.
func seedArtifactRun(t *testing.T, w *world, p recovery.RecoveryProposalV1) {
	t.Helper()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	runID, sc := uuid.NewString(), agent.RunScope{TenantID: w.tenant, SiteID: w.site}
	err = w.store.InTx(w.ctx, sc, func(tx *sql.Tx) error {
		in, e := agent.LoadCaseInput(w.ctx, tx, sc, w.caseID, agent.LoaderOptions{ModeEnabled: true})
		if e != nil {
			return e
		}
		if e = agent.CreateRunTx(w.ctx, tx, sc, agent.Run{ID: runID, RequestKind: agent.RequestCase, CaseID: w.caseID, CaseRevision: w.revision,
			TraceID: agent.NewTraceID(), ProviderName: agent.ProviderFixture, Model: "fixture-model", ModelRevision: "sha256:fixture",
			PromptVersion: agent.PromptVersion, ToolVersion: agent.ToolVersion, PolicyRevision: approvals.PolicyRevision}, agent.DefaultLimits()); e != nil {
			return e
		}
		if e = agent.AddRefsTx(w.ctx, tx, sc, runID, in.Refs); e != nil {
			return e
		}
		if _, e = agent.AddArtifactTx(w.ctx, tx, sc, runID, agent.Artifact{ID: uuid.NewString(), Schema: agent.ArtifactSchema, Body: raw}); e != nil {
			return e
		}
		return agent.FinishRunTx(w.ctx, tx, sc, runID, agent.Finish{State: agent.RunCompleted, OutcomeStatus: recovery.StatusNeedsInput})
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestWIS629SuggestionDropsRevokedMachineEvidence: a hypothesis citing a captured machine
// stoppage is shown while the stoppage is current, and left out (with every other model
// statement) once its operational source is revoked (rule 06), by the executor's own check.
func TestWIS629SuggestionDropsRevokedMachineEvidence(t *testing.T) {
	w := newWorld(t)
	feed := w.newMachineFeed(t, "machine_state,heartbeat")
	feed.send(t, map[string]any{"external_key": "stop-1", "kind": "machine_state", "event_at": "2026-09-27T00:20:00Z", "asset_external_key": "line-01", "payload": map[string]any{"state": "stopped"}})
	scope := agent.RunScope{TenantID: w.tenant, SiteID: w.site}
	var stoppage string
	if err := w.store.InTx(w.ctx, scope, func(tx *sql.Tx) error {
		in, e := agent.LoadCaseInput(w.ctx, tx, scope, w.caseID, agent.LoaderOptions{ModeEnabled: true})
		for _, r := range in.Refs {
			if r.Kind == agent.RefMachineState {
				stoppage = r.RefID
			}
		}
		return e
	}); err != nil || stoppage == "" {
		t.Fatalf("setup: no captured stoppage (%v)", err)
	}
	p := w.clarify(w.revision)
	p.ProposalID = ""
	p.Hypotheses = []recovery.Hypothesis{{Text: "The recorded stoppage may explain the deficit.", EvidenceIDs: []string{stoppage}, ContradictingEvidenceIDs: []string{}}}
	seedArtifactRun(t, w, p)
	path := "/v1/sites/" + w.site + "/cases/" + w.caseID + "/suggestion"
	read := func() advisor.SuggestionV1 {
		t.Helper()
		code, body := w.get(t, "owner", path)
		var s advisor.SuggestionV1
		if code != 200 || json.Unmarshal(body, &s) != nil {
			t.Fatalf("GET suggestion: %d %s", code, body)
		}
		return s
	}
	if s := read(); len(s.Hypotheses) != 1 || !slices.Equal(s.Hypotheses[0].EvidenceIDs, []string{stoppage}) || slices.Contains(s.Reasons, advisor.ReasonHypothesisCitationInvalid) {
		t.Fatalf("current stoppage: %v %+v", s.Reasons, s.Hypotheses)
	}
	feed.revoke(t)
	if s := read(); len(s.Hypotheses) != 0 || len(s.Unknowns) != 0 || !slices.Contains(s.Reasons, advisor.ReasonHypothesisCitationInvalid) {
		t.Fatalf("revoked stoppage still cited: %s %v %+v", s.Readiness, s.Reasons, s.Hypotheses)
	}
}
