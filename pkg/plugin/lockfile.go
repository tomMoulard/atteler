package plugin

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// LockfileSchemaVersion identifies the installed-plugin lockfile format.
const LockfileSchemaVersion = 1

// Lockfile records the plugin versions and provenance Atteler resolved from
// configured plugin paths.
type Lockfile struct {
	Plugins []LockedPlugin `json:"plugins" yaml:"plugins"`
	Version int            `json:"version" yaml:"version"`
}

// LockedPlugin is the lockfile entry for one installed plugin.
//
//nolint:govet // Field order follows serialized lockfile readability.
type LockedPlugin struct {
	Capabilities          []string `json:"capabilities,omitempty" yaml:"capabilities,omitempty"`
	Name                  string   `json:"name" yaml:"name"`
	Version               string   `json:"version" yaml:"version"`
	Root                  string   `json:"root" yaml:"root"`
	ManifestPath          string   `json:"manifest_path" yaml:"manifest_path"`
	InstallSource         string   `json:"install_source,omitempty" yaml:"install_source,omitempty"`
	Checksum              string   `json:"checksum,omitempty" yaml:"checksum,omitempty"`
	Signature             string   `json:"signature,omitempty" yaml:"signature,omitempty"`
	ProvenanceSource      string   `json:"provenance_source,omitempty" yaml:"provenance_source,omitempty"`
	ProvenanceRepository  string   `json:"provenance_repository,omitempty" yaml:"provenance_repository,omitempty"`
	ProvenanceRef         string   `json:"provenance_ref,omitempty" yaml:"provenance_ref,omitempty"`
	ProvenanceCommit      string   `json:"provenance_commit,omitempty" yaml:"provenance_commit,omitempty"`
	ProvenanceDigest      string   `json:"provenance_digest,omitempty" yaml:"provenance_digest,omitempty"`
	ProvenanceSignature   string   `json:"provenance_signature,omitempty" yaml:"provenance_signature,omitempty"`
	ProvenanceInstalledAt string   `json:"provenance_installed_at,omitempty" yaml:"provenance_installed_at,omitempty"`
	ProvenanceInstalledBy string   `json:"provenance_installed_by,omitempty" yaml:"provenance_installed_by,omitempty"`
	MinimumAttelerVersion string   `json:"min_atteler_version,omitempty" yaml:"min_atteler_version,omitempty"`
}

// Lockfile returns deterministic registry metadata suitable for persisting as a
// lockfile.
func (r *Registry) Lockfile() Lockfile {
	lock := Lockfile{
		Version: LockfileSchemaVersion,
	}
	if r == nil {
		return lock
	}

	for _, name := range r.order {
		plugin := r.plugins[name]
		lock.Plugins = append(lock.Plugins, lockedPluginFromPlugin(plugin))
	}

	return lock
}

// LoadLockfile reads and validates an installed-plugin lockfile.
func LoadLockfile(path string) (Lockfile, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return Lockfile{}, errors.New("plugin lockfile: empty path")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return Lockfile{}, fmt.Errorf("plugin lockfile: read %s: %w", path, err)
	}

	var lock Lockfile
	if err := yaml.Unmarshal(data, &lock); err != nil {
		return Lockfile{}, fmt.Errorf("plugin lockfile: parse %s: %w", path, err)
	}

	if err := lock.Validate(); err != nil {
		return Lockfile{}, fmt.Errorf("plugin lockfile: validate %s: %w", path, err)
	}

	return lock, nil
}

// SaveLockfile atomically persists an installed-plugin lockfile.
func SaveLockfile(path string, lock Lockfile) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return errors.New("plugin lockfile: empty path")
	}

	if lock.Version == 0 {
		lock.Version = LockfileSchemaVersion
	}

	if err := lock.Validate(); err != nil {
		return fmt.Errorf("plugin lockfile: validate: %w", err)
	}

	data, err := yaml.Marshal(lock)
	if err != nil {
		return fmt.Errorf("plugin lockfile: marshal: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("plugin lockfile: create dir: %w", err)
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("plugin lockfile: write %s: %w", path, err)
	}

	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)

		return fmt.Errorf("plugin lockfile: replace %s: %w", path, err)
	}

	return nil
}

// Validate checks lockfile shape and duplicate installed plugin entries.
func (l Lockfile) Validate() error {
	if l.Version != LockfileSchemaVersion {
		return fmt.Errorf("unsupported version %d", l.Version)
	}

	names := make(map[string]struct{}, len(l.Plugins))
	for i := range l.Plugins {
		plugin := &l.Plugins[i]

		name, err := validateLockedPluginShape(i, plugin)
		if err != nil {
			return err
		}

		if _, ok := names[name]; ok {
			return fmt.Errorf("duplicate plugin name %q", name)
		}

		names[name] = struct{}{}
	}

	return nil
}

func validateLockedPluginShape(index int, plugin *LockedPlugin) (string, error) {
	name := strings.TrimSpace(plugin.Name)
	if name == "" {
		return "", fmt.Errorf("plugin %d missing name", index)
	}

	if strings.TrimSpace(plugin.Version) == "" {
		return "", fmt.Errorf("plugin %q missing version", name)
	}

	if strings.TrimSpace(plugin.Root) == "" {
		return "", fmt.Errorf("plugin %q missing root", name)
	}

	if strings.TrimSpace(plugin.ManifestPath) == "" {
		return "", fmt.Errorf("plugin %q missing manifest path", name)
	}

	if err := validateUniqueNonEmpty("plugin "+name+" capabilities", plugin.Capabilities); err != nil {
		return "", err
	}

	if err := validateVersionRequirement("plugin "+name+" min_atteler_version", plugin.MinimumAttelerVersion); err != nil {
		return "", err
	}

	if err := validateLockedPluginMetadata(name, *plugin); err != nil {
		return "", err
	}

	return name, nil
}

func validateLockedPluginMetadata(name string, plugin LockedPlugin) error {
	fields := map[string]string{
		"root":                    plugin.Root,
		"manifest_path":           plugin.ManifestPath,
		"install_source":          plugin.InstallSource,
		"checksum":                plugin.Checksum,
		"signature":               plugin.Signature,
		"provenance_source":       plugin.ProvenanceSource,
		"provenance_repository":   plugin.ProvenanceRepository,
		"provenance_ref":          plugin.ProvenanceRef,
		"provenance_commit":       plugin.ProvenanceCommit,
		"provenance_digest":       plugin.ProvenanceDigest,
		"provenance_signature":    plugin.ProvenanceSignature,
		"provenance_installed_at": plugin.ProvenanceInstalledAt,
		"provenance_installed_by": plugin.ProvenanceInstalledBy,
	}
	for field, value := range fields {
		if strings.ContainsAny(value, "\x00\r\n") {
			return fmt.Errorf("plugin %q %s contains control characters", name, field)
		}
	}

	return nil
}

// ValidateRegistry checks that a lockfile still matches the loaded registry.
func (l Lockfile) ValidateRegistry(registry *Registry) error {
	if err := l.Validate(); err != nil {
		return err
	}

	if registry == nil {
		if len(l.Plugins) == 0 {
			return nil
		}

		return errors.New("registry is nil")
	}

	lockedByName := make(map[string]LockedPlugin, len(l.Plugins))
	for i := range l.Plugins {
		plugin := l.Plugins[i]
		lockedByName[plugin.Name] = plugin
	}

	for _, name := range registry.order {
		current := lockedPluginFromPlugin(registry.plugins[name])

		locked, ok := lockedByName[name]
		if !ok {
			return fmt.Errorf("plugin %q missing from lockfile", name)
		}

		if err := validateLockedPluginMatches(name, current, locked); err != nil {
			return err
		}
	}

	for i := range l.Plugins {
		locked := &l.Plugins[i]
		if _, ok := registry.plugins[locked.Name]; !ok {
			return fmt.Errorf("lockfile plugin %q is not configured", locked.Name)
		}
	}

	return nil
}

func validateLockedPluginMatches(name string, current, locked LockedPlugin) error {
	fields := []lockedStringField{
		{label: "root", current: current.Root, locked: locked.Root},
		{label: "manifest path", current: current.ManifestPath, locked: locked.ManifestPath},
		{label: "install source", current: current.InstallSource, locked: locked.InstallSource},
		{label: "checksum", current: current.Checksum, locked: locked.Checksum},
		{label: "signature", current: current.Signature, locked: locked.Signature},
		{label: "provenance source", current: current.ProvenanceSource, locked: locked.ProvenanceSource},
		{label: "provenance repository", current: current.ProvenanceRepository, locked: locked.ProvenanceRepository},
		{label: "provenance ref", current: current.ProvenanceRef, locked: locked.ProvenanceRef},
		{label: "provenance commit", current: current.ProvenanceCommit, locked: locked.ProvenanceCommit},
		{label: "provenance digest", current: current.ProvenanceDigest, locked: locked.ProvenanceDigest},
		{label: "provenance signature", current: current.ProvenanceSignature, locked: locked.ProvenanceSignature},
		{label: "provenance installed-at", current: current.ProvenanceInstalledAt, locked: locked.ProvenanceInstalledAt},
		{label: "provenance installed-by", current: current.ProvenanceInstalledBy, locked: locked.ProvenanceInstalledBy},
		{label: "minimum atteler version", current: current.MinimumAttelerVersion, locked: locked.MinimumAttelerVersion},
	}

	if current.Version != locked.Version {
		return fmt.Errorf("plugin %q version %q does not match lockfile version %q", name, current.Version, locked.Version)
	}

	for _, field := range fields {
		if field.current != field.locked {
			return fmt.Errorf("plugin %q %s does not match lockfile", name, field.label)
		}
	}

	if !slices.Equal(current.Capabilities, locked.Capabilities) {
		return fmt.Errorf("plugin %q capabilities do not match lockfile", name)
	}

	return nil
}

type lockedStringField struct {
	label   string
	current string
	locked  string
}

func lockedPluginFromPlugin(plugin Plugin) LockedPlugin {
	locked := LockedPlugin{
		Name:                  plugin.Manifest.Name,
		Version:               plugin.Manifest.Version,
		Root:                  plugin.Root,
		ManifestPath:          plugin.ManifestPath,
		Capabilities:          append([]string(nil), plugin.Manifest.Capabilities...),
		MinimumAttelerVersion: plugin.Manifest.MinimumAttelerVersion,
	}

	if plugin.Manifest.Trust != nil {
		locked.InstallSource = plugin.Manifest.Trust.InstallSource
		locked.Checksum = plugin.Manifest.Trust.Checksum
		locked.Signature = plugin.Manifest.Trust.Signature
	}

	if plugin.Manifest.Provenance != nil {
		locked.ProvenanceSource = plugin.Manifest.Provenance.Source
		locked.ProvenanceRepository = plugin.Manifest.Provenance.Repository
		locked.ProvenanceRef = plugin.Manifest.Provenance.Ref
		locked.ProvenanceCommit = plugin.Manifest.Provenance.Commit
		locked.ProvenanceDigest = plugin.Manifest.Provenance.Digest
		locked.ProvenanceSignature = plugin.Manifest.Provenance.Signature
		locked.ProvenanceInstalledAt = plugin.Manifest.Provenance.InstalledAt
		locked.ProvenanceInstalledBy = plugin.Manifest.Provenance.InstalledBy
	}

	return locked
}
