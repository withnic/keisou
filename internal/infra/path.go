package infra

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/withnic/keisou/internal/model"
)

func expandPath(path string) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") || strings.HasPrefix(path, "~\\") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		if path == "~" {
			return filepath.Clean(home), nil
		}
		rest := path[2:]
		if strings.HasPrefix(path, "~\\") {
			rest = filepath.FromSlash(strings.ReplaceAll(rest, "\\", "/"))
		}
		return filepath.Join(home, rest), nil
	}
	return filepath.Clean(path), nil
}

func ConfigPath(explicit string) (string, error) {
	if explicit != "" {
		return expandPath(explicit)
	}
	if path := os.Getenv("KEISOU_CONFIG"); path != "" {
		return expandPath(path)
	}
	var base string
	var err error
	if runtime.GOOS == "darwin" {
		base = os.Getenv("XDG_CONFIG_HOME")
		if base == "" {
			base, err = os.UserHomeDir()
			base = filepath.Join(base, ".config")
		}
	} else {
		base, err = os.UserConfigDir()
	}
	if err != nil {
		return "", fmt.Errorf("resolve config directory: %w", err)
	}
	return filepath.Join(base, "keisou", "config.json"), nil
}

func ProviderDataDir(id model.ProviderID, configured string) (string, error) {
	switch id {
	case model.ProviderClaude, model.ProviderCodex, model.ProviderGemini, model.ProviderAntigravity:
	default:
		return "", fmt.Errorf("unknown provider %q", id)
	}
	if path := os.Getenv("KEISOU_" + strings.ToUpper(string(id)) + "_DIR"); path != "" {
		return expandPath(path)
	}
	if configured != "" {
		return expandPath(configured)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve %s home directory: %w", id, err)
	}
	if id == model.ProviderAntigravity {
		return filepath.Join(home, ".gemini", "antigravity-cli"), nil
	}
	return filepath.Join(home, "."+string(id)), nil
}
