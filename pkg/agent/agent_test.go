package agent

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/config"
	"github.com/tommoulard/atteler/pkg/llm"
)

const reviewerAgentName = "reviewer"

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
