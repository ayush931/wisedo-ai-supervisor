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

func TestPostgresDiscoveryInventories(t *testing.T) {
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
	database := "discovery_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
		exec(`INSERT INTO supervisor.tenants(id,name) VALUES($1,'discovery synthetic')`, s.TenantID)
		exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'discovery synthetic')`, s.TenantID, s.SiteID)
	}
	actor := func(role string) Actor {
		t.Helper()
		a := Actor{"https://discovery.invalid", uuid.NewString(), strings.ReplaceAll(uuid.NewString(), "-", "") + strings.ReplaceAll(uuid.NewString(), "-", "")}
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
	origin := source()
	area := EntityInput{EntityID: uuid.NewString(), Type: "area", Name: "Production area", SourceID: origin.SourceID, SourceRevision: 1}
	process := EntityInput{EntityID: uuid.NewString(), Type: "process", Name: "Customer process", SourceID: origin.SourceID, SourceRevision: 1}
	for _, entity := range []EntityInput{area, process} {
		if _, e := service.CreateEntity(ctx, owner, sc, entity, uuid.NewString(), 0); e != nil {
			t.Fatal(e)
		}
	}
	_, err = service.CreateDiscoveryInventory(ctx, owner, sc, DiscoveryInput{}, uuid.NewString(), 0)
	if !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("empty inventory accepted: %v", err)
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
	in := DiscoveryInput{InventoryID: uuid.NewString(), AreaID: area.EntityID, AreaRevision: 1, ProcessID: process.EntityID, ProcessRevision: 1, Capability: "shortfall_investigation", Reason: "Discover production process"}
	key := uuid.NewString()
	for _, denied := range []Actor{operator, viewer} {
		if _, e := service.CreateDiscoveryInventory(ctx, denied, sc, in, key, 0); !errors.Is(e, ErrDenied) {
			t.Fatalf("role write allowed: %v", e)
		}
	}
	exec(`UPDATE supervisor.browser_sessions SET auth_time=NULL WHERE token_hash=$1`, supervisor.SessionHash)
	if _, e := service.CreateDiscoveryInventory(ctx, supervisor, sc, in, key, 0); !errors.Is(e, ErrReauthenticationRequired) {
		t.Fatalf("missing recent proof: %v", e)
	}
	exec(`UPDATE supervisor.browser_sessions SET auth_time=now() WHERE token_hash=$1`, supervisor.SessionHash)
	for _, bad := range []DiscoveryInput{
		{InventoryID: "invalid", AreaID: in.AreaID, AreaRevision: 1, ProcessID: in.ProcessID, ProcessRevision: 1, Capability: in.Capability, Reason: in.Reason},
		{InventoryID: in.InventoryID, AreaID: in.AreaID, AreaRevision: 0, ProcessID: in.ProcessID, ProcessRevision: 1, Capability: in.Capability, Reason: in.Reason},
		{InventoryID: in.InventoryID, AreaID: in.AreaID, AreaRevision: 1, ProcessID: in.ProcessID, ProcessRevision: 1, Capability: "invented", Reason: in.Reason},
		{InventoryID: in.InventoryID, AreaID: in.ProcessID, AreaRevision: 1, ProcessID: in.AreaID, ProcessRevision: 1, Capability: in.Capability, Reason: in.Reason},
	} {
		if _, e := service.CreateDiscoveryInventory(ctx, owner, sc, bad, uuid.NewString(), 0); !errors.Is(e, ErrInvalidContract) {
			t.Fatalf("bad input accepted: %+v %v", bad, e)
		}
	}
	created, err := service.CreateDiscoveryInventory(ctx, supervisor, sc, in, key, 0)
	if err != nil {
		t.Fatal(err)
	}
	if created.InventoryID != in.InventoryID || created.Revision != 1 || created.CurrentRevision != 1 || !created.ScopeCurrent || created.SchemaVersion != 1 || created.TemplateVersion != DiscoveryTemplateVersion || len(created.Requirements) != 9 || created.ActorIssuer != supervisor.Issuer || created.ActorSubject != supervisor.Subject || created.AuthenticatedAt.IsZero() || created.CreatedAt.IsZero() {
		t.Fatalf("inventory contract: %+v", created)
	}
	ids := map[string]string{}
	for _, r := range created.Requirements {
		if _, e := uuid.Parse(r.RequirementID); e != nil || r.Key == "" || r.Description == "" || r.InterviewRole == "" {
			t.Fatalf("invalid server requirement: %+v", r)
		}
		if _, exists := ids[r.Key]; exists {
			t.Fatal("duplicate requirement key")
		}
		ids[r.Key] = r.RequirementID
	}
	if epoch() != graphRevision {
		t.Fatal("discovery creation changed graph revision")
	}
	replayed, err := service.CreateDiscoveryInventory(ctx, supervisor, sc, in, key, 0)
	if err != nil || replayed.Revision != 1 || len(replayed.Requirements) != 9 {
		t.Fatalf("idempotent replay %+v %v", replayed, err)
	}
	for _, r := range replayed.Requirements {
		if ids[r.Key] != r.RequirementID {
			t.Fatal("replay changed denominator identity")
		}
	}
	if epoch() != graphRevision {
		t.Fatal("discovery replay changed graph revision")
	}
	// A late direct insert cannot expand an already committed fixed template.
	for _, commitScope := range []string{sc.TenantID, foreign.TenantID, ""} {
		late, e := runtime.BeginTx(ctx, nil)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = late.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, sc.TenantID); e != nil {
			late.Rollback()
			t.Fatal(e)
		}
		_, e = late.ExecContext(ctx, `INSERT INTO supervisor.knowledge_discovery_requirements(tenant_id,site_id,inventory_id,revision,requirement_key,requirement_id,definition) VALUES($1,$2,$3,1,'completion_evidence',$4,'{"key":"completion_evidence","description":"Unauthorized extra cell","interview_role":"owner"}'::jsonb)`, sc.TenantID, sc.SiteID, in.InventoryID, uuid.NewString())
		if e != nil {
			late.Rollback()
			t.Fatalf("deferred guard fixture insertion failed early: %v", e)
		}
		if _, e = late.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, commitScope); e != nil {
			late.Rollback()
			t.Fatal(e)
		}
		want := "23514"
		if commitScope != sc.TenantID {
			want = "42501"
		}
		requireSQLState(t, late.Commit(), want)
	}
	for _, reader := range []Actor{viewer, operator, owner} {
		got, e := service.ReadDiscoveryInventory(ctx, reader, sc, in.InventoryID, 0)
		if e != nil || got.Revision != 1 || !got.ScopeCurrent {
			t.Fatalf("memberread %+v %v", got, e)
		}
	}
	if _, e := service.ReadDiscoveryInventory(ctx, owner, foreign, in.InventoryID, 0); !errors.Is(e, ErrNotFound) {
		t.Fatalf("scope leak: %v", e)
	}
	changed := in
	changed.Reason = "Changed request"
	if _, e := service.CreateDiscoveryInventory(ctx, supervisor, sc, changed, key, 0); !errors.Is(e, ErrConflict) {
		t.Fatalf("key conflict: %v", e)
	}
	if _, e := service.CreateDiscoveryInventory(ctx, owner, sc, in, uuid.NewString(), 0); !errors.Is(e, ErrRevision) {
		t.Fatalf("CAS conflict: %v", e)
	}
	expanded := in
	expanded.Capability = "internal_task_coordination"
	if _, e := service.CreateDiscoveryInventory(ctx, owner, sc, expanded, uuid.NewString(), 1); !errors.Is(e, ErrDiscoveryConflict) {
		t.Fatalf("in-place capability expansion: %v", e)
	}
	expanded.InventoryID = uuid.NewString()
	expansion, err := service.CreateDiscoveryInventory(ctx, owner, sc, expanded, uuid.NewString(), 0)
	if err != nil || len(expansion.Requirements) != 11 {
		t.Fatalf("expanded template %+v %v", expansion, err)
	}
	for _, r := range expansion.Requirements {
		if r.RequirementID == ids[r.Key] {
			t.Fatal("new inventory inherited requirement identity")
		}
	}
	if epoch() != graphRevision {
		t.Fatal("expansion changed graph revision")
	}
	// Exactly one concurrent CAS update wins; both must not append revision2.
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			_, e := service.CreateDiscoveryInventory(ctx, owner, sc, changed, uuid.NewString(), 1)
			results <- e
		}()
	}
	a, b := <-results, <-results
	if !((a == nil && errors.Is(b, ErrRevision)) || (b == nil && errors.Is(a, ErrRevision))) {
		t.Fatalf("concurrentCAS %v %v", a, b)
	}
	current, err := service.ReadDiscoveryInventory(ctx, viewer, sc, in.InventoryID, 0)
	if err != nil || current.Revision != 2 {
		t.Fatalf("concurrent head %+v %v", current, err)
	}
	for _, r := range current.Requirements {
		if r.RequirementID != ids[r.Key] {
			t.Fatal("revision changed stable requirement ID")
		}
	}
	if epoch() != graphRevision {
		t.Fatal("requirementrevision changed graph inventory")
	}
	// Anchor revisions make old inventory projections stale, but history remains readable.
	area.Name = "Updated area"
	if _, e := service.CreateEntity(ctx, owner, sc, area, uuid.NewString(), 1); e != nil {
		t.Fatal(e)
	}
	graphRevision = epoch()
	old, err := service.ReadDiscoveryInventory(ctx, viewer, sc, in.InventoryID, 1)
	if err != nil || old.ScopeCurrent || old.AreaRevision != 1 || old.CurrentRevision != 2 || old.Reason != in.Reason {
		t.Fatalf("historical projection %+v %v", old, err)
	}
	if _, e := service.CreateDiscoveryInventory(ctx, owner, sc, in, uuid.NewString(), 2); !errors.Is(e, ErrSourceChanged) {
		t.Fatalf("stale anchor accepted: %v", e)
	}
	refresh := in
	refresh.AreaRevision = 2
	refresh.Reason = "Refresh pinned area"
	fresh, err := service.CreateDiscoveryInventory(ctx, owner, sc, refresh, uuid.NewString(), 2)
	if err != nil || fresh.Revision != 3 || !fresh.ScopeCurrent {
		t.Fatalf("anchor refresh %+v %v", fresh, err)
	}
	for _, r := range fresh.Requirements {
		if r.RequirementID != ids[r.Key] {
			t.Fatal("anchor refresh changed requirement ID")
		}
	}
	if epoch() != graphRevision {
		t.Fatal("anchor refresh inventory write bumped graph revision")
	}
	staleReplay, e := service.CreateDiscoveryInventory(ctx, supervisor, sc, in, key, 0)
	if e != nil || staleReplay.ScopeCurrent || staleReplay.Revision != 1 {
		t.Fatalf("stale original replay %+v %v", staleReplay, e)
	}
	exec(`UPDATE supervisor.browser_sessions SET auth_time=now()-interval '16 minutes' WHERE token_hash=$1`, supervisor.SessionHash)
	if _, e = service.CreateDiscoveryInventory(ctx, supervisor, sc, in, key, 0); !errors.Is(e, ErrReauthenticationRequired) {
		t.Fatalf("replay bypassed proof: %v", e)
	}
	exec(`UPDATE supervisor.browser_sessions SET auth_time=now() WHERE token_hash=$1`, supervisor.SessionHash)
	// Audit failure must roll back initial inventory, head, revision and requirement rows.
	exec(`UPDATE supervisor.memberships SET role='viewer' WHERE tenant_id=$1 AND site_id=$2 AND issuer=$3 AND subject=$4`, sc.TenantID, sc.SiteID, supervisor.Issuer, supervisor.Subject)
	if _, e = service.CreateDiscoveryInventory(ctx, supervisor, sc, in, key, 0); !errors.Is(e, ErrDenied) {
		t.Fatalf("replay bypassed current role: %v", e)
	}
	exec(`UPDATE supervisor.memberships SET role='supervisor' WHERE tenant_id=$1 AND site_id=$2 AND issuer=$3 AND subject=$4`, sc.TenantID, sc.SiteID, supervisor.Issuer, supervisor.Subject)
	rollback := refresh
	rollback.InventoryID = uuid.NewString()
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
	_, e = service.CreateDiscoveryInventory(short, owner, sc, rollback, rollbackKey, 0)
	stop()
	if e == nil {
		t.Fatal("audit failure accepted inventory")
	}
	if e = blocker.Rollback(); e != nil {
		t.Fatal(e)
	}
	for _, table := range []string{"knowledge_discovery_inventories", "knowledge_discovery_heads", "knowledge_discovery_revisions", "knowledge_discovery_requirements"} {
		var count int
		if e = admin.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.`+table+` WHERE tenant_id=$1 AND site_id=$2 AND inventory_id=$3`, sc.TenantID, sc.SiteID, rollback.InventoryID).Scan(&count); e != nil || count != 0 {
			t.Fatalf("rollback %s count%d %v", table, count, e)
		}
	}
	if _, e = service.CreateDiscoveryInventory(ctx, owner, sc, rollback, rollbackKey, 0); e != nil {
		t.Fatalf("rollback samekey retry: %v", e)
	}
	if epoch() != graphRevision {
		t.Fatal("rollback/retry changed graph inventory")
	}
	attempt, e := runtime.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = attempt.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, foreign.TenantID); e != nil {
		t.Fatal(e)
	}
	var visible int
	if e = attempt.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.knowledge_discovery_revisions WHERE tenant_id=$1`, sc.TenantID).Scan(&visible); e != nil || visible != 0 {
		t.Fatalf("foreign inventory rows%d %v", visible, e)
	}
	attempt.Rollback()
	for _, query := range []string{`UPDATE supervisor.knowledge_discovery_revisions SET reason='forged' WHERE inventory_id=$1`, `DELETE FROM supervisor.knowledge_discovery_requirements WHERE inventory_id=$1`} {
		tx, e := runtime.BeginTx(ctx, nil)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, sc.TenantID); e != nil {
			t.Fatal(e)
		}
		_, e = tx.ExecContext(ctx, query, in.InventoryID)
		tx.Rollback()
		requireSQLState(t, e, "42501")
	}
	if _, e = ev.Revoke(ctx, ea, es, origin.SourceID, "Anchor evidence removed", uuid.NewString(), 1); e != nil {
		t.Fatal(e)
	}
	if _, e = service.ReadDiscoveryInventory(ctx, viewer, sc, in.InventoryID, 1); !errors.Is(e, ErrNotFound) {
		t.Fatalf("revoked anchor historical read: %v", e)
	}
	if _, e = service.CreateDiscoveryInventory(ctx, supervisor, sc, in, key, 0); !errors.Is(e, ErrNotFound) {
		t.Fatalf("revoked anchor replay: %v", e)
	}
}
