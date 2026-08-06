# Support and compatibility

| Contract | Phase 0 support |
|---|---|
| Go | Exactly 1.26.5 for development and verification |
| Spice core | `v0.1.0-preview.1` |
| Spice toolchain | `v0.1.0-preview.1` |
| Runtime dependencies | Go standard library only, plus Spice public metadata contracts |
| Tool API | Concrete bounded suite; provider-neutral tool adapters deferred until the tagged core API |
| Activation | Explicit `/autoconfigure` blank import or direct constructor |
| Operating systems | Windows, Linux, and macOS |
| Privilege model | Same privileges as the user process; no sandbox or permission prompt |

Pre-1.0 releases may revise the tool adapter contract. Compatibility is
ordinary Go module selection and is recorded in `spice-compatibility.json`.
