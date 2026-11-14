//go:build integration

package dispatch

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/agent"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
	"github.com/google/uuid"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
)

func TestRelayInterviewTurnDuplicateStartIsDeliveredAndTickBecomesNoop(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	address := os.Getenv("SUPERVISOR_TEST_TEMPORAL_ADDRESS")
	if address == "" {
		t.Fatal("BLOCKED: actual Temporal service address required")
	}
	admin, db := tenantDiscoveryDatabase(t, ctx)
	defer db.Close()
	defer admin.Close()
	c, err := client.Dial(client.Options{HostPort: address, Namespace: "default"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	tenant, site, subject, sessionID, runID := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	issuer, queue := "https://interview-relay.invalid/realm", "interview-turn-test-"+uuid.NewString()
	insertDiscoveryTenant(t, ctx, admin, tenant, site, issuer, subject)
	if _, err = admin.ExecContext(ctx, `INSERT INTO supervisor.interview_sessions(tenant_id,site_id,session_id,role,participant_actor_id,participant_issuer,participant_subject,status,revision) VALUES($1,$2,$3,'owner',$4,$5,$6,'active',1)`, tenant, site, sessionID, uuid.NewString(), issuer, subject); err != nil {
		t.Fatal(err)
	}
	as := agent.NewStore(db.DB)
	if err = as.InTx(ctx, agent.RunScope{TenantID: tenant, SiteID: site}, func(tx *sql.Tx) error {
		run := agent.Run{ID: runID, RequestKind: agent.RequestConversation, ConversationID: sessionID, RequestID: uuid.NewString(),
			State: agent.RunRunning, TraceID: agent.NewTraceID(), ProviderName: "fixture", Model: "fixture-model", ModelRevision: "r1",
			PromptVersion: agent.InterviewPromptVersion, ToolVersion: agent.ToolVersion, PolicyRevision: 1}
		if err := agent.CreateRunTx(ctx, tx, agent.RunScope{TenantID: tenant, SiteID: site}, run, agent.InterviewLimits()); err != nil {
			return err
		}
		if err := agent.ReserveInvocationTx(ctx, tx, agent.RunScope{TenantID: tenant, SiteID: site}, runID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO supervisor.interview_agent_turns(tenant_id,site_id,session_id,run_id,session_revision,purpose) VALUES($1,$2,$3,$4,1,'questions')`, tenant, site, sessionID, runID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO supervisor.interview_agent_turn_outbox(tenant_id,site_id,id,session_id,run_id) VALUES($1,$2,$3,$4,$5)`, tenant, site, uuid.NewString(), sessionID, runID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{TemporalQueue: queue, Tenants: []string{tenant}}
	w, err := NewWorker(c, db, cfg)
	if err == nil {
		err = w.Start()
	}
	if err != nil {
		t.Fatal(err)
	}
	defer w.Stop()
	ref := InterviewTurnRef{Tenant: tenant, Site: site, Session: sessionID, Run: runID}
	wfID := "interview-turn:" + tenant + ":" + runID
	run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: wfID, TaskQueue: queue,
		WorkflowIDReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE}, InterviewTurnWorkflow, ref)
	if err != nil {
		t.Fatal(err)
	}
	// This models Temporal accepting StartWorkflow while the relay loses its acknowledgement.
	if n, e := Tick(ctx, db, c, cfg, w.AgentEnabled); e != nil || n != 1 {
		t.Fatalf("relay treating duplicate workflow as delivered: relayed=%d err=%v", n, e)
	}
	if err = run.Get(ctx, nil); err != nil {
		t.Fatalf("disabled agent did not fail closed through the workflow finalizer: %v", err)
	}
	var delivered sql.NullTime
	var state, status, reason string
	if err = admin.QueryRowContext(ctx, `SELECT delivered_at FROM supervisor.interview_agent_turn_outbox WHERE tenant_id=$1 AND site_id=$2 AND run_id=$3`, tenant, site, runID).Scan(&delivered); err != nil || !delivered.Valid {
		t.Fatalf("outbox not delivered: delivered=%v err=%v", delivered, err)
	}
	if err = admin.QueryRowContext(ctx, `SELECT state,outcome_status,outcome_reasons[1] FROM supervisor.agent_runs WHERE tenant_id=$1 AND site_id=$2 AND id=$3`, tenant, site, runID).Scan(&state, &status, &reason); err != nil || state != agent.RunCompleted || status != "needs_input" || reason != agent.CodeActivityFailed {
		t.Fatalf("disabled-agent terminal run state=%q status=%q reason=%q err=%v", state, status, reason, err)
	}
	if n, e := Tick(ctx, db, c, cfg, w.AgentEnabled); e != nil || n != 0 {
		t.Fatalf("second tick delivered=%d err=%v", n, e)
	}
}
