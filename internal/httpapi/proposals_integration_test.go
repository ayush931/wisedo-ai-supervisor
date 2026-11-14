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
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/approvals"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/cases"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/identity"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/migrate"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestProposalApprovalsHTTPPostgres covers the WIS-274 required cases end to end against
// PostgreSQL: simultaneous approval/rejection, replay, key reuse, header/body mismatch,
// requester self-approval, owner without the grant, a forged body principal, expiry,
// stale binding, supersession, revocation, scope revocation, restart and cross-tenant reads.
func TestProposalApprovalsHTTPPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
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
	database := "proposal_http_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	// planted runs statements as the superuser in tenant scope with triggers and FK checks
	// off, for fixture rows behind the WIS-272 pipeline and for moving the clock-bound state.
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

	tenant, site, otherTenant, otherSite := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	owner, submitter, reviewerA, reviewerB, staleReviewer, viewer, outsider := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	issuer, origin := "https://proposal-http.invalid/"+uuid.NewString(), "http://127.0.0.1:58177"
	exec(`INSERT INTO supervisor.tenants(id,name) VALUES($1,'synthetic proposal test'),($2,'other tenant')`, tenant, otherTenant)
	exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'proposal site'),($3,$4,'other site')`, tenant, site, otherTenant, otherSite)
	exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'owner'),($1,$2,$3,$5,'supervisor'),($1,$2,$3,$6,'supervisor'),($1,$2,$3,$7,'supervisor'),($1,$2,$3,$8,'supervisor'),($1,$2,$3,$9,'viewer')`, tenant, site, issuer, owner, submitter, reviewerA, reviewerB, staleReviewer, viewer)
	exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'supervisor')`, otherTenant, otherSite, issuer, outsider)
	type session struct{ token, csrf, hash, subject string }
	seed := func(subject string, authAge time.Duration) session {
		t.Helper()
		v := session{token: uuid.NewString(), csrf: uuid.NewString(), subject: subject}
		sum := sha256.Sum256([]byte(v.token))
		v.hash = hex.EncodeToString(sum[:])
		exec(`INSERT INTO supervisor.browser_sessions(token_hash,subject,issuer,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,$4,now(),now(),now()+interval '1 hour',now()-$5::interval)`, v.hash, subject, issuer, v.csrf, strconv.Itoa(int(authAge.Seconds()))+" seconds")
		return v
	}
	ownerS, submitterS, aS, bS, viewerS, outsiderS := seed(owner, 0), seed(submitter, 0), seed(reviewerA, 0), seed(reviewerB, 0), seed(viewer, 0), seed(outsider, 0)
	staleS := seed(staleReviewer, 20*time.Minute)
	auth, err := identity.New(ctx, identity.Config{Issuer: issuer, ClientID: "proposal-test", ClientSecret: "synthetic-only", RedirectURI: origin + "/auth/callback"}, runtime, nil)
	if err != nil {
		t.Fatal(err)
	}
	handler := New(config.Config{Environment: "local", WebDir: t.TempDir()}, db, auth)
	call := func(h http.Handler, actor session, method, path, key, revision, body string) *httptest.ResponseRecorder {
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
		h.ServeHTTP(w, r)
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
	view := func(w *httptest.ResponseRecorder) approvals.View {
		t.Helper()
		var v approvals.View
		if e := json.Unmarshal(w.Body.Bytes(), &v); e != nil {
			t.Fatalf("%v: %s", e, w.Body.String())
		}
		return v
	}
	decision := func(kind string, revision int64, reason string) string {
		return `{"decision":"` + kind + `","expected_revision":` + strconv.FormatInt(revision, 10) + `,"reason":"` + reason + `"}`
	}

	// One open case bound to a plant context for line and order, with its real history: it was
	// detected, investigated and reached proposed at revision 3, where a proposal is submitted.
	caseID, line, order, contextID, contextDigest := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), strings.Repeat("a", 64)
	snapshot := `{"readiness":"NOT_READY","digest":"` + contextDigest + `","context_id":"` + contextID + `","order_id":"` + order + `","asset_id":"` + line + `","site_id":"` + site + `"}`
	planted(tenant,
		`INSERT INTO supervisor.knowledge_entity_heads(tenant_id,site_id,id,current_revision) VALUES('`+tenant+`','`+site+`','`+line+`',1),('`+tenant+`','`+site+`','`+order+`',1)`,
		`INSERT INTO supervisor.plant_contexts(tenant_id,site_id,id,order_id,asset_id,window_start,window_end,process_snapshot_id,process_snapshot_sha256,readiness,snapshot,digest,actor_issuer,actor_subject,evaluated_at)
 VALUES('`+tenant+`','`+site+`','`+contextID+`','`+order+`','`+line+`','2026-10-22T06:00:00Z','2026-10-22T06:15:00Z','`+uuid.NewString()+`','`+strings.Repeat("d", 64)+`','NOT_READY','`+snapshot+`','`+contextDigest+`','test','operator','2026-10-22T06:15:00Z')`,
		`INSERT INTO supervisor.cases(tenant_id,site_id,id,line_id,order_id,exception_kind,episode,revision,context_snapshot_id,context_revision,context_digest,state,detected_at,latest_window_end,workflow_id)
 VALUES('`+tenant+`','`+site+`','`+caseID+`','`+line+`','`+order+`','production_shortfall',1,3,'`+contextID+`',1,'`+contextDigest+`','proposed','2026-10-22T06:15:00Z','2026-10-22T06:15:00Z','case:`+tenant+`:`+caseID+`')`,
		`INSERT INTO supervisor.case_transitions(tenant_id,site_id,case_id,revision,from_state,to_state,event,actor_issuer,actor_subject) VALUES('`+tenant+`','`+site+`','`+caseID+`',1,'none','detected','trigger','svc','ingestion'),('`+tenant+`','`+site+`','`+caseID+`',2,'detected','investigating','context_ready','svc','case-workflow'),('`+tenant+`','`+site+`','`+caseID+`',3,'investigating','proposed','proposal_validated','svc','case-workflow')`)

	service := approvals.NewService(db.DB)
	scope := approvals.Scope{TenantID: tenant, SiteID: site}
	submitterActor := &approvals.Actor{Issuer: issuer, Subject: submitter}
	payload := approvals.Payload{
		ActionKind: "maintenance_investigation.create", Template: "inspect_report_only",
		AssetID: line, LineID: line, OrderID: order, AssigneeID: uuid.NewString(),
		Instructions: "Inspect the infeed conveyor and report findings.", DueAt: time.Now().UTC().Add(4 * time.Hour).Truncate(time.Second),
		CaseID: caseID, CaseRevision: 3, ContextSnapshotID: contextID, ContextRevision: 1, ContextDigest: contextDigest,
		ConfigRevision: 1, GraphRevision: 1, PolicyRevision: 1,
		Evidence:       []approvals.Ref{{Kind: "plant_context", ID: contextID, Revision: 1}},
		GeneratedByRun: uuid.NewString(),
	}
	submit := func() approvals.View {
		t.Helper()
		v, e := service.Submit(ctx, scope, approvals.Submission{Payload: payload, SubmittedBy: submitterActor})
		if e != nil {
			t.Fatal(e)
		}
		return v
	}
	path := func(id string) string { return "/v1/sites/" + site + "/proposals/" + id }
	// plantCase seeds another open case at proposed (revision 3) on its own line, order and
	// context, and returns a payload bound to it.
	plantCase := func() approvals.Payload {
		t.Helper()
		id, ln, ord, ctxID := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
		snap := `{"readiness":"NOT_READY","digest":"` + contextDigest + `","context_id":"` + ctxID + `","order_id":"` + ord + `","asset_id":"` + ln + `","site_id":"` + site + `"}`
		planted(tenant,
			`INSERT INTO supervisor.knowledge_entity_heads(tenant_id,site_id,id,current_revision) VALUES('`+tenant+`','`+site+`','`+ln+`',1),('`+tenant+`','`+site+`','`+ord+`',1)`,
			`INSERT INTO supervisor.plant_contexts(tenant_id,site_id,id,order_id,asset_id,window_start,window_end,process_snapshot_id,process_snapshot_sha256,readiness,snapshot,digest,actor_issuer,actor_subject,evaluated_at)
 VALUES('`+tenant+`','`+site+`','`+ctxID+`','`+ord+`','`+ln+`','2026-10-22T06:00:00Z','2026-10-22T06:15:00Z','`+uuid.NewString()+`','`+strings.Repeat("d", 64)+`','NOT_READY','`+snap+`','`+contextDigest+`','test','operator','2026-10-22T06:15:00Z')`,
			`INSERT INTO supervisor.cases(tenant_id,site_id,id,line_id,order_id,exception_kind,episode,revision,context_snapshot_id,context_revision,context_digest,state,detected_at,latest_window_end,workflow_id)
 VALUES('`+tenant+`','`+site+`','`+id+`','`+ln+`','`+ord+`','production_shortfall',1,3,'`+ctxID+`',1,'`+contextDigest+`','proposed','2026-10-22T06:15:00Z','2026-10-22T06:15:00Z','case:`+tenant+`:`+id+`')`,
			`INSERT INTO supervisor.case_transitions(tenant_id,site_id,case_id,revision,from_state,to_state,event,actor_issuer,actor_subject) VALUES('`+tenant+`','`+site+`','`+id+`',1,'none','detected','trigger','svc','ingestion'),('`+tenant+`','`+site+`','`+id+`',2,'detected','investigating','context_ready','svc','case-workflow'),('`+tenant+`','`+site+`','`+id+`',3,'investigating','proposed','proposal_validated','svc','case-workflow')`)
		p := payload
		p.CaseID, p.LineID, p.AssetID, p.OrderID, p.ContextSnapshotID = id, ln, ln, ord, ctxID
		p.Evidence = []approvals.Ref{{Kind: "plant_context", ID: ctxID, Revision: 1}}
		return p
	}
	submitFor := func(p approvals.Payload) approvals.View {
		t.Helper()
		v, e := service.Submit(ctx, scope, approvals.Submission{Payload: p, SubmittedBy: submitterActor})
		if e != nil {
			t.Fatal(e)
		}
		return v
	}

	// A submission that disagrees with its case binding is refused before any write.
	wrong := payload
	wrong.ContextDigest = strings.Repeat("b", 64)
	if _, err = service.Submit(ctx, scope, approvals.Submission{Payload: wrong}); !errors.Is(err, approvals.ErrInvalidPayload) {
		t.Fatalf("mismatched binding: %v", err)
	}

	race := submit()
	if race.State != approvals.Pending || race.Revision != 1 || race.ProposalRevision != 1 {
		t.Fatalf("new proposal: %+v", race)
	}

	// Reads: any member sees the exact canonical bytes the digest covers; another tenant's
	// principal gets the non-disclosing 404, and its own list is empty.
	w := call(handler, viewerS, "GET", path(race.ID), "", "", "")
	expect(w, 200, "")
	if w.Header().Get("ETag") != `"1"` {
		t.Fatalf("ETag %q", w.Header().Get("ETag"))
	}
	got := view(w)
	sum := sha256.Sum256(got.CanonicalPayload)
	if hex.EncodeToString(sum[:]) != got.CanonicalPayloadSHA256 || !strings.Contains(string(got.CanonicalPayload), `"proposal_id":"`+race.ID+`"`) {
		t.Fatalf("canonical payload does not match its digest: %s", got.CanonicalPayload)
	}
	expect(call(handler, outsiderS, "GET", path(race.ID), "", "", ""), 404, "NOT_FOUND")
	expect(call(handler, outsiderS, "GET", "/v1/sites/"+otherSite+"/proposals/"+race.ID, "", "", ""), 404, "NOT_FOUND")
	w = call(handler, outsiderS, "GET", "/v1/sites/"+otherSite+"/proposals", "", "", "")
	expect(w, 200, "")
	if !strings.Contains(w.Body.String(), `"items":[]`) {
		t.Fatalf("foreign list leaked: %s", w.Body.String())
	}
	w = call(handler, viewerS, "GET", "/v1/sites/"+site+"/proposals?limit=1", "", "", "")
	expect(w, 200, "")
	if !strings.Contains(w.Body.String(), race.ID) {
		t.Fatalf("site list: %s", w.Body.String())
	}
	expect(call(handler, viewerS, "GET", "/v1/sites/"+site+"/proposals?limit=101", "", "", ""), 422, "INVALID_REQUEST")

	// Contract and authority refusals change nothing.
	key := uuid.NewString()
	expect(call(handler, aS, "POST", path(race.ID)+"/decisions", key, `"1"`, `{"decision":"approve","expected_revision":1,"reason":"ok","reviewer_subject":"`+submitter+`"}`), 422, "INVALID_DECISION")
	expect(call(handler, aS, "POST", path(race.ID)+"/decisions", key, `"1"`, decision("approve", 2, "header and body disagree")), 422, "INVALID_DECISION")
	expect(call(handler, aS, "POST", path(race.ID)+"/decisions", "not-a-uuid", `"1"`, decision("approve", 1, "ok")), 422, "INVALID_DECISION")
	expect(call(handler, aS, "POST", path(race.ID)+"/decisions", key, "", decision("approve", 1, "ok")), 428, "REVISION_REQUIRED")
	expect(call(handler, submitterS, "POST", path(race.ID)+"/decisions", uuid.NewString(), `"1"`, decision("approve", 1, "my own proposal")), 403, "REVIEWER_NOT_ELIGIBLE")
	expect(call(handler, ownerS, "POST", path(race.ID)+"/decisions", uuid.NewString(), `"1"`, decision("approve", 1, "owner bypass")), 403, "PERMISSION_DENIED")
	expect(call(handler, viewerS, "POST", path(race.ID)+"/decisions", uuid.NewString(), `"1"`, decision("approve", 1, "viewer")), 403, "PERMISSION_DENIED")
	expect(call(handler, staleS, "POST", path(race.ID)+"/decisions", uuid.NewString(), `"1"`, decision("approve", 1, "old login")), 428, "REAUTHENTICATION_REQUIRED")
	expect(call(handler, outsiderS, "POST", path(race.ID)+"/decisions", uuid.NewString(), `"1"`, decision("approve", 1, "foreign")), 404, "NOT_FOUND")
	var decisions int
	if err = control.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.approval_decisions WHERE proposal_id=$1`, race.ID).Scan(&decisions); err != nil || decisions != 0 {
		t.Fatalf("refused requests recorded %d decisions (%v)", decisions, err)
	}

	// Simultaneous approve and reject by two eligible reviewers: exactly one wins. A held
	// lock on the case row (taken before the proposal in the global lock order) parks both
	// requests until each is waiting, so they really do read the same head concurrently.
	hold, err := control.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = hold.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, tenant); err != nil {
		t.Fatal(err)
	}
	if _, err = hold.ExecContext(ctx, `SELECT 1 FROM supervisor.cases WHERE id=$1 FOR UPDATE`, caseID); err != nil {
		t.Fatal(err)
	}
	keys := map[string]string{"approve": uuid.NewString(), "reject": uuid.NewString()}
	results := map[string]*httptest.ResponseRecorder{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for kind, actor := range map[string]session{"approve": aS, "reject": bS} {
		wg.Add(1)
		go func(kind string, actor session) {
			defer wg.Done()
			r := httptest.NewRequest("POST", path(race.ID)+"/decisions", strings.NewReader(decision(kind, 1, "concurrent "+kind)))
			r.AddCookie(&http.Cookie{Name: "supervisor", Value: actor.token})
			r.Header.Set("Origin", origin)
			r.Header.Set("X-CSRF-Token", actor.csrf)
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Idempotency-Key", keys[kind])
			r.Header.Set("If-Match", `"1"`)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, r)
			mu.Lock()
			results[kind] = rec
			mu.Unlock()
		}(kind, actor)
	}
	for waiting := 0; waiting < 2; {
		if err = control.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=$1 AND wait_event_type='Lock'`, database).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if ctx.Err() != nil {
			t.Fatal("concurrent decisions never reached the case lock")
		}
	}
	if err = hold.Commit(); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	winner, loser := "approve", "reject"
	if results["approve"].Code != 200 {
		winner, loser = "reject", "approve"
	}
	expect(results[winner], 200, "")
	expect(results[loser], 412, "REVISION_CONFLICT")
	won := view(results[winner])
	if won.Revision != 2 || len(won.Decisions) != 1 || won.Decisions[0].Decision != winner || won.Decisions[0].CanonicalPayloadSHA256 != race.CanonicalPayloadSHA256 {
		t.Fatalf("winning decision: %+v", won)
	}
	winnerS := map[string]session{"approve": aS, "reject": bS}[winner]

	// Replay returns the original result, also from a freshly constructed server (restart);
	// the same key with a different request is a conflict.
	body := decision(winner, 1, "concurrent "+winner)
	w = call(handler, winnerS, "POST", path(race.ID)+"/decisions", keys[winner], `"1"`, body)
	expect(w, 200, "")
	if r := view(w); !r.Replayed || r.Revision != 2 || r.State != won.State {
		t.Fatalf("replay: %+v", r)
	}
	restarted := New(config.Config{Environment: "local", WebDir: t.TempDir()}, db, auth)
	w = call(restarted, winnerS, "POST", path(race.ID)+"/decisions", keys[winner], `"1"`, body)
	expect(w, 200, "")
	if r := view(w); !r.Replayed || len(r.Decisions) != 1 {
		t.Fatalf("replay after restart: %+v", r)
	}
	expect(call(handler, winnerS, "POST", path(race.ID)+"/decisions", keys[winner], `"1"`, decision(winner, 1, "a different reason")), 409, "IDEMPOTENCY_CONFLICT")
	// A decided proposal cannot be decided again.
	expect(call(handler, viewerS, "GET", path(race.ID), "", "", ""), 200, "")
	other := map[string]session{"approve": bS, "reject": aS}[winner]
	expect(call(handler, other, "POST", path(race.ID)+"/decisions", uuid.NewString(), `"2"`, decision("approve", 2, "too late")), 409, "ILLEGAL_TRANSITION")

	// One live proposal per case (review finding): while the race winner is approved a second
	// proposal is refused; revoking it frees the case.
	if winner == "approve" {
		if _, err = service.Submit(ctx, scope, approvals.Submission{Payload: payload, SubmittedBy: submitterActor}); !errors.Is(err, approvals.ErrActiveProposal) {
			t.Fatalf("second proposal beside an approved one: %v", err)
		}
		expect(call(handler, aS, "POST", path(race.ID)+"/revoke", uuid.NewString(), `"2"`, `{"expected_revision":2,"reason":"superseded by the next test"}`), 200, "")
	}

	// Revocation: only an approval can be revoked, at its current revision, once.
	revocable := submit()
	w = call(handler, aS, "POST", path(revocable.ID)+"/decisions", uuid.NewString(), `"1"`, decision("approve", 1, "evidence reviewed"))
	expect(w, 200, "")
	if view(w).State != approvals.Approved {
		t.Fatalf("approval: %s", w.Body.String())
	}
	if _, err = service.Submit(ctx, scope, approvals.Submission{Payload: payload}); !errors.Is(err, approvals.ErrActiveProposal) {
		t.Fatalf("second proposal beside an approved one: %v", err)
	}
	expect(call(handler, bS, "POST", path(revocable.ID)+"/revoke", uuid.NewString(), `"1"`, `{"expected_revision":1,"reason":"stale"}`), 412, "REVISION_CONFLICT")
	w = call(handler, aS, "POST", path(revocable.ID)+"/revoke", uuid.NewString(), `"2"`, `{"expected_revision":2,"reason":"line was stopped for unrelated work"}`)
	expect(w, 200, "")
	if r := view(w); r.State != approvals.Revoked || r.Revision != 3 || r.Decisions[1].Decision != "revoke" {
		t.Fatalf("revocation: %+v", r)
	}
	expect(call(handler, bS, "POST", path(revocable.ID)+"/revoke", uuid.NewString(), `"3"`, `{"expected_revision":3,"reason":"again"}`), 409, "ILLEGAL_TRANSITION")

	// Expiry on the server clock: once the window has closed a decision is refused and the
	// expiry is recorded, so the stored state does not lag the clock.
	expiring := submit()
	planted(tenant, `UPDATE supervisor.proposal_revisions SET issued_at=issued_at-interval '31 minutes',expires_at=expires_at-interval '31 minutes' WHERE proposal_id='`+expiring.ID+`'`)
	expect(call(handler, aS, "POST", path(expiring.ID)+"/decisions", uuid.NewString(), `"1"`, decision("approve", 1, "late")), 409, "APPROVAL_EXPIRED")
	w = call(handler, viewerS, "GET", path(expiring.ID), "", "", "")
	expect(w, 200, "")
	if r := view(w); r.State != approvals.Expired || r.Revision != 2 {
		t.Fatalf("expired proposal: %+v", r)
	}

	// A material edit supersedes the pending revision; a decision on the old one is stale.
	edited := submit()
	changed := payload
	changed.Instructions = "Inspect the infeed conveyor and the guard sensor; report findings."
	next, err := service.Submit(ctx, scope, approvals.Submission{ProposalID: edited.ID, ExpectedStateRevision: 1, Payload: changed, SubmittedBy: submitterActor})
	if err != nil {
		t.Fatal(err)
	}
	if next.ProposalRevision != 2 || next.Revision != 2 || next.State != approvals.Pending || next.CanonicalPayloadSHA256 == edited.CanonicalPayloadSHA256 {
		t.Fatalf("edit: %+v", next)
	}
	expect(call(handler, aS, "POST", path(edited.ID)+"/decisions", uuid.NewString(), `"1"`, decision("approve", 1, "old revision")), 412, "REVISION_CONFLICT")
	var superseded int
	if err = control.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.approval_transitions WHERE proposal_id=$1 AND proposal_revision=1 AND to_state='superseded'`, edited.ID).Scan(&superseded); err != nil || superseded != 1 {
		t.Fatalf("superseded transition rows=%d (%v)", superseded, err)
	}
	if _, err = service.Submit(ctx, scope, approvals.Submission{ProposalID: revocable.ID, ExpectedStateRevision: 3, Payload: changed}); !errors.Is(err, approvals.ErrIllegalTransition) {
		t.Fatalf("editing a revoked proposal: %v", err)
	}
	// Review finding (rule 05): whoever submitted or edited the proposal, at any revision, may
	// not review it. Revision 3 is produced by a run, but revisions 1 and 2 were the submitter's.
	regenerated := changed
	regenerated.Instructions = "Inspect the infeed conveyor, the guard sensor and the drive; report findings."
	third, err := service.Submit(ctx, scope, approvals.Submission{ProposalID: edited.ID, ExpectedStateRevision: 2, Payload: regenerated})
	if err != nil || third.ProposalRevision != 3 || third.SubmittedBy != nil {
		t.Fatalf("regenerated revision: %+v %v", third, err)
	}
	expect(call(handler, submitterS, "POST", path(edited.ID)+"/decisions", uuid.NewString(), `"3"`, decision("approve", 3, "my earlier proposal")), 403, "REVIEWER_NOT_ELIGIBLE")

	// Scope revocation takes effect on the next request.
	exec(`UPDATE supervisor.memberships SET active=false WHERE tenant_id=$1 AND site_id=$2 AND subject=$3`, tenant, site, reviewerB)
	expect(call(handler, bS, "GET", path(edited.ID), "", "", ""), 404, "NOT_FOUND")
	expect(call(handler, bS, "POST", path(edited.ID)+"/decisions", uuid.NewString(), `"3"`, decision("approve", 3, "revoked member")), 404, "NOT_FOUND")

	// Binding at decision time: a replaced context makes the proposal stale (approval refused,
	// rejection still possible) ...
	otherContext := uuid.NewString()
	otherSnapshot := `{"readiness":"NOT_READY","digest":"` + strings.Repeat("b", 64) + `","context_id":"` + otherContext + `","order_id":"` + order + `","asset_id":"` + line + `","site_id":"` + site + `"}`
	planted(tenant,
		`INSERT INTO supervisor.plant_contexts(tenant_id,site_id,id,order_id,asset_id,window_start,window_end,process_snapshot_id,process_snapshot_sha256,readiness,snapshot,digest,actor_issuer,actor_subject,evaluated_at)
 VALUES('`+tenant+`','`+site+`','`+otherContext+`','`+order+`','`+line+`','2026-10-22T06:15:00Z','2026-10-22T06:30:00Z','`+uuid.NewString()+`','`+strings.Repeat("d", 64)+`','NOT_READY','`+otherSnapshot+`','`+strings.Repeat("b", 64)+`','test','operator','2026-10-22T06:30:00Z')`,
		`UPDATE supervisor.cases SET context_snapshot_id='`+otherContext+`',context_digest='`+strings.Repeat("b", 64)+`' WHERE id='`+caseID+`'`)
	expect(call(handler, aS, "POST", path(edited.ID)+"/decisions", uuid.NewString(), `"3"`, decision("approve", 3, "context replaced")), 409, "PROPOSAL_STALE")
	expect(call(handler, aS, "POST", path(edited.ID)+"/decisions", uuid.NewString(), `"3"`, decision("reject", 3, "context replaced")), 200, "")
	planted(tenant, `UPDATE supervisor.cases SET context_snapshot_id='`+contextID+`',context_digest='`+contextDigest+`' WHERE id='`+caseID+`'`)
	// ... but the case moving on through its own lifecycle (proposed -> awaiting_approval, a new
	// case revision) does not: that is the normal path to approval (review finding).
	bound := submit()
	// One live proposal per case also holds while that proposal is pending (review finding) ...
	if _, err = service.Submit(ctx, scope, approvals.Submission{Payload: payload}); !errors.Is(err, approvals.ErrActiveProposal) {
		t.Fatalf("second proposal beside a pending one: %v", err)
	}
	// ... and an edit that reopens a rejected or expired proposal of the same case cannot put a
	// second pending proposal beside it (review finding: the check ran only on new submissions).
	if _, err = service.Submit(ctx, scope, approvals.Submission{ProposalID: edited.ID, ExpectedStateRevision: 4, Payload: payload}); !errors.Is(err, approvals.ErrActiveProposal) {
		t.Fatalf("reopening a rejected proposal beside a pending one: %v", err)
	}
	if _, err = service.Submit(ctx, scope, approvals.Submission{ProposalID: expiring.ID, ExpectedStateRevision: 2, Payload: payload}); !errors.Is(err, approvals.ErrActiveProposal) {
		t.Fatalf("reopening an expired proposal beside a pending one: %v", err)
	}
	planted(tenant,
		`INSERT INTO supervisor.case_transitions(tenant_id,site_id,case_id,revision,from_state,to_state,event,actor_issuer,actor_subject) VALUES('`+tenant+`','`+site+`','`+caseID+`',4,'proposed','awaiting_approval','approval_requested','svc','case-workflow')`,
		`UPDATE supervisor.cases SET state='awaiting_approval',revision=4 WHERE id='`+caseID+`'`)
	w = call(handler, aS, "POST", path(bound.ID)+"/decisions", uuid.NewString(), `"1"`, decision("approve", 1, "evidence reviewed"))
	expect(w, 200, "")
	if view(w).State != approvals.Approved {
		t.Fatalf("approval after the case advanced: %s", w.Body.String())
	}
	// An approved proposal of another case on the site, which the cancellation must not touch.
	sibling := submitFor(plantCase())
	expect(call(handler, aS, "POST", path(sibling.ID)+"/decisions", uuid.NewString(), `"1"`, decision("approve", 1, "evidence reviewed")), 200, "")
	// Canceling the case marks its approved proposal stale, in the same transaction: it can
	// never be consumed, and it can no longer be revoked or decided.
	expect(call(handler, aS, "POST", "/v1/sites/"+site+"/cases/"+caseID+"/cancel", uuid.NewString(), `"4"`, `{"expected_revision":4,"reason":"Order withdrawn"}`), 200, "")
	w = call(handler, viewerS, "GET", path(bound.ID), "", "", "")
	expect(w, 200, "")
	if r := view(w); r.State != approvals.Stale || r.Revision != 3 {
		t.Fatalf("approval after case cancellation: %+v", r)
	}
	expect(call(handler, aS, "POST", path(bound.ID)+"/revoke", uuid.NewString(), `"3"`, `{"expected_revision":3,"reason":"too late"}`), 409, "ILLEGAL_TRANSITION")
	var staleRows int
	if err = control.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.approval_transitions WHERE proposal_id=$1 AND event='mark_stale' AND to_state='stale'`, bound.ID).Scan(&staleRows); err != nil || staleRows != 1 {
		t.Fatalf("stale transition rows=%d (%v)", staleRows, err)
	}
	w = call(handler, viewerS, "GET", path(sibling.ID), "", "", "")
	expect(w, 200, "")
	if r := view(w); r.State != approvals.Approved || r.Revision != 2 {
		t.Fatalf("another case's approval after a cancellation: %+v", r)
	}

	// Approval is allowed only while the case waits for it (proposed or awaiting_approval).
	// Review finding: an escalated or needs_input case accepted an approval. The escalation is
	// driven through the case service's own deadline path; rejection stays possible.
	escalatedCase := plantCase()
	escalated := submitFor(escalatedCase)
	planted(tenant,
		`INSERT INTO supervisor.case_transitions(tenant_id,site_id,case_id,revision,from_state,to_state,event,actor_issuer,actor_subject,created_at) VALUES('`+tenant+`','`+site+`','`+escalatedCase.CaseID+`',4,'proposed','awaiting_approval','approval_requested','svc','case-workflow',now()-interval '25 hours')`,
		`UPDATE supervisor.cases SET state='awaiting_approval',revision=4 WHERE id='`+escalatedCase.CaseID+`'`)
	if moved, e := cases.NewService(db.DB).EscalateOnDeadline(ctx, cases.WorkflowRef{TenantID: tenant, SiteID: site, CaseID: escalatedCase.CaseID}, 4); e != nil || !moved {
		t.Fatalf("escalation: %v %v", moved, e)
	}
	expect(call(handler, aS, "POST", path(escalated.ID)+"/decisions", uuid.NewString(), `"1"`, decision("approve", 1, "case escalated")), 409, "PROPOSAL_STALE")
	expect(call(handler, aS, "POST", path(escalated.ID)+"/decisions", uuid.NewString(), `"1"`, decision("reject", 1, "case escalated")), 200, "")
	waitingCase := plantCase()
	waiting := submitFor(waitingCase)
	planted(tenant,
		`INSERT INTO supervisor.case_transitions(tenant_id,site_id,case_id,revision,from_state,to_state,event,actor_issuer,actor_subject) VALUES('`+tenant+`','`+site+`','`+waitingCase.CaseID+`',4,'proposed','awaiting_approval','approval_requested','svc','case-workflow'),('`+tenant+`','`+site+`','`+waitingCase.CaseID+`',5,'awaiting_approval','needs_input','proposal_rejected','svc','case-workflow')`,
		`UPDATE supervisor.cases SET state='needs_input',revision=5 WHERE id='`+waitingCase.CaseID+`'`)
	expect(call(handler, aS, "POST", path(waiting.ID)+"/decisions", uuid.NewString(), `"1"`, decision("approve", 1, "case needs input")), 409, "PROPOSAL_STALE")

	// Every mutation and read is audited in scope.
	var audited int
	if err = control.QueryRowContext(ctx, `SELECT count(DISTINCT event_type) FROM supervisor.approval_audit WHERE tenant_id=$1`, tenant).Scan(&audited); err != nil || audited != 7 {
		t.Fatalf("audit event kinds=%d (%v)", audited, err)
	}
}

// TestProposalStorageOutageIs503 proves an unavailable database is reported as 503
// STORAGE_UNAVAILABLE rather than a client error or a disclosure.
func TestProposalStorageOutageIs503(t *testing.T) {
	runtimeURL := os.Getenv("SUPERVISOR_TEST_DATABASE_URL")
	if runtimeURL == "" {
		t.Fatal("BLOCKED: real PostgreSQL runtime URL required")
	}
	db, err := sql.Open("pgx", runtimeURL)
	if err != nil {
		t.Fatal(err)
	}
	db.Close() // the outage
	service := approvals.NewService(db)
	principal := approvals.Principal{Issuer: "https://idp.invalid", Subject: "reviewer", SessionHash: strings.Repeat("a", 64)}
	scope := approvals.Scope{TenantID: uuid.NewString(), SiteID: uuid.NewString()}
	for name, run := range map[string]func() error{
		"get": func() error { _, e := service.Get(context.Background(), principal, scope, uuid.NewString()); return e },
		"decide": func() error {
			_, e := service.Decide(context.Background(), principal, scope, uuid.NewString(), approvals.DecisionInput{Decision: "approve", ExpectedRevision: 1, Reason: "ok"}, uuid.NewString(), 1)
			return e
		},
	} {
		w := httptest.NewRecorder()
		proposalError(w, run())
		if w.Code != 503 || !strings.Contains(w.Body.String(), `"code":"STORAGE_UNAVAILABLE"`) {
			t.Fatalf("%s during outage: %d %s", name, w.Code, w.Body.String())
		}
	}
}
