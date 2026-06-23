# Symphony

Atteler includes a standalone `symphony` command that implements the Symphony
service contract from OpenAI's draft specification.

Run the long-lived scheduler via the standalone command:

```sh
go run ./cmd/symphony --validate
go run ./cmd/symphony
go run ./cmd/symphony ./WORKFLOW.md
make run-symphony
make build-symphony
./symphony ./WORKFLOW.md
```

For a one-shot autonomous issue-to-PR run from the main CLI, use:

```sh
atteler issue implement GH-218 --open-pr --run-tests --run-lint
atteler issue implement GH-218 --open-pr --base main
```

This loads the same `WORKFLOW.md`, fetches the referenced issue, and runs the
worker once. GitHub issue references can be passed as `GH-218`, `#218`, `218`,
or the issue URL. It only enables publication when `--open-pr` is present, even
if the workflow file has `publish.enabled: true`; without `--open-pr`, the
one-shot command leaves the workspace changes local for review. When publication is
enabled, configured and flag-selected verification gates run before the push/PR
step. When `--run-tests` or `--run-lint` provides the first verification gates
for the run, the command also seeds the local verification allow list with the
needed executables (`go` and/or `make`) instead of leaving the gate execution
surface open-ended.
Unlike the long-lived scheduler, the one-shot `--open-pr` path does not require
`publish.remove_labels`; ad-hoc issue implementation can publish a verified PR
even when there is no dispatch label to remove.
Use `--update-docs` and `--update-changelog` to add explicit worker
instructions for documentation or changelog edits when they are relevant; these
flags do not force blind documentation changes when the issue does not need
them.

For a safer background queue entry point that only prepares local artifacts and
worktrees, use issue watch:

```sh
atteler issue watch --github owner/repo --label atteler-agent --once
atteler issue watch --github owner/repo --label ready-for-ai --dry-run
atteler issue watch --github owner/repo --label atteler-agent \
  --command 'atteler --once "Read $ATTELER_ISSUE_WATCH_PLAN, implement locally, and do not publish."' \
  --validation-command "go test ./..." \
  --once
atteler issue list-candidates --github owner/repo --label ready-for-ai
atteler issue run 232 --github owner/repo
```

`issue list-candidates` uses the same filters without local writes, and `issue run <issue-ref>` prepares one issue directly. `issue watch` discovers open GitHub issues with the requested label, avoids
duplicates through `.atteler/issue-watch/state.json`, creates isolated local git
worktrees, and writes review artifacts under `.atteler/runs/issues/`. It does
not push branches, open pull requests, or post issue comments.

If `--command` is present, that local implementation command runs inside the
prepared worktree. Repeated `--validation-command` values run afterward, and the
captured command output, validation status, changed-file list, and refreshed
`patch.diff` stay in the local run directory. Direct network and credential
access from those command hooks is denied so the watcher remains local-only by
default. Commands receive artifact path environment variables such as
`ATTELER_ISSUE_WATCH_PLAN`, `ATTELER_ISSUE_WATCH_ISSUE_JSON`,
`ATTELER_ISSUE_WATCH_RUN_DIR`, and `ATTELER_ISSUE_WATCH_PATCH`. Nested
`atteler issue implement --open-pr` calls are refused while running under issue
watch so publication remains a separate explicit workflow. Nested Atteler bash
tools also deny shell-network commands such as `curl`, `gh`, or `git push`
while `ATTELER_ISSUE_WATCH=1`.

If the one-shot worker fails after preparing a workspace, or completes without
leaving publishable changes, `--open-pr` uses the same draft fallback control
(`publish.draft_on_failed_validation`, enabled by default in resolved config)
to publish a draft PR with an auditable failure report. When no partial diff was
captured, Symphony creates an empty draft commit so the failure remains linked
to the source issue instead of disappearing into local logs. If the draft
fallback is disabled, the one-shot command fails instead of silently treating a
no-change worker run as a verified implementation.

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
autonomy: high
workspace:
  root: ./.symphony/workspaces
publish:
  enabled: true
  remote: origin
  base_branch: main
  branch_prefix: symphony
  draft: false
  draft_on_failed_validation: true
  verification_gates:
    - go_test                  # preset: go test ./...
    - name: lint
      command: make lint
      required: true
      timeout_ms: 600000
  verification_allow_commands: [go, make]
  verification_deny_commands: [curl, wget]
  verification_output_max_bytes: 32768
  remove_labels: [codex]
  monitor_checks: true
  required_checks: []          # exact check/status names, optional
  required_check_patterns: []  # glob patterns where * matches any substring
  discover_required_checks: true
  no_checks_policy: pass       # pass, pending, or fail when no required checks exist
  rework_optional_checks: false
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
`{% for label in issue.labels %}...{% endfor %}`. GitHub issue comments are
available as `issue.comments`; when a workflow template does not reference
`issue.comments`, Symphony appends the issue discussion automatically to the
first worker prompt so maintainer comments are not silently dropped.

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
- `gh auth token` when autonomy permits local CLI credential access

If `gh auth token` cannot return a token, configure one of the explicit token
sources above. Symphony does not use broader GitHub CLI token-dump commands as
a compatibility fallback.

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
- run configured local verification gates and capture bounded evidence
- set `publish.remote` to the GitHub repository URL
- push `publish.branch_prefix/<issue identifier>` to that remote
- open or reuse a pull request against `publish.base_branch` with a generated
  report covering changes, validation, risk, reviewer notes, suggested
  reviewers, and issue linkage
- remove `publish.remove_labels` from the source issue

Publishing requires `autonomy: high` or `autonomy: full`; lower levels block
branch creation, commits, pushes, and PR creation. Even `full` autonomy never
merges PRs automatically.
When publishing with `autonomy: full`, set `publish.monitor_checks: true`; the
workflow is rejected otherwise because full autonomy must report whether CI
passed or failed. Use `autonomy: high` for PR creation without check
monitoring.
For one-off service runs, `symphony --autonomy low|medium|high|full` overrides
the workflow autonomy while preserving the same hard capability boundaries on
reloads.

Removing the dispatch label is the finalization signal for GitHub-backed
workflows: once the PR exists, the issue no longer matches the tracker label
filter and Symphony will not redispatch it.

Local PR verification gates are configured with
`publish.verification_gates`. Entries can use presets (`go_test`,
`go_build`, `make_test`, `make_lint`, `golangci_lint`) or explicit commands:

```yaml
publish:
  verification_gates:
    - go_test
    - name: docs
      command: test -f README.md
      required: false
      timeout_ms: 30000
  verification_allow_commands: [go, test]
  verification_deny_commands: [curl, wget]
  draft_on_failed_validation: true
  verification_output_max_bytes: 32768
```

Symphony runs these commands from the issue workspace through Atteler's audited
shell policy before pushing/opening the PR. `verification_allow_commands` and
`verification_deny_commands` are passed into that policy so operators can make
the executable surface explicit; the verification policy also denies
network-like commands named directly in the configured command string by
default. This is an audited process-launch policy, not a syscall sandbox: only
allow wrappers such as `make` when the repository-controlled recipes they run
are trusted for verification. Required gate failures are never silently
skipped: when `publish.draft_on_failed_validation: true` (the default for
resolved workflow config), Symphony continues by opening the PR as a draft and
puts the failed gate output in the PR body. Failed-validation draft bodies use
`Related to #...` rather than an issue-closing keyword until a later passing
run refreshes the PR report. If the deterministic branch already
has an open non-draft PR, Symphony attempts to convert that PR back to draft
through GitHub before finalizing the issue. Conversely, when a later run updates
an existing draft PR and all required gates pass, Symphony attempts to mark the
PR ready for review unless `publish.draft: true` still requires draft mode. When
the fallback is false, publication stops before the push/PR step. After
configured gates run, Symphony also checks that the workspace is still clean;
verification commands that modify generated files or docs are reported as a
required `workspace_clean` failure instead of being silently left out of the PR.
Captured commands and output are redacted with
Atteler's conservative secret scrubber, then truncated/fail-closed at
`publish.verification_output_max_bytes` so generated validation claims stay
bounded and backed by captured evidence without publishing obvious tokens.
The same draft fallback is used by `atteler issue implement ... --open-pr` when
the worker fails before reaching verification or exits without publishable
changes: the PR body records the incomplete run as a required `worker_run`
validation failure, marks the PR draft, and links it back to the source issue
without claiming the implementation is complete.

With `publish.monitor_checks: true`, published PRs enter a separate check
monitor lane. Symphony polls GitHub check runs and commit-status contexts for
the PR head, separates required checks from optional checks, and only dispatches
PR rework for required failures by default. Required checks come from exact
`publish.required_checks`, glob-style `publish.required_check_patterns`, and,
when `publish.discover_required_checks: true`, GitHub branch protection required
status checks and repository rulesets if the token can read them. Discovery
lookup failures are kept in the debug snapshot but do not fail polling;
configure required checks explicitly if token scopes cannot read protection or
rulesets. Branch-protection and ruleset lookup errors are reported separately
in the debug snapshot. Repositories without configured or discovered required
checks follow `publish.no_checks_policy` (`pass`, `pending`, or `fail`).
Optional failing checks are reported in debug snapshots and rework prompts for
context, but only trigger rework when
`publish.rework_optional_checks: true`. Check-run conclusions follow an explicit
policy: `success`, `neutral`, and `skipped` pass; `failure`, `cancelled`,
`canceled`, `timed_out`, `action_required`, `startup_failure`, and `stale`
fail; incomplete runs or completed runs without a conclusion stay pending; any
unknown conclusion is treated as failed.

If GitHub reports that the PR branch is behind the base branch or has merge
conflicts, Symphony fetches the PR branch and base branch, rebases locally, and
pushes the updated PR branch with `--force-with-lease`. Rebase failures or dirty
workspaces dispatch a PR rework worker on the same branch so Codex can resolve
conflicts. Once an automatic branch update fails for a given PR head/base pair,
Symphony keeps that failure as pending rework instead of retrying the same
rebase on every poll. When the worker starts, Symphony prepares the workspace on
the PR branch and leaves the rebase conflict in place when possible, so the
worker can resolve files, run `git rebase --continue`, and publish the fixed
branch. Passing required checks complete the monitor. Required failing checks
dispatch the same PR rework lane, then the publish path commits, pushes, and
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
  `what_will_do`. The config summary also includes redacted publication
  verification controls such as draft fallback, gate commands, command
  allow/deny lists, and output capture limits.
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
- `SYMPHONY_BASE_BRANCH`
- `SYMPHONY_ISSUE_ID`
- `SYMPHONY_ISSUE_IDENTIFIER`
- `SYMPHONY_ISSUE_TITLE`
- `SYMPHONY_ISSUE_STATE`

`SYMPHONY_BASE_BRANCH` is the resolved `publish.base_branch`, including the
one-shot `atteler issue implement ... --base` override, so setup hooks can
create the worker branch from the same base used for PR publication.

## Codex App-Server Policy

The runner launches `codex.command` with `bash -lc` inside the issue workspace
and speaks the current Codex app-server JSONL protocol over stdio.
Startup goes through Atteler's central permission/audit gate as an
`execute` plus Codex-specific `write`, `network`, and `credential_access`
side-effect request. A stricter policy can therefore block the app-server
before the `codex` process starts, and the command/side-effect ledgers record
the allow or deny reason.
Command execution, file-change, and session-network approval requests from the
app-server are also evaluated against the same policy before Atteler responds
to Codex.

Implementation-defined safety posture:

- Approval requests for command execution and file changes are auto-approved
  for the current app-server session only after the central policy allows their
  classified side effects.
- Permission requests grant session-scoped network permission only after the
  central network gate allows it and otherwise leave filesystem policy to the
  configured Codex sandbox.
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
  `publish.monitor_checks` is enabled, distinguishes required and optional
  checks, and dispatches same-branch rework for required failures without putting
  the source issue back into the issue queue
- schedules exponential failure retries capped by `agent.max_retry_backoff_ms`

Logs are emitted through `slog` as `key=value` text on stderr. Set
`SLOG_LEVEL=debug`, `info`, `warn`, or `error`.
