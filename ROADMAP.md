# Roadmap

The canonical cross-repository program is maintained in
`spice-agent/docs/implementation/README.md`.

| Phase | Repository outcome | Status |
|---|---|---|
| 0 | Governance, exact pins, bounded configuration, capability disclosure, manifest, explicit autoconfiguration, and quality gates | in progress |
| 3 | Read, atomic replace/write, and shell implementations with cancellation and bounded output | blocked on tagged core tool API |
| 6 | Distribution acceptance and signed architecture-proof preview | planned |
| 7 | Optional permission decorator proving dispatcher interception | planned in a separate experimental module |

The architecture-proof line intentionally runs with the user's process
privileges. It does not claim sandboxing or approval enforcement.
