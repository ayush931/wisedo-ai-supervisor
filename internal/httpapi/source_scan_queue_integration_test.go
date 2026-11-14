//go:build integration

package httpapi

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/agent"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/dispatch"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/evidence"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/migrate"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/onboarding"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
)

// WIS-717: the worker's vision slots are shared by every tenant. While tenant A's pages hold them,
// tenant B's page gets a full queue. It must wait and then be read, keeping its image; before the
// fix the production ScanWorkflow quarantined it as EXTRACTION_FAILED and cleared its image
// without the model ever reading it.
func TestScanWaitsForAnotherTenantsFullVisionQueuePostgres(t *testing.T) {
	adminURL, runtimeURL := os.Getenv("SUPERVISOR_TEST_ADMIN_URL"), os.Getenv("SUPERVISOR_TEST_DATABASE_URL")
	if adminURL == "" || runtimeURL == "" {
		t.Fatal("BLOCKED: actual disposable PostgreSQL required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	control, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	name := "scanqueue_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = control.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	defer control.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	in := func(raw string) string {
		u, e := url.Parse(raw)
		if e != nil {
			t.Fatal(e)
		}
		u.Path = "/" + name
		return u.String()
	}
	admin, err := sql.Open("pgx", in(adminURL))
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if err = migrate.Apply(ctx, admin); err != nil {
		t.Fatal(err)
	}
	db, err := store.New(in(runtimeURL))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := admin.ExecContext(ctx, q, args...); e != nil {
			t.Fatalf("%s: %v", q, e)
		}
	}
	tenantA, siteA, tenantB, siteB := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	issuer := "https://scan-queue.invalid/" + uuid.NewString()
	exec(`INSERT INTO supervisor.tenants(id,name) VALUES($1,'tenant A'),($2,'tenant B')`, tenantA, tenantB)
	exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'site A'),($3,$4,'site B')`, tenantA, siteA, tenantB, siteB)
	operator := func(tenant, site string) evidence.Actor {
		subject, token := uuid.NewString(), uuid.NewString()
		sum := sha256.Sum256([]byte(token))
		exec(`INSERT INTO supervisor.browser_sessions(token_hash,subject,issuer,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,$4,now(),now(),now()+interval '1 hour',now())`, hex.EncodeToString(sum[:]), subject, issuer, uuid.NewString())
		exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'operator')`, tenant, site, issuer, subject)
		return evidence.Actor{Issuer: issuer, Subject: subject, SessionHash: hex.EncodeToString(sum[:])}
	}
	opA, opB := operator(tenantA, siteA), operator(tenantB, siteB)
	settings := onboarding.ScanSettings{Provider: agent.ProviderFixture, Model: "fixture-vision", ModelRevision: "sha256:vision", Enabled: true}
	svc := evidence.New(db.DB)
	queue := func(a evidence.Actor, tenant, site string, shade uint8) evidence.ScanJob {
		t.Helper()
		job, e := onboarding.QueueScan(ctx, svc, a, evidence.Scope{TenantID: tenant, SiteID: site},
			onboarding.DocumentUpload{Title: "page", DeclaredType: "image/png", Data: pagePNG(t, 300, 200, shade)}, uuid.NewString(), settings)
		if e != nil {
			t.Fatalf("queue: %v", e)
		}
		return job
	}
	a1, a2, b := queue(opA, tenantA, siteA, 10), queue(opA, tenantA, siteA, 20), queue(opB, tenantB, siteB, 50)

	// One worker's vision runtime with one slot and one waiting place. Tenant A's first page holds
	// the slot until released and its second page waits, so the queue is full. Tenant B's page
	// gets the third scripted reply: a transcription.
	entered, release := make(chan struct{}, 4), make(chan struct{})
	reply := func(content string) agent.ModelResponse {
		return agent.ModelResponse{SchemaVersion: agent.ModelSchemaVersion, Content: content, InputTokens: 300, OutputTokens: 40, FinishReason: "stop",
			ProviderActual: agent.ProviderFixture, ProviderModel: "fixture-vision", ModelRevision: "sha256:vision"}
	}
	text, _ := json.Marshal(map[string]any{"schema_version": 1, "run_id": uuid.NewSHA1(uuid.MustParse(b.RunID), []byte("extraction/ocr")).String(), "status": "transcribed", "text": "Press SOP: blank, press, trim."})
	p := agent.NewFixtureProvider(
		agent.FixtureStep{Err: &agent.ProviderError{Code: agent.CodeProviderFailed}, OnRequest: func(agent.ModelRequest) { entered <- struct{}{}; <-release }},
		agent.FixtureStep{Err: &agent.ProviderError{Code: agent.CodeProviderFailed}},
		agent.FixtureStep{Response: reply(string(text))},
	)
	st := agent.NewStore(db.DB)
	rt := &agent.Runtime{Store: st, Provider: p, Tools: &agent.Tools{Store: st}, Slots: agent.NewSlots(1, 1), Limits: agent.ExtractionLimits(),
		Model: "fixture-vision", ModelRevision: "sha256:vision", Options: agent.LoaderOptions{ModeEnabled: true}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	doneA := make(chan error, 2)
	extractA := func(j evidence.ScanJob) {
		_, e := rt.ExtractScan(ctx, agent.ScanRef{TenantID: tenantA, SiteID: siteA, JobID: j.JobID, RunID: j.RunID})
		doneA <- e
	}
	go extractA(a1)
	<-entered
	go extractA(a2)
	for rt.Slots.Waiting() != 1 {
		time.Sleep(10 * time.Millisecond)
	}

	// The production workflow and activities. Tenant A keeps the queue full through tenant B's first two
	// attempts (a real page holds the slot for minutes, longer than one activity retry) and finishes on
	// the third. The old workflow gave up after two queue-full attempts and quarantined the page.
	acts := &dispatch.Activities{Store: db, Vision: rt}
	var attempts, finals atomic.Int32
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.SetTestTimeout(90 * time.Second)
	env.RegisterWorkflow(dispatch.ScanWorkflow)
	env.RegisterActivityWithOptions(func(c context.Context, r dispatch.ScanRef) (agent.ScanResult, error) {
		if attempts.Add(1) == 3 {
			close(release)
			for range 2 {
				<-doneA
			}
		}
		return acts.RunScan(c, r)
	}, activity.RegisterOptions{Name: "RunScan"})
	env.RegisterActivityWithOptions(func(c context.Context, r dispatch.ScanRef) (agent.ScanResult, error) {
		finals.Add(1)
		return acts.FinalizeScanFailure(c, r)
	}, activity.RegisterOptions{Name: "FinalizeScanFailure"})
	env.ExecuteWorkflow(dispatch.ScanWorkflow, dispatch.ScanRef{Tenant: tenantB, Site: siteB, Job: b.JobID, Run: b.RunID})
	if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
		t.Fatalf("workflow: completed=%v err=%v", env.IsWorkflowCompleted(), env.GetWorkflowError())
	}
	if attempts.Load() != 3 || finals.Load() != 0 {
		t.Fatalf("RunScan attempts=%d finalizer calls=%d, want 3 and 0", attempts.Load(), finals.Load())
	}
	var state, reason string
	if err = admin.QueryRowContext(ctx, `SELECT state,coalesce(quarantine_reason,'') FROM supervisor.document_scan_jobs WHERE job_id=$1`, b.JobID).Scan(&state, &reason); err != nil {
		t.Fatal(err)
	}
	if state != "completed" || reason != "" {
		t.Fatalf("tenant B's page: state=%s quarantine_reason=%q, want completed", state, reason)
	}
	var stored int
	if err = admin.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.evidence_revisions WHERE tenant_id=$1`, tenantB).Scan(&stored); err != nil || stored != 1 {
		t.Fatalf("tenant B's transcription stored %d sources (%v), want 1", stored, err)
	}
}
