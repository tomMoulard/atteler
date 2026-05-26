package llm

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func readinessChecksByName(checks []ReadinessCheck) map[string]ReadinessCheck {
	out := make(map[string]ReadinessCheck, len(checks))
	for _, check := range checks {
		out[check.Name] = check
	}

	return out
}

func TestAdapterDiagnostics_ErrorOnlyReportsFailedChecks(t *testing.T) {
	t.Parallel()

	diagnostics := AdapterDiagnostics{
		Checks: []ReadinessCheck{
			{Name: "local_credentials", Status: ReadinessOK, Detail: "loaded"},
			{Name: "network_reachability", Status: ReadinessSkipped, Detail: "not probed"},
			{Name: "model_availability", Status: ReadinessWarning, Detail: "static catalog"},
			{Name: "token_refresh", Status: ReadinessFailed, Detail: "missing refresh token"},
		},
	}

	err := diagnostics.Error()
	require.Error(t, err)

	assert.False(t, diagnostics.Healthy())
	assert.Equal(t, "token_refresh: missing refresh token", err.Error())
}

func TestAdapterDiagnostics_EmptyChecksFailClosed(t *testing.T) {
	t.Parallel()

	diagnostics := AdapterDiagnostics{
		Contract: AdapterContract{AdapterVersion: "private-adapter-v1"},
	}

	err := diagnostics.Error()
	require.Error(t, err)

	assert.False(t, diagnostics.Healthy())
	assert.Equal(t, "no readiness checks reported", err.Error())
}

func TestAdapterDiagnostics_WarningsAndSkippedChecksStayHealthy(t *testing.T) {
	t.Parallel()

	diagnostics := AdapterDiagnostics{
		Checks: []ReadinessCheck{
			{Name: "local_credentials", Status: ReadinessOK, Detail: "loaded"},
			{Name: "network_reachability", Status: ReadinessSkipped, Detail: "not probed"},
			{Name: "model_availability", Status: ReadinessWarning, Detail: "static catalog"},
		},
	}

	assert.True(t, diagnostics.Healthy())
	assert.NoError(t, diagnostics.Error())
}
