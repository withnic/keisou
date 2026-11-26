package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/withnic/keisou/internal/infra"
	"github.com/withnic/keisou/internal/model"
)

const (
	window       = 5 * time.Hour
	margin       = 10 * time.Minute
	fetchTimeout = 2 * time.Second
)

// ProviderAdapter reads local logs for a single coding agent and returns a
// point-in-time UsageSnapshot without invoking any external LLM API.
type ProviderAdapter interface {
	ID() model.ProviderID
	FetchUsage(context.Context, time.Time) model.UsageSnapshot
}

// LookPathFunc resolves an executable name on PATH; tests may inject a mock.
type LookPathFunc func(string) (string, error)

// Collector queries configured ProviderAdapters concurrently with per-provider timeouts.
type Collector struct {
	Adapters []ProviderAdapter
}

// NewCollector builds the configured adapters in display order. A nil lookup uses exec.LookPath.
func NewCollector(cfg model.Config, lookup LookPathFunc) Collector {
	if lookup == nil {
		lookup = exec.LookPath
	}
	return Collector{Adapters: []ProviderAdapter{
		adapter{model.ProviderClaude, cfg.Providers[model.ProviderClaude], lookup, scanClaude},
		adapter{model.ProviderCodex, cfg.Providers[model.ProviderCodex], lookup, scanCodex},
		adapter{model.ProviderGemini, cfg.Providers[model.ProviderGemini], lookup, scanGemini},
		adapter{model.ProviderAntigravity, cfg.Providers[model.ProviderAntigravity], lookup, scanAntigravity},
	}}
}

// Adapter returns the adapter matching id, if present in the collector.
func (c Collector) Adapter(id model.ProviderID) (ProviderAdapter, bool) {
	for _, a := range c.Adapters {
		if a.ID() == id {
			return a, true
		}
	}
	return nil, false
}

// Collect keeps adapter order and bounds the wait even for a non-cooperative adapter.
func (c Collector) Collect(ctx context.Context, now time.Time) []model.UsageSnapshot {
	snapshots := make([]model.UsageSnapshot, len(c.Adapters))
	var workers sync.WaitGroup
	for i, a := range c.Adapters {
		workers.Add(1)
		go func() {
			defer workers.Done()
			ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
			defer cancel()
			baseline := make(chan model.UsageSnapshot, 1)
			result := make(chan model.UsageSnapshot, 1)
			go func() {
				if configured, ok := a.(adapter); ok {
					result <- configured.fetch(ctx, now, baseline)
					return
				}
				result <- a.FetchUsage(ctx, now)
			}()
			select {
			case snapshots[i] = <-result:
			case <-ctx.Done():
				snapshot := emptySnapshot(a.ID(), now)
				select {
				case snapshot = <-result:
				case snapshot = <-baseline:
				default:
				}
				snapshot.ErrorMessage = ctx.Err().Error()
				snapshots[i] = snapshot
			}
		}()
	}
	workers.Wait()
	return snapshots
}

type adapter struct {
	id     model.ProviderID
	config model.ProviderConfig
	lookup LookPathFunc
	scan   func(context.Context, string, *usage)
}

func (a adapter) ID() model.ProviderID { return a.id }

func (a adapter) FetchUsage(ctx context.Context, now time.Time) model.UsageSnapshot {
	return a.fetch(ctx, now, make(chan model.UsageSnapshot, 1))
}

func emptySnapshot(id model.ProviderID, now time.Time) model.UsageSnapshot {
	return model.UsageSnapshot{Provider: id, UpdatedAt: now, Source: model.SourceUnavailable}
}

func (a adapter) fetch(ctx context.Context, now time.Time, baseline chan<- model.UsageSnapshot) model.UsageSnapshot {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	snapshot := emptySnapshot(a.id, now)
	snapshot.PlanName = a.config.PlanName
	if !a.config.Enabled {
		return snapshot
	}
	_, err := a.lookup(a.config.Command)
	snapshot.Available = err == nil
	if err != nil {
		snapshot.ErrorMessage = fmt.Sprintf("resolve %s CLI: %v", a.id, err)
	}
	baseline <- snapshot
	dirs, err := infra.ProviderDataDirs(a.id, a.config.DataDir, a.config.ExtraDataDirs)
	if err != nil {
		snapshot.ErrorMessage = err.Error()
		return snapshot
	}
	u := newUsage(now, a.config)
	a.scan(ctx, dirs[0], &u)
	for _, extraDir := range dirs[1:] {
		if ctx.Err() != nil {
			break
		}
		info, statErr := os.Stat(extraDir)
		if statErr != nil || !info.IsDir() {
			continue
		}
		prevErr := u.err
		a.scan(ctx, extraDir, &u)
		// Do not let an optional extra directory error mask a clean primary scan.
		if prevErr == nil && u.err != nil {
			u.err = nil
		}
	}
	return u.snapshot(snapshot, a.config)
}

type object map[string]any

func (o object) value(keys ...string) any {
	for _, key := range keys {
		if v := o[key]; v != nil {
			return v
		}
	}
	return nil
}

func (o object) child(keys ...string) object {
	v, _ := o.value(keys...).(map[string]any)
	return v
}

func (o object) text(keys ...string) string {
	v, _ := o.value(keys...).(string)
	return v
}

func number(v any) (float64, bool) {
	s, ok := v.(string)
	if n, isNumber := v.(json.Number); isNumber {
		s, ok = string(n), true
	}
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseFloat(s, 64)
	return n, err == nil && !math.IsNaN(n) && !math.IsInf(n, 0)
}

func tokenCount(o object, keys ...string) int64 {
	var total int64
	for _, key := range keys {
		n, ok := number(o[key])
		if ok && n > 0 {
			if n >= float64(math.MaxInt64-total) {
				return math.MaxInt64
			}
			total += int64(n)
		}
	}
	return total
}

func parseTime(v any) time.Time {
	if s, ok := v.(string); ok {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return t
		}
	}
	n, ok := number(v)
	if !ok || n <= 0 {
		return time.Time{}
	}
	switch {
	case n >= 1e17:
		n /= 1e9
	case n >= 1e14:
		n /= 1e6
	case n >= 1e11:
		n /= 1e3
	}
	if n >= 253402300800 { // JSON timestamps must have a four-digit year.
		return time.Time{}
	}
	sec, fraction := math.Modf(n)
	return time.Unix(int64(sec), int64(fraction*1e9)).UTC()
}

func timestamp(o object) time.Time {
	return parseTime(o.value("timestamp", "created_at", "createdAt", "updated_at", "updatedAt", "ts", "time", "lastUpdated"))
}

func resetTime(o object) time.Time {
	return parseTime(o.value("resets_at", "reset_at", "resetsAt", "resetAt", "reset_time", "resetTime", "reset"))
}

func decodeObject(line []byte) object {
	if line = bytes.TrimSpace(line); len(line) == 0 || line[0] != '{' {
		return nil
	}
	d := json.NewDecoder(bytes.NewReader(line))
	d.UseNumber()
	var o object
	if d.Decode(&o) != nil || d.Decode(new(json.RawMessage)) != io.EOF {
		return nil
	}
	return o
}

func walk(ctx context.Context, root string, cutoff time.Time, match func(string) bool, visit func(string), onErr func(error)) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			if path != root {
				if onErr != nil {
					onErr(err)
				}
				return nil
			}
			return err
		}
		if !entry.Type().IsRegular() || !match(path) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			if onErr != nil {
				onErr(err)
			}
			return nil
		}
		if !info.ModTime().Before(cutoff) {
			visit(path)
		}
		return nil
	})
}

func walkLogs(ctx context.Context, root, extension string, u *usage, visit func(string)) {
	u.noteError(walk(ctx, root, u.cutoff(), func(path string) bool {
		return filepath.Ext(path) == extension
	}, visit, u.noteError))
}

func scanFile(ctx context.Context, path string, u *usage, visit func(string)) {
	u.noteError(walk(ctx, path, u.cutoff(), func(candidate string) bool { return candidate == path }, visit, u.noteError))
}

var textTime = regexp.MustCompile(`^\[?(\d{4}-\d{2}-\d{2}T[^\s\]]+)(?:[\]\s]|$)`)
var rateError = regexp.MustCompile(`(?i)(?:\b(?:resource_exhausted|rate_limit_error|rate limit exceeded|quota exceeded|too many requests)\b|(?:\b(?:http|status|code|err|error)\b\D{0,10}429\b)|(?:\b429\b\D{0,20}\b(?:rate|limit|quota|too many|exhausted)\b))`)
var textReset = regexp.MustCompile("(?i)(?:resets?_at|reset_?time)[\\x22:= ]+([^\\s,}\\x22]+)")
var fallbackEvent = regexp.MustCompile("(?i)\\b(?:fallback|falling back|model_fallback|fallback_model)\\b")
var quotaJSON = regexp.MustCompile(`"(rate_limits?)"\s*:\s*\{`)

func readLog(ctx context.Context, path string, u *usage, plainText bool, visit func(time.Time, object)) bool {
	reachedStart := true
	err := infra.ReadLinesReverse(ctx, path, func(line []byte) bool {
		o := decodeObject(line)
		t := timestamp(o)
		if t.IsZero() {
			t = timestamp(o.child("payload"))
		}
		if o == nil && plainText {
			t, o = parseTextLog(line)
		}
		if t.IsZero() {
			return true
		}
		if t.Before(u.cutoff()) {
			reachedStart = false
			return false
		}
		if !t.After(u.now) {
			visit(t, o)
		}
		return true
	})
	u.noteError(err)
	return reachedStart && err == nil
}

func parseTextLog(line []byte) (time.Time, object) {
	// Text logs start with an RFC3339 timestamp, optionally inside brackets.
	prefix := textTime.FindSubmatchIndex(line)
	if prefix == nil {
		return time.Time{}, nil
	}
	t := parseTime(string(line[prefix[2]:prefix[3]]))
	body := line[prefix[1]:]
	o := object{"message": string(body)}
	if match := textReset.FindSubmatch(body); match != nil {
		o["resets_at"] = string(match[1])
	}
	if match := quotaJSON.FindSubmatchIndex(body); match != nil {
		start := match[1] - 1
		d := json.NewDecoder(bytes.NewReader(body[start:]))
		d.UseNumber()
		var limits map[string]any
		if d.Decode(&limits) == nil {
			o[string(body[match[2]:match[3]])] = limits
		}
	}
	return t, o
}

// readObjectFile streams selected top-level arrays one object at a time.
// Day-granularity stats cannot be attributed to the rolling five-hour window.
func readObjectFile(ctx context.Context, path string, u *usage, arrays map[string]bool, visit func(object)) {
	err := infra.ReadJSON(ctx, path, func(d *json.Decoder) error {
		d.UseNumber()
		start, err := d.Token()
		if err != nil {
			return err
		}
		if start != json.Delim('{') {
			return fmt.Errorf("expected a JSON object")
		}
		root := make(object)
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return err
			}
			name := key.(string)
			if !arrays[name] {
				var value any
				if err := d.Decode(&value); err != nil {
					return err
				}
				root[name] = value
				continue
			}
			token, err := d.Token()
			if err != nil {
				return err
			}
			if token == nil {
				continue
			}
			if token != json.Delim('[') {
				return fmt.Errorf("%q must be a JSON array", name)
			}
			for d.More() {
				var entry any
				if err := d.Decode(&entry); err != nil {
					return err
				}
				if record, ok := entry.(map[string]any); ok {
					visit(record)
				}
			}
			if _, err := d.Token(); err != nil {
				return err
			}
		}
		if _, err := d.Token(); err != nil {
			return err
		}
		if err := d.Decode(new(json.RawMessage)); err != io.EOF {
			return fmt.Errorf("expected end of JSON object")
		}
		visit(root)
		return nil
	})
	if !errors.Is(err, io.EOF) {
		u.noteError(err)
	}
}
