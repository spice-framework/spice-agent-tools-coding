# Coding-tool benchmarks

The root-package benchmark suite provides deterministic, offline feedback for
the coding tools' primary filesystem and process-adapter paths:

- `BenchmarkReadExecute` reads and encodes a fixed 48 KiB file from a
  test-owned temporary worktree.
- `BenchmarkAtomicReplaceExecute` alternates two fixed 4 KiB payloads through
  the complete stale-check, temporary-file sync, atomic replace, and durability
  path.
- `BenchmarkShellDispatcherBoundedOutput` dispatches a fixed argv through the
  canonical Agent dispatcher and an injected completed-process fixture that
  produces bounded stdout and stderr.
- `BenchmarkShellDispatcherCancellation` cancels immediately after the
  injected launcher transfers process ownership and exercises stop, join,
  correlation, and uncertain-failure translation.

No benchmark starts a real command, performs network I/O, or accesses files
outside `testing.B.TempDir`. The injected shell resolver returns a fixed path,
the launcher is an in-memory fixture, and its only inherited environment value
is test-owned and fixed.

Run all samples through the repository-owned offline gate:

```text
make benchmark
```

The gate selects exactly these four benchmarks, runs five fixed 500-iteration
samples with `-cpu=1`, records allocations, forces `GOWORK=off`, and disables
the proxy and checksum database while using the committed vendor graph.
Missing dependencies therefore fail instead of causing a hidden download.

Results remain provisional diagnostic evidence during pre-1.0 contract stress.
Record the exact source, Go version, platform, CPU, and filesystem with every
comparison. Use multiple samples and `benchstat`; do not promote a single-host
measurement to a quality-gate threshold. Stable budgets require representative
Windows and Linux histories and an explicit regression policy.

## Initial provisional baseline

The benchmark-introduction tree was sampled three times on 2026-08-09 with Go
1.26.5 on Windows/amd64, an AMD Ryzen 9 5900X, and an NTFS temporary worktree.
The table records the median of each reported metric and proves that the
benchmark is executable; it is not a release budget.

| Benchmark | ns/op | MB/s | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: |
| Read 48 KiB | 596,730 | 82.37 | 308,549 | 62 |
| Atomic replace 4 KiB | 8,151,946 | 0.50 | 58,883 | 147 |
| Bounded-output shell dispatch | 316,374 | 51.79 | 36,611 | 88 |
| Post-launch shell cancellation | 293,795 | — | 5,854 | 96 |

The sampled tree was based on commit
`6edef0fd92e090dd6e11c46df76af777dbe1ea08`. Exact SHA-256 source identities
were:

| Source | SHA-256 |
| --- | --- |
| `read.go` | `2d454d9ba85e59452917e99479cd11fe7b47c63b5bd41d3d222a4cd2282caa24` |
| `replace.go` | `56dc279200cae89a5400a105bebe88102fc676461ef6d921c3d3e3a75cc2add0` |
| `shell.go` | `9e2ed5f201929fa8a1d5b73004502521394b21cd4b39f0c74c8e0994eac086b4` |
| `process.go` | `1cd22591f077f23c76bdb7e67af00405f98223efd0d826f3cd7a1eea052168dd` |
| `coding_benchmark_test.go` | `08a7a5510529e249d40bfb73a441578b30dbe591ace97e3749c9da7f75349794` |

The atomic-replace result includes file and directory synchronization and is
therefore intentionally filesystem-sensitive. The shell results include
`os.Root` workdir validation and Agent dispatcher translation but exclude an
operating-system process launch; real launcher costs belong to the consuming
distribution's separately benchmarked process implementation.

Future observations use `make benchmark`; the historical table above predates
the repository-owned command and remains descriptive evidence only.
