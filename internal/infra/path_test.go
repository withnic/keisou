package infra

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/withnic/keisou/internal/model"
)

func TestPathExpansionAndProviderPriority(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	got, err := ConfigPath("~/keisou/custom.json")
	want := filepath.Join(home, "keisou", "custom.json")
	if err != nil || got != want {
		t.Fatalf("ConfigPath(~) = %q, %v; want %q", got, err, want)
	}
	got, err = ConfigPath("~")
	if err != nil || got != filepath.Clean(home) {
		t.Fatalf("ConfigPath(bare ~) = %q, %v; want %q", got, err, filepath.Clean(home))
	}
	t.Setenv("KEISOU_CONFIG", `~\keisou\from-env.json`)
	got, err = ConfigPath("")
	want = filepath.Join(home, "keisou", "from-env.json")
	if err != nil || got != want {
		t.Fatalf("ConfigPath(env ~\\) = %q, %v; want %q", got, err, want)
	}
	t.Setenv("KEISOU_CLAUDE_DIR", "")
	got, err = ProviderDataDir(model.ProviderClaude, "~/.claude-alt")
	want = filepath.Join(home, ".claude-alt")
	if err != nil || got != want {
		t.Fatalf("ProviderDataDir(configured ~) = %q, %v; want %q", got, err, want)
	}
	envDir := filepath.Join(t.TempDir(), "override")
	t.Setenv("KEISOU_CLAUDE_DIR", envDir)
	got, err = ProviderDataDir(model.ProviderClaude, "~/.claude-alt")
	if err != nil || got != envDir {
		t.Fatalf("ProviderDataDir(env) = %q, %v; want %q", got, err, envDir)
	}
	if _, err := ProviderDataDir(model.ProviderID("unknown"), ""); err == nil {
		t.Fatal("ProviderDataDir(unknown) unexpectedly succeeded")
	}
	t.Setenv("KEISOU_ANTIGRAVITY_DIR", "")
	got, err = ProviderDataDir(model.ProviderAntigravity, "")
	want = filepath.Join(home, ".gemini", "antigravity-cli")
	if err != nil || got != want {
		t.Fatalf("ProviderDataDir(antigravity default) = %q, %v; want %q", got, err, want)
	}
	agyEnv := filepath.Join(t.TempDir(), "agy-override")
	t.Setenv("KEISOU_ANTIGRAVITY_DIR", agyEnv)
	got, err = ProviderDataDir(model.ProviderAntigravity, "")
	if err != nil || got != agyEnv {
		t.Fatalf("ProviderDataDir(antigravity env) = %q, %v; want %q", got, err, agyEnv)
	}

	extra1 := filepath.Join(t.TempDir(), "wsl-codex")
	extra2 := filepath.Join(t.TempDir(), "other-codex")
	t.Setenv("KEISOU_CODEX_DIR", agyEnv)
	t.Setenv("KEISOU_CODEX_EXTRA_DIRS", extra1+string(os.PathListSeparator)+agyEnv)
	dirs, err := ProviderDataDirs(model.ProviderCodex, "", []string{" ", extra2, extra1})
	if err != nil {
		t.Fatalf("ProviderDataDirs = %v", err)
	}
	if len(dirs) != 3 || dirs[0] != agyEnv || dirs[1] != extra1 || dirs[2] != extra2 {
		t.Fatalf("ProviderDataDirs = %v, want [%s %s %s]", dirs, agyEnv, extra1, extra2)
	}
}
