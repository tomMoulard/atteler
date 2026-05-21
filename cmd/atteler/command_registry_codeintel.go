package main

import "context"

type codeIntelCommand struct {
	match func(cliOptions) bool
	run   func(cwd string, opts cliOptions) error
	name  string
}

func providerlessConfigCodeIntelCommands() []command {
	return []command{
		{
			name:  "code-intel",
			tier:  tierProviderlessConfig,
			match: codeIntelCommandRequested,
			runProviderlessConfig: func(_ context.Context, o cliOptions, s appState) error {
				return runCodeIntelCommand(s.cwd, o)
			},
		},
	}
}

func codeIntelCommandRequested(opts cliOptions) bool {
	return matchingCodeIntelCommand(opts) != nil
}

func runCodeIntelCommand(cwd string, opts cliOptions) error {
	cmd := matchingCodeIntelCommand(opts)
	if cmd == nil {
		return nil
	}

	return cmd.run(cwd, opts)
}

func matchingCodeIntelCommand(opts cliOptions) *codeIntelCommand {
	commands := codeIntelCommands()
	for i := range commands {
		cmd := &commands[i]
		if cmd.match(opts) {
			return cmd
		}
	}

	return nil
}

func codeIntelCommands() []codeIntelCommand {
	return []codeIntelCommand{
		// ---------------------------------------------------------------
		// Tier: providerless config -- code analysis
		// ---------------------------------------------------------------
		codeSymbolCmd("code-symbol-name", func(o cliOptions) bool { return o.codeSymbolName != "" },
			func(cwd string, o cliOptions) error { return findCodeSymbol(cwd, o.codeSymbolName) }),
		codeSymbolCmd("code-symbol-file-summary", func(o cliOptions) bool { return o.codeSymbolFileSummary != "" },
			func(cwd string, o cliOptions) error {
				return listCodeSymbolNameFileSummary(cwd, o.codeSymbolFileSummary)
			}),
		codeSymbolCmd("code-symbol-package-summary", func(o cliOptions) bool { return o.codeSymbolPackageSummary != "" },
			func(cwd string, o cliOptions) error {
				return listCodeSymbolNamePackageSummary(cwd, o.codeSymbolPackageSummary)
			}),
		codeSymbolCmd("code-symbol-prefix", func(o cliOptions) bool { return o.codeSymbolPrefix != "" },
			func(cwd string, o cliOptions) error { return findCodeSymbolPrefix(cwd, o.codeSymbolPrefix) }),
		codeSymbolCmd("code-symbol-prefix-file-summary", func(o cliOptions) bool { return o.codeSymbolPrefixFileSummary != "" },
			func(cwd string, o cliOptions) error {
				return listCodeSymbolPrefixFileSummary(cwd, o.codeSymbolPrefixFileSummary)
			}),
		codeSymbolCmd("code-symbol-prefix-package-summary", func(o cliOptions) bool { return o.codeSymbolPrefixPackageSummary != "" },
			func(cwd string, o cliOptions) error {
				return listCodeSymbolPrefixPackageSummary(cwd, o.codeSymbolPrefixPackageSummary)
			}),
		codeSymbolCmd("code-symbol-kind", func(o cliOptions) bool { return o.codeSymbolKind != "" },
			func(cwd string, o cliOptions) error { return findCodeSymbolsByKind(cwd, o.codeSymbolKind) }),
		codeSymbolCmd("code-symbol-kind-file-summary", func(o cliOptions) bool { return o.codeSymbolKindFileSummary != "" },
			func(cwd string, o cliOptions) error {
				return listCodeSymbolKindFileSummary(cwd, o.codeSymbolKindFileSummary)
			}),
		codeSymbolCmd("code-symbol-kind-package-summary", func(o cliOptions) bool { return o.codeSymbolKindPackageSummary != "" },
			func(cwd string, o cliOptions) error {
				return listCodeSymbolKindPackageSummary(cwd, o.codeSymbolKindPackageSummary)
			}),
		codeSymbolCmd("list-code-symbol-summary", func(o cliOptions) bool { return o.listCodeSymbolSummary },
			func(cwd string, _ cliOptions) error { return listCodeSymbolSummary(cwd) }),
		codeSymbolCmd("list-code-symbol-file-summary", func(o cliOptions) bool { return o.listCodeSymbolFileSummary },
			func(cwd string, _ cliOptions) error { return listCodeSymbolFileSummary(cwd) }),

		// Code imports
		codeSymbolCmd("list-code-imports", func(o cliOptions) bool { return o.listCodeImports },
			func(cwd string, _ cliOptions) error { return listCodeImports(cwd) }),
		codeSymbolCmd("list-code-import-summary", func(o cliOptions) bool { return o.listCodeImportSummary },
			func(cwd string, _ cliOptions) error { return listCodeImportSummary(cwd) }),
		codeSymbolCmd("list-code-import-file-summary", func(o cliOptions) bool { return o.listCodeImportFileSummary },
			func(cwd string, _ cliOptions) error { return listCodeImportFileSummary(cwd) }),
		codeSymbolCmd("code-import-path", func(o cliOptions) bool { return o.codeImportPath != "" },
			func(cwd string, o cliOptions) error { return listCodeImportPath(cwd, o.codeImportPath) }),
		codeSymbolCmd("code-import-path-summary", func(o cliOptions) bool { return o.codeImportPathSummary != "" },
			func(cwd string, o cliOptions) error { return listCodeImportPathSummary(cwd, o.codeImportPathSummary) }),
		codeSymbolCmd("code-import-path-file-summary", func(o cliOptions) bool { return o.codeImportPathFileSummary != "" },
			func(cwd string, o cliOptions) error {
				return listCodeImportPathFileSummary(cwd, o.codeImportPathFileSummary)
			}),
		codeSymbolCmd("code-import-path-package-summary", func(o cliOptions) bool { return o.codeImportPathPackageSummary != "" },
			func(cwd string, o cliOptions) error {
				return listCodeImportPathPackageSummary(cwd, o.codeImportPathPackageSummary)
			}),
		codeSymbolCmd("code-import-prefix", func(o cliOptions) bool { return o.codeImportPrefix != "" },
			func(cwd string, o cliOptions) error { return listCodeImportPrefix(cwd, o.codeImportPrefix) }),
		codeSymbolCmd("code-import-prefix-summary", func(o cliOptions) bool { return o.codeImportPrefixSummary != "" },
			func(cwd string, o cliOptions) error {
				return listCodeImportPrefixSummary(cwd, o.codeImportPrefixSummary)
			}),
		codeSymbolCmd("code-import-prefix-file-summary", func(o cliOptions) bool { return o.codeImportPrefixFileSummary != "" },
			func(cwd string, o cliOptions) error {
				return listCodeImportPrefixFileSummary(cwd, o.codeImportPrefixFileSummary)
			}),
		codeSymbolCmd("code-import-prefix-package-summary", func(o cliOptions) bool { return o.codeImportPrefixPackageSummary != "" },
			func(cwd string, o cliOptions) error {
				return listCodeImportPrefixPackageSummary(cwd, o.codeImportPrefixPackageSummary)
			}),

		// Code packages
		codeSymbolCmd("list-code-packages", func(o cliOptions) bool { return o.listCodePackages },
			func(cwd string, _ cliOptions) error { return listCodePackages(cwd) }),
		codeSymbolCmd("code-package-name", func(o cliOptions) bool { return o.codePackageName != "" },
			func(cwd string, o cliOptions) error { return listCodePackageFiles(cwd, o.codePackageName) }),
		codeSymbolCmd("list-code-package-import-summary", func(o cliOptions) bool { return o.listCodePackageImportSummary },
			func(cwd string, _ cliOptions) error { return listCodePackageImportSummary(cwd) }),
		codeSymbolCmd("code-package-imports", func(o cliOptions) bool { return o.codePackageImports != "" },
			func(cwd string, o cliOptions) error { return listCodePackageImports(cwd, o.codePackageImports) }),
		codeSymbolCmd("code-package-import-path", func(o cliOptions) bool { return o.codePackageImportPath != "" },
			func(cwd string, o cliOptions) error { return listCodePackageImportPath(cwd, o.codePackageImportPath) }),
		codeSymbolCmd("code-package-import-files", func(o cliOptions) bool { return o.codePackageImportFiles != "" },
			func(cwd string, o cliOptions) error { return listCodePackageImportFiles(cwd, o.codePackageImportFiles) }),
		codeSymbolCmd("code-package-import-path-file-summary", func(o cliOptions) bool { return o.codePackageImportPathFileSummary != "" },
			func(cwd string, o cliOptions) error {
				return listCodePackageImportPathFileSummary(cwd, o.codePackageImportPathFileSummary)
			}),
		codeSymbolCmd("code-package-import-prefix", func(o cliOptions) bool { return o.codePackageImportPrefix != "" },
			func(cwd string, o cliOptions) error {
				return listCodePackageImportPrefix(cwd, o.codePackageImportPrefix)
			}),
		codeSymbolCmd("code-package-import-prefix-files", func(o cliOptions) bool { return o.codePackageImportPrefixFiles != "" },
			func(cwd string, o cliOptions) error {
				return listCodePackageImportPrefixFiles(cwd, o.codePackageImportPrefixFiles)
			}),
		codeSymbolCmd("code-package-import-prefix-file-summary", func(o cliOptions) bool { return o.codePackageImportPrefixFileSummary != "" },
			func(cwd string, o cliOptions) error {
				return listCodePackageImportPrefixFileSummary(cwd, o.codePackageImportPrefixFileSummary)
			}),
		codeSymbolCmd("code-package-import-file-summary", func(o cliOptions) bool { return o.codePackageImportFileSummary != "" },
			func(cwd string, o cliOptions) error {
				return listCodePackageImportFileSummary(cwd, o.codePackageImportFileSummary)
			}),
		codeSymbolCmd("code-package-symbols", func(o cliOptions) bool { return o.codePackageSymbols != "" },
			func(cwd string, o cliOptions) error { return listCodePackageSymbols(cwd, o.codePackageSymbols) }),
		codeSymbolCmd("code-package-symbol-file-summary", func(o cliOptions) bool { return o.codePackageSymbolFileSummary != "" },
			func(cwd string, o cliOptions) error {
				return listCodePackageSymbolFileSummary(cwd, o.codePackageSymbolFileSummary)
			}),
		codeSymbolCmd("code-package-symbol-name", func(o cliOptions) bool { return o.codePackageSymbolName != "" },
			func(cwd string, o cliOptions) error { return listCodePackageSymbol(cwd, o.codePackageSymbolName) }),
		codeSymbolCmd("code-package-symbol-name-file-summary", func(o cliOptions) bool { return o.codePackageSymbolNameFileSummary != "" },
			func(cwd string, o cliOptions) error {
				return listCodePackageSymbolNameFileSummary(cwd, o.codePackageSymbolNameFileSummary)
			}),
		codeSymbolCmd("code-package-symbol-list", func(o cliOptions) bool { return o.codePackageSymbolList != "" },
			func(cwd string, o cliOptions) error { return listCodePackageSymbolList(cwd, o.codePackageSymbolList) }),
		codeSymbolCmd("code-package-symbol-kind", func(o cliOptions) bool { return o.codePackageSymbolKind != "" },
			func(cwd string, o cliOptions) error { return listCodePackageSymbolKind(cwd, o.codePackageSymbolKind) }),
		codeSymbolCmd("code-package-symbol-kind-file-summary", func(o cliOptions) bool { return o.codePackageSymbolKindFileSummary != "" },
			func(cwd string, o cliOptions) error {
				return listCodePackageSymbolKindFileSummary(cwd, o.codePackageSymbolKindFileSummary)
			}),
		codeSymbolCmd("code-package-symbol-prefix", func(o cliOptions) bool { return o.codePackageSymbolPrefix != "" },
			func(cwd string, o cliOptions) error {
				return listCodePackageSymbolPrefix(cwd, o.codePackageSymbolPrefix)
			}),
		codeSymbolCmd("code-package-symbol-prefix-file-summary", func(o cliOptions) bool { return o.codePackageSymbolPrefixFileSummary != "" },
			func(cwd string, o cliOptions) error {
				return listCodePackageSymbolPrefixFileSummary(cwd, o.codePackageSymbolPrefixFileSummary)
			}),

		// Code files
		codeSymbolCmd("code-file-path", func(o cliOptions) bool { return o.codeFilePath != "" },
			func(cwd string, o cliOptions) error { return showCodeFile(cwd, o.codeFilePath) }),
		codeSymbolCmd("code-file-imports", func(o cliOptions) bool { return o.codeFileImports != "" },
			func(cwd string, o cliOptions) error { return listCodeFileImports(cwd, o.codeFileImports) }),
		codeSymbolCmd("code-file-symbols", func(o cliOptions) bool { return o.codeFileSymbols != "" },
			func(cwd string, o cliOptions) error { return listCodeFileSymbols(cwd, o.codeFileSymbols) }),
		codeSymbolCmd("code-file-symbol-summary", func(o cliOptions) bool { return o.codeFileSymbolSummary != "" },
			func(cwd string, o cliOptions) error { return listCodeFileSymbolSummary(cwd, o.codeFileSymbolSummary) }),
		codeSymbolCmd("code-file-symbol-name", func(o cliOptions) bool { return o.codeFileSymbolName != "" },
			func(cwd string, o cliOptions) error { return listCodeFileSymbol(cwd, o.codeFileSymbolName) }),
		codeSymbolCmd("code-file-symbol-kind", func(o cliOptions) bool { return o.codeFileSymbolKind != "" },
			func(cwd string, o cliOptions) error { return listCodeFileSymbolKind(cwd, o.codeFileSymbolKind) }),
		codeSymbolCmd("code-file-symbol-prefix", func(o cliOptions) bool { return o.codeFileSymbolPrefix != "" },
			func(cwd string, o cliOptions) error { return listCodeFileSymbolPrefix(cwd, o.codeFileSymbolPrefix) }),
		codeSymbolCmd("code-file-import-prefix", func(o cliOptions) bool { return o.codeFileImportPrefix != "" },
			func(cwd string, o cliOptions) error { return listCodeFileImportPrefix(cwd, o.codeFileImportPrefix) }),
		codeSymbolCmd("code-file-import-path", func(o cliOptions) bool { return o.codeFileImportPath != "" },
			func(cwd string, o cliOptions) error { return listCodeFileImportPath(cwd, o.codeFileImportPath) }),

		// Code graph / structure
		codeSymbolCmd("list-code-layers", func(o cliOptions) bool { return o.listCodeLayers },
			func(cwd string, _ cliOptions) error { return listCodeLayers(cwd) }),
		codeSymbolCmd("list-code-cycles", func(o cliOptions) bool { return o.listCodeCycles },
			func(cwd string, _ cliOptions) error { return listCodeCycles(cwd) }),
		codeSymbolCmd("code-summary", func(o cliOptions) bool { return o.codeSummary },
			func(cwd string, _ cliOptions) error { return printCodeSummary(cwd) }),
		codeSymbolCmd("list-code-files", func(o cliOptions) bool { return o.listCodeFiles },
			func(cwd string, _ cliOptions) error { return listCodeFiles(cwd) }),
		codeSymbolCmd("code-impact-target", func(o cliOptions) bool { return o.codeImpactTarget != "" },
			func(cwd string, o cliOptions) error { return listCodeImpact(cwd, o.codeImpactTarget) }),
		codeSymbolCmd("code-reach-target", func(o cliOptions) bool { return o.codeReachTarget != "" },
			func(cwd string, o cliOptions) error { return listCodeReachable(cwd, o.codeReachTarget) }),
		codeSymbolCmd("code-deps-target", func(o cliOptions) bool { return o.codeDepsTarget != "" },
			func(cwd string, o cliOptions) error { return listCodeDeps(cwd, o.codeDepsTarget) }),
		codeSymbolCmd("code-rdeps-target", func(o cliOptions) bool { return o.codeRdepsTarget != "" },
			func(cwd string, o cliOptions) error { return listCodeReverseDeps(cwd, o.codeRdepsTarget) }),
	}
}

func codeSymbolCmd(
	name string,
	matchFn func(cliOptions) bool,
	handler func(cwd string, opts cliOptions) error,
) codeIntelCommand {
	return codeIntelCommand{
		name:  name,
		match: matchFn,
		run:   handler,
	}
}
