package ui

import (
	"bytes"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/withnic/keisou/internal/model"
)

func TestRenderTableLayoutAndEdgeCases(t *testing.T) {
	now := time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC)
	future := now.Add(2*time.Hour + 15*time.Minute + 5*time.Second)
	weeklyFuture := now.Add(42 * time.Hour)
	subSecond := now.Add(500 * time.Millisecond)
	past := now.Add(-time.Minute)
	weekly := 42.5
	weeklyOver := 150.0

	snapshots := []model.UsageSnapshot{
		{
			Provider:        model.ProviderClaude,
			PlanName:        "Claude Pro",
			Available:       true,
			Source:          model.SourceLogExact,
			Window5hUsedPct: 95.4,
			Window5hResetAt: &future,
			WeeklyUsedPct:   &weekly,
			ActiveModel:     "claude-opus",
		},
		{
			Provider:        model.ProviderCodex,
			PlanName:        "ChatGPT Plus",
			Available:       false,
			Source:          model.SourceWindowCalc,
			Window5hUsedPct: math.NaN(),
			Window5hResetAt: &subSecond,
			WeeklyUsedPct:   &weeklyOver,
			ActiveModel:     "",
			IsFallbackModel: true,
		},
		{
			Provider:        model.ProviderGemini,
			PlanName:        "AI Pro",
			Available:       true,
			Source:          model.SourceWindowCalc,
			Window5hUsedPct: 100,
			Window5hResetAt: &past,
			WeeklyResetAt:   &weeklyFuture,
			ActiveModel:     "gemini-3-flash",
			IsFallbackModel: true,
		},
	}

	var buf bytes.Buffer
	if err := RenderTable(&buf, snapshots, now); err != nil {
		t.Fatalf("RenderTable = %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"claude", "codex", "gemini",
		"Claude Pro", "ChatGPT Plus", "AI Pro",
		"[#########-] 95.4%",
		"[----------] 0.0% (offline)",
		"[##########] 100.0% (est)",
		"2h15m5s", "<1s",
		"42.5%", "100.0% (est)", "- (reset 1d18h)",
		"claude-opus", "fallback", "gemini-3-flash (fallback)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "95.4% (est)") {
		t.Errorf("log_exact snapshot should not have (est) suffix:\n%s", out)
	}
	if lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n"); len(lines) != 6 {
		t.Fatalf("row count = %d, want 6:\n%s", len(lines), out)
	}
}

func TestRenderTableSanitizesControlSequences(t *testing.T) {
	now := time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC)
	snapshots := []model.UsageSnapshot{
		{
			Provider:    model.ProviderID("claude\x1b[31m"),
			PlanName:    "Plan\tName\r\nInjected",
			Available:   true,
			ActiveModel: "model\u009b31m\x07safe",
		},
	}
	var buf bytes.Buffer
	if err := RenderTable(&buf, snapshots, now); err != nil {
		t.Fatalf("RenderTable = %v", err)
	}
	out := buf.String()
	for _, forbidden := range []string{"\x1b", "\u009b", "\x07", "\r"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("output retained control sequence %q: %q", forbidden, out)
		}
	}
	if !strings.Contains(out, "Plan Name Injected") {
		t.Errorf("expected sanitized plan name in %q", out)
	}
}
