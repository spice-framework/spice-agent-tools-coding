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
| Hung or orphan process | Apply caller context and timeout, bound launcher cleanup with a Unix process group or Windows Job Object, and disclose that deliberately detached descendants may escape. |
| Secret leakage | Exclude file content, command arguments, environment, and output from default diagnostics. |
| Capability confusion | Disclose risk metadata while documenting that it is not enforcement. |
| Unsafe replay | Fingerprint explicit effect/replay metadata; treat shell as unsafe and any unconfirmed started-process outcome as uncertain and never replayable. |
| Failure-chain leakage | Return fixed bounded execution-error text and preserve only canonical caller cancellation/deadline identities through `errors.Is`. |

The implementation uses `os.Root` for read/replace traversal resistance,
same-directory synced temporary files, expected-digest revalidation, bounded
read paging, bounded shell capture, Unix process groups, and Windows Job
Objects. Those process primitives manage the launcher and ordinary descendants;
they do not contain a Unix child that deliberately changes its session/group or
a Windows child created during the pre-attachment interval. The architecture-
proof distribution treats detached descendants, the same-user final-check/
rename race, and path-based process-start races as documented trust boundaries,
not hidden sandbox claims.

Cancellation is checked again at the file commit and process-start boundaries.
Once an atomic file commit succeeds, that committed terminal result wins a
concurrent cancellation. Once a shell process starts, cancellation or
incomplete managed-launcher cleanup is conservatively uncertain and never produces
model-visible output that could authorize continuation or replay.

Windows replacement deliberately remains `os.Root.Rename`. Deriving an
absolute path from `os.Root.OpenRoot` and `Root.Name` would not make a later
Win32 replacement handle-relative; a same-user parent rename or reparse-point
swap could redirect that absolute operation outside the validated root.
