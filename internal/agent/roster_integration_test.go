//go:build integration

package agent_test

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/agent"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/membership"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/recovery"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/roster"
)

// TestWIS276ConfirmedRosterReachesTheJudge: the case loader offers exactly the line's confirmed,
// still-eligible assignees, so a maintenance proposal naming one passes the roster checks (and,
// with no machine telemetry in this world, is refused only for want of machine-fault evidence:
// see TestWIS276MachineStoppageIsMaintenanceEvidence), while one naming anyone else, or a revoked
// assignee, is refused.
func TestWIS276ConfirmedRosterReachesTheJudge(t *testing.T) {
	w := newWorld(t)
	line, issuer, operator := w.contexts[1].AssetID, w.reviewer.Issuer, uuid.NewString()
	w.exec(t, `INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'operator')`, w.tenant, w.site, issuer, operator)
	loaded := func() []recovery.Assignee {
		t.Helper()
		var out []recovery.Assignee
		if err := w.store.InTx(w.ctx, agent.RunScope{TenantID: w.tenant, SiteID: w.site}, func(tx *sql.Tx) error {
			in, e := agent.LoadCaseInput(w.ctx, tx, agent.RunScope{TenantID: w.tenant, SiteID: w.site}, w.caseID, agent.LoaderOptions{ModeEnabled: true})
			out = in.Input.Roster
			return e
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if got := loaded(); len(got) != 0 {
		t.Fatalf("roster before any confirmation: %+v", got)
	}
	decider := roster.Principal{Issuer: issuer, Subject: w.reviewer.Subject, SessionHash: w.reviewer.SessionHash}
	if _, err := roster.New(w.runtimeDB).Decide(w.ctx, decider, roster.Scope{TenantID: w.tenant, SiteID: w.site},
		roster.DecisionInput{LineID: line, AssigneeSubject: operator, Decision: roster.DecisionConfirm, Reason: "Press line maintenance on call"}, uuid.NewString(), 0); err != nil {
		t.Fatal(err)
	}
	assignee := membership.ActorID(issuer, operator).String()
	want := recovery.Assignee{ID: assignee, LineID: line, ConfirmedBy: membership.ActorID(issuer, w.reviewer.Subject).String()}
	if got := loaded(); len(got) != 1 || got[0] != want {
		t.Fatalf("roster=%+v want %+v", got, want)
	}
	judge := func(id string) string {
		t.Helper()
		rev := w.fresh(t)
		prop := w.clarify(rev)
		maintenance(w, &prop, line)
		prop.TaskPayload.AssigneeID = id
		res := w.investigate(t, w.runtime(script(finalStep(t, w.result("success"), prop)), nil), rev)
		got := w.row(t, res.RunID)
		if got.status != recovery.StatusRefused || got.artifact || res.ArtifactID != "" {
			t.Fatalf("assignee %s: %+v", id, got)
		}
		return got.reasons
	}
	if reasons := judge(assignee); reasons != recovery.RefuseNotMaintenance {
		t.Fatalf("confirmed assignee: reasons %q, want only %s", reasons, recovery.RefuseNotMaintenance)
	}
	if reasons := judge(uuid.NewString()); !strings.Contains(reasons, recovery.RefuseAssignee) {
		t.Fatalf("unconfirmed assignee: reasons %q", reasons)
	}
	// Revocation takes effect at the next load: the deactivated assignee is no longer offered,
	// and a proposal naming them falls back to needing clarification.
	w.exec(t, `UPDATE supervisor.memberships SET active=false WHERE tenant_id=$1 AND site_id=$2 AND issuer=$3 AND subject=$4`, w.tenant, w.site, issuer, operator)
	if got := loaded(); len(got) != 0 {
		t.Fatalf("revoked assignee still offered: %+v", got)
	}
	if reasons := judge(assignee); !strings.Contains(reasons, recovery.RefuseNeedsClarify) {
		t.Fatalf("revoked assignee: reasons %q", reasons)
	}
}
