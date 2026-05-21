package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	attelerplugin "github.com/tommoulard/atteler/pkg/plugin"
)

func listPlugins(paths []string) error {
	if len(paths) == 0 {
		fmt.Println("No plugins configured.")
		return nil
	}

	for _, path := range paths {
		manifest, err := attelerplugin.Load(path)
		if err != nil {
			return fmt.Errorf("list plugins: %w", err)
		}

		parts := []string{manifest.Name, manifest.Version}
		if len(manifest.Capabilities) > 0 {
			parts = append(parts, "capabilities="+strings.Join(manifest.Capabilities, ","))
		}

		if manifest.Description != "" {
			parts = append(parts, "description="+manifest.Description)
		}

		parts = append(parts, path)
		fmt.Println(strings.Join(parts, "\t"))
	}

	return nil
}

//nolint:govet // YAML readability is more important than pointer-byte packing here.
type pluginDescription struct {
	Permissions    *attelerplugin.PermissionSet            `yaml:"permissions,omitempty"`
	Output         *attelerplugin.OutputLimits             `yaml:"output,omitempty"`
	Trust          *attelerplugin.Trust                    `yaml:"trust,omitempty"`
	EntrypointArgs map[string][]attelerplugin.ArgumentSpec `yaml:"entrypoint_args,omitempty"`
	Entrypoints    map[string]string                       `yaml:"entrypoints,omitempty"`
	Capabilities   []string                                `yaml:"capabilities,omitempty"`
	Name           string                                  `yaml:"name"`
	Version        string                                  `yaml:"version"`
	Description    string                                  `yaml:"description,omitempty"`
	Root           string                                  `yaml:"root"`
	ManifestPath   string                                  `yaml:"manifest_path"`
}

func describePlugin(paths []string, name string) error {
	registry, err := attelerplugin.NewRegistry(paths)
	if err != nil {
		return fmt.Errorf("describe plugin: %w", err)
	}

	plugin, ok := registry.Get(name)
	if !ok {
		return fmt.Errorf("describe plugin: plugin %q not found", strings.TrimSpace(name))
	}

	out, err := formatPluginDescription(plugin)
	if err != nil {
		return fmt.Errorf("describe plugin: marshal %q: %w", name, err)
	}

	fmt.Print(out)

	return nil
}

func formatPluginDescription(plugin attelerplugin.Plugin) (string, error) {
	out, err := yaml.Marshal(pluginDescription{
		Name:         plugin.Manifest.Name,
		Version:      plugin.Manifest.Version,
		Description:  plugin.Manifest.Description,
		Capabilities: append([]string(nil), plugin.Manifest.Capabilities...),
		Entrypoints:  copyStringMap(plugin.Manifest.Entrypoints),
		EntrypointArgs: copyEntrypointArgsMap(
			plugin.Manifest.EntrypointArgs,
		),
		Permissions:  copyPermissions(plugin.Manifest.Permissions),
		Output:       copyOutputLimits(plugin.Manifest.Output),
		Trust:        copyTrust(plugin.Manifest.Trust),
		Root:         plugin.Root,
		ManifestPath: plugin.ManifestPath,
	})
	if err != nil {
		return "", fmt.Errorf("marshal plugin description: %w", err)
	}

	return string(out), nil
}

func initRTKPlugin(dir string) error {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return errors.New("init rtk plugin: directory is required")
	}

	files := map[string]rtkPluginFile{
		"plugin.yaml": {
			mode: 0o600,
			content: `name: rtk
version: "0.1.0"
description: RTK token-saving CLI proxy helpers for Atteler.
capabilities:
  - rtk
  - shell-output
  - token-optimization
entrypoints:
  version: bin/version
  gain: bin/gain
  show: bin/show
  init-codex: bin/init-codex
entrypoint_args:
  version: []
  gain: []
  show: []
  init-codex: []
permissions:
  filesystem:
    read:
      - "."
    write: []
  network:
    allow: false
    hosts: []
  shell:
    allow: true
  env:
    - PATH
  secrets: []
  tools:
    - rtk
output:
  stdout_max_bytes: 65536
  stderr_max_bytes: 65536
trust:
  enabled: true
  install_source: atteler plugins init-rtk
  checksum: generated-local-scaffold
  revoked: false
  audit:
    - action: accepted
      actor: atteler
      at: scaffold
`,
		},
		"bin/version": {
			mode:    0o700,
			content: "#!/bin/sh\nexec rtk --version \"$@\"\n",
		},
		"bin/gain": {
			mode:    0o700,
			content: "#!/bin/sh\nexec rtk gain \"$@\"\n",
		},
		"bin/show": {
			mode:    0o700,
			content: "#!/bin/sh\nexec rtk init --show \"$@\"\n",
		},
		"bin/init-codex": {
			mode:    0o700,
			content: "#!/bin/sh\nexec rtk init -g --codex \"$@\"\n",
		},
	}

	for name, file := range files {
		path := filepath.Join(dir, name)
		if err := writeRTKPluginFile(path, file.content, file.mode); err != nil {
			return err
		}
	}

	fmt.Println("RTK plugin written to " + dir)
	fmt.Println("Add this to your atteler config:")
	fmt.Println(rtkPluginConfigSnippet(dir))
	fmt.Println("Then run: atteler --run-plugin rtk/version")

	return nil
}

func rtkPluginConfigSnippet(dir string) string {
	return `plugins:
  paths: [` + strconv.Quote(dir) + `]
  policy:
    permissions:
      filesystem:
        read:
          - "."
        write: []
      network:
        allow: false
        hosts: []
      shell:
        allow: true
      env:
        - PATH
      secrets: []
      tools:
        - rtk
    output:
      stdout_max_bytes: 65536
      stderr_max_bytes: 65536
    trusted_install_sources:
      - atteler plugins init-rtk`
}

type rtkPluginFile struct {
	content string
	mode    os.FileMode
}

func writeRTKPluginFile(path, content string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("init rtk plugin: create dir: %w", err)
	}

	if existing, err := os.ReadFile(path); err == nil {
		if string(existing) != content {
			return fmt.Errorf("init rtk plugin: refusing to overwrite modified file %s", path)
		}

		if chmodErr := os.Chmod(path, mode); chmodErr != nil {
			return fmt.Errorf("init rtk plugin: chmod %s: %w", path, chmodErr)
		}

		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("init rtk plugin: read %s: %w", path, err)
	}

	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		return fmt.Errorf("init rtk plugin: write %s: %w", path, err)
	}

	return nil
}

func runPluginEntrypoint(
	ctx context.Context,
	paths []string,
	policy *attelerplugin.Policy,
	target, entrypointName string,
	dryRun bool,
	timeoutSeconds int,
) error {
	pluginName, entrypointName, err := parsePluginTarget(target, entrypointName)
	if err != nil {
		return err
	}

	registry, err := attelerplugin.NewRegistry(paths)
	if err != nil {
		return fmt.Errorf("run plugin: %w", err)
	}

	if dryRun {
		preview, previewErr := registry.DryRunEntrypoint(pluginName, entrypointName)
		if previewErr != nil {
			return fmt.Errorf("run plugin: %w", previewErr)
		}

		fmt.Println(formatPluginDryRun(preview))

		return nil
	}

	plugin, ok := registry.Get(pluginName)
	if !ok {
		return fmt.Errorf("run plugin: plugin %q not found", pluginName)
	}

	timeout := time.Duration(timeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	if policy == nil {
		return errors.New("run plugin: plugins.policy must accept requested permissions before execution")
	}

	acceptedPolicy := attelerplugin.ClonePolicy(*policy)

	result, err := attelerplugin.RunEntrypointWithOptions(ctx, plugin.Root, plugin.Manifest, entrypointName, attelerplugin.RunOptions{
		Policy:  &acceptedPolicy,
		Timeout: timeout,
	})
	if result.Stdout != "" {
		fmt.Print(result.Stdout)
	}

	if result.Stderr != "" {
		fmt.Fprint(os.Stderr, result.Stderr)
	}

	if err != nil {
		return fmt.Errorf("run plugin: %w", err)
	}

	return nil
}

func parsePluginTarget(target, entrypointName string) (pluginName, entrypoint string, err error) {
	target = strings.TrimSpace(target)
	entrypointName = strings.TrimSpace(entrypointName)

	if target == "" {
		return "", "", errors.New("run plugin: plugin name is required")
	}

	if entrypointName != "" {
		return target, entrypointName, nil
	}

	pluginName, entrypoint, ok := strings.Cut(target, "/")
	if !ok || strings.TrimSpace(pluginName) == "" || strings.TrimSpace(entrypoint) == "" {
		return "", "", errors.New("run plugin: pass --plugin-entrypoint or use plugin/entrypoint")
	}

	return strings.TrimSpace(pluginName), strings.TrimSpace(entrypoint), nil
}

func formatPluginDryRun(dryRun attelerplugin.DryRun) string {
	entrypoint := dryRun.Entrypoint

	return strings.Join([]string{
		dryRun.Description,
		"plugin=" + entrypoint.PluginName,
		"entrypoint=" + entrypoint.EntrypointName,
		"path=" + entrypoint.Path,
		"cwd=" + entrypoint.Root,
	}, "\n")
}

func copyStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}

	out := make(map[string]string, len(in))
	maps.Copy(out, in)

	return out
}

func copyEntrypointArgsMap(in map[string][]attelerplugin.ArgumentSpec) map[string][]attelerplugin.ArgumentSpec {
	if len(in) == 0 {
		return nil
	}

	out := make(map[string][]attelerplugin.ArgumentSpec, len(in))
	for name, args := range in {
		out[name] = append([]attelerplugin.ArgumentSpec(nil), args...)
	}

	return out
}

func copyPermissions(in *attelerplugin.PermissionSet) *attelerplugin.PermissionSet {
	if in == nil {
		return nil
	}

	out := *in
	out.Filesystem.Read = append([]string(nil), in.Filesystem.Read...)
	out.Filesystem.Write = append([]string(nil), in.Filesystem.Write...)
	out.Network.Hosts = append([]string(nil), in.Network.Hosts...)
	out.Env = append([]string(nil), in.Env...)
	out.Secrets = append([]string(nil), in.Secrets...)
	out.Tools = append([]string(nil), in.Tools...)

	return &out
}

func copyOutputLimits(in *attelerplugin.OutputLimits) *attelerplugin.OutputLimits {
	if in == nil {
		return nil
	}

	out := *in

	return &out
}

func copyTrust(in *attelerplugin.Trust) *attelerplugin.Trust {
	if in == nil {
		return nil
	}

	out := *in
	out.Audit = append([]attelerplugin.TrustAudit(nil), in.Audit...)

	return &out
}

func clonePluginPolicy(in *attelerplugin.Policy) *attelerplugin.Policy {
	if in == nil {
		return nil
	}

	out := attelerplugin.ClonePolicy(*in)

	return &out
}
