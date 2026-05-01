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
 - [x] agent metadata, capabilities, and capability-backed prompt matching
 - [x] determinism seed knob and request input-token guardrails
 - [x] local plugin manifest discovery/validation
 - [x] local plugin entrypoint execution helper for SDK workflows
 - [x] negative-knowledge capture, search, show, and export
 - [x] agent evaluation capture, search, show, and export
 - [x] sandbox artifact manifest capture, search, show, and export
 - [x] deterministic response recording/replay fixtures
 - [x] dependency-free agent orchestration planning
 - [x] CLI agent orchestration preview
 - [x] dependency-aware async agent task planning waves
 - [x] agent feedback improvement proposal primitives
 - [x] CLI feedback improvement proposal report
 - [x] cost/model routing primitives with budget, context, cache, and latency signals
 - [x] CLI cost/model routing preview
 - [x] smart context compression primitives with omission accounting
 - [x] CLI smart context compression preview
 - [x] MCP manifest validation and capability lookup primitives
 - [x] CLI MCP manifest validation and capability lookup
 - [x] dependency-free evaluation helpers for agent outputs
 - [x] CLI eval check runner
 - [x] dependency-free local memory/RAG lexical index
 - [x] CLI memory indexing/search over files and saved sessions
 - [x] CLI git history lexical search for local RAG
 - [x] CLI local vector search over indexed files
 - [x] CLI plugin describe, dry-run, and entrypoint execution
 - [x] skill synthesis suggestion primitive and CLI
 - [x] interactive `@` completion for agents and local paths
 - [x] deterministic rest-of-line prompt completion primitive and CLI preview
 - [x] dependency-free Go code intelligence and import graph foundation
 - [x] CLI Go symbol lookup over the local repository
 - [x] CLI Go import-edge listing over the local repository
 - [x] CLI Go import impact lookup over the local repository
 - [x] dependency-free code graph traversal and impact analysis primitives
 - [x] dependency-free vector retrieval primitive
 - [x] speculative three-round execution planning primitives
 - [x] CLI speculative three-round execution plan preview
 - [x] structured review-agent report and gate-check primitives
 - [x] CLI structured review scan report
 - [x] continuous background-agent repository scan primitives
 - [x] CLI background-agent repository scan
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
Code is launched with file tools (`Read`, `Write`, `Edit`, `MultiEdit`, `LS`,
`Glob`, and `Grep`) scoped to that directory, and Codex is launched with a
workspace-write sandbox rooted at that directory, so these providers can inspect
and modify files in the project you started `atteler` from.
For Anthropic, Atteler also reuses ForgeCode credentials from
`$FORGE_CONFIG/.credentials.json`, `~/forge/.credentials.json`, or
`~/.forge/.credentials.json`; a Forge `claude_code` login is used as a bearer
OAuth credential and refreshed with Forge's stored refresh token when needed,
while a Forge `anthropic` login is used as an API key.
`OPENAI_BASE_URL` and `ANTHROPIC_BASE_URL` override configured `base_url`
values for one-off local runs.

Atteler also imports best-effort defaults from existing harness config files at
lower precedence than atteler config:

- `~/.codex/config.toml`
- `~/.claude/settings.json` or `~/.claude.json`
- `~/.config/opencode/opencode.json`, `~/.config/opencode/config.json`,
  `~/.opencode.json`, or `./opencode.json`
- `$FORGE_CONFIG/.forge.toml`, `~/forge/.forge.toml`, or
  `~/.forge/.forge.toml`

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
- `--max-input-tokens <value>`: hard-stop a request before calling an LLM when
  the estimated prompt size exceeds the cap.
- `--record-response <path>`: write one-shot request/response JSON for
  deterministic fixtures.
- `--replay-response <path>`: replay a recorded response JSON without calling
  a provider.
- `--eval-output <path>` with `--eval-expected <text>` or
  `--eval-expected-file <path>`: run a deterministic output check; set
  `--eval-mode exact|contains|normalized` as needed.
- `--doctor`: print local readiness diagnostics without making API calls.
- `--version`: print build version information.
- `--list-providers`: print built-in provider names without API calls.
- `--list-known-models`: print built-in provider/model IDs without API calls.
- `--list-models`: print provider/model IDs discovered from configured
  providers.
- `--list-agents`: print configured agent names.
- `--list-plugins`: validate and print configured local plugin manifests.
- `--describe-plugin <name>`: print a configured plugin manifest and resolved
  filesystem locations as YAML.
- `--run-plugin <plugin>` with `--plugin-entrypoint <name>` or
  `--run-plugin <plugin>/<entrypoint>`: execute a configured plugin entrypoint.
  Add `--plugin-dry-run` to print the resolved command without executing it,
  and `--plugin-timeout-seconds <n>` to set a timeout.
- `--list-sessions`: print saved session IDs, timestamps, metadata, and paths.
- `--list-session-tags`: print saved session tags with counts.
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
`--max-tokens`). Omitted values are not sent to providers, so setting
`temperature: 0` or `seed: 1` is an explicit deterministic choice rather than an
accidental default. OpenAI receives `seed`; providers without seed support ignore
the field.

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
```

Evaluations and artifacts are stored in session JSON, shown by
`--show-session`, found by `--search-sessions`, and included in exports. Use
`--list-artifacts --session <id-or-path>` for a compact artifact inventory,
and `--list-evaluations --session <id-or-path>` for a compact evaluation inventory.

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

Go packages can be inventoried with file and symbol counts:

```sh
atteler --code-packages
```

One package can be expanded to its files:

```sh
atteler --code-package llm
```

Go symbols can be located without starting an LLM call:

```sh
atteler --code-symbol NewRegistry
```

Go import edges can be listed for graph/RAG workflows:

```sh
atteler --code-imports
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

Repository health findings for background-agent workflows can be scanned locally:

```sh
atteler --watch-scan
```

Speculative execution plans can be previewed locally:

```sh
atteler --speculate-plan --speculate-agent researcher --speculate-agent coder
```

Dependency-aware async task waves can also be previewed locally before spawning agents:

```sh
atteler --async-plan \
  --async-task 'plan|planner|draft plan' \
  --async-task 'code|coder|implement feature|plan'
```

Repository scans can also be rendered as structured review reports:

```sh
atteler --review-scan
```

You can also persist a small lexical memory store for UTF-8 files:

```sh
atteler --memory-store .atteler/memory.json --memory-index docs/research.md
atteler --memory-store .atteler/memory.json --memory-search "redirect risks"
```

For dependency-free local vector retrieval over specific files:

```sh
atteler --vector-index docs/research.md --vector-search "redirect risks"
```

For repeated workflows, pass observed steps and Atteler suggests a reusable
skill candidate:

```sh
atteler --skill-step plan --skill-step code --skill-step test \
  --skill-step plan --skill-step code --skill-step test
```

Model routing decisions can be previewed locally:

```sh
atteler --route-candidate 'openai/gpt-mini,input=0.000001,output=0.000002,max=128000' \
  --route-input-tokens 10000 --route-output-tokens 1000
```

Context compression can be previewed with role-prefixed transcript files:

```sh
atteler --context-pack-file transcript.txt --context-pack-tokens 4000
```

Recorded feedback can be summarized into agent improvement proposals:

```sh
atteler --session <id-or-path> --feedback-proposals
```

MCP manifests can be validated and queried by capability:

```sh
atteler --mcp-manifest .atteler/mcp.yaml --mcp-capability symbols
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
- `pkg/async` includes dependency-aware task graph validation, child task
  derivation, and ready batches for parallel agent execution waves.
- `pkg/codeintel` indexes Go packages, imports, symbols, and import edges using
  the standard parser as a dependency-free code-intelligence foundation.
- `pkg/codegraph` provides deterministic directed graph traversal, reverse
  dependency impact sets, cycle detection, and topological layering for code
  relationships.
- `pkg/contextpack` includes smart chat-context compaction that preserves system
  messages, newest turns, and omission statistics.
- `pkg/eval` includes dependency-free output checks (`exact`, `contains`, and
  normalized matching) with compact failure summaries.
- `pkg/feedback` turns recorded evaluations and negative knowledge into stable
  agent-improvement proposals.
- `pkg/githistory` parses captured `git log --name-only` output and provides
  deterministic lexical search over commit subjects, authors, and touched files.
- `pkg/mcp` validates MCP server manifests and supports capability lookup.
- `pkg/memory` includes a dependency-free lexical text index with ranked
  search, snippets, file/session indexing, and JSON persistence.
- `pkg/modelroute` ranks model candidates using estimated token cost, budget,
  context limits, prompt-cache reuse, and latency/TTFT signals.
- `pkg/plugin` includes manifest loading/validation and safe local entrypoint
  execution.
- `pkg/promptcomplete` ranks deterministic rest-of-line prompt suggestions from
  local agents, tools, resources, and prompt templates.
- `pkg/review` provides structured review reports, finding grouping, severity
  summaries, and required gate-check validation for review-agent workflows.
- `pkg/skill` includes repeated-action detection for skill synthesis
  suggestions.
- `pkg/speculate` models the three-round speculative execution workflow with
  cross-review assignments and structured gate-check validation.
- `pkg/vector` provides an in-memory cosine retrieval store plus deterministic
  lexical feature hashing for local RAG fallbacks.
- `pkg/watch` scans repositories for background-agent health findings such as
  stale TODOs, large files, and Go files missing test companions.

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
- `file_write` -- emitted when Atteler writes a local file.
- `command_execute` -- emitted when Atteler starts a local subprocess (e.g.
  `claude auth status`, `codex exec`, git operations).
- `tool_execute` -- emitted when Atteler invokes a provider/tool such as
  `llm.complete`.
- `agent_execute` -- emitted when a configured agent is selected for work.

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
