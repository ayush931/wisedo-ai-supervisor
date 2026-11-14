package onboarding

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/knowledge"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/membership"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/session"
	"github.com/google/uuid"
)

var (
	ErrInterviewInvalid   = errors.New("invalid interview request")
	ErrNotFound           = errors.New("interview unavailable")
	ErrDenied             = errors.New("interview permission denied")
	ErrConflict           = errors.New("interview idempotency conflict")
	ErrRevision           = errors.New("interview revision changed")
	ErrLifecycle          = errors.New("interview lifecycle conflict")
	ErrInventoryAmbiguous = errors.New("discovery inventory is ambiguous")
	ErrInventoryChanged   = errors.New("discovery inventory changed")
)

type InterviewRole string

const (
	RoleOwner      InterviewRole = "owner"
	RoleSupervisor InterviewRole = "supervisor"
	RoleOperator   InterviewRole = "operator"
)

type InterviewStatus string

const (
	StatusActive    InterviewStatus = "active"
	StatusPaused    InterviewStatus = "paused"
	StatusSubmitted InterviewStatus = "submitted"
	StatusClosed    InterviewStatus = "closed"
)

type QuestionReason string

const (
	QuestionMissing  QuestionReason = "missing"
	QuestionConflict QuestionReason = "conflict"
	QuestionStale    QuestionReason = "stale"
)

type AnswerState string

const (
	AnswerUnanswered AnswerState = "unanswered"
	AnswerAnswered   AnswerState = "answered"
	AnswerDismissed  AnswerState = "dismissed"
)

type Actor struct{ Issuer, Subject, SessionHash string }
type Scope struct{ TenantID, SiteID string }

type InterviewSession struct {
	SessionID          uuid.UUID       `json:"session_id"`
	Role               InterviewRole   `json:"role"`
	ParticipantActorID uuid.UUID       `json:"participant_actor_id"`
	ProcessID          *uuid.UUID      `json:"process_id,omitempty"`
	Status             InterviewStatus `json:"status"`
	Revision           int64           `json:"revision"`
	CreatedAt          time.Time       `json:"created_at"`
	UpdatedAt          time.Time       `json:"updated_at"`
	inventoryID        string
	inventoryRevision  int64
}

type CreateInterviewInput struct {
	Role      InterviewRole `json:"role"`
	ProcessID *uuid.UUID    `json:"process_id,omitempty"`
	// InventoryID chooses which discovery inventory (checklist) the interview covers when a process
	// has more than one, such as its shortfall and its value-stream inventories (WIS-617). omitempty
	// keeps the fingerprint of requests made before it existed.
	InventoryID *uuid.UUID `json:"inventory_id,omitempty"`
}

func (in CreateInterviewInput) Validate() error {
	if !validRole(in.Role) || in.ProcessID != nil && *in.ProcessID == uuid.Nil || in.InventoryID != nil && *in.InventoryID == uuid.Nil {
		return ErrInterviewInvalid
	}
	return nil
}

type PatchInterviewInput struct {
	Status           InterviewStatus `json:"status"`
	ExpectedRevision int64           `json:"expected_revision"`
}

func (in PatchInterviewInput) Validate() error {
	if !validStatus(in.Status) || in.ExpectedRevision < 1 || in.ExpectedRevision >= math.MaxInt64 {
		return ErrInterviewInvalid
	}
	return nil
}

type Question struct {
	QuestionID    uuid.UUID      `json:"question_id"`
	Revision      int64          `json:"revision"`
	RequirementID uuid.UUID      `json:"requirement_id"`
	Role          InterviewRole  `json:"role"`
	Prompt        string         `json:"prompt" contract:"max=1000"`
	SourceSpanIDs []uuid.UUID    `json:"source_span_ids"`
	Reason        QuestionReason `json:"reason"`
	AnswerState   AnswerState    `json:"answer_state"`
	SnoozedUntil  *time.Time     `json:"snoozed_until,omitempty"`
	// PromptOrigin says who worded the prompt: the deterministic template or the agent.
	PromptOrigin PromptOrigin `json:"prompt_origin"`
}

type PromptOrigin string

const (
	PromptTemplate PromptOrigin = "template"
	PromptAgent    PromptOrigin = "agent"
)

type QuestionPage struct {
	Questions  []Question `json:"questions"`
	NextCursor string     `json:"next_cursor,omitempty"`
}

type InterviewSessionPage struct {
	Sessions   []InterviewSession `json:"sessions"`
	NextCursor string             `json:"next_cursor,omitempty"`
}

type AnswerInput struct {
	QuestionID       uuid.UUID `json:"question_id"`
	QuestionRevision int64     `json:"question_revision"`
	Text             string    `json:"text" contract:"max=8000"`
	ClientAnswerID   uuid.UUID `json:"client_answer_id"`
}

func (in AnswerInput) Validate() error {
	if in.QuestionID == uuid.Nil || in.ClientAnswerID == uuid.Nil || in.QuestionRevision < 1 || !ValidateAnswerText(in.Text) {
		return ErrInterviewInvalid
	}
	return nil
}

type Answer struct {
	ClientAnswerID uuid.UUID `json:"client_answer_id"`
	QuestionID     uuid.UUID `json:"question_id"`
	Revision       int64     `json:"revision"`
	SourceID       uuid.UUID `json:"source_id"`
	SourceRevision int64     `json:"source_revision"`
	SourceSpanID   uuid.UUID `json:"source_span_id"`
	CreatedAt      time.Time `json:"created_at"`
}

type Service struct {
	db       *sql.DB
	sessions session.Policy
}

func NewInterviews(db *sql.DB) *Service { return &Service{db: db, sessions: session.Default()} }

func (s *Service) WithSessionPolicy(p session.Policy) *Service {
	c := *s
	c.sessions = p.OrDefault()
	return &c
}

func validRole(r InterviewRole) bool {
	return r == RoleOwner || r == RoleSupervisor || r == RoleOperator
}

func validStatus(v InterviewStatus) bool {
	return v == StatusActive || v == StatusPaused || v == StatusSubmitted || v == StatusClosed
}

func validID(v string) bool {
	id, err := uuid.Parse(v)
	return err == nil && id != uuid.Nil && id.String() == v
}

func validScope(sc Scope) bool { return validID(sc.TenantID) && validID(sc.SiteID) }

func validActor(a Actor) bool {
	b, err := hex.DecodeString(a.SessionHash)
	return err == nil && len(b) == sha256.Size && a.SessionHash == strings.ToLower(a.SessionHash) &&
		len(a.Issuer) > 0 && len(a.Issuer) <= 512 && len(a.Subject) > 0 && len(a.Subject) <= 255 &&
		!strings.ContainsRune(a.Issuer, 0) && !strings.ContainsRune(a.Subject, 0)
}

func validMutationKey(key string) bool {
	return len(key) >= 1 && len(key) <= 128 && strings.TrimSpace(key) == key
}

func actorID(a Actor) uuid.UUID { return membership.ActorID(a.Issuer, a.Subject) }

func requestHash(op string, value any) string {
	body, _ := json.Marshal(struct {
		Operation string `json:"operation"`
		Value     any    `json:"value"`
	}{op, value})
	h := sha256.Sum256(body)
	return hex.EncodeToString(h[:])
}

func (s *Service) checkSession(ctx context.Context, tx *sql.Tx, a Actor) error {
	var issuer, subject string
	var expiry, seen, now time.Time
	err := tx.QueryRowContext(ctx, `SELECT issuer,subject,expires_at,last_seen_at,clock_timestamp() FROM supervisor.browser_sessions WHERE token_hash=$1 FOR SHARE`, a.SessionHash).Scan(&issuer, &subject, &expiry, &seen, &now)
	if errors.Is(err, sql.ErrNoRows) || err == nil && (issuer != a.Issuer || subject != a.Subject || !expiry.After(now) || !seen.After(now.Add(-s.sessions.Idle))) {
		return ErrDenied
	}
	return err
}

func (s *Service) begin(ctx context.Context, a Actor, sc Scope, write bool) (*sql.Tx, string, error) {
	if !validScope(sc) {
		return nil, "", ErrInterviewInvalid
	}
	if !validActor(a) {
		return nil, "", ErrDenied
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, "", err
	}
	fail := func(e error) (*sql.Tx, string, error) { _ = tx.Rollback(); return nil, "", e }
	if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true),set_config('statement_timeout','10s',true),set_config('lock_timeout','5s',true)`, sc.TenantID); err != nil {
		return fail(err)
	}
	if err = s.checkSession(ctx, tx, a); err != nil {
		return fail(err)
	}
	var found bool
	if err = tx.QueryRowContext(ctx, `SELECT supervisor.lock_member_site($1,$2,$3,$4,$5)`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, write).Scan(&found); err != nil {
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
	if write && role != string(RoleOwner) && role != string(RoleSupervisor) && role != string(RoleOperator) {
		return fail(ErrDenied)
	}
	if err = s.checkSession(ctx, tx, a); err != nil {
		return fail(err)
	}
	return tx, role, nil
}

func (s *Service) finish(ctx context.Context, tx *sql.Tx, a Actor) error {
	if err := s.checkSession(ctx, tx, a); err != nil {
		return err
	}
	return tx.Commit()
}

func audit(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, sessionID string, questionID any, event string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO supervisor.interview_audit(tenant_id,site_id,session_id,question_id,actor_issuer,actor_subject,event_type) VALUES($1,$2,$3,$4,$5,$6,$7)`, sc.TenantID, sc.SiteID, sessionID, questionID, a.Issuer, a.Subject, event)
	return err
}

func receipt(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, key, digest string) ([]byte, bool, error) {
	var saved string
	var result []byte
	err := tx.QueryRowContext(ctx, `SELECT request_sha256,result FROM supervisor.interview_receipts WHERE tenant_id=$1 AND site_id=$2 AND actor_issuer=$3 AND actor_subject=$4 AND idempotency_key=$5`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, key).Scan(&saved, &result)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if saved != digest {
		return nil, false, ErrConflict
	}
	return result, true, nil
}

func saveReceipt(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, key, operation, digest, sessionID string, result any) error {
	body, err := json.Marshal(result)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO supervisor.interview_receipts(tenant_id,site_id,actor_issuer,actor_subject,idempotency_key,operation,request_sha256,session_id,result) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, key, operation, digest, sessionID, body)
	return err
}

func (s *Service) Create(ctx context.Context, a Actor, sc Scope, in CreateInterviewInput, key string) (InterviewSession, error) {
	if in.Validate() != nil || !validMutationKey(key) {
		return InterviewSession{}, ErrInterviewInvalid
	}
	tx, memberRole, err := s.begin(ctx, a, sc, true)
	if err != nil {
		return InterviewSession{}, err
	}
	defer tx.Rollback()
	if memberRole != string(in.Role) {
		return InterviewSession{}, ErrDenied
	}
	digest := requestHash("create", in)
	if body, replayed, e := receipt(ctx, tx, a, sc, key, digest); e != nil {
		return InterviewSession{}, e
	} else if replayed {
		var out InterviewSession
		if e = json.Unmarshal(body, &out); e != nil {
			return InterviewSession{}, e
		}
		if e = audit(ctx, tx, a, sc, out.SessionID.String(), nil, "mutation.replayed"); e != nil {
			return InterviewSession{}, e
		}
		return out, s.finish(ctx, tx, a)
	}
	var requestedProcess, requestedInventory any
	if in.ProcessID != nil {
		requestedProcess = in.ProcessID.String()
	}
	if in.InventoryID != nil {
		requestedInventory = in.InventoryID.String()
	}
	// A chosen inventory must be this site's and, when a process is named too, that process's;
	// anything else is not found, never another inventory.
	rows, err := tx.QueryContext(ctx, `SELECT r.process_id::text,h.inventory_id::text,h.current_revision
 FROM supervisor.knowledge_discovery_heads h
 JOIN supervisor.knowledge_discovery_revisions r ON (r.tenant_id,r.site_id,r.inventory_id,r.revision)=(h.tenant_id,h.site_id,h.inventory_id,h.current_revision)
 WHERE h.tenant_id=$1 AND h.site_id=$2 AND ($3::uuid IS NULL OR r.process_id=$3) AND ($4::uuid IS NULL OR h.inventory_id=$4)
 ORDER BY h.inventory_id LIMIT 2`, sc.TenantID, sc.SiteID, requestedProcess, requestedInventory)
	if err != nil {
		return InterviewSession{}, err
	}
	var processID, inventoryID string
	var inventoryRevision int64
	count := 0
	for rows.Next() {
		if err = rows.Scan(&processID, &inventoryID, &inventoryRevision); err != nil {
			rows.Close()
			return InterviewSession{}, err
		}
		count++
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return InterviewSession{}, err
	}
	if count > 1 {
		// shortcut: multiple inventories require an explicit selection contract; process_id alone is ambiguous.
		return InterviewSession{}, ErrInventoryAmbiguous
	}
	if count == 0 && (in.ProcessID != nil || in.InventoryID != nil) {
		return InterviewSession{}, ErrNotFound
	}
	if count == 1 && in.ProcessID != nil && processID != in.ProcessID.String() {
		return InterviewSession{}, ErrNotFound
	}
	if count == 0 {
		processID, inventoryID = "", ""
	}
	id := uuid.New()
	var process, inventory, invRevision any
	if processID != "" {
		process, inventory, invRevision = processID, inventoryID, inventoryRevision
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO supervisor.interview_sessions(tenant_id,site_id,session_id,role,participant_actor_id,participant_issuer,participant_subject,process_id,inventory_id,inventory_revision,status,revision) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'active',1)`, sc.TenantID, sc.SiteID, id.String(), in.Role, actorID(a).String(), a.Issuer, a.Subject, process, inventory, invRevision)
	if err != nil {
		return InterviewSession{}, err
	}
	out, err := loadSession(ctx, tx, sc, a, id.String(), false)
	if err != nil {
		return InterviewSession{}, err
	}
	if err = audit(ctx, tx, a, sc, id.String(), nil, "session.created"); err != nil {
		return InterviewSession{}, err
	}
	if err = saveReceipt(ctx, tx, a, sc, key, "create", digest, id.String(), out); err != nil {
		return InterviewSession{}, err
	}
	return out, s.finish(ctx, tx, a)
}

const sessionSelect = `SELECT session_id::text,role,participant_actor_id::text,process_id::text,status,revision,created_at,updated_at,inventory_id::text,inventory_revision
 FROM supervisor.interview_sessions WHERE tenant_id=$1 AND site_id=$2 AND session_id=$3 AND participant_issuer=$4 AND participant_subject=$5`

func loadSession(ctx context.Context, tx *sql.Tx, sc Scope, a Actor, id string, lock bool) (InterviewSession, error) {
	query := sessionSelect
	if lock {
		query += " FOR UPDATE"
	}
	out, err := scanInterviewSession(tx.QueryRowContext(ctx, query, sc.TenantID, sc.SiteID, id, a.Issuer, a.Subject))
	if errors.Is(err, sql.ErrNoRows) {
		return InterviewSession{}, ErrNotFound
	}
	return out, err
}

type interviewRow interface{ Scan(...any) error }

func scanInterviewSession(row interviewRow) (InterviewSession, error) {
	var out InterviewSession
	var sessionID, actorIDValue, role, status string
	var processID, inventoryID sql.NullString
	var inventoryRevision sql.NullInt64
	err := row.Scan(&sessionID, &role, &actorIDValue, &processID, &status, &out.Revision, &out.CreatedAt, &out.UpdatedAt, &inventoryID, &inventoryRevision)
	if err != nil {
		return InterviewSession{}, err
	}
	out.SessionID, err = uuid.Parse(sessionID)
	if err != nil {
		return InterviewSession{}, err
	}
	out.ParticipantActorID, err = uuid.Parse(actorIDValue)
	if err != nil {
		return InterviewSession{}, err
	}
	out.Role, out.Status = InterviewRole(role), InterviewStatus(status)
	if processID.Valid {
		value, e := uuid.Parse(processID.String)
		if e != nil {
			return InterviewSession{}, e
		}
		out.ProcessID = &value
	}
	if inventoryID.Valid {
		out.inventoryID, out.inventoryRevision = inventoryID.String, inventoryRevision.Int64
	}
	return out, nil
}

func (s *Service) List(ctx context.Context, a Actor, sc Scope, limit int, cursor string) (InterviewSessionPage, error) {
	if limit < 1 || limit > 100 || cursor != "" && !validID(cursor) {
		return InterviewSessionPage{}, ErrInterviewInvalid
	}
	tx, _, err := s.begin(ctx, a, sc, false)
	if err != nil {
		return InterviewSessionPage{}, err
	}
	defer tx.Rollback()
	if cursor == "" {
		cursor = uuid.Nil.String()
	}
	rows, err := tx.QueryContext(ctx, `SELECT session_id::text,role,participant_actor_id::text,process_id::text,status,revision,created_at,updated_at,inventory_id::text,inventory_revision
 FROM supervisor.interview_sessions WHERE tenant_id=$1 AND site_id=$2 AND participant_issuer=$3 AND participant_subject=$4 AND session_id>$5 ORDER BY session_id LIMIT $6`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, cursor, limit+1)
	if err != nil {
		return InterviewSessionPage{}, err
	}
	out := InterviewSessionPage{Sessions: []InterviewSession{}}
	for rows.Next() {
		item, e := scanInterviewSession(rows)
		if e != nil {
			rows.Close()
			return InterviewSessionPage{}, e
		}
		out.Sessions = append(out.Sessions, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return InterviewSessionPage{}, err
	}
	if len(out.Sessions) > limit {
		out.NextCursor = out.Sessions[limit-1].SessionID.String()
		out.Sessions = out.Sessions[:limit]
	}
	for _, item := range out.Sessions {
		if err = audit(ctx, tx, a, sc, item.SessionID.String(), nil, "session.listed"); err != nil {
			return InterviewSessionPage{}, err
		}
	}
	return out, s.finish(ctx, tx, a)
}

func (s *Service) Get(ctx context.Context, a Actor, sc Scope, id string) (InterviewSession, error) {
	if !validID(id) {
		return InterviewSession{}, ErrNotFound
	}
	tx, _, err := s.begin(ctx, a, sc, false)
	if err != nil {
		return InterviewSession{}, err
	}
	defer tx.Rollback()
	out, err := loadSession(ctx, tx, sc, a, id, false)
	if err != nil {
		return InterviewSession{}, err
	}
	if err = audit(ctx, tx, a, sc, id, nil, "session.read"); err != nil {
		return InterviewSession{}, err
	}
	return out, s.finish(ctx, tx, a)
}

func canMove(from, to InterviewStatus) bool {
	switch from {
	case StatusActive:
		return to == StatusPaused || to == StatusSubmitted || to == StatusClosed
	case StatusPaused:
		return to == StatusActive || to == StatusSubmitted || to == StatusClosed
	case StatusSubmitted:
		return to == StatusClosed
	default:
		return false
	}
}

func (s *Service) Patch(ctx context.Context, a Actor, sc Scope, id string, in PatchInterviewInput, key string, ifMatch int64) (InterviewSession, error) {
	if !validID(id) || in.Validate() != nil || !validMutationKey(key) || ifMatch != in.ExpectedRevision {
		return InterviewSession{}, ErrInterviewInvalid
	}
	tx, _, err := s.begin(ctx, a, sc, true)
	if err != nil {
		return InterviewSession{}, err
	}
	defer tx.Rollback()
	out, err := loadSession(ctx, tx, sc, a, id, true)
	if err != nil {
		return InterviewSession{}, err
	}
	digest := requestHash("patch", struct {
		SessionID string
		Input     PatchInterviewInput
	}{id, in})
	if body, replayed, e := receipt(ctx, tx, a, sc, key, digest); e != nil {
		return InterviewSession{}, e
	} else if replayed {
		if e = json.Unmarshal(body, &out); e != nil {
			return InterviewSession{}, e
		}
		if e = audit(ctx, tx, a, sc, id, nil, "mutation.replayed"); e != nil {
			return InterviewSession{}, e
		}
		return out, s.finish(ctx, tx, a)
	}
	if out.Revision != in.ExpectedRevision {
		return InterviewSession{}, ErrRevision
	}
	if !canMove(out.Status, in.Status) {
		return InterviewSession{}, ErrLifecycle
	}
	_, err = tx.ExecContext(ctx, `UPDATE supervisor.interview_sessions SET status=$4,revision=revision+1,updated_at=clock_timestamp() WHERE tenant_id=$1 AND site_id=$2 AND session_id=$3 AND revision=$5`, sc.TenantID, sc.SiteID, id, in.Status, in.ExpectedRevision)
	if err != nil {
		return InterviewSession{}, err
	}
	if in.Status == StatusSubmitted || in.Status == StatusClosed {
		if _, err = tx.ExecContext(ctx, `UPDATE supervisor.interview_questions SET answer_state='dismissed',revision=revision+1,updated_at=clock_timestamp() WHERE tenant_id=$1 AND site_id=$2 AND session_id=$3 AND answer_state='unanswered'`, sc.TenantID, sc.SiteID, id); err != nil {
			return InterviewSession{}, err
		}
	}
	out, err = loadSession(ctx, tx, sc, a, id, false)
	if err != nil {
		return InterviewSession{}, err
	}
	if err = audit(ctx, tx, a, sc, id, nil, "session.status_changed"); err != nil {
		return InterviewSession{}, err
	}
	if err = saveReceipt(ctx, tx, a, sc, key, "patch", digest, id, out); err != nil {
		return InterviewSession{}, err
	}
	return out, s.finish(ctx, tx, a)
}

type questionCandidate struct {
	requirementID             uuid.UUID
	role                      InterviewRole
	prompt                    string
	reason                    QuestionReason
	digest                    string
	decisionRevision          int64
	decisionInventoryRevision int64
}

func mapKnowledgeError(err error) error {
	switch {
	case errors.Is(err, knowledge.ErrNotFound):
		return ErrNotFound
	case errors.Is(err, knowledge.ErrDenied):
		return ErrDenied
	case errors.Is(err, knowledge.ErrInvalidContract):
		return ErrInterviewInvalid
	default:
		return err
	}
}

func questionDigest(inventoryID string, pinned, current int64, requirementID string, cell knowledge.CoverageRequirement) string {
	body, _ := json.Marshal(struct {
		InventoryID   string
		Pinned        int64
		Current       int64
		RequirementID string
		State         string
		ReasonCode    string
		DecisionID    string
		DecisionRev   int64
		SnapshotHash  string
	}{inventoryID, pinned, current, requirementID, cell.State, cell.ReasonCode, cell.DecisionID, cell.DecisionRevision, cell.SnapshotSHA256})
	h := sha256.Sum256(body)
	return hex.EncodeToString(h[:])
}

func (s *Service) questionCandidates(ctx context.Context, a Actor, sc Scope, in InterviewSession) ([]questionCandidate, int64, int64, error) {
	if in.inventoryID == "" {
		return nil, 0, 0, nil
	}
	ka := knowledge.Actor{Issuer: a.Issuer, Subject: a.Subject, SessionHash: a.SessionHash}
	ks := knowledge.Scope{TenantID: sc.TenantID, SiteID: sc.SiteID}
	service := knowledge.New(s.db).WithSessionPolicy(s.sessions)
	inventory, err := service.ReadDiscoveryInventory(ctx, ka, ks, in.inventoryID, in.inventoryRevision)
	if err != nil {
		return nil, 0, 0, mapKnowledgeError(err)
	}
	coverage, err := service.ReadCoverage(ctx, ka, ks, in.inventoryID)
	if err != nil {
		return nil, 0, 0, mapKnowledgeError(err)
	}
	cells := make(map[string]knowledge.CoverageRequirement, len(coverage.Requirements))
	for _, cell := range coverage.Requirements {
		cells[cell.RequirementID] = cell
	}
	out := make([]questionCandidate, 0, 3)
	for _, req := range inventory.Requirements {
		if req.InterviewRole != string(in.Role) {
			continue
		}
		cell, exists := cells[req.RequirementID]
		if exists && (cell.State == "approved" || cell.State == "not_applicable") {
			continue
		}
		reason := QuestionMissing
		// shortcut: discovery coverage has no requirement-level conflict state; map contested claims when they are linked to requirements.
		if exists && cell.State == "conflict" {
			reason = QuestionConflict
		} else if !exists || cell.State == "stale" || !inventory.ScopeCurrent || inventory.Revision != inventory.CurrentRevision {
			reason = QuestionStale
		}
		out = append(out, questionCandidate{
			requirementID:             uuid.MustParse(req.RequirementID),
			role:                      InterviewRole(req.InterviewRole),
			prompt:                    req.Description,
			reason:                    reason,
			digest:                    questionDigest(inventory.InventoryID, in.inventoryRevision, coverage.InventoryRevision, req.RequirementID, cell),
			decisionRevision:          cell.DecisionRevision,
			decisionInventoryRevision: coverage.InventoryRevision,
		})
	}
	return out, coverage.InventoryRevision, coverage.KnowledgeInventoryRevision, nil
}

func (s *Service) Questions(ctx context.Context, a Actor, sc Scope, id string, limit int, cursor string) (QuestionPage, error) {
	if !validID(id) || limit < 1 || limit > 100 || cursor != "" && !validID(cursor) {
		return QuestionPage{}, ErrInterviewInvalid
	}
	in, err := s.Get(ctx, a, sc, id)
	if err != nil {
		return QuestionPage{}, err
	}
	candidates, currentInventoryRevision, knowledgeInventoryRevision, err := s.questionCandidates(ctx, a, sc, in)
	if err != nil {
		return QuestionPage{}, err
	}
	tx, _, err := s.begin(ctx, a, sc, false)
	if err != nil {
		return QuestionPage{}, err
	}
	defer func() { _ = tx.Rollback() }()
	current, err := loadSession(ctx, tx, sc, a, id, false)
	if err != nil {
		return QuestionPage{}, err
	}
	if current.inventoryID != in.inventoryID || current.inventoryRevision != in.inventoryRevision {
		return QuestionPage{}, ErrInventoryChanged
	}
	insert := current.inventoryID != "" && len(candidates) > 0 && current.Status == StatusActive
	if insert {
		var unanswered int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.interview_questions q JOIN supervisor.interview_sessions i USING(tenant_id,site_id,session_id) WHERE i.tenant_id=$1 AND i.site_id=$2 AND i.participant_issuer=$3 AND i.participant_subject=$4 AND q.answer_state='unanswered'`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject).Scan(&unanswered); err != nil {
			return QuestionPage{}, err
		}
		insert = unanswered < 3
	}
	if insert {
		insert = false
		for _, candidate := range candidates {
			var exists bool
			if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM supervisor.interview_questions WHERE tenant_id=$1 AND site_id=$2 AND session_id=$3 AND requirement_id=$4 AND evidence_digest=$5)`, sc.TenantID, sc.SiteID, id, candidate.requirementID.String(), candidate.digest).Scan(&exists); err != nil {
				return QuestionPage{}, err
			}
			if !exists {
				insert = true
				break
			}
		}
	}
	if insert {
		_ = tx.Rollback()
		tx, _, err = s.begin(ctx, a, sc, true)
		if err != nil {
			return QuestionPage{}, err
		}
		current, err = loadSession(ctx, tx, sc, a, id, true)
		if err != nil {
			return QuestionPage{}, err
		}
		if current.inventoryID != in.inventoryID || current.inventoryRevision != in.inventoryRevision {
			return QuestionPage{}, ErrInventoryChanged
		}
		if current.inventoryID != "" && len(candidates) > 0 && current.Status == StatusActive {
			var head int64
			err = tx.QueryRowContext(ctx, `SELECT current_revision FROM supervisor.knowledge_discovery_heads WHERE tenant_id=$1 AND site_id=$2 AND inventory_id=$3`, sc.TenantID, sc.SiteID, current.inventoryID).Scan(&head)
			if errors.Is(err, sql.ErrNoRows) {
				return QuestionPage{}, ErrNotFound
			}
			if err != nil {
				return QuestionPage{}, err
			}
			if head != currentInventoryRevision {
				return QuestionPage{}, ErrInventoryChanged
			}
			var knowledgeHead int64
			err = tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT revision FROM supervisor.knowledge_inventory_revisions WHERE tenant_id=$1 AND site_id=$2),0)`, sc.TenantID, sc.SiteID).Scan(&knowledgeHead)
			if err != nil {
				return QuestionPage{}, err
			}
			if knowledgeHead != knowledgeInventoryRevision {
				return QuestionPage{}, ErrInventoryChanged
			}
			var unanswered int
			err = tx.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.interview_questions q JOIN supervisor.interview_sessions i USING(tenant_id,site_id,session_id) WHERE i.tenant_id=$1 AND i.site_id=$2 AND i.participant_issuer=$3 AND i.participant_subject=$4 AND q.answer_state='unanswered'`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject).Scan(&unanswered)
			if err != nil {
				return QuestionPage{}, err
			}
			for _, candidate := range candidates {
				if unanswered >= 3 {
					break
				}
				var decisionHead int64
				err = tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT current_revision FROM supervisor.knowledge_requirement_decision_heads WHERE tenant_id=$1 AND site_id=$2 AND inventory_id=$3 AND inventory_revision=$4 AND requirement_id=$5),0)`, sc.TenantID, sc.SiteID, current.inventoryID, candidate.decisionInventoryRevision, candidate.requirementID.String()).Scan(&decisionHead)
				if err != nil {
					return QuestionPage{}, err
				}
				if decisionHead != candidate.decisionRevision {
					return QuestionPage{}, ErrInventoryChanged
				}
				var exists bool
				err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM supervisor.interview_questions WHERE tenant_id=$1 AND site_id=$2 AND session_id=$3 AND requirement_id=$4 AND evidence_digest=$5)`, sc.TenantID, sc.SiteID, id, candidate.requirementID.String(), candidate.digest).Scan(&exists)
				if err != nil {
					return QuestionPage{}, err
				}
				if exists {
					continue
				}
				_, err = tx.ExecContext(ctx, `INSERT INTO supervisor.interview_questions(tenant_id,site_id,session_id,question_id,inventory_id,inventory_revision,requirement_id,evidence_digest,role,prompt,source_span_ids,reason,answer_state,revision) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'[]'::jsonb,$11,'unanswered',1)`, sc.TenantID, sc.SiteID, id, uuid.NewString(), current.inventoryID, current.inventoryRevision, candidate.requirementID.String(), candidate.digest, candidate.role, candidate.prompt, candidate.reason)
				if err != nil {
					return QuestionPage{}, err
				}
				unanswered++
			}
		}
	}
	if cursor == "" {
		cursor = uuid.Nil.String()
	}
	rows, err := tx.QueryContext(ctx, `SELECT question_id::text,revision,requirement_id::text,role,prompt,source_span_ids,reason,answer_state,snoozed_until,prompt_origin FROM supervisor.interview_questions WHERE tenant_id=$1 AND site_id=$2 AND session_id=$3 AND question_id>$4 ORDER BY question_id LIMIT $5`, sc.TenantID, sc.SiteID, id, cursor, limit+1)
	if err != nil {
		return QuestionPage{}, err
	}
	out := QuestionPage{Questions: []Question{}}
	for rows.Next() {
		var q Question
		var questionID, requirementID string
		var role, reason, state, origin string
		var spans []byte
		var snoozed sql.NullTime
		if err = rows.Scan(&questionID, &q.Revision, &requirementID, &role, &q.Prompt, &spans, &reason, &state, &snoozed, &origin); err != nil {
			rows.Close()
			return QuestionPage{}, err
		}
		q.QuestionID = uuid.MustParse(questionID)
		q.RequirementID = uuid.MustParse(requirementID)
		q.Role, q.Reason, q.AnswerState, q.PromptOrigin = InterviewRole(role), QuestionReason(reason), AnswerState(state), PromptOrigin(origin)
		if err = json.Unmarshal(spans, &q.SourceSpanIDs); err != nil {
			rows.Close()
			return QuestionPage{}, err
		}
		if q.SourceSpanIDs == nil {
			q.SourceSpanIDs = []uuid.UUID{}
		}
		if snoozed.Valid {
			value := snoozed.Time
			q.SnoozedUntil = &value
		}
		out.Questions = append(out.Questions, q)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return QuestionPage{}, err
	}
	if len(out.Questions) > limit {
		out.Questions = out.Questions[:limit]
		out.NextCursor = out.Questions[len(out.Questions)-1].QuestionID.String()
	}
	if err = audit(ctx, tx, a, sc, id, nil, "questions.read"); err != nil {
		return QuestionPage{}, err
	}
	return out, s.finish(ctx, tx, a)
}

func answerResult(ctx context.Context, tx *sql.Tx, sc Scope, sessionID string, a Actor, clientID string) (Answer, string, error) {
	var out Answer
	var answerID, questionID, sourceID, spanID, digest string
	err := tx.QueryRowContext(ctx, `SELECT a.client_answer_id::text,a.question_id::text,a.revision,a.source_id::text,a.source_revision,a.source_span_id::text,a.created_at,a.request_sha256
 FROM supervisor.interview_answers a JOIN supervisor.interview_sessions i USING(tenant_id,site_id,session_id)
 WHERE a.tenant_id=$1 AND a.site_id=$2 AND a.client_answer_id=$3 AND a.session_id=$4 AND i.participant_issuer=$5 AND i.participant_subject=$6`, sc.TenantID, sc.SiteID, clientID, sessionID, a.Issuer, a.Subject).Scan(&answerID, &questionID, &out.Revision, &sourceID, &out.SourceRevision, &spanID, &out.CreatedAt, &digest)
	if errors.Is(err, sql.ErrNoRows) {
		return Answer{}, "", ErrNotFound
	}
	if err != nil {
		return Answer{}, "", err
	}
	out.ClientAnswerID, out.QuestionID, out.SourceID, out.SourceSpanID = uuid.MustParse(answerID), uuid.MustParse(questionID), uuid.MustParse(sourceID), uuid.MustParse(spanID)
	return out, digest, nil
}

func (s *Service) Answer(ctx context.Context, a Actor, sc Scope, sessionID string, in AnswerInput, key string) (Answer, error) {
	if !validID(sessionID) || in.Validate() != nil || !validMutationKey(key) {
		return Answer{}, ErrInterviewInvalid
	}
	tx, _, err := s.begin(ctx, a, sc, true)
	if err != nil {
		return Answer{}, err
	}
	defer tx.Rollback()
	sessionRow, err := loadSession(ctx, tx, sc, a, sessionID, true)
	if err != nil {
		return Answer{}, err
	}
	digest := requestHash("answer", struct {
		SessionID string
		Input     AnswerInput
	}{sessionID, in})
	if body, replayed, e := receipt(ctx, tx, a, sc, key, digest); e != nil {
		return Answer{}, e
	} else if replayed {
		var out Answer
		if e = json.Unmarshal(body, &out); e != nil {
			return Answer{}, e
		}
		if e = audit(ctx, tx, a, sc, sessionID, in.QuestionID.String(), "mutation.replayed"); e != nil {
			return Answer{}, e
		}
		return out, s.finish(ctx, tx, a)
	}
	var anyAnswer bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM supervisor.interview_answers WHERE tenant_id=$1 AND site_id=$2 AND client_answer_id=$3)`, sc.TenantID, sc.SiteID, in.ClientAnswerID.String()).Scan(&anyAnswer); err != nil {
		return Answer{}, err
	}
	if anyAnswer {
		duplicate, savedDigest, e := answerResult(ctx, tx, sc, sessionID, a, in.ClientAnswerID.String())
		if e != nil {
			var sameParticipant bool
			if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM supervisor.interview_answers a JOIN supervisor.interview_sessions i USING(tenant_id,site_id,session_id) WHERE a.tenant_id=$1 AND a.site_id=$2 AND a.client_answer_id=$3 AND i.participant_issuer=$4 AND i.participant_subject=$5)`, sc.TenantID, sc.SiteID, in.ClientAnswerID.String(), a.Issuer, a.Subject).Scan(&sameParticipant); err != nil {
				return Answer{}, err
			}
			if sameParticipant {
				return Answer{}, ErrConflict
			}
			return Answer{}, ErrNotFound
		}
		if savedDigest != digest || duplicate.QuestionID != in.QuestionID {
			return Answer{}, ErrConflict
		}
		if err = saveReceipt(ctx, tx, a, sc, key, "answer", digest, sessionID, duplicate); err != nil {
			return Answer{}, err
		}
		if err = audit(ctx, tx, a, sc, sessionID, in.QuestionID.String(), "mutation.replayed"); err != nil {
			return Answer{}, err
		}
		return duplicate, s.finish(ctx, tx, a)
	}
	if sessionRow.Status != StatusActive {
		return Answer{}, ErrLifecycle
	}
	var questionRevision int64
	var role, answerState string
	var sourceID sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT q.revision,q.role,q.answer_state,q.answer_source_id::text FROM supervisor.interview_questions q WHERE q.tenant_id=$1 AND q.site_id=$2 AND q.session_id=$3 AND q.question_id=$4 AND q.role=$5 FOR UPDATE`, sc.TenantID, sc.SiteID, sessionID, in.QuestionID.String(), string(sessionRow.Role)).Scan(&questionRevision, &role, &answerState, &sourceID)
	if errors.Is(err, sql.ErrNoRows) {
		return Answer{}, ErrNotFound
	}
	if err != nil {
		return Answer{}, err
	}
	if InterviewRole(role) != sessionRow.Role {
		return Answer{}, ErrNotFound
	}
	if questionRevision != in.QuestionRevision {
		return Answer{}, ErrRevision
	}
	if AnswerState(answerState) == AnswerDismissed {
		return Answer{}, ErrLifecycle
	}
	var answerRevision int64
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(max(revision),0)+1 FROM supervisor.interview_answers WHERE tenant_id=$1 AND site_id=$2 AND question_id=$3`, sc.TenantID, sc.SiteID, in.QuestionID.String()).Scan(&answerRevision)
	if err != nil {
		return Answer{}, err
	}
	answerSourceID := uuid.NewString()
	if sourceID.Valid {
		answerSourceID = sourceID.String
	}
	var sourceRevision int64
	var revoked bool
	err = tx.QueryRowContext(ctx, `SELECT current_revision,revoked OR EXISTS(SELECT 1 FROM supervisor.evidence_revocations v WHERE (v.tenant_id,v.site_id,v.source_id)=(h.tenant_id,h.site_id,h.source_id)) FROM supervisor.evidence_heads h WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3 FOR UPDATE`, sc.TenantID, sc.SiteID, answerSourceID).Scan(&sourceRevision, &revoked)
	newSource := errors.Is(err, sql.ErrNoRows)
	if err != nil && !newSource {
		return Answer{}, err
	}
	if revoked {
		return Answer{}, ErrNotFound
	}
	if newSource {
		sourceRevision = 1
		_, err = tx.ExecContext(ctx, `INSERT INTO supervisor.evidence_heads(tenant_id,site_id,source_id,current_revision,state_revision) VALUES($1,$2,$3,1,1)`, sc.TenantID, sc.SiteID, answerSourceID)
	} else {
		sourceRevision++
		_, err = tx.ExecContext(ctx, `UPDATE supervisor.evidence_heads SET current_revision=current_revision+1,state_revision=state_revision+1 WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3`, sc.TenantID, sc.SiteID, answerSourceID)
	}
	if err != nil {
		return Answer{}, err
	}
	textHash := sha256.Sum256([]byte(in.Text))
	if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.evidence_revisions(tenant_id,site_id,source_id,revision,kind,title,body,sha256,byte_length,rune_length,actor_issuer,actor_subject) VALUES($1,$2,$3,$4,'interview','Interview answer',$5,$6,$7,$8,$9,$10)`, sc.TenantID, sc.SiteID, answerSourceID, sourceRevision, in.Text, hex.EncodeToString(textHash[:]), len(in.Text), utf8.RuneCountInString(in.Text), a.Issuer, a.Subject); err != nil {
		return Answer{}, err
	}
	spanID := uuid.NewString()
	if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.evidence_spans(tenant_id,site_id,id,source_id,revision,start_rune,end_rune,actor_issuer,actor_subject) VALUES($1,$2,$3,$4,$5,0,$6,$7,$8)`, sc.TenantID, sc.SiteID, spanID, answerSourceID, sourceRevision, utf8.RuneCountInString(in.Text), a.Issuer, a.Subject); err != nil {
		return Answer{}, err
	}
	event := "source.revised"
	if newSource {
		event = "source.created"
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.evidence_audit(tenant_id,site_id,source_id,revision,actor_issuer,actor_subject,event_type) VALUES($1,$2,$3,$4,$5,$6,$7),($1,$2,$3,$4,$5,$6,'span.created')`, sc.TenantID, sc.SiteID, answerSourceID, sourceRevision, a.Issuer, a.Subject, event); err != nil {
		return Answer{}, err
	}
	out := Answer{ClientAnswerID: in.ClientAnswerID, QuestionID: in.QuestionID, Revision: answerRevision, SourceID: uuid.MustParse(answerSourceID), SourceRevision: sourceRevision, SourceSpanID: uuid.MustParse(spanID)}
	err = tx.QueryRowContext(ctx, `INSERT INTO supervisor.interview_answers(tenant_id,site_id,session_id,question_id,revision,question_revision,client_answer_id,source_id,source_revision,source_span_id,request_sha256) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) RETURNING created_at`, sc.TenantID, sc.SiteID, sessionID, in.QuestionID.String(), answerRevision, questionRevision, in.ClientAnswerID.String(), answerSourceID, sourceRevision, spanID, digest).Scan(&out.CreatedAt)
	if err != nil {
		return Answer{}, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE supervisor.interview_questions SET revision=revision+1,answer_state='answered',answer_source_id=COALESCE(answer_source_id,$5),updated_at=clock_timestamp() WHERE tenant_id=$1 AND site_id=$2 AND session_id=$3 AND question_id=$4`, sc.TenantID, sc.SiteID, sessionID, in.QuestionID.String(), answerSourceID)
	if err != nil {
		return Answer{}, err
	}
	if err = audit(ctx, tx, a, sc, sessionID, in.QuestionID.String(), "answer.recorded"); err != nil {
		return Answer{}, err
	}
	if err = saveReceipt(ctx, tx, a, sc, key, "answer", digest, sessionID, out); err != nil {
		return Answer{}, err
	}
	return out, s.finish(ctx, tx, a)
}

func ValidateAnswerText(value string) bool {
	return utf8.ValidString(value) && !strings.ContainsRune(value, 0) && utf8.RuneCountInString(value) >= 1 && utf8.RuneCountInString(value) <= 8000
}
