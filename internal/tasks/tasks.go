// Package tasks owns the internal task an approved action becomes (WIS-277). The state machine
// here is pure; migration 066 enforces the same transition pairs in SQL, and Pairs exists so tests
// can compare the two. Only the deterministic executor creates a task, inside the transaction that
// consumes the approval: there is no route that creates one.
package tasks

import (
	"errors"
	"sort"
)

// State is one value of the task lifecycle.
type State string

const (
	Assigned     State = "assigned"
	Acknowledged State = "acknowledged"
	InProgress   State = "in_progress"
	Blocked      State = "blocked"
	Completed    State = "completed"
	Canceled     State = "canceled"
)

// States lists every state in lifecycle order.
func States() []State {
	return []State{Assigned, Acknowledged, InProgress, Blocked, Completed, Canceled}
}

// Event names a task transition.
type Event string

const (
	Acknowledge Event = "acknowledge" // assigned -> acknowledged (assignee)
	Start       Event = "start"       // acknowledged -> in_progress; blocked -> in_progress with a reason (assignee)
	Block       Event = "block"       // acknowledged|in_progress -> blocked (supervisor, reason)
	Complete    Event = "complete"    // in_progress -> completed (assignee, report evidence)
	Cancel      Event = "cancel"      // any active state -> canceled (supervisor, reason)
	Reopen      Event = "reopen"      // completed -> in_progress (supervisor, reason; correction)
)

// Role is who may perform an event: the task's assignee or a site supervisor. An owner or admin
// role alone is never enough.
type Role string

const (
	RoleAssignee   Role = "assignee"
	RoleSupervisor Role = "supervisor"
)

type rule struct {
	to          State
	role        Role
	needsReason bool
}

var (
	// ErrIllegalTransition: the state and event pair is not allowed (409).
	ErrIllegalTransition = errors.New("illegal task transition")
	// ErrNotAuthorized: the actor's role may not perform this event (403).
	ErrNotAuthorized = errors.New("task action not permitted for this role")
	// ErrReasonRequired: the event needs a reason (422).
	ErrReasonRequired = errors.New("task transition needs a reason")
)

var rules = map[State]map[Event]rule{
	Assigned:     {Acknowledge: {to: Acknowledged, role: RoleAssignee}, Cancel: {to: Canceled, role: RoleSupervisor, needsReason: true}},
	Acknowledged: {Start: {to: InProgress, role: RoleAssignee}, Block: {to: Blocked, role: RoleSupervisor, needsReason: true}, Cancel: {to: Canceled, role: RoleSupervisor, needsReason: true}},
	InProgress:   {Complete: {to: Completed, role: RoleAssignee}, Block: {to: Blocked, role: RoleSupervisor, needsReason: true}, Cancel: {to: Canceled, role: RoleSupervisor, needsReason: true}},
	Blocked:      {Start: {to: InProgress, role: RoleAssignee, needsReason: true}, Cancel: {to: Canceled, role: RoleSupervisor, needsReason: true}},
	Completed:    {Reopen: {to: InProgress, role: RoleSupervisor, needsReason: true}},
}

// Apply returns the next state for event by an actor holding role. The checks run in a fixed
// order: the pair must exist (409), the role must match (403), then a required reason (422).
func Apply(from State, event Event, role Role, reasonProvided bool) (State, error) {
	r, ok := rules[from][event]
	if !ok {
		return from, ErrIllegalTransition
	}
	if r.role != role {
		return from, ErrNotAuthorized
	}
	if r.needsReason && !reasonProvided {
		return from, ErrReasonRequired
	}
	return r.to, nil
}

// Terminal reports whether no further event is possible except a supervisor reopen.
func Terminal(s State) bool { return s == Completed || s == Canceled }

// Pairs lists every allowed (from, to) pair, sorted, for comparison with the SQL guard.
func Pairs() [][2]State {
	seen := map[[2]State]bool{}
	for from, events := range rules {
		for _, r := range events {
			seen[[2]State{from, r.to}] = true
		}
	}
	out := make([][2]State, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i][0] != out[j][0] {
			return out[i][0] < out[j][0]
		}
		return out[i][1] < out[j][1]
	})
	return out
}
