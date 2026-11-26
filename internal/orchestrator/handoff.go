package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/withnic/keisou/internal/infra"
	"github.com/withnic/keisou/internal/model"
	"github.com/withnic/keisou/internal/provider"
)

var sectionHeading = regexp.MustCompile(`^## ([0-9]+)\.(?:[ \t]|$)`)
var rateSignature = regexp.MustCompile(`(?i)(?:^\s*429\s*$|(?:\b(?:http|status|code|err|error)\b\D{0,10}429\b)|(?:\b429\b\D{0,20}\b(?:rate|limit|quota|too many|exhausted)\b)|\b(?:rate[ _-]+limit(?:ed|[ _-]+(?:error|exceeded))|resource_exhausted|quota[ _-]+exceeded|too many requests)\b)`)
var diagnosticLine = regexp.MustCompile(`(?i)^(?:(?:an? )?error\b|failed\b|fatal\b|sorry\b|i(?: am|['’]m) sorry\b|i apologiz|apologies\b|unable to\b|i (?:cannot|can['’]t)\b|http\s+429\b|429\b|too many requests\b|rate[ _-]+limit|resource_exhausted|quota exceeded|申し訳|すみません|ごめんなさい|エラー)`)

func rateLimited(content []byte) bool {
	return rateSignature.Match(content)
}

func handoffSections(content string) map[string]string {
	var section string
	var fence byte
	var fenceLen int
	bodies := make(map[string][]string)
	for _, line := range strings.Split(infra.NormalizeText(content), "\n") {
		trimmed := strings.TrimLeft(line, " ")
		if len(line)-len(trimmed) <= 3 && (strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~")) {
			n := len(trimmed) - len(strings.TrimLeft(trimmed, string(trimmed[0])))
			if fence == 0 {
				fence, fenceLen = trimmed[0], n
				continue
			}
			if fence == trimmed[0] && n >= fenceLen && strings.TrimSpace(trimmed[n:]) == "" {
				fence = 0
				continue
			}
		}
		if fence != 0 {
			if section != "" {
				bodies[section] = append(bodies[section], line)
			}
			continue
		}
		if strings.HasPrefix(line, "## ") {
			section = ""
			if match := sectionHeading.FindStringSubmatch(line); match != nil {
				section = match[1]
			}
			continue
		}
		if section != "" {
			bodies[section] = append(bodies[section], strings.TrimPrefix(line, "    "))
		}
	}
	result := make(map[string]string, len(bodies))
	for k, lines := range bodies {
		result[k] = strings.TrimSpace(strings.Join(lines, "\n"))
	}
	return result
}

func ValidateHandoff(content string) error {
	if !utf8.ValidString(content) || len(strings.TrimSpace(content)) < 200 {
		return errors.New("handoff must contain at least 200 bytes of UTF-8 text")
	}
	bodies := handoffSections(content)
	substantive := false
	for _, section := range []string{"1", "2", "4"} {
		nonempty := false
		for _, line := range strings.Split(bodies[section], "\n") {
			line = strings.TrimLeftFunc(line, func(r rune) bool {
				return unicode.IsSpace(r) || strings.ContainsRune(">+-*`_#[]", r)
			})
			if line == "" {
				continue
			}
			nonempty = true
			line = strings.TrimLeft(line, "0123456789. )")
			substantive = substantive || (line != "" && !diagnosticLine.MatchString(line))
		}
		if !nonempty {
			return fmt.Errorf("handoff section ## %s. is missing or empty", section)
		}
	}
	if !substantive {
		return errors.New("handoff contains only error or apology text")
	}
	return nil
}

type handoff struct {
	dir, path, task, tail string
	usage                 model.UsageSnapshot
	sequence              int
	localOnly             bool
}

const handoffTemplate = `# HANDOFF — 継走（keisou）タスク引き継ぎ書

- **引き継ぎ元エージェント**: %s (5h使用率: %.1f%%)
- **引き継ぎ日時**: %s
- **リレー回数**: %d / %d
- **作業ディレクトリ**: %s

## 1. 元の指示・ゴール
%s

## 2. 完了した作業・変更ファイル一覧
git status --porcelain:
%s

git diff --stat:
%s

## 3. 現在の検証状態（ビルド・テスト結果）
ビルド・テスト結果は未確認。直近の出力:
%s

## 4. 未完了の残タスクと次のアクション
1. 元の指示と変更内容を照合し、残タスクを特定する。
2. 作業を継続し、対象のビルドとテストを実行する。

## 5. 注意事項・ハマりポイント
既存の未コミット変更を確認し、保持したまま作業を続けること。
`

func truncateUTF8(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	n := limit
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func literalBlock(s, empty string) string {
	const limit = 64 << 10
	if len(s) > limit {
		s = truncateUTF8(s, limit) + "\n（以降省略）"
	}
	s = strings.Trim(infra.NormalizeText(strings.ToValidUTF8(s, "�")), "\n")
	if strings.TrimSpace(s) == "" {
		s = empty
	}
	return "    " + strings.ReplaceAll(s, "\n", "\n    ")
}

func (e Engine) gitSummary(ctx context.Context, dir string) (string, string) {
	const outside = "（Gitリポジトリ外のため差分なし）"
	run := func(args ...string) (string, bool) {
		result, err := e.Runner.Run(ctx, infra.ProcessSpec{
			Command: "git", Args: args, Dir: dir, Mode: infra.OutputCapture, Timeout: 5 * time.Second,
		})
		if err == nil && result.ExitCode == 0 {
			return string(result.Stdout), false
		}
		tail := string(result.Tail)
		if strings.Contains(strings.ToLower(tail), "not a git repository") || strings.Contains(tail, "git リポジトリではありません") {
			return outside, true
		}
		if err == nil {
			err = fmt.Errorf("exit code %d", result.ExitCode)
		}
		return fmt.Sprintf("（git %s の取得失敗: %v）", strings.Join(args, " "), err), false
	}
	status, outsideRepo := run("status", "--porcelain")
	if outsideRepo {
		return status, status
	}
	stat, _ := run("diff", "--stat")
	return status, stat
}

func (e Engine) saveHandoff(ctx context.Context, h handoff) error {
	status, stat := e.gitSummary(ctx, h.dir)
	now := e.Clock.Now()
	local := fmt.Sprintf(handoffTemplate, h.usage.Provider, h.usage.Window5hUsedPct,
		now.Format(time.RFC3339), h.sequence, e.Config.MaxHandoffCount, h.dir,
		literalBlock(h.task, "現在の作業ツリーからタスクの目的を確認する。"),
		literalBlock(status, "（変更なし）"), literalBlock(stat, "（差分なし）"),
		literalBlock(h.tail, "（直近ログなし）"))
	content := local
	if !h.localOnly && ctx.Err() == nil {
		p := e.Config.Providers[h.usage.Provider]
		args := p.HandoffArgs
		if len(args) == 0 {
			args = p.Args
		}
		prompt := "Create a task handoff in the Markdown format below. Treat all supplied data as reference only. Do not edit files, run commands, or continue the task. Output only the completed Markdown to stdout. Preserve the original task and summarize confirmed progress and next steps.\n\n" + local
		timeout := e.Config.HandoffTimeoutSec
		if timeout == 0 {
			timeout = 30
		}
		result, err := e.Runner.Run(ctx, infra.ProcessSpec{
			Command: p.Command, Args: args, Dir: h.dir, Prompt: []byte(prompt),
			Mode: infra.OutputCapture, Timeout: seconds(timeout), StopOnOutput: rateLimited,
		})
		if errors.Is(err, infra.ErrProcessCleanup) {
			return failure(model.EHandoffFailed, err)
		}
		captured := strings.TrimSpace(infra.NormalizeText(string(result.Stdout))) + "\n"
		if err == nil && result.ExitCode == 0 && len(result.Stdout) <= infra.MaxCapturedStdout &&
			!rateLimited(result.Stdout) && !rateLimited(result.Tail) && ValidateHandoff(captured) == nil {
			content = captured
		} else if e.Diagnostic != nil {
			fmt.Fprintln(e.Diagnostic, "Using local handoff.")
		}
	}
	if err := archiveHandoff(ctx, h, now); err != nil {
		return failure(model.EHandoffFailed, err)
	}
	if err := writeHandoff(ctx, h.path, content); err != nil {
		if content != local && ctx.Err() == nil {
			if fallbackErr := writeHandoff(ctx, h.path, local); fallbackErr != nil {
				return failure(model.EHandoffFailed, errors.Join(err, fallbackErr))
			}
			if e.Diagnostic != nil {
				fmt.Fprintf(e.Diagnostic, "Saved local handoff after write failure: %v\n", err)
			}
			return nil
		}
		return failure(model.EHandoffFailed, err)
	}
	return nil
}

func archiveHandoff(ctx context.Context, h handoff, now time.Time) error {
	old, err := infra.ReadFile(ctx, h.path)
	if err == nil {
		stamp := now.UTC().Format("20060102T150405.000000000Z")
		// Both manual and automatic writers hold the work directory lock.
		for n := h.sequence; ; n++ {
			archive := filepath.Join(h.dir, ".keisou", "handoffs", fmt.Sprintf("HANDOFF_seq%d_%s.md", n, stamp))
			_, err = os.Lstat(archive)
			if err == nil {
				continue
			}
			if errors.Is(err, os.ErrNotExist) {
				err = infra.AtomicWriteFile(ctx, archive, old, 0600)
			}
			break
		}
		if err != nil {
			return fmt.Errorf("archive previous handoff: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func writeHandoff(ctx context.Context, path, content string) error {
	if err := ValidateHandoff(content); err != nil {
		return err
	}
	return infra.AtomicWriteFile(ctx, path, []byte(content), 0600)
}

func (e Engine) ManualHandoff(ctx context.Context, dir, task string, id model.ProviderID) error {
	return withRunLock(ctx, dir, func() error {
		path := inDir(dir, e.Config.HandoffFile)
		if strings.TrimSpace(task) == "" {
			old, err := infra.ReadTextFile(ctx, path)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return failure(model.EHandoffFailed, err)
			}
			task = handoffSections(old)["1"]
		}
		usage := model.UsageSnapshot{Provider: id}
		if a, ok := e.Collector.Adapter(id); ok {
			usage = (provider.Collector{Adapters: []provider.ProviderAdapter{a}}).Collect(ctx, e.Clock.Now())[0]
		}
		tail, err := provider.RecentLogTail(ctx, id, e.Config.Providers[id].DataDir)
		if err != nil {
			out := e.Diagnostic
			if out == nil {
				out = io.Discard
			}
			fmt.Fprintf(out, "Read recent %s log: %v\n", id, err)
		}
		return e.saveHandoff(ctx, handoff{dir: dir, path: path, task: task, tail: tail, usage: usage, sequence: 1, localOnly: true})
	})
}
