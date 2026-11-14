//go:build integration

package migrate

import (
	"context"
	"database/sql"
	"testing"

	"github.com/google/uuid"
)

// siteLockHeld reports whether another transaction holds a row lock on the site: FOR UPDATE
// conflicts with both FOR KEY SHARE and FOR NO KEY UPDATE, and NOWAIT fails with 55P03 instead
// of waiting.
func siteLockHeld(t *testing.T, admin *sql.DB, site string) bool {
	t.Helper()
	tx, err := admin.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var one int
	err = tx.QueryRow(`SELECT 1 FROM supervisor.sites WHERE id=$1 FOR UPDATE NOWAIT`, site).Scan(&one)
	if err == nil {
		return false
	}
	if sqlState(err) == "55P03" {
		return true
	}
	t.Fatalf("lock probe: %v", err)
	return false
}

// WIS-699: lock_evidence_site checks the tenant but not site membership, so a member of site A
// could hold site B's row lock (lock contention, no data). lock_member_site refuses a caller
// without an active membership of that site before it takes any lock. It runs as the runtime role.
func TestMemberSiteLockRefusesNonMembersBeforeLocking(t *testing.T) {
	admin := newOperationalMigrationDB(t)
	ctx := context.Background()
	runtime := runtimeDB(t, admin)
	tenant, siteA := createSite(t, admin)
	otherTenant, _ := createSite(t, admin)
	siteB := uuid.NewString()
	const issuer = "https://idp.example"
	memberA, memberB, inactiveB := uuid.NewString(), uuid.NewString(), uuid.NewString()

	tx, err := admin.Begin()
	if err != nil {
		t.Fatal(err)
	}
	setTenant(t, tx, tenant)
	if _, err = tx.Exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'site B')`, tenant, siteB); err != nil {
		t.Fatal(err)
	}
	for _, m := range []struct {
		site, subject string
		active        bool
	}{{siteA, memberA, true}, {siteB, memberB, true}, {siteB, inactiveB, false}} {
		if _, err = tx.Exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role,active) VALUES($1,$2,$3,$4,'operator',$5)`, tenant, m.site, issuer, m.subject, m.active); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		name, guc, subject string
		write, wantFound   bool
	}{
		{"member of another site, write", tenant, memberA, true, false},
		{"member of another site, read", tenant, memberA, false, false},
		{"inactive member", tenant, inactiveB, true, false},
		{"tenant context of another tenant", otherTenant, memberB, true, false},
		{"member, write", tenant, memberB, true, true},
		{"member, read", tenant, memberB, false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			tx, err := runtime.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			setTenant(t, tx, c.guc)
			var found bool
			if err = tx.QueryRow(`SELECT supervisor.lock_member_site($1,$2,$3,$4,$5)`, tenant, siteB, issuer, c.subject, c.write).Scan(&found); err != nil {
				t.Fatal(err)
			}
			if found != c.wantFound {
				t.Fatalf("found = %v, want %v", found, c.wantFound)
			}
			if held := siteLockHeld(t, admin, siteB); held != c.wantFound {
				t.Fatalf("site B lock held = %v, want %v", held, c.wantFound)
			}
		})
	}

	// The pre-fix function still locks site B for a member of site A only; request paths no
	// longer call it, which is what the cases above guard.
	t.Run("lock_evidence_site still locks for a non-member", func(t *testing.T) {
		tx, err := runtime.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		setTenant(t, tx, tenant)
		var found bool
		if err = tx.QueryRow(`SELECT supervisor.lock_evidence_site($1,$2,true)`, tenant, siteB).Scan(&found); err != nil {
			t.Fatal(err)
		}
		if !found || !siteLockHeld(t, admin, siteB) {
			t.Fatalf("old function: found = %v, lock not held", found)
		}
	})
}
