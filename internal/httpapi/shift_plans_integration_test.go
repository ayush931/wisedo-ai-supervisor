//go:build integration

package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/workstation"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestShiftPlanLifecycleHTTPPostgres proves WIS-520 acceptance item 1 against PostgreSQL:
// create, approve, revise and supersede an attributed shift plan, with distinct-human
// approval over the exact digest, If-Match, idempotency, strict contracts, tenant isolation
// and immutable history.
func TestShiftPlanLifecycleHTTPPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	var err error
	type session = stationSession
	call, expect, exec, encode := f.call, f.expect, f.exec, f.encode
	control, runtime := f.control, f.runtime
	tenant, site, otherTenant, otherSite, issuer := f.tenant, f.site, f.otherTenant, f.otherSite, f.issuer
	ownerS, leadS, approverS, staleS, operatorS, outsiderS := f.owner, f.lead, f.approver, f.stale, f.operator, f.outsider
	owner, lead, approver := ownerS.subject, leadS.subject, approverS.subject
	decodePlan := func(w *httptest.ResponseRecorder) workstation.ShiftPlanV1 {
		t.Helper()
		var p workstation.ShiftPlanV1
		if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
			t.Fatal(err)
		}
		if w.Header().Get("ETag") != `"`+strconv.FormatInt(p.StateRevision, 10)+`"` {
			t.Fatalf("ETag %q does not carry state revision %d", w.Header().Get("ETag"), p.StateRevision)
		}
		return p
	}
	tag := func(p workstation.ShiftPlanV1) string { return `"` + strconv.FormatInt(p.StateRevision, 10) + `"` }

	line, stationA, stationB := uuid.NewString(), uuid.NewString(), uuid.NewString()
	// Every assignment pins a real job order whose instruction is a confirmed-current document.
	source, sha, _ := f.instruction("Synthetic SOP: inspect weld seam", true)
	assignment := func(station string, quantity int64) workstation.AssignmentV1 {
		return pin(station, f.jobOrder(ownerS, orderBody(source, sha, quantity)), quantity)
	}
	shift := func(start, end string, assignments ...workstation.AssignmentV1) workstation.ShiftPlanBodyV1 {
		return workstation.ShiftPlanBodyV1{SchemaVersion: 1, LineID: line, StartsAt: start, EndsAt: end, Timezone: "Europe/Berlin", Assignments: assignments}
	}
	// Day shift in CEST (+02:00); the stored instants are UTC 04:00-12:00.
	day := shift("2026-10-20T06:00:00+02:00", "2026-10-20T14:00:00+02:00", assignment(stationB, 80), assignment(stationA, 120))
	base := "/v1/sites/" + site + "/shift-plans"

	// Authentication, authority, headers and strict contracts.
	expect(call(session{}, "POST", base, "c", "", encode(day)), 401, "AUTHENTICATION_REQUIRED")
	expect(call(session{token: leadS.token, csrf: "wrong"}, "POST", base, "c", "", encode(day)), 403, "CSRF_REJECTED")
	expect(call(leadS, "POST", base, "", "", encode(day)), 422, "INVALID_REQUEST")
	expect(call(operatorS, "POST", base, "c", "", encode(day)), 403, "PERMISSION_DENIED")
	unknown := strings.Replace(encode(day), `"schema_version":1`, `"schema_version":1,"approved":true`, 1)
	expect(call(leadS, "POST", base, "c", "", unknown), 422, "INVALID_SHIFT_PLAN")
	smuggled := strings.Replace(encode(day), `"schema_version":1`, `"schema_version":1,"tenant_id":"`+otherTenant+`"`, 1)
	expect(call(leadS, "POST", base, "c", "", smuggled), 422, "INVALID_SHIFT_PLAN")
	wrongZone := day
	wrongZone.StartsAt = "2026-10-20T04:00:00Z"
	expect(call(leadS, "POST", base, "c", "", encode(wrongZone)), 422, "INVALID_SHIFT_PLAN")

	// Create: draft revision 1, attributed, server-ordered assignments, UTC shift.
	created := call(leadS, "POST", base, "create-1", "", encode(day))
	expect(created, 201, "")
	plan := decodePlan(created)
	r1 := plan.Revisions[0]
	if plan.CurrentRevision != 1 || plan.ApprovedRevision != nil || plan.StateRevision != 1 || len(plan.Revisions) != 1 || r1.State != workstation.StateDraft ||
		r1.SubmittedBy.Subject != lead || r1.Approval != nil || !plan.StartsAt.Equal(time.Date(2026, 10, 20, 4, 0, 0, 0, time.UTC)) ||
		r1.Plan.Assignments[0].WorkstationID > r1.Plan.Assignments[1].WorkstationID || !strings.Contains(created.Body.String(), `"starts_at":"2026-10-20T04:00:00Z"`) || len(r1.CanonicalPayloadSHA256) != 64 {
		t.Fatalf("created plan: %+v", plan)
	}
	planURL := base + "/" + plan.ID
	// Idempotent replay returns the same plan; the same key with a different body conflicts.
	replay := call(leadS, "POST", base, "create-1", "", encode(day))
	expect(replay, 201, "")
	if p := decodePlan(replay); !p.Replayed || p.ID != plan.ID || len(p.Revisions) != 1 {
		t.Fatalf("replay: %+v", p)
	}
	night := shift("2026-10-20T14:00:00+02:00", "2026-10-20T22:00:00+02:00", assignment(stationA, 100))
	expect(call(leadS, "POST", base, "create-1", "", encode(night)), 409, "IDEMPOTENCY_CONFLICT")
	// A second plan for an overlapping interval on the line is refused; revise instead.
	overlap := shift("2026-10-20T13:00:00+02:00", "2026-10-20T15:00:00+02:00", assignment(stationA, 10))
	expect(call(ownerS, "POST", base, "overlap", "", encode(overlap)), 409, "SHIFT_PLAN_EXISTS")
	// No approved plan yet: a draft is never current.
	current := base + "/current?line_id=" + line + "&at=" + url.QueryEscape("2026-10-20T08:00:00+02:00")
	expect(call(operatorS, "GET", current, "", "", ""), 404, "NO_APPROVED_SHIFT_PLAN")

	// Approval: supervisor-only, recent authentication, distinct from the submitter, exact digest.
	approve := func(p workstation.ShiftPlanV1, digest string) string {
		return encode(workstation.ShiftPlanApproveInputV1{CanonicalPayloadSHA256: digest})
	}
	approval := approve(plan, r1.CanonicalPayloadSHA256)
	expect(call(approverS, "POST", planURL+"/approvals", "a", "", approval), 428, "REVISION_REQUIRED")
	expect(call(leadS, "POST", planURL+"/approvals", "self", tag(plan), approval), 403, "SELF_REVIEW_FORBIDDEN")
	expect(call(ownerS, "POST", planURL+"/approvals", "owner", tag(plan), approval), 403, "PERMISSION_DENIED")
	expect(call(staleS, "POST", planURL+"/approvals", "stale", tag(plan), approval), 428, "REAUTHENTICATION_REQUIRED")
	expect(call(approverS, "POST", planURL+"/approvals", "digest", tag(plan), approve(plan, strings.Repeat("0", 64))), 412, "REVISION_CONFLICT")
	expect(call(approverS, "POST", planURL+"/approvals", "stale-tag", `"7"`, approval), 412, "REVISION_CONFLICT")
	approved := call(approverS, "POST", planURL+"/approvals", "approve-1", tag(plan), approval)
	expect(approved, 200, "")
	plan = decodePlan(approved)
	if plan.ApprovedRevision == nil || *plan.ApprovedRevision != 1 || plan.Revisions[0].State != workstation.StateApproved || plan.Revisions[0].Approval == nil ||
		plan.Revisions[0].Approval.Approver.Subject != approver || plan.Revisions[0].Approval.CanonicalPayloadSHA256 != r1.CanonicalPayloadSHA256 {
		t.Fatalf("approved plan: %+v", plan)
	}
	expect(call(approverS, "POST", planURL+"/approvals", "approve-again", tag(plan), approval), 409, "SHIFT_PLAN_ALREADY_APPROVED")
	cur := call(operatorS, "GET", current, "", "", "")
	expect(cur, 200, "")
	var inForce workstation.CurrentShiftPlanV1
	if err = json.Unmarshal(cur.Body.Bytes(), &inForce); err != nil || inForce.PlanID != plan.ID || inForce.Revision.Revision != 1 {
		t.Fatalf("current plan: %+v %v", inForce, err)
	}
	expect(call(operatorS, "GET", base+"/current?line_id="+line+"&at="+url.QueryEscape("2026-10-20T14:00:00+02:00"), "", "", ""), 404, "NO_APPROVED_SHIFT_PLAN")

	// Snapshot history before revising: revisions and approvals must never change.
	history := func() string {
		t.Helper()
		var s string
		if e := control.QueryRowContext(ctx, `SELECT coalesce((SELECT string_agg(concat_ws('|',plan_id,revision,payload::text,canonical_payload,canonical_payload_sha256,actor_subject,created_at),',' ORDER BY revision) FROM supervisor.shift_plan_revisions WHERE plan_id=$1),'')||'#'||coalesce((SELECT string_agg(concat_ws('|',revision,canonical_payload_sha256,approver_subject,approved_at),',' ORDER BY revision) FROM supervisor.shift_plan_approvals WHERE plan_id=$1),'')`, plan.ID).Scan(&s); e != nil {
			t.Fatal(e)
		}
		return s
	}
	before := history()

	// Revise: stale If-Match is 412; a moved shift is invalid; an identical body is refused.
	revised := shift(day.StartsAt, day.EndsAt, assignment(stationA, 150), assignment(stationB, 80))
	expect(call(ownerS, "POST", planURL+"/revisions", "rev-stale", `"1"`, encode(revised)), 412, "REVISION_CONFLICT")
	moved := revised
	moved.EndsAt = "2026-10-20T13:00:00+02:00"
	expect(call(ownerS, "POST", planURL+"/revisions", "rev-moved", tag(plan), encode(moved)), 422, "INVALID_SHIFT_PLAN")
	// Revise cannot move the plan to another line or timezone either (Paris shares Berlin's offset).
	rezoned := revised
	rezoned.Timezone = "Europe/Paris"
	expect(call(ownerS, "POST", planURL+"/revisions", "rev-zone", tag(plan), encode(rezoned)), 422, "INVALID_SHIFT_PLAN")
	relined := revised
	relined.LineID = uuid.NewString()
	expect(call(ownerS, "POST", planURL+"/revisions", "rev-line", tag(plan), encode(relined)), 422, "INVALID_SHIFT_PLAN")
	expect(call(ownerS, "POST", planURL+"/revisions", "rev-same", tag(plan), encode(day)), 409, "SHIFT_PLAN_UNCHANGED")
	rev := call(ownerS, "POST", planURL+"/revisions", "rev-2", tag(plan), encode(revised))
	expect(rev, 201, "")
	plan = decodePlan(rev)
	if plan.CurrentRevision != 2 || *plan.ApprovedRevision != 1 || plan.Revisions[0].State != workstation.StateApproved || plan.Revisions[1].State != workstation.StateDraft ||
		plan.Revisions[1].SubmittedBy.Subject != owner {
		t.Fatalf("revised plan: %+v", plan)
	}
	// Revising never silently changes the plan in force.
	cur = call(operatorS, "GET", current, "", "", "")
	if err = json.Unmarshal(cur.Body.Bytes(), &inForce); err != nil || inForce.Revision.Revision != 1 {
		t.Fatalf("draft became current: %s", cur.Body.String())
	}
	revReplay := call(ownerS, "POST", planURL+"/revisions", "rev-2", `"2"`, encode(revised))
	expect(revReplay, 201, "")
	if p := decodePlan(revReplay); !p.Replayed || p.CurrentRevision != 2 {
		t.Fatalf("revise replay: %+v", p)
	}
	expect(call(ownerS, "POST", planURL+"/revisions", "rev-2", `"2"`, encode(night)), 409, "IDEMPOTENCY_CONFLICT")

	// Approving revision 2 (the reviser is the owner, so the lead may approve) supersedes 1.
	// Rule 05 strict: the lead created revision 1, so the lead cannot approve the owner's revision 2.
	expect(call(leadS, "POST", planURL+"/approvals", "creator-2", tag(plan), approve(plan, plan.Revisions[1].CanonicalPayloadSHA256)), 403, "SELF_REVIEW_FORBIDDEN")
	approved = call(approverS, "POST", planURL+"/approvals", "approve-2", tag(plan), approve(plan, plan.Revisions[1].CanonicalPayloadSHA256))
	expect(approved, 200, "")
	plan = decodePlan(approved)
	if *plan.ApprovedRevision != 2 || plan.Revisions[0].State != workstation.StateSuperseded || plan.Revisions[1].State != workstation.StateApproved ||
		plan.Revisions[0].Approval == nil || plan.Revisions[0].Approval.Approver.Subject != approver {
		t.Fatalf("superseding approval: %+v", plan)
	}
	cur = call(operatorS, "GET", current, "", "", "")
	if err = json.Unmarshal(cur.Body.Bytes(), &inForce); err != nil || inForce.Revision.Revision != 2 || inForce.Revision.State != workstation.StateApproved {
		t.Fatalf("superseded revision still current: %s", cur.Body.String())
	}
	if after := history(); !strings.HasPrefix(after, strings.Split(before, "#")[0]) || !strings.Contains(after, strings.Split(before, "#")[1]) {
		t.Fatalf("history changed:\nbefore %s\nafter  %s", before, after)
	}
	// A third revision drafted by the lead cannot be approved by the lead.
	third := shift(day.StartsAt, day.EndsAt, assignment(stationA, 160))
	rev = call(leadS, "POST", planURL+"/revisions", "rev-3", tag(plan), encode(third))
	expect(rev, 201, "")
	plan = decodePlan(rev)
	expect(call(leadS, "POST", planURL+"/approvals", "self-3", tag(plan), approve(plan, plan.Revisions[2].CanonicalPayloadSHA256)), 403, "SELF_REVIEW_FORBIDDEN")
	if plan.Revisions[1].State != workstation.StateApproved || plan.Revisions[2].State != workstation.StateDraft {
		t.Fatalf("third revision states: %+v", plan.Revisions)
	}

	// Runtime role cannot rewrite history or approve as the submitter, even bypassing the API.
	refused := func(name, code, message, statement string, args ...any) {
		t.Helper()
		rtx, err := runtime.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer rtx.Rollback()
		if _, err = rtx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, tenant); err != nil {
			t.Fatal(err)
		}
		if _, err = rtx.ExecContext(ctx, statement, args...); err == nil {
			err = rtx.Commit()
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != code || !strings.Contains(pgErr.Message, message) {
			t.Fatalf("runtime %s: want %s %q, got %v", name, code, message, err)
		}
	}
	refused("rewrote a shift plan revision", "42501", "permission denied", `UPDATE supervisor.shift_plan_revisions SET actor_subject='x' WHERE plan_id=$1`, plan.ID)
	refused("deleted an approval", "42501", "permission denied", `DELETE FROM supervisor.shift_plan_approvals WHERE plan_id=$1`, plan.ID)
	refused("made a superseded revision current", "23514", "Invalid shift plan head transition", `UPDATE supervisor.shift_plan_heads SET approved_revision=1,state_revision=state_revision+1 WHERE id=$1`, plan.ID)
	insertApproval := `INSERT INTO supervisor.shift_plan_approvals(tenant_id,site_id,plan_id,revision,canonical_payload_sha256,approver_issuer,approver_subject,policy_revision,auth_time) VALUES($1,$2,$3,$4,$5,$6,$7,1,now())`
	refused("approved a non-current revision", "23514", "approval revision is not current", insertApproval, tenant, site, plan.ID, 2, plan.Revisions[1].CanonicalPayloadSHA256, issuer, approver)
	refused("self-approved", "42501", "approver must differ from every submitter", insertApproval, tenant, site, plan.ID, 3, plan.Revisions[2].CanonicalPayloadSHA256, issuer, lead)
	// The owner revised revision 2 only, yet is still a submitter of this plan.
	refused("earlier reviser approved", "42501", "approver must differ from every submitter", insertApproval, tenant, site, plan.ID, 3, plan.Revisions[2].CanonicalPayloadSHA256, issuer, owner)

	// Get, list and pagination.
	got := call(operatorS, "GET", planURL, "", "", "")
	expect(got, 200, "")
	if p := decodePlan(got); len(p.Revisions) != 3 {
		t.Fatalf("get: %+v", p)
	}
	expect(call(operatorS, "GET", planURL+"?x=1", "", "", ""), 422, "INVALID_REQUEST")
	expect(call(leadS, "POST", base, "night", "", encode(night)), 201, "")
	var page workstation.ShiftPlanPageV1
	list := call(operatorS, "GET", base+"?limit=1", "", "", "")
	expect(list, 200, "")
	if err = json.Unmarshal(list.Body.Bytes(), &page); err != nil || len(page.Plans) != 1 || page.NextCursor == "" {
		t.Fatalf("first page: %s", list.Body.String())
	}
	list = call(operatorS, "GET", base+"?limit=1&cursor="+page.NextCursor, "", "", "")
	var second workstation.ShiftPlanPageV1
	if err = json.Unmarshal(list.Body.Bytes(), &second); err != nil || len(second.Plans) != 1 || second.NextCursor != "" || second.Plans[0].ID == page.Plans[0].ID {
		t.Fatalf("second page: %s", list.Body.String())
	}
	expect(call(operatorS, "GET", base+"?limit=101", "", "", ""), 422, "INVALID_REQUEST")

	// Tenant isolation: non-disclosing 404 in both directions.
	expect(call(outsiderS, "GET", planURL, "", "", ""), 404, "NOT_FOUND")
	expect(call(outsiderS, "GET", "/v1/sites/"+otherSite+"/shift-plans/"+plan.ID, "", "", ""), 404, "NOT_FOUND")
	expect(call(outsiderS, "POST", "/v1/sites/"+otherSite+"/shift-plans/"+plan.ID+"/approvals", "x", tag(plan), approve(plan, plan.Revisions[2].CanonicalPayloadSHA256)), 404, "NOT_FOUND")
	expect(call(leadS, "GET", "/v1/sites/"+otherSite+"/shift-plans", "", "", ""), 404, "NOT_FOUND")
	// Concurrent overlapping creates on one line: the site writer lock admits exactly one.
	raceLine := uuid.NewString()
	results := make(chan *httptest.ResponseRecorder, 2)
	for i, window := range [][2]string{{"2026-10-21T06:00:00+02:00", "2026-10-21T14:00:00+02:00"}, {"2026-10-21T10:00:00+02:00", "2026-10-21T18:00:00+02:00"}} {
		body := shift(window[0], window[1], assignment(stationA, 10))
		body.LineID = raceLine
		go func(key, body string) { results <- call(leadS, "POST", base, key, "", body) }("race-"+strconv.Itoa(i), encode(body))
	}
	codes := map[int]int{}
	for range 2 {
		w := <-results
		codes[w.Code]++
		if w.Code == 409 && !strings.Contains(w.Body.String(), `"code":"SHIFT_PLAN_EXISTS"`) {
			t.Fatalf("losing create: %s", w.Body.String())
		}
	}
	if codes[201] != 1 || codes[409] != 1 {
		t.Fatalf("concurrent overlapping creates: %v", codes)
	}

	var outsiderPage workstation.ShiftPlanPageV1
	list = call(outsiderS, "GET", "/v1/sites/"+otherSite+"/shift-plans", "", "", "")
	if err = json.Unmarshal(list.Body.Bytes(), &outsiderPage); err != nil || len(outsiderPage.Plans) != 0 {
		t.Fatalf("other tenant listed plans: %s", list.Body.String())
	}

	// An idempotent replay still re-checks authority: a stale session cannot replay an approval.
	exec(`UPDATE supervisor.browser_sessions SET auth_time=now()-interval '20 minutes' WHERE subject=$1`, approver)
	expect(call(approverS, "POST", planURL+"/approvals", "approve-1", `"1"`, approval), 428, "REAUTHENTICATION_REQUIRED")
}
