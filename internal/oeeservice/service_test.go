package oeeservice

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

func validConfig() Config {
	return Config{RunDeadline: time.Minute, LeaseDuration: 15 * time.Second, MaxOpenRuns: 1, MaxConcurrentExecutions: 2, IdempotencyRetention: time.Hour, ExecutionOwner: "host:1"}
}

// ADR 0002 Sec. 10: configured maximums are enforced, so the service refuses to start with any
// limit missing rather than running unbounded.
func TestNewRequiresEveryLimit(t *testing.T) {
	db := &sql.DB{}
	if _, err := New(db, validConfig()); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Config){
		"deadline":   func(c *Config) { c.RunDeadline = 0 },
		"lease":      func(c *Config) { c.LeaseDuration = 0 },
		"capacity":   func(c *Config) { c.MaxOpenRuns = 0 },
		"executions": func(c *Config) { c.MaxConcurrentExecutions = 0 },
		"retention":  func(c *Config) { c.IdempotencyRetention = 0 },
		"owner":      func(c *Config) { c.ExecutionOwner = "" },
	} {
		c := validConfig()
		mutate(&c)
		if _, err := New(db, c); err == nil {
			t.Errorf("%s: missing limit accepted", name)
		}
	}
	if _, err := New(nil, validConfig()); err == nil {
		t.Error("nil database accepted")
	}
}

// ADR 0002 Sec. 15.1: separate permissions for reading, authoring and calculating, and
// publication. Publication is supervisor-only, as plan approval is; owner is no bypass.
func TestRolePermissions(t *testing.T) {
	for _, c := range []struct {
		role                                 string
		read, author, publish, calculateRole bool
	}{
		{"owner", true, true, false, true},
		{"supervisor", true, true, true, true},
		{"operator", true, false, false, false},
		{"reviewer", true, false, false, false},
		{"viewer", true, false, false, false},
	} {
		if allowed(c.role, read) != c.read || allowed(c.role, author) != c.author || allowed(c.role, publish) != c.publish || allowed(c.role, calculate) != c.calculateRole {
			t.Errorf("%s permissions", c.role)
		}
	}
}

// ADR 0002 Sec. 10: the in-process execution budget rejects work beyond its size instead of
// queueing it, releases exactly once, refuses new work once closed, and Close waits for every
// held slot.
func TestExecutionBudgetAndDrain(t *testing.T) {
	s, err := New(&sql.DB{}, validConfig())
	if err != nil {
		t.Fatal(err)
	}
	s.abortWait = 10 * time.Millisecond
	first, ok := s.reserveSlot()
	if !ok {
		t.Fatal("first slot refused")
	}
	second, ok := s.reserveSlot()
	if !ok {
		t.Fatal("second slot refused")
	}
	if _, ok := s.reserveSlot(); ok {
		t.Fatal("budget of 2 granted a third slot")
	}
	first()
	first() // idempotent: must not free a second slot
	third, ok := s.reserveSlot()
	if !ok {
		t.Fatal("released slot not reusable")
	}
	if _, ok := s.reserveSlot(); ok {
		t.Fatal("double release freed an extra slot")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := s.Close(ctx); err == nil {
		t.Fatal("Close returned while slots were still held")
	}
	if _, ok := s.reserveSlot(); ok {
		t.Fatal("closed service granted a slot")
	}
	if s.executions.Err() == nil {
		t.Fatal("a timed-out drain did not cancel the remaining executions")
	}
	second()
	third()
	if err := s.Close(context.Background()); err != nil {
		t.Fatalf("Close after release: %v", err)
	}
}
