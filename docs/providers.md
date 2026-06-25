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

Borrowed/local CLI credential stores are a trust boundary. By default Atteler
borrows the well-known local stores it can find — `env`, `codex_auth_json`,
`claude_code_keychain`, `claude_code_file`, and `forge_credentials` — and may
read, use, refresh, and write them back, so it reuses your existing
Codex/Claude Code/ForgeCode subscriptions out of the box. Stores outside that
list and the `*` wildcard still require an explicit opt-in.

To lock a provider down, configure its `credential_policy` explicitly. A
configured policy replaces the default rather than extending it, so any field
you omit reverts to its conservative zero value (for example
`allow_borrowed_oauth: false`):

```yaml
providers:
  codex:
    credential_policy:
      allowed_stores: [env]                 # stop borrowing ~/.codex/auth.json
  claude-code:
    credential_policy:
      allowed_stores: [claude_code_keychain, claude_code_file]
      allow_borrowed_oauth: true
      allow_refresh: true
      allow_write_back: true
  anthropic:
    credential_policy:
      allowed_stores: [env, forge_credentials, claude_code_keychain, claude_code_file]
      allow_borrowed_oauth: true
      allow_refresh: true
      allow_write_back: true
```

`allowed_providers` can further narrow a policy to resolved provider names
(for example `anthropic`, `codex`, or `openai`). External CLI ownership remains
controlled by `allowed_stores` plus `allow_borrowed_oauth`. Omitting
`credential_policy` entirely keeps the borrow default; `allowed_stores: []` on a
configured policy intentionally denies every credential store.
Refresh/write-back attempts, failures, and CAS-conflict events are recorded in
`credential_events.jsonl` under `ATTELER_COMMAND_AUDIT_DIR` (or the default
temporary audit directory), alongside the side-effect permission ledger, with
source/store/provenance only and no token values.

The same policy can be bootstrapped from environment variables when config
files are not available:

- `ATTELER_CREDENTIAL_ALLOWED_PROVIDERS`
- `ATTELER_CREDENTIAL_ALLOWED_STORES`
- `ATTELER_ALLOW_BORROWED_OAUTH`
- `ATTELER_ALLOW_CREDENTIAL_REFRESH`
- `ATTELER_ALLOW_CREDENTIAL_WRITE_BACK`
- `ATTELER_TRUST_BORROWED_CREDENTIALS` (broad opt-in: all stores plus
  borrowed OAuth, refresh, and write-back)

Disable the borrowed-credential adapters entirely with
`disable_private_adapter: true` (or the `ATTELER_DISABLE_*_ADAPTER` env vars)
without disabling the normal providers. For the exact execution path,
credential source, token-refresh behavior, endpoint, and health check of every
provider, see the generated **Provider runtime contracts** and **compatibility
matrix** in the [project README](https://github.com/tomMoulard/atteler#providers).

## Authentication

Auth resolves in layers (`pkg/llm/auth.go` and the keychain helpers): environment
variables, on-disk auth files (`~/.codex/auth.json`, ForgeCode credentials), and
the macOS keychain, but each non-env layer must be allowed by credential-source
policy before it is read or refreshed. Environment variables remain explicit
inputs, but borrowed OAuth values such as `CLAUDE_CODE_OAUTH_TOKEN` still require
`allow_borrowed_oauth: true`.
