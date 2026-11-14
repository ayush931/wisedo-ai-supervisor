//go:build integration

package httpapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/workstation"
	"github.com/google/uuid"
)

// TestShiftHandoverHTTPPostgres proves WIS-520 acceptance item 6 (and the "partial order
// carried forward" and "two consecutive shifts" cases of item 9) against PostgreSQL with
// manual entry only and no model: a handover drafted from a stored shift summary lists the
// partial order with its remaining quantity, the unrecorded workstation as unknown and not
// proposed, the open blocker and the recorded quality hold, and changes no shift plan row.
// Adopting it is a separate action that submits an unapproved draft plan; its adopter cannot
// approve it, another supervisor can, and shift 2's tablet then delivers the carried order.
// An overlapping plan is never overwritten; revising it needs If-Match and leaves the
// approved revision in force.
func TestShiftHandoverHTTPPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	now0 := f.clock()
	// Shift 1 records lie before the handover instant; the 60 s margin absorbs slow setup.
	handover, shift2End := now0.Add(60*time.Second), now0.Add(time.Hour)
	line, stationA, stationB := uuid.NewString(), uuid.NewString(), uuid.NewString()
	source, sha, _ := f.instruction("Synthetic SOP: press and gauge bore", true)
	for _, station := range []string{stationA, stationB} {
		profile := workstation.StationProfileBodyV1{SchemaVersion: 1, WorkstationID: station, LineID: line, DisplayName: "Press", Template: "production",
			Fields: []workstation.ProfileFieldV1{{Name: "bore", Label: "Bore", Kind: "decimal", Unit: "mm", Min: "9.95", Max: "10.05"}}}
		f.expect(f.call(f.lead, "POST", "/v1/sites/"+f.site+"/workstations/"+station+"/profile-revisions", uuid.NewString(), `"0"`, f.encode(profile)), 201, "")
	}
	f.expect(f.authorize(f.lead, stationA, f.operator, workstation.AuthorizationGrant, `"0"`), 201, "")
	orderA, orderB := f.jobOrder(f.lead, orderBody(source, sha, 100)), f.jobOrder(f.lead, orderBody(source, sha, 50))
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
	shift1 := decodePlan(f.call(f.lead, "POST", plans, uuid.NewString(), "", f.encode(workstation.ShiftPlanBodyV1{SchemaVersion: 1, LineID: line, StartsAt: stamp(now0.Add(-time.Hour)),
		EndsAt: stamp(handover), Timezone: "UTC", Assignments: []workstation.AssignmentV1{pin(stationA, orderA, 100), pin(stationB, orderB, 50)}})), 201)
	f.expect(f.call(f.approver, "POST", plans+"/"+shift1.ID+"/approvals", uuid.NewString(), `"1"`, f.encode(workstation.ShiftPlanApproveInputV1{CanonicalPayloadSHA256: shift1.Revisions[0].CanonicalPayloadSHA256})), 200, "")
	f.startRun(f.lead, shift1.ID)

	// Station A records a partial run with a quality hold and an open block; B records nothing.
	t1 := f.clock()
	records := "/v1/sites/" + f.site + "/workstations/" + stationA + "/records"
	submit := func(body workstation.StationRecordBodyV1) workstation.StationRecordV1 {
		t.Helper()
		return f.decodeRecord(f.call(f.operator, "POST", records, uuid.NewString(), "", f.encode(body)), 201)
	}
	at := func(ms int) string { return stamp(t1.Add(time.Duration(ms) * time.Millisecond)) }
	entry := func(kind workstation.RecordKind, when string) workstation.StationRecordBodyV1 {
		return workstation.StationRecordBodyV1{SchemaVersion: 1, Kind: kind, WorkstationID: stationA, JobOrderID: orderA.ID, JobOrderRevision: 1, EventAt: when}
	}
	submit(entry(workstation.KindAcknowledged, at(0)))
	submit(entry(workstation.KindStarted, at(1)))
	quantity := entry(workstation.KindQuantity, at(2))
	quantity.GoodQuantity, quantity.RejectQuantity = pointer(int64(30)), pointer(int64(1))
	submit(quantity)
	hold := entry(workstation.KindQualityDisposition, at(3))
	hold.Disposition, hold.AuthorizesRelease, hold.Comment = workstation.DispositionHold, pointer(false), "Hold tag on bin 3"
	held := submit(hold)
	block := entry(workstation.KindBlocked, at(4))
	block.Comment = "Waiting for coil"
	blocked := submit(block)
	if !f.clock().Before(handover) {
		t.Fatalf("setup took past the handover at %s", handover)
	}
	f.expect(f.call(f.lead, "POST", plans+"/"+shift1.ID+"/summaries", uuid.NewString(), "", `{"schema_version":1}`), 201, "")

	handovers := plans + "/" + shift1.ID + "/handovers"
	request := func(summary int64, ends time.Time) string {
		return f.encode(workstation.ShiftHandoverRequestV1{SchemaVersion: 1, SummaryRevision: summary,
			NextShift: workstation.NextShiftV1{LineID: line, StartsAt: stamp(handover), EndsAt: stamp(ends), Timezone: "UTC"}})
	}
	decodeHandover := func(w *httptest.ResponseRecorder, status int) workstation.ShiftHandoverSnapshotV1 {
		t.Helper()
		f.expect(w, status, "")
		var h workstation.ShiftHandoverSnapshotV1
		if err := json.Unmarshal(w.Body.Bytes(), &h); err != nil {
			t.Fatal(err)
		}
		return h
	}
	// Every shift plan row, as the control role sees it.
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

	// Refusals first: authority, scope and contract.
	f.expect(f.call(f.operator, "POST", handovers, uuid.NewString(), "", request(1, shift2End)), 403, "PERMISSION_DENIED")
	f.expect(f.call(f.viewer, "POST", handovers, uuid.NewString(), "", request(1, shift2End)), 403, "PERMISSION_DENIED")
	f.expect(f.call(f.outsider, "POST", handovers, uuid.NewString(), "", request(1, shift2End)), 404, "NOT_FOUND")
	f.expect(f.call(f.lead, "POST", handovers, uuid.NewString(), "", request(2, shift2End)), 422, "INVALID_SHIFT_HANDOVER_REQUEST")
	early := strings.Replace(request(1, shift2End), stamp(handover), stamp(handover.Add(-time.Minute)), 1)
	f.expect(f.call(f.lead, "POST", handovers, uuid.NewString(), "", early), 422, "INVALID_SHIFT_HANDOVER_REQUEST")
	f.expect(f.call(f.lead, "POST", handovers, uuid.NewString(), "", strings.Replace(request(1, shift2End), `{"schema_version":1`, `{"schema_version":1,"tenant_id":"`+f.otherTenant+`"`, 1)), 422, "INVALID_SHIFT_HANDOVER_REQUEST")

	// Generation writes the draft and nothing else.
	before := planState()
	first := decodeHandover(f.call(f.lead, "POST", handovers, "handover-1", "", request(1, shift2End)), 201)
	if after := planState(); after != before {
		t.Fatalf("generating a handover changed shift plan rows:\n%s\n%s", before, after)
	}
	h := first.Handover
	if first.Revision != 1 || first.Status != workstation.HandoverDraft || first.Adoption != nil || first.GeneratedBy.Subject != f.lead.subject || h.SummaryRevision != 1 ||
		h.PlanRevision != 1 || h.Source.Readiness != workstation.SummaryNotReady || len(h.Jobs) != 2 {
		t.Fatalf("handover: %s", f.encode(first))
	}
	var jobA, jobB workstation.HandoverJobV1
	for _, j := range h.Jobs {
		if j.WorkstationID == stationA {
			jobA = j
		} else {
			jobB = j
		}
	}
	if !jobA.Proposed || *jobA.RemainingQuantity != 70 || *jobA.GoodQuantity != 30 || len(jobA.OpenBlockers) != 1 || jobA.OpenBlockers[0].StartRecordID != blocked.ID ||
		len(jobA.QualityHolds) != 1 || jobA.QualityHolds[0].RecordID != held.ID || !jobA.Instruction.Approved || jobA.Instruction.ChangedSincePin || jobA.JobOrderChanged {
		t.Fatalf("job A: %s", f.encode(jobA))
	}
	if jobB.Proposed || jobB.RemainingQuantity != nil || jobB.NotProposedReason != workstation.CarryRemainingUnknown {
		t.Fatalf("job B: %s", f.encode(jobB))
	}
	proposal := h.ProposedPlan
	if proposal == nil || len(proposal.Assignments) != 1 || proposal.Assignments[0].WorkstationID != stationA || proposal.Assignments[0].TargetQuantity != 70 ||
		proposal.Assignments[0].JobOrderRevision != 1 || proposal.StartsAt != stamp(handover) || proposal.EndsAt != stamp(shift2End) {
		t.Fatalf("proposal: %s", f.encode(proposal))
	}
	if replay := decodeHandover(f.call(f.lead, "POST", handovers, "handover-1", "", request(1, shift2End)), 201); !replay.Replayed || replay.CanonicalPayloadSHA256 != first.CanonicalPayloadSHA256 {
		t.Fatalf("replay %+v", replay)
	}
	f.expect(f.call(f.lead, "POST", handovers, "handover-1", "", request(1, shift2End.Add(time.Minute))), 409, "IDEMPOTENCY_CONFLICT")
	f.expect(f.call(f.lead, "POST", handovers, uuid.NewString(), "", request(1, shift2End)), 409, "SHIFT_HANDOVER_UNCHANGED")
	readback := decodeHandover(f.call(f.viewer, "GET", handovers+"/1", "", "", ""), 200)
	first.Replayed = false
	if !reflect.DeepEqual(readback, first) {
		t.Fatalf("readback differs:\n%s\n%s", f.encode(readback), f.encode(first))
	}
	f.expect(f.call(f.outsider, "GET", handovers+"/1", "", "", ""), 404, "NOT_FOUND")
	f.expect(f.call(f.viewer, "GET", handovers+"/9", "", "", ""), 404, "NOT_FOUND")

	// Adoption is explicit, owner/supervisor only, bound to the reviewed digest.
	adopt := func(actor stationSession, revision, key, ifMatch, digest string) *httptest.ResponseRecorder {
		return f.call(actor, "POST", handovers+"/"+revision+"/adopt", key, ifMatch, f.encode(workstation.ShiftHandoverAdoptInputV1{CanonicalPayloadSHA256: digest}))
	}
	f.expect(adopt(f.operator, "1", uuid.NewString(), "", first.CanonicalPayloadSHA256), 403, "PERMISSION_DENIED")
	f.expect(adopt(f.outsider, "1", uuid.NewString(), "", first.CanonicalPayloadSHA256), 404, "NOT_FOUND")
	f.expect(adopt(f.lead, "1", uuid.NewString(), "", strings.Repeat("0", 64)), 412, "REVISION_CONFLICT")
	if after := planState(); after != before {
		t.Fatalf("refused adoptions changed shift plan rows")
	}
	shift2 := decodePlan(adopt(f.lead, "1", "adopt-1", "", first.CanonicalPayloadSHA256), 201)
	if shift2.ApprovedRevision != nil || len(shift2.Revisions) != 1 || shift2.Revisions[0].State != workstation.StateDraft || shift2.Revisions[0].SubmittedBy.Subject != f.lead.subject ||
		!reflect.DeepEqual(shift2.Revisions[0].Plan, *proposal) || !shift2.StartsAt.Equal(handover) {
		t.Fatalf("adopted plan: %s", f.encode(shift2))
	}
	if again := decodePlan(adopt(f.lead, "1", "adopt-1", "", first.CanonicalPayloadSHA256), 201); !again.Replayed || again.ID != shift2.ID {
		t.Fatalf("adopt replay %+v", again)
	}
	f.expect(adopt(f.approver, "1", uuid.NewString(), "", first.CanonicalPayloadSHA256), 409, "SHIFT_HANDOVER_ALREADY_ADOPTED")
	if adopted := decodeHandover(f.call(f.viewer, "GET", handovers+"/1", "", "", ""), 200); adopted.Status != workstation.HandoverAdopted || adopted.Adoption == nil ||
		adopted.Adoption.PlanID != shift2.ID || adopted.Adoption.PlanRevision != 1 || adopted.Adoption.AdoptedBy.Subject != f.lead.subject || adopted.CanonicalPayloadSHA256 != first.CanonicalPayloadSHA256 {
		t.Fatalf("adopted handover: %s", f.encode(adopted))
	}
	// Until approved, shift 2's tablet shows nothing; the adopter cannot approve it.
	current := "/v1/sites/" + f.site + "/workstations/" + stationA + "/current-job-order?at=" + stamp(handover.Add(time.Minute))
	f.expect(f.call(f.operator, "GET", current, "", "", ""), 404, "NO_CURRENT_JOB_ORDER")
	approve := func(actor stationSession, plan workstation.ShiftPlanV1) *httptest.ResponseRecorder {
		return f.call(actor, "POST", plans+"/"+plan.ID+"/approvals", uuid.NewString(), `"`+strconv.FormatInt(plan.StateRevision, 10)+`"`,
			f.encode(workstation.ShiftPlanApproveInputV1{CanonicalPayloadSHA256: plan.Revisions[plan.CurrentRevision-1].CanonicalPayloadSHA256}))
	}
	f.expect(approve(f.lead, shift2), 403, "SELF_REVIEW_FORBIDDEN")
	approved := decodePlan(approve(f.approver, shift2), 200)
	var delivered workstation.CurrentJobOrderV1
	w := f.call(f.operator, "GET", current, "", "", "")
	f.expect(w, 200, "")
	if err := json.Unmarshal(w.Body.Bytes(), &delivered); err != nil || delivered.State != workstation.DeliveryCurrent || delivered.PlanID != shift2.ID ||
		delivered.JobOrderID != orderA.ID || delivered.TargetQuantity != 70 || delivered.Order == nil {
		t.Fatalf("shift 2 delivery: %s", w.Body.String())
	}

	// Later shift 1 data: a new summary and draft; adopting it never overwrites the approved
	// shift 2 plan. Without If-Match it is refused; with it, it appends a draft revision and
	// the approved revision stays in force.
	more := entry(workstation.KindQuantity, at(5))
	more.GoodQuantity, more.RejectQuantity = pointer(int64(10)), pointer(int64(0))
	if late := submit(more); late.PlanID != shift1.ID {
		t.Fatalf("shift 1 quantity bound to %s", late.PlanID)
	}
	f.expect(f.call(f.lead, "POST", plans+"/"+shift1.ID+"/summaries", uuid.NewString(), "", `{"schema_version":1}`), 201, "")
	second := decodeHandover(f.call(f.owner, "POST", handovers, uuid.NewString(), "", request(2, shift2End)), 201)
	if second.Revision != 2 || second.Handover.ProposedPlan.Assignments[0].TargetQuantity != 60 {
		t.Fatalf("second handover: %s", f.encode(second.Handover.ProposedPlan))
	}
	f.expect(adopt(f.owner, "2", uuid.NewString(), "", second.CanonicalPayloadSHA256), 409, "SHIFT_PLAN_EXISTS")
	f.expect(adopt(f.owner, "2", uuid.NewString(), `"1"`, second.CanonicalPayloadSHA256), 412, "REVISION_CONFLICT")
	revised := decodePlan(adopt(f.owner, "2", uuid.NewString(), `"`+strconv.FormatInt(approved.StateRevision, 10)+`"`, second.CanonicalPayloadSHA256), 201)
	if revised.ID != shift2.ID || revised.CurrentRevision != 2 || *revised.ApprovedRevision != 1 || revised.Revisions[1].State != workstation.StateDraft ||
		revised.Revisions[1].Plan.Assignments[0].TargetQuantity != 60 || revised.Revisions[1].SubmittedBy.Subject != f.owner.subject {
		t.Fatalf("revised shift 2: %s", f.encode(revised))
	}
	w = f.call(f.operator, "GET", current, "", "", "")
	f.expect(w, 200, "")
	if err := json.Unmarshal(w.Body.Bytes(), &delivered); err != nil || delivered.PlanRevision != 1 || delivered.TargetQuantity != 70 {
		t.Fatalf("approved shift 2 revision must stay in force: %s", w.Body.String())
	}
	var page workstation.ShiftHandoverPageV1
	w = f.call(f.operator, "GET", handovers+"?limit=1", "", "", "")
	f.expect(w, 200, "")
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || len(page.Handovers) != 1 || page.Handovers[0].Status != workstation.HandoverAdopted || page.NextCursor != "1" {
		t.Fatalf("page: %s", w.Body.String())
	}

	// The runtime role cannot rewrite drafts, forge an adoption or cross tenants.
	f.refused(f.tenant, "updated a draft", "42501", "permission denied", `UPDATE supervisor.shift_handovers SET summary_revision=1 WHERE plan_id=$1`, shift1.ID)
	f.refused(f.tenant, "deleted an adoption", "42501", "permission denied", `DELETE FROM supervisor.shift_handover_adoptions WHERE plan_id=$1`, shift1.ID)
	f.refused(f.tenant, "skipped a revision", "23514", "not the next revision", `INSERT INTO supervisor.shift_handovers(tenant_id,site_id,plan_id,revision,summary_revision,payload,canonical_payload,canonical_payload_sha256,digest_schema_version,actor_issuer,actor_subject)
 SELECT tenant_id,site_id,plan_id,7,summary_revision,payload,canonical_payload,canonical_payload_sha256,digest_schema_version,actor_issuer,actor_subject FROM supervisor.shift_handovers WHERE plan_id=$1 AND revision=1`, shift1.ID)
	f.refused(f.tenant, "forged an adoption", "23514", "does not match its proposed plan", `INSERT INTO supervisor.shift_handover_adoptions(tenant_id,site_id,plan_id,revision,adopted_plan_id,adopted_revision,actor_issuer,actor_subject) VALUES($1,$2,$3,2,$3,1,$4,$5)`,
		f.tenant, f.site, shift1.ID, f.issuer, f.lead.subject)
	// The adoption guard: the named plan revision must be the adopter's own, with exactly the
	// draft's proposed body, for the plan's latest draft.
	f.refused(f.tenant, "adopted under another actor", "23514", "does not match its proposed plan", `INSERT INTO supervisor.shift_handover_adoptions(tenant_id,site_id,plan_id,revision,adopted_plan_id,adopted_revision,actor_issuer,actor_subject) VALUES($1,$2,$3,2,$4,2,$5,$6)`,
		f.tenant, f.site, shift1.ID, shift2.ID, f.issuer, f.lead.subject)
	f.refused(f.tenant, "adopted a superseded draft", "23514", "names a superseded draft", `INSERT INTO supervisor.shift_handover_adoptions(tenant_id,site_id,plan_id,revision,adopted_plan_id,adopted_revision,actor_issuer,actor_subject) VALUES($1,$2,$3,1,$4,1,$5,$6)`,
		f.tenant, f.site, shift1.ID, shift2.ID, f.issuer, f.lead.subject)
	f.refused(f.otherTenant, "wrote another tenant's draft", "42501", "tenant scope denied", `INSERT INTO supervisor.shift_handovers(tenant_id,site_id,plan_id,revision,summary_revision,payload,canonical_payload,canonical_payload_sha256,digest_schema_version,actor_issuer,actor_subject)
 VALUES($1,$2,$3,3,1,'{}','{}',encode(sha256(convert_to('{}','UTF8')),'hex'),1,'x','y')`, f.tenant, f.site, shift1.ID)
	var visible int
	if err := f.runtime.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.shift_handovers`).Scan(&visible); err != nil || visible != 0 {
		t.Fatalf("runtime without tenant context sees %d drafts (%v)", visible, err)
	}

	// The instruction is re-checked at adoption: a draft generated while it was current is
	// refused, naming the job order, once the source is revoked; nothing is submitted.
	last := entry(workstation.KindQuantity, at(6))
	last.GoodQuantity, last.RejectQuantity = pointer(int64(5)), pointer(int64(0))
	submit(last)
	f.expect(f.call(f.lead, "POST", plans+"/"+shift1.ID+"/summaries", uuid.NewString(), "", `{"schema_version":1}`), 201, "")
	third := decodeHandover(f.call(f.owner, "POST", handovers, uuid.NewString(), "", request(3, shift2End)), 201)
	if third.Revision != 3 || third.Handover.ProposedPlan == nil || third.Handover.ProposedPlan.Assignments[0].JobOrderID != orderA.ID {
		t.Fatalf("third handover: %s", f.encode(third.Handover))
	}
	f.expect(f.call(f.owner, "POST", "/v1/sites/"+f.site+"/sources/"+source+"/revocations", uuid.NewString(), `"1"`, `{"reason":"Source withdrawn"}`), 200, "")
	before = planState()
	w = adopt(f.owner, "3", uuid.NewString(), `"`+strconv.FormatInt(revised.StateRevision, 10)+`"`, third.CanonicalPayloadSHA256)
	f.expect(w, 409, "INSTRUCTION_UNAVAILABLE")
	if !strings.Contains(w.Body.String(), `"path":"`+orderA.ID+`"`) {
		t.Fatalf("instruction refusal must name job order %s: %s", orderA.ID, w.Body.String())
	}
	if after := planState(); after != before {
		t.Fatalf("a refused adoption changed shift plan rows")
	}
}

// TestShiftHandoverSupersededHTTPPostgres: only a plan's latest draft is adoptable. A draft
// generated before a correction is refused with HANDOVER_SUPERSEDED on both the create and
// the If-Match revise path, and the draft drafted from the corrected data is adopted.
func TestShiftHandoverSupersededHTTPPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	now0 := f.clock()
	handover := now0.Add(60 * time.Second)
	s := f.recordingStation(now0.Add(-time.Hour), handover)
	f.expect(f.authorize(f.lead, s.station, f.operator, workstation.AuthorizationGrant, `"0"`), 201, "")
	records := "/v1/sites/" + f.site + "/workstations/" + s.station + "/records"
	submit := func(body workstation.StationRecordBodyV1) workstation.StationRecordV1 {
		t.Helper()
		return f.decodeRecord(f.call(f.operator, "POST", records, uuid.NewString(), "", f.encode(body)), 201)
	}
	t1 := f.clock()
	submit(s.entry(workstation.KindAcknowledged, stamp(t1)))
	submit(s.entry(workstation.KindStarted, stamp(t1.Add(time.Millisecond))))
	q := s.entry(workstation.KindQuantity, stamp(t1.Add(2*time.Millisecond)))
	q.GoodQuantity, q.RejectQuantity = pointer(int64(30)), pointer(int64(0))
	partial := submit(q)
	if !f.clock().Before(handover) {
		t.Fatalf("setup took past the handover at %s", handover)
	}
	plans := "/v1/sites/" + f.site + "/shift-plans"
	summaries, handovers := plans+"/"+s.plan.ID+"/summaries", plans+"/"+s.plan.ID+"/handovers"
	generate := func(summary int64) workstation.ShiftHandoverSnapshotV1 {
		t.Helper()
		f.expect(f.call(f.lead, "POST", summaries, uuid.NewString(), "", `{"schema_version":1}`), 201, "")
		w := f.call(f.lead, "POST", handovers, uuid.NewString(), "", f.encode(workstation.ShiftHandoverRequestV1{SchemaVersion: 1, SummaryRevision: summary,
			NextShift: workstation.NextShiftV1{LineID: s.line, StartsAt: stamp(handover), EndsAt: stamp(now0.Add(time.Hour)), Timezone: "UTC"}}))
		f.expect(w, 201, "")
		var h workstation.ShiftHandoverSnapshotV1
		if err := json.Unmarshal(w.Body.Bytes(), &h); err != nil {
			t.Fatal(err)
		}
		return h
	}
	first := generate(1)
	fix := s.entry(workstation.KindQuantity, stamp(t1.Add(2*time.Millisecond)))
	fix.GoodQuantity, fix.RejectQuantity, fix.CorrectsRecordID = pointer(int64(31)), pointer(int64(0)), partial.ID
	submit(fix)
	second := generate(2)
	if first.Revision != 1 || second.Revision != 2 || first.Handover.ProposedPlan.Assignments[0].TargetQuantity != 70 || second.Handover.ProposedPlan.Assignments[0].TargetQuantity != 69 {
		t.Fatalf("drafts: %s %s", f.encode(first.Handover.ProposedPlan), f.encode(second.Handover.ProposedPlan))
	}
	adopt := func(h workstation.ShiftHandoverSnapshotV1, ifMatch string) *httptest.ResponseRecorder {
		return f.call(f.lead, "POST", handovers+"/"+strconv.FormatInt(h.Revision, 10)+"/adopt", uuid.NewString(), ifMatch, f.encode(workstation.ShiftHandoverAdoptInputV1{CanonicalPayloadSHA256: h.CanonicalPayloadSHA256}))
	}
	f.expect(adopt(first, ""), 409, "HANDOVER_SUPERSEDED")
	f.expect(adopt(first, `"1"`), 409, "HANDOVER_SUPERSEDED")
	w := adopt(second, "")
	f.expect(w, 201, "")
	var plan workstation.ShiftPlanV1
	if err := json.Unmarshal(w.Body.Bytes(), &plan); err != nil || plan.ApprovedRevision != nil || plan.Revisions[0].Plan.Assignments[0].TargetQuantity != 69 {
		t.Fatalf("adopted: %s", w.Body.String())
	}
	f.expect(adopt(first, ""), 409, "HANDOVER_SUPERSEDED")
}

// WIS-520 review finding (acceptance 6, "source freshness"): a handover drafted from a summary
// a newer summary has replaced carries forward remaining quantity that was already produced.
// Drafting from a non-latest summary is refused, and a draft whose summary was replaced since
// generation is not adoptable.
func TestShiftHandoverStaleSummaryHTTPPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	now0 := f.clock()
	handover := now0.Add(60 * time.Second)
	s := f.recordingStation(now0.Add(-time.Hour), handover)
	f.expect(f.authorize(f.lead, s.station, f.operator, workstation.AuthorizationGrant, `"0"`), 201, "")
	records := "/v1/sites/" + f.site + "/workstations/" + s.station + "/records"
	submit := func(body workstation.StationRecordBodyV1) {
		t.Helper()
		f.decodeRecord(f.call(f.operator, "POST", records, uuid.NewString(), "", f.encode(body)), 201)
	}
	t1 := f.clock()
	submit(s.entry(workstation.KindAcknowledged, stamp(t1)))
	submit(s.entry(workstation.KindStarted, stamp(t1.Add(time.Millisecond))))
	produce := func(offset time.Duration, good int64) {
		t.Helper()
		q := s.entry(workstation.KindQuantity, stamp(t1.Add(offset)))
		q.GoodQuantity, q.RejectQuantity = pointer(good), pointer(int64(0))
		submit(q)
	}
	plans := "/v1/sites/" + f.site + "/shift-plans"
	summaries, handovers := plans+"/"+s.plan.ID+"/summaries", plans+"/"+s.plan.ID+"/handovers"
	summarize := func() {
		t.Helper()
		f.expect(f.call(f.lead, "POST", summaries, uuid.NewString(), "", `{"schema_version":1}`), 201, "")
	}
	generate := func(summary int64) *httptest.ResponseRecorder {
		t.Helper()
		return f.call(f.lead, "POST", handovers, uuid.NewString(), "", f.encode(workstation.ShiftHandoverRequestV1{SchemaVersion: 1, SummaryRevision: summary,
			NextShift: workstation.NextShiftV1{LineID: s.line, StartsAt: stamp(handover), EndsAt: stamp(now0.Add(time.Hour)), Timezone: "UTC"}}))
	}
	produce(2*time.Millisecond, 30)
	summarize() // revision 1: 30 good
	produce(3*time.Millisecond, 10)
	summarize() // revision 2: 40 good
	if !f.clock().Before(handover) {
		t.Fatalf("setup took past the handover at %s", handover)
	}
	f.expect(generate(1), 409, "HANDOVER_SOURCE_STALE")
	w := generate(2)
	f.expect(w, 201, "")
	var draft workstation.ShiftHandoverSnapshotV1
	if err := json.Unmarshal(w.Body.Bytes(), &draft); err != nil {
		t.Fatal(err)
	}
	produce(4*time.Millisecond, 5)
	summarize() // revision 3: 45 good; the draft's remaining quantity is now overstated
	adopt := f.call(f.lead, "POST", handovers+"/"+strconv.FormatInt(draft.Revision, 10)+"/adopt", uuid.NewString(), "", f.encode(workstation.ShiftHandoverAdoptInputV1{CanonicalPayloadSHA256: draft.CanonicalPayloadSHA256}))
	f.expect(adopt, 409, "HANDOVER_SOURCE_STALE")
	var adoptions int
	if err := f.control.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.shift_handover_adoptions WHERE plan_id=$1`, s.plan.ID).Scan(&adoptions); err != nil || adoptions != 0 {
		t.Fatalf("stale draft adopted: %d (%v)", adoptions, err)
	}
	f.expect(generate(3), 201, "")
}
