//go:build integration

package httpapi

import (
	"context"
	"encoding/json"
	"math/big"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/workstation"
	"github.com/google/uuid"
)

// TestWIS520ScenarioMatrixHTTPPostgres proves WIS-520 acceptance items 9 and 7 in one
// continuous flow through the public API against PostgreSQL: two workstations with different
// screen configurations, two consecutive shifts, a partial order carried forward, a quality
// hold, a duplicate submission, a corrected cycle-time entry, a stale source and an operator
// lacking station authorization. Tablet readback and the shift summary are checked against
// the persisted rows; direct SQL is used only to read them back.
//
// Item 7 (manual only): the handler is built from a config with no Temporal address, no model
// client and no connector (httpapi.New takes nothing else), the tenant has no operational
// source, and every stored record is a manual tablet entry.
//
// Stale source: the summary's STALE_INPUT reason uses a fixed 30-minute policy window before
// the shift end (workstation.StaleInputWindow, not a request parameter), which a real-time
// test cannot reach because a record's event time must follow the plan approval. STALE_INPUT
// is covered only by the kernel unit test TestSummarizeShiftReadiness. Here the stale cases
// that are reachable are asserted: a station with no inputs is NOT_READY with NO_RECORDS
// (never zero), and an instruction superseded after delivery stops the tablet showing the
// order and refuses new entries.
func TestWIS520ScenarioMatrixHTTPPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := f.control.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(`SELECT count(*) FROM supervisor.operational_source_heads WHERE tenant_id=$1`, f.tenant); n != 0 {
		t.Fatalf("manual-only scenario starts with %d operational sources", n)
	}
	now0 := f.clock()
	// Shift 1 records lie before the handover; the margin absorbs slow setup under -race and
	// stays under the five-minute tablet clock skew so shift 2 entries at the handover are
	// accepted now.
	handover, shift2End := now0.Add(150*time.Second), now0.Add(time.Hour)
	site := "/v1/sites/" + f.site
	plans := site + "/shift-plans"
	line, stationA, stationB := uuid.NewString(), uuid.NewString(), uuid.NewString()

	// Instruction evidence confirmed current; two job orders, one per screen template.
	source, sha, _ := f.instruction("Synthetic SOP: press, gauge bore, torque check", true)
	orderA := f.jobOrder(f.lead, orderBody(source, sha, 100))
	bodyB := orderBody(source, sha, 50)
	bodyB.Template, bodyB.Fields = "quality", []workstation.FieldV1{{Name: "torque", Label: "Torque", Kind: "decimal", Required: true, Unit: "Nm"}}
	orderB := f.jobOrder(f.lead, bodyB)

	// Two station profiles: a production screen and a quality screen.
	profiles := map[string]workstation.StationProfileBodyV1{
		stationA: {SchemaVersion: 1, WorkstationID: stationA, LineID: line, DisplayName: "Press 4", Template: "production", Fields: []workstation.ProfileFieldV1{
			{Name: "good_units", Label: "Good units", Kind: "decimal", Required: true, Unit: "count"},
			{Name: "cycle_time", Label: "Cycle time", Kind: "decimal", Required: true, Unit: "s"},
			{Name: "bore", Label: "Bore", Kind: "decimal", Unit: "mm", Min: "9.95", Max: "10.05"}}},
		stationB: {SchemaVersion: 1, WorkstationID: stationB, LineID: line, DisplayName: "Torque cell 2", Template: "quality", Fields: []workstation.ProfileFieldV1{
			{Name: "torque", Label: "Torque", Kind: "decimal", Required: true, Unit: "Nm", Min: "24", Max: "26"},
			{Name: "visual", Label: "Visual", Kind: "choice", Choices: []string{"ok", "burr"}}}},
	}
	for station, profile := range profiles {
		f.expect(f.call(f.lead, "POST", site+"/workstations/"+station+"/profile-revisions", uuid.NewString(), `"0"`, f.encode(profile)), 201, "")
	}
	// Screen readback equals the persisted profile revision, and the two screens differ.
	for station, want := range profiles {
		w := f.call(f.operator, "GET", site+"/workstations/"+station+"/profile", "", "", "")
		f.expect(w, 200, "")
		var got workstation.StationProfileV1
		var stored workstation.StationProfileBodyV1
		var raw []byte
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || got.CurrentRevision != 1 {
			t.Fatalf("profile %s: %v %s", station, err, w.Body.String())
		}
		if err := f.control.QueryRowContext(ctx, `SELECT payload FROM supervisor.station_profile_revisions WHERE tenant_id=$1 AND workstation_id=$2 AND revision=1`, f.tenant, station).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &stored); err != nil || !reflect.DeepEqual(got.Revisions[0].Profile, stored) || !reflect.DeepEqual(stored, want) {
			t.Fatalf("profile %s readback %s vs persisted %s", station, f.encode(got.Revisions[0].Profile), raw)
		}
	}
	if reflect.DeepEqual(profiles[stationA].Fields, profiles[stationB].Fields) || profiles[stationA].Template == profiles[stationB].Template {
		t.Fatal("the two stations must have different screen configurations")
	}

	// Shift 1: created by supervisor A (lead), approved by supervisor B (approver).
	decodePlan := func(w *httptest.ResponseRecorder, status int) workstation.ShiftPlanV1 {
		t.Helper()
		f.expect(w, status, "")
		var p workstation.ShiftPlanV1
		if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	approve := func(actor stationSession, plan workstation.ShiftPlanV1) *httptest.ResponseRecorder {
		return f.call(actor, "POST", plans+"/"+plan.ID+"/approvals", uuid.NewString(), `"`+strconv.FormatInt(plan.StateRevision, 10)+`"`,
			f.encode(workstation.ShiftPlanApproveInputV1{CanonicalPayloadSHA256: plan.Revisions[plan.CurrentRevision-1].CanonicalPayloadSHA256}))
	}
	shift1 := decodePlan(f.call(f.lead, "POST", plans, uuid.NewString(), "", f.encode(workstation.ShiftPlanBodyV1{SchemaVersion: 1, LineID: line,
		StartsAt: stamp(now0.Add(-time.Hour)), EndsAt: stamp(handover), Timezone: "UTC", Assignments: []workstation.AssignmentV1{pin(stationA, orderA, 100), pin(stationB, orderB, 50)}})), 201)
	decodePlan(approve(f.approver, shift1), 200)
	f.startRun(f.lead, shift1.ID)

	// Operators are granted per station; an operator without a grant is refused.
	f.expect(f.authorize(f.lead, stationA, f.operator, workstation.AuthorizationGrant, `"0"`), 201, "")
	f.expect(f.authorize(f.lead, stationB, f.spare, workstation.AuthorizationGrant, `"0"`), 201, "")
	t1 := f.clock()
	at := func(ms int) string { return stamp(t1.Add(time.Duration(ms) * time.Millisecond)) }
	recordsURL := func(station string) string { return site + "/workstations/" + station + "/records" }
	entry := func(station string, order workstation.JobOrderV1, kind workstation.RecordKind, when string) workstation.StationRecordBodyV1 {
		return workstation.StationRecordBodyV1{SchemaVersion: 1, Kind: kind, WorkstationID: station, JobOrderID: order.ID, JobOrderRevision: 1, EventAt: when}
	}
	f.expect(f.call(f.spare, "POST", recordsURL(stationA), uuid.NewString(), "", f.encode(entry(stationA, orderA, workstation.KindAcknowledged, at(0)))), 403, "STATION_NOT_AUTHORIZED")
	f.expect(f.call(f.operator, "POST", recordsURL(stationB), uuid.NewString(), "", f.encode(entry(stationB, orderB, workstation.KindAcknowledged, at(0)))), 403, "STATION_NOT_AUTHORIZED")
	if n := count(`SELECT count(*) FROM supervisor.station_records WHERE tenant_id=$1`, f.tenant); n != 0 {
		t.Fatalf("refused entries stored %d rows", n)
	}

	// Tablet readback for both stations equals the persisted job-order revision.
	readback := func(actor stationSession, station, when string) (workstation.CurrentJobOrderV1, string) {
		t.Helper()
		path := site + "/workstations/" + station + "/current-job-order"
		if when != "" {
			path += "?at=" + url.QueryEscape(when)
		}
		w := f.call(actor, "GET", path, "", "", "")
		f.expect(w, 200, "")
		var c workstation.CurrentJobOrderV1
		if err := json.Unmarshal(w.Body.Bytes(), &c); err != nil {
			t.Fatal(err)
		}
		return c, w.Body.String()
	}
	matchesPersisted := func(c workstation.CurrentJobOrderV1, order workstation.JobOrderV1) {
		t.Helper()
		var raw []byte
		var digest, submitter string
		var head int64
		if err := f.control.QueryRowContext(ctx, `SELECT r.payload,r.canonical_payload_sha256,r.actor_subject,h.current_revision FROM supervisor.job_order_revisions r
 JOIN supervisor.job_order_heads h ON (h.tenant_id,h.site_id,h.id)=(r.tenant_id,r.site_id,r.job_order_id) WHERE r.tenant_id=$1 AND r.job_order_id=$2 AND r.revision=$3`,
			f.tenant, order.ID, c.PinnedRevision).Scan(&raw, &digest, &submitter, &head); err != nil {
			t.Fatal(err)
		}
		var stored workstation.JobOrderBodyV1
		if err := json.Unmarshal(raw, &stored); err != nil || c.Order == nil || !reflect.DeepEqual(c.Order.Order, stored) || c.Order.CanonicalPayloadSHA256 != digest ||
			c.Order.Revision != head || c.CurrentRevision != head || c.Order.SubmittedBy.Subject != submitter {
			t.Fatalf("tablet readback %s differs from persisted revision %d %s (%s)", f.encode(c), head, raw, digest)
		}
	}
	for station, view := range map[string]struct {
		actor  stationSession
		order  workstation.JobOrderV1
		target int64
	}{stationA: {f.operator, orderA, 100}, stationB: {f.spare, orderB, 50}} {
		c, body := readback(view.actor, station, "")
		if c.State != workstation.DeliveryCurrent || c.PlanID != shift1.ID || c.PlanRevision != 1 || c.JobOrderID != view.order.ID || c.TargetQuantity != view.target {
			t.Fatalf("shift 1 readback %s: %s", station, body)
		}
		matchesPersisted(c, view.order)
	}

	// Station A: a partial run with a quality hold, a duplicate submission and a corrected
	// cycle time. Station B (quality screen): a complete run with its torque evidence.
	submit := func(actor stationSession, station, key string, body workstation.StationRecordBodyV1) workstation.StationRecordV1 {
		t.Helper()
		return f.decodeRecord(f.call(actor, "POST", recordsURL(station), key, "", f.encode(body)), 201)
	}
	a := func(kind workstation.RecordKind, ms int) workstation.StationRecordBodyV1 {
		return entry(stationA, orderA, kind, at(ms))
	}
	b := func(kind workstation.RecordKind, ms int) workstation.StationRecordBodyV1 {
		return entry(stationB, orderB, kind, at(ms))
	}
	submit(f.operator, stationA, uuid.NewString(), a(workstation.KindAcknowledged, 1))
	submit(f.operator, stationA, uuid.NewString(), a(workstation.KindStarted, 2))
	quantity := a(workstation.KindQuantity, 3)
	quantity.GoodQuantity, quantity.RejectQuantity = pointer(int64(40)), pointer(int64(2))
	q1 := submit(f.operator, stationA, "qty-a", quantity)
	if replay := submit(f.operator, stationA, "qty-a", quantity); !replay.Replayed || replay.ID != q1.ID || !replay.ReceivedAt.Equal(q1.ReceivedAt) {
		t.Fatalf("duplicate submission must replay %s: %s", q1.ID, f.encode(replay))
	}
	different := quantity
	different.GoodQuantity = pointer(int64(41))
	f.expect(f.call(f.operator, "POST", recordsURL(stationA), "qty-a", "", f.encode(different)), 409, "IDEMPOTENCY_CONFLICT")
	if n := count(`SELECT count(*) FROM supervisor.station_records WHERE tenant_id=$1 AND workstation_id=$2 AND kind='quantity'`, f.tenant, stationA); n != 1 {
		t.Fatalf("duplicate submission stored %d quantity rows", n)
	}
	cycle := a(workstation.KindCycleTime, 4)
	cycle.CycleTimeSeconds = "2.75"
	c1 := submit(f.operator, stationA, uuid.NewString(), cycle)
	cycle.CycleTimeSeconds, cycle.CorrectsRecordID = "2.65", c1.ID
	c2 := submit(f.operator, stationA, uuid.NewString(), cycle)
	second := a(workstation.KindCycleTime, 5)
	second.CycleTimeSeconds = "2.45"
	submit(f.operator, stationA, uuid.NewString(), second)
	measure := a(workstation.KindQualityMeasurement, 6)
	measure.Values = []workstation.EntryValueV1{{Field: "bore", Value: "10.01", Unit: "mm"}}
	m1 := submit(f.operator, stationA, uuid.NewString(), measure)
	hold := a(workstation.KindQualityDisposition, 7)
	hold.Disposition, hold.AuthorizesRelease, hold.Comment = workstation.DispositionHold, pointer(false), "Hold tag on bin 3"
	held := submit(f.operator, stationA, uuid.NewString(), hold)
	submit(f.spare, stationB, uuid.NewString(), b(workstation.KindAcknowledged, 1))
	submit(f.spare, stationB, uuid.NewString(), b(workstation.KindStarted, 2))
	quantityB := b(workstation.KindQuantity, 3)
	quantityB.GoodQuantity, quantityB.RejectQuantity = pointer(int64(50)), pointer(int64(0))
	submit(f.spare, stationB, uuid.NewString(), quantityB)
	cycleB := b(workstation.KindCycleTime, 4)
	cycleB.CycleTimeSeconds = "2.5"
	submit(f.spare, stationB, uuid.NewString(), cycleB)
	torque := b(workstation.KindQualityMeasurement, 5)
	torque.Values = []workstation.EntryValueV1{{Field: "torque", Value: "25.1", Unit: "Nm"}}
	submit(f.spare, stationB, uuid.NewString(), torque)
	done := b(workstation.KindCompletionEvidence, 6)
	done.Values = []workstation.EntryValueV1{{Field: "torque", Value: "25.1", Unit: "Nm"}}
	completion := submit(f.spare, stationB, uuid.NewString(), done)

	// The correction chain and the hold as persisted.
	var predecessor, release string
	if err := f.control.QueryRowContext(ctx, `SELECT corrects_record_id::text,(SELECT payload->>'authorizes_release' FROM supervisor.station_records WHERE id=$2) FROM supervisor.station_records WHERE id=$1`, c2.ID, held.ID).Scan(&predecessor, &release); err != nil ||
		predecessor != c1.ID || release != "false" ||
		count(`SELECT count(*) FROM supervisor.station_records WHERE corrects_record_id=$1`, c1.ID) != 1 || count(`SELECT count(*) FROM supervisor.station_records WHERE id=$1 AND corrects_record_id IS NULL`, c1.ID) != 1 ||
		count(`SELECT count(*) FROM supervisor.station_records WHERE corrects_record_id=$1`, c2.ID) != 0 {
		t.Fatalf("correction chain %s<-%s or hold release %q not as persisted (%v)", c1.ID, predecessor, release, err)
	}
	if !f.clock().Before(handover) {
		t.Fatalf("setup took past the handover at %s", handover)
	}

	// Shift 1 summary, every figure recomputed from the persisted rows.
	decodeSummary := func(w *httptest.ResponseRecorder) workstation.ShiftSummarySnapshotV1 {
		t.Helper()
		f.expect(w, 201, "")
		var s workstation.ShiftSummarySnapshotV1
		if err := json.Unmarshal(w.Body.Bytes(), &s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	summary1 := decodeSummary(f.call(f.lead, "POST", plans+"/"+shift1.ID+"/summaries", uuid.NewString(), "", `{"schema_version":1}`)).Summary
	type persisted struct{ id, station, kind, corrects, payload string }
	rows, err := f.control.QueryContext(ctx, `SELECT id::text,workstation_id::text,kind,coalesce(corrects_record_id::text,''),payload::text FROM supervisor.station_records WHERE tenant_id=$1 AND plan_id=$2`, f.tenant, shift1.ID)
	if err != nil {
		t.Fatal(err)
	}
	var stored []persisted
	for rows.Next() {
		var p persisted
		if err = rows.Scan(&p.id, &p.station, &p.kind, &p.corrects, &p.payload); err != nil {
			t.Fatal(err)
		}
		stored = append(stored, p)
	}
	if err = rows.Close(); err != nil {
		t.Fatal(err)
	}
	superseded := map[string]bool{}
	for _, p := range stored {
		if p.corrects != "" {
			superseded[p.corrects] = true
		}
	}
	rat := func(s string) *big.Rat {
		t.Helper()
		v, ok := new(big.Rat).SetString(s)
		if !ok {
			t.Fatalf("not a decimal: %q", s)
		}
		return v
	}
	type figures struct {
		good, reject, target         int64
		holds, measurements, records int
		completed                    bool
		cycles                       []*big.Rat
		cycleIDs                     []string
	}
	expected := map[string]*figures{stationA: {}, stationB: {}}
	for _, p := range stored {
		e := expected[p.station]
		e.records++
		if superseded[p.id] {
			continue
		}
		var body workstation.StationRecordBodyV1
		if err = json.Unmarshal([]byte(p.payload), &body); err != nil {
			t.Fatal(err)
		}
		switch workstation.RecordKind(p.kind) {
		case workstation.KindQuantity:
			e.good, e.reject = e.good+*body.GoodQuantity, e.reject+*body.RejectQuantity
		case workstation.KindCycleTime:
			e.cycles, e.cycleIDs = append(e.cycles, rat(body.CycleTimeSeconds)), append(e.cycleIDs, p.id)
		case workstation.KindQualityMeasurement:
			e.measurements++
		case workstation.KindQualityDisposition:
			if body.Disposition == workstation.DispositionHold {
				e.holds++
			}
		case workstation.KindCompletionEvidence:
			e.completed = true
		}
	}
	for station, e := range expected {
		if err = f.control.QueryRowContext(ctx, `SELECT (a->>'target_quantity')::bigint FROM supervisor.shift_plan_revisions r,jsonb_array_elements(r.payload->'assignments') a
 WHERE r.tenant_id=$1 AND r.plan_id=$2 AND r.revision=1 AND a->>'workstation_id'=$3`, f.tenant, shift1.ID, station).Scan(&e.target); err != nil {
			t.Fatal(err)
		}
	}
	// Shift-level reasons are code-ordered; station-level ones follow readiness order.
	if summary1.SourceRecordCount != len(stored) || len(stored) != expected[stationA].records+expected[stationB].records || summary1.PlanRevision != 1 || summary1.ShiftEnded ||
		summary1.Readiness != workstation.SummaryLimited || !reflect.DeepEqual(summary1.Reasons, []workstation.SummaryReason{workstation.ReasonCorrections, workstation.ReasonShiftInProgress}) ||
		summary1.Jobs != (workstation.ShiftJobCountsV1{Planned: 2, Completed: 1, InProgress: 1, Remaining: 1}) || len(summary1.Stations) != 2 {
		t.Fatalf("shift 1 summary (%d persisted rows): %s", len(stored), f.encode(summary1))
	}
	stationSummary := func(s workstation.ShiftSummaryV1, station string) workstation.StationSummaryV1 {
		t.Helper()
		for _, st := range s.Stations {
			if st.WorkstationID == station {
				return st
			}
		}
		t.Fatalf("summary has no station %s", station)
		return workstation.StationSummaryV1{}
	}
	for station, e := range expected {
		st := stationSummary(summary1, station)
		r := st.Recorded
		if r.GoodQuantity == nil || r.RejectQuantity == nil || r.RemainingQuantity == nil || *r.GoodQuantity != e.good || *r.RejectQuantity != e.reject ||
			*r.RemainingQuantity != e.target-e.good || st.Planned == nil || st.Planned.TargetQuantity != e.target || len(st.RecordIDs) != e.records ||
			r.Quality.HoldsAsRecorded != e.holds || r.Quality.MeasurementCount != e.measurements || (r.CompletionState == workstation.CompletionCompleted) != e.completed {
			t.Fatalf("station %s figures %+v vs summary %s", station, *e, f.encode(st))
		}
		if len(e.cycles) == 0 {
			t.Fatalf("station %s has no persisted cycle time", station)
		}
		sum, lo, hi := new(big.Rat), e.cycles[0], e.cycles[0]
		for _, v := range e.cycles {
			sum.Add(sum, v)
			if v.Cmp(lo) < 0 {
				lo = v
			}
			if v.Cmp(hi) > 0 {
				hi = v
			}
		}
		mean := new(big.Rat).Quo(sum, big.NewRat(int64(len(e.cycles)), 1))
		if r.CycleTime == nil {
			t.Fatalf("station %s has no cycle time in the summary", station)
		}
		ids := append([]string(nil), r.CycleTime.RecordIDs...)
		sort.Strings(ids)
		sort.Strings(e.cycleIDs)
		if r.CycleTime.Count != len(e.cycles) || rat(r.CycleTime.SumSeconds).Cmp(sum) != 0 || rat(r.CycleTime.MinSeconds).Cmp(lo) != 0 ||
			rat(r.CycleTime.MaxSeconds).Cmp(hi) != 0 || rat(r.CycleTime.MeanSeconds).Cmp(mean) != 0 || !reflect.DeepEqual(ids, e.cycleIDs) {
			t.Fatalf("station %s cycle times %v (mean %s) vs summary %s", station, e.cycles, mean.FloatString(6), f.encode(r.CycleTime))
		}
	}
	sa, sb := stationSummary(summary1, stationA), stationSummary(summary1, stationB)
	if sa.Readiness != workstation.SummaryReady || !reflect.DeepEqual(sa.Reasons, []workstation.SummaryReason{workstation.ReasonCorrections}) || sa.Recorded.CompletionState != workstation.CompletionInProgress ||
		!reflect.DeepEqual(sa.Recorded.Corrections, []workstation.CorrectionChainV1{{RecordID: c2.ID, Supersedes: []string{c1.ID}}}) ||
		len(sa.Recorded.Quality.DispositionsAsRecorded) != 1 || sa.Recorded.Quality.DispositionsAsRecorded[0].RecordID != held.ID || sa.Recorded.Quality.MeasurementRecordIDs[0] != m1.ID {
		t.Fatalf("station A summary: %s", f.encode(sa))
	}
	if sb.Readiness != workstation.SummaryReady || len(sb.Reasons) != 0 || sb.Recorded.CompletionRecordID == nil || *sb.Recorded.CompletionRecordID != completion.ID {
		t.Fatalf("station B summary: %s", f.encode(sb))
	}
	if !reflect.DeepEqual(sa.Recorded.QuantityRecordIDs, []string{q1.ID}) {
		t.Fatalf("station A quantity records: %v", sa.Recorded.QuantityRecordIDs)
	}

	// Handover: generated from the summary without touching any shift plan row.
	planState := func() string {
		t.Helper()
		var out []string
		for _, table := range []string{"shift_plan_heads", "shift_plan_revisions", "shift_plan_approvals", "shift_plan_assignments", "shift_plan_receipts", "shift_plan_audit"} {
			var state string
			if err := f.control.QueryRowContext(ctx, `SELECT count(*)||':'||coalesce(md5(string_agg(t::text,'|' ORDER BY t::text)),'') FROM supervisor.`+table+` t`).Scan(&state); err != nil {
				t.Fatal(err)
			}
			out = append(out, table+"="+state)
		}
		return strings.Join(out, " ")
	}
	handovers := plans + "/" + shift1.ID + "/handovers"
	before := planState()
	w := f.call(f.lead, "POST", handovers, uuid.NewString(), "", f.encode(workstation.ShiftHandoverRequestV1{SchemaVersion: 1, SummaryRevision: 1,
		NextShift: workstation.NextShiftV1{LineID: line, StartsAt: stamp(handover), EndsAt: stamp(shift2End), Timezone: "UTC"}}))
	f.expect(w, 201, "")
	var draft workstation.ShiftHandoverSnapshotV1
	if err = json.Unmarshal(w.Body.Bytes(), &draft); err != nil {
		t.Fatal(err)
	}
	if after := planState(); after != before {
		t.Fatalf("generating a handover changed shift plan rows:\n%s\n%s", before, after)
	}
	proposal := draft.Handover.ProposedPlan
	if proposal == nil || len(proposal.Assignments) != 1 || proposal.Assignments[0].WorkstationID != stationA || proposal.Assignments[0].JobOrderID != orderA.ID ||
		proposal.Assignments[0].TargetQuantity != expected[stationA].target-expected[stationA].good {
		t.Fatalf("handover must carry only station A's remaining %d: %s", expected[stationA].target-expected[stationA].good, f.encode(draft.Handover))
	}
	for _, j := range draft.Handover.Jobs {
		if j.WorkstationID == stationA && (len(j.QualityHolds) != 1 || j.QualityHolds[0].RecordID != held.ID) {
			t.Fatalf("handover must list the recorded hold: %s", f.encode(j))
		}
	}

	// Adopted by supervisor A; A cannot approve its own submission, supervisor B can.
	shift2 := decodePlan(f.call(f.lead, "POST", handovers+"/1/adopt", uuid.NewString(), "", f.encode(workstation.ShiftHandoverAdoptInputV1{CanonicalPayloadSHA256: draft.CanonicalPayloadSHA256})), 201)
	if shift2.ApprovedRevision != nil || shift2.Revisions[0].SubmittedBy.Subject != f.lead.subject || !reflect.DeepEqual(shift2.Revisions[0].Plan, *proposal) {
		t.Fatalf("adopted plan: %s", f.encode(shift2))
	}
	f.expect(approve(f.lead, shift2), 403, "SELF_REVIEW_FORBIDDEN")
	approved := decodePlan(approve(f.approver, shift2), 200)
	f.startRun(f.lead, shift2.ID) // shift 2 records need its run started; it starts within 30 minutes
	if approved.ApprovedRevision == nil || *approved.ApprovedRevision != 1 {
		t.Fatalf("shift 2 approval: %s", f.encode(approved))
	}

	// Shift 2 readback delivers the carried-forward order with the remaining target.
	inShift2 := stamp(handover.Add(time.Minute))
	carried, body := readback(f.operator, stationA, inShift2)
	if carried.State != workstation.DeliveryCurrent || carried.PlanID != shift2.ID || carried.JobOrderID != orderA.ID || carried.TargetQuantity != expected[stationA].target-expected[stationA].good {
		t.Fatalf("shift 2 readback: %s", body)
	}
	matchesPersisted(carried, orderA)

	// Before any shift 2 entry, the station has no inputs: NOT_READY, nothing reported as zero.
	empty := decodeSummary(f.call(f.lead, "POST", plans+"/"+shift2.ID+"/summaries", uuid.NewString(), "", `{"schema_version":1}`)).Summary
	if s := stationSummary(empty, stationA); empty.Readiness != workstation.SummaryNotReady || s.Readiness != workstation.SummaryNotReady ||
		!reflect.DeepEqual(s.Reasons, []workstation.SummaryReason{workstation.ReasonNoRecords}) || s.Recorded.GoodQuantity != nil || s.Recorded.RemainingQuantity != nil || s.Recorded.CycleTime != nil {
		t.Fatalf("shift 2 summary without inputs: %s", f.encode(empty))
	}

	// A shift 2 entry is accepted and bound to the shift 2 plan.
	ack2 := submit(f.operator, stationA, uuid.NewString(), entry(stationA, orderA, workstation.KindAcknowledged, stamp(handover)))
	if ack2.PlanID != shift2.ID || ack2.PlanRevision != 1 || ack2.Late {
		t.Fatalf("shift 2 entry: %s", f.encode(ack2))
	}

	// Stale source: the instruction is revised, so the pinned revision is no longer current.
	// The tablet stops showing the order and new entries are refused.
	f.expect(f.call(f.owner, "POST", site+"/sources", uuid.NewString(), `"1"`, `{"source_id":"`+source+`","kind":"document","title":"Synthetic SOP","text":"Synthetic SOP: press, gauge bore, torque check, revised"}`), 200, "")
	stale, body := readback(f.operator, stationA, inShift2)
	if stale.State != workstation.DeliveryInstructionUnavailable || stale.InstructionReason != workstation.InstructionSuperseded || stale.Order != nil || stale.PlanID != shift2.ID {
		t.Fatalf("stale instruction readback: %s", body)
	}
	f.expect(f.call(f.operator, "POST", recordsURL(stationA), uuid.NewString(), "", f.encode(entry(stationA, orderA, workstation.KindStarted, stamp(handover.Add(time.Millisecond))))), 409, "INSTRUCTION_UNAVAILABLE")
	if n := count(`SELECT count(*) FROM supervisor.station_records WHERE tenant_id=$1 AND plan_id=$2`, f.tenant, shift2.ID); n != 1 {
		t.Fatalf("shift 2 stored %d records, want only the accepted acknowledgment", n)
	}

	// Manual only, end to end: no operational source appeared and every record is a tablet entry.
	if n := count(`SELECT count(*) FROM supervisor.operational_source_heads WHERE tenant_id=$1`, f.tenant); n != 0 {
		t.Fatalf("manual-only scenario created %d operational sources", n)
	}
	if n := count(`SELECT count(*) FROM supervisor.station_records WHERE tenant_id=$1 AND source<>'manual_tablet'`, f.tenant); n != 0 {
		t.Fatalf("%d records are not manual tablet entries", n)
	}
}
