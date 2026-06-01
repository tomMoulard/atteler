package config

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRedactedConfig_ReturnsIndependentCopy(t *testing.T) {
	t.Parallel()

	const changedModel = "openai/changed"

	temperature := 0.2
	seed := 7
	maxOutputBytes := int64(4096)
	maxCostMicros := int64(25_000)
	maxInputTokens := 100
	maxOutputTokens := 50
	maxTotalTokens := 150
	maxIterations := 3
	maxModelCalls := 4
	maxToolCalls := 5
	maxWallTime := "1m"
	checkpointInterval := 2
	enabled := true
	workspaceEnabled := true

	cfg := Config{
		Generation: GenerationConfig{
			Temperature: &temperature,
		},
		AgentLoop: AgentLoopConfig{
			MaxOutputBytes:     &maxOutputBytes,
			MaxCostMicros:      &maxCostMicros,
			MaxInputTokens:     &maxInputTokens,
			MaxOutputTokens:    &maxOutputTokens,
			MaxTotalTokens:     &maxTotalTokens,
			MaxIterations:      &maxIterations,
			MaxModelCalls:      &maxModelCalls,
			MaxToolCalls:       &maxToolCalls,
			MaxWallTime:        &maxWallTime,
			CheckpointInterval: &checkpointInterval,
		},
		Providers: map[string]ProviderConfig{
			"compatible": {
				BaseURL:             "https://user:" + "pass" + "@example.com/openai?api_key=secret-token",
				ChatCompletionsPath: "/v1/chat/completions?api_key=secret-token",
				EmbeddingsPath:      "/v1/embeddings?auth_token=secret-token",
				ModelsPath:          "/v1/models?access_token=secret-token",
				Models:              []string{"compatible/secret-model"},
				Capabilities:        []string{"chat"},
			},
		},
		Agents: map[string]AgentConfig{
			"reviewer": {
				Seed:             &seed,
				SystemPrompt:     "private prompt",
				RoutingPolicy:    RoutingPolicyConfig{PreferredProviders: []string{"openai"}},
				FallbackModels:   []string{"fallback-a"},
				ToolPermissions:  map[string]bool{"read": true},
				FeedbackGuidance: []FeedbackGuidance{{ID: "private-feedback"}},
			},
		},
		ModelAliases: map[string]string{"fast": "openai/gpt-4.1-mini"},
		ModelRoles: map[string]ModelRoleConfig{
			"planner": {
				FallbackModels:       []string{"openai/gpt-4.1-mini"},
				RoutingPolicy:        RoutingPolicyConfig{RequiredCapabilities: []string{"tools"}},
				PreferredProviders:   []string{"openai"},
				BannedProviders:      []string{"anthropic"},
				BannedModels:         []string{"openai/gpt-4.1-nano"},
				RequiredCapabilities: []string{"json_schema"},
			},
		},
		Hooks: map[string][]HookConfig{
			"session_end": {{
				Command: []string{"echo", "done"},
				Env:     map[string]string{"SAFE": "value"},
			}},
		},
		Context: ContextConfig{
			ReferencePolicy: ReferencePolicyConfig{
				AllowedSchemes:   []string{"https"},
				DeniedSchemes:    []string{"http"},
				AllowedHosts:     []string{"docs.example.com"},
				DeniedHosts:      []string{"blocked.example.com"},
				AllowedPorts:     []int{443},
				DeniedPorts:      []int{81},
				LocalRoots:       []string{"../shared"},
				DeniedLocalRoots: []string{"../shared/secrets"},
				AllowedGlobs:     []string{"docs/**/*.md"},
				DeniedGlobs:      []string{"**/*.pem"},
				ContentTypes:     []string{"text/*"},
			},
		},
		SkillLearning: SkillLearningConfig{Enabled: &enabled},
		Vector: VectorConfig{
			WorkspaceEnabled: &workspaceEnabled,
			Model:            "tenant-embed?api_" + "key=secret-token/v1",
			BaseURL:          "https://user:" + "pass" + "@example.com/embed",
			Stores: map[string]VectorizerConfig{
				"agent-memory": {
					Model:     "store-api_key=secret-token",
					BaseURL:   "https://user:" + "pass" + "@example.com/store-embed",
					IndexPath: "tmp/auth_token=secret-token/agent-memory.json",
				},
			},
			Agents: map[string]VectorizerConfig{
				"reviewer": {
					BaseURL: "https://user:" + "pass" + "@example.com/agent-embed",
				},
			},
			Sources: map[string]VectorizerConfig{
				"git_history": {
					IndexPath: "git/auth_token=secret-token/history.json",
				},
			},
			WorkspaceInclude: []string{"*.go", "docs/api_key=secret-token/*.md"},
			WorkspaceExclude: []string{"vendor/", "tmp/auth_token=secret-token/"},
		},
	}

	redacted := RedactedConfig(cfg)
	storeModelBeforeMutation := redacted.Vector.Stores["agent-memory"].Model
	*redacted.Generation.Temperature = 0.9
	*redacted.AgentLoop.MaxOutputBytes = 8192
	*redacted.AgentLoop.MaxCostMicros = 50_000
	*redacted.AgentLoop.MaxInputTokens = 999
	*redacted.AgentLoop.MaxOutputTokens = 888
	*redacted.AgentLoop.MaxTotalTokens = 777
	*redacted.AgentLoop.MaxIterations = 99
	*redacted.AgentLoop.MaxModelCalls = 66
	*redacted.AgentLoop.MaxToolCalls = 55
	*redacted.AgentLoop.MaxWallTime = "2m"
	*redacted.AgentLoop.CheckpointInterval = 44
	redactedProvider := redacted.Providers["compatible"]
	redactedProvider.Models[0] = changedModel
	redactedProvider.Capabilities[0] = "embeddings"
	redacted.Providers["compatible"] = redactedProvider
	*redacted.Agents["reviewer"].Seed = 42
	redacted.Agents["reviewer"].RoutingPolicy.PreferredProviders[0] = "anthropic"
	redacted.Agents["reviewer"].FallbackModels[0] = "fallback-b"
	redacted.ModelAliases["fast"] = changedModel
	redacted.ModelRoles["planner"].FallbackModels[0] = changedModel
	redacted.ModelRoles["planner"].RoutingPolicy.RequiredCapabilities[0] = "vision"
	redacted.ModelRoles["planner"].PreferredProviders[0] = "codex"
	redacted.ModelRoles["planner"].BannedProviders[0] = "ollama"
	redacted.ModelRoles["planner"].BannedModels[0] = changedModel
	redacted.ModelRoles["planner"].RequiredCapabilities[0] = "tools"
	redacted.Agents["reviewer"].ToolPermissions["read"] = false
	redacted.Hooks["session_end"][0].Command[0] = "printf"
	redacted.Hooks["session_end"][0].Env["SAFE"] = "changed"
	redacted.Context.ReferencePolicy.AllowedSchemes[0] = "http"
	redacted.Context.ReferencePolicy.DeniedSchemes[0] = "ftp"
	redacted.Context.ReferencePolicy.AllowedHosts[0] = "changed.example.com"
	redacted.Context.ReferencePolicy.DeniedHosts[0] = "changed-block.example.com"
	redacted.Context.ReferencePolicy.AllowedPorts[0] = 8443
	redacted.Context.ReferencePolicy.DeniedPorts[0] = 82
	redacted.Context.ReferencePolicy.LocalRoots[0] = "../changed"
	redacted.Context.ReferencePolicy.DeniedLocalRoots[0] = "../changed/secrets"
	redacted.Context.ReferencePolicy.AllowedGlobs[0] = "changed/**/*.md"
	redacted.Context.ReferencePolicy.DeniedGlobs[0] = "**/*.key"
	redacted.Context.ReferencePolicy.ContentTypes[0] = "application/json"
	*redacted.SkillLearning.Enabled = false
	*redacted.Vector.WorkspaceEnabled = false
	storeVector := redacted.Vector.Stores["agent-memory"]
	storeVector.Model = "changed"
	redacted.Vector.Stores["agent-memory"] = storeVector
	redacted.Vector.WorkspaceInclude[0] = "*.md"
	redacted.Vector.WorkspaceExclude[0] = "node_modules/"

	assert.InDelta(t, 0.2, *cfg.Generation.Temperature, 0)
	assert.EqualValues(t, 4096, *cfg.AgentLoop.MaxOutputBytes)
	assert.EqualValues(t, 25_000, *cfg.AgentLoop.MaxCostMicros)
	assert.Equal(t, 100, *cfg.AgentLoop.MaxInputTokens)
	assert.Equal(t, 50, *cfg.AgentLoop.MaxOutputTokens)
	assert.Equal(t, 150, *cfg.AgentLoop.MaxTotalTokens)
	assert.Equal(t, 3, *cfg.AgentLoop.MaxIterations)
	assert.Equal(t, 4, *cfg.AgentLoop.MaxModelCalls)
	assert.Equal(t, 5, *cfg.AgentLoop.MaxToolCalls)
	assert.Equal(t, "1m", *cfg.AgentLoop.MaxWallTime)
	assert.Equal(t, 2, *cfg.AgentLoop.CheckpointInterval)
	assert.Equal(t, []string{"compatible/secret-model"}, cfg.Providers["compatible"].Models)
	assert.Equal(t, []string{"chat"}, cfg.Providers["compatible"].Capabilities)
	assert.Equal(t, 7, *cfg.Agents["reviewer"].Seed)
	assert.Equal(t, []string{"openai"}, cfg.Agents["reviewer"].RoutingPolicy.PreferredProviders)
	assert.Equal(t, []string{"fallback-a"}, cfg.Agents["reviewer"].FallbackModels)
	assert.Equal(t, "openai/gpt-4.1-mini", cfg.ModelAliases["fast"])
	assert.Equal(t, []string{"openai/gpt-4.1-mini"}, cfg.ModelRoles["planner"].FallbackModels)
	assert.Equal(t, []string{"tools"}, cfg.ModelRoles["planner"].RoutingPolicy.RequiredCapabilities)
	assert.Equal(t, []string{"openai"}, cfg.ModelRoles["planner"].PreferredProviders)
	assert.Equal(t, []string{"anthropic"}, cfg.ModelRoles["planner"].BannedProviders)
	assert.Equal(t, []string{"openai/gpt-4.1-nano"}, cfg.ModelRoles["planner"].BannedModels)
	assert.Equal(t, []string{"json_schema"}, cfg.ModelRoles["planner"].RequiredCapabilities)
	assert.True(t, cfg.Agents["reviewer"].ToolPermissions["read"])
	assert.Equal(t, []string{"echo", "done"}, cfg.Hooks["session_end"][0].Command)
	assert.Equal(t, "value", cfg.Hooks["session_end"][0].Env["SAFE"])
	assert.Equal(t, []string{"https"}, cfg.Context.ReferencePolicy.AllowedSchemes)
	assert.Equal(t, []string{"http"}, cfg.Context.ReferencePolicy.DeniedSchemes)
	assert.Equal(t, []string{"docs.example.com"}, cfg.Context.ReferencePolicy.AllowedHosts)
	assert.Equal(t, []string{"blocked.example.com"}, cfg.Context.ReferencePolicy.DeniedHosts)
	assert.Equal(t, []int{443}, cfg.Context.ReferencePolicy.AllowedPorts)
	assert.Equal(t, []int{81}, cfg.Context.ReferencePolicy.DeniedPorts)
	assert.Equal(t, []string{"../shared"}, cfg.Context.ReferencePolicy.LocalRoots)
	assert.Equal(t, []string{"../shared/secrets"}, cfg.Context.ReferencePolicy.DeniedLocalRoots)
	assert.Equal(t, []string{"docs/**/*.md"}, cfg.Context.ReferencePolicy.AllowedGlobs)
	assert.Equal(t, []string{"**/*.pem"}, cfg.Context.ReferencePolicy.DeniedGlobs)
	assert.Equal(t, []string{"text/*"}, cfg.Context.ReferencePolicy.ContentTypes)
	assert.True(t, *cfg.SkillLearning.Enabled)
	assert.True(t, *cfg.Vector.WorkspaceEnabled)
	assert.Equal(t, "store-api_key=secret-token", cfg.Vector.Stores["agent-memory"].Model)
	assert.Equal(t, []string{"*.go", "docs/api_key=secret-token/*.md"}, cfg.Vector.WorkspaceInclude)
	assert.Equal(t, []string{"vendor/", "tmp/auth_token=secret-token/"}, cfg.Vector.WorkspaceExclude)
	assert.NotContains(t, redacted.Providers["compatible"].BaseURL, "pass")
	assert.NotContains(t, redacted.Providers["compatible"].BaseURL, "secret-token")
	assert.NotContains(t, redacted.Providers["compatible"].ChatCompletionsPath, "secret-token")
	assert.NotContains(t, redacted.Providers["compatible"].EmbeddingsPath, "secret-token")
	assert.NotContains(t, redacted.Providers["compatible"].ModelsPath, "secret-token")
	assert.Contains(t, redacted.Providers["compatible"].ChatCompletionsPath, "redacted")
	assert.Contains(t, redacted.Providers["compatible"].EmbeddingsPath, "redacted")
	assert.Contains(t, redacted.Providers["compatible"].ModelsPath, "redacted")
	assert.NotContains(t, redacted.Vector.BaseURL, "pass")
	assert.NotContains(t, redacted.Vector.Model, "secret-token")
	assert.NotContains(t, redacted.Vector.Stores["agent-memory"].BaseURL, "pass")
	assert.NotContains(t, storeModelBeforeMutation, "secret-token")
	assert.NotContains(t, redacted.Vector.Stores["agent-memory"].IndexPath, "secret-token")
	assert.NotContains(t, redacted.Vector.Agents["reviewer"].BaseURL, "pass")
	assert.NotContains(t, redacted.Vector.Sources["git_history"].IndexPath, "secret-token")
	assert.NotContains(t, redacted.Vector.WorkspaceInclude[1], "secret-token")
	assert.NotContains(t, redacted.Vector.WorkspaceExclude[1], "secret-token")
	assert.Equal(t, RedactedValue, redacted.Agents["reviewer"].SystemPrompt)
	assert.Empty(t, redacted.Agents["reviewer"].FeedbackGuidance)
}

func TestRedactedOriginMap_RedactsWorkspaceVectorPatternLists(t *testing.T) {
	t.Parallel()

	origins := OriginMap{
		"vector.workspace_include": {Chain: []OriginEvent{{
			Kind:      OriginProjectFile,
			Operation: OriginSet,
			Source:    "atteler.yaml",
			Value:     `["*.go","docs/api_key=secret-token/*.md"]`,
		}}},
		"vector.workspace_exclude": {Chain: []OriginEvent{{
			Kind:      OriginProjectFile,
			Operation: OriginSet,
			Source:    "atteler.yaml",
			Value:     `["vendor/","tmp/auth_token=secret-token/"]`,
		}}},
	}

	redacted := RedactedOriginMap(origins)

	includeValue := redacted["vector.workspace_include"].Chain[0].Value
	excludeValue := redacted["vector.workspace_exclude"].Chain[0].Value

	assert.NotContains(t, includeValue, "secret-token")
	assert.NotContains(t, excludeValue, "secret-token")

	var includePatterns []string
	require.NoError(t, json.Unmarshal([]byte(includeValue), &includePatterns))
	require.Len(t, includePatterns, 2)
	assert.Equal(t, "*.go", includePatterns[0])
	assert.NotContains(t, includePatterns[1], "secret-token")

	var excludePatterns []string
	require.NoError(t, json.Unmarshal([]byte(excludeValue), &excludePatterns))
	require.Len(t, excludePatterns, 2)
	assert.Equal(t, "vendor/", excludePatterns[0])
	assert.NotContains(t, excludePatterns[1], "secret-token")
}
