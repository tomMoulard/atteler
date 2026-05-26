package main

import (
	"strings"

	"github.com/tommoulard/atteler/pkg/codeintel"
)

func summarizeCodeFiles(root string, idx codeintel.Index) []codePackageFile {
	files := make([]codePackageFile, 0, len(idx.Files))
	for i := range idx.Files {
		file := idx.Files[i]
		files = append(files, codePackageFile{
			Path:    relativeCodePath(root, file.Path),
			Package: file.Package,
			Symbols: len(file.Symbols),
			Imports: len(file.Imports),
		})
	}

	sortCodePackageFiles(files)

	return files
}

type codePackageFile struct {
	Path    string
	Package string
	Symbols int
	Imports int
}

func sortCodePackageFiles(files []codePackageFile) {
	sortCodeIntelByNameAsc(files, func(file codePackageFile) string { return file.Path })
}

func codeFilePackagesByPath(idx codeintel.Index) map[string]string {
	packagesByFile := make(map[string]string, len(idx.Files))
	for i := range idx.Files {
		packagesByFile[idx.Files[i].Path] = idx.Files[i].Package
	}

	return packagesByFile
}

func codePackageFileSet(idx codeintel.Index, packageName string) map[string]struct{} {
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

	return files
}

func summarizeCodePackageFiles(root string, idx codeintel.Index, name string) []codePackageFile {
	matches := codePackageFiles(idx, name)
	files := make([]codePackageFile, 0, len(matches))

	for i := range matches {
		file := matches[i]
		files = append(files, codePackageFile{
			Path:    relativeCodePath(root, file.Path),
			Package: file.Package,
			Symbols: len(file.Symbols),
			Imports: len(file.Imports),
		})
	}

	sortCodePackageFiles(files)

	return files
}

type codePackageSummary struct {
	Name    string
	Files   int
	Symbols int
}

func summarizeCodePackages(idx codeintel.Index) []codePackageSummary {
	byPackage := make(map[string]*codePackageSummary)

	for i := range idx.Files {
		name := idx.Files[i].Package
		if name == "" {
			continue
		}

		summary, ok := byPackage[name]
		if !ok {
			summary = &codePackageSummary{Name: name}
			byPackage[name] = summary
		}

		summary.Files++
		summary.Symbols += len(idx.Files[i].Symbols)
	}

	packages := make([]codePackageSummary, 0, len(byPackage))
	for _, summary := range byPackage {
		packages = append(packages, *summary)
	}

	sortCodeIntelByNameAscCountAsc(packages,
		func(summary codePackageSummary) string { return summary.Name },
		func(summary codePackageSummary) int { return summary.Files },
	)

	return packages
}
