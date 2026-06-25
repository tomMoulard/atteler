package llm

import (
	"fmt"
	"sort"
	"strings"
)

// ProviderRuntimeInfo documents the trust boundary and execution path for a
// built-in provider without requiring credentials or network access.
type ProviderRuntimeInfo struct {
	ExecutionPath    string
	CredentialSource string
	TokenRefresh     string
	NetworkEndpoint  string
	SandboxAndTools  string
	ModelInventory   string
	HealthCheck      string
}

// ProviderRuntime returns runtime documentation for a built-in provider.
func ProviderRuntime(name string) (ProviderRuntimeInfo, bool) {
	info, ok := providerRuntimeCatalog()[name]

	return info, ok
}

func providerRuntimeCatalog() map[string]ProviderRuntimeInfo {
	return map[string]ProviderRuntimeInfo{
		providerAnthropic: { // #nosec G101 -- documentation names credential sources, not secret values.
			ExecutionPath:    "Direct HTTPS calls from atteler to the Anthropic Messages API.",
			CredentialSource: "`ANTHROPIC_API_KEY` and `ANTHROPIC_AUTH_TOKEN` env vars by default; `CLAUDE_CODE_OAUTH_TOKEN` env var, ForgeCode credential files (`$FORGE_CONFIG/.credentials.json`, `~/forge/.credentials.json`, `~/.forge/.credentials.json`), and Claude Code keychain/`~/.claude/.credentials.json` are borrowed by default and can be restricted via `credential_policy`.",
			TokenRefresh:     "ForgeCode OAuth credentials refresh during credential resolution only when `credential_policy.allow_refresh` and `allow_write_back` allow it; write-back is lock/CAS-guarded and refresh/write-back attempts and failures are audited. The Anthropic adapter itself does not refresh on 401.",
			NetworkEndpoint:  fmt.Sprintf("`ANTHROPIC_BASE_URL` or provider config, default `%s`; `POST /v1/messages` for completions and `GET /v1/models` for model/health checks.", defaultAnthropicBase),
			SandboxAndTools:  "No subprocess or workspace sandbox. Atteler sends configured bash/read/write/edit/glob/grep tool definitions in the Messages request; tool execution happens in Atteler's agent loop.",
			ModelInventory:   "Known-model listing prints the static `Models()` fallback without credentials; registered providers can fetch live models with `GET /v1/models`.",
			HealthCheck:      "Network check: calls `GET /v1/models` through `FetchModels`.",
		},
		providerClaudeCode: { // #nosec G101 -- documentation names credential sources, not secret values.
			ExecutionPath:    "Direct HTTPS calls from atteler to the Anthropic Messages API using Claude Code OAuth; it does not run the Claude Code CLI in print mode.",
			CredentialSource: "Claude Code OAuth from macOS Keychain `Claude Code-credentials` or `~/.claude/.credentials.json`, borrowed by default (`credential_policy.allowed_stores` and `allow_borrowed_oauth` are on by default); restrict with for example `allowed_stores: [env]` or `disable_private_adapter`.",
			TokenRefresh:     fmt.Sprintf("On 401, exchanges the stored refresh token at `%s` and persists refreshed tokens back to the same Claude Code credential store. Refresh and write-back are on by default and gated by `credential_policy.allow_refresh`/`allow_write_back`; file write-back is atomic/CAS-guarded and refresh/write-back attempts and failures are audited.", claudeCodeRefreshURL),
			NetworkEndpoint:  fmt.Sprintf("`ANTHROPIC_BASE_URL`, default `%s`; `POST /v1/messages` for completions. Model listing is static for this provider.", defaultAnthropicBase),
			SandboxAndTools:  "No Claude Code subprocess or Claude Code workspace sandbox. Atteler can send configured bash/read/write/edit/glob/grep tool definitions; tool execution happens in Atteler's agent loop.",
			ModelInventory:   "Known-model listing and `FetchModels` both return the static Claude Code model/alias catalog; no model-list network call is made.",
			HealthCheck:      "Local credential check only: verifies an OAuth access token is loaded; no network call.",
		},
		providerCodex: { // #nosec G101 -- documentation names credential sources, not secret values.
			ExecutionPath:    "Direct HTTPS Responses request from atteler to the ChatGPT Codex backend; it does not run `codex exec`.",
			CredentialSource: "`$CODEX_HOME/auth.json` or `~/.codex/auth.json` in `auth_mode=chatgpt` with ChatGPT access and refresh tokens. Borrowed by default (`credential_policy.allowed_stores` includes `codex_auth_json` and `allow_borrowed_oauth` are on by default); restrict with for example `allowed_stores: [env]` or `disable_private_adapter`.",
			TokenRefresh:     fmt.Sprintf("On 401, exchanges the stored refresh token at `%s` and atomically updates `auth.json`. Refresh and write-back are on by default and gated by `credential_policy.allow_refresh`/`allow_write_back`; write-back is CAS-guarded and refresh/write-back attempts and failures are audited.", codexChatGPTRefreshURL),
			NetworkEndpoint:  fmt.Sprintf("`CODEX_BASE_URL`, default `%s`; `POST /responses` for completions. Model listing is static plus any model from Codex config.", codexChatGPTAPIBase),
			SandboxAndTools:  "No Codex subprocess or Codex CLI workspace sandbox. Atteler can send configured bash/read/write/edit/glob/grep function-tool definitions; tool execution happens in Atteler's agent loop.",
			ModelInventory:   "Known-model listing prints the static Codex catalog; registered providers prepend any model configured in Codex config and `FetchModels` stays local.",
			HealthCheck:      "Local credential check only: verifies parsed ChatGPT-mode auth has an access token; no network call.",
		},
		providerOllama: { // #nosec G101 -- documentation names credential sources, not secret values.
			ExecutionPath:    "HTTP calls to a local or configured Ollama daemon; when auto-start is enabled for a local base URL, atteler may start `ollama serve`.",
			CredentialSource: "No API credential is used by the built-in adapter.",
			TokenRefresh:     "None.",
			NetworkEndpoint:  fmt.Sprintf("`OLLAMA_BASE_URL` or provider config, default `%s`; `POST /api/chat` for completions and `GET /api/tags` for model/health checks.", defaultOllamaBase),
			SandboxAndTools:  "No Ollama workspace sandbox. Atteler serializes configured bash/read/write/edit/glob/grep tool definitions and executes returned tool calls in Atteler's agent loop.",
			ModelInventory:   "Known-model listing prints useful static defaults without contacting Ollama; registered providers call `GET /api/tags` for live local model names.",
			HealthCheck:      "Network/local daemon check: calls `GET /api/tags` and may first auto-start `ollama serve` during provider creation.",
		},
		providerOpenAI: { // #nosec G101 -- documentation names credential sources, not secret values.
			ExecutionPath:    "Direct HTTPS calls from atteler to the OpenAI Chat Completions API.",
			CredentialSource: "`OPENAI_API_KEY` by default, then the `OPENAI_API_KEY` field in `~/.codex/auth.json` (borrowed by default because `credential_policy.allowed_stores` includes `codex_auth_json`; restrict with for example `allowed_stores: [env]`).",
			TokenRefresh:     "None; the API key is sent as a bearer token and is not refreshed.",
			NetworkEndpoint:  fmt.Sprintf("`OPENAI_BASE_URL` or provider config, default `%s`; `POST /v1/chat/completions` for completions and `GET /v1/models` for model/health checks.", defaultOpenAIBase),
			SandboxAndTools:  "No subprocess or workspace sandbox. Atteler sends configured bash/read/write/edit/glob/grep function-tool definitions in the chat request; tool execution happens in Atteler's agent loop.",
			ModelInventory:   "Known-model listing prints the static `Models()` fallback without credentials; registered providers can fetch live models with `GET /v1/models`.",
			HealthCheck:      "Network check: calls `GET /v1/models` through `FetchModels`.",
		},
	}
}

// providerRuntimeDocsMarkdown renders the README provider-runtime section from
// metadata keyed to the same provider inventory used by KnownProviders.
func providerRuntimeDocsMarkdown() string {
	providers := KnownProviders()
	sort.Slice(providers, func(i, j int) bool {
		return providers[i].Name < providers[j].Name
	})

	var b strings.Builder

	for i, provider := range providers {
		if i > 0 {
			b.WriteString("\n")
		}

		runtime, _ := ProviderRuntime(provider.Name)

		fmt.Fprintf(&b, "#### `%s`\n\n", provider.Name)
		fmt.Fprintf(&b, "- Execution path: %s\n", runtime.ExecutionPath)
		fmt.Fprintf(&b, "- Credential source: %s\n", runtime.CredentialSource)
		fmt.Fprintf(&b, "- Token refresh: %s\n", runtime.TokenRefresh)
		fmt.Fprintf(&b, "- Network endpoint: %s\n", runtime.NetworkEndpoint)
		fmt.Fprintf(&b, "- Sandbox and tools: %s\n", runtime.SandboxAndTools)
		fmt.Fprintf(&b, "- Model inventory: %s\n", runtime.ModelInventory)
		fmt.Fprintf(&b, "- Health check: %s\n", runtime.HealthCheck)
	}

	return b.String()
}
