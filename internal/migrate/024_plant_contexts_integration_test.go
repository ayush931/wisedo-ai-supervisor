//go:build integration

package migrate

import (
	"context"
	"testing"
)

func TestPlantContextsMigrationCatalog(t *testing.T) {
	db := newOperationalMigrationDB(t)
	ctx := context.Background()
	if err := Check(ctx, db); err != nil {
		t.Fatal(err)
	}
	assertGrants(t, db, appendOnly, "plant_contexts", "plant_context_records", "plant_context_heads", "plant_context_outbox", "plant_context_receipts", "plant_context_audit")
	var version, immutable, headUpdateColumns, indexes int
	if err := db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM public.supervisor_schema_versions WHERE version=24),(SELECT count(*) FROM pg_trigger t JOIN pg_class c ON c.oid=t.tgrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='supervisor' AND NOT t.tgisinternal AND t.tgname IN ('plant_contexts_immutable','plant_context_records_immutable','plant_context_outbox_immutable','plant_context_receipts_immutable','plant_context_audit_immutable')),(SELECT count(*) FROM information_schema.column_privileges WHERE grantee='supervisor_runtime' AND table_schema='supervisor' AND table_name='plant_context_heads' AND privilege_type='UPDATE'),(SELECT count(*) FROM pg_indexes WHERE schemaname='supervisor' AND indexname IN ('operational_event_records_chain','operational_event_records_delta_scope'))`).Scan(&version, &immutable, &headUpdateColumns, &indexes); err != nil {
		t.Fatal(err)
	}
	if version != 1 || immutable != 5 || headUpdateColumns != 2 || indexes != 2 {
		t.Fatalf("migration invariants: version=%d immutable=%d head update columns=%d indexes=%d", version, immutable, headUpdateColumns, indexes)
	}
	// Untrusted payload timestamps never fail a query: anything noncanonical is NULL.
	for value, valid := range map[string]bool{
		"2026-09-27T00:15:00Z":      true,
		"2026-09-27T00:15:00.25Z":   true,
		"2026-02-31T00:00:00Z":      false,
		"2026-09-27T05:30:00+05:30": false,
		"2026-09-27 00:15:00":       false,
		"yesterday":                 false,
	} {
		var parsed bool
		if err := db.QueryRowContext(ctx, `SELECT supervisor.try_utc_timestamp($1) IS NOT NULL`, value).Scan(&parsed); err != nil || parsed != valid {
			t.Errorf("try_utc_timestamp(%q)=%v err=%v want %v", value, parsed, err, valid)
		}
	}
}
