package agent

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestCallCost(t *testing.T) {
	price := &TokenPrice{InputMicroUSDPerMillion: 3_000_000, OutputMicroUSDPerMillion: 15_000_000}
	for _, c := range []struct {
		name    string
		p       Pricing
		in, out int64
		want    int64
		err     error
	}{
		{"local is free", Pricing{Local: true}, 8000, 2000, 0, nil},
		{"local ignores a configured price", Pricing{Local: true, Price: price}, 8000, 2000, 0, nil},
		{"priced", Pricing{Price: price}, 1000, 2000, 33000, nil},
		{"rounds up, never down to zero", Pricing{Price: &TokenPrice{1, 1}}, 1, 0, 1, nil},
		{"missing price blocks a billable call", Pricing{}, 1, 1, 0, ErrPriceMissing},
		{"negative tokens", Pricing{Local: true}, -1, 0, 0, ErrInvalidBudget},
		{"price beyond the overflow bound", Pricing{Price: &TokenPrice{maxPrice + 1, 0}}, 1, 0, 0, ErrInvalidBudget},
	} {
		got, err := CallCost(c.p, c.in, c.out)
		if got != c.want || !errors.Is(err, c.err) {
			t.Errorf("%s: CallCost = %d, %v; want %d, %v", c.name, got, err, c.want, c.err)
		}
	}
}

func TestLimitsBounds(t *testing.T) {
	l := DefaultLimits()
	if err := l.Validate(); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	// The wall-time bound wins when the activity deadline is later.
	if got := l.CycleDeadline(start, start.Add(time.Hour)); !got.Equal(start.Add(4 * time.Minute)) {
		t.Fatalf("deadline = %v", got)
	}
	// The activity deadline less 15 s wins when it is earlier.
	if got := l.CycleDeadline(start, start.Add(time.Minute)); !got.Equal(start.Add(45 * time.Second)) {
		t.Fatalf("deadline = %v", got)
	}
	if EstimateInputTokens(24000) != 8000 || EstimateInputTokens(24001) != 8001 || EstimateInputTokens(0) != 0 {
		t.Fatal("input estimate is not ceil(bytes/3)")
	}
	for _, bad := range []func(*Limits){
		func(l *Limits) { l.Depth = 2 },
		func(l *Limits) { l.MaxConcurrent = 0 },
		func(l *Limits) { l.Requests = 0 },
		func(l *Limits) { l.MaxOutputTokens = int(l.Tokens) + 1 },
	} {
		c := DefaultLimits()
		bad(&c)
		if !errors.Is(c.Validate(), ErrInvalidBudget) {
			t.Errorf("limits %+v accepted", c)
		}
	}
}

// TestQueueAndConcurrencyBound: 2 in flight plus 8 queued, then one more is refused at once.
func TestQueueAndConcurrencyBound(t *testing.T) {
	l := DefaultLimits()
	s := NewSlots(l.MaxConcurrent, l.MaxQueued)
	ctx := context.Background()
	var inFlight []func()
	for i := 0; i < 2; i++ {
		release, err := s.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		inFlight = append(inFlight, release)
	}
	got := make(chan error, 8)
	queued := make(chan func(), 8)
	for i := 0; i < 8; i++ {
		go func() {
			release, err := s.Acquire(ctx)
			got <- err
			if err == nil {
				queued <- release
			}
		}()
	}
	for deadline := time.Now().Add(5 * time.Second); s.Waiting() < 8; {
		if time.Now().After(deadline) {
			t.Fatalf("only %d waiting", s.Waiting())
		}
		time.Sleep(time.Millisecond)
	}
	// A bounded context makes a missing queue bound fail instead of hang.
	eleventh, stop := context.WithTimeout(ctx, 2*time.Second)
	defer stop()
	if _, err := s.Acquire(eleventh); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("11th caller = %v, want ErrQueueFull", err)
	}
	// Control: releasing in-flight slots lets every queued caller run, two at a time.
	for _, r := range inFlight {
		r()
	}
	for i := 0; i < 8; i++ {
		if err := <-got; err != nil {
			t.Fatalf("queued caller %d: %v", i, err)
		}
		(<-queued)()
	}
	if s.Waiting() != 0 {
		t.Fatalf("%d still waiting", s.Waiting())
	}
}
