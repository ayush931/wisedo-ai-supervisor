//go:build integration

package migrate

import (
	"context"
	"testing"
)

func TestShiftPlansMigrationCatalog(t *testing.T) {
	db := newOperationalMigrationDB(t)
	ctx := context.Background()
	if err := Apply(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := Check(ctx, db); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"shift_plan_heads", "shift_plan_revisions", "shift_plan_approvals", "shift_plan_receipts", "shift_plan_audit"} {
		var protected, nonOwner, canSelect, canInsert, canDelete, canUpdate bool
		var policies int
		err := db.QueryRowContext(ctx, `SELECT c.relrowsecurity AND c.relforcerowsecurity,c.relowner<>r.oid,has_table_privilege('supervisor_runtime',c.oid,'SELECT'),has_table_privilege('supervisor_runtime',c.oid,'INSERT'),has_table_privilege('supervisor_runtime',c.oid,'DELETE'),has_table_privilege('supervisor_runtime',c.oid,'UPDATE'),(SELECT count(*) FROM pg_policies p WHERE p.schemaname='supervisor' AND p.tablename=c.relname AND p.policyname='tenant_scope') FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace CROSS JOIN pg_roles r WHERE n.nspname='supervisor' AND c.relname=$1 AND r.rolname='supervisor_runtime'`, table).Scan(&protected, &nonOwner, &canSelect, &canInsert, &canDelete, &canUpdate, &policies)
		if err != nil {
			t.Fatalf("%s: %v", table, err)
		}
		if !protected || !nonOwner || !canSelect || !canInsert || canDelete || canUpdate || policies != 1 {
			t.Errorf("%s RLS/grant invariant: protected=%v nonowner=%v select=%v insert=%v delete=%v update=%v policies=%d", table, protected, nonOwner, canSelect, canInsert, canDelete, canUpdate, policies)
		}
	}
	var version, immutable, headUpdateColumns, shiftUnique int
	if err := db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM public.supervisor_schema_versions WHERE version=32),(SELECT count(*) FROM pg_trigger t JOIN pg_class c ON c.oid=t.tgrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='supervisor' AND NOT t.tgisinternal AND t.tgname IN ('shift_plan_revisions_immutable','shift_plan_approvals_immutable','shift_plan_receipts_immutable','shift_plan_audit_immutable')),(SELECT count(*) FROM information_schema.column_privileges WHERE grantee='supervisor_runtime' AND table_schema='supervisor' AND table_name='shift_plan_heads' AND privilege_type='UPDATE'),(SELECT count(*) FROM pg_constraint WHERE conrelid='supervisor.shift_plan_heads'::regclass AND contype='u' AND pg_get_constraintdef(oid)='UNIQUE (tenant_id, site_id, line_id, starts_at, ends_at)')`).Scan(&version, &immutable, &headUpdateColumns, &shiftUnique); err != nil {
		t.Fatal(err)
	}
	// Only the head pointers and its state counter are updatable, and only through the guard.
	// One head per line and shift, with one approved pointer, bounds approved-current plans to one.
	if version != 1 || immutable != 4 || headUpdateColumns != 3 || shiftUnique != 1 {
		t.Fatalf("migration invariants: version=%d immutable=%d head update columns=%d shift unique=%d", version, immutable, headUpdateColumns, shiftUnique)
	}
}
