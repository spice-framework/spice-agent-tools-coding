# Security policy

Report vulnerabilities privately through GitHub Security Advisories for this
repository. Do not include secrets, proprietary source, or command output in a
public issue.

These tools intentionally run with the same filesystem and process privileges
as the application. Phase 0 does not provide a sandbox or permission prompt.
The first-run and help surfaces must prominently disclose that fact. Capability
metadata is descriptive and supports future policy interception; it is not a
security boundary.

All operations must remain within the configured absolute root, reject path
escape including through symlinks, bound inputs and outputs, honor caller
cancellation, and terminate complete process trees. Writes must be atomic and
must not silently overwrite a file whose expected identity changed.

Supported versions receive fixes on the latest preview line until a stable
support policy is published.
