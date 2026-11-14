// Package approvals holds the pure rules for immutable proposal approvals (WIS-274): the
// state machine, reviewer eligibility, the approval window, the decision and dispatch checks,
// and the canonical payload digest. Persistence, the HTTP API and the executor call these
// rules; they never reimplement them. Nothing here reads a clock: callers pass server time.
package approvals

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/canonical"
	"github.com/google/uuid"
)

// ApprovalTTL is the default approval lifetime from a proposal's immutable creation time. A
// customer may tighten it. Anything longer needs an explicit commissioned profile, and even
// then never more than MaxCommissionedTTL.
const (
	ApprovalTTL        = 30 * time.Minute
	MaxCommissionedTTL = 24 * time.Hour
)

var (
	// ErrIllegalTransition means the state and event pair is not allowed (409).
	ErrIllegalTransition = errors.New("illegal approval transition")
	// ErrNotEligible means the principal may not decide or dispatch this proposal (403).
	ErrNotEligible = errors.New("reviewer is not eligible")
	// ErrExpired means the approval window has closed; the decision is refused, not recorded.
	ErrExpired = errors.New("approval window has expired")
	// ErrRevisionConflict means the decision named a stale proposal revision (412).
	ErrRevisionConflict = errors.New("proposal revision changed")
	// ErrInvalidDecision means the decision request is malformed (422).
	ErrInvalidDecision = errors.New("invalid approval decision")
	// ErrInvalidPayload means the action payload is outside the v1 contract (422).
	ErrInvalidPayload = errors.New("invalid approval payload")
	// ErrInvalidWindow means the approval window does not match the profile (422).
	ErrInvalidWindow = errors.New("invalid approval window")
	// ErrDigestMismatch means the stored digest does not match the payload (dispatch refused).
	ErrDigestMismatch = errors.New("approval digest does not match the payload")
)

// State is the proposal approval lifecycle.
type State string

const (
	Pending    State = "pending"
	Approved   State = "approved"
	Rejected   State = "rejected"
	Expired    State = "expired"
	Superseded State = "superseded"
	Consumed   State = "consumed"
	Revoked    State = "revoked"
	Stale      State = "stale"
)

// States lists every state, for exhaustive tests and contract generation.
func States() []State {
	return []State{Pending, Approved, Rejected, Expired, Superseded, Consumed, Revoked, Stale}
}

// Event is a decision or a system action on a proposal.
type Event string

const (
	Approve   Event = "approve"
	Reject    Event = "reject"
	Expire    Event = "expire"
	Supersede Event = "supersede"
	Consume   Event = "consume"
	Revoke    Event = "revoke"
	MarkStale Event = "mark_stale"
)

// Events lists every event.
func Events() []Event { return []Event{Approve, Reject, Expire, Supersede, Consume, Revoke, MarkStale} }

type transition struct {
	from  State
	event Event
}

// transitions is the whole lifecycle. Pending is never re-entered: a material edit creates a
// new proposal revision and supersedes this one. Every other pair is illegal.
var transitions = map[transition]State{
	{Pending, Approve}:    Approved,
	{Pending, Reject}:     Rejected,
	{Pending, Expire}:     Expired,
	{Pending, Supersede}:  Superseded,
	{Approved, Consume}:   Consumed,
	{Approved, Revoke}:    Revoked,
	{Approved, Expire}:    Expired,
	{Approved, MarkStale}: Stale,
}

// Apply returns the next state, or ErrIllegalTransition and the unchanged state.
func Apply(from State, event Event) (State, error) {
	if to, ok := transitions[transition{from, event}]; ok {
		return to, nil
	}
	return from, ErrIllegalTransition
}

// PrincipalKind is the verified kind of an authenticated principal. Only humans decide.
type PrincipalKind string

const (
	KindHuman   PrincipalKind = "human"
	KindService PrincipalKind = "service"
	KindModel   PrincipalKind = "model"
)

// Actor is a verified identity: the OIDC issuer and subject. Subjects are case-sensitive
// opaque strings, so two actors are the same person only when both fields are equal.
type Actor struct {
	Issuer  string
	Subject string
}

func (a Actor) valid() bool {
	return a.Issuer != "" && a.Subject != "" && strings.TrimSpace(a.Issuer) == a.Issuer &&
		strings.TrimSpace(a.Subject) == a.Subject && utf8.ValidString(a.Issuer) && utf8.ValidString(a.Subject)
}

// Reviewer is the principal deciding or whose earlier approval is being dispatched. Kind and
// HasProposalApprove come from the server's identity and capability model, never the request.
type Reviewer struct {
	Actor              Actor
	Kind               PrincipalKind
	HasProposalApprove bool
}

// ProposalAuthority records who shaped this proposal revision. SubmittedBy is nil when a
// workflow generated the proposal and no human submitted it.
type ProposalAuthority struct {
	SubmittedBy    *Actor
	EditedBy       []Actor
	PriorReviewers []Actor // reviewers already counted for this revision
}

// CheckReviewer applies the decision eligibility rules: a human holding proposal.approve, not
// the submitter or an editor of the proposal, and not already counted for this revision. Owner
// role is not consulted: an owner without the grant is refused.
func CheckReviewer(r Reviewer, a ProposalAuthority) error {
	if err := checkPerson(r, a); err != nil {
		return err
	}
	for _, prior := range a.PriorReviewers {
		if prior == r.Actor {
			return ErrNotEligible
		}
	}
	return nil
}

// checkPerson is the part of eligibility that must still hold at dispatch time.
func checkPerson(r Reviewer, a ProposalAuthority) error {
	if !r.Actor.valid() || r.Kind != KindHuman || !r.HasProposalApprove {
		return ErrNotEligible
	}
	if a.SubmittedBy != nil && *a.SubmittedBy == r.Actor {
		return ErrNotEligible
	}
	for _, editor := range a.EditedBy {
		if editor == r.Actor {
			return ErrNotEligible
		}
	}
	return nil
}

// WindowProfile is the site's approval window policy. TTL zero means ApprovalTTL. A TTL above
// ApprovalTTL is allowed only when Commissioned is set by an explicit commissioned profile.
type WindowProfile struct {
	TTL          time.Duration
	Commissioned bool
}

func (p WindowProfile) ttl() (time.Duration, error) {
	ttl := p.TTL
	if ttl == 0 {
		ttl = ApprovalTTL
	}
	switch {
	case ttl < time.Second || ttl%time.Second != 0:
		return 0, fmt.Errorf("%w: ttl must be a positive whole number of seconds", ErrInvalidWindow)
	case ttl > ApprovalTTL && !p.Commissioned:
		return 0, fmt.Errorf("%w: ttl above %s needs a commissioned profile", ErrInvalidWindow, ApprovalTTL)
	case ttl > MaxCommissionedTTL:
		return 0, fmt.Errorf("%w: ttl above %s", ErrInvalidWindow, MaxCommissionedTTL)
	}
	return ttl, nil
}

// ExpiresAt is the end of the approval window that starts at the proposal's immutable creation.
func ExpiresAt(createdAt time.Time, p WindowProfile) (time.Time, error) {
	ttl, err := p.ttl()
	if err != nil {
		return time.Time{}, err
	}
	if _, err = canonicalTime(createdAt); err != nil {
		return time.Time{}, err
	}
	return createdAt.UTC().Add(ttl), nil
}

// ValidateWindow requires expiresAt to be exactly the profile's window after issuedAt, so an
// expiry can never be chosen independently of the creation time.
func ValidateWindow(issuedAt, expiresAt time.Time, p WindowProfile) error {
	want, err := ExpiresAt(issuedAt, p)
	if err != nil {
		return err
	}
	if !expiresAt.Equal(want) {
		return fmt.Errorf("%w: expires_at must be issued_at + %s", ErrInvalidWindow, want.Sub(issuedAt.UTC()))
	}
	return nil
}

// CheckWindow refuses an action at or after expiry: the window is half-open [issued, expires).
func CheckWindow(now, expiresAt time.Time) error {
	if !now.Before(expiresAt) {
		return ErrExpired
	}
	return nil
}

// DecisionKind is what a reviewer records.
type DecisionKind string

const (
	DecideApprove DecisionKind = "approve"
	DecideReject  DecisionKind = "reject"
)

// Decision is the validated body of POST /proposals/{id}/decisions.
type Decision struct {
	Kind             DecisionKind
	ExpectedRevision int64
	Reason           string
}

// Proposal is the current server state of one proposal revision.
type Proposal struct {
	State     State
	Revision  int64
	ExpiresAt time.Time
	Authority ProposalAuthority
}

// MaxReasonRunes bounds a decision reason.
const MaxReasonRunes = 2000

// Decide runs every decision rule in a fixed order and returns the next state:
// malformed request (422), stale revision (412), not pending (409), expired, then reviewer
// eligibility (403). A caller cannot approve without passing all of them.
func Decide(now time.Time, p Proposal, r Reviewer, d Decision) (State, error) {
	var event Event
	switch d.Kind {
	case DecideApprove:
		event = Approve
	case DecideReject:
		event = Reject
	default:
		return p.State, fmt.Errorf("%w: decision must be approve or reject", ErrInvalidDecision)
	}
	if d.ExpectedRevision < 1 {
		return p.State, fmt.Errorf("%w: expected_revision must be >= 1", ErrInvalidDecision)
	}
	if err := validText(d.Reason, MaxReasonRunes); err != nil {
		return p.State, fmt.Errorf("%w: reason: %v", ErrInvalidDecision, err)
	}
	if d.ExpectedRevision != p.Revision {
		return p.State, ErrRevisionConflict
	}
	if p.State != Pending {
		return p.State, ErrIllegalTransition
	}
	if err := CheckWindow(now, p.ExpiresAt); err != nil {
		return p.State, err
	}
	if err := CheckReviewer(r, p.Authority); err != nil {
		return p.State, err
	}
	return Apply(p.State, event)
}

// CheckDispatch is the executor's independent re-evaluation before it consumes an approval:
// the proposal is still approved, the window is still open, the stored digest still matches
// the payload, and every approver is still an eligible human with the capability today.
func CheckDispatch(now time.Time, p Proposal, approvers []Reviewer, storedDigest, currentDigest string) (State, error) {
	if p.State != Approved {
		return p.State, ErrIllegalTransition
	}
	if err := CheckWindow(now, p.ExpiresAt); err != nil {
		return p.State, err
	}
	if storedDigest == "" || storedDigest != currentDigest {
		return p.State, ErrDigestMismatch
	}
	if len(approvers) == 0 {
		return p.State, ErrNotEligible
	}
	for _, r := range approvers {
		if err := checkPerson(r, p.Authority); err != nil {
			return p.State, err
		}
	}
	return Apply(p.State, Consume)
}

// PayloadSchemaVersion versions the canonical approval payload.
const PayloadSchemaVersion = 1

// Ref pins one versioned input: an evidence revision or a prerequisite version.
type Ref struct {
	Kind     string `json:"kind"`
	ID       string `json:"id"`
	Revision int64  `json:"revision"`
}

// Payload is the exact action a human approves. Every field is part of the digest.
type Payload struct {
	ActionKind        string
	Template          string
	ProposalID        string
	ProposalRevision  int64
	AssetID           string
	LineID            string
	OrderID           string
	AssigneeID        string
	Instructions      string
	DueAt             time.Time
	CaseID            string
	CaseRevision      int64
	ContextSnapshotID string
	ContextRevision   int64
	ContextDigest     string
	ConfigRevision    int64
	GraphRevision     int64
	PolicyRevision    int64
	Prerequisites     []Ref
	Evidence          []Ref
	IssuedAt          time.Time
	ExpiresAt         time.Time
	GeneratedByRun    string
}

// canonicalPayload is the schema-ordered serialized form: times are fixed-width UTC strings
// with microsecond precision (what PostgreSQL timestamptz stores) and refs are sorted.
type canonicalPayload struct {
	ActionKind        string `json:"action_kind"`
	Template          string `json:"template"`
	ProposalID        string `json:"proposal_id"`
	ProposalRevision  int64  `json:"proposal_revision"`
	AssetID           string `json:"asset_id"`
	LineID            string `json:"line_id"`
	OrderID           string `json:"order_id"`
	AssigneeID        string `json:"assignee_id"`
	Instructions      string `json:"instructions"`
	DueAt             string `json:"due_at"`
	CaseID            string `json:"case_id"`
	CaseRevision      int64  `json:"case_revision"`
	ContextSnapshotID string `json:"context_snapshot_id"`
	ContextRevision   int64  `json:"context_revision"`
	ContextDigest     string `json:"context_digest"`
	ConfigRevision    int64  `json:"config_revision"`
	GraphRevision     int64  `json:"graph_revision"`
	PolicyRevision    int64  `json:"policy_revision"`
	Prerequisites     []Ref  `json:"prerequisite_versions"`
	Evidence          []Ref  `json:"evidence_revisions"`
	IssuedAt          string `json:"issued_at"`
	ExpiresAt         string `json:"expires_at"`
	GeneratedByRun    string `json:"generated_by_run_id"`
}

const (
	v1ActionKind        = "maintenance_investigation.create"
	v1Template          = "inspect_report_only"
	MaxInstructionRunes = 4000
	timeLayout          = "2006-01-02T15:04:05.000000Z"
)

var (
	digestRE  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	refKindRE = regexp.MustCompile(`^[a-z][a-z0-9_.]{0,63}$`)
)

// Canonical validates the payload against the v1 contract and the window profile and returns
// the canonical bytes and their SHA-256 digest within one tenant and site. Instructions must
// already be the reviewed text; never hash raw model prose as authority.
func Canonical(tenantID, siteID string, p Payload, profile WindowProfile) ([]byte, string, error) {
	body, err := normalize(p, profile)
	if err != nil {
		return nil, "", err
	}
	return canonical.Envelope("ApprovedActionV1", PayloadSchemaVersion, tenantID, siteID, body)
}

// Digest returns only the digest from Canonical.
func Digest(tenantID, siteID string, p Payload, profile WindowProfile) (string, error) {
	_, digest, err := Canonical(tenantID, siteID, p, profile)
	return digest, err
}

func normalize(p Payload, profile WindowProfile) (canonicalPayload, error) {
	bad := func(format string, args ...any) (canonicalPayload, error) {
		return canonicalPayload{}, fmt.Errorf("%w: "+format, append([]any{ErrInvalidPayload}, args...)...)
	}
	if p.ActionKind != v1ActionKind || p.Template != v1Template {
		return bad("action is outside the v1 scope")
	}
	for name, id := range map[string]string{
		"proposal_id": p.ProposalID, "asset_id": p.AssetID, "line_id": p.LineID, "order_id": p.OrderID,
		"assignee_id": p.AssigneeID, "case_id": p.CaseID, "context_snapshot_id": p.ContextSnapshotID,
		"generated_by_run_id": p.GeneratedByRun,
	} {
		if !canonicalUUID(id) {
			return bad("%s must be a lowercase canonical UUID", name)
		}
	}
	for name, revision := range map[string]int64{
		"proposal_revision": p.ProposalRevision, "case_revision": p.CaseRevision, "context_revision": p.ContextRevision,
		"config_revision": p.ConfigRevision, "graph_revision": p.GraphRevision, "policy_revision": p.PolicyRevision,
	} {
		if revision < 1 {
			return bad("%s must be >= 1", name)
		}
	}
	if !digestRE.MatchString(p.ContextDigest) {
		return bad("context_digest must be 64 lowercase hex characters")
	}
	if err := validText(p.Instructions, MaxInstructionRunes); err != nil {
		return bad("instructions: %v", err)
	}
	issued, err := canonicalTime(p.IssuedAt)
	if err != nil {
		return bad("issued_at: %v", err)
	}
	expires, err := canonicalTime(p.ExpiresAt)
	if err != nil {
		return bad("expires_at: %v", err)
	}
	due, err := canonicalTime(p.DueAt)
	if err != nil {
		return bad("due_at: %v", err)
	}
	if !p.DueAt.After(p.IssuedAt) {
		return bad("due_at must be after issued_at")
	}
	if err = ValidateWindow(p.IssuedAt, p.ExpiresAt, profile); err != nil {
		return canonicalPayload{}, err
	}
	prerequisites, err := canonicalRefs(p.Prerequisites)
	if err != nil {
		return bad("prerequisite_versions: %v", err)
	}
	evidence, err := canonicalRefs(p.Evidence)
	if err != nil {
		return bad("evidence_revisions: %v", err)
	}
	if len(evidence) == 0 {
		return bad("evidence_revisions must cite at least one evidence revision")
	}
	return canonicalPayload{
		ActionKind: p.ActionKind, Template: p.Template,
		ProposalID: p.ProposalID, ProposalRevision: p.ProposalRevision,
		AssetID: p.AssetID, LineID: p.LineID, OrderID: p.OrderID, AssigneeID: p.AssigneeID,
		Instructions: p.Instructions, DueAt: due,
		CaseID: p.CaseID, CaseRevision: p.CaseRevision,
		ContextSnapshotID: p.ContextSnapshotID, ContextRevision: p.ContextRevision, ContextDigest: p.ContextDigest,
		ConfigRevision: p.ConfigRevision, GraphRevision: p.GraphRevision, PolicyRevision: p.PolicyRevision,
		Prerequisites: prerequisites, Evidence: evidence,
		IssuedAt: issued, ExpiresAt: expires, GeneratedByRun: p.GeneratedByRun,
	}, nil
}

func canonicalUUID(v string) bool {
	id, err := uuid.Parse(v)
	return err == nil && id != uuid.Nil && id.String() == v
}

// canonicalTime rejects the zero time and sub-microsecond precision, which PostgreSQL would
// silently drop and so change the digest after a round trip.
func canonicalTime(t time.Time) (string, error) {
	if t.IsZero() {
		return "", errors.New("time is required")
	}
	if t.Nanosecond()%int(time.Microsecond) != 0 {
		return "", errors.New("time must have at most microsecond precision")
	}
	return t.UTC().Format(timeLayout), nil
}

// validText requires valid UTF-8, 1..max characters (runes, not bytes), some non-space
// content and no control characters other than tab and newline.
func validText(s string, max int) error {
	if !utf8.ValidString(s) {
		return errors.New("must be valid UTF-8")
	}
	if n := utf8.RuneCountInString(s); n < 1 || n > max {
		return fmt.Errorf("must be 1 to %d characters", max)
	}
	if strings.TrimSpace(s) == "" {
		return errors.New("must not be blank")
	}
	for _, r := range s {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return errors.New("must not contain control characters")
		}
	}
	return nil
}

func canonicalRefs(refs []Ref) ([]Ref, error) {
	out := make([]Ref, len(refs))
	copy(out, refs)
	for _, r := range out {
		if !refKindRE.MatchString(r.Kind) || !canonicalUUID(r.ID) || r.Revision < 1 {
			return nil, fmt.Errorf("invalid ref %+v", r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].ID < out[j].ID
	})
	for i := 1; i < len(out); i++ {
		if out[i].Kind == out[i-1].Kind && out[i].ID == out[i-1].ID {
			return nil, fmt.Errorf("duplicate ref %s/%s", out[i].Kind, out[i].ID)
		}
	}
	return out, nil
}
