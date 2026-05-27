package config

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// OriginKind identifies the layer that supplied a configuration value.
type OriginKind string

// Configuration origin layers, ordered from lowest to highest precedence when
// returned by LoadWithOrigins.
const (
	OriginHarnessImport    OriginKind = "harness-import"
	OriginGlobalFile       OriginKind = "global-file"
	OriginProjectFile      OriginKind = "project-file"
	OriginEnvFile          OriginKind = "env-file"
	OriginExplicitFile     OriginKind = "explicit-file"
	OriginStateOverride    OriginKind = "state-override"
	OriginCLIFlag          OriginKind = "cli-flag"
	OriginRuntimeSelection OriginKind = "runtime-selection"
)

// OriginOperation describes how a layer affected a field.
type OriginOperation string

const (
	// OriginSet records the first value supplied for a field.
	OriginSet OriginOperation = "set"
	// OriginOverride records a later scalar value overriding an earlier value.
	OriginOverride OriginOperation = "override"
	// OriginReplace records a later list or map value replacing an earlier value in full.
	OriginReplace OriginOperation = "replace"
	// OriginMerge records a map layer merging entries by key.
	OriginMerge OriginOperation = "merge"
)

// PathSource pairs a config file path with its precedence layer.
type PathSource struct {
	Path string
	Kind OriginKind
}

// OriginEvent records one layer's contribution to a field path.
type OriginEvent struct {
	Kind      OriginKind      `json:"kind" yaml:"kind"`
	Operation OriginOperation `json:"operation" yaml:"operation"`
	Source    string          `json:"source" yaml:"source"`
	Value     string          `json:"value,omitempty" yaml:"value,omitempty"`
	Note      string          `json:"note,omitempty" yaml:"note,omitempty"`
}

// FieldOrigin stores the ordered chain of writes for a field path. The final
// event in Chain is the origin of the effective value.
type FieldOrigin struct {
	Chain []OriginEvent `json:"chain" yaml:"chain"`
}

// Final returns the effective origin event for this field.
func (o FieldOrigin) Final() (OriginEvent, bool) {
	if len(o.Chain) == 0 {
		return OriginEvent{}, false
	}

	return o.Chain[len(o.Chain)-1], true
}

// OriginMap maps dotted config field paths, such as "default_model" or
// "providers.openai.base_url", to the chain that produced the final value.
type OriginMap map[string]FieldOrigin

// Final returns the effective origin event for path.
func (m OriginMap) Final(path string) (OriginEvent, bool) {
	if len(m) == 0 {
		return OriginEvent{}, false
	}

	origin, ok := m[path]
	if !ok {
		return OriginEvent{}, false
	}

	return origin.Final()
}

// Paths returns stable sorted field paths present in the origin map.
func (m OriginMap) Paths() []string {
	paths := make([]string, 0, len(m))
	for path := range m {
		paths = append(paths, path)
	}

	sort.Strings(paths)

	return paths
}

type originSource struct {
	kind   OriginKind
	source string
}

type originRecorder struct {
	origins OriginMap
}

func newOriginRecorder(origins OriginMap) *originRecorder {
	if origins == nil {
		origins = OriginMap{}
	}

	return &originRecorder{origins: origins}
}

func (r *originRecorder) set(path string, source originSource, value any) {
	r.record(path, source, value, false, "")
}

func (r *originRecorder) replace(path string, source originSource, value any, note string) {
	r.record(path, source, value, true, note)
}

func (r *originRecorder) merge(path string, source originSource, value any, note string) {
	if r == nil {
		return
	}

	op := OriginSet

	if len(r.origins[path].Chain) > 0 {
		op = OriginMerge
	}

	r.append(path, OriginEvent{
		Kind:      source.kind,
		Operation: op,
		Source:    source.source,
		Value:     originValue(value),
		Note:      note,
	})
}

func (r *originRecorder) record(path string, source originSource, value any, replace bool, note string) {
	if r == nil {
		return
	}

	op := OriginSet

	if len(r.origins[path].Chain) > 0 {
		if replace {
			op = OriginReplace
		} else {
			op = OriginOverride
		}
	}

	r.append(path, OriginEvent{
		Kind:      source.kind,
		Operation: op,
		Source:    source.source,
		Value:     originValue(value),
		Note:      note,
	})
}

func (r *originRecorder) append(path string, event OriginEvent) {
	if r == nil {
		return
	}

	origin := r.origins[path]
	origin.Chain = append(origin.Chain, event)
	r.origins[path] = origin
}

func appendOriginChain(dst OriginMap, path string, src OriginMap, replace bool) {
	if dst == nil || src == nil {
		return
	}

	sourceOrigin, ok := src[path]
	if !ok || len(sourceOrigin.Chain) == 0 {
		return
	}

	targetOrigin := dst[path]
	targetHadOrigin := len(targetOrigin.Chain) > 0

	for i, event := range sourceOrigin.Chain {
		if i == 0 && targetHadOrigin {
			event.Operation = originChainOperation(event, replace)
		}

		targetOrigin.Chain = append(targetOrigin.Chain, event)
	}

	dst[path] = targetOrigin
}

func originChainOperation(event OriginEvent, replace bool) OriginOperation {
	switch {
	case replace:
		return OriginReplace
	case event.Operation == OriginMerge:
		return OriginMerge
	case event.Operation == OriginSet && strings.HasPrefix(event.Note, "merges "):
		return OriginMerge
	default:
		return OriginOverride
	}
}

func originValue(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	case fmt.Stringer:
		return v.String()
	}

	data, err := json.Marshal(value)
	if err == nil {
		return string(data)
	}

	return fmt.Sprint(value)
}

func sortedMapKeys[V any](in map[string]V) []string {
	keys := make([]string, 0, len(in))
	for key := range in {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	return keys
}

func providerFieldPath(name string, fields ...string) string {
	return dottedPath(append([]string{"providers", name}, fields...)...)
}

func modelAliasFieldPath(alias string) string {
	return dottedPath("model_aliases", alias)
}

func agentFieldPath(name string, fields ...string) string {
	return dottedPath(append([]string{"agents", name}, fields...)...)
}

func hookFieldPath(eventType string, fields ...string) string {
	return dottedPath(append([]string{"hooks", eventType}, fields...)...)
}

func dottedPath(parts ...string) string {
	cleaned := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			cleaned = append(cleaned, part)
		}
	}

	return strings.Join(cleaned, ".")
}
