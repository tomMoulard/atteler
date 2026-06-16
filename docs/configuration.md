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
need no `type`. Custom endpoints use `type: openai_compatible` (or an alias such
as `groq`, `vllm`, `azure_openai`). See [Providers](providers.md) for the full
runtime, credential, and trust-boundary details.

```yaml
providers:
  openai:
    base_url: https://api.openai.com
    retry:
      max_attempts: 2          # extra retries after the first request; 0 disables
      initial_backoff_ms: 1000
      max_backoff_ms: 10000
  ollama:
    base_url: http://127.0.0.1:11434
    auto_start: false          # opt in before Atteler runs `ollama serve`
  groq:
    type: openai_compatible
    base_url: https://api.groq.com/openai/v1
    api_key_env: GROQ_API_KEY
    models: ["llama-3.3-70b-versatile"]
    capabilities: ["chat", "tools", "json_schema"]
```

Set `local: true` (or use a loopback URL) so `prefer_local` roles favour
self-hosted endpoints. Use `capabilities` to narrow route metadata and reject
unsupported knobs. The `codex` and `claude-code` private adapters borrow CLI
credentials; disable them with `disable_private_adapter: true` or the
`ATTELER_DISABLE_*_ADAPTER` env vars without disabling the normal providers.

### Agents

```yaml
agents:
  reviewer:
    description: Code review specialist
    model: gpt-4.1-mini
    fallback_models: ["gpt-4.1-nano"]
    triggers: ["review this", "code review"]
    reasoning_level: high
    temperature: 0
    routing_policy:
      preferred_providers: ["openai"]
      banned_providers: ["ollama"]
      required_capabilities: ["tools"]
      max_budget: 0.25
    system_prompt: >
      You are a concise code reviewer.
```

An agent's `routing_policy` layers on top of any role constraints, so per-agent
provider bans and budget caps still apply.

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
