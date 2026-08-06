# Support and compatibility

| Contract | Support |
|---|---|
| Go | Exactly 1.26.5 for development and verification |
| Spice core | `v0.1.0-preview.1.0.20260806200749-524424a04df0` |
| Spice toolchain | `v0.1.0-preview.1.0.20260806203056-d0b9ac086bd6` |
| Spice Agent tool API | `v0.0.0-20260806225954-af79fc7fe4ad` |
| Runtime dependencies | Go standard library, Spice public metadata, Spice Agent public tool contract, and `x/sys/windows` for Job Objects |
| Tool API | Exact `NewRead`, `NewReplace`, and `NewShell` interface-returning factories; `(Result, error)` execution; fingerprinted effect/replay metadata |
| Activation | Explicit `/autoconfigure` blank import or direct constructor |
| Operating systems | Windows, Linux, and macOS |
| Privilege model | Same privileges as the user process; no sandbox or permission prompt |

Pre-1.0 releases may revise the tool adapter contract. Compatibility is
ordinary Go module selection and is recorded in `spice-compatibility.json`.
