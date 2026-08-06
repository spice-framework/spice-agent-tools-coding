package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	codegen "github.com/spice-framework/toolchain/compiler/generate"
	"github.com/spice-framework/toolchain/compiler/load"
	compilerservice "github.com/spice-framework/toolchain/compiler/service"
	"github.com/spice-framework/toolchain/internal/devloop"
	"github.com/spice-framework/toolchain/internal/genfs"
)

const maximumDevelopmentOutput = 256 << 10

type devArguments struct {
	generation  generationArguments
	application []string
	quiet       time.Duration
	maxDelay    time.Duration
	poll        time.Duration
	stopTimeout time.Duration
	includes    []string
	excludes    []string
}

// NewDevHandler constructs the last-known-good development-loop handler.
func NewDevHandler(runtime *Runtime) (Handler, error) {
	return newLoaderCommandHandler(
		runtime,
		[]string{"dev"},
		func(runtime *Runtime, invocation Invocation) int {
			ctx, stop := signal.NotifyContext(
				context.Background(),
				applicationTerminationSignals()...,
			)
			defer stop()
			return devCommandContext(
				ctx,
				invocation.Arguments,
				invocation.Stdin,
				invocation.Stdout,
				invocation.Stderr,
				runtime.options,
				runtime.loader,
				executeApplicationBuild,
			)
		},
	)
}

func devCommandContext(
	ctx context.Context,
	arguments []string,
	stdin io.Reader,
	stdout io.Writer,
	stderr io.Writer,
	options load.Options,
	loader programLoader,
	builder applicationBuildExecutor,
) (exitCode int) {
	parsed, err := parseDevArguments(arguments)
	if err != nil {
		if writeErr := writef(stderr, "Spice dev failed: %v\n", err); writeErr != nil {
			return 1
		}
		return 2
	}
	root := options.Dir
	if root == "" {
		root = "."
	}
	analysisService, err := newCompilerAnalysisService(options, loader)
	if err != nil {
		if writeErr := writef(stderr, "Spice dev failed: %v\n", err); writeErr != nil {
			return 1
		}
		return 1
	}
	defer func() {
		if closeErr := closeCompilerAnalysisService(
			analysisService,
		); closeErr != nil {
			if writeErr := writef(
				stderr,
				"Spice dev failed: close annotation tools: %v\n",
				closeErr,
			); writeErr != nil {
				exitCode = 1
				return
			}
			exitCode = 1
		}
	}()
	watcher, err := devloop.NewPollingWatcher(
		// Engine.Run owns watcher shutdown. Detaching the polling resource from
		// the caller's cancellation prevents its event channel from closing
		// before the engine's derived run context observes the same cancellation.
		developmentWatcherContext(ctx),
		devloop.PollingConfig{
			Root:     root,
			Interval: parsed.poll,
			PathRules: devloop.PathRules{
				Include: parsed.includes,
				Exclude: parsed.excludes,
			},
		},
		nil,
	)
	if err != nil {
		if writeErr := writef(stderr, "Spice dev failed: create watcher: %v\n", err); writeErr != nil {
			return 1
		}
		return 1
	}
	sink := &developmentEventWriter{writer: stderr}
	pipeline := &developmentPipeline{
		generation: parsed.generation,
		root:       root,
		service:    analysisService,
		builder:    builder,
		sink:       sink,
	}
	launcher := &developmentLauncher{
		arguments: parsed.application,
		stdin:     stdin,
		stdout:    stdout,
		stderr:    stderr,
	}
	engine, err := devloop.NewEngine(
		devloop.Config{
			QuietPeriod: parsed.quiet,
			MaxDelay:    parsed.maxDelay,
			StopTimeout: parsed.stopTimeout,
		},
		watcher,
		nil,
		pipeline,
		launcher,
		sink,
	)
	if err != nil {
		closeErr := watcher.Close()
		if writeErr := writef(
			stderr,
			"Spice dev failed: configure supervisor: %v\n",
			errors.Join(err, closeErr),
		); writeErr != nil {
			return 1
		}
		return 2
	}
	runErr := engine.Run(ctx)
	if sinkErr := sink.Err(); sinkErr != nil {
		runErr = errors.Join(runErr, sinkErr)
	}
	if runErr != nil {
		if writeErr := writef(stderr, "Spice dev failed: %v\n", runErr); writeErr != nil {
			return 1
		}
		return 1
	}
	return 0
}

func developmentWatcherContext(ctx context.Context) context.Context {
	return context.WithoutCancel(ctx)
}

func parseDevArguments(arguments []string) (devArguments, error) {
	separator := len(arguments)
	for index, argument := range arguments {
		if argument == "--" {
			separator = index
			break
		}
	}
	var result devArguments
	var generation []string
	for index := 0; index < separator; index++ {
		next, handled, err := parseDevOption(
			arguments[:separator],
			index,
			&result,
		)
		if err != nil {
			return devArguments{}, err
		}
		if handled {
			index = next
			continue
		}
		generation = append(generation, arguments[index])
	}
	parsedGeneration, err := parseGenerationArguments(generation, false)
	if err != nil {
		return devArguments{}, err
	}
	result.generation = parsedGeneration
	if separator < len(arguments) {
		result.application = append(
			[]string(nil),
			arguments[separator+1:]...,
		)
	}
	return result, nil
}

func parseDevOption(
	arguments []string,
	index int,
	result *devArguments,
) (int, bool, error) {
	next, handled, err := parseDevDurationOption(arguments, index, result)
	if handled || err != nil {
		return next, handled, err
	}
	return parseDevPathOption(arguments, index, result)
}

func parseDevDurationOption(
	arguments []string,
	index int,
	result *devArguments,
) (int, bool, error) {
	argument := arguments[index]
	switch {
	case argument == "--quiet" || strings.HasPrefix(argument, "--quiet="):
		next, err := setDevDuration(
			arguments,
			index,
			"--quiet",
			&result.quiet,
		)
		return next, true, err
	case argument == "--max-delay" ||
		strings.HasPrefix(argument, "--max-delay="):
		next, err := setDevDuration(
			arguments,
			index,
			"--max-delay",
			&result.maxDelay,
		)
		return next, true, err
	case argument == "--poll" || strings.HasPrefix(argument, "--poll="):
		next, err := setDevDuration(
			arguments,
			index,
			"--poll",
			&result.poll,
		)
		return next, true, err
	case argument == "--stop-timeout" ||
		strings.HasPrefix(argument, "--stop-timeout="):
		next, err := setDevDuration(
			arguments,
			index,
			"--stop-timeout",
			&result.stopTimeout,
		)
		return next, true, err
	default:
		return index, false, nil
	}
}

func parseDevPathOption(
	arguments []string,
	index int,
	result *devArguments,
) (int, bool, error) {
	argument := arguments[index]
	switch {
	case argument == "--include" || strings.HasPrefix(argument, "--include="):
		next, value, err := devOptionValue(
			arguments,
			index,
			"--include",
		)
		if err == nil {
			result.includes = append(result.includes, value)
		}
		return next, true, err
	case argument == "--exclude" ||
		strings.HasPrefix(argument, "--exclude="):
		next, value, err := devOptionValue(
			arguments,
			index,
			"--exclude",
		)
		if err == nil {
			result.excludes = append(result.excludes, value)
		}
		return next, true, err
	default:
		return index, false, nil
	}
}

func setDevDuration(
	arguments []string,
	index int,
	name string,
	target *time.Duration,
) (int, error) {
	if *target != 0 {
		return index, fmt.Errorf("%s may be specified only once", name)
	}
	next, value, err := devOptionValue(arguments, index, name)
	if err != nil {
		return index, err
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return index, fmt.Errorf(
			"%s requires a positive Go duration, got %q",
			name,
			value,
		)
	}
	*target = duration
	return next, nil
}

func devOptionValue(
	arguments []string,
	index int,
	name string,
) (int, string, error) {
	argument := arguments[index]
	if value, found := strings.CutPrefix(argument, name+"="); found {
		if value == "" {
			return index, "", fmt.Errorf("%s requires a value", name)
		}
		return index, value, nil
	}
	if index+1 >= len(arguments) ||
		strings.HasPrefix(arguments[index+1], "--") {
		return index, "", fmt.Errorf("%s requires a value", name)
	}
	return index + 1, arguments[index+1], nil
}

type developmentPipeline struct {
	generation generationArguments
	root       string
	service    *compilerservice.Service
	builder    applicationBuildExecutor
	sink       devloop.EventSink

	cacheMu            sync.Mutex
	cachedPlan         codegen.Plan
	cachedTargetName   string
	cachedStructures   map[string][32]byte
	hasGenerationCache bool
}

func (pipeline *developmentPipeline) Prepare(
	ctx context.Context,
	batch devloop.Batch,
) (devloop.Candidate, error) {
	output := newBoundedOutput(maximumDevelopmentOutput)
	generationStarted := time.Now()
	plan, targetName, reused, err := pipeline.reusableGeneration(batch)
	generatedChanged := false
	if err != nil {
		return devloop.Candidate{}, err
	}
	if !reused {
		var ok bool
		plan, targetName, ok = prepareGenerationAnalysis(
			ctx,
			pipeline.generation,
			output,
			pipeline.root,
			pipeline.service,
			batch.Revision(),
		)
		if !ok {
			if contextErr := ctx.Err(); contextErr != nil {
				return devloop.Candidate{}, contextErr
			}
			return devloop.Candidate{}, errors.New(output.String())
		}
		if plan.Target().Layout != codegen.LayoutApplicationPackage {
			return devloop.Candidate{}, fmt.Errorf(
				"target %s uses the legacy generated-package layout; move @Application to package main",
				targetName,
			)
		}
		result, applyErr := genfs.Apply(plan)
		if applyErr != nil {
			return devloop.Candidate{}, fmt.Errorf(
				"generate target %s: %w",
				targetName,
				applyErr,
			)
		}
		generatedChanged = result.Changed()
		if cacheErr := pipeline.rememberGeneration(ctx, plan, targetName); cacheErr != nil {
			return devloop.Candidate{}, cacheErr
		}
	}
	pipeline.sink.Emit(devloop.Event{
		Kind:     devloop.EventGenerationComplete,
		Revision: batch.Revision(),
		Duration: time.Since(generationStarted),
		Reused:   reused,
	})
	temporaryDirectory, err := os.MkdirTemp("", "spice-dev-*")
	if err != nil {
		return devloop.Candidate{}, fmt.Errorf(
			"create candidate directory for %s: %w",
			targetName,
			err,
		)
	}
	executable := filepath.Join(
		temporaryDirectory,
		"application"+applicationExecutableSuffix(),
	)
	output.Reset()
	buildStarted := time.Now()
	if buildErr := pipeline.builder(
		ctx,
		plan.Target().ModuleRoot,
		plan.Target().EntrypointPackagePath,
		executable,
		output,
		output,
	); buildErr != nil {
		removeErr := os.RemoveAll(temporaryDirectory)
		return devloop.Candidate{}, errors.Join(
			fmt.Errorf(
				"build target %s: %w\n%s",
				targetName,
				buildErr,
				output.String(),
			),
			removeErr,
		)
	}
	candidate, err := devloop.NewCandidate(executable, func() error {
		return os.RemoveAll(temporaryDirectory)
	})
	if err != nil {
		removeErr := os.RemoveAll(temporaryDirectory)
		return devloop.Candidate{}, errors.Join(err, removeErr)
	}
	pipeline.sink.Emit(devloop.Event{
		Kind:     devloop.EventBuildComplete,
		Revision: batch.Revision(),
		Stale:    generatedChanged,
		Duration: time.Since(buildStarted),
	})
	return candidate, nil
}

type developmentLauncher struct {
	arguments []string
	stdin     io.Reader
	stdout    io.Writer
	stderr    io.Writer
}

func (launcher *developmentLauncher) Start(
	ctx context.Context,
	candidate devloop.Candidate,
) (devloop.Process, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// #nosec G204 -- the executable is the exact unique artifact owned by the
	// prepared candidate and arguments are passed directly without a shell.
	command := exec.CommandContext(
		context.WithoutCancel(ctx),
		candidate.Artifact(),
		launcher.arguments...,
	)
	configureApplicationProcess(command)
	command.Stdin = launcher.stdin
	command.Stdout = launcher.stdout
	command.Stderr = launcher.stderr
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("start development application: %w", err)
	}
	process := &developmentProcess{
		command: command,
		exits:   make(chan error, 1),
		done:    make(chan struct{}),
	}
	go process.wait()
	return process, nil
}

type developmentProcess struct {
	command *exec.Cmd
	exits   chan error
	done    chan struct{}

	stopOnce sync.Once
	stopErr  error
	killOnce sync.Once
	killErr  error
}

func (process *developmentProcess) Wait() <-chan error {
	return process.exits
}

func (process *developmentProcess) wait() {
	err := process.command.Wait()
	process.exits <- err
	close(process.exits)
	close(process.done)
}

func (process *developmentProcess) Stop(ctx context.Context) error {
	if ctx == nil {
		return errors.New("development process stop context must not be nil")
	}
	select {
	case <-process.done:
		return nil
	default:
	}
	process.stopOnce.Do(func() {
		process.stopErr = interruptApplicationProcess(
			process.command.Process,
			os.Interrupt,
		)
		if errors.Is(process.stopErr, os.ErrProcessDone) {
			process.stopErr = nil
		}
		if process.stopErr != nil {
			process.kill()
		}
	})
	select {
	case <-process.done:
		return errors.Join(process.stopErr, process.killErr)
	case <-ctx.Done():
		process.kill()
		<-process.done
		if process.killErr != nil {
			return errors.Join(
				process.stopErr,
				ctx.Err(),
				process.killErr,
			)
		}
		return errors.Join(
			devloop.ErrGracefulStopTimeout,
			ctx.Err(),
		)
	}
}

func (process *developmentProcess) kill() {
	process.killOnce.Do(func() {
		process.killErr = killApplicationProcess(process.command.Process)
		if errors.Is(process.killErr, os.ErrProcessDone) {
			process.killErr = nil
		}
	})
}

type developmentEventWriter struct {
	writer io.Writer
	mu     sync.Mutex
	err    error
}

func (sink *developmentEventWriter) Emit(event devloop.Event) {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if sink.err != nil {
		return
	}
	sink.err = sink.write(event)
}

func (sink *developmentEventWriter) Err() error {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return sink.err
}

func (sink *developmentEventWriter) write(event devloop.Event) error {
	switch event.Kind {
	case devloop.EventChangeDetected:
		paths := slices.Clone(event.Paths)
		slices.Sort(paths)
		return writef(
			sink.writer,
			"spice dev: change detected: %s\n",
			strings.Join(paths, ", "),
		)
	case devloop.EventAnalysisStarted:
		return writef(
			sink.writer,
			"spice dev: analysis started (revision %d)\n",
			event.Revision,
		)
	case devloop.EventGenerationComplete:
		status := ""
		if event.Reused {
			status = ", structural model reused"
		}
		return writef(
			sink.writer,
			"spice dev: generation complete (revision %d); duration=%s%s\n",
			event.Revision,
			event.Duration.Round(time.Millisecond),
			status,
		)
	case devloop.EventBuildComplete:
		return writef(
			sink.writer,
			"spice dev: build complete (revision %d); duration=%s\n",
			event.Revision,
			event.Duration.Round(time.Millisecond),
		)
	case devloop.EventCandidateReady:
		return nil
	case devloop.EventPreparationFailed:
		return writef(
			sink.writer,
			"spice dev: revision %d failed: %v\n",
			event.Revision,
			event.Err,
		)
	case devloop.EventLastKnownGood:
		return writef(
			sink.writer,
			"spice dev: application remains on last-known-good revision %d\n",
			event.Revision,
		)
	case devloop.EventCandidateDiscarded:
		return writef(
			sink.writer,
			"spice dev: discarded obsolete revision %d\n",
			event.Revision,
		)
	case devloop.EventRestartRequested,
		devloop.EventApplicationStarted,
		devloop.EventApplicationExited,
		devloop.EventWatchError,
		devloop.EventCleanupFailed,
		devloop.EventShutdownTimedOut:
		return sink.writeProcessEvent(event)
	default:
		return fmt.Errorf("unsupported development event kind %q", event.Kind)
	}
}

func (sink *developmentEventWriter) writeProcessEvent(
	event devloop.Event,
) error {
	switch event.Kind {
	case devloop.EventRestartRequested:
		return writef(
			sink.writer,
			"spice dev: graceful restart requested for revision %d\n",
			event.Revision,
		)
	case devloop.EventApplicationStarted:
		return writef(
			sink.writer,
			"spice dev: application started (revision %d)\n",
			event.Revision,
		)
	case devloop.EventApplicationExited:
		if event.Err == nil {
			return writef(
				sink.writer,
				"spice dev: application exited (revision %d)\n",
				event.Revision,
			)
		}
		return writef(
			sink.writer,
			"spice dev: application exited (revision %d): %v\n",
			event.Revision,
			event.Err,
		)
	case devloop.EventWatchError:
		return writef(
			sink.writer,
			"spice dev: watcher recovered from error: %v\n",
			event.Err,
		)
	case devloop.EventCleanupFailed:
		return writef(
			sink.writer,
			"spice dev: candidate cleanup failed: %v\n",
			event.Err,
		)
	case devloop.EventShutdownTimedOut:
		return writef(
			sink.writer,
			"spice dev: graceful shutdown timed out for revision %d; process escalation completed\n",
			event.Revision,
		)
	case devloop.EventChangeDetected,
		devloop.EventAnalysisStarted,
		devloop.EventGenerationComplete,
		devloop.EventBuildComplete,
		devloop.EventCandidateReady,
		devloop.EventPreparationFailed,
		devloop.EventLastKnownGood,
		devloop.EventCandidateDiscarded:
		return fmt.Errorf(
			"non-process development event kind %q",
			event.Kind,
		)
	default:
		return fmt.Errorf("unsupported development event kind %q", event.Kind)
	}
}

type boundedOutput struct {
	buffer    bytes.Buffer
	limit     int
	truncated bool
}

func newBoundedOutput(limit int) *boundedOutput {
	return &boundedOutput{limit: limit}
}

func (output *boundedOutput) Write(content []byte) (int, error) {
	original := len(content)
	remaining := output.limit - output.buffer.Len()
	if remaining <= 0 {
		output.truncated = output.truncated || original != 0
		return original, nil
	}
	if len(content) > remaining {
		content = content[:remaining]
		output.truncated = true
	}
	_, err := output.buffer.Write(content)
	return original, err
}

func (output *boundedOutput) String() string {
	value := strings.TrimSpace(output.buffer.String())
	if output.truncated {
		return value + "\n... output truncated"
	}
	return value
}

func (output *boundedOutput) Reset() {
	output.buffer.Reset()
	output.truncated = false
}
