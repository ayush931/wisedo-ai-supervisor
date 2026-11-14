//go:build integration

package identity

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/ratelimit"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	josejwt "github.com/go-jose/go-jose/v4/jwt"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestPersistentOIDCFailurePaths(t *testing.T) {
	dsn := os.Getenv("SUPERVISOR_TEST_DATABASE_URL")
	if dsn == "" {
		t.Fatal("BLOCKED: SUPERVISOR_TEST_DATABASE_URL requires migrated disposable PostgreSQL")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithHeader("kid", "fixture"))
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var issuer, nonce, challenge string
	var changes map[string]any
	exchanges := 0
	insecureAlgorithm := false
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]any{"issuer": issuer, "authorization_endpoint": issuer + "/authorize", "token_endpoint": issuer + "/token", "jwks_uri": issuer + "/keys", "id_token_signing_alg_values_supported": []string{"RS256"}})
		case "/keys":
			_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "fixture", Algorithm: "RS256", Use: "sig"}}})
		case "/token":
			exchanges++
			if err := r.ParseForm(); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			if base64.RawURLEncoding.EncodeToString(sum[:]) != challenge {
				t.Error("PKCE not bound to exchange")
				w.WriteHeader(400)
				return
			}
			claims := map[string]any{"iss": issuer, "aud": "supervisor", "sub": "go-test-operator", "iat": time.Now().Unix(), "exp": time.Now().Add(time.Minute).Unix(), "nonce": nonce}
			for key, value := range changes {
				claims[key] = value
			}
			tokenSigner := signer
			if insecureAlgorithm {
				tokenSigner, err = jose.NewSigner(jose.SigningKey{Algorithm: jose.HS256, Key: []byte("test-only-hmac-secret-with-32-bytes")}, (&jose.SignerOptions{}).WithHeader("kid", "fixture"))
				if err != nil {
					t.Error(err)
					w.WriteHeader(500)
					return
				}
			}
			token, err := josejwt.Signed(tokenSigner).Claims(claims).Serialize()
			if err != nil {
				t.Error(err)
				w.WriteHeader(500)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "discarded-fixture", "token_type": "Bearer", "id_token": token})
		default:
			w.WriteHeader(404)
		}
	}))
	defer provider.Close()
	mu.Lock()
	issuer = provider.URL
	mu.Unlock()
	mux := http.NewServeMux()
	app := httptest.NewServer(mux)
	defer app.Close()
	service, err := New(context.Background(), Config{Issuer: provider.URL, ClientID: "supervisor", ClientSecret: "test-secret", RedirectURI: app.URL + "/auth/callback"}, db, provider.Client())
	if err != nil {
		t.Fatal(err)
	}
	service.Register(mux)
	mux.HandleFunc("GET /me", func(w http.ResponseWriter, r *http.Request) {
		p, err := service.Authenticate(r.Context(), r)
		if err != nil {
			w.WriteHeader(Status(err))
			return
		}
		_ = json.NewEncoder(w).Encode(p)
	})
	jar, _ := cookiejar.New(nil)
	browser := &http.Client{Jar: jar, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	do := func(client *http.Client, method, path string, headers map[string]string) (int, http.Header, []byte) {
		t.Helper()
		r, _ := http.NewRequest(method, app.URL+path, nil)
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		res, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		return res.StatusCode, res.Header, body
	}
	login := func() string {
		t.Helper()
		status, headers, _ := do(browser, "GET", "/auth/login", nil)
		if status != 302 {
			t.Fatalf("login status %d", status)
		}
		u, err := url.Parse(headers.Get("Location"))
		if err != nil {
			t.Fatal(err)
		}
		q := u.Query()
		if q.Get("code_challenge_method") != "S256" {
			t.Fatal("missing PKCE")
		}
		mu.Lock()
		nonce = q.Get("nonce")
		challenge = q.Get("code_challenge")
		mu.Unlock()
		return "/auth/callback?code=fixture&state=" + url.QueryEscape(q.Get("state"))
	}
	callback := login()
	attacker := &http.Client{CheckRedirect: browser.CheckRedirect}
	if status, _, _ := do(attacker, "GET", callback, nil); status != 401 {
		t.Fatalf("foreign browser %d", status)
	}
	if status, headers, _ := do(browser, "GET", callback, nil); status != 302 || !strings.Contains(headers.Get("Set-Cookie"), "HttpOnly") {
		t.Fatalf("callback %d", status)
	}
	if status, _, _ := do(browser, "GET", callback, nil); status != 401 {
		t.Fatalf("replay %d", status)
	}
	mu.Lock()
	exchangeCount := exchanges
	mu.Unlock()
	if exchangeCount != 1 {
		t.Fatal("replayed exchange reached provider")
	}
	status, _, body := do(browser, "GET", "/me", nil)
	if status != 200 {
		t.Fatalf("me %d", status)
	}
	var principal Principal
	if err = json.Unmarshal(body, &principal); err != nil {
		t.Fatal(err)
	}
	if principal.Subject != "go-test-operator" {
		t.Fatal("wrong subject")
	}
	if status, _, _ := do(browser, "POST", "/auth/logout", nil); status != 403 {
		t.Fatalf("missing CSRF %d", status)
	}
	if status, _, _ := do(browser, "POST", "/auth/logout", map[string]string{"Origin": "http://evil.example", "X-CSRF-Token": principal.CSRFToken}); status != 403 {
		t.Fatalf("foreign origin %d", status)
	}
	appURL, _ := url.Parse(app.URL)
	savedCookies := jar.Cookies(appURL)
	if status, _, _ := do(browser, "POST", "/auth/logout", map[string]string{"Origin": app.URL, "X-CSRF-Token": principal.CSRFToken}); status != 204 {
		t.Fatalf("logout %d", status)
	}
	jar.SetCookies(appURL, savedCookies)
	if status, _, _ := do(browser, "GET", "/me", nil); status != 401 {
		t.Fatalf("revoked cookie %d", status)
	}
	for _, change := range []map[string]any{{"nonce": "wrong"}, {"iss": "https://evil.example"}, {"aud": "other"}, {"exp": time.Now().Add(-time.Minute).Unix()}, {"iat": time.Now().Add(time.Hour).Unix()}, {"sub": ""}, {"azp": "other"}} {
		mu.Lock()
		changes = change
		mu.Unlock()
		callback = login()
		if status, _, _ := do(browser, "GET", callback, nil); status != 401 {
			t.Fatalf("invalid claims %v accepted status %d", change, status)
		}
	}
	mu.Lock()
	changes = nil
	insecureAlgorithm = true
	mu.Unlock()
	callback = login()
	if status, _, _ := do(browser, "GET", callback, nil); status != 401 {
		t.Fatalf("HS256 accepted: %d", status)
	}
	mu.Lock()
	insecureAlgorithm = false
	mu.Unlock()
	callback = login()
	u, _ := url.Parse(callback)
	if _, err = db.Exec("UPDATE supervisor.oidc_logins SET expires_at=now()-interval '1 second' WHERE state_hash=$1", hash(u.Query().Get("state"))); err != nil {
		t.Fatal(err)
	}
	if status, _, _ := do(browser, "GET", callback, nil); status != 401 {
		t.Fatalf("expired state %d", status)
	}
	for _, query := range []string{"UPDATE supervisor.browser_sessions SET last_seen_at=now()-interval '31 minutes' WHERE subject='go-test-operator' AND issuer=$1", "UPDATE supervisor.browser_sessions SET expires_at=now()-interval '1 second' WHERE subject='go-test-operator' AND issuer=$1"} {
		callback = login()
		if status, _, _ := do(browser, "GET", callback, nil); status != 302 {
			t.Fatalf("fresh login %d", status)
		}
		if _, err = db.Exec(query, provider.URL); err != nil {
			t.Fatal(err)
		}
		if status, _, _ := do(browser, "GET", "/me", nil); status != 401 {
			t.Fatalf("expired session %d", status)
		}
	}
	// Ordinary login without auth_time is usable, but never recent proof.
	readPrincipal := func() Principal {
		t.Helper()
		status, _, body := do(browser, "GET", "/me", nil)
		if status != 200 {
			t.Fatalf("me %d", status)
		}
		var p Principal
		if e := json.Unmarshal(body, &p); e != nil {
			t.Fatal(e)
		}
		if strings.Contains(string(body), "SessionHash") {
			t.Fatal("session hash serialized")
		}
		return p
	}
	freshOrdinary := func() Principal {
		t.Helper()
		mu.Lock()
		changes = nil
		mu.Unlock()
		callback = login()
		if status, _, _ := do(browser, "GET", callback, nil); status != 302 {
			t.Fatalf("ordinary login %d", status)
		}
		p := readPrincipal()
		if !p.AuthenticatedAt.IsZero() || !errors.Is(service.RequireRecent(p), ErrReauthenticationRequired) {
			t.Fatal("ordinary login time became recent proof")
		}
		return p
	}
	currentCookie := func() string {
		for _, cookie := range jar.Cookies(appURL) {
			if cookie.Name == service.cookie {
				return cookie.Value
			}
		}
		t.Fatal("session cookie missing")
		return ""
	}
	beginReauth := func(p Principal) string {
		t.Helper()
		status, _, body := do(browser, "POST", "/auth/reauthenticate", map[string]string{"Origin": app.URL, "X-CSRF-Token": p.CSRFToken})
		if status != 200 {
			t.Fatalf("reauthenticate %d", status)
		}
		var result map[string]string
		if e := json.Unmarshal(body, &result); e != nil {
			t.Fatal(e)
		}
		u, e := url.Parse(result["authorization_url"])
		if e != nil {
			t.Fatal(e)
		}
		q := u.Query()
		if q.Get("prompt") != "login" || q.Get("max_age") != "0" || q.Get("code_challenge_method") != "S256" {
			t.Fatal("reauth proof request missing")
		}
		mu.Lock()
		nonce = q.Get("nonce")
		challenge = q.Get("code_challenge")
		mu.Unlock()
		return "/auth/callback?code=reauth&state=" + url.QueryEscape(q.Get("state"))
	}
	principal = freshOrdinary()
	if status, _, _ := do(browser, "POST", "/auth/reauthenticate", nil); status != 403 {
		t.Fatalf("reauth missingcsrf %d", status)
	}
	originalCookie := currentCookie()
	var originalCreated, originalExpires time.Time
	if err = db.QueryRow("SELECT created_at,expires_at FROM supervisor.browser_sessions WHERE token_hash=$1", hash(originalCookie)).Scan(&originalCreated, &originalExpires); err != nil {
		t.Fatal(err)
	}
	callback = beginReauth(principal)
	// Even copying the flow cookie cannot redeem without the original session cookie.
	attackerJar, _ := cookiejar.New(nil)
	for _, c := range jar.Cookies(appURL) {
		if c.Name == service.loginCookie {
			attackerJar.SetCookies(appURL, []*http.Cookie{c})
		}
	}
	attackBrowser := &http.Client{Jar: attackerJar, CheckRedirect: browser.CheckRedirect}
	if status, _, _ := do(attackBrowser, "GET", callback, nil); status != 401 {
		t.Fatalf("crossbrowser reauth %d", status)
	}
	mu.Lock()
	changes = map[string]any{"auth_time": time.Now().Unix()}
	mu.Unlock()
	if status, _, _ := do(browser, "GET", callback, nil); status != 302 {
		t.Fatalf("valid reauth %d", status)
	}
	recent := readPrincipal()
	if err = service.RequireRecent(recent); err != nil {
		t.Fatal(err)
	}
	if currentCookie() == originalCookie || recent.CSRFToken == principal.CSRFToken {
		t.Fatal("reauth did not rotate credentials")
	}
	var created, expires time.Time
	if err = db.QueryRow("SELECT created_at,expires_at FROM supervisor.browser_sessions WHERE token_hash=$1", hash(currentCookie())).Scan(&created, &expires); err != nil {
		t.Fatal(err)
	}
	if !created.Equal(originalCreated) || !expires.Equal(originalExpires) {
		t.Fatal("reauth extended absolute lifetime")
	}
	var remaining int
	if err = db.QueryRow("SELECT count(*) FROM supervisor.browser_sessions WHERE token_hash=$1", hash(originalCookie)).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("old session remains %d %v", remaining, err)
	}
	if status, _, _ := do(browser, "GET", callback, nil); status != 401 {
		t.Fatalf("reauth replay %d", status)
	}
	for _, change := range []map[string]any{nil, {"auth_time": nil}, {"auth_time": "123"}, {"auth_time": 1.5}, {"auth_time": time.Now().Add(time.Minute).Unix()}, {"auth_time": time.Now().Add(-16 * time.Minute).Unix()}, {"auth_time": time.Now().Add(-5 * time.Minute).Unix()}, {"auth_time": time.Now().Unix(), "sub": "switched-account"}} {
		principal = freshOrdinary()
		callback = beginReauth(principal)
		mu.Lock()
		changes = change
		mu.Unlock()
		if status, _, _ := do(browser, "GET", callback, nil); status != 401 {
			t.Fatalf("invalid reauth claims %v status %d", change, status)
		}
		if p := readPrincipal(); p.Subject != "go-test-operator" || !p.AuthenticatedAt.IsZero() {
			t.Fatal("failed reauth altered original identity")
		}
	}
	for _, query := range []string{"DELETE FROM supervisor.browser_sessions WHERE token_hash=$1", "UPDATE supervisor.browser_sessions SET expires_at=now()-interval '1 second' WHERE token_hash=$1", "UPDATE supervisor.browser_sessions SET last_seen_at=now()-interval '31 minutes' WHERE token_hash=$1"} {
		principal = freshOrdinary()
		callback = beginReauth(principal)
		if _, err = db.Exec(query, hash(currentCookie())); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		changes = map[string]any{"auth_time": time.Now().Unix()}
		mu.Unlock()
		if status, _, _ := do(browser, "GET", callback, nil); status != 401 {
			t.Fatalf("revoked/expired original session reauth %d", status)
		}
	}
	// Ordinary provider login may retain signed auth_time without substituting iat.
	mu.Lock()
	changes = map[string]any{"auth_time": time.Now().Add(-time.Minute).Unix()}
	mu.Unlock()
	callback = login()
	if status, _, _ := do(browser, "GET", callback, nil); status != 302 {
		t.Fatalf("ordinary auth_time %d", status)
	}
	if err = service.RequireRecent(readPrincipal()); err != nil {
		t.Fatal(err)
	}

}

// WIS-290: login stores the display name from the verified ID token only (never the token
// response envelope or the callback URL) for every tenant the principal is an active member
// of, updates it on the next login, falls back to preferred_username and clears to NULL.
func TestLoginStoresVerifiedDisplayNamePerTenant(t *testing.T) {
	dsn, adminURL := os.Getenv("SUPERVISOR_TEST_DATABASE_URL"), os.Getenv("SUPERVISOR_TEST_ADMIN_URL")
	if dsn == "" || adminURL == "" {
		t.Fatal("BLOCKED: SUPERVISOR_TEST_DATABASE_URL and SUPERVISOR_TEST_ADMIN_URL require migrated disposable PostgreSQL")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	admin, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithHeader("kid", "fixture"))
	if err != nil {
		t.Fatal(err)
	}
	subject := "go-test-named-" + uuid.NewString()
	var mu sync.Mutex
	var issuer, nonce string
	var profile map[string]any
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]any{"issuer": issuer, "authorization_endpoint": issuer + "/authorize", "token_endpoint": issuer + "/token", "jwks_uri": issuer + "/keys", "id_token_signing_alg_values_supported": []string{"RS256"}})
		case "/keys":
			_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "fixture", Algorithm: "RS256", Use: "sig"}}})
		case "/token":
			claims := map[string]any{"iss": issuer, "aud": "supervisor", "sub": subject, "iat": time.Now().Unix(), "exp": time.Now().Add(time.Minute).Unix(), "nonce": nonce}
			for k, v := range profile {
				claims[k] = v
			}
			token, err := josejwt.Signed(signer).Claims(claims).Serialize()
			if err != nil {
				w.WriteHeader(500)
				return
			}
			// Unverified envelope members must never become the display name.
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "discarded-fixture", "token_type": "Bearer", "id_token": token, "name": "Envelope Decoy", "preferred_username": "envelope-decoy"})
		default:
			w.WriteHeader(404)
		}
	}))
	defer provider.Close()
	mu.Lock()
	issuer = provider.URL
	mu.Unlock()

	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := admin.Exec(q, args...); e != nil {
			t.Fatal(e)
		}
	}
	tenants := []string{uuid.NewString(), uuid.NewString(), uuid.NewString()} // active, active, revoked
	defer func() {
		for _, tenant := range tenants {
			for _, q := range []string{"DELETE FROM supervisor.member_profiles WHERE tenant_id=$1", "DELETE FROM supervisor.memberships WHERE tenant_id=$1", "DELETE FROM supervisor.sites WHERE tenant_id=$1", "DELETE FROM supervisor.tenants WHERE id=$1"} {
				_, _ = admin.Exec(q, tenant)
			}
		}
		_, _ = admin.Exec("DELETE FROM supervisor.browser_sessions WHERE subject=$1", subject)
	}()
	for i, tenant := range tenants {
		site := uuid.NewString()
		exec("INSERT INTO supervisor.tenants(id,name) VALUES($1,'synthetic display name')", tenant)
		exec("INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'synthetic site')", tenant, site)
		exec("INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role,active) VALUES($1,$2,$3,$4,'operator',$5)", tenant, site, provider.URL, subject, i < 2)
	}

	mux := http.NewServeMux()
	app := httptest.NewServer(mux)
	defer app.Close()
	service, err := New(context.Background(), Config{Issuer: provider.URL, ClientID: "supervisor", ClientSecret: "test-secret", RedirectURI: app.URL + "/auth/callback"}, db, provider.Client())
	if err != nil {
		t.Fatal(err)
	}
	service.Register(mux)
	login := func(claims map[string]any) {
		t.Helper()
		mu.Lock()
		profile = claims
		mu.Unlock()
		jar, _ := cookiejar.New(nil)
		browser := &http.Client{Jar: jar, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
		res, err := browser.Get(app.URL + "/auth/login")
		if err != nil || res.StatusCode != 302 {
			t.Fatalf("login: %v", err)
		}
		res.Body.Close()
		u, _ := url.Parse(res.Header.Get("Location"))
		mu.Lock()
		nonce = u.Query().Get("nonce")
		mu.Unlock()
		if !strings.Contains(u.Query().Get("scope"), "profile") {
			t.Fatalf("authorization request scope %q lacks profile", u.Query().Get("scope"))
		}
		res, err = browser.Get(app.URL + "/auth/callback?code=fixture&name=Query+Decoy&state=" + url.QueryEscape(u.Query().Get("state")))
		if err != nil || res.StatusCode != 302 {
			t.Fatalf("callback: %v %v", err, res)
		}
		res.Body.Close()
	}
	stored := func() []string {
		t.Helper()
		out := []string{}
		for _, tenant := range tenants {
			var name sql.NullString
			err := admin.QueryRow("SELECT display_name FROM supervisor.member_profiles WHERE tenant_id=$1 AND issuer=$2 AND subject=$3", tenant, provider.URL, subject).Scan(&name)
			switch {
			case errors.Is(err, sql.ErrNoRows):
				out = append(out, "<no row>")
			case err != nil:
				t.Fatal(err)
			case !name.Valid:
				out = append(out, "<null>")
			default:
				out = append(out, name.String)
			}
		}
		return out
	}
	for _, c := range []struct {
		claims map[string]any
		want   string
	}{
		{map[string]any{"name": "Shift Lead", "preferred_username": "shift-lead"}, "Shift Lead"},
		{map[string]any{"name": "Shift\u0000Lead", "preferred_username": "shift-lead"}, "shift-lead"},
		{map[string]any{"name": strings.Repeat("x", 129), "preferred_username": "{{shift}}"}, "<null>"},
		{map[string]any{"name": "  Shift Lead Two  "}, "Shift Lead Two"},
		{map[string]any{}, "<null>"},
	} {
		login(c.claims)
		if got := stored(); got[0] != c.want || got[1] != c.want || got[2] != "<no row>" {
			t.Fatalf("claims %v stored %v, want %q in both active tenants and no row in the revoked one", c.claims, got, c.want)
		}
	}

	// Names are cosmetic: when the profile write fails (here the runtime role loses INSERT, as
	// before 043 is applied), the login still creates its session and writes no profile row.
	exec("DELETE FROM supervisor.member_profiles WHERE issuer=$1 AND subject=$2", provider.URL, subject)
	exec("REVOKE INSERT ON supervisor.member_profiles FROM supervisor_runtime")
	defer exec("GRANT INSERT ON supervisor.member_profiles TO supervisor_runtime")
	var before, after int
	if err = admin.QueryRow("SELECT count(*) FROM supervisor.browser_sessions WHERE subject=$1", subject).Scan(&before); err != nil {
		t.Fatal(err)
	}
	login(map[string]any{"name": "Shift Lead"}) // fails the test unless the callback answers 302
	if err = admin.QueryRow("SELECT count(*) FROM supervisor.browser_sessions WHERE subject=$1", subject).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before+1 {
		t.Fatalf("login with an unavailable profile table created %d sessions, want 1", after-before)
	}
	if got := stored(); got[0] != "<no row>" || got[1] != "<no row>" || got[2] != "<no row>" {
		t.Fatalf("failed profile write still stored %v", got)
	}
}

// WIS-699: /auth/login and /auth/callback are rate limited per client. A request over the budget
// is refused with 429 before the handler runs: no provider fetch and no login-state row. Control:
// the requests within the budget each write their row.
func TestAuthRoutesAreRateLimitedBeforeAnyWrite(t *testing.T) {
	dsn := os.Getenv("SUPERVISOR_TEST_DATABASE_URL")
	if dsn == "" {
		t.Fatal("BLOCKED: SUPERVISOR_TEST_DATABASE_URL requires migrated disposable PostgreSQL")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var issuer string
	var discoveries int
	var mu sync.Mutex
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path != "/.well-known/openid-configuration" {
			w.WriteHeader(404)
			return
		}
		discoveries++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": issuer, "authorization_endpoint": issuer + "/authorize", "token_endpoint": issuer + "/token", "jwks_uri": issuer + "/keys", "id_token_signing_alg_values_supported": []string{"RS256"}})
	}))
	defer provider.Close()
	mu.Lock()
	issuer = provider.URL
	mu.Unlock()
	mux := http.NewServeMux()
	app := httptest.NewServer(mux)
	defer app.Close()
	limiter := ratelimit.New(1.0/3600, 2, 100, func() time.Time { return time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC) })
	key := ratelimit.ClientKey(nil)
	service, err := New(context.Background(), Config{Issuer: provider.URL, ClientID: "supervisor", ClientSecret: "test-secret", RedirectURI: app.URL + "/auth/callback",
		Limit: func(next http.Handler) http.Handler { return limiter.Wrap(key, next) }}, db, provider.Client())
	if err != nil {
		t.Fatal(err)
	}
	service.Register(mux)
	client := &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	logins := func() int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(context.Background(), `SELECT count(*) FROM supervisor.oidc_logins`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	get := func(path string) *http.Response {
		t.Helper()
		res, err := client.Get(app.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
		return res
	}
	before := logins()
	for i := range 2 {
		if res := get("/auth/login"); res.StatusCode != 302 {
			t.Fatalf("login %d within the budget: %d", i+1, res.StatusCode)
		}
	}
	if got := logins(); got != before+2 {
		t.Fatalf("logins within the budget wrote %d rows, want 2", got-before)
	}
	mu.Lock()
	fetched := discoveries
	mu.Unlock()
	refused := get("/auth/login")
	if refused.StatusCode != 429 || refused.Header.Get("Retry-After") == "" || refused.Header.Get("Content-Type") != "application/problem+json" {
		t.Fatalf("over the budget: %d %v", refused.StatusCode, refused.Header)
	}
	if res := get("/auth/callback?state=x&code=y"); res.StatusCode != 429 {
		t.Fatalf("callback over the budget: %d", res.StatusCode)
	}
	mu.Lock()
	after := discoveries
	mu.Unlock()
	if got := logins(); got != before+2 || after != fetched {
		t.Fatalf("a refused request wrote %d login rows and made %d provider fetches", got-before-2, after-fetched)
	}
}
