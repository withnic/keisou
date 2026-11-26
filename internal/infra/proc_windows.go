package infra

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

var (
	createJobObject    = kernel32.NewProc("CreateJobObjectW")
	setJobInformation  = kernel32.NewProc("SetInformationJobObject")
	assignProcessToJob = kernel32.NewProc("AssignProcessToJobObject")
	resumeProcess      = syscall.NewLazyDLL("ntdll.dll").NewProc("NtResumeProcess")
)

type jobBasicLimits struct {
	perProcessUserTime int64
	perJobUserTime     int64
	limitFlags         uint32
	minWorkingSet      uintptr
	maxWorkingSet      uintptr
	activeProcessLimit uint32
	affinity           uintptr
	priorityClass      uint32
	schedulingClass    uint32
}

type jobExtendedLimits struct {
	basic                 jobBasicLimits
	ioCounters            [6]uint64
	processMemoryLimit    uintptr
	jobMemoryLimit        uintptr
	peakProcessMemoryUsed uintptr
	peakJobMemoryUsed     uintptr
}

type processTree struct {
	job syscall.Handle
	pid int
}

func processCommand(command string, args []string) (*exec.Cmd, error) {
	path, err := exec.LookPath(command)
	if err != nil {
		return nil, err
	}
	ext := strings.ToLower(filepath.Ext(path))
	if ext != ".cmd" && ext != ".bat" {
		return exec.Command(path, args...), nil
	}

	// cmd.exe interprets metacharacters differently from native executables.
	// Permit batch switches and literal quoted values; prompts always use stdin.
	path, err = filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	parts := make([]string, 0, len(args)+1)
	for _, item := range append([]string{path}, args...) {
		if strings.ContainsAny(item, "\"%!\r\n\x00") || strings.HasSuffix(item, "\\") {
			return nil, fmt.Errorf("unsupported character in batch command path or argument")
		}
		parts = append(parts, "\""+item+"\"")
	}
	shell := filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe")
	if !filepath.IsAbs(shell) {
		return nil, errors.New("SystemRoot must be an absolute path")
	}
	cmd := exec.Command(shell)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CmdLine: "\"" + shell + "\" /d /s /v:off /c \"" + strings.Join(parts, " ") + "\"",
	}
	return cmd, nil
}

func startProcessTree(cmd *exec.Cmd) (processTree, error) {
	const (
		createSuspended          = 0x4
		createNewProcessGroup    = 0x200
		extendedLimitInformation = 9
		killOnJobClose           = 0x2000
		// AssignProcessToJobObject needs SET_QUOTA and TERMINATE.
		processAccess = 0x100 | 0x1 | 0x800
	)
	job, _, err := createJobObject.Call(0, 0)
	if job == 0 {
		return processTree{}, fmt.Errorf("create job object: %w", err)
	}
	handle := syscall.Handle(job)
	limits := jobExtendedLimits{basic: jobBasicLimits{limitFlags: killOnJobClose}}
	ok, _, err := setJobInformation.Call(job, extendedLimitInformation, uintptr(unsafe.Pointer(&limits)), unsafe.Sizeof(limits))
	if ok == 0 {
		return processTree{}, errors.Join(fmt.Errorf("set job limits: %w", err), syscall.CloseHandle(handle))
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	if err := resumeProcess.Find(); err != nil {
		return processTree{}, errors.Join(fmt.Errorf("resolve process resume function: %w", err), syscall.CloseHandle(handle))
	}
	// The suspended child cannot spawn descendants before job assignment.
	cmd.SysProcAttr.CreationFlags |= createSuspended | createNewProcessGroup
	if err := cmd.Start(); err != nil {
		return processTree{}, errors.Join(err, syscall.CloseHandle(handle))
	}
	pid := cmd.Process.Pid
	attachErr := func() error {
		proc, err := syscall.OpenProcess(processAccess, false, uint32(pid))
		if err != nil {
			return fmt.Errorf("open suspended process: %w", err)
		}
		defer syscall.CloseHandle(proc)
		ok, _, err := assignProcessToJob.Call(job, uintptr(proc))
		if ok == 0 {
			return fmt.Errorf("assign process to job: %w", err)
		}
		status, _, _ := resumeProcess.Call(uintptr(proc))
		if status != 0 {
			return fmt.Errorf("resume process: NTSTATUS 0x%08x", uint32(status))
		}
		return nil
	}()
	if attachErr != nil {
		killErr := cmd.Process.Kill()
		closeErr := syscall.CloseHandle(handle)
		_ = cmd.Wait()
		return processTree{}, errors.Join(attachErr, killErr, closeErr)
	}
	return processTree{job: handle, pid: pid}, nil
}

func (tree processTree) close() error {
	return syscall.CloseHandle(tree.job)
}

func (tree processTree) stop() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := filepath.Join(os.Getenv("SystemRoot"), "System32", "taskkill.exe")
	if filepath.IsAbs(command) {
		// The job remains authoritative if taskkill races with the parent exiting.
		_ = exec.CommandContext(ctx, command, "/PID", strconv.Itoa(tree.pid), "/T", "/F").Run()
	}
	return tree.close()
}
