package llm

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/modelroute"
)

type fakeEmbeddingProvider struct {
	embeddingResp *EmbeddingResponse
	embeddingErr  error
	embeddingErrs []error
	embedCalls    []EmbeddingParams
	capabilities  ProviderCapabilities
	fakeProvider
}

func (f *fakeEmbeddingProvider) Capabilities() ProviderCapabilities {
	if f.capabilities.SupportsEmbeddings ||
		f.capabilities.SupportsChatCompletions ||
		len(f.capabilities.CompleteParams) > 0 {
		return f.capabilities
	}

	return ProviderCapabilities{SupportsEmbeddings: true}
}

type metadataLessEmbeddingProvider struct {
	embedCalls []EmbeddingParams
	fakeProvider
}

func (f *metadataLessEmbeddingProvider) Embed(_ context.Context, params EmbeddingParams) (*EmbeddingResponse, error) {
	f.embedCalls = append(f.embedCalls, params)

	return &EmbeddingResponse{
		Embeddings:  [][]float64{{1}},
		InputTokens: 1,
	}, nil
}

func (f *fakeEmbeddingProvider) Embed(_ context.Context, params EmbeddingParams) (*EmbeddingResponse, error) {
	f.embedCalls = append(f.embedCalls, params)
	if len(f.embeddingErrs) > 0 {
		err := f.embeddingErrs[0]
		f.embeddingErrs = f.embeddingErrs[1:]

		if err != nil {
			return nil, err
		}
	}

	if f.embeddingErr != nil {
		return nil, f.embeddingErr
	}

	if f.embeddingResp != nil {
		resp := *f.embeddingResp
		resp.Embeddings = cloneEmbeddingVectors(resp.Embeddings)

		return &resp, nil
	}

	return &EmbeddingResponse{
		Embeddings:  [][]float64{{1, 2, 3}},
		InputTokens: 4,
	}, nil
}

func TestRegistry_EmbedWithModelRoleRequiresEmbeddingCapability(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	chatOnly := &capabilityFakeProvider{
		fakeProvider: fakeProvider{
			name:   "chat-only",
			models: []string{"shared-model"},
			resp:   &Response{Content: "not embeddings"},
		},
		capabilities: ProviderCapabilities{SupportsChatCompletions: true},
	}
	embedder := &fakeEmbeddingProvider{
		fakeProvider: fakeProvider{
			name:   "embedder",
			models: []string{"shared-model"},
		},
	}

	r.Register(chatOnly)
	r.Register(embedder)
	require.NoError(t, r.SetModelRole("semantic_search", ModelRole{
		Preferred:      "chat-only/shared-model",
		FallbackModels: []string{"embedder/shared-model"},
	}))

	resp, err := r.EmbedWithFallback(context.Background(), EmbeddingParams{
		Model: "semantic_search",
		Input: []string{"find related code"},
	}, nil)
	require.NoError(t, err)

	assert.Equal(t, "embedder", resp.Provider)
	assert.Equal(t, "shared-model", resp.Model)
	assert.Equal(t, [][]float64{{1, 2, 3}}, resp.Embeddings)
	assert.Empty(t, chatOnly.calls)
	require.Len(t, embedder.embedCalls, 1)
	assert.Equal(t, "shared-model", embedder.embedCalls[0].Model)

	resolution, ok, err := r.resolveModelRoleWithCapabilities(
		"semantic_search",
		CompleteParams{},
		nil,
		[]string{modelroute.CapabilityEmbeddings},
	)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "embedder/shared-model", resolution.SelectedModel)
	assertRejectionContains(t, resolution.Decision, "chat-only/shared-model", modelroute.ReasonMissingCapability)
}

func TestRegistry_EmbedWithModelRoleRequiresActualEmbeddingProvider(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	metadataOnly := &capabilityFakeProvider{
		capabilities: ProviderCapabilities{SupportsEmbeddings: true},
		fakeProvider: fakeProvider{
			name:   "metadata-only",
			models: []string{"embed"},
			resp:   &Response{Content: "not embeddings"},
		},
	}
	embedder := &fakeEmbeddingProvider{
		fakeProvider: fakeProvider{
			name:   "embedder",
			models: []string{"embed"},
		},
	}

	r.Register(metadataOnly)
	r.Register(embedder)
	require.NoError(t, r.SetModelRole("semantic_search", ModelRole{
		Preferred:      "metadata-only/embed",
		FallbackModels: []string{"embedder/embed"},
	}))

	resp, err := r.EmbedWithFallback(context.Background(), EmbeddingParams{
		Model: "semantic_search",
		Input: []string{"find related code"},
	}, nil)
	require.NoError(t, err)

	assert.Equal(t, "embedder", resp.Provider)
	assert.Empty(t, metadataOnly.calls)
	require.Len(t, embedder.embedCalls, 1)

	resolution, ok, err := r.resolveModelRoleWithCapabilities(
		"semantic_search",
		CompleteParams{},
		nil,
		[]string{modelroute.CapabilityEmbeddings},
	)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "embedder/embed", resolution.SelectedModel)
	assertRejectionContains(t, resolution.Decision, "metadata-only/embed", modelroute.ReasonMissingCapability)
}

func TestRegistry_EmbedWithFallbackUsesNextProviderAfterSetupFailure(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	r.SetRetry(retryConfig{})

	first := &fakeEmbeddingProvider{
		fakeProvider: fakeProvider{
			name:   "first",
			models: []string{"embed"},
		},
		embeddingErr: errors.New("HTTP 503: warming up"),
	}
	second := &fakeEmbeddingProvider{
		fakeProvider: fakeProvider{
			name:   "second",
			models: []string{"embed"},
		},
		embeddingResp: &EmbeddingResponse{
			Embeddings: [][]float64{{0.5, 0.25}},
		},
	}

	r.Register(first)
	r.Register(second)

	resp, err := r.EmbedWithFallback(context.Background(), EmbeddingParams{
		Model: "first/embed",
		Input: []string{"query"},
	}, []string{"second/embed"})
	require.NoError(t, err)

	assert.Equal(t, "second", resp.Provider)
	assert.Equal(t, [][]float64{{0.5, 0.25}}, resp.Embeddings)
	require.Len(t, first.embedCalls, 1)
	require.Len(t, second.embedCalls, 1)
}

func TestRegistry_EmbedWithModelRoleFallsBackOnProviderFailure(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	r.SetRetry(retryConfig{})

	first := &fakeEmbeddingProvider{
		fakeProvider: fakeProvider{
			name:   "first",
			models: []string{"embed"},
		},
		embeddingErr: errors.New("HTTP 503: warming up"),
	}
	second := &fakeEmbeddingProvider{
		fakeProvider: fakeProvider{
			name:   "second",
			models: []string{"embed"},
		},
		embeddingResp: &EmbeddingResponse{
			Embeddings: [][]float64{{0.75, 0.25}},
		},
	}

	r.Register(first)
	r.Register(second)
	require.NoError(t, r.SetModelRole("semantic_search", ModelRole{
		Preferred:      "first/embed",
		FallbackModels: []string{"second/embed"},
	}))

	resp, err := r.Embed(context.Background(), EmbeddingParams{
		Model: "semantic_search",
		Input: []string{"query"},
	})
	require.NoError(t, err)

	assert.Equal(t, "second", resp.Provider)
	assert.Equal(t, [][]float64{{0.75, 0.25}}, resp.Embeddings)
	require.Len(t, first.embedCalls, 1)
	require.Len(t, second.embedCalls, 1)
}

func TestRegistry_EmbedWithModelRoleUsesEmbeddingInputBudget(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	embedder := &fakeEmbeddingProvider{
		fakeProvider: fakeProvider{
			name:   providerOpenAI,
			models: []string{"text-embedding-3-large", "text-embedding-3-small"},
		},
		embeddingResp: &EmbeddingResponse{
			Embeddings: [][]float64{{0.9, 0.1}},
		},
	}
	r.Register(embedder)

	input := strings.Repeat("embedding budget ", 1000)

	require.NoError(t, r.SetModelRole("semantic_search", ModelRole{
		Preferred:      "openai/text-embedding-3-large",
		FallbackModels: []string{"openai/text-embedding-3-small"},
		MaxCostUSD:     0.0002,
	}))

	resp, err := r.Embed(context.Background(), EmbeddingParams{
		Model: "semantic_search",
		Input: []string{input},
	})
	require.NoError(t, err)

	assert.Equal(t, "text-embedding-3-small", resp.Model)
	require.Len(t, embedder.embedCalls, 1)
	assert.Equal(t, "text-embedding-3-small", embedder.embedCalls[0].Model)

	resolution, ok, err := r.resolveModelRoleWithProfileAndCapabilities(
		"semantic_search",
		CompleteParams{Model: "semantic_search"},
		nil,
		[]string{modelroute.CapabilityEmbeddings},
		&modelroute.RequestProfile{EstimatedInputTokens: estimateEmbeddingInputTokens([]string{input})},
	)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "openai/text-embedding-3-small", resolution.SelectedModel)
	assertRejectionContains(t, resolution.Decision, "openai/text-embedding-3-large", modelroute.ReasonOverBudget)
}

func TestRegistry_EmbedRetriesTransientError(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	telemetry := modelroute.NewTelemetry()
	r.SetRouteTelemetry(telemetry)
	r.SetRetry(retryConfig{MaxAttempts: 1, InitialBackoff: time.Millisecond})

	embedder := &fakeEmbeddingProvider{
		fakeProvider: fakeProvider{
			name:   providerOpenAI,
			models: []string{"text-embedding-3-small"},
		},
		embeddingErrs: []error{
			retryableHTTPStatusError(errors.New("HTTP 503: warming up"), 503, ""),
			nil,
		},
		embeddingResp: &EmbeddingResponse{
			Model:       "text-embedding-3-small",
			Embeddings:  [][]float64{{0.1, 0.2}},
			InputTokens: 8,
		},
	}
	r.Register(embedder)

	resp, err := r.Embed(context.Background(), EmbeddingParams{
		Model: "openai/text-embedding-3-small",
		Input: []string{"query"},
	})
	require.NoError(t, err)

	assert.Equal(t, [][]float64{{0.1, 0.2}}, resp.Embeddings)
	require.Len(t, embedder.embedCalls, 2)

	obs, ok := telemetry.Snapshot("openai/text-embedding-3-small")
	require.True(t, ok)
	assert.Equal(t, 1, obs.Count)
	assert.Equal(t, 1, obs.FailureCount)
	assert.Empty(t, obs.LastError)
}

func TestRegistry_EmbedRejectsProviderWithoutEmbeddingInterface(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	r.Register(&fakeProvider{
		name:   "chat-only",
		models: []string{"chat-model"},
		resp:   &Response{Content: "chat"},
	})

	_, err := r.Embed(context.Background(), EmbeddingParams{
		Model: "chat-only/chat-model",
		Input: []string{"query"},
	})

	require.Error(t, err)
	require.ErrorIs(t, err, ErrEmbeddingsUnsupported)
}

func TestRegistry_EmbedRejectsProviderWithoutEmbeddingMetadata(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	embedder := &metadataLessEmbeddingProvider{
		fakeProvider: fakeProvider{
			name:   "custom",
			models: []string{"embed"},
		},
	}
	r.Register(embedder)

	_, err := r.Embed(context.Background(), EmbeddingParams{
		Model: "custom/embed",
		Input: []string{"query"},
	})

	require.Error(t, err)
	require.ErrorIs(t, err, ErrEmbeddingsUnsupported)
	assert.Empty(t, embedder.embedCalls)
}

func TestRegistry_EmbedRejectsProviderThatDeclaresEmbeddingsUnsupported(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	embedder := &fakeEmbeddingProvider{
		fakeProvider: fakeProvider{
			name:   "chat-only",
			models: []string{"model"},
		},
		capabilities: ProviderCapabilities{SupportsChatCompletions: true},
	}
	r.Register(embedder)

	_, err := r.Embed(context.Background(), EmbeddingParams{
		Model: "chat-only/model",
		Input: []string{"query"},
	})

	require.Error(t, err)
	require.ErrorIs(t, err, ErrEmbeddingsUnsupported)
	assert.Empty(t, embedder.embedCalls)
}

func TestRegistry_EmbedRecordsRouteTelemetry(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	telemetry := modelroute.NewTelemetry()
	r.SetRouteTelemetry(telemetry)
	r.Register(&fakeEmbeddingProvider{
		fakeProvider: fakeProvider{
			name:   providerOpenAI,
			models: []string{"text-embedding-3-small"},
		},
		embeddingResp: &EmbeddingResponse{
			Model:       "text-embedding-3-small",
			Embeddings:  [][]float64{{1, 0}},
			InputTokens: 12,
		},
	})

	_, err := r.Embed(context.Background(), EmbeddingParams{
		Model: "openai/text-embedding-3-small",
		Input: []string{"query"},
	})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		obs, ok := telemetry.Snapshot("openai/text-embedding-3-small")

		return ok && obs.Count == 1 && obs.InputTokens == 12 && obs.LastLatencyMS > 0
	}, time.Second, 10*time.Millisecond)
}

func cloneEmbeddingVectors(in [][]float64) [][]float64 {
	out := make([][]float64, len(in))
	for i := range in {
		out[i] = append([]float64(nil), in[i]...)
	}

	return out
}
