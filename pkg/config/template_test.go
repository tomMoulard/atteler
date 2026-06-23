package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestTemplateYAML(t *testing.T) {
	t.Parallel()

	template := TemplateYAML()
	for _, want := range []string{
		"Save as ~/.config/atteler/config.yaml for global config.",
		"Use ./.atteler/config.yaml for ignored/private local config, or ./.atteler.yaml for reviewed shared config.",
		"default_provider:",
		"model_aliases:",
		"models:",
		"planner:",
		"fast_coder:",
		"max_latency_ms: 2500",
		"max_ttft_ms: 900",
		"version:",
		"generation:",
		"research:",
		"source_policy:",
		"trusted_domains:",
		"denied_domains:",
		"prefer_source_types:",
		"allow_low_trust_sources: true",
		"warn_on_low_trust_sources: true",
		"require_evidence_for_high_impact_claims: false",
		"Research source policy is evidence-first, not evidence-only",
		"agent_loop:",
		"max_output_bytes:",
		"max_cost_micros:",
		"max_input_tokens:",
		"max_output_tokens:",
		"max_total_tokens:",
		"max_iterations:",
		"max_model_calls:",
		"max_tool_calls:",
		"max_wall_time:",
		"checkpoint_interval:",
		"providers:",
		"retry:",
		"max_attempts:",
		"initial_backoff_ms:",
		"max_backoff_ms:",
		"max_elapsed_ms:",
		"jitter_fraction:",
		"agents:",
		"routing_policy:",
		"hooks:",
		"context:",
		"project_instructions:",
		"max_tokens: 8192",
		"plugins:",
		"policy:",
		"trusted_install_sources:",
		"vector:",
		"workspace_enabled: false",
		"workspace_allow_remote_embeddings: false",
		"Top-level vector.index_path is the generic file-vector search store path.",
		"Workspace vectors use vector.workspace_index_path or vector.stores.workspace.index_path.",
		"Do not share one index_path across workspace, file, session, git, ADR, or agent-memory indexes.",
		"Use vector.stores.<name>, vector.agents.<name>, and vector.sources.<kind>",
		"Supported vector store scopes: agent-memory, vector-search, workspace.",
		"Supported vector source scopes: file, session, git_history, adr.",
		"Vector agent scopes must match configured agent names.",
		"vectorizer: lexical",
		"fallback_policy: fail",
		"workspace_index_path: ./.atteler/workspace-vector-index.json",
		"workspace_exclude:",
		"workspace_limit: 4",
		"worktree:",
		"auto_merge: false",
		"verification_commands:",
		"override_verification: false",
	} {
		if !strings.Contains(template, want) {
			require.Failf(t, "unexpected failure", "TemplateYAML missing %q in:\n%s", want, template)
		}
	}

	if strings.Contains(template, "api.openai.com/v1") {
		require.Failf(t, "unexpected failure", "TemplateYAML should use OpenAI host root, got:\n%s", template)
	}

	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(template), 0o600))

	cfg, _, err := LoadFiles([]string{path})
	require.NoError(t, err)
	assert.Equal(t, ConfigSchemaVersion, cfg.Version)
	assert.Equal(t, starterTemplateConfig(), cfg)
	assert.NotContains(t, cfg.Providers["vllm"].Capabilities, "streaming")
}

func TestTemplateYAMLAgentLoopSchemaMatchesDiagnostics(t *testing.T) {
	t.Parallel()

	assert.Equal(t, yamlFieldsForType[AgentLoopConfig](), knownAgentLoopFields())
	assert.Equal(t, yamlFieldsForType[fileAgentLoopConfig](), knownAgentLoopFields())

	var root yaml.Node
	require.NoError(t, yaml.Unmarshal([]byte(TemplateYAML()), &root))
	require.NotEmpty(t, root.Content)

	agentLoop := templateMappingValue(root.Content[0], "agent_loop")
	require.NotNil(t, agentLoop, "template should contain agent_loop")

	templateFields := make(map[string]bool)
	for i := 0; i+1 < len(agentLoop.Content); i += 2 {
		templateFields[agentLoop.Content[i].Value] = true
	}

	assert.Equal(t, knownAgentLoopFields(), templateFields)

	defaultFields := make(map[string]bool)
	for _, diag := range DefaultDiagnostics() {
		defaultFields[diag.Field] = true
	}

	for field := range knownAgentLoopFields() {
		assert.Truef(t, defaultFields["agent_loop."+field], "DefaultDiagnostics missing agent_loop.%s", field)
	}
}

func TestTemplateYAMLProjectInstructionsSchemaMatchesDiagnostics(t *testing.T) {
	t.Parallel()

	assert.Equal(t, yamlFieldsForType[ProjectInstructionsConfig](), knownProjectInstructionFields())
	assert.Equal(t, yamlFieldsForType[fileProjectInstructionsConfig](), knownProjectInstructionFields())

	var root yaml.Node
	require.NoError(t, yaml.Unmarshal([]byte(TemplateYAML()), &root))
	require.NotEmpty(t, root.Content)

	contextNode := templateMappingValue(root.Content[0], "context")
	require.NotNil(t, contextNode, "template should contain context")

	projectInstructions := templateMappingValue(contextNode, "project_instructions")
	require.NotNil(t, projectInstructions, "template should contain context.project_instructions")

	templateFields := make(map[string]bool)
	for i := 0; i+1 < len(projectInstructions.Content); i += 2 {
		templateFields[projectInstructions.Content[i].Value] = true
	}

	assert.Equal(t, knownProjectInstructionFields(), templateFields)
}

func TestStarterTemplateConfigAgentLoopPointersAreIndependent(t *testing.T) {
	t.Parallel()

	cfg := starterTemplateConfig()
	require.NotNil(t, cfg.AgentLoop.MaxOutputBytes)
	require.NotNil(t, cfg.AgentLoop.MaxCostMicros)
	require.NotNil(t, cfg.AgentLoop.MaxInputTokens)
	require.NotNil(t, cfg.AgentLoop.MaxOutputTokens)
	require.NotNil(t, cfg.AgentLoop.MaxTotalTokens)

	*cfg.AgentLoop.MaxCostMicros = 456
	*cfg.AgentLoop.MaxInputTokens = 123

	assert.EqualValues(t, 456, *cfg.AgentLoop.MaxCostMicros)
	assert.Zero(t, *cfg.AgentLoop.MaxOutputBytes)
	assert.Equal(t, 123, *cfg.AgentLoop.MaxInputTokens)
	assert.Zero(t, *cfg.AgentLoop.MaxOutputTokens)
	assert.Zero(t, *cfg.AgentLoop.MaxTotalTokens)
}

func TestREADMEAgentLoopSchemaMatchesConfig(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	require.NoError(t, err)

	assert.Equal(t, knownAgentLoopFields(), readmeAgentLoopFieldsForTest(string(data)))
}

func templateMappingValue(root *yaml.Node, key string) *yaml.Node {
	if root == nil || root.Kind != yaml.MappingNode {
		return nil
	}

	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == key {
			return root.Content[i+1]
		}
	}

	return nil
}

func readmeAgentLoopFieldsForTest(readme string) map[string]bool {
	_, section, ok := strings.Cut(readme, "\nagent_loop:\n")
	if !ok {
		return nil
	}

	fields := make(map[string]bool)

	for line := range strings.SplitSeq(section, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		if !strings.HasPrefix(line, "  ") {
			break
		}

		key, _, ok := strings.Cut(trimmed, ":")
		if ok && key != "" {
			fields[key] = true
		}
	}

	return fields
}

func yamlFieldsForType[T any]() map[string]bool {
	configType := reflect.TypeFor[T]()
	fields := make(map[string]bool, configType.NumField())

	for field := range configType.Fields() {
		name, _, _ := strings.Cut(field.Tag.Get("yaml"), ",")

		if name == "" || name == "-" {
			continue
		}

		fields[name] = true
	}

	return fields
}

func TestTemplateYAMLDocumentsHookPrivacyDefaults(t *testing.T) {
	t.Parallel()

	template := TemplateYAML()
	for _, want := range []string{
		"payload defaults to metadata",
		"payload: metadata",
		"blocking: false",
		"inherit_env: false",
		"Explicit env values are passed verbatim",
		"ATTELER_* variables are reserved",
		"event_ledger_path",
	} {
		require.Contains(t, template, want)
	}
}
