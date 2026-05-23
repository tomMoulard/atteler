//nolint:wsl_v5 // Category builders keep related schema-field assignments compact and auditable.
package main

import "github.com/tommoulard/atteler/pkg/codeintel"

func buildCodeIntelImportResponse(root string, idx codeintel.Index, input codeIntelCommandInput, response codeIntelResponse, commandName string) (codeIntelResponse, bool, error) {
	switch commandName {
	case "list-code-imports":
		response.Edges = codeIntelEdgesFromImportEdges(root, idx.ImportEdges)
	case "list-code-import-summary":
		response.Imports = codeIntelImportsFromSummaries(summarizeCodeImports(idx))
	case "list-code-import-file-summary":
		response.Files = codeIntelFilesFromImportFileSummaries(summarizeCodeImportFiles(root, idx))
	case "code-import-path":
		response.Query = codeIntelQuery("import", input.ImportPath)
		response.Edges = codeIntelEdgesFromImportEdges(root, codeImportEdgesForPath(idx, input.ImportPath))
	case "code-import-path-summary":
		response.Query = codeIntelQuery("import", input.ImportPathSummary)
		response.Imports = codeIntelImportsFromSummaries(summarizeCodeImportPath(idx, input.ImportPathSummary))
	case "code-import-path-file-summary":
		response.Query = codeIntelQuery("import", input.ImportPathFileSummary)
		response.Files = codeIntelFilesFromImportFileSummaries(summarizeCodeImportPathFiles(root, idx, input.ImportPathFileSummary))
	case "code-import-path-package-summary":
		response.Query = codeIntelQuery("import", input.ImportPathPackageSummary)
		response.Packages = codeIntelPackagesFromImportMatches(summarizeCodeImportPathPackages(idx, input.ImportPathPackageSummary))
	case "code-import-prefix":
		response.Query = codeIntelQuery("prefix", input.ImportPrefix)
		response.Edges = codeIntelEdgesFromImportEdges(root, codeImportEdgesWithPrefix(idx, input.ImportPrefix))
	case "code-import-prefix-summary":
		response.Query = codeIntelQuery("prefix", input.ImportPrefixSummary)
		response.Imports = codeIntelImportsFromSummaries(summarizeCodeImportPrefix(idx, input.ImportPrefixSummary))
	case "code-import-prefix-file-summary":
		response.Query = codeIntelQuery("prefix", input.ImportPrefixFileSummary)
		response.Files = codeIntelFilesFromImportFileSummaries(summarizeCodeImportPrefixFiles(root, idx, input.ImportPrefixFileSummary))
	case "code-import-prefix-package-summary":
		response.Query = codeIntelQuery("prefix", input.ImportPrefixPackageSummary)
		response.Packages = codeIntelPackagesFromImportMatches(summarizeCodeImportPrefixPackages(idx, input.ImportPrefixPackageSummary))
	default:
		return response, false, nil
	}

	return response, true, nil
}

func buildCodeIntelPackageImportPathResponse(root string, idx codeintel.Index, input codeIntelCommandInput, response codeIntelResponse, commandName string) (codeIntelResponse, bool, error) {
	switch commandName {
	case "code-package-import-path":
		packageName, importPath, err := parseCodeIntelPairSpec(input.PackageImportPath, "code package import path", "package:import", "package", "import")
		if err != nil {
			return response, true, err
		}
		response.Query = codeIntelQueryPairs("package", packageName, "import", importPath)
		response.Imports = codeIntelImportsFromSummaries(summarizeCodePackageImportPath(idx, packageName, importPath))
	case "code-package-import-files":
		packageName, importPath, err := parseCodeIntelPairSpec(input.PackageImportFiles, "code package import files", "package:import", "package", "import")
		if err != nil {
			return response, true, err
		}
		response.Query = codeIntelQueryPairs("package", packageName, "import", importPath)
		response.Edges = codeIntelEdgesFromFiles(codePackageImportFiles(root, idx, packageName, importPath), importPath)
	case "code-package-import-path-file-summary":
		packageName, importPath, err := parseCodeIntelPairSpec(input.PackageImportPathFileSummary, "code package import path file summary", "package:import", "package", "import")
		if err != nil {
			return response, true, err
		}
		response.Query = codeIntelQueryPairs("package", packageName, "import", importPath)
		response.Files = codeIntelFilesFromImportFileSummaries(summarizeCodePackageImportPathFiles(root, idx, packageName, importPath))
	default:
		return response, false, nil
	}

	return response, true, nil
}

func buildCodeIntelPackageImportPrefixResponse(root string, idx codeintel.Index, input codeIntelCommandInput, response codeIntelResponse, commandName string) (codeIntelResponse, bool, error) {
	switch commandName {
	case "code-package-import-prefix":
		packageName, prefix, err := parseCodeIntelPairSpec(input.PackageImportPrefix, "code package import prefix", "package:prefix", "package", "prefix")
		if err != nil {
			return response, true, err
		}
		response.Query = codeIntelQueryPairs("package", packageName, "prefix", prefix)
		response.Imports = codeIntelImportsFromSummaries(summarizeCodePackageImportPrefix(idx, packageName, prefix))
	case "code-package-import-prefix-files":
		packageName, prefix, err := parseCodeIntelPairSpec(input.PackageImportPrefixFiles, "code package import prefix files", "package:prefix", "package", "prefix")
		if err != nil {
			return response, true, err
		}
		response.Query = codeIntelQueryPairs("package", packageName, "prefix", prefix)
		response.Edges = codeIntelEdgesFromImportEdges(root, codePackageImportPrefixFiles(idx, packageName, prefix))
	case "code-package-import-prefix-file-summary":
		packageName, prefix, err := parseCodeIntelPairSpec(input.PackageImportPrefixFileSummary, "code package import prefix file summary", "package:prefix", "package", "prefix")
		if err != nil {
			return response, true, err
		}
		response.Query = codeIntelQueryPairs("package", packageName, "prefix", prefix)
		response.Files = codeIntelFilesFromImportFileSummaries(summarizeCodePackageImportPrefixFiles(root, idx, packageName, prefix))
	default:
		return response, false, nil
	}

	return response, true, nil
}

func buildCodeIntelPackageImportSummaryResponse(root string, idx codeintel.Index, input codeIntelCommandInput, response codeIntelResponse, commandName string) (codeIntelResponse, bool, error) {
	switch commandName {
	case "list-code-package-import-summary":
		response.Packages = codeIntelPackagesFromImportSummaries(summarizeCodePackageImportCounts(idx))
	case "code-package-imports":
		response.Query = codeIntelQuery("package", input.PackageImports)
		response.Imports = codeIntelImportsFromSummaries(summarizeCodePackageImports(idx, input.PackageImports))
	case "code-package-import-file-summary":
		response.Query = codeIntelQuery("package", input.PackageImportFileSummary)
		response.Files = codeIntelFilesFromImportFileSummaries(summarizeCodePackageImportFiles(root, idx, input.PackageImportFileSummary))
	default:
		return response, false, nil
	}

	return response, true, nil
}
