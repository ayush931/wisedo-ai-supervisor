package main

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestSafeMigrationErrorReportsActionableCategoryWithoutRawCause(t *testing.T) {
	secretCause := errors.New("password=never-log postgres://user:secret@db/supervisor")
	if got := safeMigrationError(secretCause); strings.Contains(got, "secret") || strings.Contains(got, "postgres://") {
		t.Fatalf("raw cause leaked: %q", got)
	}
	pgFailure := &pgconn.PgError{Code: "42501", Message: "permission denied for table source_heads"}
	if got := safeMigrationError(pgFailure); got != "PostgreSQL SQLSTATE 42501: permission denied for table source_heads" {
		t.Fatalf("PostgreSQL diagnostic %q", got)
	}
	networkFailure := &net.OpError{Op: "dial", Net: "tcp", Addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 5432}, Err: errors.New("connection refused")}
	if got := safeMigrationError(networkFailure); got != "database network failure: dial tcp" {
		t.Fatalf("network diagnostic %q", got)
	}
}

// WIS-699: a migration or provisioning URL refused by the environment's rule is reported by its
// setting and reason, never by its value; without this both loggers printed only a type name.
func TestAdminDSNRefusalIsLoggedWithoutTheDSN(t *testing.T) {
	t.Setenv("SUPERVISOR_ENVIRONMENT", "production")
	dsn := "postgresql://migrator:never-log@db.example.com/supervisor?sslmode=disable"
	err := config.ValidateAdminDSN("SUPERVISOR_MIGRATION_URL", dsn)
	if err == nil {
		t.Fatal("a production URL without certificate verification was accepted")
	}
	for name, got := range map[string]string{"migrate": safeMigrationError(err), "provision": safeProvisionError(err)} {
		if !strings.Contains(got, "SUPERVISOR_MIGRATION_URL must verify the server certificate") || strings.Contains(got, "never-log") || strings.Contains(got, "example.com") {
			t.Errorf("%s logs %q", name, got)
		}
	}
}

// WIS-627: span indexing runs on its own loop so a slow embedding endpoint never delays the
// dispatch tick. The loop never overlaps passes, and a pass blocked on the endpoint ends with ctx.
func TestEveryRunsOnePassAtATimeAndStopsWithContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var active, peak, calls atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		every(ctx, time.Millisecond, func(ctx context.Context) {
			n := active.Add(1)
			if n > peak.Load() {
				peak.Store(n)
			}
			if calls.Add(1) == 3 {
				cancel()
				<-ctx.Done() // a pass blocked on a slow endpoint returns when the worker stops
			} else {
				time.Sleep(5 * time.Millisecond) // longer than the interval
			}
			active.Add(-1)
		})
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("loop did not stop after cancel")
	}
	if calls.Load() != 3 || peak.Load() != 1 {
		t.Fatalf("calls=%d peak concurrency=%d, want 3 and 1", calls.Load(), peak.Load())
	}
}

// The post-tick guard, deterministically: a tick that arrives after the worker was stopped
// starts no pass, and one that arrives while it runs does (WIS-627, Codex review of #271).
func TestRunPassSkipsAStoppedWorker(t *testing.T) {
	var calls int
	pass := func(context.Context) { calls++ }

	ctx, cancel := context.WithCancel(context.Background())
	if !runPass(ctx, pass) || calls != 1 {
		t.Fatalf("running worker: ran=%d, want one pass", calls)
	}
	cancel()
	if runPass(ctx, pass) || calls != 1 {
		t.Fatalf("stopped worker: ran=%d passes, want no further pass", calls)
	}
}

// Stress coverage for the same race through the real ticker; the deterministic check is above.
// A pass that ends after its worker was stopped, with a tick already pending, is the last one:
// select picks a ready tick and a ready cancel at random, so before the fix one stop in two ran
// one more pass (a flaky TestEveryRunsOnePassAtATimeAndStopsWithContext under -race, PR #269 CI).
// Repeated so the pre-fix behaviour cannot pass by chance (0.5^100).
func TestEveryRunsNoPassAfterCancel(t *testing.T) {
	for i := 0; i < 100; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		var calls atomic.Int32
		every(ctx, time.Millisecond, func(context.Context) {
			calls.Add(1)
			cancel()
			time.Sleep(3 * time.Millisecond) // a tick is waiting when the pass ends
		})
		if n := calls.Load(); n != 1 {
			t.Fatalf("run %d: %d passes after cancel, want 1", i, n)
		}
	}
}
