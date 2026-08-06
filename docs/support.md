# Support and compatibility

| Contract | Support |
|---|---|
| Go | Exactly 1.26.5 for development and verification |
| Spice core | `v0.1.0-preview.1` |
| Spice toolchain | `v0.1.0-preview.1` |
| Spice Agent tool API | `v0.0.0-20260806183953-eaf19180429a` |
| Runtime dependencies | Go standard library, Spice public metadata, Spice Agent public tool contract, and `x/sys/windows` for Job Objects |
| Tool API | Exact `NewRead`, `NewReplace`, and `NewShell` interface-returning factories |
| Activation | Explicit `/autoconfigure` blank import or direct constructor |
| Operating systems | Windows, Linux, and macOS |
| Privilege model | Same privileges as the user process; no sandbox or permission prompt |

Pre-1.0 releases may revise the tool adapter contract. Compatibility is
ordinary Go module selection and is recorded in `spice-compatibility.json`.
