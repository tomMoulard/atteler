package llm

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStreamFromComplete_SingleChunk(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{
		name:   "test",
		models: []string{"m-1"},
		resp: &Response{
			Content:      "hello",
			Model:        "m-1",
			InputTokens:  10,
			OutputTokens: 5,
		},
	}

	ch, err := StreamFromComplete(context.Background(), p, CompleteParams{Model: "m-1"})
	require.NoError(t, err)

	chunks := drainChunks(ch)
	require.Len(t, chunks, 1)
	assert.Equal(t, "hello", chunks[0].Content)
	assert.Equal(t, "m-1", chunks[0].Model)
	assert.True(t, chunks[0].Done)
	assert.Equal(t, 10, chunks[0].InputTokens)
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
	}

	ch, err := CompleteStreamOrFallback(context.Background(), sp, CompleteParams{Model: "s-1"})
	require.NoError(t, err)

	chunks := drainChunks(ch)
	require.NotEmpty(t, chunks)
	assert.Equal(t, "token1", chunks[0].Content)
	assert.True(t, chunks[len(chunks)-1].Done)
}

func TestCompleteStreamOrFallback_FallsBackToNonStream(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{
		name:   "non-stream",
		models: []string{"n-1"},
		resp:   &Response{Content: "full response", Model: "n-1"},
	}

	ch, err := CompleteStreamOrFallback(context.Background(), p, CompleteParams{Model: "n-1"})
	require.NoError(t, err)

	chunks := drainChunks(ch)
	require.Len(t, chunks, 1)
	assert.Equal(t, "full response", chunks[0].Content)
	assert.True(t, chunks[0].Done)
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

	ch <- Chunk{Content: "!", Done: true, InputTokens: 10, OutputTokens: 3, Model: "m-1"}

	close(ch)

	resp := CollectStream(ch)
	assert.Equal(t, "Hello world!", resp.Content)
	assert.Equal(t, "m-1", resp.Model)
	assert.Equal(t, 10, resp.InputTokens)
	assert.Equal(t, 3, resp.OutputTokens)
}

func TestCollectStream_EmptyChannel(t *testing.T) {
	t.Parallel()

	ch := make(chan Chunk)
	close(ch)

	resp := CollectStream(ch)
	assert.Empty(t, resp.Content)
	assert.Empty(t, resp.Model)
	assert.Equal(t, 0, resp.InputTokens)
}

func TestCollectStream_NilChannel(t *testing.T) {
	t.Parallel()

	resp := CollectStream(nil)
	assert.Empty(t, resp.Content)
	assert.Empty(t, resp.Model)
}

type fakeStreamProvider struct {
	fakeProvider
}

func (f *fakeStreamProvider) CompleteStream(_ context.Context, _ CompleteParams) (<-chan Chunk, error) {
	ch := make(chan Chunk, 2)
	ch <- Chunk{Content: "token1"}

	ch <- Chunk{Content: "token2", Done: true, InputTokens: 5, OutputTokens: 2}

	close(ch)

	return ch, nil
}

func drainChunks(ch <-chan Chunk) []Chunk {
	var chunks []Chunk
	for c := range ch {
		chunks = append(chunks, c)
	}

	return chunks
}
