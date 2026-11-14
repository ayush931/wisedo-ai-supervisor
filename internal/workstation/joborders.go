package workstation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/canonical"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/limits"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/session"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/strictjson"
	"github.com/google/uuid"
)

// WIS-520 digital job orders: appended as immutable JobOrderBodyV1 revisions pinned to an
// approved document instruction. A job order has no approval of its own; it reaches a
// workstation only through an approved shift plan, and a pin that is no longer the head
// revision is reported as needs_replan, never delivered.
var (
	ErrInvalidJobOrder        = errors.New("invalid job order")
	ErrInstructionNotApproved = errors.New("instruction is not a current approved document revision")
	ErrJobOrderUnchanged      = errors.New("job order revision is identical to the current revision")
	ErrJobOrderUnknown        = errors.New("shift plan pins an unknown job order revision")
	ErrJobOrderNotCurrent     = errors.New("shift plan pins a superseded job order revision")
	ErrJobOrderMismatch       = errors.New("shift plan assignment does not match its job order revision")
	ErrNoCurrentJobOrder      = errors.New("no approved shift plan assigns this workstation at this time")
	ErrAssignmentConflict     = errors.New("workstation is assigned by more than one approved shift plan")
)

const (
	jobOrderSchema        = "JobOrderBodyV1"
	jobOrderDigestVersion = 1
)

// JobOrderBodyV1 is the caller-supplied job order. Priority (1 highest, 9 lowest) and due
// time are optional; the instruction is an exact document evidence revision and hash.
type JobOrderBodyV1 struct {
	SchemaVersion          int       `json:"schema_version"`
	OrderID                string    `json:"order_id"`
	OperationID            string    `json:"operation_id"`
	ItemCode               string    `json:"item_code"`
	TargetQuantity         int64     `json:"target_quantity"`
	TargetCycleTimeSeconds string    `json:"target_cycle_time_seconds,omitempty"`
	Priority               int       `json:"priority,omitempty"`
	DueAt                  string    `json:"due_at,omitempty"`
	InstructionDocumentID  string    `json:"instruction_document_id"`
	InstructionRevision    int64     `json:"instruction_revision"`
	InstructionSHA256      string    `json:"instruction_sha256"`
	Template               string    `json:"template"`
	Fields                 []FieldV1 `json:"fields"`
	// Omitted fields preserve the bytes and digest of every pre-routing body.
	RoutingID  string `json:"routing_id,omitempty" contract:"uuid"`
	StepNumber *int   `json:"step_number,omitempty"`
	ItemID     string `json:"item_id,omitempty" contract:"uuid"`
}

// A supplied link must contain all three non-null values. A zero/empty/null field
// is not the same as omitting the optional link from a legacy body.
func (o *JobOrderBodyV1) UnmarshalJSON(data []byte) error {
	type body JobOrderBodyV1
	var decoded body
	if err := strictjson.Decode(data, &decoded, limits.RequestBodyBytes); err != nil {
		return ErrInvalidJobOrder
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return ErrInvalidJobOrder
	}
	_, routing := fields["routing_id"]
	_, step := fields["step_number"]
	_, item := fields["item_id"]
	if routing != step || routing != item || routing && (decoded.RoutingID == "" || decoded.StepNumber == nil || decoded.ItemID == "") {
		return ErrInvalidJobOrder
	}
	*o = JobOrderBodyV1(decoded)
	return nil
}

func (o JobOrderBodyV1) Validate() error {
	if (o.RoutingID != "") != (o.StepNumber != nil) || (o.RoutingID != "") != (o.ItemID != "") || o.RoutingID != "" && (!canonicalID(o.RoutingID) || !canonicalID(o.ItemID) || *o.StepNumber < 1) {
		return ErrInvalidJobOrder
	}
	if o.SchemaVersion != 1 || !canonicalID(o.OrderID) || !canonicalID(o.OperationID) || !validText(o.ItemCode, 128) || o.TargetQuantity < 1 ||
		!canonicalID(o.InstructionDocumentID) || o.InstructionRevision < 1 || !digest64.MatchString(o.InstructionSHA256) ||
		!supportedTemplate(o.Template) || o.Priority < 0 || o.Priority > 9 {
		return ErrInvalidJobOrder
	}
	if o.TargetCycleTimeSeconds != "" && (!validDecimal6(o.TargetCycleTimeSeconds) || o.TargetCycleTimeSeconds == "0" || len(o.TargetCycleTimeSeconds) > 32) {
		return ErrInvalidJobOrder
	}
	if _, ok := validTime(o.DueAt); o.DueAt != "" && !ok {
		return ErrInvalidJobOrder
	}
	if validateFields(o.Fields) != nil {
		return ErrInvalidJobOrder
	}
	return nil
}

type JobOrderState string

const (
	JobOrderCurrent    JobOrderState = "current"
	JobOrderSuperseded JobOrderState = "superseded"
)

type JobOrderRevisionV1 struct {
	Revision               int64          `json:"revision"`
	State                  JobOrderState  `json:"state"`
	Order                  JobOrderBodyV1 `json:"order"`
	CanonicalPayloadSHA256 string         `json:"canonical_payload_digest" contract:"sha256"`
	DigestSchemaVersion    int            `json:"digest_schema_version"`
	SubmittedBy            ActorRefV1     `json:"submitted_by"`
	CreatedAt              time.Time      `json:"created_at"`
}

// JobOrderV1 is a job order with its ordered revision history. CurrentRevision is the
// If-Match value for revise.
type JobOrderV1 struct {
	ID              string               `json:"id"`
	CurrentRevision int64                `json:"current_revision"`
	Revisions       []JobOrderRevisionV1 `json:"revisions"`
	Replayed        bool                 `json:"replayed"`
}

type JobOrderSummaryV1 struct {
	ID              string          `json:"id"`
	CurrentRevision int64           `json:"current_revision"`
	CurrentOrder    *JobOrderBodyV1 `json:"current_order,omitempty"`
}

type JobOrderPageV1 struct {
	JobOrders  []JobOrderSummaryV1 `json:"job_orders"`
	NextCursor string              `json:"next_cursor,omitempty"`
}

type DeliveryState string

const (
	DeliveryCurrent                DeliveryState = "current"
	DeliveryNeedsReplan            DeliveryState = "needs_replan"
	DeliveryInstructionUnavailable DeliveryState = "instruction_unavailable"
)

// InstructionReason says why a pinned instruction can no longer be delivered (rule 06).
type InstructionReason string

const (
	InstructionRevoked      InstructionReason = "INSTRUCTION_REVOKED"
	InstructionSuperseded   InstructionReason = "INSTRUCTION_SUPERSEDED"
	InstructionNotConfirmed InstructionReason = "INSTRUCTION_NOT_CONFIRMED"
)

// CurrentJobOrderV1 is what a workstation shows at an instant. Order is the pinned revision
// only while it is still the job order's head revision and its instruction is still a
// confirmed-current document revision. Otherwise State is needs_replan or
// instruction_unavailable (with InstructionReason) and Order is null, so a superseded order
// or unapproved instruction is never presented as current.
type CurrentJobOrderV1 struct {
	State                  DeliveryState       `json:"state"`
	InstructionReason      InstructionReason   `json:"instruction_reason,omitempty"`
	WorkstationID          string              `json:"workstation_id"`
	PlanID                 string              `json:"plan_id"`
	PlanRevision           int64               `json:"plan_revision"`
	LineID                 string              `json:"line_id"`
	StartsAt               time.Time           `json:"starts_at"`
	EndsAt                 time.Time           `json:"ends_at"`
	Timezone               string              `json:"timezone"`
	JobOrderID             string              `json:"job_order_id"`
	PinnedRevision         int64               `json:"pinned_revision"`
	CurrentRevision        int64               `json:"current_revision"`
	TargetQuantity         int64               `json:"target_quantity"`
	TargetCycleTimeSeconds string              `json:"target_cycle_time_seconds,omitempty"`
	Order                  *JobOrderRevisionV1 `json:"order"`
}

type JobOrderService struct{ plans *ShiftPlanService }

func NewJobOrders(db *sql.DB) *JobOrderService { return &JobOrderService{NewShiftPlans(db)} }

// WithSessionPolicy returns a copy of the service that enforces p (WIS-290).
func (s *JobOrderService) WithSessionPolicy(p session.Policy) *JobOrderService {
	return &JobOrderService{s.plans.WithSessionPolicy(p)}
}

func normalizedOrder(tenantID, siteID string, o JobOrderBodyV1) (JobOrderBodyV1, []byte, string, error) {
	if due, ok := validTime(o.DueAt); ok {
		o.DueAt = due.UTC().Format(time.RFC3339Nano)
	}
	encoded, digest, err := canonical.Envelope(jobOrderSchema, jobOrderDigestVersion, tenantID, siteID, o)
	return o, encoded, digest, err
}

// DecodeJobOrder rejects oversized, unknown-field, duplicate-key and malformed JSON before
// running semantic validation.
func DecodeJobOrder(data []byte) (JobOrderBodyV1, error) {
	var o JobOrderBodyV1
	if strictjson.Decode(data, &o, limits.RequestBodyBytes) != nil || o.Validate() != nil {
		return JobOrderBodyV1{}, ErrInvalidJobOrder
	}
	return o, nil
}

// instructionApproved applies the document confirm_current predicate of
// knowledge/freshness_qualification.go to the pinned revision: an owner-designated document
// whose latest currentness decision confirms exactly this revision, hash and access epoch,
// with the source neither revoked nor revised since. Freshness-policy age is not applied.
func instructionApproved(ctx context.Context, tx *sql.Tx, sc Scope, o InstructionRefV1) (bool, error) {
	var ok bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM supervisor.evidence_currentness_heads ch
 JOIN supervisor.evidence_currentness_decisions d ON (d.tenant_id,d.site_id,d.source_id,d.revision)=(ch.tenant_id,ch.site_id,ch.source_id,ch.current_revision)
 JOIN supervisor.evidence_heads eh ON (eh.tenant_id,eh.site_id,eh.source_id)=(ch.tenant_id,ch.site_id,ch.source_id)
 JOIN supervisor.evidence_revisions r ON (r.tenant_id,r.site_id,r.source_id,r.revision)=(d.tenant_id,d.site_id,d.source_id,d.source_revision)
 JOIN supervisor.evidence_authority_heads ah ON (ah.tenant_id,ah.site_id,ah.source_id)=(d.tenant_id,d.site_id,d.source_id)
 JOIN supervisor.evidence_authority_decisions a ON (a.tenant_id,a.site_id,a.source_id,a.revision,a.decision_id)=(d.tenant_id,d.site_id,d.source_id,d.authority_revision,d.authority_decision_id)
 WHERE ch.tenant_id=$1 AND ch.site_id=$2 AND ch.source_id=$3 AND d.source_revision=$4 AND d.source_sha256=$5 AND r.kind='document'
 AND d.verdict='confirm_current' AND eh.current_revision=d.source_revision AND eh.access_epoch=d.source_access_epoch AND NOT eh.revoked
 AND ah.current_revision=d.authority_revision AND a.verdict='designate' AND (r.extraction IS NULL OR a.model_extracted_confirmed)
 AND NOT EXISTS(SELECT 1 FROM supervisor.evidence_revocations rv WHERE (rv.tenant_id,rv.site_id,rv.source_id)=(d.tenant_id,d.site_id,d.source_id)))`,
		sc.TenantID, sc.SiteID, o.DocumentID, o.Revision, o.SHA256).Scan(&ok)
	return ok, err
}

func orderInstruction(o JobOrderBodyV1) InstructionRefV1 {
	return InstructionRefV1{DocumentID: o.InstructionDocumentID, Revision: o.InstructionRevision, SHA256: o.InstructionSHA256}
}

// instructionReason re-applies the instruction predicate at delivery time and names the
// first failing condition, or returns "" when the pinned instruction is still approved.
func instructionReason(ctx context.Context, tx *sql.Tx, sc Scope, o InstructionRefV1) (InstructionReason, error) {
	approved, err := instructionApproved(ctx, tx, sc, o)
	if err != nil || approved {
		return "", err
	}
	var revoked, superseded bool
	err = tx.QueryRowContext(ctx, `SELECT eh.revoked OR EXISTS(SELECT 1 FROM supervisor.evidence_revocations rv WHERE (rv.tenant_id,rv.site_id,rv.source_id)=(eh.tenant_id,eh.site_id,eh.source_id)),eh.current_revision<>$4 FROM supervisor.evidence_heads eh WHERE eh.tenant_id=$1 AND eh.site_id=$2 AND eh.source_id=$3`, sc.TenantID, sc.SiteID, o.DocumentID, o.Revision).Scan(&revoked, &superseded)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return InstructionRevoked, nil
	case err != nil:
		return "", err
	case revoked:
		return InstructionRevoked, nil
	case superseded:
		return InstructionSuperseded, nil
	}
	return InstructionNotConfirmed, nil
}

func jobAudit(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, id string, revision int64, event string) error {
	_, e := tx.ExecContext(ctx, `INSERT INTO supervisor.job_order_audit(tenant_id,site_id,actor_issuer,actor_subject,job_order_id,revision,event_type) VALUES($1,$2,$3,$4,NULLIF($5,'')::uuid,NULLIF($6,0),$7)`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, id, revision, event)
	return e
}

// jobReceipt returns the job order of an earlier identical request, or ErrConflict when the
// key was used for a different request.
func jobReceipt(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, key, hash, op string) (string, bool, error) {
	var stored, storedOp, id string
	var revision int64
	e := tx.QueryRowContext(ctx, `SELECT request_sha256,operation,job_order_id,revision FROM supervisor.job_order_receipts WHERE tenant_id=$1 AND site_id=$2 AND actor_issuer=$3 AND actor_subject=$4 AND idempotency_key=$5`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, key).Scan(&stored, &storedOp, &id, &revision)
	if errors.Is(e, sql.ErrNoRows) {
		return "", false, nil
	}
	if e != nil {
		return "", false, e
	}
	if stored != hash || storedOp != op {
		return "", false, ErrConflict
	}
	return id, true, jobAudit(ctx, tx, a, sc, id, revision, "mutation.replayed")
}

// write runs the shared create/revise path: replay, instruction check, append and receipt.
func (s *JobOrderService) write(ctx context.Context, a Actor, sc Scope, key, op string, request []any, o JobOrderBodyV1, apply func(tx *sql.Tx, digest string) (string, int64, error)) (JobOrderV1, error) {
	if !validText(key, 128) || o.Validate() != nil {
		return JobOrderV1{}, ErrInvalidJobOrder
	}
	o, encoded, digest, err := normalizedOrder(sc.TenantID, sc.SiteID, o)
	if err != nil {
		return JobOrderV1{}, ErrInvalidJobOrder
	}
	hash := requestDigest(append(request, digest))
	tx, _, _, err := s.plans.begin(ctx, a, sc, shiftWrite)
	if err != nil {
		return JobOrderV1{}, err
	}
	defer tx.Rollback()
	id, replayed, err := jobReceipt(ctx, tx, a, sc, key, hash, op)
	if err != nil {
		return JobOrderV1{}, err
	}
	if replayed {
		out, err := loadJobOrder(ctx, tx, sc, id)
		if err != nil {
			return out, err
		}
		out.Replayed = true
		return out, s.plans.finish(ctx, tx, a)
	}
	if err = validateJobRouting(ctx, tx, sc, o); err != nil {
		return JobOrderV1{}, err
	}
	id, revision, err := apply(tx, digest)
	if err != nil {
		return JobOrderV1{}, err
	}
	approved, err := instructionApproved(ctx, tx, sc, orderInstruction(o))
	if err != nil {
		return JobOrderV1{}, err
	}
	if !approved {
		return JobOrderV1{}, ErrInstructionNotApproved
	}
	payload, err := json.Marshal(o)
	if err != nil {
		return JobOrderV1{}, ErrInvalidJobOrder
	}
	if _, e := tx.ExecContext(ctx, `INSERT INTO supervisor.job_order_revisions(tenant_id,site_id,job_order_id,revision,payload,canonical_payload,canonical_payload_sha256,digest_schema_version,instruction_source_id,instruction_revision,instruction_sha256,actor_issuer,actor_subject) VALUES($1,$2,$3,$4,$5::jsonb,$6,$7,$8,$9,$10,$11,$12,$13)`, sc.TenantID, sc.SiteID, id, revision, string(payload), string(encoded), digest, jobOrderDigestVersion, o.InstructionDocumentID, o.InstructionRevision, o.InstructionSHA256, a.Issuer, a.Subject); e != nil {
		return JobOrderV1{}, e
	}
	if _, e := tx.ExecContext(ctx, `INSERT INTO supervisor.job_order_receipts(tenant_id,site_id,actor_issuer,actor_subject,idempotency_key,request_sha256,operation,job_order_id,revision) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, key, hash, op, id, revision); e != nil {
		return JobOrderV1{}, e
	}
	event := "job_order.created"
	if op == "revise" {
		event = "job_order.revised"
	}
	if e := jobAudit(ctx, tx, a, sc, id, revision, event); e != nil {
		return JobOrderV1{}, e
	}
	out, err := loadJobOrder(ctx, tx, sc, id)
	if err != nil {
		return out, err
	}
	return out, s.plans.finish(ctx, tx, a)
}

// Create opens a job order at revision 1.
func (s *JobOrderService) Create(ctx context.Context, a Actor, sc Scope, o JobOrderBodyV1, key string) (JobOrderV1, error) {
	return s.write(ctx, a, sc, key, "create", []any{"create"}, o, func(tx *sql.Tx, _ string) (string, int64, error) {
		id := uuid.NewString()
		_, e := tx.ExecContext(ctx, `INSERT INTO supervisor.job_order_heads(tenant_id,site_id,id,current_revision) VALUES($1,$2,$3,1)`, sc.TenantID, sc.SiteID, id)
		return id, 1, e
	})
}

// Revise appends a revision that supersedes the current one at once. A shift plan pinning
// the earlier revision stops delivering it (needs_replan) until it is re-pinned and approved.
func (s *JobOrderService) Revise(ctx context.Context, a Actor, sc Scope, id string, o JobOrderBodyV1, key string, expected int64) (JobOrderV1, error) {
	if expected < 1 {
		return JobOrderV1{}, ErrInvalidJobOrder
	}
	return s.write(ctx, a, sc, key, "revise", []any{"revise", id, expected}, o, func(tx *sql.Tx, digest string) (string, int64, error) {
		if !canonicalID(id) {
			return "", 0, ErrNotFound
		}
		var current int64
		var currentDigest string
		e := tx.QueryRowContext(ctx, `SELECT h.current_revision,r.canonical_payload_sha256 FROM supervisor.job_order_heads h JOIN supervisor.job_order_revisions r ON (r.tenant_id,r.site_id,r.job_order_id,r.revision)=(h.tenant_id,h.site_id,h.id,h.current_revision) WHERE h.tenant_id=$1 AND h.site_id=$2 AND h.id=$3 FOR UPDATE OF h`, sc.TenantID, sc.SiteID, id).Scan(&current, &currentDigest)
		if errors.Is(e, sql.ErrNoRows) {
			return "", 0, ErrNotFound
		}
		if e != nil {
			return "", 0, e
		}
		if current != expected {
			return "", 0, ErrRevision
		}
		if digest == currentDigest {
			return "", 0, ErrJobOrderUnchanged
		}
		_, e = tx.ExecContext(ctx, `UPDATE supervisor.job_order_heads SET current_revision=$4 WHERE tenant_id=$1 AND site_id=$2 AND id=$3`, sc.TenantID, sc.SiteID, id, current+1)
		return id, current + 1, e
	})
}

// Get returns a job order with every revision.
func (s *JobOrderService) Get(ctx context.Context, a Actor, sc Scope, id string) (JobOrderV1, error) {
	if !canonicalID(id) {
		return JobOrderV1{}, ErrNotFound
	}
	tx, _, _, err := s.plans.begin(ctx, a, sc, shiftRead)
	if err != nil {
		return JobOrderV1{}, err
	}
	defer tx.Rollback()
	out, err := loadJobOrder(ctx, tx, sc, id)
	if err != nil {
		return out, err
	}
	if err = jobAudit(ctx, tx, a, sc, id, 0, "job_order.read"); err != nil {
		return out, err
	}
	return out, s.plans.finish(ctx, tx, a)
}

// List pages job order heads by ID.
func (s *JobOrderService) List(ctx context.Context, a Actor, sc Scope, limit int, cursor string, includeCurrent bool) (JobOrderPageV1, error) {
	out := JobOrderPageV1{JobOrders: []JobOrderSummaryV1{}}
	if limit < 1 || limit > 100 || (cursor != "" && !canonicalID(cursor)) {
		return out, ErrInvalidJobOrder
	}
	if cursor == "" {
		cursor = uuid.Nil.String()
	}
	tx, _, _, err := s.plans.begin(ctx, a, sc, shiftRead)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	query := `SELECT id,current_revision FROM supervisor.job_order_heads WHERE tenant_id=$1 AND site_id=$2 AND id>$3 ORDER BY id LIMIT $4`
	if includeCurrent {
		query = `SELECT h.id,h.current_revision,r.revision,r.payload,r.canonical_payload_sha256,r.digest_schema_version
FROM supervisor.job_order_heads h
LEFT JOIN supervisor.job_order_revisions r ON (r.tenant_id,r.site_id,r.job_order_id,r.revision)=(h.tenant_id,h.site_id,h.id,h.current_revision)
WHERE h.tenant_id=$1 AND h.site_id=$2 AND h.id>$3 ORDER BY h.id LIMIT $4`
	}
	rows, err := tx.QueryContext(ctx, query, sc.TenantID, sc.SiteID, cursor, limit+1)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var j JobOrderSummaryV1
		if includeCurrent {
			var revision, digestVersion sql.NullInt64
			var digest sql.NullString
			var payload []byte
			if err = rows.Scan(&j.ID, &j.CurrentRevision, &revision, &payload, &digest, &digestVersion); err != nil {
				return out, err
			}
			if !revision.Valid || revision.Int64 != j.CurrentRevision || !digest.Valid || !digestVersion.Valid || digestVersion.Int64 != jobOrderDigestVersion {
				return out, errIntegrity
			}
			order, decodeErr := DecodeJobOrder(payload)
			if decodeErr != nil {
				return out, errIntegrity
			}
			if _, _, currentDigest, digestErr := normalizedOrder(sc.TenantID, sc.SiteID, order); digestErr != nil || currentDigest != digest.String {
				return out, errIntegrity
			}
			j.CurrentOrder = &order
		} else if err = rows.Scan(&j.ID, &j.CurrentRevision); err != nil {
			return out, err
		}
		out.JobOrders = append(out.JobOrders, j)
	}
	if err = rows.Err(); err != nil {
		return out, err
	}
	if len(out.JobOrders) > limit {
		out.JobOrders = out.JobOrders[:limit]
		out.NextCursor = out.JobOrders[limit-1].ID
	}
	if err = jobAudit(ctx, tx, a, sc, "", 0, "job_order.listed"); err != nil {
		return out, err
	}
	if includeCurrent {
		// The page event records the list; each disclosed body gets the detail route's per-ID read audit.
		for _, order := range out.JobOrders {
			if err = jobAudit(ctx, tx, a, sc, order.ID, 0, "job_order.read"); err != nil {
				return out, err
			}
		}
	}
	return out, s.plans.finish(ctx, tx, a)
}

// Current returns what a workstation shows at an instant (the database clock when at is
// nil): the job order pinned by the approved shift plan in force there, delivered only while
// the pin is still the job order's head revision.
func (s *JobOrderService) Current(ctx context.Context, a Actor, sc Scope, workstationID string, at *time.Time) (CurrentJobOrderV1, error) {
	if !canonicalID(workstationID) {
		return CurrentJobOrderV1{WorkstationID: workstationID}, ErrInvalidJobOrder
	}
	tx, _, _, err := s.plans.begin(ctx, a, sc, shiftRead)
	if err != nil {
		return CurrentJobOrderV1{WorkstationID: workstationID}, err
	}
	defer tx.Rollback()
	out, err := resolveCurrent(ctx, tx, sc, workstationID, at)
	if err != nil {
		return out, err
	}
	event := "delivery.needs_replan"
	switch out.State {
	case DeliveryCurrent:
		event = "job_order.delivered"
	case DeliveryInstructionUnavailable:
		event = "delivery.instruction_unavailable"
	}
	if err = jobAudit(ctx, tx, a, sc, out.JobOrderID, out.PinnedRevision, event); err != nil {
		return out, err
	}
	return out, s.plans.finish(ctx, tx, a)
}

// resolveCurrent is Current inside the caller's transaction, without the delivery audit, so
// a station record resolves its job order in the same snapshot that writes it.
func resolveCurrent(ctx context.Context, tx *sql.Tx, sc Scope, workstationID string, at *time.Time) (CurrentJobOrderV1, error) {
	out := CurrentJobOrderV1{WorkstationID: workstationID}
	rows, err := tx.QueryContext(ctx, `SELECT h.id,h.approved_revision,h.line_id,h.starts_at,h.ends_at,x.job_order_id,x.job_order_revision,j.current_revision
 FROM supervisor.shift_plan_heads h
 JOIN supervisor.shift_plan_assignments x ON (x.tenant_id,x.site_id,x.plan_id,x.revision)=(h.tenant_id,h.site_id,h.id,h.approved_revision)
 JOIN supervisor.job_order_heads j ON (j.tenant_id,j.site_id,j.id)=(x.tenant_id,x.site_id,x.job_order_id)
 WHERE h.tenant_id=$1 AND h.site_id=$2 AND x.workstation_id=$3 AND h.starts_at<=coalesce($4,clock_timestamp()) AND coalesce($4,clock_timestamp())<h.ends_at
 ORDER BY h.id LIMIT 2`, sc.TenantID, sc.SiteID, workstationID, at)
	if err != nil {
		return out, err
	}
	found := 0
	for rows.Next() {
		found++
		if err = rows.Scan(&out.PlanID, &out.PlanRevision, &out.LineID, &out.StartsAt, &out.EndsAt, &out.JobOrderID, &out.PinnedRevision, &out.CurrentRevision); err != nil {
			rows.Close()
			return out, err
		}
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return out, err
	}
	switch found {
	case 0:
		return CurrentJobOrderV1{WorkstationID: workstationID}, ErrNoCurrentJobOrder
	case 2:
		// The approval guard forbids this; refuse rather than choose between two plans.
		return CurrentJobOrderV1{WorkstationID: workstationID}, ErrAssignmentConflict
	}
	out.StartsAt, out.EndsAt = out.StartsAt.UTC(), out.EndsAt.UTC()
	plan, err := loadShiftPlan(ctx, tx, sc, out.PlanID)
	if err != nil {
		return out, err
	}
	revision := plan.Revisions[out.PlanRevision-1]
	if revision.State != StateApproved {
		return out, errIntegrity
	}
	out.Timezone = revision.Plan.Timezone
	assigned := false
	for _, x := range revision.Plan.Assignments {
		if x.WorkstationID == workstationID {
			if x.JobOrderID != out.JobOrderID || x.JobOrderRevision != out.PinnedRevision {
				return out, errIntegrity
			}
			out.TargetQuantity, out.TargetCycleTimeSeconds, assigned = x.TargetQuantity, x.TargetCycleTimeSeconds, true
		}
	}
	if !assigned {
		return out, errIntegrity
	}
	out.State = DeliveryNeedsReplan
	order, err := loadJobOrder(ctx, tx, sc, out.JobOrderID)
	if err != nil {
		return out, err
	}
	// The snapshot already makes these agree; re-checking the loaded head keeps a superseded
	// body from ever being labelled current.
	out.CurrentRevision = order.CurrentRevision
	if out.PinnedRevision == order.CurrentRevision {
		pinned := order.Revisions[out.PinnedRevision-1]
		reason, err := instructionReason(ctx, tx, sc, orderInstruction(pinned.Order))
		if err != nil {
			return out, err
		}
		if reason != "" {
			out.State, out.InstructionReason = DeliveryInstructionUnavailable, reason
		} else {
			out.State, out.Order = DeliveryCurrent, &pinned
		}
	}
	return out, nil
}

// checkPins binds every assignment to an existing job-order revision that is still its head
// and agrees with it on order, operation, item, instruction, template and every required
// field. Shift targets may differ (a partial order is carried forward) but never exceed the
// order's target quantity.
func checkPins(ctx context.Context, tx *sql.Tx, sc Scope, p ShiftPlanBodyV1) error {
	for _, x := range p.Assignments {
		var payload []byte
		var current int64
		e := tx.QueryRowContext(ctx, `SELECT r.payload,h.current_revision FROM supervisor.job_order_revisions r JOIN supervisor.job_order_heads h ON (h.tenant_id,h.site_id,h.id)=(r.tenant_id,r.site_id,r.job_order_id) WHERE r.tenant_id=$1 AND r.site_id=$2 AND r.job_order_id=$3 AND r.revision=$4`, sc.TenantID, sc.SiteID, x.JobOrderID, x.JobOrderRevision).Scan(&payload, &current)
		if errors.Is(e, sql.ErrNoRows) {
			return ErrJobOrderUnknown
		}
		if e != nil {
			return e
		}
		if current != x.JobOrderRevision {
			return ErrJobOrderNotCurrent
		}
		o, e := DecodeJobOrder(payload)
		if e != nil {
			return errIntegrity
		}
		if o.OrderID != x.OrderID || o.OperationID != x.OperationID || o.ItemCode != x.ItemCode || o.InstructionDocumentID != x.InstructionDocumentID ||
			o.InstructionRevision != x.InstructionRevision || o.InstructionSHA256 != x.InstructionSHA256 || x.TargetQuantity > o.TargetQuantity || o.Template != x.Template {
			return ErrJobOrderMismatch
		}
		// Every field the job order requires appears identically on the assignment's screen.
		assigned := make(map[string]FieldV1, len(x.Fields))
		for _, field := range x.Fields {
			assigned[field.Name] = field
		}
		for _, field := range o.Fields {
			if got, ok := assigned[field.Name]; field.Required && (!ok || !sameField(got, field)) {
				return ErrJobOrderMismatch
			}
		}
	}
	return nil
}

func sameField(a, b FieldV1) bool {
	if a.Name != b.Name || a.Label != b.Label || a.Kind != b.Kind || a.Required != b.Required || a.Unit != b.Unit || len(a.Choices) != len(b.Choices) {
		return false
	}
	for i := range a.Choices {
		if a.Choices[i] != b.Choices[i] {
			return false
		}
	}
	return true
}

// checkApprovable mirrors the approval guard so callers get a stable error: every pin must
// still be current, and no workstation may be booked by another approved, overlapping plan.
func checkApprovable(ctx context.Context, tx *sql.Tx, sc Scope, planID string, revision int64) error {
	var stale, booked bool
	if e := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM supervisor.shift_plan_assignments a JOIN supervisor.job_order_heads j ON (j.tenant_id,j.site_id,j.id)=(a.tenant_id,a.site_id,a.job_order_id) WHERE a.tenant_id=$1 AND a.site_id=$2 AND a.plan_id=$3 AND a.revision=$4 AND j.current_revision<>a.job_order_revision),
 EXISTS(SELECT 1 FROM supervisor.shift_plan_assignments mine
  JOIN supervisor.shift_plan_heads me ON (me.tenant_id,me.site_id,me.id)=(mine.tenant_id,mine.site_id,mine.plan_id)
  JOIN supervisor.shift_plan_heads other ON other.tenant_id=me.tenant_id AND other.site_id=me.site_id AND other.id<>me.id AND other.approved_revision IS NOT NULL AND other.starts_at<me.ends_at AND me.starts_at<other.ends_at
  JOIN supervisor.shift_plan_assignments theirs ON (theirs.tenant_id,theirs.site_id,theirs.plan_id,theirs.revision)=(other.tenant_id,other.site_id,other.id,other.approved_revision) AND theirs.workstation_id=mine.workstation_id
  WHERE mine.tenant_id=$1 AND mine.site_id=$2 AND mine.plan_id=$3 AND mine.revision=$4)`, sc.TenantID, sc.SiteID, planID, revision).Scan(&stale, &booked); e != nil {
		return e
	}
	if stale {
		return ErrJobOrderNotCurrent
	}
	if booked {
		return ErrAssignmentConflict
	}
	return nil
}

func loadJobOrder(ctx context.Context, tx *sql.Tx, sc Scope, id string) (JobOrderV1, error) {
	out := JobOrderV1{Revisions: []JobOrderRevisionV1{}}
	e := tx.QueryRowContext(ctx, `SELECT id,current_revision FROM supervisor.job_order_heads WHERE tenant_id=$1 AND site_id=$2 AND id=$3`, sc.TenantID, sc.SiteID, id).Scan(&out.ID, &out.CurrentRevision)
	if errors.Is(e, sql.ErrNoRows) {
		return out, ErrNotFound
	}
	if e != nil {
		return out, e
	}
	rows, e := tx.QueryContext(ctx, `SELECT revision,payload,canonical_payload_sha256,digest_schema_version,actor_issuer,actor_subject,created_at FROM supervisor.job_order_revisions WHERE tenant_id=$1 AND site_id=$2 AND job_order_id=$3 ORDER BY revision`, sc.TenantID, sc.SiteID, id)
	if e != nil {
		return out, e
	}
	defer rows.Close()
	for rows.Next() {
		var r JobOrderRevisionV1
		var payload []byte
		if e = rows.Scan(&r.Revision, &payload, &r.CanonicalPayloadSHA256, &r.DigestSchemaVersion, &r.SubmittedBy.Issuer, &r.SubmittedBy.Subject, &r.CreatedAt); e != nil {
			return out, e
		}
		// Stored bodies are re-validated and re-verified against their digest on every read.
		if r.Order, e = DecodeJobOrder(payload); e != nil {
			return out, errIntegrity
		}
		if _, _, recomputed, err := normalizedOrder(sc.TenantID, sc.SiteID, r.Order); err != nil || recomputed != r.CanonicalPayloadSHA256 || r.Revision != int64(len(out.Revisions)+1) {
			return out, errIntegrity
		}
		r.CreatedAt, r.State = r.CreatedAt.UTC(), JobOrderSuperseded
		if r.Revision == out.CurrentRevision {
			r.State = JobOrderCurrent
		}
		out.Revisions = append(out.Revisions, r)
	}
	if e = rows.Err(); e != nil {
		return out, e
	}
	if int64(len(out.Revisions)) != out.CurrentRevision {
		return out, errIntegrity
	}
	return out, nil
}
