package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/evidence"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/knowledge"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/migrate"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// blockedError marks a missing prerequisite (rule 09: BLOCKED, never a green fallback).
type blockedError struct{ error }

type Fixture struct {
	SeedRole              string  `json:"seed_role"`
	Entities              int     `json:"site_entities"`
	Claims                int     `json:"site_claims"`
	SiblingSiteEntities   int     `json:"sibling_site_entities"`
	SiblingSiteClaims     int     `json:"sibling_site_claims"`
	ForeignTenantEntities int     `json:"foreign_tenant_entities"`
	ForeignTenantClaims   int     `json:"foreign_tenant_claims"`
	SnapshotEntities      int     `json:"snapshot_entities"`
	SnapshotClaims        int     `json:"snapshot_claims"`
	SnapshotBytes         int     `json:"snapshot_canonical_bytes"`
	RevokedReviews        int     `json:"revoked_reviews_in_site"`
	RevokedSources        int     `json:"revoked_sources_in_site"`
	SnapshotEntitiesPct   float64 `json:"snapshot_entities_pct_of_200_cap"`
	SnapshotClaimsPct     float64 `json:"snapshot_claims_pct_of_500_cap"`
	SnapshotBytesPct      float64 `json:"snapshot_bytes_pct_of_cap"`
}
type Probe struct {
	Name         string `json:"name"`
	Passed       bool   `json:"passed"`
	Outcome      string `json:"outcome"`
	Unauthorized bool   `json:"unauthorized"`
}
type Timings struct {
	Migrate  float64 `json:"migrate"`
	Services float64 `json:"service_fixture"`
	Bulk     float64 `json:"bulk_candidates"`
	Snapshot float64 `json:"site_snapshot_create"`
}
type outcome struct {
	PostgresVersion string
	Measured        string
	Fixture         Fixture
	Probes          []Probe
	Timings         Timings
	Runs            []Run
	Notes           []string
}

// Run is one measured pass over the same frozen query list.
type Run struct {
	Name               string  `json:"name"`
	Concurrency        int     `json:"concurrency"`
	Latency            Latency `json:"latency"`
	Unauthorized       int     `json:"unauthorized"`
	Errors             int     `json:"errors"`
	ReturnedClaims     int     `json:"returned_claims_total"`
	ReturnedEntities   int     `json:"returned_entities_total"`
	TruncatedResponses int     `json:"truncated_responses"`
	Seconds            float64 `json:"wall_seconds"`
}

const measured = "knowledge.Service.ReadGraph, the service behind GET /v1/sites/{site}/process-snapshots/{snapshot}/graph (HTTP/cookie layer excluded), called sequentially as the restricted runtime role: one site-serialized transaction with tenant context, session and membership checks, site row lock, persisted snapshot load, source eligibility locks, inventory staleness check, in-memory 2-hop traversal, audit insert and commit. Traversal operates on a persisted approved snapshot capped at 200 entities/500 claims/64KiB, grown here until CreateSnapshot refused the next unit; the 10k/100k candidate rows are present in the site but are not read by this path."

var (
	validFrom = "2026-01-01T00:00:00Z"
	asOf      = "2026-06-01T00:00:00Z"
	// Multi-valued, non-routing predicates: no conflict or cycle records exist for them,
	// so direct candidate inserts store exactly what CreateClaim would.
	edges = []struct{ predicate, subject, object string }{
		{"performs", "asset", "operation"}, {"consumes", "operation", "material"}, {"produces", "operation", "material"},
		{"requires_tool", "operation", "tool"}, {"responsible_role", "operation", "role"}, {"follows_document", "operation", "document"},
		{"has_constraint", "asset", "constraint"}, {"has_constraint", "operation", "constraint"},
	}
	queryPredicates = []string{"consumes", "follows_document", "has_constraint", "performs", "produces", "requires_tool", "responsible_role"}
	bulkTypes       = []struct {
		kind   string
		weight int
	}{{"asset", 15}, {"operation", 35}, {"material", 15}, {"tool", 10}, {"role", 5}, {"document", 10}, {"constraint", 10}}
)

type scopeFixture struct {
	scope       knowledge.Scope
	core        map[string][]string // entity type -> approved-graph entity IDs
	coreClaims  map[string]bool
	reviews     map[string]string // claim ID -> review ID
	coreSource  string
	revokedRoot string // an entity only reachable through revoked claims
	bulkRoot    string // a candidate-only entity
	snapshot    string
}

type harness struct {
	ctx             context.Context
	admin, runtime  *sql.DB
	know            *knowledge.Service
	ev              *evidence.Service
	rng             *rand.Rand
	owner, reviewer knowledge.Actor
	viewer          knowledge.Actor
	logf            func(string, ...any)
	ownerEv         evidence.Actor
}

func (h *harness) id() string {
	u, err := uuid.NewRandomFromReader(h.rng)
	if err != nil {
		panic(err)
	}
	return u.String()
}
func (h *harness) key() string { return h.id() }

func (h *harness) exec(q string, args ...any) error {
	_, err := h.admin.ExecContext(h.ctx, q, args...)
	return err
}

// refresh keeps fixture sessions live and recently authenticated across a long seed.
func (h *harness) refresh() error {
	return h.exec(`UPDATE supervisor.browser_sessions SET last_seen_at=now(),auth_time=now() WHERE issuer='https://load.invalid'`)
}

func qualify(ctx context.Context, m Manifest, adminURL, runtimeURL string, logw io.Writer) (outcome, error) {
	out := outcome{Measured: measured, Notes: []string{"ReadGraph locks the site row FOR NO KEY UPDATE, so reads of one site serialize; the concurrent run's latency includes queueing on that lock."}}
	logf := func(format string, a ...any) { fmt.Fprintf(logw, "supervisor-load: "+format+"\n", a...) }
	control, err := sql.Open("pgx", adminURL)
	if err != nil {
		return out, blockedError{fmt.Errorf("admin database: %w", err)}
	}
	defer control.Close()
	if err = control.PingContext(ctx); err != nil {
		return out, blockedError{errors.New("admin database unreachable")}
	}
	// Disposable database: the shared schema is never written, and the drop is scoped to this name.
	database := "knowledge_load_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = control.ExecContext(ctx, `CREATE DATABASE `+database); err != nil {
		return out, blockedError{fmt.Errorf("create disposable database: %w", err)}
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		if _, e := control.ExecContext(cleanup, `DROP DATABASE IF EXISTS `+database+` WITH (FORCE)`); e != nil {
			logf("drop %s: %v", database, e)
		}
	}()
	connect := func(raw string) (*sql.DB, error) {
		u, e := url.Parse(raw)
		if e != nil {
			return nil, e
		}
		u.Path = "/" + database
		return sql.Open("pgx", u.String())
	}
	admin, err := connect(adminURL)
	if err != nil {
		return out, blockedError{err}
	}
	defer admin.Close()
	runtimeDB, err := connect(runtimeURL)
	if err != nil {
		return out, blockedError{err}
	}
	defer runtimeDB.Close()
	runtimeDB.SetMaxOpenConns(m.Concurrency + 2)
	started := time.Now()
	if err = migrate.Apply(ctx, admin); err != nil {
		return out, blockedError{fmt.Errorf("migrate disposable database: %w", err)}
	}
	if err = runtimeDB.PingContext(ctx); err != nil {
		return out, blockedError{errors.New("runtime role cannot connect to the disposable database")}
	}
	out.Timings.Migrate = time.Since(started).Seconds()
	if err = admin.QueryRowContext(ctx, `SELECT current_setting('server_version')`).Scan(&out.PostgresVersion); err != nil {
		return out, err
	}

	h := &harness{ctx: ctx, admin: admin, runtime: runtimeDB, know: knowledge.New(runtimeDB), ev: evidence.New(runtimeDB), rng: rand.New(rand.NewSource(m.Seed)), logf: logf}
	scopes := []knowledge.Scope{{TenantID: h.id(), SiteID: h.id()}}
	scopes = append(scopes, knowledge.Scope{TenantID: scopes[0].TenantID, SiteID: h.id()}, knowledge.Scope{TenantID: h.id(), SiteID: h.id()})
	for i, sc := range scopes {
		if i != 1 {
			if err = h.exec(`INSERT INTO supervisor.tenants(id,name) VALUES($1,'load synthetic')`, sc.TenantID); err != nil {
				return out, err
			}
		}
		if err = h.exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'load synthetic')`, sc.TenantID, sc.SiteID); err != nil {
			return out, err
		}
	}
	actor := func(role string, member []knowledge.Scope) (knowledge.Actor, error) {
		a := knowledge.Actor{Issuer: "https://load.invalid", Subject: h.id(), SessionHash: strings.ReplaceAll(h.id(), "-", "") + strings.ReplaceAll(h.id(), "-", "")}
		if e := h.exec(`INSERT INTO supervisor.browser_sessions(token_hash,issuer,subject,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,'fixture',now(),now(),now()+interval '4 hours',now())`, a.SessionHash, a.Issuer, a.Subject); e != nil {
			return a, e
		}
		for _, sc := range member {
			if e := h.exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,$5)`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, role); e != nil {
				return a, e
			}
		}
		return a, nil
	}
	if h.owner, err = actor("owner", scopes); err != nil {
		return out, err
	}
	if h.reviewer, err = actor("supervisor", scopes); err != nil {
		return out, err
	}
	if h.viewer, err = actor("viewer", scopes[:1]); err != nil {
		return out, err
	}
	h.ownerEv = evidence.Actor(h.owner)

	f := m.Fixture
	sizes := []struct{ entities, claims int }{{f.Entities, f.Claims}, {f.SiblingSiteEntities, f.SiblingSiteClaims}, {f.ForeignTenantEntities, f.ForeignTenantClaims}}
	fixtures := make([]*scopeFixture, len(scopes))
	for i, sc := range scopes {
		logf("seeding scope %d/%d (%d entities, %d claims)", i+1, len(scopes), sizes[i].entities, sizes[i].claims)
		if fixtures[i], err = h.seedScope(sc, sizes[i].entities, sizes[i].claims, &out.Timings); err != nil {
			return out, fmt.Errorf("seed scope %d: %w", i+1, err)
		}
	}
	site, sibling, foreign := fixtures[0], fixtures[1], fixtures[2]
	count := func(sc knowledge.Scope, table string) (n int) {
		_ = admin.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.`+table+` WHERE tenant_id=$1 AND site_id=$2`, sc.TenantID, sc.SiteID).Scan(&n)
		return n
	}
	out.Fixture.SeedRole = "core approved graph, authority, reviews, revocations and snapshots through the Go evidence/knowledge services; bulk candidates by direct multi-row INSERT as the restricted runtime role (non-owner, FORCE RLS, tenant context set, all constraints and deferred guards enabled; payloads checked by the knowledge contract validators; no receipt/audit rows for bulk rows)"
	out.Fixture.Entities, out.Fixture.Claims = count(site.scope, "knowledge_entities"), count(site.scope, "knowledge_claim_heads")
	out.Fixture.SiblingSiteEntities, out.Fixture.SiblingSiteClaims = count(sibling.scope, "knowledge_entities"), count(sibling.scope, "knowledge_claim_heads")
	out.Fixture.ForeignTenantEntities, out.Fixture.ForeignTenantClaims = count(foreign.scope, "knowledge_entities"), count(foreign.scope, "knowledge_claim_heads")
	// Cap-probe snapshots share the site; count only the measured one.
	if err = admin.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM supervisor.knowledge_snapshot_entities WHERE tenant_id=$1 AND site_id=$2 AND snapshot_id=$3),(SELECT count(*) FROM supervisor.knowledge_snapshot_claims WHERE tenant_id=$1 AND site_id=$2 AND snapshot_id=$3)`, site.scope.TenantID, site.scope.SiteID, site.snapshot).Scan(&out.Fixture.SnapshotEntities, &out.Fixture.SnapshotClaims); err != nil {
		return out, err
	}
	out.Fixture.RevokedReviews, out.Fixture.RevokedSources = count(site.scope, "knowledge_review_revocations"), count(site.scope, "evidence_revocations")
	if err = admin.QueryRowContext(ctx, `SELECT length(canonical_contents) FROM supervisor.knowledge_process_snapshots WHERE tenant_id=$1 AND site_id=$2 AND snapshot_id=$3`, site.scope.TenantID, site.scope.SiteID, site.snapshot).Scan(&out.Fixture.SnapshotBytes); err != nil {
		return out, err
	}
	if out.Fixture.Entities != f.Entities || out.Fixture.Claims != f.Claims {
		return out, fmt.Errorf("site fixture has %d entities/%d claims, manifest requires %d/%d", out.Fixture.Entities, out.Fixture.Claims, f.Entities, f.Claims)
	}

	// Independent oracle (admin role, not the product eligibility code): a returned item is
	// authorized only if it belongs to this snapshot in this tenant/site and its review and
	// cited sources are unrevoked.
	authorizedClaims, authorizedEntities := map[string]bool{}, map[string]bool{}
	if err = collect(ctx, admin, authorizedClaims, `SELECT sc.claim_id::text FROM supervisor.knowledge_snapshot_claims sc WHERE sc.tenant_id=$1 AND sc.site_id=$2 AND sc.snapshot_id=$3
 AND NOT EXISTS(SELECT 1 FROM supervisor.knowledge_review_revocations rr WHERE (rr.tenant_id,rr.site_id,rr.review_id)=(sc.tenant_id,sc.site_id,sc.review_id))
 AND NOT EXISTS(SELECT 1 FROM supervisor.knowledge_claim_spans ks JOIN supervisor.evidence_revocations er ON (er.tenant_id,er.site_id,er.source_id)=(ks.tenant_id,ks.site_id,ks.source_id)
  WHERE (ks.tenant_id,ks.site_id,ks.claim_id,ks.claim_revision)=(sc.tenant_id,sc.site_id,sc.claim_id,sc.claim_revision))`, site.scope.TenantID, site.scope.SiteID, site.snapshot); err != nil {
		return out, err
	}
	if err = collect(ctx, admin, authorizedEntities, `SELECT se.entity_id::text FROM supervisor.knowledge_snapshot_entities se JOIN supervisor.knowledge_entity_revisions er ON (er.tenant_id,er.site_id,er.id,er.revision)=(se.tenant_id,se.site_id,se.entity_id,se.entity_revision)
 WHERE se.tenant_id=$1 AND se.site_id=$2 AND se.snapshot_id=$3 AND NOT EXISTS(SELECT 1 FROM supervisor.evidence_revocations r WHERE (r.tenant_id,r.site_id,r.source_id)=(er.tenant_id,er.site_id,er.source_id))`, site.scope.TenantID, site.scope.SiteID, site.snapshot); err != nil {
		return out, err
	}
	for id := range authorizedClaims {
		// The generator's own record must agree: only claims it approved and kept eligible.
		if !site.coreClaims[id] {
			return out, fmt.Errorf("oracle disagreement: snapshot claim %s was not approved by the fixture", id)
		}
	}

	out.Fixture.SnapshotEntitiesPct = pct(out.Fixture.SnapshotEntities, 200)
	out.Fixture.SnapshotClaimsPct = pct(out.Fixture.SnapshotClaims, 500)
	out.Fixture.SnapshotBytesPct = pct(out.Fixture.SnapshotBytes, knowledge.MaxSnapshotBytes)
	var roots []string
	for _, ids := range [][]string{site.core["asset"], site.core["operation"], site.core["material"], site.core["tool"], site.core["role"], site.core["document"]} {
		roots = append(roots, ids...)
	}
	directions := []string{"outgoing", "incoming", "both"}
	queries := make([]knowledge.GraphQuery, m.Queries)
	for i := range queries {
		n := 1 + h.rng.Intn(3)
		q := knowledge.GraphQuery{Direction: directions[h.rng.Intn(3)], Predicates: queryPredicates, AsOf: asOf, Hops: m.Hops, MaxEntities: 200, MaxClaims: 500}
		for range n {
			q.Roots = append(q.Roots, roots[h.rng.Intn(len(roots))])
		}
		queries[i] = q
	}
	measure := func(name string, clients int) (Run, error) {
		run := Run{Name: name, Concurrency: clients}
		if err := h.refresh(); err != nil {
			return run, err
		}
		logf("measuring %d ReadGraph queries with %d client(s)", len(queries), clients)
		samples := make([]time.Duration, len(queries))
		var mu sync.Mutex
		next := make(chan int)
		var wg sync.WaitGroup
		started := time.Now()
		for range clients {
			wg.Go(func() {
				for i := range next {
					begin := time.Now()
					raw, e := h.know.ReadGraph(ctx, h.viewer, site.scope, site.snapshot, queries[i])
					samples[i] = time.Since(begin)
					var result struct {
						Truncated bool `json:"truncated"`
						Entities  []struct {
							EntityID string `json:"entity_id"`
						} `json:"entities"`
						Claims []struct {
							ClaimID string `json:"claim_id"`
						} `json:"claims"`
					}
					if e == nil {
						e = json.Unmarshal(raw, &result)
					}
					mu.Lock()
					if e != nil {
						run.Errors++
						if run.Errors <= 3 {
							out.Notes = append(out.Notes, name+" query error: "+e.Error())
						}
					}
					if result.Truncated {
						run.TruncatedResponses++
					}
					run.ReturnedClaims += len(result.Claims)
					run.ReturnedEntities += len(result.Entities)
					for _, c := range result.Claims {
						if !authorizedClaims[c.ClaimID] || !site.coreClaims[c.ClaimID] {
							run.Unauthorized++
						}
					}
					for _, en := range result.Entities {
						if !authorizedEntities[en.EntityID] {
							run.Unauthorized++
						}
					}
					mu.Unlock()
				}
			})
		}
		for i := range queries {
			next <- i
		}
		close(next)
		wg.Wait()
		run.Seconds = time.Since(started).Seconds()
		run.Latency = summarize(samples)
		if run.ReturnedClaims == 0 {
			out.Notes = append(out.Notes, name+": no claims were returned; the traversal did not exercise any edge")
			run.Errors++
		}
		return run, nil
	}
	for _, spec := range []struct {
		name    string
		clients int
	}{{"sequential", 1}, {"concurrent", m.Concurrency}} {
		run, e := measure(spec.name, spec.clients)
		if e != nil {
			return out, e
		}
		out.Runs = append(out.Runs, run)
	}

	// Negative probes: any returned bytes are an unauthorized disclosure.
	if err = h.refresh(); err != nil {
		return out, err
	}
	base := queries[0]
	rooted := func(root string) knowledge.GraphQuery { q := base; q.Roots = []string{root}; return q }
	probe := func(name string, want error, read func() ([]byte, error)) {
		raw, e := read()
		p := Probe{Name: name, Unauthorized: e == nil || len(raw) > 0}
		p.Passed = !p.Unauthorized && (want == nil || errors.Is(e, want))
		p.Outcome = "returned data"
		if e != nil {
			p.Outcome = e.Error()
		}
		out.Probes = append(out.Probes, p)
	}
	read := func(a knowledge.Actor, sc knowledge.Scope, snapshot string, q knowledge.GraphQuery) func() ([]byte, error) {
		return func() ([]byte, error) { return h.know.ReadGraph(ctx, a, sc, snapshot, q) }
	}
	siblingQ, foreignQ := rooted(sibling.core["asset"][0]), rooted(foreign.core["asset"][0])
	probe("non-member reads sibling-site snapshot", knowledge.ErrNotFound, read(h.viewer, sibling.scope, sibling.snapshot, siblingQ))
	probe("sibling-site snapshot ID through site scope", knowledge.ErrNotFound, read(h.owner, site.scope, sibling.snapshot, siblingQ))
	probe("foreign-tenant snapshot ID through site scope", knowledge.ErrNotFound, read(h.owner, site.scope, foreign.snapshot, foreignQ))
	probe("foreign tenant ID with this site ID", knowledge.ErrNotFound, read(h.owner, knowledge.Scope{TenantID: foreign.scope.TenantID, SiteID: site.scope.SiteID}, foreign.snapshot, foreignQ))
	probe("sibling-site entity as root on site snapshot", knowledge.ErrNotFound, read(h.viewer, site.scope, site.snapshot, siblingQ))
	probe("entity reachable only through revoked claims as root", knowledge.ErrNotFound, read(h.viewer, site.scope, site.snapshot, rooted(site.revokedRoot)))
	probe("candidate-only entity as root", knowledge.ErrNotFound, read(h.viewer, site.scope, site.snapshot, rooted(site.bulkRoot)))
	var state int64
	if err = admin.QueryRowContext(ctx, `SELECT state_revision FROM supervisor.evidence_heads WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3`, sibling.scope.TenantID, sibling.scope.SiteID, sibling.coreSource).Scan(&state); err != nil {
		return out, err
	}
	if _, err = h.ev.Revoke(ctx, h.ownerEv, evidence.Scope(sibling.scope), sibling.coreSource, "Load qualification revocation probe", h.key(), state); err != nil {
		return out, fmt.Errorf("revoke sibling source: %w", err)
	}
	probe("snapshot read after its backing source is revoked", nil, read(h.owner, sibling.scope, sibling.snapshot, siblingQ))
	var claimID string
	for id := range authorizedClaims {
		claimID = id
		break
	}
	if _, err = h.know.RevokeReview(ctx, h.reviewer, site.scope, claimID, knowledge.RevocationInput{ClaimRevision: 1, ReviewID: site.reviews[claimID], Reason: "Load qualification revocation probe"}, h.key(), 1); err != nil {
		return out, fmt.Errorf("revoke site review: %w", err)
	}
	probe("site snapshot read after an included approval is revoked", nil, read(h.viewer, site.scope, site.snapshot, queries[0]))
	return out, nil
}

func pct(n, limit int) float64 { return math.Round(float64(n)*10000/float64(limit)) / 100 }

func collect(ctx context.Context, db *sql.DB, into map[string]bool, q string, args ...any) error {
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return err
		}
		into[id] = true
	}
	return rows.Err()
}

// seedScope builds one tenant/site: an approved core graph through the services, two
// approvals made ineligible (review revoked; cited source revoked), bulk candidates up
// to the requested totals, and finally the snapshot (no site writes may follow it).
func (h *harness) seedScope(sc knowledge.Scope, totalEntities, totalClaims int, t *Timings) (*scopeFixture, error) {
	ctx := h.ctx
	fx := &scopeFixture{scope: sc, core: map[string][]string{}, coreClaims: map[string]bool{}, reviews: map[string]string{}}
	es := evidence.Scope(sc)
	if err := h.refresh(); err != nil {
		return nil, err
	}
	started := time.Now()
	source := func(designate bool) (string, string, error) {
		in := evidence.Input{SourceID: h.id(), Kind: "manual", Title: "Load fixture source", Text: "Synthetic load qualification evidence"}
		if _, e := h.ev.CreateOrRevise(ctx, h.ownerEv, es, in, h.key(), 0); e != nil {
			return "", "", e
		}
		span, e := h.ev.CreateSpan(ctx, h.ownerEv, es, evidence.SpanInput{SourceID: in.SourceID, Revision: 1, Start: 0, End: 9}, h.key(), 1)
		if e != nil {
			return "", "", e
		}
		if designate {
			var state int64
			if e = h.admin.QueryRowContext(ctx, `SELECT state_revision FROM supervisor.evidence_heads WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3`, sc.TenantID, sc.SiteID, in.SourceID).Scan(&state); e != nil {
				return "", "", e
			}
			if _, e = h.ev.DecideAuthority(ctx, h.ownerEv, es, in.SourceID, evidence.AuthorityInput{SourceRevision: 1, ExpectedSourceStateRevision: state, Verdict: "designate", Reason: "Load fixture authority"}, h.key(), 0); e != nil {
				return "", "", e
			}
		}
		return in.SourceID, span.ID, nil
	}
	coreSource, coreSpan, err := source(true)
	if err != nil {
		return nil, fmt.Errorf("core source: %w", err)
	}
	fx.coreSource = coreSource
	entity := func(kind string, n int) (string, error) {
		in := knowledge.EntityInput{EntityID: h.id(), Type: kind, Name: fmt.Sprintf("Load %s %d", kind, n), SourceID: coreSource, SourceRevision: 1}
		if kind == "asset" {
			in.AssetKind = "machine"
		}
		_, e := h.know.CreateEntity(ctx, h.owner, sc, in, h.key(), 0)
		return in.EntityID, e
	}
	// Shared pools give 2-hop fan-out: one tool or role reaches many operations and assets.
	for _, pool := range []struct {
		kind string
		n    int
	}{{"material", 6}, {"tool", 4}, {"role", 3}, {"document", 3}} {
		for n := range pool.n {
			id, e := entity(pool.kind, n)
			if e != nil {
				return nil, fmt.Errorf("core entity: %w", e)
			}
			fx.core[pool.kind] = append(fx.core[pool.kind], id)
		}
	}
	claim := func(predicate, subject, object, span string) (string, error) {
		in := knowledge.ClaimInput{ClaimID: h.id(), SubjectID: subject, SubjectRevision: 1, Predicate: predicate, ObjectEntityID: object, ObjectRevision: 1, EffectiveFrom: validFrom, EvidenceClass: "report", SourceSpanIDs: []string{span}}
		if _, e := h.know.CreateClaim(ctx, h.owner, sc, in, h.key(), 0); e != nil {
			return "", e
		}
		return in.ClaimID, nil
	}
	approve := func(id string) error {
		review, e := h.know.DecideReview(ctx, h.reviewer, sc, id, knowledge.ReviewInput{ClaimRevision: 1, Verdict: "approve", Reason: "Load fixture approval"}, h.key(), 1)
		fx.reviews[id] = review.ReviewID
		return e
	}
	// Grow the approved graph one unit at a time until the snapshot validator refuses it
	// (200 entities / 500 claims / knowledge.MaxSnapshotBytes canonical body), then withdraw the unit that did
	// not fit. The measured snapshot is therefore as large as CreateSnapshot permits.
	pick := func(kind string) string { return fx.core[kind][h.rng.Intn(len(fx.core[kind]))] }
	for unit := 0; ; unit++ {
		if unit > 200 {
			return nil, errors.New("approved graph never reached a snapshot cap")
		}
		if err = h.refresh(); err != nil {
			return nil, err
		}
		added := map[string][]string{}
		asset, e := entity("asset", unit)
		if e != nil {
			return nil, e
		}
		added["asset"] = append(added["asset"], asset)
		var group [][3]string
		for j := range 2 {
			op, e := entity("operation", 2*unit+j)
			if e != nil {
				return nil, e
			}
			limit, e := entity("constraint", 2*unit+j)
			if e != nil {
				return nil, e
			}
			added["operation"], added["constraint"] = append(added["operation"], op), append(added["constraint"], limit)
			group = append(group, [3]string{"performs", asset, op}, [3]string{"consumes", op, pick("material")}, [3]string{"requires_tool", op, pick("tool")},
				[3]string{"responsible_role", op, pick("role")}, [3]string{"follows_document", op, pick("document")}, [3]string{"has_constraint", op, limit})
		}
		ids := make([]string, 0, len(group))
		for _, g := range group {
			id, e := claim(g[0], g[1], g[2], coreSpan)
			if e != nil {
				return nil, fmt.Errorf("core claim: %w", e)
			}
			if e = approve(id); e != nil {
				return nil, fmt.Errorf("approve core claim: %w", e)
			}
			ids = append(ids, id)
		}
		inventory, e := h.know.ReadInventory(ctx, h.owner, sc)
		if e != nil {
			return nil, e
		}
		_, e = h.know.CreateSnapshot(ctx, h.owner, sc, knowledge.SnapshotInput{SnapshotID: h.id(), Reason: "Load qualification cap probe"}, h.key(), inventory.Revision)
		if errors.Is(e, knowledge.ErrTraversalLimit) {
			for _, id := range ids {
				if _, e = h.know.RevokeReview(ctx, h.reviewer, sc, id, knowledge.RevocationInput{ClaimRevision: 1, ReviewID: fx.reviews[id], Reason: "Exceeds snapshot cap"}, h.key(), 1); e != nil {
					return nil, fmt.Errorf("withdraw oversize unit: %w", e)
				}
			}
			break
		}
		if e != nil {
			return nil, fmt.Errorf("cap probe snapshot: %w", e)
		}
		for kind, list := range added {
			fx.core[kind] = append(fx.core[kind], list...)
		}
		for _, id := range ids {
			fx.coreClaims[id] = true
		}
	}
	// Approved, then withdrawn: neither may appear in the snapshot or any read.
	withdrawn, err := entity("operation", 900)
	if err != nil {
		return nil, err
	}
	fx.revokedRoot = withdrawn
	reviewRevoked, err := claim("performs", fx.core["asset"][0], withdrawn, coreSpan)
	if err != nil {
		return nil, err
	}
	if err = approve(reviewRevoked); err != nil {
		return nil, err
	}
	if _, err = h.know.RevokeReview(ctx, h.reviewer, sc, reviewRevoked, knowledge.RevocationInput{ClaimRevision: 1, ReviewID: fx.reviews[reviewRevoked], Reason: "Load fixture withdrawal"}, h.key(), 1); err != nil {
		return nil, fmt.Errorf("revoke review: %w", err)
	}
	revokedSource, revokedSpan, err := source(true)
	if err != nil {
		return nil, err
	}
	sourceRevoked, err := claim("performs", fx.core["asset"][1], withdrawn, revokedSpan)
	if err != nil {
		return nil, err
	}
	if err = approve(sourceRevoked); err != nil {
		return nil, err
	}
	var state int64
	if err = h.admin.QueryRowContext(ctx, `SELECT state_revision FROM supervisor.evidence_heads WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3`, sc.TenantID, sc.SiteID, revokedSource).Scan(&state); err != nil {
		return nil, err
	}
	if _, err = h.ev.Revoke(ctx, h.ownerEv, es, revokedSource, "Load fixture source revocation", h.key(), state); err != nil {
		return nil, fmt.Errorf("revoke source: %w", err)
	}
	spans := []string{coreSpan}
	sources := []string{coreSource}
	for range 8 {
		src, span, e := source(false)
		if e != nil {
			return nil, e
		}
		spans, sources = append(spans, span), append(sources, src)
	}
	t.Services += time.Since(started).Seconds()

	started = time.Now()
	var haveEntities, haveClaims int
	if err = h.admin.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM supervisor.knowledge_entities WHERE tenant_id=$1 AND site_id=$2),(SELECT count(*) FROM supervisor.knowledge_claim_heads WHERE tenant_id=$1 AND site_id=$2)`, sc.TenantID, sc.SiteID).Scan(&haveEntities, &haveClaims); err != nil {
		return nil, err
	}
	if fx.bulkRoot, err = h.bulk(sc, fx, totalEntities-haveEntities, totalClaims-haveClaims, spans, sources); err != nil {
		return nil, fmt.Errorf("bulk candidates: %w", err)
	}
	t.Bulk += time.Since(started).Seconds()

	if err = h.refresh(); err != nil {
		return nil, err
	}
	started = time.Now()
	inventory, err := h.know.ReadInventory(ctx, h.owner, sc)
	if err != nil {
		return nil, err
	}
	snapshot, err := h.know.CreateSnapshot(ctx, h.owner, sc, knowledge.SnapshotInput{SnapshotID: h.id(), Reason: "Load qualification snapshot"}, h.key(), inventory.Revision)
	if err != nil {
		return nil, fmt.Errorf("create snapshot: %w", err)
	}
	if t.Snapshot == 0 {
		t.Snapshot = time.Since(started).Seconds()
	}
	fx.snapshot = snapshot.SnapshotID
	return fx, nil
}

// bulk inserts candidate entities and claims exactly as CreateEntity/CreateClaim store
// them, as the restricted runtime role in tenant-scoped transactions.
func (h *harness) bulk(sc knowledge.Scope, fx *scopeFixture, entities, claims int, spans, sources []string) (string, error) {
	if entities < 1 || claims < 1 {
		return "", errors.New("fixture totals smaller than the approved core")
	}
	weights := 0
	for _, t := range bulkTypes {
		weights += t.weight
	}
	pool := map[string][]string{}
	var ids, kinds, payloads, entitySources []string
	flush := func(q string, args ...any) error {
		tx, e := h.runtime.BeginTx(h.ctx, nil)
		if e != nil {
			return e
		}
		defer tx.Rollback()
		if _, e = tx.ExecContext(h.ctx, `SELECT set_config('supervisor.tenant_id',$1,true),set_config('statement_timeout','300s',true)`, sc.TenantID); e != nil {
			return e
		}
		for _, stmt := range strings.Split(q, ";\n") {
			// Actor parameters come last; statements that omit them bind only the leading ones.
			used := args
			for len(used) > 0 && !strings.Contains(stmt, fmt.Sprintf("$%d", len(used))) {
				used = used[:len(used)-1]
			}
			if _, e = tx.ExecContext(h.ctx, stmt, used...); e != nil {
				return e
			}
		}
		return tx.Commit()
	}
	const entitySQL = `INSERT INTO supervisor.knowledge_entities(tenant_id,site_id,id,entity_type,payload,source_id,source_revision,actor_issuer,actor_subject) SELECT $1,$2,x.id::uuid,x.t,x.p::jsonb,x.s::uuid,1,$7,$8 FROM unnest($3::text[],$4::text[],$5::text[],$6::text[]) x(id,t,p,s);
INSERT INTO supervisor.knowledge_entity_heads(tenant_id,site_id,id,current_revision) SELECT $1,$2,x.id::uuid,1 FROM unnest($3::text[],$4::text[],$5::text[],$6::text[]) x(id,t,p,s);
INSERT INTO supervisor.knowledge_entity_revisions(tenant_id,site_id,id,revision,entity_type,payload,source_id,source_revision,actor_issuer,actor_subject) SELECT $1,$2,x.id::uuid,1,x.t,x.p::jsonb,x.s::uuid,1,$7,$8 FROM unnest($3::text[],$4::text[],$5::text[],$6::text[]) x(id,t,p,s)`
	const batch = 5000
	for n := range entities {
		pick, kind := h.rng.Intn(weights), ""
		for _, t := range bulkTypes {
			if pick < t.weight {
				kind = t.kind
				break
			}
			pick -= t.weight
		}
		in := knowledge.EntityInput{EntityID: h.id(), Type: kind, Name: fmt.Sprintf("Bulk %s %d", kind, n), SourceID: sources[n%len(sources)], SourceRevision: 1}
		if kind == "asset" {
			in.AssetKind = "machine"
		}
		if err := in.Validate(); err != nil {
			return "", err
		}
		payload, _ := json.Marshal(in)
		pool[kind] = append(pool[kind], in.EntityID)
		ids, kinds, payloads, entitySources = append(ids, in.EntityID), append(kinds, kind), append(payloads, string(payload)), append(entitySources, in.SourceID)
		if len(ids) == batch || n == entities-1 {
			if err := flush(entitySQL, sc.TenantID, sc.SiteID, ids, kinds, payloads, entitySources, h.owner.Issuer, h.owner.Subject); err != nil {
				return "", err
			}
			ids, kinds, payloads, entitySources = nil, nil, nil, nil
		}
	}
	for _, kind := range []string{"asset", "operation", "material", "tool", "role", "document", "constraint"} {
		if len(pool[kind]) == 0 {
			return "", fmt.Errorf("bulk fixture has no %s entities", kind)
		}
	}
	spanSource := map[string]string{}
	for i := range spans {
		spanSource[spans[i]] = sources[i]
	}
	// Candidates touch approved core entities too, so a leak of unapproved state would
	// surface in traversals from the measured roots.
	choose := func(kind string, coreRate int) string {
		if core := fx.core[kind]; len(core) > 0 && h.rng.Intn(100) < coreRate {
			return core[h.rng.Intn(len(core))]
		}
		return pool[kind][h.rng.Intn(len(pool[kind]))]
	}
	const claimSQL = `INSERT INTO supervisor.knowledge_claim_heads(tenant_id,site_id,id,current_revision) SELECT $1,$2,x.id::uuid,1 FROM unnest($3::text[],$4::text[],$5::text[],$6::text[],$7::text[],$8::text[],$9::text[]) x(id,s,p,o,j,sp,src);
INSERT INTO supervisor.knowledge_claim_revisions(tenant_id,site_id,claim_id,revision,subject_id,predicate,object_entity_id,payload,actor_issuer,actor_subject) SELECT $1,$2,x.id::uuid,1,x.s::uuid,x.p,x.o::uuid,x.j::jsonb,$10,$11 FROM unnest($3::text[],$4::text[],$5::text[],$6::text[],$7::text[],$8::text[],$9::text[]) x(id,s,p,o,j,sp,src);
INSERT INTO supervisor.knowledge_claim_entity_refs(tenant_id,site_id,claim_id,claim_revision,subject_id,subject_revision,object_entity_id,object_revision) SELECT $1,$2,x.id::uuid,1,x.s::uuid,1,x.o::uuid,1 FROM unnest($3::text[],$4::text[],$5::text[],$6::text[],$7::text[],$8::text[],$9::text[]) x(id,s,p,o,j,sp,src);
INSERT INTO supervisor.knowledge_claim_spans(tenant_id,site_id,claim_id,claim_revision,span_id,source_id,source_revision) SELECT $1,$2,x.id::uuid,1,x.sp::uuid,x.src::uuid,1 FROM unnest($3::text[],$4::text[],$5::text[],$6::text[],$7::text[],$8::text[],$9::text[]) x(id,s,p,o,j,sp,src)`
	var subjects, predicates, objects, claimSpans, claimSources []string
	ids, payloads = nil, nil
	until := "2026-12-01T00:00:00Z"
	for n := range claims {
		e := edges[h.rng.Intn(len(edges))]
		in := knowledge.ClaimInput{ClaimID: h.id(), SubjectID: choose(e.subject, 10), SubjectRevision: 1, Predicate: e.predicate, ObjectEntityID: choose(e.object, 5), ObjectRevision: 1, EffectiveFrom: validFrom, EvidenceClass: "report", SourceSpanIDs: []string{spans[h.rng.Intn(len(spans))]}}
		if h.rng.Intn(10) < 3 {
			in.EffectiveTo = &until
		}
		if err := in.Validate(); err != nil {
			return "", err
		}
		if err := in.ValidateEntityRevisions(); err != nil {
			return "", err
		}
		if err := knowledge.ValidateEntityPair(e.predicate, e.subject, e.object); err != nil {
			return "", err
		}
		payload, _ := json.Marshal(in)
		ids, subjects, predicates, objects = append(ids, in.ClaimID), append(subjects, in.SubjectID), append(predicates, in.Predicate), append(objects, in.ObjectEntityID)
		payloads, claimSpans, claimSources = append(payloads, string(payload)), append(claimSpans, in.SourceSpanIDs[0]), append(claimSources, spanSource[in.SourceSpanIDs[0]])
		if len(ids) == batch || n == claims-1 {
			if err := flush(claimSQL, sc.TenantID, sc.SiteID, ids, subjects, predicates, objects, payloads, claimSpans, claimSources, h.owner.Issuer, h.owner.Subject); err != nil {
				return "", err
			}
			h.logf("  %d/%d candidate claims", n+1, claims)
			ids, subjects, predicates, objects, payloads, claimSpans, claimSources = nil, nil, nil, nil, nil, nil, nil
		}
	}
	return pool["material"][0], nil
}
