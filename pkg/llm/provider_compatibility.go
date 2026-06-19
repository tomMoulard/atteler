package llm

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/tommoulard/atteler/pkg/modelroute"
)

// ProviderCompatibilityDimension names one axis of the built-in provider
// compatibility matrix. Keep these stable: doctor output, README generation,
// and tests use the same dimension order.
type ProviderCompatibilityDimension string

// Provider compatibility dimensions.
const (
	CompatibilityAuthSource      ProviderCompatibilityDimension = "auth_source"
	CompatibilityModelDiscovery  ProviderCompatibilityDimension = "model_discovery"
	CompatibilityCompletion      ProviderCompatibilityDimension = "completion"
	CompatibilityStreaming       ProviderCompatibilityDimension = "streaming"
	CompatibilityToolUse         ProviderCompatibilityDimension = "tool_use"
	CompatibilityShellAccess     ProviderCompatibilityDimension = "shell_access"
	CompatibilityReasoning       ProviderCompatibilityDimension = "reasoning_effort"
	CompatibilitySeed            ProviderCompatibilityDimension = "seed"
	CompatibilityTemperatureTopP ProviderCompatibilityDimension = "temperature_top_p"
	CompatibilityMaxTokens       ProviderCompatibilityDimension = "max_tokens"
	CompatibilityContextWindow   ProviderCompatibilityDimension = "context_window"
	CompatibilityTokenUsage      ProviderCompatibilityDimension = "token_usage"
	CompatibilityRetryBehavior   ProviderCompatibilityDimension = "retry_behavior"
	CompatibilityOfflineMode     ProviderCompatibilityDimension = "offline_mode"
)

// ProviderCompatibilityCell is one executable matrix cell. Status is deliberately
// short for doctor output; Detail is the human-facing contract used in docs.
type ProviderCompatibilityCell struct {
	Status string `json:"status"`
	Detail string `json:"detail"`
}

// ProviderModelCompatibility captures model-level facts that vary within a
// provider, primarily context windows and known max-output limits.
type ProviderModelCompatibility struct {
	Provider            string `json:"provider"`
	Model               string `json:"model"`
	ContextWindowSource string `json:"context_window_source"`
	Provenance          string `json:"provenance"`
	ContextWindow       int    `json:"context_window"`
	MaxOutputTokens     int    `json:"max_output_tokens,omitempty"`
}

// ProviderCompatibilityRow captures Atteler's compatibility contract for one
// built-in provider. The matrix is static and credential-free; live doctor
// checks layer current readiness on top of this contract.
type ProviderCompatibilityRow struct {
	Provider        string                       `json:"provider"`
	AuthSource      ProviderCompatibilityCell    `json:"auth_source"`
	ModelDiscovery  ProviderCompatibilityCell    `json:"model_discovery"`
	Completion      ProviderCompatibilityCell    `json:"completion"`
	Streaming       ProviderCompatibilityCell    `json:"streaming"`
	ToolUse         ProviderCompatibilityCell    `json:"tool_use"`
	ShellAccess     ProviderCompatibilityCell    `json:"shell_access"`
	Reasoning       ProviderCompatibilityCell    `json:"reasoning_effort"`
	Seed            ProviderCompatibilityCell    `json:"seed"`
	TemperatureTopP ProviderCompatibilityCell    `json:"temperature_top_p"`
	MaxTokens       ProviderCompatibilityCell    `json:"max_tokens"`
	ContextWindow   ProviderCompatibilityCell    `json:"context_window"`
	TokenUsage      ProviderCompatibilityCell    `json:"token_usage"`
	RetryBehavior   ProviderCompatibilityCell    `json:"retry_behavior"`
	OfflineMode     ProviderCompatibilityCell    `json:"offline_mode"`
	Models          []ProviderModelCompatibility `json:"models,omitempty"`
}

// ProviderCompatibilityDimensions returns the canonical matrix dimension order.
func ProviderCompatibilityDimensions() []ProviderCompatibilityDimension {
	return []ProviderCompatibilityDimension{
		CompatibilityAuthSource,
		CompatibilityModelDiscovery,
		CompatibilityCompletion,
		CompatibilityStreaming,
		CompatibilityToolUse,
		CompatibilityShellAccess,
		CompatibilityReasoning,
		CompatibilitySeed,
		CompatibilityTemperatureTopP,
		CompatibilityMaxTokens,
		CompatibilityContextWindow,
		CompatibilityTokenUsage,
		CompatibilityRetryBehavior,
		CompatibilityOfflineMode,
	}
}

// ProviderCompatibilityMatrix returns the static built-in provider contract
// keyed to the same provider inventory as KnownProviders. It performs no
// credential, network, or local daemon work.
func ProviderCompatibilityMatrix() []ProviderCompatibilityRow {
	providers := KnownProviders()
	sort.Slice(providers, func(i, j int) bool {
		return providers[i].Name < providers[j].Name
	})

	out := make([]ProviderCompatibilityRow, 0, len(providers))
	for _, provider := range providers {
		row, ok := ProviderCompatibilityFor(provider.Name)
		if !ok {
			continue
		}

		out = append(out, row)
	}

	return out
}

// ProviderCompatibilityFor returns the static compatibility row for a built-in
// provider.
func ProviderCompatibilityFor(providerName string) (ProviderCompatibilityRow, bool) {
	providerName = strings.ToLower(strings.TrimSpace(providerName))

	capabilities, ok := BuiltInProviderCapabilities(providerName)
	if !ok {
		return ProviderCompatibilityRow{}, false
	}

	row := ProviderCompatibilityRow{
		Provider:        providerName,
		Streaming:       boolCompatibilityCell(capabilities.SupportsStreaming, "llm.StreamProvider implementation", "no caller-facing streaming provider"),
		ToolUse:         completeParamCompatibilityCell(capabilities.CompleteParams["Tools"]),
		Reasoning:       completeParamCompatibilityCell(capabilities.CompleteParams["ReasoningLevel"]),
		Seed:            completeParamCompatibilityCell(capabilities.CompleteParams["Seed"]),
		TemperatureTopP: temperatureTopPCompatibilityCell(capabilities),
		MaxTokens:       completeParamCompatibilityCell(capabilities.CompleteParams["MaxTokens"]),
		Models:          providerModelCompatibility(providerName),
	}

	switch providerName {
	case providerAnthropic:
		row.AuthSource = compatibilityCell("api-key/oauth", "`ANTHROPIC_API_KEY`/`ANTHROPIC_AUTH_TOKEN`, ForgeCode credentials, or borrowed Claude Code credentials")
		row.ModelDiscovery = compatibilityCell("live+static", "`GET /v1/models` when registered; static fallback for offline known-models")
		row.Completion = compatibilityCell("messages-api", "`POST /v1/messages` direct HTTPS")
		row.ShellAccess = compatibilityCell("atteler-loop", "no provider subprocess or CLI; configured tool calls execute in Atteler's agent loop")
		row.ContextWindow = compatibilityCell("catalog+heuristic", "versioned catalog metadata, with Anthropic-name fallback for newer Claude IDs")
		row.TokenUsage = compatibilityCell("usage+cache-read-write", "input, output, cache-read, and cache-write token counts")
		row.RetryBehavior = compatibilityCell("registry", "registry retries transient 429/5xx responses; adapter does not refresh on 401")
		row.OfflineMode = compatibilityCell("metadata-only", "known providers/models/matrix work offline; completion and health require network credentials")
	case providerClaudeCode:
		row.AuthSource = compatibilityCell("borrowed-oauth", "Claude Code OAuth from macOS Keychain or `~/.claude/.credentials.json`")
		row.ModelDiscovery = compatibilityCell("static", "static Claude Code adapter catalog; no model-list network call")
		row.Completion = compatibilityCell("messages-api", "`POST /v1/messages` direct HTTPS using Claude Code OAuth")
		row.ShellAccess = compatibilityCell("atteler-loop", "does not run the Claude Code CLI; configured tool calls execute in Atteler's agent loop")
		row.ContextWindow = compatibilityCell("catalog+static-aliases", "built-in catalog metadata for known Claude IDs; static context-window assumptions for Claude Code aliases")
		row.TokenUsage = compatibilityCell("usage+cache-read-write", "input, output, cache-read, and cache-write token counts from Anthropic responses")
		row.RetryBehavior = compatibilityCell("registry+oauth-refresh", "registry retries transient 429/5xx responses; adapter refreshes OAuth once after 401")
		row.OfflineMode = compatibilityCell("local-auth+metadata", "static catalog and local credential checks work offline; completion requires network")
	case providerCodex:
		row.AuthSource = compatibilityCell("borrowed-chatgpt", "`$CODEX_HOME/auth.json` or `~/.codex/auth.json` in ChatGPT auth mode")
		row.ModelDiscovery = compatibilityCell("static+config", "static Codex catalog plus configured Codex model; no backend model-list endpoint")
		row.Completion = compatibilityCell("responses-api", "`POST /responses` direct HTTPS to the ChatGPT Codex backend")
		row.ShellAccess = compatibilityCell("atteler-loop", "does not run `codex exec`; configured tool calls execute in Atteler's agent loop")
		row.ContextWindow = compatibilityCell("catalog+static+unknown-overrides", "built-in catalog metadata for known IDs; static adapter metadata for Codex-only IDs; configured overrides intentionally return unknown context")
		row.TokenUsage = compatibilityCell("usage+cache-read", "input, output, and cached-input token counts from Responses events")
		row.RetryBehavior = compatibilityCell("registry+oauth-refresh", "registry retries transient 429/5xx responses; adapter refreshes ChatGPT OAuth once after 401")
		row.OfflineMode = compatibilityCell("local-auth+metadata", "static catalog and local credential checks work offline; completion requires network")
	case providerOllama:
		row.AuthSource = compatibilityCell("none", "no API credential is used by the built-in adapter")
		row.ModelDiscovery = compatibilityCell("local-live+static", "`GET /api/tags` against the configured daemon; static fallback for offline known-models")
		row.Completion = compatibilityCell("ollama-chat", "`POST /api/chat` against a local or configured Ollama daemon")
		row.ShellAccess = compatibilityCell("atteler-loop+daemon", "configured tool calls execute in Atteler's agent loop; may start `ollama serve` only when auto-start is explicitly enabled")
		row.ContextWindow = compatibilityCell("static+unknown", "static defaults for common local model families; unknown for unrecognized local tags")
		row.TokenUsage = compatibilityCell("usage-no-cache", "prompt/eval token counts; no cached-token accounting")
		row.RetryBehavior = compatibilityCell("registry", "registry retries transient 429/5xx responses from the daemon")
		row.OfflineMode = compatibilityCell("local-daemon", "matrix and static known-models work offline; local completion needs a reachable daemon/model")
	case providerOpenAI:
		row.AuthSource = compatibilityCell("api-key", "`OPENAI_API_KEY` or the `OPENAI_API_KEY` field in Codex `auth.json`")
		row.ModelDiscovery = compatibilityCell("live+static", "`GET /v1/models` when registered; static fallback for offline known-models")
		row.Completion = compatibilityCell("chat-completions", "`POST /v1/chat/completions` direct HTTPS")
		row.ShellAccess = compatibilityCell("atteler-loop", "no provider subprocess or CLI; configured tool calls execute in Atteler's agent loop")
		row.ContextWindow = compatibilityCell("catalog+heuristic", "versioned catalog metadata, with heuristic fallback for legacy OpenAI IDs")
		row.TokenUsage = compatibilityCell("usage+cache-read", "prompt, completion, and cached-input token counts")
		row.RetryBehavior = compatibilityCell("registry", "registry retries transient 429/5xx responses; API keys are not refreshed")
		row.OfflineMode = compatibilityCell("metadata-only", "known providers/models/matrix work offline; completion and health require network credentials")
	default:
		return ProviderCompatibilityRow{}, false
	}

	return row, true
}

// ProviderModelCompatibilityMatrix returns flattened provider/model metadata
// from the same source as ProviderCompatibilityMatrix.
func ProviderModelCompatibilityMatrix() []ProviderModelCompatibility {
	rows := ProviderCompatibilityMatrix()
	out := make([]ProviderModelCompatibility, 0)

	for i := range rows {
		out = append(out, rows[i].Models...)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider == out[j].Provider {
			return out[i].Model < out[j].Model
		}

		return out[i].Provider < out[j].Provider
	})

	return out
}

// ProviderCompatibilityStatusSummary renders the compact status-only line used
// by doctor and offline doctor. Keep it free of spaces inside values so logs are
// easy to scan and grep.
func ProviderCompatibilityStatusSummary(row *ProviderCompatibilityRow) string {
	if row == nil {
		return ""
	}

	parts := make([]string, 0, len(ProviderCompatibilityDimensions()))
	for _, dimension := range ProviderCompatibilityDimensions() {
		cell := row.compatibilityCell(dimension)
		parts = append(parts, fmt.Sprintf("%s=%s", dimension, cell.Status))
	}

	return strings.Join(parts, " ")
}

func compatibilityCell(status, detail string) ProviderCompatibilityCell {
	return ProviderCompatibilityCell{Status: status, Detail: detail}
}

func boolCompatibilityCell(ok bool, supportedDetail, unsupportedDetail string) ProviderCompatibilityCell {
	if ok {
		return compatibilityCell(string(CompleteParamSupported), supportedDetail)
	}

	return compatibilityCell(string(CompleteParamUnsupported), unsupportedDetail)
}

func completeParamCompatibilityCell(support CompleteParamSupport) ProviderCompatibilityCell {
	return compatibilityCell(string(support.Status), support.Note)
}

func temperatureTopPCompatibilityCell(capabilities ProviderCapabilities) ProviderCompatibilityCell {
	temperature := capabilities.CompleteParams["Temperature"]
	topP := capabilities.CompleteParams["TopP"]

	if temperature.Status == CompleteParamSupported && topP.Status == CompleteParamSupported {
		return compatibilityCell(
			string(CompleteParamSupported),
			"temperature and top_p are sent directly unless provider-specific constraints apply",
		)
	}

	if temperature.Status == CompleteParamUnsupported && topP.Status == CompleteParamUnsupported {
		return compatibilityCell(
			string(CompleteParamUnsupported),
			fmt.Sprintf("temperature=%s (%s); top_p=%s (%s)",
				temperature.Status,
				temperature.Note,
				topP.Status,
				topP.Note,
			),
		)
	}

	return compatibilityCell(
		"partial",
		fmt.Sprintf("temperature=%s (%s); top_p=%s (%s)",
			temperature.Status,
			temperature.Note,
			topP.Status,
			topP.Note,
		),
	)
}

func providerModelCompatibility(providerName string) []ProviderModelCompatibility {
	provider := compatibilityProvider(providerName)
	if provider == nil {
		return nil
	}

	models := mergeModelLists(provider.Models(), catalogModelsByProvider()[providerName])
	sort.Strings(models)

	out := make([]ProviderModelCompatibility, 0, len(models))
	for _, model := range models {
		out = append(out, providerModelCompatibilityEntry(providerName, provider, model))
	}

	return out
}

func providerModelCompatibilityEntry(
	providerName string,
	provider Provider,
	model string,
) ProviderModelCompatibility {
	entry := ProviderModelCompatibility{
		Provider: providerName,
		Model:    model,
	}

	if metadata, ok := modelroute.BuiltinCatalog().Lookup(providerName, model); ok {
		entry.ContextWindow = metadata.ContextWindow
		entry.MaxOutputTokens = metadata.MaxOutputTokens
		entry.ContextWindowSource = "builtin_catalog"
		entry.Provenance = metadata.Source

		return entry
	}

	if metadataProvider, ok := provider.(ModelMetadataProvider); ok {
		metadata, found := metadataProvider.ModelMetadata(model)
		if found {
			entry.ContextWindow = metadata.ContextWindow
			entry.ContextWindowSource = "adapter_static"
			entry.Provenance = metadata.Provenance

			return entry
		}
	}

	entry.ContextWindow = provider.ModelContextWindow(model)
	if entry.ContextWindow > 0 {
		entry.ContextWindowSource = "provider_heuristic"
		entry.Provenance = "provider static context-window heuristic"
	} else {
		entry.ContextWindowSource = "unknown"
		entry.Provenance = "no maintained context-window metadata"
	}

	return entry
}

func compatibilityProvider(providerName string) Provider {
	switch providerName {
	case providerAnthropic:
		return &AnthropicProvider{}
	case providerClaudeCode:
		return &ClaudeCodeProvider{}
	case providerCodex:
		return &CodexProvider{}
	case providerOllama:
		return &OllamaProvider{}
	case providerOpenAI:
		return &OpenAIProvider{}
	default:
		return nil
	}
}

func (r *ProviderCompatibilityRow) compatibilityCell(dimension ProviderCompatibilityDimension) ProviderCompatibilityCell {
	if r == nil {
		return ProviderCompatibilityCell{}
	}

	cells := map[ProviderCompatibilityDimension]ProviderCompatibilityCell{
		CompatibilityAuthSource:      r.AuthSource,
		CompatibilityModelDiscovery:  r.ModelDiscovery,
		CompatibilityCompletion:      r.Completion,
		CompatibilityStreaming:       r.Streaming,
		CompatibilityToolUse:         r.ToolUse,
		CompatibilityShellAccess:     r.ShellAccess,
		CompatibilityReasoning:       r.Reasoning,
		CompatibilitySeed:            r.Seed,
		CompatibilityTemperatureTopP: r.TemperatureTopP,
		CompatibilityMaxTokens:       r.MaxTokens,
		CompatibilityContextWindow:   r.ContextWindow,
		CompatibilityTokenUsage:      r.TokenUsage,
		CompatibilityRetryBehavior:   r.RetryBehavior,
		CompatibilityOfflineMode:     r.OfflineMode,
	}

	return cells[dimension]
}

// providerCompatibilityDocsMarkdown renders the generated README matrix from
// the executable metadata above.
func providerCompatibilityDocsMarkdown() string {
	rows := ProviderCompatibilityMatrix()

	var b strings.Builder
	b.WriteString("| Dimension |")

	for i := range rows {
		row := &rows[i]
		fmt.Fprintf(&b, " `%s` |", row.Provider)
	}

	b.WriteString("\n| --- |")

	for range rows {
		b.WriteString(" --- |")
	}

	b.WriteString("\n")

	for _, dimension := range ProviderCompatibilityDimensions() {
		fmt.Fprintf(&b, "| `%s` |", dimension)

		for i := range rows {
			row := &rows[i]
			cell := row.compatibilityCell(dimension)
			fmt.Fprintf(&b, " %s |", markdownCompatibilityCell(cell))
		}

		b.WriteString("\n")
	}

	b.WriteString("\n#### Model context and output limits\n\n")
	b.WriteString("The max-output column is catalog metadata about model limits; ")
	b.WriteString("request-level `CompleteParams.MaxTokens` support is the separate ")
	b.WriteString("`max_tokens` compatibility row above.\n\n")
	b.WriteString("| Provider | Model | Context window | Max output tokens | Provenance |\n")
	b.WriteString("| --- | --- | ---: | ---: | --- |\n")

	modelRows := ProviderModelCompatibilityMatrix()
	for i := range modelRows {
		row := &modelRows[i]
		fmt.Fprintf(
			&b,
			"| `%s` | `%s` | %s | %s | %s |\n",
			row.Provider,
			row.Model,
			formatCompatibilityInt(row.ContextWindow),
			formatCompatibilityInt(row.MaxOutputTokens),
			markdownCompatibilityCell(ProviderCompatibilityCell{
				Status: row.ContextWindowSource,
				Detail: row.Provenance,
			}),
		)
	}

	return b.String()
}

func formatCompatibilityInt(value int) string {
	if value <= 0 {
		return "unknown"
	}

	return strconv.Itoa(value)
}

func markdownCompatibilityCell(cell ProviderCompatibilityCell) string {
	text := cell.Status
	if cell.Detail != "" {
		text += " — " + cell.Detail
	}

	text = strings.ReplaceAll(text, "\n", " ")
	text = strings.ReplaceAll(text, "|", "\\|")

	return text
}
