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

// TestPostgresValueStreamVocabulary (WIS-611) drives the value-stream vocabulary through the real
// service as the runtime role: a supplier-to-dispatch process is recorded with the new entity types,
// relationships and properties, reviewed, published in an approved snapshot and read back through
// the graph. Single-valued properties conflict, many-valued relationships do not, the database
// refuses an unknown entity type, and a process_understanding inventory has exactly its template.
func TestPostgresValueStreamVocabulary(t *testing.T) {
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
	database := "value_stream_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	exec(`INSERT INTO supervisor.tenants(id,name) VALUES($1,'value stream synthetic')`, sc.TenantID)
	exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'value stream synthetic')`, sc.TenantID, sc.SiteID)
	actor := func(role string) Actor {
		t.Helper()
		a := Actor{"https://value-stream.invalid", uuid.NewString(), strings.ReplaceAll(uuid.NewString(), "-", "") + strings.ReplaceAll(uuid.NewString(), "-", "")}
		exec(`INSERT INTO supervisor.browser_sessions(token_hash,issuer,subject,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,'fixture',now(),now(),now()+interval '1 hour',now())`, a.SessionHash, a.Issuer, a.Subject)
		exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,$5)`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, role)
		return a
	}
	owner, supervisor, viewer := actor("owner"), actor("supervisor"), actor("viewer")
	service, ev := New(runtime), evidence.New(runtime)
	ea := evidence.Actor{Issuer: owner.Issuer, Subject: owner.Subject, SessionHash: owner.SessionHash}
	es := evidence.Scope{TenantID: sc.TenantID, SiteID: sc.SiteID}

	// One interview answer describes the stream; every fact cites a span of it.
	answer := evidence.Input{SourceID: uuid.NewString(), Kind: "interview", Title: "Supervisor interview",
		Text: "Steel coil comes from Bharat Steel, about 14 days lead time. It is received and checked at Gate 2 incoming QC, then stored in Rack A. The press takes 42 seconds a part with a 900 second die change. Brackets ship to Apex Motors."}
	if _, e := ev.CreateOrRevise(ctx, ea, es, answer, uuid.NewString(), 0); e != nil {
		t.Fatal(e)
	}
	span, e := ev.CreateSpan(ctx, ea, es, evidence.SpanInput{SourceID: answer.SourceID, Revision: 1, Start: 0, End: 60}, uuid.NewString(), 1)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = ev.DecideAuthority(ctx, ea, es, answer.SourceID, evidence.AuthorityInput{SourceRevision: 1, ExpectedSourceStateRevision: 1, Verdict: "designate", Reason: "Supervisor account of the line"}, uuid.NewString(), 0); e != nil {
		t.Fatal(e)
	}
	entity := func(kind, name string) string {
		t.Helper()
		in := EntityInput{EntityID: uuid.NewString(), Type: kind, Name: name, SourceID: answer.SourceID, SourceRevision: 1}
		if kind == "asset" {
			in.AssetKind = "line"
		}
		if _, e := service.CreateEntity(ctx, owner, sc, in, uuid.NewString(), 0); e != nil {
			t.Fatalf("%s entity: %v", kind, e)
		}
		return in.EntityID
	}
	coil, bracket := entity("material", "Steel coil"), entity("material", "Bracket")
	supplier, customer := entity("supplier", "Bharat Steel"), entity("customer", "Apex Motors")
	rack, gate := entity("location", "Rack A"), entity("location", "Gate 2")
	incomingQC := entity("inspection_point", "Gate 2 incoming QC")
	press := entity("operation", "Press bracket")

	claim := func(subject, predicate, object string, literal *Literal) ClaimInput {
		c := ClaimInput{ClaimID: uuid.NewString(), SubjectID: subject, SubjectRevision: 1, Predicate: predicate, EffectiveFrom: "2026-10-01T00:00:00Z", EvidenceClass: "report", SourceSpanIDs: []string{span.ID}}
		if object != "" {
			c.ObjectEntityID, c.ObjectRevision = object, 1
		} else {
			c.Literal = literal
		}
		return c
	}
	decimal := func(v, unit string) *Literal {
		raw, _ := json.Marshal(v)
		return &Literal{Type: "decimal", Value: raw, Unit: unit}
	}
	text := func(v string) *Literal {
		raw, _ := json.Marshal(v)
		return &Literal{Type: "string", Value: raw}
	}
	stream := []ClaimInput{
		claim(coil, "supplied_by", supplier, nil),
		claim(coil, "received_at", gate, nil),
		claim(coil, "stored_at", rack, nil),
		claim(coil, "inspected_at", incomingQC, nil),
		claim(press, "inspected_at", incomingQC, nil),
		claim(bracket, "ships_to", customer, nil),
		claim(coil, "lead_time_days", "", decimal("14", "day")),
		claim(supplier, "lead_time_days", "", decimal("14", "day")),
		claim(press, "cycle_time_seconds", "", decimal("42", "s")),
		claim(press, "setup_time_seconds", "", decimal("900", "s")),
		claim(press, "batch_size", "", decimal("500", "count")),
		claim(press, "yield_ratio", "", decimal("0.985", "")),
		claim(incomingQC, "sample_size", "", decimal("5", "count")),
		claim(incomingQC, "inspection_method", "", text("Thickness gauge on every fifth coil")),
	}
	for _, c := range stream {
		if _, e = service.CreateClaim(ctx, owner, sc, c, uuid.NewString(), 0); e != nil {
			t.Fatalf("%s claim: %v", c.Predicate, e)
		}
		if _, e = service.DecideReview(ctx, supervisor, sc, c.ClaimID, ReviewInput{ClaimRevision: 1, Verdict: "approve", Reason: "Matches the supervisor account"}, uuid.NewString(), 1); e != nil {
			t.Fatalf("%s review: %v", c.Predicate, e)
		}
	}

	// The persisted types decide the pair, not the caller: a reversed or mismatched pair, a
	// property on the wrong subject and a yield above one are all refused at the write boundary.
	for name, c := range map[string]ClaimInput{
		"reversed supplied_by":       claim(supplier, "supplied_by", coil, nil),
		"ships_to a supplier":        claim(bracket, "ships_to", supplier, nil),
		"cycle time of a material":   claim(coil, "cycle_time_seconds", "", decimal("42", "s")),
		"yield above one":            claim(press, "yield_ratio", "", decimal("1.2", "")),
		"inspection method as count": claim(incomingQC, "inspection_method", "", decimal("3", "")),
	} {
		if _, e = service.CreateClaim(ctx, owner, sc, c, uuid.NewString(), 0); !errors.Is(e, ErrInvalidContract) {
			t.Errorf("%s: want ErrInvalidContract, got %v", name, e)
		}
	}

	// The reviewed stream is published and traversable: every approved claim, by its ID, is in
	// the snapshot's graph, the cycle time included (published before any second value contests it).
	inventory, e := service.ReadInventory(ctx, owner, sc)
	if e != nil {
		t.Fatal(e)
	}
	snapshot, e := service.CreateSnapshot(ctx, owner, sc, SnapshotInput{SnapshotID: uuid.NewString(), Reason: "Reviewed value stream"}, uuid.NewString(), inventory.Revision)
	if e != nil {
		t.Fatal(e)
	}
	raw, e := service.ReadGraph(ctx, viewer, sc, snapshot.SnapshotID, GraphQuery{Direction: "outgoing", Roots: []string{coil, press, incomingQC, bracket, supplier},
		Predicates: []string{"supplied_by", "received_at", "stored_at", "inspected_at", "ships_to", "lead_time_days", "cycle_time_seconds", "setup_time_seconds", "batch_size", "yield_ratio", "sample_size", "inspection_method"},
		AsOf:       "2026-10-15T00:00:00Z", Hops: 1, MaxEntities: 200, MaxClaims: 500})
	if e != nil {
		t.Fatal(e)
	}
	var graph GraphResult
	if e = json.Unmarshal(raw, &graph); e != nil {
		t.Fatal(e)
	}
	inGraph := map[string]bool{}
	for _, c := range graph.Claims {
		inGraph[c.ClaimID] = true
	}
	for _, c := range stream {
		if !inGraph[c.ClaimID] {
			t.Errorf("approved %s claim %s missing from the snapshot graph", c.Predicate, c.ClaimID)
		}
	}

	// One operation has one cycle time at a time: a second value is a conflict for review. A
	// second supplier of the same material is not.
	second := claim(press, "cycle_time_seconds", "", decimal("48", "s"))
	if _, e = service.CreateClaim(ctx, owner, sc, second, uuid.NewString(), 0); e != nil {
		t.Fatal(e)
	}
	page, e := service.ListConflicts(ctx, viewer, sc, second.ClaimID, 1, 50, "")
	if e != nil {
		t.Fatal(e)
	}
	if len(page.Conflicts) != 1 || page.Conflicts[0].Kind != "value_conflict" {
		t.Fatalf("two cycle times: %+v", page.Conflicts)
	}
	another := entity("supplier", "Second mill")
	secondSupplier := claim(coil, "supplied_by", another, nil)
	if _, e = service.CreateClaim(ctx, owner, sc, secondSupplier, uuid.NewString(), 0); e != nil {
		t.Fatal(e)
	}
	if page, e = service.ListConflicts(ctx, viewer, sc, secondSupplier.ClaimID, 1, 50, ""); e != nil || len(page.Conflicts) != 0 {
		t.Fatalf("a second supplier is not a conflict: %+v %v", page.Conflicts, e)
	}

	// The database holds the entity-type set for any writer: a row consistent in every other
	// respect, with an unknown type, is refused by the type CHECK itself.
	robot := uuid.NewString()
	if _, e = admin.ExecContext(ctx, `INSERT INTO supervisor.knowledge_entities(tenant_id,site_id,id,entity_type,payload,source_id,source_revision,actor_issuer,actor_subject)
 VALUES($1,$2,$3,'warehouse_robot',jsonb_build_object('entity_id',$5::text,'type','warehouse_robot','source_id',$6::text,'source_revision',1),$4,1,'test','fixture')`,
		sc.TenantID, sc.SiteID, robot, answer.SourceID, robot, answer.SourceID); e == nil || !strings.Contains(e.Error(), "knowledge_entities_entity_type_check") {
		t.Fatalf("an unknown entity type was stored: %v", e)
	}

	// process_understanding: the value-stream template, exactly, and its coverage reads back.
	area, process := entity("area", "Press shop"), entity("process", "Bracket production")
	discovery, e := service.CreateDiscoveryInventory(ctx, owner, sc, DiscoveryInput{InventoryID: uuid.NewString(), AreaID: area, AreaRevision: 1, ProcessID: process, ProcessRevision: 1, Capability: "process_understanding", Reason: "Onboard the press shop"}, uuid.NewString(), 0)
	if e != nil {
		t.Fatal(e)
	}
	coverage, e := service.ReadCoverage(ctx, viewer, sc, discovery.InventoryID)
	if e != nil {
		t.Fatal(e)
	}
	template, _ := DiscoveryTemplate("process_understanding")
	if len(coverage.Requirements) != len(template) || coverage.ApplicableCount != len(template) || coverage.KnowledgeReady || coverage.AuthorizationState != "disabled" {
		t.Fatalf("process_understanding coverage: %+v", coverage)
	}
	roles := map[string]string{}
	for _, d := range template {
		roles[d.Key] = d.InterviewRole
	}
	for _, r := range coverage.Requirements { // listed by key, not template order
		if role, ok := roles[r.Key]; !ok || r.InterviewRole != role || r.State != "missing" {
			t.Fatalf("cell %+v", r)
		}
		delete(roles, r.Key)
	}
	// The guard requires exactly the capability's key set at commit: an extra requirement on the
	// value-stream inventory is refused even though it is a known key.
	tx, e := admin.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback()
	if _, e = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, sc.TenantID); e != nil {
		t.Fatal(e)
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO supervisor.knowledge_discovery_requirements(tenant_id,site_id,inventory_id,revision,requirement_key,requirement_id,definition)
 VALUES($1,$2,$3,1,'asset_identity',$4,'{"key":"asset_identity","description":"Smuggled cell","interview_role":"supervisor"}')`, sc.TenantID, sc.SiteID, discovery.InventoryID, uuid.NewString()); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(); e == nil || !strings.Contains(e.Error(), "Incomplete discovery template") {
		t.Fatalf("a value-stream inventory with a foreign cell was committed: %v", e)
	}
}
