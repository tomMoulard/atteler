package llm

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/modelroute"
)

func TestStreamFromComplete_SingleChunk(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{
		name:   "test",
		models: []string{"m-1"},
		resp: &Response{
			Content:               "hello",
			Model:                 "m-1",
			StopReason:            StopEndTurn,
			ToolCalls:             []ToolCall{{ID: "tool-1", Name: "bash", Input: map[string]any{"command": "pwd"}}},
			FirstTokenLatency:     15 * time.Millisecond,
			InputTokens:           10,
			CacheWriteInputTokens: 2,
			OutputTokens:          5,
		},
	}

	ch, err := StreamFromComplete(context.Background(), p, CompleteParams{Model: "m-1"})
	require.NoError(t, err)

	chunks := drainChunks(ch)
	require.Len(t, chunks, 1)
	assert.Equal(t, "hello", chunks[0].Content)
	assert.Equal(t, "m-1", chunks[0].Model)
	assert.True(t, chunks[0].Done)
	assert.Equal(t, StopEndTurn, chunks[0].StopReason)
	require.Len(t, chunks[0].ToolCalls, 1)
	assert.Equal(t, "tool-1", chunks[0].ToolCalls[0].ID)
	assert.Equal(t, 15*time.Millisecond, chunks[0].FirstTokenLatency)
	assert.Equal(t, 10, chunks[0].InputTokens)
	assert.Equal(t, 2, chunks[0].CacheWriteInputTokens)
	assert.Equal(t, 5, chunks[0].OutputTokens)
}

func TestCompleteStreamOrFallback_UsesStreamProvider(t *testing.T) {
	t.Parallel()

	sp := &fakeStreamProvider{
		fakeProvider: fakeProvider{
			name:   "stream-test",
			models: []string{"s-1"},
			resp:   &Response{Content: "streamed"},
		},
		chunks: []Chunk{
			{Content: "token1"},
			{Content: "token2", Done: true, StopReason: StopMaxToks, InputTokens: 5, OutputTokens: 2},
		},
	}

	ch, err := CompleteStreamOrFallback(context.Background(), sp, CompleteParams{Model: "s-1"})
	require.NoError(t, err)

	chunks := drainChunks(ch)
	require.NotEmpty(t, chunks)
	assert.Equal(t, "token1", chunks[0].Content)
	assert.True(t, chunks[len(chunks)-1].Done)
	assert.Equal(t, StopMaxToks, chunks[len(chunks)-1].StopReason)
}

func TestCompleteStreamOrFallback_FallsBackToNonStream(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{
		name:   "non-stream",
		models: []string{"n-1"},
		resp:   &Response{Content: "full response", Model: "n-1", StopReason: StopEndTurn},
	}

	ch, err := CompleteStreamOrFallback(context.Background(), p, CompleteParams{Model: "n-1"})
	require.NoError(t, err)

	resp, err := CollectStream(ch)
	require.NoError(t, err)
	assert.Equal(t, "full response", resp.Content)
	assert.Equal(t, "n-1", resp.Model)
	assert.Equal(t, StopEndTurn, resp.StopReason)
}

func TestRegistry_CompleteStreamFallsBackWhenProviderDeclaresStreamingUnsupported(t *testing.T) {
	t.Parallel()

	provider := &capabilityFakeStreamProvider{
		capabilities: ProviderCapabilities{
			SupportsChatCompletions: true,
			SupportsStreaming:       false,
		},
		fakeStreamProvider: fakeStreamProvider{
			fakeProvider: fakeProvider{
				name:   "buffered-stream",
				models: []string{"model"},
				resp:   &Response{Content: "buffered", StopReason: StopEndTurn},
			},
			chunks: []Chunk{{Content: "should not stream", Done: true}},
		},
	}

	r := NewRegistry()
	r.Register(provider)

	ch, err := r.CompleteStream(context.Background(), CompleteParams{
		Model:    "buffered-stream/model",
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
	})
	require.NoError(t, err)

	resp, err := CollectStream(ch)
	require.NoError(t, err)
	assert.Equal(t, "buffered", resp.Content)
	assert.Empty(t, provider.streamCalls)
	require.Len(t, provider.calls, 1)
}

func TestRegistry_CompleteStreamRejectsUnsupportedDeclaredProviderParams(t *testing.T) {
	t.Parallel()

	provider := &capabilityFakeStreamProvider{
		capabilities: ProviderCapabilities{
			SupportsChatCompletions: true,
			SupportsStreaming:       true,
			CompleteParams: map[string]CompleteParamSupport{
				"Tools": unsupported("stream endpoint does not support tools"),
			},
		},
		fakeStreamProvider: fakeStreamProvider{
			fakeProvider: fakeProvider{
				name:   "stream-compatible",
				models: []string{"stream-coder"},
			},
			chunks: []Chunk{{Content: "should not stream", Done: true}},
		},
	}

	r := NewRegistry()
	r.Register(provider)

	_, err := r.CompleteStream(context.Background(), CompleteParams{
		Model:    "stream-compatible/stream-coder",
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
		Tools:    DefaultTools(),
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "CompleteParams.Tools is unsupported")
	assert.Empty(t, provider.streamCalls)
}

func TestStreamHelpers_RequireActiveContext(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{
		name:   "non-stream",
		models: []string{"n-1"},
		resp:   &Response{Content: "full response", Model: "n-1"},
	}

	_, err := StreamFromComplete(nil, p, CompleteParams{Model: "n-1"}) //nolint:staticcheck // Verify nil contexts are rejected instead of panicking.
	require.Error(t, err)
	require.ErrorIs(t, err, ErrContextRequired)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = CompleteStreamOrFallback(ctx, p, CompleteParams{Model: "n-1"})
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
}

func TestCollectStream_AssemblesResponse(t *testing.T) {
	t.Parallel()

	ch := make(chan Chunk, 3)

	ch <- Chunk{Content: "Hello "}

	ch <- Chunk{Content: "world", Model: "m-1"}

	ch <- Chunk{
		Content:               "!",
		Done:                  true,
		StopReason:            StopEndTurn,
		FirstTokenLatency:     25 * time.Millisecond,
		InputTokens:           10,
		CachedInputTokens:     2,
		CacheWriteInputTokens: 1,
		OutputTokens:          3,
		Model:                 "m-1",
	}

	close(ch)

	resp, err := CollectStream(ch)
	require.NoError(t, err)
	assert.Equal(t, "Hello world!", resp.Content)
	assert.Equal(t, "m-1", resp.Model)
	assert.Equal(t, StopEndTurn, resp.StopReason)
	assert.Equal(t, 25*time.Millisecond, resp.FirstTokenLatency)
	assert.Equal(t, 10, resp.InputTokens)
	assert.Equal(t, 2, resp.CachedInputTokens)
	assert.Equal(t, 1, resp.CacheWriteInputTokens)
	assert.Equal(t, 3, resp.OutputTokens)
}

func TestCollectStream_KeepsFirstTokenLatencyFromInitialToken(t *testing.T) {
	t.Parallel()

	ch := make(chan Chunk, 3)
	ch <- Chunk{Content: "first", FirstTokenLatency: 12 * time.Millisecond}

	ch <- Chunk{Content: " second"}

	ch <- Chunk{Done: true, InputTokens: 4, OutputTokens: 2}

	close(ch)

	resp, err := CollectStream(ch)
	require.NoError(t, err)

	assert.Equal(t, "first second", resp.Content)
	assert.Equal(t, 12*time.Millisecond, resp.FirstTokenLatency)
	assert.Equal(t, 4, resp.InputTokens)
	assert.Equal(t, 2, resp.OutputTokens)
}

func TestCollectStream_EmptyChannel(t *testing.T) {
	t.Parallel()

	ch := make(chan Chunk)
	close(ch)

	resp, err := CollectStream(ch)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrStreamIncomplete)
	assert.Empty(t, resp.Content)
	assert.Empty(t, resp.Model)
	assert.Equal(t, 0, resp.InputTokens)
}

func TestCollectStream_NilChannel(t *testing.T) {
	t.Parallel()

	resp, err := CollectStream(nil)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrStreamIncomplete)
	assert.Empty(t, resp.Content)
	assert.Empty(t, resp.Model)
}

func TestCollectStream_MissingFinalChunkIsIncomplete(t *testing.T) {
	t.Parallel()

	ch := make(chan Chunk, 1)
	ch <- Chunk{Content: "partial"}

	close(ch)

	resp, err := CollectStream(ch)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrStreamIncomplete)
	assert.Equal(t, "partial", resp.Content)
}

func TestCollectStream_MidStreamErrorReturnsPartialAndError(t *testing.T) {
	t.Parallel()

	providerErr := errors.New("provider failed after first token")

	ch := make(chan Chunk, 2)
	ch <- Chunk{Content: "partial "}

	ch <- Chunk{Err: providerErr}

	close(ch)

	resp, err := CollectStream(ch)
	require.Error(t, err)
	require.ErrorIs(t, err, providerErr)
	assert.Equal(t, "partial ", resp.Content)
}

func TestCompleteStreamOrFallback_MidStreamProviderErrorReachesCaller(t *testing.T) {
	t.Parallel()

	providerErr := errors.New("provider stream broke")
	sp := &fakeStreamProvider{
		fakeProvider: fakeProvider{
			name:   "stream-test",
			models: []string{"s-1"},
			resp:   &Response{Content: "unused"},
		},
		chunks: []Chunk{
			{Content: "token1"},
			{Err: providerErr},
		},
	}

	ch, err := CompleteStreamOrFallback(context.Background(), sp, CompleteParams{Model: "s-1"})
	require.NoError(t, err)

	chunks := drainChunks(ch)
	require.Len(t, chunks, 2)
	assert.Equal(t, "token1", chunks[0].Content)
	require.Error(t, chunks[1].Err)
	require.ErrorIs(t, chunks[1].Err, providerErr)
	assert.False(t, chunks[1].Done)
}

func TestRegistry_CompleteStreamWithModelRoleRequiresStreamingCapability(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	openAI := &fakeProvider{
		name:   providerOpenAI,
		models: []string{"gpt-5.4-mini"},
		resp:   &Response{Content: "buffered"},
	}
	codex := &fakeStreamProvider{
		fakeProvider: fakeProvider{
			name:   providerCodex,
			models: []string{"gpt-5.4-mini"},
		},
		chunks: []Chunk{
			{Content: "streamed"},
			{Done: true, Model: "gpt-5.4-mini", StopReason: StopEndTurn},
		},
	}

	r.Register(openAI)
	r.Register(codex)
	require.NoError(t, r.SetModelRole("planner", ModelRole{
		Preferred:      "openai/gpt-5.4-mini",
		FallbackModels: []string{"codex/gpt-5.4-mini"},
	}))

	ch, err := r.CompleteStreamWithFallback(context.Background(), CompleteParams{Model: "planner"}, nil)
	require.NoError(t, err)

	resp, err := CollectStream(ch)
	require.NoError(t, err)
	assert.Equal(t, "streamed", resp.Content)
	assert.Equal(t, "gpt-5.4-mini", resp.Model)
	assert.Empty(t, openAI.calls)
	require.Len(t, codex.streamCalls, 1)
	assert.Equal(t, "gpt-5.4-mini", codex.streamCalls[0].Model)

	resolution, ok, err := r.resolveModelRoleWithCapabilities(
		"planner",
		CompleteParams{},
		nil,
		[]string{modelroute.CapabilityStreaming},
	)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "codex/gpt-5.4-mini", resolution.SelectedModel)
	assertRejectionContains(t, resolution.Decision, "openai/gpt-5.4-mini", modelroute.ReasonMissingCapability)
}

func TestRegistry_CompleteStreamWithModelRoleRequiresActualStreamProvider(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	buffered := &capabilityFakeProvider{
		capabilities: ProviderCapabilities{
			SupportsChatCompletions: true,
			SupportsStreaming:       true,
		},
		fakeProvider: fakeProvider{
			name:   "buffered",
			models: []string{"b-1"},
			resp:   &Response{Content: "buffered"},
		},
	}
	codex := &fakeStreamProvider{
		fakeProvider: fakeProvider{
			name:   providerCodex,
			models: []string{"gpt-5.4-mini"},
		},
		chunks: []Chunk{
			{Content: "streamed", Model: "gpt-5.4-mini"},
			{Done: true, Model: "gpt-5.4-mini", StopReason: StopEndTurn},
		},
	}

	r.Register(buffered)
	r.Register(codex)
	require.NoError(t, r.SetModelRole("planner", ModelRole{
		Preferred:      "buffered/b-1",
		FallbackModels: []string{"codex/gpt-5.4-mini"},
	}))

	ch, err := r.CompleteStreamWithFallback(context.Background(), CompleteParams{Model: "planner"}, nil)
	require.NoError(t, err)

	resp, err := CollectStream(ch)
	require.NoError(t, err)
	assert.Equal(t, "streamed", resp.Content)
	assert.Empty(t, buffered.calls)
	require.Len(t, codex.streamCalls, 1)
	assert.Equal(t, "gpt-5.4-mini", codex.streamCalls[0].Model)

	resolution, ok, err := r.resolveModelRoleWithCapabilities(
		"planner",
		CompleteParams{},
		nil,
		[]string{modelroute.CapabilityStreaming},
	)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "codex/gpt-5.4-mini", resolution.SelectedModel)
	assertRejectionContains(t, resolution.Decision, "buffered/b-1", modelroute.ReasonMissingCapability)
}

func TestRegistry_CompleteStreamWithModelRoleFallsBackOnSetupFailure(t *testing.T) {
	t.Parallel()

	r := NewRegistry()

	ollama := &fakeStreamProvider{
		fakeProvider: fakeProvider{
			name:   providerOllama,
			models: []string{"llama3.2"},
		},
		streamErr: errors.New("stream setup failed"),
	}
	codex := &fakeStreamProvider{
		fakeProvider: fakeProvider{
			name:   providerCodex,
			models: []string{"gpt-5.4-mini"},
		},
		chunks: []Chunk{
			{Content: "fallback stream"},
			{Done: true, Model: "gpt-5.4-mini", StopReason: StopEndTurn},
		},
	}

	r.Register(ollama)
	r.Register(codex)
	require.NoError(t, r.SetModelRole("planner", ModelRole{
		Preferred:      "ollama/llama3.2",
		FallbackModels: []string{"codex/gpt-5.4-mini"},
	}))

	ch, err := r.CompleteStream(context.Background(), CompleteParams{Model: "planner"})
	require.NoError(t, err)

	resp, err := CollectStream(ch)
	require.NoError(t, err)
	assert.Equal(t, "fallback stream", resp.Content)
	assert.Equal(t, "gpt-5.4-mini", resp.Model)
	require.Len(t, ollama.streamCalls, 1)
	require.Len(t, codex.streamCalls, 1)
}

func TestRegistry_CompleteStreamWithFallbackPreservesSuccessfulFallbackMetadata(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	r.SetRetry(retryConfig{})

	openAI := &fakeStreamProvider{
		fakeProvider: fakeProvider{
			name:   providerOpenAI,
			models: []string{"gpt-primary"},
		},
		streamErr: errors.New(`openai: HTTP 429: {"error":{"type":"rate_limit_error","message":"slow down"}}`),
	}
	anthropic := &fakeStreamProvider{
		fakeProvider: fakeProvider{
			name:   providerAnthropic,
			models: []string{"claude-fallback"},
		},
		chunks: []Chunk{
			{Content: "fallback", Model: "claude-fallback"},
			{Done: true, Model: "claude-fallback", StopReason: StopEndTurn},
		},
	}

	r.Register(openAI)
	r.Register(anthropic)

	ch, err := r.CompleteStreamWithFallback(context.Background(), CompleteParams{
		Model: "openai/gpt-primary",
	}, []string{"anthropic/claude-fallback"})
	require.NoError(t, err)

	resp, err := CollectStream(ch)
	require.NoError(t, err)

	assert.Equal(t, "fallback", resp.Content)
	assert.Equal(t, providerAnthropic, resp.Provider)
	assert.Equal(t, "claude-fallback", resp.Model)

	metadata := resp.ProviderFailureMetadata()
	require.NotEmpty(t, metadata)
	assert.Contains(t, metadata["fallback_failure_classifications"], providerOpenAI+"="+string(providerFailureRateLimit))
	assert.Contains(t, metadata["fallback_attempts"], "openai/gpt-primary")
	assert.Contains(t, metadata["fallback_rate_limit_scopes"], "="+modelroute.RateLimitScopeProvider)
	assert.Equal(t, providerOpenAI, metadata["rate_limited_providers"])
}

func TestRegistry_CompleteStreamRecordsRouteTelemetry(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	telemetry := modelroute.NewTelemetry()
	r.SetRouteTelemetry(telemetry)
	r.Register(&fakeStreamProvider{
		fakeProvider: fakeProvider{
			name:   providerCodex,
			models: []string{"gpt-5.4-mini"},
		},
		chunks: []Chunk{
			{Content: "hi", FirstTokenLatency: 12 * time.Millisecond},
			{Done: true, Model: "gpt-5.4-mini", InputTokens: 1000, OutputTokens: 50},
		},
	})

	ch, err := r.CompleteStream(context.Background(), CompleteParams{Model: "codex/gpt-5.4-mini"})
	require.NoError(t, err)

	resp, err := CollectStream(ch)
	require.NoError(t, err)
	assert.Equal(t, "hi", resp.Content)

	require.Eventually(t, func() bool {
		obs, ok := telemetry.Snapshot("codex/gpt-5.4-mini")

		return ok &&
			obs.Count == 1 &&
			obs.InputTokens == 1000 &&
			obs.OutputTokens == 50 &&
			obs.AvgTTFTMS == 12 &&
			obs.LastLatencyMS > 0
	}, time.Second, 10*time.Millisecond)
}

func TestCollectStream_ContextCancellationReturnsPartialAndError(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sp := &cancelingStreamProvider{
		fakeProvider: fakeProvider{
			name:   "canceling-stream",
			models: []string{"s-1"},
			resp:   &Response{Content: "unused"},
		},
		firstSent: make(chan struct{}),
	}

	ch, err := CompleteStreamOrFallback(ctx, sp, CompleteParams{Model: "s-1"})
	require.NoError(t, err)

	select {
	case <-sp.firstSent:
	case <-time.After(time.Second):
		require.Fail(t, "timed out waiting for first stream chunk")
	}

	cancel()

	resp, err := CollectStream(ch)
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, "partial", resp.Content)
}

type fakeStreamProvider struct {
	streamErr   error
	chunks      []Chunk
	streamCalls []CompleteParams
	fakeProvider
}

type capabilityFakeStreamProvider struct {
	capabilities ProviderCapabilities
	fakeStreamProvider
}

func (f *capabilityFakeStreamProvider) Capabilities() ProviderCapabilities {
	return f.capabilities
}

func (f *fakeStreamProvider) CompleteStream(ctx context.Context, p CompleteParams) (<-chan Chunk, error) {
	f.streamCalls = append(f.streamCalls, p)

	if f.streamErr != nil {
		return nil, f.streamErr
	}

	ch := make(chan Chunk, DefaultStreamBuffer)

	go func() {
		defer close(ch)

		for i := range f.chunks {
			select {
			case ch <- f.chunks[i]:
			case <-ctx.Done():
				return
			}
		}
	}()

	return ch, nil
}

type cancelingStreamProvider struct {
	firstSent chan struct{}
	fakeProvider
}

func (f *cancelingStreamProvider) CompleteStream(ctx context.Context, _ CompleteParams) (<-chan Chunk, error) {
	ch := make(chan Chunk, DefaultStreamBuffer)

	go func() {
		defer close(ch)

		ch <- Chunk{Content: "partial"}

		close(f.firstSent)

		<-ctx.Done()

		ch <- Chunk{Err: ctx.Err()}
	}()

	return ch, nil
}

func drainChunks(ch <-chan Chunk) []Chunk {
	var chunks []Chunk
	for c := range ch {
		chunks = append(chunks, c)
	}

	return chunks
}

func chunksToStream(chunks []Chunk) <-chan Chunk {
	ch := make(chan Chunk, len(chunks))
	for i := range chunks {
		ch <- chunks[i]
	}

	close(ch)

	return ch
}
