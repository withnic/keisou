package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/withnic/keisou/internal/infra"
	"github.com/withnic/keisou/internal/model"
	"github.com/withnic/keisou/internal/provider"
	"github.com/withnic/keisou/internal/ui"
)

type Engine struct {
	Config     model.Config
	Collector  provider.Collector
	Runner     infra.ProcessRunner
	Clock      infra.Clock
	Prompt     *ui.Prompt
	Diagnostic io.Writer
}

type RunOptions struct {
	Dir        string
	Task       string
	PromptFile string
	Resume     bool
	Agent      model.ProviderID
	NoWait     bool
}

func failure(code model.ErrorCode, err error) error {
	return &model.Error{Code: code, Err: err}
}

func inDir(dir, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(dir, path)
}

func withRunLock(ctx context.Context, dir string, run func() error) (err error) {
	defer func() {
		if ctx.Err() != nil {
			err = failure(model.EInterrupted, errors.Join(ctx.Err(), err))
		}
	}()
	lock, err := infra.AcquireRunLock(ctx, dir)
	if err != nil {
		var structured *model.Error
		if errors.As(err, &structured) {
			return err
		}
		return failure(model.EInternal, err)
	}
	defer func() {
		if releaseErr := lock.Release(); releaseErr != nil {
			err = errors.Join(err, failure(model.EInternal, releaseErr))
		}
	}()
	return run()
}

func (e Engine) Run(ctx context.Context, opt RunOptions) error {
	inputs := 0
	for _, set := range []bool{opt.Task != "", opt.PromptFile != "", opt.Resume} {
		if set {
			inputs++
		}
	}
	if inputs != 1 || (opt.Task != "" && strings.TrimSpace(opt.Task) == "") {
		return failure(model.EInvalidArgs, errors.New("specify one task, --prompt-file, or --resume"))
	}
	if e.Diagnostic == nil {
		e.Diagnostic = io.Discard
	}
	if e.Prompt == nil {
		e.Prompt = &ui.Prompt{}
	}
	if opt.Agent == "auto" {
		opt.Agent = ""
	}
	if opt.Agent != "" {
		if _, ok := e.Config.Providers[opt.Agent]; !ok {
			return failure(model.EInvalidArgs, fmt.Errorf("unknown agent %q", opt.Agent))
		}
	}
	return withRunLock(ctx, opt.Dir, func() error {
		return e.relay(ctx, opt)
	})
}

func (e Engine) relay(ctx context.Context, opt RunOptions) error {
	path := inDir(opt.Dir, e.Config.HandoffFile)
	task, prompt := opt.Task, opt.Task
	if opt.PromptFile != "" {
		data, err := infra.ReadTextFile(ctx, inDir(opt.Dir, opt.PromptFile))
		if err != nil {
			return failure(model.EInvalidArgs, err)
		}
		task = strings.TrimSpace(data)
		if task == "" {
			return failure(model.EInvalidArgs, errors.New("prompt file is empty"))
		}
		prompt = task
	}
	continuation := fmt.Sprintf("Read the latest task handoff at the path below and continue the remaining work.\n%s\n", path)
	if opt.Resume {
		data, err := infra.ReadTextFile(ctx, path)
		if err != nil {
			return failure(model.EInvalidArgs, err)
		}
		if strings.TrimSpace(data) == "" {
			return failure(model.EInvalidArgs, errors.New("handoff file is empty"))
		}
		task, prompt = handoffSections(data)["1"], continuation
		if task == "" {
			task = data
		}
	}

	var blocked model.UsageSnapshot
	for count := 1; ; count++ {
		selected, err := e.route(ctx, opt.Agent, blocked, opt.NoWait)
		if err != nil {
			return err
		}
		opt.Agent = ""
		outcome, err := e.monitor(ctx, opt.Dir, prompt, selected)
		if err != nil || outcome.complete {
			return err
		}
		fmt.Fprintf(e.Diagnostic, "HANDOFF_GENERATING: %s\n", selected.Provider)
		if err := e.saveHandoff(ctx, handoff{
			dir: opt.Dir, path: path, task: task, tail: string(outcome.result.Tail),
			usage: outcome.usage, sequence: count, localOnly: outcome.localOnly,
		}); err != nil {
			return err
		}
		if count >= e.Config.MaxHandoffCount {
			return failure(model.EMaxRelayExceeded, fmt.Errorf("handoff saved to %s", path))
		}
		blocked, prompt = outcome.usage, continuation
	}
}

type execution struct {
	result infra.ProcessResult
	err    error
}

type outcome struct {
	result    infra.ProcessResult
	usage     model.UsageSnapshot
	complete  bool
	localOnly bool
}

func (e Engine) monitor(ctx context.Context, dir, prompt string, current model.UsageSnapshot) (outcome, error) {
	parentCtx := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	p := e.Config.Providers[current.Provider]
	finished := make(chan execution, 1)
	fmt.Fprintf(e.Diagnostic, "RUNNING: %s\n", current.Provider)
	go func() {
		r, err := e.Runner.Run(ctx, infra.ProcessSpec{
			Command: p.Command, Args: p.Args, Dir: dir, Prompt: []byte(prompt), Diagnostic: e.Diagnostic,
		})
		finished <- execution{r, err}
	}()
	monitorCtx, stopMonitor := context.WithCancel(ctx)
	defer stopMonitor()
	active, ok := e.Collector.Adapter(current.Provider)
	if !ok {
		return outcome{}, failure(model.ENoAvailableProvider, fmt.Errorf("missing adapter for %s", current.Provider))
	}
	collector := provider.Collector{Adapters: []provider.ProviderAdapter{active}}
	collect := func(ctx context.Context) model.UsageSnapshot {
		return collector.Collect(ctx, e.Clock.Now())[0]
	}
	classify := func(exit execution) (outcome, error) {
		if errors.Is(exit.err, infra.ErrProcessCleanup) {
			return outcome{}, failure(model.EInternal, exit.err)
		}
		if err := ctx.Err(); err != nil {
			return outcome{}, err
		}
		if rateLimited(exit.result.Tail) {
			current.Window5hUsedPct = 100
			return outcome{result: exit.result, usage: current, localOnly: true}, nil
		}
		if exit.result.ExitCode == 0 {
			if exit.err != nil {
				return outcome{}, failure(model.EInternal, exit.err)
			}
			return outcome{complete: true}, nil
		}
		fresh := collect(ctx)
		if fresh.Available && effectiveUsedPct(fresh) >= e.Config.SoftThresholdPct {
			return outcome{result: exit.result, usage: fresh, localOnly: effectiveUsedPct(fresh) >= e.Config.HardThresholdPct}, nil
		}
		if exit.err == nil {
			exit.err = fmt.Errorf("%s exited with code %d", current.Provider, exit.result.ExitCode)
		}
		return outcome{}, failure(model.EInternal, exit.err)
	}
	stop := func() (outcome, error) {
		// Prefer an already reaped natural exit over an enforcement timer.
		select {
		case exit := <-finished:
			return classify(exit)
		default:
		}
		stopMonitor()
		cancel()
		exit := <-finished
		if errors.Is(exit.err, infra.ErrProcessCleanup) {
			return outcome{}, failure(model.EInternal, exit.err)
		}
		if parentCtx.Err() != nil {
			return outcome{}, parentCtx.Err()
		}
		if exit.err == nil && exit.result.ExitCode == 0 && !rateLimited(exit.result.Tail) {
			return outcome{complete: true}, nil
		}
		if rateLimited(exit.result.Tail) {
			current.Window5hUsedPct = 100
		} else if effectiveUsedPct(current) < e.Config.HardThresholdPct && parentCtx.Err() == nil {
			fresh := collect(parentCtx)
			if fresh.Available && effectiveUsedPct(fresh) >= e.Config.HardThresholdPct {
				current = fresh
			}
		}
		return outcome{result: exit.result, usage: current, localOnly: effectiveUsedPct(current) >= e.Config.HardThresholdPct}, nil
	}
	interval := seconds(e.Config.PollIntervalSec)
	poll := infra.SleepAsync(monitorCtx, e.Clock, interval)
	var drain <-chan error
	var measurements <-chan model.UsageSnapshot
	for {
		select {
		case <-ctx.Done():
			stopMonitor()
			exit := <-finished
			if errors.Is(exit.err, infra.ErrProcessCleanup) {
				return outcome{}, errors.Join(ctx.Err(), exit.err)
			}
			return outcome{}, ctx.Err()
		case exit := <-finished:
			stopMonitor()
			return classify(exit)
		case err := <-drain:
			if err != nil {
				cancel()
				<-finished
				return outcome{}, err
			}
			return stop()
		case err := <-poll:
			if err != nil {
				cancel()
				<-finished
				return outcome{}, err
			}
			poll = nil
			measured := make(chan model.UsageSnapshot, 1)
			measurements = measured
			go func() { measured <- collect(monitorCtx) }()
		case measured := <-measurements:
			measurements = nil
			if measured.Available {
				used := effectiveUsedPct(measured)
				if used >= e.Config.HardThresholdPct {
					current = measured
					return stop()
				}
				if drain == nil && used >= e.Config.SoftThresholdPct {
					current = measured
					fmt.Fprintf(e.Diagnostic, "DRAINING: %s (%.1f%%)\n", current.Provider, used)
					if e.Config.SoftDrainGraceSec == 0 {
						return stop()
					}
					drain = infra.SleepAsync(monitorCtx, e.Clock, seconds(e.Config.SoftDrainGraceSec))
				}
			}
			poll = infra.SleepAsync(monitorCtx, e.Clock, interval)
		}
	}
}
