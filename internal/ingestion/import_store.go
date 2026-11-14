package ingestion

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/operational"
)

// tenantTx opens a transaction scoped to one tenant, with the same timeouts as intake.
func (s *Service) tenantTx(ctx context.Context, tenantID string) (*sql.Tx, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true),set_config('statement_timeout','10s',true),set_config('lock_timeout','5s',true)`, tenantID); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	return tx, nil
}

// requireSession checks the browser session belongs to the actor and is not idle or expired.
func (s *Service) requireSession(ctx context.Context, tx *sql.Tx, a Actor) error {
	var issuer, subject string
	var expires, seen, now time.Time
	err := tx.QueryRowContext(ctx, `SELECT issuer,subject,expires_at,last_seen_at,clock_timestamp() FROM supervisor.browser_sessions WHERE token_hash=$1 FOR SHARE`, a.SessionHash).Scan(&issuer, &subject, &expires, &seen, &now)
	if err != nil || issuer != a.Issuer || subject != a.Subject || !expires.After(now) || !seen.After(now.Add(-s.sessions.Idle)) {
		return ErrDenied
	}
	return nil
}

// requireIntakeRole allows owners, supervisors and operators to submit intake. Viewers and
// non-members get no job row at all, so a denied request leaves nothing behind.
func (s *Service) requireIntakeRole(ctx context.Context, a Actor, sc Scope) error {
	tx, err := s.tenantTx(ctx, sc.TenantID)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = s.requireSession(ctx, tx, a); err != nil {
		return err
	}
	var found bool
	if err = tx.QueryRowContext(ctx, `SELECT supervisor.lock_member_site($1,$2,$3,$4,$5)`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, true).Scan(&found); err != nil {
		return err
	}
	if !found {
		return ErrNotFound
	}
	var role string
	if err = tx.QueryRowContext(ctx, `SELECT role FROM supervisor.memberships WHERE tenant_id=$1 AND site_id=$2 AND issuer=$3 AND subject=$4 AND active FOR SHARE`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject).Scan(&role); err != nil {
		return ErrNotFound
	}
	if role != "owner" && role != "supervisor" && role != "operator" {
		return ErrDenied
	}
	return tx.Commit()
}

// requireReadAccess allows any active member of the site to read jobs.
func (s *Service) requireReadAccess(ctx context.Context, a Actor, sc Scope) error {
	tx, err := s.tenantTx(ctx, sc.TenantID)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = s.readAccessTx(ctx, tx, a, sc); err != nil {
		return err
	}
	return tx.Commit()
}

// readAccessTx checks read access inside the caller's transaction. The membership row is held
// FOR SHARE, so a concurrent revocation waits until the caller's read has committed.
func (s *Service) readAccessTx(ctx context.Context, tx *sql.Tx, a Actor, sc Scope) error {
	if err := s.requireSession(ctx, tx, a); err != nil {
		return err
	}
	var found bool
	if err := tx.QueryRowContext(ctx, `SELECT supervisor.lock_member_site($1,$2,$3,$4,$5)`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, false).Scan(&found); err != nil {
		return err
	}
	if !found {
		return ErrNotFound
	}
	var one int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM supervisor.memberships WHERE tenant_id=$1 AND site_id=$2 AND issuer=$3 AND subject=$4 AND active FOR SHARE`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// recordRows stores row outcomes as each chunk finishes, so a job interrupted mid-import
// still shows what was committed. A resumed job re-records identical rows harmlessly.
func (s *Service) recordRows(ctx context.Context, sc Scope, jobID string, rows []ImportRow) error {
	tx, err := s.tenantTx(ctx, sc.TenantID)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// The job row lock serializes with a source revocation, which cancels running jobs under
	// the same lock: once a job is cancelled, no further rows are recorded for it.
	var status string
	if err = tx.QueryRowContext(ctx, `SELECT status FROM supervisor.operational_import_jobs WHERE tenant_id=$1 AND site_id=$2 AND id=$3 FOR SHARE`, sc.TenantID, sc.SiteID, jobID).Scan(&status); err != nil {
		return err
	}
	if status != "running" {
		return ErrImportCancelled
	}
	for _, o := range rows {
		if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.operational_import_rows(tenant_id,site_id,job_id,row_index,status,reason,source_sequence) VALUES($1,$2,$3,$4,$5,NULLIF($6,''),NULLIF($7,0)) ON CONFLICT DO NOTHING`, sc.TenantID, sc.SiteID, jobID, o.Index, o.Status, o.Reason, o.SourceSequence); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// finishJob records the job's counts and final status: completed when every row was
// processed, partial when a row was rejected. Rows were recorded per chunk and are never
// rewritten.
func (s *Service) finishJob(ctx context.Context, a Actor, sc Scope, jobID string, outcomes []ImportRow) error {
	var accepted, duplicate, quarantined, rejected int
	for _, o := range outcomes {
		switch o.Status {
		case "accepted":
			accepted++
		case "duplicate":
			duplicate++
		case "quarantined":
			quarantined++
		default:
			rejected++
		}
	}
	tx, err := s.tenantTx(ctx, sc.TenantID)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	status := "completed"
	if rejected > 0 {
		status = "partial"
	}
	if _, err = tx.ExecContext(ctx, `UPDATE supervisor.operational_import_jobs SET status=$4,accepted_rows=$5,duplicate_rows=$6,quarantined_rows=$7,rejected_rows=$8,finished_at=clock_timestamp() WHERE tenant_id=$1 AND site_id=$2 AND id=$3 AND status='running'`, sc.TenantID, sc.SiteID, jobID, status, accepted, duplicate, quarantined, rejected); err != nil {
		return err
	}
	return tx.Commit()
}

// loadJob reads a job and one page of its rows.
func (s *Service) loadJob(ctx context.Context, a Actor, sc Scope, jobID string, after, limit int) (ImportJob, error) {
	var out ImportJob
	tx, err := s.tenantTx(ctx, sc.TenantID)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	var finished sql.NullTime
	err = tx.QueryRowContext(ctx, `SELECT id::text,source_id::text,format,file_sha256,status,row_count,start_sequence,source_revision,mapping_revision,accepted_rows,duplicate_rows,quarantined_rows,rejected_rows,created_at,finished_at FROM supervisor.operational_import_jobs WHERE tenant_id=$1 AND site_id=$2 AND id=$3`, sc.TenantID, sc.SiteID, jobID).Scan(&out.ID, &out.SourceID, &out.Format, &out.FileSHA256, &out.Status, &out.RowCount, &out.StartSequence, &out.SourceRevision, &out.MappingRevision, &out.AcceptedRows, &out.DuplicateRows, &out.QuarantinedRows, &out.RejectedRows, &out.CreatedAt, &finished)
	if errors.Is(err, sql.ErrNoRows) {
		return out, ErrNotFound
	}
	if err != nil {
		return out, err
	}
	if finished.Valid {
		t := finished.Time
		out.FinishedAt = &t
	}
	rows, err := tx.QueryContext(ctx, `SELECT row_index,status,coalesce(reason,''),coalesce(source_sequence,0) FROM supervisor.operational_import_rows WHERE tenant_id=$1 AND site_id=$2 AND job_id=$3 AND row_index>$4 ORDER BY row_index LIMIT $5`, sc.TenantID, sc.SiteID, jobID, after, limit)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	out.Rows = []ImportRow{}
	for rows.Next() {
		var r ImportRow
		if err = rows.Scan(&r.Index, &r.Status, &r.Reason, &r.SourceSequence); err != nil {
			return out, err
		}
		out.Rows = append(out.Rows, r)
	}
	if err = rows.Err(); err != nil {
		return out, err
	}
	return out, tx.Commit()
}

// importLease is how long an import may stay running before new work for its source may abandon
// it. An import runs inside one request (the server's write timeout is 30 s), so a job still
// running after this lease was interrupted, and its author has not retried it.
const importLease = 15 * time.Minute

// cancelStaleImports ends, as 'cancelled', any import for the source still running importLease
// after it was created, with its counts frozen from the rows recorded so far (WIS-699). It takes
// the job row lock, which the importer also takes before recording rows, so the importer sees the
// cancellation and stops (ErrImportCancelled); no row can be added after it is committed. A retry
// with the same key then replays the cancelled job instead of resuming it.
func cancelStaleImports(ctx context.Context, tx *sql.Tx, sc Scope, sourceID string) error {
	return operational.CancelImports(ctx, tx, sc.TenantID, sc.SiteID, sourceID, "", importLease)
}

// abandonJob ends one import whose request failed or was interrupted before it finished (WIS-719):
// the job becomes 'cancelled' with the rows recorded so far, instead of holding its source for the
// whole importLease. It runs on a fresh context, so the client's disconnect that interrupted the
// import cannot also stop its cleanup. importLease stays the backstop for a process that died.
func (s *Service) abandonJob(ctx context.Context, sc Scope, sourceID, jobID string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	tx, err := s.tenantTx(ctx, sc.TenantID)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = operational.CancelImports(ctx, tx, sc.TenantID, sc.SiteID, sourceID, jobID, 0); err != nil {
		return err
	}
	return tx.Commit()
}

// ErrImportNotRunning is returned when a cancellation names a job that already finished or was
// cancelled: a lifecycle conflict, not an idempotent replay.
var ErrImportNotRunning = errors.New("the import is not running")

// CancelJob lets an owner or supervisor end a running import, for example one left running by a
// crashed process that would otherwise hold its source until importLease (WIS-719). It uses the
// same primitive as the lease, abandonJob and revocation (operational.CancelImports), so the job
// keeps the outcomes of every chunk that committed. The job row is locked first: a chunk in flight
// holds it FOR SHARE, so the cancellation waits for that chunk and then counts its rows. The
// receipt names the actor and key; a same-key retry by the same actor replays it.
func (s *Service) CancelJob(ctx context.Context, a Actor, sc Scope, jobID, key string) (ImportJob, error) {
	if !validID(jobID) {
		return ImportJob{}, ErrNotFound
	}
	tx, err := s.tenantTx(ctx, sc.TenantID)
	if err != nil {
		return ImportJob{}, err
	}
	defer tx.Rollback()
	if err = s.requireSession(ctx, tx, a); err != nil {
		return ImportJob{}, err
	}
	var found bool
	if err = tx.QueryRowContext(ctx, `SELECT supervisor.lock_member_site($1,$2,$3,$4,$5)`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, true).Scan(&found); err != nil {
		return ImportJob{}, err
	}
	if !found {
		return ImportJob{}, ErrNotFound
	}
	// Narrower than requireIntakeRole: operators may import but not end another person's import.
	var role string
	if err = tx.QueryRowContext(ctx, `SELECT role FROM supervisor.memberships WHERE tenant_id=$1 AND site_id=$2 AND issuer=$3 AND subject=$4 AND active FOR SHARE`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject).Scan(&role); err != nil {
		return ImportJob{}, ErrNotFound
	}
	if role != "owner" && role != "supervisor" {
		return ImportJob{}, ErrDenied
	}
	var sourceID, status string
	err = tx.QueryRowContext(ctx, `SELECT source_id::text,status FROM supervisor.operational_import_jobs WHERE tenant_id=$1 AND site_id=$2 AND id=$3 FOR UPDATE`, sc.TenantID, sc.SiteID, jobID).Scan(&sourceID, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return ImportJob{}, ErrNotFound
	}
	if err != nil {
		return ImportJob{}, err
	}
	var prior string
	err = tx.QueryRowContext(ctx, `SELECT job_id::text FROM supervisor.operational_import_cancellations WHERE tenant_id=$1 AND site_id=$2 AND actor_issuer=$3 AND actor_subject=$4 AND idempotency_key=$5`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, key).Scan(&prior)
	replayed := err == nil
	switch {
	case replayed && prior != jobID:
		return ImportJob{}, ErrConflict
	case replayed:
	case !errors.Is(err, sql.ErrNoRows):
		return ImportJob{}, err
	case status != "running":
		return ImportJob{}, ErrImportNotRunning
	default:
		if err = operational.CancelImports(ctx, tx, sc.TenantID, sc.SiteID, sourceID, jobID, 0); err != nil {
			return ImportJob{}, err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.operational_import_cancellations(tenant_id,site_id,job_id,actor_issuer,actor_subject,idempotency_key) VALUES($1,$2,$3,$4,$5,$6)`, sc.TenantID, sc.SiteID, jobID, a.Issuer, a.Subject, key); err != nil {
			return ImportJob{}, err
		}
	}
	if err = tx.Commit(); err != nil {
		return ImportJob{}, err
	}
	job, err := s.loadJob(ctx, a, sc, jobID, 0, ImportPageRows)
	job.Replayed = replayed
	return job, err
}
