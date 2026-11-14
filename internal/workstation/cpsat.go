package workstation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/limits"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/strictjson"
)

var (
	ErrSolverUnavailable   = errors.New("CP-SAT solver unavailable")
	ErrSolverUnknown       = errors.New("CP-SAT solver found no result within its search budget")
	ErrSolverOutput        = errors.New("invalid CP-SAT solver output")
	ErrSolverTimePrecision = errors.New("CP-SAT requires whole-second timestamps")
)

// cpsatLimit is the rule 01 call deadline; a variable only so tests can shorten it.
var cpsatLimit = 6 * time.Second

type cpsatResponseV1 struct {
	SchemaVersion   int                  `json:"schema_version"`
	Method          string               `json:"method"`
	Status          string               `json:"status"`
	IsOptimized     bool                 `json:"is_optimized"`
	Assignments     []PlannedOperationV1 `json:"assignments"`
	MakespanSeconds *int64               `json:"makespan_seconds,omitempty"`
}

// PlanCPsat returns a recommendation from the bundled solver process. A nil or
// missing solver path leaves manual planning available without CP-SAT installed.
func PlanCPsat(ctx context.Context, request PlanningRequestV1, solverPath string) (PlanSuggestionV1, error) {
	if err := request.Validate(); err != nil {
		return PlanSuggestionV1{}, err
	}
	if !wholeSecondTimes(request) {
		return PlanSuggestionV1{}, ErrSolverTimePrecision
	}
	if solverPath == "" {
		solverPath = os.Getenv("SUPERVISOR_WORKSTATION_CPSAT_PATH")
		if solverPath == "" {
			return PlanSuggestionV1{}, ErrSolverUnavailable
		}
	}
	// The solver requires every list as a JSON array; a nil slice would marshal as null and be
	// refused, silently falling back to manual planning (found in WIS-699 verification).
	if request.Downtimes == nil {
		request.Downtimes = []PlanningDowntimeV1{}
	}
	input, err := json.Marshal(request)
	if err != nil {
		return PlanSuggestionV1{}, ErrSolverOutput
	}
	if len(input) > limits.RequestBodyBytes {
		return PlanSuggestionV1{}, ErrInvalidShiftPlan
	}
	callCtx, cancel := context.WithTimeout(ctx, cpsatLimit)
	defer cancel()
	// WIS-699: the solver starts through this binary's confinement step (CPSATChildFlag), which
	// sets resource limits and then executes it in place. Its environment is the fixed list below
	// and nothing else: OR-Tools is found through the solver's RPATH, so no LD_LIBRARY_PATH is
	// passed on.
	self, err := os.Executable()
	if err != nil {
		return PlanSuggestionV1{}, ErrSolverUnavailable
	}
	absSolver, err := filepath.Abs(solverPath)
	if err != nil {
		return PlanSuggestionV1{}, ErrSolverUnavailable
	}
	if info, err := os.Stat(absSolver); err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return PlanSuggestionV1{}, ErrSolverUnavailable // missing or not executable: nothing to start
	}
	cmd := exec.CommandContext(callCtx, self, CPSATChildFlag, absSolver, "--time-limit-seconds", "5", "--seed", "1", "--workers", "1")
	cmd.Env = []string{"LANG=C", "LC_ALL=C", "TZ=UTC"}
	cmd.Stdin = bytes.NewReader(input)
	stdout, stderr := &boundedBuffer{limit: limits.RequestBodyBytes}, &boundedBuffer{limit: 4096}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Run(); err != nil {
		if callCtx.Err() != nil {
			if errors.Is(callCtx.Err(), context.Canceled) {
				return PlanSuggestionV1{}, context.Canceled
			}
			return PlanSuggestionV1{}, ErrSolverUnknown
		}
		if stdout.overflow {
			return PlanSuggestionV1{}, ErrSolverOutput
		}
		// Rule 01: a solver that cannot be started (missing, removed or not executable) or
		// that exits non-zero is unavailable, not a source of invalid output. Manual planning
		// is unaffected either way.
		return PlanSuggestionV1{}, ErrSolverUnavailable
	}
	if stdout.overflow {
		return PlanSuggestionV1{}, ErrSolverOutput
	}
	var response cpsatResponseV1
	if strictjson.Decode(stdout.Bytes(), &response, limits.RequestBodyBytes) != nil || response.SchemaVersion != 1 || response.Method != "ortools_cp_sat" {
		return PlanSuggestionV1{}, ErrSolverOutput
	}
	switch response.Status {
	case "INFEASIBLE":
		if response.IsOptimized || len(response.Assignments) != 0 {
			return PlanSuggestionV1{}, ErrSolverOutput
		}
		return PlanSuggestionV1{}, ErrNoFeasibleSchedule
	case "UNKNOWN":
		if response.IsOptimized || len(response.Assignments) != 0 {
			return PlanSuggestionV1{}, ErrSolverOutput
		}
		return PlanSuggestionV1{}, ErrSolverUnknown
	case "FEASIBLE", "OPTIMAL":
	default:
		return PlanSuggestionV1{}, ErrSolverOutput
	}
	if response.IsOptimized != (response.Status == "OPTIMAL") || validateSolverAssignments(request, response) != nil {
		return PlanSuggestionV1{}, ErrSolverOutput
	}
	if response.MakespanSeconds == nil || *response.MakespanSeconds < 1 {
		return PlanSuggestionV1{}, ErrSolverOutput
	}
	return PlanSuggestionV1{SchemaVersion: 1, Method: response.Method, IsOptimized: response.IsOptimized, SolverStatus: response.Status, Assignments: response.Assignments}, nil
}

func wholeSecondTimes(request PlanningRequestV1) bool {
	values := []string{request.StartsAt, request.EndsAt}
	for _, order := range request.Orders {
		values = append(values, order.ReadyAt, order.DueAt)
	}
	for _, downtime := range request.Downtimes {
		values = append(values, downtime.StartsAt, downtime.EndsAt)
	}
	for _, value := range values {
		if value == "" {
			continue
		}
		parsed, ok := validTime(value)
		if !ok || parsed.Nanosecond() != 0 {
			return false
		}
	}
	return true
}

func validateSolverAssignments(request PlanningRequestV1, result cpsatResponseV1) error {
	type expected struct {
		operation PlanningOperationV1
		order     PlanningOrderV1
	}
	want := make(map[string]expected)
	for _, order := range request.Orders {
		for _, operation := range order.Operations {
			want[order.JobOrderID+"/"+operation.OperationID] = expected{operation, order}
		}
	}
	if len(result.Assignments) != len(want) {
		return ErrSolverOutput
	}
	shiftStart, _ := validTime(request.StartsAt)
	shiftEnd, _ := validTime(request.EndsAt)
	capacity := make(map[string]int32, len(request.Workstations))
	busy := make(map[string][]scheduledInterval, len(request.Workstations))
	for _, station := range request.Workstations {
		capacity[station.ID] = station.Capacity
	}
	for _, down := range request.Downtimes {
		start, _ := validTime(down.StartsAt)
		end, _ := validTime(down.EndsAt)
		if start.Before(shiftStart) {
			start = shiftStart
		}
		if end.After(shiftEnd) {
			end = shiftEnd
		}
		if start.Before(end) {
			busy[down.WorkstationID] = append(busy[down.WorkstationID], scheduledInterval{start, end, down.Demand})
		}
	}
	byOrder := make(map[string]map[int32]time.Time)
	maxEnd := int64(0)
	for _, assignment := range result.Assignments {
		key := assignment.JobOrderID + "/" + assignment.OperationID
		expectedOp, ok := want[key]
		if !ok {
			return ErrSolverOutput
		}
		delete(want, key)
		operation, order := expectedOp.operation, expectedOp.order
		start, startOK := validTime(assignment.StartsAt)
		end, endOK := validTime(assignment.EndsAt)
		seconds, durationErr := operationDurationSeconds(operation)
		if durationErr != nil || !startOK || !endOK || assignment.Sequence != operation.Sequence || assignment.WorkstationID != operation.WorkstationID || assignment.DurationSecs != seconds || assignment.DurationBasis != operation.DurationBasis || start.Before(shiftStart) || end.After(shiftEnd) || !end.Equal(start.Add(time.Duration(seconds)*time.Second)) {
			return ErrSolverOutput
		}
		if order.ReadyAt != "" {
			ready, _ := validTime(order.ReadyAt)
			if start.Before(ready) {
				return ErrSolverOutput
			}
		}
		if byOrder[order.JobOrderID] == nil {
			byOrder[order.JobOrderID] = map[int32]time.Time{}
		}
		byOrder[order.JobOrderID][operation.Sequence] = start
		if elapsed := int64(end.Sub(shiftStart) / time.Second); elapsed > maxEnd {
			maxEnd = elapsed
		}
		busy[operation.WorkstationID] = append(busy[operation.WorkstationID], scheduledInterval{start, end, 1})
	}
	if len(want) != 0 {
		return ErrSolverOutput
	}
	if result.MakespanSeconds == nil || *result.MakespanSeconds != maxEnd {
		return ErrSolverOutput
	}
	for _, order := range request.Orders {
		var previousEnd time.Time
		for _, operation := range order.Operations {
			start := byOrder[order.JobOrderID][operation.Sequence]
			if !previousEnd.IsZero() && start.Before(previousEnd) {
				return ErrSolverOutput
			}
			seconds, err := operationDurationSeconds(operation)
			if err != nil {
				return ErrSolverOutput
			}
			previousEnd = start.Add(time.Duration(seconds) * time.Second)
		}
	}
	for station, intervals := range busy {
		type event struct {
			at    time.Time
			delta int32
		}
		events := make([]event, 0, len(intervals)*2)
		for _, interval := range intervals {
			events = append(events, event{interval.start, interval.demand}, event{interval.end, -interval.demand})
		}
		sort.Slice(events, func(i, j int) bool {
			if events[i].at.Equal(events[j].at) {
				return events[i].delta < events[j].delta
			}
			return events[i].at.Before(events[j].at)
		})
		var used int32
		for _, item := range events {
			used += item.delta
			if used < 0 || used > capacity[station] {
				return ErrSolverOutput
			}
		}
	}
	return nil
}

type boundedBuffer struct {
	bytes.Buffer
	limit    int
	overflow bool
}

func (b *boundedBuffer) Write(data []byte) (int, error) {
	if b.Len()+len(data) > b.limit {
		b.overflow = true
		return 0, io.ErrShortWrite
	}
	return b.Buffer.Write(data)
}
