# Contributing

Use Go 1.26.5. Keep product code standard-library-first, preserve the dependency
direction in `ARCHITECTURE.md`, and add meaningful boundary and failure tests.

Run:

```text
make fast
make check
make verify
```

`make verify` is the local merge gate. Update the security review whenever a
filesystem, process, path-resolution, cancellation, or output contract changes.
