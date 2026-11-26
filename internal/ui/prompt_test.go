package ui

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/withnic/keisou/internal/infra"
	"github.com/withnic/keisou/internal/model"
)

func TestPromptChooseBehaviors(t *testing.T) {
	reset := time.Now().Add(time.Hour)
	snapshots := []model.UsageSnapshot{
		{Provider: model.ProviderClaude, Available: true, Window5hUsedPct: 96, Window5hResetAt: &reset},
		{Provider: model.ProviderCodex, Available: true, Window5hUsedPct: 21},
		{Provider: model.ProviderGemini, Available: false},
	}
	candidates := snapshots[1:2]

	// Invalid line followed by valid selection without trailing newline.
	var out bytes.Buffer
	p := &Prompt{Input: strings.NewReader("99\n1")}
	got, err := p.Choose(context.Background(), &out, infra.SystemClock{}, snapshots, candidates, 0)
	if err != nil || got != model.ProviderCodex {
		t.Fatalf("Choose retry = %q, %v; want codex", got, err)
	}
	if !strings.Contains(out.String(), "[1] codex (21.0%)") || !strings.Contains(out.String(), "Enter one of the listed choices.") {
		t.Fatalf("unexpected prompt output:\n%s", out.String())
	}

	// Case-insensitive W and Q.
	got, err = (&Prompt{Input: strings.NewReader("W\n")}).Choose(context.Background(), &out, infra.SystemClock{}, snapshots, candidates, 0)
	if err != nil || got != "" {
		t.Fatalf("Choose(W) = %q, %v; want empty wait", got, err)
	}
	if _, err = (&Prompt{Input: strings.NewReader("Q\n")}).Choose(context.Background(), &out, infra.SystemClock{}, snapshots, candidates, 0); err == nil {
		t.Fatal("Choose(Q) unexpectedly succeeded")
	}

	// Nil input, empty candidates, and non-TTY regular file must error safely.
	if _, err = (&Prompt{}).Choose(context.Background(), &out, infra.SystemClock{}, snapshots, candidates, 0); err == nil {
		t.Fatal("nil input unexpectedly succeeded")
	}
	if _, err = (&Prompt{Input: strings.NewReader("1\n")}).Choose(context.Background(), &out, infra.SystemClock{}, snapshots, nil, 0); err == nil {
		t.Fatal("empty candidates unexpectedly succeeded")
	}
	regular := filepath.Join(t.TempDir(), "stdin.txt")
	if err := os.WriteFile(regular, []byte("1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(regular)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err = (&Prompt{Input: f}).Choose(context.Background(), &out, infra.SystemClock{}, snapshots, candidates, 0); err == nil || !strings.Contains(err.Error(), "terminal") {
		t.Fatalf("non-TTY file = %v, want terminal error", err)
	}
}
