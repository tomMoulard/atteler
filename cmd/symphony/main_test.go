package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunValidateAutonomyFlagOverridesWorkflow(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "token")
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	require.NoError(t, os.WriteFile(path, []byte("---\nautonomy: medium\ntracker:\n  kind: github\n  repository: openai/symphony\n---\nDo the issue.\n"), 0o600))

	var runErr error

	stdout := captureStdout(t, func() {
		runErr = run(t.Context(), []string{"--validate", "--workflow", path, "--autonomy", "full"})
	})

	require.NoError(t, runErr)
	assert.Contains(t, stdout, "autonomy=full")
}

func TestRunValidateRejectsUnknownWorkflowConfigField(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	badKey := "publ" + "sih"
	require.NoError(t, os.WriteFile(path, []byte("---\ntracker:\n  kind: github\n  repository: openai/symphony\n"+badKey+":\n  enabled: true\n---\nDo the issue.\n"), 0o600))

	err := run(t.Context(), []string{"--validate", "--workflow", path})

	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown workflow config field "`+badKey+`"`)
	assert.Contains(t, err.Error(), `did you mean "publish"`)
}

func TestRunValidateRejectsUnknownNestedWorkflowConfigField(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	require.NoError(t, os.WriteFile(path, []byte("---\ntracker:\n  kind: github\n  repository: openai/symphony\npublish:\n  monitr_checks: true\n---\nDo the issue.\n"), 0o600))

	err := run(t.Context(), []string{"--validate", "--workflow", path})

	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown workflow config field "publish.monitr_checks"`)
	assert.Contains(t, err.Error(), `did you mean "publish.monitor_checks"`)
}

func TestRunRejectsInvalidAutonomyFlag(t *testing.T) { //nolint:paralleltest // flag package writes usage to process-global stderr.
	err := run(t.Context(), []string{"--validate", "--autonomy", "unsafe"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported autonomy")
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	original := os.Stdout
	reader, writer, err := os.Pipe()
	require.NoError(t, err)
	t.Cleanup(func() {
		os.Stdout = original
		_ = reader.Close()
	})

	os.Stdout = writer

	fn()

	require.NoError(t, writer.Close())

	os.Stdout = original

	data, err := io.ReadAll(reader)
	require.NoError(t, err)

	return string(data)
}
