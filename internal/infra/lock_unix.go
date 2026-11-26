//go:build linux || darwin

package infra

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

func pidAlive(pid int) (bool, error) {
	if pid <= 0 {
		return false, nil
	}
	err := syscall.Kill(pid, 0)
	switch {
	case err == nil, errors.Is(err, syscall.EPERM):
		return true, nil
	case errors.Is(err, syscall.ESRCH):
		return false, nil
	default:
		return false, err
	}
}

func lockGuard(file *os.File) error {
	err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return lockConflict(fmt.Errorf("another process holds %q", file.Name()))
	}
	if err != nil {
		return fmt.Errorf("lock run guard: %w", err)
	}
	return nil
}
