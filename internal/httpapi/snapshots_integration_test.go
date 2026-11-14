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
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/identity"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/migrate"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/google/uuid"
)

func TestKnowledgeHTTPSnapshots(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	adminURL, runtimeURL := os.Getenv("SUPERVISOR_TEST_ADMIN_URL"), os.Getenv("SUPERVISOR_TEST_DATABASE_URL")
	if adminURL == "" || runtimeURL == "" {
		t.Fatal("actual disposable PostgreSQL required")
	}
	control, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	name := "snapshot_http_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = control.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, e := control.ExecContext(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)"); e != nil {
			t.Error(e)
		}
	}()
	connectURL := func(raw string) string {
		u, e := url.Parse(raw)
		if e != nil {
			t.Fatal(e)
		}
		u.Path = "/" + name
		return u.String()
	}
	admin, err := sql.Open("pgx", connectURL(adminURL))
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if err = migrate.Apply(ctx, admin); err != nil {
		t.Fatal(err)
	}
	db, err := store.New(connectURL(runtimeURL))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = db.VerifyRole(ctx); err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := admin.ExecContext(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	tenant, site, foreign := uuid.NewString(), uuid.NewString(), uuid.NewString()
	issuer, origin := "https://snapshot-http.invalid/"+uuid.NewString(), "http://127.0.0.1:58176"
	exec("INSERT INTO supervisor.tenants(id,name) VALUES($1,'snapshot synthetic')", tenant)
	exec("INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'snapshot synthetic'),($1,$3,'inaccessible')", tenant, site, foreign)
	type session struct{ token, csrf, hash, subject string }
	seed := func(role string) session {
		t.Helper()
		a := session{token: uuid.NewString(), csrf: uuid.NewString(), subject: uuid.NewString()}
		sum := sha256.Sum256([]byte(a.token))
		a.hash = hex.EncodeToString(sum[:])
		exec(`INSERT INTO supervisor.browser_sessions(token_hash,subject,issuer,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,$4,now(),now(),now()+interval '1 hour',now())`, a.hash, a.subject, issuer, a.csrf)
		exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,$5)`, tenant, site, issuer, a.subject, role)
		return a
	}
	owner, viewer := seed("owner"), seed("viewer")
	auth, err := identity.New(ctx, identity.Config{Issuer: issuer, ClientID: "snapshot", ClientSecret: "synthetic-only", RedirectURI: origin + "/auth/callback"}, db.DB, nil)
	if err != nil {
		t.Fatal(err)
	}
	handler := New(config.Config{Environment: "local", WebDir: t.TempDir()}, db, auth)
	path := "/v1/sites/" + site + "/process-snapshots"
	call := func(a session, method, p, body, key, match, csrf string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, p, strings.NewReader(body))
		if a.token != "" {
			r.AddCookie(&http.Cookie{Name: "supervisor", Value: a.token})
		}
		r.Header.Set("Origin", origin)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-CSRF-Token", csrf)
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
	status := func(w *httptest.ResponseRecorder, want int) {
		t.Helper()
		if w.Code != want {
			t.Fatalf("HTTP%d want%d: %s", w.Code, want, w.Body.String())
		}
	}
	status(call(session{}, "GET", path+"/"+uuid.NewString(), "", "", "", ""), 401)
	id, key := uuid.NewString(), uuid.NewString()
	body := `{"snapshot_id":"` + id + `","reason":"Supervisor validated captured knowledge"}`
	status(call(owner, "POST", path, body, key, `"0"`, "wrong"), 403)
	wantProblem(t, call(owner, "POST", path, body, key, "", owner.csrf), 428, "REVISION_REQUIRED")
	status(call(owner, "POST", path, body, key, `"00"`, owner.csrf), 422)
	status(call(owner, "POST", path, `{"snapshot_id":"`+id+`","reason":"valid","tenant_id":"`+tenant+`"}`, key, `"0"`, owner.csrf), 422)
	status(call(viewer, "POST", path, body, key, `"0"`, viewer.csrf), 403)
	wantProblem(t, call(owner, "POST", path, body, key, `"1"`, owner.csrf), 412, "REVISION_CONFLICT")
	exec(`UPDATE supervisor.browser_sessions SET auth_time=now()-interval '20 minutes' WHERE token_hash=$1`, owner.hash)
	wantProblem(t, call(owner, "POST", path, body, key, `"0"`, owner.csrf), 428, "REAUTHENTICATION_REQUIRED")
	exec(`UPDATE supervisor.browser_sessions SET auth_time=now() WHERE token_hash=$1`, owner.hash)
	created := call(owner, "POST", path, body, key, `"0"`, owner.csrf)
	status(created, 200)
	var snapshot struct {
		SnapshotID        string `json:"snapshot_id"`
		SHA256            string `json:"sha256"`
		InventoryRevision int64  `json:"inventory_revision"`
		Stale             bool   `json:"stale"`
		SchemaVersion     int    `json:"schema_version"`
	}
	if err = json.Unmarshal(created.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.SnapshotID != id || snapshot.SchemaVersion != 1 || snapshot.InventoryRevision != 0 || snapshot.Stale || len(snapshot.SHA256) != 64 {
		t.Fatalf("snapshot metadata %s", created.Body.String())
	}
	readPath := path + "/" + id
	read := call(viewer, "GET", readPath, "", "", "", "")
	status(read, 200)
	if read.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("snapshot must not be cached")
	}
	status(call(owner, "POST", path, body, key, `"0"`, owner.csrf), 200)
	wantProblem(t, call(owner, "POST", path, strings.Replace(body, "validated", "changed", 1), key, `"0"`, owner.csrf), 409, "IDEMPOTENCY_CONFLICT")
	wantProblem(t, call(owner, "POST", path, body, uuid.NewString(), `"0"`, owner.csrf), 409, "SNAPSHOT_CONFLICT")
	status(call(owner, "GET", "/v1/sites/"+foreign+"/process-snapshots/"+id, "", "", "", ""), 404)
	status(call(owner, "GET", path+"/not-a-uuid", "", "", "", ""), 422)
	// Source capture advances inventory even when no claims are yet approved.
	source := `{"source_id":"` + uuid.NewString() + `","kind":"manual","title":"New evidence","text":"Synthetic observed instruction"}`
	status(call(owner, "POST", "/v1/sites/"+site+"/sources", source, uuid.NewString(), `"0"`, owner.csrf), 200)
	replay := call(owner, "POST", path, body, key, `"0"`, owner.csrf)
	status(replay, 200)
	if err = json.Unmarshal(replay.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if !snapshot.Stale || snapshot.InventoryRevision != 0 {
		t.Fatalf("replay silently refreshed snapshot: %s", replay.Body.String())
	}
	exec(`UPDATE supervisor.browser_sessions SET auth_time=now()-interval '20 minutes' WHERE token_hash=$1`, owner.hash)
	wantProblem(t, call(owner, "POST", path, body, key, `"0"`, owner.csrf), 428, "REAUTHENTICATION_REQUIRED")
	status(call(owner, "GET", readPath, "", "", "", ""), 200)
	exec(`UPDATE supervisor.memberships SET active=false WHERE tenant_id=$1 AND site_id=$2 AND subject=$3`, tenant, site, viewer.subject)
	status(call(viewer, "GET", readPath, "", "", "", ""), 404)
	exec(`UPDATE supervisor.browser_sessions SET expires_at=now()-interval '1 second' WHERE token_hash=$1`, owner.hash)
	status(call(owner, "GET", readPath, "", "", "", ""), 401)
}
