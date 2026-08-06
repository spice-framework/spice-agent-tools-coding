# Verification

On a fresh clone, explicitly populate the exact product and tools module graphs:

```text
make tools-bootstrap
```

This is the only network-enabled quality mode. It requires Go 1.26.5, downloads
`all` from private temporary copies of both `go.mod`/`go.sum` pairs, disables Go
authentication, and permits only the public checksum database and module proxy.
It verifies that the repository is byte-for-byte unchanged even when a download
fails. A repository without a tools module is valid. No application API keys,
tokens, passwords, or secrets are passed to the Go subprocess.

- `make fast` checks the exact Go version and runs repository tests.
- `make check` adds goimports/gofumpt, tidy diff, vet, and shuffled tests.
- `make verify` adds allowlisted lint, NilAway, gosec, govulncheck, race tests,
  85% product coverage, reproducible vendor comparison, and vendor-offline
  tests/builds.

The verifier is repository-owned Go and is cross-platform. It never rewrites
product source. `make fmt` is the only formatting target that mutates Go files.
`make fast`, `make check`, and `make verify` force `GOPROXY=off`; a missing cache
entry fails with an actionable prompt to run the explicit bootstrap.
Product tests exercise traversal and symlink escape, paging and encoded payload
bounds, create/replace/stale/race behavior, committed durability uncertainty,
strict argv/workdir/environment policy, output truncation, cancellation,
timeouts, descendant cleanup, fallback DI, and concurrency. Windows uses a real
Job Object; Unix uses a real process group.
