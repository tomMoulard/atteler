package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStateStore_SaveLoadYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.yaml")
	store := NewStateStore(path)

	state := State{DefaultModel: "codex/gpt-5.5"}
	state.SetModel(ModelScopeFolder, t.TempDir(), "claude-code/claude-opus-4-6")
	if err := store.Save(state); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 || data[0] == '{' {
		t.Fatalf("state should be YAML, got: %s", data)
	}

	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.DefaultModel != "codex/gpt-5.5" {
		t.Fatalf("DefaultModel = %q", loaded.DefaultModel)
	}
}

func TestState_ModelForFolderPrefersFolder(t *testing.T) {
	dir := t.TempDir()
	state := State{DefaultModel: "codex/gpt-5.5"}
	state.SetModel(ModelScopeFolder, dir, "claude-code/claude-opus-4-6")

	if got := state.ModelForFolder(dir); got != "claude-code/claude-opus-4-6" {
		t.Fatalf("folder model = %q", got)
	}
	if got := state.ModelForFolder(t.TempDir()); got != "codex/gpt-5.5" {
		t.Fatalf("global fallback model = %q", got)
	}
}

func TestDefaultStatePath_UsesEnvOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "custom.yaml")
	t.Setenv(EnvStatePath, path)

	if got := DefaultStatePath(); got != path {
		t.Fatalf("DefaultStatePath = %q, want %q", got, path)
	}
}
