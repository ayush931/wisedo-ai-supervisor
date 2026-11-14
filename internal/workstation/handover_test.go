package workstation

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// handoverFixture is the summary fixture with job orders behind both assignments, a station
// A partial run and nothing recorded at station B.
type handoverFixture struct {
	*summaryFixture
	orders map[string]HandoverJobOrderV1
	next   NextShiftV1
	ids    map[string]string
}

const (
	instructionV1 = "1111111111111111111111111111111111111111111111111111111111111111"
	instructionV2 = "2222222222222222222222222222222222222222222222222222222222222222"
	handoverDoc   = "f0000000-0000-4000-8000-00000000000f"
)

func handoverOrder(revision int64, target int64, cycle, sha string, instructionRevision int64) JobOrderRevisionV1 {
	return JobOrderRevisionV1{Revision: revision, Order: JobOrderBodyV1{SchemaVersion: 1, OrderID: "a2000000-0000-4000-8000-0000000000a2", OperationID: "a3000000-0000-4000-8000-0000000000a3",
		ItemCode: "P-100", TargetQuantity: target, TargetCycleTimeSeconds: cycle, InstructionDocumentID: handoverDoc, InstructionRevision: instructionRevision, InstructionSHA256: sha,
		Template: "production", Fields: []FieldV1{{Name: "good_units", Label: "Good units", Kind: "decimal", Required: true, Unit: "count"}}}}
}

func newHandoverFixture() *handoverFixture {
	f := &handoverFixture{summaryFixture: newSummaryFixture(), ids: map[string]string{}}
	for i := range f.in.Approved.Plan.Assignments {
		x := &f.in.Approved.Plan.Assignments[i]
		x.InstructionDocumentID, x.InstructionRevision, x.InstructionSHA256 = handoverDoc, 1, instructionV1
	}
	f.orders = map[string]HandoverJobOrderV1{f.orderA: {Current: handoverOrder(1, 100, "3.1", instructionV1, 1)}, f.orderB: {Current: handoverOrder(1, 50, "", instructionV1, 1)}}
	f.next = NextShiftV1{LineID: f.in.LineID, StartsAt: "2026-10-20T16:00:00+02:00", EndsAt: "2026-10-21T00:00:00+02:00", Timezone: "Europe/Berlin"}
	a := f.stationA
	f.add(a, KindStarted, 1, nil)
	f.ids["qty"] = f.qty(a, 10, 30, 1)
	f.ids["c1"] = f.cycle(11, "2.75", "")
	f.ids["c2"] = f.cycle(12, "2.6", f.ids["c1"])
	f.ids["hold"] = f.add(a, KindQualityDisposition, 16, func(r *StationRecordV1) {
		r.Record.Disposition, r.Record.AuthorizesRelease, r.Record.Comment = DispositionHold, ptr(false), "Hold tag on bin 3"
	})
	f.ids["accept"] = f.add(a, KindQualityDisposition, 17, func(r *StationRecordV1) {
		r.Record.Disposition, r.Record.AuthorizesRelease = DispositionAccept, ptr(false)
	})
	f.ids["material"] = f.add(a, KindMaterialException, 18, func(r *StationRecordV1) { r.Record.MaterialCode, r.Record.MaterialIssue = "COIL-7", MaterialShortage })
	f.ids["pause"] = f.add(a, KindPaused, 30, nil)
	f.add(a, KindResumed, 40, nil)
	f.ids["late"] = f.add(a, KindNote, 41, func(r *StationRecordV1) { r.Record.Comment, r.Late = "Late: lead informed", true })
	f.ids["block"] = f.add(a, KindBlocked, 470, func(r *StationRecordV1) { r.Record.Comment = "Waiting for coil" })
	return f
}

func (f *handoverFixture) draft(t *testing.T) (ShiftHandoverV1, string) {
	t.Helper()
	s, digest := f.run(t)
	h, _, d, err := DraftHandover(f.sc, ShiftHandoverInput{SummaryRevision: 3, SummarySHA256: digest, Summary: s, Approved: f.in.Approved, JobOrders: f.orders, Next: f.next})
	if err != nil {
		t.Fatal(err)
	}
	return h, d
}

func handoverJob(t *testing.T, h ShiftHandoverV1, workstation string) HandoverJobV1 {
	t.Helper()
	for _, j := range h.Jobs {
		if j.WorkstationID == workstation {
			return j
		}
	}
	t.Fatalf("no job for %s", workstation)
	return HandoverJobV1{}
}

// TestDraftHandoverCarriesUnfinishedWork pins the draft for a partial station A run and an
// unrecorded station B: A carries forward its exact remaining quantity at the current job
// order revision with the order's declared cycle-time target; B's remaining is unknown, so B
// is listed but not proposed; blockers, holds, material, late and corrected entries are
// carried with their record IDs.
func TestDraftHandoverCarriesUnfinishedWork(t *testing.T) {
	f := newHandoverFixture()
	h, _ := f.draft(t)
	if h.PlanRevision != 2 || h.SummaryRevision != 3 || len(h.SummarySHA256) != 64 || h.Source.Readiness != SummaryNotReady || !h.Source.ShiftEnded ||
		!reflect.DeepEqual(h.Source.Reasons, []SummaryReason{ReasonCorrections, ReasonLateEntries, ReasonNoRecords, ReasonUnclosedDowntime}) || h.Source.SourceRecordCount != 11 || len(h.Jobs) != 2 {
		t.Fatalf("handover header: %+v", h.Source)
	}
	a := handoverJob(t, h, f.stationA)
	if a.CompletionState != CompletionInProgress || *a.GoodQuantity != 30 || *a.RemainingQuantity != 70 || !a.Proposed || a.NotProposedReason != "" ||
		a.ShiftTargetQuantity != 100 || a.JobOrderChanged || a.Instruction.ChangedSincePin || !a.Instruction.Approved || a.Instruction.Reason != "" {
		t.Fatalf("job A: %+v", a)
	}
	if len(a.OpenBlockers) != 1 || a.OpenBlockers[0].StartRecordID != f.ids["block"] || !a.OpenBlockers[0].Open || a.OpenBlockers[0].Reason != "Waiting for coil" ||
		len(a.QualityHolds) != 1 || a.QualityHolds[0].RecordID != f.ids["hold"] || len(a.MaterialExceptions) != 1 || a.MaterialExceptions[0].RecordID != f.ids["material"] ||
		!reflect.DeepEqual(a.LateRecordIDs, []string{f.ids["late"]}) || !reflect.DeepEqual(a.Corrections, []CorrectionChainV1{{RecordID: f.ids["c2"], Supersedes: []string{f.ids["c1"]}}}) ||
		len(a.RecordIDs) != 11 {
		t.Fatalf("job A constraints: %+v", a)
	}
	b := handoverJob(t, h, f.stationB)
	if b.RemainingQuantity != nil || b.GoodQuantity != nil || b.Proposed || b.NotProposedReason != CarryRemainingUnknown || b.CompletionState != CompletionUnknown || b.StationReadiness != SummaryNotReady {
		t.Fatalf("job B: %+v", b)
	}
	want := ShiftPlanBodyV1{SchemaVersion: 1, LineID: f.in.LineID, StartsAt: f.next.StartsAt, EndsAt: f.next.EndsAt, Timezone: "Europe/Berlin", Assignments: []AssignmentV1{{
		WorkstationID: f.stationA, JobOrderID: f.orderA, JobOrderRevision: 1, OrderID: "a2000000-0000-4000-8000-0000000000a2", OperationID: "a3000000-0000-4000-8000-0000000000a3",
		ItemCode: "P-100", TargetQuantity: 70, TargetCycleTimeSeconds: "3.1", InstructionDocumentID: handoverDoc, InstructionRevision: 1, InstructionSHA256: instructionV1,
		Template: "production", Fields: []FieldV1{{Name: "good_units", Label: "Good units", Kind: "decimal", Required: true, Unit: "count"}}}}}
	if h.ProposedPlan == nil || !reflect.DeepEqual(*h.ProposedPlan, want) {
		t.Fatalf("proposal:\n%+v\nwant\n%+v", h.ProposedPlan, want)
	}
	if len(h.Notes) != 1 || h.Notes[0].RecordID != f.ids["late"] || !h.Notes[0].Late {
		t.Fatalf("notes: %+v", h.Notes)
	}
}

// TestDraftHandoverNotProposed: each job that cannot be carried forward is listed with its
// stable reason and left out of the proposal; with nothing to carry the proposal is null.
func TestDraftHandoverNotProposed(t *testing.T) {
	cases := []struct {
		name   string
		edit   func(f *handoverFixture)
		reason CarryReason
		remain *int64
	}{
		{"completed", func(f *handoverFixture) { f.add(f.stationA, KindCompletionEvidence, 475, nil) }, CarryCompleted, ptr(int64(70))},
		{"nothing remaining", func(f *handoverFixture) { f.qty(f.stationA, 20, 70, 0) }, CarryNothingRemaining, ptr(int64(0))},
		{"over production", func(f *handoverFixture) { f.qty(f.stationA, 20, 75, 0) }, CarryNothingRemaining, ptr(int64(-5))},
		{"exceeds revised order target", func(f *handoverFixture) {
			f.orders[f.orderA] = HandoverJobOrderV1{Current: handoverOrder(2, 60, "3.1", instructionV1, 1)}
		}, CarryExceedsOrderTarget, ptr(int64(70))},
		{"instruction unavailable", func(f *handoverFixture) {
			f.orders[f.orderA] = HandoverJobOrderV1{Current: f.orders[f.orderA].Current, InstructionReason: InstructionRevoked}
		}, CarryInstructionUnavailable, ptr(int64(70))},
		// An unparseable recorded value makes the station INVALID: its recorded remaining
		// quantity is not reported, so nothing is guessed or carried.
		{"invalid station", func(f *handoverFixture) { f.cycle(20, "-1", "") }, CarryRemainingUnknown, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newHandoverFixture()
			c.edit(f)
			h, _ := f.draft(t)
			a := handoverJob(t, h, f.stationA)
			if a.Proposed || a.NotProposedReason != c.reason || !reflect.DeepEqual(a.RemainingQuantity, c.remain) || h.ProposedPlan != nil {
				t.Fatalf("job A: proposed=%v reason=%s remaining=%v plan=%v", a.Proposed, a.NotProposedReason, a.RemainingQuantity, h.ProposedPlan)
			}
		})
	}
}

// TestDraftHandoverInstructionChanged: when the job order was revised after the shift's pin
// with a new instruction, the draft flags both changes, reports the current instruction and
// pins the proposal to the current revision and its instruction.
func TestDraftHandoverInstructionChanged(t *testing.T) {
	f := newHandoverFixture()
	f.orders[f.orderA] = HandoverJobOrderV1{Current: handoverOrder(2, 100, "", instructionV2, 2)}
	h, _ := f.draft(t)
	a := handoverJob(t, h, f.stationA)
	if !a.JobOrderChanged || a.PinnedJobOrderRevision != 1 || a.CurrentJobOrderRevision != 2 || !a.Instruction.ChangedSincePin ||
		a.Instruction != (HandoverInstructionV1{DocumentID: handoverDoc, Revision: 2, SHA256: instructionV2, Approved: true, ChangedSincePin: true}) {
		t.Fatalf("job A flags: %+v", a)
	}
	x := h.ProposedPlan.Assignments[0]
	// The order declares no cycle-time target, so none is proposed even though shift 1 had one.
	if x.JobOrderRevision != 2 || x.InstructionRevision != 2 || x.InstructionSHA256 != instructionV2 || x.TargetCycleTimeSeconds != "" || x.TargetQuantity != 70 {
		t.Fatalf("proposal pin: %+v", x)
	}
}

// TestDraftHandoverDigest: identical inputs give an identical digest; a changed job order
// head, next shift or cited summary changes it.
func TestDraftHandoverDigest(t *testing.T) {
	f := newHandoverFixture()
	_, base := f.draft(t)
	if _, again := f.draft(t); again != base {
		t.Fatalf("digest not stable: %s %s", base, again)
	}
	changes := map[string]func(f *handoverFixture){
		"job order head": func(f *handoverFixture) {
			f.orders[f.orderB] = HandoverJobOrderV1{Current: handoverOrder(2, 50, "", instructionV1, 1)}
		},
		"next shift":     func(f *handoverFixture) { f.next.EndsAt = "2026-10-20T23:00:00+02:00" },
		"summary record": func(f *handoverFixture) { f.qty(f.stationA, 20, 1, 0) },
	}
	for name, edit := range changes {
		g := newHandoverFixture()
		edit(g)
		if _, d := g.draft(t); d == base {
			t.Fatalf("%s did not change the digest", name)
		}
	}
}

// TestDraftHandoverRefusesInvalidInput: a next shift that starts before the summarized shift
// ends is refused, and a plan revision that is not the one the summary cites is an integrity
// failure, never a silently re-targeted draft.
func TestDraftHandoverRefusesInvalidInput(t *testing.T) {
	f := newHandoverFixture()
	s, digest := f.run(t)
	in := ShiftHandoverInput{SummaryRevision: 1, SummarySHA256: digest, Summary: s, Approved: f.in.Approved, JobOrders: f.orders, Next: f.next}
	early := in
	early.Next.StartsAt = "2026-10-20T15:59:00+02:00"
	var config *ConfigError
	if _, _, _, err := DraftHandover(f.sc, early); !errors.As(err, &config) || config.Code != CodeInvalidShiftHandover || config.Path != "next_shift" {
		t.Fatalf("early next shift: %v", err)
	}
	zone := in
	zone.Next.Timezone = "UTC"
	if _, _, _, err := DraftHandover(f.sc, zone); !errors.As(err, &config) || config.Path != "next_shift" {
		t.Fatalf("offset not matching timezone: %v", err)
	}
	other := in
	other.Approved.CanonicalPayloadSHA256 = strings.Repeat("c", 64)
	if _, _, _, err := DraftHandover(f.sc, other); !errors.Is(err, errIntegrity) {
		t.Fatalf("mismatched plan revision: %v", err)
	}
	missing := in
	missing.JobOrders = map[string]HandoverJobOrderV1{f.orderA: f.orders[f.orderA]}
	if _, _, _, err := DraftHandover(f.sc, missing); !errors.Is(err, errIntegrity) {
		t.Fatalf("missing job order: %v", err)
	}
}

// TestDraftHandoverProposalIsStoredAsIs: a multi-station proposal is already in the order
// the shift plan service stores it, so migration 038's adoption guard (stored plan payload
// equals the draft's proposed_plan) holds for any adopted draft.
func TestDraftHandoverProposalIsStoredAsIs(t *testing.T) {
	f := newHandoverFixture()
	f.add(f.stationB, KindStarted, 2, nil)
	f.qty(f.stationB, 3, 20, 0)
	h, _ := f.draft(t)
	if h.ProposedPlan == nil || len(h.ProposedPlan.Assignments) != 2 || h.ProposedPlan.Assignments[1].TargetQuantity != 30 {
		t.Fatalf("proposal: %+v", h.ProposedPlan)
	}
	stored, _, _, err := normalized(f.sc.TenantID, f.sc.SiteID, *h.ProposedPlan)
	want, _ := json.Marshal(*h.ProposedPlan)
	got, _ := json.Marshal(stored)
	if err != nil || string(got) != string(want) {
		t.Fatalf("stored body differs (%v):\n%s\n%s", err, got, want)
	}
}
