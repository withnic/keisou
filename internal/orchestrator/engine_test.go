package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/withnic/keisou/internal/config"
	"github.com/withnic/keisou/internal/infra"
	"github.com/withnic/keisou/internal/model"
	"github.com/withnic/keisou/internal/provider"
	"github.com/withnic/keisou/internal/ui"
)

type testAdapter struct {
	id          model.ProviderID
	mu          sync.Mutex
	used        float64
	reset       *time.Time
	weekly      *float64
	weeklyReset *time.Time
}

func (a *testAdapter) ID() model.ProviderID { return a.id }

func (a *testAdapter) FetchUsage(_ context.Context, now time.Time) model.UsageSnapshot {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := model.UsageSnapshot{
		Provider: a.id, Available: true,
		Window5hUsedPct: a.used, Window5hResetAt: a.reset,
		WeeklyResetAt: a.weeklyReset,
	}
	if a.weekly != nil {
		w := *a.weekly
		s.WeeklyUsedPct = &w
	}
	if a.reset != nil && !a.reset.After(now) {
		s.Window5hUsedPct, s.Window5hResetAt = 0, nil
	}
	if a.weeklyReset != nil && !a.weeklyReset.After(now) {
		z := 0.0
		s.WeeklyUsedPct, s.WeeklyResetAt = &z, nil
	}
	return s
}

func (a *testAdapter) set(used float64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.used = used
}

func (a *testAdapter) setWeekly(used float64, reset *time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.weekly = &used
	a.weeklyReset = reset
}

type sleepRequest struct {
	duration time.Duration
	ctx      context.Context
	advance  chan struct{}
}

type testClock struct {
	mu     sync.Mutex
	now    time.Time
	sleeps chan sleepRequest
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Sleep(ctx context.Context, d time.Duration) error {
	advance := make(chan struct{})
	select {
	case c.sleeps <- sleepRequest{d, ctx, advance}:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-advance:
		return ctx.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}

func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for mock event")
		var zero T
		return zero
	}
}

func (c *testClock) fire(t *testing.T, req sleepRequest, want time.Duration) {
	t.Helper()
	if req.duration != want {
		t.Fatalf("sleep = %v, want %v", req.duration, want)
	}
	c.mu.Lock()
	c.now = c.now.Add(req.duration)
	c.mu.Unlock()
	close(req.advance)
}

type processCall struct {
	spec infra.ProcessSpec
	ctx  context.Context
	exit chan execution
}

type testRunner struct {
	calls chan processCall
}

func (r testRunner) Run(ctx context.Context, spec infra.ProcessSpec) (infra.ProcessResult, error) {
	if spec.Command == "git" {
		if spec.Mode != infra.OutputCapture || spec.Timeout <= 0 {
			return infra.ProcessResult{}, errors.New("git capture missing timeout")
		}
		switch strings.Join(spec.Args, " ") {
		case "status --porcelain":
			return infra.ProcessResult{Stdout: []byte(" M task.go\n")}, nil
		case "diff --stat":
			return infra.ProcessResult{Stdout: []byte(" task.go | 2 +-\n")}, nil
		default:
			return infra.ProcessResult{ExitCode: 1}, fmt.Errorf("unexpected git invocation: %v", spec.Args)
		}
	}
	exit := make(chan execution, 1)
	r.calls <- processCall{spec, ctx, exit}
	select {
	case outcome := <-exit:
		return outcome.result, outcome.err
	case <-ctx.Done():
		return infra.ProcessResult{ExitCode: -1, Tail: []byte("latest task output")}, ctx.Err()
	}
}

type fixture struct {
	engine                Engine
	clock                 *testClock
	calls                 chan processCall
	claude, codex, gemini *testAdapter
	opt                   RunOptions
}

func newFixture(t *testing.T) *fixture {
	cfg := config.Defaults()
	cfg.PollIntervalSec = 3
	cfg.SoftDrainGraceSec = 11
	cfg.AskTimeoutSec = 7
	clock := &testClock{now: time.Unix(1800000000, 0).UTC(), sleeps: make(chan sleepRequest, 16)}
	runner := testRunner{make(chan processCall, 12)}
	claude := &testAdapter{id: model.ProviderClaude, used: 10}
	codex := &testAdapter{id: model.ProviderCodex, used: 20}
	gemini := &testAdapter{id: model.ProviderGemini, used: 40}
	return &fixture{
		Engine{Config: cfg, Clock: clock, Runner: runner, Collector: provider.Collector{
			Adapters: []provider.ProviderAdapter{claude, codex, gemini},
		}},
		clock, runner.calls, claude, codex, gemini,
		RunOptions{Dir: t.TempDir(), Task: "Implement the test task"},
	}
}

func (f *fixture) start(t *testing.T) (context.CancelFunc, <-chan error) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- f.engine.Run(ctx, f.opt) }()
	return cancel, done
}

func (f *fixture) call(t *testing.T, agent string, mode infra.OutputMode) processCall {
	t.Helper()
	call := receive(t, f.calls)
	if call.spec.Command != agent || call.spec.Mode != mode {
		t.Fatalf("child = %s, mode %v, want %s, mode %v", call.spec.Command, call.spec.Mode, agent, mode)
	}
	return call
}

func (f *fixture) sleep(t *testing.T) sleepRequest {
	t.Helper()
	for {
		req := receive(t, f.clock.sleeps)
		if req.ctx.Err() == nil {
			return req
		}
	}
}

func (f *fixture) saved(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(f.opt.Dir, f.engine.Config.HandoffFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateHandoff(string(raw)); err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func assertExit(t *testing.T, done <-chan error, code int, want model.ErrorCode) {
	t.Helper()
	err := receive(t, done)
	if got := model.ExitCode(err); got != code {
		t.Fatalf("exit = %d, want %d: %v", got, code, err)
	}
	if want != "" {
		var e *model.Error
		if !errors.As(err, &e) || e.Code != want {
			t.Fatalf("error = %v, want %s", err, want)
		}
	}
}

func TestRelayCompletesNaturally(t *testing.T) {
	for _, draining := range []bool{false, true} {
		t.Run(fmt.Sprint(draining), func(t *testing.T) {
			f := newFixture(t)
			_, done := f.start(t)
			first := f.call(t, "claude", infra.OutputStream)
			if string(first.spec.Prompt) != f.opt.Task {
				t.Fatalf("initial prompt = %q", first.spec.Prompt)
			}
			if draining {
				f.claude.set(95.5)
				f.clock.fire(t, f.sleep(t), 3*time.Second)
				// Wait until the drain timer is armed, leaving its deadline pending.
				for f.sleep(t).duration != 11*time.Second {
				}
			}
			first.exit <- execution{}
			assertExit(t, done, 0, "")
			if _, err := os.Stat(filepath.Join(f.opt.Dir, "HANDOFF.md")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unexpected handoff: %v", err)
			}
		})
	}
}

const goodHandoff = `# HANDOFF — 継走（keisou）タスク引き継ぎ書

## 1. 元の指示・ゴール
Implement the test task with coverage for the routing and recovery behavior.

## 2. 完了した作業・変更ファイル一覧
The implementation is in task.go and a focused regression test covers its principal cases.

## 3. 現在の検証状態
The focused test passes.

## 4. 未完了の残タスクと次のアクション
Run all tests and review the final working tree.

## 5. 注意事項
Retain the existing changes.
`

func TestRelaySoftHandoff(t *testing.T) {
	for _, tc := range []struct {
		name      string
		response  execution
		expectLLM bool
	}{
		{"valid", execution{result: infra.ProcessResult{Stdout: []byte(goodHandoff)}}, true},
		{"429", execution{result: infra.ProcessResult{ExitCode: 1, Tail: []byte("HTTP 429")}}, false},
		{"low-quality", execution{result: infra.ProcessResult{Stdout: []byte("Error: sorry, the handoff could not be generated today.")}}, false},
		{"timeout", execution{err: context.DeadlineExceeded}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			_, done := f.start(t)
			first := f.call(t, "claude", infra.OutputStream)
			f.claude.set(95.5)
			f.clock.fire(t, f.sleep(t), 3*time.Second)
			for {
				req := f.sleep(t)
				if req.duration == 11*time.Second {
					f.clock.fire(t, req, 11*time.Second)
					break
				}
			}
			capture := f.call(t, "claude", infra.OutputCapture)
			if first.ctx.Err() == nil || capture.spec.Timeout != 30*time.Second || strings.Join(capture.spec.Args, " ") != "-p --output-format text --tools " {
				t.Fatalf("unexpected capture configuration: %+v", capture.spec)
			}
			for _, want := range []string{f.opt.Task, "git status --porcelain", "git diff --stat", "M task.go", "latest task output", "Do not edit files"} {
				if !bytes.Contains(capture.spec.Prompt, []byte(want)) {
					t.Errorf("capture missing %q", want)
				}
			}
			capture.exit <- tc.response
			next := f.call(t, "codex", infra.OutputStream)
			if prompt := string(next.spec.Prompt); !strings.Contains(prompt, "HANDOFF.md") || strings.Contains(prompt, f.opt.Task) || strings.Contains(prompt, "latest task output") {
				t.Fatalf("continuation prompt = %q", prompt)
			}
			saved := f.saved(t)
			if tc.expectLLM && saved != goodHandoff || !tc.expectLLM && !strings.Contains(saved, "latest task output") {
				t.Fatalf("unexpected handoff: %s", saved)
			}
			next.exit <- execution{}
			assertExit(t, done, 0, "")
		})
	}
}

func TestRelayQuotaAndUnexpectedExit(t *testing.T) {
	for _, event := range []string{"429", "429-zero", "hard", "hard-during-drain", "general", "soft-remeasurement"} {
		t.Run(event, func(t *testing.T) {
			f := newFixture(t)
			_, done := f.start(t)
			first := f.call(t, "claude", infra.OutputStream)
			switch event {
			case "429", "429-zero":
				code := 1
				if event == "429-zero" {
					code = 0
				}
				first.exit <- execution{result: infra.ProcessResult{ExitCode: code, Tail: []byte("HTTP 429")}}
			case "hard":
				f.claude.set(98.5)
				f.clock.fire(t, f.sleep(t), 3*time.Second)
			case "hard-during-drain":
				f.claude.set(95.5)
				f.clock.fire(t, f.sleep(t), 3*time.Second)
				for {
					req := f.sleep(t)
					if req.duration == 3*time.Second {
						f.claude.set(98.5)
						f.clock.fire(t, req, 3*time.Second)
						break
					}
				}
			case "general":
				first.exit <- execution{result: infra.ProcessResult{ExitCode: 2, Tail: []byte("syntax error")}}
				assertExit(t, done, 1, model.EInternal)
				return
			case "soft-remeasurement":
				f.claude.set(95.5)
				first.exit <- execution{result: infra.ProcessResult{ExitCode: 2}}
				capture := f.call(t, "claude", infra.OutputCapture)
				capture.exit <- execution{result: infra.ProcessResult{Stdout: []byte(goodHandoff)}}
			}
			next := f.call(t, "codex", infra.OutputStream)
			saved := f.saved(t)
			if event != "soft-remeasurement" && !strings.Contains(saved, "git status --porcelain") {
				t.Fatalf("local handoff missing git context: %s", saved)
			}
			next.exit <- execution{}
			assertExit(t, done, 0, "")
		})
	}
}

func TestRelayAsk(t *testing.T) {
	for _, event := range []string{"selection", "timeout", "EOF"} {
		t.Run(event, func(t *testing.T) {
			f := newFixture(t)
			f.engine.Config.DefaultMode = model.ModeAsk
			var diagnostic bytes.Buffer
			f.engine.Diagnostic = &diagnostic
			input := io.Reader(strings.NewReader("2\n"))
			want := "gemini"
			if event == "timeout" {
				reader, writer := io.Pipe()
				defer reader.Close()
				defer writer.Close()
				input, want = reader, "codex"
			} else if event == "EOF" {
				input = strings.NewReader("")
			}
			f.engine.Prompt = &ui.Prompt{Input: input}
			_, done := f.start(t)
			first := f.call(t, "claude", infra.OutputStream)
			first.exit <- execution{result: infra.ProcessResult{ExitCode: 1, Tail: []byte("429")}}
			if event == "timeout" {
				<-first.ctx.Done()
				f.clock.fire(t, f.sleep(t), 7*time.Second)
			}
			if event == "EOF" {
				assertExit(t, done, 1, model.EInternal)
				return
			}
			next := f.call(t, want, infra.OutputStream)
			next.exit <- execution{}
			assertExit(t, done, 0, "")
			for _, id := range []string{"claude", "codex", "gemini", "reset in"} {
				if !strings.Contains(diagnostic.String(), id) {
					t.Errorf("missing prompt diagnostic %q", id)
				}
			}
		})
	}
}

func TestRelayWaitsForReset(t *testing.T) {
	for _, event := range []string{"earliest", "unknown", "no-wait"} {
		t.Run(event, func(t *testing.T) {
			f := newFixture(t)
			for _, a := range []*testAdapter{f.claude, f.codex, f.gemini} {
				a.used = 99
			}
			wait := 60 * time.Second
			switch event {
			case "earliest":
				earliest := f.clock.Now().Add(20 * time.Second)
				later := earliest.Add(time.Hour)
				f.codex.reset, f.claude.reset = &earliest, &later
				wait = 50 * time.Second
			case "no-wait":
				f.opt.NoWait = true
			}
			_, done := f.start(t)
			if f.opt.NoWait {
				assertExit(t, done, 4, model.EAllQuotaExhausted)
				return
			}
			req := f.sleep(t)
			if event == "unknown" {
				f.codex.set(20)
			}
			f.clock.fire(t, req, wait)
			next := f.call(t, "codex", infra.OutputStream)
			next.exit <- execution{}
			assertExit(t, done, 0, "")
		})
	}
}

func TestRelayLimitAndArchive(t *testing.T) {
	f := newFixture(t)
	f.engine.Config.MaxHandoffCount = 1
	path := filepath.Join(f.opt.Dir, "HANDOFF.md")
	if err := os.WriteFile(path, []byte(goodHandoff), 0600); err != nil {
		t.Fatal(err)
	}
	_, done := f.start(t)
	first := f.call(t, "claude", infra.OutputStream)
	first.exit <- execution{result: infra.ProcessResult{ExitCode: 1, Tail: []byte("429")}}
	assertExit(t, done, 6, model.EMaxRelayExceeded)
	if saved := f.saved(t); !strings.Contains(saved, "1 / 1") || !strings.Contains(saved, f.opt.Task) {
		t.Fatalf("saved handoff = %q", saved)
	}
	dir := filepath.Join(f.opt.Dir, ".keisou", "handoffs")
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), "HANDOFF_seq1_") {
		t.Fatalf("archives = %v, %v", entries, err)
	}
	old, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if err != nil || string(old) != goodHandoff {
		t.Fatalf("archive mismatch: %q, %v", old, err)
	}
}

func TestRelayLockConflictAndCancellation(t *testing.T) {
	f := newFixture(t)
	cancel, done := f.start(t)
	first := f.call(t, "claude", infra.OutputStream)
	_, contender := f.start(t)
	assertExit(t, contender, 7, model.ELockConflict)
	cancel()
	assertExit(t, done, 130, model.EInterrupted)
	if first.ctx.Err() == nil {
		t.Fatal("agent context was not canceled")
	}
	if _, err := os.Stat(filepath.Join(f.opt.Dir, ".keisou", "run.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lock still exists: %v", err)
	}
	lock, err := infra.AcquireRunLock(context.Background(), f.opt.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestValidateHandoffRejectsPlaceholderSectionsAndApologies(t *testing.T) {
	if err := ValidateHandoff(goodHandoff); err != nil {
		t.Fatal(err)
	}
	for _, content := range []string{
		strings.Replace(goodHandoff, "Run all tests and review the final working tree.", "   ", 1),
		strings.Replace(goodHandoff, "## 2. ", "### 2. ", 1),
		"## 1. Goal\n" + strings.Repeat("Sorry, I cannot complete this request.\n", 6) +
			"## 2. Work\n> Error: rate limit exceeded.\n## 4. Next\n- Apologies, I cannot proceed.\n",
	} {
		if err := ValidateHandoff(content); err == nil {
			t.Errorf("accepted invalid handoff: %q", content)
		}
	}
}

func TestRelayNoFalsePositiveRateLimitOnProse(t *testing.T) {
	f := newFixture(t)
	_, done := f.start(t)
	first := f.call(t, "claude", infra.OutputStream)
	first.exit <- execution{result: infra.ProcessResult{
		ExitCode: 0,
		Tail:     []byte("Implemented rate limit handling and verified 429 test cases.\n"),
	}}
	assertExit(t, done, 0, "")
	if _, err := os.Stat(filepath.Join(f.opt.Dir, "HANDOFF.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("prose mentioning rate limit/429 falsely triggered handoff: %v", err)
	}

	// Valid LLM handoff describing a rate-limit task must pass StopOnOutput and ValidateHandoff.
	handoffText := strings.Replace(goodHandoff, "Implement the test task", "Implement rate limit handling for 429 tests", 1)
	if rateLimited([]byte(handoffText)) {
		t.Fatal("rateLimited falsely matched valid handoff prose")
	}
	if !rateLimited([]byte("HTTP 429 Too Many Requests")) || !rateLimited([]byte("429")) || !rateLimited([]byte("Error: rate limit exceeded")) {
		t.Fatal("rateLimited missed actual rate limit error signature")
	}
}

func TestRelayPromptFileResumeAndAgentPreference(t *testing.T) {
	// --prompt-file + preferred --agent already over threshold in auto mode switches to lowest candidate.
	f := newFixture(t)
	promptPath := filepath.Join(f.opt.Dir, "task.txt")
	if err := os.WriteFile(promptPath, []byte("Prompt from file\r\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f.claude.set(96)
	f.opt.Task = ""
	f.opt.PromptFile = promptPath
	f.opt.Agent = model.ProviderClaude
	_, done := f.start(t)
	next := f.call(t, "codex", infra.OutputStream)
	if string(next.spec.Prompt) != "Prompt from file" {
		t.Fatalf("prompt-file content = %q", next.spec.Prompt)
	}
	next.exit <- execution{}
	assertExit(t, done, 0, "")

	// --resume reads existing HANDOFF.md, passes continuation prompt, and preserves ## 1. on subsequent handoff.
	f2 := newFixture(t)
	handoffPath := filepath.Join(f2.opt.Dir, "HANDOFF.md")
	if err := os.WriteFile(handoffPath, []byte(goodHandoff), 0600); err != nil {
		t.Fatal(err)
	}
	f2.opt.Task = ""
	f2.opt.Resume = true
	_, done2 := f2.start(t)
	resumed := f2.call(t, "claude", infra.OutputStream)
	if !strings.Contains(string(resumed.spec.Prompt), handoffPath) {
		t.Fatalf("resume prompt missing path: %q", resumed.spec.Prompt)
	}
	resumed.exit <- execution{result: infra.ProcessResult{ExitCode: 1, Tail: []byte("HTTP 429")}}
	second := f2.call(t, "codex", infra.OutputStream)
	saved := f2.saved(t)
	if !strings.Contains(saved, "Implement the test task with coverage") {
		t.Fatalf("resumed handoff lost original ## 1. goal: %s", saved)
	}
	second.exit <- execution{}
	assertExit(t, done2, 0, "")
}

type nonGitRunner struct {
	tail string
}

func (r nonGitRunner) Run(_ context.Context, _ infra.ProcessSpec) (infra.ProcessResult, error) {
	return infra.ProcessResult{ExitCode: 128, Tail: []byte(r.tail)}, errors.New("exit status 128")
}

func TestManualHandoffNonGitAndGoalInheritance(t *testing.T) {
	for _, msg := range []string{"fatal: not a git repository", "fatal: git リポジトリではありません"} {
		dir := t.TempDir()
		cfg := config.Defaults()
		cfg.Providers[model.ProviderClaude] = model.ProviderConfig{Enabled: true, Command: "claude", DataDir: t.TempDir()}
		engine := Engine{
			Config: cfg,
			Clock:  &testClock{now: time.Unix(1800000000, 0).UTC(), sleeps: make(chan sleepRequest, 4)},
			Runner: nonGitRunner{tail: msg},
			Collector: provider.Collector{Adapters: []provider.ProviderAdapter{
				&testAdapter{id: model.ProviderClaude, used: 12.5},
			}},
		}
		if err := engine.ManualHandoff(context.Background(), dir, "", model.ProviderClaude); err != nil {
			t.Fatalf("ManualHandoff(%q) = %v", msg, err)
		}
		raw, err := os.ReadFile(filepath.Join(dir, "HANDOFF.md"))
		if err != nil {
			t.Fatal(err)
		}
		text := string(raw)
		if err := ValidateHandoff(text); err != nil {
			t.Fatalf("invalid manual handoff: %v", err)
		}
		if !strings.Contains(text, "（Gitリポジトリ外のため差分なし）") {
			t.Fatalf("missing non-git marker for %q:\n%s", msg, text)
		}
		// Second manual handoff without --task inherits ## 1. from existing HANDOFF.md.
		if err := os.WriteFile(filepath.Join(dir, "HANDOFF.md"), []byte(goodHandoff), 0600); err != nil {
			t.Fatal(err)
		}
		if err := engine.ManualHandoff(context.Background(), dir, "", model.ProviderClaude); err != nil {
			t.Fatalf("second ManualHandoff = %v", err)
		}
		raw2, err := os.ReadFile(filepath.Join(dir, "HANDOFF.md"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw2), "Implement the test task with coverage") {
			t.Fatalf("did not inherit ## 1. section:\n%s", raw2)
		}
	}
}

func TestRelayAskWaitAndQuit(t *testing.T) {
	// [q] aborts with E_INTERNAL (exit 1).
	f := newFixture(t)
	f.engine.Config.DefaultMode = model.ModeAsk
	f.engine.Prompt = &ui.Prompt{Input: strings.NewReader("Q\n")}
	_, done := f.start(t)
	first := f.call(t, "claude", infra.OutputStream)
	first.exit <- execution{result: infra.ProcessResult{ExitCode: 1, Tail: []byte("HTTP 429")}}
	assertExit(t, done, 1, model.EInternal)

	// [w] waits for depleted provider reset and then resumes.
	f2 := newFixture(t)
	f2.engine.Config.DefaultMode = model.ModeAsk
	resetAt := f2.clock.Now().Add(20 * time.Second)
	f2.claude.reset = &resetAt
	f2.engine.Prompt = &ui.Prompt{Input: strings.NewReader("W\n")}
	_, done2 := f2.start(t)
	first2 := f2.call(t, "claude", infra.OutputStream)
	first2.exit <- execution{result: infra.ProcessResult{ExitCode: 1, Tail: []byte("HTTP 429")}}
	<-first2.ctx.Done()
	for {
		req := f2.sleep(t)
		if req.duration == 50*time.Second {
			f2.clock.fire(t, req, 50*time.Second)
			break
		}
	}
	next2 := f2.call(t, "claude", infra.OutputStream)
	next2.exit <- execution{}
	assertExit(t, done2, 0, "")
}

func TestLiteralBlockTruncatesUTF8Cleanly(t *testing.T) {
	huge := strings.Repeat("あ", 30000)
	got := literalBlock(huge, "empty")
	if strings.ContainsRune(got, '\ufffd') || !strings.Contains(got, "（以降省略）") {
		t.Fatalf("literalBlock corrupted UTF-8 or missed truncation marker")
	}
}

func TestRelayEnforcesWeeklyQuota(t *testing.T) {
	// 1) Initial routing skips Claude when Weekly >= SoftThreshold even if 5h is low.
	f := newFixture(t)
	f.claude.setWeekly(99, nil)
	_, done := f.start(t)
	picked := f.call(t, "codex", infra.OutputStream)
	picked.exit <- execution{}
	assertExit(t, done, 0, "")

	// 2) Active watcher triggers hard handoff when Weekly hits HardThreshold while 5h stays low.
	f2 := newFixture(t)
	_, done2 := f2.start(t)
	first := f2.call(t, "claude", infra.OutputStream)
	f2.claude.setWeekly(98.5, nil)
	f2.clock.fire(t, f2.sleep(t), 3*time.Second)
	next := f2.call(t, "codex", infra.OutputStream)
	if first.ctx.Err() == nil {
		t.Fatal("claude was not stopped on weekly hard threshold")
	}
	next.exit <- execution{}
	assertExit(t, done2, 0, "")

	// 3) All providers blocked by Weekly wait for WeeklyResetAt (not shorter 5h reset).
	f3 := newFixture(t)
	short5h := f3.clock.Now().Add(20 * time.Second)
	weeklySoon := f3.clock.Now().Add(120 * time.Second)
	weeklyLater := f3.clock.Now().Add(time.Hour)
	for _, a := range []*testAdapter{f3.claude, f3.codex, f3.gemini} {
		a.used = 10
		a.reset = &short5h
	}
	f3.claude.setWeekly(99, &weeklyLater)
	f3.codex.setWeekly(99, &weeklySoon)
	f3.gemini.setWeekly(99, &weeklyLater)
	_, done3 := f3.start(t)
	f3.clock.fire(t, f3.sleep(t), 150*time.Second)
	resumed := f3.call(t, "codex", infra.OutputStream)
	resumed.exit <- execution{}
	assertExit(t, done3, 0, "")
}
