package provider

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/withnic/keisou/internal/model"
)

var testNow = time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func logFile(t *testing.T, dir, name string, data []byte, mtime time.Time) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func availableCLI(name string) (string, error) { return name, nil }

func fixtureAdapter(t *testing.T, id model.ProviderID, dir string, tokens int64, requests int) ProviderAdapter {
	t.Helper()
	t.Setenv("KEISOU_"+strings.ToUpper(string(id))+"_DIR", "")
	cfg := model.Config{Providers: map[model.ProviderID]model.ProviderConfig{
		id: {Enabled: true, Command: string(id), DataDir: dir, Est5hTokenLimit: tokens, Est5hReqLimit: requests},
	}}
	for _, a := range NewCollector(cfg, availableCLI).Adapters {
		if a.ID() == id {
			return a
		}
	}
	t.Fatalf("missing adapter %s", id)
	return nil
}

func TestProviderFixtures(t *testing.T) {
	cases := []struct {
		id                       model.ProviderID
		fixture, path, modelName string
		source                   model.QuotaSource
		tokens                   int64
		requests                 int
		pct                      float64
		fallback                 bool
	}{
		{model.ProviderClaude, "claude.jsonl", "projects/a/subagent/log.jsonl", "claude-opus", model.SourceWindowCalc, 94, 1, 94, false},
		{model.ProviderCodex, "codex.jsonl", "sessions/2026/11/25/session.jsonl", "gpt-5", model.SourceLogExact, 95, 2, 94.9, false},
		{model.ProviderGemini, "gemini.json", "tmp/project/deeper/chats/session.json", "gemini-3-flash", model.SourceWindowCalc, 98, 2, 98, true},
		{model.ProviderAntigravity, "antigravity.jsonl", "brain/conv-1/.system_generated/logs/transcript.jsonl", "gemini-pro-latest", model.SourceWindowCalc, 97, 2, 97, false},
	}
	for _, tc := range cases {
		t.Run(string(tc.id), func(t *testing.T) {
			dir := t.TempDir()
			// Exactly the mtime margin is still eligible. JSON timestamps decide membership.
			logFile(t, dir, tc.path, fixture(t, tc.fixture), testNow.Add(-window-margin))
			a := fixtureAdapter(t, tc.id, dir, 100, 100)
			s := a.FetchUsage(context.Background(), testNow)
			if !s.Available || s.ActiveModel != tc.modelName || s.Source != tc.source ||
				s.RecentTokens5h != tc.tokens || s.RecentReqs5h != tc.requests ||
				s.Window5hUsedPct != tc.pct || s.IsFallbackModel != tc.fallback || s.ErrorMessage != "" {
				t.Fatalf("unexpected snapshot: %+v", s)
			}
			if tc.id == model.ProviderCodex && (s.WeeklyUsedPct == nil || *s.WeeklyUsedPct != 22 || s.WeeklyResetAt == nil) {
				t.Fatalf("missing weekly quota: %+v", s)
			}
			if s.Window5hResetAt == nil || !s.Window5hResetAt.After(testNow) {
				t.Fatalf("missing future reset: %+v", s)
			}
		})
	}
}

func TestQuotaBoundaries(t *testing.T) {
	var cases []struct {
		Name     string
		Provider model.ProviderID
		Record   json.RawMessage
		Want     float64
		Source   model.QuotaSource
		Reset    bool
	}
	if err := json.Unmarshal(fixture(t, "boundaries.json"), &cases); err != nil {
		t.Fatal(err)
	}
	paths := map[model.ProviderID]string{
		model.ProviderClaude: "projects/one.jsonl",
		model.ProviderCodex:  "sessions/one.jsonl",
		model.ProviderGemini: "telemetry.log",
	}
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			dir := t.TempDir()
			logFile(t, dir, paths[tc.Provider], tc.Record, testNow)
			s := fixtureAdapter(t, tc.Provider, dir, 0, 0).FetchUsage(context.Background(), testNow)
			if s.Window5hUsedPct != tc.Want || s.Source != tc.Source || (s.Window5hResetAt != nil) != tc.Reset {
				t.Fatalf("unexpected snapshot: %+v", s)
			}
		})
	}
}

func TestMissingEmptyAndOldLogs(t *testing.T) {
	for _, id := range []model.ProviderID{model.ProviderClaude, model.ProviderCodex, model.ProviderGemini, model.ProviderAntigravity} {
		t.Run(string(id), func(t *testing.T) {
			dir := t.TempDir()
			a := fixtureAdapter(t, id, dir, 0, 0)
			for _, populate := range []bool{false, true} {
				if populate {
					logFile(t, dir, "projects/empty.jsonl", nil, testNow)
					logFile(t, dir, "sessions/empty.jsonl", nil, testNow)
					logFile(t, dir, "tmp/project/chats/empty.json", nil, testNow)
					logFile(t, dir, "brain/conv-empty/.system_generated/logs/transcript.jsonl", nil, testNow)
					logFile(t, dir, "telemetry.log", nil, testNow)
					logFile(t, dir, "projects/old.jsonl", fixture(t, "claude.jsonl"), testNow.Add(-window-margin-time.Second))
					logFile(t, dir, "brain/conv-old/.system_generated/logs/transcript.jsonl", fixture(t, "antigravity.jsonl"), testNow.Add(-window-margin-time.Second))
				}
				s := a.FetchUsage(context.Background(), testNow)
				if !s.Available || s.Source != model.SourceUnavailable || s.Window5hUsedPct != 0 || s.ErrorMessage != "" {
					t.Fatalf("unexpected snapshot: %+v", s)
				}
			}
		})
	}
}

func TestAuxiliaryLogs(t *testing.T) {
	dir := t.TempDir()
	logFile(t, dir, "stats-cache.json", []byte(`{"dailyActivity":[{"date":"2026-11-25","messageCount":999}],"events":[{"timestamp":"2026-11-25T23:45:00Z","rate_limit":{"5h_utilization":7}}]}`), testNow)
	logFile(t, dir, "log/codex.log", []byte(`[2026-11-25T23:59:00Z] INFO {"rate_limits": {"primary":{"used_percent":33}}}`), testNow)
	logFile(t, dir, "telemetry.log", []byte("2026-11-25T23:59:00Z WARN model fallback: RESOURCE_EXHAUSTED reset_at=2026-11-26T03:00:00Z\n"), testNow)
	for _, tc := range []struct {
		id   model.ProviderID
		want float64
	}{
		{model.ProviderClaude, 7}, {model.ProviderCodex, 33}, {model.ProviderGemini, 100},
	} {
		s := fixtureAdapter(t, tc.id, dir, 0, 0).FetchUsage(context.Background(), testNow)
		if s.Source != model.SourceLogExact || s.Window5hUsedPct != tc.want || (tc.id == model.ProviderGemini && (!s.IsFallbackModel || s.Window5hResetAt == nil)) {
			t.Errorf("unexpected %s snapshot: %+v", tc.id, s)
		}
	}
}

func TestReverseSkipsOversizedAndStopsAtCutoff(t *testing.T) {
	dir := t.TempDir()
	data := "{\"timestamp\":\"2026-11-25T23:58:00Z\",\"5h_utilization\":100}\n" +
		"{\"timestamp\":\"2026-11-25T18:49:59Z\"}\n" +
		strings.Repeat("x", 10<<20) + "\n" +
		"{\"timestamp\":\"2026-11-25T23:59:00Z\",\"type\":\"assistant\",\"message\":{\"model\":\"日本語\",\"usage\":{\"input_tokens\":1}},\"padding\":\"" +
		strings.Repeat("x", (64<<10)-60) + "\"}\n"
	logFile(t, dir, "projects/main.jsonl", []byte(data), testNow)
	s := fixtureAdapter(t, model.ProviderClaude, dir, 0, 0).FetchUsage(context.Background(), testNow)
	if s.Source != model.SourceWindowCalc || s.Window5hUsedPct != 0 || s.ActiveModel != "日本語" || s.RecentTokens5h != 1 {
		t.Fatalf("unexpected snapshot: %+v", s)
	}
}

type collectorStub struct {
	id    model.ProviderID
	fetch func(context.Context) model.UsageSnapshot
}

func (s collectorStub) ID() model.ProviderID { return s.id }
func (s collectorStub) FetchUsage(ctx context.Context, _ time.Time) model.UsageSnapshot {
	return s.fetch(ctx)
}

func TestCollectorIsolatesTimeout(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	started := make(chan struct{})
	collector := Collector{Adapters: []ProviderAdapter{
		collectorStub{model.ProviderClaude, func(ctx context.Context) model.UsageSnapshot {
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) > fetchTimeout {
				t.Error("adapter did not receive a two-second deadline")
			}
			close(started)
			<-release
			return model.UsageSnapshot{}
		}},
		collectorStub{model.ProviderCodex, func(ctx context.Context) model.UsageSnapshot {
			<-started
			return model.UsageSnapshot{Provider: model.ProviderCodex, Available: true}
		}},
	}}
	got := collector.Collect(context.Background(), testNow)
	if got[0].Provider != model.ProviderClaude || got[0].ErrorMessage != context.DeadlineExceeded.Error() ||
		got[1].Provider != model.ProviderCodex || !got[1].Available {
		t.Fatalf("unexpected collection: %+v", got)
	}
}

func TestAvailabilityRequiresEnabledAndResolvedCLI(t *testing.T) {
	cfg := model.Config{Providers: map[model.ProviderID]model.ProviderConfig{
		model.ProviderClaude: {Command: "disabled"},
		model.ProviderCodex:  {Enabled: true, Command: "missing", DataDir: t.TempDir()},
		model.ProviderGemini: {Enabled: true, Command: "present", DataDir: t.TempDir()},
	}}
	got := NewCollector(cfg, func(command string) (string, error) {
		switch command {
		case "present":
			return command, nil
		case "disabled":
			t.Error("looked up a disabled provider")
		}
		return "", errors.New("command missing")
	}).Collect(context.Background(), testNow)
	if got[0].Available || got[1].Available || !got[2].Available {
		t.Fatalf("unexpected availability: %+v", got)
	}
}

func TestRateLimitFalsePositivesAndRecovery(t *testing.T) {
	// Conversational messages mentioning 429 / RESOURCE_EXHAUSTED and plain INFO logs
	// with bare "429" token counts must not falsely mark a provider 100% exhausted.
	dir := t.TempDir()
	codexSession := strings.Join([]string{
		`{"timestamp":"2026-11-25T23:55:00Z","type":"event_msg","payload":{"type":"user_message","message":"fix the 429 RESOURCE_EXHAUSTED quota exceeded bug"}}`,
		`{"timestamp":"2026-11-25T23:56:00Z","type":"event_msg","payload":{"type":"agent_message","message":"handled HTTP 429 too many requests"}}`,
		`{"timestamp":"2026-11-25T23:57:00Z","type":"event_msg","payload":{"type":"rate_limit","rate_limits":{"primary":{"used_percent":35}}}}`,
	}, "\n") + "\n"
	logFile(t, dir, "sessions/chat.jsonl", []byte(codexSession), testNow)
	logFile(t, dir, "log/codex.log", []byte("2026-11-25T23:58:00Z INFO processed 429 tokens cleanly\n"), testNow)

	s := fixtureAdapter(t, model.ProviderCodex, dir, 1000, 100).FetchUsage(context.Background(), testNow)
	if s.Source != model.SourceLogExact || s.Window5hUsedPct != 35 {
		t.Fatalf("false-positive rate limit check got %+v, want 35%% log_exact", s)
	}

	// Plain-text HTTP 429 error MUST trigger 100% exhaustion.
	dir429 := t.TempDir()
	logFile(t, dir429, "log/codex.log", []byte("2026-11-25T23:58:00Z ERROR upstream returned HTTP 429\n"), testNow)
	s = fixtureAdapter(t, model.ProviderCodex, dir429, 1000, 100).FetchUsage(context.Background(), testNow)
	if s.Source != model.SourceLogExact || s.Window5hUsedPct != 100 {
		t.Fatalf("HTTP 429 text log got %+v, want 100%% log_exact", s)
	}

	// Newer explicit primary quota overrides an older 429 without reset time.
	dirRecover := t.TempDir()
	recoverSession := strings.Join([]string{
		`{"timestamp":"2026-11-25T23:52:00Z","payload":{"type":"rate_limit_error"}}`,
		`{"timestamp":"2026-11-25T23:58:00Z","payload":{"rate_limits":{"primary":{"used_percent":45,"resets_at":"2026-11-26T04:00:00Z"}}}}`,
	}, "\n") + "\n"
	logFile(t, dirRecover, "sessions/recover.jsonl", []byte(recoverSession), testNow)
	s = fixtureAdapter(t, model.ProviderCodex, dirRecover, 1000, 100).FetchUsage(context.Background(), testNow)
	if s.Source != model.SourceLogExact || s.Window5hUsedPct != 45 || s.Window5hResetAt == nil {
		t.Fatalf("recovered quota got %+v, want 45%% with reset", s)
	}
}

func TestUtilizationScaleTimestampsFallbackAndAdapterLookup(t *testing.T) {
	// utilization > 1 is treated as percentage rather than clamped to 100%.
	dir := t.TempDir()
	logFile(t, dir, "projects/u.jsonl", []byte(`{"timestamp":1795650900000000,"rate_limit_info":{"utilization":50}}`+"\n"), testNow)
	s := fixtureAdapter(t, model.ProviderClaude, dir, 100, 100).FetchUsage(context.Background(), testNow)
	if s.Source != model.SourceLogExact || s.Window5hUsedPct != 50 {
		t.Fatalf("utilization=50 with microsecond timestamp got %+v, want 50%% log_exact", s)
	}

	// Nanosecond timestamp parsing.
	if got := parseTime(json.Number("1795650900000000000")); !got.Equal(time.Unix(1795650900, 0).UTC()) {
		t.Fatalf("nanosecond parseTime = %v, want %v", got, time.Unix(1795650900, 0).UTC())
	}

	// Gemini pro -> flash -> pro recovery should not mark active pro model as fallback.
	gemDir := t.TempDir()
	gemChat := `{"messages":[` +
		`{"id":"1","timestamp":"2026-11-25T20:00:00Z","type":"gemini","model":"gemini-3-pro","tokens":{"input":10}},` +
		`{"id":"2","timestamp":"2026-11-25T21:00:00Z","type":"gemini","model":"gemini-3-flash","tokens":{"input":10}},` +
		`{"id":"3","timestamp":"2026-11-25T23:50:00Z","type":"gemini","model":"gemini-3-pro","tokens":{"input":10}}` +
		`]}`
	logFile(t, gemDir, "tmp/p/chats/recovered.json", []byte(gemChat), testNow)
	// Also add a corrupt sibling JSON to confirm valid chats are still parsed.
	logFile(t, gemDir, "tmp/p/chats/corrupt.json", []byte(`{"messages":[{`), testNow)
	gs := fixtureAdapter(t, model.ProviderGemini, gemDir, 100, 100).FetchUsage(context.Background(), testNow)
	if gs.ActiveModel != "gemini-3-pro" || gs.IsFallbackModel || gs.RecentTokens5h != 30 {
		t.Fatalf("gemini recovery + corrupt sibling got %+v", gs)
	}

	// Collector.Adapter lookup for Phase 3 single-provider polling.
	c := NewCollector(model.Config{Providers: map[model.ProviderID]model.ProviderConfig{}}, availableCLI)
	if a, ok := c.Adapter(model.ProviderCodex); !ok || a.ID() != model.ProviderCodex {
		t.Fatalf("Adapter(codex) = %v, %t", a, ok)
	}
	if _, ok := c.Adapter(model.ProviderID("unknown")); ok {
		t.Fatal("Adapter(unknown) unexpectedly succeeded")
	}
}

func TestRecentLogTailProvidersAndUTF8Truncation(t *testing.T) {
	// Missing dir returns empty tail cleanly.
	if tail, err := RecentLogTail(context.Background(), model.ProviderClaude, filepath.Join(t.TempDir(), "missing")); err != nil || tail != "" {
		t.Fatalf("missing RecentLogTail = %q, %v", tail, err)
	}

	// Claude JSONL with multi-byte UTF-8 line exceeding 16 KiB truncates cleanly without replacement runes.
	claudeDir := t.TempDir()
	longUTF8 := `{"text":"` + strings.Repeat("日", 7000) + `"}` + "\n"
	logFile(t, claudeDir, "projects/a/older.jsonl", []byte(`{"old":1}`+"\n"), testNow.Add(-time.Hour))
	logFile(t, claudeDir, "projects/a/newer.jsonl", []byte(`{"first":1}`+"\n"+longUTF8), testNow)
	tail, err := RecentLogTail(context.Background(), model.ProviderClaude, claudeDir)
	if err != nil || tail == "" || strings.ContainsRune(tail, '\ufffd') || strings.Contains(tail, `"old"`) {
		t.Fatalf("claude RecentLogTail err=%v len=%d", err, len(tail))
	}

	// Codex JSONL latest session.
	codexDir := t.TempDir()
	logFile(t, codexDir, "sessions/2026/11/25/s.jsonl", fixture(t, "codex.jsonl"), testNow)
	tail, err = RecentLogTail(context.Background(), model.ProviderCodex, codexDir)
	if err != nil || !strings.Contains(tail, "gpt-5") {
		t.Fatalf("codex RecentLogTail = %q, %v", tail, err)
	}

	// Gemini formatted JSON chat.
	gemDir := t.TempDir()
	logFile(t, gemDir, "tmp/p/chats/session.json", fixture(t, "gemini.json"), testNow)
	tail, err = RecentLogTail(context.Background(), model.ProviderGemini, gemDir)
	if err != nil || !strings.Contains(tail, "gemini-3-flash") {
		t.Fatalf("gemini RecentLogTail = %q, %v", tail, err)
	}

	// Antigravity transcript.jsonl (ignoring transcript_full.jsonl).
	agyDir := t.TempDir()
	logFile(t, agyDir, "brain/conv-1/.system_generated/logs/transcript.jsonl", fixture(t, "antigravity.jsonl"), testNow)
	logFile(t, agyDir, "brain/conv-1/.system_generated/logs/transcript_full.jsonl", []byte(`{"ignored":true}`+"\n"), testNow.Add(time.Minute))
	tail, err = RecentLogTail(context.Background(), model.ProviderAntigravity, agyDir)
	if err != nil || !strings.Contains(tail, "gemini-pro-latest") || strings.Contains(tail, `"ignored"`) {
		t.Fatalf("antigravity RecentLogTail = %q, %v", tail, err)
	}
}

func TestAntigravityMultiConversationAndFullTranscriptIgnored(t *testing.T) {
	dir := t.TempDir()
	line := `{"step_index":1,"type":"PLANNER_RESPONSE","created_at":"2026-11-25T23:55:00Z","model":"ag-model","input_tokens":20,"output_tokens":5}` + "\n"
	logFile(t, dir, "brain/conv-a/.system_generated/logs/transcript.jsonl", []byte(line), testNow)
	logFile(t, dir, "brain/conv-b/.system_generated/logs/transcript.jsonl", []byte(line), testNow)
	// transcript_full.jsonl must not be double-counted.
	logFile(t, dir, "brain/conv-a/.system_generated/logs/transcript_full.jsonl", []byte(line), testNow)

	s := fixtureAdapter(t, model.ProviderAntigravity, dir, 100, 100).FetchUsage(context.Background(), testNow)
	if s.Source != model.SourceWindowCalc || s.RecentTokens5h != 50 || s.RecentReqs5h != 2 || s.Window5hUsedPct != 50 {
		t.Fatalf("antigravity multi-conv = %+v, want 50 tokens / 2 reqs / 50%%", s)
	}
}

func TestWeeklyVariantRateLimitDoesNotLeakInto5h(t *testing.T) {
	dir := t.TempDir()
	line := `{"timestamp":"2026-11-25T23:55:00Z","rate_limit_info":{"rateLimitType":"seven_day_opus","used_percentage":30,"resets_at":"2026-11-28T08:35:00Z"}}` + "\n"
	logFile(t, dir, "projects/weekly.jsonl", []byte(line), testNow)

	s := fixtureAdapter(t, model.ProviderClaude, dir, 1000, 100).FetchUsage(context.Background(), testNow)
	if s.Window5hUsedPct != 0 || s.WeeklyUsedPct == nil || *s.WeeklyUsedPct != 30 || s.WeeklyResetAt == nil {
		t.Fatalf("seven_day_opus leaked into 5h: %+v", s)
	}
}

func TestConfiguredResetAnchorsRollForwardAndBoundWindow(t *testing.T) {
	// testNow is 2026-11-26T00:00:00Z.
	// Past 5h anchor at 2026-11-25T16:35:00Z rolls forward +5h -> 21:35:00Z -> +5h -> 2026-11-26T02:35:00Z.
	// Current 5h cycle started at 2026-11-25T21:35:00Z, so an event at 21:00:00Z is excluded.
	dir := t.TempDir()
	lines := strings.Join([]string{
		`{"step_index":1,"type":"PLANNER_RESPONSE","created_at":"2026-11-25T21:00:00Z","model":"ag-old","input_tokens":500,"output_tokens":0}`,
		`{"step_index":2,"type":"PLANNER_RESPONSE","created_at":"2026-11-25T22:00:00Z","model":"ag-new","input_tokens":20,"output_tokens":5}`,
	}, "\n") + "\n"
	logFile(t, dir, "brain/conv-1/.system_generated/logs/transcript.jsonl", []byte(lines), testNow)
	t.Setenv("KEISOU_ANTIGRAVITY_DIR", "")
	cfg := model.Config{Providers: map[model.ProviderID]model.ProviderConfig{
		model.ProviderAntigravity: {
			Enabled:             true,
			Command:             "agy",
			DataDir:             dir,
			Est5hTokenLimit:     100,
			Window5hResetAnchor: "2026-11-25T16:35:00Z",
			WeeklyResetAnchor:   "2026-11-21T08:35:00Z", // +7d -> 2026-11-28T08:35:00Z
		},
	}}
	a, ok := NewCollector(cfg, availableCLI).Adapter(model.ProviderAntigravity)
	if !ok {
		t.Fatal("missing antigravity adapter")
	}
	s := a.FetchUsage(context.Background(), testNow)
	want5h := time.Date(2026, 11, 26, 2, 35, 0, 0, time.UTC)
	wantWeekly := time.Date(2026, 11, 28, 8, 35, 0, 0, time.UTC)
	if s.Source != model.SourceWindowCalc || s.RecentTokens5h != 25 || s.Window5hUsedPct != 25 ||
		s.Window5hResetAt == nil || !s.Window5hResetAt.Equal(want5h) ||
		s.WeeklyResetAt == nil || !s.WeeklyResetAt.Equal(wantWeekly) {
		t.Fatalf("anchored snapshot = %+v, want 25%% reset=%v weekly=%v", s, want5h, wantWeekly)
	}

	// Future anchor is used directly without rolling forward even when no logs exist yet.
	emptyDir := t.TempDir()
	cfg.Providers[model.ProviderAntigravity] = model.ProviderConfig{
		Enabled:             true,
		Command:             "agy",
		DataDir:             emptyDir,
		Window5hResetAnchor: "2026-11-26T01:35:00Z",
		WeeklyResetAnchor:   "2026-11-28T08:35:00Z",
	}
	a, _ = NewCollector(cfg, availableCLI).Adapter(model.ProviderAntigravity)
	s = a.FetchUsage(context.Background(), testNow)
	wantFuture := time.Date(2026, 11, 26, 1, 35, 0, 0, time.UTC)
	if s.Window5hResetAt == nil || !s.Window5hResetAt.Equal(wantFuture) || s.WeeklyResetAt == nil || !s.WeeklyResetAt.Equal(wantWeekly) {
		t.Fatalf("empty anchored snapshot = %+v", s)
	}
}

func TestLocalWeeklyTokenEstimation(t *testing.T) {
	// testNow is 2026-11-26T00:00:00Z; weekly anchor 2026-11-28T00:00:00Z -> weekly cycle started 2026-11-21T00:00:00Z.
	dir := t.TempDir()
	// Event before weekly cycle (Nov 20) -> excluded from both 5h and weekly.
	logFile(t, dir, "brain/old/.system_generated/logs/transcript.jsonl",
		[]byte(`{"step_index":1,"type":"PLANNER_RESPONSE","created_at":"2026-11-20T23:00:00Z","model":"m","input_tokens":500,"output_tokens":0}`+"\n"),
		time.Date(2026, 11, 20, 23, 0, 0, 0, time.UTC))
	// Event 2 days ago (Nov 24) -> included in weekly, excluded from 5h.
	logFile(t, dir, "brain/mid/.system_generated/logs/transcript.jsonl",
		[]byte(`{"step_index":1,"type":"PLANNER_RESPONSE","created_at":"2026-11-24T12:00:00Z","model":"m","input_tokens":250,"output_tokens":0}`+"\n"),
		time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC))
	// Event 1 hour ago (Nov 25 23:00) -> included in both 5h and weekly.
	logFile(t, dir, "brain/now/.system_generated/logs/transcript.jsonl",
		[]byte(`{"step_index":1,"type":"PLANNER_RESPONSE","created_at":"2026-11-25T23:00:00Z","model":"m-now","input_tokens":50,"output_tokens":0}`+"\n"),
		testNow)

	t.Setenv("KEISOU_ANTIGRAVITY_DIR", "")
	cfg := model.Config{Providers: map[model.ProviderID]model.ProviderConfig{
		model.ProviderAntigravity: {
			Enabled:             true,
			Command:             "agy",
			DataDir:             dir,
			Est5hTokenLimit:     1000,
			EstWeeklyTokenLimit: 1000,
			WeeklyResetAnchor:   "2026-11-28T00:00:00Z",
		},
	}}
	a, _ := NewCollector(cfg, availableCLI).Adapter(model.ProviderAntigravity)
	s := a.FetchUsage(context.Background(), testNow)
	if s.RecentTokens5h != 50 || s.Window5hUsedPct != 5 || s.RecentTokensWeekly != 300 ||
		s.WeeklyUsedPct == nil || *s.WeeklyUsedPct != 30 || s.ActiveModel != "m-now" {
		t.Fatalf("weekly estimation snapshot = %+v, want 5h=50(5%%) weekly=300(30%%)", s)
	}
}
