# 02_data_modeling — 継走（keisou） [v3]

## 1. コアデータ構造（Go型定義仕様）

### 1.1 プロバイダ種別・モード定義

```go
package model

import "time"

type ProviderID string

const (
	ProviderClaude      ProviderID = "claude"
	ProviderCodex       ProviderID = "codex"
	ProviderGemini      ProviderID = "gemini"
	ProviderAntigravity ProviderID = "antigravity"
)

type RelayMode string

const (
	ModeAuto RelayMode = "auto" // しきい値到達時に自動引き継ぎ・自動切替
	ModeAsk  RelayMode = "ask"  // しきい値到達時にユーザーへ切替先を選択させる
)

type QuotaSource string

const (
	SourceLogExact    QuotaSource = "log_exact"   // ログ内のレート制限ヘッダ・明示残量から取得
	SourceWindowCalc  QuotaSource = "window_calc" // 直近5時間のローカルセッション消費量と上限設定から算出
	SourceUnavailable QuotaSource = "unavailable" // ログディレクトリ未検出・未使用またはパース不能
)
```

### 1.2 利用状況スナップショット（`UsageSnapshot`）

```go
type UsageSnapshot struct {
	Provider        ProviderID  `json:"provider"`
	PlanName        string      `json:"plan_name"`          // 例: "Claude Pro", "ChatGPT Plus", "AI Pro"
	Available       bool        `json:"available"`          // Enabled かつ CLI バイナリが実行可能（exec.LookPath 成功）か
	Source          QuotaSource `json:"source"`
	Window5hUsedPct float64     `json:"window_5h_used_pct"` // 0.0 〜 100.0（NaN/Inf禁止、クランプ済）
	Window5hResetAt *time.Time  `json:"window_5h_reset_at,omitempty"`
	WeeklyUsedPct   *float64    `json:"weekly_used_pct,omitempty"`
	WeeklyResetAt   *time.Time  `json:"weekly_reset_at,omitempty"`
	ActiveModel        string      `json:"active_model"`       // 直近セッションで使用中のモデル名
	IsFallbackModel    bool        `json:"is_fallback_model"`  // 上位枠切れで軽量モデルにフォールバック中か
	RecentTokens5h     int64       `json:"recent_tokens_5h"`   // 直近5時間ウィンドウ内の推定消費トークン数
	RecentTokensWeekly int64       `json:"recent_tokens_weekly,omitempty"` // 7日間サイクル内の推定消費トークン数（est_weekly_token_limit > 0 時）
	RecentReqs5h       int         `json:"recent_reqs_5h"`     // 直近5時間ウィンドウ内のリクエスト回数
	UpdatedAt          time.Time   `json:"updated_at"`
	ErrorMessage       string      `json:"error_message,omitempty"`
}
```

> **重要（`Available` と未使用ツールの扱い）**:
> `Available` は「設定上 `Enabled == true` かつ CLI バイナリが解決可能（`exec.LookPath` 成功）」を表す。CLI は導入済みだがまだ一度も使用していない（ログディレクトリが存在しない、または空である）場合、`Available = true`、`Source = SourceUnavailable`、`Window5hUsedPct = 0.0` とし、余力 100% のリレー候補として正常に選定可能とすること。

### 1.3 設定ファイルスキーマ（`Config`）とデフォルト値

設定ファイル（`config.json`）は JSONC（`//`・`/* ... */` コメントおよび末尾カンマ）を許容し、未記載フィールドはデフォルト値で補完する。

保存先パス（優先順）:

1. CLI フラグ `--config <path>`
2. 環境変数 `KEISOU_CONFIG`
3. OS標準パス:
   - Linux / WSL2 / macOS: `~/.config/keisou/config.json`（`XDG_CONFIG_HOME` 優先）
   - Windows: `%APPDATA%\keisou\config.json`

```go
type ProviderConfig struct {
	Enabled             bool     `json:"enabled"`
	Command             string   `json:"command"`                        // 実行バイナリ名/パス（例: "claude", "codex", "gemini", "agy"）
	Args                []string `json:"args"`                           // 通常タスク非対話実行時の基本引数（プロンプトは stdin 注入）
	HandoffArgs         []string `json:"handoff_args,omitempty"`         // 第1段階LLM引き継ぎ生成時の読み取り専用引数（空時は Args にフォールバック）
	DataDir             string   `json:"data_dir"`                       // ログ・状態ディレクトリ（空時はOS標準パスを自動解決）
	PlanName            string   `json:"plan_name"`
	Est5hTokenLimit     int64    `json:"est_5h_token_limit"`             // ローカル集計フォールバック時の5時間上限目安
	Est5hReqLimit       int      `json:"est_5h_req_limit"`               // ローカル集計フォールバック時の5時間リクエスト上限目安
	EstWeeklyTokenLimit int64    `json:"est_weekly_token_limit,omitempty"` // ローカル週間トークン推定時の上限目安（0時は無効）
	Window5hResetAnchor string   `json:"window_5h_reset_anchor,omitempty"` // 5時間リセット基準日時（RFC3339、5時間単位で自動繰り上げ）
	WeeklyResetAnchor   string   `json:"weekly_reset_anchor,omitempty"`    // 週間リセット基準日時（RFC3339、7日間単位で自動繰り上げ）
	Priority            int      `json:"priority"`                       // 同率使用率時の優先順位（小さい値ほど優先）
}

type Config struct {
	DefaultMode       RelayMode                     `json:"default_mode"`         // "auto" or "ask"
	SoftThresholdPct  float64                       `json:"soft_threshold_pct"`   // デフォルト: 95.0（ソフト停止・引き継ぎ開始）
	HardThresholdPct  float64                       `json:"hard_threshold_pct"`   // デフォルト: 98.0（API非消費で強制ローカル引き継ぎ）
	SoftDrainGraceSec int                           `json:"soft_drain_grace_sec"` // デフォルト: 15（ソフト検知時の自然終了待機猶予秒）
	HandoffTimeoutSec int                           `json:"handoff_timeout_sec"`  // デフォルト: 30（第1段階LLM引き継ぎ生成タイムアウト秒）
	PollIntervalSec   int                           `json:"poll_interval_sec"`    // デフォルト: 15
	AskTimeoutSec     int                           `json:"ask_timeout_sec"`      // askモード時の入力待ちタイムアウト（0=無制限、超過時はautoフォールバック）
	HandoffFile       string                        `json:"handoff_file"`         // デフォルト: "HANDOFF.md"
	MaxHandoffCount   int                           `json:"max_handoff_count"`    // 無限リレー防止上限（デフォルト: 10）
	Providers         map[ProviderID]ProviderConfig `json:"providers"`
}
```

#### デフォルト `ProviderConfig` 一覧（`keisou init` 生成値）

| ProviderID | Command | Args（通常非対話実行デフォルト） | HandoffArgs（引き継ぎ生成・編集禁止デフォルト） | PlanName | Est5hTokenLimit | Est5hReqLimit | EstWeeklyTokenLimit | Priority |
| :---- | :---- | :---- | :---- | :---- | ----: | ----: | ----: | ----: |
| `claude` | `claude` | `["-p", "--output-format", "text"]` | `["-p", "--output-format", "text", "--tools", ""]` | `Claude Pro` | `2000000` | `100` | `0` | `1` |
| `codex` | `codex` | `["exec"]` | `["exec", "--sandbox", "read-only", "--color", "never"]` | `ChatGPT Plus` | `2000000` | `100` | `0` | `2` |
| `gemini` | `gemini` | `["-p", ""]` | `["-p", ""]` | `AI Pro` | `4000000` | `200` | `0` | `3` |
| `antigravity` (β) | `agy` | `["-p", "--output-format", "text"]` | `["-p", "--output-format", "text", "--mode", "plan", "--disable-slash-commands"]` | `Antigravity` | `80000000` | `0` | `100000000` | `4` |

※プロンプト注入規約: 全プロバイダとも、タスクプロンプトおよび引き継ぎプロンプトはコマンドライン引数ではなく **`stdin` パイプ経由**で渡す（Windows `.cmd` ラッパー経由でのメタ文字破損および 8191 文字コマンドライン長制限を構造的に回避する）。`gemini` は `-p ""` とすることで `stdin` パイプ入力を非対話プロンプトとして安全に受け取る。

※バリデーション制約: `0 < SoftThresholdPct < HardThresholdPct <= 100`、`PollIntervalSec >= 1`、`SoftDrainGraceSec >= 0`、`HandoffTimeoutSec >= 5`、`AskTimeoutSec >= 0`、`MaxHandoffCount >= 1`、`HandoffFile` 非空、`DefaultMode` は `"auto"` または `"ask"`、秒数系フィールドは `time.Duration` 範囲内、`Window5hResetAnchor` / `WeeklyResetAnchor` は空または RFC3339 形式。CLI フラグ `--threshold` は `--soft-threshold` のエイリアスとし、上書き結果が `SoftThresholdPct >= HardThresholdPct` となった場合は `E_CONFIG_INVALID` で終了する。

---

## 2. 各ツールのローカルデータ解析仕様（パース規則）

各プロバイダのアダプタは `DataDir` 配下のファイルを読み取り専用（`os.Open`）で走査し、直近5時間（`now.Add(-5 * time.Hour)` 以降）のレコードのみを抽出する。

### 2.1 ファイル走査・フォーマット別リーダー使い分けと `mtime` スキューガード

- **ディレクトリ再帰走査（`filepath.WalkDir` 必須）**: Go 標準ライブラリの `filepath.Glob` は `**`（ダブルスター）を展開しないため、サブディレクトリ探索には必ず `filepath.WalkDir` を使用する。
- **`mtime` フィルタ安全マージン**: OS の遅延フラッシュや WSL2 `/mnt/c` のクロック・キャッシュ遅延による取りこぼしを防ぐため、`mtime` による事前スキップ判定には **10分の安全マージン** を設ける（`modTime.Before(now.Add(-5*time.Hour - 10*time.Minute))` のファイルのみスキップ）。ウィンドウ内外の厳密判定は必ず JSON 内部のタイムスタンプで行う。
- **逆順走査の早期打ち切り（Early Exit）**: 巨大 JSONL ファイルの高速読み取り（200ms 以内目標）のため、逆順リーダーで読み込んだ有効レコードのタイムスタンプが `now.Add(-5*time.Hour - 10*time.Minute)` より古い場合は、そのファイルの走査を打ち切る。
- **JSONL と整形済み JSON の明確な分離**:
  - **JSONL 形式**（Claude `projects/**/*.jsonl`, Codex `sessions/**/*.jsonl`, Gemini `telemetry.log`）: `internal/infra/fs.go` の **64KB 生バイト逆順チャンクリーダー**（10MB 最大行ガード付き）で末尾から走査する。
  - **整形済み JSON 形式**（Gemini `tmp/**/chats/*.json`, Claude `stats-cache.json`）: 1ファイル＝1つの複数行インデント JSON オブジェクトであるため、逆順行リーダーは**使用禁止**とする。ファイルサイズ上限（50MB）を検査した上で、`encoding/json.NewDecoder` によるストリーミング解析を行う。

### 2.2 プロバイダ別抽出ルールとスキーマドリフト耐性

| プロバイダ | デフォルト `DataDir` | 対象ファイル・フォーマット | 算出ロジック・重複排除・フォールバック |
| :---- | :---- | :---- | :---- |
| **Claude Code** | `~/.claude` | `projects/**/*.jsonl` (JSONL), `keisou-rate-limits.json` (JSON), `stats-cache.json` (JSON) | 1. 公式 `statusLine`（`keisou statusline --agent claude`）が記録した `keisou-rate-limits.json`（`rate_limits.five_hour` / `seven_day`、`used_percentage`）またはログ内レート制限メタデータ（`rate_limit`, `5h_utilization`, `resets_at` 等）があれば `SourceLogExact` を採用（`seven_day*` 系は週間枠へ分類）。 2. 未記録時は直近5時間内のアシスタントメッセージ `usage`（`input_tokens` + `output_tokens` + `cache_creation_input_tokens`、`cache_read_input_tokens` 除外）およびリクエスト数を集計し、`max(tokenRatio, reqRatio)` で算出（`SourceWindowCalc`）。 3. **重複排除**: 同一 `requestId` または `message.id` のストリーミング中間行は重複合算せず、最大/最終エントリのみ採用する（並列ターミナル・サブエージェント重複を防止）。 |
| **Codex CLI** | `~/.codex` | `sessions/**/*.jsonl` (JSONL), `log/*.log` (テキスト/JSONL) | 1. セッションイベント内の `rate_limits`（`primary.used_percent` → `Window5hUsedPct`, `primary.resets_at` → `Window5hResetAt`, `secondary.used_percent` → `WeeklyUsedPct`, `secondary.resets_at` → `WeeklyResetAt`）を抽出し `SourceLogExact` を採用（最新タイムスタンプの値を優先）。 2. 未記録時は直近5時間のトークン/ターン数（イベントID単位で重複排除）から `max(tokenRatio, reqRatio)` で算出（`SourceWindowCalc`）。 |
| **Gemini CLI** | `~/.gemini` | `tmp/**/chats/*.json` (整形JSON), `telemetry.log` (JSONL/テキスト) | 1. `chats/*.json` は `json.NewDecoder` で `messages` 配列を走査し、直近5時間内の `tokens`（`input` + `output` + `thoughts`）およびリクエスト数を集計（メッセージID単位で重複排除）。 2. `telemetry.log` およびチャット内エラーから `RESOURCE_EXHAUSTED` / `429`・リセット時刻・モデル切替（同一5時間枠内での `pro` 系 → `flash` 系への遷移、または `telemetry.log` のフォールバックイベント検出）を検出し `IsFallbackModel = true` を設定。 3. 明示残量がない場合は `max(tokenRatio, reqRatio)` を採用（`SourceWindowCalc`）。 |
| **Antigravity CLI (β)** | `~/.gemini/antigravity-cli` | `brain/**/transcript.jsonl` (JSONL), `keisou-rate-limits.json` (JSON) | 1. `brain/` 配下の `transcript.jsonl` のみを逆順走査（`transcript_full.jsonl` は二重計上防止のため除外）し、`type == "PLANNER_RESPONSE"` の `input_tokens + output_tokens`（`cache_read_tokens` 除外）とステップ数をファイル単位 `step_index` で重複排除して集計（`SourceWindowCalc`）。 2. `window_5h_reset_anchor` / `weekly_reset_anchor` 設定時は基準日時から次回リセット時刻を5時間／7日間単位で自動繰り上げし、5時間・週間それぞれのサイクル開始時刻以降のトークンを集計する（`est_weekly_token_limit > 0` で `WeeklyUsedPct` も推定）。 |

- **スキーマドリフト耐性**: すべての JSON パースは未知フィールドを許容（`DisallowUnknownFields` を使わない）し、主要キー名の揺れ（例: `timestamp` / `created_at` / `ts`、RFC3339文字列 / Unix秒 / Unixミリ秒）を柔軟に吸収するヘルパー経由でパースする。
- **レート制限エラーイベントの 100% 反映**: 全プロバイダ共通で、直近5時間内に `429` / `RESOURCE_EXHAUSTED` / `rate_limit_error` 等の明示的クォータ枯渇イベントが検出され、かつリセット時刻が未来（またはリセット時刻未記載で直近 10 分以内のイベント）である場合は `Window5hUsedPct = 100.0`、`Source = SourceLogExact` を採用する。

### 2.3 境界値・時間枠ガードルール

1. **期限切れリセット時刻の処理**:
   - ログから取得した `Window5hResetAt` が現在時刻 `now` 以前（`!resetAt.After(now)`）である場合、すでに5時間ウィンドウがリセット済みとみなし、`Window5hUsedPct` を `0.0` にリセットして `Window5hResetAt` を `nil` とする。
2. **ローリングウィンドウのリセット時刻推定**:
   - `SourceWindowCalc`（ローカル集計）で明示的な `ResetAt` がログに無い場合、直近5時間枠内の最古イベント時刻 `oldestEventIn5h` に `+ 5 * time.Hour` を加算した時刻を推定 `Window5hResetAt` としてセットする。直近5時間内にイベントが0件の場合は `Window5hUsedPct = 0.0`、`Window5hResetAt = nil` とする。
3. **ゼロ除算・浮動小数点サニタイズ**:
   - `Est5hTokenLimit <= 0` または `Est5hReqLimit <= 0` の場合、その指標による比率計算をスキップし `0.0` を返す。
   - すべての百分率は `math.IsNaN(v) || math.IsInf(v, 0)` を検査し、`[0.0, 100.0]` にクランプする。

---

## 3. 引き継ぎファイル（`HANDOFF.md`）スキーマと生成仕様

### 3.1 `HANDOFF.md` 標準フォーマット

```markdown
# HANDOFF — 継走（keisou）タスク引き継ぎ書

- **引き継ぎ元エージェント**: claude (5h使用率: 95.4%)
- **引き継ぎ日時**: 2026-11-25T23:15:00+09:00
- **リレー回数**: 1 / 10
- **作業ディレクトリ**: /path/to/repo

## 1. 元の指示・ゴール
（初回起動時に与えられたタスク概要）

## 2. 完了した作業・変更ファイル一覧
- `path/to/file1.go`: （変更内容の要約）
- `path/to/file2_test.go`: （追加したテスト内容）

## 3. 現在の検証状態（ビルド・テスト結果）
- ビルド: PASS / FAIL
- テスト: PASS / FAIL（失敗時は直近のエラー概要）

## 4. 未完了の残タスクと次のアクション
1. （次に実行すべき具体的な手順）
2. （残っているエッジケースや検証項目）

## 5. 注意事項・ハマりポイント
（試行して失敗したアプローチや避けるべき変更）
```

### 3.2 2段階引き継ぎ生成・品質検証（`ValidateHandoff`）・コンテキスト制御

1. **第1段階：ソフトしきい値到達時（`SoftThresholdPct <= used < HardThresholdPct`）**:
   - 稼働中プロセスをグレースフル・ドレイン（最大 `SoftDrainGraceSec` 秒待機）後に安全停止し、`git status --porcelain` / `git diff --stat` を取得する（作業ディレクトリが Git リポジトリでない場合はエラーにせず `（Gitリポジトリ外のため差分なし）` とする）。
   - 対象エージェントに対し、**元のタスク概要・`git status --porcelain`・`git diff --stat`・直近ログ末尾を明示的にプロンプトへ同梱**した上で、`HandoffArgs`（ファイル編集禁止・要約専念、タイムアウト `HandoffTimeoutSec` = デフォルト **30秒**）を用いて引き継ぎ生成専用プロンプトを1回だけ実行する。
   - エージェントにはファイル編集を行わせず、**`stdout` に標準フォーマットの Markdown 本文のみを出力させ、`keisou` が `stdout` 全文（最大 1MB 上限）をキャプチャ**する。
   - 実行中または出力に `429` / `rate limit` / `RESOURCE_EXHAUSTED` / `quota exceeded` を検知、またはタイムアウト・非0終了した場合は即座にキャンセルして第2段階へ降格する。
   - キャプチャした本文に対し、以下の **`ValidateHandoff(content string) error`** で構造品質を検証し、合格した場合のみ `keisou` が `HANDOFF.md` へアトミック書き込みする：
     - 空白除去後の実長が **200バイト以上** であること。
     - 必須3セクションの見出し（`## 1.`、`## 2.`、`## 4.`）がすべて含まれており、各セクション直下に空行以外の本文が1行以上存在すること。
     - 本文全体がレート制限エラー文や謝罪文のみで構成されていないこと（エラー系キーワード過多・定型謝罪のみを拒否）。
2. **第2段階：ハードしきい値到達時（`used >= HardThresholdPct`）・429終了時・第1段階失敗／品質検証不合格時**:
   - LLM呼び出しを行わず、`keisou` 本体がローカル情報（元のタスク概要、直近セッションログ末尾抜粋、`git status --porcelain`、`git diff --stat`、未コミット変更有無の警告）を組み立て、上記3.1の標準フォーマットかつ `ValidateHandoff` を確実に満たす `HANDOFF.md` を決定論的にアトミック書き込みする。
3. **手動生成（`keisou handoff`）**:
   - API を消費しないローカル決定論生成（第2段階と同一ビルダー）を実行し、既存ファイルアーカイブ＋`ValidateHandoff` 検証のうえアトミック書き込みする。
   - `--task` 未指定時は、既存 `HANDOFF.md` の `## 1.` セクションがあれば継承し、無ければ規定のプレースホルダ概要を補完して `ValidateHandoff` 要件を満たす。
4. **履歴アーカイブとリレーコンテキスト制御**:
   - 上書き前の既存 `HANDOFF.md` が存在する場合、`.keisou/handoffs/HANDOFF_seq{N}_{timestamp}.md` に退避保存する（`.keisou/` は `.gitignore` 対象）。
   - 多段リレー（2回目以降）でプロンプトが肥大化しないよう、次エージェントへ渡す引き継ぎ指示は常に最新の `HANDOFF.md` ファイルの読み込み＋未完了残タスクの継続指示のみとし、過去ログの全量連結は行わない。

---

## 4. リレー状態遷移とエラー・終了コード仕様

### 4.1 状態遷移（State Machine）

```
INIT -> SELECT_AGENT -> RUNNING
  - RUNNING 中に exit 0（レート制限シグネチャなし） -> COMPLETED (exit 0)
  - RUNNING 中に 非0終了 かつ（レート制限シグネチャあり OR 再計測 >= SoftThresholdPct）
      -> HANDOFF_GENERATING（429/Hard超過時はローカルフォールバック直行） -> ROUTING_NEXT
  - RUNNING 中に 非0終了（上記以外の一般エラー） -> ABORTED (E_INTERNAL, exit 1)
  - RUNNING 中にウォッチャーが SoftThresholdPct 到達を検知 -> DRAINING
      - DRAINING 中に exit 0（レート制限なし） -> COMPLETED (exit 0)
      - DRAINING 中に HardThresholdPct 到達 OR SoftDrainGraceSec 満了
          -> プロセスツリー停止 -> HANDOFF_GENERATING -> ROUTING_NEXT
  - RUNNING 中にウォッチャーが HardThresholdPct 到達を検知
      -> 即座にプロセスツリー停止 -> HANDOFF_GENERATING（ローカルフォールバック直行） -> ROUTING_NEXT

ROUTING_NEXT:
  - handoffCount >= MaxHandoffCount -> ABORTED (E_MAX_RELAY_EXCEEDED, exit 6; HANDOFF.md 保持)
  - 次候補あり（effectiveUsed = max(Window5hUsedPct, WeeklyUsedPct) < SoftThresholdPct から auto選定 / ask選択; askタイムアウト時はautoフォールバック, 非TTY/EOFはABORTED） -> SELECT_AGENT へループ
  - 全有効候補が SoftThresholdPct 以上（5時間枠または週間枠のいずれかが超過）:
      - --no-wait 指定 -> ABORTED (E_ALL_QUOTA_EXHAUSTED, exit 4)
      - 待機可 -> WAIT_RESET（各候補のブロック中リセット時刻 [Weekly超過時は WeeklyResetAt、両方超過時は遅い方] の最短 + 30秒、全nil時は max(PollIntervalSec, 60)秒） -> SELECT_AGENT へループ
```

### 4.2 終了コード・構造化エラーコード一覧

| 終了コード | エラーコード | 発生条件 | リカバリ・終了動作 |
| :---: | :---- | :---- | :---- |
| `0` | — | 正常完了（タスク完走・`status`/`init`/`handoff` 正常終了） | クリーン終了（ロック解放済） |
| `1` | `E_INTERNAL` / `E_INVALID_ARGS` | CLI引数不正、`init` 既存ファイル保護（`--force` なし）、非TTYでの `ask` 入力不可、予期せぬI/O・実行時エラー | 詳細を `stderr` に出力しロック解放後に終了 |
| `2` | `E_CONFIG_INVALID` | `config.json` の構文不正、または `SoftThresholdPct >= HardThresholdPct` 等の論理矛盾 | 具体的な修正箇所を `stderr` に出力し終了 |
| `3` | `E_NO_AVAILABLE_PROVIDER` | 全プロバイダのCLIバイナリが未インストール、または全無効化 | `keisou init` 実行を案内し終了 |
| `4` | `E_ALL_QUOTA_EXHAUSTED` | 全有効プロバイダが `SoftThresholdPct` 以上に達している | `run` モード時は最短 `ResetAt` まで自動待機（`--no-wait` 指定時は終了コード `4` で終了） |
| `5` | `E_HANDOFF_FAILED` | ソフト引き継ぎとローカルフォールバック双方の書き込み失敗（ディスク満杯・権限不足等） | 一時ファイルをクリーンアップし終了 |
| `6` | `E_MAX_RELAY_EXCEEDED` | リレー回数が `MaxHandoffCount` に到達（無限ループ防止） | 最新の `HANDOFF.md` を保持したまま安全停止 |
| `7` | `E_LOCK_CONFLICT` | 同一作業ディレクトリで別の `keisou run` プロセスがすでに稼働中（`.keisou/run.lock` 競合） | 二重実行によるファイル破損を防ぐためエラー出力し終了 |
| `130` | `E_INTERRUPTED` | `SIGINT` / `SIGTERM` によるユーザー中断 | 子プロセスツリーを停止し、ロックと一時ファイルを削除して終了 |
