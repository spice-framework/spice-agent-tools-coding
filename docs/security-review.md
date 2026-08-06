# Security review

The architecture-proof distribution deliberately grants coding tools the same
authority as the user process. It does not provide a sandbox, permission prompt,
or least-privilege broker. The warning exported as `coding.SecurityWarning`
must be shown on first run and in help output.

| Risk | Required control |
|---|---|
| Worktree escape | Resolve against the configured absolute root and reject traversal, volume changes, and symlink/reparse-point escape. |
| Lost update | Replace only the expected SHA-256 digest and use same-directory atomic replacement. |
| Partial write | Write, sync, and close a temporary file before atomic replacement; clean up on every failure. |
| Unbounded input/output | Enforce configured byte limits before allocation and while streaming. |
| Hung or orphan process | Apply caller context and timeout and terminate the complete process tree on Windows and Unix. |
| Secret leakage | Exclude file content, command arguments, environment, and output from default diagnostics. |
| Capability confusion | Disclose risk metadata while documenting that it is not enforcement. |

The implementation uses `os.Root` for read/replace traversal resistance,
same-directory synced temporary files, expected-digest revalidation, bounded
read paging, bounded shell capture, Unix process groups, and Windows Job
Objects. The architecture-proof distribution still treats the same-user
final-check/rename and path-based process-start races as documented trust
boundaries, not hidden sandbox claims.
