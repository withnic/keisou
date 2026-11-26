package provider

import (
	"context"
	"path/filepath"
	"time"
)

func scanClaude(ctx context.Context, dir string, u *usage) {
	walkLogs(ctx, filepath.Join(dir, "projects"), ".jsonl", u, func(path string) {
		scanJSONLog(ctx, path, u, false, func(t time.Time, o object) {
			claudeRecord(u, t, o)
		})
	})
	scanFile(ctx, filepath.Join(dir, "keisou-rate-limits.json"), u, func(path string) {
		readObjectFile(ctx, path, u, nil, func(o object) {
			t := timestamp(o)
			u.metadata(t, o)
			if name := o.text("model"); name != "" {
				u.observeModel(t, name)
			} else if m := o.child("model"); m != nil {
				u.observeModel(t, m.text("id", "display_name", "name"))
			}
		})
	})
	scanFile(ctx, filepath.Join(dir, "stats-cache.json"), u, func(path string) {
		readObjectFile(ctx, path, u, map[string]bool{
			"messages": true, "events": true, "records": true,
		}, func(o object) {
			t := timestamp(o)
			u.metadata(t, o)
			claudeRecord(u, t, o)
		})
	})
}

func claudeRecord(u *usage, t time.Time, o object) {
	message := o.child("message")
	kind := o.text("type", "role")
	if kind != "" && kind != "assistant" && message.text("role") != "assistant" {
		return
	}
	metrics := message.child("usage")
	if metrics == nil {
		metrics = o.child("usage")
	}
	name := message.text("model")
	if name == "" {
		name = o.text("model")
	}
	u.observeModel(t, name)
	if metrics == nil {
		return
	}
	requestID := o.text("requestId", "request_id")
	if requestID == "" {
		requestID = message.text("requestId", "request_id")
	}
	messageID := message.text("id")
	if messageID == "" {
		messageID = o.text("messageId", "message_id")
	}
	u.add(t, tokenCount(metrics, "input_tokens", "output_tokens", "cache_creation_input_tokens"), true,
		key("request:", requestID), key("message:", messageID))
}

func key(prefix, id string) string {
	if id == "" {
		return ""
	}
	return prefix + id
}
