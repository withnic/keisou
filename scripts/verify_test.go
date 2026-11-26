package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWritableDir(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if writableDir("") || writableDir("relative/path") || writableDir(filepath.Join(dir, "missing")) || writableDir(file) {
		t.Fatal("writableDir accepted non-writable or relative path")
	}
	if !writableDir(dir) {
		t.Fatalf("writableDir rejected valid temp dir %q", dir)
	}
}

func TestConfigureGoEnvFallbacks(t *testing.T) {
	wd := t.TempDir()
	customCache := filepath.Join(t.TempDir(), "preset")
	if err := os.MkdirAll(customCache, 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOCACHE", customCache)
	t.Setenv("GOPATH", "relative-path")
	t.Setenv("GOTMPDIR", "")

	if err := configureGoEnv(wd); err != nil {
		t.Fatalf("configureGoEnv(normal) = %v", err)
	}
	if got := os.Getenv("GOCACHE"); got != customCache {
		t.Fatalf("GOCACHE = %q, want preset %q", got, customCache)
	}
	if got, want := os.Getenv("GOPATH"), filepath.Join(wd, ".cache", "gopath"); got != want {
		t.Fatalf("GOPATH = %q, want %q", got, want)
	}
	if got, want := os.Getenv("GOTMPDIR"), filepath.Join(wd, ".cache", "tmp"); got != want {
		t.Fatalf("GOTMPDIR = %q, want %q", got, want)
	}

	// Block .cache with a regular file so configureGoEnv must fall back cleanly.
	blockedWD := t.TempDir()
	if err := os.WriteFile(filepath.Join(blockedWD, ".cache"), []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOCACHE", "")
	t.Setenv("GOPATH", "")
	t.Setenv("GOTMPDIR", "")
	if err := configureGoEnv(blockedWD); err != nil {
		t.Fatalf("configureGoEnv(blocked .cache) = %v", err)
	}
	for _, key := range []string{"GOCACHE", "GOPATH", "GOTMPDIR"} {
		if !writableDir(os.Getenv(key)) {
			t.Errorf("%s = %q is not a writable absolute dir", key, os.Getenv(key))
		}
	}
}

func TestVerifyInstructions(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"AGENTS.md", "CLAUDE.md", "GEMINI.md"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("same content\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := verifyInstructions(dir); err != nil {
		t.Fatalf("verifyInstructions(match) = %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("drifted\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifyInstructions(dir); err == nil {
		t.Fatal("verifyInstructions(mismatch) unexpectedly succeeded")
	}
}
