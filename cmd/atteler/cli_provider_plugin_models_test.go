package main

import (
	"context"
	"errors"
	"io"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/llm"
)

type providerCommandTestProvider struct {
	fetchErr      error
	healthErr     error
	name          string
	models        []string
	fetchedModels []string
	healthCalls   int
}

func (p *providerCommandTestProvider) Name() string { return p.name }

func (p *providerCommandTestProvider) Models() []string { return p.models }

func (p *providerCommandTestProvider) FetchModels(context.Context) ([]string, error) {
	if p.fetchErr != nil {
		return nil, p.fetchErr
	}

	if p.fetchedModels != nil {
		return p.fetchedModels, nil
	}

	return p.models, nil
}

func (p *providerCommandTestProvider) HealthCheck(context.Context) error {
	p.healthCalls++

	return p.healthErr
}

func (p *providerCommandTestProvider) Complete(_ context.Context, params llm.CompleteParams) (*llm.Response, error) {
	return &llm.Response{Content: "ok", Model: params.Model}, nil
}

func (p *providerCommandTestProvider) ModelContextWindow(string) int { return 128_000 }

func TestListModelsWarnsWhenStaticFallbackIsUsed(t *testing.T) { //nolint:paralleltest // Captures process stdout/stderr.
	reg := llm.NewRegistry()
	reg.Register(&providerCommandTestProvider{
		name:     "alpha",
		models:   []string{"static-b", "static-a"},
		fetchErr: errors.New("models unavailable"),
	})

	stdout, stderr, err := captureStdoutAndStderrForProviderCommands(t, func() error {
		return listModels(context.Background(), reg)
	})

	require.NoError(t, err)
	assert.Equal(t, "alpha/static-a\nalpha/static-b\n", stdout)
	assert.Contains(t, stderr, "warning: alpha live model fetch failed; using static fallback: models unavailable")
}

func TestListModelsUsesLiveModelCatalog(t *testing.T) { //nolint:paralleltest // Captures process stdout/stderr.
	reg := llm.NewRegistry()
	reg.Register(&providerCommandTestProvider{
		name:          "alpha",
		models:        []string{"static"},
		fetchedModels: []string{"live-b", "live-a"},
	})

	stdout, stderr, err := captureStdoutAndStderrForProviderCommands(t, func() error {
		return listModels(context.Background(), reg)
	})

	require.NoError(t, err)
	assert.Equal(t, "alpha/live-a\nalpha/live-b\n", stdout)
	assert.Empty(t, stderr)
}

func captureStdoutAndStderrForProviderCommands(
	t *testing.T,
	fn func() error,
) (stdout, stderr string, fnErr error) {
	t.Helper()

	oldStdout := os.Stdout
	oldStderr := os.Stderr

	stdoutReader, stdoutWriter, err := os.Pipe()
	require.NoError(t, err)
	stderrReader, stderrWriter, err := os.Pipe()
	require.NoError(t, err)

	os.Stdout = stdoutWriter
	os.Stderr = stderrWriter

	fnErr = fn()

	os.Stdout = oldStdout
	os.Stderr = oldStderr

	require.NoError(t, stdoutWriter.Close())
	require.NoError(t, stderrWriter.Close())

	stdoutBytes, err := io.ReadAll(stdoutReader)
	require.NoError(t, err)
	require.NoError(t, stdoutReader.Close())

	stderrBytes, err := io.ReadAll(stderrReader)
	require.NoError(t, err)
	require.NoError(t, stderrReader.Close())

	return string(stdoutBytes), string(stderrBytes), fnErr
}
