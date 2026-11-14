//go:build integration

package httpapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/workstation"
	"github.com/google/uuid"
)

func pointer[T any](v T) *T { return &v }

// recordingStation is one configured workstation with an approved shift plan delivering one
// job order. Approvals carry the database clock, and a record binds to the approval in force
// at its event time, so windows and event times are derived from the database clock.
type recordingStation struct {
	station, source, sha, designation, line string
	order                                   workstation.JobOrderV1
	plan                                    workstation.ShiftPlanV1
	starts, ends                            time.Time
}

// clock reads the database clock, truncated so it round-trips through RFC 3339 exactly.
func (f *stationFixture) clock() time.Time {
	f.t.Helper()
	var now time.Time
	if err := f.control.QueryRowContext(f.ctx, `SELECT date_trunc('milliseconds',clock_timestamp())`).Scan(&now); err != nil {
		f.t.Fatal(err)
	}
	return now.UTC()
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func (f *stationFixture) recordingStation(starts, ends time.Time) recordingStation {
	f.t.Helper()
	s := f.plannedStation(starts, ends)
	f.startRun(f.lead, s.plan.ID)
	return s
}

// plannedStation is recordingStation before its shift run is started: records are refused.
func (f *stationFixture) plannedStation(starts, ends time.Time) recordingStation {
	f.t.Helper()
	s := recordingStation{starts: starts, ends: ends, line: uuid.NewString(), station: uuid.NewString()}
	s.source, s.sha, s.designation = f.instruction("Synthetic SOP: press and gauge bore", true)
	profile := workstation.StationProfileBodyV1{SchemaVersion: 1, WorkstationID: s.station, LineID: s.line, DisplayName: "Press 4", Template: "production",
		Fields: []workstation.ProfileFieldV1{
			{Name: "good_units", Label: "Good units", Kind: "decimal", Required: true, Unit: "count"},
			{Name: "cycle_time", Label: "Cycle time", Kind: "decimal", Required: true, Unit: "s"},
			{Name: "bore", Label: "Bore", Kind: "decimal", Unit: "mm", Min: "9.95", Max: "10.05"},
			{Name: "surface", Label: "Surface", Kind: "choice", Choices: []string{"ok", "scratched"}},
		}}
	f.expect(f.call(f.lead, "POST", "/v1/sites/"+f.site+"/workstations/"+s.station+"/profile-revisions", uuid.NewString(), `"0"`, f.encode(profile)), 201, "")
	s.order = f.jobOrder(f.lead, orderBody(s.source, s.sha, 100))
	s.plan = f.approvedPlan(s.line, s.station, s.order, starts, ends, 100)
	return s
}

// approvedPlan creates and approves a shift plan pinning the job order's current revision.
func (f *stationFixture) approvedPlan(line, station string, order workstation.JobOrderV1, starts, ends time.Time, quantity int64) workstation.ShiftPlanV1 {
	f.t.Helper()
	w := f.call(f.lead, "POST", "/v1/sites/"+f.site+"/shift-plans", uuid.NewString(), "", f.encode(workstation.ShiftPlanBodyV1{SchemaVersion: 1, LineID: line,
		StartsAt: stamp(starts), EndsAt: stamp(ends), Timezone: "UTC", Assignments: []workstation.AssignmentV1{pin(station, order, quantity)}}))
	f.expect(w, 201, "")
	var plan workstation.ShiftPlanV1
	if err := json.Unmarshal(w.Body.Bytes(), &plan); err != nil {
		f.t.Fatal(err)
	}
	f.expect(f.call(f.approver, "POST", "/v1/sites/"+f.site+"/shift-plans/"+plan.ID+"/approvals", uuid.NewString(), `"1"`,
		f.encode(workstation.ShiftPlanApproveInputV1{CanonicalPayloadSHA256: plan.Revisions[0].CanonicalPayloadSHA256})), 200, "")
	return plan
}

func (s recordingStation) entry(kind workstation.RecordKind, at string) workstation.StationRecordBodyV1 {
	return workstation.StationRecordBodyV1{SchemaVersion: 1, Kind: kind, WorkstationID: s.station, JobOrderID: s.order.ID, JobOrderRevision: 1, EventAt: at}
}

func (f *stationFixture) authorize(actor stationSession, station string, grantee stationSession, action workstation.AuthorizationAction, revision string) *httptest.ResponseRecorder {
	return f.call(actor, "POST", "/v1/sites/"+f.site+"/workstations/"+station+"/authorizations", uuid.NewString(), revision,
		f.encode(workstation.StationAuthorizationInputV1{GranteeIssuer: f.issuer, GranteeSubject: grantee.subject, Action: action}))
}

func (f *stationFixture) decodeRecord(w *httptest.ResponseRecorder, status int) workstation.StationRecordV1 {
	f.t.Helper()
	f.expect(w, status, "")
	var r workstation.StationRecordV1
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		f.t.Fatal(err)
	}
	return r
}

// TestStationRecordsHTTPPostgres proves WIS-520 acceptance item 4 (and the duplicate
// submission, corrected cycle-time entry and unauthorized operator cases of item 9) against
// PostgreSQL: an authorized operator records the station job's lifecycle, quantities, cycle
// times, quality and material records and completion evidence, each attributed with event
// and received time, plan, profile and job-order revision and manual provenance; a quality
// record never releases material; corrections append and never branch.
func TestStationRecordsHTTPPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	now0 := f.clock()
	s := f.recordingStation(now0.Add(-time.Hour), now0.Add(3*time.Minute))
	// Event times run from just after the approval, one second per shop-floor minute of the
	// scenario, and stay inside the window and the five-minute clock-skew bound.
	testStart := f.clock()
	at := func(minute int) string { return stamp(testStart.Add(time.Duration(minute+10) * time.Second)) }
	records := "/v1/sites/" + f.site + "/workstations/" + s.station + "/records"
	submit := func(actor stationSession, key string, body workstation.StationRecordBodyV1) *httptest.ResponseRecorder {
		return f.call(actor, "POST", records, key, "", f.encode(body))
	}
	ok := func(key string, body workstation.StationRecordBodyV1) workstation.StationRecordV1 {
		t.Helper()
		return f.decodeRecord(submit(f.operator, key, body), 201)
	}
	rejected := func(name string, w *httptest.ResponseRecorder, status int, code, path string) {
		t.Helper()
		var prob map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &prob)
		if w.Code != status || prob["code"] != code || (path != "" && prob["path"] != path) {
			t.Fatalf("%s: HTTP %d %s, want %d %s at %q", name, w.Code, w.Body.String(), status, code, path)
		}
	}
	ack := s.entry(workstation.KindAcknowledged, testStart.Add(5*time.Second).In(time.FixedZone("CEST", 7200)).Format(time.RFC3339Nano))

	// Station authorization: explicit, attributed, managed by owners and supervisors only.
	f.expect(submit(f.operator, "ack", ack), 403, "STATION_NOT_AUTHORIZED")
	f.expect(f.authorize(f.operator, s.station, f.operator, workstation.AuthorizationGrant, `"0"`), 403, "PERMISSION_DENIED")
	f.expect(f.authorize(f.lead, s.station, f.lead, workstation.AuthorizationGrant, `"0"`), 403, "SELF_AUTHORIZATION_FORBIDDEN")
	f.expect(f.authorize(f.lead, s.station, f.viewer, workstation.AuthorizationGrant, `"0"`), 422, "GRANTEE_NOT_ELIGIBLE")
	f.expect(f.authorize(f.lead, s.station, f.outsider, workstation.AuthorizationGrant, `"0"`), 422, "GRANTEE_NOT_ELIGIBLE")
	f.expect(f.authorize(f.lead, uuid.NewString(), f.operator, workstation.AuthorizationGrant, `"0"`), 404, "NOT_FOUND")
	f.expect(f.authorize(f.lead, s.station, f.operator, workstation.AuthorizationRevoke, `"0"`), 409, "STATION_AUTHORIZATION_UNCHANGED")
	grantBody := f.encode(workstation.StationAuthorizationInputV1{GranteeIssuer: f.issuer, GranteeSubject: f.operator.subject, Action: workstation.AuthorizationGrant})
	authorizations := "/v1/sites/" + f.site + "/workstations/" + s.station + "/authorizations"
	f.expect(f.call(f.lead, "POST", authorizations, "grant-1", "", grantBody), 428, "REVISION_REQUIRED")
	granted := f.call(f.lead, "POST", authorizations, "grant-1", `"0"`, grantBody)
	f.expect(granted, 201, "")
	var auth workstation.StationAuthorizationV1
	if err := json.Unmarshal(granted.Body.Bytes(), &auth); err != nil || !auth.Active || auth.CurrentRevision != 1 || auth.Grantee.Subject != f.operator.subject ||
		len(auth.Events) != 1 || auth.Events[0].Actor.Subject != f.lead.subject || granted.Header().Get("ETag") != `"1"` {
		t.Fatalf("grant: %s", granted.Body.String())
	}
	if w := f.call(f.lead, "POST", authorizations, "grant-1", `"0"`, grantBody); w.Code != 201 || !strings.Contains(w.Body.String(), `"replayed":true`) {
		t.Fatalf("grant replay: %d %s", w.Code, w.Body.String())
	}
	f.expect(f.call(f.lead, "POST", authorizations, "grant-1", `"0"`, strings.Replace(grantBody, `"grant"`, `"revoke"`, 1)), 409, "IDEMPOTENCY_CONFLICT")
	f.expect(f.authorize(f.lead, s.station, f.operator, workstation.AuthorizationGrant, `"0"`), 412, "REVISION_CONFLICT")
	f.expect(f.authorize(f.lead, s.station, f.operator, workstation.AuthorizationGrant, `"1"`), 409, "STATION_AUTHORIZATION_UNCHANGED")
	f.expect(f.call(f.lead, "POST", authorizations, uuid.NewString(), `"1"`, strings.Replace(grantBody, `"grant"`, `"approve"`, 1)), 422, "INVALID_STATION_AUTHORIZATION")
	// No role bypasses the grant (rule 05): the owner and an ungranted operator are refused.
	f.expect(submit(f.owner, "owner-ack", ack), 403, "STATION_NOT_AUTHORIZED")
	f.expect(submit(f.spare, "spare-ack", ack), 403, "STATION_NOT_AUTHORIZED")
	f.expect(submit(f.viewer, "viewer-ack", ack), 403, "STATION_NOT_AUTHORIZED")
	f.expect(submit(f.outsider, "outsider-ack", ack), 404, "NOT_FOUND")

	// Lifecycle: acknowledge, then start; entries need a started job.
	quantity := s.entry(workstation.KindQuantity, at(10))
	quantity.GoodQuantity, quantity.RejectQuantity = pointer(int64(40)), pointer(int64(2))
	f.expect(submit(f.operator, "early-qty", quantity), 409, "LIFECYCLE_CONFLICT")
	acknowledged := ok("ack", ack)
	if acknowledged.Record.Kind != workstation.KindAcknowledged || acknowledged.RecordedBy.Subject != f.operator.subject || acknowledged.RecordedBy.Issuer != f.issuer ||
		acknowledged.Source != workstation.SourceManualTablet || acknowledged.PlanID != s.plan.ID || acknowledged.PlanRevision != 1 || acknowledged.ProfileRevision != 1 ||
		acknowledged.Record.JobOrderRevision != 1 || acknowledged.Record.EventAt != stamp(testStart.Add(5*time.Second)) || !acknowledged.EventAt.Equal(testStart.Add(5*time.Second)) ||
		acknowledged.ReceivedAt.Before(testStart) || acknowledged.State != workstation.JobOrderCurrent || len(acknowledged.CanonicalPayloadSHA256) != 64 || acknowledged.Replayed {
		t.Fatalf("acknowledged: %+v", acknowledged)
	}
	f.expect(submit(f.operator, "ack-again", ack), 409, "LIFECYCLE_CONFLICT")
	started := ok("start", s.entry(workstation.KindStarted, at(0)))
	f.expect(submit(f.operator, "resume-running", s.entry(workstation.KindResumed, at(1))), 409, "LIFECYCLE_CONFLICT")
	// Lifecycle follows event time: nothing is started before the start event says so.
	early := s.entry(workstation.KindPaused, at(-3))
	early.Comment = "Out of order"
	f.expect(submit(f.operator, "pause-before-start", early), 409, "LIFECYCLE_CONFLICT")
	earlyQuantity := quantity
	earlyQuantity.EventAt = at(-3)
	f.expect(submit(f.operator, "qty-before-start", earlyQuantity), 409, "LIFECYCLE_CONFLICT")

	// Event time and received time are separate stored columns (rule 04).
	var eventAt, receivedAt time.Time
	if err := f.control.QueryRowContext(ctx, `SELECT event_at,received_at FROM supervisor.station_records WHERE id=$1`, started.ID).Scan(&eventAt, &receivedAt); err != nil ||
		!eventAt.Equal(testStart.Add(10*time.Second)) || !receivedAt.Equal(started.ReceivedAt) || receivedAt.Before(testStart) {
		t.Fatalf("stored times event=%v received=%v err=%v", eventAt, receivedAt, err)
	}

	// Duplicate submission: same key and body replays the original; another body conflicts.
	q1 := ok("qty-1", quantity)
	replay := f.decodeRecord(submit(f.operator, "qty-1", quantity), 201)
	if !replay.Replayed || replay.ID != q1.ID || !replay.ReceivedAt.Equal(q1.ReceivedAt) {
		t.Fatalf("replay: %+v vs %+v", replay, q1)
	}
	different := quantity
	different.GoodQuantity = pointer(int64(41))
	f.expect(submit(f.operator, "qty-1", different), 409, "IDEMPOTENCY_CONFLICT")
	var quantities int
	if err := f.control.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.station_records WHERE kind='quantity'`).Scan(&quantities); err != nil || quantities != 1 {
		t.Fatalf("quantity rows %d (%v)", quantities, err)
	}

	// Revocation applies to the very next request, replays included (rule 07 recheck).
	f.expect(f.authorize(f.lead, s.station, f.operator, workstation.AuthorizationRevoke, `"1"`), 201, "")
	f.expect(submit(f.operator, "qty-1", quantity), 403, "STATION_NOT_AUTHORIZED")
	f.expect(submit(f.operator, "after-revoke", s.entry(workstation.KindResumed, at(20))), 403, "STATION_NOT_AUTHORIZED")
	regrant := f.authorize(f.owner, s.station, f.operator, workstation.AuthorizationGrant, `"2"`)
	f.expect(regrant, 201, "")
	if err := json.Unmarshal(regrant.Body.Bytes(), &auth); err != nil || !auth.Active || auth.CurrentRevision != 3 || len(auth.Events) != 3 || auth.Events[1].Action != workstation.AuthorizationRevoke {
		t.Fatalf("regrant: %s", regrant.Body.String())
	}

	// Cycle time, quality and material records against the resolved station form.
	cycle := s.entry(workstation.KindCycleTime, at(30))
	cycle.CycleTimeSeconds = "2.75"
	c1 := ok("cycle-1", cycle)
	measure := s.entry(workstation.KindQualityMeasurement, at(35))
	measure.Values = []workstation.EntryValueV1{{Field: "bore", Value: "10.01", Unit: "mm"}, {Field: "surface", Value: "scratched"}}
	ok("measure", measure)
	for name, tc := range map[string]struct {
		values     []workstation.EntryValueV1
		code, path string
	}{
		"out of range":   {[]workstation.EntryValueV1{{Field: "bore", Value: "10.1", Unit: "mm"}}, workstation.EntryOutOfRange, "values[0]"},
		"wrong unit":     {[]workstation.EntryValueV1{{Field: "bore", Value: "10", Unit: "in"}}, workstation.EntryWrongUnit, "values[0]"},
		"unknown field":  {[]workstation.EntryValueV1{{Field: "bore", Value: "10", Unit: "mm"}, {Field: "torque", Value: "25"}}, workstation.EntryUnknownField, "values[1]"},
		"choice":         {[]workstation.EntryValueV1{{Field: "surface", Value: "polished"}}, workstation.EntryChoiceNotAllowed, "values[0]"},
		"float decimal":  {[]workstation.EntryValueV1{{Field: "bore", Value: "1e1", Unit: "mm"}}, workstation.EntryInvalidDecimal, "values[0]"},
		"repeated value": {[]workstation.EntryValueV1{{Field: "bore", Value: "10", Unit: "mm"}, {Field: "bore", Value: "10", Unit: "mm"}}, workstation.EntryDuplicate, "values[1]"},
	} {
		bad := measure
		bad.Values = tc.values
		rejected(name, submit(f.operator, "bad-"+name, bad), 422, tc.code, tc.path)
	}
	hold := s.entry(workstation.KindQualityDisposition, at(40))
	hold.Disposition, hold.AuthorizesRelease, hold.Comment = workstation.DispositionHold, pointer(true), "Hold tag on bin 3"
	rejected("release claim", submit(f.operator, "release", hold), 422, "QUALITY_RECORD_CANNOT_RELEASE", "authorizes_release")
	rejected("release member omitted", f.call(f.operator, "POST", records, "no-flag", "", strings.Replace(f.encode(hold), `"authorizes_release":true,`, ``, 1)), 422, "INVALID_STATION_RECORD", "authorizes_release")
	rejected("source in body", f.call(f.operator, "POST", records, "src", "", strings.Replace(f.encode(ack), `"schema_version":1`, `"schema_version":1,"source":"mes_adapter"`, 1)), 422, "INVALID_STATION_RECORD", "")
	hold.AuthorizesRelease = pointer(false)
	held := ok("hold", hold)
	if held.Record.Disposition != workstation.DispositionHold || held.Record.AuthorizesRelease == nil || *held.Record.AuthorizesRelease {
		t.Fatalf("held: %+v", held.Record)
	}
	material := s.entry(workstation.KindMaterialException, at(45))
	material.MaterialCode, material.MaterialIssue = "COIL-7", workstation.MaterialShortage
	ok("material", material)
	pause := s.entry(workstation.KindPaused, at(50))
	pause.Comment = "Coil change"
	ok("pause", pause)
	ok("resume", s.entry(workstation.KindResumed, at(60)))
	block := s.entry(workstation.KindBlocked, at(70))
	block.Comment = "Waiting for coil"
	ok("block", block)
	complete := s.entry(workstation.KindCompletionEvidence, at(120))
	complete.Values = []workstation.EntryValueV1{{Field: "good_units", Value: "40", Unit: "count"}, {Field: "cycle_time", Value: "2.65", Unit: "s"}}
	f.expect(submit(f.operator, "complete-blocked", complete), 409, "LIFECYCLE_CONFLICT")
	ok("resume-2", s.entry(workstation.KindResumed, at(80)))

	// The submission targets the job order in force at the event time.
	wrong := s.entry(workstation.KindNote, at(85))
	wrong.Comment = "check"
	wrong.JobOrderRevision = 2
	f.expect(submit(f.operator, "wrong-revision", wrong), 409, "JOB_ORDER_NOT_CURRENT")
	wrong.JobOrderRevision, wrong.JobOrderID = 1, uuid.NewString()
	f.expect(submit(f.operator, "wrong-order", wrong), 409, "JOB_ORDER_NOT_CURRENT")
	wrong.JobOrderID, wrong.EventAt = s.order.ID, stamp(s.ends)
	f.expect(submit(f.operator, "after-shift", wrong), 409, "NO_JOB_ORDER_TO_RECORD")
	wrong.EventAt = stamp(s.starts.Add(-time.Second))
	f.expect(submit(f.operator, "before-shift", wrong), 409, "NO_JOB_ORDER_TO_RECORD")
	wrong.EventAt = stamp(testStart.Add(-8 * 24 * time.Hour))
	rejected("too old", submit(f.operator, "too-old", wrong), 422, "EVENT_TOO_OLD", "event_at")
	wrong.EventAt = "2030-01-01T00:00:00Z"
	rejected("future event", submit(f.operator, "future", wrong), 422, "INVALID_STATION_RECORD", "event_at")
	wrong.WorkstationID = uuid.NewString()
	rejected("other station in body", submit(f.operator, "other-station", wrong), 422, "INVALID_STATION_RECORD", "workstation_id")

	// Corrected cycle-time entry: the correction appends, names its predecessor, never branches.
	history := func() string {
		t.Helper()
		var h string
		if err := f.control.QueryRowContext(ctx, `SELECT string_agg(concat_ws('|',id,kind,payload::text,canonical_payload_sha256,event_at,received_at,actor_subject),',' ORDER BY received_at,id) FROM supervisor.station_records`).Scan(&h); err != nil {
			t.Fatal(err)
		}
		return h
	}
	before := history()
	fix := cycle
	fix.CycleTimeSeconds, fix.CorrectsRecordID = "2.65", c1.ID
	c2 := ok("cycle-fix", fix)
	if c2.Record.CorrectsRecordID != c1.ID || c2.State != workstation.JobOrderCurrent || c2.Record.CycleTimeSeconds != "2.65" {
		t.Fatalf("correction: %+v", c2)
	}
	if after := history(); !strings.HasPrefix(after, before+",") {
		t.Fatalf("record history changed:\nbefore %s\nafter  %s", before, after)
	}
	outside := fix
	outside.CorrectsRecordID, outside.EventAt = c2.ID, stamp(s.starts.Add(-time.Second))
	rejected("correction outside its shift", submit(f.operator, "fix-outside", outside), 422, "EVENT_OUTSIDE_SHIFT", "event_at")
	branch := fix
	branch.CycleTimeSeconds = "2.7"
	f.expect(submit(f.operator, "cycle-branch", branch), 409, "CORRECTION_CONFLICT")
	crossKind := quantity
	crossKind.CorrectsRecordID = c2.ID
	rejected("correct across kinds", submit(f.operator, "cross", crossKind), 422, "INVALID_CORRECTION", "corrects_record_id")
	unknown := fix
	unknown.CorrectsRecordID = uuid.NewString()
	rejected("correct unknown record", submit(f.operator, "unknown-pred", unknown), 422, "INVALID_CORRECTION", "corrects_record_id")
	lifecycleFix := s.entry(workstation.KindStarted, at(0))
	lifecycleFix.CorrectsRecordID = started.ID
	rejected("correct a lifecycle event", submit(f.operator, "fix-start", lifecycleFix), 422, "INVALID_CORRECTION", "corrects_record_id")

	// Completion evidence carries every required form value and closes the station job.
	partial := complete
	partial.Values = complete.Values[:1]
	rejected("incomplete evidence", submit(f.operator, "complete-partial", partial), 422, workstation.EntryMissingRequired, "fields[1]")
	ok("complete", complete)
	// Completion closes the station job for later events in this shift.
	afterCompletion := quantity
	afterCompletion.EventAt = at(125)
	f.expect(submit(f.operator, "qty-after", afterCompletion), 409, "STATION_JOB_COMPLETED")
	// A correction still appends after completion.
	fix2 := fix
	fix2.CycleTimeSeconds, fix2.CorrectsRecordID = "2.6", c2.ID
	c3 := ok("cycle-fix-2", fix2)

	// Readback: each link reports its state and the whole chain, oldest first.
	read := func(id string) workstation.StationRecordDetailV1 {
		t.Helper()
		w := f.call(f.viewer, "GET", records+"/"+id, "", "", "")
		f.expect(w, 200, "")
		var d workstation.StationRecordDetailV1
		if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
			t.Fatal(err)
		}
		return d
	}
	for _, id := range []string{c1.ID, c2.ID, c3.ID} {
		d := read(id)
		if d.Record.ID != id || len(d.Chain) != 3 || d.Chain[0].ID != c1.ID || d.Chain[1].ID != c2.ID || d.Chain[2].ID != c3.ID ||
			d.Chain[0].State != workstation.JobOrderSuperseded || d.Chain[0].CorrectedByRecordID != c2.ID || d.Chain[1].CorrectedByRecordID != c3.ID ||
			d.Chain[2].State != workstation.JobOrderCurrent || d.Chain[2].CorrectedByRecordID != "" || d.Chain[0].Record.CycleTimeSeconds != "2.75" {
			t.Fatalf("chain from %s: %+v", id, d)
		}
	}
	page := func(query string) workstation.StationRecordPageV1 {
		t.Helper()
		w := f.call(f.operator, "GET", records+query, "", "", "")
		f.expect(w, 200, "")
		var p workstation.StationRecordPageV1
		if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	all := page("?job_order_id=" + s.order.ID)
	if len(all.Records) != 14 || all.NextCursor != "" || all.Records[0].ID != acknowledged.ID || all.Records[13].ID != c3.ID {
		t.Fatalf("listed %d records: %+v", len(all.Records), all)
	}
	first := page("?limit=10")
	rest := page("?limit=10&cursor=" + first.NextCursor)
	if len(first.Records) != 10 || len(rest.Records) != 4 || rest.NextCursor != "" || rest.Records[0].ID != all.Records[10].ID {
		t.Fatalf("pages %d+%d", len(first.Records), len(rest.Records))
	}
	if other := page("?job_order_id=" + uuid.NewString()); len(other.Records) != 0 {
		t.Fatalf("other job order: %+v", other)
	}
	f.expect(f.call(f.operator, "GET", records+"?limit=0", "", "", ""), 422, "INVALID_REQUEST")
	f.expect(f.call(f.operator, "GET", "/v1/sites/"+f.site+"/workstations/"+uuid.NewString()+"/records/"+c1.ID, "", "", ""), 404, "NOT_FOUND")
	var listed workstation.StationAuthorizationPageV1
	lw := f.call(f.viewer, "GET", authorizations+"?limit=1", "", "", "")
	f.expect(lw, 200, "")
	if err := json.Unmarshal(lw.Body.Bytes(), &listed); err != nil || len(listed.Authorizations) != 1 || listed.Authorizations[0].CurrentRevision != 3 || listed.NextCursor != "" {
		t.Fatalf("authorizations: %s", lw.Body.String())
	}

	// Degraded delivery refuses entry with the delivery state as the code.
	note := s.entry(workstation.KindNote, at(150))
	note.Comment = "Handover"
	f.expect(f.call(f.owner, "POST", "/v1/sites/"+f.site+"/sources/"+s.source+"/currentness-decisions", uuid.NewString(), `"1"`, `{"source_revision":1,"expected_source_state_revision":1,"authority_decision_id":"`+s.designation+`","authority_revision":1,"verdict":"withdraw","reason":"Withdraw for review"}`), 200, "")
	f.expect(submit(f.operator, "note-withdrawn", note), 409, "INSTRUCTION_UNAVAILABLE")
	var notes int
	if err := f.control.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.station_records WHERE kind='note'`).Scan(&notes); err != nil || notes != 0 {
		t.Fatalf("refused notes stored: %d (%v)", notes, err)
	}

	// Tenant isolation.
	f.expect(f.call(f.outsider, "GET", records+"/"+c1.ID, "", "", ""), 404, "NOT_FOUND")
	f.expect(f.call(f.outsider, "GET", "/v1/sites/"+f.otherSite+"/workstations/"+s.station+"/records/"+c1.ID, "", "", ""), 404, "NOT_FOUND")
	f.expect(f.call(f.outsider, "GET", records, "", "", ""), 404, "NOT_FOUND")
	f.expect(f.call(f.outsider, "GET", authorizations, "", "", ""), 404, "NOT_FOUND")
	f.expect(f.call(f.outsider, "POST", "/v1/sites/"+f.otherSite+"/workstations/"+s.station+"/records", "x", "", f.encode(ack)), 403, "STATION_NOT_AUTHORIZED")

	// The database refuses the same bypasses for the runtime role. forge copies a stored
	// record with a JSON patch applied consistently to payload, envelope and digest, so only
	// the invariant under test can fail.
	forge := `INSERT INTO supervisor.station_records(tenant_id,site_id,id,workstation_id,job_order_id,job_order_revision,plan_id,plan_revision,profile_revision,kind,event_at,actor_issuer,actor_subject,source,late,corrects_record_id,payload,canonical_payload,canonical_payload_sha256,digest_schema_version)
 SELECT r.tenant_id,r.site_id,$3::uuid,r.workstation_id,r.job_order_id,r.job_order_revision,r.plan_id,r.plan_revision,r.profile_revision,r.kind,(x.p->>'event_at')::timestamptz,r.actor_issuer,r.actor_subject,r.source,r.late,(x.p->>'corrects_record_id')::uuid,x.p,x.c,encode(sha256(convert_to(x.c,'UTF8')),'hex'),1
 FROM supervisor.station_records r CROSS JOIN LATERAL (SELECT r.payload||$2::jsonb p) y CROSS JOIN LATERAL (SELECT y.p,jsonb_set(r.canonical_payload::jsonb,'{body}',y.p)::text c) x WHERE r.id=$1`
	f.refused(f.tenant, "released material", "23514", "station_record_never_releases", forge, held.ID, `{"authorizes_release":true}`, uuid.NewString())
	f.refused(f.tenant, "branched a correction chain", "23505", "station_record_single_correction", forge, c3.ID, `{"corrects_record_id":"`+c1.ID+`"}`, uuid.NewString())
	f.refused(f.tenant, "corrected across kinds", "23503", "station_record_correction_fk", forge, q1.ID, `{"corrects_record_id":"`+c3.ID+`"}`, uuid.NewString())
	f.refused(f.tenant, "claimed an adapter source", "23514", "station_records_source_check", strings.Replace(forge, "r.source,", "'mes_adapter',", 1), q1.ID, `{}`, uuid.NewString())
	self := uuid.NewString()
	f.refused(f.tenant, "corrected itself", "23514", "station_record_not_self", forge, c3.ID, `{"corrects_record_id":"`+self+`"}`, self)
	f.refused(f.tenant, "recorded outside its shift", "23514", "Station record event is outside its shift", forge, q1.ID, `{"event_at":"`+stamp(s.starts.Add(-time.Second))+`"}`, uuid.NewString())
	f.refused(f.tenant, "rewrote a record", "42501", "permission denied", `UPDATE supervisor.station_records SET payload=payload WHERE id=$1`, c1.ID)
	f.refused(f.tenant, "deleted a record", "42501", "permission denied", `DELETE FROM supervisor.station_records WHERE id=$1`, c1.ID)
	f.refused(f.tenant, "deleted a grant", "42501", "permission denied", `DELETE FROM supervisor.station_authorizations WHERE workstation_id=$1`, s.station)
	f.refused(f.tenant, "skipped an authorization revision", "23514", "Invalid station authorization transition",
		`INSERT INTO supervisor.station_authorizations(tenant_id,site_id,workstation_id,grantee_issuer,grantee_subject,revision,action,actor_issuer,actor_subject) VALUES($1,$2,$3,$4,$5,5,'revoke','i','s')`, f.tenant, f.site, s.station, f.issuer, f.operator.subject)
	f.refused(f.tenant, "granted twice", "23514", "Invalid station authorization transition",
		`INSERT INTO supervisor.station_authorizations(tenant_id,site_id,workstation_id,grantee_issuer,grantee_subject,revision,action,actor_issuer,actor_subject) VALUES($1,$2,$3,$4,$5,4,'grant','i','s')`, f.tenant, f.site, s.station, f.issuer, f.operator.subject)
	f.refused(f.otherTenant, "granted in another tenant", "42501", "Station authorization tenant scope denied",
		`INSERT INTO supervisor.station_authorizations(tenant_id,site_id,workstation_id,grantee_issuer,grantee_subject,revision,action,actor_issuer,actor_subject) VALUES($1,$2,$3,$4,$5,4,'revoke','i','s')`, f.tenant, f.site, s.station, f.issuer, f.operator.subject)
	rtx, err := f.runtime.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = rtx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, f.otherTenant); err != nil {
		t.Fatal(err)
	}
	var visible int
	if err = rtx.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM supervisor.station_records)+(SELECT count(*) FROM supervisor.station_record_receipts)+(SELECT count(*) FROM supervisor.station_record_audit)+(SELECT count(*) FROM supervisor.station_authorizations)+(SELECT count(*) FROM supervisor.station_authorization_receipts)`).Scan(&visible); err != nil {
		t.Fatal(err)
	}
	_ = rtx.Rollback()
	if visible != 0 {
		t.Fatalf("other tenant sees %d station record rows", visible)
	}
	// Deactivating the membership refuses the next submission despite the grant.
	f.exec(`UPDATE supervisor.memberships SET active=false WHERE tenant_id=$1 AND subject=$2`, f.tenant, f.operator.subject)
	f.expect(submit(f.operator, "deactivated", note), 404, "NOT_FOUND")
	var submitted int
	if err = f.control.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.station_record_audit WHERE event_type='station_record.submitted' AND actor_subject=$1`, f.operator.subject).Scan(&submitted); err != nil || submitted != 14 {
		t.Fatalf("submitted audit rows %d (%v)", submitted, err)
	}
}

// TestStationRecordConcurrencyHTTPPostgres: concurrent submissions serialize. One key sent
// twice at once yields one record and one replay; two lifecycle events that cannot both
// apply yield one 201 and one 409; two corrections of one record yield one 201 and one
// CORRECTION_CONFLICT. Never a 500.
func TestStationRecordConcurrencyHTTPPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	now0 := f.clock()
	s := f.recordingStation(now0.Add(-time.Hour), now0.Add(time.Hour))
	f.expect(f.authorize(f.lead, s.station, f.operator, workstation.AuthorizationGrant, `"0"`), 201, "")
	base := f.clock()
	at := func(second int) string { return stamp(base.Add(time.Duration(second) * time.Second)) }
	records := "/v1/sites/" + f.site + "/workstations/" + s.station + "/records"
	race := func(keys [2]string, bodies [2]workstation.StationRecordBodyV1) [2]*httptest.ResponseRecorder {
		var out [2]*httptest.ResponseRecorder
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := range out {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				out[i] = f.call(f.operator, "POST", records, keys[i], "", f.encode(bodies[i]))
			}(i)
		}
		close(start)
		wg.Wait()
		return out
	}
	codes := func(got [2]*httptest.ResponseRecorder) map[int]int {
		m := map[int]int{}
		for _, w := range got {
			m[w.Code]++
		}
		return m
	}
	ack := s.entry(workstation.KindAcknowledged, at(0))
	dup := race([2]string{"ack", "ack"}, [2]workstation.StationRecordBodyV1{ack, ack})
	replayed := 0
	for _, w := range dup {
		if r := f.decodeRecord(w, 201); r.Replayed {
			replayed++
		}
	}
	if replayed != 1 {
		t.Fatalf("duplicate submit: %d replays, want 1", replayed)
	}
	f.expect(f.call(f.operator, "POST", records, "start", "", f.encode(s.entry(workstation.KindStarted, at(1)))), 201, "")
	pause, block := s.entry(workstation.KindPaused, at(2)), s.entry(workstation.KindBlocked, at(2))
	pause.Comment, block.Comment = "Break", "Jam"
	if got := race([2]string{"pause", "block"}, [2]workstation.StationRecordBodyV1{pause, block}); codes(got)[201] != 1 || codes(got)[409] != 1 {
		t.Fatalf("pause/block race: %d %s / %d %s", got[0].Code, got[0].Body.String(), got[1].Code, got[1].Body.String())
	}
	cycle := s.entry(workstation.KindCycleTime, at(3))
	cycle.CycleTimeSeconds = "3"
	first := f.decodeRecord(f.call(f.operator, "POST", records, "cycle", "", f.encode(cycle)), 201)
	a, b := cycle, cycle
	a.CorrectsRecordID, a.CycleTimeSeconds = first.ID, "2.9"
	b.CorrectsRecordID, b.CycleTimeSeconds = first.ID, "3.1"
	got := race([2]string{"fix-a", "fix-b"}, [2]workstation.StationRecordBodyV1{a, b})
	if codes(got)[201] != 1 || codes(got)[409] != 1 {
		t.Fatalf("correction race: %d %s / %d %s", got[0].Code, got[0].Body.String(), got[1].Code, got[1].Body.String())
	}
	for _, w := range got {
		if w.Code == 409 && !strings.Contains(w.Body.String(), `"code":"CORRECTION_CONFLICT"`) {
			t.Fatalf("correction race loser: %s", w.Body.String())
		}
	}
	var rows, corrections int
	if err := f.control.QueryRowContext(ctx, `SELECT count(*),count(corrects_record_id) FROM supervisor.station_records WHERE tenant_id=$1`, f.tenant).Scan(&rows, &corrections); err != nil || rows != 5 || corrections != 1 {
		t.Fatalf("rows=%d corrections=%d err=%v", rows, corrections, err)
	}
	// A revocation racing a submission: the revocation commits and the submission either
	// committed first (201) or sees the revocation (403); never a 500.
	note := s.entry(workstation.KindNote, at(4))
	note.Comment = "Racing a revocation"
	var revoke, submitted *httptest.ResponseRecorder
	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		revoke = f.authorize(f.lead, s.station, f.operator, workstation.AuthorizationRevoke, `"1"`)
	}()
	go func() {
		defer wg.Done()
		<-start
		submitted = f.call(f.operator, "POST", records, "racing-note", "", f.encode(note))
	}()
	close(start)
	wg.Wait()
	if revoke.Code != 201 || (submitted.Code != 201 && !(submitted.Code == 403 && strings.Contains(submitted.Body.String(), `"code":"STATION_NOT_AUTHORIZED"`))) {
		t.Fatalf("revoke %d %s / submit %d %s", revoke.Code, revoke.Body.String(), submitted.Code, submitted.Body.String())
	}
	f.expect(f.call(f.operator, "POST", records, "after-race", "", f.encode(note)), 403, "STATION_NOT_AUTHORIZED")
}

// TestStationRecordLateBindingHTTPPostgres: a record binds to the plan revision approved at
// its event time and the job-order revision that revision assigned. An entry whose binding
// was superseded after the event is accepted against the historical binding and flagged
// late; an event after a job-order revision the plan has not re-pinned is NEEDS_REPLAN; a
// correction binds to its predecessor even after the job order is revised.
func TestStationRecordLateBindingHTTPPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	now0 := f.clock()
	s := f.recordingStation(now0.Add(-time.Hour), now0.Add(time.Hour))
	f.expect(f.authorize(f.lead, s.station, f.operator, workstation.AuthorizationGrant, `"0"`), 201, "")
	records := "/v1/sites/" + f.site + "/workstations/" + s.station + "/records"
	submit := func(key string, body workstation.StationRecordBodyV1) *httptest.ResponseRecorder {
		return f.call(f.operator, "POST", records, key, "", f.encode(body))
	}
	t1 := f.clock()
	ack := f.decodeRecord(submit("ack", s.entry(workstation.KindAcknowledged, stamp(t1))), 201)
	if ack.PlanRevision != 1 || ack.Late {
		t.Fatalf("live ack: %+v", ack)
	}
	// Re-approve the plan with a changed target; an event before that approval still binds
	// to revision 1, now as a late entry.
	plans := "/v1/sites/" + f.site + "/shift-plans/" + s.plan.ID
	body := workstation.ShiftPlanBodyV1{SchemaVersion: 1, LineID: s.line, StartsAt: stamp(s.starts), EndsAt: stamp(s.ends), Timezone: "UTC", Assignments: []workstation.AssignmentV1{pin(s.station, s.order, 90)}}
	w := f.call(f.lead, "POST", plans+"/revisions", uuid.NewString(), `"2"`, f.encode(body))
	f.expect(w, 201, "")
	var revised workstation.ShiftPlanV1
	if err := json.Unmarshal(w.Body.Bytes(), &revised); err != nil {
		t.Fatal(err)
	}
	f.expect(f.call(f.approver, "POST", plans+"/approvals", uuid.NewString(), `"3"`, f.encode(workstation.ShiftPlanApproveInputV1{CanonicalPayloadSHA256: revised.Revisions[1].CanonicalPayloadSHA256})), 200, "")
	started := f.decodeRecord(submit("start", s.entry(workstation.KindStarted, stamp(t1.Add(time.Millisecond)))), 201)
	if started.PlanRevision != 1 || !started.Late || started.Record.JobOrderRevision != 1 {
		t.Fatalf("late start: %+v", started)
	}
	t2 := f.clock()
	cycle := s.entry(workstation.KindCycleTime, stamp(t2))
	cycle.CycleTimeSeconds = "2.5"
	live := f.decodeRecord(submit("cycle-live", cycle), 201)
	if live.PlanRevision != 2 || live.Late {
		t.Fatalf("live cycle: %+v", live)
	}
	// Revise the job order: the plan still pins revision 1.
	f.expect(f.call(f.lead, "POST", "/v1/sites/"+f.site+"/job-orders/"+s.order.ID+"/revisions", uuid.NewString(), `"1"`, f.encode(orderBody(s.source, s.sha, 90))), 201, "")
	lateCycle := s.entry(workstation.KindCycleTime, stamp(t2.Add(time.Millisecond)))
	lateCycle.CycleTimeSeconds = "2.75"
	c1 := f.decodeRecord(submit("cycle-late", lateCycle), 201)
	if c1.PlanRevision != 2 || !c1.Late || c1.Record.JobOrderRevision != 1 {
		t.Fatalf("late cycle after job-order revision: %+v", c1)
	}
	after := s.entry(workstation.KindCycleTime, stamp(f.clock()))
	after.CycleTimeSeconds = "2.6"
	f.expect(submit("cycle-after-revision", after), 409, "NEEDS_REPLAN")
	after.JobOrderRevision = 2
	f.expect(submit("cycle-revision-2", after), 409, "NEEDS_REPLAN")
	// A correction binds to its predecessor, not to the current order.
	fix := lateCycle
	fix.CycleTimeSeconds, fix.CorrectsRecordID = "2.7", c1.ID
	c2 := f.decodeRecord(submit("fix", fix), 201)
	if c2.Record.CorrectsRecordID != c1.ID || c2.PlanRevision != 2 || c2.Record.JobOrderRevision != 1 || c2.PlanID != s.plan.ID || !c2.Late {
		t.Fatalf("correction after job-order revision: %+v", c2)
	}
	// A plan re-approval alone never waives the instruction: withdraw it, re-approve the plan
	// after an event, and the late entry for that event is refused.
	other := f.recordingStation(now0.Add(-time.Hour), now0.Add(time.Hour))
	f.expect(f.authorize(f.lead, other.station, f.operator, workstation.AuthorizationGrant, `"0"`), 201, "")
	otherRecords := "/v1/sites/" + f.site + "/workstations/" + other.station + "/records"
	te := f.clock()
	f.expect(f.call(f.owner, "POST", "/v1/sites/"+f.site+"/sources/"+other.source+"/currentness-decisions", uuid.NewString(), `"1"`, `{"source_revision":1,"expected_source_state_revision":1,"authority_decision_id":"`+other.designation+`","authority_revision":1,"verdict":"withdraw","reason":"Withdraw for review"}`), 200, "")
	otherPlans := "/v1/sites/" + f.site + "/shift-plans/" + other.plan.ID
	w = f.call(f.lead, "POST", otherPlans+"/revisions", uuid.NewString(), `"2"`, f.encode(workstation.ShiftPlanBodyV1{SchemaVersion: 1, LineID: other.line, StartsAt: stamp(other.starts), EndsAt: stamp(other.ends), Timezone: "UTC", Assignments: []workstation.AssignmentV1{pin(other.station, other.order, 90)}}))
	f.expect(w, 201, "")
	if err := json.Unmarshal(w.Body.Bytes(), &revised); err != nil {
		t.Fatal(err)
	}
	f.expect(f.call(f.approver, "POST", otherPlans+"/approvals", uuid.NewString(), `"3"`, f.encode(workstation.ShiftPlanApproveInputV1{CanonicalPayloadSHA256: revised.Revisions[1].CanonicalPayloadSHA256})), 200, "")
	f.expect(f.call(f.operator, "POST", otherRecords, "late-withdrawn", "", f.encode(other.entry(workstation.KindAcknowledged, stamp(te)))), 409, "INSTRUCTION_UNAVAILABLE")
	retarget := fix
	retarget.CorrectsRecordID, retarget.JobOrderRevision = c2.ID, 2
	f.expect(submit("fix-retarget", retarget), 422, "INVALID_CORRECTION")
}

// TestStationRecordCarryForwardHTTPPostgres: one job order carried across two consecutive
// shifts. Lifecycle state is per shift plan, so shift 2 acknowledges and starts afresh after
// a partial shift 1, and a shift 1 value is still correctable after shift 2 recorded its entries.
func TestStationRecordCarryForwardHTTPPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	now0 := f.clock()
	// Shift 2 events lie after the handover but inside the five-minute clock-skew bound, so
	// the test never waits on the wall clock; the 60 s margin absorbs slow setup.
	handover := now0.Add(60 * time.Second)
	s := f.recordingStation(now0.Add(-time.Hour), handover)
	second := f.approvedPlan(s.line, s.station, s.order, handover, now0.Add(time.Hour), 70)
	f.startRun(f.lead, second.ID)
	f.expect(f.authorize(f.lead, s.station, f.operator, workstation.AuthorizationGrant, `"0"`), 201, "")
	records := "/v1/sites/" + f.site + "/workstations/" + s.station + "/records"
	submit := func(key string, body workstation.StationRecordBodyV1) workstation.StationRecordV1 {
		t.Helper()
		return f.decodeRecord(f.call(f.operator, "POST", records, key, "", f.encode(body)), 201)
	}
	quantity := func(at time.Time, good int64) workstation.StationRecordBodyV1 {
		q := s.entry(workstation.KindQuantity, stamp(at))
		q.GoodQuantity, q.RejectQuantity = pointer(good), pointer(int64(0))
		return q
	}
	t1 := f.clock()
	if !t1.Add(10 * time.Millisecond).Before(handover) {
		t.Fatalf("setup took past the handover at %s", handover)
	}
	submit("ack-1", s.entry(workstation.KindAcknowledged, stamp(t1)))
	submit("start-1", s.entry(workstation.KindStarted, stamp(t1.Add(time.Millisecond))))
	partial := submit("qty-1", quantity(t1.Add(2*time.Millisecond), 30))
	if partial.PlanID != s.plan.ID {
		t.Fatalf("shift 1 quantity bound to %s", partial.PlanID)
	}
	t2 := handover.Add(time.Millisecond)
	ack2 := submit("ack-2", s.entry(workstation.KindAcknowledged, stamp(t2)))
	if ack2.PlanID != second.ID || ack2.Late {
		t.Fatalf("shift 2 acknowledgement: %+v", ack2)
	}
	submit("start-2", s.entry(workstation.KindStarted, stamp(t2.Add(time.Millisecond))))
	submit("qty-2", quantity(t2.Add(2*time.Millisecond), 70))
	complete := s.entry(workstation.KindCompletionEvidence, stamp(t2.Add(3*time.Millisecond)))
	complete.Values = []workstation.EntryValueV1{{Field: "good_units", Value: "100", Unit: "count"}, {Field: "cycle_time", Value: "2.5", Unit: "s"}}
	submit("complete-2", complete)
	// After shift 2's entries, shift 1's partial quantity is still correctable and stays
	// bound to shift 1.
	fix := quantity(t1.Add(2*time.Millisecond), 31)
	fix.CorrectsRecordID = partial.ID
	fixed := submit("fix-1", fix)
	if fixed.PlanID != s.plan.ID || fixed.Record.CorrectsRecordID != partial.ID {
		t.Fatalf("shift 1 correction: %+v", fixed)
	}
	var shift1, shift2 int
	if err := f.control.QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE plan_id=$1),count(*) FILTER (WHERE plan_id=$2) FROM supervisor.station_records WHERE job_order_id=$3`, s.plan.ID, second.ID, s.order.ID).Scan(&shift1, &shift2); err != nil || shift1 != 4 || shift2 != 4 {
		t.Fatalf("shift 1 rows=%d shift 2 rows=%d err=%v", shift1, shift2, err)
	}
}

// TestStationRecordCorrectionAuthorityHTTPPostgres: a correction is entered by the
// predecessor's recorder or by a station-authorized owner or supervisor. Another authorized
// operator is refused with CORRECTION_NOT_PERMITTED and nothing is stored; a supervisor's
// correction is attributed to the supervisor and audited against the corrected record.
func TestStationRecordCorrectionAuthorityHTTPPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	now0 := f.clock()
	s := f.recordingStation(now0.Add(-time.Hour), now0.Add(time.Hour))
	f.expect(f.authorize(f.lead, s.station, f.operator, workstation.AuthorizationGrant, `"0"`), 201, "")
	f.expect(f.authorize(f.lead, s.station, f.spare, workstation.AuthorizationGrant, `"0"`), 201, "")
	f.expect(f.authorize(f.owner, s.station, f.lead, workstation.AuthorizationGrant, `"0"`), 201, "")
	records := "/v1/sites/" + f.site + "/workstations/" + s.station + "/records"
	submit := func(actor stationSession, key string, body workstation.StationRecordBodyV1) *httptest.ResponseRecorder {
		return f.call(actor, "POST", records, key, "", f.encode(body))
	}
	t1 := f.clock()
	f.decodeRecord(submit(f.operator, "ack", s.entry(workstation.KindAcknowledged, stamp(t1))), 201)
	f.decodeRecord(submit(f.operator, "start", s.entry(workstation.KindStarted, stamp(t1.Add(time.Millisecond)))), 201)
	cycle := s.entry(workstation.KindCycleTime, stamp(t1.Add(2*time.Millisecond)))
	cycle.CycleTimeSeconds = "2.5"
	c1 := f.decodeRecord(submit(f.operator, "cycle", cycle), 201)

	fix := cycle
	fix.CorrectsRecordID, fix.CycleTimeSeconds = c1.ID, "2.4"
	f.expect(submit(f.spare, "spare-fix", fix), 403, "CORRECTION_NOT_PERMITTED")
	var corrections int
	if err := f.control.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.station_records WHERE tenant_id=$1 AND corrects_record_id IS NOT NULL`, f.tenant).Scan(&corrections); err != nil || corrections != 0 {
		t.Fatalf("refused correction stored: %d (%v)", corrections, err)
	}
	c2 := f.decodeRecord(submit(f.operator, "self-fix", fix), 201)
	if c2.Record.CorrectsRecordID != c1.ID || c2.RecordedBy.Subject != f.operator.subject {
		t.Fatalf("self correction: %+v", c2)
	}

	again := cycle
	again.CorrectsRecordID, again.CycleTimeSeconds = c2.ID, "2.45"
	f.expect(submit(f.spare, "spare-fix-2", again), 403, "CORRECTION_NOT_PERMITTED")
	c3 := f.decodeRecord(submit(f.lead, "lead-fix", again), 201)
	if c3.Record.CorrectsRecordID != c2.ID || c3.RecordedBy.Subject != f.lead.subject || c3.RecordedBy.Issuer != f.issuer {
		t.Fatalf("supervisor correction: %+v", c3)
	}
	// The override itself is audited with the corrected record's recorder; the operator's
	// own correction is not.
	var overrides int
	var issuer, subject string
	if err := f.control.QueryRowContext(ctx, `SELECT count(*),min(predecessor_actor_issuer),min(predecessor_actor_subject) FROM supervisor.station_record_audit
 WHERE tenant_id=$1 AND event_type='station_record.corrected_by_supervisor' AND record_id=$2 AND actor_issuer=$3 AND actor_subject=$4 AND workstation_id=$5`, f.tenant, c3.ID, f.issuer, f.lead.subject, s.station).Scan(&overrides, &issuer, &subject); err != nil ||
		overrides != 1 || issuer != f.issuer || subject != f.operator.subject {
		t.Fatalf("supervisor correction override audit rows=%d predecessor=%q %q err=%v", overrides, issuer, subject, err)
	}
	if err := f.control.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.station_record_audit WHERE tenant_id=$1 AND event_type='station_record.corrected_by_supervisor'`, f.tenant).Scan(&overrides); err != nil || overrides != 1 {
		t.Fatalf("override audit rows in tenant %d (%v)", overrides, err)
	}
	f.refused(f.tenant, "audited an override without its recorder", "23514", "station_record_audit_predecessor_check",
		`INSERT INTO supervisor.station_record_audit(tenant_id,site_id,actor_issuer,actor_subject,workstation_id,record_id,event_type) VALUES($1,$2,$3,$4,$5,$6,'station_record.corrected_by_supervisor')`,
		f.tenant, f.site, f.issuer, f.lead.subject, s.station, c3.ID)
	var audited int
	var predecessor string
	if err := f.control.QueryRowContext(ctx, `SELECT count(*),min(p.actor_subject) FROM supervisor.station_record_audit a
 JOIN supervisor.station_records c ON (c.tenant_id,c.site_id,c.id)=(a.tenant_id,a.site_id,a.record_id)
 JOIN supervisor.station_records p ON (p.tenant_id,p.site_id,p.id)=(c.tenant_id,c.site_id,c.corrects_record_id)
 WHERE a.record_id=$1 AND a.event_type='station_record.submitted' AND a.actor_issuer=$2 AND a.actor_subject=$3`, c3.ID, f.issuer, f.lead.subject).Scan(&audited, &predecessor); err != nil ||
		audited != 1 || predecessor != f.operator.subject {
		t.Fatalf("supervisor correction audit rows=%d predecessor=%q err=%v", audited, predecessor, err)
	}
}

// TestStationRecordInstructionAsOfHTTPPostgres: a late entry whose job-order revision was
// superseded after its event is checked against the document ledger as of event_at. A
// withdrawal or revocation before the event refuses it; a source revision after the event
// does not.
func TestStationRecordInstructionAsOfHTTPPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	now0 := f.clock()
	// late runs before (ledger change before the event), then fixes the event time te, then
	// after (ledger change after it), revises the job order to a fresh instruction and
	// submits an acknowledgment at te against job-order revision 1.
	late := func(before, after func(s recordingStation)) (recordingStation, *httptest.ResponseRecorder) {
		t.Helper()
		s := f.recordingStation(now0.Add(-time.Hour), now0.Add(time.Hour))
		f.expect(f.authorize(f.lead, s.station, f.operator, workstation.AuthorizationGrant, `"0"`), 201, "")
		if before != nil {
			before(s)
		}
		// The clock is truncated to milliseconds: one more puts te after every committed row,
		// and the pause puts every later row after te.
		te := f.clock().Add(time.Millisecond)
		time.Sleep(5 * time.Millisecond)
		if after != nil {
			after(s)
		}
		fresh, sha, _ := f.instruction("Synthetic SOP: revised press setup", true)
		f.expect(f.call(f.lead, "POST", "/v1/sites/"+f.site+"/job-orders/"+s.order.ID+"/revisions", uuid.NewString(), `"1"`, f.encode(orderBody(fresh, sha, 90))), 201, "")
		return s, f.call(f.operator, "POST", "/v1/sites/"+f.site+"/workstations/"+s.station+"/records", uuid.NewString(), "", f.encode(s.entry(workstation.KindAcknowledged, stamp(te))))
	}
	stored := func(s recordingStation) int {
		t.Helper()
		var n int
		if err := f.control.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.station_records WHERE workstation_id=$1`, s.station).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	withdraw := func(s recordingStation) {
		f.expect(f.call(f.owner, "POST", "/v1/sites/"+f.site+"/sources/"+s.source+"/currentness-decisions", uuid.NewString(), `"1"`, `{"source_revision":1,"expected_source_state_revision":1,"authority_decision_id":"`+s.designation+`","authority_revision":1,"verdict":"withdraw","reason":"Withdraw for review"}`), 200, "")
	}
	revise := func(s recordingStation) {
		f.expect(f.call(f.owner, "POST", "/v1/sites/"+f.site+"/sources", uuid.NewString(), `"1"`, `{"source_id":"`+s.source+`","kind":"document","title":"Synthetic SOP","text":"Synthetic SOP: press and gauge bore, revised"}`), 200, "")
	}
	revoke := func(s recordingStation) {
		f.expect(f.call(f.owner, "POST", "/v1/sites/"+f.site+"/sources/"+s.source+"/revocations", uuid.NewString(), `"1"`, `{"reason":"Source withdrawn"}`), 200, "")
	}

	s, w := late(withdraw, nil)
	f.expect(w, 409, "INSTRUCTION_UNAVAILABLE")
	if n := stored(s); n != 0 {
		t.Fatalf("withdrawn-before entry stored: %d", n)
	}
	s, w = late(nil, revise)
	if r := f.decodeRecord(w, 201); !r.Late || r.Record.JobOrderRevision != 1 || r.PlanRevision != 1 {
		t.Fatalf("superseded-after entry: %+v", r)
	}
	s, w = late(revoke, nil)
	f.expect(w, 409, "INSTRUCTION_REVOKED")
	if n := stored(s); n != 0 {
		t.Fatalf("revoked-before entry stored: %d", n)
	}
}
