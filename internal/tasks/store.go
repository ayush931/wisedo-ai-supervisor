package tasks

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/session"
	"github.com/google/uuid"
)

var (
	// ErrNotFound: no such task or site for this principal (non-disclosing 404).
	ErrNotFound = errors.New("task unavailable")
	// ErrDenied: the session does not allow this (403).
	ErrDenied = errors.New("task permission denied")
	// ErrInvalid: the request is malformed (422).
	ErrInvalid = errors.New("invalid task request")
)

const (
	DefaultPageSize = 50
	MaxPageSize     = 100
)

type Principal struct{ Issuer, Subject, SessionHash string }
type Scope struct{ TenantID, SiteID string }

// ActorRef is a stored identity.
type ActorRef struct {
	Issuer  string `json:"issuer"`
	Subject string `json:"subject"`
}

// EventV1 is one append-only task event.
type EventV1 struct {
	Revision  int64     `json:"revision"`
	FromState *State    `json:"from_state"`
	ToState   State     `json:"to_state"`
	Event     string    `json:"event" contract:"max=64"`
	Reason    *string   `json:"reason" contract:"max=2000"`
	Actor     ActorRef  `json:"actor"`
	CreatedAt time.Time `json:"created_at"`
}

// TaskV1 is an internal task. AssigneeSubject identifies the assigned site member; assignee_id is
// the stable actor UUID the approved payload named.
type TaskV1 struct {
	ID              string     `json:"id" contract:"uuid"`
	ActionID        string     `json:"action_id" contract:"uuid"`
	CaseID          string     `json:"case_id" contract:"uuid"`
	AssetID         string     `json:"asset_id" contract:"uuid"`
	LineID          string     `json:"line_id" contract:"uuid"`
	OrderID         string     `json:"order_id" contract:"uuid"`
	AssigneeID      string     `json:"assignee_id" contract:"uuid"`
	AssigneeSubject string     `json:"assignee_subject" contract:"max=255"`
	Template        string     `json:"template" contract:"max=64"`
	Description     string     `json:"description" contract:"max=4000"`
	DueAt           time.Time  `json:"due_at"`
	State           State      `json:"state"`
	Revision        int64      `json:"revision"`
	CreatedAt       time.Time  `json:"created_at"`
	AcknowledgedAt  *time.Time `json:"acknowledged_at"`
	StartedAt       *time.Time `json:"started_at"`
	CompletedAt     *time.Time `json:"completed_at"`
	// CompletionEvidenceIDs are the evidence the current completion cites; empty unless the task
	// is completed. Earlier completions (before a reopen) stay in Completions.
	CompletionEvidenceIDs []string       `json:"completion_evidence_ids" contract:"uuid"`
	Completions           []CompletionV1 `json:"completions"`
	// Outcome is the measured outcome of the current completion (pending until decided); nil
	// unless the task is completed.
	Outcome  *OutcomeV1 `json:"outcome"`
	Events   []EventV1  `json:"events"`
	Replayed bool       `json:"replayed"`
}

// CompletionV1 is one immutable completion epoch: the note and the exact evidence revisions cited.
type CompletionV1 struct {
	Epoch        int64           `json:"epoch"`
	TaskRevision int64           `json:"task_revision"`
	Note         string          `json:"completion_note" contract:"max=4000"`
	Evidence     []EvidenceRefV1 `json:"evidence"`
	Actor        ActorRef        `json:"actor"`
	CreatedAt    time.Time       `json:"created_at"`
}

// EvidenceRefV1 pins one cited evidence source revision.
type EvidenceRefV1 struct {
	SourceID string `json:"source_id" contract:"uuid"`
	Revision int64  `json:"revision"`
}

// SummaryV1 is one row of GET /tasks.
type SummaryV1 struct {
	ID              string    `json:"id" contract:"uuid"`
	CaseID          string    `json:"case_id" contract:"uuid"`
	LineID          string    `json:"line_id" contract:"uuid"`
	AssigneeID      string    `json:"assignee_id" contract:"uuid"`
	AssigneeSubject string    `json:"assignee_subject" contract:"max=255"`
	State           State     `json:"state"`
	Revision        int64     `json:"revision"`
	DueAt           time.Time `json:"due_at"`
	CreatedAt       time.Time `json:"created_at"`
}

// PageV1 is a cursor page of tasks ordered by ID.
type PageV1 struct {
	Items      []SummaryV1 `json:"items"`
	NextCursor *string     `json:"next_cursor"`
}

// New is the task the executor creates from an approved payload. Every field comes from the
// payload the reviewers approved, plus the assignee identity resolved from the roster.
type New struct {
	ActionID, CaseID, AssetID, LineID, OrderID  string
	AssigneeID, AssigneeIssuer, AssigneeSubject string
	Template, Description                       string
	DueAt                                       time.Time
}

// CreateTx inserts a task at revision 1 and its first event inside the executor's transaction.
// The task ID is derived from the action ID, so even a hand-run retry of the same action names
// the same task (the table's UNIQUE(tenant_id, action_id) also refuses a second one).
func CreateTx(ctx context.Context, tx *sql.Tx, sc Scope, in New, actor ActorRef) (string, error) {
	id := TaskID(in.ActionID)
	if _, err := tx.ExecContext(ctx, `INSERT INTO supervisor.tasks(tenant_id,site_id,id,action_id,case_id,asset_id,line_id,order_id,assignee_actor_id,assignee_issuer,assignee_subject,template,description,due_at,state,revision)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,'assigned',1)`,
		sc.TenantID, sc.SiteID, id, in.ActionID, in.CaseID, in.AssetID, in.LineID, in.OrderID, in.AssigneeID, in.AssigneeIssuer, in.AssigneeSubject, in.Template, in.Description, in.DueAt); err != nil {
		return "", err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO supervisor.task_events(tenant_id,site_id,task_id,revision,from_state,to_state,event,actor_issuer,actor_subject) VALUES($1,$2,$3,1,NULL,'assigned','create',$4,$5)`,
		sc.TenantID, sc.SiteID, id, actor.Issuer, actor.Subject)
	return id, err
}

var taskNamespace = uuid.MustParse("4f2b7c1e-9d3a-5e6f-8a1b-277000000001")

// TaskID is the task identity of an action.
func TaskID(actionID string) string { return uuid.NewSHA1(taskNamespace, []byte(actionID)).String() }

// Service reads tasks for a verified browser principal.
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

func validID(v string) bool {
	id, err := uuid.Parse(v)
	return err == nil && id != uuid.Nil && id.String() == v
}

func validField(v string, max int) bool {
	return len(v) > 0 && len(v) <= max && strings.TrimSpace(v) == v && utf8.ValidString(v) && !strings.ContainsRune(v, 0)
}

// readScope is what a member may read: an operator reads only the tasks assigned to them
// (WIS-634); supervisors, owners and viewers read the site's tasks, as with cases.
type readScope struct {
	ownOnly         bool
	issuer, subject string
}

// begin opens a tenant read transaction for a live session of an active site member and returns
// that member's read scope.
func (s *Service) begin(ctx context.Context, a Principal, sc Scope) (*sql.Tx, readScope, error) {
	if !validID(sc.TenantID) || !validID(sc.SiteID) || !validField(a.Issuer, 512) || !validField(a.Subject, 255) || !validField(a.SessionHash, 128) {
		return nil, readScope{}, ErrDenied
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, readScope{}, err
	}
	fail := func(e error) (*sql.Tx, readScope, error) {
		_ = tx.Rollback()
		return nil, readScope{}, e
	}
	if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true),set_config('statement_timeout','10s',true)`, sc.TenantID); err != nil {
		return fail(err)
	}
	var issuer, subject string
	var expiry, seen, now time.Time
	err = tx.QueryRowContext(ctx, `SELECT issuer,subject,expires_at,last_seen_at,clock_timestamp() FROM supervisor.browser_sessions WHERE token_hash=$1`, a.SessionHash).Scan(&issuer, &subject, &expiry, &seen, &now)
	if err != nil || issuer != a.Issuer || subject != a.Subject || !expiry.After(now) || !seen.After(now.Add(-s.sessions.Idle)) {
		return fail(ErrDenied)
	}
	var role string
	err = tx.QueryRowContext(ctx, `SELECT role FROM supervisor.memberships WHERE tenant_id=$1 AND site_id=$2 AND issuer=$3 AND subject=$4 AND active`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return fail(ErrNotFound)
	}
	if err != nil {
		return fail(err)
	}
	return tx, readScope{ownOnly: role == "operator", issuer: a.Issuer, subject: a.Subject}, nil
}

const taskColumns = `id,action_id,case_id,asset_id,line_id,order_id,assignee_actor_id,assignee_subject,template,description,due_at,state,revision,created_at,acknowledged_at,started_at,completed_at`

// Get returns one task with its events. A task of another site or tenant, or (for an operator)
// a task assigned to someone else, is the same 404 as a missing one.
func (s *Service) Get(ctx context.Context, a Principal, sc Scope, id string) (TaskV1, error) {
	if !validID(id) {
		return TaskV1{}, ErrNotFound
	}
	tx, rs, err := s.begin(ctx, a, sc)
	if err != nil {
		return TaskV1{}, err
	}
	defer tx.Rollback()
	if rs.ownOnly {
		var own bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM supervisor.tasks WHERE tenant_id=$1 AND site_id=$2 AND id=$3 AND assignee_issuer=$4 AND assignee_subject=$5)`,
			sc.TenantID, sc.SiteID, id, rs.issuer, rs.subject).Scan(&own); err != nil {
			return TaskV1{}, err
		}
		if !own {
			return TaskV1{}, ErrNotFound
		}
	}
	out, err := Load(ctx, tx, sc, id)
	if err != nil {
		return out, err
	}
	return out, tx.Commit()
}

// Load reads a task and its events inside the caller's tenant transaction.
func Load(ctx context.Context, tx *sql.Tx, sc Scope, id string) (TaskV1, error) {
	var t TaskV1
	var ack, started, completed sql.NullTime
	err := tx.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM supervisor.tasks WHERE tenant_id=$1 AND site_id=$2 AND id=$3`, sc.TenantID, sc.SiteID, id).Scan(
		&t.ID, &t.ActionID, &t.CaseID, &t.AssetID, &t.LineID, &t.OrderID, &t.AssigneeID, &t.AssigneeSubject, &t.Template, &t.Description, &t.DueAt, &t.State, &t.Revision, &t.CreatedAt, &ack, &started, &completed)
	if errors.Is(err, sql.ErrNoRows) {
		return t, ErrNotFound
	}
	if err != nil {
		return t, err
	}
	t.AcknowledgedAt, t.StartedAt, t.CompletedAt = timePtr(ack), timePtr(started), timePtr(completed)
	t.DueAt, t.CreatedAt = t.DueAt.UTC(), t.CreatedAt.UTC()
	t.CompletionEvidenceIDs = []string{}
	rows, err := tx.QueryContext(ctx, `SELECT revision,from_state,to_state,event,reason,actor_issuer,actor_subject,created_at FROM supervisor.task_events WHERE tenant_id=$1 AND site_id=$2 AND task_id=$3 ORDER BY revision`, sc.TenantID, sc.SiteID, id)
	if err != nil {
		return t, err
	}
	defer rows.Close()
	t.Events = []EventV1{}
	for rows.Next() {
		var e EventV1
		var from, reason sql.NullString
		if err = rows.Scan(&e.Revision, &from, &e.ToState, &e.Event, &reason, &e.Actor.Issuer, &e.Actor.Subject, &e.CreatedAt); err != nil {
			return t, err
		}
		if from.Valid {
			f := State(from.String)
			e.FromState = &f
		}
		if reason.Valid {
			r := reason.String
			e.Reason = &r
		}
		e.CreatedAt = e.CreatedAt.UTC()
		t.Events = append(t.Events, e)
	}
	if err = rows.Err(); err != nil {
		return t, err
	}
	rows.Close()
	t.Completions = []CompletionV1{}
	// LEFT JOIN: an epoch without evidence rows (which the database refuses) would show, empty,
	// rather than silently disappear.
	crows, err := tx.QueryContext(ctx, `SELECT c.epoch,c.task_revision,c.completion_note,c.actor_issuer,c.actor_subject,c.created_at,e.source_id::text,e.revision
 FROM supervisor.task_completions c LEFT JOIN supervisor.task_completion_evidence e ON (e.tenant_id,e.site_id,e.task_id,e.epoch)=(c.tenant_id,c.site_id,c.task_id,c.epoch)
 WHERE c.tenant_id=$1 AND c.site_id=$2 AND c.task_id=$3 ORDER BY c.epoch,e.source_id`, sc.TenantID, sc.SiteID, id)
	if err != nil {
		return t, err
	}
	defer crows.Close()
	for crows.Next() {
		var c CompletionV1
		var source sql.NullString
		var revision sql.NullInt64
		if err = crows.Scan(&c.Epoch, &c.TaskRevision, &c.Note, &c.Actor.Issuer, &c.Actor.Subject, &c.CreatedAt, &source, &revision); err != nil {
			return t, err
		}
		if n := len(t.Completions); n == 0 || t.Completions[n-1].Epoch != c.Epoch {
			c.CreatedAt, c.Evidence = c.CreatedAt.UTC(), []EvidenceRefV1{}
			t.Completions = append(t.Completions, c)
		}
		if source.Valid {
			last := &t.Completions[len(t.Completions)-1]
			last.Evidence = append(last.Evidence, EvidenceRefV1{SourceID: source.String, Revision: revision.Int64})
		}
	}
	if err = crows.Err(); err != nil {
		return t, err
	}
	if n := len(t.Completions); n > 0 && t.State == Completed {
		for _, r := range t.Completions[n-1].Evidence {
			t.CompletionEvidenceIDs = append(t.CompletionEvidenceIDs, r.SourceID)
		}
	}
	t.Outcome, err = loadOutcome(ctx, tx, sc, t)
	return t, err
}

func timePtr(v sql.NullTime) *time.Time {
	if !v.Valid {
		return nil
	}
	t := v.Time.UTC()
	return &t
}

// List returns the site's tasks ordered by ID after cursor (an operator's own tasks only);
// caseID optionally narrows them to one case.
func (s *Service) List(ctx context.Context, a Principal, sc Scope, caseID, cursor string, limit int) (PageV1, error) {
	if limit == 0 {
		limit = DefaultPageSize
	}
	if limit < 1 || limit > MaxPageSize || (caseID != "" && !validID(caseID)) || (cursor != "" && !validID(cursor)) {
		return PageV1{}, ErrInvalid
	}
	tx, rs, err := s.begin(ctx, a, sc)
	if err != nil {
		return PageV1{}, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id,case_id,line_id,assignee_actor_id,assignee_subject,state,revision,due_at,created_at FROM supervisor.tasks
 WHERE tenant_id=$1 AND site_id=$2 AND ($3='' OR id>$3::uuid) AND ($4='' OR case_id=$4::uuid) AND (NOT $6 OR (assignee_issuer=$7 AND assignee_subject=$8)) ORDER BY id LIMIT $5`,
		sc.TenantID, sc.SiteID, cursor, caseID, limit+1, rs.ownOnly, rs.issuer, rs.subject)
	if err != nil {
		return PageV1{}, err
	}
	defer rows.Close()
	page := PageV1{Items: []SummaryV1{}}
	for rows.Next() {
		var t SummaryV1
		if err = rows.Scan(&t.ID, &t.CaseID, &t.LineID, &t.AssigneeID, &t.AssigneeSubject, &t.State, &t.Revision, &t.DueAt, &t.CreatedAt); err != nil {
			return PageV1{}, err
		}
		t.DueAt, t.CreatedAt = t.DueAt.UTC(), t.CreatedAt.UTC()
		page.Items = append(page.Items, t)
	}
	if err = rows.Err(); err != nil {
		return PageV1{}, err
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		next := page.Items[limit-1].ID
		page.NextCursor = &next
	}
	return page, tx.Commit()
}
