package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	appconfig "github.com/tommoulard/atteler/pkg/config"
	"github.com/tommoulard/atteler/pkg/events"
)

func TestValidateConfig_PrintsHarnessImporterWarnings(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("HOME", tempDir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tempDir, "config"))
	t.Setenv("CODEX_HOME", filepath.Join(tempDir, "missing-codex"))
	t.Setenv("OPENCODE_CONFIG", "")
	t.Setenv("OPENCODE_CONFIG_DIR", "")
	t.Setenv("FORGE_CONFIG", filepath.Join(tempDir, "missing-forge"))
	t.Setenv("ATTELER_CONFIG", "")
	t.Chdir(tempDir)

	claudeDir := filepath.Join(tempDir, ".claude")
	require.NoError(t, os.MkdirAll(claudeDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(claudeDir, "settings.json"), []byte(`{
		"model": "claude-sonnet-4-20250514",
		"unsupported": true
	}`), 0o600))

	out := captureStdoutForStateDiagnostics(t, func() {
		require.NoError(t, validateConfig())
	})

	assert.Contains(t, out, "Config importer warnings:")
	assert.Contains(t, out, "claude: "+filepath.Join(claudeDir, "settings.json")+" unsupported: ignored unsupported field")
	assert.Contains(t, out, "Config valid:")
}

func TestValidateConfig_PrintsMalformedHarnessImporterWarnings(t *testing.T) {
	tempDir := t.TempDir()
	codexHome := filepath.Join(tempDir, "codex")
	require.NoError(t, os.MkdirAll(codexHome, 0o700))
	codexConfig := filepath.Join(codexHome, "config.toml")
	require.NoError(t, os.WriteFile(codexConfig, []byte(`
model = "gpt-5.5"
[model_providers.openai
base_url = "https://openai.example"
`), 0o600))

	t.Setenv("HOME", tempDir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tempDir, "config"))
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("OPENCODE_CONFIG", "")
	t.Setenv("OPENCODE_CONFIG_DIR", "")
	t.Setenv("FORGE_CONFIG", filepath.Join(tempDir, "missing-forge"))
	t.Setenv("ATTELER_CONFIG", "")
	t.Chdir(tempDir)

	out := captureStdoutForStateDiagnostics(t, func() {
		require.NoError(t, validateConfig())
	})

	assert.Contains(t, out, "Config importer warnings:")
	assert.Contains(t, out, "codex: "+codexConfig+": ignored harness config: parse TOML")
	assert.Contains(t, out, "Config valid: no config files loaded.")
}

func TestValidateConfig_PrintsHarnessImporterWarningsBeforeConfigError(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("HOME", tempDir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tempDir, "config"))
	t.Setenv("CODEX_HOME", filepath.Join(tempDir, "missing-codex"))
	t.Setenv("OPENCODE_CONFIG", "")
	t.Setenv("OPENCODE_CONFIG_DIR", "")
	t.Setenv("FORGE_CONFIG", filepath.Join(tempDir, "missing-forge"))
	t.Setenv("ATTELER_CONFIG", "")
	t.Chdir(tempDir)

	claudeDir := filepath.Join(tempDir, ".claude")
	require.NoError(t, os.MkdirAll(claudeDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(claudeDir, "settings.json"), []byte(`{
		"model": "claude-sonnet-4-20250514",
		"unsupported": true
	}`), 0o600))

	configDir := filepath.Join(tempDir, ".atteler")
	require.NoError(t, os.MkdirAll(configDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(`default_model: [`), 0o600))

	var err error

	out := captureStdoutForStateDiagnostics(t, func() {
		err = validateConfig()
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "validate config:")
	assert.Contains(t, out, "Config importer warnings:")
	assert.Contains(t, out, "claude: "+filepath.Join(claudeDir, "settings.json")+" unsupported: ignored unsupported field")
	assert.NotContains(t, out, "Config valid:")
}

func TestValidateConfig_AcceptsAllAgentLoopBudgetFields(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("HOME", tempDir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tempDir, "config"))
	t.Setenv("CODEX_HOME", filepath.Join(tempDir, "missing-codex"))
	t.Setenv("OPENCODE_CONFIG", "")
	t.Setenv("OPENCODE_CONFIG_DIR", "")
	t.Setenv("FORGE_CONFIG", filepath.Join(tempDir, "missing-forge"))
	t.Setenv("ATTELER_CONFIG", "")
	t.Chdir(tempDir)

	configPath := filepath.Join(tempDir, ".atteler", "config.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(configPath), 0o700))
	require.NoError(t, os.WriteFile(configPath, []byte(`agent_loop:
  max_output_bytes: 4096
  max_cost_micros: 250000
  max_input_tokens: 1000
  max_output_tokens: 2000
  max_total_tokens: 3000
  max_iterations: 4
  max_model_calls: 5
  max_tool_calls: 6
  max_wall_time: 30m
  checkpoint_interval: 7
`), 0o600))

	out := captureStdoutForStateDiagnostics(t, func() {
		require.NoError(t, validateConfig())
	})

	assert.Contains(t, out, "Config valid: "+configPath)
}

func TestValidateConfig_RejectsAgentLoopBudgetFields(t *testing.T) {
	for _, tc := range []struct {
		name      string
		field     string
		value     string
		wantError string
	}{
		{
			name:      "output bytes",
			field:     "max_output_bytes",
			value:     "-1",
			wantError: "agent_loop.max_output_bytes must be >= 0",
		},
		{
			name:      "cost",
			field:     "max_cost_micros",
			value:     "-1",
			wantError: "agent_loop.max_cost_micros must be >= 0",
		},
		{
			name:      "input tokens",
			field:     "max_input_tokens",
			value:     "-1",
			wantError: "agent_loop.max_input_tokens must be >= 0",
		},
		{
			name:      "output tokens",
			field:     "max_output_tokens",
			value:     "-1",
			wantError: "agent_loop.max_output_tokens must be >= 0",
		},
		{
			name:      "total tokens",
			field:     "max_total_tokens",
			value:     "-1",
			wantError: "agent_loop.max_total_tokens must be >= 0",
		},
		{
			name:      "iterations",
			field:     "max_iterations",
			value:     "-1",
			wantError: "agent_loop.max_iterations must be >= 0",
		},
		{
			name:      "model calls",
			field:     "max_model_calls",
			value:     "-1",
			wantError: "agent_loop.max_model_calls must be >= 0",
		},
		{
			name:      "tool calls",
			field:     "max_tool_calls",
			value:     "-1",
			wantError: "agent_loop.max_tool_calls must be >= 0",
		},
		{
			name:      "wall time",
			field:     "max_wall_time",
			value:     "-1s",
			wantError: "agent_loop.max_wall_time must be >= 0",
		},
		{
			name:      "checkpoint interval",
			field:     "checkpoint_interval",
			value:     "-1",
			wantError: "agent_loop.checkpoint_interval must be >= 0",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tempDir := t.TempDir()
			t.Setenv("HOME", tempDir)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(tempDir, "config"))
			t.Setenv("CODEX_HOME", filepath.Join(tempDir, "missing-codex"))
			t.Setenv("OPENCODE_CONFIG", "")
			t.Setenv("OPENCODE_CONFIG_DIR", "")
			t.Setenv("FORGE_CONFIG", filepath.Join(tempDir, "missing-forge"))
			t.Setenv("ATTELER_CONFIG", "")
			t.Chdir(tempDir)

			configDir := filepath.Join(tempDir, ".atteler")
			require.NoError(t, os.MkdirAll(configDir, 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte("agent_loop:\n  "+tc.field+": "+tc.value+"\n"), 0o600))

			err := validateConfig()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantError)
		})
	}
}

func TestValidateConfig_RejectsNegativeRoutingLimits(t *testing.T) {
	for _, tc := range []struct {
		name      string
		config    string
		wantError string
	}{
		{
			name: "model role cost",
			config: `models:
  planner:
    preferred: openai/gpt-4.1-mini
    max_cost_usd: -0.01
`,
			wantError: "models.planner.max_cost_usd must be >= 0",
		},
		{
			name: "model role latency",
			config: `models:
  planner:
    preferred: openai/gpt-4.1-mini
    max_latency_ms: -1
`,
			wantError: "models.planner.max_latency_ms must be >= 0",
		},
		{
			name: "model role ttft",
			config: `models:
  planner:
    preferred: openai/gpt-4.1-mini
    max_ttft_ms: -1
`,
			wantError: "models.planner.max_ttft_ms must be >= 0",
		},
		{
			name: "model role routing budget",
			config: `models:
  planner:
    preferred: openai/gpt-4.1-mini
    routing_policy:
      max_budget: -0.01
`,
			wantError: "models.planner.routing_policy.max_budget must be >= 0",
		},
		{
			name: "agent routing latency",
			config: `agents:
  reviewer:
    model: openai/gpt-4.1-mini
    routing_policy:
      max_latency_ms: -1
`,
			wantError: "agents.reviewer.routing_policy.max_latency_ms must be >= 0",
		},
		{
			name: "agent routing ttft",
			config: `agents:
  reviewer:
    model: openai/gpt-4.1-mini
    routing_policy:
      max_ttft_ms: -1
`,
			wantError: "agents.reviewer.routing_policy.max_ttft_ms must be >= 0",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tempDir := t.TempDir()
			t.Setenv("HOME", tempDir)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(tempDir, "config"))
			t.Setenv("CODEX_HOME", filepath.Join(tempDir, "missing-codex"))
			t.Setenv("OPENCODE_CONFIG", "")
			t.Setenv("OPENCODE_CONFIG_DIR", "")
			t.Setenv("FORGE_CONFIG", filepath.Join(tempDir, "missing-forge"))
			t.Setenv("ATTELER_CONFIG", "")
			t.Chdir(tempDir)

			configDir := filepath.Join(tempDir, ".atteler")
			require.NoError(t, os.MkdirAll(configDir, 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(tc.config), 0o600))

			err := validateConfig()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantError)
		})
	}
}

func TestValidateConfig_RejectsNonFiniteRoutingLimits(t *testing.T) {
	for _, tc := range []struct {
		name      string
		config    string
		wantError string
	}{
		{
			name: "model role cost",
			config: `models:
  planner:
    preferred: openai/gpt-4.1-mini
    max_cost_usd: .nan
`,
			wantError: "models.planner.max_cost_usd must be finite",
		},
		{
			name: "model role routing budget",
			config: `models:
  planner:
    preferred: openai/gpt-4.1-mini
    routing_policy:
      max_budget: .inf
`,
			wantError: "models.planner.routing_policy.max_budget must be finite",
		},
		{
			name: "agent routing budget",
			config: `agents:
  reviewer:
    model: openai/gpt-4.1-mini
    routing_policy:
      max_budget: -.inf
`,
			wantError: "agents.reviewer.routing_policy.max_budget must be finite",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tempDir := t.TempDir()
			t.Setenv("HOME", tempDir)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(tempDir, "config"))
			t.Setenv("CODEX_HOME", filepath.Join(tempDir, "missing-codex"))
			t.Setenv("OPENCODE_CONFIG", "")
			t.Setenv("OPENCODE_CONFIG_DIR", "")
			t.Setenv("FORGE_CONFIG", filepath.Join(tempDir, "missing-forge"))
			t.Setenv("ATTELER_CONFIG", "")
			t.Chdir(tempDir)

			configDir := filepath.Join(tempDir, ".atteler")
			require.NoError(t, os.MkdirAll(configDir, 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(tc.config), 0o600))

			err := validateConfig()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantError)
		})
	}
}

func TestValidateConfig_RejectsInvalidModelRoleDefinitions(t *testing.T) {
	for _, tc := range []struct {
		name      string
		config    string
		wantError string
	}{
		{
			name: "empty role name",
			config: `models:
  "":
    preferred: openai/gpt-4.1-mini
`,
			wantError: "models role name cannot be empty",
		},
		{
			name: "provider qualified role name",
			config: `models:
  openai/planner:
    preferred: openai/gpt-4.1-mini
`,
			wantError: "models.openai/planner role name must be a bare name",
		},
		{
			name: "empty candidate chain",
			config: `models:
  planner:
    required_capabilities: [tools]
`,
			wantError: "models.planner needs a preferred model or fallback model",
		},
		{
			name: "blank candidate chain",
			config: `models:
  planner:
    preferred: " "
    fallback_models: [" "]
`,
			wantError: "models.planner needs a preferred model or fallback model",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tempDir := t.TempDir()
			t.Setenv("HOME", tempDir)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(tempDir, "config"))
			t.Setenv("CODEX_HOME", filepath.Join(tempDir, "missing-codex"))
			t.Setenv("OPENCODE_CONFIG", "")
			t.Setenv("OPENCODE_CONFIG_DIR", "")
			t.Setenv("FORGE_CONFIG", filepath.Join(tempDir, "missing-forge"))
			t.Setenv("ATTELER_CONFIG", "")
			t.Chdir(tempDir)

			configDir := filepath.Join(tempDir, ".atteler")
			require.NoError(t, os.MkdirAll(configDir, 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(tc.config), 0o600))

			err := validateConfig()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantError)
		})
	}
}

func TestValidateConfig_RejectsUnknownRouteCapabilities(t *testing.T) {
	const validCapabilities = "valid: text,chat,tools,reasoning,json_schema,embeddings,vision,multimodal,batch,prompt_cache,streaming,rate_limits,retries,fallback,cost_tracking,local"

	for _, tc := range []struct {
		name      string
		config    string
		wantError string
	}{
		{
			name: "provider capabilities",
			config: `providers:
  localai:
    type: openai-compatible
    base_url: http://127.0.0.1:8080/v1
    models: [tiny]
    capabilities: [chat, time_travel]
`,
			wantError: `providers.localai.capabilities contains unknown capability "time_travel"`,
		},
		{
			name: "model role required capabilities",
			config: `models:
  planner:
    preferred: openai/gpt-4.1-mini
    required_capabilities: [tools, clairvoyance]
`,
			wantError: `models.planner.required_capabilities contains unknown capability "clairvoyance"`,
		},
		{
			name: "model role routing policy capabilities",
			config: `models:
  planner:
    preferred: openai/gpt-4.1-mini
    routing_policy:
      required_capabilities: [json_schema, teleport]
`,
			wantError: `models.planner.routing_policy.required_capabilities contains unknown capability "teleport"`,
		},
		{
			name: "agent routing policy capabilities",
			config: `agents:
  reviewer:
    model: openai/gpt-4.1-mini
    routing_policy:
      required_capabilities: [tools, impossible]
`,
			wantError: `agents.reviewer.routing_policy.required_capabilities contains unknown capability "impossible"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tempDir := t.TempDir()
			t.Setenv("HOME", tempDir)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(tempDir, "config"))
			t.Setenv("CODEX_HOME", filepath.Join(tempDir, "missing-codex"))
			t.Setenv("OPENCODE_CONFIG", "")
			t.Setenv("OPENCODE_CONFIG_DIR", "")
			t.Setenv("FORGE_CONFIG", filepath.Join(tempDir, "missing-forge"))
			t.Setenv("ATTELER_CONFIG", "")
			t.Chdir(tempDir)

			configDir := filepath.Join(tempDir, ".atteler")
			require.NoError(t, os.MkdirAll(configDir, 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(tc.config), 0o600))

			err := validateConfig()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantError)
			assert.Contains(t, err.Error(), validCapabilities)
		})
	}
}

func TestValidateConfig_RejectsUnknownProviderType(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("HOME", tempDir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tempDir, "config"))
	t.Setenv("CODEX_HOME", filepath.Join(tempDir, "missing-codex"))
	t.Setenv("OPENCODE_CONFIG", "")
	t.Setenv("OPENCODE_CONFIG_DIR", "")
	t.Setenv("FORGE_CONFIG", filepath.Join(tempDir, "missing-forge"))
	t.Setenv("ATTELER_CONFIG", "")
	t.Chdir(tempDir)

	configDir := filepath.Join(tempDir, ".atteler")
	require.NoError(t, os.MkdirAll(configDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(`providers:
  localai:
    type: telepathy
    base_url: http://127.0.0.1:8080/v1
    models: [tiny]
`), 0o600))

	err := validateConfig()
	require.Error(t, err)
	assert.Contains(t, err.Error(), `providers.localai.type unsupported provider type "telepathy"`)
	assert.Contains(t, err.Error(), "openai_compatible")
}

func TestValidateConfig_RejectsCustomProviderWithoutType(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("HOME", tempDir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tempDir, "config"))
	t.Setenv("CODEX_HOME", filepath.Join(tempDir, "missing-codex"))
	t.Setenv("OPENCODE_CONFIG", "")
	t.Setenv("OPENCODE_CONFIG_DIR", "")
	t.Setenv("FORGE_CONFIG", filepath.Join(tempDir, "missing-forge"))
	t.Setenv("ATTELER_CONFIG", "")
	t.Chdir(tempDir)

	configDir := filepath.Join(tempDir, ".atteler")
	require.NoError(t, os.MkdirAll(configDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(`providers:
  localai:
    base_url: http://127.0.0.1:8080/v1
    models: [tiny]
`), 0o600))

	err := validateConfig()
	require.Error(t, err)
	assert.Contains(t, err.Error(), `providers.localai.type missing for custom provider "localai"`)
	assert.Contains(t, err.Error(), "openai_compatible")
}

func TestValidateConfig_RejectsOpenAICompatibleProviderWithoutBaseURL(t *testing.T) {
	tests := []struct {
		name      string
		config    string
		wantError string
	}{
		{
			name: "explicit compatible type",
			config: `providers:
  localai:
    type: openai_compatible
    models: [tiny]
`,
			wantError: `providers.localai.base_url missing for OpenAI-compatible provider "localai"`,
		},
		{
			name: "alias provider name",
			config: `providers:
  groq:
    models: [llama-test]
`,
			wantError: `providers.groq.base_url missing for OpenAI-compatible provider "groq"`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tempDir := t.TempDir()
			t.Setenv("HOME", tempDir)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(tempDir, "config"))
			t.Setenv("CODEX_HOME", filepath.Join(tempDir, "missing-codex"))
			t.Setenv("OPENCODE_CONFIG", "")
			t.Setenv("OPENCODE_CONFIG_DIR", "")
			t.Setenv("FORGE_CONFIG", filepath.Join(tempDir, "missing-forge"))
			t.Setenv("ATTELER_CONFIG", "")
			t.Chdir(tempDir)

			configDir := filepath.Join(tempDir, ".atteler")
			require.NoError(t, os.MkdirAll(configDir, 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(tc.config), 0o600))

			err := validateConfig()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantError)
		})
	}
}

func TestValidateConfig_RejectsInvalidOpenAICompatibleProviderPath(t *testing.T) {
	tests := []struct {
		name      string
		field     string
		wantError string
	}{
		{
			name:      "chat completions path",
			field:     "chat_completions_path: v1/chat/completions",
			wantError: "providers.localai.chat_completions_path must start with /",
		},
		{
			name:      "embeddings path",
			field:     "embeddings_path: v1/embeddings",
			wantError: "providers.localai.embeddings_path must start with /",
		},
		{
			name:      "models path",
			field:     "models_path: v1/models",
			wantError: "providers.localai.models_path must start with /",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tempDir := t.TempDir()
			t.Setenv("HOME", tempDir)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(tempDir, "config"))
			t.Setenv("CODEX_HOME", filepath.Join(tempDir, "missing-codex"))
			t.Setenv("OPENCODE_CONFIG", "")
			t.Setenv("OPENCODE_CONFIG_DIR", "")
			t.Setenv("FORGE_CONFIG", filepath.Join(tempDir, "missing-forge"))
			t.Setenv("ATTELER_CONFIG", "")
			t.Chdir(tempDir)

			configDir := filepath.Join(tempDir, ".atteler")
			require.NoError(t, os.MkdirAll(configDir, 0o700))

			config := fmt.Sprintf(`providers:
  localai:
    type: openai_compatible
    base_url: http://127.0.0.1:8080/v1
    %s
    models: [tiny]
`, tc.field)
			require.NoError(t, os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(config), 0o600))

			err := validateConfig()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantError)
		})
	}
}

func TestValidateConfig_RejectsIncompatibleBuiltinProviderType(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("HOME", tempDir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tempDir, "config"))
	t.Setenv("CODEX_HOME", filepath.Join(tempDir, "missing-codex"))
	t.Setenv("OPENCODE_CONFIG", "")
	t.Setenv("OPENCODE_CONFIG_DIR", "")
	t.Setenv("FORGE_CONFIG", filepath.Join(tempDir, "missing-forge"))
	t.Setenv("ATTELER_CONFIG", "")
	t.Chdir(tempDir)

	configDir := filepath.Join(tempDir, ".atteler")
	require.NoError(t, os.MkdirAll(configDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(`providers:
  anthropic:
    type: openai_compatible
`), 0o600))

	err := validateConfig()
	require.Error(t, err)
	assert.Contains(t, err.Error(), `providers.anthropic.type unsupported provider type "openai_compatible"`)
}

func TestValidateConfig_AcceptsProviderTypeAliases(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("HOME", tempDir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tempDir, "config"))
	t.Setenv("CODEX_HOME", filepath.Join(tempDir, "missing-codex"))
	t.Setenv("OPENCODE_CONFIG", "")
	t.Setenv("OPENCODE_CONFIG_DIR", "")
	t.Setenv("FORGE_CONFIG", filepath.Join(tempDir, "missing-forge"))
	t.Setenv("ATTELER_CONFIG", "")
	t.Chdir(tempDir)

	configDir := filepath.Join(tempDir, ".atteler")
	require.NoError(t, os.MkdirAll(configDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(`providers:
  gemini:
    type: google_ai_studio
    base_url: https://generativelanguage.googleapis.com/v1beta/openai
    models: [gemini-test]
  vllm:
    base_url: http://127.0.0.1:8000
    models: [qwen-test]
  anthropic:
    type: anthropic
`), 0o600))

	require.NoError(t, validateConfig())
}

func TestValidateHookConfig_AcceptsKnownPayloadModes(t *testing.T) {
	t.Parallel()

	hooks := map[string][]appconfig.HookConfig{
		events.UserMessage: {
			{Payload: ""},
			{Payload: "metadata"},
			{Payload: " summary "},
			{Payload: "FULL"},
		},
	}

	require.NoError(t, validateHookConfig(hooks))
}

func TestValidateHookConfig_RejectsUnknownPayloadWithoutLeakingHookConfig(t *testing.T) {
	t.Parallel()

	hooks := map[string][]appconfig.HookConfig{
		events.UserMessage: {{
			Command: []string{"/tmp/secret-hook", "--api-key=sk-test-secret"},
			Env: map[string]string{
				"API_TOKEN": "secret-env-value",
			},
			Payload: "sk-payloadsecret1234567890",
		}},
	}

	err := validateHookConfig(hooks)
	require.Error(t, err)

	message := err.Error()
	assert.Contains(t, message, "hooks.user_message[0].payload")
	assert.Contains(t, message, "unknown payload mode")
	assert.NotContains(t, message, "secret-hook")
	assert.NotContains(t, message, "--api-key")
	assert.NotContains(t, message, "API_TOKEN")
	assert.NotContains(t, message, "secret-env-value")
	assert.NotContains(t, message, "sk-payloadsecret")
}

func TestValidateHookConfig_RedactsUnknownEventNameInError(t *testing.T) {
	t.Parallel()

	hooks := map[string][]appconfig.HookConfig{
		"sk-eventsecret1234567890": {{
			Payload: "sk-payloadsecret1234567890",
		}},
	}

	err := validateHookConfig(hooks)
	require.Error(t, err)

	message := err.Error()
	assert.Contains(t, message, "hooks.event[0].payload")
	assert.NotContains(t, message, "sk-eventsecret")
	assert.NotContains(t, message, "sk-payloadsecret")
}

func TestValidateConfig_RejectsInvalidHookPayload(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(`
hooks:
  user_message:
    - command: ["/tmp/secret-hook", "--token=sk-test-secret"]
      env:
        API_TOKEN: secret-env-value
      payload: sk-payloadsecret1234567890
`), 0o600))

	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tempDir, "xdg"))
	t.Setenv(appconfig.EnvPath, configPath)
	t.Chdir(tempDir)

	err := validateConfig()
	require.Error(t, err)

	message := err.Error()
	assert.Contains(t, message, "validate config:")
	assert.Contains(t, message, "hooks.user_message[0].payload")
	assert.NotContains(t, message, "secret-hook")
	assert.NotContains(t, message, "--token")
	assert.NotContains(t, message, "API_TOKEN")
	assert.NotContains(t, message, "secret-env-value")
	assert.NotContains(t, message, "sk-payloadsecret")
}
