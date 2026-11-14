package factorysim

import (
	"context"
	"testing"
	"time"
)

func TestWIS709Schedule(t *testing.T) {
	model := time.Date(2026, 9, 1, 6, 0, 0, 0, time.FixedZone("IST", 19800))
	wall := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	s := Schedule{model, wall, 60}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := s.IntervalEnd(95); !got.Equal(wall.Add(24 * time.Minute)) {
		t.Fatal(got)
	}
	for i := range 96 {
		if got := s.IntervalEnd(i); !got.Equal(wall.Add(time.Duration(i+1) * 15 * time.Second)) {
			t.Fatal(i, got)
		}
	}
	for _, speed := range []int64{0, -1, 301} {
		s.Speed = speed
		if s.Validate() == nil {
			t.Fatalf("accepted speed %d", speed)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := (WallClock{}).WaitUntil(ctx, time.Now().Add(time.Hour)); err != context.Canceled {
		t.Fatal(err)
	}
}
