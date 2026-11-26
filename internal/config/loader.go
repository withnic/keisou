package config

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"time"

	"github.com/withnic/keisou/internal/infra"
	"github.com/withnic/keisou/internal/model"
)

const maxDurationSec = math.MaxInt64 / int64(time.Second)

func Defaults() model.Config {
	return model.Config{
		DefaultMode: model.ModeAuto, SoftThresholdPct: 95, HardThresholdPct: 98,
		SoftDrainGraceSec: 15, HandoffTimeoutSec: 30, PollIntervalSec: 15,
		HandoffFile: "HANDOFF.md", MaxHandoffCount: 10,
		Providers: map[model.ProviderID]model.ProviderConfig{
			model.ProviderClaude: {
				Enabled: true, Command: "claude", Args: []string{"-p", "--output-format", "text"},
				HandoffArgs: []string{"-p", "--output-format", "text", "--tools", ""},
				PlanName:    "Claude Pro", Est5hTokenLimit: 2000000, Est5hReqLimit: 100, Priority: 1,
			},
			model.ProviderCodex: {
				Enabled: true, Command: "codex", Args: []string{"exec"},
				HandoffArgs: []string{"exec", "--sandbox", "read-only", "--color", "never"},
				PlanName:    "ChatGPT Plus", Est5hTokenLimit: 2000000, Est5hReqLimit: 100, Priority: 2,
			},
			model.ProviderGemini: {
				Enabled: true, Command: "gemini", Args: []string{"-p", ""},
				HandoffArgs: []string{"-p", ""},
				PlanName:    "AI Pro", Est5hTokenLimit: 4000000, Est5hReqLimit: 200, Priority: 3,
			},
			model.ProviderAntigravity: {
				Enabled: true, Command: "agy", Args: []string{"-p", "--output-format", "text"},
				HandoffArgs: []string{"-p", "--output-format", "text", "--mode", "plan", "--disable-slash-commands"},
				PlanName:    "Antigravity", Est5hTokenLimit: 80000000, Est5hReqLimit: 0, EstWeeklyTokenLimit: 100000000, Priority: 4,
			},
		},
	}
}

func Load(ctx context.Context, explicitPath string) (model.Config, error) {
	cfg := Defaults()
	path, err := infra.ConfigPath(explicitPath)
	if err != nil {
		return model.Config{}, err
	}
	raw, err := infra.ReadTextFile(ctx, path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		if ctx.Err() != nil {
			return model.Config{}, ctx.Err()
		}
		return model.Config{}, &model.Error{Code: model.EConfigInvalid, Err: err}
	}
	cleaned, err := stripJSONC(raw)
	if err != nil {
		return model.Config{}, &model.Error{Code: model.EConfigInvalid, Err: err}
	}
	decoder := json.NewDecoder(strings.NewReader(cleaned))
	// Decode provider entries individually over their defaults.
	input := &struct {
		*model.Config
		Providers map[model.ProviderID]json.RawMessage `json:"providers"`
	}{Config: &cfg}
	if err := decoder.Decode(&input); err != nil {
		return model.Config{}, &model.Error{Code: model.EConfigInvalid, Err: err}
	}
	if input == nil {
		return model.Config{}, &model.Error{Code: model.EConfigInvalid, Err: errors.New("expected a configuration object")}
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return model.Config{}, &model.Error{Code: model.EConfigInvalid, Err: errors.New("unexpected content after configuration object")}
	}
	for id, rawProvider := range input.Providers {
		if bytes.Equal(bytes.TrimSpace(rawProvider), []byte("null")) {
			return model.Config{}, &model.Error{Code: model.EConfigInvalid, Err: fmt.Errorf("providers.%s must be an object", id)}
		}
		provider := cfg.Providers[id]
		if err := json.Unmarshal(rawProvider, &provider); err != nil {
			return model.Config{}, &model.Error{Code: model.EConfigInvalid, Err: fmt.Errorf("providers.%s: %w", id, err)}
		}
		cfg.Providers[id] = provider
	}
	if err := Validate(cfg); err != nil {
		return model.Config{}, err
	}
	return cfg, nil
}

// stripJSONC removes // and /* */ comments as well as trailing commas outside strings.
func stripJSONC(src string) (string, error) {
	noComments, err := stripComments(src)
	if err != nil {
		return "", err
	}
	return stripTrailingCommas(noComments), nil
}

func stripComments(src string) (string, error) {
	var out strings.Builder
	out.Grow(len(src))
	inString := false
	for i := 0; i < len(src); i++ {
		ch := src[i]
		if inString {
			out.WriteByte(ch)
			if ch == '\\' && i+1 < len(src) {
				i++
				out.WriteByte(src[i])
			} else if ch == '"' {
				inString = false
			}
			continue
		}
		if ch == '"' {
			inString = true
			out.WriteByte(ch)
			continue
		}
		if ch == '/' && i+1 < len(src) && src[i+1] == '/' {
			i += 2
			for i < len(src) && src[i] != '\n' {
				i++
			}
			if i < len(src) {
				out.WriteByte('\n')
			}
			continue
		}
		if ch == '/' && i+1 < len(src) && src[i+1] == '*' {
			i += 2
			closed := false
			for i < len(src) {
				if src[i] == '*' && i+1 < len(src) && src[i+1] == '/' {
					i++
					out.WriteByte(' ')
					closed = true
					break
				}
				if src[i] == '\n' {
					out.WriteByte('\n')
				}
				i++
			}
			if !closed {
				return "", errors.New("unterminated block comment")
			}
			continue
		}
		out.WriteByte(ch)
	}
	return out.String(), nil
}

func stripTrailingCommas(src string) string {
	var out strings.Builder
	out.Grow(len(src))
	inString := false
	for i := 0; i < len(src); i++ {
		ch := src[i]
		if inString {
			out.WriteByte(ch)
			if ch == '\\' && i+1 < len(src) {
				i++
				out.WriteByte(src[i])
			} else if ch == '"' {
				inString = false
			}
			continue
		}
		if ch == '"' {
			inString = true
			out.WriteByte(ch)
			continue
		}
		if ch == ',' {
			j := i + 1
			for j < len(src) && (src[j] == ' ' || src[j] == '\t' || src[j] == '\n' || src[j] == '\r') {
				j++
			}
			if j < len(src) && (src[j] == '}' || src[j] == ']') {
				continue
			}
		}
		out.WriteByte(ch)
	}
	return out.String()
}

func Validate(cfg model.Config) error {
	var err error
	switch {
	case !(cfg.SoftThresholdPct > 0 && cfg.SoftThresholdPct < cfg.HardThresholdPct && cfg.HardThresholdPct <= 100):
		err = errors.New("require 0 < soft_threshold_pct < hard_threshold_pct <= 100")
	case cfg.PollIntervalSec < 1 || int64(cfg.PollIntervalSec) > maxDurationSec:
		err = errors.New("poll_interval_sec must be at least 1 and within duration range")
	case cfg.SoftDrainGraceSec < 0 || int64(cfg.SoftDrainGraceSec) > maxDurationSec:
		err = errors.New("soft_drain_grace_sec must be at least 0 and within duration range")
	case cfg.HandoffTimeoutSec < 5 || int64(cfg.HandoffTimeoutSec) > maxDurationSec:
		err = errors.New("handoff_timeout_sec must be at least 5 and within duration range")
	case cfg.AskTimeoutSec < 0 || int64(cfg.AskTimeoutSec) > maxDurationSec:
		err = errors.New("ask_timeout_sec must be at least 0 and within duration range")
	case cfg.MaxHandoffCount < 1:
		err = errors.New("max_handoff_count must be at least 1")
	case strings.TrimSpace(cfg.HandoffFile) == "":
		err = errors.New("handoff_file must not be empty")
	case cfg.DefaultMode != model.ModeAuto && cfg.DefaultMode != model.ModeAsk:
		err = errors.New("default_mode must be auto or ask")
	}
	if err == nil {
		for id, p := range cfg.Providers {
			if p.Window5hResetAnchor != "" {
				if _, parseErr := time.Parse(time.RFC3339, p.Window5hResetAnchor); parseErr != nil {
					err = fmt.Errorf("providers.%s.window_5h_reset_anchor must be RFC3339: %w", id, parseErr)
					break
				}
			}
			if p.WeeklyResetAnchor != "" {
				if _, parseErr := time.Parse(time.RFC3339, p.WeeklyResetAnchor); parseErr != nil {
					err = fmt.Errorf("providers.%s.weekly_reset_anchor must be RFC3339: %w", id, parseErr)
					break
				}
			}
		}
	}
	if err != nil {
		return &model.Error{Code: model.EConfigInvalid, Err: err}
	}
	return nil
}
