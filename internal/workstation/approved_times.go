package workstation

import (
	"encoding/json"
	"math/big"
	"sort"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/knowledge"
)

// Approved cycle-time comparison statuses (WIS-628).
const (
	CycleTimeMatches          = "matches"
	CycleTimeDiffers          = "differs"
	CycleTimeMissingOnOrder   = "missing_on_job_order"
	CycleTimeApprovedConflict = "approved_conflict"
)

// ApprovedCycleTimeV1 compares one selected job order's cycle time with the cycle_time_seconds
// claim approved for its operation during onboarding (WIS-611/612), as captured in the pinned
// process snapshot. It is advice for the supervisor: the planner still plans only from the job
// order's own value, and a job order changes only through a human revision (rule 05).
type ApprovedCycleTimeV1 struct {
	JobOrderID      string   `json:"job_order_id" contract:"uuid"`
	OperationID     string   `json:"operation_id" contract:"uuid"`
	Status          string   `json:"status"`
	ApprovedSeconds string   `json:"approved_seconds,omitempty"`
	JobOrderSeconds string   `json:"job_order_seconds,omitempty"`
	ClaimIDs        []string `json:"claim_ids"`
	SourceSpanIDs   []string `json:"source_span_ids"`
}

// approvedCycleTimes returns one comparison for each selected job order whose operation has an
// approved cycle_time_seconds claim effective during the shift [start, end). Values are compared
// exactly (rule 04). Approved claims that disagree give approved_conflict with no value, never a
// pick. It also returns APPROVED_CYCLE_TIME_DIFFERS when any job order differs or lacks the value.
func approvedCycleTimes(jobs []JobOrderSummaryV1, snapshot knowledge.Snapshot, start, end time.Time) ([]ApprovedCycleTimeV1, []string) {
	out := []ApprovedCycleTimeV1{}
	differs := false
	for _, j := range jobs {
		if j.CurrentOrder == nil || j.CurrentOrder.OperationID == "" {
			continue
		}
		var claims []knowledge.SnapshotClaim
		for _, c := range snapshot.Contents.Claims {
			if c.Predicate == "cycle_time_seconds" && c.SubjectID == j.CurrentOrder.OperationID && c.Literal != nil && c.Literal.Type == "decimal" && claimOverlaps(c, start, end) {
				claims = append(claims, c)
			}
		}
		if len(claims) == 0 {
			continue
		}
		row := ApprovedCycleTimeV1{JobOrderID: j.ID, OperationID: j.CurrentOrder.OperationID, JobOrderSeconds: j.CurrentOrder.TargetCycleTimeSeconds, ClaimIDs: []string{}, SourceSpanIDs: []string{}}
		var approved *big.Rat
		conflict := false
		for _, c := range claims {
			var text string
			value, ok := new(big.Rat), json.Unmarshal(c.Literal.Value, &text) == nil
			if ok {
				_, ok = value.SetString(text)
			}
			if !ok || approved != nil && approved.Cmp(value) != 0 {
				conflict = true
			}
			if ok && approved == nil {
				approved, row.ApprovedSeconds = value, text
			}
			row.ClaimIDs = append(row.ClaimIDs, c.ClaimID)
			row.SourceSpanIDs = append(row.SourceSpanIDs, c.SourceSpanIDs...)
		}
		sort.Strings(row.ClaimIDs)
		sort.Strings(row.SourceSpanIDs)
		row.SourceSpanIDs = compactSorted(row.SourceSpanIDs)
		switch {
		case conflict:
			row.Status, row.ApprovedSeconds = CycleTimeApprovedConflict, ""
		case row.JobOrderSeconds == "":
			row.Status, differs = CycleTimeMissingOnOrder, true
		default:
			current, ok := new(big.Rat).SetString(row.JobOrderSeconds)
			if ok && current.Cmp(approved) == 0 {
				row.Status = CycleTimeMatches
			} else {
				row.Status, differs = CycleTimeDiffers, true
			}
		}
		out = append(out, row)
	}
	if differs {
		return out, []string{"APPROVED_CYCLE_TIME_DIFFERS"}
	}
	return out, nil
}

func compactSorted(s []string) []string {
	out := s[:0]
	for i, v := range s {
		if i == 0 || v != s[i-1] {
			out = append(out, v)
		}
	}
	return out
}
