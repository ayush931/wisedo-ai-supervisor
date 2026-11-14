package knowledge

import (
	"strings"
	"testing"
	"time"
)

func TestWIS703IncompleteCorrectionIsDataNotKnowledge(t *testing.T) {
	in := MemoryRevisionInput{Reason: "The answer is wrong"}
	if err := in.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := in.Missing(); len(got) != 3 {
		t.Fatalf("missing requirements: %v", got)
	}
	in.ExpectedBaseRevision = 1
	if in.Validate() == nil {
		t.Fatal("base without claim accepted")
	}
	in.ExpectedBaseRevision = 0
	in.Reason = strings.Repeat("x", 2001)
	if in.Validate() == nil {
		t.Fatal("unbounded reason accepted")
	}
}

func TestWIS703CorrectionMonthUsesSiteCalendarAcrossDST(t *testing.T) {
	now := time.Date(2026, 11, 1, 2, 30, 0, 0, time.UTC)
	start, end, err := correctionMonth(now, "America/Detroit")
	if err != nil || start.Format(time.RFC3339) != "2026-10-01T04:00:00Z" || end.Format(time.RFC3339) != "2026-11-01T04:00:00Z" {
		t.Fatalf("local month %v %v %v", start, end, err)
	}
	start, end, err = correctionMonth(now.Add(24*time.Hour), "America/Detroit")
	if err != nil || start.Format(time.RFC3339) != "2026-11-01T04:00:00Z" || end.Format(time.RFC3339) != "2026-12-01T05:00:00Z" {
		t.Fatalf("DST month %v %v %v", start, end, err)
	}
	start, _, err = correctionMonth(now, "")
	if err != nil || start.Format(time.RFC3339) != "2026-11-01T00:00:00Z" {
		t.Fatalf("legacy UTC %v %v", start, err)
	}
}

func TestWIS703UnexpressibleRegressionRemainsIncomplete(t *testing.T) {
	in := MemoryRevisionInput{Reason: "Long expectation", Question: "What changed?", ExpectedAnswer: strings.Repeat("é", 1001)}
	for _, requirement := range in.Missing() {
		if requirement == "expected_answer_under_1001_characters" {
			return
		}
	}
	t.Fatal("expectation larger than the single-statement assertion was approvable")
}
