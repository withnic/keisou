package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"time"

	"github.com/withnic/keisou/internal/config"
	"github.com/withnic/keisou/internal/infra"
	"github.com/withnic/keisou/internal/provider"
	"github.com/withnic/keisou/internal/ui"
)

func status(ctx context.Context, args []string, defaultPath string, stdout, stderr io.Writer, clock infra.Clock) error {
	flags := flag.NewFlagSet("status", flag.ContinueOnError)
	flags.SetOutput(stderr)
	path := flags.String("config", defaultPath, "configuration file path")
	machine := flags.Bool("json", false, "output JSON array (or NDJSON with --watch) to stdout")
	watch := flags.Bool("watch", false, "refresh periodically until interrupted")
	interval := flags.Int64("interval", 0, "refresh interval in seconds (default: config poll_interval_sec)")
	flags.Usage = func() {
		fmt.Fprint(stderr, `Usage:
  keisou status [--json] [--watch] [--interval sec] [--config path]

Display 5h and weekly quota usage from local logs without calling external LLM APIs.
Values suffixed with (est) are local estimates; exact server values have no suffix.

Flags:
`)
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return flagError(err)
	}
	if flags.NArg() != 0 {
		return flagError(fmt.Errorf("unexpected argument %q", flags.Arg(0)))
	}
	explicitInterval := false
	flags.Visit(func(f *flag.Flag) { explicitInterval = explicitInterval || f.Name == "interval" })
	if explicitInterval && (*interval <= 0 || *interval > math.MaxInt64/int64(time.Second)) {
		return flagError(fmt.Errorf("--interval must be a positive number of seconds within duration range"))
	}
	cfg, err := config.Load(ctx, *path)
	if err != nil {
		return err
	}
	if *interval == 0 {
		*interval = int64(cfg.PollIntervalSec)
	}
	delay := time.Duration(*interval) * time.Second
	collector := provider.NewCollector(cfg, nil)
	encoder := json.NewEncoder(stdout)
	if *watch && !*machine {
		restore := infra.EnableTerminalSequences(stdout)
		defer restore()
	}
	for {
		now := clock.Now()
		snapshots := collector.Collect(ctx, now)
		if err := ctx.Err(); err != nil {
			return err
		}
		if *machine {
			err = encoder.Encode(snapshots)
		} else {
			if *watch {
				if _, err := io.WriteString(stdout, "\x1b[2J\x1b[H"); err != nil {
					return fmt.Errorf("clear status display: %w", err)
				}
			}
			err = ui.RenderTable(stdout, snapshots, now)
		}
		if err != nil {
			return fmt.Errorf("write status: %w", err)
		}
		if !*watch {
			return nil
		}
		if err := clock.Sleep(ctx, delay); err != nil {
			return err
		}
	}
}
