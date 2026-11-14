//go:build integration

package knowledge

import (
	"context"
	"database/sql"
	"encoding/json"
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

// TestPostgresValueStreamCoverage (WIS-615) drives a process_understanding cell from missing to
// approved through the real service as the runtime role: document-sourced value-stream claims are
// reviewed into a snapshot, and with a freshness policy and an owner confirmation a connected
// material_suppliers witness binds and qualifies. A witness not linked to the process is refused
// and writes nothing, and interview-sourced claims never qualify: with a policy the binding is
// refused, without one the cell stays a candidate.
func TestPostgresValueStreamCoverage(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	adminURL, runtimeURL := os.Getenv("SUPERVISOR_TEST_ADMIN_URL"), os.Getenv("SUPERVISOR_TEST_DATABASE_URL")
	if adminURL == "" || runtimeURL == "" {
		t.Fatal("BLOCKED: real PostgreSQL URLs required")
	}
	control, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { control.Close() })
	database := "value_stream_coverage_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	sc := Scope{uuid.NewString(), uuid.NewString()}
	exec(`INSERT INTO supervisor.tenants(id,name) VALUES($1,'value stream coverage')`, sc.TenantID)
	exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'value stream coverage')`, sc.TenantID, sc.SiteID)
	actor := func(role string) Actor {
		t.Helper()
		a := Actor{"https://value-stream-coverage.invalid", uuid.NewString(), strings.ReplaceAll(uuid.NewString(), "-", "") + strings.ReplaceAll(uuid.NewString(), "-", "")}
		exec(`INSERT INTO supervisor.browser_sessions(token_hash,issuer,subject,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,'fixture',now(),now(),now()+interval '1 hour',now())`, a.SessionHash, a.Issuer, a.Subject)
		exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,$5)`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, role)
		return a
	}
	owner, supervisor, viewer := actor("owner"), actor("supervisor"), actor("viewer")
	service, ev := New(runtime), evidence.New(runtime)
	ea := evidence.Actor{Issuer: owner.Issuer, Subject: owner.Subject, SessionHash: owner.SessionHash}
	es := evidence.Scope{TenantID: sc.TenantID, SiteID: sc.SiteID}

	// One routing document; every fact cites a span of it and the owner designates it.
	source := func(kind, text string) (evidence.Input, evidence.Span) {
		t.Helper()
		in := evidence.Input{SourceID: uuid.NewString(), Kind: kind, Title: "Value stream " + kind, Text: text}
		if _, e := ev.CreateOrRevise(ctx, ea, es, in, uuid.NewString(), 0); e != nil {
			t.Fatal(e)
		}
		span, e := ev.CreateSpan(ctx, ea, es, evidence.SpanInput{SourceID: in.SourceID, Revision: 1, Start: 0, End: len([]rune(text))}, uuid.NewString(), 1)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = ev.DecideAuthority(ctx, ea, es, in.SourceID, evidence.AuthorityInput{SourceRevision: 1, ExpectedSourceStateRevision: 1, Verdict: "designate", Reason: "Reviewed plant record"}, uuid.NewString(), 0); e != nil {
			t.Fatal(e)
		}
		return in, span
	}
	document, docSpan := source("document", "Bracket production: the press consumes steel coil from Bharat Steel, lead time 14 days.")
	entity := func(kind, name string) string {
		t.Helper()
		in := EntityInput{EntityID: uuid.NewString(), Type: kind, Name: name, SourceID: document.SourceID, SourceRevision: 1}
		if _, e := service.CreateEntity(ctx, owner, sc, in, uuid.NewString(), 0); e != nil {
			t.Fatalf("%s entity: %v", kind, e)
		}
		return in.EntityID
	}
	area, process := entity("area", "Press shop"), entity("process", "Bracket production")
	press, coil, mill := entity("operation", "Press"), entity("material", "Steel coil"), entity("supplier", "Bharat Steel")
	approve := func(subject, predicate, object string, literal *Literal, spanID string) ConflictClaimRef {
		t.Helper()
		c := ClaimInput{ClaimID: uuid.NewString(), SubjectID: subject, SubjectRevision: 1, Predicate: predicate, EffectiveFrom: "2026-10-01T00:00:00Z", EvidenceClass: "report", SourceSpanIDs: []string{spanID}}
		if object != "" {
			c.ObjectEntityID, c.ObjectRevision = object, 1
		} else {
			c.Literal = literal
		}
		if _, e := service.CreateClaim(ctx, owner, sc, c, uuid.NewString(), 0); e != nil {
			t.Fatalf("%s claim: %v", predicate, e)
		}
		if _, e := service.DecideReview(ctx, supervisor, sc, c.ClaimID, ReviewInput{ClaimRevision: 1, Verdict: "approve", Reason: "Matches the routing document"}, uuid.NewString(), 1); e != nil {
			t.Fatalf("%s review: %v", predicate, e)
		}
		return ConflictClaimRef{ClaimID: c.ClaimID, Revision: 1}
	}
	lead, _ := json.Marshal("14")
	part := approve(press, "part_of", process, nil, docSpan.ID)
	consumes := approve(press, "consumes", coil, nil, docSpan.ID)
	supplied := approve(coil, "supplied_by", mill, nil, docSpan.ID)
	leadTime := approve(coil, "lead_time_days", "", &Literal{Type: "decimal", Value: lead, Unit: "day"}, docSpan.ID)

	snapshotNow := func() Snapshot {
		t.Helper()
		inv, e := service.ReadInventory(ctx, owner, sc)
		if e != nil {
			t.Fatal(e)
		}
		s, e := service.CreateSnapshot(ctx, owner, sc, SnapshotInput{SnapshotID: uuid.NewString(), Reason: "Reviewed value stream"}, uuid.NewString(), inv.Revision)
		if e != nil {
			t.Fatal(e)
		}
		return s
	}
	snapshot := snapshotNow()
	discovery, e := service.CreateDiscoveryInventory(ctx, owner, sc, DiscoveryInput{InventoryID: uuid.NewString(), AreaID: area, AreaRevision: 1, ProcessID: process, ProcessRevision: 1, Capability: "process_understanding", Reason: "Onboard the press shop"}, uuid.NewString(), 0)
	if e != nil {
		t.Fatal(e)
	}
	requirement := func(key string) string {
		for _, r := range discovery.Requirements {
			if r.Key == key {
				return r.RequirementID
			}
		}
		t.Fatalf("missing requirement %s", key)
		return ""
	}
	cell := func(key string) CoverageRequirement {
		t.Helper()
		coverage, e := service.ReadCoverage(ctx, viewer, sc, discovery.InventoryID)
		if e != nil {
			t.Fatal(e)
		}
		for _, r := range coverage.Requirements {
			if !r.BindingSupported {
				t.Fatalf("value-stream cell %s does not support binding", r.Key)
			}
		}
		for _, r := range coverage.Requirements {
			if r.Key == key {
				return r
			}
		}
		t.Fatalf("coverage has no %s cell", key)
		return CoverageRequirement{}
	}
	ttl := int64(3600)
	policy, e := service.DecideFreshnessPolicy(ctx, owner, sc, FreshnessPolicyInput{Verdict: "define", MaxAgeSeconds: &ttl, Reason: "Owner policy for verified documents"}, uuid.NewString(), 0)
	if e != nil {
		t.Fatal(e)
	}
	confirm := func(sourceID string) VerificationRef {
		t.Helper()
		a, err := ev.ReadAuthority(ctx, ea, es, sourceID, 0)
		if err != nil {
			t.Fatal(err)
		}
		v, err := ev.DecideCurrentness(ctx, ea, es, sourceID, evidence.CurrentnessInput{SourceRevision: 1, ExpectedSourceStateRevision: 1, AuthorityRevision: a.Revision, AuthorityDecisionID: a.DecisionID, Verdict: "confirm_current", Reason: "Owner checks the live document"}, uuid.NewString(), 0)
		if err != nil {
			t.Fatal(err)
		}
		return VerificationRef{SourceID: sourceID, VerificationID: v.VerificationID, VerificationRevision: v.Revision}
	}
	docConfirmation := confirm(document.SourceID)
	bind := func(s Snapshot, refs []ConflictClaimRef, verifications []VerificationRef) RequirementDecisionInput {
		in := RequirementDecisionInput{InventoryRevision: 1, Verdict: "bind", Reason: "Reviewed supplier evidence", SnapshotID: s.SnapshotID, AsOf: time.Now().UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano), ClaimRefs: refs}
		if verifications != nil {
			in.FreshnessPolicyRevision, in.VerificationRefs = policy.Revision, verifications
		}
		return in
	}
	if c := cell("material_suppliers"); c.State != "missing" || c.ReasonCode != "decision_missing" {
		t.Fatalf("material_suppliers before binding: %+v", c)
	}

	// A supplier fact not linked to the interviewed process is not evidence about its value stream.
	disconnected := bind(snapshot, []ConflictClaimRef{supplied, leadTime}, []VerificationRef{docConfirmation})
	if _, e = service.DecideRequirement(ctx, owner, sc, discovery.InventoryID, requirement("material_suppliers"), disconnected, uuid.NewString(), 0); !errors.Is(e, ErrBindingBlocked) {
		t.Fatalf("disconnected supplier witness: %v", e)
	}
	if c := cell("material_suppliers"); c.State != "missing" || c.DecisionID != "" {
		t.Fatalf("a refused binding wrote a decision: %+v", c)
	}
	// The connected witness binds and qualifies on the confirmed document.
	connected := bind(snapshot, []ConflictClaimRef{part, consumes, supplied, leadTime}, []VerificationRef{docConfirmation})
	if _, e = service.DecideRequirement(ctx, owner, sc, discovery.InventoryID, requirement("material_suppliers"), connected, uuid.NewString(), 0); e != nil {
		t.Fatalf("connected supplier witness: %v", e)
	}
	if c := cell("material_suppliers"); c.State != "approved" || c.ReasonCode != "qualified_document_evidence" {
		t.Fatalf("material_suppliers after a qualified binding: %+v", c)
	}

	// WIS-637 review: a decision accepted before the WIS-615 process-scope fix, whose witness reaches
	// another process's operation only through a precedes edge, must not stay approved coverage.
	// Today the write is refused; the pre-fix row is seeded as the privileged connection.
	foreign := entity("operation", "Paint line")
	toForeign := approve(press, "precedes", foreign, nil, docSpan.ID)
	cycleSeconds, _ := json.Marshal("42")
	foreignCycle := approve(foreign, "cycle_time_seconds", "", &Literal{Type: "decimal", Value: cycleSeconds, Unit: "s"}, docSpan.ID)
	scoped := snapshotNow()
	oldWitness := bind(scoped, []ConflictClaimRef{part, toForeign, foreignCycle}, []VerificationRef{docConfirmation})
	if _, e = service.DecideRequirement(ctx, owner, sc, discovery.InventoryID, requirement("cycle_setup_times"), oldWitness, uuid.NewString(), 0); !errors.Is(e, ErrBindingBlocked) {
		t.Fatalf("a foreign operation's cycle time bound: %v", e)
	}
	// Keep one approved cell current on the new snapshot, so the counts below would move if the
	// rejected decision were counted.
	if _, e = service.DecideRequirement(ctx, owner, sc, discovery.InventoryID, requirement("material_suppliers"), bind(scoped, []ConflictClaimRef{part, consumes, supplied, leadTime}, []VerificationRef{docConfirmation}), uuid.NewString(), 1); e != nil {
		t.Fatalf("rebind suppliers on the current snapshot: %v", e)
	}
	counts := func() (int, int) {
		t.Helper()
		coverage, err := service.ReadCoverage(ctx, viewer, sc, discovery.InventoryID)
		if err != nil {
			t.Fatal(err)
		}
		return coverage.ApprovedCount, coverage.BoundCount
	}
	if approved, bound := counts(); approved != 1 || bound != 1 {
		t.Fatalf("before seeding: approved %d bound %d, want 1 and 1", approved, bound)
	}
	seed, e := admin.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	seedExec := func(q string, args ...any) {
		t.Helper()
		if _, err := seed.ExecContext(ctx, q, args...); err != nil {
			seed.Rollback()
			t.Fatal(err)
		}
	}
	cycleReq := requirement("cycle_setup_times")
	seedExec(`SELECT set_config('supervisor.tenant_id',$1,true)`, sc.TenantID)
	seedExec(`INSERT INTO supervisor.knowledge_requirement_decision_heads(tenant_id,site_id,inventory_id,inventory_revision,requirement_id,current_revision) VALUES($1,$2,$3,1,$4,1)`, sc.TenantID, sc.SiteID, discovery.InventoryID, cycleReq)
	seedExec(`INSERT INTO supervisor.knowledge_requirement_decisions(tenant_id,site_id,inventory_id,inventory_revision,requirement_id,revision,decision_id,verdict,reason,actor_issuer,actor_subject,auth_time,snapshot_id,snapshot_sha256,snapshot_inventory_revision,as_of,claim_count,freshness_policy_revision,verification_count) VALUES($1,$2,$3,1,$4,1,$5,'bind','Accepted before the scope fix',$6,$7,now(),$8,$9,$10,$11,3,$12,1)`,
		sc.TenantID, sc.SiteID, discovery.InventoryID, cycleReq, uuid.NewString(), owner.Issuer, owner.Subject, scoped.SnapshotID, scoped.SHA256, scoped.InventoryRevision, oldWitness.AsOf, policy.Revision)
	for _, ref := range oldWitness.ClaimRefs {
		seedExec(`INSERT INTO supervisor.knowledge_requirement_decision_claims(tenant_id,site_id,inventory_id,inventory_revision,requirement_id,decision_revision,snapshot_id,claim_id,claim_revision) VALUES($1,$2,$3,1,$4,1,$5,$6,$7)`, sc.TenantID, sc.SiteID, discovery.InventoryID, cycleReq, scoped.SnapshotID, ref.ClaimID, ref.Revision)
	}
	seedExec(`INSERT INTO supervisor.knowledge_requirement_decision_verifications(tenant_id,site_id,inventory_id,inventory_revision,requirement_id,decision_revision,source_id,source_revision,source_sha256,source_access_epoch,verification_id,verification_revision)
		SELECT tenant_id,site_id,inventory_id,inventory_revision,$4,1,source_id,source_revision,source_sha256,source_access_epoch,verification_id,verification_revision FROM supervisor.knowledge_requirement_decision_verifications WHERE tenant_id=$1 AND site_id=$2 AND inventory_id=$3 AND requirement_id=$5 AND decision_revision=1`,
		sc.TenantID, sc.SiteID, discovery.InventoryID, cycleReq, requirement("material_suppliers"))
	if e = seed.Commit(); e != nil {
		t.Fatal(e)
	}
	if c := cell("cycle_setup_times"); c.State != "stale" || c.ReasonCode != "witness_scope_invalid" || c.DecisionRevision != 1 {
		t.Fatalf("a pre-fix out-of-scope witness is still coverage: %+v", c)
	}
	if approved, bound := counts(); approved != 1 || bound != 1 {
		t.Fatalf("out-of-scope witness counted: approved %d bound %d, want 1 and 1", approved, bound)
	}
	// History is kept: the decision still reads, but it is not eligible.
	if d, err := service.ReadRequirementDecision(ctx, viewer, sc, discovery.InventoryID, cycleReq, 1, 0); err != nil || d.Revision != 1 || d.DecisionEligible {
		t.Fatalf("pre-fix decision read: %+v %v", d, err)
	}
	// A person re-decides it: the withdrawal is accepted and the cell is plainly missing again.
	if _, e = service.DecideRequirement(ctx, owner, sc, discovery.InventoryID, cycleReq, RequirementDecisionInput{InventoryRevision: 1, Verdict: "withdraw", Reason: "Witness used another process's operation"}, uuid.NewString(), 1); e != nil {
		t.Fatalf("withdraw the pre-fix decision: %v", e)
	}
	if c := cell("cycle_setup_times"); c.State != "missing" || c.ReasonCode != "decision_withdrawn" {
		t.Fatalf("after withdrawal: %+v", c)
	}

	// Interview evidence never qualifies: once an interview-sourced claim is in the snapshot, a
	// binding with the policy is refused, and without the policy the cell is only a candidate.
	interview, interviewSpan := source("interview", "The shift supervisor owns bracket production.")
	role := entity("role", "Shift supervisor")
	responsible := approve(process, "responsible_role", role, nil, interviewSpan.ID)
	withInterview := snapshotNow()
	authority, e := ev.ReadAuthority(ctx, ea, es, interview.SourceID, 0)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = ev.DecideCurrentness(ctx, ea, es, interview.SourceID, evidence.CurrentnessInput{SourceRevision: 1, ExpectedSourceStateRevision: 1, AuthorityRevision: authority.Revision, AuthorityDecisionID: authority.DecisionID, Verdict: "confirm_current", Reason: "Owner checks the interview"}, uuid.NewString(), 0); !errors.Is(e, evidence.ErrInvalid) {
		t.Fatalf("an interview answer was confirmed current: %v", e)
	}
	if _, e = service.DecideRequirement(ctx, owner, sc, discovery.InventoryID, requirement("stage_responsibility"), bind(withInterview, []ConflictClaimRef{responsible}, []VerificationRef{docConfirmation}), uuid.NewString(), 0); !errors.Is(e, ErrFreshnessBlocked) {
		t.Fatalf("interview evidence qualified: %v", e)
	}
	if _, e = service.DecideRequirement(ctx, owner, sc, discovery.InventoryID, requirement("stage_responsibility"), bind(withInterview, []ConflictClaimRef{responsible}, nil), uuid.NewString(), 0); e != nil {
		t.Fatalf("unqualified interview binding: %v", e)
	}
	if c := cell("stage_responsibility"); c.State != "candidate" || c.ReasonCode != "freshness_policy_missing" {
		t.Fatalf("stage_responsibility from interview evidence: %+v", c)
	}
}
