package workstation

import (
	"errors"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/limits"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/strictjson"
)

const (
	maxPlannerOrders     = 50
	maxPlannerOperations = 200
	maxPlannerStations   = 100
	maxPlannerDowntimes  = 100
)

var ErrNoFeasibleSchedule = errors.New("no feasible schedule within the requested shift")

// PlanningRequestV1 is the bounded, deterministic input to the advisory shift
// planner. It contains no tenant/site authority; the API supplies that scope.
type PlanningRequestV1 struct {
	SchemaVersion int                     `json:"schema_version"`
	LineID        string                  `json:"line_id"`
	StartsAt      string                  `json:"starts_at"`
	EndsAt        string                  `json:"ends_at"`
	Timezone      string                  `json:"timezone"`
	Orders        []PlanningOrderV1       `json:"orders"`
	Workstations  []PlanningWorkstationV1 `json:"workstations"`
	Downtimes     []PlanningDowntimeV1    `json:"downtimes"`
}

type PlanningOrderV1 struct {
	JobOrderID       string                `json:"job_order_id"`
	JobOrderRevision int64                 `json:"job_order_revision"`
	ReadyAt          string                `json:"ready_at,omitempty"`
	DueAt            string                `json:"due_at,omitempty"`
	Priority         int32                 `json:"priority"`
	Operations       []PlanningOperationV1 `json:"operations"`
}

type PlanningOperationV1 struct {
	OperationID    string `json:"operation_id"`
	Sequence       int32  `json:"sequence"`
	WorkstationID  string `json:"workstation_id"`
	Name           string `json:"name"`
	Quantity       int64  `json:"quantity"`
	SecondsPerUnit string `json:"seconds_per_unit"`
	DurationBasis  string `json:"duration_basis"`
}

type PlanningWorkstationV1 struct {
	ID       string `json:"id"`
	Capacity int32  `json:"capacity"`
}

type PlanningDowntimeV1 struct {
	WorkstationID string `json:"workstation_id"`
	StartsAt      string `json:"starts_at"`
	EndsAt        string `json:"ends_at"`
	Demand        int32  `json:"demand"`
}

type PlannedOperationV1 struct {
	JobOrderID    string `json:"job_order_id"`
	OperationID   string `json:"operation_id"`
	Sequence      int32  `json:"sequence"`
	WorkstationID string `json:"workstation_id"`
	StartsAt      string `json:"starts_at"`
	EndsAt        string `json:"ends_at"`
	DurationSecs  int64  `json:"duration_seconds"`
	DurationBasis string `json:"duration_basis"`
}

// PlanSuggestionV1 is a deterministic heuristic proposal. It is never an
// approved shift plan and must be reviewed before workstation delivery.
type PlanSuggestionV1 struct {
	SchemaVersion int                  `json:"schema_version"`
	Method        string               `json:"method"`
	IsOptimized   bool                 `json:"is_optimized"`
	SolverStatus  string               `json:"solver_status,omitempty"`
	Assignments   []PlannedOperationV1 `json:"assignments"`
}

// Validate rejects ambiguous routes and invalid capacity inputs. The planner
// only recommends a schedule; it cannot publish one.
func (p PlanningRequestV1) Validate() error {
	if p.SchemaVersion != 1 || !canonicalID(p.LineID) || len(p.Orders) == 0 || len(p.Orders) > maxPlannerOrders || len(p.Workstations) == 0 || len(p.Workstations) > maxPlannerStations || len(p.Downtimes) > maxPlannerDowntimes {
		return ErrInvalidShiftPlan
	}
	start, startOK := validTime(p.StartsAt)
	end, endOK := validTime(p.EndsAt)
	if len(p.Timezone) > 64 || !ianaZone.MatchString(p.Timezone) {
		return ErrInvalidShiftPlan
	}
	location, zoneErr := time.LoadLocation(p.Timezone)
	if !startOK || !endOK || zoneErr != nil || !start.Before(end) || end.Sub(start) > 24*time.Hour || !zoneMatches(start, location) || !zoneMatches(end, location) {
		return ErrInvalidShiftPlan
	}
	stationCapacity := make(map[string]int32, len(p.Workstations))
	for _, station := range p.Workstations {
		if !canonicalID(station.ID) || station.Capacity < 1 || station.Capacity > 64 {
			return ErrInvalidShiftPlan
		}
		if _, exists := stationCapacity[station.ID]; exists {
			return ErrInvalidShiftPlan
		}
		stationCapacity[station.ID] = station.Capacity
	}
	seenOrders := make(map[string]struct{}, len(p.Orders))
	operationCount := 0
	for _, order := range p.Orders {
		if !canonicalID(order.JobOrderID) || order.JobOrderRevision < 1 || len(order.Operations) == 0 {
			return ErrInvalidShiftPlan
		}
		if _, exists := seenOrders[order.JobOrderID]; exists {
			return ErrInvalidShiftPlan
		}
		seenOrders[order.JobOrderID] = struct{}{}
		if order.ReadyAt != "" {
			ready, ok := validTime(order.ReadyAt)
			if !ok || ready.After(end) {
				return ErrInvalidShiftPlan
			}
		}
		if order.DueAt != "" {
			due, ok := validTime(order.DueAt)
			if !ok || !due.After(start) {
				return ErrInvalidShiftPlan
			}
		}
		lastSequence := int32(0)
		seenOperationIDs := make(map[string]struct{}, len(order.Operations))
		for _, op := range order.Operations {
			operationCount++
			micros, err := rateMicros(op.SecondsPerUnit)
			if !canonicalID(op.OperationID) || op.Sequence < 1 || op.Sequence <= lastSequence || !canonicalID(op.WorkstationID) || stationCapacity[op.WorkstationID] == 0 || !validText(op.Name, 128) || op.Quantity < 1 || !validDecimal6(op.SecondsPerUnit) || err != nil || micros < 1 || micros > (24*60*60*1_000_000)/op.Quantity || !validText(op.DurationBasis, 64) {
				return ErrInvalidShiftPlan
			}
			if _, exists := seenOperationIDs[op.OperationID]; exists {
				return ErrInvalidShiftPlan
			}
			seenOperationIDs[op.OperationID] = struct{}{}
			lastSequence = op.Sequence
		}
	}
	if operationCount == 0 || operationCount > maxPlannerOperations {
		return ErrInvalidShiftPlan
	}
	for _, downtime := range p.Downtimes {
		blockStart, startOK := validTime(downtime.StartsAt)
		blockEnd, endOK := validTime(downtime.EndsAt)
		capacity := stationCapacity[downtime.WorkstationID]
		if capacity == 0 || !startOK || !endOK || !blockStart.Before(blockEnd) || blockEnd.Before(start) || blockStart.After(end) || downtime.Demand < 1 || downtime.Demand > capacity {
			return ErrInvalidShiftPlan
		}
	}
	return nil
}

func DecodePlanningRequest(data []byte) (PlanningRequestV1, error) {
	var request PlanningRequestV1
	if strictjson.Decode(data, &request, limits.RequestBodyBytes) != nil || request.Validate() != nil {
		return PlanningRequestV1{}, ErrInvalidShiftPlan
	}
	return request, nil
}

func rateMicros(value string) (int64, error) {
	whole, fraction, hasFraction := strings.Cut(value, ".")
	units, err := strconv.ParseInt(whole, 10, 64)
	if err != nil || units > math.MaxInt64/1_000_000 || hasFraction && (len(fraction) == 0 || len(fraction) > 6) {
		return 0, ErrInvalidShiftPlan
	}
	if hasFraction {
		fraction += strings.Repeat("0", 6-len(fraction))
		fractionalUnits, parseErr := strconv.ParseInt(fraction, 10, 64)
		if parseErr != nil {
			return 0, ErrInvalidShiftPlan
		}
		return units*1_000_000 + fractionalUnits, nil
	}
	return units * 1_000_000, nil
}

func operationDurationSeconds(operation PlanningOperationV1) (int64, error) {
	unitsPerSecond, err := rateMicros(operation.SecondsPerUnit)
	if err != nil || operation.Quantity < 1 || unitsPerSecond > (24*60*60*1_000_000)/operation.Quantity {
		return 0, ErrInvalidShiftPlan
	}
	durationMicros := unitsPerSecond * operation.Quantity
	return (durationMicros + 999_999) / 1_000_000, nil
}

type scheduledInterval struct {
	start, end time.Time
	demand     int32
}

// PlanGreedy applies the MES scheduler's event-driven earliest-start dispatch
// in the Go runtime. It keeps station capacity, precedence, and downtime hard;
// it does not claim CP-SAT optimality.
func PlanGreedy(p PlanningRequestV1) (PlanSuggestionV1, error) {
	if err := p.Validate(); err != nil {
		return PlanSuggestionV1{}, err
	}
	anchor, _ := validTime(p.StartsAt)
	shiftEnd, _ := validTime(p.EndsAt)
	location, _ := time.LoadLocation(p.Timezone)
	capacity := make(map[string]int32, len(p.Workstations))
	busy := make(map[string][]scheduledInterval, len(p.Workstations))
	for _, station := range p.Workstations {
		capacity[station.ID] = station.Capacity
	}
	for _, downtime := range p.Downtimes {
		start, _ := validTime(downtime.StartsAt)
		end, _ := validTime(downtime.EndsAt)
		if start.Before(anchor) {
			start = anchor
		}
		if end.After(shiftEnd) {
			end = shiftEnd
		}
		if start.Before(end) {
			busy[downtime.WorkstationID] = append(busy[downtime.WorkstationID], scheduledInterval{start, end, downtime.Demand})
		}
	}

	orderNext := make([]int, len(p.Orders))
	orderReady := make([]time.Time, len(p.Orders))
	for i, order := range p.Orders {
		orderReady[i] = anchor
		if order.ReadyAt != "" {
			ready, _ := validTime(order.ReadyAt)
			if ready.After(orderReady[i]) {
				orderReady[i] = ready
			}
		}
	}
	remaining := 0
	for _, order := range p.Orders {
		remaining += len(order.Operations)
	}
	assignments := make([]PlannedOperationV1, 0, remaining)
	for remaining > 0 {
		bestOrder, bestOp := -1, -1
		var bestStart time.Time
		var bestEnd time.Time
		var bestSeconds int64
		var bestDue time.Time
		var bestPriority int32
		for oi, order := range p.Orders {
			if orderNext[oi] >= len(order.Operations) {
				continue
			}
			opIndex := orderNext[oi]
			op := order.Operations[opIndex]
			seconds, _ := operationDurationSeconds(op)
			candidate, ok := earliestCapacitySlot(orderReady[oi], seconds, capacity[op.WorkstationID], busy[op.WorkstationID])
			if !ok || candidate.Add(time.Duration(seconds)*time.Second).After(shiftEnd) {
				continue
			}
			end := candidate.Add(time.Duration(seconds) * time.Second)
			due := time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC)
			if order.DueAt != "" {
				due, _ = validTime(order.DueAt)
			}
			if bestOrder < 0 || candidate.Before(bestStart) || candidate.Equal(bestStart) && (due.Before(bestDue) || due.Equal(bestDue) && (order.Priority < bestPriority || order.Priority == bestPriority && (order.JobOrderID < p.Orders[bestOrder].JobOrderID || order.JobOrderID == p.Orders[bestOrder].JobOrderID && op.Sequence < p.Orders[bestOrder].Operations[bestOp].Sequence))) {
				bestOrder, bestOp = oi, opIndex
				bestStart, bestEnd, bestSeconds, bestDue, bestPriority = candidate, end, seconds, due, order.Priority
			}
		}
		if bestOrder < 0 {
			return PlanSuggestionV1{}, ErrNoFeasibleSchedule
		}
		order := p.Orders[bestOrder]
		op := order.Operations[bestOp]
		assignments = append(assignments, PlannedOperationV1{
			JobOrderID: order.JobOrderID, OperationID: op.OperationID, Sequence: op.Sequence,
			WorkstationID: op.WorkstationID, StartsAt: bestStart.In(location).Format(time.RFC3339Nano),
			EndsAt: bestEnd.In(location).Format(time.RFC3339Nano), DurationSecs: bestSeconds, DurationBasis: op.DurationBasis,
		})
		busy[op.WorkstationID] = append(busy[op.WorkstationID], scheduledInterval{bestStart, bestEnd, 1})
		orderReady[bestOrder] = bestEnd
		orderNext[bestOrder]++
		remaining--
	}
	sort.Slice(assignments, func(i, j int) bool {
		if assignments[i].StartsAt != assignments[j].StartsAt {
			return assignments[i].StartsAt < assignments[j].StartsAt
		}
		if assignments[i].JobOrderID != assignments[j].JobOrderID {
			return assignments[i].JobOrderID < assignments[j].JobOrderID
		}
		return assignments[i].Sequence < assignments[j].Sequence
	})
	return PlanSuggestionV1{SchemaVersion: 1, Method: "deterministic_greedy", IsOptimized: false, Assignments: assignments}, nil
}

func earliestCapacitySlot(ready time.Time, seconds int64, capacity int32, intervals []scheduledInterval) (time.Time, bool) {
	candidate := ready
	duration := time.Duration(seconds) * time.Second
	for attempts := 0; attempts <= len(intervals); attempts++ {
		end := candidate.Add(duration)
		type event struct {
			at      time.Time
			index   int
			isStart bool
		}
		active := make(map[int]struct{})
		events := make([]event, 0, len(intervals)*2)
		for i, interval := range intervals {
			if !interval.start.Before(end) || !candidate.Before(interval.end) {
				continue
			}
			if !interval.start.After(candidate) {
				active[i] = struct{}{}
			} else {
				events = append(events, event{at: interval.start, index: i, isStart: true})
			}
			if interval.end.Before(end) {
				events = append(events, event{at: interval.end, index: i})
			}
		}
		var occupied int32
		release := time.Time{}
		for index := range active {
			interval := intervals[index]
			occupied += interval.demand
			if release.IsZero() || interval.end.Before(release) {
				release = interval.end
			}
		}
		if occupied+1 > capacity {
			if !release.After(candidate) {
				return time.Time{}, false
			}
			candidate = release
			continue
		}
		sort.Slice(events, func(i, j int) bool {
			if events[i].at.Equal(events[j].at) {
				return !events[i].isStart && events[j].isStart
			}
			return events[i].at.Before(events[j].at)
		})
		conflict := false
		for i := 0; i < len(events); {
			at := events[i].at
			for i < len(events) && events[i].at.Equal(at) && !events[i].isStart {
				delete(active, events[i].index)
				i++
			}
			for i < len(events) && events[i].at.Equal(at) && events[i].isStart {
				active[events[i].index] = struct{}{}
				i++
			}
			occupied = 0
			release = time.Time{}
			for index := range active {
				interval := intervals[index]
				occupied += interval.demand
				if release.IsZero() || interval.end.Before(release) {
					release = interval.end
				}
			}
			if occupied+1 > capacity {
				if !release.After(candidate) {
					return time.Time{}, false
				}
				candidate = release
				conflict = true
				break
			}
		}
		if !conflict {
			return candidate, true
		}
	}
	return time.Time{}, false
}
