# Architecture

> An orientation map of how Atteler is put together: a Go-only LLM harness with a thin CLI/TUI shell over reusable, stateless library packages.

## Two layers (cmd vs pkg)

Atteler is organized in two layers:

- **`cmd/atteler`** — wires the [Bubble Tea TUI](getting-started.md) and the full
  CLI surface: subcommands and flags, headless mode, replay/export, hooks,
  worktree isolation, and the rest of the user-facing wiring. New CLI surfaces
  are added here, across the package's many `*.go` files, not in a sub-binary.
- **`pkg/*`** — reusable, stateless library packages. Anything reusable belongs
  here; `cmd/atteler/` should only do wiring and TUI concerns.

A second binary, `cmd/symphony`, runs the standalone issue-queue scheduler — see
[Symphony](symphony.md).

## LLM provider layer

`pkg/llm/llm.go` defines the `Provider` interface (`Name`, `Models`,
`FetchModels`, `HealthCheck`, `Complete`, `ModelContextWindow`). Built-in
providers are `anthropic`, `claude-code`, `codex`, `openai`, and `ollama`. The
Codex and Claude Code providers shell out to the user's installed CLIs
(`codex exec`, `claude --print`) so they reuse those subscriptions instead of
direct API quota, running from atteler's working directory with bounded tool
sets.

`pkg/llm/registry.go`'s `AutoRegister` / `AutoRegisterWithConfigContext` is the
canonical factory: it tries to construct every known provider and silently skips
any whose credentials are unavailable. Provider implementations stay stateless
and thread-safe — the registry creates instances on demand. See
[Providers](providers.md) for auth resolution and per-provider details.

## Configuration

YAML config is layered (XDG global → `./.atteler/` → `./.atteler.{yaml,yml}` →
`ATTELER_CONFIG` paths), with lower-precedence defaults imported from sibling
harnesses such as `~/.codex/config.toml`, `~/.claude/settings.json`, OpenCode
config, and Forge `.forge.toml`. Generation knobs layer global `generation:` →
per-agent → CLI overrides, and omitted values are not sent to providers. Full
details live in [Configuration](configuration.md).

## Sessions, events, worktrees

- **`pkg/session`** — JSON session store (default `./.atteler/sessions/`,
  override via `--session-dir` / `ATTELER_SESSION_DIR`), plus headless run
  metadata, search/export, and aggregate performance summaries.
- **`pkg/events`** — lifecycle hook dispatcher configured via `hooks:` in YAML;
  events cover the session lifecycle plus file, context, command, tool, and
  agent activity. See [Hooks](hooks.md) and
  [`--list-hook-events`](cli-reference.md).
- **`pkg/worktree`** — `--worktree` isolates a session in a git worktree under
  `./.atteler/worktrees/`; `--no-auto-merge` keeps it, `--merge-worktree
  <session-id>` merges it back.

## Other packages

| Area | Packages |
| --- | --- |
| Agents & routing | `pkg/agent`, `pkg/modelroute`, `pkg/speculate`, `pkg/subagent` |
| Memory & retrieval | `pkg/agentmemory`, `pkg/memory`, `pkg/vector`, `pkg/retrieval` |
| Code intelligence | `pkg/codegraph`, `pkg/codeintel`, `pkg/lsp`, `pkg/githistory` |
| Context | `pkg/contextpack`, `pkg/contextref` |
| Review & feedback | `pkg/review`, `pkg/watch`, `pkg/feedback` |
| Skills & completion | `pkg/skill`, `pkg/promptcomplete` |
| Extensibility | `pkg/plugin`, `pkg/mcp` |
| Evaluation | `pkg/eval` |
| Runtime helpers | `pkg/async`, `pkg/shell`, `pkg/tasklist`, `pkg/artifactmerge` |

The code-intelligence support uses Go package loading plus a lightweight Python
scanner in the shared incremental index, with optional managed LSP lookups.
Atteler does not promise a separately versioned public SDK contract.

## Streaming completion contract

`llm.StreamProvider` implementations deliver `llm.Chunk` events, and a stream
must finish with exactly one terminal event:

- **success** — `Chunk{Done: true}`, with optional usage, model, tool-call, and
  `StopReason` metadata.
- **failure** — `Chunk{Err: err}` for provider failures or context cancellation
  after the stream started.

Channel close by itself is not success: `llm.CollectStream` returns
`(*Response, error)` and reports `llm.ErrStreamIncomplete` when a channel closes
without a successful final chunk, so callers keep any partial content while
still treating the response as failed. Adapters use `llm.DefaultStreamBuffer`
(or an unbuffered channel) and select on the caller's context when sending
chunks — a slow renderer applies backpressure rather than letting tokens
accumulate. Codex and Ollama expose native streaming adapters; other providers
use `StreamFromComplete` unless they implement `StreamProvider`.

## Conventions

- **Context propagation is strict.** Only `cmd/atteler` and `cmd/symphony`
  create process-root contexts at startup; package code propagates those instead
  of calling `context.Background()`, `context.TODO()`, or
  `context.WithoutCancel()`. `golangci-lint` enforces `contextcheck` and
  `noctx`. Library APIs that touch credential stores, refresh OAuth tokens, start
  processes, or call model/embedding endpoints take a caller-provided context;
  compatibility helpers without a `Context` suffix remain only to avoid source
  breaks and return a context-required error before doing blocking work.
- **No provider singletons or global state** — use the registry/factory pattern;
  provider implementations stay stateless and thread-safe.

## Build, CI & releases

Local development uses the Makefile as the main build surface:

```sh
make build            # compile ./atteler from ./cmd/atteler
make test             # all tests with the race detector and -count=1
make e2e              # black-box CLI tests against a freshly built binary
make lint             # pinned golangci-lint
make release-check    # validate .goreleaser.yaml
make release-snapshot # local GoReleaser artifacts in dist/ (no publish)
```

Override `TESTPACKAGE` and `TESTFLAGS` for focused runs, for example
`make test TESTFLAGS='-run TestName -count=1' TESTPACKAGE=./pkg/llm`.

GitHub Actions runs CI on pull requests and branch pushes. Pushing a semantic
version tag such as `v0.1.0` triggers the release workflow and GoReleaser
packaging. When a provider changes its execution path, credentials, endpoint,
sandbox boundary, health check, or model catalog, refresh the generated
provider docs and recorded request fixtures before tagging.
