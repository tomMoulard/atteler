package llm

import (
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewProviderHTTPError_ExtractsTypedMetadata(t *testing.T) {
	t.Parallel()

	resp := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header: http.Header{
			"Retry-After":  []string{"7"},
			"X-Request-Id": []string{"req-typed"},
		},
	}

	err := newProviderHTTPError(providerOpenAI, resp, []byte(`{"error":{"type":"rate_limit","message":"slow down"}}`))

	require.NotNil(t, err)
	assert.Equal(t, providerOpenAI, err.Provider)
	assert.Equal(t, http.StatusTooManyRequests, err.StatusCode)
	assert.Equal(t, 7*time.Second, err.RetryAfter)
	assert.Equal(t, "req-typed", err.RequestID)
	assert.Equal(t, RetryabilityRetryable, err.Retryability)
	assert.Equal(t, "rate_limit: slow down", err.Message)
	assert.Contains(t, err.Error(), "request_id=req-typed")
	assert.Contains(t, err.Error(), "retry_after=7s")
}

func TestNewProviderHTTPError_ClassifiesNonRetryableStatus(t *testing.T) {
	t.Parallel()

	resp := &http.Response{
		StatusCode: http.StatusUnauthorized,
		Header: http.Header{
			"Request-Id": []string{"req-auth"},
		},
	}

	err := newProviderHTTPError(providerAnthropic, resp, []byte(`{"message":"bad key"}`))

	require.NotNil(t, err)
	assert.Equal(t, http.StatusUnauthorized, err.StatusCode)
	assert.Equal(t, RetryabilityNonRetryable, err.Retryability)
	assert.Equal(t, "req-auth", err.RequestID)
	assert.Equal(t, "bad key", err.Message)
	assert.False(t, err.IsRetryable())
}

func TestProviderError_IsRetryableFallsBackToTypedStatus(t *testing.T) {
	t.Parallel()

	retryable := &ProviderError{StatusCode: http.StatusServiceUnavailable}
	assert.True(t, retryable.IsRetryable())
	assert.Contains(t, retryable.Error(), string(RetryabilityRetryable))

	nonRetryable := &ProviderError{StatusCode: http.StatusUnauthorized}
	assert.False(t, nonRetryable.IsRetryable())
	assert.Contains(t, nonRetryable.Error(), string(RetryabilityNonRetryable))

	explicitUnknown := &ProviderError{StatusCode: http.StatusServiceUnavailable, Retryability: RetryabilityUnknown}
	assert.True(t, explicitUnknown.IsRetryable())
	assert.Contains(t, explicitUnknown.Error(), string(RetryabilityRetryable))

	invalidClassification := &ProviderError{StatusCode: http.StatusServiceUnavailable, Retryability: Retryability("invalid")}
	assert.True(t, invalidClassification.IsRetryable())
	assert.Equal(t, RetryabilityRetryable, invalidClassification.retryability())

	okPayload := &ProviderError{StatusCode: http.StatusOK, Retryability: RetryabilityUnknown}
	assert.False(t, okPayload.IsRetryable())
	assert.Equal(t, RetryabilityUnknown, okPayload.retryability())
	assert.Contains(t, okPayload.Error(), string(RetryabilityUnknown))
}

func TestNewProviderPayloadError_ClassifiesRateLimitPayload(t *testing.T) {
	t.Parallel()

	err := newProviderPayloadError(
		providerOpenAI,
		http.StatusOK,
		http.Header{
			"Retry-After":  []string{"3"},
			"X-Request-Id": []string{"req-payload"},
		},
		"rate_limit_error",
		"slow down",
	)

	assert.Equal(t, providerOpenAI, err.Provider)
	assert.Equal(t, http.StatusOK, err.StatusCode)
	assert.Equal(t, 3*time.Second, err.RetryAfter)
	assert.Equal(t, "req-payload", err.RequestID)
	assert.Equal(t, "rate_limit_error: slow down", err.Message)
	assert.Equal(t, RetryabilityRetryable, err.Retryability)
	assert.True(t, err.IsRetryable())
}

func TestNewProviderPayloadError_ClassifiesRetryAfterPayload(t *testing.T) {
	t.Parallel()

	err := newProviderPayloadError(
		providerCodex,
		http.StatusOK,
		http.Header{"Retry-After": []string{"2"}},
		"provider_error",
		"try again later",
	)

	assert.Equal(t, 2*time.Second, err.RetryAfter)
	assert.Equal(t, RetryabilityRetryable, err.Retryability)
	assert.True(t, err.IsRetryable())
}

func TestNewProviderPayloadError_LeavesUnknownPayloadUnretryable(t *testing.T) {
	t.Parallel()

	err := newProviderPayloadError(providerOpenAI, http.StatusOK, nil, "invalid_request_error", "bad input")

	assert.Equal(t, RetryabilityUnknown, err.Retryability)
	assert.Equal(t, RetryabilityUnknown, err.retryability())
	assert.False(t, err.IsRetryable())
}

func TestProviderRequestID_RecognizesProviderHeaderVariants(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		header string
		value  string
	}{
		{header: "X-Amzn-Requestid", value: "aws-req"},
		{header: "X-Amzn-Request-Id", value: "aws-req-dashed"},
		{header: "Cf-Ray", value: "cf-ray-id"},
	} {
		t.Run(tc.header, func(t *testing.T) {
			t.Parallel()

			got := providerRequestID(http.Header{tc.header: []string{tc.value}})

			assert.Equal(t, tc.value, got)
		})
	}
}

func TestProviderErrorMessage_TruncatesBodies(t *testing.T) {
	t.Parallel()

	message := providerErrorMessage([]byte(strings.Repeat("x", providerErrorMessageLimit+10)))

	assert.Len(t, []rune(message), providerErrorMessageLimit+1)
	assert.True(t, strings.HasSuffix(message, "…"))
}

func TestProviderErrorMessage_TruncatesBodiesAtRuneBoundary(t *testing.T) {
	t.Parallel()

	message := providerErrorMessage([]byte(strings.Repeat("x", providerErrorMessageLimit-1) + "éoverflow"))

	assert.True(t, utf8.ValidString(message))
	assert.Equal(t, strings.Repeat("x", providerErrorMessageLimit-1)+"…", message)
}
