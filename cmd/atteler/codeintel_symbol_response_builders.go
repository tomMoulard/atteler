//nolint:wsl_v5 // Category builders keep related schema-field assignments compact and auditable.
package main

import "github.com/tommoulard/atteler/pkg/codeintel"

func buildCodeIntelSymbolResponse(root string, idx codeintel.Index, input codeIntelCommandInput, response codeIntelResponse, commandName string) (codeIntelResponse, bool, error) {
	switch commandName {
	case "code-symbol-name":
		response.Query = codeIntelQuery("symbol", input.SymbolName)
		response.Symbols = codeIntelSymbolsFromSymbols(root, idx.FindSymbol(input.SymbolName))
	case "code-symbol-file-summary":
		response.Query = codeIntelQuery("symbol", input.SymbolFileSummary)
		response.Files = codeIntelFilesFromSymbolFileSummaries(summarizeCodeSymbolNameFiles(root, idx, input.SymbolFileSummary))
	case "code-symbol-package-summary":
		response.Query = codeIntelQuery("symbol", input.SymbolPackageSummary)
		response.Packages = codeIntelPackagesFromSummaries(summarizeCodeSymbolNamePackages(idx, input.SymbolPackageSummary))
	case "code-symbol-prefix":
		response.Query = codeIntelQuery("prefix", input.SymbolPrefix)
		response.Symbols = codeIntelSymbolsFromSymbols(root, codeSymbolsWithPrefix(idx, input.SymbolPrefix))
	case "code-symbol-prefix-file-summary":
		response.Query = codeIntelQuery("prefix", input.SymbolPrefixFileSummary)
		response.Files = codeIntelFilesFromSymbolFileSummaries(summarizeCodeSymbolPrefixFiles(root, idx, input.SymbolPrefixFileSummary))
	case "code-symbol-prefix-package-summary":
		response.Query = codeIntelQuery("prefix", input.SymbolPrefixPackageSummary)
		response.Packages = codeIntelPackagesFromSummaries(summarizeCodeSymbolPrefixPackages(idx, input.SymbolPrefixPackageSummary))
	case "code-symbol-kind":
		response.Query = codeIntelQuery("kind", input.SymbolKind)
		response.Symbols = codeIntelSymbolsFromSymbols(root, codeSymbolsByKind(idx, input.SymbolKind))
	case "code-symbol-kind-file-summary":
		response.Query = codeIntelQuery("kind", input.SymbolKindFileSummary)
		response.Files = codeIntelFilesFromSymbolFileSummaries(summarizeCodeSymbolKindFiles(root, idx, input.SymbolKindFileSummary))
	case "code-symbol-kind-package-summary":
		response.Query = codeIntelQuery("kind", input.SymbolKindPackageSummary)
		response.Packages = codeIntelPackagesFromSummaries(summarizeCodeSymbolKindPackages(idx, input.SymbolKindPackageSummary))
	case "list-code-symbol-summary":
		response.Symbols = codeIntelSymbolsFromSummaries(summarizeCodeSymbols(idx))
	case "list-code-symbol-file-summary":
		response.Files = codeIntelFilesFromSymbolFileSummaries(summarizeCodeSymbolFiles(root, idx))
	default:
		return response, false, nil
	}

	return response, true, nil
}

func buildCodeIntelPackageSymbolSummaryResponse(root string, idx codeintel.Index, input codeIntelCommandInput, response codeIntelResponse, commandName string) (codeIntelResponse, bool, error) {
	switch commandName {
	case "code-package-symbols":
		response.Query = codeIntelQuery("package", input.PackageSymbols)
		response.Symbols = codeIntelSymbolsFromSummaries(summarizeCodePackageSymbols(idx, input.PackageSymbols))
	case "code-package-symbol-file-summary":
		response.Query = codeIntelQuery("package", input.PackageSymbolFileSummary)
		response.Files = codeIntelFilesFromSymbolFileSummaries(summarizeCodePackageSymbolFiles(root, idx, input.PackageSymbolFileSummary))
	case "code-package-symbol-list":
		response.Query = codeIntelQuery("package", input.PackageSymbolList)
		response.Symbols = codeIntelSymbolsFromSymbols(root, codePackageSymbols(idx, input.PackageSymbolList))
	default:
		return response, false, nil
	}

	return response, true, nil
}

func buildCodeIntelPackageSymbolFilterResponse(root string, idx codeintel.Index, input codeIntelCommandInput, response codeIntelResponse, commandName string) (codeIntelResponse, bool, error) {
	switch commandName {
	case "code-package-symbol-name":
		packageName, name, err := parseCodeIntelPairSpec(input.PackageSymbolName, "code package symbol", "package:name", "package", "symbol")
		if err != nil {
			return response, true, err
		}
		response.Query = codeIntelQueryPairs("package", packageName, "symbol", name)
		response.Symbols = codeIntelSymbolsFromSymbols(root, codePackageSymbolsByName(idx, packageName, name))
	case "code-package-symbol-name-file-summary":
		packageName, name, err := parseCodeIntelPairSpec(input.PackageSymbolNameFileSummary, "code package symbol name file summary", "package:name", "package", "symbol")
		if err != nil {
			return response, true, err
		}
		response.Query = codeIntelQueryPairs("package", packageName, "symbol", name)
		response.Files = codeIntelFilesFromSymbolFileSummaries(summarizeCodePackageSymbolNameFiles(root, idx, packageName, name))
	case "code-package-symbol-kind":
		packageName, kind, err := parseCodePackageSymbolKindSpec(input.PackageSymbolKind)
		if err != nil {
			return response, true, err
		}
		response.Query = codeIntelQueryPairs("package", packageName, "kind", kind)
		response.Symbols = codeIntelSymbolsFromSymbols(root, codePackageSymbolsByKind(idx, packageName, kind))
	case "code-package-symbol-kind-file-summary":
		packageName, kind, err := parseCodePackageSymbolKindSpec(input.PackageSymbolKindFileSummary)
		if err != nil {
			return response, true, err
		}
		response.Query = codeIntelQueryPairs("package", packageName, "kind", kind)
		response.Files = codeIntelFilesFromSymbolFileSummaries(summarizeCodePackageSymbolKindFiles(root, idx, packageName, kind))
	case "code-package-symbol-prefix":
		packageName, prefix, err := parseCodeIntelPairSpec(input.PackageSymbolPrefix, "code package symbol prefix", "package:prefix", "package", "prefix")
		if err != nil {
			return response, true, err
		}
		response.Query = codeIntelQueryPairs("package", packageName, "prefix", prefix)
		response.Symbols = codeIntelSymbolsFromSymbols(root, codePackageSymbolsWithPrefix(idx, packageName, prefix))
	case "code-package-symbol-prefix-file-summary":
		packageName, prefix, err := parseCodeIntelPairSpec(input.PackageSymbolPrefixFileSummary, "code package symbol prefix file summary", "package:prefix", "package", "prefix")
		if err != nil {
			return response, true, err
		}
		response.Query = codeIntelQueryPairs("package", packageName, "prefix", prefix)
		response.Files = codeIntelFilesFromSymbolFileSummaries(summarizeCodePackageSymbolPrefixFiles(root, idx, packageName, prefix))
	default:
		return response, false, nil
	}

	return response, true, nil
}

func buildCodeIntelFileImportResponse(root string, idx codeintel.Index, input codeIntelCommandInput, response codeIntelResponse, commandName string) (codeIntelResponse, bool, error) {
	switch commandName {
	case "code-file-imports":
		response.Query = codeIntelQuery("path", input.FileImports)
		if file, ok := findCodeFile(root, idx, input.FileImports); ok {
			response.Imports = codeIntelImportsFromPaths(codeFileImports(file))
		}
	case "code-file-import-path":
		target, importPath, err := parseCodeIntelPairSpec(input.FileImportPath, "code file import path", "path:import", "path", "import")
		if err != nil {
			return response, true, err
		}
		response.Query = codeIntelQueryPairs("path", target, "import", importPath)
		if file, ok := findCodeFile(root, idx, target); ok {
			response.Imports = codeIntelImportsFromPaths(codeFileImportsForPath(file, importPath))
		}
	case "code-file-import-prefix":
		target, prefix, err := parseCodeIntelPairSpec(input.FileImportPrefix, "code file import prefix", "path:prefix", "path", "prefix")
		if err != nil {
			return response, true, err
		}
		response.Query = codeIntelQueryPairs("path", target, "prefix", prefix)
		if file, ok := findCodeFile(root, idx, target); ok {
			response.Imports = codeIntelImportsFromPaths(codeFileImportsWithPrefix(file, prefix))
		}
	default:
		return response, false, nil
	}

	return response, true, nil
}

func buildCodeIntelFileSymbolResponse(root string, idx codeintel.Index, input codeIntelCommandInput, response codeIntelResponse, commandName string) (codeIntelResponse, bool, error) {
	switch commandName {
	case "code-file-symbol-summary":
		response.Query = codeIntelQuery("path", input.FileSymbolSummary)
		if file, ok := findCodeFile(root, idx, input.FileSymbolSummary); ok {
			response.Symbols = codeIntelSymbolsFromSummaries(summarizeCodeFileSymbols(file))
		}
	case "code-file-symbols":
		response.Query = codeIntelQuery("path", input.FileSymbols)
		if file, ok := findCodeFile(root, idx, input.FileSymbols); ok {
			response.Symbols = codeIntelSymbolsFromSymbols(root, codeFileSymbols(file))
		}
	case "code-file-symbol-name":
		target, name, err := parseCodeIntelPairSpec(input.FileSymbolName, "code file symbol", "path:name", "path", "symbol")
		if err != nil {
			return response, true, err
		}
		response.Query = codeIntelQueryPairs("path", target, "symbol", name)
		if file, ok := findCodeFile(root, idx, target); ok {
			response.Symbols = codeIntelSymbolsFromSymbols(root, codeFileSymbolsByName(file, name))
		}
	case "code-file-symbol-kind":
		target, kind, err := parseCodeFileSymbolKindSpec(input.FileSymbolKind)
		if err != nil {
			return response, true, err
		}
		response.Query = codeIntelQueryPairs("path", target, "kind", kind)
		if file, ok := findCodeFile(root, idx, target); ok {
			response.Symbols = codeIntelSymbolsFromSymbols(root, codeFileSymbolsByKind(file, kind))
		}
	case "code-file-symbol-prefix":
		target, prefix, err := parseCodeIntelPairSpec(input.FileSymbolPrefix, "code file symbol prefix", "path:prefix", "path", "prefix")
		if err != nil {
			return response, true, err
		}
		response.Query = codeIntelQueryPairs("path", target, "prefix", prefix)
		if file, ok := findCodeFile(root, idx, target); ok {
			response.Symbols = codeIntelSymbolsFromSymbols(root, codeFileSymbolsWithPrefix(file, prefix))
		}
	default:
		return response, false, nil
	}

	return response, true, nil
}
