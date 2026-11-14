//go:build integration

package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/evidence"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/knowledge"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/workstation"
	"github.com/google/uuid"
)

// These HTTP tests deliberately use wire values so they also compile on main.
func TestShiftSuggestionsHTTPPostgres(t *testing.T) {
	t.Setenv("SUPERVISOR_WORKSTATION_CPSAT_PATH", "/missing/suggestions-solver")
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	line, station, job := suggestionFixture(t, f)
	base := "/v1/sites/" + f.site + "/shift-plans/suggestions"
	selection := map[string]any{"schema_version": 1, "line_id": line, "starts_at": "2026-10-20T08:00:00Z", "ends_at": "2026-10-20T16:00:00Z", "timezone": "UTC", "job_order_ids": []string{job.ID}}
	t.Run("server data and no persistence", func(t *testing.T) {
		w := f.call(f.lead, "POST", base, "", "", f.encode(selection))
		expectSuggestion(t, w, 200, "")
		conformsToPublished(t, httptest.NewRequest("POST", base, strings.NewReader(f.encode(selection))), w)
		var got struct {
			Method      string                           `json:"method"`
			Optimized   bool                             `json:"is_optimized"`
			Assignments []workstation.AssignmentV1       `json:"assignments"`
			Operations  []workstation.PlannedOperationV1 `json:"operations"`
			Orders      []workstation.JobOrderSummaryV1  `json:"job_orders"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.Method != "greedy" || got.Optimized || len(got.Assignments) != 1 || got.Assignments[0].WorkstationID != station || got.Assignments[0].JobOrderRevision != 2 || got.Assignments[0].TargetCycleTimeSeconds != "3" || got.Assignments[0].TargetQuantity != 120 || len(got.Operations) != 1 || got.Operations[0].DurationSecs != 360 || len(got.Orders) != 1 || got.Orders[0].CurrentRevision != 2 {
			t.Fatalf("suggestion: %s", w.Body.String())
		}
		var effects int
		if err := f.control.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM supervisor.shift_plan_heads)+(SELECT count(*) FROM supervisor.shift_plan_audit)+(SELECT count(*) FROM supervisor.shift_plan_receipts)`).Scan(&effects); err != nil || effects != 0 {
			t.Fatalf("persisted effects=%d: %v", effects, err)
		}
	})
	for _, tc := range []struct {
		name   string
		actor  stationSession
		status int
		code   string
	}{
		{"owner", f.owner, 200, ""}, {"viewer", f.viewer, 403, "PERMISSION_DENIED"}, {"operator", f.operator, 403, "PERMISSION_DENIED"}, {"other tenant", f.outsider, 404, "NOT_FOUND"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			expectSuggestion(t, f.call(f.lead, "POST", base, "", "", f.encode(selection)), 200, "")
			expectSuggestion(t, f.call(tc.actor, "POST", base, "", "", f.encode(selection)), tc.status, tc.code)
		})
	}
	for _, field := range []string{"seconds_per_unit", "capacity", "tenant_id", "actor"} {
		t.Run("reject "+field, func(t *testing.T) {
			body := strings.Replace(f.encode(selection), "{", `{"`+field+`":"untrusted",`, 1)
			expectSuggestion(t, f.call(f.lead, "POST", base, "", "", body), 422, "INVALID_PLAN_SELECTION")
		})
	}
	t.Run("infeasible", func(t *testing.T) {
		b := cloneSelection(selection)
		b["ends_at"] = "2026-10-20T08:00:01Z"
		expectSuggestion(t, f.call(f.lead, "POST", base, "", "", f.encode(b)), 422, "NO_FEASIBLE_SCHEDULE")
	})
	t.Run("over limit", func(t *testing.T) {
		b := cloneSelection(selection)
		ids := make([]string, 51)
		for i := range ids {
			ids[i] = uuid.NewString()
		}
		b["job_order_ids"] = ids
		expectSuggestion(t, f.call(f.lead, "POST", base, "", "", f.encode(b)), 422, "PLANNER_LIMIT_EXCEEDED")
	})
	t.Run("unknown order is nondisclosing", func(t *testing.T) {
		expectSuggestion(t, f.call(f.lead, "POST", base, "", "", f.encode(selection)), 200, "")
		b := cloneSelection(selection)
		b["job_order_ids"] = []string{uuid.NewString()}
		expectSuggestion(t, f.call(f.lead, "POST", base, "", "", f.encode(b)), 404, "NOT_FOUND")
	})
	t.Run("one concurrent solve per site", func(t *testing.T) {
		expectSuggestion(t, f.call(f.lead, "POST", base, "", "", f.encode(selection)), 200, "")
		tx, err := f.control.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,520))`, f.tenant+"/"+f.site); err != nil {
			t.Fatal(err)
		}
		expectSuggestion(t, f.call(f.lead, "POST", base, "", "", f.encode(selection)), 409, "PLANNER_BUSY")
	})
	t.Run("unplaced order", func(t *testing.T) {
		body := job.Revisions[1].Order
		body.OperationID = uuid.NewString()
		body.ItemID, body.RoutingID, body.StepNumber = "", "", nil
		body.ItemCode = "unlinked-part"
		other := f.jobOrder(f.lead, body)
		b := cloneSelection(selection)
		b["job_order_ids"] = []string{job.ID, other.ID}
		w := f.call(f.lead, "POST", base, "", "", f.encode(b))
		expectSuggestion(t, w, 200, "")
		if !strings.Contains(w.Body.String(), `"reason_code":"ROUTING_UNLINKED"`) || !strings.Contains(w.Body.String(), other.ID) {
			t.Fatalf("missing unplaced reason: %s", w.Body.String())
		}
	})
	t.Run("approved planned downtime", func(t *testing.T) {
		var span string
		if err := f.control.QueryRowContext(ctx, `SELECT id FROM supervisor.evidence_spans WHERE source_id=$1 LIMIT 1`, job.Revisions[1].Order.InstructionDocumentID).Scan(&span); err != nil {
			t.Fatal(err)
		}
		plan := map[string]any{"order_id": job.Revisions[1].Order.OrderID, "asset_id": station, "plan_revision": 1, "window_start": selection["starts_at"], "window_end": selection["ends_at"], "unit": "count", "rate_segments": []any{map[string]any{"start": selection["starts_at"], "end": selection["ends_at"], "target_good_units_per_second": "1"}}, "planned_breaks": []any{map[string]any{"start": selection["starts_at"], "end": "2026-10-20T09:00:00Z"}}}
		w := f.call(f.owner, "POST", "/v1/sites/"+f.site+"/plan-revisions", uuid.NewString(), "", f.encode(map[string]any{"plan": plan, "source_span_ids": []string{span}}))
		expectSuggestion(t, w, 201, "")
		var captured struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &captured); err != nil {
			t.Fatal(err)
		}
		expectSuggestion(t, f.call(f.approver, "POST", "/v1/sites/"+f.site+"/plan-revisions/"+captured.ID+"/reviews", uuid.NewString(), `"0"`, `{"decision":"approve","reason":"Reviewed synthetic break"}`), 200, "")
		w = f.call(f.lead, "POST", base, "", "", f.encode(selection))
		expectSuggestion(t, w, 200, "")
		var got struct {
			Operations []workstation.PlannedOperationV1 `json:"operations"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if len(got.Operations) != 1 || got.Operations[0].StartsAt != "2026-10-20T09:00:00Z" {
			t.Fatalf("planned break ignored: %s", w.Body.String())
		}
		// An overdue order with a break at shift start stays plannable: its clamped due
		// time must not map back onto the shift start in working time (review finding).
		overdue := job.Revisions[1].Order
		overdue.OperationID, overdue.DueAt = uuid.NewString(), "2026-10-19T08:00:00Z"
		late := cloneSelection(selection)
		late["job_order_ids"] = []string{f.jobOrder(f.lead, overdue).ID}
		w = f.call(f.lead, "POST", base, "", "", f.encode(late))
		expectSuggestion(t, w, 200, "")
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if len(got.Operations) != 1 || got.Operations[0].StartsAt != "2026-10-20T09:00:00Z" {
			t.Fatalf("overdue order after a shift-start break: %s", w.Body.String())
		}
		// Superseded approved revisions must not block the current approved breaks.
		plan["plan_revision"] = 2
		plan["planned_breaks"] = []any{map[string]any{"start": selection["starts_at"], "end": "2026-10-20T10:00:00Z"}}
		w = f.call(f.owner, "POST", "/v1/sites/"+f.site+"/plan-revisions", uuid.NewString(), "", f.encode(map[string]any{"plan": plan, "source_span_ids": []string{span}}))
		expectSuggestion(t, w, 201, "")
		if err := json.Unmarshal(w.Body.Bytes(), &captured); err != nil {
			t.Fatal(err)
		}
		// A pending replacement contests the old approval, so cannot be ignored.
		expectSuggestion(t, f.call(f.lead, "POST", base, "", "", f.encode(selection)), 422, "PLANNER_DATA_UNAVAILABLE")
		expectSuggestion(t, f.call(f.approver, "POST", "/v1/sites/"+f.site+"/plan-revisions/"+captured.ID+"/reviews", uuid.NewString(), `"0"`, `{"decision":"approve","reason":"Reviewed replacement break"}`), 200, "")
		w = f.call(f.lead, "POST", base, "", "", f.encode(selection))
		expectSuggestion(t, w, 200, "")
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if len(got.Operations) != 1 || got.Operations[0].StartsAt != "2026-10-20T10:00:00Z" {
			t.Fatalf("replacement break ignored: %s", w.Body.String())
		}
	})
	t.Run("suggested draft requires distinct approval", func(t *testing.T) {
		w := f.call(f.lead, "POST", base, "", "", f.encode(selection))
		expectSuggestion(t, w, 200, "")
		var suggestion struct {
			Assignments []workstation.AssignmentV1 `json:"assignments"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &suggestion); err != nil {
			t.Fatal(err)
		}
		body := workstation.ShiftPlanBodyV1{SchemaVersion: 1, LineID: line, StartsAt: selection["starts_at"].(string), EndsAt: selection["ends_at"].(string), Timezone: "UTC", Assignments: suggestion.Assignments}
		w = f.call(f.lead, "POST", "/v1/sites/"+f.site+"/shift-plans", uuid.NewString(), "", f.encode(body))
		expectSuggestion(t, w, 201, "")
		var plan workstation.ShiftPlanV1
		if err := json.Unmarshal(w.Body.Bytes(), &plan); err != nil {
			t.Fatal(err)
		}
		if plan.ApprovedRevision != nil || plan.Revisions[0].State != workstation.StateDraft {
			t.Fatal("suggestion created an approved plan")
		}
		approval := f.encode(workstation.ShiftPlanApproveInputV1{CanonicalPayloadSHA256: plan.Revisions[0].CanonicalPayloadSHA256})
		url := "/v1/sites/" + f.site + "/shift-plans/" + plan.ID + "/approvals"
		expectSuggestion(t, f.call(f.lead, "POST", url, uuid.NewString(), `"1"`, approval), 403, "SELF_REVIEW_FORBIDDEN")
		expectSuggestion(t, f.call(f.approver, "POST", url, uuid.NewString(), `"1"`, approval), 200, "")
	})
	t.Run("stale optional snapshot does not block", func(t *testing.T) {
		expectSuggestion(t, f.call(f.lead, "POST", base, "", "", f.encode(selection)), 200, "")
		f.instruction("New source changes the current knowledge cut", true)
		expectSuggestion(t, f.call(f.lead, "POST", base, "", "", f.encode(selection)), 200, "")
	})
}

func cloneSelection(v map[string]any) map[string]any {
	b := map[string]any{}
	for k, x := range v {
		b[k] = x
	}
	return b
}

func suggestionFixture(t *testing.T, f *stationFixture) (string, string, workstation.JobOrderV1) {
	t.Helper()
	source, sha, _ := f.instruction("Synthetic approved routing for station and operation", true)
	sum := sha256.Sum256([]byte(f.owner.token))
	hash := hex.EncodeToString(sum[:])
	ea := evidence.Actor{Issuer: f.issuer, Subject: f.owner.subject, SessionHash: hash}
	es := evidence.Scope{TenantID: f.tenant, SiteID: f.site}
	span, err := evidence.New(f.runtime).CreateSpan(f.ctx, ea, es, evidence.SpanInput{SourceID: source, Revision: 1, Start: 0, End: 20}, uuid.NewString(), 1)
	if err != nil {
		t.Fatal(err)
	}
	kn := knowledge.New(f.runtime)
	ka := knowledge.Actor{Issuer: f.issuer, Subject: f.owner.subject, SessionHash: hash}
	ks := knowledge.Scope{TenantID: f.tenant, SiteID: f.site}
	entity := func(kind, asset string) string {
		id := uuid.NewString()
		_, err := kn.CreateEntity(f.ctx, ka, ks, knowledge.EntityInput{EntityID: id, Type: kind, AssetKind: asset, Name: "Synthetic " + kind, SourceID: source, SourceRevision: 1}, uuid.NewString(), 0)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	station, operation := entity("asset", "machine"), entity("operation", "")
	claim := knowledge.ClaimInput{ClaimID: uuid.NewString(), SubjectID: station, SubjectRevision: 1, Predicate: "performs", ObjectEntityID: operation, ObjectRevision: 1, EffectiveFrom: "2026-01-01T00:00:00Z", EvidenceClass: "report", SourceSpanIDs: []string{span.ID}}
	if _, err = kn.CreateClaim(f.ctx, ka, ks, claim, uuid.NewString(), 0); err != nil {
		t.Fatal(err)
	}
	if _, err = kn.DecideReview(f.ctx, ka, ks, claim.ClaimID, knowledge.ReviewInput{ClaimRevision: 1, Verdict: "approve", Reason: "Verified synthetic routing"}, uuid.NewString(), 1); err != nil {
		t.Fatal(err)
	}
	line := masterCreateLine(f, f.owner, f.site, uuid.NewString(), masterLineBody("Suggestions line", true)).ID
	profile := workstation.StationProfileBodyV1{SchemaVersion: 1, WorkstationID: station, LineID: line, DisplayName: "Synthetic station", Template: "production", Fields: []workstation.ProfileFieldV1{{Name: "good_units", Label: "Good units", Kind: "decimal", Required: true, Unit: "count"}}}
	expectSuggestion(t, f.call(f.owner, "POST", "/v1/sites/"+f.site+"/workstations/"+station+"/profile-revisions", uuid.NewString(), `"0"`, f.encode(profile)), 201, "")
	item := masterCreateItem(f, f.owner, f.site, uuid.NewString(), masterItemBody("Suggestions item", true))
	route := masterCreateRouting(f, f.owner, f.site, uuid.NewString(), workstation.RoutingBodyV1{SchemaVersion: 1, DisplayName: "Suggestions route", Active: true, LineID: line, ItemID: item.ID, Steps: []workstation.RoutingStepV1{{StepNumber: 1, WorkstationID: station}}})
	body := orderBody(source, sha, 100)
	step := 1
	body.RoutingID, body.StepNumber, body.ItemID = route.ID, &step, item.ID
	body.OrderID = entity("order", "")
	body.OperationID = operation
	body.DueAt = ""
	job := f.jobOrder(f.lead, body)
	body.TargetQuantity = 120
	body.TargetCycleTimeSeconds = "3"
	job = f.decodeOrder(f.call(f.lead, "POST", "/v1/sites/"+f.site+"/job-orders/"+job.ID+"/revisions", uuid.NewString(), `"1"`, f.encode(body)))
	inventory, err := kn.ReadInventory(f.ctx, ka, ks)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = kn.CreateSnapshot(f.ctx, ka, ks, knowledge.SnapshotInput{SnapshotID: uuid.NewString(), Reason: "Reviewed synthetic routing"}, uuid.NewString(), inventory.Revision); err != nil {
		t.Fatal(err)
	}
	return line, station, job
}

func expectSuggestion(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if w.Code != status || code != "" && !strings.Contains(w.Body.String(), `"code":"`+code+`"`) {
		t.Fatalf("HTTP %d %s, want %d %s", w.Code, w.Body.String(), status, code)
	}
}
