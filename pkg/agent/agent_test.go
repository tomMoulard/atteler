package agent

import (
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/config"
	"github.com/tommoulard/atteler/pkg/feedback"
	"github.com/tommoulard/atteler/pkg/llm"
)

const reviewerAgentName = "reviewer"

func TestRegistry_UpsertOverridesExisting(t *testing.T) {
	t.Parallel()

	registry := NewRegistry(map[string]config.AgentConfig{
		"writer": {Model: "claude-sonnet-4-20250514"},
	})

	// Insert a new agent.
	registry.Upsert(Agent{Name: "explorer", SystemPrompt: "explore"})
	explorer, ok := registry.Get("explorer")
	require.True(t, ok)
	assert.Equal(t, "explore", explorer.SystemPrompt)

	// Replace an existing agent.
	registry.Upsert(Agent{Name: "writer", SystemPrompt: "rewritten"})
	writer, ok := registry.Get("writer")
	require.True(t, ok)
	assert.Equal(t, "rewritten", writer.SystemPrompt)

	// A blank name is ignored.
	registry.Upsert(Agent{Name: "   ", SystemPrompt: "ignored"})
	assert.Equal(t, []string{"explorer", "writer"}, registry.List())
}

func TestRegistry_UpsertNilReceiver(t *testing.T) {
	t.Parallel()

	var registry *Registry

	assert.NotPanics(t, func() { registry.Upsert(Agent{Name: "x"}) })
}

func TestRegistry_GetAndList(t *testing.T) {
	t.Parallel()

	registry := NewRegistry(map[string]config.AgentConfig{
		reviewerAgentName: {
			Description:    "Reviews code",
			Personality:    "concise",
			Model:          "gpt-4.1",
			FallbackModels: []string{"gpt-4.1-mini"},
			Capabilities:   []string{"review", "security"},
			Triggers:       []string{"review this"},
			RoutingPolicy: config.RoutingPolicyConfig{
				PreferredProviders:   []string{"Anthropic"},
				BannedProviders:      []string{"ollama"},
				BannedModels:         []string{"openai/gpt-expensive"},
				RequiredCapabilities: []string{"Tools"},
				MaxBudget:            0.25,
				MaxLatencyMS:         1200,
				MaxTTFTMS:            300,
				RequireFreshMetadata: true,
			},
		},
		"writer": {Model: "claude-sonnet-4-20250514"},
		"internal": {
			Model:  "gpt-hidden",
			Hidden: true,
		},
	})

	names := registry.List()
	if !reflect.DeepEqual(names, []string{"reviewer", "writer"}) {
		require.Failf(t, "unexpected failure", "names = %v", names)
	}

	agent, ok := registry.Get(reviewerAgentName)
	if !ok {
		require.FailNow(t, "expected reviewer agent")
	}

	if agent.Model != "gpt-4.1" {
		assert.Failf(t, "assertion failed", "model = %q", agent.Model)
	}

	if !reflect.DeepEqual(agent.Triggers, []string{"review this"}) {
		assert.Failf(t, "assertion failed", "triggers = %v", agent.Triggers)
	}

	if agent.Description != "Reviews code" || agent.Personality != "concise" {
		assert.Failf(t, "assertion failed", "metadata = %q/%q", agent.Description, agent.Personality)
	}

	if !reflect.DeepEqual(agent.Capabilities, []string{"review", "security"}) {
		assert.Failf(t, "assertion failed", "capabilities = %v", agent.Capabilities)
	}

	if !reflect.DeepEqual(agent.ModelChain(), []string{"gpt-4.1", "gpt-4.1-mini"}) {
		assert.Failf(t, "assertion failed", "model chain = %v", agent.ModelChain())
	}

	assert.Equal(t, []string{"anthropic"}, agent.RoutingPolicy.PreferredProviders)
	assert.Equal(t, []string{"ollama"}, agent.RoutingPolicy.BannedProviders)
	assert.Equal(t, []string{"openai/gpt-expensive"}, agent.RoutingPolicy.BannedModels)
	assert.Equal(t, []string{"tools"}, agent.RoutingPolicy.RequiredCapabilities)
	assert.InDelta(t, 0.25, agent.RoutingPolicy.MaxBudget, 0.000000001)
	assert.Equal(t, 1200, agent.RoutingPolicy.MaxLatencyMS)
	assert.Equal(t, 300, agent.RoutingPolicy.MaxTTFTMS)
	assert.True(t, agent.RoutingPolicy.RequireFreshMetadata)
}

func TestRegistry_NormalizesAndDeduplicatesPhrases(t *testing.T) {
	t.Parallel()

	registry := NewRegistry(map[string]config.AgentConfig{
		"reviewer": {
			Capabilities: []string{" Review ", "review", "SECURITY", "security"},
			Triggers:     []string{" Review this ", "review this"},
			RoutingPolicy: config.RoutingPolicyConfig{
				PreferredProviders:   []string{"OpenAI", "openai"},
				BannedProviders:      []string{"Ollama", "ollama"},
				RequiredCapabilities: []string{"Tools", "tools"},
			},
		},
	})

	reviewer, ok := registry.Get("reviewer")
	require.True(t, ok)

	assert.Equal(t, []string{"review", "security"}, reviewer.Capabilities)
	assert.Equal(t, []string{"review this"}, reviewer.Triggers)
	assert.Equal(t, []string{"openai"}, reviewer.RoutingPolicy.PreferredProviders)
	assert.Equal(t, []string{"ollama"}, reviewer.RoutingPolicy.BannedProviders)
	assert.Equal(t, []string{"tools"}, reviewer.RoutingPolicy.RequiredCapabilities)
}

func TestRegistry_MatchPrompt(t *testing.T) {
	t.Parallel()

	registry := NewRegistry(map[string]config.AgentConfig{
		reviewerAgentName: {Capabilities: []string{"security"}, Triggers: []string{"review this", "code review"}},
		"writer":          {Triggers: []string{"write docs"}},
	})

	agent, ok := registry.MatchPrompt("Please REVIEW THIS change")
	if !ok {
		require.FailNow(t, "expected trigger match")
	}

	if agent.Name != reviewerAgentName {
		assert.Failf(t, "assertion failed", "agent = %q, want reviewer", agent.Name)
	}

	match, ok := registry.MatchPromptWithReason("Please check SECURITY")
	if !ok {
		require.FailNow(t, "expected capability match")
	}

	if match.Agent.Name != reviewerAgentName || match.Kind != "capability" || match.Pattern != "security" {
		assert.Failf(t, "assertion failed", "match = %+v", match)
	}

	if _, ok := registry.MatchPrompt("summarize this"); ok {
		require.FailNow(t, "expected no trigger match")
	}
}

func TestRegistry_MatchPromptWithReasonReportsAmbiguity(t *testing.T) {
	t.Parallel()

	registry := NewRegistry(map[string]config.AgentConfig{
		"auth-a": {Triggers: []string{"auth"}},
		"auth-b": {Triggers: []string{"auth"}},
	})

	match, ok := registry.MatchPromptWithReason("review auth permissions")
	require.True(t, ok)

	assert.Equal(t, "auth-a", match.Agent.Name)
	require.NotEmpty(t, match.Ambiguities)
	assert.Equal(t, "security", match.Ambiguities[0].Role)
	assert.Len(t, match.Ambiguities[0].Candidates, 2)
}

func TestRegistry_MatchPromptRejectsAmbiguousMatch(t *testing.T) {
	t.Parallel()

	registry := NewRegistry(map[string]config.AgentConfig{
		"auth-a": {Triggers: []string{"auth"}},
		"auth-b": {Triggers: []string{"auth"}},
	})

	_, ok := registry.MatchPrompt("review auth permissions")

	assert.False(t, ok)
}

func TestAgent_CompleteParams(t *testing.T) {
	t.Parallel()

	temp := 0.2
	topP := 0.9
	seed := 11
	agent := Agent{
		Name:           reviewerAgentName,
		Model:          "gpt-4.1",
		SystemPrompt:   "Review code.",
		Temperature:    &temp,
		TopP:           &topP,
		Seed:           &seed,
		ModelMode:      "fast",
		ReasoningLevel: "high",
		MaxTokens:      100,
	}

	params := agent.CompleteParams("", []llm.Message{{Role: llm.RoleUser, Content: "diff"}})

	if params.Model != "gpt-4.1" {
		assert.Failf(t, "assertion failed", "Model = %q", params.Model)
	}

	if len(params.Messages) != 2 || params.Messages[0].Role != llm.RoleSystem {
		require.Failf(t, "unexpected failure", "messages = %+v", params.Messages)
	}

	if params.Temperature == nil || *params.Temperature != temp {
		assert.Failf(t, "assertion failed", "Temperature = %v", params.Temperature)
	}

	if params.TopP == nil || *params.TopP != topP {
		assert.Failf(t, "assertion failed", "TopP = %v", params.TopP)
	}

	if params.Seed == nil || *params.Seed != seed {
		assert.Failf(t, "assertion failed", "Seed = %v", params.Seed)
	}

	if params.ReasoningLevel != "high" {
		assert.Failf(t, "assertion failed", "ReasoningLevel = %q", params.ReasoningLevel)
	}

	assert.Equal(t, "fast", params.ModelMode)

	if params.MaxTokens != 100 {
		assert.Failf(t, "assertion failed", "MaxTokens = %d", params.MaxTokens)
	}
}

func TestParseInvocation(t *testing.T) {
	t.Parallel()

	name, prompt, ok := ParseInvocation("@reviewer check this")
	if !ok || name != "reviewer" || prompt != "check this" {
		require.Failf(t, "unexpected failure", "ParseInvocation = %q %q %v", name, prompt, ok)
	}

	_, _, ok = ParseInvocation("review this")
	if ok {
		require.FailNow(t, "expected no invocation")
	}
}

func TestRegistry_ReferencesFromConfig(t *testing.T) {
	t.Parallel()

	registry := NewRegistry(map[string]config.AgentConfig{
		"reviewer": {
			Description: "Reviews code",
			References:  []string{"./docs/guide.md", "https://example.com/style-guide"},
		},
		"writer": {
			Model: "gpt-4.1",
		},
	})

	reviewer, ok := registry.Get("reviewer")
	require.True(t, ok)
	assert.Equal(t, []string{"./docs/guide.md", "https://example.com/style-guide"}, reviewer.References)

	writer, ok := registry.Get("writer")
	require.True(t, ok)
	assert.Empty(t, writer.References)
}

func TestRegistry_RendersApprovedFeedbackGuidanceAtRuntime(t *testing.T) {
	t.Parallel()

	registry := NewRegistry(map[string]config.AgentConfig{
		"reviewer": {
			SystemPrompt: "Review code.",
			FeedbackGuidance: []config.FeedbackGuidance{
				{
					ID:         "fg-runtime",
					Status:     feedback.GuidanceStatusApproved,
					SourceRun:  "run-123",
					Reviewer:   "alice",
					Action:     "Always run focused regression tests.",
					Reason:     "Previous review missed a regression.",
					Evidence:   []string{"evaluation: fail"},
					Confidence: 0.8,
					CreatedAt:  time.Date(2026, 5, 21, 10, 0, 0, 0, time.UTC),
					UpdatedAt:  time.Date(2026, 5, 21, 10, 0, 0, 0, time.UTC),
					Audit: []config.FeedbackGuidanceAuditEvent{{
						At:     time.Date(2026, 5, 21, 10, 0, 0, 0, time.UTC),
						Actor:  "alice",
						Action: feedback.GuidanceStatusApproved,
					}},
				},
				{
					ID:        "fg-pending",
					Status:    feedback.GuidanceStatusPending,
					SourceRun: "run-456",
					Reviewer:  "alice",
					Action:    "Always run pending checks.",
					Reason:    "Not approved.",
					Evidence:  []string{"evaluation: fail"},
					CreatedAt: time.Date(2026, 5, 21, 9, 0, 0, 0, time.UTC),
					UpdatedAt: time.Date(2026, 5, 21, 9, 0, 0, 0, time.UTC),
					Audit: []config.FeedbackGuidanceAuditEvent{{
						At:     time.Date(2026, 5, 21, 9, 0, 0, 0, time.UTC),
						Actor:  "alice",
						Action: feedback.GuidanceStatusPending,
					}},
				},
			},
		},
	})

	reviewer, ok := registry.Get("reviewer")
	require.True(t, ok)
	assert.Contains(t, reviewer.SystemPrompt, "Review code.")
	assert.Contains(t, reviewer.SystemPrompt, "Feedback-derived guidance:")
	assert.Contains(t, reviewer.SystemPrompt, "Always run focused regression tests.")
	assert.Contains(t, reviewer.SystemPrompt, "Source run: run-123")
	assert.NotContains(t, reviewer.SystemPrompt, "pending checks")
}

func TestAgent_HasToolPermission_NilPermissions(t *testing.T) {
	t.Parallel()

	a := Agent{Name: "default"}
	assert.True(t, a.HasToolPermission("bash"), "nil ToolPermissions should allow all tools")
	assert.True(t, a.HasToolPermission("write"), "nil ToolPermissions should allow all tools")
	assert.True(t, a.HasToolPermission("anything"), "nil ToolPermissions should allow all tools")
}

func TestAgent_HasToolPermission_ExplicitPermissions(t *testing.T) {
	t.Parallel()

	a := Agent{
		Name:            "restricted",
		ToolPermissions: map[string]bool{"bash": true, "read": true, "write": false},
	}

	assert.True(t, a.HasToolPermission("bash"))
	assert.True(t, a.HasToolPermission(" Bash "))
	assert.True(t, a.HasToolPermission("READ"))
	assert.True(t, a.HasToolPermission("read"))
	assert.False(t, a.HasToolPermission(" Write "), "explicitly false should deny after normalization")
	assert.False(t, a.HasToolPermission("write"), "explicitly false should deny")
	assert.False(t, a.HasToolPermission("edit"), "missing key should deny")
	assert.False(t, a.HasToolPermission(" "), "blank tool name should deny")
}

func TestAgent_FilterTools_NilPermissions(t *testing.T) {
	t.Parallel()

	a := Agent{Name: "default"}
	tools := llm.DefaultTools()
	filtered := a.FilterTools(tools)
	assert.Equal(t, tools, filtered, "nil ToolPermissions should return all tools")
}

func TestAgent_FilterTools_ExplicitPermissions(t *testing.T) {
	t.Parallel()

	a := Agent{
		Name:            "restricted",
		ToolPermissions: map[string]bool{"bash": true},
	}
	tools := []llm.ToolDefinition{
		{Name: " Bash ", Description: "Run commands"},
		{Name: "write", Description: "Write files"},
		{Name: "read", Description: "Read files"},
	}

	filtered := a.FilterTools(tools)
	require.Len(t, filtered, 1)
	assert.Equal(t, " Bash ", filtered[0].Name)
}

func TestAgent_FilterTools_EmptyPermissions(t *testing.T) {
	t.Parallel()

	a := Agent{
		Name:            "no-tools",
		ToolPermissions: map[string]bool{},
	}
	tools := llm.DefaultTools()
	filtered := a.FilterTools(tools)
	assert.Empty(t, filtered, "empty map should deny all tools")
}

func TestRegistry_ModeAndToolPermissions(t *testing.T) {
	t.Parallel()

	registry := NewRegistry(map[string]config.AgentConfig{
		"cr": {
			Description: "Code review agent",
			Mode:        "subagent",
			ToolPermissions: map[string]bool{
				" Bash ": true,
				"READ":   true,
				"write":  true,
				" EDIT ": true,
				"edit":   false,
				" ":      true,
			},
		},
		"simple": {
			Description: "No tools configured",
		},
	})

	cr, ok := registry.Get("cr")
	require.True(t, ok)
	assert.Equal(t, "subagent", cr.Mode)
	assert.True(t, cr.HasToolPermission("bash"))
	assert.True(t, cr.HasToolPermission("read"))
	assert.True(t, cr.HasToolPermission("write"))
	assert.False(t, cr.HasToolPermission("edit"))
	assert.NotContains(t, cr.ToolPermissions, "")

	simple, ok := registry.Get("simple")
	require.True(t, ok)
	assert.Empty(t, simple.Mode)
	assert.Nil(t, simple.ToolPermissions)
	assert.True(t, simple.HasToolPermission("bash"), "nil map should allow all")
}
