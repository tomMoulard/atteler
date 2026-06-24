package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/tommoulard/atteler/pkg/autonomy"
)

// DiagnosticSeverity identifies how strongly a config diagnostic should be
// treated by humans and tooling.
type DiagnosticSeverity string

// Config diagnostic severities.
const (
	DiagnosticInfo    DiagnosticSeverity = "info"
	DiagnosticWarning DiagnosticSeverity = "warning"
	DiagnosticError   DiagnosticSeverity = "error"

	diagnosticStatusError   = "error"
	diagnosticStatusMissing = "missing"
	diagnosticStatusPresent = "present"

	fieldDefaultModel          = "default_model"
	fieldDefaultModelMode      = "default_model_mode"
	fieldDefaultReasoningLevel = "default_reasoning_level"
	fieldRevision              = "revision"
)

// Diagnostic describes a schema, migration, or load-order finding for a
// configuration file.
type Diagnostic struct {
	Severity    DiagnosticSeverity `json:"severity" yaml:"severity"`
	Importer    string             `json:"importer,omitempty" yaml:"importer,omitempty"`
	Source      string             `json:"source,omitempty" yaml:"source,omitempty"`
	Path        string             `json:"path,omitempty" yaml:"path,omitempty"`
	Field       string             `json:"field,omitempty" yaml:"field,omitempty"`
	Message     string             `json:"message" yaml:"message"`
	Replacement string             `json:"replacement,omitempty" yaml:"replacement,omitempty"`
}

// String renders a compact human-readable diagnostic for CLI output.
func (d Diagnostic) String() string {
	parts := make([]string, 0, 3)
	if d.Importer != "" {
		parts = append(parts, d.Importer)
	}

	if d.Source != "" {
		source := d.Source
		if d.Path != "" {
			source += " " + d.Path
		}

		parts = append(parts, source)
	} else if d.Path != "" {
		parts = append(parts, d.Path)
	}

	if d.Field != "" && d.Source == "" {
		parts = append(parts, d.Field)
	}

	prefix := strings.Join(parts, ": ")
	if prefix == "" {
		return d.Message
	}

	return fmt.Sprintf("%s: %s", prefix, d.Message)
}

type diagnosticCollector struct {
	importer    string
	source      string
	diagnostics []Diagnostic
}

func newDiagnosticCollector(importer, source string) *diagnosticCollector {
	return &diagnosticCollector{
		importer: strings.TrimSpace(importer),
		source:   strings.TrimSpace(source),
	}
}

func (c *diagnosticCollector) warnf(path, format string, args ...any) {
	if c == nil {
		return
	}

	c.diagnostics = append(c.diagnostics, Diagnostic{
		Severity: DiagnosticWarning,
		Importer: c.importer,
		Source:   c.source,
		Path:     strings.TrimSpace(path),
		Message:  fmt.Sprintf(format, args...),
	})
}

func (c *diagnosticCollector) all() []Diagnostic {
	if c == nil || len(c.diagnostics) == 0 {
		return nil
	}

	return append([]Diagnostic(nil), c.diagnostics...)
}

// SourceDiagnostic describes one candidate configuration file from the
// load stack.
//
//nolint:govet // fieldalignment: field order follows user-facing YAML report grouping.
type SourceDiagnostic struct {
	Path        string       `json:"path" yaml:"path"`
	Kind        OriginKind   `json:"kind" yaml:"kind"`
	Status      string       `json:"status" yaml:"status"`
	Version     int          `json:"version,omitempty" yaml:"version,omitempty"`
	Diagnostics []Diagnostic `json:"diagnostics,omitempty" yaml:"diagnostics,omitempty"`
}

// StateDiagnostic describes the persisted interactive state file without
// including preference values.
//
//nolint:govet // fieldalignment: field order follows user-facing YAML report grouping.
type StateDiagnostic struct {
	Path        string       `json:"path" yaml:"path"`
	Status      string       `json:"status" yaml:"status"`
	Version     int          `json:"version,omitempty" yaml:"version,omitempty"`
	Revision    int64        `json:"revision,omitempty" yaml:"revision,omitempty"`
	Error       string       `json:"error,omitempty" yaml:"error,omitempty"`
	Diagnostics []Diagnostic `json:"diagnostics,omitempty" yaml:"diagnostics,omitempty"`
}

// DiagnosticsReport is a redaction-safe, issue-report-friendly view of
// the config load stack and merged config.
//
//nolint:govet // fieldalignment: field order follows user-facing YAML report grouping.
type DiagnosticsReport struct {
	ConfigSchemaVersion int                 `json:"config_schema_version" yaml:"config_schema_version"`
	StateSchemaVersion  int                 `json:"state_schema_version" yaml:"state_schema_version"`
	Sources             []SourceDiagnostic  `json:"sources" yaml:"sources"`
	LoadedSources       []string            `json:"loaded_sources,omitempty" yaml:"loaded_sources,omitempty"`
	LoadError           string              `json:"load_error,omitempty" yaml:"load_error,omitempty"`
	Diagnostics         []Diagnostic        `json:"diagnostics,omitempty" yaml:"diagnostics,omitempty"`
	State               *StateDiagnostic    `json:"state,omitempty" yaml:"state,omitempty"`
	Defaults            []DefaultDiagnostic `json:"defaults,omitempty" yaml:"defaults,omitempty"`
	Config              Config              `json:"config" yaml:"config"`
	Origins             OriginMap           `json:"origins,omitempty" yaml:"origins,omitempty"`
}

// InspectPathSources reads candidate config files and reports schema-version,
// unknown-field, and deprecated-field diagnostics without applying strict YAML
// decoding. Missing files are reported as missing rather than errors.
func InspectPathSources(sources []PathSource) []SourceDiagnostic {
	out := make([]SourceDiagnostic, 0, len(sources))
	for _, source := range sources {
		path := strings.TrimSpace(source.Path)
		if path == "" {
			continue
		}

		kind := source.Kind
		if kind == "" {
			kind = OriginExplicitFile
		}

		report := SourceDiagnostic{Path: path, Kind: kind}

		data, err := os.ReadFile(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				report.Status = diagnosticStatusMissing
			} else {
				report.Status = diagnosticStatusError
				report.Diagnostics = append(report.Diagnostics, Diagnostic{
					Severity: DiagnosticError,
					Path:     path,
					Message:  "read failed: " + err.Error(),
				})
			}

			out = append(out, report)

			continue
		}

		report.Status = diagnosticStatusPresent
		if strings.TrimSpace(string(data)) == "" {
			report.Status = "empty"
			out = append(out, report)

			continue
		}

		var root yaml.Node
		if err := yaml.Unmarshal(data, &root); err != nil {
			report.Status = diagnosticStatusError
			report.Diagnostics = append(report.Diagnostics, Diagnostic{
				Severity: DiagnosticError,
				Path:     path,
				Message:  "parse failed: " + err.Error(),
			})
			out = append(out, report)

			continue
		}

		mapping := documentMapping(&root)
		if mapping == nil || mapping.Kind != yaml.MappingNode {
			report.Status = diagnosticStatusError
			report.Diagnostics = append(report.Diagnostics, Diagnostic{
				Severity: DiagnosticError,
				Path:     path,
				Message:  "expected top-level mapping",
			})
			out = append(out, report)

			continue
		}

		report.Diagnostics = append(report.Diagnostics, inspectConfigNode(path, mapping)...)
		report.Version = diagnosedConfigVersion(mapping)
		out = append(out, report)
	}

	return out
}

// NewDiagnosticsReport returns a redacted diagnostics report suitable for
// attaching to issue reports. It includes strict load errors, if any, while
// still reporting permissive schema diagnostics for every candidate source.
func NewDiagnosticsReport(sources []PathSource) DiagnosticsReport {
	cfg, loaded, origins, err := LoadPathSources(sources)
	return newDiagnosticsReport(sources, cfg, loaded, origins, nil, err)
}

// NewDefaultDiagnosticsReport returns a redacted diagnostics report for
// the same default stack used by LoadWithOrigins, including harness imports and
// ATTELER_CONFIG overrides.
func NewDefaultDiagnosticsReport() DiagnosticsReport {
	cfg, loaded, origins, diagnostics, err := LoadWithDiagnostics()
	report := newDiagnosticsReport(DefaultPathSources(), cfg, loaded, origins, diagnostics, err)
	state := InspectStatePath(DefaultStatePath())
	report.State = &state

	return report
}

// InspectStatePath reports state path/version metadata without exposing
// persisted preference values.
func InspectStatePath(path string) StateDiagnostic {
	report := StateDiagnostic{Path: path}

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			report.Status = diagnosticStatusMissing

			return report
		}

		report.Status = diagnosticStatusError
		report.Error = err.Error()

		return report
	}

	if len(bytes.TrimSpace(data)) == 0 {
		report.Status = diagnosticStatusError

		_, loadErr := NewStateStore(path).Load()
		if loadErr != nil {
			report.Error = loadErr.Error()
		} else {
			report.Error = "empty state file"
		}

		return report
	}

	var root yaml.Node
	if unmarshalErr := yaml.Unmarshal(data, &root); unmarshalErr != nil {
		report.Status = diagnosticStatusError

		_, loadErr := NewStateStore(path).Load()
		if loadErr != nil {
			report.Error = loadErr.Error()
		} else {
			report.Error = unmarshalErr.Error()
		}

		return report
	}

	mapping := documentMapping(&root)
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		report.Status = diagnosticStatusError
		report.Error = "expected top-level mapping"

		return report
	}

	var raw struct {
		Version  int   `yaml:"version"`
		Revision int64 `yaml:"revision"`
	}
	if decodeErr := mapping.Decode(&raw); decodeErr != nil {
		report.Status = diagnosticStatusError
		report.Error = decodeErr.Error()

		return report
	}

	report.Version = raw.Version
	report.Revision = raw.Revision
	report.Diagnostics = append(report.Diagnostics, inspectStateNode(path, mapping)...)

	state, err := NewStateStore(path).Load()
	if err != nil {
		report.Status = diagnosticStatusError
		report.Error = err.Error()

		return report
	}

	report.Status = diagnosticStatusPresent
	report.Version = state.Version
	report.Revision = state.Revision

	return report
}

func inspectStateNode(path string, root *yaml.Node) []Diagnostic {
	if root == nil {
		return nil
	}

	var diagnostics []Diagnostic

	forEachMappingField(root, func(key string, value *yaml.Node) {
		switch key {
		case "version":
			diagnostics = append(diagnostics, inspectStateVersion(path, value)...)
		case fieldRevision, fieldDefaultModel, fieldDefaultModelMode, fieldDefaultReasoningLevel:
			return
		case "folders":
			diagnostics = append(diagnostics, inspectStateFolders(path, value)...)
		default:
			diagnostics = append(diagnostics, unknownStateDiagnostic(path, key))
		}
	})

	if !mappingHasKey(root, "version") {
		diagnostics = append(diagnostics, missingStateVersionDiagnostic(path))
	}

	return diagnostics
}

func inspectStateFolders(path string, value *yaml.Node) []Diagnostic {
	var diagnostics []Diagnostic

	forEachMappingField(value, func(_ string, folder *yaml.Node) {
		forEachMappingField(folder, func(key string, _ *yaml.Node) {
			switch key {
			case fieldDefaultModel, fieldDefaultModelMode, fieldDefaultReasoningLevel:
				return
			default:
				diagnostics = append(diagnostics, unknownStateDiagnostic(path, "folders.*."+key))
			}
		})
	})

	return diagnostics
}

func inspectStateVersion(path string, value *yaml.Node) []Diagnostic {
	if value == nil {
		return nil
	}

	var version int
	if err := value.Decode(&version); err != nil {
		return []Diagnostic{{
			Severity: DiagnosticError,
			Path:     path,
			Field:    "version",
			Message:  "version must be an integer",
		}}
	}

	return inspectStateVersionNumber(path, version)
}

func inspectStateVersionNumber(path string, version int) []Diagnostic {
	switch {
	case version == 0:
		return []Diagnostic{missingStateVersionDiagnostic(path)}
	case version != StateSchemaVersion:
		return []Diagnostic{{
			Severity: DiagnosticError,
			Path:     path,
			Field:    "version",
			Message:  fmt.Sprintf("unsupported state version %d; this Atteler supports version %d", version, StateSchemaVersion),
		}}
	default:
		return nil
	}
}

func missingStateVersionDiagnostic(path string) Diagnostic {
	return Diagnostic{
		Severity: DiagnosticInfo,
		Path:     path,
		Field:    "version",
		Message:  fmt.Sprintf("missing state schema version; file will be migrated to version %d", StateSchemaVersion),
	}
}

func unknownStateDiagnostic(path, field string) Diagnostic {
	return Diagnostic{
		Severity: DiagnosticInfo,
		Path:     path,
		Field:    field,
		Message:  "unknown state field is preserved across writes",
	}
}

func newDiagnosticsReport(
	sources []PathSource,
	cfg Config,
	loaded []string,
	origins OriginMap,
	diagnostics []Diagnostic,
	err error,
) DiagnosticsReport {
	report := DiagnosticsReport{
		ConfigSchemaVersion: ConfigSchemaVersion,
		StateSchemaVersion:  StateSchemaVersion,
		Sources:             InspectPathSources(sources),
		LoadedSources:       append([]string(nil), loaded...),
		Diagnostics:         append([]Diagnostic(nil), diagnostics...),
		Defaults:            DefaultDiagnostics(),
		Config:              RedactedConfig(cfg),
		Origins:             RedactedOriginMap(origins),
	}
	if err != nil {
		report.LoadError = err.Error()
	}

	return report
}

//nolint:cyclop // top-level schema dispatch is kept together so diagnostics mirror file shape.
func inspectConfigNode(path string, root *yaml.Node) []Diagnostic {
	if root == nil {
		return nil
	}

	var diagnostics []Diagnostic

	forEachMappingField(root, func(key string, value *yaml.Node) {
		switch key {
		case "version":
			diagnostics = append(diagnostics, inspectConfigVersion(path, "version", value)...)
		case "provider":
			diagnostics = append(diagnostics, deprecatedDiagnostic(path, "provider", "default_provider"))
		case "model":
			diagnostics = append(diagnostics, deprecatedDiagnostic(path, "model", fieldDefaultModel))
		case "generation":
			diagnostics = append(diagnostics, inspectNamedFields(path, "generation", value, knownGenerationFields(), deprecatedGenerationFields())...)
		case "research":
			diagnostics = append(diagnostics, inspectResearchFields(path, value)...)
		case "agent_loop":
			diagnostics = append(diagnostics, inspectNamedFields(path, "agent_loop", value, knownAgentLoopFields(), nil)...)
		case "context":
			diagnostics = append(diagnostics, inspectContextFields(path, value)...)
		case "providers":
			diagnostics = append(diagnostics, inspectProviders(path, value)...)
		case "agents":
			diagnostics = append(diagnostics, inspectAgents(path, value)...)
		case "hooks":
			diagnostics = append(diagnostics, inspectHooks(path, value)...)
		case "plugins":
			diagnostics = append(diagnostics, inspectPlugins(path, value)...)
		case "skill_learning":
			diagnostics = append(diagnostics, inspectNamedFields(path, "skill_learning", value, knownSkillLearningFields(), nil)...)
		case "vector":
			diagnostics = append(diagnostics, inspectVectorFields(path, value)...)
		case "worktree":
			diagnostics = append(diagnostics, inspectNamedFields(path, "worktree", value, knownWorktreeFields(), nil)...)
		case "model_aliases":
			diagnostics = append(diagnostics, inspectModelAliases(path, value)...)
		case "models":
			diagnostics = append(diagnostics, inspectModelRoles(path, value)...)
		case "autonomy":
			diagnostics = append(diagnostics, inspectAutonomy(path, value)...)
		case "default_provider", fieldDefaultModel, "event_ledger_path", "fallback_models", "auto":
			return
		default:
			diagnostics = append(diagnostics, unknownDiagnostic(path, key))
		}
	})

	if !mappingHasKey(root, "version") {
		diagnostics = append(diagnostics, Diagnostic{
			Severity: DiagnosticInfo,
			Path:     path,
			Field:    "version",
			Message:  fmt.Sprintf("missing config schema version; file will be read as version %d", ConfigSchemaVersion),
		})
	}

	return diagnostics
}

func inspectAutonomy(path string, value *yaml.Node) []Diagnostic {
	if value == nil {
		return nil
	}

	if value.Kind != yaml.ScalarNode {
		return []Diagnostic{autonomyDiagnostic(path, "autonomy must be one of: "+strings.Join(autonomy.SupportedValues(), ", "))}
	}

	if _, err := autonomy.Parse(value.Value); err != nil {
		return []Diagnostic{autonomyDiagnostic(path, err.Error())}
	}

	return nil
}

func autonomyDiagnostic(path, message string) Diagnostic {
	return Diagnostic{
		Severity: DiagnosticError,
		Path:     path,
		Field:    "autonomy",
		Message:  message,
	}
}

func inspectModelAliases(path string, value *yaml.Node) []Diagnostic {
	if value == nil {
		return nil
	}

	if value.Kind != yaml.MappingNode {
		return []Diagnostic{modelAliasDiagnostic(path, "model_aliases", "model_aliases must be a mapping of alias to provider/model")}
	}

	var diagnostics []Diagnostic

	forEachMappingField(value, func(alias string, target *yaml.Node) {
		field := modelAliasFieldPath(alias)
		if strings.TrimSpace(alias) == "" {
			diagnostics = append(diagnostics, modelAliasDiagnostic(path, field, "model alias cannot be empty"))

			return
		}

		if strings.Contains(strings.TrimSpace(alias), "/") {
			diagnostics = append(diagnostics, modelAliasDiagnostic(path, field, "model alias must be a bare model name"))
		}

		if target == nil || target.Kind != yaml.ScalarNode {
			diagnostics = append(diagnostics, modelAliasDiagnostic(path, field, "model alias target must be provider/model"))

			return
		}

		if !validProviderModelAlias(target.Value) {
			diagnostics = append(diagnostics, modelAliasDiagnostic(path, field, "model alias target must be provider/model"))
		}
	})

	return diagnostics
}

func validProviderModelAlias(model string) bool {
	providerName, providerModel, ok := strings.Cut(strings.TrimSpace(model), "/")
	if !ok {
		return false
	}

	providerName = strings.TrimSpace(providerName)

	providerModel = strings.TrimSpace(providerModel)

	return providerName != "" && providerModel != ""
}

func modelAliasDiagnostic(path, field, message string) Diagnostic {
	return Diagnostic{
		Severity: DiagnosticError,
		Path:     path,
		Field:    field,
		Message:  message,
	}
}

func inspectConfigVersion(path, field string, value *yaml.Node) []Diagnostic {
	if value == nil {
		return nil
	}

	var version int
	if err := value.Decode(&version); err != nil {
		return []Diagnostic{{
			Severity: DiagnosticError,
			Path:     path,
			Field:    field,
			Message:  "version must be an integer",
		}}
	}

	switch {
	case version < 0:
		return []Diagnostic{{
			Severity: DiagnosticError,
			Path:     path,
			Field:    field,
			Message:  fmt.Sprintf("unsupported config version %d; this Atteler supports version %d", version, ConfigSchemaVersion),
		}}
	case version < ConfigSchemaVersion:
		return []Diagnostic{{
			Severity: DiagnosticInfo,
			Path:     path,
			Field:    field,
			Message:  fmt.Sprintf("legacy config version %d will be migrated to version %d", version, ConfigSchemaVersion),
		}}
	case version > ConfigSchemaVersion:
		return []Diagnostic{{
			Severity: DiagnosticError,
			Path:     path,
			Field:    field,
			Message:  fmt.Sprintf("unsupported config version %d; this Atteler supports version %d", version, ConfigSchemaVersion),
		}}
	default:
		return nil
	}
}

func diagnosedConfigVersion(root *yaml.Node) int {
	if root == nil {
		return 0
	}

	value := mappingValue(root, "version")
	if value == nil {
		return ConfigSchemaVersion
	}

	var version int
	if err := value.Decode(&version); err != nil || version == 0 {
		return ConfigSchemaVersion
	}

	return version
}

func inspectContextFields(path string, value *yaml.Node) []Diagnostic {
	diagnostics := inspectNamedFields(path, "context", value, knownContextFields(), nil)
	if projectInstructions := mappingValue(value, "project_instructions"); projectInstructions != nil {
		diagnostics = append(diagnostics, inspectNamedFields(path, "context.project_instructions", projectInstructions, knownProjectInstructionFields(), nil)...)
	}

	if policy := mappingValue(value, "reference_policy"); policy != nil {
		diagnostics = append(diagnostics, inspectNamedFields(path, "context.reference_policy", policy, knownReferencePolicyFields(), nil)...)
	}

	return diagnostics
}

func inspectResearchFields(path string, value *yaml.Node) []Diagnostic {
	diagnostics := inspectNamedFields(path, "research", value, knownResearchFields(), nil)
	if policy := mappingValue(value, "source_policy"); policy != nil {
		diagnostics = append(diagnostics, inspectNamedFields(path, "research.source_policy", policy, knownSourcePolicyFields(), nil)...)
	}

	return diagnostics
}

func inspectPlugins(path string, value *yaml.Node) []Diagnostic {
	// Plugin policies are owned by pkg/plugin and may evolve independently. Keep
	// this checker focused on the config package boundary and do not flag nested
	// policy keys as unknown.
	return inspectNamedFields(path, "plugins", value, map[string]bool{
		"paths":  true,
		"policy": true,
	}, nil)
}

func inspectHooks(path string, value *yaml.Node) []Diagnostic {
	var diagnostics []Diagnostic

	forEachMappingField(value, func(event string, hooks *yaml.Node) {
		if hooks == nil || hooks.Kind != yaml.SequenceNode {
			return
		}

		for i, hook := range hooks.Content {
			field := fmt.Sprintf("hooks.%s[%d]", event, i)
			diagnostics = append(diagnostics, inspectNamedFields(path, field, hook, knownHookFields(), nil)...)
		}
	})

	return diagnostics
}

func inspectVectorFields(path string, value *yaml.Node) []Diagnostic {
	if value == nil {
		return nil
	}

	if value.Kind != yaml.MappingNode {
		return []Diagnostic{{
			Severity: DiagnosticError,
			Path:     path,
			Field:    "vector",
			Message:  "vector must be a mapping",
		}}
	}

	diagnostics := inspectNamedFields(path, "vector", value, knownVectorFields(), nil)

	diagnostics = append(diagnostics, inspectVectorizerValueFields(path, "vector", value)...)
	if stores := mappingValue(value, "stores"); stores != nil {
		diagnostics = append(diagnostics, inspectVectorizerScopeEntries(path, "vector.stores", stores)...)
	}

	if agents := mappingValue(value, "agents"); agents != nil {
		diagnostics = append(diagnostics, inspectVectorizerScopeEntries(path, "vector.agents", agents)...)
	}

	if sources := mappingValue(value, "sources"); sources != nil {
		diagnostics = append(diagnostics, inspectVectorizerScopeEntries(path, "vector.sources", sources)...)
	}

	return diagnostics
}

func inspectVectorizerScopeEntries(path, prefix string, value *yaml.Node) []Diagnostic {
	if value == nil {
		return nil
	}

	if value.Kind != yaml.MappingNode {
		return []Diagnostic{{
			Severity: DiagnosticError,
			Path:     path,
			Field:    prefix,
			Message:  prefix + " must be a mapping",
		}}
	}

	var diagnostics []Diagnostic

	diagnostics = append(diagnostics, inspectVectorizerScopeAliases(path, prefix, value)...)

	forEachMappingField(value, func(name string, entry *yaml.Node) {
		field := prefix + "." + name

		diagnostics = append(diagnostics, inspectVectorizerScopeName(path, prefix, name)...)
		if entry == nil || entry.Kind != yaml.MappingNode {
			diagnostics = append(diagnostics, Diagnostic{
				Severity: DiagnosticError,
				Path:     path,
				Field:    field,
				Message:  field + " must be a mapping",
			})

			return
		}

		diagnostics = append(diagnostics, inspectNamedFields(path, field, entry, knownVectorizerFields(), nil)...)
		diagnostics = append(diagnostics, inspectVectorizerValueFields(path, field, entry)...)
	})

	return diagnostics
}

func inspectVectorizerScopeAliases(path, prefix string, value *yaml.Node) []Diagnostic {
	canonicalName := vectorizerScopeDiagnosticCanonicalName(prefix)
	if canonicalName == nil {
		return nil
	}

	var diagnostics []Diagnostic

	seen := make(map[string]string)

	forEachMappingField(value, func(name string, _ *yaml.Node) {
		canonical, ok := canonicalName(name)
		if !ok {
			return
		}

		if previous, exists := seen[canonical]; exists {
			field := prefix + "." + name
			diagnostics = append(diagnostics, Diagnostic{
				Severity: DiagnosticError,
				Path:     path,
				Field:    field,
				Message: fmt.Sprintf(
					"%s duplicates %s.%s (both resolve to %s); keep one scope name",
					field,
					prefix,
					previous,
					canonical,
				),
			})

			return
		}

		seen[canonical] = name
	})

	return diagnostics
}

func vectorizerScopeDiagnosticCanonicalName(prefix string) func(string) (string, bool) {
	switch prefix {
	case "vector.stores":
		return canonicalVectorStoreScopeName
	case "vector.sources":
		return canonicalVectorSourceScopeName
	default:
		return nil
	}
}

func inspectVectorizerScopeName(path, prefix, name string) []Diagnostic {
	switch prefix {
	case "vector.stores":
		return inspectVectorStoreScopeName(path, prefix, name)
	case "vector.sources":
		return inspectVectorSourceScopeName(path, prefix, name)
	default:
		return nil
	}
}

func inspectVectorStoreScopeName(path, prefix, name string) []Diagnostic {
	if knownVectorStoreScope(name) {
		return nil
	}

	return []Diagnostic{{
		Severity: DiagnosticError,
		Path:     path,
		Field:    prefix + "." + name,
		Message:  "unknown vector store scope (supported: agent-memory, vector-search, workspace)",
	}}
}

func inspectVectorSourceScopeName(path, prefix, name string) []Diagnostic {
	if knownVectorSourceScope(name) {
		return nil
	}

	return []Diagnostic{{
		Severity: DiagnosticError,
		Path:     path,
		Field:    prefix + "." + name,
		Message:  "unknown vector source scope (supported: file, session, git_history, adr)",
	}}
}

func inspectVectorizerValueFields(path, prefix string, value *yaml.Node) []Diagnostic {
	var diagnostics []Diagnostic
	if vectorizer := mappingValue(value, "vectorizer"); vectorizer != nil {
		diagnostics = append(diagnostics, inspectVectorizerKindValue(path, prefix+".vectorizer", vectorizer)...)
	}

	if provider := mappingValue(value, "provider"); provider != nil {
		diagnostics = append(diagnostics, inspectVectorProviderValue(path, prefix+".provider", provider)...)
	}

	if fallback := mappingValue(value, "fallback_policy"); fallback != nil {
		diagnostics = append(diagnostics, inspectVectorFallbackPolicyValue(path, prefix+".fallback_policy", fallback)...)
	}

	return diagnostics
}

func inspectVectorizerKindValue(path, field string, value *yaml.Node) []Diagnostic {
	normalized, diagnostics := normalizedVectorScalarValue(path, field, value, "vectorizer")
	if len(diagnostics) > 0 || normalized == "" {
		return diagnostics
	}

	if knownVectorizerKind(normalized) {
		return nil
	}

	return []Diagnostic{{
		Severity: DiagnosticError,
		Path:     path,
		Field:    field,
		Message:  fmt.Sprintf("unsupported vectorizer %q (supported: lexical, embedding)", value.Value),
	}}
}

func inspectVectorProviderValue(path, field string, value *yaml.Node) []Diagnostic {
	normalized, diagnostics := normalizedVectorScalarValue(path, field, value, "provider")
	if len(diagnostics) > 0 || normalized == "" {
		return diagnostics
	}

	if knownVectorProvider(normalized) {
		return nil
	}

	return []Diagnostic{{
		Severity: DiagnosticError,
		Path:     path,
		Field:    field,
		Message:  fmt.Sprintf("unsupported vector provider %q (supported: ollama-compatible)", value.Value),
	}}
}

func inspectVectorFallbackPolicyValue(path, field string, value *yaml.Node) []Diagnostic {
	normalized, diagnostics := normalizedVectorScalarValue(path, field, value, "fallback_policy")
	if len(diagnostics) > 0 || normalized == "" {
		return diagnostics
	}

	if knownVectorFallbackPolicy(normalized) {
		return nil
	}

	return []Diagnostic{{
		Severity: DiagnosticError,
		Path:     path,
		Field:    field,
		Message:  fmt.Sprintf("unsupported vector fallback policy %q (supported: fail, lexical)", value.Value),
	}}
}

func normalizedVectorScalarValue(path, field string, value *yaml.Node, name string) (string, []Diagnostic) {
	if value == nil {
		return "", nil
	}

	if value.Kind != yaml.ScalarNode {
		return "", []Diagnostic{{
			Severity: DiagnosticError,
			Path:     path,
			Field:    field,
			Message:  name + " must be a string",
		}}
	}

	normalized := strings.ToLower(strings.TrimSpace(value.Value))
	normalized = strings.ReplaceAll(normalized, "_", "-")

	return normalized, nil
}

func inspectAgents(path string, value *yaml.Node) []Diagnostic {
	var diagnostics []Diagnostic

	forEachMappingField(value, func(name string, entry *yaml.Node) {
		prefix := "agents." + name

		diagnostics = append(diagnostics, inspectNamedFields(path, prefix, entry, knownAgentFields(), deprecatedAgentFields())...)
		if routingPolicy := mappingValue(entry, "routing_policy"); routingPolicy != nil {
			diagnostics = append(
				diagnostics,
				inspectNamedFields(path, prefix+".routing_policy", routingPolicy, knownRoutingPolicyFields(), nil)...,
			)
		}

		if toolPolicy := mappingValue(entry, "tool_policy"); toolPolicy != nil {
			diagnostics = append(diagnostics, inspectAgentToolPolicy(path, prefix+".tool_policy", toolPolicy)...)
		}
	})

	return diagnostics
}

func inspectAgentToolPolicy(path, field string, value *yaml.Node) []Diagnostic {
	if value.Kind != yaml.ScalarNode {
		return []Diagnostic{{
			Severity: DiagnosticError,
			Path:     path,
			Field:    field,
			Message:  "agent tool_policy must be a string",
		}}
	}

	if isSupportedAgentToolPolicy(value.Value) {
		return nil
	}

	return []Diagnostic{{
		Severity: DiagnosticWarning,
		Path:     path,
		Field:    field,
		Message:  fmt.Sprintf("unsupported agent tool_policy %q; effective policy is deny (supported: deny, allow-all)", value.Value),
	}}
}

func inspectModelRoles(path string, value *yaml.Node) []Diagnostic {
	var diagnostics []Diagnostic

	forEachMappingField(value, func(name string, entry *yaml.Node) {
		prefix := "models." + name

		diagnostics = append(diagnostics, inspectNamedFields(path, prefix, entry, knownModelRoleFields(), nil)...)
		if routingPolicy := mappingValue(entry, "routing_policy"); routingPolicy != nil {
			diagnostics = append(
				diagnostics,
				inspectNamedFields(path, prefix+".routing_policy", routingPolicy, knownRoutingPolicyFields(), nil)...,
			)
		}
	})

	return diagnostics
}

func inspectProviders(path string, value *yaml.Node) []Diagnostic {
	var diagnostics []Diagnostic

	forEachMappingField(value, func(name string, entry *yaml.Node) {
		prefix := "providers." + name

		diagnostics = append(diagnostics, inspectNamedFields(path, prefix, entry, knownProviderFields(), nil)...)
		if policy := mappingValue(entry, "credential_policy"); policy != nil {
			diagnostics = append(
				diagnostics,
				inspectNamedFields(path, prefix+".credential_policy", policy, knownCredentialPolicyFields(), nil)...,
			)
		}
	})

	return diagnostics
}

func inspectNamedFields(
	path string,
	prefix string,
	value *yaml.Node,
	known map[string]bool,
	deprecated map[string]string,
) []Diagnostic {
	var diagnostics []Diagnostic

	forEachMappingField(value, func(key string, _ *yaml.Node) {
		field := prefix + "." + key
		if replacement, ok := deprecated[key]; ok {
			diagnostics = append(diagnostics, deprecatedDiagnostic(path, field, prefix+"."+replacement))

			return
		}

		if known[key] {
			return
		}

		diagnostics = append(diagnostics, unknownDiagnostic(path, field))
	})

	return diagnostics
}

func deprecatedDiagnostic(path, field, replacement string) Diagnostic {
	return Diagnostic{
		Severity:    DiagnosticWarning,
		Path:        path,
		Field:       field,
		Message:     "deprecated config field will be migrated in memory",
		Replacement: replacement,
	}
}

func unknownDiagnostic(path, field string) Diagnostic {
	return Diagnostic{
		Severity: DiagnosticError,
		Path:     path,
		Field:    field,
		Message:  "unknown config field",
	}
}

func documentMapping(root *yaml.Node) *yaml.Node {
	if root == nil {
		return nil
	}

	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		return root.Content[0]
	}

	return root
}

func forEachMappingField(node *yaml.Node, fn func(key string, value *yaml.Node)) {
	if node == nil || node.Kind != yaml.MappingNode {
		return
	}

	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i].Value
		fn(key, node.Content[i+1])
	}
}

func mappingHasKey(node *yaml.Node, key string) bool {
	return mappingValue(node, key) != nil
}

func mappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}

	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}

	return nil
}

func knownGenerationFields() map[string]bool {
	return map[string]bool{
		"temperature":     true,
		"top_p":           true,
		"seed":            true,
		"model_mode":      true,
		"reasoning_level": true,
		"max_tokens":      true,
	}
}

func deprecatedGenerationFields() map[string]string {
	return map[string]string{"reasoning": "reasoning_level"}
}

func knownResearchFields() map[string]bool {
	return map[string]bool{
		"source_policy": true,
	}
}

func knownSourcePolicyFields() map[string]bool {
	return map[string]bool{
		"trusted_domains":                         true,
		"denied_domains":                          true,
		"prefer_source_types":                     true,
		"allow_low_trust_sources":                 true,
		"warn_on_low_trust_sources":               true,
		"require_evidence_for_high_impact_claims": true,
	}
}

func knownAgentLoopFields() map[string]bool {
	return map[string]bool{
		"max_output_bytes":    true,
		"max_cost_micros":     true,
		"max_input_tokens":    true,
		"max_output_tokens":   true,
		"max_total_tokens":    true,
		"max_iterations":      true,
		"max_model_calls":     true,
		"max_tool_calls":      true,
		"max_wall_time":       true,
		"checkpoint_interval": true,
	}
}

func knownContextFields() map[string]bool {
	return map[string]bool{
		"references":           true,
		"project_instructions": true,
		"max_file_bytes":       true,
		"max_total_bytes":      true,
		"max_input_tokens":     true,
		"reference_policy":     true,
	}
}

func knownProjectInstructionFields() map[string]bool {
	return map[string]bool{
		"enabled":    true,
		"max_tokens": true,
	}
}

func knownReferencePolicyFields() map[string]bool {
	return map[string]bool{
		"allowed_schemes":        true,
		"denied_schemes":         true,
		"allowed_hosts":          true,
		"denied_hosts":           true,
		"allowed_ports":          true,
		"denied_ports":           true,
		"local_roots":            true,
		"denied_local_roots":     true,
		"allowed_globs":          true,
		"denied_globs":           true,
		"content_types":          true,
		"max_redirects":          true,
		"max_files":              true,
		"allow_absolute_paths":   true,
		"allow_private_networks": true,
	}
}

func knownProviderFields() map[string]bool {
	return map[string]bool{
		"base_url":                true,
		"type":                    true,
		"api_key_env":             true,
		"api_key_header":          true,
		"api_key_scheme":          true,
		"chat_completions_path":   true,
		"embeddings_path":         true,
		"models_path":             true,
		"api_version":             true,
		"models":                  true,
		"capabilities":            true,
		"disabled":                true,
		"local":                   true,
		"auto_start":              true,
		"disable_private_adapter": true,
		"credential_policy":       true,
		"retry":                   true,
		"timeout_seconds":         true,
	}
}

func knownCredentialPolicyFields() map[string]bool {
	return map[string]bool{
		"allowed_providers":    true,
		"allowed_stores":       true,
		"allow_borrowed_oauth": true,
		"allow_refresh":        true,
		"allow_write_back":     true,
	}
}

func knownSkillLearningFields() map[string]bool {
	return map[string]bool{
		"enabled":          true,
		"store_dir":        true,
		"skill_dir":        true,
		"max_observations": true,
		"max_steps":        true,
		"min_occurrences":  true,
	}
}

func knownVectorFields() map[string]bool {
	return map[string]bool{
		"stores":                            true,
		"agents":                            true,
		"sources":                           true,
		"workspace_enabled":                 true,
		"workspace_allow_remote_embeddings": true,
		"vectorizer":                        true,
		"provider":                          true,
		"model":                             true,
		"base_url":                          true,
		"fallback_policy":                   true,
		"index_path":                        true,
		"workspace_index_path":              true,
		"workspace_include":                 true,
		"workspace_exclude":                 true,
		"timeout_seconds":                   true,
		"chunk_max_runes":                   true,
		"chunk_overlap_runes":               true,
		"workspace_limit":                   true,
		"workspace_max_file_bytes":          true,
		"workspace_max_files":               true,
	}
}

func knownWorktreeFields() map[string]bool {
	return map[string]bool{
		"auto_merge":            true,
		"verification_commands": true,
		"override_verification": true,
	}
}

func knownVectorizerFields() map[string]bool {
	return map[string]bool{
		"vectorizer":          true,
		"provider":            true,
		"model":               true,
		"base_url":            true,
		"fallback_policy":     true,
		"index_path":          true,
		"timeout_seconds":     true,
		"chunk_max_runes":     true,
		"chunk_overlap_runes": true,
	}
}

func knownAgentFields() map[string]bool {
	return map[string]bool{
		"temperature":       true,
		"top_p":             true,
		"seed":              true,
		"tools":             true,
		"tool_policy":       true,
		"routing_policy":    true,
		"model":             true,
		"mode":              true,
		"model_mode":        true,
		"reasoning_level":   true,
		"description":       true,
		"personality":       true,
		"system_prompt":     true,
		"fallback_models":   true,
		"capabilities":      true,
		"triggers":          true,
		"references":        true,
		"feedback_guidance": true,
		"max_tokens":        true,
		"hidden":            true,
	}
}

func knownModelRoleFields() map[string]bool {
	return map[string]bool{
		"preferred":              true,
		"fallback":               true,
		"fallbacks":              true,
		"fallback_models":        true,
		"routing_policy":         true,
		"preferred_providers":    true,
		"banned_providers":       true,
		"banned_models":          true,
		"required_capabilities":  true,
		"max_cost_usd":           true,
		"max_latency_ms":         true,
		"max_ttft_ms":            true,
		"require_fresh_metadata": true,
		"prefer_local":           true,
	}
}

func knownRoutingPolicyFields() map[string]bool {
	return map[string]bool{
		"preferred_providers":    true,
		"banned_providers":       true,
		"banned_models":          true,
		"required_capabilities":  true,
		"max_budget":             true,
		"max_latency_ms":         true,
		"max_ttft_ms":            true,
		"require_fresh_metadata": true,
	}
}

func deprecatedAgentFields() map[string]string {
	return map[string]string{"prompt": "system_prompt"}
}

func knownHookFields() map[string]bool {
	return map[string]bool{
		"env":                  true,
		"command":              true,
		"payload":              true,
		"timeout_seconds":      true,
		"max_attempts":         true,
		"retry_backoff_millis": true,
		"inherit_env":          true,
		"blocking":             true,
	}
}
