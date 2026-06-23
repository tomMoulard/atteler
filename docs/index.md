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
- **[Go SDK](sdk.md)** — the stable package surface, examples, and compatibility policy.
- **[Hooks](hooks.md)** — lifecycle events you can subscribe to.

## Versioned documentation

Documentation is published per version so links can stay stable even when
configuration options, commands, or provider behavior change later:

- **[`/main/`](https://tommoulard.github.io/atteler/main/)** is rebuilt from
  the `main` branch on every push.
- **`/vX.Y.Z/`** snapshots, such as
  [`/v0.0.7/`](https://tommoulard.github.io/atteler/v0.0.7/), are published
  from release tags and remain available for users on older releases.

Use the version selector in the header to switch between the same page in
another published version when it exists. Existing unversioned page links
redirect to `main` for compatibility, but new references should use a versioned
URL such as `/main/configuration/#local` or `/v0.0.7/configuration/#local`.

## Give an LLM the docs

Each documentation version includes a flattened LLM reference generated from the
same sources as that version of the site. For the current `main` docs, use
[`/main/llms-full.txt`](https://tommoulard.github.io/atteler/main/llms-full.txt).

Paste this into any assistant (Claude, ChatGPT, etc.) to make it answer from
atteler's current `main` documentation:

```text
Use https://tommoulard.github.io/atteler/main/llms-full.txt as your reference for
atteler, a Go LLM harness with a TUI/CLI over multiple providers. Fetch that URL
and answer my questions about installing, configuring, and using it based on its
contents.
```
