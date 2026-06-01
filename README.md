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

Eval suites support behavior-oriented checks beyond exact text: JSON/YAML path
assertions, regexes, unordered list matching, schema checks, required/forbidden
content, workflow side-effect assertions, replayed judge decisions, and run
metrics in the machine-readable report.

Common options for model, agent, output, generation settings, provider routing
settings, and compatibility flags can still be combined with domain commands
before or after the focused subcommand, for example
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
atteler config migrate
atteler config report
atteler config doctor-offline
atteler config doctor
```

Use the diagnostics commands for different readiness questions:

- `atteler config validate` is the strict config/schema gate. It loads the
  merged Atteler config, prints non-fatal harness-import warnings, and exits
  non-zero for parse, unknown-field, migration, hook-payload, or agent-loop
  budget errors.
- `atteler config doctor-offline` is the provider-independent readiness check
  for CI, support bundles, and machines without network/provider credentials. It
  exits non-zero on the same fatal Atteler config load errors as `validate`,
  still prints best-effort local summaries, prints text-mode fatal reasons to
  stderr, and supports machine-readable JSON output for CI; run
  `atteler help config` for the exact flag.
- `atteler config doctor` includes provider-aware readiness and credential
  checks. Run it after `validate` or `doctor-offline` when you want to confirm
  local provider registration, credential availability, and provider health.

Diagnostic severities are:

- `fatal`: a selected Atteler config file could not be read, strictly parsed,
  migrated, or schema-decoded; readiness commands exit non-zero.
- `warning`: non-fatal issues such as best-effort harness-import warnings,
  deprecated fields, or optional provider/file fallbacks.
- `info`: contextual notes such as implicit defaults, schema metadata, or
  missing optional conventional files.

Use `atteler config explain` without a field prefix to print every tracked
field, or pass a prefix such as `default_model`, `providers.openai`, or
`agents.reviewer` to focus on one model, provider, or agent. Runtime diagnostic
paths such as `runtime.selected_model` and `runtime.selected_provider` explain
the selected request model/provider after state, flags, and agent selection.
`atteler config doctor` prints provider readiness with registered, disabled,
missing-credential, health-check, live-model, and static-fallback status so a
broken backend is visible before a completion request fails.
Use `atteler config report` for a redacted YAML diagnostics bundle suitable for
issue reports; it includes schema versions, candidate source status, migrations,
implicit defaults, unknown/deprecated field findings, and redacted merged
origins, plus harness importer warnings.
Use `atteler config migrate` to rewrite existing Atteler config files and the
persisted state file to the current schema; missing files are skipped.
Read-only inspection can also be triggered through debug aliases such as
`DEBUG_ATTELER_CONFIG_REPORT=1`, `DEBUG_ATTELER_EXPLAIN_CONFIG=1`,
`DEBUG_ATTELER_EXPLAIN_CONFIG_FIELD=providers.openai`, and
`DEBUG_ATTELER_STATE_DIAGNOSTICS=1`.

Harness importer warnings, including unsupported fields, malformed best-effort
input, and ignored fallback-only sections, are printed by `atteler config
validate`, `atteler config explain`, and `atteler config report`; `explain` also
shows the source path and `harness-import` precedence for every imported value.

Minimal example:

```yaml
version: 1

default_provider: openai
default_model: gpt-4.1-mini
fallback_models: ["gpt-4.1", "gpt-4.1-nano"]
model_aliases:
  mini: openai/gpt-4.1-mini
  fast: openai/gpt-4.1-mini
models:
  planner:
    preferred: openai/gpt-4.1
    fallback: openai/gpt-4.1-mini
    required_capabilities: ["tools", "json_schema"]
    max_cost_usd: 0.25
    max_latency_ms: 2500
    max_ttft_ms: 900
  fast_coder:
    preferred: openai/gpt-4.1-mini
    fallback_models: ["ollama/llama3.2"]
    prefer_local: true

generation:
  temperature: 0
  top_p: 1
  seed: 1
  # Use "fast" only for OpenAI-family models that advertise fast-mode support.
  # For other models, omit this or leave it as "default".
  model_mode: default
  reasoning_level: medium
  max_tokens: 2048

agent_loop:
  # 0 means no ceiling. These ceilings are cumulative for one agent loop.
  max_output_bytes: 0
  # max_cost_micros caps estimated provider spend in micro-units of currency
  # (for example, 1000000 = 1.0). When set, Atteler requires provider/model
  # pricing and usage metadata, and fails closed for unpriced or unmetered
  # models.
  max_cost_micros: 0
  # max_input_tokens and max_output_tokens use provider-reported actual usage
  # accumulated across the whole loop. They are distinct from
  # context.max_input_tokens, which preflights each individual request before
  # it is sent; the stricter gate wins in practice based on which one trips.
  # When token ceilings are set, Atteler fails closed for unmetered or
  # partially metered responses.
  max_input_tokens: 0
  max_output_tokens: 0
  max_total_tokens: 0
  # max_iterations caps the number of tool-use turns per agent loop. Omit (or
  # set to 0) to run unlimited turns until the model returns a final response
  # or another budget — model calls, tool calls, wall time — trips.
  max_iterations: 0
  # max_model_calls and max_tool_calls cap cumulative model completions and
  # executed tool calls.
  max_model_calls: 0
  max_tool_calls: 0
  # max_wall_time caps the wall-clock duration of an agent loop. Parsed via
  # Go's time.ParseDuration (e.g. "30m", "1h30m"). Omit, leave empty, or set
  # to "0" for no wall-clock cap (the default).
  max_wall_time: "0"
  # checkpoint_interval prompts the user to confirm continuation every N
  # tool-use iterations. Omit (or set to 0) to never prompt — the default.
  checkpoint_interval: 0

providers:
  openai:
    base_url: https://api.openai.com
    retry:
      max_attempts: 2        # additional retries after the first request
      initial_backoff_ms: 1000
      max_backoff_ms: 10000
      max_elapsed_ms: 30000
      jitter_fraction: 0.2
  anthropic:
    disabled: false
    base_url: https://api.anthropic.com
    # Optional: keep direct Anthropic API keys but block Claude/Forge borrowed credentials.
    # disable_private_adapter: true
  codex:
    # Private adapter that borrows Codex CLI ChatGPT-login credentials.
    # Disable it without disabling the normal OpenAI provider.
    # disable_private_adapter: true
  claude-code:
    # Private adapter that borrows Claude Code OAuth credentials.
    # Disable it without disabling the normal Anthropic provider.
    # disable_private_adapter: true
  ollama:
    base_url: http://127.0.0.1:11434
    # Opt in before Atteler starts a local long-lived "ollama serve" daemon.
    auto_start: false
  vllm:
    type: openai_compatible
    base_url: http://127.0.0.1:8000
    local: true
    models: ["qwen2.5-coder"]
    capabilities: ["chat", "tools", "json_schema", "local"]
  groq:
    type: openai_compatible
    base_url: https://api.groq.com/openai/v1
    api_key_env: GROQ_API_KEY
    models: ["llama-3.3-70b-versatile"]
    capabilities: ["chat", "tools", "json_schema"]
  azure:
    type: azure_openai
    base_url: https://example-resource.openai.azure.com
    api_key_env: AZURE_OPENAI_API_KEY
    api_version: 2025-01-01-preview
    models: ["deployment-name"]

agents:
  reviewer:
    description: Code review specialist
    personality: concise
    capabilities: ["review", "security"]
    model: gpt-4.1-mini
    fallback_models: ["gpt-4.1-nano"]
    routing_policy:
      preferred_providers: ["openai"]
      banned_providers: ["ollama"]
      required_capabilities: ["tools"]
      max_budget: 0.25
      max_latency_ms: 2000
      max_ttft_ms: 800
    reasoning_level: high
    triggers: ["review this", "code review"]
    system_prompt: >
      You are a concise code reviewer. Focus on correctness, tests, and
      maintainability.
    temperature: 0
    max_tokens: 1200
```

For OpenAI-family providers, `model_mode: fast` maps to the current
`service_tier=priority` API field; imported Codex configs with
`service_tier = "priority"` are normalized to this same model mode.

Bare model names resolve only by exact provider catalog claim, explicit
`model_aliases` entry, or configured `models.<role>` entry. Role entries such as
`planner` and `fast_coder` keep task intent separate from concrete models:
Atteler evaluates the preferred model plus fallbacks against required
capabilities, budget, local preference, provider availability, telemetry, and
rate-limit evidence before sending the request. If more than one provider claims
the same bare name, Atteler reports the collision unless a configured default
provider/model or role routing policy (for example preferred/banned providers)
makes the provider choice deterministic. Legacy `gpt*`/`claude*`
prefixes are kept only as readiness-discovery hints, not as completion-routing
rules. When constraints force an ambiguous bare model to be evaluated across
providers, the configured default provider remains the tie-breaker unless it is
rejected by a hard constraint such as budget, capability, availability, or
latency.

Custom providers with `type: openai_compatible` use the same chat-completions
wire contract as OpenAI while keeping their own provider name for routing, model
selection, and run metadata. Set `api_key_env` for hosted endpoints; omit it for
local/self-hosted endpoints that do not require Authorization. `type:
azure_openai` uses Azure's deployment path shape with `api-key` auth and
`api_version`; advanced compatible endpoints can override `api_key_header`,
`api_key_scheme`, `chat_completions_path`, `embeddings_path`, and `models_path`. Set
`local: true` for self-hosted compatible endpoints when `prefer_local` roles
should prefer them; loopback URLs such as `127.0.0.1` are inferred as local.
Provider `type` aliases such as `groq`, `mistral`, `cohere`, `gemini`,
`google_gemini`, `google_ai_studio`, `vertex_ai`, `bedrock`, `aws_bedrock`,
`vllm`, `tgi`, `self_hosted`, `litellm`, and `openrouter` are accepted as
OpenAI-compatible endpoints when the configured `base_url` serves that wire
shape. When the provider name itself is one of those aliases and `base_url` is
set, the `type` field can be omitted.
Use `capabilities` on compatible endpoints to narrow route metadata and safely
reject unsupported knobs (for example tools or JSON/schema output) instead of
assuming every OpenAI-compatible backend supports the full OpenAI feature set.
Accepted route capability names are `text`, `chat`, `tools`, `reasoning`,
`json_schema`, `embeddings`, `vision`, `multimodal`, `batch`, `prompt_cache`,
`streaming`, `rate_limits`, `retries`, `fallback`, `cost_tracking`, `local`,
and `fast_mode`; `atteler config validate` rejects unknown route capability
names so a typo cannot silently reject every candidate at runtime.
Embedding calls use the same registry, role, capability, fallback, and telemetry
path as chat calls, with an implicit `embeddings` capability requirement.
Batch completion calls similarly resolve roles with an implicit `batch`
capability requirement and use the same retry/fallback/error handling path for
each item in the batch.

Agent `routing_policy` entries are evaluated against the built-in versioned
model catalog plus runtime provider evidence. The router considers context
windows, output limits, input/output/cache prices, required capabilities,
provider bans/preferences, budget and latency/TTFT caps, live provider/model availability when
known, observed latency/TTFT, rate-limit telemetry, and actual token usage from
previous calls. When routing evidence is used, Atteler emits a `route_decision`
hook artifact with every candidate considered, constraints applied, rejection
reasons, estimated cost, fallback order, provider-model verification state, and
post-response actual cost/usage when available. Runtime calls refresh provider
model lists within a short bounded window before applying availability
constraints, and event metadata repeats profile estimates, availability counts,
constraints, selected estimated cost, observed latency/TTFT, and actual
usage/cost deltas, including whether the profile was interactive or batch, for
quick log inspection without parsing the full JSON artifact.
Agent-loop checkpoint ledgers also record the concrete provider/model that
answered each model call, per-call latency/TTFT, token usage, and estimated
cost micros so resumed or audited tool loops keep the same routing/cost trail.
When an agent selects a model role, the agent `routing_policy` is layered on top
of the role's own constraints so per-agent provider bans, capability
requirements, freshness requirements, and budget caps still apply.
Manual `providers route-interactive` and `providers route-batch` previews use
the same catalog-backed estimates; `--route-input-tokens`,
`--route-output-tokens`, `--route-cache-reuse`, and
`--route-cache-write-tokens` let operators model prompt-cache read/write costs
before a live call. Unknown manual candidates can supply explicit metadata like
`provider/model,input=0.001,output=0.002,max=1000`;
optional keys include `cached`, `cache_write`, `max_output`, `latency`, `ttft`,
and `capabilities=chat|tools|json_schema`. Manual capabilities are validated
using the same route capability vocabulary as config files. The route
required-capability option is repeatable/comma-separated so previews can show
how hard capability constraints affect the selected candidate and fallback
order.

Provider retry settings are per-provider. `max_attempts` is the number of
additional retries after the first request, and the elapsed budget, max backoff,
and jitter are applied before each retry. Set `max_attempts: 0` to disable
provider retries for a specific provider. `atteler config doctor-offline` prints
the effective retry policy for known and configured providers; live
`atteler doctor` prints the policy next to each health-checked provider. Retry
lifecycle events use the `provider_retry` hook type and include attempt,
provider, model, status, delay, request ID, and final outcome metadata without
prompt or credential content.

Credentials come from environment variables, supported local harness config,
provider-owned credential stores, or local daemons depending on the provider.
Do not infer subprocess sandboxing from a provider name: the `codex` and
`claude-code` adapters reuse those tools' credentials but send HTTPS requests
directly from atteler.

### Provider runtime and trust boundaries

`atteler --list-providers` and `atteler --list-known-models` read the built-in
provider inventory from `llm.KnownProviders()` without credentials or network
calls. The doctor command registers the configured providers and then runs the
provider-specific health check described below, so some health checks hit the
network while Codex and Claude Code only validate loaded local credentials.

The following generated block is checked by `go test ./pkg/llm`; update
[`pkg/llm/provider_runtime.go`](pkg/llm/provider_runtime.go) when an execution
path, credential source, token refresh behavior, endpoint, sandbox/tool
boundary, health check, or model inventory changes. Refresh the block with
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

### Private provider adapter contracts

The `codex` and `claude-code` providers are explicit private compatibility
adapters for borrowed CLI credential stores. `atteler --doctor` (or
`atteler config doctor`) reports their adapter contract, credential/refresh
readiness, non-network-verified model catalog status, and static context-window
provenance. Disable these adapters
with `providers.codex.disable_private_adapter: true`,
`providers.claude-code.disable_private_adapter: true`,
`ATTELER_DISABLE_CODEX_ADAPTER=1`, `ATTELER_DISABLE_CLAUDE_CODE_ADAPTER=1`, or
`ATTELER_DISABLE_PRIVATE_ADAPTERS=1`; this does not disable the normal
`openai` or API-key `anthropic` providers. Use
`providers.anthropic.disable_private_adapter: true`,
`ATTELER_DISABLE_CLAUDE_CODE_ADAPTER=1`, or
`ATTELER_DISABLE_BORROWED_CREDENTIAL_ADAPTERS=1` to keep direct Anthropic
credentials enabled while blocking Anthropic fallback to borrowed Claude
Code/Forge credential stores.

Ollama daemon auto-start is explicit: set `providers.ollama.auto_start: true`
or `ATTELER_OLLAMA_AUTO_START=true` before Atteler may launch `ollama serve` for
a selected local Ollama endpoint. `ATTELER_OLLAMA_AUTO_START=false` disables it
even when config opts in. Use `atteler providers ollama-status` (or
`--ollama-status`) to inspect whether Ollama is remote, unavailable, already
running, or started by Atteler. When Atteler owns a recorded daemon,
`atteler providers ollama-stop` (or `--ollama-stop`) stops it and removes the
ownership record. Startup logs and ownership metadata are kept under Atteler's
state directory for diagnostics.

### Provider protocol contracts

Provider adapters intentionally expose `llm.ProviderCapabilities` metadata via
`llm.ProviderCapabilitiesFor` and `llm.KnownProviders` so callers can check
whether a provider supports seed, tools, reasoning, model modes, cached-token
accounting, JSON/schema-constrained outputs, streaming, and network model
discovery before setting provider-specific knobs. The same metadata documents
lossy mappings, provider-adjusted request options, and unsupported
`CompleteParams` fields:

| Provider | Intentionally lossy or adjusted mappings | Unsupported or unavailable fields |
| --- | --- | --- |
| OpenAI | `ToolResult.IsError` is not represented by Chat Completions tool messages. | None currently documented. |
| Anthropic | Reasoning levels become thinking token budgets; when thinking is enabled, `Temperature` is coerced to `1`; system messages are lifted to `system`; tool results become user-role content blocks. | `Seed`, `ModelMode`, `ResponseFormat` |
| Claude Code | Same request/response mapping as Anthropic over the Claude Code OAuth path; when thinking is enabled, `Temperature` is coerced to `1`. | `Seed`, `ModelMode`, `ResponseFormat` |
| Codex | `Temperature` and `MaxTokens` are omitted because the ChatGPT Responses adapter does not expose them; system messages become Responses `instructions`; chat/tool history becomes Responses input items; `ToolResult.IsError` is not represented. | `TopP`, `Seed`, `Stop` |
| Ollama | Reasoning levels become Ollama `think` values; `ResponseFormat` maps to Ollama `format` but `Name`/`Strict` are not represented; tool-call IDs, tool-result IDs, and `ToolResult.IsError` are not represented in Ollama chat messages. | `ModelMode`; cached-token accounting is not reported by Ollama responses. |

Unsupported non-zero knobs, non-finite sampling values, and non-JSON-serializable
tool schemas, tool-call inputs, or response schemas are rejected instead of
silently dropped.
Unavailable knobs or provider-constrained values with explicit adapter handling
are normalized before dispatch and reported in activity metadata.
`SupportsStreaming` in the capability metadata means caller-facing
`llm.StreamProvider` support, not whether an adapter happens to use a streaming
wire transport internally.

### Lifecycle hook privacy

Lifecycle hooks are local subprocess integrations, not trusted in-process
extensions. Hook payloads are schema-filtered per event type, size-bounded, and
default to least-privilege metadata only:

```yaml
hooks:
  assistant_message:
    - command: ["./scripts/notify-assistant"]
      payload: metadata      # metadata (default), summary, or full
      inherit_env: false     # default; set true only for trusted hooks
      env:
        NOTIFY_CHANNEL: dev
```

- `payload: metadata` sends event type, timestamp, session id, agent/model/role
  where applicable, and allowlisted non-sensitive metadata such as byte counts,
  operational counts, or provider/tool names. Prompt text, assistant text,
  command output, raw errors, file/session paths, commands, cwd values, and
  arbitrary metadata are omitted.
- `payload: summary` adds bounded redacted summaries for sensitive content/error
  fields without copying the raw text.
- `payload: full` is for trusted hooks that truly need content-bearing fields.
  It still applies known-secret redaction and size limits, but it may expose
  prompts, command text, paths, and command output after redaction. Redaction is
  best-effort pattern matching, not a substitute for keeping untrusted hooks on
  `metadata` or `summary`.
- Serialized hook payloads are capped at 64 KiB. Within that envelope, content
  fields are capped at 8 KiB, error fields at 2 KiB, and scalar/metadata values
  are separately bounded; payloads include `redacted`/`truncated` markers when
  Atteler omits or shortens data.
- Hooks do not inherit Atteler's full environment by default. Add explicit
  `env:` entries, or set `inherit_env: true` only when a trusted hook needs the
  parent `PATH`, `HOME`, credentials, or other ambient variables. Explicit
  `env:` values are passed verbatim, so do not place credentials there unless
  the hook genuinely needs them. Atteler-owned event variables such as
  `ATTELER_EVENT_TYPE`, `ATTELER_PAYLOAD_MODE`, `ATTELER_SESSION_ID`,
  `ATTELER_SESSION_PATH`, `ATTELER_AGENT`, `ATTELER_MODEL`, and `ATTELER_ROLE`
  are reserved and always generated from sanitized event data.

Default metadata schemas are deliberately narrow:

| Event family | Default and opt-in policy |
| --- | --- |
| `user_message`, `assistant_message` | Role by default. `payload: summary` adds generated summaries; `payload: full` includes prompt/assistant text after redaction and bounds. |
| `command_execute`, `command_output` | Safe source/provider/tool-call metadata by default. `payload: summary` adds content/error summaries; `payload: full` also includes commands, cwd, and bounded redacted content/errors. |
| `file_read`, `file_write`, `context_add` | File kind/byte/truncation metadata only by default. Paths require `payload: full`; file contents are omitted. |
| `context_manifest`, `route_decision` | Bounded audit metadata by default. Full JSON artifacts require `payload: full` or the existing headless-private logging path. |
| `tool_execute`, `agent_execute`, sessions, errors | Provider/tool/agent/session identifiers where applicable. Error details require `payload: summary` or `payload: full`; built-in logs summarize rather than copy raw errors. |

Built-in event logging follows the same privacy boundary: it records hook/event
activity with bounded metadata and redaction markers instead of raw content, raw
errors, path metadata, or secret-looking values.
Debug audit logs for hook invocation include the event type, hook command
basename, payload mode and byte size, timeout, `inherit_env`, and configured
environment key count; they do not include hook argv values, environment values,
or the event payload.

## Common workflows

### One-shot and interactive chat

```sh
atteler
atteler chat once "Explain this repository in one paragraph"
git diff | atteler chat once "Review this diff" --stdin
atteler chat once "Summarize @README.md" --headless --headless-id docs-summary --output json
atteler session headless
atteler session status-headless <headless-id>
atteler session cancel-headless <headless-id>
atteler session recover-headless
atteler session stream-headless <headless-id>
```

Headless metadata, event summaries, and logs are redacted by default; reserve
`--headless-private-log` for local private runs that intentionally keep raw
prompts, errors, event summaries, and log text.
Each headless run records PID, process group, command arguments, cwd, host,
start time, last heartbeat, optional parent/child run IDs, terminal reason, and
a separate `<id>.events.jsonl` lifecycle summary with `started`,
`user_message`, `assistant_message`, and terminal events. Lifecycle statuses
distinguish `running`, `completed`, `failed`, `canceled`, `timed_out`,
`stale`, `orphaned`, `superseded`, and `corrupt` metadata. Running one-shot
headless runs refresh their heartbeat every 15 seconds; records with no
heartbeat for 30 minutes are reconciled as `stale` or `orphaned`, while missing
or dead local PIDs reconcile immediately.
Atteler reconciles stale running records at startup and when listing or checking
status, so crashed local PIDs do not stay `running` forever.
`atteler session cancel-headless <id>` records a durable `canceled` status
before signaling the recorded local PID or process group; on Unix-like hosts it
escalates to a kill signal if the process ignores cancellation briefly.
Set a stable headless ID when launching a one-shot headless run if another
process needs a handle for `status-headless`, `cancel-headless`, or
`stream-headless`; explicit IDs must be portable file names (no leading or
trailing whitespace, path separators, control characters, or `<>:"|?*`), must
be unique, and reuse is rejected while metadata, logs, events, or artifacts for
that ID exist.
Launch nested headless work with `ATTELER_HEADLESS_PARENT_ID=<id>` to record
parent/child run relationships in both metadata and structured events.
Raw log text is retained in rotated chunks capped at 1 MiB each and 8 chunks per
run by default; older chunks are removed after the retained size is exceeded.
The printed `log=` path is the logical base, and retained chunks use
`<headless-id>.log.000001`, `<headless-id>.log.000002`, and so on.

In the interactive TUI, `Enter` sends the prompt, `Shift+Enter` inserts a
newline for multi-line drafts (`Alt+Enter` remains available as a terminal
fallback), `Ctrl+O` opens the model picker (including `mode=fast` rows for
compatible OpenAI-family models), `Tab` accepts visible prompt completions or,
only after opt-in, requests a fresh model-backed suffix when none is valid,
`Ctrl+R` rewrites under-specified prompts without adding boilerplate to
already-structured drafts, and `Ctrl+Z` undoes the latest rewrite.
Interactive prompt assistance defaults to the deterministic no-network
completion path even when providers are configured. Use `/suggestions session`,
`/suggestions folder`, or `/suggestions global` to opt in to model-backed idle
suffix suggestions; `/suggestions local` returns to local-only, and
`/suggestions status` shows the active mode, current usage, budgets, and privacy
scope. Session opt-ins are saved as `prompt_suggestions`; folder/global defaults
are saved as `default_prompt_suggestions` in Atteler state. Background
suggestions show the provider/model and a compact context summary while a draft
is being sent. Private file, task, and issue context is omitted from those
background calls, local candidate descriptions are omitted, usage is recorded
under `background_suggestions` in session metadata, and the default background
budget is 20 requests per session, 6 requests per minute, 1,024 estimated input
tokens per request, 32 output tokens per request, 12,000 session tokens, and
$0.05 estimated cost. Provider models without maintained pricing metadata use a
conservative catalog-high estimate; local Ollama models are treated as
zero-cost. `--prompt-local-only` still forces the local-only path for the
process.

For non-interactive checks, `atteler agents prompt-complete "ask @rev"` previews
the same local completion engine with per-source context freshness
(`fresh`, `stale`, `partial`, `skipped`, or `error`), source attribution,
replacement ranges, rank signals, and a short explanation of what accepting the
completion inserts.

### Session evaluations and performance summaries

Saved sessions can record agent evaluations, negative-knowledge incidents, and
artifacts for later review. Evaluation records include versioned metadata for
provenance (`human`, `harness`, or `ci`), evaluator identity, rubric version,
task type, difficulty, expected outcome, model, agent version, duration, cost,
and evaluator confidence. `atteler eval record <agent> --evaluation-report
report.json` imports pass rate, flake count, provider/model, fixture version,
latency, token, and cost metadata from a machine-readable eval report. Negative
knowledge is tracked separately by task type and severity instead of being
flattened into a score.

`atteler agents performance` is a diagnostic summary, not an automatic routing
signal. Scores are grouped into compatible source, rubric, task, difficulty,
model, and agent-version buckets before any average is shown; incompatible
rubrics are not averaged together. Each bucket reports sample size,
small-sample-adjusted confidence interval, standard error, runtime/cost
coverage, recency-window bounds and counts, latest score timestamp, and
regression status plus bucket-level routing eligibility and validity reasons.
The summary also prints explicit routing validity checks and remains
`routing_eligible=false` until a compatible bucket has enough total and recent
samples, known provenance, a versioned rubric, task class, difficulty, model,
agent version, confidence coverage, and bounded uncertainty.

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
remote, size, provider-calibrated token point estimate/error bound/upper bound,
truncation, digest, fetch time, redirect target when applicable, policy
decision, and machine-readable reason code), and prints both per-reference audit
lines and a JSON reference manifest for every loaded, truncated, skipped,
omitted, or rejected configured reference. If any configured reference is
rejected, that configured-reference block is omitted instead of silently sending
partial context. Reference text, sources, and diagnostics are sanitized for
control characters and common secret-looking values before they are printed or
sent to a model.

Before each model request, Atteler also emits a `context_manifest` hook/event
with the final message count, provider-calibrated input-token upper bound,
the estimator profile/calibration ID and error-bound margin used for that upper
bound, configured and model-window budget fit (including fallback-model
estimates), inline `@path` references with content digests, configured
reference decisions, truncation status, omissions, and skipped/rejected reason
codes. Rejected inline `@path` attempts emit the same manifest shape before
aborting request assembly, so blocked root escapes and symlink escapes remain
auditable.

By default, configured local references must stay under the current working
directory. Absolute paths require `allow_absolute_paths: true`; paths outside
the working directory also require an explicit `local_roots` entry and are
audited in the reference provenance. Local glob policy is evaluated against the
path shown relative to the allowed root that contains the file, so entries under
extra `local_roots` should use patterns relative to those roots. Remote
references support only HTTP(S) URLs, default to HTTPS-only, and are denied
unless `reference_policy.allowed_hosts` allows the host and the port policy
allows the effective port. Leave `allowed_schemes` or `content_types` unset to
use the safe defaults; set either list to `[]` only when you intentionally want
to deny all remote schemes or response MIME types. Private, loopback,
link-local, and multicast targets remain blocked unless
`allow_private_networks: true` is set deliberately. Host wildcards such as
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
    denied_hosts: []
    allowed_ports: [443]
    denied_ports: []
    local_roots:
      - ../shared-style-guides
    denied_local_roots: []
    allowed_globs:
      # Matched relative to the allowed root containing each file.
      - docs/**/*.md
      - "**/*.md"
    denied_globs:
      - "**/*.pem"
      - "**/.env"
    max_redirects: 0
    max_files: 200
    content_types:
      - text/*
      - application/json
    allow_absolute_paths: false
    allow_private_networks: false

agents:
  reviewer:
    references:
      - ./docs/reviewer-rubric.md
```

### Context compression audit metadata

`atteler memory context-pack <transcript.txt>` uses the same calibrated token
estimator and omission manifest logic as model requests. Transcript lines can
pin required context or raise its retention priority with bracket metadata:
`user[timestamp=2026-05-22T10:00:00Z,pinned,priority=42]: keep this`. Pinned
messages, positive-priority messages, and system messages are treated as
required context; if they cannot fit inside `--context-pack-tokens` (or the
selected model window), the command fails instead of silently dropping them.
Hard failures include a stable `budget_failure_code` alongside the human
message.
Omitted messages are represented by a compact evidence manifest containing
hashes, timestamps, summaries, token estimates, drop reasons, and stable reason
codes. The omission manifest is schema-versioned so downstream audit tooling can
detect future format changes.

### Deterministic response fixtures and eval checks

```sh
atteler chat once "Summarize @README.md" --record-response .atteler/fixtures/readme-summary.json
atteler chat once "Summarize @README.md" --replay-response .atteler/fixtures/readme-summary.json
atteler eval output .atteler/fixtures/readme-summary.txt \
  --eval-expected "package overview" \
  --eval-mode contains
atteler eval run .atteler/evals/readme.eval.yaml \
  --eval-json \
  --eval-report .atteler/eval-report.json
```

Replay writes normal session messages while avoiding provider availability and
sampling noise in tests. Structured eval files can combine required/forbidden
content, regex, JSON/YAML path, unordered list, inline or file-backed schema,
numeric, workflow side-effect, artifact-produced/artifact-existence, exit-code,
and recorded judge decision assertions. Reports are JSON with per-assertion
status, evidence, severity, remediation hints, redacted snippets, pass rate,
flake count, run metrics, and fixture/model/provider metadata for CI
consumption. Golden updates require both `--eval-update-golden` and
`--eval-approve-golden-update` so fixture refreshes remain reviewable.

```yaml
version: 1
metadata:
  target_command: atteler chat once "Summarize @README.md"
  provider: openai
  model: openai/gpt-5.4
  agent: reviewer
  input_fixture: prompts/readme-summary.txt
  fixture_version: readme-summary/v2
  owner: qa
metrics:
  latency_millis: 1200
  input_tokens: 900
  output_tokens: 180
actual: ../fixtures/readme-summary.json
workflow_file: ../fixtures/readme-summary.workflow.yaml
assertions:
  - id: status-json-shape
    type: json_path
    path: $.status
    equals: ok
  - id: summary-schema
    type: schema
    schema:
      type: object
      required: [status, bullets]
      properties:
        status:
          const: ok
        bullets:
          type: array
          minItems: 3
  - id: required-bullets
    type: unordered_list
    path: $.bullets
    items: [package overview, quickstart, feature map]
    allow_extra: true
  - id: used-file-context
    type: tool_called
    name: read_file
    args:
      path: README.md
  - id: produced-report-artifact
    type: artifact_produced
    path: reports/readme-summary.json
    kind: report
  - id: wrote-no-secrets
    type: contains
    required: [package overview, quickstart]
  - id: no-secret-dump
    type: not_contains
    forbidden: [api_key=, Authorization:]
    remediation: Remove secret-looking debug output from the response.
  - id: rubric-replay
    type: judge_decision
    equals: pass
    judge:
      judge: rubric-bot
      model: openai/gpt-5.4
      rubric_version: summary-rubric/v1
      input_ref: prompts/readme-summary.txt
      output_ref: ../fixtures/readme-summary.json
      decision: pass
      rationale: Covers the required README sections.
```

Judge assertions only replay recorded decisions; the eval runner never calls a
judge model during evaluation and rejects suites where a judge is the only
signal. Recorded judge metadata is copied into the machine-readable result so
the decision can be audited later.

### Plugins and local run policy

Configured `plugins.paths` entries point at local plugin directories or manifest
files. `atteler plugins list` validates `plugin.yaml`, `plugin.yml`, or
`plugin.json` manifests with `name`, `version`, optional `description`,
`capabilities`, relative `entrypoints`, optional `min_atteler_version`,
structured entrypoint contracts, and optional security/provenance metadata.

Executable plugin entrypoints must declare their runtime contract before they
can run:

```yaml
name: reviewer
version: "1.2.3"
min_atteler_version: "1.0.0"
entrypoints:
  check: bin/check
entrypoint_contracts:
  check:
    inputs:
      args: []
    output:
      format: json
      adapter: stdout
      schema:
        type: object
        required: [summary, passed]
        properties:
          summary:
            type: string
          passed:
            type: boolean
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
provenance:
  source: registry
  repository: gh:owner/plugin
  ref: v1.2.3
  digest: sha256:<package-digest>
```

CLI plugin runs and dry-runs also require an accepted local policy in config.
The policy is an upper bound: manifests requesting anything outside it fail
before execution or before a dry-run can claim the entrypoint would run.

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
accepted policy, route process start through the central side-effect policy,
including declared network/write/secret access, enforce `min_atteler_version`
when the caller supplies a release version, require shell permission for
shell-script entrypoints, pass only allowlisted environment variables, validate
declared args, and return stdout/stderr after size limiting plus secret
redaction. Entrypoints with `format: json` output contracts also populate
structured results and fail closed when stdout is malformed or misses required
schema fields.

Loaded registries can emit installed-version metadata with
`Registry.Lockfile()`, `SaveLockfile`, and `LoadLockfile`; lockfiles record the
resolved plugin version, paths, capabilities, trust checksum/signature, minimum
Atteler version, and provenance source/repository/ref/commit/digest/signature
plus installed-at/by metadata so installs are reviewable.

Plugin entrypoints can be inspected or run with `atteler plugins list`,
`atteler plugins describe reviewer`, and
`atteler plugins run reviewer/check --plugin-dry-run`. Dry-run output includes
the resolved path/cwd, policy check status, compatibility floor, input count,
output format/schema flag, output limits, and declared permission envelope.

### Command policy and audit ledger

All local process launches go through the `pkg/shell` policy/audit gate before
`exec` starts. The gate records allowed, denied, and completed commands in a
JSONL ledger named `commands.jsonl`; set `ATTELER_COMMAND_AUDIT_DIR` to choose a
durable directory, otherwise Atteler writes under the process temp directory.
Captured stdout/stderr is written separately under `outputs/` after redaction.
Interactive and long-lived stdio protocol commands are still represented in the
ledger with an explicit `not_captured` or `sensitive_not_captured` output status.

The default policy strips credential-like environment variables (`*_TOKEN`,
`*_SECRET`, `*_KEY`, auth/password/cookie/private-key names), records the env
diff without values, denies destructive command patterns such as dangerous
`rm -rf`, and lets callers add command, cwd/path-argument, network, env, and
destructive allow/deny rules for narrower execution surfaces. Command rules
inspect both direct process launches and simple command words inside audited
`bash -lc` strings.

### Incidents, review, watch, memory, and code intelligence


```sh
atteler incident diagnose --sentry ISSUE-912
atteler incident diagnose --incident-file redacted-sentry-event.json
atteler incident diagnose --sentry ISSUE-912 \
  --incident-apply-fix \
  --incident-validation-command "go test ./pkg/auth" \
  --incident-open-pr
atteler incident diagnose --incident-ref alert-42 \
  --incident-mcp-manifest .atteler/mcp.yaml \
  --incident-mcp-server grafana \
  --incident-mcp-tool get_incident

atteler review scan
atteler review plan \
  --review-agent quality-reviewer \
  --review-agent test-engineer \
  --review-path pkg/llm/auth.go

atteler review run \
  --review-agent quality-reviewer \
  --review-path pkg/llm/auth.go \
  --review-gate "tests pass"

atteler watch scan
atteler watch json
atteler watch loop
# Use atteler help watch for baseline, suppression, gate, and issue-upsert flags.

atteler memory search "OAuth retry storm"
atteler memory retrieve "OAuth retry storm"
atteler memory git-history "memory regression"
atteler memory vector-search "redirect risks" --vector-index docs/research.md --vectorizer lexical

atteler code-intel summary
atteler code-intel summary --json
atteler code-intel symbol NewRegistry
atteler code-intel import-prefix github.com/tommoulard/atteler/pkg/
atteler code-intel query definitions:NewRegistry
atteler code-intel query definitions:helper --code-language python
```

`atteler incident diagnose` normalizes incident context from Sentry, a redacted
JSON fixture, or an MCP-backed observability connector. Generic JSON/MCP
payloads recognize common OpenTelemetry and OTLP `resourceSpans` /
`resourceLogs`, Datadog-like log API/data-array/monitor tags, Grafana Loki,
Prometheus query/Alertmanager webhook, CloudWatch Logs/Logs Insights/alarm
events, GCP Cloud Logging/Error Reporting, Kubernetes event/list, GitHub deployment,
and Azure Monitor/Application Insights shapes, including Log Analytics table
results and top-level log/event arrays from simple connector exports and MCP
text, JSON, or resource-text tool content. MCP server credentials that are
explicitly listed in a manifest server `env` block are passed to that configured
server and remain redacted in command audit records. The local JSON fixture path also accepts
newline-delimited JSON log exports. The report links stack frames to local source files,
correlates local git history when available,
extracts PR references from matching commits, adds CODEOWNERS-based reviewer
hints when ownership files are present, summarizes redacted
request/log/trace/metric/deploy context, records safe repro/validation command
status, duration, and bounded output excerpts when explicitly supplied, and
adds local worktree change summaries after the repair loop, compared with the
pre-repair status when available, so PR bodies can name changed code/test
files. It emits a test-first fix prompt plus a redacted
PR body template. Sentry tokens
are read from `SENTRY_AUTH_TOKEN` by default, then `SENTRY_ACCESS_TOKEN` or
`SENTRY_TOKEN` as fallbacks, with an override for the token environment-variable
name rather than a CLI token value. Sentry base URLs default to
`SENTRY_BASE_URL`, then `SENTRY_HOST`, then the host from a Sentry issue URL,
then `https://sentry.io`. Generated reports redact secrets, tokens, cookies,
emails, IPv4/IPv6 addresses, and sensitive request metadata before they are written to stdout,
files, command audit logs, tests, or PR bodies. Report and PR-body artifact
paths are resolved under the current workspace, and outside-workspace writes are
rejected by default.
User-supplied repro and validation commands are treated as local-only repair
evidence: they still run through the command policy/audit gate, deny destructive
patterns, and deny network-like commands unless a future command surface adds an
explicit approval path. Live incident fetches and optional PR creation use their
own explicit flags as separate external-system gates.

The apply-fix switch is an explicit approval gate for the expensive,
credentialed repair loop: after diagnosis and any supplied repro command, the
redacted fix prompt is handed to the selected Atteler agent so it can create a
failing regression test, implement the smallest fix, and then run supplied
validation-command checks before optional PR creation. The repair loop streams
agent output, so structured JSON/Markdown report output is reserved for
diagnose-only runs.
Diagnose-only runs can use the standard structured-output switch or Markdown
output format for machine-readable reports and PR templates; direct PR creation
requires the apply-fix gate plus at least one harness-captured validation
command, and stops on supplied validation failures so Atteler does not open an
empty or known-failing diagnosis-only PR. PR creation runs through GitHub CLI
and allows the standard `GH_TOKEN`, `GITHUB_TOKEN`, `GH_ENTERPRISE_TOKEN`, and
`GITHUB_ENTERPRISE_TOKEN` credential environment variables while keeping them
redacted in command audit records. When GitHub CLI returns a created PR URL,
Atteler adds the redacted link to the final diagnosis output.

Watch baselines accept either the JSON emitted by `atteler watch json`, a
`{"findings":[...]}` payload, or a baseline-ref option to scan the git
merge-base between `HEAD` and that ref as the branch-point baseline.
Suppression files accept either
`[{"id":"watch.rule:fingerprint","reason":"..."}]`,
`[{"fingerprint":"...","reason":"..."}]`, hand-authored
`[{"rule_id":"watch.stale_todo","path":"docs/todo.md","reason":"..."}]`,
or `{"suppressions":[...]}`. Rule config files accept either
`[{"rule_id":"watch.large_file","severity":"high"}]` or
`{"ignore_paths":["generated/"],"rules":[...]}`; rule entries can also
override `help`, assign an `owner`, or set
`disabled: true`. Watch-loop comparisons print each finding with a `status` of
`new`, `fixed`, `unchanged`, `suppressed`, or `unstable`; findings that
reappear after disappearing during a run are marked unstable so flaky scan
behavior is visible instead of being treated as ordinary debt. When a baseline
is active, text output emits `watch_baseline` lines and JSON output includes a
`baseline` object identifying the baseline file or git merge-base commit used
for the comparison.
Issue upserts are fingerprint-deduplicated and only target new, unsuppressed
findings that meet the configured severity threshold, so repeat scans update the
same tracker issue instead of opening duplicates for acknowledged debt. Enable
GitHub issue creation/update with the watch issue-upsert and repository options;
the token comes from the watch token option, `GITHUB_TOKEN`, or `GH_TOKEN`, and
labels default to `quality,watch`. `atteler review scan` can also emit the
watch gate as a structured review gate check.

`atteler review run` loads the review paths into the reviewed snapshot and
requires LLM reviewers to return strict JSON with no unknown or duplicate
fields. Findings must cite validated file and line-range evidence, include an
evidence excerpt found in that reviewed range, severity rationale, suggested
verification, and provenance with both reviewer judgment and a reviewed-context
line/range source. Gate checks must include review-context proof or command/test
proof from an explicit `Command output` section after the reviewed snapshot,
with command-output gate proof including the provenance source command and
summary output, or an explicit not-run reason backed by reviewer model-judgment
provenance; test, type, lint, and flake gates cannot cite model judgment as
proof. Malformed review responses fail the run and are surfaced in the partial
session output.

`atteler memory retrieve` prints the shared retrieval contract fields agents
should cite before injecting context: `source`, `document`, `stable_id`,
`chunk`, `range`, `scorer`, `inject_allowed`, freshness flags, and an optional
`why` ranking explanation.

Memory and vector stores persist schema, source-hash, provenance, redaction
policy version, timestamps, TTL, and embedding/vectorizer metadata where
vectors are stored, so stale embeddings or stale privacy policy provenance fail
closed until explicitly migrated. Use
`atteler memory migrate` or `atteler memory agent-migrate` after changing a
store schema, redaction policy, or vectorizer. Use `atteler memory delete`,
`atteler memory agent-delete`, `atteler memory compact`, and
`atteler memory agent-compact` to prove deleted or expired content is removed
from persisted JSON. Set the memory TTL options when indexing intentionally
short-lived content. Saved-session transcript messages and worktree paths are
excluded from local memory by default; opt in to those metadata/session-message
options only when needed.

Vector search is explicit about retrieval quality: lexical mode uses the
deterministic hashed token-frequency fallback, while embedding mode uses an
Ollama-compatible embedding endpoint. The lexical fallback is not semantic
retrieval. Search output prints the vectorizer/model and index
`created_at`/`updated_at` metadata that produced the ranking and reuses
`.atteler` indexes only while source digests, vectorizer metadata, dimensions,
and chunk settings still match. Searching with new file inputs refreshes the
file index from still-present indexed files plus the requested files,
re-vectorizing only changed or added files and removing deleted files from
persisted source metadata instead of keeping stale chunks.

Vectorizer config can be scoped so local RAG stores do not all share one
quality/cost tradeoff. Top-level vectorizer/provider/model/base-url,
fallback, timeout, and chunk fields are defaults; optional
`vector.stores.<name>`, `vector.agents.<name>`, and
`vector.sources.<kind>` entries override them in that order. Persisted
`index_path` values are intentionally store/source-specific so agent memory,
workspace files, sessions, git history, and ADRs do not accidentally share one
JSON datastore; relative index paths resolve under the workspace root. Valid
source indexes are also guarded by persisted source kind, so Atteler refuses to
refresh or clear a session index over a file/git/ADR index (and vice versa)
rather than silently clobbering another local RAG corpus. For example,
an embedding-backed agent-memory pipeline can use
`vector.stores.agent-memory.vectorizer: embedding` while
`vector.sources.git_history.vectorizer: lexical` keeps git-log indexing fully
local. Explicit file-vector retrieval through the memory retrieval command
resolves the same `vector.stores.vector-search` / `vector.sources.file`
settings as `atteler memory vector-*` and saves/reuses the configured file
index with source metadata. It also honors the vector runtime flags documented
by generated help when file inputs are supplied, so ad-hoc file RAG does not
silently fall back to lexical after an embedding-backed store is configured.

```yaml
vector:
  vectorizer: lexical              # safe dependency-free default/fallback
  fallback_policy: lexical         # fall back locally if embeddings fail
  stores:
    agent-memory:
      vectorizer: embedding
      model: nomic-embed-text
      base_url: http://127.0.0.1:11434
  agents:
    reviewer:
      model: reviewer-memory-embed # same store, agent-specific model
      index_path: .atteler/reviewer-memory.json
  sources:
    session:
      vectorizer: embedding
      index_path: .atteler/session-vector-index.json
    git_history:
      vectorizer: lexical          # avoid embedding private commit logs
      index_path: .atteler/git-history-vector-index.json
    adr:
      vectorizer: embedding
      index_path: .atteler/adr-vector-index.json
```

`pkg/agentmemory.NewStoreWithVectorizer` and `SetVectorizer` support
embedding-backed per-agent memory stores; loaded embedding stores require the
same runtime vectorizer identity to be supplied again before add/search/re-embed.
For embedding-backed stores, that identity includes provider, model, dimensions,
normalization/version, and the redacted embedding `base_url`, so changing an
Ollama-compatible endpoint fails closed until the store is intentionally
re-embedded.
`atteler memory agent-*` and the agent-memory retrieval source now resolve the
same scoped config, defaulting to lexical but binding the configured embedding
vectorizer for new or already-embedding stores. Use `memory agent-migrate`
after enabling an embedding vectorizer to re-embed an existing lexical
per-agent store intentionally instead of silently mixing vector models. One
persisted agent-memory JSON store is pinned to one vectorizer; give agents
distinct `index_path` values when they intentionally use different embedding
models or endpoints. Source kinds persisted by the vector index include `file`,
`session`, `git_history`, and `adr`; source digests, source kind, vectorizer
metadata, chunk settings, `created_at`/`updated_at`, provenance, and
privacy-policy metadata are part of the reuse/invalidation contract. Vector
indexes, vector stores, and agent-memory stores are written through
same-directory temp files and atomic rename so an interrupted refresh does not
leave a partially written JSON store at the configured path.
Configured `vector.sources.session`, `vector.sources.git_history`, and
`vector.sources.adr` entries are used by the session, git-history, and ADR
retrieval source modes. Explicit source config builds persisted source indexes
such as `.atteler/session-vector-index.json`,
`.atteler/git-history-vector-index.json`, and
`.atteler/adr-vector-index.json` with the selected vectorizer. Embedding-backed
source indexes provide the best semantic recall; explicit lexical source
indexes are persisted local fallback quality, and only unconfigured
lexical/default session or git modes keep the existing dependency-free
searchers. Changed sessions, commits, or ADR files are re-vectorized, deleted
source records are removed from the persisted vector index, and
`vector.fallback_policy: lexical` writes a separate `.lexical` source index so
fallback rankings stay local without mixing lexical vectors into an embedding
index. The same incremental lifecycle is available to background callers
through `vector.RefreshSourceIndexAsync`; workspace file sync uses
`vector.RefreshWorkspaceIndexAsync`.

Workspace vector context is opt-in with `vector.workspace_enabled: true`. When
enabled, Atteler builds or refreshes a per-workspace ANN datastore at
`.atteler/workspace-vector-index.json`, respects `.gitignore`,
`.attelerignore`, and `vector.workspace_exclude`, and skips binary, large,
cache, dependency, generated, and secret-looking files by default. The default
workspace vectorizer is local lexical hashing; setting `vector.vectorizer:
embedding` sends indexed chunks to `vector.base_url`. Non-loopback embedding
endpoints require `vector.workspace_allow_remote_embeddings: true`; with
`vector.fallback_policy: lexical`, Atteler stays local and uses the lexical
workspace index instead of uploading chunks without consent.

The local ANN layer still exact-scans small corpora by design. The default
threshold is 64 documents (`vector.DefaultANNExactSearchMaxDocuments`); above
that, limited searches use ANN candidates unless callers raise
`ANNOptions.MinCandidates`. Runtime code can call
`ANNOptions.UsesExactSearch(documentCount, limit)` to make the same
brute-force-vs-ANN decision explicit. Track scale with:

```sh
go test ./pkg/vector -bench BenchmarkSearchScale -benchmem
```

Benchmark subcases are named `bruteforce`, `ann-exact`, and `ann-approx` so
regressions show whether a corpus is still on the exact-scan side of the
threshold or has crossed into approximate candidate search. Retrieval explain
output records the same ANN mode and candidate count in scorer details so
agents can distinguish exact small-corpus rankings from approximate large-index
rankings.

Search-quality smoke coverage lives in
`pkg/vector/testdata/retrieval_quality.json`; it guards fallback ranking on a
small checked-in corpus, not broad semantic quality. Use brute-force search for
tiny stores where exact ranking is cheaper and more predictable. The CLI vector
search and retrieval adapters use the local ANN
layer for persisted vector indexes; small indexes still exact-scan through the
threshold above, while larger workspace/session/git-history corpora use
candidate buckets. Retrieval adapters prebuild the derived ANN buckets once per
refreshed index instead of rebuilding them for each query.

Code-intelligence commands accept `--json` (or `--output json`) to emit the
stable `atteler.code_intel.v1` schema; text output is rendered from the same
typed response contract. Add `--code-limit` and `--code-offset` to paginate
list-style code-intel results; JSON output includes pagination metadata when
those flags are set. `atteler code-intel query <kind[:value]>` uses the shared
language-neutral workspace index for files, symbols, definitions, references,
diagnostics, and relationships; use the language filter to scope query results.
The shared index persists under `.atteler/codeintel-index.json`, invalidates
changed or deleted source files, and currently indexes Go plus a lightweight
Python fixture/scanner path.

### Agents, plugins, artifacts, and worktrees

```sh
atteler agents plan "review this auth change" --plan-max-agents 3
atteler agents async-plan \
  --async-task 'plan|planner|draft plan' \
  --async-task 'code|coder|implement feature|plan'
atteler agents spawn 'planner|draft the migration plan' --spawn-dry-run
atteler agents speculate-run \
  --speculate-agent planner \
  --speculate-agent verifier \
  --speculate-gate "tests pass" \
  --speculate-gate "lint pass" \
  --speculate-prompt "pick the safest migration plan"
atteler agents skill-suggest plan --skill-step code --skill-step test
atteler agents skill-suggest "open GH-15|tool=github|prompt=Fix GH-15" \
  --skill-step "edit pkg/skill|tool=file-edit|input=pkg/skill" \
  --skill-save-dir .atteler/skills --skill-review-only
atteler agents skill-learning-list
atteler agents skill-learning-show k8s-investigation
atteler agents skill-learning-edit k8s-investigation
atteler agents skill-learning-disable k8s-investigation
atteler agents skill-learning-enable k8s-investigation
atteler agents skill-learning-delete k8s-investigation
atteler agents skill-learning-disable-all
atteler agents skill-learning-enable-all

atteler plugins list
atteler plugins describe reviewer
atteler plugins run reviewer/check

atteler session record-failure "retry token refresh timer" \
  --session 20260430-120000-deadbeef \
  --failure-reason "created retry storms" \
  --failure-commit abc123
atteler session merge-artifacts .atteler/merged-artifacts.md \
  --session 20260430-120000-deadbeef
atteler session record-artifact .atteler/review/decision.md \
  --session 20260430-120000-deadbeef \
  --artifact-kind decision
atteler session export 20260430-120000-deadbeef \
  --export-format markdown        # redacted shareable default
atteler session export 20260430-120000-deadbeef \
  --export-format private-markdown # explicit full-fidelity/private export
atteler session export 20260430-120000-deadbeef \
  --export-format issue           # compact issue/PR-ready summary
atteler session runs
atteler session show-run latest
atteler session export-run review
atteler session replay-run speculation
atteler session resume-run latest

atteler worktrees run "Add unit tests for the auth package"
atteler worktrees list
atteler worktrees merge 20260430-120000-deadbeef
```

Speculative `speculate-run` verdicts fail closed: the judge must emit exactly
one explicit `GATE <name>: PASS|FAIL <notes>` line for every required
`--speculate-gate`. Missing, malformed, duplicate, unknown, or failed gate
lines make the command fail; model silence is never accepted as success.

Async task and sub-agent execution now writes JSON ledgers under
`.atteler/runs/.../ledger.json` by default, with per-child transcript paths for
sub-agent command output and artifact paths printed in CLI summaries when
available. Ledgers include pre-run admission records with `admission_id`,
`admitted`, and `deny_reason` fields before attempts, so denied children and
halted siblings are auditable without inferring launch decisions from stderr.
Attempts, results, and CLI summaries carry the same `admission_id` so each
transcript can be traced back to its launch decision.
The generated agents help lists the ledger, resume, concurrency, timeout,
retry, cancellation, token/cost, and output-byte controls for safe retry and
backpressure tuning; child scopes that escape the parent allowed write scope,
including symlink escapes, are denied before spawn and recorded as admission
denials without synthetic execution attempts. Async tasks in downstream waves
after a halted wave are also
persisted as denied admissions without attempts, so resume has a durable
boundary for work that never became runnable. Child processes
receive `ATTELER_CHILD_*` identity and
`ATTELER_ALLOWED_WRITE_SCOPE` environment metadata so their workspace and write
scope are explicit in both runtime and ledger records. Budget exhaustion stops
queued child work even when ordinary child failures are allowed to finish, and
aggregate output-byte budgets serialize child accounting so concurrent children
cannot collectively overshoot before the next budget check. The Atteler child
command capture path enforces the configured or remaining aggregate output byte
cap during execution instead of buffering unbounded stdout/stderr. On resume,
stale `running` attempts from an interrupted process are recovered as `canceled`
with stop receipts before retry decisions are made.

Session Markdown and JSON exports default to the redacted shareable profile:
known credential patterns and local absolute paths are scrubbed, untrusted
Markdown content is fenced or escaped, and each export includes a provenance
manifest. Use `private-markdown` or `private-json` only when recipients are
allowed to see the full raw session.

Artifact merge keeps Markdown export for people, but the JSON bundle is the
review-gate contract: it includes `schema_version`, `ok`, summary counts,
content entries with hashes/provenance, structured warnings, and conflicts.

Review-agent and speculative execution runs are persisted as first-class
session receipts. The run records include provider-call prompts, prompt hashes,
models, fallbacks, token estimates, wall time, budget decisions, accepted or
rejected outputs, partial errors/cancellations, workflow artifacts, and gate
results so `replay-run`/`resume-run` can inspect recorded evidence without
re-calling providers. `issue` exports keep the receipt structure, budget, usage,
prompt hashes, and decisions while omitting raw multi-agent prompt/response and
artifact bodies for safer PR summaries. `resume-run` also prints the recorded
cursor, last call, terminal/current state, and suggested next action for the
next operator or agent.

Skill synthesis looks for repeated multi-step workflows and, when saved, writes
a reviewable skill directory (`<slug>/SKILL.md` plus `evals/triggers.yaml`)
instead of a loose markdown note. The generated skill front matter controls
triggering, the body records parameters, workflow steps, tool boundaries,
failure modes, verification, and provenance, and the eval fixture includes
positive and negative trigger prompts so synthesized skills do not become
over-broad repeated-string trophies. `--skill-save-dir` prints the generated
diff before persisting the accepted skill. For richer provenance, each
`--skill-step` may append `|prompt=...`, `|tool=...`, `|input=...`,
`|output=...`, `|verify=...`, and `|stop=...` metadata after the action label.
Use `--skill-review-only` to inspect the generated diff without writing files;
rerun without that flag after approving the skill.

Automatic skill learning runs behind the normal lifecycle event stream and
quietly records redacted, reusable workflow observations in
`.atteler/skill-learning/state.json`. When a multi-step workflow recurs often
enough, Atteler writes or improves a generated skill under
`.atteler/skills/generated/<slug>/` without prompting while the user is working.
Both default directories are local git-ignored paths to avoid accidentally
committing learned workflow state.
Active generated skills are silently added to future request context only when
their trigger shape matches the current prompt.
The learner intentionally persists summarized command/action shapes rather than
raw command output, and Kubernetes-style contexts, namespaces, pods, containers,
cloud cluster/project/profile names, Helm releases, tokens, and secret resource
names are parameterized before storage. Non-internal tool invocations are
recorded by tool name only, without raw tool arguments or output. Generated
Kubernetes skills that include
mutating or sensitive secret/token steps are not broadly auto-injected for
generic incident prompts; request those skills by name when they are truly
intended. Use
`atteler agents skill-learning-list` to inspect generated skills and the
effective enabled/disabled status after config, environment, and local state,
`skill-learning-show <slug>` to review a `SKILL.md`,
`skill-learning-edit <slug>` to open it with `$VISUAL` or `$EDITOR`, or edit the
printed skill path directly to customize it. Manual edits are not overwritten by
later background updates; run `skill-learning-enable <slug>` after review to
accept the edited file as the new auto-update baseline.
Use `skill-learning-disable <slug>` to stop updating one generated skill,
`skill-learning-delete <slug>` to remove one and forget the observations that
produced it, and `skill-learning-disable-all` to opt out locally. Use
`skill-learning-enable-all` to opt back in. Config can also set
`skill_learning.enabled: false`; `ATTELER_SKILL_LEARNING=false` disables it for
a process. `ATTELER_SKILL_LEARNING_DIR` and
`ATTELER_SKILL_LEARNING_SKILL_DIR` can point state and generated skills at
temporary or profile-specific directories.

`atteler worktrees run` creates an isolated git worktree for a session with an
ownership manifest under `.git/atteler/worktrees/`. Worktrees are preserved on
session exit by default; merge with `atteler worktrees merge <session-id>` after
review. Exit-time auto-merge is opt-in via the worktree auto-merge flag or
`worktree.auto_merge: true`, and it must have passing verification commands from
flag/config; the merge override flag (or
`worktree.override_verification: true`) permits an explicit no-verification
merge only when no verification commands are supplied. Successful merge output
includes the diff summary, verification commands run, resulting commit SHA,
transaction log, and rollback commands. The base worktree must be clean, a
dry-run merge must pass, and failed merges preserve the branch/worktree with
recovery commands. See `atteler help worktrees` for the current command
contract.

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

## Go package context migration notes

Library APIs that can touch credential stores, refresh OAuth tokens, start
local processes, or call embedding/model endpoints require caller-provided
contexts. Compatibility helpers without a `Context` suffix remain only to avoid
source breaks and return a context-required error before doing blocking work.

Migrate SDK-style callers as follows:

- `llm.ResolveAnthropicKey()` → `llm.ResolveAnthropicKeyContext(ctx)`
- `llm.ResolveOpenAIKey()` → `llm.ResolveOpenAIKeyContext(ctx)`
- `llm.New*Provider(...)` / `llm.AutoRegisterWithConfig(...)` →
  the matching `*Context(ctx, ...)` variant.
- `(*vector.EmbeddingVectorizer).Vectorize(text)` →
  `VectorizeContext(ctx, text)`.
- `worktree.Create`, `Merge`, `Remove`, `List`, and `IsGitRepo` →
  their `Context` variants.
- Process-backed helpers such as shell execution, plugin entrypoints, MCP
  calls, LSP lookups, sub-agent spawning, hooks, and Symphony app-server calls
  already require `context.Context`; pass the caller's context through instead
  of constructing a new root. Nil or already-canceled contexts are rejected
  before process launch, protocol writes, or later orchestration rounds.

`cmd/atteler` and `cmd/symphony` create process-root contexts at startup and
pass them down; package code should propagate those contexts instead of calling
`context.Background()`, `context.TODO()`, or `context.WithoutCancel()`.

## Streaming completion contract

`llm.StreamProvider` implementations deliver `llm.Chunk` events. A stream must
finish with exactly one terminal event:

- success: `Chunk{Done: true}` with optional usage, model, tool-call, and
  `StopReason` metadata.
- failure: `Chunk{Err: err}` for provider failures or context cancellation
  after the stream has started.

Channel close by itself is not success. `llm.CollectStream` returns
`(*Response, error)` and reports `llm.ErrStreamIncomplete` when a channel closes
without a successful final chunk, so callers can keep any partial content while
still treating the response as failed. Rendering callers should stop on either
terminal form and surface `Err` instead of assuming a quiet close means the
model completed.

Provider adapters should use `llm.DefaultStreamBuffer` (or an unbuffered
channel) and select on the caller's context when sending chunks. Avoid
unbounded queues: a slow renderer should apply backpressure instead of allowing
tokens to accumulate in memory. Callers that stop reading early should cancel
the stream context so provider goroutines can close network bodies promptly.
Codex and Ollama expose native streaming adapters; other providers use
`StreamFromComplete` unless they implement `StreamProvider`.

## Evidence-backed feature map

This section is intentionally small and evidence-linked. Add new completed
claims only when the implementation, tests, docs, or release artifact can be
linked from the row.

| Stable capability | Evidence |
| --- | --- |
| CLI command routing, grouped help, and compatibility flags | [`cmd/atteler/cli_args.go`](cmd/atteler/cli_args.go), [`cmd/atteler/cli_help_domains.go`](cmd/atteler/cli_help_domains.go), [`cmd/atteler/cli_args_test.go`](cmd/atteler/cli_args_test.go), [`cmd/atteler/cli_help_test.go`](cmd/atteler/cli_help_test.go) |
| Error-aware streaming completion contract with bounded-buffer guidance | [`pkg/llm/stream.go`](pkg/llm/stream.go), [`pkg/llm/stream_test.go`](pkg/llm/stream_test.go), [`pkg/llm/codex.go`](pkg/llm/codex.go), [`pkg/llm/codex_test.go`](pkg/llm/codex_test.go), [`pkg/llm/ollama.go`](pkg/llm/ollama.go), [`pkg/llm/ollama_test.go`](pkg/llm/ollama_test.go) |
| OpenAI, OpenAI-compatible endpoints, Anthropic, Codex, Claude Code, and Ollama providers | [`pkg/llm/openai.go`](pkg/llm/openai.go), [`pkg/llm/openai_test.go`](pkg/llm/openai_test.go), [`pkg/llm/anthropic.go`](pkg/llm/anthropic.go), [`pkg/llm/anthropic_test.go`](pkg/llm/anthropic_test.go), [`pkg/llm/codex.go`](pkg/llm/codex.go), [`pkg/llm/codex_test.go`](pkg/llm/codex_test.go), [`pkg/llm/claude_code.go`](pkg/llm/claude_code.go), [`pkg/llm/claude_code_test.go`](pkg/llm/claude_code_test.go), [`pkg/llm/ollama.go`](pkg/llm/ollama.go), [`pkg/llm/ollama_test.go`](pkg/llm/ollama_test.go), [`pkg/llm/capabilities.go`](pkg/llm/capabilities.go), [`pkg/llm/provider_contract_test.go`](pkg/llm/provider_contract_test.go), [`pkg/llm/provider_runtime.go`](pkg/llm/provider_runtime.go), [`pkg/llm/provider_runtime_test.go`](pkg/llm/provider_runtime_test.go) |
| Typed provider errors, jittered retry budgets, retry lifecycle events, and provider retry diagnostics | [`pkg/llm/provider_error.go`](pkg/llm/provider_error.go), [`pkg/llm/provider_error_test.go`](pkg/llm/provider_error_test.go), [`pkg/llm/retry.go`](pkg/llm/retry.go), [`pkg/llm/retry_test.go`](pkg/llm/retry_test.go), [`pkg/events/events.go`](pkg/events/events.go), [`cmd/atteler/cli_config_worktree_commands.go`](cmd/atteler/cli_config_worktree_commands.go) |
| Evidence-backed model routing with catalog metadata, model roles, per-agent policy, route-decision artifacts, agent-loop checkpoint metadata, and usage telemetry | [`pkg/modelroute/catalog.go`](pkg/modelroute/catalog.go), [`pkg/modelroute/decision.go`](pkg/modelroute/decision.go), [`pkg/modelroute/telemetry.go`](pkg/modelroute/telemetry.go), [`pkg/modelroute/modelroute_test.go`](pkg/modelroute/modelroute_test.go), [`pkg/llm/model_role.go`](pkg/llm/model_role.go), [`pkg/llm/llm.go`](pkg/llm/llm.go), [`pkg/llm/agentloop_checkpoint.go`](pkg/llm/agentloop_checkpoint.go), [`pkg/llm/agentloop_test.go`](pkg/llm/agentloop_test.go), [`cmd/atteler/route_decision_event.go`](cmd/atteler/route_decision_event.go), [`cmd/atteler/agent_resolution_test.go`](cmd/atteler/agent_resolution_test.go) |
| Configuration loading, migration, redacted diagnostics, atomic state, harness import, templates, and validation | [`pkg/config/config.go`](pkg/config/config.go), [`pkg/config/config_test.go`](pkg/config/config_test.go), [`pkg/config/migrate.go`](pkg/config/migrate.go), [`pkg/config/migrate_test.go`](pkg/config/migrate_test.go), [`pkg/config/diagnostics.go`](pkg/config/diagnostics.go), [`pkg/config/diagnostics_test.go`](pkg/config/diagnostics_test.go), [`pkg/config/redaction.go`](pkg/config/redaction.go), [`pkg/config/state.go`](pkg/config/state.go), [`pkg/config/state_test.go`](pkg/config/state_test.go), [`pkg/config/harness.go`](pkg/config/harness.go), [`pkg/config/harness_test.go`](pkg/config/harness_test.go), [`pkg/config/template.go`](pkg/config/template.go), [`pkg/config/template_test.go`](pkg/config/template_test.go) |
| Sessions, transcript search/export, evaluations, failures, provenance-rich artifacts, multi-agent run audits, and performance summaries | [`pkg/session/session.go`](pkg/session/session.go), [`pkg/session/session_test.go`](pkg/session/session_test.go), [`pkg/session/export.go`](pkg/session/export.go), [`pkg/session/export_test.go`](pkg/session/export_test.go), [`cmd/atteler/multi_agent_run_commands.go`](cmd/atteler/multi_agent_run_commands.go), [`cmd/atteler/multi_agent_run_commands_test.go`](cmd/atteler/multi_agent_run_commands_test.go), [`pkg/session/search.go`](pkg/session/search.go), [`pkg/session/search_test.go`](pkg/session/search_test.go), [`pkg/artifactmerge/artifactmerge.go`](pkg/artifactmerge/artifactmerge.go), [`pkg/artifactmerge/artifactmerge_test.go`](pkg/artifactmerge/artifactmerge_test.go), [`pkg/session/performance.go`](pkg/session/performance.go), [`pkg/session/performance_test.go`](pkg/session/performance_test.go) |
| Behavior-oriented eval assertions and machine-readable eval reports | [`pkg/eval/eval.go`](pkg/eval/eval.go), [`pkg/eval/structured.go`](pkg/eval/structured.go), [`pkg/eval/structured_test.go`](pkg/eval/structured_test.go), [`cmd/atteler/cli_agent_eval_feedback_route_commands.go`](cmd/atteler/cli_agent_eval_feedback_route_commands.go), [`cmd/atteler/main_test.go`](cmd/atteler/main_test.go) |
| Bounded and policy-gated context references for local files, directories, globs, and remote URLs | [`pkg/contextref/references.go`](pkg/contextref/references.go), [`pkg/contextref/references_test.go`](pkg/contextref/references_test.go), [`pkg/contextref/contextref.go`](pkg/contextref/contextref.go), [`pkg/contextref/contextref_test.go`](pkg/contextref/contextref_test.go) |
| Agent metadata, matching, orchestration planning, bounded async waves, and auditable sub-agent fan-out | [`pkg/agent/agent.go`](pkg/agent/agent.go), [`pkg/agent/orchestration.go`](pkg/agent/orchestration.go), [`pkg/agent/orchestration_test.go`](pkg/agent/orchestration_test.go), [`pkg/async/plan.go`](pkg/async/plan.go), [`pkg/async/run_options.go`](pkg/async/run_options.go), [`pkg/async/plan_test.go`](pkg/async/plan_test.go), [`pkg/subagent/subagent.go`](pkg/subagent/subagent.go), [`pkg/subagent/subagent_test.go`](pkg/subagent/subagent_test.go), [`cmd/atteler/cli_async_commands.go`](cmd/atteler/cli_async_commands.go), [`cmd/atteler/cli_shell_spawn_commands.go`](cmd/atteler/cli_shell_spawn_commands.go), [`test/e2e/cli_test.go`](test/e2e/cli_test.go) |
| Skill synthesis into reviewable `SKILL.md` directories with trigger eval fixtures | [`pkg/skill/suggestion.go`](pkg/skill/suggestion.go), [`pkg/skill/persist.go`](pkg/skill/persist.go), [`pkg/skill/trigger.go`](pkg/skill/trigger.go), [`pkg/skill/suggestion_test.go`](pkg/skill/suggestion_test.go), [`test/e2e/cli_test.go`](test/e2e/cli_test.go) |
| Automatic recurring-workflow skill learning with redacted observations, generated-skill revisions, relevant future context injection, and management/opt-out controls | [`pkg/skill/learning.go`](pkg/skill/learning.go), [`pkg/skill/learning_test.go`](pkg/skill/learning_test.go), [`pkg/events/events.go`](pkg/events/events.go), [`cmd/atteler/skill_learning_setup.go`](cmd/atteler/skill_learning_setup.go), [`cmd/atteler/cli_skill_learning_commands.go`](cmd/atteler/cli_skill_learning_commands.go), [`cmd/atteler/cli_skill_learning_commands_test.go`](cmd/atteler/cli_skill_learning_commands_test.go) |
| Speculative and review-agent planning/execution with durable audit artifacts | [`pkg/speculate/speculate.go`](pkg/speculate/speculate.go), [`pkg/speculate/speculate_test.go`](pkg/speculate/speculate_test.go), [`pkg/review/review.go`](pkg/review/review.go), [`pkg/review/review_test.go`](pkg/review/review_test.go), [`pkg/review/llm.go`](pkg/review/llm.go), [`pkg/review/llm_test.go`](pkg/review/llm_test.go), [`cmd/atteler/cli_speculate_watch_history_commands.go`](cmd/atteler/cli_speculate_watch_history_commands.go), [`cmd/atteler/cli_review_async_task_commands.go`](cmd/atteler/cli_review_async_task_commands.go), [`cmd/atteler/multi_agent_run_commands.go`](cmd/atteler/multi_agent_run_commands.go), [`cmd/atteler/multi_agent_run_commands_test.go`](cmd/atteler/multi_agent_run_commands_test.go) |
| Memory/retrieval, unified retrieval contract, per-agent memory, local lexical/embedding vector search, workspace ANN vector context, git-history search, multi-language incremental code intelligence, import graphs, structured code-intel CLI output, and optional managed LSP lookups | [`pkg/retrieval/types.go`](pkg/retrieval/types.go), [`pkg/retrieval/search.go`](pkg/retrieval/search.go), [`pkg/retrieval/retrieval_test.go`](pkg/retrieval/retrieval_test.go), [`pkg/memory/memory.go`](pkg/memory/memory.go), [`pkg/memory/memory_test.go`](pkg/memory/memory_test.go), [`pkg/agentmemory/agentmemory.go`](pkg/agentmemory/agentmemory.go), [`pkg/agentmemory/agentmemory_test.go`](pkg/agentmemory/agentmemory_test.go), [`pkg/vector/vector.go`](pkg/vector/vector.go), [`pkg/vector/vector_test.go`](pkg/vector/vector_test.go), [`pkg/vector/index.go`](pkg/vector/index.go), [`pkg/vector/index_test.go`](pkg/vector/index_test.go), [`pkg/vector/source_index.go`](pkg/vector/source_index.go), [`pkg/vector/ann.go`](pkg/vector/ann.go), [`pkg/vector/ann_test.go`](pkg/vector/ann_test.go), [`pkg/vector/bench_test.go`](pkg/vector/bench_test.go), [`pkg/vector/testdata/retrieval_quality.json`](pkg/vector/testdata/retrieval_quality.json), [`pkg/vector/workspace.go`](pkg/vector/workspace.go), [`pkg/vector/workspace_test.go`](pkg/vector/workspace_test.go), [`pkg/vector/index_searcher.go`](pkg/vector/index_searcher.go), [`pkg/vector/index_searcher_test.go`](pkg/vector/index_searcher_test.go), [`cmd/atteler/workspace_vector_context.go`](cmd/atteler/workspace_vector_context.go), [`cmd/atteler/workspace_vector_context_test.go`](cmd/atteler/workspace_vector_context_test.go), [`cmd/atteler/cli_vector_search_commands.go`](cmd/atteler/cli_vector_search_commands.go), [`cmd/atteler/cli_vector_search_commands_test.go`](cmd/atteler/cli_vector_search_commands_test.go), [`pkg/githistory/githistory.go`](pkg/githistory/githistory.go), [`pkg/githistory/githistory_test.go`](pkg/githistory/githistory_test.go), [`pkg/codeintel/model.go`](pkg/codeintel/model.go), [`pkg/codeintel/codeintel.go`](pkg/codeintel/codeintel.go), [`pkg/codeintel/persistence.go`](pkg/codeintel/persistence.go), [`pkg/codeintel/query.go`](pkg/codeintel/query.go), [`pkg/codeintel/workspace.go`](pkg/codeintel/workspace.go), [`pkg/codeintel/codeintel_test.go`](pkg/codeintel/codeintel_test.go), [`pkg/codegraph/codegraph.go`](pkg/codegraph/codegraph.go), [`pkg/codegraph/codegraph_test.go`](pkg/codegraph/codegraph_test.go), [`cmd/atteler/codeintel_schema.go`](cmd/atteler/codeintel_schema.go), [`cmd/atteler/codeintel_response_render.go`](cmd/atteler/codeintel_response_render.go), [`cmd/atteler/codeintel_command_descriptors.go`](cmd/atteler/codeintel_command_descriptors.go), [`cmd/atteler/codeintel_schema_test.go`](cmd/atteler/codeintel_schema_test.go), [`pkg/lsp/client.go`](pkg/lsp/client.go), [`pkg/lsp/client_test.go`](pkg/lsp/client_test.go) |
| Governed plugin manifests, lockfiles, structured entrypoint output, policy-gated local execution, MCP manifest validation, lifecycle-negotiated sessions, discovered tool calls, and resource/prompt discovery | [`pkg/plugin/manifest.go`](pkg/plugin/manifest.go), [`pkg/plugin/manifest_test.go`](pkg/plugin/manifest_test.go), [`pkg/plugin/policy.go`](pkg/plugin/policy.go), [`pkg/plugin/lockfile.go`](pkg/plugin/lockfile.go), [`pkg/plugin/registry.go`](pkg/plugin/registry.go), [`pkg/plugin/registry_test.go`](pkg/plugin/registry_test.go), [`pkg/plugin/run.go`](pkg/plugin/run.go), [`pkg/plugin/run_test.go`](pkg/plugin/run_test.go), [`pkg/permission/permission.go`](pkg/permission/permission.go), [`pkg/permission/permission_test.go`](pkg/permission/permission_test.go), [`pkg/mcp/manifest.go`](pkg/mcp/manifest.go), [`pkg/mcp/manifest_test.go`](pkg/mcp/manifest_test.go), [`pkg/mcp/client.go`](pkg/mcp/client.go), [`pkg/mcp/pool.go`](pkg/mcp/pool.go), [`pkg/mcp/process_unix.go`](pkg/mcp/process_unix.go), [`pkg/mcp/process_windows.go`](pkg/mcp/process_windows.go), [`pkg/mcp/client_test.go`](pkg/mcp/client_test.go), [`cmd/atteler/cli_plugin_commands.go`](cmd/atteler/cli_plugin_commands.go), [`cmd/atteler/cli_mcp_commands.go`](cmd/atteler/cli_mcp_commands.go) |
| Background repository scanning, baseline/gate comparisons, suppressions, issue upserts, and review-scan formatting | [`pkg/watch/watch.go`](pkg/watch/watch.go), [`pkg/watch/baseline.go`](pkg/watch/baseline.go), [`pkg/watch/issues.go`](pkg/watch/issues.go), [`pkg/watch/watch_test.go`](pkg/watch/watch_test.go), [`pkg/symphony/tracker.go`](pkg/symphony/tracker.go), [`cmd/atteler/cli_speculate_watch_history_commands.go`](cmd/atteler/cli_speculate_watch_history_commands.go), [`cmd/atteler/cli_review_async_task_commands.go`](cmd/atteler/cli_review_async_task_commands.go) |
| Event hook privacy, metadata, and local hook execution | [`pkg/events/events.go`](pkg/events/events.go), [`pkg/events/events_test.go`](pkg/events/events_test.go), [`pkg/events/privacy.go`](pkg/events/privacy.go), [`pkg/events/privacy_test.go`](pkg/events/privacy_test.go), [`pkg/events/logger.go`](pkg/events/logger.go), [`pkg/events/discoverability_test.go`](pkg/events/discoverability_test.go) |
| Automatic worktree isolation | [`pkg/worktree/worktree.go`](pkg/worktree/worktree.go), [`pkg/worktree/worktree_test.go`](pkg/worktree/worktree_test.go), [`cmd/atteler/cli_worktree_commands.go`](cmd/atteler/cli_worktree_commands.go) |
| Symphony issue scheduler | [`cmd/symphony/main.go`](cmd/symphony/main.go), [`pkg/symphony/workflow.go`](pkg/symphony/workflow.go), [`pkg/symphony/workflow_test.go`](pkg/symphony/workflow_test.go), [`pkg/symphony/orchestrator.go`](pkg/symphony/orchestrator.go), [`pkg/symphony/orchestrator_test.go`](pkg/symphony/orchestrator_test.go), [`docs/symphony.md`](docs/symphony.md), [`WORKFLOW.md`](WORKFLOW.md) |
| Build, CI, and release packaging | [`Makefile`](Makefile), [`.github/workflows/ci.yml`](.github/workflows/ci.yml), [`.github/workflows/release.yml`](.github/workflows/release.yml), [`.goreleaser.yaml`](.goreleaser.yaml) |

## Explicit non-claims

The repository has reusable Go packages, but this README does not promise a
separately versioned public SDK contract. The current code-intelligence support
uses Go package loading plus a lightweight Python scanner in the shared
incremental index, with optional managed LSP lookups; tree-sitter support would
be future work and is not documented as implemented here. Native provider
adapters beyond the providers linked above and configurable OpenAI-compatible
endpoints should be tracked as GitHub Issues until code and tests exist.

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

Release checklist:

- When a provider implementation changes its execution path, credential source,
  token refresh, endpoint, sandbox/tool boundary, health check, or built-in
  model catalog, update
  [`pkg/llm/provider_runtime.go`](pkg/llm/provider_runtime.go) and refresh the
  generated README provider block before tagging with
  `UPDATE_PROVIDER_RUNTIME_DOCS=1 go test ./pkg/llm -run TestProviderRuntimeDocs_ReadmeSectionMatchesMetadata`.
  `go test ./pkg/llm` verifies that the README block still matches metadata
  keyed to the provider inventory.

## License

See [`LICENSE`](LICENSE).
