//nolint:wsl_v5 // Query filtering keeps compact, deterministic switch branches.
package codeintel

import "strings"

// QueryKind selects the language-neutral query surface backed by Index.Model.
type QueryKind string

const (
	// QueryFiles returns indexed files.
	QueryFiles QueryKind = "files"
	// QuerySymbols returns named symbols.
	QuerySymbols QueryKind = "symbols"
	// QueryDefinitions returns definitions/declarations.
	QueryDefinitions QueryKind = "definitions"
	// QueryReferences returns references.
	QueryReferences QueryKind = "references"
	// QueryDiagnostics returns diagnostics.
	QueryDiagnostics QueryKind = "diagnostics"
	// QueryRelationships returns typed code graph relationships.
	QueryRelationships QueryKind = "relationships"
)

// Query is a small language-neutral query shape the CLI can sit on top of
// instead of adding one flag for every index projection.
type Query struct {
	Kind             QueryKind
	Name             string
	File             string
	Language         string
	FromID           string
	ToID             string
	RelationshipKind string
}

// QueryResult contains the model records selected by Query plus uncertainty
// for empty or unsupported requests.
type QueryResult struct {
	Files         []CodeFile
	Symbols       []CodeSymbol
	Definitions   []CodeDefinition
	References    []CodeReference
	Diagnostics   []CodeDiagnostic
	Relationships []CodeRelationship
	Uncertainty   []string
}

// Query executes a language-neutral model query against idx.
func (idx Index) Query(query Query) QueryResult {
	model := idx.Model
	if !modelHasRecords(model) {
		model = modelFromGoIndex(idx)
	}

	return model.Query(query)
}

// Query executes a language-neutral model query.
func (model Model) Query(query Query) QueryResult {
	query = query.normalized()

	var result QueryResult
	switch query.Kind {
	case QueryFiles:
		result.Files = query.filterFiles(model.Files)
	case QuerySymbols:
		result.Symbols = query.filterSymbols(model.Symbols)
	case QueryDefinitions, "":
		result.Definitions = query.filterDefinitions(model.Definitions)
	case QueryReferences:
		result.References = query.filterReferences(model.References)
	case QueryDiagnostics:
		result.Diagnostics = query.filterDiagnostics(model.Diagnostics)
	case QueryRelationships:
		result.Relationships = query.filterRelationships(model.Relationships)
	default:
		result.Uncertainty = append(result.Uncertainty, "unsupported code-intelligence query kind: "+string(query.Kind))
	}

	if result.empty() && len(result.Uncertainty) == 0 {
		result.Uncertainty = append(result.Uncertainty, "no code-intelligence records matched the query")
	}

	return result
}

func (query Query) normalized() Query {
	query.Kind = QueryKind(strings.ToLower(strings.TrimSpace(string(query.Kind))))
	query.Name = strings.TrimSpace(query.Name)
	query.File = strings.TrimSpace(query.File)
	query.Language = strings.ToLower(strings.TrimSpace(query.Language))
	query.FromID = strings.TrimSpace(query.FromID)
	query.ToID = strings.TrimSpace(query.ToID)
	query.RelationshipKind = strings.ToLower(strings.TrimSpace(query.RelationshipKind))

	return query
}

func (query Query) filterFiles(files []CodeFile) []CodeFile {
	matches := make([]CodeFile, 0)
	for i := range files {
		file := files[i]
		if !query.languageMatches(file.Language) || !query.fileMatches(file.Path) {
			continue
		}
		matches = append(matches, file)
	}

	return matches
}

func (query Query) filterSymbols(symbols []CodeSymbol) []CodeSymbol {
	matches := make([]CodeSymbol, 0)
	for i := range symbols {
		symbol := symbols[i]
		if query.Name != "" && symbol.Name != query.Name {
			continue
		}
		if !query.languageMatches(symbol.Language) || !query.fileMatches(symbol.File) {
			continue
		}
		matches = append(matches, symbol)
	}

	return matches
}

func (query Query) filterDefinitions(definitions []CodeDefinition) []CodeDefinition {
	matches := make([]CodeDefinition, 0)
	for i := range definitions {
		definition := definitions[i]
		if query.Name != "" && !codeDefinitionNameMatches(definition, query.Name) {
			continue
		}
		if !query.languageMatches(definition.Language) || !query.fileMatches(definition.File) {
			continue
		}
		matches = append(matches, definition)
	}

	return matches
}

func (query Query) filterReferences(references []CodeReference) []CodeReference {
	matches := make([]CodeReference, 0)
	for i := range references {
		reference := references[i]
		if query.Name != "" && reference.Name != query.Name && reference.ToID != query.Name {
			continue
		}
		if !query.languageMatches(reference.Language) || !query.fileMatches(reference.File) {
			continue
		}
		matches = append(matches, reference)
	}

	return matches
}

func (query Query) filterDiagnostics(diagnostics []CodeDiagnostic) []CodeDiagnostic {
	matches := make([]CodeDiagnostic, 0)
	for i := range diagnostics {
		diagnostic := diagnostics[i]
		if query.Name != "" && !diagnosticMatches(diagnostic, query.Name) {
			continue
		}
		if !query.languageMatches(diagnostic.Language) || !query.fileMatches(diagnostic.File) {
			continue
		}
		matches = append(matches, diagnostic)
	}

	return matches
}

func (query Query) filterRelationships(relationships []CodeRelationship) []CodeRelationship {
	matches := make([]CodeRelationship, 0)
	for i := range relationships {
		relationship := relationships[i]
		if query.RelationshipKind != "" && relationship.Kind != query.RelationshipKind {
			continue
		}
		if query.FromID != "" && relationship.FromID != query.FromID {
			continue
		}
		if query.ToID != "" && relationship.ToID != query.ToID {
			continue
		}
		if !query.languageMatches(relationship.Language) || !query.fileMatches(relationship.File) {
			continue
		}
		matches = append(matches, relationship)
	}

	return matches
}

func codeDefinitionNameMatches(definition CodeDefinition, name string) bool {
	if definition.Name == name || definition.ID == name {
		return true
	}
	if definition.ContainerName != "" && definition.ContainerName+"."+definition.Name == name {
		return true
	}

	return false
}

func diagnosticMatches(diagnostic CodeDiagnostic, value string) bool {
	return containsFold(diagnostic.Message, value) ||
		containsFold(diagnostic.Severity, value) ||
		containsFold(diagnostic.Source, value) ||
		containsFold(diagnostic.ID, value)
}

func containsFold(value, substr string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	substr = strings.ToLower(strings.TrimSpace(substr))

	return substr != "" && strings.Contains(value, substr)
}

func (query Query) languageMatches(language string) bool {
	return query.Language == "" || language == query.Language
}

func (query Query) fileMatches(file string) bool {
	return query.File == "" || fileQueryMatches(file, query.File)
}

func (result QueryResult) empty() bool {
	return len(result.Files) == 0 &&
		len(result.Symbols) == 0 &&
		len(result.Definitions) == 0 &&
		len(result.References) == 0 &&
		len(result.Diagnostics) == 0 &&
		len(result.Relationships) == 0
}

func modelHasRecords(model Model) bool {
	return len(model.Files) > 0 ||
		len(model.Symbols) > 0 ||
		len(model.Definitions) > 0 ||
		len(model.References) > 0 ||
		len(model.Diagnostics) > 0 ||
		len(model.Relationships) > 0
}
