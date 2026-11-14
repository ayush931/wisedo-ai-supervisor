//go:build integration

package dispatch

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/onboarding"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/workstation"
	"github.com/google/uuid"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
)

func TestRelayConfigurationDraftsDuplicateStartMarksOutboxDeliveredOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	admin, db := tenantDiscoveryDatabase(t, ctx)
	defer db.Close()
	defer admin.Close()
	address := os.Getenv("SUPERVISOR_TEST_TEMPORAL_ADDRESS")
	if address == "" {
		t.Fatal("BLOCKED: actual Temporal service address required")
	}
	c, err := client.Dial(client.Options{HostPort: address, Namespace: "default"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	tenant, site, subject, sessionID := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	issuer, queue := "https://configuration-draft-relay.invalid/realm", "configuration-draft-test-"+uuid.NewString()
	insertDiscoveryTenant(t, ctx, admin, tenant, site, issuer, subject)
	participantActorID, token, csrf := uuid.NewString(), uuid.NewString(), uuid.NewString()
	tokenHashBytes := sha256.Sum256([]byte(token))
	tokenHash := hex.EncodeToString(tokenHashBytes[:])
	if _, err = admin.ExecContext(ctx, `INSERT INTO supervisor.browser_sessions(token_hash,subject,issuer,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,$4,clock_timestamp(),clock_timestamp(),clock_timestamp()+interval '1 hour',clock_timestamp())`, tokenHash, subject, issuer, csrf); err != nil {
		t.Fatal(err)
	}
	if _, err = admin.ExecContext(ctx, `INSERT INTO supervisor.interview_sessions(tenant_id,site_id,session_id,role,participant_actor_id,participant_issuer,participant_subject,status,revision) VALUES($1,$2,$3,'owner',$4,$5,$6,'active',1)`, tenant, site, sessionID, participantActorID, issuer, subject); err != nil {
		t.Fatal(err)
	}
	request, err := onboarding.NewInterviews(db.DB).StartConfigurationDrafts(ctx, onboarding.Actor{Issuer: issuer, Subject: subject, SessionHash: tokenHash}, onboarding.Scope{TenantID: tenant, SiteID: site}, onboarding.StartConfigurationDraftsInput{SessionID: uuid.MustParse(sessionID), Kinds: []workstation.DraftKind{workstation.DraftKindLine}}, "draft-request", onboarding.AgentTurnSettings{Enabled: true, Provider: "fixture", Model: "fixture", ModelRevision: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	var pending, delivered int
	if err = admin.QueryRowContext(ctx, `SELECT count(*) FILTER(WHERE delivered_at IS NULL),count(*) FILTER(WHERE delivered_at IS NOT NULL) FROM supervisor.configuration_draft_request_outbox WHERE tenant_id=$1 AND site_id=$2 AND run_id=$3`, tenant, site, request.RunID).Scan(&pending, &delivered); err != nil || pending != 1 || delivered != 0 {
		t.Fatalf("request outbox pending=%d delivered=%d err=%v", pending, delivered, err)
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
	workflowID := "configuration-draft:" + tenant + ":" + request.RunID
	run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: workflowID, TaskQueue: queue}, ConfigurationDraftWorkflow, ConfigurationDraftRef{Tenant: tenant, Site: site, Session: sessionID, Run: request.RunID})
	if err != nil {
		t.Fatal(err)
	}
	if err = run.Get(ctx, nil); err != nil {
		t.Fatalf("pre-existing run failed to finalize: %v", err)
	}
	startedRunID := run.GetRunID()
	if n, e := RelayConfigurationDrafts(ctx, db.DB, c, cfg, []string{tenant}); e != nil || n != 1 {
		t.Fatalf("relay after lost acknowledgement: delivered=%d err=%v", n, e)
	}
	var deliveredAt sql.NullTime
	if err = admin.QueryRowContext(ctx, `SELECT delivered_at FROM supervisor.configuration_draft_request_outbox WHERE tenant_id=$1 AND site_id=$2 AND run_id=$3`, tenant, site, request.RunID).Scan(&deliveredAt); err != nil || !deliveredAt.Valid {
		t.Fatalf("outbox not marked delivered: %v err=%v", deliveredAt, err)
	}
	if n, e := RelayConfigurationDrafts(ctx, db.DB, c, cfg, []string{tenant}); e != nil || n != 0 {
		t.Fatalf("second relay: delivered=%d err=%v", n, e)
	}
	latest := c.GetWorkflow(ctx, workflowID, "")
	if latest.GetRunID() != startedRunID {
		t.Fatalf("duplicate relay created workflow run %s after original %s", latest.GetRunID(), startedRunID)
	}
	description, err := c.DescribeWorkflowExecution(ctx, workflowID, latest.GetRunID())
	if err != nil || description.WorkflowExecutionInfo.Status != enums.WORKFLOW_EXECUTION_STATUS_COMPLETED {
		t.Fatalf("workflow status=%v err=%v", description.GetWorkflowExecutionInfo().GetStatus(), err)
	}
}
