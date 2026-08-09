package coding

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/spice-framework/spice-agent/process"
	"github.com/spice-framework/spice-agent/stage"
	"github.com/spice-framework/spice-agent/tool"
)

const benchmarkEnvironmentName = "SPICE_AGENT_CODING_BENCHMARK_FIXED"

var (
	benchmarkCodingResult tool.Result
	benchmarkCodingError  error
)

func BenchmarkReadExecute(b *testing.B) {
	root := b.TempDir()
	content := bytes.Repeat([]byte("spice-agent\n"), 4096)
	if err := os.WriteFile(filepath.Join(root, "benchmark.txt"), content, 0o600); err != nil {
		b.Fatal(err)
	}
	reader, err := NewRead(Config{Root: root, MaxReadBytes: int64(len(content))})
	if err != nil {
		b.Fatal(err)
	}
	call := makeCall(b, "read", map[string]any{"path": "benchmark.txt"})
	b.SetBytes(int64(len(content)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		result, executeErr := reader.Execute(context.Background(), call, nil)
		if executeErr != nil {
			b.Fatal(executeErr)
		}
		if problem, present := result.Problem(); present {
			b.Fatalf("read problem = %q", problem)
		}
		benchmarkCodingResult = result
	}
}

func BenchmarkAtomicReplaceExecute(b *testing.B) {
	root := b.TempDir()
	path := filepath.Join(root, "benchmark.txt")
	contentA := bytes.Repeat([]byte("a"), 4096)
	contentB := bytes.Repeat([]byte("b"), 4096)
	if err := os.WriteFile(path, contentA, 0o600); err != nil {
		b.Fatal(err)
	}
	replacer, err := NewReplace(Config{Root: root, MaxWriteBytes: int64(len(contentA))})
	if err != nil {
		b.Fatal(err)
	}
	digestA := sha256.Sum256(contentA)
	digestB := sha256.Sum256(contentB)
	calls := [...]tool.Call{
		makeCall(b, "replace", map[string]any{
			"path": "benchmark.txt", "content": string(contentB),
			"expected_sha256": hex.EncodeToString(digestA[:]),
		}),
		makeCall(b, "replace", map[string]any{
			"path": "benchmark.txt", "content": string(contentA),
			"expected_sha256": hex.EncodeToString(digestB[:]),
		}),
	}
	iteration := 0
	b.SetBytes(int64(len(contentA)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		result, executeErr := replacer.Execute(context.Background(), calls[iteration%len(calls)], nil)
		if executeErr != nil {
			b.Fatal(executeErr)
		}
		if problem, present := result.Problem(); present {
			b.Fatalf("replace problem = %q", problem)
		}
		benchmarkCodingResult = result
		iteration++
	}
}

func BenchmarkShellDispatcherBoundedOutput(b *testing.B) {
	root := b.TempDir()
	b.Setenv(benchmarkEnvironmentName, "fixed")
	outcome, err := process.NewExitedOutcome(0)
	if err != nil {
		b.Fatal(err)
	}
	completed := fakeCompletedProcess(outcome)
	stdout := bytes.Repeat([]byte("o"), 8192)
	stderr := bytes.Repeat([]byte("e"), 8192)
	executable, cleanup, err := NewShell(
		Config{
			Root: root, MaxOutputBytes: 4096,
			EnvironmentAllowlist: []string{benchmarkEnvironmentName},
		},
		process.ResolverFunc(func(context.Context, process.Lookup) (string, error) {
			return filepath.Join(root, "benchmark-tool"), nil
		}),
		process.LauncherFunc(func(_ context.Context, spec process.Spec) (process.Process, error) {
			if _, writeErr := spec.Stdout().Write(stdout); writeErr != nil {
				return nil, writeErr
			}
			if _, writeErr := spec.Stderr().Write(stderr); writeErr != nil {
				return nil, writeErr
			}
			return completed, nil
		}),
	)
	if err != nil {
		b.Fatal(err)
	}
	dispatcher, err := stage.NewDispatcher(map[string]tool.Tool{"shell": executable})
	if err != nil {
		b.Fatal(err)
	}
	call := makeCall(b, "shell", map[string]any{"argv": []string{"benchmark-tool", "--fixed"}})
	b.SetBytes(int64(len(stdout) + len(stderr)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		result, dispatchErr := dispatcher.Dispatch(context.Background(), call, nil)
		if dispatchErr != nil {
			b.Fatal(dispatchErr)
		}
		if problem, present := result.Problem(); present {
			b.Fatalf("shell problem = %q", problem)
		}
		benchmarkCodingResult = result
	}
	b.StopTimer()
	var content shellContent
	if err = json.Unmarshal(benchmarkCodingResult.Content(), &content); err != nil {
		b.Fatal(err)
	}
	if !content.OutputTruncated || content.StdoutBytes != int64(len(stdout)) ||
		content.StderrBytes != int64(len(stderr)) {
		b.Fatalf("bounded shell content = %#v", content)
	}
	if err = cleanup(context.Background()); err != nil {
		b.Fatal(err)
	}
}

func BenchmarkShellDispatcherCancellation(b *testing.B) {
	root := b.TempDir()
	b.Setenv(benchmarkEnvironmentName, "fixed")
	outcome, err := process.NewExitedOutcome(137)
	if err != nil {
		b.Fatal(err)
	}
	var cancelCurrent context.CancelFunc
	executable, cleanup, err := NewShell(
		Config{Root: root, EnvironmentAllowlist: []string{benchmarkEnvironmentName}},
		process.ResolverFunc(func(context.Context, process.Lookup) (string, error) {
			return filepath.Join(root, "benchmark-tool"), nil
		}),
		process.LauncherFunc(func(context.Context, process.Spec) (process.Process, error) {
			cancelCurrent()
			return newFakeProcess(outcome, false), nil
		}),
	)
	if err != nil {
		b.Fatal(err)
	}
	dispatcher, err := stage.NewDispatcher(map[string]tool.Tool{"shell": executable})
	if err != nil {
		b.Fatal(err)
	}
	call := makeCall(b, "shell", map[string]any{"argv": []string{"benchmark-tool", "--fixed"}})
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, cancel := context.WithCancel(context.Background())
		cancelCurrent = cancel
		result, dispatchErr := dispatcher.Dispatch(ctx, call, nil)
		cancel()
		if !result.IsZero() || !errors.Is(dispatchErr, context.Canceled) {
			b.Fatalf("cancelled shell result/error = %#v/%v", result, dispatchErr)
		}
		benchmarkCodingError = dispatchErr
	}
	b.StopTimer()
	if err = cleanup(context.Background()); err != nil {
		b.Fatal(err)
	}
}
