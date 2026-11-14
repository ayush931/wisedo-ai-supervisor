package workstation

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
)

const (
	plannerLine   = "1c6b4260-a7fc-4fef-8e80-19b611f83f7b"
	plannerOrder  = "ed6ceeb6-23a2-41e9-8f25-d88aedf78b58"
	plannerOrder2 = "f1f8fd62-1001-4b9a-93ae-02c06d8d57df"
	plannerOp1    = "5d4a1a15-ae68-42a7-81e0-1c607169e369"
	plannerOp2    = "605ff7bd-07a9-4017-a8c5-bb01d918bb7f"
	plannerWS1    = "7ee1e85e-7683-46b2-815e-38ce4d4681d1"
	plannerWS2    = "e0ff2d73-ae44-4fe1-918f-efc2a419da92"
)

func planningRequest() PlanningRequestV1 {
	return PlanningRequestV1{
		SchemaVersion: 1, LineID: plannerLine, StartsAt: "2026-10-20T06:00:00-04:00", EndsAt: "2026-10-20T14:00:00-04:00", Timezone: "America/Detroit",
		Orders: []PlanningOrderV1{{
			JobOrderID: plannerOrder, JobOrderRevision: 1, Priority: 1,
			Operations: []PlanningOperationV1{
				{OperationID: plannerOp1, Sequence: 1, WorkstationID: plannerWS1, Name: "cut", Quantity: 3600, SecondsPerUnit: "1", DurationBasis: "standard_cycle"},
				{OperationID: plannerOp2, Sequence: 2, WorkstationID: plannerWS2, Name: "pack", Quantity: 1800, SecondsPerUnit: "1", DurationBasis: "standard_cycle"},
			},
		}},
		Workstations: []PlanningWorkstationV1{{ID: plannerWS1, Capacity: 1}, {ID: plannerWS2, Capacity: 1}},
	}
}

func TestPlanGreedyPreservesPrecedenceAndDowntime(t *testing.T) {
	request := planningRequest()
	request.Downtimes = []PlanningDowntimeV1{{WorkstationID: plannerWS1, StartsAt: "2026-10-20T06:00:00-04:00", EndsAt: "2026-10-20T07:00:00-04:00", Demand: 1}}
	plan, err := PlanGreedy(request)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Method != "deterministic_greedy" || plan.IsOptimized || len(plan.Assignments) != 2 {
		t.Fatalf("unexpected plan metadata or assignment count: %+v", plan)
	}
	if plan.Assignments[0].StartsAt != "2026-10-20T07:00:00-04:00" || plan.Assignments[1].StartsAt != "2026-10-20T08:00:00-04:00" {
		t.Fatalf("downtime or order precedence was ignored: %+v", plan.Assignments)
	}
}

func TestPlanGreedyUsesParallelStationCapacity(t *testing.T) {
	request := planningRequest()
	request.Workstations = []PlanningWorkstationV1{{ID: plannerWS1, Capacity: 2}}
	request.Orders = []PlanningOrderV1{
		{JobOrderID: plannerOrder, JobOrderRevision: 1, Operations: []PlanningOperationV1{{OperationID: plannerOp1, Sequence: 1, WorkstationID: plannerWS1, Name: "cut", Quantity: 3600, SecondsPerUnit: "1", DurationBasis: "standard_cycle"}}},
		{JobOrderID: plannerOrder2, JobOrderRevision: 1, Operations: []PlanningOperationV1{{OperationID: plannerOp2, Sequence: 1, WorkstationID: plannerWS1, Name: "cut", Quantity: 3600, SecondsPerUnit: "1", DurationBasis: "standard_cycle"}}},
	}
	plan, err := PlanGreedy(request)
	if err != nil {
		t.Fatal(err)
	}
	for _, assignment := range plan.Assignments {
		if assignment.StartsAt != "2026-10-20T06:00:00-04:00" {
			t.Fatalf("capacity 2 should run two orders concurrently: %+v", plan.Assignments)
		}
	}
}

func TestPlanGreedyTreatsDowntimeDemandAsUnavailableCapacity(t *testing.T) {
	request := planningRequest()
	request.Workstations = []PlanningWorkstationV1{{ID: plannerWS1, Capacity: 2}}
	request.Orders = []PlanningOrderV1{
		{JobOrderID: plannerOrder, JobOrderRevision: 1, Operations: []PlanningOperationV1{{OperationID: plannerOp1, Sequence: 1, WorkstationID: plannerWS1, Name: "cut", Quantity: 3600, SecondsPerUnit: "1", DurationBasis: "standard_cycle"}}},
		{JobOrderID: plannerOrder2, JobOrderRevision: 1, Operations: []PlanningOperationV1{{OperationID: plannerOp2, Sequence: 1, WorkstationID: plannerWS1, Name: "cut", Quantity: 3600, SecondsPerUnit: "1", DurationBasis: "standard_cycle"}}},
	}
	request.Downtimes = []PlanningDowntimeV1{{WorkstationID: plannerWS1, StartsAt: "2026-10-20T06:00:00-04:00", EndsAt: "2026-10-20T07:00:00-04:00", Demand: 1}}
	plan, err := PlanGreedy(request)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Assignments[0].StartsAt != "2026-10-20T06:00:00-04:00" || plan.Assignments[1].StartsAt != "2026-10-20T07:00:00-04:00" {
		t.Fatalf("partial downtime demand was ignored: %+v", plan.Assignments)
	}
}

func TestPlanGreedyRejectsWorkThatDoesNotFitShift(t *testing.T) {
	request := planningRequest()
	request.Orders[0].Operations[0].Quantity = 28_800
	if _, err := PlanGreedy(request); !errors.Is(err, ErrNoFeasibleSchedule) {
		t.Fatalf("PlanGreedy error = %v, want %v", err, ErrNoFeasibleSchedule)
	}
}

func TestPlannerCeilsExactDecimalDurationWithoutFloatRounding(t *testing.T) {
	request := planningRequest()
	request.Orders[0].Operations = request.Orders[0].Operations[:1]
	request.Orders[0].Operations[0].Quantity = 1_000_000
	request.Orders[0].Operations[0].SecondsPerUnit = "0.000001"
	plan, err := PlanGreedy(request)
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.Assignments[0].DurationSecs; got != 1 {
		t.Fatalf("duration = %d seconds, want exact one-second ceiling", got)
	}
}

func TestPlanningRequestValidationRejectsUnknownStation(t *testing.T) {
	request := planningRequest()
	request.Orders[0].Operations[0].WorkstationID = plannerOp2
	if err := request.Validate(); !errors.Is(err, ErrInvalidShiftPlan) {
		t.Fatalf("Validate error = %v, want %v", err, ErrInvalidShiftPlan)
	}
}

// Rule 04: only an IANA zone projects local time. time.LoadLocation maps "" to UTC and
// "Local" to the host zone, which the shift plan contract (ianaZone) already refuses; the
// planner request accepted them (review finding).
func TestPlanningRequestValidationRejectsNonIANAZones(t *testing.T) {
	for _, zone := range []string{"", "Local"} {
		request := planningRequest()
		request.Timezone = zone
		request.StartsAt, request.EndsAt = "2026-10-20T10:00:00Z", "2026-10-20T18:00:00Z"
		for i := range request.Orders {
			request.Orders[i].ReadyAt, request.Orders[i].DueAt = "", ""
		}
		request.Downtimes = nil
		if err := request.Validate(); !errors.Is(err, ErrInvalidShiftPlan) {
			t.Fatalf("timezone %q: Validate error = %v, want %v", zone, err, ErrInvalidShiftPlan)
		}
		request.Timezone = "UTC"
		if err := request.Validate(); err != nil {
			t.Fatalf("UTC control case rejected: %v", err)
		}
	}
}

func TestDecodePlanningRequestRejectsUnknownAndDuplicateFields(t *testing.T) {
	data, err := json.Marshal(planningRequest())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodePlanningRequest(data); err != nil {
		t.Fatalf("DecodePlanningRequest rejected valid input: %v", err)
	}
	duplicate := bytes.Replace(data, []byte(`"schema_version":1`), []byte(`"schema_version":1,"schema_version":1`), 1)
	unknown := append(append([]byte(nil), data[:len(data)-1]...), []byte(`,"site_id":"28c5589a-90a0-4c6c-8708-8363ae4ca4f6"}`)...)
	for _, input := range [][]byte{duplicate, unknown, append(append([]byte(nil), data...), bytes.Repeat([]byte(" "), (1<<20)+1-len(data))...)} {
		if _, err := DecodePlanningRequest(input); !errors.Is(err, ErrInvalidShiftPlan) {
			t.Errorf("DecodePlanningRequest accepted invalid input: %v", err)
		}
	}
}
