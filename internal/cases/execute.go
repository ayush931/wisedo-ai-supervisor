package cases

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/approvals"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/roster"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/tasks"
	"github.com/google/uuid"
)

// ErrEvidenceUnverified: the executor was built without a bound-evidence check, so it cannot
// re-evaluate what the approvers signed (rule 05). Nothing is consumed or invalidated.
var ErrEvidenceUnverified = errors.New("cases: executor has no bound-evidence check")

// ExecutorActor records the deterministic executor's work: consuming an approval, creating the
// task and moving the case (WIS-277). It is a service identity; it never approves anything.
var ExecutorActor = ActorRef{Issuer: "supervisor:executor", Subject: "approved-action-executor"}

// Execution outcomes.
const (
	ExecutionCommitted = "committed" // approval consumed, task created, case observing
	ExecutionRefused   = "refused"   // the executor refused an approved proposal; case needs input
	ExecutionClosed    = "closed"    // the decision ended without approval; case needs input
	ExecutionWaiting   = "waiting"   // the decision is still open, or execution is not enabled
)

// Execution is the result for one case awaiting approval.
type Execution struct {
	SiteID, CaseID, ProposalID string
	Outcome                    string
	ActionID, TaskID, Reason   string
}

// Scope refusal codes from the executor's own predicates (rule 06: a specific reason).
const (
	refusalAssignee = "ASSIGNEE_NOT_CONFIRMED"
	refusalLineage  = "LINEAGE_INVALID"
)

// ExecuteApproved settles every case of the tenant that awaits an approval whose decision is no
// longer open, up to limit cases, each in its own transaction so one failure never holds back
// another. With enabled false (execution not commissioned) approvals are never consumed: only
// rejections, revocations and expiries are carried to the case.
func (s *Service) ExecuteApproved(ctx context.Context, tenantID string, enabled bool, limit int) ([]Execution, error) {
	if !validID(tenantID) || limit < 1 {
		return nil, ErrInvalid
	}
	pending, err := s.awaitingDecisions(ctx, tenantID, enabled, limit)
	if err != nil {
		return nil, err
	}
	out := make([]Execution, 0, len(pending))
	var failed []error
	for _, p := range pending {
		e, err := s.executeOne(ctx, Scope{TenantID: tenantID, SiteID: p.SiteID}, p.CaseID, p.ProposalID, enabled)
		if err != nil {
			failed = append(failed, err)
			continue
		}
		out = append(out, e)
	}
	return out, errors.Join(failed...)
}

// awaitingDecisions lists cases in awaiting_approval with the proposal they wait on (the newest
// proposal of the case), when its decision is closed, its window has ended, or (enabled) it is
// approved. A tenant-wide read: the RLS tenant setting scopes it.
func (s *Service) awaitingDecisions(ctx context.Context, tenantID string, enabled bool, limit int) ([]Execution, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true),set_config('statement_timeout','10s',true)`, tenantID); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT site_id,case_id,proposal_id FROM (
 SELECT DISTINCT ON (c.id) c.site_id,c.id AS case_id,s.proposal_id,s.state,r.expires_at FROM supervisor.cases c
 JOIN supervisor.proposal_revisions r ON (r.tenant_id,r.site_id,r.case_id)=(c.tenant_id,c.site_id,c.id)
 JOIN supervisor.approval_states s ON (s.tenant_id,s.site_id,s.proposal_id,s.current_revision)=(r.tenant_id,r.site_id,r.proposal_id,r.revision)
 WHERE c.tenant_id=$1 AND c.state='awaiting_approval' ORDER BY c.id,r.issued_at DESC,r.proposal_id) p
 WHERE (state<>'pending' AND (state<>'approved' OR $2 OR expires_at<=clock_timestamp())) OR (state='pending' AND expires_at<=clock_timestamp())
 ORDER BY case_id LIMIT $3`, tenantID, enabled, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Execution
	for rows.Next() {
		var e Execution
		if err = rows.Scan(&e.SiteID, &e.CaseID, &e.ProposalID); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// executeOne runs one case in a single transaction with the global lock order: site scope, the
// approvers' and assignee's memberships in ID order, the case, then the proposal (inside
// approvals.Settle), then the action ledger. A crash before commit leaves no task; after commit,
// the case is observing and a retry finds nothing to do. Retrying never creates a second action
// or task: UNIQUE(proposal, revision, action_kind) and UNIQUE(action_id) refuse it.
func (s *Service) executeOne(ctx context.Context, sc Scope, caseID, proposalID string, enabled bool) (Execution, error) {
	out := Execution{SiteID: sc.SiteID, CaseID: caseID, ProposalID: proposalID, Outcome: ExecutionWaiting}
	tx, now, err := s.workflowTx(ctx, sc.TenantID, sc.SiteID, true)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	// The tenant list was discovered at the start of the tick; a tenant deactivated since then
	// gets nothing executed. Its approvals are left as they are (only a due expiry is recorded),
	// exactly as when execution is switched off (WIS-277 review M3).
	var active bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM supervisor.active_tenant_ids() a(id) WHERE a.id=$1)`, sc.TenantID).Scan(&active); err != nil {
		return out, err
	}
	enabled = enabled && active

	// The payload is immutable, so reading who it names needs no lock; locking their memberships
	// first keeps a concurrent role change from slipping between the check and the commit.
	var assigneeID, lineID string
	err = tx.QueryRowContext(ctx, `SELECT r.canonical_payload::jsonb->'body'->>'assignee_id',r.canonical_payload::jsonb->'body'->>'line_id' FROM supervisor.approval_states s
 JOIN supervisor.proposal_revisions r ON (r.tenant_id,r.site_id,r.proposal_id,r.revision)=(s.tenant_id,s.site_id,s.proposal_id,s.current_revision)
 WHERE s.tenant_id=$1 AND s.site_id=$2 AND s.proposal_id=$3`, sc.TenantID, sc.SiteID, proposalID).Scan(&assigneeID, &lineID)
	if errors.Is(err, sql.ErrNoRows) {
		return out, ErrNotFound
	}
	if err != nil {
		return out, err
	}
	actors, err := approvals.Approvers(ctx, tx, sc.TenantID, sc.SiteID, proposalID)
	if err != nil {
		return out, err
	}
	var assignee approvals.ActorRef
	err = tx.QueryRowContext(ctx, `SELECT assignee_issuer,assignee_subject FROM supervisor.maintenance_roster_heads WHERE tenant_id=$1 AND site_id=$2 AND line_id=$3 AND assignee_actor_id=$4`,
		sc.TenantID, sc.SiteID, lineID, assigneeID).Scan(&assignee.Issuer, &assignee.Subject)
	switch {
	case err == nil:
		actors = append(actors, assignee)
	case errors.Is(err, sql.ErrNoRows):
		assignee = approvals.ActorRef{} // refused below as ASSIGNEE_NOT_CONFIRMED
	default:
		return out, err
	}
	slices.SortFunc(actors, func(a, b approvals.ActorRef) int {
		if c := strings.Compare(a.Issuer, b.Issuer); c != 0 {
			return c
		}
		return strings.Compare(a.Subject, b.Subject)
	})
	for _, a := range slices.Compact(actors) {
		if _, err = tx.ExecContext(ctx, `SELECT 1 FROM supervisor.memberships WHERE tenant_id=$1 AND site_id=$2 AND issuer=$3 AND subject=$4 FOR SHARE`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject); err != nil {
			return out, err
		}
	}
	if _, err = tx.ExecContext(ctx, `SELECT 1 FROM supervisor.cases WHERE tenant_id=$1 AND site_id=$2 AND id=$3 FOR UPDATE`, sc.TenantID, sc.SiteID, caseID); err != nil {
		return out, err
	}
	current, err := load(ctx, tx, sc, caseID)
	if err != nil {
		return out, err
	}
	if current.State != AwaitingApproval {
		return out, tx.Commit() // another executor or a person moved it first
	}

	check := func(p approvals.Payload) (string, error) {
		if p.CaseID != current.ID || p.LineID != current.LineID || p.OrderID != current.OrderID || p.AssetID != current.LineID {
			// v1 tasks inspect the case line itself; a machine asset needs an approved
			// part_of_asset lineage check before it can be accepted (fail closed until then).
			return refusalLineage, nil
		}
		if assignee.Issuer == "" {
			return refusalAssignee, nil
		}
		confirmed, err := roster.ConfirmedTx(ctx, tx, sc.TenantID, sc.SiteID, current.LineID)
		if err != nil {
			return "", err
		}
		if !slices.ContainsFunc(confirmed, func(c roster.Confirmed) bool { return c.AssigneeID == p.AssigneeID }) {
			return refusalAssignee, nil
		}
		// Every bound evidence revision is re-evaluated at dispatch (rule 05). An executor built
		// without the check cannot verify it: that is a construction error, so the run fails and
		// rolls back, and the approval stays for a correctly built executor (review minor 1).
		if s.boundEvidence == nil {
			return "", ErrEvidenceUnverified
		}
		return s.boundEvidence(ctx, tx, sc.TenantID, sc.SiteID, p)
	}
	d, err := approvals.Settle(ctx, tx, sc.TenantID, sc.SiteID, proposalID, now, approvals.ActorRef(ExecutorActor), enabled, check)
	if err != nil {
		return out, err
	}

	switch {
	case d.State == approvals.Consumed && d.Payload.ProposalID == proposalID:
		out.ActionID = uuid.NewString()
		if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.approved_actions(tenant_id,site_id,id,proposal_id,proposal_revision,action_kind,canonical_payload_sha256,case_id,state,actor_issuer,actor_subject) VALUES($1,$2,$3,$4,$5,$6,$7,$8,'committed',$9,$10)`,
			sc.TenantID, sc.SiteID, out.ActionID, proposalID, d.ProposalRevision, d.Payload.ActionKind, d.Digest, caseID, ExecutorActor.Issuer, ExecutorActor.Subject); err != nil {
			return out, err
		}
		p := d.Payload
		out.TaskID, err = tasks.CreateTx(ctx, tx, tasks.Scope(sc), tasks.New{ActionID: out.ActionID, CaseID: caseID, AssetID: p.AssetID, LineID: p.LineID, OrderID: p.OrderID,
			AssigneeID: p.AssigneeID, AssigneeIssuer: assignee.Issuer, AssigneeSubject: assignee.Subject, Template: p.Template, Description: p.Instructions, DueAt: p.DueAt}, tasks.ActorRef(ExecutorActor))
		if err != nil {
			return out, err
		}
		revision, err := move(ctx, tx, sc, current, ApprovalGranted, Guards{ApprovalEligible: true, RuntimeEnabled: true}, "APPROVAL_CONSUMED", ExecutorActor, "")
		if err != nil {
			return out, err
		}
		current.Revision, current.State = revision, Executing
		if revision, err = move(ctx, tx, sc, current, TaskAuthoritative, Guards{TaskExists: true}, "TASK_CREATED", ExecutorActor, ""); err != nil {
			return out, err
		}
		out.Outcome = ExecutionCommitted
		return out, s.signalAndCommit(ctx, tx, sc, caseID, revision)
	case d.State == approvals.Consumed:
		// A consumption without its own payload would commit a terminal approval with no task,
		// inverting "committed means a task exists" (rule 05): roll everything back instead.
		return out, errors.New("consumed approval does not match the proposal")
	case d.State == approvals.Pending || d.State == approvals.Approved:
		return out, tx.Commit() // an expiry may have been recorded; the decision is not closed
	case d.Reason != "":
		if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.approved_actions(tenant_id,site_id,id,proposal_id,proposal_revision,action_kind,canonical_payload_sha256,case_id,state,reason,actor_issuer,actor_subject) VALUES($1,$2,$3,$4,$5,'maintenance_investigation.create',$6,$7,'rejected',$8,$9,$10)`,
			sc.TenantID, sc.SiteID, uuid.NewString(), proposalID, d.ProposalRevision, d.Digest, caseID, d.Reason, ExecutorActor.Issuer, ExecutorActor.Subject); err != nil {
			return out, err
		}
		out.Outcome, out.Reason = ExecutionRefused, "DISPATCH_"+d.Reason
	default:
		out.Outcome, out.Reason = ExecutionClosed, "PROPOSAL_"+strings.ToUpper(string(d.State))
	}
	// A proposal that will not execute returns the case to needs_input with the reason.
	revision, err := move(ctx, tx, sc, current, ProposalRejected, Guards{ReasonProvided: true}, out.Reason, ExecutorActor, "")
	if err != nil {
		return out, err
	}
	return out, s.signalAndCommit(ctx, tx, sc, caseID, revision)
}

// signalAndCommit wakes the case workflow for the new revision and commits.
func (s *Service) signalAndCommit(ctx context.Context, tx *sql.Tx, sc Scope, caseID string, revision int64) error {
	if err := outbox(ctx, tx, sc, caseID, revision, "signal", transitionEvent(caseID, revision)); err != nil {
		return err
	}
	return tx.Commit()
}
