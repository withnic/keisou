package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/withnic/keisou/internal/config"
)

func TestInitProtectsExistingConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	t.Setenv("KEISOU_CONFIG", filepath.Join(dir, "wrong.json"))
	t.Setenv("KEISOU_CLAUDE_DIR", dir)
	if err := os.WriteFile(path, []byte("keep this"), 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	ctx := context.Background()
	args := []string{"--config", path, "init"}
	if code := run(ctx, args, &stdout, &stderr); code != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "--force") {
		t.Fatalf("protected init = %d, stdout %q, stderr %q", code, &stdout, &stderr)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "keep this" {
		t.Fatalf("existing data = %q, %v", data, err)
	}
	stderr.Reset()
	if code := run(ctx, append(args, "--force"), &stdout, &stderr); code != 0 {
		t.Fatalf("forced init = %d: %s", code, &stderr)
	}
	if got, err := config.Load(ctx, path); err != nil || !reflect.DeepEqual(got, config.Defaults()) {
		t.Fatalf("generated config = %+v, %v", got, err)
	}
	for _, want := range []string{"claude CLI:", "codex CLI:", "gemini CLI:", "antigravity CLI:", "claude data: " + dir, "codex data:", "gemini data:", "antigravity data:"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("missing diagnostic %q in %s", want, &stderr)
		}
	}
	if code := run(ctx, []string{"init"}, &stdout, &stderr); code != 0 {
		t.Fatalf("create via KEISOU_CONFIG = %d: %s", code, &stderr)
	}
	subPath := filepath.Join(dir, "sub.json")
	if code := run(ctx, []string{"init", "--config", subPath}, &stdout, &stderr); code != 0 {
		t.Fatalf("init with trailing --config = %d: %s", code, &stderr)
	}
	if _, err := os.Stat(subPath); err != nil {
		t.Fatalf("trailing --config target missing: %v", err)
	}
}

func TestCLIArgErrorsHelpAndInterrupt(t *testing.T) {
	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	if code := run(ctx, []string{"--help"}, &stdout, &stderr); code != 0 || !strings.Contains(stderr.String(), "Commands:") {
		t.Fatalf("--help exit = %d, stderr %q", code, &stderr)
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"help"}, "Commands:"},
		{[]string{"help", "run"}, "Relay Modes (--mode):"},
		{[]string{"help", "handoff"}, "--agent"},
		{[]string{"help", "status"}, "--watch"},
		{[]string{"help", "statusline"}, "keisou-rate-limits.json"},
		{[]string{"help", "init"}, "--force"},
	} {
		stdout.Reset()
		stderr.Reset()
		if code := run(ctx, tc.args, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), tc.want) {
			t.Errorf("%v -> exit %d, stdout %q, stderr %q; want %q", tc.args, code, stdout.String(), stderr.String(), tc.want)
		}
	}
	stdout.Reset()
	stderr.Reset()
	if code := run(ctx, []string{"help", "nope"}, &stdout, &stderr); code != 1 {
		t.Fatalf("help nope exit = %d, want 1", code)
	}
	stderr.Reset()
	if code := run(ctx, []string{"unknown"}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "E_INVALID_ARGS") {
		t.Fatalf("unknown subcommand = %d, stderr %q", code, &stderr)
	}
	stderr.Reset()
	if code := run(ctx, []string{"init", "extra"}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "E_INVALID_ARGS") {
		t.Fatalf("extra arg = %d, stderr %q", code, &stderr)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	stderr.Reset()
	if code := run(canceled, []string{"init", "--config", filepath.Join(t.TempDir(), "c.json")}, &stdout, &stderr); code != 130 || !strings.Contains(stderr.String(), "E_INTERRUPTED") {
		t.Fatalf("canceled run = %d, stderr %q", code, &stderr)
	}
}
