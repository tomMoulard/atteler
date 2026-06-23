# `.atteler/` artifact policy

Atteler writes repository-local state under `.atteler/`. Treat that tree as
private by default: raw transcripts, local task state, generated reports,
provider outputs, vector stores, memory stores, run ledgers, and worktrees can
contain prompts, paths, credentials in logs, or machine-specific state.

## Taxonomy

| Class | Examples | Git policy |
| --- | --- | --- |
| Transient/private local state | `.atteler/sessions/`, `.atteler/runs/`, `.atteler/research/`, `.atteler/worktrees/`, `.atteler/tasks.json`, `.atteler/config.{yaml,yml,json}`, `.atteler/events.jsonl`, adjacent `.atteler-state*` temp/state files, `.atteler/prompt-context-cache.json`, `.atteler/codeintel-index.json`, `.atteler/*-vector-index*.json`, `.atteler/agent-memory.json`, `.atteler/session-vector-index.json`, `.atteler/skill-learning/`, `.atteler/eval-report*.json`, recorded one-shot responses such as `.atteler/fixtures/once.json`, plugin scaffolds such as `.atteler/plugins/`, local MCP manifests such as `.atteler/mcp.yaml`, watch baselines such as `.atteler/watch-baseline.json`, and incident drafts such as `.atteler/incident.md` | Ignored. Do not commit unless manually redacted and moved to a reviewed asset path. |
| Generated-but-shareable reports | Redacted incident reports, research summaries, merged artifact bundles, eval reports prepared for review | Ignored at their default paths. Commit only after redaction, usually outside `.atteler/` or in a reviewed docs location. |
| Intentionally committed project assets | Reviewed eval suites `.atteler/evals/**/*.eval.{json,yaml,yml}`, redacted fixtures `.atteler/fixtures/**/*.fixture.{json,yaml,yml}`, curated skills under `.atteler/skills/curated/` | Allowed by `.gitignore` exceptions. Keep them deterministic, redacted, and small enough for review. |

## Review checklist before committing `.atteler/` content

- The file matches an allowed reviewable pattern above.
- Prompts, provider responses, local absolute paths, API keys, tokens, emails,
  and customer/user data are absent or redacted.
- The content is deterministic enough for future contributors to review and
  update intentionally.
- Generated reports include enough provenance to reproduce them, but not raw
  private transcripts.
