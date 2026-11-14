//go:build integration

package evidence

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/migrate"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestServiceTxReads covers the WIS-275 transaction-level exports: ReadTx under a service
// actor (no session) and HeadsTx, both as the runtime role inside a tenant-scoped transaction.
func TestServiceTxReads(t *testing.T) {
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
	scope, other := Scope{uuid.NewString(), uuid.NewString()}, Scope{uuid.NewString(), uuid.NewString()}
	for _, sc := range []Scope{scope, other} {
		exec(`INSERT INTO supervisor.tenants(id,name) VALUES($1,'agent tx test')`, sc.TenantID)
		exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'agent tx test')`, sc.TenantID, sc.SiteID)
	}
	owner := Actor{"https://evidence.example/realm", uuid.NewString(), hash([]byte(uuid.NewString()))}
	exec(`INSERT INTO supervisor.browser_sessions(token_hash,issuer,subject,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,'synthetic',now(),now(),now()+interval '1 hour',now())`, owner.SessionHash, owner.Issuer, owner.Subject)
	exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'owner')`, scope.TenantID, scope.SiteID, owner.Issuer, owner.Subject)
	s := New(runtime)
	create := func(text string) Source {
		t.Helper()
		src, e := s.CreateOrRevise(ctx, owner, scope, Input{SourceID: uuid.NewString(), Kind: "manual", Title: "Agent read", Text: text}, uuid.NewString(), 0)
		if e != nil {
			t.Fatal(e)
		}
		return src
	}
	current, revoked := create("Line 2 infeed jam reported at 06:10"), create("Revoked operator note")
	if _, err = s.Revoke(ctx, owner, scope, revoked.SourceID, "withdrawn by owner", uuid.NewString(), revoked.StateRevision); err != nil {
		t.Fatal(err)
	}
	agent := Actor{Issuer: "supervisor:agent", Subject: "investigation-runtime"} // service actor: no session hash

	// in runs fn in one runtime-role transaction carrying tenant's context and commits it, so
	// an audit row written next to an error is retained.
	in := func(tenant string, fn func(*sql.Tx) error) error {
		t.Helper()
		tx, e := runtime.BeginTx(ctx, nil)
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback()
		if _, e = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, tenant); e != nil {
			t.Fatal(e)
		}
		err := fn(tx)
		if e = tx.Commit(); e != nil {
			t.Fatal(e)
		}
		return err
	}
	audits := func(source, reason string) int {
		t.Helper()
		var n int
		if e := admin.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.evidence_audit WHERE tenant_id=$1 AND source_id=$2 AND actor_issuer='supervisor:agent' AND actor_subject='investigation-runtime' AND event_type='source.read' AND reason IS NOT DISTINCT FROM NULLIF($3,'')`, scope.TenantID, source, reason).Scan(&n); e != nil {
			t.Fatal(e)
		}
		return n
	}

	for _, c := range []struct {
		name     string
		tenant   string
		sc       Scope
		id       string
		wantErr  error
		wantText string
		audit    string // the source whose audit row must exist
		reason   string
	}{
		// Control: a current source is returned and its read is audited under the service actor.
		{"current source", scope.TenantID, scope, current.SourceID, nil, "Line 2 infeed jam reported at 06:10", current.SourceID, ""},
		// Revocation: not found, no bytes, and the attempted read is still audited.
		{"revoked source", scope.TenantID, scope, revoked.SourceID, ErrNotFound, "", revoked.SourceID, "EVIDENCE_UNAVAILABLE"},
		// Another tenant's scope sees no head at all: the same not-found, nothing to audit.
		{"cross tenant", other.TenantID, other, current.SourceID, ErrNotFound, "", "", ""},
		// A scope naming tenant A inside tenant B's transaction context is filtered by RLS.
		{"scope and context disagree", other.TenantID, scope, current.SourceID, ErrNotFound, "", "", ""},
	} {
		var got Source
		err := in(c.tenant, func(tx *sql.Tx) error { var e error; got, e = ReadTx(ctx, tx, c.sc, agent, c.id, 0); return e })
		if !errors.Is(err, c.wantErr) || got.Text != c.wantText {
			t.Fatalf("%s: ReadTx = %q, %v; want %q, %v", c.name, got.Text, err, c.wantText, c.wantErr)
		}
		if c.audit != "" && audits(c.audit, c.reason) != 1 {
			t.Fatalf("%s: want one service-actor audit row with reason %q", c.name, c.reason)
		}
	}
	if audits(current.SourceID, "") != 1 {
		t.Fatal("a failed cross-tenant read wrote an audit row against the source")
	}
	if err = in(scope.TenantID, func(tx *sql.Tx) error { _, e := ReadTx(ctx, tx, scope, Actor{}, current.SourceID, 0); return e }); !errors.Is(err, ErrDenied) {
		t.Fatalf("anonymous actor = %v", err)
	}

	// HeadsTx: current and revoked are reported as such; unknown IDs are omitted, not "revoked".
	unknown := uuid.NewString()
	var heads map[string]HeadState
	if err = in(scope.TenantID, func(tx *sql.Tx) error {
		var e error
		heads, e = HeadsTx(ctx, tx, scope, []string{current.SourceID, revoked.SourceID, unknown})
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if h, ok := heads[revoked.SourceID]; !ok || !h.Revoked || h.AccessEpoch != revoked.AccessEpoch+1 {
		t.Fatalf("revoked head = %+v %v", h, ok)
	}
	if h, ok := heads[current.SourceID]; !ok || h.Revoked || h.Revision != 1 || h.AccessEpoch != 1 {
		t.Fatalf("current head = %+v %v", h, ok)
	}
	if _, ok := heads[unknown]; ok || len(heads) != 2 {
		t.Fatalf("unknown ID reported: %+v", heads)
	}
	if err = in(other.TenantID, func(tx *sql.Tx) error {
		var e error
		heads, e = HeadsTx(ctx, tx, other, []string{current.SourceID})
		return e
	}); err != nil || len(heads) != 0 {
		t.Fatalf("cross-tenant heads = %+v %v", heads, err)
	}
}
