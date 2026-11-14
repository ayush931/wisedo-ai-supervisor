package workstation

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/canonical"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/session"
	"github.com/google/uuid"
)

// WIS-520 persisted shift plans: one plan per line and half-open shift interval, appended
// as immutable ShiftPlanBodyV1 revisions and approved by a supervisor other than the
// revision's submitter over its exact canonical digest. No optimizer runs on this path.
var (
	ErrNotFound         = errors.New("shift plan unavailable")
	ErrNoCurrentPlan    = errors.New("no approved shift plan covers this time")
	ErrDenied           = errors.New("shift plan permission denied")
	ErrConflict         = errors.New("shift plan idempotency conflict")
	ErrRevision         = errors.New("shift plan state revision changed")
	ErrDigestMismatch   = errors.New("shift plan digest does not match the current revision")
	ErrReauthentication = errors.New("recent authentication required")
	ErrSelfApproval     = errors.New("shift plan approver must differ from submitter")
	ErrShiftTaken       = errors.New("line already has a shift plan overlapping this interval")
	ErrUnchanged        = errors.New("shift plan revision is identical to the current revision")
	ErrAlreadyApproved  = errors.New("current shift plan revision is already approved")
	// errIntegrity marks stored state that fails re-verification; it is never a client error.
	errIntegrity = errors.New("shift plan integrity check failed")
)

const (
	// ShiftPlanPolicyRevision versions the approval policy: supervisor-only decisions by a
	// recently authenticated human distinct from the revision's submitter.
	ShiftPlanPolicyRevision = 1
	shiftPlanSchema         = "ShiftPlanBodyV1"
	shiftPlanDigestVersion  = 1
)

type ShiftPlanState string

const (
	StateDraft      ShiftPlanState = "draft"
	StateApproved   ShiftPlanState = "approved"
	StateSuperseded ShiftPlanState = "superseded"
)

type Actor struct{ Issuer, Subject, SessionHash string }
type Scope struct{ TenantID, SiteID string }

type ActorRefV1 struct {
	Issuer  string `json:"issuer"`
	Subject string `json:"subject"`
}

// ShiftPlanApproveInputV1 binds the approver to the exact revision content they reviewed;
// the expected state revision travels in If-Match.
type ShiftPlanApproveInputV1 struct {
	CanonicalPayloadSHA256 string `json:"canonical_payload_digest" contract:"sha256"`
}

type ShiftPlanApprovalV1 struct {
	CanonicalPayloadSHA256 string     `json:"canonical_payload_digest" contract:"sha256"`
	Approver               ActorRefV1 `json:"approver"`
	PolicyRevision         int64      `json:"policy_revision"`
	AuthenticatedAt        time.Time  `json:"authenticated_at"`
	ApprovedAt             time.Time  `json:"approved_at"`
}

type ShiftPlanRevisionV1 struct {
	Revision               int64                `json:"revision"`
	State                  ShiftPlanState       `json:"state"`
	Plan                   ShiftPlanBodyV1      `json:"plan"`
	CanonicalPayloadSHA256 string               `json:"canonical_payload_digest" contract:"sha256"`
	DigestSchemaVersion    int                  `json:"digest_schema_version"`
	SubmittedBy            ActorRefV1           `json:"submitted_by"`
	CreatedAt              time.Time            `json:"created_at"`
	Approval               *ShiftPlanApprovalV1 `json:"approval"`
}

// ShiftPlanV1 is a plan with its full, ordered revision history. StateRevision is the
// If-Match value for revise and approve.
type ShiftPlanV1 struct {
	ID               string                `json:"id"`
	LineID           string                `json:"line_id"`
	StartsAt         time.Time             `json:"starts_at"`
	EndsAt           time.Time             `json:"ends_at"`
	StateRevision    int64                 `json:"state_revision"`
	CurrentRevision  int64                 `json:"current_revision"`
	ApprovedRevision *int64                `json:"approved_revision"`
	Revisions        []ShiftPlanRevisionV1 `json:"revisions"`
	Replayed         bool                  `json:"replayed"`
}

type ShiftPlanSummaryV1 struct {
	ID               string    `json:"id"`
	LineID           string    `json:"line_id"`
	StartsAt         time.Time `json:"starts_at"`
	EndsAt           time.Time `json:"ends_at"`
	StateRevision    int64     `json:"state_revision"`
	CurrentRevision  int64     `json:"current_revision"`
	ApprovedRevision *int64    `json:"approved_revision"`
}

type ShiftPlanPageV1 struct {
	Plans      []ShiftPlanSummaryV1 `json:"plans"`
	NextCursor string               `json:"next_cursor,omitempty"`
}

// CurrentShiftPlanV1 is the approved revision in force for a line at an instant. A draft or
// superseded revision is never returned here.
type CurrentShiftPlanV1 struct {
	PlanID        string              `json:"plan_id"`
	StateRevision int64               `json:"state_revision"`
	Revision      ShiftPlanRevisionV1 `json:"revision"`
}

type ShiftPlanService struct {
	db       *sql.DB
	sessions session.Policy
}

func NewShiftPlans(db *sql.DB) *ShiftPlanService {
	return &ShiftPlanService{db: db, sessions: session.Default()}
}

// WithSessionPolicy returns a copy of the service that enforces p (WIS-290).
func (s *ShiftPlanService) WithSessionPolicy(p session.Policy) *ShiftPlanService {
	c := *s
	c.sessions = p.OrDefault()
	return &c
}

// normalized sorts assignments by workstation so an equal set always stores and hashes
// identically, and returns the canonical envelope whose shift times are UTC.
func normalized(tenantID, siteID string, p ShiftPlanBodyV1) (ShiftPlanBodyV1, []byte, string, error) {
	p.Assignments = append([]AssignmentV1(nil), p.Assignments...)
	sort.Slice(p.Assignments, func(i, j int) bool { return p.Assignments[i].WorkstationID < p.Assignments[j].WorkstationID })
	body := p
	start, _ := validTime(p.StartsAt)
	end, _ := validTime(p.EndsAt)
	body.StartsAt, body.EndsAt = start.UTC().Format(time.RFC3339Nano), end.UTC().Format(time.RFC3339Nano)
	encoded, digest, err := canonical.Envelope(shiftPlanSchema, shiftPlanDigestVersion, tenantID, siteID, body)
	return p, encoded, digest, err
}

func requestDigest(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

type shiftAccess int

const (
	shiftRead shiftAccess = iota
	shiftWrite
	shiftApprove
	// configRead is a snapshot read limited to owners and supervisors (configuration drafts).
	configRead
	// configPromote promotes a configuration draft: owner or supervisor with a recent
	// authentication, the same freshness shift plan approval requires.
	configPromote
	// stationOperate takes the site write lock for any active member; the station record
	// service then requires the member's current station authorization (rule 05: no role,
	// owner included, records without one).
	stationOperate
	// shiftRun starts or ends a shift run: owner or supervisor with a recent authentication.
	shiftRun
)

func (s *ShiftPlanService) begin(ctx context.Context, a Actor, sc Scope, mode shiftAccess) (*sql.Tx, time.Time, time.Time, error) {
	return s.beginOn(ctx, a, sc, mode, s.db)
}

type transactionStarter interface {
	BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
}

func (s *ShiftPlanService) beginOn(ctx context.Context, a Actor, sc Scope, mode shiftAccess, db transactionStarter) (*sql.Tx, time.Time, time.Time, error) {
	if !canonicalID(sc.TenantID) || !canonicalID(sc.SiteID) || !validText(a.Issuer, 255) || !validText(a.Subject, 255) || !validText(a.SessionHash, 128) {
		return nil, time.Time{}, time.Time{}, ErrInvalidShiftPlan
	}
	// Reads take one snapshot so a multi-statement read never mixes two head states (a head
	// read and the revisions it points to). They are not READ ONLY: every read is audited.
	read := mode == shiftRead || mode == configRead
	var opts *sql.TxOptions
	if read {
		opts = &sql.TxOptions{Isolation: sql.LevelRepeatableRead}
	}
	tx, e := db.BeginTx(ctx, opts)
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
	// Reads check the session and membership in their snapshot without row locks: every request
	// touches its session's last_seen_at, and a FOR SHARE on a row updated after a
	// REPEATABLE READ snapshot fails with 40001, which concurrent reads by one user hit
	// constantly. A snapshot check is still a point-in-time authorization; writers keep the locks.
	lock := " FOR SHARE"
	if read {
		lock = ""
	}
	var issuer, subject string
	var expiry, seen, auth, now time.Time
	e = tx.QueryRowContext(ctx, `SELECT issuer,subject,expires_at,last_seen_at,auth_time,clock_timestamp() FROM supervisor.browser_sessions WHERE token_hash=$1`+lock, a.SessionHash).Scan(&issuer, &subject, &expiry, &seen, &auth, &now)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return fail(e) // a storage failure is not a denial
	}
	if e != nil || issuer != a.Issuer || subject != a.Subject || !expiry.After(now) || !seen.After(now.Add(-s.sessions.Idle)) {
		return fail(ErrDenied)
	}
	// Writers lock the site row, which serializes shift overlap checks per site.
	var found bool
	if e = tx.QueryRowContext(ctx, `SELECT supervisor.lock_member_site($1,$2,$3,$4,$5)`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, !read).Scan(&found); e != nil {
		return fail(e)
	}
	if !found {
		return fail(ErrNotFound)
	}
	var role string
	e = tx.QueryRowContext(ctx, `SELECT role FROM supervisor.memberships WHERE tenant_id=$1 AND site_id=$2 AND issuer=$3 AND subject=$4 AND active`+lock, sc.TenantID, sc.SiteID, a.Issuer, a.Subject).Scan(&role)
	if errors.Is(e, sql.ErrNoRows) {
		return fail(ErrNotFound)
	}
	if e != nil {
		return fail(e)
	}
	switch mode {
	case shiftWrite, configRead, configPromote, shiftRun:
		if role != "owner" && role != "supervisor" {
			return fail(ErrDenied)
		}
		if (mode == configPromote || mode == shiftRun) && (auth.After(now) || auth.Before(now.Add(-s.sessions.RecentAuth))) {
			return fail(ErrReauthentication)
		}
	case shiftApprove:
		// Shift plan approval is supervisor-only; owner is not an implicit bypass.
		if role != "supervisor" {
			return fail(ErrDenied)
		}
		if auth.After(now) || auth.Before(now.Add(-s.sessions.RecentAuth)) {
			return fail(ErrReauthentication)
		}
	}
	return tx, auth, now, nil
}

func (s *ShiftPlanService) finish(ctx context.Context, tx *sql.Tx, a Actor) error {
	var ok bool
	if e := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM supervisor.browser_sessions WHERE token_hash=$1 AND issuer=$2 AND subject=$3 AND expires_at>clock_timestamp() AND last_seen_at>clock_timestamp()-make_interval(secs=>$4))`, a.SessionHash, a.Issuer, a.Subject, s.sessions.IdleSeconds()).Scan(&ok); e != nil {
		return e
	}
	if !ok {
		return ErrDenied
	}
	return tx.Commit()
}

func shiftAudit(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, planID string, revision int64, event string) error {
	_, e := tx.ExecContext(ctx, `INSERT INTO supervisor.shift_plan_audit(tenant_id,site_id,actor_issuer,actor_subject,plan_id,revision,event_type) VALUES($1,$2,$3,$4,NULLIF($5,'')::uuid,NULLIF($6,0),$7)`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, planID, revision, event)
	return e
}

// shiftReceipt returns the plan of an earlier identical request, or ErrConflict when the
// key was used for a different request.
func shiftReceipt(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, key, hash, op string) (string, bool, error) {
	var stored, storedOp, planID string
	var revision int64
	e := tx.QueryRowContext(ctx, `SELECT request_sha256,operation,plan_id,revision FROM supervisor.shift_plan_receipts WHERE tenant_id=$1 AND site_id=$2 AND actor_issuer=$3 AND actor_subject=$4 AND idempotency_key=$5`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, key).Scan(&stored, &storedOp, &planID, &revision)
	if errors.Is(e, sql.ErrNoRows) {
		return "", false, nil
	}
	if e != nil {
		return "", false, e
	}
	if stored != hash || storedOp != op {
		return "", false, ErrConflict
	}
	return planID, true, shiftAudit(ctx, tx, a, sc, planID, revision, "mutation.replayed")
}

func (s *ShiftPlanService) replay(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, planID string) (ShiftPlanV1, error) {
	out, err := loadShiftPlan(ctx, tx, sc, planID)
	if err != nil {
		return out, err
	}
	out.Replayed = true
	return out, s.finish(ctx, tx, a)
}

// commit appends the receipt and audit for a mutation that produced revision.
func (s *ShiftPlanService) commit(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, key, hash, op, planID string, revision int64, event string) (ShiftPlanV1, error) {
	if _, e := tx.ExecContext(ctx, `INSERT INTO supervisor.shift_plan_receipts(tenant_id,site_id,actor_issuer,actor_subject,idempotency_key,request_sha256,operation,plan_id,revision) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, key, hash, op, planID, revision); e != nil {
		return ShiftPlanV1{}, e
	}
	if e := shiftAudit(ctx, tx, a, sc, planID, revision, event); e != nil {
		return ShiftPlanV1{}, e
	}
	out, err := loadShiftPlan(ctx, tx, sc, planID)
	if err != nil {
		return out, err
	}
	return out, s.finish(ctx, tx, a)
}

func insertRevision(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, planID string, revision int64, p ShiftPlanBodyV1, encoded []byte, digest string) error {
	payload, err := json.Marshal(p)
	if err != nil {
		return ErrInvalidShiftPlan
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO supervisor.shift_plan_revisions(tenant_id,site_id,plan_id,revision,line_id,starts_at,ends_at,timezone,payload,canonical_payload,canonical_payload_sha256,digest_schema_version,actor_issuer,actor_subject) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9::jsonb,$10,$11,$12,$13,$14)`, sc.TenantID, sc.SiteID, planID, revision, p.LineID, p.StartsAt, p.EndsAt, p.Timezone, string(payload), string(encoded), digest, shiftPlanDigestVersion, a.Issuer, a.Subject)
	return err
}

// Create opens a plan for a line and shift with draft revision 1. A line's shifts never
// overlap; changing an existing shift's plan is a revision, not a second plan.
func (s *ShiftPlanService) Create(ctx context.Context, a Actor, sc Scope, p ShiftPlanBodyV1, key string) (ShiftPlanV1, error) {
	if !validText(key, 128) || p.Validate() != nil {
		return ShiftPlanV1{}, ErrInvalidShiftPlan
	}
	p, encoded, digest, err := normalized(sc.TenantID, sc.SiteID, p)
	if err != nil {
		return ShiftPlanV1{}, ErrInvalidShiftPlan
	}
	hash := requestDigest([]string{"create", digest})
	tx, _, _, err := s.begin(ctx, a, sc, shiftWrite)
	if err != nil {
		return ShiftPlanV1{}, err
	}
	defer tx.Rollback()
	if planID, replayed, err := shiftReceipt(ctx, tx, a, sc, key, hash, "create"); err != nil || replayed {
		if err != nil {
			return ShiftPlanV1{}, err
		}
		return s.replay(ctx, tx, a, sc, planID)
	}
	planID, err := createPlan(ctx, tx, a, sc, p, encoded, digest)
	if err != nil {
		return ShiftPlanV1{}, err
	}
	return s.commit(ctx, tx, a, sc, key, hash, "create", planID, 1, "plan.created")
}

// createPlan is Create inside the caller's write transaction: a new head with draft
// revision 1 submitted by a.
func createPlan(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, p ShiftPlanBodyV1, encoded []byte, digest string) (string, error) {
	var taken bool
	if e := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM supervisor.shift_plan_heads WHERE tenant_id=$1 AND site_id=$2 AND line_id=$3 AND starts_at<$5::timestamptz AND $4::timestamptz<ends_at)`, sc.TenantID, sc.SiteID, p.LineID, p.StartsAt, p.EndsAt).Scan(&taken); e != nil {
		return "", e
	}
	if taken {
		return "", ErrShiftTaken
	}
	if err := checkPins(ctx, tx, sc, p); err != nil {
		return "", err
	}
	planID := uuid.NewString()
	if _, e := tx.ExecContext(ctx, `INSERT INTO supervisor.shift_plan_heads(tenant_id,site_id,id,line_id,starts_at,ends_at,current_revision,state_revision) VALUES($1,$2,$3,$4,$5,$6,1,1)`, sc.TenantID, sc.SiteID, planID, p.LineID, p.StartsAt, p.EndsAt); e != nil {
		return "", e
	}
	return planID, insertRevision(ctx, tx, a, sc, planID, 1, p, encoded, digest)
}

type lockedHead struct {
	line                    string
	start, end              time.Time
	current, state          int64
	approved                sql.NullInt64
	currentDigest, timezone string
	// authored is true when the actor submitted any revision of this plan.
	authored bool
}

func lockHead(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, planID string) (lockedHead, error) {
	var h lockedHead
	if !canonicalID(planID) {
		return h, ErrNotFound
	}
	e := tx.QueryRowContext(ctx, `SELECT h.line_id,h.starts_at,h.ends_at,h.current_revision,h.state_revision,h.approved_revision,r.canonical_payload_sha256,r.timezone,EXISTS(SELECT 1 FROM supervisor.shift_plan_revisions x WHERE (x.tenant_id,x.site_id,x.plan_id)=(h.tenant_id,h.site_id,h.id) AND x.actor_issuer=$4 AND x.actor_subject=$5) FROM supervisor.shift_plan_heads h JOIN supervisor.shift_plan_revisions r ON (r.tenant_id,r.site_id,r.plan_id,r.revision)=(h.tenant_id,h.site_id,h.id,h.current_revision) WHERE h.tenant_id=$1 AND h.site_id=$2 AND h.id=$3 FOR UPDATE OF h`, sc.TenantID, sc.SiteID, planID, a.Issuer, a.Subject).Scan(&h.line, &h.start, &h.end, &h.current, &h.state, &h.approved, &h.currentDigest, &h.timezone, &h.authored)
	if errors.Is(e, sql.ErrNoRows) {
		return h, ErrNotFound
	}
	return h, e
}

// Revise appends a draft revision for the same line and shift. The prior draft is
// superseded at once; a prior approved revision stays in force until this one is approved.
func (s *ShiftPlanService) Revise(ctx context.Context, a Actor, sc Scope, planID string, p ShiftPlanBodyV1, key string, expected int64) (ShiftPlanV1, error) {
	if !validText(key, 128) || expected < 1 || p.Validate() != nil {
		return ShiftPlanV1{}, ErrInvalidShiftPlan
	}
	p, encoded, digest, err := normalized(sc.TenantID, sc.SiteID, p)
	if err != nil {
		return ShiftPlanV1{}, ErrInvalidShiftPlan
	}
	hash := requestDigest([]any{"revise", planID, expected, digest})
	tx, _, _, err := s.begin(ctx, a, sc, shiftWrite)
	if err != nil {
		return ShiftPlanV1{}, err
	}
	defer tx.Rollback()
	if stored, replayed, err := shiftReceipt(ctx, tx, a, sc, key, hash, "revise"); err != nil || replayed {
		if err != nil {
			return ShiftPlanV1{}, err
		}
		return s.replay(ctx, tx, a, sc, stored)
	}
	next, err := revisePlan(ctx, tx, a, sc, planID, p, encoded, digest, expected)
	if err != nil {
		return ShiftPlanV1{}, err
	}
	return s.commit(ctx, tx, a, sc, key, hash, "revise", planID, next, "plan.revised")
}

// revisePlan is Revise inside the caller's write transaction: it appends a draft revision
// submitted by a when the plan's state revision is still expected, and returns its number.
func revisePlan(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, planID string, p ShiftPlanBodyV1, encoded []byte, digest string, expected int64) (int64, error) {
	h, err := lockHead(ctx, tx, a, sc, planID)
	if err != nil {
		return 0, err
	}
	if h.state != expected {
		return 0, ErrRevision
	}
	start, _ := validTime(p.StartsAt)
	end, _ := validTime(p.EndsAt)
	// A revision keeps the plan's line, shift and timezone; a different shift is a new plan.
	if p.LineID != h.line || !start.Equal(h.start) || !end.Equal(h.end) || p.Timezone != h.timezone {
		return 0, ErrInvalidShiftPlan
	}
	if digest == h.currentDigest {
		return 0, ErrUnchanged
	}
	if err = checkPins(ctx, tx, sc, p); err != nil {
		return 0, err
	}
	next := h.current + 1
	if _, e := tx.ExecContext(ctx, `UPDATE supervisor.shift_plan_heads SET current_revision=$4,state_revision=state_revision+1 WHERE tenant_id=$1 AND site_id=$2 AND id=$3`, sc.TenantID, sc.SiteID, planID, next); e != nil {
		return 0, e
	}
	return next, insertRevision(ctx, tx, a, sc, planID, next, p, encoded, digest)
}

// Approve records a supervisor decision over the current revision's exact digest and makes
// it the plan in force, superseding any earlier approved revision of this shift.
func (s *ShiftPlanService) Approve(ctx context.Context, a Actor, sc Scope, planID string, in ShiftPlanApproveInputV1, key string, expected int64) (ShiftPlanV1, error) {
	if !validText(key, 128) || expected < 1 || !digest64.MatchString(in.CanonicalPayloadSHA256) {
		return ShiftPlanV1{}, ErrInvalidShiftPlan
	}
	hash := requestDigest([]any{"approve", planID, expected, in.CanonicalPayloadSHA256})
	tx, auth, now, err := s.begin(ctx, a, sc, shiftApprove)
	if err != nil {
		return ShiftPlanV1{}, err
	}
	defer tx.Rollback()
	if stored, replayed, err := shiftReceipt(ctx, tx, a, sc, key, hash, "approve"); err != nil || replayed {
		if err != nil {
			return ShiftPlanV1{}, err
		}
		return s.replay(ctx, tx, a, sc, stored)
	}
	h, err := lockHead(ctx, tx, a, sc, planID)
	if err != nil {
		return ShiftPlanV1{}, err
	}
	if h.state != expected {
		return ShiftPlanV1{}, ErrRevision
	}
	if h.approved.Valid && h.approved.Int64 == h.current {
		return ShiftPlanV1{}, ErrAlreadyApproved
	}
	// A started run follows a re-approval; an ended run's plan is history.
	if run, err := loadRun(ctx, tx, sc, planID); err != nil {
		return ShiftPlanV1{}, err
	} else if run.ended.Valid {
		return ShiftPlanV1{}, ErrShiftRunEnded
	}
	if in.CanonicalPayloadSHA256 != h.currentDigest {
		return ShiftPlanV1{}, ErrDigestMismatch
	}
	// Rule 05: nobody who submitted any revision of this plan may approve it.
	if h.authored {
		return ShiftPlanV1{}, ErrSelfApproval
	}
	if err = checkApprovable(ctx, tx, sc, planID, h.current); err != nil {
		return ShiftPlanV1{}, err
	}
	if _, e := tx.ExecContext(ctx, `INSERT INTO supervisor.shift_plan_approvals(tenant_id,site_id,plan_id,revision,canonical_payload_sha256,approver_issuer,approver_subject,policy_revision,auth_time,approved_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, sc.TenantID, sc.SiteID, planID, h.current, h.currentDigest, a.Issuer, a.Subject, ShiftPlanPolicyRevision, auth, now); e != nil {
		return ShiftPlanV1{}, e
	}
	if _, e := tx.ExecContext(ctx, `UPDATE supervisor.shift_plan_heads SET approved_revision=current_revision,state_revision=state_revision+1 WHERE tenant_id=$1 AND site_id=$2 AND id=$3`, sc.TenantID, sc.SiteID, planID); e != nil {
		return ShiftPlanV1{}, e
	}
	return s.commit(ctx, tx, a, sc, key, hash, "approve", planID, h.current, "plan.approved")
}

// Get returns a plan with every revision, its derived state and its approval.
func (s *ShiftPlanService) Get(ctx context.Context, a Actor, sc Scope, planID string) (ShiftPlanV1, error) {
	if !canonicalID(planID) {
		return ShiftPlanV1{}, ErrNotFound
	}
	tx, _, _, err := s.begin(ctx, a, sc, shiftRead)
	if err != nil {
		return ShiftPlanV1{}, err
	}
	defer tx.Rollback()
	out, err := loadShiftPlan(ctx, tx, sc, planID)
	if err != nil {
		return out, err
	}
	if err = shiftAudit(ctx, tx, a, sc, planID, 0, "plan.read"); err != nil {
		return out, err
	}
	return out, s.finish(ctx, tx, a)
}

// List pages plan heads by ID.
func (s *ShiftPlanService) List(ctx context.Context, a Actor, sc Scope, limit int, cursor string) (ShiftPlanPageV1, error) {
	out := ShiftPlanPageV1{Plans: []ShiftPlanSummaryV1{}}
	if limit < 1 || limit > 100 || (cursor != "" && !canonicalID(cursor)) {
		return out, ErrInvalidShiftPlan
	}
	if cursor == "" {
		cursor = uuid.Nil.String()
	}
	tx, _, _, err := s.begin(ctx, a, sc, shiftRead)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id,line_id,starts_at,ends_at,state_revision,current_revision,approved_revision FROM supervisor.shift_plan_heads WHERE tenant_id=$1 AND site_id=$2 AND id>$3 ORDER BY id LIMIT $4`, sc.TenantID, sc.SiteID, cursor, limit+1)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var p ShiftPlanSummaryV1
		var approved sql.NullInt64
		if err = rows.Scan(&p.ID, &p.LineID, &p.StartsAt, &p.EndsAt, &p.StateRevision, &p.CurrentRevision, &approved); err != nil {
			return out, err
		}
		p.ApprovedRevision, p.StartsAt, p.EndsAt = nullable(approved), p.StartsAt.UTC(), p.EndsAt.UTC()
		out.Plans = append(out.Plans, p)
	}
	if err = rows.Err(); err != nil {
		return out, err
	}
	if len(out.Plans) > limit {
		out.Plans = out.Plans[:limit]
		out.NextCursor = out.Plans[limit-1].ID
	}
	if err = shiftAudit(ctx, tx, a, sc, "", 0, "plan.listed"); err != nil {
		return out, err
	}
	return out, s.finish(ctx, tx, a)
}

// Current returns the approved revision in force for a line at an instant (the database
// clock when at is nil). Drafts and superseded revisions are never current.
func (s *ShiftPlanService) Current(ctx context.Context, a Actor, sc Scope, lineID string, at *time.Time) (CurrentShiftPlanV1, error) {
	var out CurrentShiftPlanV1
	if !canonicalID(lineID) {
		return out, ErrInvalidShiftPlan
	}
	tx, _, _, err := s.begin(ctx, a, sc, shiftRead)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	var approved int64
	e := tx.QueryRowContext(ctx, `SELECT id,approved_revision FROM supervisor.shift_plan_heads WHERE tenant_id=$1 AND site_id=$2 AND line_id=$3 AND approved_revision IS NOT NULL AND starts_at<=coalesce($4,clock_timestamp()) AND coalesce($4,clock_timestamp())<ends_at`, sc.TenantID, sc.SiteID, lineID, at).Scan(&out.PlanID, &approved)
	if errors.Is(e, sql.ErrNoRows) {
		return out, ErrNoCurrentPlan
	}
	if e != nil {
		return out, e
	}
	plan, err := loadShiftPlan(ctx, tx, sc, out.PlanID)
	if err != nil {
		return out, err
	}
	out.StateRevision, out.Revision = plan.StateRevision, plan.Revisions[approved-1]
	if out.Revision.State != StateApproved {
		return out, errIntegrity
	}
	if err = shiftAudit(ctx, tx, a, sc, out.PlanID, approved, "plan.read"); err != nil {
		return out, err
	}
	return out, s.finish(ctx, tx, a)
}

func nullable(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	return &v.Int64
}

func loadShiftPlan(ctx context.Context, tx *sql.Tx, sc Scope, planID string) (ShiftPlanV1, error) {
	var out ShiftPlanV1
	var approved sql.NullInt64
	e := tx.QueryRowContext(ctx, `SELECT id,line_id,starts_at,ends_at,state_revision,current_revision,approved_revision FROM supervisor.shift_plan_heads WHERE tenant_id=$1 AND site_id=$2 AND id=$3`, sc.TenantID, sc.SiteID, planID).Scan(&out.ID, &out.LineID, &out.StartsAt, &out.EndsAt, &out.StateRevision, &out.CurrentRevision, &approved)
	if errors.Is(e, sql.ErrNoRows) {
		return out, ErrNotFound
	}
	if e != nil {
		return out, e
	}
	out.ApprovedRevision, out.StartsAt, out.EndsAt = nullable(approved), out.StartsAt.UTC(), out.EndsAt.UTC()
	rows, e := tx.QueryContext(ctx, `SELECT r.revision,r.payload,r.canonical_payload_sha256,r.digest_schema_version,r.actor_issuer,r.actor_subject,r.created_at,a.canonical_payload_sha256,a.approver_issuer,a.approver_subject,a.policy_revision,a.auth_time,a.approved_at FROM supervisor.shift_plan_revisions r LEFT JOIN supervisor.shift_plan_approvals a ON (a.tenant_id,a.site_id,a.plan_id,a.revision)=(r.tenant_id,r.site_id,r.plan_id,r.revision) WHERE r.tenant_id=$1 AND r.site_id=$2 AND r.plan_id=$3 ORDER BY r.revision`, sc.TenantID, sc.SiteID, planID)
	if e != nil {
		return out, e
	}
	defer rows.Close()
	out.Revisions = []ShiftPlanRevisionV1{}
	for rows.Next() {
		var r ShiftPlanRevisionV1
		var payload []byte
		var digest, issuer, subject sql.NullString
		var policy sql.NullInt64
		var auth, at sql.NullTime
		if e = rows.Scan(&r.Revision, &payload, &r.CanonicalPayloadSHA256, &r.DigestSchemaVersion, &r.SubmittedBy.Issuer, &r.SubmittedBy.Subject, &r.CreatedAt, &digest, &issuer, &subject, &policy, &auth, &at); e != nil {
			return out, e
		}
		// Stored bodies are re-validated and re-verified against their digest on every read.
		if r.Plan, e = DecodeShiftPlan(payload); e != nil {
			return out, errIntegrity
		}
		if _, _, recomputed, err := normalized(sc.TenantID, sc.SiteID, r.Plan); err != nil || recomputed != r.CanonicalPayloadSHA256 || r.Revision != int64(len(out.Revisions)+1) {
			return out, errIntegrity
		}
		r.CreatedAt = r.CreatedAt.UTC()
		if digest.Valid {
			r.Approval = &ShiftPlanApprovalV1{CanonicalPayloadSHA256: digest.String, Approver: ActorRefV1{issuer.String, subject.String}, PolicyRevision: policy.Int64, AuthenticatedAt: auth.Time.UTC(), ApprovedAt: at.Time.UTC()}
		}
		switch {
		case approved.Valid && r.Revision == approved.Int64:
			r.State = StateApproved
		case r.Revision == out.CurrentRevision && (!approved.Valid || approved.Int64 < r.Revision):
			r.State = StateDraft
		default:
			r.State = StateSuperseded
		}
		out.Revisions = append(out.Revisions, r)
	}
	if e = rows.Err(); e != nil {
		return out, e
	}
	if int64(len(out.Revisions)) != out.CurrentRevision {
		return out, errIntegrity
	}
	return out, nil
}
