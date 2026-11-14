//go:build integration

package agent_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/agent"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/evidence"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/membership"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/recovery"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/roster"
)

// TestWIS276JudgeSeesCapturedDocuments: a proposal may cite the evidence source the run captured
// (before this fix Judge refused it as FOREIGN_CITATION), but a document is never machine-fault
// evidence, so a maintenance investigation citing only documents stays refused.
func TestWIS276JudgeSeesCapturedDocuments(t *testing.T) {
	w := newWorld(t)
	line, issuer, operator := w.contexts[1].AssetID, w.reviewer.Issuer, uuid.NewString()
	w.exec(t, `INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'operator')`, w.tenant, w.site, issuer, operator)
	if _, err := roster.New(w.runtimeDB).Decide(w.ctx, roster.Principal{Issuer: issuer, Subject: w.reviewer.Subject, SessionHash: w.reviewer.SessionHash}, roster.Scope{TenantID: w.tenant, SiteID: w.site},
		roster.DecisionInput{LineID: line, AssigneeSubject: operator, Decision: roster.DecisionConfirm, Reason: "Press line maintenance on call"}, uuid.NewString(), 0); err != nil {
		t.Fatal(err)
	}
	assignee := membership.ActorID(issuer, operator).String()
	for _, tc := range []struct {
		name            string
		forge           func(p *recovery.RecoveryProposalV1)
		status, reasons string
		artifact        bool
	}{
		{"clarify citing the captured plan source", func(p *recovery.RecoveryProposalV1) {
			p.Options[0].EvidenceIDs = append(p.Options[0].EvidenceIDs, w.schedule)
			p.Hypotheses = []recovery.Hypothesis{{Text: "Hypothesis: the shift plan may not match the line.", EvidenceIDs: []string{w.schedule}, ContradictingEvidenceIDs: []string{}}}
		}, recovery.StatusNeedsInput, "CLARIFICATION_REQUESTED", true},
		{"maintenance citing only the plan source", func(p *recovery.RecoveryProposalV1) {
			maintenance(w, p, line)
			p.TaskPayload.AssigneeID = assignee
			p.Options[len(p.Options)-1].EvidenceIDs = []string{w.schedule}
		}, recovery.StatusRefused, recovery.RefuseNotMaintenance, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rev := w.fresh(t)
			prop := w.clarify(rev)
			tc.forge(&prop)
			result := w.result("success")
			result.CitedEvidenceIDs = append(result.CitedEvidenceIDs, w.schedule)
			res := w.investigate(t, w.runtime(script(finalStep(t, result, prop)), nil), rev)
			if got := w.row(t, res.RunID); got.state != agent.RunCompleted || got.status != tc.status || got.reasons != tc.reasons || got.artifact != tc.artifact {
				t.Fatalf("run %+v, want %s/%s artifact=%v", got, tc.status, tc.reasons, tc.artifact)
			}
		})
	}
}

// TestWIS276RevisedDocumentMidRunIsStale (review P2): the plan source is corrected while the model
// call is in flight. Its access epoch is unchanged, so the revocation recheck passes, but Judge
// must not accept a clarify artifact citing the superseded text: such an artifact never reaches
// dispatch's EVIDENCE_REVISED check. The run ends STALE_SCOPE with no artifact.
func TestWIS276RevisedDocumentMidRunIsStale(t *testing.T) {
	w := newWorld(t)
	es := evidence.Scope{TenantID: w.tenant, SiteID: w.site}
	citing := func(rev int64) *scripted {
		prop := w.clarify(rev)
		prop.Options[0].EvidenceIDs = append(prop.Options[0].EvidenceIDs, w.schedule)
		result := w.result("success")
		result.CitedEvidenceIDs = append(result.CitedEvidenceIDs, w.schedule)
		return script(finalStep(t, result, prop))
	}

	// Control: unchanged source, the citation is accepted.
	rev := w.fresh(t)
	res := w.investigate(t, w.runtime(citing(rev), nil), rev)
	want(t, w.row(t, res.RunID), agent.RunCompleted, recovery.StatusNeedsInput, "CLARIFICATION_REQUESTED")

	rev = w.fresh(t)
	p := citing(rev)
	p.before = func(call int) {
		var state int64
		if err := w.control.QueryRowContext(w.ctx, `SELECT state_revision FROM supervisor.evidence_heads WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3`, w.tenant, w.site, w.schedule).Scan(&state); err != nil {
			t.Error(err)
			return
		}
		if _, err := w.ev.CreateOrRevise(w.ctx, w.ea, es, evidence.Input{SourceID: w.schedule, Kind: "manual", Title: "Shift plan", Text: "Order seven on press one at two units per second."}, uuid.NewString(), state); err != nil {
			t.Error(err)
		}
	}
	res = w.investigate(t, w.runtime(p, nil), rev)
	got := w.row(t, res.RunID)
	want(t, got, agent.RunCompleted, recovery.StatusNeedsInput, agent.CodeStaleScope)
	if got.artifact || got.revoked {
		t.Fatalf("revised source: %+v", got)
	}
}

// TestWIS276SupersededUncitedSourceIsNotStale (review round 2 P2): the approved plan cites a
// source revision that is already superseded when the run captures it. A measurement-only
// clarification does not rely on that text and keeps its artifact; citing the superseded document
// still ends STALE_SCOPE.
func TestWIS276SupersededUncitedSourceIsNotStale(t *testing.T) {
	w := newWorld(t)
	var state int64
	if err := w.control.QueryRowContext(w.ctx, `SELECT state_revision FROM supervisor.evidence_heads WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3`, w.tenant, w.site, w.schedule).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if _, err := w.ev.CreateOrRevise(w.ctx, w.ea, evidence.Scope{TenantID: w.tenant, SiteID: w.site}, evidence.Input{SourceID: w.schedule, Kind: "manual", Title: "Shift plan", Text: "Order seven on press one at two units per second."}, uuid.NewString(), state); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name            string
		cite            bool
		status, reasons string
		artifact        bool
	}{
		{"measurement-only clarify", false, recovery.StatusNeedsInput, "CLARIFICATION_REQUESTED", true},
		{"clarify citing the superseded source", true, recovery.StatusNeedsInput, agent.CodeStaleScope, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rev := w.fresh(t)
			prop, result := w.clarify(rev), w.result("success")
			if tc.cite {
				prop.Options[0].EvidenceIDs = append(prop.Options[0].EvidenceIDs, w.schedule)
				result.CitedEvidenceIDs = append(result.CitedEvidenceIDs, w.schedule)
			}
			res := w.investigate(t, w.runtime(script(finalStep(t, result, prop)), nil), rev)
			if got := w.row(t, res.RunID); got.state != agent.RunCompleted || got.status != tc.status || got.reasons != tc.reasons || got.artifact != tc.artifact {
				t.Fatalf("run %+v, want %s/%s artifact=%v", got, tc.status, tc.reasons, tc.artifact)
			}
		})
	}
}
