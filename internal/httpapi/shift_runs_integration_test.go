//go:build integration

package httpapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/workstation"
	"github.com/google/uuid"
)

func (f *stationFixture) runPath(planID string) string {
	return "/v1/sites/" + f.site + "/shift-plans/" + planID + "/run"
}

func (f *stationFixture) getPlan(planID string) workstation.ShiftPlanV1 {
	f.t.Helper()
	w := f.call(f.lead, "GET", "/v1/sites/"+f.site+"/shift-plans/"+planID, "", "", "")
	f.expect(w, 200, "")
	var p workstation.ShiftPlanV1
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		f.t.Fatal(err)
	}
	return p
}

func etag(v int64) string { return `"` + strconv.FormatInt(v, 10) + `"` }

func (f *stationFixture) decodeRun(w *httptest.ResponseRecorder, status int) workstation.ShiftRunV1 {
	f.t.Helper()
	f.expect(w, status, "")
	var r workstation.ShiftRunV1
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		f.t.Fatal(err)
	}
	return r
}

// startRun starts the plan's run on its approved revision, as a supervisor does before
// operators record (shift run feature: records need a started run).
func (f *stationFixture) startRun(actor stationSession, planID string) workstation.ShiftRunV1 {
	f.t.Helper()
	p := f.getPlan(planID)
	return f.decodeRun(f.call(actor, "POST", f.runPath(planID), uuid.NewString(), etag(p.StateRevision),
		f.encode(workstation.ShiftRunStartInputV1{SchemaVersion: 1, PlanRevision: *p.ApprovedRevision})), 201)
}

func nextShift(s recordingStation) workstation.NextShiftV1 {
	return workstation.NextShiftV1{LineID: s.line, StartsAt: stamp(s.ends), EndsAt: stamp(s.ends.Add(8 * time.Hour)), Timezone: "UTC"}
}

// TestWIS701ShiftRunHTTPPostgres: a supervisor starts an approved plan's run, operators
// record only while it is started, and End writes exactly one final summary and one handover
// draft and cites them; late records append summary revisions. Every authority and lifecycle
// boundary has its negative case (rules 03, 05, 07, 10), against PostgreSQL as the runtime role.
func TestWIS701ShiftRunHTTPPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	now0 := f.clock()
	s := f.plannedStation(now0.Add(-time.Hour), now0.Add(time.Hour))
	f.expect(f.authorize(f.lead, s.station, f.operator, workstation.AuthorizationGrant, `"0"`), 201, "")
	run := f.runPath(s.plan.ID)
	plan := f.getPlan(s.plan.ID) // state revision 2 after approval
	start := func(actor stationSession, key, ifMatch string, revision int64) *httptest.ResponseRecorder {
		return f.call(actor, "POST", run, key, ifMatch, f.encode(workstation.ShiftRunStartInputV1{SchemaVersion: 1, PlanRevision: revision}))
	}
	end := func(actor stationSession, key, ifMatch string) *httptest.ResponseRecorder {
		return f.call(actor, "POST", run+"/end", key, ifMatch, f.encode(workstation.ShiftRunEndInputV1{SchemaVersion: 1, NextShift: nextShift(s)}))
	}
	records := "/v1/sites/" + f.site + "/workstations/" + s.station + "/records"
	submit := func(body workstation.StationRecordBodyV1) *httptest.ResponseRecorder {
		return f.call(f.operator, "POST", records, uuid.NewString(), "", f.encode(body))
	}
	runs := func() int {
		return f.count(`SELECT count(*) FROM supervisor.shift_runs WHERE tenant_id=$1 AND plan_id=$2`, f.tenant, s.plan.ID)
	}
	planRecords := func() int {
		return f.count(`SELECT count(*) FROM supervisor.station_records WHERE tenant_id=$1 AND plan_id=$2`, f.tenant, s.plan.ID)
	}
	state := etag(plan.StateRevision)

	// Planned: anyone in the site reads it; nobody records yet.
	if got := f.decodeRun(f.call(f.operator, "GET", run, "", "", ""), 200); got.State != workstation.RunPlanned || got.PlanStateRevision != plan.StateRevision || got.StartedAt != nil {
		t.Fatalf("planned run %+v", got)
	}
	f.expect(submit(s.entry(workstation.KindAcknowledged, stamp(f.clock()))), 409, "SHIFT_NOT_STARTED")
	if n := planRecords(); n != 0 {
		t.Fatalf("a record before start was stored: %d", n)
	}
	// Review of #294: a station tablet polls the run every 10 s, and audit rows are permanent, so a
	// read is audited at most once a minute per reader and plan (as interview reads are). Another
	// reader still gets their own row.
	readAudits := func(subject string) int {
		return f.count(`SELECT count(*) FROM supervisor.shift_run_audit WHERE tenant_id=$1 AND plan_id=$2 AND actor_subject=$3 AND event_type='shift_run.read'`, f.tenant, s.plan.ID, subject)
	}
	for range 3 {
		f.decodeRun(f.call(f.operator, "GET", run, "", "", ""), 200)
	}
	f.decodeRun(f.call(f.lead, "GET", run, "", "", ""), 200)
	if op, lead := readAudits(f.operator.subject), readAudits(f.lead.subject); op != 1 || lead != 1 {
		t.Fatalf("polled run reads wrote %d audit rows for the operator and %d for the lead, want 1 each", op, lead)
	}

	// Who may start: owners and supervisors with a recent authentication, in this tenant only.
	f.expect(start(f.operator, uuid.NewString(), state, 1), 403, "PERMISSION_DENIED")
	f.expect(start(f.viewer, uuid.NewString(), state, 1), 403, "PERMISSION_DENIED")
	f.expect(start(f.stale, uuid.NewString(), state, 1), 428, "REAUTHENTICATION_REQUIRED")
	f.expect(start(f.outsider, uuid.NewString(), state, 1), 404, "NOT_FOUND")
	f.expect(f.call(f.outsider, "GET", run, "", "", ""), 404, "NOT_FOUND")
	f.expect(end(f.outsider, uuid.NewString(), state), 404, "NOT_FOUND")
	f.expect(start(f.lead, uuid.NewString(), "", 1), 428, "REVISION_REQUIRED")
	f.expect(start(f.lead, uuid.NewString(), etag(plan.StateRevision-1), 1), 412, "REVISION_CONFLICT")
	f.expect(f.call(f.lead, "POST", run, uuid.NewString(), state, `{"schema_version":1,"plan_revision":1,"tenant_id":"`+f.otherTenant+`"}`), 422, "INVALID_SHIFT_RUN")
	f.expect(start(f.lead, uuid.NewString(), state, 2), 409, "SHIFT_PLAN_NOT_APPROVED")
	f.expect(end(f.lead, uuid.NewString(), state), 409, "SHIFT_NOT_STARTED")
	if n := runs(); n != 0 {
		t.Fatalf("a refused start stored a run: %d", n)
	}

	// Start, replay, conflicting replay, second start.
	started := f.decodeRun(start(f.lead, "start-1", state, 1), 201)
	if started.State != workstation.RunStarted || *started.StartedPlanRevision != 1 || started.StartedBy.Subject != f.lead.subject || started.StartedAt == nil || started.Replayed {
		t.Fatalf("started %+v", started)
	}
	replayed := f.decodeRun(start(f.lead, "start-1", state, 1), 201)
	if !replayed.Replayed || !replayed.StartedAt.Equal(*started.StartedAt) {
		t.Fatalf("replay %+v", replayed)
	}
	f.expect(f.call(f.lead, "POST", run, "start-1", state, `{"schema_version":1,"plan_revision":2}`), 409, "IDEMPOTENCY_CONFLICT")
	f.expect(start(f.approver, uuid.NewString(), state, 1), 409, "SHIFT_RUN_ALREADY_STARTED")
	if n := runs(); n != 1 {
		t.Fatalf("runs %d", n)
	}

	// Recording while started; a re-approval keeps the run and binds new records to it.
	testStart := f.clock()
	at := func(ms int) string { return stamp(testStart.Add(time.Duration(ms) * time.Millisecond)) }
	f.decodeRecord(submit(s.entry(workstation.KindAcknowledged, at(1))), 201)
	f.decodeRecord(submit(s.entry(workstation.KindStarted, at(2))), 201)
	revisedBody := workstation.ShiftPlanBodyV1{SchemaVersion: 1, LineID: s.line, StartsAt: stamp(s.starts), EndsAt: stamp(s.ends), Timezone: "UTC",
		Assignments: []workstation.AssignmentV1{pin(s.station, s.order, 90)}}
	plans := "/v1/sites/" + f.site + "/shift-plans/" + s.plan.ID
	var revised workstation.ShiftPlanV1
	w := f.call(f.lead, "POST", plans+"/revisions", uuid.NewString(), state, f.encode(revisedBody))
	f.expect(w, 201, "")
	_ = json.Unmarshal(w.Body.Bytes(), &revised)
	f.expect(f.call(f.approver, "POST", plans+"/approvals", uuid.NewString(), etag(revised.StateRevision),
		f.encode(workstation.ShiftPlanApproveInputV1{CanonicalPayloadSHA256: revised.Revisions[1].CanonicalPayloadSHA256})), 200, "")
	still := f.decodeRun(f.call(f.operator, "GET", run, "", "", ""), 200)
	if still.State != workstation.RunStarted || *still.StartedPlanRevision != 1 || still.PlanStateRevision != revised.StateRevision+1 {
		t.Fatalf("run after re-approval %+v", still)
	}
	quantity := s.entry(workstation.KindQuantity, stamp(f.clock()))
	quantity.GoodQuantity, quantity.RejectQuantity = pointer(int64(30)), pointer(int64(1))
	if r := f.decodeRecord(submit(quantity), 201); r.PlanRevision != 2 {
		t.Fatalf("record after re-approval bound plan revision %d", r.PlanRevision)
	}
	state = etag(still.PlanStateRevision)

	// End: operators cannot; a stale If-Match is refused; the starter may end it.
	f.expect(end(f.operator, uuid.NewString(), state), 403, "PERMISSION_DENIED")
	f.expect(end(f.viewer, uuid.NewString(), state), 403, "PERMISSION_DENIED")
	f.expect(end(f.stale, uuid.NewString(), state), 428, "REAUTHENTICATION_REQUIRED")
	f.expect(end(f.lead, uuid.NewString(), etag(still.PlanStateRevision-1)), 412, "REVISION_CONFLICT")
	f.expect(f.call(f.lead, "POST", run+"/end", uuid.NewString(), state, `{"schema_version":1,"next_shift":{"line_id":"x","starts_at":"","ends_at":"","timezone":"UTC"}}`), 422, "INVALID_SHIFT_RUN")
	summaries := func() int {
		return f.count(`SELECT count(*) FROM supervisor.shift_summaries WHERE tenant_id=$1 AND plan_id=$2`, f.tenant, s.plan.ID)
	}
	handovers := func() int {
		return f.count(`SELECT count(*) FROM supervisor.shift_handovers WHERE tenant_id=$1 AND plan_id=$2`, f.tenant, s.plan.ID)
	}
	planRows := func() int {
		return f.count(`SELECT (SELECT count(*) FROM supervisor.shift_plan_revisions WHERE tenant_id=$1)+(SELECT count(*) FROM supervisor.shift_plan_approvals WHERE tenant_id=$1)+(SELECT count(*) FROM supervisor.shift_plan_heads WHERE tenant_id=$1)`, f.tenant)
	}
	beforeSummaries, beforeHandovers, beforePlans := summaries(), handovers(), planRows()
	ended := f.decodeRun(end(f.lead, "end-1", state), 200)
	if ended.State != workstation.RunEnded || ended.EndedBy.Subject != f.lead.subject || *ended.EndedPlanRevision != 2 || ended.EndedAt == nil || !ended.EndedAt.After(*ended.StartedAt) ||
		ended.Summary == nil || ended.HandoverRevision == nil || ended.SummaryRevised || *ended.LatestSummaryRevision != ended.Summary.Revision {
		t.Fatalf("ended %+v", ended)
	}
	if summaries() != beforeSummaries+1 || handovers() != beforeHandovers+1 || planRows() != beforePlans {
		t.Fatalf("end wrote summaries %d->%d handovers %d->%d plan rows %d->%d", beforeSummaries, summaries(), beforeHandovers, handovers(), beforePlans, planRows())
	}
	var summary workstation.ShiftSummarySnapshotV1
	w = f.call(f.operator, "GET", plans+"/summaries/"+strconv.FormatInt(ended.Summary.Revision, 10), "", "", "")
	f.expect(w, 200, "")
	_ = json.Unmarshal(w.Body.Bytes(), &summary)
	if summary.CanonicalPayloadSHA256 != ended.Summary.CanonicalPayloadSHA256 || !summary.Summary.ShiftEnded || summary.Summary.RunEndedAt == nil ||
		!summary.Summary.RunEndedAt.Equal(*ended.EndedAt) || !summary.Summary.AsOf.Equal(*ended.EndedAt) || summary.Summary.PlanRevision != 2 || summary.Summary.SourceRecordCount != 3 {
		t.Fatalf("final summary %+v vs run %+v", summary.Summary, ended)
	}
	var handover workstation.ShiftHandoverSnapshotV1
	w = f.call(f.operator, "GET", plans+"/handovers/"+strconv.FormatInt(*ended.HandoverRevision, 10), "", "", "")
	f.expect(w, 200, "")
	_ = json.Unmarshal(w.Body.Bytes(), &handover)
	if handover.Status != workstation.HandoverDraft || handover.Handover.SummaryRevision != ended.Summary.Revision || handover.Handover.SummarySHA256 != ended.Summary.CanonicalPayloadSHA256 {
		t.Fatalf("handover %+v", handover)
	}

	// After the end: replay, then everything that would change the shift is refused.
	if r := f.decodeRun(end(f.lead, "end-1", state), 200); !r.Replayed || !r.EndedAt.Equal(*ended.EndedAt) {
		t.Fatalf("end replay %+v", r)
	}
	f.expect(end(f.owner, uuid.NewString(), state), 409, "SHIFT_RUN_ENDED")
	f.expect(start(f.owner, uuid.NewString(), state, 2), 409, "SHIFT_RUN_ENDED")
	draft := revisedBody
	draft.Assignments = []workstation.AssignmentV1{pin(s.station, s.order, 80)}
	w = f.call(f.lead, "POST", plans+"/revisions", uuid.NewString(), state, f.encode(draft))
	f.expect(w, 201, "") // a draft is harmless
	_ = json.Unmarshal(w.Body.Bytes(), &revised)
	f.expect(f.call(f.approver, "POST", plans+"/approvals", uuid.NewString(), etag(revised.StateRevision),
		f.encode(workstation.ShiftPlanApproveInputV1{CanonicalPayloadSHA256: revised.Revisions[2].CanonicalPayloadSHA256})), 409, "SHIFT_RUN_ENDED")

	// Late data: an event at or after the end is refused; one before it appends a revision.
	tooLate := s.entry(workstation.KindNote, stamp(ended.EndedAt.Add(time.Millisecond)))
	tooLate.Comment = "After the end"
	f.expect(submit(tooLate), 409, "SHIFT_RUN_ENDED")
	cited := summaries()
	var citedPayload string
	if err := f.control.QueryRowContext(ctx, `SELECT canonical_payload FROM supervisor.shift_summaries WHERE tenant_id=$1 AND plan_id=$2 AND revision=$3`, f.tenant, s.plan.ID, ended.Summary.Revision).Scan(&citedPayload); err != nil {
		t.Fatal(err)
	}
	late := s.entry(workstation.KindNote, stamp(ended.EndedAt.Add(-time.Millisecond)))
	late.Comment = "Entered after the end"
	f.decodeRecord(submit(late), 201)
	if summaries() != cited+1 {
		t.Fatalf("late record did not append a summary revision: %d", summaries())
	}
	var again string
	if err := f.control.QueryRowContext(ctx, `SELECT canonical_payload FROM supervisor.shift_summaries WHERE tenant_id=$1 AND plan_id=$2 AND revision=$3`, f.tenant, s.plan.ID, ended.Summary.Revision).Scan(&again); err != nil || again != citedPayload {
		t.Fatalf("cited snapshot changed: %v", err)
	}
	after := f.decodeRun(f.call(f.operator, "GET", run, "", "", ""), 200)
	if !after.SummaryRevised || *after.LatestSummaryRevision != ended.Summary.Revision+1 || after.Summary.Revision != ended.Summary.Revision || after.Summary.CanonicalPayloadSHA256 != ended.Summary.CanonicalPayloadSHA256 {
		t.Fatalf("run after late record %+v", after)
	}
	// The end-of-shift handover was drafted from a summary that is no longer the latest.
	f.expect(f.call(f.lead, "POST", plans+"/handovers/"+strconv.FormatInt(*ended.HandoverRevision, 10)+"/adopt", uuid.NewString(), "",
		f.encode(workstation.ShiftHandoverAdoptInputV1{CanonicalPayloadSHA256: handover.CanonicalPayloadSHA256})), 409, "HANDOVER_SOURCE_STALE")
	// Replays return the original responses verbatim (rule 07), not the run as it is now: the
	// start replay still says started, the end replay predates the late record, and the same
	// next-shift instants spelled with +00:00 instead of Z are the same request.
	if r := f.decodeRun(start(f.lead, "start-1", etag(plan.StateRevision), 1), 201); !r.Replayed || r.State != workstation.RunStarted || r.EndedAt != nil || !r.StartedAt.Equal(*started.StartedAt) {
		t.Fatalf("start replay after the end %+v", r)
	}
	offset := nextShift(s)
	offset.StartsAt, offset.EndsAt = s.ends.UTC().Format("2006-01-02T15:04:05.999999999-07:00"), s.ends.Add(8*time.Hour).UTC().Format("2006-01-02T15:04:05.999999999-07:00")
	if offset.StartsAt == nextShift(s).StartsAt {
		t.Fatal("the offset spelling must differ from the Z spelling")
	}
	endReplay := f.decodeRun(f.call(f.lead, "POST", run+"/end", "end-1", state, f.encode(workstation.ShiftRunEndInputV1{SchemaVersion: 1, NextShift: offset})), 200)
	if !endReplay.Replayed || endReplay.SummaryRevised || *endReplay.LatestSummaryRevision != ended.Summary.Revision || !endReplay.EndedAt.Equal(*ended.EndedAt) {
		t.Fatalf("end replay after a late record %+v", endReplay)
	}
	var lateSummary workstation.ShiftSummarySnapshotV1
	w = f.call(f.operator, "GET", plans+"/summaries/"+strconv.FormatInt(*after.LatestSummaryRevision, 10), "", "", "")
	f.expect(w, 200, "")
	_ = json.Unmarshal(w.Body.Bytes(), &lateSummary)
	if lateSummary.Summary.SourceRecordCount != 4 || !lateSummary.Summary.RunEndedAt.Equal(*ended.EndedAt) {
		t.Fatalf("late summary %+v", lateSummary.Summary)
	}
}

// TestWIS701ShiftRunStartWindow: a run starts at most 30 minutes before the planned start
// (an owner may start it), never on a draft or superseded revision, and never once the
// planned end has passed.
func TestWIS701ShiftRunStartWindow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	now0 := f.clock()
	try := func(actor stationSession, planID string, revision int64) *httptest.ResponseRecorder {
		p := f.getPlan(planID)
		return f.call(actor, "POST", f.runPath(planID), uuid.NewString(), etag(p.StateRevision), f.encode(workstation.ShiftRunStartInputV1{SchemaVersion: 1, PlanRevision: revision}))
	}
	early := f.plannedStation(now0.Add(40*time.Minute), now0.Add(2*time.Hour))
	f.expect(try(f.lead, early.plan.ID, 1), 409, "SHIFT_START_TOO_EARLY")
	soon := f.plannedStation(now0.Add(20*time.Minute), now0.Add(2*time.Hour))
	if r := f.decodeRun(try(f.owner, soon.plan.ID, 1), 201); r.StartedBy.Subject != f.owner.subject {
		t.Fatalf("owner start %+v", r)
	}
	past := f.plannedStation(now0.Add(-2*time.Hour), now0.Add(-time.Minute))
	f.expect(try(f.lead, past.plan.ID, 1), 409, "SHIFT_WINDOW_ENDED")

	// A draft-only plan, and a superseded revision after re-approval, are not startable.
	line, station := uuid.NewString(), uuid.NewString()
	source, sha, _ := f.instruction("Synthetic SOP: draft check", true)
	order := f.jobOrder(f.lead, orderBody(source, sha, 10))
	body := workstation.ShiftPlanBodyV1{SchemaVersion: 1, LineID: line, StartsAt: stamp(now0.Add(-time.Hour)), EndsAt: stamp(now0.Add(time.Hour)), Timezone: "UTC",
		Assignments: []workstation.AssignmentV1{pin(station, order, 10)}}
	w := f.call(f.lead, "POST", "/v1/sites/"+f.site+"/shift-plans", uuid.NewString(), "", f.encode(body))
	f.expect(w, 201, "")
	var draft workstation.ShiftPlanV1
	_ = json.Unmarshal(w.Body.Bytes(), &draft)
	f.expect(try(f.lead, draft.ID, 1), 409, "SHIFT_PLAN_NOT_APPROVED")
	f.expect(f.call(f.approver, "POST", "/v1/sites/"+f.site+"/shift-plans/"+draft.ID+"/approvals", uuid.NewString(), `"1"`,
		f.encode(workstation.ShiftPlanApproveInputV1{CanonicalPayloadSHA256: draft.Revisions[0].CanonicalPayloadSHA256})), 200, "")
	body.Assignments = []workstation.AssignmentV1{pin(station, order, 9)}
	var revised workstation.ShiftPlanV1
	w = f.call(f.lead, "POST", "/v1/sites/"+f.site+"/shift-plans/"+draft.ID+"/revisions", uuid.NewString(), `"2"`, f.encode(body))
	f.expect(w, 201, "")
	_ = json.Unmarshal(w.Body.Bytes(), &revised)
	f.expect(f.call(f.approver, "POST", "/v1/sites/"+f.site+"/shift-plans/"+draft.ID+"/approvals", uuid.NewString(), `"3"`,
		f.encode(workstation.ShiftPlanApproveInputV1{CanonicalPayloadSHA256: revised.Revisions[1].CanonicalPayloadSHA256})), 200, "")
	f.expect(try(f.lead, draft.ID, 1), 409, "SHIFT_PLAN_NOT_APPROVED")
	if n := f.count(`SELECT count(*) FROM supervisor.shift_runs WHERE tenant_id=$1`, f.tenant); n != 1 {
		t.Fatalf("only the owner's start may be stored, got %d runs", n)
	}
}

// TestWIS701ShiftRunEndRacesRecord: an End and a record submitted together both succeed and
// the record is counted either in End's summary or in a later revision, never lost.
func TestWIS701ShiftRunEndRacesRecord(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	now0 := f.clock()
	s := f.recordingStation(now0.Add(-time.Hour), now0.Add(time.Hour))
	f.expect(f.authorize(f.lead, s.station, f.operator, workstation.AuthorizationGrant, `"0"`), 201, "")
	records := "/v1/sites/" + f.site + "/workstations/" + s.station + "/records"
	f.decodeRecord(f.call(f.operator, "POST", records, uuid.NewString(), "", f.encode(s.entry(workstation.KindAcknowledged, stamp(f.clock())))), 201)
	note := s.entry(workstation.KindNote, stamp(f.clock())) // before the end, whichever commits first
	note.Comment = "race"
	state := etag(f.getPlan(s.plan.ID).StateRevision)
	var wg sync.WaitGroup
	var endW, recW *httptest.ResponseRecorder
	wg.Add(2)
	go func() {
		defer wg.Done()
		endW = f.call(f.lead, "POST", f.runPath(s.plan.ID)+"/end", uuid.NewString(), state, f.encode(workstation.ShiftRunEndInputV1{SchemaVersion: 1, NextShift: nextShift(s)}))
	}()
	go func() {
		defer wg.Done()
		recW = f.call(f.operator, "POST", records, uuid.NewString(), "", f.encode(note))
	}()
	wg.Wait()
	if endW.Code != 200 || recW.Code != 201 {
		t.Fatalf("end %d %s / record %d %s", endW.Code, endW.Body.String(), recW.Code, recW.Body.String())
	}
	var count int
	if err := f.control.QueryRowContext(ctx, `SELECT (payload->>'source_record_count')::int FROM supervisor.shift_summaries WHERE tenant_id=$1 AND plan_id=$2 ORDER BY revision DESC LIMIT 1`, f.tenant, s.plan.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if want := f.count(`SELECT count(*) FROM supervisor.station_records WHERE tenant_id=$1 AND plan_id=$2`, f.tenant, s.plan.ID); count != want || want != 2 {
		t.Fatalf("latest summary counts %d of %d records", count, want)
	}
}

// TestWIS701ShiftRunOtherSiteNotFound (rule 03): a supervisor of two sites of one tenant
// cannot start, end or read a plan's run through the other site's path; the lookup is
// site-scoped and answers the non-disclosing 404, and no run is stored.
func TestWIS701ShiftRunOtherSiteNotFound(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	now0 := f.clock()
	s := f.plannedStation(now0.Add(-time.Hour), now0.Add(time.Hour))
	second := uuid.NewString()
	f.exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'second site')`, f.tenant, second)
	f.exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'supervisor')`, f.tenant, second, f.issuer, f.lead.subject)
	p := f.getPlan(s.plan.ID)
	wrong := "/v1/sites/" + second + "/shift-plans/" + s.plan.ID + "/run"
	f.expect(f.call(f.lead, "GET", wrong, "", "", ""), 404, "NOT_FOUND")
	f.expect(f.call(f.lead, "POST", wrong, uuid.NewString(), etag(p.StateRevision), f.encode(workstation.ShiftRunStartInputV1{SchemaVersion: 1, PlanRevision: 1})), 404, "NOT_FOUND")
	f.expect(f.call(f.lead, "POST", wrong+"/end", uuid.NewString(), etag(p.StateRevision), f.encode(workstation.ShiftRunEndInputV1{SchemaVersion: 1, NextShift: nextShift(s)})), 404, "NOT_FOUND")
	if n := f.count(`SELECT count(*) FROM supervisor.shift_runs WHERE tenant_id=$1`, f.tenant); n != 0 {
		t.Fatalf("a start through another site stored %d runs", n)
	}
	// Once started on its own site, the run is still invisible and unendable from the other.
	f.startRun(f.lead, s.plan.ID)
	f.expect(f.call(f.lead, "GET", wrong, "", "", ""), 404, "NOT_FOUND")
	f.expect(f.call(f.lead, "POST", wrong+"/end", uuid.NewString(), etag(p.StateRevision), f.encode(workstation.ShiftRunEndInputV1{SchemaVersion: 1, NextShift: nextShift(s)})), 404, "NOT_FOUND")
	if n := f.count(`SELECT count(*) FROM supervisor.shift_runs WHERE tenant_id=$1 AND ended_at IS NOT NULL`, f.tenant); n != 0 {
		t.Fatalf("an end through another site ended %d runs", n)
	}
}

// TestWIS701ShiftRunCorrectionAfterEnd: correcting a shift record after the run ended appends
// the next summary revision; the revision the run cites stays byte-identical (rule 06).
func TestWIS701ShiftRunCorrectionAfterEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	now0 := f.clock()
	s := f.recordingStation(now0.Add(-time.Hour), now0.Add(time.Hour))
	f.expect(f.authorize(f.lead, s.station, f.operator, workstation.AuthorizationGrant, `"0"`), 201, "")
	records := "/v1/sites/" + f.site + "/workstations/" + s.station + "/records"
	submit := func(body workstation.StationRecordBodyV1) *httptest.ResponseRecorder {
		return f.call(f.operator, "POST", records, uuid.NewString(), "", f.encode(body))
	}
	t0 := f.clock()
	f.decodeRecord(submit(s.entry(workstation.KindAcknowledged, stamp(t0))), 201)
	f.decodeRecord(submit(s.entry(workstation.KindStarted, stamp(t0.Add(time.Millisecond)))), 201)
	q := s.entry(workstation.KindQuantity, stamp(t0.Add(2*time.Millisecond)))
	q.GoodQuantity, q.RejectQuantity = pointer(int64(40)), pointer(int64(0))
	original := f.decodeRecord(submit(q), 201)
	ended := f.decodeRun(f.call(f.lead, "POST", f.runPath(s.plan.ID)+"/end", uuid.NewString(), etag(f.getPlan(s.plan.ID).StateRevision),
		f.encode(workstation.ShiftRunEndInputV1{SchemaVersion: 1, NextShift: nextShift(s)})), 200)
	summaryAt := func(revision int64) (string, workstation.ShiftSummaryV1) {
		t.Helper()
		var payload string
		if err := f.control.QueryRowContext(ctx, `SELECT canonical_payload FROM supervisor.shift_summaries WHERE tenant_id=$1 AND plan_id=$2 AND revision=$3`, f.tenant, s.plan.ID, revision).Scan(&payload); err != nil {
			t.Fatal(err)
		}
		var env struct {
			Body workstation.ShiftSummaryV1 `json:"body"`
		}
		if err := json.Unmarshal([]byte(payload), &env); err != nil {
			t.Fatal(err)
		}
		return payload, env.Body
	}
	citedBefore, cited := summaryAt(ended.Summary.Revision)
	if good := cited.Stations[0].Recorded.GoodQuantity; good == nil || *good != 40 {
		t.Fatalf("final summary good quantity %v", good)
	}

	fix := q
	fix.GoodQuantity, fix.CorrectsRecordID = pointer(int64(38)), original.ID
	corrected := f.decodeRecord(submit(fix), 201)
	if corrected.Record.CorrectsRecordID != original.ID {
		t.Fatalf("correction %+v", corrected)
	}
	latest := f.count(`SELECT max(revision) FROM supervisor.shift_summaries WHERE tenant_id=$1 AND plan_id=$2`, f.tenant, s.plan.ID)
	if int64(latest) != ended.Summary.Revision+1 {
		t.Fatalf("correction after the end: latest summary revision %d, want %d", latest, ended.Summary.Revision+1)
	}
	if citedAfter, _ := summaryAt(ended.Summary.Revision); citedAfter != citedBefore {
		t.Fatal("the cited end-of-shift summary changed")
	}
	_, revised := summaryAt(int64(latest))
	if good := revised.Stations[0].Recorded.GoodQuantity; good == nil || *good != 38 || revised.SourceRecordCount != 4 || revised.RunEndedAt == nil || !revised.RunEndedAt.Equal(*ended.EndedAt) {
		t.Fatalf("revised summary: good %v records %d run_ended_at %v", good, revised.SourceRecordCount, revised.RunEndedAt)
	}
	run := f.decodeRun(f.call(f.operator, "GET", f.runPath(s.plan.ID), "", "", ""), 200)
	if !run.SummaryRevised || *run.LatestSummaryRevision != int64(latest) || run.Summary.Revision != ended.Summary.Revision {
		t.Fatalf("run after correction %+v", run)
	}
}

// TestWIS701ShiftRunEndClockAfterLocks: End reads its clock only once it holds the site and
// plan locks. Another transaction holds the site lock while End waits; anything that commits
// under that lock (a station record, say) does so before the holder releases it, so ended_at
// must not precede the release. Reading the clock before waiting would put it earlier.
func TestWIS701ShiftRunEndClockAfterLocks(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	now0 := f.clock()
	s := f.recordingStation(now0.Add(-time.Hour), now0.Add(time.Hour))
	state := etag(f.getPlan(s.plan.ID).StateRevision)
	holder, err := f.control.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback()
	var holderPID int
	if err = holder.QueryRowContext(ctx, `SELECT pg_backend_pid() FROM supervisor.sites WHERE tenant_id=$1 AND id=$2 FOR NO KEY UPDATE`, f.tenant, f.site).Scan(&holderPID); err != nil {
		t.Fatal(err)
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- f.call(f.lead, "POST", f.runPath(s.plan.ID)+"/end", uuid.NewString(), state, f.encode(workstation.ShiftRunEndInputV1{SchemaVersion: 1, NextShift: nextShift(s)}))
	}()
	// Wait until End is blocked on the holder's lock.
	for i := 0; ; i++ {
		var waiting bool
		if err = f.control.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, holderPID).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		if i > 2000 {
			t.Fatal("End never waited on the site lock")
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	var released time.Time
	if err = holder.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&released); err != nil {
		t.Fatal(err)
	}
	if err = holder.Commit(); err != nil {
		t.Fatal(err)
	}
	ended := f.decodeRun(<-done, 200)
	if ended.EndedAt.Before(released) {
		t.Fatalf("ended_at %v precedes the lock release %v: the clock was read before the locks", ended.EndedAt, released)
	}
}

// TestWIS701ShiftRunClockSkewRecord: a tablet clock up to five minutes ahead can put a record
// at or after the run end. The final summary counts it and flags RECORDED_AFTER_RUN_END
// (LIMITED, rule 06), and the record can still be corrected keeping its own event time; a
// correction moved to another instant after the end is refused.
func TestWIS701ShiftRunClockSkewRecord(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	now0 := f.clock()
	s := f.recordingStation(now0.Add(-time.Hour), now0.Add(time.Hour))
	f.expect(f.authorize(f.lead, s.station, f.operator, workstation.AuthorizationGrant, `"0"`), 201, "")
	records := "/v1/sites/" + f.site + "/workstations/" + s.station + "/records"
	submit := func(body workstation.StationRecordBodyV1) *httptest.ResponseRecorder {
		return f.call(f.operator, "POST", records, uuid.NewString(), "", f.encode(body))
	}
	t0 := f.clock()
	f.decodeRecord(submit(s.entry(workstation.KindAcknowledged, stamp(t0))), 201)
	f.decodeRecord(submit(s.entry(workstation.KindStarted, stamp(t0.Add(time.Millisecond)))), 201)
	ahead := s.entry(workstation.KindQuantity, stamp(t0.Add(2*time.Minute)))
	ahead.GoodQuantity, ahead.RejectQuantity = pointer(int64(12)), pointer(int64(0))
	original := f.decodeRecord(submit(ahead), 201)
	ended := f.decodeRun(f.call(f.lead, "POST", f.runPath(s.plan.ID)+"/end", uuid.NewString(), etag(f.getPlan(s.plan.ID).StateRevision),
		f.encode(workstation.ShiftRunEndInputV1{SchemaVersion: 1, NextShift: nextShift(s)})), 200)
	if !original.EventAt.After(*ended.EndedAt) {
		t.Fatalf("fixture: event %v must lie after the end %v", original.EventAt, ended.EndedAt)
	}
	readSummary := func(revision int64) workstation.ShiftSummarySnapshotV1 {
		t.Helper()
		var out workstation.ShiftSummarySnapshotV1
		w := f.call(f.operator, "GET", "/v1/sites/"+f.site+"/shift-plans/"+s.plan.ID+"/summaries/"+strconv.FormatInt(revision, 10), "", "", "")
		f.expect(w, 200, "")
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	final := readSummary(ended.Summary.Revision).Summary
	if final.Readiness == workstation.SummaryReady || !slices.Contains(final.Reasons, workstation.ReasonAfterRunEnd) || final.SourceRecordCount != 3 ||
		final.FormulaVersion != workstation.ShiftSummaryRunFormulaVersion || *final.Stations[0].Recorded.GoodQuantity != 12 {
		t.Fatalf("final summary %s %v count %d version %d", final.Readiness, final.Reasons, final.SourceRecordCount, final.FormulaVersion)
	}
	moved := ahead
	moved.GoodQuantity, moved.CorrectsRecordID, moved.EventAt = pointer(int64(11)), original.ID, stamp(original.EventAt.Add(time.Second))
	f.expect(submit(moved), 409, "SHIFT_RUN_ENDED")
	fix := ahead
	fix.GoodQuantity, fix.CorrectsRecordID = pointer(int64(11)), original.ID
	f.decodeRecord(submit(fix), 201)
	run := f.decodeRun(f.call(f.operator, "GET", f.runPath(s.plan.ID), "", "", ""), 200)
	if !run.SummaryRevised || *run.LatestSummaryRevision != ended.Summary.Revision+1 {
		t.Fatalf("run after the correction %+v", run)
	}
	if revised := readSummary(*run.LatestSummaryRevision).Summary; *revised.Stations[0].Recorded.GoodQuantity != 11 || !slices.Contains(revised.Reasons, workstation.ReasonAfterRunEnd) {
		t.Fatalf("revised summary %v good %d", revised.Reasons, *revised.Stations[0].Recorded.GoodQuantity)
	}
}
