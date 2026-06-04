package llm

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"strings"

	"github.com/tommoulard/atteler/pkg/modelroute"
)

// ErrAgentLoopCostPricingUnavailable means a cost ceiling was requested for a
// model that lacks maintained pricing metadata. Cost budgets fail closed rather
// than treating unknown pricing as free.
var ErrAgentLoopCostPricingUnavailable = errors.New("llm: agent loop cost pricing metadata unavailable")

// ErrAgentLoopCostUsageUnavailable means a cost ceiling was requested but a
// model response did not include provider-reported token usage. Cost budgets
// fail closed rather than treating unknown usage as free.
var ErrAgentLoopCostUsageUnavailable = errors.New("llm: agent loop cost usage metadata unavailable")

// AgentLoopCostEstimator returns a cost estimator backed by the maintained
// model routing catalog. The primary model and every configured fallback must
// have pricing metadata before the estimator is returned; the estimator also
// verifies the provider/model and usage reported by each response so provider
// aliases and unexpected fallbacks cannot bypass MaxCostMicros.
func (r *Registry) AgentLoopCostEstimator(primaryModel string, fallbackModels []string) (AgentLoopCostEstimator, error) {
	models, err := r.agentLoopCostModelChain(primaryModel, fallbackModels)
	if err != nil {
		return nil, err
	}

	if len(models) == 0 && strings.TrimSpace(primaryModel) == "" {
		if providerName, providerModel, ok := r.resolveModelForCost(""); ok {
			models = append(models, modelrouteID(providerName, providerModel))
		}
	}

	if len(models) == 0 {
		models = []string{primaryModel}
	}

	configuredMetadata := make([]modelroute.ModelMetadata, 0, len(models))
	seenMetadata := make(map[string]bool, len(models))

	for _, model := range models {
		metadata, err := r.agentLoopPricingMetadataForModel(model)
		if err != nil {
			return nil, err
		}

		if seenMetadata[metadata.ID()] {
			continue
		}

		configuredMetadata = append(configuredMetadata, metadata)
		seenMetadata[metadata.ID()] = true
	}

	return func(resp *Response) (int64, error) {
		if resp == nil {
			return 0, nil
		}

		metadata, err := r.agentLoopPricingMetadataForResponse(resp, primaryModel, configuredMetadata)
		if err != nil {
			return 0, err
		}

		return agentLoopResponseCostMicros(metadata, resp)
	}, nil
}

func (r *Registry) agentLoopCostModelChain(primaryModel string, fallbackModels []string) ([]string, error) {
	models := modelFallbackChain(primaryModel, fallbackModels)
	if r == nil {
		return models, nil
	}

	if roleName, _, ok := r.ModelRoleForRequest(primaryModel); ok {
		return r.agentLoopCostResolvedRoleModels(roleName, fallbackModels)
	}

	return r.expandAgentLoopCostModelRoles(models, nil)
}

func (r *Registry) expandAgentLoopCostModelRoles(models []string, visited map[string]bool) ([]string, error) {
	if len(models) == 0 {
		return nil, nil
	}

	expanded := make([]string, 0, len(models))
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}

		if _, ok := r.agentLoopCostModelRole(model); !ok {
			expanded = append(expanded, model)

			continue
		}

		if visited[model] {
			expanded = append(expanded, model)

			continue
		}

		nextVisited := cloneBoolMap(visited)
		nextVisited[model] = true

		roleModels, err := r.agentLoopCostResolvedRoleModels(model, nil)
		if err != nil {
			return nil, err
		}

		nested, err := r.expandAgentLoopCostModelRoles(roleModels, nextVisited)
		if err != nil {
			return nil, err
		}

		expanded = append(expanded, nested...)
	}

	return modelFallbackChain("", expanded), nil
}

func (r *Registry) agentLoopCostResolvedRoleModels(roleName string, fallbackModels []string) ([]string, error) {
	resolution, ok, err := r.ResolveModelRole(roleName, CompleteParams{
		Model:    roleName,
		Messages: []Message{{Role: RoleUser}},
	}, fallbackModels)
	if !ok {
		return modelFallbackChain(roleName, fallbackModels), nil
	}

	if err != nil {
		return nil, err
	}

	return modelFallbackChain(resolution.SelectedModel, resolution.FallbackModels), nil
}

func (r *Registry) agentLoopCostModelRole(model string) (ModelRole, bool) {
	model = strings.TrimSpace(model)
	if model == "" {
		return ModelRole{}, false
	}

	role, ok := r.ModelRole(model)

	return role, ok
}

func cloneBoolMap(in map[string]bool) map[string]bool {
	out := make(map[string]bool, len(in)+1)
	maps.Copy(out, in)

	return out
}

func (r *Registry) agentLoopPricingMetadataForResponse(
	resp *Response,
	fallbackModel string,
	configuredMetadata []modelroute.ModelMetadata,
) (modelroute.ModelMetadata, error) {
	model := strings.TrimSpace(resp.Model)

	provider := strings.TrimSpace(resp.Provider)
	if provider != "" {
		return agentLoopPricingMetadataForProviderResponse(provider, model, configuredMetadata)
	}

	if model == "" {
		if len(configuredMetadata) > 1 {
			return modelroute.ModelMetadata{}, fmt.Errorf("%w for %q", ErrAgentLoopCostPricingUnavailable, "unknown")
		}

		if len(configuredMetadata) == 1 {
			return configuredMetadata[0], nil
		}

		model = strings.TrimSpace(fallbackModel)
	}

	if providerName, providerModel, ok := splitProviderModel(model); ok {
		return agentLoopConfiguredPricingMetadata(providerName, providerModel, configuredMetadata)
	}

	if metadata, ok := agentLoopConfiguredMetadataForResponseModel(model, configuredMetadata); ok {
		return metadata, nil
	}

	if model == "" {
		return modelroute.ModelMetadata{}, fmt.Errorf("%w for %q", ErrAgentLoopCostPricingUnavailable, "unknown")
	}

	return modelroute.ModelMetadata{}, fmt.Errorf("%w for %q", ErrAgentLoopCostPricingUnavailable, model)
}

func agentLoopPricingMetadataForProviderResponse(
	provider string,
	model string,
	configuredMetadata []modelroute.ModelMetadata,
) (modelroute.ModelMetadata, error) {
	if model == "" {
		return agentLoopPricingMetadataForProviderOnlyResponse(provider, configuredMetadata)
	}

	return agentLoopConfiguredPricingMetadata(provider, model, configuredMetadata)
}

func agentLoopConfiguredPricingMetadata(
	provider string,
	model string,
	configuredMetadata []modelroute.ModelMetadata,
) (modelroute.ModelMetadata, error) {
	metadata, err := agentLoopPricingMetadata(provider, model)
	if err != nil {
		return modelroute.ModelMetadata{}, err
	}

	if !agentLoopMetadataConfigured(metadata, configuredMetadata) {
		return modelroute.ModelMetadata{}, fmt.Errorf("%w for %q", ErrAgentLoopCostPricingUnavailable, metadata.ID())
	}

	return metadata, nil
}

func agentLoopPricingMetadataForProviderOnlyResponse(
	provider string,
	configuredMetadata []modelroute.ModelMetadata,
) (modelroute.ModelMetadata, error) {
	if metadata, ok := agentLoopSingleConfiguredProviderMetadata(provider, configuredMetadata); ok {
		return metadata, nil
	}

	return modelroute.ModelMetadata{}, fmt.Errorf("%w for %q", ErrAgentLoopCostPricingUnavailable, modelrouteID(provider, ""))
}

func (r *Registry) agentLoopPricingMetadataForModel(model string) (modelroute.ModelMetadata, error) {
	provider, providerModel, ok := r.resolveModelForCost(model)
	if ok {
		return agentLoopPricingMetadata(provider, providerModel)
	}

	return agentLoopPricingMetadata("", model)
}

func (r *Registry) resolveModelForCost(model string) (providerName, providerModel string, ok bool) {
	if r == nil {
		if providerName, providerModel, ok = splitProviderModel(model); ok {
			return providerName, providerModel, true
		}

		return "", strings.TrimSpace(model), false
	}

	if providerName, providerModel, ok = r.ResolveModel(model); ok {
		return providerName, providerModel, true
	}

	if providerName, providerModel, ok = splitProviderModel(model); ok {
		return providerName, providerModel, true
	}

	return "", strings.TrimSpace(model), false
}

func agentLoopPricingMetadata(provider, model string) (modelroute.ModelMetadata, error) {
	if providerFromModel, providerModel, ok := splitProviderModel(model); ok {
		trimmedProvider := strings.TrimSpace(provider)
		if trimmedProvider != "" && !strings.EqualFold(trimmedProvider, providerFromModel) {
			return modelroute.ModelMetadata{}, fmt.Errorf("%w for %q", ErrAgentLoopCostPricingUnavailable, modelrouteID(trimmedProvider, model))
		}

		if trimmedProvider == "" {
			provider = providerFromModel
		}

		model = providerModel
	}

	catalog := modelroute.BuiltinCatalog()

	var (
		metadata modelroute.ModelMetadata
		ok       bool
	)

	if strings.TrimSpace(provider) == "" {
		candidate, candidateOK := catalog.Candidate(model)
		if !candidateOK {
			return modelroute.ModelMetadata{}, fmt.Errorf("%w for %q", ErrAgentLoopCostPricingUnavailable, modelrouteID(provider, model))
		}

		metadata, ok = catalog.Lookup(candidate.Provider, candidate.Name)
	} else {
		metadata, ok = catalog.Lookup(provider, model)
	}

	if !ok {
		return modelroute.ModelMetadata{}, fmt.Errorf("%w for %q", ErrAgentLoopCostPricingUnavailable, modelrouteID(provider, model))
	}

	if !agentLoopMetadataHasPricing(metadata) {
		return modelroute.ModelMetadata{}, fmt.Errorf("%w for %q", ErrAgentLoopCostPricingUnavailable, metadata.ID())
	}

	return metadata, nil
}

func modelrouteID(provider, model string) string {
	provider = strings.TrimSpace(provider)
	model = strings.TrimSpace(model)

	if provider == "" {
		return model
	}

	if model == "" {
		return provider
	}

	return provider + "/" + model
}

func agentLoopMetadataHasPricing(metadata modelroute.ModelMetadata) bool {
	return metadata.InputTokenCost > 0 && metadata.OutputTokenCost > 0
}

func agentLoopSingleConfiguredProviderMetadata(provider string, configured []modelroute.ModelMetadata) (modelroute.ModelMetadata, bool) {
	provider = strings.TrimSpace(provider)

	var (
		matched modelroute.ModelMetadata
		count   int
	)

	for i := range configured {
		metadata := configured[i]
		if strings.EqualFold(strings.TrimSpace(metadata.Provider), provider) {
			matched = metadata
			count++
		}
	}

	return matched, count == 1
}

func agentLoopMetadataConfigured(metadata modelroute.ModelMetadata, configured []modelroute.ModelMetadata) bool {
	for i := range configured {
		if configured[i].ID() == metadata.ID() {
			return true
		}
	}

	return false
}

func agentLoopConfiguredMetadataForResponseModel(
	model string,
	configured []modelroute.ModelMetadata,
) (modelroute.ModelMetadata, bool) {
	model = strings.TrimSpace(model)
	if model == "" {
		if len(configured) == 0 {
			return modelroute.ModelMetadata{}, false
		}

		return configured[0], true
	}

	var (
		matched modelroute.ModelMetadata
		count   int
	)

	for i := range configured {
		configuredMetadata := configured[i]

		metadata, err := agentLoopPricingMetadata(configuredMetadata.Provider, model)
		if err != nil {
			continue
		}

		if metadata.ID() == configuredMetadata.ID() {
			matched = metadata
			count++
		}
	}

	return matched, count == 1
}

func agentLoopResponseCostMicros(metadata modelroute.ModelMetadata, resp *Response) (int64, error) {
	if resp == nil {
		return 0, nil
	}

	uncachedTokens, err := agentLoopResponseUsage(resp, metadata.ID())
	if err != nil {
		return 0, err
	}

	cacheWriteCost, err := agentLoopPricingForUsage(metadata, resp, uncachedTokens)
	if err != nil {
		return 0, err
	}

	cost := float64(uncachedTokens)*metadata.InputTokenCost +
		float64(resp.CachedInputTokens)*metadata.CachedInputTokenCost +
		float64(resp.CacheWriteInputTokens)*cacheWriteCost +
		float64(resp.OutputTokens)*metadata.OutputTokenCost
	if cost <= 0 {
		return 0, nil
	}

	// Prices are stored as decimal floats per token. Subtract a tiny epsilon
	// before ceiling so values that should be exact integer micros (for example
	// 0.4 + 1.6) do not overcharge due to binary floating-point representation.
	return int64(math.Ceil(cost*1_000_000 - 1e-9)), nil
}

func agentLoopResponseUsage(resp *Response, modelID string) (int, error) {
	if resp.InputTokens < 0 ||
		resp.CachedInputTokens < 0 ||
		resp.CacheWriteInputTokens < 0 ||
		resp.OutputTokens < 0 {
		return 0, fmt.Errorf("%w for %q: negative token usage", ErrAgentLoopCostUsageUnavailable, modelID)
	}

	if resp.InputTokens <= 0 &&
		resp.CachedInputTokens <= 0 &&
		resp.CacheWriteInputTokens <= 0 &&
		resp.OutputTokens <= 0 {
		return 0, fmt.Errorf("%w for %q", ErrAgentLoopCostUsageUnavailable, modelID)
	}

	if resp.InputTokens <= 0 {
		return 0, fmt.Errorf("%w for %q: input token usage unavailable", ErrAgentLoopCostUsageUnavailable, modelID)
	}

	if agentLoopResponseHasVisibleOutput(resp) && resp.OutputTokens <= 0 {
		return 0, fmt.Errorf("%w for %q: output token usage unavailable", ErrAgentLoopCostUsageUnavailable, modelID)
	}

	if resp.CachedInputTokens+resp.CacheWriteInputTokens > resp.InputTokens {
		return 0, fmt.Errorf("%w for %q: cache token usage exceeds input tokens", ErrAgentLoopCostUsageUnavailable, modelID)
	}

	return resp.InputTokens - resp.CachedInputTokens - resp.CacheWriteInputTokens, nil
}

func agentLoopPricingForUsage(metadata modelroute.ModelMetadata, resp *Response, uncachedTokens int) (float64, error) {
	if uncachedTokens > 0 && metadata.InputTokenCost <= 0 {
		return 0, fmt.Errorf("%w for %q: input token price unavailable", ErrAgentLoopCostPricingUnavailable, metadata.ID())
	}

	if resp.CachedInputTokens > 0 && metadata.CachedInputTokenCost <= 0 {
		return 0, fmt.Errorf("%w for %q: cached input token price unavailable", ErrAgentLoopCostPricingUnavailable, metadata.ID())
	}

	if resp.OutputTokens > 0 && metadata.OutputTokenCost <= 0 {
		return 0, fmt.Errorf("%w for %q: output token price unavailable", ErrAgentLoopCostPricingUnavailable, metadata.ID())
	}

	cacheWriteCost := metadata.CacheWriteTokenCost
	if cacheWriteCost <= 0 {
		cacheWriteCost = metadata.InputTokenCost
	}

	if resp.CacheWriteInputTokens > 0 && cacheWriteCost <= 0 {
		return 0, fmt.Errorf("%w for %q: cache-write input token price unavailable", ErrAgentLoopCostPricingUnavailable, metadata.ID())
	}

	return cacheWriteCost, nil
}
