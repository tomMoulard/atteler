package llm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tommoulard/atteler/pkg/modelroute"
)

// ErrEmbeddingsUnsupported is returned when a resolved provider cannot serve
// embedding requests through the provider-agnostic embedding interface.
var ErrEmbeddingsUnsupported = errors.New("llm: provider does not support embeddings")

// EmbeddingParams groups provider-agnostic inputs for vector embedding calls.
// Model follows the same registry resolution rules as CompleteParams.Model and
// may be a configured model role.
type EmbeddingParams struct {
	Model      string
	Input      []string
	Dimensions int
}

// EmbeddingResponse is the provider-normalized result of an embedding call.
type EmbeddingResponse struct {
	Provider    string
	Model       string
	Embeddings  [][]float64
	Latency     time.Duration
	InputTokens int
}

// EmbeddingProvider is an optional provider interface for vector embeddings.
// Registry.Embed uses this interface after resolving model roles, providers,
// and fallbacks through the same routing layer as chat completions.
type EmbeddingProvider interface {
	Provider

	Embed(ctx context.Context, params EmbeddingParams) (*EmbeddingResponse, error)
}

// Embed resolves params.Model through the registry and performs one embedding
// request. If params.Model names a model role, the role is resolved with an
// implicit "embeddings" capability requirement.
func (r *Registry) Embed(ctx context.Context, params EmbeddingParams) (*EmbeddingResponse, error) {
	if err := requireCredentialContext(ctx); err != nil {
		return nil, err
	}

	if routedParams, routedFallbacks, routed, err := r.routeEmbeddingModelRole(params, nil); err != nil {
		return nil, err
	} else if routed {
		params = routedParams
		if len(routedFallbacks) > 0 {
			return r.embedResolvedWithFallback(ctx, params, routedFallbacks)
		}
	}

	return r.embedResolved(ctx, params)
}

// EmbedWithFallback tries params.Model followed by fallbackModels until one
// embedding request succeeds. Role fallbacks are resolved before provider calls.
func (r *Registry) EmbedWithFallback(
	ctx context.Context,
	params EmbeddingParams,
	fallbackModels []string,
) (*EmbeddingResponse, error) {
	if err := requireCredentialContext(ctx); err != nil {
		return nil, err
	}

	if routedParams, routedFallbacks, routed, err := r.routeEmbeddingModelRole(params, fallbackModels); err != nil {
		return nil, err
	} else if routed {
		params = routedParams
		fallbackModels = routedFallbacks
	}

	models := modelFallbackChain(params.Model, fallbackModels)
	if len(models) == 0 {
		return r.Embed(ctx, params)
	}

	var failures []error

	for _, model := range models {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("llm: embedding fallback canceled: %w", err)
		}

		next := params
		next.Model = model

		resp, err := r.Embed(ctx, next)
		if err == nil {
			return resp, nil
		}

		failures = append(failures, fmt.Errorf("%s via %s: %w", model, r.modelResolutionLabel(model), err))
	}

	return nil, r.withReadinessContext(fmt.Errorf("llm: all embedding fallback models failed: %w", joinFallbackFailures(failures)))
}

func (r *Registry) embedResolvedWithFallback(
	ctx context.Context,
	params EmbeddingParams,
	fallbackModels []string,
) (*EmbeddingResponse, error) {
	models := modelFallbackChain(params.Model, fallbackModels)
	if len(models) == 0 {
		return r.embedResolved(ctx, params)
	}

	var failures []error

	for _, model := range models {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("llm: embedding fallback canceled: %w", err)
		}

		next := params
		next.Model = model

		resp, err := r.embedResolved(ctx, next)
		if err == nil {
			return resp, nil
		}

		failures = append(failures, fmt.Errorf("%s via %s: %w", model, r.modelResolutionLabel(model), err))
	}

	return nil, r.withReadinessContext(fmt.Errorf("llm: all embedding fallback models failed: %w", joinFallbackFailures(failures)))
}

func (r *Registry) routeEmbeddingModelRole(
	params EmbeddingParams,
	fallbackModels []string,
) (routedParams EmbeddingParams, routedFallbacks []string, routed bool, err error) {
	completeParams := CompleteParams{Model: params.Model}
	profile := modelroute.RequestProfile{
		EstimatedInputTokens: estimateEmbeddingInputTokens(params.Input),
		Interactive:          false,
	}

	resolution, ok, err := r.resolveModelRoleWithProfileAndCapabilities(
		strings.TrimSpace(params.Model),
		completeParams,
		fallbackModels,
		[]string{modelroute.CapabilityEmbeddings},
		&profile,
	)
	if !ok || err != nil {
		return params, fallbackModels, ok, err
	}

	params.Model = resolution.SelectedModel

	return params, resolution.FallbackModels, true, nil
}

func estimateEmbeddingInputTokens(input []string) int {
	var total int

	for _, text := range input {
		base := estimateLegacyTextTokens(text)
		total += base + ceilLegacyEstimateDiv(base*legacyEstimateErrorBoundPercent, 100)
	}

	return total
}

func (r *Registry) embedResolved(ctx context.Context, params EmbeddingParams) (*EmbeddingResponse, error) {
	if len(params.Input) == 0 {
		return nil, errors.New("llm: embedding input cannot be empty")
	}

	if params.Dimensions < 0 {
		return nil, errors.New("llm: embedding dimensions cannot be negative")
	}

	provider, resolvedModel, err := r.resolveEmbeddingProvider(params.Model)
	if err != nil {
		return nil, err
	}

	params.Model = resolvedModel

	embedder, ok := provider.(EmbeddingProvider)
	if !ok {
		unsupportedErr := fmt.Errorf("%w: %s", ErrEmbeddingsUnsupported, provider.Name())
		r.recordRouteFailure(provider.Name(), params.Model, unsupportedErr)

		return nil, unsupportedErr
	}

	if providerDeclaresEmbeddingsUnsupported(provider) {
		unsupportedErr := fmt.Errorf("%w: %s capabilities do not include embeddings", ErrEmbeddingsUnsupported, provider.Name())
		r.recordRouteFailure(provider.Name(), params.Model, unsupportedErr)

		return nil, unsupportedErr
	}

	startedAt := time.Now()

	r.mu.RLock()
	retryCfg := r.retryPolicyForProviderLocked(provider.Name())
	r.mu.RUnlock()

	resp, err := embeddingWithRetry(ctx, retryCfg, retryMetadata{provider: provider.Name(), model: params.Model}, func(ctx context.Context) (*EmbeddingResponse, error) {
		providerResp, embedErr := embedder.Embed(ctx, params)
		if embedErr != nil {
			wrappedErr := fmt.Errorf("llm: %s embeddings: %w", provider.Name(), embedErr)
			r.recordRouteFailure(provider.Name(), params.Model, wrappedErr)

			return nil, wrappedErr
		}

		return providerResp, nil
	})
	if err != nil {
		return nil, err
	}

	latency := time.Since(startedAt)

	if resp == nil {
		resp = &EmbeddingResponse{}
	}

	if resp.Latency <= 0 {
		resp.Latency = latency
	}

	if resp.Provider == "" {
		resp.Provider = provider.Name()
	}

	if resp.Model == "" {
		resp.Model = params.Model
	}

	r.recordRouteObservation(provider.Name(), params.Model, &Response{
		Model:       resp.Model,
		InputTokens: resp.InputTokens,
		Latency:     resp.Latency,
	}, resp.Latency)

	return resp, nil
}

func (r *Registry) resolveEmbeddingProvider(model string) (Provider, string, error) {
	p, completeParams, err := r.resolve(CompleteParams{Model: model})
	if err != nil {
		return nil, "", err
	}

	return p, completeParams.Model, nil
}

func providerDeclaresEmbeddingsUnsupported(provider Provider) bool {
	if provider == nil {
		return true
	}

	return !ProviderCapabilitiesFor(provider).SupportsEmbeddings
}
