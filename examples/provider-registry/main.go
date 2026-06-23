// Package main demonstrates creating and resolving an SDK provider registry.
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/sdk"
)

type staticProvider struct{}

func (staticProvider) Name() string { return "static" }

func (staticProvider) Models() []string { return []string{"static-model"} }

func (staticProvider) FetchModels(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("fetch static models: %w", err)
	}

	return []string{"static-model"}, nil
}

func (staticProvider) HealthCheck(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("check static provider health: %w", err)
	}

	return nil
}

func (staticProvider) Complete(ctx context.Context, params llm.CompleteParams) (*llm.Response, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("complete static response: %w", err)
	}

	return &llm.Response{Content: "registered", Model: params.Model}, nil
}

func (staticProvider) ModelContextWindow(model string) int {
	if model == "static-model" {
		return 4096
	}

	return 0
}

func main() {
	registry, err := sdk.NewProviderRegistry(staticProvider{})
	if err != nil {
		log.Fatal(err)
	}

	provider, model, ok := registry.ResolveModel("static-model")
	fmt.Printf("provider=%s model=%s ok=%t\n", provider, model, ok)
}
