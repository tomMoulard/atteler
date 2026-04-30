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
  max_tokens: 2048

providers:
  openai:
    base_url: https://api.openai.com
  anthropic:
    disabled: false
    base_url: https://api.anthropic.com

agents:
  reviewer:
    model: gpt-4.1-mini
    fallback_models: ["gpt-4.1-nano"]
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
- `--temperature <value>`: override the configured request temperature.
- `--top-p <value>`: override the configured nucleus sampling value (`0..1`).
- `--max-tokens <value>`: override the configured max output tokens.
- `--doctor`: print local readiness diagnostics without making API calls.
- `--version`: print build version information.
- `--list-providers`: print built-in provider names without API calls.
- `--list-known-models`: print built-in provider/model IDs without API calls.
- `--list-models`: print provider/model IDs discovered from configured
  providers.
- `--list-agents`: print configured agent names.
- `--list-sessions`: print saved session IDs, timestamps, metadata, and paths.
- `--list-session-tags`: print saved session tags with counts.
- `--search-sessions <query>`: search saved session metadata and transcripts.
- `--session <id-or-path>`: continue a previous session.
- `--show-session <id-or-path>`: print saved session details as YAML.
- `--session-title <title>`: set or update the saved session title.
- `--session-tag <tag>`: add a saved session tag (repeatable or
  comma-separated).
- `--replay <id-or-path>`: print a previous transcript and exit.
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

`fallback_models` defines an ordered retry chain. Global fallbacks apply to
default routing; per-agent fallbacks apply when that agent chooses the model.
An explicit `--model` or picker selection locks the requested model and disables
the configured fallback chain for that request.

Generation settings are layered as global `generation`, then agent-specific
values, then explicit CLI overrides (`--temperature`, `--top-p`,
`--max-tokens`). Omitted values are not sent to providers, so setting
`temperature: 0` is an explicit deterministic choice rather than an accidental
default.

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
