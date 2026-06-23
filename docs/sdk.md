# Go SDK surface

Atteler is SDK-first: the CLI and TUI are interfaces over reusable Go packages.
Starting with the `v0.1.0` SDK contract, external callers should build against
this documented surface instead of importing `cmd/atteler` internals.

## Install

```sh
go get github.com/tommoulard/atteler@latest
```

Use version tags for production integrations. For source checkouts, run the
package examples with:

```sh
go test ./pkg/sdk ./pkg/review ./examples/...
```

## Compatibility policy

The public SDK is intentionally narrower than `pkg/*` as a whole.

- **Stable packages** preserve exported source compatibility across patch
  releases and avoid breaking changes across minor releases without a documented
  deprecation window and migration note.
- Within stable packages, exported identifiers are covered by that source
  compatibility policy unless they are explicitly deprecated. The table below
  calls out the primary types and functions external integrations should prefer.
- **Experimental packages** are usable, tested library code, but their exported
  APIs may change between releases while the workflow is still settling.
- **`cmd/atteler` is not an SDK surface.** Treat it as CLI/TUI wiring only.
- Security or correctness fixes may change behavior when preserving old behavior
  would keep a bug, credential leak, or unsafe side effect.
- Before a future `v1.0.0`, compatibility is still best-effort under Go module
  `v0` semantics, but changes to stable packages should be called out in release
  notes with a migration path.

The machine-readable source of the compatibility table is
`pkg/sdk.APIContract()` and its policy text is exported as
`pkg/sdk.CompatibilityPolicy`. The JSON envelope uses `schema_version`,
`compatibility_policy`, and `packages`. Each package row uses stable snake-case
fields: `import_path`, `stability`, `since`, `summary`, and optional
`primary_identifiers` for stable package/type/function entry points.

## Release compatibility checklist

Before tagging a release that changes exported SDK code, maintainers should:

1. Run `go test ./pkg/sdk ./pkg/review ./examples/...` so package examples,
   review workflow contracts, and standalone SDK examples still compile and pass.
2. Keep `pkg/sdk.APIContract()` synchronized with any package promoted into or
   removed from the stable surface.
3. Document any stable-package breaking change in release notes with the
   deprecation window, migration path, and reason preserving old behavior is
   unsafe or impractical.
4. Avoid importing `cmd/atteler` from examples or SDK packages; reusable workflow
   behavior should move into `pkg/*` APIs first.

## Stable packages and primary types

| Package | Stable types/functions | Use for |
| --- | --- | --- |
| `pkg/sdk` | `RunOneShotChat`, `NewProviderRegistry`, `BuildMemoryIndex`, `NewReviewRun`, `RunPlugin`, `NewSession`, `AttachNewWorktree`, `APIContract`, `CompatibilityPolicy`, `Contract`, `PackageContract`, `Stability`, `PackageContracts`, `PackagesByStability` | Facade for common SDK workflows and compatibility metadata. |
| `pkg/llm` | `Provider`, `Registry`, `CompleteParams`, `Response`, `Message`, `ToolDefinition`, `ResponseFormat`, streaming and embedding contracts | Provider-agnostic model calls and provider registration. |
| `pkg/session` | `Session`, `Store`, `New`, `NewStore`, `ExportOptions`, `HeadlessRun`, `MultiAgentRun` | Durable transcripts, exports, search/list metadata, and run receipts. |
| `pkg/memory` | `Store`, `Document`, `Result`, `NewStore`, `Add`, `AddFile`, `Search`, `Save`, `Load` | Local text indexing/search for retrieval workflows. |
| `pkg/retrieval` | `Query`, `Result`, chunking helpers | Shared retrieval contracts across memory/vector callers. |
| `pkg/review` | `Reviewer`, `Plan`, `RunPlanOptions`, `NewRunPlan`, `Report`, `Finding`, `GateCheck`, `ValidateReport`, `FormatPlan`, `FormatReport` | Review plans, report contracts, gate validation, and deterministic text rendering. |
| `pkg/plugin` | `Manifest`, `PermissionSet`, `Policy`, `AcceptManifestPolicy`, `Registry`, `RunOptions`, `RunEntrypointWithOptions`, `RunResult` | Governed plugin discovery, contracts, dry runs, and bounded execution. |
| `pkg/worktree` | `Info`, `CreateContext`, `MergeWithResultContext`, `MergeOptions`, `RemoveContext`, `WithAuditContext` | Session-scoped git worktrees and reviewed merge-back transactions. |
| `pkg/permission` | `Policy`, `Request`, `Operation`, `Decision`, context helpers | Central side-effect policy gates and audit metadata. |
| `pkg/events` | `Event`, `Runner`, hook payload contracts | Lifecycle event subscriptions and hook dispatch. |

The `pkg/sdk` facade currently exports these source-compatible identifiers:
`APIContract`, `APIContractSchemaVersion`, `AttachNewWorktree`, `AttachWorktree`,
`BuildMemoryIndex`, `CompatibilityPolicy`, `Contract`, `MemoryIndexOptions`,
`NewProviderRegistry`, `NewReviewRun`, `NewSession`, `OneShotChatOptions`,
`OneShotChatResult`, `PackageContract`, `PackageContracts`,
`PackagesByStability`, `PluginRunOptions`, `ReviewRun`, `ReviewRunOptions`,
`RunOneShotChat`, `RunPlugin`, `SaveSession`, `SearchMemory`, `SessionOptions`,
`Stability`, `StabilityExperimental`, `StabilityStable`, and `ValidateReport`.

## Experimental packages

These packages are useful for advanced integrations but are not yet covered by
the stable compatibility promise: `pkg/agent`, `pkg/agentmemory`,
`pkg/artifactmerge`, `pkg/async`, `pkg/autonomy`, `pkg/autopilot`,
`pkg/codegraph`, `pkg/codeintel`, `pkg/config`, `pkg/contextpack`,
`pkg/contextref`, `pkg/eval`, `pkg/feedback`, `pkg/githistory`,
`pkg/incident`, `pkg/lsp`, `pkg/mcp`, `pkg/modelroute`, `pkg/privacy`,
`pkg/promptcomplete`, `pkg/research`, `pkg/shell`, `pkg/skill`,
`pkg/sourcepolicy`, `pkg/speculate`, `pkg/subagent`, `pkg/symphony`,
`pkg/tasklist`, `pkg/vector`, and `pkg/watch`.

If an integration needs one of these to become stable, open an issue describing
which exported types need a compatibility contract and which behavior should be
covered by examples/tests.

## Runnable examples

The `examples/` directory contains credential-free examples for the common SDK
workflows requested by external callers:

| Workflow | Directory |
| --- | --- |
| One-shot chat | `examples/one-shot-chat` |
| Provider registry | `examples/provider-registry` |
| Review run | `examples/review-run` |
| Memory indexing/search | `examples/memory-search` |
| Plugin execution | `examples/plugin-execution` |
| Worktree/session usage | `examples/worktree-session` |

Each directory is a small `main` package and is compiled by `go test
./examples/...`. The package-level examples in `pkg/sdk` and `pkg/review` also
run under `go test ./pkg/sdk ./pkg/review` and avoid live provider credentials
by using local fake providers or deterministic review data.

## Minimal one-shot chat

```go
registry, err := sdk.NewProviderRegistry(myProvider)
if err != nil {
    return err
}

result, err := sdk.RunOneShotChat(ctx, sdk.OneShotChatOptions{
    Registry: registry,
    Model:    "my-model",
    Prompt:   "Summarize this repository",
})
if err != nil {
    return err
}

fmt.Println(result.Response.Content)
```

For lower-level control, build `llm.CompleteParams` directly and call
`(*llm.Registry).Complete` or `CompleteWithFallback`.
