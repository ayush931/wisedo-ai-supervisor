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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/identity"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/migrate"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/modelhosting"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/google/uuid"
)

// TestModelHostingSwitchHTTPPostgres (WIS-623): the per-site hosted-inference switch. Off until
// decided; only a recently authenticated owner of the site decides, with a DPA reference to turn
// it on; supervisors read; everyone else, and every other tenant, is refused.
func TestModelHostingSwitchHTTPPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	adminURL, runtimeURL := os.Getenv("SUPERVISOR_TEST_ADMIN_URL"), os.Getenv("SUPERVISOR_TEST_DATABASE_URL")
	if adminURL == "" || runtimeURL == "" {
		t.Fatal("BLOCKED: disposable PostgreSQL admin and runtime URLs required")
	}
	control, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = control.Close() })
	name := "model_hosting_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = control.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = control.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})
	in := func(raw string) string { u, _ := url.Parse(raw); u.Path = "/" + name; return u.String() }
	admin, err := sql.Open("pgx", in(adminURL))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	if err = migrate.Apply(ctx, admin); err != nil {
		t.Fatal(err)
	}
	db, err := store.New(in(runtimeURL))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := admin.ExecContext(ctx, q, args...); e != nil {
			t.Fatalf("%s: %v", q, e)
		}
	}
	tenant, site, otherTenant, otherSite := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	issuer, origin := "https://hosting.invalid/"+uuid.NewString(), "http://127.0.0.1:58176"
	exec(`INSERT INTO supervisor.tenants(id,name) VALUES($1,'hosting'),($2,'other')`, tenant, otherTenant)
	exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'plant'),($3,$4,'other plant')`, tenant, site, otherTenant, otherSite)
	type sess struct{ token, csrf string }
	seed := func(role, tenantID, siteID string, authAge time.Duration) sess {
		s := sess{token: uuid.NewString(), csrf: uuid.NewString()}
		subject := uuid.NewString()
		sum := sha256.Sum256([]byte(s.token))
		exec(`INSERT INTO supervisor.browser_sessions(token_hash,subject,issuer,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,$4,now(),now(),now()+interval '1 hour',now()-$5::interval)`,
			hex.EncodeToString(sum[:]), subject, issuer, s.csrf, authAge.String())
		exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,$5)`, tenantID, siteID, issuer, subject, role)
		return s
	}
	owner, staleOwner := seed("owner", tenant, site, 0), seed("owner", tenant, site, 2*time.Hour)
	supervisor, viewer, outsider := seed("supervisor", tenant, site, 0), seed("viewer", tenant, site, 0), seed("owner", otherTenant, otherSite, 0)
	auth, err := identity.New(ctx, identity.Config{Issuer: issuer, ClientID: "hosting-test", ClientSecret: "synthetic-only", RedirectURI: origin + "/auth/callback"}, db.DB, nil)
	if err != nil {
		t.Fatal(err)
	}
	handler := New(config.Config{Environment: "local", WebDir: t.TempDir()}, db, auth)
	path := "/v1/sites/" + site + "/model-hosting"
	call := func(a sess, method, p, key, match, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, p, strings.NewReader(body))
		r.AddCookie(&http.Cookie{Name: "supervisor", Value: a.token})
		r.Header.Set("Origin", origin)
		r.Header.Set("X-CSRF-Token", a.csrf)
		r.Header.Set("Content-Type", "application/json")
		if key != "" {
			r.Header.Set("Idempotency-Key", key)
		}
		if match != "" {
			r.Header.Set("If-Match", match)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		conformsToPublished(t, r, w)
		return w
	}
	read := func(w *httptest.ResponseRecorder) modelhosting.SiteModelHostingV1 {
		t.Helper()
		var v modelhosting.SiteModelHostingV1
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &v) != nil {
			t.Fatalf("HTTP %d %s", w.Code, w.Body.String())
		}
		return v
	}
	on := `{"enabled":true,"dpa_reference":"DPA-2026-11-05","reason":"Pilot on hosted Qwen"}`

	// Never decided: off, revision 0; owners and supervisors read, viewers and outsiders do not.
	if v := read(call(supervisor, "GET", path, "", "", "")); v.Enabled || v.Revision != 0 || len(v.Decisions) != 0 {
		t.Fatalf("initial: %+v", v)
	}
	wantProblem(t, call(viewer, "GET", path, "", "", ""), 403, "PERMISSION_DENIED")
	wantProblem(t, call(outsider, "GET", path, "", "", ""), 404, "NOT_FOUND")

	// Only a recently authenticated owner decides; the body cannot name a decider or a tenant.
	wantProblem(t, call(supervisor, "POST", path+"/decisions", uuid.NewString(), `"0"`, on), 403, "PERMISSION_DENIED")
	wantProblem(t, call(viewer, "POST", path+"/decisions", uuid.NewString(), `"0"`, on), 403, "PERMISSION_DENIED")
	wantProblem(t, call(outsider, "POST", path+"/decisions", uuid.NewString(), `"0"`, on), 404, "NOT_FOUND")
	wantProblem(t, call(staleOwner, "POST", path+"/decisions", uuid.NewString(), `"0"`, on), 428, "REAUTHENTICATION_REQUIRED")
	wantProblem(t, call(owner, "POST", path+"/decisions", uuid.NewString(), `"0"`, `{"enabled":true,"reason":"No DPA"}`), 422, "INVALID_MODEL_HOSTING_DECISION")
	wantProblem(t, call(owner, "POST", path+"/decisions", uuid.NewString(), `"0"`, `{"enabled":true,"dpa_reference":"D","reason":"r","tenant_id":"`+otherTenant+`"}`), 422, "INVALID_MODEL_HOSTING_DECISION")
	wantProblem(t, call(owner, "POST", path+"/decisions", uuid.NewString(), `"0"`, `{"enabled":false,"reason":"Already off"}`), 409, "MODEL_HOSTING_UNCHANGED")
	var heads int
	if err = admin.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.site_model_hosting_heads`).Scan(&heads); err != nil || heads != 0 {
		t.Fatalf("refused decisions wrote %d heads (%v)", heads, err)
	}

	// The owner turns it on; the same key and body replays, another body conflicts.
	key := uuid.NewString()
	v := read(call(owner, "POST", path+"/decisions", key, `"0"`, on))
	if !v.Enabled || v.Revision != 1 || v.DPAReference == nil || *v.DPAReference != "DPA-2026-11-05" || len(v.Decisions) != 1 || v.Decisions[0].Reason != "Pilot on hosted Qwen" {
		t.Fatalf("on: %+v", v)
	}
	if again := read(call(owner, "POST", path+"/decisions", key, `"0"`, on)); !again.Replayed || again.Revision != 1 {
		t.Fatalf("replay: %+v", again)
	}
	wantProblem(t, call(owner, "POST", path+"/decisions", key, `"0"`, `{"enabled":false,"reason":"other"}`), 409, "IDEMPOTENCY_CONFLICT")
	wantProblem(t, call(owner, "POST", path+"/decisions", uuid.NewString(), `"0"`, `{"enabled":false,"reason":"stale"}`), 412, "REVISION_CONFLICT")

	// Review finding 6: "enabled" must be present and a boolean. A body without it, or with null,
	// used to decode as false and turn hosting off; it is refused and the switch stays on.
	for _, body := range []string{`{"reason":"Pause"}`, `{"enabled":null,"reason":"Pause"}`} {
		wantProblem(t, call(owner, "POST", path+"/decisions", uuid.NewString(), `"1"`, body), 422, "INVALID_MODEL_HOSTING_DECISION")
	}
	if v := read(call(owner, "GET", path, "", "", "")); !v.Enabled || v.Revision != 1 {
		t.Fatalf("a body without enabled changed the switch: %+v", v)
	}
	// Off again: the head moves to revision 2, both decisions are kept, newest first.
	off := read(call(owner, "POST", path+"/decisions", uuid.NewString(), `"1"`, `{"enabled":false,"reason":"Pause"}`))
	if off.Enabled || off.Revision != 2 || off.DPAReference != nil || len(off.Decisions) != 2 || off.Decisions[0].Revision != 2 {
		t.Fatalf("off: %+v", off)
	}
	// Review finding 5: a retry replays the result its key recorded, not the current state. Key
	// "key" turned hosting on at revision 1; after the off decision it still answers that.
	if again := read(call(owner, "POST", path+"/decisions", key, `"0"`, on)); !again.Replayed || !again.Enabled || again.Revision != 1 || len(again.Decisions) != 1 || again.DPAReference == nil {
		t.Fatalf("replay after a later decision: %+v", again)
	}
	// Review finding 5: concurrent retries of one key (a double-click, a client retry racing the
	// first attempt) all succeed: one decides, the rest replay it. Before the fix the receipt was
	// read before the head lock, so the losers saw a moved revision and got 412. Holding the head
	// row until all eight requests wait on a lock makes the interleaving deterministic.
	hold, err := admin.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = hold.ExecContext(ctx, `SELECT 1 FROM supervisor.site_model_hosting_heads WHERE tenant_id=$1 AND site_id=$2 FOR UPDATE`, tenant, site); err != nil {
		t.Fatal(err)
	}
	retry, retries := uuid.NewString(), make([]*httptest.ResponseRecorder, 8)
	var wg sync.WaitGroup
	for i := range retries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			retries[i] = call(owner, "POST", path+"/decisions", retry, `"2"`, on)
		}()
	}
	for waiting := 0; waiting < len(retries); {
		if err = admin.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=$1 AND wait_event_type='Lock'`, name).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err = hold.Rollback(); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	decided := 0
	for _, w := range retries {
		var v modelhosting.SiteModelHostingV1
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &v) != nil || !v.Enabled || v.Revision != 3 {
			t.Fatalf("concurrent retry: HTTP %d %s", w.Code, w.Body.String())
		}
		if !v.Replayed {
			decided++
		}
	}
	if decided != 1 {
		t.Fatalf("%d of the concurrent retries decided", decided)
	}
	// Codex review: the same race on a site's FIRST decision, where no head row exists to lock.
	// Before the fix every retry missed the receipt and raced to insert the head; the losers hit
	// the primary key and answered 503. An uncommitted head insert holds them all at that point.
	fresh := uuid.NewString()
	exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'second plant')`, tenant, fresh)
	freshOwner := seed("owner", tenant, fresh, 0)
	block, err := admin.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = block.ExecContext(ctx, `INSERT INTO supervisor.site_model_hosting_heads(tenant_id,site_id,enabled,dpa_reference,revision) VALUES($1,$2,true,'held',1)`, tenant, fresh); err != nil {
		t.Fatal(err)
	}
	first, firsts := uuid.NewString(), make([]*httptest.ResponseRecorder, 8)
	var fwg sync.WaitGroup
	for i := range firsts {
		fwg.Add(1)
		go func() {
			defer fwg.Done()
			firsts[i] = call(freshOwner, "POST", "/v1/sites/"+fresh+"/model-hosting/decisions", first, `"0"`, on)
		}()
	}
	for waiting := 0; waiting < len(firsts); {
		if err = admin.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=$1 AND wait_event_type='Lock'`, name).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err = block.Rollback(); err != nil {
		t.Fatal(err)
	}
	fwg.Wait()
	decided = 0
	for _, w := range firsts {
		var v modelhosting.SiteModelHostingV1
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &v) != nil || !v.Enabled || v.Revision != 1 || len(v.Decisions) != 1 {
			t.Fatalf("concurrent first decision: HTTP %d %s", w.Code, w.Body.String())
		}
		if !v.Replayed {
			decided++
		}
	}
	if decided != 1 {
		t.Fatalf("%d of the concurrent first decisions decided", decided)
	}
	// WIS-623 post-merge review (P2): an owner's decision cannot commit after the owner's
	// revocation. The membership check used to take no lock, so a revocation committing between
	// the check and the commit let the revoked owner's decision through. Holding the revocation
	// uncommitted, the decision must wait for it and then be refused, not commit around it.
	revoked := seed("owner", tenant, site, 0)
	revokedHash := sha256.Sum256([]byte(revoked.token))
	revoke, err := admin.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = revoke.ExecContext(ctx, `UPDATE supervisor.memberships m SET active=false FROM supervisor.browser_sessions s
 WHERE s.token_hash=$1 AND (m.tenant_id,m.site_id,m.issuer,m.subject)=($2,$3,s.issuer,s.subject)`, hex.EncodeToString(revokedHash[:]), tenant, site); err != nil {
		t.Fatal(err)
	}
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		result <- call(revoked, "POST", path+"/decisions", uuid.NewString(), `"3"`, `{"enabled":false,"reason":"Revoked owner"}`)
	}()
	for waiting := 0; waiting == 0; {
		select {
		case w := <-result:
			_ = revoke.Rollback()
			t.Fatalf("the decision did not wait for the revocation in flight: HTTP %d %s", w.Code, w.Body.String())
		default:
		}
		if err = admin.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=$1 AND wait_event_type='Lock'`, name).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err = revoke.Commit(); err != nil {
		t.Fatal(err)
	}
	wantProblem(t, <-result, 404, "NOT_FOUND")
	if v := read(call(owner, "GET", path, "", "", "")); !v.Enabled || v.Revision != 3 {
		t.Fatalf("a revoked owner's decision changed the switch: %+v", v)
	}
	// The other tenant's site is untouched, and RLS hides this tenant's rows from it.
	if v := read(call(outsider, "GET", "/v1/sites/"+otherSite+"/model-hosting", "", "", "")); v.Enabled || v.Revision != 0 {
		t.Fatalf("other tenant sees %+v", v)
	}
	// Decisions are retained: the runtime role cannot rewrite or delete them.
	if _, err = db.DB.ExecContext(ctx, `DELETE FROM supervisor.site_model_hosting_events`); err == nil {
		t.Fatal("the runtime role deleted model hosting decisions")
	}
}
