# atteler

Atteler is a Go CLI for running LLM-assisted development workflows from a local
repository. It combines chat, provider routing, local context references,
session history, review/watch scans, agent planning, worktree isolation, and the
standalone Symphony issue scheduler.

The README is the stable user-facing guide. Active and aspirational work belongs
in [GitHub Issues](https://github.com/tomMoulard/atteler/issues) or linked
project items, not in checked-off markdown roadmaps.

## Documentation map

| Surface | Purpose |
| --- | --- |
| `README.md` | Product overview, quickstart, stable commands, and evidence-backed feature map. |
| [GitHub Issues](https://github.com/tomMoulard/atteler/issues) / linked project items | Active roadmap, future work, owners, and prioritization. |
| `NOTES.md` | Historical documentation notes only; it is not a feature list or roadmap. |
| [`docs/symphony.md`](docs/symphony.md) | Detailed Symphony scheduler configuration and runtime behavior. |

`TODO.md` is intentionally not kept as a local task ledger. Use GitHub Issues or
linked project items for live work so each item has an owner, discussion, and
state. If work is not in GitHub, it is not part of the active roadmap.

## Quickstart

Run from a clone:

```sh
go run ./cmd/atteler help
go run ./cmd/atteler chat once "Explain this repository"
go run ./cmd/atteler review scan
```

Install or build a binary:

```sh
go install github.com/tommoulard/atteler/cmd/atteler@latest
make build
./atteler help
```

Useful local verification commands:

```sh
make test
make lint
make build
```

## Stable command surface

Grouped command surface:

Atteler keeps top-level help short and routes discoverability through focused
domains. Run `atteler help` for the domain list, `atteler help <domain>` for one
area, and `atteler help legacy` for the compatibility flag catalog. If you know
an old flag name, `atteler help --code-summary` jumps to the focused domain that
owns it.

Domain help is rendered from structured command metadata and covered by routing tests,
so README examples stay representative instead of duplicating the whole flag
catalog.

<!-- atteler:cli-domains:start -->
| Domain | Examples |
|--------|----------|
| `chat` / `session` | `atteler chat once "Explain this repository in one paragraph"`, `atteler session list`, `atteler session search "auth retry"` |
| `config` | `atteler config paths`, `atteler config validate`, `atteler config explain default_model`, `atteler config doctor-offline` |
| `providers` | `atteler providers list`, `atteler providers known-models`, `atteler providers models` |
| `agents` | `atteler agents list`, `atteler agents plan "review auth changes"`, `atteler agents task-list` |
| `memory` / `rag` | `atteler memory search "OAuth retry storm"`, `atteler memory git-history "memory regression"`, `atteler memory vector-search "redirect risks"` |
| `code-intel` | `atteler code-intel summary`, `atteler code-intel symbol NewRegistry`, `atteler code-intel import-prefix github.com/tommoulard/atteler/pkg/` |
| `review` | `atteler review scan`, `atteler review plan`, `atteler review run` |
| `watch` | `atteler watch scan`, `atteler watch json`, `atteler watch loop` |
| `plugins` | `atteler plugins list`, `atteler plugins run reviewer/check`, `atteler plugins manifest .atteler/mcp.yaml` |
| `worktrees` | `atteler worktrees run "Add unit tests for auth"`, `atteler worktrees list`, `atteler worktrees merge 20260430-120000-deadbeef` |
| `eval` | `atteler eval output .atteler/fixtures/readme-summary.txt --eval-expected "package overview"`, `atteler eval record reviewer`, `atteler eval replay-response .atteler/fixtures/once.json "Summarize @README.md"` |
<!-- atteler:cli-domains:end -->

Common options such as `--model`, `--agent`, `--output`, generation settings,
provider routing settings, and compatibility flags can still be combined with
domain commands before or after the focused subcommand, for example
`atteler session --session <id> messages` or
`atteler chat once "Summarize" --model openai/gpt-5.4`. Prefer the grouped form
for humans and the legacy flags for existing automation until scripts are
migrated. No legacy flag is deprecated in this release; future deprecations
should add an explicit warning before removing or changing an existing
script-facing flag.

## Configuration

Atteler loads optional YAML/JSON configuration in this order; later layers
override earlier layers:

1. Best-effort imports from local coding harnesses (Codex, Claude Code,
   opencode, Forge). These are lowest precedence and are shown by
   `atteler config explain` so imported defaults are visible.
2. Global Atteler config:
   `$XDG_CONFIG_HOME/atteler/config.yaml`, `config.yml`, then `config.json`, or
   `~/.config/atteler/config.yaml`, `config.yml`, then `config.json`.
3. Project config in the current working directory:
   `./.atteler/config.yaml`, `config.yml`, `config.json`, then
   `./.atteler.yaml`, `.atteler.yml`, `.atteler.json`.
4. Any paths listed in `ATTELER_CONFIG` or `--config`, using the platform
   path-list separator. These env-provided files are highest-precedence config
   files.
5. Runtime choices after files load: persisted state, CLI flags such as
   `--model` and generation overrides, and runtime agent/model selection.

Provider and agent maps merge by name, and fields inside the same provider or
agent override independently. Lists replace the earlier value in full when set
later, including `fallback_models`, `context.references`, agent list fields, hook
lists, and `plugins.paths`. Per-agent `tools` maps also replace in full.

Bootstrap or inspect configuration:

```sh
atteler config template
atteler config init ~/.config/atteler/config.yaml
atteler config paths
atteler config validate
atteler config explain default_model
```

Use `atteler config explain` without a field prefix to print every tracked
field, or pass a prefix such as `default_model`, `providers.openai`, or
`agents.reviewer` to focus on one model, provider, or agent. Runtime diagnostic
paths such as `runtime.selected_model` and `runtime.selected_provider` explain
the selected request model/provider after state, flags, and agent selection.

Minimal example:

```yaml
default_provider: openai
default_model: gpt-4.1-mini
fallback_models: ["gpt-4.1", "gpt-4.1-nano"]

generation:
  temperature: 0
  top_p: 1
  seed: 1
  reasoning_level: medium
  max_tokens: 2048

providers:
  openai:
    base_url: https://api.openai.com
  anthropic:
    disabled: false
    base_url: https://api.anthropic.com
  ollama:
    base_url: http://127.0.0.1:11434

agents:
  reviewer:
    description: Code review specialist
    personality: concise
    capabilities: ["review", "security"]
    model: gpt-4.1-mini
    fallback_models: ["gpt-4.1-nano"]
    reasoning_level: high
    triggers: ["review this", "code review"]
    system_prompt: >
      You are a concise code reviewer. Focus on correctness, tests, and
      maintainability.
    temperature: 0
    max_tokens: 1200
```

Credentials come from environment variables, supported local harness config, or
provider-specific command-line tools. OpenAI Platform calls require
`OPENAI_API_KEY`; direct Anthropic calls require Anthropic credentials. The
`codex`, `claude-code`, and `ollama` providers use their local CLIs or daemons
when available.

## Common workflows

### One-shot and interactive chat

```sh
atteler
atteler chat once "Explain this repository in one paragraph"
git diff | atteler chat once "Review this diff" --stdin
atteler chat once "Summarize @README.md" --headless --output json
atteler session headless
atteler session stream-headless <headless-id>
```

In the interactive TUI, `Ctrl+O` opens the model picker, `Tab` accepts visible
local prompt completions (agents, slash commands, session context, and safe
model-backed suffixes when configured), `Ctrl+R` rewrites under-specified
prompts without adding boilerplate to already-structured drafts, and `Ctrl+Z`
undoes the latest rewrite.
Use `--prompt-local-only` to keep interactive prompt assistance on the
deterministic no-network completion path even when providers are configured.

For non-interactive checks, `atteler agents prompt-complete "ask @rev"` previews
the same local completion engine with source attribution, replacement ranges,
rank signals, and a short explanation of what accepting the completion inserts.

### Local file and directory context

Prompts can reference local files or directories with `@path` tokens. Atteler
keeps the visible transcript unchanged, but appends bounded reference content to
the provider request.

```sh
atteler chat once "Summarize @README.md and @pkg/llm/llm.go"
atteler chat once "Map the package layout in @pkg"
```

References are resolved relative to the current working directory, must stay
inside that directory, and are bounded by `context.max_file_bytes`,
`context.max_total_bytes`, and optional `context.max_input_tokens` settings.

### Configured references trust boundary

`context.references` and agent-level `references` are loaded automatically and
prepended to model requests. Treat them as untrusted ingestion inputs: a local
file can contain prompt-injection text, and a remote URL can become an SSRF
target if it is not constrained. Atteler escapes reference content before
placing it in `<configured_references>`, records provenance (scope, local vs
remote, size, truncation, digest, fetch time, and policy decision), and prints a
stderr line for every loaded, truncated, skipped, or rejected configured
reference. If any configured reference is rejected, that configured-reference
block is omitted instead of silently sending partial context.

By default, configured local references must stay under the current working
directory. Add explicit `local_roots` for audited outside-root reads. Remote
references support only HTTP(S) URLs and are denied unless
`reference_policy.allowed_hosts` allows the host; private, loopback, link-local,
and multicast targets remain blocked unless `allow_private_networks: true` is
set deliberately. Host wildcards such as
`*.docs.example.com` match subdomains only; list `docs.example.com` separately
if the apex host should also be trusted.

```yaml
context:
  references:
    - ./docs/style-guide.md
    - https://docs.example.com/llm-style.md
  reference_policy:
    allowed_schemes: [https]
    allowed_hosts:
      - docs.example.com
    local_roots:
      - ../shared-style-guides
    max_redirects: 0
    content_types:
      - text/*
      - application/json
    allow_private_networks: false

agents:
  reviewer:
    references:
      - ./docs/reviewer-rubric.md
```

### Deterministic response fixtures and eval checks

```sh
atteler chat once "Summarize @README.md" --record-response .atteler/fixtures/readme-summary.json
atteler chat once "Summarize @README.md" --replay-response .atteler/fixtures/readme-summary.json
atteler eval output .atteler/fixtures/readme-summary.txt \
  --eval-expected "package overview" \
  --eval-mode contains
```

Replay writes normal session messages while avoiding provider availability and
sampling noise in tests.

### Plugins and local run policy

Configured `plugins.paths` entries point at local plugin directories or manifest
files. `atteler plugins list` validates `plugin.yaml`, `plugin.yml`, or
`plugin.json` manifests with `name`, `version`, optional `description`,
`capabilities`, relative `entrypoints`, and optional security metadata.

Executable plugin entrypoints must declare their runtime contract before they
can run:

```yaml
entrypoints:
  check: bin/check
entrypoint_args:
  check: []
permissions:
  filesystem:
    read: ["."]
    write: []
  network:
    allow: false
    hosts: []
  shell:
    allow: false
  env: []
  secrets: []
  tools: []
output:
  stdout_max_bytes: 65536
  stderr_max_bytes: 65536
trust:
  enabled: true
  install_source: local
  checksum: sha256:<manifest-or-package-checksum>
  revoked: false
  audit:
    - action: accepted
      actor: local-admin
      at: "2026-05-21T00:00:00Z"
```

CLI plugin runs also require an accepted local policy in config. The policy is
an upper bound: manifests requesting anything outside it fail before execution.

```yaml
plugins:
  paths: ["./plugins/reviewer"]
  policy:
    permissions:
      filesystem:
        read: ["."]
        write: []
      network:
        allow: false
        hosts: []
      shell:
        allow: false
      env: []
      secrets: []
      tools: []
    output:
      stdout_max_bytes: 65536
      stderr_max_bytes: 65536
    trusted_install_sources: ["local"]
```

The SDK package `pkg/plugin` also exposes validated run helpers for local
workflows that want to execute a manifest entrypoint. Runs resolve paths under
the plugin root, keep the process working directory at that root, reject
undeclared/unaccepted permissions at the policy layer, require an explicit
accepted policy, require shell permission for shell-script entrypoints, pass
only allowlisted environment variables, validate declared args, and return
stdout/stderr after size limiting plus secret redaction.

Plugin entrypoints can be inspected or run with `atteler plugins list`,
`atteler plugins describe reviewer`, and
`atteler plugins run reviewer/check --plugin-dry-run`.

### Review, watch, memory, and code intelligence


```sh
atteler review scan
atteler review plan \
  --review-agent quality-reviewer \
  --review-agent test-engineer \
  --review-path pkg/llm/auth.go \
  --review-gate "tests pass"

atteler watch scan
atteler watch json
atteler watch loop --watch-interval-seconds 60 --watch-max-iterations 3

atteler memory search "OAuth retry storm"
atteler memory git-history "memory regression"
atteler memory vector-search "redirect risks" --vector-index docs/research.md

atteler code-intel summary
atteler code-intel symbol NewRegistry
atteler code-intel import-prefix github.com/tommoulard/atteler/pkg/
```

### Agents, plugins, artifacts, and worktrees

```sh
atteler agents plan "review this auth change" --plan-max-agents 3
atteler agents async-plan \
  --async-task 'plan|planner|draft plan' \
  --async-task 'code|coder|implement feature|plan'
atteler agents spawn 'planner|draft the migration plan' --spawn-dry-run

atteler plugins list
atteler plugins describe reviewer
atteler plugins run reviewer/check --plugin-dry-run

atteler session record-failure "retry token refresh timer" \
  --session 20260430-120000-deadbeef \
  --failure-reason "created retry storms" \
  --failure-commit abc123
atteler session merge-artifacts .atteler/merged-artifacts.md \
  --session 20260430-120000-deadbeef

atteler worktrees run "Add unit tests for the auth package"
atteler worktrees list
atteler worktrees merge 20260430-120000-deadbeef
```

`atteler worktrees run` creates an isolated git worktree for a session with an
ownership manifest under `.git/atteler/worktrees/`. Merge-back now runs as a
reviewed transaction: the base worktree must be clean, session changes must be
committed (or explicitly reviewed for auto-commit by an API caller), a dry-run
merge must pass, and failed merges preserve the branch/worktree with recovery
commands. See `atteler help worktrees` for the current command contract.

## Symphony

Symphony is a standalone service command, not part of the main interactive
`atteler` CLI surface:

```sh
go run ./cmd/symphony --validate
go run ./cmd/symphony ./WORKFLOW.md
make run-symphony
make build-symphony
```

It loads a repository-owned `WORKFLOW.md`, polls Linear or GitHub Issues,
creates per-issue workspaces, and runs Codex app-server turns with bounded
concurrency and retry/reconciliation logic. See
[`docs/symphony.md`](docs/symphony.md) for tracker configuration, publishing,
debug endpoints, hooks, and sandbox posture.

## Evidence-backed feature map

This section is intentionally small and evidence-linked. Add new completed
claims only when the implementation, tests, docs, or release artifact can be
linked from the row.

| Stable capability | Evidence |
| --- | --- |
| CLI command routing, grouped help, and compatibility flags | [`cmd/atteler/cli_args.go`](cmd/atteler/cli_args.go), [`cmd/atteler/cli_help_domains.go`](cmd/atteler/cli_help_domains.go), [`cmd/atteler/cli_args_test.go`](cmd/atteler/cli_args_test.go), [`cmd/atteler/cli_help_test.go`](cmd/atteler/cli_help_test.go) |
| OpenAI, Anthropic, Codex CLI, Claude Code, and Ollama providers | [`pkg/llm/openai.go`](pkg/llm/openai.go), [`pkg/llm/openai_test.go`](pkg/llm/openai_test.go), [`pkg/llm/anthropic.go`](pkg/llm/anthropic.go), [`pkg/llm/anthropic_test.go`](pkg/llm/anthropic_test.go), [`pkg/llm/codex.go`](pkg/llm/codex.go), [`pkg/llm/codex_test.go`](pkg/llm/codex_test.go), [`pkg/llm/claude_code.go`](pkg/llm/claude_code.go), [`pkg/llm/claude_code_test.go`](pkg/llm/claude_code_test.go), [`pkg/llm/ollama.go`](pkg/llm/ollama.go), [`pkg/llm/ollama_test.go`](pkg/llm/ollama_test.go) |
| Configuration loading, harness import, templates, and validation | [`pkg/config/config.go`](pkg/config/config.go), [`pkg/config/config_test.go`](pkg/config/config_test.go), [`pkg/config/harness.go`](pkg/config/harness.go), [`pkg/config/harness_test.go`](pkg/config/harness_test.go), [`pkg/config/template.go`](pkg/config/template.go), [`pkg/config/template_test.go`](pkg/config/template_test.go) |
| Sessions, transcript search/export, evaluations, failures, artifacts, and performance summaries | [`pkg/session/session.go`](pkg/session/session.go), [`pkg/session/session_test.go`](pkg/session/session_test.go), [`pkg/session/export.go`](pkg/session/export.go), [`pkg/session/export_test.go`](pkg/session/export_test.go), [`pkg/session/search.go`](pkg/session/search.go), [`pkg/session/search_test.go`](pkg/session/search_test.go), [`pkg/session/performance.go`](pkg/session/performance.go), [`pkg/session/performance_test.go`](pkg/session/performance_test.go) |
| Bounded and policy-gated context references for local files, directories, globs, and remote URLs | [`pkg/contextref/references.go`](pkg/contextref/references.go), [`pkg/contextref/references_test.go`](pkg/contextref/references_test.go), [`pkg/contextref/contextref.go`](pkg/contextref/contextref.go), [`pkg/contextref/contextref_test.go`](pkg/contextref/contextref_test.go) |
| Agent metadata, matching, orchestration planning, async waves, and sub-agent fan-out | [`pkg/agent/agent.go`](pkg/agent/agent.go), [`pkg/agent/orchestration.go`](pkg/agent/orchestration.go), [`pkg/agent/orchestration_test.go`](pkg/agent/orchestration_test.go), [`pkg/async/plan.go`](pkg/async/plan.go), [`pkg/async/plan_test.go`](pkg/async/plan_test.go), [`pkg/subagent/subagent.go`](pkg/subagent/subagent.go), [`pkg/subagent/subagent_test.go`](pkg/subagent/subagent_test.go), [`cmd/atteler/cli_async_commands.go`](cmd/atteler/cli_async_commands.go) |
| Speculative and review-agent planning/execution primitives | [`pkg/speculate/speculate.go`](pkg/speculate/speculate.go), [`pkg/speculate/speculate_test.go`](pkg/speculate/speculate_test.go), [`pkg/review/review.go`](pkg/review/review.go), [`pkg/review/review_test.go`](pkg/review/review_test.go), [`pkg/review/llm.go`](pkg/review/llm.go), [`pkg/review/llm_test.go`](pkg/review/llm_test.go), [`cmd/atteler/cli_review_async_task_commands.go`](cmd/atteler/cli_review_async_task_commands.go) |
| Memory/RAG, local vector search, git-history search, Go code intelligence, import graphs, and optional LSP lookups | [`pkg/memory/memory.go`](pkg/memory/memory.go), [`pkg/memory/memory_test.go`](pkg/memory/memory_test.go), [`pkg/vector/vector.go`](pkg/vector/vector.go), [`pkg/vector/vector_test.go`](pkg/vector/vector_test.go), [`pkg/githistory/githistory.go`](pkg/githistory/githistory.go), [`pkg/githistory/githistory_test.go`](pkg/githistory/githistory_test.go), [`pkg/codeintel/codeintel.go`](pkg/codeintel/codeintel.go), [`pkg/codeintel/codeintel_test.go`](pkg/codeintel/codeintel_test.go), [`pkg/codegraph/codegraph.go`](pkg/codegraph/codegraph.go), [`pkg/codegraph/codegraph_test.go`](pkg/codegraph/codegraph_test.go), [`pkg/lsp/client.go`](pkg/lsp/client.go), [`pkg/lsp/client_test.go`](pkg/lsp/client_test.go) |
| Plugin manifests, safe local entrypoint execution, MCP manifest validation, and stdio JSON-RPC calls | [`pkg/plugin/manifest.go`](pkg/plugin/manifest.go), [`pkg/plugin/manifest_test.go`](pkg/plugin/manifest_test.go), [`pkg/plugin/run.go`](pkg/plugin/run.go), [`pkg/plugin/run_test.go`](pkg/plugin/run_test.go), [`pkg/mcp/manifest.go`](pkg/mcp/manifest.go), [`pkg/mcp/manifest_test.go`](pkg/mcp/manifest_test.go), [`pkg/mcp/client.go`](pkg/mcp/client.go), [`pkg/mcp/client_test.go`](pkg/mcp/client_test.go), [`cmd/atteler/cli_plugin_commands.go`](cmd/atteler/cli_plugin_commands.go), [`cmd/atteler/cli_mcp_commands.go`](cmd/atteler/cli_mcp_commands.go) |
| Background repository scanning and review-scan formatting | [`pkg/watch/watch.go`](pkg/watch/watch.go), [`pkg/watch/watch_test.go`](pkg/watch/watch_test.go), [`cmd/atteler/cli_review_async_task_commands.go`](cmd/atteler/cli_review_async_task_commands.go) |
| Event hook metadata and local hook execution | [`pkg/events/events.go`](pkg/events/events.go), [`pkg/events/events_test.go`](pkg/events/events_test.go), [`pkg/events/logger.go`](pkg/events/logger.go), [`pkg/events/discoverability_test.go`](pkg/events/discoverability_test.go) |
| Automatic worktree isolation | [`pkg/worktree/worktree.go`](pkg/worktree/worktree.go), [`pkg/worktree/worktree_test.go`](pkg/worktree/worktree_test.go), [`cmd/atteler/cli_worktree_commands.go`](cmd/atteler/cli_worktree_commands.go) |
| Symphony issue scheduler | [`cmd/symphony/main.go`](cmd/symphony/main.go), [`pkg/symphony/workflow.go`](pkg/symphony/workflow.go), [`pkg/symphony/workflow_test.go`](pkg/symphony/workflow_test.go), [`pkg/symphony/orchestrator.go`](pkg/symphony/orchestrator.go), [`pkg/symphony/orchestrator_test.go`](pkg/symphony/orchestrator_test.go), [`docs/symphony.md`](docs/symphony.md), [`WORKFLOW.md`](WORKFLOW.md) |
| Build, CI, and release packaging | [`Makefile`](Makefile), [`.github/workflows/ci.yml`](.github/workflows/ci.yml), [`.github/workflows/release.yml`](.github/workflows/release.yml), [`.goreleaser.yaml`](.goreleaser.yaml) |

## Explicit non-claims

The repository has reusable Go packages, but this README does not promise a
separately versioned public SDK contract. The current code-intelligence support
uses Go parser packages and optional LSP calls; tree-sitter support would be
future work and is not documented as implemented here. The provider list is
limited to the providers linked above; additional providers should be tracked as
GitHub Issues until code and tests exist.

## Build, CI, and releases

Local development uses the Makefile as the main build surface:

- `make build` compiles `./atteler` from `./cmd/atteler`.
- `make test` runs all Go tests with the race detector.
- `make e2e` runs black-box CLI tests against a freshly built binary.
- `make lint` runs the pinned golangci-lint version.
- `make release-check` validates `.goreleaser.yaml`.
- `make release-snapshot` builds local GoReleaser artifacts in `dist/` without publishing.

GitHub Actions runs CI on pull requests and branch pushes. Pushing a semantic
version tag such as `v0.1.0` triggers the release workflow and GoReleaser
packaging.

## License

See [`LICENSE`](LICENSE).
