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

	attelerplugin "github.com/tommoulard/atteler/pkg/plugin"
)

const (
	testVectorAgentMemoryStore = "agent-memory"
	testVectorReviewerAgent    = "reviewer"
)

func TestLoadFiles_MergesInOrder(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	global := writeConfig(t, dir, "global.yaml", `
default_provider: anthropic
default_model: claude-default
fallback_models: [claude-fallback]
model_aliases:
  fast: openai/gpt-global
  safe: anthropic/claude-global
models:
  planner:
    preferred: anthropic/claude-global
    fallback: openai/gpt-global
    required_capabilities: [tools]
providers:
  anthropic:
    base_url: https://anthropic.global
  openai:
    disabled: true
    base_url: https://openai.global
  codex:
    disable_private_adapter: true
`)
	local := writeConfig(t, dir, "local.yml", `
default_model: gpt-local
event_ledger_path: ./.atteler/events.jsonl
fallback_models: [gpt-backup]
model_aliases:
  fast: openai/gpt-local
  review: codex/gpt-5.5
models:
  planner:
    preferred: openai/gpt-local
    fallback_models: [openai/gpt-backup]
    preferred_providers: [openai]
    max_cost_usd: 0.25
    max_latency_ms: 2500
    max_ttft_ms: 900
    prefer_local: true
providers:
  openai:
    disabled: false
  vllm:
    type: openai_compatible
    base_url: http://127.0.0.1:8000
    local: true
    api_key_env: VLLM_API_KEY
    api_key_header: Authorization
    api_key_scheme: Bearer
    chat_completions_path: /v1/chat/completions
    models_path: /v1/models
    api_version: preview
    models: [qwen2.5-coder]
    capabilities: [chat, tools, json_schema, local]
agents:
  reviewer:
    description: Reviews code changes
    personality: concise
    system_prompt: review code
    fallback_models: [gpt-review-backup]
    capabilities: [review, security]
    temperature: 0.2
    seed: 42
    model_mode: fast
    reasoning_level: high
    triggers: ["review this", "code review"]
hooks:
  assistant_message:
    - command: [logger, --assistant]
      timeout_seconds: 3
      max_attempts: 4
      retry_backoff_millis: 25
      blocking: true
      env:
        EXTRA: "1"
context:
  max_file_bytes: 123
  max_total_bytes: 456
  max_input_tokens: 789
generation:
  temperature: 0
  top_p: 0.8
  seed: 7
  model_mode: fast
  reasoning_level: medium
  max_tokens: 900
agent_loop:
  max_output_bytes: 0
  max_cost_micros: 0
  max_input_tokens: 0
  max_output_tokens: 0
  max_total_tokens: 0
  max_iterations: 0
  max_model_calls: 0
  max_tool_calls: 0
  max_wall_time: "0"
  checkpoint_interval: 0
plugins:
  paths:
    - ./plugin-a
skill_learning:
  enabled: false
  store_dir: ./.atteler/learn
  skill_dir: ./.atteler/skills/generated
  max_observations: 42
  max_steps: 4
  min_occurrences: 3
vector:
  workspace_enabled: true
  workspace_allow_remote_embeddings: false
  vectorizer: embedding
  provider: ollama
  model: nomic-embed-text
  base_url: http://127.0.0.1:11434
  timeout_seconds: 12
  fallback_policy: lexical
  index_path: ./.atteler/test-vector-index.json
  workspace_index_path: ./.atteler/workspace-vector-index.json
  workspace_include: ["*.go", "*.md"]
  workspace_exclude: ["vendor/", "*.gen.go"]
  chunk_max_runes: 900
  chunk_overlap_runes: 90
  workspace_limit: 3
  workspace_max_file_bytes: 12345
  workspace_max_files: 321
worktree:
  auto_merge: true
  verification_commands:
    - go test ./...
  override_verification: false
`)

	cfg, loaded, err := LoadFiles([]string{global, filepath.Join(dir, "missing.json"), local})
	if err != nil {
		require.NoError(t, err)
	}

	if !reflect.DeepEqual(loaded, []string{global, local}) {
		require.Failf(t, "unexpected failure", "loaded = %v, want [%s %s]", loaded, global, local)
	}

	if cfg.DefaultProvider != "anthropic" {
		assert.Failf(t, "assertion failed", "DefaultProvider = %q, want anthropic", cfg.DefaultProvider)
	}

	if cfg.DefaultModel != "gpt-local" {
		assert.Failf(t, "assertion failed", "DefaultModel = %q, want gpt-local", cfg.DefaultModel)
	}

	if !reflect.DeepEqual(cfg.FallbackModels, []string{"gpt-backup"}) {
		assert.Failf(t, "assertion failed", "FallbackModels = %v", cfg.FallbackModels)
	}

	assert.Equal(t, map[string]string{
		"fast":   "openai/gpt-local",
		"review": "codex/gpt-5.5",
		"safe":   "anthropic/claude-global",
	}, cfg.ModelAliases)

	planner := cfg.ModelRoles["planner"]
	assert.Equal(t, "openai/gpt-local", planner.Preferred)
	assert.Equal(t, []string{"openai/gpt-backup"}, planner.FallbackModels)
	assert.Equal(t, []string{"tools"}, planner.RequiredCapabilities)
	assert.Equal(t, []string{"openai"}, planner.PreferredProviders)
	assert.InDelta(t, 0.25, planner.MaxCostUSD, 0.000000001)
	assert.Equal(t, 2500, planner.MaxLatencyMS)
	assert.Equal(t, 900, planner.MaxTTFTMS)
	assert.True(t, planner.PreferLocal)

	openai := cfg.Providers["openai"]
	if openai.Disabled {
		assert.Fail(t, "openai disabled should be overridden to false")
	}

	if openai.BaseURL != "https://openai.global" {
		assert.Failf(t, "assertion failed", "openai base_url = %q", openai.BaseURL)
	}

	anthropic := cfg.Providers["anthropic"]
	if anthropic.BaseURL != "https://anthropic.global" {
		assert.Failf(t, "assertion failed", "anthropic base_url = %q", anthropic.BaseURL)
	}

	assert.True(t, cfg.Providers["codex"].DisablePrivateAdapter)

	vllm := cfg.Providers["vllm"]
	assert.Equal(t, "openai_compatible", vllm.Type)
	assert.Equal(t, "http://127.0.0.1:8000", vllm.BaseURL)
	assert.True(t, vllm.Local)
	assert.Equal(t, "VLLM_API_KEY", vllm.APIKeyEnv)
	assert.Equal(t, "Authorization", vllm.APIKeyHeader)
	assert.Equal(t, "Bearer", vllm.APIKeyScheme)
	assert.Equal(t, "/v1/chat/completions", vllm.ChatCompletionsPath)
	assert.Equal(t, "/v1/models", vllm.ModelsPath)
	assert.Equal(t, "preview", vllm.APIVersion)
	assert.Equal(t, []string{"qwen2.5-coder"}, vllm.Models)
	assert.Equal(t, []string{"chat", "tools", "json_schema", "local"}, vllm.Capabilities)

	reviewer := cfg.Agents["reviewer"]
	if reviewer.SystemPrompt != "review code" {
		assert.Failf(t, "assertion failed", "reviewer system_prompt = %q", reviewer.SystemPrompt)
	}

	if reviewer.Description != "Reviews code changes" {
		assert.Failf(t, "assertion failed", "reviewer description = %q", reviewer.Description)
	}

	if reviewer.Personality != "concise" {
		assert.Failf(t, "assertion failed", "reviewer personality = %q", reviewer.Personality)
	}

	if !reflect.DeepEqual(reviewer.FallbackModels, []string{"gpt-review-backup"}) {
		assert.Failf(t, "assertion failed", "reviewer fallback_models = %v", reviewer.FallbackModels)
	}

	if !reflect.DeepEqual(reviewer.Capabilities, []string{"review", "security"}) {
		assert.Failf(t, "assertion failed", "reviewer capabilities = %v", reviewer.Capabilities)
	}

	if reviewer.Temperature == nil || *reviewer.Temperature != 0.2 {
		assert.Failf(t, "assertion failed", "reviewer temperature = %v", reviewer.Temperature)
	}

	if reviewer.Seed == nil || *reviewer.Seed != 42 {
		assert.Failf(t, "assertion failed", "reviewer seed = %v", reviewer.Seed)
	}

	if reviewer.ReasoningLevel != "high" {
		assert.Failf(t, "assertion failed", "reviewer reasoning_level = %q", reviewer.ReasoningLevel)
	}

	assert.Equal(t, "fast", reviewer.ModelMode)

	if !reflect.DeepEqual(reviewer.Triggers, []string{"review this", "code review"}) {
		assert.Failf(t, "assertion failed", "reviewer triggers = %v", reviewer.Triggers)
	}

	hooks := cfg.Hooks["assistant_message"]
	if len(hooks) != 1 {
		require.Failf(t, "unexpected failure", "assistant hooks len = %d, want 1", len(hooks))
	}

	if !reflect.DeepEqual(hooks[0].Command, []string{"logger", "--assistant"}) {
		assert.Failf(t, "assertion failed", "hook command = %v", hooks[0].Command)
	}

	if hooks[0].TimeoutSeconds != 3 {
		assert.Failf(t, "assertion failed", "hook timeout = %d", hooks[0].TimeoutSeconds)
	}

	assert.Equal(t, 4, hooks[0].MaxAttempts)
	assert.Equal(t, 25, hooks[0].RetryBackoffMillis)
	assert.True(t, hooks[0].Blocking)

	if hooks[0].Env["EXTRA"] != "1" {
		assert.Failf(t, "assertion failed", "hook env EXTRA = %q", hooks[0].Env["EXTRA"])
	}

	assert.Equal(t, "./.atteler/events.jsonl", cfg.EventLedgerPath)

	if cfg.Context.MaxFileBytes != 123 {
		assert.Failf(t, "assertion failed", "MaxFileBytes = %d, want 123", cfg.Context.MaxFileBytes)
	}

	if cfg.Context.MaxTotalBytes != 456 {
		assert.Failf(t, "assertion failed", "MaxTotalBytes = %d, want 456", cfg.Context.MaxTotalBytes)
	}

	if cfg.Context.MaxInputTokens != 789 {
		assert.Failf(t, "assertion failed", "MaxInputTokens = %d, want 789", cfg.Context.MaxInputTokens)
	}

	if cfg.Generation.Temperature == nil || *cfg.Generation.Temperature != 0 {
		assert.Failf(t, "assertion failed", "generation temperature = %v", cfg.Generation.Temperature)
	}

	if cfg.Generation.TopP == nil || *cfg.Generation.TopP != 0.8 {
		assert.Failf(t, "assertion failed", "generation top_p = %v", cfg.Generation.TopP)
	}

	if cfg.Generation.Seed == nil || *cfg.Generation.Seed != 7 {
		assert.Failf(t, "assertion failed", "generation seed = %v", cfg.Generation.Seed)
	}

	if cfg.Generation.ReasoningLevel != "medium" {
		assert.Failf(t, "assertion failed", "generation reasoning_level = %q", cfg.Generation.ReasoningLevel)
	}

	assert.Equal(t, "fast", cfg.Generation.ModelMode)

	if cfg.Generation.MaxTokens != 900 {
		assert.Failf(t, "assertion failed", "generation max_tokens = %d, want 900", cfg.Generation.MaxTokens)
	}

	if cfg.AgentLoop.MaxOutputBytes == nil || *cfg.AgentLoop.MaxOutputBytes != 0 {
		assert.Failf(t, "assertion failed", "agent_loop.max_output_bytes = %v, want 0", cfg.AgentLoop.MaxOutputBytes)
	}

	if cfg.AgentLoop.MaxCostMicros == nil || *cfg.AgentLoop.MaxCostMicros != 0 {
		assert.Failf(t, "assertion failed", "agent_loop.max_cost_micros = %v, want 0", cfg.AgentLoop.MaxCostMicros)
	}

	if cfg.AgentLoop.MaxInputTokens == nil || *cfg.AgentLoop.MaxInputTokens != 0 {
		assert.Failf(t, "assertion failed", "agent_loop.max_input_tokens = %v, want 0", cfg.AgentLoop.MaxInputTokens)
	}

	if cfg.AgentLoop.MaxOutputTokens == nil || *cfg.AgentLoop.MaxOutputTokens != 0 {
		assert.Failf(t, "assertion failed", "agent_loop.max_output_tokens = %v, want 0", cfg.AgentLoop.MaxOutputTokens)
	}

	if cfg.AgentLoop.MaxTotalTokens == nil || *cfg.AgentLoop.MaxTotalTokens != 0 {
		assert.Failf(t, "assertion failed", "agent_loop.max_total_tokens = %v, want 0", cfg.AgentLoop.MaxTotalTokens)
	}

	if cfg.AgentLoop.MaxIterations == nil || *cfg.AgentLoop.MaxIterations != 0 {
		assert.Failf(t, "assertion failed", "agent_loop.max_iterations = %v, want 0", cfg.AgentLoop.MaxIterations)
	}

	if cfg.AgentLoop.MaxModelCalls == nil || *cfg.AgentLoop.MaxModelCalls != 0 {
		assert.Failf(t, "assertion failed", "agent_loop.max_model_calls = %v, want 0", cfg.AgentLoop.MaxModelCalls)
	}

	if cfg.AgentLoop.MaxToolCalls == nil || *cfg.AgentLoop.MaxToolCalls != 0 {
		assert.Failf(t, "assertion failed", "agent_loop.max_tool_calls = %v, want 0", cfg.AgentLoop.MaxToolCalls)
	}

	if cfg.AgentLoop.MaxWallTime == nil || *cfg.AgentLoop.MaxWallTime != "0" {
		assert.Failf(t, "assertion failed", "agent_loop.max_wall_time = %v, want 0", cfg.AgentLoop.MaxWallTime)
	}

	if cfg.AgentLoop.CheckpointInterval == nil || *cfg.AgentLoop.CheckpointInterval != 0 {
		assert.Failf(t, "assertion failed", "agent_loop.checkpoint_interval = %v, want 0", cfg.AgentLoop.CheckpointInterval)
	}

	if !reflect.DeepEqual(cfg.Plugins.Paths, []string{"./plugin-a"}) {
		assert.Failf(t, "assertion failed", "plugin paths = %v", cfg.Plugins.Paths)
	}

	if cfg.SkillLearning.Enabled == nil || *cfg.SkillLearning.Enabled {
		assert.Failf(t, "assertion failed", "skill_learning.enabled = %v, want false", cfg.SkillLearning.Enabled)
	}

	assert.Equal(t, "./.atteler/learn", cfg.SkillLearning.StoreDir)
	assert.Equal(t, "./.atteler/skills/generated", cfg.SkillLearning.SkillDir)
	assert.Equal(t, 42, cfg.SkillLearning.MaxObservations)
	assert.Equal(t, 4, cfg.SkillLearning.MaxSteps)
	assert.Equal(t, 3, cfg.SkillLearning.MinOccurrences)
	require.NotNil(t, cfg.Vector.WorkspaceEnabled)
	assert.True(t, *cfg.Vector.WorkspaceEnabled)
	require.NotNil(t, cfg.Vector.WorkspaceAllowRemoteEmbeddings)
	assert.False(t, *cfg.Vector.WorkspaceAllowRemoteEmbeddings)
	assert.Equal(t, "embedding", cfg.Vector.Vectorizer)
	assert.Equal(t, "ollama", cfg.Vector.Provider)
	assert.Equal(t, "nomic-embed-text", cfg.Vector.Model)
	assert.Equal(t, "http://127.0.0.1:11434", cfg.Vector.BaseURL)
	assert.Equal(t, 12, cfg.Vector.TimeoutSeconds)
	assert.Equal(t, "lexical", cfg.Vector.FallbackPolicy)
	assert.Equal(t, "./.atteler/test-vector-index.json", cfg.Vector.IndexPath)
	assert.Equal(t, "./.atteler/workspace-vector-index.json", cfg.Vector.WorkspaceIndexPath)
	assert.Equal(t, []string{"*.go", "*.md"}, cfg.Vector.WorkspaceInclude)
	assert.Equal(t, []string{"vendor/", "*.gen.go"}, cfg.Vector.WorkspaceExclude)
	assert.Equal(t, 900, cfg.Vector.ChunkMaxRunes)
	assert.Equal(t, 90, cfg.Vector.ChunkOverlapRunes)
	assert.Equal(t, 3, cfg.Vector.WorkspaceLimit)
	assert.Equal(t, 12345, cfg.Vector.WorkspaceMaxFileBytes)
	assert.Equal(t, 321, cfg.Vector.WorkspaceMaxFiles)
	require.NotNil(t, cfg.Worktree.AutoMerge)
	assert.True(t, *cfg.Worktree.AutoMerge)
	assert.Equal(t, []string{"go test ./..."}, cfg.Worktree.VerificationCommands)
	assert.False(t, cfg.Worktree.OverrideVerification)
}

func TestMergeConfigFromSource_ModelRolesAndProviderLocal(t *testing.T) {
	t.Parallel()

	origins := OriginMap{}
	cfg := Config{}
	mergeConfigFromSource(&cfg, Config{
		ModelRoles: map[string]ModelRoleConfig{
			"planner": {
				Preferred:            "openai/gpt-4.1",
				FallbackModels:       []string{"openai/gpt-4.1-mini"},
				RequiredCapabilities: []string{"tools", "json_schema"},
				MaxCostUSD:           0.25,
				MaxLatencyMS:         2500,
				MaxTTFTMS:            900,
				PreferLocal:          true,
			},
		},
		Providers: map[string]ProviderConfig{
			"vllm": {
				Type:    "openai_compatible",
				BaseURL: "https://vllm.example",
				Local:   true,
			},
		},
	}, newOriginRecorder(origins), originSource{
		kind:   OriginHarnessImport,
		source: "test",
	})

	planner := cfg.ModelRoles["planner"]
	assert.Equal(t, "openai/gpt-4.1", planner.Preferred)
	assert.Equal(t, []string{"openai/gpt-4.1-mini"}, planner.FallbackModels)
	assert.Equal(t, []string{"tools", "json_schema"}, planner.RequiredCapabilities)
	assert.InDelta(t, 0.25, planner.MaxCostUSD, 0.000000001)
	assert.Equal(t, 2500, planner.MaxLatencyMS)
	assert.Equal(t, 900, planner.MaxTTFTMS)
	assert.True(t, planner.PreferLocal)
	assert.True(t, cfg.Providers["vllm"].Local)

	roleOrigin, ok := origins.Final("models.planner.prefer_local")
	require.True(t, ok)
	assert.Equal(t, OriginSet, roleOrigin.Operation)
	assert.Equal(t, "true", roleOrigin.Value)

	origin, ok := origins.Final("providers.vllm.local")
	require.True(t, ok)
	assert.Equal(t, OriginSet, origin.Operation)
	assert.Equal(t, "true", origin.Value)
}

func TestLoadFiles_ModelRoleFallbackAliases(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := writeConfig(t, dir, "config.yaml", `
models:
  planner:
    preferred: openai/gpt-4.1
    fallback: openai/gpt-4.1-mini
  fast_coder:
    preferred: openai/gpt-4.1-mini
    fallbacks: [ollama/llama3.2]
`)

	cfg, _, origins, err := LoadFilesWithOrigins([]string{path})
	require.NoError(t, err)

	assert.Equal(t, []string{"openai/gpt-4.1-mini"}, cfg.ModelRoles["planner"].FallbackModels)
	assert.Equal(t, []string{"ollama/llama3.2"}, cfg.ModelRoles["fast_coder"].FallbackModels)

	origin, ok := origins.Final("models.planner.fallback")
	require.True(t, ok)
	assert.Equal(t, OriginSet, origin.Operation)
	assert.Equal(t, `["openai/gpt-4.1-mini"]`, origin.Value)
}

func TestMergeConfigModelRolesFromOrigins_CanClearScalars(t *testing.T) {
	t.Parallel()

	dst := Config{
		ModelRoles: map[string]ModelRoleConfig{
			"planner": {
				Preferred:            "openai/gpt-4.1",
				FallbackModels:       []string{"openai/gpt-4.1-mini"},
				RoutingPolicy:        RoutingPolicyConfig{RequiredCapabilities: []string{"tools"}},
				PreferredProviders:   []string{"openai"},
				BannedProviders:      []string{"anthropic"},
				BannedModels:         []string{"openai/gpt-4.1-nano"},
				RequiredCapabilities: []string{"tools"},
				MaxCostUSD:           1.25,
				MaxLatencyMS:         2500,
				MaxTTFTMS:            900,
				RequireFreshMetadata: true,
				PreferLocal:          true,
			},
		},
	}
	dstOrigins := OriginMap{}
	srcOrigins := OriginMap{}
	rec := newOriginRecorder(srcOrigins)
	source := originSource{kind: OriginExplicitFile, source: "override.yaml"}
	rec.set(modelRoleFieldPath("planner", "preferred"), source, "")
	rec.replace(modelRoleFieldPath("planner", "fallback_models"), source, []string{}, "replaces the model role fallback list")
	rec.replace(modelRoleFieldPath("planner", "routing_policy"), source, RoutingPolicyConfig{}, "replaces the model role routing policy")
	rec.replace(modelRoleFieldPath("planner", "preferred_providers"), source, []string{}, "replaces the model role preferred provider list")
	rec.replace(modelRoleFieldPath("planner", "banned_providers"), source, []string{}, "replaces the model role banned provider list")
	rec.replace(modelRoleFieldPath("planner", "banned_models"), source, []string{}, "replaces the model role banned model list")
	rec.replace(modelRoleFieldPath("planner", "required_capabilities"), source, []string{}, "replaces the model role required capability list")
	rec.set(modelRoleFieldPath("planner", "max_cost_usd"), source, 0)
	rec.set(modelRoleFieldPath("planner", "max_latency_ms"), source, 0)
	rec.set(modelRoleFieldPath("planner", "max_ttft_ms"), source, 0)
	rec.set(modelRoleFieldPath("planner", "require_fresh_metadata"), source, false)
	rec.set(modelRoleFieldPath("planner", "prefer_local"), source, false)

	mergeConfigModelRolesFromOrigins(&dst, map[string]ModelRoleConfig{
		"planner": {},
	}, dstOrigins, srcOrigins)

	planner := dst.ModelRoles["planner"]
	assert.Empty(t, planner.Preferred)
	assert.Empty(t, planner.FallbackModels)
	assert.Empty(t, planner.RoutingPolicy.RequiredCapabilities)
	assert.Empty(t, planner.PreferredProviders)
	assert.Empty(t, planner.BannedProviders)
	assert.Empty(t, planner.BannedModels)
	assert.Empty(t, planner.RequiredCapabilities)
	assert.Zero(t, planner.MaxCostUSD)
	assert.Zero(t, planner.MaxLatencyMS)
	assert.Zero(t, planner.MaxTTFTMS)
	assert.False(t, planner.RequireFreshMetadata)
	assert.False(t, planner.PreferLocal)

	origin, ok := dstOrigins.Final("models.planner.prefer_local")
	require.True(t, ok)
	assert.Equal(t, OriginSet, origin.Operation)
	assert.Equal(t, "false", origin.Value)
}

func TestLoadFiles_AutonomyScalarMerges(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	global := writeConfig(t, dir, "global.yaml", `autonomy: low`)
	local := writeConfig(t, dir, "local.yaml", `autonomy: high`)

	cfg, loaded, err := LoadFiles([]string{global, local})
	require.NoError(t, err)
	assert.Equal(t, []string{global, local}, loaded)
	assert.Equal(t, "high", cfg.Autonomy)
}

func TestLoadFiles_ConfiguresAllAgentLoopBudgetFields(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := writeConfig(t, dir, "config.yaml", `
agent_loop:
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
`)

	cfg, loaded, err := LoadFiles([]string{path})
	require.NoError(t, err)
	require.Equal(t, []string{path}, loaded)

	require.NotNil(t, cfg.AgentLoop.MaxOutputBytes)
	assert.EqualValues(t, 4096, *cfg.AgentLoop.MaxOutputBytes)
	require.NotNil(t, cfg.AgentLoop.MaxCostMicros)
	assert.EqualValues(t, 250000, *cfg.AgentLoop.MaxCostMicros)
	require.NotNil(t, cfg.AgentLoop.MaxInputTokens)
	assert.Equal(t, 1000, *cfg.AgentLoop.MaxInputTokens)
	require.NotNil(t, cfg.AgentLoop.MaxOutputTokens)
	assert.Equal(t, 2000, *cfg.AgentLoop.MaxOutputTokens)
	require.NotNil(t, cfg.AgentLoop.MaxTotalTokens)
	assert.Equal(t, 3000, *cfg.AgentLoop.MaxTotalTokens)
	require.NotNil(t, cfg.AgentLoop.MaxIterations)
	assert.Equal(t, 4, *cfg.AgentLoop.MaxIterations)
	require.NotNil(t, cfg.AgentLoop.MaxModelCalls)
	assert.Equal(t, 5, *cfg.AgentLoop.MaxModelCalls)
	require.NotNil(t, cfg.AgentLoop.MaxToolCalls)
	assert.Equal(t, 6, *cfg.AgentLoop.MaxToolCalls)
	require.NotNil(t, cfg.AgentLoop.MaxWallTime)
	assert.Equal(t, "30m", *cfg.AgentLoop.MaxWallTime)
	require.NotNil(t, cfg.AgentLoop.CheckpointInterval)
	assert.Equal(t, 7, *cfg.AgentLoop.CheckpointInterval)
}

func TestLoadFiles_ResolvesScopedVectorizerConfigs(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := writeConfig(t, dir, "config.yaml", `
vector:
  vectorizer: lexical
  provider: ollama
  model: default-embed
  base_url: http://127.0.0.1:11434
  fallback_policy: fail
  timeout_seconds: 5
  stores:
    agent-memory:
      vectorizer: embedding
      model: memory-embed
  agents:
    reviewer:
      model: reviewer-embed
      timeout_seconds: 7
  sources:
    git_history:
      vectorizer: lexical
      index_path: ./.atteler/git-history-vector.json
      chunk_max_runes: 600
`)

	cfg, loaded, origins, err := LoadFilesWithOrigins([]string{path})
	require.NoError(t, err)
	require.Equal(t, []string{path}, loaded)

	memory := cfg.Vector.ResolveVectorizerConfig(VectorScope{Store: testVectorAgentMemoryStore})
	assert.Equal(t, "embedding", memory.Vectorizer)
	assert.Equal(t, "ollama", memory.Provider)
	assert.Equal(t, "memory-embed", memory.Model)
	assert.Equal(t, "http://127.0.0.1:11434", memory.BaseURL)
	assert.Equal(t, "fail", memory.FallbackPolicy)
	assert.Equal(t, 5, memory.TimeoutSeconds)

	agentMemory := cfg.Vector.ResolveVectorizerConfig(VectorScope{
		Store: testVectorAgentMemoryStore,
		Agent: testVectorReviewerAgent,
	})
	assert.Equal(t, "embedding", agentMemory.Vectorizer)
	assert.Equal(t, "reviewer-embed", agentMemory.Model)
	assert.Equal(t, 7, agentMemory.TimeoutSeconds)

	gitHistory := cfg.Vector.ResolveVectorizerConfig(VectorScope{
		Store:  testVectorAgentMemoryStore,
		Agent:  testVectorReviewerAgent,
		Source: "git_history",
	})
	assert.Equal(t, "lexical", gitHistory.Vectorizer)
	assert.Equal(t, "reviewer-embed", gitHistory.Model, "source override should inherit agent/store provider fields it does not set")
	assert.Equal(t, "./.atteler/git-history-vector.json", gitHistory.IndexPath)
	assert.Equal(t, 600, gitHistory.ChunkMaxRunes)

	assert.Contains(t, origins, "vector.stores."+testVectorAgentMemoryStore+".vectorizer")
	assert.Contains(t, origins, "vector.agents."+testVectorReviewerAgent+".timeout_seconds")
	assert.Contains(t, origins, "vector.sources.git_history.index_path")
}

func TestVectorConfig_ResolveVectorizerConfigMatchesHyphenAndUnderscoreScopes(t *testing.T) {
	t.Parallel()

	cfg := VectorConfig{
		Stores: map[string]VectorizerConfig{
			"agent_memory": {
				Vectorizer: "embedding",
				Model:      "memory-embed",
			},
		},
		Sources: map[string]VectorizerConfig{
			"git-history": {
				Vectorizer: "lexical",
				IndexPath:  ".atteler/git-history-vector.json",
			},
		},
	}

	memory := cfg.ResolveVectorizerConfig(VectorScope{Store: "agent-memory"})
	assert.Equal(t, "embedding", memory.Vectorizer)
	assert.Equal(t, "memory-embed", memory.Model)

	gitHistory := cfg.ResolveVectorizerConfig(VectorScope{Source: "git_history"})
	assert.Equal(t, "lexical", gitHistory.Vectorizer)
	assert.Equal(t, ".atteler/git-history-vector.json", gitHistory.IndexPath)
}

func TestLoadFiles_JSONCompatibility(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeConfig(t, dir, "legacy.json", `{"default_model":"gpt-json"}`)

	cfg, loaded, err := LoadFiles([]string{path})
	if err != nil {
		require.NoError(t, err)
	}

	if !reflect.DeepEqual(loaded, []string{path}) {
		require.Failf(t, "unexpected failure", "loaded = %v, want [%s]", loaded, path)
	}

	if cfg.DefaultModel != "gpt-json" {
		require.Failf(t, "unexpected failure", "DefaultModel = %q, want gpt-json", cfg.DefaultModel)
	}
}

func TestLoadFiles_ConfigVersionAndDeprecatedFieldMigration(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeConfig(t, dir, "legacy.yaml", `
version: 0
provider: openai
model: gpt-legacy
generation:
  reasoning: high
agents:
  reviewer:
    prompt: review safely
`)

	cfg, loaded, origins, err := LoadFilesWithOrigins([]string{path})
	require.NoError(t, err)
	require.Equal(t, []string{path}, loaded)
	assert.Equal(t, ConfigSchemaVersion, cfg.Version)
	assert.Equal(t, "openai", cfg.DefaultProvider)
	assert.Equal(t, "gpt-legacy", cfg.DefaultModel)
	assert.Equal(t, "high", cfg.Generation.ReasoningLevel)
	assert.Equal(t, "review safely", cfg.Agents["reviewer"].SystemPrompt)

	final, ok := origins.Final("version")
	require.True(t, ok)
	assert.Equal(t, "1", final.Value)
}

func TestLoadFiles_RejectsFutureConfigVersion(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeConfig(t, dir, "future.yaml", `version: 99`)

	_, _, err := LoadFiles([]string{path})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported version 99")
	assert.Contains(t, err.Error(), path)
}

func TestLoadFiles_RejectsNegativeConfigVersion(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeConfig(t, dir, "negative.yaml", `version: -1`)

	_, _, err := LoadFiles([]string{path})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported version -1")
	assert.Contains(t, err.Error(), path)
}

func TestLoadFiles_HookPrivacyFieldsOverride(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	first := writeConfig(t, dir, "first.yaml", `
hooks:
  user_message:
    - command: [old-hook]
      payload: full
      inherit_env: true
      timeout_seconds: 10
      max_attempts: 5
      retry_backoff_millis: 100
      blocking: true
      env:
        OLD: "1"
`)
	second := writeConfig(t, dir, "second.yaml", `
hooks:
  user_message:
    - command: [new-hook]
      payload: summary
      inherit_env: false
      timeout_seconds: 2
      max_attempts: 3
      retry_backoff_millis: 25
      blocking: false
      env:
        NEW: "1"
`)

	cfg, _, err := LoadFiles([]string{first, second})
	require.NoError(t, err)

	hooks := cfg.Hooks["user_message"]
	require.Len(t, hooks, 1)
	assert.Equal(t, []string{"new-hook"}, hooks[0].Command)
	assert.Equal(t, "summary", hooks[0].Payload)
	assert.False(t, hooks[0].InheritEnv)
	assert.Equal(t, 2, hooks[0].TimeoutSeconds)
	assert.Equal(t, 3, hooks[0].MaxAttempts)
	assert.Equal(t, 25, hooks[0].RetryBackoffMillis)
	assert.False(t, hooks[0].Blocking)
	assert.Equal(t, map[string]string{"NEW": "1"}, hooks[0].Env)
}

func TestLoadFiles_PluginPolicy(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeConfig(t, dir, "plugins.yaml", `
plugins:
  paths:
    - ./plugin-a
  policy:
    permissions:
      filesystem:
        read:
          - "."
        write:
          - tmp
      network:
        allow: true
        hosts:
          - api.example.com
      shell:
        allow: true
      env:
        - PATH
      secrets:
        - API_TOKEN
      tools:
        - git
    output:
      stdout_max_bytes: 4096
      stderr_max_bytes: 1024
    trusted_install_sources:
      - local
    require_signature: true
`)

	cfg, loaded, err := LoadFiles([]string{path})
	require.NoError(t, err)
	require.Equal(t, []string{path}, loaded)
	require.NotNil(t, cfg.Plugins.Policy)
	assert.Equal(t, []string{"./plugin-a"}, cfg.Plugins.Paths)
	assert.Equal(t, []string{"."}, cfg.Plugins.Policy.Permissions.Filesystem.Read)
	assert.Equal(t, []string{"tmp"}, cfg.Plugins.Policy.Permissions.Filesystem.Write)
	assert.True(t, cfg.Plugins.Policy.Permissions.Network.Allow)
	assert.Equal(t, []string{"api.example.com"}, cfg.Plugins.Policy.Permissions.Network.Hosts)
	assert.True(t, cfg.Plugins.Policy.Permissions.Shell.Allow)
	assert.Equal(t, []string{"PATH"}, cfg.Plugins.Policy.Permissions.Env)
	assert.Equal(t, []string{"API_TOKEN"}, cfg.Plugins.Policy.Permissions.Secrets)
	assert.Equal(t, []string{"git"}, cfg.Plugins.Policy.Permissions.Tools)
	assert.Equal(t, 4096, cfg.Plugins.Policy.Output.StdoutMaxBytes)
	assert.Equal(t, 1024, cfg.Plugins.Policy.Output.StderrMaxBytes)
	assert.Equal(t, []string{"local"}, cfg.Plugins.Policy.TrustedInstallSources)
	assert.True(t, cfg.Plugins.Policy.RequireSignature)
}

func TestMergeConfigFromSource_PluginPolicy(t *testing.T) {
	t.Parallel()

	policy := attelerplugin.Policy{
		Permissions: attelerplugin.PermissionSet{
			Filesystem: attelerplugin.FilesystemPermissions{
				Read:  []string{"."},
				Write: []string{"tmp"},
			},
			Network: attelerplugin.NetworkPermissions{
				Allow: true,
				Hosts: []string{"api.example.com"},
			},
			Shell:   attelerplugin.ShellPermissions{Allow: true},
			Env:     []string{"PATH"},
			Secrets: []string{"API_TOKEN"},
			Tools:   []string{"git"},
		},
		Output: attelerplugin.OutputLimits{
			StdoutMaxBytes: 4096,
			StderrMaxBytes: 1024,
		},
		TrustedInstallSources: []string{"local"},
		RequireSignature:      true,
	}

	origins := OriginMap{}

	var cfg Config

	mergeConfigFromSource(&cfg, Config{
		Plugins: PluginConfig{
			Paths:  []string{"./plugin-a"},
			Policy: &policy,
		},
	}, newOriginRecorder(origins), originSource{
		kind:   OriginRuntimeSelection,
		source: "test",
	})

	require.NotNil(t, cfg.Plugins.Policy)
	assert.Equal(t, []string{"./plugin-a"}, cfg.Plugins.Paths)
	assert.Equal(t, []string{"."}, cfg.Plugins.Policy.Permissions.Filesystem.Read)
	assert.Equal(t, []string{"tmp"}, cfg.Plugins.Policy.Permissions.Filesystem.Write)
	assert.True(t, cfg.Plugins.Policy.Permissions.Network.Allow)
	assert.Equal(t, []string{"api.example.com"}, cfg.Plugins.Policy.Permissions.Network.Hosts)
	assert.True(t, cfg.Plugins.Policy.Permissions.Shell.Allow)
	assert.Equal(t, []string{"PATH"}, cfg.Plugins.Policy.Permissions.Env)
	assert.Equal(t, []string{"API_TOKEN"}, cfg.Plugins.Policy.Permissions.Secrets)
	assert.Equal(t, []string{"git"}, cfg.Plugins.Policy.Permissions.Tools)
	assert.Equal(t, 4096, cfg.Plugins.Policy.Output.StdoutMaxBytes)
	assert.Equal(t, 1024, cfg.Plugins.Policy.Output.StderrMaxBytes)
	assert.Equal(t, []string{"local"}, cfg.Plugins.Policy.TrustedInstallSources)
	assert.True(t, cfg.Plugins.Policy.RequireSignature)

	const mutatedPolicyValue = "mutated"

	policy.Permissions.Filesystem.Read[0] = mutatedPolicyValue
	policy.TrustedInstallSources[0] = mutatedPolicyValue

	assert.Equal(t, []string{"."}, cfg.Plugins.Policy.Permissions.Filesystem.Read)
	assert.Equal(t, []string{"local"}, cfg.Plugins.Policy.TrustedInstallSources)

	policyOrigin, ok := origins.Final("plugins.policy")
	require.True(t, ok)
	assert.Equal(t, OriginRuntimeSelection, policyOrigin.Kind)
	assert.Equal(t, "test", policyOrigin.Source)
	assert.Equal(t, "configured", policyOrigin.Value)
}

func TestLoadFilesWithOrigins_TracksScalarOverwriteAndSliceReplacement(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	first := writeConfig(t, dir, "first.yaml", `
default_model: gpt-first
fallback_models: [gpt-first-fallback]
agent_loop:
  max_output_bytes: 1048576
  max_cost_micros: 250000
  max_input_tokens: 1000
  max_output_tokens: 2000
  max_total_tokens: 200000
`)
	second := writeConfig(t, dir, "second.yaml", `
default_model: gpt-second
fallback_models: [gpt-second-fallback]
agent_loop:
  max_output_bytes: 0
  max_cost_micros: 0
  max_input_tokens: 0
  max_output_tokens: 0
  max_total_tokens: 0
`)

	cfg, loaded, origins, err := LoadFilesWithOrigins([]string{first, second})
	require.NoError(t, err)
	require.Equal(t, []string{first, second}, loaded)
	assert.Equal(t, "gpt-second", cfg.DefaultModel)
	assert.Equal(t, []string{"gpt-second-fallback"}, cfg.FallbackModels)
	require.NotNil(t, cfg.AgentLoop.MaxOutputBytes)
	assert.EqualValues(t, 0, *cfg.AgentLoop.MaxOutputBytes)
	require.NotNil(t, cfg.AgentLoop.MaxCostMicros)
	assert.EqualValues(t, 0, *cfg.AgentLoop.MaxCostMicros)
	require.NotNil(t, cfg.AgentLoop.MaxInputTokens)
	assert.Equal(t, 0, *cfg.AgentLoop.MaxInputTokens)
	require.NotNil(t, cfg.AgentLoop.MaxOutputTokens)
	assert.Equal(t, 0, *cfg.AgentLoop.MaxOutputTokens)
	require.NotNil(t, cfg.AgentLoop.MaxTotalTokens)
	assert.Equal(t, 0, *cfg.AgentLoop.MaxTotalTokens)

	modelOrigin := origins["default_model"].Chain
	require.Len(t, modelOrigin, 2)
	assert.Equal(t, OriginSet, modelOrigin[0].Operation)
	assert.Equal(t, first, modelOrigin[0].Source)
	assert.Equal(t, OriginOverride, modelOrigin[1].Operation)
	assert.Equal(t, second, modelOrigin[1].Source)

	fallbackOrigin := origins["fallback_models"].Chain
	require.Len(t, fallbackOrigin, 2)
	assert.Equal(t, OriginSet, fallbackOrigin[0].Operation)
	assert.Equal(t, OriginReplace, fallbackOrigin[1].Operation)
	assert.Equal(t, second, fallbackOrigin[1].Source)
	assert.Contains(t, fallbackOrigin[1].Note, "replaces")

	outputBytesOrigin := origins["agent_loop.max_output_bytes"].Chain
	require.Len(t, outputBytesOrigin, 2)
	assert.Equal(t, OriginOverride, outputBytesOrigin[1].Operation)
	assert.Equal(t, second, outputBytesOrigin[1].Source)
	assert.Equal(t, "0", outputBytesOrigin[1].Value)

	totalTokensOrigin := origins["agent_loop.max_total_tokens"].Chain
	require.Len(t, totalTokensOrigin, 2)
	assert.Equal(t, OriginOverride, totalTokensOrigin[1].Operation)
	assert.Equal(t, second, totalTokensOrigin[1].Source)
	assert.Equal(t, "0", totalTokensOrigin[1].Value)

	costOrigin := origins["agent_loop.max_cost_micros"].Chain
	require.Len(t, costOrigin, 2)
	assert.Equal(t, OriginOverride, costOrigin[1].Operation)
	assert.Equal(t, second, costOrigin[1].Source)
	assert.Equal(t, "0", costOrigin[1].Value)

	inputTokensOrigin := origins["agent_loop.max_input_tokens"].Chain
	require.Len(t, inputTokensOrigin, 2)
	assert.Equal(t, OriginOverride, inputTokensOrigin[1].Operation)
	assert.Equal(t, second, inputTokensOrigin[1].Source)
	assert.Equal(t, "0", inputTokensOrigin[1].Value)

	outputTokensOrigin := origins["agent_loop.max_output_tokens"].Chain
	require.Len(t, outputTokensOrigin, 2)
	assert.Equal(t, OriginOverride, outputTokensOrigin[1].Operation)
	assert.Equal(t, second, outputTokensOrigin[1].Source)
	assert.Equal(t, "0", outputTokensOrigin[1].Value)
}

func TestLoadFiles_MergesAgentRoutingPolicy(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := writeConfig(t, dir, "routing.yaml", `
agents:
  reviewer:
    routing_policy:
      preferred_providers: [anthropic]
      banned_providers: [ollama]
      banned_models: [openai/gpt-expensive]
      required_capabilities: [tools]
      max_budget: 0.25
      max_latency_ms: 1200
      max_ttft_ms: 300
      require_fresh_metadata: true
`)

	cfg, _, origins, err := LoadFilesWithOrigins([]string{path})
	require.NoError(t, err)

	policy := cfg.Agents["reviewer"].RoutingPolicy
	assert.Equal(t, []string{"anthropic"}, policy.PreferredProviders)
	assert.Equal(t, []string{"ollama"}, policy.BannedProviders)
	assert.Equal(t, []string{"openai/gpt-expensive"}, policy.BannedModels)
	assert.Equal(t, []string{"tools"}, policy.RequiredCapabilities)
	assert.InDelta(t, 0.25, policy.MaxBudget, 0.000000001)
	assert.Equal(t, 1200, policy.MaxLatencyMS)
	assert.Equal(t, 300, policy.MaxTTFTMS)
	assert.True(t, policy.RequireFreshMetadata)

	origin := origins["agents.reviewer.routing_policy"].Chain
	require.Len(t, origin, 1)
	assert.Equal(t, OriginSet, origin[0].Operation)
}

func TestLoadFilesWithOrigins_TracksReferencePolicyOverrides(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	first := writeConfig(t, dir, "first.yaml", `
context:
  reference_policy:
    allowed_hosts: [docs.example.com]
    max_redirects: 3
    max_files: 5
    allow_absolute_paths: true
    allow_private_networks: true
`)
	second := writeConfig(t, dir, "second.yaml", `
context:
  reference_policy:
    allowed_hosts: []
    max_redirects: 0
    max_files: 0
    allow_absolute_paths: false
    allow_private_networks: false
`)

	cfg, loaded, origins, err := LoadFilesWithOrigins([]string{first, second})
	require.NoError(t, err)
	require.Equal(t, []string{first, second}, loaded)
	assert.Empty(t, cfg.Context.ReferencePolicy.AllowedHosts)
	assert.Equal(t, 0, cfg.Context.ReferencePolicy.MaxRedirects)
	assert.Equal(t, 0, cfg.Context.ReferencePolicy.MaxFiles)
	assert.False(t, cfg.Context.ReferencePolicy.AllowAbsolutePaths)
	assert.False(t, cfg.Context.ReferencePolicy.AllowPrivateNetworks)

	hostsOrigin := origins["context.reference_policy.allowed_hosts"].Chain
	require.Len(t, hostsOrigin, 2)
	assert.Equal(t, OriginSet, hostsOrigin[0].Operation)
	assert.Equal(t, OriginReplace, hostsOrigin[1].Operation)
	assert.Equal(t, second, hostsOrigin[1].Source)
	assert.Equal(t, "[]", hostsOrigin[1].Value)

	maxFilesOrigin := origins["context.reference_policy.max_files"].Chain
	require.Len(t, maxFilesOrigin, 2)
	assert.Equal(t, OriginOverride, maxFilesOrigin[1].Operation)
	assert.Equal(t, second, maxFilesOrigin[1].Source)
	assert.Equal(t, "0", maxFilesOrigin[1].Value)

	redirectOrigin := origins["context.reference_policy.max_redirects"].Chain
	require.Len(t, redirectOrigin, 2)
	assert.Equal(t, OriginOverride, redirectOrigin[1].Operation)
	assert.Equal(t, second, redirectOrigin[1].Source)
	assert.Equal(t, "0", redirectOrigin[1].Value)

	absoluteOrigin := origins["context.reference_policy.allow_absolute_paths"].Chain
	require.Len(t, absoluteOrigin, 2)
	assert.Equal(t, OriginOverride, absoluteOrigin[1].Operation)
	assert.Equal(t, second, absoluteOrigin[1].Source)
	assert.Equal(t, "false", absoluteOrigin[1].Value)

	privateNetworksOrigin := origins["context.reference_policy.allow_private_networks"].Chain
	require.Len(t, privateNetworksOrigin, 2)
	assert.Equal(t, OriginOverride, privateNetworksOrigin[1].Operation)
	assert.Equal(t, second, privateNetworksOrigin[1].Source)
	assert.Equal(t, "false", privateNetworksOrigin[1].Value)
}

func TestLoadFiles_ContextReferencePolicyPreservesExplicitEmptyDefaultLists(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := writeConfig(t, dir, "empty-reference-policy-lists.yaml", `
context:
  reference_policy:
    allowed_schemes: []
    content_types: []
`)

	cfg, _, origins, err := LoadFilesWithOrigins([]string{path})
	require.NoError(t, err)

	assert.NotNil(t, cfg.Context.ReferencePolicy.AllowedSchemes)
	assert.Empty(t, cfg.Context.ReferencePolicy.AllowedSchemes)
	assert.NotNil(t, cfg.Context.ReferencePolicy.ContentTypes)
	assert.Empty(t, cfg.Context.ReferencePolicy.ContentTypes)

	schemesOrigin := origins["context.reference_policy.allowed_schemes"].Chain
	require.Len(t, schemesOrigin, 1)
	assert.Equal(t, "[]", schemesOrigin[0].Value)

	contentTypesOrigin := origins["context.reference_policy.content_types"].Chain
	require.Len(t, contentTypesOrigin, 1)
	assert.Equal(t, "[]", contentTypesOrigin[0].Value)
}

func TestReferencePolicyConfigMarshalYAML_PreservesExplicitEmptyLists(t *testing.T) {
	t.Parallel()

	out, err := yaml.Marshal(ReferencePolicyConfig{
		AllowedSchemes: []string{},
		DeniedHosts:    []string{},
		ContentTypes:   []string{},
	})
	require.NoError(t, err)

	text := string(out)
	assert.Contains(t, text, "allowed_schemes: []")
	assert.Contains(t, text, "denied_hosts: []")
	assert.Contains(t, text, "content_types: []")
	assert.NotContains(t, text, "allowed_hosts:")
}

func TestLoadFilesWithOrigins_TracksMapMergeProviderAgentAndPluginReplacement(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	first := writeConfig(t, dir, "first.yaml", `
providers:
  anthropic:
    base_url: https://anthropic.first
    retry:
      max_attempts: 1
      initial_backoff_ms: 250
  openai:
    base_url: https://openai.first
agents:
  reviewer:
    model: gpt-review-first
    tools:
      bash: true
plugins:
  paths: [./plugin-a]
`)
	second := writeConfig(t, dir, "second.yaml", `
providers:
  openai:
    base_url: https://openai.second
    disabled: false
    auto_start: true
    retry:
      max_attempts: 0
      jitter_fraction: 0.1
agents:
  reviewer:
    model: gpt-review-second
    tools:
      shell: false
plugins:
  paths: [./plugin-b]
`)

	cfg, _, origins, err := LoadFilesWithOrigins([]string{first, second})
	require.NoError(t, err)
	assert.Equal(t, "https://anthropic.first", cfg.Providers["anthropic"].BaseURL)
	assert.Equal(t, "https://openai.second", cfg.Providers["openai"].BaseURL)
	assert.True(t, cfg.Providers["openai"].AutoStart)
	require.NotNil(t, cfg.Providers["anthropic"].Retry.MaxAttempts)
	assert.Equal(t, 1, *cfg.Providers["anthropic"].Retry.MaxAttempts)
	require.NotNil(t, cfg.Providers["anthropic"].Retry.InitialBackoffMS)
	assert.Equal(t, 250, *cfg.Providers["anthropic"].Retry.InitialBackoffMS)
	require.NotNil(t, cfg.Providers["openai"].Retry.MaxAttempts)
	assert.Equal(t, 0, *cfg.Providers["openai"].Retry.MaxAttempts)
	require.NotNil(t, cfg.Providers["openai"].Retry.JitterFraction)
	assert.InEpsilon(t, 0.1, *cfg.Providers["openai"].Retry.JitterFraction, 0.0001)
	assert.Equal(t, "gpt-review-second", cfg.Agents["reviewer"].Model)
	assert.Equal(t, map[string]bool{"shell": false}, cfg.Agents["reviewer"].ToolPermissions)
	assert.Equal(t, []string{"./plugin-b"}, cfg.Plugins.Paths)

	providerMapOrigin := origins["providers"].Chain
	require.Len(t, providerMapOrigin, 2)
	assert.Equal(t, OriginSet, providerMapOrigin[0].Operation)
	assert.Equal(t, OriginMerge, providerMapOrigin[1].Operation)

	providerFieldOrigin := origins["providers.openai.base_url"].Chain
	require.Len(t, providerFieldOrigin, 2)
	assert.Equal(t, OriginOverride, providerFieldOrigin[1].Operation)
	assert.Equal(t, second, providerFieldOrigin[1].Source)

	autoStartOrigin := origins["providers.openai.auto_start"].Chain
	require.Len(t, autoStartOrigin, 1)
	assert.Equal(t, OriginSet, autoStartOrigin[0].Operation)
	assert.Equal(t, second, autoStartOrigin[0].Source)

	retryOrigin := origins["providers.openai.retry.max_attempts"].Chain
	require.Len(t, retryOrigin, 1)
	assert.Equal(t, OriginSet, retryOrigin[0].Operation)
	assert.Equal(t, second, retryOrigin[0].Source)

	agentOrigin := origins["agents.reviewer.model"].Chain
	require.Len(t, agentOrigin, 2)
	assert.Equal(t, OriginOverride, agentOrigin[1].Operation)
	assert.Equal(t, second, agentOrigin[1].Source)

	toolsOrigin := origins["agents.reviewer.tools"].Chain
	require.Len(t, toolsOrigin, 2)
	assert.Equal(t, OriginReplace, toolsOrigin[1].Operation)

	pluginOrigin := origins["plugins.paths"].Chain
	require.Len(t, pluginOrigin, 2)
	assert.Equal(t, OriginReplace, pluginOrigin[1].Operation)
	assert.Equal(t, second, pluginOrigin[1].Source)
}

func TestOriginChain_MergesMapOriginsAcrossOriginMaps(t *testing.T) {
	t.Parallel()

	dst := OriginMap{
		"providers": {
			Chain: []OriginEvent{{
				Kind:      OriginHarnessImport,
				Operation: OriginSet,
				Source:    "harness",
				Value:     `["codex"]`,
				Note:      "merges provider definitions by name",
			}},
		},
	}
	src := OriginMap{
		"providers": {
			Chain: []OriginEvent{{
				Kind:      OriginEnvFile,
				Operation: OriginSet,
				Source:    "env.yaml",
				Value:     `["openai"]`,
				Note:      "merges provider definitions by name",
			}},
		},
		"plugins.paths": {
			Chain: []OriginEvent{{
				Kind:      OriginEnvFile,
				Operation: OriginSet,
				Source:    "env.yaml",
				Value:     `["./plugin"]`,
				Note:      "replaces the entire plugin path list",
			}},
		},
	}

	appendOriginChain(dst, "providers", src, false)
	appendOriginChain(dst, "plugins.paths", src, true)

	require.Len(t, dst["providers"].Chain, 2)
	assert.Equal(t, OriginMerge, dst["providers"].Chain[1].Operation)
	require.Len(t, dst["plugins.paths"].Chain, 1)
	assert.Equal(t, OriginSet, dst["plugins.paths"].Chain[0].Operation)

	appendOriginChain(dst, "plugins.paths", src, true)
	require.Len(t, dst["plugins.paths"].Chain, 2)
	assert.Equal(t, OriginReplace, dst["plugins.paths"].Chain[1].Operation)
}

func TestMergeConfigFromOrigins_PreservesProviderBoolWhenSourceOmitsIt(t *testing.T) {
	t.Parallel()

	dst := Config{
		Providers: map[string]ProviderConfig{
			"codex": {DisablePrivateAdapter: true},
		},
	}
	dstOrigins := OriginMap{
		"providers.codex.disable_private_adapter": {
			Chain: []OriginEvent{{
				Kind:      OriginHarnessImport,
				Operation: OriginSet,
				Source:    "harness",
				Value:     "true",
			}},
		},
	}
	src := Config{
		Providers: map[string]ProviderConfig{
			"codex": {BaseURL: "https://codex.example"},
		},
	}
	srcOrigins := OriginMap{
		"providers.codex.base_url": {
			Chain: []OriginEvent{{
				Kind:      OriginProjectFile,
				Operation: OriginSet,
				Source:    "project.yaml",
				Value:     "https://codex.example",
			}},
		},
	}

	mergeConfigFromOrigins(&dst, src, dstOrigins, srcOrigins)

	assert.True(t, dst.Providers["codex"].DisablePrivateAdapter)
	require.Len(t, dstOrigins["providers.codex.disable_private_adapter"].Chain, 1)

	baseURLOrigin, ok := dstOrigins.Final("providers.codex.base_url")
	require.True(t, ok)
	assert.Equal(t, "project.yaml", baseURLOrigin.Source)
}

func TestMergeConfigFromOrigins_AllowsExplicitProviderBoolFalse(t *testing.T) {
	t.Parallel()

	dst := Config{
		Providers: map[string]ProviderConfig{
			"codex": {DisablePrivateAdapter: true},
		},
	}
	dstOrigins := OriginMap{
		"providers.codex.disable_private_adapter": {
			Chain: []OriginEvent{{
				Kind:      OriginHarnessImport,
				Operation: OriginSet,
				Source:    "harness",
				Value:     "true",
			}},
		},
	}
	src := Config{
		Providers: map[string]ProviderConfig{
			"codex": {DisablePrivateAdapter: false},
		},
	}
	srcOrigins := OriginMap{
		"providers.codex.disable_private_adapter": {
			Chain: []OriginEvent{{
				Kind:      OriginProjectFile,
				Operation: OriginSet,
				Source:    "project.yaml",
				Value:     "false",
			}},
		},
	}

	mergeConfigFromOrigins(&dst, src, dstOrigins, srcOrigins)

	assert.False(t, dst.Providers["codex"].DisablePrivateAdapter)

	chain := dstOrigins["providers.codex.disable_private_adapter"].Chain
	require.Len(t, chain, 2)
	assert.Equal(t, OriginOverride, chain[1].Operation)
	assert.Equal(t, "project.yaml", chain[1].Source)
	assert.Equal(t, "false", chain[1].Value)
}

func TestLoadPathSources_EnvPathPrecedence(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	global := writeConfig(t, dir, "global.yaml", `default_model: gpt-global`)
	project := writeConfig(t, dir, "project.yaml", `default_model: gpt-project`)
	envPath := writeConfig(t, dir, "env.yaml", `default_model: gpt-env`)

	cfg, loaded, origins, err := LoadPathSources([]PathSource{
		{Path: global, Kind: OriginGlobalFile},
		{Path: project, Kind: OriginProjectFile},
		{Path: envPath, Kind: OriginEnvFile},
	})
	require.NoError(t, err)
	require.Equal(t, []string{global, project, envPath}, loaded)
	assert.Equal(t, "gpt-env", cfg.DefaultModel)

	chain := origins["default_model"].Chain
	require.Len(t, chain, 3)
	assert.Equal(t, OriginGlobalFile, chain[0].Kind)
	assert.Equal(t, OriginProjectFile, chain[1].Kind)
	assert.Equal(t, OriginEnvFile, chain[2].Kind)
	assert.Equal(t, OriginOverride, chain[2].Operation)
}

func TestLoadWithOrigins_DefaultStackClassifiesEnvAndOverridesProject(t *testing.T) {
	tempDir := t.TempDir()
	configHome := filepath.Join(tempDir, "xdg-config")
	projectDir := filepath.Join(tempDir, "project")
	require.NoError(t, os.MkdirAll(filepath.Join(configHome, "atteler"), 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(projectDir, ".atteler"), 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(tempDir, "codex-home"), 0o700))

	global := writeConfig(t, filepath.Join(configHome, "atteler"), "config.yaml", `default_model: gpt-global`)
	project := writeConfig(t, filepath.Join(projectDir, ".atteler"), "config.yaml", `default_model: gpt-project`)
	envPath := writeConfig(t, tempDir, "env.yaml", `
default_model: gpt-env
context:
  reference_policy:
    allowed_schemes: [http]
    allowed_hosts: [docs.example.com]
    max_files: 9
    allow_absolute_paths: true
`)

	t.Setenv("HOME", tempDir)
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("CODEX_HOME", filepath.Join(tempDir, "codex-home"))
	t.Setenv("OPENCODE_CONFIG", "")
	t.Setenv("OPENCODE_CONFIG_DIR", "")
	t.Setenv("FORGE_CONFIG", "")
	t.Setenv(EnvPath, envPath)
	t.Chdir(projectDir)

	cfg, loaded, origins, err := LoadWithOrigins()
	require.NoError(t, err)
	assert.Equal(t, "gpt-env", cfg.DefaultModel)
	assert.Contains(t, loaded, global)
	assert.Contains(t, loaded, project)
	assert.Contains(t, loaded, envPath)

	chain := origins["default_model"].Chain
	require.Len(t, chain, 3)
	assert.Equal(t, OriginGlobalFile, chain[0].Kind)
	assert.Equal(t, OriginProjectFile, chain[1].Kind)
	assert.Equal(t, OriginEnvFile, chain[2].Kind)
	assert.Equal(t, envPath, chain[2].Source)

	assert.Equal(t, []string{"http"}, cfg.Context.ReferencePolicy.AllowedSchemes)
	assert.Equal(t, []string{"docs.example.com"}, cfg.Context.ReferencePolicy.AllowedHosts)
	assert.Equal(t, 9, cfg.Context.ReferencePolicy.MaxFiles)
	assert.True(t, cfg.Context.ReferencePolicy.AllowAbsolutePaths)

	policyOrigin := origins["context.reference_policy.max_files"].Chain
	require.Len(t, policyOrigin, 1)
	assert.Equal(t, OriginEnvFile, policyOrigin[0].Kind)
	assert.Equal(t, envPath, policyOrigin[0].Source)
	assert.Equal(t, "9", policyOrigin[0].Value)
}

func TestLoadWithDiagnostics_ReturnsHarnessWarningsWhenConfigFileFails(t *testing.T) {
	tempDir := t.TempDir()
	configHome := filepath.Join(tempDir, "xdg-config")
	projectDir := filepath.Join(tempDir, "project")
	codexHome := filepath.Join(tempDir, "codex-home")
	require.NoError(t, os.MkdirAll(codexHome, 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(projectDir, ".atteler"), 0o700))

	codexConfig := filepath.Join(codexHome, "config.toml")
	require.NoError(t, os.WriteFile(codexConfig, []byte(`
model = "gpt-5.5"
trusted_project_roots = ["/repo"]
`), 0o600))

	projectConfig := filepath.Join(projectDir, ".atteler", "config.yaml")
	require.NoError(t, os.WriteFile(projectConfig, []byte(`default_model: [`), 0o600))

	t.Setenv("HOME", tempDir)
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("OPENCODE_CONFIG", "")
	t.Setenv("OPENCODE_CONFIG_DIR", "")
	t.Setenv("FORGE_CONFIG", filepath.Join(tempDir, "missing-forge"))
	t.Setenv(EnvPath, "")
	t.Chdir(projectDir)

	cfg, loaded, origins, diagnostics, err := LoadWithDiagnostics()

	require.Error(t, err)
	assert.Contains(t, err.Error(), projectConfig)
	assert.Equal(t, "gpt-5.5", cfg.DefaultModel)
	assert.Contains(t, loaded, codexConfig)
	assertDiagnosticContains(t, diagnostics, "trusted_project_roots: ignored unsupported field")

	origin, ok := origins.Final("default_model")
	require.True(t, ok)
	assert.Equal(t, OriginHarnessImport, origin.Kind)
	assert.Equal(t, codexConfig, origin.Source)
}

func TestLoadFiles_InvalidYAMLIncludesPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeConfig(t, dir, "bad.yaml", `default_model: [`)

	_, _, err := LoadFiles([]string{path})
	if err == nil {
		require.FailNow(t, "expected parse error")
	}

	if got := err.Error(); !strings.Contains(got, path) {
		require.Failf(t, "unexpected failure", "error = %q, want path %q", got, path)
	}
}

func TestDefaultPaths_IncludesEnvOverrideLast(t *testing.T) {
	t.Setenv(EnvPath, "one"+string(os.PathListSeparator)+"two")

	paths := DefaultPaths()
	if len(paths) < 2 {
		require.Failf(t, "unexpected failure", "paths = %v, want env paths included", paths)
	}

	gotTail := paths[len(paths)-2:]
	if !reflect.DeepEqual(gotTail, []string{"one", "two"}) {
		require.Failf(t, "unexpected failure", "tail = %v, want [one two]", gotTail)
	}
}

func TestDefaultPaths_PrefersYAML(t *testing.T) {
	t.Setenv(EnvPath, "")

	paths := DefaultPaths()
	if len(paths) < 3 {
		require.Failf(t, "unexpected failure", "paths = %v, want default paths", paths)
	}

	for i, path := range paths {
		if strings.HasSuffix(path, "config.json") && i > 0 && strings.HasSuffix(paths[i-1], "config.yml") {
			return
		}
	}

	require.Failf(t, "unexpected failure", "paths = %v, want config.yaml/config.yml before config.json", paths)
}

func TestLoadFiles_ContextReferences(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := writeConfig(t, dir, "refs.yaml", `
context:
  references:
    - ./docs/guide.md
    - https://example.com/api-docs
`)

	cfg, loaded, err := LoadFiles([]string{path})
	require.NoError(t, err)
	require.Equal(t, []string{path}, loaded)
	assert.Equal(t, []string{"./docs/guide.md", "https://example.com/api-docs"}, cfg.Context.References)
}

func TestLoadFiles_ContextReferencesOverride(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	first := writeConfig(t, dir, "first.yaml", `
context:
  references:
    - ./old-docs.md
`)
	second := writeConfig(t, dir, "second.yaml", `
context:
  references:
    - ./new-docs.md
    - https://example.com/ref
`)

	cfg, _, err := LoadFiles([]string{first, second})
	require.NoError(t, err)
	assert.Equal(t, []string{"./new-docs.md", "https://example.com/ref"}, cfg.Context.References)
}

func TestLoadFiles_ContextReferencePolicy(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := writeConfig(t, dir, "refs-policy.yaml", `
context:
  reference_policy:
    allowed_schemes: [https]
    denied_schemes: [http]
    allowed_hosts:
      - docs.example.com
      - "*.trusted.example"
    denied_hosts:
      - blocked.example.com
    allowed_ports: [443, 8443]
    denied_ports: [81]
    local_roots:
      - ../shared-style
    denied_local_roots:
      - ../shared-style/secrets
    allowed_globs:
      - docs/**/*.md
    denied_globs:
      - "**/*.pem"
    max_redirects: 2
    max_files: 7
    content_types:
      - text/*
      - application/json
    allow_absolute_paths: true
    allow_private_networks: false
`)

	cfg, _, origins, err := LoadFilesWithOrigins([]string{path})
	require.NoError(t, err)
	assert.Equal(t, []string{"https"}, cfg.Context.ReferencePolicy.AllowedSchemes)
	assert.Equal(t, []string{"http"}, cfg.Context.ReferencePolicy.DeniedSchemes)
	assert.Equal(t, []string{"docs.example.com", "*.trusted.example"}, cfg.Context.ReferencePolicy.AllowedHosts)
	assert.Equal(t, []string{"blocked.example.com"}, cfg.Context.ReferencePolicy.DeniedHosts)
	assert.Equal(t, []int{443, 8443}, cfg.Context.ReferencePolicy.AllowedPorts)
	assert.Equal(t, []int{81}, cfg.Context.ReferencePolicy.DeniedPorts)
	assert.Equal(t, []string{"../shared-style"}, cfg.Context.ReferencePolicy.LocalRoots)
	assert.Equal(t, []string{"../shared-style/secrets"}, cfg.Context.ReferencePolicy.DeniedLocalRoots)
	assert.Equal(t, []string{"docs/**/*.md"}, cfg.Context.ReferencePolicy.AllowedGlobs)
	assert.Equal(t, []string{"**/*.pem"}, cfg.Context.ReferencePolicy.DeniedGlobs)
	assert.Equal(t, 2, cfg.Context.ReferencePolicy.MaxRedirects)
	assert.Equal(t, 7, cfg.Context.ReferencePolicy.MaxFiles)
	assert.Equal(t, []string{"text/*", "application/json"}, cfg.Context.ReferencePolicy.ContentTypes)
	assert.True(t, cfg.Context.ReferencePolicy.AllowAbsolutePaths)
	assert.False(t, cfg.Context.ReferencePolicy.AllowPrivateNetworks)

	allowedHostsOrigin, ok := origins.Final("context.reference_policy.allowed_hosts")
	require.True(t, ok)
	assert.Equal(t, OriginExplicitFile, allowedHostsOrigin.Kind)
	assert.Equal(t, `["docs.example.com","*.trusted.example"]`, allowedHostsOrigin.Value)

	privateNetworksOrigin, ok := origins.Final("context.reference_policy.allow_private_networks")
	require.True(t, ok)
	assert.Equal(t, "false", privateNetworksOrigin.Value)
}

func TestLoadWithOrigins_PreservesContextReferencePolicy(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	cwd := filepath.Join(dir, "cwd")

	require.NoError(t, os.MkdirAll(home, 0o700))
	require.NoError(t, os.MkdirAll(cwd, 0o700))

	first := writeConfig(t, dir, "first.yaml", `
context:
  reference_policy:
    allowed_hosts: [old.example.com]
    max_redirects: 5
    allow_private_networks: true
`)
	second := writeConfig(t, dir, "second.yaml", `
context:
  reference_policy:
    allowed_schemes: [https]
    allowed_hosts: [docs.example.com]
    local_roots: [../shared]
    max_redirects: 0
    content_types: [text/*]
    allow_private_networks: false
`)

	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv(EnvPath, first+string(os.PathListSeparator)+second)
	t.Chdir(cwd)

	cfg, loaded, origins, err := LoadWithOrigins()
	require.NoError(t, err)
	assert.Contains(t, loaded, first)
	assert.Contains(t, loaded, second)
	assert.Equal(t, []string{"https"}, cfg.Context.ReferencePolicy.AllowedSchemes)
	assert.Equal(t, []string{"docs.example.com"}, cfg.Context.ReferencePolicy.AllowedHosts)
	assert.Equal(t, []string{"../shared"}, cfg.Context.ReferencePolicy.LocalRoots)
	assert.Equal(t, 0, cfg.Context.ReferencePolicy.MaxRedirects)
	assert.Equal(t, []string{"text/*"}, cfg.Context.ReferencePolicy.ContentTypes)
	assert.False(t, cfg.Context.ReferencePolicy.AllowPrivateNetworks)

	maxRedirectsOrigin, ok := origins.Final("context.reference_policy.max_redirects")
	require.True(t, ok)
	assert.Equal(t, OriginEnvFile, maxRedirectsOrigin.Kind)
	assert.Equal(t, second, maxRedirectsOrigin.Source)
	assert.Equal(t, "0", maxRedirectsOrigin.Value)

	privateNetworkOrigin, ok := origins.Final("context.reference_policy.allow_private_networks")
	require.True(t, ok)
	assert.Equal(t, OriginEnvFile, privateNetworkOrigin.Kind)
	assert.Equal(t, second, privateNetworkOrigin.Source)
	assert.Equal(t, "false", privateNetworkOrigin.Value)
}

func TestLoadFiles_AgentReferences(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := writeConfig(t, dir, "agent-refs.yaml", `
agents:
  reviewer:
    description: Reviews code
    references:
      - ./review-guidelines.md
      - https://example.com/style-guide
`)

	cfg, _, err := LoadFiles([]string{path})
	require.NoError(t, err)

	reviewer := cfg.Agents["reviewer"]
	assert.Equal(t, "Reviews code", reviewer.Description)
	assert.Equal(t, []string{"./review-guidelines.md", "https://example.com/style-guide"}, reviewer.References)
}

func TestLoadFiles_AgentReferencesOverride(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	first := writeConfig(t, dir, "first.yaml", `
agents:
  reviewer:
    references:
      - ./old-guide.md
`)
	second := writeConfig(t, dir, "second.yaml", `
agents:
  reviewer:
    references:
      - ./new-guide.md
`)

	cfg, _, err := LoadFiles([]string{first, second})
	require.NoError(t, err)
	assert.Equal(t, []string{"./new-guide.md"}, cfg.Agents["reviewer"].References)
}

func writeConfig(t *testing.T, dir, name, content string) string {
	t.Helper()

	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		require.NoError(t, err)
	}

	return path
}
