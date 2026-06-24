package llm

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProviderHealthFromDiagnostics_RedactsCredentialLikeValues(t *testing.T) {
	t.Parallel()

	fakeKey := "sk-" + "diagnosticcredential123456"
	opaqueValue := "diagnostic-" + "opaque-value"

	health := providerHealthFromDiagnostics("codex", AdapterDiagnostics{
		Contract: AdapterContract{
			Provider:       "codex",
			AdapterVersion: "test",
			Credential:     "api_key=" + fakeKey,
			KillSwitches:   []string{"refresh_token=" + opaqueValue},
		},
		Checks: []ReadinessCheck{
			{Name: "local_credentials", Status: ReadinessFailed, Detail: "access_token=" + opaqueValue},
		},
		Warnings: []string{"Authorization: Bearer " + opaqueValue},
		Models:   []string{"gpt-test"},
	})

	require.Error(t, health.Error)
	assert.NotContains(t, health.Error.Error(), opaqueValue)
	assert.NotContains(t, health.Checks[0].Detail, opaqueValue)
	assert.NotContains(t, health.Warnings[0], opaqueValue)
	require.NotNil(t, health.Contract)
	assert.NotContains(t, health.Contract.Credential, fakeKey)
	require.Len(t, health.Contract.KillSwitches, 1)
	assert.NotContains(t, health.Contract.KillSwitches[0], opaqueValue)
}

func TestReadinessReport_RedactsStoredCredentialLikeErrors(t *testing.T) {
	t.Parallel()

	fakeKey := "sk-" + "readinesscredential123456"
	opaqueValue := "readiness-" + "opaque-value"

	r := NewRegistry()
	r.upsertReadinessProviderLocked(ProviderReadiness{
		Name:            "openai",
		Status:          ProviderStatusFailed,
		Error:           errors.New("api_key=" + fakeKey),
		HealthError:     errors.New("Authorization: Bearer " + opaqueValue),
		ModelFetchError: &ProviderError{Provider: "openai", StatusCode: 401, Message: "refresh_token=" + opaqueValue, RequestID: "api_key=" + fakeKey},
	})
	r.readiness.Default = DefaultSelectionReport{
		Provider:      "openai",
		Model:         "gpt-test",
		ProviderError: errors.New("api_key=" + fakeKey),
		ModelError:    errors.New("access_token=" + opaqueValue),
	}

	report := r.ReadinessReport()
	require.Len(t, report.Providers, 1)
	provider := report.Providers[0]

	require.Error(t, provider.Error)
	assert.NotContains(t, provider.Error.Error(), fakeKey)
	require.Error(t, provider.HealthError)
	assert.NotContains(t, provider.HealthError.Error(), opaqueValue)
	require.Error(t, provider.ModelFetchError)
	assert.NotContains(t, provider.ModelFetchError.Error(), opaqueValue)
	assert.NotContains(t, provider.ModelFetchError.Error(), fakeKey)
	require.Error(t, report.Default.ProviderError)
	assert.NotContains(t, report.Default.ProviderError.Error(), fakeKey)
	require.Error(t, report.Default.ModelError)
	assert.NotContains(t, report.Default.ModelError.Error(), opaqueValue)
}

func TestProviderReadinessSummary_RedactsCredentialLikeErrors(t *testing.T) {
	t.Parallel()

	fakeKey := "sk-" + "summarycredential123456"

	summary := providerReadinessSummary(&ProviderReadiness{
		Name:   "openai",
		Status: ProviderStatusFailed,
		Error:  errors.New("api_key=" + fakeKey),
	})

	assert.Contains(t, summary, "[REDACTED]")
	assert.NotContains(t, summary, fakeKey)
}
