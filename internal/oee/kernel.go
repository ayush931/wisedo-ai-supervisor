package oee

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"time"

	"github.com/google/uuid"
)

// ErrInvalidKPIInput means the kernel input breaks the calculation contract (a window that
// does not match the timeline, a segment or count outside the window, an unknown state).
// Contract errors are returned, not converted into a readiness status.
var ErrInvalidKPIInput = errors.New("invalid KPI calculation input")

// ErrIncompatibleChildren means a rollup mixes results that were not calculated under the
// same formula, kernel and policy, or contains the same result twice.
var ErrIncompatibleChildren = errors.New("rollup children are not comparable")

// RoundingMode defines the rounding policy for decimal results.
type RoundingMode string

const (
	RoundHalfEven RoundingMode = "HALF_EVEN" // Banker's rounding (ties to even)
	RoundHalfUp   RoundingMode = "HALF_UP"   // Arithmetic rounding (ties away from zero)
	RoundFloor    RoundingMode = "FLOOR"     // Towards -infinity
	RoundCeiling  RoundingMode = "CEILING"   // Towards +infinity
)

// ZeroDenomBehavior defines the policy behavior when a ratio denominator is zero.
type ZeroDenomBehavior string

const (
	ZeroDenomNotReady ZeroDenomBehavior = "NOT_READY" // Mark ratio not ready and flag reason
	ZeroDenomInvalid  ZeroDenomBehavior = "INVALID"   // Mark calculation invalid
	ZeroDenomZero     ZeroDenomBehavior = "ZERO"      // Emit zero value and flag limited/not ready
)

// PerformanceCapBehavior defines how the kernel handles Performance > 100%.
// Under ADR 0001/0002 and Industry Standards, performance is NEVER silently clamped to 1.0.
type PerformanceCapBehavior string

const (
	CapAllowAndRetain PerformanceCapBehavior = "ALLOW_AND_RETAIN" // Retain raw value, flag LIMITED/warn
	CapFlagInvalid    PerformanceCapBehavior = "FLAG_INVALID"     // Reject calculation as INVALID
)

// StateBucket is where a timeline state's duration counts (ADR 0005 Sec. 2: the mapping is
// policy, never hardcoded in application logic).
type StateBucket string

const (
	BucketRun      StateBucket = "RUN"      // operating time
	BucketStop     StateBucket = "STOP"     // planned production time lost to stops
	BucketExcluded StateBucket = "EXCLUDED" // planned exclusion (schedule loss)
)

// bucketedStates are the owner-fact states a policy must map to a bucket.
var bucketedStates = []TimelineState{StateRunning, StateUnplannedDowntime, StateChangeover, StateStandby, StatePlannedBreak, StateShutdownExclusion}

func isBucketedState(s TimelineState) bool {
	for _, b := range bucketedStates {
		if b == s {
			return true
		}
	}
	return false
}

// DefaultStateBuckets is the ISO 22400 / Nakajima mapping the default policy uses.
func DefaultStateBuckets() map[TimelineState]StateBucket {
	return map[TimelineState]StateBucket{
		StateRunning:           BucketRun,
		StateUnplannedDowntime: BucketStop,
		StateChangeover:        BucketStop,
		StateStandby:           BucketStop,
		StatePlannedBreak:      BucketExcluded,
		StateShutdownExclusion: BucketExcluded,
	}
}

// KPIPolicy defines the exact versioned rules for pure deterministic calculation.
type KPIPolicy struct {
	PolicyRevisionID      uuid.UUID                     `json:"policy_revision_id"`
	PolicyVersion         string                        `json:"policy_version"`
	FormulaVersion        string                        `json:"formula_version"`       // e.g. "2026.1-nakajima-iso22400"
	KernelVersion         string                        `json:"kernel_version"`        // e.g. "v1.0.0"
	RoundingScale         int                           `json:"rounding_scale"`        // 0 to 6
	RoundingMode          RoundingMode                  `json:"rounding_mode"`         // HALF_EVEN, HALF_UP, FLOOR, CEILING
	ZeroDenominatorRule   ZeroDenomBehavior             `json:"zero_denominator_rule"` // NOT_READY, INVALID, ZERO
	ReworkCountsAsGood    bool                          `json:"rework_counts_as_good"` // true => Good = Conforming + Rework
	PerformanceCapRule    PerformanceCapBehavior        `json:"performance_cap_rule"`  // ALLOW_AND_RETAIN or FLAG_INVALID
	MicrostopThresholdSec *big.Rat                      `json:"microstop_threshold_sec"`
	StateBuckets          map[TimelineState]StateBucket `json:"state_buckets"` // nil: DefaultStateBuckets
	// PolicyDigest is computed by the kernel from every field above; a caller value is ignored.
	PolicyDigest string `json:"policy_digest"`
}

// DefaultKPIPolicy returns standard world-class defaults conforming to ISO 22400-2 / Nakajima TPM.
func DefaultKPIPolicy(policyID uuid.UUID) KPIPolicy {
	return KPIPolicy{
		PolicyRevisionID:      policyID,
		PolicyVersion:         "1.0.0",
		FormulaVersion:        "2026.1-nakajima-iso22400",
		KernelVersion:         "v1.0.0",
		RoundingScale:         6,
		RoundingMode:          RoundHalfEven,
		ZeroDenominatorRule:   ZeroDenomNotReady,
		ReworkCountsAsGood:    false,
		PerformanceCapRule:    CapAllowAndRetain,
		MicrostopThresholdSec: big.NewRat(300, 1), // 5 minutes
		StateBuckets:          DefaultStateBuckets(),
	}
}

// IdealCycleRevision represents an engineering-approved ideal cycle time for a specific product and routing.
type IdealCycleRevision struct {
	ProductID       string           `json:"product_id"`
	RoutingID       string           `json:"routing_id,omitempty"`
	Revision        int64            `json:"revision"`
	SecondsPerUnit  *big.Rat         `json:"seconds_per_unit"`
	Unit            UnitCode         `json:"unit"` // e.g. "s/unit", "min/unit"
	EffectiveWindow HalfOpenInterval `json:"effective_window"`
}

// ProductCountContribution represents a discrete production count record.
type ProductCountContribution struct {
	ContributionID  string           `json:"contribution_id"`
	ProductID       string           `json:"product_id"`
	RoutingID       string           `json:"routing_id,omitempty"`
	Interval        HalfOpenInterval `json:"interval"`
	TotalCount      *big.Rat         `json:"total_count"`
	ConformingCount *big.Rat         `json:"conforming_count"`
	ScrapCount      *big.Rat         `json:"scrap_count"`
	ReworkCount     *big.Rat         `json:"rework_count"`
	Unit            UnitCode         `json:"unit"` // e.g. "pcs", "count", "dozen"
}

// RatioMetric stores exact rational components and formatted decimal string for an OEE factor.
type RatioMetric struct {
	Numerator   *big.Rat `json:"numerator"`
	Denominator *big.Rat `json:"denominator"`
	Value       *big.Rat `json:"value,omitempty"`
	Formatted   string   `json:"formatted"`
	Percentage  string   `json:"percentage"`
	IsZeroDenom bool     `json:"is_zero_denom"`
	IsOvershoot bool     `json:"is_overshoot"`
}

// KPICalculationInput groups all verified, bounded inputs for the pure calculation kernel.
type KPICalculationInput struct {
	Window             HalfOpenInterval           `json:"window"`
	Timeline           TimelineResolutionResult   `json:"timeline"`
	CountContributions []ProductCountContribution `json:"count_contributions"`
	IdealCycles        []IdealCycleRevision       `json:"ideal_cycles"`
	Policy             KPIPolicy                  `json:"policy"`
}

// KPICalculationResult is the immutable, cryptographically verifiable result of the KPI kernel.
type KPICalculationResult struct {
	Window                   HalfOpenInterval `json:"window"`
	TotalCalendarSeconds     *big.Rat         `json:"total_calendar_seconds"`
	PlannedExclusionSeconds  *big.Rat         `json:"planned_exclusion_seconds"`
	PlannedProductionSeconds *big.Rat         `json:"planned_production_seconds"`
	RunSeconds               *big.Rat         `json:"run_seconds"`
	StopSeconds              *big.Rat         `json:"stop_seconds"`
	MicrostopSeconds         *big.Rat         `json:"microstop_seconds"`
	MicrostopCount           int64            `json:"microstop_count"`
	GapSeconds               *big.Rat         `json:"gap_seconds"`
	IdealRunSeconds          *big.Rat         `json:"ideal_run_seconds"`
	TotalPieces              *big.Rat         `json:"total_pieces"`
	GoodPieces               *big.Rat         `json:"good_pieces"`
	ScrapPieces              *big.Rat         `json:"scrap_pieces"`
	ReworkPieces             *big.Rat         `json:"rework_pieces"`

	Availability RatioMetric `json:"availability"`
	Performance  RatioMetric `json:"performance"`
	Quality      RatioMetric `json:"quality"`
	OEE          RatioMetric `json:"oee"`
	Loading      RatioMetric `json:"loading"`
	TEEP         RatioMetric `json:"teep"`

	Readiness ReadinessDecision `json:"readiness"`

	FormulaVersion    string `json:"formula_version"`
	PolicyVersion     string `json:"policy_version"`
	KernelVersion     string `json:"kernel_version"`
	PolicyDigest      string `json:"policy_digest"`
	CalculationDigest string `json:"calculation_digest"`
}

// FormatDecimal renders an exact rational to a fixed decimal scale using the specified rounding mode.
// Invariant to locale, process timezone, and platform.
func FormatDecimal(val *big.Rat, scale int, mode RoundingMode) (string, error) {
	if val == nil {
		return "", errors.New("cannot format nil rational")
	}
	if scale < 0 || scale > 6 {
		return "", errors.New("rounding scale must be between 0 and 6")
	}

	neg := val.Sign() < 0
	num := new(big.Int).Abs(val.Num())
	den := new(big.Int).Set(val.Denom())

	mult := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(scale)), nil)
	scaledNum := new(big.Int).Mul(num, mult)

	quotient := new(big.Int)
	remainder := new(big.Int)
	quotient.QuoRem(scaledNum, den, remainder)

	if remainder.Sign() != 0 {
		switch mode {
		case RoundHalfEven:
			doubled := new(big.Int).Lsh(remainder, 1)
			cmp := doubled.Cmp(den)
			if cmp > 0 || (cmp == 0 && quotient.Bit(0) == 1) {
				quotient.Add(quotient, big.NewInt(1))
			}
		case RoundHalfUp:
			doubled := new(big.Int).Lsh(remainder, 1)
			if doubled.Cmp(den) >= 0 {
				quotient.Add(quotient, big.NewInt(1))
			}
		case RoundFloor:
			if neg {
				quotient.Add(quotient, big.NewInt(1))
			}
		case RoundCeiling:
			if !neg {
				quotient.Add(quotient, big.NewInt(1))
			}
		default:
			return "", fmt.Errorf("unsupported rounding mode: %s", mode)
		}
	}

	signStr := ""
	if neg && quotient.Sign() != 0 {
		signStr = "-"
	}

	if scale == 0 {
		return fmt.Sprintf("%s%s", signStr, quotient.String()), nil
	}

	whole := new(big.Int)
	frac := new(big.Int)
	whole.QuoRem(quotient, mult, frac)

	fracStr := frac.String()
	for len(fracStr) < scale {
		fracStr = "0" + fracStr
	}

	return fmt.Sprintf("%s%s.%s", signStr, whole.String(), fracStr), nil
}

// ErrUnsupportedUnit means a unit is not on the allowlist. Units are never guessed.
var ErrUnsupportedUnit = errors.New("unsupported unit")

// cycleUnitSeconds are the exact factors to seconds per unit (ADR 0005 Sec. 11).
var cycleUnitSeconds = map[UnitCode]*big.Rat{
	"s/unit": big.NewRat(1, 1), "seconds/unit": big.NewRat(1, 1), "s": big.NewRat(1, 1), "sec": big.NewRat(1, 1),
	"min/unit": big.NewRat(60, 1), "minutes/unit": big.NewRat(60, 1), "min": big.NewRat(60, 1),
	"ms/unit": big.NewRat(1, 1000), "milliseconds/unit": big.NewRat(1, 1000), "ms": big.NewRat(1, 1000),
	"hours/unit": big.NewRat(3600, 1), "h/unit": big.NewRat(3600, 1), "hr": big.NewRat(3600, 1),
}

// countUnitPieces are the exact factors to pieces.
var countUnitPieces = map[UnitCode]*big.Rat{
	"pcs": big.NewRat(1, 1), "pieces": big.NewRat(1, 1), "count": big.NewRat(1, 1), "units": big.NewRat(1, 1), "unit": big.NewRat(1, 1),
	"dozen": big.NewRat(12, 1), "dz": big.NewRat(12, 1),
	"thousand": big.NewRat(1000, 1), "k": big.NewRat(1000, 1),
}

// ConvertCycleTimeToSeconds normalizes an ideal cycle rate to canonical seconds per unit.
// An empty or unknown unit fails: it is never assumed to be seconds.
func ConvertCycleTimeToSeconds(rate *big.Rat, unit UnitCode) (*big.Rat, error) {
	if rate == nil || rate.Sign() <= 0 {
		return nil, errors.New("ideal cycle rate must be positive")
	}
	factor, ok := cycleUnitSeconds[unit]
	if !ok {
		return nil, fmt.Errorf("%w: cycle unit %q", ErrUnsupportedUnit, unit)
	}
	return new(big.Rat).Mul(rate, factor), nil
}

// ConvertCountToPieces normalizes a count to pieces. An empty or unknown unit fails.
func ConvertCountToPieces(count *big.Rat, unit UnitCode) (*big.Rat, error) {
	if count == nil || count.Sign() < 0 {
		return nil, errors.New("count must be non-negative")
	}
	factor, ok := countUnitPieces[unit]
	if !ok {
		return nil, fmt.Errorf("%w: count unit %q", ErrUnsupportedUnit, unit)
	}
	return new(big.Rat).Mul(count, factor), nil
}

// normalizeKPIPolicy fills documented defaults for empty fields and rejects any value it does
// not recognize. An invalid policy is an error: it is never silently repaired.
func normalizeKPIPolicy(policy KPIPolicy) (KPIPolicy, error) {
	if policy.FormulaVersion == "" {
		policy.FormulaVersion = "2026.1-nakajima-iso22400"
	}
	if policy.KernelVersion == "" {
		policy.KernelVersion = "v1.0.0"
	}
	if policy.RoundingMode == "" {
		policy.RoundingMode = RoundHalfEven
	}
	if policy.ZeroDenominatorRule == "" {
		policy.ZeroDenominatorRule = ZeroDenomNotReady
	}
	if policy.PerformanceCapRule == "" {
		policy.PerformanceCapRule = CapAllowAndRetain
	}
	if policy.MicrostopThresholdSec == nil {
		policy.MicrostopThresholdSec = big.NewRat(300, 1)
	}
	if policy.StateBuckets == nil {
		policy.StateBuckets = DefaultStateBuckets()
	}
	switch {
	case policy.RoundingScale < 0 || policy.RoundingScale > 6:
		return KPIPolicy{}, fmt.Errorf("%w: rounding_scale %d", ErrInvalidPolicy, policy.RoundingScale)
	case policy.RoundingMode != RoundHalfEven && policy.RoundingMode != RoundHalfUp && policy.RoundingMode != RoundFloor && policy.RoundingMode != RoundCeiling:
		return KPIPolicy{}, fmt.Errorf("%w: rounding_mode %q", ErrInvalidPolicy, policy.RoundingMode)
	case policy.ZeroDenominatorRule != ZeroDenomNotReady && policy.ZeroDenominatorRule != ZeroDenomInvalid && policy.ZeroDenominatorRule != ZeroDenomZero:
		return KPIPolicy{}, fmt.Errorf("%w: zero_denominator_rule %q", ErrInvalidPolicy, policy.ZeroDenominatorRule)
	case policy.PerformanceCapRule != CapAllowAndRetain && policy.PerformanceCapRule != CapFlagInvalid:
		return KPIPolicy{}, fmt.Errorf("%w: performance_cap_rule %q", ErrInvalidPolicy, policy.PerformanceCapRule)
	case policy.MicrostopThresholdSec.Sign() < 0:
		return KPIPolicy{}, fmt.Errorf("%w: microstop_threshold_sec is negative", ErrInvalidPolicy)
	}
	for _, state := range bucketedStates {
		switch policy.StateBuckets[state] {
		case BucketRun, BucketStop, BucketExcluded:
		default:
			return KPIPolicy{}, fmt.Errorf("%w: state %s has no bucket", ErrInvalidPolicy, state)
		}
	}
	for state := range policy.StateBuckets {
		if !isBucketedState(state) {
			return KPIPolicy{}, fmt.Errorf("%w: unknown state %s in state_buckets", ErrInvalidPolicy, state)
		}
	}
	digest, err := computePolicyDigest(policy)
	if err != nil {
		return KPIPolicy{}, err
	}
	policy.PolicyDigest = digest
	return policy, nil
}

// computePolicyDigest hashes every rule that can change a result, in a fixed field order.
func computePolicyDigest(p KPIPolicy) (string, error) {
	states := make([]string, 0, len(p.StateBuckets))
	for s := range p.StateBuckets {
		states = append(states, string(s))
	}
	sort.Strings(states)
	buckets := make([][2]string, len(states))
	for i, s := range states {
		buckets[i] = [2]string{s, string(p.StateBuckets[TimelineState(s)])}
	}
	b, err := json.Marshal(struct {
		Schema             string      `json:"schema"`
		PolicyRevisionID   string      `json:"policy_revision_id"`
		PolicyVersion      string      `json:"policy_version"`
		FormulaVersion     string      `json:"formula_version"`
		KernelVersion      string      `json:"kernel_version"`
		RoundingScale      int         `json:"rounding_scale"`
		RoundingMode       string      `json:"rounding_mode"`
		ZeroDenominator    string      `json:"zero_denominator_rule"`
		ReworkCountsAsGood bool        `json:"rework_counts_as_good"`
		PerformanceCap     string      `json:"performance_cap_rule"`
		MicrostopThreshold string      `json:"microstop_threshold_sec"`
		StateBuckets       [][2]string `json:"state_buckets"`
	}{"KPIPolicyV1", p.PolicyRevisionID.String(), p.PolicyVersion, p.FormulaVersion, p.KernelVersion, p.RoundingScale,
		string(p.RoundingMode), string(p.ZeroDenominatorRule), p.ReworkCountsAsGood, string(p.PerformanceCapRule),
		p.MicrostopThresholdSec.RatString(), buckets})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// evaluateKPIReadiness is the single readiness decision for per-window and rollup results.
func evaluateKPIReadiness(reasons map[ReasonCode]struct{}, policy KPIPolicy, perf RatioMetric, zeroCalendar bool) ReadinessDecision {
	readiness := EvaluateReadiness(reasons)
	if (policy.PerformanceCapRule == CapFlagInvalid && perf.IsOvershoot) || zeroCalendar {
		// ADR 0005 Sec. 11: zero calendar time is INVALID with ZERO_DENOMINATOR.
		readiness.Status = StatusInvalid
	}
	return readiness
}

// ratCopy returns an independent copy, so callers never alias the inputs.
func ratCopy(r *big.Rat) *big.Rat {
	if r == nil {
		return new(big.Rat)
	}
	return new(big.Rat).Set(r)
}

// format renders value and percentage; the policy was validated, so an error is a bug.
func format(metric *RatioMetric, val *big.Rat, policy KPIPolicy) error {
	fmtVal, err := FormatDecimal(val, policy.RoundingScale, policy.RoundingMode)
	if err != nil {
		return err
	}
	fmtPct, err := FormatDecimal(new(big.Rat).Mul(val, big.NewRat(100, 1)), policy.RoundingScale, policy.RoundingMode)
	if err != nil {
		return err
	}
	metric.Formatted, metric.Percentage = fmtVal, fmtPct+"%"
	return nil
}

// applyZeroDenominatorPolicy sets a ratio whose denominator is zero. ADR 0005 Sec. 3
// forbids publishing a number for it. The default policy (NOT_READY) publishes no value.
// The ZERO policy emits an explicit zero that stays flagged, so it is never presented
// as a measured result.
func applyZeroDenominatorPolicy(metric *RatioMetric, policy KPIPolicy) error {
	metric.IsZeroDenom = true
	if policy.ZeroDenominatorRule != ZeroDenomZero {
		metric.Value = nil
		metric.Formatted = ""
		metric.Percentage = ""
		return nil
	}
	metric.Value = new(big.Rat)
	return format(metric, metric.Value, policy)
}

// ratioMetric computes num/den. A zero denominator sets ZERO_DENOMINATOR (and
// MISSING_DATA for availability) and never produces a fabricated 0%.
func ratioMetric(num, den *big.Rat, name string, policy KPIPolicy, checkOvershoot bool, reasons map[ReasonCode]struct{}) (RatioMetric, error) {
	metric := RatioMetric{Numerator: ratCopy(num), Denominator: ratCopy(den)}
	if den == nil || den.Sign() <= 0 {
		reasons[ReasonZeroDenominator] = struct{}{}
		if name == "Availability" {
			reasons[ReasonMissingData] = struct{}{}
		}
		if policy.ZeroDenominatorRule == ZeroDenomInvalid {
			// An INVALID zero-denominator policy invalidates the calculation.
			reasons[ReasonUnsupportedRule] = struct{}{}
		}
		return metric, applyZeroDenominatorPolicy(&metric, policy)
	}

	val := new(big.Rat).Quo(num, den)
	metric.Value = val

	if checkOvershoot && val.Cmp(big.NewRat(1, 1)) > 0 {
		metric.IsOvershoot = true
		reasons[ReasonExceedsTheoretical] = struct{}{}
		if name == "Availability" {
			reasons[ReasonAvailabilityOvershoot] = struct{}{}
		}
		if name == "Performance" {
			reasons[ReasonPerformanceOvershoot] = struct{}{}
		}
	}
	return metric, format(&metric, val, policy)
}

// composeProduct multiplies factor metrics (OEE = A x P x Q, TEEP = OEE x Loading). Any
// zero-denominator factor makes the composite unpublished under the same policy.
func composeProduct(policy KPIPolicy, factors ...RatioMetric) (RatioMetric, error) {
	for _, f := range factors {
		if f.IsZeroDenom {
			metric := RatioMetric{Numerator: new(big.Rat), Denominator: new(big.Rat)}
			return metric, applyZeroDenominatorPolicy(&metric, policy)
		}
	}
	metric := RatioMetric{Numerator: big.NewRat(1, 1), Denominator: big.NewRat(1, 1), Value: big.NewRat(1, 1)}
	for _, f := range factors {
		metric.Numerator.Mul(metric.Numerator, f.Numerator)
		metric.Denominator.Mul(metric.Denominator, f.Denominator)
		metric.Value.Mul(metric.Value, f.Value)
	}
	metric.IsOvershoot = metric.Value.Cmp(big.NewRat(1, 1)) > 0
	return metric, format(&metric, metric.Value, policy)
}

// composeOEE is the product Availability x Performance x Quality.
func composeOEE(avail, perf, qual RatioMetric, policy KPIPolicy) (RatioMetric, error) {
	return composeProduct(policy, avail, perf, qual)
}

// composeTEEP is TEEP = OEE x Loading = OEE x (Planned Production / Total Calendar Time).
func composeTEEP(oee, loading RatioMetric, policy KPIPolicy) (RatioMetric, error) {
	return composeProduct(policy, oee, loading)
}

func within(outer, inner HalfOpenInterval) bool {
	return !inner.Start.Before(outer.Start) && !inner.End.After(outer.End) && inner.Start.Before(inner.End)
}

func utcInterval(iv HalfOpenInterval) HalfOpenInterval {
	return HalfOpenInterval{Start: iv.Start.UTC(), End: iv.End.UTC()}
}

// bindIdealCycle returns the one approved cycle whose effective window contains the whole
// contribution. Effective windows must be fully set or fully open. A contribution that spans
// a revision boundary is not split proportionally: it fails closed.
func bindIdealCycle(cycles []IdealCycleRevision, c ProductCountContribution, reasons map[ReasonCode]struct{}) *IdealCycleRevision {
	var match *IdealCycleRevision
	partial := false
	for i := range cycles {
		ic := &cycles[i]
		if ic.ProductID != c.ProductID || (ic.RoutingID != "" && c.RoutingID != "" && ic.RoutingID != c.RoutingID) {
			continue
		}
		open := ic.EffectiveWindow.Start.IsZero() && ic.EffectiveWindow.End.IsZero()
		if !open && (ic.EffectiveWindow.Start.IsZero() || ic.EffectiveWindow.End.IsZero() || !ic.EffectiveWindow.Start.Before(ic.EffectiveWindow.End)) {
			reasons[ReasonUnsupportedRule] = struct{}{}
			return nil
		}
		switch {
		case open || within(ic.EffectiveWindow, c.Interval):
			if match != nil {
				// Two approved revisions both claim this interval.
				reasons[ReasonConflictingFact] = struct{}{}
				return nil
			}
			match = ic
		case ic.EffectiveWindow.Overlaps(c.Interval):
			partial = true
		}
	}
	if match == nil {
		if partial {
			reasons[ReasonConflictingFact] = struct{}{}
		} else {
			reasons[ReasonMissingPolicy] = struct{}{}
			reasons[ReasonMissingIdealCycle] = struct{}{}
		}
	}
	return match
}

// CalculateKPIs executes the pure, deterministic KPI calculation over the canonical timeline.
// It performs no I/O, clock reads, database reads or random calls, and its result does not
// depend on the order of contributions or ideal cycles.
func CalculateKPIs(input KPICalculationInput) (KPICalculationResult, error) {
	policy, err := normalizeKPIPolicy(input.Policy)
	if err != nil {
		return KPICalculationResult{}, err
	}
	window := utcInterval(input.Window)
	if window.End.Before(window.Start) {
		return KPICalculationResult{}, fmt.Errorf("%w: window ends before it starts", ErrInvalidKPIInput)
	}
	zeroCalendar := !window.Start.Before(window.End)
	if len(input.Timeline.Segments) > 0 && !(input.Timeline.Window.Start.Equal(window.Start) && input.Timeline.Window.End.Equal(window.End)) {
		return KPICalculationResult{}, fmt.Errorf("%w: timeline window does not match the calculation window", ErrInvalidKPIInput)
	}

	reasons := make(map[ReasonCode]struct{})
	// Inherit existing timeline readiness reasons
	for _, code := range input.Timeline.Readiness.ReasonCodes {
		reasons[code] = struct{}{}
	}

	// Sort inputs canonically to guarantee strict invariance to input order.
	contributions := make([]ProductCountContribution, len(input.CountContributions))
	copy(contributions, input.CountContributions)
	sort.Slice(contributions, func(i, j int) bool {
		a, b := contributions[i], contributions[j]
		if !a.Interval.Start.Equal(b.Interval.Start) {
			return a.Interval.Start.Before(b.Interval.Start)
		}
		return a.ContributionID < b.ContributionID
	})
	for i := 1; i < len(contributions); i++ {
		if contributions[i].ContributionID == contributions[i-1].ContributionID {
			return KPICalculationResult{}, fmt.Errorf("%w: duplicate contribution %q", ErrInvalidKPIInput, contributions[i].ContributionID)
		}
	}
	idealCycles := make([]IdealCycleRevision, len(input.IdealCycles))
	copy(idealCycles, input.IdealCycles)
	sort.Slice(idealCycles, func(i, j int) bool {
		a, b := idealCycles[i], idealCycles[j]
		if a.ProductID != b.ProductID {
			return a.ProductID < b.ProductID
		}
		if a.RoutingID != b.RoutingID {
			return a.RoutingID < b.RoutingID
		}
		return a.Revision > b.Revision
	})
	for i := 1; i < len(idealCycles); i++ {
		a, b := idealCycles[i-1], idealCycles[i]
		if a.ProductID == b.ProductID && a.RoutingID == b.RoutingID && a.Revision == b.Revision {
			// The same revision twice is ambiguous whatever the values say.
			reasons[ReasonConflictingFact] = struct{}{}
		}
	}

	totalCalendarSec := window.DurationSeconds()
	if zeroCalendar {
		reasons[ReasonZeroDenominator] = struct{}{}
		totalCalendarSec = new(big.Rat)
	}

	// 1. Time over the canonical timeline. States map to buckets by policy. Consecutive
	// unplanned stops form one stop event; an event no longer than the microstop threshold
	// is a minor stop (Loss 3, a Performance loss), so it counts as operating time. The
	// threshold applies to the event's true length: a stop fact that crosses a window edge
	// keeps the part outside the window in the comparison (only in-window seconds are
	// counted), so a long breakdown clipped by the window stays a breakdown (Loss 1).
	runSec := new(big.Rat)
	stopSec := new(big.Rat)
	plannedExclSec := new(big.Rat)
	gapSec := new(big.Rat)
	microstopSec := new(big.Rat)
	microstopCount := int64(0)
	pendingStop := new(big.Rat)
	pendingOutside := new(big.Rat) // seconds of the current stop event outside the window
	flushStop := func() {
		if pendingStop.Sign() == 0 {
			return
		}
		eventLength := new(big.Rat).Add(pendingStop, pendingOutside)
		if eventLength.Cmp(policy.MicrostopThresholdSec) <= 0 {
			microstopSec.Add(microstopSec, pendingStop)
			runSec.Add(runSec, pendingStop)
			microstopCount++
		} else {
			stopSec.Add(stopSec, pendingStop)
		}
		pendingStop, pendingOutside = new(big.Rat), new(big.Rat)
	}
	// outsideWindow is how much of a segment's fact lies beyond the window edge it touches.
	outsideWindow := func(seg CanonicalFactSegment) *big.Rat {
		out := new(big.Rat)
		if seg.EventInterval == nil {
			return out
		}
		full, part := utcInterval(*seg.EventInterval), utcInterval(seg.Interval)
		if part.Start.Equal(window.Start) && full.Start.Before(window.Start) {
			out.Add(out, HalfOpenInterval{Start: full.Start, End: window.Start}.DurationSeconds())
		}
		if part.End.Equal(window.End) && full.End.After(window.End) {
			out.Add(out, HalfOpenInterval{Start: window.End, End: full.End}.DurationSeconds())
		}
		return out
	}
	segments := make([]CanonicalFactSegment, len(input.Timeline.Segments))
	copy(segments, input.Timeline.Segments)
	sort.Slice(segments, func(i, j int) bool { return segments[i].Interval.Start.Before(segments[j].Interval.Start) })
	for i, seg := range segments {
		if !within(window, utcInterval(seg.Interval)) {
			return KPICalculationResult{}, fmt.Errorf("%w: segment %d lies outside the window", ErrInvalidKPIInput, i)
		}
		if i > 0 && seg.Interval.Start.Before(segments[i-1].Interval.End) {
			return KPICalculationResult{}, fmt.Errorf("%w: segments %d and %d overlap", ErrInvalidKPIInput, i-1, i)
		}
		dur := seg.Interval.DurationSeconds()
		if seg.State == StateUnplannedDowntime && !seg.IsGap {
			if i > 0 && !segments[i-1].Interval.End.Equal(seg.Interval.Start) {
				flushStop()
			}
			pendingStop.Add(pendingStop, dur)
			pendingOutside.Add(pendingOutside, outsideWindow(seg))
			continue
		}
		flushStop()
		if seg.IsGap || seg.State == StateGap {
			gapSec.Add(gapSec, dur)
			continue
		}
		bucket, ok := policy.StateBuckets[seg.State]
		if !ok {
			return KPICalculationResult{}, fmt.Errorf("%w: segment %d has unknown state %q", ErrInvalidKPIInput, i, seg.State)
		}
		switch bucket {
		case BucketRun:
			runSec.Add(runSec, dur)
		case BucketStop:
			stopSec.Add(stopSec, dur)
		case BucketExcluded:
			plannedExclSec.Add(plannedExclSec, dur)
		}
	}
	flushStop()
	if policy.StateBuckets[StateUnplannedDowntime] != BucketStop {
		return KPICalculationResult{}, fmt.Errorf("%w: UNPLANNED_DOWNTIME must map to STOP", ErrInvalidPolicy)
	}

	// Planned Production Time = Run Time + Stop Duration
	plannedProdSec := new(big.Rat).Add(runSec, stopSec)

	// 2. Ideal run time and output. Total = Conforming + Scrap + Rework (ADR 0005 Sec. 10).
	idealRunSec := new(big.Rat)
	totalPieces := new(big.Rat)
	goodPieces := new(big.Rat)
	scrapPieces := new(big.Rat)
	reworkPieces := new(big.Rat)

	for _, contrib := range contributions {
		if !within(window, utcInterval(contrib.Interval)) {
			return KPICalculationResult{}, fmt.Errorf("%w: contribution %q lies outside the window", ErrInvalidKPIInput, contrib.ContributionID)
		}
		if contrib.TotalCount == nil || contrib.TotalCount.Sign() < 0 {
			reasons[ReasonNegativeCounterDelta] = struct{}{}
			continue
		}
		for _, part := range []*big.Rat{contrib.ConformingCount, contrib.ScrapCount, contrib.ReworkCount} {
			if part != nil && part.Sign() < 0 {
				reasons[ReasonInvalidQualityCounts] = struct{}{}
			}
		}
		pieces := func(v *big.Rat) (*big.Rat, bool) {
			if v == nil {
				return new(big.Rat), true
			}
			p, err := ConvertCountToPieces(v, contrib.Unit)
			if err != nil {
				return nil, false
			}
			return p, true
		}
		cTotal, okT := pieces(contrib.TotalCount)
		cScrap, okS := pieces(contrib.ScrapCount)
		cRework, okR := pieces(contrib.ReworkCount)
		if !okT || !okS || !okR {
			if _, err := ConvertCountToPieces(new(big.Rat), contrib.Unit); errors.Is(err, ErrUnsupportedUnit) {
				reasons[ReasonUnsupportedRule] = struct{}{}
			}
			continue
		}
		totalPieces.Add(totalPieces, cTotal)
		scrapPieces.Add(scrapPieces, cScrap)
		reworkPieces.Add(reworkPieces, cRework)

		var cConf *big.Rat
		if contrib.ConformingCount != nil {
			c, ok := pieces(contrib.ConformingCount)
			if !ok {
				continue
			}
			cConf = c
			if new(big.Rat).Add(cConf, new(big.Rat).Add(cScrap, cRework)).Cmp(cTotal) != 0 {
				// The components must reconcile to the total exactly.
				reasons[ReasonInvalidQualityCounts] = struct{}{}
			}
		} else {
			// Conforming derived from this contribution only. A negative result is a data
			// error: the raw value is kept and flagged, never clamped.
			cConf = new(big.Rat).Sub(cTotal, new(big.Rat).Add(cScrap, cRework))
			if cConf.Sign() < 0 {
				reasons[ReasonInvalidQualityCounts] = struct{}{}
			}
		}
		cGood := cConf
		if policy.ReworkCountsAsGood {
			cGood = new(big.Rat).Add(cConf, cRework)
		}
		goodPieces.Add(goodPieces, cGood)

		matchedCycle := bindIdealCycle(idealCycles, contrib, reasons)
		if matchedCycle == nil {
			continue
		}
		secPerUnit, err := ConvertCycleTimeToSeconds(matchedCycle.SecondsPerUnit, matchedCycle.Unit)
		if err != nil {
			reasons[ReasonUnsupportedRule] = struct{}{}
			continue
		}
		idealRunSec.Add(idealRunSec, new(big.Rat).Mul(cTotal, secPerUnit))
	}

	// Quality Bounds Check: Good count cannot exceed total count
	if goodPieces.Cmp(totalPieces) > 0 {
		reasons[ReasonInvalidQualityCounts] = struct{}{}
	}

	// 3. Compute ratios with zero-denominator policy and overshoot flags.
	res := KPICalculationResult{
		Window: window, TotalCalendarSeconds: totalCalendarSec, PlannedExclusionSeconds: plannedExclSec,
		PlannedProductionSeconds: plannedProdSec, RunSeconds: runSec, StopSeconds: stopSec,
		MicrostopSeconds: microstopSec, MicrostopCount: microstopCount, GapSeconds: gapSec,
		IdealRunSeconds: idealRunSec, TotalPieces: totalPieces, GoodPieces: goodPieces,
		ScrapPieces: scrapPieces, ReworkPieces: reworkPieces,
	}
	if err = finishResult(&res, reasons, policy, zeroCalendar); err != nil {
		return KPICalculationResult{}, err
	}
	return res, nil
}

// finishResult computes every ratio from the exact sums in res, the readiness, the versions
// and the digest. Per-window results and rollups share it.
func finishResult(res *KPICalculationResult, reasons map[ReasonCode]struct{}, policy KPIPolicy, zeroCalendar bool) error {
	var err error
	if res.Availability, err = ratioMetric(res.RunSeconds, res.PlannedProductionSeconds, "Availability", policy, true, reasons); err != nil {
		return err
	}
	if res.Performance, err = ratioMetric(res.IdealRunSeconds, res.RunSeconds, "Performance", policy, true, reasons); err != nil {
		return err
	}
	if res.Quality, err = ratioMetric(res.GoodPieces, res.TotalPieces, "Quality", policy, false, reasons); err != nil {
		return err
	}
	if res.Loading, err = ratioMetric(res.PlannedProductionSeconds, res.TotalCalendarSeconds, "Loading", policy, true, reasons); err != nil {
		return err
	}
	if res.OEE, err = composeOEE(res.Availability, res.Performance, res.Quality, policy); err != nil {
		return err
	}
	if res.TEEP, err = composeTEEP(res.OEE, res.Loading, policy); err != nil {
		return err
	}
	res.Readiness = evaluateKPIReadiness(reasons, policy, res.Performance, zeroCalendar)
	res.FormulaVersion, res.PolicyVersion, res.KernelVersion, res.PolicyDigest = policy.FormulaVersion, policy.PolicyVersion, policy.KernelVersion, policy.PolicyDigest
	res.CalculationDigest, err = ComputeKPICalculationDigest(*res)
	return err
}

// ComputeKPICalculationDigest is the SHA-256 of a schema-ordered canonical body that binds
// every exact input sum and result, the readiness decision and the formula, policy and
// kernel versions with the policy content digest. Values are exact rational strings and
// times are UTC, so equal results digest equally and any changed field changes the digest.
// Tenant and site are bound where the result is persisted.
func ComputeKPICalculationDigest(r KPICalculationResult) (string, error) {
	rat := func(v *big.Rat) string {
		if v == nil {
			return ""
		}
		return v.RatString()
	}
	type metric struct {
		Numerator, Denominator, Value, Formatted string
		ZeroDenominator, Overshoot               bool
	}
	m := func(x RatioMetric) metric {
		return metric{rat(x.Numerator), rat(x.Denominator), rat(x.Value), x.Formatted, x.IsZeroDenom, x.IsOvershoot}
	}
	codes := make([]string, len(r.Readiness.ReasonCodes))
	for i, c := range r.Readiness.ReasonCodes {
		codes[i] = string(c)
	}
	b, err := json.Marshal(struct {
		Schema                                                   string
		WindowStart, WindowEnd                                   string
		Calendar, Excluded, Planned, Run, Stop, Microstop, Gap   string
		MicrostopCount                                           int64
		IdealRun, Total, Good, Scrap, Rework                     string
		Availability, Performance, Quality, OEE, Loading, TEEP   metric
		Status                                                   string
		Reasons                                                  []string
		FormulaVersion, PolicyVersion, KernelVersion, PolicyHash string
	}{
		"KPICalculationResultV1",
		r.Window.Start.UTC().Format(time.RFC3339Nano), r.Window.End.UTC().Format(time.RFC3339Nano),
		rat(r.TotalCalendarSeconds), rat(r.PlannedExclusionSeconds), rat(r.PlannedProductionSeconds), rat(r.RunSeconds), rat(r.StopSeconds), rat(r.MicrostopSeconds), rat(r.GapSeconds),
		r.MicrostopCount,
		rat(r.IdealRunSeconds), rat(r.TotalPieces), rat(r.GoodPieces), rat(r.ScrapPieces), rat(r.ReworkPieces),
		m(r.Availability), m(r.Performance), m(r.Quality), m(r.OEE), m(r.Loading), m(r.TEEP),
		string(r.Readiness.Status), codes,
		r.FormulaVersion, r.PolicyVersion, r.KernelVersion, r.PolicyDigest,
	})
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

// AggregateKPIResults aggregates asset or shift results into one rollup. It sums raw
// numerators and denominators and recomputes every ratio; child percentages are NEVER
// averaged. Children must share the rollup's formula, kernel and policy (digest), must be
// complete, and must not repeat a result.
func AggregateKPIResults(children []KPICalculationResult, policy KPIPolicy) (KPICalculationResult, error) {
	if len(children) == 0 {
		return KPICalculationResult{}, errors.New("cannot aggregate empty children results")
	}
	policy, err := normalizeKPIPolicy(policy)
	if err != nil {
		return KPICalculationResult{}, err
	}
	res := KPICalculationResult{
		TotalCalendarSeconds: new(big.Rat), PlannedExclusionSeconds: new(big.Rat), PlannedProductionSeconds: new(big.Rat),
		RunSeconds: new(big.Rat), StopSeconds: new(big.Rat), MicrostopSeconds: new(big.Rat), GapSeconds: new(big.Rat),
		IdealRunSeconds: new(big.Rat), TotalPieces: new(big.Rat), GoodPieces: new(big.Rat), ScrapPieces: new(big.Rat), ReworkPieces: new(big.Rat),
	}
	reasons := make(map[ReasonCode]struct{})
	seen := make(map[string]bool, len(children))
	minStart, maxEnd := children[0].Window.Start.UTC(), children[0].Window.End.UTC()
	for i, c := range children {
		sums := []*big.Rat{c.TotalCalendarSeconds, c.PlannedExclusionSeconds, c.PlannedProductionSeconds, c.RunSeconds, c.StopSeconds,
			c.MicrostopSeconds, c.GapSeconds, c.IdealRunSeconds, c.TotalPieces, c.GoodPieces, c.ScrapPieces, c.ReworkPieces}
		for _, v := range sums {
			if v == nil {
				return KPICalculationResult{}, fmt.Errorf("%w: child %d is incomplete", ErrIncompatibleChildren, i)
			}
		}
		if c.FormulaVersion != policy.FormulaVersion || c.KernelVersion != policy.KernelVersion || c.PolicyDigest != policy.PolicyDigest {
			return KPICalculationResult{}, fmt.Errorf("%w: child %d was calculated under another formula, kernel or policy", ErrIncompatibleChildren, i)
		}
		if c.CalculationDigest == "" || seen[c.CalculationDigest] {
			return KPICalculationResult{}, fmt.Errorf("%w: child %d is unsealed or repeated", ErrIncompatibleChildren, i)
		}
		seen[c.CalculationDigest] = true
		if c.Window.Start.Before(minStart) {
			minStart = c.Window.Start.UTC()
		}
		if c.Window.End.After(maxEnd) {
			maxEnd = c.Window.End.UTC()
		}
		targets := []*big.Rat{res.TotalCalendarSeconds, res.PlannedExclusionSeconds, res.PlannedProductionSeconds, res.RunSeconds, res.StopSeconds,
			res.MicrostopSeconds, res.GapSeconds, res.IdealRunSeconds, res.TotalPieces, res.GoodPieces, res.ScrapPieces, res.ReworkPieces}
		for j, v := range sums {
			targets[j].Add(targets[j], v)
		}
		res.MicrostopCount += c.MicrostopCount
		for _, code := range c.Readiness.ReasonCodes {
			reasons[code] = struct{}{}
		}
		if c.Readiness.Status == StatusInvalid {
			reasons[ReasonUnsupportedRule] = struct{}{}
		}
	}
	res.Window = HalfOpenInterval{Start: minStart, End: maxEnd}
	if res.GoodPieces.Cmp(res.TotalPieces) > 0 {
		reasons[ReasonInvalidQualityCounts] = struct{}{}
	}
	if err = finishResult(&res, reasons, policy, res.TotalCalendarSeconds.Sign() == 0); err != nil {
		return KPICalculationResult{}, err
	}
	return res, nil
}
