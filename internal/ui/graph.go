package ui

import (
	"fmt"
	"io"
	"math"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/withnic/keisou/internal/history"
	"github.com/withnic/keisou/internal/model"
)

var sparkBlocks = []rune{'▁', '▂', '▃', '▄', '▅', '▆', '▇', '█'}

// RenderGraphs writes the 5h trend sparkline, limit ETA, and 7-day daily peak summary.
func RenderGraphs(w io.Writer, summaries []history.ProviderSummary, now time.Time) error {
	visible := make([]history.ProviderSummary, 0, len(summaries))
	for _, s := range summaries {
		if s.Available || s.HasHistory {
			visible = append(visible, s)
		}
	}
	if len(visible) == 0 {
		return nil
	}

	if _, err := fmt.Fprintf(w, "\nUpdated: %s (7d history)\n\n", now.Format("2006-01-02 15:04:05")); err != nil {
		return err
	}

	if _, err := fmt.Fprintln(w, "5h Trend & Limit ETA (last 5h, 15m/col):"); err != nil {
		return err
	}
	trendTable := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	for _, s := range visible {
		name := tableValue(string(s.Provider))
		spark := formatSparkline(s.Trend5h)
		eta := format5hETA(s.ETA5h)
		if _, err := fmt.Fprintf(trendTable, "  %s\t%s\t%s\n", name, spark, eta); err != nil {
			return err
		}
	}
	if err := trendTable.Flush(); err != nil {
		return err
	}

	if _, err := fmt.Fprintln(w, "\n7d Daily Peak (5h%):"); err != nil {
		return err
	}
	weekTable := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	labels := visible[0].DailyLabels
	if len(labels) == 7 {
		header := make([]string, 0, 10)
		header = append(header, "  ")
		header = append(header, labels...)
		header = append(header, "Peak", "Wk ETA")
		if _, err := fmt.Fprintln(weekTable, strings.Join(header, "\t")); err != nil {
			return err
		}
	}
	for _, s := range visible {
		cols := make([]string, 0, 10)
		cols = append(cols, "  "+tableValue(string(s.Provider)))
		maxPeak := -1.0
		for _, peak := range s.DailyPeaks {
			cols = append(cols, string(sparkRune(peak)))
			if peak > maxPeak {
				maxPeak = peak
			}
		}
		if maxPeak < 0 {
			cols = append(cols, "-")
		} else {
			cols = append(cols, fmt.Sprintf("%.0f%%", model.ClampPercent(maxPeak)))
		}
		cols = append(cols, formatWeeklyETA(s.WeeklyETA))
		if _, err := fmt.Fprintln(weekTable, strings.Join(cols, "\t")); err != nil {
			return err
		}
	}
	return weekTable.Flush()
}

func formatSparkline(values []float64) string {
	var b strings.Builder
	b.Grow(len(values) * 3)
	for _, v := range values {
		b.WriteRune(sparkRune(v))
	}
	return b.String()
}

func sparkRune(pct float64) rune {
	if pct < 0 {
		return '·'
	}
	clamped := model.ClampPercent(pct)
	if clamped <= 0 {
		return sparkBlocks[0]
	}
	idx := int(math.Round(clamped / 100.0 * float64(len(sparkBlocks)-1)))
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sparkBlocks) {
		idx = len(sparkBlocks) - 1
	}
	return sparkBlocks[idx]
}

func format5hETA(eta history.LimitETA) string {
	switch eta.State {
	case history.ETALimit:
		return "LIMIT"
	case history.ETAOffline:
		return "offline"
	case history.ETAIdle:
		return "idle"
	case history.ETASafe:
		if eta.RatePerHr > 0 {
			return fmt.Sprintf("safe (+%.1f%%/h)", eta.RatePerHr)
		}
		return "safe"
	case history.ETAHit:
		return fmt.Sprintf("~%s (+%.1f%%/h)", formatETADuration(eta.Duration), eta.RatePerHr)
	default:
		return "-"
	}
}

func formatWeeklyETA(eta history.LimitETA) string {
	switch eta.State {
	case history.ETALimit:
		return "LIMIT"
	case history.ETAOffline:
		return "offline"
	case history.ETAIdle:
		return "idle"
	case history.ETASafe:
		return "safe"
	case history.ETAHit:
		return "~" + formatETADuration(eta.Duration)
	default:
		return "-"
	}
}

func formatETADuration(d time.Duration) string {
	if d < time.Minute {
		return "<1m"
	}
	if d >= 24*time.Hour {
		days := d / (24 * time.Hour)
		hours := (d % (24 * time.Hour)) / time.Hour
		return fmt.Sprintf("%dd%dh", days, hours)
	}
	hours := d / time.Hour
	mins := (d % time.Hour) / time.Minute
	if hours > 0 {
		return fmt.Sprintf("%dh%dm", hours, mins)
	}
	return fmt.Sprintf("%dm", mins)
}
