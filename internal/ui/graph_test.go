package ui

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/withnic/keisou/internal/history"
	"github.com/withnic/keisou/internal/model"
)

func TestRenderGraphsLayout(t *testing.T) {
	now := time.Date(2026, 11, 27, 15, 4, 5, 0, time.UTC)
	trend := make([]float64, 20)
	for i := range trend {
		trend[i] = -1
	}
	trend[17] = 20
	trend[18] = 50
	trend[19] = 80

	summaries := []history.ProviderSummary{
		{
			Provider:    model.ProviderCodex,
			Available:   true,
			HasHistory:  true,
			Trend5h:     trend,
			ETA5h:       history.LimitETA{State: history.ETAHit, Duration: 45 * time.Minute, RatePerHr: 26.5},
			WeeklyETA:   history.LimitETA{State: history.ETAHit, Duration: 28 * time.Hour},
			DailyPeaks:  []float64{-1, 15, 45, 75, 95, 30, 80},
			DailyLabels: []string{"Sat", "Sun", "Mon", "Tue", "Wed", "Thu", "Fri"},
		},
		{
			Provider:    model.ProviderClaude,
			Available:   true,
			HasHistory:  true,
			Trend5h:     trend,
			ETA5h:       history.LimitETA{State: history.ETASafe, RatePerHr: 4.2},
			WeeklyETA:   history.LimitETA{State: history.ETASafe},
			DailyPeaks:  []float64{-1, -1, -1, -1, -1, 10, 20},
			DailyLabels: []string{"Sat", "Sun", "Mon", "Tue", "Wed", "Thu", "Fri"},
		},
		{
			Provider:    model.ProviderGemini,
			Available:   false,
			HasHistory:  false,
			Trend5h:     trend,
			ETA5h:       history.LimitETA{State: history.ETAOffline},
			DailyPeaks:  []float64{-1, -1, -1, -1, -1, -1, -1},
			DailyLabels: []string{"Sat", "Sun", "Mon", "Tue", "Wed", "Thu", "Fri"},
		},
	}

	var buf bytes.Buffer
	if err := RenderGraphs(&buf, summaries, now); err != nil {
		t.Fatalf("RenderGraphs = %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"Updated: 2026-11-27 15:04:05 (7d history)",
		"5h Trend & Limit ETA",
		"7d Daily Peak (5h%):",
		"codex",
		"~45m (+26.5%/h)",
		"~1d4h",
		"95%",
		"claude",
		"safe (+4.2%/h)",
		"safe",
		"20%",
		"Sat", "Fri", "Peak", "Wk ETA",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "gemini") {
		t.Errorf("offline provider without history should be omitted:\n%s", out)
	}
}
