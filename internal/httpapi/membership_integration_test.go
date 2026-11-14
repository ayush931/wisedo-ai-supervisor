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
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/identity"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/google/uuid"
)

func TestMembershipHTTPAuthorityRevisionsAndRevocation(t *testing.T) {
	adminURL, runtimeURL := os.Getenv("SUPERVISOR_TEST_ADMIN_URL"), os.Getenv("SUPERVISOR_TEST_DATABASE_URL")
	if adminURL == "" || runtimeURL == "" {
		t.Fatal("BLOCKED: actual disposable PostgreSQL required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	db, err := store.New(runtimeURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tenant, site, owner, target := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	defer func() {
		for _, table := range []string{"membership_changes", "memberships", "sites", "tenants"} {
			key := "tenant_id"
			if table == "tenants" {
				key = "id"
			}
			if _, e := admin.ExecContext(context.Background(), "DELETE FROM supervisor."+table+" WHERE "+key+"=$1", tenant); e != nil {
				t.Error(e)
			}
		}
	}()
	issuer, origin := "https://membership-http.invalid/realm", "http://127.0.0.1:58176"
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := admin.ExecContext(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	exec("INSERT INTO supervisor.tenants(id,name) VALUES($1,'synthetic membership HTTP')", tenant)
	exec("INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'synthetic membership site')", tenant, site)
	exec("INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'owner')", tenant, site, issuer, owner)
	token, csrf := uuid.NewString(), uuid.NewString()
	sum := sha256.Sum256([]byte(token))
	hash := hex.EncodeToString(sum[:])
	exec(`INSERT INTO supervisor.browser_sessions(token_hash,subject,issuer,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,$4,now(),now(),now()+interval '1 hour',NULL)`, hash, owner, issuer, csrf)
	defer func() {
		_, _ = admin.ExecContext(context.Background(), "DELETE FROM supervisor.browser_sessions WHERE token_hash=$1", hash)
	}()
	auth, err := identity.New(ctx, identity.Config{Issuer: issuer, ClientID: "test", ClientSecret: "synthetic-only", RedirectURI: origin + "/auth/callback"}, db.DB, nil)
	if err != nil {
		t.Fatal(err)
	}
	h := New(config.Config{Environment: "local", WebDir: t.TempDir()}, db, auth)
	call := func(method, path, key, revision, csrfValue, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.AddCookie(&http.Cookie{Name: "supervisor", Value: token})
		r.Header.Set("Origin", origin)
		r.Header.Set("X-CSRF-Token", csrfValue)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Idempotency-Key", key)
		r.Header.Set("If-Match", revision)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	path := "/v1/sites/" + site + "/membership-changes"
	body, _ := json.Marshal(map[string]any{"issuer": issuer, "subject": target, "role": "operator", "active": true})
	key := uuid.NewString()
	assertStatus := func(w *httptest.ResponseRecorder, want int) {
		t.Helper()
		if w.Code != want {
			t.Fatalf("HTTP%d want%d: %s", w.Code, want, w.Body.String())
		}
	}
	wantProblem(t, call("POST", path, key, `"0"`, csrf, string(body)), 428, "REAUTHENTICATION_REQUIRED")
	exec("UPDATE supervisor.browser_sessions SET auth_time=now() WHERE token_hash=$1", hash)
	assertStatus(call("POST", path, key, `"0"`, "", string(body)), 403)
	wantProblem(t, call("POST", path, key, "", csrf, string(body)), 428, "REVISION_REQUIRED")
	assertStatus(call("POST", path, key, `W/"0"`, csrf, string(body)), 422)
	assertStatus(call("POST", path, key, `"0"`, csrf, strings.TrimSuffix(string(body), "}")+`,"tenant_id":"forged"}`), 422)
	w := call("POST", path, key, `"0"`, csrf, string(body))
	assertStatus(w, 200)
	if w.Header().Get("ETag") != `"1"` {
		t.Fatalf("revision ETag %s", w.Header().Get("ETag"))
	}
	assertStatus(call("POST", path, key, `"0"`, csrf, string(body)), 200)
	wantProblem(t, call("POST", path, uuid.NewString(), `"0"`, csrf, string(body)), 412, "REVISION_CONFLICT")
	changed := strings.Replace(string(body), `"operator"`, `"supervisor"`, 1)
	wantProblem(t, call("POST", path, key, `"1"`, csrf, changed), 409, "IDEMPOTENCY_CONFLICT")
	assertStatus(call("GET", "/v1/sites/"+site+"/memberships?limit=101", "", "", "", ""), 422)
	assertStatus(call("GET", "/v1/sites/"+site+"/memberships", "", "", "", ""), 200)
	// Replay remains a privileged operation: old proof or revoked ownership cannot
	// retrieve an earlier successful authority mutation.
	exec("UPDATE supervisor.browser_sessions SET auth_time=now()-interval '16 minutes' WHERE token_hash=$1", hash)
	wantProblem(t, call("POST", path, key, `"0"`, csrf, string(body)), 428, "REAUTHENTICATION_REQUIRED")
	exec("UPDATE supervisor.browser_sessions SET auth_time=now() WHERE token_hash=$1", hash)
	exec("UPDATE supervisor.memberships SET active=false WHERE tenant_id=$1 AND site_id=$2 AND issuer=$3 AND subject=$4", tenant, site, issuer, owner)
	assertStatus(call("POST", path, key, `"0"`, csrf, string(body)), 404)
	assertStatus(call("GET", "/v1/sites/"+uuid.NewString()+"/memberships", "", "", "", ""), 404)
	exec("UPDATE supervisor.memberships SET active=true WHERE tenant_id=$1 AND site_id=$2 AND issuer=$3 AND subject=$4", tenant, site, issuer, owner)
	assertStatus(call("GET", "/v1/sites/"+site+"/memberships", "", "", "", ""), 200)
	exec("UPDATE supervisor.tenants SET deactivated_at=now() WHERE id=$1", tenant)
	wantProblem(t, call("GET", "/v1/sites/"+site+"/memberships", "", "", "", ""), 404, "NOT_FOUND")
}
