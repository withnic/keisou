package model

import "errors"

type ErrorCode string

const (
	EInternal            ErrorCode = "E_INTERNAL"
	EInvalidArgs         ErrorCode = "E_INVALID_ARGS"
	EConfigInvalid       ErrorCode = "E_CONFIG_INVALID"
	ENoAvailableProvider ErrorCode = "E_NO_AVAILABLE_PROVIDER"
	EAllQuotaExhausted   ErrorCode = "E_ALL_QUOTA_EXHAUSTED"
	EHandoffFailed       ErrorCode = "E_HANDOFF_FAILED"
	EMaxRelayExceeded    ErrorCode = "E_MAX_RELAY_EXCEEDED"
	ELockConflict        ErrorCode = "E_LOCK_CONFLICT"
	EInterrupted         ErrorCode = "E_INTERRUPTED"
)

type Error struct {
	Code ErrorCode
	Err  error
}

func (e *Error) Error() string {
	if e.Err == nil {
		return string(e.Code)
	}
	return string(e.Code) + ": " + e.Err.Error()
}

func (e *Error) Unwrap() error { return e.Err }

func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var structured *Error
	if errors.As(err, &structured) {
		switch structured.Code {
		case EConfigInvalid:
			return 2
		case ENoAvailableProvider:
			return 3
		case EAllQuotaExhausted:
			return 4
		case EHandoffFailed:
			return 5
		case EMaxRelayExceeded:
			return 6
		case ELockConflict:
			return 7
		case EInterrupted:
			return 130
		}
	}
	return 1
}
