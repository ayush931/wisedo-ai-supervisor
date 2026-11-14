package ingestion

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/limits"
)

// SourceRef identifies one operational source for worker-side processing.
type SourceRef struct{ TenantID, SiteID, SourceID string }

// Progress is one immutable fold of the outbox into completeness and liveness.
// Absent facts are nil, never zero: a missing heartbeat or watermark is NOT_READY.
type Progress struct {
	Revision                 int64
	ProcessedSequence        int64
	CompleteSequence         int64
	QuarantinedCount         int
	FirstQuarantinedSequence *int64
	LastEventAt              *time.Time
	LastEventReceivedAt      *time.Time
	LastHeartbeatEventAt     *time.Time
	LastHeartbeatReceivedAt  *time.Time
	ClockQuality             string
	WatermarkSequence        *int64
	WatermarkEventAt         *time.Time
}

type outboxRow struct {
	Sequence    int64
	Kind        string
	Quarantined bool
	EventAt     time.Time
	ReceivedAt  time.Time
}

// fold applies contiguous outbox rows to prev. It stops at the first gap so the
// processed cursor only ever covers a durable, contiguous range. Completeness is
// the longest quarantine-free prefix; the first quarantined row freezes it.
func fold(prev Progress, rows []outboxRow, skew time.Duration) (Progress, int) {
	next := prev
	next.Revision = prev.Revision + 1
	if next.ClockQuality == "" {
		next.ClockQuality = "unknown"
	}
	n := 0
	for _, r := range rows {
		if r.Sequence != next.ProcessedSequence+1 {
			break
		}
		next.ProcessedSequence = r.Sequence
		n++
		eventAt, receivedAt := r.EventAt, r.ReceivedAt
		next.LastEventAt, next.LastEventReceivedAt = &eventAt, &receivedAt
		if r.Quarantined {
			next.QuarantinedCount++
			if next.FirstQuarantinedSequence == nil {
				seq := r.Sequence
				next.FirstQuarantinedSequence = &seq
			}
			continue
		}
		if next.FirstQuarantinedSequence == nil {
			next.CompleteSequence = r.Sequence
		}
		switch r.Kind {
		case "heartbeat":
			next.LastHeartbeatEventAt, next.LastHeartbeatReceivedAt = &eventAt, &receivedAt
			drift := receivedAt.Sub(eventAt)
			if drift < 0 {
				drift = -drift
			}
			if drift > skew {
				next.ClockQuality = "suspect"
			} else {
				next.ClockQuality = "ok"
			}
		case "watermark":
			// A watermark only counts once the prefix up to it is complete.
			if next.FirstQuarantinedSequence == nil {
				seq := r.Sequence
				next.WatermarkSequence, next.WatermarkEventAt = &seq, &eventAt
			}
		}
	}
	return next, n
}

func (s *Service) beginWorker(ctx context.Context, tenantID string) (*sql.Tx, error) {
	if !validID(tenantID) {
		return nil, ErrInvalid
	}
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

func scanProgress(row interface{ Scan(...any) error }) (Progress, error) {
	var p Progress
	err := row.Scan(&p.Revision, &p.ProcessedSequence, &p.CompleteSequence, &p.QuarantinedCount, &p.FirstQuarantinedSequence, &p.LastEventAt, &p.LastEventReceivedAt, &p.LastHeartbeatEventAt, &p.LastHeartbeatReceivedAt, &p.ClockQuality, &p.WatermarkSequence, &p.WatermarkEventAt)
	return p, err
}

const progressColumns = `r.revision,r.processed_sequence,r.complete_sequence,r.quarantined_count,r.first_quarantined_sequence,r.last_event_at,r.last_event_received_at,r.last_heartbeat_event_at,r.last_heartbeat_received_at,r.clock_quality,r.watermark_sequence,r.watermark_event_at`

func insertRevision(ctx context.Context, tx *sql.Tx, ref SourceRef, p Progress) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO supervisor.operational_source_progress_revisions(tenant_id,site_id,source_id,revision,processed_sequence,complete_sequence,quarantined_count,first_quarantined_sequence,last_event_at,last_event_received_at,last_heartbeat_event_at,last_heartbeat_received_at,clock_quality,watermark_sequence,watermark_event_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`, ref.TenantID, ref.SiteID, ref.SourceID, p.Revision, p.ProcessedSequence, p.CompleteSequence, p.QuarantinedCount, p.FirstQuarantinedSequence, p.LastEventAt, p.LastEventReceivedAt, p.LastHeartbeatEventAt, p.LastHeartbeatReceivedAt, p.ClockQuality, p.WatermarkSequence, p.WatermarkEventAt)
	return err
}

// Fold durably consumes up to limit contiguous outbox rows for one source and
// records one new progress revision. It returns the number of rows folded; 0
// means the cursor is current and nothing was written. Retries are idempotent:
// the guard trigger rejects any advance that is not exactly the next
// contiguous durable range, and a current cursor writes nothing.
func (s *Service) Fold(ctx context.Context, ref SourceRef, limit int) (int, error) {
	if !validID(ref.SiteID) || !validID(ref.SourceID) || limit < 1 {
		return 0, ErrInvalid
	}
	tx, err := s.beginWorker(ctx, ref.TenantID)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var accepted int64
	if err = tx.QueryRowContext(ctx, `SELECT last_sequence FROM supervisor.operational_event_sequence_heads WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3 FOR SHARE`, ref.TenantID, ref.SiteID, ref.SourceID).Scan(&accepted); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrNotFound
		}
		return 0, err
	}
	var skewSeconds int64
	if err = tx.QueryRowContext(ctx, `SELECT r.clock_skew_seconds FROM supervisor.operational_source_heads h JOIN supervisor.operational_source_revisions r ON (r.tenant_id,r.site_id,r.source_id,r.revision)=(h.tenant_id,h.site_id,h.source_id,h.current_revision) WHERE h.tenant_id=$1 AND h.site_id=$2 AND h.source_id=$3`, ref.TenantID, ref.SiteID, ref.SourceID).Scan(&skewSeconds); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrNotFound
		}
		return 0, err
	}
	// Create-or-lock the head before reading its revision in a separate
	// statement: under READ COMMITTED a joined read re-evaluated after another
	// worker's commit would see the new head but not its revision row.
	created, err := tx.ExecContext(ctx, `INSERT INTO supervisor.operational_source_progress_heads(tenant_id,site_id,source_id,processed_sequence,current_revision) VALUES($1,$2,$3,0,1) ON CONFLICT DO NOTHING`, ref.TenantID, ref.SiteID, ref.SourceID)
	if err != nil {
		return 0, err
	}
	if n, _ := created.RowsAffected(); n == 1 {
		if err = insertRevision(ctx, tx, ref, Progress{Revision: 1, ClockQuality: "unknown"}); err != nil {
			return 0, err
		}
	}
	var currentRevision int64
	if err = tx.QueryRowContext(ctx, `SELECT current_revision FROM supervisor.operational_source_progress_heads WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3 FOR UPDATE`, ref.TenantID, ref.SiteID, ref.SourceID).Scan(&currentRevision); err != nil {
		return 0, err
	}
	prev, err := scanProgress(tx.QueryRowContext(ctx, `SELECT `+progressColumns+` FROM supervisor.operational_source_progress_revisions r WHERE r.tenant_id=$1 AND r.site_id=$2 AND r.source_id=$3 AND r.revision=$4`, ref.TenantID, ref.SiteID, ref.SourceID, currentRevision))
	if err != nil {
		return 0, err
	}
	if prev.ProcessedSequence >= accepted {
		return 0, tx.Commit()
	}
	rows, err := tx.QueryContext(ctx, `SELECT o.source_sequence,e.event_kind,e.quarantine_reason IS NOT NULL,e.event_at,b.accepted_at FROM supervisor.operational_event_outbox o JOIN supervisor.operational_event_records e ON (e.tenant_id,e.site_id,e.source_id,e.source_sequence)=(o.tenant_id,o.site_id,o.source_id,o.source_sequence) JOIN supervisor.operational_event_batches b ON (b.tenant_id,b.site_id,b.batch_id)=(o.tenant_id,o.site_id,o.batch_id) WHERE o.tenant_id=$1 AND o.site_id=$2 AND o.source_id=$3 AND o.source_sequence>$4 ORDER BY o.source_sequence LIMIT $5`, ref.TenantID, ref.SiteID, ref.SourceID, prev.ProcessedSequence, limit)
	if err != nil {
		return 0, err
	}
	var pending []outboxRow
	for rows.Next() {
		var r outboxRow
		if err = rows.Scan(&r.Sequence, &r.Kind, &r.Quarantined, &r.EventAt, &r.ReceivedAt); err != nil {
			rows.Close()
			return 0, err
		}
		pending = append(pending, r)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return 0, err
	}
	next, n := fold(prev, pending, time.Duration(skewSeconds)*time.Second)
	if n == 0 {
		return 0, tx.Commit()
	}
	if err = insertRevision(ctx, tx, ref, next); err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE supervisor.operational_source_progress_heads SET processed_sequence=$4,current_revision=$5 WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3`, ref.TenantID, ref.SiteID, ref.SourceID, next.ProcessedSequence, next.Revision); err != nil {
		return 0, err
	}
	return n, tx.Commit()
}

// Lagging lists sources whose accepted cursor is ahead of the processed cursor.
func (s *Service) Lagging(ctx context.Context, tenantID string) ([]SourceRef, error) {
	tx, err := s.beginWorker(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT q.site_id,q.source_id FROM supervisor.operational_event_sequence_heads q LEFT JOIN supervisor.operational_source_progress_heads p ON (p.tenant_id,p.site_id,p.source_id)=(q.tenant_id,q.site_id,q.source_id) WHERE q.tenant_id=$1 AND q.last_sequence>coalesce(p.processed_sequence,0) ORDER BY q.last_sequence-coalesce(p.processed_sequence,0) DESC,q.site_id,q.source_id LIMIT 500`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var refs []SourceRef
	for rows.Next() {
		ref := SourceRef{TenantID: tenantID}
		if err = rows.Scan(&ref.SiteID, &ref.SourceID); err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return refs, tx.Commit()
}

// Liveness is the read model for one source. Null fields are unknown facts.
type Liveness struct {
	SourceID     string           `json:"source_id"`
	EvaluatedAt  time.Time        `json:"evaluated_at"`
	Readiness    string           `json:"readiness"`
	Reasons      []string         `json:"reasons"`
	Policy       LivenessPolicy   `json:"policy"`
	Ingest       LivenessIngest   `json:"ingest"`
	Liveness     *LivenessSignals `json:"liveness"`
	Completeness *Completeness    `json:"completeness"`
}
type LivenessPolicy struct {
	FreshnessSeconds int64 `json:"freshness_seconds"`
	ClockSkewSeconds int64 `json:"clock_skew_seconds"`
	// EffectiveFreshnessSeconds is the heartbeat age limit actually enforced: freshness_seconds
	// capped at limits.MaxHeartbeatAge, the same limit readiness and dispatch apply.
	EffectiveFreshnessSeconds int64 `json:"effective_freshness_seconds"`
}
type LivenessIngest struct {
	AcceptedSequence  int64  `json:"accepted_sequence"`
	ProcessedSequence *int64 `json:"processed_sequence"`
	ProgressRevision  *int64 `json:"progress_revision"`
}
type LivenessSignals struct {
	LastHeartbeatEventAt    *time.Time `json:"last_heartbeat_event_at"`
	LastHeartbeatReceivedAt *time.Time `json:"last_heartbeat_received_at"`
	HeartbeatAgeSeconds     *int64     `json:"heartbeat_age_seconds"`
	ClockQuality            string     `json:"clock_quality"`
	LastEventAt             *time.Time `json:"last_event_at"`
}
type Completeness struct {
	CompleteSequence         int64      `json:"complete_sequence"`
	FirstQuarantinedSequence *int64     `json:"first_quarantined_sequence"`
	QuarantinedCount         int        `json:"quarantined_count"`
	WatermarkSequence        *int64     `json:"watermark_sequence"`
	WatermarkEventAt         *time.Time `json:"watermark_event_at"`
}

// evaluate derives readiness from a fold. Unknown or stale facts are NOT_READY.
func evaluate(p *Progress, accepted int64, policy LivenessPolicy, now time.Time) (string, []string) {
	reasons := []string{}
	if p == nil {
		return "NOT_READY", []string{"NOT_PROCESSED"}
	}
	if p.ProcessedSequence < accepted {
		reasons = append(reasons, "CONSUMER_LAGGING")
	}
	switch {
	case p.LastHeartbeatReceivedAt == nil:
		reasons = append(reasons, "NO_HEARTBEAT")
	case now.Sub(*p.LastHeartbeatReceivedAt) > limits.HeartbeatLimit(policy.FreshnessSeconds):
		reasons = append(reasons, "HEARTBEAT_STALE")
	}
	if p.ClockQuality == "suspect" {
		reasons = append(reasons, "CLOCK_SUSPECT")
	}
	if p.FirstQuarantinedSequence != nil {
		reasons = append(reasons, "QUARANTINE_GAP")
	}
	if p.WatermarkSequence == nil {
		reasons = append(reasons, "NO_WATERMARK")
	}
	if len(reasons) == 0 {
		return "READY", reasons
	}
	return "NOT_READY", reasons
}

// Liveness evaluates one source for an authenticated site member (read scope).
func (s *Service) Liveness(ctx context.Context, a Actor, sc Scope, sourceID string) (Liveness, error) {
	var out Liveness
	if !validID(sc.TenantID) || !validID(sc.SiteID) || !validID(sourceID) || !validText(a.Issuer, 255) || !validText(a.Subject, 255) || !validText(a.SessionHash, 128) {
		return out, ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true),set_config('statement_timeout','10s',true),set_config('lock_timeout','5s',true)`, sc.TenantID); err != nil {
		return out, err
	}
	var issuer, subject string
	var expires, seen, now time.Time
	err = tx.QueryRowContext(ctx, `SELECT issuer,subject,expires_at,last_seen_at,clock_timestamp() FROM supervisor.browser_sessions WHERE token_hash=$1 FOR SHARE`, a.SessionHash).Scan(&issuer, &subject, &expires, &seen, &now)
	if err != nil || issuer != a.Issuer || subject != a.Subject || !expires.After(now) || !seen.After(now.Add(-s.sessions.Idle)) {
		return out, ErrDenied
	}
	var found bool
	if err = tx.QueryRowContext(ctx, `SELECT supervisor.lock_member_site($1,$2,$3,$4,$5)`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, false).Scan(&found); err != nil {
		return out, err
	}
	if !found {
		return out, ErrNotFound
	}
	var role string
	if err = tx.QueryRowContext(ctx, `SELECT role FROM supervisor.memberships WHERE tenant_id=$1 AND site_id=$2 AND issuer=$3 AND subject=$4 AND active FOR SHARE`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject).Scan(&role); err != nil {
		return out, ErrNotFound
	}
	if err = tx.QueryRowContext(ctx, `SELECT r.freshness_seconds,r.clock_skew_seconds FROM supervisor.operational_source_heads h JOIN supervisor.operational_source_revisions r ON (r.tenant_id,r.site_id,r.source_id,r.revision)=(h.tenant_id,h.site_id,h.source_id,h.current_revision) WHERE h.tenant_id=$1 AND h.site_id=$2 AND h.source_id=$3`, sc.TenantID, sc.SiteID, sourceID).Scan(&out.Policy.FreshnessSeconds, &out.Policy.ClockSkewSeconds); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return out, ErrNotFound
		}
		return out, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT coalesce((SELECT last_sequence FROM supervisor.operational_event_sequence_heads WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3),0)`, sc.TenantID, sc.SiteID, sourceID).Scan(&out.Ingest.AcceptedSequence); err != nil {
		return out, err
	}
	var progress *Progress
	p, err := scanProgress(tx.QueryRowContext(ctx, `SELECT `+progressColumns+` FROM supervisor.operational_source_progress_heads h JOIN supervisor.operational_source_progress_revisions r ON (r.tenant_id,r.site_id,r.source_id,r.revision)=(h.tenant_id,h.site_id,h.source_id,h.current_revision) WHERE h.tenant_id=$1 AND h.site_id=$2 AND h.source_id=$3`, sc.TenantID, sc.SiteID, sourceID))
	if err == nil {
		progress = &p
	} else if !errors.Is(err, sql.ErrNoRows) {
		return out, err
	}
	out.SourceID = sourceID
	buildLiveness(&out, progress, now)
	return out, tx.Commit()
}

// buildLiveness fills out's evaluation, signals and completeness from a fold at now.
// Every time is emitted in UTC, like every other read API (rule 04).
func buildLiveness(out *Liveness, progress *Progress, now time.Time) {
	now = now.UTC()
	out.EvaluatedAt = now
	out.Policy.EffectiveFreshnessSeconds = int64(limits.HeartbeatLimit(out.Policy.FreshnessSeconds) / time.Second)
	out.Readiness, out.Reasons = evaluate(progress, out.Ingest.AcceptedSequence, out.Policy, now)
	if progress != nil {
		out.Ingest.ProcessedSequence, out.Ingest.ProgressRevision = &progress.ProcessedSequence, &progress.Revision
		signals := &LivenessSignals{LastHeartbeatEventAt: utc(progress.LastHeartbeatEventAt), LastHeartbeatReceivedAt: utc(progress.LastHeartbeatReceivedAt), ClockQuality: progress.ClockQuality, LastEventAt: utc(progress.LastEventAt)}
		if progress.LastHeartbeatReceivedAt != nil {
			age := int64(now.Sub(*progress.LastHeartbeatReceivedAt) / time.Second)
			signals.HeartbeatAgeSeconds = &age
		}
		out.Liveness = signals
		out.Completeness = &Completeness{CompleteSequence: progress.CompleteSequence, FirstQuarantinedSequence: progress.FirstQuarantinedSequence, QuarantinedCount: progress.QuarantinedCount, WatermarkSequence: progress.WatermarkSequence, WatermarkEventAt: utc(progress.WatermarkEventAt)}
	}
}

func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}
