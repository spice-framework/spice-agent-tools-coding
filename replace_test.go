package coding

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/spice-framework/spice-agent/stage"
	"github.com/spice-framework/spice-agent/tool"
)

func TestReplaceCreatesAndAtomicallyReplaces(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	constructed, err := NewReplace(Config{Root: root, MaxWriteBytes: 64})
	if err != nil {
		t.Fatal(err)
	}
	replacer, ok := constructed.(*replaceTool)
	if !ok {
		t.Fatalf("NewReplace() type = %T", constructed)
	}
	rename := replacer.renameTarget
	var lastRenameErr error
	replacer.renameTarget = func(workspace *os.Root, oldName, newName string) error {
		lastRenameErr = rename(workspace, oldName, newName)
		return lastRenameErr
	}
	created := executeResult(t, replacer, t.Context(), makeCall(t, "replace", map[string]any{
		"path": "nested.txt", "content": "first", "create": true,
	}), nil)
	createdContent := decodeContent[replaceContent](t, created)
	if !createdContent.OK || !createdContent.Created || !createdContent.Changed || !createdContent.Committed || !createdContent.Durable ||
		!createdContent.TemporaryCleaned {
		t.Fatalf("create result = %#v", createdContent)
	}
	assertFileContent(t, filepath.Join(root, "nested.txt"), "first")
	firstDigest := sha256.Sum256([]byte("first"))
	replaced := executeResult(t, replacer, t.Context(), makeCall(t, "replace", map[string]any{
		"path": "nested.txt", "content": "second", "expected_sha256": hex.EncodeToString(firstDigest[:]),
	}), nil)
	replacedContent := decodeContent[replaceContent](t, replaced)
	if !replacedContent.OK || replacedContent.Created || !replacedContent.Changed || !replacedContent.Committed || !replacedContent.Durable ||
		!replacedContent.TemporaryCleaned {
		problem, _ := replaced.Problem()
		t.Fatalf("replace result = %#v, problem = %q, rename error = %#v", replacedContent, problem, lastRenameErr)
	}
	assertFileContent(t, filepath.Join(root, "nested.txt"), "second")
	matches, err := filepath.Glob(filepath.Join(root, ".spice-replace-*.tmp"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("temporary files = %v, %v", matches, err)
	}
}

func TestReplaceReplayAfterAcknowledgementLossDoesNotDuplicateEffect(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	replacer, err := NewReplace(Config{Root: root, MaxWriteBytes: 64})
	if err != nil {
		t.Fatal(err)
	}
	if definition := replacer.Definition(); definition.Effect() != tool.EffectMutating ||
		definition.ReplaySafety() != tool.ReplayIdempotent {
		t.Fatalf(
			"replace effect/replay = %q/%q",
			definition.Effect(), definition.ReplaySafety(),
		)
	}

	create := makeCall(t, "replace", map[string]any{
		"path": "created", "content": "once", "create": true,
	})
	firstCreate := executeResult(t, replacer, t.Context(), create, nil)
	if problem, present := firstCreate.Problem(); present {
		t.Fatalf("first create problem = %q", problem)
	}
	replayedCreate := executeResult(t, replacer, t.Context(), create, nil)
	requireProblem(t, replayedCreate, "exists")
	assertFileContent(t, filepath.Join(root, "created"), "once")

	path := filepath.Join(root, "replaced")
	if writeErr := os.WriteFile(path, []byte("before"), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	digest := sha256.Sum256([]byte("before"))
	replace := makeCall(t, "replace", map[string]any{
		"path": "replaced", "content": "after", "expected_sha256": hex.EncodeToString(digest[:]),
	})
	firstReplace := executeResult(t, replacer, t.Context(), replace, nil)
	if problem, present := firstReplace.Problem(); present {
		t.Fatalf("first replace problem = %q", problem)
	}
	replayedReplace := executeResult(t, replacer, t.Context(), replace, nil)
	requireProblem(t, replayedReplace, "changed")
	assertFileContent(t, path, "after")

	unchangedPath := filepath.Join(root, "unchanged")
	if writeErr := os.WriteFile(unchangedPath, []byte("same"), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	constructed, err := NewReplace(Config{Root: root, MaxWriteBytes: 64})
	if err != nil {
		t.Fatal(err)
	}
	concrete, ok := constructed.(*replaceTool)
	if !ok {
		t.Fatalf("NewReplace() type = %T", constructed)
	}
	renameCalls := 0
	rename := concrete.renameTarget
	concrete.renameTarget = func(workspace *os.Root, oldName, newName string) error {
		renameCalls++
		return rename(workspace, oldName, newName)
	}
	unchangedDigest := sha256.Sum256([]byte("same"))
	unchanged := makeCall(t, "replace", map[string]any{
		"path": "unchanged", "content": "same", "expected_sha256": hex.EncodeToString(unchangedDigest[:]),
	})
	for range 2 {
		result := executeResult(t, concrete, t.Context(), unchanged, nil)
		content := decodeContent[replaceContent](t, result)
		if !content.OK || content.Changed || content.Committed || !content.Durable {
			t.Fatalf("unchanged replay result = %#v", content)
		}
	}
	if renameCalls != 0 {
		t.Fatalf("unchanged replay rename calls = %d", renameCalls)
	}
	assertFileContent(t, unchangedPath, "same")

	matches, err := filepath.Glob(filepath.Join(root, ".spice-replace-*.tmp"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("replay temporary files = %v, %v", matches, err)
	}
}

func TestReplaceRejectsStaleCreateAndEscapesWithoutMutation(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := filepath.Join(root, "value")
	if err := os.WriteFile(path, []byte("current"), 0o600); err != nil {
		t.Fatal(err)
	}
	replacer, err := NewReplace(Config{Root: root, MaxWriteBytes: 8})
	if err != nil {
		t.Fatal(err)
	}
	stale := sha256.Sum256([]byte("stale"))
	tests := []struct {
		name      string
		arguments map[string]any
		problem   string
	}{
		{name: "stale", arguments: map[string]any{"path": "value", "content": "new", "expected_sha256": hex.EncodeToString(stale[:])}, problem: "changed"},
		{name: "create existing", arguments: map[string]any{"path": "value", "content": "new", "create": true}, problem: "exists"},
		{name: "missing expected", arguments: map[string]any{"path": "value", "content": "new"}, problem: "requires"},
		{name: "create expected", arguments: map[string]any{"path": "value", "content": "new", "create": true, "expected_sha256": strings.Repeat("0", 64)}, problem: "must not"},
		{name: "large content", arguments: map[string]any{"path": "value", "content": "123456789", "create": true}, problem: "bound"},
		{name: "escape", arguments: map[string]any{"path": "../value", "content": "new", "create": true}, problem: "relative"},
		{name: "unknown", arguments: map[string]any{"path": "value", "content": "new", "create": true, "extra": true}, problem: "schema"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			requireProblem(t, executeResult(t, replacer, t.Context(), makeCall(t, "replace", test.arguments), nil), test.problem)
		})
	}
	assertFileContent(t, path, "current")
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Logf("symlink test skipped: %v", err)
		return
	}
	digest := sha256.Sum256([]byte("outside"))
	requireProblem(t, executeResult(t, replacer, t.Context(), makeCall(t, "replace", map[string]any{
		"path": "link", "content": "new", "expected_sha256": hex.EncodeToString(digest[:]),
	}), nil), "non-symbolic-link")
	assertFileContent(t, outside, "outside")
}

func TestReplaceDetectsRaceBeforeCommit(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := filepath.Join(root, "value")
	if err := os.WriteFile(path, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	constructed, err := NewReplace(Config{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	replacer, ok := constructed.(*replaceTool)
	if !ok {
		t.Fatalf("NewReplace() type = %T", constructed)
	}
	replacer.beforeCommit = func() {
		if err := os.WriteFile(path, []byte("external"), 0o600); err != nil {
			panic(err)
		}
	}
	digest := sha256.Sum256([]byte("first"))
	result := executeResult(t, replacer, t.Context(), makeCall(t, "replace", map[string]any{
		"path": "value", "content": "ours", "expected_sha256": hex.EncodeToString(digest[:]),
	}), nil)
	requireProblem(t, result, "changed")
	assertFileContent(t, path, "external")
}

func TestReplaceReportsCommittedDurabilityUncertainty(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	constructed, err := NewReplace(Config{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	replacer, ok := constructed.(*replaceTool)
	if !ok {
		t.Fatalf("NewReplace() type = %T", constructed)
	}
	replacer.syncParent = func(*os.Root, string) error { return errors.New("sync unavailable") }
	result := executeResult(t, replacer, t.Context(), makeCall(t, "replace", map[string]any{
		"path": "value", "content": "committed", "create": true,
	}), nil)
	requireProblem(t, result, "committed")
	content := decodeContent[replaceContent](t, result)
	if content.OK || !content.Changed || !content.Committed || content.Durable {
		t.Fatalf("uncertain durability result = %#v", content)
	}
	assertFileContent(t, filepath.Join(root, "value"), "committed")
}

func TestReplaceSerializesConcurrentExpectedWrites(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := filepath.Join(root, "value")
	if err := os.WriteFile(path, []byte("initial"), 0o600); err != nil {
		t.Fatal(err)
	}
	replacer, err := NewReplace(Config{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("initial"))
	results := make(chan bool, 2)
	var group sync.WaitGroup
	for _, content := range []string{"one", "two"} {
		group.Go(func() {
			result := executeResult(t, replacer, t.Context(), makeCall(t, "replace", map[string]any{
				"path": "value", "content": content, "expected_sha256": hex.EncodeToString(digest[:]),
			}), nil)
			_, problem := result.Problem()
			results <- !problem
		})
	}
	group.Wait()
	close(results)
	successes := 0
	for success := range results {
		if success {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("successful concurrent replacements = %d, want 1", successes)
	}
}

func TestReplaceHonorsCancellationBeforeMutation(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	replacer, err := NewReplace(Config{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	requireExecutionError(t, replacer, ctx, makeCall(t, "replace", map[string]any{
		"path": "value", "content": "content", "create": true,
	}), nil, tool.ExecutionDefinitive, tool.RetryAllowed, context.Canceled)
	if _, err := os.Stat(filepath.Join(root, "value")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled replace created file: %v", err)
	}
}

func TestReplaceCancellationCannotBecomeSuccessfulNoOp(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		configure func(*replaceTool, context.CancelFunc) tool.Reporter
	}{
		{
			name: "reporter cancels and succeeds",
			configure: func(_ *replaceTool, cancel context.CancelFunc) tool.Reporter {
				return reporterFunc(func(context.Context, tool.Progress) error {
					cancel()
					return nil
				})
			},
		},
		{
			name: "cancel after preflight",
			configure: func(replacer *replaceTool, cancel context.CancelFunc) tool.Reporter {
				replacer.afterPreflight = cancel
				return nil
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			path := filepath.Join(root, "value")
			if err := os.WriteFile(path, []byte("same"), 0o600); err != nil {
				t.Fatal(err)
			}
			constructed, err := NewReplace(Config{Root: root})
			if err != nil {
				t.Fatal(err)
			}
			replacer, ok := constructed.(*replaceTool)
			if !ok {
				t.Fatalf("NewReplace() type = %T", constructed)
			}
			renames := 0
			rename := replacer.renameTarget
			replacer.renameTarget = func(workspace *os.Root, oldName, newName string) error {
				renames++
				return rename(workspace, oldName, newName)
			}
			ctx, cancel := context.WithCancel(t.Context())
			reporter := test.configure(replacer, cancel)
			digest := sha256.Sum256([]byte("same"))
			call := makeCall(t, "replace", map[string]any{
				"path": "value", "content": "same", "expected_sha256": hex.EncodeToString(digest[:]),
			})
			requireExecutionError(
				t, replacer, ctx, call, reporter,
				tool.ExecutionDefinitive, tool.RetryAllowed, context.Canceled,
			)
			if renames != 0 {
				t.Fatalf("cancelled no-op rename calls = %d", renames)
			}
			assertFileContent(t, path, "same")
		})
	}
}

func TestReplaceRechecksCancellationAtCreateAndRenameCommitPoints(t *testing.T) {
	t.Parallel()
	t.Run("create", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		constructed, err := NewReplace(Config{Root: root})
		if err != nil {
			t.Fatal(err)
		}
		replacer, ok := constructed.(*replaceTool)
		if !ok {
			t.Fatalf("NewReplace() type = %T", constructed)
		}
		ctx, cancel := context.WithCancel(t.Context())
		replacer.beforeCommit = cancel
		call := makeCall(t, "replace", map[string]any{
			"path": "value", "content": "content", "create": true,
		})
		requireExecutionError(
			t, replacer, ctx, call, nil, tool.ExecutionDefinitive, tool.RetryAllowed, context.Canceled,
		)
		if _, err := os.Stat(filepath.Join(root, "value")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("cancelled create committed target: %v", err)
		}
	})

	t.Run("replace", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		path := filepath.Join(root, "value")
		if err := os.WriteFile(path, []byte("before"), 0o600); err != nil {
			t.Fatal(err)
		}
		constructed, err := NewReplace(Config{Root: root})
		if err != nil {
			t.Fatal(err)
		}
		replacer, ok := constructed.(*replaceTool)
		if !ok {
			t.Fatalf("NewReplace() type = %T", constructed)
		}
		ctx, cancel := context.WithCancel(t.Context())
		replacer.beforeRename = cancel
		digest := sha256.Sum256([]byte("before"))
		call := makeCall(t, "replace", map[string]any{
			"path": "value", "content": "after", "expected_sha256": hex.EncodeToString(digest[:]),
		})
		requireExecutionError(
			t, replacer, ctx, call, nil, tool.ExecutionDefinitive, tool.RetryAllowed, context.Canceled,
		)
		assertFileContent(t, path, "before")
	})
}

func TestReplaceCancellationAfterSuccessfulRenameReturnsCommittedResult(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := filepath.Join(root, "value")
	if err := os.WriteFile(path, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	constructed, err := NewReplace(Config{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	replacer, ok := constructed.(*replaceTool)
	if !ok {
		t.Fatalf("NewReplace() type = %T", constructed)
	}
	ctx, cancel := context.WithCancel(t.Context())
	rename := replacer.renameTarget
	replacer.renameTarget = func(workspace *os.Root, oldName, newName string) error {
		renameErr := rename(workspace, oldName, newName)
		if renameErr == nil {
			cancel()
		}
		return renameErr
	}
	digest := sha256.Sum256([]byte("before"))
	call := makeCall(t, "replace", map[string]any{
		"path": "value", "content": "after", "expected_sha256": hex.EncodeToString(digest[:]),
	})
	result, executeErr := replacer.Execute(ctx, call, nil)
	if executeErr != nil {
		t.Fatalf("Execute() after commit error = %v", executeErr)
	}
	content := decodeContent[replaceContent](t, result)
	if !content.Committed || !content.OK || ctx.Err() == nil {
		t.Fatalf("post-commit cancellation result = %#v, context = %v", content, ctx.Err())
	}
	assertFileContent(t, path, "after")
}

func TestDispatcherPreservesCommittedReplaceWhenContextCancelsAtCommit(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := filepath.Join(root, "value")
	if err := os.WriteFile(path, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	constructed, err := NewReplace(Config{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	replacer, ok := constructed.(*replaceTool)
	if !ok {
		t.Fatalf("NewReplace() type = %T", constructed)
	}
	ctx, cancel := context.WithCancel(t.Context())
	rename := replacer.renameTarget
	replacer.renameTarget = func(workspace *os.Root, oldName, newName string) error {
		renameErr := rename(workspace, oldName, newName)
		if renameErr == nil {
			cancel()
		}
		return renameErr
	}
	dispatcher, err := stage.NewDispatcher(map[string]tool.Tool{"replace": replacer})
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("before"))
	call := makeCall(t, "replace", map[string]any{
		"path": "value", "content": "after", "expected_sha256": hex.EncodeToString(digest[:]),
	})
	result, dispatchErr := dispatcher.Dispatch(ctx, call, nil)
	if dispatchErr != nil {
		t.Fatalf("Dispatch() after committed mutation error = %v", dispatchErr)
	}
	content := decodeContent[replaceContent](t, result)
	if !content.Committed || !content.OK || ctx.Err() == nil {
		t.Fatalf("dispatch post-commit cancellation result = %#v, context = %v", content, ctx.Err())
	}
	assertFileContent(t, path, "after")
}

func TestReplaceDefinitionValidationAndTargetFailures(t *testing.T) {
	t.Parallel()
	if _, err := NewReplace(Config{Root: "relative"}); err == nil {
		t.Fatal("NewReplace() accepted a relative root")
	}
	root := t.TempDir()
	replacer, err := NewReplace(Config{Root: root, MaxWriteBytes: 8})
	if err != nil {
		t.Fatal(err)
	}
	definition := replacer.Definition()
	if definition.Name() != "replace" || len(definition.Capabilities()) != 2 {
		t.Fatalf("Definition() = %#v", definition)
	}
	tests := []struct {
		name      string
		arguments map[string]any
		problem   string
	}{
		{name: "bad hex", arguments: map[string]any{
			"path": "missing", "content": "next", "expected_sha256": strings.Repeat("g", 64),
		}, problem: "lowercase hexadecimal"},
		{name: "uppercase hex", arguments: map[string]any{
			"path": "missing", "content": "next", "expected_sha256": strings.Repeat("A", 64),
		}, problem: "lowercase hexadecimal"},
		{name: "missing target", arguments: map[string]any{
			"path": "missing", "content": "next", "expected_sha256": strings.Repeat("0", 64),
		}, problem: "does not exist"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			requireProblem(t, executeResult(t, replacer, t.Context(), makeCall(t, "replace", test.arguments), nil), test.problem)
		})
	}
	reporter := reporterFunc(func(context.Context, tool.Progress) error { return errors.New("reject") })
	requireExecutionError(t, replacer, t.Context(), makeCall(t, "replace", map[string]any{
		"path": "value", "content": "next", "create": true,
	}), reporter, tool.ExecutionDefinitive, tool.RetryAllowed, nil)
	missingRoot, err := NewReplace(Config{Root: filepath.Join(root, "missing-root")})
	if err != nil {
		t.Fatal(err)
	}
	requireProblem(t, executeResult(t, missingRoot, t.Context(), makeCall(t, "replace", map[string]any{
		"path": "value", "content": "next", "create": true,
	}), nil), "unavailable")
}

func TestReplaceStorageHelpersReportBoundariesAndFilesystemFailures(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "large"), []byte("12345"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "directory"), 0o750); err != nil {
		t.Fatal(err)
	}
	workspace, workspaceErr := openWorktree(root)
	if workspaceErr != nil {
		t.Fatal(workspaceErr)
	}
	defer workspace.Close() //nolint:errcheck // Test cleanup cannot affect assertions.
	if _, digestErr := boundedDigest(workspace, "missing", 4); digestErr == nil ||
		!strings.Contains(digestErr.Error(), "read") {
		t.Fatalf("boundedDigest(missing) error = %v", digestErr)
	}
	if _, digestErr := boundedDigest(workspace, "directory", 4); digestErr == nil ||
		!strings.Contains(digestErr.Error(), "regular") {
		t.Fatalf("boundedDigest(directory) error = %v", digestErr)
	}
	if _, digestErr := boundedDigest(workspace, "large", 4); digestErr == nil ||
		!strings.Contains(digestErr.Error(), "bound") {
		t.Fatalf("boundedDigest(large) error = %v", digestErr)
	}
	if commitErr := commitCreate(workspace, "large", "large"); commitErr == nil ||
		!strings.Contains(commitErr.Error(), "appeared") {
		t.Fatalf("commitCreate(existing) error = %v", commitErr)
	}
	if commitErr := commitCreate(workspace, "large", filepath.Join("missing", "target")); commitErr == nil ||
		!strings.Contains(commitErr.Error(), "unavailable") {
		t.Fatalf("commitCreate(unavailable) error = %v", commitErr)
	}
	if syncErr := syncCommittedFile(workspace, "missing"); syncErr == nil {
		t.Fatal("syncCommittedFile(missing) succeeded")
	}

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	file, err := os.CreateTemp(root, "cancelled-write-")
	if err != nil {
		t.Fatal(err)
	}
	if writeErr := writeAndSync(cancelled, file, []byte("content")); !errors.Is(writeErr, context.Canceled) {
		t.Fatalf("writeAndSync(cancelled) error = %v", writeErr)
	}
	closed, err := os.CreateTemp(root, "closed-write-")
	if err != nil {
		t.Fatal(err)
	}
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	if err := writeAndSync(t.Context(), closed, []byte("content")); err == nil || !strings.Contains(err.Error(), "written") {
		t.Fatalf("writeAndSync(closed) error = %v", err)
	}
}

func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	content, err := os.ReadFile(path) // #nosec G304 -- test-owned path.
	if err != nil || string(content) != want {
		t.Fatalf("file %q = %q, %v; want %q", path, content, err, want)
	}
}
