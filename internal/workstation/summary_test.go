package workstation

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// summaryFixture is one approved plan revision assigning two workstations, and a builder for
// records bound to it. Event times are minutes after the 06:00 UTC shift start.
type summaryFixture struct {
	sc       Scope
	in       ShiftSummaryInput
	stationA string
	stationB string
	orderA   string
	orderB   string
	next     int
}

const (
	summaryTenant = "11111111-1111-4111-8111-111111111111"
	summarySite   = "22222222-2222-4222-8222-222222222222"
)

func newSummaryFixture() *summaryFixture {
	f := &summaryFixture{sc: Scope{summaryTenant, summarySite}, stationA: "a0000000-0000-4000-8000-00000000000a", stationB: "b0000000-0000-4000-8000-00000000000b",
		orderA: "a1000000-0000-4000-8000-0000000000a1", orderB: "b1000000-0000-4000-8000-0000000000b1"}
	start := time.Date(2026, 10, 20, 6, 0, 0, 0, time.UTC)
	assign := func(station, order string, qty int64, cycle string) AssignmentV1 {
		return AssignmentV1{WorkstationID: station, JobOrderID: order, JobOrderRevision: 1, ItemCode: "P-100", TargetQuantity: qty, TargetCycleTimeSeconds: cycle}
	}
	f.in = ShiftSummaryInput{PlanID: "c0000000-0000-4000-8000-00000000000c", LineID: "d0000000-0000-4000-8000-00000000000d", StartsAt: start, EndsAt: start.Add(8 * time.Hour),
		Approved: ShiftPlanRevisionV1{Revision: 2, CanonicalPayloadSHA256: "ab" + fmt.Sprintf("%062d", 0),
			Plan: ShiftPlanBodyV1{Assignments: []AssignmentV1{assign(f.stationA, f.orderA, 100, "2.5"), assign(f.stationB, f.orderB, 50, "")}}},
		AsOf: start.Add(8 * time.Hour)}
	return f
}

// add appends a record at minute m on station A (or B) and returns its ID.
func (f *summaryFixture) add(station string, kind RecordKind, m int, edit func(*StationRecordV1)) string {
	f.next++
	order := f.orderA
	if station == f.stationB {
		order = f.orderB
	}
	at := f.in.StartsAt.Add(time.Duration(m) * time.Minute)
	r := StationRecordV1{ID: fmt.Sprintf("e0000000-0000-4000-8000-%012d", f.next), PlanID: f.in.PlanID, PlanRevision: 2, EventAt: at, ReceivedAt: at.Add(time.Second),
		RecordedBy: ActorRefV1{"https://idp.invalid", "operator-1"}, Source: SourceManualTablet,
		Record: StationRecordBodyV1{SchemaVersion: 1, Kind: kind, WorkstationID: station, JobOrderID: order, JobOrderRevision: 1, EventAt: at.Format(time.RFC3339Nano)}}
	if edit != nil {
		edit(&r)
	}
	f.in.Records = append(f.in.Records, r)
	return r.ID
}

func (f *summaryFixture) qty(station string, m int, good, reject int64) string {
	return f.add(station, KindQuantity, m, func(r *StationRecordV1) { r.Record.GoodQuantity, r.Record.RejectQuantity = ptr(good), ptr(reject) })
}

func (f *summaryFixture) cycle(m int, seconds, corrects string) string {
	return f.add(f.stationA, KindCycleTime, m, func(r *StationRecordV1) { r.Record.CycleTimeSeconds, r.Record.CorrectsRecordID = seconds, corrects })
}

func (f *summaryFixture) run(t *testing.T) (ShiftSummaryV1, string) {
	t.Helper()
	s, _, digest, err := SummarizeShift(f.sc, f.in)
	if err != nil {
		t.Fatal(err)
	}
	return s, digest
}

func station(t *testing.T, s ShiftSummaryV1, workstation string) StationSummaryV1 {
	t.Helper()
	for _, st := range s.Stations {
		if st.WorkstationID == workstation {
			return st
		}
	}
	t.Fatalf("no station %s", workstation)
	return StationSummaryV1{}
}

// TestSummarizeShiftFigures pins planned versus recorded figures on a full station A run
// and an unrecorded station B, with exact values.
func TestSummarizeShiftFigures(t *testing.T) {
	f := newSummaryFixture()
	a := f.stationA
	f.add(a, KindAcknowledged, 0, nil)
	f.add(a, KindStarted, 1, nil)
	q1 := f.qty(a, 10, 40, 2)
	q2 := f.qty(a, 20, 35, 1)
	c1 := f.cycle(11, "2.75", "")
	c2 := f.cycle(12, "2.6", c1) // corrects c1: only 2.6 counts
	c3 := f.cycle(13, "2.600001", "")
	m1 := f.add(a, KindQualityMeasurement, 15, func(r *StationRecordV1) {
		r.Record.Values = []EntryValueV1{{Field: "bore", Value: "10.01", Unit: "mm"}}
	})
	hold := f.add(a, KindQualityDisposition, 16, func(r *StationRecordV1) {
		r.Record.Disposition, r.Record.AuthorizesRelease, r.Record.Comment = DispositionHold, ptr(false), "Hold tag on bin 3"
	})
	mat := f.add(a, KindMaterialException, 17, func(r *StationRecordV1) { r.Record.MaterialCode, r.Record.MaterialIssue = "COIL-7", MaterialShortage })
	p := f.add(a, KindPaused, 30, func(r *StationRecordV1) { r.Record.Comment = "Coil change" })
	res := f.add(a, KindResumed, 42, func(r *StationRecordV1) { r.EventAt = r.EventAt.Add(500 * time.Microsecond) })
	note := f.add(a, KindNote, 43, func(r *StationRecordV1) {
		r.Record.Comment, r.RecordedBy = "Operator says die is worn", ActorRefV1{"https://idp.invalid", "operator-2"}
	})
	done := f.add(a, KindCompletionEvidence, 470, func(r *StationRecordV1) {
		r.Record.Values = []EntryValueV1{{Field: "good_units", Value: "75", Unit: "count"}}
	})

	s, _ := f.run(t)
	st := station(t, s, a)
	rec := st.Recorded
	if st.Planned == nil || st.Planned.TargetQuantity != 100 || *st.Planned.TargetCycleTimeSeconds != "2.5" || st.Planned.JobOrderRevision != 1 {
		t.Fatalf("planned: %+v", st.Planned)
	}
	if *rec.GoodQuantity != 75 || *rec.RejectQuantity != 3 || *rec.RemainingQuantity != 25 || rec.OverProduction || !reflect.DeepEqual(rec.QuantityRecordIDs, []string{q1, q2}) {
		t.Fatalf("quantities: good=%v reject=%v remaining=%v ids=%v", *rec.GoodQuantity, *rec.RejectQuantity, *rec.RemainingQuantity, rec.QuantityRecordIDs)
	}
	// (2.6 + 2.600001) / 2 = 2.6000005: the tie rounds half-even to 2.600000, not up.
	want := CycleTimeStatsV1{Count: 2, MinSeconds: "2.6", MaxSeconds: "2.600001", SumSeconds: "5.200001", MeanSeconds: "2.6", RecordIDs: []string{c2, c3}}
	if !reflect.DeepEqual(*rec.CycleTime, want) {
		t.Fatalf("cycle time %+v want %+v", *rec.CycleTime, want)
	}
	if !reflect.DeepEqual(rec.Corrections, []CorrectionChainV1{{RecordID: c2, Supersedes: []string{c1}}}) {
		t.Fatalf("corrections %+v", rec.Corrections)
	}
	if rec.Quality.MeasurementCount != 1 || rec.Quality.MeasurementRecordIDs[0] != m1 || rec.Quality.HoldsAsRecorded != 1 || rec.Quality.AcceptsAsRecorded != 0 ||
		rec.Quality.DispositionsAsRecorded[0].RecordID != hold || rec.Quality.DispositionsAsRecorded[0].Comment != "Hold tag on bin 3" {
		t.Fatalf("quality %+v", rec.Quality)
	}
	if len(rec.MaterialExceptions) != 1 || rec.MaterialExceptions[0].RecordID != mat || rec.MaterialExceptions[0].Issue != MaterialShortage {
		t.Fatalf("material %+v", rec.MaterialExceptions)
	}
	if len(rec.Downtime) != 1 || rec.Downtime[0].Open || rec.Downtime[0].StartRecordID != p || *rec.Downtime[0].EndRecordID != res || *rec.Downtime[0].DurationSeconds != "720.0005" ||
		rec.ClosedDowntime != "720.0005" || rec.Downtime[0].Reason != "Coil change" {
		t.Fatalf("downtime %+v closed %s", rec.Downtime, rec.ClosedDowntime)
	}
	if rec.CompletionState != CompletionCompleted || *rec.CompletionRecordID != done || rec.CompletionValues[0].Value != "75" {
		t.Fatalf("completion %+v", rec)
	}
	// Notes are attributed and only in the notes section.
	if !reflect.DeepEqual(s.Notes, []SummaryNoteV1{{a, f.orderA, note, "Operator says die is worn", ActorRefV1{"https://idp.invalid", "operator-2"}, f.in.StartsAt.Add(43 * time.Minute), false}}) {
		t.Fatalf("notes %+v", s.Notes)
	}
	if st.Readiness != SummaryReady || !reflect.DeepEqual(st.Reasons, []SummaryReason{ReasonCorrections}) || len(st.RecordIDs) != 14 {
		t.Fatalf("station A readiness %s %v records %d", st.Readiness, st.Reasons, len(st.RecordIDs))
	}
	// Station B: nothing recorded is unknown and NOT_READY, never zero.
	b := station(t, s, f.stationB)
	if b.Readiness != SummaryNotReady || !reflect.DeepEqual(b.Reasons, []SummaryReason{ReasonNoRecords}) || b.Recorded.GoodQuantity != nil || b.Recorded.RejectQuantity != nil ||
		b.Recorded.RemainingQuantity != nil || b.Recorded.CycleTime != nil || b.Recorded.CompletionState != CompletionUnknown || b.Planned.TargetCycleTimeSeconds != nil {
		t.Fatalf("station B %+v", b)
	}
	encoded, _ := json.Marshal(b.Recorded)
	for _, member := range []string{`"good_quantity":null`, `"remaining_quantity":null`, `"cycle_time":null`} {
		if !strings.Contains(string(encoded), member) {
			t.Fatalf("station B recorded %s lacks %s", encoded, member)
		}
	}
	// The plan-level reasons explain the roll-up: each station reason once, sorted.
	if s.Readiness != SummaryNotReady || s.Jobs != (ShiftJobCountsV1{Planned: 2, Completed: 1, Unknown: 1, Remaining: 1}) || s.SourceRecordCount != 14 ||
		!reflect.DeepEqual(s.Reasons, []SummaryReason{ReasonCorrections, ReasonNoRecords}) || !s.ShiftEnded || !s.AsOf.Equal(f.in.EndsAt) {
		t.Fatalf("plan readiness %s jobs %+v reasons %v", s.Readiness, s.Jobs, s.Reasons)
	}
}

// TestSummarizeShiftReadiness pins each reason code to the records that raise it.
func TestSummarizeShiftReadiness(t *testing.T) {
	for name, tc := range map[string]struct {
		build     func(f *summaryFixture)
		readiness SummaryReadiness
		reasons   []SummaryReason
		check     func(t *testing.T, st StationSummaryV1)
	}{
		"over-production keeps the negative remaining": {func(f *summaryFixture) {
			f.add(f.stationA, KindStarted, 1, nil)
			f.qty(f.stationA, 460, 104, 0)
			f.cycle(461, "2.5", "")
		}, SummaryLimited, []SummaryReason{ReasonOverProduction}, func(t *testing.T, st StationSummaryV1) {
			if *st.Recorded.RemainingQuantity != -4 || !st.Recorded.OverProduction {
				t.Fatalf("remaining %d over %v", *st.Recorded.RemainingQuantity, st.Recorded.OverProduction)
			}
		}},
		"unclosed block is open with no end or duration": {func(f *summaryFixture) {
			f.add(f.stationA, KindStarted, 1, nil)
			f.qty(f.stationA, 440, 10, 0)
			f.cycle(441, "2.5", "")
			f.add(f.stationA, KindBlocked, 460, func(r *StationRecordV1) { r.Record.Comment = "Waiting for coil" })
		}, SummaryLimited, []SummaryReason{ReasonUnclosedDowntime}, func(t *testing.T, st StationSummaryV1) {
			d := st.Recorded.Downtime
			if len(d) != 1 || !d[0].Open || d[0].EndsAt != nil || d[0].DurationSeconds != nil || d[0].Kind != KindBlocked || st.Recorded.ClosedDowntime != "0" {
				t.Fatalf("downtime %+v", d)
			}
		}},
		"stale input after the window": {func(f *summaryFixture) {
			f.add(f.stationA, KindStarted, 1, nil)
			f.qty(f.stationA, 2, 10, 0)
			f.cycle(449, "2.5", "") // 31 minutes before the 08:00 shift end
		}, SummaryLimited, []SummaryReason{ReasonStaleInput}, nil},
		"inside the window is not stale": {func(f *summaryFixture) {
			f.add(f.stationA, KindStarted, 1, nil)
			f.qty(f.stationA, 2, 10, 0)
			f.cycle(451, "2.5", "")
		}, SummaryReady, []SummaryReason{}, nil},
		"started without quantities or cycle times": {func(f *summaryFixture) {
			f.add(f.stationA, KindAcknowledged, 470, nil)
			f.add(f.stationA, KindStarted, 471, nil)
		}, SummaryLimited, []SummaryReason{ReasonNoQuantityRecords, ReasonNoCycleTimeRecords}, func(t *testing.T, st StationSummaryV1) {
			if st.Recorded.GoodQuantity != nil || st.Recorded.CompletionState != CompletionInProgress {
				t.Fatalf("recorded %+v", st.Recorded)
			}
		}},
		"late entry on a superseded plan revision": {func(f *summaryFixture) {
			f.add(f.stationA, KindStarted, 1, func(r *StationRecordV1) { r.PlanRevision, r.Late = 1, true })
			f.qty(f.stationA, 470, 10, 0)
			f.cycle(471, "2.5", "")
		}, SummaryLimited, []SummaryReason{ReasonLateEntries, ReasonSupersededBinding}, func(t *testing.T, st StationSummaryV1) {
			if len(st.Recorded.LateRecordIDs) != 1 || !reflect.DeepEqual(st.BoundPlanRevisions, []int64{1, 2}) {
				t.Fatalf("late %v bound %v", st.Recorded.LateRecordIDs, st.BoundPlanRevisions)
			}
		}},
		"notes and superseded records do not keep a job fresh or flag it": {func(f *summaryFixture) {
			f.add(f.stationA, KindStarted, 1, nil)
			f.qty(f.stationA, 2, 10, 0)
			c1 := f.cycle(470, "2.75", "")
			f.in.Records[len(f.in.Records)-1].Late, f.in.Records[len(f.in.Records)-1].PlanRevision = true, 1
			f.cycle(10, "2.5", c1)
			f.add(f.stationA, KindNote, 475, func(r *StationRecordV1) { r.Record.Comment = "still here" })
		}, SummaryLimited, []SummaryReason{ReasonStaleInput, ReasonCorrections}, func(t *testing.T, st StationSummaryV1) {
			if !st.Recorded.LastEventAt.Equal(time.Date(2026, 10, 20, 6, 10, 0, 0, time.UTC)) || len(st.Recorded.LateRecordIDs) != 0 || !reflect.DeepEqual(st.BoundPlanRevisions, []int64{2}) {
				t.Fatalf("last %v late %v bound %v", st.Recorded.LastEventAt, st.Recorded.LateRecordIDs, st.BoundPlanRevisions)
			}
		}},
		"an unparseable stored value is invalid, not skipped": {func(f *summaryFixture) {
			f.add(f.stationA, KindStarted, 1, nil)
			f.qty(f.stationA, 470, 10, 0)
			f.cycle(471, "abc", "")
		}, SummaryInvalid, []SummaryReason{ReasonInvalidValue, ReasonNoCycleTimeRecords}, func(t *testing.T, st StationSummaryV1) {
			if st.Recorded.CycleTime != nil {
				t.Fatalf("cycle time %+v", st.Recorded.CycleTime)
			}
		}},
		"overflowing quantities are invalid, not wrapped": {func(f *summaryFixture) {
			f.add(f.stationA, KindStarted, 1, nil)
			f.qty(f.stationA, 470, 1<<62, 0)
			f.qty(f.stationA, 471, 1<<62, 0)
			f.qty(f.stationA, 472, 1<<62, 0)
			f.cycle(473, "2.5", "")
		}, SummaryInvalid, []SummaryReason{ReasonQuantityOverflow}, func(t *testing.T, st StationSummaryV1) {
			if st.Recorded.GoodQuantity != nil {
				t.Fatalf("good %d", *st.Recorded.GoodQuantity)
			}
		}},
	} {
		t.Run(name, func(t *testing.T) {
			f := newSummaryFixture()
			tc.build(f)
			st := station(t, mustRun(t, f), f.stationA)
			if st.Readiness != tc.readiness || !reflect.DeepEqual(st.Reasons, tc.reasons) {
				t.Fatalf("readiness %s %v want %s %v", st.Readiness, st.Reasons, tc.readiness, tc.reasons)
			}
			if tc.check != nil {
				tc.check(t, st)
			}
		})
	}
}

func mustRun(t *testing.T, f *summaryFixture) ShiftSummaryV1 {
	s, _ := f.run(t)
	return s
}

// TestSummarizeShiftScopeAndProgress: a record for a job order the approved revision does
// not assign stays visible without planned figures; a shift in progress is LIMITED and is
// not judged stale.
func TestSummarizeShiftScopeAndProgress(t *testing.T) {
	f := newSummaryFixture()
	f.add(f.stationA, KindStarted, 1, func(r *StationRecordV1) {
		r.Record.JobOrderID, r.PlanRevision = "f0000000-0000-4000-8000-00000000000f", 1
	})
	f.in.AsOf = f.in.StartsAt.Add(time.Hour)
	s := mustRun(t, f)
	if len(s.Stations) != 3 || s.Jobs.Planned != 2 || s.Readiness != SummaryNotReady || s.ShiftEnded || !s.AsOf.Equal(f.in.AsOf) ||
		!reflect.DeepEqual(s.Reasons, []SummaryReason{ReasonNotInApprovedPlan, ReasonNoCycleTimeRecords, ReasonNoQuantityRecords, ReasonNoRecords, ReasonShiftInProgress, ReasonSupersededBinding}) {
		t.Fatalf("summary %d stations jobs %+v reasons %v readiness %s", len(s.Stations), s.Jobs, s.Reasons, s.Readiness)
	}
	var unplanned StationSummaryV1
	for _, st := range s.Stations {
		if st.JobOrderID == "f0000000-0000-4000-8000-00000000000f" {
			unplanned = st
		}
	}
	// A plan with nothing assigned and nothing recorded is NOT_READY, never READY.
	empty := newSummaryFixture()
	empty.in.Approved.Plan.Assignments = nil
	if e := mustRun(t, empty); e.Readiness != SummaryNotReady || !reflect.DeepEqual(e.Reasons, []SummaryReason{ReasonNoRecords}) || len(e.Stations) != 0 {
		t.Fatalf("empty plan %s %v", e.Readiness, e.Reasons)
	}
	if unplanned.Planned != nil || !reflect.DeepEqual(unplanned.Reasons, []SummaryReason{ReasonNoQuantityRecords, ReasonNoCycleTimeRecords, ReasonSupersededBinding, ReasonNotInApprovedPlan}) {
		t.Fatalf("unplanned station %+v", unplanned)
	}
}

// TestSummarizeShiftDigest: the digest ignores record order and generation time (the kernel
// has none), and changes with any record, correction, plan revision or scope.
func TestSummarizeShiftDigest(t *testing.T) {
	base := func() *summaryFixture {
		f := newSummaryFixture()
		f.add(f.stationA, KindStarted, 1, nil)
		f.qty(f.stationA, 10, 40, 2)
		c := f.cycle(11, "2.75", "")
		f.cycle(12, "2.65", c)
		f.add(f.stationA, KindNote, 13, func(r *StationRecordV1) { r.Record.Comment = "check" })
		return f
	}
	f := base()
	_, encoded, digest, err := SummarizeShift(f.sc, f.in)
	if err != nil || len(digest) != 64 {
		t.Fatalf("digest %q %v", digest, err)
	}
	reversed := base()
	for i, j := 0, len(reversed.in.Records)-1; i < j; i, j = i+1, j-1 {
		reversed.in.Records[i], reversed.in.Records[j] = reversed.in.Records[j], reversed.in.Records[i]
	}
	if _, again, d, _ := SummarizeShift(reversed.sc, reversed.in); d != digest || string(again) != string(encoded) {
		t.Fatal("record order changed the digest")
	}
	// After the shift end, as_of is the shift end: generating later changes nothing.
	later := base()
	later.in.AsOf = later.in.EndsAt.Add(5 * time.Hour)
	if _, _, d, _ := SummarizeShift(later.sc, later.in); d != digest {
		t.Fatal("a later generation instant changed the digest of an ended shift")
	}
	for name, edit := range map[string]func(f *summaryFixture){
		"another record":     func(f *summaryFixture) { f.qty(f.stationA, 20, 1, 0) },
		"another correction": func(f *summaryFixture) { f.cycle(14, "2.7", f.in.Records[3].ID) },
		"plan revision": func(f *summaryFixture) {
			f.in.Approved.Revision, f.in.Approved.CanonicalPayloadSHA256 = 3, "cd"+fmt.Sprintf("%062d", 0)
		},
		"target":            func(f *summaryFixture) { f.in.Approved.Plan.Assignments[0].TargetQuantity = 90 },
		"shift in progress": func(f *summaryFixture) { f.in.AsOf = f.in.EndsAt.Add(-time.Minute) },
		"note text":         func(f *summaryFixture) { f.in.Records[4].Record.Comment = "checked" },
		"other site":        func(f *summaryFixture) { f.sc.SiteID = "33333333-3333-4333-8333-333333333333" },
	} {
		g := base()
		edit(g)
		if _, _, d, _ := SummarizeShift(g.sc, g.in); d == digest {
			t.Errorf("%s did not change the digest", name)
		}
	}
}

// TestWIS701ShiftRunSummaryEndedAt: a run ended before the planned end is summarized as ended
// at the run end (no SHIFT_IN_PROGRESS, staleness judged then) under formula version 2; an
// end after the planned end is capped there; a record at or after the run end is counted and
// flagged RECORDED_AFTER_RUN_END. Without EndedAt the bytes, digest and formula version are
// those stored before shift runs existed (digests pinned from the kernel at fb435022).
func TestWIS701ShiftRunSummaryEndedAt(t *testing.T) {
	const storedEnded, storedInProgress = "4a7d99fef39ade6612226315b316eb9cc7e51737827574b2b3320c9b377bb9dc", "bcdbc0fec50bf329de3281d45adaad0d819f3e0f0c6197059d06d7025009eca8"
	base := func() *summaryFixture {
		f := newSummaryFixture()
		f.add(f.stationA, KindStarted, 1, nil)
		f.qty(f.stationA, 10, 40, 2)
		c := f.cycle(11, "2.75", "")
		f.cycle(12, "2.65", c)
		f.add(f.stationA, KindNote, 13, func(r *StationRecordV1) { r.Record.Comment = "check" })
		return f
	}
	endAt := func(f *summaryFixture, d time.Duration) {
		v := f.in.StartsAt.Add(d)
		f.in.AsOf, f.in.EndedAt = v, &v
	}
	for name, tc := range map[string]struct {
		edit  func(f *summaryFixture)
		check func(t *testing.T, f *summaryFixture, s ShiftSummaryV1, encoded []byte, digest string)
	}{
		"no run end, after the shift: stored bytes and version": {func(f *summaryFixture) {}, func(t *testing.T, f *summaryFixture, s ShiftSummaryV1, encoded []byte, digest string) {
			if digest != storedEnded || s.FormulaVersion != 1 || strings.Contains(string(encoded), "run_ended_at") {
				t.Fatalf("digest %s version %d", digest, s.FormulaVersion)
			}
		}},
		"no run end, during the shift: stored bytes": {func(f *summaryFixture) { f.in.AsOf = f.in.StartsAt.Add(time.Hour) }, func(t *testing.T, f *summaryFixture, s ShiftSummaryV1, encoded []byte, digest string) {
			if digest != storedInProgress || s.FormulaVersion != 1 {
				t.Fatalf("digest %s version %d", digest, s.FormulaVersion)
			}
		}},
		// Last station event 10 minutes before the run end, but over 5 hours before the planned
		// end: fresh at the actual end, so no STALE_INPUT; judging at ends_at would flag it.
		"ended early, fresh at the run end": {func(f *summaryFixture) {
			f.qty(f.stationA, 170, 5, 0)
			endAt(f, 3*time.Hour)
		}, func(t *testing.T, f *summaryFixture, s ShiftSummaryV1, encoded []byte, digest string) {
			want := f.in.StartsAt.Add(3 * time.Hour)
			if !s.ShiftEnded || !s.AsOf.Equal(want) || s.RunEndedAt == nil || !s.RunEndedAt.Equal(want) || s.FormulaVersion != ShiftSummaryRunFormulaVersion {
				t.Fatalf("ended early: ended=%v as_of=%v run_ended_at=%v version=%d", s.ShiftEnded, s.AsOf, s.RunEndedAt, s.FormulaVersion)
			}
			if st := station(t, s, f.stationA); slices.Contains(st.Reasons, ReasonStaleInput) || slices.Contains(s.Reasons, ReasonShiftInProgress) || slices.Contains(s.Reasons, ReasonAfterRunEnd) {
				t.Fatalf("reasons %v / station %v", s.Reasons, st.Reasons)
			}
			if digest == storedEnded {
				t.Fatal("a run end did not change the digest")
			}
		}},
		"ended early, stale at the run end": {func(f *summaryFixture) { endAt(f, 3*time.Hour) }, func(t *testing.T, f *summaryFixture, s ShiftSummaryV1, encoded []byte, digest string) {
			if st := station(t, s, f.stationA); !slices.Contains(st.Reasons, ReasonStaleInput) {
				t.Fatalf("station reasons %v", st.Reasons)
			}
		}},
		"ended late is capped at the planned end": {func(f *summaryFixture) { endAt(f, 9*time.Hour) }, func(t *testing.T, f *summaryFixture, s ShiftSummaryV1, encoded []byte, digest string) {
			if !s.ShiftEnded || !s.AsOf.Equal(f.in.EndsAt) || !s.RunEndedAt.Equal(f.in.StartsAt.Add(9*time.Hour)) {
				t.Fatalf("ended late: as_of=%v run_ended_at=%v", s.AsOf, s.RunEndedAt)
			}
		}},
		// A tablet clock ahead of the server: the record lies at the run end. It is counted
		// (raw value kept) and flagged LIMITED, never silently dropped or silently counted.
		"record at the run end is counted and flagged": {func(f *summaryFixture) {
			f.qty(f.stationA, 180, 7, 0)
			endAt(f, 3*time.Hour)
		}, func(t *testing.T, f *summaryFixture, s ShiftSummaryV1, encoded []byte, digest string) {
			st := station(t, s, f.stationA)
			if !slices.Contains(st.Reasons, ReasonAfterRunEnd) || !slices.Contains(s.Reasons, ReasonAfterRunEnd) || st.Readiness == SummaryReady || s.Readiness == SummaryReady ||
				st.Recorded.GoodQuantity == nil || *st.Recorded.GoodQuantity != 47 || s.SourceRecordCount != 6 {
				t.Fatalf("after-end record: reasons %v readiness %s good %v count %d", st.Reasons, st.Readiness, st.Recorded.GoodQuantity, s.SourceRecordCount)
			}
		}},
	} {
		t.Run(name, func(t *testing.T) {
			f := base()
			tc.edit(f)
			s, encoded, digest, err := SummarizeShift(f.sc, f.in)
			if err != nil {
				t.Fatal(err)
			}
			tc.check(t, f, s, encoded, digest)
		})
	}
	// The run end, not the generation instant, fixes an ended run's summary: End and a later
	// regeneration over the same records agree.
	a, b := base(), base()
	endAt(a, 3*time.Hour)
	endAt(b, 3*time.Hour)
	b.in.AsOf = b.in.StartsAt.Add(20 * time.Hour)
	if _, _, da, _ := SummarizeShift(a.sc, a.in); da != func() string { _, _, d, _ := SummarizeShift(b.sc, b.in); return d }() {
		t.Fatal("the same run end with a later generation instant changed the digest")
	}
}
