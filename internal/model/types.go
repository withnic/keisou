package model

import (
	"math"
	"time"
)

type ProviderID string

const (
	ProviderClaude      ProviderID = "claude"
	ProviderCodex       ProviderID = "codex"
	ProviderGemini      ProviderID = "gemini"
	ProviderAntigravity ProviderID = "antigravity"
)

type RelayMode string

const (
	ModeAuto RelayMode = "auto"
	ModeAsk  RelayMode = "ask"
)

type QuotaSource string

const (
	SourceLogExact    QuotaSource = "log_exact"
	SourceWindowCalc  QuotaSource = "window_calc"
	SourceUnavailable QuotaSource = "unavailable"
)

type UsageSnapshot struct {
	Provider           ProviderID  `json:"provider"`
	PlanName           string      `json:"plan_name"`
	Available          bool        `json:"available"`
	Source             QuotaSource `json:"source"`
	Window5hUsedPct    float64     `json:"window_5h_used_pct"`
	Window5hResetAt    *time.Time  `json:"window_5h_reset_at,omitempty"`
	WeeklyUsedPct      *float64    `json:"weekly_used_pct,omitempty"`
	WeeklyResetAt      *time.Time  `json:"weekly_reset_at,omitempty"`
	ActiveModel        string      `json:"active_model"`
	IsFallbackModel    bool        `json:"is_fallback_model"`
	RecentTokens5h     int64       `json:"recent_tokens_5h"`
	RecentTokensWeekly int64       `json:"recent_tokens_weekly,omitempty"`
	RecentReqs5h       int         `json:"recent_reqs_5h"`
	UpdatedAt          time.Time   `json:"updated_at"`
	ErrorMessage       string      `json:"error_message,omitempty"`
}

func ClampPercent(value float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0
	}
	return min(100, max(0, value))
}

type ProviderConfig struct {
	Enabled             bool     `json:"enabled"`
	Command             string   `json:"command"`
	Args                []string `json:"args"`
	HandoffArgs         []string `json:"handoff_args,omitempty"`
	DataDir             string   `json:"data_dir"`
	ExtraDataDirs       []string `json:"extra_data_dirs,omitempty"`
	PlanName            string   `json:"plan_name"`
	Est5hTokenLimit     int64    `json:"est_5h_token_limit"`
	Est5hReqLimit       int      `json:"est_5h_req_limit"`
	EstWeeklyTokenLimit int64    `json:"est_weekly_token_limit,omitempty"`
	Window5hResetAnchor string   `json:"window_5h_reset_anchor,omitempty"`
	WeeklyResetAnchor   string   `json:"weekly_reset_anchor,omitempty"`
	Priority            int      `json:"priority"`
}

type Config struct {
	DefaultMode       RelayMode                     `json:"default_mode"`
	SoftThresholdPct  float64                       `json:"soft_threshold_pct"`
	HardThresholdPct  float64                       `json:"hard_threshold_pct"`
	SoftDrainGraceSec int                           `json:"soft_drain_grace_sec"`
	HandoffTimeoutSec int                           `json:"handoff_timeout_sec"`
	PollIntervalSec   int                           `json:"poll_interval_sec"`
	AskTimeoutSec     int                           `json:"ask_timeout_sec"`
	HandoffFile       string                        `json:"handoff_file"`
	MaxHandoffCount   int                           `json:"max_handoff_count"`
	Providers         map[ProviderID]ProviderConfig `json:"providers"`
}
