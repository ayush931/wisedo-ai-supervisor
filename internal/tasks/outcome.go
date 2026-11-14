package tasks

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/outcome"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/plantcontext"
)

// ObserverActor records outcome observations. It measures; it never closes a case.
var ObserverActor = ActorRef{Issuer: "supervisor:observer", Subject: "outcome-observer"}

// OutcomeV1 is OutcomeObservationV1 for the task's current completion. Status is "pending" until
// it is decided; a decided observation is immutable and a case closure reason never replaces it.
//
// Revision rules (WIS-277 review m3, m4; recorded on the issue for product confirmation):
//   - Late or corrected data after the decision does not revise it. The way to revise is a
//     supervisor reopen: the next completion is a new epoch, observed afresh; the earlier epoch's
//     observation is retained.
//   - The decision is taken when outcome.Needed comparable windows are counted, or when the
//     horizon (outcome.Horizon after completion) plus the evaluation delay has passed; a source
//     still behind its watermark then counts as missing data.
//   - Comparability is the case's approved plan revision, not only its rate: a window under any
//     other approved plan revision is not_comparable (fails closed).
type OutcomeV1 struct {
	Epoch int64 `json:"epoch"`
	// CaseID and BaselineContextID name what the outcome is compared against; null while pending.
	CaseID            *string           `json:"case_id"`
	BaselineContextID *string           `json:"baseline_context_id"`
	Status            outcome.Status    `json:"status"`
	RuleVersion       string            `json:"rule_version"`
	CompletedAt       time.Time         `json:"completed_at"`
	Windows           []OutcomeWindowV1 `json:"windows"`
	Interpretation    string            `json:"interpretation"`
	ObservedAt        *time.Time        `json:"observed_at"`
}

// OutcomeWindowV1 is one post-completion window as it was assessed, with exact values.
type OutcomeWindowV1 struct {
	Start          string   `json:"start"`
	End            string   `json:"end"`
	Readiness      string   `json:"readiness"`
	ReasonCodes    []string `json:"reason_codes"`
	Expected       *string  `json:"expected_good_units_exact"`
	Actual         *string  `json:"actual_good_units_exact"`
	Deficit        *string  `json:"deficit_good_units_exact"`
	Unit           string   `json:"unit"`
	PlanRevisionID *string  `json:"plan_revision_id"`
	ContextDigest  *string  `json:"context_digest"` // null when the window could not be assessed
	RecordIDs      []string `json:"record_ids"`
}

func outcomeWindow(s plantcontext.Snapshot) OutcomeWindowV1 {
	w := OutcomeWindowV1{Start: s.Window.Start, End: s.Window.End, Readiness: s.Readiness, ReasonCodes: s.ReasonCodes, Expected: s.ExpectedExact,
		Actual: s.ActualExact, Deficit: s.DeficitExact, Unit: s.Unit, RecordIDs: []string{}}
	if s.Digest != "" {
		d := s.Digest
		w.ContextDigest = &d
	}
	if w.ReasonCodes == nil {
		w.ReasonCodes = []string{}
	}
	if s.ApprovedPlanRevision != nil {
		id := s.ApprovedPlanRevision.PlanRevisionID
		w.PlanRevisionID = &id
	}
	for _, r := range s.SourceRecordRefs {
		w.RecordIDs = append(w.RecordIDs, r.RecordID)
	}
	return w
}

// Observation is the result of observing one task's current completion.
type Observation struct {
	SiteID, TaskID, CaseID string
	Epoch                  int64
	Status                 outcome.Status
}

// ObserveOutcomes decides the outcome of the current completion of each completed task of the
// tenant that has none yet, up to limit tasks, each in its own transaction. Post windows are
// assessed (never persisted as contexts) under the case's bound context: same order, asset and
// process snapshot. A pending result writes nothing and is tried again on the next tick.
func (s *Service) ObserveOutcomes(ctx context.Context, tenantID string, limit int) ([]Observation, error) {
	if !validID(tenantID) || limit < 1 {
		return nil, ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true),set_config('statement_timeout','10s',true)`, tenantID); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT t.site_id,t.id,t.case_id,c.epoch FROM supervisor.tasks t
 JOIN supervisor.task_completions c ON (c.tenant_id,c.site_id,c.task_id)=(t.tenant_id,t.site_id,t.id)
 WHERE t.tenant_id=$1 AND t.state='completed'
 AND c.epoch=(SELECT max(epoch) FROM supervisor.task_completions m WHERE (m.tenant_id,m.site_id,m.task_id)=(t.tenant_id,t.site_id,t.id))
 AND NOT EXISTS(SELECT 1 FROM supervisor.outcome_observations o WHERE (o.tenant_id,o.site_id,o.task_id,o.epoch)=(c.tenant_id,c.site_id,c.task_id,c.epoch))
 ORDER BY c.created_at,t.id LIMIT $2`, tenantID, limit)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	var todo []Observation
	for rows.Next() {
		var o Observation
		if err = rows.Scan(&o.SiteID, &o.TaskID, &o.CaseID, &o.Epoch); err != nil {
			rows.Close()
			_ = tx.Rollback()
			return nil, err
		}
		todo = append(todo, o)
	}
	rows.Close()
	err = rows.Err()
	_ = tx.Rollback()
	if err != nil {
		return nil, err
	}
	out := make([]Observation, 0, len(todo))
	var failed []error
	for _, o := range todo {
		status, err := s.observe(ctx, Scope{TenantID: tenantID, SiteID: o.SiteID}, o)
		if err != nil {
			failed = append(failed, err)
			continue
		}
		o.Status = status
		out = append(out, o)
	}
	return out, errors.Join(failed...)
}

func (s *Service) observe(ctx context.Context, sc Scope, o Observation) (outcome.Status, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var now time.Time
	if err = tx.QueryRowContext(ctx, `SELECT clock_timestamp() FROM (SELECT set_config('supervisor.tenant_id',$1,true),set_config('statement_timeout','20s',true),set_config('lock_timeout','5s',true)) c`, sc.TenantID).Scan(&now); err != nil {
		return "", err
	}
	// Recheck under the task lock: still completed at this epoch and not yet observed.
	var state State
	var latest int64
	var completedAt time.Time
	var contextID string
	err = tx.QueryRowContext(ctx, `SELECT t.state,(SELECT max(epoch) FROM supervisor.task_completions m WHERE (m.tenant_id,m.site_id,m.task_id)=(t.tenant_id,t.site_id,t.id)),c.created_at,cs.context_snapshot_id
 FROM supervisor.tasks t JOIN supervisor.task_completions c ON (c.tenant_id,c.site_id,c.task_id,c.epoch)=(t.tenant_id,t.site_id,t.id,$4)
 JOIN supervisor.cases cs ON (cs.tenant_id,cs.site_id,cs.id)=(t.tenant_id,t.site_id,t.case_id)
 WHERE t.tenant_id=$1 AND t.site_id=$2 AND t.id=$3 FOR SHARE OF t`, sc.TenantID, sc.SiteID, o.TaskID, o.Epoch).Scan(&state, &latest, &completedAt, &contextID)
	if errors.Is(err, sql.ErrNoRows) {
		return outcome.Pending, tx.Commit()
	}
	if err != nil {
		return "", err
	}
	if state != Completed || latest != o.Epoch {
		return outcome.Pending, tx.Commit() // reopened since: the next completion is a new epoch
	}
	pc := plantcontext.Scope{TenantID: sc.TenantID, SiteID: sc.SiteID}
	base, err := plantcontext.LoadTx(ctx, tx, pc, contextID, now)
	var result outcome.Result
	switch reason := plantcontext.AssessmentReason(err); {
	case err == nil:
		if result, err = observe(ctx, tx, pc, base, completedAt, now); err != nil {
			return "", err
		}
	case reason == "":
		return "", err
	case now.Before(completedAt.Add(outcome.Horizon).Add(plantcontext.EvaluationDelay)):
		return outcome.Pending, tx.Commit()
	default:
		// The case's bound context can never be loaded (its stored row fails its integrity check):
		// once the horizon has closed the outcome is recorded as insufficient data with the reason,
		// so the task neither stays pending forever nor takes the first slots of every observer
		// batch, and the failure stays visible on the task (WIS-277 review N1).
		result = outcome.Result{Status: outcome.InsufficientData,
			Interpretation: "The case's baseline context could not be loaded (BASELINE_" + reason + "); no recovery is inferred."}
	}
	if result.Status == outcome.Pending {
		return outcome.Pending, tx.Commit()
	}
	windows := make([]OutcomeWindowV1, 0, len(result.Windows))
	for _, w := range result.Windows {
		windows = append(windows, outcomeWindow(w))
	}
	body, err := json.Marshal(windows)
	if err != nil {
		return "", err
	}
	// The primary key makes a concurrent observer's identical decision a no-op.
	if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.outcome_observations(tenant_id,site_id,task_id,epoch,case_id,baseline_context_id,status,rule_version,completed_at,windows,interpretation,observer_issuer,observer_subject)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10::jsonb,$11,$12,$13) ON CONFLICT DO NOTHING`,
		sc.TenantID, sc.SiteID, o.TaskID, o.Epoch, o.CaseID, contextID, string(result.Status), outcome.RuleVersion, completedAt, string(body), result.Interpretation, ObserverActor.Issuer, ObserverActor.Subject); err != nil {
		return "", err
	}
	return result.Status, tx.Commit()
}

// observe assesses the post-completion windows that have ended against the case's baseline and
// applies the outcome rule.
func observe(ctx context.Context, tx *sql.Tx, pc plantcontext.Scope, base plantcontext.View, completedAt, now time.Time) (outcome.Result, error) {
	var assessed []plantcontext.Snapshot
	for _, w := range outcome.Candidates(completedAt) {
		end, err := time.Parse(time.RFC3339, w.End)
		if err != nil || end.After(now) {
			break // later windows have not ended yet
		}
		snap, err := plantcontext.AssessTx(ctx, tx, pc, plantcontext.Request{OrderID: base.OrderID, AssetID: base.AssetID, WindowStart: w.Start, WindowEnd: w.End, ProcessSnapshotID: base.ProcessSnapshotID}, now)
		if err != nil {
			// An error that recurs for this window can never become data: the window is
			// NOT_READY with its reason, so the horizon ends in insufficient_data with the reason
			// recorded instead of retrying forever and blocking the case (WIS-277 review M3).
			reason := plantcontext.AssessmentReason(err)
			if reason == "" {
				return outcome.Result{}, err
			}
			snap = plantcontext.Snapshot{OrderID: base.OrderID, AssetID: base.AssetID, Window: plantcontext.Window{Start: w.Start, End: w.End},
				Readiness: plantcontext.NotReady, ReasonCodes: []string{reason}}
		}
		assessed = append(assessed, snap)
	}
	return outcome.Decide(base.Snapshot, assessed, completedAt, now), nil
}

// loadOutcome returns the outcome of the task's current completion: the stored observation, or
// pending when the task is completed and none is stored yet; nil when there is no completion to
// observe (never completed, reopened or canceled).
func loadOutcome(ctx context.Context, tx *sql.Tx, sc Scope, t TaskV1) (*OutcomeV1, error) {
	if t.State != Completed || len(t.Completions) == 0 {
		return nil, nil
	}
	current := t.Completions[len(t.Completions)-1]
	out := &OutcomeV1{Epoch: current.Epoch, Status: outcome.Pending, CompletedAt: current.CreatedAt, Windows: []OutcomeWindowV1{}}
	var windows []byte
	var observed time.Time
	var caseID, baseline string
	err := tx.QueryRowContext(ctx, `SELECT status,rule_version,windows,interpretation,observed_at,case_id::text,baseline_context_id::text FROM supervisor.outcome_observations WHERE tenant_id=$1 AND site_id=$2 AND task_id=$3 AND epoch=$4`,
		sc.TenantID, sc.SiteID, t.ID, current.Epoch).Scan(&out.Status, &out.RuleVersion, &windows, &out.Interpretation, &observed, &caseID, &baseline)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(windows, &out.Windows); err != nil {
		return nil, err
	}
	observed = observed.UTC()
	out.ObservedAt, out.CaseID, out.BaselineContextID = &observed, &caseID, &baseline
	return out, nil
}
