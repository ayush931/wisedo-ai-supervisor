package knowledge

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"strings"
	"time"
)

var ErrSnapshotConflict = errors.New("snapshot ID already exists")

// MaxSnapshotBytes bounds the canonical body of one approved snapshot (WIS-621, migration 072).
// The binding limits are the documented 200 entities and 500 claims; at those bounds a body
// measures about 680 KiB typically and about 4.1 MiB with every field at its contract maximum.
// The graph read keeps its separate 64 KiB returned-text budget.
const MaxSnapshotBytes = 8 << 20

// encodeSnapshot returns the canonical body of a snapshot, or ErrTraversalLimit when it is larger
// than MaxSnapshotBytes: an oversized capture is refused whole, never truncated.
func encodeSnapshot(contents SnapshotContents) ([]byte, error) {
	canonical, err := json.Marshal(contents)
	if err != nil {
		return nil, err
	}
	if len(canonical) > MaxSnapshotBytes {
		return nil, ErrTraversalLimit
	}
	return canonical, nil
}

type SnapshotInput struct {
	SnapshotID string `json:"snapshot_id"`
	Reason     string `json:"reason"`
}
type SnapshotAuthority struct {
	DecisionID     string `json:"decision_id"`
	SourceID       string `json:"source_id"`
	SourceRevision int64  `json:"source_revision"`
	Revision       int64  `json:"revision"`
}
type SnapshotEntity struct {
	EntityInput
	Revision int64 `json:"revision"`
}
type SnapshotClaim struct {
	ClaimInput
	Revision       int64             `json:"revision"`
	ReviewID       string            `json:"review_id"`
	Authority      SnapshotAuthority `json:"authority"`
	ResolutionRefs []ResolutionRef   `json:"resolution_refs"`
}

// Avoid promoting the strict input decoders to persisted proof wrappers.
func (s *SnapshotEntity) UnmarshalJSON(data []byte) error {
	type payload EntityInput
	var value struct {
		payload
		Revision int64 `json:"revision"`
	}
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	s.EntityInput = EntityInput(value.payload)
	s.Revision = value.Revision
	return nil
}
func (s *SnapshotClaim) UnmarshalJSON(data []byte) error {
	type payload ClaimInput
	var value struct {
		payload
		Revision       int64             `json:"revision"`
		ReviewID       string            `json:"review_id"`
		Authority      SnapshotAuthority `json:"authority"`
		ResolutionRefs []ResolutionRef   `json:"resolution_refs"`
	}
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	s.ClaimInput = ClaimInput(value.payload)
	s.Revision = value.Revision
	s.ReviewID = value.ReviewID
	s.Authority = value.Authority
	s.ResolutionRefs = value.ResolutionRefs
	return nil
}

type SnapshotSource struct {
	SourceID    string `json:"source_id"`
	Revision    int64  `json:"revision"`
	SHA256      string `json:"sha256"`
	AccessEpoch int64  `json:"access_epoch"`
}
type SnapshotSpan struct {
	SpanID   string `json:"span_id"`
	SourceID string `json:"source_id"`
	Revision int64  `json:"revision"`
	Start    int    `json:"start_rune"`
	End      int    `json:"end_rune"`
}
type SnapshotResolution struct {
	ResolutionRef
	Decision  string            `json:"decision"`
	Left      ConflictClaimRef  `json:"left"`
	Right     ConflictClaimRef  `json:"right"`
	Authority SnapshotAuthority `json:"authority"`
}
type SnapshotContents struct {
	SchemaVersion     int                  `json:"schema_version"`
	TenantID          string               `json:"tenant_id"`
	SiteID            string               `json:"site_id"`
	InventoryRevision int64                `json:"inventory_revision"`
	Entities          []SnapshotEntity     `json:"entities"`
	Claims            []SnapshotClaim      `json:"claims"`
	Sources           []SnapshotSource     `json:"sources"`
	Spans             []SnapshotSpan       `json:"spans"`
	Resolutions       []SnapshotResolution `json:"resolutions"`
}
type Snapshot struct {
	SchemaVersion            int              `json:"schema_version"`
	SnapshotID               string           `json:"snapshot_id"`
	Reason                   string           `json:"reason"`
	ActorIssuer              string           `json:"actor_issuer"`
	ActorSubject             string           `json:"actor_subject"`
	AuthenticatedAt          time.Time        `json:"authenticated_at"`
	CreatedAt                time.Time        `json:"created_at"`
	InventoryRevision        int64            `json:"inventory_revision"`
	CurrentInventoryRevision int64            `json:"current_inventory_revision"`
	Stale                    bool             `json:"stale"`
	SHA256                   string           `json:"sha256"`
	Contents                 SnapshotContents `json:"contents"`
}

func inventoryRevision(ctx context.Context, tx *sql.Tx, sc Scope) (int64, error) {
	var revision int64
	err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT revision FROM supervisor.knowledge_inventory_revisions WHERE tenant_id=$1 AND site_id=$2),0)`, sc.TenantID, sc.SiteID).Scan(&revision)
	return revision, err
}

const snapshotCandidatesSQL = ` FROM supervisor.knowledge_claim_heads h JOIN supervisor.knowledge_claim_reviews r ON (r.tenant_id,r.site_id,r.claim_id,r.claim_revision)=(h.tenant_id,h.site_id,h.id,h.current_revision) WHERE h.tenant_id=$1 AND h.site_id=$2 AND r.verdict='approve'`

func snapshotCandidates(ctx context.Context, tx *sql.Tx, sc Scope) ([]ConflictClaimRef, error) {
	rows, err := tx.QueryContext(ctx, `SELECT r.claim_id,r.claim_revision`+snapshotCandidatesSQL+` ORDER BY r.claim_id LIMIT 501`, sc.TenantID, sc.SiteID)
	if err != nil {
		return nil, err
	}
	candidates := []ConflictClaimRef{}
	for rows.Next() {
		var ref ConflictClaimRef
		if err = rows.Scan(&ref.ClaimID, &ref.Revision); err != nil {
			rows.Close()
			return nil, err
		}
		candidates = append(candidates, ref)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if len(candidates) > 500 {
		return nil, ErrTraversalLimit
	}
	// Bound total persisted pair visits before any per-review resolution evaluation.
	// A pair shared by two candidates counts twice, matching the work it causes.
	var dependencies int
	err = tx.QueryRowContext(ctx, `SELECT count(*) FROM (SELECT 1 FROM supervisor.knowledge_conflicts c JOIN (SELECT r.claim_id,r.claim_revision`+snapshotCandidatesSQL+`) r ON ((c.left_claim_id=r.claim_id AND c.left_revision=r.claim_revision) OR (c.right_claim_id=r.claim_id AND c.right_revision=r.claim_revision)) WHERE c.tenant_id=$1 AND c.site_id=$2 LIMIT 1001) bounded`, sc.TenantID, sc.SiteID).Scan(&dependencies)
	if err != nil {
		return nil, err
	}
	if dependencies > 1000 {
		return nil, ErrTraversalLimit
	}
	return candidates, nil
}

func snapshotContents(ctx context.Context, tx *sql.Tx, sc Scope, revision int64) (SnapshotContents, error) {
	out := SnapshotContents{SchemaVersion: 1, TenantID: sc.TenantID, SiteID: sc.SiteID, InventoryRevision: revision, Entities: []SnapshotEntity{}, Claims: []SnapshotClaim{}, Sources: []SnapshotSource{}, Spans: []SnapshotSpan{}, Resolutions: []SnapshotResolution{}}
	candidates, err := snapshotCandidates(ctx, tx, sc)
	if err != nil {
		return out, err
	}
	entities := map[string]Entity{}
	resolutions := map[string]SnapshotResolution{}
	refs := []sourceRef{}
	claims := []Claim{}
	for _, candidate := range candidates {
		claim, current, e := reviewClaimState(ctx, tx, sc, candidate.ClaimID, candidate.Revision)
		if errors.Is(e, ErrNotFound) {
			continue
		}
		if e != nil {
			return out, e
		}
		review, e := loadReview(ctx, tx, sc, candidate.ClaimID, candidate.Revision)
		if e != nil {
			return out, e
		}
		approved, e := reviewEligibility(ctx, tx, sc, review, current)
		if e != nil {
			return out, e
		}
		if !approved {
			continue
		}
		cr, subject, object, e := claimRefs(ctx, tx, sc, claim.ClaimInput)
		if e != nil {
			return out, e
		}
		refs = append(refs, cr...)
		entities[subject.EntityID] = subject
		if claim.ObjectEntityID != "" {
			entities[object.EntityID] = object
		}
		if len(entities) > 200 {
			return out, ErrTraversalLimit
		}
		// Input citation order is not semantically significant; canonicalize it.
		sort.Strings(claim.SourceSpanIDs)
		out.Claims = append(out.Claims, SnapshotClaim{ClaimInput: claim.ClaimInput, Revision: claim.Revision, ReviewID: review.ReviewID, Authority: SnapshotAuthority{review.AuthorityDecisionID, review.AuthoritySourceID, review.AuthoritySourceRevision, review.AuthorityRevision}, ResolutionRefs: review.ResolutionRefs})
		claims = append(claims, claim)
		for _, pin := range review.ResolutionRefs {
			if _, ok := resolutions[pin.ConflictID]; ok {
				continue
			}
			resolution, e := loadResolution(ctx, tx, sc, pin.ConflictID, pin.Revision)
			if e != nil {
				return out, e
			}
			resolutions[pin.ConflictID] = SnapshotResolution{ResolutionRef: pin, Decision: resolution.Decision, Left: resolution.Left, Right: resolution.Right, Authority: SnapshotAuthority{resolution.AuthorityDecisionID, resolution.AuthoritySourceID, resolution.AuthoritySourceRevision, resolution.AuthorityRevision}}
			// Peer facts stay outside approved membership; their provenance still gates access.
			for _, peer := range []ConflictClaimRef{resolution.Left, resolution.Right} {
				peerClaim, e := loadClaim(ctx, tx, sc, peer.ClaimID, peer.Revision)
				if e != nil {
					return out, e
				}
				pr, _, _, e := claimRefs(ctx, tx, sc, peerClaim.ClaimInput)
				if e != nil {
					return out, e
				}
				refs = append(refs, pr...)
			}
		}
	}
	graphEntities := make([]Entity, 0, len(entities))
	for _, entity := range entities {
		graphEntities = append(graphEntities, entity)
		out.Entities = append(out.Entities, SnapshotEntity{entity.EntityInput, entity.Revision})
	}
	if err = ValidateApprovedGraph(graphEntities, claims); err != nil {
		return out, err
	}
	for _, resolution := range resolutions {
		out.Resolutions = append(out.Resolutions, resolution)
	}
	sort.Slice(out.Entities, func(i, j int) bool { return out.Entities[i].EntityID < out.Entities[j].EntityID })
	sort.Slice(out.Resolutions, func(i, j int) bool { return out.Resolutions[i].ConflictID < out.Resolutions[j].ConflictID })
	// Exact source hashes and span locators contain no source body or quote text.
	sources := map[ConflictClaimRef]bool{}
	spans := map[string]bool{}
	for _, ref := range refs {
		sources[ConflictClaimRef{ClaimID: ref.id, Revision: ref.revision}] = true
		if ref.spanID != "" {
			spans[ref.spanID] = true
		}
	}
	for ref := range sources {
		pin := SnapshotSource{SourceID: ref.ClaimID, Revision: ref.Revision}
		err = tx.QueryRowContext(ctx, `SELECT r.sha256,h.access_epoch FROM supervisor.evidence_revisions r JOIN supervisor.evidence_heads h USING(tenant_id,site_id,source_id) WHERE r.tenant_id=$1 AND r.site_id=$2 AND r.source_id=$3 AND r.revision=$4`, sc.TenantID, sc.SiteID, pin.SourceID, pin.Revision).Scan(&pin.SHA256, &pin.AccessEpoch)
		if err != nil {
			return out, err
		}
		out.Sources = append(out.Sources, pin)
	}
	for id := range spans {
		pin := SnapshotSpan{SpanID: id}
		err = tx.QueryRowContext(ctx, `SELECT source_id,revision,start_rune,end_rune FROM supervisor.evidence_spans WHERE tenant_id=$1 AND site_id=$2 AND id=$3`, sc.TenantID, sc.SiteID, id).Scan(&pin.SourceID, &pin.Revision, &pin.Start, &pin.End)
		if err != nil {
			return out, err
		}
		out.Spans = append(out.Spans, pin)
	}
	sort.Slice(out.Sources, func(i, j int) bool {
		if out.Sources[i].SourceID == out.Sources[j].SourceID {
			return out.Sources[i].Revision < out.Sources[j].Revision
		}
		return out.Sources[i].SourceID < out.Sources[j].SourceID
	})
	sort.Slice(out.Spans, func(i, j int) bool { return out.Spans[i].SpanID < out.Spans[j].SpanID })
	return out, nil
}

func loadSnapshot(ctx context.Context, tx *sql.Tx, sc Scope, id string) (Snapshot, error) {
	var out Snapshot
	var canonical string
	err := tx.QueryRowContext(ctx, `SELECT schema_version,snapshot_id,reason,actor_issuer,actor_subject,auth_time,created_at,inventory_revision,sha256,canonical_contents FROM supervisor.knowledge_process_snapshots WHERE tenant_id=$1 AND site_id=$2 AND snapshot_id=$3`, sc.TenantID, sc.SiteID, id).Scan(&out.SchemaVersion, &out.SnapshotID, &out.Reason, &out.ActorIssuer, &out.ActorSubject, &out.AuthenticatedAt, &out.CreatedAt, &out.InventoryRevision, &out.SHA256, &canonical)
	if errors.Is(err, sql.ErrNoRows) {
		return Snapshot{}, ErrNotFound
	}
	if err != nil {
		return Snapshot{}, err
	}
	if err = json.Unmarshal([]byte(canonical), &out.Contents); err != nil {
		return Snapshot{}, err
	}
	refs := make([]sourceRef, 0, len(out.Contents.Sources))
	for _, source := range out.Contents.Sources {
		refs = append(refs, sourceRef{id: source.SourceID, revision: source.Revision})
	}
	states, err := lockSources(ctx, tx, sc, refs)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Snapshot{}, errors.Join(ErrSnapshotEvidenceUnavailable, err)
		}
		return Snapshot{}, err
	}
	if _, err = eligible(refs, states, false); err != nil {
		if errors.Is(err, ErrNotFound) {
			return Snapshot{}, errors.Join(ErrSnapshotEvidenceUnavailable, err)
		}
		return Snapshot{}, err
	}
	if err = pruneFallen(ctx, tx, sc, &out.Contents); err != nil {
		return Snapshot{}, err
	}
	out.CurrentInventoryRevision, err = inventoryRevision(ctx, tx, sc)
	if err != nil {
		return Snapshot{}, err
	}
	out.Stale = out.InventoryRevision != out.CurrentInventoryRevision
	return out, nil
}

// designationStands is the SQL test that the authority pin (source, authority revision,
// decision) of row r is still the source's current designation, and that a model-extracted
// source revision it designates was confirmed (WIS-625, migration 077): the pins reviewAuthority
// holds, without its current-source-revision check.
func designationStands(r, source, revision, decision string) string {
	return `EXISTS(SELECT 1 FROM supervisor.evidence_authority_heads ah
 JOIN supervisor.evidence_authority_decisions d ON (d.tenant_id,d.site_id,d.source_id,d.revision)=(ah.tenant_id,ah.site_id,ah.source_id,ah.current_revision)
 JOIN supervisor.evidence_revisions e ON (e.tenant_id,e.site_id,e.source_id,e.revision)=(d.tenant_id,d.site_id,d.source_id,d.source_revision)
 WHERE (ah.tenant_id,ah.site_id)=(` + r + `.tenant_id,` + r + `.site_id) AND ah.source_id=` + source + ` AND ah.current_revision=` + revision + ` AND d.decision_id=` + decision + `
 AND d.verdict='designate' AND (e.extraction IS NULL OR d.model_extracted_confirmed))`
}

// ApprovalStandsSQL is the SQL condition that the knowledge_claim_reviews row aliased r is an
// approval that still stands (WIS-517 review): not withdrawn, its designation still current (and
// confirmed if it designates model-extracted text), and every conflict resolution it rests on
// still the conflict's current decision, not a withdrawal, with its own designation standing.
// These are the checks reviewEligibility makes, without its current-revision checks, so a stale
// snapshot keeps the approvals that still stand. A conflict recorded later does not count: a
// candidate claim, which an agent can write, must not suppress an approved fact; only a human
// decision removes it. Snapshot loading and the embedding queue (internal/retrieval) share it.
func ApprovalStandsSQL(r string) string {
	return `(` + r + `.verdict='approve' AND ` + designationStands(r, r+".authority_source_id", r+".authority_revision", r+".authority_decision_id") + `
 AND NOT EXISTS(SELECT 1 FROM supervisor.knowledge_review_revocations v WHERE (v.tenant_id,v.site_id,v.review_id)=(` + r + `.tenant_id,` + r + `.site_id,` + r + `.review_id))
 AND NOT EXISTS(SELECT 1 FROM supervisor.knowledge_review_resolution_refs p
  JOIN supervisor.knowledge_resolution_heads rh ON (rh.tenant_id,rh.site_id,rh.conflict_id)=(p.tenant_id,p.site_id,p.conflict_id)
  JOIN supervisor.knowledge_resolution_decisions rd ON (rd.tenant_id,rd.site_id,rd.conflict_id,rd.revision)=(p.tenant_id,p.site_id,p.conflict_id,p.resolution_revision)
  WHERE (p.tenant_id,p.site_id,p.review_id)=(` + r + `.tenant_id,` + r + `.site_id,` + r + `.review_id)
  AND (rh.current_revision<>p.resolution_revision OR rd.decision='withdraw'
   OR (rd.authority_decision_id IS NOT NULL AND NOT ` + designationStands("rd", "rd.authority_source_id", "rd.authority_revision", "rd.authority_decision_id") + `))))`
}

// fallenApprovalsSQL returns the snapshot reviews whose approval no longer stands.
var fallenApprovalsSQL = `SELECT x.review_id::text FROM unnest($3::uuid[]) x(review_id)
 LEFT JOIN supervisor.knowledge_claim_reviews r ON (r.tenant_id,r.site_id,r.review_id)=($1,$2,x.review_id)
 WHERE r.review_id IS NULL OR NOT ` + ApprovalStandsSQL("r")

// pruneFallen drops the claims whose approval fell after the snapshot, then the entities,
// resolutions and span locators only those claims referenced, so nothing about them reaches a
// reader (chat, agent tools, advisor, workstation, the snapshot read; rule 06). Sources stay: they
// carry no content and are the revocation gate already applied to the whole snapshot. The stored
// body and its SHA256 stay as captured; the digest attests the capture, and Contents is that
// capture less what no longer stands.
func pruneFallen(ctx context.Context, tx *sql.Tx, sc Scope, c *SnapshotContents) error {
	if len(c.Claims) == 0 {
		return nil
	}
	ids := make([]string, len(c.Claims))
	for i, cl := range c.Claims {
		ids[i] = cl.ReviewID
	}
	rows, err := tx.QueryContext(ctx, fallenApprovalsSQL, sc.TenantID, sc.SiteID, ids)
	if err != nil {
		return err
	}
	fallen := map[string]bool{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return errors.Join(err, rows.Close())
		}
		fallen[id] = true
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil || len(fallen) == 0 {
		return err
	}
	c.Claims = slices.DeleteFunc(c.Claims, func(cl SnapshotClaim) bool { return fallen[cl.ReviewID] })
	entities, conflicts, spans := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, cl := range c.Claims {
		entities[cl.SubjectID], entities[cl.ObjectEntityID] = true, true
		for _, r := range cl.ResolutionRefs {
			conflicts[r.ConflictID] = true
		}
		for _, id := range cl.SourceSpanIDs {
			spans[id] = true
		}
	}
	c.Entities = slices.DeleteFunc(c.Entities, func(e SnapshotEntity) bool { return !entities[e.EntityID] })
	c.Resolutions = slices.DeleteFunc(c.Resolutions, func(r SnapshotResolution) bool { return !conflicts[r.ConflictID] })
	// A kept resolution keeps its peer claims' span locators, as snapshotContents pinned them:
	// they gate access and approve nothing.
	for _, r := range c.Resolutions {
		for _, peer := range []ConflictClaimRef{r.Left, r.Right} {
			pc, e := loadClaim(ctx, tx, sc, peer.ClaimID, peer.Revision)
			if e != nil {
				return e
			}
			for _, id := range pc.SourceSpanIDs {
				spans[id] = true
			}
		}
	}
	c.Spans = slices.DeleteFunc(c.Spans, func(s SnapshotSpan) bool { return !spans[s.SpanID] })
	return nil
}

func (s *Service) CreateSnapshot(ctx context.Context, a Actor, sc Scope, in SnapshotInput, key string, expected int64) (Snapshot, error) {
	if !contractUUID(in.SnapshotID) || !contractText(in.Reason, 1000) || strings.TrimSpace(in.Reason) == "" || !mutation(key, expected) {
		return Snapshot{}, ErrInvalidContract
	}
	tx, err := s.beginLocked(ctx, a, sc, true, true)
	if err != nil {
		return Snapshot{}, err
	}
	defer tx.Rollback()
	auth, err := s.requireReviewer(ctx, tx, a, sc)
	if err != nil {
		return Snapshot{}, err
	}
	digest := fingerprint("snapshot", in, expected)
	_, replayed, err := replay(ctx, tx, a, sc, key, digest)
	if err != nil {
		return Snapshot{}, err
	}
	if replayed {
		out, e := loadSnapshot(ctx, tx, sc, in.SnapshotID)
		if e != nil {
			return Snapshot{}, e
		}
		if e = audit(ctx, tx, a, sc, in.SnapshotID, 1, "mutation.replayed"); e != nil {
			return Snapshot{}, e
		}
		if e = s.finishReview(ctx, tx, a); e != nil {
			return Snapshot{}, e
		}
		return out, nil
	}
	revision, err := inventoryRevision(ctx, tx, sc)
	if err != nil {
		return Snapshot{}, err
	}
	if revision != expected {
		return Snapshot{}, ErrRevision
	}
	var exists bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM supervisor.knowledge_process_snapshots WHERE tenant_id=$1 AND site_id=$2 AND snapshot_id=$3)`, sc.TenantID, sc.SiteID, in.SnapshotID).Scan(&exists); err != nil {
		return Snapshot{}, err
	}
	if exists {
		return Snapshot{}, ErrSnapshotConflict
	}
	contents, err := snapshotContents(ctx, tx, sc, revision)
	if err != nil {
		return Snapshot{}, err
	}
	canonical, err := encodeSnapshot(contents)
	if err != nil {
		return Snapshot{}, err
	}
	hash := sha256.Sum256(canonical)
	digestContents := hex.EncodeToString(hash[:])
	_, err = tx.ExecContext(ctx, `INSERT INTO supervisor.knowledge_process_snapshots(tenant_id,site_id,snapshot_id,inventory_revision,sha256,canonical_contents,reason,actor_issuer,actor_subject,auth_time) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, sc.TenantID, sc.SiteID, in.SnapshotID, revision, digestContents, string(canonical), in.Reason, a.Issuer, a.Subject, auth)
	if err != nil {
		return Snapshot{}, err
	}
	for _, claim := range contents.Claims {
		_, err = tx.ExecContext(ctx, `INSERT INTO supervisor.knowledge_snapshot_claims(tenant_id,site_id,snapshot_id,claim_id,claim_revision,review_id) VALUES($1,$2,$3,$4,$5,$6)`, sc.TenantID, sc.SiteID, in.SnapshotID, claim.ClaimID, claim.Revision, claim.ReviewID)
		if err != nil {
			return Snapshot{}, err
		}
	}
	for _, entity := range contents.Entities {
		_, err = tx.ExecContext(ctx, `INSERT INTO supervisor.knowledge_snapshot_entities(tenant_id,site_id,snapshot_id,entity_id,entity_revision) VALUES($1,$2,$3,$4,$5)`, sc.TenantID, sc.SiteID, in.SnapshotID, entity.EntityID, entity.Revision)
		if err != nil {
			return Snapshot{}, err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO supervisor.knowledge_outbox(tenant_id,site_id,snapshot_id,inventory_revision,sha256) VALUES($1,$2,$3,$4,$5)`, sc.TenantID, sc.SiteID, in.SnapshotID, revision, digestContents)
	if err != nil {
		return Snapshot{}, err
	}
	if err = saveReceipt(ctx, tx, a, sc, key, digest, "snapshot", in.SnapshotID, 1); err != nil {
		return Snapshot{}, err
	}
	if err = audit(ctx, tx, a, sc, in.SnapshotID, 1, "snapshot.created"); err != nil {
		return Snapshot{}, err
	}
	out, err := loadSnapshot(ctx, tx, sc, in.SnapshotID)
	if err != nil {
		return Snapshot{}, err
	}
	if err = s.finishReview(ctx, tx, a); err != nil {
		return Snapshot{}, err
	}
	return out, nil
}

func (s *Service) ReadSnapshot(ctx context.Context, a Actor, sc Scope, id string) (Snapshot, error) {
	if !contractUUID(id) {
		return Snapshot{}, ErrInvalidContract
	}
	tx, err := s.beginLocked(ctx, a, sc, false, true)
	if err != nil {
		return Snapshot{}, err
	}
	defer tx.Rollback()
	out, err := loadSnapshot(ctx, tx, sc, id)
	if err != nil {
		return Snapshot{}, err
	}
	if err = audit(ctx, tx, a, sc, id, 1, "snapshot.read"); err != nil {
		return Snapshot{}, err
	}
	if err = s.finish(ctx, tx, a); err != nil {
		return Snapshot{}, err
	}
	return out, nil
}

// LoadSnapshotTx loads exactly the pinned snapshot inside the caller's transaction, which must
// already carry the transaction-local tenant context. Unlike ReadKnowledge it never selects the
// latest snapshot for an as-of time, so a run cannot drift from the snapshot its context pinned.
func LoadSnapshotTx(ctx context.Context, tx *sql.Tx, sc Scope, snapshotID string) (Snapshot, error) {
	if !contractUUID(sc.TenantID) || !contractUUID(sc.SiteID) || !contractUUID(snapshotID) {
		return Snapshot{}, ErrInvalidContract
	}
	return loadSnapshot(ctx, tx, sc, snapshotID)
}
