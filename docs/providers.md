# Providers

> The built-in LLM providers and how authentication resolves.

Atteler's `Provider` interface (`pkg/llm`) exposes `Name`, `Models`,
`FetchModels`, `HealthCheck`, `Complete`, and `ModelContextWindow`. The registry
(`AutoRegister`) is the canonical factory: it tries to construct every known
provider and silently skips any whose credentials are unavailable. Providers are
stateless and thread-safe — there are no package-level provider singletons.

## Built-in providers

<!-- The list below is generated from `atteler --list-providers`. -->

--8<-- "generated/providers.md"

The `claude-code` and `codex` providers let you reuse those subscriptions
instead of spending direct API quota — but they do **not** run the vendor CLIs.
They *borrow* the credentials those tools store locally (Claude Code OAuth from
the macOS keychain or `~/.claude/.credentials.json`; the ChatGPT auth in
`~/.codex/auth.json`) and then make direct HTTPS calls from atteler:
`claude-code` calls the Anthropic Messages API, and `codex` calls the ChatGPT
Codex Responses backend. There is no subprocess, CLI tool sandbox, or workspace
sandbox; atteler only forwards the tool definitions configured for the request.

Disable the borrowed-credential adapters with `disable_private_adapter: true`
(or the `ATTELER_DISABLE_*_ADAPTER` env vars) without disabling the normal
providers. For the exact execution path, credential source, token-refresh
behavior, endpoint, and health check of every provider, see the generated
**Provider runtime contracts** and **compatibility matrix** in the
[project README](https://github.com/tomMoulard/atteler#providers).

## Authentication

Auth resolves in layers (`pkg/llm/auth.go` and the keychain helpers): environment
variables, on-disk auth files (`~/.codex/auth.json`, ForgeCode credentials), and
the macOS keychain. Anthropic is the only provider that actively refreshes Forge
OAuth tokens.
