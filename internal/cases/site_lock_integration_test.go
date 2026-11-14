//go:build integration

package cases

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/migrate"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestCaseRoutesNonMemberDoesNotWaitOnAnotherSitesLock (WIS-699, review of #293): the case routes
// took the site lock before checking membership, so a member of site A could hold up site B's
// writers until the membership check refused it. A non-member now gets the non-disclosing
// not-found at once; a member of site B still waits on the held lock (control).
func TestCaseRoutesNonMemberDoesNotWaitOnAnotherSitesLock(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	adminURL, runtimeURL := os.Getenv("SUPERVISOR_TEST_ADMIN_URL"), os.Getenv("SUPERVISOR_TEST_DATABASE_URL")
	if adminURL == "" || runtimeURL == "" {
		t.Fatal("BLOCKED: disposable PostgreSQL admin/runtime URLs required")
	}
	admin, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	runtime, err := sql.Open("pgx", runtimeURL)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if err = migrate.Apply(ctx, admin); err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := admin.ExecContext(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	tenant := uuid.NewString()
	siteA, siteB := Scope{TenantID: tenant, SiteID: uuid.NewString()}, Scope{TenantID: tenant, SiteID: uuid.NewString()}
	exec(`INSERT INTO supervisor.tenants(id,name) VALUES($1,'synthetic case lock test')`, tenant)
	exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'site A'),($1,$3,'site B')`, tenant, siteA.SiteID, siteB.SiteID)
	member := func(site Scope) Principal {
		t.Helper()
		sum := sha256.Sum256([]byte(uuid.NewString()))
		p := Principal{Issuer: "https://cases.example/realm", Subject: uuid.NewString(), SessionHash: hex.EncodeToString(sum[:])}
		exec(`INSERT INTO supervisor.browser_sessions(token_hash,issuer,subject,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,'synthetic',now(),now(),now()+interval '1 hour',now())`, p.SessionHash, p.Issuer, p.Subject)
		exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'supervisor')`, site.TenantID, site.SiteID, p.Issuer, p.Subject)
		return p
	}
	memberA, memberB := member(siteA), member(siteB)
	s := NewService(runtime)

	// Another transaction holds site B's row: FOR UPDATE conflicts with the read site lock.
	busy, err := admin.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Rollback()
	if _, err = busy.ExecContext(ctx, `SELECT 1 FROM supervisor.sites WHERE id=$1 FOR UPDATE`, siteB.SiteID); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	if _, err = s.List(ctx, memberA, siteB, "", 10); !errors.Is(err, ErrNotFound) {
		t.Fatalf("case list by a member of site A on site B: %v, want ErrNotFound", err)
	}
	if waited := time.Since(start); waited > 2*time.Second {
		t.Fatalf("a non-member waited %v on site B's lock", waited)
	}

	// Control: the held lock is real. A member of site B waits on it and fails on the 5 s
	// lock_timeout instead of reading past it.
	start = time.Now()
	if _, err = s.List(ctx, memberB, siteB, "", 10); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("a member of site B read past a held site lock: %v", err)
	}
	if waited := time.Since(start); waited < 4*time.Second {
		t.Fatalf("a member of site B returned after %v, before lock_timeout", waited)
	}
}
