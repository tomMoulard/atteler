// Package plugin loads and validates atteler plugin manifests.
package plugin

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

var manifestFilenames = []string{"plugin.yaml", "plugin.yml", "plugin.json"}

// Manifest describes an atteler plugin.
//
//nolint:govet // Field order follows manifest readability, not memory layout.
type Manifest struct {
	Name         string            `json:"name" yaml:"name"`
	Version      string            `json:"version" yaml:"version"`
	Description  string            `json:"description,omitempty" yaml:"description,omitempty"`
	Capabilities []string          `json:"capabilities,omitempty" yaml:"capabilities,omitempty"`
	Entrypoints  map[string]string `json:"entrypoints,omitempty" yaml:"entrypoints,omitempty"`
}

// Load reads and validates a plugin manifest from a plugin directory or from an
// explicit manifest file path. Directories are searched for plugin.yaml,
// plugin.yml, then plugin.json.
func Load(path string) (Manifest, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return Manifest{}, errors.New("plugin: empty manifest path")
	}

	info, err := os.Stat(path)
	if err != nil {
		return Manifest{}, fmt.Errorf("plugin: stat %s: %w", path, err)
	}

	if info.IsDir() {
		return LoadDir(path)
	}

	return LoadFile(path)
}

// LoadDir reads and validates the first conventional plugin manifest found in
// dir. The search order is plugin.yaml, plugin.yml, then plugin.json.
func LoadDir(dir string) (Manifest, error) {
	path, err := FindManifest(dir)
	if err != nil {
		return Manifest{}, err
	}

	return loadFile(path, dir)
}

// LoadFile reads and validates an explicit plugin manifest file.
func LoadFile(path string) (Manifest, error) {
	return loadFile(path, filepath.Dir(path))
}

// FindManifest returns the first conventional plugin manifest path in dir. The
// search order is plugin.yaml, plugin.yml, then plugin.json.
func FindManifest(dir string) (string, error) {
	for _, name := range manifestFilenames {
		path := filepath.Join(dir, name)

		info, err := os.Stat(path)
		if err == nil && !info.IsDir() {
			return path, nil
		}

		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("plugin: stat %s: %w", path, err)
		}
	}

	return "", fmt.Errorf("plugin: no manifest found in %s", dir)
}

func loadFile(path, root string) (Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, fmt.Errorf("plugin: read %s: %w", path, err)
	}

	var manifest Manifest
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("plugin: parse %s: %w", path, err)
	}

	if err := manifest.Validate(root); err != nil {
		return Manifest{}, fmt.Errorf("plugin: validate %s: %w", path, err)
	}

	return manifest, nil
}

// Validate checks required manifest fields and ensures entrypoint paths stay
// within root.
func (m Manifest) Validate(root string) error {
	if strings.TrimSpace(m.Name) == "" {
		return errors.New("missing name")
	}

	if strings.TrimSpace(m.Version) == "" {
		return errors.New("missing version")
	}

	for name, path := range m.Entrypoints {
		if strings.TrimSpace(name) == "" {
			return errors.New("entrypoint has empty name")
		}

		if err := validateEntrypoint(root, path); err != nil {
			return fmt.Errorf("entrypoint %q: %w", name, err)
		}
	}

	return nil
}

func validateEntrypoint(root, entrypoint string) error {
	entrypoint = strings.TrimSpace(entrypoint)
	if entrypoint == "" {
		return errors.New("empty path")
	}

	if filepath.IsAbs(entrypoint) {
		return fmt.Errorf("path %q escapes plugin root %q", entrypoint, root)
	}

	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("resolve plugin root: %w", err)
	}

	targetAbs, err := filepath.Abs(filepath.Join(rootAbs, entrypoint))
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}

	rel, err := filepath.Rel(rootAbs, targetAbs)
	if err != nil {
		return fmt.Errorf("compare with plugin root: %w", err)
	}

	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("path %q escapes plugin root %q", entrypoint, root)
	}

	return nil
}
