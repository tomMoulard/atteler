package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/tommoulard/atteler/pkg/codeintel"
)

func buildCodeIntelModelQueryResponse(ctx context.Context, root string, input codeIntelCommandInput, commandName string, textKind codeIntelTextKind) (codeIntelResponse, error) {
	query, queryValues, err := codeIntelModelQueryFromInput(input)
	if err != nil {
		return codeIntelResponse{}, err
	}

	index, err := codeintel.NewWorkspaceIndexer(codeintel.WorkspaceIndexOptions{
		CachePath: defaultCodeIntelWorkspaceCachePath(root),
	}).IndexDirContext(ctx, root)
	if err != nil {
		return codeIntelResponse{}, fmt.Errorf("%s: index %s: %w", commandName, root, err)
	}

	result := index.Query(query)
	response := newCodeIntelResponse(commandName)
	response.TextKind = textKind
	response.Query = queryValues
	response.Records = codeIntelRecordsFromQueryResult(root, result)
	response.Uncertainty = append([]string(nil), result.Uncertainty...)

	return finalizeCodeIntelResponse(paginateCodeIntelResponse(response, input)), nil
}

func defaultCodeIntelWorkspaceCachePath(root string) string {
	return filepath.Join(root, ".atteler", "codeintel-index.json")
}

func codeIntelModelQueryFromInput(input codeIntelCommandInput) (codeintel.Query, map[string]string, error) {
	raw := strings.TrimSpace(input.ModelQuery)
	if raw == "" {
		return codeintel.Query{}, nil, fmt.Errorf("%s requires a query kind", codeIntelQueryCommandName)
	}

	kind, value, _ := strings.Cut(raw, ":")
	kind = strings.ToLower(strings.TrimSpace(kind))
	value = strings.TrimSpace(value)

	query := codeintel.Query{Kind: codeintel.QueryKind(kind), Language: strings.ToLower(strings.TrimSpace(input.ModelLanguage))}
	switch query.Kind {
	case codeintel.QueryFiles:
		query.File = value
	case codeintel.QuerySymbols, codeintel.QueryDefinitions, codeintel.QueryReferences, codeintel.QueryDiagnostics:
		query.Name = value
	case codeintel.QueryRelationships:
		query.RelationshipKind = strings.ToLower(value)
	case "defs":
		query.Kind = codeintel.QueryDefinitions
		query.Name = value
	case "refs":
		query.Kind = codeintel.QueryReferences
		query.Name = value
	case "diags":
		query.Kind = codeintel.QueryDiagnostics
		query.Name = value
	case "relations", codeintel.QueryKind(codeIntelTextEdges):
		query.Kind = codeintel.QueryRelationships
		query.RelationshipKind = strings.ToLower(value)
	default:
		return codeintel.Query{}, nil, fmt.Errorf("unsupported %s kind %q; use files, symbols, definitions, references, diagnostics, or relationships", codeIntelQueryCommandName, kind)
	}

	values := map[string]string{"kind": string(query.Kind)}
	if queryValue := codeIntelModelQueryValue(query); queryValue != "" {
		values["value"] = queryValue
	}

	if query.Language != "" {
		values["language"] = query.Language
	}

	return query, values, nil
}

func codeIntelModelQueryValue(query codeintel.Query) string {
	switch query.Kind {
	case codeintel.QueryFiles:
		return query.File
	case codeintel.QuerySymbols, codeintel.QueryDefinitions, codeintel.QueryReferences, codeintel.QueryDiagnostics:
		return query.Name
	case codeintel.QueryRelationships:
		return query.RelationshipKind
	default:
		return ""
	}
}

func codeIntelRecordsFromQueryResult(root string, result codeintel.QueryResult) []codeIntelRecord {
	records := make([]codeIntelRecord, 0,
		len(result.Files)+len(result.Symbols)+len(result.Definitions)+len(result.References)+len(result.Diagnostics)+len(result.Relationships))

	for i := range result.Files {
		records = append(records, codeIntelRecordFromFile(root, result.Files[i]))
	}

	for i := range result.Symbols {
		records = append(records, codeIntelRecordFromSymbol(root, result.Symbols[i]))
	}

	for i := range result.Definitions {
		records = append(records, codeIntelRecordFromDefinition(root, result.Definitions[i]))
	}

	for i := range result.References {
		records = append(records, codeIntelRecordFromReference(root, result.References[i]))
	}

	for i := range result.Diagnostics {
		records = append(records, codeIntelRecordFromDiagnostic(root, result.Diagnostics[i]))
	}

	for i := range result.Relationships {
		records = append(records, codeIntelRecordFromRelationship(root, result.Relationships[i]))
	}

	return records
}

func codeIntelRecordFromFile(root string, file codeintel.CodeFile) codeIntelRecord {
	return codeIntelRecordWithRange(root, codeIntelRecord{
		Type:     "file",
		ID:       file.ID,
		Language: file.Language,
		Path:     relativeCodePath(root, file.Path),
	}, file.Range)
}

func codeIntelRecordFromSymbol(root string, symbol codeintel.CodeSymbol) codeIntelRecord {
	return codeIntelRecordWithRange(root, codeIntelRecord{
		Type:     "symbol",
		ID:       symbol.ID,
		Name:     symbol.Name,
		Kind:     symbol.Kind,
		Language: symbol.Language,
		Path:     relativeCodePath(root, symbol.File),
		ToID:     symbol.DefinitionID,
	}, symbol.Range)
}

func codeIntelRecordFromDefinition(root string, definition codeintel.CodeDefinition) codeIntelRecord {
	return codeIntelRecordWithRange(root, codeIntelRecord{
		Type:     "definition",
		ID:       definition.ID,
		Name:     definition.Name,
		Kind:     definition.Kind,
		Language: definition.Language,
		Path:     relativeCodePath(root, definition.File),
	}, definition.Range)
}

func codeIntelRecordFromReference(root string, reference codeintel.CodeReference) codeIntelRecord {
	return codeIntelRecordWithRange(root, codeIntelRecord{
		Type:     "reference",
		ID:       reference.ID,
		Name:     reference.Name,
		Kind:     reference.Kind,
		Language: reference.Language,
		Path:     relativeCodePath(root, reference.File),
		FromID:   reference.FromID,
		ToID:     reference.ToID,
	}, reference.Range)
}

func codeIntelRecordFromDiagnostic(root string, diagnostic codeintel.CodeDiagnostic) codeIntelRecord {
	return codeIntelRecordWithRange(root, codeIntelRecord{
		Type:     "diagnostic",
		ID:       diagnostic.ID,
		Language: diagnostic.Language,
		Path:     relativeCodePath(root, diagnostic.File),
		Source:   diagnostic.Source,
		Severity: diagnostic.Severity,
		Message:  diagnostic.Message,
	}, diagnostic.Range)
}

func codeIntelRecordFromRelationship(root string, relationship codeintel.CodeRelationship) codeIntelRecord {
	return codeIntelRecordWithRange(root, codeIntelRecord{
		Type:             "relationship",
		Kind:             relationship.Kind,
		Language:         relationship.Language,
		Path:             relativeCodePath(root, relationship.File),
		FromID:           relationship.FromID,
		ToID:             relationship.ToID,
		RelationshipKind: relationship.Kind,
	}, relationship.Range)
}

func codeIntelRecordWithRange(root string, record codeIntelRecord, sourceRange codeintel.SourceRange) codeIntelRecord {
	if record.Path == "" && sourceRange.File != "" {
		record.Path = relativeCodePath(root, sourceRange.File)
	}

	record.Line = sourceRange.StartLine
	record.Column = sourceRange.StartColumn
	record.EndLine = sourceRange.EndLine
	record.EndColumn = sourceRange.EndColumn

	return record
}
