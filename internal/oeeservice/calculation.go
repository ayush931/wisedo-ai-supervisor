package oeeservice

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/oee"
	"github.com/google/uuid"
)

// reapBatch bounds how many expired runs one request reaps, so reaping never turns a single
// request into unbounded work. Remaining runs are reaped by later requests.
const reapBatch = 100

// liveRunPredicate selects runs that still hold capacity: accepted and before their deadline,
// or running under an unexpired lease. Expired runs never count, whether or not they have been
// reaped yet.
const liveRunPredicate = `((status='ACCEPTED' AND deadline_at>clock_timestamp()) OR (status='RUNNING' AND execution_lease_until>clock_timestamp() AND deadline_at>clock_timestamp()))`

// Submit is the entry point for a calculation request. It accepts the run (Transaction A) and
// executes it in the background, bounded by the run's own deadline rather than the request's.
//
// A new run needs one unit of this process's execution budget, reserved inside Transaction A
// just before the run is inserted: overload is rejected with ErrOverloaded and nothing is
// stored, while replays, invalid and unauthorized requests never consume or wait for a slot
// (a replay always answers with the original result, rule 07).
//
// A replay of a run that is still ACCEPTED is dispatched again with the replayed dataset (whose
// digest the run already fixes) when a slot is free. The claim is fenced, so at most one
// executor ever runs it; this lets a client retry recover a run whose executor was lost.
func (s *Service) Submit(ctx context.Context, a Actor, sc Scope, req oee.OeeCalculationRequestV1, key, correlation string) (oee.OeeCalculationRunV1, error) {
	var release func()
	admit := func() bool {
		r, ok := s.reserveSlot()
		if !ok {
			slog.Warn("oee execution budget exhausted", "max_concurrent_executions", s.cfg.MaxConcurrentExecutions, "correlation_id", correlation)
			return false
		}
		release = r
		return true
	}
	run, err := s.accept(ctx, a, sc, req, key, correlation, admit)
	if err != nil || run.Status != oee.RunAccepted {
		if release != nil {
			release()
		}
		return run, err
	}
	if release == nil {
		// A replayed ACCEPTED run: dispatch only if a slot is free; otherwise a later retry or
		// the reaper decides its outcome.
		r, ok := s.reserveSlot()
		if !ok {
			return run, nil
		}
		release = r
	}
	go func() {
		defer release()
		if err := s.Execute(s.executions, sc, run.RunID, req.Dataset, correlation); err != nil {
			slog.Warn("oee run execution failed", "run_id", run.RunID, "correlation_id", correlation, "error_kind", errorKind(err))
		}
	}()
	return run, nil
}

// errorKind classifies an execution error into a bounded log label; error text is never logged.
func errorKind(err error) string {
	switch {
	case errors.Is(err, ErrNotFound):
		return "not_found"
	case errors.Is(err, ErrInvalid):
		return "invalid"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return "deadline"
	default:
		return "storage"
	}
}

// Accept is Transaction A (ADR 0002 Sec. 7.1): verify the whole dataset before any side
// effect, reserve the idempotency key, reap expired runs, enforce the tenant's open-run budget,
// select exactly one published effective policy under a pointer lock, check the expected
// revision and insert the run as ACCEPTED with its digests and absolute deadline. Calculation
// happens after commit, in Execute. A replay returns the run's current state.
//
// Accept itself never executes the run; Submit is the entry point that also does.
func (s *Service) Accept(ctx context.Context, a Actor, sc Scope, req oee.OeeCalculationRequestV1, key, correlation string) (oee.OeeCalculationRunV1, error) {
	return s.accept(ctx, a, sc, req, key, correlation, nil)
}

// accept implements Accept. admit, when set, is asked for permission immediately before a new
// run is inserted; refusing aborts the transaction with ErrOverloaded.
func (s *Service) accept(ctx context.Context, a Actor, sc Scope, req oee.OeeCalculationRequestV1, key, correlation string, admit func() bool) (oee.OeeCalculationRunV1, error) {
	var out oee.OeeCalculationRunV1
	if req.ExpectedPolicyRevision < 0 {
		return out, ErrInvalid
	}
	if err := oee.VerifyDataset(req.Dataset); err != nil {
		if errors.Is(err, oee.ErrTimelineLimit) {
			return out, ErrLimit
		}
		return out, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	// VerifyDataset has parsed both window bounds; these cannot fail.
	windowStart, _ := time.Parse(time.RFC3339Nano, req.Dataset.WindowStart)
	windowEnd, _ := time.Parse(time.RFC3339Nano, req.Dataset.WindowEnd)
	hash, err := requestDigest(sc, "OeeCalculationRequestV1", req)
	if err != nil {
		return out, ErrInvalid
	}
	tx, err := s.begin(ctx, a, sc, calculate)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	// The open-run budget is per tenant; this lock serializes its check across sites and
	// replicas for the rest of the transaction.
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('supervisor.oee_capacity:'||$1,0))`, sc.TenantID); err != nil {
		return out, err
	}
	if ref, ok, err := s.claimKey(ctx, tx, a, sc, "calculation.create", key, hash); err != nil {
		return out, err
	} else if ok {
		if !validID(ref.RunID) {
			return out, errIntegrity
		}
		if err = reap(ctx, tx, sc, ref.RunID, correlation); err != nil {
			return out, err
		}
		if err = audit(ctx, tx, sc, auditEntry{a.Issuer, a.Subject, "mutation.replayed", "calculation", ref.RunID, "", "", "", correlation}); err != nil {
			return out, err
		}
		if out, err = loadRun(ctx, tx, sc, ref.RunID); err != nil {
			return out, err
		}
		out.Replayed = true
		return out, s.finish(ctx, tx, a)
	}
	if err = reap(ctx, tx, sc, "", correlation); err != nil {
		return out, err
	}
	var open int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.oee_calculation_run WHERE tenant_id=$1 AND `+liveRunPredicate, sc.TenantID).Scan(&open); err != nil {
		return out, err
	}
	if open >= s.cfg.MaxOpenRuns {
		slog.Warn("oee capacity exhausted", "open_runs", open, "max_open_runs", s.cfg.MaxOpenRuns, "correlation_id", correlation)
		return out, ErrOverloaded
	}
	ref, err := selectPolicy(ctx, tx, sc, windowStart, windowEnd)
	if err != nil {
		return out, err
	}
	if req.ExpectedPolicyRevision != 0 && req.ExpectedPolicyRevision != ref.Revision {
		return out, ErrRevision
	}
	if admit != nil && !admit() {
		return out, ErrOverloaded
	}
	runID := uuid.NewString()
	if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.oee_calculation_run(tenant_id,site_id,run_id,dataset_id,asset_id,window_start,window_end,dataset_sha256,policy_id,policy_revision_id,policy_sha256,requested_by_issuer,requested_by_subject,request_sha256,status,deadline_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,'ACCEPTED',clock_timestamp()+make_interval(secs=>$15))`,
		sc.TenantID, sc.SiteID, runID, req.Dataset.DatasetID, req.Dataset.AssetID, windowStart.UTC(), windowEnd.UTC(), req.Dataset.ProvenanceDigest,
		ref.PolicyID, ref.PolicyRevisionID, ref.Digest, a.Issuer, a.Subject, hash, s.cfg.RunDeadline.Seconds()); err != nil {
		return out, err
	}
	if err = audit(ctx, tx, sc, auditEntry{a.Issuer, a.Subject, "calculation.accepted", "calculation", runID, "", req.Dataset.ProvenanceDigest, "", correlation}); err != nil {
		return out, err
	}
	if err = completeKey(ctx, tx, a, sc, "calculation.create", key, 202, keyResponse{RunID: runID}); err != nil {
		return out, err
	}
	if out, err = loadRun(ctx, tx, sc, runID); err != nil {
		return out, err
	}
	return out, s.finish(ctx, tx, a)
}

// selectPolicy finds exactly one published policy whose effective revision covers the whole
// window and share-locks its pointer (ADR 0002 Sec. 4.3, 7.1 step 4). None or several is
// ErrPolicyNotReady; the caller can never choose a draft or historical revision.
func selectPolicy(ctx context.Context, tx *sql.Tx, sc Scope, start, end time.Time) (oee.OeePolicyRefV1, error) {
	rows, err := tx.QueryContext(ctx, `SELECT p.policy_id,r.policy_revision_id,r.revision_no,r.policy_json,r.policy_sha256 FROM supervisor.oee_policy p
 JOIN supervisor.oee_policy_publication pub ON (pub.tenant_id,pub.site_id,pub.policy_id,pub.publication_id)=(p.tenant_id,p.site_id,p.policy_id,p.current_publication_id)
 JOIN supervisor.oee_policy_revision r ON (r.tenant_id,r.site_id,r.policy_id,r.policy_revision_id)=(pub.tenant_id,pub.site_id,pub.policy_id,pub.policy_revision_id)
 WHERE p.tenant_id=$1 AND p.site_id=$2 AND pub.action IN ('PUBLISH','ROLLBACK')
   AND r.effective_start<=$3 AND (r.effective_end IS NULL OR r.effective_end>=$4)
 FOR SHARE OF p`, sc.TenantID, sc.SiteID, start.UTC(), end.UTC())
	if err != nil {
		return oee.OeePolicyRefV1{}, err
	}
	defer rows.Close()
	var found []oee.OeePolicyRefV1
	for rows.Next() {
		var ref oee.OeePolicyRefV1
		var body []byte
		if err = rows.Scan(&ref.PolicyID, &ref.PolicyRevisionID, &ref.Revision, &body, &ref.Digest); err != nil {
			return oee.OeePolicyRefV1{}, err
		}
		if _, err = verifiedPolicy(sc, body, ref.Digest); err != nil {
			return oee.OeePolicyRefV1{}, err
		}
		found = append(found, ref)
	}
	if err = rows.Err(); err != nil {
		return oee.OeePolicyRefV1{}, err
	}
	if len(found) != 1 {
		return oee.OeePolicyRefV1{}, ErrPolicyNotReady
	}
	return found[0], nil
}

// reap moves expired runs at one site to their terminal state and audits each one (ADR 0002
// Sec. 18): a run with a cancel request becomes CANCELLED; otherwise an accepted run past its
// deadline, or a running run whose lease or deadline passed, becomes FAILED with
// DEADLINE_EXCEEDED or LEASE_EXPIRED. With runID set it reaps only that run.
//
// It is site-scoped (the caller holds that site's lock) and skips rows another transaction has
// locked, so it never waits on an executor that is sealing. The audit actor is the run's
// requester, on whose behalf the run was held; the reason code names the cause.
func reap(ctx context.Context, tx *sql.Tx, sc Scope, runID, correlation string) error {
	_, err := tx.ExecContext(ctx, `WITH due AS (
  SELECT run_id FROM supervisor.oee_calculation_run
   WHERE tenant_id=$1 AND site_id=$2 AND ($3::text='' OR run_id=NULLIF($3::text,'')::uuid)
     AND ((status='ACCEPTED' AND deadline_at<=clock_timestamp())
       OR (status='RUNNING' AND (execution_lease_until<=clock_timestamp() OR deadline_at<=clock_timestamp())))
   ORDER BY deadline_at LIMIT $5
   FOR UPDATE SKIP LOCKED
), reaped AS (
  UPDATE supervisor.oee_calculation_run r SET
    status=CASE WHEN r.cancel_requested_at IS NOT NULL THEN 'CANCELLED' ELSE 'FAILED' END,
    failure_code=CASE WHEN r.cancel_requested_at IS NOT NULL THEN 'CANCELLED'
                      WHEN r.deadline_at<=clock_timestamp() THEN 'DEADLINE_EXCEEDED'
                      ELSE 'LEASE_EXPIRED' END,
    finished_at=clock_timestamp(),row_version=r.row_version+1
   FROM due WHERE r.tenant_id=$1 AND r.site_id=$2 AND r.run_id=due.run_id
   RETURNING r.run_id,r.status,r.failure_code,r.requested_by_issuer,r.requested_by_subject
)
INSERT INTO supervisor.oee_audit_entry(tenant_id,site_id,actor_issuer,actor_subject,action,resource_type,resource_id,reason_code,correlation_id)
SELECT $1,$2,requested_by_issuer,requested_by_subject,
       CASE WHEN status='CANCELLED' THEN 'calculation.cancelled' ELSE 'calculation.failed' END,
       'calculation',run_id,failure_code,$4
  FROM reaped`, sc.TenantID, sc.SiteID, runID, correlation, reapBatch)
	return err
}

func loadRun(ctx context.Context, tx *sql.Tx, sc Scope, runID string) (oee.OeeCalculationRunV1, error) {
	var out oee.OeeCalculationRunV1
	var windowStart, windowEnd, accepted, deadline time.Time
	var started, finished sql.NullTime
	var resultStatus, truth, failure, evidence sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT r.run_id,r.dataset_id,r.asset_id,r.window_start,r.window_end,r.policy_id,r.policy_revision_id,v.revision_no,r.policy_sha256,
 r.status,r.result_status,r.truth_state,r.failure_code,e.evidence_id,r.accepted_at,r.deadline_at,r.started_at,r.finished_at,r.cancel_generation,r.row_version
 FROM supervisor.oee_calculation_run r
 JOIN supervisor.oee_policy_revision v ON (v.tenant_id,v.site_id,v.policy_id,v.policy_revision_id)=(r.tenant_id,r.site_id,r.policy_id,r.policy_revision_id)
 LEFT JOIN supervisor.oee_evidence_pack e ON (e.tenant_id,e.site_id,e.run_id)=(r.tenant_id,r.site_id,r.run_id)
 WHERE r.tenant_id=$1 AND r.site_id=$2 AND r.run_id=$3`, sc.TenantID, sc.SiteID, runID).Scan(
		&out.RunID, &out.DatasetID, &out.AssetID, &windowStart, &windowEnd, &out.Policy.PolicyID, &out.Policy.PolicyRevisionID, &out.Policy.Revision, &out.Policy.Digest,
		&out.Status, &resultStatus, &truth, &failure, &evidence, &accepted, &deadline, &started, &finished, &out.CancelGeneration, &out.RowVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return out, ErrNotFound
	}
	if err != nil {
		return out, err
	}
	out.WindowStart, out.WindowEnd, out.AcceptedAt, out.DeadlineAt = wireTime(windowStart), wireTime(windowEnd), wireTime(accepted), wireTime(deadline)
	out.ResultStatus, out.TruthState, out.FailureCode, out.EvidenceID = oee.Status(resultStatus.String), truth.String, failure.String, evidence.String
	if started.Valid {
		out.StartedAt = wireTime(started.Time)
	}
	if finished.Valid {
		out.FinishedAt = wireTime(finished.Time)
	}
	return out, nil
}

// GetRun returns a run's current state. An expired run is reaped first, so a run whose executor
// was lost never reads as ACCEPTED or RUNNING past its lease or deadline.
func (s *Service) GetRun(ctx context.Context, a Actor, sc Scope, runID string, correlation string) (oee.OeeCalculationRunV1, error) {
	if !validID(runID) {
		return oee.OeeCalculationRunV1{}, ErrInvalid
	}
	tx, err := s.begin(ctx, a, sc, read)
	if err != nil {
		return oee.OeeCalculationRunV1{}, err
	}
	defer tx.Rollback()
	if err = reap(ctx, tx, sc, runID, correlation); err != nil {
		return oee.OeeCalculationRunV1{}, err
	}
	out, err := loadRun(ctx, tx, sc, runID)
	if err != nil {
		return out, err
	}
	return out, s.finish(ctx, tx, a)
}

// Cancel requests cancellation (ADR 0002 Sec. 7.4). An ACCEPTED run is cancelled at once; a
// RUNNING run has its cancel generation incremented, so its seal no longer matches and the
// executor (or, if the executor is gone, the reaper) records CANCELLED. A terminal run cannot
// be cancelled.
func (s *Service) Cancel(ctx context.Context, a Actor, sc Scope, runID, key, correlation string) (oee.OeeCalculationRunV1, error) {
	var out oee.OeeCalculationRunV1
	if !validID(runID) {
		return out, ErrInvalid
	}
	hash, err := requestDigest(sc, "OeeCalculationCancelRequestV1", struct {
		RunID string `json:"run_id"`
	}{runID})
	if err != nil {
		return out, ErrInvalid
	}
	tx, err := s.begin(ctx, a, sc, calculate)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	if _, ok, err := s.claimKey(ctx, tx, a, sc, "calculation.cancel", key, hash); err != nil {
		return out, err
	} else if ok {
		if err = audit(ctx, tx, sc, auditEntry{a.Issuer, a.Subject, "mutation.replayed", "calculation", runID, "", "", "", correlation}); err != nil {
			return out, err
		}
		if out, err = loadRun(ctx, tx, sc, runID); err != nil {
			return out, err
		}
		out.Replayed = true
		return out, s.finish(ctx, tx, a)
	}
	if err = reap(ctx, tx, sc, runID, correlation); err != nil {
		return out, err
	}
	var status string
	err = tx.QueryRowContext(ctx, `SELECT status FROM supervisor.oee_calculation_run WHERE tenant_id=$1 AND site_id=$2 AND run_id=$3 FOR UPDATE`, sc.TenantID, sc.SiteID, runID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return out, ErrNotFound
	}
	if err != nil {
		return out, err
	}
	switch status {
	case oee.RunAccepted:
		_, err = tx.ExecContext(ctx, `UPDATE supervisor.oee_calculation_run SET status='CANCELLED',failure_code='CANCELLED',cancel_generation=cancel_generation+1,cancel_requested_at=clock_timestamp(),finished_at=clock_timestamp(),row_version=row_version+1 WHERE tenant_id=$1 AND site_id=$2 AND run_id=$3`, sc.TenantID, sc.SiteID, runID)
	case oee.RunRunning:
		_, err = tx.ExecContext(ctx, `UPDATE supervisor.oee_calculation_run SET cancel_generation=cancel_generation+1,cancel_requested_at=clock_timestamp(),row_version=row_version+1 WHERE tenant_id=$1 AND site_id=$2 AND run_id=$3`, sc.TenantID, sc.SiteID, runID)
	default:
		return out, ErrLifecycle
	}
	if err != nil {
		return out, err
	}
	if err = audit(ctx, tx, sc, auditEntry{a.Issuer, a.Subject, "calculation.cancel_requested", "calculation", runID, "", "", "", correlation}); err != nil {
		return out, err
	}
	if status == oee.RunAccepted {
		if err = audit(ctx, tx, sc, auditEntry{a.Issuer, a.Subject, "calculation.cancelled", "calculation", runID, "", "", "CANCELLED", correlation}); err != nil {
			return out, err
		}
	}
	if err = completeKey(ctx, tx, a, sc, "calculation.cancel", key, 202, keyResponse{RunID: runID}); err != nil {
		return out, err
	}
	if out, err = loadRun(ctx, tx, sc, runID); err != nil {
		return out, err
	}
	return out, s.finish(ctx, tx, a)
}

// GetEvidence returns sealed evidence after re-authorizing the caller and re-verifying the
// stored digest, and records the read (ADR 0002 Sec. 11, 17).
func (s *Service) GetEvidence(ctx context.Context, a Actor, sc Scope, evidenceID, correlation string) (oee.OeeCalculationEvidenceV1, error) {
	var out oee.OeeCalculationEvidenceV1
	if !validID(evidenceID) {
		return out, ErrInvalid
	}
	tx, err := s.begin(ctx, a, sc, read)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	var body []byte
	var digest string
	err = tx.QueryRowContext(ctx, `SELECT evidence_json,evidence_sha256 FROM supervisor.oee_evidence_pack WHERE tenant_id=$1 AND site_id=$2 AND evidence_id=$3`, sc.TenantID, sc.SiteID, evidenceID).Scan(&body, &digest)
	if errors.Is(err, sql.ErrNoRows) {
		return out, ErrNotFound
	}
	if err != nil {
		return out, err
	}
	if err = json.Unmarshal(body, &out); err != nil {
		return out, errIntegrity
	}
	if got, err := oee.EvidenceDigest(sc.TenantID, sc.SiteID, out); err != nil || got != digest {
		return out, errIntegrity
	}
	if err = audit(ctx, tx, sc, auditEntry{a.Issuer, a.Subject, "evidence.read", "evidence", evidenceID, "", digest, "", correlation}); err != nil {
		return out, err
	}
	return out, s.finish(ctx, tx, a)
}
