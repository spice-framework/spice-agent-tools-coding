package coding

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/spice-framework/spice-agent/tool"
)

const (
	shellInputSchema = `{"type":"object","additionalProperties":false,"required":["argv"],"properties":{"argv":{"type":"array","minItems":1,"maxItems":128,"items":{"type":"string"}},"workdir":{"type":"string"}}}`
	maximumArguments = 128
	maximumArgvBytes = 64 << 10
)

type shellTool struct {
	config     Config
	definition tool.Definition
}

type shellArguments struct {
	Argv    []string `json:"argv"`
	Workdir string   `json:"workdir,omitempty"`
}

type shellContent struct {
	OK                   bool   `json:"ok"`
	Workdir              string `json:"workdir"`
	ExitCode             int    `json:"exit_code"`
	Stdout               string `json:"stdout"`
	Stderr               string `json:"stderr"`
	Encoding             string `json:"encoding"`
	StdoutBytes          int64  `json:"stdout_bytes"`
	StderrBytes          int64  `json:"stderr_bytes"`
	OutputTruncated      bool   `json:"output_truncated"`
	Cancelled            bool   `json:"cancelled"`
	TimedOut             bool   `json:"timed_out"`
	TerminationConfirmed bool   `json:"termination_confirmed"`
}

// NewShell constructs the exact Spice Agent discrete-argv shell tool binding
// without starting a process.
func NewShell(config Config) (tool.Tool, error) {
	normalized, err := validatedConfig(config)
	if err != nil {
		return nil, err
	}
	definition, err := tool.NewDefinition(
		"shell",
		"Execute unsandboxed discrete argv from a worktree-selected directory with bounded output.",
		json.RawMessage(shellInputSchema),
		tool.CapabilityFilesystemRead,
		tool.CapabilityFilesystemWrite,
		tool.CapabilityProcessExecute,
		tool.CapabilityNetworkAccess,
		tool.CapabilitySecretsRead,
		tool.CapabilityEnvironmentRead,
		tool.CapabilityEnvironmentWrite,
	)
	if err != nil {
		return nil, err
	}
	return &shellTool{config: normalized, definition: definition}, nil
}

func (shell *shellTool) Definition() tool.Definition { return shell.definition.Clone() }

func (shell *shellTool) Execute(ctx context.Context, call tool.Call, reporter tool.Reporter) tool.Result {
	if err := validateCall(call, shell.definition.Name()); err != nil {
		return failureResult(call.ID(), err)
	}
	if err := contextFailure(ctx); err != nil {
		return failureResult(call.ID(), err)
	}
	var arguments shellArguments
	if err := decodeArguments(call.Arguments(), &arguments); err != nil {
		return failureResult(call.ID(), err)
	}
	if err := validateArgv(arguments.Argv); err != nil {
		return failureResult(call.ID(), err)
	}
	workdir := arguments.Workdir
	if workdir == "" {
		workdir = "."
	}
	path, err := parseRelativePath(workdir, true)
	if err != nil {
		return failureResult(call.ID(), err)
	}
	if progressErr := reportProgress(ctx, reporter, call.ID(), "starting bounded child process"); progressErr != nil {
		return failureResult(call.ID(), progressErr)
	}
	content, problem, err := shell.execute(ctx, arguments.Argv, path)
	if err != nil {
		return failureResult(call.ID(), err)
	}
	return encodeShellResult(call.ID(), content, problem)
}

func (shell *shellTool) execute(ctx context.Context, argv []string, workdir relativePath) (shellContent, string, error) {
	workspace, directory, command, stdout, stderr, err := shell.prepareCommand(ctx, argv, workdir)
	if err != nil {
		return shellContent{}, "", err
	}
	defer closeBestEffort(workspace)
	defer closeBestEffort(directory)
	outcome := runProcess(ctx, command, shell.config.CommandTimeout)
	content := capturedShellContent(workdir, stdout, stderr, outcome)
	problem, err := classifyProcessOutcome(outcome)
	if err != nil {
		return shellContent{}, "", err
	}
	return content, problem, nil
}

func (shell *shellTool) prepareCommand(
	ctx context.Context,
	argv []string,
	workdir relativePath,
) (*os.Root, *os.Root, *exec.Cmd, *boundedCapture, *boundedCapture, error) {
	workspace, err := openWorktree(shell.config.Root)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	if pathErr := rejectSymlinkComponents(workspace, workdir); pathErr != nil {
		closeBestEffort(workspace)
		return nil, nil, nil, nil, nil, pathErr
	}
	directory, err := workspace.OpenRoot(workdir.native)
	if err != nil {
		closeBestEffort(workspace)
		return nil, nil, nil, nil, nil,
			executionFailure("workdir_unavailable", "command workdir is unavailable or escapes the configured worktree")
	}
	info, err := directory.Stat(".")
	if err != nil || !info.IsDir() {
		closeBestEffort(directory)
		closeBestEffort(workspace)
		return nil, nil, nil, nil, nil, executionFailure("workdir_unavailable", "command workdir is not a directory")
	}
	stdout, stderr := newCapturePair(shell.config.MaxOutputBytes)
	// #nosec G204 -- argv is deliberately executed without a shell under an explicit process capability.
	command := exec.CommandContext(context.WithoutCancel(ctx), argv[0], argv[1:]...)
	command.Dir = directory.Name()
	command.Env = selectedEnvironment(shell.config.EnvironmentAllowlist)
	command.Stdout = stdout
	command.Stderr = stderr
	if pathErr := rejectSymlinkComponents(workspace, workdir); pathErr != nil {
		closeBestEffort(directory)
		closeBestEffort(workspace)
		return nil, nil, nil, nil, nil, pathErr
	}
	return workspace, directory, command, stdout, stderr, nil
}

func capturedShellContent(
	workdir relativePath,
	stdout, stderr *boundedCapture,
	outcome processOutcome,
) shellContent {
	stdoutContent, stdoutBytes, truncated := stdout.snapshot()
	stderrContent, stderrBytes, stderrTruncated := stderr.snapshot()
	content := shellContent{
		OK:      outcome.waitErr == nil && !outcome.cancelled && !outcome.timedOut && outcome.stopErr == nil,
		Workdir: workdir.display, ExitCode: exitCode(outcome.waitErr),
		Stdout: string(stdoutContent), Stderr: string(stderrContent), Encoding: "utf-8",
		StdoutBytes: stdoutBytes, StderrBytes: stderrBytes,
		OutputTruncated: truncated || stderrTruncated, Cancelled: outcome.cancelled, TimedOut: outcome.timedOut,
		TerminationConfirmed: outcome.stopErr == nil,
	}
	if !utf8.Valid(stdoutContent) || !utf8.Valid(stderrContent) {
		content.Encoding = "base64"
		content.Stdout = base64.StdEncoding.EncodeToString(stdoutContent)
		content.Stderr = base64.StdEncoding.EncodeToString(stderrContent)
	}
	return content
}

func classifyProcessOutcome(outcome processOutcome) (string, error) {
	terminationConfirmed := outcome.stopErr == nil
	if outcome.timedOut {
		if !terminationConfirmed {
			return "command timed out and process-tree termination could not be confirmed", nil
		}
		return "command exceeded the configured timeout", nil
	}
	if outcome.cancelled {
		if !terminationConfirmed {
			return "command was cancelled and process-tree termination could not be confirmed", nil
		}
		return "command was cancelled", nil
	}
	if !terminationConfirmed {
		return "command completed but process-tree cleanup could not be confirmed", nil
	}
	if outcome.waitErr != nil {
		if exitError, ok := errors.AsType[*exec.ExitError](outcome.waitErr); ok {
			return fmt.Sprintf("command exited with status %d", exitError.ExitCode()), nil
		}
		return "", executionFailure("start_failed", "command could not be started or observed")
	}
	return "", nil
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

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	if exitError, ok := errors.AsType[*exec.ExitError](err); ok {
		return exitError.ExitCode()
	}
	return -1
}

func encodeShellResult(callID tool.CallID, content shellContent, problem string) tool.Result {
	result, err := newShellResult(callID, content, problem)
	if err == nil {
		return result
	}
	if content.Encoding == "utf-8" {
		content.Encoding = "base64"
		content.Stdout = base64.StdEncoding.EncodeToString([]byte(content.Stdout))
		content.Stderr = base64.StdEncoding.EncodeToString([]byte(content.Stderr))
		if result, err = newShellResult(callID, content, problem); err == nil {
			return result
		}
	}
	return failureResult(callID, executionFailure("result_too_large", "command result exceeds the supported payload limit"))
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
