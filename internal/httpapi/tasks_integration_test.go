//go:build integration

package httpapi

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/agent"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/approvals"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/cases"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/dispatch"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/evidence"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/identity"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/membership"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/migrate"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/tasks"
	"github.com/google/uuid"
	temporalclient "go.temporal.io/sdk/client"
)

// TestApprovedTaskExecutionPostgres drives WIS-277 through the public entry points against real
// PostgreSQL as the runtime role: a proposal is submitted, a distinct supervisor approves it over
// HTTP, the deterministic executor runs, and the task is read back over HTTP. Each scenario gets
// its own case so they do not depend on order.
func TestApprovedTaskExecutionPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	adminURL, runtimeURL := os.Getenv("SUPERVISOR_TEST_ADMIN_URL"), os.Getenv("SUPERVISOR_TEST_DATABASE_URL")
	if adminURL == "" || runtimeURL == "" {
		t.Fatal("BLOCKED: real PostgreSQL admin and runtime URLs required")
	}
	admin, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close() })
	database := "task_exec_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = admin.ExecContext(ctx, "CREATE DATABASE "+database); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if _, e := admin.ExecContext(cleanup, "DROP DATABASE IF EXISTS "+database+" WITH (FORCE)"); e != nil {
			t.Error(e)
		}
	})
	connect := func(raw string) *sql.DB {
		t.Helper()
		u, e := url.Parse(raw)
		if e != nil {
			t.Fatal(e)
		}
		u.Path = "/" + database
		db, e := sql.Open("pgx", u.String())
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { db.Close() })
		return db
	}
	control, runtime := connect(adminURL), connect(runtimeURL)
	if err = migrate.Apply(ctx, control); err != nil {
		t.Fatal(err)
	}
	db, err := store.New(runtimeURLFor(runtimeURL, database))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := control.ExecContext(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	// planted writes fixture rows behind the WIS-272/276 pipelines as the superuser in tenant
	// scope, with triggers and FK checks off; it is never used for the behavior under test.
	planted := func(tenant string, statements ...string) {
		t.Helper()
		tx, e := control.BeginTx(ctx, nil)
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback()
		for _, s := range append([]string{`SET LOCAL session_replication_role = replica`, `SELECT set_config('supervisor.tenant_id','` + tenant + `',true)`}, statements...) {
			if _, e = tx.ExecContext(ctx, s); e != nil {
				t.Fatalf("%s: %v", s, e)
			}
		}
		if e = tx.Commit(); e != nil {
			t.Fatal(e)
		}
	}
	scalar := func(tenant, q string, args ...any) string {
		t.Helper()
		tx, e := control.BeginTx(ctx, nil)
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback()
		if _, e = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, tenant); e != nil {
			t.Fatal(e)
		}
		var v sql.NullString
		if e = tx.QueryRowContext(ctx, q, args...).Scan(&v); e != nil {
			t.Fatalf("%s: %v", q, e)
		}
		return v.String
	}

	tenant, site, otherTenant, otherSite := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	submitter, approver, rosterSupervisor, operator, viewer, outsider := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	owner, staleSupervisor, otherOperator := uuid.NewString(), uuid.NewString(), uuid.NewString()
	issuer, origin := "https://task-exec.invalid/"+uuid.NewString(), "http://127.0.0.1:58177"
	exec(`INSERT INTO supervisor.tenants(id,name) VALUES($1,'synthetic task test'),($2,'other tenant')`, tenant, otherTenant)
	exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'task site'),($3,$4,'other site')`, tenant, site, otherTenant, otherSite)
	exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'supervisor'),($1,$2,$3,$5,'supervisor'),($1,$2,$3,$6,'supervisor'),($1,$2,$3,$7,'operator'),($1,$2,$3,$8,'viewer')`,
		tenant, site, issuer, submitter, approver, rosterSupervisor, operator, viewer)
	exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'owner'),($1,$2,$3,$5,'supervisor'),($1,$2,$3,$6,'operator')`, tenant, site, issuer, owner, staleSupervisor, otherOperator)
	exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'supervisor')`, otherTenant, otherSite, issuer, outsider)
	type session struct{ token, csrf, hash, subject string }
	seedAged := func(subject string, authAge time.Duration) session {
		t.Helper()
		v := session{token: uuid.NewString(), csrf: uuid.NewString(), subject: subject}
		sum := sha256.Sum256([]byte(v.token))
		v.hash = hex.EncodeToString(sum[:])
		exec(`INSERT INTO supervisor.browser_sessions(token_hash,subject,issuer,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,$4,now(),now(),now()+interval '1 hour',now()-$5::interval)`, v.hash, subject, issuer, v.csrf, strconv.Itoa(int(authAge.Seconds()))+" seconds")
		return v
	}
	seed := func(subject string) session { return seedAged(subject, 0) }
	approverS, viewerS, outsiderS, submitterS := seed(approver), seed(viewer), seed(outsider), seed(submitter)
	operatorS, ownerS, staleS, otherOperatorS := seed(operator), seed(owner), seedAged(staleSupervisor, 2*time.Hour), seed(otherOperator)
	auth, err := identity.New(ctx, identity.Config{Issuer: issuer, ClientID: "task-test", ClientSecret: "synthetic-only", RedirectURI: origin + "/auth/callback"}, runtime, nil)
	if err != nil {
		t.Fatal(err)
	}
	handler := New(config.Config{Environment: "local", WebDir: t.TempDir()}, db, auth)
	call := func(actor session, method, path, key, revision, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.AddCookie(&http.Cookie{Name: "supervisor", Value: actor.token})
		r.Header.Set("Origin", origin)
		r.Header.Set("X-CSRF-Token", actor.csrf)
		r.Header.Set("Content-Type", "application/json")
		if key != "" {
			r.Header.Set("Idempotency-Key", key)
		}
		if revision != "" {
			r.Header.Set("If-Match", revision)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	expect := func(w *httptest.ResponseRecorder, status int, code string) {
		t.Helper()
		if w.Code != status {
			t.Fatalf("HTTP %d want %d: %s", w.Code, status, w.Body.String())
		}
		if code != "" && !strings.Contains(w.Body.String(), `"code":"`+code+`"`) {
			t.Fatalf("problem code want %s: %s", code, w.Body.String())
		}
	}

	proposals := approvals.NewService(db.DB)
	scope := approvals.Scope{TenantID: tenant, SiteID: site}
	// The worker's executor: bound evidence is re-evaluated with the agent runtime's rules.
	executor := cases.NewService(db.DB).WithBoundEvidence(agent.DispatchEvidenceCode)
	sources := evidence.New(runtime)
	runs := agent.NewStore(db.DB)
	assigneeID := membership.ActorID(issuer, operator).String()
	// policy is the policy revision the next fixture's payload is approved under.
	policy := int64(approvals.PolicyRevision)
	contextDigest := strings.Repeat("a", 64)

	type fixture struct{ caseID, line, order, proposalID, sourceID string }
	// awaitingOn plants a case at awaiting_approval (revision 4) on its own line, order and
	// context, confirms the operator on that line's maintenance roster, submits a proposal naming
	// the operator and asset (the line when empty), and has a distinct supervisor decide it over
	// HTTP.
	awaitingOn := func(approve bool, asset string) fixture {
		t.Helper()
		f := fixture{caseID: uuid.NewString(), line: uuid.NewString(), order: uuid.NewString()}
		if asset == "" {
			asset = f.line
		}
		ctxID := uuid.NewString()
		snap := `{"readiness":"NOT_READY","digest":"` + contextDigest + `","context_id":"` + ctxID + `","order_id":"` + f.order + `","asset_id":"` + f.line + `","site_id":"` + site + `"}`
		planted(tenant,
			`INSERT INTO supervisor.knowledge_entity_heads(tenant_id,site_id,id,current_revision) VALUES('`+tenant+`','`+site+`','`+f.line+`',1),('`+tenant+`','`+site+`','`+f.order+`',1)`,
			`INSERT INTO supervisor.plant_contexts(tenant_id,site_id,id,order_id,asset_id,window_start,window_end,process_snapshot_id,process_snapshot_sha256,readiness,snapshot,digest,actor_issuer,actor_subject,evaluated_at)
 VALUES('`+tenant+`','`+site+`','`+ctxID+`','`+f.order+`','`+f.line+`','2026-10-22T06:00:00Z','2026-10-22T06:15:00Z','`+uuid.NewString()+`','`+strings.Repeat("d", 64)+`','NOT_READY','`+snap+`','`+contextDigest+`','test','operator','2026-10-22T06:15:00Z')`,
			`INSERT INTO supervisor.cases(tenant_id,site_id,id,line_id,order_id,exception_kind,episode,revision,context_snapshot_id,context_revision,context_digest,state,detected_at,latest_window_end,workflow_id)
 VALUES('`+tenant+`','`+site+`','`+f.caseID+`','`+f.line+`','`+f.order+`','production_shortfall',1,3,'`+ctxID+`',1,'`+contextDigest+`','proposed','2026-10-22T06:15:00Z','2026-10-22T06:15:00Z','case:`+tenant+`:`+f.caseID+`')`,
			`INSERT INTO supervisor.case_transitions(tenant_id,site_id,case_id,revision,from_state,to_state,event,actor_issuer,actor_subject) VALUES('`+tenant+`','`+site+`','`+f.caseID+`',1,'none','detected','trigger','svc','ingestion'),('`+tenant+`','`+site+`','`+f.caseID+`',2,'detected','investigating','context_ready','svc','case-workflow'),('`+tenant+`','`+site+`','`+f.caseID+`',3,'investigating','proposed','proposal_validated','svc','case-workflow')`,
			`INSERT INTO supervisor.maintenance_roster_heads(tenant_id,site_id,line_id,assignee_actor_id,assignee_issuer,assignee_subject,confirmed,revision) VALUES('`+tenant+`','`+site+`','`+f.line+`','`+assigneeID+`','`+issuer+`','`+operator+`',true,1)`,
			`INSERT INTO supervisor.maintenance_roster_events(tenant_id,site_id,line_id,assignee_actor_id,revision,confirmed,reason,actor_issuer,actor_subject,auth_time) VALUES('`+tenant+`','`+site+`','`+f.line+`','`+assigneeID+`',1,true,'on call','`+issuer+`','`+rosterSupervisor+`',now())`)
		// The generating run captured one operator note; the payload binds that exact revision,
		// which the executor re-evaluates at dispatch.
		note, e := sources.CreateOrRevise(ctx, evidence.Actor{Issuer: issuer, Subject: submitter, SessionHash: submitterS.hash}, evidence.Scope{TenantID: tenant, SiteID: site},
			evidence.Input{SourceID: uuid.NewString(), Kind: "manual", Title: "Operator note", Text: "Infeed jam at 06:10"}, uuid.NewString(), 0)
		if e != nil {
			t.Fatal(e)
		}
		f.sourceID = note.SourceID
		runID := uuid.NewString()
		rs := agent.RunScope{TenantID: tenant, SiteID: site}
		if e = runs.InTx(ctx, rs, func(tx *sql.Tx) error {
			if e := agent.CreateRunTx(ctx, tx, rs, agent.Run{ID: runID, RequestKind: agent.RequestCase, CaseID: f.caseID, CaseRevision: 3, TraceID: agent.NewTraceID(), ProviderName: agent.ProviderFixture,
				Model: "fixture-model", ModelRevision: "sha256:fixture", PromptVersion: agent.PromptVersion, ToolVersion: agent.ToolVersion, PolicyRevision: 1}, agent.DefaultLimits()); e != nil {
				return e
			}
			return agent.AddRefsTx(ctx, tx, rs, runID, []agent.RunRef{{Kind: agent.RefEvidence, RefID: note.SourceID, Revision: note.Revision, SHA256: note.SHA256, AccessEpoch: note.AccessEpoch}})
		}); e != nil {
			t.Fatal(e)
		}
		v, e := proposals.Submit(ctx, scope, approvals.Submission{SubmittedBy: &approvals.Actor{Issuer: issuer, Subject: submitter}, Payload: approvals.Payload{
			ActionKind: "maintenance_investigation.create", Template: "inspect_report_only",
			AssetID: asset, LineID: f.line, OrderID: f.order, AssigneeID: assigneeID,
			Instructions: "Inspect the infeed conveyor and report findings.", DueAt: time.Now().UTC().Add(4 * time.Hour).Truncate(time.Second),
			CaseID: f.caseID, CaseRevision: 3, ContextSnapshotID: ctxID, ContextRevision: 1, ContextDigest: contextDigest,
			ConfigRevision: 1, GraphRevision: 1, PolicyRevision: policy,
			Evidence: []approvals.Ref{{Kind: agent.RefEvidence, ID: note.SourceID, Revision: note.Revision}}, GeneratedByRun: runID}})
		if e != nil {
			t.Fatal(e)
		}
		f.proposalID = v.ID
		// The investigation moved the case on to wait for this decision.
		planted(tenant,
			`UPDATE supervisor.cases SET state='awaiting_approval',revision=4 WHERE tenant_id='`+tenant+`' AND id='`+f.caseID+`'`,
			`INSERT INTO supervisor.case_transitions(tenant_id,site_id,case_id,revision,from_state,to_state,event,actor_issuer,actor_subject) VALUES('`+tenant+`','`+site+`','`+f.caseID+`',4,'proposed','awaiting_approval','approval_requested','svc','case-workflow')`)
		decision := "approve"
		if !approve {
			decision = "reject"
		}
		expect(call(approverS, "POST", "/v1/sites/"+site+"/proposals/"+f.proposalID+"/decisions", uuid.NewString(), `"1"`,
			`{"decision":"`+decision+`","expected_revision":1,"reason":"reviewed"}`), 200, "")
		return f
	}
	awaiting := func(approve bool) fixture { return awaitingOn(approve, "") }
	run := func(enabled bool) []cases.Execution {
		t.Helper()
		out, e := executor.ExecuteApproved(ctx, tenant, enabled, 50)
		if e != nil {
			t.Fatal(e)
		}
		return out
	}
	outcome := func(out []cases.Execution, caseID string) cases.Execution {
		t.Helper()
		for _, e := range out {
			if e.CaseID == caseID {
				return e
			}
		}
		return cases.Execution{}
	}
	caseState := func(caseID string) string {
		return scalar(tenant, `SELECT state||'@'||revision FROM supervisor.cases WHERE id=$1`, caseID)
	}
	approvalState := func(proposalID string) string {
		return scalar(tenant, `SELECT state FROM supervisor.approval_states WHERE proposal_id=$1`, proposalID)
	}
	count := func(q string, args ...any) int {
		n, e := strconv.Atoi(scalar(tenant, q, args...))
		if e != nil {
			t.Fatal(e)
		}
		return n
	}
	// refused asserts the executor turned an approved proposal down with a specific reason and
	// left no task: approval stale, a rejected action, the case back at needs_input.
	refused := func(f fixture, out []cases.Execution, reason string) {
		t.Helper()
		got := outcome(out, f.caseID)
		if got.Outcome != cases.ExecutionRefused || got.Reason != "DISPATCH_"+reason {
			t.Fatalf("want refused DISPATCH_%s, got %+v", reason, got)
		}
		if s := approvalState(f.proposalID); s != "stale" {
			t.Fatalf("approval must be stale, got %s", s)
		}
		if s := caseState(f.caseID); s != "needs_input@5" {
			t.Fatalf("case must return to needs_input, got %s", s)
		}
		if r := scalar(tenant, `SELECT reason FROM supervisor.case_transitions WHERE case_id=$1 AND revision=5`, f.caseID); r != "DISPATCH_"+reason {
			t.Fatalf("case transition reason %q", r)
		}
		if a := scalar(tenant, `SELECT state||':'||reason FROM supervisor.approved_actions WHERE proposal_id=$1`, f.proposalID); a != "rejected:"+reason {
			t.Fatalf("action ledger %q", a)
		}
		if n := count(`SELECT count(*) FROM supervisor.tasks WHERE case_id=$1`, f.caseID); n != 0 {
			t.Fatalf("a refused dispatch created %d tasks", n)
		}
	}

	t.Run("execution not enabled never consumes an approval", func(t *testing.T) {
		f := awaiting(true)
		out := run(false)
		if got := outcome(out, f.caseID); got.Outcome != "" {
			t.Fatalf("a live approval is not a closed decision while execution is off: %+v", got)
		}
		if s := approvalState(f.proposalID); s != "approved" {
			t.Fatalf("approval must stay approved, got %s", s)
		}
		if s := caseState(f.caseID); s != "awaiting_approval@4" {
			t.Fatalf("case must keep waiting, got %s", s)
		}
	})

	t.Run("approved proposal becomes exactly one task and the case observes it", func(t *testing.T) {
		f := awaiting(true)
		got := outcome(run(true), f.caseID)
		if got.Outcome != cases.ExecutionCommitted || got.TaskID == "" || got.ActionID == "" {
			t.Fatalf("want committed with a task, got %+v", got)
		}
		if s := approvalState(f.proposalID); s != "consumed" {
			t.Fatalf("approval must be consumed, got %s", s)
		}
		if s := caseState(f.caseID); s != "observing@6" {
			t.Fatalf("case must reach observing at revision 6, got %s", s)
		}
		if e := scalar(tenant, `SELECT string_agg(event||':'||to_state,',' ORDER BY revision) FROM supervisor.case_transitions WHERE case_id=$1 AND revision>4`, f.caseID); e != "approval_granted:executing,task_authoritative:observing" {
			t.Fatalf("case transitions %q", e)
		}
		if n := count(`SELECT count(*) FROM supervisor.case_outbox WHERE case_id=$1 AND case_revision=6 AND kind='signal'`, f.caseID); n != 1 {
			t.Fatalf("the case workflow must be signaled once, got %d", n)
		}
		if n := count(`SELECT count(*) FROM supervisor.approval_audit WHERE proposal_id=$1 AND event_type='proposal.consumed' AND actor_issuer='supervisor:executor'`, f.proposalID); n != 1 {
			t.Fatalf("consumption audit rows %d", n)
		}

		// Read back over HTTP: any active member sees the task exactly as approved.
		w := call(viewerS, "GET", "/v1/sites/"+site+"/tasks/"+got.TaskID, "", "", "")
		expect(w, 200, "")
		var task tasks.TaskV1
		if e := json.Unmarshal(w.Body.Bytes(), &task); e != nil {
			t.Fatal(e)
		}
		if task.ActionID != got.ActionID || task.CaseID != f.caseID || task.State != tasks.Assigned || task.Revision != 1 || task.AssigneeID != assigneeID ||
			task.AssigneeSubject != operator || task.LineID != f.line || task.OrderID != f.order || task.Template != "inspect_report_only" ||
			task.Description != "Inspect the infeed conveyor and report findings." || len(task.Events) != 1 || task.Events[0].Event != "create" {
			t.Fatalf("task read back %+v", task)
		}
		if w.Header().Get("ETag") != `"1"` {
			t.Fatalf("ETag %q", w.Header().Get("ETag"))
		}
		list := call(viewerS, "GET", "/v1/sites/"+site+"/tasks?case_id="+f.caseID, "", "", "")
		expect(list, 200, "")
		if !strings.Contains(list.Body.String(), got.TaskID) {
			t.Fatalf("task missing from the case list: %s", list.Body.String())
		}
		// WIS-634: an operator reads only the tasks assigned to them. The assignee sees this one;
		// another operator of the same site gets the 404 an unknown task gets, and an empty list.
		expect(call(operatorS, "GET", "/v1/sites/"+site+"/tasks/"+got.TaskID, "", "", ""), 200, "")
		if own := call(operatorS, "GET", "/v1/sites/"+site+"/tasks", "", "", ""); own.Code != 200 || !strings.Contains(own.Body.String(), got.TaskID) {
			t.Fatalf("the assignee's own list: %d %s", own.Code, own.Body.String())
		}
		withoutCorrelation := func(w *httptest.ResponseRecorder) map[string]any {
			t.Helper()
			var m map[string]any
			if e := json.Unmarshal(w.Body.Bytes(), &m); e != nil {
				t.Fatal(e)
			}
			delete(m, "correlation_id")
			return m
		}
		foreign := call(otherOperatorS, "GET", "/v1/sites/"+site+"/tasks/"+got.TaskID, "", "", "")
		unknown := call(otherOperatorS, "GET", "/v1/sites/"+site+"/tasks/"+uuid.NewString(), "", "", "")
		expect(foreign, 404, "NOT_FOUND")
		if a, b := withoutCorrelation(foreign), withoutCorrelation(unknown); !reflect.DeepEqual(a, b) {
			t.Fatalf("another operator's task discloses its existence: %v vs %v", a, b)
		}
		for _, q := range []string{"", "?case_id=" + f.caseID} {
			others := call(otherOperatorS, "GET", "/v1/sites/"+site+"/tasks"+q, "", "", "")
			if others.Code != 200 || strings.Contains(others.Body.String(), got.TaskID) {
				t.Fatalf("another operator's list%s shows the task: %d %s", q, others.Code, others.Body.String())
			}
		}

		// A retry (worker restart, Temporal redelivery) finds nothing to do: never a second
		// action or task.
		if again := outcome(run(true), f.caseID); again.Outcome != "" {
			t.Fatalf("a committed case must not be executed again: %+v", again)
		}
		if n := count(`SELECT count(*) FROM supervisor.tasks WHERE case_id=$1`, f.caseID); n != 1 {
			t.Fatalf("tasks after retry: %d", n)
		}

		// Tenant isolation: another tenant's supervisor gets the non-disclosing 404.
		expect(call(outsiderS, "GET", "/v1/sites/"+site+"/tasks/"+got.TaskID, "", "", ""), 404, "NOT_FOUND")
		expect(call(outsiderS, "GET", "/v1/sites/"+otherSite+"/tasks/"+got.TaskID, "", "", ""), 404, "NOT_FOUND")
		// There is no route that creates a task.
		if w := call(approverS, "POST", "/v1/sites/"+site+"/tasks", uuid.NewString(), "", `{}`); w.Code != 405 && w.Code != 404 {
			t.Fatalf("POST /tasks must not exist, got %d", w.Code)
		}
	})

	t.Run("two executors racing on one approval create one task", func(t *testing.T) {
		f := awaiting(true)
		var wg sync.WaitGroup
		errs := make(chan error, 4)
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, e := executor.ExecuteApproved(ctx, tenant, true, 50); e != nil {
					errs <- e
				}
			}()
		}
		wg.Wait()
		close(errs)
		for e := range errs {
			t.Fatal(e)
		}
		if n := count(`SELECT count(*) FROM supervisor.tasks WHERE case_id=$1`, f.caseID); n != 1 {
			t.Fatalf("concurrent executors created %d tasks", n)
		}
		if n := count(`SELECT count(*) FROM supervisor.approved_actions WHERE proposal_id=$1`, f.proposalID); n != 1 {
			t.Fatalf("concurrent executors created %d actions", n)
		}
		if s := caseState(f.caseID); s != "observing@6" {
			t.Fatalf("case %s", s)
		}
	})

	t.Run("approver who lost the supervisor role is refused at dispatch", func(t *testing.T) {
		f := awaiting(true)
		exec(`UPDATE supervisor.memberships SET role='viewer' WHERE tenant_id=$1 AND site_id=$2 AND subject=$3`, tenant, site, approver)
		out := run(true)
		exec(`UPDATE supervisor.memberships SET role='supervisor' WHERE tenant_id=$1 AND site_id=$2 AND subject=$3`, tenant, site, approver)
		refused(f, out, approvals.DispatchApproverIneligible)
	})

	t.Run("assignee withdrawn from the roster is refused at dispatch", func(t *testing.T) {
		f := awaiting(true)
		planted(tenant, `UPDATE supervisor.maintenance_roster_heads SET confirmed=false WHERE tenant_id='`+tenant+`' AND line_id='`+f.line+`'`)
		refused(f, run(true), "ASSIGNEE_NOT_CONFIRMED")
	})

	t.Run("assignee who is no longer an active member is refused at dispatch", func(t *testing.T) {
		f := awaiting(true)
		exec(`UPDATE supervisor.memberships SET active=false WHERE tenant_id=$1 AND site_id=$2 AND subject=$3`, tenant, site, operator)
		out := run(true)
		exec(`UPDATE supervisor.memberships SET active=true WHERE tenant_id=$1 AND site_id=$2 AND subject=$3`, tenant, site, operator)
		refused(f, out, "ASSIGNEE_NOT_CONFIRMED")
	})

	t.Run("a payload altered after approval is refused", func(t *testing.T) {
		f := awaiting(true)
		// Rewrite the stored payload (and its self-check digest) as if storage were tampered
		// with: the approvers signed a different digest.
		planted(tenant, `UPDATE supervisor.proposal_revisions SET canonical_payload=replace(canonical_payload,'Inspect the infeed','Restart the infeed'),
 canonical_payload_sha256=encode(sha256(convert_to(replace(canonical_payload,'Inspect the infeed','Restart the infeed'),'UTF8')),'hex') WHERE tenant_id='`+tenant+`' AND proposal_id='`+f.proposalID+`'`)
		out := run(true)
		got := outcome(out, f.caseID)
		if got.Outcome != cases.ExecutionRefused || got.Reason != "DISPATCH_"+approvals.DispatchDigestMismatch {
			t.Fatalf("want DIGEST_MISMATCH refusal, got %+v", got)
		}
		if n := count(`SELECT count(*) FROM supervisor.tasks WHERE case_id=$1`, f.caseID); n != 0 {
			t.Fatalf("altered payload created %d tasks", n)
		}
	})

	t.Run("a context replaced after approval is refused as a stale binding", func(t *testing.T) {
		f := awaiting(true)
		other := uuid.NewString()
		planted(tenant, `INSERT INTO supervisor.plant_contexts(tenant_id,site_id,id,order_id,asset_id,window_start,window_end,process_snapshot_id,process_snapshot_sha256,readiness,snapshot,digest,actor_issuer,actor_subject,evaluated_at)
 SELECT tenant_id,site_id,'`+other+`',order_id,asset_id,window_start,window_end,process_snapshot_id,process_snapshot_sha256,readiness,jsonb_set(snapshot,'{context_id}',to_jsonb('`+other+`'::text)),digest,actor_issuer,actor_subject,evaluated_at FROM supervisor.plant_contexts WHERE id=(SELECT context_snapshot_id FROM supervisor.cases WHERE id='`+f.caseID+`')`,
			`UPDATE supervisor.cases SET context_snapshot_id='`+other+`' WHERE id='`+f.caseID+`'`)
		refused(f, run(true), approvals.DispatchBindingStale)
	})

	// Rule 05 / review of #188 B1: the bound evidence is re-evaluated at dispatch. A source the
	// run cited is corrected after the approval: the approval is not dispatched on superseded
	// evidence.
	t.Run("an evidence source revised after approval is refused at dispatch", func(t *testing.T) {
		f := awaiting(true)
		head, e := sources.Read(ctx, evidence.Actor{Issuer: issuer, Subject: submitter, SessionHash: submitterS.hash}, evidence.Scope{TenantID: tenant, SiteID: site}, f.sourceID, 0)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = sources.CreateOrRevise(ctx, evidence.Actor{Issuer: issuer, Subject: submitter, SessionHash: submitterS.hash}, evidence.Scope{TenantID: tenant, SiteID: site},
			evidence.Input{SourceID: f.sourceID, Kind: "manual", Title: "Operator note", Text: "Sensor misread, no jam"}, uuid.NewString(), head.StateRevision); e != nil {
			t.Fatal(e)
		}
		refused(f, run(true), approvals.DispatchEvidenceRevised)
	})

	t.Run("an evidence source revoked between approval and dispatch is refused", func(t *testing.T) {
		f := awaiting(true)
		// The revocation races the executor: it lands after the decision without the
		// decision-time invalidation having marked the proposal stale.
		planted(tenant, `UPDATE supervisor.evidence_heads SET revoked=true,access_epoch=access_epoch+1 WHERE tenant_id='`+tenant+`' AND source_id='`+f.sourceID+`'`)
		refused(f, run(true), approvals.DispatchEvidenceRevoked)
	})

	// Review of #188 minor 1: a missing hook is a construction error, not an evidence finding.
	// The run fails and rolls back; the approval stays for a correctly built executor.
	t.Run("an executor that cannot re-evaluate bound evidence fails without consuming", func(t *testing.T) {
		f := awaiting(true)
		_, e := cases.NewService(db.DB).ExecuteApproved(ctx, tenant, true, 50)
		if !errors.Is(e, cases.ErrEvidenceUnverified) {
			t.Fatalf("want ErrEvidenceUnverified, got %v", e)
		}
		if s := approvalState(f.proposalID); s != "approved" {
			t.Fatalf("the approval must stay approved, got %s", s)
		}
		if s := caseState(f.caseID); s != "awaiting_approval@4" {
			t.Fatalf("the case must keep waiting, got %s", s)
		}
		if n := count(`SELECT count(*) FROM supervisor.approved_actions WHERE proposal_id=$1`, f.proposalID); n != 0 {
			t.Fatalf("a construction error wrote %d action rows", n)
		}
		// The approval was kept, so a correctly built executor then dispatches it.
		if got := outcome(run(true), f.caseID); got.Outcome != cases.ExecutionCommitted {
			t.Fatalf("a wired executor must commit the kept approval, got %+v", got)
		}
	})

	// Review of #188 minor 2: the digest is judged before the policy revision, so a tampered
	// payload claiming another policy is refused for what it is.
	t.Run("a tampered payload naming another policy is a digest mismatch", func(t *testing.T) {
		f := awaiting(true)
		planted(tenant, `UPDATE supervisor.proposal_revisions SET canonical_payload=regexp_replace(canonical_payload,'"policy_revision":[0-9]+','"policy_revision":99'),
 canonical_payload_sha256=encode(sha256(convert_to(regexp_replace(canonical_payload,'"policy_revision":[0-9]+','"policy_revision":99'),'UTF8')),'hex') WHERE tenant_id='`+tenant+`' AND proposal_id='`+f.proposalID+`'`)
		if !strings.Contains(scalar(tenant, `SELECT canonical_payload FROM supervisor.proposal_revisions WHERE proposal_id=$1`, f.proposalID), `"policy_revision":99`) {
			t.Fatal("setup: the payload was not altered")
		}
		refused(f, run(true), approvals.DispatchDigestMismatch)
	})

	// Review of #188 addendum M2: the transaction's first clock read predates its lock waits.
	// The approval expires while the executor waits for the site lock; it must be expired, not
	// consumed.
	t.Run("an approval that expires during the executor's lock wait is not consumed", func(t *testing.T) {
		f := awaiting(true)
		hold, e := control.BeginTx(ctx, nil)
		if e != nil {
			t.Fatal(e)
		}
		defer hold.Rollback()
		if _, e = hold.ExecContext(ctx, `SELECT 1 FROM supervisor.sites WHERE tenant_id=$1 AND id=$2 FOR NO KEY UPDATE`, tenant, site); e != nil {
			t.Fatal(e)
		}
		planted(tenant, `UPDATE supervisor.proposal_revisions SET expires_at=clock_timestamp()+interval '1500 milliseconds' WHERE tenant_id='`+tenant+`' AND proposal_id='`+f.proposalID+`'`)
		done := make(chan []cases.Execution, 1)
		failed := make(chan error, 1)
		go func() {
			out, e := executor.ExecuteApproved(ctx, tenant, true, 50)
			if e != nil {
				failed <- e
				return
			}
			done <- out
		}()
		for waiting := 0; waiting == 0; {
			if e = control.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=$1 AND wait_event_type='Lock'`, database).Scan(&waiting); e != nil {
				t.Fatal(e)
			}
		}
		time.Sleep(2500 * time.Millisecond) // past the deadline, within lock_timeout (5 s)
		if e = hold.Rollback(); e != nil {
			t.Fatal(e)
		}
		var out []cases.Execution
		select {
		case out = <-done:
		case e = <-failed:
			t.Fatal(e)
		}
		if got := outcome(out, f.caseID); got.Outcome != cases.ExecutionClosed || got.Reason != "PROPOSAL_EXPIRED" {
			t.Fatalf("want closed PROPOSAL_EXPIRED, got %+v", got)
		}
		if s := approvalState(f.proposalID); s != "expired" {
			t.Fatalf("approval must be expired, got %s", s)
		}
		if n := count(`SELECT count(*) FROM supervisor.tasks WHERE case_id=$1`, f.caseID); n != 0 {
			t.Fatalf("an approval that expired in the lock wait created %d tasks", n)
		}
	})

	// Review of #188 addendum M3: the tenant list is discovered at the start of a tick; a tenant
	// deactivated since then gets nothing consumed or created, and its approval is kept.
	t.Run("a tenant deactivated after discovery gets no task", func(t *testing.T) {
		f := awaiting(true)
		exec(`UPDATE supervisor.tenants SET deactivated_at=now() WHERE id=$1`, tenant)
		out, e := executor.ExecuteApproved(ctx, tenant, true, 50)
		exec(`UPDATE supervisor.tenants SET deactivated_at=NULL WHERE id=$1`, tenant)
		if e != nil {
			t.Fatal(e)
		}
		if got := outcome(out, f.caseID); got.Outcome == cases.ExecutionCommitted {
			t.Fatalf("a deactivated tenant's approval was executed: %+v", got)
		}
		if s := approvalState(f.proposalID); s != "approved" {
			t.Fatalf("the approval must be kept, got %s", s)
		}
		if n := count(`SELECT count(*) FROM supervisor.tasks WHERE case_id=$1`, f.caseID) + count(`SELECT count(*) FROM supervisor.approved_actions WHERE proposal_id=$1`, f.proposalID); n != 0 {
			t.Fatalf("a deactivated tenant got %d tasks or actions", n)
		}
		if got := outcome(run(true), f.caseID); got.Outcome != cases.ExecutionCommitted {
			t.Fatalf("reactivated, the kept approval must execute, got %+v", got)
		}
	})

	// Review of #188 M1: current policy is re-evaluated at dispatch.
	t.Run("an approval granted under another policy revision is refused", func(t *testing.T) {
		policy = approvals.PolicyRevision + 1
		f := awaiting(true)
		policy = approvals.PolicyRevision
		refused(f, run(true), approvals.DispatchPolicyChanged)
	})

	// Review of #188 M2: expiry with the executor live, not only with execution off.
	t.Run("an approval that expires before execution creates nothing with execution enabled", func(t *testing.T) {
		f := awaiting(true)
		planted(tenant, `UPDATE supervisor.proposal_revisions SET issued_at=issued_at-interval '2 hours',expires_at=clock_timestamp()-interval '1 second' WHERE tenant_id='`+tenant+`' AND proposal_id='`+f.proposalID+`'`)
		got := outcome(run(true), f.caseID)
		if got.Outcome != cases.ExecutionClosed || got.Reason != "PROPOSAL_EXPIRED" {
			t.Fatalf("want closed PROPOSAL_EXPIRED, got %+v", got)
		}
		if s := approvalState(f.proposalID); s != "expired" {
			t.Fatalf("approval must be expired, got %s", s)
		}
		if n := count(`SELECT count(*) FROM supervisor.tasks WHERE case_id=$1`, f.caseID) + count(`SELECT count(*) FROM supervisor.approved_actions WHERE proposal_id=$1`, f.proposalID); n != 0 {
			t.Fatalf("an expired approval left %d tasks or actions", n)
		}
	})

	// Review of #188 minor 4: the stored payload stays hash-consistent and the approvers' signed
	// digest is moved with it, so only the re-encode check (stored bytes must be the canonical
	// encoding of what they decode to) can catch it. Changing the payload alone is not even
	// storable: a CHECK ties canonical_payload_sha256 to the payload bytes.
	t.Run("a non-canonical payload whose digests all agree is refused", func(t *testing.T) {
		f := awaiting(true)
		planted(tenant, `UPDATE supervisor.proposal_revisions SET canonical_payload=' '||canonical_payload,
 canonical_payload_sha256=encode(sha256(convert_to(' '||canonical_payload,'UTF8')),'hex') WHERE tenant_id='`+tenant+`' AND proposal_id='`+f.proposalID+`'`,
			`UPDATE supervisor.approval_decisions d SET canonical_payload_sha256=r.canonical_payload_sha256 FROM supervisor.proposal_revisions r
 WHERE (r.tenant_id,r.site_id,r.proposal_id,r.revision)=(d.tenant_id,d.site_id,d.proposal_id,d.proposal_revision) AND d.tenant_id='`+tenant+`' AND d.proposal_id='`+f.proposalID+`'`)
		refused(f, run(true), approvals.DispatchDigestMismatch)
	})

	// Review of #188 minor 5: the approver must differ from the submitter at dispatch too.
	t.Run("an approval by the submitter is refused at dispatch", func(t *testing.T) {
		f := awaiting(true)
		planted(tenant, `UPDATE supervisor.approval_decisions SET reviewer_subject='`+submitter+`' WHERE tenant_id='`+tenant+`' AND proposal_id='`+f.proposalID+`'`)
		refused(f, run(true), approvals.DispatchApproverIneligible)
	})

	// Review of #188 minor 6: a task can only belong to a committed action of its own case.
	t.Run("the database refuses a task for a rejected action", func(t *testing.T) {
		f := awaiting(true)
		planted(tenant, `UPDATE supervisor.maintenance_roster_heads SET confirmed=false WHERE tenant_id='`+tenant+`' AND line_id='`+f.line+`'`)
		refused(f, run(true), "ASSIGNEE_NOT_CONFIRMED")
		action := scalar(tenant, `SELECT id::text FROM supervisor.approved_actions WHERE proposal_id=$1`, f.proposalID)
		_, e := control.ExecContext(ctx, `INSERT INTO supervisor.tasks(tenant_id,site_id,id,action_id,case_id,asset_id,line_id,order_id,assignee_actor_id,assignee_issuer,assignee_subject,template,description,due_at,state,revision)
 VALUES($1,$2,$3,$4,$5,$6,$6,$7,$8,$9,$10,'inspect_report_only','x',now()+interval '1 hour','assigned',1)`, tenant, site, uuid.NewString(), action, f.caseID, f.line, f.order, assigneeID, issuer, operator)
		if e == nil || !strings.Contains(e.Error(), "committed action") {
			t.Fatalf("a task for a rejected action was accepted: %v", e)
		}
		// An action naming another case than its proposal's is unrepresentable (review addendum).
		other := awaiting(true)
		_, e = control.ExecContext(ctx, `INSERT INTO supervisor.approved_actions(tenant_id,site_id,id,proposal_id,proposal_revision,action_kind,canonical_payload_sha256,case_id,state,reason,actor_issuer,actor_subject)
 SELECT r.tenant_id,r.site_id,$3,r.proposal_id,r.revision,'maintenance_investigation.create',r.canonical_payload_sha256,$4,'rejected','FIXTURE','svc','fixture'
 FROM supervisor.proposal_revisions r WHERE r.tenant_id=$1 AND r.site_id=$2 AND r.proposal_id=$5`, tenant, site, uuid.NewString(), f.caseID, other.proposalID)
		if e == nil || !strings.Contains(e.Error(), "approved_actions_proposal_case_fkey") {
			t.Fatalf("an action bound to another case than its proposal's was accepted: %v", e)
		}
	})

	t.Run("a task outside the case line is refused (lineage fails closed)", func(t *testing.T) {
		// The approval contract binds asset_id but not its lineage. Until approved part_of_asset
		// lineage is checked, the executor accepts only the case line itself.
		machine := uuid.NewString()
		planted(tenant, `INSERT INTO supervisor.knowledge_entity_heads(tenant_id,site_id,id,current_revision) VALUES('`+tenant+`','`+site+`','`+machine+`',1)`)
		refused(awaitingOn(true, machine), run(true), "LINEAGE_INVALID")
	})

	t.Run("rejected, expired and revoked decisions return the case to needs_input", func(t *testing.T) {
		rejected := awaiting(false)
		expired := awaiting(true)
		planted(tenant, `UPDATE supervisor.proposal_revisions SET issued_at=issued_at-interval '2 hours',expires_at=clock_timestamp()-interval '1 second' WHERE tenant_id='`+tenant+`' AND proposal_id='`+expired.proposalID+`'`)
		revoked := awaiting(true)
		expect(call(approverS, "POST", "/v1/sites/"+site+"/proposals/"+revoked.proposalID+"/revoke", uuid.NewString(), `"2"`, `{"expected_revision":2,"reason":"changed my mind"}`), 200, "")
		// Even with execution off, a closed decision is carried to the case.
		out := run(false)
		for _, c := range []struct {
			f      fixture
			reason string
		}{{rejected, "PROPOSAL_REJECTED"}, {expired, "PROPOSAL_EXPIRED"}, {revoked, "PROPOSAL_REVOKED"}} {
			got := outcome(out, c.f.caseID)
			if got.Outcome != cases.ExecutionClosed || got.Reason != c.reason {
				t.Fatalf("want closed %s, got %+v", c.reason, got)
			}
			if s := caseState(c.f.caseID); s != "needs_input@5" {
				t.Fatalf("%s: case %s", c.reason, s)
			}
			if n := count(`SELECT count(*) FROM supervisor.tasks WHERE case_id=$1`, c.f.caseID); n != 0 {
				t.Fatalf("%s created a task", c.reason)
			}
		}
	})

	t.Run("the runtime role cannot write tasks around the executor", func(t *testing.T) {
		f := awaiting(true)
		got := outcome(run(true), f.caseID)
		tx, e := db.DB.BeginTx(ctx, nil)
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback()
		if _, e = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, tenant); e != nil {
			t.Fatal(e)
		}
		if _, e = tx.ExecContext(ctx, `UPDATE supervisor.tasks SET assignee_subject='someone-else' WHERE id=$1`, got.TaskID); e == nil || !strings.Contains(e.Error(), "permission denied") {
			t.Fatalf("runtime role must not update a task directly: %v", e)
		}
	})

	t.Run("task lifecycle: assignee works it, supervisor governs it, completion cites the assignee's report", func(t *testing.T) {
		ev := evidence.New(db.DB)
		es := evidence.Scope{TenantID: tenant, SiteID: site}
		// A report the assignee wrote before this task existed (review of #190, M2).
		stale, e := ev.CreateOrRevise(ctx, evidence.Actor{Issuer: issuer, Subject: operatorS.subject, SessionHash: operatorS.hash}, es,
			evidence.Input{SourceID: uuid.NewString(), Kind: "manual", Title: "Generic check", Text: "Checked, OK."}, uuid.NewString(), 0)
		if e != nil {
			t.Fatal(e)
		}
		f := awaiting(true)
		task := outcome(run(true), f.caseID).TaskID
		if task == "" {
			t.Fatal("no task to work")
		}
		path := "/v1/sites/" + site + "/tasks/" + task
		do := func(actor session, action, key string, revision int64, body string) *httptest.ResponseRecorder {
			t.Helper()
			return call(actor, "POST", path+"/"+action, key, `"`+strconv.FormatInt(revision, 10)+`"`, body)
		}
		body := func(revision int64, reason string) string {
			if reason == "" {
				return `{"expected_revision":` + strconv.FormatInt(revision, 10) + `}`
			}
			return `{"expected_revision":` + strconv.FormatInt(revision, 10) + `,"reason":"` + reason + `"}`
		}
		state := func(w *httptest.ResponseRecorder) tasks.TaskV1 {
			t.Helper()
			var v tasks.TaskV1
			if e := json.Unmarshal(w.Body.Bytes(), &v); e != nil {
				t.Fatalf("%v: %s", e, w.Body.String())
			}
			return v
		}
		ok := func(w *httptest.ResponseRecorder, want tasks.State, revision int64) tasks.TaskV1 {
			t.Helper()
			expect(w, 200, "")
			v := state(w)
			if v.State != want || v.Revision != revision {
				t.Fatalf("task %s@%d, want %s@%d", v.State, v.Revision, want, revision)
			}
			return v
		}

		// Only the assignee acknowledges: a supervisor or the owner is refused.
		expect(do(approverS, "acknowledge", uuid.NewString(), 1, body(1, "")), 403, "TASK_ROLE_REQUIRED")
		expect(do(ownerS, "acknowledge", uuid.NewString(), 1, body(1, "")), 403, "TASK_ROLE_REQUIRED")
		expect(do(viewerS, "acknowledge", uuid.NewString(), 1, body(1, "")), 403, "TASK_ROLE_REQUIRED")
		expect(do(operatorS, "acknowledge", uuid.NewString(), 1, `{"expected_revision":1,"assignee_subject":"someone"}`), 422, "INVALID_TASK_REQUEST")
		expect(do(operatorS, "acknowledge", uuid.NewString(), 1, body(2, "")), 422, "INVALID_TASK_REQUEST") // If-Match and body disagree
		expect(do(operatorS, "start", uuid.NewString(), 1, body(1, "")), 409, "ILLEGAL_TRANSITION")
		ack := uuid.NewString()
		acked := ok(do(operatorS, "acknowledge", ack, 1, body(1, "")), tasks.Acknowledged, 2)
		if acked.AcknowledgedAt == nil || acked.Events[1].Event != "acknowledge" || acked.Events[1].Actor.Subject != operator {
			t.Fatalf("acknowledge event %+v", acked)
		}
		if v := ok(do(operatorS, "acknowledge", ack, 1, body(1, "")), tasks.Acknowledged, 2); !v.Replayed {
			t.Fatal("same key and body must replay")
		}
		expect(do(operatorS, "acknowledge", ack, 1, body(1, "again")), 409, "IDEMPOTENCY_CONFLICT")
		expect(do(operatorS, "start", uuid.NewString(), 1, body(1, "")), 412, "REVISION_CONFLICT")
		ok(do(operatorS, "start", uuid.NewString(), 2, body(2, "")), tasks.InProgress, 3)

		// A supervisor blocks with a reason and a recent sign-in; the assignee resumes with a reason.
		expect(do(approverS, "block", uuid.NewString(), 3, body(3, "")), 422, "REASON_REQUIRED")
		expect(do(staleS, "block", uuid.NewString(), 3, body(3, "Spare part missing")), 428, "REAUTHENTICATION_REQUIRED")
		expect(do(operatorS, "block", uuid.NewString(), 3, body(3, "Spare part missing")), 403, "TASK_ROLE_REQUIRED")
		ok(do(approverS, "block", uuid.NewString(), 3, body(3, "Spare part missing")), tasks.Blocked, 4)
		expect(do(operatorS, "start", uuid.NewString(), 4, body(4, "")), 422, "REASON_REQUIRED")
		ok(do(operatorS, "start", uuid.NewString(), 4, body(4, "Part arrived")), tasks.InProgress, 5)

		// A case with a live task cannot be canceled around it.
		caseRev := scalar(tenant, `SELECT revision FROM supervisor.cases WHERE id=$1`, f.caseID)
		expect(call(approverS, "POST", "/v1/sites/"+site+"/cases/"+f.caseID+"/cancel", uuid.NewString(), `"`+caseRev+`"`, `{"expected_revision":`+caseRev+`,"reason":"Line shut down"}`), 409, "TRANSITION_GUARD_FAILED")

		// Completion cites evidence the assignee wrote, at least one a written report.
		write := func(s session, kind, text string) string {
			t.Helper()
			src, e := ev.CreateOrRevise(ctx, evidence.Actor{Issuer: issuer, Subject: s.subject, SessionHash: s.hash}, es,
				evidence.Input{SourceID: uuid.NewString(), Kind: kind, Title: "Inspection report", Text: text}, uuid.NewString(), 0)
			if e != nil {
				t.Fatal(e)
			}
			return src.SourceID
		}
		report := write(operatorS, "manual", "Infeed belt tension low; reported to engineering, nothing adjusted.")
		interview := write(operatorS, "interview", "Asked the shift lead about the stop.")
		theirs := write(approverS, "manual", "Supervisor notes.")
		revoked := write(operatorS, "manual", "Draft report, withdrawn.")
		if _, e := ev.Revoke(ctx, evidence.Actor{Issuer: issuer, Subject: owner, SessionHash: ownerS.hash}, es, revoked, "withdrawn", uuid.NewString(), 1); e != nil {
			t.Fatal(e)
		}
		complete := func(revision int64, ids ...string) string {
			b, _ := json.Marshal(map[string]any{"expected_revision": revision, "completion_note": "Inspected the infeed; see report.", "evidence_ids": ids})
			return string(b)
		}
		for name, ids := range map[string][]string{
			"another person's report": {report, theirs},
			"no written report":       {interview},
			"revoked evidence":        {report, revoked},
			"unknown evidence":        {report, uuid.NewString()},
			"written before the task": {stale.SourceID},
		} {
			if w := do(operatorS, "complete", uuid.NewString(), 5, complete(5, ids...)); w.Code != 422 || !strings.Contains(w.Body.String(), "COMPLETION_EVIDENCE_UNUSABLE") {
				t.Fatalf("%s: HTTP %d %s", name, w.Code, w.Body.String())
			}
		}
		expect(do(operatorS, "complete", uuid.NewString(), 5, `{"expected_revision":5,"completion_note":"x","evidence_ids":[]}`), 422, "INVALID_TASK_REQUEST")
		expect(do(approverS, "complete", uuid.NewString(), 5, complete(5, report)), 403, "TASK_ROLE_REQUIRED")
		done := ok(do(operatorS, "complete", uuid.NewString(), 5, complete(5, report, interview)), tasks.Completed, 6)
		if done.CompletedAt == nil || len(done.Completions) != 1 || len(done.CompletionEvidenceIDs) != 2 || done.Completions[0].Note != "Inspected the infeed; see report." {
			t.Fatalf("completion %+v", done)
		}
		expect(do(operatorS, "complete", uuid.NewString(), 6, complete(6, report)), 409, "ILLEGAL_TRANSITION")

		// A supervisor reopens as a correction: the first completion stays in the history. The
		// owner is not an implicit supervisor for block, cancel or reopen (review of #190, minor 1).
		expect(do(ownerS, "reopen", uuid.NewString(), 6, body(6, "Owner reopen")), 403, "TASK_ROLE_REQUIRED")
		expect(do(approverS, "reopen", uuid.NewString(), 6, body(6, "")), 422, "REASON_REQUIRED")
		reopened := ok(do(approverS, "reopen", uuid.NewString(), 6, body(6, "Report missing the belt reading")), tasks.InProgress, 7)
		if reopened.CompletedAt != nil || len(reopened.CompletionEvidenceIDs) != 0 || len(reopened.Completions) != 1 {
			t.Fatalf("reopened task %+v", reopened)
		}
		expect(do(ownerS, "block", uuid.NewString(), 7, body(7, "Owner block")), 403, "TASK_ROLE_REQUIRED")

		// The report the supervisor sent back cannot close the correction loop again; a new report
		// written after the reopen can, as a second epoch (review of #190, M2 and minor 5).
		if w := do(operatorS, "complete", uuid.NewString(), 7, complete(7, report)); w.Code != 422 || !strings.Contains(w.Body.String(), "COMPLETION_EVIDENCE_UNUSABLE") {
			t.Fatalf("re-citing the reopened report: HTTP %d %s", w.Code, w.Body.String())
		}
		corrected := write(operatorS, "manual", "Belt reading 42 N; tension low, reported to engineering.")
		again := ok(do(operatorS, "complete", uuid.NewString(), 7, complete(7, corrected)), tasks.Completed, 8)
		if len(again.Completions) != 2 || again.Completions[1].Epoch != 2 || len(again.CompletionEvidenceIDs) != 1 || again.CompletionEvidenceIDs[0] != corrected {
			t.Fatalf("second completion %+v", again)
		}
		ok(do(approverS, "reopen", uuid.NewString(), 8, body(8, "Engineering asked for a photo")), tasks.InProgress, 9)

		// Cancel by a supervisor; then the case can be canceled too.
		expect(do(ownerS, "cancel", uuid.NewString(), 9, body(9, "Owner cancel")), 403, "TASK_ROLE_REQUIRED")
		ok(do(approverS, "cancel", uuid.NewString(), 9, body(9, "Line shut down for the week")), tasks.Canceled, 10)
		expect(do(operatorS, "start", uuid.NewString(), 10, body(10, "resume")), 409, "ILLEGAL_TRANSITION")
		expect(call(approverS, "POST", "/v1/sites/"+site+"/cases/"+f.caseID+"/cancel", uuid.NewString(), `"`+caseRev+`"`, `{"expected_revision":`+caseRev+`,"reason":"Line shut down"}`), 200, "")

		// Another tenant cannot act on the task.
		expect(do(outsiderS, "acknowledge", uuid.NewString(), 10, body(10, "")), 404, "NOT_FOUND")
	})

	// working executes a fresh approval and has the assignee acknowledge and start the task.
	post := func(actor session, task, action string, revision int64, extra string) *httptest.ResponseRecorder {
		r := strconv.FormatInt(revision, 10)
		return call(actor, "POST", "/v1/sites/"+site+"/tasks/"+task+"/"+action, uuid.NewString(), `"`+r+`"`, `{"expected_revision":`+r+extra+`}`)
	}
	working := func(t *testing.T) (fixture, string) {
		t.Helper()
		f := awaiting(true)
		task := outcome(run(true), f.caseID).TaskID
		if task == "" {
			t.Fatal("no task to work")
		}
		expect(post(operatorS, task, "acknowledge", 1, ""), 200, "")
		expect(post(operatorS, task, "start", 2, ""), 200, "")
		return f, task
	}
	report := func(t *testing.T, text string) string {
		t.Helper()
		src, e := evidence.New(db.DB).CreateOrRevise(ctx, evidence.Actor{Issuer: issuer, Subject: operatorS.subject, SessionHash: operatorS.hash}, evidence.Scope{TenantID: tenant, SiteID: site},
			evidence.Input{SourceID: uuid.NewString(), Kind: "manual", Title: "Inspection report", Text: text}, uuid.NewString(), 0)
		if e != nil {
			t.Fatal(e)
		}
		return `,"completion_note":"Inspected; see report.","evidence_ids":["` + src.SourceID + `"]`
	}

	// Review of #190 M1 and minor 2: role and membership revocation apply on the next request.
	t.Run("revoked roles and memberships stop task actions at once", func(t *testing.T) {
		_, task := working(t)
		setMember := func(subject, set string) {
			exec(`UPDATE supervisor.memberships SET `+set+` WHERE tenant_id=$1 AND site_id=$2 AND subject=$3`, tenant, site, subject)
		}
		// Restored even if an assertion fails, so later scenarios keep an eligible assignee.
		defer setMember(operator, "role='operator',active=true")
		defer setMember(approver, "role='supervisor'")
		completion := report(t, "Belt tension low, reported.") // written while still an operator
		setMember(operator, "role='viewer'")
		expect(post(operatorS, task, "complete", 3, completion), 403, "TASK_ROLE_REQUIRED")
		setMember(operator, "role='operator'")
		setMember(operator, "active=false")
		expect(post(operatorS, task, "complete", 3, completion), 404, "NOT_FOUND")
		setMember(operator, "active=true")
		setMember(approver, "role='operator'")
		expect(post(approverS, task, "block", 3, `,"reason":"Spare part missing"`), 403, "TASK_ROLE_REQUIRED")
		setMember(approver, "role='supervisor'")
		// A supervisor of another site of the same tenant, using this task's ID under their site.
		otherSiteOfTenant, elsewhere := uuid.NewString(), uuid.NewString()
		exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'second site')`, tenant, otherSiteOfTenant)
		exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'supervisor')`, tenant, otherSiteOfTenant, issuer, elsewhere)
		elsewhereS := seed(elsewhere)
		expect(call(elsewhereS, "POST", "/v1/sites/"+otherSiteOfTenant+"/tasks/"+task+"/block", uuid.NewString(), `"3"`, `{"expected_revision":3,"reason":"Not my site"}`), 404, "NOT_FOUND")
		// Control: the restored assignee still completes.
		expect(post(operatorS, task, "complete", 3, completion), 200, "")
	})

	// WIS-694 (security audit run 1): a replayed task mutation checks read authority again (rule
	// 07). A supervisor since demoted to operator gets the 404 that GET gives for another person's
	// task, not the stored body with events added after the demotion; the assignee's own replay
	// and a supervisor's replay still return the stored result.
	t.Run("a replay after demotion to operator is the same 404 as GET", func(t *testing.T) {
		_, task := working(t)
		setRole := func(subject, role string) {
			exec(`UPDATE supervisor.memberships SET role=$4 WHERE tenant_id=$1 AND site_id=$2 AND subject=$3`, tenant, site, subject, role)
		}
		defer setRole(approver, "supervisor") // restored even if an assertion fails
		blockKey, startKey := uuid.NewString(), uuid.NewString()
		block := func() *httptest.ResponseRecorder {
			return call(approverS, "POST", "/v1/sites/"+site+"/tasks/"+task+"/block", blockKey, `"3"`, `{"expected_revision":3,"reason":"Spare part missing"}`)
		}
		start := func() *httptest.ResponseRecorder {
			return call(operatorS, "POST", "/v1/sites/"+site+"/tasks/"+task+"/start", startKey, `"4"`, `{"expected_revision":4,"reason":"WIS-694 later event"}`)
		}
		expect(block(), 200, "")
		expect(start(), 200, "") // a later event the demoted supervisor must not read

		setRole(approver, "operator")
		expect(call(approverS, "GET", "/v1/sites/"+site+"/tasks/"+task, "", "", ""), 404, "NOT_FOUND")
		if w := block(); w.Code != 404 || strings.Contains(w.Body.String(), "WIS-694 later event") {
			t.Fatalf("demoted supervisor's replay: HTTP %d %s, want 404 NOT_FOUND without the task", w.Code, w.Body.String())
		}
		// Controls: the assignee replays their own request, and a supervisor's replay still works.
		if w := start(); w.Code != 200 || !strings.Contains(w.Body.String(), `"replayed":true`) {
			t.Fatalf("assignee's replay: HTTP %d %s", w.Code, w.Body.String())
		}
		setRole(approver, "supervisor")
		if w := block(); w.Code != 200 || !strings.Contains(w.Body.String(), `"replayed":true`) {
			t.Fatalf("supervisor's replay: HTTP %d %s", w.Code, w.Body.String())
		}
	})

	// Review of #190 minor 4: a task under a closed case does not move.
	t.Run("a task under a resolved case refuses events", func(t *testing.T) {
		f, task := working(t)
		planted(tenant, `UPDATE supervisor.cases SET state='resolved',resolution_reason='Closed after observation' WHERE tenant_id='`+tenant+`' AND id='`+f.caseID+`'`)
		expect(post(approverS, task, "block", 3, `,"reason":"After close"`), 409, "ILLEGAL_TRANSITION")
	})

	// Review of #190 M3: the database ties every head change to its history.
	t.Run("the database refuses a task change without its event, and rewritten timestamps", func(t *testing.T) {
		f := awaiting(true)
		task := outcome(run(true), f.caseID).TaskID
		attempt := func(statements ...string) error {
			tx, e := db.DB.BeginTx(ctx, nil)
			if e != nil {
				t.Fatal(e)
			}
			defer tx.Rollback()
			for _, q := range append([]string{`SELECT set_config('supervisor.tenant_id','` + tenant + `',true)`}, statements...) {
				if _, e = tx.ExecContext(ctx, q); e != nil {
					return e
				}
			}
			return tx.Commit()
		}
		if e := attempt(`UPDATE supervisor.tasks SET state='acknowledged',revision=2,acknowledged_at=now() WHERE id='` + task + `'`); e == nil || !strings.Contains(e.Error(), "needs the event") {
			t.Fatalf("a head change without its event was committed: %v", e)
		}
		expect(post(operatorS, task, "acknowledge", 1, ""), 200, "")
		event := func(rev int, from, to, ev string) string {
			return `INSERT INTO supervisor.task_events(tenant_id,site_id,task_id,revision,from_state,to_state,event,actor_issuer,actor_subject) VALUES('` + tenant + `','` + site + `','` + task + `',` + strconv.Itoa(rev) + `,'` + from + `','` + to + `','` + ev + `','` + issuer + `','` + operator + `')`
		}
		// Head first, then its event (the event guard requires the head it describes), exactly as
		// the service writes them; only the rewritten acknowledged_at is wrong.
		if e := attempt(`UPDATE supervisor.tasks SET state='in_progress',revision=3,started_at=now(),acknowledged_at=now()+interval '1 hour' WHERE id='`+task+`'`,
			event(3, "acknowledged", "in_progress", "start")); e == nil || !strings.Contains(e.Error(), "written once") {
			t.Fatalf("a rewritten acknowledged_at was committed: %v", e)
		}
		if e := attempt(event(3, "acknowledged", "completed", "complete")); e == nil {
			t.Fatal("an event that does not match its head was committed")
		}
		// Review of #190 N1/N2: each remaining clause, by its exact message, from in_progress.
		expect(post(operatorS, task, "start", 2, ""), 200, "")
		reasoned := func(rev int, from, to, ev string) string {
			return `INSERT INTO supervisor.task_events(tenant_id,site_id,task_id,revision,from_state,to_state,event,reason,actor_issuer,actor_subject) VALUES('` + tenant + `','` + site + `','` + task + `',` + strconv.Itoa(rev) + `,'` + from + `','` + to + `','` + ev + `','Part missing','` + issuer + `','` + operator + `')`
		}
		for _, c := range []struct {
			name, want string
			statements []string
		}{
			{"completed with no completion epoch", "needs its completion with evidence",
				[]string{`UPDATE supervisor.tasks SET state='completed',revision=4,completed_at=now() WHERE id='` + task + `'`, event(4, "in_progress", "completed", "complete")}},
			{"completed with no completed_at", "tasks_completed_at",
				[]string{`UPDATE supervisor.tasks SET state='completed',revision=4 WHERE id='` + task + `'`}},
			{"an event naming another from_state", "needs the event",
				[]string{`UPDATE supervisor.tasks SET state='blocked',revision=4 WHERE id='` + task + `'`, reasoned(4, "acknowledged", "blocked", "block")}},
			{"an event name that is not its transition", "task_events_transition",
				[]string{`UPDATE supervisor.tasks SET state='blocked',revision=4 WHERE id='` + task + `'`, reasoned(4, "in_progress", "blocked", "start")}},
			{"a block without a reason", "task_events_reason",
				[]string{`UPDATE supervisor.tasks SET state='blocked',revision=4 WHERE id='` + task + `'`, event(4, "in_progress", "blocked", "block")}},
		} {
			if e := attempt(c.statements...); e == nil || !strings.Contains(e.Error(), c.want) {
				t.Fatalf("%s: want %q, got %v", c.name, c.want, e)
			}
		}
		// Control: the legal block with its reason commits.
		if e := attempt(`UPDATE supervisor.tasks SET state='blocked',revision=4 WHERE id='`+task+`'`, reasoned(4, "in_progress", "blocked", "block")); e != nil {
			t.Fatalf("the legal block was refused: %v", e)
		}
	})

	// Review of #190 N3: two copies of one request (same key and body) racing: the second waits
	// for the task lock, then replays the first's receipt instead of failing on the moved revision.
	t.Run("the same request racing with one key replays", func(t *testing.T) {
		_, task := working(t)
		key := uuid.NewString()
		bodies := make(chan string, 2)
		var wg sync.WaitGroup
		for range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				w := call(approverS, "POST", "/v1/sites/"+site+"/tasks/"+task+"/block", key, `"3"`, `{"expected_revision":3,"reason":"Part missing"}`)
				bodies <- strconv.Itoa(w.Code) + " " + w.Body.String()
			}()
		}
		wg.Wait()
		close(bodies)
		replays := 0
		for b := range bodies {
			if !strings.HasPrefix(b, "200 ") {
				t.Fatalf("a copy of the request failed: %s", b)
			}
			if strings.Contains(b, `"replayed":true`) {
				replays++
			}
		}
		if replays != 1 || count(`SELECT count(*) FROM supervisor.task_events WHERE task_id=$1 AND revision=4`, task) != 1 {
			t.Fatalf("want one replay and one event at revision 4, got %d replays", replays)
		}
	})

	// Review of #190 M4: two decisions on one revision; exactly one wins, the other is 412.
	race := func(t *testing.T, task string, revision int64, a, b func() *httptest.ResponseRecorder) {
		t.Helper()
		codes := make(chan int, 2)
		var wg sync.WaitGroup
		for _, f := range []func() *httptest.ResponseRecorder{a, b} {
			wg.Add(1)
			go func() { defer wg.Done(); codes <- f().Code }()
		}
		wg.Wait()
		close(codes)
		got := map[int]int{}
		for c := range codes {
			got[c]++
		}
		if got[200] != 1 || got[412] != 1 {
			t.Fatalf("race outcome %v, want one 200 and one 412", got)
		}
		if n := count(`SELECT count(*) FROM supervisor.task_events WHERE task_id=$1 AND revision=$2`, task, revision+1); n != 1 {
			t.Fatalf("%d events at revision %d", n, revision+1)
		}
		if n := count(`SELECT count(*) FROM supervisor.task_completions WHERE task_id=$1`, task); n > 1 {
			t.Fatalf("%d completion epochs", n)
		}
	}
	t.Run("completion and cancel racing on one revision", func(t *testing.T) {
		_, task := working(t)
		evidence := report(t, "Raced completion.")
		race(t, task, 3, func() *httptest.ResponseRecorder { return post(operatorS, task, "complete", 3, evidence) },
			func() *httptest.ResponseRecorder { return post(approverS, task, "cancel", 3, `,"reason":"Line down"`) })
	})
	t.Run("completion and block racing on one revision", func(t *testing.T) {
		_, task := working(t)
		evidence := report(t, "Raced completion.")
		race(t, task, 3, func() *httptest.ResponseRecorder { return post(operatorS, task, "complete", 3, evidence) },
			func() *httptest.ResponseRecorder {
				return post(approverS, task, "block", 3, `,"reason":"Part missing"`)
			})
	})
	t.Run("two reopens of one completion racing", func(t *testing.T) {
		_, task := working(t)
		expect(post(operatorS, task, "complete", 3, report(t, "Done.")), 200, "")
		reopen := func() *httptest.ResponseRecorder {
			return post(approverS, task, "reopen", 4, `,"reason":"Add the reading"`)
		}
		race(t, task, 4, reopen, reopen)
	})

	t.Run("the task guard refuses a skipped step even from the runtime role", func(t *testing.T) {
		f := awaiting(true)
		got := outcome(run(true), f.caseID)
		tx, e := db.DB.BeginTx(ctx, nil)
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback()
		if _, e = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, tenant); e != nil {
			t.Fatal(e)
		}
		if _, e = tx.ExecContext(ctx, `UPDATE supervisor.tasks SET state='completed',revision=2 WHERE id=$1`, got.TaskID); e == nil || !strings.Contains(e.Error(), "advances one allowed step") {
			t.Fatalf("assigned -> completed must be refused by the guard: %v", e)
		}
	})

	// WIS-718 acceptance (threshold frozen before the change: 30 s, far under the 30 min approval
	// TTL): case evaluation ran inside the worker Tick before ExecuteApproved, so a slow evaluation
	// of tenant A delayed tenant B's approved action. Holding A's site row lock makes every
	// evaluation of A's contexts wait out its 5 s lock_timeout; eight of them cost 40 s, more than
	// the bound, when evaluation is inside Tick. With evaluation on its own loop, one Tick turns
	// B's approval into exactly one task within the bound and A's contexts stay unevaluated.
	// Run last: Tick serves every active tenant of this database.
	t.Run("a slow evaluation of another tenant does not delay an approved task", func(t *testing.T) {
		const bound = 30 * time.Second
		f := awaiting(true)
		pending := make([]string, 8)
		rows := make([]string, len(pending))
		for i := range pending {
			pending[i] = uuid.NewString()
			rows[i] = `('` + otherTenant + `','` + otherSite + `','` + pending[i] + `','context.created')`
		}
		planted(otherTenant, `INSERT INTO supervisor.plant_context_outbox(tenant_id,site_id,context_id,event_type) VALUES`+strings.Join(rows, ","))
		// FOR UPDATE conflicts with both the evaluator's read (KEY SHARE) and write site locks.
		lock, e := control.BeginTx(ctx, nil)
		if e != nil {
			t.Fatal(e)
		}
		defer lock.Rollback()
		if _, e = lock.ExecContext(ctx, `SELECT 1 FROM supervisor.sites WHERE tenant_id=$1 AND id=$2 FOR UPDATE`, otherTenant, otherSite); e != nil {
			t.Fatal(e)
		}
		cfg := config.Config{TemporalQueue: "wis-718-" + uuid.NewString(), TaskExecution: true}
		// The evaluator loop runs beside the Tick, as in the worker, and is stuck on A's lock.
		loop, stopLoop := context.WithCancel(ctx)
		looped := make(chan struct{})
		go func() {
			defer close(looped)
			ev := dispatch.NewEvaluator(db)
			for loop.Err() == nil {
				_, _ = ev.Pass(loop, cfg.Tenants)
			}
		}()
		defer func() { stopLoop(); <-looped }()
		temporal := &recordingTemporal{}
		start := time.Now()
		// The Tick's error is not asserted: earlier subtests leave completed tasks on planted
		// contexts whose outcome observation fails in this shared database. B's rows are asserted.
		_, _ = dispatch.Tick(ctx, db, temporal, cfg, false)
		if took := time.Since(start); took > bound {
			t.Fatalf("Tick took %v with tenant A's evaluation stalled, bound %v", took, bound)
		}
		if n := count(`SELECT count(*) FROM supervisor.tasks WHERE case_id=$1`, f.caseID); n != 1 {
			t.Fatalf("tenant B's approval made %d tasks in one Tick, want 1", n)
		}
		if s := approvalState(f.proposalID); s != "consumed" {
			t.Fatalf("tenant B's approval is %s, want consumed", s)
		}
		if n := count(`SELECT count(*) FROM supervisor.case_context_evaluations WHERE tenant_id=$1`, otherTenant); n != 0 {
			t.Fatalf("tenant A recorded %d evaluations under its held lock", n)
		}
		if temporal.signals.Load() < 1 {
			t.Fatal("the committed execution was not signalled to its case workflow")
		}
	})
}

// recordingTemporal stands in for Temporal in a worker Tick: it accepts and counts workflow
// signals and starts. Any other call panics on the nil embedded client.
type recordingTemporal struct {
	temporalclient.Client
	signals atomic.Int64
}

func (r *recordingTemporal) SignalWithStartWorkflow(context.Context, string, string, any, temporalclient.StartWorkflowOptions, any, ...any) (temporalclient.WorkflowRun, error) {
	r.signals.Add(1)
	return nil, nil
}

func (r *recordingTemporal) ExecuteWorkflow(context.Context, temporalclient.StartWorkflowOptions, any, ...any) (temporalclient.WorkflowRun, error) {
	r.signals.Add(1)
	return nil, nil
}
