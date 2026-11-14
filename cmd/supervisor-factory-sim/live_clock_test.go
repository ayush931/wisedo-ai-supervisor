package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/workstation"
)

// fakeClock jumps straight to each wait target; a wait for lateAt lands late instead.
type fakeClock struct {
	now    time.Time
	lateAt time.Time
	late   time.Duration
	waits  int
}

func (c *fakeClock) Now() time.Time { return c.now }
func (c *fakeClock) WaitUntil(ctx context.Context, at time.Time) error {
	if c.waits++; c.waits > 10_000 {
		return errors.New("runaway wait loop")
	}
	if at.After(c.now) {
		c.now = at
	}
	if at.Equal(c.lateAt) {
		c.now = at.Add(c.late)
	}
	return ctx.Err()
}

type recordingServer struct {
	sync.Mutex
	paths []string
	body  string
}

func (s *recordingServer) start(t *testing.T) *loader {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.Lock()
		s.paths = append(s.paths, r.Method+" "+r.URL.Path)
		body := s.body
		s.Unlock()
		if body == "" {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(500)
			_, _ = io.WriteString(w, `{"code":"FAKE_STOP"}`)
			return
		}
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	l, err := newLoader(srv.URL, func(k string) string {
		if k == "SUPERVISOR_ENVIRONMENT" {
			return "local"
		}
		return "fake-session"
	})
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// WIS-709 review finding 7: runLive's wall-start, bootstrap and interval deadlines and the fold
// deadline run on the injected clock. The bootstrap seam replaces only the HTTP master-data load.
func TestWIS709LiveDeadlinesOnFakeClock(t *testing.T) {
	d, schedule, err := generateLive(278, "2026-09-01", time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC), 60)
	if err != nil {
		t.Fatal(err)
	}
	tolerance := 15 * time.Minute / time.Duration(schedule.Speed)
	restore := liveBootstrap
	t.Cleanup(func() { liveBootstrap = restore })
	run := func(clock *fakeClock, bootstrap func(), l *loader) error {
		liveBootstrap = func(context.Context, *loader, dataset) ([][]workstation.ShiftPlanV1, error) {
			bootstrap()
			return make([][]workstation.ShiftPlanV1, 3), nil
		}
		_, err := runLive(context.Background(), l, d, schedule, clock, t.TempDir(), "", io.Discard)
		return err
	}
	expectErr := func(t *testing.T, err error, want string) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("got %v, want %q", err, want)
		}
	}

	t.Run("wall start already passed", func(t *testing.T) {
		booted := false
		expectErr(t, run(&fakeClock{now: schedule.WallStart}, func() { booted = true }, (&recordingServer{}).start(t)), "wall start must be in the future")
		if booted {
			t.Fatal("bootstrapped after the wall start had passed")
		}
	})
	t.Run("bootstrap overran the wall start", func(t *testing.T) {
		clock := &fakeClock{now: schedule.WallStart.Add(-time.Minute)}
		expectErr(t, run(clock, func() { clock.now = schedule.WallStart }, (&recordingServer{}).start(t)), "bootstrap missed the frozen wall start")
	})
	first := schedule.IntervalEnd(0)
	t.Run("interval sample later than one interval", func(t *testing.T) {
		server := &recordingServer{}
		l := server.start(t)
		expectErr(t, run(&fakeClock{now: schedule.WallStart.Add(-time.Minute), lateAt: first, late: tolerance + time.Nanosecond}, func() {}, l), "missed interval 0 deadline")
		if len(server.paths) != 0 {
			t.Fatalf("posted after a missed deadline: %v", server.paths)
		}
	})
	t.Run("interval sample exactly at the tolerance is posted", func(t *testing.T) {
		server := &recordingServer{}
		l := server.start(t)
		err := run(&fakeClock{now: schedule.WallStart.Add(-time.Minute), lateAt: first, late: tolerance}, func() {}, l)
		expectErr(t, err, "FAKE_STOP")
		if len(server.paths) != 1 || !strings.HasSuffix(server.paths[0], "/batches") {
			t.Fatalf("interval 0 batch not posted: %v", server.paths)
		}
	})

	folded := `{"ingest":{"processed_sequence":10},"completeness":{"complete_sequence":10,"quarantined_count":0}}`
	for name, tc := range map[string]struct {
		body, want string
		waits      int
	}{
		"folds":             {folded, "", 0},
		"never folds":       {`{"ingest":{"processed_sequence":9},"completeness":{"complete_sequence":9,"quarantined_count":0}}`, "did not finish folding", 300},
		"quarantined event": {`{"ingest":{"processed_sequence":10},"completeness":{"complete_sequence":10,"quarantined_count":1}}`, "quarantined 1 events", 0},
	} {
		t.Run("fold "+name, func(t *testing.T) {
			server := &recordingServer{body: tc.body}
			l := server.start(t)
			clock := &fakeClock{now: schedule.WallStart}
			start := clock.now
			err := awaitLiveFold(context.Background(), l, clock, "/operational-sources/x", 10, &liveReport{})
			if tc.want == "" && err != nil {
				t.Fatal(err)
			}
			if tc.want != "" {
				expectErr(t, err, tc.want)
			}
			// 60 seconds of 200 ms polls on the fake clock, then the deadline.
			if clock.waits != tc.waits || (tc.waits > 0 && clock.now.Sub(start) != 60*time.Second) {
				t.Fatalf("waits=%d elapsed=%s", clock.waits, clock.now.Sub(start))
			}
		})
	}
}
