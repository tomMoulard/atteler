// Package plugin loads and validates atteler plugin manifests.
package plugin

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

var manifestFilenames = []string{"plugin.yaml", "plugin.yml", "plugin.json"}

const hardOutputLimitBytes = 1024 * 1024

var envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Manifest describes an atteler plugin.
//
//nolint:govet // Field order follows manifest readability, not memory layout.
type Manifest struct {
	Permissions    *PermissionSet            `json:"permissions,omitempty" yaml:"permissions,omitempty"`
	Output         *OutputLimits             `json:"output,omitempty" yaml:"output,omitempty"`
	Trust          *Trust                    `json:"trust,omitempty" yaml:"trust,omitempty"`
	EntrypointArgs map[string][]ArgumentSpec `json:"entrypoint_args,omitempty" yaml:"entrypoint_args,omitempty"`
	Name           string                    `json:"name" yaml:"name"`
	Version        string                    `json:"version" yaml:"version"`
	Description    string                    `json:"description,omitempty" yaml:"description,omitempty"`
	Capabilities   []string                  `json:"capabilities,omitempty" yaml:"capabilities,omitempty"`
	Entrypoints    map[string]string         `json:"entrypoints,omitempty" yaml:"entrypoints,omitempty"`
}

// PermissionSet declares the ambient resources a plugin requests before it can
// run.
//
//nolint:govet // Field order mirrors manifest readability.
type PermissionSet struct {
	Filesystem FilesystemPermissions `json:"filesystem" yaml:"filesystem"`
	Network    NetworkPermissions    `json:"network" yaml:"network"`
	Shell      ShellPermissions      `json:"shell" yaml:"shell"`
	Env        []string              `json:"env,omitempty" yaml:"env,omitempty"`
	Secrets    []string              `json:"secrets,omitempty" yaml:"secrets,omitempty"`
	Tools      []string              `json:"tools,omitempty" yaml:"tools,omitempty"`
}

// FilesystemPermissions declares plugin-root-relative read and write scopes.
type FilesystemPermissions struct {
	Read  []string `json:"read,omitempty" yaml:"read,omitempty"`
	Write []string `json:"write,omitempty" yaml:"write,omitempty"`
}

// NetworkPermissions declares whether a plugin may make network calls and to
// which hosts.
type NetworkPermissions struct {
	Hosts []string `json:"hosts,omitempty" yaml:"hosts,omitempty"`
	Allow bool     `json:"allow" yaml:"allow"`
}

// ShellPermissions declares whether a plugin may intentionally invoke a shell
// or shell-backed helper.
type ShellPermissions struct {
	Allow bool `json:"allow" yaml:"allow"`
}

// ArgumentSpec declares one positional argument accepted by an entrypoint.
type ArgumentSpec struct {
	Name     string   `json:"name" yaml:"name"`
	Pattern  string   `json:"pattern,omitempty" yaml:"pattern,omitempty"`
	Allowed  []string `json:"allowed,omitempty" yaml:"allowed,omitempty"`
	Required bool     `json:"required,omitempty" yaml:"required,omitempty"`
}

// OutputLimits declares the maximum captured stdout/stderr bytes for a plugin
// process before atteler truncates the stream.
type OutputLimits struct {
	StdoutMaxBytes int `json:"stdout_max_bytes" yaml:"stdout_max_bytes"`
	StderrMaxBytes int `json:"stderr_max_bytes" yaml:"stderr_max_bytes"`
}

// Trust records local trust provenance and lifecycle state for a plugin.
type Trust struct {
	InstallSource string       `json:"install_source" yaml:"install_source"`
	Checksum      string       `json:"checksum,omitempty" yaml:"checksum,omitempty"`
	Signature     string       `json:"signature,omitempty" yaml:"signature,omitempty"`
	Audit         []TrustAudit `json:"audit,omitempty" yaml:"audit,omitempty"`
	Enabled       bool         `json:"enabled" yaml:"enabled"`
	Revoked       bool         `json:"revoked,omitempty" yaml:"revoked,omitempty"`
}

// TrustAudit records a local trust lifecycle event for a plugin.
type TrustAudit struct {
	Action string `json:"action" yaml:"action"`
	Actor  string `json:"actor,omitempty" yaml:"actor,omitempty"`
	At     string `json:"at,omitempty" yaml:"at,omitempty"`
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

// Validate checks required manifest fields, entrypoint path containment, and
// the shape of any declared security metadata. Runtime execution requires the
// security metadata to be present and accepted by policy.
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

	if err := validateEntrypointArgs(m); err != nil {
		return err
	}

	if m.Permissions != nil {
		if err := validatePermissions(root, *m.Permissions); err != nil {
			return fmt.Errorf("permissions: %w", err)
		}
	}

	if m.Output != nil {
		if err := validateOutputLimits(*m.Output); err != nil {
			return fmt.Errorf("output: %w", err)
		}
	}

	if m.Trust != nil {
		if err := validateTrust(*m.Trust); err != nil {
			return fmt.Errorf("trust: %w", err)
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

func validateEntrypointArgs(manifest Manifest) error {
	for name, args := range manifest.EntrypointArgs {
		name = strings.TrimSpace(name)
		if name == "" {
			return errors.New("entrypoint_args has empty entrypoint name")
		}

		if _, ok := manifest.Entrypoints[name]; !ok {
			return fmt.Errorf("entrypoint_args %q has no matching entrypoint", name)
		}

		if err := validateEntrypointArgList(name, args); err != nil {
			return err
		}
	}

	return nil
}

func validateEntrypointArgList(entrypointName string, args []ArgumentSpec) error {
	seen := make(map[string]struct{}, len(args))
	seenOptional := false

	for i, arg := range args {
		arg.Name = strings.TrimSpace(arg.Name)
		if arg.Name == "" {
			return fmt.Errorf("entrypoint_args %q argument %d missing name", entrypointName, i)
		}

		if arg.Required && seenOptional {
			return fmt.Errorf("entrypoint_args %q argument %q required after optional argument", entrypointName, arg.Name)
		}

		if !arg.Required {
			seenOptional = true
		}

		if _, ok := seen[arg.Name]; ok {
			return fmt.Errorf("entrypoint_args %q duplicate argument %q", entrypointName, arg.Name)
		}

		seen[arg.Name] = struct{}{}

		if strings.TrimSpace(arg.Pattern) != "" {
			if _, err := regexp.Compile(arg.Pattern); err != nil {
				return fmt.Errorf("entrypoint_args %q argument %q pattern: %w", entrypointName, arg.Name, err)
			}
		}
	}

	return nil
}

func validatePermissions(root string, permissions PermissionSet) error {
	if err := validateScopes(root, "filesystem.read", permissions.Filesystem.Read); err != nil {
		return err
	}

	if err := validateScopes(root, "filesystem.write", permissions.Filesystem.Write); err != nil {
		return err
	}

	if err := validateNetworkPermissions(permissions.Network); err != nil {
		return err
	}

	if err := validateEnvNames("env", permissions.Env); err != nil {
		return err
	}

	if err := validateEnvNames("secrets", permissions.Secrets); err != nil {
		return err
	}

	if err := validateUniqueNonEmpty("tools", permissions.Tools); err != nil {
		return err
	}

	return nil
}

func validateScopes(root, field string, scopes []string) error {
	seen := make(map[string]struct{}, len(scopes))
	for _, scope := range scopes {
		scope = strings.TrimSpace(scope)
		if scope == "" {
			return fmt.Errorf("%s has empty scope", field)
		}

		if _, ok := seen[scope]; ok {
			return fmt.Errorf("%s duplicate scope %q", field, scope)
		}

		seen[scope] = struct{}{}

		if err := validatePathInRoot(root, scope); err != nil {
			return fmt.Errorf("%s %q: %w", field, scope, err)
		}
	}

	return nil
}

func validateNetworkPermissions(network NetworkPermissions) error {
	if !network.Allow && len(network.Hosts) > 0 {
		return errors.New("network hosts require allow: true")
	}

	if network.Allow && len(network.Hosts) == 0 {
		return errors.New("network allow requires at least one host or \"*\"")
	}

	return validateUniqueNonEmpty("network.hosts", network.Hosts)
}

func validateEnvNames(field string, names []string) error {
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			return fmt.Errorf("%s has empty name", field)
		}

		if !envNamePattern.MatchString(name) {
			return fmt.Errorf("%s %q is not a valid environment variable name", field, name)
		}

		if _, ok := seen[name]; ok {
			return fmt.Errorf("%s duplicate name %q", field, name)
		}

		seen[name] = struct{}{}
	}

	return nil
}

func validateUniqueNonEmpty(field string, values []string) error {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			return fmt.Errorf("%s has empty value", field)
		}

		if _, ok := seen[value]; ok {
			return fmt.Errorf("%s duplicate value %q", field, value)
		}

		seen[value] = struct{}{}
	}

	return nil
}

func validateOutputLimits(output OutputLimits) error {
	if output.StdoutMaxBytes <= 0 {
		return errors.New("stdout_max_bytes must be positive")
	}

	if output.StderrMaxBytes <= 0 {
		return errors.New("stderr_max_bytes must be positive")
	}

	if output.StdoutMaxBytes > hardOutputLimitBytes {
		return fmt.Errorf("stdout_max_bytes exceeds hard limit %d", hardOutputLimitBytes)
	}

	if output.StderrMaxBytes > hardOutputLimitBytes {
		return fmt.Errorf("stderr_max_bytes exceeds hard limit %d", hardOutputLimitBytes)
	}

	return nil
}

func validateTrust(trust Trust) error {
	for i, event := range trust.Audit {
		if strings.TrimSpace(event.Action) == "" {
			return fmt.Errorf("audit event %d missing action", i)
		}
	}

	return nil
}

func validatePathInRoot(root, relativePath string) error {
	relativePath = strings.TrimSpace(relativePath)
	if relativePath == "" {
		return errors.New("empty path")
	}

	if filepath.IsAbs(relativePath) {
		return fmt.Errorf("path %q escapes plugin root %q", relativePath, root)
	}

	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("resolve plugin root: %w", err)
	}

	targetAbs, err := filepath.Abs(filepath.Join(rootAbs, relativePath))
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}

	rel, err := filepath.Rel(rootAbs, targetAbs)
	if err != nil {
		return fmt.Errorf("compare with plugin root: %w", err)
	}

	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("path %q escapes plugin root %q", relativePath, root)
	}

	return nil
}
