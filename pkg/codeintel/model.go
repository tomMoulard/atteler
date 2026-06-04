//nolint:wsl_v5 // Public code-intelligence model structs prioritize stable/readable field order.
package codeintel

import (
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tommoulard/atteler/pkg/codegraph"
)

const (
	// LanguageGo identifies Go source files in the language-neutral model.
	LanguageGo = "go"
	// LanguagePython identifies Python source files in the language-neutral model.
	LanguagePython = "python"
)

// Model is the language-neutral code-intelligence index that higher-level query
// surfaces should use. The legacy Index fields remain for existing Go-specific
// CLI responses; Model is intentionally shaped around files, symbols,
// definitions, references, diagnostics, and relationships across languages.
type Model struct {
	Files         []CodeFile
	Symbols       []CodeSymbol
	Definitions   []CodeDefinition
	References    []CodeReference
	Diagnostics   []CodeDiagnostic
	Relationships []CodeRelationship
	Stats         IndexStats
}

// CodeFile describes one indexed source file independent of parser or language.
type CodeFile struct {
	ModTime     time.Time
	Provenance  Provenance
	ID          string
	Path        string
	Language    string
	ContentHash string
	Range       SourceRange
	Size        int64
}

// CodeSymbol is a named item exposed by a language adapter. Symbols may point
// at definitions when the adapter can resolve declaration identity.
type CodeSymbol struct {
	Provenance   Provenance
	ID           string
	Name         string
	Kind         string
	Language     string
	File         string
	ContainerID  string
	DefinitionID string
	Range        SourceRange
}

// CodeDefinition is a language-neutral declaration or definition.
type CodeDefinition struct {
	Provenance    Provenance
	ID            string
	Name          string
	Kind          string
	Language      string
	File          string
	ContainerID   string
	ContainerName string
	Range         SourceRange
	Exported      bool
}

// CodeReference is a language-neutral identifier/reference edge endpoint.
type CodeReference struct {
	Provenance Provenance
	ID         string
	Name       string
	Kind       string
	Language   string
	File       string
	FromID     string
	ToID       string
	Range      SourceRange
}

// CodeDiagnostic describes an indexing or language-server diagnostic.
type CodeDiagnostic struct {
	ID       string
	Language string
	File     string
	Source   string
	Severity string
	Message  string
	Range    SourceRange
}

// CodeRelationship describes one typed relationship between model items.
type CodeRelationship struct {
	FromID     string
	ToID       string
	Kind       string
	Language   string
	File       string
	Provenance []Provenance
	Range      SourceRange
}

func modelFromGoIndex(index Index) Model {
	model := Model{Stats: index.Stats}

	for i := range index.FileDetails {
		file := index.FileDetails[i]
		model.Files = append(model.Files, CodeFile{
			ID:          string(fileNodeID(file.Path)),
			Path:        file.Path,
			Language:    LanguageGo,
			ContentHash: file.ContentHash,
			Size:        file.Size,
			ModTime:     file.ModTime,
			Range:       file.Range,
			Provenance:  file.Provenance,
		})
	}

	for i := range index.Declarations {
		declaration := index.Declarations[i]
		containerName := declaration.PackagePath
		if containerName == "" {
			containerName = declaration.PackageName
		}
		if declaration.Receiver != "" {
			containerName = declaration.Receiver
		}

		definition := CodeDefinition{
			ID:            declaration.ID,
			Name:          declaration.Name,
			Kind:          declaration.Kind,
			Language:      LanguageGo,
			File:          declaration.File,
			ContainerID:   declaration.PackageID,
			ContainerName: containerName,
			Range:         declaration.Range,
			Exported:      declaration.Exported,
			Provenance:    declaration.Provenance,
		}
		model.Definitions = append(model.Definitions, definition)
		model.Symbols = append(model.Symbols, CodeSymbol{
			ID:           declaration.ID,
			Name:         declaration.Name,
			Kind:         declaration.Kind,
			Language:     LanguageGo,
			File:         declaration.File,
			ContainerID:  declaration.PackageID,
			DefinitionID: declaration.ID,
			Range:        declaration.Range,
			Provenance:   declaration.Provenance,
		})
	}

	for i := range index.References {
		reference := index.References[i]
		model.References = append(model.References, CodeReference{
			ID:         reference.ID,
			Name:       reference.Name,
			Kind:       reference.Kind,
			Language:   LanguageGo,
			File:       reference.File,
			FromID:     reference.FromDeclarationID,
			ToID:       reference.ToDeclarationID,
			Range:      reference.Range,
			Provenance: reference.Provenance,
		})
	}

	for i := range index.Diagnostics {
		diagnostic := index.Diagnostics[i]
		rangeInfo := diagnosticRange(diagnostic.Position)
		model.Diagnostics = append(model.Diagnostics, CodeDiagnostic{
			ID:       diagnosticID(LanguageGo, diagnostic.Kind, diagnostic.Position, diagnostic.Message),
			Language: LanguageGo,
			File:     rangeInfo.File,
			Source:   diagnostic.PackageID,
			Severity: diagnostic.Kind,
			Message:  diagnostic.Message,
			Range:    rangeInfo,
		})
	}

	model.Relationships = append(model.Relationships, relationshipsFromGraph(index.Graph, LanguageGo)...)
	model.sort()

	return model
}

func diagnosticRange(position string) SourceRange {
	position = strings.TrimSpace(position)
	if position == "" {
		return SourceRange{}
	}

	parts := strings.Split(position, ":")
	if len(parts) < 2 {
		return SourceRange{File: cleanPath(position)}
	}

	line, lineOK := parseTrailingInt(parts[len(parts)-2])
	column, columnOK := parseTrailingInt(parts[len(parts)-1])
	if lineOK && columnOK {
		file := strings.Join(parts[:len(parts)-2], ":")
		return SourceRange{File: cleanPath(file), StartLine: line, StartColumn: column, EndLine: line, EndColumn: column}
	}

	line, lineOK = parseTrailingInt(parts[len(parts)-1])
	if !lineOK {
		return SourceRange{File: cleanPath(position)}
	}

	file := strings.Join(parts[:len(parts)-1], ":")
	return SourceRange{File: cleanPath(file), StartLine: line, EndLine: line}
}

func parseTrailingInt(value string) (int, bool) {
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	return parsed, err == nil
}

func diagnosticID(language, severity, position, message string) string {
	return strings.Join([]string{"diagnostic", language, severity, position, message}, ":")
}

func relationshipsFromGraph(graph *codegraph.EvidenceGraph, language string) []CodeRelationship {
	if graph == nil {
		return nil
	}

	relationships := graph.Relationships()
	out := make([]CodeRelationship, 0, len(relationships))
	for i := range relationships {
		relationship := relationships[i]
		provenance := provenanceFromGraph(relationship.Provenance)
		rangeInfo := SourceRange{}
		file := ""
		if len(provenance) > 0 {
			rangeInfo = provenance[0].Range
			file = rangeInfo.File
		}

		out = append(out, CodeRelationship{
			FromID:     string(relationship.From),
			ToID:       string(relationship.To),
			Kind:       relationship.Kind,
			Language:   language,
			File:       file,
			Range:      rangeInfo,
			Provenance: provenance,
		})
	}

	return out
}

func provenanceFromGraph(in []codegraph.Provenance) []Provenance {
	out := make([]Provenance, 0, len(in))
	for i := range in {
		out = append(out, Provenance{
			Source: in[i].Source,
			Range: SourceRange{
				File:        in[i].File,
				StartLine:   in[i].StartLine,
				StartColumn: in[i].StartColumn,
				EndLine:     in[i].EndLine,
				EndColumn:   in[i].EndColumn,
			},
			Confidence: in[i].Confidence,
		})
	}

	return out
}

func mergeModels(models ...Model) Model {
	var merged Model
	for i := range models {
		merged.Files = append(merged.Files, models[i].Files...)
		merged.Symbols = append(merged.Symbols, models[i].Symbols...)
		merged.Definitions = append(merged.Definitions, models[i].Definitions...)
		merged.References = append(merged.References, models[i].References...)
		merged.Diagnostics = append(merged.Diagnostics, models[i].Diagnostics...)
		merged.Relationships = append(merged.Relationships, models[i].Relationships...)
	}
	merged.sort()

	return merged
}

func cloneModel(model Model) Model {
	model.Files = append([]CodeFile(nil), model.Files...)
	for i := range model.Files {
		model.Files[i].Provenance = cloneProvenance(model.Files[i].Provenance)
	}

	model.Symbols = append([]CodeSymbol(nil), model.Symbols...)
	for i := range model.Symbols {
		model.Symbols[i].Provenance = cloneProvenance(model.Symbols[i].Provenance)
	}

	model.Definitions = append([]CodeDefinition(nil), model.Definitions...)
	for i := range model.Definitions {
		model.Definitions[i].Provenance = cloneProvenance(model.Definitions[i].Provenance)
	}

	model.References = append([]CodeReference(nil), model.References...)
	for i := range model.References {
		model.References[i].Provenance = cloneProvenance(model.References[i].Provenance)
	}

	model.Diagnostics = append([]CodeDiagnostic(nil), model.Diagnostics...)
	model.Relationships = cloneCodeRelationships(model.Relationships)

	return model
}

func cloneProvenance(provenance Provenance) Provenance {
	provenance.Build.Tags = append([]string(nil), provenance.Build.Tags...)

	return provenance
}

func cloneCodeRelationships(relationships []CodeRelationship) []CodeRelationship {
	out := append([]CodeRelationship(nil), relationships...)
	for i := range out {
		out[i].Provenance = append([]Provenance(nil), out[i].Provenance...)
		for j := range out[i].Provenance {
			out[i].Provenance[j] = cloneProvenance(out[i].Provenance[j])
		}
	}

	return out
}

func (model Model) sort() {
	sort.Slice(model.Files, func(i, j int) bool { return codeFileLess(model.Files[i], model.Files[j]) })
	sort.Slice(model.Symbols, func(i, j int) bool { return codeSymbolLess(model.Symbols[i], model.Symbols[j]) })
	sort.Slice(model.Definitions, func(i, j int) bool { return codeDefinitionLess(model.Definitions[i], model.Definitions[j]) })
	sort.Slice(model.References, func(i, j int) bool { return codeReferenceLess(model.References[i], model.References[j]) })
	sort.Slice(model.Diagnostics, func(i, j int) bool { return codeDiagnosticLess(model.Diagnostics[i], model.Diagnostics[j]) })
	sort.Slice(model.Relationships, func(i, j int) bool { return codeRelationshipLess(model.Relationships[i], model.Relationships[j]) })
}

func codeFileLess(left, right CodeFile) bool {
	if left.Language != right.Language {
		return left.Language < right.Language
	}

	return filepath.ToSlash(left.Path) < filepath.ToSlash(right.Path)
}

func codeSymbolLess(left, right CodeSymbol) bool {
	if left.Name != right.Name {
		return left.Name < right.Name
	}
	if left.Language != right.Language {
		return left.Language < right.Language
	}
	if left.File != right.File {
		return left.File < right.File
	}
	if left.Range.StartLine != right.Range.StartLine {
		return left.Range.StartLine < right.Range.StartLine
	}

	return left.ID < right.ID
}

func codeDefinitionLess(left, right CodeDefinition) bool {
	if left.Name != right.Name {
		return left.Name < right.Name
	}
	if left.Language != right.Language {
		return left.Language < right.Language
	}
	if left.File != right.File {
		return left.File < right.File
	}
	if left.Range.StartLine != right.Range.StartLine {
		return left.Range.StartLine < right.Range.StartLine
	}

	return left.ID < right.ID
}

func codeReferenceLess(left, right CodeReference) bool {
	if left.Name != right.Name {
		return left.Name < right.Name
	}
	if left.Language != right.Language {
		return left.Language < right.Language
	}
	if left.File != right.File {
		return left.File < right.File
	}
	if left.Range.StartLine != right.Range.StartLine {
		return left.Range.StartLine < right.Range.StartLine
	}
	if left.Range.StartColumn != right.Range.StartColumn {
		return left.Range.StartColumn < right.Range.StartColumn
	}

	return left.ID < right.ID
}

func codeDiagnosticLess(left, right CodeDiagnostic) bool {
	if left.File != right.File {
		return left.File < right.File
	}
	if left.Range.StartLine != right.Range.StartLine {
		return left.Range.StartLine < right.Range.StartLine
	}
	if left.Source != right.Source {
		return left.Source < right.Source
	}

	return left.Message < right.Message
}

func codeRelationshipLess(left, right CodeRelationship) bool {
	if left.FromID != right.FromID {
		return left.FromID < right.FromID
	}
	if left.ToID != right.ToID {
		return left.ToID < right.ToID
	}
	if left.Kind != right.Kind {
		return left.Kind < right.Kind
	}

	return left.Language < right.Language
}
