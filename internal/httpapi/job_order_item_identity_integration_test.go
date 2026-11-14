//go:build integration

package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/workstation"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
)

// Wire item_id fields keep the regressions executable on e66c6d6.
func itemIdentityFixture(f *stationFixture) (line, item, route, station string, body map[string]any) {
	f.t.Helper()
	line = masterCreateLine(f, f.owner, f.site, "identity-line", masterLineBody("Identity line", true)).ID
	item = masterCreateItem(f, f.owner, f.site, "identity-item", masterItemBody("Press part", true)).ID
	station = uuid.NewString()
	masterCreateProfile(f, f.owner, f.site, "identity-profile", masterProfileBody(station, line, "Press"))
	route = masterCreateRouting(f, f.owner, f.site, "identity-route", workstation.RoutingBodyV1{SchemaVersion: 1, DisplayName: "Press route", Active: true, LineID: line, ItemID: item, Steps: []workstation.RoutingStepV1{{StepNumber: 1, WorkstationID: station}}}).ID
	source, sha, _ := f.instruction("Press part instruction", true)
	body = wireOrder(f, orderBody(source, sha, 100))
	body["item_code"], body["item_id"], body["routing_id"], body["step_number"] = "SKU-PRESS-42", item, route, 1
	return
}

func identityActor(f *stationFixture) workstation.Actor {
	sum := sha256.Sum256([]byte(f.lead.token))
	return workstation.Actor{Issuer: f.issuer, Subject: f.lead.subject, SessionHash: hex.EncodeToString(sum[:])}
}

func TestJobOrderItemIdentityPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	_, item, _, _, linked := itemIdentityFixture(f)
	path := "/v1/sites/" + f.site + "/job-orders"
	job := identityOrder(t, f, f.call(f.lead, "POST", path, "identity-create", "", f.encode(linked)))
	if got := wireOrder(f, job.Revisions[0].Order); got["item_id"] != item || got["item_code"] != "SKU-PRESS-42" {
		t.Fatalf("catalog identity replaced the code: %s", f.encode(got))
	}
	other := masterCreateItem(f, f.owner, f.site, "other-item", masterItemBody("Other part", true)).ID
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"item only", func(b map[string]any) { delete(b, "routing_id"); delete(b, "step_number") }},
		{"missing item", func(b map[string]any) { delete(b, "item_id") }},
		{"missing route", func(b map[string]any) { delete(b, "routing_id") }},
		{"missing step", func(b map[string]any) { delete(b, "step_number") }},
		{"wrong catalog item", func(b map[string]any) { b["item_id"] = other }},
		{"null item", func(b map[string]any) { b["item_id"] = nil }},
		{"empty item", func(b map[string]any) { b["item_id"] = "" }},
		{"invalid item", func(b map[string]any) { b["item_id"] = "not-a-uuid" }},
	} {
		invalid := cloneSelection(linked)
		tc.change(invalid)
		for _, target := range []struct{ path, revision string }{{path, ""}, {path + "/" + job.ID + "/revisions", `"1"`}} {
			w := f.call(f.lead, "POST", target.path, uuid.NewString(), target.revision, f.encode(invalid))
			if w.Code != 422 {
				t.Fatalf("%s: expected 422, got %d: %s", tc.name, w.Code, w.Body.String())
			}
			expectSuggestion(t, w, 422, "INVALID_JOB_ORDER")
		}
	}
	linked["item_code"] = "SKU-PRESS-43"
	revised := identityOrder(t, f, f.call(f.lead, "POST", path+"/"+job.ID+"/revisions", "identity-revise", `"1"`, f.encode(linked)))
	if revised.CurrentRevision != 2 || revised.Revisions[1].Order.ItemCode != "SKU-PRESS-43" || revised.Revisions[1].CanonicalPayloadSHA256 == job.Revisions[0].CanonicalPayloadSHA256 {
		t.Fatalf("code revision was not preserved: %s", f.encode(revised))
	}
}

func TestLinkedJobOrderItemCodeDisplaysPostgres(t *testing.T) {
	t.Setenv("SUPERVISOR_WORKSTATION_CPSAT_PATH", "/missing/item-identity-solver")
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	line, _, _, station, body := itemIdentityFixture(f)
	job := identityOrder(t, f, f.call(f.lead, "POST", "/v1/sites/"+f.site+"/job-orders", "display-job", "", f.encode(body)))
	plans := "/v1/sites/" + f.site + "/shift-plans"
	w := f.call(f.lead, "POST", plans+"/suggestions", "", "", f.encode(routingSelection(line, []string{job.ID})))
	expectSuggestion(t, w, 200, "")
	var suggested workstation.ShiftPlanSuggestionV1
	if err := json.Unmarshal(w.Body.Bytes(), &suggested); err != nil {
		t.Fatal(err)
	}
	if len(suggested.Assignments) != 1 || suggested.Assignments[0].ItemCode != "SKU-PRESS-42" {
		t.Fatalf("suggestion display: %s", w.Body.String())
	}
	now := f.clock()
	end := now.Add(time.Minute)
	w = f.call(f.lead, "POST", plans, "display-plan", "", f.encode(workstation.ShiftPlanBodyV1{SchemaVersion: 1, LineID: line, StartsAt: stamp(now.Add(-time.Hour)), EndsAt: stamp(end), Timezone: "UTC", Assignments: suggested.Assignments}))
	expectSuggestion(t, w, 201, "")
	var plan workstation.ShiftPlanV1
	if err := json.Unmarshal(w.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	f.expect(f.call(f.approver, "POST", plans+"/"+plan.ID+"/approvals", "display-approve", `"1"`, f.encode(workstation.ShiftPlanApproveInputV1{CanonicalPayloadSHA256: plan.Revisions[0].CanonicalPayloadSHA256})), 200, "")
	f.startRun(f.lead, plan.ID)
	f.expect(f.authorize(f.lead, station, f.operator, workstation.AuthorizationGrant, `"0"`), 201, "")
	recordedAt := f.clock()
	for i, kind := range []workstation.RecordKind{workstation.KindAcknowledged, workstation.KindStarted, workstation.KindQuantity} {
		record := workstation.StationRecordBodyV1{SchemaVersion: 1, Kind: kind, WorkstationID: station, JobOrderID: job.ID, JobOrderRevision: 1, EventAt: stamp(recordedAt.Add(time.Duration(i) * time.Millisecond))}
		if kind == workstation.KindQuantity {
			record.GoodQuantity, record.RejectQuantity = pointer(int64(30)), pointer(int64(0))
		}
		f.expect(f.call(f.operator, "POST", "/v1/sites/"+f.site+"/workstations/"+station+"/records", uuid.NewString(), "", f.encode(record)), 201, "")
	}
	w = f.call(f.lead, "POST", plans+"/"+plan.ID+"/summaries", "display-summary", "", `{"schema_version":1}`)
	expectSuggestion(t, w, 201, "")
	var summary workstation.ShiftSummarySnapshotV1
	if err := json.Unmarshal(w.Body.Bytes(), &summary); err != nil {
		t.Fatal(err)
	}
	if len(summary.Summary.Stations) != 1 || summary.Summary.Stations[0].Planned == nil || summary.Summary.Stations[0].Planned.ItemCode != "SKU-PRESS-42" {
		t.Fatalf("summary display: %s", w.Body.String())
	}
	w = f.call(f.lead, "POST", plans+"/"+plan.ID+"/handovers", "display-handover", "", f.encode(workstation.ShiftHandoverRequestV1{SchemaVersion: 1, SummaryRevision: 1, NextShift: workstation.NextShiftV1{LineID: line, StartsAt: stamp(end), EndsAt: stamp(end.Add(time.Hour)), Timezone: "UTC"}}))
	expectSuggestion(t, w, 201, "")
	var handover workstation.ShiftHandoverSnapshotV1
	if err := json.Unmarshal(w.Body.Bytes(), &handover); err != nil {
		t.Fatal(err)
	}
	if p := handover.Handover.ProposedPlan; p == nil || len(p.Assignments) != 1 || p.Assignments[0].ItemCode != "SKU-PRESS-42" || p.Assignments[0].TargetQuantity != 70 {
		t.Fatalf("handover display: %s", w.Body.String())
	}
}

func TestUnlinkedUUIDItemCodeIsNotCatalogIdentityPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	line, item, _, _, body := itemIdentityFixture(f)
	delete(body, "item_id")
	delete(body, "routing_id")
	delete(body, "step_number")
	body["item_code"] = item // A UUID-looking human code is still only a code.
	job := identityOrder(t, f, f.call(f.lead, "POST", "/v1/sites/"+f.site+"/job-orders", "unlinked-uuid", "", f.encode(body)))
	var selection workstation.ShiftPlanSelectionV1
	if err := json.Unmarshal([]byte(f.encode(routingSelection(line, []string{job.ID}))), &selection); err != nil {
		t.Fatal(err)
	}
	out, err := workstation.NewShiftPlans(f.runtime).Suggest(ctx, identityActor(f), workstation.Scope{TenantID: f.tenant, SiteID: f.site}, selection, "/missing/solver")
	if !errors.Is(err, workstation.ErrNoFeasibleSchedule) || len(out.Assignments) != 0 || len(out.UnplacedOrders) != 1 || out.UnplacedOrders[0].JobOrderID != job.ID || out.UnplacedOrders[0].ReasonCode != "ROUTING_UNLINKED" {
		t.Fatalf("UUID code inferred catalog identity: %+v %v", out, err)
	}
}

func TestShiftSuggestionsBusyDoesNotUnlockPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	config, err := pgx.ParseConfig(os.Getenv("SUPERVISOR_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	if err = f.runtime.QueryRowContext(ctx, "SELECT current_database()").Scan(&config.Database); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	warnings := 0
	config.OnNotice = func(_ *pgconn.PgConn, n *pgconn.Notice) {
		if n.Severity == "WARNING" {
			mu.Lock()
			warnings++
			mu.Unlock()
		}
	}
	db := stdlib.OpenDB(*config)
	defer db.Close()
	holder, err := f.control.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback()
	if _, err = holder.ExecContext(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,520))", f.tenant+"/"+f.site); err != nil {
		t.Fatal(err)
	}
	var selection workstation.ShiftPlanSelectionV1
	if err = json.Unmarshal([]byte(f.encode(routingSelection(uuid.NewString(), []string{uuid.NewString()}))), &selection); err != nil {
		t.Fatal(err)
	}
	if _, err = workstation.NewShiftPlans(db).Suggest(ctx, identityActor(f), workstation.Scope{TenantID: f.tenant, SiteID: f.site}, selection, "/missing/solver"); !errors.Is(err, workstation.ErrPlannerBusy) {
		t.Fatalf("expected busy: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if warnings != 0 {
		t.Fatalf("busy request emitted %d PostgreSQL warnings", warnings)
	}
}

func identityOrder(t *testing.T, f *stationFixture, w *httptest.ResponseRecorder) workstation.JobOrderV1 {
	t.Helper()
	expectSuggestion(t, w, 201, "")
	return f.decodeOrder(w)
}
