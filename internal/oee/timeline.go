package oee

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/canonical"
)

var (
	ErrConflictingDuplicate  = errors.New("conflicting duplicate event detected")
	ErrUnresolvableOverlap   = errors.New("unresolvable overlap conflict detected")
	ErrInvalidPolicy         = errors.New("invalid timeline policy")
	ErrInvalidEventTime      = errors.New("invalid event timestamp")
	ErrNegativeCounter       = errors.New("counter value cannot be negative")
	ErrCounterDropNoEvidence = errors.New("counter decreased without explicit rollover or reset evidence")
	ErrInvalidTimezone       = errors.New("invalid or unresolvable IANA timezone")
	ErrMalformedFact         = errors.New("malformed owner fact")
	ErrTimelineLimit         = errors.New("timeline input exceeds limit")
	ErrLocalTimeInDSTGap     = errors.New("local time does not exist in timezone (DST gap)")
	ErrInvalidLocalTime      = errors.New("invalid local date or clock")
	ErrConflictingCounter    = errors.New("counter samples at the same source time disagree")
	ErrMixedCounterCodes     = errors.New("counter readings belong to different counters")
	ErrInvalidRollover       = errors.New("rollover evidence is inconsistent with readings")
	ErrMissingGeneratedAt    = errors.New("generated_at must be supplied by the caller")
	ErrRevisionLineage       = errors.New("previous revision belongs to a different calculation or window")
)

// Hard ceilings for timeline and counter inputs. They are checked before any
// slice, map or segment is materialized. A policy may tighten them, never raise them.
const (
	MaxTimelineFacts   = 10_000
	MaxTimelineWindow  = 31 * 24 * time.Hour
	MaxCounterReadings = 100_000
)

// TimelineLimits tightens the hard ceilings for one policy. Zero means the ceiling.
type TimelineLimits struct {
	MaxFacts  int           `json:"max_facts,omitempty"`
	MaxWindow time.Duration `json:"max_window,omitempty"`
}

func (l TimelineLimits) effective() (TimelineLimits, error) {
	if l.MaxFacts < 0 || l.MaxFacts > MaxTimelineFacts || l.MaxWindow < 0 || l.MaxWindow > MaxTimelineWindow {
		return TimelineLimits{}, fmt.Errorf("%w: limits may only tighten the hard ceilings", ErrInvalidPolicy)
	}
	out := l
	if out.MaxFacts == 0 {
		out.MaxFacts = MaxTimelineFacts
	}
	if out.MaxWindow == 0 {
		out.MaxWindow = MaxTimelineWindow
	}
	return out, nil
}

// checkTimelineInput enforces window validity and size limits before materialization.
func checkTimelineInput(window HalfOpenInterval, factCount int, policy TimelinePolicy) error {
	limits, err := policy.Limits.effective()
	if err != nil {
		return err
	}
	if m := policy.MinCoverage; m != nil && (m.Sign() < 0 || m.Cmp(big.NewRat(1, 1)) > 0) {
		return fmt.Errorf("%w: min_coverage must be within [0, 1]", ErrInvalidPolicy)
	}
	if policy.MaxSourceSilence < 0 {
		return fmt.Errorf("%w: max_source_silence must not be negative", ErrInvalidPolicy)
	}
	if !window.Start.Before(window.End) {
		return ErrInvalidInterval
	}
	if window.Duration() > limits.MaxWindow {
		return fmt.Errorf("%w: window %s exceeds %s", ErrTimelineLimit, window.Duration(), limits.MaxWindow)
	}
	if factCount > limits.MaxFacts {
		return fmt.Errorf("%w: %d facts exceeds %d", ErrTimelineLimit, factCount, limits.MaxFacts)
	}
	return nil
}

// TimelineSegment represents a source-labelled interval retained for timeline normalization.
type TimelineSegment struct {
	Interval  HalfOpenInterval `json:"interval"`
	SourceKey string           `json:"source_key"`
}

// OrderSegments sorts timeline segments canonically by Start time, then SourceKey tiebreaker.
func OrderSegments(segments []TimelineSegment) []TimelineSegment {
	out := make([]TimelineSegment, len(segments))
	copy(out, segments)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Interval.Start.Equal(out[j].Interval.Start) {
			return out[i].SourceKey < out[j].SourceKey
		}
		return out[i].Interval.Start.Before(out[j].Interval.Start)
	})
	return out
}

// HasOverlap detects whether any two segments in the list overlap.
func HasOverlap(segments []TimelineSegment) bool {
	if len(segments) < 2 {
		return false
	}
	ordered := OrderSegments(segments)
	for i := 0; i < len(ordered)-1; i++ {
		if ordered[i].Interval.Overlaps(ordered[i+1].Interval) {
			return true
		}
	}
	return false
}

// TimelineState represents the normalized operational classification of an interval.
type TimelineState string

const (
	StateRunning           TimelineState = "RUNNING"
	StateUnplannedDowntime TimelineState = "UNPLANNED_DOWNTIME"
	StatePlannedBreak      TimelineState = "PLANNED_BREAK"
	StateChangeover        TimelineState = "CHANGEOVER"
	StateShutdownExclusion TimelineState = "SHUTDOWN_EXCLUSION"
	StateStandby           TimelineState = "STANDBY"
	StateGap               TimelineState = "GAP"
)

// OwnerFactEvent represents a source-backed operational observation.
// Source/event time and recorded/ingestion time remain strictly separate.
type OwnerFactEvent struct {
	EventID       string           `json:"event_id"`
	SourceKey     string           `json:"source_key"`
	State         TimelineState    `json:"state"`
	Interval      HalfOpenInterval `json:"interval"`
	EventAt       time.Time        `json:"event_at"`
	RecordedAt    time.Time        `json:"recorded_at"`
	Revision      int64            `json:"revision"`
	PayloadDigest string           `json:"payload_digest"`
}

// CanonicalFactSegment is an unambiguous, non-overlapping slice in the normalized timeline.
type CanonicalFactSegment struct {
	Interval  HalfOpenInterval `json:"interval"`
	State     TimelineState    `json:"state"`
	SourceKey string           `json:"source_key"`
	EventID   string           `json:"event_id"`
	Revision  int64            `json:"revision"`
	IsGap     bool             `json:"is_gap"`
	// EventInterval is the winning fact's full interval, set only on a segment at a window edge
	// whose fact extends past that edge. The kernel needs it to classify a stop by its true
	// length: a long breakdown clipped by the window is still a breakdown, not a microstop.
	EventInterval *HalfOpenInterval `json:"event_interval,omitempty"`
}

// TimelinePolicy defines precedence and calendar projection parameters.
type TimelinePolicy struct {
	PrecedenceOrder []TimelineState `json:"precedence_order"`
	IANATimezone    string          `json:"iana_timezone"`
	Limits          TimelineLimits  `json:"limits"`
	// MinCoverage is the covered fraction of the window below which the timeline is NOT_READY
	// with MISSING_DATA. Nil means 1: with no declared tolerance every second must be covered.
	MinCoverage *big.Rat `json:"min_coverage,omitempty"`
	// MaxSourceSilence, when set, makes a contributing source stale (NOT_READY, STALE_SOURCE)
	// unless its newest fact in the window was recorded no earlier than window end minus
	// this duration. Calendar exclusions are exempt. Zero disables the check.
	MaxSourceSilence time.Duration `json:"max_source_silence,omitempty"`
}

// PrecedenceRank returns the numerical rank of a state (lower number = higher precedence).
// If state is not in policy, returns -1.
func (p TimelinePolicy) PrecedenceRank(state TimelineState) int {
	for i, s := range p.PrecedenceOrder {
		if s == state {
			return i
		}
	}
	return -1
}

// TimelineResolutionResult contains the canonical segments, coverage ratio, and readiness outcome.
type TimelineResolutionResult struct {
	Segments       []CanonicalFactSegment `json:"segments"`
	Readiness      ReadinessDecision      `json:"readiness"`
	Window         HalfOpenInterval       `json:"window"`
	CoveredSeconds *big.Rat               `json:"covered_seconds"`
	GapSeconds     *big.Rat               `json:"gap_seconds"`
	CoverageRatio  *big.Rat               `json:"coverage_ratio"`
}

// inputStates are the states an owner fact may carry. GAP is output-only.
var inputStates = map[TimelineState]bool{
	StateRunning: true, StateUnplannedDowntime: true, StatePlannedBreak: true,
	StateChangeover: true, StateShutdownExclusion: true, StateStandby: true,
}

type factKey struct{ source, event string }

// DeduplicateOwnerFacts validates facts, normalizes their times to UTC, and resolves
// repeated (SourceKey, EventID) pairs. Event IDs are scoped by source, so two sources may
// use the same ID.
//   - Exact duplicate (equal apart from RecordedAt) -> kept once, with the earliest RecordedAt.
//   - Same pair, different revisions -> the highest revision is the correction and wins.
//   - Same pair and revision, any other difference -> ErrConflictingDuplicate.
//
// The output is sorted by (SourceKey, EventID), so it does not depend on input order.
func DeduplicateOwnerFacts(events []OwnerFactEvent) ([]OwnerFactEvent, error) {
	byKey := make(map[factKey]OwnerFactEvent, len(events))

	for _, e := range events {
		if e.EventID == "" {
			return nil, fmt.Errorf("%w: event_id must not be empty", ErrMalformedFact)
		}
		if e.Interval.Start.IsZero() || e.Interval.End.IsZero() || !e.Interval.Start.Before(e.Interval.End) {
			return nil, fmt.Errorf("%w: event_id %q: %v", ErrMalformedFact, e.EventID, ErrInvalidInterval)
		}
		if !inputStates[e.State] {
			return nil, fmt.Errorf("%w: event_id %q has unknown state %q", ErrMalformedFact, e.EventID, e.State)
		}
		if e.Revision < 1 {
			return nil, fmt.Errorf("%w: event_id %q revision must be >= 1", ErrMalformedFact, e.EventID)
		}
		e.Interval = HalfOpenInterval{Start: e.Interval.Start.UTC(), End: e.Interval.End.UTC()}
		e.EventAt, e.RecordedAt = e.EventAt.UTC(), e.RecordedAt.UTC()

		key := factKey{e.SourceKey, e.EventID}
		existing, seen := byKey[key]
		switch {
		case !seen || e.Revision > existing.Revision:
			byKey[key] = e
		case e.Revision < existing.Revision:
			// An older revision of a fact that has since been corrected.
		case !sameOwnerFact(existing, e):
			return nil, fmt.Errorf("%w: source %q event_id %q revision %d has conflicting facts", ErrConflictingDuplicate, e.SourceKey, e.EventID, e.Revision)
		case e.RecordedAt.Before(existing.RecordedAt):
			// A redelivery of the same fact: keep the first receipt, whatever the input order.
			byKey[key] = e
		}
	}

	out := make([]OwnerFactEvent, 0, len(byKey))
	for _, e := range byKey {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].SourceKey != out[j].SourceKey {
			return out[i].SourceKey < out[j].SourceKey
		}
		return out[i].EventID < out[j].EventID
	})
	return out, nil
}

// factBefore orders facts by (SourceKey, EventID); equal-precedence identical facts resolve to the first.
func factBefore(a, b OwnerFactEvent) bool {
	if a.SourceKey != b.SourceKey {
		return a.SourceKey < b.SourceKey
	}
	return a.EventID < b.EventID
}

func sameOwnerFact(a, b OwnerFactEvent) bool {
	return a.State == b.State && a.Interval.Start.Equal(b.Interval.Start) && a.Interval.End.Equal(b.Interval.End) &&
		a.EventAt.Equal(b.EventAt) && a.PayloadDigest == b.PayloadDigest
}

// ResolveTimeline converts versioned owner facts into one canonical calculation timeline without inventing data.
// 1. Exact duplicate event IDs are deduplicated; conflicting duplicates fail closed (returns NOT_READY).
// 2. Overlap precedence is policy-defined. Unresolvable conflicts return NOT_READY.
// 3. Gaps remain visible and reduce coverage. No implicit running/stopped fill.
// 4. Source/event time and recorded/ingestion time remain separate.
// 5. Half-open UTC intervals [start, end) are preserved.
//
// Limits are checked before anything is materialized. Malformed facts are a contract error.
// The result does not depend on input order: facts are sorted canonically and equal-precedence
// identical facts resolve to the lowest event ID.
func ResolveTimeline(window HalfOpenInterval, facts []OwnerFactEvent, policy TimelinePolicy) (TimelineResolutionResult, error) {
	window = HalfOpenInterval{Start: window.Start.UTC(), End: window.End.UTC()}
	if err := checkTimelineInput(window, len(facts), policy); err != nil {
		return TimelineResolutionResult{}, err
	}

	// Deduplicate events first
	deduped, err := DeduplicateOwnerFacts(facts)
	if err != nil {
		if errors.Is(err, ErrMalformedFact) {
			return TimelineResolutionResult{}, err
		}
		return TimelineResolutionResult{
			Window: window,
			Readiness: EvaluateReadiness(map[ReasonCode]struct{}{
				ReasonConflictingFact: {},
			}),
		}, nil
	}

	// Filter and clip events that intersect the window, remembering the full interval of any
	// fact that extends past a window edge.
	clippedEvents := make([]OwnerFactEvent, 0, len(deduped))
	extendsPastEdge := map[factKey]HalfOpenInterval{}
	for _, e := range deduped {
		sect, ok := window.Intersection(e.Interval)
		if !ok {
			continue
		}
		if e.Interval.Start.Before(window.Start) || e.Interval.End.After(window.End) {
			extendsPastEdge[factKey{e.SourceKey, e.EventID}] = e.Interval
		}
		c := e
		c.Interval = sect
		clippedEvents = append(clippedEvents, c)
	}
	// edgeInterval returns the fact's full interval when this slice sits on a window edge that
	// the fact crosses.
	edgeInterval := func(slice HalfOpenInterval, ev OwnerFactEvent) *HalfOpenInterval {
		full, ok := extendsPastEdge[factKey{ev.SourceKey, ev.EventID}]
		if !ok {
			return nil
		}
		atStart := slice.Start.Equal(window.Start) && full.Start.Before(window.Start)
		atEnd := slice.End.Equal(window.End) && full.End.After(window.End)
		if !atStart && !atEnd {
			return nil
		}
		return &full
	}
	sort.Slice(clippedEvents, func(i, j int) bool {
		a, b := clippedEvents[i], clippedEvents[j]
		if !a.Interval.Start.Equal(b.Interval.Start) {
			return a.Interval.Start.Before(b.Interval.Start)
		}
		if !a.Interval.End.Equal(b.Interval.End) {
			return a.Interval.End.Before(b.Interval.End)
		}
		if a.SourceKey != b.SourceKey {
			return a.SourceKey < b.SourceKey
		}
		return a.EventID < b.EventID
	})

	// Collect all distinct time boundaries within the window
	timePointsMap := make(map[int64]time.Time)
	timePointsMap[window.Start.UnixNano()] = window.Start
	timePointsMap[window.End.UnixNano()] = window.End

	for _, e := range clippedEvents {
		timePointsMap[e.Interval.Start.UnixNano()] = e.Interval.Start
		timePointsMap[e.Interval.End.UnixNano()] = e.Interval.End
	}

	timePoints := make([]time.Time, 0, len(timePointsMap))
	for _, t := range timePointsMap {
		timePoints = append(timePoints, t)
	}
	sort.Slice(timePoints, func(i, j int) bool {
		return timePoints[i].Before(timePoints[j])
	})

	canonicalSegments := make([]CanonicalFactSegment, 0, len(timePoints)-1)
	coveredSec := new(big.Rat)
	gapSec := new(big.Rat)

	// Sweep the elementary slices [t[i], t[i+1]). Every boundary is a slice edge, so a fact
	// covers a slice exactly when it has started by t[i] and has not ended by t[i].
	var active []OwnerFactEvent
	next := 0
	for i := 0; i < len(timePoints)-1; i++ {
		tSlice, _ := NewHalfOpenInterval(timePoints[i], timePoints[i+1])
		sliceDuration := tSlice.DurationSeconds()

		for next < len(clippedEvents) && !clippedEvents[next].Interval.Start.After(tSlice.Start) {
			active = append(active, clippedEvents[next])
			next++
		}
		kept := active[:0]
		for _, e := range active {
			if e.Interval.End.After(tSlice.Start) {
				kept = append(kept, e)
			}
		}
		active = kept
		covering := active

		if len(covering) == 0 {
			// GAP: No implicit running or stopped fill
			canonicalSegments = append(canonicalSegments, CanonicalFactSegment{
				Interval: tSlice,
				State:    StateGap,
				IsGap:    true,
			})
			gapSec.Add(gapSec, sliceDuration)
			continue
		}

		if len(covering) == 1 {
			ev := covering[0]
			canonicalSegments = append(canonicalSegments, CanonicalFactSegment{
				Interval:      tSlice,
				State:         ev.State,
				SourceKey:     ev.SourceKey,
				EventID:       ev.EventID,
				Revision:      ev.Revision,
				IsGap:         false,
				EventInterval: edgeInterval(tSlice, ev),
			})
			coveredSec.Add(coveredSec, sliceDuration)
			continue
		}

		// Multiple covering events: resolve via policy precedence
		bestRank := 999999
		var winningEvent *OwnerFactEvent
		conflict := false

		for idx := range covering {
			ev := &covering[idx]
			rank := policy.PrecedenceRank(ev.State)
			if rank == -1 {
				// State not recognized by policy -> unresolvable conflict
				conflict = true
				break
			}
			if rank < bestRank {
				bestRank = rank
				winningEvent = ev
				conflict = false
			} else if rank == bestRank {
				// Same precedence rank conflict
				if winningEvent != nil && (winningEvent.State != ev.State || winningEvent.PayloadDigest != ev.PayloadDigest) {
					conflict = true
				} else if winningEvent != nil && factBefore(*ev, *winningEvent) {
					winningEvent = ev
				}
			}
		}

		if conflict || winningEvent == nil {
			// Overlap precedence is unresolvable -> fail closed with NOT_READY
			return TimelineResolutionResult{
				Window: window,
				Readiness: EvaluateReadiness(map[ReasonCode]struct{}{
					ReasonConflictingFact: {},
				}),
			}, nil
		}

		canonicalSegments = append(canonicalSegments, CanonicalFactSegment{
			Interval:      tSlice,
			State:         winningEvent.State,
			SourceKey:     winningEvent.SourceKey,
			EventID:       winningEvent.EventID,
			Revision:      winningEvent.Revision,
			IsGap:         false,
			EventInterval: edgeInterval(tSlice, *winningEvent),
		})
		coveredSec.Add(coveredSec, sliceDuration)
	}

	// Merge adjacent slices with identical state, event_id, source_key
	mergedSegments := make([]CanonicalFactSegment, 0, len(canonicalSegments))
	for _, seg := range canonicalSegments {
		if len(mergedSegments) == 0 {
			mergedSegments = append(mergedSegments, seg)
			continue
		}
		last := &mergedSegments[len(mergedSegments)-1]
		if last.State == seg.State && last.EventID == seg.EventID && last.SourceKey == seg.SourceKey && last.IsGap == seg.IsGap && last.Interval.End.Equal(seg.Interval.Start) {
			last.Interval.End = seg.Interval.End
			// Same fact, so the same full interval; keep it whichever slice touched the edge.
			if last.EventInterval == nil {
				last.EventInterval = seg.EventInterval
			}
		} else {
			mergedSegments = append(mergedSegments, seg)
		}
	}

	totalSec := window.DurationSeconds()
	covRatio := new(big.Rat).Quo(coveredSec, totalSec)

	// Coverage below the policy minimum is missing data, never a READY number computed over
	// the covered part only. Without a declared minimum, every second must be covered.
	reasons := map[ReasonCode]struct{}{}
	minCoverage := big.NewRat(1, 1)
	if policy.MinCoverage != nil {
		minCoverage = policy.MinCoverage
	}
	if coveredSec.Sign() == 0 || covRatio.Cmp(minCoverage) < 0 {
		reasons[ReasonMissingData] = struct{}{}
	}
	if policy.MaxSourceSilence > 0 {
		newest := map[string]time.Time{}
		for _, e := range clippedEvents {
			if e.SourceKey == calendarSource {
				continue
			}
			if cur, ok := newest[e.SourceKey]; !ok || e.RecordedAt.After(cur) {
				newest[e.SourceKey] = e.RecordedAt
			}
		}
		cutoff := window.End.Add(-policy.MaxSourceSilence)
		for _, last := range newest {
			if last.IsZero() || last.Before(cutoff) {
				reasons[ReasonStaleSource] = struct{}{}
			}
		}
	}

	return TimelineResolutionResult{
		Segments:       mergedSegments,
		Readiness:      EvaluateReadiness(reasons),
		Window:         window,
		CoveredSeconds: coveredSec,
		GapSeconds:     gapSec,
		CoverageRatio:  covRatio,
	}, nil
}

// ProjectLocalShiftToUTC deterministically projects local shifts into UTC intervals,
// accounting for cross-midnight shifts and deterministic DST gap and fold behaviors.
// localDateStr: "YYYY-MM-DD"
// startClock: "HH:MM:SS"
// endClock: "HH:MM:SS" (if endClock <= startClock, cross-midnight shift)
func ProjectLocalShiftToUTC(ianaZone, localDateStr, startClock, endClock string) (HalfOpenInterval, error) {
	loc, err := time.LoadLocation(ianaZone)
	if err != nil {
		return HalfOpenInterval{}, fmt.Errorf("%w: %s", ErrInvalidTimezone, ianaZone)
	}

	date, err := time.Parse("2006-01-02", localDateStr)
	if err != nil {
		return HalfOpenInterval{}, fmt.Errorf("%w: date %q, expected YYYY-MM-DD", ErrInvalidLocalTime, localDateStr)
	}
	start, err := parseClock(startClock)
	if err != nil {
		return HalfOpenInterval{}, err
	}
	end, err := parseClock(endClock)
	if err != nil {
		return HalfOpenInterval{}, err
	}

	// An end clock at or before the start clock ends on the next local day.
	endDate := date
	if end <= start {
		endDate = date.AddDate(0, 0, 1)
	}

	startTime, err := resolveLocalWall(loc, date, start)
	if err != nil {
		return HalfOpenInterval{}, err
	}
	endTime, err := resolveLocalWall(loc, endDate, end)
	if err != nil {
		return HalfOpenInterval{}, err
	}
	return NewHalfOpenInterval(startTime, endTime)
}

// parseClock parses "HH:MM" or "HH:MM:SS" into seconds after local midnight.
func parseClock(clock string) (time.Duration, error) {
	for _, layout := range []string{"15:04:05", "15:04"} {
		if t, err := time.Parse(layout, clock); err == nil {
			return time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute + time.Duration(t.Second())*time.Second, nil
		}
	}
	return 0, fmt.Errorf("%w: clock %q, expected HH:MM[:SS]", ErrInvalidLocalTime, clock)
}

// resolveLocalWall maps a local wall-clock time to UTC with an explicit DST rule:
// in a fall-back fold the earlier instant (the first occurrence) is chosen; a wall time
// inside a spring-forward gap does not exist and fails closed with ErrLocalTimeInDSTGap.
// time.Date leaves both cases unspecified, so it is not relied on here.
func resolveLocalWall(loc *time.Location, date time.Time, clock time.Duration) (time.Time, error) {
	wall := time.Date(date.Year(), date.Month(), date.Day(), 0, 0, 0, 0, time.UTC).Add(clock)

	// Real zones change offset at most once within a few days, so the offsets in force
	// two days either side of the wall time cover both sides of any transition.
	var found bool
	var best time.Time
	for _, probe := range []time.Time{wall.Add(-48 * time.Hour), wall, wall.Add(48 * time.Hour)} {
		_, offset := probe.In(loc).Zone()
		candidate := wall.Add(-time.Duration(offset) * time.Second)
		local := candidate.In(loc)
		if time.Date(local.Year(), local.Month(), local.Day(), local.Hour(), local.Minute(), local.Second(), 0, time.UTC).Equal(wall) {
			if !found || candidate.Before(best) {
				best, found = candidate, true
			}
		}
	}
	if !found {
		return time.Time{}, fmt.Errorf("%w: %s in %s", ErrLocalTimeInDSTGap, wall.Format("2006-01-02T15:04:05"), loc)
	}
	return best.UTC(), nil
}

// CounterReading represents a raw monotonic counter observation.
type CounterReading struct {
	ReadingID           string    `json:"reading_id"`
	CounterCode         string    `json:"counter_code"`
	RawValue            *big.Rat  `json:"raw_value"`
	OccurredAt          time.Time `json:"occurred_at"`
	RecordedAt          time.Time `json:"recorded_at"`
	HasResetEvidence    bool      `json:"has_reset_evidence"`
	HasRolloverEvidence bool      `json:"has_rollover_evidence"`
	RolloverThreshold   *big.Rat  `json:"rollover_threshold,omitempty"`
}

// NormalizedCounterDelta represents the verified delta between two counter readings.
type NormalizedCounterDelta struct {
	Interval   HalfOpenInterval `json:"interval"`
	DeltaUnits *big.Rat         `json:"delta_units"`
	EvidenceID string           `json:"evidence_id"`
}

// ComputeMonotonicCounterDeltas computes unit deltas from raw counter readings.
// Invariants enforced:
//   - Sorts by OccurredAt (source time) canonically, tiebroken by ReadingID.
//   - Negative counter reading values fail closed.
//   - Drop in counter without explicit rollover or reset evidence fails closed with ErrCounterDropNoEvidence.
//   - Rollover with explicit evidence adds the RolloverThreshold.
//   - Reset with explicit evidence treats the new value as starting from 0.
//   - Readings with the same ReadingID must be identical (kept once); otherwise ErrConflictingDuplicate.
//   - Readings at the same source time must agree on value (kept once, lowest ReadingID);
//     otherwise ErrConflictingCounter. Nothing is silently skipped.
//   - All readings must share one CounterCode.
func ComputeMonotonicCounterDeltas(readings []CounterReading) ([]NormalizedCounterDelta, error) {
	if len(readings) > MaxCounterReadings {
		return nil, fmt.Errorf("%w: %d counter readings exceeds %d", ErrTimelineLimit, len(readings), MaxCounterReadings)
	}
	for _, r := range readings {
		if r.RawValue == nil || r.RawValue.Sign() < 0 {
			return nil, ErrNegativeCounter
		}
		if r.CounterCode != readings[0].CounterCode {
			return nil, ErrMixedCounterCodes
		}
	}
	if len(readings) < 2 {
		return nil, nil
	}

	// Sort by OccurredAt, tiebreak by ReadingID
	sorted := make([]CounterReading, len(readings))
	copy(sorted, readings)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].OccurredAt.Equal(sorted[j].OccurredAt) {
			return sorted[i].ReadingID < sorted[j].ReadingID
		}
		return sorted[i].OccurredAt.Before(sorted[j].OccurredAt)
	})

	byID := make(map[string]CounterReading, len(sorted))
	ordered := make([]CounterReading, 0, len(sorted))
	for _, r := range sorted {
		if seen, ok := byID[r.ReadingID]; ok {
			if !sameCounterReading(seen, r) {
				return nil, fmt.Errorf("%w: reading_id %q has conflicting samples", ErrConflictingDuplicate, r.ReadingID)
			}
			continue
		}
		byID[r.ReadingID] = r
		if n := len(ordered); n > 0 && ordered[n-1].OccurredAt.Equal(r.OccurredAt) {
			if ordered[n-1].RawValue.Cmp(r.RawValue) != 0 {
				return nil, fmt.Errorf("%w: %s and %s at %s", ErrConflictingCounter, ordered[n-1].ReadingID, r.ReadingID, r.OccurredAt.UTC().Format(time.RFC3339Nano))
			}
			continue
		}
		ordered = append(ordered, r)
	}

	deltas := make([]NormalizedCounterDelta, 0, len(ordered))

	for i := 0; i < len(ordered)-1; i++ {
		prev := ordered[i]
		curr := ordered[i+1]

		interval, err := NewHalfOpenInterval(prev.OccurredAt, curr.OccurredAt)
		if err != nil {
			return nil, err
		}

		if curr.HasResetEvidence && curr.HasRolloverEvidence {
			return nil, fmt.Errorf("%w: reading %s carries both reset and rollover evidence", ErrInvalidRollover, curr.ReadingID)
		}
		if curr.HasResetEvidence {
			// An evidenced reset restarts the counter from zero whatever the raw values show.
			deltas = append(deltas, NormalizedCounterDelta{Interval: interval, DeltaUnits: new(big.Rat).Set(curr.RawValue), EvidenceID: curr.ReadingID})
			continue
		}

		cmp := curr.RawValue.Cmp(prev.RawValue)
		if cmp >= 0 && curr.HasRolloverEvidence {
			return nil, fmt.Errorf("%w: reading %s claims a rollover but the counter did not drop", ErrInvalidRollover, curr.ReadingID)
		}
		if cmp >= 0 {
			// Normal monotonic progression
			delta := new(big.Rat).Sub(curr.RawValue, prev.RawValue)
			deltas = append(deltas, NormalizedCounterDelta{
				Interval:   interval,
				DeltaUnits: delta,
				EvidenceID: curr.ReadingID,
			})
		} else {
			// Counter decreased
			if curr.HasRolloverEvidence && curr.RolloverThreshold != nil && curr.RolloverThreshold.Sign() > 0 {
				if prev.RawValue.Cmp(curr.RolloverThreshold) > 0 || curr.RawValue.Cmp(curr.RolloverThreshold) >= 0 {
					return nil, fmt.Errorf("%w: reading %s", ErrInvalidRollover, curr.ReadingID)
				}
				// Delta = (threshold - prev) + curr
				sub := new(big.Rat).Sub(curr.RolloverThreshold, prev.RawValue)
				delta := new(big.Rat).Add(sub, curr.RawValue)
				deltas = append(deltas, NormalizedCounterDelta{
					Interval:   interval,
					DeltaUnits: delta,
					EvidenceID: curr.ReadingID,
				})
			} else {
				// No explicit evidence: fail closed
				return nil, fmt.Errorf("%w: reading %s dropped from %s to %s at %s",
					ErrCounterDropNoEvidence, curr.ReadingID, prev.RawValue.String(), curr.RawValue.String(), curr.OccurredAt)
			}
		}
	}

	return deltas, nil
}

func sameCounterReading(a, b CounterReading) bool {
	ratEq := func(x, y *big.Rat) bool {
		if x == nil || y == nil {
			return x == nil && y == nil
		}
		return x.Cmp(y) == 0
	}
	return a.CounterCode == b.CounterCode && a.OccurredAt.Equal(b.OccurredAt) && ratEq(a.RawValue, b.RawValue) &&
		a.HasResetEvidence == b.HasResetEvidence && a.HasRolloverEvidence == b.HasRolloverEvidence &&
		ratEq(a.RolloverThreshold, b.RolloverThreshold)
}

// ComputeOwnerFactDigest calculates canonical SHA-256 digest for an owner fact.
func ComputeOwnerFactDigest(e OwnerFactEvent) string {
	raw := fmt.Sprintf("%s|%s|%s|%s|%s|%d",
		e.EventID,
		e.SourceKey,
		e.State,
		e.Interval.Start.Format(time.RFC3339Nano),
		e.Interval.End.Format(time.RFC3339Nano),
		e.Revision,
	)
	h := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(h[:])
}

// CalendarExclusion represents versioned planned breaks, changeovers, or shutdown exclusions.
type CalendarExclusion struct {
	ExclusionID string           `json:"exclusion_id"`
	State       TimelineState    `json:"state"`
	Interval    HalfOpenInterval `json:"interval"`
	Revision    int64            `json:"revision"`
	Description string           `json:"description,omitempty"`
	// RecordedAt is when the calendar system recorded this exclusion revision. Required:
	// recorded time is never fabricated from the planned interval.
	RecordedAt time.Time `json:"recorded_at"`
}

// CalendarExclusionPolicy groups versioned planned breaks, changeovers, and shutdown exclusions.
type CalendarExclusionPolicy struct {
	PlannedBreaks      []CalendarExclusion `json:"planned_breaks"`
	Changeovers        []CalendarExclusion `json:"changeovers"`
	ShutdownExclusions []CalendarExclusion `json:"shutdown_exclusions"`
	PolicyRevision     int64               `json:"policy_revision"`
}

// ResolveTimelineWithExclusions merges versioned calendar exclusions (planned breaks, changeovers,
// shutdown exclusions) with operational owner facts into one canonical calculation timeline.
func ResolveTimelineWithExclusions(
	window HalfOpenInterval,
	facts []OwnerFactEvent,
	exclusions CalendarExclusionPolicy,
	policy TimelinePolicy,
) (TimelineResolutionResult, error) {
	total := len(facts) + len(exclusions.PlannedBreaks) + len(exclusions.Changeovers) + len(exclusions.ShutdownExclusions)
	if err := checkTimelineInput(window, total, policy); err != nil {
		return TimelineResolutionResult{}, err
	}
	for _, group := range [][]CalendarExclusion{exclusions.PlannedBreaks, exclusions.Changeovers, exclusions.ShutdownExclusions} {
		for _, ex := range group {
			if ex.RecordedAt.IsZero() {
				return TimelineResolutionResult{}, fmt.Errorf("%w: exclusion %q has no recorded_at", ErrMalformedFact, ex.ExclusionID)
			}
		}
	}
	allFacts := make([]OwnerFactEvent, 0, total)
	allFacts = append(allFacts, facts...)

	addExclusion := func(ex CalendarExclusion, fallbackState TimelineState) {
		state := ex.State
		if state == "" {
			state = fallbackState
		}
		allFacts = append(allFacts, OwnerFactEvent{
			EventID:       ex.ExclusionID,
			SourceKey:     calendarSource,
			State:         state,
			Interval:      ex.Interval,
			EventAt:       ex.Interval.Start,
			RecordedAt:    ex.RecordedAt,
			Revision:      ex.Revision,
			PayloadDigest: fmt.Sprintf("exclusion|%s|%d", ex.ExclusionID, ex.Revision),
		})
	}

	for _, b := range exclusions.PlannedBreaks {
		addExclusion(b, StatePlannedBreak)
	}
	for _, c := range exclusions.Changeovers {
		addExclusion(c, StateChangeover)
	}
	for _, s := range exclusions.ShutdownExclusions {
		addExclusion(s, StateShutdownExclusion)
	}

	return ResolveTimeline(window, allFacts, policy)
}

// VersionedTimelineCalculation represents an immutable, versioned calculation record.
// Published history is never mutated; late arriving data produces a strictly greater revision.
type VersionedTimelineCalculation struct {
	CalculationID   string                   `json:"calculation_id"`
	Window          HalfOpenInterval         `json:"window"`
	Revision        int64                    `json:"revision"`
	Timeline        TimelineResolutionResult `json:"timeline"`
	SuperseededByID string                   `json:"superseeded_by_id,omitempty"`
	GeneratedAtUTC  time.Time                `json:"generated_at_utc"`
	DatasetDigest   string                   `json:"dataset_digest"`
}

// BuildNewCalculationRevision constructs a new revision when late data arrives, ensuring published history is untouched.
// The kernel reads no clock: the caller supplies generatedAt, and the previous revision must
// belong to the same calculation and window.
func BuildNewCalculationRevision(
	prev *VersionedTimelineCalculation,
	calcID string,
	window HalfOpenInterval,
	facts []OwnerFactEvent,
	exclusions CalendarExclusionPolicy,
	policy TimelinePolicy,
	datasetDigest string,
	generatedAt time.Time,
) (VersionedTimelineCalculation, error) {
	if generatedAt.IsZero() {
		return VersionedTimelineCalculation{}, ErrMissingGeneratedAt
	}
	if prev != nil && (prev.CalculationID != calcID || !prev.Window.Start.Equal(window.Start) || !prev.Window.End.Equal(window.End)) {
		return VersionedTimelineCalculation{}, ErrRevisionLineage
	}
	res, err := ResolveTimelineWithExclusions(window, facts, exclusions, policy)
	if err != nil {
		return VersionedTimelineCalculation{}, err
	}

	nextRev := int64(1)
	if prev != nil {
		nextRev = prev.Revision + 1
	}

	return VersionedTimelineCalculation{
		CalculationID:  calcID,
		Window:         window,
		Revision:       nextRev,
		Timeline:       res,
		GeneratedAtUTC: generatedAt.UTC(),
		DatasetDigest:  datasetDigest,
	}, nil
}

// calendarSource labels facts synthesized from versioned calendar exclusions.
const calendarSource = "calendar_exclusion"

// TimelineDigestSchemaVersion versions the canonical NormalizedTimelineV1 body.
const TimelineDigestSchemaVersion = 1

type digestSegment struct {
	Start     string `json:"start"`
	End       string `json:"end"`
	State     string `json:"state"`
	SourceKey string `json:"source_key"`
	EventID   string `json:"event_id"`
	Revision  int64  `json:"revision"`
	IsGap     bool   `json:"is_gap"`
}

type digestPolicy struct {
	PrecedenceOrder    []string `json:"precedence_order"`
	MinCoverage        string   `json:"min_coverage"`
	MaxSourceSilenceNs int64    `json:"max_source_silence_ns"`
	MaxFacts           int      `json:"max_facts"`
	MaxWindowNs        int64    `json:"max_window_ns"`
}

// TimelineDigest is the SHA-256 of the normalized timeline through the shared canonical
// serializer, bound to tenant and site and to the policy that produced it: window,
// ordered segments with their source event and revision, exact coverage, readiness and
// every policy rule. The same dataset and policy give the same digest at any time; any
// changed fact, revision or rule changes it, and it is never portable across tenants.
func TimelineDigest(tenantID, siteID string, r TimelineResolutionResult, policy TimelinePolicy) (string, error) {
	limits, err := policy.Limits.effective()
	if err != nil {
		return "", err
	}
	rat := func(v *big.Rat) string {
		if v == nil {
			return ""
		}
		return v.RatString()
	}
	utc := func(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
	segments := make([]digestSegment, len(r.Segments))
	for i, s := range r.Segments {
		segments[i] = digestSegment{utc(s.Interval.Start), utc(s.Interval.End), string(s.State), s.SourceKey, s.EventID, s.Revision, s.IsGap}
	}
	order := make([]string, len(policy.PrecedenceOrder))
	for i, s := range policy.PrecedenceOrder {
		order[i] = string(s)
	}
	codes := make([]string, len(r.Readiness.ReasonCodes))
	for i, c := range r.Readiness.ReasonCodes {
		codes[i] = string(c)
	}
	minCoverage := "1"
	if policy.MinCoverage != nil {
		minCoverage = policy.MinCoverage.RatString()
	}
	_, digest, err := canonical.Envelope("NormalizedTimelineV1", TimelineDigestSchemaVersion, tenantID, siteID, struct {
		WindowStart    string          `json:"window_start"`
		WindowEnd      string          `json:"window_end"`
		Segments       []digestSegment `json:"segments"`
		CoveredSeconds string          `json:"covered_seconds"`
		GapSeconds     string          `json:"gap_seconds"`
		CoverageRatio  string          `json:"coverage_ratio"`
		Status         string          `json:"status"`
		ReasonCodes    []string        `json:"reason_codes"`
		Policy         digestPolicy    `json:"policy"`
	}{
		utc(r.Window.Start), utc(r.Window.End), segments,
		rat(r.CoveredSeconds), rat(r.GapSeconds), rat(r.CoverageRatio),
		string(r.Readiness.Status), codes,
		digestPolicy{order, minCoverage, int64(policy.MaxSourceSilence), limits.MaxFacts, int64(limits.MaxWindow)},
	})
	return digest, err
}
