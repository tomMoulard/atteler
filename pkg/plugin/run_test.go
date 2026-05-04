package plugin

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRunEntrypoint_CapturesOutputAndUsesPluginRoot(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "data.txt"), []byte("root-data"), 0o600); err != nil {
		require.NoError(t, err)
	}

	writeScript(t, root, "bin/run", `#!/bin/sh
set -eu
if [ ! -f data.txt ]; then
  echo "missing cwd file" >&2
  exit 11
fi
printf 'stdout:%s\n' "$(cat data.txt)"
printf 'stderr:%s\n' "$(cat data.txt)" >&2
`)

	manifest := Manifest{
		Name:        "runner",
		Version:     "1.0.0",
		Entrypoints: map[string]string{"run": "bin/run"},
	}

	result, err := RunEntrypoint(context.Background(), root, manifest, "run", 5*time.Second)
	require.NoError(t, err)
	require.Equal(t, "stdout:root-data\n", result.Stdout)
	require.Equal(t, "stderr:root-data\n", result.Stderr)
}

func TestRunEntrypoint_ReturnsExitErrorAndCapturedOutput(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeScript(t, root, "bin/fail", `#!/bin/sh
printf 'before failure\n'
printf 'problem\n' >&2
exit 7
`)

	manifest := Manifest{
		Name:        "runner",
		Version:     "1.0.0",
		Entrypoints: map[string]string{"fail": "bin/fail"},
	}

	result, err := RunEntrypoint(context.Background(), root, manifest, "fail", 5*time.Second)
	require.Error(t, err)

	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr)
	require.Equal(t, "before failure\n", result.Stdout)
	require.Equal(t, "problem\n", result.Stderr)
}

func TestRunEntrypoint_TimesOutWithCapturedOutput(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeScript(t, root, "bin/slow", `#!/bin/sh
sleep 5 >/dev/null 2>&1
`)

	manifest := Manifest{
		Name:        "runner",
		Version:     "1.0.0",
		Entrypoints: map[string]string{"slow": "bin/slow"},
	}

	result, err := RunEntrypoint(context.Background(), root, manifest, "slow", 100*time.Millisecond)
	require.Error(t, err)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Empty(t, result.Stdout)
	require.Empty(t, result.Stderr)
}

func TestRunEntrypoint_ValidatesManifestAndEntrypointName(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeScript(t, root, "bin/run", `#!/bin/sh
printf 'ok\n'
`)

	_, err := RunEntrypoint(context.Background(), root, Manifest{Version: "1.0.0", Entrypoints: map[string]string{"run": "bin/run"}}, "run", time.Second)
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing name")

	manifest := Manifest{Name: "runner", Version: "1.0.0", Entrypoints: map[string]string{"run": "bin/run"}}
	_, err = RunEntrypoint(context.Background(), root, manifest, "missing", time.Second)
	require.Error(t, err)
	require.Contains(t, err.Error(), `entrypoint "missing" not found`)
}

func TestRunEntrypoint_RejectsSymlinkEscape(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	outside := t.TempDir()
	outsideScript := writeScript(t, outside, "outside", `#!/bin/sh
printf 'escaped\n'
`)

	link := filepath.Join(root, "bin", "outside")
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		require.NoError(t, err)
	}

	if err := os.Symlink(outsideScript, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	manifest := Manifest{
		Name:        "runner",
		Version:     "1.0.0",
		Entrypoints: map[string]string{"run": "bin/outside"},
	}

	_, err := RunEntrypoint(context.Background(), root, manifest, "run", 5*time.Second)
	require.Error(t, err)
	require.Contains(t, err.Error(), "escapes plugin root")
}

func writeScript(t *testing.T, root, relativePath, content string) string {
	t.Helper()

	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}

	path := filepath.Join(root, relativePath)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		require.NoError(t, err)
	}

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		require.NoError(t, err)
	}
	//nolint:gosec // Test helper creates intentionally executable shell scripts.
	if err := os.Chmod(path, 0o700); err != nil {
		require.NoError(t, err)
	}

	return path
}
