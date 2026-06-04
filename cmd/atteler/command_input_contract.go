package main

type agentPerformanceSummaryCommandInput struct{}

type agentMemoryCommandInput struct {
	Search     string
	Agent      string
	StorePath  string
	DeleteID   string
	IndexFiles []string
	Limit      int
	TTLSeconds int
	Migrate    bool
	Compact    bool
}

type asyncPlanCommandInput struct {
	TaskSpecs []string
}

type asyncRunCommandInput struct {
	SpawnBinary    string
	TaskSpecs      []string
	Execution      childExecutionCommandInput
	TimeoutSeconds int
}

type childExecutionCommandInput struct {
	LedgerPath          string
	MaxConcurrency      int
	TaskTimeoutSeconds  int
	Retries             int
	RetryBackoffSeconds int
	TokenBudget         int
	CostBudgetMicros    int
	OutputBudgetBytes   int
	RetriesSet          bool
	CancelOnFailure     bool
	Resume              bool
}

type codeIntelCommandInput struct {
	ModelQuery                     string
	ModelLanguage                  string
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
	OutputFormat                   string
	Limit                          int
	Offset                         int
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
	JSON                           bool
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
	OutputFormat string
	SessionDir   string
	JSON         bool
}

type configMigrateCommandInput struct{}

type configReportCommandInput struct{}

type evalOutputCommandInput struct {
	ActualPath   string
	ExpectedText string
	ExpectedPath string
	Mode         string
}

type explainConfigCommandInput struct {
	FieldPath string
}

type explainModelResolutionCommandInput struct {
	Model string
}

type feedbackApproveCommandInput struct {
	ConfigPath  string
	HistoryPath string
	Agent       string
	ID          string
}

type feedbackProposalsCommandInput struct{}

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

//nolint:govet // Field order follows grouped incident source/report controls.
type incidentDiagnoseCommandInput struct {
	SentryIssue        string
	Reference          string
	FilePath           string
	SentryOrg          string
	SentryBaseURL      string
	SentryTokenEnv     string
	SentryEventID      string
	MCPManifestPath    string
	MCPServerName      string
	MCPToolName        string
	MCPToolArgsJSON    string
	ReproCommand       string
	ValidationCommands []string
	ReportPath         string
	PRBodyPath         string
	OutputFormat       string
	TimeoutSeconds     int
	JSON               bool
	ApplyFix           bool
	OpenPR             bool
}

//nolint:govet // Field order follows grouped incident source/report controls.
type incidentDiagnoseOnlyCommandInput struct {
	SentryIssue        string
	Reference          string
	FilePath           string
	SentryOrg          string
	SentryBaseURL      string
	SentryTokenEnv     string
	SentryEventID      string
	MCPManifestPath    string
	MCPServerName      string
	MCPToolName        string
	MCPToolArgsJSON    string
	ReproCommand       string
	ValidationCommands []string
	ReportPath         string
	PRBodyPath         string
	OutputFormat       string
	TimeoutSeconds     int
	JSON               bool
}

type initConfigCommandInput struct {
	Path string
}

type initRTKPluginCommandInput struct {
	Dir string
}

type issueImplementCommandInput struct {
	IssueRef        string
	WorkflowPath    string
	BaseBranch      string
	OpenPR          bool
	RunTests        bool
	RunLint         bool
	UpdateDocs      bool
	UpdateChangelog bool
}

type headlessCommandInput struct {
	StatusID     string
	CancelID     string
	RetryID      string
	RetryNewID   string
	StreamID     string
	StatusFilter string
	MaxAge       string
	Recover      bool
	List         bool
	Cleanup      bool
}

type listAgentsCommandInput struct{}

type listConfigPathsCommandInput struct{}

type listHookEventsCommandInput struct {
	JSON bool
}

type listKnownModelsCommandInput struct{}

type listModelsCommandInput struct{}

type listPluginsCommandInput struct{}

type listProvidersCommandInput struct{}

type ollamaStatusCommandInput struct{}

type ollamaStopCommandInput struct{}

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
	OutputFormat     string
	Args             []string
	DocumentSymbols  bool
	JSON             bool
}

type memoryCommandInput struct {
	Search                  string
	StorePath               string
	DeleteID                string
	IndexFiles              []string
	Limit                   int
	TTLSeconds              int
	IncludeSessionMessages  bool
	IncludeWorktreeMetadata bool
	Migrate                 bool
	Compact                 bool
}

type retrievalCommandInput struct {
	Search               string
	AgentName            string
	AgentMemoryAgent     string
	AgentMemoryStorePath string
	MemoryStorePath      string
	Filters              []string
	MemoryIndexFiles     []string
	Sources              []string
	VectorIndexFiles     []string
	Vector               retrievalVectorCommandInput
	Limit                int
	Explain              bool
	IncludeUnsafe        bool
}

type retrievalVectorCommandInput struct {
	Vectorizer        string
	Provider          string
	Model             string
	BaseURL           string
	FallbackPolicy    string
	StorePath         string
	TimeoutSeconds    int
	ChunkMaxRunes     int
	ChunkOverlapRunes int
	TimeoutSet        bool
	ChunkMaxSet       bool
	ChunkOverlapSet   bool
}

type mergeArtifactsCommandInput struct {
	OutputPath string
	Format     string
	MaxBytes   int
}

type mergeWorktreeCommandInput struct {
	Ref                  string
	VerificationCommands []string
	AllowBaseMismatch    bool
	OverrideVerification bool
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

type stateDiagnosticsCommandInput struct {
	SessionRef     string
	AgentName      string
	Model          string
	ModelMode      string
	ReasoningLevel string
}

type suggestSkillCommandInput struct {
	SaveDir            string
	LearningDir        string
	LearningSkillDir   string
	LearningShow       string
	LearningEdit       string
	LearningEnable     string
	LearningDisable    string
	LearningDelete     string
	Steps              []string
	MaxSteps           int
	MinOccurrences     int
	ReviewOnly         bool
	LearningList       bool
	LearningEnableAll  bool
	LearningDisableAll bool
}

type skillLearningCommandInput struct {
	EffectiveEnabled *bool
	Dir              string
	SkillDir         string
	Show             string
	Edit             string
	Editor           string
	Enable           string
	Disable          string
	Delete           string
	SuggestSteps     []string
	List             bool
	EnableAll        bool
	DisableAll       bool
}

type vectorSearchCommandInput struct {
	Query      string
	IndexFiles []string
	Vector     retrievalVectorCommandInput
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
		"configMigrateCommandInput":           func(opts cliOptions) any { return configMigrateCommandInputFromOptions(opts) },
		"configReportCommandInput":            func(opts cliOptions) any { return configReportCommandInputFromOptions(opts) },
		"contextPackCommandInput":             func(opts cliOptions) any { return contextPackCommandInputFromOptions(opts) },
		"describeAgentCommandInput":           func(opts cliOptions) any { return describeAgentCommandInputFromOptions(opts) },
		"describePluginCommandInput":          func(opts cliOptions) any { return describePluginCommandInputFromOptions(opts) },
		"doctorCommandInput":                  func(opts cliOptions) any { return doctorCommandInputFromOptions(opts) },
		"doctorOfflineCommandInput":           func(opts cliOptions) any { return doctorOfflineCommandInputFromOptions(opts) },
		"evalOutputCommandInput":              func(opts cliOptions) any { return evalOutputCommandInputFromOptions(opts) },
		"explainConfigCommandInput":           func(opts cliOptions) any { return explainConfigCommandInputFromOptions(opts) },
		"explainModelResolutionCommandInput":  func(opts cliOptions) any { return explainModelResolutionCommandInputFromOptions(opts) },
		"feedbackApproveCommandInput":         func(opts cliOptions) any { return feedbackApproveCommandInputFromOptions(opts) },
		"feedbackProposalsCommandInput":       func(opts cliOptions) any { return feedbackProposalsCommandInputFromOptions(opts) },
		"feedbackRollbackCommandInput":        func(opts cliOptions) any { return feedbackRollbackCommandInputFromOptions(opts) },
		"gitHistorySearchCommandInput":        func(opts cliOptions) any { return gitHistorySearchCommandInputFromOptions(opts) },
		"incidentDiagnoseCommandInput":        func(opts cliOptions) any { return incidentDiagnoseCommandInputFromOptions(opts) },
		"incidentDiagnoseOnlyCommandInput":    func(opts cliOptions) any { return incidentDiagnoseOnlyCommandInputFromOptions(opts) },
		"headlessCommandInput":                func(opts cliOptions) any { return headlessCommandInputFromOptions(opts) },
		"initConfigCommandInput":              func(opts cliOptions) any { return initConfigCommandInputFromOptions(opts) },
		"initRTKPluginCommandInput":           func(opts cliOptions) any { return initRTKPluginCommandInputFromOptions(opts) },
		"issueImplementCommandInput":          func(opts cliOptions) any { return issueImplementCommandInputFromOptions(opts) },
		"listAgentsCommandInput":              func(opts cliOptions) any { return listAgentsCommandInputFromOptions(opts) },
		"listConfigPathsCommandInput":         func(opts cliOptions) any { return listConfigPathsCommandInputFromOptions(opts) },
		"listHookEventsCommandInput":          func(opts cliOptions) any { return listHookEventsCommandInputFromOptions(opts) },
		"listKnownModelsCommandInput":         func(opts cliOptions) any { return listKnownModelsCommandInputFromOptions(opts) },
		"listModelsCommandInput":              func(opts cliOptions) any { return listModelsCommandInputFromOptions(opts) },
		"listPluginsCommandInput":             func(opts cliOptions) any { return listPluginsCommandInputFromOptions(opts) },
		"listProvidersCommandInput":           func(opts cliOptions) any { return listProvidersCommandInputFromOptions(opts) },
		"ollamaStatusCommandInput":            func(opts cliOptions) any { return ollamaStatusCommandInputFromOptions(opts) },
		"ollamaStopCommandInput":              func(opts cliOptions) any { return ollamaStopCommandInputFromOptions(opts) },
		"listSessionsCommandInput":            func(opts cliOptions) any { return listSessionsCommandInputFromOptions(opts) },
		"listSessionTagsCommandInput":         func(opts cliOptions) any { return listSessionTagsCommandInputFromOptions(opts) },
		"listWorktreesCommandInput":           func(opts cliOptions) any { return listWorktreesCommandInputFromOptions(opts) },
		"lspSymbolsCommandInput":              func(opts cliOptions) any { return lspSymbolsCommandInputFromOptions(opts) },
		"mcpInvokeCommandInput":               func(opts cliOptions) any { return mcpInvokeCommandInputFromOptions(opts) },
		"mcpManifestCommandInput":             func(opts cliOptions) any { return mcpManifestCommandInputFromOptions(opts) },
		"memoryCommandInput":                  func(opts cliOptions) any { return memoryCommandInputFromOptions(opts) },
		"retrievalCommandInput":               func(opts cliOptions) any { return retrievalCommandInputFromOptions(opts) },
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
		DeleteID:   opts.agentMemoryDelete,
		IndexFiles: append([]string(nil), opts.agentMemoryIndexFiles...),
		Limit:      opts.agentMemoryLimit.value,
		TTLSeconds: opts.agentMemoryTTL.value,
		Migrate:    opts.agentMemoryMigrate,
		Compact:    opts.agentMemoryCompact,
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
		Execution:      childExecutionCommandInputFromOptions(opts),
	}
}

func childExecutionCommandInputFromOptions(opts cliOptions) childExecutionCommandInput {
	return childExecutionCommandInput{
		LedgerPath:          opts.spawnLedgerPath,
		MaxConcurrency:      opts.spawnMaxConcurrency.value,
		TaskTimeoutSeconds:  opts.spawnTaskTimeout.value,
		Retries:             opts.spawnRetries.value,
		RetryBackoffSeconds: opts.spawnRetryBackoff.value,
		TokenBudget:         opts.spawnTokenBudget.value,
		CostBudgetMicros:    opts.spawnCostBudgetMicros.value,
		OutputBudgetBytes:   opts.spawnOutputBudgetBytes.value,
		RetriesSet:          opts.spawnRetries.set,
		CancelOnFailure:     opts.spawnCancelOnFailure,
		Resume:              opts.spawnResume,
	}
}

func codeIntelCommandInputFromOptions(opts cliOptions) codeIntelCommandInput {
	return codeIntelCommandInput{
		ModelQuery:                     opts.codeQuery,
		ModelLanguage:                  opts.codeLanguage,
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
		OutputFormat:                   opts.outputFormat,
		Limit:                          opts.codeLimit.value,
		Offset:                         opts.codeOffset.value,
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
		JSON:                           opts.jsonOutput,
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
	return doctorOfflineCommandInput{OutputFormat: opts.outputFormat, SessionDir: opts.sessionDir, JSON: opts.jsonOutput}
}

func configMigrateCommandInputFromOptions(_ cliOptions) configMigrateCommandInput {
	return configMigrateCommandInput{}
}

func configReportCommandInputFromOptions(_ cliOptions) configReportCommandInput {
	return configReportCommandInput{}
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

func feedbackApproveCommandInputFromOptions(opts cliOptions) feedbackApproveCommandInput {
	return feedbackApproveCommandInput{
		ConfigPath:  opts.feedbackApproveConfig,
		HistoryPath: opts.feedbackHistoryPath,
		Agent:       opts.feedbackApproveAgent,
		ID:          opts.feedbackApproveID,
	}
}

func feedbackProposalsCommandInputFromOptions(_ cliOptions) feedbackProposalsCommandInput {
	return feedbackProposalsCommandInput{}
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

func incidentDiagnoseCommandInputFromOptions(opts cliOptions) incidentDiagnoseCommandInput {
	return incidentDiagnoseCommandInput{
		SentryIssue:        opts.sentryIssue,
		Reference:          opts.incidentReference,
		FilePath:           opts.incidentFilePath,
		SentryOrg:          opts.incidentSentryOrg,
		SentryBaseURL:      opts.incidentSentryBaseURL,
		SentryTokenEnv:     opts.incidentSentryTokenEnv,
		SentryEventID:      opts.incidentSentryEventID,
		MCPManifestPath:    opts.incidentMCPManifestPath,
		MCPServerName:      opts.incidentMCPServerName,
		MCPToolName:        opts.incidentMCPToolName,
		MCPToolArgsJSON:    opts.incidentMCPToolArgsJSON,
		ReproCommand:       opts.incidentReproCommand,
		ValidationCommands: append([]string(nil), opts.incidentValidationCommands...),
		ReportPath:         opts.incidentReportPath,
		PRBodyPath:         opts.incidentPRBodyPath,
		OutputFormat:       opts.outputFormat,
		TimeoutSeconds:     opts.incidentTimeout.value,
		JSON:               opts.jsonOutput,
		ApplyFix:           opts.incidentApplyFix,
		OpenPR:             opts.incidentOpenPR,
	}
}

func incidentDiagnoseOnlyCommandInputFromOptions(opts cliOptions) incidentDiagnoseOnlyCommandInput {
	return incidentDiagnoseOnlyCommandInput{
		SentryIssue:        opts.sentryIssue,
		Reference:          opts.incidentReference,
		FilePath:           opts.incidentFilePath,
		SentryOrg:          opts.incidentSentryOrg,
		SentryBaseURL:      opts.incidentSentryBaseURL,
		SentryTokenEnv:     opts.incidentSentryTokenEnv,
		SentryEventID:      opts.incidentSentryEventID,
		MCPManifestPath:    opts.incidentMCPManifestPath,
		MCPServerName:      opts.incidentMCPServerName,
		MCPToolName:        opts.incidentMCPToolName,
		MCPToolArgsJSON:    opts.incidentMCPToolArgsJSON,
		ReproCommand:       opts.incidentReproCommand,
		ValidationCommands: append([]string(nil), opts.incidentValidationCommands...),
		ReportPath:         opts.incidentReportPath,
		PRBodyPath:         opts.incidentPRBodyPath,
		OutputFormat:       opts.outputFormat,
		TimeoutSeconds:     opts.incidentTimeout.value,
		JSON:               opts.jsonOutput,
	}
}

func headlessCommandInputFromOptions(opts cliOptions) headlessCommandInput {
	return headlessCommandInput{
		StatusID:     opts.statusHeadlessID,
		CancelID:     opts.cancelHeadlessID,
		RetryID:      opts.retryHeadlessID,
		RetryNewID:   opts.retryHeadlessNewID,
		StreamID:     opts.streamHeadlessID,
		StatusFilter: opts.headlessStatusFilter,
		MaxAge:       opts.headlessMaxAge,
		Recover:      opts.recoverHeadless,
		List:         opts.listHeadless,
		Cleanup:      opts.cleanupHeadless,
	}
}

func initConfigCommandInputFromOptions(opts cliOptions) initConfigCommandInput {
	return initConfigCommandInput{Path: opts.initConfigPath}
}

func initRTKPluginCommandInputFromOptions(opts cliOptions) initRTKPluginCommandInput {
	return initRTKPluginCommandInput{Dir: opts.initRTKPluginDir}
}

func issueImplementCommandInputFromOptions(opts cliOptions) issueImplementCommandInput {
	return issueImplementCommandInput{
		IssueRef:        opts.issueImplementRef,
		WorkflowPath:    opts.issueWorkflowPath,
		BaseBranch:      opts.issueBaseBranch,
		OpenPR:          opts.issueOpenPR,
		RunTests:        opts.issueRunTests,
		RunLint:         opts.issueRunLint,
		UpdateDocs:      opts.issueUpdateDocs,
		UpdateChangelog: opts.issueUpdateChangelog,
	}
}

func listAgentsCommandInputFromOptions(_ cliOptions) listAgentsCommandInput {
	return listAgentsCommandInput{}
}

func listConfigPathsCommandInputFromOptions(_ cliOptions) listConfigPathsCommandInput {
	return listConfigPathsCommandInput{}
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

func explainModelResolutionCommandInputFromOptions(opts cliOptions) explainModelResolutionCommandInput {
	return explainModelResolutionCommandInput{Model: opts.explainModelResolution}
}

func listPluginsCommandInputFromOptions(_ cliOptions) listPluginsCommandInput {
	return listPluginsCommandInput{}
}

func listProvidersCommandInputFromOptions(_ cliOptions) listProvidersCommandInput {
	return listProvidersCommandInput{}
}

func ollamaStatusCommandInputFromOptions(_ cliOptions) ollamaStatusCommandInput {
	return ollamaStatusCommandInput{}
}

func ollamaStopCommandInputFromOptions(_ cliOptions) ollamaStopCommandInput {
	return ollamaStopCommandInput{}
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
		OutputFormat:     opts.outputFormat,
		Args:             append([]string(nil), opts.lspArgs...),
		DocumentSymbols:  opts.lspSymbols,
		JSON:             opts.jsonOutput,
	}
}

func memoryCommandInputFromOptions(opts cliOptions) memoryCommandInput {
	return memoryCommandInput{
		Search:                  opts.memorySearch,
		StorePath:               opts.memoryStorePath,
		DeleteID:                opts.memoryDelete,
		IndexFiles:              append([]string(nil), opts.memoryIndexFiles...),
		Limit:                   opts.memoryLimit.value,
		TTLSeconds:              opts.memoryTTL.value,
		IncludeSessionMessages:  opts.memoryIncludeSessionMessages,
		IncludeWorktreeMetadata: opts.memoryIncludeWorktreeMetadata,
		Migrate:                 opts.memoryMigrate,
		Compact:                 opts.memoryCompact,
	}
}

func retrievalCommandInputFromOptions(opts cliOptions) retrievalCommandInput {
	return retrievalCommandInput{
		Search:               opts.retrievalSearch,
		AgentName:            opts.agentName,
		AgentMemoryAgent:     opts.agentMemoryAgent,
		AgentMemoryStorePath: opts.agentMemoryStorePath,
		MemoryStorePath:      opts.memoryStorePath,
		Vector:               retrievalVectorCommandInputFromOptions(opts),
		Filters:              append([]string(nil), opts.retrievalFilters...),
		MemoryIndexFiles:     append([]string(nil), opts.memoryIndexFiles...),
		Sources:              append([]string(nil), opts.retrievalSources...),
		VectorIndexFiles:     append([]string(nil), opts.vectorIndexFiles...),
		Limit:                opts.retrievalLimit.value,
		Explain:              opts.retrievalExplain,
		IncludeUnsafe:        opts.retrievalIncludeUnsafe,
	}
}

func retrievalVectorCommandInputFromOptions(opts cliOptions) retrievalVectorCommandInput {
	return retrievalVectorCommandInput{
		Vectorizer:        opts.vectorizer,
		Provider:          opts.vectorProvider,
		Model:             opts.vectorModel,
		BaseURL:           opts.vectorBaseURL,
		FallbackPolicy:    opts.vectorFallbackPolicy,
		StorePath:         opts.vectorStorePath,
		TimeoutSeconds:    opts.vectorTimeout.value,
		ChunkMaxRunes:     opts.vectorChunkMaxRunes.value,
		ChunkOverlapRunes: opts.vectorChunkOverlapRunes.value,
		TimeoutSet:        opts.vectorTimeout.set,
		ChunkMaxSet:       opts.vectorChunkMaxRunes.set,
		ChunkOverlapSet:   opts.vectorChunkOverlapRunes.set,
	}
}

func mergeArtifactsCommandInputFromOptions(opts cliOptions) mergeArtifactsCommandInput {
	return mergeArtifactsCommandInput{
		OutputPath: opts.mergeArtifactsPath,
		Format:     opts.mergeArtifactsFormat,
		MaxBytes:   opts.mergeArtifactMaxBytes.value,
	}
}

func mergeWorktreeCommandInputFromOptions(opts cliOptions) mergeWorktreeCommandInput {
	return mergeWorktreeCommandInput{
		Ref:                  opts.mergeWorktreeRef,
		VerificationCommands: append([]string(nil), opts.worktreeVerificationCommands...),
		AllowBaseMismatch:    opts.mergeWorktreeAllowBaseMismatch,
		OverrideVerification: opts.worktreeMergeOverride,
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

func stateDiagnosticsCommandInputFromOptions(opts cliOptions) stateDiagnosticsCommandInput {
	return stateDiagnosticsCommandInput{
		SessionRef:     opts.sessionRef,
		AgentName:      opts.agentName,
		Model:          opts.model,
		ModelMode:      opts.modelMode,
		ReasoningLevel: opts.reasoningLevel,
	}
}

func suggestSkillCommandInputFromOptions(opts cliOptions) suggestSkillCommandInput {
	return suggestSkillCommandInput{
		SaveDir:            opts.skillSaveDir,
		LearningDir:        opts.skillLearningDir,
		LearningSkillDir:   opts.skillLearningSkillDir,
		LearningShow:       opts.skillLearningShow,
		LearningEdit:       opts.skillLearningEdit,
		LearningEnable:     opts.skillLearningEnable,
		LearningDisable:    opts.skillLearningDisable,
		LearningDelete:     opts.skillLearningDelete,
		Steps:              append([]string(nil), opts.suggestSkillSteps...),
		MaxSteps:           opts.skillMaxSteps.value,
		MinOccurrences:     opts.skillMinOccurrences.value,
		ReviewOnly:         opts.skillReviewOnly,
		LearningList:       opts.skillLearningList,
		LearningEnableAll:  opts.skillLearningEnableAll,
		LearningDisableAll: opts.skillLearningDisableAll,
	}
}

func skillLearningCommandInputFromOptions(opts cliOptions) skillLearningCommandInput {
	return skillLearningCommandInput{
		Dir:          opts.skillLearningDir,
		SkillDir:     opts.skillLearningSkillDir,
		Show:         opts.skillLearningShow,
		Edit:         opts.skillLearningEdit,
		Enable:       opts.skillLearningEnable,
		Disable:      opts.skillLearningDisable,
		Delete:       opts.skillLearningDelete,
		SuggestSteps: append([]string(nil), opts.suggestSkillSteps...),
		List:         opts.skillLearningList,
		EnableAll:    opts.skillLearningEnableAll,
		DisableAll:   opts.skillLearningDisableAll,
	}
}

func validateConfigCommandInputFromOptions(_ cliOptions) validateConfigCommandInput {
	return validateConfigCommandInput{}
}

func vectorSearchCommandInputFromOptions(opts cliOptions) vectorSearchCommandInput {
	return vectorSearchCommandInput{
		Query:      opts.vectorSearch,
		IndexFiles: append([]string(nil), opts.vectorIndexFiles...),
		Vector:     retrievalVectorCommandInputFromOptions(opts),
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
