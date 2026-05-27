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

func TestListModelsPrintsConfiguredAliasAsBareAliasMapping(t *testing.T) { //nolint:paralleltest // Captures process stdout/stderr.
	reg := llm.NewRegistry()
	reg.Register(&providerCommandTestProvider{
		name:   "codex",
		models: []string{"static-model"},
	})
	require.NoError(t, reg.SetModelAlias("fast", "codex", "static-model"))

	stdout, stderr, err := captureStdoutAndStderrForProviderCommands(t, func() error {
		return listModels(context.Background(), reg)
	})

	require.NoError(t, err)
	assert.Empty(t, stderr)
	assert.Contains(t, stdout, "codex/static-model\n")
	assert.Contains(t, stdout, "fast -> codex/static-model\n")
	assert.NotContains(t, stdout, "codex/fast")
}

func TestExplainModelResolutionPrintsDiagnostic(t *testing.T) { //nolint:paralleltest // Captures process stdout/stderr.
	reg := llm.NewRegistry()
	reg.Register(&providerCommandTestProvider{
		name:   "codex",
		models: []string{"static-model"},
	})

	stdout, stderr, err := captureStdoutAndStderrForProviderCommands(t, func() error {
		return explainModelResolution(context.Background(), "static-model", reg)
	})

	require.NoError(t, err)
	assert.Empty(t, stderr)
	assert.Contains(t, stdout, "Model resolution")
	assert.Contains(t, stdout, "requested: static-model")
	assert.Contains(t, stdout, "provider: codex")
	assert.Contains(t, stdout, "provider_model: static-model")
	assert.Contains(t, stdout, "provenance: static")
	assert.Contains(t, stdout, "bare model matched exactly one registered provider claim")
}

func TestExplainModelResolutionPrintsProviderQualifiedDiagnostic(t *testing.T) { //nolint:paralleltest // Captures process stdout/stderr.
	reg := llm.NewRegistry()
	reg.Register(&providerCommandTestProvider{
		name:   "codex",
		models: []string{"static-model"},
	})

	stdout, stderr, err := captureStdoutAndStderrForProviderCommands(t, func() error {
		return explainModelResolution(context.Background(), "codex/private-model", reg)
	})

	require.NoError(t, err)
	assert.Empty(t, stderr)
	assert.Contains(t, stdout, "requested: codex/private-model")
	assert.Contains(t, stdout, "provider: codex")
	assert.Contains(t, stdout, "provider_model: private-model")
	assert.Contains(t, stdout, "provenance: user_override")
	assert.Contains(t, stdout, "provider-qualified model selected provider directly")
}

func TestExplainModelResolutionPrintsExactSlashModelDiagnostic(t *testing.T) { //nolint:paralleltest // Captures process stdout/stderr.
	reg := llm.NewRegistry()
	reg.Register(&providerCommandTestProvider{
		name:   "codex",
		models: []string{"namespace/model"},
	})

	stdout, stderr, err := captureStdoutAndStderrForProviderCommands(t, func() error {
		return explainModelResolution(context.Background(), "namespace/model", reg)
	})

	require.NoError(t, err)
	assert.Empty(t, stderr)
	assert.Contains(t, stdout, "requested: namespace/model")
	assert.Contains(t, stdout, "provider: codex")
	assert.Contains(t, stdout, "provider_model: namespace/model")
	assert.Contains(t, stdout, "provenance: static")
	assert.Contains(t, stdout, "provider prefix was not registered; full model ID matched exactly one registered provider claim")
}

func TestExplainModelResolutionReportsUnknownModel(t *testing.T) { //nolint:paralleltest // Captures process stdout/stderr.
	reg := llm.NewRegistry()
	reg.Register(&providerCommandTestProvider{
		name:   "codex",
		models: []string{"static-model"},
	})

	stdout, stderr, err := captureStdoutAndStderrForProviderCommands(t, func() error {
		return explainModelResolution(context.Background(), "missing-model", reg)
	})

	require.Error(t, err)
	assert.Empty(t, stderr)
	assert.Contains(t, err.Error(), `unknown model "missing-model"`)
	assert.Contains(t, stdout, "requested: missing-model")
	assert.Contains(t, stdout, "status: unresolved")
	assert.Contains(t, stdout, `error: llm: unknown model "missing-model"`)
	assert.Contains(t, stdout, "no registered provider catalog, live fetch, configured alias, or user override claims this bare model")
	assert.NotContains(t, stdout, "candidates:")
}

func TestExplainModelResolutionPrintsConfiguredAliasDiagnostic(t *testing.T) { //nolint:paralleltest // Captures process stdout/stderr.
	reg := llm.NewRegistry()
	reg.Register(&providerCommandTestProvider{
		name:   "codex",
		models: []string{"static-model"},
	})
	require.NoError(t, reg.SetModelAlias("fast", "codex", "static-model"))

	stdout, stderr, err := captureStdoutAndStderrForProviderCommands(t, func() error {
		return explainModelResolution(context.Background(), "fast", reg)
	})

	require.NoError(t, err)
	assert.Empty(t, stderr)
	assert.Contains(t, stdout, "requested: fast")
	assert.Contains(t, stdout, "provider: codex")
	assert.Contains(t, stdout, "provider_model: static-model")
	assert.Contains(t, stdout, "provenance: configured_alias")
	assert.Contains(t, stdout, "codex/static-model [configured_alias]")
}

func TestExplainModelResolutionMarksConfiguredAliasTargetStaleFallback(t *testing.T) { //nolint:paralleltest // Captures process stdout/stderr.
	reg := llm.NewRegistry()
	reg.Register(&providerCommandTestProvider{
		name:     "alpha",
		models:   []string{"static-model"},
		fetchErr: errors.New("models unavailable"),
	})
	require.NoError(t, reg.SetModelAlias("fast", "alpha", "static-model"))

	stdout, stderr, err := captureStdoutAndStderrForProviderCommands(t, func() error {
		return explainModelResolution(context.Background(), "fast", reg)
	})

	require.NoError(t, err)
	assert.Contains(t, stderr, "warning: alpha live model fetch failed; using static fallback: models unavailable")
	assert.Contains(t, stdout, "requested: fast")
	assert.Contains(t, stdout, "provider: alpha")
	assert.Contains(t, stdout, "provider_model: static-model")
	assert.Contains(t, stdout, "provenance: configured_alias")
	assert.Contains(t, stdout, "stale: true")
	assert.Contains(t, stdout, "alpha/static-model [configured_alias stale]")
}

func TestExplainModelResolutionRefreshesLiveCatalogs(t *testing.T) { //nolint:paralleltest // Captures process stdout/stderr.
	reg := llm.NewRegistry()
	reg.Register(&providerCommandTestProvider{
		name:          "alpha",
		models:        []string{"static-model"},
		fetchedModels: []string{"custom-live"},
	})

	stdout, stderr, err := captureStdoutAndStderrForProviderCommands(t, func() error {
		return explainModelResolution(context.Background(), "custom-live", reg)
	})

	require.NoError(t, err)
	assert.Empty(t, stderr)
	assert.Contains(t, stdout, "requested: custom-live")
	assert.Contains(t, stdout, "provider: alpha")
	assert.Contains(t, stdout, "provider_model: custom-live")
	assert.Contains(t, stdout, "provenance: fetched_live")
}

func TestExplainModelResolutionMarksStaleStaticFallback(t *testing.T) { //nolint:paralleltest // Captures process stdout/stderr.
	reg := llm.NewRegistry()
	reg.Register(&providerCommandTestProvider{
		name:     "alpha",
		models:   []string{"static-model"},
		fetchErr: errors.New("models unavailable"),
	})

	stdout, stderr, err := captureStdoutAndStderrForProviderCommands(t, func() error {
		return explainModelResolution(context.Background(), "static-model", reg)
	})

	require.NoError(t, err)
	assert.Contains(t, stderr, "warning: alpha live model fetch failed; using static fallback: models unavailable")
	assert.Contains(t, stdout, "provider: alpha")
	assert.Contains(t, stdout, "provenance: static")
	assert.Contains(t, stdout, "stale: true")
	assert.Contains(t, stdout, "alpha/static-model [static stale]")
}

func TestExplainModelResolutionReportsAmbiguousModel(t *testing.T) { //nolint:paralleltest // Captures process stdout/stderr.
	reg := llm.NewRegistry()
	reg.Register(&providerCommandTestProvider{name: "claude-code", models: []string{"shared"}})
	reg.Register(&providerCommandTestProvider{name: "codex", models: []string{"shared"}})

	stdout, _, err := captureStdoutAndStderrForProviderCommands(t, func() error {
		return explainModelResolution(context.Background(), "shared", reg)
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "ambiguous model")
	assert.Contains(t, stdout, "status: unresolved")
	assert.Contains(t, stdout, `error: llm: ambiguous model "shared"`)
	assert.Contains(t, stdout, "candidates:")
	assert.Contains(t, stdout, "claude-code/shared [static]")
	assert.Contains(t, stdout, "codex/shared [static]")
}

func TestExplainModelResolutionPrintsDefaultProviderDisambiguation(t *testing.T) { //nolint:paralleltest // Captures process stdout/stderr.
	reg := llm.NewRegistry()
	reg.Register(&providerCommandTestProvider{name: "claude-code", models: []string{"shared"}})
	reg.Register(&providerCommandTestProvider{name: "codex", models: []string{"shared"}})
	require.NoError(t, reg.SetDefault("codex"))

	stdout, stderr, err := captureStdoutAndStderrForProviderCommands(t, func() error {
		return explainModelResolution(context.Background(), "shared", reg)
	})

	require.NoError(t, err)
	assert.Empty(t, stderr)
	assert.Contains(t, stdout, "requested: shared")
	assert.Contains(t, stdout, "provider: codex")
	assert.Contains(t, stdout, "provider_model: shared")
	assert.Contains(t, stdout, "default_provider: codex (configured)")
	assert.Contains(t, stdout, `bare model is claimed by multiple providers; configured default provider "codex" selected a deterministic match`)
	assert.Contains(t, stdout, "candidates:")
	assert.Contains(t, stdout, "claude-code/shared [static]")
	assert.Contains(t, stdout, "codex/shared [static]")
	assert.NotContains(t, stdout, "status: unresolved")
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
