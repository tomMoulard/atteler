package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// MigrationResult reports whether a config file was changed by migration.
//
//nolint:govet // fieldalignment: field order follows user-facing YAML report grouping.
type MigrationResult struct {
	Path        string       `json:"path" yaml:"path"`
	Changed     bool         `json:"changed" yaml:"changed"`
	Diagnostics []Diagnostic `json:"diagnostics,omitempty" yaml:"diagnostics,omitempty"`
}

// MigrateConfigFile updates one Atteler YAML/JSON config file to the current
// schema version. Unknown fields are preserved; unsupported future versions are
// rejected instead of rewritten.
func MigrateConfigFile(path string) (MigrationResult, error) {
	result := MigrationResult{Path: path}

	data, err := os.ReadFile(path)
	if err != nil {
		return result, fmt.Errorf("config migrate %s: read: %w", path, err)
	}

	if len(bytes.TrimSpace(data)) == 0 {
		return result, nil
	}

	var root yaml.Node

	unmarshalErr := yaml.Unmarshal(data, &root)
	if unmarshalErr != nil {
		return result, fmt.Errorf("config migrate %s: parse: %w", path, unmarshalErr)
	}

	mapping := documentMapping(&root)
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return result, fmt.Errorf("config migrate %s: expected top-level mapping", path)
	}

	result.Diagnostics = inspectConfigNode(path, mapping)

	changed, err := migrateConfigNode(mapping)
	if err != nil {
		return result, fmt.Errorf("config migrate %s: %w", path, err)
	}

	if !changed {
		return result, nil
	}

	out, err := marshalMigratedConfig(path, &root)
	if err != nil {
		return result, fmt.Errorf("config migrate %s: marshal: %w", path, err)
	}

	mode := fileModeOrDefault(path, 0o600)
	if err := writeConfigFileAtomic(path, out, mode); err != nil {
		return result, fmt.Errorf("config migrate %s: write atomically: %w", path, err)
	}

	result.Changed = true

	return result, nil
}

func marshalMigratedConfig(path string, root *yaml.Node) ([]byte, error) {
	if !strings.EqualFold(filepath.Ext(path), ".json") {
		data, err := yaml.Marshal(root)
		if err != nil {
			return nil, fmt.Errorf("marshal yaml: %w", err)
		}

		return data, nil
	}

	var value any
	if err := root.Decode(&value); err != nil {
		return nil, fmt.Errorf("decode yaml node: %w", err)
	}

	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal json: %w", err)
	}

	return append(data, '\n'), nil
}

// MigratePathSources updates each present config file in sources and skips
// missing files. It returns all per-file results that were attempted.
func MigratePathSources(sources []PathSource) ([]MigrationResult, error) {
	results := make([]MigrationResult, 0, len(sources))
	for _, source := range sources {
		path := source.Path
		if path == "" {
			continue
		}

		if _, err := os.Stat(path); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}

			return results, fmt.Errorf("config migrate %s: stat: %w", path, err)
		}

		result, err := MigrateConfigFile(path)
		results = append(results, result)

		if err != nil {
			return results, fmt.Errorf("migrate config file: %w", err)
		}
	}

	return results, nil
}

func migrateConfigNode(root *yaml.Node) (bool, error) {
	changed := false

	versionChanged, err := migrateVersionNode(root)
	if err != nil {
		return false, err
	}

	changed = changed || versionChanged
	changed = renameMappingKey(root, "provider", "default_provider") || changed
	changed = renameMappingKey(root, "model", "default_model") || changed

	if generation := mappingValue(root, "generation"); generation != nil {
		changed = renameMappingKey(generation, "reasoning", "reasoning_level") || changed
	}

	if agents := mappingValue(root, "agents"); agents != nil {
		forEachMappingField(agents, func(_ string, agent *yaml.Node) {
			changed = renameMappingKey(agent, "prompt", "system_prompt") || changed
		})
	}

	return changed, nil
}

func migrateVersionNode(root *yaml.Node) (bool, error) {
	value := mappingValue(root, "version")
	if value == nil {
		prependMappingField(root, "version", strconv.Itoa(ConfigSchemaVersion))

		return true, nil
	}

	var version int
	if err := value.Decode(&version); err != nil {
		return false, errors.New("version must be an integer")
	}

	if version < 0 || version > ConfigSchemaVersion {
		return false, fmt.Errorf("unsupported version %d; this Atteler supports version %d", version, ConfigSchemaVersion)
	}

	if version == ConfigSchemaVersion {
		return false, nil
	}

	value.Kind = yaml.ScalarNode
	value.Tag = "!!int"
	value.Value = strconv.Itoa(ConfigSchemaVersion)

	return true, nil
}

func renameMappingKey(node *yaml.Node, from, to string) bool {
	if node == nil || node.Kind != yaml.MappingNode {
		return false
	}

	fromIndex := mappingKeyIndex(node, from)
	if fromIndex < 0 {
		return false
	}

	toIndex := mappingKeyIndex(node, to)
	if toIndex >= 0 {
		removeMappingFieldAt(node, fromIndex)

		return true
	}

	node.Content[fromIndex].Value = to

	return true
}

func prependMappingField(node *yaml.Node, key, value string) {
	if node == nil || node.Kind != yaml.MappingNode {
		return
	}

	keyNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}
	valueNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: value}
	node.Content = append([]*yaml.Node{keyNode, valueNode}, node.Content...)
}

func mappingKeyIndex(node *yaml.Node, key string) int {
	if node == nil || node.Kind != yaml.MappingNode {
		return -1
	}

	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return i
		}
	}

	return -1
}

func removeMappingFieldAt(node *yaml.Node, keyIndex int) {
	if node == nil || keyIndex < 0 || keyIndex+1 >= len(node.Content) {
		return
	}

	node.Content = append(node.Content[:keyIndex], node.Content[keyIndex+2:]...)
}

func fileModeOrDefault(path string, fallback os.FileMode) os.FileMode {
	info, err := os.Stat(path)
	if err != nil {
		return fallback
	}

	return info.Mode().Perm()
}

func writeConfigFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)

	tmp, err := os.CreateTemp(dir, ".atteler-config-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp in %s: %w", dir, err)
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
		return fmt.Errorf("chmod temp %s: %w", tmpPath, err)
	}

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp %s: %w", tmpPath, err)
	}

	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp %s: %w", tmpPath, err)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp %s: %w", tmpPath, err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}

	cleanup = false

	return syncDir(dir)
}
