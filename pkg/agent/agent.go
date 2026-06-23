// Package agent defines config-backed atteler agent personas.
package agent

import (
	"context"
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
	ToolPolicy      string
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
			ToolPolicy:      normalizeToolPolicy(cfg.ToolPolicy),
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
		MaxLatencyMS:         cfg.MaxLatencyMS,
		MaxTTFTMS:            cfg.MaxTTFTMS,
		RequireFreshMetadata: cfg.RequireFreshMetadata,
	}
}

// ModelChain returns the ordered model preference chain for this agent.
func (a Agent) ModelChain() []string {
	return modelChain(a.Model, a.FallbackModels)
}

const (
	// ToolPolicyDeny is the default tool policy: omitted tools deny every tool.
	ToolPolicyDeny = "deny"
	// ToolPolicyAllowAll is an explicit compatibility mode for legacy configs
	// that intentionally want every advertised tool, including future tools.
	ToolPolicyAllowAll = "allow-all"
)

func normalizeToolPolicy(policy string) string {
	policy = strings.ToLower(strings.TrimSpace(policy))
	policy = strings.ReplaceAll(policy, "_", "-")

	switch policy {
	case "", ToolPolicyDeny, "deny-all", "default":
		return ToolPolicyDeny
	case ToolPolicyAllowAll, "allow", "all", "compat", "compatibility", "legacy":
		return ToolPolicyAllowAll
	default:
		return policy
	}
}

// HasToolPermission reports whether the agent is allowed to use the named tool.
// Omitted ToolPermissions are deny-by-default unless ToolPolicy is explicitly
// set to allow-all compatibility mode. ToolPermissions may contain either raw
// tool names or capability-scoped grants such as read, search, shell.readonly,
// shell.write, network, and filesystem.write.
func (a Agent) HasToolPermission(tool string) bool {
	tool = normalizeToolName(tool)
	if tool == "" {
		return false
	}

	if normalizeToolPolicy(a.ToolPolicy) == ToolPolicyAllowAll {
		return !toolDeniedByPermissions(tool, a.ToolPermissions)
	}

	return toolAllowedByPermissions(tool, a.ToolPermissions)
}

func toolAllowedByPermissions(tool string, permissions map[string]bool) bool {
	if len(permissions) == 0 {
		return false
	}

	if allowed, ok := permissions[tool]; ok {
		return allowed
	}

	for capability, allowed := range permissions {
		if !allowed {
			continue
		}

		if capabilityGrantsTool(capability, tool) {
			return true
		}
	}

	return false
}

func toolDeniedByPermissions(tool string, permissions map[string]bool) bool {
	if len(permissions) == 0 {
		return false
	}

	if allowed, ok := permissions[tool]; ok {
		return !allowed
	}

	for capability, allowed := range permissions {
		if allowed {
			continue
		}

		if capabilityGrantsTool(capability, tool) {
			return true
		}
	}

	return false
}

func capabilityGrantsTool(capability, tool string) bool {
	switch normalizeToolName(capability) {
	case "read":
		return tool == llm.ToolNameRead || tool == llm.ToolNameGlob || tool == llm.ToolNameGrep
	case "search":
		return tool == llm.ToolNameGlob || tool == llm.ToolNameGrep
	case "shell.readonly", "shell.read-only", "shell.read":
		return tool == llm.ToolNameBash
	case "shell.write", "shell":
		return tool == llm.ToolNameBash
	case "filesystem.write", "filesystem", "fs.write":
		return tool == llm.ToolNameWrite || tool == llm.ToolNameEdit
	case "network":
		return tool == "network" || tool == "web" || tool == "fetch"
	default:
		return false
	}
}

// FilterTools returns only the tools the agent is permitted to use. Omitted
// permissions deny every tool unless ToolPolicy is explicit allow-all.
func (a Agent) FilterTools(tools []llm.ToolDefinition) []llm.ToolDefinition {
	filtered := make([]llm.ToolDefinition, 0, len(tools))
	for _, tool := range tools {
		if a.HasToolPermission(tool.Name) {
			filtered = append(filtered, tool)
		}
	}

	return filtered
}

// RuntimeToolPolicy wraps next with agent-level capability enforcement for tool calls.
// Filtering advertised tools catches most cases before the model can call them;
// this runtime guard preserves capability scope for coarse tools like bash where
// shell.readonly and shell.write share the same underlying tool name.
func (a Agent) RuntimeToolPolicy(next llm.ToolPolicy) llm.ToolPolicy {
	if next == nil {
		next = func(_ context.Context, _ llm.ToolCall, _ llm.AgentLoopBudgetSnapshot) llm.ToolPolicyDecision {
			return llm.ToolPolicyDecision{
				Verdict:     llm.ToolPolicyAllow,
				Reason:      "tool passed default allow policy",
				MatchedRule: "agent.default_allow",
			}
		}
	}

	return func(ctx context.Context, call llm.ToolCall, budget llm.AgentLoopBudgetSnapshot) llm.ToolPolicyDecision {
		if !a.HasToolPermission(call.Name) {
			return llm.ToolPolicyDecision{
				Verdict:     llm.ToolPolicyDeny,
				Reason:      "agent tool policy does not grant " + normalizeToolName(call.Name),
				MatchedRule: "agent.tool.deny",
			}
		}

		if decision, ok := a.shellReadOnlyDecision(call); ok {
			return decision
		}

		if decision, ok := a.networkDecision(call); ok {
			return decision
		}

		return next(ctx, call, budget)
	}
}

func (a Agent) shellReadOnlyDecision(call llm.ToolCall) (llm.ToolPolicyDecision, bool) {
	if normalizeToolName(call.Name) != llm.ToolNameBash {
		return llm.ToolPolicyDecision{}, false
	}

	if normalizeToolPolicy(a.ToolPolicy) == ToolPolicyAllowAll ||
		!permissionMapAllows(a.ToolPermissions, "shell.readonly", "shell.read-only", "shell.read") ||
		permissionMapAllows(a.ToolPermissions, "shell.write", "shell", llm.ToolNameBash) {
		return llm.ToolPolicyDecision{}, false
	}

	command, ok := call.Input["command"].(string)
	if !ok || !llm.BashCommandRequiresWrite(command) {
		return llm.ToolPolicyDecision{}, false
	}

	return llm.ToolPolicyDecision{
		Verdict:     llm.ToolPolicyDeny,
		Reason:      "agent shell.readonly grant does not allow mutating shell commands",
		MatchedRule: "agent.shell.readonly",
	}, true
}

func (a Agent) networkDecision(call llm.ToolCall) (llm.ToolPolicyDecision, bool) {
	if normalizeToolName(call.Name) != llm.ToolNameBash {
		return llm.ToolPolicyDecision{}, false
	}

	if normalizeToolPolicy(a.ToolPolicy) == ToolPolicyAllowAll && !permissionMapDenies(a.ToolPermissions, "network") {
		return llm.ToolPolicyDecision{}, false
	}

	if permissionMapAllows(a.ToolPermissions, "network") {
		return llm.ToolPolicyDecision{}, false
	}

	command, ok := call.Input["command"].(string)
	if !ok || !llm.BashCommandRequiresNetwork(command) {
		return llm.ToolPolicyDecision{}, false
	}

	return llm.ToolPolicyDecision{
		Verdict:     llm.ToolPolicyDeny,
		Reason:      "agent tool policy does not grant network capability",
		MatchedRule: "agent.network.deny",
	}, true
}

func permissionMapAllows(permissions map[string]bool, names ...string) bool {
	if len(permissions) == 0 || len(names) == 0 {
		return false
	}

	allowedNames := make(map[string]struct{}, len(names))
	for _, name := range names {
		allowedNames[normalizeToolName(name)] = struct{}{}
	}

	for name, allowed := range permissions {
		if !allowed {
			continue
		}

		if _, ok := allowedNames[normalizeToolName(name)]; ok {
			return true
		}
	}

	return false
}

func permissionMapDenies(permissions map[string]bool, names ...string) bool {
	if len(permissions) == 0 || len(names) == 0 {
		return false
	}

	deniedNames := make(map[string]struct{}, len(names))
	for _, name := range names {
		deniedNames[normalizeToolName(name)] = struct{}{}
	}

	for name, allowed := range permissions {
		if allowed {
			continue
		}

		if _, ok := deniedNames[normalizeToolName(name)]; ok {
			return true
		}
	}

	return false
}

// EffectiveToolNames returns sorted tool names from tools that the agent may use.
func (a Agent) EffectiveToolNames(tools []llm.ToolDefinition) []string {
	filtered := a.FilterTools(tools)
	names := make([]string, 0, len(filtered))

	for _, tool := range filtered {
		name := normalizeToolName(tool.Name)
		if name != "" {
			names = append(names, name)
		}
	}

	sort.Strings(names)

	return names
}

// EffectivePermissionNames returns sorted configured capability/tool grants that
// are allowed for this agent. It intentionally reports capability names (for
// example shell.readonly) separately from EffectiveToolNames so humans can see
// the scoped grant that caused a raw tool such as bash to be advertised.
func (a Agent) EffectivePermissionNames() []string {
	if normalizeToolPolicy(a.ToolPolicy) == ToolPolicyAllowAll {
		return []string{ToolPolicyAllowAll}
	}

	if len(a.ToolPermissions) == 0 {
		return nil
	}

	names := make([]string, 0, len(a.ToolPermissions))

	for name, allowed := range a.ToolPermissions {
		name = normalizeToolName(name)
		if name != "" && allowed {
			names = append(names, name)
		}
	}

	sort.Strings(names)

	return names
}

// ToolPolicySummary returns a compact user-facing summary of the effective tool policy.
func (a Agent) ToolPolicySummary() string {
	if normalizeToolPolicy(a.ToolPolicy) == ToolPolicyAllowAll {
		return ToolPolicyAllowAll
	}

	return ToolPolicyDeny
}

// Upsert inserts or replaces an agent by name. It is used to merge built-in
// personas (such as the auto-mode orchestrator and its workers) into a registry
// loaded from config. A trimmed-empty name is ignored.
func (r *Registry) Upsert(a Agent) {
	if r == nil {
		return
	}

	a.Name = strings.TrimSpace(a.Name)
	if a.Name == "" {
		return
	}

	if r.agents == nil {
		r.agents = make(map[string]Agent, 1)
	}

	r.agents[a.Name] = a
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
