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

func TestCompleteWithRetry_SucceedsFirstAttempt(t *testing.T) {
	t.Parallel()

	resp, err := completeWithRetry(context.Background(), defaultRetryConfig(), func(_ context.Context) (*Response, error) {
		return &Response{Content: "ok"}, nil
	})
	require.NoError(t, err)
	assert.Equal(t, "ok", resp.Content)
}

func TestCompleteWithRetry_RetriesOnTransientError(t *testing.T) {
	t.Parallel()

	attempts := 0
	resp, err := completeWithRetry(context.Background(), retryConfig{
		MaxAttempts:    2,
		InitialBackoff: 1 * time.Millisecond,
	}, func(_ context.Context) (*Response, error) {
		attempts++
		if attempts < 2 {
			return nil, errors.New("openai: HTTP 429: rate limited")
		}

		return &Response{Content: "ok"}, nil
	})
	require.NoError(t, err)
	assert.Equal(t, "ok", resp.Content)
	assert.Equal(t, 2, attempts)
}

func TestCompleteWithRetry_DoesNotRetryNonTransient(t *testing.T) {
	t.Parallel()

	attempts := 0
	_, err := completeWithRetry(context.Background(), retryConfig{
		MaxAttempts:    2,
		InitialBackoff: 1 * time.Millisecond,
	}, func(_ context.Context) (*Response, error) {
		attempts++

		return nil, errors.New("openai: HTTP 400: bad request")
	})
	require.Error(t, err)
	assert.Equal(t, 1, attempts)
}

func TestCompleteWithRetry_ExhaustsRetries(t *testing.T) {
	t.Parallel()

	attempts := 0
	_, err := completeWithRetry(context.Background(), retryConfig{
		MaxAttempts:    2,
		InitialBackoff: 1 * time.Millisecond,
	}, func(_ context.Context) (*Response, error) {
		attempts++

		return nil, errors.New("openai: HTTP 503: service unavailable")
	})
	require.Error(t, err)
	assert.Equal(t, 3, attempts) // 1 initial + 2 retries
}

func TestCompleteWithRetry_DisabledWithZeroAttempts(t *testing.T) {
	t.Parallel()

	attempts := 0
	_, err := completeWithRetry(context.Background(), retryConfig{}, func(_ context.Context) (*Response, error) {
		attempts++

		return nil, errors.New("openai: HTTP 503: service unavailable")
	})
	require.Error(t, err)
	assert.Equal(t, 1, attempts)
}

func TestCompleteWithRetry_RespectsContextCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	attempts := 0
	_, err := completeWithRetry(ctx, retryConfig{
		MaxAttempts:    3,
		InitialBackoff: 1 * time.Millisecond,
	}, func(_ context.Context) (*Response, error) {
		attempts++

		return nil, errors.New("openai: HTTP 429: rate limited")
	})
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
}

func TestIsRetryableStatus(t *testing.T) {
	t.Parallel()

	assert.True(t, isRetryableStatus(429))
	assert.True(t, isRetryableStatus(500))
	assert.True(t, isRetryableStatus(502))
	assert.True(t, isRetryableStatus(503))
	assert.True(t, isRetryableStatus(504))
	assert.False(t, isRetryableStatus(400))
	assert.False(t, isRetryableStatus(401))
	assert.False(t, isRetryableStatus(404))
	assert.False(t, isRetryableStatus(200))
}

func TestParseRetryAfter(t *testing.T) {
	t.Parallel()

	assert.Equal(t, 5*time.Second, parseRetryAfter("5"))
	assert.Equal(t, time.Duration(0), parseRetryAfter(""))
	assert.Equal(t, time.Duration(0), parseRetryAfter("invalid"))
	assert.Equal(t, time.Duration(0), parseRetryAfter("0"))
	assert.Equal(t, time.Duration(0), parseRetryAfter("-1"))
}

func TestRetryableHTTPStatusError_ParsesRetryAfter(t *testing.T) {
	t.Parallel()

	err := retryableHTTPStatusError(errors.New("openai: HTTP 429: rate limited"), 429, "2")

	retryAfter, retryable := isRetryable(err)
	assert.True(t, retryable)
	assert.Equal(t, 2*time.Second, retryAfter)
	assert.Equal(t, "openai: HTTP 429: rate limited", err.Error())
}

func TestRetryableHTTPStatusError_IgnoresNonTransientStatus(t *testing.T) {
	t.Parallel()

	err := retryableHTTPStatusError(errors.New("openai: HTTP 400: bad request"), 400, "2")

	retryAfter, retryable := isRetryable(err)
	assert.False(t, retryable)
	assert.Zero(t, retryAfter)
	assert.Equal(t, "openai: HTTP 400: bad request", err.Error())
}

func TestRegistry_CompleteRetriesTransientError(t *testing.T) {
	t.Parallel()

	attempts := 0
	r := NewRegistry()
	telemetry := modelroute.NewTelemetry()
	r.SetRouteTelemetry(telemetry)
	r.SetRetry(retryConfig{MaxAttempts: 2, InitialBackoff: 1 * time.Millisecond})
	r.Register(&retryFakeProvider{
		fakeProvider: fakeProvider{
			name:   "transient",
			models: []string{"m-1"},
			resp:   &Response{Content: "ok"},
		},
		failCount: 1,
		failErr:   retryableHTTPStatusError(errors.New("HTTP 429: rate limited"), 429, "1"),
		attempts:  &attempts,
	})

	resp, err := r.Complete(context.Background(), CompleteParams{Model: "m-1"})
	require.NoError(t, err)
	assert.Equal(t, "ok", resp.Content)
	assert.Equal(t, 2, attempts)

	obs, ok := telemetry.Snapshot("transient/m-1")
	require.True(t, ok)
	assert.Equal(t, 1, obs.Count)
	assert.Equal(t, 1, obs.FailureCount)
	assert.Equal(t, 1, obs.RateLimitCount)
	assert.Empty(t, obs.LastError)
	assert.False(t, obs.LastFailureRateLimited)
}

type retryFakeProvider struct {
	failErr  error
	attempts *int
	fakeProvider
	failCount int
}

func (f *retryFakeProvider) Complete(_ context.Context, p CompleteParams) (*Response, error) {
	*f.attempts++
	if *f.attempts <= f.failCount {
		return nil, f.failErr
	}

	r := *f.resp
	r.Model = p.Model

	return &r, nil
}
