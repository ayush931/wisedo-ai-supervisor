package outcome

import (
	"strings"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/plantcontext"
)

func ptr(s string) *string { return &s }

var plan = &plantcontext.PlanRef{PlanRevisionID: "plan-1", PlanRevision: 1}

func baseline() plantcontext.Snapshot {
	return plantcontext.Snapshot{OrderID: "order", AssetID: "line", Unit: "count", ApprovedPlanRevision: plan}
}

// ready is a READY post window with exact expected and deficit values.
func ready(start, expected, deficit string) plantcontext.Snapshot {
	s := baseline()
	s.Window = plantcontext.Window{Start: start}
	s.Readiness = plantcontext.ReadyState
	s.ReasonCodes = []string{}
	s.ExpectedExact, s.DeficitExact = ptr(expected), ptr(deficit)
	return s
}

func notReady(start string, codes ...string) plantcontext.Snapshot {
	s := baseline()
	s.Window = plantcontext.Window{Start: start}
	s.Readiness = "NOT_READY"
	s.ReasonCodes = codes
	return s
}

func breakWindow(start string) plantcontext.Snapshot {
	s := notReady(start, "NO_PLANNED_PRODUCTION")
	s.Readiness = plantcontext.LimitedState
	return s
}

func TestDecide(t *testing.T) {
	completed := time.Date(2026, 9, 27, 0, 37, 0, 0, time.UTC)
	during := completed.Add(30 * time.Minute)                      // horizon still open
	after := completed.Add(Horizon + plantcontext.EvaluationDelay) // horizon passed, evaluation delay included
	// WIS-277 review of #191, M1: 30 s past the deadline the last window has not finished its
	// evaluation delay yet; it is still pending, not insufficient_data.
	grace := completed.Add(Horizon + 30*time.Second)
	other := func(edit func(*plantcontext.Snapshot)) plantcontext.Snapshot {
		s := ready("w2", "100", "0")
		edit(&s)
		return s
	}
	for _, c := range []struct {
		name     string
		windows  []plantcontext.Snapshot
		now      time.Time
		want     Status
		usedLast string // start of the last window the decision rests on
	}{
		{"two recovered windows", []plantcontext.Snapshot{ready("w1", "100", "4"), ready("w2", "100", "5")}, during, ObservedImprovement, "w2"},
		{"exactly 5% is recovered", []plantcontext.Snapshot{ready("w1", "100", "5"), ready("w2", "3", "0.15")}, during, ObservedImprovement, "w2"},
		{"one window above 5%", []plantcontext.Snapshot{ready("w1", "100", "5.000001"), ready("w2", "100", "0")}, during, NoImprovement, "w2"},
		{"third window not needed", []plantcontext.Snapshot{ready("w1", "100", "20"), ready("w2", "100", "20"), ready("w3", "100", "0")}, after, NoImprovement, "w2"},
		{"break window is skipped, not counted", []plantcontext.Snapshot{ready("w1", "100", "0"), breakWindow("w2"), ready("w3", "100", "1")}, during, ObservedImprovement, "w3"},
		{"only one window so far", []plantcontext.Snapshot{ready("w1", "100", "0")}, during, Pending, ""},
		{"not ready may still arrive", []plantcontext.Snapshot{notReady("w1", "INCOMPLETE_SEQUENCE"), ready("w2", "100", "0"), ready("w3", "100", "0")}, during, Pending, ""},
		{"not ready after the horizon is skipped", []plantcontext.Snapshot{notReady("w1", "INCOMPLETE_SEQUENCE"), ready("w2", "100", "0"), ready("w3", "100", "0")}, after, ObservedImprovement, "w3"},
		{"no trustworthy windows by the horizon", []plantcontext.Snapshot{ready("w1", "100", "0"), notReady("w2", "NO_PLAN"), notReady("w3", "NO_PLAN")}, after, InsufficientData, "w3"},
		{"nothing at all by the horizon", nil, after, InsufficientData, ""},
		{"last window still in its evaluation delay at the deadline", []plantcontext.Snapshot{ready("w1", "100", "0"), notReady("w2", "WINDOW_NOT_ELAPSED")}, grace, Pending, ""},
		// Review of #191, M2: nothing planned is not recovery, and a READY window without its exact
		// values is not a measured failure.
		{"zero expected is skipped, not recovered", []plantcontext.Snapshot{ready("w1", "0", "0"), ready("w2", "100", "0")}, after, InsufficientData, "w2"},
		{"zero expected between two counted windows", []plantcontext.Snapshot{ready("w1", "100", "0"), ready("w2", "0", "0"), ready("w3", "100", "1")}, during, ObservedImprovement, "w3"},
		{"ready without values is untrustworthy", []plantcontext.Snapshot{other(func(s *plantcontext.Snapshot) { s.DeficitExact = nil }), ready("w3", "100", "0")}, after, InsufficientData, "w3"},
		{"changed plan revision", []plantcontext.Snapshot{ready("w1", "100", "0"), other(func(s *plantcontext.Snapshot) {
			s.ApprovedPlanRevision = &plantcontext.PlanRef{PlanRevisionID: "plan-2"}
		})}, during, NotComparable, "w2"},
		{"changed unit", []plantcontext.Snapshot{other(func(s *plantcontext.Snapshot) { s.Unit = "kg" })}, during, NotComparable, "w2"},
		{"changed order", []plantcontext.Snapshot{other(func(s *plantcontext.Snapshot) { s.OrderID = "order-2" })}, during, NotComparable, "w2"},
		{"missing plan", []plantcontext.Snapshot{other(func(s *plantcontext.Snapshot) { s.ApprovedPlanRevision = nil })}, during, NotComparable, "w2"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := Decide(baseline(), c.windows, completed, c.now)
			if got.Status != c.want {
				t.Fatalf("status %s, want %s", got.Status, c.want)
			}
			if c.want == Pending {
				if len(got.Windows) != 0 || got.Interpretation != "" {
					t.Fatalf("a pending result records nothing: %+v", got)
				}
				return
			}
			if got.Interpretation == "" {
				t.Fatal("a decided outcome explains itself")
			}
			last := ""
			if n := len(got.Windows); n > 0 {
				last = got.Windows[n-1].Window.Start
			}
			if last != c.usedLast {
				t.Fatalf("decision rests on windows up to %q, want %q", last, c.usedLast)
			}
		})
	}
}

// TestImprovementIsLabelledThresholdRecovery: an improvement is reported as threshold recovery,
// never as the task's effect (rule 06).
func TestImprovementIsLabelledThresholdRecovery(t *testing.T) {
	got := Decide(baseline(), []plantcontext.Snapshot{ready("w1", "100", "0"), ready("w2", "100", "0")}, time.Unix(0, 0), time.Unix(60, 0))
	if got.Status != ObservedImprovement || !containsAll(got.Interpretation, "Threshold recovery", "not proof") {
		t.Fatalf("interpretation %q", got.Interpretation)
	}
}

func containsAll(s string, parts ...string) bool {
	for _, p := range parts {
		if !strings.Contains(s, p) {
			return false
		}
	}
	return true
}

func TestCandidates(t *testing.T) {
	for _, c := range []struct {
		name      string
		completed time.Time
		first     string
		count     int
	}{
		// The partial window containing completion is skipped.
		{"mid window", time.Date(2026, 9, 27, 0, 37, 30, 0, time.UTC), "2026-09-27T00:45:00Z", 7},
		// Completion exactly on a boundary: the window starting there is complete afterwards.
		{"on a boundary", time.Date(2026, 9, 27, 0, 45, 0, 0, time.UTC), "2026-09-27T00:45:00Z", 8},
		{"non-UTC input", time.Date(2026, 9, 27, 6, 7, 30, 0, time.FixedZone("IST", 5*3600+1800)), "2026-09-27T00:45:00Z", 7},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := Candidates(c.completed)
			if len(got) != c.count || got[0].Start != c.first {
				t.Fatalf("%d windows from %v, want %d from %s", len(got), got, c.count, c.first)
			}
			deadline := c.completed.UTC().Add(Horizon)
			for i, w := range got {
				s, _ := time.Parse(time.RFC3339, w.Start)
				e, _ := time.Parse(time.RFC3339, w.End)
				if e.Sub(s) != Window || e.After(deadline) || (i > 0 && w.Start != got[i-1].End) {
					t.Fatalf("window %d %+v breaks the 15-minute, gap-free, within-horizon rule", i, w)
				}
			}
		})
	}
}
