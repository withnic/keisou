package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/withnic/keisou/internal/config"
	"github.com/withnic/keisou/internal/infra"
	"github.com/withnic/keisou/internal/model"
	"github.com/withnic/keisou/internal/orchestrator"
	"github.com/withnic/keisou/internal/provider"
	"github.com/withnic/keisou/internal/ui"
)

func relayCommand(ctx context.Context, name string, args []string, configPath string, stdout, stderr io.Writer) error {
	return relayCommandInDir(ctx, name, args, configPath, "", stdout, stderr)
}

func relayCommandInDir(ctx context.Context, name string, args []string, configPath, dir string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&configPath, "config", configPath, "configuration file path")
	var handoffPath, agent, mode, task, promptFile string
	var resume, noWait bool
	var soft, hard float64
	flags.StringVar(&handoffPath, "handoff", "", "handoff Markdown file path (default: ./HANDOFF.md or config handoff_file)")
	if name == "run" {
		flags.StringVar(&agent, "agent", "", "initial agent: auto, claude, codex, gemini, or antigravity")
		flags.StringVar(&mode, "mode", "", "handoff routing mode: auto (auto-switch) or ask (interactive prompt)")
		flags.Float64Var(&soft, "soft-threshold", 0, "soft stop threshold % for LLM handoff (default: config soft_threshold_pct)")
		flags.Float64Var(&soft, "threshold", 0, "alias for --soft-threshold")
		flags.Float64Var(&hard, "hard-threshold", 0, "hard stop threshold % for local-only handoff (default: config hard_threshold_pct)")
		flags.BoolVar(&noWait, "no-wait", false, "exit with code 4 instead of waiting when all agents are exhausted")
		flags.StringVar(&promptFile, "prompt-file", "", "read the initial task prompt from a UTF-8 file (exclusive with [prompt]/--resume)")
		flags.BoolVar(&resume, "resume", false, "resume from existing handoff file (default: ./HANDOFF.md; exclusive with [prompt]/--prompt-file)")
		flags.Usage = func() {
			fmt.Fprint(stderr, `Usage:
  keisou run [flags] "<prompt>"
  keisou run [flags] --prompt-file <path>
  keisou run [flags] --resume

Run a coding agent while monitoring 5h and weekly quota usage. When usage reaches the
soft/hard threshold (or hits 429), stop safely, write HANDOFF.md, and relay to the next
available agent (lowest effective usage first, tie-broken by config priority).

Relay Modes (--mode):
  auto  Automatically pick the available agent with the lowest usage
  ask   Show remaining quotas and interactively prompt for the next agent

Flags:
`)
			flags.PrintDefaults()
			fmt.Fprint(stderr, `
Examples:
  keisou run "Add tests and refactor parser"
  keisou run --mode ask --agent claude --prompt-file task.txt
  keisou run --resume --handoff HANDOFF.md
`)
		}
	} else {
		flags.StringVar(&agent, "agent", "", "source agent: claude, codex, gemini, or antigravity (default: claude)")
		flags.StringVar(&task, "task", "", "task summary for ## 1. (default: inherit from existing HANDOFF.md)")
		flags.Usage = func() {
			fmt.Fprint(stderr, `Usage:
  keisou handoff [--agent name] [--handoff path] [--task summary] [--config path]

Generate and validate HANDOFF.md locally from git status/diff and the latest session log
without calling any external LLM API.

Flags:
`)
			flags.PrintDefaults()
			fmt.Fprint(stderr, `
Examples:
  keisou handoff --agent codex --task "Finish Phase 3 tests"
  keisou handoff --agent claude --handoff HANDOFF.md
`)
		}
	}
	if err := flags.Parse(args); err != nil {
		return flagError(err)
	}
	seen := map[string]bool{}
	flags.Visit(func(f *flag.Flag) { seen[f.Name] = true })
	if seen["agent"] {
		switch agent {
		case "claude", "codex", "gemini", "antigravity":
		case "auto":
			if name == "run" {
				break
			}
			fallthrough
		default:
			return flagError(fmt.Errorf("unknown agent %q", agent))
		}
	}
	if name == "run" {
		count := flags.NArg()
		if seen["prompt-file"] {
			count++
		}
		if resume {
			count++
		}
		if count != 1 || (seen["prompt-file"] && strings.TrimSpace(promptFile) == "") ||
			(flags.NArg() == 1 && strings.TrimSpace(flags.Arg(0)) == "") {
			return flagError(fmt.Errorf("specify exactly one nonempty task, --prompt-file, or --resume"))
		}
		task = flags.Arg(0)
	} else if flags.NArg() != 0 {
		return flagError(fmt.Errorf("unexpected argument %q", flags.Arg(0)))
	}
	cfg, err := config.Load(ctx, configPath)
	if err != nil {
		return err
	}
	if seen["mode"] {
		cfg.DefaultMode = model.RelayMode(mode)
	}
	if seen["soft-threshold"] || seen["threshold"] {
		cfg.SoftThresholdPct = soft
	}
	if seen["hard-threshold"] {
		cfg.HardThresholdPct = hard
	}
	if seen["handoff"] {
		cfg.HandoffFile = handoffPath
	}
	if err := config.Validate(cfg); err != nil {
		return err
	}
	if dir == "" {
		var err error
		dir, err = os.Getwd()
		if err != nil {
			return fmt.Errorf("get working directory: %w", err)
		}
	}
	engine := orchestrator.Engine{
		Config: cfg, Collector: provider.NewCollector(cfg, nil), Runner: infra.SystemProcessRunner{},
		Clock: infra.SystemClock{}, Diagnostic: stderr, Prompt: &ui.Prompt{Input: os.Stdin},
	}
	if name == "run" {
		return engine.Run(ctx, orchestrator.RunOptions{
			Dir: dir, Task: task, PromptFile: promptFile, Resume: resume, Agent: model.ProviderID(agent), NoWait: noWait,
		})
	}
	if agent == "" {
		agent = string(model.ProviderClaude)
	}
	if err := engine.ManualHandoff(ctx, dir, task, model.ProviderID(agent)); err != nil {
		return err
	}
	fmt.Fprintln(stdout, cfg.HandoffFile)
	return nil
}
