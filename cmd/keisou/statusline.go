package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/withnic/keisou/internal/config"
	"github.com/withnic/keisou/internal/infra"
	"github.com/withnic/keisou/internal/model"
)

const maxStatuslineInput = 1 << 20

type statuslineCache struct {
	UpdatedAt  time.Time       `json:"updated_at"`
	Model      json.RawMessage `json:"model,omitempty"`
	RateLimits json.RawMessage `json:"rate_limits"`
}

func statuslineCommand(ctx context.Context, args []string, configPath string, stdin io.Reader, stdout, stderr io.Writer, clock infra.Clock) error {
	flags := flag.NewFlagSet("statusline", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&configPath, "config", configPath, "configuration file path")
	var agent string
	var quiet bool
	flags.StringVar(&agent, "agent", string(model.ProviderClaude), "source agent: claude or antigravity")
	flags.BoolVar(&quiet, "quiet", false, "cache rate_limits without printing status text to stdout")
	flags.Usage = func() {
		fmt.Fprint(stderr, `Usage:
  keisou statusline [--agent claude|antigravity] [--quiet] [--config path]

Read official statusLine JSON from stdin, cache rate_limits to keisou-rate-limits.json
for exact quota display in 'keisou status' / 'keisou run', and print a concise status line.

Flags:
`)
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return flagError(err)
	}
	if flags.NArg() != 0 {
		return flagError(fmt.Errorf("unexpected argument %q", flags.Arg(0)))
	}
	id := model.ProviderID(agent)
	switch id {
	case model.ProviderClaude, model.ProviderAntigravity:
	default:
		return flagError(fmt.Errorf("unsupported statusline agent %q", agent))
	}
	cfg, err := config.Load(ctx, configPath)
	if err != nil {
		return err
	}
	raw, err := io.ReadAll(io.LimitReader(stdin, maxStatuslineInput+1))
	if err != nil {
		return fmt.Errorf("read statusline input: %w", err)
	}
	if len(raw) > maxStatuslineInput {
		return fmt.Errorf("statusline input exceeds %d bytes", maxStatuslineInput)
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &payload); err != nil {
		return nil
	}
	rateLimits := firstRaw(payload, "rate_limits", "rateLimits", "rate_limit", "rateLimit")
	if len(rateLimits) > 0 && !bytes.Equal(bytes.TrimSpace(rateLimits), []byte("null")) {
		dir, err := infra.ProviderDataDir(id, cfg.Providers[id].DataDir)
		if err != nil {
			return err
		}
		record := statuslineCache{
			UpdatedAt:  clock.Now().UTC(),
			Model:      payload["model"],
			RateLimits: rateLimits,
		}
		encoded, err := json.Marshal(record)
		if err != nil {
			return fmt.Errorf("encode rate limit cache: %w", err)
		}
		cachePath := filepath.Join(dir, "keisou-rate-limits.json")
		if err := infra.AtomicWriteFile(ctx, cachePath, append(encoded, '\n'), 0600); err != nil {
			return err
		}
	}
	if !quiet {
		if line := formatStatusline(rateLimits); line != "" {
			fmt.Fprintln(stdout, line)
		}
	}
	return nil
}

func firstRaw(m map[string]json.RawMessage, keys ...string) json.RawMessage {
	for _, k := range keys {
		if v := bytes.TrimSpace(m[k]); len(v) > 0 && !bytes.Equal(v, []byte("null")) {
			return v
		}
	}
	return nil
}

func formatStatusline(rateLimits json.RawMessage) string {
	if len(rateLimits) == 0 {
		return ""
	}
	var parsed struct {
		FiveHour struct {
			UsedPercentage *float64 `json:"used_percentage"`
			UsedPercent    *float64 `json:"used_percent"`
		} `json:"five_hour"`
		SevenDay struct {
			UsedPercentage *float64 `json:"used_percentage"`
			UsedPercent    *float64 `json:"used_percent"`
		} `json:"seven_day"`
	}
	if err := json.Unmarshal(rateLimits, &parsed); err != nil {
		return ""
	}
	var parts []string
	if v := firstFloat(parsed.FiveHour.UsedPercentage, parsed.FiveHour.UsedPercent); v != nil {
		parts = append(parts, fmt.Sprintf("5h: %.1f%%", model.ClampPercent(*v)))
	}
	if v := firstFloat(parsed.SevenDay.UsedPercentage, parsed.SevenDay.UsedPercent); v != nil {
		parts = append(parts, fmt.Sprintf("7d: %.1f%%", model.ClampPercent(*v)))
	}
	return strings.Join(parts, " | ")
}

func firstFloat(values ...*float64) *float64 {
	for _, v := range values {
		if v != nil {
			return v
		}
	}
	return nil
}
