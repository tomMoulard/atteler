# atteler

**Drive every LLM you already pay for from one Go binary — in your terminal, in
your repo, on your terms.**

Atteler is a Go-only LLM harness. It puts a fast Bubble Tea TUI and a large,
scriptable CLI in front of Anthropic, OpenAI, Codex, Claude Code, and Ollama,
then layers on the things that make an assistant actually useful inside a
codebase: durable sessions, agent personas and routing, lifecycle hooks,
git-worktree isolation, retrieval over your code and history, and auditable
multi-agent review/speculation runs.

No service to host, no SDK to vendor — clone it, point it at a model, and go.

> 📚 **Documentation**: human guide at **<https://tommoulard.github.io/atteler/main/>**
> with release snapshots under versioned URLs such as
> **<https://tommoulard.github.io/atteler/v0.0.7/>** (sources live under
> [`docs/`](docs/)).
>
> Active and aspirational work lives in
> [GitHub Issues](https://github.com/tomMoulard/atteler/issues), not in
> checked-off markdown roadmaps. `NOTES.md` is historical notes only.

### Give an LLM the docs

Paste this into any assistant (Claude, ChatGPT, etc.) to make it answer from
atteler's full documentation:

```text
Use https://tommoulard.github.io/atteler/main/llms-full.txt as your reference for
atteler, a Go LLM harness with a TUI/CLI over multiple providers. Fetch that URL
and answer my questions about installing, configuring, and using it based on its
contents.
```

## Why atteler

- **One interface, five backends.** `anthropic`, `openai`, `codex`,
  `claude-code`, and `ollama` behind a single `Provider` contract, plus
  OpenAI-compatible endpoints for everything else. The registry constructs each
  on demand and silently skips any you have no credentials for.
- **Evidence-backed routing.** Pick a model by *role* and budget, not by
  hardcoding a name; the router scores candidates against capabilities, cost,
  and latency and records why it chose what it chose.
- **It stays out of your repo's way.** `--worktree` isolates a run in its own
  git worktree; sessions, artifacts, and multi-agent run audits are written
  under `.atteler/` and are fully replayable without spending a token.
- **Built to be inspected.** Most discovery commands are offline and
  credential-free — perfect for CI, dotfiles, and curious humans.

## Quickstart

Run straight from a clone:

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
make test TESTPACKAGE=./pkg/llm
make test TESTFLAGS='-run TestName -count=1' TESTPACKAGE=./pkg/llm
make lint
make build
```

Live provider e2e checks are opt-in; set ATTELER_E2E_LIVE=1 to run live LLM e2e tests.

| Test | Required credential/config | Skip message | Model override | Default model |
|------|----------------------------|--------------|----------------|---------------|
| `TestLiveOpenAIOneShot` | `OPENAI_API_KEY` | `OPENAI_API_KEY is required for this live e2e test` | `ATTELER_E2E_OPENAI_MODEL` | `gpt-4.1-mini` |
| `TestLiveAnthropicOneShot` | `ANTHROPIC_API_KEY` | `ANTHROPIC_API_KEY is required for this live e2e test` | `ATTELER_E2E_ANTHROPIC_MODEL` | `claude-haiku-4-5-20251001` |
| `TestLiveForgeClaudeOneShot` | `ATTELER_E2E_FORGE_CONFIG` | `ATTELER_E2E_FORGE_CONFIG is required for this live ForgeCode test`; `ForgeCode credentials not available in <dir>` | `ATTELER_E2E_FORGE_ANTHROPIC_MODEL` | `claude-haiku-4-5-20251001` |
| `TestLiveClaudeCodeOneShot` | Claude Code login | `Claude Code login is required for this live test` | `ATTELER_E2E_CLAUDE_CODE_MODEL` | `claude-haiku-4-5-20251001` |
| `TestLiveCodexOneShot` | Codex `auth.json` | `Codex credentials not available in <dir>` | `ATTELER_E2E_CODEX_MODEL` | `gpt-5.5` |

Agent-loop budgets are optional; zero means no cap for that dimension:

```yaml
agent_loop:
  max_output_bytes: 0
  max_cost_micros: 0
  max_input_tokens: 0
  max_output_tokens: 0
  max_total_tokens: 0
  max_iterations: 0
  max_model_calls: 0
  max_tool_calls: 0
  max_wall_time: ""
  checkpoint_interval: 0
```

New here? Start with **[Getting started](docs/getting-started.md)**,
**[Common workflows](docs/common-workflows.md)**, and
**[Configuration](docs/configuration.md)**.

## Stable command surface

Grouped command surface:

Atteler keeps top-level help short and routes discoverability through focused
domains. Run `atteler help` for the domain list, `atteler help <domain>` for one
area, and `atteler help legacy` for the compatibility flag catalog. If you know
an old flag name, `atteler help --code-summary` jumps to the focused domain that
owns it.

Domain help is rendered from structured command metadata and covered by routing tests,
so README examples stay representative instead of duplicating the whole flag
catalog. The same contract can be dumped for documentation, shell completion,
and tests with `atteler config commands-json`; Markdown docs can be rendered
from that dispatch contract with `atteler config commands-docs`. Code-intel
query docs include generated text formats, JSON fields, and concrete examples
from the same descriptors that route the commands.

<!-- atteler:cli-domains:start -->
| Domain | Examples |
|--------|----------|
| `chat` / `session` | `atteler chat once "Explain this repository in one paragraph"`, `atteler session list`, `atteler session search "auth retry"` |
| `config` | `atteler config paths`, `atteler config validate`, `atteler config migrate`, `atteler config report` |
| `providers` | `atteler providers list`, `atteler providers known-models`, `atteler providers models`, `atteler providers resolve gpt-5.5`, `atteler providers ollama-status`, `atteler providers ollama-stop` |
| `agents` | `atteler agents list`, `atteler agents plan "review auth changes"`, `atteler agents task-list` |
| `research` | `atteler research run "Compare approaches for plugin sandboxing in Go CLIs"`, `atteler research run --trusted-source go.dev --trusted-source github.com --deny-source example-content-farm.com --warn-low-trust "Research best practices for safe agent worktrees"`, `atteler research run --output .atteler/research/plugin-sandboxing --generate-tasks "Find viable implementation approaches for sandboxing Atteler plugins"` |
| `autoresearch` | `atteler autoresearch run "Improve agent-loop recovery; keep only changes that pass make test"`, `atteler autoresearch "Reduce prompt-context cache misses and validate with go test ./cmd/atteler"`, `atteler session headless` |
| `issue` | `atteler issue implement GH-218 --open-pr`, `atteler issue implement https://github.com/owner/repo/issues/218 --open-pr`, `atteler issue implement GH-218 --open-pr --base main --run-tests --run-lint` |
| `memory` / `retrieval` | `atteler memory search "OAuth retry storm"`, `atteler memory retrieve "OAuth retry storm"`, `atteler memory retrieve "OAuth retry storm" --retrieval-source vector`, `atteler memory git-history "memory regression"`, `atteler memory vector-search "redirect risks"`, `atteler memory vector-index docs/research.md` |
| `code-intel` | `atteler code-intel summary`, `atteler code-intel summary --json`, `atteler code-intel query definitions:Run`, `atteler code-intel symbol NewRegistry`, `atteler code-intel import-prefix github.com/tommoulard/atteler/pkg/` |
| `incident` | `atteler incident diagnose --sentry ISSUE-912`, `atteler incident diagnose --incident-file redacted-sentry-event.json`, `atteler incident diagnose --sentry ISSUE-912 --incident-apply-fix --incident-validation-command "go test ./pkg/auth"` |
| `review` | `atteler review scan`, `atteler review plan`, `atteler review run` |
| `watch` | `atteler watch scan`, `atteler watch json`, `atteler watch loop` |
| `plugins` | `atteler plugins list`, `atteler plugins run reviewer/check`, `atteler plugins manifest .atteler/mcp.yaml` |
| `worktrees` | `atteler worktrees run "Add unit tests for auth"`, `atteler worktrees list`, `atteler worktrees merge 20260430-120000-deadbeef` |
| `eval` | `atteler eval output .atteler/fixtures/readme-summary.txt --eval-expected "package overview"`, `atteler eval run .atteler/evals/readme.eval.yaml`, `atteler eval fixtures .atteler/evals --eval-report .atteler/eval-report.json`, `atteler eval record reviewer`, `atteler eval replay-response .atteler/fixtures/once.json "Summarize @README.md"` |
<!-- atteler:cli-domains:end -->

Use `atteler providers resolve <model>` when routing is unclear. It prints the
selected provider/model when resolution is safe, lists every provider claim
considered for ambiguous bare names, and includes provenance/stale-catalog
markers so static fallbacks are not mistaken for fresh live catalogs.

Common options for model, agent, output, generation settings, provider routing
settings, and compatibility flags can still be combined with domain commands
before or after the focused subcommand, for example
`atteler session --session <id> messages` or
`atteler chat once "Summarize" --model openai/gpt-5.4`. Prefer the grouped form
for humans and the legacy flags for existing automation until scripts are
migrated. No legacy flag is deprecated in this release; future deprecations
should add an explicit warning before removing or changing an existing
script-facing flag.

Code-intelligence commands accept `--json` (or `--output json`) to emit the
stable `atteler.code_intel.v1` schema; text output is rendered from the same
typed response contract. Add `--code-limit` and `--code-offset` to paginate
list-style code-intel results; JSON output includes pagination metadata when
those flags are set. The shared index persists under
`.atteler/codeintel-index.json`, invalidates changed or deleted source files,
and currently indexes Go plus a lightweight Python fixture/scanner path.

The full, always-current command and flag catalog lives in the
**[CLI reference](docs/cli-reference.md)**, generated from the same dispatch
contract that routes the commands.

## Providers

The five built-in providers sit behind one stateless `Provider` interface
(`pkg/llm`). The `codex` and `claude-code` providers reuse those subscriptions
by borrowing their stored credentials, but make **direct HTTPS calls** from
atteler — they do not run the vendor CLIs. Full credential, endpoint, and
trust-boundary details are on the **[Providers](docs/providers.md)** page.

### Provider runtime contracts

This block is generated from `llm.ProviderRuntime` metadata and verified by
`go test ./pkg/llm`. Update it after any change to a provider's execution path,
credential source, token refresh behavior, endpoint, sandbox/tool boundary,
health check, or model inventory with
`UPDATE_PROVIDER_RUNTIME_DOCS=1 go test ./pkg/llm -run TestProviderRuntimeDocs_ReadmeSectionMatchesMetadata`.

<!-- BEGIN GENERATED PROVIDER RUNTIME DOCS -->
#### `anthropic`

- Execution path: Direct HTTPS calls from atteler to the Anthropic Messages API.
- Credential source: `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`, `CLAUDE_CODE_OAUTH_TOKEN`, ForgeCode credential files (`$FORGE_CONFIG/.credentials.json`, `~/forge/.credentials.json`, `~/.forge/.credentials.json`), then Claude Code keychain or `~/.claude/.credentials.json`.
- Token refresh: ForgeCode OAuth credentials may refresh during credential resolution; the Anthropic adapter itself does not refresh on 401.
- Network endpoint: `ANTHROPIC_BASE_URL` or provider config, default `https://api.anthropic.com`; `POST /v1/messages` for completions and `GET /v1/models` for model/health checks.
- Sandbox and tools: No subprocess or workspace sandbox. Atteler sends tool definitions in the Messages request; any tool execution happens in Atteler's agent loop.
- Model inventory: Known-model listing prints the static `Models()` fallback without credentials; registered providers can fetch live models with `GET /v1/models`.
- Health check: Network check: calls `GET /v1/models` through `FetchModels`.

#### `claude-code`

- Execution path: Direct HTTPS calls from atteler to the Anthropic Messages API using Claude Code OAuth; it does not run the Claude Code CLI in print mode.
- Credential source: Claude Code OAuth from macOS Keychain `Claude Code-credentials` or `~/.claude/.credentials.json`.
- Token refresh: On 401, exchanges the stored refresh token at `https://platform.claude.com/v1/oauth/token` and persists refreshed tokens back to the same Claude Code credential store.
- Network endpoint: `ANTHROPIC_BASE_URL`, default `https://api.anthropic.com`; `POST /v1/messages` for completions. Model listing is static for this provider.
- Sandbox and tools: No Claude Code subprocess, file/search/edit tool sandbox, or workspace sandbox. Atteler only forwards configured request tools.
- Model inventory: Known-model listing and `FetchModels` both return the static Claude Code model/alias catalog; no model-list network call is made.
- Health check: Local credential check only: verifies an OAuth access token is loaded; no network call.

#### `codex`

- Execution path: Direct HTTPS Responses request from atteler to the ChatGPT Codex backend; it does not run `codex exec`.
- Credential source: `$CODEX_HOME/auth.json` or `~/.codex/auth.json` in `auth_mode=chatgpt` with ChatGPT access and refresh tokens.
- Token refresh: On 401, exchanges the stored refresh token at `https://auth.openai.com/oauth/token` and atomically updates `auth.json`.
- Network endpoint: `CODEX_BASE_URL`, default `https://chatgpt.com/backend-api/codex`; `POST /responses` for completions. Model listing is static plus any model from Codex config.
- Sandbox and tools: No Codex subprocess, file/search/edit tool sandbox, or workspace sandbox. Atteler sends Responses API function-tool definitions only.
- Model inventory: Known-model listing prints the static Codex catalog; registered providers prepend any model configured in Codex config and `FetchModels` stays local.
- Health check: Local credential check only: verifies parsed ChatGPT-mode auth has an access token; no network call.

#### `ollama`

- Execution path: HTTP calls to a local or configured Ollama daemon; when auto-start is enabled for a local base URL, atteler may start `ollama serve`.
- Credential source: No API credential is used by the built-in adapter.
- Token refresh: None.
- Network endpoint: `OLLAMA_BASE_URL` or provider config, default `http://127.0.0.1:11434`; `POST /api/chat` for completions and `GET /api/tags` for model/health checks.
- Sandbox and tools: No workspace sandbox. Local model execution and any model tool behavior are governed by the Ollama daemon; Atteler serializes configured tool definitions.
- Model inventory: Known-model listing prints useful static defaults without contacting Ollama; registered providers call `GET /api/tags` for live local model names.
- Health check: Network/local daemon check: calls `GET /api/tags` and may first auto-start `ollama serve` during provider creation.

#### `openai`

- Execution path: Direct HTTPS calls from atteler to the OpenAI Chat Completions API.
- Credential source: `OPENAI_API_KEY`, then the `OPENAI_API_KEY` field in `~/.codex/auth.json`.
- Token refresh: None; the API key is sent as a bearer token and is not refreshed.
- Network endpoint: `OPENAI_BASE_URL` or provider config, default `https://api.openai.com`; `POST /v1/chat/completions` for completions and `GET /v1/models` for model/health checks.
- Sandbox and tools: No subprocess or workspace sandbox. Atteler sends function-tool definitions in the chat request; any tool execution happens in Atteler's agent loop.
- Model inventory: Known-model listing prints the static `Models()` fallback without credentials; registered providers can fetch live models with `GET /v1/models`.
- Health check: Network check: calls `GET /v1/models` through `FetchModels`.
<!-- END GENERATED PROVIDER RUNTIME DOCS -->

### Provider compatibility matrix

The compatibility matrix below is generated from `llm.ProviderCompatibilityMatrix`
and checked by `go test ./pkg/llm`. It is credential-free and should match what
`atteler config doctor` and `atteler config doctor-offline` summarize under
`compatibility_matrix`. Refresh it with
`UPDATE_PROVIDER_COMPATIBILITY_DOCS=1 go test ./pkg/llm -run TestProviderCompatibilityDocs_ReadmeSectionMatchesMetadata`.

<!-- BEGIN GENERATED PROVIDER COMPATIBILITY MATRIX -->
| Dimension | `anthropic` | `claude-code` | `codex` | `ollama` | `openai` |
| --- | --- | --- | --- | --- | --- |
| `auth_source` | api-key/oauth — `ANTHROPIC_API_KEY`/`ANTHROPIC_AUTH_TOKEN`, ForgeCode credentials, or borrowed Claude Code credentials | borrowed-oauth — Claude Code OAuth from macOS Keychain or `~/.claude/.credentials.json` | borrowed-chatgpt — `$CODEX_HOME/auth.json` or `~/.codex/auth.json` in ChatGPT auth mode | none — no API credential is used by the built-in adapter | api-key — `OPENAI_API_KEY` or the `OPENAI_API_KEY` field in Codex `auth.json` |
| `model_discovery` | live+static — `GET /v1/models` when registered; static fallback for offline known-models | static — static Claude Code adapter catalog; no model-list network call | static+config — static Codex catalog plus configured Codex model; no backend model-list endpoint | local-live+static — `GET /api/tags` against the configured daemon; static fallback for offline known-models | live+static — `GET /v1/models` when registered; static fallback for offline known-models |
| `completion` | messages-api — `POST /v1/messages` direct HTTPS | messages-api — `POST /v1/messages` direct HTTPS using Claude Code OAuth | responses-api — `POST /responses` direct HTTPS to the ChatGPT Codex backend | ollama-chat — `POST /api/chat` against a local or configured Ollama daemon | chat-completions — `POST /v1/chat/completions` direct HTTPS |
| `streaming` | unsupported — no caller-facing streaming provider | unsupported — no caller-facing streaming provider | supported — llm.StreamProvider implementation | supported — llm.StreamProvider implementation | unsupported — no caller-facing streaming provider |
| `tool_use` | supported — maps to tools input_schema | supported — maps to tools input_schema | supported — maps to Responses function tools | supported — maps to function tools | supported — maps to function tools |
| `shell_access` | none — no subprocess, CLI, or workspace shell/tool sandbox | none — does not run the Claude Code CLI or expose its file/search/edit tools | none — does not run `codex exec` or expose Codex CLI workspace tools | daemon-autostart — no shell/tool access; may start `ollama serve` only when auto-start is explicitly enabled | none — no subprocess, CLI, or workspace shell/tool sandbox |
| `reasoning_effort` | lossy — maps Atteler levels to Anthropic thinking token budgets | lossy — maps Atteler levels to Anthropic thinking token budgets | supported — maps to responses reasoning.effort | lossy — maps Atteler levels to Ollama think false/low/medium/high | supported — maps to reasoning_effort |
| `seed` | unsupported — Anthropic Messages has no seed parameter | unsupported — Claude Code OAuth path uses Anthropic Messages, which has no seed parameter | unsupported — Codex ChatGPT responses endpoint does not expose seed in this adapter | supported — maps to options.seed | supported — maps to chat.completions seed |
| `temperature_top_p` | supported — temperature and top_p are sent directly unless provider-specific constraints apply | supported — temperature and top_p are sent directly unless provider-specific constraints apply | partial — temperature=omitted (Codex ChatGPT responses endpoint does not expose temperature in this adapter); top_p=unsupported (Codex ChatGPT responses endpoint does not expose top_p in this adapter) | supported — temperature and top_p are sent directly unless provider-specific constraints apply | supported — temperature and top_p are sent directly unless provider-specific constraints apply |
| `max_tokens` | supported — maps to max_tokens; defaults to 4096 when unset | supported — maps to max_tokens; defaults to 4096 when unset | omitted — Codex ChatGPT responses endpoint does not expose max output tokens in this adapter | supported — maps to options.num_predict | supported — maps to max_tokens |
| `context_window` | catalog+heuristic — versioned catalog metadata, with Anthropic-name fallback for newer Claude IDs | catalog+static-aliases — built-in catalog metadata for known Claude IDs; static context-window assumptions for Claude Code aliases | catalog+static+unknown-overrides — built-in catalog metadata for known IDs; static adapter metadata for Codex-only IDs; configured overrides intentionally return unknown context | static+unknown — static defaults for common local model families; unknown for unrecognized local tags | catalog+heuristic — versioned catalog metadata, with heuristic fallback for legacy OpenAI IDs |
| `token_usage` | usage+cache-read-write — input, output, cache-read, and cache-write token counts | usage+cache-read-write — input, output, cache-read, and cache-write token counts from Anthropic responses | usage+cache-read — input, output, and cached-input token counts from Responses events | usage-no-cache — prompt/eval token counts; no cached-token accounting | usage+cache-read — prompt, completion, and cached-input token counts |
| `retry_behavior` | registry — registry retries transient 429/5xx responses; adapter does not refresh on 401 | registry+oauth-refresh — registry retries transient 429/5xx responses; adapter refreshes OAuth once after 401 | registry+oauth-refresh — registry retries transient 429/5xx responses; adapter refreshes ChatGPT OAuth once after 401 | registry — registry retries transient 429/5xx responses from the daemon | registry — registry retries transient 429/5xx responses; API keys are not refreshed |
| `offline_mode` | metadata-only — known providers/models/matrix work offline; completion and health require network credentials | local-auth+metadata — static catalog and local credential checks work offline; completion requires network | local-auth+metadata — static catalog and local credential checks work offline; completion requires network | local-daemon — matrix and static known-models work offline; local completion needs a reachable daemon/model | metadata-only — known providers/models/matrix work offline; completion and health require network credentials |

#### Model context and output limits

The max-output column is catalog metadata about model limits; request-level `CompleteParams.MaxTokens` support is the separate `max_tokens` compatibility row above.

| Provider | Model | Context window | Max output tokens | Provenance |
| --- | --- | ---: | ---: | --- |
| `anthropic` | `claude-haiku-4-5-20251001` | 200000 | 64000 | builtin_catalog — Anthropic Claude pricing |
| `anthropic` | `claude-opus-4-20250514` | 200000 | 32000 | builtin_catalog — Anthropic Claude pricing |
| `anthropic` | `claude-opus-4-5-20251101` | 200000 | 64000 | builtin_catalog — Anthropic Claude pricing |
| `anthropic` | `claude-opus-4-6` | 1000000 | 128000 | builtin_catalog — Anthropic Claude pricing |
| `anthropic` | `claude-opus-4-7` | 1000000 | 128000 | builtin_catalog — Anthropic Claude pricing |
| `anthropic` | `claude-sonnet-4-20250514` | 200000 | 64000 | builtin_catalog — Anthropic Claude pricing |
| `anthropic` | `claude-sonnet-4-5-20250929` | 200000 | 64000 | builtin_catalog — Anthropic Claude pricing |
| `anthropic` | `claude-sonnet-4-6` | 1000000 | 64000 | builtin_catalog — Anthropic Claude pricing |
| `claude-code` | `claude-haiku-4-5` | 200000 | 64000 | builtin_catalog — Anthropic Claude pricing |
| `claude-code` | `claude-haiku-4-5-20251001` | 200000 | 64000 | builtin_catalog — Anthropic Claude pricing |
| `claude-code` | `claude-opus-4-1-20250805` | 200000 | 64000 | builtin_catalog — Anthropic Claude pricing |
| `claude-code` | `claude-opus-4-20250514` | 200000 | 32000 | builtin_catalog — Anthropic Claude pricing |
| `claude-code` | `claude-opus-4-5-20251101` | 200000 | 64000 | builtin_catalog — Anthropic Claude pricing |
| `claude-code` | `claude-opus-4-6` | 1000000 | 128000 | builtin_catalog — Anthropic Claude pricing |
| `claude-code` | `claude-opus-4-7` | 1000000 | 128000 | builtin_catalog — Anthropic Claude pricing |
| `claude-code` | `claude-sonnet-4-20250514` | 200000 | 64000 | builtin_catalog — Anthropic Claude pricing |
| `claude-code` | `claude-sonnet-4-5-20250929` | 200000 | 64000 | builtin_catalog — Anthropic Claude pricing |
| `claude-code` | `claude-sonnet-4-6` | 1000000 | 64000 | builtin_catalog — Anthropic Claude pricing |
| `claude-code` | `haiku` | 200000 | unknown | adapter_static — static Claude Code CLI alias reviewed against the local provider contract; not network verified |
| `claude-code` | `opus` | 200000 | unknown | adapter_static — static Claude Code CLI alias reviewed against the local provider contract; not network verified |
| `claude-code` | `sonnet` | 200000 | unknown | adapter_static — static Claude Code CLI alias reviewed against the local provider contract; not network verified |
| `codex` | `gpt-5.3-codex` | 200000 | 128000 | builtin_catalog — OpenAI API pricing and priority processing (Codex reference) |
| `codex` | `gpt-5.3-codex-spark` | 200000 | unknown | adapter_static — static Codex adapter catalog reviewed against the local provider contract; not network verified |
| `codex` | `gpt-5.4` | 1050000 | 128000 | builtin_catalog — OpenAI API pricing and priority processing (Codex reference) |
| `codex` | `gpt-5.4-mini` | 400000 | 128000 | builtin_catalog — OpenAI API pricing and priority processing (Codex reference) |
| `codex` | `gpt-5.5` | 1050000 | 128000 | builtin_catalog — OpenAI API pricing and priority processing (Codex reference) |
| `ollama` | `deepseek-r1` | 128000 | unknown | builtin_catalog — local Ollama catalog |
| `ollama` | `gemma3` | 128000 | unknown | builtin_catalog — local Ollama catalog |
| `ollama` | `llama3.1` | 128000 | unknown | builtin_catalog — local Ollama catalog |
| `ollama` | `llama3.2` | 128000 | unknown | builtin_catalog — local Ollama catalog |
| `ollama` | `mistral` | 128000 | unknown | builtin_catalog — local Ollama catalog |
| `ollama` | `nomic-embed-text` | 8192 | unknown | builtin_catalog — local Ollama catalog |
| `ollama` | `qwen2.5` | 128000 | unknown | builtin_catalog — local Ollama catalog |
| `openai` | `gpt-4.1` | 1047576 | 32768 | builtin_catalog — OpenAI API model comparison, pricing, and priority processing |
| `openai` | `gpt-4.1-mini` | 1047576 | 32768 | builtin_catalog — OpenAI API model comparison, pricing, and priority processing |
| `openai` | `gpt-4.1-nano` | 1047576 | 32768 | builtin_catalog — OpenAI API model comparison, pricing, and priority processing |
| `openai` | `gpt-4o` | 128000 | 16384 | builtin_catalog — OpenAI API model comparison, pricing, and priority processing |
| `openai` | `gpt-4o-mini` | 128000 | 16384 | builtin_catalog — OpenAI API model comparison, pricing, and priority processing |
| `openai` | `gpt-5` | 400000 | 128000 | builtin_catalog — OpenAI API model comparison, pricing, and priority processing |
| `openai` | `gpt-5-codex` | 400000 | 128000 | builtin_catalog — OpenAI API model comparison, pricing, and priority processing |
| `openai` | `gpt-5-mini` | 400000 | 128000 | builtin_catalog — OpenAI API model comparison, pricing, and priority processing |
| `openai` | `gpt-5.1` | 400000 | 128000 | builtin_catalog — OpenAI API model comparison, pricing, and priority processing |
| `openai` | `gpt-5.1-codex` | 400000 | 128000 | builtin_catalog — OpenAI API model comparison, pricing, and priority processing |
| `openai` | `gpt-5.2` | 400000 | 128000 | builtin_catalog — OpenAI API model comparison, pricing, and priority processing |
| `openai` | `gpt-5.3-codex` | 400000 | 128000 | builtin_catalog — OpenAI API model comparison, pricing, and priority processing |
| `openai` | `gpt-5.4` | 1050000 | 128000 | builtin_catalog — OpenAI API model comparison, pricing, and priority processing |
| `openai` | `gpt-5.4-mini` | 400000 | 128000 | builtin_catalog — OpenAI API model comparison, pricing, and priority processing |
| `openai` | `gpt-5.4-nano` | 400000 | 128000 | builtin_catalog — OpenAI API model comparison, pricing, and priority processing |
| `openai` | `gpt-5.5` | 1050000 | 128000 | builtin_catalog — OpenAI API model comparison, pricing, and priority processing |
| `openai` | `o3` | 200000 | 100000 | builtin_catalog — OpenAI API model comparison, pricing, and priority processing |
| `openai` | `o4-mini` | 200000 | 100000 | builtin_catalog — OpenAI API model comparison, pricing, and priority processing |
| `openai` | `text-embedding-3-large` | 8192 | unknown | builtin_catalog — OpenAI API embedding model pricing |
| `openai` | `text-embedding-3-small` | 8192 | unknown | builtin_catalog — OpenAI API embedding model pricing |
<!-- END GENERATED PROVIDER COMPATIBILITY MATRIX -->

## Documentation

The full guide is published at **<https://tommoulard.github.io/atteler/main/>**,
with immutable release snapshots under URLs such as
**<https://tommoulard.github.io/atteler/v0.0.7/>**. The source pages are mirrored
in [`docs/`](docs/):

Existing unversioned page URLs redirect to `main` for compatibility; use the
versioned URLs when linking to behavior for a specific Atteler release.

| Page | What's inside |
| --- | --- |
| [Getting started](docs/getting-started.md) | Build, run, and your first prompt. |
| [Common workflows](docs/common-workflows.md) | Task recipes: worktrees, headless, replay, routing, multi-agent. |
| [Configuration](docs/configuration.md) | Layered YAML/JSON config, generation knobs, the routing/agent schema. |
| [Providers](docs/providers.md) | The built-in providers and how auth resolves. |
| [Hooks](docs/hooks.md) | Lifecycle events you can subscribe to. |
| [CLI reference](docs/cli-reference.md) | The complete, generated command surface. |
| [Architecture](docs/architecture.md) | How the codebase fits together. |
| [Symphony](docs/symphony.md) | The issue scheduler and one-shot issue-to-PR publishing. |
| [Lifecycle events](docs/lifecycle-events.md) | Generated hook payload schemas and examples. |

LLM consumers should read the version-specific `llms-full.txt`, for example
<https://tommoulard.github.io/atteler/main/llms-full.txt> for current `main`, or
[`llms-full.txt`](llms-full.txt) locally — the whole guide flattened into one
file (see [Give an LLM the docs](#give-an-llm-the-docs)).

## What's proven

Atteler's stable capabilities are tracked in the codebase with linked
implementation and tests. Highlights — see
[Architecture](docs/architecture.md) for the full map:

- CLI command routing, grouped help, and compatibility flags
- Error-aware streaming completion contract with bounded-buffer guidance
- Six provider adapters (incl. OpenAI-compatible endpoints), typed provider
  errors, jittered retry budgets, and retry lifecycle events
- Evidence-backed model routing with catalog metadata, model roles, per-agent
  policy, and usage telemetry
- Sessions, transcript search/export, evaluations, provenance-rich artifacts,
  and multi-agent run audits
- Bounded, policy-gated context references; per-agent and workspace retrieval
  (lexical + vector), git-history search, and incremental code intelligence
- Governed plugins and MCP, lifecycle hook privacy with a durable delivery
  ledger, automatic git-worktree isolation, and the Symphony issue-to-PR pipeline

The repository has reusable Go packages, but does not promise a separately
versioned public SDK contract. Native provider adapters beyond those above
should be tracked as GitHub Issues until code and tests exist.

## Build, CI, and releases

Local development uses the Makefile as the main build surface:

- `make build` compiles `./atteler` from `./cmd/atteler`.
- `make test` runs all Go tests with the race detector and `-count=1`; override
  `TESTPACKAGE` and `TESTFLAGS` for focused runs.
- `make e2e` runs black-box CLI tests against a freshly built binary.
- `make lint` runs the pinned golangci-lint version.
- `make generate` regenerates derived files (lifecycle event docs, the docs-site
  fragments, and `llms.txt`/`llms-full.txt`); CI fails if the result is uncommitted.
- `make release-check` validates `.goreleaser.yaml`.

GitHub Actions runs CI on pull requests and branch pushes, builds and deploys the
versioned documentation site to GitHub Pages under `/main/` on pushes to `main`
and under `/vX.Y.Z/` on release tags, and packages a release via GoReleaser when
a semantic version tag such as `v0.1.0` is pushed.

## License

See [`LICENSE`](LICENSE).
