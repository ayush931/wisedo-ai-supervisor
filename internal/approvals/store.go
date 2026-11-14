package approvals

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/session"
	"github.com/google/uuid"
)

// Store errors. The pure rule errors above (ErrRevisionConflict, ErrIllegalTransition,
// ErrExpired, ErrNotEligible, ErrInvalidDecision, ErrInvalidPayload) pass through unchanged.
var (
	ErrNotFound         = errors.New("proposal unavailable")
	ErrDenied           = errors.New("proposal permission denied")
	ErrReauthentication = errors.New("recent authentication required")
	ErrIdempotency      = errors.New("idempotency key reused for a different request")
	// ErrStaleBinding means the case the proposal is bound to has moved on (a new case
	// revision, or a closed case), so the payload no longer describes current work.
	ErrStaleBinding = errors.New("proposal binding is no longer current")
	// ErrEvidenceUnavailable means an evidence source the proposal revision binds is revoked or
	// missing (rule 06: a specific reason, distinct from a case that moved on).
	ErrEvidenceUnavailable = errors.New("bound evidence is no longer available")
	// ErrActiveProposal means the case already has a pending or approved proposal inside its
	// window; a change to it is an edit of that proposal, not a second proposal.
	ErrActiveProposal = errors.New("case already has a live proposal")
	// errIntegrity marks stored state that fails re-verification; it is never a client error.
	errIntegrity = errors.New("approval integrity check failed")
)

// PolicyRevision versions the interim reviewer policy. Until the WIS-290 capability model
// exists, proposal.approve is held by an active human member with the site supervisor role;
// owner is not an implicit bypass. Every decision also needs a recent authentication.
const PolicyRevision = 1

// Principal is the authenticated browser principal from the verified session.
type Principal struct{ Issuer, Subject, SessionHash string }

// Scope is the verified tenant and the path site.
type Scope struct{ TenantID, SiteID string }

// ActorRef is a stored identity.
type ActorRef struct {
	Issuer  string `json:"issuer"`
	Subject string `json:"subject"`
}

// Submission is a new proposal or a material edit of an existing one. It is an internal
// entry point for proposal generation (WIS-276); there is no HTTP route that creates an
// approved proposal. The server assigns the proposal identity, revision and approval window:
// those Payload fields are ignored and overwritten.
type Submission struct {
	// ProposalID is empty for a new proposal. For an edit it names the proposal and
	// ExpectedStateRevision must be its current state revision.
	ProposalID            string
	ExpectedStateRevision int64
	Payload               Payload
	// SubmittedBy is the human who explicitly submitted or edited this revision, or nil
	// when a generating run produced it. That human can never review it.
	SubmittedBy *Actor
	Profile     WindowProfile
}

// DecisionInput is the POST /proposals/{id}/decisions body.
type DecisionInput struct {
	Decision         string `json:"decision"`
	ExpectedRevision int64  `json:"expected_revision"`
	Reason           string `json:"reason"`
}

// RevokeInput is the POST /proposals/{id}/revoke body.
type RevokeInput struct {
	ExpectedRevision int64  `json:"expected_revision"`
	Reason           string `json:"reason"`
}

// DecisionRecord is one immutable approval, rejection or revocation.
type DecisionRecord struct {
	ID                     string    `json:"id"`
	ProposalRevision       int64     `json:"proposal_revision"`
	CanonicalPayloadSHA256 string    `json:"canonical_payload_sha256"`
	Revision               int64     `json:"revision"`
	Decision               string    `json:"decision"`
	Reason                 string    `json:"reason"`
	Reviewer               ActorRef  `json:"reviewer"`
	AuthenticatedAt        time.Time `json:"authenticated_at"`
	DecidedAt              time.Time `json:"decided_at"`
}

// View is the GET /proposals/{id} response: the current proposal revision with its exact
// canonical payload, the approval state and the full decision history. Revision is the
// approval state revision, the If-Match and expected_revision token for decisions.
type View struct {
	ID                     string           `json:"id"`
	ProposalRevision       int64            `json:"proposal_revision"`
	Revision               int64            `json:"revision"`
	State                  State            `json:"state"`
	CaseID                 string           `json:"case_id"`
	CaseRevision           int64            `json:"case_revision"`
	CanonicalPayload       json.RawMessage  `json:"canonical_payload"`
	CanonicalPayloadSHA256 string           `json:"canonical_payload_sha256"`
	SubmittedBy            *ActorRef        `json:"submitted_by"`
	IssuedAt               time.Time        `json:"issued_at"`
	ExpiresAt              time.Time        `json:"expires_at"`
	PolicyRevision         int64            `json:"policy_revision"`
	Decisions              []DecisionRecord `json:"decisions"`
	Replayed               bool             `json:"replayed"`
}

// Summary is one row of GET /proposals.
type Summary struct {
	ID               string    `json:"id"`
	ProposalRevision int64     `json:"proposal_revision"`
	Revision         int64     `json:"revision"`
	State            State     `json:"state"`
	CaseID           string    `json:"case_id"`
	ExpiresAt        time.Time `json:"expires_at"`
}

// Page is a cursor page of proposals ordered by ID.
type Page struct {
	Items      []Summary `json:"items"`
	NextCursor *string   `json:"next_cursor"`
}

const (
	DefaultPageSize = 50
	MaxPageSize     = 100
)

// Service persists proposals and decisions. Every rule comes from the pure functions in
// this package; the service adds locking, the server clock, idempotency and audit.
type Service struct {
	db       *sql.DB
	sessions session.Policy
}

func NewService(db *sql.DB) *Service { return &Service{db: db, sessions: session.Default()} }

// WithSessionPolicy returns a copy that enforces p (WIS-290 shared session policy).
func (s *Service) WithSessionPolicy(p session.Policy) *Service {
	c := *s
	c.sessions = p.OrDefault()
	return &c
}

type access int

const (
	read access = iota
	review
)

func validID(v string) bool { return canonicalUUID(v) }

func validField(v string, max int) bool {
	return len(v) > 0 && len(v) <= max && strings.TrimSpace(v) == v && utf8.ValidString(v) && !strings.ContainsRune(v, 0)
}

func requestDigest(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// begin opens a tenant-scoped transaction for a verified browser principal and applies the
// global lock order's first steps: the site scope, then the principal's membership. It
// returns whether the principal holds the interim proposal.approve grant, the session's
// auth_time and the server clock.
func (s *Service) begin(ctx context.Context, a Principal, sc Scope, mode access) (*sql.Tx, bool, time.Time, time.Time, error) {
	if !validID(sc.TenantID) || !validID(sc.SiteID) || !validField(a.Issuer, 255) || !validField(a.Subject, 255) || !validField(a.SessionHash, 128) {
		return nil, false, time.Time{}, time.Time{}, ErrDenied
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, time.Time{}, time.Time{}, err
	}
	fail := func(e error) (*sql.Tx, bool, time.Time, time.Time, error) {
		_ = tx.Rollback()
		return nil, false, time.Time{}, time.Time{}, e
	}
	if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true),set_config('statement_timeout','10s',true),set_config('lock_timeout','5s',true)`, sc.TenantID); err != nil {
		return fail(err)
	}
	var issuer, subject string
	var expiry, seen, auth, now time.Time
	err = tx.QueryRowContext(ctx, `SELECT issuer,subject,expires_at,last_seen_at,auth_time,clock_timestamp() FROM supervisor.browser_sessions WHERE token_hash=$1 FOR SHARE`, a.SessionHash).Scan(&issuer, &subject, &expiry, &seen, &auth, &now)
	if err != nil || issuer != a.Issuer || subject != a.Subject || !expiry.After(now) || !seen.After(now.Add(-s.sessions.Idle)) {
		return fail(ErrDenied)
	}
	var found bool
	if err = tx.QueryRowContext(ctx, `SELECT supervisor.lock_member_site($1,$2,$3,$4,$5)`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, mode != read).Scan(&found); err != nil {
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
	canApprove := role == "supervisor"
	if mode == review {
		if !canApprove {
			return fail(ErrDenied)
		}
		if auth.After(now) || auth.Before(now.Add(-s.sessions.RecentAuth)) {
			return fail(ErrReauthentication)
		}
	}
	return tx, canApprove, auth, now, nil
}

// finish re-checks the session inside the transaction so a sign-out or idle expiry that
// raced the request cannot commit it.
func (s *Service) finish(ctx context.Context, tx *sql.Tx, a Principal) error {
	var ok bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM supervisor.browser_sessions WHERE token_hash=$1 AND issuer=$2 AND subject=$3 AND expires_at>clock_timestamp() AND last_seen_at>clock_timestamp()-make_interval(secs=>$4))`, a.SessionHash, a.Issuer, a.Subject, s.sessions.IdleSeconds()).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return ErrDenied
	}
	return tx.Commit()
}

func audit(ctx context.Context, tx *sql.Tx, actor *ActorRef, sc Scope, proposalID string, stateRevision int64, event string) error {
	var issuer, subject any
	if actor != nil {
		issuer, subject = actor.Issuer, actor.Subject
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO supervisor.approval_audit(tenant_id,site_id,actor_issuer,actor_subject,event_type,proposal_id,state_revision) VALUES($1,$2,$3,$4,$5,$6,$7)`, sc.TenantID, sc.SiteID, issuer, subject, event, proposalID, stateRevision)
	return err
}

// head is the locked approval state plus the immutable facts of its current revision.
type head struct {
	proposalRevision, stateRevision, caseRevision int64
	state                                         State
	caseID, contextID, digest                     string
	expiresAt                                     time.Time
	submittedBy                                   *Actor
}

// lockHead follows the global lock order: the bound case row (share) before the proposal
// state (update). The case ID is read first from the immutable revision.
func lockHead(ctx context.Context, tx *sql.Tx, sc Scope, proposalID string) (head, error) {
	var h head
	var caseID string
	err := tx.QueryRowContext(ctx, `SELECT case_id FROM supervisor.proposal_revisions WHERE tenant_id=$1 AND site_id=$2 AND proposal_id=$3 AND revision=1`, sc.TenantID, sc.SiteID, proposalID).Scan(&caseID)
	if errors.Is(err, sql.ErrNoRows) {
		return h, ErrNotFound
	}
	if err != nil {
		return h, err
	}
	if _, err = tx.ExecContext(ctx, `SELECT 1 FROM supervisor.cases WHERE tenant_id=$1 AND site_id=$2 AND id=$3 FOR SHARE`, sc.TenantID, sc.SiteID, caseID); err != nil {
		return h, err
	}
	var issuer, subject sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT s.current_revision,s.state_revision,s.state,r.case_id,r.context_snapshot_id,r.case_revision,r.canonical_payload_sha256,r.expires_at,r.submitted_by_issuer,r.submitted_by_subject
 FROM supervisor.approval_states s JOIN supervisor.proposal_revisions r ON (r.tenant_id,r.site_id,r.proposal_id,r.revision)=(s.tenant_id,s.site_id,s.proposal_id,s.current_revision)
 WHERE s.tenant_id=$1 AND s.site_id=$2 AND s.proposal_id=$3 FOR UPDATE OF s`, sc.TenantID, sc.SiteID, proposalID).Scan(&h.proposalRevision, &h.stateRevision, &h.state, &h.caseID, &h.contextID, &h.caseRevision, &h.digest, &h.expiresAt, &issuer, &subject)
	if errors.Is(err, sql.ErrNoRows) {
		return h, ErrNotFound
	}
	if err != nil {
		return h, err
	}
	if issuer.Valid {
		h.submittedBy = &Actor{Issuer: issuer.String, Subject: subject.String}
	}
	return h, nil
}

// moveState moves the head to state `to` on the same proposal revision and records it.
func moveState(ctx context.Context, tx *sql.Tx, sc Scope, proposalID string, h head, to State, event Event, decisionID string) (int64, error) {
	next := h.stateRevision + 1
	if _, err := tx.ExecContext(ctx, `UPDATE supervisor.approval_states SET state=$4,state_revision=$5,updated_at=clock_timestamp() WHERE tenant_id=$1 AND site_id=$2 AND proposal_id=$3`, sc.TenantID, sc.SiteID, proposalID, string(to), next); err != nil {
		return 0, err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO supervisor.approval_transitions(tenant_id,site_id,proposal_id,state_revision,proposal_revision,from_state,to_state,event,decision_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,NULLIF($9,'')::uuid)`, sc.TenantID, sc.SiteID, proposalID, next, h.proposalRevision, string(h.state), string(to), string(event), decisionID)
	return next, err
}

// expireIfDue records the expiry of a pending or approved proposal whose window has closed
// on the server clock. It reports whether it did.
func expireIfDue(ctx context.Context, tx *sql.Tx, sc Scope, proposalID string, h *head, now time.Time) (bool, error) {
	if (h.state != Pending && h.state != Approved) || CheckWindow(now, h.expiresAt) == nil {
		return false, nil
	}
	next, err := moveState(ctx, tx, sc, proposalID, *h, Expired, Expire, "")
	if err != nil {
		return false, err
	}
	if err = audit(ctx, tx, nil, sc, proposalID, next, "proposal.expired"); err != nil {
		return false, err
	}
	h.state, h.stateRevision = Expired, next
	return true, nil
}

// checkBinding requires the bound case to be open and still at the revision the payload
// names. Its line, order and context are fixed by the case row's own foreign key.
func checkBinding(ctx context.Context, tx *sql.Tx, sc Scope, caseID string, caseRevision int64) error {
	var revision int64
	var state string
	err := tx.QueryRowContext(ctx, `SELECT revision,state FROM supervisor.cases WHERE tenant_id=$1 AND site_id=$2 AND id=$3`, sc.TenantID, sc.SiteID, caseID).Scan(&revision, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrStaleBinding
	}
	if err != nil {
		return err
	}
	if revision != caseRevision || state == "resolved" || state == "canceled" {
		return ErrStaleBinding
	}
	return nil
}

// checkDecisionBinding requires, at approval time, that the case is waiting for this decision
// (proposed or awaiting_approval) and still bound to the context the proposal was generated
// for. It is an allowlist: any other state (escalated, needs_input, executing, closed, or one
// added later) makes the proposal stale. The case revision is not compared: the case advances
// its own revision as it moves through proposed and awaiting_approval, which is not a change to
// what was proposed. A replaced context (clarification or resume) also makes it stale.
func checkDecisionBinding(ctx context.Context, tx *sql.Tx, sc Scope, caseID, contextID string) error {
	var state, current string
	err := tx.QueryRowContext(ctx, `SELECT state,context_snapshot_id FROM supervisor.cases WHERE tenant_id=$1 AND site_id=$2 AND id=$3`, sc.TenantID, sc.SiteID, caseID).Scan(&state, &current)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrStaleBinding
	}
	if err != nil {
		return err
	}
	if (state != "proposed" && state != "awaiting_approval") || current != contextID {
		return ErrStaleBinding
	}
	return nil
}

// unavailableEvidenceSQL selects the proposal revisions ($3 proposal, $4 revision; or every
// current revision when $3 is NULL) that bind an evidence source which is no longer readable:
// revoked, carrying a revocation row, or missing. Bound refs come from the immutable canonical
// payload, so the check covers exactly what the reviewer approved (WIS-274 staleness, rule 06).
// Revoke sets evidence_heads.revoked in the same transaction as its revocation row, so the head
// flag alone is authoritative. The scan JSON-expands the canonical payload of every approved
// current revision on the site: linear in live proposals, which is one per case (tens on a v1
// line). Index the bound refs before multi-line sites carry hundreds of live proposals.
const unavailableEvidenceSQL = `SELECT DISTINCT r.proposal_id::text FROM supervisor.proposal_revisions r
 CROSS JOIN LATERAL jsonb_array_elements(r.canonical_payload::jsonb->'body'->'evidence_revisions') e
 WHERE r.tenant_id=$1 AND r.site_id=$2 AND e->>'kind'='evidence'
 AND ($5::text IS NULL OR e->>'id'=$5)
 AND (($3::uuid IS NULL AND (r.proposal_id,r.revision) IN (SELECT s.proposal_id,s.current_revision FROM supervisor.approval_states s WHERE s.tenant_id=$1 AND s.site_id=$2 AND s.state='approved'))
   OR (r.proposal_id=$3::uuid AND r.revision=$4))
 AND NOT EXISTS(SELECT 1 FROM supervisor.evidence_heads h
   WHERE (h.tenant_id,h.site_id)=(r.tenant_id,r.site_id) AND h.source_id::text=e->>'id' AND NOT h.revoked)
 ORDER BY 1`

// checkBoundEvidence refuses approval of a proposal revision whose bound evidence is no longer
// readable. A pending proposal cannot become stale (no pending->stale transition), so the
// decision is refused instead; rejection stays possible.
func checkBoundEvidence(ctx context.Context, tx *sql.Tx, sc Scope, proposalID string, revision int64) error {
	var id string
	err := tx.QueryRowContext(ctx, unavailableEvidenceSQL, sc.TenantID, sc.SiteID, proposalID, revision, nil).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return ErrEvidenceUnavailable
}

// MarkEvidenceProposalsStale moves every approved proposal whose current revision binds the
// revoked evidence source to stale, inside the caller's revocation transaction. The caller
// already holds the site write lock and the source head, so the global lock order is kept:
// source row, then each bound case (share), then the proposal state (update). The audit row
// records actor, the person who revoked the source. It returns how many proposals it marked.
func MarkEvidenceProposalsStale(ctx context.Context, tx *sql.Tx, tenantID, siteID, sourceID string, actor ActorRef) (int, error) {
	sc := Scope{TenantID: tenantID, SiteID: siteID}
	rows, err := tx.QueryContext(ctx, unavailableEvidenceSQL, tenantID, siteID, nil, nil, sourceID)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	marked := 0
	for _, id := range ids {
		h, err := lockHead(ctx, tx, sc, id)
		if err != nil {
			return marked, err
		}
		if h.state != Approved {
			continue
		}
		next, err := moveState(ctx, tx, sc, id, h, Stale, MarkStale, "")
		if err != nil {
			return marked, err
		}
		if err = audit(ctx, tx, &actor, sc, id, next, "proposal.stale"); err != nil {
			return marked, err
		}
		marked++
	}
	return marked, nil
}

// earlierAuthors returns the humans who submitted or edited earlier revisions of a proposal.
// Rule 05: anyone who shaped the proposal, at any revision, may not review it.
func earlierAuthors(ctx context.Context, tx *sql.Tx, sc Scope, proposalID string, current int64) ([]Actor, error) {
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT submitted_by_issuer,submitted_by_subject FROM supervisor.proposal_revisions WHERE tenant_id=$1 AND site_id=$2 AND proposal_id=$3 AND revision<$4 AND submitted_by_issuer IS NOT NULL`, sc.TenantID, sc.SiteID, proposalID, current)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Actor
	for rows.Next() {
		var a Actor
		if err = rows.Scan(&a.Issuer, &a.Subject); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// MarkCaseProposalsStale moves every approved proposal of a case to stale inside the caller's
// transaction, which already holds the case row (global lock order: case, then proposal). It
// is called when the case is canceled, so an approval can never be consumed for a case that no
// longer exists as active work. The audit row records actor, the person who canceled the case.
// It returns how many proposals it marked.
func MarkCaseProposalsStale(ctx context.Context, tx *sql.Tx, tenantID, siteID, caseID string, actor ActorRef) (int, error) {
	sc := Scope{TenantID: tenantID, SiteID: siteID}
	rows, err := tx.QueryContext(ctx, `SELECT s.proposal_id FROM supervisor.approval_states s JOIN supervisor.proposal_revisions r ON (r.tenant_id,r.site_id,r.proposal_id,r.revision)=(s.tenant_id,s.site_id,s.proposal_id,s.current_revision) WHERE s.tenant_id=$1 AND s.site_id=$2 AND r.case_id=$3 AND s.state='approved' ORDER BY s.proposal_id`, tenantID, siteID, caseID)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	for _, id := range ids {
		h, err := lockHead(ctx, tx, sc, id)
		if err != nil {
			return 0, err
		}
		if h.state != Approved {
			continue
		}
		next, err := moveState(ctx, tx, sc, id, h, Stale, MarkStale, "")
		if err != nil {
			return 0, err
		}
		if err = audit(ctx, tx, &actor, sc, id, next, "proposal.stale"); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

// Submit records a new immutable proposal revision. A new proposal starts pending at
// revision 1. An edit of a pending proposal supersedes it; an edit of a rejected or expired
// one starts a fresh review. Approved, consumed, revoked or stale proposals cannot be edited.
func (s *Service) Submit(ctx context.Context, sc Scope, in Submission) (View, error) {
	return s.submit(ctx, sc, in, nil)
}

// SubmitWithCheck runs a trusted caller's current-scope check after the site and case locks,
// in the same transaction that creates the proposal.
func (s *Service) SubmitWithCheck(ctx context.Context, sc Scope, in Submission, check func(*sql.Tx) error) (View, error) {
	if check == nil {
		return View{}, ErrInvalidPayload
	}
	return s.submit(ctx, sc, in, check)
}

func (s *Service) submit(ctx context.Context, sc Scope, in Submission, check func(*sql.Tx) error) (View, error) {
	if !validID(sc.TenantID) || !validID(sc.SiteID) {
		return View{}, ErrNotFound
	}
	if in.SubmittedBy != nil && !in.SubmittedBy.valid() {
		return View{}, ErrInvalidPayload
	}
	edit := in.ProposalID != ""
	if edit && (!validID(in.ProposalID) || in.ExpectedStateRevision < 1) {
		return View{}, ErrInvalidPayload
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return View{}, err
	}
	defer tx.Rollback()
	var now time.Time
	if err = tx.QueryRowContext(ctx, `SELECT clock_timestamp() FROM (SELECT set_config('supervisor.tenant_id',$1,true),set_config('statement_timeout','10s',true),set_config('lock_timeout','5s',true)) c`, sc.TenantID).Scan(&now); err != nil {
		return View{}, err
	}
	var found bool
	if err = tx.QueryRowContext(ctx, `SELECT supervisor.lock_evidence_site($1,$2,true)`, sc.TenantID, sc.SiteID).Scan(&found); err != nil {
		return View{}, err
	}
	if !found {
		return View{}, ErrNotFound
	}
	p := in.Payload
	var h head
	proposalID, revision := uuid.NewString(), int64(1)
	// Global lock order: case row first, then the proposal head.
	if _, err = tx.ExecContext(ctx, `SELECT 1 FROM supervisor.cases WHERE tenant_id=$1 AND site_id=$2 AND id=$3 FOR SHARE`, sc.TenantID, sc.SiteID, p.CaseID); err != nil {
		return View{}, err
	}
	if check != nil {
		if err = check(tx); err != nil {
			return View{}, err
		}
	}
	if edit {
		proposalID = in.ProposalID
		if h, err = lockHead(ctx, tx, sc, proposalID); err != nil {
			return View{}, err
		}
		if h.caseID != p.CaseID {
			return View{}, ErrInvalidPayload
		}
		if _, err = expireIfDue(ctx, tx, sc, proposalID, &h, now); err != nil {
			return View{}, err
		}
		if h.stateRevision != in.ExpectedStateRevision {
			return View{}, ErrRevisionConflict
		}
		if h.state != Pending && h.state != Rejected && h.state != Expired {
			return View{}, ErrIllegalTransition
		}
		revision = h.proposalRevision + 1
	}
	// One live proposal per case: a second one could be approved alongside the first and
	// produce two tasks for one case. A material change edits the live proposal instead. An
	// edit that reopens a rejected or expired proposal is held to the same rule; the proposal
	// being edited is excluded.
	var live bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM supervisor.approval_states s JOIN supervisor.proposal_revisions r ON (r.tenant_id,r.site_id,r.proposal_id,r.revision)=(s.tenant_id,s.site_id,s.proposal_id,s.current_revision) WHERE s.tenant_id=$1 AND s.site_id=$2 AND r.case_id=$3 AND s.state IN ('pending','approved') AND r.expires_at>$4 AND s.proposal_id<>$5)`, sc.TenantID, sc.SiteID, p.CaseID, now, proposalID).Scan(&live); err != nil {
		return View{}, err
	}
	if live {
		return View{}, ErrActiveProposal
	}
	// The case row binds line, order, context snapshot and digest; the payload must agree.
	var lineID, orderID, contextID, contextDigest string
	var contextRevision int64
	err = tx.QueryRowContext(ctx, `SELECT line_id,order_id,context_snapshot_id,context_revision,context_digest FROM supervisor.cases WHERE tenant_id=$1 AND site_id=$2 AND id=$3`, sc.TenantID, sc.SiteID, p.CaseID).Scan(&lineID, &orderID, &contextID, &contextRevision, &contextDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return View{}, ErrInvalidPayload
	}
	if err != nil {
		return View{}, err
	}
	if lineID != p.LineID || orderID != p.OrderID || contextID != p.ContextSnapshotID || contextRevision != p.ContextRevision || contextDigest != p.ContextDigest {
		return View{}, ErrInvalidPayload
	}
	if err = checkBinding(ctx, tx, sc, p.CaseID, p.CaseRevision); err != nil {
		return View{}, err
	}
	// The server owns identity, revision and the approval window. Times are truncated to
	// microseconds, the precision PostgreSQL stores.
	issued := now.UTC().Truncate(time.Microsecond)
	expires, err := ExpiresAt(issued, in.Profile)
	if err != nil {
		return View{}, err
	}
	p.ProposalID, p.ProposalRevision, p.IssuedAt, p.ExpiresAt = proposalID, revision, issued, expires
	body, digest, err := Canonical(sc.TenantID, sc.SiteID, p, in.Profile)
	if err != nil {
		return View{}, err
	}
	var issuer, subject any
	if in.SubmittedBy != nil {
		issuer, subject = in.SubmittedBy.Issuer, in.SubmittedBy.Subject
	}
	inputDigest := requestDigest(struct {
		Payload     string
		SubmittedBy *Actor
	}{digest, in.SubmittedBy})
	if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.proposal_revisions(tenant_id,site_id,proposal_id,revision,case_id,case_revision,context_snapshot_id,generated_by_run_id,submitted_by_issuer,submitted_by_subject,issued_at,expires_at,input_sha256,canonical_payload,canonical_payload_sha256) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
		sc.TenantID, sc.SiteID, proposalID, revision, p.CaseID, p.CaseRevision, p.ContextSnapshotID, p.GeneratedByRun, issuer, subject, issued, expires, inputDigest, string(body), digest); err != nil {
		return View{}, err
	}
	var stateRevision int64 = 1
	if !edit {
		if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.approval_states(tenant_id,site_id,proposal_id,current_revision,state,state_revision) VALUES($1,$2,$3,1,'pending',1)`, sc.TenantID, sc.SiteID, proposalID); err != nil {
			return View{}, err
		}
	} else {
		stateRevision = h.stateRevision + 1
		if h.state == Pending {
			// A material edit supersedes the pending revision (recorded at the same state
			// revision as the new submission).
			if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.approval_transitions(tenant_id,site_id,proposal_id,state_revision,proposal_revision,from_state,to_state,event) VALUES($1,$2,$3,$4,$5,'pending','superseded','supersede')`, sc.TenantID, sc.SiteID, proposalID, stateRevision, h.proposalRevision); err != nil {
				return View{}, err
			}
		}
		if _, err = tx.ExecContext(ctx, `UPDATE supervisor.approval_states SET current_revision=$4,state='pending',state_revision=$5,updated_at=clock_timestamp() WHERE tenant_id=$1 AND site_id=$2 AND proposal_id=$3`, sc.TenantID, sc.SiteID, proposalID, revision, stateRevision); err != nil {
			return View{}, err
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.approval_transitions(tenant_id,site_id,proposal_id,state_revision,proposal_revision,from_state,to_state,event) VALUES($1,$2,$3,$4,$5,NULL,'pending','submit')`, sc.TenantID, sc.SiteID, proposalID, stateRevision, revision); err != nil {
		return View{}, err
	}
	var submitter *ActorRef
	if in.SubmittedBy != nil {
		submitter = &ActorRef{Issuer: in.SubmittedBy.Issuer, Subject: in.SubmittedBy.Subject}
	}
	if err = audit(ctx, tx, submitter, sc, proposalID, stateRevision, "proposal.submitted"); err != nil {
		return View{}, err
	}
	out, err := load(ctx, tx, sc, proposalID)
	if err != nil {
		return View{}, err
	}
	return out, tx.Commit()
}

// FindLiveGeneratedByRun returns the pending or approved proposal created by one run, if any.
func (s *Service) FindLiveGeneratedByRun(ctx context.Context, sc Scope, caseID, runID string) (string, error) {
	if !validID(sc.TenantID) || !validID(sc.SiteID) || !validID(caseID) || !validID(runID) {
		return "", ErrNotFound
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var found bool
	if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, sc.TenantID); err != nil {
		return "", err
	}
	if err = tx.QueryRowContext(ctx, `SELECT supervisor.lock_evidence_site($1,$2,false)`, sc.TenantID, sc.SiteID).Scan(&found); err != nil {
		return "", err
	}
	if !found {
		return "", ErrNotFound
	}
	var id string
	err = tx.QueryRowContext(ctx, `SELECT r.proposal_id FROM supervisor.proposal_revisions r JOIN supervisor.approval_states s ON (s.tenant_id,s.site_id,s.proposal_id,s.current_revision)=(r.tenant_id,r.site_id,r.proposal_id,r.revision)
 WHERE r.tenant_id=$1 AND r.site_id=$2 AND r.case_id=$3 AND r.generated_by_run_id=$4 AND s.state IN ('pending','approved') AND r.expires_at>clock_timestamp()
 ORDER BY r.revision DESC LIMIT 1`, sc.TenantID, sc.SiteID, caseID, runID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	if err != nil {
		return "", err
	}
	return id, tx.Commit()
}

// receipt returns the decision recorded under an idempotency key, or ErrIdempotency when the
// key was used for a different request. A replay is audited and still scope-authorized: the
// caller has already passed begin.
func receipt(ctx context.Context, tx *sql.Tx, a Principal, sc Scope, key, hash, op string) (string, string, bool, error) {
	var stored, storedOp, proposalID, decisionID string
	err := tx.QueryRowContext(ctx, `SELECT request_sha256,operation,proposal_id,decision_id FROM supervisor.approval_receipts WHERE tenant_id=$1 AND site_id=$2 AND actor_issuer=$3 AND actor_subject=$4 AND idempotency_key=$5`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, key).Scan(&stored, &storedOp, &proposalID, &decisionID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	if stored != hash || storedOp != op {
		return "", "", false, ErrIdempotency
	}
	return proposalID, decisionID, true, nil
}

// Decide records one human approval or rejection of the current proposal revision.
// ifMatch is the If-Match revision; it must equal the body's expected_revision (422).
func (s *Service) Decide(ctx context.Context, a Principal, sc Scope, proposalID string, in DecisionInput, key string, ifMatch int64) (View, error) {
	if !validID(proposalID) {
		return View{}, ErrNotFound
	}
	if !validID(key) || ifMatch != in.ExpectedRevision {
		return View{}, ErrInvalidDecision
	}
	decision := Decision{Kind: DecisionKind(in.Decision), ExpectedRevision: in.ExpectedRevision, Reason: in.Reason}
	// Validate the request shape before any storage access (the pure order puts 422 first).
	if _, err := Decide(time.Time{}, Proposal{Revision: in.ExpectedRevision}, Reviewer{}, decision); errors.Is(err, ErrInvalidDecision) {
		return View{}, err
	}
	return s.mutate(ctx, a, sc, proposalID, key, "proposal.decide", in, func(tx *sql.Tx, h head, now, auth time.Time, canApprove bool) (string, error) {
		prior, err := priorReviewers(ctx, tx, sc, proposalID, h.proposalRevision)
		if err != nil {
			return "", err
		}
		editors, err := earlierAuthors(ctx, tx, sc, proposalID, h.proposalRevision)
		if err != nil {
			return "", err
		}
		proposal := Proposal{State: h.state, Revision: h.stateRevision, ExpiresAt: h.expiresAt, Authority: ProposalAuthority{SubmittedBy: h.submittedBy, EditedBy: editors, PriorReviewers: prior}}
		reviewer := Reviewer{Actor: Actor{Issuer: a.Issuer, Subject: a.Subject}, Kind: KindHuman, HasProposalApprove: canApprove}
		next, err := Decide(now, proposal, reviewer, decision)
		if err != nil {
			return "", err
		}
		// Rejection is always possible; approval needs a binding that is still current.
		if next == Approved {
			if err = checkDecisionBinding(ctx, tx, sc, h.caseID, h.contextID); err != nil {
				return "", err
			}
			if err = checkBoundEvidence(ctx, tx, sc, proposalID, h.proposalRevision); err != nil {
				return "", err
			}
		}
		event := Approve
		if next == Rejected {
			event = Reject
		}
		return record(ctx, tx, a, sc, proposalID, h, in.Decision, in.Reason, auth, now, next, event)
	})
}

// Revoke withdraws an approval before it is consumed. Any eligible site reviewer may revoke,
// including the approver; revocation only ever reduces authority.
func (s *Service) Revoke(ctx context.Context, a Principal, sc Scope, proposalID string, in RevokeInput, key string, ifMatch int64) (View, error) {
	if !validID(proposalID) {
		return View{}, ErrNotFound
	}
	if !validID(key) || ifMatch != in.ExpectedRevision || in.ExpectedRevision < 1 {
		return View{}, ErrInvalidDecision
	}
	if err := validText(in.Reason, MaxReasonRunes); err != nil {
		return View{}, ErrInvalidDecision
	}
	return s.mutate(ctx, a, sc, proposalID, key, "proposal.revoke", in, func(tx *sql.Tx, h head, now, auth time.Time, _ bool) (string, error) {
		if h.stateRevision != in.ExpectedRevision {
			return "", ErrRevisionConflict
		}
		next, err := Apply(h.state, Revoke)
		if err != nil {
			return "", err
		}
		return record(ctx, tx, a, sc, proposalID, h, "revoke", in.Reason, auth, now, next, Revoke)
	})
}

// mutate runs a decision-like mutation: session and role, idempotent replay, the global lock
// order, server-clock expiry, then apply. An expiry found here is committed before the
// request is refused, so the stored state never lags the clock.
func (s *Service) mutate(ctx context.Context, a Principal, sc Scope, proposalID, key, op string, body any, apply func(*sql.Tx, head, time.Time, time.Time, bool) (string, error)) (View, error) {
	tx, canApprove, auth, now, err := s.begin(ctx, a, sc, review)
	if err != nil {
		return View{}, err
	}
	defer tx.Rollback()
	hash := requestDigest(struct {
		Op, ProposalID string
		Body           any
		Policy         int
	}{op, proposalID, body, PolicyRevision})
	storedProposal, _, replayed, err := receipt(ctx, tx, a, sc, key, hash, op)
	if err != nil {
		return View{}, err
	}
	if replayed {
		out, err := load(ctx, tx, sc, storedProposal)
		if err != nil {
			return View{}, err
		}
		if err = audit(ctx, tx, &ActorRef{a.Issuer, a.Subject}, sc, storedProposal, out.Revision, "mutation.replayed"); err != nil {
			return View{}, err
		}
		out.Replayed = true
		return out, s.finish(ctx, tx, a)
	}
	h, err := lockHead(ctx, tx, sc, proposalID)
	if err != nil {
		return View{}, err
	}
	expired, err := expireIfDue(ctx, tx, sc, proposalID, &h, now)
	if err != nil {
		return View{}, err
	}
	if expired {
		if err = s.finish(ctx, tx, a); err != nil {
			return View{}, err
		}
		return View{}, ErrExpired
	}
	decisionID, err := apply(tx, h, now, auth, canApprove)
	if err != nil {
		return View{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.approval_receipts(tenant_id,site_id,actor_issuer,actor_subject,idempotency_key,operation,request_sha256,proposal_id,decision_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, key, op, hash, proposalID, decisionID); err != nil {
		return View{}, err
	}
	out, err := load(ctx, tx, sc, proposalID)
	if err != nil {
		return View{}, err
	}
	return out, s.finish(ctx, tx, a)
}

func record(ctx context.Context, tx *sql.Tx, a Principal, sc Scope, proposalID string, h head, decision, reason string, auth, now time.Time, to State, event Event) (string, error) {
	id := uuid.NewString()
	if _, err := tx.ExecContext(ctx, `INSERT INTO supervisor.approval_decisions(tenant_id,site_id,id,proposal_id,proposal_revision,canonical_payload_sha256,decided_state_revision,decision,reason,reviewer_issuer,reviewer_subject,auth_time,decided_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		sc.TenantID, sc.SiteID, id, proposalID, h.proposalRevision, h.digest, h.stateRevision+1, decision, reason, a.Issuer, a.Subject, auth, now); err != nil {
		return "", err
	}
	next, err := moveState(ctx, tx, sc, proposalID, h, to, event, id)
	if err != nil {
		return "", err
	}
	eventType := "proposal.decided"
	if event == Revoke {
		eventType = "proposal.revoked"
	}
	return id, audit(ctx, tx, &ActorRef{a.Issuer, a.Subject}, sc, proposalID, next, eventType)
}

func priorReviewers(ctx context.Context, tx *sql.Tx, sc Scope, proposalID string, revision int64) ([]Actor, error) {
	rows, err := tx.QueryContext(ctx, `SELECT reviewer_issuer,reviewer_subject FROM supervisor.approval_decisions WHERE tenant_id=$1 AND site_id=$2 AND proposal_id=$3 AND proposal_revision=$4 AND decision IN ('approve','reject')`, sc.TenantID, sc.SiteID, proposalID, revision)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Actor
	for rows.Next() {
		var r Actor
		if err = rows.Scan(&r.Issuer, &r.Subject); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Get returns the current proposal revision, recording a closed approval window first.
func (s *Service) Get(ctx context.Context, a Principal, sc Scope, proposalID string) (View, error) {
	if !validID(proposalID) {
		return View{}, ErrNotFound
	}
	tx, _, _, now, err := s.begin(ctx, a, sc, read)
	if err != nil {
		return View{}, err
	}
	defer tx.Rollback()
	h, err := lockHead(ctx, tx, sc, proposalID)
	if err != nil {
		return View{}, err
	}
	if _, err = expireIfDue(ctx, tx, sc, proposalID, &h, now); err != nil {
		return View{}, err
	}
	out, err := load(ctx, tx, sc, proposalID)
	if err != nil {
		return View{}, err
	}
	if err = audit(ctx, tx, &ActorRef{a.Issuer, a.Subject}, sc, proposalID, out.Revision, "proposal.read"); err != nil {
		return View{}, err
	}
	return out, s.finish(ctx, tx, a)
}

// List returns the site's proposals ordered by ID after cursor. The state shown is the
// stored state; a closed window is recorded when the proposal itself is read or decided.
func (s *Service) List(ctx context.Context, a Principal, sc Scope, cursor string, limit int) (Page, error) {
	if limit == 0 {
		limit = DefaultPageSize
	}
	if limit < 1 || limit > MaxPageSize || (cursor != "" && !validID(cursor)) {
		return Page{}, ErrInvalidDecision
	}
	tx, _, _, _, err := s.begin(ctx, a, sc, read)
	if err != nil {
		return Page{}, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT s.proposal_id,s.current_revision,s.state_revision,s.state,r.case_id,r.expires_at
 FROM supervisor.approval_states s JOIN supervisor.proposal_revisions r ON (r.tenant_id,r.site_id,r.proposal_id,r.revision)=(s.tenant_id,s.site_id,s.proposal_id,s.current_revision)
 WHERE s.tenant_id=$1 AND s.site_id=$2 AND ($3='' OR s.proposal_id>$3::uuid) ORDER BY s.proposal_id LIMIT $4`, sc.TenantID, sc.SiteID, cursor, limit+1)
	if err != nil {
		return Page{}, err
	}
	out := Page{Items: make([]Summary, 0)}
	for rows.Next() {
		var v Summary
		if err = rows.Scan(&v.ID, &v.ProposalRevision, &v.Revision, &v.State, &v.CaseID, &v.ExpiresAt); err != nil {
			rows.Close()
			return Page{}, err
		}
		v.ExpiresAt = v.ExpiresAt.UTC()
		out.Items = append(out.Items, v)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return Page{}, err
	}
	rows.Close()
	if len(out.Items) > limit {
		out.Items = out.Items[:limit]
		next := out.Items[limit-1].ID
		out.NextCursor = &next
	}
	return out, s.finish(ctx, tx, a)
}

func load(ctx context.Context, tx *sql.Tx, sc Scope, proposalID string) (View, error) {
	var out View
	var payload string
	var issuer, subject sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT s.proposal_id,s.current_revision,s.state_revision,s.state,r.case_id,r.case_revision,r.canonical_payload,r.canonical_payload_sha256,r.submitted_by_issuer,r.submitted_by_subject,r.issued_at,r.expires_at
 FROM supervisor.approval_states s JOIN supervisor.proposal_revisions r ON (r.tenant_id,r.site_id,r.proposal_id,r.revision)=(s.tenant_id,s.site_id,s.proposal_id,s.current_revision)
 WHERE s.tenant_id=$1 AND s.site_id=$2 AND s.proposal_id=$3`, sc.TenantID, sc.SiteID, proposalID).Scan(&out.ID, &out.ProposalRevision, &out.Revision, &out.State, &out.CaseID, &out.CaseRevision, &payload, &out.CanonicalPayloadSHA256, &issuer, &subject, &out.IssuedAt, &out.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return out, ErrNotFound
	}
	if err != nil {
		return out, err
	}
	// The stored bytes are what was hashed; re-verify them on every read.
	sum := sha256.Sum256([]byte(payload))
	if hex.EncodeToString(sum[:]) != out.CanonicalPayloadSHA256 || !json.Valid([]byte(payload)) {
		return out, errIntegrity
	}
	out.CanonicalPayload = json.RawMessage(payload)
	out.IssuedAt, out.ExpiresAt = out.IssuedAt.UTC(), out.ExpiresAt.UTC()
	if issuer.Valid {
		out.SubmittedBy = &ActorRef{Issuer: issuer.String, Subject: subject.String}
	}
	out.PolicyRevision = PolicyRevision
	rows, err := tx.QueryContext(ctx, `SELECT id,proposal_revision,canonical_payload_sha256,decided_state_revision,decision,reason,reviewer_issuer,reviewer_subject,auth_time,decided_at FROM supervisor.approval_decisions WHERE tenant_id=$1 AND site_id=$2 AND proposal_id=$3 ORDER BY decided_state_revision`, sc.TenantID, sc.SiteID, proposalID)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	out.Decisions = make([]DecisionRecord, 0)
	for rows.Next() {
		var d DecisionRecord
		if err = rows.Scan(&d.ID, &d.ProposalRevision, &d.CanonicalPayloadSHA256, &d.Revision, &d.Decision, &d.Reason, &d.Reviewer.Issuer, &d.Reviewer.Subject, &d.AuthenticatedAt, &d.DecidedAt); err != nil {
			return out, err
		}
		d.AuthenticatedAt, d.DecidedAt = d.AuthenticatedAt.UTC(), d.DecidedAt.UTC()
		out.Decisions = append(out.Decisions, d)
	}
	return out, rows.Err()
}
