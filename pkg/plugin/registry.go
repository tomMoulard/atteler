package plugin

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"

	"github.com/tommoulard/atteler/pkg/permission"
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
	Entrypoint            Entrypoint
	Contract              *EntrypointContract
	Permissions           *PermissionSet
	Output                *OutputLimits
	Description           string
	MinimumAttelerVersion string
	PolicyChecked         bool
}

// DryRunOptions controls policy and input validation for dry-run previews.
type DryRunOptions struct {
	Policy         *Policy
	Permission     *permission.Policy
	AttelerVersion string
	Args           []string
	// RequireAcceptedPolicy makes dry-run fail the same way execution would
	// when no accepted plugin policy is available.
	RequireAcceptedPolicy bool
}

type dryRunPermissionContext struct {
	ctx context.Context
	ok  bool
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

// LoadRegistryContext loads all configured plugin paths through Atteler's
// central permission policy.
func LoadRegistryContext(ctx context.Context, paths []string) (*Registry, error) {
	registry := &Registry{plugins: make(map[string]Plugin, len(paths))}
	for _, path := range paths {
		plugin, err := loadPluginContext(ctx, path)
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

// NewRegistryContext loads all configured plugin paths through Atteler's
// central permission policy.
func NewRegistryContext(ctx context.Context, paths []string) (*Registry, error) {
	return LoadRegistryContext(ctx, paths)
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

// ResolveEntrypointContext is ResolveEntrypoint with an explicit central
// permission gate around filesystem inspection needed to resolve symlinks.
func (r *Registry) ResolveEntrypointContext(ctx context.Context, policy *permission.Policy, pluginName, entrypointName string) (Entrypoint, error) {
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

	root, target, err := authorizeAndResolveEntrypoint(ctx, policy, plugin.Root, plugin.Manifest, relativePath)
	if err != nil {
		if permission.ErrDenied(err) {
			return Entrypoint{}, fmt.Errorf("plugin: authorize entrypoint %q: %w", entrypointName, err)
		}

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
	return r.dryRunEntrypoint(pluginName, entrypointName, DryRunOptions{}, dryRunPermissionContext{})
}

// DryRunEntrypointWithOptions describes a named plugin entrypoint without
// executing it. When a policy is supplied through options or context, it runs
// the same manifest, compatibility, permissions, and argument gates used by
// execution.
func (r *Registry) DryRunEntrypointWithOptions(
	ctx context.Context,
	pluginName, entrypointName string,
	options DryRunOptions,
) (DryRun, error) {
	return r.dryRunEntrypoint(pluginName, entrypointName, options, dryRunPermissionContext{
		ctx: ctx,
		ok:  true,
	})
}

func (r *Registry) dryRunEntrypoint(
	pluginName, entrypointName string,
	options DryRunOptions,
	permissionContext dryRunPermissionContext,
) (DryRun, error) {
	entrypoint, err := r.ResolveEntrypoint(pluginName, entrypointName)
	if err != nil {
		return DryRun{}, err
	}

	plugin, _ := r.Get(pluginName)

	policyChecked := options.Policy != nil
	if options.RequireAcceptedPolicy && options.Policy == nil {
		return DryRun{}, fmt.Errorf("plugin: authorize entrypoint %q: accepted policy must be provided", entrypoint.EntrypointName)
	}

	if options.Policy != nil {
		accepted := ClonePolicy(*options.Policy)
		if err := authorizeRun(plugin.Root, plugin.Manifest, entrypoint.EntrypointName, accepted, options.AttelerVersion); err != nil {
			return DryRun{}, err
		}

		if err := authorizeEntrypointRuntimeShape(entrypoint.Path, plugin.Manifest); err != nil {
			return DryRun{}, fmt.Errorf("plugin: authorize entrypoint %q: %w", entrypoint.EntrypointName, err)
		}

		argSchema, _ := entrypointArgsFor(plugin.Manifest, entrypoint.EntrypointName)
		if _, err := validateRunArgs(entrypoint.EntrypointName, argSchema, options.Args); err != nil {
			return DryRun{}, err
		}
	}

	hasPermissionPolicy := options.Permission != nil ||
		(permissionContext.ok && permission.PolicyFromContext(permissionContext.ctx) != nil)
	if hasPermissionPolicy {
		if err := authorizeDryRunCentralPermission(permissionContext, plugin, entrypoint, options); err != nil {
			return DryRun{}, err
		}

		policyChecked = true
	}

	description := fmt.Sprintf(
		"would run plugin %q entrypoint %q at %s with working directory %s; policy_checked=%t",
		entrypoint.PluginName,
		entrypoint.EntrypointName,
		entrypoint.Path,
		entrypoint.Root,
		policyChecked,
	)

	var contract *EntrypointContract

	if value, ok := plugin.Manifest.EntrypointContracts[entrypoint.EntrypointName]; ok {
		contractCopy := copyEntrypointContract(value)
		contract = &contractCopy
	}

	return DryRun{
		Entrypoint:            entrypoint,
		Contract:              contract,
		Permissions:           copyPermissions(plugin.Manifest.Permissions),
		Output:                copyOutputLimits(plugin.Manifest.Output),
		Description:           description,
		MinimumAttelerVersion: plugin.Manifest.MinimumAttelerVersion,
		PolicyChecked:         policyChecked,
	}, nil
}

func authorizeDryRunCentralPermission(
	permissionContext dryRunPermissionContext,
	plugin Plugin,
	entrypoint Entrypoint,
	options DryRunOptions,
) error {
	if !permissionContext.ok || permissionContext.ctx == nil {
		return errors.New("plugin: context is required")
	}

	permissionOps := pluginPermissionOperations(plugin.Manifest, entrypoint.EntrypointName, entrypoint.Path)

	commandOps := permission.CommandOperations(
		entrypoint.Path,
		options.Args,
		"",
		entrypoint.Root,
		pluginPermissionSource(plugin.Manifest.Name, entrypoint.EntrypointName),
	)
	for i := range commandOps {
		commandOps[i].Action = pluginPermissionAction(plugin.Manifest.Name, entrypoint.EntrypointName)
	}

	permissionOps = append(permissionOps, commandOps...)

	permissionDecision := permission.Evaluate(permissionContext.ctx, options.Permission, permission.Request{
		Operations: permissionOps,
		Action:     pluginPermissionAction(plugin.Manifest.Name, entrypoint.EntrypointName),
		Source:     pluginPermissionSource(plugin.Manifest.Name, entrypoint.EntrypointName),
		Target:     entrypoint.Root,
	})
	if !permissionDecision.Allowed {
		return fmt.Errorf("%s: %w", permissionDecision.Rule, &permission.Error{Decision: permissionDecision})
	}

	return nil
}

// DryRunEntrypointContext describes a named plugin entrypoint without executing
// it, while still gating entrypoint filesystem inspection.
func (r *Registry) DryRunEntrypointContext(ctx context.Context, policy *permission.Policy, pluginName, entrypointName string) (DryRun, error) {
	entrypoint, err := r.ResolveEntrypointContext(ctx, policy, pluginName, entrypointName)
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

func copyPermissions(in *PermissionSet) *PermissionSet {
	if in == nil {
		return nil
	}

	out := clonePermissionSet(*in)

	return &out
}

func copyOutputLimits(in *OutputLimits) *OutputLimits {
	if in == nil {
		return nil
	}

	out := *in

	return &out
}

func copyEntrypointContract(in EntrypointContract) EntrypointContract {
	out := in
	out.Inputs.Args = append([]ArgumentSpec(nil), in.Inputs.Args...)

	if in.Output == nil {
		return out
	}

	output := *in.Output
	if in.Output.Schema != nil {
		schema := *in.Output.Schema
		schema.Required = append([]string(nil), in.Output.Schema.Required...)

		if len(in.Output.Schema.Properties) > 0 {
			schema.Properties = make(map[string]JSONSchemaProperty, len(in.Output.Schema.Properties))
			maps.Copy(schema.Properties, in.Output.Schema.Properties)
		}

		output.Schema = &schema
	}

	out.Output = &output

	return out
}

func loadPluginContext(ctx context.Context, path string) (Plugin, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return Plugin{}, errors.New("plugin: empty manifest path")
	}

	absPath, err := filepath.Abs(path)
	if err != nil {
		return Plugin{}, fmt.Errorf("plugin: resolve %s: %w", path, err)
	}

	info, err := statManifestPath(ctx, absPath)
	if err != nil {
		return Plugin{}, err
	}

	root := filepath.Dir(absPath)

	manifestPath := absPath
	if info.IsDir() {
		root = absPath

		manifestPath, err = FindManifestContext(ctx, root)
		if err != nil {
			return Plugin{}, err
		}
	}

	manifest, err := LoadFileContext(ctx, manifestPath)
	if err != nil {
		return Plugin{}, err
	}

	return Plugin{Manifest: manifest, Root: root, ManifestPath: manifestPath}, nil
}
