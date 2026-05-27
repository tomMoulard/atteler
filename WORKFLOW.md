---
tracker:
  kind: github
  repository: tomMoulard/atteler
  active_states: [OPEN]
  terminal_states: [CLOSED]
  labels: [symphony]
workspace:
  root: ./.symphony/workspaces
publish:
  enabled: true
  remote: origin
  base_branch: main
  branch_prefix: symphony
  draft: false
  remove_labels: [symphony]
  monitor_checks: true
  required_checks: []
  required_check_patterns: []
  discover_required_checks: true
  no_checks_policy: pass
  rework_optional_checks: false
  check_interval_ms: 30000
  max_check_rework_attempts: 3
debug:
  enabled: true
  address: 127.0.0.1:34000
  event_limit: 200
hooks:
  before_run: |
    repo_root="$(cd ../../.. && pwd)"
    if [ ! -d .git ]; then
      git clone --shared "$repo_root" .
    fi
    git checkout -B "symphony/${SYMPHONY_WORKSPACE_KEY}"
polling:
  interval_ms: 30000
agent:
  max_concurrent_agents: 2
  max_turns: 8
  max_retry_backoff_ms: 300000
codex:
  command: codex app-server
  approval_policy: on-request
  thread_sandbox: workspace-write
  turn_sandbox_policy: workspace-write
  turn_timeout_ms: 3600000
  read_timeout_ms: 5000
  stall_timeout_ms: 300000
---

You are working on GitHub issue {{ issue.identifier }} in tomMoulard/atteler.

Title: {{ issue.title }}
State: {{ issue.state }}
URL: {{ issue.url }}
Priority: {{ issue.priority }}
Labels:{% for label in issue.labels %} {{ label }}{% endfor %}

Issue description:
{{ issue.description }}

Work inside the prepared Symphony workspace for this issue. Inspect the
repository before editing, preserve unrelated user changes, and keep the diff
focused on the issue's requested behavior.

Implementation expectations:
- follow the repo's AGENTS.md guidance
- use existing package boundaries and helpers before adding new abstractions
- avoid changing the main atteler CLI unless the issue explicitly asks for it
- add or update focused tests for behavior changes
- run the smallest useful verification loop first, then broader checks when the
  change affects shared behavior

Before ending the run, leave the workspace in a reviewable state and summarize:
- files changed
- verification commands run
- any known risks or follow-up work
