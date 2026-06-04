package llm

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/modelroute"
)

func TestRegistry_CompleteBatchWithModelRoleRequiresBatchCapability(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	ollama := &fakeProvider{
		name:   providerOllama,
		models: []string{"llama3.2"},
		resp:   &Response{Content: "local"},
	}
	openAI := &fakeProvider{
		name:   providerOpenAI,
		models: []string{"gpt-4.1-mini"},
		resp:   &Response{Content: "remote"},
	}

	r.Register(ollama)
	r.Register(openAI)
	require.NoError(t, r.SetModelRole("batch_writer", ModelRole{
		Preferred:      "ollama/llama3.2",
		FallbackModels: []string{"openai/gpt-4.1-mini"},
	}))

	resp, err := r.CompleteBatch(context.Background(), BatchCompleteParams{
		Model: "batch_writer",
		Requests: []CompleteParams{{
			Messages: []Message{{Role: RoleUser, Content: "summarize"}},
		}},
	})
	require.NoError(t, err)

	require.Len(t, resp.Responses, 1)
	assert.Equal(t, "remote", resp.Responses[0].Content)
	assert.Empty(t, ollama.calls)
	require.Len(t, openAI.calls, 1)
	assert.Equal(t, "gpt-4.1-mini", openAI.calls[0].Model)

	resolution, ok, err := r.resolveModelRoleWithProfileAndCapabilities(
		"batch_writer",
		CompleteParams{Model: "batch_writer"},
		nil,
		[]string{modelroute.CapabilityBatch},
		&modelroute.RequestProfile{Batch: true, EstimatedInputTokens: 10},
	)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "openai/gpt-4.1-mini", resolution.SelectedModel)
	assertRejectionContains(t, resolution.Decision, "ollama/llama3.2", modelroute.ReasonMissingCapability)
}

func TestRegistry_CompleteBatchWithModelRoleRequiresChatCapabilityEvenWithoutMessages(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	embedder := &capabilityFakeProvider{
		capabilities: ProviderCapabilities{
			SupportsBatch:      true,
			SupportsEmbeddings: true,
		},
		fakeProvider: fakeProvider{
			name:   "embedder",
			models: []string{"embed-only"},
			resp:   &Response{Content: "not a chat completion"},
		},
	}
	openAI := &fakeProvider{
		name:   providerOpenAI,
		models: []string{"gpt-4.1-mini"},
		resp:   &Response{Content: "chat batch"},
	}

	r.Register(embedder)
	r.Register(openAI)
	require.NoError(t, r.SetModelRole("batch_writer", ModelRole{
		Preferred:      "embedder/embed-only",
		FallbackModels: []string{"openai/gpt-4.1-mini"},
	}))

	resp, err := r.CompleteBatch(context.Background(), BatchCompleteParams{
		Model:    "batch_writer",
		Requests: []CompleteParams{{}},
	})
	require.NoError(t, err)

	require.Len(t, resp.Responses, 1)
	assert.Equal(t, "chat batch", resp.Responses[0].Content)
	assert.Empty(t, embedder.calls)
	require.Len(t, openAI.calls, 1)
	assert.Equal(t, "gpt-4.1-mini", openAI.calls[0].Model)
}

func TestRegistry_CompleteBatchModelRoleRejectsAnthropicReasoningWhenMaxTokensTooSmall(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	anthropic := &fakeProvider{
		name:   providerAnthropic,
		models: []string{"claude-sonnet-4-6"},
		resp:   &Response{Content: "anthropic"},
	}
	openAI := &fakeProvider{
		name:   providerOpenAI,
		models: []string{"gpt-5.4-mini"},
		resp:   &Response{Content: "openai"},
	}

	r.Register(anthropic)
	r.Register(openAI)
	require.NoError(t, r.SetModelRole("batch_reasoner", ModelRole{
		Preferred:      "anthropic/claude-sonnet-4-6",
		FallbackModels: []string{"openai/gpt-5.4-mini"},
	}))

	resp, err := r.CompleteBatch(context.Background(), BatchCompleteParams{
		Model: "batch_reasoner",
		Requests: []CompleteParams{{
			ReasoningLevel: reasoningLevelHigh,
			MaxTokens:      16,
			Messages:       []Message{{Role: RoleUser, Content: "think"}},
		}},
	})
	require.NoError(t, err)

	require.Len(t, resp.Responses, 1)
	assert.Equal(t, "openai", resp.Responses[0].Content)
	assert.Empty(t, anthropic.calls)
	require.Len(t, openAI.calls, 1)
	assert.Equal(t, "gpt-5.4-mini", openAI.calls[0].Model)
	assert.Equal(t, reasoningLevelHigh, openAI.calls[0].ReasoningLevel)

	resolution, ok, err := r.resolveModelRoleWithProfileAndCapabilities(
		"batch_reasoner",
		batchRoutingCompleteParams(BatchCompleteParams{
			Model: "batch_reasoner",
			Requests: []CompleteParams{{
				ReasoningLevel: reasoningLevelHigh,
				MaxTokens:      16,
			}},
		}),
		nil,
		[]string{modelroute.CapabilityBatch, modelroute.CapabilityReasoning},
		&modelroute.RequestProfile{Batch: true, EstimatedInputTokens: 10, EstimatedOutputTokens: 16},
	)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "openai/gpt-5.4-mini", resolution.SelectedModel)
	assertRejectionContains(t, resolution.Decision, "anthropic/claude-sonnet-4-6", modelroute.ReasonMissingCapability)
	assertRejectionContains(t, resolution.Decision, "anthropic/claude-sonnet-4-6", modelroute.CapabilityReasoning)
}

func TestRegistry_CompleteBatchWithModelRoleFallsBackOnProviderFailure(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	r.SetRetry(retryConfig{})

	openAI := &fakeProvider{
		err:    errors.New("HTTP 503: batch warming up"),
		name:   providerOpenAI,
		models: []string{"gpt-4.1-mini"},
	}
	anthropic := &fakeProvider{
		name:   providerAnthropic,
		models: []string{"claude-sonnet-4-20250514"},
		resp:   &Response{Content: "fallback"},
	}

	r.Register(openAI)
	r.Register(anthropic)
	require.NoError(t, r.SetModelRole("batch_writer", ModelRole{
		Preferred:      "openai/gpt-4.1-mini",
		FallbackModels: []string{"anthropic/claude-sonnet-4-20250514"},
	}))

	resp, err := r.CompleteBatch(context.Background(), BatchCompleteParams{
		Model: "batch_writer",
		Requests: []CompleteParams{
			{Messages: []Message{{Role: RoleUser, Content: "one"}}},
			{Messages: []Message{{Role: RoleUser, Content: "two"}}},
		},
	})
	require.NoError(t, err)

	require.Len(t, resp.Responses, 2)
	assert.Equal(t, "fallback", resp.Responses[0].Content)
	assert.Equal(t, "fallback", resp.Responses[1].Content)
	require.Len(t, openAI.calls, 1)
	require.Len(t, anthropic.calls, 2)
	assert.Equal(t, "claude-sonnet-4-20250514", anthropic.calls[0].Model)
	assert.Positive(t, resp.Latency)
}

func TestRegistry_CompleteBatchWithFallbackRoutesFallbackModelRole(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	r.SetRetry(retryConfig{})

	openAI := &fakeProvider{
		err:    errors.New("HTTP 503: batch warming up"),
		name:   providerOpenAI,
		models: []string{"gpt-4.1-mini"},
	}
	ollama := &fakeProvider{
		name:   providerOllama,
		models: []string{"llama3.2"},
		resp:   &Response{Content: "local should not batch"},
	}
	anthropic := &fakeProvider{
		name:   providerAnthropic,
		models: []string{"claude-sonnet-4-20250514"},
		resp:   &Response{Content: "role fallback"},
	}

	r.Register(openAI)
	r.Register(ollama)
	r.Register(anthropic)
	require.NoError(t, r.SetModelRole("batch_fallback", ModelRole{
		Preferred:      "ollama/llama3.2",
		FallbackModels: []string{"anthropic/claude-sonnet-4-20250514"},
	}))

	resp, err := r.CompleteBatchWithFallback(context.Background(), BatchCompleteParams{
		Model: "openai/gpt-4.1-mini",
		Requests: []CompleteParams{{
			Messages: []Message{{Role: RoleUser, Content: "summarize"}},
		}},
	}, []string{"batch_fallback"})
	require.NoError(t, err)

	require.Len(t, resp.Responses, 1)
	assert.Equal(t, "role fallback", resp.Responses[0].Content)
	require.Len(t, openAI.calls, 1)
	assert.Empty(t, ollama.calls)
	require.Len(t, anthropic.calls, 1)
	assert.Equal(t, "claude-sonnet-4-20250514", anthropic.calls[0].Model)
}

func TestRegistry_CompleteBatchRejectsBatchUnsupportedProvider(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	ollama := &fakeProvider{
		name:   providerOllama,
		models: []string{"llama3.2"},
		resp:   &Response{Content: "should not batch"},
	}
	r.Register(ollama)

	_, err := r.CompleteBatch(context.Background(), BatchCompleteParams{
		Model: "ollama/llama3.2",
		Requests: []CompleteParams{{
			Messages: []Message{{Role: RoleUser, Content: "summarize"}},
		}},
	})

	require.Error(t, err)
	require.ErrorIs(t, err, ErrBatchUnsupported)
	assert.Empty(t, ollama.calls)
}

func TestRegistry_CompleteBatchWithFallbackSkipsBatchUnsupportedProvider(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	ollama := &fakeProvider{
		name:   providerOllama,
		models: []string{"llama3.2"},
		resp:   &Response{Content: "should not batch"},
	}
	openAI := &fakeProvider{
		name:   providerOpenAI,
		models: []string{"gpt-4.1-mini"},
		resp:   &Response{Content: "fallback batch"},
	}

	r.Register(ollama)
	r.Register(openAI)

	resp, err := r.CompleteBatchWithFallback(context.Background(), BatchCompleteParams{
		Model: "ollama/llama3.2",
		Requests: []CompleteParams{{
			Messages: []Message{{Role: RoleUser, Content: "summarize"}},
		}},
	}, []string{"openai/gpt-4.1-mini"})
	require.NoError(t, err)

	require.Len(t, resp.Responses, 1)
	assert.Equal(t, "fallback batch", resp.Responses[0].Content)
	assert.Empty(t, ollama.calls)
	require.Len(t, openAI.calls, 1)
	assert.Equal(t, "gpt-4.1-mini", openAI.calls[0].Model)
}

func TestRegistry_CompleteBatchRejectsProviderWithoutBatchMetadata(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	provider := &fakeProvider{
		name:   "custom",
		models: []string{"chat"},
		resp:   &Response{Content: "should not batch without metadata"},
	}
	r.Register(provider)

	_, err := r.CompleteBatch(context.Background(), BatchCompleteParams{
		Model: "custom/chat",
		Requests: []CompleteParams{{
			Messages: []Message{{Role: RoleUser, Content: "summarize"}},
		}},
	})

	require.Error(t, err)
	require.ErrorIs(t, err, ErrBatchUnsupported)
	assert.Empty(t, provider.calls)
}

func TestRegistry_CompleteBatchRejectsMixedModels(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	r.Register(&fakeProvider{
		name:   providerOpenAI,
		models: []string{"gpt-4.1-mini", "gpt-4.1-nano"},
		resp:   &Response{Content: "unused"},
	})

	_, err := r.CompleteBatch(context.Background(), BatchCompleteParams{
		Requests: []CompleteParams{
			{Model: "openai/gpt-4.1-mini"},
			{Model: "openai/gpt-4.1-nano"},
		},
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "expected common model")
}

func TestRegistry_CompleteBatchRejectsEmptyRequests(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	r.Register(&fakeProvider{
		name:   providerOpenAI,
		models: []string{"gpt-4.1-mini"},
		resp:   &Response{Content: "unused"},
	})

	_, err := r.CompleteBatch(context.Background(), BatchCompleteParams{
		Model: "openai/gpt-4.1-mini",
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "batch requests cannot be empty")
}
