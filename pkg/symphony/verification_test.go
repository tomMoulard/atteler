//nolint:paralleltest // Verification gate tests execute shell commands; keep them sequential to avoid process-limit flakes.
package symphony

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunVerificationGatesAppliesCommandAllowList(t *testing.T) {
	workspacePath := t.TempDir()
	cfg := Config{
		Publish: PublishConfig{
			VerificationAllowCommands:  []string{"printf"},
			VerificationOutputMaxBytes: defaultPRGateOutputBytes,
			VerificationGates: []VerificationGateConfig{{
				Name:     "unit",
				Command:  "printf ok; touch denied-by-allow-list",
				Timeout:  time.Second,
				Required: true,
			}},
		},
	}

	report, err := runVerificationGates(t.Context(), cfg, Issue{ID: "node", Identifier: "GH-12"}, Workspace{Path: workspacePath})
	require.NoError(t, err)
	require.Len(t, report.Gates, 1)

	assert.False(t, report.Passed)
	assert.Equal(t, []string{"unit"}, report.FailedRequired)
	assert.Equal(t, VerificationFailed, report.Gates[0].Status)
	assert.Contains(t, report.Gates[0].Error, "allow list")
	assert.NoFileExists(t, filepath.Join(workspacePath, "denied-by-allow-list"))
}

func TestRunVerificationGatesAppliesCommandDenyList(t *testing.T) {
	workspacePath := t.TempDir()
	cfg := Config{
		Publish: PublishConfig{
			VerificationDenyCommands:   []string{"touch"},
			VerificationOutputMaxBytes: defaultPRGateOutputBytes,
			VerificationGates: []VerificationGateConfig{{
				Name:     "unit",
				Command:  "touch denied-by-deny-list",
				Timeout:  time.Second,
				Required: true,
			}},
		},
	}

	report, err := runVerificationGates(t.Context(), cfg, Issue{ID: "node", Identifier: "GH-12"}, Workspace{Path: workspacePath})
	require.NoError(t, err)
	require.Len(t, report.Gates, 1)

	assert.False(t, report.Passed)
	assert.Equal(t, []string{"unit"}, report.FailedRequired)
	assert.Equal(t, VerificationFailed, report.Gates[0].Status)
	assert.Contains(t, report.Gates[0].Error, "command.deny")
	assert.NoFileExists(t, filepath.Join(workspacePath, "denied-by-deny-list"))
}

func TestRunVerificationGatesDeniesNetworkCommandsByDefault(t *testing.T) {
	cfg := Config{
		Publish: PublishConfig{
			VerificationAllowCommands:  []string{"curl"},
			VerificationOutputMaxBytes: defaultPRGateOutputBytes,
			VerificationGates: []VerificationGateConfig{{
				Name:     "security-scan",
				Command:  "curl https://example.invalid",
				Timeout:  time.Second,
				Required: true,
			}},
		},
	}

	report, err := runVerificationGates(t.Context(), cfg, Issue{ID: "node", Identifier: "GH-12"}, Workspace{Path: t.TempDir()})
	require.NoError(t, err)
	require.Len(t, report.Gates, 1)

	assert.False(t, report.Passed)
	assert.Equal(t, []string{"security-scan"}, report.FailedRequired)
	assert.Equal(t, VerificationFailed, report.Gates[0].Status)
	assert.Contains(t, report.Gates[0].Error, "network-like command requires explicit policy allowance")
}

func TestRunVerificationGatesDoesNotBlockOnOptionalFailure(t *testing.T) {
	cfg := Config{
		Publish: PublishConfig{
			VerificationAllowCommands:  []string{"printf", "false"},
			VerificationOutputMaxBytes: defaultPRGateOutputBytes,
			VerificationGates: []VerificationGateConfig{
				{
					Name:     "unit",
					Command:  "printf ok",
					Timeout:  time.Second,
					Required: true,
				},
				{
					Name:     "advisory",
					Command:  "false",
					Timeout:  time.Second,
					Required: false,
				},
			},
		},
	}

	report, err := runVerificationGates(t.Context(), cfg, Issue{ID: "node", Identifier: "GH-12"}, Workspace{Path: t.TempDir()})
	require.NoError(t, err)
	require.Len(t, report.Gates, 2)

	assert.True(t, report.Passed)
	assert.Empty(t, report.FailedRequired)
	assert.Equal(t, VerificationPassed, report.Gates[0].Status)
	assert.Equal(t, VerificationFailed, report.Gates[1].Status)
	assert.False(t, report.Gates[1].Required)
}

func TestRunVerificationGatesRedactsCapturedSecretOutput(t *testing.T) {
	workspacePath := t.TempDir()
	cfg := Config{
		Publish: PublishConfig{
			VerificationAllowCommands:  []string{"printf"},
			VerificationOutputMaxBytes: defaultPRGateOutputBytes,
			VerificationGates: []VerificationGateConfig{{
				Name:     "unit",
				Command:  "printf 'api_key=supersecret'",
				Timeout:  time.Second,
				Required: true,
			}},
		},
	}

	report, err := runVerificationGates(t.Context(), cfg, Issue{ID: "node", Identifier: "GH-12"}, Workspace{Path: workspacePath})
	require.NoError(t, err)
	require.Len(t, report.Gates, 1)

	assert.True(t, report.Passed)
	assert.NotContains(t, report.Gates[0].Command, "supersecret")
	assert.NotContains(t, report.Gates[0].Stdout, "supersecret")
	assert.Contains(t, report.Gates[0].Command, "[REDACTED]")
	assert.Contains(t, report.Gates[0].Stdout, "[REDACTED]")
}

func TestRunVerificationGatesMarksTruncatedOutput(t *testing.T) {
	workspacePath := t.TempDir()
	cfg := Config{
		Publish: PublishConfig{
			VerificationAllowCommands:  []string{"printf"},
			VerificationOutputMaxBytes: 3,
			VerificationGates: []VerificationGateConfig{{
				Name:     "unit",
				Command:  "printf abcdef",
				Timeout:  time.Second,
				Required: true,
			}},
		},
	}

	report, err := runVerificationGates(t.Context(), cfg, Issue{ID: "node", Identifier: "GH-12"}, Workspace{Path: workspacePath})
	require.NoError(t, err)
	require.Len(t, report.Gates, 1)

	assert.False(t, report.Passed)
	assert.Equal(t, []string{"unit"}, report.FailedRequired)
	assert.Equal(t, VerificationFailed, report.Gates[0].Status)
	assert.Equal(t, "abc", report.Gates[0].Stdout)
	assert.True(t, report.Gates[0].OutputTruncated)
	assert.Contains(t, report.Gates[0].Error, "output exceeded 3 bytes")
}

func TestRunVerificationGatesRedactsFailedGateName(t *testing.T) {
	cfg := Config{
		Publish: PublishConfig{
			VerificationAllowCommands:  []string{"false"},
			VerificationOutputMaxBytes: defaultPRGateOutputBytes,
			VerificationGates: []VerificationGateConfig{{
				Name:     "api_key=gate-secret",
				Command:  "false",
				Timeout:  time.Second,
				Required: true,
			}},
		},
	}

	report, err := runVerificationGates(t.Context(), cfg, Issue{ID: "node", Identifier: "GH-12"}, Workspace{Path: t.TempDir()})
	require.NoError(t, err)
	require.Len(t, report.Gates, 1)

	assert.False(t, report.Passed)
	assert.NotContains(t, report.Gates[0].Name, "gate-secret")
	assert.NotContains(t, report.FailedRequired[0], "gate-secret")
	assert.Contains(t, report.Gates[0].Name, "[REDACTED]")
	assert.Contains(t, (&VerificationGateError{Report: report}).Error(), "[REDACTED]")
}

func TestRunVerificationGatesStopsWhenParentContextCanceledDuringGate(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	cfg := Config{
		Publish: PublishConfig{
			VerificationAllowCommands:  []string{"sleep"},
			VerificationOutputMaxBytes: defaultPRGateOutputBytes,
			VerificationGates: []VerificationGateConfig{{
				Name:     "unit",
				Command:  "sleep 5",
				Timeout:  time.Minute,
				Required: true,
			}},
		},
	}

	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	report, err := runVerificationGates(ctx, cfg, Issue{ID: "node", Identifier: "GH-12"}, Workspace{Path: t.TempDir()})
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
	require.Len(t, report.Gates, 1)
	assert.False(t, report.Passed)
	assert.Equal(t, []string{"unit"}, report.FailedRequired)
}
