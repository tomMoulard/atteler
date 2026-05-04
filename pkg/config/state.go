package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// EnvStatePath overrides the YAML file used for persisted interactive state.
const EnvStatePath = "ATTELER_STATE"

// ModelScope controls how long an interactively selected model should remain
// the default.
type ModelScope string

// Supported persisted model scopes.
const (
	ModelScopeSession ModelScope = "session"
	ModelScopeFolder  ModelScope = "folder"
	ModelScopeGlobal  ModelScope = "global"
)

// State stores user choices that are not part of durable project config.
type State struct {
	Folders      map[string]FolderState `json:"folders,omitempty" yaml:"folders,omitempty"`
	DefaultModel string                 `json:"default_model,omitempty" yaml:"default_model,omitempty"`
}

// FolderState stores choices that only apply when Atteler starts from a
// specific working directory.
type FolderState struct {
	DefaultModel string `json:"default_model,omitempty" yaml:"default_model,omitempty"`
}

// StateStore reads and writes Atteler's persisted interactive state as YAML.
type StateStore struct {
	path string
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
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return State{}, nil
		}

		return State{}, fmt.Errorf("state: read %s: %w", s.path, err)
	}

	var state State
	if err := yaml.Unmarshal(data, &state); err != nil {
		return State{}, fmt.Errorf("state: parse %s: %w", s.path, err)
	}

	return state, nil
}

// Save writes the state file.
func (s *StateStore) Save(state State) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o750); err != nil {
		return fmt.Errorf("state: create dir: %w", err)
	}

	data, err := yaml.Marshal(state)
	if err != nil {
		return fmt.Errorf("state: marshal: %w", err)
	}

	if err := os.WriteFile(s.path, data, 0o600); err != nil {
		return fmt.Errorf("state: write %s: %w", s.path, err)
	}

	return nil
}

// ModelForFolder resolves the persisted model for cwd, preferring folder state
// over the global default.
func (s *State) ModelForFolder(cwd string) string {
	if s == nil {
		return ""
	}

	key := FolderKey(cwd)
	if key != "" && s.Folders != nil {
		if folder := s.Folders[key]; folder.DefaultModel != "" {
			return folder.DefaultModel
		}
	}

	return s.DefaultModel
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

		s.Folders[key] = FolderState{DefaultModel: model}
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
