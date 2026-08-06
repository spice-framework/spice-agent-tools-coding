# Spice Agent coding tools implementation contract

This repository owns the independently versioned read, atomic replace/write,
and shell tools for Spice Agent. Work directly on local `main` in bounded
commits. Fetch before editing and immediately before pushing; never overwrite
unexpected remote work.

Go 1.26.5 is mandatory. Keep the product standard-library-first. Preserve
caller cancellation, process-tree termination, path-boundary enforcement,
bounded input/output, atomic writes, explicit capabilities, prominent bare-
privilege warnings, instance ownership, and explicit `/autoconfigure`
activation. Product packages must not import Spice compiler, command, or
internal packages.

Add positive, negative, boundary, cancellation, and failure tests as behavior
is introduced. Run `make fast` and `make check` during development, then
`make verify` on the exact commit tree. Commit and push only a green tree.
