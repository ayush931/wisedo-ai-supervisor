package cases

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// PR #232 review: a case that fails on every 1-second sweep is logged once per window, per case.
func TestSweepFailureLogIsRateLimitedPerCase(t *testing.T) {
	l := &failureLog{last: map[string]time.Time{}}
	t0 := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	steps := []struct {
		key  string
		at   time.Duration
		want bool
	}{
		{"t/a", 0, true},
		{"t/a", time.Second, false},
		{"t/b", time.Second, true},
		{"t/a", sweepFailureEvery - time.Second, false},
		{"t/a", sweepFailureEvery, true},
		{"t/a", sweepFailureEvery + time.Second, false},
	}
	for i, s := range steps {
		if got := l.allow(s.key, t0.Add(s.at)); got != s.want {
			t.Fatalf("step %d %s at %v: logged=%v want %v", i, s.key, s.at, got, s.want)
		}
	}
}

// The logged class never carries the error text, which may hold SQL (rule 07).
func TestSweepErrorClass(t *testing.T) {
	for err, want := range map[error]string{
		fmt.Errorf("load: %w", errIntegrity):               "integrity",
		ErrNotFound:                                        "not_found",
		context.DeadlineExceeded:                           "canceled",
		errors.New(`ERROR: relation "supervisor.x" (SQL)`): "storage",
	} {
		if got := sweepErrorClass(err); got != want {
			t.Fatalf("%v: class %q want %q", err, got, want)
		}
	}
}
