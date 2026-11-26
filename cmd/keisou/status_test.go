package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/withnic/keisou/internal/model"
)

func TestStatusFormats(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("KEISOU_CONFIG", filepath.Join(t.TempDir(), "unused.json"))
	missing := filepath.Join(t.TempDir(), "no-logs")
	for _, name := range []string{"CLAUDE", "CODEX", "GEMINI", "ANTIGRAVITY"} {
		t.Setenv("KEISOU_"+name+"_DIR", missing)
	}
	var out, diagnostics bytes.Buffer
	if code := run(context.Background(), []string{"--config", path, "init"}, &out, &diagnostics); code != 0 {
		t.Fatalf("init exit %d: %s", code, &diagnostics)
	}
	out.Reset()
	diagnostics.Reset()
	if code := run(context.Background(), []string{"--config", path, "status", "--json"}, &out, &diagnostics); code != 0 {
		t.Fatalf("status exit %d: %s", code, &diagnostics)
	}
	assertStatusJSON(t, out.Bytes())
	out.Reset()
	if code := run(context.Background(), []string{"status", "--config", path}, &out, &diagnostics); code != 0 {
		t.Fatalf("status exit %d: %s", code, &diagnostics)
	}
	for _, label := range []string{"Plan", "5h Used", "Reset In", "Weekly", "Model", "[", "%"} {
		if !strings.Contains(out.String(), label) {
			t.Errorf("missing %q in %s", label, &out)
		}
	}
}

func assertStatusJSON(t *testing.T, data []byte) {
	t.Helper()
	var snapshots []model.UsageSnapshot
	if err := json.Unmarshal(data, &snapshots); err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 4 || snapshots[0].Provider != model.ProviderClaude ||
		snapshots[1].Provider != model.ProviderCodex || snapshots[2].Provider != model.ProviderGemini ||
		snapshots[3].Provider != model.ProviderAntigravity {
		t.Fatalf("unexpected snapshots: %+v", snapshots)
	}
}

type watchClock struct {
	now    time.Time
	delays []time.Duration
	cancel context.CancelFunc
}

func (c *watchClock) Now() time.Time { return c.now }

func (c *watchClock) Sleep(ctx context.Context, delay time.Duration) error {
	c.delays = append(c.delays, delay)
	c.now = c.now.Add(delay)
	if len(c.delays) == 2 {
		c.cancel()
	}
	return ctx.Err()
}

func TestStatusWatch(t *testing.T) {
	for _, machine := range []bool{true, false} {
		args := []string{"--watch", "--interval", "2"}
		if machine {
			args = append(args, "--json")
		}
		ctx, cancel := context.WithCancel(context.Background())
		clock := &watchClock{now: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), cancel: cancel}
		var out, diagnostics bytes.Buffer
		path := filepath.Join(t.TempDir(), "config.json")
		missing := filepath.Join(t.TempDir(), "no-logs")
		for _, name := range []string{"CLAUDE", "CODEX", "GEMINI"} {
			t.Setenv("KEISOU_"+name+"_DIR", missing)
		}
		err := status(ctx, args, path, &out, &diagnostics, clock)
		cancel()
		if err != context.Canceled || len(clock.delays) != 2 || clock.delays[0] != 2*time.Second {
			t.Fatalf("unexpected watch result: delays=%v err=%v stderr=%s", clock.delays, err, &diagnostics)
		}
		if machine {
			lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
			if len(lines) != 2 {
				t.Fatalf("expected two NDJSON lines: %s", &out)
			}
			for _, line := range lines {
				assertStatusJSON(t, []byte(line))
			}
		} else if strings.Count(out.String(), "\x1b[2J\x1b[H") != 2 {
			t.Fatalf("expected two redraws: %s", &out)
		}
	}
}

func TestStatusRejectsInvalidInterval(t *testing.T) {
	for _, interval := range []string{"0", "-1", "9223372037", "NaN"} {
		var out, diagnostics bytes.Buffer
		code := run(context.Background(), []string{"status", "--watch", "--interval", interval}, &out, &diagnostics)
		if code != 1 || out.Len() != 0 || !strings.Contains(diagnostics.String(), "interval") {
			t.Fatalf("interval %q got exit %d, stdout %q, stderr %q", interval, code, out.String(), diagnostics.String())
		}
	}
	var out, diagnostics bytes.Buffer
	if code := run(context.Background(), []string{"status", "extra"}, &out, &diagnostics); code != 1 || out.Len() != 0 {
		t.Fatalf("extra arg got exit %d, stdout %q, stderr %q", code, out.String(), diagnostics.String())
	}
	badConfig := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(badConfig, []byte("{bad"), 0600); err != nil {
		t.Fatal(err)
	}
	diagnostics.Reset()
	if code := run(context.Background(), []string{"status", "--config", badConfig}, &out, &diagnostics); code != 2 || out.Len() != 0 {
		t.Fatalf("bad config got exit %d, stdout %q, stderr %q", code, out.String(), diagnostics.String())
	}
}

func TestStatusGraphAndHistoryPersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	codexDir := filepath.Join(dir, "codex")
	sessionDir := filepath.Join(codexDir, "sessions", "2026", "11", "27")
	if err := os.MkdirAll(sessionDir, 0755); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "no-logs")
	t.Setenv("KEISOU_CLAUDE_DIR", missing)
	t.Setenv("KEISOU_CODEX_DIR", codexDir)
	t.Setenv("KEISOU_GEMINI_DIR", missing)
	t.Setenv("KEISOU_ANTIGRAVITY_DIR", missing)

	var out, diagnostics bytes.Buffer
	if code := run(context.Background(), []string{"--config", path, "init"}, &out, &diagnostics); code != 0 {
		t.Fatalf("init exit %d: %s", code, &diagnostics)
	}
	out.Reset()
	diagnostics.Reset()

	now := time.Now().UTC().Truncate(time.Second)
	resetAt := now.Add(3 * time.Hour).Unix()
	session := filepath.Join(sessionDir, "rollout.jsonl")
	line := `{"timestamp":"` + now.Format(time.RFC3339) + `","type":"event_msg","payload":{"type":"token_count","rate_limits":{"primary":{"used_percent":42.5,"resets_at":` + jsonInt(resetAt) + `}}}}` + "\n"
	if err := os.WriteFile(session, []byte(line), 0600); err != nil {
		t.Fatal(err)
	}

	// --graph renders table + trend/7d sections and persists history.jsonl alongside config.json.
	if code := run(context.Background(), []string{"status", "--config", path, "--graph"}, &out, &diagnostics); code != 0 {
		t.Fatalf("status --graph exit %d: %s", code, &diagnostics)
	}
	if _, err := os.Stat(filepath.Join(dir, "history.jsonl")); err != nil {
		t.Fatalf("expected history.jsonl beside config.json: %v", err)
	}
	for _, want := range []string{"5h Used", "5h Trend & Limit ETA", "7d Daily Peak (5h%):", "codex", "42%"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q in graph output:\n%s", want, &out)
		}
	}
}

func jsonInt(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}
