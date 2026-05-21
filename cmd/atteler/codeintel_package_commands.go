package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/tommoulard/atteler/pkg/codeintel"
)

func listCodeFiles(root string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code files: index %s: %w", root, err)
	}

	files := summarizeCodeFiles(root, idx)
	if len(files) == 0 {
		fmt.Println("No Go files found.")
		return nil
	}

	for i := range files {
		fmt.Println(formatCodePackageFile(files[i]))
	}

	return nil
}

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

	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })

	return files
}

func listCodePackageFiles(root, name string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code package: index %s: %w", root, err)
	}

	files := summarizeCodePackageFiles(root, idx, name)
	if len(files) == 0 {
		fmt.Println("No Go package files found.")
		return nil
	}

	for i := range files {
		fmt.Println(formatCodePackageFile(files[i]))
	}

	return nil
}

type codePackageFile struct {
	Path    string
	Package string
	Symbols int
	Imports int
}

func summarizeCodePackageFiles(root string, idx codeintel.Index, name string) []codePackageFile {
	name = strings.TrimSpace(name)
	files := make([]codePackageFile, 0)

	for i := range idx.Files {
		file := idx.Files[i]
		if file.Package != name {
			continue
		}

		files = append(files, codePackageFile{
			Path:    relativeCodePath(root, file.Path),
			Package: file.Package,
			Symbols: len(file.Symbols),
			Imports: len(file.Imports),
		})
	}

	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })

	return files
}

func formatCodePackageFile(file codePackageFile) string {
	return "path=" + file.Path + "	package=" + file.Package + "	symbols=" + strconv.Itoa(file.Symbols) + "	imports=" + strconv.Itoa(file.Imports)
}

type codePackageSummary struct {
	Name    string
	Files   int
	Symbols int
}

func listCodePackages(root string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code packages: index %s: %w", root, err)
	}

	packages := summarizeCodePackages(idx)
	if len(packages) == 0 {
		fmt.Println("No Go packages found.")
		return nil
	}

	for i := range packages {
		fmt.Println(formatCodePackageSummary(packages[i]))
	}

	return nil
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

	sort.Slice(packages, func(i, j int) bool {
		if packages[i].Name != packages[j].Name {
			return packages[i].Name < packages[j].Name
		}

		return packages[i].Files < packages[j].Files
	})

	return packages
}

func formatCodePackageSummary(summary codePackageSummary) string {
	return "package=" + summary.Name + "	files=" + strconv.Itoa(summary.Files) + "	symbols=" + strconv.Itoa(summary.Symbols)
}
