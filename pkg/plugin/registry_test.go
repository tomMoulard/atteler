package plugin

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRegistry_LoadsConfiguredPathsAndLooksUpByName(t *testing.T) {
	t.Parallel()
	alphaRoot := t.TempDir()
	alphaManifest := writeManifest(t, alphaRoot, "plugin.yaml", `
name: alpha
version: "1.0.0"
description: Alpha plugin
capabilities:
  - review
entrypoints:
  run: bin/run
`)
	writeScript(t, alphaRoot, "bin/run", `#!/bin/sh
printf 'do not execute from registry tests\n'
`)

	betaRoot := t.TempDir()
	betaManifest := writeManifest(t, betaRoot, "custom.json", `{
  "name": "beta",
  "version": "2.0.0",
  "entrypoints": {"check": "bin/check"}
}`)
	writeScript(t, betaRoot, "bin/check", `#!/bin/sh
printf 'do not execute from registry tests\n'
`)

	registry, err := NewRegistry([]string{alphaRoot, betaManifest})
	require.NoError(t, err)
	require.Equal(t, []string{"alpha", "beta"}, registry.List())

	alpha, ok := registry.Get(" alpha ")
	require.True(t, ok)
	require.Equal(t, "alpha", alpha.Manifest.Name)
	require.Equal(t, absPath(t, alphaRoot), alpha.Root)
	require.Equal(t, absPath(t, alphaManifest), alpha.ManifestPath)

	beta, ok := registry.Get("beta")
	require.True(t, ok)
	require.Equal(t, "beta", beta.Manifest.Name)
	require.Equal(t, absPath(t, betaRoot), beta.Root)
	require.Equal(t, absPath(t, betaManifest), beta.ManifestPath)
}

func TestRegistry_RejectsDuplicatePluginNames(t *testing.T) {
	t.Parallel()
	first := t.TempDir()
	second := t.TempDir()
	writeManifest(t, first, "plugin.yaml", `name: duplicate
version: "1"
`)
	writeManifest(t, second, "plugin.yaml", `name: duplicate
version: "2"
`)

	_, err := NewRegistry([]string{first, second})
	require.Error(t, err)
	require.Contains(t, err.Error(), `duplicate plugin name "duplicate"`)
	require.Contains(t, err.Error(), filepath.Join(first, "plugin.yaml"))
	require.Contains(t, err.Error(), filepath.Join(second, "plugin.yaml"))
}

func TestRegistry_ResolveEntrypoint(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	manifestPath := writeManifest(t, root, "plugin.yml", `
name: runner
version: "1.0.0"
entrypoints:
  run: bin/run
`)
	scriptPath := writeScript(t, root, "bin/run", `#!/bin/sh
printf 'not run by resolve\n'
`)

	registry, err := NewRegistry([]string{root})
	require.NoError(t, err)

	entrypoint, err := registry.ResolveEntrypoint("runner", " run ")
	require.NoError(t, err)
	require.Equal(t, "runner", entrypoint.PluginName)
	require.Equal(t, "run", entrypoint.EntrypointName)
	require.Equal(t, absPath(t, root), entrypoint.Root)
	require.Equal(t, absPath(t, manifestPath), entrypoint.ManifestPath)
	require.Equal(t, "bin/run", entrypoint.RelativePath)
	require.Equal(t, evalAbsPath(t, scriptPath), entrypoint.Path)
}

func TestRegistry_DryRunEntrypointDescribesWithoutExecuting(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	marker := filepath.Join(root, "executed")
	writeManifest(t, root, "plugin.yaml", `
name: dry-runner
version: "1.0.0"
entrypoints:
  run: bin/run
`)
	writeScript(t, root, "bin/run", `#!/bin/sh
touch executed
`)

	registry, err := NewRegistry([]string{root})
	require.NoError(t, err)

	dryRun, err := registry.DryRunEntrypoint("dry-runner", "run")
	require.NoError(t, err)
	require.Contains(t, dryRun.Description, `would run plugin "dry-runner" entrypoint "run"`)
	require.Contains(t, dryRun.Description, dryRun.Entrypoint.Path)
	require.Contains(t, dryRun.Description, dryRun.Entrypoint.Root)

	_, err = os.Stat(marker)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestRegistry_ResolveEntrypointErrors(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeManifest(t, root, "plugin.yaml", `
name: runner
version: "1.0.0"
entrypoints:
  run: bin/run
`)
	writeScript(t, root, "bin/run", `#!/bin/sh
printf 'not run\n'
`)
	registry, err := NewRegistry([]string{root})
	require.NoError(t, err)

	_, err = registry.ResolveEntrypoint("missing", "run")
	require.Error(t, err)
	require.Contains(t, err.Error(), `plugin: "missing" not found`)

	_, err = registry.ResolveEntrypoint("runner", "missing")
	require.Error(t, err)
	require.Contains(t, err.Error(), `entrypoint "missing" not found`)

	_, err = registry.ResolveEntrypoint("runner", " ")
	require.Error(t, err)
	require.Contains(t, err.Error(), "empty entrypoint name")
}

func TestRegistry_ResolveEntrypointRejectsSymlinkEscape(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	outside := t.TempDir()
	outsideScript := writeScript(t, outside, "outside", `#!/bin/sh
printf 'escaped\n'
`)
	writeManifest(t, root, "plugin.yaml", `
name: runner
version: "1.0.0"
entrypoints:
  run: bin/outside
`)
	link := filepath.Join(root, "bin", "outside")
	require.NoError(t, os.MkdirAll(filepath.Dir(link), 0o700))

	if err := os.Symlink(outsideScript, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	registry, err := NewRegistry([]string{root})
	require.NoError(t, err)

	_, err = registry.ResolveEntrypoint("runner", "run")
	require.Error(t, err)
	require.Contains(t, err.Error(), "escapes plugin root")
}

func TestRegistry_NilRegistryIsSafeForLookup(t *testing.T) {
	t.Parallel()

	var registry *Registry

	require.Nil(t, registry.List())
	_, ok := registry.Get("anything")
	require.False(t, ok)

	_, err := registry.ResolveEntrypoint("anything", "run")
	require.Error(t, err)
	require.Contains(t, err.Error(), `plugin: "anything" not found`)
}

func absPath(t *testing.T, path string) string {
	t.Helper()

	abs, err := filepath.Abs(path)
	require.NoError(t, err)

	return abs
}

func evalAbsPath(t *testing.T, path string) string {
	t.Helper()
	abs := absPath(t, path)
	evaluated, err := filepath.EvalSymlinks(abs)
	require.NoError(t, err)

	return evaluated
}
