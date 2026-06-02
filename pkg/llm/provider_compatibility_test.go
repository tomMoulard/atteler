package llm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProviderCompatibilityMatrix_CoversKnownProvidersAndDimensions(t *testing.T) {
	t.Parallel()

	knownNames := make([]string, 0)
	for _, provider := range KnownProviders() {
		knownNames = append(knownNames, provider.Name)
	}

	rows := ProviderCompatibilityMatrix()
	rowNames := make([]string, 0, len(rows))

	for i := range rows {
		row := &rows[i]
		rowNames = append(rowNames, row.Provider)

		summary := ProviderCompatibilityStatusSummary(row)
		for _, dimension := range ProviderCompatibilityDimensions() {
			cell := row.compatibilityCell(dimension)
			assert.NotEmpty(t, cell.Status, "%s %s status", row.Provider, dimension)
			assert.NotEmpty(t, cell.Detail, "%s %s detail", row.Provider, dimension)
			assert.Contains(t, summary, string(dimension)+"=", "%s summary", row.Provider)
		}

		assert.NotEmpty(t, row.Models, "%s model compatibility rows", row.Provider)
	}

	assert.ElementsMatch(t, knownNames, rowNames)
}

func TestProviderCompatibilityFor_NormalizesLookupAndReturnsIndependentRows(t *testing.T) {
	t.Parallel()

	row, ok := ProviderCompatibilityFor(" OpenAI ")
	require.True(t, ok)
	assert.Equal(t, providerOpenAI, row.Provider)
	require.NotEmpty(t, row.Models)

	row.Models[0].Model = "mutated-by-caller"

	again, ok := ProviderCompatibilityFor(providerOpenAI)
	require.True(t, ok)
	assert.NotEqual(t, "mutated-by-caller", again.Models[0].Model)

	_, ok = ProviderCompatibilityFor("cohere")
	assert.False(t, ok)
}

func TestProviderCompatibilityMatrix_UsesRequiredDimensions(t *testing.T) {
	t.Parallel()

	assert.Equal(t, []ProviderCompatibilityDimension{
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
	}, ProviderCompatibilityDimensions())
}

func TestProviderCompatibilityMatrix_AlignsWithProviderCapabilities(t *testing.T) {
	t.Parallel()

	rows := ProviderCompatibilityMatrix()
	for i := range rows {
		row := &rows[i]
		t.Run(row.Provider, func(t *testing.T) {
			t.Parallel()

			capabilities, ok := BuiltInProviderCapabilities(row.Provider)
			require.True(t, ok)

			assert.Equal(t, boolStatus(capabilities.SupportsStreaming), row.Streaming.Status)
			assert.Equal(t, string(capabilities.CompleteParams["Tools"].Status), row.ToolUse.Status)
			assert.Equal(t, string(capabilities.CompleteParams["ReasoningLevel"].Status), row.Reasoning.Status)
			assert.Equal(t, string(capabilities.CompleteParams["Seed"].Status), row.Seed.Status)
			assert.Equal(t, string(capabilities.CompleteParams["MaxTokens"].Status), row.MaxTokens.Status)

			temp := capabilities.CompleteParams["Temperature"].Status
			topP := capabilities.CompleteParams["TopP"].Status

			switch {
			case temp == CompleteParamSupported && topP == CompleteParamSupported:
				assert.Equal(t, string(CompleteParamSupported), row.TemperatureTopP.Status)
			case temp == CompleteParamUnsupported && topP == CompleteParamUnsupported:
				assert.Equal(t, string(CompleteParamUnsupported), row.TemperatureTopP.Status)
			default:
				assert.Equal(t, "partial", row.TemperatureTopP.Status)
				assert.Contains(t, row.TemperatureTopP.Detail, "temperature=")
				assert.Contains(t, row.TemperatureTopP.Detail, "top_p=")
			}
		})
	}
}

func TestProviderCompatibilityMatrix_AlignsShellAccessWithRuntimeBoundaries(t *testing.T) {
	t.Parallel()

	want := map[string]struct {
		status          string
		runtimeFragment string
		shellFragment   string
	}{
		providerAnthropic: {
			status:          "none",
			runtimeFragment: "Direct HTTPS",
			shellFragment:   "no subprocess",
		},
		providerClaudeCode: {
			status:          "none",
			runtimeFragment: "does not run the Claude Code CLI",
			shellFragment:   "does not run the Claude Code CLI",
		},
		providerCodex: {
			status:          "none",
			runtimeFragment: "does not run `codex exec`",
			shellFragment:   "does not run `codex exec`",
		},
		providerOllama: {
			status:          "daemon-autostart",
			runtimeFragment: "may start `ollama serve`",
			shellFragment:   "may start `ollama serve`",
		},
		providerOpenAI: {
			status:          "none",
			runtimeFragment: "Direct HTTPS",
			shellFragment:   "no subprocess",
		},
	}

	for _, row := range ProviderCompatibilityMatrix() {
		t.Run(row.Provider, func(t *testing.T) {
			t.Parallel()

			expected, ok := want[row.Provider]
			require.True(t, ok)

			runtime, ok := ProviderRuntime(row.Provider)
			require.True(t, ok)

			assert.Equal(t, expected.status, row.ShellAccess.Status)
			assert.Contains(t, runtime.ExecutionPath, expected.runtimeFragment)
			assert.Contains(t, row.ShellAccess.Detail, expected.shellFragment)
		})
	}
}

func TestProviderCompatibilityMatrix_StatusContract(t *testing.T) {
	t.Parallel()

	want := map[string]map[ProviderCompatibilityDimension]string{
		providerAnthropic: {
			CompatibilityAuthSource:      "api-key/oauth",
			CompatibilityModelDiscovery:  "live+static",
			CompatibilityCompletion:      "messages-api",
			CompatibilityStreaming:       "unsupported",
			CompatibilityToolUse:         "supported",
			CompatibilityShellAccess:     "none",
			CompatibilityReasoning:       "lossy",
			CompatibilitySeed:            "unsupported",
			CompatibilityTemperatureTopP: "supported",
			CompatibilityMaxTokens:       "supported",
			CompatibilityContextWindow:   "catalog+heuristic",
			CompatibilityTokenUsage:      "usage+cache-read-write",
			CompatibilityRetryBehavior:   "registry",
			CompatibilityOfflineMode:     "metadata-only",
		},
		providerClaudeCode: {
			CompatibilityAuthSource:      "borrowed-oauth",
			CompatibilityModelDiscovery:  "static",
			CompatibilityCompletion:      "messages-api",
			CompatibilityStreaming:       "unsupported",
			CompatibilityToolUse:         "supported",
			CompatibilityShellAccess:     "none",
			CompatibilityReasoning:       "lossy",
			CompatibilitySeed:            "unsupported",
			CompatibilityTemperatureTopP: "supported",
			CompatibilityMaxTokens:       "supported",
			CompatibilityContextWindow:   "static",
			CompatibilityTokenUsage:      "usage+cache-read-write",
			CompatibilityRetryBehavior:   "registry+oauth-refresh",
			CompatibilityOfflineMode:     "local-auth+metadata",
		},
		providerCodex: {
			CompatibilityAuthSource:      "borrowed-chatgpt",
			CompatibilityModelDiscovery:  "static+config",
			CompatibilityCompletion:      "responses-api",
			CompatibilityStreaming:       "supported",
			CompatibilityToolUse:         "supported",
			CompatibilityShellAccess:     "none",
			CompatibilityReasoning:       "supported",
			CompatibilitySeed:            "unsupported",
			CompatibilityTemperatureTopP: "partial",
			CompatibilityMaxTokens:       "omitted",
			CompatibilityContextWindow:   "static+unknown-overrides",
			CompatibilityTokenUsage:      "usage+cache-read",
			CompatibilityRetryBehavior:   "registry+oauth-refresh",
			CompatibilityOfflineMode:     "local-auth+metadata",
		},
		providerOllama: { //nolint:gosec // Status literals document seed capability support, not credentials.
			CompatibilityAuthSource:      "none",
			CompatibilityModelDiscovery:  "local-live+static",
			CompatibilityCompletion:      "ollama-chat",
			CompatibilityStreaming:       "supported",
			CompatibilityToolUse:         "supported",
			CompatibilityShellAccess:     "daemon-autostart",
			CompatibilityReasoning:       "lossy",
			CompatibilitySeed:            "supported",
			CompatibilityTemperatureTopP: "supported",
			CompatibilityMaxTokens:       "supported",
			CompatibilityContextWindow:   "static+unknown",
			CompatibilityTokenUsage:      "usage-no-cache",
			CompatibilityRetryBehavior:   "registry",
			CompatibilityOfflineMode:     "local-daemon",
		},
		providerOpenAI: {
			CompatibilityAuthSource:      "api-key",
			CompatibilityModelDiscovery:  "live+static",
			CompatibilityCompletion:      "chat-completions",
			CompatibilityStreaming:       "unsupported",
			CompatibilityToolUse:         "supported",
			CompatibilityShellAccess:     "none",
			CompatibilityReasoning:       "supported",
			CompatibilitySeed:            "supported",
			CompatibilityTemperatureTopP: "supported",
			CompatibilityMaxTokens:       "supported",
			CompatibilityContextWindow:   "catalog+heuristic",
			CompatibilityTokenUsage:      "usage+cache-read",
			CompatibilityRetryBehavior:   "registry",
			CompatibilityOfflineMode:     "metadata-only",
		},
	}

	got := make(map[string]map[ProviderCompatibilityDimension]string)
	for _, row := range ProviderCompatibilityMatrix() {
		got[row.Provider] = make(map[ProviderCompatibilityDimension]string)
		for _, dimension := range ProviderCompatibilityDimensions() {
			got[row.Provider][dimension] = row.compatibilityCell(dimension).Status
		}
	}

	assert.Equal(t, want, got)
}

func TestProviderCompatibilityStatusSummary_RendersStableDoctorLine(t *testing.T) {
	t.Parallel()

	for _, row := range ProviderCompatibilityMatrix() {
		t.Run(row.Provider, func(t *testing.T) {
			t.Parallel()

			parts := strings.Fields(ProviderCompatibilityStatusSummary(&row))
			require.Len(t, parts, len(ProviderCompatibilityDimensions()))

			for i, dimension := range ProviderCompatibilityDimensions() {
				prefix := string(dimension) + "="
				require.Truef(t, strings.HasPrefix(parts[i], prefix), "summary[%d] = %q, want prefix %q", i, parts[i], prefix)
				assert.NotEmpty(t, strings.TrimPrefix(parts[i], prefix), dimension)
			}
		})
	}
}

func TestProviderModelCompatibilityMatrix_CoversKnownModels(t *testing.T) {
	t.Parallel()

	want := make(map[string][]string)
	for _, provider := range KnownProviders() {
		want[provider.Name] = append([]string(nil), provider.Models...)
	}

	got := make(map[string][]string)
	for _, row := range ProviderModelCompatibilityMatrix() {
		got[row.Provider] = append(got[row.Provider], row.Model)
		assert.Positive(t, row.ContextWindow, "%s/%s context window", row.Provider, row.Model)
		assert.NotEmpty(t, row.ContextWindowSource, "%s/%s context source", row.Provider, row.Model)
		assert.NotEmpty(t, row.Provenance, "%s/%s provenance", row.Provider, row.Model)
	}

	assert.Len(t, got, len(want))

	for provider, models := range want {
		assert.ElementsMatch(t, models, got[provider], provider)
	}
}

func TestProviderCompatibilityDocs_ReadmeSectionMatchesMetadata(t *testing.T) { //nolint:paralleltest // update mode rewrites README and must not run in parallel.
	if os.Getenv("UPDATE_PROVIDER_COMPATIBILITY_DOCS") == "" {
		t.Parallel()
	}

	readmePath := filepath.Join("..", "..", "README.md")

	readme, err := os.ReadFile(readmePath)
	require.NoError(t, err)

	got := readmeGeneratedProviderCompatibilitySection(t, string(readme))

	want := providerCompatibilityDocsMarkdown()
	if got == want {
		return
	}

	if os.Getenv("UPDATE_PROVIDER_COMPATIBILITY_DOCS") == "1" {
		updated := replaceReadmeGeneratedProviderCompatibilitySection(t, string(readme), want)
		// #nosec G703 -- opt-in test update writes the fixed repository README path.
		require.NoError(t, os.WriteFile(readmePath, []byte(updated), readmeFileMode(t, readmePath)))
		t.Log("updated README provider compatibility docs")

		return
	}

	assert.Equal(t, want, got)
}

func TestProviderCompatibilityDocs_RenderEveryRequiredDimension(t *testing.T) {
	t.Parallel()

	docs := providerCompatibilityDocsMarkdown()
	rows := ProviderCompatibilityMatrix()

	for i := range rows {
		row := &rows[i]
		assert.Contains(t, docs, "`"+row.Provider+"`")
	}

	for _, dimension := range ProviderCompatibilityDimensions() {
		assert.Equalf(t, 1, strings.Count(docs, "| `"+string(dimension)+"` |"), "dimension %s should render once", dimension)
	}
}

func TestProviderCompatibilityDocs_ReadmeDoesNotAdvertiseUnknownProviders(t *testing.T) {
	t.Parallel()

	readme, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	require.NoError(t, err)

	text := string(readme)
	for _, staleClaim := range []string{
		"Cohere",
		"OpenAI, Anthropic, Cohere",
		"OpenAI, Anthropic, Cohere, etc.",
		"OpenAI, Anthropic, Cohere, etc",
	} {
		assert.NotContains(t, text, staleClaim)
	}
}

func boolStatus(ok bool) string {
	if ok {
		return string(CompleteParamSupported)
	}

	return string(CompleteParamUnsupported)
}

func readmeGeneratedProviderCompatibilitySection(t *testing.T, readme string) string {
	t.Helper()

	start := strings.Index(readme, providerCompatibilityDocsStartMarker)
	require.NotEqual(t, -1, start, "README missing provider compatibility start marker")

	sectionStart := start + len(providerCompatibilityDocsStartMarker)
	end := strings.Index(readme[sectionStart:], providerCompatibilityDocsEndMarker)
	require.NotEqual(t, -1, end, "README missing provider compatibility end marker")

	return readme[sectionStart : sectionStart+end]
}

func replaceReadmeGeneratedProviderCompatibilitySection(t *testing.T, readme, replacement string) string {
	t.Helper()

	start := strings.Index(readme, providerCompatibilityDocsStartMarker)
	require.NotEqual(t, -1, start, "README missing provider compatibility start marker")

	sectionStart := start + len(providerCompatibilityDocsStartMarker)
	end := strings.Index(readme[sectionStart:], providerCompatibilityDocsEndMarker)
	require.NotEqual(t, -1, end, "README missing provider compatibility end marker")

	return readme[:sectionStart] + replacement + readme[sectionStart+end:]
}

const (
	providerCompatibilityDocsStartMarker = "<!-- BEGIN GENERATED PROVIDER COMPATIBILITY MATRIX -->\n"
	providerCompatibilityDocsEndMarker   = "<!-- END GENERATED PROVIDER COMPATIBILITY MATRIX -->"
)
