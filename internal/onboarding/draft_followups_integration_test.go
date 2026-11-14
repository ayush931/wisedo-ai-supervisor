//go:build integration

package onboarding

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/agent"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/migrate"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/recovery"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestReapStaleAgentTurnsDeliveredWindow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	admin, db := draftFollowupDatabase(t, ctx)
	tenant, site, sessionID := seedDraftFollowupSession(t, ctx, admin)
	staleRun := seedDraftFollowupRun(t, ctx, admin, db, tenant, site, sessionID, "questions", false)
	insideRun := seedDraftFollowupRun(t, ctx, admin, db, tenant, site, sessionID, "questions", false)
	for runID, age := range map[string]time.Duration{
		staleRun:  interviewTurnDeliveredReapAfter + time.Second,
		insideRun: interviewTurnDeliveredReapAfter - time.Second,
	} {
		if _, err := admin.ExecContext(ctx, `UPDATE supervisor.interview_agent_turn_outbox SET delivered_at=clock_timestamp()-($4::bigint*interval '1 second') WHERE tenant_id=$1 AND site_id=$2 AND run_id=$3`, tenant, site, runID, int64(age/time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	if err := agent.NewStore(db.DB).InTx(ctx, agent.RunScope{TenantID: tenant, SiteID: site}, func(tx *sql.Tx) error {
		return reapStaleAgentTurns(ctx, tx, Scope{TenantID: tenant, SiteID: site}, sessionID)
	}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		runID, state, status, reason string
	}{
		{staleRun, agent.RunCompleted, recovery.StatusNeedsInput, agent.CodeActivityFailed},
		{insideRun, agent.RunRunning, "", ""},
	} {
		var state, status, reason string
		if err := admin.QueryRowContext(ctx, `SELECT state,COALESCE(outcome_status,''),COALESCE(outcome_reasons[1],'') FROM supervisor.agent_runs WHERE tenant_id=$1 AND site_id=$2 AND id=$3`, tenant, site, tc.runID).Scan(&state, &status, &reason); err != nil {
			t.Fatal(err)
		}
		if state != tc.state || status != tc.status || reason != tc.reason {
			t.Errorf("run %s = %s/%s/%s, want %s/%s/%s", tc.runID, state, status, reason, tc.state, tc.status, tc.reason)
		}
	}
}

func TestFinalizeConfigurationDraftFailureRecordsOutcomeAndNoOpsTerminalRun(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	admin, db := draftFollowupDatabase(t, ctx)
	tenant, site, sessionID := seedDraftFollowupSession(t, ctx, admin)
	runningRun := seedDraftFollowupRun(t, ctx, admin, db, tenant, site, sessionID, "drafts", true)
	terminalRun := seedDraftFollowupRun(t, ctx, admin, db, tenant, site, sessionID, "drafts", true)
	if err := agent.NewStore(db.DB).InTx(ctx, agent.RunScope{TenantID: tenant, SiteID: site}, func(tx *sql.Tx) error {
		return agent.FinishRunTx(ctx, tx, agent.RunScope{TenantID: tenant, SiteID: site}, terminalRun, agent.Finish{State: agent.RunCompleted, OutcomeStatus: recovery.StatusRefused, OutcomeReasons: []string{"MODEL_REJECTED"}})
	}); err != nil {
		t.Fatal(err)
	}
	ref := agent.ConfigurationDraftRef{TenantID: tenant, SiteID: site, SessionID: sessionID, RunID: runningRun}
	result, err := agent.FinalizeConfigurationDraftFailure(ctx, agent.NewStore(db.DB), ref)
	if err != nil || result.State != agent.RunCompleted || result.OutcomeStatus != recovery.StatusNeedsInput || len(result.OutcomeReasons) != 1 || result.OutcomeReasons[0] != agent.CodeActivityFailed {
		t.Fatalf("failure finalization = %+v, err=%v", result, err)
	}
	terminalRef := ref
	terminalRef.RunID = terminalRun
	before := runOutcome(t, ctx, admin, tenant, site, terminalRun)
	result, err = agent.FinalizeConfigurationDraftFailure(ctx, agent.NewStore(db.DB), terminalRef)
	if err != nil || !result.Replayed || result.State != agent.RunCompleted || result.OutcomeStatus != recovery.StatusRefused || len(result.OutcomeReasons) != 1 || result.OutcomeReasons[0] != "MODEL_REJECTED" {
		t.Fatalf("terminal finalization = %+v, err=%v", result, err)
	}
	if after := runOutcome(t, ctx, admin, tenant, site, terminalRun); after != before {
		t.Fatalf("terminal run changed: before=%+v after=%+v", before, after)
	}
}

type runOutcomeRow struct {
	state, status, reason string
	endedAt               time.Time
}

func runOutcome(t *testing.T, ctx context.Context, db *sql.DB, tenant, site, runID string) runOutcomeRow {
	t.Helper()
	var out runOutcomeRow
	if err := db.QueryRowContext(ctx, `SELECT state,COALESCE(outcome_status,''),COALESCE(outcome_reasons[1],''),ended_at FROM supervisor.agent_runs WHERE tenant_id=$1 AND site_id=$2 AND id=$3`, tenant, site, runID).Scan(&out.state, &out.status, &out.reason, &out.endedAt); err != nil {
		t.Fatal(err)
	}
	return out
}

func seedDraftFollowupSession(t *testing.T, ctx context.Context, db *sql.DB) (tenant, site, sessionID string) {
	t.Helper()
	tenant, site, sessionID = uuid.NewString(), uuid.NewString(), uuid.NewString()
	issuer, subject := "https://draft-followups.invalid/realm", uuid.NewString()
	for _, q := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO supervisor.tenants(id,name) VALUES($1,'draft followups')`, []any{tenant}},
		{`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'draft followups')`, []any{tenant, site}},
		{`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'owner')`, []any{tenant, site, issuer, subject}},
		{`INSERT INTO supervisor.interview_sessions(tenant_id,site_id,session_id,role,participant_actor_id,participant_issuer,participant_subject,status,revision) VALUES($1,$2,$3,'owner',$4,$5,$6,'active',1)`, []any{tenant, site, sessionID, uuid.NewString(), issuer, subject}},
	} {
		if _, err := db.ExecContext(ctx, q.query, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	return tenant, site, sessionID
}

func seedDraftFollowupRun(t *testing.T, ctx context.Context, admin *sql.DB, db *store.Store, tenant, site, sessionID, purpose string, request bool) string {
	t.Helper()
	runID, requestID := uuid.NewString(), uuid.NewString()
	err := agent.NewStore(db.DB).InTx(ctx, agent.RunScope{TenantID: tenant, SiteID: site}, func(tx *sql.Tx) error {
		run := agent.Run{ID: runID, RequestKind: agent.RequestConversation, ConversationID: sessionID, RequestID: requestID, State: agent.RunRunning, TraceID: agent.NewTraceID(), ProviderName: "fixture", Model: "fixture-model", ModelRevision: "r1", PromptVersion: agent.DraftPromptVersion, ToolVersion: agent.ToolVersion, PolicyRevision: 1}
		if err := agent.CreateRunTx(ctx, tx, agent.RunScope{TenantID: tenant, SiteID: site}, run, agent.InterviewLimits()); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO supervisor.interview_agent_turns(tenant_id,site_id,session_id,run_id,session_revision,purpose) VALUES($1,$2,$3,$4,1,$5)`, tenant, site, sessionID, runID, purpose); err != nil {
			return err
		}
		if request {
			_, err := tx.ExecContext(ctx, `INSERT INTO supervisor.configuration_draft_requests(tenant_id,site_id,session_id,run_id,requested_by_issuer,requested_by_subject,kinds,request_sha256) VALUES($1,$2,$3,$4,'https://draft-followups.invalid/realm','requester',ARRAY['line']::text[],$5)`, tenant, site, sessionID, runID, strings.Repeat("a", 64))
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO supervisor.interview_agent_turn_outbox(tenant_id,site_id,id,session_id,run_id) VALUES($1,$2,$3,$4,$5)`, tenant, site, uuid.NewString(), sessionID, runID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return runID
}

func draftFollowupDatabase(t *testing.T, ctx context.Context) (*sql.DB, *store.Store) {
	t.Helper()
	adminURL, runtimeURL, migrationPassword := os.Getenv("SUPERVISOR_TEST_ADMIN_URL"), os.Getenv("SUPERVISOR_TEST_DATABASE_URL"), os.Getenv("SUPERVISOR_MIGRATION_PASSWORD")
	if adminURL == "" || runtimeURL == "" || migrationPassword == "" {
		t.Fatal("BLOCKED: disposable PostgreSQL admin, runtime and migration credentials required")
	}
	root, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatal(err)
	}
	database := "draft_followups_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = root.ExecContext(ctx, "CREATE DATABASE "+database+" OWNER supervisor_migrator"); err != nil {
		_ = root.Close()
		t.Fatal(err)
	}
	databaseURL := func(raw string, user string) string {
		u, parseErr := url.Parse(raw)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		u.Path = "/" + database
		if user != "" {
			u.User = url.UserPassword(user, migrationPassword)
		}
		return u.String()
	}
	for _, q := range []string{"REVOKE CONNECT ON DATABASE " + database + " FROM PUBLIC", "GRANT CONNECT ON DATABASE " + database + " TO supervisor_migrator,supervisor_runtime"} {
		if _, err = root.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	admin, err := sql.Open("pgx", databaseURL(adminURL, ""))
	if err != nil {
		t.Fatal(err)
	}
	migrator, err := sql.Open("pgx", databaseURL(adminURL, "supervisor_migrator"))
	if err != nil {
		t.Fatal(err)
	}
	if err = migrate.Apply(ctx, migrator); err != nil {
		_ = migrator.Close()
		t.Fatal(err)
	}
	_ = migrator.Close()
	db, err := store.New(databaseURL(runtimeURL, ""))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = db.Close()
		_ = admin.Close()
		_ = root.Close()
		cleanup, e := sql.Open("pgx", adminURL)
		if e != nil {
			t.Error(e)
			return
		}
		defer cleanup.Close()
		if _, e = cleanup.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+database+" WITH (FORCE)"); e != nil {
			t.Error(e)
		}
	})
	return admin, db
}
