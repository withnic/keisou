# 03_architecture — 継走（keisou） [v3]

## 1. システムアーキテクチャとコンポーネント分割

**継走（keisou）** は依存関係を明確に分離したクリーンアーキテクチャ（ヘキサゴナル構成）を採用する。ドメインロジック（しきい値判定・ルーティング・状態遷移）は OS や外部CLIの実行詳細に一切依存しない。

```
+-------------------------------------------------------------------+
|                        CLI Layer (cmd/keisou)                     |
| status / run / handoff / statusline / init | Flag & Exit Code Map |
+---------------------------------+---------------------------------+
                                  |
                                  v
+-------------------------------------------------------------------+
|                   Application / Orchestrator Layer                |
|  - RelayEngine (状態遷移・しきい値監視・ドレイン/停止制御)         |
|  - Router (auto最適選定 / ask対話選択 / 最短リセット待機計算)     |
|  - HandoffManager (30s LLM引き継ぎ + ValidateHandoff + ローカル)   |
+----------------+---------------------------------+----------------+
                 |                                 |
                 v                                 v
+---------------------------------+ +-------------------------------+
|    Provider Adapter Layer       | |   Infra / OS Adapter Layer    |
|  - ClaudeAdapter (~/.claude)    | |  - AtomicFileWriter / Readers |
|  - CodexAdapter  (~/.codex)     | |  - LockManager (.keisou/lock) |
|  - GeminiAdapter (~/.gemini)    | |  - ProcessRunner (Job/PGID)   |
|  - AntigravityAdapter (β)       | |  - PathResolver (Win/WSL2)    |
+---------------------------------+ |  - Clock / TerminalSequences  |
                                    +-------------------------------+
```

---

## 2. ディレクトリ構成

リポジトリ直下に `AGENTS.md` を単一ソース（Single Source of Truth）として配置し、`CLAUDE.md` および `GEMINI.md` をハードリンクとして共有する（乖離検知は `scripts/verify.go` で自動化）。

```
keisou/
├── AGENTS.md                        # 全エージェント共通マスター定義（実体）
├── CLAUDE.md                        # AGENTS.md へのハードリンク
├── GEMINI.md                        # AGENTS.md へのハードリンク
├── .gitignore                       # .cache/, .keisou/, keisou, keisou.exe 等を除外
├── README.md                        # セットアップ手順（ハードリンク作成・ビルド方法）
├── go.mod                           # Go 1.23+ モジュール定義（標準ライブラリのみ）
├── .claude/
│   └── settings.json                # Claude Code 設定（Stop フック: scripts/verify.go）
├── .codex/
│   └── config.toml                  # Codex CLI 設定（workspace-write / network_access=false）
├── .gemini/
│   └── settings.json                # Gemini CLI / Antigravity 設定（AGENTS.md / AfterAgent）
├── scripts/
│   └── verify.go                    # クロスOS検証スクリプト（gofmt/vet/build/test/AGENTS同期）
├── docs/
│   ├── 01_project_overview.md
│   ├── 02_data_modeling.md
│   └── 03_architecture.md
├── prompts/
│   ├── phase1.md                    # Phase 1: 骨格・データモデル・設定・OS基盤・verify.go
│   ├── phase2.md                    # Phase 2: 3プロバイダログパーサ・statusコマンド
│   └── phase3.md                    # Phase 3: リレーオーケストレータ・引き継ぎ制御・結合テスト
├── cmd/
│   └── keisou/
│       ├── main.go                  # エントリーポイント（シグナル捕捉・終了コード制御・init）
│       ├── status.go                # status サブコマンド（--json / --watch / --interval）
│       ├── relay.go                 # run / handoff サブコマンド CLI 接続
│       └── statusline.go            # statusline サブコマンド（Claude 等公式 rate_limits キャッシュ）
└── internal/
    ├── model/
    │   ├── types.go                 # UsageSnapshot, Config, ProviderID, RelayMode 等
    │   └── errors.go                # 構造化エラーコード・終了コードマップ定義
    ├── config/
    │   ├── loader.go                # 設定読込・デフォルト値補完・バリデーション
    │   └── loader_test.go
    ├── provider/
    │   ├── provider.go              # ProviderAdapter インターフェース・Collector・スキーマ吸収ヘルパ
    │   ├── usage.go                 # 5時間／週間ウィンドウ集計・重複排除・アンカー繰り上げ
    │   ├── claude.go                # Claude Code ログ・statusline キャッシュ解析（重複排除付）
    │   ├── codex.go                 # Codex CLI セッション・レート制限解析
    │   ├── gemini.go                # Gemini CLI チャットJSONストリーミング・テレメトリ解析
    │   ├── antigravity.go           # Antigravity CLI (β) transcript.jsonl 解析
    │   ├── recent.go                # 手動 handoff 用の直近セッション末尾抜粋リーダー
    │   ├── provider_test.go         # フィクスチャJSONL/JSONを用いた境界値テスト
    │   └── testdata/                # Claude / Codex / Gemini / Antigravity テストフィクスチャ
    ├── orchestrator/
    │   ├── router.go                # 次エージェント選定（5h+Weekly評価 / auto / ask / 待機時間算出）
    │   ├── handoff.go               # HANDOFF.md 生成・ValidateHandoff・ローカルフォールバック
    │   ├── engine.go                # 監視ループ・ドレイン停止制御・ロック連携メインエンジン
    │   └── engine_test.go           # モックプロバイダを用いたリレー遷移結合テスト
    ├── ui/
    │   ├── table.go                 # status 横並びASCIIテーブル・プログレスバー・(est)描画
    │   └── prompt.go                # ask モード用対話選択プロンプト（タイムアウト付・io.Reader注入可）
    └── infra/
        ├── fs.go                    # アトミック書き込み・UTF-8安全64KB逆順行/JSONLリーダ・JSON上限リーダ
        ├── lock.go                  # 作業ディレクトリ単位の多重起動防止ロック (.keisou/run.lock)
        ├── lock_unix.go             # POSIX PID生存確認 (syscall.Kill(pid, 0))
        ├── lock_windows.go          # Windows PID生存確認 (OpenProcess + GetExitCodeProcess)
        ├── proc.go                  # 共通ランナー定義（非TTY stdin固定・ストリーミング/キャプチャ2モード）
        ├── proc_unix.go             # Linux/WSL2用プロセスグループ終了 (Setpgid / SIGTERM->SIGKILL)
        ├── proc_windows.go          # Windows用 JobObject (KILL_ON_JOB_CLOSE) + taskkill /T /F
        ├── terminal_unix.go         # Unix 向けターミナルエスケープ制御（no-op）
        ├── terminal_windows.go      # Windows コンソール仮想端末シーケンス有効化・復元
        ├── path.go                  # Windows / WSL2 / Linux パス正規化・~展開・ホーム/設定パス解決
        └── clock.go                 # Clock インターフェース（SystemClock / SleepAsync / モック注入）
```

---

## 3. OS差異吸収・安全設計

### 3.1 Windows / WSL2 クロスプラットフォームとパス解決 (`internal/infra/path.go`)

- 設定ファイルパス解決順: CLI `--config` → 環境変数 `KEISOU_CONFIG` → OS標準パス（`os.UserConfigDir()` 配下の `keisou/config.json`。Linux/WSL2 では `XDG_CONFIG_HOME` を優先し未設定時は `~/.config/keisou/config.json`）。
- プロバイダデータディレクトリ解決: 環境変数 `KEISOU_CLAUDE_DIR` / `KEISOU_CODEX_DIR` / `KEISOU_GEMINI_DIR` / `KEISOU_ANTIGRAVITY_DIR` による明示オーバーライド → `ProviderConfig.DataDir`（先頭 `~` 展開対応）→ `os.UserHomeDir()` 配下の既定ディレクトリ（`.claude` / `.codex` / `.gemini` / `.gemini/antigravity-cli`）。これにより WSL2 から `/mnt/c/Users/<name>/.claude` を参照するユースケースに対応する。
- すべてのファイル読み書きは UTF-8（BOMなし）/ LF（`\n`）に正規化する。

### 3.2 サブプロセス制御・TTY競合防止・プロセスツリー確実終了 (`internal/infra/proc*.go`)

- **TTY競合（`SIGTTIN` / `SIGTTOU`）および Windows 引数破損の構造的防止**:
  - Unix 系で `Setpgid: true` を設定すると子プロセスがフォアグラウンドプロセスグループから外れるため、子プロセスに端末の `os.Stdin` を直接継承させない。
  - Windows では `claude` / `codex` / `gemini` が npm 製の `.cmd` ラッパーとして実行されるケースが多く、コマンドライン引数で長文プロンプトを渡すと `cmd.exe` のメタ文字解釈や 8191 文字制限で破損する。
  - そのため `ProcessRunner` は常にプロンプト文字列の `bytes.NewReader` または `os.DevNull`（`/dev/null`）を `cmd.Stdin` に接続する。
  - `--mode ask` のユーザー対話入力は、子プロセスが完全に終了・回収された後に親プロセス（フォアグラウンド PGID）が注入可能な `io.Reader`（本番は `os.Stdin`）から読み取る。
- **パイプ接続時のブロックバッファリング対策と 2つの出力モード**:
  - 非対話サブプロセス起動時、環境変数に `CI=true`, `NO_COLOR=1`, `FORCE_COLOR=0`, `PYTHONUNBUFFERED=1` を注入し、対話プロンプト待機や出力バッファリングを抑止する。
  - `ProcessRunner` は用途に応じて以下の2モードをサポートする：
    1. **通常実行（ストリーミングモード）**: 子プロセスの `stdout` / `stderr` を、`keisou` の `stdout`（`--json` 等）を汚染しないよう、行単位でフラッシュするストリーミングライター経由で `keisou` の `stderr` へリアルタイム転送しつつ、同時に末尾N行（例: 200行）をリングバッファに保持して 429 検知およびローカルハンドオフ要約に利用する。
    2. **引き継ぎ生成実行（キャプチャモード）**: 子プロセスの `stdout` を最大 1MB 上限でメモリへ全文キャプチャして `ValidateHandoff` へ返し、`stderr` のみ診断ログ（および 429 検知用リングバッファ）として捕捉する。
- **Unix 系（Linux / WSL2: `proc_unix.go`）**:
  - `SysProcAttr{Setpgid: true}` で起動し、停止時は `syscall.Kill(-pgid, syscall.SIGTERM)` を送信して **3秒待機** したのち、未終了であれば `syscall.Kill(-pgid, syscall.SIGKILL)` でプロセスグループ全体を確実に回収する。
- **Windows 系（`proc_windows.go`）**:
  - Go 標準ライブラリ（`syscall.NewLazyDLL("kernel32.dll")` のみ、外部依存ゼロ）で **JobObject**（`CreateJobObjectW` + `SetInformationJobObject` による `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`）を作成する。
  - `CREATE_NEW_PROCESS_GROUP` で子プロセスを起動し、JobObject へ `AssignProcessToJobObject` で紐づける（これにより `keisou` 本体が異常終了しても OS カーネルが孫プロセスまで自動回収する）。
  - 明示的な停止要求時は `taskkill /PID <pid> /T /F`（タイムアウト 5秒）を実行した上で JobObject ハンドルを `CloseHandle` し、孫プロセス残存を完全に防ぐ。
- **親プロセス中断時のクリーンアップ**:
  - 親プロセス（`keisou`）自身が `SIGINT` / `SIGTERM` を受けた際も、実行中の子プロセスグループを確実に停止し、`.keisou/run.lock` と一時ファイルを削除してから終了コード `130` で終了する。

### 3.3 安全なファイルI/Oと巨大ログファイルの高速・省メモリ読み取り (`internal/infra/fs.go`)

- **アトミック書き込み (`AtomicWriteFile`)**:
  - 親ディレクトリを `os.MkdirAll` で確保したのち、同一ディレクトリ内に一時ファイル（`os.CreateTemp`）を作成して書き込み、`Sync()` と `Close()` を**必ず完了させてから** `os.Rename` を行う（Windows ではオープン中のファイルを `os.Rename` すると失敗するため必須）。エラー時は `defer` で一時ファイルを削除する。
- **JSONL 専用 64KB 逆順チャンクリーダー（UTF-8 マルチバイト安全設計）**:
  - ファイル全体を `os.ReadFile` せず、ファイル末尾から 64KB 単位で生バイト列（`[]byte`）を逆順シークして読み込む。
  - **UTF-8 分断対策**: チャンク途中で UTF-8 マルチバイト文字（日本語等）が分断されても破損しないよう、`string` 変換を行わず **生バイト列 `[]byte` のまま `0x0A`（`\n`）のみを区切り** として分割する（UTF-8 の仕様上、マルチバイト文字の後続バイト `0x80..0xBF` に `0x0A` は出現しない）。前チャンク先頭の未完了断片（`carry []byte`）を次チャンク末尾に連結し、完全な1行が確定してから行末 `\r` を除去してコールバックへ渡す（コールバックが `false` を返した時点で早期打ち切り可能とする）。
  - **10MB 超長大行ガード**: `carry` が 10MB を超えた場合は `discardUntilNewline` 状態へ遷移し、次の `\n` が見つかるまでバッファを破棄して OOM を防止する。
  - 破損した不完全な JSON 行（書き込み途中の最終行など）はパニックせずスキップして直前の有効行を採用する。
- **整形済み JSON 専用ストリーミングリーダー（Gemini `chats/*.json` / Claude `stats-cache.json`）**:
  - 複数行インデント JSON ファイルには逆順行リーダーを使用せず、ファイルサイズ上限（50MB）を検証した上で `os.Open` + `encoding/json.NewDecoder` でストリーミング解析する。

### 3.4 多重起動防止・並行制御・テスト用時刻抽象 (`internal/infra/lock*.go`, `internal/infra/clock.go`, `internal/orchestrator/engine.go`)

- `keisou run` 開始時に `os.MkdirAll(".keisou", 0755)` のうえで `.keisou/run.lock`（PID と開始時刻の JSON を記録）を `os.OpenFile(..., os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)` で排他作成し、同一ディレクトリでの二重リレー起動による `HANDOFF.md` 競合を防止する。
- ロックファイルが既に存在する場合:
  - 書き込み直後のレースによる空ファイル／途中JSON誤削除を防ぐため、JSON パース失敗時は短時間リトライ（50ms × 3回）を行う。
  - 記録された PID の生存確認を OS 別実装（Unix: `syscall.Kill(pid, 0)`、Windows: `OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION, ...)` + `GetExitCodeProcess`、なお `ERROR_ACCESS_DENIED` は生存中と判定）で行い、プロセスが存在しない stale lock であれば安全に削除して再取得する。生存していれば `E_LOCK_CONFLICT`（終了コード `7`）を返す。
- `status` 実行時は3プロバイダの解析を `sync.WaitGroup` + チャネルで並行実行し、各プロバイダ単位にタイムアウト（2秒）を設定する。
- `run` 実行時はウォッチャーゴルーチンとメインプロセス制御間の状態共有を `context.Context` と `sync.Mutex` で保護し、データ競合ゼロを検証する。
- `Clock` インターフェース（`Now() time.Time`, `Sleep(ctx context.Context, d time.Duration) error`）を `orchestrator` へ注入し、リセット待機・ドレイン猶予・ポーリングを実時間待ちなしで決定論的にテスト可能にする。

---

## 4. テスト戦略とクロスOS検証 (`scripts/verify.go`)

- **クロスOS統一検証スクリプト (`scripts/verify.go`)**:
  - **オフライン・サンドボックス絶対パス保証**: 実行時に `os.Getwd()` からリポジトリ内キャッシュの**絶対パス**（`filepath.Join(wd, ".cache", "go-build")` / `gopath` / `tmp`）を算出し、`GOCACHE` / `GOPATH` / `GOTMPDIR` が未設定・相対パス・書き込み不可の場合に `os.MkdirAll` で作成して子プロセス環境へ注入する（Go は相対パスの `GOCACHE`/`GOPATH` を拒否するため必須）。
  - `AGENTS.md` / `CLAUDE.md` / `GEMINI.md` の SHA-256 ハッシュ完全一致検証（ハードリンク切断検知。不一致時は同期手順を `stderr` へ出力し終了コード `2` で失敗）。
  - `gofmt -s -w`（`.cache` 等を巻き込まないよう `./cmd`, `./internal`, `./scripts` 等の存在する Go ソースディレクトリを対象とする）、`go vet ./...`。
  - `go build -o <tempDir>/keisou(.exe) ./cmd/keisou`（一時ディレクトリへビルド出力し、リポジトリルートに実行ファイル残骸を残さない）。
  - `go test`: `CGO_ENABLED=1` かつ C コンパイラ（`gcc`/`clang`）が利用可能な環境では `go test -race ./...` を実行し、Windows 非CGO環境では `go test ./...` へ安全フォールバック（警告出力）してフック無限ループを防ぐ。失敗時は必ず終了コード `2` で終了する。
- **単体テスト（モック＆フィクスチャ駆動）**:
  - `internal/provider/testdata/` に Claude Code / Codex CLI / Gemini CLI の実ログ形式を模した JSONL および整形済み JSON フィクスチャ（正常・境界値 94.9%/95.0%/98.0%/100%・64KB境界を跨ぐ日本語マルチバイト文字・10MB超長大行・重複 `requestId`・破損JSON行・0バイトファイル・期限切れ/未来リセット時刻）を配置し、パーサ精度を検証する。
- **結合テスト（擬似エージェントリレー検証）**:
  - `ProviderAdapter`、`ProcessRunner`、`Clock`、および対話入力 `io.Reader` をモック化し、「正常完走 → ソフト到達（ドレイン＋自然終了 / LLMハンドオフ＋`ValidateHandoff`）→ 429途中異常終了・低品質出力時のローカルフォールバック降格 → ハード到達 → `ask` モード選択・タイムアウトフォールバック → 全ツール枯渇待機（`ResetAt` 有/全`nil`）→ リレー上限到達 → `.keisou/run.lock` 競合検知 → シグナル中断クリーンアップ」の一連のシナリオを高速（1秒未満）で自動検証する。
