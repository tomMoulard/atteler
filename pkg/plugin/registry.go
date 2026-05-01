package plugin

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Plugin is a loaded plugin manifest plus its filesystem locations.
type Plugin struct {
	Manifest     Manifest
	Root         string
	ManifestPath string
}

// Entrypoint is a resolved plugin entrypoint that is ready to run, but has not
// been executed.
type Entrypoint struct {
	PluginName     string
	EntrypointName string
	Root           string
	ManifestPath   string
	RelativePath   string
	Path           string
}

// DryRun describes what would be executed for a plugin entrypoint without
// running it.
type DryRun struct {
	Entrypoint  Entrypoint
	Description string
}

// Registry stores configured plugins by manifest name.
type Registry struct {
	plugins map[string]Plugin
	order   []string
}

// LoadRegistry loads all configured plugin paths into a registry.
func LoadRegistry(paths []string) (*Registry, error) {
	registry := &Registry{plugins: make(map[string]Plugin, len(paths))}
	for _, path := range paths {
		plugin, err := loadPlugin(path)
		if err != nil {
			return nil, err
		}

		name := strings.TrimSpace(plugin.Manifest.Name)
		if existing, ok := registry.plugins[name]; ok {
			return nil, fmt.Errorf(
				"plugin: duplicate plugin name %q in %s and %s",
				name,
				existing.ManifestPath,
				plugin.ManifestPath,
			)
		}
		registry.plugins[name] = plugin
		registry.order = append(registry.order, name)
	}
	return registry, nil
}

// NewRegistry loads all configured plugin paths into a registry.
func NewRegistry(paths []string) (*Registry, error) {
	return LoadRegistry(paths)
}

// List returns loaded plugin names in configuration order.
func (r *Registry) List() []string {
	if r == nil || len(r.order) == 0 {
		return nil
	}
	names := append([]string(nil), r.order...)
	return names
}

// Get returns a loaded plugin by manifest name.
func (r *Registry) Get(name string) (Plugin, bool) {
	if r == nil {
		return Plugin{}, false
	}
	plugin, ok := r.plugins[strings.TrimSpace(name)]
	return plugin, ok
}

// ResolveEntrypoint returns the filesystem path that a named plugin entrypoint
// would execute. It validates the manifest and keeps the resolved path inside
// the plugin root, including through symlinks.
func (r *Registry) ResolveEntrypoint(pluginName, entrypointName string) (Entrypoint, error) {
	plugin, ok := r.Get(pluginName)
	if !ok {
		return Entrypoint{}, fmt.Errorf("plugin: %q not found", strings.TrimSpace(pluginName))
	}

	entrypointName = strings.TrimSpace(entrypointName)
	if entrypointName == "" {
		return Entrypoint{}, errors.New("plugin: empty entrypoint name")
	}
	if err := plugin.Manifest.Validate(plugin.Root); err != nil {
		return Entrypoint{}, fmt.Errorf("plugin: validate manifest: %w", err)
	}

	relativePath, ok := plugin.Manifest.Entrypoints[entrypointName]
	if !ok {
		return Entrypoint{}, fmt.Errorf("plugin: entrypoint %q not found", entrypointName)
	}
	root, target, err := resolveEntrypoint(plugin.Root, relativePath)
	if err != nil {
		return Entrypoint{}, fmt.Errorf("plugin: resolve entrypoint %q: %w", entrypointName, err)
	}

	return Entrypoint{
		PluginName:     strings.TrimSpace(plugin.Manifest.Name),
		EntrypointName: entrypointName,
		Root:           root,
		ManifestPath:   plugin.ManifestPath,
		RelativePath:   strings.TrimSpace(relativePath),
		Path:           target,
	}, nil
}

// DryRunEntrypoint describes a named plugin entrypoint without executing it.
func (r *Registry) DryRunEntrypoint(pluginName, entrypointName string) (DryRun, error) {
	entrypoint, err := r.ResolveEntrypoint(pluginName, entrypointName)
	if err != nil {
		return DryRun{}, err
	}

	description := fmt.Sprintf(
		"would run plugin %q entrypoint %q at %s with working directory %s",
		entrypoint.PluginName,
		entrypoint.EntrypointName,
		entrypoint.Path,
		entrypoint.Root,
	)
	return DryRun{Entrypoint: entrypoint, Description: description}, nil
}

func loadPlugin(path string) (Plugin, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return Plugin{}, errors.New("plugin: empty manifest path")
	}

	absPath, err := filepath.Abs(path)
	if err != nil {
		return Plugin{}, fmt.Errorf("plugin: resolve %s: %w", path, err)
	}
	info, err := os.Stat(absPath)
	if err != nil {
		return Plugin{}, fmt.Errorf("plugin: stat %s: %w", absPath, err)
	}

	root := filepath.Dir(absPath)
	manifestPath := absPath
	if info.IsDir() {
		root = absPath
		manifestPath, err = FindManifest(root)
		if err != nil {
			return Plugin{}, err
		}
	}

	manifest, err := Load(absPath)
	if err != nil {
		return Plugin{}, err
	}
	return Plugin{Manifest: manifest, Root: root, ManifestPath: manifestPath}, nil
}
