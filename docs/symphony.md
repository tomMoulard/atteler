# Symphony

Atteler includes a standalone `symphony` command that implements the Symphony
service contract from OpenAI's draft specification.

Run it without adding flags to the main `atteler` CLI:

```sh
go run ./cmd/symphony --validate
go run ./cmd/symphony
go run ./cmd/symphony ./WORKFLOW.md
make run-symphony
make build-symphony
./symphony ./WORKFLOW.md
```

## Workflow File

By default, `symphony` loads `./WORKFLOW.md`. A positional path or
`--workflow path` overrides that default.

The file uses optional YAML front matter plus a Markdown prompt body:

```md
---
tracker:
  kind: github
  repository: owner/repo
  active_states: [OPEN]
  terminal_states: [CLOSED]
  labels: [codex]
workspace:
  root: ./.symphony/workspaces
publish:
  enabled: true
  remote: origin
  base_branch: main
  branch_prefix: symphony
  draft: false
  remove_labels: [codex]
  monitor_checks: true
  check_interval_ms: 30000
  max_check_rework_attempts: 3
debug:
  enabled: true
  address: 127.0.0.1:34000
  event_limit: 200
polling:
  interval_ms: 30000
agent:
  max_concurrent_agents: 2
  max_turns: 10
codex:
  command: codex app-server
  approval_policy: on-request
  thread_sandbox: workspace-write
  turn_timeout_ms: 3600000
---

Work on {{ issue.identifier }}: {{ issue.title }}.

{{ issue.description }}

Labels:{% for label in issue.labels %} {{ label }}{% endfor %}
```

The prompt renderer is strict: unknown variables and unknown filters fail the
affected run attempt. It supports `{{ issue.title }}`, `{{ attempt }}`,
`{% if attempt %}...{% else %}...{% endif %}`, and
`{% for label in issue.labels %}...{% endfor %}`.

## Trackers

### GitHub Issues

`tracker.kind: github` is an Atteler extension for teams using GitHub Issues
instead of Linear.

Required config:

- `tracker.repository: owner/repo`, or `tracker.owner` plus `tracker.repo`

Authentication is resolved in this order:

- `tracker.api_key`, including `$ENV_VAR` indirection
- `GITHUB_TOKEN`
- `GH_TOKEN`
- `gh auth token`
- `gh auth status --show-token` as a compatibility fallback for keyring-backed
  GitHub CLI sessions

Defaults:

- `tracker.endpoint: https://api.github.com`
- `tracker.active_states: [OPEN]`
- `tracker.terminal_states: [CLOSED]`

GitHub labels are normalized to lowercase. Labels like `p0`, `p1`, `p2`,
`p3`, and `p4` are mapped to Symphony priority values. Pull requests returned
by GitHub's Issues API are ignored.

## Publishing And Finalization

`publish.enabled: true` turns a successful worker run into an explicit GitHub
publication path:

- commit the worker workspace locally
- set `publish.remote` to the GitHub repository URL
- push `publish.branch_prefix/<issue identifier>` to that remote
- open or reuse a pull request against `publish.base_branch`
- remove `publish.remove_labels` from the source issue

Removing the dispatch label is the finalization signal for GitHub-backed
workflows: once the PR exists, the issue no longer matches the tracker label
filter and Symphony will not redispatch it.

With `publish.monitor_checks: true`, published PRs enter a separate check
monitor lane. Symphony polls GitHub check runs and commit-status contexts for
the PR head. Passing checks complete the monitor. Failing checks dispatch a PR
rework worker on the same branch, then the publish path commits, pushes, and
reuses the existing PR. This loop is capped by
`publish.max_check_rework_attempts`; it does not re-add the source issue label
or put the issue back into the normal dispatch queue.

Publishing is currently supported for `tracker.kind: github`. The GitHub token
is the same resolved token used by the tracker, including `gh auth token`; git
pushes use a temporary `GIT_ASKPASS` helper so the token is not required in the
workflow file.

## Debug API

`debug.enabled: true` starts a local HTTP server, defaulting to
`127.0.0.1:34000`, with operator-friendly JSON endpoints:

- `GET /debug/healthz` returns `ok`
- `GET /debug/status` returns workflow/config summaries, running workers,
  queued retries, watched pull requests, recent events, token totals, and a
  `summary` object with `what_happened`, `what_is_going_on`, and
  `what_will_do`
- `GET /debug/events` returns the recent scheduler event ring

The API is intended for local debugging and does not expose tracker tokens.
Keep the address bound to localhost unless you are deliberately exposing local
process state.

### Linear

`tracker.kind: linear` follows the draft specification.

Required config:

- `tracker.project_slug`
- `tracker.api_key`, or `LINEAR_API_KEY`

Defaults:

- `tracker.endpoint: https://api.linear.app/graphql`
- `tracker.active_states: [Todo, In Progress]`
- `tracker.terminal_states: [Closed, Cancelled, Canceled, Duplicate, Done]`

## Workspaces And Hooks

Workspaces are created under `workspace.root`, defaulting to the host temp
directory's `symphony_workspaces`. Issue identifiers are sanitized by replacing
characters outside `[A-Za-z0-9._-]` with `_`.

Supported hooks:

- `hooks.after_create`
- `hooks.before_run`
- `hooks.after_run`
- `hooks.before_remove`
- `hooks.timeout_ms`

Hooks run through `bash -lc` with the workspace as `cwd`. Symphony sets:

- `SYMPHONY_HOOK`
- `SYMPHONY_WORKSPACE_PATH`
- `SYMPHONY_WORKSPACE_KEY`
- `SYMPHONY_ISSUE_ID`
- `SYMPHONY_ISSUE_IDENTIFIER`
- `SYMPHONY_ISSUE_TITLE`
- `SYMPHONY_ISSUE_STATE`

## Codex App-Server Policy

The runner launches `codex.command` with `bash -lc` inside the issue workspace
and speaks the current Codex app-server JSONL protocol over stdio.

Implementation-defined safety posture:

- Approval requests for command execution and file changes are auto-approved
  for the current app-server session.
- Permission requests grant session-scoped network permission and otherwise
  leave filesystem policy to the configured Codex sandbox.
- Unsupported dynamic client-side tool calls return structured
  `success=false` tool output so the turn does not stall.
- User-input-required and MCP elicitation requests fail the run attempt
  immediately; the orchestrator retries according to the configured retry
  policy.
- `codex.thread_sandbox`, `codex.turn_sandbox_policy`, and
  `codex.approval_policy` are passed through to the installed Codex app-server
  protocol. `thread_sandbox` accepts Codex sandbox mode strings. For
  `turn_sandbox_policy`, Symphony accepts the same friendly strings
  (`read-only`, `workspace-write`, `danger-full-access`) and sends the
  app-server's typed policy object.

## Runtime Behavior

The orchestrator:

- reloads `WORKFLOW.md` when file metadata changes and keeps the last known good
  config on invalid reloads
- reconciles running issues before dispatching new work
- dispatches by priority, creation time, then identifier
- enforces global and per-state concurrency limits
- preserves successful workspaces
- removes terminal issue workspaces during startup cleanup and active-run
  reconciliation
- schedules one-second continuation retries after clean worker exits unless a
  publish step opened or reused a pull request and finalized the issue
- monitors published pull requests for failed checks when
  `publish.monitor_checks` is enabled, and dispatches same-branch rework without
  putting the source issue back into the issue queue
- schedules exponential failure retries capped by `agent.max_retry_backoff_ms`

Logs are emitted through `slog` as `key=value` text on stderr. Set
`SLOG_LEVEL=debug`, `info`, `warn`, or `error`.
