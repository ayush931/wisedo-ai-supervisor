//go:build integration

package migrate

import (
	"context"
	"testing"
)

func TestOperationalIngestionMigrationCatalog(t *testing.T) {
	db := newOperationalMigrationDB(t)
	ctx := context.Background()
	if err := Check(ctx, db); err != nil {
		t.Fatal(err)
	}
	assertGrants(t, db, appendOnly, "operational_event_sequence_heads", "operational_event_batches", "operational_event_records", "operational_event_validations", "operational_event_inbox", "operational_event_outbox", "operational_event_batch_receipts")
	var version, rlsPolicies, immutableTriggers int
	if err := db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM public.supervisor_schema_versions WHERE version=21),(SELECT count(*) FROM pg_policies WHERE schemaname='supervisor' AND tablename LIKE 'operational_event_%' AND policyname='tenant_scope'),(SELECT count(*) FROM pg_trigger t JOIN pg_class c ON c.oid=t.tgrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='supervisor' AND c.relname IN ('operational_event_batches','operational_event_records','operational_event_validations','operational_event_inbox','operational_event_outbox','operational_event_batch_receipts') AND NOT t.tgisinternal AND t.tgname LIKE 'operational_event_%_immutable')`).Scan(&version, &rlsPolicies, &immutableTriggers); err != nil {
		t.Fatal(err)
	}
	if version != 1 || rlsPolicies != 7 || immutableTriggers != 6 {
		t.Fatalf("migration invariants: version=%d RLS policies=%d immutable triggers=%d", version, rlsPolicies, immutableTriggers)
	}
}
