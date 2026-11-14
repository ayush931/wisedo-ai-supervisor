package cases

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/approvals"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/plantcontext"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/session"
	"github.com/google/uuid"
)

// Store errors. ErrIllegalTransition and ErrGuardFailed from the pure state machine pass
// through unchanged (409).
var (
	ErrNotFound         = errors.New("case unavailable")
	ErrDenied           = errors.New("case permission denied")
	ErrReauthentication = errors.New("recent authentication required")
	ErrInvalid          = errors.New("invalid case request")
	ErrIdempotency      = errors.New("idempotency key or source event reused for a different request")
	ErrRevision         = errors.New("case revision changed")
	// ErrNotQualified means the trigger's context is not an eligible READY window that, with
	// the previous window, meets the shortfall rule. Nothing is written.
	ErrNotQualified = errors.New("trigger does not qualify as a production shortfall")
	// ErrActiveEpisode means a reopen would start a second nonterminal episode for the key,
	// or the case is not the latest episode of its key.
	ErrActiveEpisode = errors.New("an active or later episode exists for this line and order")
	errIntegrity     = errors.New("case integrity check failed")
)

// ExceptionKind is the only v1 exception kind.
const ExceptionKind = "production_shortfall"

// MaxTriggerEvidence bounds trigger_evidence_ids per case (migration 027 checks it too).
const MaxTriggerEvidence = 100

// MaxReasonRunes bounds a lifecycle reason.
const MaxReasonRunes = 2000

// Scope is the verified tenant and the path site.
type Scope struct{ TenantID, SiteID string }

// Principal is an authenticated browser principal.
type Principal struct{ Issuer, Subject, SessionHash string }

// ServiceActor is the server-side identity a trigger is recorded under. It is supplied by
// trusted Go code (the shortfall evaluator or ingestion), never by a request body.
type ServiceActor struct{ Issuer, Subject string }

// TriggerInput is the WIS-273 trigger: one operational source event (its record_id from the
// context's source_record_refs) and the context it qualified.
type TriggerInput struct {
	SourceID          string `json:"source_id"`
	SourceEventID     string `json:"source_event_id"`
	ContextSnapshotID string `json:"context_snapshot_id"`
	ExceptionKind     string `json:"exception_kind"`
}

// TriggerResult names the case a trigger opened or joined.
type TriggerResult struct {
	CaseID   string `json:"case_id"`
	Revision int64  `json:"revision"`
	Opened   bool   `json:"opened"`
	Replayed bool   `json:"replayed"`
}

// TransitionV1 is one recorded change of a case.
type TransitionV1 struct {
	Revision  int64     `json:"revision"`
	FromState string    `json:"from_state"`
	ToState   State     `json:"to_state"`
	Event     string    `json:"event"`
	Reason    *string   `json:"reason"`
	Actor     ActorRef  `json:"actor"`
	CreatedAt time.Time `json:"created_at"`
}

// TriggerV1 is one qualifying shortfall recorded against the case (migration 045 ledger): the
// one that opened it, or a later one that joined the active episode. A join changes no state,
// so it is shown here rather than silently absorbed (rule 06).
type TriggerV1 struct {
	SourceEventID     string    `json:"source_event_id"`
	ContextSnapshotID string    `json:"context_snapshot_id"`
	Opened            bool      `json:"opened"`
	Actor             ActorRef  `json:"actor"`
	CreatedAt         time.Time `json:"created_at"`
}

// ActorRef is a stored identity.
type ActorRef struct {
	Issuer  string `json:"issuer"`
	Subject string `json:"subject"`
}

// CaseV1 is the WIS-273 case contract with its ordered transition history.
type CaseV1 struct {
	ID                 string            `json:"id"`
	SiteID             string            `json:"site_id"`
	LineID             string            `json:"line_id"`
	OrderID            string            `json:"order_id"`
	ExceptionKind      string            `json:"exception_kind"`
	Episode            int64             `json:"episode"`
	PreviousCaseID     *string           `json:"previous_case_id"`
	Revision           int64             `json:"revision"`
	ContextSnapshotID  string            `json:"context_snapshot_id"`
	ContextRevision    int64             `json:"context_revision"`
	ContextDigest      string            `json:"context_digest" contract:"sha256"`
	OwnerUserID        *string           `json:"owner_user_id"`
	State              State             `json:"state"`
	DetectedAt         time.Time         `json:"detected_at"`
	LatestWindowEnd    time.Time         `json:"latest_window_end"`
	TriggerEvidenceIDs []string          `json:"trigger_evidence_ids"`
	Triggers           []TriggerV1       `json:"triggers"`
	WorkflowID         string            `json:"workflow_id"`
	ResolutionReason   *string           `json:"resolution_reason"`
	Transitions        []TransitionV1    `json:"transitions"`
	Clarifications     []ClarificationV1 `json:"clarifications"`
	Replayed           bool              `json:"replayed"`
}

// SummaryV1 is one row of GET /cases.
type SummaryV1 struct {
	ID              string    `json:"id"`
	LineID          string    `json:"line_id"`
	OrderID         string    `json:"order_id"`
	Episode         int64     `json:"episode"`
	Revision        int64     `json:"revision"`
	State           State     `json:"state"`
	DetectedAt      time.Time `json:"detected_at"`
	LatestWindowEnd time.Time `json:"latest_window_end"`
}

// PageV1 is a cursor page of cases ordered by ID.
type PageV1 struct {
	Items      []SummaryV1 `json:"items"`
	NextCursor *string     `json:"next_cursor"`
}

// CancelInput is the POST /cases/{id}/cancel body.
type CancelInput struct {
	ExpectedRevision int64  `json:"expected_revision"`
	Reason           string `json:"reason"`
}

// ResumeInput is the POST /cases/{id}/resume body: an escalated case returns to
// investigating bound to a new eligible context.
type ResumeInput struct {
	ExpectedRevision int64  `json:"expected_revision"`
	Reason           string `json:"reason"`
	NewContextID     string `json:"new_context_id"`
}

// ReopenInput is the POST /cases/{id}/reopen body: a resolved or canceled case starts a new
// episode that references it; its own history stays immutable.
type ReopenInput struct {
	ExpectedRevision int64  `json:"expected_revision"`
	Reason           string `json:"reason"`
}

// CloseInput is POST /cases/{id}/close: a supervisor resolves an observing or escalated case
// after viewing its task and measured outcome. The reason is stored as the resolution reason; it
// never replaces or edits the measured outcome.
type CloseInput struct {
	ExpectedRevision int64  `json:"expected_revision"`
	Reason           string `json:"reason" contract:"max=2000"`
}

const (
	DefaultPageSize = 50
	MaxPageSize     = 100
)

// Service persists cases. The transition rules are the pure Apply; migration 027 fences the
// same pairs in SQL.
type Service struct {
	db       *sql.DB
	sessions session.Policy
	// boundEvidence re-evaluates an approved payload's bound evidence revisions at dispatch. The
	// worker supplies it (the evidence rules live in the agent runtime); the executor refuses
	// every dispatch without it.
	boundEvidence BoundEvidenceCheck
}

// BoundEvidenceCheck returns a dispatch refusal code when any evidence revision the payload binds
// is no longer current, inside the executor's transaction.
type BoundEvidenceCheck func(ctx context.Context, tx *sql.Tx, tenantID, siteID string, p approvals.Payload) (code string, err error)

// WithBoundEvidence returns a copy whose executor re-evaluates bound evidence with check.
func (s *Service) WithBoundEvidence(check BoundEvidenceCheck) *Service {
	c := *s
	c.boundEvidence = check
	return &c
}

func NewService(db *sql.DB) *Service { return &Service{db: db, sessions: session.Default()} }

// WithSessionPolicy returns a copy that enforces p (WIS-290 shared session policy).
func (s *Service) WithSessionPolicy(p session.Policy) *Service {
	c := *s
	c.sessions = p.OrDefault()
	return &c
}

func validID(v string) bool {
	id, e := uuid.Parse(v)
	return e == nil && id != uuid.Nil && id.String() == v
}

func validField(v string, max int) bool {
	return len(v) > 0 && len(v) <= max && strings.TrimSpace(v) == v && utf8.ValidString(v) && !strings.ContainsRune(v, 0)
}

// validReason requires 1..MaxReasonRunes characters with some non-space content.
func validReason(v string) bool {
	n := utf8.RuneCountInString(v)
	return utf8.ValidString(v) && n >= 1 && n <= MaxReasonRunes && strings.TrimSpace(v) != "" && !strings.ContainsRune(v, 0)
}

func requestDigest(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// eventNamespace derives deterministic outbox event IDs from a case revision.
var eventNamespace = uuid.MustParse("6b1f6a0e-4f0c-5d6e-9a3e-273000000001")

func transitionEvent(caseID string, revision int64) string {
	return uuid.NewSHA1(eventNamespace, []byte(caseID+"|"+strconv.FormatInt(revision, 10))).String()
}

func scopeTx(ctx context.Context, tx *sql.Tx, sc Scope, write bool) (time.Time, error) {
	var now time.Time
	if err := tx.QueryRowContext(ctx, `SELECT clock_timestamp() FROM (SELECT set_config('supervisor.tenant_id',$1,true),set_config('statement_timeout','10s',true),set_config('lock_timeout','5s',true)) c`, sc.TenantID).Scan(&now); err != nil {
		return now, err
	}
	var found bool
	if err := tx.QueryRowContext(ctx, `SELECT supervisor.lock_evidence_site($1,$2,$3)`, sc.TenantID, sc.SiteID, write).Scan(&found); err != nil {
		return now, err
	}
	if !found {
		return now, ErrNotFound
	}
	return now, nil
}

// memberScopeTx is scopeTx for a browser principal: lock_member_site refuses a non-member of the
// site before taking its lock, so a member of another site cannot hold up this site's writers
// (WIS-699). Worker paths, which have no member actor, keep scopeTx.
func memberScopeTx(ctx context.Context, tx *sql.Tx, sc Scope, a Principal, write bool) (time.Time, error) {
	var now time.Time
	if err := tx.QueryRowContext(ctx, `SELECT clock_timestamp() FROM (SELECT set_config('supervisor.tenant_id',$1,true),set_config('statement_timeout','10s',true),set_config('lock_timeout','5s',true)) c`, sc.TenantID).Scan(&now); err != nil {
		return now, err
	}
	var found bool
	if err := tx.QueryRowContext(ctx, `SELECT supervisor.lock_member_site($1,$2,$3,$4,$5)`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, write).Scan(&found); err != nil {
		return now, err
	}
	if !found {
		return now, ErrNotFound
	}
	return now, nil
}

type access int

const (
	read access = iota
	supervise
	// clarify: an active site supervisor or operator supplies the input a case waits on. It is
	// not a privileged decision, so no recent authentication; owner is not an implicit bypass.
	clarify
)

// begin opens a tenant transaction for a verified browser principal: session, site lock,
// active membership. Lifecycle mutations need the supervisor role and a recent
// authentication; owner is not an implicit bypass.
func (s *Service) begin(ctx context.Context, a Principal, sc Scope, mode access) (*sql.Tx, time.Time, error) {
	if !validID(sc.TenantID) || !validID(sc.SiteID) || !validField(a.Issuer, 255) || !validField(a.Subject, 255) || !validField(a.SessionHash, 128) {
		return nil, time.Time{}, ErrDenied
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, time.Time{}, err
	}
	fail := func(e error) (*sql.Tx, time.Time, error) {
		_ = tx.Rollback()
		return nil, time.Time{}, e
	}
	var issuer, subject string
	var expiry, seen, auth, now time.Time
	if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, sc.TenantID); err != nil {
		return fail(err)
	}
	err = tx.QueryRowContext(ctx, `SELECT issuer,subject,expires_at,last_seen_at,auth_time,clock_timestamp() FROM supervisor.browser_sessions WHERE token_hash=$1 FOR SHARE`, a.SessionHash).Scan(&issuer, &subject, &expiry, &seen, &auth, &now)
	if err != nil || issuer != a.Issuer || subject != a.Subject || !expiry.After(now) || !seen.After(now.Add(-s.sessions.Idle)) {
		return fail(ErrDenied)
	}
	if now, err = memberScopeTx(ctx, tx, sc, a, mode != read); err != nil {
		return fail(err)
	}
	var role string
	err = tx.QueryRowContext(ctx, `SELECT role FROM supervisor.memberships WHERE tenant_id=$1 AND site_id=$2 AND issuer=$3 AND subject=$4 AND active FOR SHARE`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return fail(ErrNotFound)
	}
	if err != nil {
		return fail(err)
	}
	if mode == clarify && role != "supervisor" && role != "operator" {
		return fail(ErrDenied)
	}
	if mode == supervise {
		if role != "supervisor" {
			return fail(ErrDenied)
		}
		if auth.After(now) || auth.Before(now.Add(-s.sessions.RecentAuth)) {
			return fail(ErrReauthentication)
		}
	}
	return tx, now, nil
}

// finish re-checks the session inside the transaction before committing.
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

// eligibleReady loads a context and requires it READY and eligible now.
func eligibleReady(ctx context.Context, tx *sql.Tx, sc Scope, id string, now time.Time) (plantcontext.View, error) {
	view, err := plantcontext.LoadTx(ctx, tx, plantcontext.Scope{TenantID: sc.TenantID, SiteID: sc.SiteID}, id, now)
	if errors.Is(err, plantcontext.ErrNotFound) {
		return view, ErrInvalid
	}
	if err != nil {
		return view, err
	}
	if view.Readiness != plantcontext.ReadyState || !view.CurrentStatus.Eligible {
		return view, ErrNotQualified
	}
	return view, nil
}

// Trigger opens a case for a qualifying shortfall, or joins the active episode for the same
// line, order and kind. It is idempotent per source event: the same event with the same
// request replays; with a different request it is ErrIdempotency. The case, its evidence, the
// trigger record and the outbox row commit together; there is no HTTP route until service
// principals exist (WIS-290), so only trusted Go code calls it.
func (s *Service) Trigger(ctx context.Context, sc Scope, actor ServiceActor, in TriggerInput) (TriggerResult, error) {
	var out TriggerResult
	if !validID(sc.TenantID) || !validID(sc.SiteID) || !validField(actor.Issuer, 255) || !validField(actor.Subject, 255) ||
		!validID(in.SourceID) || !validID(in.SourceEventID) || !validID(in.ContextSnapshotID) || in.ExceptionKind != ExceptionKind {
		return out, ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	now, err := scopeTx(ctx, tx, sc, true)
	if err != nil {
		return out, err
	}
	hash := requestDigest(in)
	if out, found, err := storedTrigger(ctx, tx, sc, in, hash); err != nil || found {
		if err != nil {
			return out, err
		}
		return out, tx.Commit()
	}
	current, err := currentTx(ctx, tx, sc, in.ContextSnapshotID, now)
	if err != nil {
		return out, err
	}
	// The source event must be a delta record this context selected.
	sequence := deltaSequences(current)[in.SourceID+"/"+in.SourceEventID]
	if sequence == 0 {
		return out, ErrInvalid
	}
	windowEnd, err := qualifiesTx(ctx, tx, sc, current, now)
	if err != nil {
		return out, err
	}
	if out, err = attachTx(ctx, tx, sc, actor, current, windowEnd, in, sequence, hash, now); err != nil {
		return out, err
	}
	return out, tx.Commit()
}

// triggerContext records a trigger for every delta record of one READY context in one transaction
// (WIS-718). Trigger, called once per record, re-assembled the current and previous windows for
// each of up to plantcontext.MaxRecords (10,000) records, so one context cost O(n^2) inside the
// serial worker Tick. The context is now validated once and each new record is attached as
// Trigger would attach it: the same trigger rows, request digests, evidence cap and outbox
// signals. Records that already triggered are recognised first, without validating the context
// again.
func (s *Service) triggerContext(ctx context.Context, sc Scope, actor ServiceActor, contextID string, refs []plantcontext.RecordRef) (string, string, error) {
	if !validID(sc.TenantID) || !validID(sc.SiteID) || !validField(actor.Issuer, 255) || !validField(actor.Subject, 255) || !validID(contextID) {
		return OutcomeNotApplicable, "", nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback()
	now, err := scopeTx(ctx, tx, sc, true)
	if err != nil {
		return "", "", err
	}
	type pending struct {
		in   TriggerInput
		hash string
	}
	var fresh []pending
	already := false
	for _, ref := range refs {
		in := TriggerInput{SourceID: ref.SourceID, SourceEventID: ref.RecordID, ContextSnapshotID: contextID, ExceptionKind: ExceptionKind}
		if !validID(in.SourceID) || !validID(in.SourceEventID) {
			return OutcomeNotApplicable, "", nil
		}
		hash := requestDigest(in)
		_, found, err := storedTrigger(ctx, tx, sc, in, hash)
		switch {
		case found, errors.Is(err, ErrIdempotency):
			// This record already triggered, through this or another context of its window.
			already = true
		case err != nil:
			return "", "", err
		default:
			fresh = append(fresh, pending{in, hash})
		}
	}
	if len(fresh) == 0 {
		if !already {
			return OutcomeNotApplicable, "", nil
		}
		return OutcomeAlreadyTriggered, "", tx.Commit()
	}
	// A READY context that is no longer eligible (superseded, inputs changed, source revoked)
	// is a recorded not_qualified outcome, not an error the evaluator would retry every pass.
	current, err := currentTx(ctx, tx, sc, contextID, now)
	var windowEnd time.Time
	if err == nil {
		windowEnd, err = qualifiesTx(ctx, tx, sc, current, now)
	}
	switch {
	case errors.Is(err, ErrNotQualified):
		return OutcomeNotQualified, "", nil
	case errors.Is(err, ErrInvalid):
		return OutcomeNotApplicable, "", nil
	case err != nil:
		return "", "", err
	}
	sequences := deltaSequences(current)
	opened, joined := "", ""
	for _, p := range fresh {
		sequence := sequences[p.in.SourceID+"/"+p.in.SourceEventID]
		if sequence == 0 {
			return OutcomeNotApplicable, "", nil
		}
		result, err := attachTx(ctx, tx, sc, actor, current, windowEnd, p.in, sequence, p.hash, now)
		if err != nil {
			return "", "", err
		}
		if result.Opened {
			opened = result.CaseID
		} else {
			joined = result.CaseID
		}
	}
	if err = tx.Commit(); err != nil {
		return "", "", err
	}
	if opened != "" {
		return OutcomeOpened, opened, nil
	}
	return OutcomeJoined, joined, nil
}

// storedTrigger returns the recorded trigger for the source event, if any: a replay when the
// request digest matches, ErrIdempotency when it does not.
func storedTrigger(ctx context.Context, tx *sql.Tx, sc Scope, in TriggerInput, hash string) (TriggerResult, bool, error) {
	var out TriggerResult
	var stored, storedCase string
	var opened bool
	err := tx.QueryRowContext(ctx, `SELECT request_sha256,case_id,opened FROM supervisor.case_triggers WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3 AND source_event_id=$4`, sc.TenantID, sc.SiteID, in.SourceID, in.SourceEventID).Scan(&stored, &storedCase, &opened)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return out, false, nil
	case err != nil:
		return out, false, err
	case stored != hash:
		return out, false, ErrIdempotency
	}
	out = TriggerResult{CaseID: storedCase, Opened: opened, Replayed: true}
	if err = tx.QueryRowContext(ctx, `SELECT revision FROM supervisor.cases WHERE tenant_id=$1 AND site_id=$2 AND id=$3`, sc.TenantID, sc.SiteID, storedCase).Scan(&out.Revision); err != nil {
		return TriggerResult{}, false, err
	}
	return out, true, nil
}

// currentTx loads the context a trigger cites. A case belongs to a line: the context's asset must
// be an asset_kind=line entity (WIS-273: line_id maps to the context asset_id; a machine-level
// context opens nothing).
func currentTx(ctx context.Context, tx *sql.Tx, sc Scope, contextID string, now time.Time) (plantcontext.View, error) {
	var assetKind sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT r.payload->>'asset_kind' FROM supervisor.plant_contexts c JOIN supervisor.knowledge_entity_revisions r ON (r.tenant_id,r.site_id,r.id)=(c.tenant_id,c.site_id,c.asset_id) WHERE c.tenant_id=$1 AND c.site_id=$2 AND c.id=$3 AND r.entity_type='asset' ORDER BY r.revision DESC LIMIT 1`, sc.TenantID, sc.SiteID, contextID).Scan(&assetKind)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && assetKind.String != "line") {
		return plantcontext.View{}, ErrInvalid
	}
	if err != nil {
		return plantcontext.View{}, err
	}
	return eligibleReady(ctx, tx, sc, contextID, now)
}

// deltaSequences maps source/record to the source sequence of each delta record the context selected.
func deltaSequences(current plantcontext.View) map[string]int64 {
	out := map[string]int64{}
	for _, ref := range current.DeltaRefs() {
		out[ref.SourceID+"/"+ref.RecordID] = ref.SourceSequence
	}
	return out
}

// qualifiesTx requires two consecutive eligible READY windows under the same process snapshot that
// together fall short, and returns the current window's end.
func qualifiesTx(ctx context.Context, tx *sql.Tx, sc Scope, current plantcontext.View, now time.Time) (time.Time, error) {
	start, err := time.Parse(time.RFC3339, current.Window.Start)
	if err != nil {
		return time.Time{}, errIntegrity
	}
	pc := plantcontext.Scope{TenantID: sc.TenantID, SiteID: sc.SiteID}
	previousID, err := plantcontext.HeadTx(ctx, tx, pc, current.OrderID, current.AssetID, start.Add(-15*time.Minute).UTC().Format(time.RFC3339), current.ProcessSnapshotID)
	if err != nil {
		return time.Time{}, err
	}
	if previousID == "" {
		return time.Time{}, ErrNotQualified
	}
	previous, err := eligibleReady(ctx, tx, sc, previousID, now)
	if errors.Is(err, ErrInvalid) {
		return time.Time{}, ErrNotQualified
	}
	if err != nil {
		return time.Time{}, err
	}
	if !plantcontext.EvaluateShortfall(previous.Snapshot, current.Snapshot).Qualifies {
		return time.Time{}, ErrNotQualified
	}
	windowEnd, err := time.Parse(time.RFC3339, current.Window.End)
	if err != nil {
		return time.Time{}, errIntegrity
	}
	return windowEnd, nil
}

// attachTx opens the line's case or joins its active episode for one qualified source event:
// it cites the record while the case has room, records the trigger and signals the workflow.
func attachTx(ctx context.Context, tx *sql.Tx, sc Scope, actor ServiceActor, current plantcontext.View, windowEnd time.Time, in TriggerInput, sequence int64, hash string, now time.Time) (TriggerResult, error) {
	var out TriggerResult
	var err error
	var caseID string
	var revision int64
	err = tx.QueryRowContext(ctx, `SELECT id,revision FROM supervisor.cases WHERE tenant_id=$1 AND site_id=$2 AND line_id=$3 AND order_id=$4 AND exception_kind=$5 AND state NOT IN ('resolved','canceled') FOR UPDATE`, sc.TenantID, sc.SiteID, current.AssetID, current.OrderID, ExceptionKind).Scan(&caseID, &revision)
	switch {
	case err == nil:
		// Deduplicated onto the active episode: record the trigger, cite its record while the
		// case has room, and signal the workflow.
		var cited int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.case_trigger_evidence WHERE tenant_id=$1 AND site_id=$2 AND case_id=$3`, sc.TenantID, sc.SiteID, caseID).Scan(&cited); err != nil {
			return out, err
		}
		if cited < MaxTriggerEvidence {
			if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.case_trigger_evidence(tenant_id,site_id,case_id,source_id,source_sequence) VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, sc.TenantID, sc.SiteID, caseID, in.SourceID, sequence); err != nil {
				return out, err
			}
		}
		out = TriggerResult{CaseID: caseID, Revision: revision}
	case errors.Is(err, sql.ErrNoRows):
		caseID, revision = uuid.NewString(), 1
		var episode int64 = 1
		var previousCase any
		var last string
		var lastEpisode int64
		e := tx.QueryRowContext(ctx, `SELECT id,episode FROM supervisor.cases WHERE tenant_id=$1 AND site_id=$2 AND line_id=$3 AND order_id=$4 AND exception_kind=$5 ORDER BY episode DESC LIMIT 1`, sc.TenantID, sc.SiteID, current.AssetID, current.OrderID, ExceptionKind).Scan(&last, &lastEpisode)
		if e == nil {
			episode, previousCase = lastEpisode+1, last
		} else if !errors.Is(e, sql.ErrNoRows) {
			return out, e
		}
		if err = insertCase(ctx, tx, sc, caseID, current.AssetID, current.OrderID, episode, previousCase, current.ContextID, current.Digest, now, windowEnd); err != nil {
			return out, err
		}
		if err = recordTransition(ctx, tx, sc, caseID, 1, "none", Detected, "trigger", nil, ActorRef(actor)); err != nil {
			return out, err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.case_trigger_evidence(tenant_id,site_id,case_id,source_id,source_sequence) VALUES($1,$2,$3,$4,$5)`, sc.TenantID, sc.SiteID, caseID, in.SourceID, sequence); err != nil {
			return out, err
		}
		out = TriggerResult{CaseID: caseID, Revision: 1, Opened: true}
	default:
		return out, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.case_triggers(tenant_id,site_id,source_id,source_event_id,source_sequence,context_snapshot_id,exception_kind,request_sha256,case_id,opened,actor_issuer,actor_subject) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		sc.TenantID, sc.SiteID, in.SourceID, in.SourceEventID, sequence, in.ContextSnapshotID, ExceptionKind, hash, caseID, out.Opened, actor.Issuer, actor.Subject); err != nil {
		return out, err
	}
	kind := "signal"
	if out.Opened {
		kind = "start"
	}
	if err = outbox(ctx, tx, sc, caseID, out.Revision, kind, in.SourceEventID); err != nil {
		return out, err
	}
	return out, nil
}

func insertCase(ctx context.Context, tx *sql.Tx, sc Scope, id, lineID, orderID string, episode int64, previous any, contextID, digest string, detectedAt, windowEnd time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO supervisor.cases(tenant_id,site_id,id,line_id,order_id,exception_kind,episode,previous_case_id,revision,context_snapshot_id,context_revision,context_digest,state,detected_at,latest_window_end,workflow_id)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,1,$9,1,$10,'detected',$11,$12,$13)`,
		sc.TenantID, sc.SiteID, id, lineID, orderID, ExceptionKind, episode, previous, contextID, digest, detectedAt, windowEnd, WorkflowID(sc.TenantID, id))
	return err
}

// WorkflowID is the stable Temporal workflow ID for a case.
func WorkflowID(tenantID, caseID string) string { return "case:" + tenantID + ":" + caseID }

func recordTransition(ctx context.Context, tx *sql.Tx, sc Scope, caseID string, revision int64, from string, to State, event string, reason *string, actor ActorRef) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO supervisor.case_transitions(tenant_id,site_id,case_id,revision,from_state,to_state,event,reason,actor_issuer,actor_subject) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		sc.TenantID, sc.SiteID, caseID, revision, from, string(to), event, reason, actor.Issuer, actor.Subject)
	return err
}

func outbox(ctx context.Context, tx *sql.Tx, sc Scope, caseID string, revision int64, kind, eventID string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO supervisor.case_outbox(tenant_id,site_id,id,case_id,case_revision,kind,event_id) VALUES($1,$2,$3,$4,$5,$6,$7)`,
		sc.TenantID, sc.SiteID, uuid.NewString(), caseID, revision, kind, eventID)
	return err
}

// Get returns a case with its transitions and trigger evidence.
func (s *Service) Get(ctx context.Context, a Principal, sc Scope, id string) (CaseV1, error) {
	if !validID(id) {
		return CaseV1{}, ErrNotFound
	}
	tx, _, err := s.begin(ctx, a, sc, read)
	if err != nil {
		return CaseV1{}, err
	}
	defer tx.Rollback()
	out, err := load(ctx, tx, sc, id)
	if err != nil {
		return out, err
	}
	return out, s.finish(ctx, tx, a)
}

// ReadTx runs fn inside one read of the site under the same session, active-membership and tenant
// checks as Get. The WIS-629 advisor suggestion is assembled through it; fn must not write.
func (s *Service) ReadTx(ctx context.Context, a Principal, sc Scope, fn func(*sql.Tx) error) error {
	tx, _, err := s.begin(ctx, a, sc, read)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = fn(tx); err != nil {
		return err
	}
	return s.finish(ctx, tx, a)
}

// List returns the site's cases ordered by ID after cursor.
func (s *Service) List(ctx context.Context, a Principal, sc Scope, cursor string, limit int) (PageV1, error) {
	if limit == 0 {
		limit = DefaultPageSize
	}
	if limit < 1 || limit > MaxPageSize || (cursor != "" && !validID(cursor)) {
		return PageV1{}, ErrInvalid
	}
	tx, _, err := s.begin(ctx, a, sc, read)
	if err != nil {
		return PageV1{}, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id,line_id,order_id,episode,revision,state,detected_at,latest_window_end FROM supervisor.cases WHERE tenant_id=$1 AND site_id=$2 AND ($3='' OR id>$3::uuid) ORDER BY id LIMIT $4`, sc.TenantID, sc.SiteID, cursor, limit+1)
	if err != nil {
		return PageV1{}, err
	}
	out := PageV1{Items: make([]SummaryV1, 0)}
	for rows.Next() {
		var v SummaryV1
		if err = rows.Scan(&v.ID, &v.LineID, &v.OrderID, &v.Episode, &v.Revision, &v.State, &v.DetectedAt, &v.LatestWindowEnd); err != nil {
			rows.Close()
			return PageV1{}, err
		}
		v.DetectedAt, v.LatestWindowEnd = v.DetectedAt.UTC(), v.LatestWindowEnd.UTC()
		out.Items = append(out.Items, v)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return PageV1{}, err
	}
	rows.Close()
	if len(out.Items) > limit {
		out.Items = out.Items[:limit]
		next := out.Items[limit-1].ID
		out.NextCursor = &next
	}
	return out, s.finish(ctx, tx, a)
}

func load(ctx context.Context, tx *sql.Tx, sc Scope, id string) (CaseV1, error) {
	var out CaseV1
	var previous, owner, resolution sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT id,site_id,line_id,order_id,exception_kind,episode,previous_case_id,revision,context_snapshot_id,context_revision,context_digest,owner_user_id,state,detected_at,latest_window_end,workflow_id,resolution_reason FROM supervisor.cases WHERE tenant_id=$1 AND site_id=$2 AND id=$3`, sc.TenantID, sc.SiteID, id).
		Scan(&out.ID, &out.SiteID, &out.LineID, &out.OrderID, &out.ExceptionKind, &out.Episode, &previous, &out.Revision, &out.ContextSnapshotID, &out.ContextRevision, &out.ContextDigest, &owner, &out.State, &out.DetectedAt, &out.LatestWindowEnd, &out.WorkflowID, &resolution)
	if errors.Is(err, sql.ErrNoRows) {
		return out, ErrNotFound
	}
	if err != nil {
		return out, err
	}
	if previous.Valid {
		out.PreviousCaseID = &previous.String
	}
	if owner.Valid {
		out.OwnerUserID = &owner.String
	}
	if resolution.Valid {
		out.ResolutionReason = &resolution.String
	}
	out.DetectedAt, out.LatestWindowEnd = out.DetectedAt.UTC(), out.LatestWindowEnd.UTC()
	if out.WorkflowID != WorkflowID(sc.TenantID, out.ID) {
		return out, errIntegrity
	}
	rows, err := tx.QueryContext(ctx, `SELECT source_id::text,source_sequence FROM supervisor.case_trigger_evidence WHERE tenant_id=$1 AND site_id=$2 AND case_id=$3 ORDER BY source_id,source_sequence`, sc.TenantID, sc.SiteID, id)
	if err != nil {
		return out, err
	}
	out.TriggerEvidenceIDs = make([]string, 0)
	pc := plantcontext.Scope{TenantID: sc.TenantID, SiteID: sc.SiteID}
	for rows.Next() {
		var source string
		var sequence int64
		if err = rows.Scan(&source, &sequence); err != nil {
			rows.Close()
			return out, err
		}
		out.TriggerEvidenceIDs = append(out.TriggerEvidenceIDs, plantcontext.RecordID(pc, source, sequence))
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return out, err
	}
	rows.Close()
	// ponytail: no case_id index on case_triggers (tenant-scoped scan); add one in a migration if
	// the ledger grows large.
	rows, err = tx.QueryContext(ctx, `SELECT source_event_id::text,context_snapshot_id::text,opened,actor_issuer,actor_subject,created_at FROM supervisor.case_triggers WHERE tenant_id=$1 AND site_id=$2 AND case_id=$3 ORDER BY created_at,source_sequence`, sc.TenantID, sc.SiteID, id)
	if err != nil {
		return out, err
	}
	out.Triggers = make([]TriggerV1, 0)
	for rows.Next() {
		var v TriggerV1
		if err = rows.Scan(&v.SourceEventID, &v.ContextSnapshotID, &v.Opened, &v.Actor.Issuer, &v.Actor.Subject, &v.CreatedAt); err != nil {
			rows.Close()
			return out, err
		}
		v.CreatedAt = v.CreatedAt.UTC()
		out.Triggers = append(out.Triggers, v)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return out, err
	}
	rows.Close()
	rows, err = tx.QueryContext(ctx, `SELECT revision,from_state,to_state,event,reason,actor_issuer,actor_subject,created_at FROM supervisor.case_transitions WHERE tenant_id=$1 AND site_id=$2 AND case_id=$3 ORDER BY revision`, sc.TenantID, sc.SiteID, id)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	out.Transitions = make([]TransitionV1, 0)
	for rows.Next() {
		var t TransitionV1
		var reason sql.NullString
		if err = rows.Scan(&t.Revision, &t.FromState, &t.ToState, &t.Event, &reason, &t.Actor.Issuer, &t.Actor.Subject, &t.CreatedAt); err != nil {
			return out, err
		}
		if reason.Valid {
			t.Reason = &reason.String
		}
		t.CreatedAt = t.CreatedAt.UTC()
		out.Transitions = append(out.Transitions, t)
	}
	if err = rows.Err(); err != nil {
		return out, err
	}
	if n := len(out.Transitions); n == 0 || out.Transitions[n-1].Revision != out.Revision || out.Transitions[n-1].ToState != out.State {
		return out, errIntegrity
	}
	crows, err := tx.QueryContext(ctx, `SELECT id,case_revision,body,body_sha256,context_snapshot_id,actor_issuer,actor_subject,created_at FROM supervisor.case_clarifications WHERE tenant_id=$1 AND site_id=$2 AND case_id=$3 ORDER BY case_revision`, sc.TenantID, sc.SiteID, id)
	if err != nil {
		return out, err
	}
	defer crows.Close()
	out.Clarifications = make([]ClarificationV1, 0)
	for crows.Next() {
		var c ClarificationV1
		var contextID sql.NullString
		if err = crows.Scan(&c.ID, &c.CaseRevision, &c.Body, &c.BodySHA256, &contextID, &c.Actor.Issuer, &c.Actor.Subject, &c.CreatedAt); err != nil {
			return out, err
		}
		if contextID.Valid {
			c.ContextSnapshotID = &contextID.String
		}
		c.CreatedAt = c.CreatedAt.UTC()
		out.Clarifications = append(out.Clarifications, c)
	}
	return out, crows.Err()
}

// lifecycle is one human mutation after the shared checks: it returns the case ID the
// response describes (the new episode for reopen).
type lifecycle func(tx *sql.Tx, c CaseV1, now time.Time, actor ActorRef) (string, error)

func (s *Service) mutate(ctx context.Context, a Principal, sc Scope, id, key, op string, ifMatch, expected int64, reason string, body any, apply lifecycle) (CaseV1, error) {
	return s.mutateAs(ctx, a, sc, supervise, id, key, op, ifMatch, expected, reason, body, apply)
}

func (s *Service) mutateAs(ctx context.Context, a Principal, sc Scope, mode access, id, key, op string, ifMatch, expected int64, reason string, body any, apply lifecycle) (CaseV1, error) {
	if !validID(id) {
		return CaseV1{}, ErrNotFound
	}
	if !validID(key) || expected < 1 || ifMatch != expected || !validReason(reason) {
		return CaseV1{}, ErrInvalid
	}
	tx, now, err := s.begin(ctx, a, sc, mode)
	if err != nil {
		return CaseV1{}, err
	}
	defer tx.Rollback()
	hash := requestDigest(struct {
		Op, ID string
		Body   any
	}{op, id, body})
	var stored, storedOp, storedCase string
	err = tx.QueryRowContext(ctx, `SELECT request_sha256,operation,case_id FROM supervisor.case_receipts WHERE tenant_id=$1 AND site_id=$2 AND actor_issuer=$3 AND actor_subject=$4 AND idempotency_key=$5`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, key).Scan(&stored, &storedOp, &storedCase)
	switch {
	case err == nil:
		if stored != hash || storedOp != op {
			return CaseV1{}, ErrIdempotency
		}
		out, err := load(ctx, tx, sc, storedCase)
		if err != nil {
			return out, err
		}
		out.Replayed = true
		return out, s.finish(ctx, tx, a)
	case !errors.Is(err, sql.ErrNoRows):
		return CaseV1{}, err
	}
	if _, err = tx.ExecContext(ctx, `SELECT 1 FROM supervisor.cases WHERE tenant_id=$1 AND site_id=$2 AND id=$3 FOR UPDATE`, sc.TenantID, sc.SiteID, id); err != nil {
		return CaseV1{}, err
	}
	current, err := load(ctx, tx, sc, id)
	if err != nil {
		return current, err
	}
	if current.Revision != expected {
		return CaseV1{}, ErrRevision
	}
	result, err := apply(tx, current, now, ActorRef{Issuer: a.Issuer, Subject: a.Subject})
	if err != nil {
		return CaseV1{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.case_receipts(tenant_id,site_id,actor_issuer,actor_subject,idempotency_key,operation,request_sha256,case_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, key, op, hash, result); err != nil {
		return CaseV1{}, err
	}
	out, err := load(ctx, tx, sc, result)
	if err != nil {
		return out, err
	}
	return out, s.finish(ctx, tx, a)
}

// move records one transition of c to the state Apply allows and advances its revision.
func move(ctx context.Context, tx *sql.Tx, sc Scope, c CaseV1, event Event, g Guards, reason string, actor ActorRef, set string, args ...any) (int64, error) {
	next, err := Apply(c.State, event, g)
	if err != nil {
		return 0, err
	}
	revision := c.Revision + 1
	if err = recordTransition(ctx, tx, sc, c.ID, revision, string(c.State), next, string(event), &reason, actor); err != nil {
		return 0, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE supervisor.cases SET state=$4,revision=$5`+set+` WHERE tenant_id=$1 AND site_id=$2 AND id=$3`, append([]any{sc.TenantID, sc.SiteID, c.ID, string(next), revision}, args...)...)
	return revision, err
}

// Cancel ends a nonterminal case. A case that reached executing or observing has a committed
// task, which must be explicitly canceled first (WIS-277): there is no fictitious rollback of work
// that may have started.
func (s *Service) Cancel(ctx context.Context, a Principal, sc Scope, id string, in CancelInput, key string, ifMatch int64) (CaseV1, error) {
	return s.mutate(ctx, a, sc, id, key, "case.cancel", ifMatch, in.ExpectedRevision, in.Reason, in, func(tx *sql.Tx, c CaseV1, _ time.Time, actor ActorRef) (string, error) {
		// The case row is locked; tasks of the case are read under it (lock order case, task).
		var tasks, canceled int
		if err := tx.QueryRowContext(ctx, `SELECT count(*),count(*) FILTER (WHERE state='canceled') FROM supervisor.tasks WHERE tenant_id=$1 AND site_id=$2 AND case_id=$3`, sc.TenantID, sc.SiteID, c.ID).Scan(&tasks, &canceled); err != nil {
			return "", err
		}
		revision, err := move(ctx, tx, sc, c, Cancel, Guards{CancelAuthorized: true, ReasonProvided: true, CommittedTaskCanceled: tasks > 0 && canceled == tasks}, in.Reason, actor, "")
		if err != nil {
			return "", err
		}
		// An approval for a canceled case must never be consumed (WIS-274 staleness).
		if _, err = approvals.MarkCaseProposalsStale(ctx, tx, sc.TenantID, sc.SiteID, c.ID, approvals.ActorRef(actor)); err != nil {
			return "", err
		}
		return c.ID, outbox(ctx, tx, sc, c.ID, revision, "cancel", transitionEvent(c.ID, revision))
	})
}

// Close resolves a case: a recently authenticated site supervisor, the case's task terminal and
// the outcome of its current completion recorded (any decided status, including insufficient_data
// or not_comparable, so missing evidence cannot trap a case). Task completion alone never
// resolves a case, and the closure reason never overwrites the measured outcome.
func (s *Service) Close(ctx context.Context, a Principal, sc Scope, id string, in CloseInput, key string, ifMatch int64) (CaseV1, error) {
	return s.mutate(ctx, a, sc, id, key, "case.close", ifMatch, in.ExpectedRevision, in.Reason, in, func(tx *sql.Tx, c CaseV1, _ time.Time, actor ActorRef) (string, error) {
		// The case row is locked; its newest task and that task's outcome are read under it.
		var terminal, recorded bool
		err := tx.QueryRowContext(ctx, `SELECT t.state IN ('completed','canceled'),
 t.state='completed' AND EXISTS(SELECT 1 FROM supervisor.outcome_observations o WHERE (o.tenant_id,o.site_id,o.task_id)=(t.tenant_id,t.site_id,t.id)
   AND o.epoch=(SELECT max(epoch) FROM supervisor.task_completions m WHERE (m.tenant_id,m.site_id,m.task_id)=(t.tenant_id,t.site_id,t.id)))
 FROM supervisor.tasks t WHERE t.tenant_id=$1 AND t.site_id=$2 AND t.case_id=$3 ORDER BY t.created_at DESC,t.id DESC LIMIT 1`, sc.TenantID, sc.SiteID, c.ID).Scan(&terminal, &recorded)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
		revision, err := move(ctx, tx, sc, c, ClosedBySupervisor, Guards{TaskTerminal: terminal, OutcomeRecorded: recorded, SupervisorActor: true, ReasonProvided: true}, in.Reason, actor, ",resolution_reason=$6", in.Reason)
		if err != nil {
			return "", err
		}
		return c.ID, outbox(ctx, tx, sc, c.ID, revision, "signal", transitionEvent(c.ID, revision))
	})
}

// Resume returns an escalated case to investigating bound to a new eligible READY context for
// the same order and line. It is refused when the episode already has an approved or consumed
// payload: that is never regenerated.
func (s *Service) Resume(ctx context.Context, a Principal, sc Scope, id string, in ResumeInput, key string, ifMatch int64) (CaseV1, error) {
	if !validID(in.NewContextID) {
		return CaseV1{}, ErrInvalid
	}
	return s.mutate(ctx, a, sc, id, key, "case.resume", ifMatch, in.ExpectedRevision, in.Reason, in, func(tx *sql.Tx, c CaseV1, now time.Time, actor ActorRef) (string, error) {
		view, err := eligibleReady(ctx, tx, sc, in.NewContextID, now)
		if err != nil {
			return "", err
		}
		if view.OrderID != c.OrderID || view.AssetID != c.LineID {
			return "", ErrInvalid
		}
		var approved bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM supervisor.proposal_revisions r JOIN supervisor.approval_states s ON (s.tenant_id,s.site_id,s.proposal_id)=(r.tenant_id,r.site_id,r.proposal_id) WHERE r.tenant_id=$1 AND r.site_id=$2 AND r.case_id=$3 AND r.revision=s.current_revision AND (s.state='consumed' OR (s.state='approved' AND r.expires_at>$4)))`, sc.TenantID, sc.SiteID, c.ID, now).Scan(&approved); err != nil {
			return "", err
		}
		g := Guards{NewContextProvided: view.ContextID != c.ContextSnapshotID, SupervisorActor: true, ReasonProvided: true, ApprovedPayloadExists: approved}
		revision, err := move(ctx, tx, sc, c, Resume, g, in.Reason, actor, ",context_snapshot_id=$6,context_digest=$7", view.ContextID, view.Digest)
		if err != nil {
			return "", err
		}
		return c.ID, outbox(ctx, tx, sc, c.ID, revision, "signal", transitionEvent(c.ID, revision))
	})
}

// Reopen starts a new episode after a resolved or canceled case, referencing it and its
// trigger evidence; the old episode stays immutable. Only the latest episode of a key can be
// reopened, and never while another episode is active.
func (s *Service) Reopen(ctx context.Context, a Principal, sc Scope, id string, in ReopenInput, key string, ifMatch int64) (CaseV1, error) {
	return s.mutate(ctx, a, sc, id, key, "case.reopen", ifMatch, in.ExpectedRevision, in.Reason, in, func(tx *sql.Tx, c CaseV1, now time.Time, actor ActorRef) (string, error) {
		if _, err := Apply(c.State, Reopen, Guards{ReasonProvided: true, SupervisorActor: true}); err != nil {
			return "", err
		}
		var later bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM supervisor.cases WHERE tenant_id=$1 AND site_id=$2 AND line_id=$3 AND order_id=$4 AND exception_kind=$5 AND (episode>$6 OR state NOT IN ('resolved','canceled')))`, sc.TenantID, sc.SiteID, c.LineID, c.OrderID, c.ExceptionKind, c.Episode).Scan(&later); err != nil {
			return "", err
		}
		if later {
			return "", ErrActiveEpisode
		}
		next := uuid.NewString()
		if err := insertCase(ctx, tx, sc, next, c.LineID, c.OrderID, c.Episode+1, c.ID, c.ContextSnapshotID, c.ContextDigest, now, c.LatestWindowEnd); err != nil {
			return "", err
		}
		reason := in.Reason
		if err := recordTransition(ctx, tx, sc, next, 1, "none", Detected, string(Reopen), &reason, actor); err != nil {
			return "", err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO supervisor.case_trigger_evidence(tenant_id,site_id,case_id,source_id,source_sequence) SELECT tenant_id,site_id,$4,source_id,source_sequence FROM supervisor.case_trigger_evidence WHERE tenant_id=$1 AND site_id=$2 AND case_id=$3`, sc.TenantID, sc.SiteID, c.ID, next); err != nil {
			return "", err
		}
		return next, outbox(ctx, tx, sc, next, 1, "start", transitionEvent(next, 1))
	})
}

// MaxClarificationRunes bounds a clarification body.
const MaxClarificationRunes = 4000

// ClarificationV1 is attributed source evidence a person supplied while the case waited on
// input. It is appended, never written over an observation.
type ClarificationV1 struct {
	ID                string    `json:"id"`
	CaseRevision      int64     `json:"case_revision"`
	Body              string    `json:"body"`
	BodySHA256        string    `json:"body_sha256" contract:"sha256"`
	ContextSnapshotID *string   `json:"context_snapshot_id"`
	Actor             ActorRef  `json:"actor"`
	CreatedAt         time.Time `json:"created_at"`
}

// ClarifyInput is the POST /cases/{id}/clarifications body. NewContextID optionally binds a
// new eligible READY context for the same order and line.
type ClarifyInput struct {
	ExpectedRevision int64   `json:"expected_revision"`
	Body             string  `json:"body"`
	NewContextID     *string `json:"new_context_id"`
}

// Clarify records a person's clarification as case evidence and returns a case waiting on
// input to investigating, in one transaction. Whether the evidence is now complete is judged
// again by the investigation (WIS-276), which can return the case to needs_input; a case in any
// other state refuses the clarification and records nothing.
func (s *Service) Clarify(ctx context.Context, a Principal, sc Scope, id string, in ClarifyInput, key string, ifMatch int64) (CaseV1, error) {
	n := utf8.RuneCountInString(in.Body)
	if !utf8.ValidString(in.Body) || n < 1 || n > MaxClarificationRunes || strings.TrimSpace(in.Body) == "" || strings.ContainsRune(in.Body, 0) ||
		(in.NewContextID != nil && !validID(*in.NewContextID)) {
		return CaseV1{}, ErrInvalid
	}
	return s.mutateAs(ctx, a, sc, clarify, id, key, "case.clarify", ifMatch, in.ExpectedRevision, "CLARIFICATION_RECEIVED", in, func(tx *sql.Tx, c CaseV1, now time.Time, actor ActorRef) (string, error) {
		contextID, digest := c.ContextSnapshotID, c.ContextDigest
		var boundContext any
		if in.NewContextID != nil {
			view, err := eligibleReady(ctx, tx, sc, *in.NewContextID, now)
			if err != nil {
				return "", err
			}
			if view.OrderID != c.OrderID || view.AssetID != c.LineID {
				return "", ErrInvalid
			}
			contextID, digest, boundContext = view.ContextID, view.Digest, view.ContextID
		}
		g := Guards{ClarificationAuthorized: true, EvidenceComplete: true, NewContextProvided: in.NewContextID != nil}
		revision, err := move(ctx, tx, sc, c, Clarified, g, "CLARIFICATION_RECEIVED", actor, ",context_snapshot_id=$6,context_digest=$7", contextID, digest)
		if err != nil {
			return "", err
		}
		sum := sha256.Sum256([]byte(in.Body))
		if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.case_clarifications(tenant_id,site_id,id,case_id,case_revision,body,body_sha256,context_snapshot_id,actor_issuer,actor_subject) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
			sc.TenantID, sc.SiteID, uuid.NewString(), c.ID, revision, in.Body, hex.EncodeToString(sum[:]), boundContext, actor.Issuer, actor.Subject); err != nil {
			return "", err
		}
		return c.ID, outbox(ctx, tx, sc, c.ID, revision, "signal", transitionEvent(c.ID, revision))
	})
}
