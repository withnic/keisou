package infra

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/withnic/keisou/internal/model"
)

type lockRecord struct {
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started_at"`
}

type RunLock struct {
	path    string
	info    os.FileInfo
	release func() error
	once    sync.Once
	err     error
}

func lockConflict(err error) error {
	return &model.Error{Code: model.ELockConflict, Err: err}
}

func AcquireRunLock(ctx context.Context, workDir string) (*RunLock, error) {
	ctx, cancel := context.WithTimeout(ctx, fileTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir := filepath.Join(workDir, ".keisou")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create run state directory: %w", err)
	}
	// Keep a stable inode for the OS lock. Holding it through Release also
	// prevents two contenders from deleting each other's replacement run.lock.
	guard, err := os.OpenFile(filepath.Join(dir, "run.guard"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open run guard: %w", err)
	}
	if err := lockGuard(guard); err != nil {
		guard.Close()
		return nil, err
	}
	path := filepath.Join(dir, "run.lock")
	lock, err := acquireRunFile(ctx, path)
	if err != nil {
		guard.Close()
		return nil, err
	}
	lock.release = guard.Close
	return lock, nil
}

func acquireRunFile(ctx context.Context, path string) (*RunLock, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if errors.Is(err, os.ErrExist) {
		if err := reclaimRunFile(ctx, path); err != nil {
			return nil, err
		}
		file, err = os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	}
	if errors.Is(err, os.ErrExist) {
		return nil, lockConflict(err)
	}
	if err != nil {
		return nil, fmt.Errorf("create run lock: %w", err)
	}

	info, writeErr := file.Stat()
	if writeErr == nil {
		writeErr = json.NewEncoder(file).Encode(lockRecord{PID: os.Getpid(), StartedAt: time.Now().UTC()})
	}
	if err := errors.Join(writeErr, file.Close(), ctx.Err()); err != nil {
		return nil, fmt.Errorf("initialize run lock: %w", errors.Join(err, os.Remove(path)))
	}
	return &RunLock{path: path, info: info}, nil
}

func reclaimRunFile(ctx context.Context, path string) error {
	var record lockRecord
	for attempt := 0; ; attempt++ {
		record = lockRecord{}
		err := ReadJSON(ctx, path, func(decoder *json.Decoder) error {
			if err := decoder.Decode(&record); err != nil {
				return err
			}
			if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
				return errors.New("unexpected content after run lock record")
			}
			return nil
		})
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err == nil && record.PID > 0 {
			break
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if attempt == 3 {
			return lockConflict(fmt.Errorf("unreadable run lock %q", path))
		}
		if err := (SystemClock{}).Sleep(ctx, 50*time.Millisecond); err != nil {
			return err
		}
	}
	alive, err := pidAlive(record.PID)
	if err != nil {
		return fmt.Errorf("check lock owner PID %d: %w", record.PID, err)
	}
	if alive {
		return lockConflict(fmt.Errorf("PID %d owns %q", record.PID, path))
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove stale run lock: %w", err)
	}
	return nil
}

func (lock *RunLock) Release() error {
	if lock == nil {
		return nil
	}
	lock.once.Do(func() {
		info, err := os.Stat(lock.path)
		switch {
		case err == nil && os.SameFile(lock.info, info):
			err = os.Remove(lock.path)
		case errors.Is(err, os.ErrNotExist):
			err = nil
		}
		if err := errors.Join(err, lock.release()); err != nil {
			lock.err = fmt.Errorf("release run lock: %w", err)
		}
	})
	return lock.err
}
