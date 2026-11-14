package oee

type ReasonCode string

const (
	ReasonMissingPolicy    ReasonCode = "MISSING_POLICY"
	ReasonStaleSource      ReasonCode = "STALE_SOURCE"
	ReasonConflictingFact  ReasonCode = "CONFLICTING_FACT"
	ReasonUnsupportedRule  ReasonCode = "UNSUPPORTED_RULE"
	ReasonCancelled        ReasonCode = "CANCELLED"
	ReasonDeadlineExceeded ReasonCode = "DEADLINE_EXCEEDED"

	ReasonZeroDenominator       ReasonCode = "ZERO_DENOMINATOR"
	ReasonMissingData           ReasonCode = "MISSING_DATA"
	ReasonPerformanceOvershoot  ReasonCode = "PERFORMANCE_OVERSHOOT"
	ReasonAvailabilityOvershoot ReasonCode = "AVAILABILITY_OVERSHOOT"
	ReasonInvalidQualityCounts  ReasonCode = "INVALID_QUALITY_COUNTS"
	ReasonNegativeCounterDelta  ReasonCode = "NEGATIVE_COUNTER_DELTA"
	ReasonExceedsTheoretical    ReasonCode = "EXCEEDS_THEORETICAL_BOUNDS"
	ReasonMissingIdealCycle     ReasonCode = "MISSING_IDEAL_CYCLE"
)

var reasonCodePrecedence = []ReasonCode{
	ReasonMissingPolicy,
	ReasonMissingIdealCycle,
	ReasonMissingData,
	ReasonStaleSource,
	ReasonConflictingFact,
	ReasonUnsupportedRule,
	ReasonInvalidQualityCounts,
	ReasonAvailabilityOvershoot,
	ReasonZeroDenominator,
	ReasonNegativeCounterDelta,
	ReasonExceedsTheoretical,
	ReasonPerformanceOvershoot,
	ReasonCancelled,
	ReasonDeadlineExceeded,
}

// ReasonCodes returns every published reason code in stable precedence order.
func ReasonCodes() []ReasonCode { return append([]ReasonCode(nil), reasonCodePrecedence...) }

type Status string

const (
	StatusReady    Status = "READY"
	StatusLimited  Status = "LIMITED"
	StatusNotReady Status = "NOT_READY"
	StatusInvalid  Status = "INVALID"
)

// Statuses returns the stable readiness states.
func Statuses() []Status { return []Status{StatusReady, StatusLimited, StatusNotReady, StatusInvalid} }

// ReadinessDecision is the deterministic evaluation of readiness reasons.
type ReadinessDecision struct {
	Status      Status       `json:"status"`
	ReasonCodes []ReasonCode `json:"reason_codes"`
}

// EvaluateReadiness evaluates a set of detected structural reasons with stable precedence.
func EvaluateReadiness(reasons map[ReasonCode]struct{}) ReadinessDecision {
	var ordered []ReasonCode
	hasInvalid := false

	for _, code := range reasonCodePrecedence {
		if _, ok := reasons[code]; ok {
			ordered = append(ordered, code)
			if code == ReasonUnsupportedRule || code == ReasonInvalidQualityCounts || code == ReasonAvailabilityOvershoot {
				hasInvalid = true
			}
		}
	}

	if len(ordered) == 0 {
		return ReadinessDecision{
			Status:      StatusReady,
			ReasonCodes: []ReasonCode{},
		}
	}

	status := StatusNotReady
	if hasInvalid {
		status = StatusInvalid
	} else {
		allLimited := true
		for _, code := range ordered {
			if code != ReasonPerformanceOvershoot && code != ReasonExceedsTheoretical {
				allLimited = false
				break
			}
		}
		if allLimited {
			status = StatusLimited
		}
	}

	return ReadinessDecision{
		Status:      status,
		ReasonCodes: ordered,
	}
}
