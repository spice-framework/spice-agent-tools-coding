# Architecture

This module owns three small, instance-scoped tools. `NewRead`, `NewReplace`,
and `NewShell` return the exact public Spice Agent `tool.Tool` interface. The
root package owns validated bounds and static capability declarations.
`autoconfigure` is selected only through an explicit blank import and
contributes three exact fallback beans through ordinary generated Spice DI; it
does not register tools at runtime.

Read opens relative files through `os.Root`, returns bounded offset/limit pages,
and changes to base64 when byte content or JSON escaping requires it. Replace
writes and syncs a same-directory temporary file, revalidates the expected
SHA-256 immediately before an atomic link or replacement, and reports separately
whether the update committed and whether durability was confirmed. A
per-instance lease serializes Spice-originated writes. The expected digest is
stale-write protection, not a filesystem compare-and-swap against another
process changing the file in the final check/commit interval.

Definitions declare behavior used by dispatch policy: read is
`read_only`/`safe`, replace is `mutating`/`idempotent`, and shell is
`mutating`/`unsafe`. Replace replay is idempotent because the successful create
consumes target absence and the successful replace consumes the expected
digest; a repeated call reports already-existing or stale state without a
second mutation. Same-digest replacement is a successful no-op, preventing
inode/metadata churn from defeating this contract. This does not turn a
durability-uncertain result into a safe automatic retry decision.

Shell executes discrete argv without a shell. Its initial working directory is
opened through `os.Root`, symbolic-link components are rejected and revalidated,
and only application-allowlisted host environment variables are inherited.
After process start, the child has the operating-system user's full authority;
it is not confined to the worktree. Unix process groups and Windows kill-on-
close Job Objects provide bounded cleanup for the managed launcher and ordinary
descendants. They are not a containment boundary: a process may deliberately
detach from a Unix group, and a Windows child created before Job Object
attachment is outside that job. `managed_cleanup_completed` therefore means
the requested launcher cleanup and wait completed without error, not that every
possible descendant was terminated. Output and post-force waiting are bounded.

The module may depend on `spice-agent` after its tool contract is tagged. It
must not own an agent loop, model provider, daemon transport, service locator,
global registry, runtime package scanner, or policy engine. Every executable
route must flow through the canonical tool dispatcher so a later permission
decorator can intercept it.

The same-user trust boundary remains explicit. A concurrent actor able to
rename directories or rewrite files can race path-based process startup and the
final stale check. `os.Root`, symlink rejection, immediate revalidation, atomic
replacement, and deterministic diagnostics narrow those races but are not a
sandbox or cross-process transaction.

Expected validation and operating-system failures are terminal model-visible
results. Cooperative cancellation and host/reporting failures are direct,
bounded, call-correlated `*tool.ExecutionError` values with a zero result.
Read/replace pre-commit cancellation is definitive; shell cancellation is
uncertain after process start and never replayable. Wrapped context identity is
preserved for `errors.Is` without exposing unbounded lower-level diagnostics.
