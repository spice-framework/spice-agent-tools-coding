package coding

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spice-framework/spice-agent/tool"
)

const shellHelperMarker = "--spice-shell-helper"

func TestShellExecutesDiscreteArgvWorkdirAndEnvironmentPolicy(t *testing.T) {
	root := t.TempDir()
	workdir := filepath.Join(root, "nested")
	if err := os.Mkdir(workdir, 0o750); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SPICE_SAFE", "visible")
	t.Setenv("SPICE_SECRET", "hidden")
	shell, err := NewShell(Config{Root: root, EnvironmentAllowlist: []string{"GOCOVERDIR", "SPICE_SAFE"}})
	if err != nil {
		t.Fatal(err)
	}
	definition := shell.Definition()
	if definition.Name() != "shell" || len(definition.Capabilities()) != 7 ||
		definition.Effect() != tool.EffectMutating || definition.ReplaySafety() != tool.ReplayUnsafe {
		t.Fatalf("Definition() = %#v", definition)
	}
	echo := decodeContent[shellContent](t, executeResult(t, shell, t.Context(), makeCall(t, "shell", map[string]any{
		"argv": helperArgv("echo", "stdout", "stderr"), "workdir": "nested",
	}), nil))
	if !echo.OK || echo.Stdout != "stdout" || echo.Stderr != "stderr" || echo.Workdir != "nested" ||
		!echo.ManagedCleanupCompleted {
		t.Fatalf("echo result = %#v", echo)
	}
	environment := decodeContent[shellContent](t, executeResult(t, shell, t.Context(), makeCall(t, "shell", map[string]any{
		"argv": helperArgv("env", "SPICE_SAFE", "SPICE_SECRET"),
	}), nil))
	if environment.Stdout != "visible|" {
		t.Fatalf("environment result = %#v", environment)
	}
	workingDirectory := decodeContent[shellContent](t, executeResult(t, shell, t.Context(), makeCall(t, "shell", map[string]any{
		"argv": helperArgv("cwd"), "workdir": "nested",
	}), nil))
	if filepath.Clean(workingDirectory.Stdout) != filepath.Clean(workdir) {
		t.Fatalf("cwd = %q, want %q", workingDirectory.Stdout, workdir)
	}
}

func TestShellBoundsOutputAndPreservesNonzeroOutcome(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	shell, err := NewShell(Config{Root: root, MaxOutputBytes: 32, EnvironmentAllowlist: []string{"GOCOVERDIR"}})
	if err != nil {
		t.Fatal(err)
	}
	bounded := decodeContent[shellContent](t, executeResult(t, shell, t.Context(), makeCall(t, "shell", map[string]any{
		"argv": helperArgv("flood", "1000"),
	}), nil))
	if !bounded.OK || !bounded.OutputTruncated || bounded.StdoutBytes != 1000 || len(bounded.Stdout) > 32 {
		t.Fatalf("bounded output = %#v", bounded)
	}
	failed := executeResult(t, shell, t.Context(), makeCall(t, "shell", map[string]any{
		"argv": helperArgv("fail", "diagnostic"),
	}), nil)
	requireProblem(t, failed, "status 7")
	failedContent := decodeContent[shellContent](t, failed)
	if failedContent.ExitCode != 7 || failedContent.Stderr != "diagnostic" || failedContent.OK {
		t.Fatalf("failed command = %#v", failedContent)
	}
}

func TestShellCancellationTerminatesManagedProcessTree(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	pidFile := filepath.Join(root, "child.pid")
	shell, err := NewShell(Config{
		Root: root, CommandTimeout: 20 * time.Second, EnvironmentAllowlist: []string{"GOCOVERDIR"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	type executionOutcome struct {
		result tool.Result
		err    error
	}
	resultChannel := make(chan executionOutcome, 1)
	call := makeCall(t, "shell", map[string]any{"argv": helperArgv("spawn", pidFile)})
	go func() {
		result, executeErr := shell.Execute(ctx, call, nil)
		resultChannel <- executionOutcome{result: result, err: executeErr}
	}()
	pid := waitForChildPID(t, pidFile)
	cancel()
	select {
	case outcome := <-resultChannel:
		requireExecutionFailure(
			t, outcome.result, outcome.err, call.ID(), tool.ExecutionUncertain, tool.RetryNever, context.Canceled,
		)
	case <-time.After(10 * time.Second):
		t.Fatal("cancelled shell call did not return")
	}
	time.Sleep(500 * time.Millisecond)
	process, err := os.FindProcess(pid)
	if err == nil {
		if killErr := process.Kill(); killErr == nil {
			t.Fatal("managed child survived process-group or Job Object cancellation")
		}
	}
}

func TestManagedCleanupDoesNotClaimDetachedDescendants(t *testing.T) {
	t.Parallel()
	pidFile := filepath.Join(t.TempDir(), "detached.pid")
	ctx, cancel := context.WithCancel(t.Context())
	// #nosec G204 -- the test executes its fixed helper with a test-owned PID path.
	command := exec.Command(
		os.Args[0], "-test.run=TestShellHelperProcess", "--",
		shellHelperMarker, "spawn-detached", pidFile,
	)
	outcome := runProcessWithHooks(ctx, command, 20*time.Second, processHooks{
		afterStartBeforeAttach: func() {
			waitForChildPID(t, pidFile)
			cancel()
		},
	})
	if !outcome.started || !errors.Is(outcome.contextErr, context.Canceled) || outcome.stopErr != nil {
		t.Fatalf("runProcessWithHooks() outcome = %#v", outcome)
	}
	pid := waitForChildPID(t, pidFile)
	detached, err := os.FindProcess(pid)
	if err != nil {
		t.Fatalf("find detached process %d: %v", pid, err)
	}
	if err := detached.Kill(); err != nil {
		t.Fatalf("detached process did not demonstrate the documented cleanup boundary: %v", err)
	}
}

func TestShellTimeoutAndArgumentFailures(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	shell, err := NewShell(Config{
		Root: root, CommandTimeout: 50 * time.Millisecond, EnvironmentAllowlist: []string{"GOCOVERDIR"},
	})
	if err != nil {
		t.Fatal(err)
	}
	timedOut := executeResult(t, shell, t.Context(), makeCall(t, "shell", map[string]any{
		"argv": helperArgv("sleep"),
	}), nil)
	requireProblem(t, timedOut, "timeout")
	timedOutContent := decodeContent[shellContent](t, timedOut)
	if !timedOutContent.TimedOut || !timedOutContent.ManagedCleanupCompleted {
		t.Fatalf("timeout result = %#v", timedOutContent)
	}
	tests := []struct {
		name      string
		arguments map[string]any
		problem   string
	}{
		{name: "empty argv", arguments: map[string]any{"argv": []string{}}, problem: "argv"},
		{name: "nul", arguments: map[string]any{"argv": []string{"bad\x00value"}}, problem: "NUL"},
		{name: "escape", arguments: map[string]any{"argv": helperArgv("echo", "out", "err"), "workdir": ".."}, problem: "relative"},
		{name: "unknown env", arguments: map[string]any{"argv": helperArgv("echo", "out", "err"), "env": map[string]string{"SECRET": "value"}}, problem: "schema"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			requireProblem(t, executeResult(t, shell, t.Context(), makeCall(t, "shell", test.arguments), nil), test.problem)
		})
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Logf("symlink test skipped: %v", err)
		return
	}
	requireProblem(t, executeResult(t, shell, t.Context(), makeCall(t, "shell", map[string]any{
		"argv": helperArgv("echo", "out", "err"), "workdir": "linked",
	}), nil), "symbolic-link")
}

func TestShellCallerDeadlineIsUncertainExecutionFailure(t *testing.T) {
	t.Parallel()
	shell, err := NewShell(Config{
		Root: t.TempDir(), CommandTimeout: 20 * time.Second, EnvironmentAllowlist: []string{"GOCOVERDIR"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	call := makeCall(t, "shell", map[string]any{"argv": helperArgv("sleep")})
	requireExecutionError(
		t, shell, ctx, call, nil, tool.ExecutionUncertain, tool.RetryNever, context.DeadlineExceeded,
	)
}

func TestRunProcessRechecksCancellationImmediatelyBeforeStart(t *testing.T) {
	t.Parallel()
	marker := filepath.Join(t.TempDir(), "started")
	ctx, cancel := context.WithCancel(t.Context())
	// #nosec G204 -- the test executes its fixed helper with a test-owned marker path.
	command := exec.Command(os.Args[0], "-test.run=TestShellHelperProcess", "--", shellHelperMarker, "mark", marker)
	outcome := runProcessWithHooks(ctx, command, time.Minute, processHooks{beforeStart: cancel})
	if outcome.started || !errors.Is(outcome.contextErr, context.Canceled) {
		t.Fatalf("runProcessWithHooks() outcome = %#v", outcome)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled process created marker: %v", err)
	}
}

func TestShellPreStartCancellationIsDefinitive(t *testing.T) {
	t.Parallel()
	constructed, err := NewShell(Config{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	shell, ok := constructed.(*shellTool)
	if !ok {
		t.Fatalf("NewShell() type = %T", constructed)
	}
	shell.run = func(context.Context, *exec.Cmd, time.Duration) processOutcome {
		return processOutcome{contextErr: context.Canceled}
	}
	call := makeCall(t, "shell", map[string]any{
		"argv": helperArgv("echo", "unused", "unused"),
	})
	requireExecutionError(
		t, shell, t.Context(), call, nil, tool.ExecutionDefinitive, tool.RetryNever, context.Canceled,
	)
}

func TestShellUnconfirmedStartedProcessOutcomesAreInfrastructureFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		outcome processOutcome
	}{
		{
			name: "configured timeout",
			outcome: processOutcome{
				started: true, timedOut: true, stopErr: errors.New("termination unavailable"),
			},
		},
		{
			name: "attach failure after start",
			outcome: processOutcome{
				started: true, waitErr: errors.New("wait failed"), stopErr: errors.New("attach and cleanup failed"),
			},
		},
		{
			name:    "cleanup failure after completion",
			outcome: processOutcome{started: true, stopErr: errors.New("cleanup failed")},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			constructed, err := NewShell(Config{Root: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			shell, ok := constructed.(*shellTool)
			if !ok {
				t.Fatalf("NewShell() type = %T", constructed)
			}
			shell.run = func(context.Context, *exec.Cmd, time.Duration) processOutcome {
				return test.outcome
			}
			call := makeCall(t, "shell", map[string]any{
				"argv": helperArgv("echo", "unused", "unused"),
			})
			failure := requireExecutionErrorValue(
				t, shell, t.Context(), call, nil, tool.ExecutionUncertain, tool.RetryNever, nil,
			)
			if strings.Contains(failure.Error(), test.outcome.stopErr.Error()) {
				t.Fatalf("execution failure exposed raw process detail: %q", failure)
			}
		})
	}
}

func TestShellRejectsInvalidConfigurationReporterAndExecutionTargets(t *testing.T) {
	t.Parallel()
	if _, err := NewShell(Config{Root: "relative"}); err == nil {
		t.Fatal("NewShell() accepted a relative root")
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("value"), 0o600); err != nil {
		t.Fatal(err)
	}
	shell, err := NewShell(Config{Root: root, EnvironmentAllowlist: []string{"GOCOVERDIR"}})
	if err != nil {
		t.Fatal(err)
	}
	reporter := reporterFunc(func(context.Context, tool.Progress) error { return errors.New("reject") })
	requireExecutionError(t, shell, t.Context(), makeCall(t, "shell", map[string]any{
		"argv": helperArgv("echo", "out", "err"),
	}), reporter, tool.ExecutionDefinitive, tool.RetryNever, nil)
	requireProblem(t, executeResult(t, shell, t.Context(), makeCall(t, "shell", map[string]any{
		"argv": []string{filepath.Join(root, "definitely-missing-executable")},
	}), nil), "started")
	requireProblem(t, executeResult(t, shell, t.Context(), makeCall(t, "shell", map[string]any{
		"argv": helperArgv("echo", "out", "err"), "workdir": "file",
	}), nil), "unavailable")
	missingRoot, err := NewShell(Config{Root: filepath.Join(root, "missing-root")})
	if err != nil {
		t.Fatal(err)
	}
	requireProblem(t, executeResult(t, missingRoot, t.Context(), makeCall(t, "shell", map[string]any{
		"argv": helperArgv("echo", "out", "err"),
	}), nil), "unavailable")
}

func TestShellResultEncodingAndUncertainProcessOutcomes(t *testing.T) {
	t.Parallel()
	stdout, stderr := newCapturePair(8)
	if _, err := stdout.Write([]byte{0xff, 0x00}); err != nil {
		t.Fatal(err)
	}
	if _, err := stderr.Write([]byte("error")); err != nil {
		t.Fatal(err)
	}
	captured := capturedShellContent(relativePath{display: ".", native: "."}, stdout, stderr, processOutcome{})
	if captured.Encoding != "base64" || captured.ExitCode != 0 || !captured.ManagedCleanupCompleted {
		t.Fatalf("capturedShellContent() = %#v", captured)
	}

	callID := tool.CallID(t.Name())
	controls := strings.Repeat("\x00", 180<<10)
	result, resultErr := encodeShellResult(callID, shellContent{OK: true, Encoding: "utf-8", Stdout: controls}, "")
	if resultErr != nil {
		t.Fatal(resultErr)
	}
	encoded := decodeContent[shellContent](t, result)
	decoded, err := base64.StdEncoding.DecodeString(encoded.Stdout)
	if err != nil || encoded.Encoding != "base64" || string(decoded) != controls {
		t.Fatalf("encoded shell result = encoding %q, bytes %d, error %v", encoded.Encoding, len(decoded), err)
	}
	tooLarge, tooLargeErr := encodeShellResult(callID, shellContent{
		OK: true, Encoding: "base64", Stdout: strings.Repeat("x", tool.MaximumPayloadBytes),
	}, "")
	if tooLargeErr == nil || !tooLarge.IsZero() {
		t.Fatalf("encodeShellResult(too large) = %#v, %v", tooLarge, tooLargeErr)
	}

	stopFailure := errors.New("termination unknown")
	for _, test := range []struct {
		name    string
		outcome processOutcome
		want    string
	}{
		{
			name: "timeout", outcome: processOutcome{started: true, timedOut: true, stopErr: stopFailure},
			want: "timed out",
		},
		{name: "cleanup", outcome: processOutcome{started: true, stopErr: stopFailure}, want: "cleanup"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			problem, classifyErr := classifyProcessOutcome(test.outcome)
			if classifyErr == nil || problem != "" || !strings.Contains(classifyErr.Error(), test.want) {
				t.Fatalf("classifyProcessOutcome() = %q, %v", problem, classifyErr)
			}
		})
	}
	if _, err := classifyProcessOutcome(processOutcome{
		started: true, contextErr: context.Canceled, stopErr: stopFailure,
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("classifyProcessOutcome(cancelled) error = %v", err)
	}
	if _, err := classifyProcessOutcome(processOutcome{waitErr: errors.New("start failure")}); err == nil ||
		!strings.Contains(err.Error(), "started") {
		t.Fatalf("classifyProcessOutcome(start) error = %v", err)
	}
	if got := exitCode(errors.New("unknown")); got != -1 {
		t.Fatalf("exitCode(unknown) = %d", got)
	}
}

func TestProcessWaitHelpersCompleteWithoutAStartedProcess(t *testing.T) {
	t.Parallel()
	command := exec.Command("not-started")
	if err := boundedWait(command); err == nil {
		t.Fatal("boundedWait() unexpectedly succeeded")
	}
	waited := make(chan error, 1)
	waited <- errors.New("already complete")
	waitErr, stopErr := stopAndWait(&processTree{}, waited)
	if waitErr == nil || stopErr != nil {
		t.Fatalf("stopAndWait() = %v, %v", waitErr, stopErr)
	}
	if err := (&processTree{}).forceStop(); err != nil {
		t.Fatalf("forceStop(empty) error = %v", err)
	}
}

func TestShellHelperProcess(t *testing.T) {
	index := -1
	for candidate, argument := range os.Args {
		if argument == shellHelperMarker {
			index = candidate
			break
		}
	}
	if index < 0 {
		return
	}
	arguments := os.Args[index+1:]
	if len(arguments) == 0 {
		os.Exit(90)
	}
	switch arguments[0] {
	case "echo":
		writeHelperOutput(os.Stdout, []byte(arguments[1]))
		writeHelperOutput(os.Stderr, []byte(arguments[2]))
	case "env":
		writeHelperOutput(os.Stdout, []byte(os.Getenv(arguments[1])+"|"+os.Getenv(arguments[2])))
	case "cwd":
		workingDirectory, err := os.Getwd()
		if err != nil {
			os.Exit(91)
		}
		writeHelperOutput(os.Stdout, []byte(workingDirectory))
	case "flood":
		count, err := strconv.Atoi(arguments[1])
		if err != nil {
			os.Exit(92)
		}
		writeHelperOutput(os.Stdout, bytes.Repeat([]byte("x"), count))
	case "binary":
		writeHelperOutput(os.Stdout, []byte{0xff, 0x00})
	case "fail":
		writeHelperOutput(os.Stderr, []byte(arguments[1]))
		os.Exit(7)
	case "mark":
		if err := os.WriteFile(arguments[1], []byte("started"), 0o600); err != nil {
			os.Exit(98)
		}
	case "sleep":
		time.Sleep(30 * time.Second)
	case "spawn":
		spawnHelperChild(arguments[1], false)
	case "spawn-detached":
		spawnHelperChild(arguments[1], true)
	default:
		os.Exit(93)
	}
	os.Exit(0)
}

func spawnHelperChild(pidFile string, detached bool) {
	// #nosec G204 -- test helper uses the current fixed test executable and discrete arguments.
	child := exec.Command(os.Args[0], "-test.run=TestShellHelperProcess", "--", shellHelperMarker, "sleep")
	if detached {
		configureDetachedChild(child)
	}
	if err := child.Start(); err != nil {
		os.Exit(94)
	}
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
		if killErr := child.Process.Kill(); killErr != nil {
			os.Exit(96)
		}
		os.Exit(95)
	}
	time.Sleep(30 * time.Second)
}

func writeHelperOutput(destination *os.File, content []byte) {
	if _, err := destination.Write(content); err != nil {
		os.Exit(97)
	}
}

func helperArgv(mode string, arguments ...string) []string {
	result := []string{os.Args[0], "-test.run=TestShellHelperProcess", "--", shellHelperMarker, mode}
	return append(result, arguments...)
}

func waitForChildPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		content, err := os.ReadFile(path) // #nosec G304 -- test-owned path.
		if err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(content)))
			if parseErr == nil {
				return pid
			}
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("child PID was not published")
	return 0
}
