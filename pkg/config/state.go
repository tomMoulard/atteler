package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// EnvStatePath overrides the YAML file used for persisted interactive state.
const EnvStatePath = "ATTELER_STATE"

// StateSchemaVersion is the current on-disk schema version for interactive
// state files.
const StateSchemaVersion = 1

// ModelScope controls how long an interactively selected model should remain
// the default.
type ModelScope string

// Supported persisted model scopes.
const (
	ModelScopeSession ModelScope = "session"
	ModelScopeFolder  ModelScope = "folder"
	ModelScopeGlobal  ModelScope = "global"

	stateReasoningLevelDefault = "default"
)

// ErrStateConflict identifies a save that would overwrite a newer state
// revision on disk.
var ErrStateConflict = errors.New("state revision conflict")

// State stores user choices that are not part of durable project config.
//
//nolint:govet // field order keeps the YAML schema stable and readable.
type State struct {
	Version      int                    `json:"version,omitempty" yaml:"version,omitempty"`
	Revision     int64                  `json:"revision,omitempty" yaml:"revision,omitempty"`
	Folders      map[string]FolderState `json:"folders,omitempty" yaml:"folders,omitempty"`
	DefaultModel string                 `json:"default_model,omitempty" yaml:"default_model,omitempty"`
	// DefaultReasoningLevel stores the global default effort/reasoning level
	// selected interactively. It intentionally lives in state rather than
	// config because it is a user preference, not project policy.
	DefaultReasoningLevel string `json:"default_reasoning_level,omitempty" yaml:"default_reasoning_level,omitempty"`
	// UnknownFields preserves unrecognized YAML keys so newer metadata is not
	// silently deleted by older Atteler versions.
	UnknownFields map[string]any `json:"-" yaml:",inline,omitempty"`
}

// FolderState stores choices that only apply when Atteler starts from a
// specific working directory.
//
//nolint:govet // field order keeps the YAML schema stable and readable.
type FolderState struct {
	DefaultModel          string `json:"default_model,omitempty" yaml:"default_model,omitempty"`
	DefaultReasoningLevel string `json:"default_reasoning_level,omitempty" yaml:"default_reasoning_level,omitempty"`
	// UnknownFields preserves unrecognized per-folder YAML keys across writes.
	UnknownFields map[string]any `json:"-" yaml:",inline,omitempty"`
}

// PreferenceResolution describes where a persisted preference came from.
type PreferenceResolution struct {
	Value     string
	Source    string
	Scope     ModelScope
	FolderKey string
}

// StateConflictError reports that the caller loaded an older state revision
// than the one currently on disk.
type StateConflictError struct {
	Path            string
	LoadedRevision  int64
	CurrentRevision int64
}

// StateStore reads and writes Atteler's persisted interactive state as YAML.
type StateStore struct {
	path string
}

// Error returns a user-facing conflict diagnostic with the affected path and a
// recovery hint.
func (e StateConflictError) Error() string {
	return fmt.Sprintf(
		"state: conflict writing %s: loaded revision %d, current revision %d; reload the state and retry, or inspect this file before editing it manually",
		e.Path,
		e.LoadedRevision,
		e.CurrentRevision,
	)
}

// Unwrap makes errors.Is(err, ErrStateConflict) work for conflict failures.
func (e StateConflictError) Unwrap() error {
	return ErrStateConflict
}

// NewStateStore returns a state store. If path is empty, DefaultStatePath is
// used.
func NewStateStore(path string) *StateStore {
	if path == "" {
		path = DefaultStatePath()
	}

	return &StateStore{path: path}
}

// DefaultStatePath returns the default path for persisted interactive state.
func DefaultStatePath() string {
	if path := strings.TrimSpace(os.Getenv(EnvStatePath)); path != "" {
		return path
	}

	if dir := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); dir != "" {
		return filepath.Join(dir, "atteler", "state.yaml")
	}

	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(os.TempDir(), "atteler", "state.yaml")
	}

	return filepath.Join(home, ".local", "state", "atteler", "state.yaml")
}

// Path returns the underlying YAML state file path.
func (s *StateStore) Path() string {
	return s.path
}

// Load reads the state file. A missing state file is treated as empty state.
func (s *StateStore) Load() (State, error) {
	return s.load()
}

func (s *StateStore) load() (State, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return emptyState(), nil
		}

		return State{}, fmt.Errorf("state: read %s: %w", s.path, err)
	}

	if len(bytes.TrimSpace(data)) == 0 {
		return State{}, fmt.Errorf("state: parse %s: empty file; move this file aside to let Atteler recreate it", s.path)
	}

	var state State
	if err := yaml.Unmarshal(data, &state); err != nil {
		return State{}, fmt.Errorf("state: parse %s: %w; fix the YAML or move this file aside to let Atteler recreate it", s.path, err)
	}

	return migrateState(state, s.path)
}

// Save writes the state file atomically. It refuses to overwrite a newer
// revision than the one carried by state; callers that want merge-on-write
// semantics should use Update.
func (s *StateStore) Save(state State) error {
	return s.withLock(func() error {
		current, err := s.load()
		if err != nil {
			return err
		}

		if state.Revision != current.Revision {
			return StateConflictError{
				Path:            s.path,
				LoadedRevision:  state.Revision,
				CurrentRevision: current.Revision,
			}
		}

		return s.saveLocked(state, current.Revision+1)
	})
}

// Update reloads, mutates, and atomically writes state while holding the state
// file lock. It is the preferred path for interactive preference updates
// because concurrent writers merge by reading the latest committed revision.
func (s *StateStore) Update(update func(*State) error) (State, error) {
	if update == nil {
		return State{}, errors.New("state: update function is required")
	}

	var saved State

	err := s.withLock(func() error {
		current, err := s.load()
		if err != nil {
			return err
		}

		next := current
		if err := update(&next); err != nil {
			return err
		}

		if err := s.saveLocked(next, current.Revision+1); err != nil {
			return err
		}

		saved = normalizeStateForSave(next, current.Revision+1)

		return nil
	})
	if err != nil {
		return State{}, err
	}

	return saved, nil
}

func (s *StateStore) saveLocked(state State, revision int64) error {
	state = normalizeStateForSave(state, revision)

	data, err := yaml.Marshal(state)
	if err != nil {
		return fmt.Errorf("state: marshal %s: %w", s.path, err)
	}

	return writeStateFileAtomic(s.path, data, 0o600)
}

func (s *StateStore) withLock(fn func() error) (err error) {
	dir := filepath.Dir(s.path)

	if mkdirErr := os.MkdirAll(dir, 0o750); mkdirErr != nil {
		return fmt.Errorf("state: create dir %s: %w", dir, mkdirErr)
	}

	lockPath := s.path + ".lock"

	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("state: open lock %s: %w", lockPath, err)
	}
	defer lockFile.Close()

	if lockErr := lockStateFile(lockFile); lockErr != nil {
		return fmt.Errorf("state: lock %s: %w", lockPath, lockErr)
	}

	defer func() {
		if unlockErr := unlockStateFile(lockFile); unlockErr != nil && err == nil {
			err = fmt.Errorf("state: unlock %s: %w", lockPath, unlockErr)
		}
	}()

	return fn()
}

func writeStateFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)

	tmp, err := os.CreateTemp(dir, ".atteler-state-*.tmp")
	if err != nil {
		return fmt.Errorf("state: create temp in %s: %w", dir, err)
	}

	tmpPath := tmp.Name()
	cleanup := true

	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	if err := os.Chmod(tmpPath, mode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("state: chmod temp %s: %w", tmpPath, err)
	}

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("state: write temp %s: %w", tmpPath, err)
	}

	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("state: sync temp %s: %w", tmpPath, err)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("state: close temp %s: %w", tmpPath, err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("state: replace %s: %w", path, err)
	}

	cleanup = false

	if err := syncStateDir(dir); err != nil {
		return err
	}

	return nil
}

func emptyState() State {
	return State{}
}

func migrateState(state State, path string) (State, error) {
	switch state.Version {
	case 0:
		state.Version = StateSchemaVersion
	case StateSchemaVersion:
	default:
		return State{}, fmt.Errorf(
			"state: unsupported version %d in %s; upgrade Atteler or move this file aside after backing it up",
			state.Version,
			path,
		)
	}

	return state, nil
}

func normalizeStateForSave(state State, revision int64) State {
	state.Version = StateSchemaVersion
	state.Revision = revision

	return state
}

// ModelForFolder resolves the persisted model for cwd, preferring folder state
// over the global default.
func (s *State) ModelForFolder(cwd string) string {
	return s.ResolveModelPreference(cwd).Value
}

// ReasoningLevelForFolder resolves the persisted effort for cwd, preferring
// folder state over the global default.
func (s *State) ReasoningLevelForFolder(cwd string) string {
	return s.ResolveReasoningPreference(cwd).Value
}

// ResolveModelPreference resolves the persisted model for cwd and reports the
// state scope that supplied it.
func (s *State) ResolveModelPreference(cwd string) PreferenceResolution {
	if s == nil {
		return PreferenceResolution{}
	}

	key := FolderKey(cwd)
	if key != "" && s.Folders != nil {
		if folder := s.Folders[key]; folder.DefaultModel != "" {
			return PreferenceResolution{
				Value:     folder.DefaultModel,
				Source:    "state.folder",
				Scope:     ModelScopeFolder,
				FolderKey: key,
			}
		}
	}

	if s.DefaultModel != "" {
		return PreferenceResolution{
			Value:     s.DefaultModel,
			Source:    "state.global",
			Scope:     ModelScopeGlobal,
			FolderKey: key,
		}
	}

	return PreferenceResolution{FolderKey: key}
}

// ResolveReasoningPreference resolves the persisted reasoning level for cwd
// and reports the state scope that supplied it.
func (s *State) ResolveReasoningPreference(cwd string) PreferenceResolution {
	if s == nil {
		return PreferenceResolution{}
	}

	key := FolderKey(cwd)
	if key != "" && s.Folders != nil {
		if folder, ok := s.Folders[key]; ok && folder.DefaultReasoningLevel != "" {
			resolution := PreferenceResolution{
				Source:    "state.folder",
				Scope:     ModelScopeFolder,
				FolderKey: key,
			}
			if folder.DefaultReasoningLevel != stateReasoningLevelDefault {
				resolution.Value = folder.DefaultReasoningLevel
			}

			return resolution
		}
	}

	if s.DefaultReasoningLevel != "" {
		resolution := PreferenceResolution{
			Source:    "state.global",
			Scope:     ModelScopeGlobal,
			FolderKey: key,
		}
		if s.DefaultReasoningLevel != stateReasoningLevelDefault {
			resolution.Value = s.DefaultReasoningLevel
		}

		return resolution
	}

	return PreferenceResolution{FolderKey: key}
}

// SetModel stores model at the requested scope. Session scope is intentionally
// not persisted.
func (s *State) SetModel(scope ModelScope, cwd, model string) {
	model = strings.TrimSpace(model)

	switch scope {
	case ModelScopeGlobal:
		s.DefaultModel = model
	case ModelScopeFolder:
		key := FolderKey(cwd)
		if key == "" {
			return
		}

		if s.Folders == nil {
			s.Folders = make(map[string]FolderState)
		}

		folder := s.Folders[key]
		folder.DefaultModel = model
		s.Folders[key] = folder
	}
}

// SetReasoningLevel stores effort at the requested scope. Session scope is
// intentionally not persisted. An empty level clears that persisted override.
func (s *State) SetReasoningLevel(scope ModelScope, cwd, level string) {
	level = strings.TrimSpace(level)

	switch scope {
	case ModelScopeGlobal:
		if level == stateReasoningLevelDefault {
			level = ""
		}

		s.DefaultReasoningLevel = level
	case ModelScopeFolder:
		key := FolderKey(cwd)
		if key == "" {
			return
		}

		if s.Folders == nil {
			s.Folders = make(map[string]FolderState)
		}

		folder := s.Folders[key]
		folder.DefaultReasoningLevel = level
		s.Folders[key] = folder
	}
}

// FolderKey canonicalizes a folder path for persisted folder-scoped state.
func FolderKey(cwd string) string {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		return ""
	}

	abs, err := filepath.Abs(cwd)
	if err != nil {
		return filepath.Clean(cwd)
	}

	if evaluated, err := filepath.EvalSymlinks(abs); err == nil {
		abs = evaluated
	}

	return filepath.Clean(abs)
}
