package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRelayCLIValidationAndManualHandoff(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	for _, name := range []string{"CLAUDE", "CODEX", "GEMINI", "ANTIGRAVITY"} {
		t.Setenv("KEISOU_"+name+"_DIR", filepath.Join(dir, strings.ToLower(name)))
	}
	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	// Mutual exclusion and missing input checks for keisou run.
	for _, args := range [][]string{
		{"run"},
		{"run", "   "},
		{"run", "--prompt-file", "", "task"},
		{"run", "--prompt-file", "p.txt", "task"},
		{"run", "--resume", "task"},
		{"run", "--agent", "invalid", "task"},
		{"handoff", "unexpected"},
		{"handoff", "--agent", "auto"},
	} {
		stdout.Reset()
		stderr.Reset()
		if code := run(ctx, append([]string{"--config", cfgPath}, args...), &stdout, &stderr); code != 1 || stdout.Len() != 0 {
			t.Errorf("args %v -> exit %d, stdout %q, stderr %q; want exit 1", args, code, stdout.String(), stderr.String())
		}
	}

	// --threshold alias exceeding hard threshold must return E_CONFIG_INVALID (exit 2).
	for _, args := range [][]string{
		{"run", "--threshold", "99", "task"},
		{"run", "--soft-threshold", "98", "--hard-threshold", "98", "task"},
		{"run", "--mode", "bogus", "task"},
	} {
		stdout.Reset()
		stderr.Reset()
		if code := run(ctx, append([]string{"--config", cfgPath}, args...), &stdout, &stderr); code != 2 || stdout.Len() != 0 {
			t.Errorf("args %v -> exit %d, stdout %q, stderr %q; want exit 2", args, code, stdout.String(), stderr.String())
		}
	}

	// keisou handoff with --agent and --handoff creates a valid file and writes path to stdout.
	workDir := t.TempDir()
	stdout.Reset()
	stderr.Reset()
	handoffFile := "CUSTOM_HANDOFF.md"
	err := relayCommandInDir(ctx, "handoff", []string{"--config", cfgPath, "--agent", "codex", "--handoff", handoffFile, "--task", "Finish CLI verification"}, "", workDir, &stdout, &stderr)
	if err != nil || strings.TrimSpace(stdout.String()) != handoffFile {
		t.Fatalf("handoff err=%v, stdout %q, stderr %q", err, stdout.String(), stderr.String())
	}
	data, err := os.ReadFile(filepath.Join(workDir, handoffFile))
	if err != nil || !strings.Contains(string(data), "Finish CLI verification") || !strings.Contains(string(data), "codex") {
		t.Fatalf("generated handoff = %q, err %v", data, err)
	}
}
