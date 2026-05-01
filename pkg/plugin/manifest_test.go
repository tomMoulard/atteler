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
