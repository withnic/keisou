package provider

import (
	"context"
	"fmt"
	"path/filepath"
	"time"
)

func scanAntigravity(ctx context.Context, dir string, u *usage) {
	u.noteError(walk(ctx, filepath.Join(dir, "brain"), u.cutoff(), func(path string) bool {
		return filepath.Base(path) == "transcript.jsonl"
	}, func(path string) {
		scanJSONLog(ctx, path, u, false, func(t time.Time, o object) {
			antigravityRecord(u, path, t, o)
		})
	}, u.noteError))
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
}

func antigravityRecord(u *usage, path string, t time.Time, o object) {
	if o.text("type") != "PLANNER_RESPONSE" {
		return
	}
	u.observeModel(t, o.text("model"))
	tokens := tokenCount(o, "input_tokens", "output_tokens")
	var stepID string
	if step, ok := o["step_index"]; ok && step != nil {
		stepID = fmt.Sprintf("agstep:%s:%v", path, step)
	}
	u.add(t, tokens, true, stepID)
}
