package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	appconfig "github.com/tommoulard/atteler/pkg/config"
	"github.com/tommoulard/atteler/pkg/events"
	"github.com/tommoulard/atteler/pkg/modelroute"
)

func TestCompleteWithRetry_SucceedsFirstAttempt(t *testing.T) {
	t.Parallel()

	resp, err := completeWithRetry(
		context.Background(),
		defaultRetryConfig(),
		retryMetadata{provider: providerOpenAI, model: "gpt-test"},
		func(_ context.Context) (*Response, error) {
			return &Response{Content: "ok"}, nil
		},
	)
	require.NoError(t, err)
	assert.Equal(t, "ok", resp.Content)
}

func TestCompleteWithRetry_RetriesTypedRetryableStatuses(t *testing.T) {
	t.Parallel()

	for _, status := range []int{429, 500, 502, 503, 504} {
		t.Run(httpStatusName(status), func(t *testing.T) {
			t.Parallel()

			var delays []time.Duration

			attempts := 0
			resp, err := completeWithRetry(
				context.Background(),
				retryConfig{
					MaxAttempts:    2,
					InitialBackoff: time.Millisecond,
					sleep:          captureSleep(&delays),
				},
				retryMetadata{provider: providerOpenAI, model: "gpt-test"},
				func(_ context.Context) (*Response, error) {
					attempts++
					if attempts < 2 {
						return nil, testProviderError(status, 0)
					}

					return &Response{Content: "ok"}, nil
				},
			)
			require.NoError(t, err)
			assert.Equal(t, "ok", resp.Content)
			assert.Equal(t, 2, attempts)
			require.Len(t, delays, 1)
			assert.GreaterOrEqual(t, delays[0], time.Millisecond)
		})
	}
}

func TestCompleteWithRetry_DoesNotRetryTypedNonRetryableStatuses(t *testing.T) {
	t.Parallel()

	for _, status := range []int{400, 401} {
		t.Run(httpStatusName(status), func(t *testing.T) {
			t.Parallel()

			var out bytes.Buffer

			ctx := events.WithEmitter(
				context.Background(),
				events.NewRunnerWithLogger(nil, &out),
				events.Event{},
			)

			attempts := 0
			_, err := completeWithRetry(
				ctx,
				retryConfig{
					MaxAttempts:    2,
					InitialBackoff: time.Millisecond,
					sleep:          captureSleep(nil),
				},
				retryMetadata{provider: providerOpenAI, model: "gpt-test"},
				func(_ context.Context) (*Response, error) {
					attempts++

					return nil, testProviderError(status, 0)
				},
			)
			require.Error(t, err)
			assert.Equal(t, 1, attempts)

			var retryErr *RetryError
			require.ErrorAs(t, err, &retryErr)
			assert.Equal(t, "non_retryable", retryErr.Outcome)
			assert.Equal(t, 1, retryErr.Attempts)
			assert.Equal(t, 0, retryErr.Retries)
			assert.Equal(t, status, retryErr.StatusCode)
			assert.Contains(t, err.Error(), "failed after 1 attempt")

			var providerErr *ProviderError
			require.ErrorAs(t, err, &providerErr)
			assert.Equal(t, status, providerErr.StatusCode)

			logs := out.String()
			assert.Contains(t, logs, "event:provider_retry")
			assert.Contains(t, logs, "outcome=non_retryable")
			assert.Contains(t, logs, "attempt=1")
			assert.Contains(t, logs, "retryable=false")
			assert.Contains(t, logs, "status="+strconv.Itoa(status))
		})
	}
}

func TestCompleteWithRetry_RespectsRetryAfterAndAppliesPositiveJitter(t *testing.T) {
	t.Parallel()

	var delays []time.Duration

	attempts := 0
	resp, err := completeWithRetry(
		context.Background(),
		retryConfig{
			MaxAttempts:    1,
			InitialBackoff: time.Millisecond,
			JitterFraction: 0.2,
			randFloat64:    func() float64 { return 1 },
			sleep:          captureSleep(&delays),
		},
		retryMetadata{provider: providerAnthropic, model: "claude-test"},
		func(_ context.Context) (*Response, error) {
			attempts++
			if attempts == 1 {
				return nil, testProviderError(429, 10*time.Millisecond)
			}

			return &Response{Content: "ok"}, nil
		},
	)
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.Len(t, delays, 1)
	assert.Equal(t, 12*time.Millisecond, delays[0])
}

func TestCompleteWithRetry_BoundsJitter(t *testing.T) {
	t.Parallel()

	cfg := retryConfig{
		InitialBackoff: 100 * time.Millisecond,
		JitterFraction: 0.5,
		randFloat64:    func() float64 { return 2 },
	}

	assert.Equal(t, 150*time.Millisecond, retryDelay(cfg.normalized(), 0, 0))
}

func TestRetryPolicy_NormalizesUnsafeBounds(t *testing.T) {
	t.Parallel()

	cfg := retryConfig{
		MaxAttempts:    -1,
		InitialBackoff: -time.Millisecond,
		MaxBackoff:     -time.Millisecond,
		MaxElapsedTime: -time.Millisecond,
		JitterFraction: 2,
		randFloat64:    func() float64 { return 1 },
	}.normalized()

	assert.Equal(t, 0, cfg.MaxAttempts)
	assert.Equal(t, time.Duration(0), cfg.InitialBackoff)
	assert.Equal(t, time.Duration(0), cfg.MaxBackoff)
	assert.Equal(t, time.Duration(0), cfg.MaxElapsedTime)
	assert.InEpsilon(t, 1.0, cfg.JitterFraction, 0.0001)
	assert.Equal(t, 200*time.Millisecond, jitterDelay(cfg, 100*time.Millisecond))
}

func TestRetryPolicyConfig_ApplyInheritsBasePolicy(t *testing.T) {
	t.Parallel()

	maxAttempts := 4
	jitterFraction := 0.3
	base := retryConfig{
		MaxAttempts:    2,
		InitialBackoff: time.Second,
		MaxBackoff:     10 * time.Second,
		MaxElapsedTime: 30 * time.Second,
		JitterFraction: 0.2,
	}

	got := RetryPolicyConfig{
		MaxAttempts:    &maxAttempts,
		JitterFraction: &jitterFraction,
	}.apply(base).info()

	assert.Equal(t, maxAttempts, got.MaxAttempts)
	assert.Equal(t, time.Second, got.InitialBackoff)
	assert.Equal(t, 10*time.Second, got.MaxBackoff)
	assert.Equal(t, 30*time.Second, got.MaxElapsedTime)
	assert.InEpsilon(t, jitterFraction, got.JitterFraction, 0.0001)
}

func TestRegistry_ApplyProviderRetryConfigPreservesBaseRuntimeHooks(t *testing.T) {
	t.Parallel()

	var delays []time.Duration

	maxAttempts := 2
	r := NewRegistry()
	r.SetRetry(retryConfig{
		MaxAttempts:    1,
		InitialBackoff: time.Millisecond,
		sleep:          captureSleep(&delays),
	})
	r.applyProviderRetryConfig("transient", RetryPolicyConfig{MaxAttempts: &maxAttempts})

	attempts := 0
	r.Register(&retryFakeProvider{
		fakeProvider: fakeProvider{
			name:   "transient",
			models: []string{"m-1"},
			resp:   &Response{Content: "ok"},
		},
		failCount: 1,
		failErr:   testProviderError(503, 0),
		attempts:  &attempts,
	})

	resp, err := r.Complete(context.Background(), CompleteParams{Model: "m-1"})
	require.NoError(t, err)
	assert.Equal(t, "ok", resp.Content)
	assert.Equal(t, 2, attempts)
	assert.Equal(t, []time.Duration{time.Millisecond}, delays)

	info := r.RetryPolicyForProvider("transient")
	assert.Equal(t, maxAttempts, info.MaxAttempts)
	assert.Equal(t, time.Millisecond, info.InitialBackoff)
}

func TestCompleteWithRetry_MaxBackoffCapsExponentialDelay(t *testing.T) {
	t.Parallel()

	var delays []time.Duration

	_, err := completeWithRetry(
		context.Background(),
		retryConfig{
			MaxAttempts:    2,
			InitialBackoff: 100 * time.Millisecond,
			MaxBackoff:     150 * time.Millisecond,
			sleep:          captureSleep(&delays),
		},
		retryMetadata{provider: providerOpenAI, model: "gpt-test"},
		func(_ context.Context) (*Response, error) {
			return nil, testProviderError(503, 0)
		},
	)
	require.Error(t, err)
	assert.Equal(t, []time.Duration{100 * time.Millisecond, 150 * time.Millisecond}, delays)
}

func TestRetryDelay_MaxBackoffCapsJitteredDelay(t *testing.T) {
	t.Parallel()

	cfg := retryConfig{
		InitialBackoff: 100 * time.Millisecond,
		MaxBackoff:     110 * time.Millisecond,
		JitterFraction: 0.5,
		randFloat64:    func() float64 { return 1 },
	}.normalized()

	assert.Equal(t, 110*time.Millisecond, retryDelay(cfg, 0, 0))
}

func TestRetryDelay_MaxBackoffCapsRetryAfterDelay(t *testing.T) {
	t.Parallel()

	cfg := retryConfig{
		MaxBackoff:     110 * time.Millisecond,
		JitterFraction: 0.5,
		randFloat64:    func() float64 { return 1 },
	}.normalized()

	assert.Equal(t, 110*time.Millisecond, retryDelay(cfg, 0, 100*time.Millisecond))
}

func TestCompleteWithRetry_StopsWhenRetryBudgetCannotCoverDelay(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer

	ctx := events.WithEmitter(
		context.Background(),
		events.NewRunnerWithLogger(nil, &out),
		events.Event{},
	)

	attempts := 0
	_, err := completeWithRetry(
		ctx,
		retryConfig{
			MaxAttempts:    2,
			InitialBackoff: time.Millisecond,
			MaxElapsedTime: 50 * time.Millisecond,
			sleep:          captureSleep(nil),
		},
		retryMetadata{provider: providerOpenAI, model: "gpt-test"},
		func(_ context.Context) (*Response, error) {
			attempts++

			return nil, testProviderError(503, 100*time.Millisecond)
		},
	)
	require.Error(t, err)
	assert.Equal(t, 1, attempts)

	var retryErr *RetryError
	require.ErrorAs(t, err, &retryErr)
	assert.Equal(t, "budget_exhausted", retryErr.Outcome)
	assert.Equal(t, 1, retryErr.Attempts)

	logs := out.String()
	assert.Contains(t, logs, "outcome=budget_exhausted")
	assert.Contains(t, logs, "retry_after_ms=100")
	assertLogLineContains(t, logs, "outcome=budget_exhausted", "delay_ms=100")
}

func TestCompleteWithRetry_CapsRetryAfterBeforeBudgetCheck(t *testing.T) {
	t.Parallel()

	var delays []time.Duration

	attempts := 0
	resp, err := completeWithRetry(
		context.Background(),
		retryConfig{
			MaxAttempts:    1,
			MaxBackoff:     50 * time.Millisecond,
			MaxElapsedTime: 75 * time.Millisecond,
			sleep:          captureSleep(&delays),
		},
		retryMetadata{provider: providerOpenAI, model: "gpt-test"},
		func(_ context.Context) (*Response, error) {
			attempts++
			if attempts == 1 {
				return nil, testProviderError(503, 100*time.Millisecond)
			}

			return &Response{Content: "ok"}, nil
		},
	)
	require.NoError(t, err)
	assert.Equal(t, "ok", resp.Content)
	assert.Equal(t, 2, attempts)
	assert.Equal(t, []time.Duration{50 * time.Millisecond}, delays)
}

func TestCompleteWithRetry_ExhaustsRetriesWithSummary(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer

	ctx := events.WithEmitter(
		context.Background(),
		events.NewRunnerWithLogger(nil, &out),
		events.Event{},
	)

	attempts := 0
	_, err := completeWithRetry(
		ctx,
		retryConfig{
			MaxAttempts:    2,
			InitialBackoff: time.Millisecond,
			sleep:          captureSleep(nil),
		},
		retryMetadata{provider: providerOpenAI, model: "gpt-test"},
		func(_ context.Context) (*Response, error) {
			attempts++

			return nil, testProviderError(503, 0)
		},
	)
	require.Error(t, err)
	assert.Equal(t, 3, attempts)

	var retryErr *RetryError
	require.ErrorAs(t, err, &retryErr)
	assert.Equal(t, "exhausted", retryErr.Outcome)
	assert.Equal(t, 3, retryErr.Attempts)
	assert.Equal(t, 2, retryErr.Retries)
	assert.Contains(t, err.Error(), "failed after 3 attempts")
	assert.Contains(t, err.Error(), "2 retries")
	assert.Contains(t, err.Error(), "outcome=exhausted")
	assert.Contains(t, err.Error(), "last_status=503")
	assert.Contains(t, err.Error(), "request_id=req-test")
	assert.Contains(t, err.Error(), "provider unavailable")

	var providerErr *ProviderError
	require.ErrorAs(t, err, &providerErr)
	assert.Equal(t, 503, providerErr.StatusCode)

	logs := out.String()
	assert.Contains(t, logs, "event:provider_retry")
	assert.Contains(t, logs, "attempt=2")
	assert.Contains(t, logs, "attempt=3")
	assert.Contains(t, logs, "outcome=scheduled")
	assert.Contains(t, logs, "outcome=exhausted")
	assertLogLineContains(t, logs, "outcome=scheduled", "attempt=2")
	assertLogLineContains(t, logs, "outcome=scheduled", "attempt=3")
	assertLogLineContains(t, logs, "outcome=exhausted", "attempt=3")
}

func TestCompleteWithRetry_ExhaustionReportsFinalProviderError(t *testing.T) {
	t.Parallel()

	firstErr := testProviderError(http.StatusServiceUnavailable, 0)
	firstErr.RequestID = "req-first"
	firstErr.Message = "first transient failure"

	finalErr := testProviderError(http.StatusTooManyRequests, 0)
	finalErr.RequestID = "req-final"
	finalErr.Message = "final rate limit"

	attempts := 0
	_, err := completeWithRetry(
		context.Background(),
		retryConfig{
			MaxAttempts:    1,
			InitialBackoff: time.Millisecond,
			sleep:          captureSleep(nil),
		},
		retryMetadata{provider: providerOpenAI, model: "gpt-test"},
		func(_ context.Context) (*Response, error) {
			attempts++
			if attempts == 1 {
				return nil, firstErr
			}

			return nil, finalErr
		},
	)
	require.Error(t, err)
	assert.Equal(t, 2, attempts)
	assert.Contains(t, err.Error(), "last_status=429")
	assert.Contains(t, err.Error(), "request_id=req-final")
	assert.Contains(t, err.Error(), "final rate limit")
	assert.NotContains(t, err.Error(), "req-first")
	assert.NotContains(t, err.Error(), "first transient failure")

	var retryErr *RetryError
	require.ErrorAs(t, err, &retryErr)
	assert.Equal(t, retryOutcomeExhausted, retryErr.Outcome)
	assert.Equal(t, http.StatusTooManyRequests, retryErr.StatusCode)
	assert.Equal(t, "req-final", retryErr.RequestID)

	var providerErr *ProviderError
	require.ErrorAs(t, err, &providerErr)
	assert.Equal(t, http.StatusTooManyRequests, providerErr.StatusCode)
	assert.Equal(t, "req-final", providerErr.RequestID)
}

func TestCompleteWithRetry_DisabledWithZeroAttempts(t *testing.T) {
	t.Parallel()

	attempts := 0
	_, err := completeWithRetry(
		context.Background(),
		retryConfig{},
		retryMetadata{provider: providerOpenAI, model: "gpt-test"},
		func(_ context.Context) (*Response, error) {
			attempts++

			return nil, testProviderError(503, 0)
		},
	)
	require.Error(t, err)
	assert.Equal(t, 1, attempts)
}

func TestCompleteWithRetry_ReportsNonRetryableAfterRetrySummary(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer

	ctx := events.WithEmitter(
		context.Background(),
		events.NewRunnerWithLogger(nil, &out),
		events.Event{},
	)

	attempts := 0
	finalErr := testProviderError(400, 0)
	finalErr.RequestID = "req-final-bad"
	finalErr.Message = "final bad request"

	_, err := completeWithRetry(
		ctx,
		retryConfig{
			MaxAttempts:    2,
			InitialBackoff: time.Millisecond,
			sleep:          captureSleep(nil),
		},
		retryMetadata{provider: providerOpenAI, model: "gpt-test"},
		func(_ context.Context) (*Response, error) {
			attempts++
			if attempts == 1 {
				return nil, testProviderError(503, 0)
			}

			return nil, finalErr
		},
	)
	require.Error(t, err)
	assert.Equal(t, 2, attempts)

	var retryErr *RetryError
	require.ErrorAs(t, err, &retryErr)
	assert.Equal(t, "non_retryable", retryErr.Outcome)
	assert.Equal(t, 2, retryErr.Attempts)
	assert.Equal(t, 1, retryErr.Retries)
	assert.Equal(t, 400, retryErr.StatusCode)
	assert.Equal(t, "req-final-bad", retryErr.RequestID)
	assert.Contains(t, err.Error(), "failed after 2 attempts")
	assert.Contains(t, err.Error(), "final bad request")

	var providerErr *ProviderError
	require.ErrorAs(t, err, &providerErr)
	assert.Equal(t, http.StatusBadRequest, providerErr.StatusCode)
	assert.Equal(t, "req-final-bad", providerErr.RequestID)

	logs := out.String()
	assert.Contains(t, logs, "outcome=scheduled")
	assert.Contains(t, logs, "outcome=non_retryable")
	assert.Contains(t, logs, "status=400")
	assert.Contains(t, logs, "request_id=req-final-bad")
}

func TestCompleteWithRetry_RespectsContextCancellationDuringDelay(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	attempts := 0
	_, err := completeWithRetry(
		ctx,
		retryConfig{
			MaxAttempts:    3,
			InitialBackoff: time.Millisecond,
			sleep: func(ctx context.Context, _ time.Duration) error {
				cancel()

				return ctx.Err()
			},
		},
		retryMetadata{provider: providerOpenAI, model: "gpt-test"},
		func(_ context.Context) (*Response, error) {
			attempts++

			return nil, testProviderError(429, 0)
		},
	)
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
	assert.NotContains(t, err.Error(), "\n")
	assert.Equal(t, 1, attempts)

	var retryErr *RetryError
	require.ErrorAs(t, err, &retryErr)
	assert.Equal(t, "canceled", retryErr.Outcome)
	assert.Equal(t, 1, retryErr.Attempts)

	var providerErr *ProviderError
	require.ErrorAs(t, err, &providerErr)
	assert.Equal(t, http.StatusTooManyRequests, providerErr.StatusCode)
}

func TestCompleteWithRetry_ReportsCancellationDuringFirstAttempt(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer

	baseCtx := events.WithEmitter(
		context.Background(),
		events.NewRunnerWithLogger(nil, &out),
		events.Event{},
	)

	ctx, cancel := context.WithCancel(baseCtx)
	defer cancel()

	attempts := 0
	_, err := completeWithRetry(
		ctx,
		retryConfig{
			MaxAttempts:    2,
			InitialBackoff: time.Millisecond,
			sleep:          captureSleep(nil),
		},
		retryMetadata{provider: providerOpenAI, model: "gpt-test"},
		func(ctx context.Context) (*Response, error) {
			attempts++

			cancel()

			return nil, ctx.Err()
		},
	)
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, attempts)
	assert.NotContains(t, err.Error(), "context canceled; context canceled")

	var retryErr *RetryError
	require.ErrorAs(t, err, &retryErr)
	assert.Equal(t, "canceled", retryErr.Outcome)
	assert.Equal(t, 1, retryErr.Attempts)
	assert.Equal(t, 0, retryErr.Retries)

	logs := out.String()
	assert.Contains(t, logs, "event:provider_retry")
	assert.Contains(t, logs, "outcome=canceled")
	assert.Contains(t, logs, "attempt=1")
}

func TestCompleteWithRetry_ReportsCancellationDuringRetryAttempt(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	attempts := 0
	_, err := completeWithRetry(
		ctx,
		retryConfig{
			MaxAttempts:    2,
			InitialBackoff: time.Millisecond,
			sleep:          captureSleep(nil),
		},
		retryMetadata{provider: providerOpenAI, model: "gpt-test"},
		func(ctx context.Context) (*Response, error) {
			attempts++
			if attempts == 1 {
				return nil, testProviderError(503, 0)
			}

			cancel()

			return nil, ctx.Err()
		},
	)
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
	assert.NotContains(t, err.Error(), "\n")
	assert.Equal(t, 2, attempts)

	var retryErr *RetryError
	require.ErrorAs(t, err, &retryErr)
	assert.Equal(t, "canceled", retryErr.Outcome)
	assert.Equal(t, 2, retryErr.Attempts)
	assert.Equal(t, http.StatusServiceUnavailable, retryErr.StatusCode)

	var providerErr *ProviderError
	require.ErrorAs(t, err, &providerErr)
	assert.Equal(t, http.StatusServiceUnavailable, providerErr.StatusCode)
}

func TestCompleteWithRetry_CanceledRetryAttemptReportsFinalProviderError(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer

	baseCtx := events.WithEmitter(
		context.Background(),
		events.NewRunnerWithLogger(nil, &out),
		events.Event{},
	)

	ctx, cancel := context.WithCancel(baseCtx)
	defer cancel()

	attempts := 0
	_, err := completeWithRetry(
		ctx,
		retryConfig{
			MaxAttempts:    2,
			InitialBackoff: time.Millisecond,
			sleep:          captureSleep(nil),
		},
		retryMetadata{provider: providerOpenAI, model: "gpt-test"},
		func(context.Context) (*Response, error) {
			attempts++
			if attempts == 1 {
				err := testProviderError(http.StatusServiceUnavailable, 0)
				err.RequestID = "req-first"

				return nil, err
			}

			cancel()

			err := testProviderError(http.StatusTooManyRequests, 0)
			err.RequestID = "req-final"

			return nil, err
		},
	)
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 2, attempts)

	var retryErr *RetryError
	require.ErrorAs(t, err, &retryErr)
	assert.Equal(t, retryOutcomeCanceled, retryErr.Outcome)
	assert.Equal(t, http.StatusTooManyRequests, retryErr.StatusCode)
	assert.Equal(t, "req-final", retryErr.RequestID)

	var providerErr *ProviderError
	require.ErrorAs(t, err, &providerErr)
	assert.Equal(t, http.StatusTooManyRequests, providerErr.StatusCode)
	assert.Equal(t, "req-final", providerErr.RequestID)

	assertLogLineContains(t, out.String(), "outcome=canceled", "status=429", "request_id=req-final")
}

func TestCompleteWithRetry_EmitsCanceledOutcomeAfterContextCancellation(t *testing.T) {
	t.Parallel()

	var (
		out    bytes.Buffer
		ledger bytes.Buffer
	)

	baseCtx := events.WithEmitter(
		context.Background(),
		events.NewRunnerWithOptions(nil, events.RunnerOptions{
			LogWriter:    &out,
			LedgerWriter: &ledger,
		}),
		events.Event{},
	)

	ctx, cancel := context.WithCancel(baseCtx)
	defer cancel()

	attempts := 0
	_, err := completeWithRetry(
		ctx,
		retryConfig{
			MaxAttempts:    1,
			InitialBackoff: time.Millisecond,
			sleep: func(ctx context.Context, _ time.Duration) error {
				cancel()

				return ctx.Err()
			},
		},
		retryMetadata{provider: providerOpenAI, model: "gpt-test"},
		func(_ context.Context) (*Response, error) {
			attempts++

			return nil, testProviderError(429, 0)
		},
	)
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, attempts)

	logs := out.String()
	assert.Contains(t, logs, "event:provider_retry")
	assert.Contains(t, logs, "outcome=scheduled")
	assert.Contains(t, logs, "outcome=canceled")
	assert.Contains(t, logs, "provider=openai")
	assert.Contains(t, logs, "model=gpt-test")

	records := readRetryLifecycleLedgerRecords(t, ledger.String())
	require.Len(t, records, 2)
	require.NotNil(t, records[0].Event)
	require.NotNil(t, records[1].Event)
	assert.Equal(t, events.ProviderRetry, records[0].Event.Type)
	assert.Equal(t, retryOutcomeScheduled, records[0].Event.Metadata["outcome"])
	assert.Equal(t, events.ProviderRetry, records[1].Event.Type)
	assert.Equal(t, retryOutcomeCanceled, records[1].Event.Metadata["outcome"])
	assert.NotEmpty(t, records[1].Event.EventID)
	assert.False(t, records[1].Event.Timestamp.IsZero())
}

func TestCompleteWithRetry_EmitsRetryLifecycleEvents(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer

	ctx := events.WithEmitter(
		context.Background(),
		events.NewRunnerWithLogger(nil, &out),
		events.Event{},
	)

	sensitiveValues := []string{
		strings.Join([]string{"model", "input", "never", "log"}, "-"),
		strings.Join([]string{"auth", "material", "never", "log"}, "-"),
	}

	attempts := 0
	resp, err := completeWithRetry(
		ctx,
		retryConfig{
			MaxAttempts:    1,
			InitialBackoff: time.Millisecond,
			sleep:          captureSleep(nil),
		},
		retryMetadata{provider: providerOpenAI, model: "gpt-test"},
		func(_ context.Context) (*Response, error) {
			attempts++
			if attempts == 1 {
				err := testProviderError(503, 0)
				err.Message = strings.Join(sensitiveValues, " ")

				return nil, err
			}

			return &Response{Content: "ok"}, nil
		},
	)
	require.NoError(t, err)
	assert.Equal(t, "ok", resp.Content)

	logs := out.String()
	assert.Contains(t, logs, "event:provider_retry")
	assert.Contains(t, logs, "model=gpt-test")
	assert.Contains(t, logs, "provider=openai")
	assert.Contains(t, logs, "status=503")
	assert.Contains(t, logs, "classification=retryable")
	assert.Contains(t, logs, "request_id=req-test")
	assert.Contains(t, logs, "delay_ms=1")
	assert.Contains(t, logs, "max_retries=1")
	assert.Contains(t, logs, "max_attempts=2")
	assert.Contains(t, logs, "outcome=scheduled")
	assert.Contains(t, logs, "outcome=success")
	assertLogLineContains(t, logs, "outcome=success", "status=503", "retryable=true")

	for _, value := range sensitiveValues {
		assert.NotContains(t, logs, value)
	}
}

func TestRetryEventMetadata_DoesNotIncludeProviderMessage(t *testing.T) {
	t.Parallel()

	decision := retryDecisionForError(&ProviderError{
		Provider:     providerOpenAI,
		StatusCode:   http.StatusServiceUnavailable,
		RetryAfter:   2 * time.Second,
		RequestID:    "req-safe",
		Message:      "prompt-secret credential-secret",
		Retryability: RetryabilityRetryable,
	})

	metadata := retryEventMetadata(
		retryMetadata{provider: providerOpenAI, model: "gpt-test"},
		decision,
		2,
		3,
	)

	assert.Equal(t, map[string]string{
		"attempt":        "2",
		"classification": "retryable",
		"max_attempts":   "4",
		"max_retries":    "3",
		"provider":       providerOpenAI,
		"request_id":     "req-safe",
		"retry_after_ms": "2000",
		"retryable":      "true",
		"status":         "503",
	}, metadata)

	values := make([]string, 0, len(metadata))
	for _, value := range metadata {
		values = append(values, value)
	}

	joinedValues := strings.Join(values, " ")
	assert.NotContains(t, joinedValues, "prompt-secret")
	assert.NotContains(t, joinedValues, "credential-secret")
}

func TestCompleteWithRetry_EmitsProviderRetryHookPayload(t *testing.T) {
	if os.Getenv("ATTELER_LLM_RETRY_HOOK") == "1" {
		retryHookHelper(t)
		return
	}

	t.Parallel()

	out := t.TempDir() + "/retry-events.jsonl"
	runner := events.NewRunner(map[string][]appconfig.HookConfig{
		events.ProviderRetry: {{
			Command: []string{os.Args[0], "-test.run=TestCompleteWithRetry_EmitsProviderRetryHookPayload"},
			Env: map[string]string{
				"ATTELER_LLM_RETRY_HOOK":     "1",
				"ATTELER_LLM_RETRY_HOOK_OUT": out,
			},
			TimeoutSeconds: 2,
			// This test asserts provider_retry payload shape, not delivery mode.
			// Keep helpers sequential so race-mode subprocesses do not overlap.
			Blocking: true,
		}},
	})

	ctx := events.WithEmitter(
		context.Background(),
		runner,
		events.Event{SessionID: "session-1"},
	)

	sensitive := "prompt-secret credential-secret"
	attempts := 0

	resp, err := completeWithRetry(
		ctx,
		retryConfig{
			MaxAttempts:    1,
			InitialBackoff: time.Millisecond,
			sleep:          captureSleep(nil),
		},
		retryMetadata{provider: providerOpenAI, model: "gpt-test"},
		func(context.Context) (*Response, error) {
			attempts++
			if attempts == 1 {
				err := testProviderError(http.StatusServiceUnavailable, 0)
				err.Message = sensitive

				return nil, err
			}

			return &Response{Content: "ok"}, nil
		},
	)
	require.NoError(t, err)
	require.NotNil(t, resp)

	waitCtx, cancelWait := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelWait()

	require.NoError(t, runner.Wait(waitCtx))

	data, err := os.ReadFile(out)
	require.NoError(t, err)
	assert.NotContains(t, string(data), sensitive)

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	require.Len(t, lines, 2)

	eventsByOutcome := make(map[string]events.Event, len(lines))
	for _, line := range lines {
		var delivered events.Event
		require.NoError(t, json.Unmarshal([]byte(line), &delivered))
		require.NotEmpty(t, delivered.Metadata["outcome"])
		require.NotContains(t, eventsByOutcome, delivered.Metadata["outcome"])

		eventsByOutcome[delivered.Metadata["outcome"]] = delivered
	}

	require.Contains(t, eventsByOutcome, retryOutcomeScheduled)
	require.Contains(t, eventsByOutcome, retryOutcomeSuccess)

	scheduled := eventsByOutcome[retryOutcomeScheduled]
	final := eventsByOutcome[retryOutcomeSuccess]

	assert.Equal(t, events.ProviderRetry, scheduled.Type)
	assert.Equal(t, "session-1", scheduled.SessionID)
	assert.Equal(t, "gpt-test", scheduled.Model)
	assert.Equal(t, retryOutcomeScheduled, scheduled.Metadata["outcome"])
	assert.Equal(t, providerOpenAI, scheduled.Metadata["provider"])
	assert.Equal(t, "503", scheduled.Metadata["status"])
	assert.NotEmpty(t, scheduled.Metadata["delay_ms"])

	assert.Equal(t, events.ProviderRetry, final.Type)
	assert.Equal(t, retryOutcomeSuccess, final.Metadata["outcome"])
	assert.Equal(t, "2", final.Metadata["attempt"])
	assert.Equal(t, "503", final.Metadata["status"])
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

func TestRetryDecision_LegacyStringFallbackIsDegraded(t *testing.T) {
	t.Parallel()

	decision := retryDecisionForError(errors.New("legacy provider: HTTP 503: unavailable"))
	assert.True(t, decision.retryable)
	assert.True(t, decision.legacyMatch)
	assert.Equal(t, 503, decision.statusCode)

	decision = retryDecisionForError(errors.New("legacy provider: HTTP 400: bad request"))
	assert.False(t, decision.retryable)
	assert.False(t, decision.legacyMatch)

	decision = retryDecisionForError(errors.New("legacy provider: HTTP 5030: not actually a known status"))
	assert.False(t, decision.retryable)
	assert.False(t, decision.legacyMatch)

	decision = retryDecisionForError(errors.New("legacy provider: HTTP 503oops: malformed status"))
	assert.False(t, decision.retryable)
	assert.False(t, decision.legacyMatch)
}

func TestRetryDecision_TypedProviderErrorTakesPrecedenceOverMessage(t *testing.T) {
	t.Parallel()

	decision := retryDecisionForError(&ProviderError{
		Provider:     providerOpenAI,
		StatusCode:   400,
		Message:      "body mentions HTTP 503 but status is client error",
		Retryability: RetryabilityNonRetryable,
	})

	assert.False(t, decision.retryable)
	assert.False(t, decision.legacyMatch)
	assert.Equal(t, RetryabilityNonRetryable, decision.class)
	assert.Equal(t, 400, decision.statusCode)
}

func TestRetryDecision_TypedProviderClassificationOverridesStatusDefault(t *testing.T) {
	t.Parallel()

	decision := retryDecisionForError(&ProviderError{
		Provider:     providerOpenAI,
		StatusCode:   http.StatusServiceUnavailable,
		Message:      "provider-specific permanent overload",
		Retryability: RetryabilityNonRetryable,
	})
	assert.False(t, decision.retryable)
	assert.Equal(t, RetryabilityNonRetryable, decision.class)
	assert.Equal(t, http.StatusServiceUnavailable, decision.statusCode)

	decision = retryDecisionForError(&ProviderError{
		Provider:     providerOpenAI,
		StatusCode:   http.StatusBadRequest,
		Message:      "provider-specific transient validation race",
		Retryability: RetryabilityRetryable,
	})
	assert.True(t, decision.retryable)
	assert.Equal(t, RetryabilityRetryable, decision.class)
	assert.Equal(t, http.StatusBadRequest, decision.statusCode)
}

func TestRetryDecision_TypedStatusFallbackDoesNotUseLegacyStringMatching(t *testing.T) {
	t.Parallel()

	decision := retryDecisionForError(&ProviderError{
		Provider:   providerOpenAI,
		StatusCode: http.StatusServiceUnavailable,
		Message:    "typed status only",
	})

	assert.True(t, decision.retryable)
	assert.False(t, decision.legacyMatch)
	assert.Equal(t, RetryabilityRetryable, decision.class)
	assert.Equal(t, http.StatusServiceUnavailable, decision.statusCode)
}

func TestRetryDecisionWithFallback_UsesFallbackForEmptyDecision(t *testing.T) {
	t.Parallel()

	fallback := retryDecision{
		requestID:  "req-fallback",
		class:      RetryabilityRetryable,
		statusCode: http.StatusServiceUnavailable,
		retryable:  true,
	}

	assert.Equal(t, fallback, retryDecisionWithFallback(retryDecision{}, fallback))
	assert.Equal(t, fallback, retryDecisionWithFallback(retryDecision{class: RetryabilityUnknown}, fallback))

	primary := retryDecision{
		requestID:  "req-primary",
		class:      RetryabilityNonRetryable,
		statusCode: http.StatusBadRequest,
	}
	assert.Equal(t, primary, retryDecisionWithFallback(primary, fallback))
}

func TestCompleteWithRetry_LegacyFallbackEmitsDegradedMetadata(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer

	ctx := events.WithEmitter(
		context.Background(),
		events.NewRunnerWithLogger(nil, &out),
		events.Event{},
	)

	attempts := 0
	resp, err := completeWithRetry(
		ctx,
		retryConfig{
			MaxAttempts:    1,
			InitialBackoff: time.Millisecond,
			sleep:          captureSleep(nil),
		},
		retryMetadata{provider: "legacy", model: "legacy-model"},
		func(_ context.Context) (*Response, error) {
			attempts++
			if attempts == 1 {
				return nil, errors.New("legacy provider: HTTP 503: unavailable")
			}

			return &Response{Content: "ok"}, nil
		},
	)
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, 2, attempts)

	logs := out.String()
	assert.Contains(t, logs, "event:provider_retry")
	assert.Contains(t, logs, "legacy_fallback=true")
	assert.Contains(t, logs, "classification=retryable")
}

func TestParseRetryAfter(t *testing.T) {
	t.Parallel()

	assert.Equal(t, 5*time.Second, parseRetryAfter("5"))

	httpDate := time.Now().Add(2 * time.Minute).UTC().Format(http.TimeFormat)
	assert.Greater(t, parseRetryAfter(httpDate), time.Minute)

	assert.Equal(t, time.Duration(0), parseRetryAfter(""))
	assert.Equal(t, time.Duration(0), parseRetryAfter("invalid"))
	assert.Equal(t, time.Duration(0), parseRetryAfter("0"))
	assert.Equal(t, time.Duration(0), parseRetryAfter("-1"))
}

func TestRegistry_CompleteRetriesTransientTypedError(t *testing.T) {
	t.Parallel()

	attempts := 0
	failErr := testProviderError(http.StatusTooManyRequests, time.Second)
	failErr.Provider = "transient"

	r := NewRegistry()
	telemetry := modelroute.NewTelemetry()
	r.SetRouteTelemetry(telemetry)
	r.SetRetry(retryConfig{MaxAttempts: 2, InitialBackoff: time.Millisecond, sleep: captureSleep(nil)})
	r.Register(&retryFakeProvider{
		fakeProvider: fakeProvider{
			name:   "transient",
			models: []string{"m-1"},
			resp:   &Response{Content: "ok"},
		},
		failCount: 1,
		failErr:   failErr,
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

func TestRegistry_CompleteFailureReportsRetrySummary(t *testing.T) {
	t.Parallel()

	attempts := 0
	failErr := testProviderError(http.StatusServiceUnavailable, 0)
	failErr.Provider = "transient"

	r := NewRegistry()
	r.SetRetry(retryConfig{MaxAttempts: 1, InitialBackoff: time.Millisecond, sleep: captureSleep(nil)})
	r.Register(&retryFakeProvider{
		fakeProvider: fakeProvider{
			name:   "transient",
			models: []string{"m-1"},
			resp:   &Response{Content: "unused"},
		},
		failCount: 2,
		failErr:   failErr,
		attempts:  &attempts,
	})

	_, err := r.Complete(context.Background(), CompleteParams{Model: "m-1"})
	require.Error(t, err)
	assert.Equal(t, 2, attempts)
	assert.Contains(t, err.Error(), "transient/m-1 failed after 2 attempts")
	assert.Contains(t, err.Error(), "1 retry")
	assert.Contains(t, err.Error(), "outcome=exhausted")
	assert.Contains(t, err.Error(), "last_status=503")
	assert.Contains(t, err.Error(), "request_id=req-test")

	var retryErr *RetryError
	require.ErrorAs(t, err, &retryErr)
	assert.Equal(t, "transient", retryErr.Provider)
	assert.Equal(t, "m-1", retryErr.Model)
	assert.Equal(t, retryOutcomeExhausted, retryErr.Outcome)
	assert.Equal(t, 2, retryErr.Attempts)
	assert.Equal(t, 1, retryErr.Retries)
	assert.Equal(t, http.StatusServiceUnavailable, retryErr.StatusCode)
	assert.Equal(t, "req-test", retryErr.RequestID)

	var providerErr *ProviderError
	require.ErrorAs(t, err, &providerErr)
	assert.Equal(t, "transient", providerErr.Provider)
	assert.Equal(t, http.StatusServiceUnavailable, providerErr.StatusCode)
}

func TestRegistry_UsesProviderRetryPolicy(t *testing.T) {
	t.Parallel()

	attempts := 0
	r := NewRegistry()
	r.SetRetry(retryConfig{MaxAttempts: 0})
	r.SetProviderRetry("transient", retryConfig{MaxAttempts: 1, InitialBackoff: time.Millisecond, sleep: captureSleep(nil)})
	r.Register(&retryFakeProvider{
		fakeProvider: fakeProvider{
			name:   "transient",
			models: []string{"m-1"},
			resp:   &Response{Content: "ok"},
		},
		failCount: 1,
		failErr:   testProviderError(503, 0),
		attempts:  &attempts,
	})

	resp, err := r.Complete(context.Background(), CompleteParams{Model: "m-1"})
	require.NoError(t, err)
	assert.Equal(t, "ok", resp.Content)
	assert.Equal(t, 2, attempts)

	info := r.RetryPolicyForProvider("transient")
	assert.Equal(t, 1, info.MaxAttempts)
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

func testProviderError(status int, retryAfter time.Duration) *ProviderError {
	return &ProviderError{
		Provider:     providerOpenAI,
		StatusCode:   status,
		RetryAfter:   retryAfter,
		RequestID:    "req-test",
		Message:      "provider unavailable",
		Retryability: retryabilityForStatus(status),
	}
}

func captureSleep(delays *[]time.Duration) func(context.Context, time.Duration) error {
	return func(ctx context.Context, delay time.Duration) error {
		if delays != nil {
			*delays = append(*delays, delay)
		}

		return ctx.Err()
	}
}

func httpStatusName(status int) string {
	return "status_" + strconv.Itoa(status)
}

func assertLogLineContains(t *testing.T, logs string, required ...string) {
	t.Helper()

	for line := range strings.SplitSeq(logs, "\n") {
		if line == "" {
			continue
		}

		matches := true

		for _, want := range required {
			if !strings.Contains(line, want) {
				matches = false
				break
			}
		}

		if matches {
			return
		}
	}

	assert.Failf(t, "missing log line", "no log line contained %v in:\n%s", required, logs)
}

func readRetryLifecycleLedgerRecords(t *testing.T, data string) []events.LedgerRecord {
	t.Helper()

	lines := strings.Split(strings.TrimSpace(data), "\n")
	records := make([]events.LedgerRecord, 0, len(lines))

	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}

		var record events.LedgerRecord
		require.NoError(t, json.Unmarshal([]byte(line), &record))

		records = append(records, record)
	}

	return records
}

func retryHookHelper(t *testing.T) {
	t.Helper()

	if os.Getenv("ATTELER_EVENT_TYPE") != events.ProviderRetry {
		require.Failf(t, "unexpected event type", "ATTELER_EVENT_TYPE = %q", os.Getenv("ATTELER_EVENT_TYPE"))
	}

	data, err := io.ReadAll(os.Stdin)
	require.NoError(t, err)

	out := os.Getenv("ATTELER_LLM_RETRY_HOOK_OUT")
	require.NotEmpty(t, out)

	file, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // test helper writes to temp path supplied by parent test.
	require.NoError(t, err)

	_, err = file.Write(data)
	require.NoError(t, err)

	require.NoError(t, file.Close())

	os.Exit(0)
}
