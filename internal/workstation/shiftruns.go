package workstation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/session"
)

// Shift runs: an owner or supervisor starts an approved shift plan and later ends it. Only
// started runs accept station records. Ending writes, in one transaction, the final summary
// snapshot and a next-shift handover draft and cites both on the run. Neither start nor end
// approves anything (rule 05); the handover stays a draft.
var (
	ErrShiftNotStarted        = errors.New("shift run has not been started")
	ErrShiftRunAlreadyStarted = errors.New("shift run is already started")
	ErrShiftRunEnded          = errors.New("shift run has ended")
	ErrShiftWindowEnded       = errors.New("shift plan window has ended")
	ErrShiftStartTooEarly     = errors.New("shift run starts more than 30 minutes before the planned start")
)

const (
	CodeInvalidShiftRun = "INVALID_SHIFT_RUN"
	// EarlyStartWindow is how long before the planned start a run may start.
	EarlyStartWindow = 30 * time.Minute
)

type ShiftRunState string

const (
	RunPlanned ShiftRunState = "planned"
	RunStarted ShiftRunState = "started"
	RunEnded   ShiftRunState = "ended"
)

// ShiftRunStartInputV1 names the approved plan revision the supervisor is starting; the
// plan's state revision travels in If-Match.
type ShiftRunStartInputV1 struct {
	SchemaVersion int   `json:"schema_version"`
	PlanRevision  int64 `json:"plan_revision"`
}

// ShiftRunEndInputV1 ends the run; next_shift is the shift the handover draft proposes for.
type ShiftRunEndInputV1 struct {
	SchemaVersion int         `json:"schema_version"`
	NextShift     NextShiftV1 `json:"next_shift"`
}

type ShiftRunSummaryRefV1 struct {
	Revision               int64  `json:"revision"`
	CanonicalPayloadSHA256 string `json:"canonical_payload_digest" contract:"sha256"`
}

// ShiftRunV1 is a plan's run state. PlanStateRevision is the If-Match value for start and
// end. Summary and HandoverRevision are what End wrote; SummaryRevised is true when a late
// record has since appended a newer summary revision.
type ShiftRunV1 struct {
	SchemaVersion         int                   `json:"schema_version"`
	PlanID                string                `json:"plan_id"`
	State                 ShiftRunState         `json:"state"`
	PlanStateRevision     int64                 `json:"plan_state_revision"`
	StartedPlanRevision   *int64                `json:"started_plan_revision"`
	StartedBy             *ActorRefV1           `json:"started_by"`
	StartedAt             *time.Time            `json:"started_at"`
	EndedPlanRevision     *int64                `json:"ended_plan_revision"`
	EndedBy               *ActorRefV1           `json:"ended_by"`
	EndedAt               *time.Time            `json:"ended_at"`
	Summary               *ShiftRunSummaryRefV1 `json:"summary"`
	HandoverRevision      *int64                `json:"handover_revision"`
	LatestSummaryRevision *int64                `json:"latest_summary_revision"`
	SummaryRevised        bool                  `json:"summary_revised"`
	Replayed              bool                  `json:"replayed"`
}

type ShiftRunService struct{ plans *ShiftPlanService }

func NewShiftRuns(db *sql.DB) *ShiftRunService { return &ShiftRunService{NewShiftPlans(db)} }

// WithSessionPolicy returns a copy of the service that enforces p (WIS-290).
func (s *ShiftRunService) WithSessionPolicy(p session.Policy) *ShiftRunService {
	return &ShiftRunService{s.plans.WithSessionPolicy(p)}
}

// storedRun is a run row; started is false when the plan has no run.
type storedRun struct {
	started bool
	ended   sql.NullTime
}

func (r storedRun) endedAt() *time.Time {
	if !r.ended.Valid {
		return nil
	}
	t := r.ended.Time.UTC()
	return &t
}

// loadRun reads the plan's run in the caller's transaction. Writers lock the row.
func loadRun(ctx context.Context, tx *sql.Tx, sc Scope, planID string) (storedRun, error) {
	var r storedRun
	e := tx.QueryRowContext(ctx, `SELECT ended_at FROM supervisor.shift_runs WHERE tenant_id=$1 AND site_id=$2 AND plan_id=$3`, sc.TenantID, sc.SiteID, planID).Scan(&r.ended)
	if errors.Is(e, sql.ErrNoRows) {
		return r, nil
	}
	r.started = e == nil
	return r, e
}

func runAudit(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, planID, event string) error {
	_, e := tx.ExecContext(ctx, `INSERT INTO supervisor.shift_run_audit(tenant_id,site_id,actor_issuer,actor_subject,plan_id,event_type) VALUES($1,$2,$3,$4,$5,$6)`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, planID, event)
	return e
}

// runReadAudit records a run read at most once a minute per reader and plan: station pages poll
// the run every 10 s and audit rows are permanent, as interview reads are bounded (AuditReadSQL).
// The window uses now(), this transaction's start, so the created_at range can use the index.
func runReadAudit(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, planID string) error {
	_, e := tx.ExecContext(ctx, `INSERT INTO supervisor.shift_run_audit(tenant_id,site_id,actor_issuer,actor_subject,plan_id,event_type)
 SELECT $1,$2,$3,$4,$5,'shift_run.read' WHERE NOT EXISTS(SELECT 1 FROM supervisor.shift_run_audit WHERE tenant_id=$1 AND site_id=$2 AND plan_id=$5
 AND actor_issuer=$3 AND actor_subject=$4 AND event_type='shift_run.read' AND created_at > now()-interval '1 minute')`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, planID)
	return e
}

// runReceipt returns the original response of an earlier identical request, or ErrConflict
// when the key was used for a different one.
func runReceipt(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, key, hash string) (*ShiftRunV1, error) {
	var stored, planID string
	var response []byte
	e := tx.QueryRowContext(ctx, `SELECT request_sha256,plan_id,response FROM supervisor.shift_run_receipts WHERE tenant_id=$1 AND site_id=$2 AND actor_issuer=$3 AND actor_subject=$4 AND idempotency_key=$5`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, key).Scan(&stored, &planID, &response)
	switch {
	case errors.Is(e, sql.ErrNoRows):
		return nil, nil
	case e != nil:
		return nil, e
	case stored != hash:
		return nil, ErrConflict
	}
	var out ShiftRunV1
	if json.Unmarshal(response, &out) != nil || out.PlanID != planID {
		return nil, errIntegrity
	}
	out.Replayed = true
	return &out, runAudit(ctx, tx, a, sc, planID, "mutation.replayed")
}

// mutate runs the shared prologue of start and end: authority, replay (the original response,
// authority rechecked first), the plan lock and If-Match. It returns a nil tx after a replay
// has been answered. now is read after the site and plan locks are held, so a record that
// committed while this request waited for them was received before now.
func (s *ShiftRunService) mutate(ctx context.Context, a Actor, sc Scope, planID, key, hash string, expected int64) (*sql.Tx, lockedHead, time.Time, time.Time, *ShiftRunV1, error) {
	var h lockedHead
	tx, auth, now, err := s.plans.begin(ctx, a, sc, shiftRun)
	if err != nil {
		return nil, h, auth, now, nil, err
	}
	fail := func(err error) (*sql.Tx, lockedHead, time.Time, time.Time, *ShiftRunV1, error) {
		_ = tx.Rollback()
		return nil, h, auth, now, nil, err
	}
	replay, err := runReceipt(ctx, tx, a, sc, key, hash)
	if err != nil {
		return fail(err)
	}
	if replay != nil {
		if err = s.plans.finish(ctx, tx, a); err != nil {
			return fail(err)
		}
		return nil, h, auth, now, replay, nil
	}
	if h, err = lockHead(ctx, tx, a, sc, planID); err != nil {
		return fail(err)
	}
	if h.state != expected {
		return fail(ErrRevision)
	}
	if err = tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return fail(err)
	}
	return tx, h, auth, now, nil, nil
}

// commit stores the response with the receipt, so a replay returns exactly this response
// even after the run has moved on.
func (s *ShiftRunService) commit(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, key, hash, op, planID, event string) (ShiftRunV1, error) {
	if e := runAudit(ctx, tx, a, sc, planID, event); e != nil {
		return ShiftRunV1{}, e
	}
	out, err := readRun(ctx, tx, sc, planID)
	if err != nil {
		return out, err
	}
	response, err := json.Marshal(out)
	if err != nil {
		return out, err
	}
	if _, e := tx.ExecContext(ctx, `INSERT INTO supervisor.shift_run_receipts(tenant_id,site_id,actor_issuer,actor_subject,idempotency_key,request_sha256,operation,plan_id,response) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9::jsonb)`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, key, hash, op, planID, string(response)); e != nil {
		return ShiftRunV1{}, e
	}
	return out, s.plans.finish(ctx, tx, a)
}

// Start begins the run of the plan's approved revision in.PlanRevision at the server clock.
// It may start at most EarlyStartWindow before the planned start and never at or after the
// planned end.
func (s *ShiftRunService) Start(ctx context.Context, a Actor, sc Scope, planID string, in ShiftRunStartInputV1, key string, expected int64) (ShiftRunV1, error) {
	switch {
	case !canonicalID(planID):
		return ShiftRunV1{}, ErrNotFound
	case in.SchemaVersion != 1:
		return ShiftRunV1{}, &ConfigError{CodeInvalidShiftRun, "schema_version"}
	case in.PlanRevision < 1:
		return ShiftRunV1{}, &ConfigError{CodeInvalidShiftRun, "plan_revision"}
	case !validText(key, 128) || expected < 1:
		return ShiftRunV1{}, &ConfigError{CodeInvalidShiftRun, ""}
	}
	hash := requestDigest([]any{"shift_run.start", planID, expected, in})
	tx, h, auth, now, replay, err := s.mutate(ctx, a, sc, planID, key, hash, expected)
	if err != nil || replay != nil {
		if err != nil {
			return ShiftRunV1{}, err
		}
		return *replay, nil
	}
	defer tx.Rollback()
	run, err := loadRun(ctx, tx, sc, planID)
	switch {
	case err != nil:
		return ShiftRunV1{}, err
	case run.ended.Valid:
		return ShiftRunV1{}, ErrShiftRunEnded
	case run.started:
		return ShiftRunV1{}, ErrShiftRunAlreadyStarted
	case !h.approved.Valid || h.approved.Int64 != in.PlanRevision:
		return ShiftRunV1{}, ErrPlanNotApproved
	case !now.Before(h.end):
		return ShiftRunV1{}, ErrShiftWindowEnded
	case now.Before(h.start.Add(-EarlyStartWindow)):
		return ShiftRunV1{}, ErrShiftStartTooEarly
	}
	if _, e := tx.ExecContext(ctx, `INSERT INTO supervisor.shift_runs(tenant_id,site_id,plan_id,started_plan_revision,started_by_issuer,started_by_subject,start_auth_time,started_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`,
		sc.TenantID, sc.SiteID, planID, in.PlanRevision, a.Issuer, a.Subject, auth, now); e != nil {
		return ShiftRunV1{}, e
	}
	return s.commit(ctx, tx, a, sc, key, hash, "start", planID, "shift_run.started")
}

// End closes a started run at the server clock and, in the same transaction, appends the
// final summary (as ended at that instant) and a handover draft for in.NextShift from it.
// The run cites both; a late record later appends newer summary revisions, never rewrites.
func (s *ShiftRunService) End(ctx context.Context, a Actor, sc Scope, planID string, in ShiftRunEndInputV1, key string, expected int64) (ShiftRunV1, error) {
	switch {
	case !canonicalID(planID):
		return ShiftRunV1{}, ErrNotFound
	case in.SchemaVersion != 1:
		return ShiftRunV1{}, &ConfigError{CodeInvalidShiftRun, "schema_version"}
	case !validShift(in.NextShift.LineID, in.NextShift.StartsAt, in.NextShift.EndsAt, in.NextShift.Timezone):
		return ShiftRunV1{}, &ConfigError{CodeInvalidShiftRun, "next_shift"}
	case !validText(key, 128) || expected < 1:
		return ShiftRunV1{}, &ConfigError{CodeInvalidShiftRun, ""}
	}
	// The same instants in another offset spelling are the same request.
	hashed := in
	for _, v := range []*string{&hashed.NextShift.StartsAt, &hashed.NextShift.EndsAt} {
		t, _ := validTime(*v)
		*v = t.UTC().Format(time.RFC3339Nano)
	}
	hash := requestDigest([]any{"shift_run.end", planID, expected, hashed})
	tx, _, auth, now, replay, err := s.mutate(ctx, a, sc, planID, key, hash, expected)
	if err != nil || replay != nil {
		if err != nil {
			return ShiftRunV1{}, err
		}
		return *replay, nil
	}
	defer tx.Rollback()
	run, err := loadRun(ctx, tx, sc, planID)
	switch {
	case err != nil:
		return ShiftRunV1{}, err
	case run.ended.Valid:
		return ShiftRunV1{}, ErrShiftRunEnded
	case !run.started:
		return ShiftRunV1{}, ErrShiftNotStarted
	}
	plan, err := loadShiftPlan(ctx, tx, sc, planID)
	if err != nil {
		return ShiftRunV1{}, err
	}
	summary, err := appendSummary(ctx, tx, a, sc, plan, now, &now)
	if err != nil {
		return ShiftRunV1{}, err
	}
	handover, err := appendHandover(ctx, tx, a, sc, plan, summary, in.NextShift)
	if err != nil {
		return ShiftRunV1{}, err
	}
	if _, e := tx.ExecContext(ctx, `UPDATE supervisor.shift_runs r SET ended_plan_revision=$4,ended_by_issuer=$5,ended_by_subject=$6,end_auth_time=$7,ended_at=$8,summary_revision=$9,
 summary_sha256=(SELECT canonical_payload_sha256 FROM supervisor.shift_summaries WHERE tenant_id=$1 AND site_id=$2 AND plan_id=$3 AND revision=$9),handover_revision=$10
 WHERE tenant_id=$1 AND site_id=$2 AND plan_id=$3`, sc.TenantID, sc.SiteID, planID, *plan.ApprovedRevision, a.Issuer, a.Subject, auth, now, summary, handover); e != nil {
		return ShiftRunV1{}, e
	}
	if e := summaryAudit(ctx, tx, a, sc, planID, summary, "shift_summary.generated"); e != nil {
		return ShiftRunV1{}, e
	}
	if e := handoverAudit(ctx, tx, a, sc, planID, handover, "", "shift_handover.generated"); e != nil {
		return ShiftRunV1{}, e
	}
	return s.commit(ctx, tx, a, sc, key, hash, "end", planID, "shift_run.ended")
}

// Get returns the plan's run state to any active member; a plan with no run is planned.
func (s *ShiftRunService) Get(ctx context.Context, a Actor, sc Scope, planID string) (ShiftRunV1, error) {
	if !canonicalID(planID) {
		return ShiftRunV1{}, ErrNotFound
	}
	tx, _, _, err := s.plans.begin(ctx, a, sc, shiftRead)
	if err != nil {
		return ShiftRunV1{}, err
	}
	defer tx.Rollback()
	out, err := readRun(ctx, tx, sc, planID)
	if err != nil {
		return out, err
	}
	if err = runReadAudit(ctx, tx, a, sc, planID); err != nil {
		return out, err
	}
	return out, s.plans.finish(ctx, tx, a)
}

func readRun(ctx context.Context, tx *sql.Tx, sc Scope, planID string) (ShiftRunV1, error) {
	out := ShiftRunV1{SchemaVersion: 1, PlanID: planID, State: RunPlanned}
	var latest int64
	e := tx.QueryRowContext(ctx, `SELECT state_revision,(SELECT coalesce(max(revision),0) FROM supervisor.shift_summaries s WHERE (s.tenant_id,s.site_id,s.plan_id)=(h.tenant_id,h.site_id,h.id)) FROM supervisor.shift_plan_heads h WHERE tenant_id=$1 AND site_id=$2 AND id=$3`,
		sc.TenantID, sc.SiteID, planID).Scan(&out.PlanStateRevision, &latest)
	if errors.Is(e, sql.ErrNoRows) {
		return out, ErrNotFound
	}
	if e != nil {
		return out, e
	}
	var startedRev, endedRev, summaryRev, handoverRev sql.NullInt64
	var sIssuer, sSubject, eIssuer, eSubject, digest sql.NullString
	var startedAt, endedAt sql.NullTime
	e = tx.QueryRowContext(ctx, `SELECT started_plan_revision,started_by_issuer,started_by_subject,started_at,ended_plan_revision,ended_by_issuer,ended_by_subject,ended_at,summary_revision,summary_sha256,handover_revision
 FROM supervisor.shift_runs WHERE tenant_id=$1 AND site_id=$2 AND plan_id=$3`, sc.TenantID, sc.SiteID, planID).Scan(&startedRev, &sIssuer, &sSubject, &startedAt, &endedRev, &eIssuer, &eSubject, &endedAt, &summaryRev, &digest, &handoverRev)
	if errors.Is(e, sql.ErrNoRows) {
		return out, nil
	}
	if e != nil {
		return out, e
	}
	at := func(t sql.NullTime) *time.Time {
		if !t.Valid {
			return nil
		}
		v := t.Time.UTC()
		return &v
	}
	out.State, out.StartedPlanRevision, out.StartedBy, out.StartedAt = RunStarted, nullable(startedRev), &ActorRefV1{sIssuer.String, sSubject.String}, at(startedAt)
	if endedAt.Valid {
		out.State, out.EndedPlanRevision, out.EndedBy, out.EndedAt = RunEnded, nullable(endedRev), &ActorRefV1{eIssuer.String, eSubject.String}, at(endedAt)
		out.Summary, out.HandoverRevision = &ShiftRunSummaryRefV1{summaryRev.Int64, digest.String}, nullable(handoverRev)
		out.LatestSummaryRevision, out.SummaryRevised = &latest, latest > summaryRev.Int64
	}
	return out, nil
}
