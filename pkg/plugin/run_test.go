package plugin

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/permission"
	attshell "github.com/tommoulard/atteler/pkg/shell"
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
printf 'stdout:%s\n' "$(/bin/cat data.txt)"
printf 'stderr:%s\n' "$(/bin/cat data.txt)" >&2
`)

	manifest := runnableManifest(map[string]string{"run": "bin/run"})

	result, err := runEntrypointForTest(t, root, manifest, "run", 5*time.Second)
	require.NoError(t, err)
	require.Equal(t, "stdout:root-data\n", result.Stdout)
	require.Equal(t, "stderr:root-data\n", result.Stderr)
}

func TestRunEntrypoint_PropagatesAutonomyToEnvAndAudit(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeScript(t, root, "bin/run", `#!/bin/sh
printf 'autonomy=%s\n' "$ATTELER_AUTONOMY"
`)

	manifest := runnableManifest(map[string]string{"run": "bin/run"})
	policy := AcceptManifestPolicy(manifest)
	auditDir := filepath.Join(t.TempDir(), "audit")

	result, err := RunEntrypointWithOptions(
		context.Background(),
		root,
		manifest,
		"run",
		RunOptions{
			Policy:   &policy,
			Timeout:  5 * time.Second,
			Autonomy: "full",
			AuditDir: auditDir,
		},
	)
	require.NoError(t, err)
	require.Equal(t, "autonomy=full\n", result.Stdout)

	records := readPluginAuditRecords(t, auditDir)
	require.NotEmpty(t, records)

	for _, record := range records {
		require.Equal(t, "full", record.Autonomy)
	}
}

func TestRunEntrypoint_AutonomyOverridesDeclaredEnv(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeScript(t, root, "bin/run", `#!/bin/sh
printf 'autonomy=%s\n' "$ATTELER_AUTONOMY"
`)

	manifest := runnableManifest(map[string]string{"run": "bin/run"})
	manifest.Permissions.Env = []string{"ATTELER_AUTONOMY"}
	policy := AcceptManifestPolicy(manifest)
	auditDir := filepath.Join(t.TempDir(), "audit")

	result, err := RunEntrypointWithOptions(
		context.Background(),
		root,
		manifest,
		"run",
		RunOptions{
			Policy:   &policy,
			Timeout:  5 * time.Second,
			Env:      map[string]string{"ATTELER_AUTONOMY": "spoofed-low"},
			Autonomy: "high",
			AuditDir: auditDir,
		},
	)
	require.NoError(t, err)
	require.Equal(t, "autonomy=high\n", result.Stdout)

	records := readPluginAuditRecords(t, auditDir)
	require.NotEmpty(t, records)

	for _, record := range records {
		require.Equal(t, "high", record.Autonomy)
	}
}

func TestRunEntrypoint_ReturnsExitErrorAndCapturedOutput(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeScript(t, root, "bin/fail", `#!/bin/sh
printf 'before failure\n'
printf 'problem\n' >&2
exit 7
`)

	manifest := runnableManifest(map[string]string{"fail": "bin/fail"})

	result, err := runEntrypointForTest(t, root, manifest, "fail", 5*time.Second)
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
/bin/sleep 5 >/dev/null 2>&1
`)

	manifest := runnableManifest(map[string]string{"slow": "bin/slow"})

	result, err := runEntrypointForTest(t, root, manifest, "slow", 100*time.Millisecond)
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

	_, err := runEntrypointForTest(t, root, Manifest{Version: "1.0.0", Entrypoints: map[string]string{"run": "bin/run"}}, "run", 5*time.Second)
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing name")

	manifest := runnableManifest(map[string]string{"run": "bin/run"})
	_, err = runEntrypointForTest(t, root, manifest, "missing", 5*time.Second)
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

	manifest := runnableManifest(map[string]string{"run": "bin/outside"})

	_, err := runEntrypointForTest(t, root, manifest, "run", 5*time.Second)
	require.Error(t, err)
	require.Contains(t, err.Error(), "escapes plugin root")
}

func TestRunEntrypoint_RejectsMissingRuntimeDeclarationsBeforeExecution(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	marker := filepath.Join(root, "executed")
	writeScript(t, root, "bin/run", `#!/bin/sh
printf executed > executed
`)

	manifest := Manifest{
		Name:        "runner",
		Version:     "1.0.0",
		Entrypoints: map[string]string{"run": "bin/run"},
	}

	_, err := runEntrypointForTest(t, root, manifest, "run", 5*time.Second)
	require.Error(t, err)
	require.Contains(t, err.Error(), "permissions must be declared")
	require.NoFileExists(t, marker)
}

func TestRunEntrypoint_RequiresExplicitAcceptedPolicy(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	marker := filepath.Join(root, "executed")
	writeScript(t, root, "bin/run", `#!/bin/sh
printf executed > executed
`)

	manifest := runnableManifest(map[string]string{"run": "bin/run"})

	_, err := RunEntrypoint(context.Background(), root, manifest, "run", 5*time.Second)
	require.Error(t, err)
	require.Contains(t, err.Error(), "accepted policy must be provided")
	require.NoFileExists(t, marker)
}

func TestRunEntrypoint_RequiresCompatibilityDeclarationBeforeExecution(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	marker := filepath.Join(root, "executed")
	writeScript(t, root, "bin/run", `#!/bin/sh
printf executed > executed
`)

	manifest := runnableManifest(map[string]string{"run": "bin/run"})
	manifest.MinimumAttelerVersion = ""

	_, err := runEntrypointForTest(t, root, manifest, "run", 5*time.Second)
	require.Error(t, err)
	require.Contains(t, err.Error(), "min_atteler_version must be declared")
	require.NoFileExists(t, marker)
}

func TestRunEntrypoint_RequiresOutputContractBeforeExecution(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	marker := filepath.Join(root, "executed")
	writeScript(t, root, "bin/run", `#!/bin/sh
printf executed > executed
`)

	manifest := runnableManifest(map[string]string{"run": "bin/run"})
	manifest.EntrypointContracts = nil

	_, err := runEntrypointForTest(t, root, manifest, "run", 5*time.Second)
	require.Error(t, err)
	require.Contains(t, err.Error(), `entrypoint "run" output contract must be declared`)
	require.NoFileExists(t, marker)
}

func TestRunEntrypoint_CentralPermissionPolicyDeniesExecution(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	marker := filepath.Join(root, "executed")
	writeScript(t, root, "bin/run", `#!/bin/sh
printf executed > executed
`)

	manifest := runnableManifest(map[string]string{"run": "bin/run"})
	acceptedPolicy := AcceptManifestPolicy(manifest)
	permissionPolicy := permission.DefaultPolicy()
	permissionPolicy.SetMode(permission.OperationExecute, permission.ModeDeny)

	_, err := RunEntrypointWithOptions(
		context.Background(),
		root,
		manifest,
		"run",
		RunOptions{
			Policy:     &acceptedPolicy,
			Permission: &permissionPolicy,
			Timeout:    5 * time.Second,
		},
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "permission.execute.deny")
	require.True(t, permission.ErrDenied(err))
	require.NoFileExists(t, marker)
}

func TestRunEntrypoint_CentralPermissionPolicyDeniesDeclaredNetwork(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	marker := filepath.Join(root, "executed")
	writeScript(t, root, "bin/run", `#!/bin/sh
printf executed > executed
`)

	manifest := runnableManifest(map[string]string{"run": "bin/run"})
	manifest.Permissions.Network = NetworkPermissions{Allow: true, Hosts: []string{"api.example.com"}}
	acceptedPolicy := AcceptManifestPolicy(manifest)
	permissionPolicy := permission.DefaultPolicy()
	permissionPolicy.SetMode(permission.OperationNetwork, permission.ModeDeny)
	ctx := permission.ContextWithPolicy(context.Background(), &permissionPolicy)

	_, err := RunEntrypointWithOptions(
		ctx,
		root,
		manifest,
		"run",
		RunOptions{
			Policy:  &acceptedPolicy,
			Timeout: 5 * time.Second,
		},
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "permission.network.deny")
	require.Contains(t, err.Error(), "plugin entrypoint runner/run")

	var permissionErr *permission.Error
	require.ErrorAs(t, err, &permissionErr)
	requirePermissionOperation(t, permissionErr.Decision.Operations, permission.OperationNetwork, permission.Operation{
		Metadata: map[string]string{
			"plugin":     "runner",
			"entrypoint": "run",
		},
		Kind:   permission.OperationNetwork,
		Action: "plugin entrypoint runner/run",
		Target: "api.example.com",
		Source: "atteler.plugin.runner.run",
	})
	require.NoFileExists(t, marker)
}

func TestRunEntrypoint_ReadOnlyPolicyDeniesPluginExecution(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	marker := filepath.Join(root, "executed")
	writeScript(t, root, "bin/printf", `#!/bin/sh
printf executed > executed
`)

	manifest := runnableManifest(map[string]string{"run": "bin/printf"})
	acceptedPolicy := AcceptManifestPolicy(manifest)
	permissionPolicy := permission.ReadOnlyPolicy()

	_, err := RunEntrypointWithOptions(
		context.Background(),
		root,
		manifest,
		"run",
		RunOptions{
			Policy:     &acceptedPolicy,
			Permission: &permissionPolicy,
			Timeout:    5 * time.Second,
		},
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "permission.execute.deny")
	require.True(t, permission.ErrDenied(err))
	require.NoFileExists(t, marker)
}

func TestRunEntrypoint_RequiresActiveContext(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	marker := filepath.Join(root, "executed")
	writeScript(t, root, "bin/run", `#!/bin/sh
printf executed > executed
`)

	manifest := runnableManifest(map[string]string{"run": "bin/run"})
	policy := AcceptManifestPolicy(manifest)

	_, err := RunEntrypointWithOptions(nil, root, manifest, "run", RunOptions{Timeout: 5 * time.Second, Policy: &policy}) //nolint:staticcheck // Verify nil contexts are rejected instead of panicking.
	require.Error(t, err)
	require.Contains(t, err.Error(), "context is required")
	require.NoFileExists(t, marker)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = RunEntrypointWithOptions(ctx, root, manifest, "run", RunOptions{Timeout: 5 * time.Second, Policy: &policy})
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
	require.NoFileExists(t, marker)
}

func TestRunEntrypoint_RejectsIncompatibleAttelerVersion(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	marker := filepath.Join(root, "executed")
	writeScript(t, root, "bin/run", `#!/bin/sh
printf executed > executed
`)

	manifest := runnableManifest(map[string]string{"run": "bin/run"})
	manifest.MinimumAttelerVersion = "9.0.0"
	acceptedPolicy := AcceptManifestPolicy(manifest)

	_, err := RunEntrypointWithOptions(
		context.Background(),
		root,
		manifest,
		"run",
		RunOptions{
			Policy:         &acceptedPolicy,
			Timeout:        5 * time.Second,
			AttelerVersion: "1.2.3",
		},
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "requires atteler >= 9.0.0")
	require.NoFileExists(t, marker)
}

func TestRunEntrypoint_RejectsEntrypointOutsideDeclaredFilesystemRead(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeScript(t, root, "bin/run", `#!/bin/sh
printf 'should not run\n'
`)

	manifest := runnableManifest(map[string]string{"run": "bin/run"})
	manifest.Permissions.Filesystem.Read = []string{"docs"}

	_, err := runEntrypointForTest(t, root, manifest, "run", 5*time.Second)
	require.Error(t, err)
	require.Contains(t, err.Error(), "filesystem.read does not include entrypoint")
}

func TestRunEntrypoint_RejectsUnacceptedNetworkPermission(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeScript(t, root, "bin/run", `#!/bin/sh
printf 'should not run\n'
`)

	manifest := runnableManifest(map[string]string{"run": "bin/run"})
	manifest.Permissions.Network = NetworkPermissions{Allow: true, Hosts: []string{"api.example.com"}}
	policy := AcceptManifestPolicy(manifest)
	policy.Permissions.Network = NetworkPermissions{}

	_, err := RunEntrypointWithOptions(
		context.Background(),
		root,
		manifest,
		"run",
		RunOptions{Timeout: 5 * time.Second, Policy: &policy},
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "network access was not accepted")
}

func TestRunEntrypoint_RejectsManifestRequestsBeyondPolicy(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeScript(t, root, "bin/run", `#!/bin/sh
printf 'should not run\n'
`)

	tests := []struct {
		name   string
		mutate func(*Manifest, *Policy)
		want   string
	}{
		{
			name: "filesystem write",
			mutate: func(manifest *Manifest, policy *Policy) {
				manifest.Permissions.Filesystem.Write = []string{"tmp"}
				policy.Permissions.Filesystem.Write = nil
			},
			want: `filesystem.write scope "tmp" was not accepted`,
		},
		{
			name: "environment variable",
			mutate: func(manifest *Manifest, policy *Policy) {
				manifest.Permissions.Env = []string{"PLUGIN_MODE"}
				policy.Permissions.Env = nil
			},
			want: "environment variables exceed accepted policy",
		},
		{
			name: "secret variable",
			mutate: func(manifest *Manifest, policy *Policy) {
				manifest.Permissions.Secrets = []string{"PLUGIN_TOKEN"}
				policy.Permissions.Secrets = nil
			},
			want: "secret variables exceed accepted policy",
		},
		{
			name: "tool capability",
			mutate: func(manifest *Manifest, policy *Policy) {
				manifest.Permissions.Tools = []string{"git"}
				policy.Permissions.Tools = nil
			},
			want: "tool capabilities exceed accepted policy",
		},
		{
			name: "output limit",
			mutate: func(manifest *Manifest, policy *Policy) {
				manifest.Output.StdoutMaxBytes = 2048
				policy.Output.StdoutMaxBytes = 1024
			},
			want: "stdout_max_bytes 2048 exceeds accepted 1024",
		},
		{
			name: "missing install source policy",
			mutate: func(_ *Manifest, policy *Policy) {
				policy.TrustedInstallSources = nil
			},
			want: "trusted_install_sources must include install_source",
		},
		{
			name: "untrusted install source",
			mutate: func(_ *Manifest, policy *Policy) {
				policy.TrustedInstallSources = []string{"trusted-source"}
			},
			want: `install_source "test" is not trusted by policy`,
		},
		{
			name: "signature",
			mutate: func(_ *Manifest, policy *Policy) {
				policy.RequireSignature = true
			},
			want: "signature is required by policy",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			manifest := runnableManifest(map[string]string{"run": "bin/run"})
			policy := AcceptManifestPolicy(manifest)
			tt.mutate(&manifest, &policy)

			_, err := RunEntrypointWithOptions(
				context.Background(),
				root,
				manifest,
				"run",
				RunOptions{Timeout: 5 * time.Second, Policy: &policy},
			)
			require.Error(t, err)
			require.Contains(t, err.Error(), tt.want)
		})
	}
}

func TestAcceptManifestPolicy_DoesNotAliasManifestSlices(t *testing.T) {
	t.Parallel()

	manifest := runnableManifest(map[string]string{"run": "bin/run"})
	manifest.Permissions.Filesystem.Write = []string{"tmp"}
	manifest.Permissions.Network = NetworkPermissions{Allow: true, Hosts: []string{"api.example.com"}}
	manifest.Permissions.Env = []string{"PLUGIN_MODE"}
	manifest.Permissions.Secrets = []string{"PLUGIN_TOKEN"}
	manifest.Permissions.Tools = []string{"git"}

	policy := AcceptManifestPolicy(manifest)

	manifest.Permissions.Filesystem.Read[0] = "docs"
	manifest.Permissions.Filesystem.Write[0] = "other"
	manifest.Permissions.Network.Hosts[0] = "other.example.com"
	manifest.Permissions.Env[0] = "OTHER_MODE"
	manifest.Permissions.Secrets[0] = "OTHER_TOKEN"
	manifest.Permissions.Tools[0] = "other-tool"

	require.Equal(t, []string{"."}, policy.Permissions.Filesystem.Read)
	require.Equal(t, []string{"tmp"}, policy.Permissions.Filesystem.Write)
	require.Equal(t, []string{"api.example.com"}, policy.Permissions.Network.Hosts)
	require.Equal(t, []string{"PLUGIN_MODE"}, policy.Permissions.Env)
	require.Equal(t, []string{"PLUGIN_TOKEN"}, policy.Permissions.Secrets)
	require.Equal(t, []string{"git"}, policy.Permissions.Tools)
	require.Equal(t, []string{"test"}, policy.TrustedInstallSources)
}

func TestRunEntrypoint_RejectsShellEntrypointWithoutShellPermission(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	marker := filepath.Join(root, "executed")
	writeScript(t, root, "bin/run", `#!/bin/sh
printf executed > executed
`)

	manifest := runnableManifest(map[string]string{"run": "bin/run"})
	manifest.Permissions.Shell.Allow = false

	_, err := runEntrypointForTest(t, root, manifest, "run", 5*time.Second)
	require.Error(t, err)
	require.Contains(t, err.Error(), "shell access must be declared")
	require.NoFileExists(t, marker)
}

func TestRunEntrypoint_RejectsDisabledOrRevokedTrust(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeScript(t, root, "bin/run", `#!/bin/sh
printf 'should not run\n'
`)

	tests := []struct {
		name   string
		mutate func(*Manifest)
		want   string
	}{
		{
			name: "disabled",
			mutate: func(manifest *Manifest) {
				manifest.Trust.Enabled = false
			},
			want: "plugin is disabled",
		},
		{
			name: "revoked",
			mutate: func(manifest *Manifest) {
				manifest.Trust.Revoked = true
			},
			want: "plugin trust is revoked",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			manifest := runnableManifest(map[string]string{"run": "bin/run"})
			tt.mutate(&manifest)

			_, err := runEntrypointForTest(t, root, manifest, "run", 5*time.Second)
			require.Error(t, err)
			require.Contains(t, err.Error(), tt.want)
		})
	}
}

func TestRunEntrypoint_ScrubsAmbientEnvAndRejectsUndeclaredEnv(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ATTELER_PLUGIN_LEAK", "ambient-secret")
	writeScript(t, root, "bin/run", `#!/bin/sh
if [ -n "${ATTELER_PLUGIN_LEAK:-}" ]; then
  printf 'leaked\n'
  exit 9
fi
printf 'scrubbed\n'
`)

	manifest := runnableManifest(map[string]string{"run": "bin/run"})

	result, err := runEntrypointForTest(t, root, manifest, "run", 5*time.Second)
	require.NoError(t, err)
	require.Equal(t, "scrubbed\n", result.Stdout)

	policy := AcceptManifestPolicy(manifest)
	_, err = RunEntrypointWithOptions(
		context.Background(),
		root,
		manifest,
		"run",
		RunOptions{
			Policy:  &policy,
			Timeout: 5 * time.Second,
			Env:     map[string]string{"ATTELER_PLUGIN_LEAK": "explicit-secret"},
		},
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), `env "ATTELER_PLUGIN_LEAK" was not declared`)
}

func TestRunEntrypoint_RedactsSecretsAndBoundsOutput(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeScript(t, root, "bin/run", `#!/bin/sh
printf 'token=%s\n' "$PLUGIN_TOKEN"
printf 'abcdefghijklmnopqrstuvwxyz\n'
printf 'stderr token=%s\n' "$PLUGIN_TOKEN" >&2
printf 'ABCDEFGHIJKLMNOPQRSTUVWXYZ\n' >&2
`)

	manifest := runnableManifest(map[string]string{"run": "bin/run"})
	manifest.Permissions.Secrets = []string{"PLUGIN_TOKEN"}
	manifest.Output.StdoutMaxBytes = 24
	manifest.Output.StderrMaxBytes = 31

	policy := AcceptManifestPolicy(manifest)
	result, err := RunEntrypointWithOptions(
		context.Background(),
		root,
		manifest,
		"run",
		RunOptions{
			Policy:  &policy,
			Timeout: 5 * time.Second,
			Env:     map[string]string{"PLUGIN_TOKEN": "super-secret-token"},
		},
	)
	require.NoError(t, err)
	require.NotContains(t, result.Stdout, "super-secret-token")
	require.Contains(t, result.Stdout, "[REDACTED:PLUGIN_TOKEN]")
	require.Contains(t, result.Stdout, "output truncated")
	require.NotContains(t, result.Stderr, "super-secret-token")
	require.Contains(t, result.Stderr, "[REDACTED:PLUGIN_TOKEN]")
	require.Contains(t, result.Stderr, "output truncated")
}

func TestRunEntrypoint_AdaptsStructuredJSONOutput(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeScript(t, root, "bin/run", `#!/bin/sh
printf '{"summary":"ok","passed":true}\n'
`)

	manifest := runnableManifest(map[string]string{"run": "bin/run"})
	manifest.EntrypointContracts = map[string]EntrypointContract{
		"run": {
			Output: &StructuredOutputContract{
				Format: OutputFormatJSON,
				Schema: &JSONSchema{
					Type:     "object",
					Required: []string{"summary", "passed"},
					Properties: map[string]JSONSchemaProperty{
						"summary": {Type: "string"},
						"passed":  {Type: "boolean"},
					},
				},
			},
		},
	}

	result, err := runEntrypointForTest(t, root, manifest, "run", 5*time.Second)
	require.NoError(t, err)
	require.Equal(t, map[string]any{
		"summary": "ok",
		"passed":  true,
	}, result.Structured)
}

func TestRunEntrypoint_RejectsMalformedStructuredOutput(t *testing.T) {
	root := t.TempDir()
	auditDir := filepath.Join(t.TempDir(), "audit")
	t.Setenv(attshell.EnvAuditDir, auditDir)

	writeScript(t, root, "bin/run", `#!/bin/sh
printf 'not-json\n'
`)

	manifest := runnableManifest(map[string]string{"run": "bin/run"})
	manifest.EntrypointContracts = map[string]EntrypointContract{
		"run": {
			Output: &StructuredOutputContract{
				Format: OutputFormatJSON,
				Schema: &JSONSchema{
					Type:     "object",
					Required: []string{"summary"},
				},
			},
		},
	}

	result, err := runEntrypointForTest(t, root, manifest, "run", 5*time.Second)
	require.Error(t, err)
	require.Contains(t, err.Error(), "parse JSON stdout")
	require.Equal(t, "not-json\n", result.Stdout)
	require.Nil(t, result.Structured)

	records := readPluginAuditRecords(t, auditDir)
	require.Len(t, records, 2)
	require.Equal(t, "finish", records[1].Phase)
	require.Contains(t, records[1].Error, "parse JSON stdout")
}

func TestRunEntrypoint_RejectsStructuredOutputMissingRequiredField(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeScript(t, root, "bin/run", `#!/bin/sh
printf '{"summary":"missing passed"}\n'
`)

	manifest := runnableManifest(map[string]string{"run": "bin/run"})
	manifest.EntrypointContracts = map[string]EntrypointContract{
		"run": {
			Output: &StructuredOutputContract{
				Format: OutputFormatJSON,
				Schema: &JSONSchema{
					Type:     "object",
					Required: []string{"summary", "passed"},
					Properties: map[string]JSONSchemaProperty{
						"summary": {Type: "string"},
						"passed":  {Type: "boolean"},
					},
				},
			},
		},
	}

	result, err := runEntrypointForTest(t, root, manifest, "run", 5*time.Second)
	require.Error(t, err)
	require.Contains(t, err.Error(), `missing required property "passed"`)
	require.JSONEq(t, `{"summary":"missing passed"}`, result.Stdout)
	require.Nil(t, result.Structured)
}

func TestRunEntrypoint_ValidatesDeclaredArgs(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeScript(t, root, "bin/run", `#!/bin/sh
printf 'mode:%s\n' "$1"
`)

	manifest := runnableManifest(map[string]string{"run": "bin/run"})
	manifest.EntrypointArgs["run"] = []ArgumentSpec{{
		Name:     "mode",
		Allowed:  []string{"safe"},
		Required: true,
	}}

	policy := AcceptManifestPolicy(manifest)
	_, err := RunEntrypointWithOptions(
		context.Background(),
		root,
		manifest,
		"run",
		RunOptions{Policy: &policy, Timeout: 5 * time.Second, Args: []string{"unsafe"}},
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "is not allowed")

	result, err := RunEntrypointWithOptions(
		context.Background(),
		root,
		manifest,
		"run",
		RunOptions{Policy: &policy, Timeout: 5 * time.Second, Args: []string{"safe"}},
	)
	require.NoError(t, err)
	require.Equal(t, "mode:safe\n", result.Stdout)
}

func TestRunEntrypoint_UsesContractInputArgs(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeScript(t, root, "bin/run", `#!/bin/sh
printf 'mode:%s\n' "$1"
`)

	manifest := runnableManifest(map[string]string{"run": "bin/run"})
	manifest.EntrypointArgs = nil
	manifest.EntrypointContracts["run"] = EntrypointContract{
		Inputs: EntrypointInputs{
			Args: []ArgumentSpec{{
				Name:     "mode",
				Allowed:  []string{"safe"},
				Required: true,
			}},
		},
		Output: &StructuredOutputContract{Format: OutputFormatText},
	}

	policy := AcceptManifestPolicy(manifest)
	_, err := RunEntrypointWithOptions(
		context.Background(),
		root,
		manifest,
		"run",
		RunOptions{Policy: &policy, Timeout: 5 * time.Second},
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), `missing required arg "mode"`)

	result, err := RunEntrypointWithOptions(
		context.Background(),
		root,
		manifest,
		"run",
		RunOptions{Policy: &policy, Timeout: 5 * time.Second, Args: []string{"safe"}},
	)
	require.NoError(t, err)
	require.Equal(t, "mode:safe\n", result.Stdout)
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

func runEntrypointForTest(
	t *testing.T,
	root string,
	manifest Manifest,
	entrypointName string,
	timeout time.Duration,
) (RunResult, error) {
	t.Helper()

	policy := AcceptManifestPolicy(manifest)

	return RunEntrypointWithOptions(context.Background(), root, manifest, entrypointName, RunOptions{
		Policy:  &policy,
		Timeout: timeout,
	})
}

func readPluginAuditRecords(t *testing.T, auditDir string) []attshell.AuditRecord {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(auditDir, "commands.jsonl"))
	require.NoError(t, err)

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")

	records := make([]attshell.AuditRecord, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}

		var record attshell.AuditRecord
		require.NoError(t, json.Unmarshal([]byte(line), &record))
		records = append(records, record)
	}

	return records
}

func requirePermissionOperation(
	t *testing.T,
	operations []permission.Operation,
	kind permission.OperationKind,
	want permission.Operation,
) {
	t.Helper()

	for _, operation := range operations {
		if operation.Kind == kind {
			require.Equal(t, want, operation)

			return
		}
	}

	require.Failf(t, "missing permission operation", "kind %s not found in %#v", kind, operations)
}

func runnableManifest(entrypoints map[string]string) Manifest {
	var (
		entrypointArgs      = make(map[string][]ArgumentSpec, len(entrypoints))
		entrypointContracts = make(map[string]EntrypointContract, len(entrypoints))
	)

	for name := range entrypoints {
		entrypointArgs[name] = nil
		entrypointContracts[name] = EntrypointContract{
			Output: &StructuredOutputContract{Format: OutputFormatText},
		}
	}

	return Manifest{
		Name:                  "runner",
		Version:               "1.0.0",
		MinimumAttelerVersion: "0.1.0",
		Entrypoints:           entrypoints,
		EntrypointArgs:        entrypointArgs,
		EntrypointContracts:   entrypointContracts,
		Permissions: &PermissionSet{
			Filesystem: FilesystemPermissions{
				Read: []string{"."},
			},
			Shell: ShellPermissions{
				Allow: true,
			},
		},
		Output: &OutputLimits{
			StdoutMaxBytes: 4096,
			StderrMaxBytes: 4096,
		},
		Trust: &Trust{
			Enabled:       true,
			InstallSource: "test",
			Checksum:      "sha256:test",
			Audit: []TrustAudit{{
				Action: "accepted",
				Actor:  "test",
				At:     "2026-05-21T00:00:00Z",
			}},
		},
	}
}
