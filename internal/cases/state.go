// Package cases owns the exception case lifecycle (WIS-273). The state machine here is pure:
// it decides which transitions are legal and which guards must hold. Persistence, outbox
// delivery and Temporal scheduling live in separate layers that call Apply. Migration 027
// enforces the same transition pairs in SQL; Pairs exists so tests can compare the two.
package cases

import (
	"errors"
	"sort"
)

// State is one value of the CaseV1 lifecycle.
type State string

const (
	Detected         State = "detected"
	Investigating    State = "investigating"
	NeedsInput       State = "needs_input"
	Proposed         State = "proposed"
	AwaitingApproval State = "awaiting_approval"
	Executing        State = "executing"
	Observing        State = "observing"
	Resolved         State = "resolved"
	Escalated        State = "escalated"
	Canceled         State = "canceled"
)

// States lists every state in table order.
func States() []State {
	return []State{Detected, Investigating, NeedsInput, Proposed, AwaitingApproval, Executing, Observing, Resolved, Escalated, Canceled}
}

// Event names a transition trigger from the specification's table.
type Event string

const (
	ContextReady       Event = "context_ready"        // detected -> investigating
	EvidenceMissing    Event = "evidence_missing"     // detected|investigating -> needs_input
	Clarified          Event = "clarified"            // needs_input -> investigating
	ProposalValidated  Event = "proposal_validated"   // investigating -> proposed
	ApprovalRequested  Event = "approval_requested"   // proposed -> awaiting_approval
	ProposalRejected   Event = "proposal_rejected"    // awaiting_approval -> needs_input (rejected, expired or stale)
	ApprovalGranted    Event = "approval_granted"     // awaiting_approval -> executing
	TaskAuthoritative  Event = "task_authoritative"   // executing -> observing
	ClosedBySupervisor Event = "closed_by_supervisor" // observing|escalated -> resolved
	Fatal              Event = "fatal"                // any active, non-escalated -> escalated
	WaitDeadline       Event = "wait_deadline"        // any active, non-escalated -> escalated
	Cancel             Event = "cancel"               // any nonterminal -> canceled
	Resume             Event = "resume"               // escalated -> investigating (new context)
	Reopen             Event = "reopen"               // resolved|canceled -> detected in a new episode
)

// Guards are the facts a transition requires, established by the caller from durable state.
// A transition whose guards are not all true is refused: the case does not move.
type Guards struct {
	ContextReady            bool // an immutable READY context exists
	ScopeEnabled            bool // the site/line scope is enabled for investigation
	ReasonProvided          bool // a reason code or text explains the transition
	ClarificationAuthorized bool // an authorized person supplied the clarification
	NewContextProvided      bool // a new context or version is bound
	EvidenceComplete        bool // required evidence present and not contradictory
	ProposalValid           bool // proposal validated and persisted
	ApprovalRequestExists   bool // an immutable approval request exists
	ApprovalEligible        bool // approval exists, unexpired, current
	RuntimeEnabled          bool // execution is enabled for this scope
	TaskExists              bool // an authoritative internal task exists
	TaskTerminal            bool // the internal task reached a terminal state
	OutcomeRecorded         bool // a measured outcome exists (including insufficient_data)
	SupervisorActor         bool // the actor is an authorized human supervisor, not the system
	CancelAuthorized        bool // cancellation by an authorized actor
	CommittedTaskCanceled   bool // a committed task was explicitly canceled first
	ApprovedPayloadExists   bool // this episode already has an approved or consumed payload
}

// ErrIllegalTransition means the state and event pair is not in the table.
var ErrIllegalTransition = errors.New("illegal case transition")

// ErrGuardFailed means the transition exists but a required guard is false.
var ErrGuardFailed = errors.New("case transition guard failed")

type rule struct {
	from  []State
	to    State
	needs func(from State, g Guards) bool
}

// active excludes escalated so that fatal and deadline events never self-loop.
var activeNotEscalated = []State{Detected, Investigating, NeedsInput, Proposed, AwaitingApproval, Executing, Observing}
var nonterminal = append(append([]State(nil), activeNotEscalated...), Escalated)

var table = map[Event]rule{
	ContextReady: {[]State{Detected}, Investigating, func(_ State, g Guards) bool { return g.ContextReady && g.ScopeEnabled }},
	EvidenceMissing: {[]State{Detected, Investigating}, NeedsInput, func(_ State, g Guards) bool {
		return g.ReasonProvided
	}},
	Clarified: {[]State{NeedsInput}, Investigating, func(_ State, g Guards) bool {
		return (g.ClarificationAuthorized || g.NewContextProvided) && g.EvidenceComplete
	}},
	ProposalValidated: {[]State{Investigating}, Proposed, func(_ State, g Guards) bool { return g.ProposalValid }},
	ApprovalRequested: {[]State{Proposed}, AwaitingApproval, func(_ State, g Guards) bool { return g.ApprovalRequestExists }},
	ProposalRejected:  {[]State{AwaitingApproval}, NeedsInput, func(_ State, g Guards) bool { return g.ReasonProvided }},
	ApprovalGranted: {[]State{AwaitingApproval}, Executing, func(_ State, g Guards) bool {
		return g.ApprovalEligible && g.RuntimeEnabled
	}},
	TaskAuthoritative: {[]State{Executing}, Observing, func(_ State, g Guards) bool { return g.TaskExists }},
	// Task completion is not resolution: a supervisor closes with a recorded outcome. An
	// escalated case is closed the same way, so missing post-window evidence cannot trap it.
	ClosedBySupervisor: {[]State{Observing, Escalated}, Resolved, func(_ State, g Guards) bool {
		return g.TaskTerminal && g.OutcomeRecorded && g.SupervisorActor && g.ReasonProvided
	}},
	Fatal:        {activeNotEscalated, Escalated, func(_ State, g Guards) bool { return g.ReasonProvided }},
	WaitDeadline: {activeNotEscalated, Escalated, func(State, Guards) bool { return true }},
	// Already committed tasks need explicit cancellation, not a fictitious rollback.
	Cancel: {nonterminal, Canceled, func(from State, g Guards) bool {
		if from == Executing || from == Observing {
			return g.CancelAuthorized && g.CommittedTaskCanceled
		}
		return g.CancelAuthorized
	}},
	// Resume never regenerates an already approved payload: such a case is closed or canceled.
	Resume: {[]State{Escalated}, Investigating, func(_ State, g Guards) bool {
		return g.NewContextProvided && g.SupervisorActor && g.ReasonProvided && !g.ApprovedPayloadExists
	}},
	Reopen: {[]State{Resolved, Canceled}, Detected, func(_ State, g Guards) bool { return g.ReasonProvided && g.SupervisorActor }},
}

// Apply returns the next state for an event, or an error that leaves the case where it was.
// Reopen returns Detected; the caller must create a new episode and keep the old history.
func Apply(from State, event Event, g Guards) (State, error) {
	r, ok := table[event]
	if !ok || !contains(r.from, from) {
		return from, ErrIllegalTransition
	}
	if !r.needs(from, g) {
		return from, ErrGuardFailed
	}
	return r.to, nil
}

// Terminal reports whether a state ends an episode. Escalated is not terminal: it stays
// active for deduplication until someone cancels, resolves or resumes it.
func Terminal(s State) bool { return s == Resolved || s == Canceled }

// Active reports whether a state blocks a new case for the same key (the partial unique
// index covers exactly these states).
func Active(s State) bool { return !Terminal(s) }

// Events returns every event name in stable order, for documentation and tests.
func Events() []Event {
	out := make([]Event, 0, len(table))
	for e := range table {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Pair is one legal from/to state change within an episode. Reopen is excluded: it starts a
// new case row rather than changing this one.
type Pair struct{ From, To State }

// Pairs returns every from/to change the table allows, in stable order.
func Pairs() []Pair {
	seen := map[Pair]bool{}
	for event, r := range table {
		if event == Reopen {
			continue
		}
		for _, from := range r.from {
			seen[Pair{from, r.to}] = true
		}
	}
	out := make([]Pair, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].From != out[j].From {
			return out[i].From < out[j].From
		}
		return out[i].To < out[j].To
	})
	return out
}

func contains(states []State, want State) bool {
	for _, s := range states {
		if s == want {
			return true
		}
	}
	return false
}
