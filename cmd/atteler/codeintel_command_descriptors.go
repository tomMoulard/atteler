package main

import "strings"

// codeIntelCommandDescriptor is the single source for Go code-intel query
// selectors. It feeds command matching, compatibility flags, and grouped help so
// new queries do not need separate registry and documentation edits.
const (
	codeIntelDomainName         = "code-intel"
	codeIntelSummaryCommandName = "code-summary"
	codeIntelCyclesCommandName  = "list-code-cycles"
	codeIntelLSPSymbolsName     = "lsp-symbols"
	codeIntelLSPWorkspaceName   = "lsp-workspace" //nolint:gosec // CLI command name, not a credential.
)

type codeIntelCommandDescriptor struct {
	Match         func(codeIntelCommandInput) bool
	Name          string
	DomainCommand string
	Args          string
	LegacyFlag    string
	Summary       string
	TextKind      codeIntelTextKind
}

type codeIntelQueryDoc struct {
	Name          string   `json:"name"`
	DomainCommand string   `json:"domain_command"`
	Args          string   `json:"args,omitempty"`
	LegacyFlag    string   `json:"legacy_flag"`
	Summary       string   `json:"summary"`
	Examples      []string `json:"examples"`
	TextOutput    string   `json:"text_output"`
	JSONSchema    string   `json:"json_schema"`
	JSONFields    []string `json:"json_fields"`
}

func codeIntelCommandDescriptors() []codeIntelCommandDescriptor {
	return []codeIntelCommandDescriptor{
		// Symbols.
		codeIntelDescriptor("code-symbol-name", "symbol", "<name>", "--code-symbol", "find Go symbols by exact name", codeIntelTextSymbols, func(input codeIntelCommandInput) bool { return input.SymbolName != "" }),
		codeIntelDescriptor("code-symbol-file-summary", "symbol-name-file-summary", "<name>", "--code-symbol-name-file-summary", "list files with symbol counts for one exact name", codeIntelTextSymbolFileSummary, func(input codeIntelCommandInput) bool { return input.SymbolFileSummary != "" }),
		codeIntelDescriptor("code-symbol-package-summary", "symbol-name-package-summary", "<name>", "--code-symbol-name-package-summary", "list packages with symbol counts for one exact name", codeIntelTextPackages, func(input codeIntelCommandInput) bool { return input.SymbolPackageSummary != "" }),
		codeIntelDescriptor("code-symbol-prefix", "symbol-prefix", "<prefix>", "--code-symbol-prefix", "find Go symbols by name prefix", codeIntelTextSymbols, func(input codeIntelCommandInput) bool { return input.SymbolPrefix != "" }),
		codeIntelDescriptor("code-symbol-prefix-file-summary", "symbol-prefix-file-summary", "<prefix>", "--code-symbol-prefix-file-summary", "list files with symbol counts for names matching a prefix", codeIntelTextSymbolFileSummary, func(input codeIntelCommandInput) bool { return input.SymbolPrefixFileSummary != "" }),
		codeIntelDescriptor("code-symbol-prefix-package-summary", "symbol-prefix-package-summary", "<prefix>", "--code-symbol-prefix-package-summary", "list packages with symbol counts for names matching a prefix", codeIntelTextPackages, func(input codeIntelCommandInput) bool { return input.SymbolPrefixPackageSummary != "" }),
		codeIntelDescriptor("code-symbol-kind", "symbol-kind", "<kind>", "--code-symbol-kind", "list Go symbols by kind", codeIntelTextSymbols, func(input codeIntelCommandInput) bool { return input.SymbolKind != "" }),
		codeIntelDescriptor("code-symbol-kind-file-summary", "symbol-kind-file-summary", "<kind>", "--code-symbol-kind-file-summary", "list files with symbol counts for one kind", codeIntelTextSymbolFileSummary, func(input codeIntelCommandInput) bool { return input.SymbolKindFileSummary != "" }),
		codeIntelDescriptor("code-symbol-kind-package-summary", "symbol-kind-package-summary", "<kind>", "--code-symbol-kind-package-summary", "list packages with symbol counts for one kind", codeIntelTextPackages, func(input codeIntelCommandInput) bool { return input.SymbolKindPackageSummary != "" }),
		codeIntelDescriptor("list-code-symbol-summary", "symbol-summary", "", "--code-symbol-summary", "list symbol kind counts", codeIntelTextSymbolSummary, func(input codeIntelCommandInput) bool { return input.ListSymbolSummary }),
		codeIntelDescriptor("list-code-symbol-file-summary", "symbol-file-summary", "", "--code-symbol-file-summary", "list files with total symbol counts", codeIntelTextSymbolFileSummary, func(input codeIntelCommandInput) bool { return input.ListSymbolFileSummary }),

		// Imports.
		codeIntelDescriptor("list-code-imports", "imports", "", "--code-imports", "list Go import edges", codeIntelTextEdges, func(input codeIntelCommandInput) bool { return input.ListImports }),
		codeIntelDescriptor("list-code-import-summary", "import-summary", "", "--code-import-summary", "list import paths with usage counts", codeIntelTextImportSummary, func(input codeIntelCommandInput) bool { return input.ListImportSummary }),
		codeIntelDescriptor("list-code-import-file-summary", "import-file-summary", "", "--code-import-file-summary", "list files with import counts", codeIntelTextImportFileSummary, func(input codeIntelCommandInput) bool { return input.ListImportFileSummary }),
		codeIntelDescriptor("code-import-path", "import-path", "<path>", "--code-import-path", "list files importing one exact path", codeIntelTextEdges, func(input codeIntelCommandInput) bool { return input.ImportPath != "" }),
		codeIntelDescriptor("code-import-path-summary", "import-path-summary", "<path>", "--code-import-path-summary", "summarize usage for one exact import path", codeIntelTextImportSummary, func(input codeIntelCommandInput) bool { return input.ImportPathSummary != "" }),
		codeIntelDescriptor("code-import-path-file-summary", "import-path-file-summary", "<path>", "--code-import-path-file-summary", "list files with import counts for one exact path", codeIntelTextImportFileSummary, func(input codeIntelCommandInput) bool { return input.ImportPathFileSummary != "" }),
		codeIntelDescriptor("code-import-path-package-summary", "import-path-package-summary", "<path>", "--code-import-path-package-summary", "list packages importing one exact path", codeIntelTextPackageImportMatchSummary, func(input codeIntelCommandInput) bool { return input.ImportPathPackageSummary != "" }),
		codeIntelDescriptor("code-import-prefix", "import-prefix", "<prefix>", "--code-import-prefix", "list files importing paths with one prefix", codeIntelTextEdges, func(input codeIntelCommandInput) bool { return input.ImportPrefix != "" }),
		codeIntelDescriptor("code-import-prefix-summary", "import-prefix-summary", "<prefix>", "--code-import-prefix-summary", "summarize usage for import paths matching a prefix", codeIntelTextImportSummary, func(input codeIntelCommandInput) bool { return input.ImportPrefixSummary != "" }),
		codeIntelDescriptor("code-import-prefix-file-summary", "import-prefix-file-summary", "<prefix>", "--code-import-prefix-file-summary", "list files with import counts for paths matching a prefix", codeIntelTextImportFileSummary, func(input codeIntelCommandInput) bool { return input.ImportPrefixFileSummary != "" }),
		codeIntelDescriptor("code-import-prefix-package-summary", "import-prefix-package-summary", "<prefix>", "--code-import-prefix-package-summary", "list packages importing paths with one prefix", codeIntelTextPackageImportMatchSummary, func(input codeIntelCommandInput) bool { return input.ImportPrefixPackageSummary != "" }),

		// Packages.
		codeIntelDescriptor("list-code-packages", "packages", "", "--code-packages", "list Go packages with file and symbol counts", codeIntelTextPackages, func(input codeIntelCommandInput) bool { return input.ListPackages }),
		codeIntelDescriptor("code-package-name", "package", "<package>", "--code-package", "list files and symbol counts for one package", codeIntelTextFiles, func(input codeIntelCommandInput) bool { return input.PackageName != "" }),
		codeIntelDescriptor("list-code-package-import-summary", "package-import-summary", "", "--code-package-import-summary", "list packages with import counts", codeIntelTextPackageImportSummary, func(input codeIntelCommandInput) bool { return input.ListPackageImportSummary }),
		codeIntelDescriptor("code-package-imports", "package-imports", "<package>", "--code-package-imports", "list import usage counts for one package", codeIntelTextImportSummary, func(input codeIntelCommandInput) bool { return input.PackageImports != "" }),
		codeIntelDescriptor("code-package-import-path", "package-import-path", "<package:import>", "--code-package-import-path", "list exact import usage for one package", codeIntelTextImportSummary, func(input codeIntelCommandInput) bool { return input.PackageImportPath != "" }),
		codeIntelDescriptor("code-package-import-files", "package-import-files", "<package:import>", "--code-package-import-files", "list files in one package importing an exact path", codeIntelTextEdges, func(input codeIntelCommandInput) bool { return input.PackageImportFiles != "" }),
		codeIntelDescriptor("code-package-import-path-file-summary", "package-import-path-file-summary", "<package:import>", "--code-package-import-path-file-summary", "list files in one package with import counts for an exact path", codeIntelTextImportFileSummary, func(input codeIntelCommandInput) bool { return input.PackageImportPathFileSummary != "" }),
		codeIntelDescriptor("code-package-import-prefix", "package-import-prefix", "<package:prefix>", "--code-package-import-prefix", "list import usage for one package and import prefix", codeIntelTextImportSummary, func(input codeIntelCommandInput) bool { return input.PackageImportPrefix != "" }),
		codeIntelDescriptor("code-package-import-prefix-files", "package-import-prefix-files", "<package:prefix>", "--code-package-import-prefix-files", "list files in one package importing paths with a prefix", codeIntelTextEdges, func(input codeIntelCommandInput) bool { return input.PackageImportPrefixFiles != "" }),
		codeIntelDescriptor("code-package-import-prefix-file-summary", "package-import-prefix-file-summary", "<package:prefix>", "--code-package-import-prefix-file-summary", "list files in one package with import counts for a prefix", codeIntelTextImportFileSummary, func(input codeIntelCommandInput) bool { return input.PackageImportPrefixFileSummary != "" }),
		codeIntelDescriptor("code-package-import-file-summary", "package-import-file-summary", "<package>", "--code-package-import-file-summary", "list files in one package with import counts", codeIntelTextImportFileSummary, func(input codeIntelCommandInput) bool { return input.PackageImportFileSummary != "" }),
		codeIntelDescriptor("code-package-symbols", "package-symbols", "<package>", "--code-package-symbols", "list symbol kind counts for one package", codeIntelTextSymbolSummary, func(input codeIntelCommandInput) bool { return input.PackageSymbols != "" }),
		codeIntelDescriptor("code-package-symbol-file-summary", "package-symbol-file-summary", "<package>", "--code-package-symbol-file-summary", "list files in one package with symbol counts", codeIntelTextSymbolFileSummary, func(input codeIntelCommandInput) bool { return input.PackageSymbolFileSummary != "" }),
		codeIntelDescriptor("code-package-symbol-name", "package-symbol", "<package:name>", "--code-package-symbol", "list symbols for one package and exact name", codeIntelTextSymbols, func(input codeIntelCommandInput) bool { return input.PackageSymbolName != "" }),
		codeIntelDescriptor("code-package-symbol-name-file-summary", "package-symbol-name-file-summary", "<package:name>", "--code-package-symbol-name-file-summary", "list files in one package with symbol counts for an exact name", codeIntelTextSymbolFileSummary, func(input codeIntelCommandInput) bool { return input.PackageSymbolNameFileSummary != "" }),
		codeIntelDescriptor("code-package-symbol-list", "package-symbol-list", "<package>", "--code-package-symbol-list", "list symbols declared in one package", codeIntelTextSymbols, func(input codeIntelCommandInput) bool { return input.PackageSymbolList != "" }),
		codeIntelDescriptor("code-package-symbol-kind", "package-symbol-kind", "<package:kind>", "--code-package-symbol-kind", "list symbols for one package and kind", codeIntelTextSymbols, func(input codeIntelCommandInput) bool { return input.PackageSymbolKind != "" }),
		codeIntelDescriptor("code-package-symbol-kind-file-summary", "package-symbol-kind-file-summary", "<package:kind>", "--code-package-symbol-kind-file-summary", "list files in one package with symbol counts for one kind", codeIntelTextSymbolFileSummary, func(input codeIntelCommandInput) bool { return input.PackageSymbolKindFileSummary != "" }),
		codeIntelDescriptor("code-package-symbol-prefix", "package-symbol-prefix", "<package:prefix>", "--code-package-symbol-prefix", "list symbols for one package and name prefix", codeIntelTextSymbols, func(input codeIntelCommandInput) bool { return input.PackageSymbolPrefix != "" }),
		codeIntelDescriptor("code-package-symbol-prefix-file-summary", "package-symbol-prefix-file-summary", "<package:prefix>", "--code-package-symbol-prefix-file-summary", "list files in one package with symbol counts for a prefix", codeIntelTextSymbolFileSummary, func(input codeIntelCommandInput) bool { return input.PackageSymbolPrefixFileSummary != "" }),

		// Files.
		codeIntelDescriptor("code-file-path", "file", "<path>", "--code-file", "print package, symbols, and imports for one file", codeIntelTextFileDetail, func(input codeIntelCommandInput) bool { return input.FilePath != "" }),
		codeIntelDescriptor("code-file-imports", "file-imports", "<path>", "--code-file-imports", "list imports for one Go file", codeIntelTextImports, func(input codeIntelCommandInput) bool { return input.FileImports != "" }),
		codeIntelDescriptor("code-file-symbols", "file-symbols", "<path>", "--code-file-symbols", "list symbols for one Go file", codeIntelTextFileSymbols, func(input codeIntelCommandInput) bool { return input.FileSymbols != "" }),
		codeIntelDescriptor("code-file-symbol-summary", "file-symbol-summary", "<path>", "--code-file-symbol-summary", "list symbol kind counts for one Go file", codeIntelTextSymbolSummary, func(input codeIntelCommandInput) bool { return input.FileSymbolSummary != "" }),
		codeIntelDescriptor("code-file-symbol-name", "file-symbol", "<path:name>", "--code-file-symbol", "list symbols for one file and exact name", codeIntelTextFileSymbols, func(input codeIntelCommandInput) bool { return input.FileSymbolName != "" }),
		codeIntelDescriptor("code-file-symbol-kind", "file-symbol-kind", "<path:kind>", "--code-file-symbol-kind", "list symbols for one file and kind", codeIntelTextFileSymbols, func(input codeIntelCommandInput) bool { return input.FileSymbolKind != "" }),
		codeIntelDescriptor("code-file-symbol-prefix", "file-symbol-prefix", "<path:prefix>", "--code-file-symbol-prefix", "list symbols for one file and name prefix", codeIntelTextFileSymbols, func(input codeIntelCommandInput) bool { return input.FileSymbolPrefix != "" }),
		codeIntelDescriptor("code-file-import-prefix", "file-import-prefix", "<path:prefix>", "--code-file-import-prefix", "list imports for one file matching a prefix", codeIntelTextImports, func(input codeIntelCommandInput) bool { return input.FileImportPrefix != "" }),
		codeIntelDescriptor("code-file-import-path", "file-import-path", "<path:import>", "--code-file-import-path", "check one file import path", codeIntelTextImports, func(input codeIntelCommandInput) bool { return input.FileImportPath != "" }),

		// Graph / repository structure.
		codeIntelDescriptor("list-code-layers", "layers", "", "--code-layers", "list topological Go import graph layers", codeIntelTextLayers, func(input codeIntelCommandInput) bool { return input.ListLayers }),
		codeIntelDescriptor(codeIntelCyclesCommandName, "cycles", "", "--code-cycles", "list Go import graph cycles", codeIntelTextCycles, func(input codeIntelCommandInput) bool { return input.ListCycles }),
		codeIntelDescriptor(codeIntelSummaryCommandName, "summary", "", "--code-summary", "print compact Go code index counts", codeIntelTextSummary, func(input codeIntelCommandInput) bool { return input.Summary }),
		codeIntelDescriptor("list-code-files", "files", "", "--code-files", "list Go files with package/import/symbol counts", codeIntelTextFiles, func(input codeIntelCommandInput) bool { return input.ListFiles }),
		codeIntelDescriptor("code-impact-target", "impact", "<path>", "--code-impact", "list files impacted by an import path", codeIntelTextImpactSet, func(input codeIntelCommandInput) bool { return input.ImpactTarget != "" }),
		codeIntelDescriptor("code-reach-target", "reachable", "<path>", "--code-reachable", "list reachable import graph nodes", codeIntelTextGraphNodes, func(input codeIntelCommandInput) bool { return input.ReachTarget != "" }),
		codeIntelDescriptor("code-deps-target", "deps", "<path>", "--code-deps", "list direct import graph dependencies", codeIntelTextGraphNodes, func(input codeIntelCommandInput) bool { return input.DepsTarget != "" }),
		codeIntelDescriptor("code-rdeps-target", "rdeps", "<path>", "--code-rdeps", "list direct reverse dependencies", codeIntelTextGraphNodes, func(input codeIntelCommandInput) bool { return input.RDepsTarget != "" }),
	}
}

func codeIntelDescriptor(
	name string,
	domainCommand string,
	args string,
	legacyFlag string,
	summary string,
	textKind codeIntelTextKind,
	match func(codeIntelCommandInput) bool,
) codeIntelCommandDescriptor {
	return codeIntelCommandDescriptor{
		Match:         match,
		Name:          name,
		DomainCommand: domainCommand,
		Args:          args,
		LegacyFlag:    legacyFlag,
		Summary:       summary,
		TextKind:      textKind,
	}
}

func codeIntelDomainCommandAliases() []cliCommandAlias {
	descriptors := codeIntelCommandDescriptors()

	commands := make([]cliCommandAlias, 0, len(descriptors)+2)
	for _, descriptor := range descriptors {
		commands = append(commands, descriptor.domainAlias())
	}

	commands = append(commands, codeIntelLSPDomainCommandAliases()...)

	return commands
}

func focusedCodeIntelDomainCommandAliases() []cliCommandAlias {
	descriptors := codeIntelCommandDescriptorsByName()

	commands := make([]cliCommandAlias, 0, len(focusedCodeIntelCommandDescriptorNames())+2)
	for _, name := range focusedCodeIntelCommandDescriptorNames() {
		commands = append(commands, descriptors[name].domainAlias())
	}

	commands = append(commands, codeIntelLSPDomainCommandAliases()...)

	return commands
}

func codeIntelLSPDomainCommandAliases() []cliCommandAlias {
	return []cliCommandAlias{
		{
			Name:       codeIntelLSPSymbolsName,
			Summary:    "request document symbols from an external LSP",
			TextOutput: codeIntelTextOutputForKind(codeIntelTextLSPSymbols),
			JSONSchema: codeIntelSchemaVersion,
			Legacy:     []string{"--lsp-symbols"},
			Examples:   codeIntelLSPQueryExamples(codeIntelLSPSymbolsName),
			JSONFields: codeIntelJSONFieldsForKind(codeIntelTextLSPSymbols),
		},
		{
			Name:       codeIntelLSPWorkspaceName,
			Args:       "<query>",
			Summary:    "request workspace symbols from an external LSP",
			TextOutput: codeIntelTextOutputForKind(codeIntelTextLSPSymbols),
			JSONSchema: codeIntelSchemaVersion,
			Legacy:     []string{"--lsp-workspace-symbols"},
			Examples:   codeIntelLSPQueryExamples(codeIntelLSPWorkspaceName),
			JSONFields: codeIntelJSONFieldsForKind(codeIntelTextLSPSymbols),
		},
	}
}

func codeIntelCommandDescriptorsByName() map[string]codeIntelCommandDescriptor {
	descriptors := codeIntelCommandDescriptors()

	byName := make(map[string]codeIntelCommandDescriptor, len(descriptors))
	for _, descriptor := range descriptors {
		byName[descriptor.Name] = descriptor
	}

	return byName
}

func focusedCodeIntelCommandDescriptorNames() []string {
	return []string{
		codeIntelSummaryCommandName,
		"list-code-files",
		"list-code-packages",
		"code-package-name",
		"code-package-imports",
		"code-package-symbols",
		"code-file-path",
		"code-file-imports",
		"code-file-symbols",
		"code-symbol-name",
		"code-symbol-prefix",
		"code-symbol-kind",
		"list-code-imports",
		"list-code-import-summary",
		"code-import-path",
		"code-import-prefix",
		"list-code-layers",
		codeIntelCyclesCommandName,
		"code-impact-target",
		"code-reach-target",
		"code-deps-target",
		"code-rdeps-target",
	}
}

func (descriptor codeIntelCommandDescriptor) domainAlias() cliCommandAlias {
	return cliCommandAlias{
		Name:       descriptor.DomainCommand,
		Args:       descriptor.Args,
		Summary:    descriptor.Summary,
		TextOutput: codeIntelTextOutputForKind(descriptor.TextKind),
		JSONSchema: codeIntelSchemaVersion,
		Legacy:     []string{descriptor.LegacyFlag},
		Examples:   descriptor.examples(),
		JSONFields: codeIntelJSONFieldsForKind(descriptor.TextKind),
	}
}

func codeIntelDomainExamples() []string {
	descriptors := codeIntelCommandDescriptorsByName()
	summary := descriptors[codeIntelSummaryCommandName]

	return []string{
		codeIntelCommandExample(summary),
		codeIntelCommandExample(summary, "--json"),
		codeIntelCommandExample(descriptors["code-symbol-name"], "NewRegistry"),
		codeIntelCommandExample(descriptors["code-import-prefix"], "github.com/tommoulard/atteler/pkg/"),
	}
}

func codeIntelCommandExample(descriptor codeIntelCommandDescriptor, args ...string) string {
	parts := []string{"atteler", codeIntelDomainName, descriptor.DomainCommand}

	for _, arg := range args {
		if arg != "" {
			parts = append(parts, arg)
		}
	}

	return strings.Join(parts, " ")
}

func codeIntelQueryDocs() []codeIntelQueryDoc {
	descriptors := codeIntelCommandDescriptors()

	docs := make([]codeIntelQueryDoc, 0, len(descriptors))
	for _, descriptor := range descriptors {
		docs = append(docs, descriptor.queryDoc())
	}

	return docs
}

func codeIntelLSPQueryDocs() []codeIntelQueryDoc {
	aliases := codeIntelLSPDomainCommandAliases()

	docs := make([]codeIntelQueryDoc, 0, len(aliases))
	for i := range aliases {
		alias := aliases[i]

		legacyFlag := ""
		if len(alias.Legacy) > 0 {
			legacyFlag = alias.Legacy[0]
		}

		docs = append(docs, codeIntelQueryDoc{
			Name:          alias.Name,
			DomainCommand: alias.Name,
			Args:          alias.Args,
			LegacyFlag:    legacyFlag,
			Summary:       alias.Summary,
			Examples:      codeIntelLSPQueryExamples(alias.Name),
			TextOutput:    alias.TextOutput,
			JSONSchema:    alias.JSONSchema,
			JSONFields:    append([]string(nil), alias.JSONFields...),
		})
	}

	return docs
}

func (descriptor codeIntelCommandDescriptor) queryDoc() codeIntelQueryDoc {
	return codeIntelQueryDoc{
		Name:          descriptor.Name,
		DomainCommand: descriptor.DomainCommand,
		Args:          descriptor.Args,
		LegacyFlag:    descriptor.LegacyFlag,
		Summary:       descriptor.Summary,
		Examples:      descriptor.examples(),
		TextOutput:    codeIntelTextOutputForKind(descriptor.TextKind),
		JSONSchema:    codeIntelSchemaVersion,
		JSONFields:    codeIntelJSONFieldsForKind(descriptor.TextKind),
	}
}

func (descriptor codeIntelCommandDescriptor) examples() []string {
	return []string{codeIntelCommandExample(descriptor, codeIntelExampleArgsForDescriptor(descriptor)...)}
}

func codeIntelExampleArgsForDescriptor(descriptor codeIntelCommandDescriptor) []string {
	shape := descriptor.Args
	if strings.Contains(descriptor.DomainCommand, "import-prefix") {
		shape = "import-prefix:" + shape
	}

	if strings.Contains(descriptor.DomainCommand, "import-path") || descriptor.DomainCommand == "impact" {
		shape = "import-path:" + shape
	}

	if descriptor.DomainCommand == "rdeps" {
		shape = "import-path:" + shape
	}

	if strings.Contains(descriptor.DomainCommand, "package-symbol-prefix") {
		shape = "symbol-prefix:" + shape
	}

	if args, ok := codeIntelExampleArgsByShape[shape]; ok {
		return append([]string(nil), args...)
	}

	return []string{descriptor.Args}
}

func codeIntelLSPQueryExamples(name string) []string {
	switch name {
	case codeIntelLSPSymbolsName:
		return []string{"atteler code-intel lsp-symbols --lsp-file main.go"}
	case codeIntelLSPWorkspaceName:
		return []string{"atteler code-intel lsp-workspace Handler --json"}
	default:
		return nil
	}
}

var codeIntelExampleArgsByShape = map[string][]string{
	"":                               nil,
	"<name>":                         {"Run"},
	"<prefix>":                       {"Ru"},
	"<kind>":                         {"func"},
	"<path>":                         {"runner.go"},
	"<package>":                      {"main"},
	"<package:import>":               {"main:context"},
	"<package:prefix>":               {"main:con"},
	"<package:name>":                 {"main:Run"},
	"<package:kind>":                 {"main:func"},
	"<path:name>":                    {"runner.go:Run"},
	"<path:kind>":                    {"runner.go:func"},
	"<path:prefix>":                  {"runner.go:Ru"},
	"<path:import>":                  {"runner.go:context"},
	"import-prefix:<prefix>":         {"con"},
	"import-prefix:<package:prefix>": {"main:con"},
	"import-prefix:<path:prefix>":    {"runner.go:con"},
	"import-path:<path>":             {"context"},
	"import-path:<package:import>":   {"main:context"},
	"import-path:<path:import>":      {"runner.go:context"},
	"symbol-prefix:<package:prefix>": {"main:Ru"},
}

func codeIntelTextKindForCommand(commandName string) codeIntelTextKind {
	for _, descriptor := range codeIntelCommandDescriptors() {
		if descriptor.Name == commandName {
			return descriptor.TextKind
		}
	}

	return ""
}

var codeIntelTextOutputsByKind = map[codeIntelTextKind]string{
	codeIntelTextSummary:                   `files=<n>\tpackages=<n>\tsymbols=<n>\timports=<n>\tnodes=<n>\tedges=<n>\tcycles=<n>\tlayers=<n>`,
	codeIntelTextFiles:                     `path=<path>\tpackage=<package>\tsymbols=<n>\timports=<n>`,
	codeIntelTextFileDetail:                `path=<path>\tpackage=<package>\timports=<n>\tsymbols=<n>, then imports/symbols sections`,
	codeIntelTextSymbols:                   `<name>\tkind=<kind>\tpath=<path>\tline=<n>`,
	codeIntelTextFileSymbols:               `<name>\tkind=<kind>\tline=<n>`,
	codeIntelTextSymbolSummary:             `kind=<kind>\tsymbols=<n>`,
	codeIntelTextSymbolFileSummary:         `path=<path>\tpackage=<package>\tsymbols=<n>`,
	codeIntelTextPackages:                  `package=<package>\tfiles=<n>\tsymbols=<n>`,
	codeIntelTextImports:                   `import=<path>`,
	codeIntelTextImportSummary:             `import=<path>\tfiles=<n>`,
	codeIntelTextImportFileSummary:         `path=<path>\tpackage=<package>\timports=<n>`,
	codeIntelTextPackageImportSummary:      `package=<package>\tfiles=<n>\timports=<n>\tunique_imports=<n>`,
	codeIntelTextPackageImportMatchSummary: `package=<package>\tfiles=<n>[\timports=<n>]`,
	codeIntelTextEdges:                     `path=<path>\timport=<path>`,
	codeIntelTextImpactSet:                 `path=<path>`,
	codeIntelTextGraphNodes:                `node=<path>`,
	codeIntelTextCycles:                    `cycle=<n>\tnodes=<path> -> <path>`,
	codeIntelTextLayers:                    `layer=<n>\tnodes=<path>,<path>`,
	codeIntelTextLSPSymbols:                `<name>\tkind=<n>\trange=<start-end>[\tdetail=<detail>][\tcontainer=<name>][\turi=<uri>]`,
}

func codeIntelTextOutputForKind(kind codeIntelTextKind) string {
	return codeIntelTextOutputsByKind[kind]
}

func codeIntelJSONFieldsForKind(kind codeIntelTextKind) []string {
	fields := []string{"schema", "command", "query", "empty", "message"}
	if codeIntelTextKindSupportsPagination(kind) {
		fields = append(fields, "pagination")
	}

	if payloadField, ok := codeIntelPayloadFieldForKind(kind); ok {
		return append(fields, payloadField)
	}

	return fields
}

func codeIntelTextKindSupportsPagination(kind codeIntelTextKind) bool {
	switch kind {
	case codeIntelTextSummary, codeIntelTextFileDetail, codeIntelTextLSPSymbols, "":
		return false
	default:
		return true
	}
}
