//go:build integration

package httpapi

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/approvals"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/evidence"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/identity"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/migrate"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestProposalStaleOnBoundEvidenceRevocation covers the WIS-274 staleness rule for a revoked
// bound evidence source, through the real revocation route and the decision route:
//   - an approved proposal citing the source becomes stale in the revocation transaction,
//     audited with the revoking person;
//   - a pending proposal citing it can no longer be approved (409 PROPOSAL_STALE, nothing
//     recorded) but can still be rejected;
//   - a proposal citing an unrelated source is not invalidated.
//
// Before this fix the approved proposal stayed approved and the pending one could be approved.
func TestProposalStaleOnBoundEvidenceRevocation(t *testing.T) {
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
	database := "proposal_stale_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	owner, submitter, reviewer, otherOwner := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	issuer, origin := "https://proposal-stale.invalid/"+uuid.NewString(), "http://127.0.0.1:58178"
	exec(`INSERT INTO supervisor.tenants(id,name) VALUES($1,'synthetic stale test')`, tenant)
	exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'stale site')`, tenant, site)
	exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'owner'),($1,$2,$3,$5,'supervisor'),($1,$2,$3,$6,'supervisor')`, tenant, site, issuer, owner, submitter, reviewer)
	exec(`INSERT INTO supervisor.tenants(id,name) VALUES($1,'other tenant')`, otherTenant)
	exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'other site')`, otherTenant, otherSite)
	exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'owner')`, otherTenant, otherSite, issuer, otherOwner)
	type session struct{ token, csrf string }
	seed := func(subject string) session {
		t.Helper()
		v := session{token: uuid.NewString(), csrf: uuid.NewString()}
		sum := sha256.Sum256([]byte(v.token))
		exec(`INSERT INTO supervisor.browser_sessions(token_hash,subject,issuer,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,$4,now(),now(),now()+interval '1 hour',now())`, hex.EncodeToString(sum[:]), subject, issuer, v.csrf)
		return v
	}
	ownerS, reviewerS, otherOwnerS := seed(owner), seed(reviewer), seed(otherOwner)
	auth, err := identity.New(ctx, identity.Config{Issuer: issuer, ClientID: "proposal-stale-test", ClientSecret: "synthetic-only", RedirectURI: origin + "/auth/callback"}, runtime, nil)
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
	base := "/v1/sites/" + site
	createSource := func(title string) string {
		t.Helper()
		id := uuid.NewString()
		body, e := json.Marshal(evidence.Input{SourceID: id, Kind: "document", Title: title, Text: "Synthetic procedure text for " + title})
		if e != nil {
			t.Fatal(e)
		}
		expect(call(ownerS, "POST", base+"/sources", uuid.NewString(), `"0"`, string(body)), 200, "")
		return id
	}
	revoked, unrelated := createSource("Infeed SOP"), createSource("Packaging SOP")

	// Three open cases at proposed, each on its own line, order and context.
	contextDigest := strings.Repeat("a", 64)
	plantCase := func() approvals.Payload {
		t.Helper()
		id, line, order, contextID := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
		snap := `{"readiness":"NOT_READY","digest":"` + contextDigest + `","context_id":"` + contextID + `","order_id":"` + order + `","asset_id":"` + line + `","site_id":"` + site + `"}`
		planted(tenant,
			`INSERT INTO supervisor.knowledge_entity_heads(tenant_id,site_id,id,current_revision) VALUES('`+tenant+`','`+site+`','`+line+`',1),('`+tenant+`','`+site+`','`+order+`',1)`,
			`INSERT INTO supervisor.plant_contexts(tenant_id,site_id,id,order_id,asset_id,window_start,window_end,process_snapshot_id,process_snapshot_sha256,readiness,snapshot,digest,actor_issuer,actor_subject,evaluated_at)
 VALUES('`+tenant+`','`+site+`','`+contextID+`','`+order+`','`+line+`','2026-10-22T06:00:00Z','2026-10-22T06:15:00Z','`+uuid.NewString()+`','`+strings.Repeat("d", 64)+`','NOT_READY','`+snap+`','`+contextDigest+`','test','operator','2026-10-22T06:15:00Z')`,
			`INSERT INTO supervisor.cases(tenant_id,site_id,id,line_id,order_id,exception_kind,episode,revision,context_snapshot_id,context_revision,context_digest,state,detected_at,latest_window_end,workflow_id)
 VALUES('`+tenant+`','`+site+`','`+id+`','`+line+`','`+order+`','production_shortfall',1,3,'`+contextID+`',1,'`+contextDigest+`','proposed','2026-10-22T06:15:00Z','2026-10-22T06:15:00Z','case:`+tenant+`:`+id+`')`,
			`INSERT INTO supervisor.case_transitions(tenant_id,site_id,case_id,revision,from_state,to_state,event,actor_issuer,actor_subject) VALUES('`+tenant+`','`+site+`','`+id+`',1,'none','detected','trigger','svc','ingestion'),('`+tenant+`','`+site+`','`+id+`',2,'detected','investigating','context_ready','svc','case-workflow'),('`+tenant+`','`+site+`','`+id+`',3,'investigating','proposed','proposal_validated','svc','case-workflow')`)
		return approvals.Payload{
			ActionKind: "maintenance_investigation.create", Template: "inspect_report_only",
			AssetID: line, LineID: line, OrderID: order, AssigneeID: uuid.NewString(),
			Instructions: "Inspect the infeed conveyor and report findings.", DueAt: time.Now().UTC().Add(4 * time.Hour).Truncate(time.Second),
			CaseID: id, CaseRevision: 3, ContextSnapshotID: contextID, ContextRevision: 1, ContextDigest: contextDigest,
			ConfigRevision: 1, GraphRevision: 1, PolicyRevision: 1,
			GeneratedByRun: uuid.NewString(),
		}
	}
	service := approvals.NewService(db.DB)
	scope := approvals.Scope{TenantID: tenant, SiteID: site}
	submit := func(source string) approvals.View {
		t.Helper()
		p := plantCase()
		p.Evidence = []approvals.Ref{{Kind: "plant_context", ID: p.ContextSnapshotID, Revision: 1}, {Kind: "evidence", ID: source, Revision: 1}}
		v, e := service.Submit(ctx, scope, approvals.Submission{Payload: p, SubmittedBy: &approvals.Actor{Issuer: issuer, Subject: submitter}})
		if e != nil {
			t.Fatal(e)
		}
		return v
	}
	path := func(id string) string { return base + "/proposals/" + id }
	decide := func(id, kind string, revision int64) *httptest.ResponseRecorder {
		t.Helper()
		rev := strconv.FormatInt(revision, 10)
		return call(reviewerS, "POST", path(id)+"/decisions", uuid.NewString(), `"`+rev+`"`, `{"decision":"`+kind+`","expected_revision":`+rev+`,"reason":"reviewed the evidence"}`)
	}
	read := func(id string) approvals.View {
		t.Helper()
		w := call(reviewerS, "GET", path(id), "", "", "")
		expect(w, 200, "")
		var v approvals.View
		if e := json.Unmarshal(w.Body.Bytes(), &v); e != nil {
			t.Fatalf("%v: %s", e, w.Body.String())
		}
		return v
	}

	approved, pending, untouched := submit(revoked), submit(revoked), submit(unrelated)
	expect(decide(approved.ID, "approve", approved.Revision), 200, "")
	expect(decide(untouched.ID, "approve", untouched.Revision), 200, "")
	if v := read(approved.ID); v.State != approvals.Approved {
		t.Fatalf("setup: %+v", v)
	}

	var state int64
	if err = control.QueryRowContext(ctx, `SELECT state_revision FROM supervisor.evidence_heads WHERE tenant_id=$1 AND source_id=$2`, tenant, revoked).Scan(&state); err != nil {
		t.Fatal(err)
	}
	revokeKey := uuid.NewString()
	expect(call(ownerS, "POST", base+"/sources/"+revoked+"/revocations", revokeKey, `"`+strconv.FormatInt(state, 10)+`"`, `{"reason":"SOP withdrawn by quality"}`), 200, "")
	// An idempotent replay of the revocation marks nothing a second time.
	expect(call(ownerS, "POST", base+"/sources/"+revoked+"/revocations", revokeKey, `"`+strconv.FormatInt(state, 10)+`"`, `{"reason":"SOP withdrawn by quality"}`), 200, "")
	var staleAudits int
	if err = control.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.approval_audit WHERE proposal_id=$1 AND event_type='proposal.stale'`, approved.ID).Scan(&staleAudits); err != nil || staleAudits != 1 {
		t.Fatalf("stale audits after a replayed revocation: %d (%v), want 1", staleAudits, err)
	}

	// The approval citing the revoked source can no longer be consumed: stale, audited with
	// the person who revoked the source, in the same transaction as the revocation.
	if v := read(approved.ID); v.State != approvals.Stale {
		t.Fatalf("approved proposal citing a revoked source: state %s, want stale", v.State)
	}
	var auditIssuer, auditSubject string
	if err = control.QueryRowContext(ctx, `SELECT actor_issuer,actor_subject FROM supervisor.approval_audit WHERE proposal_id=$1 AND event_type='proposal.stale'`, approved.ID).Scan(&auditIssuer, &auditSubject); err != nil || auditIssuer != issuer || auditSubject != owner {
		t.Fatalf("stale audit actor %s/%s (%v), want the revoking owner", auditIssuer, auditSubject, err)
	}
	// Another tenant revoking a source with the same client-chosen UUID touches nothing here.
	otherBase := "/v1/sites/" + otherSite
	clone, e := json.Marshal(evidence.Input{SourceID: unrelated, Kind: "document", Title: "Other tenant SOP", Text: "Same UUID, other tenant"})
	if e != nil {
		t.Fatal(e)
	}
	expect(call(otherOwnerS, "POST", otherBase+"/sources", uuid.NewString(), `"0"`, string(clone)), 200, "")
	var otherState int64
	if err = control.QueryRowContext(ctx, `SELECT state_revision FROM supervisor.evidence_heads WHERE tenant_id=$1 AND source_id=$2`, otherTenant, unrelated).Scan(&otherState); err != nil {
		t.Fatal(err)
	}
	expect(call(otherOwnerS, "POST", otherBase+"/sources/"+unrelated+"/revocations", uuid.NewString(), `"`+strconv.FormatInt(otherState, 10)+`"`, `{"reason":"Other tenant withdrawal"}`), 200, "")
	// Unrelated evidence: still approved (rule: do not invalidate on unrelated changes).
	if v := read(untouched.ID); v.State != approvals.Approved {
		t.Fatalf("proposal citing an unrelated source: state %s, want approved", v.State)
	}
	// A pending proposal citing the source cannot be approved and nothing is recorded...
	expect(decide(pending.ID, "approve", pending.Revision), 409, "EVIDENCE_UNAVAILABLE")
	var decisions int
	if err = control.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.approval_decisions WHERE proposal_id=$1`, pending.ID).Scan(&decisions); err != nil || decisions != 0 {
		t.Fatalf("refused approval recorded %d decisions (%v)", decisions, err)
	}
	// ...but a reviewer can still reject it.
	expect(decide(pending.ID, "reject", pending.Revision), 200, "")
	if v := read(pending.ID); v.State != approvals.Rejected {
		t.Fatalf("pending proposal after rejection: %s", v.State)
	}
	// A proposal citing a source that does not exist on this site fails closed the same way.
	missing := submit(uuid.NewString())
	expect(decide(missing.ID, "approve", missing.Revision), 409, "EVIDENCE_UNAVAILABLE")
}
