# Common workflows

> Task-oriented recipes for everyday Atteler runs, from one-shot chat to multi-agent review.

## Chat once or interactively

Run the TUI with no arguments, or fire a single prompt. Pipe a diff in with
`--stdin`, and use `@path` tokens to attach local files or directories — they
are appended to the provider request without changing the visible transcript.

```sh
atteler
atteler chat once "Explain this repository in one paragraph"
git diff | atteler chat once "Review this diff" --stdin
atteler chat once "Summarize @README.md and @pkg/llm/llm.go"
```

Use the `autonomy` flag (`low|medium|high|full`) to set action boundaries:
`low` is advisory-only and disables tools, `medium` allows local edits but
blocks branches/commits/pushes/PRs, and `high`/`full` can prepare or publish a
PR. Worktree create/merge operations require `high` or `full`.

## Run headless jobs

Headless runs are real jobs with durable metadata, cancellation, and recovery.
Set a stable `--headless-id` when another process needs a handle. Metadata,
event summaries, and logs are redacted by default.

```sh
atteler chat once "Summarize @README.md" --headless --headless-id docs-summary --output json
atteler session headless
atteler session status-headless docs-summary
atteler session stream-headless docs-summary
atteler session cancel-headless docs-summary
atteler session retry-headless docs-summary
atteler session recover-headless
atteler session cleanup-headless --headless-max-age 168h
```

Launch nested work with `ATTELER_HEADLESS_PARENT_ID=<id>` to record
parent/child relationships in metadata and events.

## Replay a session without provider calls

Record a response to a fixture, then replay it. Replay writes normal session
messages while avoiding provider availability and sampling noise in tests.

```sh
atteler chat once "Summarize @README.md" --record-response .atteler/fixtures/readme-summary.json
atteler chat once "Summarize @README.md" --replay-response .atteler/fixtures/readme-summary.json
```

Recorded responses under `.atteler/fixtures/*.json` are raw provider outputs and
are ignored/private by default. Commit only reviewed, redacted fixtures renamed
to `.atteler/fixtures/**/*.fixture.{json,yaml,yml}`.

## Run eval checks

Assert against a recorded fixture or a structured eval suite. Reports are JSON
with per-assertion status, evidence, pass rate, and run metrics for CI.

```sh
atteler eval output .atteler/fixtures/readme-summary.txt \
  --eval-expected "package overview" \
  --eval-mode contains
atteler eval run .atteler/evals/readme.eval.yaml \
  --eval-json \
  --eval-report .atteler/eval-report.json
atteler eval record reviewer --evaluation-report report.json
```

`.atteler/eval-report*.json` and ad-hoc outputs are ignored/private by default;
commit only reviewed suites named `.atteler/evals/**/*.eval.{json,yaml,yml}`.

Suites combine required/forbidden content, regex, JSON/YAML path, schema,
numeric, artifact, exit-code, and recorded judge assertions. Judge assertions
only replay recorded decisions — the runner never calls a judge model. Golden
updates require both `--eval-update-golden` and `--eval-approve-golden-update`.

## Isolate work in a git worktree

`atteler worktrees run` creates an isolated git worktree for a session.
Worktrees are preserved on exit by default; merge after review. Exit-time
auto-merge is opt-in and requires passing verification commands.

```sh
atteler worktrees run "Add unit tests for the auth package"
atteler worktrees list
atteler worktrees merge 20260430-120000-deadbeef
```

Successful merges report the diff summary, verification commands run, the
resulting commit SHA, transaction log, and rollback commands. See
`atteler help worktrees` for the full command contract.

## Multi-agent review and speculation

Plan and run review agents over specific paths with explicit gates, or run
several agents speculatively and let a judge pick a winner. Speculative
verdicts fail closed: the judge must emit one `GATE <name>: PASS|FAIL` line per
required gate, and model silence is never success.

```sh
atteler agents plan "review this auth change" --plan-max-agents 3
atteler review plan \
  --review-agent quality-reviewer \
  --review-agent test-engineer \
  --review-path pkg/llm/auth.go
atteler review run \
  --review-agent quality-reviewer \
  --review-path pkg/llm/auth.go \
  --review-gate "tests pass"
atteler agents speculate-run \
  --speculate-agent planner \
  --speculate-agent verifier \
  --speculate-gate "tests pass" \
  --speculate-prompt "pick the safest migration plan"
```

Review and speculative runs are persisted as first-class session receipts.
Inspect recorded evidence without re-calling providers:

```sh
atteler session runs
atteler session show-run latest
atteler session export-run review
atteler session replay-run speculation
atteler session resume-run latest
```

## Auto mode (self-fork orchestration)

`--auto` turns the main model into an orchestrator that forks atteler into
worker sub-agents through the bash tool: it can spawn an `explorer` to map the
code, a `planner`, several `implementer`s on different models, and a `reviewer`,
then synthesize the result. Pick a playbook with `--auto=<mode>` (`auto`, the
default, `bug-hunt`, or `autoresearch`).

```sh
atteler --auto --once "implement structured logging across pkg/llm"
atteler --auto=bug-hunt --once "the registry drops providers under load — find why"
```

Recursion is bounded by `--auto-max-depth` (default 2): once the depth budget is
exhausted, auto mode gracefully downgrades to a single agent. Because forking
needs the bash tool, `--auto` raises the autonomy floor to `medium`.

To default interactive (TUI) sessions into auto mode, set `auto` in config (see
[Configuration](configuration.md#auto-mode)); headless one-shots stay opt-in via
the flag, and a CLI `--auto` always overrides the config value.

Forked children authenticate with borrowed file credentials (Claude Code,
Codex) because the bash sandbox redacts credential environment variables — so
auto mode works with atteler's primary auth model, not with bare
`ANTHROPIC_API_KEY`/`OPENAI_API_KEY` environment keys.

## Autoresearch loops

Use autoresearch when the right answer is likely to need many small
change-and-validate attempts. The shortcut below starts a headless run in an
isolated worktree with `--auto=autoresearch`, `--headless`, and `--autonomy=high`
so the agent can commit kept candidates and reset discarded ones:

```sh
atteler autoresearch run "Improve agent-loop recovery; keep only changes that pass make test"
atteler autoresearch "Reduce prompt-context cache misses and validate with go test ./cmd/atteler"
atteler session headless
atteler session stream-headless <run-id>
atteler worktrees list
```

The playbook establishes a baseline evaluator, writes ignored ledgers under
`.atteler/runs/autoresearch/<run-id>/`, commits each candidate before
validation, keeps improvements, and resets regressions. If your mission has a
specific metric or command, put it in the prompt; otherwise the agent chooses the
smallest meaningful repo-local gate first and broadens verification before
claiming success.

## Research runs

Use `atteler research run` when you need an auditable technical research packet
before implementation, architecture, dependency, security, or planning work:

```sh
atteler research run "Compare approaches for plugin sandboxing in Go CLIs"
atteler research run \
  --trusted-source go.dev \
  --trusted-source github.com \
  --deny-source example-content-farm.com \
  --warn-low-trust \
  "Research best practices for safe agent worktrees"
atteler research run \
  --output .atteler/research/plugin-sandboxing \
  --generate-tasks \
  "Find viable implementation approaches for sandboxing Atteler plugins"
```

The MVP is local-first. It creates `.atteler/runs/research/<run-id>/` by
default (or the directory passed with `--output` / `--research-output`) and
writes ignored/private artifacts until you review and copy a redacted summary to
a committed docs location:

- `research.md` — human-readable summary, findings, tradeoffs,
  recommendations, risks, claims, and citations.
- `sources.jsonl` — structured source records for discovered project guidance
  and supplied `--research-source` files/URLs, including source type, domain,
  trust level, trust score, policy match, and low-trust warnings.
- `claims.jsonl` — important claims mapped to evidence where available.
- `run.json` — run metadata and artifact paths.
- `tasks.generated.yaml` — optional follow-up task stubs when
  `--generate-tasks` is set.

Before writing recommendations, the command inspects project/harness guidance
files when present, including `AGENTS.md`, `CLAUDE.md`, `.cursor/rules/*`, and
similar agent instruction files. The discovered guidance is included as source
context and cited in the report. Source-related guidance in those files, such
as preferred official docs, denied domains, citation requirements, or low-trust
warnings, is folded into the effective source policy below Atteler config and
explicit CLI flags.

Project config can define source policy defaults:

```yaml
research:
  source_policy:
    trusted_domains: [go.dev, github.com, docs.anthropic.com, openai.com]
    denied_domains: [example-content-farm.com]
    prefer_source_types: [official_docs, source_code, standard_or_spec, issue_discussion]
    allow_low_trust_sources: true
    warn_on_low_trust_sources: true
    require_evidence_for_high_impact_claims: false
```

`--trusted-source` and `--deny-source` extend the effective policy for a single
run. Denied sources are omitted from `sources.jsonl` and listed in `run.json`;
low-trust sources remain allowed by default but are marked in artifacts and
warned about when policy asks for warnings.

Atteler research reports should include evidence for important claims whenever
possible. Evidence can include URLs, documentation links, repository files,
command output, tests, logs, or prior session artifacts. This improves
reliability and makes research easier to audit, but evidence is not mandatory
for every statement.

## Continuous watch and incidents

Scan the working tree for quality debt on demand, as JSON, or in a loop. Watch
findings can upsert deduplicated GitHub issues. `incident diagnose` normalizes
incident context from Sentry, a redacted JSON fixture, or an MCP connector.

```sh
atteler watch scan
atteler watch json
atteler watch loop
atteler incident diagnose --sentry ISSUE-912
atteler incident diagnose --incident-file redacted-sentry-event.json
atteler incident diagnose --sentry ISSUE-912 \
  --incident-apply-fix \
  --incident-validation-command "go test ./pkg/auth" \
  --incident-open-pr
```

The `--incident-apply-fix` switch is an explicit approval gate for the
credentialed repair loop; PR creation requires it plus at least one captured
validation command.

## Memory, RAG, and code intelligence

Search lexical memory or vector indexes, and query the language-neutral
workspace code index. Lexical vector mode is a deterministic fallback (not
semantic); embedding mode uses an Ollama-compatible endpoint.

```sh
atteler memory search "OAuth retry storm"
atteler memory retrieve "OAuth retry storm" --retrieval-explain
atteler memory retrieve "OAuth retry storm" --trusted-source docs.github.com --deny-source example-content-farm.com
atteler memory vector-search "redirect risks" --vector-index docs/research.md --vectorizer lexical
atteler memory migrate
atteler code-intel summary --json
atteler code-intel symbol NewRegistry
atteler code-intel query definitions:NewRegistry
```

Run `atteler memory migrate` / `agent-migrate` after changing a store schema,
redaction policy, or vectorizer. Vectorizer settings are scoped per store,
agent, and source — see [Configuration](configuration.md).
Retrieval results carry source-quality metadata from harness guidance,
`research.source_policy`, and per-run source flags; denied sources are filtered
and equally relevant results prefer higher-trust sources before the stable
source/name tie-breakers. When low-trust warnings are enabled, retrieval output
includes a `source_warning` field for weak evidence.

## Use plugins, MCP, and LSP

Configured `plugins.paths` entries point at local plugin directories or
manifests. Inspect and run entrypoints, using `--plugin-dry-run` to check the
policy envelope before execution. MCP servers are referenced through manifests
(for example by `incident diagnose`).

```sh
atteler plugins list
atteler plugins describe reviewer
atteler plugins run reviewer/check --plugin-dry-run
atteler incident diagnose --incident-ref alert-42 \
  --incident-mcp-manifest .atteler/mcp.yaml \
  --incident-mcp-server grafana \
  --incident-mcp-tool get_incident
```

Plugin runs require an accepted local policy that acts as an upper bound;
manifests requesting anything outside it fail before execution. MCP manifests
under `.atteler/` are ignored/private by default because they can contain local
server paths, tool arguments, or integration details.

## Synthesize and manage skills

Suggest a skill from repeated steps, then review the generated diff before
saving. Automatic skill learning records redacted workflow observations and
writes ignored/private generated skills under `.atteler/skills/generated/`.
Commit only reviewed skills copied to `.atteler/skills/curated/`.

```sh
atteler agents skill-suggest plan --skill-step code --skill-step test
atteler agents skill-learning-list
atteler agents skill-learning-show k8s-investigation
atteler agents skill-learning-disable k8s-investigation
```

Use `--skill-review-only` to inspect the diff without writing files. Disable
learning with `skill_learning.enabled: false` or `ATTELER_SKILL_LEARNING=false`.

## Export and share sessions

Exports default to the redacted shareable profile; credential patterns and
local absolute paths are scrubbed and a provenance manifest is included.

```sh
atteler session export 20260430-120000-deadbeef --export-format markdown
atteler session export 20260430-120000-deadbeef --export-format issue
atteler session export 20260430-120000-deadbeef --export-format private-markdown
```

Use `private-markdown` or `private-json` only when recipients may see the full
raw session.

## See also

- [Configuration](configuration.md) — layered YAML config and generation knobs.
- [Providers](providers.md) — built-in providers and auth resolution.
- [Hooks](hooks.md) — lifecycle event hooks.
- [CLI reference](cli-reference.md) — the complete command and flag surface.
