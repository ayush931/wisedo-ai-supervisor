package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/workstation"
)

// makePlan returns the plan and the observed status of its create request.
func (s scenarioRunner) makePlan(ctx context.Context, overnight bool) (workstation.ShiftPlanV1, int, error) {
	var plan workstation.ShiftPlanV1
	var profile workstation.StationProfileBodyV1
	if err := json.Unmarshal(s.d.files[datasetPaths(s.d, "profiles/")[0]], &profile); err != nil {
		return plan, 0, err
	}
	profile.WorkstationID = s.id("station")
	profile.LineID = s.id("line")
	profile.DisplayName = "Facility edge " + s.prefix
	profile.InstructionReferences = nil
	if err := s.l.call(ctx, "planner", "/workstations/"+profile.WorkstationID+"/profile-revisions", s.prefix+"profile", "0", profile, nil); err != nil {
		return plan, 0, err
	}
	var jobBody workstation.JobOrderBodyV1
	if err := json.Unmarshal(s.d.files[datasetPaths(s.d, "jobs/")[0]], &jobBody); err != nil {
		return plan, 0, err
	}
	jobBody.RoutingID = ""
	jobBody.ItemID = ""
	jobBody.StepNumber = nil
	jobBody.DueAt = ""
	jobBody.OrderID = s.id("order")
	jobBody.OperationID = s.id("operation")
	jobBody.ItemCode = "Facility edge"
	var job workstation.JobOrderV1
	if err := s.l.call(ctx, "planner", "/job-orders", s.prefix+"job", "", jobBody, &job); err != nil {
		return plan, 0, err
	}
	zone, _ := time.LoadLocation("Asia/Kolkata")
	start := time.Now().In(zone).Truncate(time.Second).Add(-time.Minute)
	end := start.Add(30 * time.Minute)
	if overnight {
		now := time.Now().In(zone)
		start = time.Date(now.Year(), now.Month(), now.Day()+1, 22, 0, 0, 0, zone)
		end = start.Add(8 * time.Hour)
	}
	body := workstation.ShiftPlanBodyV1{SchemaVersion: 1, LineID: profile.LineID, StartsAt: stamp(start), EndsAt: stamp(end), Timezone: "Asia/Kolkata", Assignments: []workstation.AssignmentV1{{WorkstationID: profile.WorkstationID, JobOrderID: job.ID, JobOrderRevision: 1, OrderID: jobBody.OrderID, OperationID: jobBody.OperationID, ItemCode: jobBody.ItemCode, TargetQuantity: jobBody.TargetQuantity, TargetCycleTimeSeconds: jobBody.TargetCycleTimeSeconds, InstructionDocumentID: jobBody.InstructionDocumentID, InstructionRevision: jobBody.InstructionRevision, InstructionSHA256: jobBody.InstructionSHA256, Template: jobBody.Template, Fields: jobBody.Fields}}}
	created, err := s.request(ctx, "planner", "POST", "/shift-plans", "plan", "", "application/json", wire(body), &plan)
	if err == nil && (created.Status < 200 || created.Status > 299) {
		err = fmt.Errorf("shift plan create refused: %d %s", created.Status, created.Code)
	}
	return plan, created.Status, err
}
func (s scenarioRunner) shift(ctx context.Context, id string) (scenarioObservation, error) {
	o := scenarioObservation{}
	plan, created, err := s.makePlan(ctx, id == "overnight-shift")
	if err != nil {
		return o, err
	}
	p := plan.Revisions[0].Plan
	path := "/shift-plans/" + plan.ID
	if id == "overnight-shift" {
		start, e := time.Parse(time.RFC3339, p.StartsAt)
		if e != nil {
			return o, e
		}
		end, e := time.Parse(time.RFC3339, p.EndsAt)
		if e != nil {
			return o, e
		}
		if end.Sub(start) != 8*time.Hour || start.Hour() != 22 || end.Hour() != 6 || start.AddDate(0, 0, 1).Day() != end.Day() {
			return o, errors.New("overnight plan changed dates or duration")
		}
		// Plan creation only: the window starts tomorrow at 22:00, so the run is not started here.
		o.Status = created
		o.Evidence = plan
		return o, nil
	}
	approve := workstation.ShiftPlanApproveInputV1{CanonicalPayloadSHA256: plan.Revisions[0].CanonicalPayloadSHA256}
	if id == "self-approval" {
		return s.request(ctx, "planner", "POST", path+"/approvals", "self", "1", "application/json", wire(approve), nil)
	}
	if err = s.l.call(ctx, "approver", path+"/approvals", s.prefix+"approve", "1", approve, nil); err != nil {
		return o, err
	}
	role := "operator"
	if id == "membership-revoked" {
		issuer, e := s.fixtureSession(ctx, "revoked")
		if e != nil {
			return o, e
		}
		role = "revoked"
		if err = s.l.call(ctx, "owner", "/membership-changes", s.prefix+"grant-member", "0", map[string]any{"issuer": issuer, "subject": s.l.actors[role].subject, "role": "operator", "active": true}, nil); err != nil {
			return o, err
		}
	}
	a := p.Assignments[0]
	station := "/workstations/" + a.WorkstationID
	if err = s.l.call(ctx, "planner", station+"/authorizations", s.prefix+"grant", "0", workstation.StationAuthorizationInputV1{GranteeIssuer: plan.Revisions[0].SubmittedBy.Issuer, GranteeSubject: s.l.actors[role].subject, Action: workstation.AuthorizationGrant}, nil); err != nil {
		return o, err
	}
	var started workstation.ShiftRunV1
	if err = s.l.call(ctx, "planner", path+"/run", s.prefix+"start", "2", workstation.ShiftRunStartInputV1{SchemaVersion: 1, PlanRevision: 1}, &started); err != nil {
		return o, err
	}
	record := workstation.StationRecordBodyV1{SchemaVersion: 1, WorkstationID: a.WorkstationID, JobOrderID: a.JobOrderID, JobOrderRevision: 1, EventAt: time.Now().UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano)}
	for _, kind := range []workstation.RecordKind{workstation.KindAcknowledged, workstation.KindStarted} {
		record.Kind = kind
		if err = s.l.call(ctx, role, station+"/records", s.prefix+string(kind), "", record, nil); err != nil {
			return o, err
		}
	}
	if id == "membership-revoked" {
		if err = s.l.call(ctx, "owner", "/membership-changes", s.prefix+"revoke-member", "1", map[string]any{"issuer": plan.Revisions[0].SubmittedBy.Issuer, "subject": s.l.actors[role].subject, "role": "operator", "active": false}, nil); err != nil {
			return o, err
		}
		record.Kind = workstation.KindNote
		record.Comment = "Should be refused after membership revocation"
		return s.request(ctx, role, "POST", station+"/records", "after-revoke", "", "application/json", wire(record), nil)
	}
	good, reject := int64(10), int64(0)
	record.Kind = workstation.KindQuantity
	record.GoodQuantity = &good
	record.RejectQuantity = &reject
	var original workstation.StationRecordV1
	if err = s.l.call(ctx, role, station+"/records", s.prefix+"quantity", "", record, &original); err != nil {
		return o, err
	}
	end, _ := time.Parse(time.RFC3339, p.EndsAt)
	var ended workstation.ShiftRunV1
	if err = s.l.call(ctx, "planner", path+"/run/end", s.prefix+"end", strconv.FormatInt(started.PlanStateRevision, 10), workstation.ShiftRunEndInputV1{SchemaVersion: 1, NextShift: workstation.NextShiftV1{LineID: p.LineID, StartsAt: p.EndsAt, EndsAt: stamp(end.Add(8 * time.Hour)), Timezone: p.Timezone}}, &ended); err != nil {
		return o, err
	}
	good = 13
	record.CorrectsRecordID = original.ID
	var correction workstation.StationRecordV1
	o, err = s.request(ctx, role, "POST", station+"/records", "correction", "", "application/json", wire(record), &correction)
	if err != nil || o.Status != 201 {
		return o, err
	}
	var detail workstation.StationRecordDetailV1
	if err = s.l.call(ctx, "owner", station+"/records/"+original.ID, "", "", nil, &detail); err != nil {
		return o, err
	}
	if len(detail.Chain) != 2 || detail.Chain[0].ID != original.ID || detail.Chain[1].ID != correction.ID || *detail.Chain[0].Record.GoodQuantity != 10 || *detail.Chain[1].Record.GoodQuantity != 13 || detail.Chain[1].Record.CorrectsRecordID != original.ID {
		return o, errors.New("late correction did not preserve linear count history")
	}
	var summary workstation.ShiftSummarySnapshotV1
	if err = s.l.call(ctx, "owner", path+"/summaries/2", "", "", nil, &summary); err != nil {
		return o, err
	}
	if len(summary.Summary.Stations) != 1 || summary.Summary.Stations[0].Recorded.GoodQuantity == nil || *summary.Summary.Stations[0].Recorded.GoodQuantity != 13 {
		return o, errors.New("late correction did not revise ended summary total to 13")
	}
	o.Evidence = map[string]any{"ended_run": ended, "chain": detail, "revised_summary": summary}
	return o, nil
}
