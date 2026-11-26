package infra

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSystemClockSleep(t *testing.T) {
	var clk SystemClock
	if err := clk.Sleep(context.Background(), 0); err != nil {
		t.Fatalf("Sleep(0) = %v", err)
	}
	if err := clk.Sleep(context.Background(), -time.Second); err != nil {
		t.Fatalf("Sleep(-1s) = %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := clk.Sleep(canceled, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("Sleep(canceled, 0) = %v, want context.Canceled", err)
	}
	ctx, cancelLater := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancelLater()
	if err := clk.Sleep(ctx, 5*time.Second); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Sleep(timeout) = %v, want context.DeadlineExceeded", err)
	}
	if err := clk.Sleep(context.Background(), 5*time.Millisecond); err != nil {
		t.Fatalf("Sleep(5ms) = %v", err)
	}
}
