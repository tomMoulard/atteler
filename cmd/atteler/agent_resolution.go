package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/tommoulard/atteler/pkg/agent"
	appconfig "github.com/tommoulard/atteler/pkg/config"
	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/modelroute"
	"github.com/tommoulard/atteler/pkg/session"
)

const routeAvailabilityRefreshTimeout = 5 * time.Second

func requestModelAndFallbacks(
	selectedModel string,
	modelLocked bool,
	fallbackModels []string,
	activeAgent agentSelection,
	routeProfile modelroute.RequestProfile,
	routeTelemetry *modelroute.Telemetry,
	routeAvailability ...modelroute.Availability,
) (requestModel string, modelFallbacks []string, routeDecision *modelroute.Decision, err error) {
	requestModel = selectedModel

	modelFallbacks = fallbackModels
	if !activeAgent.ok || modelLocked {
		return requestModel, modelFallbacks, nil, nil
	}

	requestModel, modelFallbacks = effectiveAgentModelSelection(selectedModel, fallbackModels, activeAgent)

	availability := optionalRouteAvailability(routeAvailability)
	if routedModel, routedFallbacks, decision, routed, err := routeAgentModelChain(activeAgent.agent, routeModelChain(requestModel, modelFallbacks), routeProfile, routeTelemetry, availability); err != nil {
		return "", nil, decision, err
	} else if routed {
		requestModel = routedModel
		modelFallbacks = routedFallbacks
		routeDecision = decision
	}

	return requestModel, modelFallbacks, routeDecision, nil
}

func optionalRouteAvailability(values []modelroute.Availability) modelroute.Availability {
	if len(values) == 0 {
		return modelroute.Availability{}
	}

	return values[0]
}

func effectiveAgentModelSelection(selectedModel string, fallbackModels []string, activeAgent agentSelection) (requestModel string, modelFallbacks []string) {
	requestModel = selectedModel

	modelFallbacks = append([]string(nil), fallbackModels...)

	if !activeAgent.ok {
		return requestModel, modelFallbacks
	}

	if activeAgent.agent.Model != "" {
		requestModel = activeAgent.agent.Model
	}

	if len(activeAgent.agent.FallbackModels) > 0 {
		modelFallbacks = append([]string(nil), activeAgent.agent.FallbackModels...)
	}

	return requestModel, modelFallbacks
}

func effectiveRouteCandidateChain(selectedModel string, fallbackModels []string, activeAgent agentSelection, modelLocked bool) []string {
	if !activeAgent.ok || modelLocked {
		return nil
	}

	requestModel, modelFallbacks := effectiveAgentModelSelection(selectedModel, fallbackModels, activeAgent)

	return routeModelChain(requestModel, modelFallbacks)
}

func routeModelChain(primary string, fallbacks []string) []string {
	chain := make([]string, 0, len(fallbacks)+1)
	seen := make(map[string]bool, len(fallbacks)+1)

	for _, model := range append([]string{primary}, fallbacks...) {
		model = strings.TrimSpace(model)
		if model == "" || seen[model] {
			continue
		}

		seen[model] = true
		chain = append(chain, model)
	}

	return chain
}

func routeAgentModelChain(activeAgent agent.Agent, chain []string, profile modelroute.RequestProfile, telemetry *modelroute.Telemetry, availability modelroute.Availability) (requestModel string, fallbackModels []string, decision *modelroute.Decision, routed bool, err error) {
	if len(chain) == 0 {
		return "", nil, nil, false, nil
	}

	now := time.Now().UTC()
	if !routeAgentChainHasEvidence(activeAgent.RoutingPolicy, telemetry, availability, chain, profile, now) {
		return "", nil, nil, false, nil
	}

	routeDecision := modelroute.DecideFromCatalog(
		modelroute.BuiltinCatalog(),
		chain,
		profile,
		activeAgent.RoutingPolicy,
		telemetry,
		now,
	)

	routeDecision = modelroute.DecisionWithAvailability(routeDecision, availability)
	if len(routeDecision.FallbackOrder) == 0 {
		rejectedMessage := "agent routing rejected all model candidates"
		if routePolicyConfigured(activeAgent.RoutingPolicy) {
			rejectedMessage = "agent routing policy rejected all model candidates"
		}

		return "", nil, &routeDecision, true, fmt.Errorf("%s: %s", rejectedMessage, formatRouteRejections(routeDecision))
	}

	return routeDecision.FallbackOrder[0], append([]string(nil), routeDecision.FallbackOrder[1:]...), &routeDecision, true, nil
}

func routePolicyConfigured(policy modelroute.Policy) bool {
	return len(policy.PreferredProviders) > 0 ||
		len(policy.BannedProviders) > 0 ||
		len(policy.BannedModels) > 0 ||
		len(policy.RequiredCapabilities) > 0 ||
		policy.MaxBudget > 0 ||
		policy.RequireFreshMetadata
}

func routeAgentChainHasEvidence(
	policy modelroute.Policy,
	telemetry *modelroute.Telemetry,
	availability modelroute.Availability,
	chain []string,
	profile modelroute.RequestProfile,
	now time.Time,
) bool {
	return routePolicyConfigured(policy) ||
		routeTelemetryRelevant(telemetry, chain, now) ||
		routeAvailabilityRelevant(availability, chain) ||
		routePrimaryConstraintRelevant(chain, profile)
}

func routePrimaryConstraintRelevant(chain []string, profile modelroute.RequestProfile) bool {
	if len(chain) == 0 {
		return false
	}

	primary, ok := modelroute.BuiltinCatalog().Candidate(chain[0])
	if !ok {
		return false
	}

	return !modelroute.FitsContext(primary, profile) || !modelroute.FitsBudget(primary, profile)
}

func routeTelemetryRelevant(telemetry *modelroute.Telemetry, chain []string, now time.Time) bool {
	if telemetry == nil {
		return false
	}

	catalog := modelroute.BuiltinCatalog()

	for _, id := range chain {
		candidate, ok := catalog.Candidate(id)
		if !ok {
			continue
		}

		if observation, ok := telemetry.Snapshot(candidate.ID()); ok && routeObservationRelevant(observation, now) {
			return true
		}
	}

	return false
}

func routeObservationRelevant(observation modelroute.Observation, now time.Time) bool {
	return observation.Count > 0 ||
		observation.RateLimitActive(now)
}

func routeAvailabilityRelevant(availability modelroute.Availability, chain []string) bool {
	if !availability.Checked || (len(availability.Unavailable) == 0 && len(availability.Unverified) == 0) {
		return false
	}

	catalog := modelroute.BuiltinCatalog()

	for _, id := range chain {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}

		if routeAvailabilityHasEvidence(availability, id) {
			return true
		}

		if candidate, ok := catalog.Candidate(id); ok {
			if routeAvailabilityHasEvidence(availability, candidate.ID()) {
				return true
			}
		}
	}

	return false
}

func routeAvailabilityHasEvidence(availability modelroute.Availability, id string) bool {
	if _, ok := availability.Unavailable[id]; ok {
		return true
	}

	if _, ok := availability.Unverified[id]; ok {
		return true
	}

	return false
}

func routeProfileForMessages(messages []llm.Message, generation generationSettings) modelroute.RequestProfile {
	return modelroute.RequestProfile{
		EstimatedInputTokens:  llm.EstimateTokens(messages),
		EstimatedOutputTokens: generation.MaxTokens,
		Interactive:           true,
	}
}

func routeTelemetryFromRegistry(registry *llm.Registry) *modelroute.Telemetry {
	if registry == nil {
		return nil
	}

	return registry.RouteTelemetry()
}

func routeAvailabilityFromRegistryWithRefresh(ctx context.Context, registry *llm.Registry, candidateIDs []string) modelroute.Availability {
	refreshRouteProviderModels(ctx, registry, candidateIDs)

	availability := routeAvailabilityFromRegistry(registry, candidateIDs)
	if ctx != nil && registry != nil && len(candidateIDs) > 0 {
		availability.RefreshAttempted = true
		availability.RefreshTimeoutMS = int(routeAvailabilityRefreshTimeout / time.Millisecond)
	}

	return availability
}

func refreshRouteProviderModels(ctx context.Context, registry *llm.Registry, candidateIDs []string) {
	if ctx == nil || registry == nil || len(candidateIDs) == 0 {
		return
	}

	refreshCtx, cancel := context.WithTimeout(ctx, routeAvailabilityRefreshTimeout)
	defer cancel()

	for _, providerName := range routeAvailabilityProviders(registry, candidateIDs) {
		if err := refreshCtx.Err(); err != nil {
			return
		}

		if _, err := registry.ProviderModelCatalog(refreshCtx, providerName); err != nil {
			continue
		}
	}
}

func routeAvailabilityProviders(registry *llm.Registry, candidateIDs []string) []string {
	seen := make(map[string]bool)

	for _, id := range candidateIDs {
		providerName := routeAvailabilityCandidate(registry, id).provider
		if providerName == "" {
			continue
		}

		seen[providerName] = true
	}

	providers := make([]string, 0, len(seen))
	for providerName := range seen {
		providers = append(providers, providerName)
	}

	sort.Strings(providers)

	return providers
}

func routeAvailabilityFromRegistry(registry *llm.Registry, candidateIDs []string) modelroute.Availability {
	if registry == nil {
		return modelroute.Availability{}
	}

	providers := registry.ListProviders()
	models := registry.ListModels()

	sort.Strings(providers)
	sort.Strings(models)

	availability := modelroute.Availability{
		Checked:                true,
		Providers:              providers,
		Models:                 models,
		ProviderModels:         registry.IndexedProviderModels(),
		ProviderModelsVerified: providerModelsVerifiedByProvider(registry, providers),
	}

	for _, id := range candidateIDs {
		candidate := routeAvailabilityCandidate(registry, id)
		if candidate.decisionID == "" {
			continue
		}

		if candidate.resolvable {
			if candidate.model != "" && !routeAvailabilityModelIndexed(registry, candidate, models) {
				reason := candidate.provider + "/" + candidate.model
				if registry.ProviderModelsVerified(candidate.provider) {
					availability.Unavailable = addUnavailableRoute(
						availability.Unavailable,
						candidate.decisionID,
						modelroute.ReasonModelUnavailable+": "+reason,
					)
				} else {
					availability.Unverified = addUnverifiedRoute(
						availability.Unverified,
						candidate.decisionID,
						modelroute.ReasonModelUnverified+": "+reason,
					)
				}
			}

			continue
		}

		if candidate.provider != "" && !containsString(providers, candidate.provider) {
			availability.Unavailable = addUnavailableRoute(
				availability.Unavailable,
				candidate.decisionID,
				modelroute.ReasonProviderUnavailable+": "+candidate.provider,
			)

			continue
		}

		availability.Unavailable = addUnavailableRoute(
			availability.Unavailable,
			candidate.decisionID,
			modelroute.ReasonModelUnavailable,
		)
	}

	return availability
}

func providerModelsVerifiedByProvider(registry *llm.Registry, providers []string) map[string]bool {
	out := make(map[string]bool, len(providers))

	for _, provider := range providers {
		out[provider] = registry.ProviderModelsVerified(provider)
	}

	return out
}

type routeAvailabilityCandidateEvidence struct {
	decisionID        string
	provider          string
	model             string
	aliases           []string
	resolvable        bool
	providerQualified bool
}

func routeAvailabilityCandidate(registry *llm.Registry, id string) routeAvailabilityCandidateEvidence {
	id = strings.TrimSpace(id)
	if id == "" {
		return routeAvailabilityCandidateEvidence{}
	}

	candidate := routeAvailabilityCandidateEvidence{decisionID: id}
	if catalogCandidate, ok := modelroute.BuiltinCatalog().Candidate(id); ok {
		candidate.decisionID = catalogCandidate.ID()
		candidate.provider = catalogCandidate.Provider
		candidate.model = catalogCandidate.Name
		candidate.aliases = append([]string(nil), catalogCandidate.Aliases...)
	}

	if provider, model, ok := strings.Cut(id, "/"); ok {
		candidate.providerQualified = true
		if candidate.provider == "" {
			candidate.provider = strings.TrimSpace(provider)
		}

		if candidate.model == "" {
			candidate.model = strings.TrimSpace(model)
		}
	} else if candidate.model == "" {
		candidate.model = id
	}

	resolvedProvider, resolvedModel, ok := registry.ResolveModel(id)
	if ok {
		candidate.resolvable = true
		if !candidate.providerQualified && id != resolvedModel {
			candidate.aliases = append(candidate.aliases, id)
		}

		if candidate.provider == "" || (candidate.providerQualified && !routeAvailabilityProviderRegistered(registry, candidate.provider)) {
			candidate.provider = resolvedProvider
			candidate.model = resolvedModel
			candidate.providerQualified = false
		} else if candidate.model == "" {
			candidate.model = resolvedModel
		}
	}

	return candidate
}

func routeAvailabilityProviderRegistered(registry *llm.Registry, providerName string) bool {
	if registry == nil {
		return false
	}

	_, ok := registry.Provider(strings.TrimSpace(providerName))

	return ok
}

func routeAvailabilityModelIndexed(registry *llm.Registry, candidate routeAvailabilityCandidateEvidence, models []string) bool {
	if candidate.provider != "" {
		allowConfiguredAliases := !candidate.providerQualified
		if routeAvailabilityProviderHasModel(registry, candidate.provider, candidate.model, allowConfiguredAliases) {
			return true
		}

		for _, alias := range candidate.aliases {
			if routeAvailabilityProviderHasModel(registry, candidate.provider, alias, allowConfiguredAliases) {
				return true
			}
		}

		return false
	}

	if containsString(models, candidate.model) {
		return true
	}

	for _, alias := range candidate.aliases {
		if containsString(models, alias) {
			return true
		}
	}

	return false
}

func routeAvailabilityProviderHasModel(
	registry *llm.Registry,
	providerName string,
	model string,
	allowConfiguredAliases bool,
) bool {
	model = strings.TrimSpace(model)
	if model == "" {
		return false
	}

	if provenance, ok := registry.ProviderModelProvenance(providerName, model); ok {
		if allowConfiguredAliases || provenance != llm.ModelProvenanceConfiguredAlias {
			return true
		}

		if _, ok := registry.ProviderModelCatalogProvenance(providerName, model); ok {
			return true
		}

		return registry.ProviderModelUserOverride(providerName, model)
	}

	if qualifiedProvider, providerModel, ok := strings.Cut(strings.TrimSpace(model), "/"); ok {
		if !strings.EqualFold(strings.TrimSpace(qualifiedProvider), strings.TrimSpace(providerName)) {
			return false
		}

		model = providerModel
	}

	provenance, ok := registry.ProviderModelProvenance(providerName, model)
	if !ok {
		return false
	}

	if allowConfiguredAliases || provenance != llm.ModelProvenanceConfiguredAlias {
		return true
	}

	if _, ok := registry.ProviderModelCatalogProvenance(providerName, model); ok {
		return true
	}

	return registry.ProviderModelUserOverride(providerName, model)
}

func addUnavailableRoute(unavailable map[string]string, id, reason string) map[string]string {
	if unavailable == nil {
		unavailable = make(map[string]string)
	}

	unavailable[id] = reason

	return unavailable
}

func addUnverifiedRoute(unverified map[string]string, id, reason string) map[string]string {
	if unverified == nil {
		unverified = make(map[string]string)
	}

	unverified[id] = reason

	return unverified
}

func containsString(values []string, want string) bool {
	want = strings.ToLower(strings.TrimSpace(want))
	for _, value := range values {
		if strings.ToLower(strings.TrimSpace(value)) == want {
			return true
		}
	}

	return false
}

func formatRouteRejections(decision modelroute.Decision) string {
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

func resolveAgent(agents *agent.Registry, selectedAgent, input string) (agentSelection, string, error) {
	agentName := selectedAgent

	prompt := input
	if inlineName, inlinePrompt, ok := agent.ParseInvocation(input); ok {
		agentName = inlineName
		prompt = inlinePrompt
	}

	if agentName == "" {
		if matchedAgent, ok := agents.MatchPrompt(prompt); ok {
			return agentSelection{name: matchedAgent.Name, agent: matchedAgent, ok: true}, prompt, nil
		}

		return agentSelection{}, prompt, nil
	}

	activeAgent, ok := agents.Get(agentName)
	if !ok {
		return agentSelection{}, input, fmt.Errorf("unknown agent %q", agentName)
	}

	if strings.TrimSpace(prompt) == "" {
		return agentSelection{}, input, fmt.Errorf("agent %q needs a prompt", agentName)
	}

	return agentSelection{name: agentName, agent: activeAgent, ok: true}, prompt, nil
}

func generationOverridesFromState(opts cliOptions, selection selectionState, persistedState appconfig.State, cwd string) generationSettings {
	generation := generationFromOptions(opts)

	if generation.ReasoningLevel == "" {
		if level := strings.TrimSpace(selection.sessionState.DefaultReasoningLevel); level != "" {
			generation.ReasoningLevel = level
		} else if level := strings.TrimSpace(persistedState.ReasoningLevelForFolder(cwd)); level != "" {
			generation.ReasoningLevel = level
		}
	}

	if generation.ModelMode == "" {
		generation.ModelMode = modelModeOverrideFromState(selection, persistedState, cwd)
	}

	return generation
}

func modelModeOverrideFromState(selection selectionState, persistedState appconfig.State, cwd string) string {
	if mode := strings.TrimSpace(selection.sessionState.DefaultModelMode); mode != "" {
		return mode
	}

	resolution := persistedState.ResolveModelModePreference(cwd)
	if mode := strings.TrimSpace(resolution.Value); mode != "" {
		return mode
	}

	if resolution.Source != "" {
		return llm.ModelModeDefault
	}

	return ""
}

type selectionState struct {
	sessionState   session.Session
	selectedModel  string
	selectedAgent  string
	fallbackModels []string
	modelLocked    bool
}

func resolveSelection(
	opts cliOptions,
	cfg appconfig.Config,
	persistedModel string,
	agentRegistry *agent.Registry,
	store *session.Store,
) (selectionState, error) {
	state := selectionState{
		selectedAgent:  opts.agentName,
		selectedModel:  opts.model,
		modelLocked:    opts.model != "",
		fallbackModels: append([]string(nil), cfg.FallbackModels...),
	}
	if state.modelLocked {
		state.fallbackModels = nil
	}

	state.sessionState = session.New(state.selectedModel, nil)
	if err := loadRequestedSession(opts, store, &state); err != nil {
		return selectionState{}, err
	}

	if err := applySelectedAgent(opts, agentRegistry, &state); err != nil {
		return selectionState{}, err
	}

	if err := applyRouteSelection(routeModelsCommandInputFromOptions(opts), &state); err != nil {
		return selectionState{}, err
	}

	if state.selectedModel == "" {
		state.selectedModel = persistedModel
	}

	if state.selectedModel == "" {
		state.selectedModel = cfg.DefaultModel
	}

	if state.selectedModel != "" {
		state.sessionState.DefaultModel = state.selectedModel
	}

	if opts.sessionTitle != "" {
		state.sessionState.Title = opts.sessionTitle
	}

	if len(opts.sessionTags) > 0 {
		state.sessionState.Tags = mergeTags(state.sessionState.Tags, opts.sessionTags)
	}

	return state, nil
}

func loadRequestedSession(opts cliOptions, store *session.Store, state *selectionState) error {
	if opts.sessionRef == "" && opts.replayRef == "" && opts.exportRef == "" && opts.showSessionRef == "" && opts.summarySessionRef == "" {
		return nil
	}

	ref := firstNonEmpty(opts.replayRef, opts.showSessionRef, opts.summarySessionRef, opts.exportRef, opts.sessionRef)

	loadedSession, err := store.Load(ref)
	if err != nil {
		return fmt.Errorf("load session: %w", err)
	}

	state.sessionState = loadedSession
	if state.selectedAgent == "" {
		state.selectedAgent = state.sessionState.DefaultAgent
	}

	if state.selectedModel == "" {
		state.selectedModel = state.sessionState.DefaultModel
	}

	return nil
}

func applySelectedAgent(opts cliOptions, agentRegistry *agent.Registry, state *selectionState) error {
	if state.selectedAgent == "" || (opts.agentName == "" && sessionUtilityCommandRequested(opts)) {
		return nil
	}

	activeAgent, ok := agentRegistry.Get(state.selectedAgent)
	if !ok {
		return fmt.Errorf("unknown agent %q", state.selectedAgent)
	}

	if state.selectedModel == "" {
		state.selectedModel = activeAgent.Model
	}

	if !state.modelLocked && len(activeAgent.FallbackModels) > 0 {
		state.fallbackModels = activeAgent.FallbackModels
	}

	state.sessionState.DefaultAgent = state.selectedAgent

	return nil
}

func sessionUtilityCommandRequested(opts cliOptions) bool {
	return sessionReadUtilityRequested(opts) ||
		sessionWriteUtilityRequested(opts) ||
		sessionLocalUtilityRequested(opts) ||
		workflowExecutionUtilityRequested(opts) ||
		providerInspectionUtilityRequested(opts)
}

func sessionReadUtilityRequested(opts cliOptions) bool {
	return opts.replayRef != "" ||
		opts.exportRef != "" ||
		opts.showSessionRef != "" ||
		opts.summarySessionRef != "" ||
		opts.listArtifacts ||
		opts.listEvaluations ||
		opts.listFailures ||
		opts.listMessages
}

func sessionWriteUtilityRequested(opts cliOptions) bool {
	return opts.recordFailure != "" ||
		opts.recordEvaluation != "" ||
		opts.recordArtifact != "" ||
		opts.feedbackApplyConfig != ""
}

func sessionLocalUtilityRequested(opts cliOptions) bool {
	return opts.mergeArtifactsPath != "" ||
		opts.feedbackProposals ||
		opts.agentMemorySearch != "" ||
		opts.agentMemoryDelete != "" ||
		opts.agentMemoryCompact ||
		opts.agentMemoryMigrate ||
		len(opts.agentMemoryIndexFiles) > 0
}

func workflowExecutionUtilityRequested(opts cliOptions) bool {
	return opts.bashCommand != "" ||
		opts.asyncRun ||
		len(opts.spawnAgentSpecs) > 0 ||
		opts.speculateRun ||
		opts.reviewRun
}

func providerInspectionUtilityRequested(opts cliOptions) bool {
	return opts.listModels ||
		opts.explainModelResolution != "" ||
		opts.doctor
}
