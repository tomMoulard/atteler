// Package main demonstrates a credential-free one-shot chat SDK call.
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/sdk"
)

type fakeProvider struct{}

func (fakeProvider) Name() string { return "fake" }

func (fakeProvider) Models() []string { return []string{"fake-model"} }

func (fakeProvider) FetchModels(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("fetch fake models: %w", err)
	}

	return []string{"fake-model"}, nil
}

func (fakeProvider) HealthCheck(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("check fake provider health: %w", err)
	}

	return nil
}

func (fakeProvider) Complete(ctx context.Context, params llm.CompleteParams) (*llm.Response, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("complete fake response: %w", err)
	}

	prompt := ""

	for i := len(params.Messages) - 1; i >= 0; i-- {
		if params.Messages[i].Role == llm.RoleUser {
			prompt = params.Messages[i].Content
			break
		}
	}

	return &llm.Response{Content: "echo: " + prompt, Model: params.Model}, nil
}

func (fakeProvider) ModelContextWindow(model string) int {
	if model == "fake-model" {
		return 8192
	}

	return 0
}

func main() {
	registry, err := sdk.NewProviderRegistry(fakeProvider{})
	if err != nil {
		log.Fatal(err)
	}

	result, err := sdk.RunOneShotChat(exampleContext{}, sdk.OneShotChatOptions{
		Registry: registry,
		Model:    "fake-model",
		Prompt:   "Summarize the SDK surface",
	})
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println(result.Response.Content)
}

// exampleContext keeps examples free of process-root context creation; real applications should pass their request or command context.
type exampleContext struct{}

func (exampleContext) Deadline() (time.Time, bool) { return time.Time{}, false }

func (exampleContext) Done() <-chan struct{} { return nil }

func (exampleContext) Err() error { return nil }

func (exampleContext) Value(_ any) any { return nil }
