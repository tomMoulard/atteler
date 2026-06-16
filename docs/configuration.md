# Configuration

> Layered YAML/JSON configuration, generation knobs, and the routing/agent schema.

Atteler runs without any config file. When you want to pin models, define agents,
or tune routing, drop a YAML (or JSON) file in one of the locations below.

To discover what you can set, start from a generated template and the resolved
search paths:

```sh
atteler config template          # print starter YAML (also: --print-config-template)
atteler config paths             # show search paths (also: --list-config-paths)
atteler config validate          # strict schema gate
atteler config explain default_model   # trace where a value came from
```

## Load order & precedence

Layers load lowest precedence first; later layers override earlier ones:

1. **Imported sibling-harness defaults** — best-effort imports from local coding
   harnesses (Codex, Claude Code, OpenCode, Forge). Lowest precedence; visible
   via `atteler config explain`.
2. **Global config** — `$XDG_CONFIG_HOME/atteler/config.{yaml,yml,json}` (or
   `~/.config/atteler/...`).
3. **Project config** — `./.atteler/config.{yaml,yml,json}`, then
   `./.atteler.{yaml,yml,json}`.
4. **`ATTELER_CONFIG` / `--config`** — extra files (platform path-list
   separator); highest-precedence config files.
5. **Runtime choices** — persisted state, CLI flags (`--model`, generation
   overrides), and runtime agent/model selection.

Provider and agent maps **merge by name**, and fields inside the same provider or
agent override independently. Lists **replace in full** when set in a later layer
(`fallback_models`, `context.references`, hook lists, `plugins.paths`, per-agent
`tools`).

## The YAML schema

A representative file. Run `atteler config template` for the full annotated
version.

```yaml
version: 1

default_provider: openai
default_model: gpt-4.1-mini
fallback_models: ["gpt-4.1", "gpt-4.1-nano"]
autonomy: medium

model_aliases:
  fast: openai/gpt-4.1-mini

# Roles keep task intent separate from concrete models; the router scores the
# preferred model plus fallbacks against capabilities, budget, and availability.
models:
  planner:
    preferred: openai/gpt-4.1
    fallback: openai/gpt-4.1-mini
    required_capabilities: ["tools", "json_schema"]
    max_cost_usd: 0.25
    max_latency_ms: 2500
```

Bare model names resolve only by exact provider-catalog claim, a `model_aliases`
entry, or a `models.<role>` entry. If two providers claim the same bare name,
Atteler reports the collision unless `default_provider`/`default_model` or a role
routing policy makes the choice deterministic.

### Providers

Built-in providers (`anthropic`, `claude-code`, `codex`, `openai`, `ollama`)
need no options at all — `anthropic: {}` is valid. Every key below is an
optional override. Custom endpoints use `type: openai_compatible` (or an alias
such as `groq`, `vllm`, `azure_openai`). See [Providers](providers.md) for the
full runtime, credential, and trust-boundary details.

#### Full provider example (with defaults)

Copy-pastable, with every key set to its default value:

```yaml
providers:
  openai:                                       # any provider name
    disabled: false                             # default: false
    local: false                                # default: false
    auto_start: false                           # default: false (Ollama only)
    disable_private_adapter: false              # default: false (codex/claude-code only)
    base_url: https://api.openai.com            # default: provider-specific
    type: ""                                    # default: "" (built-ins need no type)
    api_key_env: OPENAI_API_KEY                 # default: provider-specific
    api_key_header: Authorization               # default: Authorization
    api_key_scheme: Bearer                      # default: Bearer
    chat_completions_path: /v1/chat/completions # default: /v1/chat/completions
    embeddings_path: /v1/embeddings             # default: /v1/embeddings
    models_path: /v1/models                     # default: /v1/models
    api_version: ""                             # default: "" (none)
    models: []                                  # default: [] (use built-in catalog)
    capabilities: []                            # default: [] (inferred from provider)
    timeout_seconds: 120                        # default: 120
    retry:
      max_attempts: 2                           # default: 2 (0 disables)
      initial_backoff_ms: 1000                  # default: 1000
      max_backoff_ms: 10000                     # default: 10000
      max_elapsed_ms: 30000                     # default: 30000
      jitter_fraction: 0.2                      # default: 0.2
```

#### `disabled`

Turn this provider off entirely — it is neither constructed nor routed to.

- **Default:** `false`

```yaml
providers:
  ollama:
    disabled: true
```

#### `local`

Mark the endpoint as local so `prefer_local` model roles favour it. A loopback
`base_url` implies this too.

- **Default:** `false`

```yaml
providers:
  vllm:
    local: true
```

#### `auto_start`

Ollama only: let Atteler run `ollama serve` automatically for a loopback base
URL before its first request.

- **Default:** `false`

```yaml
providers:
  ollama:
    auto_start: true
```

#### `disable_private_adapter`

Disable the borrowed-credential adapter for `codex`/`claude-code` without
disabling the provider. Equivalent to the `ATTELER_DISABLE_*_ADAPTER` env vars.

- **Default:** `false`

```yaml
providers:
  codex:
    disable_private_adapter: true
```

#### `base_url`

Override the provider endpoint URL.

- **Default:** provider-specific (`openai` → `https://api.openai.com`,
  `ollama` → `http://127.0.0.1:11434`; required for custom endpoints).

```yaml
providers:
  openai:
    base_url: https://api.openai.com
```

#### `type`

Provider type for custom endpoints: `openai_compatible`, or an alias
(`groq`, `vllm`, `azure_openai`). Built-in providers need no `type`.

- **Default:** `""` (none)

```yaml
providers:
  groq:
    type: openai_compatible
    base_url: https://api.groq.com/openai/v1
    api_key_env: GROQ_API_KEY
```

#### `api_key_env`

Name of the environment variable holding the API key.

- **Default:** provider-specific (e.g. `OPENAI_API_KEY`, `ANTHROPIC_API_KEY`).

```yaml
providers:
  groq:
    api_key_env: GROQ_API_KEY
```

#### `api_key_header`

HTTP header that carries the API key.

- **Default:** `Authorization`

```yaml
providers:
  azure:
    api_key_header: api-key
```

#### `api_key_scheme`

Auth scheme/prefix prepended to the key in the header.

- **Default:** `Bearer`

```yaml
providers:
  azure:
    api_key_scheme: ""   # send the raw key with no prefix
```

#### `chat_completions_path`

Request path for chat completions on custom endpoints.

- **Default:** `/v1/chat/completions`

```yaml
providers:
  custom:
    chat_completions_path: /openai/v1/chat/completions
```

#### `embeddings_path`

Request path for embeddings on custom endpoints.

- **Default:** `/v1/embeddings`

```yaml
providers:
  custom:
    embeddings_path: /openai/v1/embeddings
```

#### `models_path`

Request path for model listing / health checks on custom endpoints.

- **Default:** `/v1/models`

```yaml
providers:
  custom:
    models_path: /openai/v1/models
```

#### `api_version`

API version sent as a query parameter or header (e.g. Azure OpenAI).

- **Default:** `""` (none)

```yaml
providers:
  azure:
    api_version: "2024-06-01"
```

#### `models`

Explicit model IDs this provider serves. Overrides the built-in catalog.

- **Default:** `[]` (use the built-in catalog)

```yaml
providers:
  groq:
    models: ["llama-3.3-70b-versatile"]
```

#### `capabilities`

Capability tags (`chat`, `tools`, `json_schema`, `local`, …) used by routing to
match roles and reject unsupported knobs.

- **Default:** `[]` (inferred from the provider)

```yaml
providers:
  groq:
    capabilities: ["chat", "tools", "json_schema"]
```

#### `timeout_seconds`

Per-request HTTP timeout, in seconds.

- **Default:** `120`

```yaml
providers:
  ollama:
    timeout_seconds: 300
```

#### `retry`

Retry policy for transient (429/5xx) responses.

- **Defaults:** `max_attempts: 2` (extra retries after the first request; `0`
  disables), `initial_backoff_ms: 1000`, `max_backoff_ms: 10000`,
  `max_elapsed_ms: 30000`, `jitter_fraction: 0.2`.

```yaml
providers:
  openai:
    retry:
      max_attempts: 4
      initial_backoff_ms: 500
      max_backoff_ms: 20000
      max_elapsed_ms: 60000
      jitter_fraction: 0.3
```

### Agents

Agents are defined under `agents.<name>:`. Every key is optional — an agent can
be as small as a `description` and a `system_prompt`. Generation knobs
(`temperature`, `top_p`, `seed`, `model_mode`, `reasoning_level`, `max_tokens`)
set here override the global `generation:` block for this agent, and CLI flags
override both. An agent's `routing_policy` layers on top of any model-role
constraints, so per-agent provider bans and budget caps still apply.

#### Full agent example (with defaults)

Copy-pastable, with every key shown. Most keys have no built-in default; the
values below are illustrative, and a comment marks the true default where one
exists:

```yaml
agents:
  reviewer:                                  # any agent name
    description: Code review specialist      # default: "" (none)
    model: gpt-4.1-mini                       # default: "" (use default_model/routing)
    fallback_models: ["gpt-4.1-nano"]         # default: [] (use global fallback_models)
    mode: ""                                  # default: "" (none)
    model_mode: default                       # default: inherits generation (default)
    reasoning_level: high                     # default: inherits generation
    temperature: 0                            # default: inherits generation
    top_p: 1                                  # default: inherits generation
    seed: 1                                   # default: inherits generation
    max_tokens: 2048                          # default: inherits generation
    system_prompt: You are a concise code reviewer.  # default: "" (none)
    personality: ""                           # default: "" (none)
    triggers: ["review this", "code review"]  # default: [] (none)
    capabilities: ["review", "security"]      # default: [] (none)
    references: ["docs/style.md"]             # default: [] (none)
    tools:                                    # default: {} (all tools allowed)
      bash: false
    hidden: false                             # default: false
    routing_policy:                           # default: none
      preferred_providers: ["openai"]         # default: [] (none)
      banned_providers: ["ollama"]            # default: [] (none)
      banned_models: []                       # default: [] (none)
      required_capabilities: ["tools"]        # default: [] (none)
      max_budget: 0.25                        # default: 0 (no cap)
      max_latency_ms: 0                       # default: 0 (no cap)
      max_ttft_ms: 0                          # default: 0 (no cap)
      require_fresh_metadata: false           # default: false
```

#### `description`

Human-readable summary shown in `atteler agents list`.

- **Default:** `""` (none)

```yaml
agents:
  reviewer:
    description: Code review specialist
```

#### `model`

Preferred model for this agent.

- **Default:** `""` (falls back to `default_model` / role routing)

```yaml
agents:
  reviewer:
    model: gpt-4.1-mini
```

#### `fallback_models`

Ordered fallbacks tried if the preferred model is unavailable.

- **Default:** `[]` (uses the global `fallback_models`)

```yaml
agents:
  reviewer:
    fallback_models: ["gpt-4.1-nano"]
```

#### `mode`

Agent execution mode.

- **Default:** `""` (none)

```yaml
agents:
  reviewer:
    mode: ""
```

#### `model_mode`

`default` or `fast` (`fast` maps to the priority service tier for the
OpenAI-family providers).

- **Default:** inherits the global `generation.model_mode` (`default`)

```yaml
agents:
  reviewer:
    model_mode: fast
```

#### `reasoning_level`

Reasoning effort: `low`, `medium`, or `high`.

- **Default:** inherits the global `generation.reasoning_level`

```yaml
agents:
  reviewer:
    reasoning_level: high
```

#### `temperature`, `top_p`, `seed`, `max_tokens`

Per-agent generation knobs. Omitted values are not sent to the provider;
`temperature: 0` and `seed: 1` are explicit deterministic choices.

- **Default:** inherits the global `generation:` block

```yaml
agents:
  reviewer:
    temperature: 0
    top_p: 1
    seed: 1
    max_tokens: 2048
```

#### `system_prompt`

The agent's system prompt. (`prompt` is a **deprecated** alias.)

- **Default:** `""` (none)

```yaml
agents:
  reviewer:
    system_prompt: >
      You are a concise code reviewer.
```

#### `personality`

Optional persona/style modifier layered onto the prompt.

- **Default:** `""` (none)

```yaml
agents:
  reviewer:
    personality: terse and direct
```

#### `triggers`

Phrases that route to this agent via `@agent` mention or auto-selection.

- **Default:** `[]` (none)

```yaml
agents:
  reviewer:
    triggers: ["review this", "code review"]
```

#### `capabilities`

Capability tags this agent requires/advertises for routing.

- **Default:** `[]` (none)

```yaml
agents:
  reviewer:
    capabilities: ["review", "security"]
```

#### `references`

Default `@path` context references automatically attached when this agent runs.

- **Default:** `[]` (none)

```yaml
agents:
  reviewer:
    references: ["docs/style.md", "CONTRIBUTING.md"]
```

#### `tools`

Per-tool allow/deny map, keyed by tool name. Unlisted tools follow the global
permission policy.

- **Default:** `{}` (all tools allowed by policy)

```yaml
agents:
  reviewer:
    tools:
      bash: false      # deny shell for this agent
      read: true
```

#### `hidden`

Hide the agent from listings and selection UIs.

- **Default:** `false`

```yaml
agents:
  internal-helper:
    hidden: true
```

#### `routing_policy`

Per-agent routing constraints, layered on top of model-role constraints. Sub-keys:

- `preferred_providers` (`[]`) — providers to try first.
- `banned_providers` (`[]`) — providers this agent must never use.
- `banned_models` (`[]`) — models this agent must never use.
- `required_capabilities` (`[]`) — capabilities a candidate model must advertise.
- `max_budget` (`0` = no cap) — max estimated cost (USD) per request.
- `max_latency_ms` (`0` = no cap) — reject candidates slower than this.
- `max_ttft_ms` (`0` = no cap) — reject on time-to-first-token over this.
- `require_fresh_metadata` (`false`) — require live (not static-fallback) catalog metadata.

```yaml
agents:
  reviewer:
    routing_policy:
      preferred_providers: ["openai"]
      banned_providers: ["ollama"]
      required_capabilities: ["tools"]
      max_budget: 0.25
```

#### `feedback_guidance`

Auditable, feedback-derived prompt instructions. Normally written and managed by
the feedback system rather than by hand.

- **Default:** `[]` (none)

## Generation knobs

Generation settings layer: global `generation:` → per-agent → CLI overrides
(`--temperature`, `--top-p`, `--seed`, `--reasoning-level`, `--max-tokens`).
Omitted values are **not** sent to providers — `temperature: 0` and `seed: 1` are
explicit deterministic choices, not "unset".

```yaml
generation:
  temperature: 0
  top_p: 1
  seed: 1
  model_mode: default       # "fast" maps to service_tier=priority for OpenAI-family
  reasoning_level: medium
  max_tokens: 2048
```

### Agent-loop budgets

`agent_loop:` caps a single agent loop. `0` (or omitted) means no ceiling.
Ceilings are cumulative across the loop and **fail closed** for unpriced or
unmetered models when set.

```yaml
agent_loop:
  max_iterations: 0       # tool-use turns
  max_model_calls: 0
  max_tool_calls: 0
  max_cost_micros: 0      # 1000000 = 1.0 currency unit
  max_input_tokens: 0
  max_output_tokens: 0
  max_wall_time: "0"      # Go duration, e.g. "30m"
  checkpoint_interval: 0  # prompt to continue every N iterations
```

## Imported sibling-harness defaults

When present, Atteler imports defaults from `~/.codex/config.toml`,
`~/.claude/settings.json` / `~/.claude.json`, OpenCode config + agent markdown,
and Forge `.forge.toml`. OpenCode agents merge into the normal agent registry.
These are lowest precedence; `atteler config explain` shows each imported value's
source path and `harness-import` precedence, and surfaces importer warnings.

## Diagnostics

| Command | Use |
| --- | --- |
| `atteler config validate` | Strict schema gate; non-zero on parse/unknown-field/migration errors. |
| `atteler config doctor-offline` | Provider-independent readiness for CI; no network/credentials. JSON output supported. |
| `atteler config doctor` | Adds provider-aware credential and health checks. |
| `atteler config explain [prefix]` | Trace a field (e.g. `providers.openai`, `runtime.selected_model`). |
| `atteler config report` | Redacted YAML diagnostics bundle for issue reports. |
| `atteler config migrate` | Rewrite config + state files to the current schema. |

Read-only aliases such as `DEBUG_ATTELER_CONFIG_REPORT=1` and
`DEBUG_ATTELER_EXPLAIN_CONFIG_FIELD=providers.openai` trigger the same
inspection.

## Hooks

Lifecycle hooks are configured under top-level `hooks:` and run as local
subprocesses with least-privilege, schema-filtered payloads (`metadata` default,
`summary`, or `full`). See [Hooks](hooks.md) for event types, payload modes, and
privacy rules.

## Environment variables

- `ATTELER_CONFIG` — extra config paths.
- `ATTELER_SESSION_DIR` / `--session-dir` — session store location.
- `ATTELER_OLLAMA_AUTO_START=false` — disable Ollama auto-spawn.
- `OPENAI_BASE_URL` / `ANTHROPIC_BASE_URL` / `OLLAMA_BASE_URL` — per-provider URL overrides.
- `ATTELER_DISABLE_PRIVATE_ADAPTERS=1` — disable the `codex`/`claude-code` borrowed-credential adapters.

See the [CLI reference](cli-reference.md) for the full flag catalog.
