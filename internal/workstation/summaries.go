package workstation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/canonical"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/session"
)

// WIS-520 persisted shift summaries. An owner or supervisor generates a snapshot of
// SummarizeShift over the plan's approved revision and every station record bound to the
// plan; any active member reads them. Snapshots are immutable revisions: later data appends
// a revision, and regenerating identical inputs is refused as unchanged.
var (
	ErrPlanNotApproved  = errors.New("shift plan has no approved revision to summarize")
	ErrSummaryUnchanged = errors.New("shift summary is identical to the latest snapshot")
)

const CodeInvalidShiftSummary = "INVALID_SHIFT_SUMMARY_REQUEST"

// ShiftSummaryRequestV1 asks for a snapshot; the plan comes from the path, and tenant, site
// and actor from the session.
type ShiftSummaryRequestV1 struct {
	SchemaVersion int `json:"schema_version"`
}

type ShiftSummarySnapshotV1 struct {
	PlanID                 string         `json:"plan_id"`
	Revision               int64          `json:"revision"`
	Summary                ShiftSummaryV1 `json:"summary"`
	CanonicalPayloadSHA256 string         `json:"canonical_payload_digest" contract:"sha256"`
	DigestSchemaVersion    int            `json:"digest_schema_version"`
	GeneratedBy            ActorRefV1     `json:"generated_by"`
	GeneratedAt            time.Time      `json:"generated_at"`
	Replayed               bool           `json:"replayed"`
}

type ShiftSummaryHeaderV1 struct {
	Revision               int64            `json:"revision"`
	PlanRevision           int64            `json:"plan_revision"`
	Readiness              SummaryReadiness `json:"readiness"`
	CanonicalPayloadSHA256 string           `json:"canonical_payload_digest" contract:"sha256"`
	GeneratedBy            ActorRefV1       `json:"generated_by"`
	GeneratedAt            time.Time        `json:"generated_at"`
}

type ShiftSummaryPageV1 struct {
	Summaries  []ShiftSummaryHeaderV1 `json:"summaries"`
	NextCursor string                 `json:"next_cursor,omitempty"`
}

type ShiftSummaryService struct{ plans *ShiftPlanService }

func NewShiftSummaries(db *sql.DB) *ShiftSummaryService {
	return &ShiftSummaryService{NewShiftPlans(db)}
}

// WithSessionPolicy returns a copy of the service that enforces p (WIS-290).
func (s *ShiftSummaryService) WithSessionPolicy(p session.Policy) *ShiftSummaryService {
	return &ShiftSummaryService{s.plans.WithSessionPolicy(p)}
}

func summaryAudit(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, planID string, revision int64, event string) error {
	_, e := tx.ExecContext(ctx, `INSERT INTO supervisor.shift_summary_audit(tenant_id,site_id,actor_issuer,actor_subject,plan_id,revision,event_type) VALUES($1,$2,$3,$4,$5,NULLIF($6,0),$7)`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, planID, revision, event)
	return e
}

// Generate computes and stores the next snapshot. The site write lock taken by begin
// serializes generations, so the next revision number is uncontended.
// ponytail: every record of the plan is loaded into memory; page through records if a
// shift ever carries more than a few thousand.
func (s *ShiftSummaryService) Generate(ctx context.Context, a Actor, sc Scope, planID string, in ShiftSummaryRequestV1, key string) (ShiftSummarySnapshotV1, error) {
	switch {
	case !canonicalID(planID):
		return ShiftSummarySnapshotV1{}, ErrNotFound
	case in.SchemaVersion != 1:
		return ShiftSummarySnapshotV1{}, &ConfigError{CodeInvalidShiftSummary, "schema_version"}
	case !validText(key, 128):
		return ShiftSummarySnapshotV1{}, &ConfigError{CodeInvalidShiftSummary, ""}
	}
	hash := requestDigest([]any{"shift_summary.generate", planID, in})
	tx, _, now, err := s.plans.begin(ctx, a, sc, shiftWrite)
	if err != nil {
		return ShiftSummarySnapshotV1{}, err
	}
	defer tx.Rollback()
	var stored string
	var revision int64
	e := tx.QueryRowContext(ctx, `SELECT request_sha256,revision FROM supervisor.shift_summary_receipts WHERE tenant_id=$1 AND site_id=$2 AND actor_issuer=$3 AND actor_subject=$4 AND idempotency_key=$5`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, key).Scan(&stored, &revision)
	switch {
	case e == nil && stored != hash:
		return ShiftSummarySnapshotV1{}, ErrConflict
	case e == nil:
		if err = summaryAudit(ctx, tx, a, sc, planID, revision, "mutation.replayed"); err != nil {
			return ShiftSummarySnapshotV1{}, err
		}
		out, err := loadSummary(ctx, tx, sc, planID, revision)
		if err != nil {
			return out, err
		}
		out.Replayed = true
		return out, s.plans.finish(ctx, tx, a)
	case !errors.Is(e, sql.ErrNoRows):
		return ShiftSummarySnapshotV1{}, e
	}
	plan, err := loadShiftPlan(ctx, tx, sc, planID)
	if err != nil {
		return ShiftSummarySnapshotV1{}, err
	}
	// A manual regeneration after the run ended describes the shift as ended then, as End does.
	run, err := loadRun(ctx, tx, sc, planID)
	if err != nil {
		return ShiftSummarySnapshotV1{}, err
	}
	if revision, err = appendSummary(ctx, tx, a, sc, plan, now, run.endedAt()); err != nil {
		return ShiftSummarySnapshotV1{}, err
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO supervisor.shift_summary_receipts(tenant_id,site_id,actor_issuer,actor_subject,idempotency_key,request_sha256,plan_id,revision) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`,
		sc.TenantID, sc.SiteID, a.Issuer, a.Subject, key, hash, planID, revision); e != nil {
		return ShiftSummarySnapshotV1{}, e
	}
	if e = summaryAudit(ctx, tx, a, sc, planID, revision, "shift_summary.generated"); e != nil {
		return ShiftSummarySnapshotV1{}, e
	}
	out, err := loadSummary(ctx, tx, sc, planID, revision)
	if err != nil {
		return out, err
	}
	return out, s.plans.finish(ctx, tx, a)
}

// appendSummary computes the plan's next snapshot over its approved revision and every record
// bound to it, appends it inside the caller's write transaction attributed to a, and returns
// its revision. endedAt is the shift run's end, when it has ended.
func appendSummary(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, plan ShiftPlanV1, now time.Time, endedAt *time.Time) (int64, error) {
	planID := plan.ID
	if plan.ApprovedRevision == nil {
		return 0, ErrPlanNotApproved
	}
	rows, err := tx.QueryContext(ctx, recordScope+` AND r.plan_id=$3 ORDER BY r.received_at,r.id`, sc.TenantID, sc.SiteID, planID)
	if err != nil {
		return 0, err
	}
	records := []StationRecordV1{}
	for rows.Next() {
		r, err := scanRecord(rows, sc)
		if err != nil {
			rows.Close()
			return 0, err
		}
		records = append(records, r)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return 0, err
	}
	summary, encoded, digest, err := SummarizeShift(sc, ShiftSummaryInput{PlanID: plan.ID, LineID: plan.LineID, StartsAt: plan.StartsAt, EndsAt: plan.EndsAt,
		AsOf: now, EndedAt: endedAt, Approved: plan.Revisions[*plan.ApprovedRevision-1], Records: records})
	if err != nil {
		return 0, err
	}
	var revision int64
	var latest string
	if e := tx.QueryRowContext(ctx, `SELECT coalesce(max(revision),0),coalesce((SELECT canonical_payload_sha256 FROM supervisor.shift_summaries WHERE tenant_id=$1 AND site_id=$2 AND plan_id=$3 ORDER BY revision DESC LIMIT 1),'') FROM supervisor.shift_summaries WHERE tenant_id=$1 AND site_id=$2 AND plan_id=$3`,
		sc.TenantID, sc.SiteID, planID).Scan(&revision, &latest); e != nil {
		return 0, e
	}
	if latest == digest {
		return 0, ErrSummaryUnchanged
	}
	payload, err := json.Marshal(summary)
	if err != nil {
		return 0, err
	}
	revision++
	if _, e := tx.ExecContext(ctx, `INSERT INTO supervisor.shift_summaries(tenant_id,site_id,plan_id,revision,plan_revision,payload,canonical_payload,canonical_payload_sha256,digest_schema_version,actor_issuer,actor_subject) VALUES($1,$2,$3,$4,$5,$6::jsonb,$7,$8,$9,$10,$11)`,
		sc.TenantID, sc.SiteID, planID, revision, summary.PlanRevision, string(payload), string(encoded), digest, shiftSummaryDigestV1, a.Issuer, a.Subject); e != nil {
		return 0, e
	}
	return revision, nil
}

// Get returns one snapshot, re-verified against its digest.
func (s *ShiftSummaryService) Get(ctx context.Context, a Actor, sc Scope, planID string, revision int64) (ShiftSummarySnapshotV1, error) {
	if !canonicalID(planID) || revision < 1 {
		return ShiftSummarySnapshotV1{}, ErrNotFound
	}
	tx, _, _, err := s.plans.begin(ctx, a, sc, shiftRead)
	if err != nil {
		return ShiftSummarySnapshotV1{}, err
	}
	defer tx.Rollback()
	out, err := loadSummary(ctx, tx, sc, planID, revision)
	if err != nil {
		return out, err
	}
	if err = summaryAudit(ctx, tx, a, sc, planID, revision, "shift_summary.read"); err != nil {
		return out, err
	}
	return out, s.plans.finish(ctx, tx, a)
}

// List pages a plan's snapshots by revision; the cursor is the last revision returned.
func (s *ShiftSummaryService) List(ctx context.Context, a Actor, sc Scope, planID string, limit int, cursor string) (ShiftSummaryPageV1, error) {
	out := ShiftSummaryPageV1{Summaries: []ShiftSummaryHeaderV1{}}
	if !canonicalID(planID) {
		return out, ErrNotFound
	}
	var after int64
	if cursor != "" {
		var err error
		if after, err = strconv.ParseInt(cursor, 10, 64); err != nil || after < 1 || strconv.FormatInt(after, 10) != cursor {
			return out, &ConfigError{CodeInvalidShiftSummary, "cursor"}
		}
	}
	if limit < 1 || limit > 100 {
		return out, &ConfigError{CodeInvalidShiftSummary, "limit"}
	}
	tx, _, _, err := s.plans.begin(ctx, a, sc, shiftRead)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	var exists bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM supervisor.shift_plan_heads WHERE tenant_id=$1 AND site_id=$2 AND id=$3)`, sc.TenantID, sc.SiteID, planID).Scan(&exists); err != nil {
		return out, err
	}
	if !exists {
		return out, ErrNotFound
	}
	rows, err := tx.QueryContext(ctx, `SELECT revision,plan_revision,payload->>'readiness',canonical_payload_sha256,actor_issuer,actor_subject,generated_at FROM supervisor.shift_summaries WHERE tenant_id=$1 AND site_id=$2 AND plan_id=$3 AND revision>$4 ORDER BY revision LIMIT $5`,
		sc.TenantID, sc.SiteID, planID, after, limit+1)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var h ShiftSummaryHeaderV1
		if err = rows.Scan(&h.Revision, &h.PlanRevision, &h.Readiness, &h.CanonicalPayloadSHA256, &h.GeneratedBy.Issuer, &h.GeneratedBy.Subject, &h.GeneratedAt); err != nil {
			return out, err
		}
		h.GeneratedAt = h.GeneratedAt.UTC()
		out.Summaries = append(out.Summaries, h)
	}
	if err = rows.Err(); err != nil {
		return out, err
	}
	if len(out.Summaries) > limit {
		out.Summaries = out.Summaries[:limit]
		out.NextCursor = strconv.FormatInt(out.Summaries[limit-1].Revision, 10)
	}
	if err = summaryAudit(ctx, tx, a, sc, planID, 0, "shift_summary.listed"); err != nil {
		return out, err
	}
	return out, s.plans.finish(ctx, tx, a)
}

func loadSummary(ctx context.Context, tx *sql.Tx, sc Scope, planID string, revision int64) (ShiftSummarySnapshotV1, error) {
	out := ShiftSummarySnapshotV1{PlanID: planID}
	var payload []byte
	e := tx.QueryRowContext(ctx, `SELECT revision,payload,canonical_payload_sha256,digest_schema_version,actor_issuer,actor_subject,generated_at FROM supervisor.shift_summaries WHERE tenant_id=$1 AND site_id=$2 AND plan_id=$3 AND revision=$4`,
		sc.TenantID, sc.SiteID, planID, revision).Scan(&out.Revision, &payload, &out.CanonicalPayloadSHA256, &out.DigestSchemaVersion, &out.GeneratedBy.Issuer, &out.GeneratedBy.Subject, &out.GeneratedAt)
	if errors.Is(e, sql.ErrNoRows) {
		return out, ErrNotFound
	}
	if e != nil {
		return out, e
	}
	// Stored bodies are re-verified against their digest on every read.
	if json.Unmarshal(payload, &out.Summary) != nil || out.Summary.PlanID != planID {
		return out, errIntegrity
	}
	if _, recomputed, err := canonical.Envelope(shiftSummarySchema, shiftSummaryDigestV1, sc.TenantID, sc.SiteID, out.Summary); err != nil || recomputed != out.CanonicalPayloadSHA256 {
		return out, errIntegrity
	}
	out.GeneratedAt = out.GeneratedAt.UTC()
	return out, nil
}
