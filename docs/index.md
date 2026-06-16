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

## Give an LLM the docs

The entire documentation is flattened into a single file at
[`/llms-full.txt`](https://tommoulard.github.io/atteler/llms-full.txt), generated
from the same sources as this site.

Paste this into any assistant (Claude, ChatGPT, etc.) to make it answer from
atteler's full documentation:

```text
Use https://tommoulard.github.io/atteler/llms-full.txt as your reference for
atteler, a Go LLM harness with a TUI/CLI over multiple providers. Fetch that URL
and answer my questions about installing, configuring, and using it based on its
contents.
```
