package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	codegen "github.com/spice-framework/toolchain/compiler/generate"
	"github.com/spice-framework/toolchain/compiler/load"
	"github.com/spice-framework/toolchain/internal/genfs"
)

const applicationProcessStopTimeout = 15 * time.Second

type runArguments struct {
	generation  generationArguments
	application []string
}

type applicationBuildExecutor func(
	context.Context,
	string,
	string,
	string,
	io.Writer,
	io.Writer,
) error

type applicationRunExecutor func(
	context.Context,
	string,
	[]string,
	io.Reader,
	io.Writer,
	io.Writer,
) (int, error)

// NewRunHandler constructs the generated-application execution handler.
func NewRunHandler(runtime *Runtime) (Handler, error) {
	return newLoaderCommandHandler(
		runtime,
		[]string{"run"},
		func(runtime *Runtime, invocation Invocation) int {
			return runCommandWithExecutors(
				context.Background(),
				invocation.Arguments,
				invocation.Stdin,
				invocation.Stdout,
				invocation.Stderr,
				runtime.options,
				runtime.loader,
				executeApplicationBuild,
				executeApplication,
			)
		},
	)
}

func runCommandWithExecutors(
	ctx context.Context,
	arguments []string,
	stdin io.Reader,
	stdout io.Writer,
	stderr io.Writer,
	options load.Options,
	loader programLoader,
	builder applicationBuildExecutor,
	executor applicationRunExecutor,
) int {
	parsed, err := parseRunArguments(arguments)
	if err != nil {
		if writeErr := writef(stderr, "Spice run failed: %v\n", err); writeErr != nil {
			return 1
		}
		return 2
	}
	plan, targetName, ok := prepareGeneration(
		parsed.generation,
		stderr,
		options,
		loader,
	)
	if !ok {
		return 1
	}
	target := plan.Target()
	if target.Layout != codegen.LayoutApplicationPackage {
		if writeErr := writef(
			stderr,
			"Spice run failed for target %s: legacy @Application markers do not define a runnable package main; migrate the marker to func main\n",
			targetName,
		); writeErr != nil {
			return 1
		}
		return 1
	}
	if !applyRunGeneration(plan, targetName, stderr) {
		return 1
	}
	return buildAndRunApplication(
		ctx,
		plan,
		targetName,
		parsed.application,
		stdin,
		stdout,
		stderr,
		builder,
		executor,
	)
}

func parseRunArguments(arguments []string) (runArguments, error) {
	separator := len(arguments)
	for index, argument := range arguments {
		if argument == "--" {
			separator = index
			break
		}
	}
	generation, err := parseGenerationArguments(arguments[:separator], false)
	if err != nil {
		return runArguments{}, err
	}
	var application []string
	if separator < len(arguments) {
		application = append([]string(nil), arguments[separator+1:]...)
	}
	return runArguments{
		generation:  generation,
		application: application,
	}, nil
}

func applyRunGeneration(
	plan codegen.Plan,
	targetName string,
	stderr io.Writer,
) bool {
	result, err := genfs.Apply(plan)
	if err != nil {
		if writeErr := writef(
			stderr,
			"Spice run generation failed for target %s: %v\n",
			targetName,
			err,
		); writeErr != nil {
			return false
		}
		return false
	}
	if !result.Changed() {
		return true
	}
	return writef(
		stderr,
		"Spice generated target %s: wrote %d file(s), removed %d stale file(s), manifest updated=%t.\n",
		targetName,
		len(result.Written),
		len(result.Removed),
		result.ManifestUpdated,
	) == nil
}

func buildAndRunApplication(
	ctx context.Context,
	plan codegen.Plan,
	targetName string,
	arguments []string,
	stdin io.Reader,
	stdout io.Writer,
	stderr io.Writer,
	builder applicationBuildExecutor,
	executor applicationRunExecutor,
) int {
	temporaryDirectory, err := os.MkdirTemp("", "spice-run-*")
	if err != nil {
		if writeErr := writef(stderr, "Spice run failed for target %s: create temporary build directory: %v\n", targetName, err); writeErr != nil {
			return 1
		}
		return 1
	}
	executable := filepath.Join(
		temporaryDirectory,
		"application"+applicationExecutableSuffix(),
	)
	code := buildAndExecute(
		ctx,
		plan.Target(),
		targetName,
		executable,
		arguments,
		stdin,
		stdout,
		stderr,
		builder,
		executor,
	)
	if removeErr := os.RemoveAll(temporaryDirectory); removeErr != nil {
		if writeErr := writef(
			stderr,
			"Spice run failed for target %s: remove temporary build directory: %v\n",
			targetName,
			removeErr,
		); writeErr != nil || code == 0 {
			return 1
		}
	}
	return code
}

func buildAndExecute(
	ctx context.Context,
	target codegen.Target,
	targetName string,
	executable string,
	arguments []string,
	stdin io.Reader,
	stdout io.Writer,
	stderr io.Writer,
	builder applicationBuildExecutor,
	executor applicationRunExecutor,
) int {
	if err := builder(
		ctx,
		target.ModuleRoot,
		target.EntrypointPackagePath,
		executable,
		stdout,
		stderr,
	); err != nil {
		if writeErr := writef(stderr, "Spice run build failed for target %s: %v\n", targetName, err); writeErr != nil {
			return 1
		}
		return 1
	}
	if err := writef(stderr, "Spice running target %s.\n", targetName); err != nil {
		return 1
	}
	code, err := executor(
		ctx,
		executable,
		arguments,
		stdin,
		stdout,
		stderr,
	)
	if err != nil {
		if writeErr := writef(stderr, "Spice run failed for target %s: %v\n", targetName, err); writeErr != nil {
			return 1
		}
		return 1
	}
	return code
}

func applicationExecutableSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

func executeApplicationBuild(
	ctx context.Context,
	directory string,
	packagePath string,
	outputPath string,
	stdout io.Writer,
	stderr io.Writer,
) error {
	if err := validateApplicationPackagePath(packagePath); err != nil {
		return err
	}
	// #nosec G204 -- packagePath is a compiler-owned import identity and every
	// argument is passed directly to the fixed Go executable without a shell.
	command := exec.CommandContext(
		ctx,
		"go",
		"build",
		"-trimpath",
		"-o",
		outputPath,
		packagePath,
	)
	command.Dir = directory
	command.Env = os.Environ()
	command.Stdout = stdout
	command.Stderr = stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf(
			"go build -trimpath -o <temporary> %s: %w",
			packagePath,
			err,
		)
	}
	return nil
}

func executeApplication(
	ctx context.Context,
	executable string,
	arguments []string,
	stdin io.Reader,
	stdout io.Writer,
	stderr io.Writer,
) (int, error) {
	if err := ctx.Err(); err != nil {
		return 1, err
	}
	// #nosec G204 -- executable is the exact temporary artifact just produced
	// by executeApplicationBuild and arguments are passed without a shell.
	command := exec.CommandContext(
		context.WithoutCancel(ctx),
		executable,
		arguments...,
	)
	configureApplicationProcess(command)
	command.Stdin = stdin
	command.Stdout = stdout
	command.Stderr = stderr
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, applicationTerminationSignals()...)
	defer signal.Stop(signals)
	if err := command.Start(); err != nil {
		return 1, fmt.Errorf("start application: %w", err)
	}
	return waitForApplication(ctx, command, signals)
}

func waitForApplication(
	ctx context.Context,
	command *exec.Cmd,
	signals <-chan os.Signal,
) (int, error) {
	wait := make(chan error, 1)
	go func() {
		wait <- command.Wait()
	}()

	contextDone := ctx.Done()
	var stopTimer *time.Timer
	var forceStop <-chan time.Time
	stopping := false
	for {
		select {
		case err := <-wait:
			if stopTimer != nil {
				stopTimer.Stop()
			}
			return applicationExitCode(err)
		case <-contextDone:
			contextDone = nil
			if !stopping {
				stopping = true
				if err := interruptApplicationProcess(command.Process, os.Interrupt); err != nil {
					return killAndWait(command.Process, wait, err)
				}
				stopTimer = time.NewTimer(applicationProcessStopTimeout)
				forceStop = stopTimer.C
			}
		case received := <-signals:
			if stopping {
				return killAndWait(command.Process, wait, nil)
			}
			stopping = true
			if err := interruptApplicationProcess(command.Process, received); err != nil {
				return killAndWait(command.Process, wait, err)
			}
			stopTimer = time.NewTimer(applicationProcessStopTimeout)
			forceStop = stopTimer.C
		case <-forceStop:
			return killAndWait(
				command.Process,
				wait,
				errors.New("application did not stop before the termination timeout"),
			)
		}
	}
}

func killAndWait(
	process *os.Process,
	wait <-chan error,
	cause error,
) (int, error) {
	killErr := killApplicationProcess(process)
	waitErr := <-wait
	if errors.Is(killErr, os.ErrProcessDone) {
		killErr = nil
	}
	exitError, exitErrorFound := errors.AsType[*exec.ExitError](waitErr)
	if exitErrorFound && exitError != nil {
		waitErr = nil
	}
	return 1, errors.Join(cause, killErr, waitErr)
}

func applicationExitCode(err error) (int, error) {
	if err == nil {
		return 0, nil
	}
	exitError, ok := errors.AsType[*exec.ExitError](err)
	if !ok {
		return 1, fmt.Errorf("wait for application: %w", err)
	}
	code := exitError.ExitCode()
	if code < 0 {
		return 1, errors.New("application terminated by a signal")
	}
	return code, nil
}

func validateApplicationPackagePath(packagePath string) error {
	if packagePath == "" || strings.HasPrefix(packagePath, "-") {
		return fmt.Errorf("invalid application package path %q", packagePath)
	}
	return nil
}
