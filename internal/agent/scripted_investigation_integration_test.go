//go:build integration

package agent_test

import (
	"database/sql"
	"encoding/json"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/agent"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/approvals"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/cases"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/membership"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/recovery"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/roster"
)

const committedScript = "../../deploy/supervisor/agent-fixture-script.v1.json"

// scriptVariant returns the committed fixture script with edit applied to the investigation
// proposal (a decoded JSON object), for the negative controls.
func scriptVariant(t *testing.T, edit func(proposal map[string]any)) *agent.FixtureProvider {
	t.Helper()
	raw, err := os.ReadFile(committedScript)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err = json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for _, p := range doc["purposes"].([]any) {
		purpose := p.(map[string]any)
		if purpose["purpose"] != "investigation" {
			continue
		}
		steps := purpose["steps"].([]any)
		content := steps[len(steps)-1].(map[string]any)["content"].(map[string]any)
		edit(content["proposal"].(map[string]any))
	}
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := agent.NewFixtureProviderFromScript(out)
	if err != nil {
		t.Fatalf("variant script: %v", err)
	}
	return provider
}

// TestWIS635ScriptedInvestigationReachesTask runs the committed fixture script's investigation
// purpose on the real WIS-272 shortfall case, with a confirmed roster assignee and a real machine
// stoppage. It goes through Runtime.Investigate to a server-built proposal; a supervisor who did
// not submit it approves it; the deterministic executor, with its dispatch-time evidence check,
// creates the one internal task. The proposal comes from the script, but every server value in it
// is copied from the run's own brief and tool results, and the Judge validates it like any model
// output. Each negative control changes one thing and must be refused with no artifact. Scripted
// fixture output is a contract check, not evidence of actual model behaviour (rule 09).
func TestWIS635ScriptedInvestigationReachesTask(t *testing.T) {
	w := newWorld(t)
	line, issuer, operator := w.contexts[1].AssetID, w.reviewer.Issuer, uuid.NewString()
	w.exec(t, `INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'operator')`, w.tenant, w.site, issuer, operator)
	if _, err := roster.New(w.runtimeDB).Decide(w.ctx, roster.Principal{Issuer: issuer, Subject: w.reviewer.Subject, SessionHash: w.reviewer.SessionHash}, roster.Scope{TenantID: w.tenant, SiteID: w.site},
		roster.DecisionInput{LineID: line, AssigneeSubject: operator, Decision: roster.DecisionConfirm, Reason: "Press line maintenance on call"}, uuid.NewString(), 0); err != nil {
		t.Fatal(err)
	}
	assignee := membership.ActorID(issuer, operator).String()
	// An unexplained stoppage inside the case windows (00:00-00:30): machine-fault evidence.
	feed := w.newMachineFeed(t, "machine_state,heartbeat")
	feed.send(t, map[string]any{"external_key": "stop-1", "kind": "machine_state", "event_at": "2026-09-27T00:20:00Z", "asset_external_key": "line-01", "payload": map[string]any{"state": "stopped"}})

	refused := func(name string, p *agent.FixtureProvider, reason string) {
		t.Helper()
		rev := w.fresh(t)
		res := w.investigate(t, w.runtime(p, nil), rev)
		got := w.row(t, res.RunID)
		if got.status != recovery.StatusRefused || got.artifact || (reason != "" && got.reasons != reason) {
			t.Fatalf("%s: want refused %q with no artifact, got %+v", name, reason, got)
		}
	}
	refused("observation value not the server's", scriptVariant(t, func(p map[string]any) {
		p["observations"].([]any)[0].(map[string]any)["value_decimal"] = "1"
	}), "")
	refused("maintenance option without machine evidence", scriptVariant(t, func(p map[string]any) {
		p["options"].([]any)[0].(map[string]any)["evidence_ids"] = []any{p["observations"].([]any)[0].(map[string]any)["measurement_id"]}
	}), recovery.RefuseNotMaintenance)
	refused("assignee not on the confirmed roster", scriptVariant(t, func(p map[string]any) {
		p["task_payload"].(map[string]any)["assignee_id"] = uuid.NewString()
	}), recovery.RefuseAssignee)

	// The committed script, unchanged: proposed, submitted, approved, executed.
	provider, err := agent.LoadFixtureProvider(committedScript)
	if err != nil {
		t.Fatal(err)
	}
	rev := w.fresh(t)
	res := w.investigate(t, w.runtimeWithBridge(provider, nil), rev)
	if got := w.row(t, res.RunID); got.status != recovery.StatusProposed || !got.artifact || got.actual {
		t.Fatalf("committed script run: %+v", got)
	}
	var proposalID string
	if err = w.control.QueryRowContext(w.ctx, `SELECT proposal_id::text FROM supervisor.proposal_revisions WHERE tenant_id=$1 AND site_id=$2 AND case_id=$3 AND generated_by_run_id=$4`, w.tenant, w.site, w.caseID, res.RunID).Scan(&proposalID); err != nil {
		t.Fatalf("no proposal generated by the run: %v", err)
	}
	scope := approvals.Scope{TenantID: w.tenant, SiteID: w.site}
	if decided, err := approvals.NewService(w.runtimeDB).Decide(w.ctx, w.reviewer, scope, proposalID, approvals.DecisionInput{Decision: "approve", ExpectedRevision: 1, Reason: "Reviewed the stoppage evidence"}, uuid.NewString(), 1); err != nil || decided.State != approvals.Approved {
		t.Fatalf("approve: %+v %v", decided, err)
	}
	executor := cases.NewService(w.runtimeDB).WithBoundEvidence(agent.DispatchEvidenceCode)
	if _, err = executor.ExecuteApproved(w.ctx, w.tenant, true, 50); err != nil {
		t.Fatalf("execute: %v", err)
	}
	var tasks int
	var taskAssignee, taskLine, creator string
	err = w.control.QueryRowContext(w.ctx, `SELECT count(*) OVER (),t.assignee_actor_id::text,t.line_id::text,e.actor_issuer FROM supervisor.tasks t
 JOIN supervisor.task_events e ON (e.tenant_id,e.site_id,e.task_id,e.revision)=(t.tenant_id,t.site_id,t.id,1)
 WHERE t.tenant_id=$1 AND t.site_id=$2 AND t.case_id=$3`, w.tenant, w.site, w.caseID).Scan(&tasks, &taskAssignee, &taskLine, &creator)
	if err == sql.ErrNoRows {
		t.Fatal("the executor created no task")
	}
	if err != nil || tasks != 1 || taskAssignee != assignee || taskLine != line || creator != cases.ExecutorActor.Issuer {
		t.Fatalf("task: count=%d assignee=%s line=%s creator=%s err=%v", tasks, taskAssignee, taskLine, creator, err)
	}
}
