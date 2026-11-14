//go:build integration

package knowledge

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/migrate"
	"github.com/google/uuid"
)

// TestLoadSnapshotTxReturnsPinned: the agent runtime must read the snapshot its context
// pinned, not the latest one, and only within its tenant.
func TestLoadSnapshotTxReturnsPinned(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	adminURL, runtimeURL := os.Getenv("SUPERVISOR_TEST_ADMIN_URL"), os.Getenv("SUPERVISOR_TEST_DATABASE_URL")
	if adminURL == "" || runtimeURL == "" {
		t.Fatal("BLOCKED: disposable PostgreSQL admin/runtime URLs required")
	}
	admin, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	runtime, err := sql.Open("pgx", runtimeURL)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if err = migrate.Apply(ctx, admin); err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := admin.ExecContext(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	sc, foreign := Scope{uuid.NewString(), uuid.NewString()}, Scope{uuid.NewString(), uuid.NewString()}
	for _, s := range []Scope{sc, foreign} {
		exec(`INSERT INTO supervisor.tenants(id,name) VALUES($1,'pinned snapshot')`, s.TenantID)
		exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'pinned snapshot')`, s.TenantID, s.SiteID)
	}
	owner := Actor{"https://snapshot.invalid", uuid.NewString(), strings.ReplaceAll(uuid.NewString(), "-", "") + strings.ReplaceAll(uuid.NewString(), "-", "")}
	exec(`INSERT INTO supervisor.browser_sessions(token_hash,issuer,subject,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,'fixture',now(),now(),now()+interval '1 hour',now())`, owner.SessionHash, owner.Issuer, owner.Subject)
	exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'owner')`, sc.TenantID, sc.SiteID, owner.Issuer, owner.Subject)
	service := New(runtime)
	inventory := func() int64 {
		t.Helper()
		v, e := service.ReadInventory(ctx, owner, sc)
		if e != nil {
			t.Fatal(e)
		}
		return v.Revision
	}
	snapshot := func(reason string) Snapshot {
		t.Helper()
		out, e := service.CreateSnapshot(ctx, owner, sc, SnapshotInput{SnapshotID: uuid.NewString(), Reason: reason}, uuid.NewString(), inventory())
		if e != nil {
			t.Fatal(e)
		}
		return out
	}
	pinned := snapshot("Pinned by the plant context")
	// The approved inventory moves on (the superuser stands in for an approval), then a later
	// snapshot is captured.
	exec(`INSERT INTO supervisor.knowledge_inventory_revisions(tenant_id,site_id,revision) VALUES($1,$2,1) ON CONFLICT(tenant_id,site_id) DO UPDATE SET revision=supervisor.knowledge_inventory_revisions.revision+1`, sc.TenantID, sc.SiteID)
	latest := snapshot("Captured after the context")
	if latest.InventoryRevision == pinned.InventoryRevision {
		t.Fatal("fixture did not advance the inventory")
	}

	load := func(tenant string, s Scope, id string) (Snapshot, error) {
		t.Helper()
		tx, e := runtime.BeginTx(ctx, nil)
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback()
		if _, e = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, tenant); e != nil {
			t.Fatal(e)
		}
		return LoadSnapshotTx(ctx, tx, s, id)
	}
	got, err := load(sc.TenantID, sc, pinned.SnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SnapshotID != pinned.SnapshotID || got.Reason != pinned.Reason || got.InventoryRevision != pinned.InventoryRevision || got.SHA256 != pinned.SHA256 ||
		!got.Stale || got.CurrentInventoryRevision != latest.InventoryRevision {
		t.Fatalf("LoadSnapshotTx returned %+v, want the pinned snapshot %s (stale against %d)", got, pinned.SnapshotID, latest.InventoryRevision)
	}
	for _, c := range []struct {
		name   string
		tenant string
		scope  Scope
		id     string
		want   error
	}{
		{"unknown snapshot", sc.TenantID, sc, uuid.NewString(), ErrNotFound},
		{"foreign tenant", foreign.TenantID, foreign, pinned.SnapshotID, ErrNotFound},
		{"scope and context disagree", foreign.TenantID, sc, pinned.SnapshotID, ErrNotFound},
		{"malformed id", sc.TenantID, sc, "latest", ErrInvalidContract},
	} {
		if _, e := load(c.tenant, c.scope, c.id); !errors.Is(e, c.want) {
			t.Fatalf("%s: %v, want %v", c.name, e, c.want)
		}
	}
}
