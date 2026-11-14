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

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/identity"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/migrate"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/roster"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestMaintenanceRosterHTTPPostgres covers the WIS-276 roster routes against PostgreSQL: each
// rule 07 precondition and each authority boundary maps to its status and problem code, and a
// refused request changes nothing.
func TestMaintenanceRosterHTTPPostgres(t *testing.T) {
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
	database := "roster_http_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
			t.Fatalf("%s: %v", q, e)
		}
	}
	tenant, site, otherTenant, otherSite := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	owner, supervisor, stale, operator, assignee, outsider := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	issuer, origin := "https://roster-http.invalid/"+uuid.NewString(), "http://127.0.0.1:58177"
	exec(`INSERT INTO supervisor.tenants(id,name) VALUES($1,'synthetic roster test'),($2,'other tenant')`, tenant, otherTenant)
	exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'roster site'),($3,$4,'other site')`, tenant, site, otherTenant, otherSite)
	exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'owner'),($1,$2,$3,$5,'supervisor'),($1,$2,$3,$6,'supervisor'),($1,$2,$3,$7,'operator'),($1,$2,$3,$8,'operator')`,
		tenant, site, issuer, owner, supervisor, stale, operator, assignee)
	exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'supervisor')`, otherTenant, otherSite, issuer, outsider)
	line := uuid.NewString()
	{
		tx, e := control.BeginTx(ctx, nil)
		if e != nil {
			t.Fatal(e)
		}
		source := uuid.NewString()
		payload := `{"entity_id":"` + line + `","type":"asset","asset_kind":"line","source_id":"` + source + `","source_revision":1}`
		for _, s := range []string{`SET LOCAL session_replication_role = replica`,
			`INSERT INTO supervisor.knowledge_entity_heads(tenant_id,site_id,id,current_revision) VALUES('` + tenant + `','` + site + `','` + line + `',1)`,
			`INSERT INTO supervisor.knowledge_entity_revisions(tenant_id,site_id,id,revision,entity_type,payload,source_id,source_revision,actor_issuer,actor_subject) VALUES('` + tenant + `','` + site + `','` + line + `',1,'asset','` + payload + `','` + source + `',1,'test','fixture')`} {
			if _, e = tx.ExecContext(ctx, s); e != nil {
				t.Fatalf("%s: %v", s, e)
			}
		}
		if e = tx.Commit(); e != nil {
			t.Fatal(e)
		}
	}
	type session struct{ token, csrf string }
	seed := func(subject string, authAge time.Duration) session {
		t.Helper()
		v := session{token: uuid.NewString(), csrf: uuid.NewString()}
		sum := sha256.Sum256([]byte(v.token))
		exec(`INSERT INTO supervisor.browser_sessions(token_hash,subject,issuer,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,$4,now(),now(),now()+interval '1 hour',now()-$5::interval)`,
			hex.EncodeToString(sum[:]), subject, issuer, v.csrf, strconv.Itoa(int(authAge.Seconds()))+" seconds")
		return v
	}
	ownerS, supervisorS, staleS, operatorS, outsiderS := seed(owner, 0), seed(supervisor, 0), seed(stale, 20*time.Minute), seed(operator, 0), seed(outsider, 0)
	auth, err := identity.New(ctx, identity.Config{Issuer: issuer, ClientID: "roster-test", ClientSecret: "synthetic-only", RedirectURI: origin + "/auth/callback"}, runtime, nil)
	if err != nil {
		t.Fatal(err)
	}
	handler := New(config.Config{Environment: "local", WebDir: t.TempDir()}, db, auth)
	call := func(actor session, method, path, key, revision, body string, csrf bool) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.AddCookie(&http.Cookie{Name: "supervisor", Value: actor.token})
		r.Header.Set("Origin", origin)
		if csrf {
			r.Header.Set("X-CSRF-Token", actor.csrf)
		}
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
	expect := func(name string, w *httptest.ResponseRecorder, status int, code string) {
		t.Helper()
		if w.Code != status || (code != "" && !strings.Contains(w.Body.String(), `"code":"`+code+`"`)) {
			t.Fatalf("%s: HTTP %d want %d %s: %s", name, w.Code, status, code, w.Body.String())
		}
	}
	decisions := "/v1/sites/" + site + "/maintenance-roster/decisions"
	body := func(subject, decision, reason string) string {
		return `{"line_id":"` + line + `","assignee_subject":"` + subject + `","decision":"` + decision + `","reason":"` + reason + `"}`
	}
	confirm := body(assignee, "confirm", "Qualified on the press line")

	for _, c := range []struct {
		name          string
		actor         session
		path, key     string
		revision, req string
		csrf          bool
		status        int
		code          string
	}{
		{"no CSRF token", supervisorS, decisions, uuid.NewString(), `"0"`, confirm, false, 403, "CSRF_REJECTED"},
		{"no If-Match", supervisorS, decisions, uuid.NewString(), "", confirm, true, 428, "REVISION_REQUIRED"},
		{"no Idempotency-Key", supervisorS, decisions, "", `"0"`, confirm, true, 422, "INVALID_REQUEST"},
		{"forged decider field", supervisorS, decisions, uuid.NewString(), `"0"`, strings.TrimSuffix(confirm, "}") + `,"decided_by_subject":"` + owner + `"}`, true, 422, "INVALID_ROSTER_DECISION"},
		{"forged tenant field", supervisorS, decisions, uuid.NewString(), `"0"`, strings.TrimSuffix(confirm, "}") + `,"tenant_id":"` + otherTenant + `"}`, true, 422, "INVALID_ROSTER_DECISION"},
		{"unknown decision", supervisorS, decisions, uuid.NewString(), `"0"`, body(assignee, "approve", "x"), true, 422, "INVALID_ROSTER_DECISION"},
		{"operator decides", operatorS, decisions, uuid.NewString(), `"0"`, confirm, true, 403, "PERMISSION_DENIED"},
		{"owner decides", ownerS, decisions, uuid.NewString(), `"0"`, confirm, true, 403, "PERMISSION_DENIED"},
		{"stale sign-in", staleS, decisions, uuid.NewString(), `"0"`, confirm, true, 428, "REAUTHENTICATION_REQUIRED"},
		{"other tenant's supervisor", outsiderS, decisions, uuid.NewString(), `"0"`, confirm, true, 404, "NOT_FOUND"},
		{"unknown line", supervisorS, decisions, uuid.NewString(), `"0"`, strings.Replace(confirm, line, uuid.NewString(), 1), true, 422, "LINE_UNAVAILABLE"},
		{"owner as assignee", supervisorS, decisions, uuid.NewString(), `"0"`, body(owner, "confirm", "x"), true, 422, "ASSIGNEE_NOT_ELIGIBLE"},
		{"withdraw absent entry", supervisorS, decisions, uuid.NewString(), `"0"`, body(assignee, "withdraw", "x"), true, 409, "ROSTER_UNCHANGED"},
		{"stale revision", supervisorS, decisions, uuid.NewString(), `"3"`, confirm, true, 412, "REVISION_CONFLICT"},
	} {
		expect(c.name, call(c.actor, http.MethodPost, c.path, c.key, c.revision, c.req, c.csrf), c.status, c.code)
	}
	var rows int
	if err = control.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.maintenance_roster_events`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("refused requests recorded %d events (err=%v)", rows, err)
	}

	key := uuid.NewString()
	w := call(supervisorS, http.MethodPost, decisions, key, `"0"`, confirm, true)
	expect("confirm", w, 200, "")
	var entry roster.Entry
	if err = json.Unmarshal(w.Body.Bytes(), &entry); err != nil || !entry.Confirmed || entry.Revision != 1 || entry.AssigneeSubject != assignee || w.Header().Get("ETag") != `"1"` {
		t.Fatalf("confirm=%+v etag=%s err=%v", entry, w.Header().Get("ETag"), err)
	}
	replay := call(supervisorS, http.MethodPost, decisions, key, `"0"`, confirm, true)
	expect("replay", replay, 200, "")
	if replay.Body.String() != w.Body.String() {
		t.Fatalf("replay body changed:\n%s\n%s", replay.Body.String(), w.Body.String())
	}
	expect("key reuse", call(supervisorS, http.MethodPost, decisions, key, `"0"`, body(assignee, "confirm", "other reason"), true), 409, "IDEMPOTENCY_CONFLICT")
	expect("confirm twice", call(supervisorS, http.MethodPost, decisions, uuid.NewString(), `"1"`, confirm, true), 409, "ROSTER_UNCHANGED")
	expect("withdraw", call(supervisorS, http.MethodPost, decisions, uuid.NewString(), `"1"`, body(assignee, "withdraw", "Moved line"), true), 200, "")

	list := "/v1/sites/" + site + "/maintenance-roster"
	got := call(ownerS, http.MethodGet, list+"?line_id="+line, "", "", "", false)
	expect("owner lists", got, 200, "")
	var page roster.Page
	if err = json.Unmarshal(got.Body.Bytes(), &page); err != nil || len(page.Entries) != 1 || page.Entries[0].Confirmed || page.Entries[0].Revision != 2 {
		t.Fatalf("list=%s err=%v", got.Body.String(), err)
	}
	expect("operator lists", call(operatorS, http.MethodGet, list, "", "", "", false), 403, "PERMISSION_DENIED")
	expect("other tenant lists", call(outsiderS, http.MethodGet, list, "", "", "", false), 404, "NOT_FOUND")
	expect("unknown query", call(ownerS, http.MethodGet, list+"?tenant_id="+otherTenant, "", "", "", false), 422, "INVALID_REQUEST")
	expect("limit over cap", call(ownerS, http.MethodGet, list+"?limit=101", "", "", "", false), 422, "INVALID_REQUEST")
	expect("bad line filter", call(ownerS, http.MethodGet, list+"?line_id=nope", "", "", "", false), 422, "INVALID_ROSTER_DECISION")
}
