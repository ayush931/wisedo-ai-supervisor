package workstation

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/knowledge"
)

func TestSuggestionFallbackNeverReturnsInvalidSolverOutput(t *testing.T) {
	for _, tc := range []struct{ name, output string }{
		{"infeasible", `{"schema_version":1,"method":"ortools_cp_sat","status":"INFEASIBLE","is_optimized":false,"assignments":[]}`},
		{"unknown", `{"schema_version":1,"method":"ortools_cp_sat","status":"UNKNOWN","is_optimized":false,"assignments":[]}`},
		{"invalid", `{"schema_version":1,"method":"ortools_cp_sat","status":"OPTIMAL","is_optimized":true,"assignments":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := suggestWithFallback(context.Background(), planningRequest(), solverFixture(t, tc.output))
			if err != nil || p.Method != "greedy" || p.IsOptimized || len(p.Assignments) != 2 || p.Assignments[0].StartsAt != "2026-10-20T06:00:00-04:00" {
				t.Fatalf("fallback: %+v %v", p, err)
			}
		})
	}
	t.Run("timeout", func(t *testing.T) {
		before := cpsatLimit
		cpsatLimit = 20 * time.Millisecond
		t.Cleanup(func() { cpsatLimit = before })
		p, err := suggestWithFallback(context.Background(), planningRequest(), scriptFixture(t, "exec /bin/sleep 2"))
		if err != nil || p.Method != "greedy" || len(p.Assignments) != 2 {
			t.Fatalf("timeout fallback: %+v %v", p, err)
		}
	})
	t.Run("cancelled does not solve again", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		p, err := suggestWithFallback(ctx, planningRequest(), "/missing/solver")
		if !errors.Is(err, context.Canceled) || len(p.Assignments) != 0 {
			t.Fatalf("cancelled: %+v %v", p, err)
		}
	})
}

func TestSuggestionRoutingCrossChecksCurrentKnowledge(t *testing.T) {
	start := time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC)
	end := start.Add(8 * time.Hour)
	base := knowledge.SnapshotClaim{ClaimInput: knowledge.ClaimInput{SubjectID: plannerWS1, Predicate: "performs", ObjectEntityID: "operation", EffectiveFrom: "2026-01-01T00:00:00Z"}}
	other := base
	other.SubjectID = plannerWS2
	partial := other
	partial.EffectiveFrom = "2026-10-20T12:00:00Z"
	precedence := base
	precedence.Predicate = "precedes"
	precedence.SubjectID = "operation"
	precedence.ObjectEntityID = "later-operation"
	precedence.EffectiveFrom = "2026-10-20T12:00:00Z"
	foreign := base
	foreign.SubjectID = "unconfigured-station"
	// The master-data route is authoritative even without a snapshot.
	for _, tc := range []struct {
		name   string
		claims []knowledge.SnapshotClaim
		reason string
	}{
		{"explicit route", []knowledge.SnapshotClaim{base}, ""},
		{"missing", nil, ""},
		{"other line", []knowledge.SnapshotClaim{foreign}, "ROUTING_AMBIGUOUS"},
		{"ambiguous", []knowledge.SnapshotClaim{base, other}, "ROUTING_AMBIGUOUS"},
		{"changes during shift", []knowledge.SnapshotClaim{base, partial}, "ROUTING_AMBIGUOUS"},
		{"precedence during shift", []knowledge.SnapshotClaim{base, precedence}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reason := suggestionRoute(knowledge.Snapshot{Contents: knowledge.SnapshotContents{Claims: tc.claims}}, "operation", plannerWS1, start, end)
			if reason != tc.reason {
				t.Fatalf("reason=%q, want %q", reason, tc.reason)
			}
		})
	}
}

func TestSuggestionDependencyRefusals(t *testing.T) {
	base := planningRequest()
	first := base.Orders[0]
	first.Operations = first.Operations[:1]
	second := first
	second.JobOrderID = plannerOrder2
	second.Operations = []PlanningOperationV1{planningRequest().Orders[0].Operations[1]}
	base.Orders = []PlanningOrderV1{second, first}
	for _, tc := range []struct {
		name     string
		edges    map[string][]precedenceEdge
		rejected map[string]string
		end      string
		want     string
	}{
		{"cycle", map[string][]precedenceEdge{plannerOrder: {{before: plannerOrder2}}, plannerOrder2: {{before: plannerOrder}}}, map[string]string{}, "", "ROUTING_PRECEDENCE_UNSUPPORTED"},
		{"selected predecessor unplaced", map[string][]precedenceEdge{plannerOrder2: {{before: plannerOrder}}}, map[string]string{plannerOrder: "INSTRUCTION_UNAVAILABLE"}, "", "ROUTING_PREDECESSOR_UNPLACED"},
		{"chain exceeds window", map[string][]precedenceEdge{plannerOrder2: {{before: plannerOrder}}}, map[string]string{}, "2026-10-20T07:00:00-04:00", "SHIFT_CAPACITY_EXCEEDED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := base
			if tc.end != "" {
				p.EndsAt = tc.end
			}
			got, rejected := prepareSuggestionPrecedence(p, tc.edges, tc.rejected)
			if rejected[plannerOrder2] != tc.want {
				t.Fatalf("refusal=%v want %s", rejected, tc.want)
			}
			for _, o := range got.Orders {
				if o.JobOrderID == plannerOrder2 {
					t.Fatal("refused dependent still planned")
				}
			}
		})
	}
}

// WIS-520 flow lines: consecutive routing steps of one order overlap with one-piece
// transfer. A faster successor is held back so it cannot outrun the last part; a slower
// one starts after the first part. Claim edges stay finish-to-start.
func TestSuggestionFlowPrecedence(t *testing.T) {
	base := planningRequest()
	first := base.Orders[0]
	first.Operations = []PlanningOperationV1{first.Operations[0]}
	first.Operations[0].Quantity, first.Operations[0].SecondsPerUnit = 600, "6" // 06:00-07:00
	for _, tc := range []struct {
		name, cycle, start, end string
		flow                    bool
	}{
		{"faster successor waits for the last part", "3", "2026-10-20T06:30:03-04:00", "2026-10-20T07:00:03-04:00", true},
		{"slower successor starts after the first part", "9", "2026-10-20T06:00:06-04:00", "2026-10-20T07:30:06-04:00", true},
		{"finish-to-start claim", "3", "2026-10-20T07:00:00-04:00", "2026-10-20T07:30:00-04:00", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			second := first
			second.JobOrderID = plannerOrder2
			second.Operations = []PlanningOperationV1{base.Orders[0].Operations[1]}
			second.Operations[0].Quantity, second.Operations[0].SecondsPerUnit = 600, tc.cycle
			p := base
			p.Orders = []PlanningOrderV1{first, second}
			edges := map[string][]precedenceEdge{plannerOrder2: {{before: plannerOrder, flow: tc.flow}}}
			got, rejected := prepareSuggestionPrecedence(p, edges, map[string]string{})
			if len(rejected) != 0 || len(got.Orders) != 2 {
				t.Fatalf("rejected=%v orders=%d", rejected, len(got.Orders))
			}
			plan, err := PlanGreedy(got)
			if err != nil {
				t.Fatal(err)
			}
			op := plan.Assignments[1]
			s, _ := validTime(op.StartsAt)
			e, _ := validTime(op.EndsAt)
			ws, _ := validTime(tc.start)
			we, _ := validTime(tc.end)
			if op.JobOrderID != plannerOrder2 || !s.Equal(ws) || !e.Equal(we) {
				t.Fatalf("successor %s-%s, want %s-%s", op.StartsAt, op.EndsAt, tc.start, tc.end)
			}
			if !suggestionPrecedenceValid(plan, edges, got) {
				t.Fatal("own schedule rejected")
			}
			// Negative control: both runs starting together breaks the flow rule.
			plan.Assignments[1].StartsAt, plan.Assignments[1].EndsAt = plan.Assignments[0].StartsAt, plan.Assignments[0].EndsAt
			if suggestionPrecedenceValid(plan, edges, got) {
				t.Fatal("simultaneous start accepted")
			}
		})
	}
}

// WIS-520: a job longer than the time before a shared break pauses over it instead of
// being refused for want of an unbroken window. Station-specific breaks keep the old model.
func TestSuggestionWorkPausesOverSharedBreaks(t *testing.T) {
	p := planningRequest() // 06:00-14:00 -04:00, two stations
	p.Orders[0].Operations = p.Orders[0].Operations[:1]
	p.Orders[0].Operations[0].Quantity = 5 * 3600 // five hours at 1s
	brk := func(ws string) PlanningDowntimeV1 {
		return PlanningDowntimeV1{WorkstationID: ws, StartsAt: "2026-10-20T09:30:00-04:00", EndsAt: "2026-10-20T10:00:00-04:00", Demand: 1}
	}
	if _, err := PlanGreedy(func() PlanningRequestV1 {
		q := p
		q.Downtimes = []PlanningDowntimeV1{brk(plannerWS1), brk(plannerWS2)}
		return q
	}()); err == nil {
		t.Fatal("precondition: an unbroken five-hour window should not exist after the break")
	}
	for _, tc := range []struct {
		name      string
		downtimes []PlanningDowntimeV1
		paused    bool
	}{
		{"shared line break", []PlanningDowntimeV1{brk(plannerWS1), brk(plannerWS2)}, true},
		{"station-specific break", []PlanningDowntimeV1{brk(plannerWS1)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := p
			q.Downtimes = tc.downtimes
			clock, paused := sharedBreaks(q)
			if paused != tc.paused {
				t.Fatalf("paused=%v", paused)
			}
			if !paused {
				return
			}
			end, _ := validTime(q.EndsAt)
			q.EndsAt, q.Downtimes = clock.toWork(end).Format(time.RFC3339), nil
			plan, err := PlanGreedy(q)
			if err != nil {
				t.Fatal(err)
			}
			s, _ := validTime(plan.Assignments[0].StartsAt)
			e, _ := validTime(plan.Assignments[0].EndsAt)
			ws, _ := validTime("2026-10-20T06:00:00-04:00")
			we, _ := validTime("2026-10-20T11:30:00-04:00") // five hours of work plus the break
			if !clock.toWall(s, true).Equal(ws) || !clock.toWall(e, false).Equal(we) {
				t.Fatalf("wall times %s-%s", clock.toWall(s, true), clock.toWall(e, false))
			}
		})
	}
	// Boundaries: a start at the break resumes after it; an end at the break stays before it.
	clock, _ := sharedBreaks(func() PlanningRequestV1 {
		q := p
		q.Downtimes = []PlanningDowntimeV1{brk(plannerWS1), brk(plannerWS2)}
		return q
	}())
	at, _ := validTime("2026-10-20T09:30:00-04:00")
	inside, _ := validTime("2026-10-20T09:45:00-04:00")
	if !clock.toWork(inside).Equal(at) || !clock.toWall(at, true).Equal(at.Add(30*time.Minute)) || !clock.toWall(at, false).Equal(at) {
		t.Fatal("break boundary mapping")
	}
}
