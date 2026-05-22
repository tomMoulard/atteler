package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCopyToClipboard_RequiresActiveContext(t *testing.T) {
	t.Parallel()

	err := copyToClipboard(nil, "text") //nolint:staticcheck // Verify nil contexts are rejected instead of panicking.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "context is required")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err = copyToClipboard(ctx, "text")
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
}
