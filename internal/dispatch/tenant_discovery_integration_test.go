//go:build integration

package dispatch

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/cases"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/ingestion"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/migrate"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/google/uuid"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
)

func TestWorkerRelaysTenantProvisionedAfterStart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	admin, db := tenantDiscoveryDatabase(t, ctx)
	defer db.Close()
	address := os.Getenv("SUPERVISOR_TEST_TEMPORAL_ADDRESS")
	if address == "" {
		t.Fatal("BLOCKED: actual Temporal service address required")
	}
	c, err := client.Dial(client.Options{HostPort: address, Namespace: "default"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	tenantA, siteA, subjectA := uuid.NewString(), uuid.NewString(), uuid.NewString()
	tenantB, siteB, subjectB := uuid.NewString(), uuid.NewString(), uuid.NewString()
	tenantC, siteC, subjectC := uuid.NewString(), uuid.NewString(), uuid.NewString()
	tenantD, siteD, subjectD := uuid.NewString(), uuid.NewString(), uuid.NewString()
	caseID, sourceID, contextID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	issuer := "https://tenant-discovery.invalid/realm"
	queue := "tenant-discovery-" + uuid.NewString()
	cfg := config.Config{TemporalQueue: queue}
	w, err := NewWorker(c, db, cfg)
	if err == nil {
		err = w.Start()
	}
	if err != nil {
		t.Fatal(err)
	}
	defer w.Stop()
	if n, err := Tick(ctx, db, c, cfg, w.AgentEnabled); err != nil || n != 0 {
		t.Fatalf("empty worker tick: relayed=%d err=%v", n, err)
	}

	// Provision only after the worker is already running.
	insertDiscoveryTenant(t, ctx, admin, tenantA, siteA, issuer, subjectA)
	pendingA, err := db.SubmitProbe(ctx, tenantA, siteA, issuer, subjectA, uuid.NewString(), "created after worker start")
	if err != nil {
		t.Fatal(err)
	}
	if n, err := Tick(ctx, db, c, cfg, w.AgentEnabled); err != nil || n != 1 {
		t.Fatalf("first discovered tick: relayed=%d err=%v", n, err)
	}
	waitCommandStatus(t, ctx, admin, pendingA.ID, "completed")

	queuedA, err := db.SubmitProbe(ctx, tenantA, siteA, issuer, subjectA, uuid.NewString(), "queued before deactivation")
	if err != nil {
		t.Fatal(err)
	}
	insertDiscoveryTenant(t, ctx, admin, tenantB, siteB, issuer, subjectB)
	pendingB, err := db.SubmitProbe(ctx, tenantB, siteB, issuer, subjectB, uuid.NewString(), "active tenant remains served")
	if err != nil {
		t.Fatal(err)
	}
	seedQueuedTenantWork(t, ctx, admin, tenantA, siteA, sourceID, contextID, caseID)
	if _, err = admin.ExecContext(ctx, `UPDATE supervisor.tenants SET deactivated_at=now() WHERE id=$1`, tenantA); err != nil {
		t.Fatal(err)
	}
	t.Run("TestWorkerStopsRelayingDeactivatedTenantNextTick", func(t *testing.T) {
		if n, err := Tick(ctx, db, c, cfg, w.AgentEnabled); err != nil || n != 1 {
			t.Fatalf("post-deactivation tick: relayed=%d err=%v", n, err)
		}
		waitCommandStatus(t, ctx, admin, pendingB.ID, "completed")
		assertPendingProbe(t, ctx, admin, tenantA, queuedA.ID)
		// WIS-718: evaluation runs on the evaluator's own loop, not in Tick, so drive that pass the
		// way the worker does. A's planted context cannot load, so attempting it would fail the pass.
		if n, err := NewEvaluator(db).Pass(ctx, cfg.Tenants); err != nil || n != 0 {
			t.Fatalf("evaluator pass after deactivation: evaluated=%d err=%v", n, err)
		}
		for _, tc := range []struct {
			name  string
			query string
		}{
			{"operational event", `SELECT count(*) FROM supervisor.operational_event_outbox WHERE tenant_id=$1 AND source_id=$2`},
			{"case workflow", `SELECT count(*) FROM supervisor.case_outbox WHERE tenant_id=$1 AND case_id=$2 AND delivered_at IS NULL`},
			{"context evaluation", `SELECT count(*) FROM supervisor.plant_context_outbox o LEFT JOIN supervisor.case_context_evaluations e ON e.tenant_id=o.tenant_id AND e.site_id=o.site_id AND e.context_id=o.context_id WHERE o.tenant_id=$1 AND o.context_id=$2 AND e.context_id IS NULL`},
		} {
			var count int
			if err = admin.QueryRowContext(ctx, tc.query, tenantA, map[string]string{"operational event": sourceID, "case workflow": caseID, "context evaluation": contextID}[tc.name]).Scan(&count); err != nil || count != 1 {
				t.Errorf("%s queued rows=%d err=%v", tc.name, count, err)
			}
		}
		if _, err = c.DescribeWorkflowExecution(ctx, progressWorkflowID(ingestion.SourceRef{TenantID: tenantA, SiteID: siteA, SourceID: sourceID}), ""); err == nil {
			t.Fatal("deactivated tenant's operational workflow was started")
		}
	})
	a := &Activities{Store: db, Ingestion: ingestion.New(db.DB), Cases: cases.NewService(db.DB)}
	t.Run("TestDeactivatedTenantQueuedCaseWorkflowRefused", func(t *testing.T) {
		if _, err := a.Process(ctx, Reference{Tenant: tenantA, Command: queuedA.ID}); !nonRetryableScope(err) {
			t.Fatalf("queued probe scope error=%v", err)
		}
		if _, err := a.ReadCaseStatus(ctx, cases.WorkflowRef{TenantID: tenantA, SiteID: siteA, CaseID: caseID}); !nonRetryableScope(err) {
			t.Fatalf("queued case scope error=%v", err)
		}
		workflowID := cases.WorkflowID(tenantA, caseID)
		if _, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: workflowID, TaskQueue: queue}, SupervisorCaseWorkflow, cases.WorkflowRef{TenantID: tenantA, SiteID: siteA, CaseID: caseID}); err != nil {
			t.Fatal(err)
		}
		workflowErr := c.GetWorkflow(ctx, workflowID, "").Get(ctx, nil)
		if workflowErr == nil {
			t.Fatal("already queued case workflow continued for a deactivated tenant")
		}
		var escalations int
		if err = admin.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.case_transitions WHERE tenant_id=$1 AND case_id=$2 AND event='wait_deadline'`, tenantA, caseID).Scan(&escalations); err != nil || escalations != 0 {
			t.Fatalf("deactivated tenant escalations=%d err=%v", escalations, err)
		}
	})

	insertDiscoveryTenant(t, ctx, admin, tenantC, siteC, issuer, subjectC)
	pendingC, err := db.SubmitProbe(ctx, tenantC, siteC, issuer, subjectC, uuid.NewString(), "excluded by allowlist")
	if err != nil {
		t.Fatal(err)
	}
	t.Run("TestAllowlistExcludesActiveTenant", func(t *testing.T) {
		if n, err := Tick(ctx, db, c, config.Config{TemporalQueue: queue, Tenants: []string{tenantB}}, w.AgentEnabled); err != nil || n != 0 {
			t.Fatalf("allowlisted tick: relayed=%d err=%v", n, err)
		}
		assertPendingProbe(t, ctx, admin, tenantC, pendingC.ID)
		allowScoped := &Activities{Store: db, Allow: map[string]bool{tenantB: true}}
		if _, err := allowScoped.Process(ctx, Reference{Tenant: tenantC, Command: pendingC.ID}); !nonRetryableScope(err) {
			t.Fatalf("activity ignored allowlist: %v", err)
		}
	})

	insertDiscoveryTenant(t, ctx, admin, tenantD, siteD, issuer, subjectD)
	if n, err := Tick(ctx, db, c, config.Config{TemporalQueue: queue, Tenants: []string{tenantD}}, w.AgentEnabled); err != nil || n != 0 {
		t.Fatalf("discovery warmup tick: relayed=%d err=%v", n, err)
	}
	pendingD, err := db.SubmitProbe(ctx, tenantD, siteD, issuer, subjectD, uuid.NewString(), "retained through discovery failure")
	if err != nil {
		t.Fatal(err)
	}
	t.Run("TestDiscoveryErrorRelaysNothing", func(t *testing.T) {
		if _, err := admin.ExecContext(ctx, `REVOKE EXECUTE ON FUNCTION supervisor.active_tenant_ids() FROM supervisor_runtime`); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if _, err := admin.ExecContext(context.Background(), `GRANT EXECUTE ON FUNCTION supervisor.active_tenant_ids() TO supervisor_runtime`); err != nil {
				t.Error(err)
			}
		}()
		n, err := Tick(ctx, db, c, config.Config{TemporalQueue: queue, Tenants: []string{tenantD}}, w.AgentEnabled)
		if _, grantErr := admin.ExecContext(ctx, `GRANT EXECUTE ON FUNCTION supervisor.active_tenant_ids() TO supervisor_runtime`); grantErr != nil {
			t.Fatal(grantErr)
		}
		if err == nil || n != 0 {
			t.Fatalf("failed discovery relayed=%d err=%v", n, err)
		}
		assertPendingProbe(t, ctx, admin, tenantD, pendingD.ID)
		if n, err = Tick(ctx, db, c, config.Config{TemporalQueue: queue, Tenants: []string{tenantD}}, w.AgentEnabled); err != nil || n != 1 {
			t.Fatalf("live retry relayed=%d err=%v", n, err)
		}
		waitCommandStatus(t, ctx, admin, pendingD.ID, "completed")
	})
}

func tenantDiscoveryDatabase(t *testing.T, ctx context.Context) (*sql.DB, *store.Store) {
	t.Helper()
	adminURL, runtimeURL := os.Getenv("SUPERVISOR_TEST_ADMIN_URL"), os.Getenv("SUPERVISOR_TEST_DATABASE_URL")
	migrationPassword := os.Getenv("SUPERVISOR_MIGRATION_PASSWORD")
	if adminURL == "" || runtimeURL == "" || migrationPassword == "" {
		t.Fatal("BLOCKED: disposable PostgreSQL admin, runtime and migration credentials required")
	}
	root, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatal(err)
	}
	database := "tenant_discovery_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = root.ExecContext(ctx, "CREATE DATABASE "+database+" OWNER supervisor_migrator"); err != nil {
		_ = root.Close()
		t.Fatal(err)
	}
	var admin *sql.DB
	t.Cleanup(func() {
		if admin != nil {
			_ = admin.Close()
		}
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
	adminDBURL, err := url.Parse(adminURL)
	if err != nil {
		t.Fatal(err)
	}
	adminDBURL.Path = "/" + database
	admin, err = sql.Open("pgx", adminDBURL.String())
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"REVOKE CONNECT ON DATABASE " + database + " FROM PUBLIC",
		"GRANT CONNECT ON DATABASE " + database + " TO supervisor_migrator,supervisor_runtime",
	} {
		if _, err = root.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	migrationURL, err := url.Parse(adminURL)
	if err != nil {
		t.Fatal(err)
	}
	migrationURL.Path = "/" + database
	migrationURL.User = url.UserPassword("supervisor_migrator", migrationPassword)
	migrator, err := sql.Open("pgx", migrationURL.String())
	if err != nil {
		t.Fatal(err)
	}
	if err = migrate.Apply(ctx, migrator); err != nil {
		_ = migrator.Close()
		t.Fatal(err)
	}
	_ = migrator.Close()
	runtimeDBURL, err := url.Parse(runtimeURL)
	if err != nil {
		t.Fatal(err)
	}
	runtimeDBURL.Path = "/" + database
	db, err := store.New(runtimeDBURL.String())
	if err != nil {
		t.Fatal(err)
	}
	return admin, db
}

func insertDiscoveryTenant(t *testing.T, ctx context.Context, admin *sql.DB, tenant, site, issuer, subject string) {
	t.Helper()
	for _, q := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO supervisor.tenants(id,name) VALUES($1,'tenant discovery')`, []any{tenant}},
		{`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'tenant discovery site')`, []any{tenant, site}},
		{`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'owner')`, []any{tenant, site, issuer, subject}},
	} {
		if _, err := admin.ExecContext(ctx, q.query, q.args...); err != nil {
			t.Fatal(err)
		}
	}
}

func seedQueuedTenantWork(t *testing.T, ctx context.Context, admin *sql.DB, tenant, site, source, contextID, caseID string) {
	t.Helper()
	tx, err := admin.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, q := range []string{
		`SET LOCAL session_replication_role=replica`,
		`INSERT INTO supervisor.operational_source_heads(tenant_id,site_id,source_id,current_revision) VALUES('` + tenant + `','` + site + `','` + source + `',1)`,
		`INSERT INTO supervisor.operational_event_sequence_heads(tenant_id,site_id,source_id,last_sequence) VALUES('` + tenant + `','` + site + `','` + source + `',1)`,
		`INSERT INTO supervisor.operational_event_outbox(tenant_id,site_id,source_id,source_sequence,batch_id,event_type) VALUES('` + tenant + `','` + site + `','` + source + `',1,'` + uuid.NewString() + `','operational.event.received')`,
		`INSERT INTO supervisor.plant_context_outbox(tenant_id,site_id,context_id,event_type) VALUES('` + tenant + `','` + site + `','` + contextID + `','context.created')`,
		`INSERT INTO supervisor.case_outbox(tenant_id,site_id,id,case_id,case_revision,kind,event_id) VALUES('` + tenant + `','` + site + `','` + uuid.NewString() + `','` + caseID + `',1,'start','` + uuid.NewString() + `')`,
		`INSERT INTO supervisor.cases(tenant_id,site_id,id,line_id,order_id,exception_kind,episode,revision,context_snapshot_id,context_revision,context_digest,state,detected_at,latest_window_end,workflow_id) VALUES('` + tenant + `','` + site + `','` + caseID + `','` + uuid.NewString() + `','` + uuid.NewString() + `','production_shortfall',1,2,'` + contextID + `',1,'` + strings.Repeat("a", 64) + `','needs_input',now()-interval '1 hour',now()-interval '1 hour','case:` + tenant + `:` + caseID + `')`,
	} {
		if _, err = tx.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func waitCommandStatus(t *testing.T, ctx context.Context, admin *sql.DB, command, want string) {
	t.Helper()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		var status string
		err := admin.QueryRowContext(ctx, `SELECT status FROM supervisor.commands WHERE id=$1`, command).Scan(&status)
		if err == nil && status == want {
			return
		}
		if ctx.Err() != nil {
			t.Fatalf("command %s status=%q err=%v, want %q", command, status, err, want)
		}
		<-ticker.C
	}
}

func assertPendingProbe(t *testing.T, ctx context.Context, admin *sql.DB, tenant, command string) {
	t.Helper()
	var pending int
	if err := admin.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.outbox WHERE tenant_id=$1 AND command_id=$2 AND delivered_at IS NULL`, tenant, command).Scan(&pending); err != nil || pending != 1 {
		t.Fatalf("tenant %s command %s pending outbox=%d err=%v", tenant, command, pending, err)
	}
}

func nonRetryableScope(err error) bool {
	var app *temporal.ApplicationError
	return errors.As(err, &app) && app.Type() == "scope" && app.NonRetryable()
}
