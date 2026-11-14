//go:build integration

package migrate

import (
	"context"
	"strings"
	"testing"
)

func TestPlanRevisionsMigrationCatalog(t *testing.T) {
	db := newOperationalMigrationDB(t)
	ctx := context.Background()
	if err := Check(ctx, db); err != nil {
		t.Fatal(err)
	}
	assertGrants(t, db, appendOnly, "plan_revisions", "plan_revision_spans", "plan_review_heads", "plan_approvals", "plan_approval_spans", "plan_receipts", "plan_audit")
	var version, immutable, headUpdateColumns int
	if err := db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM public.supervisor_schema_versions WHERE version=23),(SELECT count(*) FROM pg_trigger t JOIN pg_class c ON c.oid=t.tgrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='supervisor' AND NOT t.tgisinternal AND t.tgname IN ('plan_revisions_immutable','plan_revision_spans_immutable','plan_approvals_immutable','plan_approval_spans_immutable','plan_receipts_immutable','plan_audit_immutable')),(SELECT count(*) FROM information_schema.column_privileges WHERE grantee='supervisor_runtime' AND table_schema='supervisor' AND table_name='plan_review_heads' AND privilege_type='UPDATE')`).Scan(&version, &immutable, &headUpdateColumns); err != nil {
		t.Fatal(err)
	}
	// Only the head's current_revision column is updatable, and only through the guard.
	if version != 1 || immutable != 6 || headUpdateColumns != 1 {
		t.Fatalf("migration invariants: version=%d immutable=%d head update columns=%d", version, immutable, headUpdateColumns)
	}
	var reasons, inboxReasons, planCheck string
	if err := db.QueryRowContext(ctx, `SELECT pg_get_constraintdef((SELECT oid FROM pg_constraint WHERE conname='operational_event_records_quarantine_reason_check')),pg_get_constraintdef((SELECT oid FROM pg_constraint WHERE conname='operational_event_inbox_reason_check')),pg_get_constraintdef((SELECT oid FROM pg_constraint WHERE conname='operational_event_plan_approval_check'))`).Scan(&reasons, &inboxReasons, &planCheck); err != nil {
		t.Fatal(err)
	}
	for _, def := range []string{reasons, inboxReasons} {
		for _, code := range []string{"UNAPPROVED_PLAN", "UNKNOWN_MAPPING", "DUPLICATE_EVENT", "MAPPING_MISMATCH", "SUPERSEDED_PLAN", "OUT_OF_SCOPE"} {
			if !strings.Contains(def, code) {
				t.Errorf("quarantine reason %s missing from %s", code, def)
			}
		}
	}
	if !strings.Contains(planCheck, "production_plan") {
		t.Fatalf("admitted-plan invariant missing: %s", planCheck)
	}
}
