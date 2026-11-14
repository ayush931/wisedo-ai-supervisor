//go:build integration

package evidence

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/migrate"
	"github.com/google/uuid"
)

// WIS-699: a member of site A asking for site B of the same tenant got the non-disclosing 404
// only after taking site B's row lock, so while site B was busy the request waited on it (up to
// lock_timeout, then failed with a lock error instead of the 404). The request paths now refuse a
// non-member before locking: the 404 comes back at once and site B's writers never wait on it.
func TestNonMemberDoesNotWaitOnAnotherSitesLock(t *testing.T) {
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
	siteA, siteB := Scope{tenant, uuid.NewString()}, Scope{tenant, uuid.NewString()}
	exec(`INSERT INTO supervisor.tenants(id,name) VALUES($1,'synthetic site lock test')`, tenant)
	exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'site A'),($1,$3,'site B')`, tenant, siteA.SiteID, siteB.SiteID)
	actor := func(site Scope) Actor {
		t.Helper()
		a := Actor{"https://evidence.example/realm", uuid.NewString(), hash([]byte(uuid.NewString()))}
		exec(`INSERT INTO supervisor.browser_sessions(token_hash,issuer,subject,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,'synthetic',now(),now(),now()+interval '1 hour',now())`, a.SessionHash, a.Issuer, a.Subject)
		exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'operator')`, site.TenantID, site.SiteID, a.Issuer, a.Subject)
		return a
	}
	memberA, memberB := actor(siteA), actor(siteB)
	s := New(runtime)

	// Another transaction is busy on site B: FOR UPDATE conflicts with both the read
	// (FOR KEY SHARE) and the write (FOR NO KEY UPDATE) site lock.
	busy, err := admin.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Rollback()
	if _, err = busy.ExecContext(ctx, `SELECT 1 FROM supervisor.sites WHERE id=$1 FOR UPDATE`, siteB.SiteID); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		name string
		call func() error
	}{
		{"read", func() error { _, e := s.List(ctx, memberA, siteB, 10, ""); return e }},
		{"write", func() error {
			_, e := s.CreateOrRevise(ctx, memberA, siteB, Input{SourceID: uuid.NewString(), Kind: "manual", Title: "t", Text: "x"}, "k-"+uuid.NewString(), 0)
			return e
		}},
	} {
		start := time.Now()
		err := c.call()
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s by a member of site A on site B: %v, want ErrNotFound", c.name, err)
		}
		if waited := time.Since(start); waited > 2*time.Second {
			t.Fatalf("%s by a non-member waited %v on site B's lock", c.name, waited)
		}
	}

	// Control: the busy lock is real. A member of site B does wait on it, and fails on the
	// 5-second lock_timeout rather than reading past it.
	start := time.Now()
	if _, err = s.List(ctx, memberB, siteB, 10, ""); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("member of site B read past a held site lock: %v", err)
	}
	if waited := time.Since(start); waited < 4*time.Second {
		t.Fatalf("member of site B returned after %v, before lock_timeout", waited)
	}
}
