package tasks

import (
	"errors"
	"os"
	"regexp"
	"sort"
	"testing"
)

// TestTransitionMatrix checks every state x event x role, so a widened rule fails here.
func TestTransitionMatrix(t *testing.T) {
	type want struct {
		to     State
		role   Role
		reason bool
	}
	allowed := map[State]map[Event]want{
		Assigned:     {Acknowledge: {Acknowledged, RoleAssignee, false}, Cancel: {Canceled, RoleSupervisor, true}},
		Acknowledged: {Start: {InProgress, RoleAssignee, false}, Block: {Blocked, RoleSupervisor, true}, Cancel: {Canceled, RoleSupervisor, true}},
		InProgress:   {Complete: {Completed, RoleAssignee, false}, Block: {Blocked, RoleSupervisor, true}, Cancel: {Canceled, RoleSupervisor, true}},
		Blocked:      {Start: {InProgress, RoleAssignee, true}, Cancel: {Canceled, RoleSupervisor, true}},
		Completed:    {Reopen: {InProgress, RoleSupervisor, true}},
	}
	events := []Event{Acknowledge, Start, Block, Complete, Cancel, Reopen}
	for _, from := range States() {
		for _, ev := range events {
			for _, role := range []Role{RoleAssignee, RoleSupervisor, Role("owner")} {
				for _, reason := range []bool{false, true} {
					got, err := Apply(from, ev, role, reason)
					w, ok := allowed[from][ev]
					switch {
					case !ok:
						if !errors.Is(err, ErrIllegalTransition) || got != from {
							t.Errorf("%s --%s--> must be illegal, got %s %v", from, ev, got, err)
						}
					case role != w.role:
						if !errors.Is(err, ErrNotAuthorized) || got != from {
							t.Errorf("%s --%s-- by %s must be refused as not authorized, got %s %v", from, ev, role, got, err)
						}
					case w.reason && !reason:
						if !errors.Is(err, ErrReasonRequired) || got != from {
							t.Errorf("%s --%s-- without reason must need one, got %s %v", from, ev, got, err)
						}
					default:
						if err != nil || got != w.to {
							t.Errorf("%s --%s-- by %s = %s %v, want %s", from, ev, role, got, err, w.to)
						}
					}
				}
			}
		}
	}
}

// TestCanceledIsFinal: a canceled task never moves again; a material change needs a new
// approved proposal (no silent reassignment).
func TestCanceledIsFinal(t *testing.T) {
	for _, ev := range []Event{Acknowledge, Start, Block, Complete, Cancel, Reopen} {
		for _, role := range []Role{RoleAssignee, RoleSupervisor} {
			if _, err := Apply(Canceled, ev, role, true); !errors.Is(err, ErrIllegalTransition) {
				t.Errorf("canceled --%s-- by %s: %v", ev, role, err)
			}
		}
	}
	if !Terminal(Canceled) || !Terminal(Completed) || Terminal(InProgress) {
		t.Fatal("terminal states are completed and canceled only")
	}
}

// TestPairsMatchTheSQLGuard compares the pure table with the pairs migration 066 enforces, so the
// application and the database can never disagree on a legal task move.
func TestPairsMatchTheSQLGuard(t *testing.T) {
	raw, err := os.ReadFile("../migrate/066_approved_task_execution.sql")
	if err != nil {
		t.Fatal(err)
	}
	block := regexp.MustCompile(`(?s)\(OLD\.state,NEW\.state\) NOT IN \((.*?)\) THEN`).FindSubmatch(raw)
	if block == nil {
		t.Fatal("transition list not found in migration 066")
	}
	var sqlPairs [][2]State
	for _, m := range regexp.MustCompile(`\('([a-z_]+)','([a-z_]+)'\)`).FindAllSubmatch(block[1], -1) {
		sqlPairs = append(sqlPairs, [2]State{State(m[1]), State(m[2])})
	}
	sort.Slice(sqlPairs, func(i, j int) bool {
		if sqlPairs[i][0] != sqlPairs[j][0] {
			return sqlPairs[i][0] < sqlPairs[j][0]
		}
		return sqlPairs[i][1] < sqlPairs[j][1]
	})
	goPairs := Pairs()
	if len(sqlPairs) != len(goPairs) {
		t.Fatalf("SQL allows %v, Go allows %v", sqlPairs, goPairs)
	}
	for i := range goPairs {
		if goPairs[i] != sqlPairs[i] {
			t.Fatalf("SQL allows %v, Go allows %v", sqlPairs, goPairs)
		}
	}
}
