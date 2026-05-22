package main

type agentPerformanceSummaryCommandInput struct{}

type agentMemoryCommandInput struct {
	Search     string
	Agent      string
	StorePath  string
	IndexFiles []string
	Limit      int
}

type asyncPlanCommandInput struct {
	TaskSpecs []string
}

type asyncRunCommandInput struct {
	SpawnBinary    string
	TaskSpecs      []string
	TimeoutSeconds int
}

type codeIntelCommandInput struct {
	SymbolName                     string
	SymbolFileSummary              string
	SymbolPackageSummary           string
	SymbolPrefix                   string
	SymbolPrefixFileSummary        string
	SymbolPrefixPackageSummary     string
	SymbolKind                     string
	SymbolKindFileSummary          string
	SymbolKindPackageSummary       string
	ImpactTarget                   string
	ImportPath                     string
	ImportPathSummary              string
	ImportPathFileSummary          string
	ImportPathPackageSummary       string
	ImportPrefix                   string
	ImportPrefixSummary            string
	ImportPrefixFileSummary        string
	ImportPrefixPackageSummary     string
	ReachTarget                    string
	DepsTarget                     string
	RDepsTarget                    string
	PackageName                    string
	PackageImports                 string
	PackageImportPath              string
	PackageImportFiles             string
	PackageImportPathFileSummary   string
	PackageImportPrefix            string
	PackageImportPrefixFiles       string
	PackageImportPrefixFileSummary string
	PackageImportFileSummary       string
	PackageSymbols                 string
	PackageSymbolFileSummary       string
	PackageSymbolName              string
	PackageSymbolNameFileSummary   string
	PackageSymbolList              string
	PackageSymbolKind              string
	PackageSymbolKindFileSummary   string
	PackageSymbolPrefix            string
	PackageSymbolPrefixFileSummary string
	FilePath                       string
	FileImports                    string
	FileSymbols                    string
	FileSymbolSummary              string
	FileSymbolName                 string
	FileSymbolKind                 string
	FileSymbolPrefix               string
	FileImportPrefix               string
	FileImportPath                 string
	ListImports                    bool
	ListImportSummary              bool
	ListImportFileSummary          bool
	ListLayers                     bool
	ListCycles                     bool
	Summary                        bool
	ListFiles                      bool
	ListSymbolSummary              bool
	ListSymbolFileSummary          bool
	ListPackageImportSummary       bool
	ListPackages                   bool
}

type contextPackCommandInput struct {
	Path      string
	Model     string
	MaxTokens int
}

type commandSurfaceDocsCommandInput struct{}

type commandSurfaceJSONCommandInput struct{}

type describeAgentCommandInput struct {
	Name string
}

type describePluginCommandInput struct {
	Name string
}

type doctorCommandInput struct{}

type doctorOfflineCommandInput struct {
	SessionDir string
}

type evalOutputCommandInput struct {
	ActualPath   string
	ExpectedText string
	ExpectedPath string
	Mode         string
}

type explainConfigCommandInput struct {
	FieldPath string
}

type feedbackProposalsCommandInput struct{}

type feedbackApproveCommandInput struct {
	ConfigPath  string
	HistoryPath string
	Agent       string
	ID          string
}

type feedbackRollbackCommandInput struct {
	ConfigPath  string
	HistoryPath string
	Agent       string
	ID          string
	Reason      string
}

type gitHistorySearchCommandInput struct {
	Query string
	Limit int
}

type initConfigCommandInput struct {
	Path string
}

type initRTKPluginCommandInput struct {
	Dir string
}

type listAgentsCommandInput struct{}

type listConfigPathsCommandInput struct{}

type listHeadlessCommandInput struct{}

type recoverHeadlessCommandInput struct{}

type listHookEventsCommandInput struct {
	JSON bool
}

type listKnownModelsCommandInput struct{}

type listModelsCommandInput struct{}

type listPluginsCommandInput struct{}

type listProvidersCommandInput struct{}

type listSessionsCommandInput struct {
	Tag string
}

type listSessionTagsCommandInput struct{}

type listWorktreesCommandInput struct{}

type lspSymbolsCommandInput struct {
	Command          string
	FilePath         string
	RootPath         string
	LanguageID       string
	WorkspaceSymbols string
	Args             []string
	DocumentSymbols  bool
}

type memoryCommandInput struct {
	Search     string
	StorePath  string
	IndexFiles []string
	Limit      int
}

type mergeArtifactsCommandInput struct {
	OutputPath string
	MaxBytes   int
}

type mergeWorktreeCommandInput struct {
	Ref               string
	AllowBaseMismatch bool
}

type planAgentsCommandInput struct {
	Prompt     string
	AgentNames []string
	MaxAgents  int
}

type printConfigTemplateCommandInput struct{}

type promptCompleteCommandInput struct {
	Input      string
	SessionRef string
	AgentName  string
	Limit      int
}

type reviewPlanCommandInput struct {
	Agents []string
	Paths  []string
	Gates  []string
}

type reviewRunCommandInput struct {
	Prompt string
	Agents []string
	Paths  []string
	Gates  []string
}

type reviewScanCommandInput struct {
	LargeFileBytes int
}

type searchSessionsCommandInput struct {
	Query string
}

type showVersionCommandInput struct{}

type runPluginCommandInput struct {
	Target         string
	Entrypoint     string
	TimeoutSeconds int
	DryRun         bool
}

type speculatePlanCommandInput struct {
	Prompt string
	Agents []string
	Gates  []string
}

type speculateRunCommandInput struct {
	Prompt string
	Agents []string
	Gates  []string
}

type streamHeadlessCommandInput struct {
	ID string
}

type stateDiagnosticsCommandInput struct {
	SessionRef     string
	AgentName      string
	Model          string
	ReasoningLevel string
}

type suggestSkillCommandInput struct {
	SaveDir        string
	Steps          []string
	MaxSteps       int
	MinOccurrences int
}

type vectorSearchCommandInput struct {
	Query      string
	IndexFiles []string
	Limit      int
}

type validateConfigCommandInput struct{}

type watchLoopCommandInput struct {
	LargeFileBytes  int
	IntervalSeconds int
	MaxIterations   int
}

type watchScanCommandInput struct {
	LargeFileBytes int
	JSON           bool
}

type commandInputBuilder func(cliOptions) any

func commandInputBuildersByType() map[string]commandInputBuilder {
	return map[string]commandInputBuilder{
		"agentPerformanceSummaryCommandInput": func(opts cliOptions) any { return agentPerformanceSummaryCommandInputFromOptions(opts) },
		"agentMemoryCommandInput":             func(opts cliOptions) any { return agentMemoryCommandInputFromOptions(opts) },
		"asyncPlanCommandInput":               func(opts cliOptions) any { return asyncPlanCommandInputFromOptions(opts) },
		"asyncRunCommandInput":                func(opts cliOptions) any { return asyncRunCommandInputFromOptions(opts) },
		"bashCommandInput":                    func(opts cliOptions) any { return bashCommandInputFromOptions(opts) },
		"codeIntelCommandInput":               func(opts cliOptions) any { return codeIntelCommandInputFromOptions(opts) },
		"commandSurfaceDocsCommandInput":      func(opts cliOptions) any { return commandSurfaceDocsCommandInputFromOptions(opts) },
		"commandSurfaceJSONCommandInput":      func(opts cliOptions) any { return commandSurfaceJSONCommandInputFromOptions(opts) },
		"contextPackCommandInput":             func(opts cliOptions) any { return contextPackCommandInputFromOptions(opts) },
		"describeAgentCommandInput":           func(opts cliOptions) any { return describeAgentCommandInputFromOptions(opts) },
		"describePluginCommandInput":          func(opts cliOptions) any { return describePluginCommandInputFromOptions(opts) },
		"doctorCommandInput":                  func(opts cliOptions) any { return doctorCommandInputFromOptions(opts) },
		"doctorOfflineCommandInput":           func(opts cliOptions) any { return doctorOfflineCommandInputFromOptions(opts) },
		"evalOutputCommandInput":              func(opts cliOptions) any { return evalOutputCommandInputFromOptions(opts) },
		"explainConfigCommandInput":           func(opts cliOptions) any { return explainConfigCommandInputFromOptions(opts) },
		"feedbackProposalsCommandInput":       func(opts cliOptions) any { return feedbackProposalsCommandInputFromOptions(opts) },
		"feedbackApproveCommandInput":         func(opts cliOptions) any { return feedbackApproveCommandInputFromOptions(opts) },
		"feedbackRollbackCommandInput":        func(opts cliOptions) any { return feedbackRollbackCommandInputFromOptions(opts) },
		"gitHistorySearchCommandInput":        func(opts cliOptions) any { return gitHistorySearchCommandInputFromOptions(opts) },
		"initConfigCommandInput":              func(opts cliOptions) any { return initConfigCommandInputFromOptions(opts) },
		"initRTKPluginCommandInput":           func(opts cliOptions) any { return initRTKPluginCommandInputFromOptions(opts) },
		"listAgentsCommandInput":              func(opts cliOptions) any { return listAgentsCommandInputFromOptions(opts) },
		"listConfigPathsCommandInput":         func(opts cliOptions) any { return listConfigPathsCommandInputFromOptions(opts) },
		"listHeadlessCommandInput":            func(opts cliOptions) any { return listHeadlessCommandInputFromOptions(opts) },
		"recoverHeadlessCommandInput":         func(opts cliOptions) any { return recoverHeadlessCommandInputFromOptions(opts) },
		"listHookEventsCommandInput":          func(opts cliOptions) any { return listHookEventsCommandInputFromOptions(opts) },
		"listKnownModelsCommandInput":         func(opts cliOptions) any { return listKnownModelsCommandInputFromOptions(opts) },
		"listModelsCommandInput":              func(opts cliOptions) any { return listModelsCommandInputFromOptions(opts) },
		"listPluginsCommandInput":             func(opts cliOptions) any { return listPluginsCommandInputFromOptions(opts) },
		"listProvidersCommandInput":           func(opts cliOptions) any { return listProvidersCommandInputFromOptions(opts) },
		"listSessionsCommandInput":            func(opts cliOptions) any { return listSessionsCommandInputFromOptions(opts) },
		"listSessionTagsCommandInput":         func(opts cliOptions) any { return listSessionTagsCommandInputFromOptions(opts) },
		"listWorktreesCommandInput":           func(opts cliOptions) any { return listWorktreesCommandInputFromOptions(opts) },
		"lspSymbolsCommandInput":              func(opts cliOptions) any { return lspSymbolsCommandInputFromOptions(opts) },
		"mcpInvokeCommandInput":               func(opts cliOptions) any { return mcpInvokeCommandInputFromOptions(opts) },
		"mcpManifestCommandInput":             func(opts cliOptions) any { return mcpManifestCommandInputFromOptions(opts) },
		"memoryCommandInput":                  func(opts cliOptions) any { return memoryCommandInputFromOptions(opts) },
		"mergeArtifactsCommandInput":          func(opts cliOptions) any { return mergeArtifactsCommandInputFromOptions(opts) },
		"mergeWorktreeCommandInput":           func(opts cliOptions) any { return mergeWorktreeCommandInputFromOptions(opts) },
		"planAgentsCommandInput":              func(opts cliOptions) any { return planAgentsCommandInputFromOptions(opts) },
		"printConfigTemplateCommandInput":     func(opts cliOptions) any { return printConfigTemplateCommandInputFromOptions(opts) },
		"promptCompleteCommandInput":          func(opts cliOptions) any { return promptCompleteCommandInputFromOptions(opts) },
		"reviewPlanCommandInput":              func(opts cliOptions) any { return reviewPlanCommandInputFromOptions(opts) },
		"reviewRunCommandInput":               func(opts cliOptions) any { return reviewRunCommandInputFromOptions(opts) },
		"reviewScanCommandInput":              func(opts cliOptions) any { return reviewScanCommandInputFromOptions(opts) },
		"routeModelsCommandInput":             func(opts cliOptions) any { return routeModelsCommandInputFromOptions(opts) },
		"runPluginCommandInput":               func(opts cliOptions) any { return runPluginCommandInputFromOptions(opts) },
		"searchSessionsCommandInput":          func(opts cliOptions) any { return searchSessionsCommandInputFromOptions(opts) },
		"sessionReadCommandInput":             func(opts cliOptions) any { return sessionReadCommandInputFromOptions(opts) },
		"sessionWriteCommandInput":            func(opts cliOptions) any { return sessionWriteCommandInputFromOptions(opts) },
		"showVersionCommandInput":             func(opts cliOptions) any { return showVersionCommandInputFromOptions(opts) },
		"spawnAgentsCommandInput":             func(opts cliOptions) any { return spawnAgentsCommandInputFromOptions(opts) },
		"speculatePlanCommandInput":           func(opts cliOptions) any { return speculatePlanCommandInputFromOptions(opts) },
		"speculateRunCommandInput":            func(opts cliOptions) any { return speculateRunCommandInputFromOptions(opts) },
		"stateDiagnosticsCommandInput":        func(opts cliOptions) any { return stateDiagnosticsCommandInputFromOptions(opts) },
		"streamHeadlessCommandInput":          func(opts cliOptions) any { return streamHeadlessCommandInputFromOptions(opts) },
		"suggestSkillCommandInput":            func(opts cliOptions) any { return suggestSkillCommandInputFromOptions(opts) },
		"taskCommandInput":                    func(opts cliOptions) any { return taskCommandInputFromOptions(opts) },
		"validateConfigCommandInput":          func(opts cliOptions) any { return validateConfigCommandInputFromOptions(opts) },
		"vectorSearchCommandInput":            func(opts cliOptions) any { return vectorSearchCommandInputFromOptions(opts) },
		"watchLoopCommandInput":               func(opts cliOptions) any { return watchLoopCommandInputFromOptions(opts) },
		"watchScanCommandInput":               func(opts cliOptions) any { return watchScanCommandInputFromOptions(opts) },
	}
}

func agentPerformanceSummaryCommandInputFromOptions(_ cliOptions) agentPerformanceSummaryCommandInput {
	return agentPerformanceSummaryCommandInput{}
}

func agentMemoryCommandInputFromOptions(opts cliOptions) agentMemoryCommandInput {
	return agentMemoryCommandInput{
		Search:     opts.agentMemorySearch,
		Agent:      opts.agentMemoryAgent,
		StorePath:  opts.agentMemoryStorePath,
		IndexFiles: append([]string(nil), opts.agentMemoryIndexFiles...),
		Limit:      opts.agentMemoryLimit.value,
	}
}

func asyncPlanCommandInputFromOptions(opts cliOptions) asyncPlanCommandInput {
	return asyncPlanCommandInput{TaskSpecs: append([]string(nil), opts.asyncTaskSpecs...)}
}

func asyncRunCommandInputFromOptions(opts cliOptions) asyncRunCommandInput {
	return asyncRunCommandInput{
		SpawnBinary:    opts.spawnBinary,
		TaskSpecs:      append([]string(nil), opts.asyncTaskSpecs...),
		TimeoutSeconds: opts.spawnTimeout.value,
	}
}

func codeIntelCommandInputFromOptions(opts cliOptions) codeIntelCommandInput {
	return codeIntelCommandInput{
		SymbolName:                     opts.codeSymbolName,
		SymbolFileSummary:              opts.codeSymbolFileSummary,
		SymbolPackageSummary:           opts.codeSymbolPackageSummary,
		SymbolPrefix:                   opts.codeSymbolPrefix,
		SymbolPrefixFileSummary:        opts.codeSymbolPrefixFileSummary,
		SymbolPrefixPackageSummary:     opts.codeSymbolPrefixPackageSummary,
		SymbolKind:                     opts.codeSymbolKind,
		SymbolKindFileSummary:          opts.codeSymbolKindFileSummary,
		SymbolKindPackageSummary:       opts.codeSymbolKindPackageSummary,
		ImpactTarget:                   opts.codeImpactTarget,
		ImportPath:                     opts.codeImportPath,
		ImportPathSummary:              opts.codeImportPathSummary,
		ImportPathFileSummary:          opts.codeImportPathFileSummary,
		ImportPathPackageSummary:       opts.codeImportPathPackageSummary,
		ImportPrefix:                   opts.codeImportPrefix,
		ImportPrefixSummary:            opts.codeImportPrefixSummary,
		ImportPrefixFileSummary:        opts.codeImportPrefixFileSummary,
		ImportPrefixPackageSummary:     opts.codeImportPrefixPackageSummary,
		ReachTarget:                    opts.codeReachTarget,
		DepsTarget:                     opts.codeDepsTarget,
		RDepsTarget:                    opts.codeRdepsTarget,
		PackageName:                    opts.codePackageName,
		PackageImports:                 opts.codePackageImports,
		PackageImportPath:              opts.codePackageImportPath,
		PackageImportFiles:             opts.codePackageImportFiles,
		PackageImportPathFileSummary:   opts.codePackageImportPathFileSummary,
		PackageImportPrefix:            opts.codePackageImportPrefix,
		PackageImportPrefixFiles:       opts.codePackageImportPrefixFiles,
		PackageImportPrefixFileSummary: opts.codePackageImportPrefixFileSummary,
		PackageImportFileSummary:       opts.codePackageImportFileSummary,
		PackageSymbols:                 opts.codePackageSymbols,
		PackageSymbolFileSummary:       opts.codePackageSymbolFileSummary,
		PackageSymbolName:              opts.codePackageSymbolName,
		PackageSymbolNameFileSummary:   opts.codePackageSymbolNameFileSummary,
		PackageSymbolList:              opts.codePackageSymbolList,
		PackageSymbolKind:              opts.codePackageSymbolKind,
		PackageSymbolKindFileSummary:   opts.codePackageSymbolKindFileSummary,
		PackageSymbolPrefix:            opts.codePackageSymbolPrefix,
		PackageSymbolPrefixFileSummary: opts.codePackageSymbolPrefixFileSummary,
		FilePath:                       opts.codeFilePath,
		FileImports:                    opts.codeFileImports,
		FileSymbols:                    opts.codeFileSymbols,
		FileSymbolSummary:              opts.codeFileSymbolSummary,
		FileSymbolName:                 opts.codeFileSymbolName,
		FileSymbolKind:                 opts.codeFileSymbolKind,
		FileSymbolPrefix:               opts.codeFileSymbolPrefix,
		FileImportPrefix:               opts.codeFileImportPrefix,
		FileImportPath:                 opts.codeFileImportPath,
		ListImports:                    opts.listCodeImports,
		ListImportSummary:              opts.listCodeImportSummary,
		ListImportFileSummary:          opts.listCodeImportFileSummary,
		ListLayers:                     opts.listCodeLayers,
		ListCycles:                     opts.listCodeCycles,
		Summary:                        opts.codeSummary,
		ListFiles:                      opts.listCodeFiles,
		ListSymbolSummary:              opts.listCodeSymbolSummary,
		ListSymbolFileSummary:          opts.listCodeSymbolFileSummary,
		ListPackageImportSummary:       opts.listCodePackageImportSummary,
		ListPackages:                   opts.listCodePackages,
	}
}

func commandSurfaceDocsCommandInputFromOptions(_ cliOptions) commandSurfaceDocsCommandInput {
	return commandSurfaceDocsCommandInput{}
}

func commandSurfaceJSONCommandInputFromOptions(_ cliOptions) commandSurfaceJSONCommandInput {
	return commandSurfaceJSONCommandInput{}
}

func contextPackCommandInputFromOptions(opts cliOptions) contextPackCommandInput {
	return contextPackCommandInput{Path: opts.contextPackPath, Model: opts.model, MaxTokens: opts.contextPackTokens.value}
}

func describeAgentCommandInputFromOptions(opts cliOptions) describeAgentCommandInput {
	return describeAgentCommandInput{Name: opts.describeAgentName}
}

func describePluginCommandInputFromOptions(opts cliOptions) describePluginCommandInput {
	return describePluginCommandInput{Name: opts.describePluginName}
}

func doctorCommandInputFromOptions(_ cliOptions) doctorCommandInput {
	return doctorCommandInput{}
}

func doctorOfflineCommandInputFromOptions(opts cliOptions) doctorOfflineCommandInput {
	return doctorOfflineCommandInput{SessionDir: opts.sessionDir}
}

func evalOutputCommandInputFromOptions(opts cliOptions) evalOutputCommandInput {
	return evalOutputCommandInput{
		ActualPath:   opts.evalOutputPath,
		ExpectedText: opts.evalExpected,
		ExpectedPath: opts.evalExpectedPath,
		Mode:         opts.evalMode,
	}
}

func explainConfigCommandInputFromOptions(opts cliOptions) explainConfigCommandInput {
	return explainConfigCommandInput{FieldPath: opts.explainConfigPath}
}

func feedbackProposalsCommandInputFromOptions(_ cliOptions) feedbackProposalsCommandInput {
	return feedbackProposalsCommandInput{}
}

func feedbackApproveCommandInputFromOptions(opts cliOptions) feedbackApproveCommandInput {
	return feedbackApproveCommandInput{
		ConfigPath:  opts.feedbackApproveConfig,
		HistoryPath: opts.feedbackHistoryPath,
		Agent:       opts.feedbackApproveAgent,
		ID:          opts.feedbackApproveID,
	}
}

func feedbackRollbackCommandInputFromOptions(opts cliOptions) feedbackRollbackCommandInput {
	return feedbackRollbackCommandInput{
		ConfigPath:  opts.feedbackRollbackConfig,
		HistoryPath: opts.feedbackHistoryPath,
		Agent:       opts.feedbackRollbackAgent,
		ID:          opts.feedbackRollbackID,
		Reason:      opts.feedbackRollbackReason,
	}
}

func gitHistorySearchCommandInputFromOptions(opts cliOptions) gitHistorySearchCommandInput {
	return gitHistorySearchCommandInput{Query: opts.gitHistorySearch, Limit: opts.gitHistoryLimit.value}
}

func initConfigCommandInputFromOptions(opts cliOptions) initConfigCommandInput {
	return initConfigCommandInput{Path: opts.initConfigPath}
}

func initRTKPluginCommandInputFromOptions(opts cliOptions) initRTKPluginCommandInput {
	return initRTKPluginCommandInput{Dir: opts.initRTKPluginDir}
}

func listAgentsCommandInputFromOptions(_ cliOptions) listAgentsCommandInput {
	return listAgentsCommandInput{}
}

func listConfigPathsCommandInputFromOptions(_ cliOptions) listConfigPathsCommandInput {
	return listConfigPathsCommandInput{}
}

func listHeadlessCommandInputFromOptions(_ cliOptions) listHeadlessCommandInput {
	return listHeadlessCommandInput{}
}

func recoverHeadlessCommandInputFromOptions(_ cliOptions) recoverHeadlessCommandInput {
	return recoverHeadlessCommandInput{}
}

func listHookEventsCommandInputFromOptions(opts cliOptions) listHookEventsCommandInput {
	return listHookEventsCommandInput{JSON: opts.listHookEventsJSON}
}

func listKnownModelsCommandInputFromOptions(_ cliOptions) listKnownModelsCommandInput {
	return listKnownModelsCommandInput{}
}

func listModelsCommandInputFromOptions(_ cliOptions) listModelsCommandInput {
	return listModelsCommandInput{}
}

func listPluginsCommandInputFromOptions(_ cliOptions) listPluginsCommandInput {
	return listPluginsCommandInput{}
}

func listProvidersCommandInputFromOptions(_ cliOptions) listProvidersCommandInput {
	return listProvidersCommandInput{}
}

func listSessionsCommandInputFromOptions(opts cliOptions) listSessionsCommandInput {
	return listSessionsCommandInput{Tag: opts.listSessionsTag}
}

func listSessionTagsCommandInputFromOptions(_ cliOptions) listSessionTagsCommandInput {
	return listSessionTagsCommandInput{}
}

func listWorktreesCommandInputFromOptions(_ cliOptions) listWorktreesCommandInput {
	return listWorktreesCommandInput{}
}

func lspSymbolsCommandInputFromOptions(opts cliOptions) lspSymbolsCommandInput {
	return lspSymbolsCommandInput{
		Command:          opts.lspCommand,
		FilePath:         opts.lspFilePath,
		RootPath:         opts.lspRootPath,
		LanguageID:       opts.lspLanguageID,
		WorkspaceSymbols: opts.lspWorkspaceSymbols,
		Args:             append([]string(nil), opts.lspArgs...),
		DocumentSymbols:  opts.lspSymbols,
	}
}

func memoryCommandInputFromOptions(opts cliOptions) memoryCommandInput {
	return memoryCommandInput{
		Search:     opts.memorySearch,
		StorePath:  opts.memoryStorePath,
		IndexFiles: append([]string(nil), opts.memoryIndexFiles...),
		Limit:      opts.memoryLimit.value,
	}
}

func mergeArtifactsCommandInputFromOptions(opts cliOptions) mergeArtifactsCommandInput {
	return mergeArtifactsCommandInput{OutputPath: opts.mergeArtifactsPath, MaxBytes: opts.mergeArtifactMaxBytes.value}
}

func mergeWorktreeCommandInputFromOptions(opts cliOptions) mergeWorktreeCommandInput {
	return mergeWorktreeCommandInput{
		Ref:               opts.mergeWorktreeRef,
		AllowBaseMismatch: opts.mergeWorktreeAllowBaseMismatch,
	}
}

func planAgentsCommandInputFromOptions(opts cliOptions) planAgentsCommandInput {
	return planAgentsCommandInput{
		Prompt:     opts.planAgentsPrompt,
		AgentNames: append([]string(nil), opts.planAgentNames...),
		MaxAgents:  opts.planMaxAgents.value,
	}
}

func printConfigTemplateCommandInputFromOptions(_ cliOptions) printConfigTemplateCommandInput {
	return printConfigTemplateCommandInput{}
}

func promptCompleteCommandInputFromOptions(opts cliOptions) promptCompleteCommandInput {
	return promptCompleteCommandInput{
		Input:      opts.promptCompleteInput,
		SessionRef: opts.sessionRef,
		AgentName:  opts.agentName,
		Limit:      opts.promptCompleteLimit.value,
	}
}

func reviewPlanCommandInputFromOptions(opts cliOptions) reviewPlanCommandInput {
	return reviewPlanCommandInput{
		Agents: append([]string(nil), opts.reviewAgents...),
		Paths:  append([]string(nil), opts.reviewPaths...),
		Gates:  append([]string(nil), opts.reviewGates...),
	}
}

func reviewRunCommandInputFromOptions(opts cliOptions) reviewRunCommandInput {
	return reviewRunCommandInput{
		Prompt: opts.reviewPrompt,
		Agents: append([]string(nil), opts.reviewAgents...),
		Paths:  append([]string(nil), opts.reviewPaths...),
		Gates:  append([]string(nil), opts.reviewGates...),
	}
}

func reviewScanCommandInputFromOptions(opts cliOptions) reviewScanCommandInput {
	return reviewScanCommandInput{LargeFileBytes: opts.watchLargeFileBytes.value}
}

func runPluginCommandInputFromOptions(opts cliOptions) runPluginCommandInput {
	return runPluginCommandInput{
		Target:         opts.runPluginTarget,
		Entrypoint:     opts.pluginEntrypoint,
		TimeoutSeconds: opts.pluginTimeout.value,
		DryRun:         opts.pluginDryRun,
	}
}

func searchSessionsCommandInputFromOptions(opts cliOptions) searchSessionsCommandInput {
	return searchSessionsCommandInput{Query: opts.searchQuery}
}

func showVersionCommandInputFromOptions(_ cliOptions) showVersionCommandInput {
	return showVersionCommandInput{}
}

func speculatePlanCommandInputFromOptions(opts cliOptions) speculatePlanCommandInput {
	return speculatePlanCommandInput{
		Prompt: opts.speculatePrompt,
		Agents: append([]string(nil), opts.speculateAgents...),
		Gates:  append([]string(nil), opts.speculateGates...),
	}
}

func speculateRunCommandInputFromOptions(opts cliOptions) speculateRunCommandInput {
	return speculateRunCommandInput{
		Prompt: opts.speculatePrompt,
		Agents: append([]string(nil), opts.speculateAgents...),
		Gates:  append([]string(nil), opts.speculateGates...),
	}
}

func streamHeadlessCommandInputFromOptions(opts cliOptions) streamHeadlessCommandInput {
	return streamHeadlessCommandInput{ID: opts.streamHeadlessID}
}

func stateDiagnosticsCommandInputFromOptions(opts cliOptions) stateDiagnosticsCommandInput {
	return stateDiagnosticsCommandInput{
		SessionRef:     opts.sessionRef,
		AgentName:      opts.agentName,
		Model:          opts.model,
		ReasoningLevel: opts.reasoningLevel,
	}
}

func suggestSkillCommandInputFromOptions(opts cliOptions) suggestSkillCommandInput {
	return suggestSkillCommandInput{
		SaveDir:        opts.skillSaveDir,
		Steps:          append([]string(nil), opts.suggestSkillSteps...),
		MaxSteps:       opts.skillMaxSteps.value,
		MinOccurrences: opts.skillMinOccurrences.value,
	}
}

func validateConfigCommandInputFromOptions(_ cliOptions) validateConfigCommandInput {
	return validateConfigCommandInput{}
}

func vectorSearchCommandInputFromOptions(opts cliOptions) vectorSearchCommandInput {
	return vectorSearchCommandInput{
		Query:      opts.vectorSearch,
		IndexFiles: append([]string(nil), opts.vectorIndexFiles...),
		Limit:      opts.vectorLimit.value,
	}
}

func watchLoopCommandInputFromOptions(opts cliOptions) watchLoopCommandInput {
	return watchLoopCommandInput{
		LargeFileBytes:  opts.watchLargeFileBytes.value,
		IntervalSeconds: opts.watchIntervalSeconds.value,
		MaxIterations:   opts.watchMaxIterations.value,
	}
}

func watchScanCommandInputFromOptions(opts cliOptions) watchScanCommandInput {
	return watchScanCommandInput{LargeFileBytes: opts.watchLargeFileBytes.value, JSON: opts.watchJSON}
}
