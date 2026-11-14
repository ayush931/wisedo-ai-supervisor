//go:build integration

package migrate

import (
	"context"
	"testing"
)

func TestShiftSummariesMigrationCatalog(t *testing.T) {
	db := newOperationalMigrationDB(t)
	ctx := context.Background()
	if err := Check(ctx, db); err != nil {
		t.Fatal(err)
	}
	// Every shift summary table is append-only for the runtime role.
	for _, table := range []string{"shift_summaries", "shift_summary_receipts", "shift_summary_audit"} {
		var protected, nonOwner, canSelect, canInsert, canDelete, canUpdate, immutable bool
		var policies int
		err := db.QueryRowContext(ctx, `SELECT c.relrowsecurity AND c.relforcerowsecurity,c.relowner<>r.oid,has_table_privilege('supervisor_runtime',c.oid,'SELECT'),has_table_privilege('supervisor_runtime',c.oid,'INSERT'),has_table_privilege('supervisor_runtime',c.oid,'DELETE'),has_any_column_privilege('supervisor_runtime',c.oid,'UPDATE'),
 (SELECT count(*) FROM pg_policies p WHERE p.schemaname='supervisor' AND p.tablename=c.relname AND p.policyname='tenant_scope'),
 EXISTS(SELECT 1 FROM pg_trigger g WHERE g.tgrelid=c.oid AND g.tgname=c.relname||'_immutable' AND g.tgenabled='O')
 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace CROSS JOIN pg_roles r WHERE n.nspname='supervisor' AND c.relname=$1 AND r.rolname='supervisor_runtime'`, table).Scan(&protected, &nonOwner, &canSelect, &canInsert, &canDelete, &canUpdate, &policies, &immutable)
		if err != nil {
			t.Fatalf("%s: %v", table, err)
		}
		if !protected || !nonOwner || !canSelect || !canInsert || canDelete || canUpdate || policies != 1 || !immutable {
			t.Errorf("%s RLS/grant invariant: protected=%v nonowner=%v select=%v insert=%v delete=%v update=%v policies=%d immutable=%v", table, protected, nonOwner, canSelect, canInsert, canDelete, canUpdate, policies, immutable)
		}
	}
	var version int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM public.supervisor_schema_versions WHERE version=36`).Scan(&version); err != nil || version != 1 {
		t.Fatalf("migration 36 recorded %d times (%v)", version, err)
	}
}
