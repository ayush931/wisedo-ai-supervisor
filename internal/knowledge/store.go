package knowledge

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/session"
)

var (
	ErrNotFound                    = errors.New("candidate unavailable")
	ErrSnapshotEvidenceUnavailable = errors.New("snapshot evidence unavailable")
	ErrDenied                      = errors.New("candidate permission denied")
	ErrConflict                    = errors.New("idempotency conflict")
	ErrRevision                    = errors.New("candidate revision changed")
	ErrSourceChanged               = errors.New("candidate source revision changed")
	ErrCycle                       = errors.New("candidate relation would create a cycle")
	ErrTraversalLimit              = errors.New("candidate cycle search limit reached")
)

type Actor struct{ Issuer, Subject, SessionHash string }
type Scope struct{ TenantID, SiteID string }
type Entity struct {
	EntityInput
	Revision        int64     `json:"revision"`
	CurrentRevision int64     `json:"current_revision"`
	State           string    `json:"state"`
	CreatedAt       time.Time `json:"created_at"`
	ActorIssuer     string    `json:"actor_issuer"`
	ActorSubject    string    `json:"actor_subject"`
	SourcesCurrent  bool      `json:"sources_current"`
}
type Claim struct {
	ClaimInput
	Revision        int64         `json:"revision"`
	CurrentRevision int64         `json:"current_revision"`
	State           string        `json:"state"`
	CreatedAt       time.Time     `json:"created_at"`
	ActorIssuer     string        `json:"actor_issuer"`
	ActorSubject    string        `json:"actor_subject"`
	SourcesCurrent  bool          `json:"sources_current"`
	Corroboration   Corroboration `json:"corroboration"`
}

// Corroboration is derived on read and is never a promotion gate: approval still requires an
// authorized reviewer and a designated, exact, current source (WIS-270).
type Corroboration struct {
	// IndependentGroups counts distinct independence groups among the cited spans' current,
	// unrevoked source revisions. Copies of one SOP share a group and count once. Text a model
	// extracted (WIS-625 OCR) never counts: it is a reading of a source, not a source.
	IndependentGroups int `json:"independent_groups"`
	// ModelExtractedSources counts the cited current sources whose text a model extracted.
	ModelExtractedSources int `json:"model_extracted_sources"`
	// Authoritative is true when a cited exact current revision is owner-designated now. It
	// reads the designation ledger, never the caller-declared authority_class.
	Authoritative bool `json:"authoritative"`
}

func claimCorroboration(ctx context.Context, tx *sql.Tx, sc Scope, id string, revision int64) (Corroboration, error) {
	var out Corroboration
	err := tx.QueryRowContext(ctx, `SELECT count(DISTINCT e.independence_group_id) FILTER (WHERE e.extraction IS NULL),count(DISTINCT e.source_id) FILTER (WHERE e.extraction IS NOT NULL) FROM supervisor.knowledge_claim_spans s
 JOIN supervisor.evidence_heads h ON (h.tenant_id,h.site_id,h.source_id,h.current_revision)=(s.tenant_id,s.site_id,s.source_id,s.source_revision)
 JOIN supervisor.evidence_revisions e ON (e.tenant_id,e.site_id,e.source_id,e.revision)=(s.tenant_id,s.site_id,s.source_id,s.source_revision)
 WHERE s.tenant_id=$1 AND s.site_id=$2 AND s.claim_id=$3 AND s.claim_revision=$4 AND NOT h.revoked
 AND NOT EXISTS(SELECT 1 FROM supervisor.evidence_revocations rv WHERE (rv.tenant_id,rv.site_id,rv.source_id)=(h.tenant_id,h.site_id,h.source_id))`, sc.TenantID, sc.SiteID, id, revision).Scan(&out.IndependentGroups, &out.ModelExtractedSources)
	if err != nil {
		return out, err
	}
	_, out.Authoritative, err = reviewAuthority(ctx, tx, sc, id, revision, "")
	return out, err
}

type ClaimSummary struct {
	ClaimID         string `json:"claim_id"`
	Revision        int64  `json:"revision"`
	CurrentRevision int64  `json:"current_revision"`
	SubjectID       string `json:"subject_id"`
	Predicate       string `json:"predicate"`
	ObjectEntityID  string `json:"object_entity_id,omitempty"`
	State           string `json:"state"`
	// ReviewState is derived from the immutable review ledger for exactly this
	// revision. It is not execution eligibility: read the review for that.
	ReviewState    string        `json:"review_state"`
	SourcesCurrent bool          `json:"sources_current"`
	Corroboration  Corroboration `json:"corroboration"`
}
type EntityPage struct {
	Entities   []Entity `json:"entities"`
	NextCursor string   `json:"next_cursor,omitempty"`
}
type ClaimPage struct {
	Claims     []ClaimSummary `json:"claims"`
	NextCursor string         `json:"next_cursor,omitempty"`
}
type Service struct {
	db       *sql.DB
	sessions session.Policy
}

func New(db *sql.DB) *Service { return &Service{db: db, sessions: session.Default()} }

// WithSessionPolicy returns a copy of the service that enforces p (WIS-290: one configurable
// session policy for every service). New uses session.Default.
func (s *Service) WithSessionPolicy(p session.Policy) *Service {
	c := *s
	c.sessions = p.OrDefault()
	return &c
}

func (s *Service) checkSession(ctx context.Context, tx *sql.Tx, a Actor) error {
	decoded, err := hex.DecodeString(a.SessionHash)
	if err != nil || len(decoded) != 32 || strings.ToLower(a.SessionHash) != a.SessionHash {
		return ErrDenied
	}
	var issuer, subject string
	var expiry, seen, now time.Time
	err = tx.QueryRowContext(ctx, `SELECT issuer,subject,expires_at,last_seen_at,clock_timestamp() FROM supervisor.browser_sessions WHERE token_hash=$1 FOR SHARE`, a.SessionHash).Scan(&issuer, &subject, &expiry, &seen, &now)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrDenied
	}
	if err != nil {
		return err
	}
	if issuer != a.Issuer || subject != a.Subject || !expiry.After(now) || !seen.After(now.Add(-s.sessions.Idle)) {
		return ErrDenied
	}
	return nil
}

// ponytail: serialize candidate/evidence writes per site; narrow locks only after measured contention.
func (s *Service) begin(ctx context.Context, a Actor, sc Scope, write bool) (*sql.Tx, error) {
	return s.beginLocked(ctx, a, sc, write, write)
}

// Reviews serialize their evidence projection before membership and provenance locks.
func (s *Service) beginLocked(ctx context.Context, a Actor, sc Scope, write, serialize bool) (*sql.Tx, error) {
	if !contractUUID(sc.TenantID) || !contractUUID(sc.SiteID) {
		return nil, ErrInvalidContract
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	fail := func(e error) (*sql.Tx, error) { _ = tx.Rollback(); return nil, e }
	if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true),set_config('statement_timeout','10s',true),set_config('lock_timeout','5s',true)`, sc.TenantID); err != nil {
		return fail(err)
	}
	if err = s.checkSession(ctx, tx, a); err != nil {
		return fail(err)
	}
	var found bool
	if err = tx.QueryRowContext(ctx, `SELECT supervisor.lock_member_site($1,$2,$3,$4,$5)`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, serialize).Scan(&found); err != nil {
		return fail(err)
	}
	if !found {
		return fail(ErrNotFound)
	}
	var role string
	err = tx.QueryRowContext(ctx, `SELECT role FROM supervisor.memberships WHERE tenant_id=$1 AND site_id=$2 AND issuer=$3 AND subject=$4 AND active FOR SHARE`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return fail(ErrNotFound)
	}
	if err != nil {
		return fail(err)
	}
	if write && role != "owner" && role != "supervisor" && role != "operator" {
		return fail(ErrDenied)
	}
	if err = s.checkSession(ctx, tx, a); err != nil {
		return fail(err)
	}
	return tx, nil
}
func (s *Service) finish(ctx context.Context, tx *sql.Tx, a Actor) error {
	if err := s.checkSession(ctx, tx, a); err != nil {
		return err
	}
	return tx.Commit()
}
func mutation(key string, expected int64) bool {
	return contractText(key, 128) && len(key) <= 128 && expected >= 0 && expected < math.MaxInt64
}
func fingerprint(op string, input any, expected int64) string {
	body, _ := json.Marshal(struct {
		Op       string
		Input    any
		Expected int64
	}{op, input, expected})
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

type sourceRef struct {
	id       string
	revision int64
	spanID   string
}
type sourceState struct {
	revision int64
	revoked  bool
}

func lockSources(ctx context.Context, tx *sql.Tx, sc Scope, refs []sourceRef) (map[string]sourceState, error) {
	ids := map[string]bool{}
	for _, r := range refs {
		ids[r.id] = true
	}
	sorted := make([]string, 0, len(ids))
	for id := range ids {
		sorted = append(sorted, id)
	}
	sort.Strings(sorted)
	states := map[string]sourceState{}
	for _, id := range sorted {
		var state sourceState
		err := tx.QueryRowContext(ctx, `SELECT current_revision,(revoked OR EXISTS(SELECT 1 FROM supervisor.evidence_revocations v WHERE (v.tenant_id,v.site_id,v.source_id)=(h.tenant_id,h.site_id,h.source_id))) FROM supervisor.evidence_heads h WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3 FOR SHARE`, sc.TenantID, sc.SiteID, id).Scan(&state.revision, &state.revoked)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		if err != nil {
			return nil, err
		}
		states[id] = state
	}
	return states, nil
}
func eligible(refs []sourceRef, states map[string]sourceState, requireCurrent bool) (bool, error) {
	current := true
	for _, r := range refs {
		state, ok := states[r.id]
		if !ok || state.revoked {
			return false, ErrNotFound
		}
		if state.revision != r.revision {
			current = false
		}
	}
	if requireCurrent && !current {
		return false, ErrSourceChanged
	}
	return current, nil
}
func loadEntity(ctx context.Context, tx *sql.Tx, sc Scope, id string, revision int64) (Entity, error) {
	var out Entity
	var payload []byte
	err := tx.QueryRowContext(ctx, `SELECT r.payload,r.revision,h.current_revision,r.state,r.created_at,r.actor_issuer,r.actor_subject FROM supervisor.knowledge_entity_heads h JOIN supervisor.knowledge_entity_revisions r ON (r.tenant_id,r.site_id,r.id)=(h.tenant_id,h.site_id,h.id) WHERE h.tenant_id=$1 AND h.site_id=$2 AND h.id=$3 AND r.revision=CASE WHEN $4=0 THEN h.current_revision ELSE $4 END`, sc.TenantID, sc.SiteID, id, revision).Scan(&payload, &out.Revision, &out.CurrentRevision, &out.State, &out.CreatedAt, &out.ActorIssuer, &out.ActorSubject)
	if errors.Is(err, sql.ErrNoRows) {
		return out, ErrNotFound
	}
	if err != nil {
		return out, err
	}
	err = json.Unmarshal(payload, &out.EntityInput)
	return out, err
}
func spanRefs(ctx context.Context, tx *sql.Tx, sc Scope, ids []string) ([]sourceRef, error) {
	refs := []sourceRef{}
	for _, id := range ids {
		var ref sourceRef
		ref.spanID = id
		err := tx.QueryRowContext(ctx, `SELECT source_id,revision FROM supervisor.evidence_spans WHERE tenant_id=$1 AND site_id=$2 AND id=$3`, sc.TenantID, sc.SiteID, id).Scan(&ref.id, &ref.revision)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		if err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	return refs, nil
}
func claimRefs(ctx context.Context, tx *sql.Tx, sc Scope, in ClaimInput) ([]sourceRef, Entity, Entity, error) {
	subject, err := loadEntity(ctx, tx, sc, in.SubjectID, in.SubjectRevision)
	if err != nil {
		return nil, subject, Entity{}, err
	}
	refs := []sourceRef{{id: subject.SourceID, revision: subject.SourceRevision}}
	var object Entity
	if in.ObjectEntityID != "" {
		object, err = loadEntity(ctx, tx, sc, in.ObjectEntityID, in.ObjectRevision)
		if err != nil {
			return nil, subject, object, err
		}
		refs = append(refs, sourceRef{id: object.SourceID, revision: object.SourceRevision})
	}
	spans, err := spanRefs(ctx, tx, sc, in.SourceSpanIDs)
	return append(refs, spans...), subject, object, err
}
func audit(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, id any, rev any, event string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO supervisor.knowledge_audit(tenant_id,site_id,actor_issuer,actor_subject,target_id,revision,event_type) VALUES($1,$2,$3,$4,$5,$6,$7)`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, id, rev, event)
	return err
}
func replay(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, key, digest string) (int64, bool, error) {
	var revision int64
	var stored string
	err := tx.QueryRowContext(ctx, `SELECT request_hash,revision FROM supervisor.knowledge_receipts WHERE tenant_id=$1 AND site_id=$2 AND actor_issuer=$3 AND actor_subject=$4 AND idempotency_key=$5`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, key).Scan(&stored, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	if stored != digest {
		return 0, false, ErrConflict
	}
	return revision, true, nil
}
func saveReceipt(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, key, digest, op, id string, revision int64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO supervisor.knowledge_receipts(tenant_id,site_id,actor_issuer,actor_subject,idempotency_key,request_hash,operation,target_id,revision) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, key, digest, op, id, revision)
	return err
}
func lockEntities(ctx context.Context, tx *sql.Tx, sc Scope, ids []string) (map[string]int64, error) {
	sort.Strings(ids)
	out := map[string]int64{}
	for _, id := range ids {
		if id == "" || out[id] != 0 {
			continue
		}
		var revision int64
		err := tx.QueryRowContext(ctx, `SELECT current_revision FROM supervisor.knowledge_entity_heads WHERE tenant_id=$1 AND site_id=$2 AND id=$3 FOR SHARE`, sc.TenantID, sc.SiteID, id).Scan(&revision)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		if err != nil {
			return nil, err
		}
		out[id] = revision
	}
	return out, nil
}
func entityPinsCurrent(in ClaimInput, heads map[string]int64) bool {
	return heads[in.SubjectID] == in.SubjectRevision && (in.ObjectEntityID == "" || heads[in.ObjectEntityID] == in.ObjectRevision)
}
func (s *Service) CreateEntity(ctx context.Context, a Actor, sc Scope, in EntityInput, key string, expected int64) (Entity, error) {
	tx, err := s.begin(ctx, a, sc, true)
	if err != nil {
		return Entity{}, err
	}
	defer tx.Rollback()
	out, err := s.createEntityTx(ctx, tx, a, sc, in, key, expected)
	if err != nil {
		return Entity{}, err
	}
	return out, s.finish(ctx, tx, a)
}

// CreateEntityTx creates a candidate entity inside the caller's already scoped transaction. Like
// CreateClaimTx it is reserved for the server agent identity (WIS-612 onboarding specialist); a
// reviewer still approves, rejects or merges what it proposes.
func (s *Service) CreateEntityTx(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, in EntityInput, key string, expected int64) (Entity, error) {
	if tx == nil || a.Issuer != "supervisor:agent" || a.SessionHash != "" || !contractUUID(a.Subject) {
		return Entity{}, ErrDenied
	}
	return s.createEntityTx(ctx, tx, a, sc, in, key, expected)
}

func (s *Service) createEntityTx(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, in EntityInput, key string, expected int64) (Entity, error) {
	var out Entity
	if err := in.Validate(); err != nil {
		return out, err
	}
	if !mutation(key, expected) || !contractUUID(sc.TenantID) || !contractUUID(sc.SiteID) {
		return out, ErrInvalidContract
	}
	refs := []sourceRef{{id: in.SourceID, revision: in.SourceRevision}}
	states, err := lockSources(ctx, tx, sc, refs)
	if err != nil {
		return out, err
	}
	current, err := eligible(refs, states, false)
	if err != nil {
		return out, err
	}
	digest := fingerprint("entity", in, expected)
	prior, ok, err := replay(ctx, tx, a, sc, key, digest)
	if err != nil {
		return out, err
	}
	if ok {
		out, err = loadEntity(ctx, tx, sc, in.EntityID, prior)
		if err != nil {
			return out, err
		}
		out.SourcesCurrent = current && out.Revision == out.CurrentRevision
		if err = audit(ctx, tx, a, sc, in.EntityID, prior, "mutation.replayed"); err != nil {
			return out, err
		}
		return out, nil
	}
	if !current {
		return out, ErrSourceChanged
	}
	previous, err := loadEntity(ctx, tx, sc, in.EntityID, 0)
	exists := err == nil
	if err != nil && !errors.Is(err, ErrNotFound) {
		return out, err
	}
	if (!exists && expected != 0) || (exists && previous.Revision != expected) {
		return out, ErrRevision
	}
	if exists && (previous.Type != in.Type || previous.AssetKind != in.AssetKind) {
		return out, ErrInvalidContract
	}
	revision := expected + 1
	payload, _ := json.Marshal(in)
	if exists {
		_, err = tx.ExecContext(ctx, `UPDATE supervisor.knowledge_entity_heads SET current_revision=$4 WHERE tenant_id=$1 AND site_id=$2 AND id=$3`, sc.TenantID, sc.SiteID, in.EntityID, revision)
		// Checked after the head row lock, which waits for any concurrent merge of this entity.
		if merged, e := redirected(ctx, tx, sc, in.EntityID); err == nil && (e != nil || merged) {
			if e != nil {
				return out, e
			}
			return out, ErrEntityRedirected
		}
	} else {
		_, err = tx.ExecContext(ctx, `INSERT INTO supervisor.knowledge_entities(tenant_id,site_id,id,entity_type,payload,source_id,source_revision,actor_issuer,actor_subject) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, sc.TenantID, sc.SiteID, in.EntityID, in.Type, payload, in.SourceID, in.SourceRevision, a.Issuer, a.Subject)
		if err == nil {
			_, err = tx.ExecContext(ctx, `INSERT INTO supervisor.knowledge_entity_heads(tenant_id,site_id,id,current_revision) VALUES($1,$2,$3,1)`, sc.TenantID, sc.SiteID, in.EntityID)
		}
	}
	if err != nil {
		return out, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO supervisor.knowledge_entity_revisions(tenant_id,site_id,id,revision,entity_type,payload,source_id,source_revision,actor_issuer,actor_subject) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, sc.TenantID, sc.SiteID, in.EntityID, revision, in.Type, payload, in.SourceID, in.SourceRevision, a.Issuer, a.Subject)
	if err != nil {
		return out, err
	}
	if err = saveReceipt(ctx, tx, a, sc, key, digest, "entity", in.EntityID, revision); err != nil {
		return out, err
	}
	event := "entity.created"
	if exists {
		event = "entity.revised"
	}
	if err = audit(ctx, tx, a, sc, in.EntityID, revision, event); err != nil {
		return out, err
	}
	out, err = loadEntity(ctx, tx, sc, in.EntityID, revision)
	if err != nil {
		return out, err
	}
	out.SourcesCurrent = true
	return out, nil
}
func (s *Service) ReadEntity(ctx context.Context, a Actor, sc Scope, id string, revision int64) (Entity, error) {
	if !contractUUID(id) || revision < 0 {
		return Entity{}, ErrInvalidContract
	}
	tx, err := s.begin(ctx, a, sc, false)
	if err != nil {
		return Entity{}, err
	}
	defer tx.Rollback()
	out, err := loadEntity(ctx, tx, sc, id, revision)
	if err != nil {
		return Entity{}, err
	}
	refs := []sourceRef{{id: out.SourceID, revision: out.SourceRevision}}
	states, err := lockSources(ctx, tx, sc, refs)
	if err != nil {
		return Entity{}, err
	}
	out.SourcesCurrent, err = eligible(refs, states, false)
	if err != nil {
		return Entity{}, err
	}
	heads, err := lockEntities(ctx, tx, sc, []string{id})
	if err != nil {
		return Entity{}, err
	}
	out.CurrentRevision = heads[id]
	out.SourcesCurrent = out.SourcesCurrent && out.Revision == out.CurrentRevision
	if err = audit(ctx, tx, a, sc, id, out.Revision, "entity.read"); err != nil {
		return Entity{}, err
	}
	return out, s.finish(ctx, tx, a)
}
func head(ctx context.Context, tx *sql.Tx, sc Scope, id string, lock bool) (int64, error) {
	var revision int64
	query := `SELECT current_revision FROM supervisor.knowledge_claim_heads WHERE tenant_id=$1 AND site_id=$2 AND id=$3`
	if lock {
		query += " FOR SHARE"
	}
	err := tx.QueryRowContext(ctx, query, sc.TenantID, sc.SiteID, id).Scan(&revision)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return revision, err
}
func loadClaim(ctx context.Context, tx *sql.Tx, sc Scope, id string, revision int64) (Claim, error) {
	var out Claim
	var payload []byte
	if revision == 0 {
		var err error
		revision, err = head(ctx, tx, sc, id, false)
		if err != nil {
			return out, err
		}
	}
	var subjectRevision int64
	var objectRevision sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT r.payload,r.revision,r.state,r.created_at,r.actor_issuer,r.actor_subject,p.subject_revision,p.object_revision FROM supervisor.knowledge_claim_revisions r JOIN supervisor.knowledge_claim_entity_refs p ON (p.tenant_id,p.site_id,p.claim_id,p.claim_revision)=(r.tenant_id,r.site_id,r.claim_id,r.revision) WHERE r.tenant_id=$1 AND r.site_id=$2 AND r.claim_id=$3 AND r.revision=$4`, sc.TenantID, sc.SiteID, id, revision).Scan(&payload, &out.Revision, &out.State, &out.CreatedAt, &out.ActorIssuer, &out.ActorSubject, &subjectRevision, &objectRevision)
	if errors.Is(err, sql.ErrNoRows) {
		return out, ErrNotFound
	}
	if err != nil {
		return out, err
	}
	err = json.Unmarshal(payload, &out.ClaimInput)
	if err == nil {
		out.SubjectRevision = subjectRevision
		out.ObjectRevision = objectRevision.Int64
	}
	return out, err
}

// Legacy receipts fingerprinted payloads without entity pins. Only the exact
// migrated revision may use that fingerprint, never a new pinned receipt.
func replayClaim(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, in ClaimInput, key, digest string, expected int64) (int64, bool, error) {
	prior, ok, err := replay(ctx, tx, a, sc, key, digest)
	if !errors.Is(err, ErrConflict) || in.SubjectRevision != 1 || (in.ObjectEntityID != "" && in.ObjectRevision != 1) {
		return prior, ok, err
	}
	var legacy bool
	err = tx.QueryRowContext(ctx, `SELECT NOT (r.payload ? 'subject_revision') AND NOT (r.payload ? 'object_revision') FROM supervisor.knowledge_receipts x JOIN supervisor.knowledge_claim_revisions r ON (r.tenant_id,r.site_id,r.claim_id,r.revision)=(x.tenant_id,x.site_id,x.target_id,x.revision) WHERE x.tenant_id=$1 AND x.site_id=$2 AND x.actor_issuer=$3 AND x.actor_subject=$4 AND x.idempotency_key=$5 AND x.operation='claim' AND x.target_id=$6`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, key, in.ClaimID).Scan(&legacy)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !legacy) {
		return 0, false, ErrConflict
	}
	if err != nil {
		return 0, false, err
	}
	in.SubjectRevision, in.ObjectRevision = 0, 0
	return replay(ctx, tx, a, sc, key, fingerprint("claim", in, expected))
}
func (s *Service) CreateClaim(ctx context.Context, a Actor, sc Scope, in ClaimInput, key string, expected int64) (Claim, error) {
	tx, err := s.begin(ctx, a, sc, true)
	if err != nil {
		return Claim{}, err
	}
	defer tx.Rollback()
	out, err := s.createClaimTx(ctx, tx, a, sc, in, key, expected)
	if err != nil {
		return Claim{}, err
	}
	return out, s.finish(ctx, tx, a)
}

// CreateClaimTx creates a candidate claim inside the caller's already scoped transaction.
// It is reserved for the server agent identity; approval remains a separate human operation.
func (s *Service) CreateClaimTx(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, in ClaimInput, key string, expected int64) (Claim, error) {
	if tx == nil || a.Issuer != "supervisor:agent" || a.SessionHash != "" || !contractUUID(a.Subject) {
		return Claim{}, ErrDenied
	}
	return s.createClaimTx(ctx, tx, a, sc, in, key, expected)
}

func (s *Service) createClaimTx(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, in ClaimInput, key string, expected int64) (Claim, error) {
	var out Claim
	if err := in.Validate(); err != nil {
		return out, err
	}
	if err := in.ValidateEntityRevisions(); err != nil {
		return out, err
	}
	if !mutation(key, expected) || !contractUUID(sc.TenantID) || !contractUUID(sc.SiteID) {
		return out, ErrInvalidContract
	}
	refs, subject, object, err := claimRefs(ctx, tx, sc, in)
	if err != nil {
		return out, err
	}
	if in.ObjectEntityID != "" {
		err = ValidateEntityPair(in.Predicate, subject.Type, object.Type)
	} else {
		err = ValidateLiteralSubject(in.Predicate, subject.Type)
	}
	if err != nil {
		return out, err
	}
	if in.Predicate == "part_of_asset" && (subject.AssetKind != "machine" || object.AssetKind != "line") {
		return out, ErrInvalidContract
	}
	states, err := lockSources(ctx, tx, sc, refs)
	if err != nil {
		return out, err
	}
	current, err := eligible(refs, states, false)
	if err != nil {
		return out, err
	}
	entityHeads, err := lockEntities(ctx, tx, sc, []string{in.SubjectID, in.ObjectEntityID})
	if err != nil {
		return out, err
	}
	current = current && entityPinsCurrent(in, entityHeads)
	revision, err := head(ctx, tx, sc, in.ClaimID, true)
	exists := err == nil
	if err != nil && !errors.Is(err, ErrNotFound) {
		return out, err
	}
	digest := fingerprint("claim", in, expected)
	prior, ok, err := replayClaim(ctx, tx, a, sc, in, key, digest, expected)
	if err != nil {
		return out, err
	}
	if ok {
		out, err = loadClaim(ctx, tx, sc, in.ClaimID, prior)
		if err != nil {
			return out, err
		}
		out.CurrentRevision = revision
		out.SourcesCurrent = current
		if err = audit(ctx, tx, a, sc, in.ClaimID, prior, "mutation.replayed"); err != nil {
			return out, err
		}
		return out, nil
	}
	if !current {
		return out, ErrSourceChanged
	}
	if (!exists && expected != 0) || (exists && revision != expected) {
		return out, ErrRevision
	}
	// A merged entity takes no new claim pins; the share locks above serialize with MergeEntity.
	for _, id := range []string{in.SubjectID, in.ObjectEntityID} {
		if id == "" {
			continue
		}
		if merged, e := redirected(ctx, tx, sc, id); e != nil || merged {
			if e != nil {
				return out, e
			}
			return out, ErrEntityRedirected
		}
	}
	if err = cycle(ctx, tx, sc, in); err != nil {
		return out, err
	}
	revision++
	if exists {
		_, err = tx.ExecContext(ctx, `UPDATE supervisor.knowledge_claim_heads SET current_revision=current_revision+1 WHERE tenant_id=$1 AND site_id=$2 AND id=$3`, sc.TenantID, sc.SiteID, in.ClaimID)
	} else {
		_, err = tx.ExecContext(ctx, `INSERT INTO supervisor.knowledge_claim_heads(tenant_id,site_id,id,current_revision) VALUES($1,$2,$3,1)`, sc.TenantID, sc.SiteID, in.ClaimID)
	}
	if err != nil {
		return out, err
	}
	payload, _ := json.Marshal(in)
	var objectID any
	if in.ObjectEntityID != "" {
		objectID = in.ObjectEntityID
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO supervisor.knowledge_claim_revisions(tenant_id,site_id,claim_id,revision,subject_id,predicate,object_entity_id,payload,actor_issuer,actor_subject) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, sc.TenantID, sc.SiteID, in.ClaimID, revision, in.SubjectID, in.Predicate, objectID, payload, a.Issuer, a.Subject)
	if err != nil {
		return out, err
	}
	var objectRevision any
	if in.ObjectEntityID != "" {
		objectRevision = in.ObjectRevision
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO supervisor.knowledge_claim_entity_refs(tenant_id,site_id,claim_id,claim_revision,subject_id,subject_revision,object_entity_id,object_revision) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, sc.TenantID, sc.SiteID, in.ClaimID, revision, in.SubjectID, in.SubjectRevision, objectID, objectRevision)
	if err != nil {
		return out, err
	}
	for _, ref := range refs {
		if ref.spanID == "" {
			continue
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO supervisor.knowledge_claim_spans(tenant_id,site_id,claim_id,claim_revision,span_id,source_id,source_revision) VALUES($1,$2,$3,$4,$5,$6,$7)`, sc.TenantID, sc.SiteID, in.ClaimID, revision, ref.spanID, ref.id, ref.revision)
		if err != nil {
			return out, err
		}
	}
	if err = saveReceipt(ctx, tx, a, sc, key, digest, "claim", in.ClaimID, revision); err != nil {
		return out, err
	}
	if err = recordConflicts(ctx, tx, a, sc, in, revision); err != nil {
		return out, err
	}
	event := "claim.created"
	if exists {
		event = "claim.revised"
	}
	if err = audit(ctx, tx, a, sc, in.ClaimID, revision, event); err != nil {
		return out, err
	}
	out, err = loadClaim(ctx, tx, sc, in.ClaimID, revision)
	if err != nil {
		return out, err
	}
	out.CurrentRevision = revision
	out.SourcesCurrent = true
	return out, nil
}
func (s *Service) ReadClaim(ctx context.Context, a Actor, sc Scope, id string, revision int64) (Claim, error) {
	var out Claim
	if !contractUUID(id) || revision < 0 {
		return out, ErrInvalidContract
	}
	tx, err := s.begin(ctx, a, sc, false)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	out, err = loadClaim(ctx, tx, sc, id, revision)
	if err != nil {
		return out, err
	}
	refs, _, _, err := claimRefs(ctx, tx, sc, out.ClaimInput)
	if err != nil {
		return Claim{}, err
	}
	states, err := lockSources(ctx, tx, sc, refs)
	if err != nil {
		return Claim{}, err
	}
	out.SourcesCurrent, err = eligible(refs, states, false)
	if err != nil {
		return Claim{}, err
	}
	entityHeads, err := lockEntities(ctx, tx, sc, []string{out.SubjectID, out.ObjectEntityID})
	if err != nil {
		return Claim{}, err
	}
	out.SourcesCurrent = out.SourcesCurrent && entityPinsCurrent(out.ClaimInput, entityHeads)
	out.CurrentRevision, err = head(ctx, tx, sc, id, true)
	if err != nil {
		return Claim{}, err
	}
	if out.Corroboration, err = claimCorroboration(ctx, tx, sc, id, out.Revision); err != nil {
		return Claim{}, err
	}
	if err = audit(ctx, tx, a, sc, id, out.Revision, "claim.read"); err != nil {
		return Claim{}, err
	}
	return out, s.finish(ctx, tx, a)
}
func pageArgs(limit int, cursor string) bool {
	return limit >= 1 && limit <= 100 && (cursor == "" || contractUUID(cursor))
}

// SQL prefilter avoids returning pagination identifiers for already revoked provenance.
const liveEntity = `NOT EXISTS(SELECT 1 FROM supervisor.evidence_heads eh WHERE eh.tenant_id=e.tenant_id AND eh.site_id=e.site_id AND eh.source_id=e.source_id AND (eh.revoked OR EXISTS(SELECT 1 FROM supervisor.evidence_revocations rv WHERE (rv.tenant_id,rv.site_id,rv.source_id)=(eh.tenant_id,eh.site_id,eh.source_id))))`

var errPageChanged = errors.New("candidate provenance changed during pagination")

func (s *Service) ListEntities(ctx context.Context, a Actor, sc Scope, limit int, cursor string) (EntityPage, error) {
	for range 3 {
		page, err := s.listEntities(ctx, a, sc, limit, cursor)
		if !errors.Is(err, errPageChanged) {
			return page, err
		}
	}
	return EntityPage{}, ErrSourceChanged
}
func (s *Service) listEntities(ctx context.Context, a Actor, sc Scope, limit int, cursor string) (EntityPage, error) {
	out := EntityPage{Entities: []Entity{}}
	if !pageArgs(limit, cursor) {
		return out, ErrInvalidContract
	}
	if cursor == "" {
		cursor = uuid.Nil.String()
	}
	tx, err := s.begin(ctx, a, sc, false)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT e.id FROM supervisor.knowledge_entity_heads h JOIN supervisor.knowledge_entity_revisions e ON (e.tenant_id,e.site_id,e.id,e.revision)=(h.tenant_id,h.site_id,h.id,h.current_revision) WHERE e.tenant_id=$1 AND e.site_id=$2 AND e.id>$3 AND `+liveEntity+` ORDER BY e.id LIMIT $4`, sc.TenantID, sc.SiteID, cursor, limit+1)
	if err != nil {
		return out, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return out, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	items := []Entity{}
	refs := []sourceRef{}
	for _, id := range ids {
		e, err := loadEntity(ctx, tx, sc, id, 0)
		if err != nil {
			return out, err
		}
		items = append(items, e)
		refs = append(refs, sourceRef{id: e.SourceID, revision: e.SourceRevision})
	}
	states, err := lockSources(ctx, tx, sc, refs)
	if err != nil {
		return out, err
	}
	heads, err := lockEntities(ctx, tx, sc, ids)
	if err != nil {
		return out, err
	}
	for _, e := range items {
		e.SourcesCurrent, err = eligible([]sourceRef{{id: e.SourceID, revision: e.SourceRevision}}, states, false)
		if errors.Is(err, ErrNotFound) {
			return out, errPageChanged
		}
		if err != nil {
			return out, err
		}
		if heads[e.EntityID] != e.Revision {
			return out, errPageChanged
		}
		e.CurrentRevision = heads[e.EntityID]
		out.Entities = append(out.Entities, e)
	}
	if len(out.Entities) > limit {
		out.Entities = out.Entities[:limit]
		out.NextCursor = out.Entities[limit-1].EntityID
	}
	if err = audit(ctx, tx, a, sc, nil, nil, "entity.listed"); err != nil {
		return out, err
	}
	return out, s.finish(ctx, tx, a)
}
func (s *Service) ListClaims(ctx context.Context, a Actor, sc Scope, limit int, cursor, subjectID string) (ClaimPage, error) {
	return s.ListClaimsByState(ctx, a, sc, limit, cursor, subjectID, "")
}

// Claim review states (WIS-270 state reducer). A listed state is derived from the immutable
// review, conflict and resolution ledgers for the claim's exact current revision.
const (
	StateCandidate = "candidate"
	StateApproved  = "approved"
	StateRejected  = "rejected"
	StateContested = "contested"
)

// ListClaimsByState filters the current claim heads by the review state of their exact current
// revision: candidate, approved, rejected or contested.
func (s *Service) ListClaimsByState(ctx context.Context, a Actor, sc Scope, limit int, cursor, subjectID, state string) (ClaimPage, error) {
	switch state {
	case "", StateCandidate, StateApproved, StateRejected, StateContested:
	default:
		return ClaimPage{Claims: []ClaimSummary{}}, ErrInvalidContract
	}
	for range 3 {
		page, err := s.listClaims(ctx, a, sc, limit, cursor, subjectID, state)
		if !errors.Is(err, errPageChanged) {
			return page, err
		}
	}
	return ClaimPage{}, ErrSourceChanged
}

// claimReviewState derives the state of the head's current revision, following the WIS-270
// reducer ("candidate -> approved/rejected; any candidate/approved may become contested;
// contested -> approved/rejected only by explicit conflict resolution"):
//  1. an explicit rejection of this revision is rejected;
//  2. otherwise a revision that is a side of a recorded conflict with no standing explicit
//     resolution (none yet, or the current one is a withdrawal) is contested, even if it was
//     approved: an approval never outranks an open contradiction;
//  3. otherwise an unrevoked approval is approved, and anything else is a candidate. An
//     approval with a recorded withdrawal is a candidate again, never approved.
const claimReviewState = `(CASE
 WHEN EXISTS(SELECT 1 FROM supervisor.knowledge_claim_reviews v WHERE (v.tenant_id,v.site_id,v.claim_id,v.claim_revision)=(h.tenant_id,h.site_id,h.id,h.current_revision) AND v.verdict='reject') THEN 'rejected'
 WHEN EXISTS(SELECT 1 FROM supervisor.knowledge_conflicts c
   WHERE (c.tenant_id,c.site_id)=(h.tenant_id,h.site_id)
     AND ((c.left_claim_id,c.left_revision)=(h.id,h.current_revision) OR (c.right_claim_id,c.right_revision)=(h.id,h.current_revision))
     AND NOT EXISTS(SELECT 1 FROM supervisor.knowledge_resolution_heads rh
       JOIN supervisor.knowledge_resolution_decisions d ON (d.tenant_id,d.site_id,d.conflict_id,d.revision)=(rh.tenant_id,rh.site_id,rh.conflict_id,rh.current_revision)
       WHERE (rh.tenant_id,rh.site_id,rh.conflict_id)=(c.tenant_id,c.site_id,c.id) AND d.decision<>'withdraw')) THEN 'contested'
 WHEN EXISTS(SELECT 1 FROM supervisor.knowledge_claim_reviews v WHERE (v.tenant_id,v.site_id,v.claim_id,v.claim_revision)=(h.tenant_id,h.site_id,h.id,h.current_revision) AND v.verdict='approve' AND NOT EXISTS(SELECT 1 FROM supervisor.knowledge_review_revocations x WHERE (x.tenant_id,x.site_id,x.review_id)=(v.tenant_id,v.site_id,v.review_id))) THEN 'approved'
 ELSE 'candidate' END)`

func (s *Service) listClaims(ctx context.Context, a Actor, sc Scope, limit int, cursor, subjectID, state string) (ClaimPage, error) {
	out := ClaimPage{Claims: []ClaimSummary{}}
	if !pageArgs(limit, cursor) || (subjectID != "" && !contractUUID(subjectID)) {
		return out, ErrInvalidContract
	}
	if cursor == "" {
		cursor = uuid.Nil.String()
	}
	tx, err := s.begin(ctx, a, sc, false)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	query := `SELECT h.id,h.current_revision,` + claimReviewState + ` FROM supervisor.knowledge_claim_heads h JOIN supervisor.knowledge_claim_revisions r ON (r.tenant_id,r.site_id,r.claim_id,r.revision)=(h.tenant_id,h.site_id,h.id,h.current_revision) WHERE h.tenant_id=$1 AND h.site_id=$2 AND h.id>$3 AND ($5='' OR r.subject_id::text=$5) AND ($6='' OR ` + claimReviewState + `=$6)
 AND NOT EXISTS(SELECT 1 FROM supervisor.knowledge_claim_entity_refs p JOIN supervisor.knowledge_entity_revisions e ON e.tenant_id=p.tenant_id AND e.site_id=p.site_id AND ((e.id=p.subject_id AND e.revision=p.subject_revision) OR (e.id=p.object_entity_id AND e.revision=p.object_revision)) WHERE (p.tenant_id,p.site_id,p.claim_id,p.claim_revision)=(r.tenant_id,r.site_id,r.claim_id,r.revision) AND NOT (` + liveEntity + `))
 AND NOT EXISTS(SELECT 1 FROM supervisor.knowledge_claim_spans c JOIN supervisor.evidence_heads eh ON (eh.tenant_id,eh.site_id,eh.source_id)=(c.tenant_id,c.site_id,c.source_id) WHERE (c.tenant_id,c.site_id,c.claim_id,c.claim_revision)=(r.tenant_id,r.site_id,r.claim_id,r.revision) AND (eh.revoked OR EXISTS(SELECT 1 FROM supervisor.evidence_revocations rv WHERE (rv.tenant_id,rv.site_id,rv.source_id)=(eh.tenant_id,eh.site_id,eh.source_id)))) ORDER BY h.id LIMIT $4`
	rows, err := tx.QueryContext(ctx, query, sc.TenantID, sc.SiteID, cursor, limit+1, subjectID, state)
	if err != nil {
		return out, err
	}
	type selected struct {
		id    string
		rev   int64
		state string
	}
	ids := []selected{}
	for rows.Next() {
		var item selected
		if err = rows.Scan(&item.id, &item.rev, &item.state); err != nil {
			rows.Close()
			return out, err
		}
		ids = append(ids, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	claims := []Claim{}
	allRefs := []sourceRef{}
	references := [][]sourceRef{}
	for _, id := range ids {
		claim, err := loadClaim(ctx, tx, sc, id.id, id.rev)
		if err != nil {
			return out, err
		}
		refs, _, _, err := claimRefs(ctx, tx, sc, claim.ClaimInput)
		if err != nil {
			return out, err
		}
		claims = append(claims, claim)
		allRefs = append(allRefs, refs...)
		references = append(references, refs)
	}
	states, err := lockSources(ctx, tx, sc, allRefs)
	if err != nil {
		return out, err
	}
	entityIDs := []string{}
	for _, c := range claims {
		entityIDs = append(entityIDs, c.SubjectID, c.ObjectEntityID)
	}
	entityHeads, err := lockEntities(ctx, tx, sc, entityIDs)
	if err != nil {
		return out, err
	}
	for i, c := range claims {
		current, err := eligible(references[i], states, false)
		if errors.Is(err, ErrNotFound) {
			return out, errPageChanged
		}
		if err != nil {
			return out, err
		}
		latest, err := head(ctx, tx, sc, c.ClaimID, true)
		if err != nil {
			return out, err
		}
		corroboration, err := claimCorroboration(ctx, tx, sc, c.ClaimID, c.Revision)
		if err != nil {
			return out, err
		}
		out.Claims = append(out.Claims, ClaimSummary{c.ClaimID, c.Revision, latest, c.SubjectID, c.Predicate, c.ObjectEntityID, c.State, ids[i].state, current && entityPinsCurrent(c.ClaimInput, entityHeads), corroboration})
	}
	if len(out.Claims) > limit {
		out.Claims = out.Claims[:limit]
		out.NextCursor = out.Claims[limit-1].ClaimID
	}
	if err = audit(ctx, tx, a, sc, nil, nil, "claim.listed"); err != nil {
		return out, err
	}
	return out, s.finish(ctx, tx, a)
}
