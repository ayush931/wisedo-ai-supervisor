package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func environment() map[string]string {
	return map[string]string{
		"SUPERVISOR_ENVIRONMENT":          "local",
		"SUPERVISOR_LOCAL_KEYCLOAK":       "http://127.0.0.1:58077",
		"SUPERVISOR_TEST_ADMIN_URL":       "postgresql://postgres:fixture@127.0.0.1:55477/supervisor",
		"SUPERVISOR_LOCAL_ADMIN":          "local-admin",
		"SUPERVISOR_LOCAL_ADMIN_PASSWORD": "admin-secret",
		"SUPERVISOR_LOCAL_USER_PASSWORD":  "user-secret",
		"SUPERVISOR_OIDC_CLIENT_SECRET":   "client-secret",
		"SUPERVISOR_PUBLIC_ORIGIN":        "http://127.0.0.1:58176",
	}
}
func TestRejectNonlocalAndEffectiveDatabaseHost(t *testing.T) {
	for _, test := range []struct{ name, key, value string }{
		{"production", "SUPERVISOR_ENVIRONMENT", "production"},
		{"remote provider", "SUPERVISOR_LOCAL_KEYCLOAK", "http://example.com"},
		{"provider credentials", "SUPERVISOR_LOCAL_KEYCLOAK", "http://user:secret@127.0.0.1"},
		{"provider query", "SUPERVISOR_LOCAL_KEYCLOAK", "http://127.0.0.1?redirect=remote"},
		{"remote browser", "SUPERVISOR_PUBLIC_ORIGIN", "http://example.com"},
		{"remote database", "SUPERVISOR_TEST_ADMIN_URL", "postgresql://postgres:secret@example.com/supervisor"},
		{"query overrides host", "SUPERVISOR_TEST_ADMIN_URL", "postgresql://postgres:secret@127.0.0.1/supervisor?host=example.com"},
		{"missing credential", "SUPERVISOR_LOCAL_ADMIN_PASSWORD", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			values := environment()
			values[test.key] = test.value
			if _, err := loadConfig(func(k string) string { return values[k] }); err == nil {
				t.Fatal("unsafe fixture configuration accepted")
			}
		})
	}
	if _, err := loadConfig(func(k string) string { return environment()[k] }); err != nil {
		t.Fatal(err)
	}
}
func TestRedirectNeverForwardsCredentials(t *testing.T) {
	var hits atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	values := environment()
	values["SUPERVISOR_LOCAL_KEYCLOAK"] = source.URL
	cfg, err := loadConfig(func(k string) string { return values[k] })
	if err != nil {
		t.Fatal(err)
	}
	_, err = seedIdentity(context.Background(), cfg)
	if err == nil || hits.Load() != 0 {
		t.Fatalf("redirect followed or accepted: hits=%d error=%v", hits.Load(), err)
	}
	for _, secret := range []string{"admin-secret", "user-secret", "client-secret"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatal("credential leaked")
		}
	}
}
func TestKeycloakFixtureCreatesAndUpdatesWithExactIdentity(t *testing.T) {
	for _, existing := range []bool{false, true} {
		var calls atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			if r.URL.Path != "/realms/master/protocol/openid-connect/token" && r.Header.Get("Authorization") != "Bearer fixture-token" {
				t.Error("missing admin token")
			}
			w.Header().Set("Content-Type", "application/json")
			switch r.Method + " " + r.URL.Path {
			case "POST /realms/master/protocol/openid-connect/token":
				if err := r.ParseForm(); err != nil || r.Form.Get("username") != "local-admin" || r.Form.Get("password") != "admin-secret" {
					t.Error("incorrect credential exchange")
				}
				_, _ = w.Write([]byte(`{"access_token":"fixture-token"}`))
			case "POST /admin/realms":
				w.WriteHeader(http.StatusConflict)
			case "GET /admin/realms/supervisor-local/clients":
				if r.URL.Query().Get("clientId") != "supervisor" {
					t.Error("wrong client lookup")
				}
				if existing {
					_, _ = w.Write([]byte(`[{"id":"00000000-0000-4000-8000-000000000292"}]`))
				} else {
					_, _ = w.Write([]byte(`[]`))
				}
			case "POST /admin/realms/supervisor-local/clients", "PUT /admin/realms/supervisor-local/clients/00000000-0000-4000-8000-000000000292":
				var body struct {
					Secret       string            `json:"secret"`
					RedirectURIs []string          `json:"redirectUris"`
					DirectAccess bool              `json:"directAccessGrantsEnabled"`
					Attributes   map[string]string `json:"attributes"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Secret != "client-secret" || body.DirectAccess || len(body.RedirectURIs) != 1 || body.RedirectURIs[0] != "http://127.0.0.1:58176/auth/callback" || body.Attributes["pkce.code.challenge.method"] != "S256" || body.Attributes["post.logout.redirect.uris"] != "http://127.0.0.1:58176/" {
					t.Error("client security configuration changed")
				}
				w.WriteHeader(http.StatusNoContent)
			case "POST /admin/realms/supervisor-local/users":
				w.WriteHeader(http.StatusConflict)
			case "GET /admin/realms/supervisor-local/users":
				if r.URL.Query().Get("username") != "foundation-reviewer" || r.URL.Query().Get("exact") != "true" {
					t.Error("incorrect reviewer lookup")
				}
				_, _ = w.Write([]byte(`[{"id":"00000000-0000-4000-8000-000000000293"}]`))
			case "PUT /admin/realms/supervisor-local/users/00000000-0000-4000-8000-000000000293/reset-password":
				w.WriteHeader(http.StatusNoContent)
			default:
				t.Errorf("unexpected route: %s %s", r.Method, r.URL.Path)
				w.WriteHeader(http.StatusBadRequest)
			}
		}))
		values := environment()
		values["SUPERVISOR_LOCAL_KEYCLOAK"] = server.URL
		cfg, err := loadConfig(func(k string) string { return values[k] })
		if err != nil {
			t.Fatal(err)
		}
		subject, err := seedIdentity(context.Background(), cfg)
		server.Close()
		if err != nil || subject != "00000000-0000-4000-8000-000000000293" || calls.Load() != 7 {
			t.Fatalf("subject=%q calls=%d err=%v", subject, calls.Load(), err)
		}
	}
}

func TestIdentityResponseBoundsAndSafeErrors(t *testing.T) {
	for _, oversized := range []bool{false, true} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if oversized {
				_, _ = w.Write([]byte(strings.Repeat("x", (1<<20)+1)))
				return
			}
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte("admin-secret user-secret client-secret"))
		}))
		values := environment()
		values["SUPERVISOR_LOCAL_KEYCLOAK"] = server.URL
		cfg, err := loadConfig(func(k string) string { return values[k] })
		if err != nil {
			t.Fatal(err)
		}
		_, err = seedIdentity(context.Background(), cfg)
		server.Close()
		if err == nil {
			t.Fatal("invalid provider response accepted")
		}
		for _, secret := range []string{"admin-secret", "user-secret", "client-secret"} {
			if strings.Contains(err.Error(), secret) {
				t.Fatal("provider response leaked a secret")
			}
		}
	}
}
