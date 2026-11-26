# 継走（keisou）

複数の AI コーディングエージェント（**Claude Code** / **Codex CLI** / **Gemini CLI** / **Antigravity CLI (`agy`・β)**）の残り枠をひと目で比較し、上限到達前に `HANDOFF.md` を書いて別エージェントへ引き継ぐ Go 製シングルバイナリ CLI です。

> [!WARNING]
> **本ツールは実験的（Experimental）なプロジェクトです。**
> 各エージェント CLI のアップデートでログ解析が動かなくなる可能性があります。また、サーバー側の正確な残量（`log_exact`）をそのまま取得できるのは現在 **Codex CLI のみ**（および公式 `statusLine` 連携時の Claude Code）で、それ以外はローカルログからの **推定値（`(est)`）** です。

---

## クイックスタート（最短これだけ）

設定ファイルを作らなくても、インストールしてすぐ動かせます。

```powershell
# 1. インストール（Go 1.23+・外部依存ゼロ / またはリポジトリで go build ./cmd/keisou）
go install github.com/withnic/keisou/cmd/keisou@latest

# 2. 各ツールの今の使用率を横並びで確認（ローカルログ読み取りのみ・API非消費）
keisou status

# 3. 監視つきでタスクを実行（上限に近づくとカレントディレクトリに ./HANDOFF.md を生成して別ツールへ自動リレー）
keisou run "テストを追加してリファクタリングする"
```

よく使うオプション：

```powershell
# 定期的に画面更新しながら残枠・5h推移グラフ・上限到達予測(ETA)・7日間ピークを監視
keisou status --watch

# 推移グラフと7日間まとめを1回だけ表示したいとき
keisou status --graph

# 切替先を自動ではなく対話で選びたいとき
keisou run --mode ask "タスク内容"

# 既存の ./HANDOFF.md から作業を再開したいとき（別ファイルを使う場合は --handoff <path>）
keisou run --resume

# 各コマンドの詳しいヘルプを見る
keisou help
keisou help run
```

---

## 各プロバイダの計測精度・制約

| プロバイダ | デフォルトの計測方式 | 精度・制約 |
| :--- | :--- | :--- |
| **Codex CLI** (`codex`) | **実測（`log_exact`）** | セッションログにサーバー返却の 5時間枠・週間枠・リセット時刻が直接記録されるため、追加設定なしで正確 |
| **Claude Code** (`claude`) | **推定（`window_calc`）** / `statusLine` 連携時は**実測** | 標準ログには残量 `%` が無いため既定では推定値（`(est)`）。公式 `statusLine` に `keisou statusline --agent claude` を設定すると 5h / 7d の実測値を取得可能（対話セッション応答時に更新） |
| **Gemini CLI** (`gemini`) | **推定（`window_calc`）** | ローカルチャットログからのトークン・回数推定（`(est)`）＋ `429` 検知。実残量と大きく乖離する場合あり |
| **Antigravity CLI** (`antigravity` / `agy`・β) | **推定（`window_calc`）** | `~/.gemini/antigravity-cli/brain/*/transcript.jsonl` からのトークン推定（`(est)`）＋ リセット基準日時アンカー ＋ `429` 検知。デスクトップ版／IDE 版／他端末の利用分は含まれず、実残量と乖離する場合あり |

---

## コマンド一覧

| コマンド | 用途 | 主な例 |
| :--- | :--- | :--- |
| `keisou status` | 各ツールの 5時間枠・週間枠・モデルを横並び表示（API非消費。履歴は `config.json` と同じ場所の `history.jsonl` に直近7日分を自動保存） | `keisou status --watch` / `keisou status --graph` / `keisou status --json` |
| `keisou run` | しきい値（5h / 週間）監視つきで実行し、上限到達時に引き継ぎリレー | `keisou run --agent claude --prompt-file task.txt` / `keisou run --no-wait "..."` |
| `keisou handoff` | 現在の `git` 差分と直近ログから手動で `HANDOFF.md` を生成（API非消費） | `keisou handoff --agent codex --task "残タスクの引き継ぎ"` |
| `keisou init` | デフォルトの `config.json` を生成し、CLI・ログパスを診断 | `keisou init` / `keisou init --force` |
| `keisou statusline` | Claude Code 等の公式 `statusLine` JSON から `rate_limits` をキャッシュ | `keisou statusline --agent claude` |
| `keisou help` | 全体またはサブコマンド別のヘルプを表示 | `keisou help run` / `keisou help handoff` |

### `keisou run` のリレー挙動

- **`HANDOFF.md` の場所**: 既定の読み書き先は**実行時のカレントディレクトリ直下の `./HANDOFF.md`** です（上書き前の古い版は `./.keisou/handoffs/` へ自動退避）。`keisou run --resume` や `keisou handoff` も既定でこの `./HANDOFF.md` を参照します。別パスを使いたい場合は `--handoff <path>` または `config.json` の `handoff_file` を指定してください。
- **選定順**: `--mode auto`（既定）では、有効（`enabled: true`）かつ実行可能なプロバイダのうち **実効使用率 `max(5h, Weekly)` が最も低いもの** を自動選択します。同率時は `config.json` の `priority` 昇順（既定: `claude: 1` → `codex: 2` → `gemini: 3` → `antigravity: 4`）で選ばれます。初回だけ固定したい場合は `--agent <name>` を指定します。
- **しきい値・`429` 時の動作**: 既定のソフトしきい値（`95%`）で猶予後に LLM 引き継ぎ生成、ハードしきい値（`98%`）または `429` エラー検知時は即座にローカル決定論生成で `./HANDOFF.md` を出力し、次エージェントへ切り替えます。作業ディレクトリ配下に `.keisou/run.lock` を作成して多重起動を防止します。
- **推定値（`(est)`）プロバイダでの注意**: 仮上限が実態とズレていると、余裕がある段階で早期停止したり、逆に `95%` 前にサーバー側 `429` で終了する場合があります（`429` 終了時は自動でローカル `HANDOFF.md` 生成・次エージェント切替へフォールバックします）。

---

## カスタマイズ（必要な人だけ）

### 1. 設定ファイル（`config.json` / JSONC 対応）

`keisou init` で設定ファイルを作成できます（保存先優先順: `--config <path>` → 環境変数 `KEISOU_CONFIG` → Windows `%APPDATA%\keisou\config.json` / Linux・macOS `~/.config/keisou/config.json`）。

`config.json` は **JSONC（`//`・`/* ... */` コメントと末尾カンマ）** に対応しており、変更したい項目だけ書けば残りはデフォルト値で補完されます。

```jsonc
{
  // しきい値到達時の動作: "auto"（自動切替）または "ask"（対話選択）
  "default_mode": "auto",
  "soft_threshold_pct": 95,
  "hard_threshold_pct": 98,
  "handoff_file": "HANDOFF.md", // 既定はカレントディレクトリ直下の ./HANDOFF.md
  "max_handoff_count": 10,

  "providers": {
    "claude": {
      "enabled": true,
      "priority": 1, // 同率使用率時の優先順位（小さいほど優先）
      "est_5h_token_limit": 2000000,
      "est_5h_req_limit": 100,
    },
    "codex": {
      "enabled": true,
      "priority": 2, // Codex はログ内の実測値（log_exact）を自動使用
      // Windows と WSL2 の両方で使う場合は、もう片方のログディレクトリを追加指定すると合算・最新値反映
      "extra_data_dirs": [
        // WSL2 側から Windows 側を読む例: "/mnt/c/Users/<name>/.codex"
        // Windows 側から WSL2 側を読む例: "\\\\wsl.localhost\\Ubuntu\\home\\<name>\\.codex"
      ],
    },
    "gemini": {
      "enabled": false, // 使わないツールは false にするとリレー候補から除外
      "priority": 3,
    },
    "antigravity": {
      "enabled": true,
      "priority": 4,
      // 以下は公式上限ではない仮の目安。status --json と実画面を見比べて調整してください
      "est_5h_token_limit": 80000000,
      "est_5h_req_limit": 0, // 0 でリクエスト回数比率を無効化（トークン基準のみ）
      "est_weekly_token_limit": 100000000,
      // 画面で確認したリセット基準日時（RFC3339）。過ぎると 5h / 7d 単位で自動繰り上げ
      "window_5h_reset_anchor": "2026-11-27T00:35:00+09:00",
      "weekly_reset_anchor": "2026-11-28T17:35:00+09:00",
    },
  },
}
```

### 2. Claude Code を実測値（`log_exact`）にする（任意）

`~/.claude/settings.json` の公式 `statusLine` に登録すると、Claude Code から渡される 5h / 7d の実測 `rate_limits` を `~/.claude/keisou-rate-limits.json` へキャッシュします。

```json
{
  "statusLine": {
    "type": "command",
    "command": "keisou statusline --agent claude"
  }
}
```

既存のステータスラインスクリプトから呼び出す場合は `--quiet` を付けると標準出力を抑止してキャッシュのみ更新します。なお、`statusLine` は主に対話セッション（`claude`）の応答時に更新されるため、非対話（`claude -p`）単体実行中などはローカル推定および `429` 検知で動作します。

### 3. Windows / WSL2 併用とバックグラウンド自動記録（任意）

- **設定・7日間履歴の共有**: `history.jsonl` は `config.json` と同じディレクトリに保存されるため、WSL2 側で `export KEISOU_CONFIG="/mnt/c/Users/<name>/AppData/Roaming/keisou/config.json"` を設定すると設定と履歴を共有できます。
- **複数ログディレクトリの合算**: Windows と WSL2 の両方で CLI を使う場合は、各プロバイダの `extra_data_dirs`（または環境変数 `KEISOU_<PROVIDER>_EXTRA_DIRS`、区切り文字は Windows `;` / Unix `:`）にもう片方のディレクトリを指定してください。未起動・未存在の追加ディレクトリは安全にスキップされます。主ディレクトリ単体の差し替えには `KEISOU_CLAUDE_DIR` / `KEISOU_CODEX_DIR` / `KEISOU_GEMINI_DIR` / `KEISOU_ANTIGRAVITY_DIR` を利用できます。
- **`keisou` を起動し忘れても 7日間グラフを育てる（`keisou status --quiet`）**:
  OS の定期実行で 5 分おきに `keisou status --quiet` を回しておくと、API 枠を消費せず `history.jsonl` だけ静かに蓄積され、好きなタイミングで `keisou status --graph` / `--watch` を開いた瞬間から推移・ETA・7日間ピークを確認できます。

```powershell
# Windows タスクスケジューラ（5分おきにバックグラウンド記録）
schtasks /Create /SC MINUTE /MO 5 /TN "keisou-history" /TR "keisou status --quiet" /F
```

```sh
# WSL2 / Linux / macOS（crontab -e に追記）
*/5 * * * * keisou status --quiet
```

---

## 開発・検証

### 統一検証

```text
go run ./scripts/verify.go
```

指示ファイルの SHA-256 一致、整形、静的解析、ビルド、テスト（CGO + C コンパイラ利用可能時は `-race`）を実行します。制限環境で外側の `go run` 自体がキャッシュに書き込めない場合は、先にワークスペース内のキャッシュを絶対パスで指定します（PowerShell 例）：

```powershell
$root = (Get-Location).Path
New-Item -Force -ItemType Directory "$root/.cache/go-build","$root/.cache/gopath","$root/.cache/tmp" | Out-Null
$env:GOCACHE = "$root/.cache/go-build"
$env:GOPATH = "$root/.cache/gopath"
$env:GOTMPDIR = "$root/.cache/tmp"
go run ./scripts/verify.go
```

### エージェント共通指示の同期

`AGENTS.md` が編集元です。チェックアウト後などにリンクが切れた場合は、`CLAUDE.md` と `GEMINI.md` を削除してハードリンクを作り直します。

```powershell
# PowerShell
New-Item -ItemType HardLink -Path CLAUDE.md -Target AGENTS.md
New-Item -ItemType HardLink -Path GEMINI.md -Target AGENTS.md
```

```sh
# Linux / WSL2 / macOS
ln AGENTS.md CLAUDE.md
ln AGENTS.md GEMINI.md
```

## ライセンス

MIT License — 詳細は [LICENSE](LICENSE) を参照してください。
