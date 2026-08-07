package coding

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/spice-framework/spice-agent/process"
	"github.com/spice-framework/spice-agent/tool"
	"github.com/spice-framework/spice/lifecycle"
)

const (
	shellInputSchema = `{"type":"object","additionalProperties":false,"required":["argv"],"properties":{"argv":{"type":"array","minItems":1,"maxItems":128,"items":{"type":"string"}},"workdir":{"type":"string"}}}`
	maximumArguments = 128
	maximumArgvBytes = 64 << 10
)

type shellTool struct {
	config       Config
	definition   tool.Definition
	resolver     process.ExecutableResolver
	launcher     process.Launcher
	ownershipMu  sync.Mutex
	closing      bool
	active       int
	reservations int
	owned        []*ownedProcess
	drained      chan struct{}
	drainedOnce  sync.Once
	cleanupToken chan struct{}
}

type shellArguments struct {
	Argv    []string `json:"argv"`
	Workdir string   `json:"workdir,omitempty"`
}

type shellContent struct {
	OK                      bool   `json:"ok"`
	Workdir                 string `json:"workdir"`
	ExitCode                int64  `json:"exit_code"`
	Stdout                  string `json:"stdout"`
	Stderr                  string `json:"stderr"`
	Encoding                string `json:"encoding"`
	StdoutBytes             int64  `json:"stdout_bytes"`
	StderrBytes             int64  `json:"stderr_bytes"`
	OutputTruncated         bool   `json:"output_truncated"`
	TimedOut                bool   `json:"timed_out"`
	ManagedCleanupCompleted bool   `json:"managed_cleanup_completed"`
}

// NewShell constructs the exact Spice Agent discrete-argv shell tool binding
// without starting a process.
func NewShell(
	config Config,
	resolver process.ExecutableResolver,
	launcher process.Launcher,
) (tool.Tool, lifecycle.Cleanup, error) {
	normalized, err := validatedConfig(config)
	if err != nil {
		return nil, nil, err
	}
	if resolver == nil {
		return nil, nil, errors.New("shell executable resolver must not be nil")
	}
	if launcher == nil {
		return nil, nil, errors.New("shell process launcher must not be nil")
	}
	definition, err := tool.NewDefinition(
		"shell",
		"Execute unsandboxed discrete argv from a worktree-selected directory with bounded output.",
		json.RawMessage(shellInputSchema),
		tool.EffectMutating,
		tool.ReplayUnsafe,
		tool.CapabilityFilesystemRead,
		tool.CapabilityFilesystemWrite,
		tool.CapabilityProcessExecute,
		tool.CapabilityNetworkAccess,
		tool.CapabilitySecretsRead,
		tool.CapabilityEnvironmentRead,
		tool.CapabilityEnvironmentWrite,
	)
	if err != nil {
		return nil, nil, err
	}
	shell := &shellTool{
		config: normalized, definition: definition, resolver: resolver, launcher: launcher,
		drained: make(chan struct{}), cleanupToken: make(chan struct{}, 1),
	}
	return shell, shell.cleanup, nil
}

func (shell *shellTool) Definition() tool.Definition { return shell.definition.Clone() }

func (shell *shellTool) Execute(ctx context.Context, call tool.Call, reporter tool.Reporter) (tool.Result, error) {
	if err := validateCall(call, shell.definition.Name()); err != nil {
		return modelFailure(call.ID(), err)
	}
	if result, err, stop := initialContextOutcome(ctx, call.ID(), tool.RetryNever); stop {
		return result, err
	}
	var arguments shellArguments
	if err := decodeArguments(call.Arguments(), &arguments); err != nil {
		return modelFailure(call.ID(), err)
	}
	if err := validateArgv(arguments.Argv); err != nil {
		return modelFailure(call.ID(), err)
	}
	workdir := arguments.Workdir
	if workdir == "" {
		workdir = "."
	}
	path, err := parseRelativePath(workdir, true)
	if err != nil {
		return modelFailure(call.ID(), err)
	}
	if progressErr := reportProgress(
		ctx, reporter, call.ID(), "starting bounded child process", tool.RetryNever,
	); progressErr != nil {
		return tool.Result{}, progressErr
	}
	lease, err := shell.beginExecution()
	if err != nil {
		return modelFailure(call.ID(), err)
	}
	defer lease.finish()
	content, problem, err := shell.execute(ctx, arguments.Argv, path, lease)
	if err != nil {
		if uncertain, present := errors.AsType[executionUncertainty](err); present {
			if uncertain.preserveContext {
				return tool.Result{}, contextInfrastructureFailure(
					call.ID(), uncertain.state, tool.RetryNever,
					uncertain.message, uncertain.cause,
				)
			}
			return tool.Result{}, infrastructureFailure(
				call.ID(), uncertain.state, tool.RetryNever,
				uncertain.message,
			)
		}
		return modelFailure(call.ID(), err)
	}
	result, err := encodeShellResult(call.ID(), content, problem)
	if err != nil {
		return tool.Result{}, infrastructureFailure(
			call.ID(), tool.ExecutionUncertain, tool.RetryNever,
			"command result could not be encoded",
		)
	}
	return result, nil
}

func (shell *shellTool) execute(
	ctx context.Context,
	argv []string,
	workdir relativePath,
	lease *executionLease,
) (shellContent, string, error) {
	workspace, directory, spec, stdout, stderr, err := shell.prepareProcess(ctx, argv, workdir)
	if err != nil {
		return shellContent{}, "", err
	}
	defer closeBestEffort(workspace)
	defer closeBestEffort(directory)
	outcome := shell.runProcess(ctx, spec, lease)
	content := capturedShellContent(workdir, stdout, stderr, outcome)
	problem, err := classifyProcessOutcome(outcome)
	if err != nil {
		return shellContent{}, "", err
	}
	return content, problem, nil
}

func (shell *shellTool) prepareProcess(
	ctx context.Context,
	argv []string,
	workdir relativePath,
) (*os.Root, *os.Root, process.Spec, *boundedCapture, *boundedCapture, error) {
	workspace, err := openWorktree(shell.config.Root)
	if err != nil {
		return nil, nil, process.Spec{}, nil, nil, err
	}
	if pathErr := rejectSymlinkComponents(workspace, workdir); pathErr != nil {
		closeBestEffort(workspace)
		return nil, nil, process.Spec{}, nil, nil, pathErr
	}
	directory, err := workspace.OpenRoot(workdir.native)
	if err != nil {
		closeBestEffort(workspace)
		return nil, nil, process.Spec{}, nil, nil,
			executionFailure("workdir_unavailable", "command workdir is unavailable or escapes the configured worktree")
	}
	info, err := directory.Stat(".")
	if err != nil || !info.IsDir() {
		closeBestEffort(directory)
		closeBestEffort(workspace)
		return nil, nil, process.Spec{}, nil, nil, executionFailure("workdir_unavailable", "command workdir is not a directory")
	}
	stdout, stderr := newCapturePair(shell.config.MaxOutputBytes)
	environment := selectedEnvironment(shell.config.EnvironmentAllowlist)
	workingDirectory := filepath.Join(shell.config.Root, workdir.native)
	lookup, err := process.NewLookup(argv[0], workingDirectory, environment)
	if err != nil {
		closeBestEffort(directory)
		closeBestEffort(workspace)
		return nil, nil, process.Spec{}, nil, nil, executionUncertainty{
			message: "command executable lookup could not be prepared", cause: err,
			state: tool.ExecutionDefinitive,
		}
	}
	resolved, err := shell.resolver.Resolve(ctx, lookup)
	if err != nil {
		closeBestEffort(directory)
		closeBestEffort(workspace)
		return nil, nil, process.Spec{}, nil, nil, resolutionUncertainty(ctx, err)
	}
	spec, err := process.NewSpec(process.Config{
		Executable: resolved, Arguments: argv[1:], WorkingDirectory: workingDirectory,
		Environment: environment, Stdin: strings.NewReader(""), Stdout: stdout, Stderr: stderr,
		Capabilities: shell.definition.Capabilities(),
	})
	if err != nil {
		closeBestEffort(directory)
		closeBestEffort(workspace)
		return nil, nil, process.Spec{}, nil, nil, executionUncertainty{
			message: "resolved command executable is invalid", cause: err,
			state: tool.ExecutionDefinitive,
		}
	}
	if pathErr := rejectSymlinkComponents(workspace, workdir); pathErr != nil {
		closeBestEffort(directory)
		closeBestEffort(workspace)
		return nil, nil, process.Spec{}, nil, nil, pathErr
	}
	return workspace, directory, spec, stdout, stderr, nil
}

func resolutionUncertainty(ctx context.Context, err error) executionUncertainty {
	wrapped := process.NewFailure(process.OperationResolve, err)
	if ctx != nil && ctx.Err() != nil {
		return executionUncertainty{
			message: "command executable resolution was cancelled", cause: ctx.Err(),
			preserveContext: true, state: tool.ExecutionDefinitive,
		}
	}
	return executionUncertainty{
		message: "command executable could not be resolved", cause: wrapped,
		state: tool.ExecutionDefinitive,
	}
}

func capturedShellContent(
	workdir relativePath,
	stdout, stderr *boundedCapture,
	outcome processOutcome,
) shellContent {
	stdoutContent, stdoutBytes, truncated := stdout.snapshot()
	stderrContent, stderrBytes, stderrTruncated := stderr.snapshot()
	content := shellContent{
		OK: outcome.hasResult && outcome.result.Successful() && outcome.resultErr == nil &&
			outcome.launchErr == nil && outcome.contextErr == nil && !outcome.timedOut &&
			outcome.controlErr == nil && outcome.waitErr == nil,
		Workdir: workdir.display, ExitCode: outcomeExitCode(outcome),
		Stdout: string(stdoutContent), Stderr: string(stderrContent), Encoding: "utf-8",
		StdoutBytes: stdoutBytes, StderrBytes: stderrBytes,
		OutputTruncated: truncated || stderrTruncated, TimedOut: outcome.timedOut,
		ManagedCleanupCompleted: outcome.started && outcome.waitErr == nil,
	}
	if !utf8.Valid(stdoutContent) || !utf8.Valid(stderrContent) {
		content.Encoding = "base64"
		content.Stdout = base64.StdEncoding.EncodeToString(stdoutContent)
		content.Stderr = base64.StdEncoding.EncodeToString(stderrContent)
	}
	return content
}

func classifyProcessOutcome(outcome processOutcome) (string, error) {
	managedCleanupCompleted := !outcome.started || outcome.waitErr == nil
	if outcome.contextErr != nil {
		message := "command execution was cancelled"
		if errors.Is(outcome.contextErr, context.DeadlineExceeded) {
			message = "command execution exceeded the caller deadline"
		}
		if !managedCleanupCompleted {
			message += " and managed launcher cleanup did not complete"
		}
		return "", executionUncertainty{
			message: message, cause: outcome.contextErr, preserveContext: true, state: shellInterruptionState(outcome),
		}
	}
	if outcome.timedOut {
		if !managedCleanupCompleted {
			return "", executionUncertainty{
				message: "command timed out and managed launcher cleanup did not complete",
				cause:   outcome.waitErr, state: tool.ExecutionUncertain,
			}
		}
		return "command exceeded the configured timeout", nil
	}
	if outcome.launchErr != nil {
		if outcome.started {
			return "", executionUncertainty{
				message: "command launch was only partially observed",
				cause:   outcome.launchErr, state: tool.ExecutionUncertain,
			}
		}
		return "command could not be started", nil
	}
	if outcome.resultErr != nil {
		return "", executionUncertainty{
			message: "command result could not be observed", cause: outcome.resultErr,
			state: tool.ExecutionUncertain,
		}
	}
	if !managedCleanupCompleted {
		return "", executionUncertainty{
			message: "command process ownership could not be safely released",
			cause:   outcome.waitErr, state: tool.ExecutionUncertain,
		}
	}
	if outcome.controlErr != nil {
		return "", executionUncertainty{
			message: "command process control did not complete cleanly",
			cause:   outcome.controlErr, state: tool.ExecutionUncertain,
		}
	}
	if !outcome.hasResult {
		return "", executionUncertainty{
			message: "command outcome could not be observed", state: tool.ExecutionUncertain,
		}
	}
	if !outcome.result.Successful() {
		return formatProcessProblem(outcome), nil
	}
	return "", nil
}

type executionUncertainty struct {
	message         string
	cause           error
	preserveContext bool
	state           tool.ExecutionState
}

func (uncertainty executionUncertainty) Error() string { return uncertainty.message }

func (uncertainty executionUncertainty) Unwrap() error { return uncertainty.cause }

func shellInterruptionState(outcome processOutcome) tool.ExecutionState {
	if outcome.started {
		return tool.ExecutionUncertain
	}
	return tool.ExecutionDefinitive
}

func validateArgv(argv []string) error {
	if len(argv) == 0 || len(argv) > maximumArguments {
		return invalidArguments("argv must contain between 1 and 128 discrete arguments")
	}
	total := 0
	for _, argument := range argv {
		if argument == "" || strings.IndexByte(argument, 0) >= 0 {
			return invalidArguments("argv entries must be non-empty and must not contain NUL")
		}
		total += len(argument)
		if total > maximumArgvBytes {
			return invalidArguments("argv exceeds the configured metadata bound")
		}
	}
	return nil
}

func selectedEnvironment(allowlist []string) []string {
	result := make([]string, 0, len(allowlist))
	for _, name := range allowlist {
		if value, present := os.LookupEnv(name); present {
			result = append(result, name+"="+value)
		}
	}
	slices.Sort(result)
	return result
}

func outcomeExitCode(outcome processOutcome) int64 {
	if !outcome.hasResult {
		return -1
	}
	code, present := outcome.result.ExitCode()
	if !present {
		return -1
	}
	return code
}

func encodeShellResult(callID tool.CallID, content shellContent, problem string) (tool.Result, error) {
	result, err := newShellResult(callID, content, problem)
	if err == nil {
		return result, nil
	}
	if content.Encoding == "utf-8" {
		content.Encoding = "base64"
		content.Stdout = base64.StdEncoding.EncodeToString([]byte(content.Stdout))
		content.Stderr = base64.StdEncoding.EncodeToString([]byte(content.Stderr))
		if result, err = newShellResult(callID, content, problem); err == nil {
			return result, nil
		}
	}
	return tool.Result{}, err
}

func newShellResult(callID tool.CallID, content shellContent, problem string) (tool.Result, error) {
	encoded, err := json.Marshal(content)
	if err != nil {
		return tool.Result{}, err
	}
	if problem == "" {
		return tool.NewResult(callID, encoded)
	}
	return tool.NewErrorResult(callID, encoded, problem)
}

var _ tool.Tool = (*shellTool)(nil)
