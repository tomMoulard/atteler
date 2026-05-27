package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRedactedConfig_ReturnsIndependentCopy(t *testing.T) {
	t.Parallel()

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
	}

	redacted := RedactedConfig(cfg)
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
	*redacted.Agents["reviewer"].Seed = 42
	redacted.Agents["reviewer"].RoutingPolicy.PreferredProviders[0] = "anthropic"
	redacted.Agents["reviewer"].FallbackModels[0] = "fallback-b"
	redacted.ModelAliases["fast"] = "openai/changed"
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
	assert.Equal(t, 7, *cfg.Agents["reviewer"].Seed)
	assert.Equal(t, []string{"openai"}, cfg.Agents["reviewer"].RoutingPolicy.PreferredProviders)
	assert.Equal(t, []string{"fallback-a"}, cfg.Agents["reviewer"].FallbackModels)
	assert.Equal(t, "openai/gpt-4.1-mini", cfg.ModelAliases["fast"])
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
	assert.Equal(t, RedactedValue, redacted.Agents["reviewer"].SystemPrompt)
	assert.Empty(t, redacted.Agents["reviewer"].FeedbackGuidance)
}
