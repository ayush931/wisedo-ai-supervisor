package cases

import (
	"errors"
	"reflect"
	"testing"
)

// expected is the WIS-273 transition table written out independently of the implementation:
// for each legal (state, event) pair, the target state and the guards that must all hold.
type expectation struct {
	to    State
	needs []string // Guards field names; all must be true
}

func expectedTable() map[State]map[Event]expectation {
	active := []State{Detected, Investigating, NeedsInput, Proposed, AwaitingApproval, Executing, Observing}
	t := map[State]map[Event]expectation{}
	add := func(from State, e Event, x expectation) {
		if t[from] == nil {
			t[from] = map[Event]expectation{}
		}
		t[from][e] = x
	}
	add(Detected, ContextReady, expectation{Investigating, []string{"ContextReady", "ScopeEnabled"}})
	add(Detected, EvidenceMissing, expectation{NeedsInput, []string{"ReasonProvided"}})
	add(Investigating, EvidenceMissing, expectation{NeedsInput, []string{"ReasonProvided"}})
	add(NeedsInput, Clarified, expectation{Investigating, []string{"ClarificationAuthorized", "EvidenceComplete"}})
	add(Investigating, ProposalValidated, expectation{Proposed, []string{"ProposalValid"}})
	add(Proposed, ApprovalRequested, expectation{AwaitingApproval, []string{"ApprovalRequestExists"}})
	add(AwaitingApproval, ProposalRejected, expectation{NeedsInput, []string{"ReasonProvided"}})
	add(AwaitingApproval, ApprovalGranted, expectation{Executing, []string{"ApprovalEligible", "RuntimeEnabled"}})
	add(Executing, TaskAuthoritative, expectation{Observing, []string{"TaskExists"}})
	for _, from := range []State{Observing, Escalated} {
		add(from, ClosedBySupervisor, expectation{Resolved, []string{"TaskTerminal", "OutcomeRecorded", "SupervisorActor", "ReasonProvided"}})
	}
	for _, from := range active {
		add(from, Fatal, expectation{Escalated, []string{"ReasonProvided"}})
		add(from, WaitDeadline, expectation{Escalated, nil})
		cancel := []string{"CancelAuthorized"}
		if from == Executing || from == Observing {
			cancel = append(cancel, "CommittedTaskCanceled")
		}
		add(from, Cancel, expectation{Canceled, cancel})
	}
	add(Escalated, Cancel, expectation{Canceled, []string{"CancelAuthorized"}})
	add(Escalated, Resume, expectation{Investigating, []string{"NewContextProvided", "SupervisorActor", "ReasonProvided"}})
	add(Resolved, Reopen, expectation{Detected, []string{"ReasonProvided", "SupervisorActor"}})
	add(Canceled, Reopen, expectation{Detected, []string{"ReasonProvided", "SupervisorActor"}})
	return t
}

func guardsWith(names ...string) Guards {
	var g Guards
	v := reflect.ValueOf(&g).Elem()
	for _, n := range names {
		f := v.FieldByName(n)
		if !f.IsValid() {
			panic("unknown guard " + n)
		}
		f.SetBool(true)
	}
	return g
}

// Every state x event pair: legal pairs move with exactly their guards, every other pair is
// illegal even with all guards true.
func TestTransitionMatrix(t *testing.T) {
	want := expectedTable()
	allTrue := Guards{}
	v := reflect.ValueOf(&allTrue).Elem()
	for i := 0; i < v.NumField(); i++ {
		v.Field(i).SetBool(true)
	}
	allTrue.ApprovedPayloadExists = false // the only guard that must be false to allow a move
	pairs := 0
	for _, from := range States() {
		for _, event := range Events() {
			pairs++
			x, legal := want[from][event]
			if !legal {
				if got, err := Apply(from, event, allTrue); !errors.Is(err, ErrIllegalTransition) || got != from {
					t.Errorf("%s --%s-- must be illegal, got %s err=%v", from, event, got, err)
				}
				continue
			}
			if got, err := Apply(from, event, guardsWith(x.needs...)); err != nil || got != x.to {
				t.Errorf("%s --%s--> want %s with %v, got %s err=%v", from, event, x.to, x.needs, got, err)
			}
			// Each required guard is necessary on its own.
			for i, missing := range x.needs {
				rest := append(append([]string(nil), x.needs[:i]...), x.needs[i+1:]...)
				if got, err := Apply(from, event, guardsWith(rest...)); !errors.Is(err, ErrGuardFailed) || got != from {
					t.Errorf("%s --%s-- moved without %s: got %s err=%v", from, event, missing, got, err)
				}
			}
		}
	}
	if pairs != len(States())*14 {
		t.Fatalf("checked %d pairs, want %d", pairs, len(States())*14)
	}
}

// A new context is the alternative to an authorized clarification; evidence must still be complete.
func TestClarifiedByNewContext(t *testing.T) {
	if got, err := Apply(NeedsInput, Clarified, guardsWith("NewContextProvided", "EvidenceComplete")); err != nil || got != Investigating {
		t.Fatalf("new context: %s %v", got, err)
	}
	if _, err := Apply(NeedsInput, Clarified, guardsWith("NewContextProvided")); !errors.Is(err, ErrGuardFailed) {
		t.Fatalf("new context without complete evidence: %v", err)
	}
}

func TestResumeNeverRegeneratesAnApprovedPayload(t *testing.T) {
	g := guardsWith("NewContextProvided", "SupervisorActor", "ReasonProvided", "ApprovedPayloadExists")
	if got, err := Apply(Escalated, Resume, g); !errors.Is(err, ErrGuardFailed) || got != Escalated {
		t.Fatalf("resume with an approved payload: %s %v", got, err)
	}
}

// Only Resolved and Canceled are terminal; Escalated stays active for deduplication.
func TestTerminalAndActive(t *testing.T) {
	for _, s := range States() {
		terminal := s == Resolved || s == Canceled
		if Terminal(s) != terminal || Active(s) == terminal {
			t.Errorf("%s: terminal=%v active=%v", s, Terminal(s), Active(s))
		}
	}
}

// Pairs is the list migration 027 encodes in its CHECK; the integration test compares them.
func TestPairsMatchTheTable(t *testing.T) {
	want := map[Pair]bool{}
	for from, events := range expectedTable() {
		for event, x := range events {
			if event != Reopen {
				want[Pair{from, x.to}] = true
			}
		}
	}
	got := Pairs()
	if len(got) != len(want) {
		t.Fatalf("Pairs has %d entries, want %d: %v", len(got), len(want), got)
	}
	for _, p := range got {
		if !want[p] {
			t.Errorf("unexpected pair %v", p)
		}
		if p.From == p.To {
			t.Errorf("self-loop %v", p)
		}
	}
}
