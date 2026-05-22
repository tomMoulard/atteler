package shell

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRunBash_CapturesStdoutAndEnv(t *testing.T) {
	t.Parallel()

	result, err := RunBash(context.Background(), Options{
		Command: `printf "hello $ATTELER_TEST_VALUE"`,
		Env:     map[string]string{"ATTELER_TEST_VALUE": "world"},
	})

	require.NoError(t, err)
	require.Equal(t, "hello world", result.Stdout)
	require.Empty(t, result.Stderr)
	require.Positive(t, result.Duration)
}

func TestRunBash_ReturnsStderrAndExitError(t *testing.T) {
	t.Parallel()

	result, err := RunBash(context.Background(), Options{Command: `printf problem >&2; exit 7`})

	require.Error(t, err)
	require.Contains(t, err.Error(), "bash command failed")
	require.Equal(t, "problem", result.Stderr)
	require.NotEmpty(t, result.ExitError)
}

func TestRunBash_TimesOut(t *testing.T) {
	t.Parallel()

	_, err := RunBash(context.Background(), Options{Command: `sleep 1`, Timeout: 10 * time.Millisecond})

	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "timed out") || strings.Contains(err.Error(), "killed"))
}

func TestRunBash_RejectsBlankCommand(t *testing.T) {
	t.Parallel()

	_, err := RunBash(context.Background(), Options{Command: " \t"})
	require.Error(t, err)
}

func TestRunBash_RequiresActiveContext(t *testing.T) {
	t.Parallel()

	_, err := RunBash(nil, Options{Command: "echo hello"}) //nolint:staticcheck // Verify nil contexts are rejected instead of panicking.
	require.Error(t, err)
	require.Contains(t, err.Error(), "context is required")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = RunBash(ctx, Options{Command: " \t"})
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
}

func TestRunBash_LimitsCapturedOutputBytes(t *testing.T) {
	t.Parallel()

	result, err := RunBash(context.Background(), Options{
		Command:        `printf abcdef`,
		MaxOutputBytes: 3,
	})

	require.Error(t, err)
	require.Contains(t, err.Error(), "output exceeded 3 bytes")
	require.Equal(t, "abc", result.Stdout)
	require.Empty(t, result.Stderr)
	require.True(t, result.OutputTruncated)
}

func TestRunInteractive_RejectsBlankCommand(t *testing.T) {
	t.Parallel()

	_, err := RunInteractive(context.Background(), Options{Command: ""})
	require.Error(t, err)
	require.Contains(t, err.Error(), "command is required")
}

func TestRunInteractive_RejectsNilContext(t *testing.T) {
	t.Parallel()

	_, err := RunInteractive(nil, Options{Command: "echo hello"}) //nolint:staticcheck // intentional nil context for test
	require.Error(t, err)
	require.Contains(t, err.Error(), "context is required")
}

func TestRunInteractive_RejectsCanceledContextBeforeCommandValidation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := RunInteractive(ctx, Options{Command: " \t"})
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
}

func TestRunInteractive_RunsSimpleCommand(t *testing.T) {
	t.Parallel()

	result, err := RunInteractive(context.Background(), Options{Command: "true"})
	require.NoError(t, err)
	require.Positive(t, result.Duration)
	require.Empty(t, result.ExitError)
}

func TestRunInteractive_ReportsNonZeroExit(t *testing.T) {
	t.Parallel()

	result, err := RunInteractive(context.Background(), Options{Command: "exit 42"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "interactive command failed")
	require.NotEmpty(t, result.ExitError)
}

func TestRunInteractive_RespectsCancel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err := RunInteractive(ctx, Options{Command: "sleep 10"})
	require.Error(t, err)
}
