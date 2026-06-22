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
  draft_on_failed_validation: true
  verification_gates:
    - name: go_test
      command: go test ./...
      required: true
      timeout_ms: 600000
  verification_allow_commands: [go]
  verification_output_max_bytes: 32768
  remove_labels: [symphony]
  monitor_checks: true
  required_checks: []
  required_check_patterns: []
  discover_required_checks: true
  no_checks_policy: pass
  rework_optional_checks: false
  check_interval_ms: 30000
  max_check_rework_attempts: 3
  git_user_name: tommoulard
  git_user_email: tom@moulard.org
debug:
  enabled: true
  address: 127.0.0.1:34000
  event_limit: 200
hooks:
  before_run: |
    set -euo pipefail
    repo_root="$(cd ../../.. && pwd)"
    base_branch="${SYMPHONY_BASE_BRANCH:-main}"
    worker_branch="symphony/${SYMPHONY_WORKSPACE_KEY}"
    if [ ! -d .git ]; then
      git clone --shared "$repo_root" .
    fi
    git config user.name "tommoulard"
    git config user.email "tom@moulard.org"
    if [ -n "$(git status --porcelain)" ]; then
      git add -A
      if ! git diff --cached --quiet; then
        git commit --no-verify -m "chore: preserve Symphony WIP for ${SYMPHONY_WORKSPACE_KEY}"
      fi
    fi
    git fetch origin "+refs/heads/${base_branch}:refs/remotes/origin/${base_branch}"
    base_ref="origin/${base_branch}"
    if git rev-parse --verify --quiet "refs/heads/${worker_branch}" >/dev/null; then
      ahead="$(git rev-list --count "${base_ref}..${worker_branch}" 2>/dev/null || printf '0')"
      if [ "$ahead" != "0" ]; then
        git checkout "$worker_branch"
        exit 0
      fi
    fi
    ahead_head="$(git rev-list --count "${base_ref}..HEAD" 2>/dev/null || printf '0')"
    if [ "$ahead_head" != "0" ]; then
      git checkout -B "$worker_branch"
      exit 0
    fi
    git checkout -B "$worker_branch" "$base_ref"
polling:
  interval_ms: 30000
agent:
  max_concurrent_agents: 1
  max_turns: 12
  max_retry_backoff_ms: 300000
codex:
  command: codex app-server
  approval_policy: on-request
  thread_sandbox: workspace-write
  turn_sandbox_policy: workspace-write
  turn_timeout_ms: 3600000
  read_timeout_ms: 30000
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
