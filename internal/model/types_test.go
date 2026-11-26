package model

import (
	"errors"
	"fmt"
	"math"
	"testing"
)

func TestClampPercent(t *testing.T) {
	for _, test := range []struct{ input, want float64 }{
		{-1, 0}, {95.4, 95.4}, {100, 100}, {101, 100},
		{math.NaN(), 0}, {math.Inf(1), 0}, {math.Inf(-1), 0},
	} {
		if got := ClampPercent(test.input); got != test.want {
			t.Errorf("ClampPercent(%v) = %v, want %v", test.input, got, test.want)
		}
	}
}

func TestExitCode(t *testing.T) {
	if ExitCode(nil) != 0 || ExitCode(errors.New("failure")) != 1 {
		t.Fatal("incorrect success or default exit code")
	}
	for code, want := range map[ErrorCode]int{
		EInternal: 1, EInvalidArgs: 1, EConfigInvalid: 2, ENoAvailableProvider: 3,
		EAllQuotaExhausted: 4, EHandoffFailed: 5, EMaxRelayExceeded: 6,
		ELockConflict: 7, EInterrupted: 130,
	} {
		err := fmt.Errorf("outer: %w", &Error{Code: code})
		if got := ExitCode(err); got != want {
			t.Errorf("ExitCode(%v) = %d, want %d", err, got, want)
		}
	}
}
