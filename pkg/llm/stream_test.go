package llm

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	chunks []Chunk
	fakeProvider
}

func (f *fakeStreamProvider) CompleteStream(ctx context.Context, _ CompleteParams) (<-chan Chunk, error) {
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
