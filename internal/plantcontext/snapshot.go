// Package plantcontext assembles PlantContextSnapshotV1: a reproducible, immutable
// production-exception context for one order, asset and 15-minute window. Assembly is
// a pure function of resolved inputs; it reads no clock, network or database and never
// infers root cause. Missing or unproven facts are NOT_READY, never zero.
package plantcontext

import (
	"errors"
	"math/big"
	"sort"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/canonical"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/limits"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/production"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/strictjson"
)

const (
	SchemaName = "PlantContextSnapshotV1"
	// SchemaVersion 2 adds record_id to every source record reference (WIS-272 contract).
	// The digest covers it, so contexts created under version 1 keep their stored digest.
	SchemaVersion = 2
	// FormulaVersion names the expected/actual/deficit arithmetic in internal/production.
	FormulaVersion = "production-window-v1"
	// ReadinessPolicyRevision versions the defaults below and limits.MaxHeartbeatAge; changing
	// them creates new contexts.
	ReadinessPolicyRevision = 1
	// EvaluationDelay is the minimum time after window end before a window may be READY.
	EvaluationDelay = 120 * time.Second
	// MaxRecords bounds the records one context may read; more is WINDOW_TOO_LARGE.
	MaxRecords = 10000
)

const (
	ReadyState   = "READY"
	LimitedState = "LIMITED"
	NotReady     = "NOT_READY"
)

var ErrWindowTooLarge = errors.New("context window exceeds the record limit")

type Window struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

// PlanRef pins the exact approved plan revision and the approval that admitted it.
type PlanRef struct {
	PlanRevisionID         string `json:"plan_revision_id"`
	PlanRevision           int64  `json:"plan_revision"`
	ApprovalID             string `json:"approval_id"`
	CanonicalPayloadDigest string `json:"canonical_payload_digest"`
}

// RecordRef pins one source record revision. (source_id, source_sequence) is the
// record's durable identity; sha256 is its content digest.
type RecordRef struct {
	// RecordID is a deterministic identifier of the source record: a name-based UUID over
	// tenant, site, source and source sequence. Records have no stored UUID of their own.
	RecordID       string `json:"record_id"`
	SourceID       string `json:"source_id"`
	SourceSequence int64  `json:"source_sequence"`
	Kind           string `json:"kind"`
	ExternalKey    string `json:"external_key"`
	Revision       int64  `json:"revision"`
	SHA256         string `json:"sha256"`
}

type MappingRef struct {
	SourceID        string `json:"source_id"`
	MappingRevision int64  `json:"mapping_revision"`
}

type PolicyRef struct {
	SourceID             string `json:"source_id"`
	SourceRevision       int64  `json:"source_revision"`
	AccessPolicyRevision int64  `json:"access_policy_revision"`
}

// PlanInput is the eligible approved plan resolved for the order and asset.
type PlanInput struct {
	Ref  PlanRef
	Body production.PlanBody
}

// DeltaRecord is a stored, non-quarantined production_delta record naming the order
// and asset. Entity IDs are what the source's approved mapping resolved its keys to.
type DeltaRecord struct {
	Ref                          RecordRef
	Payload                      []byte
	AssetEntityID, OrderEntityID string
	// Quarantine is the intake quarantine reason, empty when admitted. DUPLICATE_EVENT
	// retransmits are never loaded; any other quarantined revision blocks its chain.
	Quarantine string
}

type WatermarkInput struct {
	Ref     RecordRef
	Payload []byte
}

// ProgressInput is the source's latest folded completeness and liveness.
type ProgressInput struct {
	CompleteSequence        int64
	ClockQuality            string
	LastHeartbeatReceivedAt *time.Time
	Watermark               *WatermarkInput
}

type SourceInput struct {
	SourceID             string
	SourceRevision       int64
	MappingRevision      int64
	AccessPolicyRevision int64
	// Approved: enabled, and the current review approves the current source and mapping
	// revisions with current mapping targets.
	Approved          bool
	ScopeAllowsDeltas bool
	// ScopeAllowsMachineState: the reviewed scope covers machine_state records (WIS-276
	// machine-fault evidence). Not part of the snapshot or its digest.
	ScopeAllowsMachineState bool
	FreshnessSeconds        int64
	Progress                *ProgressInput
}

type Inputs struct {
	OrderID, AssetID      string
	Window                Window
	ProcessSnapshotID     string
	ProcessSnapshotSHA256 string
	// Plan is nil when no eligible approved plan exists; PlanReason then says why.
	Plan       *PlanInput
	PlanReason string
	Deltas     []DeltaRecord
	Sources    map[string]SourceInput
	// EvaluatedAt is supplied by the caller; assembly never reads a clock.
	EvaluatedAt time.Time
}

// Snapshot is PlantContextSnapshotV1. Values are rounded half-even to six places for
// display; the *_exact fields preserve the exact decimal inputs and results.
type Snapshot struct {
	ContextID               string       `json:"context_id"`
	Revision                int64        `json:"revision"`
	SiteID                  string       `json:"site_id"`
	OrderID                 string       `json:"order_id"`
	AssetID                 string       `json:"asset_id"`
	Window                  Window       `json:"window"`
	ApprovedPlanRevision    *PlanRef     `json:"approved_plan_revision"`
	ProcessSnapshotID       string       `json:"process_snapshot_id"`
	ProcessSnapshotSHA256   string       `json:"process_snapshot_sha256"`
	SourceRecordRefs        []RecordRef  `json:"source_record_refs"`
	MappingRevisionIDs      []MappingRef `json:"mapping_revision_ids"`
	SourcePolicyRevisionIDs []PolicyRef  `json:"source_policy_revision_ids"`
	CreatedAt               time.Time    `json:"created_at"`
	Readiness               string       `json:"readiness"`
	ReasonCodes             []string     `json:"reason_codes"`
	ExpectedGoodUnits       *string      `json:"expected_good_units"`
	ActualGoodUnits         *string      `json:"actual_good_units"`
	DeficitGoodUnits        *string      `json:"deficit_good_units"`
	ExpectedExact           *string      `json:"expected_good_units_exact"`
	ActualExact             *string      `json:"actual_good_units_exact"`
	DeficitExact            *string      `json:"deficit_good_units_exact"`
	Unit                    string       `json:"unit"`
	MissingCapabilities     []string     `json:"missing_capabilities"`
	FormulaVersion          string       `json:"formula_version"`
	ReadinessPolicyRevision int64        `json:"readiness_policy_revision"`
	SupersedesContextID     *string      `json:"supersedes_context_id"`
	Digest                  string       `json:"digest"`
}

// digestBody is the hashed subset in fixed schema order. It excludes the generated
// context_id, created_at, supersedes_context_id and the digest itself.
type digestBody struct {
	OrderID                 string       `json:"order_id"`
	AssetID                 string       `json:"asset_id"`
	Window                  Window       `json:"window"`
	ApprovedPlanRevision    *PlanRef     `json:"approved_plan_revision"`
	ProcessSnapshotID       string       `json:"process_snapshot_id"`
	ProcessSnapshotSHA256   string       `json:"process_snapshot_sha256"`
	SourceRecordRefs        []RecordRef  `json:"source_record_refs"`
	MappingRevisionIDs      []MappingRef `json:"mapping_revision_ids"`
	SourcePolicyRevisionIDs []PolicyRef  `json:"source_policy_revision_ids"`
	Readiness               string       `json:"readiness"`
	ReasonCodes             []string     `json:"reason_codes"`
	ExpectedExact           *string      `json:"expected_good_units_exact"`
	ActualExact             *string      `json:"actual_good_units_exact"`
	DeficitExact            *string      `json:"deficit_good_units_exact"`
	Unit                    string       `json:"unit"`
	MissingCapabilities     []string     `json:"missing_capabilities"`
	FormulaVersion          string       `json:"formula_version"`
	ReadinessPolicyRevision int64        `json:"readiness_policy_revision"`
}

type watermarkPayload struct {
	CompleteThrough string `json:"complete_through"`
	ThroughSequence int64  `json:"through_sequence"`
}

type reasons map[string]bool

func (r reasons) list() []string {
	out := make([]string, 0, len(r))
	for code := range r {
		out = append(out, code)
	}
	sort.Strings(out)
	return out
}

// parseUTC accepts RFC3339 with any explicit offset and returns the UTC instant. Assemble
// stores the normalized form, so offset spellings of one instant produce one digest.
func parseUTC(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

// Assemble derives the snapshot body and digest from resolved inputs. The caller
// assigns context_id, created_at and supersedes_context_id.
func Assemble(tenantID, siteID string, in Inputs) (Snapshot, error) {
	start, okStart := parseUTC(in.Window.Start)
	end, okEnd := parseUTC(in.Window.End)
	if !okStart || !okEnd || start.Unix()%900 != 0 || start.Nanosecond() != 0 || end.Sub(start) != 15*time.Minute {
		return Snapshot{}, errors.New("invalid context window")
	}
	in.Window = Window{Start: start.Format(time.RFC3339Nano), End: end.Format(time.RFC3339Nano)}
	why := reasons{}
	out := Snapshot{
		Revision: 1, SiteID: siteID, OrderID: in.OrderID, AssetID: in.AssetID, Window: in.Window,
		ProcessSnapshotID: in.ProcessSnapshotID, ProcessSnapshotSHA256: in.ProcessSnapshotSHA256,
		SourceRecordRefs: []RecordRef{}, MappingRevisionIDs: []MappingRef{}, SourcePolicyRevisionIDs: []PolicyRef{},
		Unit: "count", MissingCapabilities: []string{}, FormulaVersion: FormulaVersion, ReadinessPolicyRevision: ReadinessPolicyRevision,
	}
	if in.EvaluatedAt.Before(end.Add(EvaluationDelay)) {
		why["WINDOW_NOT_ELAPSED"] = true
	}
	var expected *big.Rat
	noPlannedProduction := false
	if in.Plan == nil {
		code := in.PlanReason
		if code == "" {
			code = "NO_APPROVED_PLAN"
		}
		why[code] = true
	} else {
		ref := in.Plan.Ref
		out.ApprovedPlanRevision = &ref
		productive, err := production.ProductiveSeconds(in.Plan.Body, in.Window.Start, in.Window.End)
		switch {
		case errors.Is(err, production.ErrPlanDoesNotCoverWindow):
			why["PLAN_NOT_COVERING_WINDOW"] = true
		case err != nil:
			why["PLAN_INVALID"] = true
		case productive.Sign() == 0:
			noPlannedProduction = true
			expected = new(big.Rat)
		default:
			expected, err = production.ExpectedGoodUnits(in.Plan.Body.WithApproval(ref.ApprovalID), in.Window.Start, in.Window.End)
			if err != nil {
				why["PLAN_INVALID"] = true
				expected = nil
			}
		}
	}
	var actual *big.Rat
	if noPlannedProduction {
		// No planned productive time: there is nothing to fall short of, and no actuals
		// are required. This is LIMITED, never an exception or a fabricated zero actual.
		why["NO_PLANNED_PRODUCTION"] = true
	} else {
		actual = assembleActuals(in, start, end, why, &out)
	}
	sortRefs(&out)
	out.ReasonCodes = why.list()
	switch {
	case len(why) == 0:
		out.Readiness = ReadyState
	case noPlannedProduction && len(why) == 1:
		out.Readiness = LimitedState
	default:
		out.Readiness = NotReady
	}
	if expected != nil && !why["PLAN_INVALID"] {
		if err := setValue(expected, &out.ExpectedGoodUnits, &out.ExpectedExact); err != nil {
			return Snapshot{}, err
		}
	}
	if out.Readiness == ReadyState && expected != nil && actual != nil {
		deficit := new(big.Rat).Sub(expected, actual)
		if deficit.Sign() < 0 {
			deficit = new(big.Rat)
		}
		if err := setValue(actual, &out.ActualGoodUnits, &out.ActualExact); err != nil {
			return Snapshot{}, err
		}
		if err := setValue(deficit, &out.DeficitGoodUnits, &out.DeficitExact); err != nil {
			return Snapshot{}, err
		}
	}
	_, digest, err := canonical.Envelope(SchemaName, SchemaVersion, tenantID, siteID, digestBody{
		OrderID: out.OrderID, AssetID: out.AssetID, Window: out.Window, ApprovedPlanRevision: out.ApprovedPlanRevision,
		ProcessSnapshotID: out.ProcessSnapshotID, ProcessSnapshotSHA256: out.ProcessSnapshotSHA256,
		SourceRecordRefs: out.SourceRecordRefs, MappingRevisionIDs: out.MappingRevisionIDs, SourcePolicyRevisionIDs: out.SourcePolicyRevisionIDs,
		Readiness: out.Readiness, ReasonCodes: out.ReasonCodes, ExpectedExact: out.ExpectedExact, ActualExact: out.ActualExact, DeficitExact: out.DeficitExact,
		Unit: out.Unit, MissingCapabilities: out.MissingCapabilities, FormulaVersion: out.FormulaVersion, ReadinessPolicyRevision: out.ReadinessPolicyRevision,
	})
	if err != nil {
		return Snapshot{}, err
	}
	out.Digest = digest
	return out, nil
}

// assembleActuals selects the latest revision of each delta, validates scope, window
// alignment, source authority, completeness and liveness, and sums exact counts. Any
// unproven condition adds a reason and leaves the actual unknown.
func assembleActuals(in Inputs, start, end time.Time, why reasons, out *Snapshot) *big.Rat {
	type chain struct{ records []DeltaRecord }
	chains := map[string]*chain{}
	for _, record := range in.Deltas {
		key := record.Ref.SourceID + "\x00" + record.Ref.ExternalKey
		if chains[key] == nil {
			chains[key] = &chain{}
		}
		chains[key].records = append(chains[key].records, record)
	}
	keys := make([]string, 0, len(chains))
	for key := range chains {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var deltas []production.Delta
	used := map[string]bool{}
	for _, key := range keys {
		records := chains[key].records
		sort.Slice(records, func(i, j int) bool { return records[i].Ref.Revision < records[j].Ref.Revision })
		// Corrections append contiguous revisions 1..n; the latest replaces earlier ones.
		quarantined := false
		for i, record := range records {
			if record.Ref.Revision != int64(i+1) {
				why["CORRECTION_GAP"] = true
			}
			quarantined = quarantined || record.Quarantine != ""
		}
		if quarantined {
			// A quarantined revision means the chain's true latest value is unknown;
			// falling back to an earlier admitted revision would report a stale value.
			why["CORRECTION_QUARANTINED"] = true
			continue
		}
		latest := records[len(records)-1]
		delta, err := production.DecodeDelta(latest.Payload)
		if err != nil {
			why["INVALID_DELTA"] = true
			continue
		}
		if delta.OrderID != in.OrderID || delta.AssetID != in.AssetID {
			// A correction moved the fact to another order/asset: it no longer contributes.
			continue
		}
		if latest.AssetEntityID != delta.AssetID || latest.OrderEntityID != delta.OrderID {
			why["MAPPING_MISMATCH"] = true
			continue
		}
		ds, okS := parseUTC(delta.IntervalStart)
		de, okE := parseUTC(delta.IntervalEnd)
		if !okS || !okE {
			why["INVALID_DELTA"] = true
			continue
		}
		if !ds.Before(end) || !de.After(start) {
			continue // a correction moved it outside this window
		}
		if ds.Before(start) || de.After(end) {
			// Never split a count proportionally across a window boundary.
			why["BOUNDARY_NOT_ALIGNED"] = true
			continue
		}
		deltas = append(deltas, delta)
		out.SourceRecordRefs = append(out.SourceRecordRefs, latest.Ref)
		used[latest.Ref.SourceID] = true
	}
	if len(used) == 0 {
		why["MISSING_ACTUALS"] = true
		return nil
	}
	if len(used) > 1 {
		// Overlapping authoritative sources for the same scope are a conflict, never
		// last-writer-wins.
		why["SOURCE_CONFLICT"] = true
	}
	maxSequence := map[string]int64{}
	for _, ref := range out.SourceRecordRefs {
		if ref.SourceSequence > maxSequence[ref.SourceID] {
			maxSequence[ref.SourceID] = ref.SourceSequence
		}
	}
	for sourceID := range used {
		source, ok := in.Sources[sourceID]
		if !ok {
			why["SOURCE_NOT_APPROVED"] = true
			continue
		}
		out.MappingRevisionIDs = append(out.MappingRevisionIDs, MappingRef{SourceID: sourceID, MappingRevision: source.MappingRevision})
		out.SourcePolicyRevisionIDs = append(out.SourcePolicyRevisionIDs, PolicyRef{SourceID: sourceID, SourceRevision: source.SourceRevision, AccessPolicyRevision: source.AccessPolicyRevision})
		if !source.Approved {
			why["SOURCE_NOT_APPROVED"] = true
		}
		if !source.ScopeAllowsDeltas {
			why["SOURCE_OUT_OF_SCOPE"] = true
		}
		progress := source.Progress
		if progress == nil {
			why["NOT_PROCESSED"] = true
			continue
		}
		if maxSequence[sourceID] > progress.CompleteSequence {
			why["INCOMPLETE_SEQUENCE"] = true
		}
		switch {
		case progress.LastHeartbeatReceivedAt == nil:
			why["NO_HEARTBEAT"] = true
		// Liveness is judged at the window's end, not at request time: the source must have
		// heartbeated no earlier than end - limit. The latest heartbeat only moves forward, so
		// the same inputs give the same readiness and digest whenever they are evaluated
		// (rule 04); watermark and sequence checks prove completeness.
		case progress.LastHeartbeatReceivedAt.Before(end.Add(-limits.HeartbeatLimit(source.FreshnessSeconds))):
			why["HEARTBEAT_STALE"] = true
		}
		switch progress.ClockQuality {
		case "ok":
		case "suspect":
			why["CLOCK_SUSPECT"] = true
		default:
			why["CLOCK_UNKNOWN"] = true
		}
		if progress.Watermark == nil {
			why["NO_WATERMARK"] = true
			continue
		}
		var mark watermarkPayload
		completeThrough, valid := time.Time{}, false
		if strictjson.Decode(progress.Watermark.Payload, &mark, 4096) == nil && mark.ThroughSequence == progress.Watermark.Ref.SourceSequence-1 {
			completeThrough, valid = parseUTC(mark.CompleteThrough)
		}
		if !valid {
			why["WATERMARK_INVALID"] = true
			continue
		}
		out.SourceRecordRefs = append(out.SourceRecordRefs, progress.Watermark.Ref)
		if completeThrough.Before(end) {
			why["WATERMARK_BEHIND"] = true
		}
	}
	intervals := make([][2]time.Time, 0, len(deltas))
	for _, d := range deltas {
		ds, _ := parseUTC(d.IntervalStart)
		de, _ := parseUTC(d.IntervalEnd)
		intervals = append(intervals, [2]time.Time{ds, de})
	}
	sort.Slice(intervals, func(i, j int) bool { return intervals[i][0].Before(intervals[j][0]) })
	for i := 1; i < len(intervals); i++ {
		if intervals[i][0].Before(intervals[i-1][1]) {
			why["DELTA_OVERLAP"] = true
			return nil
		}
	}
	if in.Plan == nil || len(why) > 0 {
		return nil
	}
	actual, err := production.SumGoodUnitsDeltas(in.Plan.Body.WithApproval(in.Plan.Ref.ApprovalID), in.Window.Start, in.Window.End, deltas)
	if err != nil {
		// Deltas must cover every planned productive instant, with explicit zero intervals.
		why["MISSING_ACTUALS"] = true
		return nil
	}
	return actual
}

// DeltaRefs returns the production_delta records a snapshot used, in stored order.
func (s Snapshot) DeltaRefs() []RecordRef {
	refs := make([]RecordRef, 0, len(s.SourceRecordRefs))
	for _, ref := range s.SourceRecordRefs {
		if ref.Kind == "production_delta" {
			refs = append(refs, ref)
		}
	}
	return refs
}

func sortRefs(out *Snapshot) {
	sort.Slice(out.SourceRecordRefs, func(i, j int) bool {
		a, b := out.SourceRecordRefs[i], out.SourceRecordRefs[j]
		if a.SourceID != b.SourceID {
			return a.SourceID < b.SourceID
		}
		return a.SourceSequence < b.SourceSequence
	})
	sort.Slice(out.MappingRevisionIDs, func(i, j int) bool { return out.MappingRevisionIDs[i].SourceID < out.MappingRevisionIDs[j].SourceID })
	sort.Slice(out.SourcePolicyRevisionIDs, func(i, j int) bool {
		return out.SourcePolicyRevisionIDs[i].SourceID < out.SourcePolicyRevisionIDs[j].SourceID
	})
}

func setValue(value *big.Rat, rounded, exact **string) error {
	r, err := production.Decimal6HalfEven(value)
	if err != nil {
		return err
	}
	e, err := production.ExactDecimal(value)
	if err != nil {
		return err
	}
	*rounded, *exact = &r, &e
	return nil
}

// ShortfallEvaluation is the two-window rule WIS-273 consumes to open a case.
type ShortfallEvaluation struct {
	Qualifies   bool   `json:"qualifies"`
	RuleVersion int    `json:"rule_version"`
	Reason      string `json:"reason"`
}

// EvaluateShortfall applies deficit >= max(5, 0.10*expected) to two consecutive,
// complete (READY) windows of the same order, asset and process snapshot, using exact
// values. It is pure: callers must pass contexts whose current_status is eligible.
func EvaluateShortfall(previous, current Snapshot) ShortfallEvaluation {
	result := ShortfallEvaluation{RuleVersion: production.ShortfallRuleVersion}
	switch {
	case previous.SiteID != current.SiteID || previous.OrderID != current.OrderID || previous.AssetID != current.AssetID:
		result.Reason = "DIFFERENT_SCOPE"
	case previous.ProcessSnapshotID != current.ProcessSnapshotID:
		result.Reason = "DIFFERENT_PROCESS_SNAPSHOT"
	case previous.Window.End != current.Window.Start:
		result.Reason = "NOT_CONSECUTIVE"
	case previous.Readiness != ReadyState || current.Readiness != ReadyState:
		result.Reason = "WINDOW_NOT_READY"
	default:
		for _, s := range []Snapshot{previous, current} {
			expected, okE := exactRat(s.ExpectedExact)
			deficit, okD := exactRat(s.DeficitExact)
			if !okE || !okD {
				result.Reason = "WINDOW_NOT_READY"
				return result
			}
			if !production.WindowQualifies(expected, deficit) {
				result.Reason = "BELOW_THRESHOLD"
				return result
			}
		}
		result.Qualifies = true
	}
	return result
}

func exactRat(s *string) (*big.Rat, bool) {
	if s == nil {
		return nil, false
	}
	return new(big.Rat).SetString(*s)
}
