package infra

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

type OutputMode uint8

const (
	OutputStream OutputMode = iota
	OutputCapture
)

const (
	MaxCapturedStdout = 1 << 20
	tailCapacity      = 64 << 10
	tailLines         = 200
)

var ErrOutputLimit = errors.New("stdout exceeds 1 MiB capture limit")
var ErrProcessCleanup = errors.New("process tree cleanup failed")

type ProcessSpec struct {
	Command    string
	Args       []string
	Dir        string
	Prompt     []byte
	Mode       OutputMode
	Diagnostic io.Writer
	Timeout    time.Duration
	// StopOnOutput checks a rolling window independently for each captured stream.
	StopOnOutput func([]byte) bool
}

type ProcessResult struct {
	ExitCode int
	Stdout   []byte
	Tail     []byte
}

type ProcessRunner interface {
	Run(context.Context, ProcessSpec) (ProcessResult, error)
}

type SystemProcessRunner struct{}

func (SystemProcessRunner) Run(ctx context.Context, spec ProcessSpec) (ProcessResult, error) {
	if spec.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, spec.Timeout)
		defer cancel()
	}
	result := ProcessResult{ExitCode: -1}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	cmd, err := processCommand(spec.Command, spec.Args)
	if err != nil {
		return result, err
	}
	cmd.Dir = spec.Dir
	cmd.Env = append(cmd.Environ(), "CI=true", "NO_COLOR=1", "FORCE_COLOR=0", "PYTHONUNBUFFERED=1")
	if spec.Prompt != nil {
		cmd.Stdin = bytes.NewReader(spec.Prompt)
	}
	// A nil Cmd.Stdin is connected to os.DevNull by os/exec.
	diagnostic := spec.Diagnostic
	if diagnostic == nil {
		diagnostic = os.Stderr
	}
	tail := &outputTail{}
	log := io.MultiWriter(tail, diagnostic)
	capture := &stdoutCapture{}
	cmd.Stdout, cmd.Stderr = log, log
	if spec.Mode == OutputCapture {
		cmd.Stdout, cmd.Stderr = capture, tail
		if spec.StopOnOutput != nil {
			stopCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			ctx = stopCtx
			cmd.Stdout = &outputWatch{dst: capture, match: spec.StopOnOutput, stop: cancel}
			cmd.Stderr = &outputWatch{dst: tail, match: spec.StopOnOutput, stop: cancel}
		}
	}
	cmd.WaitDelay = time.Second
	tree, err := startProcessTree(cmd)
	if err != nil {
		return result, fmt.Errorf("start %q: %w", spec.Command, err)
	}

	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	stopped := false
	select {
	case err = <-wait:
	case <-ctx.Done():
		select {
		case err = <-wait:
		default:
			stopped = true
			stopErr := tree.stop()
			<-wait
			err = ctx.Err()
			if stopErr != nil {
				err = errors.Join(err, ErrProcessCleanup, stopErr)
			}
		}
	}
	if !stopped {
		if closeErr := tree.close(); closeErr != nil {
			err = errors.Join(err, ErrProcessCleanup, closeErr)
		}
	}
	result.ExitCode = cmd.ProcessState.ExitCode()
	result.Tail = tail.lastLines()
	if spec.Mode == OutputCapture {
		var overflow bool
		result.Stdout, overflow = capture.result()
		if overflow {
			err = errors.Join(err, ErrOutputLimit)
		}
	}
	if err != nil {
		return result, fmt.Errorf("run %q: %w", spec.Command, err)
	}
	return result, nil
}

type outputWatch struct {
	mu    sync.Mutex
	dst   io.Writer
	match func([]byte) bool
	stop  func()
	tail  []byte
}

func (w *outputWatch) Write(p []byte) (int, error) {
	n, err := w.dst.Write(p)
	w.mu.Lock()
	w.tail = append(w.tail, p[:n]...)
	triggered := w.match(w.tail)
	if len(w.tail) > 128 {
		w.tail = append(w.tail[:0], w.tail[len(w.tail)-128:]...)
	}
	w.mu.Unlock()
	if triggered {
		w.stop()
	}
	return n, err
}

type stdoutCapture struct {
	mu       sync.Mutex
	buffer   bytes.Buffer
	overflow bool
}

func (c *stdoutCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	keep := min(len(p), MaxCapturedStdout-c.buffer.Len())
	_, _ = c.buffer.Write(p[:keep])
	c.overflow = c.overflow || keep < len(p)
	return len(p), nil
}

func (c *stdoutCapture) result() ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return bytes.Clone(c.buffer.Bytes()), c.overflow
}

type outputTail struct {
	mu    sync.Mutex
	data  [tailCapacity]byte
	next  int
	count int
}

func (t *outputTail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := len(p)
	if n >= len(t.data) {
		p = p[n-len(t.data):]
	}
	first := copy(t.data[t.next:], p)
	copy(t.data[:], p[first:])
	t.next = (t.next + len(p)) % len(t.data)
	t.count = min(len(t.data), t.count+len(p))
	return n, nil
}

func (t *outputTail) lastLines() []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	start := (t.next - t.count + len(t.data)) % len(t.data)
	first := min(t.count, len(t.data)-start)
	data := make([]byte, 0, t.count)
	data = append(data, t.data[start:start+first]...)
	data = append(data, t.data[:t.count-first]...)
	end := len(data)
	if end > 0 && data[end-1] == '\n' {
		end--
	}
	for i := 0; i < tailLines; i++ {
		end = bytes.LastIndexByte(data[:end], '\n')
		if end < 0 {
			return data
		}
	}
	return data[end+1:]
}
