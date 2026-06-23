package llm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tommoulard/atteler/pkg/modelroute"
)

const (
	// DefaultStreamBuffer is the maximum channel capacity provider adapters
	// should use unless they have a measured reason to do otherwise. A capacity
	// of two leaves room for one in-flight content chunk plus a terminal result
	// while still making slow consumers apply backpressure instead of letting
	// providers build unbounded in-memory token queues.
	DefaultStreamBuffer = 2

	sseDonePayload = "[DONE]"
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
//
//nolint:govet // Field order keeps the public stream contract grouped by meaning; Chunk values are transient.
type Chunk struct {
	// Content is the token or token group text.
	Content string

	// Provider is set when the producer knows which provider emitted the chunk.
	Provider string

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

	// ProviderFailureMetadata carries fallback/provider failure metadata on the
	// final successful chunk when a stream succeeds after earlier fallback
	// attempts failed.
	ProviderFailureMetadata map[string]string

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

// CompleteStream resolves params.Model through the registry and returns a
// provider-agnostic completion stream. Model roles are resolved with an
// implicit "streaming" capability requirement so a role does not silently pick
// a backend that can only produce a buffered non-streaming response.
func (r *Registry) CompleteStream(ctx context.Context, params CompleteParams) (<-chan Chunk, error) {
	if err := requireCredentialContext(ctx); err != nil {
		return nil, err
	}

	if routedParams, routedFallbacks, routed, err := r.routeModelRoleRequestWithCapabilities(
		params,
		nil,
		[]string{modelroute.CapabilityStreaming},
	); err != nil {
		return nil, err
	} else if routed {
		params = routedParams
		if len(routedFallbacks) > 0 {
			return r.completeStreamResolvedWithFallback(ctx, params, routedFallbacks)
		}
	}

	return r.completeStreamResolved(ctx, params)
}

func (r *Registry) completeStreamResolved(ctx context.Context, params CompleteParams) (<-chan Chunk, error) {
	p, params, err := r.resolve(params)
	if err != nil {
		return nil, err
	}

	capabilities := ProviderCapabilitiesFor(p)

	sp, ok := p.(StreamProvider)
	if !ok || !capabilities.SupportsStreaming {
		resp, completeErr := r.completeResolved(ctx, params, r.fallbackRetryConfig(false))
		if completeErr != nil {
			return nil, completeErr
		}

		return streamFromResponse(resp), nil
	}

	params, adjustments, err := prepareRoutedCompleteParamsForProviderCapabilities(p.Name(), capabilities, params)
	if err != nil {
		return nil, err
	}

	if validateErr := validateCompleteParamsAgainstDeclaredCapabilities(p, params); validateErr != nil {
		return nil, validateErr
	}

	emitToolExecute(ctx, p, params, adjustments)

	startedAt := time.Now()

	ch, err := sp.CompleteStream(ctx, params)
	if err != nil {
		wrappedErr := fmt.Errorf("llm: %s stream: %w", p.Name(), err)
		r.recordRouteFailure(p.Name(), params.Model, wrappedErr)

		return nil, wrappedErr
	}

	return r.observeCompletionStream(ctx, p.Name(), params.Model, ch, startedAt), nil
}

func (r *Registry) completeStreamResolvedWithFallback(
	ctx context.Context,
	params CompleteParams,
	fallbackModels []string,
) (<-chan Chunk, error) {
	models := modelFallbackChain(params.Model, fallbackModels)
	if len(models) == 0 {
		return r.completeStreamResolved(ctx, params)
	}

	var failures []fallbackAttemptFailure

	for _, model := range models {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("llm: stream fallback canceled: %w", err)
		}

		target := r.fallbackAttemptTarget(model)

		next := params
		next.Model = model

		ch, err := r.completeStreamResolved(ctx, next)
		if err == nil {
			return r.streamWithSuccessfulFallbackMetadata(ctx, ch, failures), nil
		}

		failures = append(failures, newFallbackAttemptFailure(model, target, err))
	}

	return nil, newFallbackError(failures, r.streamReadinessReport())
}

// CompleteStreamWithFallback tries params.Model followed by fallbackModels
// until one stream starts successfully. Once a provider returns a stream,
// mid-stream failures are delivered to the caller as terminal error chunks
// rather than replaying partial output against another model.
func (r *Registry) CompleteStreamWithFallback(
	ctx context.Context,
	params CompleteParams,
	fallbackModels []string,
) (<-chan Chunk, error) {
	if err := requireCredentialContext(ctx); err != nil {
		return nil, err
	}

	if routedParams, routedFallbacks, routed, err := r.routeModelRoleRequestWithCapabilities(
		params,
		fallbackModels,
		[]string{modelroute.CapabilityStreaming},
	); err != nil {
		return nil, err
	} else if routed {
		params = routedParams
		fallbackModels = routedFallbacks
	}

	models := modelFallbackChain(params.Model, fallbackModels)
	if len(models) == 0 {
		return r.CompleteStream(ctx, params)
	}

	var failures []fallbackAttemptFailure

	rateLimitedProviders := make(map[string]fallbackAttemptFailure)

	for _, model := range models {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("llm: stream fallback canceled: %w", err)
		}

		target := r.fallbackAttemptTarget(model)
		if skipped, ok := r.skippedFallbackFailure(model, target, rateLimitedProviders); ok {
			failures = append(failures, skipped)
			r.recordFallbackFailure(model, target, skipped)

			continue
		}

		next := params
		next.Model = model

		ch, err := r.CompleteStream(ctx, next)
		if err == nil {
			return r.streamWithSuccessfulFallbackMetadata(ctx, ch, failures), nil
		}

		failure := newFallbackAttemptFailure(model, target, err)
		failures = append(failures, failure)

		if failure.classification.RateLimited && target.providerName != "" {
			rateLimitedProviders[target.providerName] = failure
		}
	}

	return nil, newFallbackError(failures, r.streamReadinessReport())
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

	return streamFromResponse(resp), nil
}

func streamFromResponse(resp *Response) <-chan Chunk {
	if resp == nil {
		resp = &Response{}
	}

	ch := make(chan Chunk, DefaultStreamBuffer)
	ch <- Chunk{
		Content:                 resp.Content,
		Provider:                resp.Provider,
		Model:                   resp.Model,
		Done:                    true,
		StopReason:              resp.StopReason,
		ToolCalls:               append([]ToolCall(nil), resp.ToolCalls...),
		FirstTokenLatency:       resp.FirstTokenLatency,
		InputTokens:             resp.InputTokens,
		CachedInputTokens:       resp.CachedInputTokens,
		CacheWriteInputTokens:   resp.CacheWriteInputTokens,
		OutputTokens:            resp.OutputTokens,
		ProviderFailureMetadata: resp.ProviderFailureMetadata(),
	}

	close(ch)

	return ch
}

func (r *Registry) observeCompletionStream(
	ctx context.Context,
	providerName string,
	requestedModel string,
	ch <-chan Chunk,
	startedAt time.Time,
) <-chan Chunk {
	out := make(chan Chunk, DefaultStreamBuffer)

	go func() {
		defer close(out)

		var firstTokenLatency time.Duration

		for chunk := range ch {
			if firstTokenLatency <= 0 && chunk.FirstTokenLatency > 0 {
				firstTokenLatency = chunk.FirstTokenLatency
			}

			chunk = observedStreamChunk(chunk, providerName, requestedModel, firstTokenLatency)
			if !forwardObservedStreamChunk(ctx, out, chunk) {
				r.recordRouteFailure(providerName, requestedModel, ctx.Err())

				return
			}

			switch {
			case chunk.Err != nil:
				r.recordRouteFailure(providerName, requestedModel, chunk.Err)

				return
			case chunk.Done:
				r.recordRouteObservation(providerName, requestedModel, responseFromStreamChunk(chunk), time.Since(startedAt))

				return
			}
		}

		r.recordRouteFailure(providerName, requestedModel, ErrStreamIncomplete)
	}()

	return out
}

func observedStreamChunk(chunk Chunk, providerName, requestedModel string, firstTokenLatency time.Duration) Chunk {
	if chunk.Provider == "" {
		chunk.Provider = providerName
	}

	if chunk.Model == "" {
		chunk.Model = requestedModel
	}

	if chunk.Done && chunk.FirstTokenLatency <= 0 {
		chunk.FirstTokenLatency = firstTokenLatency
	}

	return chunk
}

func forwardObservedStreamChunk(ctx context.Context, out chan<- Chunk, chunk Chunk) bool {
	select {
	case out <- chunk:
		return true
	case <-ctx.Done():
		return false
	}
}

func responseFromStreamChunk(chunk Chunk) *Response {
	return &Response{
		Provider:              chunk.Provider,
		Model:                 chunk.Model,
		StopReason:            chunk.StopReason,
		ToolCalls:             append([]ToolCall(nil), chunk.ToolCalls...),
		FirstTokenLatency:     chunk.FirstTokenLatency,
		InputTokens:           chunk.InputTokens,
		CachedInputTokens:     chunk.CachedInputTokens,
		CacheWriteInputTokens: chunk.CacheWriteInputTokens,
		OutputTokens:          chunk.OutputTokens,
		metadata:              cloneStringMap(chunk.ProviderFailureMetadata),
	}
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
	return collectStream(ch, nil)
}

func collectStream(ch <-chan Chunk, onChunk func(Chunk)) (*Response, error) {
	if ch == nil {
		return &Response{}, ErrStreamIncomplete
	}

	var b strings.Builder

	resp := &Response{}

	for c := range ch {
		if onChunk != nil {
			onChunk(c)
		}

		b.WriteString(c.Content)

		if c.Provider != "" {
			resp.Provider = c.Provider
		}

		if c.Model != "" {
			resp.Model = c.Model
		}

		if len(c.ProviderFailureMetadata) > 0 {
			resp.metadata = mergeResponseMetadata(resp.metadata, c.ProviderFailureMetadata)
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

func (r *Registry) streamWithSuccessfulFallbackMetadata(
	ctx context.Context,
	ch <-chan Chunk,
	failures []fallbackAttemptFailure,
) <-chan Chunk {
	if len(failures) == 0 {
		return ch
	}

	metadata := fallbackMetadataForAttempts(failures, r.streamReadinessReport())
	if len(metadata) == 0 {
		return ch
	}

	out := make(chan Chunk, DefaultStreamBuffer)

	go func() {
		defer close(out)

		for chunk := range ch {
			if chunk.Done {
				chunk.ProviderFailureMetadata = mergeResponseMetadata(chunk.ProviderFailureMetadata, metadata)
			}

			if !sendStreamChunk(ctx, out, chunk) {
				select {
				case out <- Chunk{Err: ctx.Err()}:
				default:
				}

				return
			}
		}
	}()

	return out
}

func (r *Registry) streamReadinessReport() ProviderReadinessReport {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.readinessReportLocked()
}

func sendStreamChunk(ctx context.Context, ch chan<- Chunk, chunk Chunk) bool {
	select {
	case ch <- chunk:
		return true
	case <-ctx.Done():
		return false
	}
}

func sendStreamTerminalError(ctx context.Context, ch chan<- Chunk, err error) {
	if err == nil {
		return
	}

	chunk := Chunk{Err: err}

	select {
	case ch <- chunk:
		return
	default:
	}

	select {
	case ch <- chunk:
	case <-ctx.Done():
	}
}
