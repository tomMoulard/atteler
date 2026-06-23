package main

import (
	"context"
	"fmt"

	"github.com/tommoulard/atteler/pkg/llm"
)

func completeRegistryStreamWithFallback(
	ctx context.Context,
	reg *llm.Registry,
	params llm.CompleteParams,
	fallbackModels []string,
) (*llm.Response, error) {
	ch, err := reg.CompleteStreamWithFallback(ctx, params, fallbackModels)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%w", err)
		}

		resp, fallbackErr := reg.CompleteWithFallback(ctx, params, fallbackModels)
		normalizeStreamedRegistryResponse(reg, resp, params.Model)

		if fallbackErr != nil {
			return resp, fmt.Errorf("%w", fallbackErr)
		}

		return resp, nil
	}

	resp, err := llm.CollectStream(ch)
	normalizeStreamedRegistryResponse(reg, resp, params.Model)

	if err != nil {
		return resp, fmt.Errorf("collect registry stream: %w", err)
	}

	return resp, nil
}

func completeRegistryStream(
	ctx context.Context,
	reg *llm.Registry,
	params llm.CompleteParams,
) (*llm.Response, error) {
	ch, err := reg.CompleteStream(ctx, params)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%w", err)
		}

		resp, fallbackErr := reg.Complete(ctx, params)
		normalizeStreamedRegistryResponse(reg, resp, params.Model)

		if fallbackErr != nil {
			return resp, fmt.Errorf("%w", fallbackErr)
		}

		return resp, nil
	}

	resp, err := llm.CollectStream(ch)
	normalizeStreamedRegistryResponse(reg, resp, params.Model)

	if err != nil {
		return resp, fmt.Errorf("collect registry stream: %w", err)
	}

	return resp, nil
}

func normalizeStreamedRegistryResponse(reg *llm.Registry, resp *llm.Response, requestedModel string) {
	if resp == nil {
		return
	}

	if resp.Provider == "" {
		resp.Provider = providerNameForModel(reg, firstNonEmptyString(resp.Model, requestedModel))
	}

	if resp.Model == "" {
		resp.Model = requestedModel
	}
}
