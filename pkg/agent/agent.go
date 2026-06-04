// Package agent defines config-backed atteler agent personas.
package agent

import (
	"sort"
	"strings"
	"time"

	"github.com/tommoulard/atteler/pkg/config"
	"github.com/tommoulard/atteler/pkg/feedback"
	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/modelroute"
)

// Agent is a named LLM persona with optional model and generation knobs.
//
//nolint:govet // Field order follows persona/config grouping.
type Agent struct {
	Temperature     *float64
	TopP            *float64
	Seed            *int
	ToolPermissions map[string]bool
	RoutingPolicy   modelroute.Policy
	Name            string
	Model           string
	Mode            string
	ModelMode       string
	Description     string
	Personality     string
	SystemPrompt    string
	ReasoningLevel  string
	FallbackModels  []string
	Capabilities    []string
	Triggers        []string
	References      []string
	MaxTokens       int
	Hidden          bool
}

// Registry stores agents by name.
type Registry struct {
	agents map[string]Agent
}

// NewRegistry builds an agent registry from configuration.
func NewRegistry(configs map[string]config.AgentConfig) *Registry {
	registry := &Registry{agents: make(map[string]Agent, len(configs))}
	now := time.Now().UTC()

	for name := range configs {
		cfg := configs[name]

		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}

		registry.agents[name] = Agent{
			Name:            name,
			Model:           cfg.Model,
			Mode:            strings.TrimSpace(cfg.Mode),
			ModelMode:       strings.TrimSpace(cfg.ModelMode),
			ToolPermissions: cloneToolPermissions(cfg.ToolPermissions),
			RoutingPolicy:   routingPolicyFromConfig(cfg.RoutingPolicy),
			Description:     strings.TrimSpace(cfg.Description),
			Personality:     strings.TrimSpace(cfg.Personality),
			SystemPrompt:    feedback.RenderSystemPrompt(cfg.SystemPrompt, cfg.FeedbackGuidance, now),
			ReasoningLevel:  strings.TrimSpace(cfg.ReasoningLevel),
			FallbackModels:  normalizeModels(cfg.FallbackModels),
			Capabilities:    normalizePhrases(cfg.Capabilities),
			Temperature:     cfg.Temperature,
			TopP:            cfg.TopP,
			Seed:            cfg.Seed,
			Triggers:        normalizePhrases(cfg.Triggers),
			References:      append([]string(nil), cfg.References...),
			MaxTokens:       cfg.MaxTokens,
			Hidden:          cfg.Hidden,
		}
	}

	return registry
}

func cloneToolPermissions(in map[string]bool) map[string]bool {
	if in == nil {
		return nil
	}

	out := make(map[string]bool, len(in))
	for tool, allowed := range in {
		tool = normalizeToolName(tool)
		if tool == "" {
			continue
		}

		if !allowed {
			out[tool] = false

			continue
		}

		if _, ok := out[tool]; !ok {
			out[tool] = true
		}
	}

	return out
}

func routingPolicyFromConfig(cfg config.RoutingPolicyConfig) modelroute.Policy {
	return modelroute.Policy{
		PreferredProviders:   normalizePhrases(cfg.PreferredProviders),
		BannedProviders:      normalizePhrases(cfg.BannedProviders),
		BannedModels:         normalizeModels(cfg.BannedModels),
		RequiredCapabilities: normalizePhrases(cfg.RequiredCapabilities),
		MaxBudget:            cfg.MaxBudget,
		RequireFreshMetadata: cfg.RequireFreshMetadata,
	}
}

// ModelChain returns the ordered model preference chain for this agent.
func (a Agent) ModelChain() []string {
	return modelChain(a.Model, a.FallbackModels)
}

// HasToolPermission reports whether the agent is allowed to use the named tool.
// When ToolPermissions is nil (not configured), all tools are permitted.
// When ToolPermissions is non-nil, only tools explicitly set to true are allowed.
func (a Agent) HasToolPermission(tool string) bool {
	if a.ToolPermissions == nil {
		return true
	}

	return a.ToolPermissions[normalizeToolName(tool)]
}

// FilterTools returns only the tools the agent is permitted to use.
// When ToolPermissions is nil, all tools pass through unchanged.
func (a Agent) FilterTools(tools []llm.ToolDefinition) []llm.ToolDefinition {
	if a.ToolPermissions == nil {
		return tools
	}

	filtered := make([]llm.ToolDefinition, 0, len(tools))
	for _, tool := range tools {
		if a.HasToolPermission(tool.Name) {
			filtered = append(filtered, tool)
		}
	}

	return filtered
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
		agent := r.agents[name]
		if agent.Hidden {
			continue
		}

		names = append(names, name)
	}

	sort.Strings(names)

	return names
}

// MatchPrompt returns the highest-scoring unambiguous agent for prompt.
func (r *Registry) MatchPrompt(prompt string) (Agent, bool) {
	match, ok := r.MatchPromptWithReason(prompt)
	if !ok || len(match.Ambiguities) > 0 {
		return Agent{}, false
	}

	return match.Agent, true
}

// Match identifies why a prompt matched an agent.
//
//nolint:govet // Field order groups the matched agent before reason metadata.
type Match struct {
	Agent       Agent
	Kind        string
	Pattern     string
	Evidence    []MatchEvidence
	Ambiguities []Ambiguity
	Score       float64
}

// MatchPromptWithReason returns the highest-scoring prompt match with the
// evidence that made the selected agent win.
func (r *Registry) MatchPromptWithReason(prompt string) (Match, bool) {
	if r == nil {
		return Match{}, false
	}

	plan, err := r.PlanOrchestration(OrchestrationRequest{Prompt: prompt, MaxParticipants: 1})
	if err != nil || len(plan.Participants) == 0 {
		return Match{}, false
	}

	participant := plan.Participants[0]

	return Match{
		Agent:       participant.Agent,
		Kind:        participant.Source,
		Pattern:     participant.Pattern,
		Evidence:    append([]MatchEvidence(nil), participant.Evidence...),
		Ambiguities: append([]Ambiguity(nil), plan.Ambiguities...),
		Score:       participant.Score,
	}, true
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
		Model:          model,
		Messages:       requestMessages,
		MaxTokens:      a.MaxTokens,
		Temperature:    a.Temperature,
		TopP:           a.TopP,
		Seed:           a.Seed,
		ModelMode:      a.ModelMode,
		ReasoningLevel: a.ReasoningLevel,
	}

	return params
}

func normalizePhrases(phrases []string) []string {
	out := make([]string, 0, len(phrases))
	seen := make(map[string]bool, len(phrases))

	for _, phrase := range phrases {
		phrase = strings.ToLower(strings.TrimSpace(phrase))
		if phrase != "" && !seen[phrase] {
			seen[phrase] = true
			out = append(out, phrase)
		}
	}

	return out
}

func normalizeToolName(tool string) string {
	return strings.ToLower(strings.TrimSpace(tool))
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
