package cases

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// HumanWait is how long a case may wait on a person (a clarification in needs_input, a
// decision in awaiting_approval) before the workflow escalates it. There is no automatic
// approval.
const HumanWait = 24 * time.Hour

// WorkflowActor records transitions the case workflow makes, distinct from any human.
var WorkflowActor = ActorRef{Issuer: "supervisor:temporal", Subject: "case-workflow"}

// WorkflowRef identifies one case workflow. It carries IDs only: the workflow reads business
// state from PostgreSQL through activities, never from its own payload.
type WorkflowRef struct {
	TenantID string `json:"tenant_id"`
	SiteID   string `json:"site_id"`
	CaseID   string `json:"case_id"`
}

// WorkflowEvent is one outbox delivery to the workflow.
type WorkflowEvent struct {
	EventID  string `json:"event_id"`
	Kind     string `json:"kind"`
	Revision int64  `json:"revision"`
}

// WorkflowStatus is what the workflow needs to decide its next wait.
type WorkflowStatus struct {
	State        State `json:"state"`
	Revision     int64 `json:"revision"`
	AgentEnabled bool  `json:"agent_enabled"`
	Terminal     bool  `json:"terminal"`
	// WaitingOnHuman is true in needs_input and awaiting_approval; WaitDeadline is then the
	// database time at which the case escalates.
	WaitingOnHuman bool      `json:"waiting_on_human"`
	WaitDeadline   time.Time `json:"wait_deadline"`
}

// OutboxRow is one undelivered workflow delivery.
type OutboxRow struct {
	ID    string
	Ref   WorkflowRef
	Event WorkflowEvent
}

func waitingOnHuman(s State) bool { return s == NeedsInput || s == AwaitingApproval }

func (r WorkflowRef) valid() bool {
	return validID(r.TenantID) && validID(r.SiteID) && validID(r.CaseID)
}

func (s *Service) workflowTx(ctx context.Context, tenantID, siteID string, write bool) (*sql.Tx, time.Time, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, time.Time{}, err
	}
	now, err := scopeTx(ctx, tx, Scope{TenantID: tenantID, SiteID: siteID}, write)
	if err != nil {
		_ = tx.Rollback()
		return nil, time.Time{}, err
	}
	return tx, now, nil
}

// status reads the case state and, while it waits on a person, the deadline measured from
// the transition that started the wait.
func status(ctx context.Context, tx *sql.Tx, ref WorkflowRef, lock bool) (WorkflowStatus, error) {
	var out WorkflowStatus
	var since time.Time
	query := `SELECT c.state,c.revision,t.created_at FROM supervisor.cases c JOIN supervisor.case_transitions t ON (t.tenant_id,t.site_id,t.case_id,t.revision)=(c.tenant_id,c.site_id,c.id,c.revision) WHERE c.tenant_id=$1 AND c.site_id=$2 AND c.id=$3`
	if lock {
		query += ` FOR UPDATE OF c`
	}
	err := tx.QueryRowContext(ctx, query, ref.TenantID, ref.SiteID, ref.CaseID).Scan(&out.State, &out.Revision, &since)
	if errors.Is(err, sql.ErrNoRows) {
		return out, ErrNotFound
	}
	if err != nil {
		return out, err
	}
	out.Terminal = Terminal(out.State)
	if out.WaitingOnHuman = waitingOnHuman(out.State); out.WaitingOnHuman {
		out.WaitDeadline = since.UTC().Add(HumanWait)
	}
	return out, nil
}

// WorkflowStatus reads the case for its workflow.
func (s *Service) WorkflowStatus(ctx context.Context, ref WorkflowRef) (WorkflowStatus, error) {
	if !ref.valid() {
		return WorkflowStatus{}, ErrInvalid
	}
	tx, _, err := s.workflowTx(ctx, ref.TenantID, ref.SiteID, false)
	if err != nil {
		return WorkflowStatus{}, err
	}
	defer tx.Rollback()
	out, err := status(ctx, tx, ref, false)
	if err != nil {
		return out, err
	}
	return out, tx.Commit()
}

// RecordProposal applies one stored investigation result. The proposal head is locked only
// after the site and case locks, matching Submit and the human case-mutation order.
func (s *Service) RecordProposal(ctx context.Context, ref WorkflowRef, expectedRevision int64, runID, status string, reasons []string, proposalID string) (CaseV1, error) {
	if !ref.valid() || expectedRevision < 1 || !validID(runID) {
		return CaseV1{}, ErrInvalid
	}
	if status == "proposed" {
		if !validID(proposalID) || len(reasons) != 0 {
			return CaseV1{}, ErrInvalid
		}
	} else if status == "needs_input" || status == "escalated" || status == "refused" {
		if proposalID != "" || len(reasons) == 0 {
			return CaseV1{}, ErrInvalid
		}
	} else {
		return CaseV1{}, ErrInvalid
	}
	reason := strings.Join(reasons, ",")
	if status != "proposed" && !validReason(reason) {
		return CaseV1{}, ErrInvalid
	}
	tx, _, err := s.workflowTx(ctx, ref.TenantID, ref.SiteID, true)
	if err != nil {
		return CaseV1{}, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT 1 FROM supervisor.cases WHERE tenant_id=$1 AND site_id=$2 AND id=$3 FOR UPDATE`, ref.TenantID, ref.SiteID, ref.CaseID); err != nil {
		return CaseV1{}, err
	}
	sc := Scope{TenantID: ref.TenantID, SiteID: ref.SiteID}
	current, err := load(ctx, tx, sc, ref.CaseID)
	if err != nil {
		return current, err
	}
	if current.Revision > expectedRevision {
		if status == "proposed" && current.State == AwaitingApproval {
			if err = lockGeneratedProposal(ctx, tx, sc, ref.CaseID, expectedRevision, runID, proposalID, false); err != nil {
				return current, err
			}
		}
		return current, tx.Commit()
	}
	if current.Revision != expectedRevision {
		return CaseV1{}, ErrRevision
	}
	if current.State != Investigating {
		return CaseV1{}, ErrIllegalTransition
	}
	if status == "proposed" {
		if err = lockGeneratedProposal(ctx, tx, sc, ref.CaseID, expectedRevision, runID, proposalID, true); err != nil {
			return CaseV1{}, err
		}
		revision, e := move(ctx, tx, sc, current, ProposalValidated, Guards{ProposalValid: true}, "AGENT_PROPOSAL_VALIDATED", WorkflowActor, "")
		if e != nil {
			return CaseV1{}, e
		}
		current.Revision, current.State = revision, Proposed
		if _, err = move(ctx, tx, sc, current, ApprovalRequested, Guards{ApprovalRequestExists: true}, "AGENT_APPROVAL_REQUESTED", WorkflowActor, ""); err != nil {
			return CaseV1{}, err
		}
	} else {
		event, to := EvidenceMissing, NeedsInput
		if status == "escalated" || status == "refused" {
			event, to = Fatal, Escalated
		}
		if _, err = move(ctx, tx, sc, current, event, Guards{ReasonProvided: true}, reason, WorkflowActor, ""); err != nil {
			return CaseV1{}, err
		}
		current.Revision, current.State = current.Revision+1, to
	}
	current, err = load(ctx, tx, sc, ref.CaseID)
	if err != nil {
		return current, err
	}
	return current, tx.Commit()
}

func lockGeneratedProposal(ctx context.Context, tx *sql.Tx, sc Scope, caseID string, caseRevision int64, runID, proposalID string, requireLive bool) error {
	var storedCase, state string
	var revision int64
	var generated sql.NullString
	var unexpired bool
	err := tx.QueryRowContext(ctx, `SELECT r.case_id,r.case_revision,r.generated_by_run_id,s.state,r.expires_at>clock_timestamp() FROM supervisor.approval_states s JOIN supervisor.proposal_revisions r
 ON (r.tenant_id,r.site_id,r.proposal_id,r.revision)=(s.tenant_id,s.site_id,s.proposal_id,s.current_revision)
 WHERE s.tenant_id=$1 AND s.site_id=$2 AND s.proposal_id=$3 FOR UPDATE OF s`, sc.TenantID, sc.SiteID, proposalID).Scan(&storedCase, &revision, &generated, &state, &unexpired)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if storedCase != caseID || revision != caseRevision || !generated.Valid || generated.String != runID ||
		(requireLive && ((state != "pending" && state != "approved") || !unexpired)) {
		return ErrRevision
	}
	return nil
}

// RecordWorkflowEvent records that the workflow processed an event and reports whether it had
// already been processed, so a duplicate or replayed signal has no second effect.
func (s *Service) RecordWorkflowEvent(ctx context.Context, ref WorkflowRef, ev WorkflowEvent) (bool, error) {
	if !ref.valid() || !validID(ev.EventID) || (ev.Kind != "start" && ev.Kind != "signal" && ev.Kind != "cancel") || ev.Revision < 1 {
		return false, ErrInvalid
	}
	tx, _, err := s.workflowTx(ctx, ref.TenantID, ref.SiteID, false)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if _, err = status(ctx, tx, ref, false); err != nil {
		return false, err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO supervisor.case_workflow_events(tenant_id,site_id,case_id,event_id,kind,case_revision) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`, ref.TenantID, ref.SiteID, ref.CaseID, ev.EventID, ev.Kind, ev.Revision)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 0, tx.Commit()
}

// EscalateOnDeadline moves a case that is still waiting on a person at expectedRevision to
// escalated once the database clock has passed its wait deadline. It is fenced: a case that
// moved on (another revision or state) or whose deadline has not passed is left unchanged and
// false is returned, so a late or repeated timer has no effect.
func (s *Service) EscalateOnDeadline(ctx context.Context, ref WorkflowRef, expectedRevision int64) (bool, error) {
	if !ref.valid() || expectedRevision < 1 {
		return false, ErrInvalid
	}
	tx, now, err := s.workflowTx(ctx, ref.TenantID, ref.SiteID, true)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	st, err := status(ctx, tx, ref, true)
	if err != nil {
		return false, err
	}
	if st.Revision != expectedRevision || !st.WaitingOnHuman || now.Before(st.WaitDeadline) {
		return false, tx.Commit()
	}
	sc := Scope{TenantID: ref.TenantID, SiteID: ref.SiteID}
	c := CaseV1{ID: ref.CaseID, State: st.State, Revision: st.Revision}
	if _, err = move(ctx, tx, sc, c, WaitDeadline, Guards{ReasonProvided: true}, "HUMAN_WAIT_DEADLINE", WorkflowActor, ""); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// PendingOutbox returns up to limit undelivered workflow deliveries of a tenant, oldest first.
func (s *Service) PendingOutbox(ctx context.Context, tenantID string, limit int) ([]OutboxRow, error) {
	if !validID(tenantID) || limit < 1 || limit > MaxPageSize {
		return nil, ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, tenantID); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,site_id,case_id,event_id,kind,case_revision FROM supervisor.case_outbox WHERE tenant_id=$1 AND delivered_at IS NULL ORDER BY created_at,id LIMIT $2`, tenantID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]OutboxRow, 0)
	for rows.Next() {
		r := OutboxRow{Ref: WorkflowRef{TenantID: tenantID}}
		if err = rows.Scan(&r.ID, &r.Ref.SiteID, &r.Ref.CaseID, &r.Event.EventID, &r.Event.Kind, &r.Event.Revision); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

// MarkDelivered records that one outbox row reached Temporal. A row already marked is left as
// it was.
func (s *Service) MarkDelivered(ctx context.Context, row OutboxRow) error {
	if !row.Ref.valid() || !validID(row.ID) {
		return ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, row.Ref.TenantID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE supervisor.case_outbox SET delivered_at=clock_timestamp() WHERE tenant_id=$1 AND site_id=$2 AND id=$3 AND delivered_at IS NULL`, row.Ref.TenantID, row.Ref.SiteID, row.ID); err != nil {
		return err
	}
	return tx.Commit()
}

// ReconcileWorkflowEvents records in the ledger every outbox delivery of the case that is not
// recorded yet, and returns how many it added. Only delivered rows count: an undelivered row
// has not reached any run, and recording it would mark it processed before the workflow saw it
// (migration 046: an event is recorded when the workflow acts on it). A run that failed before
// recording the signals it received loses them (a retried run does not inherit them, and the
// relay has already marked them delivered); the outbox, not the signal, is the source of truth,
// so each run starts here.
func (s *Service) ReconcileWorkflowEvents(ctx context.Context, ref WorkflowRef) (int, error) {
	if !ref.valid() {
		return 0, ErrInvalid
	}
	tx, _, err := s.workflowTx(ctx, ref.TenantID, ref.SiteID, false)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err = status(ctx, tx, ref, false); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO supervisor.case_workflow_events(tenant_id,site_id,case_id,event_id,kind,case_revision)
 SELECT tenant_id,site_id,case_id,event_id,kind,case_revision FROM supervisor.case_outbox WHERE tenant_id=$1 AND site_id=$2 AND case_id=$3 AND delivered_at IS NOT NULL
 ON CONFLICT DO NOTHING`, ref.TenantID, ref.SiteID, ref.CaseID)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), tx.Commit()
}
