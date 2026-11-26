package infra

import (
	"context"
	"time"
)

type Clock interface {
	Now() time.Time
	Sleep(context.Context, time.Duration) error
}

type SystemClock struct{}

// SleepAsync exposes an injected clock's cancellable wait to a select.
func SleepAsync(ctx context.Context, clock Clock, d time.Duration) <-chan error {
	done := make(chan error, 1)
	go func() { done <- clock.Sleep(ctx, d) }()
	return done
}

func (SystemClock) Now() time.Time { return time.Now() }

func (SystemClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ctx.Err()
	}
}
