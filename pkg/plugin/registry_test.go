package plugin

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/permission"
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

func TestRegistry_DryRunEntrypointWithOptionsChecksPolicyWithoutExecuting(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	marker := filepath.Join(root, "executed")
	writeManifest(t, root, "plugin.yaml", `
name: dry-policy
version: "1.0.0"
min_atteler_version: "0.1.0"
entrypoints:
  run: bin/run
entrypoint_args:
  run: []
entrypoint_contracts:
  run:
    output:
      format: text
permissions:
  filesystem:
    read: ["."]
    write: ["tmp"]
  network:
    allow: false
    hosts: []
  shell:
    allow: true
  env: []
  secrets: []
  tools: []
output:
  stdout_max_bytes: 4096
  stderr_max_bytes: 4096
trust:
  enabled: true
  install_source: test
  checksum: sha256:test
  audit:
    - action: accepted
`)
	writeScript(t, root, "bin/run", `#!/bin/sh
touch executed
`)

	registry, err := NewRegistry([]string{root})
	require.NoError(t, err)

	plugin, ok := registry.Get("dry-policy")
	require.True(t, ok)

	_, err = registry.DryRunEntrypointWithOptions(t.Context(), "dry-policy", "run", DryRunOptions{
		RequireAcceptedPolicy: true,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "accepted policy must be provided")
	require.NoFileExists(t, marker)

	policy := AcceptManifestPolicy(plugin.Manifest)
	policy.Permissions.Filesystem.Write = nil

	_, err = registry.DryRunEntrypointWithOptions(t.Context(), "dry-policy", "run", DryRunOptions{
		Policy: &policy,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), `filesystem.write scope "tmp" was not accepted`)
	require.NoFileExists(t, marker)

	policy = AcceptManifestPolicy(plugin.Manifest)
	dryRun, err := registry.DryRunEntrypointWithOptions(t.Context(), "dry-policy", "run", DryRunOptions{
		Policy: &policy,
	})
	require.NoError(t, err)
	require.True(t, dryRun.PolicyChecked)
	require.Contains(t, dryRun.Description, "policy_checked=true")
	require.NoFileExists(t, marker)
}

func TestRegistry_DryRunEntrypointWithOptionsRejectsUndeclaredShellEntrypoint(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	marker := filepath.Join(root, "executed")
	writeManifest(t, root, "plugin.yaml", `
name: dry-shell
version: "1.0.0"
min_atteler_version: "0.1.0"
entrypoints:
  run: bin/run
entrypoint_args:
  run: []
entrypoint_contracts:
  run:
    output:
      format: text
permissions:
  filesystem:
    read: ["."]
    write: []
  network:
    allow: false
    hosts: []
  shell:
    allow: false
  env: []
  secrets: []
  tools: []
output:
  stdout_max_bytes: 4096
  stderr_max_bytes: 4096
trust:
  enabled: true
  install_source: test
  checksum: sha256:test
  audit:
    - action: accepted
`)
	writeScript(t, root, "bin/run", `#!/bin/sh
touch executed
`)

	registry, err := NewRegistry([]string{root})
	require.NoError(t, err)

	plugin, ok := registry.Get("dry-shell")
	require.True(t, ok)

	accepted := AcceptManifestPolicy(plugin.Manifest)
	_, err = registry.DryRunEntrypointWithOptions(t.Context(), "dry-shell", "run", DryRunOptions{
		Policy: &accepted,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "shell access must be declared in permissions")
	require.NoFileExists(t, marker)
}

func TestRegistry_DryRunEntrypointReturnsContractCopy(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeManifest(t, root, "plugin.yaml", `
name: dry-contract
version: "1.0.0"
entrypoints:
  run: bin/run
entrypoint_contracts:
  run:
    inputs:
      args:
        - name: mode
          required: true
    output:
      format: json
      schema:
        type: object
        required: [summary]
        properties:
          summary:
            type: string
`)
	writeScript(t, root, "bin/run", `#!/bin/sh
printf '{"summary":"not run"}\n'
`)

	registry, err := NewRegistry([]string{root})
	require.NoError(t, err)

	dryRun, err := registry.DryRunEntrypoint("dry-contract", "run")
	require.NoError(t, err)
	require.NotNil(t, dryRun.Contract)
	require.NotNil(t, dryRun.Contract.Output)
	require.NotNil(t, dryRun.Contract.Output.Schema)

	dryRun.Contract.Inputs.Args[0].Name = "changed"
	dryRun.Contract.Output.Schema.Required[0] = "changed"
	dryRun.Contract.Output.Schema.Properties["summary"] = JSONSchemaProperty{Type: "boolean"}

	plugin, ok := registry.Get("dry-contract")
	require.True(t, ok)

	contract := plugin.Manifest.EntrypointContracts["run"]
	require.Equal(t, "mode", contract.Inputs.Args[0].Name)
	require.Equal(t, "summary", contract.Output.Schema.Required[0])
	require.Equal(t, "string", contract.Output.Schema.Properties["summary"].Type)
}

func TestRegistry_DryRunEntrypointWithOptionsChecksCentralPermissionPolicy(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	marker := filepath.Join(root, "executed")
	writeManifest(t, root, "plugin.yaml", `
name: dry-permission
version: "1.0.0"
min_atteler_version: "0.1.0"
entrypoints:
  run: bin/run
entrypoint_args:
  run: []
entrypoint_contracts:
  run:
    output:
      format: text
permissions:
  filesystem:
    read: ["."]
    write: []
  network:
    allow: true
    hosts: [api.example.com]
  shell:
    allow: true
  env: []
  secrets: []
  tools: []
output:
  stdout_max_bytes: 4096
  stderr_max_bytes: 4096
trust:
  enabled: true
  install_source: test
  checksum: sha256:test
  audit:
    - action: accepted
`)
	writeScript(t, root, "bin/run", `#!/bin/sh
touch executed
`)

	registry, err := NewRegistry([]string{root})
	require.NoError(t, err)

	plugin, ok := registry.Get("dry-permission")
	require.True(t, ok)

	accepted := AcceptManifestPolicy(plugin.Manifest)
	permissionPolicy := permission.DefaultPolicy()
	permissionPolicy.SetMode(permission.OperationNetwork, permission.ModeDeny)
	ctx := permission.ContextWithPolicy(context.Background(), &permissionPolicy)

	_, err = registry.DryRunEntrypointWithOptions(ctx, "dry-permission", "run", DryRunOptions{
		Policy: &accepted,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "permission.network.deny")
	require.True(t, permission.ErrDenied(err))
	require.NoFileExists(t, marker)

	permissionPolicy.SetMode(permission.OperationNetwork, permission.ModeAllow)

	dryRun, err := registry.DryRunEntrypointWithOptions(ctx, "dry-permission", "run", DryRunOptions{
		Policy: &accepted,
	})
	require.NoError(t, err)
	require.True(t, dryRun.PolicyChecked)
	require.NoFileExists(t, marker)
}

func TestRegistry_LockfileRecordsInstalledPluginVersions(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeManifest(t, root, "plugin.yaml", `
name: locked
version: "1.2.3"
min_atteler_version: "1.0.0"
capabilities:
  - review
entrypoints:
  run: bin/run
trust:
  enabled: true
  install_source: test-registry
  checksum: sha256:abc123
  signature: sig:test
provenance:
  source: registry
  repository: gh:tomMoulard/locked
  ref: v1.2.3
  commit: abcdef1234567890
  digest: sha256:def456
  signature: sig:provenance
  installed_at: "2026-05-30T12:00:00Z"
  installed_by: atteler-test
`)
	writeScript(t, root, "bin/run", `#!/bin/sh
printf 'not run\n'
`)

	registry, err := NewRegistry([]string{root})
	require.NoError(t, err)

	lock := registry.Lockfile()
	require.Equal(t, LockfileSchemaVersion, lock.Version)
	require.Len(t, lock.Plugins, 1)
	require.Equal(t, "locked", lock.Plugins[0].Name)
	require.Equal(t, "1.2.3", lock.Plugins[0].Version)
	require.Equal(t, "sha256:abc123", lock.Plugins[0].Checksum)
	require.Equal(t, "registry", lock.Plugins[0].ProvenanceSource)
	require.Equal(t, "gh:tomMoulard/locked", lock.Plugins[0].ProvenanceRepository)
	require.Equal(t, "v1.2.3", lock.Plugins[0].ProvenanceRef)
	require.Equal(t, "abcdef1234567890", lock.Plugins[0].ProvenanceCommit)
	require.Equal(t, "2026-05-30T12:00:00Z", lock.Plugins[0].ProvenanceInstalledAt)
	require.Equal(t, "atteler-test", lock.Plugins[0].ProvenanceInstalledBy)
	require.NoError(t, lock.ValidateRegistry(registry))

	path := filepath.Join(t.TempDir(), "plugins.lock.yaml")
	require.NoError(t, SaveLockfile(path, lock))
	loaded, err := LoadLockfile(path)
	require.NoError(t, err)
	require.Equal(t, lock, loaded)
	require.NoError(t, loaded.ValidateRegistry(registry))

	tampered := lock
	tampered.Plugins = append([]LockedPlugin(nil), lock.Plugins...)
	tampered.Plugins[0].ProvenanceDigest = "sha256:changed"
	err = tampered.ValidateRegistry(registry)
	require.Error(t, err)
	require.Contains(t, err.Error(), "provenance digest does not match lockfile")

	tampered = lock
	tampered.Plugins = append([]LockedPlugin(nil), lock.Plugins...)
	tampered.Plugins[0].ProvenanceInstalledBy = "someone-else"
	err = tampered.ValidateRegistry(registry)
	require.Error(t, err)
	require.Contains(t, err.Error(), "provenance installed-by does not match lockfile")
}

func TestLockfileValidateRejectsMalformedInstalledMetadata(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*LockedPlugin)
		want   string
	}{
		{
			name: "missing root",
			mutate: func(plugin *LockedPlugin) {
				plugin.Root = ""
			},
			want: `plugin "locked" missing root`,
		},
		{
			name: "missing manifest path",
			mutate: func(plugin *LockedPlugin) {
				plugin.ManifestPath = ""
			},
			want: `plugin "locked" missing manifest path`,
		},
		{
			name: "duplicate capability",
			mutate: func(plugin *LockedPlugin) {
				plugin.Capabilities = []string{"review", "review"}
			},
			want: `plugin locked capabilities duplicate value "review"`,
		},
		{
			name: "control character in provenance",
			mutate: func(plugin *LockedPlugin) {
				plugin.ProvenanceRepository = "gh:owner/plugin\nextra"
			},
			want: `plugin "locked" provenance_repository contains control characters`,
		},
		{
			name: "control character in installed by provenance",
			mutate: func(plugin *LockedPlugin) {
				plugin.ProvenanceInstalledBy = "atteler\noperator"
			},
			want: `plugin "locked" provenance_installed_by contains control characters`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			locked := LockedPlugin{
				Name:         "locked",
				Version:      "1.2.3",
				Root:         "/plugins/locked",
				ManifestPath: "/plugins/locked/plugin.yaml",
				Capabilities: []string{"review"},
			}
			tt.mutate(&locked)

			err := (Lockfile{
				Version: LockfileSchemaVersion,
				Plugins: []LockedPlugin{
					locked,
				},
			}).Validate()
			require.Error(t, err)
			require.Contains(t, err.Error(), tt.want)
		})
	}
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
