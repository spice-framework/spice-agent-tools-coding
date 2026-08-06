package coding

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/spice-framework/spice-agent/tool"
)

func TestReadFullPagePagingAndBinaryEncoding(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	content := []byte("abcdefgh")
	if err := os.WriteFile(filepath.Join(root, "value.txt"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	reader, err := NewRead(Config{Root: root, MaxReadBytes: 16})
	if err != nil {
		t.Fatal(err)
	}
	definition := reader.Definition()
	if definition.Name() != "read" || len(definition.Capabilities()) != 1 ||
		definition.Capabilities()[0] != tool.CapabilityFilesystemRead {
		t.Fatalf("Definition() = %#v", definition)
	}
	full := decodeContent[readContent](t, executeResult(t, reader, t.Context(), makeCall(t, "read", map[string]any{
		"path": "value.txt",
	}), nil))
	digest := sha256.Sum256(content)
	if !full.OK || full.Content != string(content) || full.SHA256 != hex.EncodeToString(digest[:]) || full.Truncated {
		t.Fatalf("full read = %#v", full)
	}
	first := decodeContent[readContent](t, executeResult(t, reader, t.Context(), makeCall(t, "read", map[string]any{
		"path": "value.txt", "limit": 4,
	}), nil))
	if first.Content != "abcd" || !first.Truncated || first.NextOffset != 4 || first.SHA256 != "" {
		t.Fatalf("first page = %#v", first)
	}
	second := decodeContent[readContent](t, executeResult(t, reader, t.Context(), makeCall(t, "read", map[string]any{
		"path": "value.txt", "offset": 4, "limit": 4,
	}), nil))
	if second.Content != "efgh" || second.Truncated || second.Offset != 4 {
		t.Fatalf("second page = %#v", second)
	}
	binary := []byte{0xff, 0x00, 0xfe}
	if writeErr := os.WriteFile(filepath.Join(root, "binary"), binary, 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	binaryResult := decodeContent[readContent](t, executeResult(t, reader, t.Context(), makeCall(t, "read", map[string]any{
		"path": "binary",
	}), nil))
	decoded, err := base64.StdEncoding.DecodeString(binaryResult.Content)
	if err != nil || binaryResult.Encoding != "base64" || !bytes.Equal(decoded, binary) {
		t.Fatalf("binary read = %#v, decoded=%x, err=%v", binaryResult, decoded, err)
	}
}

func TestReadFallsBackToBoundedBase64Result(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	content := bytes.Repeat([]byte{0}, 300<<10)
	if err := os.WriteFile(filepath.Join(root, "controls"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	reader, err := NewRead(Config{Root: root, MaxReadBytes: int64(len(content))})
	if err != nil {
		t.Fatal(err)
	}
	result := executeResult(t, reader, t.Context(), makeCall(t, "read", map[string]any{"path": "controls"}), nil)
	decodedResult := decodeContent[readContent](t, result)
	decoded, decodeErr := base64.StdEncoding.DecodeString(decodedResult.Content)
	if decodeErr != nil || decodedResult.Encoding != "base64" || !bytes.Equal(decoded, content) {
		t.Fatalf("bounded fallback = encoding %q, bytes %d, error %v", decodedResult.Encoding, len(decoded), decodeErr)
	}
	if len(result.Content()) >= tool.MaximumPayloadBytes {
		t.Fatalf("result size = %d", len(result.Content()))
	}
}

func TestReadRejectsEscapeInvalidRangesAndUnknownArguments(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	reader, err := NewRead(Config{Root: root, MaxReadBytes: 4})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name      string
		toolName  string
		arguments map[string]any
		problem   string
	}{
		{name: "traversal", toolName: "read", arguments: map[string]any{"path": "../outside"}, problem: "relative"},
		{name: "absolute", toolName: "read", arguments: map[string]any{"path": outside}, problem: "relative"},
		{name: "limit", toolName: "read", arguments: map[string]any{"path": "missing", "limit": 5}, problem: "limit"},
		{name: "negative", toolName: "read", arguments: map[string]any{"path": "missing", "offset": -1}, problem: "offset"},
		{name: "unknown", toolName: "read", arguments: map[string]any{"path": "missing", "extra": true}, problem: "schema"},
		{name: "wrong tool", toolName: "shell", arguments: map[string]any{"path": "missing"}, problem: "must be"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			result := executeResult(t, reader, t.Context(), makeCall(t, test.toolName, test.arguments), nil)
			requireProblem(t, result, test.problem)
		})
	}
	link := filepath.Join(root, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Logf("symlink test skipped: %v", err)
		return
	}
	requireProblem(t, executeResult(t, reader, t.Context(), makeCall(t, "read", map[string]any{"path": "escape"}), nil), "unavailable")
}

func TestReadHonorsCancellationAndReporterFailure(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "value"), []byte("value"), 0o600); err != nil {
		t.Fatal(err)
	}
	reader, err := NewRead(Config{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	call := makeCall(t, "read", map[string]any{"path": "value"})
	requireExecutionError(t, reader, ctx, call, nil, tool.ExecutionDefinitive, tool.RetryAllowed, context.Canceled)
	reporter := reporterFunc(func(context.Context, tool.Progress) error { return errors.New("reject") })
	requireExecutionError(
		t, reader, t.Context(), call, reporter, tool.ExecutionDefinitive, tool.RetryAllowed, nil,
	)
}

func TestReadRejectsInvalidConfigurationAndUnavailableFiles(t *testing.T) {
	t.Parallel()
	if _, err := NewRead(Config{Root: "relative"}); err == nil {
		t.Fatal("NewRead() accepted a relative root")
	}
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "directory"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "value"), []byte("value"), 0o600); err != nil {
		t.Fatal(err)
	}
	reader, err := NewRead(Config{Root: root, MaxReadBytes: 8})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name      string
		arguments map[string]any
		problem   string
	}{
		{name: "missing", arguments: map[string]any{"path": "missing"}, problem: "does not exist"},
		{name: "directory", arguments: map[string]any{"path": "directory"}, problem: "regular"},
		{name: "offset beyond end", arguments: map[string]any{"path": "value", "offset": 6}, problem: "file size"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			requireProblem(t, executeResult(t, reader, t.Context(), makeCall(t, "read", test.arguments), nil), test.problem)
		})
	}
	missingRoot, err := NewRead(Config{Root: filepath.Join(root, "missing-root")})
	if err != nil {
		t.Fatal(err)
	}
	requireProblem(t, executeResult(t, missingRoot, t.Context(), makeCall(t, "read", map[string]any{"path": "value"}), nil),
		"unavailable")
}

type reporterFunc func(context.Context, tool.Progress) error

func (reporter reporterFunc) Report(ctx context.Context, progress tool.Progress) error {
	return reporter(ctx, progress)
}
