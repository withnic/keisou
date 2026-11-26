package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/withnic/keisou/internal/config"
	"github.com/withnic/keisou/internal/infra"
	"github.com/withnic/keisou/internal/model"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	err := dispatch(ctx, args, stdout, stderr)
	if ctx.Err() != nil && model.ExitCode(err) != 130 {
		err = &model.Error{Code: model.EInterrupted, Err: errors.Join(ctx.Err(), err)}
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
	}
	return model.ExitCode(err)
}

func printGlobalUsage(w io.Writer) {
	fmt.Fprint(w, `keisou - monitor local AI agent quotas and hand off tasks across agents

Usage:
  keisou [--config path] <command> [flags]

Commands:
  status      Show 5h/weekly quota usage side-by-side (read-only, no API calls)
  run         Run an agent with quota monitoring and automatic/ask handoff relay
  handoff     Generate HANDOFF.md locally from git status/diff and recent logs
  statusline  Cache official statusLine rate_limits JSON from stdin (Claude Code)
  init        Create default config.json and diagnose CLI/log paths
  help        Show help for keisou or a specific subcommand

Global Flags:
  --config path   Configuration file path (overrides KEISOU_CONFIG and OS default)

Run 'keisou help <command>' or 'keisou <command> --help' for command details.
`)
}

func dispatch(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	global := flag.NewFlagSet("keisou", flag.ContinueOnError)
	global.SetOutput(stderr)
	path := global.String("config", "", "configuration file path")
	global.Usage = func() { printGlobalUsage(stderr) }
	if err := global.Parse(args); err != nil {
		return flagError(err)
	}
	switch global.Arg(0) {
	case "help":
		rest := global.Args()[1:]
		if len(rest) == 0 {
			printGlobalUsage(stdout)
			return nil
		}
		if len(rest) > 1 {
			return flagError(fmt.Errorf("unexpected argument %q", rest[1]))
		}
		switch rest[0] {
		case "status", "run", "handoff", "statusline", "init":
			return dispatch(ctx, []string{rest[0], "--help"}, stdout, stdout)
		default:
			return flagError(fmt.Errorf("unknown command %q", rest[0]))
		}
	case "status":
		return status(ctx, global.Args()[1:], *path, stdout, stderr, infra.SystemClock{})
	case "run", "handoff":
		return relayCommand(ctx, global.Arg(0), global.Args()[1:], *path, stdout, stderr)
	case "statusline":
		return statuslineCommand(ctx, global.Args()[1:], *path, os.Stdin, stdout, stderr, infra.SystemClock{})
	case "init":
	default:
		global.Usage()
		return flagError(fmt.Errorf("expected status, run, handoff, statusline, init, or help subcommand"))
	}
	initFlags := flag.NewFlagSet("init", flag.ContinueOnError)
	initFlags.SetOutput(stderr)
	subPath := initFlags.String("config", *path, "configuration file path")
	force := initFlags.Bool("force", false, "overwrite an existing configuration file")
	initFlags.Usage = func() {
		fmt.Fprint(stderr, `Usage:
  keisou init [--config path] [--force]

Create a default config.json (JSONC supported) and check installed CLIs and log directories.

Flags:
`)
		initFlags.PrintDefaults()
	}
	if err := initFlags.Parse(global.Args()[1:]); err != nil {
		return flagError(err)
	}
	if initFlags.NArg() != 0 {
		return flagError(fmt.Errorf("unexpected argument %q", initFlags.Arg(0)))
	}
	return initConfig(ctx, *subPath, *force, stdout, stderr)
}

func flagError(err error) error {
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	return &model.Error{Code: model.EInvalidArgs, Err: err}
}

func initConfig(ctx context.Context, explicitPath string, force bool, stdout, stderr io.Writer) error {
	path, err := infra.ConfigPath(explicitPath)
	if err != nil {
		return err
	}
	cfg := config.Defaults()
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode default configuration: %w", err)
	}
	publish := infra.AtomicCreateFile
	if force {
		publish = infra.AtomicWriteFile
	}
	if err := publish(ctx, path, append(data, '\n'), 0600); err != nil {
		if !force && errors.Is(err, os.ErrExist) {
			return &model.Error{Code: model.EInternal, Err: fmt.Errorf("%q already exists; use keisou init --force to replace it", path)}
		}
		return err
	}
	fmt.Fprintf(stdout, "Created configuration: %s\n", path)
	for _, id := range []model.ProviderID{model.ProviderClaude, model.ProviderCodex, model.ProviderGemini, model.ProviderAntigravity} {
		provider := cfg.Providers[id]
		cli, err := exec.LookPath(provider.Command)
		if err != nil {
			fmt.Fprintf(stderr, "%s CLI: %v\n", id, err)
		} else {
			fmt.Fprintf(stderr, "%s CLI: %s\n", id, cli)
		}
		dir, err := infra.ProviderDataDir(id, provider.DataDir)
		if err == nil {
			var info os.FileInfo
			info, err = os.Stat(dir)
			if err == nil && !info.IsDir() {
				err = fmt.Errorf("%q is not a directory", dir)
			}
		}
		if err != nil {
			fmt.Fprintf(stderr, "%s data: %v\n", id, err)
		} else {
			fmt.Fprintf(stderr, "%s data: %s\n", id, dir)
		}
	}
	return nil
}
