package workstation

import (
	"math"
	"math/big"
	"sort"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/canonical"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/production"
)

// WIS-520 shift summary: a pure, deterministic kernel over the approved shift plan revision
// in force and the operator station records bound to that plan. It separates what was
// planned from what was recorded, never turns a missing input into a zero, keeps manual
// notes attributed and apart from measured figures, and names the records behind every
// figure. It reads no clock, database or network (rule 04); the caller supplies the as-of
// instant. After the shift ends as_of is the shift end, so the digest no longer depends on
// when the summary is generated: it changes once, when the shift ends.

const (
	// ShiftSummaryFormulaVersion versions the arithmetic below; ShiftSummaryPolicyRevision
	// versions the readiness policy (reason-to-readiness mapping and the stale window).
	ShiftSummaryFormulaVersion = 1
	// ShiftSummaryRunFormulaVersion is the formula of a summary over an ended shift run: the
	// shift is judged as ended at the run end (as_of, SHIFT_IN_PROGRESS and staleness use it)
	// and records at or after it are flagged. Summaries without a run end keep version 1.
	ShiftSummaryRunFormulaVersion = 2
	ShiftSummaryPolicyRevision    = 1
	// StaleInputWindow: a station job that is not completed and has no record in this
	// window before the shift end is STALE_INPUT.
	StaleInputWindow     = 30 * time.Minute
	shiftSummarySchema   = "ShiftSummaryV1"
	shiftSummaryDigestV1 = 1
)

type SummaryReadiness string

const (
	SummaryReady    SummaryReadiness = "READY"
	SummaryLimited  SummaryReadiness = "LIMITED"
	SummaryNotReady SummaryReadiness = "NOT_READY"
	SummaryInvalid  SummaryReadiness = "INVALID"
)

// SummaryReason is a stable reason code. Its readiness effect is fixed by reasonReadiness.
type SummaryReason string

const (
	ReasonNoRecords          SummaryReason = "NO_RECORDS"
	ReasonNoQuantityRecords  SummaryReason = "NO_QUANTITY_RECORDS"
	ReasonNoCycleTimeRecords SummaryReason = "NO_CYCLE_TIME_RECORDS"
	ReasonStaleInput         SummaryReason = "STALE_INPUT"
	ReasonUnclosedDowntime   SummaryReason = "UNCLOSED_DOWNTIME"
	ReasonLateEntries        SummaryReason = "LATE_ENTRIES"
	ReasonCorrections        SummaryReason = "CORRECTIONS_PRESENT"
	ReasonOverProduction     SummaryReason = "OVER_PRODUCTION"
	ReasonSupersededBinding  SummaryReason = "SUPERSEDED_PLAN_BINDING"
	ReasonNotInApprovedPlan  SummaryReason = "NOT_IN_APPROVED_PLAN"
	ReasonShiftInProgress    SummaryReason = "SHIFT_IN_PROGRESS"
	ReasonQuantityOverflow   SummaryReason = "QUANTITY_OVERFLOW"
	ReasonInvalidValue       SummaryReason = "INVALID_RECORDED_VALUE"
	// ReasonAfterRunEnd: an effective record's event time is at or after the run end (a
	// tablet clock ahead of the server). It is counted as recorded and flagged, never dropped.
	ReasonAfterRunEnd SummaryReason = "RECORDED_AFTER_RUN_END"
)

// SummaryReasons lists every reason code in readiness order.
func SummaryReasons() []SummaryReason {
	return []SummaryReason{ReasonQuantityOverflow, ReasonInvalidValue, ReasonNoRecords, ReasonNoQuantityRecords, ReasonNoCycleTimeRecords, ReasonStaleInput, ReasonUnclosedDowntime, ReasonLateEntries,
		ReasonOverProduction, ReasonSupersededBinding, ReasonNotInApprovedPlan, ReasonShiftInProgress, ReasonCorrections, ReasonAfterRunEnd}
}

// reasonReadiness: a correction is reported but leaves the figure READY; it is the
// effective value, with its chain referenced.
func reasonReadiness(r SummaryReason) SummaryReadiness {
	switch r {
	case ReasonQuantityOverflow, ReasonInvalidValue:
		return SummaryInvalid
	case ReasonNoRecords:
		return SummaryNotReady
	case ReasonCorrections:
		return SummaryReady
	}
	return SummaryLimited
}

func worst(a, b SummaryReadiness) SummaryReadiness {
	rank := map[SummaryReadiness]int{SummaryReady: 0, SummaryLimited: 1, SummaryNotReady: 2, SummaryInvalid: 3}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

// CompletionState is the station job's state as recorded. unknown means no record at all:
// absence of a start record is not proof that work did not start.
type CompletionState string

const (
	CompletionUnknown    CompletionState = "unknown"
	CompletionNotStarted CompletionState = "not_started"
	CompletionInProgress CompletionState = "in_progress"
	CompletionCompleted  CompletionState = "completed"
)

type ShiftSummaryPolicyV1 struct {
	Revision          int   `json:"revision"`
	StaleAfterSeconds int64 `json:"stale_after_seconds"`
}

// ShiftSummaryV1 is the computed summary. It carries no generated ID, creation time or
// actor, so identical inputs always serialize and hash identically.
type ShiftSummaryV1 struct {
	SchemaVersion  int                  `json:"schema_version"`
	FormulaVersion int                  `json:"formula_version"`
	Policy         ShiftSummaryPolicyV1 `json:"policy"`
	PlanID         string               `json:"plan_id"`
	// PlanRevision is the approved revision in force whose targets are the planned figures.
	PlanRevision       int64     `json:"plan_revision"`
	PlanRevisionSHA256 string    `json:"plan_revision_digest" contract:"sha256"`
	LineID             string    `json:"line_id"`
	StartsAt           time.Time `json:"starts_at"`
	EndsAt             time.Time `json:"ends_at"`
	// AsOf is the instant the summary describes: the generation instant during the shift,
	// afterwards the shift end, or the run end when the run ended earlier. ShiftEnded is
	// as_of >= ends_at, or true once the run has ended.
	AsOf       time.Time `json:"as_of"`
	ShiftEnded bool      `json:"shift_ended"`
	// RunEndedAt is when a supervisor ended the shift run; absent before the end and in
	// snapshots stored before shift runs existed, so their digests are unchanged.
	RunEndedAt        *time.Time         `json:"run_ended_at,omitempty"`
	Readiness         SummaryReadiness   `json:"readiness"`
	Reasons           []SummaryReason    `json:"reasons"`
	Jobs              ShiftJobCountsV1   `json:"jobs"`
	Stations          []StationSummaryV1 `json:"stations"`
	Notes             []SummaryNoteV1    `json:"notes"`
	SourceRecordCount int                `json:"source_record_count"`
}

// ShiftJobCountsV1 counts the approved revision's assignments by recorded state;
// remaining is every planned job not recorded as completed.
type ShiftJobCountsV1 struct {
	Planned    int `json:"planned"`
	Completed  int `json:"completed"`
	InProgress int `json:"in_progress"`
	NotStarted int `json:"not_started"`
	Unknown    int `json:"unknown"`
	Remaining  int `json:"remaining"`
}

// StationSummaryV1 is one workstation's job order in this shift. Planned is null when the
// records name a job order the approved revision no longer assigns there.
type StationSummaryV1 struct {
	WorkstationID string            `json:"workstation_id"`
	JobOrderID    string            `json:"job_order_id"`
	Readiness     SummaryReadiness  `json:"readiness"`
	Reasons       []SummaryReason   `json:"reasons"`
	Planned       *PlannedWorkV1    `json:"planned"`
	Recorded      RecordedActualsV1 `json:"recorded"`
	// BoundPlanRevisions and BoundJobOrderRevisions are the revisions the records were
	// entered against; RecordIDs is every record considered, superseded ones included.
	BoundPlanRevisions     []int64  `json:"bound_plan_revisions"`
	BoundJobOrderRevisions []int64  `json:"bound_job_order_revisions"`
	RecordIDs              []string `json:"record_ids"`
}

type PlannedWorkV1 struct {
	JobOrderRevision       int64   `json:"job_order_revision"`
	ItemCode               string  `json:"item_code"`
	TargetQuantity         int64   `json:"target_quantity"`
	TargetCycleTimeSeconds *string `json:"target_cycle_time_seconds" contract:"decimal6"`
}

// RecordedActualsV1 holds only what operators recorded. A null figure is unknown (nothing
// recorded), never zero. Remaining is target minus recorded good and is negative, never
// clamped, when more was recorded than planned.
type RecordedActualsV1 struct {
	CompletionState    CompletionState       `json:"completion_state"`
	CompletionRecordID *string               `json:"completion_record_id"`
	CompletionValues   []EntryValueV1        `json:"completion_values_as_recorded"`
	GoodQuantity       *int64                `json:"good_quantity"`
	RejectQuantity     *int64                `json:"reject_quantity"`
	RemainingQuantity  *int64                `json:"remaining_quantity"`
	OverProduction     bool                  `json:"over_production"`
	QuantityRecordIDs  []string              `json:"quantity_record_ids"`
	CycleTime          *CycleTimeStatsV1     `json:"cycle_time"`
	Quality            QualityAsRecordedV1   `json:"quality"`
	Downtime           []DowntimeIntervalV1  `json:"downtime"`
	ClosedDowntime     string                `json:"closed_downtime_seconds" contract:"decimal6"`
	MaterialExceptions []MaterialExceptionV1 `json:"material_exceptions"`
	LateRecordIDs      []string              `json:"late_record_ids"`
	Corrections        []CorrectionChainV1   `json:"corrections"`
	LastEventAt        *time.Time            `json:"last_event_at"`
}

// CycleTimeStatsV1: sum and count are exact, so mean = sum/count exactly; mean is that
// value rounded half-even to six places.
type CycleTimeStatsV1 struct {
	Count       int      `json:"count"`
	MinSeconds  string   `json:"min_seconds" contract:"decimal6"`
	MaxSeconds  string   `json:"max_seconds" contract:"decimal6"`
	SumSeconds  string   `json:"sum_seconds" contract:"decimal6"`
	MeanSeconds string   `json:"mean_seconds" contract:"decimal6"`
	RecordIDs   []string `json:"record_ids"`
}

// QualityAsRecordedV1 reports measurements and dispositions as operators recorded them. A
// recorded hold is an observation for a later quality decision; nothing here releases.
type QualityAsRecordedV1 struct {
	MeasurementCount       int                       `json:"measurement_count"`
	MeasurementRecordIDs   []string                  `json:"measurement_record_ids"`
	HoldsAsRecorded        int                       `json:"holds_as_recorded"`
	RejectsAsRecorded      int                       `json:"rejects_as_recorded"`
	AcceptsAsRecorded      int                       `json:"accepts_as_recorded"`
	DispositionsAsRecorded []DispositionAsRecordedV1 `json:"dispositions_as_recorded"`
}

type DispositionAsRecordedV1 struct {
	RecordID    string             `json:"record_id"`
	Disposition QualityDisposition `json:"disposition"`
	Comment     string             `json:"comment,omitempty"`
	RecordedBy  ActorRefV1         `json:"recorded_by"`
	EventAt     time.Time          `json:"event_at"`
}

// DowntimeIntervalV1 is a half-open [starts_at, ends_at) pause or block closed by the next
// resume. An open interval has no end and no duration: its end is unknown, not assumed.
type DowntimeIntervalV1 struct {
	Kind            RecordKind `json:"kind"`
	Reason          string     `json:"reason"`
	RecordedBy      ActorRefV1 `json:"recorded_by"`
	StartRecordID   string     `json:"start_record_id"`
	EndRecordID     *string    `json:"end_record_id"`
	StartsAt        time.Time  `json:"starts_at"`
	EndsAt          *time.Time `json:"ends_at"`
	Open            bool       `json:"open"`
	DurationSeconds *string    `json:"duration_seconds" contract:"decimal6"`
}

type MaterialExceptionV1 struct {
	RecordID     string        `json:"record_id"`
	MaterialCode string        `json:"material_code"`
	Issue        MaterialIssue `json:"issue"`
	Comment      string        `json:"comment,omitempty"`
	RecordedBy   ActorRefV1    `json:"recorded_by"`
	EventAt      time.Time     `json:"event_at"`
}

// CorrectionChainV1 names the record in force and every record it replaced, oldest first.
type CorrectionChainV1 struct {
	RecordID   string   `json:"record_id"`
	Supersedes []string `json:"supersedes"`
}

// SummaryNoteV1 is a manual note, attributed and kept apart from measured figures.
type SummaryNoteV1 struct {
	WorkstationID string     `json:"workstation_id"`
	JobOrderID    string     `json:"job_order_id"`
	RecordID      string     `json:"record_id"`
	Comment       string     `json:"comment"`
	RecordedBy    ActorRefV1 `json:"recorded_by"`
	EventAt       time.Time  `json:"event_at"`
	Late          bool       `json:"late"`
}

// ShiftSummaryInput is everything the kernel reads. Approved is the plan revision in
// force; Records are all records bound to the plan, in any order and any plan revision.
// AsOf is the generation instant; a value at or after EndsAt means the shift has ended.
// EndedAt, when set, is the actual end of the shift run: the shift has then ended, and is
// summarized as of the earlier of EndedAt and EndsAt.
type ShiftSummaryInput struct {
	PlanID   string
	LineID   string
	StartsAt time.Time
	EndsAt   time.Time
	AsOf     time.Time
	EndedAt  *time.Time
	Approved ShiftPlanRevisionV1
	Records  []StationRecordV1
}

// end is the instant the shift is judged to end: the planned end, or an earlier run end.
func (in ShiftSummaryInput) end() time.Time {
	if in.EndedAt != nil && in.EndedAt.Before(in.EndsAt) {
		return *in.EndedAt
	}
	return in.EndsAt
}

type stationKey struct{ workstation, jobOrder string }

// SummarizeShift computes the summary and its canonical envelope and digest over the
// tenant and site scope. Records are sorted here, so input order never matters.
func SummarizeShift(sc Scope, in ShiftSummaryInput) (ShiftSummaryV1, []byte, string, error) {
	records := append([]StationRecordV1(nil), in.Records...)
	sort.Slice(records, func(i, j int) bool {
		a, b := records[i], records[j]
		if !a.EventAt.Equal(b.EventAt) {
			return a.EventAt.Before(b.EventAt)
		}
		if !a.ReceivedAt.Equal(b.ReceivedAt) {
			return a.ReceivedAt.Before(b.ReceivedAt)
		}
		return a.ID < b.ID
	})
	byID := map[string]StationRecordV1{}
	superseded := map[string]bool{}
	for _, r := range records {
		byID[r.ID] = r
		if r.Record.CorrectsRecordID != "" {
			superseded[r.Record.CorrectsRecordID] = true
		}
	}
	groups := map[stationKey][]StationRecordV1{}
	for _, r := range records {
		k := stationKey{r.Record.WorkstationID, r.Record.JobOrderID}
		groups[k] = append(groups[k], r)
	}
	planned := map[stationKey]AssignmentV1{}
	for _, a := range in.Approved.Plan.Assignments {
		planned[stationKey{a.WorkstationID, a.JobOrderID}] = a
		if _, ok := groups[stationKey{a.WorkstationID, a.JobOrderID}]; !ok {
			groups[stationKey{a.WorkstationID, a.JobOrderID}] = nil
		}
	}
	keys := make([]stationKey, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].workstation != keys[j].workstation {
			return keys[i].workstation < keys[j].workstation
		}
		return keys[i].jobOrder < keys[j].jobOrder
	})
	asOf, ended := in.AsOf.UTC(), !in.AsOf.Before(in.EndsAt) || in.EndedAt != nil
	if ended {
		asOf = in.end().UTC()
	}
	var runEnded *time.Time
	if in.EndedAt != nil {
		v := in.EndedAt.UTC()
		runEnded = &v
	}
	formula := ShiftSummaryFormulaVersion
	if in.EndedAt != nil {
		formula = ShiftSummaryRunFormulaVersion
	}
	out := ShiftSummaryV1{SchemaVersion: 1, FormulaVersion: formula, Policy: ShiftSummaryPolicyV1{ShiftSummaryPolicyRevision, int64(StaleInputWindow / time.Second)},
		PlanID: in.PlanID, PlanRevision: in.Approved.Revision, PlanRevisionSHA256: in.Approved.CanonicalPayloadSHA256, LineID: in.LineID,
		StartsAt: in.StartsAt.UTC(), EndsAt: in.EndsAt.UTC(), AsOf: asOf, ShiftEnded: ended, RunEndedAt: runEnded, Readiness: SummaryReady, Reasons: []SummaryReason{},
		Stations: []StationSummaryV1{}, Notes: []SummaryNoteV1{}, SourceRecordCount: len(records)}
	for _, k := range keys {
		var assignment *AssignmentV1
		if a, ok := planned[k]; ok {
			assignment = &a
		}
		station, notes := summarizeStation(k, assignment, groups[k], byID, superseded, in, ended)
		out.Stations = append(out.Stations, station)
		out.Notes = append(out.Notes, notes...)
		out.Readiness = worst(out.Readiness, station.Readiness)
		if assignment == nil {
			continue
		}
		out.Jobs.Planned++
		switch station.Recorded.CompletionState {
		case CompletionCompleted:
			out.Jobs.Completed++
		case CompletionInProgress:
			out.Jobs.InProgress++
		case CompletionNotStarted:
			out.Jobs.NotStarted++
		default:
			out.Jobs.Unknown++
		}
	}
	out.Jobs.Remaining = out.Jobs.Planned - out.Jobs.Completed
	// The plan-level reasons explain the roll-up: every station reason once, plus the
	// plan's own. A plan with nothing to summarize is NOT_READY, never READY.
	reasons := map[SummaryReason]bool{}
	for _, st := range out.Stations {
		for _, r := range st.Reasons {
			reasons[r] = true
		}
	}
	if len(out.Stations) == 0 {
		reasons[ReasonNoRecords] = true
	}
	if !ended {
		reasons[ReasonShiftInProgress] = true
	}
	for r := range reasons {
		out.Reasons = append(out.Reasons, r)
		out.Readiness = worst(out.Readiness, reasonReadiness(r))
	}
	sort.Slice(out.Reasons, func(i, j int) bool { return out.Reasons[i] < out.Reasons[j] })
	sort.SliceStable(out.Notes, func(i, j int) bool {
		if !out.Notes[i].EventAt.Equal(out.Notes[j].EventAt) {
			return out.Notes[i].EventAt.Before(out.Notes[j].EventAt)
		}
		return out.Notes[i].RecordID < out.Notes[j].RecordID
	})
	encoded, digest, err := canonical.Envelope(shiftSummarySchema, shiftSummaryDigestV1, sc.TenantID, sc.SiteID, out)
	return out, encoded, digest, err
}

func summarizeStation(k stationKey, assignment *AssignmentV1, records []StationRecordV1, byID map[string]StationRecordV1, superseded map[string]bool, in ShiftSummaryInput, ended bool) (StationSummaryV1, []SummaryNoteV1) {
	s := StationSummaryV1{WorkstationID: k.workstation, JobOrderID: k.jobOrder, Reasons: []SummaryReason{}, BoundPlanRevisions: []int64{}, BoundJobOrderRevisions: []int64{}, RecordIDs: []string{}}
	rec := RecordedActualsV1{CompletionState: CompletionUnknown, CompletionValues: []EntryValueV1{}, QuantityRecordIDs: []string{}, Downtime: []DowntimeIntervalV1{}, ClosedDowntime: "0",
		MaterialExceptions: []MaterialExceptionV1{}, LateRecordIDs: []string{}, Corrections: []CorrectionChainV1{},
		Quality: QualityAsRecordedV1{MeasurementRecordIDs: []string{}, DispositionsAsRecorded: []DispositionAsRecordedV1{}}}
	reasons := map[SummaryReason]bool{}
	if assignment != nil {
		s.Planned = &PlannedWorkV1{JobOrderRevision: assignment.JobOrderRevision, ItemCode: assignment.ItemCode, TargetQuantity: assignment.TargetQuantity}
		if assignment.TargetCycleTimeSeconds != "" {
			target := assignment.TargetCycleTimeSeconds
			s.Planned.TargetCycleTimeSeconds = &target
		}
	} else {
		reasons[ReasonNotInApprovedPlan] = true
	}
	notes := []SummaryNoteV1{}
	planRevisions, orderRevisions := map[int64]bool{}, map[int64]bool{}
	var good, reject int64
	var quantities bool
	var cycles []*big.Rat
	var cycleIDs []string
	var minCycle, maxCycle *big.Rat
	var minText, maxText string
	var open *DowntimeIntervalV1
	closed := new(big.Rat)
	started := false
	// A stored value that does not parse or render makes
	// the station INVALID instead of being skipped silently.
	render := func(v *big.Rat) string {
		text, ok := exactDecimal(v)
		if !ok {
			reasons[ReasonInvalidValue] = true
		}
		return text
	}
	for _, r := range records {
		s.RecordIDs = append(s.RecordIDs, r.ID)
		// A superseded record is history: it neither binds, flags nor keeps a job fresh.
		if superseded[r.ID] {
			continue
		}
		if in.EndedAt != nil && !r.EventAt.Before(*in.EndedAt) {
			reasons[ReasonAfterRunEnd] = true
		}
		planRevisions[r.PlanRevision], orderRevisions[r.Record.JobOrderRevision] = true, true
		if r.PlanRevision != in.Approved.Revision {
			reasons[ReasonSupersededBinding] = true
		}
		if r.Late {
			rec.LateRecordIDs = append(rec.LateRecordIDs, r.ID)
			reasons[ReasonLateEntries] = true
		}
		// A note is not station data: it never keeps a job fresh.
		if last := r.EventAt.UTC(); r.Record.Kind != KindNote && (rec.LastEventAt == nil || last.After(*rec.LastEventAt)) {
			rec.LastEventAt = &last
		}
		// The record in force: walk back to every record it replaced.
		if r.Record.CorrectsRecordID != "" {
			chain := []string{}
			seen := map[string]bool{r.ID: true}
			for id := r.Record.CorrectsRecordID; id != "" && !seen[id]; id = byID[id].Record.CorrectsRecordID {
				seen[id] = true
				chain = append([]string{id}, chain...)
			}
			rec.Corrections = append(rec.Corrections, CorrectionChainV1{RecordID: r.ID, Supersedes: chain})
			reasons[ReasonCorrections] = true
		}
		b := r.Record
		switch b.Kind {
		case KindStarted:
			started = true
		case KindPaused, KindBlocked:
			open = &DowntimeIntervalV1{Kind: b.Kind, Reason: b.Comment, RecordedBy: r.RecordedBy, StartRecordID: r.ID, StartsAt: r.EventAt.UTC(), Open: true}
		case KindResumed:
			if open != nil {
				id, end := r.ID, r.EventAt.UTC()
				// Event times carry microsecond precision, so the duration is exact.
				duration := big.NewRat(end.Sub(open.StartsAt).Microseconds(), 1_000_000)
				closed.Add(closed, duration)
				text := render(duration)
				open.EndRecordID, open.EndsAt, open.Open, open.DurationSeconds = &id, &end, false, &text
				rec.Downtime = append(rec.Downtime, *open)
				open = nil
			}
		case KindCompletionEvidence:
			id := r.ID
			rec.CompletionRecordID, rec.CompletionValues = &id, append([]EntryValueV1{}, b.Values...)
		case KindQuantity:
			quantities = true
			rec.QuantityRecordIDs = append(rec.QuantityRecordIDs, r.ID)
			if good > math.MaxInt64-*b.GoodQuantity || reject > math.MaxInt64-*b.RejectQuantity {
				reasons[ReasonQuantityOverflow] = true
				continue
			}
			// ponytail: a quantity record is an increment; the recorded good is their sum.
			good, reject = good+*b.GoodQuantity, reject+*b.RejectQuantity
		case KindCycleTime:
			v, ok := new(big.Rat).SetString(b.CycleTimeSeconds)
			if !ok || v.Sign() <= 0 {
				reasons[ReasonInvalidValue] = true
				continue
			}
			if minCycle == nil || v.Cmp(minCycle) < 0 {
				minCycle, minText = v, b.CycleTimeSeconds
			}
			if maxCycle == nil || v.Cmp(maxCycle) > 0 {
				maxCycle, maxText = v, b.CycleTimeSeconds
			}
			cycles, cycleIDs = append(cycles, v), append(cycleIDs, r.ID)
		case KindQualityMeasurement:
			rec.Quality.MeasurementCount++
			rec.Quality.MeasurementRecordIDs = append(rec.Quality.MeasurementRecordIDs, r.ID)
		case KindQualityDisposition:
			switch b.Disposition {
			case DispositionHold:
				rec.Quality.HoldsAsRecorded++
			case DispositionReject:
				rec.Quality.RejectsAsRecorded++
			case DispositionAccept:
				rec.Quality.AcceptsAsRecorded++
			}
			rec.Quality.DispositionsAsRecorded = append(rec.Quality.DispositionsAsRecorded, DispositionAsRecordedV1{r.ID, b.Disposition, b.Comment, r.RecordedBy, r.EventAt.UTC()})
		case KindMaterialException:
			rec.MaterialExceptions = append(rec.MaterialExceptions, MaterialExceptionV1{r.ID, b.MaterialCode, b.MaterialIssue, b.Comment, r.RecordedBy, r.EventAt.UTC()})
		case KindNote:
			notes = append(notes, SummaryNoteV1{k.workstation, k.jobOrder, r.ID, b.Comment, r.RecordedBy, r.EventAt.UTC(), r.Late})
		}
	}
	if open != nil {
		rec.Downtime = append(rec.Downtime, *open)
		reasons[ReasonUnclosedDowntime] = true
	}
	rec.ClosedDowntime = render(closed)
	switch {
	case len(records) == 0:
		reasons[ReasonNoRecords] = true
	case rec.CompletionRecordID != nil:
		rec.CompletionState = CompletionCompleted
	case started:
		rec.CompletionState = CompletionInProgress
	default:
		rec.CompletionState = CompletionNotStarted
	}
	if len(records) > 0 && !quantities {
		reasons[ReasonNoQuantityRecords] = true
	}
	if quantities && !reasons[ReasonQuantityOverflow] {
		rec.GoodQuantity, rec.RejectQuantity = &good, &reject
		if s.Planned != nil {
			remaining := s.Planned.TargetQuantity - good
			rec.RemainingQuantity, rec.OverProduction = &remaining, remaining < 0
			if remaining < 0 {
				reasons[ReasonOverProduction] = true
			}
		}
	}
	if len(cycles) > 0 {
		sum := new(big.Rat)
		for _, v := range cycles {
			sum.Add(sum, v)
		}
		rec.CycleTime = &CycleTimeStatsV1{Count: len(cycles), MinSeconds: minText, MaxSeconds: maxText, SumSeconds: render(sum),
			MeanSeconds: render(new(big.Rat).Quo(sum, big.NewRat(int64(len(cycles)), 1))), RecordIDs: cycleIDs}
	} else if len(records) > 0 {
		reasons[ReasonNoCycleTimeRecords] = true
	}
	// Staleness is judged at the shift end from effective station data (notes and superseded
	// records excluded); during the shift the summary says it is in progress instead.
	if ended && len(records) > 0 && rec.CompletionState != CompletionCompleted && (rec.LastEventAt == nil || rec.LastEventAt.Before(in.end().Add(-StaleInputWindow))) {
		reasons[ReasonStaleInput] = true
	}
	s.Readiness = SummaryReady
	for _, r := range SummaryReasons() {
		if reasons[r] {
			s.Reasons = append(s.Reasons, r)
			s.Readiness = worst(s.Readiness, reasonReadiness(r))
		}
	}
	for _, v := range sortedKeys(planRevisions) {
		s.BoundPlanRevisions = append(s.BoundPlanRevisions, v)
	}
	for _, v := range sortedKeys(orderRevisions) {
		s.BoundJobOrderRevisions = append(s.BoundJobOrderRevisions, v)
	}
	s.Recorded = rec
	return s, notes
}

// exactDecimal renders a nonnegative exact value half-even to six places in the canonical
// decimal form ("2.7", "3", never "2.700000"); false for a value it cannot render.
func exactDecimal(v *big.Rat) (string, bool) {
	text, err := production.Decimal6HalfEven(v)
	if err != nil {
		return "0", false
	}
	for text[len(text)-1] == '0' {
		text = text[:len(text)-1]
	}
	if text[len(text)-1] == '.' {
		text = text[:len(text)-1]
	}
	return text, true
}

func sortedKeys(m map[int64]bool) []int64 {
	out := make([]int64, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
