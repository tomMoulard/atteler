package plugin

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadDir_LoadsYAMLManifest(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeManifest(t, dir, "plugin.yaml", `
name: reviewer
version: 1.2.3
description: Reviews code changes
capabilities:
  - review
  - summarize
entrypoints:
  run: bin/reviewer
  setup: scripts/setup.sh
`)

	manifest, err := LoadDir(dir)
	if err != nil {
		require.NoError(t, err)
	}

	if manifest.Name != "reviewer" {
		require.Failf(t, "unexpected manifest", "Name = %q, want reviewer", manifest.Name)
	}

	if manifest.Version != "1.2.3" {
		require.Failf(t, "unexpected manifest", "Version = %q, want 1.2.3", manifest.Version)
	}

	if manifest.Description != "Reviews code changes" {
		require.Failf(t, "unexpected manifest", "Description = %q", manifest.Description)
	}

	if !reflect.DeepEqual(manifest.Capabilities, []string{"review", "summarize"}) {
		require.Failf(t, "unexpected manifest", "Capabilities = %v", manifest.Capabilities)
	}

	if !reflect.DeepEqual(manifest.Entrypoints, map[string]string{"run": "bin/reviewer", "setup": "scripts/setup.sh"}) {
		require.Failf(t, "unexpected manifest", "Entrypoints = %v", manifest.Entrypoints)
	}
}

func TestLoad_LoadsExplicitJSONManifest(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeManifest(t, dir, "custom.json", `{
  "name": "json-plugin",
  "version": "0.1.0",
  "capabilities": ["chat"],
  "entrypoints": {"run": "main.js"}
}`)

	manifest, err := Load(path)
	if err != nil {
		require.NoError(t, err)
	}

	if manifest.Name != "json-plugin" {
		require.Failf(t, "unexpected manifest", "Name = %q", manifest.Name)
	}

	if manifest.Version != "0.1.0" {
		require.Failf(t, "unexpected manifest", "Version = %q", manifest.Version)
	}

	if !reflect.DeepEqual(manifest.Capabilities, []string{"chat"}) {
		require.Failf(t, "unexpected manifest", "Capabilities = %v", manifest.Capabilities)
	}

	if !reflect.DeepEqual(manifest.Entrypoints, map[string]string{"run": "main.js"}) {
		require.Failf(t, "unexpected manifest", "Entrypoints = %v", manifest.Entrypoints)
	}
}

func TestLoadDir_LoadsSecurityDeclarations(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeManifest(t, dir, "plugin.yaml", `
name: secure
version: "1.0.0"
entrypoints:
  run: bin/run
entrypoint_args:
  run:
    - name: mode
      required: true
      allowed:
        - safe
permissions:
  filesystem:
    read:
      - "."
    write:
      - tmp
  network:
    allow: true
    hosts:
      - api.example.com
  shell:
    allow: true
  env:
    - PATH
  secrets:
    - API_TOKEN
  tools:
    - reviewer
output:
  stdout_max_bytes: 4096
  stderr_max_bytes: 1024
trust:
  enabled: true
  install_source: gh:tomMoulard/example
  checksum: sha256:abc123
  audit:
    - action: accepted
      actor: test
      at: "2026-05-21T00:00:00Z"
`)

	manifest, err := LoadDir(dir)
	require.NoError(t, err)
	require.Equal(t, []string{"."}, manifest.Permissions.Filesystem.Read)
	require.Equal(t, []string{"tmp"}, manifest.Permissions.Filesystem.Write)
	require.True(t, manifest.Permissions.Network.Allow)
	require.Equal(t, []string{"api.example.com"}, manifest.Permissions.Network.Hosts)
	require.Equal(t, []string{"PATH"}, manifest.Permissions.Env)
	require.Equal(t, []string{"API_TOKEN"}, manifest.Permissions.Secrets)
	require.Equal(t, []string{"reviewer"}, manifest.Permissions.Tools)
	require.Equal(t, 4096, manifest.Output.StdoutMaxBytes)
	require.Equal(t, "gh:tomMoulard/example", manifest.Trust.InstallSource)
	require.Equal(t, []ArgumentSpec{{
		Name:     "mode",
		Allowed:  []string{"safe"},
		Required: true,
	}}, manifest.EntrypointArgs["run"])
}

func TestLoadDir_LoadsExtensionContractMetadata(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeManifest(t, dir, "plugin.yaml", `
name: structured
version: "1.0.0"
min_atteler_version: "1.2.0"
description: Emits structured findings
capabilities:
  - review
entrypoints:
  run: bin/run
entrypoint_contracts:
  run:
    inputs:
      args:
        - name: mode
          required: true
          allowed: [safe]
    output:
      format: json
      schema:
        type: object
        required: [summary, passed]
        properties:
          summary:
            type: string
          passed:
            type: boolean
provenance:
  source: registry
  repository: gh:tomMoulard/structured
  ref: v1.0.0
  digest: sha256:abc123
`)

	manifest, err := LoadDir(dir)
	require.NoError(t, err)
	require.Equal(t, "1.2.0", manifest.MinimumAttelerVersion)
	require.Equal(t, "registry", manifest.Provenance.Source)
	require.Equal(t, "sha256:abc123", manifest.Provenance.Digest)

	contract := manifest.EntrypointContracts["run"]
	require.Equal(t, []ArgumentSpec{{
		Name:     "mode",
		Allowed:  []string{"safe"},
		Required: true,
	}}, contract.Inputs.Args)
	require.NotNil(t, contract.Output)
	require.Equal(t, "json", contract.Output.Format)
	require.NotNil(t, contract.Output.Schema)
	require.Equal(t, []string{"summary", "passed"}, contract.Output.Schema.Required)
	require.Equal(t, "string", contract.Output.Schema.Properties["summary"].Type)
	require.Equal(t, "boolean", contract.Output.Schema.Properties["passed"].Type)
}

func TestFindManifest_PrefersYAMLThenYMLThenJSON(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	jsonPath := writeManifest(t, dir, "plugin.json", `{"name":"json","version":"1"}`)
	ymlPath := writeManifest(t, dir, "plugin.yml", `name: yml
version: "1"
`)
	yamlPath := writeManifest(t, dir, "plugin.yaml", `name: yaml
version: "1"
`)

	path, err := FindManifest(dir)
	if err != nil {
		require.NoError(t, err)
	}

	if path != yamlPath {
		require.Failf(t, "unexpected path", "path = %s, want %s", path, yamlPath)
	}

	if removeErr := os.Remove(yamlPath); removeErr != nil {
		require.NoError(t, removeErr)
	}

	path, err = FindManifest(dir)
	if err != nil {
		require.NoError(t, err)
	}

	if path != ymlPath {
		require.Failf(t, "unexpected path", "path = %s, want %s", path, ymlPath)
	}

	if removeErr := os.Remove(ymlPath); removeErr != nil {
		require.NoError(t, removeErr)
	}

	path, err = FindManifest(dir)
	if err != nil {
		require.NoError(t, err)
	}

	if path != jsonPath {
		require.Failf(t, "unexpected path", "path = %s, want %s", path, jsonPath)
	}
}

func TestLoadDir_RejectsMissingNameOrVersion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
		want    string
	}{
		{name: "missing name", content: `version: "1"`, want: "missing name"},
		{name: "missing version", content: `name: sample`, want: "missing version"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			writeManifest(t, dir, "plugin.yaml", tt.content)

			_, err := LoadDir(dir)
			if err == nil {
				require.FailNow(t, "expected validation error")
			}

			if got := err.Error(); !strings.Contains(got, tt.want) {
				require.Failf(t, "unexpected error", "error = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLoadDir_RejectsEntrypointEscapingRoot(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		entrypoint string
	}{
		{name: "parent traversal", entrypoint: "../outside.sh"},
		{name: "absolute path", entrypoint: filepath.Join(string(filepath.Separator), "tmp", "outside.sh")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			writeManifest(t, dir, "plugin.yaml", `
name: escape
version: "1"
entrypoints:
  run: `+tt.entrypoint+`
`)

			_, err := LoadDir(dir)
			if err == nil {
				require.FailNow(t, "expected validation error")
			}

			if got := err.Error(); !strings.Contains(got, "escapes plugin root") {
				require.Failf(t, "unexpected error", "error = %q", got)
			}
		})
	}
}

func TestLoadDir_RejectsInvalidSecurityDeclarations(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name: "filesystem write escapes root",
			content: `
name: escape
version: "1"
permissions:
  filesystem:
    write:
      - ../outside
`,
			want: "escapes plugin root",
		},
		{
			name: "invalid env name",
			content: `
name: bad-env
version: "1"
permissions:
  env:
    - BAD-NAME
`,
			want: "not a valid environment variable name",
		},
		{
			name: "network hosts without allow",
			content: `
name: bad-network
version: "1"
permissions:
  network:
    hosts:
      - api.example.com
`,
			want: "network hosts require allow: true",
		},
		{
			name: "missing output limit",
			content: `
name: bad-output
version: "1"
output:
  stdout_max_bytes: 4096
`,
			want: "stderr_max_bytes must be positive",
		},
		{
			name: "entrypoint args without entrypoint",
			content: `
name: bad-args
version: "1"
entrypoint_args:
  run: []
`,
			want: `entrypoint_args "run" has no matching entrypoint`,
		},
		{
			name: "required arg after optional",
			content: `
name: bad-args
version: "1"
entrypoints:
  run: bin/run
entrypoint_args:
  run:
    - name: optional
    - name: required
      required: true
`,
			want: `required after optional argument`,
		},
		{
			name: "duplicate capabilities",
			content: `
name: duplicate-capability
version: "1"
capabilities:
  - review
  - review
`,
			want: `capabilities duplicate value "review"`,
		},
		{
			name: "invalid structured output format",
			content: `
name: bad-output-contract
version: "1"
entrypoints:
  run: bin/run
entrypoint_contracts:
  run:
    output:
      format: xml
`,
			want: `output format "xml" is not supported`,
		},
		{
			name: "invalid minimum atteler version",
			content: `
name: bad-version
version: "1"
min_atteler_version: latest
`,
			want: `min_atteler_version "latest" is not a supported version requirement`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			writeManifest(t, dir, "plugin.yaml", tt.content)

			_, err := LoadDir(dir)
			require.Error(t, err)
			require.Contains(t, err.Error(), tt.want)
		})
	}
}

func TestLoadDir_ErrorsWhenManifestMissing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	_, err := LoadDir(dir)
	if err == nil {
		require.FailNow(t, "expected missing manifest error")
	}

	if got := err.Error(); !strings.Contains(got, "no manifest found") {
		require.Failf(t, "unexpected error", "error = %q", got)
	}
}

func writeManifest(t *testing.T, dir, name, content string) string {
	t.Helper()

	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		require.NoError(t, err)
	}

	return path
}
