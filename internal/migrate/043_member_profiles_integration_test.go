//go:build integration

package migrate

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// WIS-290 member display names: the runtime role reads profiles only in the request tenant and
// writes only the authenticated principal's own rows, only in tenants where it is an active
// member, and only display_name/updated_at; one login statement upserts every tenant.
func TestMemberProfilesCatalogAndSelfWrite(t *testing.T) {
	admin := newOperationalMigrationDB(t)
	ctx := context.Background()
	assertGrants(t, admin, tableGrants{Forced: true, Select: true, Insert: true, TenantPolicies: 1}, "member_profiles")
	var updateColumns string
	if err := admin.QueryRow(`SELECT string_agg(column_name,',' ORDER BY column_name) FROM information_schema.column_privileges WHERE grantee='supervisor_runtime' AND table_schema='supervisor' AND table_name='member_profiles' AND privilege_type='UPDATE'`).Scan(&updateColumns); err != nil || updateColumns != "display_name,updated_at" {
		t.Fatalf("runtime UPDATE columns = %q (%v), want display_name,updated_at", updateColumns, err)
	}
	assertTenantTablesForced(t, admin)

	issuer, subject, other := "https://idp.example/realm", uuid.NewString(), uuid.NewString()
	tenantA, siteA := createSite(t, admin)
	tenantB, siteB := createSite(t, admin)
	tenantC, siteC := createSite(t, admin) // revoked membership only
	tenantD, siteD := createSite(t, admin) // active membership in a deactivated tenant
	for _, m := range []struct {
		tenant, site, subject string
		active                bool
	}{{tenantA, siteA, subject, true}, {tenantB, siteB, subject, true}, {tenantC, siteC, subject, false}, {tenantD, siteD, subject, true}, {tenantA, siteA, other, true}} {
		tx, err := admin.Begin()
		if err != nil {
			t.Fatal(err)
		}
		setTenant(t, tx, m.tenant)
		if _, err = tx.Exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role,active) VALUES($1,$2,$3,$4,'operator',$5)`, m.tenant, m.site, issuer, m.subject, m.active); err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := admin.Exec(`UPDATE supervisor.tenants SET deactivated_at=now() WHERE id=$1`, tenantD); err != nil {
		t.Fatal(err)
	}
	runtime := runtimeDB(t, admin)
	// as runs one statement as the runtime role with the login GUCs of actor.
	as := func(actor, tenant, query string, args ...any) error {
		t.Helper()
		tx, err := runtime.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if _, err = tx.Exec(`SELECT set_config('supervisor.issuer',$1,true),set_config('supervisor.subject',$2,true),set_config('supervisor.tenant_id',$3,true)`, issuer, actor, tenant); err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(query, args...); err != nil {
			return err
		}
		return tx.Commit()
	}
	login := MemberProfileUpsert
	names := func() map[string]string {
		t.Helper()
		rows, err := admin.Query(`SELECT tenant_id::text,coalesce(display_name,'<null>') FROM supervisor.member_profiles WHERE issuer=$1 AND subject=$2`, issuer, subject)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]string{}
		for rows.Next() {
			var tenant, name string
			if err = rows.Scan(&tenant, &name); err != nil {
				t.Fatal(err)
			}
			out[tenant] = name
		}
		return out
	}
	if err := as(subject, "", login, issuer, subject, "Shift Lead"); err != nil {
		t.Fatalf("first login upsert: %v", err)
	}
	if got := names(); len(got) != 2 || got[tenantA] != "Shift Lead" || got[tenantB] != "Shift Lead" {
		t.Fatalf("first login stored %v, want both active tenants and not the revoked one", got)
	}
	// Second login exercises ON CONFLICT DO UPDATE in two tenants in one statement.
	if err := as(subject, "", login, issuer, subject, "Lead Two"); err != nil {
		t.Fatalf("second login upsert: %v", err)
	}
	if got := names(); len(got) != 2 || got[tenantA] != "Lead Two" || got[tenantB] != "Lead Two" {
		t.Fatalf("second login stored %v", got)
	}
	if err := as(subject, "", login, issuer, subject, nil); err != nil {
		t.Fatal(err)
	}
	if got := names(); got[tenantA] != "<null>" || got[tenantB] != "<null>" {
		t.Fatalf("a removed IdP name must clear the stored one: %v", got)
	}

	denied := []struct{ name, actor, tenant, query string }{
		{"another principal's row", subject, tenantA, `INSERT INTO supervisor.member_profiles(tenant_id,issuer,subject,display_name) VALUES('` + tenantA + `','` + issuer + `','` + other + `','Forged')`},
		{"tenant with only a revoked membership", subject, tenantC, `INSERT INTO supervisor.member_profiles(tenant_id,issuer,subject,display_name) VALUES('` + tenantC + `','` + issuer + `','` + subject + `','Revoked')`},
		{"identity column update", subject, tenantA, `UPDATE supervisor.member_profiles SET subject='` + other + `' WHERE subject='` + subject + `'`},
		{"delete", subject, tenantA, `DELETE FROM supervisor.member_profiles WHERE subject='` + subject + `'`},
	}
	for _, c := range denied {
		if err := as(c.actor, c.tenant, c.query); sqlState(err) != "42501" {
			t.Errorf("%s: want 42501, got %v", c.name, err)
		}
	}
	// Another member's display update in the same tenant touches no row (self_profile USING).
	if err := as(other, "", login, issuer, other, "Operator One"); err != nil {
		t.Fatal(err)
	}
	if err := as(subject, tenantA, `UPDATE supervisor.member_profiles SET display_name='Hijacked' WHERE subject='`+other+`'`); err != nil {
		t.Fatal(err)
	}
	var otherName string
	if err := admin.QueryRow(`SELECT display_name FROM supervisor.member_profiles WHERE subject=$1`, other).Scan(&otherName); err != nil || otherName != "Operator One" {
		t.Fatalf("another member's name changed: %q %v", otherName, err)
	}

	// Directory reads are tenant scoped: tenant A context sees A's profiles, never B's.
	tx, err := runtime.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	setTenant(t, tx, tenantA)
	var visibleA, visibleB int
	if err = tx.QueryRow(`SELECT count(*) FILTER (WHERE tenant_id=$1),count(*) FILTER (WHERE tenant_id=$2) FROM supervisor.member_profiles`, tenantA, tenantB).Scan(&visibleA, &visibleB); err != nil {
		t.Fatal(err)
	}
	if visibleA != 2 || visibleB != 0 {
		t.Fatalf("tenant A context sees %d A rows and %d B rows, want 2 and 0", visibleA, visibleB)
	}
}
