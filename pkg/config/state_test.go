package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStateStore_SaveLoadYAML(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state.yaml")
	store := NewStateStore(path)

	state := State{DefaultModel: "codex/gpt-5.5"}
	state.SetModel(ModelScopeFolder, t.TempDir(), "claude-code/claude-opus-4-6")

	if err := store.Save(state); err != nil {
		require.NoError(t, err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		require.NoError(t, err)
	}

	if len(data) == 0 || data[0] == '{' {
		require.Failf(t, "unexpected failure", "state should be YAML, got: %s", data)
	}

	loaded, err := store.Load()
	if err != nil {
		require.NoError(t, err)
	}

	if loaded.DefaultModel != "codex/gpt-5.5" {
		require.Failf(t, "unexpected failure", "DefaultModel = %q", loaded.DefaultModel)
	}
}

func TestState_ModelForFolderPrefersFolder(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	state := State{DefaultModel: "codex/gpt-5.5"}
	state.SetModel(ModelScopeFolder, dir, "claude-code/claude-opus-4-6")

	if got := state.ModelForFolder(dir); got != "claude-code/claude-opus-4-6" {
		require.Failf(t, "unexpected failure", "folder model = %q", got)
	}

	if got := state.ModelForFolder(t.TempDir()); got != "codex/gpt-5.5" {
		require.Failf(t, "unexpected failure", "global fallback model = %q", got)
	}
}

func TestDefaultStatePath_UsesEnvOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "custom.yaml")
	t.Setenv(EnvStatePath, path)

	if got := DefaultStatePath(); got != path {
		require.Failf(t, "unexpected failure", "DefaultStatePath = %q, want %q", got, path)
	}
}
