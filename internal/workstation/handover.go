package workstation

import (
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/canonical"
)

// WIS-520 next-shift handover: a pure, deterministic draft over one persisted shift summary
// snapshot (cited by revision and digest) and the job orders' current head revisions. It
// lists each planned job with what remains, the current instruction and whether it changed
// since the shift's pin, open blockers, recorded holds and material exceptions, late and
// corrected entries, and copies the summary's readiness and reasons as source freshness. It
// proposes a ShiftPlanBodyV1 for the next shift that only carries forward jobs whose
// remaining quantity is known; it never infers a quantity or a cycle-time target. The draft
// is data: it creates or changes no plan (rule 05). It reads no clock, database or network.

const (
	ShiftHandoverFormulaVersion = 1
	shiftHandoverSchema         = "ShiftHandoverV1"
	shiftHandoverDigestV1       = 1
	CodeInvalidShiftHandover    = "INVALID_SHIFT_HANDOVER_REQUEST"
)

// CarryReason says why a job is not carried into the proposed plan. A stable code.
type CarryReason string

const (
	CarryCompleted              CarryReason = "COMPLETED"
	CarryRemainingUnknown       CarryReason = "REMAINING_UNKNOWN"
	CarryNothingRemaining       CarryReason = "NOTHING_REMAINING"
	CarryExceedsOrderTarget     CarryReason = "REMAINING_EXCEEDS_ORDER_TARGET"
	CarryInstructionUnavailable CarryReason = "INSTRUCTION_UNAVAILABLE"
)

func CarryReasons() []CarryReason {
	return []CarryReason{CarryCompleted, CarryRemainingUnknown, CarryNothingRemaining, CarryExceedsOrderTarget, CarryInstructionUnavailable}
}

// NextShiftV1 is the supervisor-supplied next shift the proposal targets.
type NextShiftV1 struct {
	LineID   string `json:"line_id"`
	StartsAt string `json:"starts_at"`
	EndsAt   string `json:"ends_at"`
	Timezone string `json:"timezone"`
}

// ShiftHandoverRequestV1 asks for a handover draft from one summary snapshot of the plan in
// the path; tenant, site and actor come from the session.
type ShiftHandoverRequestV1 struct {
	SchemaVersion   int         `json:"schema_version"`
	SummaryRevision int64       `json:"summary_revision"`
	NextShift       NextShiftV1 `json:"next_shift"`
}

// HandoverSourceV1 is the cited summary's freshness, copied as reported.
type HandoverSourceV1 struct {
	Readiness         SummaryReadiness `json:"readiness"`
	Reasons           []SummaryReason  `json:"reasons"`
	AsOf              time.Time        `json:"as_of"`
	ShiftEnded        bool             `json:"shift_ended"`
	SourceRecordCount int              `json:"source_record_count"`
}

// HandoverInstructionV1 is the instruction of the job order's current head revision.
// Approved re-applies the delivery predicate at generation; ChangedSincePin compares it with
// the instruction the shift's approved assignment pinned.
type HandoverInstructionV1 struct {
	DocumentID      string            `json:"document_id"`
	Revision        int64             `json:"revision"`
	SHA256          string            `json:"sha256" contract:"sha256"`
	Approved        bool              `json:"approved"`
	Reason          InstructionReason `json:"reason,omitempty"`
	ChangedSincePin bool              `json:"changed_since_pin"`
}

// HandoverJobV1 is one planned job of the summarized shift. RemainingQuantity is null when
// unknown: no quantity was recorded, or the station's summary is NOT_READY or INVALID.
type HandoverJobV1 struct {
	WorkstationID           string                    `json:"workstation_id"`
	JobOrderID              string                    `json:"job_order_id"`
	CompletionState         CompletionState           `json:"completion_state"`
	StationReadiness        SummaryReadiness          `json:"station_readiness"`
	StationReasons          []SummaryReason           `json:"station_reasons"`
	PinnedJobOrderRevision  int64                     `json:"pinned_job_order_revision"`
	CurrentJobOrderRevision int64                     `json:"current_job_order_revision"`
	JobOrderChanged         bool                      `json:"job_order_changed"`
	ShiftTargetQuantity     int64                     `json:"shift_target_quantity"`
	GoodQuantity            *int64                    `json:"good_quantity"`
	RemainingQuantity       *int64                    `json:"remaining_quantity"`
	Instruction             HandoverInstructionV1     `json:"instruction"`
	OpenBlockers            []DowntimeIntervalV1      `json:"open_blockers"`
	QualityHolds            []DispositionAsRecordedV1 `json:"quality_holds_as_recorded"`
	MaterialExceptions      []MaterialExceptionV1     `json:"material_exceptions"`
	LateRecordIDs           []string                  `json:"late_record_ids"`
	Corrections             []CorrectionChainV1       `json:"corrections"`
	RecordIDs               []string                  `json:"record_ids"`
	Proposed                bool                      `json:"proposed"`
	NotProposedReason       CarryReason               `json:"not_proposed_reason,omitempty"`
}

// ShiftHandoverV1 is the computed draft. It carries no generated ID, creation time or
// actor, so identical inputs always serialize and hash identically. ProposedPlan is null
// when no job can be carried forward.
type ShiftHandoverV1 struct {
	SchemaVersion      int              `json:"schema_version"`
	FormulaVersion     int              `json:"formula_version"`
	PlanID             string           `json:"plan_id"`
	PlanRevision       int64            `json:"plan_revision"`
	PlanRevisionSHA256 string           `json:"plan_revision_digest" contract:"sha256"`
	LineID             string           `json:"line_id"`
	StartsAt           time.Time        `json:"starts_at"`
	EndsAt             time.Time        `json:"ends_at"`
	SummaryRevision    int64            `json:"summary_revision"`
	SummarySHA256      string           `json:"summary_digest" contract:"sha256"`
	Source             HandoverSourceV1 `json:"source"`
	NextShift          NextShiftV1      `json:"next_shift"`
	Jobs               []HandoverJobV1  `json:"jobs"`
	Notes              []SummaryNoteV1  `json:"notes"`
	ProposedPlan       *ShiftPlanBodyV1 `json:"proposed_plan"`
}

// HandoverJobOrderV1 is a job order's head revision at generation and, when its instruction
// fails the delivery predicate, the reason ("" when approved).
type HandoverJobOrderV1 struct {
	Current           JobOrderRevisionV1
	InstructionReason InstructionReason
}

// ShiftHandoverInput is everything the kernel reads. Approved is the plan revision the
// summary cites; JobOrders holds the head of every job order that revision assigns.
type ShiftHandoverInput struct {
	SummaryRevision int64
	SummarySHA256   string
	Summary         ShiftSummaryV1
	Approved        ShiftPlanRevisionV1
	JobOrders       map[string]HandoverJobOrderV1
	Next            NextShiftV1
}

// DraftHandover computes the handover and its canonical envelope and digest.
func DraftHandover(sc Scope, in ShiftHandoverInput) (ShiftHandoverV1, []byte, string, error) {
	s := in.Summary
	if in.Approved.Revision != s.PlanRevision || in.Approved.CanonicalPayloadSHA256 != s.PlanRevisionSHA256 || in.SummaryRevision < 1 || !digest64.MatchString(in.SummarySHA256) {
		return ShiftHandoverV1{}, nil, "", errIntegrity
	}
	// The next shift starts no earlier than the summarized shift ends: a handover never
	// proposes changes to the shift it hands over.
	next, _ := validTime(in.Next.StartsAt)
	if !validShift(in.Next.LineID, in.Next.StartsAt, in.Next.EndsAt, in.Next.Timezone) || next.Before(s.EndsAt) {
		return ShiftHandoverV1{}, nil, "", &ConfigError{CodeInvalidShiftHandover, "next_shift"}
	}
	out := ShiftHandoverV1{SchemaVersion: 1, FormulaVersion: ShiftHandoverFormulaVersion, PlanID: s.PlanID, PlanRevision: s.PlanRevision, PlanRevisionSHA256: s.PlanRevisionSHA256,
		LineID: s.LineID, StartsAt: s.StartsAt.UTC(), EndsAt: s.EndsAt.UTC(), SummaryRevision: in.SummaryRevision, SummarySHA256: in.SummarySHA256,
		Source:    HandoverSourceV1{Readiness: s.Readiness, Reasons: append([]SummaryReason{}, s.Reasons...), AsOf: s.AsOf.UTC(), ShiftEnded: s.ShiftEnded, SourceRecordCount: s.SourceRecordCount},
		NextShift: in.Next, Jobs: []HandoverJobV1{}, Notes: append([]SummaryNoteV1{}, s.Notes...)}
	pinned := map[stationKey]AssignmentV1{}
	for _, a := range in.Approved.Plan.Assignments {
		pinned[stationKey{a.WorkstationID, a.JobOrderID}] = a
	}
	proposal := []AssignmentV1{}
	// Summary stations are sorted by workstation and job order; a station whose records name
	// a job order the approved revision does not assign is not a planned job.
	for _, st := range s.Stations {
		a, ok := pinned[stationKey{st.WorkstationID, st.JobOrderID}]
		if !ok || st.Planned == nil {
			continue
		}
		order, ok := in.JobOrders[st.JobOrderID]
		if !ok || order.Current.Revision < a.JobOrderRevision {
			return ShiftHandoverV1{}, nil, "", errIntegrity
		}
		o, rec := order.Current.Order, st.Recorded
		job := HandoverJobV1{WorkstationID: st.WorkstationID, JobOrderID: st.JobOrderID, CompletionState: rec.CompletionState, StationReadiness: st.Readiness,
			StationReasons: append([]SummaryReason{}, st.Reasons...), PinnedJobOrderRevision: a.JobOrderRevision, CurrentJobOrderRevision: order.Current.Revision,
			JobOrderChanged: order.Current.Revision != a.JobOrderRevision, ShiftTargetQuantity: a.TargetQuantity, GoodQuantity: rec.GoodQuantity,
			Instruction: HandoverInstructionV1{DocumentID: o.InstructionDocumentID, Revision: o.InstructionRevision, SHA256: o.InstructionSHA256, Approved: order.InstructionReason == "", Reason: order.InstructionReason,
				ChangedSincePin: o.InstructionDocumentID != a.InstructionDocumentID || o.InstructionRevision != a.InstructionRevision || o.InstructionSHA256 != a.InstructionSHA256},
			OpenBlockers: []DowntimeIntervalV1{}, QualityHolds: []DispositionAsRecordedV1{}, MaterialExceptions: append([]MaterialExceptionV1{}, rec.MaterialExceptions...),
			LateRecordIDs: append([]string{}, rec.LateRecordIDs...), Corrections: append([]CorrectionChainV1{}, rec.Corrections...), RecordIDs: append([]string{}, st.RecordIDs...)}
		for _, d := range rec.Downtime {
			if d.Open {
				job.OpenBlockers = append(job.OpenBlockers, d)
			}
		}
		for _, d := range rec.Quality.DispositionsAsRecorded {
			if d.Disposition == DispositionHold {
				job.QualityHolds = append(job.QualityHolds, d)
			}
		}
		// A remaining quantity is reported only from a station summary that has one and is
		// neither NOT_READY nor INVALID; otherwise it is unknown, never zero or the target.
		if rec.RemainingQuantity != nil && st.Readiness != SummaryNotReady && st.Readiness != SummaryInvalid {
			remaining := *rec.RemainingQuantity
			job.RemainingQuantity = &remaining
		}
		switch {
		case rec.CompletionState == CompletionCompleted:
			job.NotProposedReason = CarryCompleted
		case job.RemainingQuantity == nil:
			job.NotProposedReason = CarryRemainingUnknown
		case *job.RemainingQuantity < 1:
			job.NotProposedReason = CarryNothingRemaining
		case *job.RemainingQuantity > o.TargetQuantity:
			job.NotProposedReason = CarryExceedsOrderTarget
		case !job.Instruction.Approved:
			job.NotProposedReason = CarryInstructionUnavailable
		default:
			// Pinned to the current head revision; every other field, the declared cycle-time
			// target included, is copied from that revision and never computed.
			job.Proposed = true
			proposal = append(proposal, AssignmentV1{WorkstationID: st.WorkstationID, JobOrderID: st.JobOrderID, JobOrderRevision: order.Current.Revision, OrderID: o.OrderID,
				OperationID: o.OperationID, ItemCode: o.ItemCode, TargetQuantity: *job.RemainingQuantity, TargetCycleTimeSeconds: o.TargetCycleTimeSeconds,
				InstructionDocumentID: o.InstructionDocumentID, InstructionRevision: o.InstructionRevision, InstructionSHA256: o.InstructionSHA256, Template: o.Template,
				Fields: append([]FieldV1{}, o.Fields...)})
		}
		out.Jobs = append(out.Jobs, job)
	}
	if len(proposal) > 0 {
		p := ShiftPlanBodyV1{SchemaVersion: 1, LineID: in.Next.LineID, StartsAt: in.Next.StartsAt, EndsAt: in.Next.EndsAt, Timezone: in.Next.Timezone, Assignments: proposal}
		if p.Validate() != nil {
			return ShiftHandoverV1{}, nil, "", errIntegrity
		}
		out.ProposedPlan = &p
	}
	encoded, digest, err := canonical.Envelope(shiftHandoverSchema, shiftHandoverDigestV1, sc.TenantID, sc.SiteID, out)
	return out, encoded, digest, err
}
