package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/tommoulard/atteler/pkg/agent"
	appconfig "github.com/tommoulard/atteler/pkg/config"
	"github.com/tommoulard/atteler/pkg/contextref"
	"github.com/tommoulard/atteler/pkg/events"
	"github.com/tommoulard/atteler/pkg/llm"
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

type generationSettings struct {
	Temperature    *float64
	TopP           *float64
	Seed           *int
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
	outputFormat                       string
	listSessionsTag                    string
	headlessID                         string
	streamHeadlessID                   string
	statusHeadlessID                   string
	cancelHeadlessID                   string
	searchQuery                        string
	initConfigPath                     string
	explainConfigPath                  string
	configPaths                        string
	contextPackPath                    string
	model                              string
	describeAgentName                  string
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
	evaluationModel                    string
	evaluationAgentVersion             string
	planAgentsPrompt                   string
	evalOutputPath                     string
	evalAssertionsPath                 string
	evalFixtureDir                     string
	evalReportPath                     string
	evalExpected                       string
	evalExpectedPath                   string
	evalMode                           string
	gitHistorySearch                   string
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
	memorySearch                       string
	memoryStorePath                    string
	retrievalSearch                    string
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
	vectorSearch                       string
	mergeArtifactsPath                 string
	recordArtifact                     string
	artifactKind                       string
	artifactSummary                    string
	recordResponsePath                 string
	replayResponsePath                 string
	sessionTags                        stringListFlag
	agentMemoryIndexFiles              stringListFlag
	planAgentNames                     stringListFlag
	retrievalFilters                   stringListFlag
	retrievalSources                   stringListFlag
	suggestSkillSteps                  stringListFlag
	routeCandidates                    rawStringListFlag
	lspArgs                            rawStringListFlag
	spawnAgentSpecs                    rawStringListFlag
	speculateAgents                    stringListFlag
	speculateGates                     stringListFlag
	memoryIndexFiles                   stringListFlag
	vectorIndexFiles                   stringListFlag
	maxTokens                          positiveIntFlag
	maxInputTokens                     positiveIntFlag
	contextPackTokens                  positiveIntFlag
	planMaxAgents                      positiveIntFlag
	memoryLimit                        positiveIntFlag
	memoryTTL                          positiveIntFlag
	agentMemoryLimit                   positiveIntFlag
	agentMemoryTTL                     positiveIntFlag
	retrievalLimit                     positiveIntFlag
	vectorLimit                        positiveIntFlag
	mergeArtifactMaxBytes              positiveIntFlag
	routeInputTokens                   positiveIntFlag
	routeOutputTokens                  positiveIntFlag
	gitHistoryLimit                    positiveIntFlag
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
	watchMaxIterations                 positiveIntFlag
	skillMaxSteps                      positiveIntFlag
	skillMinOccurrences                positiveIntFlag
	evaluationScore                    nonNegativeIntFlag
	evalExitCode                       nonNegativeIntFlag
	evaluationDurationMillis           nonNegativeIntFlag
	spawnRetries                       nonNegativeIntFlag
	seed                               nonNegativeIntFlag
	reasoningLevel                     string
	temperature                        floatFlag
	evaluationCost                     floatFlag
	evaluationConfidence               floatFlag
	routeBudget                        floatFlag
	routeCacheReuse                    floatFlag
	topP                               floatFlag
	listModels                         bool
	listKnownModels                    bool
	listProviders                      bool
	taskList                           bool
	speculatePlan                      bool
	speculateRun                       bool
	skillReviewOnly                    bool
	skillLearningList                  bool
	skillLearningEnableAll             bool
	skillLearningDisableAll            bool
	reviewPlan                         bool
	reviewRun                          bool
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
	listSessions                       bool
	listHeadless                       bool
	recoverHeadless                    bool
	listSessionTags                    bool
	agentPerformanceSummary            bool
	listArtifacts                      bool
	listEvaluations                    bool
	listFailures                       bool
	listMessages                       bool
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
	reviewScan                         bool
	lspSymbols                         bool
	asyncPlan                          bool
	asyncRun                           bool
	spawnDryRun                        bool
	spawnResume                        bool
	spawnCancelOnFailure               bool
	feedbackProposals                  bool
	validateConfig                     bool
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
	showVersion                        bool
	useWorktree                        bool
	mergeWorktreeAllowBaseMismatch     bool
	pluginDryRun                       bool
	listWorktrees                      bool
	noAutoMerge                        bool
	parseErr                           error
}

//nolint:govet // field order follows app state grouping; padding is not performance-sensitive.
type appState struct {
	config                      appconfig.Config
	sessionState                session.Session
	contextOptions              contextref.Options
	generationDefaults          generationSettings
	generationOverrides         generationSettings
	agentLoopBudget             llm.AgentLoopBudget
	agentLoopCheckpointInterval int
	hookConfig                  map[string][]appconfig.HookConfig
	agentRegistry               *agent.Registry
	hookRunner                  *events.Runner
	eventObservers              []events.Observer
	sessionStore                *session.Store
	stateStore                  *appconfig.StateStore
	registry                    *llm.Registry
	worktreeInfo                *worktree.Info
	pluginPolicy                *attelerplugin.Policy
	fallbackModels              []string
	pluginPaths                 []string
	providers                   []string
	loadedConfigPaths           []string
	referenceContext            string
	skillLearningStoreDir       string
	skillLearningSkillDir       string
	selectedModel               string
	selectedAgent               string
	cwd                         string
	maxInputTokens              int
	modelLocked                 bool
	autoMergeWorktree           bool
	promptLocalOnly             bool
	skillLearningEnabled        bool
}
