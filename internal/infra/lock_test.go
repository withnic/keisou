package infra

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/withnic/keisou/internal/model"
)

func TestRunLockExclusiveAndRelease(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	lock, err := AcquireRunLock(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Release() })
	if _, err := AcquireRunLock(ctx, dir); model.ExitCode(err) != 7 {
		t.Fatalf("second lock = %v, want E_LOCK_CONFLICT", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".keisou", "run.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("released file stat = %v", err)
	}
	again, err := AcquireRunLock(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := again.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestRunLockReclaimsStaleOwnerOnce(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	child := exec.Command(os.Args[0], "-test.run=^$")
	if err := child.Run(); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(dir, ".keisou")
	if err := os.Mkdir(state, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(state, "run.lock")
	record := `{"pid":` + strconv.Itoa(child.Process.Pid) + `,"started_at":"2026-11-26T00:00:00Z"}`
	if err := os.WriteFile(path, []byte(record), 0600); err != nil {
		t.Fatal(err)
	}
	const contenders = 8
	locks := make(chan *RunLock, contenders)
	var group sync.WaitGroup
	for i := 0; i < contenders; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			lock, err := AcquireRunLock(ctx, dir)
			if err == nil {
				locks <- lock
			} else if model.ExitCode(err) != 7 {
				t.Errorf("contender: %v", err)
			}
		}()
	}
	group.Wait()
	close(locks)
	if len(locks) != 1 {
		t.Errorf("successful contenders = %d, want 1", len(locks))
	}
	for lock := range locks {
		if err := lock.Release(); err != nil {
			t.Error(err)
		}
	}
}

func TestRunLockEdgeCases(t *testing.T) {
	var nilLock *RunLock
	if err := nilLock.Release(); err != nil {
		t.Fatalf("nil Release = %v", err)
	}
	for _, pid := range []int{0, -1} {
		if alive, err := pidAlive(pid); alive || err != nil {
			t.Fatalf("pidAlive(%d) = %v, %v; want false, nil", pid, alive, err)
		}
	}

	dir := t.TempDir()
	state := filepath.Join(dir, ".keisou")
	if err := os.Mkdir(state, 0755); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(state, "run.lock")

	// Live PID in run.lock without an active OS guard must still report conflict.
	live := `{"pid":` + strconv.Itoa(os.Getpid()) + `,"started_at":"2026-11-26T00:00:00Z"}`
	if err := os.WriteFile(lockPath, []byte(live), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireRunLock(context.Background(), dir); model.ExitCode(err) != 7 {
		t.Fatalf("live PID without guard = %v, want E_LOCK_CONFLICT", err)
	}

	// Corrupt lock file must retry and fail with E_LOCK_CONFLICT without deleting the file.
	if err := os.WriteFile(lockPath, []byte("{corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireRunLock(context.Background(), dir); model.ExitCode(err) != 7 {
		t.Fatalf("corrupt lock = %v, want E_LOCK_CONFLICT", err)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("corrupt lock file was unexpectedly deleted: %v", err)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := AcquireRunLock(canceled, dir); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled AcquireRunLock = %v, want context.Canceled", err)
	}
}
