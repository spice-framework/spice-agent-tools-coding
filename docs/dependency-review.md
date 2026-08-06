# Dependency review

## Decision

The coding-tools product remains standard-library-first. It adds no third-party
runtime module. Spice core and toolchain are the explicit framework/build
dependencies at `v0.1.0-preview.1`.

## Maintenance and license

Filesystem, process, context, hashing, and atomic-file behavior use Go's
supported standard library under its BSD-style license. Platform-specific
process-tree handling introduced in Phase 3 must use reviewed standard OS APIs;
a new module requires a separate maintenance, license, security, cancellation,
and observability review before adoption.

## Security and ownership

- A Suite is instance-owned and rooted at one validated absolute path.
- Construction performs no filesystem or process operation.
- Limits default to 4 MiB reads, 4 MiB writes, 2 MiB captured output, and a
  two-minute command timeout; values above 64 MiB or 30 minutes fail closed.
- Capability metadata discloses read, write, and process risk but is not
  represented as a sandbox or permission boundary.
- Phase 3 must resolve and revalidate real paths, reject escape through links,
  perform identity-checked atomic writes, bound stdout/stderr, and terminate
  process trees on cancellation.

## Observability

Future observations contain stable operation/tool identity, duration, byte
counts, exit classification, and typed outcome. File contents, command output,
environment values, and arguments are excluded from general diagnostics.

## Build-only dependencies

The Spice toolchain is authorized through the standard Go `tool` directive and
normal module selection. Committed vendor data makes compiler and product
verification available offline. No custom plugin or dependency registry exists.
