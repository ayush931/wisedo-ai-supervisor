//go:build integration

package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/evidence"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/knowledge"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/workstation"
	"github.com/google/uuid"
)

// Wire bodies keep these regressions runnable on the pre-fix commit 7d9fac3.
func TestShiftSuggestionsMasterRoutingPostgres(t *testing.T) {
	t.Setenv("SUPERVISOR_WORKSTATION_CPSAT_PATH", "/missing/routing-solver")
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	line := masterCreateLine(f, f.owner, f.site, "routing-line", masterLineBody("Routing line", true))
	item := masterCreateItem(f, f.owner, f.site, "routing-item", masterItemBody("Routing part", true))
	station := uuid.NewString()
	masterCreateProfile(f, f.owner, f.site, "routing-profile", masterProfileBody(station, line.ID, "Routing station"))
	routeBody := workstation.RoutingBodyV1{SchemaVersion: 1, DisplayName: "Single step", Active: true, LineID: line.ID, ItemID: item.ID, Steps: []workstation.RoutingStepV1{{StepNumber: 1, WorkstationID: station}}}
	route := masterCreateRouting(f, f.owner, f.site, "routing-route", routeBody)
	source, sha, _ := f.instruction("Instruction only, without a process snapshot", true)
	body := orderBody(source, sha, 100)
	body.ItemID, body.DueAt, body.RoutingID = item.ID, "", route.ID
	step := 1
	body.StepNumber = &step
	linkedJob := f.jobOrder(f.lead, body)
	url := "/v1/sites/" + f.site + "/shift-plans/suggestions"
	selection := map[string]any{"schema_version": 1, "line_id": line.ID, "starts_at": "2026-10-20T08:00:00Z", "ends_at": "2026-10-20T16:00:00Z", "timezone": "UTC", "job_order_ids": []string{linkedJob.ID}}
	t.Run("no snapshot linked single routing", func(t *testing.T) {
		w := f.call(f.lead, "POST", url, "", "", f.encode(selection))
		expectSuggestion(t, w, 200, "")
		var got workstation.ShiftPlanSuggestionV1
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if len(got.Assignments) != 1 || got.Assignments[0].WorkstationID != station || got.ProcessSnapshotID != "" {
			t.Fatalf("master-only result: %s", w.Body.String())
		}
	})
	t.Run("explicit link create revise and validate", func(t *testing.T) {
		linked := wireOrder(f, body)
		linked["routing_id"], linked["step_number"] = route.ID, 1
		path := "/v1/sites/" + f.site + "/job-orders"
		w := f.call(f.lead, "POST", path, uuid.NewString(), "", f.encode(linked))
		expectSuggestion(t, w, 201, "")
		var job workstation.JobOrderV1
		if err := json.Unmarshal(w.Body.Bytes(), &job); err != nil {
			t.Fatal(err)
		}
		chosen := cloneSelection(selection)
		chosen["job_order_ids"] = []string{job.ID}
		expectSuggestion(t, f.call(f.lead, "POST", url, "", "", f.encode(chosen)), 200, "")
		for _, tc := range []struct {
			name   string
			change func(map[string]any)
		}{
			{"routing only", func(b map[string]any) { delete(b, "step_number") }},
			{"step only", func(b map[string]any) { delete(b, "routing_id") }},
			{"unknown routing", func(b map[string]any) { b["routing_id"] = uuid.NewString() }},
			{"unknown step", func(b map[string]any) { b["step_number"] = 2 }},
			{"zero step", func(b map[string]any) { b["step_number"] = 0 }},
			{"wrong item", func(b map[string]any) { b["item_id"] = uuid.NewString() }},
		} {
			t.Run(tc.name, func(t *testing.T) {
				invalid := cloneSelection(linked)
				tc.change(invalid)
				expectSuggestion(t, f.call(f.lead, "POST", path, uuid.NewString(), "", f.encode(invalid)), 422, "INVALID_JOB_ORDER")
				expectSuggestion(t, f.call(f.lead, "POST", path+"/"+job.ID+"/revisions", uuid.NewString(), `"1"`, f.encode(invalid)), 422, "INVALID_JOB_ORDER")
			})
		}
		linked["target_quantity"] = 90
		expectSuggestion(t, f.call(f.lead, "POST", path+"/"+job.ID+"/revisions", uuid.NewString(), `"1"`, f.encode(linked)), 201, "")
	})
	t.Run("lock released after unknown and infeasible requests", func(t *testing.T) {
		for _, tc := range []struct {
			change func(map[string]any)
			status int
			code   string
		}{
			{func(b map[string]any) { b["job_order_ids"] = []string{uuid.NewString()} }, 404, "NOT_FOUND"},
			{func(b map[string]any) { b["ends_at"] = "2026-10-20T08:00:01Z" }, 422, "NO_FEASIBLE_SCHEDULE"},
		} {
			bad := cloneSelection(selection)
			tc.change(bad)
			expectSuggestion(t, f.call(f.lead, "POST", url, "", "", f.encode(bad)), tc.status, tc.code)
			expectSuggestion(t, f.call(f.lead, "POST", url, "", "", f.encode(selection)), 200, "")
		}
	})
}

func wireOrder(f *stationFixture, b workstation.JobOrderBodyV1) map[string]any {
	f.t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(f.encode(b)), &out); err != nil {
		f.t.Fatal(err)
	}
	return out
}

func TestShiftSuggestionsRoutingDecisionsPostgres(t *testing.T) {
	t.Setenv("SUPERVISOR_WORKSTATION_CPSAT_PATH", "/missing/routing-solver")
	for _, name := range []string{"ambiguous fallback", "step on other line", "station moved off line", "current routing revision", "same order precedence", "multi step unmapped", "multi step mapped", "inactive routing"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()
			f := newStationFixture(t, ctx)
			line := masterCreateLine(f, f.owner, f.site, "line", masterLineBody("Line", true)).ID
			station, route, job := routingTestJob(t, f, line)
			anchorStation, _, anchor := routingTestJob(t, f, line)
			body := job.Revisions[0].Order
			chosen := []string{anchor.ID, job.ID}
			reason := ""
			expectedStation := station
			switch name {
			case "ambiguous fallback":
				routeBody := route.Revisions[0].Body
				routeBody.DisplayName = "Other active route"
				masterCreateRouting(f, f.owner, f.site, uuid.NewString(), routeBody)
				body.ItemID, body.RoutingID, body.StepNumber = "", "", nil
				job = f.jobOrder(f.lead, body)
				chosen = []string{anchor.ID, job.ID}
				reason = "ROUTING_UNLINKED"
			case "step on other line":
				otherLine := masterCreateLine(f, f.owner, f.site, "other-line", masterLineBody("Other line", true)).ID
				routeBody := route.Revisions[0].Body
				routeBody.LineID = otherLine
				expectSuggestion(t, f.call(f.owner, "POST", "/v1/sites/"+f.site+"/routings/"+route.ID+"/revisions", uuid.NewString(), `"1"`, f.encode(routeBody)), 201, "")
				reason = "ROUTING_NOT_ON_LINE"
			case "station moved off line":
				expectSuggestion(t, f.call(f.owner, "POST", "/v1/sites/"+f.site+"/workstations/"+station+"/profile-revisions", uuid.NewString(), `"1"`, f.encode(masterProfileBody(station, uuid.NewString(), "Moved"))), 201, "")
				reason = "ROUTING_NOT_ON_LINE"
			case "current routing revision":
				expectedStation = uuid.NewString()
				masterCreateProfile(f, f.owner, f.site, uuid.NewString(), masterProfileBody(expectedStation, line, "Replacement"))
				routeBody := route.Revisions[0].Body
				routeBody.Steps[0].WorkstationID = expectedStation
				expectSuggestion(t, f.call(f.owner, "POST", "/v1/sites/"+f.site+"/routings/"+route.ID+"/revisions", uuid.NewString(), `"1"`, f.encode(routeBody)), 201, "")
			case "same order precedence", "multi step unmapped", "multi step mapped":
				routeBody := route.Revisions[0].Body
				routeBody.Steps = append(routeBody.Steps, workstation.RoutingStepV1{StepNumber: 2, WorkstationID: anchorStation})
				expectSuggestion(t, f.call(f.owner, "POST", "/v1/sites/"+f.site+"/routings/"+route.ID+"/revisions", uuid.NewString(), `"1"`, f.encode(routeBody)), 201, "")
				later := body
				later.OperationID = uuid.NewString()
				step := 2
				later.StepNumber = &step
				second := f.jobOrder(f.lead, later)
				if name == "same order precedence" {
					chosen = []string{second.ID, job.ID}
				} else {
					unlinked := later
					unlinked.ItemID, unlinked.RoutingID, unlinked.StepNumber = "", "", nil
					if name == "multi step unmapped" {
						unlinked.OperationID = uuid.NewString()
					}
					// A mapped operation alone cannot identify a catalog item from display text.
					reason = "ROUTING_UNLINKED"
					job = f.jobOrder(f.lead, unlinked)
					chosen = []string{job.ID}
					// Keep a valid unrelated assignment so an unplaced reason is observable.
					if reason != "" {
						chosen = append(chosen, anchor.ID)
					}
				}
			case "inactive routing":
				routeBody := route.Revisions[0].Body
				routeBody.Active = false
				expectSuggestion(t, f.call(f.owner, "POST", "/v1/sites/"+f.site+"/routings/"+route.ID+"/revisions", uuid.NewString(), `"1"`, f.encode(routeBody)), 201, "")
				reason = "ROUTING_UNLINKED"
			}
			selection := routingSelection(line, chosen)
			w := f.call(f.lead, "POST", "/v1/sites/"+f.site+"/shift-plans/suggestions", "", "", f.encode(selection))
			expectSuggestion(t, w, 200, "")
			var got workstation.ShiftPlanSuggestionV1
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if reason != "" {
				if len(got.UnplacedOrders) != 1 || got.UnplacedOrders[0].JobOrderID != job.ID || got.UnplacedOrders[0].ReasonCode != reason || len(got.Assignments) != 1 || got.Assignments[0].JobOrderID != anchor.ID {
					t.Fatalf("unplaced: %s", w.Body.String())
				}
				return
			}
			if len(got.UnplacedOrders) != 0 {
				t.Fatalf("unexpected refusals: %s", w.Body.String())
			}
			found := false
			for _, a := range got.Assignments {
				if a.JobOrderID == job.ID && a.WorkstationID == expectedStation {
					found = true
				}
			}
			if !found {
				t.Fatalf("current route not used: %s", w.Body.String())
			}
			if name == "same order precedence" {
				if len(got.Operations) != 2 || got.Operations[0].JobOrderID != job.ID {
					t.Fatalf("step sequence: %s", w.Body.String())
				}
				// Flow line (100 parts at 2.5s, 3s unit cycle): step 2 starts after step 1's
				// first part and ends one cycle after its last part, overlapping step 1.
				times := []time.Time{}
				for _, v := range []string{got.Operations[0].StartsAt, got.Operations[0].EndsAt, got.Operations[1].StartsAt, got.Operations[1].EndsAt} {
					at, _ := time.Parse(time.RFC3339, v)
					times = append(times, at)
				}
				if times[2].Sub(times[0]) != 3*time.Second || times[3].Sub(times[1]) != 3*time.Second || !times[2].Before(times[1]) || !slices.Contains(got.Warnings, "ROUTING_FLOW_OVERLAP_ASSUMED") {
					t.Fatalf("flow precedence: %s", w.Body.String())
				}
			}
		})
	}
}

func routingSelection(line string, ids []string) map[string]any {
	return map[string]any{"schema_version": 1, "line_id": line, "starts_at": "2026-10-20T08:00:00Z", "ends_at": "2026-10-20T16:00:00Z", "timezone": "UTC", "job_order_ids": ids}
}
func routingTestJob(t *testing.T, f *stationFixture, line string) (string, workstation.RoutingV1, workstation.JobOrderV1) {
	t.Helper()
	item := masterCreateItem(f, f.owner, f.site, uuid.NewString(), masterItemBody("Part", true)).ID
	station := uuid.NewString()
	masterCreateProfile(f, f.owner, f.site, uuid.NewString(), masterProfileBody(station, line, "Station"))
	route := masterCreateRouting(f, f.owner, f.site, uuid.NewString(), workstation.RoutingBodyV1{SchemaVersion: 1, DisplayName: "Route", Active: true, LineID: line, ItemID: item, Steps: []workstation.RoutingStepV1{{StepNumber: 1, WorkstationID: station}}})
	source, sha, _ := f.instruction("Routing instruction", true)
	b := orderBody(source, sha, 100)
	b.ItemID, b.DueAt = item, ""
	wire := wireOrder(f, b)
	wire["routing_id"], wire["step_number"] = route.ID, 1
	w := f.call(f.lead, "POST", "/v1/sites/"+f.site+"/job-orders", uuid.NewString(), "", f.encode(wire))
	expectSuggestion(t, w, 201, "")
	var job workstation.JobOrderV1
	if err := json.Unmarshal(w.Body.Bytes(), &job); err != nil {
		t.Fatal(err)
	}
	return station, route, job
}

func TestShiftSuggestionsKnowledgeCrossCheckPostgres(t *testing.T) {
	t.Setenv("SUPERVISOR_WORKSTATION_CPSAT_PATH", "/missing/routing-solver")
	for _, name := range []string{"conflicting performs", "stale conflicting snapshot", "selected precedence", "external precedence"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()
			f := newStationFixture(t, ctx)
			line, _, job := suggestionFixture(t, f)
			otherStation, _, other := routingTestJob(t, f, line)
			source, _, _ := f.instruction("Reviewed cross-check claim", true)
			sum := sha256.Sum256([]byte(f.owner.token))
			hash := hex.EncodeToString(sum[:])
			ea := evidence.Actor{Issuer: f.issuer, Subject: f.owner.subject, SessionHash: hash}
			es := evidence.Scope{TenantID: f.tenant, SiteID: f.site}
			span, err := evidence.New(f.runtime).CreateSpan(ctx, ea, es, evidence.SpanInput{SourceID: source, Revision: 1, Start: 0, End: 20}, uuid.NewString(), 1)
			if err != nil {
				t.Fatal(err)
			}
			kn := knowledge.New(f.runtime)
			ka := knowledge.Actor{Issuer: f.issuer, Subject: f.owner.subject, SessionHash: hash}
			ks := knowledge.Scope{TenantID: f.tenant, SiteID: f.site}
			entity := knowledge.EntityInput{EntityID: otherStation, Type: "asset", AssetKind: "machine", Name: "Other station", SourceID: source, SourceRevision: 1}
			predicate, subject, object := "performs", otherStation, job.Revisions[1].Order.OperationID
			if name == "selected precedence" || name == "external precedence" {
				entity.EntityID = other.Revisions[0].Order.OperationID
				entity.Type, entity.AssetKind, entity.Name = "operation", "", "Next operation"
				predicate, subject, object = "precedes", job.Revisions[1].Order.OperationID, entity.EntityID
				if name == "external precedence" {
					entity.EntityID = uuid.NewString()
					object = entity.EntityID
				}
			}
			if _, err = kn.CreateEntity(ctx, ka, ks, entity, uuid.NewString(), 0); err != nil {
				t.Fatal(err)
			}
			claim := knowledge.ClaimInput{ClaimID: uuid.NewString(), SubjectID: subject, SubjectRevision: 1, Predicate: predicate, ObjectEntityID: object, ObjectRevision: 1, EffectiveFrom: "2026-01-01T00:00:00Z", EvidenceClass: "report", SourceSpanIDs: []string{span.ID}}
			if _, err = kn.CreateClaim(ctx, ka, ks, claim, uuid.NewString(), 0); err != nil {
				t.Fatal(err)
			}
			if _, err = kn.DecideReview(ctx, ka, ks, claim.ClaimID, knowledge.ReviewInput{ClaimRevision: 1, Verdict: "approve", Reason: "Reviewed cross-check"}, uuid.NewString(), 1); err != nil {
				t.Fatal(err)
			}
			inv, err := kn.ReadInventory(ctx, ka, ks)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = kn.CreateSnapshot(ctx, ka, ks, knowledge.SnapshotInput{SnapshotID: uuid.NewString(), Reason: "Cross-check snapshot"}, uuid.NewString(), inv.Revision); err != nil {
				t.Fatal(err)
			}
			if name == "stale conflicting snapshot" {
				f.instruction("Changes the inventory after snapshot", true)
			}
			w := f.call(f.lead, "POST", "/v1/sites/"+f.site+"/shift-plans/suggestions", "", "", f.encode(routingSelection(line, []string{other.ID, job.ID})))
			expectSuggestion(t, w, 200, "")
			var got workstation.ShiftPlanSuggestionV1
			if err = json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if name == "conflicting performs" {
				if len(got.Assignments) != 1 || got.Assignments[0].JobOrderID != other.ID || len(got.UnplacedOrders) != 1 || got.UnplacedOrders[0].JobOrderID != job.ID || got.UnplacedOrders[0].ReasonCode != "ROUTING_AMBIGUOUS" {
					t.Fatalf("conflict not refused: %s", w.Body.String())
				}
				return
			}
			if len(got.Assignments) != 2 || len(got.UnplacedOrders) != 0 {
				t.Fatalf("optional knowledge blocked: %s", w.Body.String())
			}
			if name == "stale conflicting snapshot" && got.ProcessSnapshotID != "" {
				t.Fatalf("stale snapshot used: %s", w.Body.String())
			}
			if name == "external precedence" && !slices.Contains(got.Warnings, "EXTERNAL_PRECEDENCE_NOT_SCHEDULED") {
				t.Fatalf("missing warning: %s", w.Body.String())
			}
			if name == "selected precedence" {
				var before, after time.Time
				for _, op := range got.Operations {
					if op.JobOrderID == job.ID {
						before, _ = time.Parse(time.RFC3339, op.EndsAt)
					} else {
						after, _ = time.Parse(time.RFC3339, op.StartsAt)
					}
				}
				if before.IsZero() || after.IsZero() || after.Before(before) {
					t.Fatalf("precedence dropped: %s", w.Body.String())
				}
			}
		})
	}
}

func TestShiftSuggestionsReleaseTransactionDuringSolvePostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	line := masterCreateLine(f, f.owner, f.site, "line", masterLineBody("Line", true)).ID
	_, _, job := routingTestJob(t, f, line)
	sum := sha256.Sum256([]byte(f.lead.token))
	actor := workstation.Actor{Issuer: f.issuer, Subject: f.lead.subject, SessionHash: hex.EncodeToString(sum[:])}
	scope := workstation.Scope{TenantID: f.tenant, SiteID: f.site}
	var selection workstation.ShiftPlanSelectionV1
	if err := json.Unmarshal([]byte(f.encode(routingSelection(line, []string{job.ID}))), &selection); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "started")
	script := filepath.Join(dir, "solver")
	// Synthetic slow child tests cancellation/connection lifetime, not solver quality.
	if err := os.WriteFile(script, []byte("#!/bin/sh\n/usr/bin/touch '"+marker+"'\nexec /bin/sleep 30\n"), 0700); err != nil {
		t.Fatal(err)
	}
	service := workstation.NewShiftPlans(f.runtime)
	callCtx, stop := context.WithCancel(ctx)
	defer stop()
	finished := make(chan error, 1)
	go func() { _, err := service.Suggest(callCtx, actor, scope, selection, script); finished <- err }()
	defer func() { stop() }()
	for deadline := time.Now().Add(5 * time.Second); ; {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		select {
		case err := <-finished:
			t.Fatalf("solver did not start: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("solver did not start within bound")
		}
		time.Sleep(10 * time.Millisecond)
	}
	var transactions int
	// Identify the runtime user from its own connection, not a fixture-specific role name.
	var runtimeUser string
	if err := f.runtime.QueryRowContext(ctx, `SELECT current_user`).Scan(&runtimeUser); err != nil {
		t.Fatal(err)
	}
	if err := f.control.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND usename=$1 AND xact_start IS NOT NULL`, runtimeUser).Scan(&transactions); err != nil || transactions != 0 {
		t.Fatalf("open transactions during solve=%d: %v", transactions, err)
	}
	if _, err := service.Suggest(ctx, actor, scope, selection, "/missing/solver"); !errors.Is(err, workstation.ErrPlannerBusy) {
		t.Fatalf("solve lock not held: %v", err)
	}
	stop()
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled solver: %v", err)
	}
	if got, err := service.Suggest(ctx, actor, scope, selection, "/missing/solver"); err != nil || len(got.Assignments) != 1 {
		t.Fatalf("lock leaked after cancellation: %+v %v", got, err)
	}
}

// A valid native makespan result can delay an earlier job past its successor's
// ready-time lower bound. The service must validate the actual cross-job edges.
func TestShiftSuggestionsRejectDelayedPredecessorPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	line := masterCreateLine(f, f.owner, f.site, "line", masterLineBody("Line", true)).ID
	station, route, first := routingTestJob(t, f, line)
	nextStation := uuid.NewString()
	masterCreateProfile(f, f.owner, f.site, "later", masterProfileBody(nextStation, line, "Later"))
	b := route.Revisions[0].Body
	b.Steps = append(b.Steps, workstation.RoutingStepV1{StepNumber: 2, WorkstationID: nextStation})
	expectSuggestion(t, f.call(f.owner, "POST", "/v1/sites/"+f.site+"/routings/"+route.ID+"/revisions", uuid.NewString(), `"1"`, f.encode(b)), 201, "")
	next := first.Revisions[0].Order
	step := 2
	next.StepNumber, next.OperationID = &step, uuid.NewString()
	second := f.jobOrder(f.lead, next)
	operations := []workstation.PlannedOperationV1{}
	for i, j := range []workstation.JobOrderV1{first, second} {
		ws := station
		if i == 1 {
			ws = nextStation
		}
		operations = append(operations, workstation.PlannedOperationV1{JobOrderID: j.ID, OperationID: j.Revisions[0].Order.OperationID, Sequence: 1, WorkstationID: ws, StartsAt: "2026-10-20T08:04:10Z", EndsAt: "2026-10-20T08:08:20Z", DurationSecs: 250, DurationBasis: "job_order_revision"})
	}
	output := f.encode(map[string]any{"schema_version": 1, "method": "ortools_cp_sat", "status": "OPTIMAL", "is_optimized": true, "makespan_seconds": 500, "assignments": operations})
	script := filepath.Join(t.TempDir(), "solver")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ncat <<'RESPONSE'\n"+output+"\nRESPONSE\n"), 0700); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(f.lead.token))
	actor := workstation.Actor{Issuer: f.issuer, Subject: f.lead.subject, SessionHash: hex.EncodeToString(sum[:])}
	var selection workstation.ShiftPlanSelectionV1
	if err := json.Unmarshal([]byte(f.encode(routingSelection(line, []string{first.ID, second.ID}))), &selection); err != nil {
		t.Fatal(err)
	}
	got, err := workstation.NewShiftPlans(f.runtime).Suggest(ctx, actor, workstation.Scope{TenantID: f.tenant, SiteID: f.site}, selection, script)
	if err != nil || got.Method != "greedy" || got.IsOptimized || len(got.Operations) != 2 {
		t.Fatalf("unsafe solver output returned: %+v %v", got, err)
	}
	if got.Operations[0].JobOrderID != first.ID || got.Operations[0].StartsAt != "2026-10-20T08:00:00Z" || got.Operations[1].JobOrderID != second.ID || got.Operations[1].StartsAt != "2026-10-20T08:00:03Z" {
		t.Fatalf("precedence fallback: %+v", got.Operations)
	}
}

// WIS-628: cycle times approved during onboarding are compared with the selected job orders and
// cited, but planning still uses each job order's own value; nothing is copied into a job order.
func TestShiftSuggestionsApprovedCycleTimePostgres(t *testing.T) {
	t.Setenv("SUPERVISOR_WORKSTATION_CPSAT_PATH", "/missing/routing-solver")
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	line, _, job := suggestionFixture(t, f)
	_, _, other := routingTestJob(t, f, line)
	jobOrder, otherOrder := job.Revisions[1].Order, other.Revisions[0].Order
	if jobOrder.TargetCycleTimeSeconds == "" || otherOrder.TargetCycleTimeSeconds == "" {
		t.Fatalf("fixture job orders need cycle times: %q %q", jobOrder.TargetCycleTimeSeconds, otherOrder.TargetCycleTimeSeconds)
	}
	source, _, _ := f.instruction("Onboarding routing sheet", true)
	sum := sha256.Sum256([]byte(f.owner.token))
	hash := hex.EncodeToString(sum[:])
	span, err := evidence.New(f.runtime).CreateSpan(ctx, evidence.Actor{Issuer: f.issuer, Subject: f.owner.subject, SessionHash: hash}, evidence.Scope{TenantID: f.tenant, SiteID: f.site},
		evidence.SpanInput{SourceID: source, Revision: 1, Start: 0, End: 20}, uuid.NewString(), 1)
	if err != nil {
		t.Fatal(err)
	}
	kn := knowledge.New(f.runtime)
	ka := knowledge.Actor{Issuer: f.issuer, Subject: f.owner.subject, SessionHash: hash}
	ks := knowledge.Scope{TenantID: f.tenant, SiteID: f.site}
	// suggestionFixture already holds the job's operation entity; the other job's is new.
	approve := func(operation, seconds string, create bool) string {
		t.Helper()
		if create {
			if _, e := kn.CreateEntity(ctx, ka, ks, knowledge.EntityInput{EntityID: operation, Type: "operation", Name: "Onboarded operation", SourceID: source, SourceRevision: 1}, uuid.NewString(), 0); e != nil {
				t.Fatal(e)
			}
		}
		value, _ := json.Marshal(seconds)
		claim := knowledge.ClaimInput{ClaimID: uuid.NewString(), SubjectID: operation, SubjectRevision: 1, Predicate: "cycle_time_seconds", Literal: &knowledge.Literal{Type: "decimal", Value: value, Unit: "s"},
			EffectiveFrom: "2026-01-01T00:00:00Z", EvidenceClass: "report", SourceSpanIDs: []string{span.ID}}
		if _, e := kn.CreateClaim(ctx, ka, ks, claim, uuid.NewString(), 0); e != nil {
			t.Fatal(e)
		}
		if _, e := kn.DecideReview(ctx, ka, ks, claim.ClaimID, knowledge.ReviewInput{ClaimRevision: 1, Verdict: "approve", Reason: "Reviewed onboarding cycle time"}, uuid.NewString(), 1); e != nil {
			t.Fatal(e)
		}
		return claim.ClaimID
	}
	// The job's approved value differs from its job order; the other job's matches exactly.
	differing := approve(jobOrder.OperationID, "987.5", false)
	matching := approve(otherOrder.OperationID, otherOrder.TargetCycleTimeSeconds, true)
	inv, err := kn.ReadInventory(ctx, ka, ks)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = kn.CreateSnapshot(ctx, ka, ks, knowledge.SnapshotInput{SnapshotID: uuid.NewString(), Reason: "Onboarded cycle times"}, uuid.NewString(), inv.Revision); err != nil {
		t.Fatal(err)
	}
	w := f.call(f.lead, "POST", "/v1/sites/"+f.site+"/shift-plans/suggestions", "", "", f.encode(routingSelection(line, []string{other.ID, job.ID})))
	expectSuggestion(t, w, 200, "")
	var got workstation.ShiftPlanSuggestionV1
	if err = json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	byJob := map[string]workstation.ApprovedCycleTimeV1{}
	for _, row := range got.ApprovedCycleTimes {
		byJob[row.JobOrderID] = row
	}
	d, m := byJob[job.ID], byJob[other.ID]
	if len(got.ApprovedCycleTimes) != 2 ||
		d.Status != workstation.CycleTimeDiffers || d.ApprovedSeconds != "987.5" || d.JobOrderSeconds != jobOrder.TargetCycleTimeSeconds || !slices.Equal(d.ClaimIDs, []string{differing}) || !slices.Equal(d.SourceSpanIDs, []string{span.ID}) ||
		m.Status != workstation.CycleTimeMatches || !slices.Equal(m.ClaimIDs, []string{matching}) || !slices.Contains(got.Warnings, "APPROVED_CYCLE_TIME_DIFFERS") {
		t.Fatalf("approved cycle times: %s", w.Body.String())
	}
	// Advice only: the suggestion still plans from the job order's own cycle time.
	for _, a := range got.Assignments {
		if a.JobOrderID == job.ID && a.TargetCycleTimeSeconds != jobOrder.TargetCycleTimeSeconds {
			t.Fatalf("approved value replaced the job order's: %s", w.Body.String())
		}
	}
	if len(got.Assignments) != 2 {
		t.Fatalf("comparison changed placement: %s", w.Body.String())
	}
}
