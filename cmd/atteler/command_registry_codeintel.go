package main

import (
	"context"
	"fmt"
	"strings"
)

type codeIntelCommand struct {
	match func(codeIntelCommandInput) bool
	run   func(cwd string, input codeIntelCommandInput) error
	name  string
}

func providerlessConfigCodeIntelCommands() []command {
	return []command{
		{
			name:  "code-intel",
			tier:  tierProviderlessConfig,
			match: codeIntelCommandRequested,
			runProviderlessConfig: func(_ context.Context, o cliOptions, s appState) error {
				return runCodeIntelCommand(s.cwd, codeIntelCommandInputFromOptions(o))
			},
		},
	}
}

func codeIntelCommandRequested(opts cliOptions) bool {
	return matchingCodeIntelCommand(codeIntelCommandInputFromOptions(opts)) != nil
}

func runCodeIntelCommand(cwd string, input codeIntelCommandInput) error {
	cmd, err := selectCodeIntelCommand(input)
	if err != nil {
		return err
	}

	if cmd == nil {
		return nil
	}

	return cmd.run(cwd, input)
}

func matchingCodeIntelCommand(input codeIntelCommandInput) *codeIntelCommand {
	matches := matchingCodeIntelCommands(input)
	if len(matches) == 0 {
		return nil
	}

	return matches[0]
}

func selectCodeIntelCommand(input codeIntelCommandInput) (*codeIntelCommand, error) {
	matches := matchingCodeIntelCommands(input)
	switch len(matches) {
	case 0:
		return nil, nil
	case 1:
		return matches[0], nil
	default:
		return nil, fmt.Errorf("ambiguous CLI command: flags match multiple code-intel commands (%s); choose one command or remove conflicting flags",
			codeIntelCommandNames(matches))
	}
}

func matchingCodeIntelCommands(input codeIntelCommandInput) []*codeIntelCommand {
	matches := make([]*codeIntelCommand, 0, 1)
	commands := codeIntelCommands()

	for i := range commands {
		cmd := &commands[i]
		if cmd.match(input) {
			matches = append(matches, cmd)
		}
	}

	return matches
}

func codeIntelCommandNames(commands []*codeIntelCommand) string {
	names := make([]string, 0, len(commands))
	for _, command := range commands {
		names = append(names, command.name)
	}

	return strings.Join(names, ", ")
}

func codeIntelCommands() []codeIntelCommand {
	return []codeIntelCommand{
		// ---------------------------------------------------------------
		// Tier: providerless config -- code analysis
		// ---------------------------------------------------------------
		codeSymbolCmd("code-symbol-name", func(input codeIntelCommandInput) bool { return input.SymbolName != "" },
			func(cwd string, input codeIntelCommandInput) error { return findCodeSymbol(cwd, input.SymbolName) }),
		codeSymbolCmd("code-symbol-file-summary", func(input codeIntelCommandInput) bool { return input.SymbolFileSummary != "" },
			func(cwd string, input codeIntelCommandInput) error {
				return listCodeSymbolNameFileSummary(cwd, input.SymbolFileSummary)
			}),
		codeSymbolCmd("code-symbol-package-summary", func(input codeIntelCommandInput) bool { return input.SymbolPackageSummary != "" },
			func(cwd string, input codeIntelCommandInput) error {
				return listCodeSymbolNamePackageSummary(cwd, input.SymbolPackageSummary)
			}),
		codeSymbolCmd("code-symbol-prefix", func(input codeIntelCommandInput) bool { return input.SymbolPrefix != "" },
			func(cwd string, input codeIntelCommandInput) error {
				return findCodeSymbolPrefix(cwd, input.SymbolPrefix)
			}),
		codeSymbolCmd("code-symbol-prefix-file-summary", func(input codeIntelCommandInput) bool { return input.SymbolPrefixFileSummary != "" },
			func(cwd string, input codeIntelCommandInput) error {
				return listCodeSymbolPrefixFileSummary(cwd, input.SymbolPrefixFileSummary)
			}),
		codeSymbolCmd("code-symbol-prefix-package-summary", func(input codeIntelCommandInput) bool { return input.SymbolPrefixPackageSummary != "" },
			func(cwd string, input codeIntelCommandInput) error {
				return listCodeSymbolPrefixPackageSummary(cwd, input.SymbolPrefixPackageSummary)
			}),
		codeSymbolCmd("code-symbol-kind", func(input codeIntelCommandInput) bool { return input.SymbolKind != "" },
			func(cwd string, input codeIntelCommandInput) error {
				return findCodeSymbolsByKind(cwd, input.SymbolKind)
			}),
		codeSymbolCmd("code-symbol-kind-file-summary", func(input codeIntelCommandInput) bool { return input.SymbolKindFileSummary != "" },
			func(cwd string, input codeIntelCommandInput) error {
				return listCodeSymbolKindFileSummary(cwd, input.SymbolKindFileSummary)
			}),
		codeSymbolCmd("code-symbol-kind-package-summary", func(input codeIntelCommandInput) bool { return input.SymbolKindPackageSummary != "" },
			func(cwd string, input codeIntelCommandInput) error {
				return listCodeSymbolKindPackageSummary(cwd, input.SymbolKindPackageSummary)
			}),
		codeSymbolCmd("list-code-symbol-summary", func(input codeIntelCommandInput) bool { return input.ListSymbolSummary },
			func(cwd string, _ codeIntelCommandInput) error { return listCodeSymbolSummary(cwd) }),
		codeSymbolCmd("list-code-symbol-file-summary", func(input codeIntelCommandInput) bool { return input.ListSymbolFileSummary },
			func(cwd string, _ codeIntelCommandInput) error { return listCodeSymbolFileSummary(cwd) }),

		// Code imports
		codeSymbolCmd("list-code-imports", func(input codeIntelCommandInput) bool { return input.ListImports },
			func(cwd string, _ codeIntelCommandInput) error { return listCodeImports(cwd) }),
		codeSymbolCmd("list-code-import-summary", func(input codeIntelCommandInput) bool { return input.ListImportSummary },
			func(cwd string, _ codeIntelCommandInput) error { return listCodeImportSummary(cwd) }),
		codeSymbolCmd("list-code-import-file-summary", func(input codeIntelCommandInput) bool { return input.ListImportFileSummary },
			func(cwd string, _ codeIntelCommandInput) error { return listCodeImportFileSummary(cwd) }),
		codeSymbolCmd("code-import-path", func(input codeIntelCommandInput) bool { return input.ImportPath != "" },
			func(cwd string, input codeIntelCommandInput) error { return listCodeImportPath(cwd, input.ImportPath) }),
		codeSymbolCmd("code-import-path-summary", func(input codeIntelCommandInput) bool { return input.ImportPathSummary != "" },
			func(cwd string, input codeIntelCommandInput) error {
				return listCodeImportPathSummary(cwd, input.ImportPathSummary)
			}),
		codeSymbolCmd("code-import-path-file-summary", func(input codeIntelCommandInput) bool { return input.ImportPathFileSummary != "" },
			func(cwd string, input codeIntelCommandInput) error {
				return listCodeImportPathFileSummary(cwd, input.ImportPathFileSummary)
			}),
		codeSymbolCmd("code-import-path-package-summary", func(input codeIntelCommandInput) bool { return input.ImportPathPackageSummary != "" },
			func(cwd string, input codeIntelCommandInput) error {
				return listCodeImportPathPackageSummary(cwd, input.ImportPathPackageSummary)
			}),
		codeSymbolCmd("code-import-prefix", func(input codeIntelCommandInput) bool { return input.ImportPrefix != "" },
			func(cwd string, input codeIntelCommandInput) error {
				return listCodeImportPrefix(cwd, input.ImportPrefix)
			}),
		codeSymbolCmd("code-import-prefix-summary", func(input codeIntelCommandInput) bool { return input.ImportPrefixSummary != "" },
			func(cwd string, input codeIntelCommandInput) error {
				return listCodeImportPrefixSummary(cwd, input.ImportPrefixSummary)
			}),
		codeSymbolCmd("code-import-prefix-file-summary", func(input codeIntelCommandInput) bool { return input.ImportPrefixFileSummary != "" },
			func(cwd string, input codeIntelCommandInput) error {
				return listCodeImportPrefixFileSummary(cwd, input.ImportPrefixFileSummary)
			}),
		codeSymbolCmd("code-import-prefix-package-summary", func(input codeIntelCommandInput) bool { return input.ImportPrefixPackageSummary != "" },
			func(cwd string, input codeIntelCommandInput) error {
				return listCodeImportPrefixPackageSummary(cwd, input.ImportPrefixPackageSummary)
			}),

		// Code packages
		codeSymbolCmd("list-code-packages", func(input codeIntelCommandInput) bool { return input.ListPackages },
			func(cwd string, _ codeIntelCommandInput) error { return listCodePackages(cwd) }),
		codeSymbolCmd("code-package-name", func(input codeIntelCommandInput) bool { return input.PackageName != "" },
			func(cwd string, input codeIntelCommandInput) error {
				return listCodePackageFiles(cwd, input.PackageName)
			}),
		codeSymbolCmd("list-code-package-import-summary", func(input codeIntelCommandInput) bool { return input.ListPackageImportSummary },
			func(cwd string, _ codeIntelCommandInput) error { return listCodePackageImportSummary(cwd) }),
		codeSymbolCmd("code-package-imports", func(input codeIntelCommandInput) bool { return input.PackageImports != "" },
			func(cwd string, input codeIntelCommandInput) error {
				return listCodePackageImports(cwd, input.PackageImports)
			}),
		codeSymbolCmd("code-package-import-path", func(input codeIntelCommandInput) bool { return input.PackageImportPath != "" },
			func(cwd string, input codeIntelCommandInput) error {
				return listCodePackageImportPath(cwd, input.PackageImportPath)
			}),
		codeSymbolCmd("code-package-import-files", func(input codeIntelCommandInput) bool { return input.PackageImportFiles != "" },
			func(cwd string, input codeIntelCommandInput) error {
				return listCodePackageImportFiles(cwd, input.PackageImportFiles)
			}),
		codeSymbolCmd("code-package-import-path-file-summary", func(input codeIntelCommandInput) bool { return input.PackageImportPathFileSummary != "" },
			func(cwd string, input codeIntelCommandInput) error {
				return listCodePackageImportPathFileSummary(cwd, input.PackageImportPathFileSummary)
			}),
		codeSymbolCmd("code-package-import-prefix", func(input codeIntelCommandInput) bool { return input.PackageImportPrefix != "" },
			func(cwd string, input codeIntelCommandInput) error {
				return listCodePackageImportPrefix(cwd, input.PackageImportPrefix)
			}),
		codeSymbolCmd("code-package-import-prefix-files", func(input codeIntelCommandInput) bool { return input.PackageImportPrefixFiles != "" },
			func(cwd string, input codeIntelCommandInput) error {
				return listCodePackageImportPrefixFiles(cwd, input.PackageImportPrefixFiles)
			}),
		codeSymbolCmd("code-package-import-prefix-file-summary", func(input codeIntelCommandInput) bool { return input.PackageImportPrefixFileSummary != "" },
			func(cwd string, input codeIntelCommandInput) error {
				return listCodePackageImportPrefixFileSummary(cwd, input.PackageImportPrefixFileSummary)
			}),
		codeSymbolCmd("code-package-import-file-summary", func(input codeIntelCommandInput) bool { return input.PackageImportFileSummary != "" },
			func(cwd string, input codeIntelCommandInput) error {
				return listCodePackageImportFileSummary(cwd, input.PackageImportFileSummary)
			}),
		codeSymbolCmd("code-package-symbols", func(input codeIntelCommandInput) bool { return input.PackageSymbols != "" },
			func(cwd string, input codeIntelCommandInput) error {
				return listCodePackageSymbols(cwd, input.PackageSymbols)
			}),
		codeSymbolCmd("code-package-symbol-file-summary", func(input codeIntelCommandInput) bool { return input.PackageSymbolFileSummary != "" },
			func(cwd string, input codeIntelCommandInput) error {
				return listCodePackageSymbolFileSummary(cwd, input.PackageSymbolFileSummary)
			}),
		codeSymbolCmd("code-package-symbol-name", func(input codeIntelCommandInput) bool { return input.PackageSymbolName != "" },
			func(cwd string, input codeIntelCommandInput) error {
				return listCodePackageSymbol(cwd, input.PackageSymbolName)
			}),
		codeSymbolCmd("code-package-symbol-name-file-summary", func(input codeIntelCommandInput) bool { return input.PackageSymbolNameFileSummary != "" },
			func(cwd string, input codeIntelCommandInput) error {
				return listCodePackageSymbolNameFileSummary(cwd, input.PackageSymbolNameFileSummary)
			}),
		codeSymbolCmd("code-package-symbol-list", func(input codeIntelCommandInput) bool { return input.PackageSymbolList != "" },
			func(cwd string, input codeIntelCommandInput) error {
				return listCodePackageSymbolList(cwd, input.PackageSymbolList)
			}),
		codeSymbolCmd("code-package-symbol-kind", func(input codeIntelCommandInput) bool { return input.PackageSymbolKind != "" },
			func(cwd string, input codeIntelCommandInput) error {
				return listCodePackageSymbolKind(cwd, input.PackageSymbolKind)
			}),
		codeSymbolCmd("code-package-symbol-kind-file-summary", func(input codeIntelCommandInput) bool { return input.PackageSymbolKindFileSummary != "" },
			func(cwd string, input codeIntelCommandInput) error {
				return listCodePackageSymbolKindFileSummary(cwd, input.PackageSymbolKindFileSummary)
			}),
		codeSymbolCmd("code-package-symbol-prefix", func(input codeIntelCommandInput) bool { return input.PackageSymbolPrefix != "" },
			func(cwd string, input codeIntelCommandInput) error {
				return listCodePackageSymbolPrefix(cwd, input.PackageSymbolPrefix)
			}),
		codeSymbolCmd("code-package-symbol-prefix-file-summary", func(input codeIntelCommandInput) bool { return input.PackageSymbolPrefixFileSummary != "" },
			func(cwd string, input codeIntelCommandInput) error {
				return listCodePackageSymbolPrefixFileSummary(cwd, input.PackageSymbolPrefixFileSummary)
			}),

		// Code files
		codeSymbolCmd("code-file-path", func(input codeIntelCommandInput) bool { return input.FilePath != "" },
			func(cwd string, input codeIntelCommandInput) error { return showCodeFile(cwd, input.FilePath) }),
		codeSymbolCmd("code-file-imports", func(input codeIntelCommandInput) bool { return input.FileImports != "" },
			func(cwd string, input codeIntelCommandInput) error {
				return listCodeFileImports(cwd, input.FileImports)
			}),
		codeSymbolCmd("code-file-symbols", func(input codeIntelCommandInput) bool { return input.FileSymbols != "" },
			func(cwd string, input codeIntelCommandInput) error {
				return listCodeFileSymbols(cwd, input.FileSymbols)
			}),
		codeSymbolCmd("code-file-symbol-summary", func(input codeIntelCommandInput) bool { return input.FileSymbolSummary != "" },
			func(cwd string, input codeIntelCommandInput) error {
				return listCodeFileSymbolSummary(cwd, input.FileSymbolSummary)
			}),
		codeSymbolCmd("code-file-symbol-name", func(input codeIntelCommandInput) bool { return input.FileSymbolName != "" },
			func(cwd string, input codeIntelCommandInput) error {
				return listCodeFileSymbol(cwd, input.FileSymbolName)
			}),
		codeSymbolCmd("code-file-symbol-kind", func(input codeIntelCommandInput) bool { return input.FileSymbolKind != "" },
			func(cwd string, input codeIntelCommandInput) error {
				return listCodeFileSymbolKind(cwd, input.FileSymbolKind)
			}),
		codeSymbolCmd("code-file-symbol-prefix", func(input codeIntelCommandInput) bool { return input.FileSymbolPrefix != "" },
			func(cwd string, input codeIntelCommandInput) error {
				return listCodeFileSymbolPrefix(cwd, input.FileSymbolPrefix)
			}),
		codeSymbolCmd("code-file-import-prefix", func(input codeIntelCommandInput) bool { return input.FileImportPrefix != "" },
			func(cwd string, input codeIntelCommandInput) error {
				return listCodeFileImportPrefix(cwd, input.FileImportPrefix)
			}),
		codeSymbolCmd("code-file-import-path", func(input codeIntelCommandInput) bool { return input.FileImportPath != "" },
			func(cwd string, input codeIntelCommandInput) error {
				return listCodeFileImportPath(cwd, input.FileImportPath)
			}),

		// Code graph / structure
		codeSymbolCmd("list-code-layers", func(input codeIntelCommandInput) bool { return input.ListLayers },
			func(cwd string, _ codeIntelCommandInput) error { return listCodeLayers(cwd) }),
		codeSymbolCmd("list-code-cycles", func(input codeIntelCommandInput) bool { return input.ListCycles },
			func(cwd string, _ codeIntelCommandInput) error { return listCodeCycles(cwd) }),
		codeSymbolCmd("code-summary", func(input codeIntelCommandInput) bool { return input.Summary },
			func(cwd string, _ codeIntelCommandInput) error { return printCodeSummary(cwd) }),
		codeSymbolCmd("list-code-files", func(input codeIntelCommandInput) bool { return input.ListFiles },
			func(cwd string, _ codeIntelCommandInput) error { return listCodeFiles(cwd) }),
		codeSymbolCmd("code-impact-target", func(input codeIntelCommandInput) bool { return input.ImpactTarget != "" },
			func(cwd string, input codeIntelCommandInput) error { return listCodeImpact(cwd, input.ImpactTarget) }),
		codeSymbolCmd("code-reach-target", func(input codeIntelCommandInput) bool { return input.ReachTarget != "" },
			func(cwd string, input codeIntelCommandInput) error { return listCodeReachable(cwd, input.ReachTarget) }),
		codeSymbolCmd("code-deps-target", func(input codeIntelCommandInput) bool { return input.DepsTarget != "" },
			func(cwd string, input codeIntelCommandInput) error { return listCodeDeps(cwd, input.DepsTarget) }),
		codeSymbolCmd("code-rdeps-target", func(input codeIntelCommandInput) bool { return input.RDepsTarget != "" },
			func(cwd string, input codeIntelCommandInput) error {
				return listCodeReverseDeps(cwd, input.RDepsTarget)
			}),
	}
}

func codeSymbolCmd(
	name string,
	matchFn func(codeIntelCommandInput) bool,
	handler func(cwd string, input codeIntelCommandInput) error,
) codeIntelCommand {
	return codeIntelCommand{
		name:  name,
		match: matchFn,
		run:   handler,
	}
}
