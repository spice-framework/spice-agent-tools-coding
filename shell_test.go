package coding

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spice-framework/spice-agent/process"
	"github.com/spice-framework/spice-agent/tool"
	"github.com/spice-framework/spice/lifecycle"
)

func TestShellResolvesNaturalExecutableAndLaunchesExactSpec(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "nested")
	if err := os.Mkdir(nested, 0o750); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SPICE_VISIBLE", "yes")
	t.Setenv("SPICE_HIDDEN", "no")
	resolved := filepath.Join(root, "bin", "tool")
	resolver := &fakeResolver{resolved: resolved}
	fixture := completedProcess(t, 0)
	launcher := &fakeLauncher{start: func(_ context.Context, spec process.Spec) (process.Process, error) {
		if _, err := io.WriteString(spec.Stdout(), "stdout"); err != nil {
			return nil, err
		}
		if _, err := io.WriteString(spec.Stderr(), "stderr"); err != nil {
			return nil, err
		}
		return fixture, nil
	}}
	executable, cleanup := newTestShell(t, Config{
		Root: root, EnvironmentAllowlist: []string{"SPICE_VISIBLE"},
	}, resolver, launcher)
	result := executeResult(t, executable, t.Context(), makeCall(t, "shell", map[string]any{
		"argv": []string{"tool", "first", "second"}, "workdir": "nested",
	}), nil)
	content := decodeContent[shellContent](t, result)
	if !content.OK || content.Stdout != "stdout" || content.Stderr != "stderr" ||
		content.Workdir != "nested" || content.ExitCode != 0 || !content.ManagedCleanupCompleted {
		t.Fatalf("shell result = %#v", content)
	}
	lookup := resolver.singleLookup(t)
	if lookup.RequestedExecutable() != "tool" || lookup.WorkingDirectory() != filepath.Clean(nested) ||
		!slices.Equal(lookup.Environment(), []string{"SPICE_VISIBLE=yes"}) {
		t.Fatalf("lookup = requested %q, cwd %q, env %#v", lookup.RequestedExecutable(), lookup.WorkingDirectory(), lookup.Environment())
	}
	spec := launcher.singleSpec(t)
	if spec.Executable() != filepath.Clean(resolved) || spec.WorkingDirectory() != filepath.Clean(nested) ||
		!slices.Equal(spec.Arguments(), []string{"first", "second"}) ||
		!slices.Equal(spec.Environment(), []string{"SPICE_VISIBLE=yes"}) ||
		spec.Stdin() == nil || !slices.Contains(spec.Capabilities(), tool.CapabilityProcessExecute) {
		t.Fatalf("process specification was not exact: %#v", spec)
	}
	if fixture.waitCalls() != 1 {
		t.Fatalf("Wait calls = %d", fixture.waitCalls())
	}
	if err := cleanup(t.Context()); err != nil {
		t.Fatalf("cleanup after joined process: %v", err)
	}
}

func TestShellClassifiesPortableOutcomes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		outcome     process.Outcome
		wantCode    int64
		wantProblem string
	}{
		{name: "nonzero", outcome: exitedOutcome(t, 23), wantCode: 23, wantProblem: "status 23"},
		{name: "signaled", outcome: process.NewSignaledOutcome(), wantCode: -1, wantProblem: "terminated"},
		{name: "unknown", outcome: process.NewUnknownOutcome(), wantCode: -1, wantProblem: "portable"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fixture := fakeCompletedProcess(test.outcome)
			executable, _ := newTestShell(t, Config{Root: t.TempDir()},
				&fakeResolver{resolved: filepath.Join(t.TempDir(), "tool")},
				&fakeLauncher{process: fixture})
			result := executeResult(t, executable, t.Context(), makeCall(t, "shell", map[string]any{"argv": []string{"tool"}}), nil)
			requireProblem(t, result, test.wantProblem)
			content := decodeContent[shellContent](t, result)
			if content.OK || content.ExitCode != test.wantCode || !content.ManagedCleanupCompleted {
				t.Fatalf("content = %#v", content)
			}
		})
	}
}

func TestShellBoundsAndEncodesCapturedOutput(t *testing.T) {
	t.Parallel()
	launcher := &fakeLauncher{start: func(_ context.Context, spec process.Spec) (process.Process, error) {
		if _, err := spec.Stdout().Write(append([]byte{0xff, 0}, []byte(strings.Repeat("x", 30))...)); err != nil {
			return nil, err
		}
		if _, err := io.WriteString(spec.Stderr(), "stderr-overflow"); err != nil {
			return nil, err
		}
		return completedProcess(t, 0), nil
	}}
	executable, _ := newTestShell(t, Config{Root: t.TempDir(), MaxOutputBytes: 8},
		&fakeResolver{resolved: filepath.Join(t.TempDir(), "tool")}, launcher)
	content := decodeContent[shellContent](t, executeResult(t, executable, t.Context(),
		makeCall(t, "shell", map[string]any{"argv": []string{"tool"}}), nil))
	if !content.OK || content.Encoding != "base64" || !content.OutputTruncated ||
		content.StdoutBytes != 32 || content.StderrBytes != int64(len("stderr-overflow")) {
		t.Fatalf("bounded content = %#v", content)
	}
	if _, err := base64.StdEncoding.DecodeString(content.Stdout); err != nil {
		t.Fatalf("stdout is not base64: %v", err)
	}
}

func TestShellCancellationStopsForcesAndJoins(t *testing.T) {
	t.Parallel()
	fixture := newFakeProcess(exitedOutcome(t, 137), false)
	fixture.stopLeavesRunning = true
	fixture.forceCompletes = true
	started := make(chan struct{})
	launcher := &fakeLauncher{process: fixture, started: started}
	executable, cleanup := newTestShell(t, Config{Root: t.TempDir(), CommandTimeout: time.Minute},
		&fakeResolver{resolved: filepath.Join(t.TempDir(), "tool")}, launcher)
	ctx, cancel := context.WithCancel(t.Context())
	call := makeCall(t, "shell", map[string]any{"argv": []string{"tool"}})
	result := make(chan error, 1)
	go func() {
		_, err := executable.Execute(ctx, call, nil)
		result <- err
	}()
	<-started
	cancel()
	select {
	case err := <-result:
		requireExecutionFailure(t, tool.Result{}, err, call.ID(), tool.ExecutionUncertain, tool.RetryNever, context.Canceled)
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled execution did not return")
	}
	stop, force, waits := fixture.calls()
	if stop != 1 || force != 1 || waits != 1 {
		t.Fatalf("control calls = stop %d, force %d, wait %d", stop, force, waits)
	}
	if err := cleanup(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestShellTimeoutWithIncompleteJoinIsValidUncertainFailure(t *testing.T) {
	t.Parallel()
	fixture := newFakeProcess(exitedOutcome(t, 1), false)
	fixture.waitErrors = []error{process.NewFailure(process.OperationWait, errors.New("retry join")), nil}
	executable, cleanup := newTestShell(t, Config{Root: t.TempDir(), CommandTimeout: time.Millisecond},
		&fakeResolver{resolved: filepath.Join(t.TempDir(), "tool")}, &fakeLauncher{process: fixture})
	call := makeCall(t, "shell", map[string]any{"argv": []string{"tool"}})
	requireExecutionError(t, executable, t.Context(), call, nil, tool.ExecutionUncertain, tool.RetryNever, nil)
	if err := cleanup(t.Context()); err != nil {
		t.Fatalf("cleanup after timeout join retry: %v", err)
	}
}

func TestShellRetainsProcessFromPartialStartFailure(t *testing.T) {
	t.Parallel()
	retryable := process.NewFailure(process.OperationWait, errors.New("private transient join"))
	fixture := completedProcess(t, 1)
	fixture.waitErrors = []error{retryable, nil}
	launcher := &fakeLauncher{start: func(context.Context, process.Spec) (process.Process, error) {
		return fixture, errors.New("private partial launch")
	}}
	executable, cleanup := newTestShell(t, Config{Root: t.TempDir()},
		&fakeResolver{resolved: filepath.Join(t.TempDir(), "tool")}, launcher)
	shell := requireShellTool(t, executable)
	call := makeCall(t, "shell", map[string]any{"argv": []string{"tool"}})
	requireExecutionError(t, executable, t.Context(), call, nil, tool.ExecutionUncertain, tool.RetryNever, nil)
	if shell.ownedCount() != 1 || fixture.waitCalls() != 1 {
		t.Fatalf("partial launch ownership = %d, waits = %d", shell.ownedCount(), fixture.waitCalls())
	}
	if err := cleanup(t.Context()); err != nil {
		t.Fatalf("cleanup retry: %v", err)
	}
	if shell.ownedCount() != 0 || fixture.waitCalls() != 2 {
		t.Fatalf("cleanup did not release partial launch: owned %d, waits %d", shell.ownedCount(), fixture.waitCalls())
	}
}

func TestShellCleanupRetriesRetryableWaitAndIsIdempotent(t *testing.T) {
	t.Parallel()
	fixture := completedProcess(t, 0)
	fixture.waitErrors = []error{
		process.NewFailure(process.OperationWait, errors.New("retry one")),
		process.NewFailure(process.OperationWait, errors.New("retry two")),
		nil,
	}
	executable, cleanup := newTestShell(t, Config{Root: t.TempDir()},
		&fakeResolver{resolved: filepath.Join(t.TempDir(), "tool")}, &fakeLauncher{process: fixture})
	call := makeCall(t, "shell", map[string]any{"argv": []string{"tool"}})
	requireExecutionError(t, executable, t.Context(), call, nil, tool.ExecutionUncertain, tool.RetryNever, nil)
	if err := cleanup(t.Context()); err != nil {
		t.Fatalf("cleanup retry: %v", err)
	}
	if err := cleanup(t.Context()); err != nil {
		t.Fatalf("repeated cleanup: %v", err)
	}
	if fixture.waitCalls() != 3 {
		t.Fatalf("Wait calls = %d", fixture.waitCalls())
	}
	result := executeResult(t, executable, t.Context(), call, nil)
	requireProblem(t, result, "cleanup")
}

func TestShellCleanupCachesTerminalWaitFailure(t *testing.T) {
	t.Parallel()
	terminal := process.NewFailure(process.OperationWait, retryClassifiedError{retryable: false})
	fixture := completedProcess(t, 0)
	fixture.waitErrors = []error{terminal}
	executable, cleanup := newTestShell(t, Config{Root: t.TempDir()},
		&fakeResolver{resolved: filepath.Join(t.TempDir(), "tool")}, &fakeLauncher{process: fixture})
	call := makeCall(t, "shell", map[string]any{"argv": []string{"tool"}})
	requireExecutionError(t, executable, t.Context(), call, nil, tool.ExecutionUncertain, tool.RetryNever, nil)
	for range 2 {
		if err := cleanup(t.Context()); err == nil {
			t.Fatal("terminal cleanup unexpectedly succeeded")
		}
	}
	if fixture.waitCalls() != 1 {
		t.Fatalf("terminal Wait was replayed %d times", fixture.waitCalls())
	}
}

func TestShellExecuteCleanupOverlapClosesAdmission(t *testing.T) {
	t.Parallel()
	fixture := newFakeProcess(exitedOutcome(t, 0), false)
	started := make(chan struct{})
	launcher := &fakeLauncher{process: fixture, started: started}
	executable, cleanup := newTestShell(t, Config{Root: t.TempDir(), CommandTimeout: time.Minute},
		&fakeResolver{resolved: filepath.Join(t.TempDir(), "tool")}, launcher)
	call := makeCall(t, "shell", map[string]any{"argv": []string{"tool"}})
	executed := make(chan error, 1)
	go func() {
		_, err := executable.Execute(t.Context(), call, nil)
		executed <- err
	}()
	<-started
	cleaned := make(chan error, 1)
	go func() { cleaned <- cleanup(t.Context()) }()
	deadline := time.Now().Add(time.Second)
	shell := requireShellTool(t, executable)
	for !shell.isClosing() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	second := executeResult(t, executable, t.Context(), call, nil)
	requireProblem(t, second, "cleanup")
	fixture.complete()
	if err := <-executed; err != nil {
		t.Fatalf("first execute: %v", err)
	}
	if err := <-cleaned; err != nil {
		t.Fatalf("overlapping cleanup: %v", err)
	}
}

func TestShellCleanupCancellationRetainsOwnership(t *testing.T) {
	t.Parallel()
	fixture := completedProcess(t, 0)
	fixture.waitErrors = []error{process.NewFailure(process.OperationWait, errors.New("retry join")), nil}
	executable, cleanup := newTestShell(t, Config{Root: t.TempDir()},
		&fakeResolver{resolved: filepath.Join(t.TempDir(), "tool")}, &fakeLauncher{process: fixture})
	call := makeCall(t, "shell", map[string]any{"argv": []string{"tool"}})
	requireExecutionError(t, executable, t.Context(), call, nil, tool.ExecutionUncertain, tool.RetryNever, nil)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := cleanup(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled cleanup = %v", err)
	}
	if err := cleanup(t.Context()); err != nil {
		t.Fatalf("later cleanup: %v", err)
	}
	if fixture.waitCalls() != 2 {
		t.Fatalf("cancelled cleanup changed ownership, Wait calls = %d", fixture.waitCalls())
	}
}

func TestShellRejectsResolverLauncherAndInputFailures(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	resolver := &fakeResolver{resolved: filepath.Join(root, "tool")}
	launcher := &fakeLauncher{process: completedProcess(t, 0)}
	if _, _, err := NewShell(Config{Root: "relative"}, resolver, launcher); err == nil {
		t.Fatal("relative root succeeded")
	}
	if _, _, err := NewShell(Config{Root: root}, nil, launcher); err == nil {
		t.Fatal("nil resolver succeeded")
	}
	if _, _, err := NewShell(Config{Root: root}, resolver, nil); err == nil {
		t.Fatal("nil launcher succeeded")
	}
	executable, _ := newTestShell(t, Config{Root: root}, resolver, launcher)
	for name, arguments := range map[string]map[string]any{
		"empty":  {"argv": []string{}},
		"nul":    {"argv": []string{"bad\x00value"}},
		"escape": {"argv": []string{"tool"}, "workdir": ".."},
		"schema": {"argv": []string{"tool"}, "env": map[string]string{"BAD": "value"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			requireProblem(t, executeResult(t, executable, t.Context(), makeCall(t, "shell", arguments), nil), nameProblem(name))
		})
	}
	badResolver := &fakeResolver{err: errors.New("private resolution detail")}
	badExecutable, _ := newTestShell(t, Config{Root: root}, badResolver, launcher)
	requireExecutionError(t, badExecutable, t.Context(), makeCall(t, "shell", map[string]any{"argv": []string{"tool"}}),
		nil, tool.ExecutionDefinitive, tool.RetryNever, nil)
	invalidExecutable, _ := newTestShell(t, Config{Root: root}, &fakeResolver{resolved: "relative"}, launcher)
	requireExecutionError(t, invalidExecutable, t.Context(), makeCall(t, "shell", map[string]any{"argv": []string{"tool"}}),
		nil, tool.ExecutionDefinitive, tool.RetryNever, nil)
	nilProcess, _ := newTestShell(t, Config{Root: root}, resolver, &fakeLauncher{})
	requireProblem(t, executeResult(t, nilProcess, t.Context(), makeCall(t, "shell", map[string]any{"argv": []string{"tool"}}), nil), "started")
}

func TestShellReporterFailurePrecedesResolution(t *testing.T) {
	t.Parallel()
	resolver := &fakeResolver{resolved: filepath.Join(t.TempDir(), "tool")}
	executable, _ := newTestShell(t, Config{Root: t.TempDir()}, resolver,
		&fakeLauncher{process: completedProcess(t, 0)})
	call := makeCall(t, "shell", map[string]any{"argv": []string{"tool"}})
	requireExecutionError(t, executable, t.Context(), call,
		reporterFunc(func(context.Context, tool.Progress) error { return errors.New("reject") }),
		tool.ExecutionDefinitive, tool.RetryNever, nil)
	if resolver.count() != 0 {
		t.Fatalf("resolver calls = %d", resolver.count())
	}
}

func TestShellBoundsConcurrentProcessOwnershipReservations(t *testing.T) {
	t.Parallel()
	executable, cleanup := newTestShell(t, Config{Root: t.TempDir()},
		&fakeResolver{resolved: filepath.Join(t.TempDir(), "tool")},
		&fakeLauncher{process: completedProcess(t, 0)})
	shell := requireShellTool(t, executable)
	leases := make([]*executionLease, 0, maximumOwnedShells)
	for range maximumOwnedShells {
		lease, err := shell.beginExecution()
		if err != nil {
			t.Fatal(err)
		}
		leases = append(leases, lease)
	}
	if _, err := shell.beginExecution(); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("overflow admission = %v", err)
	}
	for _, lease := range leases {
		lease.finish()
	}
	if err := cleanup(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestEncodeShellResultFallbackAndMaximum(t *testing.T) {
	t.Parallel()
	callID := tool.CallID(t.Name())
	controls := strings.Repeat("\x00", 180<<10)
	result, err := encodeShellResult(callID, shellContent{OK: true, Encoding: "utf-8", Stdout: controls}, "")
	if err != nil {
		t.Fatal(err)
	}
	encoded := decodeContent[shellContent](t, result)
	decoded, err := base64.StdEncoding.DecodeString(encoded.Stdout)
	if err != nil || encoded.Encoding != "base64" || string(decoded) != controls {
		t.Fatalf("fallback encoding = %q/%d/%v", encoded.Encoding, len(decoded), err)
	}
	tooLarge, err := encodeShellResult(callID, shellContent{
		OK: true, Encoding: "base64", Stdout: strings.Repeat("x", tool.MaximumPayloadBytes),
	}, "")
	if err == nil || !tooLarge.IsZero() {
		t.Fatalf("oversized result = %#v, %v", tooLarge, err)
	}
}

func newTestShell(
	t *testing.T,
	config Config,
	resolver process.ExecutableResolver,
	launcher process.Launcher,
) (tool.Tool, lifecycle.Cleanup) {
	t.Helper()
	value, cleanup, err := NewShell(config, resolver, launcher)
	if err != nil {
		t.Fatal(err)
	}
	return value, cleanup
}

func requireShellTool(t *testing.T, executable tool.Tool) *shellTool {
	t.Helper()
	shell, ok := executable.(*shellTool)
	if !ok {
		t.Fatalf("tool type = %T, want *shellTool", executable)
	}
	return shell
}

func nameProblem(name string) string {
	switch name {
	case "empty", "nul":
		return "argv"
	case "escape":
		return "relative"
	default:
		return "schema"
	}
}

type fakeResolver struct {
	mu       sync.Mutex
	resolved string
	err      error
	lookups  []process.Lookup
}

func (resolver *fakeResolver) Resolve(_ context.Context, lookup process.Lookup) (string, error) {
	resolver.mu.Lock()
	defer resolver.mu.Unlock()
	resolver.lookups = append(resolver.lookups, lookup.Clone())
	return resolver.resolved, resolver.err
}

func (resolver *fakeResolver) count() int {
	resolver.mu.Lock()
	defer resolver.mu.Unlock()
	return len(resolver.lookups)
}

func (resolver *fakeResolver) singleLookup(t *testing.T) process.Lookup {
	t.Helper()
	resolver.mu.Lock()
	defer resolver.mu.Unlock()
	if len(resolver.lookups) != 1 {
		t.Fatalf("resolver lookups = %d", len(resolver.lookups))
	}
	return resolver.lookups[0].Clone()
}

type fakeLauncher struct {
	mu      sync.Mutex
	process process.Process
	start   func(context.Context, process.Spec) (process.Process, error)
	specs   []process.Spec
	started chan struct{}
}

func (launcher *fakeLauncher) Start(ctx context.Context, spec process.Spec) (process.Process, error) {
	launcher.mu.Lock()
	launcher.specs = append(launcher.specs, spec.Clone())
	started := launcher.started
	launcher.started = nil
	custom := launcher.start
	value := launcher.process
	launcher.mu.Unlock()
	if started != nil {
		close(started)
	}
	if custom != nil {
		return custom(ctx, spec)
	}
	return value, nil
}

func (launcher *fakeLauncher) singleSpec(t *testing.T) process.Spec {
	t.Helper()
	launcher.mu.Lock()
	defer launcher.mu.Unlock()
	if len(launcher.specs) != 1 {
		t.Fatalf("launcher specs = %d", len(launcher.specs))
	}
	return launcher.specs[0].Clone()
}

type fakeProcess struct {
	mu                sync.Mutex
	done              chan struct{}
	doneOnce          sync.Once
	outcome           process.Outcome
	resultErr         error
	waitErrors        []error
	waits             int
	stops             int
	forces            int
	stopLeavesRunning bool
	forceCompletes    bool
}

func newFakeProcess(outcome process.Outcome, complete bool) *fakeProcess {
	value := &fakeProcess{done: make(chan struct{}), outcome: outcome}
	if complete {
		value.complete()
	}
	return value
}

func fakeCompletedProcess(outcome process.Outcome) *fakeProcess {
	return newFakeProcess(outcome, true)
}

func completedProcess(t *testing.T, code int64) *fakeProcess {
	t.Helper()
	return fakeCompletedProcess(exitedOutcome(t, code))
}

func exitedOutcome(t *testing.T, code int64) process.Outcome {
	t.Helper()
	outcome, err := process.NewExitedOutcome(code)
	if err != nil {
		t.Fatal(err)
	}
	return outcome
}

func (fixture *fakeProcess) Done() <-chan struct{} { return fixture.done }

func (fixture *fakeProcess) Result() (process.Outcome, error) {
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	return fixture.outcome, fixture.resultErr
}

func (fixture *fakeProcess) RequestStop(context.Context) error {
	fixture.mu.Lock()
	fixture.stops++
	leavesRunning := fixture.stopLeavesRunning
	fixture.mu.Unlock()
	if !leavesRunning {
		fixture.complete()
	}
	return nil
}

func (fixture *fakeProcess) ForceKill(context.Context) error {
	fixture.mu.Lock()
	fixture.forces++
	completes := fixture.forceCompletes
	fixture.mu.Unlock()
	if completes {
		fixture.complete()
	}
	return nil
}

func (fixture *fakeProcess) Wait(context.Context) error {
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	index := fixture.waits
	fixture.waits++
	if index >= len(fixture.waitErrors) {
		return nil
	}
	return fixture.waitErrors[index]
}

func (fixture *fakeProcess) complete() {
	fixture.doneOnce.Do(func() { close(fixture.done) })
}

func (fixture *fakeProcess) waitCalls() int {
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	return fixture.waits
}

func (fixture *fakeProcess) calls() (int, int, int) {
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	return fixture.stops, fixture.forces, fixture.waits
}

type retryClassifiedError struct{ retryable bool }

func (failure retryClassifiedError) Error() string   { return "private classified wait failure" }
func (failure retryClassifiedError) Retryable() bool { return failure.retryable }

func (shell *shellTool) isClosing() bool {
	shell.ownershipMu.Lock()
	defer shell.ownershipMu.Unlock()
	return shell.closing
}
