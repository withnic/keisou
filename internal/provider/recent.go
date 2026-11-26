package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/withnic/keisou/internal/infra"
	"github.com/withnic/keisou/internal/model"
)

func truncateBytesUTF8(data []byte, limit int) string {
	if len(data) > limit {
		n := limit
		for n > 0 && !utf8.RuneStart(data[n]) {
			n--
		}
		data = data[:n]
	}
	return strings.ToValidUTF8(string(data), "\ufffd")
}

// RecentLogTail samples the end of the latest local session for manual handoff.
func RecentLogTail(ctx context.Context, id model.ProviderID, configuredDir string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	dir, err := infra.ProviderDataDir(id, configuredDir)
	if err != nil {
		return "", err
	}
	extension := ".jsonl"
	switch id {
	case model.ProviderClaude:
		dir = filepath.Join(dir, "projects")
	case model.ProviderCodex:
		dir = filepath.Join(dir, "sessions")
	case model.ProviderGemini:
		dir, extension = filepath.Join(dir, "tmp"), ".json"
	case model.ProviderAntigravity:
		dir = filepath.Join(dir, "brain")
	}
	var path string
	var latest time.Time
	var scanErr error
	err = walk(ctx, dir, time.Time{}, func(path string) bool {
		switch id {
		case model.ProviderGemini:
			return filepath.Ext(path) == extension && filepath.Base(filepath.Dir(path)) == "chats"
		case model.ProviderAntigravity:
			return filepath.Base(path) == "transcript.jsonl"
		default:
			return filepath.Ext(path) == extension
		}
	}, func(candidate string) {
		// WalkDir order breaks ties reproducibly.
		info, err := os.Lstat(candidate)
		if err != nil {
			scanErr = err
		} else if info.ModTime().After(latest) {
			latest, path = info.ModTime(), candidate
		}
	}, func(err error) { scanErr = err })
	if err != nil || path == "" {
		if err == nil {
			err = scanErr
		}
		return "", err
	}

	const maxExcerpt = 16 << 10
	if id == model.ProviderGemini {
		var lines []string
		u := usage{}
		readObjectFile(ctx, path, &u, map[string]bool{"messages": true}, func(o object) {
			if len(lines) == 8 {
				lines = lines[1:]
			}
			raw, err := json.Marshal(o)
			if err != nil {
				u.noteError(err)
				return
			}
			lines = append(lines, truncateBytesUTF8(raw, maxExcerpt/8))
		})
		return strings.Join(lines, "\n"), u.err
	}
	var lines []string
	remaining := maxExcerpt
	err = infra.ReadJSONLReverse(ctx, path, func(line []byte) bool {
		lines = append(lines, truncateBytesUTF8(line, remaining))
		remaining -= min(len(line)+1, remaining)
		return remaining > 0 && len(lines) < 20
	})
	if err != nil {
		return "", fmt.Errorf("read latest %s session: %w", id, err)
	}
	for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
		lines[i], lines[j] = lines[j], lines[i]
	}
	return strings.Join(lines, "\n"), nil
}
