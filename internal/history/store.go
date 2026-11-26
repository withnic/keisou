package history

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/withnic/keisou/internal/infra"
	"github.com/withnic/keisou/internal/model"
)

const (
	Retention         = 7 * 24 * time.Hour
	MaxEntries        = 7 * 24 * 60 // Up to 1 entry/minute for 7 days.
	minChangeInterval = 15 * time.Second
	heartbeatInterval = 5 * time.Minute
	minPctDelta       = 0.1
	trendWindow       = 5 * time.Hour
	trendBuckets      = 20 // 15 minutes per bucket across 5 hours.
	paceLookback      = 60 * time.Minute
	minPaceSpan       = 30 * time.Second
	minWeeklyPaceSpan = 10 * time.Minute
	resetDropPct      = 10.0
)

// ProviderSample stores a compact quota observation for one provider.
type ProviderSample struct {
	Provider      model.ProviderID  `json:"p"`
	Available     bool              `json:"a"`
	Source        model.QuotaSource `json:"s,omitempty"`
	Window5hPct   float64           `json:"w5"`
	Window5hReset *time.Time        `json:"r5,omitempty"`
	WeeklyPct     *float64          `json:"wk,omitempty"`
	WeeklyReset   *time.Time        `json:"rw,omitempty"`
}

// Entry is a timestamped set of provider observations persisted to history.jsonl.
type Entry struct {
	At        time.Time        `json:"at"`
	Providers []ProviderSample `json:"providers"`
}

// DefaultPath returns history.jsonl alongside the resolved config.json path.
func DefaultPath(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "history.jsonl")
}

// FromSnapshots converts current UsageSnapshots into a compact history Entry.
func FromSnapshots(now time.Time, snapshots []model.UsageSnapshot) Entry {
	samples := make([]ProviderSample, 0, len(snapshots))
	for _, s := range snapshots {
		sample := ProviderSample{
			Provider:      s.Provider,
			Available:     s.Available,
			Source:        s.Source,
			Window5hPct:   model.ClampPercent(s.Window5hUsedPct),
			Window5hReset: cloneTime(s.Window5hResetAt),
			WeeklyReset:   cloneTime(s.WeeklyResetAt),
		}
		if s.WeeklyUsedPct != nil {
			v := model.ClampPercent(*s.WeeklyUsedPct)
			sample.WeeklyPct = &v
		}
		samples = append(samples, sample)
	}
	return Entry{
		At:        now.UTC(),
		Providers: samples,
	}
}

// Load reads entries within the 7-day retention window from path, sorted oldest to newest.
// Missing files return an empty slice without error.
func Load(ctx context.Context, path string, now time.Time) ([]Entry, error) {
	cutoff := now.Add(-Retention)
	var entries []Entry
	err := infra.ReadJSONLReverse(ctx, path, func(line []byte) bool {
		var entry Entry
		if err := json.Unmarshal(line, &entry); err != nil || entry.At.IsZero() {
			return true
		}
		if entry.At.Before(cutoff) {
			return false
		}
		if entry.At.After(now.Add(5 * time.Minute)) {
			return true
		}
		entries = append(entries, entry)
		return len(entries) < MaxEntries
	})
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("load history %q: %w", path, err)
	}
	slices.SortFunc(entries, func(a, b Entry) int {
		return a.At.Compare(b.At)
	})
	return entries, nil
}

// Record appends the current snapshot observation if enough time has elapsed or
// values changed, prunes entries older than 7 days, and writes atomically.
// It returns the updated in-memory history slice (oldest to newest).
func Record(ctx context.Context, path string, existing []Entry, snapshots []model.UsageSnapshot, now time.Time) ([]Entry, error) {
	cutoff := now.Add(-Retention)
	pruned := make([]Entry, 0, len(existing)+1)
	needsRewrite := false
	for _, e := range existing {
		if e.At.Before(cutoff) {
			needsRewrite = true
			continue
		}
		pruned = append(pruned, e)
	}
	next := FromSnapshots(now, snapshots)
	shouldAdd := shouldRecord(pruned, next)
	if !shouldAdd && !needsRewrite {
		return pruned, nil
	}
	if shouldAdd {
		pruned = append(pruned, next)
	}
	if len(pruned) > MaxEntries {
		pruned = pruned[len(pruned)-MaxEntries:]
	}
	if err := save(ctx, path, pruned); err != nil {
		return pruned, err
	}
	return pruned, nil
}

func shouldRecord(existing []Entry, next Entry) bool {
	if len(existing) == 0 {
		return true
	}
	last := existing[len(existing)-1]
	elapsed := next.At.Sub(last.At)
	if elapsed < 0 {
		return false
	}
	if elapsed >= heartbeatInterval {
		return true
	}
	if elapsed < minChangeInterval {
		return false
	}
	return entriesDiffer(last, next)
}

func entriesDiffer(a, b Entry) bool {
	if len(a.Providers) != len(b.Providers) {
		return true
	}
	byID := make(map[model.ProviderID]ProviderSample, len(a.Providers))
	for _, p := range a.Providers {
		byID[p.Provider] = p
	}
	for _, p := range b.Providers {
		prev, ok := byID[p.Provider]
		if !ok || prev.Available != p.Available || prev.Source != p.Source {
			return true
		}
		if math.Abs(prev.Window5hPct-p.Window5hPct) >= minPctDelta {
			return true
		}
		if (prev.WeeklyPct == nil) != (p.WeeklyPct == nil) {
			return true
		}
		if prev.WeeklyPct != nil && p.WeeklyPct != nil && math.Abs(*prev.WeeklyPct-*p.WeeklyPct) >= minPctDelta {
			return true
		}
	}
	return false
}

func save(ctx context.Context, path string, entries []Entry) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	for _, e := range entries {
		if err := enc.Encode(e); err != nil {
			return fmt.Errorf("encode history entry: %w", err)
		}
	}
	if err := infra.AtomicWriteFile(ctx, path, buf.Bytes(), 0600); err != nil {
		return fmt.Errorf("write history %q: %w", path, err)
	}
	return nil
}

func cloneTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := t.UTC()
	return &v
}
