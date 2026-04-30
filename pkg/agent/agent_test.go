package agent

import (
	"reflect"
	"testing"

	"github.com/tommoulard/atteler/pkg/config"
	"github.com/tommoulard/atteler/pkg/llm"
)

func TestRegistry_GetAndList(t *testing.T) {
	registry := NewRegistry(map[string]config.AgentConfig{
		"reviewer": {
			Model:          "gpt-4.1",
			FallbackModels: []string{"gpt-4.1-mini"},
			Triggers:       []string{"review this"},
		},
		"writer": {Model: "claude-sonnet-4-20250514"},
	})

	names := registry.List()
	if !reflect.DeepEqual(names, []string{"reviewer", "writer"}) {
		t.Fatalf("names = %v", names)
	}

	agent, ok := registry.Get("reviewer")
	if !ok {
		t.Fatal("expected reviewer agent")
	}
	if agent.Model != "gpt-4.1" {
		t.Errorf("model = %q", agent.Model)
	}
	if !reflect.DeepEqual(agent.Triggers, []string{"review this"}) {
		t.Errorf("triggers = %v", agent.Triggers)
	}
	if !reflect.DeepEqual(agent.ModelChain(), []string{"gpt-4.1", "gpt-4.1-mini"}) {
		t.Errorf("model chain = %v", agent.ModelChain())
	}
}

func TestRegistry_MatchPrompt(t *testing.T) {
	registry := NewRegistry(map[string]config.AgentConfig{
		"reviewer": {Triggers: []string{"review this", "code review"}},
		"writer":   {Triggers: []string{"write docs"}},
	})

	agent, ok := registry.MatchPrompt("Please REVIEW THIS change")
	if !ok {
		t.Fatal("expected trigger match")
	}
	if agent.Name != "reviewer" {
		t.Errorf("agent = %q, want reviewer", agent.Name)
	}

	if _, ok := registry.MatchPrompt("summarize this"); ok {
		t.Fatal("expected no trigger match")
	}
}

func TestAgent_CompleteParams(t *testing.T) {
	temp := 0.2
	topP := 0.9
	agent := Agent{
		Name:         "reviewer",
		Model:        "gpt-4.1",
		SystemPrompt: "Review code.",
		Temperature:  &temp,
		TopP:         &topP,
		MaxTokens:    100,
	}

	params := agent.CompleteParams("", []llm.Message{{Role: llm.RoleUser, Content: "diff"}})

	if params.Model != "gpt-4.1" {
		t.Errorf("Model = %q", params.Model)
	}
	if len(params.Messages) != 2 || params.Messages[0].Role != llm.RoleSystem {
		t.Fatalf("messages = %+v", params.Messages)
	}
	if params.Temperature == nil || *params.Temperature != temp {
		t.Errorf("Temperature = %v", params.Temperature)
	}
	if params.TopP == nil || *params.TopP != topP {
		t.Errorf("TopP = %v", params.TopP)
	}
	if params.MaxTokens != 100 {
		t.Errorf("MaxTokens = %d", params.MaxTokens)
	}
}

func TestParseInvocation(t *testing.T) {
	name, prompt, ok := ParseInvocation("@reviewer check this")
	if !ok || name != "reviewer" || prompt != "check this" {
		t.Fatalf("ParseInvocation = %q %q %v", name, prompt, ok)
	}

	_, _, ok = ParseInvocation("review this")
	if ok {
		t.Fatal("expected no invocation")
	}
}
