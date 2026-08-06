# Spice Agent Coding Tools

`spice-agent-tools-coding` provides the opt-in read, atomic replace/write, and
shell tools for Spice Agent. It is standard-library-first, instance-owned, and
designed for generated Spice dependency injection.

Install the module and exact Spice compiler tool:

```text
go get github.com/spice-framework/spice-agent-tools-coding@<version>
go get -tool github.com/spice-framework/toolchain/cmd/spice@v0.1.0-preview.1
```

Applications opt into defaults only with:

```go
import _ "github.com/spice-framework/spice-agent-tools-coding/autoconfigure"
```

Importing the root package alone never activates tools. The application owns a
typed `coding.Config`, including its absolute worktree root and explicit bounds.

> **Security warning:** these tools run with the user's process privileges.
> Phase 0 provides no sandbox and no permission prompt.

Phase 0 exposes a concrete capability suite. Exact `tool.Tool` bindings are
added only after the core API is tagged; no temporary local interface or
runtime registry is introduced.

See [the dependency review](docs/dependency-review.md),
[security review](docs/security-review.md), and [support matrix](docs/support.md).
