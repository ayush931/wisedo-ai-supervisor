// Package outcome decides OutcomeObservationV1 (WIS-277): whether production met the plan again in
// the windows after a reported task completion. It is pure: callers pass the case's bound context,
// the post-completion windows already assessed, the completion time and the server time. It never
// reads a clock or a database (rule 04). Its result is threshold recovery, never causation (rule
// 06): a task completing and production recovering at the same time does not show that the task
// caused the recovery.
package outcome

import (
	"fmt"
	"math/big"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/plantcontext"
)

// RuleVersion versions the observation rule; it is stored with every observation.
const RuleVersion = "wis277-outcome-1"

// Status is the observation result.
type Status string

const (
	Pending             Status = "pending"
	ObservedImprovement Status = "observed_improvement"
	NoImprovement       Status = "no_improvement"
	InsufficientData    Status = "insufficient_data"
	NotComparable       Status = "not_comparable"
)

const (
	// Window is the epoch-aligned production window length (WIS-272).
	Window = 15 * time.Minute
	// Horizon bounds how long after completion trustworthy windows may arrive.
	Horizon = 2 * time.Hour
	// Needed is how many complete productive post windows are compared.
	Needed = 2
)

// recoveredShare: a window counts as recovered when its deficit is at most 5% of expected.
var recoveredShare = big.NewRat(5, 100)

// Candidates returns the post-completion windows in order: epoch-aligned 15-minute windows that
// start at or after completion (the partial window containing it is skipped) and end within the
// horizon.
func Candidates(completedAt time.Time) []plantcontext.Window {
	completedAt = completedAt.UTC()
	start := completedAt.Truncate(Window)
	if start.Before(completedAt) {
		start = start.Add(Window)
	}
	deadline := completedAt.Add(Horizon)
	var out []plantcontext.Window
	for s := start; !s.Add(Window).After(deadline); s = s.Add(Window) {
		out = append(out, plantcontext.Window{Start: s.Format(time.RFC3339), End: s.Add(Window).Format(time.RFC3339)})
	}
	return out
}

// Result is a decided (or pending) observation. Windows are the assessed post windows that the
// decision rests on, in order, including skipped non-productive ones; values are kept exactly.
type Result struct {
	Status         Status
	Windows        []plantcontext.Snapshot
	Interpretation string
}

// Decide applies the rule to the post windows assessed so far (in Candidates order; a window not
// yet ended is not passed). baseline is the case's bound context: the post windows must share its
// order, asset, unit and approved plan revision, else the outcome is not comparable.
//
//   - A window with no planned production is not productive and is skipped.
//   - A READY window counts; after Needed of them the outcome is decided: both with deficit at most
//     5% of expected is observed_improvement, otherwise no_improvement.
//   - A READY window with zero expected output had nothing planned to recover: it is skipped as not
//     productive, never counted as recovered (rule 04: no undecided zero denominator).
//   - A window that is not READY, or READY without its exact values, may still become ready while
//     the horizon is open, so the result is pending; once the horizon has passed it is skipped as
//     untrustworthy. The horizon stays open for plantcontext.EvaluationDelay past the deadline, so
//     the last window that ends before the deadline can still be evaluated on its data.
//   - When the horizon has passed without Needed productive READY windows: insufficient_data.
func Decide(baseline plantcontext.Snapshot, assessed []plantcontext.Snapshot, completedAt, now time.Time) Result {
	deadline := completedAt.UTC().Add(Horizon)
	open := now.Before(deadline.Add(plantcontext.EvaluationDelay))
	var counted []plantcontext.Snapshot
	var used []plantcontext.Snapshot
	for _, w := range assessed {
		used = append(used, w)
		switch {
		case w.Readiness == plantcontext.LimitedState && onlyNoPlannedProduction(w.ReasonCodes):
			continue
		case w.Readiness != plantcontext.ReadyState || !exact(w):
			if open {
				return Result{Status: Pending}
			}
			continue
		case zeroExpected(w):
			continue
		case !sameBasis(baseline, w):
			return Result{Status: NotComparable, Windows: used,
				Interpretation: fmt.Sprintf("The window %s to %s ran under a different order, asset, unit or approved plan than the case, so it is not compared.", w.Window.Start, w.Window.End)}
		}
		counted = append(counted, w)
		if len(counted) == Needed {
			return decided(counted, used)
		}
	}
	if open {
		return Result{Status: Pending}
	}
	return Result{Status: InsufficientData, Windows: used,
		Interpretation: fmt.Sprintf("Fewer than %d complete productive windows with trustworthy data were available within %s of completion; no recovery is inferred.", Needed, Horizon)}
}

func decided(counted, used []plantcontext.Snapshot) Result {
	recovered := true
	for _, w := range counted {
		expected, okE := rat(w.ExpectedExact)
		deficit, okD := rat(w.DeficitExact)
		// deficit <= 5% of expected, compared exactly (no rounding).
		if !okE || !okD || deficit.Cmp(new(big.Rat).Mul(expected, recoveredShare)) > 0 {
			recovered = false
		}
	}
	if recovered {
		return Result{Status: ObservedImprovement, Windows: used,
			Interpretation: "Threshold recovery: in both windows after completion the deficit was at most 5% of expected. This is an observation, not proof that the task caused it."}
	}
	return Result{Status: NoImprovement, Windows: used,
		Interpretation: "In at least one of the two windows after completion the deficit was above 5% of expected."}
}

func sameBasis(a, b plantcontext.Snapshot) bool {
	if a.OrderID != b.OrderID || a.AssetID != b.AssetID || a.Unit != b.Unit || a.ApprovedPlanRevision == nil || b.ApprovedPlanRevision == nil {
		return false
	}
	return a.ApprovedPlanRevision.PlanRevisionID == b.ApprovedPlanRevision.PlanRevisionID
}

func onlyNoPlannedProduction(codes []string) bool {
	return len(codes) == 1 && codes[0] == "NO_PLANNED_PRODUCTION"
}

// exact reports whether a window carries its exact expected and deficit values.
func exact(w plantcontext.Snapshot) bool {
	_, okE := rat(w.ExpectedExact)
	_, okD := rat(w.DeficitExact)
	return okE && okD
}

func zeroExpected(w plantcontext.Snapshot) bool {
	e, ok := rat(w.ExpectedExact)
	return ok && e.Sign() == 0
}

func rat(v *string) (*big.Rat, bool) {
	if v == nil {
		return nil, false
	}
	return new(big.Rat).SetString(*v)
}
