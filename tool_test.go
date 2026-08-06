package coding

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spice-framework/spice-agent/stage"
	"github.com/spice-framework/spice-agent/tool"
)

func TestExecutionHelpersRejectMalformedCallsAndPreserveCodes(t *testing.T) {
	t.Parallel()
	if got := (operationError{problem: "specific failure"}).Error(); got != "specific failure" {
		t.Fatalf("operationError.Error() = %q", got)
	}
	if err := decodeArguments(json.RawMessage(`{"path":"value"} {"extra":true}`), &readArguments{}); err == nil ||
		!strings.Contains(err.Error(), "trailing") {
		t.Fatalf("decodeArguments() error = %v", err)
	}
	if err := validateCall(tool.Call{}, "read"); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("validateCall() error = %v", err)
	}
	call := makeCall(t, "read", map[string]any{"path": "value"})
	if err := reportProgress(t.Context(), nil, call.ID(), "working", tool.RetryAllowed); err != nil {
		t.Fatalf("reportProgress(nil) error = %v", err)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	reporter := reporterFunc(func(context.Context, tool.Progress) error { return errors.New("stopped") })
	if err := reportProgress(cancelled, reporter, call.ID(), "working", tool.RetryAllowed); err == nil ||
		!strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("reportProgress(cancelled) error = %v", err)
	}
	//nolint:staticcheck // Deliberately verifies the public nil-context rejection boundary.
	if err := contextFailure(nil); err == nil || !strings.Contains(err.Error(), "must not be nil") {
		t.Fatalf("contextFailure(nil) error = %v", err)
	}
	result := failureResult(call.ID(), errors.New("opaque"))
	content := decodeContent[struct {
		Code string `json:"code"`
	}](t, result)
	if content.Code != "operation_failed" {
		t.Fatalf("failure code = %q", content.Code)
	}
}

func TestDefinitionsDeclareStableEffectReplayAndFingerprintContracts(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	factories := []struct {
		name   string
		create func(Config) (tool.Tool, error)
		effect tool.Effect
		replay tool.ReplaySafety
	}{
		{name: "read", create: NewRead, effect: tool.EffectReadOnly, replay: tool.ReplaySafe},
		{name: "replace", create: NewReplace, effect: tool.EffectMutating, replay: tool.ReplayIdempotent},
		{name: "shell", create: NewShell, effect: tool.EffectMutating, replay: tool.ReplayUnsafe},
	}
	fingerprints := make(map[string]string, len(factories))
	for _, factory := range factories {
		instance, err := factory.create(Config{Root: root})
		if err != nil {
			t.Fatalf("%s factory error = %v", factory.name, err)
		}
		definition := instance.Definition()
		if definition.Name() != factory.name || definition.Effect() != factory.effect ||
			definition.ReplaySafety() != factory.replay {
			t.Fatalf(
				"%s definition = name %q, effect %q, replay %q",
				factory.name, definition.Name(), definition.Effect(), definition.ReplaySafety(),
			)
		}
		fingerprint := definition.Fingerprint()
		if len(fingerprint) != sha256.Size*2 {
			t.Fatalf("%s fingerprint length = %d", factory.name, len(fingerprint))
		}
		if _, err := hex.DecodeString(fingerprint); err != nil {
			t.Fatalf("%s fingerprint is not hexadecimal: %v", factory.name, err)
		}
		if definition.Clone().Fingerprint() != fingerprint || instance.Definition().Fingerprint() != fingerprint {
			t.Fatalf("%s fingerprint is not stable", factory.name)
		}
		if previous, duplicate := fingerprints[fingerprint]; duplicate {
			t.Fatalf("%s and %s share fingerprint %q", previous, factory.name, fingerprint)
		}
		fingerprints[fingerprint] = factory.name
	}
}

func TestInfrastructureFailuresAreDirectBoundedAndCorrelated(t *testing.T) {
	t.Parallel()
	reader, err := NewRead(Config{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	call := makeCall(t, "read", map[string]any{"path": "missing"})
	reporterCause := errors.New(strings.Repeat("untrusted reporter failure", tool.MaximumExecutionErrorBytes))
	reporter := reporterFunc(func(context.Context, tool.Progress) error { return reporterCause })
	failure := requireExecutionErrorValue(
		t, reader, t.Context(), call, reporter, tool.ExecutionDefinitive, tool.RetryAllowed, nil,
	)
	if len(failure.Error()) > tool.MaximumExecutionErrorBytes {
		t.Fatalf("execution failure length = %d", len(failure.Error()))
	}
	if errors.Is(failure, reporterCause) || strings.Contains(failure.Error(), "untrusted reporter failure") {
		t.Fatalf("execution failure exposed untrusted reporter cause: %q", failure)
	}
	reporterCancellation := requireExecutionErrorValue(
		t,
		reader,
		t.Context(),
		call,
		reporterFunc(func(context.Context, tool.Progress) error { return context.Canceled }),
		tool.ExecutionDefinitive,
		tool.RetryAllowed,
		nil,
	)
	if errors.Is(reporterCancellation, context.Canceled) {
		t.Fatalf("active tool context was misclassified from reporter error: %v", reporterCancellation)
	}

	deadline, cancel := context.WithDeadline(t.Context(), time.Unix(1, 0))
	defer cancel()
	requireExecutionError(
		t, reader, deadline, call, nil, tool.ExecutionDefinitive, tool.RetryAllowed, context.DeadlineExceeded,
	)
}

func TestDispatcherPreservesCodingToolReporterExecutionFailure(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	rejection := errors.New("downstream progress receiver rejected the event")
	reporter := reporterFunc(func(context.Context, tool.Progress) error { return rejection })
	tests := []struct {
		name      string
		create    func(Config) (tool.Tool, error)
		arguments map[string]any
		retry     tool.RetryDisposition
	}{
		{
			name: "read", create: NewRead, retry: tool.RetryAllowed,
			arguments: map[string]any{"path": "missing"},
		},
		{
			name: "replace", create: NewReplace, retry: tool.RetryAllowed,
			arguments: map[string]any{"path": "must-not-exist", "content": "value", "create": true},
		},
		{
			name: "shell", create: NewShell, retry: tool.RetryNever,
			arguments: map[string]any{"argv": []string{"must-not-execute"}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			implementation, err := test.create(Config{Root: root})
			if err != nil {
				t.Fatal(err)
			}
			dispatcher, err := stage.NewDispatcher(map[string]tool.Tool{test.name: implementation})
			if err != nil {
				t.Fatal(err)
			}
			call := makeCall(t, test.name, test.arguments)
			result, dispatchErr := dispatcher.Dispatch(t.Context(), call, reporter)
			combined, failure := requireDispatchFailure(
				t, result, dispatchErr, call.ID(), tool.ExecutionDefinitive, test.retry, rejection,
			)
			var throughAs *tool.ExecutionError
			if !errors.As(dispatchErr, &throughAs) || throughAs != failure {
				t.Fatalf("errors.As(%T) did not preserve the authoritative execution error", dispatchErr)
			}
			//nolint:errorlint // DispatchFailure promises exact reporter identity through its accessor.
			if combined.ExecutionFailure() != failure || combined.ReporterFailure() != rejection ||
				dispatchErr.Error() != "tool execution and progress reporting both failed" ||
				strings.Contains(dispatchErr.Error(), "tool progress receiver rejected the operation") ||
				strings.Contains(dispatchErr.Error(), rejection.Error()) ||
				len(dispatchErr.Error()) > tool.MaximumExecutionErrorBytes {
				t.Fatalf("dispatcher error is duplicated or unbounded: %q", dispatchErr)
			}
			if errors.Is(dispatchErr, rejection) {
				t.Fatalf("dispatcher exposed an untrusted reporter cause: %v", dispatchErr)
			}
		})
	}
	if _, err := os.Stat(filepath.Join(root, "must-not-exist")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejecting reporter allowed replace mutation: %v", err)
	}
}

func TestDispatcherDoesNotTreatReporterCancellationAsToolContextCancellation(t *testing.T) {
	t.Parallel()
	reader, err := NewRead(Config{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	dispatcher, err := stage.NewDispatcher(map[string]tool.Tool{"read": reader})
	if err != nil {
		t.Fatal(err)
	}
	call := makeCall(t, "read", map[string]any{"path": "missing"})
	result, dispatchErr := dispatcher.Dispatch(
		t.Context(),
		call,
		reporterFunc(func(context.Context, tool.Progress) error { return context.Canceled }),
	)
	combined, authoritative := requireDispatchFailure(
		t, result, dispatchErr, call.ID(), tool.ExecutionDefinitive, tool.RetryAllowed, context.Canceled,
	)
	if authoritative == nil || !errors.Is(combined.ReporterFailure(), context.Canceled) ||
		errors.Is(dispatchErr, context.Canceled) {
		t.Fatalf("active tool context was misclassified as cancelled: %v", dispatchErr)
	}
	conflicting, err := tool.NewExecutionError(
		"different-call", tool.ExecutionDefinitive, tool.RetryNever, errors.New("conflicting reporter payload"),
	)
	if err != nil {
		t.Fatal(err)
	}
	result, dispatchErr = dispatcher.Dispatch(
		t.Context(), call, reporterFunc(func(context.Context, tool.Progress) error { return conflicting }),
	)
	combined, authoritative = requireDispatchFailure(
		t, result, dispatchErr, call.ID(), tool.ExecutionDefinitive, tool.RetryAllowed, conflicting,
	)
	var throughAs *tool.ExecutionError
	//nolint:errorlint // DispatchFailure promises exact reporter identity through its accessor.
	if !errors.As(dispatchErr, &throughAs) || throughAs != authoritative ||
		combined.ExecutionFailure() != authoritative || combined.ReporterFailure() != conflicting {
		t.Fatalf("reporter execution error displaced authoritative failure: %T %v", dispatchErr, dispatchErr)
	}
}

func TestBoundedCaptureSaturatesObservedByteCount(t *testing.T) {
	t.Parallel()
	if got := saturatingAdd(math.MaxInt64-1, 2); got != math.MaxInt64 {
		t.Fatalf("saturatingAdd() = %d", got)
	}
	stdout, stderr := newCapturePair(3)
	if _, err := stdout.Write([]byte("ab")); err != nil {
		t.Fatal(err)
	}
	if _, err := stderr.Write([]byte("cd")); err != nil {
		t.Fatal(err)
	}
	stdoutContent, stdoutBytes, truncated := stdout.snapshot()
	stderrContent, stderrBytes, stderrTruncated := stderr.snapshot()
	if string(stdoutContent) != "ab" || string(stderrContent) != "c" || stdoutBytes != 2 || stderrBytes != 2 ||
		!truncated || !stderrTruncated {
		t.Fatalf("captures = %q/%d/%v and %q/%d/%v", stdoutContent, stdoutBytes, truncated,
			stderrContent, stderrBytes, stderrTruncated)
	}
}

func TestPathHelpersRejectUnavailableAndNonRegularTargets(t *testing.T) {
	t.Parallel()
	if _, err := openWorktree(t.TempDir() + string(os.PathSeparator) + "missing"); err == nil {
		t.Fatal("openWorktree() accepted a missing root")
	}
	root := t.TempDir()
	workspace, err := openWorktree(root)
	if err != nil {
		t.Fatal(err)
	}
	defer workspace.Close() //nolint:errcheck // Read-only test cleanup.
	for _, requested := range []string{"", " value", ".", "..", "../value"} {
		if _, parseErr := parseRelativePath(requested, false); parseErr == nil {
			t.Fatalf("parseRelativePath(%q) succeeded", requested)
		}
	}
	missing, parseErr := parseRelativePath("missing", false)
	if parseErr != nil {
		t.Fatal(parseErr)
	}
	if _, _, openErr := openRegularFile(workspace, missing); openErr == nil ||
		!strings.Contains(openErr.Error(), "does not exist") {
		t.Fatalf("openRegularFile(missing) error = %v", openErr)
	}
	directory, parseErr := parseRelativePath("directory", false)
	if parseErr != nil {
		t.Fatal(parseErr)
	}
	if mkdirErr := os.Mkdir(root+string(os.PathSeparator)+"directory", 0o750); mkdirErr != nil {
		t.Fatal(mkdirErr)
	}
	if _, _, openErr := openRegularFile(workspace, directory); openErr == nil ||
		!strings.Contains(openErr.Error(), "regular") {
		t.Fatalf("openRegularFile(directory) error = %v", openErr)
	}
	if err := rejectSymlinkComponents(workspace, relativePath{display: ".", native: "."}); err != nil {
		t.Fatalf("rejectSymlinkComponents(root) error = %v", err)
	}
}

type failingCloser struct{}

func (failingCloser) Close() error { return io.ErrClosedPipe }

func TestBestEffortCleanupIgnoresCleanupFailures(t *testing.T) {
	t.Parallel()
	closeBestEffort(failingCloser{})
	workspace, err := openWorktree(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer workspace.Close() //nolint:errcheck // Read-only test cleanup.
	removeBestEffort(workspace, "missing")
}

func makeCall(t *testing.T, name string, arguments any) tool.Call {
	t.Helper()
	content, err := json.Marshal(arguments)
	if err != nil {
		t.Fatal(err)
	}
	call, err := tool.NewCall(tool.CallID(t.Name()), name, content)
	if err != nil {
		t.Fatal(err)
	}
	return call
}

func decodeContent[T any](t *testing.T, result tool.Result) T {
	t.Helper()
	if err := result.Validate(); err != nil {
		t.Fatalf("Result.Validate() error = %v", err)
	}
	var content T
	if err := json.Unmarshal(result.Content(), &content); err != nil {
		t.Fatal(err)
	}
	return content
}

func requireProblem(t *testing.T, result tool.Result, contains string) {
	t.Helper()
	problem, present := result.Problem()
	if !present || !strings.Contains(problem, contains) {
		t.Fatalf("Result.Problem() = %q, %v; want containing %q", problem, present, contains)
	}
}

//nolint:unparam // Keeping the reporter slot mirrors Tool.Execute and keeps test call sites structurally explicit.
func executeResult(
	t *testing.T,
	executable tool.Tool,
	ctx context.Context,
	call tool.Call,
	reporter tool.Reporter,
) tool.Result {
	t.Helper()
	result, err := executable.Execute(ctx, call, reporter)
	if err != nil {
		t.Fatalf("Tool.Execute() error = %T %v", err, err)
	}
	return result
}

func requireExecutionError(
	t *testing.T,
	executable tool.Tool,
	ctx context.Context,
	call tool.Call,
	reporter tool.Reporter,
	wantState tool.ExecutionState,
	wantRetry tool.RetryDisposition,
	wantCause error,
) {
	t.Helper()
	result, err := executable.Execute(ctx, call, reporter)
	requireExecutionFailure(t, result, err, call.ID(), wantState, wantRetry, wantCause)
}

func requireExecutionFailure(
	t *testing.T,
	result tool.Result,
	err error,
	wantCallID tool.CallID,
	wantState tool.ExecutionState,
	wantRetry tool.RetryDisposition,
	wantCause error,
) {
	t.Helper()
	failure := requireExecutionFailureValue(t, result, err, wantCallID, wantState, wantRetry, wantCause)
	if failure == nil {
		t.Fatal("requireExecutionFailureValue() returned nil")
	}
}

func requireExecutionErrorValue(
	t *testing.T,
	executable tool.Tool,
	ctx context.Context,
	call tool.Call,
	reporter tool.Reporter,
	wantState tool.ExecutionState,
	wantRetry tool.RetryDisposition,
	wantCause error,
) *tool.ExecutionError {
	t.Helper()
	result, err := executable.Execute(ctx, call, reporter)
	return requireExecutionFailureValue(t, result, err, call.ID(), wantState, wantRetry, wantCause)
}

func requireDispatchFailure(
	t *testing.T,
	result tool.Result,
	err error,
	wantCallID tool.CallID,
	wantState tool.ExecutionState,
	wantRetry tool.RetryDisposition,
	wantReporter error,
) (*stage.DispatchFailure, *tool.ExecutionError) {
	t.Helper()
	if !result.IsZero() {
		t.Fatalf("Dispatcher.Dispatch() result = %#v; want zero with dual failure", result)
	}
	var combined *stage.DispatchFailure
	if !errors.As(err, &combined) || combined == nil {
		t.Fatalf("Dispatcher.Dispatch() error = %T %v; want *stage.DispatchFailure", err, err)
	}
	//nolint:errorlint // DispatchFailure promises exact reporter identity through its accessor.
	if combined.ReporterFailure() != wantReporter {
		t.Fatalf("DispatchFailure.ReporterFailure() = %T %v; want identity %T %v",
			combined.ReporterFailure(), combined.ReporterFailure(), wantReporter, wantReporter)
	}
	execution := requireExecutionFailureValue(
		t,
		tool.Result{},
		combined.ExecutionFailure(),
		wantCallID,
		wantState,
		wantRetry,
		nil,
	)
	return combined, execution
}

func requireExecutionFailureValue(
	t *testing.T,
	result tool.Result,
	err error,
	wantCallID tool.CallID,
	wantState tool.ExecutionState,
	wantRetry tool.RetryDisposition,
	wantCause error,
) *tool.ExecutionError {
	t.Helper()
	if !result.IsZero() {
		t.Fatalf("Tool.Execute() result = %#v; want zero with infrastructure failure", result)
	}
	//nolint:errorlint // The public contract requires one direct, unwrapped execution error.
	failure, ok := err.(*tool.ExecutionError)
	if !ok || failure.CallID() != wantCallID || failure.State() != wantState ||
		failure.RetryDisposition() != wantRetry {
		t.Fatalf(
			"Tool.Execute() error = %T %#v; want call=%q state=%q retry=%q",
			err, err, wantCallID, wantState, wantRetry,
		)
	}
	if failure.Validate() != nil {
		t.Fatalf("ExecutionError.Validate() error = %v", failure.Validate())
	}
	if wantCause != nil && !errors.Is(failure, wantCause) {
		t.Fatalf("errors.Is(%v) = false for %v", failure, wantCause)
	}
	return failure
}
