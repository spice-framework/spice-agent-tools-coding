package coding

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"strings"
	"testing"

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
	if err := reportProgress(t.Context(), nil, call.ID(), "working"); err != nil {
		t.Fatalf("reportProgress(nil) error = %v", err)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	reporter := reporterFunc(func(context.Context, tool.Progress) error { return errors.New("stopped") })
	if err := reportProgress(cancelled, reporter, call.ID(), "working"); err == nil ||
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
