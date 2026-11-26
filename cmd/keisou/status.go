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
	"github.com/withnic/keisou/internal/history"
	"github.com/withnic/keisou/internal/infra"
	"github.com/withnic/keisou/internal/provider"
	"github.com/withnic/keisou/internal/ui"
)

func status(ctx context.Context, args []string, defaultPath string, stdout, stderr io.Writer, clock infra.Clock) error {
	flags := flag.NewFlagSet("status", flag.ContinueOnError)
	flags.SetOutput(stderr)
	path := flags.String("config", defaultPath, "configuration file path")
	machine := flags.Bool("json", false, "output JSON array (or NDJSON with --watch) to stdout")
	watch := flags.Bool("watch", false, "refresh periodically with 5h trend, limit ETA, and 7d graph until interrupted")
	graph := flags.Bool("graph", false, "show 5h trend sparkline, limit ETA, and 7d daily peak graph (auto-enabled with --watch)")
	interval := flags.Int64("interval", 0, "refresh interval in seconds (default: config poll_interval_sec)")
	flags.Usage = func() {
		fmt.Fprint(stderr, `Usage:
  keisou status [--json] [--watch] [--graph] [--interval sec] [--config path]

Display 5h and weekly quota usage from local logs without calling external LLM APIs.
Values suffixed with (est) are local estimates; exact server values have no suffix.
Snapshots are cached in history.jsonl (7-day retention) alongside config.json for trend/ETA graphs.

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
	var historyPath string
	var entries []history.Entry
	if cfgPath, pathErr := infra.ConfigPath(*path); pathErr == nil {
		historyPath = history.DefaultPath(cfgPath)
		loaded, loadErr := history.Load(ctx, historyPath, clock.Now())
		if loadErr != nil {
			fmt.Fprintf(stderr, "warning: load history: %v\n", loadErr)
		} else {
			entries = loaded
		}
	}
	if *watch && !*machine {
		restore := infra.EnableTerminalSequences(stdout)
		defer restore()
	}
	showGraph := *watch || *graph
	for {
		now := clock.Now()
		snapshots := collector.Collect(ctx, now)
		if err := ctx.Err(); err != nil {
			return err
		}
		if historyPath != "" {
			updated, recErr := history.Record(ctx, historyPath, entries, snapshots, now)
			if recErr != nil {
				fmt.Fprintf(stderr, "warning: save history: %v\n", recErr)
			}
			entries = updated
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
			if err == nil && showGraph {
				summaries := history.Summarize(entries, snapshots, now)
				err = ui.RenderGraphs(stdout, summaries, now)
			}
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
