# Spice Agent Coding Tools

Unified documentation: [spiceframework.dev/agent/tools/coding](https://spiceframework.dev/agent/tools/coding/).

`spice-agent-tools-coding` provides the opt-in read, atomic replace/write, and
shell tools for Spice Agent. It is standard-library-first, instance-owned, and
designed for generated Spice dependency injection.

Install the module and exact Spice compiler tool:

```text
go get github.com/spice-framework/spice-agent-tools-coding@<version>
go get -tool github.com/spice-framework/toolchain/cmd/spice@v0.1.0-preview.1.0.20260806203056-d0b9ac086bd6
```

Applications opt into defaults only with:

```go
import _ "github.com/spice-framework/spice-agent-tools-coding/autoconfigure"
```

Importing the root package alone never activates tools. The application owns a
typed `coding.Config`, including its absolute worktree root, byte/time bounds,
and inherited-environment allowlist. Direct composition uses exact factories:

```go
read, err := coding.NewRead(config)
replace, err := coding.NewReplace(config)
shell, err := coding.NewShell(config)
```

Each factory returns `tool.Tool` and performs no filesystem or process action.
The explicit `/autoconfigure` package contributes those same three factories as
fallback beans through generated Spice DI; there is no global registry.

- `read` uses relative paths and offset/limit paging. A complete page includes
  a SHA-256 suitable for a later bounded replace. It declares `read_only` and
  `safe` replay.
- `replace` requires either `create=true` for no-overwrite creation or the exact
  lowercase `expected_sha256` for replacement. Results distinguish committed
  state from confirmed durability. It declares `mutating` and `idempotent`:
  replay after a lost acknowledgement cannot repeat the file effect because
  create observes the existing target and replace observes the consumed digest.
  A replacement whose content already has the expected digest is an explicit
  successful no-op (`changed=false`, `committed=false`).
- `shell` accepts discrete argv and an optional relative workdir. It never
  invokes a shell, inherits only application-allowlisted environment names, and
  reports captured/observed byte counts plus deterministic truncation metadata.
  It declares `mutating` and `unsafe` replay because an arbitrary process may
  have committed effects before its outcome becomes unavailable.

Argument, path, operating-system, timeout, exit, stale-write, and durability
problems are bounded model-visible `tool.Result` values. Cancellation and host
progress/result-delivery failures return a zero result with one direct,
correlated `*tool.ExecutionError`; cancellation and deadline identity remains
available through `errors.Is`. Callers must inspect both return values and must
not infer replay safety from capabilities alone.

> **Security warning:** these tools can read and write files, execute processes,
> and use network or environment access with the operating-system user's
> privileges. They provide no sandbox or approval prompt. The shell child's
> authority is not confined to the configured worktree. Process groups and Job
> Objects provide bounded managed cleanup, but deliberately detached descendants
> may outlive the launcher; `managed_cleanup_completed` is not a containment
> guarantee.

Read and replace paths use `os.Root`. Shell workdirs reject symbolic-link
components and are revalidated before start, but same-user concurrent path
mutation remains a trust boundary. Expected hashes detect ordinary stale
writes; they are not an atomic filesystem compare-and-swap against another
process racing the final commit.

See [the dependency review](docs/dependency-review.md),
[security review](docs/security-review.md), and [support matrix](docs/support.md).

On a fresh clone, run `make tools-bootstrap` once to populate the exact product
and tools module graphs without changing tracked module files. All ordinary
quality targets remain offline; run the complete suite with `make verify`.
