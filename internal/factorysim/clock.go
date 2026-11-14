package factorysim

import (
	"context"
	"errors"
	"time"
)

// Schedule keeps model time separate from the real timestamps checked by the API.
type Schedule struct {
	ModelStart time.Time `json:"model_start"`
	WallStart  time.Time `json:"wall_start"`
	Speed      int64     `json:"model_seconds_per_wall_second"`
}

func (s Schedule) Validate() error {
	if s.ModelStart.IsZero() || s.WallStart.IsZero() || s.Speed < 1 || s.Speed > 300 || s.WallStart.Nanosecond() != 0 {
		return errors.New("schedule requires model start, whole-second wall start and speed 1..300")
	}
	return nil
}

func (s Schedule) Wall(model time.Time) time.Time {
	return s.WallStart.Add(model.Sub(s.ModelStart) / time.Duration(s.Speed))
}

func (s Schedule) IntervalEnd(index int) time.Time {
	return s.Wall(s.ModelStart.Add(time.Duration(index+1) * 15 * time.Minute))
}

type Clock interface {
	Now() time.Time
	WaitUntil(context.Context, time.Time) error
}

type WallClock struct{}

func (WallClock) Now() time.Time { return time.Now() }
func (WallClock) WaitUntil(ctx context.Context, at time.Time) error {
	timer := time.NewTimer(time.Until(at))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
