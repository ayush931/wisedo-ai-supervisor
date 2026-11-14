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

// WIS-520 persisted next-shift handovers. An owner or supervisor generates an immutable draft
// of DraftHandover over a stored summary snapshot and the job orders' current heads; any
// active member reads them. Generation writes no shift plan (rule 05). Adoption is a
// separate, explicit owner/supervisor action that submits the draft's proposed plan through
// the shift-plan create (or, with If-Match, revise) path as an unapproved draft revision; the
// adopter is its submitter, so a different supervisor must approve it.
var (
	ErrHandoverUnchanged = errors.New("shift handover is identical to the latest draft")
	ErrHandoverAdopted   = errors.New("shift handover has already been adopted")
	ErrHandoverNoPlan    = errors.New("shift handover proposes no plan to adopt")
	// ErrHandoverSuperseded: only a plan's latest draft is adoptable; an older one was
	// drafted from staler inputs.
	ErrHandoverSuperseded = errors.New("shift handover is not the plan's latest draft")
	// ErrHandoverSourceStale: a handover is drafted from, and adopted only while based on, the
	// plan's latest summary of its currently approved revision. An older summary understates
	// what was produced, so the draft would carry forward quantity already made (WIS-520
	// acceptance 6, source freshness).
	ErrHandoverSourceStale = errors.New("shift handover source summary is no longer current")
)

// summaryCurrent reports whether summaryRevision is the plan's latest summary and was
// computed from the plan's currently approved revision.
func summaryCurrent(ctx context.Context, tx *sql.Tx, sc Scope, plan ShiftPlanV1, summaryRevision, summaryPlanRevision int64) (bool, error) {
	var latest int64
	if e := tx.QueryRowContext(ctx, `SELECT coalesce(max(revision),0) FROM supervisor.shift_summaries WHERE tenant_id=$1 AND site_id=$2 AND plan_id=$3`, sc.TenantID, sc.SiteID, plan.ID).Scan(&latest); e != nil {
		return false, e
	}
	return summaryRevision == latest && plan.ApprovedRevision != nil && *plan.ApprovedRevision == summaryPlanRevision, nil
}

// InstructionUnavailableError names the job order whose instruction failed the delivery
// predicate at adoption; it is ErrInstructionUnavailable.
type InstructionUnavailableError struct{ JobOrderID string }

func (e *InstructionUnavailableError) Error() string {
	return ErrInstructionUnavailable.Error() + ": " + e.JobOrderID
}
func (e *InstructionUnavailableError) Unwrap() error { return ErrInstructionUnavailable }

type HandoverStatus string

const (
	HandoverDraft   HandoverStatus = "draft"
	HandoverAdopted HandoverStatus = "adopted"
)

// ShiftHandoverAdoptInputV1 binds the adopter to the exact draft they reviewed.
type ShiftHandoverAdoptInputV1 struct {
	CanonicalPayloadSHA256 string `json:"canonical_payload_digest" contract:"sha256"`
}

type ShiftHandoverAdoptionV1 struct {
	PlanID       string     `json:"plan_id"`
	PlanRevision int64      `json:"plan_revision"`
	AdoptedBy    ActorRefV1 `json:"adopted_by"`
	AdoptedAt    time.Time  `json:"adopted_at"`
}

type ShiftHandoverSnapshotV1 struct {
	PlanID                 string                   `json:"plan_id"`
	Revision               int64                    `json:"revision"`
	Status                 HandoverStatus           `json:"status"`
	Handover               ShiftHandoverV1          `json:"handover"`
	CanonicalPayloadSHA256 string                   `json:"canonical_payload_digest" contract:"sha256"`
	DigestSchemaVersion    int                      `json:"digest_schema_version"`
	GeneratedBy            ActorRefV1               `json:"generated_by"`
	GeneratedAt            time.Time                `json:"generated_at"`
	Adoption               *ShiftHandoverAdoptionV1 `json:"adoption"`
	Replayed               bool                     `json:"replayed"`
}

type ShiftHandoverHeaderV1 struct {
	Revision               int64          `json:"revision"`
	SummaryRevision        int64          `json:"summary_revision"`
	Status                 HandoverStatus `json:"status"`
	CanonicalPayloadSHA256 string         `json:"canonical_payload_digest" contract:"sha256"`
	GeneratedBy            ActorRefV1     `json:"generated_by"`
	GeneratedAt            time.Time      `json:"generated_at"`
}

type ShiftHandoverPageV1 struct {
	Handovers  []ShiftHandoverHeaderV1 `json:"handovers"`
	NextCursor string                  `json:"next_cursor,omitempty"`
}

type ShiftHandoverService struct{ plans *ShiftPlanService }

func NewShiftHandovers(db *sql.DB) *ShiftHandoverService {
	return &ShiftHandoverService{NewShiftPlans(db)}
}

// WithSessionPolicy returns a copy of the service that enforces p (WIS-290).
func (s *ShiftHandoverService) WithSessionPolicy(p session.Policy) *ShiftHandoverService {
	return &ShiftHandoverService{s.plans.WithSessionPolicy(p)}
}

func handoverAudit(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, planID string, revision int64, adopted, event string) error {
	_, e := tx.ExecContext(ctx, `INSERT INTO supervisor.shift_handover_audit(tenant_id,site_id,actor_issuer,actor_subject,plan_id,revision,adopted_plan_id,event_type) VALUES($1,$2,$3,$4,$5,NULLIF($6,0),NULLIF($7,'')::uuid,$8)`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, planID, revision, adopted, event)
	return e
}

// handoverReceipt returns the draft revision of an earlier identical request, or
// ErrConflict when the key was used for a different request.
func handoverReceipt(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, key, hash string) (string, int64, bool, error) {
	var stored, planID string
	var revision int64
	e := tx.QueryRowContext(ctx, `SELECT request_sha256,plan_id,revision FROM supervisor.shift_handover_receipts WHERE tenant_id=$1 AND site_id=$2 AND actor_issuer=$3 AND actor_subject=$4 AND idempotency_key=$5`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, key).Scan(&stored, &planID, &revision)
	switch {
	case errors.Is(e, sql.ErrNoRows):
		return "", 0, false, nil
	case e != nil:
		return "", 0, false, e
	case stored != hash:
		return "", 0, false, ErrConflict
	}
	return planID, revision, true, handoverAudit(ctx, tx, a, sc, planID, revision, "", "mutation.replayed")
}

func handoverReceiptInsert(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, key, hash, op, planID string, revision int64) error {
	_, e := tx.ExecContext(ctx, `INSERT INTO supervisor.shift_handover_receipts(tenant_id,site_id,actor_issuer,actor_subject,idempotency_key,request_sha256,operation,plan_id,revision) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		sc.TenantID, sc.SiteID, a.Issuer, a.Subject, key, hash, op, planID, revision)
	return e
}

// Generate drafts and stores the next handover revision. It reads the plan, the summary
// snapshot and job orders and writes only handover rows.
func (s *ShiftHandoverService) Generate(ctx context.Context, a Actor, sc Scope, planID string, in ShiftHandoverRequestV1, key string) (ShiftHandoverSnapshotV1, error) {
	switch {
	case !canonicalID(planID):
		return ShiftHandoverSnapshotV1{}, ErrNotFound
	case in.SchemaVersion != 1:
		return ShiftHandoverSnapshotV1{}, &ConfigError{CodeInvalidShiftHandover, "schema_version"}
	case in.SummaryRevision < 1:
		return ShiftHandoverSnapshotV1{}, &ConfigError{CodeInvalidShiftHandover, "summary_revision"}
	case !validShift(in.NextShift.LineID, in.NextShift.StartsAt, in.NextShift.EndsAt, in.NextShift.Timezone):
		return ShiftHandoverSnapshotV1{}, &ConfigError{CodeInvalidShiftHandover, "next_shift"}
	case !validText(key, 128):
		return ShiftHandoverSnapshotV1{}, &ConfigError{CodeInvalidShiftHandover, ""}
	}
	hash := requestDigest([]any{"shift_handover.generate", planID, in})
	tx, _, _, err := s.plans.begin(ctx, a, sc, shiftWrite)
	if err != nil {
		return ShiftHandoverSnapshotV1{}, err
	}
	defer tx.Rollback()
	if stored, revision, replayed, err := handoverReceipt(ctx, tx, a, sc, key, hash); err != nil || replayed {
		if err != nil {
			return ShiftHandoverSnapshotV1{}, err
		}
		out, err := loadHandover(ctx, tx, sc, stored, revision)
		if err != nil {
			return out, err
		}
		out.Replayed = true
		return out, s.plans.finish(ctx, tx, a)
	}
	plan, err := loadShiftPlan(ctx, tx, sc, planID)
	if err != nil {
		return ShiftHandoverSnapshotV1{}, err
	}
	revision, err := appendHandover(ctx, tx, a, sc, plan, in.SummaryRevision, in.NextShift)
	if err != nil {
		return ShiftHandoverSnapshotV1{}, err
	}
	if e := handoverReceiptInsert(ctx, tx, a, sc, key, hash, "generate", planID, revision); e != nil {
		return ShiftHandoverSnapshotV1{}, e
	}
	if e := handoverAudit(ctx, tx, a, sc, planID, revision, "", "shift_handover.generated"); e != nil {
		return ShiftHandoverSnapshotV1{}, e
	}
	out, err := loadHandover(ctx, tx, sc, planID, revision)
	if err != nil {
		return out, err
	}
	return out, s.plans.finish(ctx, tx, a)
}

// appendHandover drafts the plan's next handover from its summary snapshot summaryRevision
// and the job orders' current heads, appends it inside the caller's write transaction
// attributed to a, and returns its revision. It writes only handover rows (rule 05).
func appendHandover(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, plan ShiftPlanV1, summaryRevision int64, next NextShiftV1) (int64, error) {
	summary, err := loadSummary(ctx, tx, sc, plan.ID, summaryRevision)
	if errors.Is(err, ErrNotFound) {
		return 0, &ConfigError{CodeInvalidShiftHandover, "summary_revision"}
	}
	if err != nil {
		return 0, err
	}
	if summary.Summary.PlanRevision < 1 || summary.Summary.PlanRevision > int64(len(plan.Revisions)) {
		return 0, errIntegrity
	}
	if current, err := summaryCurrent(ctx, tx, sc, plan, summary.Revision, summary.Summary.PlanRevision); err != nil {
		return 0, err
	} else if !current {
		return 0, ErrHandoverSourceStale
	}
	approved := plan.Revisions[summary.Summary.PlanRevision-1]
	orders := map[string]HandoverJobOrderV1{}
	for _, x := range approved.Plan.Assignments {
		order, err := loadJobOrder(ctx, tx, sc, x.JobOrderID)
		if err != nil {
			return 0, err
		}
		head := order.Revisions[order.CurrentRevision-1]
		reason, err := instructionReason(ctx, tx, sc, orderInstruction(head.Order))
		if err != nil {
			return 0, err
		}
		orders[x.JobOrderID] = HandoverJobOrderV1{Current: head, InstructionReason: reason}
	}
	handover, encoded, digest, err := DraftHandover(sc, ShiftHandoverInput{SummaryRevision: summary.Revision, SummarySHA256: summary.CanonicalPayloadSHA256, Summary: summary.Summary,
		Approved: approved, JobOrders: orders, Next: next})
	if err != nil {
		return 0, err
	}
	var revision int64
	var latest string
	if e := tx.QueryRowContext(ctx, `SELECT coalesce(max(revision),0),coalesce((SELECT canonical_payload_sha256 FROM supervisor.shift_handovers WHERE tenant_id=$1 AND site_id=$2 AND plan_id=$3 ORDER BY revision DESC LIMIT 1),'') FROM supervisor.shift_handovers WHERE tenant_id=$1 AND site_id=$2 AND plan_id=$3`,
		sc.TenantID, sc.SiteID, plan.ID).Scan(&revision, &latest); e != nil {
		return 0, e
	}
	if latest == digest {
		return 0, ErrHandoverUnchanged
	}
	payload, err := json.Marshal(handover)
	if err != nil {
		return 0, err
	}
	revision++
	if _, e := tx.ExecContext(ctx, `INSERT INTO supervisor.shift_handovers(tenant_id,site_id,plan_id,revision,summary_revision,payload,canonical_payload,canonical_payload_sha256,digest_schema_version,actor_issuer,actor_subject) VALUES($1,$2,$3,$4,$5,$6::jsonb,$7,$8,$9,$10,$11)`,
		sc.TenantID, sc.SiteID, plan.ID, revision, summary.Revision, string(payload), string(encoded), digest, shiftHandoverDigestV1, a.Issuer, a.Subject); e != nil {
		return 0, e
	}
	return revision, nil
}

// Adopt submits the draft's proposed plan as an unapproved shift plan revision by a: a new
// plan when expected is 0, otherwise a revision of the plan with exactly the proposed line
// and shift whose state revision is still expected. An overlapping plan without If-Match is
// SHIFT_PLAN_EXISTS; nothing is overwritten and nothing is approved here.
func (s *ShiftHandoverService) Adopt(ctx context.Context, a Actor, sc Scope, planID string, revision int64, in ShiftHandoverAdoptInputV1, key string, expected int64) (ShiftPlanV1, error) {
	switch {
	case !canonicalID(planID) || revision < 1:
		return ShiftPlanV1{}, ErrNotFound
	case !digest64.MatchString(in.CanonicalPayloadSHA256):
		return ShiftPlanV1{}, &ConfigError{CodeInvalidShiftHandover, "canonical_payload_digest"}
	case !validText(key, 128) || expected < 0:
		return ShiftPlanV1{}, &ConfigError{CodeInvalidShiftHandover, ""}
	}
	hash := requestDigest([]any{"shift_handover.adopt", planID, revision, expected, in})
	tx, _, _, err := s.plans.begin(ctx, a, sc, shiftWrite)
	if err != nil {
		return ShiftPlanV1{}, err
	}
	defer tx.Rollback()
	if stored, storedRevision, replayed, err := handoverReceipt(ctx, tx, a, sc, key, hash); err != nil || replayed {
		if err != nil {
			return ShiftPlanV1{}, err
		}
		h, err := loadHandover(ctx, tx, sc, stored, storedRevision)
		if err != nil {
			return ShiftPlanV1{}, err
		}
		if h.Adoption == nil {
			return ShiftPlanV1{}, errIntegrity
		}
		out, err := loadShiftPlan(ctx, tx, sc, h.Adoption.PlanID)
		if err != nil {
			return out, err
		}
		out.Replayed = true
		return out, s.plans.finish(ctx, tx, a)
	}
	h, err := loadHandover(ctx, tx, sc, planID, revision)
	if err != nil {
		return ShiftPlanV1{}, err
	}
	switch {
	case h.CanonicalPayloadSHA256 != in.CanonicalPayloadSHA256:
		return ShiftPlanV1{}, ErrDigestMismatch
	case h.Adoption != nil:
		return ShiftPlanV1{}, ErrHandoverAdopted
	case h.Handover.ProposedPlan == nil:
		return ShiftPlanV1{}, ErrHandoverNoPlan
	case h.Handover.ProposedPlan.Validate() != nil:
		return ShiftPlanV1{}, errIntegrity
	}
	var latest int64
	if e := tx.QueryRowContext(ctx, `SELECT max(revision) FROM supervisor.shift_handovers WHERE tenant_id=$1 AND site_id=$2 AND plan_id=$3`, sc.TenantID, sc.SiteID, planID).Scan(&latest); e != nil {
		return ShiftPlanV1{}, e
	}
	if revision != latest {
		return ShiftPlanV1{}, ErrHandoverSuperseded
	}
	source, err := loadShiftPlan(ctx, tx, sc, planID)
	if err != nil {
		return ShiftPlanV1{}, err
	}
	if current, err := summaryCurrent(ctx, tx, sc, source, h.Handover.SummaryRevision, h.Handover.PlanRevision); err != nil {
		return ShiftPlanV1{}, err
	} else if !current {
		return ShiftPlanV1{}, ErrHandoverSourceStale
	}
	// The instruction predicate is re-applied now: a draft whose instruction was withdrawn,
	// revised or revoked since generation is not submitted.
	for _, x := range h.Handover.ProposedPlan.Assignments {
		reason, err := instructionReason(ctx, tx, sc, InstructionRefV1{DocumentID: x.InstructionDocumentID, Revision: x.InstructionRevision, SHA256: x.InstructionSHA256})
		if err != nil {
			return ShiftPlanV1{}, err
		}
		if reason != "" {
			return ShiftPlanV1{}, &InstructionUnavailableError{x.JobOrderID}
		}
	}
	p, encoded, digest, err := normalized(sc.TenantID, sc.SiteID, *h.Handover.ProposedPlan)
	if err != nil {
		return ShiftPlanV1{}, errIntegrity
	}
	adopted, planRevision, event := "", int64(1), "plan.created"
	if expected == 0 {
		if adopted, err = createPlan(ctx, tx, a, sc, p, encoded, digest); err != nil {
			return ShiftPlanV1{}, err
		}
	} else {
		e := tx.QueryRowContext(ctx, `SELECT id FROM supervisor.shift_plan_heads WHERE tenant_id=$1 AND site_id=$2 AND line_id=$3 AND starts_at=$4::timestamptz AND ends_at=$5::timestamptz`, sc.TenantID, sc.SiteID, p.LineID, p.StartsAt, p.EndsAt).Scan(&adopted)
		if errors.Is(e, sql.ErrNoRows) {
			return ShiftPlanV1{}, ErrRevision
		}
		if e != nil {
			return ShiftPlanV1{}, e
		}
		if planRevision, err = revisePlan(ctx, tx, a, sc, adopted, p, encoded, digest, expected); err != nil {
			return ShiftPlanV1{}, err
		}
		event = "plan.revised"
	}
	if e := shiftAudit(ctx, tx, a, sc, adopted, planRevision, event); e != nil {
		return ShiftPlanV1{}, e
	}
	if _, e := tx.ExecContext(ctx, `INSERT INTO supervisor.shift_handover_adoptions(tenant_id,site_id,plan_id,revision,adopted_plan_id,adopted_revision,actor_issuer,actor_subject) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`,
		sc.TenantID, sc.SiteID, planID, revision, adopted, planRevision, a.Issuer, a.Subject); e != nil {
		return ShiftPlanV1{}, e
	}
	if e := handoverReceiptInsert(ctx, tx, a, sc, key, hash, "adopt", planID, revision); e != nil {
		return ShiftPlanV1{}, e
	}
	if e := handoverAudit(ctx, tx, a, sc, planID, revision, adopted, "shift_handover.adopted"); e != nil {
		return ShiftPlanV1{}, e
	}
	out, err := loadShiftPlan(ctx, tx, sc, adopted)
	if err != nil {
		return out, err
	}
	return out, s.plans.finish(ctx, tx, a)
}

// Get returns one draft, re-verified against its digest.
func (s *ShiftHandoverService) Get(ctx context.Context, a Actor, sc Scope, planID string, revision int64) (ShiftHandoverSnapshotV1, error) {
	if !canonicalID(planID) || revision < 1 {
		return ShiftHandoverSnapshotV1{}, ErrNotFound
	}
	tx, _, _, err := s.plans.begin(ctx, a, sc, shiftRead)
	if err != nil {
		return ShiftHandoverSnapshotV1{}, err
	}
	defer tx.Rollback()
	out, err := loadHandover(ctx, tx, sc, planID, revision)
	if err != nil {
		return out, err
	}
	if err = handoverAudit(ctx, tx, a, sc, planID, revision, "", "shift_handover.read"); err != nil {
		return out, err
	}
	return out, s.plans.finish(ctx, tx, a)
}

// List pages a plan's drafts by revision; the cursor is the last revision returned.
func (s *ShiftHandoverService) List(ctx context.Context, a Actor, sc Scope, planID string, limit int, cursor string) (ShiftHandoverPageV1, error) {
	out := ShiftHandoverPageV1{Handovers: []ShiftHandoverHeaderV1{}}
	if !canonicalID(planID) {
		return out, ErrNotFound
	}
	var after int64
	if cursor != "" {
		var err error
		if after, err = strconv.ParseInt(cursor, 10, 64); err != nil || after < 1 || strconv.FormatInt(after, 10) != cursor {
			return out, &ConfigError{CodeInvalidShiftHandover, "cursor"}
		}
	}
	if limit < 1 || limit > 100 {
		return out, &ConfigError{CodeInvalidShiftHandover, "limit"}
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
	rows, err := tx.QueryContext(ctx, `SELECT h.revision,h.summary_revision,(d.plan_id IS NOT NULL),h.canonical_payload_sha256,h.actor_issuer,h.actor_subject,h.generated_at FROM supervisor.shift_handovers h
 LEFT JOIN supervisor.shift_handover_adoptions d ON (d.tenant_id,d.site_id,d.plan_id,d.revision)=(h.tenant_id,h.site_id,h.plan_id,h.revision)
 WHERE h.tenant_id=$1 AND h.site_id=$2 AND h.plan_id=$3 AND h.revision>$4 ORDER BY h.revision LIMIT $5`, sc.TenantID, sc.SiteID, planID, after, limit+1)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var h ShiftHandoverHeaderV1
		var adopted bool
		if err = rows.Scan(&h.Revision, &h.SummaryRevision, &adopted, &h.CanonicalPayloadSHA256, &h.GeneratedBy.Issuer, &h.GeneratedBy.Subject, &h.GeneratedAt); err != nil {
			return out, err
		}
		h.GeneratedAt, h.Status = h.GeneratedAt.UTC(), HandoverDraft
		if adopted {
			h.Status = HandoverAdopted
		}
		out.Handovers = append(out.Handovers, h)
	}
	if err = rows.Err(); err != nil {
		return out, err
	}
	if len(out.Handovers) > limit {
		out.Handovers = out.Handovers[:limit]
		out.NextCursor = strconv.FormatInt(out.Handovers[limit-1].Revision, 10)
	}
	if err = handoverAudit(ctx, tx, a, sc, planID, 0, "", "shift_handover.listed"); err != nil {
		return out, err
	}
	return out, s.plans.finish(ctx, tx, a)
}

func loadHandover(ctx context.Context, tx *sql.Tx, sc Scope, planID string, revision int64) (ShiftHandoverSnapshotV1, error) {
	out := ShiftHandoverSnapshotV1{PlanID: planID, Status: HandoverDraft}
	var payload []byte
	var adopted sql.NullString
	var adoptedRevision sql.NullInt64
	var issuer, subject sql.NullString
	var at sql.NullTime
	e := tx.QueryRowContext(ctx, `SELECT h.revision,h.payload,h.canonical_payload_sha256,h.digest_schema_version,h.actor_issuer,h.actor_subject,h.generated_at,d.adopted_plan_id,d.adopted_revision,d.actor_issuer,d.actor_subject,d.adopted_at
 FROM supervisor.shift_handovers h LEFT JOIN supervisor.shift_handover_adoptions d ON (d.tenant_id,d.site_id,d.plan_id,d.revision)=(h.tenant_id,h.site_id,h.plan_id,h.revision)
 WHERE h.tenant_id=$1 AND h.site_id=$2 AND h.plan_id=$3 AND h.revision=$4`,
		sc.TenantID, sc.SiteID, planID, revision).Scan(&out.Revision, &payload, &out.CanonicalPayloadSHA256, &out.DigestSchemaVersion, &out.GeneratedBy.Issuer, &out.GeneratedBy.Subject, &out.GeneratedAt,
		&adopted, &adoptedRevision, &issuer, &subject, &at)
	if errors.Is(e, sql.ErrNoRows) {
		return out, ErrNotFound
	}
	if e != nil {
		return out, e
	}
	// Stored bodies are re-verified against their digest on every read.
	if json.Unmarshal(payload, &out.Handover) != nil || out.Handover.PlanID != planID {
		return out, errIntegrity
	}
	if _, recomputed, err := canonical.Envelope(shiftHandoverSchema, shiftHandoverDigestV1, sc.TenantID, sc.SiteID, out.Handover); err != nil || recomputed != out.CanonicalPayloadSHA256 {
		return out, errIntegrity
	}
	out.GeneratedAt = out.GeneratedAt.UTC()
	if adopted.Valid {
		out.Status = HandoverAdopted
		out.Adoption = &ShiftHandoverAdoptionV1{PlanID: adopted.String, PlanRevision: adoptedRevision.Int64, AdoptedBy: ActorRefV1{issuer.String, subject.String}, AdoptedAt: at.Time.UTC()}
	}
	return out, nil
}
