//go:build integration

package migrate

import (
	"context"
	"strings"
	"testing"
)

func TestExceptionCasesMigrationCatalog(t *testing.T) {
	db := newOperationalMigrationDB(t)
	ctx := context.Background()
	if err := Check(ctx, db); err != nil {
		t.Fatal(err)
	}
	// The case row advances only through column-level UPDATE grants behind the lifecycle guard.
	assertGrants(t, db, appendOnly, "cases", "case_transitions", "case_trigger_evidence")
	var index string
	if err := db.QueryRowContext(ctx, `SELECT indexdef FROM pg_indexes WHERE schemaname='supervisor' AND indexname='cases_one_active_episode'`).Scan(&index); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(index, "UNIQUE") || !strings.Contains(index, "resolved") || !strings.Contains(index, "canceled") {
		t.Fatalf("one-active-episode index is wrong: %s", index)
	}
}
