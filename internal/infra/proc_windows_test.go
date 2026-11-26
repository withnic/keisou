package infra

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWindowsBatchCommand(t *testing.T) {
	path := filepath.Join(t.TempDir(), "with spaces.cmd")
	data := "@echo off\r\nif not \"%~1\"==\"hello world\" exit /b 2\r\nif not \"%2\"==\"\"\"\" exit /b 3\r\n<nul set /p =batch-ok\r\nexit /b 0\r\n"
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := (SystemProcessRunner{}).Run(context.Background(), ProcessSpec{
		Command: path, Args: []string{"hello world", ""}, Mode: OutputCapture, Timeout: 10 * time.Second,
	})
	if err != nil || string(result.Stdout) != "batch-ok" {
		t.Fatalf("batch = %q, exit %d, %v; tail %q", result.Stdout, result.ExitCode, err, result.Tail)
	}
	for _, bad := range []string{`a"b`, `%PATH%`, `!VAR!`, "a\rb", "a\nb", "a\x00b", `C:\trailing\`} {
		if _, err := (SystemProcessRunner{}).Run(context.Background(), ProcessSpec{
			Command: path, Args: []string{bad}, Mode: OutputCapture, Timeout: 5 * time.Second,
		}); err == nil {
			t.Errorf("batch accepted unsafe argument %q", bad)
		}
	}
}
