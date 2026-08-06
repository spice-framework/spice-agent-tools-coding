//go:build windows

package coding

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/spice-framework/spice-agent/tool"
	"golang.org/x/sys/windows"
)

func TestReplaceRetriesSharingViolationWithFreshStaleCheck(t *testing.T) {
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
	attempts := 0
	replacer.renameTarget = func(workspace *os.Root, oldName, newName string) error {
		attempts++
		if attempts == 1 {
			return windows.ERROR_SHARING_VIOLATION
		}
		return renameWithinRoot(workspace, oldName, newName)
	}
	digest := sha256.Sum256([]byte("first"))
	result := replacer.Execute(t.Context(), makeCall(t, "replace", map[string]any{
		"path": "value", "content": "second", "expected_sha256": hex.EncodeToString(digest[:]),
	}), nil)
	content := decodeContent[replaceContent](t, result)
	if !content.OK || attempts != 2 {
		problem, _ := result.Problem()
		t.Fatalf("replace = %#v, attempts = %d, problem = %q", content, attempts, problem)
	}
	assertFileContent(t, path, "second")
}

func TestReplaceRetriesWindowsAccessDeniedThenSucceeds(t *testing.T) {
	t.Parallel()
	replacer, path := newWindowsRenameTestTool(t)
	attempts := 0
	replacer.renameTarget = func(workspace *os.Root, oldName, newName string) error {
		attempts++
		if attempts == 1 {
			return windows.ERROR_ACCESS_DENIED
		}
		return renameWithinRoot(workspace, oldName, newName)
	}
	result := executeWindowsRenameTest(t, replacer)
	content := decodeContent[replaceContent](t, result)
	if !content.OK || attempts != 2 {
		problem, _ := result.Problem()
		t.Fatalf("replace = %#v, attempts = %d, problem = %q", content, attempts, problem)
	}
	assertFileContent(t, path, "second")
}

func TestReplaceExhaustsPersistentTransientWindowsRename(t *testing.T) {
	t.Parallel()
	replacer, path := newWindowsRenameTestTool(t)
	attempts := 0
	replacer.renameTarget = func(*os.Root, string, string) error {
		attempts++
		return windows.ERROR_LOCK_VIOLATION
	}
	result := executeWindowsRenameTest(t, replacer)
	requireProblem(t, result, "could not be committed")
	if attempts != maximumRenameAttempts {
		t.Fatalf("rename attempts = %d, want %d", attempts, maximumRenameAttempts)
	}
	assertFileContent(t, path, "first")
}

func TestReplaceExhaustsPersistentWindowsAccessDenied(t *testing.T) {
	t.Parallel()
	replacer, path := newWindowsRenameTestTool(t)
	attempts := 0
	replacer.renameTarget = func(*os.Root, string, string) error {
		attempts++
		return windows.ERROR_ACCESS_DENIED
	}
	result := executeWindowsRenameTest(t, replacer)
	requireProblem(t, result, "could not be committed")
	if attempts != maximumRenameAttempts {
		t.Fatalf("rename attempts = %d, want %d", attempts, maximumRenameAttempts)
	}
	assertFileContent(t, path, "first")
}

func TestReplaceDoesNotRetryNonTransientWindowsRename(t *testing.T) {
	t.Parallel()
	replacer, path := newWindowsRenameTestTool(t)
	attempts := 0
	replacer.renameTarget = func(*os.Root, string, string) error {
		attempts++
		return windows.ERROR_INVALID_PARAMETER
	}
	result := executeWindowsRenameTest(t, replacer)
	requireProblem(t, result, "could not be committed")
	if attempts != 1 {
		t.Fatalf("rename attempts = %d, want 1", attempts)
	}
	assertFileContent(t, path, "first")
}

func TestReplaceCancelsDuringWindowsRenameRetry(t *testing.T) {
	t.Parallel()
	replacer, path := newWindowsRenameTestTool(t)
	ctx, cancel := context.WithCancel(t.Context())
	attempts := 0
	replacer.renameTarget = func(*os.Root, string, string) error {
		attempts++
		cancel()
		return windows.ERROR_SHARING_VIOLATION
	}
	result := replacer.Execute(ctx, windowsRenameCall(t), nil)
	requireProblem(t, result, "cancelled")
	if attempts != 1 {
		t.Fatalf("rename attempts = %d, want 1", attempts)
	}
	assertFileContent(t, path, "first")
}

func TestReplaceRevalidatesContentBeforeWindowsRenameRetry(t *testing.T) {
	t.Parallel()
	replacer, path := newWindowsRenameTestTool(t)
	attempts := 0
	replacer.renameTarget = func(*os.Root, string, string) error {
		attempts++
		if err := os.WriteFile(path, []byte("external"), 0o600); err != nil {
			t.Fatal(err)
		}
		return windows.ERROR_SHARING_VIOLATION
	}
	result := executeWindowsRenameTest(t, replacer)
	requireProblem(t, result, "changed")
	if attempts != 1 {
		t.Fatalf("rename attempts = %d, want 1", attempts)
	}
	assertFileContent(t, path, "external")
}

func newWindowsRenameTestTool(t *testing.T) (*replaceTool, string) {
	t.Helper()
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
	return replacer, path
}

func windowsRenameCall(t *testing.T) tool.Call {
	t.Helper()
	digest := sha256.Sum256([]byte("first"))
	return makeCall(t, "replace", map[string]any{
		"path": "value", "content": "second", "expected_sha256": hex.EncodeToString(digest[:]),
	})
}

func executeWindowsRenameTest(t *testing.T, replacer *replaceTool) tool.Result {
	t.Helper()
	return replacer.Execute(t.Context(), windowsRenameCall(t), nil)
}
