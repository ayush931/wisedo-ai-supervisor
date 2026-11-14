package workstation

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/ingestion"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/production"
)

var (
	ErrInvalidPlanSelection = errors.New("invalid plan selection")
	ErrPlannerLimit         = errors.New("planner limit exceeded")
	ErrPlannerBusy          = errors.New("a suggestion is already running for this site")
	ErrPlannerData          = errors.New("station profiles or approved downtime are unavailable")
)

// ShiftPlanSelectionV1 contains selection only. Durations, routing, revisions and
// capacity cannot be supplied by callers. Priority/due time come from the order.
type ShiftPlanSelectionV1 struct {
	SchemaVersion int      `json:"schema_version"`
	LineID        string   `json:"line_id" contract:"uuid"`
	StartsAt      string   `json:"starts_at" contract:"datetime"`
	EndsAt        string   `json:"ends_at" contract:"datetime"`
	Timezone      string   `json:"timezone" contract:"max=64"`
	JobOrderIDs   []string `json:"job_order_ids"`
}
type UnplacedOrderV1 struct {
	JobOrderID string `json:"job_order_id" contract:"uuid"`
	ReasonCode string `json:"reason_code"`
}

// Assignments can be submitted as a draft through the existing human-controlled
// plan API. Operations are a timing preview; ShiftPlanBodyV1 has no timed rows.
type ShiftPlanSuggestionV1 struct {
	SchemaVersion     int                  `json:"schema_version"`
	Method            string               `json:"method"`
	IsOptimized       bool                 `json:"is_optimized"`
	SolverStatus      string               `json:"solver_status,omitempty"`
	FallbackReason    string               `json:"fallback_reason,omitempty"`
	ProcessSnapshotID string               `json:"process_snapshot_id,omitempty" contract:"uuid"`
	JobOrders         []JobOrderSummaryV1  `json:"job_orders"`
	Assignments       []AssignmentV1       `json:"assignments"`
	Operations        []PlannedOperationV1 `json:"operations"`
	UnplacedOrders    []UnplacedOrderV1    `json:"unplaced_orders"`
	// ApprovedCycleTimes compares selected job orders with the cycle times approved during
	// onboarding (WIS-628). Advice only; planning uses the job orders' own values.
	ApprovedCycleTimes []ApprovedCycleTimeV1 `json:"approved_cycle_times"`
	Warnings           []string              `json:"warnings"`
}

func (s ShiftPlanSelectionV1) Validate() error {
	if len(s.JobOrderIDs) > maxPlannerOrders {
		return ErrPlannerLimit
	}
	if s.SchemaVersion != 1 || len(s.JobOrderIDs) == 0 || !validShift(s.LineID, s.StartsAt, s.EndsAt, s.Timezone) {
		return ErrInvalidPlanSelection
	}
	seen := map[string]bool{}
	for _, id := range s.JobOrderIDs {
		if !canonicalID(id) || seen[id] {
			return ErrInvalidPlanSelection
		}
		seen[id] = true
	}
	return nil
}

// Suggest is a snapshot computation. No receipts, audit mutations, draft plans or
// approvals are written. Authentication middleware may refresh the browser session.
// A database advisory lock bounds concurrent solves across API replicas per site.
func (s *ShiftPlanService) Suggest(ctx context.Context, a Actor, sc Scope, in ShiftPlanSelectionV1, solverPath string) (ShiftPlanSuggestionV1, error) {
	out := ShiftPlanSuggestionV1{SchemaVersion: 1, JobOrders: []JobOrderSummaryV1{}, Assignments: []AssignmentV1{}, Operations: []PlannedOperationV1{}, UnplacedOrders: []UnplacedOrderV1{}, ApprovedCycleTimes: []ApprovedCycleTimeV1{}, Warnings: []string{"ONE_JOB_PER_STATION", "RECORDED_APPROVED_BREAKS_ONLY"}}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return out, err
	}
	defer conn.Close()
	tx, _, _, err := s.beginOn(ctx, a, sc, configRead, conn)
	if err != nil {
		return out, err
	}
	var locked, acquisitionFailed bool
	// Explicit unlock on all exits, even cancellation. Discard a connection whose
	// unlock fails so a session lock can never leak back into the pool.
	defer func() {
		if tx != nil {
			_ = tx.Rollback()
		}
		if acquisitionFailed {
			// A cancelled acquisition may have reached the server without a reply.
			// Close that session instead of returning an uncertain lock to the pool.
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		} else if locked {
			clean, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			var released bool
			if e := conn.QueryRowContext(clean, "SELECT pg_advisory_unlock(hashtextextended($1,520))", sc.TenantID+"/"+sc.SiteID).Scan(&released); e != nil || !released {
				_ = conn.Raw(func(any) error { return driver.ErrBadConn })
			}
		}
	}()
	if err = in.Validate(); err != nil {
		return out, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1,520))`, sc.TenantID+"/"+sc.SiteID).Scan(&locked); err != nil {
		acquisitionFailed = true
		return out, err
	}
	if !locked {
		return out, ErrPlannerBusy
	}
	// Match include=current: read and digest-check just the head body, never an
	// unbounded revision history. Resolve every selected ID before returning data.
	for _, id := range in.JobOrderIDs {
		j, e := currentSuggestionOrder(ctx, tx, sc, id)
		if e != nil {
			return out, e
		}
		out.JobOrders = append(out.JobOrders, j)
	}
	profiles, err := suggestionProfiles(ctx, tx, sc, in.LineID)
	if err != nil {
		return out, err
	}
	if len(profiles) == 0 {
		return out, ErrPlannerData
	}
	snapshot, err := optionalSuggestionSnapshot(ctx, tx, sc)
	if err != nil {
		return out, err
	}
	out.ProcessSnapshotID = snapshot.SnapshotID
	if out.ProcessSnapshotID == "" {
		out.Warnings = append(out.Warnings, "KNOWLEDGE_CROSS_CHECK_UNAVAILABLE")
	}
	request := PlanningRequestV1{SchemaVersion: 1, LineID: in.LineID, StartsAt: in.StartsAt, EndsAt: in.EndsAt, Timezone: in.Timezone, Orders: []PlanningOrderV1{}, Workstations: []PlanningWorkstationV1{}, Downtimes: []PlanningDowntimeV1{}}
	for id := range profiles {
		request.Workstations = append(request.Workstations, PlanningWorkstationV1{ID: id, Capacity: 1})
	}
	sort.Slice(request.Workstations, func(i, j int) bool { return request.Workstations[i].ID < request.Workstations[j].ID })
	request.Downtimes, err = suggestionDowntimes(ctx, tx, sc, request)
	if err != nil {
		return out, err
	}
	// Plan in working time when every station shares the same breaks, so a job pauses
	// over a break instead of needing an unbroken window. Times map back at the end.
	clock, paused := sharedBreaks(request)
	if paused {
		// Format in the shift's zone so the offset matches the instant (DST nights).
		shiftEnd, _ := validTime(request.EndsAt)
		location, _ := time.LoadLocation(request.Timezone)
		request.EndsAt, request.Downtimes = clock.toWork(shiftEnd).In(location).Format(time.RFC3339), []PlanningDowntimeV1{}
	}
	// Whole-shift assignments permit one job per station. Stable dispatch order
	// chooses which job gets the slot; CP-SAT optimises timing of that feasible set.
	sort.Slice(out.JobOrders, func(i, j int) bool {
		x, y := out.JobOrders[i], out.JobOrders[j]
		px, py := x.CurrentOrder.Priority, y.CurrentOrder.Priority
		if px == 0 {
			px = 9
		}
		if py == 0 {
			py = 9
		}
		if px != py {
			return px < py
		}
		dx, dy := x.CurrentOrder.DueAt, y.CurrentOrder.DueAt
		if dx == "" {
			dx = "9999"
		}
		if dy == "" {
			dy = "9999"
		}
		if dx != dy {
			return dx < dy
		}
		return x.ID < y.ID
	})
	used := map[string]bool{}
	routes := map[string]suggestionRouting{}
	rejected := map[string]string{}
	start, _ := validTime(in.StartsAt)
	end, _ := validTime(in.EndsAt)
	for _, j := range out.JobOrders {
		o := j.CurrentOrder
		reject := func(code string) { rejected[j.ID] = code }
		route, reason, e := resolveSuggestionRouting(ctx, tx, sc, in.LineID, *o)
		if e != nil {
			return out, e
		}
		routes[j.ID] = route
		station := route.station
		if reason == "" {
			reason = suggestionRoute(snapshot, o.OperationID, station, start, end)
		}
		if reason != "" {
			reject(reason)
			continue
		}
		if _, ok := profiles[station]; !ok {
			var exists bool
			if e = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM supervisor.station_profile_heads WHERE tenant_id=$1 AND site_id=$2 AND workstation_id=$3)`, sc.TenantID, sc.SiteID, station).Scan(&exists); e != nil {
				return out, e
			}
			if !exists {
				return out, ErrPlannerData
			}
			reject("ROUTING_NOT_ON_LINE")
			continue
		}
		if used[station] {
			reject("STATION_ALREADY_ASSIGNED")
			continue
		}
		if o.TargetCycleTimeSeconds == "" {
			reject("CYCLE_TIME_MISSING")
			continue
		}
		approved, e := instructionApproved(ctx, tx, sc, orderInstruction(*o))
		if e != nil {
			return out, e
		}
		if !approved {
			reject("INSTRUCTION_UNAVAILABLE")
			continue
		}
		if profiles[station].Template != o.Template {
			reject("STATION_TEMPLATE_MISMATCH")
			continue
		}
		due := o.DueAt
		if d, ok := validTime(due); ok && paused {
			due = clock.toWork(d).Format(time.RFC3339Nano)
		}
		// An overdue order remains plannable; due dates are priorities, not deadlines.
		// Clamp after the working-time mapping: a due time inside a break at shift
		// start would otherwise map back onto the start and fail validation.
		if d, ok := validTime(due); ok && !d.After(start) {
			due = start.Add(time.Second).Format(time.RFC3339Nano)
		}
		po := PlanningOrderV1{JobOrderID: j.ID, JobOrderRevision: j.CurrentRevision, Priority: int32(o.Priority), DueAt: due, Operations: []PlanningOperationV1{{OperationID: o.OperationID, Sequence: 1, WorkstationID: station, Name: o.ItemCode, Quantity: o.TargetQuantity, SecondsPerUnit: o.TargetCycleTimeSeconds, DurationBasis: "job_order_revision"}}}
		single := request
		single.Orders = []PlanningOrderV1{po}
		if _, e = PlanGreedy(single); e != nil {
			if errors.Is(e, ErrNoFeasibleSchedule) {
				reject("SHIFT_CAPACITY_EXCEEDED")
			} else {
				reject("INVALID_DURATION")
			}
			continue
		}
		request.Orders = append(request.Orders, po)
		used[station] = true
		out.Assignments = append(out.Assignments, AssignmentV1{WorkstationID: station, JobOrderID: j.ID, JobOrderRevision: j.CurrentRevision, OrderID: o.OrderID, OperationID: o.OperationID, ItemCode: o.ItemCode, TargetQuantity: o.TargetQuantity, TargetCycleTimeSeconds: o.TargetCycleTimeSeconds, InstructionDocumentID: o.InstructionDocumentID, InstructionRevision: o.InstructionRevision, InstructionSHA256: o.InstructionSHA256, Template: o.Template, Fields: o.Fields})
	}
	edges, precedenceRejects, warnings := suggestionPrecedence(out.JobOrders, routes, snapshot, start, end)
	out.Warnings = append(out.Warnings, warnings...)
	cycleTimes, cycleWarnings := approvedCycleTimes(out.JobOrders, snapshot, start, end)
	out.ApprovedCycleTimes, out.Warnings = cycleTimes, append(out.Warnings, cycleWarnings...)
	for id, code := range precedenceRejects {
		if rejected[id] == "" {
			rejected[id] = code
		}
	}
	request, rejected = prepareSuggestionPrecedence(request, edges, rejected)
	kept := []AssignmentV1{}
	for _, x := range out.Assignments {
		if rejected[x.JobOrderID] == "" {
			kept = append(kept, x)
		}
	}
	out.Assignments = kept
	for _, j := range out.JobOrders {
		if code := rejected[j.ID]; code != "" {
			out.UnplacedOrders = append(out.UnplacedOrders, UnplacedOrderV1{j.ID, code})
		}
	}
	if len(request.Orders) == 0 {
		return out, ErrNoFeasibleSchedule
	}
	// The current job-order schema has exactly one operation, so the 50-order
	// limit also bounds operations below 200; retain the kernel's full validation.
	if err = request.Validate(); err != nil {
		return out, ErrInvalidPlanSelection
	}
	// Release the snapshot and its locks before any child process runs. Only the
	// dedicated session advisory lock connection remains, without an open tx.
	if err = s.finish(ctx, tx, a); err != nil {
		return out, err
	}
	plan, err := suggestWithFallback(ctx, request, solverPath)
	if err != nil {
		return out, err
	}
	if !suggestionPrecedenceValid(plan, edges, request) {
		plan, err = PlanGreedy(request)
		plan.Method = "greedy"
		if err != nil {
			return out, err
		}
		if !suggestionPrecedenceValid(plan, edges, request) {
			return out, ErrNoFeasibleSchedule
		}
	}
	out.Method, out.IsOptimized, out.SolverStatus, out.Operations = plan.Method, plan.IsOptimized, plan.SolverStatus, plan.Assignments
	if paused {
		location, _ := time.LoadLocation(in.Timezone)
		for i, op := range out.Operations {
			s, _ := validTime(op.StartsAt)
			e, _ := validTime(op.EndsAt)
			out.Operations[i].StartsAt = clock.toWall(s, true).In(location).Format(time.RFC3339Nano)
			out.Operations[i].EndsAt = clock.toWall(e, false).In(location).Format(time.RFC3339Nano)
		}
	}
	if out.Method == "greedy" {
		out.FallbackReason = "CP_SAT_UNAVAILABLE_OR_NO_VALID_SOLUTION"
	}
	// Recheck membership/session in a fresh snapshot after the bounded computation.
	tx, _, _, err = s.beginOn(ctx, a, sc, configRead, conn)
	if err != nil {
		return out, err
	}
	return out, s.finish(ctx, tx, a)
}

func suggestWithFallback(ctx context.Context, p PlanningRequestV1, path string) (PlanSuggestionV1, error) {
	plan, err := PlanCPsat(ctx, p, path)
	if err == nil {
		return plan, nil
	}
	if ctx.Err() != nil {
		return PlanSuggestionV1{}, ctx.Err()
	}
	switch {
	case errors.Is(err, ErrSolverUnavailable), errors.Is(err, ErrSolverUnknown), errors.Is(err, ErrNoFeasibleSchedule), errors.Is(err, ErrSolverOutput), errors.Is(err, ErrSolverTimePrecision):
		plan, err = PlanGreedy(p)
		plan.Method = "greedy"
		return plan, err
	default:
		return PlanSuggestionV1{}, err
	}
}

func currentSuggestionOrder(ctx context.Context, tx *sql.Tx, sc Scope, id string) (JobOrderSummaryV1, error) {
	j := JobOrderSummaryV1{ID: id}
	var payload []byte
	var digest string
	var version int
	err := tx.QueryRowContext(ctx, `SELECT h.current_revision,r.payload,r.canonical_payload_sha256,r.digest_schema_version FROM supervisor.job_order_heads h JOIN supervisor.job_order_revisions r ON (r.tenant_id,r.site_id,r.job_order_id,r.revision)=(h.tenant_id,h.site_id,h.id,h.current_revision) WHERE h.tenant_id=$1 AND h.site_id=$2 AND h.id=$3`, sc.TenantID, sc.SiteID, id).Scan(&j.CurrentRevision, &payload, &digest, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return j, ErrNotFound
	}
	if err != nil {
		return j, err
	}
	o, err := DecodeJobOrder(payload)
	if err != nil || version != jobOrderDigestVersion {
		return j, errIntegrity
	}
	if _, _, computed, e := normalizedOrder(sc.TenantID, sc.SiteID, o); e != nil || computed != digest {
		return j, errIntegrity
	}
	j.CurrentOrder = &o
	return j, nil
}

func suggestionProfiles(ctx context.Context, tx *sql.Tx, sc Scope, line string) (map[string]StationProfileBodyV1, error) {
	rows, err := tx.QueryContext(ctx, `SELECT r.payload,r.canonical_payload_sha256 FROM supervisor.station_profile_heads h JOIN supervisor.station_profile_revisions r ON (r.tenant_id,r.site_id,r.workstation_id,r.revision)=(h.tenant_id,h.site_id,h.workstation_id,h.current_revision) WHERE h.tenant_id=$1 AND h.site_id=$2 AND r.payload->>'line_id'=$3 ORDER BY h.workstation_id LIMIT 101`, sc.TenantID, sc.SiteID, line)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]StationProfileBodyV1{}
	for rows.Next() {
		var payload []byte
		var digest string
		if err = rows.Scan(&payload, &digest); err != nil {
			return nil, err
		}
		p, e := DecodeStationProfile(payload)
		if e != nil {
			return nil, errIntegrity
		}
		if _, d, e := profileEnvelope(sc, p); e != nil || d != digest {
			return nil, errIntegrity
		}
		out[p.WorkstationID] = p
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if len(out) > maxPlannerStations {
		return nil, ErrPlannerLimit
	}
	return out, nil
}

// Read approved recorded breaks for the line and its configured station IDs.
// Reuse intake's authoritative approval predicate (supersession/revocation included).
func suggestionDowntimes(ctx context.Context, tx *sql.Tx, sc Scope, p PlanningRequestV1) ([]PlanningDowntimeV1, error) {
	ids := []string{p.LineID}
	for _, s := range p.Workstations {
		ids = append(ids, s.ID)
	}
	encoded, _ := json.Marshal(ids)
	// Select the surviving revisions with intake's overlapping, non-rejected
	// supersession rule. Keep pending heads visible: they contest earlier breaks,
	// and must refuse the computation rather than becoming an empty break set.
	rows, err := tx.QueryContext(ctx, `SELECT r.payload,a.id FROM supervisor.plan_revisions r
LEFT JOIN supervisor.plan_review_heads h ON (h.tenant_id,h.site_id,h.plan_revision_id)=(r.tenant_id,r.site_id,r.id)
LEFT JOIN supervisor.plan_approvals a ON (a.tenant_id,a.site_id,a.plan_revision_id,a.review_revision)=(h.tenant_id,h.site_id,h.plan_revision_id,h.current_revision)
WHERE r.tenant_id=$1 AND r.site_id=$2 AND r.asset_id IN (SELECT value::uuid FROM jsonb_array_elements_text($3::jsonb))
AND r.window_start<$5::timestamptz AND r.window_end>$4::timestamptz AND a.decision IS DISTINCT FROM 'reject'
AND NOT EXISTS (SELECT 1 FROM supervisor.plan_revisions later
 LEFT JOIN supervisor.plan_review_heads lh ON (lh.tenant_id,lh.site_id,lh.plan_revision_id)=(later.tenant_id,later.site_id,later.id)
 LEFT JOIN supervisor.plan_approvals la ON (la.tenant_id,la.site_id,la.plan_revision_id,la.review_revision)=(lh.tenant_id,lh.site_id,lh.plan_revision_id,lh.current_revision)
 WHERE (later.tenant_id,later.site_id,later.order_id,later.asset_id)=(r.tenant_id,r.site_id,r.order_id,r.asset_id)
 AND later.plan_revision>r.plan_revision AND later.window_start<r.window_end AND later.window_end>r.window_start
 AND la.decision IS DISTINCT FROM 'reject')
ORDER BY r.id LIMIT 101`, sc.TenantID, sc.SiteID, string(encoded), p.StartsAt, p.EndsAt)
	if err != nil {
		return nil, err
	}
	plans := []production.Plan{}
	for rows.Next() {
		var b []byte
		var approval sql.NullString
		if err = rows.Scan(&b, &approval); err != nil {
			rows.Close()
			return nil, err
		}
		if !approval.Valid {
			rows.Close()
			return nil, ErrPlannerData
		}
		var body production.PlanBody
		if err = json.Unmarshal(b, &body); err != nil {
			rows.Close()
			return nil, errIntegrity
		}
		plans = append(plans, body.WithApproval(approval.String))
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if len(plans) > 100 {
		return nil, ErrPlannerLimit
	}
	intervals := map[string][]scheduledInterval{}
	start, _ := validTime(p.StartsAt)
	end, _ := validTime(p.EndsAt)
	for _, plan := range plans {
		reason, err := ingestion.PlanApprovalEligibility(ctx, tx, ingestion.Scope{TenantID: sc.TenantID, SiteID: sc.SiteID}, plan)
		if err != nil {
			return nil, err
		}
		if reason != "" {
			return nil, ErrPlannerData
		}
		for _, b := range plan.PlannedBreaks {
			lo, lok := validTime(b.Start)
			hi, hok := validTime(b.End)
			if !lok || !hok {
				return nil, errIntegrity
			}
			if lo.Before(start) {
				lo = start
			}
			if hi.After(end) {
				hi = end
			}
			if !lo.Before(hi) {
				continue
			}
			for _, station := range p.Workstations {
				if plan.AssetID == p.LineID || plan.AssetID == station.ID {
					intervals[station.ID] = append(intervals[station.ID], scheduledInterval{lo, hi, 1})
					if len(intervals[station.ID]) > maxPlannerDowntimes {
						return nil, ErrPlannerLimit
					}
				}
			}
		}
	}
	out := []PlanningDowntimeV1{}
	for _, station := range p.Workstations {
		values := intervals[station.ID]
		sort.Slice(values, func(i, j int) bool { return values[i].start.Before(values[j].start) })
		merged := []scheduledInterval{}
		for _, v := range values {
			if len(merged) > 0 && !v.start.After(merged[len(merged)-1].end) {
				if v.end.After(merged[len(merged)-1].end) {
					merged[len(merged)-1].end = v.end
				}
			} else {
				merged = append(merged, v)
			}
		}
		for _, v := range merged {
			out = append(out, PlanningDowntimeV1{station.ID, v.start.Format(time.RFC3339Nano), v.end.Format(time.RFC3339Nano), 1})
		}
	}
	if len(out) > maxPlannerDowntimes {
		return nil, ErrPlannerLimit
	}
	return out, nil
}

// workingClock removes breaks shared by every station from the timeline. Wall-clock
// breaks are sorted and disjoint; work pauses over them and resumes afterwards.
type workingClock struct{ breaks []scheduledInterval }

// sharedBreaks returns the breaks when every station has the same full downtime.
// Station-specific downtime keeps the unbroken-window model.
func sharedBreaks(p PlanningRequestV1) (workingClock, bool) {
	per := map[string][]PlanningDowntimeV1{}
	for _, d := range p.Downtimes {
		per[d.WorkstationID] = append(per[d.WorkstationID], d)
	}
	if len(p.Downtimes) == 0 || len(p.Workstations) == 0 {
		return workingClock{}, false
	}
	shared := per[p.Workstations[0].ID]
	for _, s := range p.Workstations {
		if len(per[s.ID]) != len(shared) || s.Capacity != 1 {
			return workingClock{}, false
		}
		for i, d := range per[s.ID] {
			if d.StartsAt != shared[i].StartsAt || d.EndsAt != shared[i].EndsAt || d.Demand < s.Capacity {
				return workingClock{}, false
			}
		}
	}
	c := workingClock{}
	for _, d := range shared {
		lo, _ := validTime(d.StartsAt)
		hi, _ := validTime(d.EndsAt)
		c.breaks = append(c.breaks, scheduledInterval{lo, hi, d.Demand})
	}
	sort.Slice(c.breaks, func(i, j int) bool { return c.breaks[i].start.Before(c.breaks[j].start) })
	return c, true
}

// toWork drops break time before t; a time inside a break maps to the break's start.
func (c workingClock) toWork(t time.Time) time.Time {
	removed := time.Duration(0)
	for _, b := range c.breaks {
		if !t.After(b.start) {
			break
		}
		if t.Before(b.end) {
			removed += t.Sub(b.start)
			break
		}
		removed += b.end.Sub(b.start)
	}
	return t.Add(-removed)
}

// toWall adds back the breaks before w. At a break boundary a start resumes after
// the break and an end stays before it.
func (c workingClock) toWall(w time.Time, start bool) time.Time {
	removed := time.Duration(0)
	for _, b := range c.breaks {
		at := b.start.Add(-removed)
		if !(w.After(at) || start && w.Equal(at)) {
			break
		}
		removed += b.end.Sub(b.start)
	}
	return w.Add(removed)
}
