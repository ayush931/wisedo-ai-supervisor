package workstation

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/knowledge"
)

// WIS-628: a shift suggestion compares each selected job order's cycle time with the value
// approved for its operation during onboarding, exactly and only within the shift.
func TestApprovedCycleTimes(t *testing.T) {
	const op, other = "00000000-0000-4000-8000-000000000001", "00000000-0000-4000-8000-000000000002"
	start, end := time.Date(2026, 11, 2, 6, 0, 0, 0, time.UTC), time.Date(2026, 11, 2, 14, 0, 0, 0, time.UTC)
	claim := func(id, subject, predicate, value, from string, to *string) knowledge.SnapshotClaim {
		v, _ := json.Marshal(value)
		return knowledge.SnapshotClaim{ClaimInput: knowledge.ClaimInput{ClaimID: id, SubjectID: subject, Predicate: predicate, Literal: &knowledge.Literal{Type: "decimal", Value: v, Unit: "s"},
			EffectiveFrom: from, EffectiveTo: to, SourceSpanIDs: []string{"span-" + id}}}
	}
	ended := "2026-11-01T00:00:00Z"
	job := func(seconds string) JobOrderSummaryV1 {
		return JobOrderSummaryV1{ID: "job-1", CurrentRevision: 1, CurrentOrder: &JobOrderBodyV1{OperationID: op, TargetCycleTimeSeconds: seconds}}
	}
	for _, tc := range []struct {
		name     string
		order    string
		claims   []knowledge.SnapshotClaim
		status   string
		approved string
		warned   bool
	}{
		{"equal value written differently", "60.0", []knowledge.SnapshotClaim{claim("c1", op, "cycle_time_seconds", "60", "2026-10-01T00:00:00Z", nil)}, CycleTimeMatches, "60", false},
		{"job order differs", "45", []knowledge.SnapshotClaim{claim("c1", op, "cycle_time_seconds", "60", "2026-10-01T00:00:00Z", nil)}, CycleTimeDiffers, "60", true},
		{"job order has none", "", []knowledge.SnapshotClaim{claim("c1", op, "cycle_time_seconds", "60.5", "2026-10-01T00:00:00Z", nil)}, CycleTimeMissingOnOrder, "60.5", true},
		{"approved values disagree", "60", []knowledge.SnapshotClaim{claim("c1", op, "cycle_time_seconds", "60", "2026-10-01T00:00:00Z", nil), claim("c2", op, "cycle_time_seconds", "70", "2026-10-01T00:00:00Z", nil)}, CycleTimeApprovedConflict, "", false},
		{"claim ended before the shift", "45", []knowledge.SnapshotClaim{claim("c1", op, "cycle_time_seconds", "60", "2026-10-01T00:00:00Z", &ended)}, "", "", false},
		{"claim starts after the shift", "45", []knowledge.SnapshotClaim{claim("c1", op, "cycle_time_seconds", "60", "2026-11-02T14:00:00Z", nil)}, "", "", false},
		{"another operation", "45", []knowledge.SnapshotClaim{claim("c1", other, "cycle_time_seconds", "60", "2026-10-01T00:00:00Z", nil)}, "", "", false},
		{"setup time is not cycle time", "45", []knowledge.SnapshotClaim{claim("c1", op, "setup_time_seconds", "60", "2026-10-01T00:00:00Z", nil)}, "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := knowledge.Snapshot{Contents: knowledge.SnapshotContents{Claims: tc.claims}}
			rows, warnings := approvedCycleTimes([]JobOrderSummaryV1{job(tc.order)}, snapshot, start, end)
			if slices.Contains(warnings, "APPROVED_CYCLE_TIME_DIFFERS") != tc.warned {
				t.Fatalf("warnings %v, want differs=%v", warnings, tc.warned)
			}
			if tc.status == "" {
				if len(rows) != 0 {
					t.Fatalf("unexpected comparison %+v", rows)
				}
				return
			}
			if len(rows) != 1 || rows[0].Status != tc.status || rows[0].ApprovedSeconds != tc.approved || rows[0].JobOrderSeconds != tc.order || rows[0].JobOrderID != "job-1" || rows[0].OperationID != op {
				t.Fatalf("got %+v, want status %s approved %q", rows, tc.status, tc.approved)
			}
			if len(rows[0].ClaimIDs) != len(tc.claims) || len(rows[0].SourceSpanIDs) != len(tc.claims) {
				t.Fatalf("citations %v %v", rows[0].ClaimIDs, rows[0].SourceSpanIDs)
			}
		})
	}
}
