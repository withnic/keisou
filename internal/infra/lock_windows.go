package infra

import (
	"errors"
	"fmt"
	"math"
	"os"
	"syscall"
	"unsafe"
)

var kernel32 = syscall.NewLazyDLL("kernel32.dll")
var lockFileEx = kernel32.NewProc("LockFileEx")

func pidAlive(pid int) (bool, error) {
	if pid <= 0 || uint64(pid) > math.MaxUint32 {
		return false, nil
	}
	const queryLimitedInformation = 0x1000
	handle, err := syscall.OpenProcess(queryLimitedInformation, false, uint32(pid))
	if errors.Is(err, syscall.ERROR_ACCESS_DENIED) {
		return true, nil
	}
	if errors.Is(err, syscall.Errno(87)) { // ERROR_INVALID_PARAMETER: PID does not exist.
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer syscall.CloseHandle(handle)
	var exitCode uint32
	if err := syscall.GetExitCodeProcess(handle, &exitCode); err != nil {
		return false, err
	}
	return exitCode == 259, nil // STILL_ACTIVE
}

func lockGuard(file *os.File) error {
	const exclusiveAndFailImmediately = 3
	var offset syscall.Overlapped
	ok, _, err := lockFileEx.Call(file.Fd(), exclusiveAndFailImmediately, 0, 1, 0, uintptr(unsafe.Pointer(&offset)))
	if ok != 0 {
		return nil
	}
	if errors.Is(err, syscall.Errno(33)) { // ERROR_LOCK_VIOLATION
		return lockConflict(fmt.Errorf("another process holds %q", file.Name()))
	}
	return fmt.Errorf("lock run guard: %w", err)
}
