//go:build integration

package httpapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/workstation"
	"github.com/google/uuid"
)

// TestStationProfilesHTTPPostgres proves WIS-520 acceptance item 3 (and the "two workstations
// with different screen configurations" case of item 9) against PostgreSQL: station-specific
// operator screens are configured from the fixed templates and field kinds, previewed without
// persistence, and unsupported or executable configuration is refused with its path.
func TestStationProfilesHTTPPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	source, sha, designation := f.instruction("Synthetic SOP: measure bore at 20 C", true)
	unconfirmed, unconfirmedSHA, _ := f.instruction("Unconfirmed gauge SOP", false)
	stations := "/v1/sites/" + f.site + "/workstations"
	stationA, stationB, line := uuid.NewString(), uuid.NewString(), uuid.NewString()
	revisions := func(station string) string { return stations + "/" + station + "/profile-revisions" }
	previews := func(station string) string { return stations + "/" + station + "/profile-previews" }
	tag := func(revision int64) string { return `"` + strconv.FormatInt(revision, 10) + `"` }
	quality := workstation.StationProfileBodyV1{
		SchemaVersion: 1, WorkstationID: stationA, LineID: line, DisplayName: "Bore gauge 2", Template: "quality",
		Fields: []workstation.ProfileFieldV1{
			{Name: "bore_diameter", Label: "Bore diameter (Torque > 25 Nm, see https://sop.invalid)", Kind: "decimal", Required: true, Unit: "mm", Min: "9.95", Max: "10.05"},
			{Name: "disposition", Label: "Disposition", Kind: "choice", Required: true, Choices: []string{"accept", "hold", "reject"}},
		},
		InstructionReferences: []workstation.InstructionRefV1{{DocumentID: source, Revision: 1, SHA256: sha}},
	}
	production := workstation.StationProfileBodyV1{
		SchemaVersion: 1, WorkstationID: stationB, LineID: line, DisplayName: "Press 4", Template: "production",
		Fields: []workstation.ProfileFieldV1{
			{Name: "good_units", Label: "Good units", Kind: "decimal", Required: true, Unit: "count"},
			{Name: "cycle_time", Label: "Cycle time", Kind: "decimal", Required: true, Unit: "s"},
			{Name: "blocked", Label: "Blocked", Kind: "boolean"},
		},
	}
	decode := func(w *httptest.ResponseRecorder, status int) workstation.StationProfileV1 {
		t.Helper()
		f.expect(w, status, "")
		var p workstation.StationProfileV1
		if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
			t.Fatal(err)
		}
		if w.Header().Get("ETag") != tag(p.CurrentRevision) {
			t.Fatalf("ETag %q does not carry current revision %d", w.Header().Get("ETag"), p.CurrentRevision)
		}
		return p
	}
	// rejected requires a 422 with the exact code and member path.
	rejected := func(name string, w *httptest.ResponseRecorder, code, path string) {
		t.Helper()
		var prob map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &prob)
		if w.Code != 422 || prob["code"] != code || (path != "" && prob["path"] != path) || (path == "" && prob["path"] != nil) {
			t.Fatalf("%s: HTTP %d %s, want 422 %s at %q", name, w.Code, w.Body.String(), code, path)
		}
	}
	body := f.encode(quality)

	// Authority: only owners and supervisors configure; a write needs both mutation headers.
	f.expect(f.call(stationSession{}, "POST", revisions(stationA), "a", `"0"`, body), 401, "AUTHENTICATION_REQUIRED")
	f.expect(f.call(f.operator, "POST", revisions(stationA), "a", `"0"`, body), 403, "PERMISSION_DENIED")
	f.expect(f.call(f.viewer, "POST", revisions(stationA), "a", `"0"`, body), 403, "PERMISSION_DENIED")
	f.expect(f.call(f.lead, "POST", revisions(stationA), "a", "", body), 428, "REVISION_REQUIRED")
	f.expect(f.call(f.lead, "POST", revisions(stationA), "", `"0"`, body), 422, "INVALID_REQUEST")
	f.expect(f.call(f.lead, "POST", revisions(stationA), "a", `"1"`, body), 412, "REVISION_CONFLICT")

	// Unsupported or executable configuration is refused explicitly, naming the member.
	for name, tc := range map[string]struct{ body, code, path string }{
		"script member":        {strings.Replace(body, `"schema_version":1`, `"schema_version":1,"script":"fetch('/x')"`, 1), "UNSUPPORTED_STATION_CONFIGURATION", "*"},
		"field handler":        {strings.Replace(body, `"name":"disposition"`, `"name":"disposition","on_change":"run()"`, 1), "UNSUPPORTED_STATION_CONFIGURATION", "fields[1].*"},
		"fields as object":     {strings.Replace(strings.Replace(body, `"fields":[`, `"fields":{"x":[`, 1), `}],"instruction_references"`, `}]},"instruction_references"`, 1), "INVALID_STATION_PROFILE", "fields"},
		"html template":        {strings.Replace(body, `"template":"quality"`, `"template":"html"`, 1), "UNSUPPORTED_STATION_CONFIGURATION", "template"},
		"plugin field kind":    {strings.Replace(body, `"kind":"choice"`, `"kind":"plugin"`, 1), "UNSUPPORTED_STATION_CONFIGURATION", "fields[1].kind"},
		"template label":       {strings.Replace(body, `"label":"Disposition"`, `"label":"{{order.secret}}"`, 1), "UNSUPPORTED_STATION_CONFIGURATION", "fields[1].label"},
		"script scheme choice": {strings.Replace(body, `"hold"`, `" JavaScript:alert(1)"`, 1), "UNSUPPORTED_STATION_CONFIGURATION", "fields[1].choices[1]"},
		"expression choice":    {strings.Replace(body, `"hold"`, `"${order.id}"`, 1), "UNSUPPORTED_STATION_CONFIGURATION", "fields[1].choices[1]"},
		"tenant in body":       {strings.Replace(body, `"schema_version":1`, `"schema_version":1,"tenant_id":"`+f.otherTenant+`"`, 1), "UNSUPPORTED_STATION_CONFIGURATION", "*"},
		"wrong station":        {strings.Replace(body, stationA, stationB, 1), "INVALID_STATION_PROFILE", "workstation_id"},
		"decimal without unit": {strings.Replace(body, `"unit":"mm",`, ``, 1), "INVALID_STATION_PROFILE", "fields[0]"},
		"inverted range":       {strings.Replace(body, `"min":"9.95"`, `"min":"10.5"`, 1), "INVALID_STATION_PROFILE", "fields[0].min"},
		"duplicate key":        {strings.Replace(body, `"schema_version":1`, `"schema_version":1,"schema_version":1`, 1), "INVALID_STATION_PROFILE", ""},
	} {
		rejected(name, f.call(f.lead, "POST", revisions(stationA), "bad-"+name, `"0"`, tc.body), tc.code, tc.path)
	}
	notApproved := quality
	notApproved.InstructionReferences = []workstation.InstructionRefV1{{DocumentID: unconfirmed, Revision: 1, SHA256: unconfirmedSHA}}
	f.expect(f.call(f.lead, "POST", revisions(stationA), "instr", `"0"`, f.encode(notApproved)), 422, "INSTRUCTION_NOT_APPROVED")
	var stored int
	if err := f.control.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.station_profile_heads WHERE tenant_id=$1`, f.tenant).Scan(&stored); err != nil || stored != 0 {
		t.Fatalf("rejected profiles stored %d rows (%v)", stored, err)
	}

	// Two workstations, two different screens; each reads back its own configuration.
	a := decode(f.call(f.lead, "POST", revisions(stationA), "profile-a", `"0"`, body), 201)
	b := decode(f.call(f.owner, "POST", revisions(stationB), "profile-b", `"0"`, f.encode(production)), 201)
	if a.CurrentRevision != 1 || a.WorkstationID != stationA || a.Revisions[0].State != workstation.JobOrderCurrent || a.Revisions[0].SubmittedBy.Subject != f.lead.subject ||
		!reflect.DeepEqual(a.Revisions[0].Profile, quality) || len(a.Revisions[0].CanonicalPayloadSHA256) != 64 {
		t.Fatalf("created profile A: %+v", a)
	}
	readA := decode(f.call(f.operator, "GET", stations+"/"+stationA+"/profile", "", "", ""), 200)
	readB := decode(f.call(f.viewer, "GET", stations+"/"+stationB+"/profile", "", "", ""), 200)
	if !reflect.DeepEqual(readA.Revisions, a.Revisions) || !reflect.DeepEqual(readB.Revisions, b.Revisions) || !reflect.DeepEqual(readB.Revisions[0].Profile, production) ||
		readA.Revisions[0].CanonicalPayloadSHA256 == readB.Revisions[0].CanonicalPayloadSHA256 {
		t.Fatalf("readback A %+v B %+v", readA, readB)
	}
	replay := decode(f.call(f.lead, "POST", revisions(stationA), "profile-a", `"0"`, body), 201)
	if !replay.Replayed || replay.CurrentRevision != 1 {
		t.Fatalf("replay: %+v", replay)
	}
	f.expect(f.call(f.lead, "POST", revisions(stationA), "profile-a", `"0"`, strings.Replace(body, "Bore gauge 2", "Bore gauge 3", 1)), 409, "IDEMPOTENCY_CONFLICT")
	f.expect(f.call(f.lead, "POST", revisions(stationA), "again", `"0"`, body), 412, "REVISION_CONFLICT")
	f.expect(f.call(f.lead, "POST", revisions(stationA), "same", `"1"`, body), 409, "STATION_PROFILE_UNCHANGED")

	// Revise appends and supersedes; history is never rewritten.
	history := func() string {
		t.Helper()
		var s string
		if e := f.control.QueryRowContext(ctx, `SELECT string_agg(concat_ws('|',revision,payload::text,canonical_payload_sha256,actor_subject,created_at),',' ORDER BY revision) FROM supervisor.station_profile_revisions WHERE workstation_id=$1`, stationA).Scan(&s); e != nil {
			t.Fatal(e)
		}
		return s
	}
	before := history()
	widened := quality
	widened.Fields = append([]workstation.ProfileFieldV1{{Name: "gauge_id", Label: "Gauge ID", Kind: "text", Required: true}}, quality.Fields...)
	a = decode(f.call(f.owner, "POST", revisions(stationA), "profile-a2", `"1"`, f.encode(widened)), 201)
	if a.CurrentRevision != 2 || a.Revisions[0].State != workstation.JobOrderSuperseded || a.Revisions[1].State != workstation.JobOrderCurrent || a.Revisions[1].SubmittedBy.Subject != f.owner.subject ||
		len(a.Revisions[1].Profile.Fields) != 3 {
		t.Fatalf("revised profile: %+v", a)
	}
	if after := history(); !strings.HasPrefix(after, before+",") {
		t.Fatalf("profile history changed:\nbefore %s\nafter  %s", before, after)
	}
	f.expect(f.call(f.owner, "POST", revisions(stationA), "stale", `"1"`, body), 412, "REVISION_CONFLICT")
	// A replay returns the revision its request created and today's history, which labels that
	// revision superseded.
	if r := decode(f.call(f.lead, "POST", revisions(stationA), "profile-a", `"0"`, body), 201); !r.Replayed || r.ReplayedRevision != 1 || r.CurrentRevision != 2 ||
		r.Revisions[0].State != workstation.JobOrderSuperseded || r.Revisions[1].State != workstation.JobOrderCurrent {
		t.Fatalf("replay after revise: %+v", r)
	}

	// Tenant isolation: another tenant sees neither the site nor the workstation's profile.
	f.expect(f.call(f.outsider, "GET", stations+"/"+stationA+"/profile", "", "", ""), 404, "NOT_FOUND")
	f.expect(f.call(f.outsider, "GET", "/v1/sites/"+f.otherSite+"/workstations/"+stationA+"/profile", "", "", ""), 404, "NOT_FOUND")
	f.expect(f.call(f.outsider, "POST", "/v1/sites/"+f.otherSite+"/workstations/"+stationA+"/profile-revisions", "x", `"1"`, f.encode(widened)), 412, "REVISION_CONFLICT")
	f.expect(f.call(f.outsider, "POST", previews(stationA), "", "", `{"profile":`+body+`}`), 404, "NOT_FOUND")
	f.expect(f.call(f.lead, "GET", stations+"/"+uuid.NewString()+"/profile", "", "", ""), 404, "NOT_FOUND")

	// A same-tenant site can reuse a workstation UUID. Listing this site must join each
	// summary to its own site's current profile, never the other site's profile.
	otherSiteInTenant := uuid.NewString()
	otherLine := uuid.NewString()
	f.exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'same tenant other site')`, f.tenant, otherSiteInTenant)
	f.exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'owner')`, f.tenant, otherSiteInTenant, f.issuer, f.owner.subject)
	otherSiteProfile := production
	otherSiteProfile.WorkstationID = stationA
	otherSiteProfile.DisplayName, otherSiteProfile.LineID = "Other site's private profile", otherLine
	f.expect(f.call(f.owner, "POST", "/v1/sites/"+otherSiteInTenant+"/workstations/"+stationA+"/profile-revisions", "other-site-profile", `"0"`, f.encode(otherSiteProfile)), 201, "")
	otherSiteProfile.DisplayName = "Other site's private profile revision 2"
	f.expect(f.call(f.owner, "POST", "/v1/sites/"+otherSiteInTenant+"/workstations/"+stationA+"/profile-revisions", "other-site-profile-2", `"1"`, f.encode(otherSiteProfile)), 201, "")
	otherSiteStationB := stationB
	otherSiteStationBProfile := production
	otherSiteStationBProfile.WorkstationID = otherSiteStationB
	otherSiteStationBProfile.DisplayName, otherSiteStationBProfile.LineID = "Other site's station B", otherLine
	f.expect(f.call(f.owner, "POST", "/v1/sites/"+otherSiteInTenant+"/workstations/"+otherSiteStationB+"/profile-revisions", "other-site-b", `"0"`, f.encode(otherSiteStationBProfile)), 201, "")
	otherSiteStationBProfile.DisplayName = "Other site's station B revision 2"
	f.expect(f.call(f.owner, "POST", "/v1/sites/"+otherSiteInTenant+"/workstations/"+otherSiteStationB+"/profile-revisions", "other-site-b-2", `"1"`, f.encode(otherSiteStationBProfile)), 201, "")
	f.expect(f.call(f.operator, "GET", "/v1/sites/"+otherSiteInTenant+"/workstations/"+stationA+"/profile", "", "", ""), 404, "NOT_FOUND")

	type listedProfile struct {
		WorkstationID   string `json:"workstation_id"`
		CurrentRevision int64  `json:"current_revision"`
		DisplayName     string `json:"display_name"`
		LineID          string `json:"line_id"`
	}
	listSite := func(actor stationSession, siteID string) []listedProfile {
		t.Helper()
		response := f.call(actor, "GET", "/v1/sites/"+siteID+"/workstations?limit=100", "", "", "")
		f.expect(response, 200, "")
		var page struct {
			Workstations []listedProfile `json:"workstations"`
			NextCursor   string          `json:"next_cursor"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		if page.NextCursor != "" {
			t.Fatalf("unexpected extra workstation page: %q", page.NextCursor)
		}
		return page.Workstations
	}
	assertSiteRows := func(site string, got []listedProfile, want map[string]listedProfile) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("site %s list returned %d rows, want exactly %d: %+v", site, len(got), len(want), got)
		}
		seen := make(map[string]bool, len(got))
		for _, row := range got {
			if expected, ok := want[row.WorkstationID]; !ok || row != expected || seen[row.WorkstationID] {
				t.Fatalf("site %s list row %+v, want only its own profile %+v", site, row, expected)
			}
			seen[row.WorkstationID] = true
		}
	}
	assertSiteRows(f.site, listSite(f.operator, f.site), map[string]listedProfile{
		stationA: {WorkstationID: stationA, CurrentRevision: 2, DisplayName: "Bore gauge 2", LineID: line},
		stationB: {WorkstationID: stationB, CurrentRevision: 1, DisplayName: "Press 4", LineID: line},
	})
	assertSiteRows(otherSiteInTenant, listSite(f.owner, otherSiteInTenant), map[string]listedProfile{
		stationA:          {WorkstationID: stationA, CurrentRevision: 2, DisplayName: "Other site's private profile revision 2", LineID: otherLine},
		otherSiteStationB: {WorkstationID: otherSiteStationB, CurrentRevision: 2, DisplayName: "Other site's station B revision 2", LineID: otherLine},
	})

	// WIS-278: list summaries include scoped profile names and lines without N+1 reads.
	var page struct {
		Workstations []struct {
			WorkstationID   string `json:"workstation_id"`
			CurrentRevision int64  `json:"current_revision"`
			DisplayName     string `json:"display_name"`
			LineID          string `json:"line_id"`
		} `json:"workstations"`
		NextCursor string `json:"next_cursor"`
	}
	list := f.call(f.operator, "GET", stations+"?limit=1", "", "", "")
	f.expect(list, 200, "")
	if err := json.Unmarshal(list.Body.Bytes(), &page); err != nil || len(page.Workstations) != 1 || page.NextCursor == "" {
		t.Fatalf("page: %s", list.Body.String())
	}
	want := map[string]struct {
		name     string
		line     string
		revision int64
	}{stationA: {name: "Bore gauge 2", line: line, revision: 2}, stationB: {name: "Press 4", line: line, revision: 1}}
	checkSummary := func(summary struct {
		WorkstationID   string `json:"workstation_id"`
		CurrentRevision int64  `json:"current_revision"`
		DisplayName     string `json:"display_name"`
		LineID          string `json:"line_id"`
	}) {
		t.Helper()
		w, ok := want[summary.WorkstationID]
		if !ok || summary.DisplayName != w.name || summary.LineID != w.line || summary.CurrentRevision != w.revision {
			t.Fatalf("station summary %+v, want profile name/line/revision %+v", summary, w)
		}
	}
	checkSummary(page.Workstations[0])
	second := f.call(f.operator, "GET", stations+"?limit=1&cursor="+page.NextCursor, "", "", "")
	f.expect(second, 200, "")
	page.Workstations, page.NextCursor = nil, ""
	if err := json.Unmarshal(second.Body.Bytes(), &page); err != nil || len(page.Workstations) != 1 || page.NextCursor != "" {
		t.Fatalf("second page: %s", second.Body.String())
	}
	checkSummary(page.Workstations[0])
	f.expect(f.call(f.operator, "GET", stations+"?limit=101", "", "", ""), 422, "INVALID_REQUEST")

	// Preview resolves the operator form without persisting anything.
	preview := func(actor stationSession, station string, in workstation.StationProfilePreviewInputV1) workstation.StationProfilePreviewV1 {
		t.Helper()
		w := f.call(actor, "POST", previews(station), "", "", f.encode(in))
		f.expect(w, 200, "")
		var out workstation.StationProfilePreviewV1
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	revisionsBefore := history()
	alone := preview(f.operator, stationA, workstation.StationProfilePreviewInputV1{Profile: widened})
	if !alone.Ready || alone.ProfileDigest != a.Revisions[1].CanonicalPayloadSHA256 || len(alone.Fields) != 3 || alone.Fields[0].Field.Name != "gauge_id" || alone.Fields[0].Source != workstation.SourceStationProfile ||
		len(alone.Conflicts) != 0 || len(alone.Instructions) != 1 || !alone.Instructions[0].Available {
		t.Fatalf("profile preview: %+v", alone)
	}
	// The resolved form is what slice 4 validates operator entries against.
	if v := workstation.ValidateEntry(alone.Fields, []workstation.EntryValueV1{{Field: "gauge_id", Value: "G-7"}, {Field: "bore_diameter", Value: "10.1", Unit: "mm"}, {Field: "disposition", Value: "release"}}); !reflect.DeepEqual(v,
		[]workstation.EntryViolationV1{{Path: "values[1]", Code: workstation.EntryOutOfRange}, {Path: "values[2]", Code: workstation.EntryChoiceNotAllowed}}) {
		t.Fatalf("entry violations: %+v", v)
	}
	// A job order merges only when an approved shift plan assigns that exact revision here.
	order, orderA := f.jobOrder(f.lead, orderBody(source, sha, 100)), f.jobOrder(f.lead, orderBody(source, sha, 40))
	planBody := workstation.ShiftPlanBodyV1{SchemaVersion: 1, LineID: line, StartsAt: "2026-10-21T06:00:00Z", EndsAt: "2026-10-21T14:00:00Z", Timezone: "UTC",
		Assignments: []workstation.AssignmentV1{pin(stationB, order, 100), pin(stationA, orderA, 40)}}
	planned := f.call(f.lead, "POST", "/v1/sites/"+f.site+"/shift-plans", "plan", "", f.encode(planBody))
	f.expect(planned, 201, "")
	var plan workstation.ShiftPlanV1
	if err := json.Unmarshal(planned.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	withOrder := func(profile workstation.StationProfileBodyV1, id string, revision int64) string {
		return f.encode(workstation.StationProfilePreviewInputV1{Profile: profile, JobOrderID: id, JobOrderRevision: revision})
	}
	f.expect(f.call(f.lead, "POST", previews(stationB), "", "", withOrder(production, order.ID, 1)), 409, "JOB_ORDER_NOT_ASSIGNED")
	f.expect(f.call(f.approver, "POST", "/v1/sites/"+f.site+"/shift-plans/"+plan.ID+"/approvals", "approve", tag(plan.StateRevision),
		f.encode(workstation.ShiftPlanApproveInputV1{CanonicalPayloadSHA256: plan.Revisions[0].CanonicalPayloadSHA256})), 200, "")
	f.expect(f.call(f.lead, "POST", previews(stationA), "", "", withOrder(widened, order.ID, 1)), 409, "JOB_ORDER_NOT_ASSIGNED")
	// Merged with an assigned job order: its required fields are added, never dropped.
	merged := preview(f.lead, stationB, workstation.StationProfilePreviewInputV1{Profile: production, JobOrderID: order.ID, JobOrderRevision: 1})
	if !merged.Ready || merged.JobOrderID != order.ID || len(merged.Fields) != 3 || merged.Fields[0].Source != workstation.SourceBoth || merged.Fields[1].Source != workstation.SourceBoth ||
		merged.Fields[2].Source != workstation.SourceStationProfile || len(merged.Instructions) != 1 || merged.Instructions[0].Source != workstation.SourceJobOrder {
		t.Fatalf("merged preview: %+v", merged)
	}
	// A quality screen cannot silently host a production order: conflicts are reported, not reconciled.
	conflicted := preview(f.lead, stationA, workstation.StationProfilePreviewInputV1{Profile: widened, JobOrderID: orderA.ID, JobOrderRevision: 1})
	if conflicted.Ready || !reflect.DeepEqual(conflicted.Conflicts, []string{"template"}) || len(conflicted.Fields) != 5 ||
		conflicted.Fields[3].Source != workstation.SourceJobOrder || !conflicted.Fields[3].Field.Required {
		t.Fatalf("conflicted preview: %+v", conflicted)
	}
	changed := orderBody(source, sha, 90)
	f.expect(f.call(f.lead, "POST", "/v1/sites/"+f.site+"/job-orders/"+order.ID+"/revisions", "o2", `"1"`, f.encode(changed)), 201, "")
	// Revision 2 is current but the approved plan still pins revision 1.
	f.expect(f.call(f.lead, "POST", previews(stationB), "", "", withOrder(production, order.ID, 2)), 409, "JOB_ORDER_NOT_ASSIGNED")
	f.expect(f.call(f.lead, "POST", previews(stationB), "", "", f.encode(workstation.StationProfilePreviewInputV1{Profile: production, JobOrderID: order.ID, JobOrderRevision: 1})), 409, "JOB_ORDER_NOT_CURRENT")
	f.expect(f.call(f.lead, "POST", previews(stationB), "", "", f.encode(workstation.StationProfilePreviewInputV1{Profile: production, JobOrderID: order.ID, JobOrderRevision: 3})), 422, "JOB_ORDER_REVISION_UNKNOWN")
	f.expect(f.call(f.lead, "POST", previews(stationB), "", "", f.encode(workstation.StationProfilePreviewInputV1{Profile: production, JobOrderID: uuid.NewString(), JobOrderRevision: 1})), 422, "JOB_ORDER_REVISION_UNKNOWN")
	rejected("preview of another station", f.call(f.lead, "POST", previews(stationA), "", "", f.encode(workstation.StationProfilePreviewInputV1{Profile: production})), "INVALID_STATION_PROFILE", "profile.workstation_id")
	rejected("preview script", f.call(f.lead, "POST", previews(stationA), "", "", `{"profile":`+strings.Replace(body, `"template":"quality"`, `"template":"quality","html":"<b>"`, 1)+`}`), "UNSUPPORTED_STATION_CONFIGURATION", "profile.*")
	// Rule 06: a withdrawn instruction is reported unavailable and the form is not ready.
	f.expect(f.call(f.owner, "POST", "/v1/sites/"+f.site+"/sources/"+source+"/currentness-decisions", uuid.NewString(), `"1"`, `{"source_revision":1,"expected_source_state_revision":1,"authority_decision_id":"`+designation+`","authority_revision":1,"verdict":"withdraw","reason":"Withdraw for review"}`), 200, "")
	if w := preview(f.lead, stationA, workstation.StationProfilePreviewInputV1{Profile: widened}); w.Ready || w.Instructions[0].Available || w.Instructions[0].Reason != workstation.InstructionNotConfirmed {
		t.Fatalf("withdrawn instruction preview: %+v", w)
	}
	if after := history(); after != revisionsBefore {
		t.Fatal("preview changed stored profiles")
	}
	var previewed int
	if err := f.control.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.station_profile_audit WHERE event_type='station_profile.previewed' AND workstation_id=$1 AND revision IS NULL`, stationA).Scan(&previewed); err != nil || previewed < 3 {
		t.Fatalf("preview audit rows=%d err=%v", previewed, err)
	}

	// The database refuses the same bypasses for the runtime role.
	f.refused(f.tenant, "rewrote profile history", "42501", "permission denied", `UPDATE supervisor.station_profile_revisions SET actor_subject='x' WHERE workstation_id=$1`, stationA)
	f.refused(f.tenant, "deleted a profile", "42501", "permission denied", `DELETE FROM supervisor.station_profile_heads WHERE workstation_id=$1`, stationA)
	f.refused(f.tenant, "rewound a profile head", "23514", "Invalid station profile head transition", `UPDATE supervisor.station_profile_heads SET current_revision=1 WHERE workstation_id=$1`, stationA)
	f.refused(f.tenant, "skipped a revision", "23514", "Station profile revision is not the head revision",
		`INSERT INTO supervisor.station_profile_revisions(tenant_id,site_id,workstation_id,revision,payload,canonical_payload,canonical_payload_sha256,digest_schema_version,actor_issuer,actor_subject) SELECT tenant_id,site_id,workstation_id,5,payload,canonical_payload,canonical_payload_sha256,1,'i','s' FROM supervisor.station_profile_revisions WHERE workstation_id=$1 AND revision=1`, stationA)
	f.refused(f.otherTenant, "wrote into another tenant", "42501", "Station profile tenant scope denied", `INSERT INTO supervisor.station_profile_heads(tenant_id,site_id,workstation_id,current_revision) VALUES($1,$2,$3,1)`, f.tenant, f.site, uuid.NewString())
	rtx, err := f.runtime.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = rtx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, f.otherTenant); err != nil {
		t.Fatal(err)
	}
	var visible int
	if err = rtx.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM supervisor.station_profile_heads)+(SELECT count(*) FROM supervisor.station_profile_revisions)+(SELECT count(*) FROM supervisor.station_profile_receipts)+(SELECT count(*) FROM supervisor.station_profile_audit)`).Scan(&visible); err != nil {
		t.Fatal(err)
	}
	_ = rtx.Rollback()
	if visible != 0 {
		t.Fatalf("other tenant sees %d station profile rows", visible)
	}
}

// TestStationProfileConcurrentCreateHTTPPostgres: two concurrent first writes (If-Match "0")
// for one workstation yield exactly one 201 and one 412 REVISION_CONFLICT, never a 500 from a
// unique violation; two concurrent requests with one Idempotency-Key yield one revision.
func TestStationProfileConcurrentCreateHTTPPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	profile := func(station, name string) string {
		return f.encode(workstation.StationProfileBodyV1{SchemaVersion: 1, WorkstationID: station, LineID: uuid.NewString(), DisplayName: name, Template: "production",
			Fields: []workstation.ProfileFieldV1{{Name: "good_units", Label: "Good units", Kind: "decimal", Required: true, Unit: "count"}}})
	}
	race := func(station string, keys, bodies [2]string, actors [2]stationSession) [2]*httptest.ResponseRecorder {
		var out [2]*httptest.ResponseRecorder
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := range out {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				out[i] = f.call(actors[i], "POST", "/v1/sites/"+f.site+"/workstations/"+station+"/profile-revisions", keys[i], `"0"`, bodies[i])
			}(i)
		}
		close(start)
		wg.Wait()
		return out
	}
	for round := 0; round < 5; round++ {
		station := uuid.NewString()
		got := race(station, [2]string{uuid.NewString(), uuid.NewString()}, [2]string{profile(station, "Press A"), profile(station, "Press B")}, [2]stationSession{f.lead, f.owner})
		codes := map[int]int{got[0].Code: 1}
		codes[got[1].Code]++
		if codes[201] != 1 || codes[412] != 1 {
			t.Fatalf("round %d: concurrent creates gave %d %s / %d %s", round, got[0].Code, got[0].Body.String(), got[1].Code, got[1].Body.String())
		}
		for _, w := range got {
			if w.Code == 412 && !strings.Contains(w.Body.String(), `"code":"REVISION_CONFLICT"`) {
				t.Fatalf("round %d: %s", round, w.Body.String())
			}
		}
		// Same actor, same key, same body: one revision, the other request replays it.
		same := uuid.NewString()
		body := profile(same, "Press C")
		dup := race(same, [2]string{"dup-" + same, "dup-" + same}, [2]string{body, body}, [2]stationSession{f.lead, f.lead})
		replayed := 0
		for _, w := range dup {
			f.expect(w, 201, "")
			var p workstation.StationProfileV1
			if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil || p.CurrentRevision != 1 || len(p.Revisions) != 1 {
				t.Fatalf("round %d duplicate: %s", round, w.Body.String())
			}
			if p.Replayed {
				replayed++
				if p.ReplayedRevision != 1 {
					t.Fatalf("round %d replayed revision %d", round, p.ReplayedRevision)
				}
			}
		}
		if replayed != 1 {
			t.Fatalf("round %d: %d replays, want 1", round, replayed)
		}
	}
	var heads, revisions int
	if err := f.control.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM supervisor.station_profile_heads WHERE tenant_id=$1),(SELECT count(*) FROM supervisor.station_profile_revisions WHERE tenant_id=$1)`, f.tenant).Scan(&heads, &revisions); err != nil || heads != 10 || revisions != 10 {
		t.Fatalf("heads=%d revisions=%d err=%v", heads, revisions, err)
	}
}

func TestStationProfileListDoesNot500OnMissingOptionalFields(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	station, line := uuid.NewString(), uuid.NewString()
	profile := workstation.StationProfileBodyV1{
		SchemaVersion: 1, WorkstationID: station, LineID: line, DisplayName: "Press 4", Template: "production",
		Fields: []workstation.ProfileFieldV1{{Name: "good_units", Label: "Good units", Kind: "decimal", Required: true, Unit: "count"}},
	}
	response := f.call(f.lead, "POST", "/v1/sites/"+f.site+"/workstations/"+station+"/profile-revisions", "create-null-safe-profile", `"0"`, f.encode(profile))
	f.expect(response, 201, "")
	if _, err := f.control.ExecContext(ctx, `ALTER TABLE supervisor.station_profile_revisions DISABLE TRIGGER station_profile_revisions_immutable`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := f.control.ExecContext(context.Background(), `ALTER TABLE supervisor.station_profile_revisions ENABLE TRIGGER station_profile_revisions_immutable`); err != nil {
			t.Errorf("restore immutable profile trigger: %v", err)
		}
	})
	_, err := f.control.ExecContext(ctx, `WITH changed AS (
 SELECT tenant_id,site_id,workstation_id,revision,payload-'display_name'-'line_id' AS payload,
        (canonical_payload::jsonb || jsonb_build_object('body',payload-'display_name'-'line_id'))::text AS canonical_payload
 FROM supervisor.station_profile_revisions WHERE tenant_id=$1 AND site_id=$2 AND workstation_id=$3 AND revision=1
)
UPDATE supervisor.station_profile_revisions r SET payload=c.payload,canonical_payload=c.canonical_payload,
 canonical_payload_sha256=encode(sha256(convert_to(c.canonical_payload,'UTF8')),'hex')
FROM changed c WHERE (r.tenant_id,r.site_id,r.workstation_id,r.revision)=(c.tenant_id,c.site_id,c.workstation_id,c.revision)`, f.tenant, f.site, station)
	if err != nil {
		t.Fatal(err)
	}
	response = f.call(f.operator, "GET", "/v1/sites/"+f.site+"/workstations?limit=50", "", "", "")
	f.expect(response, 200, "")
	var page struct {
		Workstations []map[string]json.RawMessage `json:"workstations"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil || len(page.Workstations) != 1 {
		t.Fatalf("null-safe workstation page %s: %v", response.Body.String(), err)
	}
	for _, field := range []string{"display_name", "line_id"} {
		if _, ok := page.Workstations[0][field]; ok {
			t.Errorf("empty optional %s was not omitted: %s", field, response.Body.String())
		}
	}
}
