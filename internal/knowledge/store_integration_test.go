//go:build integration

package knowledge

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/evidence"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// requireSQLState pins the database rejection reason so a typo in a fixture
// statement cannot pass as an intended privilege or integrity denial.
func requireSQLState(t *testing.T, err error, want string) {
	t.Helper()
	var state interface{ SQLState() string }
	if !errors.As(err, &state) || state.SQLState() != want {
		t.Fatalf("want SQLSTATE %s, got %v", want, err)
	}
}

func TestPostgresCandidateKnowledgeProvenanceAndConcurrency(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	adminURL, runtimeURL := os.Getenv("SUPERVISOR_TEST_ADMIN_URL"), os.Getenv("SUPERVISOR_TEST_DATABASE_URL")
	if adminURL == "" || runtimeURL == "" {
		t.Fatal("real PostgreSQL URLs required")
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
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := admin.ExecContext(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	sc, foreign := Scope{uuid.NewString(), uuid.NewString()}, Scope{uuid.NewString(), uuid.NewString()}
	issuer := "https://knowledge.example/realm"
	makeActor := func(role string) Actor {
		t.Helper()
		sum := sha256.Sum256([]byte(uuid.NewString()))
		a := Actor{issuer, uuid.NewString(), hex.EncodeToString(sum[:])}
		exec(`INSERT INTO supervisor.browser_sessions(token_hash,subject,issuer,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,'fixture',now(),now(),now()+interval '8 hours',now())`, a.SessionHash, a.Subject, a.Issuer)
		exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,$5)`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, role)
		return a
	}
	for _, scope := range []Scope{sc, foreign} {
		exec(`INSERT INTO supervisor.tenants(id,name) VALUES($1,'candidate fixture')`, scope.TenantID)
		exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'candidate line')`, scope.TenantID, scope.SiteID)
	}
	owner, viewer := makeActor("owner"), makeActor("viewer")
	exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'owner')`, foreign.TenantID, foreign.SiteID, owner.Issuer, owner.Subject)
	// These synthetic provenance fixtures remain until this explicitly disposable database is removed.
	ev := evidence.New(runtime)
	ea := evidence.Actor{Issuer: owner.Issuer, Subject: owner.Subject, SessionHash: owner.SessionHash}
	es := evidence.Scope{TenantID: sc.TenantID, SiteID: sc.SiteID}
	source := func(scope evidence.Scope, title string) (evidence.Input, evidence.Source) {
		t.Helper()
		in := evidence.Input{SourceID: uuid.NewString(), Kind: "manual", Title: title, Text: "Observed process steps."}
		out, e := ev.CreateOrRevise(ctx, ea, scope, in, uuid.NewString(), 0)
		if e != nil {
			t.Fatal(e)
		}
		return in, out
	}
	entityInput, entitySource := source(es, "Entity origin")
	citationInput, citationSource := source(es, "Claim citation")
	span, e := ev.CreateSpan(ctx, ea, es, evidence.SpanInput{SourceID: citationSource.SourceID, Revision: 1, Start: 0, End: 8}, uuid.NewString(), 1)
	if e != nil {
		t.Fatal(e)
	}
	foreignES := evidence.Scope{TenantID: foreign.TenantID, SiteID: foreign.SiteID}
	_, foreignSource := source(foreignES, "Other tenant")
	foreignSpan, e := ev.CreateSpan(ctx, ea, foreignES, evidence.SpanInput{SourceID: foreignSource.SourceID, Revision: 1, Start: 0, End: 8}, uuid.NewString(), 1)
	if e != nil {
		t.Fatal(e)
	}
	service := New(runtime)
	makeEntity := func(name string) EntityInput {
		return EntityInput{EntityID: uuid.NewString(), Type: "operation", Name: name, SourceID: entitySource.SourceID, SourceRevision: 1}
	}
	left, right := makeEntity("First operation"), makeEntity("Second operation")
	left.EntityID = "00000000-0000-4000-8000-000000000100"
	right.EntityID = "00000000-0000-4000-8000-000000000200"
	firstEntity, e := service.CreateEntity(ctx, owner, sc, left, "entity-left", 0)
	if e != nil || firstEntity.State != "candidate" || !firstEntity.SourcesCurrent {
		t.Fatalf("entity=%+v %v", firstEntity, e)
	}
	if _, e = service.CreateEntity(ctx, owner, sc, right, "entity-right", 0); e != nil {
		t.Fatal(e)
	}
	if _, e = service.CreateEntity(ctx, viewer, sc, makeEntity("Denied"), uuid.NewString(), 0); !errors.Is(e, ErrDenied) {
		t.Fatalf("viewer write=%v", e)
	}
	input := ClaimInput{ClaimID: "00000000-0000-4000-8000-000000000100", SubjectID: left.EntityID, SubjectRevision: 1, Predicate: "precedes", ObjectEntityID: right.EntityID, ObjectRevision: 1, EffectiveFrom: "2026-09-27T00:00:00Z", EvidenceClass: "report", SourceSpanIDs: []string{span.ID}}
	claimed, e := service.CreateClaim(ctx, owner, sc, input, "claim-create", 0)
	if e != nil || claimed.Revision != 1 || claimed.State != "candidate" || !claimed.SourcesCurrent {
		t.Fatalf("claim=%+v %v", claimed, e)
	}
	replayed, e := service.CreateClaim(ctx, owner, sc, input, "claim-create", 0)
	if e != nil || replayed.Revision != 1 {
		t.Fatalf("replay=%+v %v", replayed, e)
	}
	changed := input
	changed.EvidenceClass = "hypothesis"
	if _, e = service.CreateClaim(ctx, owner, sc, changed, "claim-create", 0); !errors.Is(e, ErrConflict) {
		t.Fatalf("conflict=%v", e)
	}
	reverse := input
	reverse.ClaimID = uuid.NewString()
	reverse.SubjectID = right.EntityID
	reverse.ObjectEntityID = left.EntityID
	if _, e = service.CreateClaim(ctx, owner, sc, reverse, uuid.NewString(), 0); !errors.Is(e, ErrCycle) {
		t.Fatalf("candidate cycle=%v", e)
	}
	cross := input
	cross.ClaimID = uuid.NewString()
	cross.SourceSpanIDs = []string{foreignSpan.ID}
	if _, e = service.CreateClaim(ctx, owner, sc, cross, uuid.NewString(), 0); !errors.Is(e, ErrNotFound) {
		t.Fatalf("foreign span=%v", e)
	}
	if _, e = service.ReadClaim(ctx, owner, foreign, input.ClaimID, 1); !errors.Is(e, ErrNotFound) {
		t.Fatalf("foreign read=%v", e)
	}
	_, e = admin.ExecContext(ctx, `INSERT INTO supervisor.knowledge_claim_spans(tenant_id,site_id,claim_id,claim_revision,span_id,source_id,source_revision) VALUES($1,$2,$3,1,$4,$5,1)`, sc.TenantID, sc.SiteID, input.ClaimID, foreignSpan.ID, foreignSource.SourceID)
	var pg *pgconn.PgError
	if !errors.As(e, &pg) || pg.Code != "23503" {
		t.Fatalf("composite provenance FK=%v", e)
	}
	// Exactly one competing change owns revision 2.
	var wg sync.WaitGroup
	outcomes := make(chan error, 2)
	for _, class := range []string{"observation", "hypothesis"} {
		wg.Add(1)
		go func(class string) {
			defer wg.Done()
			revision := input
			revision.EvidenceClass = class
			_, err := service.CreateClaim(ctx, owner, sc, revision, class, 1)
			outcomes <- err
		}(class)
	}
	wg.Wait()
	close(outcomes)
	wins := 0
	for err := range outcomes {
		if err == nil {
			wins++
		} else if !errors.Is(err, ErrRevision) {
			t.Fatal(err)
		}
	}
	if wins != 1 {
		t.Fatalf("CAS winners=%d", wins)
	}
	if read, e := service.ReadClaim(ctx, viewer, sc, input.ClaimID, 1); e != nil || read.Revision != 1 || read.CurrentRevision != 2 || !read.SourcesCurrent {
		t.Fatalf("immutable history=%+v %v", read, e)
	}
	page, e := service.ListClaims(ctx, viewer, sc, 1, "", left.EntityID)
	if e != nil || len(page.Claims) != 1 {
		t.Fatalf("subject page=%+v %v", page, e)
	}
	page, e = service.ListClaims(ctx, viewer, sc, 1, "", right.EntityID)
	if e != nil || len(page.Claims) != 0 {
		t.Fatalf("subject filter=%+v %v", page, e)
	}
	// Audit failure rolls back candidate, receipt and event together.
	failed := makeEntity("Audit rollback")
	constraint := "candidate_audit_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	exec(fmt.Sprintf("ALTER TABLE supervisor.knowledge_audit ADD CONSTRAINT %s CHECK(target_id <> '%s'::uuid) NOT VALID", constraint, failed.EntityID))
	_, failure := service.CreateEntity(ctx, owner, sc, failed, "audit-failure", 0)
	exec("ALTER TABLE supervisor.knowledge_audit DROP CONSTRAINT " + constraint)
	requireSQLState(t, failure, "23514")
	var count int
	if e = admin.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.knowledge_entities WHERE tenant_id=$1 AND id=$2`, sc.TenantID, failed.EntityID).Scan(&count); e != nil || count != 0 {
		t.Fatalf("partial entity=%d %v", count, e)
	}
	// Source revisions remain readable historically, but never authorize new current candidates.
	citationInput.Text = "Updated observed process steps."
	if _, e = ev.CreateOrRevise(ctx, ea, es, citationInput, uuid.NewString(), 1); e != nil {
		t.Fatal(e)
	}
	historical, e := service.ReadClaim(ctx, viewer, sc, input.ClaimID, 1)
	if e != nil || historical.SourcesCurrent {
		t.Fatalf("stale history=%+v %v", historical, e)
	}
	stale := input
	stale.ClaimID = uuid.NewString()
	if _, e = service.CreateClaim(ctx, owner, sc, stale, uuid.NewString(), 0); !errors.Is(e, ErrSourceChanged) {
		t.Fatalf("stale citation accepted=%v", e)
	}
	freshSpan, e := ev.CreateSpan(ctx, ea, es, evidence.SpanInput{SourceID: citationSource.SourceID, Revision: 2, Start: 0, End: 7}, uuid.NewString(), 2)
	if e != nil {
		t.Fatal(e)
	}
	// Stale and then revoked edges cannot keep governing the candidate-cycle check.
	_, reverseSource := source(es, "Reverse candidate citation")
	reverseSpan, e := ev.CreateSpan(ctx, ea, es, evidence.SpanInput{SourceID: reverseSource.SourceID, Revision: 1, Start: 0, End: 8}, uuid.NewString(), 1)
	if e != nil {
		t.Fatal(e)
	}
	reverse.SourceSpanIDs = []string{reverseSpan.ID}
	if _, e = service.CreateClaim(ctx, owner, sc, reverse, "stale-edge-no-authority", 0); e != nil {
		t.Fatalf("stale edge governed graph: %v", e)
	}
	if _, e = ev.Revoke(ctx, ea, es, reverseSource.SourceID, "Withdraw reverse claim citation", uuid.NewString(), 1); e != nil {
		t.Fatal(e)
	}
	fresh := input
	fresh.ClaimID = "00000000-0000-4000-8000-000000000200"
	fresh.SourceSpanIDs = []string{freshSpan.ID}
	if _, e = service.CreateClaim(ctx, owner, sc, fresh, "fresh-citation", 0); e != nil {
		t.Fatal(e)
	}
	entityInput.Text = "Updated entity definitions."
	if _, e = ev.CreateOrRevise(ctx, ea, es, entityInput, uuid.NewString(), 1); e != nil {
		t.Fatal(e)
	}
	staleOrigin := fresh
	staleOrigin.ClaimID = uuid.NewString()
	if _, e = service.CreateClaim(ctx, owner, sc, staleOrigin, uuid.NewString(), 0); !errors.Is(e, ErrSourceChanged) {
		t.Fatalf("stale entity origin laundered=%v", e)
	}
	if _, e = service.CreateEntity(ctx, owner, sc, makeEntity("Stale entity"), uuid.NewString(), 0); !errors.Is(e, ErrSourceChanged) {
		t.Fatalf("stale entity created=%v", e)
	}
	// A committed source withdrawal between page selection and row locking must refill
	// the page, never silently truncate visible candidates beyond the selected window.
	_, healthySource := source(es, "Unrelated pagination survivor")
	healthySpan, e := ev.CreateSpan(ctx, ea, es, evidence.SpanInput{SourceID: healthySource.SourceID, Revision: 1, Start: 0, End: 8}, uuid.NewString(), 1)
	if e != nil {
		t.Fatal(e)
	}
	healthyLeft := EntityInput{EntityID: "00000000-0000-4000-8000-000000000300", Type: "operation", Name: "Surviving third operation", SourceID: healthySource.SourceID, SourceRevision: 1}
	healthyRight := healthyLeft
	healthyRight.EntityID = "00000000-0000-4000-8000-000000000400"
	healthyRight.Name = "Surviving fourth operation"
	for _, in := range []EntityInput{healthyLeft, healthyRight} {
		if _, e = service.CreateEntity(ctx, owner, sc, in, uuid.NewString(), 0); e != nil {
			t.Fatal(e)
		}
	}
	healthyClaim := input
	healthyClaim.ClaimID = "00000000-0000-4000-8000-000000000300"
	healthyClaim.SubjectID = healthyLeft.EntityID
	healthyClaim.ObjectEntityID = healthyRight.EntityID
	healthyClaim.SourceSpanIDs = []string{healthySpan.ID}
	if _, e = service.CreateClaim(ctx, owner, sc, healthyClaim, uuid.NewString(), 0); e != nil {
		t.Fatal(e)
	}
	blocker, e := admin.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer blocker.Rollback()
	if _, e = blocker.ExecContext(ctx, "SELECT set_config('supervisor.tenant_id',$1,true)", sc.TenantID); e != nil {
		t.Fatal(e)
	}
	var blockerID int
	if e = blocker.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&blockerID); e != nil {
		t.Fatal(e)
	}
	if _, e = blocker.ExecContext(ctx, `SELECT 1 FROM supervisor.evidence_heads WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3 FOR UPDATE`, sc.TenantID, sc.SiteID, entitySource.SourceID); e != nil {
		t.Fatal(e)
	}
	type entityResult struct {
		page EntityPage
		err  error
	}
	type claimResult struct {
		page ClaimPage
		err  error
	}
	entityPages, claimPages := make(chan entityResult, 1), make(chan claimResult, 1)
	go func() { p, e := service.ListEntities(ctx, viewer, sc, 1, ""); entityPages <- entityResult{p, e} }()
	go func() { p, e := service.ListClaims(ctx, viewer, sc, 1, "", ""); claimPages <- claimResult{p, e} }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		var blocked int
		if e = admin.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid))`, blockerID).Scan(&blocked); e != nil {
			t.Fatal(e)
		}
		if blocked >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("pagination queries did not reach source lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, e = blocker.ExecContext(ctx, `UPDATE supervisor.evidence_heads SET revoked=true,state_revision=state_revision+1,access_epoch=access_epoch+1 WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3`, sc.TenantID, sc.SiteID, entitySource.SourceID); e != nil {
		t.Fatal(e)
	}
	if _, e = blocker.ExecContext(ctx, `INSERT INTO supervisor.evidence_revocations(tenant_id,site_id,source_id,state_revision,access_epoch,actor_issuer,actor_subject,reason) VALUES($1,$2,$3,3,2,$4,$5,'deterministic pagination revocation fixture')`, sc.TenantID, sc.SiteID, entitySource.SourceID, owner.Issuer, owner.Subject); e != nil {
		t.Fatal(e)
	}
	if e = blocker.Commit(); e != nil {
		t.Fatal(e)
	}
	ep, cp := <-entityPages, <-claimPages
	if ep.err != nil || len(ep.page.Entities) != 1 || ep.page.Entities[0].EntityID != healthyLeft.EntityID || ep.page.NextCursor == "" {
		t.Fatalf("truncated entity page=%+v %v", ep.page, ep.err)
	}
	if cp.err != nil || len(cp.page.Claims) != 1 || cp.page.Claims[0].ClaimID != healthyClaim.ClaimID {
		t.Fatalf("truncated claim page=%+v %v", cp.page, cp.err)
	}
	if _, e = ev.Revoke(ctx, ea, es, healthySource.SourceID, "Withdraw surviving fixture", uuid.NewString(), 1); e != nil {
		t.Fatal(e)
	}
	for _, id := range []string{input.ClaimID, fresh.ClaimID} {
		if _, e = service.ReadClaim(ctx, viewer, sc, id, 0); !errors.Is(e, ErrNotFound) {
			t.Fatalf("revoked origin claim=%v", e)
		}
	}
	if _, e = service.CreateClaim(ctx, owner, sc, input, "claim-create", 0); !errors.Is(e, ErrNotFound) {
		t.Fatalf("revoked origin replay=%v", e)
	}
	if _, e = service.CreateEntity(ctx, owner, sc, left, "entity-left", 0); !errors.Is(e, ErrNotFound) {
		t.Fatalf("revoked entity replay=%v", e)
	}
	if page, e = service.ListClaims(ctx, viewer, sc, 100, "", ""); e != nil || len(page.Claims) != 0 {
		t.Fatalf("revoked claim list=%+v %v", page, e)
	}
	t.Run("entity revisions retain exact claim provenance", func(t *testing.T) {
		_, origin := source(es, "Versioned entity origin")
		_, replacement := source(es, "Replacement entity origin")
		e1 := EntityInput{EntityID: uuid.NewString(), Type: "operation", Name: "Original", SourceID: origin.SourceID, SourceRevision: 1}
		e2 := e1
		e2.EntityID = uuid.NewString()
		e2.SourceID = replacement.SourceID
		for _, in := range []EntityInput{e1, e2} {
			if _, err := service.CreateEntity(ctx, owner, sc, in, in.EntityID, 0); err != nil {
				t.Fatal(err)
			}
		}
		pinSpan, err := ev.CreateSpan(ctx, ea, es, evidence.SpanInput{SourceID: replacement.SourceID, Revision: 1, Start: 0, End: 8}, uuid.NewString(), 1)
		if err != nil {
			t.Fatal(err)
		}
		c := input
		c.ClaimID, c.SubjectID, c.ObjectEntityID, c.SourceSpanIDs = uuid.NewString(), e1.EntityID, e2.EntityID, []string{pinSpan.ID}
		if _, err = service.CreateClaim(ctx, owner, sc, c, "pinned-claim", 0); err != nil {
			t.Fatal(err)
		}
		revised := e1
		revised.Name, revised.SourceID = "Revised", replacement.SourceID
		results := make(chan error, 2)
		for i := range 2 {
			go func(i int) {
				_, err := service.CreateEntity(ctx, owner, sc, revised, fmt.Sprintf("entity-revision-%d", i), 1)
				results <- err
			}(i)
		}
		winners := 0
		for range 2 {
			if err := <-results; err == nil {
				winners++
			} else if !errors.Is(err, ErrRevision) {
				t.Fatal(err)
			}
		}
		if winners != 1 {
			t.Fatalf("entity CAS winners: %d", winners)
		}
		if _, err := service.CreateEntity(ctx, owner, sc, revised, "stale-entity-cas", 1); !errors.Is(err, ErrRevision) {
			t.Fatalf("stale CAS: %v", err)
		}
		if got, err := service.ReadEntity(ctx, viewer, sc, e1.EntityID, 1); err != nil || got.Name != "Original" || got.CurrentRevision != 2 || got.SourcesCurrent {
			t.Fatalf("entity history: %+v %v", got, err)
		}
		if got, err := service.CreateEntity(ctx, owner, sc, e1, e1.EntityID, 0); err != nil || got.Revision != 1 || got.CurrentRevision != 2 || got.SourcesCurrent {
			t.Fatalf("old replay: %+v %v", got, err)
		}
		if got, err := service.ReadClaim(ctx, viewer, sc, c.ClaimID, 1); err != nil || got.SubjectRevision != 1 || got.SourcesCurrent {
			t.Fatalf("claim history: %+v %v", got, err)
		}
		if got, err := service.ListClaims(ctx, viewer, sc, 100, "", e1.EntityID); err != nil || len(got.Claims) != 1 || got.Claims[0].SourcesCurrent {
			t.Fatalf("claim list freshness: %+v %v", got, err)
		}
		stale := c
		stale.ClaimID = uuid.NewString()
		if _, err := service.CreateClaim(ctx, owner, sc, stale, "stale-pin", 0); !errors.Is(err, ErrSourceChanged) {
			t.Fatalf("stale entity pin: %v", err)
		}
		reverse := c
		reverse.ClaimID, reverse.SubjectID, reverse.ObjectEntityID = uuid.NewString(), e2.EntityID, e1.EntityID
		reverse.ObjectRevision = 2
		if _, err := service.CreateClaim(ctx, owner, sc, reverse, "obsolete-edge", 0); err != nil {
			t.Fatalf("obsolete pin still governed cycle: %v", err)
		}
		if _, err := ev.Revoke(ctx, ea, es, origin.SourceID, "Withdraw original", uuid.NewString(), 1); err != nil {
			t.Fatal(err)
		}
		if _, err := service.ReadEntity(ctx, viewer, sc, e1.EntityID, 1); !errors.Is(err, ErrNotFound) {
			t.Fatalf("revoked history: %v", err)
		}
		if got, err := service.ReadEntity(ctx, viewer, sc, e1.EntityID, 0); err != nil || got.Revision != 2 || !got.SourcesCurrent {
			t.Fatalf("current entity concealed: %+v %v", got, err)
		}
		if _, err := service.ReadClaim(ctx, viewer, sc, c.ClaimID, 1); !errors.Is(err, ErrNotFound) {
			t.Fatalf("revoked claim origin: %v", err)
		}
		if got, err := service.ListClaims(ctx, viewer, sc, 100, "", ""); err != nil || len(got.Claims) != 1 || got.Claims[0].ClaimID != reverse.ClaimID {
			t.Fatalf("current pinned claim concealed: %+v %v", got, err)
		}
		if got, err := service.ListEntities(ctx, viewer, sc, 100, ""); err != nil || len(got.Entities) != 2 {
			t.Fatalf("current entity list concealed: %+v %v", got, err)
		}
		if _, err := ev.Revoke(ctx, ea, es, replacement.SourceID, "Withdraw replacement fixture", uuid.NewString(), 1); err != nil {
			t.Fatal(err)
		}
	})
	if entities, e := service.ListEntities(ctx, viewer, sc, 100, ""); e != nil || len(entities.Entities) != 0 {
		t.Fatalf("revoked entity list=%+v %v", entities, e)
	}
	exec(`UPDATE supervisor.memberships SET active=false WHERE tenant_id=$1 AND site_id=$2 AND issuer=$3 AND subject=$4`, sc.TenantID, sc.SiteID, viewer.Issuer, viewer.Subject)
	if _, e = service.ReadClaim(ctx, viewer, sc, input.ClaimID, 1); !errors.Is(e, ErrNotFound) {
		t.Fatalf("removed member read=%v", e)
	}
}
