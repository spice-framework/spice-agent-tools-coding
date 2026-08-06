# Dependency review

## Decision

The coding-tools product remains standard-library-first. Spice core and
toolchain remain the framework/build dependencies at `v0.1.0-preview.1`. Spice
Agent is pinned at `v0.0.0-20260806183953-eaf19180429a` for its public immutable
tool contract. `golang.org/x/sys/windows` v0.47.0 is the sole non-framework
runtime package and is compiled only on Windows for Job Object ownership.

## Maintenance and license

Filesystem, process, context, hashing, and atomic-file behavior use Go's
supported standard library under its BSD-style license. `x/sys` is maintained
by the Go project, uses a BSD-3-Clause license, follows the Go vulnerability and
release process, and exposes the Windows Job Object APIs missing from the
standard library. Version 0.47.0 is checksum-pinned and reviewed as a direct
Windows dependency.

## Security and ownership

- A Suite is instance-owned and rooted at one validated absolute path.
- Construction performs no filesystem or process operation.
- Read pages, writes, and total captured output default to 256 KiB and may not
  exceed 512 KiB; commands default to two minutes and may not exceed 30 minutes.
  JSON construction is additionally checked by Spice Agent's 1 MiB public
  payload bound.
- Capability metadata discloses read, write, and process risk but is not
  represented as a sandbox or permission boundary.
- `os.Root` rejects traversal and link escape for read/replace operations.
  Shell rejects and revalidates link components before its path-based process
  start. Same-user concurrent mutation remains explicitly trusted.
- Replace performs expected-digest checks before and immediately before atomic
  commit. This is stale detection, not an external-writer filesystem CAS.
- Windows Job Objects and Unix process groups own child trees; forced-wait
  completion is bounded and unconfirmed termination is returned explicitly.

## Observability

Results contain byte counts, exit classification, truncation,
cancellation/timeout, termination confirmation, and replace commit/durability
state. File contents, command output, environment values, and arguments are
excluded from general diagnostics.

## Build-only dependencies

The Spice toolchain is authorized through the standard Go `tool` directive and
normal module selection. Committed vendor data makes compiler and product
verification available offline. No custom plugin or dependency registry exists.
