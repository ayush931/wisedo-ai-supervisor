// Package plans owns authoritative production-plan bootstrap: typed plan revisions
// captured with source-span provenance, and PlanApprovalV1 supervisor decisions over
// an exact canonical payload digest. Source credentials can never approve a plan.
package plans

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/production"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/session"
	"github.com/google/uuid"
)

var (
	ErrInvalid          = errors.New("invalid plan contract")
	ErrNotFound         = errors.New("plan revision unavailable")
	ErrDenied           = errors.New("plan permission denied")
	ErrConflict         = errors.New("plan idempotency conflict")
	ErrRevision         = errors.New("plan review revision changed")
	ErrReauthentication = errors.New("recent authentication required")
	ErrPlanConflict     = errors.New("plan revision already captured with different content")
	ErrPlanGap          = errors.New("plan revision is not the next contiguous revision")
	ErrSelfReview       = errors.New("plan reviewer must differ from submitter")
	ErrUnknownEntity    = errors.New("plan order or asset is not a known entity")
	ErrUnknownSpan      = errors.New("plan source span unavailable")
	ErrSpanUnavailable  = errors.New("plan source span revoked or superseded")
	ErrSuperseded       = errors.New("plan revision superseded by a later revision")
	// errIntegrity marks stored state that fails re-verification; it is never a client error.
	errIntegrity = errors.New("plan integrity check failed")
)

const (
	// PolicyRevision versions the plan review policy: supervisor-only decisions by a
	// recently authenticated human distinct from the submitter, over current provenance.
	PolicyRevision = 1
	MaxSpans       = 100
)

type Actor struct{ Issuer, Subject, SessionHash string }
type Scope struct{ TenantID, SiteID string }

// CaptureInput is the POST /plan-revisions body: the exact typed business payload
// (approval_ref excluded) and the source spans it was read from.
type CaptureInput struct {
	Plan          production.PlanBody `json:"plan"`
	SourceSpanIDs []string            `json:"source_span_ids"`
}

// ReviewInput is the POST /plan-revisions/{id}/reviews body. The expected review
// revision travels in If-Match.
type ReviewInput struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
}

type ActorRef struct {
	Issuer  string `json:"issuer"`
	Subject string `json:"subject"`
}

// Approval is PlanApprovalV1. Its ID is the approval_ref intake events must cite.
type Approval struct {
	ID                     string    `json:"id"`
	PlanRevisionID         string    `json:"plan_revision_id"`
	ReviewRevision         int64     `json:"review_revision"`
	CanonicalPayloadSHA256 string    `json:"canonical_payload_digest"`
	Decision               string    `json:"decision"`
	Reason                 string    `json:"reason"`
	Reviewer               ActorRef  `json:"reviewer_actor_id"`
	PolicyRevision         int64     `json:"policy_revision"`
	SourceSpanIDs          []string  `json:"source_span_ids"`
	AuthenticatedAt        time.Time `json:"authenticated_at"`
	ReviewedAt             time.Time `json:"reviewed_at"`
	Replayed               bool      `json:"replayed"`
}

type Revision struct {
	ID                     string              `json:"id"`
	Plan                   production.PlanBody `json:"plan"`
	CanonicalPayloadSHA256 string              `json:"canonical_payload_digest"`
	DigestSchemaVersion    int                 `json:"digest_schema_version"`
	SourceSpanIDs          []string            `json:"source_span_ids"`
	SubmittedBy            ActorRef            `json:"submitted_by"`
	CreatedAt              time.Time           `json:"created_at"`
	ReviewRevision         int64               `json:"review_revision"`
	CurrentDecision        *Approval           `json:"current_decision"`
	Decisions              []Approval          `json:"decisions"`
	// Superseded is true once a later revision exists for the same order and asset.
	Superseded bool `json:"superseded"`
	// Approved is true only while the current decision approves, no later revision
	// supersedes this one and every cited source span is still unrevoked. It is
	// evaluated on every read with the same rules intake applies.
	Approved bool `json:"approved"`
	Replayed bool `json:"replayed"`
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

func validID(v string) bool {
	id, e := uuid.Parse(v)
	return e == nil && id != uuid.Nil && id.String() == v
}
func validText(v string, max int) bool {
	return len(v) > 0 && len(v) <= max && strings.TrimSpace(v) == v && !strings.ContainsRune(v, 0)
}
func digest(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// canonicalSpans requires 1..MaxSpans distinct canonical IDs and returns them sorted.
func canonicalSpans(in []string) ([]string, error) {
	if len(in) == 0 || len(in) > MaxSpans {
		return nil, ErrInvalid
	}
	out := append([]string(nil), in...)
	sort.Strings(out)
	for i, id := range out {
		if !validID(id) || (i > 0 && out[i-1] == id) {
			return nil, ErrInvalid
		}
	}
	return out, nil
}

type access int

const (
	read access = iota
	capture
	review
)

func (s *Service) begin(ctx context.Context, a Actor, sc Scope, mode access) (*sql.Tx, time.Time, time.Time, error) {
	if !validID(sc.TenantID) || !validID(sc.SiteID) || !validText(a.Issuer, 255) || !validText(a.Subject, 255) || !validText(a.SessionHash, 128) {
		return nil, time.Time{}, time.Time{}, ErrInvalid
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return nil, time.Time{}, time.Time{}, e
	}
	fail := func(err error) (*sql.Tx, time.Time, time.Time, error) {
		_ = tx.Rollback()
		return nil, time.Time{}, time.Time{}, err
	}
	if _, e = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true),set_config('statement_timeout','10s',true),set_config('lock_timeout','5s',true)`, sc.TenantID); e != nil {
		return fail(e)
	}
	var issuer, subject string
	var expiry, seen, auth, now time.Time
	e = tx.QueryRowContext(ctx, `SELECT issuer,subject,expires_at,last_seen_at,auth_time,clock_timestamp() FROM supervisor.browser_sessions WHERE token_hash=$1 FOR SHARE`, a.SessionHash).Scan(&issuer, &subject, &expiry, &seen, &auth, &now)
	if e != nil || issuer != a.Issuer || subject != a.Subject || !expiry.After(now) || !seen.After(now.Add(-s.sessions.Idle)) {
		return fail(ErrDenied)
	}
	var found bool
	if e = tx.QueryRowContext(ctx, `SELECT supervisor.lock_member_site($1,$2,$3,$4,$5)`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, mode != read).Scan(&found); e != nil {
		return fail(e)
	}
	if !found {
		return fail(ErrNotFound)
	}
	var role string
	e = tx.QueryRowContext(ctx, `SELECT role FROM supervisor.memberships WHERE tenant_id=$1 AND site_id=$2 AND issuer=$3 AND subject=$4 AND active FOR SHARE`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject).Scan(&role)
	if errors.Is(e, sql.ErrNoRows) {
		return fail(ErrNotFound)
	}
	if e != nil {
		return fail(e)
	}
	switch mode {
	case capture:
		if role != "owner" && role != "supervisor" {
			return fail(ErrDenied)
		}
	case review:
		// Plan approval is supervisor-only; owner is not an implicit bypass.
		if role != "supervisor" {
			return fail(ErrDenied)
		}
		if auth.After(now) || auth.Before(now.Add(-s.sessions.RecentAuth)) {
			return fail(ErrReauthentication)
		}
	}
	return tx, auth, now, nil
}

func (s *Service) finish(ctx context.Context, tx *sql.Tx, a Actor) error {
	var ok bool
	if e := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM supervisor.browser_sessions WHERE token_hash=$1 AND issuer=$2 AND subject=$3 AND expires_at>clock_timestamp() AND last_seen_at>clock_timestamp()-make_interval(secs=>$4))`, a.SessionHash, a.Issuer, a.Subject, s.sessions.IdleSeconds()).Scan(&ok); e != nil {
		return e
	}
	if !ok {
		return ErrDenied
	}
	return tx.Commit()
}

func audit(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, planID, approvalID, event string) error {
	_, e := tx.ExecContext(ctx, `INSERT INTO supervisor.plan_audit(tenant_id,site_id,actor_issuer,actor_subject,plan_revision_id,approval_id,event_type) VALUES($1,$2,$3,$4,$5,NULLIF($6,'')::uuid,$7)`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, planID, approvalID, event)
	return e
}

// receipt returns the stored result IDs for an idempotency key, or ErrConflict when
// the same key was used for a different request.
func receipt(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, key, hash, op string) (string, string, bool, error) {
	var stored, storedOp, planID string
	var approvalID sql.NullString
	e := tx.QueryRowContext(ctx, `SELECT request_sha256,operation,plan_revision_id,approval_id FROM supervisor.plan_receipts WHERE tenant_id=$1 AND site_id=$2 AND actor_issuer=$3 AND actor_subject=$4 AND idempotency_key=$5`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, key).Scan(&stored, &storedOp, &planID, &approvalID)
	if errors.Is(e, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if e != nil {
		return "", "", false, e
	}
	if stored != hash || storedOp != op {
		return "", "", false, ErrConflict
	}
	if e = audit(ctx, tx, a, sc, planID, approvalID.String, "mutation.replayed"); e != nil {
		return "", "", false, e
	}
	return planID, approvalID.String, true, nil
}

func saveReceipt(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, key, hash, op, planID, approvalID string) error {
	_, e := tx.ExecContext(ctx, `INSERT INTO supervisor.plan_receipts(tenant_id,site_id,actor_issuer,actor_subject,idempotency_key,request_sha256,operation,plan_revision_id,approval_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,NULLIF($9,'')::uuid)`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, key, hash, op, planID, approvalID)
	return e
}

// checkSpans verifies every span exists in scope and its evidence source is unrevoked.
// Capture and approval also require requireCurrent: the span must cite the source's
// current revision. Once approved, only revocation withdraws eligibility — exactly the
// rule intake applies. Evidence heads are share-locked so a concurrent revocation
// serializes against this decision.
func checkSpans(ctx context.Context, tx *sql.Tx, sc Scope, spans []string, requireCurrent bool) error {
	for _, id := range spans {
		var revision, current int64
		var revoked bool
		e := tx.QueryRowContext(ctx, `SELECT s.revision,h.current_revision,(h.revoked OR EXISTS(SELECT 1 FROM supervisor.evidence_revocations v WHERE (v.tenant_id,v.site_id,v.source_id)=(h.tenant_id,h.site_id,h.source_id))) FROM supervisor.evidence_spans s JOIN supervisor.evidence_heads h ON (h.tenant_id,h.site_id,h.source_id)=(s.tenant_id,s.site_id,s.source_id) WHERE s.tenant_id=$1 AND s.site_id=$2 AND s.id=$3 FOR SHARE OF h`, sc.TenantID, sc.SiteID, id).Scan(&revision, &current, &revoked)
		if errors.Is(e, sql.ErrNoRows) {
			return ErrUnknownSpan
		}
		if e != nil {
			return e
		}
		if revoked || (requireCurrent && revision != current) {
			return ErrSpanUnavailable
		}
	}
	return nil
}

// Capture records an immutable plan revision. Re-capturing an identical revision is
// idempotent; any different content for the same order/asset/plan_revision conflicts.
func (s *Service) Capture(ctx context.Context, a Actor, sc Scope, in CaptureInput, key string) (Revision, error) {
	var out Revision
	if !validText(key, 128) || in.Plan.Validate() != nil {
		return out, ErrInvalid
	}
	spans, err := canonicalSpans(in.SourceSpanIDs)
	if err != nil {
		return out, err
	}
	canonical, payloadDigest, err := production.CanonicalPlanPayload(sc.TenantID, sc.SiteID, in.Plan)
	if err != nil {
		return out, ErrInvalid
	}
	payload, err := json.Marshal(in.Plan)
	if err != nil {
		return out, ErrInvalid
	}
	requestHash := digest(struct {
		PayloadDigest string
		Spans         []string
	}{payloadDigest, spans})
	tx, _, _, err := s.begin(ctx, a, sc, capture)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	planID, _, replayed, err := receipt(ctx, tx, a, sc, key, requestHash, "plan")
	if err != nil {
		return out, err
	}
	if replayed {
		if out, err = load(ctx, tx, sc, planID); err != nil {
			return out, err
		}
		out.Replayed = true
		return out, s.finish(ctx, tx, a)
	}
	var existingID, existingDigest string
	e := tx.QueryRowContext(ctx, `SELECT id,canonical_payload_sha256 FROM supervisor.plan_revisions WHERE tenant_id=$1 AND site_id=$2 AND order_id=$3 AND asset_id=$4 AND plan_revision=$5`, sc.TenantID, sc.SiteID, in.Plan.OrderID, in.Plan.AssetID, in.Plan.PlanRevision).Scan(&existingID, &existingDigest)
	if e == nil {
		existing, err := load(ctx, tx, sc, existingID)
		if err != nil {
			return out, err
		}
		// Identical recapture is idempotent only for the original submitter; anyone else
		// would otherwise become an unrecorded co-submitter able to approve it.
		if existingDigest != payloadDigest || strings.Join(existing.SourceSpanIDs, ",") != strings.Join(spans, ",") ||
			existing.SubmittedBy.Issuer != a.Issuer || existing.SubmittedBy.Subject != a.Subject {
			return out, ErrPlanConflict
		}
		if err = audit(ctx, tx, a, sc, existingID, "", "mutation.replayed"); err != nil {
			return out, err
		}
		if err = saveReceipt(ctx, tx, a, sc, key, requestHash, "plan", existingID, ""); err != nil {
			return out, err
		}
		existing.Replayed = true
		return existing, s.finish(ctx, tx, a)
	}
	if !errors.Is(e, sql.ErrNoRows) {
		return out, e
	}
	if in.Plan.PlanRevision > 1 {
		var previous bool
		if e = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM supervisor.plan_revisions WHERE tenant_id=$1 AND site_id=$2 AND order_id=$3 AND asset_id=$4 AND plan_revision=$5)`, sc.TenantID, sc.SiteID, in.Plan.OrderID, in.Plan.AssetID, in.Plan.PlanRevision-1).Scan(&previous); e != nil {
			return out, e
		}
		if !previous {
			return out, ErrPlanGap
		}
	}
	// A slice, not a map: an order and asset given the same ID must check both types.
	for _, check := range [][2]string{{in.Plan.OrderID, "order"}, {in.Plan.AssetID, "asset"}} {
		id, want := check[0], check[1]
		var kind string
		e = tx.QueryRowContext(ctx, `SELECT e.entity_type FROM supervisor.knowledge_entities e JOIN supervisor.knowledge_entity_heads h ON (h.tenant_id,h.site_id,h.id)=(e.tenant_id,e.site_id,e.id) WHERE e.tenant_id=$1 AND e.site_id=$2 AND e.id=$3`, sc.TenantID, sc.SiteID, id).Scan(&kind)
		if errors.Is(e, sql.ErrNoRows) || (e == nil && kind != want) {
			return out, ErrUnknownEntity
		}
		if e != nil {
			return out, e
		}
	}
	if err = checkSpans(ctx, tx, sc, spans, true); err != nil {
		return out, err
	}
	out.ID = uuid.NewString()
	if _, e = tx.ExecContext(ctx, `INSERT INTO supervisor.plan_revisions(tenant_id,site_id,id,order_id,asset_id,plan_revision,window_start,window_end,payload,canonical_payload,canonical_payload_sha256,digest_schema_version,actor_issuer,actor_subject) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9::jsonb,$10,$11,$12,$13,$14)`, sc.TenantID, sc.SiteID, out.ID, in.Plan.OrderID, in.Plan.AssetID, in.Plan.PlanRevision, in.Plan.WindowStart, in.Plan.WindowEnd, string(payload), string(canonical), payloadDigest, production.PlanDigestSchemaVersion, a.Issuer, a.Subject); e != nil {
		return out, e
	}
	for _, span := range spans {
		if _, e = tx.ExecContext(ctx, `INSERT INTO supervisor.plan_revision_spans(tenant_id,site_id,plan_revision_id,span_id) VALUES($1,$2,$3,$4)`, sc.TenantID, sc.SiteID, out.ID, span); e != nil {
			return out, e
		}
	}
	if e = audit(ctx, tx, a, sc, out.ID, "", "plan.captured"); e != nil {
		return out, e
	}
	if e = saveReceipt(ctx, tx, a, sc, key, requestHash, "plan", out.ID, ""); e != nil {
		return out, e
	}
	if out, e = load(ctx, tx, sc, out.ID); e != nil {
		return out, e
	}
	return out, s.finish(ctx, tx, a)
}

// Decide appends a PlanApprovalV1 decision. expected is the current review revision
// (0 before any decision); a stale value is a revision conflict.
func (s *Service) Decide(ctx context.Context, a Actor, sc Scope, planID string, in ReviewInput, key string, expected int64) (Approval, error) {
	var out Approval
	reason := strings.TrimSpace(in.Reason)
	if !validID(planID) || !validText(key, 128) || expected < 0 || (in.Decision != "approve" && in.Decision != "reject") ||
		!validText(reason, 1000) || !utf8.ValidString(reason) {
		return out, ErrInvalid
	}
	tx, auth, now, err := s.begin(ctx, a, sc, review)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	requestHash := digest(struct {
		PlanID   string
		Decision string
		Reason   string
		Expected int64
	}{planID, in.Decision, reason, expected})
	_, approvalID, replayed, err := receipt(ctx, tx, a, sc, key, requestHash, "review")
	if err != nil {
		return out, err
	}
	if replayed {
		if out, err = loadApproval(ctx, tx, sc, approvalID); err != nil {
			return out, err
		}
		out.Replayed = true
		return out, s.finish(ctx, tx, a)
	}
	var payloadDigest, submitterIssuer, submitterSubject string
	e := tx.QueryRowContext(ctx, `SELECT canonical_payload_sha256,actor_issuer,actor_subject FROM supervisor.plan_revisions WHERE tenant_id=$1 AND site_id=$2 AND id=$3`, sc.TenantID, sc.SiteID, planID).Scan(&payloadDigest, &submitterIssuer, &submitterSubject)
	if errors.Is(e, sql.ErrNoRows) {
		return out, ErrNotFound
	}
	if e != nil {
		return out, e
	}
	if submitterIssuer == a.Issuer && submitterSubject == a.Subject {
		return out, ErrSelfReview
	}
	var current int64
	e = tx.QueryRowContext(ctx, `SELECT current_revision FROM supervisor.plan_review_heads WHERE tenant_id=$1 AND site_id=$2 AND plan_revision_id=$3 FOR UPDATE`, sc.TenantID, sc.SiteID, planID).Scan(&current)
	if errors.Is(e, sql.ErrNoRows) {
		current = 0
	} else if e != nil {
		return out, e
	}
	if current != expected {
		return out, ErrRevision
	}
	spans, err := planSpans(ctx, tx, sc, planID)
	if err != nil {
		return out, err
	}
	// Rejection must always be possible; approval requires the latest revision and
	// current, unrevoked provenance.
	if in.Decision == "approve" {
		superseded, err := isSuperseded(ctx, tx, sc, planID)
		if err != nil {
			return out, err
		}
		if superseded {
			return out, ErrSuperseded
		}
		if err = checkSpans(ctx, tx, sc, spans, true); err != nil {
			return out, err
		}
	}
	next := current + 1
	if current == 0 {
		_, e = tx.ExecContext(ctx, `INSERT INTO supervisor.plan_review_heads(tenant_id,site_id,plan_revision_id,current_revision) VALUES($1,$2,$3,1)`, sc.TenantID, sc.SiteID, planID)
	} else {
		_, e = tx.ExecContext(ctx, `UPDATE supervisor.plan_review_heads SET current_revision=$4 WHERE tenant_id=$1 AND site_id=$2 AND plan_revision_id=$3`, sc.TenantID, sc.SiteID, planID, next)
	}
	if e != nil {
		return out, e
	}
	id := uuid.NewString()
	if _, e = tx.ExecContext(ctx, `INSERT INTO supervisor.plan_approvals(tenant_id,site_id,id,plan_revision_id,review_revision,canonical_payload_sha256,decision,reason,reviewer_issuer,reviewer_subject,policy_revision,auth_time,reviewed_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`, sc.TenantID, sc.SiteID, id, planID, next, payloadDigest, in.Decision, reason, a.Issuer, a.Subject, PolicyRevision, auth, now); e != nil {
		return out, e
	}
	for _, span := range spans {
		if _, e = tx.ExecContext(ctx, `INSERT INTO supervisor.plan_approval_spans(tenant_id,site_id,approval_id,plan_revision_id,span_id) VALUES($1,$2,$3,$4,$5)`, sc.TenantID, sc.SiteID, id, planID, span); e != nil {
			return out, e
		}
	}
	if e = audit(ctx, tx, a, sc, planID, id, "review.decided"); e != nil {
		return out, e
	}
	if e = saveReceipt(ctx, tx, a, sc, key, requestHash, "review", planID, id); e != nil {
		return out, e
	}
	if out, e = loadApproval(ctx, tx, sc, id); e != nil {
		return out, e
	}
	return out, s.finish(ctx, tx, a)
}

// Get returns a plan revision with its full, ordered decision history.
func (s *Service) Get(ctx context.Context, a Actor, sc Scope, planID string) (Revision, error) {
	if !validID(planID) {
		return Revision{}, ErrNotFound
	}
	tx, _, _, err := s.begin(ctx, a, sc, read)
	if err != nil {
		return Revision{}, err
	}
	defer tx.Rollback()
	out, err := load(ctx, tx, sc, planID)
	if err != nil {
		return out, err
	}
	if err = audit(ctx, tx, a, sc, planID, "", "plan.read"); err != nil {
		return out, err
	}
	return out, s.finish(ctx, tx, a)
}

func isSuperseded(ctx context.Context, tx *sql.Tx, sc Scope, planID string) (bool, error) {
	var superseded bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM supervisor.plan_revisions p JOIN supervisor.plan_revisions later ON (later.tenant_id,later.site_id,later.order_id,later.asset_id)=(p.tenant_id,p.site_id,p.order_id,p.asset_id) AND later.plan_revision>p.plan_revision WHERE p.tenant_id=$1 AND p.site_id=$2 AND p.id=$3)`, sc.TenantID, sc.SiteID, planID).Scan(&superseded)
	return superseded, err
}

func planSpans(ctx context.Context, tx *sql.Tx, sc Scope, planID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT span_id FROM supervisor.plan_revision_spans WHERE tenant_id=$1 AND site_id=$2 AND plan_revision_id=$3 ORDER BY span_id`, sc.TenantID, sc.SiteID, planID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	spans := make([]string, 0)
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		spans = append(spans, id)
	}
	return spans, rows.Err()
}

func load(ctx context.Context, tx *sql.Tx, sc Scope, planID string) (Revision, error) {
	var out Revision
	var payload []byte
	e := tx.QueryRowContext(ctx, `SELECT p.id,p.payload,p.canonical_payload_sha256,p.digest_schema_version,p.actor_issuer,p.actor_subject,p.created_at,coalesce(h.current_revision,0) FROM supervisor.plan_revisions p LEFT JOIN supervisor.plan_review_heads h ON (h.tenant_id,h.site_id,h.plan_revision_id)=(p.tenant_id,p.site_id,p.id) WHERE p.tenant_id=$1 AND p.site_id=$2 AND p.id=$3`, sc.TenantID, sc.SiteID, planID).Scan(&out.ID, &payload, &out.CanonicalPayloadSHA256, &out.DigestSchemaVersion, &out.SubmittedBy.Issuer, &out.SubmittedBy.Subject, &out.CreatedAt, &out.ReviewRevision)
	if errors.Is(e, sql.ErrNoRows) {
		return out, ErrNotFound
	}
	if e != nil {
		return out, e
	}
	if out.Plan, e = production.DecodePlanBody(payload); e != nil {
		return out, errIntegrity
	}
	// Stored bodies are re-verified against their digest on every read.
	if _, recomputed, err := production.CanonicalPlanPayload(sc.TenantID, sc.SiteID, out.Plan); err != nil || recomputed != out.CanonicalPayloadSHA256 {
		return out, errIntegrity
	}
	if out.SourceSpanIDs, e = planSpans(ctx, tx, sc, planID); e != nil {
		return out, e
	}
	if out.Superseded, e = isSuperseded(ctx, tx, sc, planID); e != nil {
		return out, e
	}
	rows, e := tx.QueryContext(ctx, `SELECT id FROM supervisor.plan_approvals WHERE tenant_id=$1 AND site_id=$2 AND plan_revision_id=$3 ORDER BY review_revision`, sc.TenantID, sc.SiteID, planID)
	if e != nil {
		return out, e
	}
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			rows.Close()
			return out, e
		}
		ids = append(ids, id)
	}
	if e = rows.Err(); e != nil {
		rows.Close()
		return out, e
	}
	rows.Close()
	out.Decisions = make([]Approval, 0, len(ids))
	for _, id := range ids {
		approval, err := loadApproval(ctx, tx, sc, id)
		if err != nil {
			return out, err
		}
		out.Decisions = append(out.Decisions, approval)
	}
	if n := len(out.Decisions); n > 0 {
		current := out.Decisions[n-1]
		if current.ReviewRevision != out.ReviewRevision {
			return out, errIntegrity
		}
		out.CurrentDecision = &current
		if current.Decision == "approve" && !out.Superseded {
			out.Approved = checkSpans(ctx, tx, sc, current.SourceSpanIDs, false) == nil
		}
	}
	return out, nil
}

func loadApproval(ctx context.Context, tx *sql.Tx, sc Scope, id string) (Approval, error) {
	var out Approval
	e := tx.QueryRowContext(ctx, `SELECT id,plan_revision_id,review_revision,canonical_payload_sha256,decision,reason,reviewer_issuer,reviewer_subject,policy_revision,auth_time,reviewed_at FROM supervisor.plan_approvals WHERE tenant_id=$1 AND site_id=$2 AND id=$3`, sc.TenantID, sc.SiteID, id).Scan(&out.ID, &out.PlanRevisionID, &out.ReviewRevision, &out.CanonicalPayloadSHA256, &out.Decision, &out.Reason, &out.Reviewer.Issuer, &out.Reviewer.Subject, &out.PolicyRevision, &out.AuthenticatedAt, &out.ReviewedAt)
	if errors.Is(e, sql.ErrNoRows) {
		return out, ErrNotFound
	}
	if e != nil {
		return out, e
	}
	rows, e := tx.QueryContext(ctx, `SELECT span_id FROM supervisor.plan_approval_spans WHERE tenant_id=$1 AND site_id=$2 AND approval_id=$3 ORDER BY span_id`, sc.TenantID, sc.SiteID, id)
	if e != nil {
		return out, e
	}
	defer rows.Close()
	out.SourceSpanIDs = make([]string, 0)
	for rows.Next() {
		var span string
		if e = rows.Scan(&span); e != nil {
			return out, e
		}
		out.SourceSpanIDs = append(out.SourceSpanIDs, span)
	}
	return out, rows.Err()
}
