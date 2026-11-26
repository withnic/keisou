package provider

import (
	"context"
	"path/filepath"
	"time"
)

type codexSample struct {
	at                 time.Time
	id                 string
	tokens, cumulative int64
	hasLast, hasTotal  bool
}

func scanCodex(ctx context.Context, dir string, u *usage) {
	walkLogs(ctx, filepath.Join(dir, "sessions"), ".jsonl", u, func(path string) {
		scanCodexSession(ctx, path, u)
	})
	logDir := filepath.Join(dir, "log")
	u.noteError(walk(ctx, logDir, u.cutoff(), func(path string) bool {
		return filepath.Dir(path) == logDir && filepath.Ext(path) == ".log"
	}, func(path string) {
		scanJSONLog(ctx, path, u, true, func(t time.Time, o object) {
			u.observeModel(t, o.text("model"))
		})
	}, u.noteError))
}

func scanCodexSession(ctx context.Context, path string, u *usage) {
	var samples []codexSample
	hasTurns := false
	reachedStart := scanJSONLog(ctx, path, u, false, func(t time.Time, o object) {
		payload := o.child("payload")
		if payload == nil {
			payload = o
		}
		name := payload.text("model", "model_name")
		if name == "" {
			name = o.text("model")
		}
		u.observeModel(t, name)
		kind := payload.text("type", "event")
		id := payload.text("event_id", "eventId", "id")
		if id == "" {
			id = o.text("event_id", "eventId", "id")
		}
		switch kind {
		case "task_started", "task_complete", "turn_started", "turn_completed", "turn":
			if !u.inWindow(t) {
				return
			}
			turnID := payload.text("turn_id", "turnId")
			if turnID == "" && (kind == "task_complete" || kind == "turn_completed") {
				return
			}
			hasTurns = true
			u.add(t, 0, true, key("turn:", turnID), key("event:", id))
		case "token_count":
			info := payload.child("info")
			last := info.child("last_token_usage")
			if last == nil {
				last = payload.child("last_token_usage", "usage", "token_usage")
			}
			if last == nil {
				last = info.child("usage")
			}
			total := info.child("total_token_usage")
			if total == nil {
				total = payload.child("total_token_usage")
			}
			if last != nil || total != nil {
				samples = append(samples, codexSample{t, id, codexTokens(last), codexTokens(total), last != nil, total != nil})
			}
		}
	})

	// Samples are collected newest first. Walk them forward so cumulative
	// counters contribute only their delta, including the pre-window baseline.
	var previous int64
	havePrevious := reachedStart
	for i := len(samples) - 1; i >= 0; i-- {
		s := samples[i]
		tokens := s.tokens
		if s.hasTotal {
			if havePrevious && previous == s.cumulative {
				continue
			}
			if !s.hasLast && havePrevious && s.cumulative >= previous {
				tokens = s.cumulative - previous
			}
			previous, havePrevious = s.cumulative, true
		}
		// A first cumulative sample can include activity older than the scan.
		// Without a baseline or last_token_usage only its request is known.
		u.add(s.at, tokens, !hasTurns, key("event:", s.id))
	}
}

func codexTokens(metrics object) int64 {
	if _, ok := number(metrics["total_tokens"]); ok {
		return tokenCount(metrics, "total_tokens")
	}
	return tokenCount(metrics, "input_tokens", "output_tokens", "reasoning_output_tokens")
}
