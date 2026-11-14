//go:build integration

package migrate

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestSiteMasterDataMigrationCatalog(t *testing.T) {
	if len(migrations) < 52 || migrations[51].version != 52 || migrations[51].sql != siteMasterDataSchema {
		t.Fatal("site master-data migration 052 moved or changed in the immutable catalog")
	}
	if checksum(siteMasterDataSchema) == "" || !strings.Contains(siteMasterDataSchema, "CREATE TABLE supervisor.master_data_receipts") {
		t.Fatal("migration 052 body is missing or has no content")
	}
}

func TestSiteMasterDataMigrationUpgradeFrom051(t *testing.T) {
	raw := os.Getenv("SUPERVISOR_TEST_ADMIN_URL")
	if raw == "" {
		t.Fatal("BLOCKED: disposable PostgreSQL admin URL required")
	}
	admin, err := sql.Open("pgx", raw)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	name := "master_data_upgrade_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = admin.ExecContext(context.Background(), "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if _, e := admin.ExecContext(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); e != nil {
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
	if err = applyThrough(ctx, db, 51); err != nil {
		t.Fatalf("build schema 051: %v", err)
	}
	if err = Apply(ctx, db); err != nil {
		t.Fatalf("upgrade schema 051 to current: %v", err)
	}
	if err = Check(ctx, db); err != nil {
		t.Fatal(err)
	}

	tables := []string{"line_heads", "line_revisions", "item_heads", "item_revisions", "routing_heads", "routing_revisions", "routing_steps", "shift_calendar_heads", "shift_calendar_revisions", "site_master_data_setting_heads", "site_master_data_setting_revisions", "master_data_receipts"}
	for _, table := range tables {
		var forced bool
		var policies int
		if err = db.QueryRowContext(ctx, `SELECT c.relrowsecurity AND c.relforcerowsecurity,(SELECT count(*) FROM pg_policy p WHERE p.polrelid=c.oid) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='supervisor' AND c.relname=$1`, table).Scan(&forced, &policies); err != nil {
			t.Fatal(err)
		}
		if !forced || policies != 1 {
			t.Errorf("%s forced-RLS=%v policies=%d, want 1 each", table, forced, policies)
		}
		var selectOK, insertOK, deleteOK bool
		if err = db.QueryRowContext(ctx, `SELECT has_table_privilege('supervisor_runtime','supervisor.'||$1,'SELECT'),has_table_privilege('supervisor_runtime','supervisor.'||$1,'INSERT'),has_table_privilege('supervisor_runtime','supervisor.'||$1,'DELETE')`, table).Scan(&selectOK, &insertOK, &deleteOK); err != nil {
			t.Fatal(err)
		}
		if !selectOK || !insertOK || deleteOK {
			t.Errorf("%s grants: select=%v insert=%v delete=%v", table, selectOK, insertOK, deleteOK)
		}
	}
	for _, table := range []string{"line_revisions", "item_revisions", "routing_revisions", "routing_steps", "shift_calendar_revisions", "site_master_data_setting_revisions", "master_data_receipts"} {
		var immutable bool
		if err = db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_trigger t JOIN pg_class c ON c.oid=t.tgrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='supervisor' AND c.relname=$1 AND t.tgname=$2 AND NOT t.tgisinternal)`, table, table+"_immutable").Scan(&immutable); err != nil {
			t.Fatal(err)
		}
		if !immutable {
			t.Errorf("%s is missing its immutable trigger", table)
		}
	}
	for _, table := range []string{"line_heads", "item_heads", "routing_heads", "shift_calendar_heads", "site_master_data_setting_heads"} {
		var pointer, identity bool
		if err = db.QueryRowContext(ctx, `SELECT has_column_privilege('supervisor_runtime','supervisor.'||$1,'current_revision','UPDATE'),has_column_privilege('supervisor_runtime','supervisor.'||$1,'tenant_id','UPDATE')`, table).Scan(&pointer, &identity); err != nil {
			t.Fatal(err)
		}
		if !pointer || identity {
			t.Errorf("%s update grants: pointer=%v identity=%v", table, pointer, identity)
		}
	}
	var compositeFKs, settingsRows int
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM pg_constraint c JOIN pg_class t ON t.oid=c.conrelid JOIN pg_namespace n ON n.oid=t.relnamespace WHERE n.nspname='supervisor' AND t.relname IN ('routing_revisions','routing_steps') AND c.contype='f' AND cardinality(c.conkey)=3`).Scan(&compositeFKs); err != nil {
		t.Fatal(err)
	}
	if compositeFKs < 3 {
		t.Errorf("found %d three-column master-data FKs, want line, item and workstation site fences", compositeFKs)
	}
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.site_master_data_setting_heads`).Scan(&settingsRows); err != nil {
		t.Fatal(err)
	}
	if settingsRows != 0 {
		t.Errorf("migration created %d implicit setting rows", settingsRows)
	}
}
