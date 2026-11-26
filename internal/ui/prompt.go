package ui

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/withnic/keisou/internal/infra"
	"github.com/withnic/keisou/internal/model"
)

// Prompt keeps at most one read pending, even if a selection times out.
type Prompt struct {
	Input   io.Reader
	pending chan answer
	reader  *bufio.Reader
}

type answer struct {
	line string
	err  error
}

// Choose returns a provider, an empty provider for reset waiting, or an
// error for cancelled/unavailable input. Timeout selects the first candidate.
func (p *Prompt) Choose(ctx context.Context, out io.Writer, clock infra.Clock, snapshots, candidates []model.UsageSnapshot, timeout time.Duration) (model.ProviderID, error) {
	if len(candidates) == 0 {
		return "", errors.New("no selectable candidates")
	}
	now := clock.Now()
	for _, s := range snapshots {
		reset := "unknown"
		if s.Window5hResetAt != nil {
			reset = max(0, s.Window5hResetAt.Sub(now)).Round(time.Second).String()
		}
		if s.Available {
			fmt.Fprintf(out, "%s  %.1f%%  reset in %s\n", s.Provider, s.Window5hUsedPct, reset)
		} else {
			fmt.Fprintf(out, "%s  unavailable\n", s.Provider)
		}
	}
	for i, s := range candidates {
		fmt.Fprintf(out, "[%d] %s (%.1f%%)\n", i+1, s.Provider, s.Window5hUsedPct)
	}
	fmt.Fprintln(out, "[w] Wait for reset  [q] Quit")
	if p.Input == nil {
		return "", errors.New("selection input unavailable")
	}
	if f, ok := p.Input.(*os.File); ok {
		info, err := f.Stat()
		if err != nil || info.Mode()&os.ModeCharDevice == 0 {
			return "", errors.New("selection requires a terminal")
		}
	}
	if p.reader == nil {
		p.reader = bufio.NewReader(p.Input)
	}
	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var expired <-chan error
	if timeout > 0 {
		expired = infra.SleepAsync(waitCtx, clock, timeout)
	}
	for {
		if p.pending == nil {
			pending := make(chan answer, 1)
			p.pending = pending
			go func() {
				line, err := p.reader.ReadString('\n')
				pending <- answer{strings.TrimSpace(line), err}
			}()
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case err := <-expired:
			if err != nil {
				return "", err
			}
			return candidates[0].Provider, nil
		case input := <-p.pending:
			p.pending = nil
			if input.err != nil && (input.line == "" || !errors.Is(input.err, io.EOF)) {
				return "", fmt.Errorf("read selection: %w", input.err)
			}
			switch strings.ToLower(input.line) {
			case "w":
				return "", nil
			case "q":
				return "", errors.New("selection cancelled")
			}
			if n, err := strconv.Atoi(input.line); err == nil && n >= 1 && n <= len(candidates) {
				return candidates[n-1].Provider, nil
			}
			fmt.Fprintln(out, "Enter one of the listed choices.")
		}
	}
}
