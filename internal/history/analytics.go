package history

import (
	"math"
	"time"

	"github.com/withnic/keisou/internal/model"
)

// ETAState describes the limit projection status for a quota window.
type ETAState string

const (
	ETAUnknown ETAState = "unknown"
	ETAOffline ETAState = "offline"
	ETALimit   ETAState = "limit"
	ETAIdle    ETAState = "idle"
	ETASafe    ETAState = "safe" // Current pace will not reach 100% before reset.
	ETAHit     ETAState = "hit"  // Current pace will reach 100% in Duration.
)

// LimitETA holds the projected time to 100% usage and observed burn rate.
type LimitETA struct {
	State      ETAState
	Duration   time.Duration
	RatePerHr  float64 // Percentage points per hour (for 5h) or per day when weekly.
	RatePerDay float64
}

// ProviderSummary aggregates 5h trend, limit ETAs, and 7-day daily peaks for one provider.
type ProviderSummary struct {
	Provider      model.ProviderID
	Available     bool
	Source        model.QuotaSource
	HasHistory    bool
	Trend5h       []float64 // Len == trendBuckets; negative means no observation in bucket.
	ETA5h         LimitETA
	WeeklyETA     LimitETA
	DailyPeaks    []float64 // Len == 7 (oldest day -> today); negative means no data.
	DailyLabels   []string  // Len == 7 (e.g. "Sat", "Sun", ..., "Fri").
	WeeklyResetAt *time.Time
}

type point struct {
	at    time.Time
	pct   float64
	reset *time.Time
}

// Summarize builds 5h trend buckets, 5h/weekly ETAs, and 7-day daily peaks per provider.
func Summarize(entries []Entry, current []model.UsageSnapshot, now time.Time) []ProviderSummary {
	loc := now.Location()
	todayStart := startOfDay(now.In(loc))
	dayLabels := make([]string, 7)
	for i := range 7 {
		day := todayStart.AddDate(0, 0, i-6)
		dayLabels[i] = day.Format("Mon")
	}

	// Build timeline including the live current snapshot as the newest observation.
	timeline := make([]Entry, 0, len(entries)+1)
	timeline = append(timeline, entries...)
	if len(current) > 0 {
		live := FromSnapshots(now, current)
		if len(timeline) == 0 || !timeline[len(timeline)-1].At.Equal(live.At) {
			timeline = append(timeline, live)
		}
	}

	summaries := make([]ProviderSummary, 0, len(current))
	for _, snap := range current {
		pts5h := make([]point, 0, len(timeline))
		ptsWk := make([]point, 0, len(timeline))
		for _, e := range timeline {
			for _, p := range e.Providers {
				if p.Provider != snap.Provider {
					continue
				}
				if p.Available || p.Window5hPct > 0 {
					pts5h = append(pts5h, point{
						at:    e.At,
						pct:   model.ClampPercent(p.Window5hPct),
						reset: p.Window5hReset,
					})
				}
				if p.WeeklyPct != nil {
					ptsWk = append(ptsWk, point{
						at:    e.At,
						pct:   model.ClampPercent(*p.WeeklyPct),
						reset: p.WeeklyReset,
					})
				}
			}
		}

		s := ProviderSummary{
			Provider:      snap.Provider,
			Available:     snap.Available,
			Source:        snap.Source,
			HasHistory:    len(entries) > 0 && hasProviderInEntries(entries, snap.Provider),
			Trend5h:       bucket5h(pts5h, now),
			ETA5h:         estimate5hETA(snap, pts5h, now),
			WeeklyETA:     estimateWeeklyETA(snap, ptsWk, now),
			DailyPeaks:    dailyPeaks(pts5h, todayStart, loc),
			DailyLabels:   dayLabels,
			WeeklyResetAt: cloneTime(snap.WeeklyResetAt),
		}
		summaries = append(summaries, s)
	}
	return summaries
}

func hasProviderInEntries(entries []Entry, id model.ProviderID) bool {
	for _, e := range entries {
		for _, p := range e.Providers {
			if p.Provider == id && (p.Available || p.Window5hPct > 0 || p.WeeklyPct != nil) {
				return true
			}
		}
	}
	return false
}

func startOfDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

func bucket5h(points []point, now time.Time) []float64 {
	buckets := make([]float64, trendBuckets)
	for i := range buckets {
		buckets[i] = -1
	}
	windowStart := now.Add(-trendWindow)
	bucketWidth := trendWindow / trendBuckets
	for _, pt := range points {
		if pt.at.Before(windowStart) || pt.at.After(now) {
			continue
		}
		idx := int(pt.at.Sub(windowStart) / bucketWidth)
		if idx >= trendBuckets {
			idx = trendBuckets - 1
		}
		if idx < 0 {
			continue
		}
		// Keep latest observation in each bucket (points are ordered oldest->newest).
		buckets[idx] = pt.pct
	}
	return buckets
}

func dailyPeaks(points []point, todayStart time.Time, loc *time.Location) []float64 {
	peaks := make([]float64, 7)
	for i := range peaks {
		peaks[i] = -1
	}
	firstDay := todayStart.AddDate(0, 0, -6)
	for _, pt := range points {
		localDay := startOfDay(pt.at.In(loc))
		if localDay.Before(firstDay) {
			continue
		}
		// Compute calendar day offset safely across DST transitions.
		offset := calendarDayDiff(firstDay, localDay)
		if offset < 0 || offset >= 7 {
			continue
		}
		if pt.pct > peaks[offset] {
			peaks[offset] = pt.pct
		}
	}
	return peaks
}

func calendarDayDiff(from, to time.Time) int {
	fromUTC := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, time.UTC)
	toUTC := time.Date(to.Year(), to.Month(), to.Day(), 0, 0, 0, 0, time.UTC)
	return int(toUTC.Sub(fromUTC) / (24 * time.Hour))
}

func estimate5hETA(snap model.UsageSnapshot, points []point, now time.Time) LimitETA {
	if !snap.Available {
		return LimitETA{State: ETAOffline}
	}
	cur := model.ClampPercent(snap.Window5hUsedPct)
	if cur >= 100 {
		return LimitETA{State: ETALimit}
	}
	cycle := activeCyclePoints(points, now.Add(-paceLookback), now, snap.Window5hResetAt)
	if len(cycle) < 2 {
		cycle = activeCyclePoints(points, now.Add(-trendWindow), now, snap.Window5hResetAt)
	}
	if len(cycle) < 2 {
		return LimitETA{State: ETAUnknown}
	}
	first, last := cycle[0], cycle[len(cycle)-1]
	span := last.at.Sub(first.at)
	if span < minPaceSpan {
		return LimitETA{State: ETAUnknown}
	}
	delta := last.pct - first.pct
	hours := span.Hours()
	if hours <= 0 || math.IsNaN(hours) || math.IsInf(hours, 0) {
		return LimitETA{State: ETAUnknown}
	}
	ratePerHr := delta / hours
	if math.IsNaN(ratePerHr) || math.IsInf(ratePerHr, 0) || ratePerHr <= 0.05 {
		return LimitETA{State: ETAIdle}
	}
	remaining := 100.0 - cur
	etaHours := remaining / ratePerHr
	if math.IsNaN(etaHours) || math.IsInf(etaHours, 0) || etaHours <= 0 {
		return LimitETA{State: ETAUnknown}
	}
	const maxETAHours = 7 * 24.0
	if etaHours > maxETAHours {
		return LimitETA{State: ETASafe, RatePerHr: ratePerHr}
	}
	eta := time.Duration(etaHours * float64(time.Hour))
	if snap.Window5hResetAt != nil && snap.Window5hResetAt.After(now) && now.Add(eta).After(*snap.Window5hResetAt) {
		return LimitETA{State: ETASafe, Duration: eta, RatePerHr: ratePerHr}
	}
	return LimitETA{State: ETAHit, Duration: eta, RatePerHr: ratePerHr}
}

func estimateWeeklyETA(snap model.UsageSnapshot, points []point, now time.Time) LimitETA {
	if !snap.Available {
		return LimitETA{State: ETAOffline}
	}
	if snap.WeeklyUsedPct == nil {
		return LimitETA{State: ETAUnknown}
	}
	cur := model.ClampPercent(*snap.WeeklyUsedPct)
	if cur >= 100 {
		return LimitETA{State: ETALimit}
	}
	cycle := activeCyclePoints(points, now.Add(-Retention), now, snap.WeeklyResetAt)
	if len(cycle) < 2 {
		return LimitETA{State: ETAUnknown}
	}
	first, last := cycle[0], cycle[len(cycle)-1]
	span := last.at.Sub(first.at)
	if span < minWeeklyPaceSpan {
		return LimitETA{State: ETAUnknown}
	}
	delta := last.pct - first.pct
	days := span.Hours() / 24.0
	if days <= 0 || math.IsNaN(days) || math.IsInf(days, 0) {
		return LimitETA{State: ETAUnknown}
	}
	ratePerDay := delta / days
	if math.IsNaN(ratePerDay) || math.IsInf(ratePerDay, 0) || ratePerDay <= 0.1 {
		return LimitETA{State: ETAIdle}
	}
	remaining := 100.0 - cur
	etaDays := remaining / ratePerDay
	if math.IsNaN(etaDays) || math.IsInf(etaDays, 0) || etaDays <= 0 {
		return LimitETA{State: ETAUnknown}
	}
	const maxETADays = 30.0
	if etaDays > maxETADays {
		return LimitETA{State: ETASafe, RatePerDay: ratePerDay}
	}
	eta := time.Duration(etaDays * float64(24*time.Hour))
	if snap.WeeklyResetAt != nil && snap.WeeklyResetAt.After(now) && now.Add(eta).After(*snap.WeeklyResetAt) {
		return LimitETA{State: ETASafe, Duration: eta, RatePerDay: ratePerDay}
	}
	return LimitETA{State: ETAHit, Duration: eta, RatePerDay: ratePerDay}
}

// activeCyclePoints returns points within [since, now] after the latest reset boundary.
func activeCyclePoints(points []point, since, now time.Time, currentReset *time.Time) []point {
	var filtered []point
	for _, pt := range points {
		if pt.at.Before(since) || pt.at.After(now) {
			continue
		}
		if currentReset != nil && pt.reset != nil {
			// Different reset timestamp (>5m difference) implies a previous quota cycle.
			if absDuration(pt.reset.Sub(*currentReset)) > 5*time.Minute {
				continue
			}
		}
		filtered = append(filtered, pt)
	}
	if len(filtered) < 2 {
		return filtered
	}
	// If usage dropped sharply without an explicit reset timestamp change, trim before the drop.
	start := 0
	for i := 1; i < len(filtered); i++ {
		if filtered[i-1].pct-filtered[i].pct >= resetDropPct {
			start = i
		}
	}
	return filtered[start:]
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
