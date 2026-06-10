package llm

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/tommoulard/atteler/pkg/modelroute"
)

// ModelRole describes a task-oriented model alias such as "planner" or
// "fast_coder". It keeps user intent separate from concrete provider/model IDs
// while still preserving backwards-compatible direct model selection.
//
//nolint:govet // Field order follows config and route-policy grouping.
type ModelRole struct {
	RoutingPolicy        modelroute.Policy
	Preferred            string
	FallbackModels       []string
	PreferredProviders   []string
	BannedProviders      []string
	BannedModels         []string
	RequiredCapabilities []string
	MaxCostUSD           float64
	MaxLatencyMS         int
	MaxTTFTMS            int
	RequireFreshMetadata bool
	PreferLocal          bool
}

// ModelRoleResolution records how a role resolved to a concrete fallback
// chain. Decision contains the inspectable modelroute artifact with candidate
// rankings, constraints, estimated cost, telemetry, and availability evidence.
type ModelRoleResolution struct {
	Decision       modelroute.Decision
	Role           string
	SelectedModel  string
	FallbackModels []string
}

// SetModelRole configures a task-oriented model role. Role names are bare
// identifiers so provider/model keeps its existing meaning as a concrete model
// selector.
func (r *Registry) SetModelRole(name string, role ModelRole) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("llm: model role name cannot be empty")
	}

	if strings.Contains(name, "/") {
		return fmt.Errorf("llm: model role %q must be a bare name", name)
	}

	role = normalizeModelRole(role)
	if len(modelFallbackChain(role.Preferred, role.FallbackModels)) == 0 {
		return fmt.Errorf("llm: model role %q needs a preferred model or fallback model", name)
	}

	if err := validateModelRoleLimits(name, role); err != nil {
		return err
	}

	if err := validateModelRoleCapabilities(name, role); err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.modelRoles == nil {
		r.modelRoles = make(map[string]ModelRole)
	}

	r.modelRoles[name] = role

	return nil
}

// ModelRole returns a copy of a configured model role.
func (r *Registry) ModelRole(name string) (ModelRole, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	role, ok := r.modelRoles[strings.TrimSpace(name)]
	if !ok {
		return ModelRole{}, false
	}

	return cloneModelRole(role), true
}

// ModelRoleForRequest returns the configured model role that would apply to a
// request model. An empty request model resolves to the configured default
// model when that default is a role.
func (r *Registry) ModelRoleForRequest(name string) (string, ModelRole, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	roleName := strings.TrimSpace(name)
	if roleName == "" {
		roleName = r.defaultModelRoleNameLocked()
	}

	role, ok := r.modelRoles[roleName]
	if !ok {
		return "", ModelRole{}, false
	}

	return roleName, cloneModelRole(role), true
}

// ListModelRoles returns configured role names in stable order.
func (r *Registry) ListModelRoles() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	names := make([]string, 0, len(r.modelRoles))
	for name := range r.modelRoles {
		names = append(names, name)
	}

	sort.Strings(names)

	return names
}

// ResolveModelRole resolves a configured role to the concrete model and
// fallback chain that should be used for params. Additional fallbackModels are
// appended after the role's own fallbacks.
func (r *Registry) ResolveModelRole(
	roleName string,
	params CompleteParams,
	fallbackModels []string,
) (ModelRoleResolution, bool, error) {
	return r.resolveModelRoleWithProfileAndCapabilities(
		roleName,
		params,
		fallbackModels,
		completionRouteCapabilities(nil),
		nil,
	)
}

// ResolveModelRoleWithPolicy resolves a configured role while layering
// caller-specific routing constraints, such as an agent routing policy, on top
// of the role's own policy.
func (r *Registry) ResolveModelRoleWithPolicy(
	roleName string,
	params CompleteParams,
	fallbackModels []string,
	policy modelroute.Policy,
) (ModelRoleResolution, bool, error) {
	return r.resolveModelRoleWithPolicyProfileAndCapabilities(
		roleName,
		params,
		fallbackModels,
		policy,
		completionRouteCapabilities(nil),
		nil,
	)
}

func (r *Registry) resolveModelRoleWithCapabilities(
	roleName string,
	params CompleteParams,
	fallbackModels []string,
	extraCapabilities []string,
) (ModelRoleResolution, bool, error) {
	return r.resolveModelRoleWithPolicyProfileAndCapabilities(
		roleName,
		params,
		fallbackModels,
		modelroute.Policy{},
		extraCapabilities,
		nil,
	)
}

func (r *Registry) resolveModelRoleWithProfileAndCapabilities(
	roleName string,
	params CompleteParams,
	fallbackModels []string,
	extraCapabilities []string,
	profileOverride *modelroute.RequestProfile,
) (ModelRoleResolution, bool, error) {
	return r.resolveModelRoleWithPolicyProfileAndCapabilities(
		roleName,
		params,
		fallbackModels,
		modelroute.Policy{},
		extraCapabilities,
		profileOverride,
	)
}

func (r *Registry) resolveModelRoleWithPolicyProfileAndCapabilities(
	roleName string,
	params CompleteParams,
	fallbackModels []string,
	policyOverride modelroute.Policy,
	extraCapabilities []string,
	profileOverride *modelroute.RequestProfile,
) (ModelRoleResolution, bool, error) {
	roleName = strings.TrimSpace(roleName)

	now := time.Now().UTC()

	r.mu.RLock()

	if roleName == "" {
		roleName = r.defaultModelRoleNameLocked()
	}

	role, ok := r.modelRoles[roleName]
	if !ok {
		r.mu.RUnlock()

		return ModelRoleResolution{}, false, nil
	}

	role = normalizeModelRole(role)
	policy := mergeModelRoleRoutingPolicy(role.policy(), policyOverride)

	if err := validateModelRoleResolutionConstraints(roleName, policyOverride, extraCapabilities); err != nil {
		r.mu.RUnlock()

		return ModelRoleResolution{
			Role: roleName,
			Decision: modelroute.Decision{
				ModelRole: roleName,
				Policy:    policy,
			},
		}, true, err
	}

	profile := modelRoleRequestProfile(params)
	if profileOverride != nil {
		profile = *profileOverride
	}

	expansion := r.expandedModelRoleChainDetailsLocked(roleName, role, fallbackModels, profile)
	chain := expansion.models

	selectionPolicy := policy
	selectionPolicy.RequiredCapabilities = append(
		selectionPolicy.RequiredCapabilities,
		disambiguatingRoleCapabilities(completeParamsRequiredCapabilities(params))...,
	)
	selectionPolicy.RequiredCapabilities = append(
		selectionPolicy.RequiredCapabilities,
		disambiguatingRoleCapabilities(extraCapabilities)...,
	)
	selectionPolicy.RequiredCapabilities = normalizeRoleStrings(selectionPolicy.RequiredCapabilities)
	policy.RequiredCapabilities = append(policy.RequiredCapabilities, completeParamsRequiredCapabilities(params)...)
	policy.RequiredCapabilities = append(policy.RequiredCapabilities, extraCapabilities...)
	policy.RequiredCapabilities = normalizeRoleStrings(policy.RequiredCapabilities)
	telemetry := r.routeTelemetry
	candidates, unknown := r.modelRoleCandidatesLocked(chain, role, selectionPolicy)
	candidates = adjustModelRoleCandidatesForRequest(candidates, params)
	availability := r.modelRoleAvailabilityLocked(modelRoleAvailabilityIDs(chain, candidates))
	r.mu.RUnlock()

	if len(chain) == 0 && len(expansion.rejectedCandidates) == 0 {
		return ModelRoleResolution{}, true, fmt.Errorf("llm: model role %q has no candidate models", roleName)
	}

	decision := modelroute.DecideAt(candidates, profile, policy, telemetry, now)
	decision.ModelRole = roleName
	decision.CatalogVersion = modelroute.BuiltinCatalogVersion
	decision.Constraints = appendUniqueString(decision.Constraints, modelroute.ConstraintCatalogMetadata)
	decision = rejectFreshMetadataIfRequired(decision, policy, now)
	decision = appendNestedModelRoleRejections(decision, expansion.rejectedCandidates)
	decision = appendUnknownModelRoleCandidates(decision, unknown)
	decision = modelroute.DecisionWithAvailability(decision, availability)

	if len(decision.FallbackOrder) == 0 {
		return ModelRoleResolution{
			Role:     roleName,
			Decision: decision,
		}, true, fmt.Errorf("llm: model role %q rejected all candidates: %s", roleName, formatModelRoleRejections(decision))
	}

	return ModelRoleResolution{
		Role:           roleName,
		Decision:       decision,
		SelectedModel:  decision.FallbackOrder[0],
		FallbackModels: append([]string(nil), decision.FallbackOrder[1:]...),
	}, true, nil
}

func (r *Registry) routeModelRoleRequest(
	params CompleteParams,
	fallbackModels []string,
) (routedParams CompleteParams, routedFallbacks []string, routed bool, err error) {
	return r.routeModelRoleRequestWithCapabilities(params, fallbackModels, nil)
}

func (r *Registry) routeModelRoleRequestWithCapabilities(
	params CompleteParams,
	fallbackModels []string,
	extraCapabilities []string,
) (routedParams CompleteParams, routedFallbacks []string, routed bool, err error) {
	roleName := strings.TrimSpace(params.Model)

	resolution, ok, err := r.resolveModelRoleWithCapabilities(
		roleName,
		params,
		fallbackModels,
		completionRouteCapabilities(extraCapabilities),
	)
	if !ok || err != nil {
		return params, fallbackModels, ok, err
	}

	params.Model = resolution.SelectedModel

	return params, resolution.FallbackModels, true, nil
}

func completionRouteCapabilities(extraCapabilities []string) []string {
	capabilities := []string{modelroute.CapabilityChat}

	for _, capability := range extraCapabilities {
		capabilities = appendUniqueString(capabilities, capability)
	}

	return capabilities
}

func (r *Registry) defaultModelRoleNameLocked() string {
	if _, ok := r.modelRoles[r.defaultModel]; ok {
		return r.defaultModel
	}

	return ""
}

func normalizeModelRole(role ModelRole) ModelRole {
	role.Preferred = strings.TrimSpace(role.Preferred)
	role.FallbackModels = cleanModelList(role.FallbackModels)
	role.PreferredProviders = normalizeRoleStrings(role.PreferredProviders)
	role.BannedProviders = normalizeRoleStrings(role.BannedProviders)
	role.BannedModels = cleanModelList(role.BannedModels)
	role.RequiredCapabilities = normalizeRoleStrings(role.RequiredCapabilities)
	role.RoutingPolicy.PreferredProviders = normalizeRoleStrings(role.RoutingPolicy.PreferredProviders)
	role.RoutingPolicy.BannedProviders = normalizeRoleStrings(role.RoutingPolicy.BannedProviders)
	role.RoutingPolicy.BannedModels = cleanModelList(role.RoutingPolicy.BannedModels)
	role.RoutingPolicy.RequiredCapabilities = normalizeRoleStrings(role.RoutingPolicy.RequiredCapabilities)

	return role
}

func validateModelRoleLimits(name string, role ModelRole) error {
	checks := []struct {
		path  string
		value float64
	}{
		{path: "max_cost_usd", value: role.MaxCostUSD},
		{path: "routing_policy.max_budget", value: role.RoutingPolicy.MaxBudget},
	}
	for _, check := range checks {
		if !isFiniteRouteLimit(check.value) {
			return fmt.Errorf("llm: model role %q %s must be finite", name, check.path)
		}

		if check.value < 0 {
			return fmt.Errorf("llm: model role %q %s must be >= 0", name, check.path)
		}
	}

	intChecks := []struct {
		path  string
		value int
	}{
		{path: "max_latency_ms", value: role.MaxLatencyMS},
		{path: "max_ttft_ms", value: role.MaxTTFTMS},
		{path: "routing_policy.max_latency_ms", value: role.RoutingPolicy.MaxLatencyMS},
		{path: "routing_policy.max_ttft_ms", value: role.RoutingPolicy.MaxTTFTMS},
	}
	for _, check := range intChecks {
		if check.value < 0 {
			return fmt.Errorf("llm: model role %q %s must be >= 0", name, check.path)
		}
	}

	return nil
}

func validateModelRoleCapabilities(name string, role ModelRole) error {
	if err := validateKnownModelRoleCapabilities(
		name,
		"required_capabilities",
		role.RequiredCapabilities,
	); err != nil {
		return err
	}

	return validateKnownModelRoleCapabilities(
		name,
		"routing_policy.required_capabilities",
		role.RoutingPolicy.RequiredCapabilities,
	)
}

func validateKnownModelRoleCapabilities(name, path string, capabilities []string) error {
	if err := validateKnownRouteCapabilities(path, capabilities); err != nil {
		return fmt.Errorf("llm: model role %q %w", name, err)
	}

	return nil
}

func validateModelRoleResolutionConstraints(
	name string,
	policy modelroute.Policy,
	extraCapabilities []string,
) error {
	if err := validateModelRoleLimits(name, ModelRole{RoutingPolicy: policy}); err != nil {
		return err
	}

	if err := validateKnownModelRoleCapabilities(
		name,
		"routing_policy.required_capabilities",
		policy.RequiredCapabilities,
	); err != nil {
		return err
	}

	return validateKnownModelRoleCapabilities(
		name,
		"request.required_capabilities",
		extraCapabilities,
	)
}

func isFiniteRouteLimit(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func (r *Registry) expandedModelRoleChainLocked(roleName string, role ModelRole, fallbackModels []string) []string {
	return r.expandedModelRoleChainWithProfileLocked(roleName, role, fallbackModels, modelroute.RequestProfile{})
}

func (r *Registry) expandedModelRoleChainWithProfileLocked(
	roleName string,
	role ModelRole,
	fallbackModels []string,
	profile modelroute.RequestProfile,
) []string {
	return r.expandedModelRoleChainDetailsLocked(roleName, role, fallbackModels, profile).models
}

type modelRoleChainExpansion struct {
	models             []string
	rejectedCandidates []modelroute.CandidateDecision
}

func (r *Registry) expandedModelRoleChainDetailsLocked(
	roleName string,
	role ModelRole,
	fallbackModels []string,
	profile modelroute.RequestProfile,
) modelRoleChainExpansion {
	roleName = strings.TrimSpace(roleName)
	chain := modelFallbackChain(role.Preferred, append(append([]string(nil), role.FallbackModels...), fallbackModels...))

	visited := make(map[string]bool)
	if roleName != "" {
		visited[roleName] = true
	}

	return r.expandModelRoleChainLocked(chain, visited, profile)
}

func (r *Registry) expandModelRoleChainLocked(
	chain []string,
	visited map[string]bool,
	profile modelroute.RequestProfile,
) modelRoleChainExpansion {
	if len(chain) == 0 {
		return modelRoleChainExpansion{}
	}

	expanded := make([]string, 0, len(chain))

	var rejectedCandidates []modelroute.CandidateDecision

	for _, model := range chain {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}

		role, ok := r.modelRoles[model]
		if !ok {
			expanded = append(expanded, model)

			continue
		}

		if visited[model] {
			expanded = append(expanded, model)

			continue
		}

		nextVisited := make(map[string]bool, len(visited)+1)
		maps.Copy(nextVisited, visited)
		nextVisited[model] = true

		nestedChain := modelFallbackChain(role.Preferred, role.FallbackModels)
		nestedExpansion := r.modelRolePolicyExpandedChainLocked(nestedChain, role, profile)

		recursiveExpansion := r.expandModelRoleChainLocked(
			nestedExpansion.models,
			nextVisited,
			profile,
		)

		expanded = append(expanded, recursiveExpansion.models...)
		rejectedCandidates = append(rejectedCandidates, nestedExpansion.rejectedCandidates...)
		rejectedCandidates = append(rejectedCandidates, recursiveExpansion.rejectedCandidates...)
	}

	return modelRoleChainExpansion{
		models:             modelFallbackChain("", expanded),
		rejectedCandidates: rejectedCandidates,
	}
}

func (r *Registry) modelRolePolicyExpandedChainLocked(
	chain []string,
	role ModelRole,
	profile modelroute.RequestProfile,
) modelRoleChainExpansion {
	policy := role.policy()
	if !modelRolePolicyHasSelectionConstraints(policy, role) {
		return modelRoleChainExpansion{models: chain}
	}

	if decision, ok := r.modelRolePolicyCandidateChainDecisionLocked(chain, role, policy, profile); ok {
		return modelRoleChainExpansion{
			models:             append([]string(nil), decision.FallbackOrder...),
			rejectedCandidates: rejectedModelRoleCandidateDecisions(decision),
		}
	}

	expanded := make([]string, 0, len(chain))

	var rejectedCandidates []modelroute.CandidateDecision

	for _, model := range chain {
		candidateIDs, rejected, ok := r.modelRolePolicyCandidateIDsLocked(model, role, policy, profile)
		if !ok {
			expanded = append(expanded, model)

			continue
		}

		expanded = append(expanded, candidateIDs...)
		rejectedCandidates = append(rejectedCandidates, rejected...)
	}

	return modelRoleChainExpansion{
		models:             modelFallbackChain("", expanded),
		rejectedCandidates: rejectedCandidates,
	}
}

func (r *Registry) modelRolePolicyCandidateChainDecisionLocked(
	chain []string,
	role ModelRole,
	policy modelroute.Policy,
	profile modelroute.RequestProfile,
) (modelroute.Decision, bool) {
	candidates := make([]modelroute.Candidate, 0, len(chain))

	for i, model := range chain {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}

		if _, ok := r.modelRoles[model]; ok {
			return modelroute.Decision{}, false
		}

		candidateOptions, ok := r.modelRoleCandidateOptionsLocked(model, role, policy)
		if !ok {
			return modelroute.Decision{}, false
		}

		for j := range candidateOptions {
			candidate := candidateOptions[j]
			candidate.Priority = i
			candidate = r.applyModelRoleCandidatePreferencesLocked(candidate, role, len(chain))
			candidates = append(candidates, candidate)
		}
	}

	decision := modelroute.DecideAt(candidates, profile, policy, r.routeTelemetry, time.Now().UTC())

	return decision, true
}

func (r *Registry) modelRolePolicyCandidateIDsLocked(
	model string,
	role ModelRole,
	policy modelroute.Policy,
	profile modelroute.RequestProfile,
) ([]string, []modelroute.CandidateDecision, bool) {
	candidates, ok := r.modelRoleCandidateOptionsLocked(model, role, policy)
	if !ok {
		return nil, nil, false
	}

	for i := range candidates {
		candidates[i] = r.applyModelRoleCandidatePreferencesLocked(candidates[i], role, 1)
	}

	decision := modelroute.DecideAt(candidates, profile, policy, r.routeTelemetry, time.Now().UTC())

	return append([]string(nil), decision.FallbackOrder...), rejectedModelRoleCandidateDecisions(decision), true
}

func (role ModelRole) policy() modelroute.Policy {
	policy := modelroute.Policy{
		PreferredProviders:   append([]string(nil), role.RoutingPolicy.PreferredProviders...),
		BannedProviders:      append([]string(nil), role.RoutingPolicy.BannedProviders...),
		BannedModels:         append([]string(nil), role.RoutingPolicy.BannedModels...),
		RequiredCapabilities: append([]string(nil), role.RoutingPolicy.RequiredCapabilities...),
		MaxBudget:            role.RoutingPolicy.MaxBudget,
		MaxLatencyMS:         role.RoutingPolicy.MaxLatencyMS,
		MaxTTFTMS:            role.RoutingPolicy.MaxTTFTMS,
		RequireFreshMetadata: role.RoutingPolicy.RequireFreshMetadata,
	}
	policy.PreferredProviders = append(policy.PreferredProviders, role.PreferredProviders...)
	policy.BannedProviders = append(policy.BannedProviders, role.BannedProviders...)
	policy.BannedModels = append(policy.BannedModels, role.BannedModels...)
	policy.RequiredCapabilities = append(policy.RequiredCapabilities, role.RequiredCapabilities...)
	policy.PreferredProviders = normalizeRoleStrings(policy.PreferredProviders)
	policy.BannedProviders = normalizeRoleStrings(policy.BannedProviders)
	policy.BannedModels = cleanModelList(policy.BannedModels)
	policy.RequiredCapabilities = normalizeRoleStrings(policy.RequiredCapabilities)
	policy.RequireFreshMetadata = policy.RequireFreshMetadata || role.RequireFreshMetadata

	if role.MaxCostUSD > 0 && (policy.MaxBudget <= 0 || role.MaxCostUSD < policy.MaxBudget) {
		policy.MaxBudget = role.MaxCostUSD
	}

	if role.MaxLatencyMS > 0 && (policy.MaxLatencyMS <= 0 || role.MaxLatencyMS < policy.MaxLatencyMS) {
		policy.MaxLatencyMS = role.MaxLatencyMS
	}

	if role.MaxTTFTMS > 0 && (policy.MaxTTFTMS <= 0 || role.MaxTTFTMS < policy.MaxTTFTMS) {
		policy.MaxTTFTMS = role.MaxTTFTMS
	}

	return policy
}

func mergeModelRoleRoutingPolicy(base, override modelroute.Policy) modelroute.Policy {
	base.PreferredProviders = append(base.PreferredProviders, override.PreferredProviders...)
	base.BannedProviders = append(base.BannedProviders, override.BannedProviders...)
	base.BannedModels = append(base.BannedModels, override.BannedModels...)
	base.RequiredCapabilities = append(base.RequiredCapabilities, override.RequiredCapabilities...)
	base.PreferredProviders = normalizeRoleStrings(base.PreferredProviders)
	base.BannedProviders = normalizeRoleStrings(base.BannedProviders)
	base.BannedModels = cleanModelList(base.BannedModels)
	base.RequiredCapabilities = normalizeRoleStrings(base.RequiredCapabilities)
	base.RequireFreshMetadata = base.RequireFreshMetadata || override.RequireFreshMetadata

	if override.MaxBudget > 0 && (base.MaxBudget <= 0 || override.MaxBudget < base.MaxBudget) {
		base.MaxBudget = override.MaxBudget
	}

	if override.MaxLatencyMS > 0 && (base.MaxLatencyMS <= 0 || override.MaxLatencyMS < base.MaxLatencyMS) {
		base.MaxLatencyMS = override.MaxLatencyMS
	}

	if override.MaxTTFTMS > 0 && (base.MaxTTFTMS <= 0 || override.MaxTTFTMS < base.MaxTTFTMS) {
		base.MaxTTFTMS = override.MaxTTFTMS
	}

	return base
}

func modelRoleRequestProfile(params CompleteParams) modelroute.RequestProfile {
	return modelroute.RequestProfile{
		EstimatedInputTokens:  EstimateTokens(params.Messages),
		EstimatedOutputTokens: params.MaxTokens,
		Interactive:           true,
	}
}

func completeParamsRequiredCapabilities(params CompleteParams) []string {
	var capabilities []string

	if chatCapabilityRequested(params) {
		capabilities = appendUniqueString(capabilities, modelroute.CapabilityChat)
	}

	if len(params.Tools) > 0 {
		capabilities = appendUniqueString(capabilities, modelroute.CapabilityTools)
	}

	for _, message := range params.Messages {
		if len(message.ToolCalls) > 0 || message.ToolResult != nil {
			capabilities = appendUniqueString(capabilities, modelroute.CapabilityTools)
		}
	}

	if reasoningCapabilityRequested(params.ReasoningLevel) {
		capabilities = appendUniqueString(capabilities, modelroute.CapabilityReasoning)
	}

	if responseFormatRequested(params.ResponseFormat) {
		capabilities = appendUniqueString(capabilities, modelroute.CapabilityJSONSchema)
	}

	return capabilities
}

func disambiguatingRoleCapabilities(capabilities []string) []string {
	out := make([]string, 0, len(capabilities))

	for _, capability := range capabilities {
		capability = strings.ToLower(strings.TrimSpace(capability))
		if capability == "" || capability == modelroute.CapabilityChat {
			continue
		}

		out = appendUniqueString(out, capability)
	}

	return out
}

func adjustModelRoleCandidatesForRequest(candidates []modelroute.Candidate, params CompleteParams) []modelroute.Candidate {
	if !anthropicReasoningImpossibleForRequest(params) {
		return candidates
	}

	out := make([]modelroute.Candidate, len(candidates))
	copy(out, candidates)

	for i := range out {
		if !anthropicProviderName(out[i].Provider) {
			continue
		}

		out[i].Capabilities = removeRoleCapability(out[i].Capabilities, modelroute.CapabilityReasoning)
	}

	return out
}

func anthropicReasoningImpossibleForRequest(params CompleteParams) bool {
	return anthropicThinkingRequested(params.ReasoningLevel) &&
		params.MaxTokens > 0 &&
		params.MaxTokens <= anthropicThinkingMinMaxTokens
}

func removeRoleCapability(capabilities []string, remove string) []string {
	remove = strings.ToLower(strings.TrimSpace(remove))
	if remove == "" {
		return uniqueRoleStrings(capabilities)
	}

	out := make([]string, 0, len(capabilities))
	for _, capability := range capabilities {
		capability = strings.ToLower(strings.TrimSpace(capability))
		if capability == "" || capability == remove {
			continue
		}

		out = append(out, capability)
	}

	return uniqueRoleStrings(out)
}

func chatCapabilityRequested(params CompleteParams) bool {
	return len(params.Messages) > 0 ||
		len(params.Tools) > 0 ||
		reasoningCapabilityRequested(params.ReasoningLevel) ||
		responseFormatRequested(params.ResponseFormat)
}

func reasoningCapabilityRequested(level string) bool {
	switch normalizeReasoningLevel(level) {
	case "", ReasoningLevelDefault, reasoningLevelNone, reasoningLevelMinimal:
		return false
	default:
		return true
	}
}

type modelRoleUnknownCandidate struct {
	ID     string
	Reason string
}

func (r *Registry) modelRoleCandidatesLocked(
	chain []string,
	role ModelRole,
	policy modelroute.Policy,
) (candidates []modelroute.Candidate, unknown []modelRoleUnknownCandidate) {
	candidates = make([]modelroute.Candidate, 0, len(chain))
	unknown = make([]modelRoleUnknownCandidate, 0)
	seen := make(map[string]bool, len(chain))

	for i, id := range chain {
		candidateOptions, ok := r.modelRoleCandidateOptionsLocked(id, role, policy)
		if !ok {
			unknown = append(unknown, modelRoleUnknownCandidate{
				ID:     strings.TrimSpace(id),
				Reason: r.modelRoleCandidateFailureReasonLocked(id),
			})

			continue
		}

		for j := range candidateOptions {
			candidate := candidateOptions[j]
			candidate.Priority = i
			candidate = r.applyModelRoleCandidatePreferencesLocked(candidate, role, len(chain))

			candidateID := candidate.ID()
			if candidateID == "" || seen[candidateID] {
				continue
			}

			seen[candidateID] = true

			candidates = append(candidates, candidate)
		}
	}

	return candidates, unknown
}

func (r *Registry) modelRoleCandidateOptionsLocked(
	id string,
	role ModelRole,
	policy modelroute.Policy,
) ([]modelroute.Candidate, bool) {
	if candidates, ok, collision := r.runtimeCollisionModelRoleCandidateOptionsLocked(id, policy, role); collision {
		return candidates, ok
	}

	if candidate, ok := r.catalogModelRoleCandidateLocked(id); ok {
		if _, providerRegistered := r.providers[candidate.Provider]; !providerRegistered {
			if runtimeCandidate, runtimeOK := r.runtimeModelRoleCandidateLocked(id); runtimeOK {
				return []modelroute.Candidate{runtimeCandidate}, true
			}
		}

		return []modelroute.Candidate{candidate}, true
	}

	candidates := r.constrainedCatalogCandidatesLocked(id, policy, role)
	if len(candidates) > 0 {
		return candidates, true
	}

	if candidate, ok := r.runtimeModelRoleCandidateLocked(id); ok {
		return []modelroute.Candidate{candidate}, true
	}

	return nil, false
}

func (r *Registry) runtimeCollisionModelRoleCandidateOptionsLocked(
	id string,
	policy modelroute.Policy,
	role ModelRole,
) ([]modelroute.Candidate, bool, bool) {
	if !r.bareModelHasRuntimeCollisionLocked(id) {
		return nil, false, false
	}

	if !modelRolePolicyHasSelectionConstraints(policy, role) {
		if candidate, ok := r.configuredProviderRuntimeModelRoleCandidateLocked(id); ok {
			return []modelroute.Candidate{candidate}, true, true
		}

		if candidate, ok := r.defaultDisambiguatedRuntimeModelRoleCandidateLocked(id); ok {
			return []modelroute.Candidate{candidate}, true, true
		}
	}

	if candidates := r.constrainedRuntimeModelRoleCandidatesLocked(id, policy, role); len(candidates) > 0 {
		return candidates, true, true
	}

	if candidate, ok := r.defaultDisambiguatedRuntimeModelRoleCandidateLocked(id); ok {
		return []modelroute.Candidate{candidate}, true, true
	}

	return nil, false, true
}

func (r *Registry) bareModelHasRuntimeCollisionLocked(id string) bool {
	id = strings.TrimSpace(id)
	if id == "" {
		return false
	}

	if _, _, ok := splitProviderModel(id); ok {
		return false
	}

	return len(r.modelResolutionCandidatesLocked(id)) > 1
}

func (r *Registry) defaultDisambiguatedRuntimeModelRoleCandidateLocked(id string) (modelroute.Candidate, bool) {
	diagnostic := r.explainModelResolutionLocked(id)
	if diagnostic.Error != nil || len(diagnostic.Candidates) <= 1 {
		return modelroute.Candidate{}, false
	}

	return r.modelRouteCandidateFromResolutionLocked(ModelResolutionCandidate{
		ProviderName: diagnostic.ProviderName,
		Model:        diagnostic.ProviderModel,
		Provenance:   diagnostic.Provenance,
		Stale:        diagnostic.Stale,
	})
}

func (r *Registry) configuredProviderRuntimeModelRoleCandidateLocked(id string) (modelroute.Candidate, bool) {
	resolutions := r.modelResolutionCandidatesLocked(id)

	var (
		selected      modelroute.Candidate
		selectedCount int
	)

	for _, resolution := range resolutions {
		readiness, ok := r.readinessProviderLocked(resolution.ProviderName)
		if !ok || !readiness.Configured {
			continue
		}

		candidate, ok := r.modelRouteCandidateFromResolutionLocked(resolution)
		if !ok {
			continue
		}

		selected = candidate
		selectedCount++

		if selectedCount > 1 {
			return modelroute.Candidate{}, false
		}
	}

	return selected, selectedCount == 1
}

func modelRoleAvailabilityIDs(chain []string, candidates []modelroute.Candidate) []string {
	ids := make([]string, 0, len(chain)+len(candidates))
	ids = append(ids, chain...)

	for i := range candidates {
		candidate := candidates[i]
		if id := candidate.ID(); id != "" {
			ids = append(ids, id)
		}
	}

	return modelFallbackChain("", ids)
}

func (r *Registry) catalogModelRoleCandidateLocked(id string) (modelroute.Candidate, bool) {
	if candidate, ok := modelroute.BuiltinCatalog().Candidate(id); ok {
		if provider := r.providers[candidate.Provider]; provider != nil {
			candidate.Capabilities = boundCandidateCapabilitiesToProvider(candidate.Capabilities, provider)
		}

		return candidate, true
	}

	return modelroute.Candidate{}, false
}

func (r *Registry) runtimeModelRoleCandidateLocked(id string) (modelroute.Candidate, bool) {
	diagnostic := r.explainModelResolutionLocked(id)
	if diagnostic.Error != nil {
		return modelroute.Candidate{}, false
	}

	return r.modelRouteCandidateFromResolutionLocked(ModelResolutionCandidate{
		ProviderName: diagnostic.ProviderName,
		Model:        diagnostic.ProviderModel,
		Provenance:   diagnostic.Provenance,
		Stale:        diagnostic.Stale,
	})
}

func (r *Registry) modelRouteCandidateFromResolutionLocked(
	resolution ModelResolutionCandidate,
) (modelroute.Candidate, bool) {
	provider := r.providers[resolution.ProviderName]
	if provider == nil {
		return modelroute.Candidate{}, false
	}

	if candidate, ok := modelroute.BuiltinCatalog().Candidate(
		resolution.ProviderName + "/" + resolution.Model,
	); ok {
		candidate.Capabilities = boundCandidateCapabilitiesToProvider(candidate.Capabilities, provider)

		return candidate, true
	}

	candidate := modelroute.Candidate{
		Provider:        provider.Name(),
		Name:            resolution.Model,
		MaxInputTokens:  catalogContextWindow(provider.Name(), resolution.Model),
		Capabilities:    providerRouteCapabilities(provider),
		MetadataSource:  "runtime registry",
		MetadataVersion: string(resolution.Provenance),
	}
	if candidate.MaxInputTokens <= 0 {
		candidate.MaxInputTokens = provider.ModelContextWindow(resolution.Model)
	}

	return candidate, true
}

func (r *Registry) constrainedCatalogCandidatesLocked(
	id string,
	policy modelroute.Policy,
	role ModelRole,
) []modelroute.Candidate {
	id = strings.TrimSpace(id)
	if id == "" || !modelRolePolicyCanDisambiguateCatalog(policy, role) {
		return nil
	}

	if _, _, ok := splitProviderModel(id); ok {
		return nil
	}

	catalog := modelroute.BuiltinCatalog()
	if catalog.CandidateFailureReason(id) != modelroute.ReasonAmbiguousMetadata {
		return nil
	}

	if len(policy.PreferredProviders) > 0 {
		return r.preferredProviderCatalogCandidatesLocked(id, policy.PreferredProviders)
	}

	candidates := catalog.CandidatesForModel(id)
	for i := range candidates {
		candidate := &candidates[i]
		if provider := r.providers[candidate.Provider]; provider != nil {
			candidate.Capabilities = boundCandidateCapabilitiesToProvider(candidate.Capabilities, provider)
		}
	}

	return candidates
}

func (r *Registry) constrainedRuntimeModelRoleCandidatesLocked(
	id string,
	policy modelroute.Policy,
	role ModelRole,
) []modelroute.Candidate {
	id = strings.TrimSpace(id)
	if id == "" || !modelRolePolicyCanDisambiguateCatalog(policy, role) {
		return nil
	}

	if _, _, ok := splitProviderModel(id); ok {
		return nil
	}

	resolutions := r.modelResolutionCandidatesLocked(id)
	if len(resolutions) <= 1 {
		return nil
	}

	if len(policy.PreferredProviders) > 0 {
		resolutions = preferredProviderRuntimeModelResolutions(resolutions, policy.PreferredProviders)
	}

	candidates := make([]modelroute.Candidate, 0, len(resolutions))
	for _, resolution := range resolutions {
		candidate, ok := r.modelRouteCandidateFromResolutionLocked(resolution)
		if !ok {
			continue
		}

		candidates = append(candidates, candidate)
	}

	return candidates
}

func preferredProviderRuntimeModelResolutions(
	resolutions []ModelResolutionCandidate,
	preferredProviders []string,
) []ModelResolutionCandidate {
	if len(resolutions) == 0 || len(preferredProviders) == 0 {
		return resolutions
	}

	byProvider := make(map[string]ModelResolutionCandidate, len(resolutions))
	for _, resolution := range resolutions {
		byProvider[strings.ToLower(strings.TrimSpace(resolution.ProviderName))] = resolution
	}

	filtered := make([]ModelResolutionCandidate, 0, len(resolutions))
	seen := make(map[string]bool, len(preferredProviders))

	for _, providerName := range preferredProviders {
		providerName = strings.ToLower(strings.TrimSpace(providerName))
		if providerName == "" || seen[providerName] {
			continue
		}

		seen[providerName] = true
		if resolution, ok := byProvider[providerName]; ok {
			filtered = append(filtered, resolution)
		}
	}

	return filtered
}

func (r *Registry) preferredProviderCatalogCandidatesLocked(id string, preferredProviders []string) []modelroute.Candidate {
	catalog := modelroute.BuiltinCatalog()
	candidates := make([]modelroute.Candidate, 0, len(preferredProviders))
	seen := make(map[string]bool, len(preferredProviders))

	for _, providerName := range preferredProviders {
		providerName = strings.TrimSpace(providerName)
		if providerName == "" {
			continue
		}

		candidate, ok := catalog.Candidate(providerName + "/" + strings.TrimSpace(id))
		if !ok || seen[candidate.ID()] {
			continue
		}

		if provider := r.providers[candidate.Provider]; provider != nil {
			candidate.Capabilities = boundCandidateCapabilitiesToProvider(candidate.Capabilities, provider)
		}

		seen[candidate.ID()] = true
		candidates = append(candidates, candidate)
	}

	return candidates
}

func modelRolePolicyHasSelectionConstraints(policy modelroute.Policy, role ModelRole) bool {
	return len(policy.PreferredProviders) > 0 ||
		len(policy.BannedProviders) > 0 ||
		len(policy.BannedModels) > 0 ||
		len(policy.RequiredCapabilities) > 0 ||
		policy.MaxBudget > 0 ||
		policy.MaxLatencyMS > 0 ||
		policy.MaxTTFTMS > 0 ||
		role.PreferLocal
}

func modelRolePolicyCanDisambiguateCatalog(policy modelroute.Policy, role ModelRole) bool {
	return len(policy.PreferredProviders) > 0 ||
		len(policy.BannedProviders) > 0 ||
		len(policy.BannedModels) > 0 ||
		len(policy.RequiredCapabilities) > 0 ||
		policy.MaxBudget > 0 ||
		policy.MaxLatencyMS > 0 ||
		policy.MaxTTFTMS > 0 ||
		role.PreferLocal
}

func (r *Registry) modelRoleCandidateFailureReasonLocked(id string) string {
	if _, _, ok := splitProviderModel(id); !ok && len(r.modelResolutionCandidatesLocked(strings.TrimSpace(id))) > 1 {
		return modelroute.ReasonAmbiguousMetadata
	}

	return modelRoleCandidateFailureReason(id)
}

func modelRoleCandidateFailureReason(id string) string {
	reason := modelroute.BuiltinCatalog().CandidateFailureReason(id)
	if reason == "" {
		return modelroute.ReasonUnknownMetadata
	}

	return reason
}

func boundCandidateCapabilitiesToProvider(capabilities []string, provider Provider) []string {
	providerCapabilities := providerRouteCapabilities(provider)
	if len(capabilities) == 0 || len(providerCapabilities) == 0 {
		return uniqueRoleStrings(capabilities)
	}

	providerSet := make(map[string]bool, len(providerCapabilities))
	for _, capability := range providerCapabilities {
		providerSet[strings.ToLower(strings.TrimSpace(capability))] = true
	}

	out := make([]string, 0, len(capabilities))
	for _, capability := range capabilities {
		capability = strings.ToLower(strings.TrimSpace(capability))
		if capability == "" || !providerSet[capability] {
			continue
		}

		out = append(out, capability)
	}

	return uniqueRoleStrings(out)
}

func (r *Registry) applyModelRoleCandidatePreferencesLocked(candidate modelroute.Candidate, role ModelRole, chainLength int) modelroute.Candidate {
	if role.PreferLocal && slices.ContainsFunc(candidate.Capabilities, func(capability string) bool {
		return strings.EqualFold(capability, modelroute.CapabilityLocal)
	}) {
		candidate.Priority -= max(1, chainLength)
	} else if !role.PreferLocal && r.defaultConfigured && strings.EqualFold(candidate.Provider, r.fallback) {
		// Keep the configured default provider as the deterministic tie-breaker
		// when constraints expand an otherwise ambiguous bare model into all
		// provider candidates. Hard constraints can still reject this candidate,
		// allowing a compatible fallback provider to win.
		candidate.Priority -= max(1, chainLength)
	}

	return candidate
}

func providerRouteCapabilities(provider Provider) []string {
	var capabilities []string

	providerCapabilities := ProviderCapabilitiesFor(provider)
	for _, mapped := range providerCapabilityRouteMappings(providerCapabilities) {
		if !mapped.supported {
			continue
		}

		if mapped.requiresStreamProvider && !providerImplementsStreaming(provider) {
			continue
		}

		if mapped.requiresEmbeddingProvider && !providerImplementsEmbeddings(provider) {
			continue
		}

		capabilities = append(capabilities, mapped.capabilities...)
	}

	if provider.Name() == providerOllama || providerIsLocal(provider) {
		capabilities = append(capabilities, modelroute.CapabilityLocal)
	}

	return uniqueRoleStrings(capabilities)
}

type providerCapabilityRouteMapping struct {
	capabilities              []string
	supported                 bool
	requiresEmbeddingProvider bool
	requiresStreamProvider    bool
}

func providerCapabilityRouteMappings(capabilities ProviderCapabilities) []providerCapabilityRouteMapping {
	return []providerCapabilityRouteMapping{
		{
			supported:    capabilities.SupportsChatCompletions,
			capabilities: []string{modelroute.CapabilityChat, modelroute.CapabilityText},
		},
		{supported: capabilities.SupportsTools, capabilities: []string{modelroute.CapabilityTools}},
		{supported: capabilities.SupportsReasoning, capabilities: []string{modelroute.CapabilityReasoning}},
		{supported: capabilities.SupportsJSONSchema, capabilities: []string{modelroute.CapabilityJSONSchema}},
		{
			supported:                 capabilities.SupportsEmbeddings,
			capabilities:              []string{modelroute.CapabilityEmbeddings},
			requiresEmbeddingProvider: true,
		},
		{
			supported:    capabilities.SupportsMultimodalInput || capabilities.SupportsMultimodalOutput,
			capabilities: []string{modelroute.CapabilityMultimodal},
		},
		{supported: capabilities.SupportsMultimodalInput, capabilities: []string{modelroute.CapabilityVision}},
		{supported: capabilities.SupportsBatch, capabilities: []string{modelroute.CapabilityBatch}},
		{supported: capabilities.SupportsPromptCaching, capabilities: []string{modelroute.CapabilityPromptCache}},
		{
			supported:              capabilities.SupportsStreaming,
			capabilities:           []string{modelroute.CapabilityStreaming},
			requiresStreamProvider: true,
		},
		{supported: capabilities.SupportsRateLimitMetadata, capabilities: []string{modelroute.CapabilityRateLimits}},
		{supported: capabilities.SupportsRetries, capabilities: []string{modelroute.CapabilityRetries}},
		{supported: capabilities.SupportsFallbacks, capabilities: []string{modelroute.CapabilityFallback}},
		{supported: capabilities.SupportsCostTracking, capabilities: []string{modelroute.CapabilityCostTracking}},
	}
}

func providerImplementsStreaming(provider Provider) bool {
	_, ok := provider.(StreamProvider)

	return ok
}

func providerImplementsEmbeddings(provider Provider) bool {
	_, ok := provider.(EmbeddingProvider)

	return ok
}

func providerIsLocal(provider Provider) bool {
	localProvider, ok := provider.(interface{ Local() bool })

	return ok && localProvider.Local()
}

func rejectFreshMetadataIfRequired(decision modelroute.Decision, policy modelroute.Policy, now time.Time) modelroute.Decision {
	catalog := modelroute.BuiltinCatalog()

	decision.Constraints = appendUniqueString(decision.Constraints, modelroute.ConstraintMetadataFreshness)
	if !catalog.IsStale(now) {
		return decision
	}

	decision.CatalogStale = true
	decision.Warnings = append(decision.Warnings, fmt.Sprintf(
		"%s: catalog %s stale after %s",
		modelroute.ReasonMetadataStale,
		catalog.Version,
		catalog.StaleAfter.Format(time.RFC3339),
	))

	if !policy.RequireFreshMetadata {
		return decision
	}

	decision.Selected = ""
	decision.FallbackOrder = nil

	for i := range decision.Candidates {
		decision.Candidates[i].Status = modelroute.StatusRejected
		decision.Candidates[i].Rejected = appendUniqueString(
			decision.Candidates[i].Rejected,
			modelroute.ReasonMetadataStale,
		)
		decision.Candidates[i].Rank = 0
	}

	return decision
}

func appendNestedModelRoleRejections(
	decision modelroute.Decision,
	rejectedCandidates []modelroute.CandidateDecision,
) modelroute.Decision {
	if len(rejectedCandidates) == 0 {
		return decision
	}

	seen := make(map[string]bool, len(decision.Candidates)+len(rejectedCandidates))
	for i := range decision.Candidates {
		for _, id := range candidateDecisionIDs(decision.Candidates[i]) {
			seen[id] = true
		}
	}

	for i := range rejectedCandidates {
		candidate := rejectedCandidates[i]
		if candidate.Status != modelroute.StatusRejected || len(candidate.Rejected) == 0 {
			continue
		}

		ids := candidateDecisionIDs(candidate)
		if len(ids) == 0 {
			continue
		}

		if candidateDecisionIDSeen(seen, ids) {
			continue
		}

		for _, id := range ids {
			seen[id] = true
		}

		candidate.Rank = 0
		candidate.Rejected = append([]string(nil), candidate.Rejected...)
		candidate.Candidate = cloneRouteCandidate(candidate.Candidate)
		decision.Candidates = append(decision.Candidates, candidate)
	}

	return decision
}

func candidateDecisionIDSeen(seen map[string]bool, ids []string) bool {
	for _, id := range ids {
		if seen[id] {
			return true
		}
	}

	return false
}

func rejectedModelRoleCandidateDecisions(decision modelroute.Decision) []modelroute.CandidateDecision {
	rejected := make([]modelroute.CandidateDecision, 0)

	for i := range decision.Candidates {
		candidate := decision.Candidates[i]
		if candidate.Status != modelroute.StatusRejected || len(candidate.Rejected) == 0 {
			continue
		}

		candidate.Rank = 0
		candidate.Rejected = append([]string(nil), candidate.Rejected...)
		candidate.Candidate = cloneRouteCandidate(candidate.Candidate)
		rejected = append(rejected, candidate)
	}

	return rejected
}

func candidateDecisionIDs(candidate modelroute.CandidateDecision) []string {
	ids := make([]string, 0, 2)

	if id := strings.TrimSpace(candidate.ID); id != "" {
		ids = append(ids, id)
	}

	if id := strings.TrimSpace(candidate.Candidate.ID()); id != "" {
		ids = appendUniqueString(ids, id)
	}

	return ids
}

func cloneRouteCandidate(candidate modelroute.Candidate) modelroute.Candidate {
	candidate.Capabilities = append([]string(nil), candidate.Capabilities...)
	candidate.Aliases = append([]string(nil), candidate.Aliases...)

	return candidate
}

func appendUnknownModelRoleCandidates(
	decision modelroute.Decision,
	unknown []modelRoleUnknownCandidate,
) modelroute.Decision {
	for _, candidate := range unknown {
		id := strings.TrimSpace(candidate.ID)
		if id == "" {
			continue
		}

		reason := strings.TrimSpace(candidate.Reason)
		if reason == "" {
			reason = modelroute.ReasonUnknownMetadata
		}

		decision.Candidates = append(decision.Candidates, modelroute.CandidateDecision{
			ID:       id,
			Status:   modelroute.StatusRejected,
			Rejected: []string{reason},
		})
	}

	return decision
}

func (r *Registry) modelRoleAvailabilityLocked(candidateIDs []string) modelroute.Availability {
	providers := make([]string, 0, len(r.providers))
	for providerName := range r.providers {
		providers = append(providers, providerName)
	}

	models := make([]string, 0, len(r.models))
	for model := range r.models {
		models = append(models, model)
	}

	sort.Strings(providers)
	sort.Strings(models)

	availability := modelroute.Availability{
		Checked:                true,
		Providers:              providers,
		Models:                 models,
		ProviderModels:         cloneProviderModels(r.providerModels),
		ProviderModelsVerified: cloneProviderModelsVerified(r.providerModelsLive, providers),
	}

	for _, id := range candidateIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}

		diagnostic := r.explainModelResolutionLocked(id)
		if diagnostic.Error != nil {
			if modelResolutionAmbiguous(diagnostic) {
				continue
			}

			if providerName, _, ok := splitProviderModel(id); ok && !slices.Contains(providers, providerName) {
				availability.Unavailable = addModelRoleUnavailable(
					availability.Unavailable,
					id,
					modelroute.ReasonProviderUnavailable+": "+providerName,
				)

				continue
			}

			availability.Unavailable = addModelRoleUnavailable(
				availability.Unavailable,
				id,
				modelroute.ReasonModelUnavailable,
			)

			continue
		}

		if diagnostic.ProviderName == "" || diagnostic.ProviderModel == "" ||
			r.providerHasModelLocked(diagnostic.ProviderName, diagnostic.ProviderModel) {
			continue
		}

		candidateID := diagnostic.ProviderName + "/" + diagnostic.ProviderModel
		if r.providerModelsLive[diagnostic.ProviderName] {
			availability.Unavailable = addModelRoleUnavailable(
				availability.Unavailable,
				candidateID,
				modelroute.ReasonModelUnavailable+": "+candidateID,
			)
		} else {
			availability.Unverified = addModelRoleUnavailable(
				availability.Unverified,
				candidateID,
				modelroute.ReasonModelUnverified+": "+candidateID,
			)
		}
	}

	return availability
}

func modelResolutionAmbiguous(diagnostic ModelResolutionDiagnostic) bool {
	return len(diagnostic.Candidates) > 1
}

func (r *Registry) providerHasModelLocked(providerName, model string) bool {
	providerName = strings.TrimSpace(providerName)
	model = strings.TrimSpace(model)

	if providerName == "" || model == "" {
		return false
	}

	return r.providerModels[providerName][model]
}

func cloneProviderModels(in map[string]map[string]bool) map[string][]string {
	if len(in) == 0 {
		return nil
	}

	out := make(map[string][]string, len(in))
	for providerName, indexed := range in {
		models := make([]string, 0, len(indexed))
		for model := range indexed {
			models = append(models, model)
		}

		sort.Strings(models)
		out[providerName] = models
	}

	return out
}

func cloneProviderModelsVerified(in map[string]bool, providers []string) map[string]bool {
	if len(providers) == 0 {
		return nil
	}

	out := make(map[string]bool, len(providers))
	for _, provider := range providers {
		out[provider] = in[provider]
	}

	return out
}

func addModelRoleUnavailable(values map[string]string, id, reason string) map[string]string {
	if values == nil {
		values = make(map[string]string)
	}

	values[id] = reason

	return values
}

func formatModelRoleRejections(decision modelroute.Decision) string {
	var rejected []string

	for i := range decision.Candidates {
		candidate := decision.Candidates[i]
		if len(candidate.Rejected) == 0 {
			continue
		}

		rejected = append(rejected, candidate.ID+" ("+strings.Join(candidate.Rejected, "; ")+")")
	}

	if len(rejected) == 0 {
		return "no eligible candidates"
	}

	return strings.Join(rejected, ", ")
}

func cloneModelRole(role ModelRole) ModelRole {
	role.FallbackModels = append([]string(nil), role.FallbackModels...)
	role.PreferredProviders = append([]string(nil), role.PreferredProviders...)
	role.BannedProviders = append([]string(nil), role.BannedProviders...)
	role.BannedModels = append([]string(nil), role.BannedModels...)
	role.RequiredCapabilities = append([]string(nil), role.RequiredCapabilities...)
	role.RoutingPolicy.PreferredProviders = append([]string(nil), role.RoutingPolicy.PreferredProviders...)
	role.RoutingPolicy.BannedProviders = append([]string(nil), role.RoutingPolicy.BannedProviders...)
	role.RoutingPolicy.BannedModels = append([]string(nil), role.RoutingPolicy.BannedModels...)
	role.RoutingPolicy.RequiredCapabilities = append([]string(nil), role.RoutingPolicy.RequiredCapabilities...)

	return role
}

func normalizeRoleStrings(values []string) []string {
	return uniqueRoleStrings(values)
}

func uniqueRoleStrings(values []string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))

	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" || seen[value] {
			continue
		}

		seen[value] = true
		out = append(out, value)
	}

	return out
}

func appendUniqueString(values []string, next string) []string {
	if slices.Contains(values, next) {
		return values
	}

	return append(values, next)
}
