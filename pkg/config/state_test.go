package config

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testStateModelModeFast = "fast"

func TestStateStore_SaveLoadYAML(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state.yaml")
	store := NewStateStore(path)

	state := State{DefaultModel: "codex/gpt-5.5", DefaultReasoningLevel: "high", DefaultModelMode: testStateModelModeFast}
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

	assert.Contains(t, string(data), "version: 1")
	assert.Contains(t, string(data), "revision: 1")

	loaded, err := store.Load()
	if err != nil {
		require.NoError(t, err)
	}

	assert.Equal(t, StateSchemaVersion, loaded.Version)
	assert.Equal(t, int64(1), loaded.Revision)

	if loaded.DefaultModel != "codex/gpt-5.5" {
		require.Failf(t, "unexpected failure", "DefaultModel = %q", loaded.DefaultModel)
	}

	if loaded.DefaultReasoningLevel != "high" {
		require.Failf(t, "unexpected failure", "DefaultReasoningLevel = %q", loaded.DefaultReasoningLevel)
	}

	if loaded.DefaultModelMode != testStateModelModeFast {
		require.Failf(t, "unexpected failure", "DefaultModelMode = %q", loaded.DefaultModelMode)
	}
}

func TestState_ModelForFolderPrefersFolder(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	state := State{DefaultModel: "codex/gpt-5.5", DefaultReasoningLevel: "medium", DefaultModelMode: testStateModelModeFast}
	state.SetModel(ModelScopeFolder, dir, "claude-code/claude-opus-4-6")
	state.SetReasoningLevel(ModelScopeFolder, dir, "xhigh")
	state.SetModelMode(ModelScopeFolder, dir, testStateModelModeFast)

	if got := state.ModelForFolder(dir); got != "claude-code/claude-opus-4-6" {
		require.Failf(t, "unexpected failure", "folder model = %q", got)
	}

	if got := state.ReasoningLevelForFolder(dir); got != "xhigh" {
		require.Failf(t, "unexpected failure", "folder reasoning = %q", got)
	}

	if got := state.ModelModeForFolder(dir); got != testStateModelModeFast {
		require.Failf(t, "unexpected failure", "folder model mode = %q", got)
	}

	if got := state.ModelForFolder(t.TempDir()); got != "codex/gpt-5.5" {
		require.Failf(t, "unexpected failure", "global fallback model = %q", got)
	}

	if got := state.ReasoningLevelForFolder(t.TempDir()); got != "medium" {
		require.Failf(t, "unexpected failure", "global fallback reasoning = %q", got)
	}

	if got := state.ModelModeForFolder(t.TempDir()); got != testStateModelModeFast {
		require.Failf(t, "unexpected failure", "global fallback model mode = %q", got)
	}
}

func TestState_FolderReasoningDefaultOverridesGlobal(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	state := State{DefaultReasoningLevel: "high"}
	state.SetReasoningLevel(ModelScopeFolder, dir, "default")

	if got := state.ReasoningLevelForFolder(dir); got != "" {
		require.Failf(t, "unexpected failure", "folder default reasoning = %q", got)
	}
}

func TestState_FolderModelModeDefaultOverridesGlobal(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	state := State{DefaultModelMode: testStateModelModeFast}
	state.SetModelMode(ModelScopeFolder, dir, "default")

	if got := state.ModelModeForFolder(dir); got != "" {
		require.Failf(t, "unexpected failure", "folder default model mode = %q", got)
	}
}

func TestDefaultStatePath_UsesEnvOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "custom.yaml")
	t.Setenv(EnvStatePath, path)

	if got := DefaultStatePath(); got != path {
		require.Failf(t, "unexpected failure", "DefaultStatePath = %q, want %q", got, path)
	}
}

func TestStateStore_LoadMissingReturnsEmptyState(t *testing.T) {
	t.Parallel()
	store := NewStateStore(filepath.Join(t.TempDir(), "state.yaml"))

	loaded, err := store.Load()
	require.NoError(t, err)
	assert.Equal(t, State{}, loaded)
}

func TestStateStore_LoadMigratesLegacyState(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state.yaml")
	require.NoError(t, os.WriteFile(path, []byte("default_model: legacy-model\n"), 0o600))

	store := NewStateStore(path)
	loaded, err := store.Load()
	require.NoError(t, err)
	assert.Equal(t, StateSchemaVersion, loaded.Version)
	assert.Equal(t, int64(0), loaded.Revision)
	assert.Equal(t, "legacy-model", loaded.DefaultModel)

	require.NoError(t, store.Save(loaded))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(data), "version: 1")
	assert.Contains(t, string(data), "revision: 1")
}

func TestStateStore_LoadRejectsFutureVersion(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state.yaml")
	require.NoError(t, os.WriteFile(path, []byte("version: 99\nrevision: 7\n"), 0o600))

	_, err := NewStateStore(path).Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported version 99")
	assert.Contains(t, err.Error(), path)
}

func TestStateStore_LoadParseErrorNamesFileAndRecovery(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state.yaml")
	require.NoError(t, os.WriteFile(path, []byte("version: [broken\n"), 0o600))

	_, err := NewStateStore(path).Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), path)
	assert.Contains(t, err.Error(), "fix the YAML")
	assert.Contains(t, err.Error(), "move this file aside")
}

func TestStateStore_LoadEmptyFileFailsLoudly(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state.yaml")
	require.NoError(t, os.WriteFile(path, []byte(" \n\t"), 0o600))

	_, err := NewStateStore(path).Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), path)
	assert.Contains(t, err.Error(), "empty file")
	assert.Contains(t, err.Error(), "move this file aside")
}

func TestStateStore_SaveErrorNamesFileAndRecovery(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	parentFile := filepath.Join(dir, "not-a-dir")
	require.NoError(t, os.WriteFile(parentFile, []byte("blocks state dir"), 0o600))

	path := filepath.Join(parentFile, "state.yaml")
	err := NewStateStore(path).Save(State{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), path)
	assert.Contains(t, err.Error(), EnvStatePath)
	assert.Contains(t, err.Error(), "writable state file")
}

func TestStateStore_SaveDetectsStaleRevision(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state.yaml")
	storeA := NewStateStore(path)
	storeB := NewStateStore(path)

	stateA, err := storeA.Load()
	require.NoError(t, err)
	stateB, err := storeB.Load()
	require.NoError(t, err)

	stateA.SetModel(ModelScopeGlobal, "", "global-model")
	require.NoError(t, storeA.Save(stateA))

	stateB.SetModel(ModelScopeFolder, t.TempDir(), "folder-model")
	err = storeB.Save(stateB)
	require.ErrorIs(t, err, ErrStateConflict)
	assert.Contains(t, err.Error(), path)
	assert.Contains(t, err.Error(), "loaded revision 0")
	assert.Contains(t, err.Error(), "current revision 1")

	loaded, err := storeA.Load()
	require.NoError(t, err)
	assert.Equal(t, "global-model", loaded.DefaultModel)
	assert.Empty(t, loaded.Folders)
}

func TestStateStore_UpdateMergesDifferentScopes(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state.yaml")
	storeA := NewStateStore(path)
	storeB := NewStateStore(path)
	folder := t.TempDir()

	_, err := storeA.Update(func(state *State) error {
		state.SetModel(ModelScopeGlobal, "", "global-model")
		return nil
	})
	require.NoError(t, err)

	_, err = storeB.Update(func(state *State) error {
		state.SetModel(ModelScopeFolder, folder, "folder-model")
		return nil
	})
	require.NoError(t, err)

	loaded, err := storeA.Load()
	require.NoError(t, err)
	assert.Equal(t, int64(2), loaded.Revision)
	assert.Equal(t, "global-model", loaded.ModelForFolder(t.TempDir()))
	assert.Equal(t, "folder-model", loaded.ModelForFolder(folder))
}

func TestStateStore_UpdateConcurrentStoresMergeDifferentScopes(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.yaml")
	storeA := NewStateStore(path)
	storeB := NewStateStore(path)
	folder := t.TempDir()
	start := make(chan struct{})
	errs := make(chan error, 2)

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()

		<-start

		_, err := storeA.Update(func(state *State) error {
			state.SetModel(ModelScopeGlobal, "", "global-model")
			return nil
		})
		errs <- err
	}()

	go func() {
		defer wg.Done()

		<-start

		_, err := storeB.Update(func(state *State) error {
			state.SetModel(ModelScopeFolder, folder, "folder-model")
			return nil
		})
		errs <- err
	}()

	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		require.NoError(t, err)
	}

	loaded, err := storeA.Load()
	require.NoError(t, err)
	assert.Equal(t, int64(2), loaded.Revision)
	assert.Equal(t, "global-model", loaded.ModelForFolder(t.TempDir()))
	assert.Equal(t, "folder-model", loaded.ModelForFolder(folder))
}

func TestStateStore_InterruptedTempWriteLeavesPreviousState(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.yaml")
	store := NewStateStore(path)
	require.NoError(t, store.Save(State{DefaultModel: "valid-model"}))

	tmp, err := os.CreateTemp(dir, ".atteler-state-*.tmp")
	require.NoError(t, err)
	_, err = tmp.WriteString("version: [interrupted")
	require.NoError(t, err)
	require.NoError(t, tmp.Sync())
	require.NoError(t, tmp.Close())

	loaded, err := store.Load()
	require.NoError(t, err)
	assert.Equal(t, "valid-model", loaded.DefaultModel)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(data), "valid-model")
	assert.NotContains(t, string(data), "interrupted")
}

func TestStateStore_PreservesUnknownFields(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state.yaml")
	folderKey := "/tmp/atteler-future"

	require.NoError(t, os.WriteFile(path, []byte(`version: 1
revision: 0
future_metadata:
    owner: future-atteler
folders:
    /tmp/atteler-future:
        default_model: folder-model
        future_folder_metadata: keep-me
`), 0o600))

	store := NewStateStore(path)
	loaded, err := store.Load()
	require.NoError(t, err)
	assert.Equal(t, "folder-model", loaded.Folders[folderKey].DefaultModel)
	require.Contains(t, loaded.UnknownFields, "future_metadata")
	require.Contains(t, loaded.Folders[folderKey].UnknownFields, "future_folder_metadata")

	loaded.DefaultModel = "global-model"
	require.NoError(t, store.Save(loaded))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(data), "future_metadata:")
	assert.Contains(t, string(data), "future_folder_metadata: keep-me")
	assert.Contains(t, string(data), "global-model")
}
