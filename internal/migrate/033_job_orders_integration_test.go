//go:build integration

package migrate

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/canonical"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestJobOrdersMigrationCatalog(t *testing.T) {
	db := newOperationalMigrationDB(t)
	ctx := context.Background()
	if err := Check(ctx, db); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"job_order_heads", "job_order_revisions", "job_order_receipts", "job_order_audit", "shift_plan_assignments"} {
		var protected, nonOwner, canSelect, canInsert, canDelete, canUpdate bool
		var policies int
		err := db.QueryRowContext(ctx, `SELECT c.relrowsecurity AND c.relforcerowsecurity,c.relowner<>r.oid,has_table_privilege('supervisor_runtime',c.oid,'SELECT'),has_table_privilege('supervisor_runtime',c.oid,'INSERT'),has_table_privilege('supervisor_runtime',c.oid,'DELETE'),has_table_privilege('supervisor_runtime',c.oid,'UPDATE'),(SELECT count(*) FROM pg_policies p WHERE p.schemaname='supervisor' AND p.tablename=c.relname AND p.policyname='tenant_scope') FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace CROSS JOIN pg_roles r WHERE n.nspname='supervisor' AND c.relname=$1 AND r.rolname='supervisor_runtime'`, table).Scan(&protected, &nonOwner, &canSelect, &canInsert, &canDelete, &canUpdate, &policies)
		if err != nil {
			t.Fatalf("%s: %v", table, err)
		}
		// Assignments are derived by trigger only: the runtime may read but never insert them.
		wantInsert := table != "shift_plan_assignments"
		if !protected || !nonOwner || !canSelect || canInsert != wantInsert || canDelete || canUpdate || policies != 1 {
			t.Errorf("%s RLS/grant invariant: protected=%v nonowner=%v select=%v insert=%v delete=%v update=%v policies=%d", table, protected, nonOwner, canSelect, canInsert, canDelete, canUpdate, policies)
		}
	}
	var version, headUpdateColumns int
	var deriveExecutable bool
	if err := db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM public.supervisor_schema_versions WHERE version=33),(SELECT count(*) FROM information_schema.column_privileges WHERE grantee='supervisor_runtime' AND table_schema='supervisor' AND table_name='job_order_heads' AND privilege_type='UPDATE'),has_function_privilege('supervisor_runtime','supervisor.derive_shift_plan_assignments()','EXECUTE')`).Scan(&version, &headUpdateColumns, &deriveExecutable); err != nil {
		t.Fatal(err)
	}
	if version != 1 || headUpdateColumns != 1 || deriveExecutable {
		t.Fatalf("migration invariants: version=%d head update columns=%d derive executable=%v", version, headUpdateColumns, deriveExecutable)
	}
}

// TestJobOrderGuardsPostgres drives the 033 guards directly: the instruction foreign key,
// append-only job-order heads, derived plan assignments, and the extended approval guard
// that refuses a stale job-order pin or a workstation double-booked across lines.
func TestJobOrderGuardsPostgres(t *testing.T) {
	db := newOperationalMigrationDB(t)
	ctx := context.Background()
	tenant, site := createSite(t, db)
	// run executes statements in one tenant-scoped transaction and returns the first error,
	// including deferred constraint errors raised at commit.
	run := func(statements ...func(*sql.Tx) error) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		setTenant(t, tx, tenant)
		for _, s := range statements {
			if err = s(tx); err != nil {
				return err
			}
		}
		return tx.Commit()
	}
	exec := func(q string, args ...any) func(*sql.Tx) error {
		return func(tx *sql.Tx) error { _, err := tx.ExecContext(ctx, q, args...); return err }
	}
	ok := func(name string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	refused := func(name, code, message string, err error) {
		t.Helper()
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != code || !strings.Contains(pgErr.Message, message) {
			t.Fatalf("%s: want %s %q, got %v", name, code, message, err)
		}
	}
	envelope := func(schema string, body map[string]any) (string, string) {
		t.Helper()
		encoded, digest, err := canonical.Envelope(schema, 1, tenant, site, body)
		if err != nil {
			t.Fatal(err)
		}
		return string(encoded), digest
	}
	payload := func(body map[string]any) string {
		t.Helper()
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	source, text := uuid.NewString(), "Synthetic SOP revision one"
	sum := sha256.Sum256([]byte(text))
	sha := hex.EncodeToString(sum[:])
	ok("seed instruction", run(
		exec(`INSERT INTO supervisor.evidence_heads(tenant_id,site_id,source_id,current_revision,state_revision) VALUES($1,$2,$3,1,1)`, tenant, site, source),
		exec(`INSERT INTO supervisor.evidence_revisions(tenant_id,site_id,source_id,revision,kind,title,body,sha256,byte_length,rune_length,actor_issuer,actor_subject) VALUES($1,$2,$3,1,'document','SOP',$4,$5,$6,$6,'i','owner')`, tenant, site, source, text, sha, len(text)),
	))
	orderRevision := func(id string, revision int64, instructionSHA string) func(*sql.Tx) error {
		body := map[string]any{"schema_version": 1, "item_code": "P-100", "target_quantity": 100, "instruction_document_id": source, "instruction_revision": 1, "instruction_sha256": instructionSHA}
		encoded, digest := envelope("JobOrderBodyV1", body)
		return exec(`INSERT INTO supervisor.job_order_revisions(tenant_id,site_id,job_order_id,revision,payload,canonical_payload,canonical_payload_sha256,digest_schema_version,instruction_source_id,instruction_revision,instruction_sha256,actor_issuer,actor_subject) VALUES($1,$2,$3,$4,$5::jsonb,$6,$7,1,$8,1,$9,'i','lead')`, tenant, site, id, revision, payload(body), encoded, digest, source, instructionSHA)
	}
	createOrder := func(id, instructionSHA string) error {
		return run(exec(`INSERT INTO supervisor.job_order_heads(tenant_id,site_id,id,current_revision) VALUES($1,$2,$3,1)`, tenant, site, id), orderRevision(id, 1, instructionSHA))
	}
	reviseOrder := func(id string, next int64) error {
		return run(exec(`UPDATE supervisor.job_order_heads SET current_revision=$4 WHERE tenant_id=$1 AND site_id=$2 AND id=$3`, tenant, site, id, next), orderRevision(id, next, sha))
	}

	// The instruction must be an exact stored evidence revision with its content hash.
	refused("instruction hash mismatch", "23503", "job_order_instruction_fk", createOrder(uuid.NewString(), strings.Repeat("0", 64)))
	j1, j2, j3 := uuid.NewString(), uuid.NewString(), uuid.NewString()
	for _, id := range []string{j1, j2, j3} {
		ok("create job order", createOrder(id, sha))
	}
	refused("skipped job order revision", "23514", "Invalid job order head transition", reviseOrder(j1, 3))
	refused("rewrote job order history", "42501", "Evidence history is immutable", run(exec(`UPDATE supervisor.job_order_revisions SET actor_subject='x' WHERE job_order_id=$1`, j1)))

	type pin struct {
		station, order string
		revision       int64
	}
	planRevision := func(plan, line string, revision int64, start, end string, pins ...pin) func(*sql.Tx) error {
		assignments := []any{}
		for _, p := range pins {
			assignments = append(assignments, map[string]any{"workstation_id": p.station, "job_order_id": p.order, "job_order_revision": p.revision})
		}
		body := map[string]any{"schema_version": 1, "line_id": line, "starts_at": start, "ends_at": end, "timezone": "UTC", "assignments": assignments}
		encoded, digest := envelope("ShiftPlanBodyV1", body)
		return exec(`INSERT INTO supervisor.shift_plan_revisions(tenant_id,site_id,plan_id,revision,line_id,starts_at,ends_at,timezone,payload,canonical_payload,canonical_payload_sha256,digest_schema_version,actor_issuer,actor_subject) VALUES($1,$2,$3,$4,$5,$6,$7,'UTC',$8::jsonb,$9,$10,1,'i','lead')`, tenant, site, plan, revision, line, start, end, payload(body), encoded, digest)
	}
	createPlan := func(line, start, end string, pins ...pin) (string, error) {
		plan := uuid.NewString()
		return plan, run(exec(`INSERT INTO supervisor.shift_plan_heads(tenant_id,site_id,id,line_id,starts_at,ends_at,current_revision,state_revision) VALUES($1,$2,$3,$4,$5,$6,1,1)`, tenant, site, plan, line, start, end), planRevision(plan, line, 1, start, end, pins...))
	}
	approve := func(plan string, revision int64) error {
		return run(
			exec(`INSERT INTO supervisor.shift_plan_approvals(tenant_id,site_id,plan_id,revision,canonical_payload_sha256,approver_issuer,approver_subject,policy_revision,auth_time) SELECT tenant_id,site_id,plan_id,revision,canonical_payload_sha256,'i','approver',1,now() FROM supervisor.shift_plan_revisions WHERE plan_id=$1 AND revision=$2`, plan, revision),
			exec(`UPDATE supervisor.shift_plan_heads SET approved_revision=current_revision,state_revision=state_revision+1 WHERE id=$1`, plan),
		)
	}
	lineA, lineB, w1, w2 := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()

	// Assignments are derived from the revision payload, row for row.
	p1, err := createPlan(lineA, "2026-10-20T04:00:00Z", "2026-10-20T12:00:00Z", pin{w1, j1, 1}, pin{w2, j2, 1})
	ok("create plan", err)
	rows, err := db.QueryContext(ctx, `SELECT workstation_id::text||'/'||job_order_id::text||'/'||job_order_revision FROM supervisor.shift_plan_assignments WHERE plan_id=$1 AND revision=1 ORDER BY 1`, p1)
	ok("read assignments", err)
	derived := []string{}
	for rows.Next() {
		var s string
		ok("scan assignment", rows.Scan(&s))
		derived = append(derived, s)
	}
	rows.Close()
	want := []string{w1 + "/" + j1 + "/1", w2 + "/" + j2 + "/1"}
	if want[0] > want[1] {
		want[0], want[1] = want[1], want[0]
	}
	if strings.Join(derived, ",") != strings.Join(want, ",") {
		t.Fatalf("derived assignments %v want %v", derived, want)
	}
	// A plan cannot pin a job-order revision that does not exist.
	_, err = createPlan(uuid.NewString(), "2026-10-21T04:00:00Z", "2026-10-21T12:00:00Z", pin{w1, j1, 9})
	refused("unknown job order pin", "23503", "shift_plan_assignment_job_order_fk", err)
	refused("rewrote an assignment", "42501", "Evidence history is immutable", run(exec(`UPDATE supervisor.shift_plan_assignments SET job_order_revision=1 WHERE plan_id=$1`, p1)))

	// Revising a pinned job order makes the pin stale: the plan can no longer be approved.
	ok("revise job order", reviseOrder(j1, 2))
	refused("approved a stale pin", "23514", "pins a superseded job order revision", approve(p1, 1))
	ok("re-pin plan", run(exec(`UPDATE supervisor.shift_plan_heads SET current_revision=2,state_revision=2 WHERE id=$1`, p1), planRevision(p1, lineA, 2, "2026-10-20T04:00:00Z", "2026-10-20T12:00:00Z", pin{w1, j1, 2}, pin{w2, j2, 1})))
	ok("approve re-pinned plan", approve(p1, 2))

	// Another line's overlapping shift cannot also book w1; an adjacent shift can.
	p3, err := createPlan(lineB, "2026-10-20T08:00:00Z", "2026-10-20T16:00:00Z", pin{w1, j3, 1})
	ok("create overlapping plan", err)
	refused("double-booked workstation", "23P01", "Workstation is assigned in an overlapping approved shift plan", approve(p3, 1))
	p4, err := createPlan(uuid.NewString(), "2026-10-20T12:00:00Z", "2026-10-20T20:00:00Z", pin{w1, j3, 1})
	ok("create adjacent plan", err)
	ok("approve adjacent plan", approve(p4, 1))
}
