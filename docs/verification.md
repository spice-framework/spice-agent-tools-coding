# Verification

- `make fast` checks the exact Go version and runs repository tests.
- `make check` adds goimports/gofumpt, tidy diff, vet, and shuffled tests.
- `make verify` adds allowlisted lint, NilAway, gosec, govulncheck, race tests,
  85% product coverage, reproducible vendor comparison, and vendor-offline
  tests/builds.

The verifier is repository-owned Go and is cross-platform. It never rewrites
product source. `make fmt` is the only formatting target that mutates Go files.
Product tests exercise traversal and symlink escape, paging and encoded payload
bounds, create/replace/stale/race behavior, committed durability uncertainty,
strict argv/workdir/environment policy, output truncation, cancellation,
timeouts, descendant cleanup, fallback DI, and concurrency. Windows uses a real
Job Object; Unix uses a real process group.
