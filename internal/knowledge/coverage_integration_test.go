//go:build integration

package knowledge

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/evidence"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/migrate"
	"github.com/google/uuid"
)

func TestPostgresDiscoveryCoverage(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	adminURL, runtimeURL := os.Getenv("SUPERVISOR_TEST_ADMIN_URL"), os.Getenv("SUPERVISOR_TEST_DATABASE_URL")
	if adminURL == "" || runtimeURL == "" {
		t.Fatal("real PostgreSQL URLs required")
	}
	control, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { control.Close() })
	database := "coverage_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = control.ExecContext(ctx, `CREATE DATABASE `+database); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 60*time.Second)
		defer stop()
		if _, e := control.ExecContext(cleanup, `DROP DATABASE IF EXISTS `+database+` WITH (FORCE)`); e != nil {
			t.Error(e)
		}
	})
	connect := func(raw string) *sql.DB {
		t.Helper()
		u, e := url.Parse(raw)
		if e != nil {
			t.Fatal(e)
		}
		u.Path = "/" + database
		db, e := sql.Open("pgx", u.String())
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { db.Close() })
		return db
	}
	admin, runtime := connect(adminURL), connect(runtimeURL)
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
		exec(`INSERT INTO supervisor.tenants(id,name) VALUES($1,'requirement synthetic')`, s.TenantID)
		exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'requirement synthetic')`, s.TenantID, s.SiteID)
	}
	actor := func(role string) Actor {
		t.Helper()
		a := Actor{"https://requirement.invalid", uuid.NewString(), strings.ReplaceAll(uuid.NewString(), "-", "") + strings.ReplaceAll(uuid.NewString(), "-", "")}
		exec(`INSERT INTO supervisor.browser_sessions(token_hash,issuer,subject,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,'fixture',now(),now(),now()+interval '1 hour',now())`, a.SessionHash, a.Issuer, a.Subject)
		exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,$5)`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, role)
		return a
	}
	owner, supervisor, operator, viewer := actor("owner"), actor("supervisor"), actor("operator"), actor("viewer")
	exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'owner')`, foreign.TenantID, foreign.SiteID, owner.Issuer, owner.Subject)
	service, ev := New(runtime), evidence.New(runtime)
	ea := evidence.Actor{Issuer: owner.Issuer, Subject: owner.Subject, SessionHash: owner.SessionHash}
	es := evidence.Scope{TenantID: sc.TenantID, SiteID: sc.SiteID}
	source := func() evidence.Input {
		t.Helper()
		in := evidence.Input{SourceID: uuid.NewString(), Kind: "manual", Title: "Resolution source", Text: "Synthetic conflict evidence"}
		if _, e := ev.CreateOrRevise(ctx, ea, es, in, uuid.NewString(), 0); e != nil {
			t.Fatal(e)
		}
		return in
	}
	origin, report := source(), source()
	entities := []EntityInput{
		{EntityID: uuid.NewString(), Type: "area", Name: "Area", SourceID: origin.SourceID, SourceRevision: 1},
		{EntityID: uuid.NewString(), Type: "process", Name: "Process", SourceID: origin.SourceID, SourceRevision: 1},
		{EntityID: uuid.NewString(), Type: "role", Name: "Responsible role", SourceID: report.SourceID, SourceRevision: 1},
	}
	for _, v := range entities {
		if _, e := service.CreateEntity(ctx, owner, sc, v, uuid.NewString(), 0); e != nil {
			t.Fatal(e)
		}
	}
	span, e := ev.CreateSpan(ctx, ea, es, evidence.SpanInput{SourceID: report.SourceID, Revision: 1, Start: 0, End: 9}, uuid.NewString(), 1)
	if e != nil {
		t.Fatal(e)
	}
	claim := ClaimInput{ClaimID: uuid.NewString(), SubjectID: entities[1].EntityID, SubjectRevision: 1, Predicate: "responsible_role", ObjectEntityID: entities[2].EntityID, ObjectRevision: 1, EffectiveFrom: "2026-09-27T00:00:00Z", EvidenceClass: "report", SourceSpanIDs: []string{span.ID}}
	if _, e = service.CreateClaim(ctx, owner, sc, claim, uuid.NewString(), 0); e != nil {
		t.Fatal(e)
	}
	if _, e = ev.DecideAuthority(ctx, ea, es, report.SourceID, evidence.AuthorityInput{SourceRevision: 1, ExpectedSourceStateRevision: 1, Verdict: "designate", Reason: "Verified role account"}, uuid.NewString(), 0); e != nil {
		t.Fatal(e)
	}
	if _, e = service.DecideReview(ctx, supervisor, sc, claim.ClaimID, ReviewInput{ClaimRevision: 1, Verdict: "approve", Reason: "Reviewed role assignment"}, uuid.NewString(), 1); e != nil {
		t.Fatal(e)
	}
	bindingRefs := []ConflictClaimRef{{ClaimID: claim.ClaimID, Revision: 1}}
	for i := 0; i < 2; i++ {
		role := EntityInput{EntityID: uuid.NewString(), Type: "role", Name: "Additional responsible role", SourceID: report.SourceID, SourceRevision: 1}
		if _, e = service.CreateEntity(ctx, owner, sc, role, uuid.NewString(), 0); e != nil {
			t.Fatal(e)
		}
		additional := claim
		additional.ClaimID, additional.ObjectEntityID = uuid.NewString(), role.EntityID
		if _, e = service.CreateClaim(ctx, owner, sc, additional, uuid.NewString(), 0); e != nil {
			t.Fatal(e)
		}
		if _, e = service.DecideReview(ctx, supervisor, sc, additional.ClaimID, ReviewInput{ClaimRevision: 1, Verdict: "approve", Reason: "Reviewed additional role"}, uuid.NewString(), 1); e != nil {
			t.Fatal(e)
		}
		bindingRefs = append(bindingRefs, ConflictClaimRef{ClaimID: additional.ClaimID, Revision: 1})
	}
	epoch := func() int64 {
		t.Helper()
		v, e := service.ReadInventory(ctx, owner, sc)
		if e != nil {
			t.Fatal(e)
		}
		return v.Revision
	}
	graphRevision := epoch()
	snapshot, e := service.CreateSnapshot(ctx, owner, sc, SnapshotInput{SnapshotID: uuid.NewString(), Reason: "Reviewed process role"}, uuid.NewString(), graphRevision)
	if e != nil {
		t.Fatal(e)
	}
	createInventory := func(capability string) DiscoveryInventory {
		t.Helper()
		v, err := service.CreateDiscoveryInventory(ctx, owner, sc, DiscoveryInput{InventoryID: uuid.NewString(), AreaID: entities[0].EntityID, AreaRevision: 1, ProcessID: entities[1].EntityID, ProcessRevision: 1, Capability: capability, Reason: "Inspect discovery coverage"}, uuid.NewString(), 0)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	inventory := createInventory("shortfall_investigation")
	read := func(id string) Coverage {
		t.Helper()
		v, err := service.ReadCoverage(ctx, viewer, sc, id)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	assertDisabled := func(v Coverage) {
		t.Helper()
		if v.SchemaVersion != 1 || v.EvaluatorVersion != 3 || v.ApprovedCount != 0 || v.KnowledgeReady || v.AuthorizationState != "disabled" || v.EvaluationState != "partial_support" {
			t.Fatalf("unsafe coverage: %+v", v)
		}
	}
	initial := read(inventory.InventoryID)
	assertDisabled(initial)
	if len(initial.Requirements) != 9 || initial.ApplicableCount != 9 || initial.BoundCount != 0 || !initial.ScopeCurrent || initial.InventoryRevision != 1 || initial.KnowledgeInventoryRevision != graphRevision {
		t.Fatalf("initial coverage: %+v", initial)
	}
	for _, r := range initial.Requirements {
		if r.State != "missing" || r.ReasonCode != "decision_missing" {
			t.Fatalf("invented initial evidence: %+v", r)
		}
	}
	for _, a := range []Actor{owner, supervisor, operator, viewer} {
		if _, e = service.ReadCoverage(ctx, a, sc, inventory.InventoryID); e != nil {
			t.Fatalf("member read: %v", e)
		}
	}
	if _, e = service.ReadCoverage(ctx, owner, foreign, inventory.InventoryID); !errors.Is(e, ErrNotFound) {
		t.Fatalf("foreign scope leaked: %v", e)
	}
	exec(`UPDATE supervisor.browser_sessions SET auth_time=NULL WHERE token_hash=$1`, viewer.SessionHash)
	read(inventory.InventoryID) // Reading does not require recent identity proof.
	exec(`UPDATE supervisor.browser_sessions SET expires_at=now()-interval '1 minute' WHERE token_hash=$1`, viewer.SessionHash)
	if _, e = service.ReadCoverage(ctx, viewer, sc, inventory.InventoryID); !errors.Is(e, ErrDenied) {
		t.Fatalf("expired session read: %v", e)
	}
	exec(`UPDATE supervisor.browser_sessions SET expires_at=now()+interval '1 hour' WHERE token_hash=$1`, viewer.SessionHash)
	var roleID string
	for _, r := range inventory.Requirements {
		if r.Key == "responsible_role" {
			roleID = r.RequirementID
		}
	}
	bind := RequirementDecisionInput{InventoryRevision: 1, Verdict: "bind", Reason: "Exact role witness", SnapshotID: snapshot.SnapshotID, AsOf: "2026-09-27T12:00:00Z", ClaimRefs: bindingRefs}
	decision, e := service.DecideRequirement(ctx, owner, sc, inventory.InventoryID, roleID, bind, uuid.NewString(), 0)
	if e != nil {
		t.Fatal(e)
	}
	bound := read(inventory.InventoryID)
	assertDisabled(bound)
	if bound.BoundCount != 1 || bound.ApplicableCount != 9 {
		t.Fatalf("binding counts: %+v", bound)
	}
	found := false
	for _, r := range bound.Requirements {
		if r.RequirementID == roleID {
			found = true
			if r.State != "candidate" || r.ReasonCode != "freshness_policy_missing" || r.DecisionID != decision.DecisionID || r.SnapshotID != snapshot.SnapshotID || r.SnapshotSHA256 != snapshot.SHA256 {
				t.Fatalf("candidate trace: %+v", r)
			}
		}
	}
	if !found {
		t.Fatal("role cell missing")
	}
	// Privileged retention must not silently shrink the fixed denominator.
	retained := createInventory("shortfall_investigation")
	retention, e := admin.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = retention.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, sc.TenantID); e != nil {
		t.Fatal(e)
	}
	if _, e = retention.ExecContext(ctx, `DELETE FROM supervisor.knowledge_discovery_requirements WHERE tenant_id=$1 AND site_id=$2 AND inventory_id=$3 AND revision=1 AND requirement_id=$4`, sc.TenantID, sc.SiteID, retained.InventoryID, retained.Requirements[0].RequirementID); e != nil {
		t.Fatal(e)
	}
	if e = retention.Commit(); e != nil {
		t.Fatal(e)
	}
	if _, e = service.ReadCoverage(ctx, viewer, sc, retained.InventoryID); !errors.Is(e, ErrInvalidContract) {
		t.Fatalf("partial retained denominator: %v", e)
	}
	allNA := createInventory("internal_task_coordination")
	na := RequirementDecisionInput{InventoryRevision: 1, Verdict: "not_applicable", Reason: "Explicit scoped exception"}
	for _, r := range allNA.Requirements {
		if _, e = service.DecideRequirement(ctx, owner, sc, allNA.InventoryID, r.RequirementID, na, uuid.NewString(), 0); e != nil {
			t.Fatal(e)
		}
	}
	excluded := read(allNA.InventoryID)
	assertDisabled(excluded)
	if len(excluded.Requirements) != 11 || excluded.ApplicableCount != 0 || excluded.BoundCount != 0 {
		t.Fatalf("all N/A counts: %+v", excluded)
	}
	for _, r := range excluded.Requirements {
		if r.State != "not_applicable" {
			t.Fatalf("N/A cell: %+v", r)
		}
	}
	if epoch() != graphRevision {
		t.Fatal("coverage or decisions changed graph epoch")
	}
	// Failed audit must prevent a successful read and leave no audit row.
	var auditBefore, auditAfter int
	if e = admin.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.knowledge_audit`).Scan(&auditBefore); e != nil {
		t.Fatal(e)
	}
	blocker, e := admin.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer blocker.Rollback()
	if _, e = blocker.ExecContext(ctx, `LOCK TABLE supervisor.knowledge_audit IN ACCESS EXCLUSIVE MODE`); e != nil {
		t.Fatal(e)
	}
	short, stop := context.WithTimeout(ctx, 150*time.Millisecond)
	_, e = service.ReadCoverage(short, viewer, sc, inventory.InventoryID)
	stop()
	if e == nil {
		t.Fatal("coverage escaped failed audit")
	}
	if e = blocker.Rollback(); e != nil {
		t.Fatal(e)
	}
	if e = admin.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.knowledge_audit`).Scan(&auditAfter); e != nil || auditBefore != auditAfter {
		t.Fatalf("partial audit: %d %d %v", auditBefore, auditAfter, e)
	}
	// Withdrawn bindings retain their predecessor's provenance access boundary.
	withdrawInventory := createInventory("shortfall_investigation")
	var withdrawRole string
	for _, r := range withdrawInventory.Requirements {
		if r.Key == "responsible_role" {
			withdrawRole = r.RequirementID
		}
	}
	if _, e = service.DecideRequirement(ctx, owner, sc, withdrawInventory.InventoryID, withdrawRole, bind, uuid.NewString(), 0); e != nil {
		t.Fatal(e)
	}
	if _, e = service.DecideRequirement(ctx, owner, sc, withdrawInventory.InventoryID, withdrawRole, RequirementDecisionInput{InventoryRevision: 1, Verdict: "withdraw", Reason: "Review withdrawn"}, uuid.NewString(), 1); e != nil {
		t.Fatal(e)
	}
	withdrawn := read(withdrawInventory.InventoryID)
	for _, r := range withdrawn.Requirements {
		if r.RequirementID == withdrawRole && (r.State != "missing" || r.ReasonCode != "decision_withdrawn") {
			t.Fatalf("withdraw state: %+v", r)
		}
	}
	// A refreshed discovery revision never inherits old exceptions or bindings.
	refreshed, e := service.CreateDiscoveryInventory(ctx, owner, sc, DiscoveryInput{InventoryID: allNA.InventoryID, AreaID: entities[0].EntityID, AreaRevision: 1, ProcessID: entities[1].EntityID, ProcessRevision: 1, Capability: allNA.Capability, Reason: "Refresh scope"}, uuid.NewString(), 1)
	if e != nil {
		t.Fatal(e)
	}
	fresh := read(refreshed.InventoryID)
	assertDisabled(fresh)
	if fresh.InventoryRevision != 2 || fresh.ApplicableCount != 11 {
		t.Fatalf("inherited exclusions: %+v", fresh)
	}
	for _, r := range fresh.Requirements {
		if r.State != "missing" || r.DecisionID != "" {
			t.Fatalf("inherited decision: %+v", r)
		}
	}
	// A graph mutation makes old snapshot bindings stale without treating them as approved.
	source()
	stale := read(inventory.InventoryID)
	assertDisabled(stale)
	if stale.BoundCount != 0 || stale.ApplicableCount != 9 {
		t.Fatalf("stale binding counts: %+v", stale)
	}
	for _, r := range stale.Requirements {
		if r.RequirementID == roleID && r.State != "stale" {
			t.Fatalf("snapshot staleness omitted: %+v", r)
		}
	}
	// Anchor supersession returns exceptions to the denominator.
	for _, r := range refreshed.Requirements {
		if _, e = service.DecideRequirement(ctx, owner, sc, refreshed.InventoryID, r.RequirementID, RequirementDecisionInput{InventoryRevision: 2, Verdict: "not_applicable", Reason: "Current explicit exception"}, uuid.NewString(), 0); e != nil {
			t.Fatal(e)
		}
	}
	area := entities[0]
	area.Name = "Revised area"
	if _, e = service.CreateEntity(ctx, owner, sc, area, uuid.NewString(), 1); e != nil {
		t.Fatal(e)
	}
	staleNA := read(allNA.InventoryID)
	assertDisabled(staleNA)
	if staleNA.ScopeCurrent || staleNA.ApplicableCount != 11 {
		t.Fatalf("stale N/A denominator: %+v", staleNA)
	}
	for _, r := range staleNA.Requirements {
		if r.State != "stale" {
			t.Fatalf("stale N/A cell: %+v", r)
		}
	}
	if _, e = ev.Revoke(ctx, ea, es, report.SourceID, "Withdraw report access", uuid.NewString(), 1); e != nil {
		t.Fatal(e)
	}
	for _, id := range []string{inventory.InventoryID, withdrawInventory.InventoryID} {
		if _, e = service.ReadCoverage(ctx, viewer, sc, id); !errors.Is(e, ErrNotFound) {
			t.Fatalf("revoked provenance projected %s: %v", id, e)
		}
	}
}
