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
