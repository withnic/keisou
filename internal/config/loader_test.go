package config

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/withnic/keisou/internal/infra"
	"github.com/withnic/keisou/internal/model"
)

func TestValidate(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*model.Config)
		want int
	}{
		{"equal thresholds", func(c *model.Config) { c.SoftThresholdPct = c.HardThresholdPct }, 2},
		{"reversed thresholds", func(c *model.Config) { c.SoftThresholdPct = 99 }, 2},
		{"negative threshold", func(c *model.Config) { c.SoftThresholdPct = -1 }, 2},
		{"nonfinite threshold", func(c *model.Config) { c.SoftThresholdPct = math.NaN() }, 2},
		{"hard above 100", func(c *model.Config) { c.HardThresholdPct = 101 }, 2},
		{"poll zero", func(c *model.Config) { c.PollIntervalSec = 0 }, 2},
		{"poll overflow", func(c *model.Config) { c.PollIntervalSec = int(maxDurationSec) + 1 }, 2},
		{"negative drain", func(c *model.Config) { c.SoftDrainGraceSec = -1 }, 2},
		{"short handoff", func(c *model.Config) { c.HandoffTimeoutSec = 4 }, 2},
		{"negative ask", func(c *model.Config) { c.AskTimeoutSec = -1 }, 2},
		{"zero relay count", func(c *model.Config) { c.MaxHandoffCount = 0 }, 2},
		{"empty handoff file", func(c *model.Config) { c.HandoffFile = "   " }, 2},
		{"invalid 5h reset anchor", func(c *model.Config) {
			p := c.Providers[model.ProviderAntigravity]
			p.Window5hResetAnchor = "not-a-time"
			c.Providers[model.ProviderAntigravity] = p
		}, 2},
		{"invalid weekly reset anchor", func(c *model.Config) {
			p := c.Providers[model.ProviderAntigravity]
			p.WeeklyResetAnchor = "2026/11/28 17:35"
			c.Providers[model.ProviderAntigravity] = p
		}, 2},
		{"unknown mode", func(c *model.Config) { c.DefaultMode = "manual" }, 2},
		{"valid boundaries", func(c *model.Config) {
			c.HardThresholdPct, c.PollIntervalSec, c.SoftDrainGraceSec = 100, 1, 0
			c.HandoffTimeoutSec, c.AskTimeoutSec, c.MaxHandoffCount = 5, 0, 1
			c.DefaultMode = model.ModeAsk
			p := c.Providers[model.ProviderAntigravity]
			p.Window5hResetAnchor = "2026-11-27T00:35:00+09:00"
			p.WeeklyResetAnchor = "2026-11-28T17:35:00+09:00"
			c.Providers[model.ProviderAntigravity] = p
		}, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := Defaults()
			test.edit(&cfg)
			if err := Validate(cfg); model.ExitCode(err) != test.want {
				t.Fatalf("Validate = %v, want exit code %d", err, test.want)
			}
		})
	}
}

func TestLoadPathPriorityAndDefaults(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("APPDATA", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("KEISOU_CONFIG", "")
	standard := filepath.Join(dir, "keisou", "config.json")
	if path, err := infra.ConfigPath(""); err != nil || path != standard {
		t.Fatalf("ConfigPath = %q, %v, want %q", path, err, standard)
	}
	ctx := context.Background()
	if cfg, err := Load(ctx, ""); err != nil || !reflect.DeepEqual(cfg, Defaults()) {
		t.Fatalf("missing config = %+v, %v", cfg, err)
	}
	envPath := filepath.Join(dir, "env.json")
	explicit := filepath.Join(dir, "explicit.json")
	for path, content := range map[string]string{
		envPath:  `{"default_mode":"ask"}`,
		explicit: `{"soft_drain_grace_sec":0,"providers":{"codex":{"enabled":false},"gemini":{"handoff_args":[]}}}`,
	} {
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("KEISOU_CONFIG", envPath)
	if cfg, err := Load(ctx, ""); err != nil || cfg.DefaultMode != model.ModeAsk {
		t.Fatalf("environment config = %+v, %v", cfg, err)
	}
	cfg, err := Load(ctx, explicit)
	if err != nil {
		t.Fatal(err)
	}
	want := Defaults()
	want.SoftDrainGraceSec = 0
	codex := want.Providers[model.ProviderCodex]
	codex.Enabled = false
	want.Providers[model.ProviderCodex] = codex
	gemini := want.Providers[model.ProviderGemini]
	gemini.HandoffArgs = []string{}
	want.Providers[model.ProviderGemini] = gemini
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("explicit config = %+v, want %+v", cfg, want)
	}
}

func TestLoadMalformedJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	for _, content := range []string{`{`, `{} {}`, `null`, `{"providers":{"claude":null}}`, `{"providers":{"claude":  null  }}`, `{"a": /* unterminated`, `{"default_mode":"auto",,}`, `// only comment`} {
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(context.Background(), path); model.ExitCode(err) != 2 {
			t.Errorf("load %q: %v, want E_CONFIG_INVALID", content, err)
		}
	}
	if err := os.WriteFile(path, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Load(canceled, path); !errors.Is(err, context.Canceled) || model.ExitCode(err) == 2 {
		t.Fatalf("canceled Load = %v, want context.Canceled (not E_CONFIG_INVALID)", err)
	}
}

func TestLoadJSONCCommentsAndTrailingCommas(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	content := `{
  // Line comment at top
  "default_mode": "ask", /* inline block comment */
  "soft_threshold_pct": 90,
  "providers": {
    "antigravity": {
      // Strings containing //, /* */, escaped quotes, and ,} must remain intact
      "plan_name": "https://example.com/*keep*/ \"quoted\",}",
      "args": ["-p", "--output-format", "text",],
      "priority": 1,
    },
  },
}
`
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(context.Background(), path)
	if err != nil {
		t.Fatalf("Load(JSONC) = %v", err)
	}
	ag := cfg.Providers[model.ProviderAntigravity]
	if cfg.DefaultMode != model.ModeAsk || cfg.SoftThresholdPct != 90 || ag.Priority != 1 ||
		ag.PlanName != `https://example.com/*keep*/ "quoted",}` || len(ag.Args) != 3 {
		t.Fatalf("unexpected JSONC config: %+v (ag=%+v)", cfg, ag)
	}
}
