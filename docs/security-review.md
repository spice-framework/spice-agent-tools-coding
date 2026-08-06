# Security review

The architecture-proof distribution deliberately grants coding tools the same
authority as the user process. It does not provide a sandbox, permission prompt,
or least-privilege broker. The warning exported as `coding.SecurityWarning`
must be shown on first run and in help output.

| Risk | Required control |
|---|---|
| Worktree escape | Resolve against the configured absolute root and reject traversal, volume changes, and symlink/reparse-point escape. |
| Lost update | Replace only expected content or an expected digest and use same-directory atomic rename. |
| Partial write | Write, sync, and close a temporary file before atomic replacement; clean up on every failure. |
| Unbounded input/output | Enforce configured byte limits before allocation and while streaming. |
| Hung or orphan process | Apply caller context and timeout and terminate the complete process tree on Windows and Unix. |
| Secret leakage | Exclude file content, command arguments, environment, and output from default diagnostics. |
| Capability confusion | Disclose risk metadata while documenting that it is not enforcement. |

Phase 0 establishes configuration, bounds, and disclosure only. Operational
tools cannot be security-approved until Phase 3 tests prove these controls on
Windows and Unix.
