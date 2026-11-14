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

func TestEvidenceHTTPCurrentness(t *testing.T) {
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
	name := "currentness_http_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	issuer, origin := "https://discovery-http.invalid/"+uuid.NewString(), "http://127.0.0.1:58176"
	exec("INSERT INTO supervisor.tenants(id,name) VALUES($1,'discovery synthetic')", tenant)
	exec("INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'discovery synthetic'),($1,$3,'inaccessible')", tenant, site, foreign)
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
	owner, viewer, supervisor := seed("owner"), seed("viewer"), seed("supervisor")
	auth, err := identity.New(ctx, identity.Config{Issuer: issuer, ClientID: "snapshot", ClientSecret: "synthetic-only", RedirectURI: origin + "/auth/callback"}, db.DB, nil)
	if err != nil {
		t.Fatal(err)
	}
	handler := New(config.Config{Environment: "local", WebDir: t.TempDir()}, db, auth)
	path := "/v1/sites/" + site + "/sources/" + uuid.NewString() + "/currentness"
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
	status(call(session{}, "GET", path, "", "", "", ""), 401)
	sourceID := uuid.NewString()
	base := "/v1/sites/" + site + "/sources/" + sourceID
	path = base + "/currentness"
	post := base + "/currentness-decisions"
	source := `{"source_id":"` + sourceID + `","kind":"document","title":"Synthetic SOP","text":"Synthetic owner reviewed process"}`
	status(call(owner, "POST", "/v1/sites/"+site+"/sources", source, uuid.NewString(), `"0"`, owner.csrf), 200)
	designation := call(owner, "POST", base+"/authority-decisions", `{"source_revision":1,"expected_source_state_revision":1,"verdict":"designate","reason":"Owner designated SOP"}`, uuid.NewString(), `"0"`, owner.csrf)
	status(designation, 200)
	var authority struct {
		DecisionID string `json:"decision_id"`
	}
	if err = json.Unmarshal(designation.Body.Bytes(), &authority); err != nil || authority.DecisionID == "" {
		t.Fatalf("authority=%s %v", designation.Body, err)
	}
	body := `{"source_revision":1,"expected_source_state_revision":1,"authority_decision_id":"` + authority.DecisionID + `","authority_revision":1,"verdict":"confirm_current","reason":"Owner confirms document is current"}`
	key := uuid.NewString()
	status(call(viewer, "POST", post, body, key, `"0"`, viewer.csrf), 403)
	status(call(supervisor, "POST", post, body, key, `"0"`, supervisor.csrf), 403)
	status(call(owner, "POST", post, body, key, `"0"`, "wrong"), 403)
	wantProblem(t, call(owner, "POST", post, body, key, "", owner.csrf), 428, "REVISION_REQUIRED")
	status(call(owner, "POST", post, body, key, `"00"`, owner.csrf), 422)
	wantProblem(t, call(owner, "POST", post, body, key, `"1"`, owner.csrf), 412, "REVISION_CONFLICT")
	for _, extra := range []string{`"verified_at":"2026-09-27T00:00:00Z"`, `"actor":"forged"`, `"verification_current":true`, `"eligible":true`} {
		forged := strings.TrimSuffix(body, "}") + "," + extra + "}"
		status(call(owner, "POST", post, forged, key, `"0"`, owner.csrf), 422)
	}
	exec(`UPDATE supervisor.browser_sessions SET auth_time=now()-interval '20 minutes' WHERE token_hash=$1`, owner.hash)
	wantProblem(t, call(owner, "POST", post, body, key, `"0"`, owner.csrf), 428, "REAUTHENTICATION_REQUIRED")
	exec(`UPDATE supervisor.browser_sessions SET auth_time=now() WHERE token_hash=$1`, owner.hash)
	created := call(owner, "POST", post, body, key, `"0"`, owner.csrf)
	status(created, 200)
	type verification struct {
		VerificationID      string     `json:"verification_id"`
		Revision            int64      `json:"revision"`
		VerifiedAt          *time.Time `json:"verified_at"`
		VerificationCurrent bool       `json:"verification_current"`
		SourceSHA256        string     `json:"source_sha256"`
	}
	decode := func(w *httptest.ResponseRecorder) verification {
		t.Helper()
		var v verification
		if e := json.Unmarshal(w.Body.Bytes(), &v); e != nil {
			t.Fatal(e)
		}
		return v
	}
	confirmed := decode(created)
	if confirmed.VerificationID == "" || confirmed.VerifiedAt == nil || !confirmed.VerificationCurrent || confirmed.Revision != 1 || len(confirmed.SourceSHA256) != 64 {
		t.Fatalf("confirmation=%s", created.Body)
	}
	if created.Header().Get("ETag") != `"1"` {
		t.Fatalf("etag=%s", created.Header().Get("ETag"))
	}
	read := call(viewer, "GET", path+"?revision=1", "", "", "", "")
	status(read, 200)
	if v := decode(read); v.VerifiedAt == nil || !v.VerifiedAt.Equal(*confirmed.VerifiedAt) {
		t.Fatal("read renewed time")
	}
	replay := call(owner, "POST", post, body, key, `"0"`, owner.csrf)
	status(replay, 200)
	if v := decode(replay); v.VerificationID != confirmed.VerificationID || v.VerifiedAt == nil || !v.VerifiedAt.Equal(*confirmed.VerifiedAt) {
		t.Fatal("replay renewed confirmation")
	}
	wantProblem(t, call(owner, "POST", post, strings.Replace(body, "Owner confirms document is current", "Changed reason", 1), key, `"0"`, owner.csrf), 409, "IDEMPOTENCY_CONFLICT")
	for _, query := range []string{"?unknown=1", "?revision=1&revision=1", "?revision=01", "?revision=-1", "?revision=1.0", "?revision=", "?revision=9223372036854775808"} {
		status(call(viewer, "GET", path+query, "", "", "", ""), 422)
	}
	status(call(viewer, "GET", strings.Replace(path, site, foreign, 1), "", "", "", ""), 404)
	exec(`UPDATE supervisor.browser_sessions SET auth_time=now()-interval '20 minutes' WHERE token_hash=$1`, owner.hash)
	wantProblem(t, call(owner, "POST", post, body, key, `"0"`, owner.csrf), 428, "REAUTHENTICATION_REQUIRED")
	exec(`UPDATE supervisor.browser_sessions SET auth_time=now() WHERE token_hash=$1`, owner.hash)
	exec(`UPDATE supervisor.memberships SET role='supervisor' WHERE tenant_id=$1 AND site_id=$2 AND subject=$3`, tenant, site, owner.subject)
	status(call(owner, "POST", post, body, key, `"0"`, owner.csrf), 403)
	exec(`UPDATE supervisor.memberships SET role='owner' WHERE tenant_id=$1 AND site_id=$2 AND subject=$3`, tenant, site, owner.subject)
	withdraw := strings.Replace(strings.Replace(body, "confirm_current", "withdraw", 1), "Owner confirms document is current", "Withdraw confirmation", 1)
	withdrawn := call(owner, "POST", post, withdraw, uuid.NewString(), `"1"`, owner.csrf)
	status(withdrawn, 200)
	if v := decode(withdrawn); v.VerifiedAt != nil || v.VerificationCurrent || v.Revision != 2 {
		t.Fatalf("withdraw=%s", withdrawn.Body)
	}
	historical := call(viewer, "GET", path+"?revision=1", "", "", "", "")
	status(historical, 200)
	if v := decode(historical); v.VerificationCurrent || v.VerifiedAt == nil || !v.VerifiedAt.Equal(*confirmed.VerifiedAt) {
		t.Fatal("history mutated or resurrected")
	}
	replay = call(owner, "POST", post, body, key, `"0"`, owner.csrf)
	status(replay, 200)
	if decode(replay).VerificationCurrent {
		t.Fatal("old replay resurrected currentness")
	}
	status(call(owner, "POST", base+"/revocations", `{"reason":"Withdraw source access"}`, uuid.NewString(), `"1"`, owner.csrf), 200)
	status(call(viewer, "GET", path+"?revision=1", "", "", "", ""), 404)
	status(call(owner, "POST", post, body, key, `"0"`, owner.csrf), 404)
}
