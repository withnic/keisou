package provider

import (
	"context"
	"path/filepath"
	"time"
)

func scanGemini(ctx context.Context, dir string, u *usage) {
	u.noteError(walk(ctx, filepath.Join(dir, "tmp"), u.cutoff(), func(path string) bool {
		return filepath.Base(filepath.Dir(path)) == "chats" && filepath.Ext(path) == ".json"
	}, func(path string) {
		readObjectFile(ctx, path, u, map[string]bool{"messages": true}, func(o object) {
			t := timestamp(o)
			u.metadata(t, o)
			modelName := o.text("model", "modelName", "model_name")
			u.observeModel(t, modelName)
			metrics := o.child("tokens", "tokenUsage", "usage")
			kind := o.text("type", "role")
			if kind == "gemini" || kind == "assistant" || kind == "" && metrics != nil {
				u.add(t, tokenCount(metrics, "input", "output", "thoughts"), true,
					key("message:", o.text("id", "messageId", "message_id")))
			}
		})
	}, u.noteError))
	scanFile(ctx, filepath.Join(dir, "telemetry.log"), u, func(path string) {
		scanJSONLog(ctx, path, u, true, func(t time.Time, o object) {
			if !u.inWindow(t) {
				return
			}
			for _, record := range []object{o, o.child("payload"), o.child("attributes")} {
				u.observeModel(t, record.text("to_model", "fallbackModel", "model", "modelName", "model_name"))
				for _, field := range []string{"event", "event_name", "event.name", "type", "message", "reason"} {
					u.fallback = u.fallback || fallbackEvent.MatchString(record.text(field))
				}
			}
		})
	})
}
