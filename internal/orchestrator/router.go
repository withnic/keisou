package orchestrator

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/withnic/keisou/internal/model"
)

func seconds(n int) time.Duration { return time.Duration(n) * time.Second }

func effectiveUsedPct(s model.UsageSnapshot) float64 {
	used := s.Window5hUsedPct
	if s.WeeklyUsedPct != nil {
		w := *s.WeeklyUsedPct
		if math.IsNaN(w) || math.IsInf(w, 0) || w > used {
			used = w
		}
	}
	return used
}

func blockingResetAt(s model.UsageSnapshot, soft float64) *time.Time {
	hit5h := s.Window5hUsedPct >= soft
	hitWeekly := s.WeeklyUsedPct != nil && *s.WeeklyUsedPct >= soft
	switch {
	case hit5h && hitWeekly:
		if s.Window5hResetAt == nil {
			return s.WeeklyResetAt
		}
		if s.WeeklyResetAt == nil {
			return s.Window5hResetAt
		}
		if s.WeeklyResetAt.After(*s.Window5hResetAt) {
			return s.WeeklyResetAt
		}
		return s.Window5hResetAt
	case hitWeekly:
		return s.WeeklyResetAt
	default:
		return s.Window5hResetAt
	}
}

func (e Engine) candidates(snapshots []model.UsageSnapshot, exclude model.ProviderID) []model.UsageSnapshot {
	var candidates []model.UsageSnapshot
	for _, s := range snapshots {
		used := effectiveUsedPct(s)
		if s.Available && s.Provider != exclude && used >= 0 && used < e.Config.SoftThresholdPct {
			candidates = append(candidates, s)
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		ua, ub := effectiveUsedPct(a), effectiveUsedPct(b)
		if ua != ub {
			return ua < ub
		}
		return e.Config.Providers[a.Provider].Priority < e.Config.Providers[b.Provider].Priority
	})
	return candidates
}

func resetWait(snapshots []model.UsageSnapshot, now time.Time, poll int, soft float64) time.Duration {
	var earliest time.Time
	for _, s := range snapshots {
		if !s.Available {
			continue
		}
		reset := blockingResetAt(s, soft)
		if reset != nil && (earliest.IsZero() || reset.Before(earliest)) {
			earliest = *reset
		}
	}
	if earliest.IsZero() {
		return seconds(max(poll, 60))
	}
	return max(time.Second, earliest.Add(30*time.Second).Sub(now))
}

// blocked is the previous provider's most recent known usage, which may
// precede the provider's next log update (especially after an exit with 429).
func (e Engine) route(ctx context.Context, preferred model.ProviderID, blocked model.UsageSnapshot, noWait bool) (model.UsageSnapshot, error) {
	for {
		snapshots := e.Collector.Collect(ctx, e.Clock.Now())
		if err := ctx.Err(); err != nil {
			return model.UsageSnapshot{}, err
		}
		available := false
		for _, s := range snapshots {
			available = available || s.Available
		}
		if !available {
			return model.UsageSnapshot{}, failure(model.ENoAvailableProvider, fmt.Errorf("run keisou init to check provider setup"))
		}
		candidates := e.candidates(snapshots, blocked.Provider)
		if preferred != "" {
			for _, s := range candidates {
				if s.Provider == preferred {
					return s, nil
				}
			}
		}
		if len(candidates) != 0 {
			if e.Config.DefaultMode != model.ModeAsk || (preferred == "" && blocked.Provider == "") {
				return candidates[0], nil
			}
			id, err := e.Prompt.Choose(ctx, e.Diagnostic, e.Clock, snapshots, candidates, seconds(e.Config.AskTimeoutSec))
			if err != nil {
				return model.UsageSnapshot{}, failure(model.EInternal, err)
			}
			for _, s := range candidates {
				if s.Provider == id {
					return s, nil
				}
			}
			// A requested wait should use the reset of a depleted provider.
			for i := range snapshots {
				if snapshots[i].Provider != blocked.Provider && effectiveUsedPct(snapshots[i]) < e.Config.SoftThresholdPct {
					snapshots[i].Window5hResetAt = nil
					snapshots[i].WeeklyResetAt = nil
				}
			}
		}
		if noWait {
			return model.UsageSnapshot{}, failure(model.EAllQuotaExhausted, nil)
		}
		for i := range snapshots {
			if snapshots[i].Provider == blocked.Provider {
				if snapshots[i].Window5hResetAt == nil {
					snapshots[i].Window5hResetAt = blocked.Window5hResetAt
				}
				if snapshots[i].WeeklyResetAt == nil {
					snapshots[i].WeeklyResetAt = blocked.WeeklyResetAt
				}
			}
		}
		delay := resetWait(snapshots, e.Clock.Now(), e.Config.PollIntervalSec, e.Config.SoftThresholdPct)
		fmt.Fprintf(e.Diagnostic, "WAIT_RESET: %s\n", delay.Round(time.Second))
		if err := e.Clock.Sleep(ctx, delay); err != nil {
			return model.UsageSnapshot{}, err
		}
		blocked = model.UsageSnapshot{}
		preferred = ""
	}
}
