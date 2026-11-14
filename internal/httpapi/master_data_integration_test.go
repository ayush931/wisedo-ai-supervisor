//go:build integration

package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/canonical"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/workstation"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

func masterLineBody(name string, active bool) workstation.LineBodyV1 {
	return workstation.LineBodyV1{SchemaVersion: 1, DisplayName: name, Active: active}
}
func masterItemBody(name string, active bool) workstation.ItemBodyV1 {
	return workstation.ItemBodyV1{SchemaVersion: 1, DisplayName: name, Unit: workstation.MasterDataUnitCount, Active: active}
}
func masterCalendarBody(name string) workstation.ShiftCalendarBodyV1 {
	return workstation.ShiftCalendarBodyV1{SchemaVersion: 1, DisplayName: name, TimeZone: "America/Detroit", Shifts: []workstation.ShiftCalendarShiftV1{{DayOfWeek: 1, StartLocal: "07:00", EndLocal: "15:00"}}}
}
func masterProfileBody(station, line, name string) workstation.StationProfileBodyV1 {
	return workstation.StationProfileBodyV1{SchemaVersion: 1, WorkstationID: station, LineID: line, DisplayName: name, Template: "production", Fields: []workstation.ProfileFieldV1{{Name: "good_units", Label: "Good units", Kind: "decimal", Required: true, Unit: "count"}}}
}
func masterCreateLine(f *stationFixture, actor stationSession, site, key string, body workstation.LineBodyV1) workstation.LineV1 {
	f.t.Helper()
	w := f.call(actor, "POST", "/v1/sites/"+site+"/lines", key, `"0"`, f.encode(body))
	f.expect(w, 201, "")
	var out workstation.LineV1
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		f.t.Fatal(err)
	}
	return out
}
func masterCreateItem(f *stationFixture, actor stationSession, site, key string, body workstation.ItemBodyV1) workstation.ItemV1 {
	f.t.Helper()
	w := f.call(actor, "POST", "/v1/sites/"+site+"/items", key, `"0"`, f.encode(body))
	f.expect(w, 201, "")
	var out workstation.ItemV1
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		f.t.Fatal(err)
	}
	return out
}
func masterCreateCalendar(f *stationFixture, actor stationSession, site, key string, body workstation.ShiftCalendarBodyV1) workstation.ShiftCalendarV1 {
	f.t.Helper()
	w := f.call(actor, "POST", "/v1/sites/"+site+"/shift-calendars", key, `"0"`, f.encode(body))
	f.expect(w, 201, "")
	var out workstation.ShiftCalendarV1
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		f.t.Fatal(err)
	}
	return out
}
func masterCreateProfile(f *stationFixture, actor stationSession, site, key string, body workstation.StationProfileBodyV1) workstation.StationProfileV1 {
	f.t.Helper()
	w := f.call(actor, "POST", "/v1/sites/"+site+"/workstations/"+body.WorkstationID+"/profile-revisions", key, `"0"`, f.encode(body))
	f.expect(w, 201, "")
	var out workstation.StationProfileV1
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		f.t.Fatal(err)
	}
	return out
}
func masterCreateRouting(f *stationFixture, actor stationSession, site, key string, body workstation.RoutingBodyV1) workstation.RoutingV1 {
	f.t.Helper()
	w := f.call(actor, "POST", "/v1/sites/"+site+"/routings", key, `"0"`, f.encode(body))
	f.expect(w, 201, "")
	var out workstation.RoutingV1
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		f.t.Fatal(err)
	}
	return out
}
func masterDecodeProblem(w *httptest.ResponseRecorder) map[string]any {
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return out
}
func masterSettings(f *stationFixture, actor stationSession, enabled bool, key, revision string) *httptest.ResponseRecorder {
	f.t.Helper()
	body := workstation.SiteMasterDataSettingsBodyV1{SchemaVersion: 1, StationProfileLineValidationEnabled: enabled}
	return f.call(actor, "POST", "/v1/sites/"+f.site+"/master-data-settings/revisions", key, revision, f.encode(body))
}

func TestMasterDataRevisionReplayAndPreconditions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	body := masterLineBody("Assembly", true)
	created := f.call(f.lead, "POST", "/v1/sites/"+f.site+"/lines", "line-create", `"0"`, f.encode(body))
	f.expect(created, 201, "")
	var first workstation.LineV1
	if err := json.Unmarshal(created.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	if first.CurrentRevision != 1 || first.ID == "" || created.Header().Get("ETag") != `"1"` || first.Revisions[0].CanonicalPayloadSHA256 == "" {
		t.Fatalf("created line: %+v", first)
	}
	replay := f.call(f.lead, "POST", "/v1/sites/"+f.site+"/lines", "line-create", `"0"`, f.encode(body))
	f.expect(replay, 201, "")
	var replayed workstation.LineV1
	if err := json.Unmarshal(replay.Body.Bytes(), &replayed); err != nil {
		t.Fatal(err)
	}
	if !replayed.Replayed || replayed.ID != first.ID || replayed.CurrentRevision != first.CurrentRevision {
		t.Fatalf("replay: %+v", replayed)
	}
	changed := masterLineBody("Changed", true)
	f.expect(f.call(f.lead, "POST", "/v1/sites/"+f.site+"/lines", "line-create", `"0"`, f.encode(changed)), 409, "IDEMPOTENCY_CONFLICT")
	f.expect(f.call(f.lead, "POST", "/v1/sites/"+f.site+"/lines/"+first.ID+"/revisions", "stale", `"0"`, f.encode(changed)), 412, "REVISION_CONFLICT")
	updated := f.call(f.lead, "POST", "/v1/sites/"+f.site+"/lines/"+first.ID+"/revisions", "line-revise", `"1"`, f.encode(changed))
	f.expect(updated, 201, "")
	var current workstation.LineV1
	if err := json.Unmarshal(updated.Body.Bytes(), &current); err != nil {
		t.Fatal(err)
	}
	if current.CurrentRevision != 2 || current.Revisions[0].Body.DisplayName != "Assembly" || current.Revisions[1].Body.DisplayName != "Changed" {
		t.Fatalf("immutable revision history: %+v", current)
	}
	replay = f.call(f.lead, "POST", "/v1/sites/"+f.site+"/lines", "line-create", `"0"`, f.encode(body))
	f.expect(replay, 201, "")
	if err := json.Unmarshal(replay.Body.Bytes(), &replayed); err != nil {
		t.Fatal(err)
	}
	if !replayed.Replayed || replayed.ReplayedRevision != 1 || replayed.CurrentRevision != 1 || len(replayed.Revisions) != 1 || replay.Header().Get("ETag") != `"1"` {
		t.Fatalf("create replay after revision: status=%d etag=%q result=%+v", replay.Code, replay.Header().Get("ETag"), replayed)
	}
	f.refused(f.tenant, "rewrote a master-data revision", "42501", "permission denied", `UPDATE supervisor.line_revisions SET actor_subject='x' WHERE tenant_id=$1 AND site_id=$2 AND object_id=$3`, f.tenant, f.site, first.ID)
	f.refused(f.tenant, "deleted a master-data revision", "42501", "permission denied", `DELETE FROM supervisor.line_revisions WHERE tenant_id=$1 AND site_id=$2 AND object_id=$3`, f.tenant, f.site, first.ID)
}

func TestMasterDataHeadGuardsRejectRewindAndSkip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	line := masterCreateLine(f, f.owner, f.site, "head-line", masterLineBody("Line", true))
	item := masterCreateItem(f, f.owner, f.site, "head-item", masterItemBody("Part", true))
	station := uuid.NewString()
	masterCreateProfile(f, f.owner, f.site, "head-profile", masterProfileBody(station, uuid.NewString(), "Station"))
	routeBody := workstation.RoutingBodyV1{SchemaVersion: 1, DisplayName: "Route", Active: true, LineID: line.ID, ItemID: item.ID, Steps: []workstation.RoutingStepV1{{StepNumber: 1, WorkstationID: station}}}
	route := masterCreateRouting(f, f.owner, f.site, "head-routing", routeBody)
	calendarBody := masterCalendarBody("Calendar")
	calendar := masterCreateCalendar(f, f.owner, f.site, "head-calendar", calendarBody)

	for _, tc := range []struct {
		path, key string
		body      any
	}{
		{"lines/" + line.ID + "/revisions", "head-line-2", masterLineBody("Line 2", true)},
		{"items/" + item.ID + "/revisions", "head-item-2", masterItemBody("Part 2", true)},
		{"routings/" + route.ID + "/revisions", "head-routing-2", workstation.RoutingBodyV1{SchemaVersion: 1, DisplayName: "Route 2", Active: true, LineID: line.ID, ItemID: item.ID, Steps: routeBody.Steps}},
		{"shift-calendars/" + calendar.ID + "/revisions", "head-calendar-2", workstation.ShiftCalendarBodyV1{SchemaVersion: 1, DisplayName: "Calendar 2", TimeZone: calendarBody.TimeZone, Shifts: calendarBody.Shifts}},
	} {
		f.expect(f.call(f.owner, "POST", "/v1/sites/"+f.site+"/"+tc.path, tc.key, `"1"`, f.encode(tc.body)), 201, "")
	}

	for _, tc := range []struct{ table, id string }{
		{"line_heads", line.ID}, {"item_heads", item.ID}, {"routing_heads", route.ID}, {"shift_calendar_heads", calendar.ID},
	} {
		for _, mutation := range []struct{ label, value string }{{"rewind", "1"}, {"skip", "current_revision+2"}} {
			f.refused(f.tenant, tc.table+" "+mutation.label, "23514", "Invalid master data head transition",
				`UPDATE supervisor.`+tc.table+` SET current_revision=`+mutation.value+` WHERE tenant_id=$1 AND site_id=$2 AND id=$3`, f.tenant, f.site, tc.id)
		}
	}
}

func TestMasterDataImmutableTriggersRejectPrivilegedMutations(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	line := masterCreateLine(f, f.owner, f.site, "immutable-line", masterLineBody("Line", true))
	item := masterCreateItem(f, f.owner, f.site, "immutable-item", masterItemBody("Part", true))
	station := uuid.NewString()
	masterCreateProfile(f, f.owner, f.site, "immutable-profile", masterProfileBody(station, line.ID, "Station"))
	route := masterCreateRouting(f, f.owner, f.site, "immutable-routing", workstation.RoutingBodyV1{SchemaVersion: 1, DisplayName: "Route", Active: true, LineID: line.ID, ItemID: item.ID, Steps: []workstation.RoutingStepV1{{StepNumber: 1, WorkstationID: station}}})
	calendar := masterCreateCalendar(f, f.owner, f.site, "immutable-calendar", masterCalendarBody("Calendar"))
	f.expect(masterSettings(f, f.owner, false, "immutable-settings", `"0"`), 201, "")

	tables := []string{"line_revisions", "item_revisions", "routing_revisions", "shift_calendar_revisions", "site_master_data_setting_revisions", "routing_steps", "master_data_receipts"}
	for _, table := range tables {
		f.exec(`GRANT UPDATE,DELETE ON supervisor.` + table + ` TO supervisor_runtime`)
	}
	checks := []struct {
		table, key, update, remove string
		args                       []any
	}{
		{"line_revisions", "line", `UPDATE supervisor.line_revisions SET actor_subject='changed' WHERE tenant_id=$1 AND site_id=$2 AND object_id=$3 AND revision=1`, `DELETE FROM supervisor.line_revisions WHERE tenant_id=$1 AND site_id=$2 AND object_id=$3 AND revision=1`, []any{f.tenant, f.site, line.ID}},
		{"item_revisions", "item", `UPDATE supervisor.item_revisions SET actor_subject='changed' WHERE tenant_id=$1 AND site_id=$2 AND object_id=$3 AND revision=1`, `DELETE FROM supervisor.item_revisions WHERE tenant_id=$1 AND site_id=$2 AND object_id=$3 AND revision=1`, []any{f.tenant, f.site, item.ID}},
		{"routing_revisions", "routing", `UPDATE supervisor.routing_revisions SET actor_subject='changed' WHERE tenant_id=$1 AND site_id=$2 AND object_id=$3 AND revision=1`, `DELETE FROM supervisor.routing_revisions WHERE tenant_id=$1 AND site_id=$2 AND object_id=$3 AND revision=1`, []any{f.tenant, f.site, route.ID}},
		{"shift_calendar_revisions", "calendar", `UPDATE supervisor.shift_calendar_revisions SET actor_subject='changed' WHERE tenant_id=$1 AND site_id=$2 AND object_id=$3 AND revision=1`, `DELETE FROM supervisor.shift_calendar_revisions WHERE tenant_id=$1 AND site_id=$2 AND object_id=$3 AND revision=1`, []any{f.tenant, f.site, calendar.ID}},
		{"site_master_data_setting_revisions", "settings", `UPDATE supervisor.site_master_data_setting_revisions SET actor_subject='changed' WHERE tenant_id=$1 AND site_id=$2 AND revision=1`, `DELETE FROM supervisor.site_master_data_setting_revisions WHERE tenant_id=$1 AND site_id=$2 AND revision=1`, []any{f.tenant, f.site}},
		{"routing_steps", "step", `UPDATE supervisor.routing_steps SET step_number=step_number WHERE tenant_id=$1 AND site_id=$2 AND routing_id=$3 AND routing_revision=1 AND step_number=1`, `DELETE FROM supervisor.routing_steps WHERE tenant_id=$1 AND site_id=$2 AND routing_id=$3 AND routing_revision=1 AND step_number=1`, []any{f.tenant, f.site, route.ID}},
		{"master_data_receipts", "receipt", `UPDATE supervisor.master_data_receipts SET actor_subject='changed' WHERE tenant_id=$1 AND site_id=$2 AND actor_issuer=$3 AND actor_subject=$4 AND idempotency_key='immutable-line'`, `DELETE FROM supervisor.master_data_receipts WHERE tenant_id=$1 AND site_id=$2 AND actor_issuer=$3 AND actor_subject=$4 AND idempotency_key='immutable-line'`, []any{f.tenant, f.site, f.issuer, f.owner.subject}},
	}
	for _, check := range checks {
		for _, mutation := range []struct{ verb, statement string }{{"updated", check.update}, {"deleted", check.remove}} {
			f.refused(f.tenant, mutation.verb+" privileged "+check.key, "42501", "Evidence history is immutable", mutation.statement, check.args...)
		}
	}
}

func TestMasterDataDigestCanonicalOrderingAndCreatedAtUTC(t *testing.T) {
	// time.Local is process-global; integration tests do not use t.Parallel.
	local, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	previousLocal := time.Local
	time.Local = local
	defer func() { time.Local = previousLocal }()

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)

	line := masterCreateLine(f, f.owner, f.site, "digest-line", masterLineBody("Line", true))
	item := masterCreateItem(f, f.owner, f.site, "digest-item", masterItemBody("Part", true))
	stationA, stationB := uuid.NewString(), uuid.NewString()
	masterCreateProfile(f, f.owner, f.site, "digest-profile-a", masterProfileBody(stationA, line.ID, "Station A"))
	masterCreateProfile(f, f.owner, f.site, "digest-profile-b", masterProfileBody(stationB, line.ID, "Station B"))
	routeBody := workstation.RoutingBodyV1{SchemaVersion: 1, DisplayName: "Route", Active: true, LineID: line.ID, ItemID: item.ID,
		Steps: []workstation.RoutingStepV1{{StepNumber: 1, WorkstationID: stationA}, {StepNumber: 2, WorkstationID: stationB}}}
	routeA := masterCreateRouting(f, f.owner, f.site, "digest-route-a", routeBody)
	reversedRoute := routeBody
	reversedRoute.Steps = []workstation.RoutingStepV1{routeBody.Steps[1], routeBody.Steps[0]}
	routeB := masterCreateRouting(f, f.owner, f.site, "digest-route-b", reversedRoute)
	if routeA.Revisions[0].CanonicalPayloadSHA256 != routeB.Revisions[0].CanonicalPayloadSHA256 || !routeA.Revisions[0].CreatedAt.Before(routeB.Revisions[0].CreatedAt) {
		t.Fatalf("routing digest or creation times differ unexpectedly: A=%+v B=%+v", routeA.Revisions[0], routeB.Revisions[0])
	}
	var wire struct {
		Revisions []struct {
			CreatedAt string `json:"created_at"`
		} `json:"revisions"`
	}
	routeRead := f.call(f.owner, "GET", "/v1/sites/"+f.site+"/routings/"+routeA.ID, "", "", "")
	f.expect(routeRead, 200, "")
	if err := json.Unmarshal(routeRead.Body.Bytes(), &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.Revisions) != 1 || !strings.HasSuffix(wire.Revisions[0].CreatedAt, "Z") {
		t.Fatalf("created_at was not emitted in UTC: %+v", wire.Revisions)
	}
	changedRoute := routeBody
	changedRoute.DisplayName = "Changed route"
	routeChanged := masterCreateRouting(f, f.owner, f.site, "digest-route-changed", changedRoute)
	if routeA.Revisions[0].CanonicalPayloadSHA256 == routeChanged.Revisions[0].CanonicalPayloadSHA256 {
		t.Fatal("routing body field change did not change digest")
	}

	calendarBody := workstation.ShiftCalendarBodyV1{SchemaVersion: 1, DisplayName: "Day", TimeZone: "America/Detroit", Shifts: []workstation.ShiftCalendarShiftV1{
		{DayOfWeek: 1, StartLocal: "07:00", EndLocal: "11:00"}, {DayOfWeek: 1, StartLocal: "12:00", EndLocal: "16:00"},
	}}
	calendarA := masterCreateCalendar(f, f.owner, f.site, "digest-calendar-a", calendarBody)
	reversedCalendar := calendarBody
	reversedCalendar.Shifts = []workstation.ShiftCalendarShiftV1{calendarBody.Shifts[1], calendarBody.Shifts[0]}
	calendarB := masterCreateCalendar(f, f.owner, f.site, "digest-calendar-b", reversedCalendar)
	if calendarA.Revisions[0].CanonicalPayloadSHA256 != calendarB.Revisions[0].CanonicalPayloadSHA256 || !calendarA.Revisions[0].CreatedAt.Before(calendarB.Revisions[0].CreatedAt) {
		t.Fatalf("calendar digest or creation times differ unexpectedly: A=%+v B=%+v", calendarA.Revisions[0], calendarB.Revisions[0])
	}
	changedCalendar := calendarBody
	changedCalendar.Shifts = []workstation.ShiftCalendarShiftV1{{DayOfWeek: 1, StartLocal: "07:00", EndLocal: "14:00"}, {DayOfWeek: 1, StartLocal: "15:00", EndLocal: "16:00"}}
	calendarChanged := masterCreateCalendar(f, f.owner, f.site, "digest-calendar-changed", changedCalendar)
	if calendarA.Revisions[0].CanonicalPayloadSHA256 == calendarChanged.Revisions[0].CanonicalPayloadSHA256 {
		t.Fatal("calendar body field change did not change digest")
	}
	f.expect(masterSettings(f, f.owner, false, "digest-settings", `"0"`), 201, "")
	settingsRead := f.call(f.owner, "GET", "/v1/sites/"+f.site+"/master-data-settings", "", "", "")
	f.expect(settingsRead, 200, "")
	var settingsWire struct {
		Revisions []struct {
			CreatedAt string `json:"created_at"`
		} `json:"revisions"`
	}
	if err := json.Unmarshal(settingsRead.Body.Bytes(), &settingsWire); err != nil {
		t.Fatal(err)
	}
	if len(settingsWire.Revisions) != 1 || !strings.HasSuffix(settingsWire.Revisions[0].CreatedAt, "Z") {
		t.Fatalf("settings created_at was not emitted in UTC: %+v", settingsWire.Revisions)
	}
}

func TestMasterDataViewerReadOnly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	line := masterCreateLine(f, f.owner, f.site, "viewer-line", masterLineBody("Line", true))
	item := masterCreateItem(f, f.owner, f.site, "viewer-item", masterItemBody("Part", true))
	station := uuid.NewString()
	masterCreateProfile(f, f.owner, f.site, "viewer-profile", masterProfileBody(station, uuid.NewString(), "Station"))
	route := masterCreateRouting(f, f.owner, f.site, "viewer-routing", workstation.RoutingBodyV1{SchemaVersion: 1, DisplayName: "Route", Active: true, LineID: line.ID, ItemID: item.ID, Steps: []workstation.RoutingStepV1{{StepNumber: 1, WorkstationID: station}}})
	calendar := masterCreateCalendar(f, f.owner, f.site, "viewer-calendar", masterCalendarBody("Day"))
	for _, path := range []string{"lines", "items", "routings", "shift-calendars"} {
		f.expect(f.call(f.viewer, "GET", "/v1/sites/"+f.site+"/"+path, "", "", ""), 200, "")
	}
	for _, path := range []string{"lines/" + line.ID, "lines/" + line.ID + "/revisions", "items/" + item.ID, "routings/" + route.ID, "shift-calendars/" + calendar.ID, "master-data-settings"} {
		f.expect(f.call(f.viewer, "GET", "/v1/sites/"+f.site+"/"+path, "", "", ""), 200, "")
	}
	f.expect(f.call(f.viewer, "POST", "/v1/sites/"+f.site+"/lines", "viewer-write-line", `"0"`, f.encode(masterLineBody("no", true))), 403, "PERMISSION_DENIED")
	f.expect(f.call(f.viewer, "POST", "/v1/sites/"+f.site+"/items", "viewer-write-item", `"0"`, f.encode(masterItemBody("no", true))), 403, "PERMISSION_DENIED")
	f.expect(f.call(f.viewer, "POST", "/v1/sites/"+f.site+"/routings", "viewer-write-route", `"0"`, f.encode(workstation.RoutingBodyV1{SchemaVersion: 1, DisplayName: "no", Active: true, LineID: line.ID, ItemID: item.ID, Steps: []workstation.RoutingStepV1{{StepNumber: 1, WorkstationID: station}}})), 403, "PERMISSION_DENIED")
	f.expect(f.call(f.viewer, "POST", "/v1/sites/"+f.site+"/shift-calendars", "viewer-write-calendar", `"0"`, f.encode(masterCalendarBody("no"))), 403, "PERMISSION_DENIED")
	f.expect(masterSettings(f, f.viewer, true, "viewer-write-settings", `"0"`), 403, "PERMISSION_DENIED")
}

func TestMasterDataCrossScopeReadsAreNotFound(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	masterCreateLine(f, f.owner, f.site, "scope-line", masterLineBody("Private line", true))
	foreign := masterCreateLine(f, f.outsider, f.otherSite, "foreign-line", masterLineBody("Foreign secret", true))
	sameTenantSite := uuid.NewString()
	f.exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'same tenant site')`, f.tenant, sameTenantSite)
	f.exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'owner')`, f.tenant, sameTenantSite, f.issuer, f.owner.subject)
	otherSiteLine := masterCreateLine(f, f.owner, sameTenantSite, "same-tenant-line", masterLineBody("Other site private", true))
	base := "/v1/sites/" + f.site + "/lines"
	f.expect(f.call(f.owner, "GET", base+"/"+foreign.ID, "", "", ""), 404, "NOT_FOUND")
	f.expect(f.call(f.owner, "GET", base+"/"+otherSiteLine.ID, "", "", ""), 404, "NOT_FOUND")
	f.expect(f.call(f.owner, "GET", "/v1/sites/"+f.otherSite+"/lines", "", "", ""), 404, "NOT_FOUND")
	w := f.call(f.owner, "GET", base, "", "", "")
	f.expect(w, 200, "")
	if strings.Contains(w.Body.String(), "Foreign secret") || strings.Contains(w.Body.String(), "Other site private") || !strings.Contains(w.Body.String(), "Private line") {
		t.Fatalf("site list leaked or omitted data: %s", w.Body.String())
	}
	page := f.call(f.owner, "GET", "/v1/sites/"+f.otherSite+"/lines", "", "", "")
	if page.Code != 404 || strings.Contains(page.Body.String(), foreign.ID) {
		t.Fatalf("foreign site leaked: %d %s", page.Code, page.Body.String())
	}
}

func TestMasterDataRoutingStepsRequireSameSiteWorkstation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	line := masterCreateLine(f, f.owner, f.site, "step-line", masterLineBody("Line", true))
	item := masterCreateItem(f, f.owner, f.site, "step-item", masterItemBody("Item", true))
	stationA, stationB := uuid.NewString(), uuid.NewString()
	masterCreateProfile(f, f.owner, f.site, "step-station-a", masterProfileBody(stationA, uuid.NewString(), "Station A"))
	masterCreateProfile(f, f.owner, f.site, "step-station-b", masterProfileBody(stationB, uuid.NewString(), "Station B"))
	foreignStation := uuid.NewString()
	masterCreateProfile(f, f.outsider, f.otherSite, "step-foreign-station", masterProfileBody(foreignStation, uuid.NewString(), "Foreign station"))
	body := workstation.RoutingBodyV1{SchemaVersion: 1, DisplayName: "Route", Active: true, LineID: line.ID, ItemID: item.ID,
		Steps: []workstation.RoutingStepV1{{StepNumber: 2, WorkstationID: stationB}, {StepNumber: 1, WorkstationID: stationA}}}
	route := masterCreateRouting(f, f.owner, f.site, "ordered-route", body)
	if got := route.Revisions[0].Body.Steps; len(got) != 2 || got[0].StepNumber != 1 || got[0].WorkstationID != stationA || got[1].StepNumber != 2 || got[1].WorkstationID != stationB {
		t.Fatalf("routing step order: %+v", got)
	}
	bad := body
	bad.Steps = []workstation.RoutingStepV1{{StepNumber: 1, WorkstationID: foreignStation}}
	w := f.call(f.owner, "POST", "/v1/sites/"+f.site+"/routings", "foreign-workstation", `"0"`, f.encode(bad))
	if w.Code != 422 || masterDecodeProblem(w)["path"] != "steps[0].workstation_id" {
		t.Fatalf("cross-site workstation: %d %s", w.Code, w.Body.String())
	}
	f.refused(f.tenant, "inserted a cross-site routing workstation", "23503", "violates foreign key constraint", `INSERT INTO supervisor.routing_steps(tenant_id,site_id,routing_id,routing_revision,step_number,workstation_id) VALUES($1,$2,$3,1,99,$4)`, f.tenant, f.site, route.ID, foreignStation)
}

func TestMasterDataCrossSiteLineItemReferencesFail(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	line := masterCreateLine(f, f.owner, f.site, "ref-line", masterLineBody("Local line", true))
	item := masterCreateItem(f, f.owner, f.site, "ref-item", masterItemBody("Local item", true))
	foreignLine := masterCreateLine(f, f.outsider, f.otherSite, "ref-foreign-line", masterLineBody("Foreign line", true))
	foreignItem := masterCreateItem(f, f.outsider, f.otherSite, "ref-foreign-item", masterItemBody("Foreign item", true))
	station := uuid.NewString()
	masterCreateProfile(f, f.owner, f.site, "ref-station", masterProfileBody(station, uuid.NewString(), "Station"))
	base := workstation.RoutingBodyV1{SchemaVersion: 1, DisplayName: "Route", Active: true, LineID: line.ID, ItemID: item.ID, Steps: []workstation.RoutingStepV1{{StepNumber: 1, WorkstationID: station}}}
	badLine := base
	badLine.LineID = foreignLine.ID
	w := f.call(f.owner, "POST", "/v1/sites/"+f.site+"/routings", "foreign-line-route", `"0"`, f.encode(badLine))
	if w.Code != 422 || masterDecodeProblem(w)["path"] != "line_id" {
		t.Fatalf("cross-site line: %d %s", w.Code, w.Body.String())
	}
	badItem := base
	badItem.ItemID = foreignItem.ID
	w = f.call(f.owner, "POST", "/v1/sites/"+f.site+"/routings", "foreign-item-route", `"0"`, f.encode(badItem))
	if w.Code != 422 || masterDecodeProblem(w)["path"] != "item_id" {
		t.Fatalf("cross-site item: %d %s", w.Code, w.Body.String())
	}
	masterCreateRouting(f, f.owner, f.site, "reference-route", base)
	f.expect(f.call(f.owner, "POST", "/v1/sites/"+f.site+"/items/"+item.ID+"/revisions", "retire-used-item", `"1"`, f.encode(masterItemBody("Local item", false))), 409, "MASTER_DATA_IN_USE")

	for _, pair := range []struct{ lineID, itemID string }{{foreignLine.ID, item.ID}, {line.ID, foreignItem.ID}} {
		id := uuid.NewString()
		body := workstation.RoutingBodyV1{SchemaVersion: 1, DisplayName: "Direct route", Active: true, LineID: pair.lineID, ItemID: pair.itemID, Steps: base.Steps}
		payload, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		canonicalPayload, digest, err := canonical.Envelope("RoutingBodyV1", 1, f.tenant, f.site, body)
		if err != nil {
			t.Fatal(err)
		}
		rtx, err := f.runtime.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = rtx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, f.tenant); err != nil {
			t.Fatal(err)
		}
		if _, err = rtx.ExecContext(ctx, `INSERT INTO supervisor.routing_heads(tenant_id,site_id,id,current_revision) VALUES($1,$2,$3,1)`, f.tenant, f.site, id); err != nil {
			t.Fatal(err)
		}
		_, err = rtx.ExecContext(ctx, `INSERT INTO supervisor.routing_revisions(tenant_id,site_id,object_id,revision,line_id,item_id,payload,canonical_payload,canonical_payload_sha256,digest_schema_version,actor_issuer,actor_subject) VALUES($1,$2,$3,1,$4,$5,$6::jsonb,$7,$8,1,'test-issuer','test-subject')`, f.tenant, f.site, id, pair.lineID, pair.itemID, string(payload), string(canonicalPayload), digest)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23503" || !strings.Contains(pgErr.Message, "violates foreign key constraint") {
			t.Fatalf("direct cross-site line/item insert: want foreign-key violation, got %v", err)
		}
		_ = rtx.Rollback()
	}
	inactiveLine := masterCreateLine(f, f.owner, f.site, "inactive-line", masterLineBody("Inactive line", false))
	inactiveItem := masterCreateItem(f, f.owner, f.site, "inactive-item", masterItemBody("Inactive item", false))
	badLine = base
	badLine.LineID = inactiveLine.ID
	w = f.call(f.owner, "POST", "/v1/sites/"+f.site+"/routings", "inactive-line-route", `"0"`, f.encode(badLine))
	if w.Code != 422 || masterDecodeProblem(w)["path"] != "line_id" {
		t.Fatalf("inactive line: %d %s", w.Code, w.Body.String())
	}
	badItem = base
	badItem.ItemID = inactiveItem.ID
	w = f.call(f.owner, "POST", "/v1/sites/"+f.site+"/routings", "inactive-item-route", `"0"`, f.encode(badItem))
	if w.Code != 422 || masterDecodeProblem(w)["path"] != "item_id" {
		t.Fatalf("inactive item: %d %s", w.Code, w.Body.String())
	}
}

func TestMasterDataCursorPagesAreStable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	lines := []workstation.LineV1{masterCreateLine(f, f.owner, f.site, "page-line-a", masterLineBody("A", true)), masterCreateLine(f, f.owner, f.site, "page-line-b", masterLineBody("B", true))}
	items := []workstation.ItemV1{masterCreateItem(f, f.owner, f.site, "page-item-a", masterItemBody("A", true)), masterCreateItem(f, f.owner, f.site, "page-item-b", masterItemBody("B", true))}
	calendars := []workstation.ShiftCalendarV1{masterCreateCalendar(f, f.owner, f.site, "page-calendar-a", masterCalendarBody("A")), masterCreateCalendar(f, f.owner, f.site, "page-calendar-b", masterCalendarBody("B"))}
	stationA, stationB := uuid.NewString(), uuid.NewString()
	masterCreateProfile(f, f.owner, f.site, "page-station-a", masterProfileBody(stationA, uuid.NewString(), "A"))
	masterCreateProfile(f, f.owner, f.site, "page-station-b", masterProfileBody(stationB, uuid.NewString(), "B"))
	routes := []workstation.RoutingV1{
		masterCreateRouting(f, f.owner, f.site, "page-route-a", workstation.RoutingBodyV1{SchemaVersion: 1, DisplayName: "A", Active: true, LineID: lines[0].ID, ItemID: items[0].ID, Steps: []workstation.RoutingStepV1{{StepNumber: 1, WorkstationID: stationA}}}),
		masterCreateRouting(f, f.owner, f.site, "page-route-b", workstation.RoutingBodyV1{SchemaVersion: 1, DisplayName: "B", Active: true, LineID: lines[1].ID, ItemID: items[1].ID, Steps: []workstation.RoutingStepV1{{StepNumber: 1, WorkstationID: stationB}}}),
	}
	cases := []struct {
		path, field string
		want        []string
	}{
		{"lines", "lines", []string{lines[0].ID, lines[1].ID}}, {"items", "items", []string{items[0].ID, items[1].ID}},
		{"routings", "routings", []string{routes[0].ID, routes[1].ID}}, {"shift-calendars", "shift_calendars", []string{calendars[0].ID, calendars[1].ID}},
	}
	for _, tc := range cases {
		slices.Sort(tc.want)
		var got []string
		cursor := ""
		for {
			path := "/v1/sites/" + f.site + "/" + tc.path + "?limit=1"
			if cursor != "" {
				path += "&cursor=" + cursor
			}
			w := f.call(f.viewer, "GET", path, "", "", "")
			f.expect(w, 200, "")
			var page map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
				t.Fatal(err)
			}
			rows := page[tc.field].([]any)
			for _, raw := range rows {
				got = append(got, raw.(map[string]any)["id"].(string))
			}
			next, _ := page["next_cursor"].(string)
			if next == "" {
				break
			}
			cursor = next
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%s cursor pages: got %v want %v", tc.path, got, tc.want)
		}
		defaultPage := f.call(f.viewer, "GET", "/v1/sites/"+f.site+"/"+tc.path, "", "", "")
		f.expect(defaultPage, 200, "")
		f.expect(f.call(f.viewer, "GET", "/v1/sites/"+f.site+"/"+tc.path+"?limit=101", "", "", ""), 422, "INVALID_REQUEST")
		f.expect(f.call(f.viewer, "GET", "/v1/sites/"+f.site+"/"+tc.path+"?cursor=bad", "", "", ""), 422, "INVALID_REQUEST")
	}
}

func TestStationProfileLineValidationDefaultOff(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	line := uuid.NewString()
	settings := f.call(f.viewer, "GET", "/v1/sites/"+f.site+"/master-data-settings", "", "", "")
	f.expect(settings, 200, "")
	var current workstation.SiteMasterDataSettingsV1
	if err := json.Unmarshal(settings.Body.Bytes(), &current); err != nil {
		t.Fatal(err)
	}
	if current.CurrentRevision != 0 || current.Body.StationProfileLineValidationEnabled {
		t.Fatalf("default settings: %+v", current)
	}
	station := uuid.NewString()
	body := masterProfileBody(station, line, "legacy free UUID")
	profile := masterCreateProfile(f, f.owner, f.site, "legacy-profile", body)
	wantCanonical, wantDigest, err := canonical.Envelope("StationProfileBodyV1", 1, f.tenant, f.site, body)
	if err != nil {
		t.Fatal(err)
	}
	if profile.Revisions[0].CanonicalPayloadSHA256 != wantDigest {
		t.Fatalf("profile digest changed while setting absent: got %s expected %s (%s)", profile.Revisions[0].CanonicalPayloadSHA256, wantDigest, wantCanonical)
	}
	body.DisplayName = "revised legacy UUID"
	w := f.call(f.owner, "POST", "/v1/sites/"+f.site+"/workstations/"+station+"/profile-revisions", "legacy-profile-2", `"1"`, f.encode(body))
	f.expect(w, 201, "")
	f.expect(f.call(f.viewer, "GET", "/v1/sites/"+f.site+"/workstations/"+station+"/profile", "", "", ""), 200, "")
}

func TestStationProfileLineValidationOptIn(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	station, unknown := uuid.NewString(), uuid.NewString()
	profile := masterCreateProfile(f, f.owner, f.site, "optin-profile", masterProfileBody(station, unknown, "Legacy"))
	f.expect(masterSettings(f, f.owner, true, "enable-blocked", `"0"`), 409, "LINE_VALIDATION_BLOCKED")
	line := masterCreateLine(f, f.owner, f.site, "optin-line", masterLineBody("Line", true))
	body := masterProfileBody(station, line.ID, "Mapped")
	w := f.call(f.owner, "POST", "/v1/sites/"+f.site+"/workstations/"+station+"/profile-revisions", "map-profile", `"1"`, f.encode(body))
	f.expect(w, 201, "")
	settings := masterSettings(f, f.owner, true, "enable-valid", `"0"`)
	f.expect(settings, 201, "")
	var enabled workstation.SiteMasterDataSettingsV1
	if err := json.Unmarshal(settings.Body.Bytes(), &enabled); err != nil {
		t.Fatal(err)
	}
	if enabled.CurrentRevision != 1 || !enabled.Body.StationProfileLineValidationEnabled {
		t.Fatalf("enabled setting: %+v", enabled)
	}
	body.LineID = unknown
	w = f.call(f.owner, "POST", "/v1/sites/"+f.site+"/workstations/"+station+"/profile-revisions", "reject-unknown-line", `"2"`, f.encode(body))
	if w.Code != 422 || masterDecodeProblem(w)["code"] != "INVALID_STATION_PROFILE" || masterDecodeProblem(w)["path"] != "line_id" {
		t.Fatalf("unknown active line: %d %s", w.Code, w.Body.String())
	}
	f.expect(f.call(f.owner, "POST", "/v1/sites/"+f.site+"/lines/"+line.ID+"/revisions", "retire-line", `"1"`, f.encode(masterLineBody("Line", false))), 409, "LINE_VALIDATION_BLOCKED")
	f.expect(masterSettings(f, f.owner, false, "disable", `"1"`), 201, "")
	replay := masterSettings(f, f.owner, true, "enable-valid", `"0"`)
	f.expect(replay, 201, "")
	var replayed workstation.SiteMasterDataSettingsV1
	if err := json.Unmarshal(replay.Body.Bytes(), &replayed); err != nil {
		t.Fatal(err)
	}
	if !replayed.Replayed || replayed.ReplayedRevision != 1 || replayed.CurrentRevision != 1 || len(replayed.Revisions) != 1 || replay.Header().Get("ETag") != `"1"` || !replayed.Body.StationProfileLineValidationEnabled {
		t.Fatalf("settings replay after revision: status=%d etag=%q result=%+v", replay.Code, replay.Header().Get("ETag"), replayed)
	}
	accepted := f.call(f.owner, "POST", "/v1/sites/"+f.site+"/workstations/"+station+"/profile-revisions", "legacy-again", `"2"`, f.encode(body))
	f.expect(accepted, 201, "")
	var read workstation.StationProfileV1
	if err := json.Unmarshal(accepted.Body.Bytes(), &read); err != nil {
		t.Fatal(err)
	}
	if len(read.Revisions) != 3 || read.Revisions[0].CanonicalPayloadSHA256 != profile.Revisions[0].CanonicalPayloadSHA256 {
		t.Fatalf("line setting rewrote profile history: %+v", read)
	}
}

func TestMasterDataUnknownJSONFieldIs422(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	cases := []struct{ path, body string }{
		{"lines", `{"schema_version":1,"display_name":"Line","active":true,"tenant_id":"` + f.tenant + `"}`},
		{"items", `{"schema_version":1,"display_name":"Item","unit":"count","active":true,"tenant_id":"` + f.tenant + `"}`},
		{"routings", `{"schema_version":1,"display_name":"Route","active":true,"line_id":"` + uuid.NewString() + `","item_id":"` + uuid.NewString() + `","steps":[{"step_number":1,"workstation_id":"` + uuid.NewString() + `"}],"tenant_id":"` + f.tenant + `"}`},
		{"shift-calendars", `{"schema_version":1,"display_name":"Calendar","time_zone":"UTC","shifts":[{"day_of_week":1,"start_local":"07:00","end_local":"15:00"}],"tenant_id":"` + f.tenant + `"}`},
		{"master-data-settings/revisions", `{"schema_version":1,"station_profile_line_validation_enabled":false,"unexpected":true}`},
	}
	for i, tc := range cases {
		f.expect(f.call(f.owner, "POST", "/v1/sites/"+f.site+"/"+tc.path, fmt.Sprintf("unknown-%d", i), `"0"`, tc.body), 422, "INVALID_MASTER_DATA")
	}
}

func TestMasterDataExecutableDisplayNameReturnsMemberPath(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	w := f.call(f.owner, "POST", "/v1/sites/"+f.site+"/lines", "formula-line", `"0"`, f.encode(masterLineBody("=HYPERLINK(...)", true)))
	problem := masterDecodeProblem(w)
	if w.Code != 422 || problem["code"] != "INVALID_MASTER_DATA" || problem["path"] != "display_name" {
		t.Fatalf("formula name response: HTTP %d %s, want 422 INVALID_MASTER_DATA at display_name", w.Code, w.Body.String())
	}
}

func TestMasterDataRLSAndLeastPrivilege(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	line := masterCreateLine(f, f.owner, f.site, "rls-line", masterLineBody("Line", true))
	item := masterCreateItem(f, f.owner, f.site, "rls-item", masterItemBody("Item", true))
	masterCreateCalendar(f, f.owner, f.site, "rls-calendar", masterCalendarBody("Calendar"))
	station := uuid.NewString()
	masterCreateProfile(f, f.owner, f.site, "rls-profile", masterProfileBody(station, line.ID, "Station"))
	masterCreateRouting(f, f.owner, f.site, "rls-route", workstation.RoutingBodyV1{SchemaVersion: 1, DisplayName: "Route", Active: true, LineID: line.ID, ItemID: item.ID, Steps: []workstation.RoutingStepV1{{StepNumber: 1, WorkstationID: station}}})
	f.expect(masterSettings(f, f.owner, true, "rls-settings", `"0"`), 201, "")
	tables := []string{"line_heads", "line_revisions", "item_heads", "item_revisions", "routing_heads", "routing_revisions", "routing_steps", "shift_calendar_heads", "shift_calendar_revisions", "site_master_data_setting_heads", "site_master_data_setting_revisions", "master_data_receipts"}
	rtx, err := f.runtime.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rtx.Rollback()
	if _, err = rtx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, f.otherTenant); err != nil {
		t.Fatal(err)
	}
	for _, table := range tables {
		var visible int
		if err = rtx.QueryRowContext(ctx, "SELECT count(*) FROM supervisor."+table+" WHERE tenant_id=$1", f.tenant).Scan(&visible); err != nil {
			t.Fatal(err)
		}
		if visible != 0 {
			t.Errorf("runtime saw %d foreign rows in %s", visible, table)
		}
	}
	for _, table := range tables {
		var forced bool
		var policies int
		if err = f.control.QueryRowContext(ctx, `SELECT c.relrowsecurity AND c.relforcerowsecurity,(SELECT count(*) FROM pg_policy p WHERE p.polrelid=c.oid) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='supervisor' AND c.relname=$1`, table).Scan(&forced, &policies); err != nil {
			t.Fatal(err)
		}
		if !forced || policies != 1 {
			t.Errorf("%s RLS=%v policies=%d", table, forced, policies)
		}
	}
	f.refused(f.tenant, "updated a master-data revision", "42501", "permission denied", `UPDATE supervisor.line_revisions SET actor_subject='x' WHERE tenant_id=$1 AND site_id=$2 AND object_id=$3`, f.tenant, f.site, line.ID)
	f.refused(f.tenant, "deleted a master-data receipt", "42501", "permission denied", `DELETE FROM supervisor.master_data_receipts WHERE tenant_id=$1 AND site_id=$2`, f.tenant, f.site)
}
