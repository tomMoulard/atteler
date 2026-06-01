package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestInspectPathSources_ReportsUnknownDeprecatedAndVersionDiagnostics(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
provider: openai
model: gpt-legacy
generation:
  reasoning: high
  surprise: true
agent_loop:
  max_cost_micros: 100000
  max_input_tokens: 1000
  max_output_tokens: 2000
  max_tool_calls: 10
  max_wall_time: 30m
  checkpoint_interval: 2
agents:
  reviewer:
    prompt: review safely
    routing_policy:
      preferred_providers: [openai]
      max_latency_ms: 1200
      max_ttft_ms: 300
      surprise: true
models:
  planner:
    preferred: openai/gpt-4.1
    fallback: anthropic/claude-sonnet
    routing_policy:
      required_capabilities: [tools]
      max_latency_ms: 2000
      max_ttft_ms: 800
providers:
  openai:
    disable_private_adapter: true
    token: should-not-be-here
skill_learning:
  enabled: true
  store_dir: ./.atteler/skill-learning
  skill_dir: ./.atteler/skills/generated
  min_occurrences: 2
  max_steps: 6
  max_observations: 300
`), 0o600))

	reports := InspectPathSources([]PathSource{{Path: path, Kind: OriginExplicitFile}})
	require.Len(t, reports, 1)
	assert.Equal(t, "present", reports[0].Status)
	assert.Equal(t, ConfigSchemaVersion, reports[0].Version)

	assertDiagnostic(t, reports[0].Diagnostics, DiagnosticInfo, "version", "")
	assertDiagnostic(t, reports[0].Diagnostics, DiagnosticWarning, "provider", "default_provider")
	assertDiagnostic(t, reports[0].Diagnostics, DiagnosticWarning, "model", "default_model")
	assertDiagnostic(t, reports[0].Diagnostics, DiagnosticWarning, "generation.reasoning", "generation.reasoning_level")
	assertDiagnostic(t, reports[0].Diagnostics, DiagnosticWarning, "agents.reviewer.prompt", "agents.reviewer.system_prompt")
	assertDiagnostic(t, reports[0].Diagnostics, DiagnosticError, "generation.surprise", "")
	assertDiagnostic(t, reports[0].Diagnostics, DiagnosticError, "agents.reviewer.routing_policy.surprise", "")
	assertDiagnostic(t, reports[0].Diagnostics, DiagnosticError, "providers.openai.token", "")
	assertNoDiagnostic(t, reports[0].Diagnostics, "models")
	assertNoDiagnostic(t, reports[0].Diagnostics, "models.planner.preferred")
	assertNoDiagnostic(t, reports[0].Diagnostics, "models.planner.fallback")
	assertNoDiagnostic(t, reports[0].Diagnostics, "models.planner.routing_policy.required_capabilities")
	assertNoDiagnostic(t, reports[0].Diagnostics, "models.planner.routing_policy.max_latency_ms")
	assertNoDiagnostic(t, reports[0].Diagnostics, "models.planner.routing_policy.max_ttft_ms")
	assertNoDiagnostic(t, reports[0].Diagnostics, "agents.reviewer.routing_policy")
	assertNoDiagnostic(t, reports[0].Diagnostics, "agents.reviewer.routing_policy.preferred_providers")
	assertNoDiagnostic(t, reports[0].Diagnostics, "agents.reviewer.routing_policy.max_latency_ms")
	assertNoDiagnostic(t, reports[0].Diagnostics, "agents.reviewer.routing_policy.max_ttft_ms")
	assertNoDiagnostic(t, reports[0].Diagnostics, "agent_loop.max_tool_calls")
	assertNoDiagnostic(t, reports[0].Diagnostics, "agent_loop.max_wall_time")
	assertNoDiagnostic(t, reports[0].Diagnostics, "agent_loop.checkpoint_interval")
	assertNoDiagnostic(t, reports[0].Diagnostics, "agent_loop.max_cost_micros")
	assertNoDiagnostic(t, reports[0].Diagnostics, "agent_loop.max_input_tokens")
	assertNoDiagnostic(t, reports[0].Diagnostics, "agent_loop.max_output_tokens")
	assertNoDiagnostic(t, reports[0].Diagnostics, "providers.openai.disable_private_adapter")
	assertNoDiagnostic(t, reports[0].Diagnostics, "skill_learning")
	assertNoDiagnostic(t, reports[0].Diagnostics, "skill_learning.enabled")
}

func TestInspectPathSources_AcceptsScopedVectorizerConfig(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
version: 1
vector:
  vectorizer: lexical
  stores:
    agent-memory:
      vectorizer: embedding
      provider: ollama
      model: memory-embed
      base_url: http://127.0.0.1:11434
      fallback_policy: lexical
      index_path: ./.atteler/agent-memory.json
      timeout_seconds: 5
      chunk_max_runes: 600
      chunk_overlap_runes: 60
  agents:
    reviewer:
      model: reviewer-memory-embed
      index_path: ./.atteler/reviewer-memory.json
  sources:
    session:
      vectorizer: embedding
      index_path: ./.atteler/session-vector-index.json
      surprise: true
    git_history:
      vectorizer: lexical
      index_path: ./.atteler/git-history-vector-index.json
`), 0o600))

	reports := InspectPathSources([]PathSource{{Path: path, Kind: OriginExplicitFile}})
	require.Len(t, reports, 1)
	assert.Equal(t, "present", reports[0].Status)

	assertNoDiagnostic(t, reports[0].Diagnostics, "vector.stores")
	assertNoDiagnostic(t, reports[0].Diagnostics, "vector.stores.agent-memory.vectorizer")
	assertNoDiagnostic(t, reports[0].Diagnostics, "vector.stores.agent-memory.provider")
	assertNoDiagnostic(t, reports[0].Diagnostics, "vector.stores.agent-memory.model")
	assertNoDiagnostic(t, reports[0].Diagnostics, "vector.stores.agent-memory.base_url")
	assertNoDiagnostic(t, reports[0].Diagnostics, "vector.stores.agent-memory.fallback_policy")
	assertNoDiagnostic(t, reports[0].Diagnostics, "vector.stores.agent-memory.index_path")
	assertNoDiagnostic(t, reports[0].Diagnostics, "vector.stores.agent-memory.timeout_seconds")
	assertNoDiagnostic(t, reports[0].Diagnostics, "vector.stores.agent-memory.chunk_max_runes")
	assertNoDiagnostic(t, reports[0].Diagnostics, "vector.stores.agent-memory.chunk_overlap_runes")
	assertNoDiagnostic(t, reports[0].Diagnostics, "vector.agents")
	assertNoDiagnostic(t, reports[0].Diagnostics, "vector.agents.reviewer.model")
	assertNoDiagnostic(t, reports[0].Diagnostics, "vector.agents.reviewer.index_path")
	assertNoDiagnostic(t, reports[0].Diagnostics, "vector.sources")
	assertNoDiagnostic(t, reports[0].Diagnostics, "vector.sources.session.vectorizer")
	assertNoDiagnostic(t, reports[0].Diagnostics, "vector.sources.session.index_path")
	assertNoDiagnostic(t, reports[0].Diagnostics, "vector.sources.git_history.vectorizer")
	assertNoDiagnostic(t, reports[0].Diagnostics, "vector.sources.git_history.index_path")
	assertDiagnostic(t, reports[0].Diagnostics, DiagnosticError, "vector.sources.session.surprise", "")
}

func TestInspectPathSources_ReportsNonMappingConfig(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("- not-a-map\n"), 0o600))

	reports := InspectPathSources([]PathSource{{Path: path, Kind: OriginExplicitFile}})
	require.Len(t, reports, 1)
	assert.Equal(t, "error", reports[0].Status)
	assertDiagnosticMessage(t, reports[0].Diagnostics, DiagnosticError, "expected top-level mapping")
}

func TestInspectPathSources_AcceptsGeneratedTemplate(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(TemplateYAML()), 0o600))

	reports := InspectPathSources([]PathSource{{Path: path, Kind: OriginExplicitFile}})
	require.Len(t, reports, 1)
	assert.Equal(t, "present", reports[0].Status)
	assert.Empty(t, reports[0].Diagnostics)
}

func TestInspectPathSources_ReportsInvalidModelAliases(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
model_aliases:
  fast: openai/gpt-4.1-mini
  openai/fast: openai/gpt-4.1-mini
  openai/: openai/gpt-4.1-mini
  missing_provider: gpt-4.1-mini
  nested:
    provider: openai
`), 0o600))

	reports := InspectPathSources([]PathSource{{Path: path, Kind: OriginExplicitFile}})
	require.Len(t, reports, 1)

	assertNoDiagnostic(t, reports[0].Diagnostics, "model_aliases.fast")
	assertDiagnosticMessage(t, reports[0].Diagnostics, DiagnosticError, "model alias must be a bare model name")
	assertDiagnosticMessage(t, reports[0].Diagnostics, DiagnosticError, "model alias target must be provider/model")
	assertDiagnostic(t, reports[0].Diagnostics, DiagnosticError, "model_aliases.openai/fast", "")
	assertDiagnostic(t, reports[0].Diagnostics, DiagnosticError, "model_aliases.openai/", "")
	assertDiagnostic(t, reports[0].Diagnostics, DiagnosticError, "model_aliases.missing_provider", "")
	assertDiagnostic(t, reports[0].Diagnostics, DiagnosticError, "model_aliases.nested", "")
}

func TestInspectPathSources_AcceptsReferencePolicyFields(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
context:
  reference_policy:
    allowed_schemes: [https]
    denied_schemes: [http]
    allowed_hosts: [docs.example.com]
    denied_hosts: [blocked.example.com]
    allowed_ports: [443]
    denied_ports: [81]
    local_roots: [../shared]
    denied_local_roots: [../shared/secrets]
    allowed_globs: ["docs/**/*.md"]
    denied_globs: ["**/*.pem"]
    content_types: [text/*]
    max_redirects: 1
    max_files: 20
    allow_absolute_paths: true
    allow_private_networks: false
`), 0o600))

	reports := InspectPathSources([]PathSource{{Path: path, Kind: OriginExplicitFile}})
	require.Len(t, reports, 1)
	assert.Equal(t, "present", reports[0].Status)
	assertNoDiagnostic(t, reports[0].Diagnostics, "context.reference_policy.allowed_schemes")
	assertNoDiagnostic(t, reports[0].Diagnostics, "context.reference_policy.denied_schemes")
	assertNoDiagnostic(t, reports[0].Diagnostics, "context.reference_policy.allowed_hosts")
	assertNoDiagnostic(t, reports[0].Diagnostics, "context.reference_policy.denied_hosts")
	assertNoDiagnostic(t, reports[0].Diagnostics, "context.reference_policy.allowed_ports")
	assertNoDiagnostic(t, reports[0].Diagnostics, "context.reference_policy.denied_ports")
	assertNoDiagnostic(t, reports[0].Diagnostics, "context.reference_policy.local_roots")
	assertNoDiagnostic(t, reports[0].Diagnostics, "context.reference_policy.denied_local_roots")
	assertNoDiagnostic(t, reports[0].Diagnostics, "context.reference_policy.allowed_globs")
	assertNoDiagnostic(t, reports[0].Diagnostics, "context.reference_policy.denied_globs")
	assertNoDiagnostic(t, reports[0].Diagnostics, "context.reference_policy.content_types")
	assertNoDiagnostic(t, reports[0].Diagnostics, "context.reference_policy.max_redirects")
	assertNoDiagnostic(t, reports[0].Diagnostics, "context.reference_policy.max_files")
	assertNoDiagnostic(t, reports[0].Diagnostics, "context.reference_policy.allow_absolute_paths")
	assertNoDiagnostic(t, reports[0].Diagnostics, "context.reference_policy.allow_private_networks")
}

func TestInspectPathSources_ReportsNegativeVersion(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("version: -1\n"), 0o600))

	reports := InspectPathSources([]PathSource{{Path: path, Kind: OriginExplicitFile}})
	require.Len(t, reports, 1)
	assert.Equal(t, "present", reports[0].Status)
	assert.Equal(t, -1, reports[0].Version)
	assertDiagnostic(t, reports[0].Diagnostics, DiagnosticError, "version", "")
}

func TestNewDiagnosticsReport_RedactsSecrets(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
version: 1
providers:
  openai:
    base_url: https://user:secret@example.com/v1?x-api-key=sk-secret
agents:
  reviewer:
    description: internal reviewer description secret
    personality: internal reviewer personality secret
    system_prompt: proprietary instructions
    references:
      - https://example.com/agent?api_key=agent-secret
context:
  references:
    - https://example.com/doc?token=ref-secret
hooks:
  session_end:
    - command: ["curl", "--token", "hook-secret", "-H", "Authorization: Bearer header-secret", "https://example.com/hook?token=url-secret"]
      env:
        API_TOKEN: hook-secret
`), 0o600))

	report := NewDiagnosticsReport([]PathSource{{Path: path, Kind: OriginExplicitFile}})
	require.Empty(t, report.LoadError)
	assertDefaultDiagnostic(t, report.Defaults, "agent_loop.max_iterations")
	assertDefaultDiagnostic(t, report.Defaults, "agent_loop.max_tool_calls")
	assertDefaultDiagnostic(t, report.Defaults, "agent_loop.max_cost_micros")
	assertDefaultDiagnostic(t, report.Defaults, "agent_loop.max_input_tokens")
	assertDefaultDiagnostic(t, report.Defaults, "agent_loop.max_output_tokens")
	assertDefaultDiagnostic(t, report.Defaults, "context.reference_policy.allow_absolute_paths")
	assertDefaultDiagnostic(t, report.Defaults, "providers.*.disable_private_adapter")
	assertDefaultDiagnostic(t, report.Defaults, "skill_learning.enabled")
	assertDefaultDiagnostic(t, report.Defaults, "vector.workspace_enabled")
	assertDefaultDiagnostic(t, report.Defaults, "vector.workspace_allow_remote_embeddings")

	assert.NotContains(t, report.Config.Providers["openai"].BaseURL, "secret")
	assert.Equal(t, RedactedValue, report.Config.Agents["reviewer"].Description)
	assert.Equal(t, RedactedValue, report.Config.Agents["reviewer"].Personality)
	assert.Equal(t, RedactedValue, report.Config.Agents["reviewer"].SystemPrompt)
	require.Len(t, report.Config.Hooks["session_end"], 1)
	assert.Equal(t, RedactedValue, report.Config.Hooks["session_end"][0].Env["API_TOKEN"])
	assert.Equal(t, RedactedValue, report.Config.Hooks["session_end"][0].Command[2])
	assert.NotContains(t, report.Config.Hooks["session_end"][0].Command[4], "header-secret")
	assert.NotContains(t, report.Config.Hooks["session_end"][0].Command[5], "url-secret")
	assert.NotContains(t, report.Config.Context.References[0], "ref-secret")
	assert.NotContains(t, report.Config.Agents["reviewer"].References[0], "agent-secret")

	final, ok := report.Origins.Final("providers.openai.base_url")
	require.True(t, ok)
	assert.NotContains(t, final.Value, "sk-secret")
	assert.NotContains(t, final.Value, "user:secret")

	hookOrigin, ok := report.Origins.Final("hooks.session_end")
	require.True(t, ok)
	assert.NotContains(t, hookOrigin.Value, "hook-secret")
	assert.NotContains(t, hookOrigin.Value, "header-secret")
	assert.NotContains(t, hookOrigin.Value, "url-secret")

	contextRefsOrigin, ok := report.Origins.Final("context.references")
	require.True(t, ok)
	assert.NotContains(t, contextRefsOrigin.Value, "ref-secret")

	descriptionOrigin, ok := report.Origins.Final("agents.reviewer.description")
	require.True(t, ok)
	assert.Equal(t, RedactedValue, descriptionOrigin.Value)

	personalityOrigin, ok := report.Origins.Final("agents.reviewer.personality")
	require.True(t, ok)
	assert.Equal(t, RedactedValue, personalityOrigin.Value)

	systemPromptOrigin, ok := report.Origins.Final("agents.reviewer.system_prompt")
	require.True(t, ok)
	assert.Equal(t, RedactedValue, systemPromptOrigin.Value)

	out, err := yaml.Marshal(report)
	require.NoError(t, err)
	assert.NotContains(t, string(out), "internal reviewer")
	assert.NotContains(t, string(out), "proprietary instructions")
	assert.NotContains(t, string(out), "secret")
	assert.NotContains(t, string(out), "header-secret")
	assert.NotContains(t, string(out), "url-secret")
	assert.NotContains(t, string(out), "ref-secret")
	assert.NotContains(t, string(out), "agent-secret")
}

func TestNewDefaultDiagnosticsReport_IncludesHarnessDiagnostics(t *testing.T) {
	tempDir := t.TempDir()
	codexHome := filepath.Join(tempDir, "codex")
	require.NoError(t, os.MkdirAll(codexHome, 0o700))

	codexConfig := filepath.Join(codexHome, "config.toml")
	require.NoError(t, os.WriteFile(codexConfig, []byte(`
model = "gpt-5.5"
trusted_project_roots = ["/private/repo"]
`), 0o600))

	t.Setenv("HOME", tempDir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tempDir, "config"))
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("OPENCODE_CONFIG", "")
	t.Setenv("OPENCODE_CONFIG_DIR", "")
	t.Setenv("FORGE_CONFIG", filepath.Join(tempDir, "missing-forge"))
	t.Setenv(EnvPath, "")
	t.Setenv(EnvStatePath, filepath.Join(tempDir, "state.yaml"))
	t.Chdir(tempDir)

	report := NewDefaultDiagnosticsReport()

	assert.Empty(t, report.LoadError)
	assert.Contains(t, report.LoadedSources, codexConfig)
	assertDiagnosticContains(t, report.Diagnostics, "trusted_project_roots: ignored unsupported field")

	out, err := yaml.Marshal(report)
	require.NoError(t, err)
	assert.NotContains(t, string(out), "/private/repo")
}

func TestInspectStatePath_ReportsStateMetadataWithoutPreferences(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`version: 1
revision: 7
default_model: secret-model
default_reasoning_level: high
default_model_mode: fast
folders:
  /private/project:
    default_model: folder-model
    default_reasoning_level: xhigh
    default_model_mode: default
`), 0o600))

	report := InspectStatePath(path)
	assert.Equal(t, path, report.Path)
	assert.Equal(t, "present", report.Status)
	assert.Equal(t, StateSchemaVersion, report.Version)
	assert.Equal(t, int64(7), report.Revision)
	assert.Empty(t, report.Error)
	assert.Empty(t, report.Diagnostics)
}

func TestInspectStatePath_ReportsLegacyStateVersion(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.yaml")
	require.NoError(t, os.WriteFile(path, []byte("default_model: private-model\n"), 0o600))

	report := InspectStatePath(path)
	assert.Equal(t, "present", report.Status)
	assert.Equal(t, StateSchemaVersion, report.Version)
	assert.Empty(t, report.Error)
	assertDiagnostic(t, report.Diagnostics, DiagnosticInfo, "version", "")
}

func TestInspectStatePath_ReportsCorruptedStateWithRecoveryHint(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.yaml")
	require.NoError(t, os.WriteFile(path, []byte("version: [broken\n"), 0o600))

	report := InspectStatePath(path)
	assert.Equal(t, path, report.Path)
	assert.Equal(t, "error", report.Status)
	assert.Contains(t, report.Error, path)
	assert.Contains(t, report.Error, "fix the YAML")
	assert.Contains(t, report.Error, "move this file aside")
}

func TestInspectStatePath_ReportsUnknownStateFieldsWithoutValues(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`version: 1
revision: 7
future_metadata:
  token: private-state-value
folders:
  /private/project:
    default_model: folder-model
    future_folder_metadata: private-folder-value
`), 0o600))

	report := InspectStatePath(path)
	assert.Equal(t, "present", report.Status)
	assert.Empty(t, report.Error)
	assertDiagnosticMessage(t, report.Diagnostics, DiagnosticInfo, "unknown state field is preserved across writes")
	assertDiagnostic(t, report.Diagnostics, DiagnosticInfo, "future_metadata", "")
	assertDiagnostic(t, report.Diagnostics, DiagnosticInfo, "folders.*.future_folder_metadata", "")

	out, err := yaml.Marshal(report)
	require.NoError(t, err)
	assert.NotContains(t, string(out), "private-state-value")
	assert.NotContains(t, string(out), "private-folder-value")
	assert.NotContains(t, string(out), "/private/project")
}

func TestDefaultDiagnostics_ReturnsCopy(t *testing.T) {
	t.Parallel()

	defaults := DefaultDiagnostics()
	require.NotEmpty(t, defaults)

	defaults[0].Value = "mutated"

	assert.NotEqual(t, "mutated", DefaultDiagnostics()[0].Value)
}

func TestDefaultDiagnostics_ReferencePolicySafetyDefaults(t *testing.T) {
	t.Parallel()

	defaults := DefaultDiagnostics()

	assertDefaultDiagnosticValue(t, defaults, "context.reference_policy.allowed_schemes", "[https]")
	assertDefaultDiagnosticValue(t, defaults, "context.reference_policy.allow_absolute_paths", "false")
	assertDefaultDiagnosticValue(t, defaults, "context.reference_policy.max_redirects", "0")
	assertDefaultDiagnosticValue(t, defaults, "context.reference_policy.max_files", "200")

	contentTypes := defaultDiagnosticValue(t, defaults, "context.reference_policy.content_types")
	assert.Contains(t, contentTypes, "text/*")
	assert.Contains(t, contentTypes, "application/json")
	assert.Contains(t, contentTypes, "application/toml")
}

func assertDiagnostic(
	t *testing.T,
	diagnostics []Diagnostic,
	severity DiagnosticSeverity,
	field string,
	replacement string,
) {
	t.Helper()

	for _, diagnostic := range diagnostics {
		if diagnostic.Severity == severity && diagnostic.Field == field && diagnostic.Replacement == replacement {
			return
		}
	}

	require.Failf(t, "missing diagnostic", "severity=%s field=%s replacement=%s diagnostics=%v", severity, field, replacement, diagnostics)
}

func assertDiagnosticMessage(
	t *testing.T,
	diagnostics []Diagnostic,
	severity DiagnosticSeverity,
	message string,
) {
	t.Helper()

	for _, diagnostic := range diagnostics {
		if diagnostic.Severity == severity && diagnostic.Message == message {
			return
		}
	}

	require.Failf(t, "missing diagnostic", "severity=%s message=%s diagnostics=%v", severity, message, diagnostics)
}

func assertNoDiagnostic(t *testing.T, diagnostics []Diagnostic, field string) {
	t.Helper()

	for _, diagnostic := range diagnostics {
		if diagnostic.Field == field {
			require.Failf(t, "unexpected diagnostic", "field=%s diagnostic=%v diagnostics=%v", field, diagnostic, diagnostics)
		}
	}
}

func assertDefaultDiagnostic(t *testing.T, defaults []DefaultDiagnostic, field string) {
	t.Helper()

	_ = defaultDiagnosticValue(t, defaults, field)
}

func assertDefaultDiagnosticValue(t *testing.T, defaults []DefaultDiagnostic, field, value string) {
	t.Helper()

	assert.Equal(t, value, defaultDiagnosticValue(t, defaults, field))
}

func defaultDiagnosticValue(t *testing.T, defaults []DefaultDiagnostic, field string) string {
	t.Helper()

	for _, defaultInfo := range defaults {
		if defaultInfo.Field == field {
			return defaultInfo.Value
		}
	}

	require.Failf(t, "missing default diagnostic", "field=%s defaults=%v", field, defaults)

	return ""
}
