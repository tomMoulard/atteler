//nolint:wsl_v5 // Existing tests and query builders use compact assertion/evidence blocks.
package codeintel

import (
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// ImpactQuery asks how a file, import path, or symbol affects the indexed code.
type ImpactQuery struct {
	File       string
	ImportPath string
	SymbolName string
}

// ImpactResult separates import, reference, and public API evidence so callers
// can distinguish why a change may matter.
type ImpactResult struct {
	DirectImports         []Import
	ReverseImports        []Import
	References            []Reference
	Callers               []CallEdge
	PublicAPIDeclarations []Declaration
	Evidence              []Provenance
	Uncertainty           []string
}

// FindDefinitions returns semantic declarations with the exact name.
func (idx Index) FindDefinitions(name string) []Declaration {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}

	matches := make([]Declaration, 0)
	for i := range idx.Declarations {
		if declarationNameMatches(idx.Declarations[i], name) {
			matches = append(matches, idx.Declarations[i])
		}
	}

	sort.Slice(matches, func(i, j int) bool { return declarationLess(matches[i], matches[j]) })

	return matches
}

func declarationNameMatches(declaration Declaration, name string) bool {
	return slices.Contains(declarationQueryNames(declaration), name)
}

func declarationQueryNames(declaration Declaration) []string {
	localName := declaration.Name
	if declaration.Receiver != "" {
		localName = declaration.Receiver + "." + declaration.Name
	}

	names := []string{declaration.Name}
	if declaration.Receiver != "" {
		names = append(names, localName)
	}

	for _, qualifier := range []string{declaration.PackageName, declaration.PackagePath, declaration.PackageID} {
		qualifier = strings.TrimSpace(qualifier)
		if qualifier == "" {
			continue
		}

		names = append(names, qualifier+"."+localName)
	}

	return names
}

// FindReferences returns semantic references to declarations with the exact
// target name or identifier name.
func (idx Index) FindReferences(name string) []Reference {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}

	targets := make(map[string]struct{})
	definitions := idx.FindDefinitions(name)
	for i := range definitions {
		targets[definitions[i].ID] = struct{}{}
	}

	matches := make([]Reference, 0)
	for i := range idx.References {
		reference := &idx.References[i]
		if reference.Name == name {
			matches = append(matches, *reference)
			continue
		}
		if _, ok := targets[reference.ToDeclarationID]; ok {
			matches = append(matches, *reference)
		}
	}

	sort.Slice(matches, func(i, j int) bool { return referenceLess(matches[i], matches[j]) })

	return matches
}

// PublicAPI returns exported declarations, optionally scoped to a package name,
// package path, or package ID.
func (idx Index) PublicAPI(packageNameOrPath string) []Declaration {
	packageNameOrPath = strings.TrimSpace(packageNameOrPath)

	matches := make([]Declaration, 0)
	for i := range idx.Declarations {
		declaration := idx.Declarations[i]
		if !declaration.Exported {
			continue
		}
		if packageNameOrPath != "" && declaration.PackageName != packageNameOrPath && declaration.PackagePath != packageNameOrPath && declaration.PackageID != packageNameOrPath {
			continue
		}

		matches = append(matches, declaration)
	}

	sort.Slice(matches, func(i, j int) bool { return declarationLess(matches[i], matches[j]) })

	return matches
}

// FindImports returns imports matching the exact import path.
func (idx Index) FindImports(importPath string) []Import {
	importPath = strings.TrimSpace(importPath)
	if importPath == "" {
		return nil
	}

	matches := make([]Import, 0)
	for i := range idx.Imports {
		imp := &idx.Imports[i]
		if imp.Path == importPath || imp.ResolvedPackagePath == importPath {
			matches = append(matches, *imp)
		}
	}

	sort.Slice(matches, func(i, j int) bool { return importLess(matches[i], matches[j]) })

	return matches
}

// FindCallers returns call edges whose callee is a declaration matching name.
func (idx Index) FindCallers(name string) []CallEdge {
	targets := idx.definitionIDs(name)
	if len(targets) == 0 {
		return nil
	}

	matches := make([]CallEdge, 0)
	for i := range idx.CallEdges {
		if _, ok := targets[idx.CallEdges[i].CalleeID]; ok {
			matches = append(matches, idx.CallEdges[i])
		}
	}

	sort.Slice(matches, func(i, j int) bool { return callEdgeLess(matches[i], matches[j]) })

	return matches
}

// FindCallees returns call edges whose caller is a declaration matching name.
func (idx Index) FindCallees(name string) []CallEdge {
	callers := idx.definitionIDs(name)
	if len(callers) == 0 {
		return nil
	}

	matches := make([]CallEdge, 0)
	for i := range idx.CallEdges {
		if _, ok := callers[idx.CallEdges[i].CallerID]; ok {
			matches = append(matches, idx.CallEdges[i])
		}
	}

	sort.Slice(matches, func(i, j int) bool { return callEdgeLess(matches[i], matches[j]) })

	return matches
}

func (idx Index) definitionIDs(name string) map[string]struct{} {
	definitions := idx.FindDefinitions(name)
	if len(definitions) == 0 {
		return nil
	}

	ids := make(map[string]struct{}, len(definitions))
	for i := range definitions {
		ids[definitions[i].ID] = struct{}{}
	}

	return ids
}

// AnalyzeImpact answers file/import/symbol impact questions with evidence and
// uncertainty instead of returning naked graph node IDs.
func (idx Index) AnalyzeImpact(query ImpactQuery) ImpactResult {
	var result ImpactResult

	if file := strings.TrimSpace(query.File); file != "" {
		result.DirectImports = append(result.DirectImports, idx.directImportsForFile(file)...)
		if len(result.DirectImports) == 0 {
			result.Uncertainty = append(result.Uncertainty, "no direct imports found for file")
		}
	}

	if importPath := strings.TrimSpace(query.ImportPath); importPath != "" {
		result.ReverseImports = append(result.ReverseImports, idx.FindImports(importPath)...)
		if len(result.ReverseImports) == 0 {
			result.Uncertainty = append(result.Uncertainty, "no reverse imports found for import path")
		}
	}

	if symbolName := strings.TrimSpace(query.SymbolName); symbolName != "" {
		result.References = append(result.References, idx.FindReferences(symbolName)...)
		result.Callers = append(result.Callers, idx.FindCallers(symbolName)...)
		definitions := idx.FindDefinitions(symbolName)
		for i := range definitions {
			if definitions[i].Exported {
				result.PublicAPIDeclarations = append(result.PublicAPIDeclarations, definitions[i])
			}
		}
		if len(result.References) == 0 {
			result.Uncertainty = append(result.Uncertainty, "no references found for symbol")
		}
		if len(result.PublicAPIDeclarations) == 0 {
			result.Uncertainty = append(result.Uncertainty, "symbol is not part of the exported API boundary")
		}
	}

	result.Evidence = impactEvidence(result)
	result.Uncertainty = append(result.Uncertainty, confidenceUncertainty(result.Evidence)...)
	return result
}

func (idx Index) directImportsForFile(file string) []Import {
	matches := make([]Import, 0)
	for i := range idx.Imports {
		imp := &idx.Imports[i]
		if fileQueryMatches(imp.File, file) {
			matches = append(matches, *imp)
		}
	}

	sort.Slice(matches, func(i, j int) bool { return importLess(matches[i], matches[j]) })

	return matches
}

func fileQueryMatches(indexed, query string) bool {
	indexed = filepath.ToSlash(filepath.Clean(strings.TrimSpace(indexed)))
	query = filepath.ToSlash(filepath.Clean(strings.TrimSpace(query)))
	if indexed == "" || query == "" || query == "." {
		return false
	}
	if filepath.IsAbs(query) {
		return indexed == query
	}
	if indexed == query || strings.HasSuffix(indexed, "/"+query) {
		return true
	}
	if abs, err := filepath.Abs(query); err == nil && indexed == filepath.ToSlash(filepath.Clean(abs)) {
		return true
	}

	return false
}

func impactEvidence(result ImpactResult) []Provenance {
	var evidence []Provenance
	for i := range result.DirectImports {
		evidence = append(evidence, result.DirectImports[i].Provenance)
	}
	for i := range result.ReverseImports {
		evidence = append(evidence, result.ReverseImports[i].Provenance)
	}
	for i := range result.References {
		evidence = append(evidence, result.References[i].Provenance)
	}
	for i := range result.Callers {
		evidence = append(evidence, result.Callers[i].Provenance)
	}
	for i := range result.PublicAPIDeclarations {
		evidence = append(evidence, result.PublicAPIDeclarations[i].Provenance)
	}

	sort.Slice(evidence, func(i, j int) bool {
		if evidence[i].Range.File != evidence[j].Range.File {
			return evidence[i].Range.File < evidence[j].Range.File
		}
		if evidence[i].Range.StartLine != evidence[j].Range.StartLine {
			return evidence[i].Range.StartLine < evidence[j].Range.StartLine
		}
		if evidence[i].Range.StartColumn != evidence[j].Range.StartColumn {
			return evidence[i].Range.StartColumn < evidence[j].Range.StartColumn
		}

		return evidence[i].Source < evidence[j].Source
	})

	return evidence
}

func confidenceUncertainty(evidence []Provenance) []string {
	seen := make(map[string]struct{})
	var uncertainty []string
	for i := range evidence {
		confidence := strings.TrimSpace(evidence[i].Confidence)
		if confidence == "" || confidence == "high" {
			continue
		}

		message := "evidence resolved with " + confidence + " confidence"
		if evidence[i].Source != "" {
			message += " from " + evidence[i].Source
		}
		if evidence[i].Range.File != "" {
			message += " at " + evidence[i].Range.File
			if evidence[i].Range.StartLine > 0 {
				message += ":" + strconv.Itoa(evidence[i].Range.StartLine)
			}
		}
		if _, ok := seen[message]; ok {
			continue
		}

		seen[message] = struct{}{}
		uncertainty = append(uncertainty, message)
	}

	sort.Strings(uncertainty)

	return uncertainty
}
