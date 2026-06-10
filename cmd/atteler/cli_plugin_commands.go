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

	"github.com/tommoulard/atteler/pkg/autonomy"
	"github.com/tommoulard/atteler/pkg/permission"
	attelerplugin "github.com/tommoulard/atteler/pkg/plugin"
)

func listPlugins(ctx context.Context, paths []string) error {
	if len(paths) == 0 {
		fmt.Println("No plugins configured.")
		return nil
	}

	if err := authorizePluginManifestReads(ctx, "list plugin manifests", paths); err != nil {
		return err
	}

	for _, path := range paths {
		manifest, err := attelerplugin.LoadContext(ctx, path)
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
	Permissions           *attelerplugin.PermissionSet                `yaml:"permissions,omitempty"`
	Output                *attelerplugin.OutputLimits                 `yaml:"output,omitempty"`
	Trust                 *attelerplugin.Trust                        `yaml:"trust,omitempty"`
	Provenance            *attelerplugin.Provenance                   `yaml:"provenance,omitempty"`
	EntrypointArgs        map[string][]attelerplugin.ArgumentSpec     `yaml:"entrypoint_args,omitempty"`
	EntrypointContracts   map[string]attelerplugin.EntrypointContract `yaml:"entrypoint_contracts,omitempty"`
	Entrypoints           map[string]string                           `yaml:"entrypoints,omitempty"`
	Capabilities          []string                                    `yaml:"capabilities,omitempty"`
	Name                  string                                      `yaml:"name"`
	Version               string                                      `yaml:"version"`
	MinimumAttelerVersion string                                      `yaml:"min_atteler_version,omitempty"`
	Description           string                                      `yaml:"description,omitempty"`
	Root                  string                                      `yaml:"root"`
	ManifestPath          string                                      `yaml:"manifest_path"`
}

func describePlugin(ctx context.Context, paths []string, name string) error {
	if err := authorizePluginManifestReads(ctx, "describe plugin manifest", paths); err != nil {
		return err
	}

	registry, err := attelerplugin.NewRegistryContext(ctx, paths)
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
		Name:                  plugin.Manifest.Name,
		Version:               plugin.Manifest.Version,
		MinimumAttelerVersion: plugin.Manifest.MinimumAttelerVersion,
		Description:           plugin.Manifest.Description,
		Capabilities:          append([]string(nil), plugin.Manifest.Capabilities...),
		Entrypoints:           copyStringMap(plugin.Manifest.Entrypoints),
		EntrypointArgs: copyEntrypointArgsMap(
			plugin.Manifest.EntrypointArgs,
		),
		EntrypointContracts: copyEntrypointContractsMap(plugin.Manifest.EntrypointContracts),
		Permissions:         copyPermissions(plugin.Manifest.Permissions),
		Output:              copyOutputLimits(plugin.Manifest.Output),
		Trust:               copyTrust(plugin.Manifest.Trust),
		Provenance:          copyProvenance(plugin.Manifest.Provenance),
		Root:                plugin.Root,
		ManifestPath:        plugin.ManifestPath,
	})
	if err != nil {
		return "", fmt.Errorf("marshal plugin description: %w", err)
	}

	return string(out), nil
}

func initRTKPlugin(ctx context.Context, dir string) error {
	if ctx == nil {
		return errors.New("init rtk plugin: context is required")
	}

	dir = strings.TrimSpace(dir)
	if dir == "" {
		return errors.New("init rtk plugin: directory is required")
	}

	if err := authorizeInitRTKPlugin(ctx, dir); err != nil {
		return err
	}

	files := map[string]rtkPluginFile{
		"plugin.yaml": {
			mode: 0o600,
			content: `name: rtk
version: "0.1.0"
min_atteler_version: "0.1.0"
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
entrypoint_contracts:
  version:
    output:
      format: text
  gain:
    output:
      format: text
  show:
    output:
      format: text
  init-codex:
    output:
      format: text
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
provenance:
  source: atteler plugins init-rtk
  digest: generated-local-scaffold
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
		if err := writeRTKPluginFile(ctx, path, file.content, file.mode); err != nil {
			return err
		}
	}

	fmt.Println("RTK plugin written to " + dir)
	fmt.Println("Add this to your atteler config:")
	fmt.Println(rtkPluginConfigSnippet(dir))
	fmt.Println("Then run: atteler --run-plugin rtk/version")

	return nil
}

func authorizeInitRTKPlugin(ctx context.Context, dir string) error {
	decision := permission.Evaluate(ctx, nil, permission.Request{
		Action: "initialize RTK plugin scaffold",
		Source: "atteler.plugins.init_rtk",
		Target: dir,
		Operations: []permission.Operation{{
			Kind:   permission.OperationWrite,
			Action: "initialize RTK plugin scaffold",
			Source: "atteler.plugins.init_rtk",
			Target: dir,
		}},
	})
	if decision.Allowed {
		return nil
	}

	return &permission.Error{Decision: decision}
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

func writeRTKPluginFile(ctx context.Context, path, content string, mode os.FileMode) error {
	if err := authorizeReadPermission(ctx, "inspect RTK plugin scaffold file", "atteler.plugins.init_rtk", path); err != nil {
		return fmt.Errorf("init rtk plugin: authorize file inspection: %w", err)
	}

	if existing, err := os.ReadFile(path); err == nil {
		if string(existing) != content {
			return fmt.Errorf("init rtk plugin: refusing to overwrite modified file %s", path)
		}

		if authErr := authorizeWritePermission(ctx, "update RTK plugin scaffold file mode", "atteler.plugins.init_rtk", path); authErr != nil {
			return fmt.Errorf("init rtk plugin: authorize file mode update: %w", authErr)
		}

		if chmodErr := os.Chmod(path, mode); chmodErr != nil {
			return fmt.Errorf("init rtk plugin: chmod %s: %w", path, chmodErr)
		}

		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("init rtk plugin: read %s: %w", path, err)
	}

	if err := authorizeWritePermission(ctx, "write RTK plugin scaffold file", "atteler.plugins.init_rtk", path); err != nil {
		return fmt.Errorf("init rtk plugin: authorize file write: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("init rtk plugin: create dir: %w", err)
	}

	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		return fmt.Errorf("init rtk plugin: write %s: %w", path, err)
	}

	return nil
}

//nolint:cyclop // CLI plugin execution coordinates dry-run, policy, autonomy, and output paths.
func runPluginEntrypoint(
	ctx context.Context,
	paths []string,
	policy *attelerplugin.Policy,
	permissionPolicy *permission.Policy,
	target, entrypointName string,
	dryRun bool,
	timeoutSeconds int,
	level autonomy.Level,
) error {
	pluginName, entrypointName, err := parsePluginTarget(target, entrypointName)
	if err != nil {
		return err
	}

	if authErr := authorizePluginManifestReads(ctx, "run plugin manifest lookup", paths); authErr != nil {
		return authErr
	}

	registry, err := attelerplugin.NewRegistryContext(ctx, paths)
	if err != nil {
		return fmt.Errorf("run plugin: %w", err)
	}

	var acceptedPolicy *attelerplugin.Policy

	if policy != nil {
		clonedPolicy := attelerplugin.ClonePolicy(*policy)
		acceptedPolicy = &clonedPolicy
	}

	if dryRun {
		if acceptedPolicy == nil {
			if _, resolveErr := registry.ResolveEntrypoint(pluginName, entrypointName); resolveErr != nil {
				return fmt.Errorf("run plugin: %w", resolveErr)
			}

			return errors.New("run plugin: plugins.policy must accept requested permissions before execution")
		}

		preview, previewErr := registry.DryRunEntrypointWithOptions(ctx, pluginName, entrypointName, attelerplugin.DryRunOptions{
			Policy:                acceptedPolicy,
			Permission:            permissionPolicy,
			AttelerVersion:        version,
			RequireAcceptedPolicy: true,
		})
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

	if acceptedPolicy == nil {
		return errors.New("run plugin: plugins.policy must accept requested permissions before execution")
	}

	if authErr := authorizePluginRunAutonomy(level, plugin.Manifest); authErr != nil {
		return authErr
	}

	result, err := attelerplugin.RunEntrypointWithOptions(ctx, plugin.Root, plugin.Manifest, entrypointName, attelerplugin.RunOptions{
		Policy:         acceptedPolicy,
		Permission:     permissionPolicy,
		Timeout:        timeout,
		AttelerVersion: version,
		Autonomy:       autonomy.Normalize(level).String(),
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

func authorizePluginManifestReads(ctx context.Context, action string, paths []string) error {
	for _, path := range paths {
		if err := authorizeReadPermission(ctx, action, "atteler.plugins", path); err != nil {
			return fmt.Errorf("%s: %w", strings.TrimSpace(action), err)
		}
	}

	return nil
}

func authorizePluginRunAutonomy(level autonomy.Level, manifest attelerplugin.Manifest) error {
	level = autonomy.Normalize(level)
	if !level.Allows(autonomy.ActionMutatingShell) {
		return fmt.Errorf("%s", autonomy.DenialMessage(level, autonomy.ActionMutatingShell, "--run-plugin"))
	}

	if manifest.Permissions != nil &&
		manifest.Permissions.Network.Allow &&
		!level.Allows(autonomy.ActionRemoteMutation) {
		return fmt.Errorf(
			"%s",
			autonomy.DenialMessage(level, autonomy.ActionRemoteMutation, "--run-plugin network permissions"),
		)
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

	outputFormat := attelerplugin.OutputFormatText
	outputSchema := false

	if dryRun.Contract != nil && dryRun.Contract.Output != nil {
		outputFormat = strings.TrimSpace(dryRun.Contract.Output.Format)
		if outputFormat == "" {
			outputFormat = attelerplugin.OutputFormatText
			if dryRun.Contract.Output.Schema != nil {
				outputFormat = attelerplugin.OutputFormatJSON
			}
		}

		outputSchema = dryRun.Contract.Output.Schema != nil
	}

	lines := []string{
		dryRun.Description,
		"plugin=" + entrypoint.PluginName,
		"entrypoint=" + entrypoint.EntrypointName,
		"path=" + entrypoint.Path,
		"cwd=" + entrypoint.Root,
		"policy_checked=" + strconv.FormatBool(dryRun.PolicyChecked),
		"output_format=" + outputFormat,
		"output_schema=" + strconv.FormatBool(outputSchema),
	}

	if strings.TrimSpace(dryRun.MinimumAttelerVersion) != "" {
		lines = append(lines, "min_atteler_version="+strings.TrimSpace(dryRun.MinimumAttelerVersion))
	}

	if dryRun.Output != nil {
		lines = append(lines, fmt.Sprintf(
			"output_limits=stdout:%d,stderr:%d",
			dryRun.Output.StdoutMaxBytes,
			dryRun.Output.StderrMaxBytes,
		))
	}

	if dryRun.Permissions != nil {
		lines = append(lines, "permissions="+formatPluginDryRunPermissions(*dryRun.Permissions))
	}

	if dryRun.Contract != nil {
		argCount, requiredCount := pluginDryRunArgCounts(dryRun.Contract.Inputs.Args)
		lines = append(lines, fmt.Sprintf("input_args=%d,required:%d", argCount, requiredCount))
	}

	return strings.Join(lines, "\n")
}

func pluginDryRunArgCounts(args []attelerplugin.ArgumentSpec) (argCount, requiredCount int) {
	for _, arg := range args {
		argCount++

		if arg.Required {
			requiredCount++
		}
	}

	return argCount, requiredCount
}

func formatPluginDryRunPermissions(permissions attelerplugin.PermissionSet) string {
	parts := []string{
		"filesystem.read=" + formatPluginDryRunList(permissions.Filesystem.Read),
		"filesystem.write=" + formatPluginDryRunList(permissions.Filesystem.Write),
		"network.allow=" + strconv.FormatBool(permissions.Network.Allow),
		"network.hosts=" + formatPluginDryRunList(permissions.Network.Hosts),
		"shell.allow=" + strconv.FormatBool(permissions.Shell.Allow),
		"env=" + formatPluginDryRunList(permissions.Env),
		"secrets=" + formatPluginDryRunList(permissions.Secrets),
		"tools=" + formatPluginDryRunList(permissions.Tools),
	}

	return strings.Join(parts, ";")
}

func formatPluginDryRunList(values []string) string {
	if len(values) == 0 {
		return "[]"
	}

	return "[" + strings.Join(values, ",") + "]"
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

func copyEntrypointContractsMap(
	in map[string]attelerplugin.EntrypointContract,
) map[string]attelerplugin.EntrypointContract {
	if len(in) == 0 {
		return nil
	}

	out := make(map[string]attelerplugin.EntrypointContract, len(in))
	for name, contract := range in {
		out[name] = copyEntrypointContract(contract)
	}

	return out
}

func copyEntrypointContract(in attelerplugin.EntrypointContract) attelerplugin.EntrypointContract {
	out := in

	out.Inputs.Args = append([]attelerplugin.ArgumentSpec(nil), in.Inputs.Args...)
	if in.Output != nil {
		output := *in.Output
		if in.Output.Schema != nil {
			schema := *in.Output.Schema

			schema.Required = append([]string(nil), in.Output.Schema.Required...)
			if len(in.Output.Schema.Properties) > 0 {
				schema.Properties = make(map[string]attelerplugin.JSONSchemaProperty, len(in.Output.Schema.Properties))
				maps.Copy(schema.Properties, in.Output.Schema.Properties)
			}

			output.Schema = &schema
		}

		out.Output = &output
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

func copyProvenance(in *attelerplugin.Provenance) *attelerplugin.Provenance {
	if in == nil {
		return nil
	}

	out := *in

	return &out
}

func clonePluginPolicy(in *attelerplugin.Policy) *attelerplugin.Policy {
	if in == nil {
		return nil
	}

	out := attelerplugin.ClonePolicy(*in)

	return &out
}
