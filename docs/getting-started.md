# Getting started

> Build atteler, run it from a clone, and explore the command surface offline.

Atteler is a Go CLI for running LLM-assisted development workflows from a local
repository: chat, provider routing, local context references, session history,
review/watch scans, agent planning, and worktree isolation.

## Run from a clone

The quickest way to try atteler is to run it straight from a checkout:

```sh
go run ./cmd/atteler help
go run ./cmd/atteler chat once "Explain this repository"
go run ./cmd/atteler review scan
```

## Install or build a binary

Install the latest released binary, or build one locally with the Makefile:

```sh
go install github.com/tommoulard/atteler/cmd/atteler@latest
make build
./atteler help
```

`make build` compiles `./atteler` from `./cmd/atteler`. The bare `atteler`
command (or `make run`) launches the interactive TUI and requires a real
terminal.

## One-shot prompts (no TUI)

Use `chat once` for a single, non-interactive completion:

```sh
atteler chat once "Explain this repository in one paragraph"
atteler chat once "Summarize" --model openai/gpt-5.4
```

## The command surface

Atteler keeps top-level help short and routes discoverability through focused
domains. Run `atteler help` for the domain list, `atteler help <domain>` for one
area, and `atteler help legacy` for the compatibility flag catalog. If you know
an old flag name, `atteler help --code-summary` jumps to the domain that owns it.

A few representative domains:

```sh
atteler session list
atteler providers list
atteler agents plan "review auth changes"
atteler review scan
atteler worktrees run "Add unit tests for auth"
```

Common options for model, agent, output, and provider routing can be combined
with domain commands before or after the subcommand, for example
`atteler session --session <id> messages`. See the
**[CLI reference](cli-reference.md)** for the full catalog.

## Offline inspection

These commands never hit the network or require credentials — useful in CI and
for discovery:

```sh
atteler config paths               # config files in load order
atteler config validate            # check configuration
atteler providers list             # built-in provider names
atteler providers known-models     # built-in provider/model IDs, no API calls
atteler providers resolve gpt-5.5  # which provider/model a name resolves to
```

`atteler providers resolve <model>` is handy when routing is unclear: it prints
the selected provider/model when resolution is safe, and otherwise lists every
provider claim considered, with provenance markers so stale catalogs are not
mistaken for fresh live ones.

## Local verification

Use the Makefile so pinned tool versions stay consistent:

```sh
make test                                  # full suite with -race -count=1
make test TESTPACKAGE=./pkg/llm            # narrow to one package
make lint                                  # golangci-lint
make all                                   # generate, lint, test, build (CI)
```

## Next steps

- [Configuration](configuration.md) — layered YAML config and generation knobs.
- [Providers](providers.md) — built-in providers and auth resolution.
- [Hooks](hooks.md) — lifecycle event hooks.
- [CLI reference](cli-reference.md) — the complete command and flag surface.
