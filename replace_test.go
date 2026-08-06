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

	"github.com/spice-framework/spice-agent/tool"
)

func TestReplaceCreatesAndAtomicallyReplaces(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	replacer, err := NewReplace(Config{Root: root, MaxWriteBytes: 64})
	if err != nil {
		t.Fatal(err)
	}
	created := replacer.Execute(t.Context(), makeCall(t, "replace", map[string]any{
		"path": "nested.txt", "content": "first", "create": true,
	}), nil)
	createdContent := decodeContent[replaceContent](t, created)
	if !createdContent.OK || !createdContent.Created || !createdContent.Committed || !createdContent.Durable ||
		!createdContent.TemporaryCleaned {
		t.Fatalf("create result = %#v", createdContent)
	}
	assertFileContent(t, filepath.Join(root, "nested.txt"), "first")
	firstDigest := sha256.Sum256([]byte("first"))
	replaced := replacer.Execute(t.Context(), makeCall(t, "replace", map[string]any{
		"path": "nested.txt", "content": "second", "expected_sha256": hex.EncodeToString(firstDigest[:]),
	}), nil)
	replacedContent := decodeContent[replaceContent](t, replaced)
	if !replacedContent.OK || replacedContent.Created || !replacedContent.Committed || !replacedContent.Durable ||
		!replacedContent.TemporaryCleaned {
		t.Fatalf("replace result = %#v", replacedContent)
	}
	assertFileContent(t, filepath.Join(root, "nested.txt"), "second")
	matches, err := filepath.Glob(filepath.Join(root, ".spice-replace-*.tmp"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("temporary files = %v, %v", matches, err)
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
			requireProblem(t, replacer.Execute(t.Context(), makeCall(t, "replace", test.arguments), nil), test.problem)
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
	requireProblem(t, replacer.Execute(t.Context(), makeCall(t, "replace", map[string]any{
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
	result := replacer.Execute(t.Context(), makeCall(t, "replace", map[string]any{
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
	result := replacer.Execute(t.Context(), makeCall(t, "replace", map[string]any{
		"path": "value", "content": "committed", "create": true,
	}), nil)
	requireProblem(t, result, "committed")
	content := decodeContent[replaceContent](t, result)
	if content.OK || !content.Committed || content.Durable {
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
			result := replacer.Execute(t.Context(), makeCall(t, "replace", map[string]any{
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
	requireProblem(t, replacer.Execute(ctx, makeCall(t, "replace", map[string]any{
		"path": "value", "content": "content", "create": true,
	}), nil), "cancelled")
	if _, err := os.Stat(filepath.Join(root, "value")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled replace created file: %v", err)
	}
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
			requireProblem(t, replacer.Execute(t.Context(), makeCall(t, "replace", test.arguments), nil), test.problem)
		})
	}
	reporter := reporterFunc(func(context.Context, tool.Progress) error { return errors.New("reject") })
	requireProblem(t, replacer.Execute(t.Context(), makeCall(t, "replace", map[string]any{
		"path": "value", "content": "next", "create": true,
	}), reporter), "rejected")
	missingRoot, err := NewReplace(Config{Root: filepath.Join(root, "missing-root")})
	if err != nil {
		t.Fatal(err)
	}
	requireProblem(t, missingRoot.Execute(t.Context(), makeCall(t, "replace", map[string]any{
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
	if writeErr := writeAndSync(cancelled, file, []byte("content")); writeErr == nil ||
		!strings.Contains(writeErr.Error(), "cancelled") {
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
