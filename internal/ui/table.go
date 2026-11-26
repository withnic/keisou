package ui

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"github.com/withnic/keisou/internal/model"
)

// RenderTable places each provider in its own column.
func RenderTable(w io.Writer, snapshots []model.UsageSnapshot, now time.Time) error {
	table := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	labels := []string{"", "Plan", "5h Used", "Reset In", "Weekly", "Model"}
	rows := make([][]string, len(labels))
	for i, label := range labels {
		rows[i] = []string{label}
	}
	for _, s := range snapshots {
		weekly := "-"
		if s.WeeklyUsedPct != nil {
			weekly = fmt.Sprintf("%.1f%%", model.ClampPercent(*s.WeeklyUsedPct))
			if s.Source != model.SourceLogExact {
				if reset := resetIn(s.WeeklyResetAt, now); reset != "-" {
					weekly += fmt.Sprintf(" (est, %s)", reset)
				} else {
					weekly += " (est)"
				}
			}
		} else if reset := resetIn(s.WeeklyResetAt, now); reset != "-" {
			weekly = fmt.Sprintf("- (reset %s)", reset)
		}
		name := tableValue(s.ActiveModel)
		if s.IsFallbackModel {
			if name == "-" {
				name = "fallback"
			} else {
				name += " (fallback)"
			}
		}
		used := progress(s.Window5hUsedPct)
		if !s.Available {
			used += " (offline)"
		} else if s.Source == model.SourceWindowCalc {
			used += " (est)"
		}
		cells := []string{tableValue(string(s.Provider)), tableValue(s.PlanName), used, resetIn(s.Window5hResetAt, now), weekly, name}
		for i, cell := range cells {
			rows[i] = append(rows[i], cell)
		}
	}
	for _, row := range rows {
		if _, err := fmt.Fprintln(table, strings.Join(row, "\t")); err != nil {
			return err
		}
	}
	return table.Flush()
}

func progress(value float64) string {
	const width = 10
	pct := model.ClampPercent(value)
	filled := int(pct * width / 100)
	return fmt.Sprintf("[%s%s] %.1f%%", strings.Repeat("#", filled), strings.Repeat("-", width-filled), pct)
}

func resetIn(reset *time.Time, now time.Time) string {
	if reset == nil || !reset.After(now) {
		return "-"
	}
	delay := reset.Sub(now)
	if delay < time.Second {
		return "<1s"
	}
	if delay >= 24*time.Hour {
		days := delay / (24 * time.Hour)
		hours := (delay % (24 * time.Hour)) / time.Hour
		return fmt.Sprintf("%dd%dh", days, hours)
	}
	return delay.Truncate(time.Second).String()
}

func tableValue(value string) string {
	value = strings.Join(strings.FieldsFunc(value, unicode.IsControl), " ")
	if value == "" {
		return "-"
	}
	return value
}
