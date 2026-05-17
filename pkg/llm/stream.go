package llm

import (
	"context"
	"fmt"
	"strings"
)

// Chunk is a single piece of a streaming response.
type Chunk struct {
	// Content is the token or token group text.
	Content string

	// Model is set once the provider reports the resolved model (may be empty
	// until the stream finishes or a metadata event arrives).
	Model string

	// Done signals the final chunk; fields below are populated only when
	// Done is true.
	Done bool

	// Usage is populated on the final chunk when the provider reports token
	// counts.
	InputTokens       int
	CachedInputTokens int
	OutputTokens      int
}

// StreamProvider is an optional interface that providers can implement to
// support streaming completions. Callers should type-assert a Provider to
// StreamProvider before using CompleteStream; non-streaming providers can be
// wrapped with StreamFromComplete as a fallback.
type StreamProvider interface {
	Provider

	// CompleteStream starts a streaming completion and returns a channel that
	// delivers Chunk values. The channel is closed when the stream finishes
	// or an error occurs. The final Chunk has Done == true and carries usage
	// data if available. If the returned error is non-nil, the channel is nil.
	CompleteStream(ctx context.Context, params CompleteParams) (<-chan Chunk, error)
}

// StreamFromComplete wraps a non-streaming Complete call as a single-chunk
// stream. This is useful as a fallback for providers that do not implement
// StreamProvider.
func StreamFromComplete(ctx context.Context, p Provider, params CompleteParams) (<-chan Chunk, error) {
	resp, err := p.Complete(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("stream fallback: %w", err)
	}

	ch := make(chan Chunk, 1)
	ch <- Chunk{
		Content:           resp.Content,
		Model:             resp.Model,
		Done:              true,
		InputTokens:       resp.InputTokens,
		CachedInputTokens: resp.CachedInputTokens,
		OutputTokens:      resp.OutputTokens,
	}

	close(ch)

	return ch, nil
}

// CompleteStreamOrFallback attempts to use the streaming interface if the
// provider supports it, otherwise falls back to StreamFromComplete.
func CompleteStreamOrFallback(ctx context.Context, p Provider, params CompleteParams) (<-chan Chunk, error) {
	if sp, ok := p.(StreamProvider); ok {
		ch, err := sp.CompleteStream(ctx, params)
		if err != nil {
			return nil, fmt.Errorf("stream: %w", err)
		}

		return ch, nil
	}

	return StreamFromComplete(ctx, p, params)
}

// CollectStream drains a chunk channel and assembles the result into a single
// Response.  This is useful when a caller needs the full response but the
// provider only offers streaming.  The channel must be closed by the producer;
// CollectStream blocks until it is.
func CollectStream(ch <-chan Chunk) *Response {
	if ch == nil {
		return &Response{}
	}

	var b strings.Builder

	resp := &Response{}

	for c := range ch {
		b.WriteString(c.Content)

		if c.Model != "" {
			resp.Model = c.Model
		}

		if c.Done {
			resp.InputTokens = c.InputTokens
			resp.CachedInputTokens = c.CachedInputTokens
			resp.OutputTokens = c.OutputTokens
		}
	}

	resp.Content = b.String()

	return resp
}
