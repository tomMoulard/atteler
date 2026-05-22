package llm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProviderRuntimeInfo_CoversKnownProviders(t *testing.T) {
	t.Parallel()

	knownNames := make([]string, 0)
	for _, provider := range KnownProviders() {
		knownNames = append(knownNames, provider.Name)

		runtime, ok := ProviderRuntime(provider.Name)
		require.Truef(t, ok, "provider runtime missing for %s", provider.Name)
		assert.NotEmpty(t, runtime.ExecutionPath)
		assert.NotEmpty(t, runtime.CredentialSource)
		assert.NotEmpty(t, runtime.TokenRefresh)
		assert.NotEmpty(t, runtime.NetworkEndpoint)
		assert.NotEmpty(t, runtime.SandboxAndTools)
		assert.NotEmpty(t, runtime.ModelInventory)
		assert.NotEmpty(t, runtime.HealthCheck)
	}

	runtimeNames := make([]string, 0)
	for name := range providerRuntimeCatalog() {
		runtimeNames = append(runtimeNames, name)
	}

	assert.ElementsMatch(t, knownNames, runtimeNames, "provider runtime metadata should not contain stale or missing providers")
}

func TestProviderRuntimeDocs_ReadmeSectionMatchesMetadata(t *testing.T) { //nolint:paralleltest // update mode rewrites README and must not run in parallel.
	if os.Getenv("UPDATE_PROVIDER_RUNTIME_DOCS") == "" {
		t.Parallel()
	}

	readmePath := filepath.Join("..", "..", "README.md")

	readme, err := os.ReadFile(readmePath)
	require.NoError(t, err)

	got := readmeGeneratedProviderRuntimeSection(t, string(readme))

	want := providerRuntimeDocsMarkdown()
	if got == want {
		return
	}

	if os.Getenv("UPDATE_PROVIDER_RUNTIME_DOCS") == "1" {
		updated := replaceReadmeGeneratedProviderRuntimeSection(t, string(readme), want)
		// #nosec G703 -- opt-in test update writes the fixed repository README path.
		require.NoError(t, os.WriteFile(readmePath, []byte(updated), readmeFileMode(t, readmePath)))
		t.Log("updated README provider runtime docs")

		return
	}

	assert.Equal(t, want, got)
}

func TestProviderRuntimeDocs_RenderEveryRequiredField(t *testing.T) {
	t.Parallel()

	docs := providerRuntimeDocsMarkdown()
	providers := KnownProviders()

	for _, provider := range providers {
		header := "#### `" + provider.Name + "`"
		assert.Contains(t, docs, header)
	}

	for _, label := range []string{
		"- Execution path:",
		"- Credential source:",
		"- Token refresh:",
		"- Network endpoint:",
		"- Sandbox and tools:",
		"- Model inventory:",
		"- Health check:",
	} {
		assert.Equalf(t, len(providers), strings.Count(docs, label), "provider docs should render %s for every provider", label)
	}
}

func TestProviderRuntimeDocs_ReadmeDoesNotReintroduceSubprocessClaims(t *testing.T) {
	t.Parallel()

	readme, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	require.NoError(t, err)

	text := string(readme)
	for _, staleClaim := range []string{
		"shells out to `codex exec`",
		"shells out to `claude --print`",
		"provider-specific command-line tools",
		"local CLIs or daemons",
		"launched local coding providers",
		"file/search/edit tools and workspace sandboxing",
	} {
		assert.NotContains(t, text, staleClaim)
	}
}

func TestProviderRuntimeInfo_DocumentsActualAdapterBoundaries(t *testing.T) {
	t.Parallel()

	codex, ok := ProviderRuntime(providerCodex)
	require.True(t, ok)
	assert.Contains(t, codex.ExecutionPath, "does not run `codex exec`")
	assert.Contains(t, codex.SandboxAndTools, "No Codex subprocess")
	assert.Contains(t, codex.HealthCheck, "no network call")

	claudeCode, ok := ProviderRuntime(providerClaudeCode)
	require.True(t, ok)
	assert.Contains(t, claudeCode.ExecutionPath, "does not run `claude --print`")
	assert.Contains(t, claudeCode.SandboxAndTools, "No Claude Code subprocess")
	assert.Contains(t, claudeCode.HealthCheck, "no network call")

	openai, ok := ProviderRuntime(providerOpenAI)
	require.True(t, ok)
	assert.Contains(t, openai.ExecutionPath, "Direct HTTPS")
	assert.Contains(t, openai.HealthCheck, "Network check")

	anthropic, ok := ProviderRuntime(providerAnthropic)
	require.True(t, ok)
	assert.Contains(t, anthropic.ExecutionPath, "Direct HTTPS")
	assert.Contains(t, anthropic.HealthCheck, "Network check")

	ollama, ok := ProviderRuntime(providerOllama)
	require.True(t, ok)
	assert.Contains(t, ollama.ExecutionPath, "Ollama daemon")
	assert.Contains(t, ollama.HealthCheck, "Network/local daemon check")
}

func readmeGeneratedProviderRuntimeSection(t *testing.T, readme string) string {
	t.Helper()

	start := strings.Index(readme, providerRuntimeDocsStartMarker)
	require.NotEqual(t, -1, start, "README missing provider runtime start marker")

	sectionStart := start + len(providerRuntimeDocsStartMarker)
	end := strings.Index(readme[sectionStart:], providerRuntimeDocsEndMarker)
	require.NotEqual(t, -1, end, "README missing provider runtime end marker")

	return readme[sectionStart : sectionStart+end]
}

func replaceReadmeGeneratedProviderRuntimeSection(t *testing.T, readme, replacement string) string {
	t.Helper()

	start := strings.Index(readme, providerRuntimeDocsStartMarker)
	require.NotEqual(t, -1, start, "README missing provider runtime start marker")

	sectionStart := start + len(providerRuntimeDocsStartMarker)
	end := strings.Index(readme[sectionStart:], providerRuntimeDocsEndMarker)
	require.NotEqual(t, -1, end, "README missing provider runtime end marker")

	return readme[:sectionStart] + replacement + readme[sectionStart+end:]
}

func readmeFileMode(t *testing.T, path string) os.FileMode {
	t.Helper()

	info, err := os.Stat(path)
	require.NoError(t, err)

	return info.Mode().Perm()
}

const (
	providerRuntimeDocsStartMarker = "<!-- BEGIN GENERATED PROVIDER RUNTIME DOCS -->\n"
	providerRuntimeDocsEndMarker   = "<!-- END GENERATED PROVIDER RUNTIME DOCS -->"
)
