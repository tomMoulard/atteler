# atteler

A Go program that can be run using `go run ./cmd/atteler/` or `go run github.com/tommoulard/atteler/cmd/atteler@latest`.

It's a one of a kind LLM harness that leverage multiple LLMs. Here is the list of features in no particular order, on top of already existing claude code features:

 - auto import configuration for other coding harnesses (e.g., claude code, codex, opencode, ...).
 - async all the way: all agents should run asynchronously and in parallel, and the system should be able to handle that (e.g., if an agent is waiting for a response from an LLM, it shouldn't block other agents from doing their work).
 - does feedback improvement of the defined agents. Once a task is completed, the system should do a retrospective of the execution and actually improve the agents that were involved in the execution of the task. For example, if an agent was not able to complete its task, the system should analyze why and improve the agent's prompt or tool usage accordingly. Keep an history of the changes made to the agents and the reasons for those changes. This way, the system can learn from its mistakes and improve over time.
 - agents can spawn other agents.
 - agents can have a memory (i.e., using a local vector database).
 - Speculative parallel execution: executing a task should spawn task exploration with multiple llm thinking at the same time, and doing 3 rounds of confrontation before executing the task. For example: it can be run with any number of agents (this should be configurable):
   - first round: each agent thinks independently and come up with a plan to execute the task.
   - second round: agent A review the proposition of agent B to improve it's own proposition, and vice versa.
   - third round: have an agent C review all propositions and aggregate to a final proposition. the gate should be structured: tests pass, types pass, lint pass, no new flakes, behavioral diff vs baseline, optionally a property check. The judge agent should be a tiebreaker, not the primary signal. The user's input can be gathered to have a referee role in choosing the best trajectory.
 - Skill synthesis. When the agent does the same multi-step thing twice, it proposes turning it into a named skill (parameterized prompt + tool sequence) and asks if you want to keep it. The toolset grows with use. Compounds across the team if you commit them.
 - sandbox: all the work done by any agent must be usable: research plans, design decisions, ADR, code. The system should be able to surface that work in a way that is easily accessible and usable by the user. For example, if an agent does some research on a topic, the system should be able to surface that research in a way that the user can easily access it and use it for their own work. Or a code change can be aggregated from the work of two agents that ran side by side. An agent can be specialized in doing code merges
 - agent registry: a way to register agents with their capabilities, personality, and other metadata. This registry can be used to find the right agent for a given task, or to find agents that can work together on a task.
 - agent orchestration: a way to orchestrate the work of multiple agents on a given task. For example, if a task requires both research and coding, the system should be able to orchestrate the work of a research agent and a coding agent to complete the task.
 - agent evaluation: a way to evaluate the performance of agents on a given task. This can be done by comparing the output of the agent to a reference output, or by using a human evaluator to assess the quality of the agent's work. The system should be able to use this evaluation to improve the agents over time.
 - local rag of files, git history, and other relevant data sources to provide agents with the information they need to complete their tasks. This local rag should be fast and efficient, and should be able to handle large amounts of data. Should be synched async.
 - Cost & model routing as a first-class layer. This is literally your job. A multi-LLM harness without smart routing leaves the biggest win on the floor. Per-agent model preference with fallback chains, per-task budget caps that hard-stop runaway loops, prompt-cache reuse across speculative branches (huge if branches share prefix), TTFT-aware routing for interactive vs batch agents. Bake it in from day one — retrofitting later is painful.
 - Negative knowledge. Memory of what didn't work. "We tried X, broke Y, here's the commit." Cheap to capture, massive value — most current harnesses re-suggest the same broken approach forever.
 - Determinism knobs. Seed, temperature-0 mode, response recording/replay. Without these your eval and self-improvement loops are measuring noise.
 - modular: anyone can bring in code, tools, agents, and prompts from anywhere. The system should be able to integrate with any existing codebase, tool, or agent, and should be able to use them in a way that is seamless and efficient. For example, if there is an existing agent that does a specific task well, the system should be able to integrate that agent into its workflow without requiring a lot of work to adapt it to the system. kind of plugins systems.
 - SDK first: the cli tool is "just" an interface built on top of a powerful SDK that can be used to build custom workflows, agents, and tools. The SDK should be well-documented and easy to use, and should provide a lot of flexibility for users to build their own custom solutions on top of the system.

Here is a more in depth list of features:

 - Support for multiple LLMs (OpenAI, Anthropic, Cohere, etc.)
 - CLI tool
 - agents can have a personality (e.g., "be more concise", "be more verbose", "be more creative", "be more logical", etc.); a temperature; a reasoning level.
 - agents can either be called directly (@{agent_name}) or indirectly (e.g., "review this" and it calls the reviewer agent).
 - sessions + replay
 - fast to open
 - treesitter + LSP
 - Vector DB for prose/ADRs, graph for code.
 - MCPs
 - smart context compression (i.e., the context that is sent to the llm can be compressed, but the full context should be kept in case some data gets distilled).
 - reference things using the `@` sign with auto complete.

## Agents ideas

### 1- Continuous background agent.

A separate loop that watches the repo independently of any active session — flagging perf regressions, dead code, missing tests, drift from conventions. Like a local CI that thinks. Surfaces work proactively rather than waiting for you to ask.

### 2- Review agent

Should work in the same maner as the Speculative parallel execution, but for code review.
I should/can also include specialized tool to do the review like coderabbit.

## TODO

 - [x] llm connection (claude code, codex)
 - [x] Claude Code provider exposes Bash for shell-command requests
 - [x] local Ollama provider for offline/local inference
 - [x] auto-start local Ollama daemon for selected local Ollama runs
 - [x] configuration loading
    - [x] general configuration
    - [x] local configuration
    - [x] other harness configurations
        - [x] codex
        - [x] claude code
        - [x] opencode
 - [x] sessions + replay
 - [x] config-backed agent registry and `@agent` invocation
 - [x] config-backed agent reasoning-level metadata
 - [x] CLI reasoning-level override
 - [x] response token usage summaries with cached-input accounting
 - [x] agent metadata, capabilities, and capability-backed prompt matching
 - [x] determinism seed knob and request input-token guardrails
 - [x] local plugin manifest discovery/validation
 - [x] local plugin entrypoint execution helper for SDK workflows
 - [x] negative-knowledge capture, search, show, and export
 - [x] agent evaluation capture, search, show, and export
 - [x] aggregate agent performance summaries across sessions
 - [x] sandbox artifact manifest capture, search, show, and export
 - [x] deterministic response recording/replay fixtures
 - [x] dependency-free agent orchestration planning
 - [x] CLI agent orchestration preview
 - [x] dependency-aware async agent task planning waves
 - [x] CLI dependency-aware async task execution by spawning sub-agents
 - [x] agent feedback improvement proposal primitives
 - [x] CLI feedback improvement proposal report
 - [x] persistent agent task/TODO list with add, assign, complete, and list commands
 - [x] CLI feedback proposal application to agent config plus history log
 - [x] cost/model routing primitives with budget, context, cache, and latency signals
 - [x] CLI cost/model routing preview
 - [x] CLI model-route budget hard-stop for one-shot/stdin requests
 - [x] smart context compression primitives with omission accounting
 - [x] CLI smart context compression preview
 - [x] MCP manifest validation and capability lookup primitives
 - [x] CLI MCP manifest validation and capability lookup
 - [x] MCP stdio JSON-RPC client invocation primitive and CLI tool/method call
 - [x] dependency-free evaluation helpers for agent outputs
 - [x] CLI eval check runner
 - [x] dependency-free local memory/RAG lexical index
 - [x] CLI memory indexing/search over files and saved sessions
 - [x] per-agent persistent vector memory
 - [x] CLI per-agent vector memory indexing/search
 - [x] CLI git history lexical search for local RAG
 - [x] CLI local vector search over indexed files
 - [x] CLI plugin describe, dry-run, and entrypoint execution
 - [x] skill synthesis suggestion primitive and CLI
 - [x] skill acceptance and markdown persistence
 - [x] interactive `@` completion for agents and local paths
 - [x] deterministic rest-of-line prompt completion primitive and CLI preview
 - [x] dependency-free Go code intelligence and import graph foundation
 - [x] LSP document-symbol code intelligence primitive and CLI
 - [x] LSP workspace-symbol lookup primitive and CLI
 - [x] CLI Go symbol lookup over the local repository
 - [x] CLI Go import-edge listing over the local repository
 - [x] CLI Go import impact lookup over the local repository
 - [x] dependency-free code graph traversal and impact analysis primitives
 - [x] dependency-free vector retrieval primitive
 - [x] concurrent sub-agent spawning primitive and CLI dry-run/runner
 - [x] speculative three-round execution planning primitives
 - [x] speculative three-round session runner primitives
 - [x] speculative prompt-cache prefix reuse estimates
 - [x] CLI speculative three-round execution plan preview
 - [x] structured review-agent report and gate-check primitives
 - [x] review-agent speculative plan preview
 - [x] review-agent three-round LLM execution pipeline
 - [x] CLI structured review scan report
 - [x] continuous background-agent repository scan primitives
 - [x] CLI background-agent repository scan
 - [x] CLI continuous background-agent watch loop
 - [x] background convention-drift scan for misplaced `context.Background()`
 - [x] explicit local bash command runner
 - [x] sandbox artifact merge aggregation
 - [x] CLI merged artifact markdown export
 - [x] context-aware command propagation with a single main entry context
 - [x] providerless local inspection commands avoid credential/network side effects
 - [x] `DEBUG_ATTELER_*` environment aliases for local debug/inspection flags
 - [x] CLI hook-event discovery
 - [x] CLI session inventory filtering by exact tag
 - [x] automatic git worktree isolation per session
 - [x] lifecycle event hooks with granular file/command/tool/agent activity events

## Configuration

`atteler` loads optional YAML configuration in this order; later files override
earlier files:

1. `$XDG_CONFIG_HOME/atteler/config.yaml` / `config.yml`, or
   `~/.config/atteler/config.yaml` / `config.yml`
2. `./.atteler/config.yaml` or `./.atteler/config.yml`
3. `./.atteler.yaml` or `./.atteler.yml`
4. Any paths listed in `ATTELER_CONFIG` (use the platform path-list separator)

Legacy `.json` config files are still accepted after the YAML candidates at the
same scope.

Bootstrap a YAML config:

```sh
atteler config template
atteler config init ~/.config/atteler/config.yaml
atteler config paths
atteler config validate
```

Example:

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

hooks:
  assistant_message:
    - command: ["./scripts/log-atteler-event"]
      timeout_seconds: 5
      env:
        ATTELER_LOG_LEVEL: info

context:
  max_file_bytes: 65536
  max_total_bytes: 262144
  max_input_tokens: 120000

plugins:
  paths: ["./.atteler/plugins/reviewer"]
```

Credentials still come from the existing environment/keychain discovery paths.
OpenAI Platform calls require a real platform API key from `OPENAI_API_KEY` or
the `OPENAI_API_KEY` field in `~/.codex/auth.json`; Codex ChatGPT OAuth tokens
are intentionally not sent to `api.openai.com` because they use separate Codex
account limits. When `codex` is installed and logged in, Atteler registers a
separate `codex` provider that shells out to `codex exec`, so models such as
`codex/gpt-5.5` reuse the same login as the Codex CLI.
Similarly, when `claude` (Claude Code) is installed and logged in, Atteler
registers a separate `claude-code` provider that shells out to `claude --print`
so models such as `claude-code/claude-opus-4-6` reuse Claude Code's
subscription/session path instead of direct Anthropic API quota.
The local coding providers run from Atteler's current working directory. Claude
Code is launched with file/search/edit tools (`Read`, `Write`, `Edit`,
`MultiEdit`, `LS`, `Glob`, and `Grep`) plus `Bash` for explicit shell-command
requests, scoped to that directory; Codex is launched with a workspace-write
sandbox rooted at that directory, so these providers can inspect and modify
files in the project you started `atteler` from.
For Anthropic, Atteler also reuses ForgeCode credentials from
`$FORGE_CONFIG/.credentials.json`, `~/forge/.credentials.json`, or
`~/.forge/.credentials.json`; a Forge `claude_code` login is used as a bearer
OAuth credential and refreshed with Forge's stored refresh token when needed,
while a Forge `anthropic` login is used as an API key.
`OPENAI_BASE_URL` and `ANTHROPIC_BASE_URL` override configured `base_url`
values for one-off local runs.
A local `ollama` provider is also available for offline inference. Set `default_provider: ollama` and a local model such as `default_model: llama3.2`, or use model IDs like `ollama/llama3.2`; `OLLAMA_BASE_URL` overrides the configured `base_url` for one-off local runs. When a selected local Ollama provider is unavailable, Atteler tries to fork `ollama serve` and waits briefly for `/api/tags` before failing; set `ATTELER_OLLAMA_AUTO_START=false` to disable that startup attempt.

Atteler also imports best-effort defaults from existing harness config files at
lower precedence than atteler config:

- `~/.codex/config.toml`
- `~/.claude/settings.json` or `~/.claude.json`
- `~/.config/opencode/opencode.json` or `opencode.jsonc`,
  `~/.config/opencode/config.json` or `config.jsonc`,
  `~/.config/opencode/oh-my-openagent.json` or `oh-my-opencode.json`,
  `~/.opencode.json` or `.opencode.jsonc`, any file pointed to by
  `OPENCODE_CONFIG`, `./opencode.json` or `./opencode.jsonc`, and OpenCode
  agent markdown files under `~/.config/opencode/agents/`,
  `OPENCODE_CONFIG_DIR/agents/`, or `./.opencode/agents/`
- `$FORGE_CONFIG/.forge.toml`, `~/forge/.forge.toml`, or
  `~/.forge/.forge.toml`

OpenCode agent imports populate Atteler's normal agent registry, so visible
OpenCode agents show up in the interactive TUI, prompt completion, and `@agent`
invocation. Hidden OpenCode agents remain addressable by explicit name but are
omitted from the normal picker/list surfaces.

## CLI usage

Interactive TUI:

```sh
atteler
```

In the interactive TUI, press `Ctrl+O` to choose a model. If `fzf` is installed,
Atteler opens an external fuzzy finder over `provider/model` entries; otherwise
it falls back to the built-in keyboard picker. After choosing a model, pick how
long it should remain the default:

- `Session only`: update the current saved session.
- `This folder`: reuse the model the next time Atteler starts from the same
  working directory, even in a different session.
- `Globally`: reuse the model everywhere unless a folder/session/CLI selection
  is more specific.

When a picker row includes a reasoning/effort suffix, that effort is persisted
with the same scope and reused for the session, folder, or global default.

The prompt input keeps useful editing state:

- `Up` / `Down`: cycle through recent user prompts from the current and saved
  sessions. If the cursor is in the middle of the current input, `Up` moves to
  the beginning and `Down` moves to the end instead of replacing the draft.
- `Tab`: accept the visible gray rest-of-line suggestion, or an active `@`
  agent/path completion.
- `Ctrl+R`: deterministically revamp the current prompt with goal/context/
  constraints/output-format guidance.
- `Ctrl+Z`: undo the latest `Ctrl+R` prompt revamp.

After one second of idle typing, Atteler may ask the selected LLM for a short
append-only prompt suffix; stale suggestions are ignored if you keep typing.

Folder and global choices are stored as YAML in Atteler state. Override the
state file with `ATTELER_STATE`; otherwise Atteler uses
`$XDG_STATE_HOME/atteler/state.yaml` or `~/.local/state/atteler/state.yaml`.

One-shot prompt:

```sh
atteler chat once "Explain this repository in one paragraph"
# or:
atteler "Explain this repository in one paragraph"
# with piped context:
git diff | atteler chat once "Review this diff" --stdin
# stdin can be the whole prompt:
cat README.md | atteler chat once --stdin
# machine-readable result:
atteler chat once "Explain this repository in one paragraph" --output json
# headless run metadata/logs for CI or library-style callers:
atteler chat once "Summarize @README.md" --headless --output json
atteler session headless
atteler session stream-headless <headless-id>
```

Grouped command surface:

Atteler keeps the top-level help short and routes feature discovery through
focused domains. Run `atteler help` for the domain list, `atteler help <domain>`
for commands/examples in one area, and `atteler help legacy` for the full
compatibility flag catalog. Existing `--flag` aliases remain supported for
scripts. If you know an old flag name, `atteler help --code-summary` jumps to
the focused domain that owns it.

Domain help is rendered from structured command metadata and covered by routing tests,
so README examples stay representative instead of duplicating the whole flag
catalog.

<!-- atteler:cli-domains:start -->
| Domain | Examples |
|--------|----------|
| `chat` / `session` | `atteler chat once "Explain this repository in one paragraph"`, `atteler session list`, `atteler session search "auth retry"` |
| `config` | `atteler config paths`, `atteler config validate`, `atteler config doctor-offline` |
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

Example session export:

```sh
atteler chat once "Review @pkg/auth.go" --session-title "Auth review" --session-tag auth,review
atteler session export 20260430-120000-deadbeef > transcript.md
atteler session export 20260430-120000-deadbeef --export-format json > transcript.json
atteler session show 20260430-120000-deadbeef
atteler session search "auth flow"
```

Configured agents can also be invoked per prompt with an `@agent` prefix:

```sh
atteler chat once "@reviewer Review this diff: ..."
```

Agents can declare `triggers` for indirect routing. Explicit `--agent` and
`@agent` always win, but without those overrides a prompt containing a trigger
phrase such as `review this` can select the matching agent automatically.
Use `atteler agents plan` to preview multi-agent routing without making an LLM call:

```sh
atteler agents plan "review this auth change" --plan-max-agents 3
atteler agents plan "research and implement OAuth refresh" --plan-agent researcher --plan-agent coder
```

`fallback_models` defines an ordered retry chain. Global fallbacks apply to
default routing; per-agent fallbacks apply when that agent chooses the model.
An explicit `--model` or picker selection locks the requested model and disables
the configured fallback chain for that request.

Generation settings are layered as global `generation`, then agent-specific
values, then explicit CLI overrides (`--temperature`, `--top-p`, `--seed`,
`--reasoning-level`, `--max-tokens`). Omitted values are not sent to providers, so setting
`temperature: 0` or `seed: 1` is an explicit deterministic choice rather than an
accidental default. OpenAI receives `seed`; providers without seed support ignore
the field. One-shot and interactive sessions print a compact token usage summary
when usage data is available: input, cached input, and output tokens.

## Local file and directory context

Prompts can reference local files or directories with `@path` tokens. Atteler
keeps the visible session transcript unchanged, but appends referenced file
contents or bounded directory trees to the LLM request inside a
`<context_references>` block.

```sh
atteler chat once "Summarize @README.md and @pkg/llm/llm.go"
atteler chat once "Map the package layout in @pkg"
```

References are resolved relative to the current working directory, must stay
inside that directory, and are bounded by `context.max_file_bytes` plus
`context.max_total_bytes`. Directory references include a sorted tree listing,
not file contents, and are capped to avoid flooding the model context.
Set `context.max_input_tokens` or pass `--max-input-tokens` to hard-stop
oversized requests before provider calls. In the interactive TUI, pressing Tab
after an active `@` token completes matching configured agents or local paths.

## Deterministic response fixtures

For evals and regression tests, one-shot runs can record the request envelope
and provider response to JSON, then replay that response later without a live
LLM call:

```sh
atteler chat once "Summarize @README.md" --record-response .atteler/fixtures/readme-summary.json
atteler chat once "Summarize @README.md" --replay-response .atteler/fixtures/readme-summary.json
```

Replay still writes normal session messages, so exports and searches work the
same way while removing provider availability and sampling noise from test
runs.

Use the eval check runner to validate recorded or generated output without a
provider call:

```sh
atteler eval output .atteler/fixtures/readme-summary.txt \
  --eval-expected "package overview" \
  --eval-mode contains
```

## Plugins, evaluations, artifacts, and negative knowledge

Configured `plugins.paths` entries point at local plugin directories or manifest
files. `atteler plugins list` validates `plugin.yaml`, `plugin.yml`, or
`plugin.json` manifests with `name`, `version`, optional `description`,
`capabilities`, and relative `entrypoints`.

The SDK package `pkg/plugin` also exposes a validated `RunEntrypoint` helper
for local workflows that want to execute a manifest entrypoint with captured
stdout/stderr, a timeout, root-relative path resolution, and symlink escape
protection.

Plugin entrypoints can be inspected or run from the CLI:

```sh
atteler plugins describe reviewer
atteler plugins run reviewer/check --plugin-dry-run
atteler plugins run reviewer --plugin-entrypoint check
```

RTK users can scaffold a local plugin and then add the printed path to
`plugins.paths`:

```sh
atteler plugins init-rtk .atteler/plugins/rtk
atteler plugins run rtk/version
atteler plugins run rtk/init-codex
```

Record failed approaches with:

```sh
atteler session record-failure "retry token refresh timer" \
  --session 20260430-120000-deadbeef \
  --failure-reason "created retry storms" \
  --failure-commit abc123
```

Negative knowledge is stored in session JSON, shown by `atteler session show`,
found by `atteler session search`, and included in Markdown/JSON exports. Use
`atteler session failures --session <id-or-path>` for a compact
negative-knowledge inventory.

Record evaluations and artifacts with:

```sh
atteler eval record reviewer \
  --session 20260430-120000-deadbeef \
  --evaluation-outcome pass \
  --evaluation-score 5 \
  --evaluation-notes "caught the auth regression"

atteler session record-artifact docs/research.md \
  --session 20260430-120000-deadbeef \
  --artifact-kind research \
  --artifact-summary "comparison of auth refresh approaches"

atteler session merge-artifacts .atteler/merged-artifacts.md \
  --session 20260430-120000-deadbeef
```

Evaluations and artifacts are stored in session JSON, shown by
`atteler session show`, found by `atteler session search`, and included in exports.
Merged artifact export reads text artifacts safely under the current repo root,
skips unsafe or oversized entries with warnings, and writes deterministic
Markdown for code-merge/research aggregation. Use `atteler session artifacts
--session <id-or-path>` for a compact artifact inventory, and `atteler eval list
--session <id-or-path>` for a compact evaluation inventory.

## Local memory, code intelligence, review, and workflow domains

The high-volume local tools are grouped behind domain help instead of being
documented as hundreds of one-off flags. Use the focused help as the generated
catalog and keep README examples to the common paths:

```sh
atteler help memory
atteler help code-intel
atteler help agents
atteler help review
atteler help watch
```

Saved sessions, UTF-8 memory stores, agent memory, local vector indexes, and
git history all live under the memory/RAG domain:

```sh
atteler memory search "OAuth retry storm"
atteler memory git-history "memory regression"
atteler memory vector-search "redirect risks" --vector-index docs/research.md
atteler memory agent-search "redirect risks" \
  --agent-memory-agent reviewer \
  --agent-memory-store .atteler/agent-memory.json \
  --agent-memory-index docs/review-notes.md
```

Code intelligence commands expose the Go index, import graph, symbol lookup,
impact queries, and optional LSP lookups without an LLM call:

```sh
atteler code-intel summary
atteler code-intel symbol NewRegistry
atteler code-intel import-prefix github.com/tommoulard/atteler/pkg/
atteler code-intel lsp-symbols \
  --lsp-command gopls \
  --lsp-arg serve \
  --lsp-file cmd/atteler/main.go
```

Background health and review workflows have their own focused surfaces:

```sh
atteler watch scan
atteler watch json
atteler watch loop --watch-interval-seconds 60 --watch-max-iterations 3

atteler review scan
atteler review plan \
  --review-agent quality-reviewer \
  --review-agent test-engineer \
  --review-path pkg/llm/auth.go \
  --review-gate "tests pass"
```

Agent orchestration commands cover speculative plans, async task waves,
persistent tasks, prompt completion, feedback proposals, and sub-agent fan-out:

```sh
atteler agents plan "review this auth change" --plan-max-agents 3
atteler agents async-plan \
  --async-task 'plan|planner|draft plan' \
  --async-task 'code|coder|implement feature|plan'
atteler agents task-add "draft the migration plan" --task-id plan --task-agent planner
atteler agents spawn 'planner|draft the migration plan' --spawn-dry-run
atteler agents prompt-complete "ask rev"
```

Supporting local workflow helpers remain available as compatibility flags and
are discoverable from the relevant domain help: MCP/plugin commands under
`plugins`, deterministic output checks and response fixtures under `eval`,
context compression under `memory`, model routing under `providers`, and local
bash/task utilities under `agents`.

## SDK building blocks

Atteler keeps workflow primitives in reusable packages so the CLI remains a
thin interface:

- `pkg/agent` includes metadata/capability matching plus deterministic
  orchestration planning for choosing a bounded set of agents for a task.
- `pkg/agentmemory` persists vectorized documents by agent namespace and
  searches only the selected agent's memory.
- `pkg/artifactmerge` safely reads session artifact files under a repo root and
  renders deterministic merged Markdown for sandbox/code aggregation.
- `pkg/async` includes dependency-aware task graph validation, child task
  derivation, ready batches, and a same-wave concurrent runner for parallel
  agent execution waves.
- `pkg/codeintel` indexes Go packages, imports, symbols, and import edges using
  the standard parser as a dependency-free code-intelligence foundation.
- `pkg/codegraph` provides deterministic directed graph traversal, reverse
  dependency impact sets, cycle detection, and topological layering for code
  relationships.
- `pkg/contextpack` includes smart chat-context compaction that preserves system
  messages, newest turns, and omission statistics.
- `pkg/events` exposes lifecycle hook event metadata and runs configured local
  hook commands with JSON payloads and event-specific environment variables.
- `pkg/eval` includes dependency-free output checks (`exact`, `contains`, and
  normalized matching) with compact failure summaries.
- `pkg/feedback` turns recorded evaluations and negative knowledge into stable
  agent-improvement proposals and applies them idempotently to configured
  agent prompts with history entries.
- `pkg/githistory` parses captured `git log --name-only` output and provides
  deterministic lexical search over commit subjects, authors, and touched files.
- `pkg/lsp` runs one-shot LSP document-symbol and workspace-symbol requests
  over stdio and normalizes `DocumentSymbol` and `SymbolInformation` responses.
- `pkg/llm` exposes the provider-agnostic registry and built-in OpenAI,
  Anthropic, Claude Code, Codex, and Ollama providers.
- `pkg/mcp` validates MCP server manifests, supports capability lookup, and can
  invoke one JSON-RPC method or `tools/call` request against a stdio server.
- `pkg/memory` includes a dependency-free lexical text index with ranked
  search, snippets, file/session indexing, and JSON persistence.
- `pkg/modelroute` ranks model candidates using estimated token cost, budget,
  context limits, prompt-cache reuse, and latency/TTFT signals.
- `pkg/plugin` includes manifest loading/validation and safe local entrypoint
  execution.
- `pkg/promptcomplete` ranks deterministic rest-of-line prompt suggestions from
  local agents, tools, resources, and prompt templates.
- `pkg/review` provides structured review reports, speculative review-agent
  plans, finding grouping, severity summaries, required gate-check validation,
  and a three-round LLM-backed review runner for review-agent workflows.
- `pkg/session` persists sessions and supports metadata inventories, exact tag
  filtering, transcript search, exports, evaluations, failures, artifacts, and
  aggregate agent performance summaries.
- `pkg/shell` runs explicit local bash commands with captured stdout/stderr,
  timeout handling, and caller-supplied environment overlays.
- `pkg/skill` includes repeated-action detection for skill synthesis
  suggestions plus safe markdown persistence for accepted skills.
- `pkg/speculate` models and runs the three-round speculative execution
  workflow with concurrent proposals, concurrent cross-reviews, aggregation,
  structured gate-check validation, and prompt-cache shared-prefix estimates.
- `pkg/subagent` concurrently fans out child Atteler one-shot requests while
  preserving stable input-order results.
- `pkg/vector` provides an in-memory cosine retrieval store plus deterministic
  lexical feature hashing for local RAG fallbacks.
- `pkg/watch` scans repositories for background-agent health findings such as
  stale TODOs, large files, Go files missing test companions, and convention
  drift, skips runtime/generated artifact directories, and includes a
  context-aware continuous runner for background loops.

## Event hooks

Hooks are optional local commands that receive lifecycle events as one JSON
object on stdin. They are disabled unless configured. `command` is an argv list
and is executed directly without a shell.

Supported event names:

- `session_start`
- `user_message`
- `assistant_message`
- `error`
- `session_end`
- `file_read` -- emitted when Atteler reads a user/project file (for example
  while expanding `@path` references).
- `context_add` -- emitted when a local reference is added to LLM context.
- `file_write` -- emitted when Atteler writes a local file.
- `command_execute` -- emitted when Atteler starts a local subprocess (e.g.
  `claude auth status`, `codex exec`, git operations).
- `command_output` -- emitted after a local command finishes, with the rendered
  output in the event `content`.
- `tool_execute` -- emitted when Atteler invokes a provider/tool such as
  `llm.complete`.
- `agent_execute` -- emitted when a configured agent is selected for work.

Run `atteler config hooks` or `atteler config hooks-json` to print
the same supported-event inventory from the installed binary.

Every hook also receives useful environment variables such as
`ATTELER_EVENT_TYPE`, `ATTELER_SESSION_ID`, `ATTELER_SESSION_PATH`,
`ATTELER_AGENT`, `ATTELER_MODEL`, `ATTELER_ROLE`, and `ATTELER_EVENT_UNIX`
(the event timestamp). Hook commands additionally receive any `env` map
declared in the hook's configuration.

In addition to user-configured hooks, Atteler can stream a single compact
line per event to a writer via the built-in `events.Logger`. This is the
mechanism that surfaces granular file/command/tool/agent activity in
one-shot mode without requiring any hook configuration.

## Automatic worktree isolation

When you run `atteler worktrees run`, Atteler creates a dedicated git worktree for the
session so that file changes made during the conversation do not touch your
working copy. This is especially useful when several sessions run in the same
repository at the same time, or when you want to review LLM-generated edits
before they land on your branch.

### How it works

1. **Create** -- On session start, `atteler worktrees run` runs
   `git branch atteler/<session-id>` from the current HEAD and then
   `git worktree add` to check it out under
   `.atteler/worktrees/<session-id>` (or `$ATTELER_WORKTREE_DIR`).
   All `@file` context references are automatically re-rooted to the worktree
   directory so providers that operate on the filesystem see the isolated copy.

2. **Re-join** -- Continuing an existing session that already has a worktree
   (`atteler worktrees run --session <id>`) re-uses the same worktree directory
   instead of creating a new one. The session JSON stores the worktree path,
   branch, and base branch so this works across invocations.

3. **Auto-merge on exit** -- By default, when the session ends Atteler
   automatically merges the worktree branch back into the base branch with
   `--no-ff`, then removes the worktree and deletes the branch. Any
   uncommitted changes in the worktree are staged and committed first with a
   timestamped message. If the merge fails (e.g. due to conflicts) the
   worktree is preserved and a hint is printed to retry manually.

4. **Deferred merge** -- Pass `--no-auto-merge` to keep the worktree alive
   after the session ends. Atteler prints the worktree path and a command to
   merge later:

   ```sh
   atteler worktrees run "Refactor auth flow" --no-auto-merge
   # later:
   atteler worktrees merge <session-id>
   ```

5. **List and inspect** -- `atteler worktrees list` prints all active
   atteler-managed worktrees with their branch, base branch, and session ID.

### Worktree commands

| Command | Description |
|---------|-------------|
| `atteler worktrees run [prompt]` | Isolate the session in a git worktree |
| `atteler worktrees run --no-auto-merge [prompt]` | Keep the worktree alive on exit instead of auto-merging |
| `atteler worktrees list` | List active atteler worktrees and exit |
| `atteler worktrees merge <session-id>` | Merge a session worktree back and clean up |

### Environment variables

| Variable | Description |
|----------|-------------|
| `ATTELER_WORKTREE_DIR` | Override the parent directory for worktree directories (default: `<repo-root>/.atteler/worktrees/`) |

### Session metadata

When a worktree is active, the session JSON includes three additional fields:

- `worktree_path` -- absolute path to the worktree directory.
- `worktree_branch` -- the `atteler/<session-id>` branch name.
- `worktree_base` -- the branch from which the worktree was created.

These fields are cleared after a successful merge. `atteler session show` and
session export include them when present.

### Examples

```sh
# Interactive session with worktree isolation
atteler worktrees run

# One-shot with worktree, auto-merge on exit
atteler worktrees run "Add unit tests for the auth package"

# One-shot with worktree, keep worktree after exit
atteler worktrees run "Rewrite the config loader" --no-auto-merge

# Continue a previous worktree session
atteler worktrees run --session 20260430-120000-deadbeef

# List active worktrees
atteler worktrees list

# Manually merge a deferred worktree
atteler worktrees merge 20260430-120000-deadbeef
```

### Diagnostics

`atteler config doctor` includes worktree status when one is active. The interactive
TUI header also displays the worktree path and branch name.

## Build, CI, and releases

Local development uses the Makefile as the main build surface:

- `make build` compiles `./atteler` from `./cmd/atteler`.
- `make test` runs all Go tests with the race detector.
- `make e2e` runs black-box CLI tests against a freshly built binary.
- `make e2e-live` runs opt-in live LLM CLI tests. Set
  `ATTELER_E2E_LIVE=1` plus `OPENAI_API_KEY` and/or `ANTHROPIC_API_KEY`;
  override defaults with `ATTELER_E2E_OPENAI_MODEL` or
  `ATTELER_E2E_ANTHROPIC_MODEL` when needed. Set
  `ATTELER_E2E_FORGE_CONFIG=~/forge` to run the ForgeCode Claude live test, or
  `ATTELER_E2E_CODEX_HOME=~/.codex` to run the Codex CLI live test. A logged-in
  `claude` executable enables the Claude Code live test.
- `make lint` runs the pinned golangci-lint version.
- `make release-check` validates `.goreleaser.yaml`.
- `make release-snapshot` builds local GoReleaser artifacts in `dist/` without publishing.

GitHub Actions runs CI on pull requests and every branch push. CI generates, lints, tests, builds, validates GoReleaser, and builds a snapshot package set with concurrency cancellation for superseded runs on the same ref.

Pushing a tag triggers the release workflow. Use semantic version tags such as `v0.1.0` for package-manager-friendly versions. GoReleaser builds cross-platform archives, Linux packages (`.deb`, `.rpm`, `.apk`), checksums, and publishes them to the GitHub Release for that tag.
