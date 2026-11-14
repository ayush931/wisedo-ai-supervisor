//go:build integration

package httpapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/workstation"
	"github.com/google/uuid"
)

// TestShiftSummaryHTTPPostgres proves WIS-520 acceptance item 5 against PostgreSQL, manual
// entry only: a shift summary over two workstations separates planned from recorded figures,
// reports a corrected cycle time by its correction, a recorded quality hold as recorded, an
// open block as open, a late entry and attributed notes; a workstation without records is
// NOT_READY, never zero. A snapshot reads back equal to an independent recomputation from
// the API's own records, later data appends a revision and leaves the earlier one unchanged,
// and generation is owner/supervisor only and tenant-scoped.
func TestShiftSummaryHTTPPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	now0 := f.clock()
	line, stationA, stationB := uuid.NewString(), uuid.NewString(), uuid.NewString()
	source, sha, _ := f.instruction("Synthetic SOP: press and gauge bore", true)
	for _, station := range []string{stationA, stationB} {
		profile := workstation.StationProfileBodyV1{SchemaVersion: 1, WorkstationID: station, LineID: line, DisplayName: "Press", Template: "production",
			Fields: []workstation.ProfileFieldV1{{Name: "bore", Label: "Bore", Kind: "decimal", Unit: "mm", Min: "9.95", Max: "10.05"}}}
		f.expect(f.call(f.lead, "POST", "/v1/sites/"+f.site+"/workstations/"+station+"/profile-revisions", uuid.NewString(), `"0"`, f.encode(profile)), 201, "")
		f.expect(f.authorize(f.lead, station, f.operator, workstation.AuthorizationGrant, `"0"`), 201, "")
	}
	orderA, orderB := f.jobOrder(f.lead, orderBody(source, sha, 100)), f.jobOrder(f.lead, orderBody(source, sha, 50))
	starts, ends := now0.Add(-time.Hour), now0.Add(10*time.Minute)
	planBody := func(targetA int64) workstation.ShiftPlanBodyV1 {
		return workstation.ShiftPlanBodyV1{SchemaVersion: 1, LineID: line, StartsAt: stamp(starts), EndsAt: stamp(ends), Timezone: "UTC",
			Assignments: []workstation.AssignmentV1{pin(stationA, orderA, targetA), pin(stationB, orderB, 50)}}
	}
	plans := "/v1/sites/" + f.site + "/shift-plans"
	decodePlan := func(w *httptest.ResponseRecorder, status int) workstation.ShiftPlanV1 {
		t.Helper()
		f.expect(w, status, "")
		var p workstation.ShiftPlanV1
		if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	plan := decodePlan(f.call(f.lead, "POST", plans, uuid.NewString(), "", f.encode(planBody(100))), 201)
	summaries := plans + "/" + plan.ID + "/summaries"
	generate := func(actor stationSession, key string) *httptest.ResponseRecorder {
		return f.call(actor, "POST", summaries, key, "", `{"schema_version":1}`)
	}
	// Nothing is summarized against a draft.
	f.expect(generate(f.lead, uuid.NewString()), 409, "SHIFT_PLAN_NOT_APPROVED")
	f.expect(f.call(f.approver, "POST", plans+"/"+plan.ID+"/approvals", uuid.NewString(), `"1"`, f.encode(workstation.ShiftPlanApproveInputV1{CanonicalPayloadSHA256: plan.Revisions[0].CanonicalPayloadSHA256})), 200, "")
	f.startRun(f.lead, plan.ID)

	// Station A records a full manual run; station B records nothing.
	testStart := f.clock()
	at := func(second int) string { return stamp(testStart.Add(time.Duration(second) * time.Second)) }
	records := "/v1/sites/" + f.site + "/workstations/" + stationA + "/records"
	submit := func(body workstation.StationRecordBodyV1) workstation.StationRecordV1 {
		t.Helper()
		return f.decodeRecord(f.call(f.operator, "POST", records, uuid.NewString(), "", f.encode(body)), 201)
	}
	entry := func(kind workstation.RecordKind, when string) workstation.StationRecordBodyV1 {
		return workstation.StationRecordBodyV1{SchemaVersion: 1, Kind: kind, WorkstationID: stationA, JobOrderID: orderA.ID, JobOrderRevision: 1, EventAt: when}
	}
	submit(entry(workstation.KindAcknowledged, at(1)))
	submit(entry(workstation.KindStarted, at(2)))
	quantity := entry(workstation.KindQuantity, at(3))
	quantity.GoodQuantity, quantity.RejectQuantity = pointer(int64(40)), pointer(int64(2))
	q1 := submit(quantity)
	cycle := entry(workstation.KindCycleTime, at(4))
	cycle.CycleTimeSeconds = "2.75"
	c1 := submit(cycle)
	cycle.CycleTimeSeconds, cycle.CorrectsRecordID = "2.65", c1.ID
	c2 := submit(cycle)
	measure := entry(workstation.KindQualityMeasurement, at(6))
	measure.Values = []workstation.EntryValueV1{{Field: "bore", Value: "10.01", Unit: "mm"}}
	m1 := submit(measure)
	hold := entry(workstation.KindQualityDisposition, at(7))
	hold.Disposition, hold.AuthorizesRelease, hold.Comment = workstation.DispositionHold, pointer(false), "Hold tag on bin 3"
	held := submit(hold)
	material := entry(workstation.KindMaterialException, at(8))
	material.MaterialCode, material.MaterialIssue = "COIL-7", workstation.MaterialShortage
	mat := submit(material)
	pause := entry(workstation.KindPaused, at(9))
	pause.Comment = "Coil change"
	paused := submit(pause)
	resumed := submit(entry(workstation.KindResumed, at(10)))
	block := entry(workstation.KindBlocked, at(11))
	block.Comment = "Waiting for coil"
	blocked := submit(block)
	note := entry(workstation.KindNote, at(12))
	note.Comment = "Die looks worn"
	noted := submit(note)

	// Re-approve with a new target; a note for an instant before that approval is late.
	tLate := f.clock()
	revised := decodePlan(f.call(f.lead, "POST", plans+"/"+plan.ID+"/revisions", uuid.NewString(), `"2"`, f.encode(planBody(90))), 201)
	f.expect(f.call(f.approver, "POST", plans+"/"+plan.ID+"/approvals", uuid.NewString(), `"3"`, f.encode(workstation.ShiftPlanApproveInputV1{CanonicalPayloadSHA256: revised.Revisions[1].CanonicalPayloadSHA256})), 200, "")
	lateNote := entry(workstation.KindNote, stamp(tLate))
	lateNote.Comment = "Late: shift lead informed"
	late := submit(lateNote)
	if !late.Late || late.PlanRevision != 1 {
		t.Fatalf("late note %+v", late)
	}

	// Generation is for owners and supervisors; any member reads; other tenants see nothing.
	f.expect(generate(f.operator, uuid.NewString()), 403, "PERMISSION_DENIED")
	f.expect(generate(f.viewer, uuid.NewString()), 403, "PERMISSION_DENIED")
	f.expect(generate(f.outsider, uuid.NewString()), 404, "NOT_FOUND")
	f.expect(f.call(f.lead, "POST", summaries, uuid.NewString(), "", `{"schema_version":1,"tenant_id":"`+f.otherTenant+`"}`), 422, "INVALID_SHIFT_SUMMARY_REQUEST")
	f.expect(f.call(f.lead, "POST", summaries, uuid.NewString(), "", `{"schema_version":2}`), 422, "INVALID_SHIFT_SUMMARY_REQUEST")
	f.expect(f.call(f.lead, "POST", summaries, "", "", `{"schema_version":1}`), 422, "INVALID_REQUEST")

	decodeSnapshot := func(w *httptest.ResponseRecorder, status int) workstation.ShiftSummarySnapshotV1 {
		t.Helper()
		f.expect(w, status, "")
		var s workstation.ShiftSummarySnapshotV1
		if err := json.Unmarshal(w.Body.Bytes(), &s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	first := decodeSnapshot(generate(f.lead, "summary-1"), 201)
	s := first.Summary
	if first.Revision != 1 || first.Replayed || first.GeneratedBy.Subject != f.lead.subject || len(first.CanonicalPayloadSHA256) != 64 || s.PlanRevision != 2 ||
		s.PlanRevisionSHA256 != revised.Revisions[1].CanonicalPayloadSHA256 || s.ShiftEnded || s.Readiness != workstation.SummaryNotReady ||
		!reflect.DeepEqual(s.Reasons, []workstation.SummaryReason{workstation.ReasonCorrections, workstation.ReasonLateEntries, workstation.ReasonNoRecords,
			workstation.ReasonShiftInProgress, workstation.ReasonSupersededBinding, workstation.ReasonUnclosedDowntime}) || s.SourceRecordCount != 13 ||
		s.Jobs != (workstation.ShiftJobCountsV1{Planned: 2, InProgress: 1, Unknown: 1, Remaining: 2}) || len(s.Stations) != 2 {
		t.Fatalf("snapshot: %s", f.encode(first))
	}
	var a, b workstation.StationSummaryV1
	for _, st := range s.Stations {
		if st.WorkstationID == stationA {
			a = st
		} else {
			b = st
		}
	}
	// Planned figures come from the approved revision; recorded figures from the records.
	r := a.Recorded
	if a.Planned == nil || a.Planned.TargetQuantity != 90 || *a.Planned.TargetCycleTimeSeconds != "2.5" || a.Planned.JobOrderRevision != 1 ||
		*r.GoodQuantity != 40 || *r.RejectQuantity != 2 || *r.RemainingQuantity != 50 || r.OverProduction || !reflect.DeepEqual(r.QuantityRecordIDs, []string{q1.ID}) ||
		r.CompletionState != workstation.CompletionInProgress || r.CompletionRecordID != nil {
		t.Fatalf("station A planned/quantities: %s", f.encode(a))
	}
	if !reflect.DeepEqual(*r.CycleTime, workstation.CycleTimeStatsV1{Count: 1, MinSeconds: "2.65", MaxSeconds: "2.65", SumSeconds: "2.65", MeanSeconds: "2.65", RecordIDs: []string{c2.ID}}) ||
		!reflect.DeepEqual(r.Corrections, []workstation.CorrectionChainV1{{RecordID: c2.ID, Supersedes: []string{c1.ID}}}) {
		t.Fatalf("station A cycle time: %s", f.encode(r))
	}
	if r.Quality.MeasurementCount != 1 || r.Quality.MeasurementRecordIDs[0] != m1.ID || r.Quality.HoldsAsRecorded != 1 || len(r.Quality.DispositionsAsRecorded) != 1 ||
		r.Quality.DispositionsAsRecorded[0].RecordID != held.ID || r.Quality.DispositionsAsRecorded[0].RecordedBy.Subject != f.operator.subject ||
		len(r.MaterialExceptions) != 1 || r.MaterialExceptions[0].RecordID != mat.ID || r.MaterialExceptions[0].MaterialCode != "COIL-7" {
		t.Fatalf("station A quality/material: %s", f.encode(r))
	}
	if len(r.Downtime) != 2 || r.Downtime[0].Open || r.Downtime[0].StartRecordID != paused.ID || *r.Downtime[0].EndRecordID != resumed.ID || *r.Downtime[0].DurationSeconds != "1" ||
		!r.Downtime[1].Open || r.Downtime[1].StartRecordID != blocked.ID || r.Downtime[1].EndsAt != nil || r.Downtime[1].DurationSeconds != nil || r.ClosedDowntime != "1" {
		t.Fatalf("station A downtime: %s", f.encode(r.Downtime))
	}
	if !reflect.DeepEqual(r.LateRecordIDs, []string{late.ID}) || !reflect.DeepEqual(a.BoundPlanRevisions, []int64{1}) || a.Readiness != workstation.SummaryLimited ||
		!reflect.DeepEqual(a.Reasons, []workstation.SummaryReason{workstation.ReasonUnclosedDowntime, workstation.ReasonLateEntries, workstation.ReasonSupersededBinding, workstation.ReasonCorrections}) {
		t.Fatalf("station A readiness: %s %v late %v bound %v", a.Readiness, a.Reasons, r.LateRecordIDs, a.BoundPlanRevisions)
	}
	if b.Readiness != workstation.SummaryNotReady || !reflect.DeepEqual(b.Reasons, []workstation.SummaryReason{workstation.ReasonNoRecords}) || b.Recorded.GoodQuantity != nil ||
		b.Recorded.RemainingQuantity != nil || b.Recorded.CycleTime != nil || b.Recorded.CompletionState != workstation.CompletionUnknown || b.Planned.TargetQuantity != 50 {
		t.Fatalf("station B: %s", f.encode(b))
	}
	// Notes are attributed, flagged late where late, and nowhere among measured figures.
	notes := map[string]workstation.SummaryNoteV1{}
	for _, n := range s.Notes {
		notes[n.RecordID] = n
	}
	if len(s.Notes) != 2 || notes[noted.ID].Late || !notes[late.ID].Late || notes[noted.ID].RecordedBy.Subject != f.operator.subject ||
		notes[noted.ID].Comment != "Die looks worn" || notes[late.ID].Comment != "Late: shift lead informed" || notes[noted.ID].WorkstationID != stationA {
		t.Fatalf("notes: %s", f.encode(s.Notes))
	}
	// The figures agree with the persisted records: effective (uncorrected) rows only.
	var good, reject int64
	var cycleSeconds string
	if err := f.control.QueryRowContext(ctx, `SELECT sum((payload->>'good_quantity')::bigint),sum((payload->>'reject_quantity')::bigint),
 (SELECT payload->>'cycle_time_seconds' FROM supervisor.station_records r WHERE r.workstation_id=$1 AND r.kind='cycle_time' AND NOT EXISTS(SELECT 1 FROM supervisor.station_records c WHERE c.corrects_record_id=r.id))
 FROM supervisor.station_records r WHERE r.workstation_id=$1 AND r.kind='quantity' AND NOT EXISTS(SELECT 1 FROM supervisor.station_records c WHERE c.corrects_record_id=r.id)`, stationA).Scan(&good, &reject, &cycleSeconds); err != nil ||
		good != *r.GoodQuantity || reject != *r.RejectQuantity || cycleSeconds != r.CycleTime.SumSeconds {
		t.Fatalf("persisted good=%d reject=%d cycle=%s err=%v", good, reject, cycleSeconds, err)
	}

	// Duplicate submission replays.
	if replay := decodeSnapshot(generate(f.lead, "summary-1"), 201); !replay.Replayed || replay.Revision != 1 || replay.CanonicalPayloadSHA256 != first.CanonicalPayloadSHA256 {
		t.Fatalf("replay %+v", replay)
	}
	if !first.Summary.AsOf.After(testStart) || !first.Summary.AsOf.Before(ends) {
		t.Fatalf("in-shift as_of %s", first.Summary.AsOf)
	}

	// An ended shift is summarized as of its end, so regenerating identical inputs later is
	// refused as unchanged.
	past := decodePlan(f.call(f.lead, "POST", plans, uuid.NewString(), "", f.encode(workstation.ShiftPlanBodyV1{SchemaVersion: 1, LineID: line,
		StartsAt: stamp(now0.Add(-3 * time.Hour)), EndsAt: stamp(now0.Add(-2 * time.Hour)), Timezone: "UTC", Assignments: []workstation.AssignmentV1{pin(stationB, orderB, 20)}})), 201)
	f.expect(f.call(f.approver, "POST", plans+"/"+past.ID+"/approvals", uuid.NewString(), `"1"`, f.encode(workstation.ShiftPlanApproveInputV1{CanonicalPayloadSHA256: past.Revisions[0].CanonicalPayloadSHA256})), 200, "")
	pastSummaries := plans + "/" + past.ID + "/summaries"
	ended := decodeSnapshot(f.call(f.lead, "POST", pastSummaries, uuid.NewString(), "", `{"schema_version":1}`), 201)
	if !ended.Summary.ShiftEnded || !ended.Summary.AsOf.Equal(now0.Add(-2*time.Hour)) || ended.Summary.Readiness != workstation.SummaryNotReady ||
		!reflect.DeepEqual(ended.Summary.Reasons, []workstation.SummaryReason{workstation.ReasonNoRecords}) {
		t.Fatalf("ended shift summary: %s", f.encode(ended.Summary))
	}
	f.expect(f.call(f.lead, "POST", pastSummaries, uuid.NewString(), "", `{"schema_version":1}`), 409, "SHIFT_SUMMARY_UNCHANGED")
	f.expect(f.call(f.owner, "POST", pastSummaries, uuid.NewString(), "", `{"schema_version":1}`), 409, "SHIFT_SUMMARY_UNCHANGED")

	// Readback equals the response, and an independent recomputation from the plan and the
	// records the API serves yields the same body and digest.
	readback := decodeSnapshot(f.call(f.viewer, "GET", summaries+"/1", "", "", ""), 200)
	first.Replayed = false
	if !reflect.DeepEqual(readback, first) {
		t.Fatalf("readback differs:\n%s\n%s", f.encode(readback), f.encode(first))
	}
	current := decodePlan(f.call(f.viewer, "GET", plans+"/"+plan.ID, "", "", ""), 200)
	var served []workstation.StationRecordV1
	for _, station := range []string{stationA, stationB} {
		var page workstation.StationRecordPageV1
		w := f.call(f.viewer, "GET", "/v1/sites/"+f.site+"/workstations/"+station+"/records?limit=100", "", "", "")
		f.expect(w, 200, "")
		if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || page.NextCursor != "" {
			t.Fatalf("records page: %v %s", err, page.NextCursor)
		}
		served = append(served, page.Records...)
	}
	recomputed, _, digest, err := workstation.SummarizeShift(workstation.Scope{TenantID: f.tenant, SiteID: f.site}, workstation.ShiftSummaryInput{PlanID: current.ID, LineID: current.LineID,
		StartsAt: current.StartsAt, EndsAt: current.EndsAt, AsOf: first.Summary.AsOf, Approved: current.Revisions[*current.ApprovedRevision-1], Records: served})
	if err != nil || digest != first.CanonicalPayloadSHA256 || !reflect.DeepEqual(recomputed, first.Summary) {
		t.Fatalf("recomputation %s (%v) vs snapshot %s:\n%s\n%s", digest, err, first.CanonicalPayloadSHA256, f.encode(recomputed), f.encode(first.Summary))
	}

	// Later data appends revision 2; revision 1 is unchanged.
	more := entry(workstation.KindQuantity, at(13))
	more.GoodQuantity, more.RejectQuantity = pointer(int64(5)), pointer(int64(0))
	submit(more)
	second := decodeSnapshot(generate(f.lead, "summary-2"), 201)
	if second.Revision != 2 || second.CanonicalPayloadSHA256 == first.CanonicalPayloadSHA256 {
		t.Fatalf("second snapshot %d %s", second.Revision, second.CanonicalPayloadSHA256)
	}
	for _, st := range second.Summary.Stations {
		if st.WorkstationID == stationA && (*st.Recorded.GoodQuantity != 45 || *st.Recorded.RemainingQuantity != 45) {
			t.Fatalf("second snapshot station A: %s", f.encode(st.Recorded))
		}
	}
	if again := decodeSnapshot(f.call(f.viewer, "GET", summaries+"/1", "", "", ""), 200); !reflect.DeepEqual(again, first) {
		t.Fatalf("revision 1 changed:\n%s", f.encode(again))
	}
	var page workstation.ShiftSummaryPageV1
	w := f.call(f.operator, "GET", summaries+"?limit=1", "", "", "")
	f.expect(w, 200, "")
	if err = json.Unmarshal(w.Body.Bytes(), &page); err != nil || len(page.Summaries) != 1 || page.Summaries[0].Revision != 1 || page.NextCursor != "1" ||
		page.Summaries[0].Readiness != workstation.SummaryNotReady || page.Summaries[0].CanonicalPayloadSHA256 != first.CanonicalPayloadSHA256 {
		t.Fatalf("page: %s", w.Body.String())
	}
	w = f.call(f.operator, "GET", summaries+"?cursor=1", "", "", "")
	f.expect(w, 200, "")
	page = workstation.ShiftSummaryPageV1{}
	if err = json.Unmarshal(w.Body.Bytes(), &page); err != nil || len(page.Summaries) != 1 || page.Summaries[0].Revision != 2 || page.NextCursor != "" {
		t.Fatalf("page 2: %s", w.Body.String())
	}
	f.expect(f.call(f.operator, "GET", summaries+"?cursor=x", "", "", ""), 422, "INVALID_SHIFT_SUMMARY_REQUEST")
	// The same key on another plan's path is a different request.
	f.expect(f.call(f.lead, "POST", plans+"/"+uuid.NewString()+"/summaries", "summary-2", "", `{"schema_version":1}`), 409, "IDEMPOTENCY_CONFLICT")

	// Non-disclosing reads across tenants and for unknown snapshots.
	f.expect(f.call(f.outsider, "GET", summaries+"/1", "", "", ""), 404, "NOT_FOUND")
	f.expect(f.call(f.outsider, "GET", summaries, "", "", ""), 404, "NOT_FOUND")
	f.expect(f.call(f.viewer, "GET", summaries+"/3", "", "", ""), 404, "NOT_FOUND")
	f.expect(f.call(f.viewer, "GET", summaries+"/one", "", "", ""), 404, "NOT_FOUND")
	f.expect(f.call(f.viewer, "GET", plans+"/"+uuid.NewString()+"/summaries", "", "", ""), 404, "NOT_FOUND")

	// The runtime role cannot rewrite, back-fill or cross tenants.
	f.refused(f.tenant, "updated a snapshot", "42501", "permission denied", `UPDATE supervisor.shift_summaries SET plan_revision=1 WHERE plan_id=$1`, plan.ID)
	f.refused(f.tenant, "deleted a snapshot", "42501", "permission denied", `DELETE FROM supervisor.shift_summaries WHERE plan_id=$1`, plan.ID)
	f.refused(f.tenant, "skipped a revision", "23514", "not the next revision", `INSERT INTO supervisor.shift_summaries(tenant_id,site_id,plan_id,revision,plan_revision,payload,canonical_payload,canonical_payload_sha256,digest_schema_version,actor_issuer,actor_subject)
 SELECT tenant_id,site_id,plan_id,5,plan_revision,payload,canonical_payload,canonical_payload_sha256,digest_schema_version,actor_issuer,actor_subject FROM supervisor.shift_summaries WHERE plan_id=$1 AND revision=1`, plan.ID)
	f.refused(f.otherTenant, "wrote another tenant's snapshot", "42501", "tenant scope denied", `INSERT INTO supervisor.shift_summaries(tenant_id,site_id,plan_id,revision,plan_revision,payload,canonical_payload,canonical_payload_sha256,digest_schema_version,actor_issuer,actor_subject)
 VALUES($1,$2,$3,3,2,'{}','{}',encode(sha256(convert_to('{}','UTF8')),'hex'),1,'x','y')`, f.tenant, f.site, plan.ID)
	var visible int
	if err = f.runtime.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.shift_summaries`).Scan(&visible); err != nil || visible != 0 {
		t.Fatalf("runtime without tenant context sees %d snapshots (%v)", visible, err)
	}
	if !strings.Contains(f.encode(second), `"holds_as_recorded":1`) || strings.Contains(f.encode(second), "release") {
		t.Fatalf("a recorded hold must stay a recorded hold: %s", f.encode(second.Summary.Stations))
	}
}
