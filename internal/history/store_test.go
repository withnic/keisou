package history

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/withnic/keisou/internal/model"
)

func TestRecordLoadPruneAndThrottle(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := DefaultPath(filepath.Join(dir, "config.json"))
	if filepath.Base(path) != "history.jsonl" {
		t.Fatalf("DefaultPath = %q, want history.jsonl", path)
	}

	base := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	wk := 20.0
	snap := func(pct5h float64, wkPct *float64) []model.UsageSnapshot {
		return []model.UsageSnapshot{
			{
				Provider:        model.ProviderCodex,
				Available:       true,
				Source:          model.SourceLogExact,
				Window5hUsedPct: pct5h,
				WeeklyUsedPct:   wkPct,
			},
		}
	}

	// Missing file loads empty without error.
	entries, err := Load(ctx, path, base)
	if err != nil || len(entries) != 0 {
		t.Fatalf("Load(missing) = %v, %v; want empty, nil", entries, err)
	}

	// First record always writes.
	entries, err = Record(ctx, path, entries, snap(10, &wk), base)
	if err != nil || len(entries) != 1 {
		t.Fatalf("first Record = %d, %v; want 1, nil", len(entries), err)
	}

	// Unchanged within heartbeat window is skipped.
	entries, err = Record(ctx, path, entries, snap(10, &wk), base.Add(2*time.Minute))
	if err != nil || len(entries) != 1 {
		t.Fatalf("unchanged Record = %d, %v; want 1, nil", len(entries), err)
	}

	// Changed after minChangeInterval is recorded.
	entries, err = Record(ctx, path, entries, snap(25, &wk), base.Add(30*time.Second))
	if err != nil || len(entries) != 2 {
		t.Fatalf("changed Record = %d, %v; want 2, nil", len(entries), err)
	}

	// Heartbeat after 5m records even when unchanged.
	entries, err = Record(ctx, path, entries, snap(25, &wk), base.Add(6*time.Minute))
	if err != nil || len(entries) != 3 {
		t.Fatalf("heartbeat Record = %d, %v; want 3, nil", len(entries), err)
	}

	// Reload from disk matches.
	reloaded, err := Load(ctx, path, base.Add(10*time.Minute))
	if err != nil || len(reloaded) != 3 {
		t.Fatalf("Load = %d, %v; want 3, nil", len(reloaded), err)
	}

	// Advance past 7 days: old entries are pruned on Record.
	future := base.Add(8 * 24 * time.Hour)
	pruned, err := Record(ctx, path, reloaded, snap(5, &wk), future)
	if err != nil || len(pruned) != 1 {
		t.Fatalf("pruned Record = %d, %v; want 1, nil", len(pruned), err)
	}
	reloaded, err = Load(ctx, path, future)
	if err != nil || len(reloaded) != 1 || !reloaded[0].At.Equal(future) {
		t.Fatalf("reloaded pruned = %+v, %v", reloaded, err)
	}

	// Corrupt line is ignored gracefully.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{broken json\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	reloaded, err = Load(ctx, path, future.Add(time.Minute))
	if err != nil || len(reloaded) != 1 {
		t.Fatalf("Load with corrupt line = %d, %v; want 1, nil", len(reloaded), err)
	}
}

func TestSummarizeETAAndWeeklyPeaks(t *testing.T) {
	jst := time.FixedZone("JST", 9*3600)
	now := time.Date(2026, 11, 27, 15, 0, 0, 0, jst)
	resetFar := now.Add(3 * time.Hour)
	resetSoon := now.Add(15 * time.Minute)
	wkResetFar := now.Add(5 * 24 * time.Hour)
	wkResetSoon := now.Add(6 * time.Hour)

	wk40 := 40.0
	wk60 := 60.0
	wk20 := 20.0
	wk22 := 22.0

	entries := []Entry{
		// Two days ago peak for codex (85%).
		{
			At: now.AddDate(0, 0, -2).UTC(),
			Providers: []ProviderSample{
				{Provider: model.ProviderCodex, Available: true, Window5hPct: 85, WeeklyPct: &wk40, WeeklyReset: &wkResetFar},
			},
		},
		// Previous cycle before reset drop should not skew current 5h pace.
		{
			At: now.Add(-50 * time.Minute).UTC(),
			Providers: []ProviderSample{
				{Provider: model.ProviderCodex, Available: true, Window5hPct: 90, Window5hReset: &resetFar},
			},
		},
		// Current cycle: 40% -> 70% over 30m (+60%/h => ~30m to 100%, before 3h reset => ETAHit).
		{
			At: now.Add(-30 * time.Minute).UTC(),
			Providers: []ProviderSample{
				{Provider: model.ProviderCodex, Available: true, Window5hPct: 40, Window5hReset: &resetFar, WeeklyPct: &wk40, WeeklyReset: &wkResetFar},
				{Provider: model.ProviderClaude, Available: true, Window5hPct: 40, Window5hReset: &resetSoon, WeeklyPct: &wk20, WeeklyReset: &wkResetSoon},
			},
		},
	}

	current := []model.UsageSnapshot{
		{
			Provider:        model.ProviderCodex,
			Available:       true,
			Source:          model.SourceLogExact,
			Window5hUsedPct: 70,
			Window5hResetAt: &resetFar,
			WeeklyUsedPct:   &wk60,
			WeeklyResetAt:   &wkResetFar,
		},
		{
			Provider:        model.ProviderClaude,
			Available:       true,
			Source:          model.SourceLogExact,
			Window5hUsedPct: 50, // +20%/h => 2.5h to 100%, resets in 15m => ETASafe.
			Window5hResetAt: &resetSoon,
			WeeklyUsedPct:   &wk22, // +2% in 30m => ~19.5h to 100%, resets in 6h => ETASafe.
			WeeklyResetAt:   &wkResetSoon,
		},
		{
			Provider:        model.ProviderGemini,
			Available:       false,
			Window5hUsedPct: math.NaN(),
		},
	}

	summaries := Summarize(entries, current, now)
	if len(summaries) != 3 {
		t.Fatalf("len(summaries) = %d, want 3", len(summaries))
	}

	codex := summaries[0]
	if codex.ETA5h.State != ETAHit {
		t.Fatalf("codex ETA5h = %+v, want ETAHit", codex.ETA5h)
	}
	if math.Abs(codex.ETA5h.RatePerHr-60.0) > 0.5 || absDuration(codex.ETA5h.Duration-30*time.Minute) > time.Minute {
		t.Fatalf("codex ETA5h pace/dur = %+v, want ~60%%/h and ~30m", codex.ETA5h)
	}
	if codex.WeeklyETA.State != ETAHit {
		t.Fatalf("codex WeeklyETA = %+v, want ETAHit", codex.WeeklyETA)
	}
	if len(codex.DailyPeaks) != 7 || codex.DailyPeaks[4] != 85 || codex.DailyPeaks[6] != 90 {
		t.Fatalf("codex DailyPeaks = %v, want day[4]=85 and day[6]=90", codex.DailyPeaks)
	}
	if len(codex.Trend5h) != trendBuckets || codex.Trend5h[trendBuckets-1] != 70 {
		t.Fatalf("codex Trend5h last = %v, want 70", codex.Trend5h)
	}

	claude := summaries[1]
	if claude.ETA5h.State != ETASafe {
		t.Fatalf("claude ETA5h = %+v, want ETASafe (resets before hitting limit)", claude.ETA5h)
	}
	if claude.WeeklyETA.State != ETASafe {
		t.Fatalf("claude WeeklyETA = %+v, want ETASafe", claude.WeeklyETA)
	}

	gemini := summaries[2]
	if gemini.ETA5h.State != ETAOffline {
		t.Fatalf("gemini ETA5h = %+v, want ETAOffline", gemini.ETA5h)
	}
}
