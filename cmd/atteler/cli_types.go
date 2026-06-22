package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/tommoulard/atteler/pkg/agent"
	"github.com/tommoulard/atteler/pkg/autonomy"
	appconfig "github.com/tommoulard/atteler/pkg/config"
	"github.com/tommoulard/atteler/pkg/contextref"
	"github.com/tommoulard/atteler/pkg/events"
	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/permission"
	attelerplugin "github.com/tommoulard/atteler/pkg/plugin"
	"github.com/tommoulard/atteler/pkg/session"
	"github.com/tommoulard/atteler/pkg/worktree"
)

type floatFlag struct {
	name   string
	value  float64
	min    float64
	max    float64
	set    bool
	hasMax bool
}

func (f *floatFlag) Set(raw string) error {
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return fmt.Errorf("parse %s: %w", f.name, err)
	}

	if value < f.min {
		return fmt.Errorf("%s must be >= %g", f.name, f.min)
	}

	if f.hasMax && value > f.max {
		return fmt.Errorf("%s must be <= %g", f.name, f.max)
	}

	f.value = value
	f.set = true

	return nil
}

func (f *floatFlag) String() string {
	if f == nil || !f.set {
		return ""
	}

	return strconv.FormatFloat(f.value, 'f', -1, 64)
}

type positiveIntFlag struct {
	name  string
	value int
	set   bool
}

func (f *positiveIntFlag) Set(raw string) error {
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fmt.Errorf("parse %s: %w", f.name, err)
	}

	if value <= 0 {
		return fmt.Errorf("%s must be > 0", f.name)
	}

	f.value = value
	f.set = true

	return nil
}

func (f *positiveIntFlag) String() string {
	if f == nil || !f.set {
		return ""
	}

	return strconv.Itoa(f.value)
}

type nonNegativeIntFlag struct {
	name  string
	value int
	set   bool
}

func (f *nonNegativeIntFlag) Set(raw string) error {
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fmt.Errorf("parse %s: %w", f.name, err)
	}

	if value < 0 {
		return fmt.Errorf("%s must be >= 0", f.name)
	}

	f.value = value
	f.set = true

	return nil
}

func (f *nonNegativeIntFlag) String() string {
	if f == nil || !f.set {
		return ""
	}

	return strconv.Itoa(f.value)
}

type stringListFlag []string

func (f *stringListFlag) Set(raw string) error {
	for value := range strings.SplitSeq(raw, ",") {
		value = strings.TrimSpace(value)
		if value != "" {
			*f = append(*f, value)
		}
	}

	return nil
}

func (f *stringListFlag) String() string {
	if f == nil {
		return ""
	}

	return strings.Join(*f, ",")
}

type rawStringListFlag []string

func (f *rawStringListFlag) Set(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw != "" {
		*f = append(*f, raw)
	}

	return nil
}

func (f *rawStringListFlag) String() string {
	if f == nil {
		return ""
	}

	return strings.Join(*f, ",")
}

type autonomyFlag struct {
	value autonomy.Level
	set   bool
}

func (f *autonomyFlag) Set(raw string) error {
	level, err := autonomy.Parse(raw)
	if err != nil {
		return fmt.Errorf("parse autonomy: %w", err)
	}

	f.value = level
	f.set = true

	return nil
}

func (f *autonomyFlag) String() string {
	if f == nil || !f.set {
		return ""
	}

	return f.value.String()
}

// autoFlag backs the --auto flag. It behaves as a boolean flag (bare --auto
// selects the default mode) but also accepts a mode name via --auto=<mode>.
type autoFlag struct {
	value string
	set   bool
}

func (f *autoFlag) Set(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.EqualFold(raw, "true") {
		raw = "auto"
	}

	f.value = raw
	f.set = true

	return nil
}

func (f *autoFlag) String() string {
	if f == nil || !f.set {
		return ""
	}

	return f.value
}

// IsBoolFlag lets `--auto` be used without a value while still accepting
// `--auto=<mode>`.
func (f *autoFlag) IsBoolFlag() bool { return true }

type generationSettings struct {
	Temperature    *float64
	TopP           *float64
	Seed           *int
	ModelMode      string
	ReasoningLevel string
	MaxTokens      int
}

//nolint:govet // field order follows CLI option grouping; padding is not performance-sensitive.
type cliOptions struct {
	oncePrompt                         string
	agentName                          string
	sessionDir                         string
	sessionRef                         string
	showSessionRef                     string
	summarySessionRef                  string
	replayRef                          string
	exportRef                          string
	exportFormat                       string
	showRunRef                         string
	exportRunRef                       string
	replayRunRef                       string
	resumeRunRef                       string
	outputFormat                       string
	listSessionsTag                    string
	headlessID                         string
	streamHeadlessID                   string
	statusHeadlessID                   string
	cancelHeadlessID                   string
	retryHeadlessID                    string
	retryHeadlessNewID                 string
	headlessStatusFilter               string
	headlessMaxAge                     string
	searchQuery                        string
	initConfigPath                     string
	explainConfigPath                  string
	configPaths                        string
	issueImplementRef                  string
	issueWorkflowPath                  string
	issueBaseBranch                    string
	issueWatchGitHub                   string
	issueWatchGitHubEndpoint           string
	issueWatchGitHubToken              string
	issueWatchRunRef                   string
	issueWatchCommand                  string
	issueImplementRequested            bool
	contextPackPath                    string
	model                              string
	explainModelResolution             string
	describeAgentName                  string
	codeQuery                          string
	codeLanguage                       string
	codeSymbolName                     string
	codeSymbolFileSummary              string
	codeSymbolPackageSummary           string
	codeSymbolPrefix                   string
	codeSymbolPrefixFileSummary        string
	codeSymbolPrefixPackageSummary     string
	codeSymbolKind                     string
	codeSymbolKindFileSummary          string
	codeSymbolKindPackageSummary       string
	codeImpactTarget                   string
	codeImportPath                     string
	codeImportPathSummary              string
	codeImportPathFileSummary          string
	codeImportPathPackageSummary       string
	codeImportPrefix                   string
	codeImportPrefixSummary            string
	codeImportPrefixFileSummary        string
	codeImportPrefixPackageSummary     string
	codeReachTarget                    string
	codeDepsTarget                     string
	codeRdepsTarget                    string
	codePackageName                    string
	codePackageImports                 string
	codePackageImportPath              string
	codePackageImportFiles             string
	codePackageImportPathFileSummary   string
	codePackageImportPrefix            string
	codePackageImportPrefixFiles       string
	codePackageImportPrefixFileSummary string
	codePackageImportFileSummary       string
	codePackageSymbols                 string
	codePackageSymbolFileSummary       string
	codePackageSymbolName              string
	codePackageSymbolNameFileSummary   string
	codePackageSymbolList              string
	codePackageSymbolKind              string
	codePackageSymbolKindFileSummary   string
	codePackageSymbolPrefix            string
	codePackageSymbolPrefixFileSummary string
	codeFilePath                       string
	codeFileImports                    string
	codeFileSymbols                    string
	codeFileSymbolSummary              string
	codeFileSymbolName                 string
	codeFileSymbolKind                 string
	codeFileSymbolPrefix               string
	codeFileImportPrefix               string
	codeFileImportPath                 string
	sessionTitle                       string
	mergeWorktreeRef                   string
	recordFailure                      string
	failureReason                      string
	failureCommit                      string
	failureTaskType                    string
	failureSeverity                    string
	recordEvaluation                   string
	evaluationOutcome                  string
	evaluationNotes                    string
	evaluationReference                string
	evaluationSource                   string
	evaluationEvaluator                string
	evaluationRubricVersion            string
	evaluationTaskType                 string
	evaluationDifficulty               string
	evaluationExpectedOutcome          string
	evaluationProvider                 string
	evaluationModel                    string
	evaluationFixtureVersion           string
	evaluationAgentVersion             string
	evaluationReportPath               string
	planAgentsPrompt                   string
	researchRunQuestion                string
	researchOutputDir                  string
	evalOutputPath                     string
	evalAssertionsPath                 string
	evalFixtureDir                     string
	evalReportPath                     string
	evalExpected                       string
	evalExpectedPath                   string
	evalMode                           string
	gitHistorySearch                   string
	gitHistoryIncludeHunks             bool
	gitHistoryAll                      bool
	gitHistoryFirstParent              bool
	gitHistoryNoMerges                 bool
	gitHistoryMergesOnly               bool
	gitHistoryRange                    string
	gitHistorySince                    string
	gitHistoryUntil                    string
	incidentFilePath                   string
	incidentMCPManifestPath            string
	incidentMCPServerName              string
	incidentMCPToolArgsJSON            string
	incidentMCPToolName                string
	incidentPRBodyPath                 string
	incidentReference                  string
	incidentReportPath                 string
	incidentReproCommand               string
	incidentSentryBaseURL              string
	incidentSentryEventID              string
	incidentSentryOrg                  string
	incidentSentryTokenEnv             string
	sentryIssue                        string
	describePluginName                 string
	runPluginTarget                    string
	pluginEntrypoint                   string
	initRTKPluginDir                   string
	bashCommand                        string
	bashDir                            string
	taskFilePath                       string
	taskAddTitle                       string
	taskAddID                          string
	taskAgent                          string
	taskAssignSpec                     string
	taskCompleteID                     string
	taskHeartbeatID                    string
	taskUpdateID                       string
	taskReviewID                       string
	taskFailID                         string
	taskCancelID                       string
	taskReopenID                       string
	taskTitle                          string
	taskMessage                        string
	taskReason                         string
	taskSessionID                      string
	taskRunID                          string
	taskRisk                           string
	taskBlockerReason                  string
	taskDependencies                   stringListFlag
	feedbackApproveAgent               string
	feedbackApproveConfig              string
	feedbackApproveID                  string
	feedbackApplyConfig                string
	feedbackRollbackAgent              string
	feedbackRollbackConfig             string
	feedbackRollbackID                 string
	feedbackRollbackReason             string
	feedbackHistoryPath                string
	agentMemoryAgent                   string
	agentMemoryDelete                  string
	agentMemorySearch                  string
	agentMemoryStorePath               string
	memoryDelete                       string
	memoryAgent                        string
	memoryRepoPath                     string
	memoryScope                        string
	memorySearch                       string
	memorySessionRef                   string
	memorySince                        string
	memoryStorePath                    string
	memoryPurgeSpec                    string
	retrievalSearch                    string
	memoryUntil                        string
	mcpManifestPath                    string
	mcpCapability                      string
	mcpServerName                      string
	mcpMethod                          string
	mcpParamsJSON                      string
	mcpToolName                        string
	mcpToolArgsJSON                    string
	lspCommand                         string
	lspFilePath                        string
	lspRootPath                        string
	lspLanguageID                      string
	lspWorkspaceSymbols                string
	helpDomain                         string
	spawnBinary                        string
	spawnLedgerPath                    string
	promptCompleteInput                string
	speculatePrompt                    string
	reviewPrompt                       string
	reviewFixFrom                      string
	reviewFixPR                        string
	skillSaveDir                       string
	skillLearningDir                   string
	skillLearningSkillDir              string
	skillLearningShow                  string
	skillLearningEdit                  string
	skillLearningEnable                string
	skillLearningDisable               string
	skillLearningDelete                string
	asyncTaskSpecs                     stringListFlag
	reviewAgents                       stringListFlag
	reviewPaths                        stringListFlag
	reviewGates                        stringListFlag
	reviewFixValidationCommands        rawStringListFlag
	vectorSearch                       string
	vectorizer                         string
	vectorProvider                     string
	vectorModel                        string
	vectorBaseURL                      string
	vectorFallbackPolicy               string
	vectorStorePath                    string
	mergeArtifactsPath                 string
	mergeArtifactsFormat               string
	recordArtifact                     string
	artifactKind                       string
	artifactLogicalPath                string
	artifactReviewStatus               string
	artifactSummary                    string
	recordResponsePath                 string
	replayResponsePath                 string
	permissionMode                     string
	watchBaselinePath                  string
	watchBaselineRef                   string
	watchRulesPath                     string
	watchSuppressionsPath              string
	watchGateMinSeverity               string
	watchIssueMinSeverity              string
	watchIssueRepository               string
	watchGitHubEndpoint                string
	watchGitHubToken                   string
	sessionTags                        stringListFlag
	issueWatchLabels                   stringListFlag
	issueWatchValidationCommands       rawStringListFlag
	watchIssueLabels                   stringListFlag
	agentMemoryIndexFiles              stringListFlag
	memoryRedactRules                  rawStringListFlag
	worktreeVerificationCommands       rawStringListFlag
	incidentValidationCommands         rawStringListFlag
	memoryTags                         stringListFlag
	planAgentNames                     stringListFlag
	trustedSources                     stringListFlag
	deniedSources                      stringListFlag
	researchSources                    stringListFlag
	retrievalFilters                   stringListFlag
	retrievalSources                   stringListFlag
	suggestSkillSteps                  stringListFlag
	routeCandidates                    rawStringListFlag
	routeRequiredCapabilities          stringListFlag
	lspArgs                            rawStringListFlag
	spawnAgentSpecs                    rawStringListFlag
	speculateAgents                    stringListFlag
	speculateGates                     stringListFlag
	allowOperations                    stringListFlag
	denyOperations                     stringListFlag
	memoryIndexFiles                   stringListFlag
	vectorIndexFiles                   stringListFlag
	maxTokens                          positiveIntFlag
	maxInputTokens                     positiveIntFlag
	contextPackTokens                  positiveIntFlag
	planMaxAgents                      positiveIntFlag
	memoryLimit                        positiveIntFlag
	memoryTTL                          positiveIntFlag
	memoryRetentionDays                positiveIntFlag
	agentMemoryLimit                   positiveIntFlag
	agentMemoryTTL                     positiveIntFlag
	retrievalLimit                     positiveIntFlag
	vectorLimit                        positiveIntFlag
	codeLimit                          positiveIntFlag
	vectorTimeout                      positiveIntFlag
	vectorChunkMaxRunes                positiveIntFlag
	vectorChunkOverlapRunes            positiveIntFlag
	mergeArtifactMaxBytes              positiveIntFlag
	routeInputTokens                   positiveIntFlag
	routeOutputTokens                  positiveIntFlag
	routeCacheWriteTokens              positiveIntFlag
	gitHistoryLimit                    positiveIntFlag
	gitHistoryMaxHunkBytes             positiveIntFlag
	gitHistoryAuthors                  stringListFlag
	gitHistoryPaths                    stringListFlag
	gitHistoryRefs                     stringListFlag
	incidentTimeout                    positiveIntFlag
	pluginTimeout                      positiveIntFlag
	bashTimeout                        positiveIntFlag
	mcpTimeout                         positiveIntFlag
	spawnTimeout                       positiveIntFlag
	spawnTaskTimeout                   positiveIntFlag
	spawnMaxConcurrency                positiveIntFlag
	spawnTokenBudget                   positiveIntFlag
	spawnCostBudgetMicros              positiveIntFlag
	spawnOutputBudgetBytes             positiveIntFlag
	spawnRetryBackoff                  positiveIntFlag
	promptCompleteLimit                positiveIntFlag
	watchLargeFileBytes                positiveIntFlag
	watchIntervalSeconds               positiveIntFlag
	issueWatchIntervalSeconds          positiveIntFlag
	issueWatchCommandTimeout           positiveIntFlag
	watchMaxIterations                 positiveIntFlag
	skillMaxSteps                      positiveIntFlag
	skillMinOccurrences                positiveIntFlag
	evaluationScore                    nonNegativeIntFlag
	evalExitCode                       nonNegativeIntFlag
	evaluationDurationMillis           nonNegativeIntFlag
	evaluationFlakeCount               nonNegativeIntFlag
	evaluationInputTokens              nonNegativeIntFlag
	evaluationOutputTokens             nonNegativeIntFlag
	evaluationTotalTokens              nonNegativeIntFlag
	codeOffset                         nonNegativeIntFlag
	spawnRetries                       nonNegativeIntFlag
	seed                               nonNegativeIntFlag
	modelMode                          string
	autonomy                           autonomyFlag
	auto                               autoFlag
	autoMaxDepth                       int
	reasoningLevel                     string
	temperature                        floatFlag
	evaluationCost                     floatFlag
	evaluationConfidence               floatFlag
	evaluationPassRate                 floatFlag
	taskExpectedRevision               positiveIntFlag
	taskLeaseSeconds                   positiveIntFlag
	taskPriority                       nonNegativeIntFlag
	routeBudget                        floatFlag
	routeCacheReuse                    floatFlag
	topP                               floatFlag
	listModels                         bool
	listKnownModels                    bool
	listProviders                      bool
	ollamaStatus                       bool
	ollamaStop                         bool
	taskList                           bool
	taskReconcile                      bool
	taskRepair                         bool
	taskClearBlocker                   bool
	taskClearDependencies              bool
	taskClearRisk                      bool
	speculatePlan                      bool
	speculateRun                       bool
	skillReviewOnly                    bool
	skillLearningList                  bool
	skillLearningEnableAll             bool
	skillLearningDisableAll            bool
	reviewPlan                         bool
	reviewRun                          bool
	reviewFix                          bool
	routeInteractive                   bool
	routeBatch                         bool
	listAgents                         bool
	listCodeImports                    bool
	listCodeImportSummary              bool
	listCodeImportFileSummary          bool
	listCodeLayers                     bool
	listCodeCycles                     bool
	codeSummary                        bool
	listCodeFiles                      bool
	listCodeSymbolSummary              bool
	listCodeSymbolFileSummary          bool
	listCodePackageImportSummary       bool
	listCodePackages                   bool
	jsonOutput                         bool
	listSessions                       bool
	listHeadless                       bool
	recoverHeadless                    bool
	cleanupHeadless                    bool
	listSessionTags                    bool
	agentPerformanceSummary            bool
	listArtifacts                      bool
	listEvaluations                    bool
	listFailures                       bool
	listMessages                       bool
	listRuns                           bool
	listConfigPaths                    bool
	stateDiagnostics                   bool
	commandSurfaceJSON                 bool
	commandSurfaceDocs                 bool
	evalJSON                           bool
	evalUpdateGolden                   bool
	evalApproveGoldenUpdate            bool
	listPlugins                        bool
	listHookEvents                     bool
	listHookEventsJSON                 bool
	watchScan                          bool
	watchJSON                          bool
	watchLoop                          bool
	watchGate                          bool
	watchIssueUpsert                   bool
	incidentApplyFix                   bool
	incidentDiagnose                   bool
	incidentOpenPR                     bool
	reviewScan                         bool
	lspSymbols                         bool
	asyncPlan                          bool
	asyncRun                           bool
	spawnDryRun                        bool
	spawnResume                        bool
	spawnCancelOnFailure               bool
	feedbackProposals                  bool
	configMigrate                      bool
	configReport                       bool
	validateConfig                     bool
	issueOpenPR                        bool
	issueRunTests                      bool
	issueRunLint                       bool
	issueUpdateDocs                    bool
	issueUpdateChangelog               bool
	issueWatch                         bool
	issueWatchOnce                     bool
	issueWatchDryRun                   bool
	explainConfig                      bool
	memoryCompact                      bool
	memoryIncludeSessionMessages       bool
	memoryIncludeWorktreeMetadata      bool
	memoryMigrate                      bool
	agentMemoryCompact                 bool
	agentMemoryMigrate                 bool
	printConfigTemplate                bool
	doctor                             bool
	doctorOffline                      bool
	retrievalExplain                   bool
	retrievalIncludeUnsafe             bool
	helpRequested                      bool
	readStdin                          bool
	promptLocalOnly                    bool
	headless                           bool
	headlessPrivateLog                 bool
	autoresearch                       bool
	researchGenerateTasks              bool
	warnLowTrustSources                bool
	showVersion                        bool
	useWorktree                        bool
	worktreeAutoMerge                  bool
	worktreeMergeOverride              bool
	mergeWorktreeAllowBaseMismatch     bool
	memoryGlobal                       bool
	memoryListCorpus                   bool
	memoryRebuild                      bool
	pluginDryRun                       bool
	listWorktrees                      bool
	noAutoMerge                        bool
	parseErr                           error
}

//nolint:govet // field order follows app state grouping; padding is not performance-sensitive.
type appState struct {
	config                       appconfig.Config
	sessionState                 session.Session
	contextOptions               contextref.Options
	generationDefaults           generationSettings
	generationOverrides          generationSettings
	agentLoopBudget              llm.AgentLoopBudget
	agentLoopCheckpointInterval  int
	autonomy                     autonomy.Level
	hookConfig                   map[string][]appconfig.HookConfig
	vectorConfig                 appconfig.VectorConfig
	agentRegistry                *agent.Registry
	hookRunner                   *events.Runner
	eventObservers               []events.Observer
	sessionStore                 *session.Store
	stateStore                   *appconfig.StateStore
	registry                     *llm.Registry
	providerReadiness            llm.ProviderReadinessReport
	worktreeInfo                 *worktree.Info
	pluginPolicy                 *attelerplugin.Policy
	permissionPolicy             *permission.Policy
	promptContextCache           *promptContextCache
	fallbackModels               []string
	pluginPaths                  []string
	providers                    []string
	loadedConfigPaths            []string
	configuredReferences         []string
	referenceManifest            contextref.ReferenceManifest
	referenceContext             string
	referenceContextEstimator    string
	skillLearningStoreDir        string
	skillLearningSkillDir        string
	selectedModel                string
	selectedAgent                string
	promptSuggestionConsent      promptSuggestionConsent
	configLoadErr                error
	idleSuggestionBudget         idleSuggestionBudget
	cwd                          string
	maxInputTokens               int
	modelLocked                  bool
	autoMergeWorktree            bool
	worktreeMergeOverride        bool
	promptLocalOnly              bool
	skillLearningEnabled         bool
	worktreeVerificationCommands []string
}
