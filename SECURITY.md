# Security policy

Report vulnerabilities privately through GitHub Security Advisories for this
repository. Do not include secrets, proprietary source, or command output in a
public issue.

These tools intentionally run with the operating-system user's filesystem,
process, network, secret, and environment privileges. They do not provide a
sandbox or approval prompt. The first-run and help surfaces must prominently
disclose that fact. Capability metadata is descriptive and supports future
policy interception; it is not a security boundary. A shell starts in a
validated worktree directory but can subsequently access any user-authorized
resource.

Read and replace operations use Go's traversal-resistant `os.Root`. Replace
serializes calls per tool instance, rejects symbolic-link targets, writes and
syncs a same-directory temporary file, rechecks the expected digest immediately
before commit, and uses atomic replacement or no-overwrite hard-link creation. An
external same-user writer can still race the final check and commit because the
portable filesystem contract has no conditional compare-and-swap primitive.

Shell uses discrete argv, rejects symbolic-link workdir components, revalidates
them immediately before launch, and inherits only configured environment names.
The consuming application injects the public executable resolver and process
launcher; this module contains no platform launcher. Every non-nil returned
process is retained immediately and ownership is released only after typed
`Wait` success. `managed_cleanup_completed` confirms that join, not containment
of resources the platform launcher never owned. Incomplete managed
cleanup after start returns a direct uncertain, never-replayable execution
failure rather than model-visible output. Output is bounded and truncation is
explicit.

File contents, command arguments, inherited environment values, process output,
and raw reporter/operating-system failure chains must not enter general
diagnostics. Only canonical caller cancellation/deadline identity is preserved
through `errors.Is`. Tool call/result events may contain
the model-requested arguments and bounded result by design, so callers must not
place secrets in argv or file content.

Supported versions receive fixes on the latest preview line until a stable
support policy is published.
