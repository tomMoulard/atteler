package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRedactedConfig_ReturnsIndependentCopy(t *testing.T) {
	t.Parallel()

	temperature := 0.2
	seed := 7
	maxIterations := 3
	enabled := true

	cfg := Config{
		Generation: GenerationConfig{
			Temperature: &temperature,
		},
		AgentLoop: AgentLoopConfig{
			MaxIterations: &maxIterations,
		},
		Agents: map[string]AgentConfig{
			"reviewer": {
				Seed:             &seed,
				SystemPrompt:     "private prompt",
				FallbackModels:   []string{"fallback-a"},
				ToolPermissions:  map[string]bool{"read": true},
				FeedbackGuidance: []FeedbackGuidance{{ID: "private-feedback"}},
			},
		},
		Hooks: map[string][]HookConfig{
			"session_end": {{
				Command: []string{"echo", "done"},
				Env:     map[string]string{"SAFE": "value"},
			}},
		},
		SkillLearning: SkillLearningConfig{Enabled: &enabled},
	}

	redacted := RedactedConfig(cfg)
	*redacted.Generation.Temperature = 0.9
	*redacted.AgentLoop.MaxIterations = 99
	*redacted.Agents["reviewer"].Seed = 42
	redacted.Agents["reviewer"].FallbackModels[0] = "fallback-b"
	redacted.Agents["reviewer"].ToolPermissions["read"] = false
	redacted.Hooks["session_end"][0].Command[0] = "printf"
	redacted.Hooks["session_end"][0].Env["SAFE"] = "changed"
	*redacted.SkillLearning.Enabled = false

	assert.InDelta(t, 0.2, *cfg.Generation.Temperature, 0)
	assert.Equal(t, 3, *cfg.AgentLoop.MaxIterations)
	assert.Equal(t, 7, *cfg.Agents["reviewer"].Seed)
	assert.Equal(t, []string{"fallback-a"}, cfg.Agents["reviewer"].FallbackModels)
	assert.True(t, cfg.Agents["reviewer"].ToolPermissions["read"])
	assert.Equal(t, []string{"echo", "done"}, cfg.Hooks["session_end"][0].Command)
	assert.Equal(t, "value", cfg.Hooks["session_end"][0].Env["SAFE"])
	assert.True(t, *cfg.SkillLearning.Enabled)
	assert.Equal(t, RedactedValue, redacted.Agents["reviewer"].SystemPrompt)
	assert.Empty(t, redacted.Agents["reviewer"].FeedbackGuidance)
}
