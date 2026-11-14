package cases

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/plantcontext"
)

// EvaluatorActor records triggers raised by automatic context evaluation.
var EvaluatorActor = ServiceActor{Issuer: "supervisor:evaluator", Subject: "shortfall-evaluator"}

// Evaluation outcomes recorded in case_context_evaluations.
const (
	OutcomeOpened           = "opened"
	OutcomeJoined           = "joined"
	OutcomeAlreadyTriggered = "already_triggered"
	OutcomeNotQualified     = "not_qualified"
	OutcomeNotApplicable    = "not_applicable"
)

// Evaluation is the recorded result for one created context.
type Evaluation struct {
	SiteID, ContextID, Outcome, CaseID string
}

// EvaluationBudget bounds one tenant's share of one evaluation pass (WIS-718). A pass stops
// starting new contexts once it has attempted Contexts of them, failures included, or their
// delta records reach Records; it always attempts at least one. It is checked between contexts,
// never inside one, so a context is never cut off mid-transaction.
type EvaluationBudget struct{ Contexts, Records int }

// DefaultEvaluationBudget is the worker's budget: 100 contexts or one maximal context's records.
var DefaultEvaluationBudget = EvaluationBudget{Contexts: 100, Records: plantcontext.MaxRecords}

// EvaluationCursor is where a tenant's next pass resumes: after the last context attempted.
type EvaluationCursor struct {
	createdAt time.Time
	id        string
}

// EvaluateCreatedContexts is one pass from the oldest unevaluated context with a budget of limit
// attempts.
func (s *Service) EvaluateCreatedContexts(ctx context.Context, tenantID string, actor ServiceActor, limit int) ([]Evaluation, error) {
	out, _, err := s.EvaluatePass(ctx, tenantID, actor, EvaluationBudget{Contexts: limit, Records: plantcontext.MaxRecords}, nil)
	return out, err
}

// EvaluatePass evaluates the tenant's context.created events that have no recorded evaluation,
// oldest first after the cursor (from the oldest when nil), within the budget. A READY context
// raises its triggers through the checked trigger path, so the shortfall rule, the line check,
// deduplication onto the active episode and idempotency are exactly the trigger's. Each context's
// outcome is recorded once; a context that fails records nothing and is retried later. The pass
// returns the cursor to resume from, nil once it reached the end of the queue (a pass that started
// after a cursor wraps once to the oldest), so the next pass continues after the contexts this
// one attempted: contexts that fail every time cost each pass part of its budget but cannot hold
// newer ones back. The failures are returned joined.
func (s *Service) EvaluatePass(ctx context.Context, tenantID string, actor ServiceActor, budget EvaluationBudget, after *EvaluationCursor) ([]Evaluation, *EvaluationCursor, error) {
	if !validID(tenantID) || budget.Contexts < 1 || budget.Contexts > MaxPageSize || budget.Records < 1 || !validField(actor.Issuer, 255) || !validField(actor.Subject, 255) {
		return nil, after, ErrInvalid
	}
	out := make([]Evaluation, 0, budget.Contexts)
	var failed []error
	seen := map[string]bool{}
	attempts, records := 0, 0
	started, wrapped := after, false
	for {
		pending, err := s.pendingContexts(ctx, tenantID, budget.Contexts, after)
		if err != nil {
			return out, after, errors.Join(append(failed, err)...)
		}
		for _, p := range pending {
			if attempts == budget.Contexts || records >= budget.Records {
				return out, after, errors.Join(failed...)
			}
			after = &p.at
			if seen[p.SiteID+p.ContextID] {
				continue
			}
			seen[p.SiteID+p.ContextID] = true
			attempts++
			e, n, err := s.evaluate(ctx, tenantID, p.SiteID, p.ContextID, actor)
			records += n
			if err != nil {
				// Nothing is recorded for this context, so it is retried on a later pass; the
				// other contexts are still evaluated, so one failing context cannot hold them back.
				failed = append(failed, err)
				continue
			}
			out = append(out, e)
		}
		if len(pending) < budget.Contexts {
			if !wrapped && started != nil {
				// The end of the queue: go on from the oldest, skipping what this pass attempted.
				wrapped, after = true, nil
				continue
			}
			return out, nil, errors.Join(failed...)
		}
	}
}

type pendingContext struct {
	Evaluation
	at EvaluationCursor
}

// pendingContexts reads up to limit unevaluated context.created rows after the cursor, oldest
// first, each with its own cursor.
func (s *Service) pendingContexts(ctx context.Context, tenantID string, limit int, after *EvaluationCursor) ([]pendingContext, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, tenantID); err != nil {
		return nil, err
	}
	var afterAt any
	afterID := "00000000-0000-0000-0000-000000000000"
	if after != nil {
		afterAt, afterID = after.createdAt, after.id
	}
	rows, err := tx.QueryContext(ctx, `SELECT o.site_id,o.context_id,o.created_at,o.id FROM supervisor.plant_context_outbox o
 WHERE o.tenant_id=$1 AND o.event_type='context.created'
  AND ($3::timestamptz IS NULL OR (o.created_at,o.id)>($3::timestamptz,$4::uuid))
  AND NOT EXISTS(SELECT 1 FROM supervisor.case_context_evaluations e WHERE (e.tenant_id,e.site_id,e.context_id)=(o.tenant_id,o.site_id,o.context_id))
 ORDER BY o.created_at,o.id LIMIT $2`, tenantID, limit, afterAt, afterID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]pendingContext, 0)
	for rows.Next() {
		var p pendingContext
		if err = rows.Scan(&p.SiteID, &p.ContextID, &p.at.createdAt, &p.at.id); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

// evaluate evaluates one context and also returns how many delta records it selected, which
// count toward the pass budget.
func (s *Service) evaluate(ctx context.Context, tenantID, siteID, contextID string, actor ServiceActor) (Evaluation, int, error) {
	out := Evaluation{SiteID: siteID, ContextID: contextID, Outcome: OutcomeNotQualified}
	sc := Scope{TenantID: tenantID, SiteID: siteID}
	refs, ready, err := s.contextDeltas(ctx, sc, contextID)
	if err != nil {
		return out, 0, err
	}
	if ready {
		outcome, caseID, err := s.triggerAll(ctx, sc, contextID, refs, actor)
		if err != nil {
			return out, len(refs), err
		}
		out.Outcome, out.CaseID = outcome, caseID
	}
	return out, len(refs), s.recordEvaluation(ctx, sc, out, actor)
}

// triggerAll raises one trigger per delta record of a READY context. Outcomes rank opened,
// joined, already_triggered; a refusal of the context itself (not qualified, or not a line)
// stops at the first record, since every record of the context would be refused alike.
func (s *Service) triggerAll(ctx context.Context, sc Scope, contextID string, refs []plantcontext.RecordRef, actor ServiceActor) (string, string, error) {
	if len(refs) == 0 {
		return OutcomeNotApplicable, "", nil
	}
	return s.triggerContext(ctx, sc, actor, contextID, refs)
}

// contextDeltas reads the delta records a context selected and whether it is READY. Eligibility
// is left to Trigger, which judges it now.
func (s *Service) contextDeltas(ctx context.Context, sc Scope, contextID string) ([]plantcontext.RecordRef, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	now, err := scopeTx(ctx, tx, sc, false)
	if err != nil {
		return nil, false, err
	}
	view, err := plantcontext.LoadTx(ctx, tx, plantcontext.Scope{TenantID: sc.TenantID, SiteID: sc.SiteID}, contextID, now)
	if err != nil {
		return nil, false, err
	}
	return view.DeltaRefs(), view.Readiness == plantcontext.ReadyState, tx.Commit()
}

func (s *Service) recordEvaluation(ctx context.Context, sc Scope, e Evaluation, actor ServiceActor) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, sc.TenantID); err != nil {
		return err
	}
	var caseID any
	if e.CaseID != "" {
		caseID = e.CaseID
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.case_context_evaluations(tenant_id,site_id,context_id,outcome,case_id,actor_issuer,actor_subject) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT DO NOTHING`,
		sc.TenantID, sc.SiteID, e.ContextID, e.Outcome, caseID, actor.Issuer, actor.Subject); err != nil {
		return err
	}
	return tx.Commit()
}
