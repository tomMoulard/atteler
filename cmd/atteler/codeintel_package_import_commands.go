package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/tommoulard/atteler/pkg/codeintel"
)

func listCodePackageImportFileSummary(root, packageName string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code package import file summary: index %s: %w", root, err)
	}

	summaries := summarizeCodePackageImportFiles(root, idx, packageName)
	if len(summaries) == 0 {
		fmt.Println("No Go package imports found.")
		return nil
	}

	for i := range summaries {
		fmt.Println(formatCodeImportFileSummary(summaries[i]))
	}

	return nil
}

func summarizeCodePackageImportFiles(root string, idx codeintel.Index, packageName string) []codeImportFileSummary {
	packageName = strings.TrimSpace(packageName)
	if packageName == "" {
		return nil
	}

	summaries := make([]codeImportFileSummary, 0)

	for i := range idx.Files {
		file := idx.Files[i]
		if file.Package != packageName || len(file.Imports) == 0 {
			continue
		}

		summaries = append(summaries, codeImportFileSummary{
			Path:    relativeCodePath(root, file.Path),
			Package: file.Package,
			Imports: len(file.Imports),
		})
	}

	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].Imports != summaries[j].Imports {
			return summaries[i].Imports > summaries[j].Imports
		}

		return summaries[i].Path < summaries[j].Path
	})

	return summaries
}

func listCodePackageImportPrefixFileSummary(root, spec string) error {
	packageName, prefix, err := parseCodeFileSymbolFilterSpec(spec, "code package import prefix file summary", "package:prefix")
	if err != nil {
		return err
	}

	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code package import prefix file summary: index %s: %w", root, err)
	}

	summaries := summarizeCodePackageImportPrefixFiles(root, idx, packageName, prefix)
	if len(summaries) == 0 {
		fmt.Println("No Go package import files found.")
		return nil
	}

	for i := range summaries {
		fmt.Println(formatCodeImportFileSummary(summaries[i]))
	}

	return nil
}

func summarizeCodePackageImportPrefixFiles(root string, idx codeintel.Index, packageName, prefix string) []codeImportFileSummary {
	edges := codePackageImportPrefixFiles(idx, packageName, prefix)
	if len(edges) == 0 {
		return edgesToCodeImportFileSummaries(root, idx, nil)
	}

	return edgesToCodeImportFileSummaries(root, idx, edges)
}

func edgesToCodeImportFileSummaries(root string, idx codeintel.Index, edges []codeintel.ImportEdge) []codeImportFileSummary {
	if len(edges) == 0 {
		return nil
	}

	packagesByFile := make(map[string]string, len(idx.Files))
	for i := range idx.Files {
		packagesByFile[idx.Files[i].Path] = idx.Files[i].Package
	}

	importsByFile := make(map[string]map[string]struct{})

	for i := range edges {
		edge := edges[i]
		if _, ok := importsByFile[edge.From]; !ok {
			importsByFile[edge.From] = make(map[string]struct{})
		}

		importsByFile[edge.From][edge.Import] = struct{}{}
	}

	summaries := make([]codeImportFileSummary, 0, len(importsByFile))
	for file, imports := range importsByFile {
		summaries = append(summaries, codeImportFileSummary{
			Path:    relativeCodePath(root, file),
			Package: packagesByFile[file],
			Imports: len(imports),
		})
	}

	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].Imports != summaries[j].Imports {
			return summaries[i].Imports > summaries[j].Imports
		}

		return summaries[i].Path < summaries[j].Path
	})

	return summaries
}

func listCodePackageImportPrefixFiles(root, spec string) error {
	packageName, prefix, err := parseCodeFileSymbolFilterSpec(spec, "code package import prefix files", "package:prefix")
	if err != nil {
		return err
	}

	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code package import prefix files: index %s: %w", root, err)
	}

	edges := codePackageImportPrefixFiles(idx, packageName, prefix)
	if len(edges) == 0 {
		fmt.Println("No Go package import files found.")
		return nil
	}

	for i := range edges {
		fmt.Println(formatCodeImportEdge(root, edges[i]))
	}

	return nil
}

func codePackageImportPrefixFiles(idx codeintel.Index, packageName, prefix string) []codeintel.ImportEdge {
	packageName = strings.TrimSpace(packageName)

	prefix = strings.TrimSpace(prefix)
	if packageName == "" || prefix == "" {
		return nil
	}

	packageFiles := make(map[string]struct{})

	for i := range idx.Files {
		if idx.Files[i].Package == packageName {
			packageFiles[idx.Files[i].Path] = struct{}{}
		}
	}

	if len(packageFiles) == 0 {
		return nil
	}

	seen := make(map[string]struct{})
	edges := make([]codeintel.ImportEdge, 0)

	for i := range idx.ImportEdges {
		edge := idx.ImportEdges[i]
		if !strings.HasPrefix(edge.Import, prefix) {
			continue
		}

		if _, ok := packageFiles[edge.From]; !ok {
			continue
		}

		seenKey := edge.From + "\x00" + edge.Import
		if _, ok := seen[seenKey]; ok {
			continue
		}

		seen[seenKey] = struct{}{}

		edges = append(edges, edge)
	}

	sortCodeImportEdges(edges)

	return edges
}

func listCodePackageImportFiles(root, spec string) error {
	packageName, importPath, err := parseCodeFileSymbolFilterSpec(spec, "code package import files", "package:import")
	if err != nil {
		return err
	}

	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code package import files: index %s: %w", root, err)
	}

	files := codePackageImportFiles(root, idx, packageName, importPath)
	if len(files) == 0 {
		fmt.Println("No Go package import files found.")
		return nil
	}

	for _, file := range files {
		fmt.Println("path=" + file + "	import=" + importPath)
	}

	return nil
}

func codePackageImportFiles(root string, idx codeintel.Index, packageName, importPath string) []string {
	packageName = strings.TrimSpace(packageName)

	importPath = strings.TrimSpace(importPath)
	if packageName == "" || importPath == "" {
		return nil
	}

	packageFiles := make(map[string]struct{})

	for i := range idx.Files {
		if idx.Files[i].Package == packageName {
			packageFiles[idx.Files[i].Path] = struct{}{}
		}
	}

	if len(packageFiles) == 0 {
		return nil
	}

	seen := make(map[string]struct{})
	files := make([]string, 0)

	for i := range idx.ImportEdges {
		edge := idx.ImportEdges[i]
		if edge.Import != importPath {
			continue
		}

		if _, ok := packageFiles[edge.From]; !ok {
			continue
		}

		rel := relativeCodePath(root, edge.From)
		if _, ok := seen[rel]; ok {
			continue
		}

		seen[rel] = struct{}{}
		files = append(files, rel)
	}

	sort.Strings(files)

	return files
}

func listCodePackageImportPathFileSummary(root, spec string) error {
	packageName, importPath, err := parseCodeFileSymbolFilterSpec(spec, "code package import path file summary", "package:import")
	if err != nil {
		return err
	}

	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code package import path file summary: index %s: %w", root, err)
	}

	summaries := summarizeCodePackageImportPathFiles(root, idx, packageName, importPath)
	if len(summaries) == 0 {
		fmt.Println("No Go package import files found.")
		return nil
	}

	for i := range summaries {
		fmt.Println(formatCodeImportFileSummary(summaries[i]))
	}

	return nil
}

func summarizeCodePackageImportPathFiles(root string, idx codeintel.Index, packageName, importPath string) []codeImportFileSummary {
	packageName = strings.TrimSpace(packageName)

	importPath = strings.TrimSpace(importPath)
	if packageName == "" || importPath == "" {
		return nil
	}

	packageFiles := make(map[string]struct{})

	for i := range idx.Files {
		if idx.Files[i].Package == packageName {
			packageFiles[idx.Files[i].Path] = struct{}{}
		}
	}

	if len(packageFiles) == 0 {
		return nil
	}

	files := make(map[string]struct{})

	for i := range idx.ImportEdges {
		edge := idx.ImportEdges[i]
		if edge.Import != importPath {
			continue
		}

		if _, ok := packageFiles[edge.From]; !ok {
			continue
		}

		files[edge.From] = struct{}{}
	}

	summaries := make([]codeImportFileSummary, 0, len(files))
	for file := range files {
		summaries = append(summaries, codeImportFileSummary{
			Path:    relativeCodePath(root, file),
			Package: packageName,
			Imports: 1,
		})
	}

	sort.Slice(summaries, func(i, j int) bool {
		return summaries[i].Path < summaries[j].Path
	})

	return summaries
}

func listCodePackageImportPath(root, spec string) error {
	packageName, importPath, err := parseCodeFileSymbolFilterSpec(spec, "code package import path", "package:import")
	if err != nil {
		return err
	}

	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code package import path: index %s: %w", root, err)
	}

	summaries := summarizeCodePackageImportPath(idx, packageName, importPath)
	if len(summaries) == 0 {
		fmt.Println("No Go package imports found.")
		return nil
	}

	for i := range summaries {
		fmt.Println(formatCodeImportSummary(summaries[i]))
	}

	return nil
}

func summarizeCodePackageImportPath(idx codeintel.Index, packageName, importPath string) []codeImportSummary {
	packageName = strings.TrimSpace(packageName)

	importPath = strings.TrimSpace(importPath)
	if packageName == "" || importPath == "" {
		return nil
	}

	all := summarizeCodePackageImports(idx, packageName)
	filtered := make([]codeImportSummary, 0, 1)

	for i := range all {
		if all[i].Path == importPath {
			filtered = append(filtered, all[i])
		}
	}

	return filtered
}

func listCodePackageImportPrefix(root, spec string) error {
	packageName, prefix, err := parseCodeFileSymbolFilterSpec(spec, "code package import prefix", "package:prefix")
	if err != nil {
		return err
	}

	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code package import prefix: index %s: %w", root, err)
	}

	summaries := summarizeCodePackageImportPrefix(idx, packageName, prefix)
	if len(summaries) == 0 {
		fmt.Println("No Go package imports found.")
		return nil
	}

	for i := range summaries {
		fmt.Println(formatCodeImportSummary(summaries[i]))
	}

	return nil
}

func summarizeCodePackageImportPrefix(idx codeintel.Index, packageName, prefix string) []codeImportSummary {
	packageName = strings.TrimSpace(packageName)

	prefix = strings.TrimSpace(prefix)
	if packageName == "" || prefix == "" {
		return nil
	}

	all := summarizeCodePackageImports(idx, packageName)

	filtered := make([]codeImportSummary, 0, len(all))
	for i := range all {
		if strings.HasPrefix(all[i].Path, prefix) {
			filtered = append(filtered, all[i])
		}
	}

	return filtered
}

func listCodePackageImports(root, packageName string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code package imports: index %s: %w", root, err)
	}

	summaries := summarizeCodePackageImports(idx, packageName)
	if len(summaries) == 0 {
		fmt.Println("No Go package imports found.")
		return nil
	}

	for i := range summaries {
		fmt.Println(formatCodeImportSummary(summaries[i]))
	}

	return nil
}

func summarizeCodePackageImports(idx codeintel.Index, packageName string) []codeImportSummary {
	packageName = strings.TrimSpace(packageName)
	if packageName == "" {
		return nil
	}

	files := make(map[string]struct{})

	for i := range idx.Files {
		if idx.Files[i].Package == packageName {
			files[idx.Files[i].Path] = struct{}{}
		}
	}

	if len(files) == 0 {
		return nil
	}

	filtered := codeintel.Index{ImportEdges: make([]codeintel.ImportEdge, 0)}
	for i := range idx.ImportEdges {
		if _, ok := files[idx.ImportEdges[i].From]; ok {
			filtered.ImportEdges = append(filtered.ImportEdges, idx.ImportEdges[i])
		}
	}

	return summarizeCodeImports(filtered)
}

type codePackageImportSummary struct {
	Name          string
	Files         int
	Imports       int
	UniqueImports int
}

type codePackageImportMatchSummary struct {
	Name    string
	Files   int
	Imports int
}

func formatCodePackageImportMatchSummary(summary codePackageImportMatchSummary) string {
	parts := []string{
		"package=" + summary.Name,
		"files=" + strconv.Itoa(summary.Files),
	}
	if summary.Imports > 0 {
		parts = append(parts, "imports="+strconv.Itoa(summary.Imports))
	}

	return strings.Join(parts, "\t")
}

func sortCodePackageImportMatchSummaries(summaries []codePackageImportMatchSummary) {
	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].Imports != summaries[j].Imports {
			return summaries[i].Imports > summaries[j].Imports
		}

		if summaries[i].Files != summaries[j].Files {
			return summaries[i].Files > summaries[j].Files
		}

		return summaries[i].Name < summaries[j].Name
	})
}

func listCodePackageImportSummary(root string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code package import summary: index %s: %w", root, err)
	}

	summaries := summarizeCodePackageImportCounts(idx)
	if len(summaries) == 0 {
		fmt.Println("No Go package imports found.")
		return nil
	}

	for i := range summaries {
		fmt.Println(formatCodePackageImportSummary(summaries[i]))
	}

	return nil
}

func summarizeCodePackageImportCounts(idx codeintel.Index) []codePackageImportSummary {
	byPackage := make(map[string]*codePackageImportSummary)
	uniqueByPackage := make(map[string]map[string]struct{})

	for i := range idx.Files {
		file := idx.Files[i]
		if file.Package == "" {
			continue
		}

		summary, ok := byPackage[file.Package]
		if !ok {
			summary = &codePackageImportSummary{Name: file.Package}
			byPackage[file.Package] = summary
			uniqueByPackage[file.Package] = make(map[string]struct{})
		}

		summary.Files++

		summary.Imports += len(file.Imports)
		for _, imp := range file.Imports {
			if imp != "" {
				uniqueByPackage[file.Package][imp] = struct{}{}
			}
		}
	}

	summaries := make([]codePackageImportSummary, 0, len(byPackage))
	for name, summary := range byPackage {
		summary.UniqueImports = len(uniqueByPackage[name])
		if summary.Imports == 0 {
			continue
		}

		summaries = append(summaries, *summary)
	}

	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].Imports != summaries[j].Imports {
			return summaries[i].Imports > summaries[j].Imports
		}

		return summaries[i].Name < summaries[j].Name
	})

	return summaries
}

func formatCodePackageImportSummary(summary codePackageImportSummary) string {
	return "package=" + summary.Name + "	files=" + strconv.Itoa(summary.Files) + "	imports=" + strconv.Itoa(summary.Imports) + "	unique_imports=" + strconv.Itoa(summary.UniqueImports)
}
