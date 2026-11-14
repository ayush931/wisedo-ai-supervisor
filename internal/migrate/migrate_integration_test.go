//go:build integration

package migrate

import (
	"context"
	"database/sql"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"net/url"
	"os"
	"strings"
	"testing"
)

func TestMigrationReadbackAndRepeat(t *testing.T) {
	url := os.Getenv("SUPERVISOR_TEST_ADMIN_URL")
	if url == "" {
		t.Fatal("BLOCKED: disposable PostgreSQL URL required")
	}
	db, err := sql.Open("pgx", url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err = Apply(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err = Apply(ctx, db); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='supervisor' AND c.relrowsecurity AND c.relforcerowsecurity`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 204 { // 058 adds the configuration draft tables; 059 adds three request tables; 062 adds four chat tables and 063 the chat answer turns (WIS-517); 064 adds three maintenance roster tables (WIS-276); 066 adds the action ledger, tasks and task events and 067 the task completions, completion evidence and receipts and 068 the outcome observations (WIS-277); 069 adds feedback, its evidence and receipts (WIS-279); 070 widens CHECKs only (WIS-611); 071 adds agent_run_spans (WIS-614 document passages); 072 widens a CHECK only (WIS-621); 073 adds evidence_span_search (WIS-626); 074 adds the site model hosting head, events and receipts (WIS-623); 075 adds span_embeddings (WIS-627); 076 adds the document scan jobs and their outbox (WIS-625); 077 adds a column and backfills only (WIS-625); 078 adds a column only (WIS-625); 079 adds a function only (WIS-699 member site lock); 080 adds shift runs, their receipts and audit (WIS-701); 081 adds document_uploads (WIS-645); 082 adds agent_runs columns only (WIS-702); 083 adds four correction ledgers (WIS-703); 084 adds an index only (WIS-715); 085 adds operational_import_cancellations (WIS-719).
		t.Fatalf("expected 204 forced-RLS tables after migration 085, got %d", count)
	}
}

func TestMissingSchemaIsNotReady(t *testing.T) {
	raw := os.Getenv("SUPERVISOR_TEST_ADMIN_URL")
	if raw == "" {
		t.Fatal("BLOCKED: disposable admin URL required")
	}
	admin, err := sql.Open("pgx", raw)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	name := "supervisor_empty_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = admin.Exec("CREATE DATABASE " + name); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, e := admin.Exec("DROP DATABASE " + name); e != nil {
			t.Error(e)
		}
	}()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	empty, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer empty.Close()
	// No history table at all: Check must fail on the missing relation, not report ready.
	if err = Check(context.Background(), empty); sqlState(err) != "42P01" {
		t.Fatalf("unmigrated database: want undefined_table, got %v", err)
	}
}

func TestUpgradeFrom001PreservesHistoryAndRejectsDrift(t *testing.T) {
	raw := os.Getenv("SUPERVISOR_TEST_ADMIN_URL")
	if raw == "" {
		t.Fatal("BLOCKED: disposable admin URL required")
	}
	admin, err := sql.Open("pgx", raw)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	name := "supervisor_upgrade_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = admin.Exec("CREATE DATABASE " + name); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, e := admin.Exec("DROP DATABASE " + name); e != nil {
			t.Error(e)
		}
	}()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `CREATE TABLE public.supervisor_schema_versions(version integer PRIMARY KEY,sha256 text NOT NULL,applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, schema); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO public.supervisor_schema_versions(version,sha256) VALUES(1,$1)", checksum(schema)); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = Check(ctx, db); err == nil || err.Error() != "schema upgrade required" {
		t.Fatalf("old schema: want upgrade required, got %v", err)
	}
	if err = Apply(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err = Check(ctx, db); err != nil {
		t.Fatal(err)
	}
	var original string
	if err = db.QueryRowContext(ctx, "SELECT sha256 FROM public.supervisor_schema_versions WHERE version=1").Scan(&original); err != nil {
		t.Fatal(err)
	}
	if original != checksum(schema) {
		t.Fatal("001 checksum changed during upgrade")
	}
	if _, err = db.ExecContext(ctx, "UPDATE public.supervisor_schema_versions SET sha256='tampered' WHERE version=2"); err != nil {
		t.Fatal(err)
	}
	wantMismatch(t, Apply(ctx, db), "changed applied migration applied")
	wantMismatch(t, Check(ctx, db), "changed applied migration checked")
	if _, err = db.ExecContext(ctx, "UPDATE public.supervisor_schema_versions SET sha256=$1 WHERE version=2", checksum(bootstrapSchema)); err != nil {
		t.Fatal(err)
	}
	t.Run("WIS703_unknown_future_version_after_reserved_gap", func(t *testing.T) {
		future := migrations[len(migrations)-1].version + 1
		if _, err = db.ExecContext(ctx, "INSERT INTO public.supervisor_schema_versions(version,sha256) VALUES($1,'unknown')", future); err != nil {
			t.Fatal(err)
		}
		wantMismatch(t, Apply(ctx, db), "unknown future migration")
		if _, err = db.ExecContext(ctx, "DELETE FROM public.supervisor_schema_versions WHERE version IN(1,$1)", future); err != nil {
			t.Fatal(err)
		}
	})
	wantMismatch(t, Apply(ctx, db), "gapped history")
}

// wantMismatch requires the history guard itself, not an incidental failure, to refuse.
func wantMismatch(t *testing.T, err error, what string) {
	t.Helper()
	if err == nil || err.Error() != "applied migration checksum or sequence mismatch" {
		t.Fatalf("%s: want checksum or sequence mismatch, got %v", what, err)
	}
}
