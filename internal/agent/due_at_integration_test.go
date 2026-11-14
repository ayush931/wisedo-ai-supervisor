//go:build integration

package agent_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/agent"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/membership"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/recovery"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/roster"
)

// WIS-721: a model proposal whose task_payload.due_at carries nanoseconds passed the Judge, but
// approvals refused every submission, so Investigate (what each Temporal attempt and workflow
// restart calls) failed forever and the case stayed investigating with no reason. The Judge now
// refuses such a due time, so the case is escalated to a person like any refused model output.
func TestNanosecondDueAtReachesAPersonPostgres(t *testing.T) {
	w := newWorld(t)
	line, issuer, operator := w.contexts[1].AssetID, w.reviewer.Issuer, uuid.NewString()
	w.exec(t, `INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'operator')`, w.tenant, w.site, issuer, operator)
	if _, err := roster.New(w.runtimeDB).Decide(w.ctx, roster.Principal{Issuer: issuer, Subject: w.reviewer.Subject, SessionHash: w.reviewer.SessionHash}, roster.Scope{TenantID: w.tenant, SiteID: w.site},
		roster.DecisionInput{LineID: line, AssigneeSubject: operator, Decision: roster.DecisionConfirm, Reason: "Press line maintenance on call"}, uuid.NewString(), 0); err != nil {
		t.Fatal(err)
	}
	_ = membership.ActorID(issuer, operator)
	feed := w.newMachineFeed(t, "machine_state,heartbeat")
	feed.send(t, map[string]any{"external_key": "stop-1", "kind": "machine_state", "event_at": "2026-09-27T00:20:00Z", "asset_external_key": "line-01", "payload": map[string]any{"state": "stopped"}})

	due := time.Now().UTC().Add(4 * time.Hour).Truncate(time.Second).Add(1) // +1 ns
	dueText := due.Format(time.RFC3339Nano)
	provider := scriptVariant(t, func(p map[string]any) {
		p["task_payload"].(map[string]any)["due_at"] = dueText
	})
	rev := w.fresh(t)
	rt := w.runtimeWithBridge(provider, nil)
	ref := agent.InvestigateRef{TenantID: w.tenant, SiteID: w.site, CaseID: w.caseID}
	res, err := rt.Investigate(w.ctx, ref, rev)
	if err != nil {
		t.Fatalf("Investigate: %v (a refused due time must not fail the activity)", err)
	}
	if res.Status == recovery.StatusProposed {
		t.Fatalf("a nanosecond due_at was proposed: %+v", res)
	}
	var state string
	var proposals int
	if err = w.control.QueryRowContext(w.ctx, `SELECT state,(SELECT count(*) FROM supervisor.proposal_revisions p WHERE p.tenant_id=c.tenant_id AND p.site_id=c.site_id AND p.case_id=c.id) FROM supervisor.cases c WHERE tenant_id=$1 AND site_id=$2 AND id=$3`, w.tenant, w.site, w.caseID).Scan(&state, &proposals); err != nil {
		t.Fatal(err)
	}
	if state != "escalated" || proposals != 0 {
		t.Fatalf("case state=%s proposals=%d, want escalated and 0", state, proposals)
	}
}
