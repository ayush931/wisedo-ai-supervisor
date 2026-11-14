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

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/approvals"
	"github.com/google/uuid"
)

// approvalCase is one open case and the scope it lives in.
type approvalCase struct {
	tenant, site, caseID, line, order, contextID, digest string
}

// seedApprovalCase inserts a site and one open case with the rows it references as the
// superuser with FK triggers off: a fully derived plant context needs the whole WIS-272
// pipeline. Everything the test then does to the approval tables runs as the runtime role.
func seedApprovalCase(t *testing.T, admin *sql.DB) approvalCase {
	t.Helper()
	c := approvalCase{caseID: uuid.NewString(), line: uuid.NewString(), order: uuid.NewString(), contextID: uuid.NewString(), digest: strings.Repeat("a", 64)}
	c.tenant, c.site = createSite(t, admin)
	tx, err := admin.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	setTenant(t, tx, c.tenant)
	window := time.Date(2026, 10, 16, 6, 0, 0, 0, time.UTC)
	snapshot := `{"readiness":"NOT_READY","digest":"` + c.digest + `","context_id":"` + c.contextID + `","order_id":"` + c.order + `","asset_id":"` + c.line + `","site_id":"` + c.site + `"}`
	for _, s := range []struct {
		q    string
		args []any
	}{
		{`SET LOCAL session_replication_role = replica`, nil},
		{`INSERT INTO supervisor.knowledge_entity_heads(tenant_id,site_id,id,current_revision) VALUES($1,$2,$3,1),($1,$2,$4,1)`, []any{c.tenant, c.site, c.line, c.order}},
		{`INSERT INTO supervisor.plant_contexts(tenant_id,site_id,id,order_id,asset_id,window_start,window_end,process_snapshot_id,process_snapshot_sha256,readiness,snapshot,digest,actor_issuer,actor_subject,evaluated_at)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,'NOT_READY',$10,$11,'test','operator',$7)`, []any{c.tenant, c.site, c.contextID, c.order, c.line, window, window.Add(15 * time.Minute), uuid.NewString(), strings.Repeat("d", 64), snapshot, c.digest}},
		{`INSERT INTO supervisor.cases(tenant_id,site_id,id,line_id,order_id,exception_kind,episode,revision,context_snapshot_id,context_revision,context_digest,state,detected_at,latest_window_end,workflow_id)
 VALUES($1,$2,$3,$4,$5,'production_shortfall',1,3,$6,1,$7,'awaiting_approval',$8,$9,$10)`, []any{c.tenant, c.site, c.caseID, c.line, c.order, c.contextID, c.digest, window, window.Add(15 * time.Minute), "case:" + c.tenant + ":" + c.caseID}},
	} {
		if _, err = tx.Exec(s.q, s.args...); err != nil {
			t.Fatalf("%s: %v", s.q, err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return c
}

// canonicalRevision builds the server's canonical ApprovedActionV1 envelope for revision 1
// of proposal id against c.
func canonicalRevision(t *testing.T, c approvalCase, id string, revision int64, issued time.Time) (string, string) {
	t.Helper()
	p := approvals.Payload{
		ActionKind: "maintenance_investigation.create", Template: "inspect_report_only",
		ProposalID: id, ProposalRevision: revision,
		AssetID: c.line, LineID: c.line, OrderID: c.order, AssigneeID: uuid.NewString(),
		Instructions: "Inspect the infeed and report findings.", DueAt: issued.Add(4 * time.Hour),
		CaseID: c.caseID, CaseRevision: 3, ContextSnapshotID: c.contextID, ContextRevision: 1, ContextDigest: c.digest,
		ConfigRevision: 1, GraphRevision: 1, PolicyRevision: 1,
		Evidence: []approvals.Ref{{Kind: "plant_context", ID: c.contextID, Revision: 1}},
		IssuedAt: issued, ExpiresAt: issued.Add(approvals.ApprovalTTL), GeneratedByRun: uuid.NewString(),
	}
	body, digest, err := approvals.Canonical(c.tenant, c.site, p, approvals.WindowProfile{})
	if err != nil {
		t.Fatal(err)
	}
	return string(body), digest
}

// TestProposalApprovalsUpgradeFrom043AndGuards upgrades a populated 043 database to 044 and
// proves, as the runtime role, the storage guards WIS-274 requires: the canonical digest is
// re-checked, a foreign-tenant case cannot be attached, the state machine has no path back
// to pending, a reviewer is counted once per revision, and history is immutable.
func TestProposalApprovalsUpgradeFrom043AndGuards(t *testing.T) {
	raw := os.Getenv("SUPERVISOR_TEST_ADMIN_URL")
	if raw == "" {
		t.Fatal("BLOCKED: disposable PostgreSQL URL required")
	}
	root, err := sql.Open("pgx", raw)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	name := "approvals_migration_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = root.Exec("CREATE DATABASE " + name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, e := root.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)"); e != nil {
			t.Error(e)
		}
	})
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	admin, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	ctx := context.Background()
	if err = applyThrough(ctx, admin, 43); err != nil {
		t.Fatal(err)
	}
	c := seedApprovalCase(t, admin) // populated before 044
	if err = Apply(ctx, admin); err != nil {
		t.Fatalf("upgrade 043 -> 044: %v", err)
	}
	if err = Check(ctx, admin); err != nil {
		t.Fatal(err)
	}
	assertTenantTablesForced(t, admin)
	for _, table := range []string{"proposal_revisions", "approval_decisions", "approval_transitions", "approval_receipts", "approval_audit"} {
		assertGrants(t, admin, appendOnly, table)
	}
	var updateColumns string
	if err = admin.QueryRow(`SELECT string_agg(column_name,',' ORDER BY column_name) FROM information_schema.column_privileges WHERE grantee='supervisor_runtime' AND table_schema='supervisor' AND table_name='approval_states' AND privilege_type='UPDATE'`).Scan(&updateColumns); err != nil || updateColumns != "current_revision,state,state_revision,updated_at" {
		t.Fatalf("approval_states runtime UPDATE columns = %q (%v)", updateColumns, err)
	}

	runtime := runtimeDB(t, admin)
	as := func(tenant string, statements ...string) func(args ...[]any) error {
		return func(args ...[]any) error {
			tx, e := runtime.Begin()
			if e != nil {
				t.Fatal(e)
			}
			defer tx.Rollback()
			setTenant(t, tx, tenant)
			for i, s := range statements {
				if _, e = tx.Exec(s, args[i]...); e != nil {
					return e
				}
			}
			return tx.Commit()
		}
	}
	insertRevision := `INSERT INTO supervisor.proposal_revisions(tenant_id,site_id,proposal_id,revision,case_id,case_revision,context_snapshot_id,generated_by_run_id,issued_at,expires_at,input_sha256,canonical_payload,canonical_payload_sha256) VALUES($1,$2,$3,$4,$5,3,$6,$7,$8,$9,$10,$11,$12)`
	issued := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	proposal := uuid.NewString()
	body, digest := canonicalRevision(t, c, proposal, 1, issued)
	run := func(body string) string {
		var v string
		if e := admin.QueryRow(`SELECT $1::jsonb->'body'->>'generated_by_run_id'`, body).Scan(&v); e != nil {
			t.Fatal(e)
		}
		return v
	}
	revisionArgs := func(tenant, site, id, body, digest string) []any {
		return []any{tenant, site, id, 1, c.caseID, c.contextID, run(body), issued, issued.Add(approvals.ApprovalTTL), strings.Repeat("f", 64), body, digest}
	}

	// A digest that does not hash the stored bytes is refused.
	err = as(c.tenant, insertRevision)(revisionArgs(c.tenant, c.site, proposal, body, strings.Repeat("0", 64)))
	wantState(t, err, "23514", "digest mismatch")

	// A tenant cannot attach a proposal to another tenant's case: the composite FK has no row.
	other, otherSite := createSite(t, admin)
	err = as(other, insertRevision)(revisionArgs(other, otherSite, proposal, body, digest))
	wantState(t, err, "23514", "envelope bound to another tenant") // the envelope names c.tenant
	foreignBody, foreignDigest := canonicalRevision(t, approvalCase{tenant: other, site: otherSite, caseID: c.caseID, line: c.line, order: c.order, contextID: c.contextID, digest: c.digest}, proposal, 1, issued)
	err = as(other, insertRevision)(revisionArgs(other, otherSite, proposal, foreignBody, foreignDigest))
	wantState(t, err, "23503", "foreign-tenant case")

	// A valid revision and its pending head.
	insertHead := `INSERT INTO supervisor.approval_states(tenant_id,site_id,proposal_id,current_revision,state,state_revision) VALUES($1,$2,$3,1,'pending',1)`
	if err = as(c.tenant, insertRevision, insertHead)(revisionArgs(c.tenant, c.site, proposal, body, digest), []any{c.tenant, c.site, proposal}); err != nil {
		t.Fatal(err)
	}

	// One review per reviewer per proposal revision; a revocation is not a review.
	decide := `INSERT INTO supervisor.approval_decisions(tenant_id,site_id,id,proposal_id,proposal_revision,canonical_payload_sha256,decided_state_revision,decision,reason,reviewer_issuer,reviewer_subject,auth_time) VALUES($1,$2,$3,$4,1,$5,$6,$7,'checked','https://idp.invalid',$8,clock_timestamp())`
	if err = as(c.tenant, decide)([]any{c.tenant, c.site, uuid.NewString(), proposal, digest, 2, "approve", "reviewer-1"}); err != nil {
		t.Fatal(err)
	}
	err = as(c.tenant, decide)([]any{c.tenant, c.site, uuid.NewString(), proposal, digest, 2, "reject", "reviewer-1"})
	wantState(t, err, "23505", "same reviewer counted twice")
	if err = as(c.tenant, decide)([]any{c.tenant, c.site, uuid.NewString(), proposal, digest, 3, "revoke", "reviewer-1"}); err != nil {
		t.Fatalf("approver revoking own approval: %v", err)
	}
	// A decision must cite the exact digest of the revision it decides.
	err = as(c.tenant, decide)([]any{c.tenant, c.site, uuid.NewString(), proposal, strings.Repeat("1", 64), 2, "approve", "reviewer-2"})
	wantState(t, err, "23503", "decision over a different digest")

	// The head follows the state machine, one revision at a time, never back to pending.
	move := `UPDATE supervisor.approval_states SET state=$4,state_revision=$5 WHERE tenant_id=$1 AND site_id=$2 AND proposal_id=$3`
	for _, step := range []struct {
		state    string
		revision int
		want     string
	}{
		{"approved", 3, "23514"}, // skips a state revision
		{"consumed", 2, "23514"}, // pending -> consumed is not a transition
		{"approved", 2, ""},      // pending -> approved
		{"pending", 3, "23514"},  // nothing returns to pending
		{"rejected", 3, "23514"}, // approved -> rejected is not a transition
		{"revoked", 3, ""},       // approved -> revoked
		{"approved", 4, "23514"}, // revoked is terminal
	} {
		err = as(c.tenant, move)([]any{c.tenant, c.site, proposal, step.state, step.revision})
		if step.want == "" && err != nil {
			t.Fatalf("-> %s: %v", step.state, err)
		}
		if step.want != "" {
			wantState(t, err, step.want, "-> "+step.state)
		}
	}
	// A revoked proposal cannot be edited into a new pending revision either.
	err = as(c.tenant, `UPDATE supervisor.approval_states SET current_revision=2,state='pending',state_revision=4 WHERE tenant_id=$1 AND site_id=$2 AND proposal_id=$3`)([]any{c.tenant, c.site, proposal})
	wantState(t, err, "23514", "new revision after revocation") // FK to revision 2 is checked after the guard

	// History is immutable to the runtime role; heads cannot be deleted.
	for _, s := range []string{
		`UPDATE supervisor.approval_decisions SET reason='rewritten' WHERE tenant_id=$1 AND site_id=$2`,
		`UPDATE supervisor.proposal_revisions SET expires_at=expires_at+interval '1 hour' WHERE tenant_id=$1 AND site_id=$2`,
	} {
		err = as(c.tenant, s)([]any{c.tenant, c.site})
		wantState(t, err, "42501", s)
	}
	err = as(c.tenant, `DELETE FROM supervisor.approval_states WHERE tenant_id=$1 AND site_id=$2`)([]any{c.tenant, c.site})
	wantState(t, err, "42501", "delete head")

	// RLS: another tenant sees none of it.
	var visible int
	tx, err := runtime.Begin()
	if err != nil {
		t.Fatal(err)
	}
	setTenant(t, tx, other)
	if err = tx.QueryRow(`SELECT (SELECT count(*) FROM supervisor.proposal_revisions)+(SELECT count(*) FROM supervisor.approval_states)+(SELECT count(*) FROM supervisor.approval_decisions)`).Scan(&visible); err != nil || visible != 0 {
		t.Fatalf("foreign tenant sees %d approval rows (%v)", visible, err)
	}
	_ = tx.Rollback()
}
