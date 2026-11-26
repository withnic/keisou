//go:build linux || darwin

package infra

import (
	"errors"
	"os/exec"
	"syscall"
	"time"
)

func processCommand(command string, args []string) (*exec.Cmd, error) {
	return exec.Command(command, args...), nil
}

type processTree struct {
	pgid int
}

func startProcessTree(cmd *exec.Cmd) (processTree, error) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return processTree{}, err
	}
	return processTree{pgid: cmd.Process.Pid}, nil
}

func (tree processTree) signal(signal syscall.Signal) error {
	return syscall.Kill(-tree.pgid, signal)
}

func (tree processTree) close() error {
	err := tree.signal(syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func (tree processTree) stop() error {
	err := tree.signal(syscall.SIGTERM)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	if err != nil {
		return errors.Join(err, tree.close())
	}
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	poll := time.NewTicker(50 * time.Millisecond)
	defer poll.Stop()
	for {
		select {
		case <-deadline.C:
			return tree.close()
		case <-poll.C:
			if errors.Is(tree.signal(0), syscall.ESRCH) {
				return nil
			}
		}
	}
}
