// Package agent defines config-backed atteler agent personas.
package agent

import (
	"sort"
	"strings"

	"github.com/tommoulard/atteler/pkg/config"
	"github.com/tommoulard/atteler/pkg/llm"
)

// Agent is a named LLM persona with optional model and generation knobs.
type Agent struct {
	Temperature    *float64
	TopP           *float64
	Name           string
	Model          string
	SystemPrompt   string
	FallbackModels []string
	Triggers       []string
	MaxTokens      int
}

// Registry stores agents by name.
type Registry struct {
	agents map[string]Agent
}

// NewRegistry builds an agent registry from configuration.
func NewRegistry(configs map[string]config.AgentConfig) *Registry {
	registry := &Registry{agents: make(map[string]Agent, len(configs))}
	for name, cfg := range configs {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		registry.agents[name] = Agent{
			Name:           name,
			Model:          cfg.Model,
			SystemPrompt:   cfg.SystemPrompt,
			FallbackModels: normalizeModels(cfg.FallbackModels),
			Temperature:    cfg.Temperature,
			TopP:           cfg.TopP,
			Triggers:       normalizeTriggers(cfg.Triggers),
			MaxTokens:      cfg.MaxTokens,
		}
	}
	return registry
}

// ModelChain returns the ordered model preference chain for this agent.
func (a Agent) ModelChain() []string {
	return modelChain(a.Model, a.FallbackModels)
}

// Get returns a named agent.
func (r *Registry) Get(name string) (Agent, bool) {
	if r == nil {
		return Agent{}, false
	}
	agent, ok := r.agents[name]
	return agent, ok
}

// List returns sorted agent names.
func (r *Registry) List() []string {
	if r == nil {
		return nil
	}

	names := make([]string, 0, len(r.agents))
	for name := range r.agents {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// MatchPrompt returns the first agent whose configured trigger appears in
// prompt. Matching is case-insensitive and stable by sorted agent name.
func (r *Registry) MatchPrompt(prompt string) (Agent, bool) {
	if r == nil {
		return Agent{}, false
	}

	prompt = strings.ToLower(prompt)
	for _, name := range r.List() {
		agent := r.agents[name]
		for _, trigger := range agent.Triggers {
			if trigger != "" && strings.Contains(prompt, trigger) {
				return agent, true
			}
		}
	}
	return Agent{}, false
}

// CompleteParams applies the agent persona to an LLM completion request.
func (a Agent) CompleteParams(model string, messages []llm.Message) llm.CompleteParams {
	if model == "" {
		model = a.Model
	}

	requestMessages := append([]llm.Message(nil), messages...)
	if a.SystemPrompt != "" {
		requestMessages = append([]llm.Message{{Role: llm.RoleSystem, Content: a.SystemPrompt}}, requestMessages...)
	}

	params := llm.CompleteParams{
		Model:       model,
		Messages:    requestMessages,
		MaxTokens:   a.MaxTokens,
		Temperature: a.Temperature,
		TopP:        a.TopP,
	}
	return params
}

func normalizeTriggers(triggers []string) []string {
	out := make([]string, 0, len(triggers))
	for _, trigger := range triggers {
		trigger = strings.ToLower(strings.TrimSpace(trigger))
		if trigger != "" {
			out = append(out, trigger)
		}
	}
	return out
}

func normalizeModels(models []string) []string {
	out := make([]string, 0, len(models))
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model != "" {
			out = append(out, model)
		}
	}
	return out
}

func modelChain(primary string, fallbacks []string) []string {
	var out []string
	seen := make(map[string]bool, len(fallbacks)+1)
	for _, model := range append([]string{primary}, fallbacks...) {
		model = strings.TrimSpace(model)
		if model != "" && !seen[model] {
			out = append(out, model)
			seen[model] = true
		}
	}
	return out
}

// ParseInvocation extracts an @agent prefix from user input.
func ParseInvocation(input string) (name, prompt string, ok bool) {
	trimmed := strings.TrimSpace(input)
	if !strings.HasPrefix(trimmed, "@") {
		return "", input, false
	}

	withoutAt := strings.TrimPrefix(trimmed, "@")
	name, prompt, hasPrompt := strings.Cut(withoutAt, " ")
	name = strings.TrimSpace(name)
	if name == "" {
		return "", input, false
	}
	if !hasPrompt {
		return name, "", true
	}
	return name, strings.TrimSpace(prompt), true
}
