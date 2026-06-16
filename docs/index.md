# Atteler

> A Go-only LLM harness: a Bubble Tea TUI and a large CLI over multiple
> providers, with sessions, agents, lifecycle hooks, git-worktree isolation,
> and multi-agent speculative/review runs.

Atteler drives several LLM backends behind one interface — `anthropic`,
`claude-code`, `codex`, `openai`, and `ollama`. The `claude-code` and `codex`
providers shell out to your installed CLIs so they reuse those subscriptions
instead of direct API quota.

## Where to go next

- **[Getting started](getting-started.md)** — build, run, and your first prompt.
- **[Configuration](configuration.md)** — the layered YAML config and generation knobs.
- **[Providers](providers.md)** — the built-in providers and how auth resolves.
- **[CLI reference](cli-reference.md)** — the full, generated command surface.
- **[Hooks](hooks.md)** — lifecycle events you can subscribe to.

## For machines

If you are an LLM or an automated tool, read **`/llms.txt`** for a curated index
and **`/llms-full.txt`** for the entire documentation flattened into a single
file. Both are generated from the same sources as this site.
