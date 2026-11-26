package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/withnic/keisou/internal/config"
	"github.com/withnic/keisou/internal/model"
	"github.com/withnic/keisou/internal/provider"
)

type fixedClock struct{ now time.Time }

func (c fixedClock) Now() time.Time                             { return c.now }
func (c fixedClock) Sleep(context.Context, time.Duration) error { return nil }

func TestStatuslineCachesOfficialClaudeRateLimits(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	claudeDir := t.TempDir()
	t.Setenv("KEISOU_CLAUDE_DIR", claudeDir)
	cfgPath := filepath.Join(t.TempDir(), "config.json")

	payload := `{"model":{"id":"claude-opus-4-6","display_name":"Claude Opus"},"rate_limits":{"five_hour":{"used_percentage":42.3,"resets_at":1795700000},"seven_day":{"used_percentage":85.7,"resets_at":1796200000}}}`
	var stdout, stderr bytes.Buffer
	err := statuslineCommand(context.Background(), []string{"--config", cfgPath, "--agent", "claude"}, "", strings.NewReader(payload), &stdout, &stderr, fixedClock{now: now})
	if err != nil {
		t.Fatalf("statuslineCommand = %v, stderr=%s", err, &stderr)
	}
	if got := strings.TrimSpace(stdout.String()); got != "5h: 42.3% | 7d: 85.7%" {
		t.Fatalf("statusline stdout = %q, want %q", got, "5h: 42.3% | 7d: 85.7%")
	}

	// Verify Collector picks up the cached official rate_limits as log_exact.
	cfg := config.Defaults()
	adapter, ok := provider.NewCollector(cfg, func(cmd string) (string, error) { return cmd, nil }).Adapter(model.ProviderClaude)
	if !ok {
		t.Fatal("missing claude adapter")
	}
	snap := adapter.FetchUsage(context.Background(), now)
	if snap.Source != model.SourceLogExact || snap.Window5hUsedPct != 42.3 || snap.Window5hResetAt == nil ||
		snap.WeeklyUsedPct == nil || *snap.WeeklyUsedPct != 85.7 || snap.WeeklyResetAt == nil || snap.ActiveModel != "claude-opus-4-6" {
		t.Fatalf("cached claude snapshot = %+v", snap)
	}

	// --quiet writes cache without printing to stdout.
	stdout.Reset()
	payloadQuiet := `{"rate_limits":{"five_hour":{"used_percentage":50.0,"resets_at":1795700000}}}`
	if err := statuslineCommand(context.Background(), []string{"--config", cfgPath, "--agent", "claude", "--quiet"}, "", strings.NewReader(payloadQuiet), &stdout, &stderr, fixedClock{now: now.Add(time.Minute)}); err != nil {
		t.Fatalf("quiet statusline = %v", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("quiet statusline wrote %q", stdout.String())
	}
	snap = adapter.FetchUsage(context.Background(), now.Add(time.Minute))
	if snap.Window5hUsedPct != 50.0 {
		t.Fatalf("updated claude snapshot = %+v, want 50%%", snap)
	}

	// Missing rate_limits, empty stdin, or invalid JSON must not fail noisily.
	for _, input := range []string{"", "{}", `{"rate_limits":null}`, "{not-json"} {
		stdout.Reset()
		if err := statuslineCommand(context.Background(), []string{"--config", cfgPath}, "", strings.NewReader(input), &stdout, &stderr, fixedClock{now: now}); err != nil {
			t.Errorf("input %q unexpectedly failed: %v", input, err)
		}
	}

	// Invalid agent or extra argument returns E_INVALID_ARGS.
	if err := statuslineCommand(context.Background(), []string{"--agent", "codex"}, "", strings.NewReader("{}"), &stdout, &stderr, fixedClock{now: now}); model.ExitCode(err) != 1 {
		t.Fatalf("unsupported agent err = %v, want exit 1", err)
	}
	if err := statuslineCommand(context.Background(), []string{"extra"}, "", strings.NewReader("{}"), &stdout, &stderr, fixedClock{now: now}); model.ExitCode(err) != 1 {
		t.Fatalf("extra arg err = %v, want exit 1", err)
	}
	if _, err := os.Stat(filepath.Join(claudeDir, "keisou-rate-limits.json")); err != nil {
		t.Fatalf("missing cache file: %v", err)
	}
}
