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

func TestPostgresFreshnessQualification(t *testing.T) {
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
	database := "freshness_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
		in := evidence.Input{SourceID: uuid.NewString(), Kind: "document", Title: "Resolution source", Text: "Synthetic conflict evidence"}
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
	claim := ClaimInput{ClaimID: uuid.NewString(), SubjectID: entities[1].EntityID, SubjectRevision: 1, Predicate: "responsible_role", ObjectEntityID: entities[2].EntityID, ObjectRevision: 1, EffectiveFrom: time.Now().Add(-time.Hour).UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano), EvidenceClass: "report", SourceSpanIDs: []string{span.ID}}
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
	if _, e = ev.DecideAuthority(ctx, ea, es, origin.SourceID, evidence.AuthorityInput{SourceRevision: 1, ExpectedSourceStateRevision: 1, Verdict: "designate", Reason: "Verified process documentation"}, uuid.NewString(), 0); e != nil {
		t.Fatal(e)
	}
	graphRevision := epoch()
	snapshot, e := service.CreateSnapshot(ctx, owner, sc, SnapshotInput{SnapshotID: uuid.NewString(), Reason: "Reviewed process role"}, uuid.NewString(), graphRevision)
	if e != nil {
		t.Fatal(e)
	}
	createInventory := func() DiscoveryInventory {
		t.Helper()
		v, err := service.CreateDiscoveryInventory(ctx, owner, sc, DiscoveryInput{InventoryID: uuid.NewString(), AreaID: entities[0].EntityID, AreaRevision: 1, ProcessID: entities[1].EntityID, ProcessRevision: 1, Capability: "shortfall_investigation", Reason: "Freshness qualification fixture"}, uuid.NewString(), 0)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	requirement := func(inv DiscoveryInventory, key string) string {
		for _, r := range inv.Requirements {
			if r.Key == key {
				return r.RequirementID
			}
		}
		t.Fatalf("missing requirement %s", key)
		return ""
	}
	readCoverage := func(inv DiscoveryInventory) Coverage {
		t.Helper()
		v, err := service.ReadCoverage(ctx, viewer, sc, inv.InventoryID)
		if err != nil {
			t.Fatal(err)
		}
		if v.KnowledgeReady || v.AuthorizationState != "disabled" {
			t.Fatalf("partial qualification activated site: %+v", v)
		}
		return v
	}
	ttl := int64(3600)
	policyInput := FreshnessPolicyInput{Verdict: "define", MaxAgeSeconds: &ttl, Reason: "Owner policy for verified documents"}
	for _, a := range []Actor{supervisor, operator, viewer} {
		if _, e = service.DecideFreshnessPolicy(ctx, a, sc, policyInput, uuid.NewString(), 0); !errors.Is(e, ErrDenied) {
			t.Fatalf("nonowner policy: %v", e)
		}
	}
	exec(`UPDATE supervisor.browser_sessions SET auth_time=NULL WHERE token_hash=$1`, owner.SessionHash)
	if _, e = service.DecideFreshnessPolicy(ctx, owner, sc, policyInput, uuid.NewString(), 0); !errors.Is(e, ErrReauthenticationRequired) {
		t.Fatalf("policy missing proof: %v", e)
	}
	exec(`UPDATE supervisor.browser_sessions SET auth_time=now() WHERE token_hash=$1`, owner.SessionHash)
	if _, e = service.DecideFreshnessPolicy(ctx, owner, sc, FreshnessPolicyInput{Verdict: "withdraw", Reason: "No prior policy"}, uuid.NewString(), 0); !errors.Is(e, ErrInvalidContract) {
		t.Fatalf("initial policy withdrawal: %v", e)
	}
	policyKey := uuid.NewString()
	policy, e := service.DecideFreshnessPolicy(ctx, owner, sc, policyInput, policyKey, 0)
	if e != nil {
		t.Fatal(e)
	}
	if !policy.PolicyCurrent || policy.Revision != 1 || policy.Basis != "owner_currentness_verified_at" || policy.MaxAgeSeconds == nil || *policy.MaxAgeSeconds != ttl {
		t.Fatalf("policy pins: %+v", policy)
	}
	replay, e := service.DecideFreshnessPolicy(ctx, owner, sc, policyInput, policyKey, 0)
	if e != nil || replay.DecisionID != policy.DecisionID || !replay.CreatedAt.Equal(policy.CreatedAt) {
		t.Fatalf("policy replay: %+v %v", replay, e)
	}
	if _, e = service.DecideFreshnessPolicy(ctx, owner, sc, policyInput, uuid.NewString(), 0); !errors.Is(e, ErrRevision) {
		t.Fatalf("policy CAS: %v", e)
	}
	changedPolicy := policyInput
	changedPolicy.Reason = "Different policy reason"
	if _, e = service.DecideFreshnessPolicy(ctx, owner, sc, changedPolicy, policyKey, 0); !errors.Is(e, ErrConflict) {
		t.Fatalf("policy key conflict: %v", e)
	}
	if _, e = service.ReadFreshnessPolicy(ctx, owner, foreign, 1); !errors.Is(e, ErrNotFound) {
		t.Fatalf("foreign policy: %v", e)
	}
	for _, a := range []Actor{owner, supervisor, operator, viewer} {
		if _, e = service.ReadFreshnessPolicy(ctx, a, sc, 0); e != nil {
			t.Fatal(e)
		}
	}
	exec(`UPDATE supervisor.browser_sessions SET auth_time=now()-interval '16 minutes' WHERE token_hash=$1`, owner.SessionHash)
	if _, e = service.DecideFreshnessPolicy(ctx, owner, sc, policyInput, policyKey, 0); !errors.Is(e, ErrReauthenticationRequired) {
		t.Fatalf("policy replay proof: %v", e)
	}
	exec(`UPDATE supervisor.browser_sessions SET auth_time=now() WHERE token_hash=$1`, owner.SessionHash)
	confirmations := map[string]evidence.Currentness{}
	for _, id := range []string{origin.SourceID, report.SourceID} {
		a, err := ev.ReadAuthority(ctx, ea, es, id, 0)
		if err != nil {
			t.Fatal(err)
		}
		v, err := ev.DecideCurrentness(ctx, ea, es, id, evidence.CurrentnessInput{SourceRevision: 1, ExpectedSourceStateRevision: 1, AuthorityRevision: a.Revision, AuthorityDecisionID: a.DecisionID, Verdict: "confirm_current", Reason: "Owner checks live document"}, uuid.NewString(), 0)
		if err != nil {
			t.Fatal(err)
		}
		confirmations[id] = v
	}
	unqualifiedInventory := createInventory()
	unqualified := RequirementDecisionInput{InventoryRevision: 1, Verdict: "bind", Reason: "Legacy unqualified binding", SnapshotID: snapshot.SnapshotID, AsOf: time.Now().UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano), ClaimRefs: bindingRefs}
	if _, e = service.DecideRequirement(ctx, owner, sc, unqualifiedInventory.InventoryID, requirement(unqualifiedInventory, "responsible_role"), unqualified, uuid.NewString(), 0); e != nil {
		t.Fatal(e)
	}
	legacy := readCoverage(unqualifiedInventory)
	if legacy.ApprovedCount != 0 || legacy.BoundCount != 1 {
		t.Fatalf("legacy silently qualified: %+v", legacy)
	}
	qualified := unqualified
	qualified.Reason = "Explicit qualified structural binding"
	qualified.FreshnessPolicyRevision = policy.Revision
	for _, source := range snapshot.Contents.Sources {
		v, ok := confirmations[source.SourceID]
		if !ok {
			t.Fatalf("unexpected snapshot source %s", source.SourceID)
		}
		qualified.VerificationRefs = append(qualified.VerificationRefs, VerificationRef{SourceID: source.SourceID, VerificationID: v.VerificationID, VerificationRevision: v.Revision})
	}
	if len(qualified.VerificationRefs) != 2 {
		t.Fatalf("fixture requires both source pins: %+v", snapshot.Contents.Sources)
	}
	inv := createInventory()
	roleID := requirement(inv, "responsible_role")
	missing := qualified
	missing.VerificationRefs = append([]VerificationRef(nil), qualified.VerificationRefs[:1]...)
	if _, e = service.DecideRequirement(ctx, owner, sc, inv.InventoryID, roleID, missing, uuid.NewString(), 0); !errors.Is(e, ErrFreshnessBlocked) {
		t.Fatal("missing snapshot source confirmation accepted")
	}
	extra := qualified
	extra.VerificationRefs = append(append([]VerificationRef(nil), qualified.VerificationRefs...), VerificationRef{SourceID: uuid.NewString(), VerificationID: uuid.NewString(), VerificationRevision: 1})
	if _, e = service.DecideRequirement(ctx, owner, sc, inv.InventoryID, roleID, extra, uuid.NewString(), 0); !errors.Is(e, ErrFreshnessBlocked) {
		t.Fatal("extra snapshot source confirmation accepted")
	}
	decisionKey := uuid.NewString()
	approved, e := service.DecideRequirement(ctx, owner, sc, inv.InventoryID, roleID, qualified, decisionKey, 0)
	if e != nil {
		t.Fatal(e)
	}
	if approved.FreshnessPolicyRevision != policy.Revision || len(approved.VerificationRefs) != 2 {
		t.Fatalf("qualification pins absent: %+v", approved)
	}
	coverage := readCoverage(inv)
	if coverage.ApprovedCount != 1 || coverage.BoundCount != 1 {
		t.Fatalf("fresh structural qualification: %+v", coverage)
	}
	policyBinding := RequirementDecisionInput{InventoryRevision: 1, Verdict: "bind", Reason: "Explicit owner policy binding", FreshnessPolicyRevision: policy.Revision}
	if _, e = service.DecideRequirement(ctx, owner, sc, inv.InventoryID, requirement(inv, "evidence_freshness_policy"), policyBinding, uuid.NewString(), 0); e != nil {
		t.Fatal(e)
	}
	coverage = readCoverage(inv)
	if coverage.ApprovedCount != 2 {
		t.Fatalf("policy-only binding not qualified: %+v", coverage)
	}
	if epoch() != graphRevision {
		t.Fatal("policy/confirmation/qualification changed graph epoch")
	}
	// Current N/A on the policy cell cannot make legacy evidence ready.
	if _, e = service.DecideRequirement(ctx, owner, sc, unqualifiedInventory.InventoryID, requirement(unqualifiedInventory, "evidence_freshness_policy"), RequirementDecisionInput{InventoryRevision: 1, Verdict: "not_applicable", Reason: "Explicit exception"}, uuid.NewString(), 0); e != nil {
		t.Fatal(e)
	}
	legacy = readCoverage(unqualifiedInventory)
	if legacy.ApprovedCount != 0 || legacy.ApplicableCount != 8 {
		t.Fatalf("N/A activated qualification: %+v", legacy)
	}
	// Identical policy replacement invalidates exact historical pins permanently.
	policy2, e := service.DecideFreshnessPolicy(ctx, owner, sc, policyInput, uuid.NewString(), 1)
	if e != nil {
		t.Fatal(e)
	}
	coverage = readCoverage(inv)
	if coverage.ApprovedCount != 0 {
		t.Fatalf("policy replacement resurrected old qualification: %+v", coverage)
	}
	policyOld, e := service.ReadFreshnessPolicy(ctx, viewer, sc, 1)
	if e != nil || policyOld.PolicyCurrent {
		t.Fatalf("old policy revived: %+v %v", policyOld, e)
	}
	replay, e = service.DecideFreshnessPolicy(ctx, owner, sc, policyInput, policyKey, 0)
	if e != nil || replay.PolicyCurrent || replay.DecisionID != policy.DecisionID {
		t.Fatalf("old policy receipt current: %+v %v", replay, e)
	}
	qualified.FreshnessPolicyRevision = policy2.Revision
	second := createInventory()
	secondRole := requirement(second, "responsible_role")
	if _, e = service.DecideRequirement(ctx, owner, sc, second.InventoryID, secondRole, qualified, uuid.NewString(), 0); e != nil {
		t.Fatal(e)
	}
	prior := confirmations[report.SourceID]
	nextConfirmation, e := ev.DecideCurrentness(ctx, ea, es, report.SourceID, evidence.CurrentnessInput{SourceRevision: 1, ExpectedSourceStateRevision: 1, AuthorityRevision: prior.AuthorityRevision, AuthorityDecisionID: prior.AuthorityDecisionID, Verdict: "confirm_current", Reason: "New owner verification"}, uuid.NewString(), prior.Revision)
	if e != nil {
		t.Fatal(e)
	}
	coverage = readCoverage(second)
	if coverage.ApprovedCount != 0 {
		t.Fatalf("confirmation replacement left old approval: %+v", coverage)
	}
	for i := range qualified.VerificationRefs {
		if qualified.VerificationRefs[i].SourceID == report.SourceID {
			qualified.VerificationRefs[i] = VerificationRef{SourceID: report.SourceID, VerificationID: nextConfirmation.VerificationID, VerificationRevision: nextConfirmation.Revision}
		}
	}
	third := createInventory()
	if _, e = service.DecideRequirement(ctx, owner, sc, third.InventoryID, requirement(third, "responsible_role"), qualified, uuid.NewString(), 0); e != nil {
		t.Fatal(e)
	}
	if _, e = ev.DecideCurrentness(ctx, ea, es, report.SourceID, evidence.CurrentnessInput{SourceRevision: 1, ExpectedSourceStateRevision: 1, AuthorityRevision: prior.AuthorityRevision, AuthorityDecisionID: prior.AuthorityDecisionID, Verdict: "withdraw", Reason: "Verification withdrawn"}, uuid.NewString(), nextConfirmation.Revision); e != nil {
		t.Fatal(e)
	}
	if v := readCoverage(third); v.ApprovedCount != 0 {
		t.Fatalf("withdrawn confirmation still approved: %+v", v)
	}
	if _, e = service.DecideFreshnessPolicy(ctx, owner, sc, FreshnessPolicyInput{Verdict: "withdraw", Reason: "Policy withdrawn"}, uuid.NewString(), policy2.Revision); e != nil {
		t.Fatal(e)
	}
	if _, e = service.DecideFreshnessPolicy(ctx, owner, sc, FreshnessPolicyInput{Verdict: "withdraw", Reason: "Already withdrawn"}, uuid.NewString(), 3); !errors.Is(e, ErrInvalidContract) {
		t.Fatalf("consecutive policy withdrawal: %v", e)
	}
	// One-second expiry is evaluated from immutable server verification time, not as_of.
	shortTTL := int64(1)
	shortPolicy, e := service.DecideFreshnessPolicy(ctx, owner, sc, FreshnessPolicyInput{Verdict: "define", MaxAgeSeconds: &shortTTL, Reason: "Short expiry regression"}, uuid.NewString(), 3)
	if e != nil {
		t.Fatal(e)
	}
	qualified.FreshnessPolicyRevision = shortPolicy.Revision
	qualified.VerificationRefs = nil
	var earliest time.Time
	for _, id := range []string{origin.SourceID, report.SourceID} {
		head, err := ev.ReadCurrentness(ctx, ea, es, id, 0)
		if err != nil {
			t.Fatal(err)
		}
		v, err := ev.DecideCurrentness(ctx, ea, es, id, evidence.CurrentnessInput{SourceRevision: 1, ExpectedSourceStateRevision: 1, AuthorityRevision: head.AuthorityRevision, AuthorityDecisionID: head.AuthorityDecisionID, Verdict: "confirm_current", Reason: "Renew explicit verification"}, uuid.NewString(), head.Revision)
		if err != nil {
			t.Fatal(err)
		}
		qualified.VerificationRefs = append(qualified.VerificationRefs, VerificationRef{SourceID: id, VerificationID: v.VerificationID, VerificationRevision: v.Revision})
		if earliest.IsZero() || v.VerifiedAt.Before(earliest) {
			earliest = *v.VerifiedAt
		}
	}
	expiryInventory := createInventory()
	expiryKey := uuid.NewString()
	if _, e = service.DecideRequirement(ctx, owner, sc, expiryInventory.InventoryID, requirement(expiryInventory, "responsible_role"), qualified, expiryKey, 0); e != nil {
		t.Fatal(e)
	}
	if v := readCoverage(expiryInventory); v.ApprovedCount != 1 {
		t.Fatalf("fresh short TTL not approved: %+v", v)
	}
	wait := time.Until(earliest.Add(time.Second + 20*time.Millisecond))
	if wait > 0 {
		time.Sleep(wait)
	}
	expired := readCoverage(expiryInventory)
	if expired.ApprovedCount != 0 || expired.BoundCount != 1 {
		t.Fatalf("expired binding qualified: %+v", expired)
	}
	foundExpired := false
	for _, r := range expired.Requirements {
		if r.RequirementID == requirement(expiryInventory, "responsible_role") {
			foundExpired = r.ReasonCode == "document_verification_expired"
		}
	}
	if !foundExpired {
		t.Fatalf("expiry reason absent: %+v", expired)
	}
	if _, e = service.DecideRequirement(ctx, owner, sc, expiryInventory.InventoryID, requirement(expiryInventory, "responsible_role"), qualified, expiryKey, 0); e != nil {
		t.Fatalf("historical replay inaccessible after expiry: %v", e)
	}
	if v := readCoverage(expiryInventory); v.ApprovedCount != 0 {
		t.Fatal("receipt replay renewed freshness")
	}
	if epoch() != graphRevision {
		t.Fatal("freshness lifecycle changed graph epoch")
	}
	// Policy audit failure leaves neither a partial revision nor an idempotency receipt.
	blocker, e := admin.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer blocker.Rollback()
	if _, e = blocker.ExecContext(ctx, `LOCK TABLE supervisor.knowledge_audit IN ACCESS EXCLUSIVE MODE`); e != nil {
		t.Fatal(e)
	}
	rollbackKey := uuid.NewString()
	short, stop := context.WithTimeout(ctx, 150*time.Millisecond)
	_, e = service.DecideFreshnessPolicy(short, owner, sc, policyInput, rollbackKey, shortPolicy.Revision)
	stop()
	if e == nil {
		t.Fatal("failed audit committed policy")
	}
	blocker.Rollback()
	var policyCount int
	if e = admin.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.document_freshness_policy_decisions WHERE tenant_id=$1 AND site_id=$2`, sc.TenantID, sc.SiteID).Scan(&policyCount); e != nil || policyCount != 4 {
		t.Fatalf("partial policy revision: %d %v", policyCount, e)
	}
	restoredPolicy, e := service.DecideFreshnessPolicy(ctx, owner, sc, policyInput, rollbackKey, shortPolicy.Revision)
	if e != nil {
		t.Fatal(e)
	}
	if v := readCoverage(inv); v.ApprovedCount != 0 {
		t.Fatal("policy redefine restored historical bindings")
	}
	// Runtime RLS and immutable policy history remain independent of service checks.
	tx, e := runtime.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, foreign.TenantID); e != nil {
		t.Fatal(e)
	}
	var visible int
	if e = tx.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.document_freshness_policy_decisions WHERE tenant_id=$1`, sc.TenantID).Scan(&visible); e != nil || visible != 0 {
		t.Fatalf("policy RLS: %d %v", visible, e)
	}
	tx.Rollback()
	tx, e = runtime.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, sc.TenantID); e != nil {
		t.Fatal(e)
	}
	_, e = tx.ExecContext(ctx, `UPDATE supervisor.document_freshness_policy_decisions SET max_age_seconds=9999 WHERE tenant_id=$1 AND site_id=$2`, sc.TenantID, sc.SiteID)
	tx.Rollback()
	requireSQLState(t, e, "42501")
	// Deferred child cardinality stays fail-closed even after the tenant GUC changes.
	for _, mode := range []string{"missing", "extra"} {
		for _, commitScope := range []string{sc.TenantID, foreign.TenantID, ""} {
			guard, err := runtime.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = guard.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, sc.TenantID); err != nil {
				t.Fatal(err)
			}
			if mode == "missing" {
				_, err = guard.ExecContext(ctx, `INSERT INTO supervisor.knowledge_requirement_decisions(tenant_id,site_id,inventory_id,inventory_revision,requirement_id,revision,decision_id,verdict,reason,actor_issuer,actor_subject,auth_time,snapshot_id,snapshot_sha256,snapshot_inventory_revision,as_of,claim_count,freshness_policy_revision,verification_count) SELECT tenant_id,site_id,inventory_id,inventory_revision,requirement_id,2,$5,verdict,reason,actor_issuer,actor_subject,now(),snapshot_id,snapshot_sha256,snapshot_inventory_revision,as_of,claim_count,freshness_policy_revision,verification_count FROM supervisor.knowledge_requirement_decisions WHERE tenant_id=$1 AND site_id=$2 AND inventory_id=$3 AND requirement_id=$4 AND revision=1`, sc.TenantID, sc.SiteID, inv.InventoryID, roleID, uuid.NewString())
				if err != nil {
					guard.Rollback()
					t.Fatalf("valid header fixture: %v", err)
				}
				_, err = guard.ExecContext(ctx, `INSERT INTO supervisor.knowledge_requirement_decision_claims(tenant_id,site_id,inventory_id,inventory_revision,requirement_id,decision_revision,snapshot_id,claim_id,claim_revision) SELECT tenant_id,site_id,inventory_id,inventory_revision,requirement_id,2,snapshot_id,claim_id,claim_revision FROM supervisor.knowledge_requirement_decision_claims WHERE tenant_id=$1 AND site_id=$2 AND inventory_id=$3 AND requirement_id=$4 AND decision_revision=1`, sc.TenantID, sc.SiteID, inv.InventoryID, roleID)
			} else {
				_, err = guard.ExecContext(ctx, `INSERT INTO supervisor.knowledge_requirement_decision_verifications(tenant_id,site_id,inventory_id,inventory_revision,requirement_id,decision_revision,source_id,source_revision,source_sha256,source_access_epoch,verification_id,verification_revision) SELECT tenant_id,site_id,$5,inventory_revision,$6,1,source_id,source_revision,source_sha256,source_access_epoch,verification_id,verification_revision FROM supervisor.knowledge_requirement_decision_verifications WHERE tenant_id=$1 AND site_id=$2 AND inventory_id=$3 AND requirement_id=$4 AND decision_revision=1 LIMIT 1`, sc.TenantID, sc.SiteID, inv.InventoryID, roleID, unqualifiedInventory.InventoryID, requirement(unqualifiedInventory, "responsible_role"))
			}
			if err != nil {
				guard.Rollback()
				t.Fatalf("valid %s fixture failed before deferred guard: %v", mode, err)
			}
			if _, err = guard.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, commitScope); err != nil {
				t.Fatal(err)
			}
			err = guard.Commit()
			var state interface{ SQLState() string }
			want := "23514"
			if commitScope != sc.TenantID {
				want = "42501"
			}
			if !errors.As(err, &state) || state.SQLState() != want {
				t.Fatalf("%s scope %q expected deferred %s got %v", mode, commitScope, want, err)
			}
		}
	}
	// SQL NULL must not bypass the policy-only binding shape constraint.
	shape, e := runtime.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = shape.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, sc.TenantID); e != nil {
		t.Fatal(e)
	}
	_, e = shape.ExecContext(ctx, `INSERT INTO supervisor.knowledge_requirement_decisions(tenant_id,site_id,inventory_id,inventory_revision,requirement_id,revision,decision_id,verdict,reason,actor_issuer,actor_subject,auth_time,snapshot_id,snapshot_sha256,snapshot_inventory_revision,as_of,claim_count,freshness_policy_revision,verification_count) SELECT tenant_id,site_id,inventory_id,inventory_revision,requirement_id,2,$5,verdict,reason,actor_issuer,actor_subject,now(),NULL,NULL,NULL,NULL,0,NULL,0 FROM supervisor.knowledge_requirement_decisions WHERE tenant_id=$1 AND site_id=$2 AND inventory_id=$3 AND requirement_id=$4 AND revision=1`, sc.TenantID, sc.SiteID, inv.InventoryID, requirement(inv, "evidence_freshness_policy"), uuid.NewString())
	shape.Rollback()
	var shapeState interface{ SQLState() string }
	if !errors.As(e, &shapeState) || shapeState.SQLState() != "23514" {
		t.Fatalf("NULL snapshot/policy bypassed shape: %v", e)
	}
	// Partial privileged retention of source confirmations must fail closed.
	retention, e := admin.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = retention.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, sc.TenantID); e != nil {
		t.Fatal(e)
	}
	deleted, e := retention.ExecContext(ctx, `DELETE FROM supervisor.knowledge_requirement_decision_verifications WHERE tenant_id=$1 AND site_id=$2 AND inventory_id=$3 AND requirement_id=$4 AND decision_revision=1 AND source_id=$5`, sc.TenantID, sc.SiteID, inv.InventoryID, roleID, origin.SourceID)
	if e != nil {
		t.Fatal(e)
	}
	if n, err := deleted.RowsAffected(); err != nil || n != 1 {
		t.Fatalf("retention fixture: %d %v", n, err)
	}
	if e = retention.Commit(); e != nil {
		t.Fatal(e)
	}
	if _, e = service.ReadCoverage(ctx, viewer, sc, inv.InventoryID); !errors.Is(e, ErrInvalidContract) {
		t.Fatalf("partial verification pins projected: %v", e)
	}
	if _, e = service.DecideRequirement(ctx, owner, sc, inv.InventoryID, roleID, RequirementDecisionInput{InventoryRevision: 1, Verdict: "bind", Reason: approved.Reason, SnapshotID: snapshot.SnapshotID, AsOf: unqualified.AsOf, ClaimRefs: bindingRefs, FreshnessPolicyRevision: 1, VerificationRefs: approved.VerificationRefs}, decisionKey, 0); !errors.Is(e, ErrInvalidContract) {
		t.Fatalf("partial verification pins replayed: %v", e)
	}
	// Client as_of cannot qualify a claim whose effective interval has not begun now.
	futureClaim := claim
	futureClaim.ClaimID = uuid.NewString()
	futureClaim.EffectiveFrom = time.Now().Add(24 * time.Hour).UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano)
	if _, e = service.CreateClaim(ctx, owner, sc, futureClaim, uuid.NewString(), 0); e != nil {
		t.Fatal(e)
	}
	if _, e = service.DecideReview(ctx, owner, sc, futureClaim.ClaimID, ReviewInput{ClaimRevision: 1, Verdict: "approve", Reason: "Reviewed planned future role"}, uuid.NewString(), 1); e != nil {
		t.Fatal(e)
	}
	futureSnapshot, e := service.CreateSnapshot(ctx, owner, sc, SnapshotInput{SnapshotID: uuid.NewString(), Reason: "Future claim fixture"}, uuid.NewString(), epoch())
	if e != nil {
		t.Fatal(e)
	}
	futureInventory := createInventory()
	futureInput := qualified
	futureInput.FreshnessPolicyRevision = restoredPolicy.Revision
	futureInput.SnapshotID = futureSnapshot.SnapshotID
	futureInput.AsOf = time.Now().Add(25 * time.Hour).UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano)
	futureInput.ClaimRefs = []ConflictClaimRef{{ClaimID: futureClaim.ClaimID, Revision: 1}}
	if _, e = service.DecideRequirement(ctx, owner, sc, futureInventory.InventoryID, requirement(futureInventory, "responsible_role"), futureInput, uuid.NewString(), 0); !errors.Is(e, ErrFreshnessBlocked) {
		t.Fatalf("future as_of granted freshness: %v", e)
	}
	// A snapshot containing any non-document source cannot receive document qualification.
	manual := evidence.Input{SourceID: uuid.NewString(), Kind: "manual", Title: "Manual role report", Text: "Manually reported process role"}
	if _, e = ev.CreateOrRevise(ctx, ea, es, manual, uuid.NewString(), 0); e != nil {
		t.Fatal(e)
	}
	manualRole := EntityInput{EntityID: uuid.NewString(), Type: "role", Name: "Manually described role", SourceID: manual.SourceID, SourceRevision: 1}
	if _, e = service.CreateEntity(ctx, owner, sc, manualRole, uuid.NewString(), 0); e != nil {
		t.Fatal(e)
	}
	manualSpan, e := ev.CreateSpan(ctx, ea, es, evidence.SpanInput{SourceID: manual.SourceID, Revision: 1, Start: 0, End: 8}, uuid.NewString(), 1)
	if e != nil {
		t.Fatal(e)
	}
	manualClaim := claim
	manualClaim.ClaimID = uuid.NewString()
	manualClaim.ObjectEntityID = manualRole.EntityID
	manualClaim.SourceSpanIDs = []string{manualSpan.ID}
	if _, e = service.CreateClaim(ctx, owner, sc, manualClaim, uuid.NewString(), 0); e != nil {
		t.Fatal(e)
	}
	if _, e = ev.DecideAuthority(ctx, ea, es, manual.SourceID, evidence.AuthorityInput{SourceRevision: 1, ExpectedSourceStateRevision: 1, Verdict: "designate", Reason: "Owner designates report"}, uuid.NewString(), 0); e != nil {
		t.Fatal(e)
	}
	if _, e = service.DecideReview(ctx, owner, sc, manualClaim.ClaimID, ReviewInput{ClaimRevision: 1, Verdict: "approve", Reason: "Reviewed manual report"}, uuid.NewString(), 1); e != nil {
		t.Fatal(e)
	}
	mixedSnapshot, e := service.CreateSnapshot(ctx, owner, sc, SnapshotInput{SnapshotID: uuid.NewString(), Reason: "Mixed source fixture"}, uuid.NewString(), epoch())
	if e != nil {
		t.Fatal(e)
	}
	mixed := qualified
	mixed.FreshnessPolicyRevision = restoredPolicy.Revision
	mixed.SnapshotID = mixedSnapshot.SnapshotID
	mixed.AsOf = time.Now().UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano)
	mixedInventory := createInventory()
	if _, e = service.DecideRequirement(ctx, owner, sc, mixedInventory.InventoryID, requirement(mixedInventory, "responsible_role"), mixed, uuid.NewString(), 0); !errors.Is(e, ErrFreshnessBlocked) {
		t.Fatalf("mixed snapshot qualified: %v", e)
	}
	if _, e = ev.Revoke(ctx, ea, es, origin.SourceID, "Remove process provenance access", uuid.NewString(), 1); e != nil {
		t.Fatal(e)
	}
	if _, e = service.ReadCoverage(ctx, viewer, sc, second.InventoryID); !errors.Is(e, ErrNotFound) {
		t.Fatalf("revoked snapshot provenance disclosed: %v", e)
	}
}
