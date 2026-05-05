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
atteler --print-config-template
atteler --init-config ~/.config/atteler/config.yaml
atteler --list-config-paths
atteler --validate-config
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

The prompt input keeps useful editing state:

- `Up` / `Down`: cycle through recent user prompts from the current and saved
  sessions.
- `Tab`: accept the visible gray rest-of-line suggestion, or an active `@`
  agent/path completion.
- `Ctrl+R`: deterministically revamp the current prompt with goal/context/
  constraints/output-format guidance.
- `Ctrl+Z`: undo the latest `Ctrl+R` prompt revamp.

Folder and global choices are stored as YAML in Atteler state. Override the
state file with `ATTELER_STATE`; otherwise Atteler uses
`$XDG_STATE_HOME/atteler/state.yaml` or `~/.local/state/atteler/state.yaml`.

One-shot prompt:

```sh
atteler --once "Explain this repository in one paragraph"
# or:
atteler "Explain this repository in one paragraph"
# with piped context:
git diff | atteler --stdin --once "Review this diff"
# machine-readable result:
atteler --output json --once "Explain this repository in one paragraph"
# headless run metadata/logs for CI or library-style callers:
atteler --headless --output json --once "Summarize @README.md"
atteler --list-headless
atteler --stream-headless <headless-id>
```

Useful flags:

- `--config <path>`: add an overriding config file path. Use the platform
  path-list separator to pass more than one path.
- `--print-config-template`: print a starter YAML config.
- `--init-config <path>`: write a starter YAML config without overwriting an
  existing file.
- `--list-config-paths`: print config files in load order with present/missing
  status.
- `--validate-config`: parse and merge config files, then exit.
- `--model <id>`: select a model for this run.
- `--agent <name>`: select a configured agent persona for this run.
- `--describe-agent <name>`: print one configured agent as YAML.
- `--plan-agents <prompt>`: preview which configured agents match a prompt;
  repeat `--plan-agent <name>` to force agents into the plan and use
  `--plan-max-agents <n>` to cap the result.
- `--temperature <value>`: override the configured request temperature.
- `--top-p <value>`: override the configured nucleus sampling value (`0..1`).
- `--max-tokens <value>`: override the configured max output tokens.
- `--seed <value>`: pass a best-effort deterministic seed to providers that
  support it.
- `--reasoning-level <value>`: override the configured model/provider effort
  for this run.
- `--max-input-tokens <value>`: hard-stop a request before calling an LLM when
  the estimated prompt size exceeds the cap.
- `--record-response <path>`: write one-shot request/response JSON for
  deterministic fixtures.
- `--replay-response <path>`: replay a recorded response JSON without calling
  a provider.
- `--output text|json`: choose plain text or machine-readable JSON for one-shot
  prompt results.
- `--headless`: run a one-shot prompt without TUI/plain output while recording
  headless run metadata and logs under the session store.
- `--list-headless`: list active headless runs.
- `--stream-headless <id>`: stream one headless run log until it exits.
- `--eval-output <path>` with `--eval-expected <text>` or
  `--eval-expected-file <path>`: run a deterministic output check; set
  `--eval-mode exact|contains|normalized` as needed.
- `--doctor`: print local readiness diagnostics and configured provider health.
- `--doctor-offline`: print config/session/provider inventory without provider
  health checks or API calls.
- `--version`: print build version information.
- `--list-providers`: print built-in provider names without API calls.
- `--list-known-models`: print built-in provider/model IDs without API calls.
- `--list-models`: print provider/model IDs discovered from configured
  providers.
- `--list-agents`: print configured agent names.
- `--list-plugins`: validate and print configured local plugin manifests.
- `--list-hook-events`: print supported lifecycle hook event names and short
  descriptions.
- `--list-hook-events-json`: print the same hook event inventory as JSON for
  scripts.
- `--describe-plugin <name>`: print a configured plugin manifest and resolved
  filesystem locations as YAML.
- `--run-plugin <plugin>` with `--plugin-entrypoint <name>` or
  `--run-plugin <plugin>/<entrypoint>`: execute a configured plugin entrypoint.
  Add `--plugin-dry-run` to print the resolved command without executing it,
  and `--plugin-timeout-seconds <n>` to set a timeout.
- `--bash <command>`: run an explicit local `bash -lc` command and exit.
  Use `--bash-dir <path>` for the working directory and
  `--bash-timeout-seconds <n>` for a timeout.
- `DEBUG_ATTELER_*` aliases can drive local debug/inspection flags from the
  environment, for example `DEBUG_ATTELER_LIST_PROVIDERS=1`,
  `DEBUG_ATTELER_WATCH_SCAN=1`, or `DEBUG_ATTELER_MCP_MANIFEST=...`.
- `--mcp-manifest <path>` with `--mcp-server <name>` and either
  `--mcp-tool <tool>` or `--mcp-method <method>` invokes a configured MCP
  stdio server once. Use `--mcp-tool-args` or `--mcp-params` for JSON inputs
  and `--mcp-timeout-seconds <n>` for a timeout.
- `--lsp-symbols --lsp-command <server> --lsp-file <path>` requests document
  symbols from an external LSP server. Repeat `--lsp-arg` for server args, and
  optionally pass `--lsp-root` and `--lsp-language`.
- `--lsp-workspace-symbols <query> --lsp-command <server>` requests workspace
  symbols from an external LSP server. Repeat `--lsp-arg` for server args and
  optionally pass `--lsp-root`.
- `--spawn-agent <agent|prompt>` or `--spawn-agent <id|agent|prompt>` runs
  child Atteler one-shot prompts concurrently. Use `--spawn-dry-run` to inspect
  the fan-out without calling an LLM, `--spawn-binary` to choose the binary, and
  `--spawn-timeout-seconds <n>` to bound the run.
- `--list-sessions`: print saved session IDs, timestamps, metadata, and paths.
- `--list-sessions --list-sessions-tag <tag>`: print only sessions containing
  that exact tag, matched case-insensitively.
- `--list-session-tags`: print saved session tags with counts.
- `--agent-performance-summary`: print aggregate evaluation, failure, score,
  outcome, and latest-activity summaries grouped by agent across saved sessions.
- `--list-artifacts`: print artifact records for the selected session.
- `--list-evaluations`: print agent evaluation records for the selected session.
- `--list-failures`: print negative-knowledge records for the selected session.
- `--list-messages`: print compact message roles, sizes, and previews for the selected session.
- `--search-sessions <query>`: search saved session metadata and transcripts.
- `--session <id-or-path>`: continue a previous session.
- `--show-session <id-or-path>`: print saved session details as YAML.
- `--session-summary <id-or-path>`: print compact saved session metadata and counts.
- `--session-title <title>`: set or update the saved session title.
- `--session-tag <tag>`: add a saved session tag (repeatable or
  comma-separated).
- `--record-failure <approach>` with `--failure-reason <reason>`: record
  negative knowledge on the selected session so failed approaches are searchable
  and exported.
- `--record-evaluation <agent>` with `--evaluation-outcome <outcome>`:
  append an agent evaluation to the selected session; optional
  `--evaluation-score`, `--evaluation-notes`, and `--evaluation-reference`
  fields are shown, searched, and exported.
- `--record-artifact <path>` with `--artifact-kind <kind>`: append a useful
  sandbox/research/code artifact to the selected session; optional
  `--artifact-summary` describes why it matters.
- `--merge-artifacts <path>`: aggregate selected-session text artifacts into a
  deterministic Markdown file. Use `-` for stdout and
  `--merge-artifact-max-bytes <n>` to bound each input artifact.
- `--feedback-apply-config <path>`: apply selected-session feedback proposals
  to configured agent prompts and append a decision record. Override the
  default `<config>.feedback.md` log with `--feedback-history <path>`.
- `--replay <id-or-path>`: print a previous transcript and exit. Use `--list-messages --session <id-or-path>` for compact transcript previews.
- `--export-session <id-or-path>`: export a previous transcript and exit.
- `--export-format <markdown|json>`: choose the export format (`markdown` by
  default).
- `--session-dir <path>`: store session JSON files somewhere other than
  `./.atteler/sessions` (also available as `ATTELER_SESSION_DIR`).
- `--stdin`: append stdin to a one-shot prompt.
- `--worktree`: isolate the session in a dedicated git worktree.
- `--no-auto-merge`: keep the worktree alive on exit instead of auto-merging.
- `--list-worktrees`: list active atteler worktrees and exit.
- `--merge-worktree <session-id>`: merge a session worktree back into its base
  branch and exit.
- `--memory-search <query>`: search local memory. By default this indexes saved
  sessions; combine with `--memory-store <path>` to load a JSON memory store and
  with repeated `--memory-index <file>` to add UTF-8 files before search or
  saving. `--memory-limit <n>` caps results.
- `--agent-memory-agent <name>` with `--agent-memory-store <path>`: index and
  search one agent's persistent vector memory using `--agent-memory-index`,
  `--agent-memory-search`, and optional `--agent-memory-limit`.
- `--skill-step <action>`: add observed actions for a skill-synthesis
  suggestion. Repeat it (or pass comma-separated values), then optionally tune
  `--skill-max-steps` and `--skill-min-occurrences`.

Example session export:

```sh
atteler --session-title "Auth review" --session-tag auth,review --once "Review @pkg/auth.go"
atteler --export-session 20260430-120000-deadbeef > transcript.md
atteler --export-session 20260430-120000-deadbeef --export-format json > transcript.json
atteler --show-session 20260430-120000-deadbeef
atteler --search-sessions "auth flow"
```

Configured agents can also be invoked per prompt with an `@agent` prefix:

```sh
atteler --once "@reviewer Review this diff: ..."
```

Agents can declare `triggers` for indirect routing. Explicit `--agent` and
`@agent` always win, but without those overrides a prompt containing a trigger
phrase such as `review this` can select the matching agent automatically.
Use `--plan-agents` to preview multi-agent routing without making an LLM call:

```sh
atteler --plan-agents "review this auth change" --plan-max-agents 3
atteler --plan-agents "research and implement OAuth refresh" --plan-agent researcher --plan-agent coder
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
atteler --once "Summarize @README.md and @pkg/llm/llm.go"
atteler --once "Map the package layout in @pkg"
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
atteler --once "Summarize @README.md" --record-response .atteler/fixtures/readme-summary.json
atteler --once "Summarize @README.md" --replay-response .atteler/fixtures/readme-summary.json
```

Replay still writes normal session messages, so exports and searches work the
same way while removing provider availability and sampling noise from test
runs.

Use the eval check runner to validate recorded or generated output without a
provider call:

```sh
atteler --eval-output .atteler/fixtures/readme-summary.txt \
  --eval-expected "package overview" \
  --eval-mode contains
```

## Plugins, evaluations, artifacts, and negative knowledge

Configured `plugins.paths` entries point at local plugin directories or manifest
files. `atteler --list-plugins` validates `plugin.yaml`, `plugin.yml`, or
`plugin.json` manifests with `name`, `version`, optional `description`,
`capabilities`, and relative `entrypoints`.

The SDK package `pkg/plugin` also exposes a validated `RunEntrypoint` helper
for local workflows that want to execute a manifest entrypoint with captured
stdout/stderr, a timeout, root-relative path resolution, and symlink escape
protection.

Plugin entrypoints can be inspected or run from the CLI:

```sh
atteler --describe-plugin reviewer
atteler --run-plugin reviewer/check --plugin-dry-run
atteler --run-plugin reviewer --plugin-entrypoint check
```

Record failed approaches with:

```sh
atteler --session 20260430-120000-deadbeef \
  --record-failure "retry token refresh timer" \
  --failure-reason "created retry storms" \
  --failure-commit abc123
```

Negative knowledge is stored in session JSON, shown by `--show-session`, found
by `--search-sessions`, and included in Markdown/JSON exports. Use
`--list-failures --session <id-or-path>` for a compact negative-knowledge inventory.

Record evaluations and artifacts with:

```sh
atteler --session 20260430-120000-deadbeef \
  --record-evaluation reviewer \
  --evaluation-outcome pass \
  --evaluation-score 5 \
  --evaluation-notes "caught the auth regression"

atteler --session 20260430-120000-deadbeef \
  --record-artifact docs/research.md \
  --artifact-kind research \
  --artifact-summary "comparison of auth refresh approaches"

atteler --session 20260430-120000-deadbeef \
  --merge-artifacts .atteler/merged-artifacts.md
```

Evaluations and artifacts are stored in session JSON, shown by
`--show-session`, found by `--search-sessions`, and included in exports.
Merged artifact export reads text artifacts safely under the current repo root,
skips unsafe or oversized entries with warnings, and writes deterministic
Markdown for code-merge/research aggregation. Use `--list-artifacts --session
<id-or-path>` for a compact artifact inventory, and `--list-evaluations
--session <id-or-path>` for a compact evaluation inventory.

## Local memory and skill suggestions

Saved sessions are searchable as local memory without a separate service:

```sh
atteler --memory-search "OAuth retry storm"
```

Git history is searchable through the same local-first retrieval posture:

```sh
atteler --git-history-search "memory regression"
```

Go code index and graph counts can be summarized locally:

```sh
atteler --code-summary
```

All Go files can be inventoried with package, import, and symbol counts:

```sh
atteler --code-files
```

Go packages can be inventoried with file and symbol counts:

```sh
atteler --code-packages
```

Go packages can also be summarized by import counts:

```sh
atteler --code-package-import-summary
```

One package can be expanded to its files:

```sh
atteler --code-package llm
```

One package's import usage can be summarized:

```sh
atteler --code-package-imports llm
```

One package's import usage can be filtered by exact import path or prefix:

```sh
atteler --code-package-import-path llm:context
atteler --code-package-import-prefix llm:github.com/tommoulard/atteler/pkg/
```

Files in a package that import an exact path can also be listed:

```sh
atteler --code-package-import-files llm:context
```

Files in a package that import an exact path can be summarized:

```sh
atteler --code-package-import-path-file-summary llm:context
```

Files in one package can be summarized by import count:

```sh
atteler --code-package-import-file-summary llm
```

Files in a package that import paths with a prefix can be listed too:

```sh
atteler --code-package-import-prefix-files llm:github.com/tommoulard/atteler/pkg/
```

Files in one package can be summarized by matching import-prefix count:

```sh
atteler --code-package-import-prefix-file-summary llm:github.com/tommoulard/atteler/pkg/
```

One package's symbol kinds can be summarized:

```sh
atteler --code-package-symbols llm
```

Files in one package can be summarized by symbol count:

```sh
atteler --code-package-symbol-file-summary llm
```

One package's concrete symbols can be listed:

```sh
atteler --code-package-symbol-list llm
```

One package's symbols can be filtered by exact name, kind, or name prefix:

```sh
atteler --code-package-symbol llm:NewRegistry
atteler --code-package-symbol-kind llm:func
atteler --code-package-symbol-prefix llm:New
```

Files in one package can be summarized by exact symbol-name count:

```sh
atteler --code-package-symbol-name-file-summary llm:NewRegistry
```

Files in one package can be summarized by symbol-kind count:

```sh
atteler --code-package-symbol-kind-file-summary llm:func
```

Files in one package can be summarized by symbol-prefix count:

```sh
atteler --code-package-symbol-prefix-file-summary llm:New
```

One Go file can be expanded to its imports and symbols:

```sh
atteler --code-file pkg/llm/llm.go
```

One Go file's imports can be listed directly:

```sh
atteler --code-file-imports pkg/llm/llm.go
```

One Go file's symbols can be listed directly:

```sh
atteler --code-file-symbols pkg/llm/llm.go
```

One Go file's symbol kinds can be summarized:

```sh
atteler --code-file-symbol-summary pkg/llm/llm.go
```

One Go file can be checked for an exact import path:

```sh
atteler --code-file-import-path pkg/llm/llm.go:context
```

One Go file's imports can be filtered by prefix:

```sh
atteler --code-file-import-prefix cmd/atteler/main.go:github.com/tommoulard/atteler/pkg/
```

One Go file's symbols can be filtered by exact name, kind, or prefix:

```sh
atteler --code-file-symbol pkg/llm/llm.go:NewRegistry
atteler --code-file-symbol-kind pkg/llm/llm.go:func
```

One Go file's symbols can also be filtered by prefix:

```sh
atteler --code-file-symbol-prefix pkg/llm/llm.go:New
```

Go symbols can be located without starting an LLM call:

```sh
atteler --code-symbol NewRegistry
```

Files can be summarized by exact symbol-name count:

```sh
atteler --code-symbol-name-file-summary NewRegistry
```

Packages can be summarized by exact symbol-name count:

```sh
atteler --code-symbol-name-package-summary NewRegistry
```

Related Go symbols can be discovered by prefix:

```sh
atteler --code-symbol-prefix New
```

Files can be summarized by matching symbol-prefix count:

```sh
atteler --code-symbol-prefix-file-summary New
```

Packages can be summarized by matching symbol-prefix count:

```sh
atteler --code-symbol-prefix-package-summary New
```

Go symbol kinds can be summarized by count:

```sh
atteler --code-symbol-summary
```

Files with symbol counts can be summarized to spot symbol-heavy files:

```sh
atteler --code-symbol-file-summary
```

Go symbols can also be listed by kind:

```sh
atteler --code-symbol-kind type
```

Files can be summarized by symbol kind count:

```sh
atteler --code-symbol-kind-file-summary func
```

Packages can be summarized by symbol kind count:

```sh
atteler --code-symbol-kind-package-summary func
```

Go import edges can be listed for graph/RAG workflows:

```sh
atteler --code-imports
```

Import usage hotspots can be summarized by file count:

```sh
atteler --code-import-summary
```

Files with import counts can be summarized to spot import-heavy files:

```sh
atteler --code-import-file-summary
```

Files using one import path can be listed directly:

```sh
atteler --code-import-path context
```

One import path's file usage can be summarized:

```sh
atteler --code-import-path-summary context
```

Files using one import path can be summarized:

```sh
atteler --code-import-path-file-summary context
```

Packages using one import path can be summarized:

```sh
atteler --code-import-path-package-summary context
```

Files using an import path family can be listed by prefix:

```sh
atteler --code-import-prefix github.com/tommoulard/atteler/pkg/
```

Import usage for an import path family can be summarized by prefix:

```sh
atteler --code-import-prefix-summary github.com/tommoulard/atteler/pkg/
```

Files using an import path family can be summarized by matching import count:

```sh
atteler --code-import-prefix-file-summary github.com/tommoulard/atteler/pkg/
```

Packages using an import path family can be summarized:

```sh
atteler --code-import-prefix-package-summary github.com/tommoulard/atteler/pkg/
```

Topological import graph layers can be listed for dependency planning:

```sh
atteler --code-layers
```

Import graph cycles can be checked explicitly:

```sh
atteler --code-cycles
```

Reverse import impact can be queried locally:

```sh
atteler --code-impact context
```

Forward import graph reachability can be queried locally:

```sh
atteler --code-reachable cmd/atteler/main.go
```

Direct import graph edges can be queried in either direction:

```sh
atteler --code-deps cmd/atteler/main.go
atteler --code-rdeps context
```

When a language server is available, Atteler can ask it for document symbols
or workspace symbols without adding an LSP dependency to the binary:

```sh
atteler --lsp-symbols \
  --lsp-command gopls \
  --lsp-arg serve \
  --lsp-file cmd/atteler/main.go

atteler --lsp-workspace-symbols Handler \
  --lsp-command gopls \
  --lsp-arg serve \
  --lsp-root .
```

Repository health findings for background-agent workflows can be scanned locally:

```sh
atteler --watch-scan
atteler --watch-scan --watch-json
atteler --watch-loop --watch-interval-seconds 60 --watch-max-iterations 3
```

The watch scan also flags convention drift such as production Go files that
introduce `context.Background()` outside entrypoints or tests, including aliased
context imports, while skipping Atteler runtime and generated artifact folders.

Speculative execution plans can be previewed locally; when a base prompt is
provided, Atteler also estimates shared-prefix prompt-cache reuse across
branches. The SDK exposes a three-round runner for proposal, cross-review, and
verdict aggregation:

```sh
atteler --speculate-plan \
  --speculate-agent researcher \
  --speculate-agent coder \
  --speculate-prompt "plan the auth refresh migration"
```

Dependency-aware async task waves can also be previewed locally before spawning
agents; the SDK runner executes ready tasks in the same wave concurrently while
preserving deterministic wave/order results:

```sh
atteler --async-plan \
  --async-task 'plan|planner|draft plan' \
  --async-task 'code|coder|implement feature|plan'
```

Agents and sub-agents can coordinate through a small persistent task/TODO list:

```sh
atteler --task-add "draft the migration plan" --task-id plan --task-agent planner
atteler --task-assign plan:executor
atteler --task-complete plan --task-agent verifier
atteler --task-list
```

Use `--task-file <path>` to point several sessions or agents at the same JSON
task list. When omitted, Atteler stores tasks at `.atteler/tasks.json`.

Sub-agent fan-out can be previewed or executed with stable child IDs:

```sh
atteler --spawn-agent 'planner|draft the migration plan' \
  --spawn-agent 'review-1|reviewer|review the plan for risks' \
  --spawn-dry-run
```

Repository scans can also be rendered as structured review reports:

```sh
atteler --review-scan
```

Review-agent speculative plans mirror the three-round execution model for code review: independent reviews, cross-review of findings, and aggregate verdict gates.

```sh
atteler --review-plan \
  --review-agent quality-reviewer \
  --review-agent test-engineer \
  --review-path pkg/llm/auth.go \
  --review-gate "tests pass"
```

You can also persist a small lexical memory store for UTF-8 files:

```sh
atteler --memory-store .atteler/memory.json --memory-index docs/research.md
atteler --memory-store .atteler/memory.json --memory-search "redirect risks"
```

Agent-specific memory can be persisted separately using the local vector
fallback, so one agent's notes do not leak into another agent's search results:

```sh
atteler --agent-memory-agent reviewer \
  --agent-memory-store .atteler/agent-memory.json \
  --agent-memory-index docs/review-notes.md \
  --agent-memory-search "redirect risks"
```

For dependency-free local vector retrieval over specific files:

```sh
atteler --vector-index docs/research.md --vector-search "redirect risks"
```

For repeated workflows, pass observed steps and Atteler suggests a reusable
skill candidate. Add `--skill-save-dir <dir>` to accept and persist the
suggestion as a markdown artifact:

```sh
atteler --skill-step plan --skill-step code --skill-step test \
  --skill-step plan --skill-step code --skill-step test
atteler --skill-save-dir .atteler/skills \
  --skill-step plan --skill-step code --skill-step plan --skill-step code
```

Model routing decisions can be previewed locally, and the same route candidates
can hard-stop one-shot/stdin requests when every candidate exceeds budget or
context limits:

```sh
atteler --route-candidate 'openai/gpt-mini,input=0.000001,output=0.000002,max=128000' \
  --route-input-tokens 10000 --route-output-tokens 1000
atteler --route-candidate 'openai/gpt-mini,input=0.000001,output=0.000002,max=128000' \
  --route-input-tokens 10000 --route-output-tokens 1000 --route-budget 0.02 \
  --once "Summarize this repository"
```

Context compression can be previewed with role-prefixed transcript files:

```sh
atteler --context-pack-file transcript.txt --context-pack-tokens 4000
```

Recorded feedback can be summarized into agent improvement proposals, then
applied back to an agent config with a markdown history log:

```sh
atteler --session <id-or-path> --feedback-proposals
atteler --session <id-or-path> \
  --feedback-apply-config .atteler/config.yaml \
  --feedback-history .atteler/agent-feedback.md
```

MCP manifests can be validated and queried by capability:

```sh
atteler --mcp-manifest .atteler/mcp.yaml --mcp-capability symbols
atteler --mcp-manifest .atteler/mcp.yaml \
  --mcp-server repo \
  --mcp-tool search \
  --mcp-tool-args '{"query":"symbols"}'
```

Prompt-line completion can also be previewed without opening the TUI:

```sh
atteler --prompt-complete "ask rev"
```

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
  plans, finding grouping, severity summaries, and required gate-check validation
  for review-agent workflows.
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
- `tool_execute` -- emitted when Atteler invokes a provider/tool such as
  `llm.complete`.
- `agent_execute` -- emitted when a configured agent is selected for work.

Run `atteler --list-hook-events` or `atteler --list-hook-events-json` to print
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

When you pass `--worktree`, Atteler creates a dedicated git worktree for the
session so that file changes made during the conversation do not touch your
working copy. This is especially useful when several sessions run in the same
repository at the same time, or when you want to review LLM-generated edits
before they land on your branch.

### How it works

1. **Create** -- On session start, `atteler --worktree` runs
   `git branch atteler/<session-id>` from the current HEAD and then
   `git worktree add` to check it out under
   `.atteler/worktrees/<session-id>` (or `$ATTELER_WORKTREE_DIR`).
   All `@file` context references are automatically re-rooted to the worktree
   directory so providers that operate on the filesystem see the isolated copy.

2. **Re-join** -- Continuing an existing session that already has a worktree
   (`atteler --session <id> --worktree`) re-uses the same worktree directory
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
   atteler --worktree --no-auto-merge --once "Refactor auth flow"
   # later:
   atteler --merge-worktree <session-id>
   ```

5. **List and inspect** -- `atteler --list-worktrees` prints all active
   atteler-managed worktrees with their branch, base branch, and session ID.

### CLI flags

| Flag | Description |
|------|-------------|
| `--worktree` | Isolate the session in a git worktree |
| `--no-auto-merge` | Keep the worktree alive on exit instead of auto-merging |
| `--list-worktrees` | List active atteler worktrees and exit |
| `--merge-worktree <session-id>` | Merge a session worktree back and clean up |

### Environment variables

| Variable | Description |
|----------|-------------|
| `ATTELER_WORKTREE_DIR` | Override the parent directory for worktree directories (default: `<repo-root>/.atteler/worktrees/`) |

### Session metadata

When a worktree is active, the session JSON includes three additional fields:

- `worktree_path` -- absolute path to the worktree directory.
- `worktree_branch` -- the `atteler/<session-id>` branch name.
- `worktree_base` -- the branch from which the worktree was created.

These fields are cleared after a successful merge. `--show-session` and session
export include them when present.

### Examples

```sh
# Interactive session with worktree isolation
atteler --worktree

# One-shot with worktree, auto-merge on exit
atteler --worktree --once "Add unit tests for the auth package"

# One-shot with worktree, keep worktree after exit
atteler --worktree --no-auto-merge --once "Rewrite the config loader"

# Continue a previous worktree session
atteler --session 20260430-120000-deadbeef --worktree

# List active worktrees
atteler --list-worktrees

# Manually merge a deferred worktree
atteler --merge-worktree 20260430-120000-deadbeef
```

### Diagnostics

`atteler --doctor` includes worktree status when one is active. The interactive
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
