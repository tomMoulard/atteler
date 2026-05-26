package llm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	// DefaultStreamBuffer is the maximum channel capacity provider adapters
	// should use unless they have a measured reason to do otherwise. A capacity
	// of two leaves room for one in-flight content chunk plus a terminal result
	// while still making slow consumers apply backpressure instead of letting
	// providers build unbounded in-memory token queues.
	DefaultStreamBuffer = 2
)

// ErrStreamIncomplete is returned when a stream closes before sending a final
// successful chunk.
var ErrStreamIncomplete = errors.New("llm: stream ended without final successful chunk")

// Chunk is a single event in a streaming response.
//
// A stream must end with exactly one terminal chunk:
//   - success: Done is true and Err is nil; StopReason, ToolCalls, and usage may be set.
//   - failure: Err is non-nil.
//
// Closing the channel without a terminal success chunk is an incomplete
// response, not success. Error chunks are terminal; providers should close the
// channel after sending one and must not send more content after Err.
type Chunk struct {
	// Content is the token or token group text.
	Content string

	// Model is set once the provider reports the resolved model (may be empty
	// until the stream finishes or a metadata event arrives).
	Model string

	// Err reports a terminal streaming failure, including provider errors
	// after partial output or context cancellation. When Err is non-nil, callers
	// should treat any collected content as partial.
	Err error

	// StopReason indicates why the model stopped on a successful final chunk.
	StopReason StopReason

	// ToolCalls is populated on the final chunk when the provider reports
	// tool-use requests.
	ToolCalls []ToolCall

	// FirstTokenLatency may be populated as soon as the first content token is
	// observed. Usage is populated on the final chunk when the provider reports
	// token counts.
	FirstTokenLatency     time.Duration
	InputTokens           int
	CachedInputTokens     int
	CacheWriteInputTokens int
	OutputTokens          int

	// Done signals a final successful chunk. When Done is true, StopReason,
	// ToolCalls, latency, and usage fields may be populated.
	Done bool
}

// StreamProvider is an optional interface that providers can implement to
// support streaming completions. Callers should type-assert a Provider to
// StreamProvider before using CompleteStream; non-streaming providers can be
// wrapped with StreamFromComplete as a fallback.
type StreamProvider interface {
	Provider

	// CompleteStream starts a streaming completion and returns a bounded channel
	// that delivers Chunk values. Providers should prefer DefaultStreamBuffer
	// (or an unbuffered channel) and select on ctx.Done() while sending so slow
	// consumers apply backpressure instead of leaking goroutines or memory.
	//
	// Setup failures are returned directly with a nil channel. Once a channel is
	// returned, mid-stream failures must be delivered as a terminal Chunk with
	// Err set. Successful streams must send a final Chunk with Done == true;
	// closing without that final success chunk is treated as incomplete.
	CompleteStream(ctx context.Context, params CompleteParams) (<-chan Chunk, error)
}

// StreamFromComplete wraps a non-streaming Complete call as a single-chunk
// stream. This is useful as a fallback for providers that do not implement
// StreamProvider.
func StreamFromComplete(ctx context.Context, p Provider, params CompleteParams) (<-chan Chunk, error) {
	if err := requireCredentialContext(ctx); err != nil {
		return nil, err
	}

	resp, err := p.Complete(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("stream fallback: %w", err)
	}

	ch := make(chan Chunk, DefaultStreamBuffer)
	ch <- Chunk{
		Content:               resp.Content,
		Model:                 resp.Model,
		Done:                  true,
		StopReason:            resp.StopReason,
		ToolCalls:             append([]ToolCall(nil), resp.ToolCalls...),
		FirstTokenLatency:     resp.FirstTokenLatency,
		InputTokens:           resp.InputTokens,
		CachedInputTokens:     resp.CachedInputTokens,
		CacheWriteInputTokens: resp.CacheWriteInputTokens,
		OutputTokens:          resp.OutputTokens,
	}

	close(ch)

	return ch, nil
}

// CompleteStreamOrFallback attempts to use the streaming interface if the
// provider supports it, otherwise falls back to StreamFromComplete.
func CompleteStreamOrFallback(ctx context.Context, p Provider, params CompleteParams) (<-chan Chunk, error) {
	if err := requireCredentialContext(ctx); err != nil {
		return nil, err
	}

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
// Response. This is useful when a caller needs the full response but the
// provider only offers streaming.
//
// It returns partial content with a non-nil error when the stream emits Err or
// closes before a final successful Done chunk. A final Done chunk is terminal,
// so CollectStream returns as soon as it sees one instead of waiting for the
// channel to close.
func CollectStream(ch <-chan Chunk) (*Response, error) {
	if ch == nil {
		return &Response{}, ErrStreamIncomplete
	}

	var b strings.Builder

	resp := &Response{}

	for c := range ch {
		b.WriteString(c.Content)

		if c.Model != "" {
			resp.Model = c.Model
		}

		if c.Err != nil {
			resp.Content = b.String()

			return resp, c.Err
		}

		if resp.FirstTokenLatency <= 0 && c.FirstTokenLatency > 0 {
			resp.FirstTokenLatency = c.FirstTokenLatency
		}

		if c.Done {
			resp.StopReason = c.StopReason
			resp.ToolCalls = append([]ToolCall(nil), c.ToolCalls...)
			resp.InputTokens = c.InputTokens
			resp.CachedInputTokens = c.CachedInputTokens
			resp.CacheWriteInputTokens = c.CacheWriteInputTokens
			resp.OutputTokens = c.OutputTokens

			resp.Content = b.String()

			return resp, nil
		}
	}

	resp.Content = b.String()

	return resp, ErrStreamIncomplete
}
