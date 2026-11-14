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

func TestPostgresRequirementDecisions(t *testing.T) {
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
	database := "requirement_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	discoveryInput := DiscoveryInput{InventoryID: uuid.NewString(), AreaID: entities[0].EntityID, AreaRevision: 1, ProcessID: entities[1].EntityID, ProcessRevision: 1, Capability: "shortfall_investigation", Reason: "Discover role coverage"}
	inventory, e := service.CreateDiscoveryInventory(ctx, owner, sc, discoveryInput, uuid.NewString(), 0)
	if e != nil {
		t.Fatal(e)
	}
	requirements := map[string]string{}
	for _, r := range inventory.Requirements {
		requirements[r.Key] = r.RequirementID
	}
	requirementID := requirements["responsible_role"]
	bind := RequirementDecisionInput{InventoryRevision: 1, Verdict: "bind", Reason: "Bind exact reviewed role", SnapshotID: snapshot.SnapshotID, AsOf: "2026-09-27T12:00:00Z", ClaimRefs: bindingRefs}
	key := uuid.NewString()
	for _, denied := range []Actor{operator, viewer} {
		if _, e = service.DecideRequirement(ctx, denied, sc, inventory.InventoryID, requirementID, bind, key, 0); !errors.Is(e, ErrDenied) {
			t.Fatalf("role accepted requirementdecision: %v", e)
		}
	}
	exec(`UPDATE supervisor.browser_sessions SET auth_time=NULL WHERE token_hash=$1`, supervisor.SessionHash)
	if _, e = service.DecideRequirement(ctx, supervisor, sc, inventory.InventoryID, requirementID, bind, key, 0); !errors.Is(e, ErrReauthenticationRequired) {
		t.Fatalf("proof missing: %v", e)
	}
	exec(`UPDATE supervisor.browser_sessions SET auth_time=now() WHERE token_hash=$1`, supervisor.SessionHash)
	if _, e = service.DecideRequirement(ctx, owner, sc, inventory.InventoryID, requirements["good_count_source"], bind, uuid.NewString(), 0); !errors.Is(e, ErrBindingUnsupported) {
		t.Fatalf("unsupported requirement bound: %v", e)
	}
	bad := bind
	bad.ClaimRefs = []ConflictClaimRef{{ClaimID: uuid.NewString(), Revision: 1}}
	if _, e = service.DecideRequirement(ctx, owner, sc, inventory.InventoryID, requirementID, bad, uuid.NewString(), 0); !errors.Is(e, ErrBindingBlocked) {
		t.Fatalf("unknown witness allowed: %v", e)
	}
	created, e := service.DecideRequirement(ctx, supervisor, sc, inventory.InventoryID, requirementID, bind, key, 0)
	if e != nil {
		t.Fatal(e)
	}
	if created.SchemaVersion != 1 || created.Revision != 1 || created.CurrentRevision != 1 || !created.DecisionEligible || created.InventoryID != inventory.InventoryID || created.InventoryRevision != 1 || created.RequirementID != requirementID || created.SnapshotID != snapshot.SnapshotID || created.SnapshotSHA256 != snapshot.SHA256 || created.SnapshotInventoryRevision != graphRevision || len(created.ClaimRefs) != 3 || created.ActorIssuer != supervisor.Issuer || created.ActorSubject != supervisor.Subject || created.AuthenticatedAt.IsZero() {
		t.Fatalf("binding pins/identity: %+v", created)
	}
	if _, e = uuid.Parse(created.DecisionID); e != nil {
		t.Fatal(e)
	}
	if epoch() != graphRevision {
		t.Fatal("binding changed graph inventory")
	}
	replay, e := service.DecideRequirement(ctx, supervisor, sc, inventory.InventoryID, requirementID, bind, key, 0)
	if e != nil || replay.DecisionID != created.DecisionID {
		t.Fatalf("replay %+v %v", replay, e)
	}
	if _, e = service.DecideRequirement(ctx, owner, sc, inventory.InventoryID, requirementID, bind, uuid.NewString(), 0); !errors.Is(e, ErrRevision) {
		t.Fatalf("CAS failed: %v", e)
	}
	changed := bind
	changed.Reason = "Different request"
	if _, e = service.DecideRequirement(ctx, supervisor, sc, inventory.InventoryID, requirementID, changed, key, 0); !errors.Is(e, ErrConflict) {
		t.Fatalf("idempotency conflict: %v", e)
	}
	for _, reader := range []Actor{owner, operator, viewer} {
		v, e := service.ReadRequirementDecision(ctx, reader, sc, inventory.InventoryID, requirementID, 1, 0)
		if e != nil || v.DecisionID != created.DecisionID {
			t.Fatalf("member read %+v %v", v, e)
		}
	}
	if _, e = service.ReadRequirementDecision(ctx, owner, foreign, inventory.InventoryID, requirementID, 1, 0); !errors.Is(e, ErrNotFound) {
		t.Fatalf("foreign read leaked: %v", e)
	}
	na := RequirementDecisionInput{InventoryRevision: 1, Verdict: "not_applicable", Reason: "Explicitly excluded from this process"}
	naKey := uuid.NewString()
	notApplicable, e := service.DecideRequirement(ctx, owner, sc, inventory.InventoryID, requirements["good_count_source"], na, naKey, 0)
	if e != nil || !notApplicable.DecisionEligible || notApplicable.SnapshotID != "" || len(notApplicable.ClaimRefs) != 0 {
		t.Fatalf("N/A %+v %v", notApplicable, e)
	}
	withdraw := RequirementDecisionInput{InventoryRevision: 1, Verdict: "withdraw", Reason: "Binding requires review"}
	withdrawKey := uuid.NewString()
	withdrawn, e := service.DecideRequirement(ctx, supervisor, sc, inventory.InventoryID, requirementID, withdraw, withdrawKey, 1)
	if e != nil || withdrawn.Revision != 2 || withdrawn.DecisionEligible {
		t.Fatalf("withdraw %+v %v", withdrawn, e)
	}
	old, e := service.ReadRequirementDecision(ctx, viewer, sc, inventory.InventoryID, requirementID, 1, 1)
	if e != nil || old.DecisionEligible || old.DecisionID != created.DecisionID {
		t.Fatalf("old bind %+v %v", old, e)
	}
	if _, e = service.DecideRequirement(ctx, owner, sc, inventory.InventoryID, requirementID, bind, uuid.NewString(), 2); e != nil {
		t.Fatal(e)
	}
	if epoch() != graphRevision {
		t.Fatal("decision history changed graph epoch")
	}
	// Late children cannot expand a persisted binding, including after a scope switch.
	guardInput := discoveryInput
	guardInput.InventoryID = uuid.NewString()
	guardInventory, e := service.CreateDiscoveryInventory(ctx, owner, sc, guardInput, uuid.NewString(), 0)
	if e != nil {
		t.Fatal(e)
	}
	var guardRequirement string
	for _, r := range guardInventory.Requirements {
		if r.Key == "responsible_role" {
			guardRequirement = r.RequirementID
		}
	}
	one := bind
	one.ClaimRefs = append([]ConflictClaimRef(nil), bindingRefs[:1]...)
	if _, e = service.DecideRequirement(ctx, owner, sc, guardInventory.InventoryID, guardRequirement, one, uuid.NewString(), 0); e != nil {
		t.Fatal(e)
	}
	for _, commitScope := range []string{sc.TenantID, foreign.TenantID, ""} {
		tx, err := runtime.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, sc.TenantID); err != nil {
			t.Fatal(err)
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO supervisor.knowledge_requirement_decision_claims(tenant_id,site_id,inventory_id,inventory_revision,requirement_id,decision_revision,snapshot_id,claim_id,claim_revision) VALUES($1,$2,$3,1,$4,1,$5,$6,1)`, sc.TenantID, sc.SiteID, guardInventory.InventoryID, guardRequirement, snapshot.SnapshotID, bindingRefs[1].ClaimID)
		if err != nil {
			tx.Rollback()
			t.Fatalf("valid late child did not reach deferred guard: %v", err)
		}
		if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, commitScope); err != nil {
			t.Fatal(err)
		}
		want := "23514"
		if commitScope != sc.TenantID {
			want = "42501"
		}
		requireSQLState(t, tx.Commit(), want)
	}
	guardRead, e := service.ReadRequirementDecision(ctx, viewer, sc, guardInventory.InventoryID, guardRequirement, 1, 0)
	if e != nil || len(guardRead.ClaimRefs) != 1 {
		t.Fatalf("failed child insert changed binding: %+v %v", guardRead, e)
	}
	// Privileged retention of one of three pins must fail closed on reads and receipts.
	retentionInput := discoveryInput
	retentionInput.InventoryID = uuid.NewString()
	retentionInventory, e := service.CreateDiscoveryInventory(ctx, owner, sc, retentionInput, uuid.NewString(), 0)
	if e != nil {
		t.Fatal(e)
	}
	var retentionRequirement string
	for _, r := range retentionInventory.Requirements {
		if r.Key == "responsible_role" {
			retentionRequirement = r.RequirementID
		}
	}
	retentionKey := uuid.NewString()
	if _, e = service.DecideRequirement(ctx, owner, sc, retentionInventory.InventoryID, retentionRequirement, bind, retentionKey, 0); e != nil {
		t.Fatal(e)
	}
	retention, e := admin.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = retention.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, sc.TenantID); e != nil {
		t.Fatal(e)
	}
	deleted, e := retention.ExecContext(ctx, `DELETE FROM supervisor.knowledge_requirement_decision_claims WHERE tenant_id=$1 AND site_id=$2 AND inventory_id=$3 AND requirement_id=$4 AND decision_revision=1 AND claim_id=$5`, sc.TenantID, sc.SiteID, retentionInventory.InventoryID, retentionRequirement, bindingRefs[0].ClaimID)
	if e != nil {
		t.Fatal(e)
	}
	if count, err := deleted.RowsAffected(); err != nil || count != 1 {
		t.Fatalf("retained child deletion: %d %v", count, err)
	}
	if e = retention.Commit(); e != nil {
		t.Fatal(e)
	}
	if _, e = service.ReadRequirementDecision(ctx, viewer, sc, retentionInventory.InventoryID, retentionRequirement, 1, 0); !errors.Is(e, ErrInvalidContract) {
		t.Fatalf("partial retained binding read: %v", e)
	}
	if _, e = service.DecideRequirement(ctx, owner, sc, retentionInventory.InventoryID, retentionRequirement, bind, retentionKey, 0); !errors.Is(e, ErrInvalidContract) {
		t.Fatalf("partial retained binding replay: %v", e)
	}
	// Failed audit rolls back decision head/history/references and its receipt.
	rollbackID := requirements["order_window"]
	rollbackKey := uuid.NewString()
	blocker, e := admin.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer blocker.Rollback()
	if _, e = blocker.ExecContext(ctx, `LOCK TABLE supervisor.knowledge_audit IN ACCESS EXCLUSIVE MODE`); e != nil {
		t.Fatal(e)
	}
	short, stop := context.WithTimeout(ctx, 150*time.Millisecond)
	_, e = service.DecideRequirement(short, owner, sc, inventory.InventoryID, rollbackID, na, rollbackKey, 0)
	stop()
	if e == nil {
		t.Fatal("audit failure committed requirement")
	}
	if e = blocker.Rollback(); e != nil {
		t.Fatal(e)
	}
	for _, table := range []string{"knowledge_requirement_decision_heads", "knowledge_requirement_decisions", "knowledge_requirement_decision_claims"} {
		var n int
		if e = admin.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.`+table+` WHERE tenant_id=$1 AND site_id=$2 AND requirement_id=$3`, sc.TenantID, sc.SiteID, rollbackID).Scan(&n); e != nil || n != 0 {
			t.Fatalf("partial %s rows%d %v", table, n, e)
		}
	}
	if _, e = service.DecideRequirement(ctx, owner, sc, inventory.InventoryID, rollbackID, na, rollbackKey, 0); e != nil {
		t.Fatal(e)
	}
	if epoch() != graphRevision {
		t.Fatal("rollback/retry advanced graph epoch")
	}
	attempt, e := runtime.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = attempt.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, foreign.TenantID); e != nil {
		t.Fatal(e)
	}
	var n int
	if e = attempt.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.knowledge_requirement_decisions WHERE tenant_id=$1`, sc.TenantID).Scan(&n); e != nil || n != 0 {
		t.Fatalf("RLS leak%d %v", n, e)
	}
	attempt.Rollback()
	for _, query := range []string{`UPDATE supervisor.knowledge_requirement_decisions SET reason='forged' WHERE decision_id=$1`, `DELETE FROM supervisor.knowledge_requirement_decisions WHERE decision_id=$1`} {
		tx, e := runtime.BeginTx(ctx, nil)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, sc.TenantID); e != nil {
			t.Fatal(e)
		}
		_, e = tx.ExecContext(ctx, query, created.DecisionID)
		tx.Rollback()
		requireSQLState(t, e, "42501")
	}
	exec(`UPDATE supervisor.browser_sessions SET auth_time=now()-interval '16 minutes' WHERE token_hash=$1`, supervisor.SessionHash)
	if _, e = service.DecideRequirement(ctx, supervisor, sc, inventory.InventoryID, requirementID, bind, key, 0); !errors.Is(e, ErrReauthenticationRequired) {
		t.Fatalf("replay skipped current proof: %v", e)
	}
	exec(`UPDATE supervisor.browser_sessions SET auth_time=now() WHERE token_hash=$1`, supervisor.SessionHash)
	if _, e = ev.CreateOrRevise(ctx, ea, es, evidence.Input{SourceID: uuid.NewString(), Kind: "manual", Title: "New observation", Text: "Changes graph epoch"}, uuid.NewString(), 0); e != nil {
		t.Fatal(e)
	}
	if _, e = service.DecideRequirement(ctx, owner, sc, inventory.InventoryID, requirementID, bind, uuid.NewString(), 3); !errors.Is(e, ErrSnapshotStale) {
		t.Fatalf("stale snapshot bound: %v", e)
	}
	stale, e := service.ReadRequirementDecision(ctx, viewer, sc, inventory.InventoryID, requirementID, 1, 0)
	if e != nil || stale.DecisionEligible {
		t.Fatalf("stale binding eligible %+v %v", stale, e)
	}
	if _, e = ev.Revoke(ctx, ea, es, report.SourceID, "Bound evidence access withdrawn", uuid.NewString(), 1); e != nil {
		t.Fatal(e)
	}
	for _, revision := range []int64{1, 2, 3} {
		if _, e = service.ReadRequirementDecision(ctx, viewer, sc, inventory.InventoryID, requirementID, 1, revision); !errors.Is(e, ErrNotFound) {
			t.Fatalf("revoked predecessor/history rev%d %v", revision, e)
		}
	}
	if _, e = service.DecideRequirement(ctx, supervisor, sc, inventory.InventoryID, requirementID, withdraw, withdrawKey, 1); !errors.Is(e, ErrNotFound) {
		t.Fatalf("withdrawal replay leaked predecessor: %v", e)
	}
	if _, e = service.DecideRequirement(ctx, owner, sc, inventory.InventoryID, requirementID, withdraw, uuid.NewString(), 3); !errors.Is(e, ErrNotFound) {
		t.Fatalf("withdraw accessed revoked predecessor: %v", e)
	}
	// A new N/A may replace inaccessible historical evidence without returning it.
	replacement, e := service.DecideRequirement(ctx, owner, sc, inventory.InventoryID, requirementID, na, uuid.NewString(), 3)
	if e != nil || replacement.Revision != 4 || !replacement.DecisionEligible || replacement.SnapshotID != "" || len(replacement.ClaimRefs) != 0 {
		t.Fatalf("replacement leaked old binding %+v %v", replacement, e)
	}
	area := entities[0]
	area.Name = "Revised scope"
	if _, e = service.CreateEntity(ctx, owner, sc, area, uuid.NewString(), 1); e != nil {
		t.Fatal(e)
	}
	if _, e = service.DecideRequirement(ctx, owner, sc, inventory.InventoryID, requirementID, na, uuid.NewString(), 4); !errors.Is(e, ErrSourceChanged) {
		t.Fatalf("stale anchor accepted: %v", e)
	}
	discoveryInput.AreaRevision = 2
	if _, e = service.CreateDiscoveryInventory(ctx, owner, sc, discoveryInput, uuid.NewString(), 1); e != nil {
		t.Fatal(e)
	}
	if _, e = service.DecideRequirement(ctx, owner, sc, inventory.InventoryID, requirementID, na, uuid.NewString(), 4); !errors.Is(e, ErrSourceChanged) {
		t.Fatalf("superseded inventory accepted: %v", e)
	}
	if old, e := service.ReadRequirementDecision(ctx, viewer, sc, inventory.InventoryID, requirementID, 1, 4); e != nil || old.DecisionEligible {
		t.Fatalf("superseded decision eligible %+v %v", old, e)
	}
	na.InventoryRevision = 2
	if newDecision, e := service.DecideRequirement(ctx, owner, sc, inventory.InventoryID, requirementID, na, uuid.NewString(), 0); e != nil || newDecision.Revision != 1 || !newDecision.DecisionEligible {
		t.Fatalf("new inventory decision %+v %v", newDecision, e)
	}
}
