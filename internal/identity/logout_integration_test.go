//go:build integration

package identity

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	josejwt "github.com/go-jose/go-jose/v4/jwt"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// WIS-713: sign out must end the provider sign-in, and the next sign-in must show the provider's
// form. Before the fix, /auth/login sent no prompt (Keycloak signed the previous account straight
// back in) and /auth/logout only deleted the Supervisor session (the Keycloak session lived on).
// No provider token may persist to do this (product decision 2026-11-12): the end-session URL
// carries client_id only, and the provider asks the person to confirm.
func TestWIS713SignOutEndsProviderSession(t *testing.T) {
	dsn, adminURL := os.Getenv("SUPERVISOR_TEST_DATABASE_URL"), os.Getenv("SUPERVISOR_TEST_ADMIN_URL")
	if dsn == "" || adminURL == "" {
		t.Fatal("BLOCKED: SUPERVISOR_TEST_DATABASE_URL and SUPERVISOR_TEST_ADMIN_URL require migrated disposable PostgreSQL")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// The runtime role may only INSERT identity_audit; the administrator reads it back.
	admin, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	subject := "wis713-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithHeader("kid", "fixture"))
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var issuer, nonce, endSession, issued string
	var down bool
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			if down {
				w.WriteHeader(503)
				return
			}
			discovery := map[string]any{"issuer": issuer, "authorization_endpoint": issuer + "/authorize", "token_endpoint": issuer + "/token", "jwks_uri": issuer + "/keys", "id_token_signing_alg_values_supported": []string{"RS256"}}
			if endSession != "" {
				discovery["end_session_endpoint"] = endSession
			}
			_ = json.NewEncoder(w).Encode(discovery)
		case "/keys":
			_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "fixture", Algorithm: "RS256", Use: "sig"}}})
		case "/token":
			claims := map[string]any{"iss": issuer, "aud": "supervisor", "sub": subject, "iat": time.Now().Unix(), "exp": time.Now().Add(time.Minute).Unix(), "nonce": nonce}
			token, err := josejwt.Signed(signer).Claims(claims).Serialize()
			if err != nil {
				t.Error(err)
				w.WriteHeader(500)
				return
			}
			issued = token
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
	do := func(method, path string, headers map[string]string) (int, http.Header, []byte) {
		t.Helper()
		r, _ := http.NewRequest(method, app.URL+path, nil)
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		res, err := browser.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		return res.StatusCode, res.Header, body
	}
	// signIn starts a normal (not re-authentication) sign-in, asserts it forces the provider's
	// form, completes the callback and returns the signed-in principal.
	signIn := func(label string) Principal {
		t.Helper()
		status, headers, _ := do("GET", "/auth/login", nil)
		if status != 302 {
			t.Fatalf("%s: login status %d", label, status)
		}
		u, err := url.Parse(headers.Get("Location"))
		if err != nil {
			t.Fatal(err)
		}
		q := u.Query()
		if q.Get("prompt") != "login" || q.Get("max_age") != "0" {
			t.Errorf("%s: sign-in does not force the provider form: prompt=%q max_age=%q", label, q.Get("prompt"), q.Get("max_age"))
		}
		mu.Lock()
		nonce = q.Get("nonce")
		mu.Unlock()
		if status, _, _ := do("GET", "/auth/callback?code=fixture&state="+url.QueryEscape(q.Get("state")), nil); status != 302 {
			t.Fatalf("%s: callback %d", label, status)
		}
		status, _, body := do("GET", "/me", nil)
		if status != 200 {
			t.Fatalf("%s: me %d", label, status)
		}
		var p Principal
		if err := json.Unmarshal(body, &p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	appURL, _ := url.Parse(app.URL)
	logout := func(p Principal) (int, []byte) {
		t.Helper()
		saved := jar.Cookies(appURL)
		status, headers, body := do("POST", "/auth/logout", map[string]string{"Origin": app.URL, "X-CSRF-Token": p.CSRFToken})
		// The cleared cookie must carry the attributes it was set with, or the browser keeps the
		// original (a different Path is a different cookie).
		cleared := false
		for _, c := range (&http.Response{Header: headers}).Cookies() {
			if c.MaxAge < 0 && c.Value == "" {
				cleared = true
				if c.Path != "/" || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode {
					t.Errorf("cleared cookie %s: Path=%q HttpOnly=%v SameSite=%v, want / true Lax", c.Name, c.Path, c.HttpOnly, c.SameSite)
				}
			}
		}
		if !cleared {
			t.Errorf("logout did not clear the session cookie: %q", headers.Values("Set-Cookie"))
		}
		// The old cookie, replayed, no longer authenticates.
		jar.SetCookies(appURL, saved)
		if status, _, _ := do("GET", "/me", nil); status != 401 {
			t.Errorf("old session cookie after logout: %d, want 401", status)
		}
		return status, body
	}

	// 1. A provider that publishes end_session_endpoint: logout hands the browser the
	// RP-initiated logout URL bound to this session's own ID token.
	mu.Lock()
	endSession = provider.URL + "/logout"
	mu.Unlock()
	p := signIn("first sign-in")
	mu.Lock()
	idToken := issued
	mu.Unlock()
	// Provider tokens never persist: the live session row holds neither the ID token (nor any of
	// its segments) nor the access token the provider returned.
	var row string
	if err := db.QueryRow("SELECT row_to_json(s)::text FROM supervisor.browser_sessions s WHERE subject=$1 AND issuer=$2", subject, provider.URL).Scan(&row); err != nil {
		t.Fatal(err)
	}
	for _, part := range append(strings.Split(idToken, "."), "discarded-fixture") {
		if strings.Contains(row, part) {
			t.Errorf("a provider token is persisted on the session row")
		}
	}
	status, body := logout(p)
	if status != 200 {
		t.Fatalf("logout with provider end-session: %d, want 200 with end_session_url", status)
	}
	var result map[string]string
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatal(err)
	}
	end, err := url.Parse(result["end_session_url"])
	if err != nil || end.Scheme+"://"+end.Host+end.Path != provider.URL+"/logout" {
		t.Fatalf("end_session_url %q does not target the provider end-session endpoint", result["end_session_url"])
	}
	q := end.Query()
	if q.Has("id_token_hint") || q.Get("client_id") != "supervisor" || q.Get("post_logout_redirect_uri") != app.URL+"/" {
		t.Errorf("end-session parameters: id_token_hint present=%v client_id=%q post_logout_redirect_uri=%q", q.Has("id_token_hint"), q.Get("client_id"), q.Get("post_logout_redirect_uri"))
	}
	var kept int
	if err := db.QueryRow("SELECT count(*) FROM supervisor.browser_sessions WHERE subject=$1 AND issuer=$2", subject, provider.URL).Scan(&kept); err != nil || kept != 0 {
		t.Errorf("session rows after logout: %d (%v)", kept, err)
	}
	// skipped counts the recorded "provider session not ended" sign outs of this subject.
	skipped := func() int {
		t.Helper()
		var n int
		if err := admin.QueryRow("SELECT count(*) FROM supervisor.identity_audit WHERE event_type='provider_logout_skipped' AND subject=$1", subject).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := skipped(); n != 0 {
		t.Errorf("provider logout recorded as skipped although it was handed out: %d", n)
	}

	// 2. Signing in again after sign out still forces the provider's form (asserted in signIn).
	p = signIn("sign-in after sign-out")

	// 3. An end-session endpoint that is not HTTPS on a public host is never handed out; local
	// sign out still succeeds without the provider (204), and the next sign-in still forces the form.
	mu.Lock()
	endSession = "http://evil.example/logout"
	mu.Unlock()
	if status, body := logout(p); status != 204 || len(body) != 0 {
		t.Errorf("unsafe end-session endpoint: status %d body %q, want 204 and no URL", status, body)
	}
	// A sign out that leaves the provider session alive is recorded (review finding 4).
	if n := skipped(); n != 1 {
		t.Errorf("unsafe end-session: %d provider_logout_skipped audit entries, want 1", n)
	}
	p = signIn("sign-in after unsafe end-session")

	// 4. A provider without end_session_endpoint: plain local sign out (204), recorded.
	mu.Lock()
	endSession = ""
	mu.Unlock()
	if status, body := logout(p); status != 204 || len(body) != 0 {
		t.Errorf("provider without end-session: status %d body %q, want 204", status, body)
	}
	if n := skipped(); n != 2 {
		t.Errorf("no end-session: %d provider_logout_skipped audit entries, want 2", n)
	}
	p = signIn("sign-in after no end-session")

	// 5. The provider is unreachable at sign out: the local sign out still succeeds (204), recorded.
	mu.Lock()
	endSession, down = provider.URL+"/logout", true
	mu.Unlock()
	if status, body := logout(p); status != 204 || len(body) != 0 {
		t.Errorf("provider unreachable: status %d body %q, want 204", status, body)
	}
	if n := skipped(); n != 3 {
		t.Errorf("provider unreachable: %d provider_logout_skipped audit entries, want 3", n)
	}
}
