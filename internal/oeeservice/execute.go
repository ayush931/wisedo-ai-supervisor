package oeeservice

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/oee"
	"github.com/google/uuid"
)

// claim is the fenced execution right obtained in Transaction B.
type claim struct {
	fence            string
	cancelGeneration int64
	deadline         time.Time
	issuer, subject  string
	ref              oee.OeePolicyRefV1
	policyJSON       []byte
	datasetSHA256    string
	started          time.Time
}

// Failure codes recorded on terminal runs; the set is fixed by the failure_code CHECK in
// migration 029.
const (
	codeDeadline    = "DEADLINE_EXCEEDED"
	codeLease       = "LEASE_EXPIRED"
	codeCancelled   = "CANCELLED"
	codeInvalid     = "INVALID_DATASET"
	codeCalculation = "CALCULATION_FAILED"
	codeRevoked     = "ACCESS_REVOKED"
	codeInternal    = "INTERNAL"
)

const (
	statusFailed    = oee.RunFailed
	statusCancelled = oee.RunCancelled
	// renewalsPerLease is how many renewals fit in one lease, so a single missed renewal
	// (a slow transaction) never lets a healthy executor lose its lease.
	renewalsPerLease = 3
)

// Execute runs one accepted calculation to a single terminal outcome (ADR 0002 Sec. 7.2-7.4).
// The dataset is the one verified at acceptance; it is held only in memory and must match the
// run's dataset digest. Execute never reports success without a committed seal: a failure,
// cancellation, passed deadline or lost lease ends in FAILED or CANCELLED with no evidence. A
// run that another executor already claimed is left alone.
func (s *Service) Execute(ctx context.Context, sc Scope, runID string, dataset oee.OeeDatasetV1, correlation string) error {
	if !validID(sc.TenantID) || !validID(sc.SiteID) || !validID(runID) {
		return ErrInvalid
	}
	c, err := s.claim(ctx, sc, runID, correlation)
	if err != nil || c == nil {
		return err
	}
	// The absolute deadline captured at acceptance bounds every later stage (ADR 0002 Sec. 10).
	// The renewer cancels the same context when the lease is lost or the run is cancelled, so
	// the next stage boundary stops work that can no longer seal.
	ctx, cancel := context.WithDeadline(ctx, c.deadline)
	defer cancel()
	stopRenewal := s.renewLease(ctx, sc, runID, c, cancel)
	defer stopRenewal()

	if dataset.ProvenanceDigest != c.datasetSHA256 {
		return s.fail(ctx, sc, runID, c, statusFailed, codeInvalid, correlation)
	}
	body, err := verifiedPolicy(sc, c.policyJSON, c.ref.Digest)
	if err != nil {
		return s.fail(ctx, sc, runID, c, statusFailed, codeInternal, correlation)
	}
	if ctx.Err() != nil {
		return s.resolveUnsealed(ctx, sc, runID, c, correlation)
	}
	evidence, err := oee.CalculateDataset(sc.TenantID, sc.SiteID, dataset, c.ref, body)
	if err != nil {
		// Acceptance already rejects malformed input, so these are a backstop: any kernel
		// input-contract error is still the dataset's fault, not the calculation's.
		code := codeCalculation
		if errors.Is(err, oee.ErrInvalidRequest) || errors.Is(err, oee.ErrTimelineLimit) || errors.Is(err, oee.ErrMalformedFact) ||
			errors.Is(err, oee.ErrInvalidKPIInput) || errors.Is(err, oee.ErrInvalidInterval) {
			code = codeInvalid
		}
		return s.fail(ctx, sc, runID, c, statusFailed, code, correlation)
	}
	if ctx.Err() != nil {
		return s.resolveUnsealed(ctx, sc, runID, c, correlation)
	}
	stopRenewal()
	sealed, err := s.seal(ctx, sc, runID, c, evidence, correlation)
	switch {
	case err != nil && (ctx.Err() != nil || isCheckViolation(err)):
		// The deadline ended mid-seal, or the run guard refused success because the lease or
		// deadline lapsed between the predicate and the trigger: a timeout, not a fault.
		if rerr := s.resolveUnsealed(ctx, sc, runID, c, correlation); rerr != nil {
			return errors.Join(err, rerr)
		}
		return nil
	case err != nil:
		// Any other error is a storage fault: record INTERNAL. The seal transaction rolled
		// back, so no partial evidence exists.
		if ferr := s.fail(ctx, sc, runID, c, statusFailed, codeInternal, correlation); ferr != nil {
			return errors.Join(err, ferr)
		}
		return err
	case !sealed:
		return s.resolveUnsealed(ctx, sc, runID, c, correlation)
	}
	return nil
}

// isCheckViolation reports a PostgreSQL check_violation (SQLSTATE 23514), which the run guard
// trigger raises when success can no longer be sealed.
func isCheckViolation(err error) bool {
	var state interface{ SQLState() string }
	return errors.As(err, &state) && state.SQLState() == "23514"
}

// claim is Transaction B: ACCEPTED -> RUNNING with a new fence, this owner and a lease of
// LeaseDuration capped at the deadline. The requester's membership is re-checked so revocation
// stops queued work (rule 03). A run past its deadline is failed here rather than left
// ACCEPTED. A run that is no longer ACCEPTED returns nil: its outcome belongs to someone else.
func (s *Service) claim(ctx context.Context, sc Scope, runID, correlation string) (*claim, error) {
	tx, err := s.scoped(ctx, sc)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	c := &claim{fence: uuid.NewString(), started: time.Now()}
	var status string
	var expired bool
	err = tx.QueryRowContext(ctx, `SELECT status,requested_by_issuer,requested_by_subject,deadline_at<=clock_timestamp() FROM supervisor.oee_calculation_run WHERE tenant_id=$1 AND site_id=$2 AND run_id=$3 FOR UPDATE`, sc.TenantID, sc.SiteID, runID).Scan(&status, &c.issuer, &c.subject, &expired)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if status != oee.RunAccepted {
		return nil, nil
	}
	if expired {
		return nil, s.terminateAccepted(ctx, tx, sc, runID, c, codeDeadline, correlation)
	}
	role, err := memberRole(ctx, tx, sc, c.issuer, c.subject)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	if err != nil || !allowed(role, calculate) {
		return nil, s.terminateAccepted(ctx, tx, sc, runID, c, codeRevoked, correlation)
	}
	err = tx.QueryRowContext(ctx, `UPDATE supervisor.oee_calculation_run r SET status='RUNNING',started_at=clock_timestamp(),execution_owner=$4,execution_fence=$5,
   execution_lease_until=LEAST(r.deadline_at,clock_timestamp()+make_interval(secs=>$6)),row_version=r.row_version+1
 FROM supervisor.oee_policy_revision v
 WHERE r.tenant_id=$1 AND r.site_id=$2 AND r.run_id=$3 AND r.status='ACCEPTED' AND r.deadline_at>clock_timestamp()
   AND (v.tenant_id,v.site_id,v.policy_id,v.policy_revision_id)=(r.tenant_id,r.site_id,r.policy_id,r.policy_revision_id)
 RETURNING r.cancel_generation,r.deadline_at,r.policy_id,r.policy_revision_id,v.revision_no,r.policy_sha256,v.policy_json,r.dataset_sha256`,
		sc.TenantID, sc.SiteID, runID, s.cfg.ExecutionOwner, c.fence, s.cfg.LeaseDuration.Seconds()).Scan(&c.cancelGeneration, &c.deadline, &c.ref.PolicyID, &c.ref.PolicyRevisionID, &c.ref.Revision, &c.ref.Digest, &c.policyJSON, &c.datasetSHA256)
	if errors.Is(err, sql.ErrNoRows) {
		// The deadline passed between the read above and this update.
		return nil, s.terminateAccepted(ctx, tx, sc, runID, c, codeDeadline, correlation)
	}
	if err != nil {
		return nil, err
	}
	return c, tx.Commit()
}

// terminateAccepted fails a still-ACCEPTED run inside the claim transaction and commits.
func (s *Service) terminateAccepted(ctx context.Context, tx *sql.Tx, sc Scope, runID string, c *claim, code, correlation string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE supervisor.oee_calculation_run SET status='FAILED',failure_code=$4,finished_at=clock_timestamp(),row_version=row_version+1 WHERE tenant_id=$1 AND site_id=$2 AND run_id=$3 AND status='ACCEPTED'`, sc.TenantID, sc.SiteID, runID, code); err != nil {
		return err
	}
	if err := audit(ctx, tx, sc, auditEntry{c.issuer, c.subject, "calculation.failed", "calculation", runID, "", "", code, correlation}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	logTerminal(runID, statusFailed, "", code, 0, c.started, correlation)
	return nil
}

// renewLease extends the claim's lease every LeaseDuration/renewalsPerLease until stopped or
// ctx ends. Renewal is forward-only, capped at the deadline, and only while this fence still
// holds an unexpired lease at the claimed cancel generation (migration 030 enforces the same in
// the database). When a renewal finds the lease lost or the run cancelled, it calls interrupt,
// so a cancelled run stops holding capacity instead of renewing until it finishes. The
// returned stop function is idempotent and waits for the renewer to exit.
func (s *Service) renewLease(ctx context.Context, sc Scope, runID string, c *claim, interrupt context.CancelFunc) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(s.cfg.LeaseDuration / renewalsPerLease)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				switch s.extendLease(ctx, sc, runID, c) {
				case leaseLost:
					interrupt()
					return
				case leaseAtDeadline:
					return
				}
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

type leaseState int

const (
	leaseHeld       leaseState = iota // renewed, or a transient error: try again next tick
	leaseAtDeadline                   // already reaches the deadline: nothing left to renew
	leaseLost                         // expired, taken, cancelled or no longer RUNNING
)

// extendLease performs one renewal. A transient database error reports leaseHeld so the next
// tick retries; the lease is long enough (renewalsPerLease ticks) to survive one failure.
func (s *Service) extendLease(ctx context.Context, sc Scope, runID string, c *claim) leaseState {
	tx, err := s.scoped(ctx, sc)
	if err != nil {
		return leaseHeld
	}
	defer tx.Rollback()
	var held, atDeadline bool
	err = tx.QueryRowContext(ctx, `SELECT status='RUNNING' AND execution_owner=$4 AND execution_fence=$5 AND cancel_generation=$6 AND execution_lease_until>clock_timestamp(),
       execution_lease_until>=deadline_at
  FROM supervisor.oee_calculation_run WHERE tenant_id=$1 AND site_id=$2 AND run_id=$3 FOR UPDATE`,
		sc.TenantID, sc.SiteID, runID, s.cfg.ExecutionOwner, c.fence, c.cancelGeneration).Scan(&held, &atDeadline)
	switch {
	case err != nil:
		return leaseHeld
	case !held:
		return leaseLost
	case atDeadline:
		return leaseAtDeadline
	}
	if _, err = tx.ExecContext(ctx, `UPDATE supervisor.oee_calculation_run SET execution_lease_until=LEAST(deadline_at,clock_timestamp()+make_interval(secs=>$4)),row_version=row_version+1
 WHERE tenant_id=$1 AND site_id=$2 AND run_id=$3 AND LEAST(deadline_at,clock_timestamp()+make_interval(secs=>$4))>execution_lease_until`,
		sc.TenantID, sc.SiteID, runID, s.cfg.LeaseDuration.Seconds()); err != nil {
		return leaseHeld
	}
	_ = tx.Commit() // a failed commit is transient: the next tick retries
	return leaseHeld
}

// seal is Transaction C (ADR 0002 Sec. 7.3): the conditional SUCCEEDED update must match
// status, owner, fence, cancel generation, an unexpired lease and deadline under the database
// clock. Only then are run sources, the evidence pack and audit written, in the same
// transaction. Zero matching rows rolls everything back and reports unsealed.
func (s *Service) seal(ctx context.Context, sc Scope, runID string, c *claim, evidence oee.OeeCalculationEvidenceV1, correlation string) (bool, error) {
	tx, err := s.scoped(ctx, sc)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var sealedAt time.Time
	if err = tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&sealedAt); err != nil {
		return false, err
	}
	evidence.EvidenceID, evidence.RunID, evidence.SealedAt = uuid.NewString(), runID, wireTime(sealedAt)
	res, err := tx.ExecContext(ctx, `UPDATE supervisor.oee_calculation_run SET status='SUCCEEDED',result_status=$7,truth_state='CANDIDATE',finished_at=clock_timestamp(),row_version=row_version+1
 WHERE tenant_id=$1 AND site_id=$2 AND run_id=$3 AND status='RUNNING' AND execution_owner=$4 AND execution_fence=$5 AND cancel_generation=$6
   AND execution_lease_until>clock_timestamp() AND deadline_at>clock_timestamp()`,
		sc.TenantID, sc.SiteID, runID, s.cfg.ExecutionOwner, c.fence, c.cancelGeneration, string(evidence.Status))
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return false, nil
	}
	for i, src := range evidence.Sources {
		if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.oee_run_source(tenant_id,site_id,run_id,source_ordinal,source_key,owner_service,source_revision,dataset_schema_id,consistency_mode,snapshot_ref,watermark_ref,contribution_sha256,fact_count) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,NULLIF($10,''),NULLIF($11,''),$12,$13)`,
			sc.TenantID, sc.SiteID, runID, i, src.SourceKey, src.OwnerService, src.SourceRevision, oee.DatasetSchemaID, src.ConsistencyMode, src.SnapshotRef, src.WatermarkRef, src.ContributionSHA256, src.FactCount); err != nil {
			return false, err
		}
	}
	body, err := json.Marshal(evidence)
	if err != nil {
		return false, err
	}
	digest, err := oee.EvidenceDigest(sc.TenantID, sc.SiteID, evidence)
	if err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.oee_evidence_pack(tenant_id,site_id,evidence_id,run_id,schema_version,result_status,truth_state,dataset_sha256,policy_sha256,calculation_version,evidence_json,evidence_sha256) VALUES($1,$2,$3,$4,1,$5,'CANDIDATE',$6,$7,$8,$9,$10)`,
		sc.TenantID, sc.SiteID, evidence.EvidenceID, runID, string(evidence.Status), evidence.Digests.Dataset, evidence.Policy.Digest, evidence.KernelVersion+"/"+evidence.FormulaVersion, body, digest); err != nil {
		return false, err
	}
	if err = audit(ctx, tx, sc, auditEntry{c.issuer, c.subject, "calculation.succeeded", "calculation", runID, "", digest, string(evidence.Status), correlation}); err != nil {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	facts := 0
	for _, src := range evidence.Sources {
		facts += src.FactCount
	}
	logTerminal(runID, oee.RunSucceeded, string(evidence.Status), "", facts, c.started, correlation)
	return true, nil
}

// resolveUnsealed records why a run held by this executor did not seal, in one transaction
// under the run's row lock: a newer cancel generation is CANCELLED; otherwise the deadline or
// the lease ended, which is FAILED. If this fence no longer holds the run, another actor
// already decided its outcome and nothing is written. It runs under its own short budget,
// because the run's context may already have ended.
func (s *Service) resolveUnsealed(ctx context.Context, sc Scope, runID string, c *claim, correlation string) error {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	tx, err := s.scoped(cleanup, sc)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var status, fence string
	var generation int64
	var deadlinePassed bool
	err = tx.QueryRowContext(cleanup, `SELECT status,COALESCE(execution_fence::text,''),cancel_generation,deadline_at<=clock_timestamp() FROM supervisor.oee_calculation_run WHERE tenant_id=$1 AND site_id=$2 AND run_id=$3 FOR UPDATE`, sc.TenantID, sc.SiteID, runID).Scan(&status, &fence, &generation, &deadlinePassed)
	if err != nil {
		return err
	}
	if status != oee.RunRunning || fence != c.fence {
		return nil
	}
	finalStatus, code := statusFailed, codeLease
	switch {
	case generation != c.cancelGeneration:
		finalStatus, code = statusCancelled, codeCancelled
	case deadlinePassed || errors.Is(ctx.Err(), context.DeadlineExceeded):
		code = codeDeadline
	}
	return s.terminateRunning(cleanup, tx, sc, runID, c, finalStatus, code, correlation)
}

// fail commits FAILED or CANCELLED with the given code for a run this fence still holds. It
// runs under its own short budget so it can record the outcome after the run context ended.
func (s *Service) fail(ctx context.Context, sc Scope, runID string, c *claim, status, code, correlation string) error {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	tx, err := s.scoped(cleanup, sc)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	return s.terminateRunning(cleanup, tx, sc, runID, c, status, code, correlation)
}

// terminateRunning moves a RUNNING run held by this fence to a terminal state, audits it and
// commits. A run the fence no longer holds is left untouched, so this can never overwrite
// another terminal outcome or a success.
func (s *Service) terminateRunning(ctx context.Context, tx *sql.Tx, sc Scope, runID string, c *claim, status, code, correlation string) error {
	res, err := tx.ExecContext(ctx, `UPDATE supervisor.oee_calculation_run SET status=$5,failure_code=$6,finished_at=clock_timestamp(),row_version=row_version+1 WHERE tenant_id=$1 AND site_id=$2 AND run_id=$3 AND status='RUNNING' AND execution_fence=$4`,
		sc.TenantID, sc.SiteID, runID, c.fence, status, code)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil
	}
	action := "calculation.failed"
	if status == statusCancelled {
		action = "calculation.cancelled"
	}
	if err = audit(ctx, tx, sc, auditEntry{c.issuer, c.subject, action, "calculation", runID, "", "", code, correlation}); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	logTerminal(runID, status, "", code, 0, c.started, correlation)
	return nil
}

// logTerminal records one terminal outcome with bounded fields only: opaque run and correlation
// IDs, enum status and code, a count and a duration (ADR 0002 Sec. 11, 19). No tenant ID, fact
// content, policy body or SQL is logged.
func logTerminal(runID, status, resultStatus, failureCode string, facts int, started time.Time, correlation string) {
	slog.Info("oee run terminal", "run_id", runID, "status", status, "result_status", resultStatus,
		"failure_code", failureCode, "fact_count", facts, "duration_ms", time.Since(started).Milliseconds(), "correlation_id", correlation)
}
