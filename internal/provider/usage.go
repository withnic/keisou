package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"math"
	"strings"
	"time"

	"github.com/withnic/keisou/internal/model"
)

type quota struct {
	at      time.Time
	reset   time.Time
	pct     float64
	present bool
}

func (q *quota) update(t, reset time.Time, pct float64) {
	if !q.present || t.After(q.at) || (t.Equal(q.at) && q.reset.IsZero() && !reset.IsZero()) {
		*q = quota{t, reset, model.ClampPercent(pct), true}
	}
}

func (q quota) current(now time.Time) (float64, *time.Time) {
	switch {
	case q.reset.IsZero():
		return q.pct, nil
	case q.reset.After(now):
		return q.pct, &q.reset
	default:
		return 0, nil
	}
}

type request struct {
	parent *request
	tokens int64
	count  bool
}

func (r *request) root() *request {
	for r.parent != nil {
		if r.parent.parent != nil {
			r.parent = r.parent.parent
		}
		r = r.parent
	}
	return r
}

const weeklyWindow = 7 * 24 * time.Hour

type usage struct {
	now                        time.Time
	since                      time.Time
	weeklySince                time.Time
	trackWeekly                bool
	anchor5hReset              *time.Time
	anchorWeeklyReset          *time.Time
	oldest                     time.Time
	oldestWeekly               time.Time
	modelAt                    time.Time
	modelName                  string
	firstPro                   time.Time
	lastFlash                  time.Time
	fallback                   bool
	primary, weekly, exhausted quota
	resetHint                  quota
	ids                        map[string]*request
	requests                   []*request
	weeklyIDs                  map[string]*request
	weeklyRequests             []*request
	err                        error
}

func newUsage(now time.Time, cfg model.ProviderConfig) usage {
	u := usage{
		now:         now,
		since:       now.Add(-window),
		weeklySince: now.Add(-weeklyWindow),
		trackWeekly: cfg.EstWeeklyTokenLimit > 0,
	}
	if t, ok := parseAnchorReset(cfg.Window5hResetAnchor, now, window); ok {
		u.anchor5hReset = &t
		if start := t.Add(-window); start.After(u.since) && !start.After(now) {
			u.since = start
		}
	}
	if t, ok := parseAnchorReset(cfg.WeeklyResetAnchor, now, weeklyWindow); ok {
		u.anchorWeeklyReset = &t
		if start := t.Add(-weeklyWindow); start.After(u.weeklySince) && !start.After(now) {
			u.weeklySince = start
		}
	}
	return u
}

func parseAnchorReset(raw string, now time.Time, period time.Duration) (time.Time, bool) {
	if strings.TrimSpace(raw) == "" {
		return time.Time{}, false
	}
	anchor, err := time.Parse(time.RFC3339, strings.TrimSpace(raw))
	if err != nil {
		return time.Time{}, false
	}
	if anchor.After(now) {
		return anchor, true
	}
	rem := now.Sub(anchor) % period
	return now.Add(period - rem), true
}

func (u *usage) windowStart() time.Time {
	if !u.since.IsZero() {
		return u.since
	}
	return u.now.Add(-window)
}

func (u *usage) weeklyWindowStart() time.Time {
	if !u.weeklySince.IsZero() {
		return u.weeklySince
	}
	return u.now.Add(-weeklyWindow)
}

func (u *usage) cutoff() time.Time {
	if u.trackWeekly {
		return u.weeklyWindowStart().Add(-margin)
	}
	return u.windowStart().Add(-margin)
}

func (u *usage) inWindow(t time.Time) bool {
	return !t.IsZero() && !t.Before(u.windowStart()) && !t.After(u.now)
}

func (u *usage) inWeeklyWindow(t time.Time) bool {
	return !t.IsZero() && !t.Before(u.weeklyWindowStart()) && !t.After(u.now)
}

func (u *usage) noteError(err error) {
	if u.err == nil && err != nil && !errors.Is(err, fs.ErrNotExist) {
		u.err = err
	}
}

func recordRequest(ids *map[string]*request, requests *[]*request, tokens int64, count bool, keys []string) {
	if *ids == nil {
		*ids = make(map[string]*request)
	}
	var record *request
	for _, id := range keys {
		if existing := (*ids)[id]; existing != nil {
			root := existing.root()
			if record == nil {
				record = root
			} else if root != record {
				record.tokens = max(record.tokens, root.tokens)
				record.count = record.count || root.count
				root.parent = record
			}
		}
	}
	if record == nil {
		record = &request{}
		*requests = append(*requests, record)
	}
	record.tokens = max(record.tokens, tokens)
	record.count = record.count || count
	for _, id := range keys {
		if id != "" {
			(*ids)[id] = record
		}
	}
}

func (u *usage) add(t time.Time, tokens int64, count bool, ids ...string) {
	if u.inWindow(t) {
		if u.oldest.IsZero() || t.Before(u.oldest) {
			u.oldest = t
		}
		recordRequest(&u.ids, &u.requests, tokens, count, ids)
	}
	if u.trackWeekly && u.inWeeklyWindow(t) {
		if u.oldestWeekly.IsZero() || t.Before(u.oldestWeekly) {
			u.oldestWeekly = t
		}
		recordRequest(&u.weeklyIDs, &u.weeklyRequests, tokens, count, ids)
	}
}

func (u *usage) observeModel(t time.Time, name string) {
	if !u.inWindow(t) || name == "" {
		return
	}
	if t.After(u.modelAt) {
		u.modelAt, u.modelName = t, name
	}
	switch name = strings.ToLower(name); {
	case strings.Contains(name, "pro"):
		if u.firstPro.IsZero() || t.Before(u.firstPro) {
			u.firstPro = t
		}
	case strings.Contains(name, "flash"):
		if t.After(u.lastFlash) {
			u.lastFlash = t
		}
	}
}

func (u *usage) metadata(t time.Time, o object) {
	if !u.inWindow(t) {
		return
	}
	payload := o.child("payload")
	message := o.child("message")
	groups := []object{o, payload, message, o.child("error"), payload.child("error"), message.child("error"), o.child("attributes")}
	var reset time.Time
	exhausted := false
	for _, group := range groups {
		if group == nil {
			continue
		}
		if reset.IsZero() {
			reset = resetTime(group)
		}
		for _, key := range []string{"type", "event", "event.name", "code", "status", "status_code", "statusCode", "http.status_code", "error", "error_type", "errorType", "message", "errorMessage", "reason"} {
			if key == "message" && isConversationRole(group.text("type", "role")) {
				continue
			}
			switch value := group[key].(type) {
			case string:
				if strings.TrimSpace(value) == "429" && isStatusKey(key) {
					exhausted = true
				} else {
					exhausted = exhausted || rateError.MatchString(value)
				}
			case json.Number:
				exhausted = exhausted || value == "429"
			}
		}
		limits := group.child("rate_limits", "rate_limit", "rateLimits", "rateLimit", "rate_limit_info", "quota")
		for i, candidate := range []object{limits.child("primary", "five_hour", "fiveHour", "5h"), limits, group} {
			if candidate == nil {
				continue
			}
			r := resetTime(candidate)
			if r.IsZero() {
				r = resetTime(group)
			}
			if r.IsZero() {
				r = reset
			}
			if reset.IsZero() {
				reset = r
			}
			if pct, ok := usedPercent(candidate); ok {
				if isWeeklyType(candidate.text("rateLimitType", "rate_limit_type")) {
					u.weekly.update(t, r, pct)
				} else {
					u.primary.update(t, r, pct)
				}
			} else if i < 2 && !r.IsZero() && !isWeeklyType(candidate.text("rateLimitType", "rate_limit_type")) {
				u.resetHint.update(t, r, 0)
			}
		}
		if weekly := limits.child("secondary", "weekly", "seven_day", "sevenDay", "7d"); weekly != nil {
			if pct, ok := usedPercent(weekly); ok {
				u.weekly.update(t, resetTime(weekly), pct)
			}
		}
	}
	if exhausted && (reset.After(u.now) || reset.IsZero() && !t.Before(u.now.Add(-margin))) {
		u.exhausted.update(t, reset, 100)
	}
}

// scanJSONLog also accepts the ordered records in the text formats.
func scanJSONLog(ctx context.Context, path string, u *usage, plainText bool, visit func(time.Time, object)) bool {
	return readLog(ctx, path, u, plainText, func(t time.Time, o object) {
		u.metadata(t, o)
		visit(t, o)
	})
}

func isConversationRole(kind string) bool {
	switch strings.ToLower(kind) {
	case "user", "assistant", "user_message", "agent_message", "agent_reasoning", "gemini", "model", "user_input", "planner_response", "generic":
		return true
	default:
		return false
	}
}

func isStatusKey(key string) bool {
	switch key {
	case "code", "status", "status_code", "statusCode", "http.status_code", "error":
		return true
	default:
		return false
	}
}

func isWeeklyType(kind string) bool {
	kind = strings.ToLower(strings.TrimSpace(kind))
	return strings.HasPrefix(kind, "seven_day") || strings.HasPrefix(kind, "sevenday") || strings.Contains(kind, "weekly") || kind == "7d"
}

func usedPercent(o object) (float64, bool) {
	if pct, ok := number(o.value("used_percentage", "usedPercentage", "used_percent", "usedPercent", "used_pct", "5h_utilization", "five_hour_utilization")); ok {
		return pct, true
	}
	if remaining, ok := number(o.value("remaining_percent", "remainingPercent", "remaining_pct")); ok {
		return 100 - remaining, true
	}
	if utilization, ok := number(o.value("utilization")); ok {
		if utilization <= 1 {
			return utilization * 100, true
		}
		return utilization, true
	}
	return 0, false
}

func (u *usage) snapshot(s model.UsageSnapshot, cfg model.ProviderConfig) model.UsageSnapshot {
	if u.err != nil {
		s.ErrorMessage = u.err.Error()
	}
	s.ActiveModel = u.modelName
	proToFlash := !u.firstPro.IsZero() && u.lastFlash.After(u.firstPro) && !strings.Contains(strings.ToLower(u.modelName), "pro")
	s.IsFallbackModel = s.Provider == model.ProviderGemini && (u.fallback || proToFlash)
	for _, r := range u.requests {
		if r.parent != nil {
			continue
		}
		s.RecentTokens5h += min(r.tokens, math.MaxInt64-s.RecentTokens5h)
		if r.count {
			s.RecentReqs5h++
		}
	}
	if u.trackWeekly {
		for _, r := range u.weeklyRequests {
			if r.parent != nil {
				continue
			}
			s.RecentTokensWeekly += min(r.tokens, math.MaxInt64-s.RecentTokensWeekly)
		}
	}
	if u.weekly.present {
		pct, reset := u.weekly.current(u.now)
		s.WeeklyUsedPct, s.WeeklyResetAt = &pct, reset
	} else if u.trackWeekly && !u.oldestWeekly.IsZero() && cfg.EstWeeklyTokenLimit > 0 {
		pct := model.ClampPercent(100 * (float64(s.RecentTokensWeekly) / float64(cfg.EstWeeklyTokenLimit)))
		s.WeeklyUsedPct = &pct
		if u.anchorWeeklyReset == nil {
			reset := u.oldestWeekly.Add(weeklyWindow)
			if reset.After(u.now) {
				s.WeeklyResetAt = &reset
			}
		}
	}
	if s.WeeklyResetAt == nil && u.anchorWeeklyReset != nil {
		t := *u.anchorWeeklyReset
		s.WeeklyResetAt = &t
	}
	switch {
	case u.exhausted.present && (!u.primary.present || !u.primary.at.After(u.exhausted.at)):
		s.Source = model.SourceLogExact
		s.Window5hUsedPct, s.Window5hResetAt = u.exhausted.current(u.now)
	case u.primary.present:
		s.Source = model.SourceLogExact
		s.Window5hUsedPct, s.Window5hResetAt = u.primary.current(u.now)
	case !u.oldest.IsZero():
		s.Source = model.SourceWindowCalc
		if cfg.Est5hTokenLimit > 0 {
			s.Window5hUsedPct = 100 * (float64(s.RecentTokens5h) / float64(cfg.Est5hTokenLimit))
		}
		if cfg.Est5hReqLimit > 0 {
			s.Window5hUsedPct = max(s.Window5hUsedPct, 100*(float64(s.RecentReqs5h)/float64(cfg.Est5hReqLimit)))
		}
		s.Window5hUsedPct = model.ClampPercent(s.Window5hUsedPct)
		reset := u.oldest.Add(window)
		if u.anchor5hReset != nil {
			reset = *u.anchor5hReset
		}
		if u.resetHint.present {
			reset = u.resetHint.reset
			if !reset.After(u.now) {
				s.Window5hUsedPct = 0
			}
		}
		if reset.After(u.now) {
			s.Window5hResetAt = &reset
		}
	default:
		if u.anchor5hReset != nil {
			t := *u.anchor5hReset
			s.Window5hResetAt = &t
		}
	}
	return s
}
