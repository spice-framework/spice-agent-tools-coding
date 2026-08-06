# Architecture

This module owns a small, instance-scoped coding tool suite. The root package
owns validated bounds and static capability declarations. Future `read`,
`replace`, and `shell` packages implement operations through the public Spice
Agent tool contract. `autoconfigure` is selected only through an explicit blank
import and contributes fallback beans through ordinary generated Spice DI.

The module may depend on `spice-agent` after its tool contract is tagged. It
must not own an agent loop, model provider, daemon transport, service locator,
global registry, runtime package scanner, or policy engine. Every executable
route must flow through the canonical tool dispatcher so a later permission
decorator can intercept it.

Phase 0 intentionally exposes a concrete Suite without guessing the unpublished
tool interface. The exact interface-returning factories are added after the
core contract is published.
