package infra

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func childSpec(mode string, args ...string) ProcessSpec {
	return ProcessSpec{
		Command: os.Args[0], Args: append([]string{"-test.run=^TestProcessHelper$", "--", mode}, args...),
		Mode: OutputCapture, Timeout: 15 * time.Second,
	}
}

func TestProcessHelper(t *testing.T) {
	if len(os.Args) < 4 || os.Args[2] != "--" {
		return
	}
	switch os.Args[3] {
	case "capture":
		input, err := io.ReadAll(os.Stdin)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(os.Stdout, "%s:%s:%s:%s:%s", os.Getenv("CI"), os.Getenv("NO_COLOR"), os.Getenv("FORCE_COLOR"), os.Getenv("PYTHONUNBUFFERED"), input)
		fmt.Fprintln(os.Stderr, "diagnostic")
	case "overflow":
		fmt.Fprint(os.Stdout, strings.Repeat("x", MaxCapturedStdout+1))
	case "fail429":
		input, err := io.ReadAll(os.Stdin)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(os.Stderr, "stdin_len=%d\nHTTP 429 RESOURCE_EXHAUSTED\n", len(input))
		os.Exit(42)
	case "stall429":
		fmt.Fprint(os.Stderr, "HTTP 4")
		fmt.Fprintln(os.Stderr, "29 RESOURCE_EXHAUSTED")
		time.Sleep(20 * time.Second)
	case "parent":
		child := childSpec("grandchild", os.Args[4])
		cmd := exec.Command(child.Command, child.Args...)
		if err := cmd.Run(); err != nil {
			t.Fatal(err)
		}
	case "grandchild":
		signal.Ignore(syscall.SIGTERM)
		file, err := os.Create(os.Args[4])
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		for end := time.Now().Add(12 * time.Second); time.Now().Before(end); {
			if _, err := file.WriteString("heartbeat\n"); err != nil {
				t.Fatal(err)
			}
			time.Sleep(30 * time.Millisecond)
		}
	default:
		t.Fatalf("unknown mode %q", os.Args[3])
	}
	os.Exit(0)
}

func TestProcessCapture(t *testing.T) {
	spec := childSpec("capture")
	spec.Prompt = []byte("日本語 & %PATH%\n")
	t.Setenv("CI", "false")
	result, err := (SystemProcessRunner{}).Run(context.Background(), spec)
	if err != nil || result.ExitCode != 0 || string(result.Stdout) != "true:1:0:1:日本語 & %PATH%\n" || string(result.Tail) != "diagnostic\n" {
		t.Fatalf("capture = %+v, %v", result, err)
	}
	result, err = (SystemProcessRunner{}).Run(context.Background(), childSpec("overflow"))
	if !errors.Is(err, ErrOutputLimit) || len(result.Stdout) != MaxCapturedStdout {
		t.Fatalf("overflow = %d bytes, %v", len(result.Stdout), err)
	}
}

func TestProcessStream(t *testing.T) {
	var output bytes.Buffer
	spec := childSpec("capture")
	spec.Mode, spec.Diagnostic = OutputStream, &output
	result, err := (SystemProcessRunner{}).Run(context.Background(), spec)
	if err != nil || len(result.Stdout) != 0 || !strings.Contains(output.String(), "true:1:0:1:") ||
		!strings.Contains(output.String(), "diagnostic") || string(result.Tail) != output.String() {
		t.Fatalf("stream = %+v, output %q, %v", result, &output, err)
	}
}

func TestProcessCaptureStopsOnOutput(t *testing.T) {
	spec := childSpec("stall429")
	spec.StopOnOutput = func(data []byte) bool { return bytes.Contains(data, []byte("HTTP 429")) }
	result, err := (SystemProcessRunner{}).Run(context.Background(), spec)
	if !errors.Is(err, context.Canceled) || !bytes.Contains(result.Tail, []byte("HTTP 429")) {
		t.Fatalf("capture cancellation: tail %q, error %v", result.Tail, err)
	}
}

func TestProcessCancellationStopsGrandchild(t *testing.T) {
	path := filepath.Join(t.TempDir(), "heartbeat")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := (SystemProcessRunner{}).Run(ctx, childSpec("parent", path))
		done <- err
	}()
	deadline := time.NewTimer(6 * time.Second)
	defer deadline.Stop()
	poll := time.NewTicker(20 * time.Millisecond)
	defer poll.Stop()
ready:
	for {
		select {
		case err := <-done:
			t.Fatalf("parent exited before grandchild was ready: %v", err)
		case <-deadline.C:
			cancel()
			t.Fatalf("grandchild did not start: %v", <-done)
		case <-poll.C:
			if info, err := os.Stat(path); err == nil && info.Size() > 0 {
				break ready
			}
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled process returned %v", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("process tree did not stop")
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(250 * time.Millisecond)
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if before.Size() != after.Size() {
		t.Fatalf("grandchild still wrote after cancellation: %d -> %d", before.Size(), after.Size())
	}
}

func TestProcessNonZeroExitAndNilPrompt(t *testing.T) {
	var diag bytes.Buffer
	spec := childSpec("fail429")
	spec.Mode = OutputStream
	spec.Diagnostic = &diag
	spec.Prompt = nil
	result, err := (SystemProcessRunner{}).Run(context.Background(), spec)
	if err == nil || result.ExitCode != 42 {
		t.Fatalf("fail429 = exit %d, err %v; want exit 42 and error", result.ExitCode, err)
	}
	if !strings.Contains(string(result.Tail), "stdin_len=0") || !strings.Contains(string(result.Tail), "429 RESOURCE_EXHAUSTED") {
		t.Fatalf("fail429 tail = %q", result.Tail)
	}
}

func TestOutputTailBoundariesAndConcurrency(t *testing.T) {
	var tail outputTail
	for i := 1; i <= tailLines+25; i++ {
		_, _ = tail.Write([]byte("line-" + strconv.Itoa(i) + "\n"))
	}
	lines := strings.Split(strings.TrimSuffix(string(tail.lastLines()), "\n"), "\n")
	if len(lines) != tailLines || lines[0] != "line-26" || lines[len(lines)-1] != "line-225" {
		t.Fatalf("tail lines = len %d (%q .. %q)", len(lines), lines[0], lines[len(lines)-1])
	}

	// Wrap past 64 KiB capacity and verify latest content is preserved.
	var wrapped outputTail
	_, _ = wrapped.Write([]byte(strings.Repeat("a", tailCapacity) + "\nfinal-line\n"))
	if got := string(wrapped.lastLines()); !strings.HasSuffix(got, "final-line\n") || len(got) > tailCapacity {
		t.Fatalf("wrapped tail len=%d suffix=%q", len(got), got[max(0, len(got)-20):])
	}

	// Concurrent writes + reads under -race.
	var concurrent outputTail
	var capture stdoutCapture
	var group sync.WaitGroup
	for g := 0; g < 4; g++ {
		group.Add(1)
		go func(id int) {
			defer group.Done()
			payload := []byte("g" + strconv.Itoa(id) + "\n")
			for i := 0; i < 100; i++ {
				_, _ = concurrent.Write(payload)
				_, _ = capture.Write(payload)
			}
		}(g)
	}
	group.Wait()
	if len(concurrent.lastLines()) == 0 {
		t.Fatal("concurrent tail unexpectedly empty")
	}
	if out, overflow := capture.result(); overflow || len(out) != 4*100*3 {
		t.Fatalf("concurrent capture = %d bytes, overflow %t", len(out), overflow)
	}
}
