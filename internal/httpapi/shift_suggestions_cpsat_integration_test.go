//go:build integration && cpsat

package httpapi

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/workstation"
	"github.com/google/uuid"
)

// Actual native binary, never a stub. Explicit opt-in is reported BLOCKED when absent.
func TestShiftSuggestionsCPsatHTTPPostgres(t *testing.T) {
	if os.Getenv("SUPERVISOR_TEST_CPSAT") == "" {
		t.Fatal("BLOCKED: actual CP-SAT HTTP subtest requires SUPERVISOR_TEST_CPSAT=1 and SUPERVISOR_WORKSTATION_CPSAT_PATH")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	line, _, job := suggestionFixture(t, f)
	body := map[string]any{"schema_version": 1, "line_id": line, "starts_at": "2026-10-20T08:00:00Z", "ends_at": "2026-10-20T16:00:00Z", "timezone": "UTC", "job_order_ids": []string{job.ID}}
	w := f.call(f.lead, "POST", "/v1/sites/"+f.site+"/shift-plans/suggestions", "", "", f.encode(body))
	expectSuggestion(t, w, 200, "")
	if !strings.Contains(w.Body.String(), `"method":"ortools_cp_sat"`) || !strings.Contains(w.Body.String(), `"is_optimized":true`) {
		t.Fatalf("actual solver not used: %s", w.Body.String())
	}
	t.Run("master routing step precedence", func(t *testing.T) {
		first := job.Revisions[1].Order
		station := uuid.NewString()
		masterCreateProfile(f, f.owner, f.site, uuid.NewString(), masterProfileBody(station, line, "Later step"))
		var route workstation.RoutingV1
		read := f.call(f.owner, "GET", "/v1/sites/"+f.site+"/routings/"+first.RoutingID, "", "", "")
		expectSuggestion(t, read, 200, "")
		if err := json.Unmarshal(read.Body.Bytes(), &route); err != nil {
			t.Fatal(err)
		}
		routing := route.Revisions[0].Body
		routing.Steps = append(routing.Steps, workstation.RoutingStepV1{StepNumber: 2, WorkstationID: station})
		expectSuggestion(t, f.call(f.owner, "POST", "/v1/sites/"+f.site+"/routings/"+route.ID+"/revisions", uuid.NewString(), `"1"`, f.encode(routing)), 201, "")
		step := 2
		later := first
		later.StepNumber, later.OperationID = &step, uuid.NewString()
		next := f.jobOrder(f.lead, later)
		body["job_order_ids"] = []string{next.ID, job.ID}
		w := f.call(f.lead, "POST", "/v1/sites/"+f.site+"/shift-plans/suggestions", "", "", f.encode(body))
		expectSuggestion(t, w, 200, "")
		var got workstation.ShiftPlanSuggestionV1
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.Method != "ortools_cp_sat" || !got.IsOptimized || len(got.Operations) != 2 {
			t.Fatalf("native precedence result: %s", w.Body.String())
		}
		var end, start time.Time
		for _, op := range got.Operations {
			if op.JobOrderID == job.ID {
				end, _ = time.Parse(time.RFC3339, op.EndsAt)
			}
			if op.JobOrderID == next.ID {
				start, _ = time.Parse(time.RFC3339, op.StartsAt)
			}
		}
		if start.IsZero() || end.IsZero() || start.Before(end) {
			t.Fatalf("native precedence ignored: %s", w.Body.String())
		}
	})
}
