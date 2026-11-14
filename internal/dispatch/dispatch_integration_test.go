//go:build integration

package dispatch

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/google/uuid"
	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
)

func TestRealTemporalLostAckRestartRetriesAndReplay(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	runtimeURL, adminURL, address := os.Getenv("SUPERVISOR_TEST_DATABASE_URL"), os.Getenv("SUPERVISOR_TEST_ADMIN_URL"), os.Getenv("SUPERVISOR_TEST_TEMPORAL_ADDRESS")
	if runtimeURL == "" || adminURL == "" || address == "" {
		t.Fatal("real PostgreSQL and Temporal service addresses are required")
	}
	db, err := store.New(runtimeURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	admin, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	c, err := client.Dial(client.Options{HostPort: address, Namespace: "default"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	tenant, site, subject := uuid.NewString(), uuid.NewString(), uuid.NewString()
	issuer := "https://identity.example/realm"
	cfg := config.Config{TemporalQueue: "foundation-go-test-" + uuid.NewString(), Tenants: []string{tenant}}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, e := admin.ExecContext(ctx, query, args...); e != nil {
			t.Fatal(e)
		}
	}
	exec(`INSERT INTO supervisor.tenants(id,name) VALUES($1,'worker-test')`, tenant)
	defer func() {
		for _, name := range []string{"outbox", "audit_events", "commands", "memberships", "sites", "tenants"} {
			column := "tenant_id"
			if name == "tenants" {
				column = "id"
			}
			if _, e := admin.ExecContext(context.Background(), fmt.Sprintf("DELETE FROM supervisor.%s WHERE %s=$1", name, column), tenant); e != nil {
				t.Error(e)
			}
		}
	}()
	exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'line')`, tenant, site)
	exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'owner')`, tenant, site, issuer, subject)
	receipt, err := db.SubmitProbe(ctx, tenant, site, issuer, subject, uuid.NewString(), "restart probe")
	if err != nil {
		t.Fatal(err)
	}
	first, err := NewWorker(c, db, cfg)
	if err == nil {
		err = first.Start()
	}
	if err != nil {
		t.Fatal(err)
	}
	first.Stop()
	id := "foundation-go:" + tenant + ":" + receipt.ID
	run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: id, TaskQueue: cfg.TemporalQueue, WorkflowIDReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE}, Probe, Reference{tenant, receipt.ID})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.TerminateWorkflow(context.Background(), id, run.GetRunID(), "test cleanup") }()
	// Start succeeded, but the relay process lost its acknowledgement before the DB write.
	events, err := db.Pending(ctx, tenant)
	if err != nil || len(events) != 1 {
		t.Fatalf("pending=%v err=%v", events, err)
	}
	if count, e := Relay(ctx, db, c, cfg, []string{tenant}); e != nil || count != 1 {
		t.Fatalf("relay=%d err=%v", count, e)
	}
	if count, e := Relay(ctx, db, c, cfg, []string{tenant}); e != nil || count != 0 {
		t.Fatalf("duplicate relay=%d err=%v", count, e)
	}
	var attempts atomic.Int32
	actions := &Activities{Store: db, Allow: map[string]bool{tenant: true}}
	next := worker.New(c, cfg.TemporalQueue, worker.Options{})
	next.RegisterWorkflow(Probe)
	next.RegisterActivityWithOptions(func(ctx context.Context, reference Reference) (string, error) {
		if attempts.Add(1) <= 6 {
			return "", errors.New("injected_transient_database_outage")
		}
		return actions.Process(ctx, reference)
	}, activity.RegisterOptions{Name: "ProcessFoundationProbe"})
	if err = next.Start(); err != nil {
		t.Fatal(err)
	}
	defer next.Stop()
	var status string
	if err = run.Get(ctx, &status); err != nil || status != "completed" {
		t.Fatalf("result=%s err=%v", status, err)
	}
	if attempts.Load() != 7 {
		t.Fatalf("attempts=%d; expected recovery beyond five retries", attempts.Load())
	}
	if status, err = db.ProcessProbe(ctx, tenant, receipt.ID); err != nil || status != "completed" {
		t.Fatalf("replay result=%s err=%v", status, err)
	}
	var count int
	if err = admin.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.audit_events WHERE tenant_id=$1`, tenant).Scan(&count); err != nil || count != 2 {
		t.Fatalf("audit count=%d err=%v", count, err)
	}
	if err = admin.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.commands WHERE tenant_id=$1`, tenant).Scan(&count); err != nil || count != 1 {
		t.Fatalf("command count=%d err=%v", count, err)
	}
	// Re-execute production workflow logic against the actual persisted history.
	history := &historypb.History{}
	iter := c.GetWorkflowHistory(ctx, id, run.GetRunID(), false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	for iter.HasNext() {
		event, e := iter.Next()
		if e != nil {
			t.Fatal(e)
		}
		history.Events = append(history.Events, event)
	}
	replayer := worker.NewWorkflowReplayer()
	replayer.RegisterWorkflow(Probe)
	if err = replayer.ReplayWorkflowHistory(nil, history); err != nil {
		t.Fatal(err)
	}
}
