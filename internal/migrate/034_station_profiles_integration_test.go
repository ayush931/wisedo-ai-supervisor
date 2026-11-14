//go:build integration

package migrate

import (
	"context"
	"testing"
)

func TestStationProfilesMigrationCatalog(t *testing.T) {
	db := newOperationalMigrationDB(t)
	ctx := context.Background()
	if err := Check(ctx, db); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"station_profile_heads", "station_profile_revisions", "station_profile_receipts", "station_profile_audit"} {
		var protected, nonOwner, canSelect, canInsert, canDelete, canUpdate, immutable bool
		var policies int
		err := db.QueryRowContext(ctx, `SELECT c.relrowsecurity AND c.relforcerowsecurity,c.relowner<>r.oid,has_table_privilege('supervisor_runtime',c.oid,'SELECT'),has_table_privilege('supervisor_runtime',c.oid,'INSERT'),has_table_privilege('supervisor_runtime',c.oid,'DELETE'),has_table_privilege('supervisor_runtime',c.oid,'UPDATE'),
 (SELECT count(*) FROM pg_policies p WHERE p.schemaname='supervisor' AND p.tablename=c.relname AND p.policyname='tenant_scope'),
 EXISTS(SELECT 1 FROM pg_trigger g WHERE g.tgrelid=c.oid AND g.tgname=c.relname||'_immutable' AND g.tgenabled='O')
 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace CROSS JOIN pg_roles r WHERE n.nspname='supervisor' AND c.relname=$1 AND r.rolname='supervisor_runtime'`, table).Scan(&protected, &nonOwner, &canSelect, &canInsert, &canDelete, &canUpdate, &policies, &immutable)
		if err != nil {
			t.Fatalf("%s: %v", table, err)
		}
		// Only the head advances (by column grant and guard); every other table is append-only.
		wantImmutable := table != "station_profile_heads"
		if !protected || !nonOwner || !canSelect || !canInsert || canDelete || canUpdate || policies != 1 || immutable != wantImmutable {
			t.Errorf("%s RLS/grant invariant: protected=%v nonowner=%v select=%v insert=%v delete=%v update=%v policies=%d immutable=%v", table, protected, nonOwner, canSelect, canInsert, canDelete, canUpdate, policies, immutable)
		}
	}
	var version, headUpdateColumns int
	if err := db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM public.supervisor_schema_versions WHERE version=34),(SELECT count(*) FROM information_schema.column_privileges WHERE grantee='supervisor_runtime' AND table_schema='supervisor' AND table_name='station_profile_heads' AND privilege_type='UPDATE')`).Scan(&version, &headUpdateColumns); err != nil {
		t.Fatal(err)
	}
	if version != 1 || headUpdateColumns != 1 {
		t.Fatalf("migration invariants: version=%d head update columns=%d", version, headUpdateColumns)
	}
}
