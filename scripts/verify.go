package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

func main() {
	if err := verify(); err != nil {
		fmt.Fprintln(os.Stderr, "Verification failed:", err)
		os.Exit(2)
	}
}

func verify() (err error) {
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	if err := configureGoEnv(wd); err != nil {
		return err
	}
	if err := verifyInstructions(wd); err != nil {
		return fmt.Errorf("%w; recreate CLAUDE.md and GEMINI.md as hard links to AGENTS.md (see README.md)", err)
	}
	fmt.Fprintln(os.Stderr, "Agent instructions: SHA-256 match")
	var dirs []string
	for _, dir := range []string{"cmd", "internal", "scripts"} {
		info, err := os.Stat(dir)
		if err == nil && info.IsDir() {
			dirs = append(dirs, dir)
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("stat %s: %w", dir, err)
		}
	}
	if len(dirs) > 0 {
		if err := run("gofmt", append([]string{"-s", "-w"}, dirs...)...); err != nil {
			return err
		}
	}
	if err := run("go", "vet", "./..."); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(os.Getenv("GOTMPDIR"), "keisou-verify-*")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(tmp)) }()
	name := "keisou"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if err := run("go", "build", "-o", filepath.Join(tmp, name), "./cmd/keisou"); err != nil {
		return err
	}
	args := []string{"test"}
	race, err := raceAvailable()
	if err != nil {
		return err
	}
	if race {
		args = append(args, "-race")
	} else {
		fmt.Fprintln(os.Stderr, "Warning: race testing unavailable (requires CGO and a working C compiler); running standard tests")
	}
	return run("go", append(args, "./...")...)
}

func configureGoEnv(wd string) error {
	defaults := goEnvDefaults()
	for _, entry := range []struct{ key, dir string }{
		{"GOCACHE", "go-build"}, {"GOPATH", "gopath"}, {"GOTMPDIR", "tmp"},
	} {
		if writableDir(os.Getenv(entry.key)) {
			continue
		}
		fallback := filepath.Join(wd, ".cache", entry.dir)
		if err := os.MkdirAll(fallback, 0755); err == nil && writableDir(fallback) {
			if err := os.Setenv(entry.key, fallback); err != nil {
				return err
			}
			continue
		}
		def := defaults[entry.key]
		if entry.key == "GOTMPDIR" && def == "" {
			def = os.TempDir()
		}
		candidates := []string{def, filepath.Join(os.TempDir(), "keisou-go", entry.dir)}
		configured := false
		for _, candidate := range candidates {
			if candidate == "" {
				continue
			}
			_ = os.MkdirAll(candidate, 0755)
			if writableDir(candidate) {
				if err := os.Setenv(entry.key, candidate); err != nil {
					return err
				}
				configured = true
				break
			}
		}
		if !configured {
			return fmt.Errorf("no writable directory for %s (tried %s and %s)", entry.key, fallback, def)
		}
	}
	return nil
}

func goEnvDefaults() map[string]string {
	result := map[string]string{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "go", "env", "GOCACHE", "GOPATH", "GOTMPDIR").Output()
	if err != nil {
		return result
	}
	lines := strings.Split(strings.ReplaceAll(strings.TrimRight(string(out), "\r\n"), "\r\n", "\n"), "\n")
	keys := []string{"GOCACHE", "GOPATH", "GOTMPDIR"}
	for i := 0; i < len(keys) && i < len(lines); i++ {
		result[keys[i]] = strings.TrimSpace(lines[i])
	}
	return result
}

func writableDir(path string) bool {
	if !filepath.IsAbs(path) {
		return false
	}
	file, err := os.CreateTemp(path, ".keisou-verify-*")
	if err != nil {
		return false
	}
	if err := errors.Join(file.Close(), os.Remove(file.Name())); err != nil {
		return false
	}
	dir, err := os.MkdirTemp(path, ".keisou-verify-dir-*")
	if err != nil {
		return false
	}
	return os.RemoveAll(dir) == nil
}

func verifyInstructions(dir string) error {
	data, err := os.ReadFile(filepath.Join(dir, "AGENTS.md"))
	if err != nil {
		return fmt.Errorf("read AGENTS.md: %w", err)
	}
	checksum := sha256.Sum256(data)
	for _, name := range []string{"CLAUDE.md", "GEMINI.md"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		if sha256.Sum256(data) != checksum {
			return fmt.Errorf("%s differs from AGENTS.md", name)
		}
	}
	return nil
}

func run(name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	fmt.Fprintln(os.Stderr, name, strings.Join(args, " "))
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), errors.Join(err, ctx.Err()))
	}
	return nil
}

func raceAvailable() (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "env", "CGO_ENABLED", "CC")
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("read Go compiler configuration: %w", err)
	}
	cgo, cc, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	if strings.TrimSpace(cgo) != "1" {
		return false, nil
	}
	// A configured absolute path may contain spaces.
	cc = strings.TrimSpace(cc)
	if _, err := exec.LookPath(cc); err != nil {
		fields := strings.Fields(cc)
		if len(fields) == 0 {
			return false, nil
		}
		return exec.CommandContext(ctx, fields[0], append(fields[1:], "--version")...).Run() == nil, nil
	}
	return exec.CommandContext(ctx, cc, "--version").Run() == nil, nil
}
