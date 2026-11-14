//go:build integration

package httpapi

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
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
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/plans"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/session"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestConfiguredSessionPolicyIsEnforcedEverywhere proves WIS-290's single configurable session
// policy reaches both the identity layer and the services: a session idle for five minutes is
// accepted under the 30-minute default and rejected under a configured 2-minute idle limit.
func TestConfiguredSessionPolicyIsEnforcedEverywhere(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	adminURL, runtimeURL := os.Getenv("SUPERVISOR_TEST_ADMIN_URL"), os.Getenv("SUPERVISOR_TEST_DATABASE_URL")
	if adminURL == "" || runtimeURL == "" {
		t.Fatal("real PostgreSQL admin and runtime URLs required")
	}
	admin, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close() })
	database := "session_policy_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	tenant, site, subject := uuid.NewString(), uuid.NewString(), uuid.NewString()
	issuer, origin := "https://session-policy.invalid/"+uuid.NewString(), "http://127.0.0.1:58177"
	exec(`INSERT INTO supervisor.tenants(id,name) VALUES($1,'synthetic session test')`, tenant)
	exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'session site')`, tenant, site)
	exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'viewer')`, tenant, site, issuer, subject)
	// One session, last used five minutes ago.
	token := uuid.NewString()
	sum := sha256.Sum256([]byte(token))
	tokenHash := hex.EncodeToString(sum[:])
	seed := func() {
		exec(`DELETE FROM supervisor.browser_sessions WHERE token_hash=$1`, tokenHash)
		exec(`INSERT INTO supervisor.browser_sessions(token_hash,subject,issuer,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,'synthetic',now()-interval '10 minutes',now()-interval '5 minutes',now()+interval '1 hour',now()-interval '10 minutes')`, tokenHash, subject, issuer)
	}
	strict := session.Policy{Idle: 2 * time.Minute, Absolute: 8 * time.Hour, RecentAuth: 2 * time.Minute}

	me := func(policy session.Policy) int {
		t.Helper()
		seed()
		auth, err := identity.New(ctx, identity.Config{Issuer: issuer, ClientID: "session-test", ClientSecret: "synthetic-only", RedirectURI: origin + "/auth/callback", Sessions: policy}, runtime, nil)
		if err != nil {
			t.Fatal(err)
		}
		handler := New(config.Config{Environment: "local", WebDir: t.TempDir(), Sessions: policy}, db, auth)
		r := httptest.NewRequest("GET", "/v1/me", nil)
		r.AddCookie(&http.Cookie{Name: "supervisor", Value: token})
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Code
	}
	if code := me(session.Default()); code != http.StatusOK {
		t.Fatalf("default policy rejected a session idle for 5 minutes: HTTP %d", code)
	}
	if code := me(strict); code != http.StatusUnauthorized {
		t.Fatalf("2-minute idle policy accepted a session idle for 5 minutes: HTTP %d", code)
	}

	// The services enforce the same policy on their own session checks.
	actor := plans.Actor{Issuer: issuer, Subject: subject, SessionHash: tokenHash}
	scope := plans.Scope{TenantID: tenant, SiteID: site}
	seed()
	if _, err := plans.New(runtime).Get(ctx, actor, scope, uuid.NewString()); !errors.Is(err, plans.ErrNotFound) {
		t.Fatalf("default policy: session check should pass and the plan be missing, got %v", err)
	}
	if _, err := plans.New(runtime).WithSessionPolicy(strict).Get(ctx, actor, scope, uuid.NewString()); !errors.Is(err, plans.ErrDenied) {
		t.Fatalf("2-minute idle policy: the plans service accepted an idle session, got %v", err)
	}
}
