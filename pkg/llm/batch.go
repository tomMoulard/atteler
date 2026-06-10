package llm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tommoulard/atteler/pkg/modelroute"
)

// ErrBatchUnsupported is returned when a resolved provider declares that it
// cannot safely serve batch completion requests.
var ErrBatchUnsupported = errors.New("llm: provider does not support batch completions")

// BatchCompleteParams groups a set of completion requests that should be
// routed as one batch. Model is the common model or model role for the batch;
// per-request Model fields may be left empty or set to the same value.
type BatchCompleteParams struct {
	Model    string
	Requests []CompleteParams
}

// BatchResponse is the provider-normalized result of a batch completion call.
type BatchResponse struct {
	Responses []*Response
	Latency   time.Duration
}

// CompleteBatch runs a provider-agnostic batch of chat completions. Model roles
// are resolved with an implicit "batch" capability requirement, then each item
// is sent through the same retry, fallback, validation, and telemetry path as a
// normal completion.
func (r *Registry) CompleteBatch(ctx context.Context, params BatchCompleteParams) (*BatchResponse, error) {
	return r.CompleteBatchWithFallback(ctx, params, nil)
}

// CompleteBatchWithFallback tries the batch model followed by fallbackModels
// until every request in the batch succeeds against one compatible model.
func (r *Registry) CompleteBatchWithFallback(
	ctx context.Context,
	params BatchCompleteParams,
	fallbackModels []string,
) (*BatchResponse, error) {
	if err := requireCredentialContext(ctx); err != nil {
		return nil, err
	}

	commonModel, err := batchCommonModel(params)
	if err != nil {
		return nil, err
	}

	params.Model = commonModel

	if routedParams, routedFallbacks, routed, err := r.routeBatchModelRole(params, fallbackModels); err != nil {
		return nil, err
	} else if routed {
		params = routedParams
		fallbackModels = routedFallbacks
	}

	return r.completeBatchResolvedWithFallback(ctx, params, fallbackModels)
}

func (r *Registry) routeBatchModelRole(
	params BatchCompleteParams,
	fallbackModels []string,
) (routedParams BatchCompleteParams, routedFallbacks []string, routed bool, err error) {
	profile := batchRouteProfile(params.Requests)
	extraCapabilities := batchRequiredCapabilities(params.Requests)
	extraCapabilities = completionRouteCapabilities(extraCapabilities)
	extraCapabilities = appendUniqueString(extraCapabilities, modelroute.CapabilityBatch)

	resolution, ok, err := r.resolveModelRoleWithProfileAndCapabilities(
		strings.TrimSpace(params.Model),
		batchRoutingCompleteParams(params),
		fallbackModels,
		extraCapabilities,
		&profile,
	)
	if !ok || err != nil {
		return params, fallbackModels, ok, err
	}

	params.Model = resolution.SelectedModel

	return params, resolution.FallbackModels, true, nil
}

func (r *Registry) completeBatchResolvedWithFallback(
	ctx context.Context,
	params BatchCompleteParams,
	fallbackModels []string,
) (*BatchResponse, error) {
	models := modelFallbackChain(params.Model, fallbackModels)
	if len(models) == 0 {
		return r.completeBatchResolved(ctx, params)
	}

	var failures []error

	for _, model := range models {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("llm: batch fallback canceled: %w", err)
		}

		next := params
		next.Model = model

		resp, err := r.completeBatchRoutedRoleOrResolved(ctx, next)
		if err == nil {
			return resp, nil
		}

		failures = append(failures, fmt.Errorf("%s via %s: %w", model, r.modelResolutionLabel(model), err))
	}

	return nil, r.withReadinessContext(fmt.Errorf("llm: all batch fallback models failed: %w", joinFallbackFailures(failures)))
}

func (r *Registry) completeBatchRoutedRoleOrResolved(ctx context.Context, params BatchCompleteParams) (*BatchResponse, error) {
	routedParams, routedFallbacks, routed, err := r.routeBatchModelRole(params, nil)
	if err != nil {
		return nil, err
	}

	if routed {
		return r.completeBatchResolvedWithFallback(ctx, routedParams, routedFallbacks)
	}

	return r.completeBatchResolved(ctx, params)
}

func (r *Registry) completeBatchResolved(ctx context.Context, params BatchCompleteParams) (*BatchResponse, error) {
	if len(params.Requests) == 0 {
		return nil, errors.New("llm: batch requests cannot be empty")
	}

	resolvedModel, err := r.batchResolvedModel(params.Model)
	if err != nil {
		return nil, err
	}

	params.Model = resolvedModel

	startedAt := time.Now()
	responses := make([]*Response, 0, len(params.Requests))

	for i := range params.Requests {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("llm: batch canceled: %w", err)
		}

		next := params.Requests[i]
		next.Model = params.Model

		resp, err := r.completeResolved(ctx, next, r.fallbackRetryConfig(false))
		if err != nil {
			return nil, fmt.Errorf("batch request %d: %w", i, err)
		}

		responses = append(responses, resp)
	}

	return &BatchResponse{
		Responses: responses,
		Latency:   time.Since(startedAt),
	}, nil
}

func (r *Registry) batchResolvedModel(model string) (string, error) {
	provider, completeParams, err := r.resolve(CompleteParams{Model: model})
	if err != nil {
		return "", err
	}

	if providerDeclaresBatchUnsupported(provider) {
		unsupportedErr := fmt.Errorf("%w: %s", ErrBatchUnsupported, provider.Name())
		r.recordRouteFailure(provider.Name(), completeParams.Model, unsupportedErr)

		return "", unsupportedErr
	}

	return completeParams.Model, nil
}

func batchCommonModel(params BatchCompleteParams) (string, error) {
	model := strings.TrimSpace(params.Model)

	for i := range params.Requests {
		requestModel := strings.TrimSpace(params.Requests[i].Model)
		if requestModel == "" {
			continue
		}

		if model == "" {
			model = requestModel

			continue
		}

		if model != requestModel {
			return "", fmt.Errorf("llm: batch request %d uses model %q, expected common model %q", i, requestModel, model)
		}
	}

	return model, nil
}

func batchRouteProfile(requests []CompleteParams) modelroute.RequestProfile {
	profile := modelroute.RequestProfile{Batch: true}

	for i := range requests {
		profile.EstimatedInputTokens += EstimateTokens(requests[i].Messages)
		profile.EstimatedOutputTokens += max(0, requests[i].MaxTokens)
	}

	return profile
}

func batchRoutingCompleteParams(params BatchCompleteParams) CompleteParams {
	out := CompleteParams{Model: params.Model}

	for i := range params.Requests {
		request := params.Requests[i]
		if anthropicReasoningImpossibleForRequest(request) {
			out.ReasoningLevel = request.ReasoningLevel
			out.MaxTokens = request.MaxTokens

			return out
		}

		if out.ReasoningLevel == "" && reasoningCapabilityRequested(request.ReasoningLevel) {
			out.ReasoningLevel = request.ReasoningLevel
		}
	}

	return out
}

func batchRequiredCapabilities(requests []CompleteParams) []string {
	var capabilities []string

	for i := range requests {
		for _, capability := range completeParamsRequiredCapabilities(requests[i]) {
			capabilities = appendUniqueString(capabilities, capability)
		}
	}

	return capabilities
}

func providerDeclaresBatchUnsupported(provider Provider) bool {
	if provider == nil {
		return true
	}

	return !ProviderCapabilitiesFor(provider).SupportsBatch
}
