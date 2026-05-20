package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/tommoulard/atteler/pkg/agent"
	appconfig "github.com/tommoulard/atteler/pkg/config"
	"github.com/tommoulard/atteler/pkg/contextref"
	atteval "github.com/tommoulard/atteler/pkg/eval"
	"github.com/tommoulard/atteler/pkg/events"
	"github.com/tommoulard/atteler/pkg/llm"
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
	streamHeadlessID                   string
	searchQuery                        string
	initConfigPath                     string
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
	recordEvaluation                   string
	evaluationOutcome                  string
	evaluationNotes                    string
	evaluationReference                string
	planAgentsPrompt                   string
	evalOutputPath                     string
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
	feedbackApplyConfig                string
	feedbackHistoryPath                string
	agentMemoryAgent                   string
	agentMemorySearch                  string
	agentMemoryStorePath               string
	memorySearch                       string
	memoryStorePath                    string
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
	spawnBinary                        string
	promptCompleteInput                string
	speculatePrompt                    string
	skillSaveDir                       string
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
	agentMemoryLimit                   positiveIntFlag
	vectorLimit                        positiveIntFlag
	mergeArtifactMaxBytes              positiveIntFlag
	routeInputTokens                   positiveIntFlag
	routeOutputTokens                  positiveIntFlag
	gitHistoryLimit                    positiveIntFlag
	pluginTimeout                      positiveIntFlag
	bashTimeout                        positiveIntFlag
	mcpTimeout                         positiveIntFlag
	spawnTimeout                       positiveIntFlag
	promptCompleteLimit                positiveIntFlag
	watchLargeFileBytes                positiveIntFlag
	watchIntervalSeconds               positiveIntFlag
	watchMaxIterations                 positiveIntFlag
	skillMaxSteps                      positiveIntFlag
	skillMinOccurrences                positiveIntFlag
	evaluationScore                    nonNegativeIntFlag
	seed                               nonNegativeIntFlag
	reasoningLevel                     string
	temperature                        floatFlag
	routeBudget                        floatFlag
	routeCacheReuse                    floatFlag
	topP                               floatFlag
	listModels                         bool
	listKnownModels                    bool
	listProviders                      bool
	taskList                           bool
	speculatePlan                      bool
	speculateRun                       bool
	reviewPlan                         bool
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
	listSessionTags                    bool
	agentPerformanceSummary            bool
	listArtifacts                      bool
	listEvaluations                    bool
	listFailures                       bool
	listMessages                       bool
	listConfigPaths                    bool
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
	feedbackProposals                  bool
	validateConfig                     bool
	printConfigTemplate                bool
	doctor                             bool
	doctorOffline                      bool
	readStdin                          bool
	headless                           bool
	showVersion                        bool
	useWorktree                        bool
	pluginDryRun                       bool
	listWorktrees                      bool
	noAutoMerge                        bool
}

//nolint:govet // field order follows app state grouping; padding is not performance-sensitive.
type appState struct {
	sessionState        session.Session
	contextOptions      contextref.Options
	generationDefaults  generationSettings
	generationOverrides generationSettings
	hookConfig          map[string][]appconfig.HookConfig
	agentRegistry       *agent.Registry
	hookRunner          *events.Runner
	sessionStore        *session.Store
	stateStore          *appconfig.StateStore
	registry            *llm.Registry
	worktreeInfo        *worktree.Info
	fallbackModels      []string
	pluginPaths         []string
	providers           []string
	loadedConfigPaths   []string
	referenceContext    string
	selectedModel       string
	selectedAgent       string
	cwd                 string
	maxInputTokens      int
	modelLocked         bool
	autoMergeWorktree   bool
}

func parseOptions() cliOptions {
	var opts cliOptions

	opts.temperature = floatFlag{name: "temperature", min: 0}
	opts.topP = floatFlag{name: "top-p", min: 0, max: 1, hasMax: true}
	opts.routeBudget = floatFlag{name: "route-budget", min: 0}
	opts.routeCacheReuse = floatFlag{name: "route-cache-reuse", min: 0, max: 1, hasMax: true}
	opts.maxTokens = positiveIntFlag{name: "max-tokens"}
	opts.maxInputTokens = positiveIntFlag{name: "max-input-tokens"}
	opts.seed = nonNegativeIntFlag{name: "seed"}
	opts.mcpTimeout = positiveIntFlag{name: "mcp-timeout-seconds"}
	opts.spawnTimeout = positiveIntFlag{name: "spawn-timeout-seconds"}
	flag.StringVar(&opts.configPaths, "config", "", "additional YAML/JSON config file path(s); same format as ATTELER_CONFIG")
	flag.StringVar(&opts.contextPackPath, "context-pack-file", "", "compact a role-prefixed transcript file and exit")
	flag.StringVar(&opts.initConfigPath, "init-config", "", "write a starter YAML config to this path without overwriting")
	flag.StringVar(&opts.sessionDir, "session-dir", "", "directory for session JSON files")
	flag.StringVar(&opts.sessionRef, "session", "", "session ID or path to continue")
	flag.StringVar(&opts.sessionRef, "session-id", "", "alias for --session")
	flag.StringVar(&opts.showSessionRef, "show-session", "", "print saved session details as YAML and exit")
	flag.StringVar(&opts.summarySessionRef, "session-summary", "", "print compact saved session metadata and counts and exit")
	flag.StringVar(&opts.sessionTitle, "session-title", "", "set or update the saved session title")
	flag.Var(&opts.sessionTags, "session-tag", "add a saved session tag (repeatable or comma-separated)")
	flag.StringVar(&opts.replayRef, "replay", "", "session ID or path to print and exit")
	flag.StringVar(&opts.exportRef, "export-session", "", "session ID or path to export and exit")
	flag.StringVar(&opts.exportFormat, "export-format", "markdown", "session export format: markdown or json")
	flag.StringVar(&opts.outputFormat, "output", outputFormatText, "one-shot output format: text or json")
	flag.StringVar(&opts.searchQuery, "search-sessions", "", "search saved session transcripts and exit")
	flag.StringVar(&opts.oncePrompt, "once", "", "send one prompt and exit")
	flag.StringVar(&opts.model, "model", "", "model ID to use")
	flag.StringVar(&opts.agentName, "agent", "", "agent name to use for prompts")
	flag.StringVar(&opts.describeAgentName, "describe-agent", "", "print a configured agent as YAML and exit")
	flag.StringVar(&opts.codeSymbolName, "code-symbol", "", "find Go symbols by exact name in the current repository and exit")
	flag.StringVar(&opts.codeSymbolFileSummary, "code-symbol-name-file-summary", "", "list Go files with symbol counts for this exact name and exit")
	flag.StringVar(&opts.codeSymbolPackageSummary, "code-symbol-name-package-summary", "", "list Go packages with file and symbol counts for this exact name and exit")
	flag.StringVar(&opts.codeSymbolPrefix, "code-symbol-prefix", "", "find Go symbols by name prefix in the current repository and exit")
	flag.StringVar(&opts.codeSymbolPrefixFileSummary, "code-symbol-prefix-file-summary", "", "list Go files with symbol counts for names matching this prefix and exit")
	flag.StringVar(&opts.codeSymbolPrefixPackageSummary, "code-symbol-prefix-package-summary", "", "list Go packages with file and symbol counts for names matching this prefix and exit")
	flag.StringVar(&opts.codeSymbolKind, "code-symbol-kind", "", "list Go symbols by kind (func, method, type, const, var) and exit")
	flag.StringVar(&opts.codeSymbolKindFileSummary, "code-symbol-kind-file-summary", "", "list Go files with symbol counts for one kind and exit")
	flag.StringVar(&opts.codeSymbolKindPackageSummary, "code-symbol-kind-package-summary", "", "list Go packages with file and symbol counts for one kind and exit")
	flag.StringVar(&opts.codeImpactTarget, "code-impact", "", "list Go files that directly or transitively import this path and exit")
	flag.StringVar(&opts.codeImportPath, "code-import-path", "", "list Go files that directly import this import path and exit")
	flag.StringVar(&opts.codeImportPathSummary, "code-import-path-summary", "", "summarize file usage count for one exact import path and exit")
	flag.StringVar(&opts.codeImportPathFileSummary, "code-import-path-file-summary", "", "list Go files with import counts for one exact import path and exit")
	flag.StringVar(&opts.codeImportPathPackageSummary, "code-import-path-package-summary", "", "list Go packages with file counts for one exact import path and exit")
	flag.StringVar(&opts.codeImportPrefix, "code-import-prefix", "", "list Go files that directly import paths with this prefix and exit")
	flag.StringVar(&opts.codeImportPrefixSummary, "code-import-prefix-summary", "", "summarize import usage counts for paths with this prefix and exit")
	flag.StringVar(&opts.codeImportPrefixFileSummary, "code-import-prefix-file-summary", "", "list Go files with import counts for paths matching this prefix and exit")
	flag.StringVar(&opts.codeImportPrefixPackageSummary, "code-import-prefix-package-summary", "", "list Go packages with file and import counts for paths matching this prefix and exit")
	flag.StringVar(&opts.codeReachTarget, "code-reachable", "", "list Go import graph nodes reachable from this file path or import path and exit")
	flag.StringVar(&opts.codeDepsTarget, "code-deps", "", "list direct Go import graph dependencies for this file path or import path and exit")
	flag.StringVar(&opts.codeRdepsTarget, "code-rdeps", "", "list direct Go import graph reverse dependencies for this file path or import path and exit")
	flag.StringVar(&opts.codePackageName, "code-package", "", "list Go files and symbol counts for one package and exit")
	flag.StringVar(&opts.codePackageImports, "code-package-imports", "", "list import usage counts for one Go package and exit")
	flag.StringVar(&opts.codePackageImportPath, "code-package-import-path", "", "list exact import usage for one package as package:import and exit")
	flag.StringVar(&opts.codePackageImportFiles, "code-package-import-files", "", "list files in one package importing an exact path as package:import and exit")
	flag.StringVar(&opts.codePackageImportPathFileSummary, "code-package-import-path-file-summary", "", "list files in one package with import counts for package:import and exit")
	flag.StringVar(&opts.codePackageImportPrefix, "code-package-import-prefix", "", "list import usage for one package and import prefix as package:prefix and exit")
	flag.StringVar(&opts.codePackageImportPrefixFiles, "code-package-import-prefix-files", "", "list files in one package importing paths with prefix as package:prefix and exit")
	flag.StringVar(&opts.codePackageImportPrefixFileSummary, "code-package-import-prefix-file-summary", "", "list files in one package with import counts for paths matching package:prefix and exit")
	flag.StringVar(&opts.codePackageImportFileSummary, "code-package-import-file-summary", "", "list files in one Go package with import counts and exit")
	flag.StringVar(&opts.codePackageSymbols, "code-package-symbols", "", "list symbol kind counts for one Go package and exit")
	flag.StringVar(&opts.codePackageSymbolFileSummary, "code-package-symbol-file-summary", "", "list files in one Go package with symbol counts and exit")
	flag.StringVar(&opts.codePackageSymbolName, "code-package-symbol", "", "list Go symbols for one package and exact name as package:name and exit")
	flag.StringVar(&opts.codePackageSymbolNameFileSummary, "code-package-symbol-name-file-summary", "", "list files in one Go package with symbol counts for package:name and exit")
	flag.StringVar(&opts.codePackageSymbolList, "code-package-symbol-list", "", "list Go symbols declared in one package and exit")
	flag.StringVar(&opts.codePackageSymbolKind, "code-package-symbol-kind", "", "list Go symbols for one package and kind as package:kind and exit")
	flag.StringVar(&opts.codePackageSymbolKindFileSummary, "code-package-symbol-kind-file-summary", "", "list files in one Go package with symbol counts for package:kind and exit")
	flag.StringVar(&opts.codePackageSymbolPrefix, "code-package-symbol-prefix", "", "list Go symbols for one package and name prefix as package:prefix and exit")
	flag.StringVar(&opts.codePackageSymbolPrefixFileSummary, "code-package-symbol-prefix-file-summary", "", "list files in one Go package with symbol counts for package:prefix and exit")
	flag.StringVar(&opts.codeFilePath, "code-file", "", "print Go package, symbols, and imports for one file and exit")
	flag.StringVar(&opts.codeFileImports, "code-file-imports", "", "list imports for one Go file and exit")
	flag.StringVar(&opts.codeFileSymbols, "code-file-symbols", "", "list symbols for one Go file and exit")
	flag.StringVar(&opts.codeFileSymbolSummary, "code-file-symbol-summary", "", "list symbol kind counts for one Go file and exit")
	flag.StringVar(&opts.codeFileSymbolName, "code-file-symbol", "", "list Go symbols for one file and exact name as path:name and exit")
	flag.StringVar(&opts.codeFileSymbolKind, "code-file-symbol-kind", "", "list Go symbols for one file and kind as path:kind and exit")
	flag.StringVar(&opts.codeFileSymbolPrefix, "code-file-symbol-prefix", "", "list Go symbols for one file and name prefix as path:prefix and exit")
	flag.StringVar(&opts.codeFileImportPrefix, "code-file-import-prefix", "", "list imports for one Go file matching path:prefix and exit")
	flag.StringVar(&opts.codeFileImportPath, "code-file-import-path", "", "check/list one Go file import as path:import and exit")
	flag.StringVar(&opts.recordFailure, "record-failure", "", "record a failed approach/negative-knowledge note on the selected session and exit")
	flag.StringVar(&opts.failureReason, "failure-reason", "", "reason for --record-failure")
	flag.StringVar(&opts.failureCommit, "failure-commit", "", "commit or reference associated with --record-failure")
	flag.StringVar(&opts.recordEvaluation, "record-evaluation", "", "record an evaluation for this agent on the selected session and exit")
	flag.StringVar(&opts.evaluationOutcome, "evaluation-outcome", "", "outcome for --record-evaluation")
	flag.StringVar(&opts.evaluationNotes, "evaluation-notes", "", "notes for --record-evaluation")
	flag.StringVar(&opts.evaluationReference, "evaluation-reference", "", "reference for --record-evaluation")
	flag.StringVar(&opts.planAgentsPrompt, "plan-agents", "", "plan configured agents for this prompt and exit")
	flag.Var(&opts.planAgentNames, "plan-agent", "explicit agent name to include in --plan-agents (repeatable or comma-separated)")
	flag.Var(&opts.planMaxAgents, "plan-max-agents", "maximum agents to include in --plan-agents")
	flag.StringVar(&opts.evalOutputPath, "eval-output", "", "actual output file to compare and exit")
	flag.StringVar(&opts.evalExpected, "eval-expected", "", "expected text for --eval-output")
	flag.StringVar(&opts.evalExpectedPath, "eval-expected-file", "", "expected output file for --eval-output")
	flag.StringVar(&opts.evalMode, "eval-mode", string(atteval.ModeContains), "eval mode: exact, contains, or normalized")
	flag.StringVar(&opts.gitHistorySearch, "git-history-search", "", "search local git history subjects/files/authors and exit")
	flag.Var(&opts.gitHistoryLimit, "git-history-limit", "maximum --git-history-search results")
	flag.StringVar(&opts.describePluginName, "describe-plugin", "", "print a configured plugin manifest as YAML and exit")
	flag.StringVar(&opts.runPluginTarget, "run-plugin", "", "run configured plugin name, or plugin/entrypoint when --plugin-entrypoint is omitted")
	flag.StringVar(&opts.pluginEntrypoint, "plugin-entrypoint", "", "entrypoint name for --run-plugin")
	flag.StringVar(&opts.initRTKPluginDir, "init-rtk-plugin", "", "write an RTK plugin scaffold to this directory and exit")
	flag.Var(&opts.pluginTimeout, "plugin-timeout-seconds", "timeout in seconds for --run-plugin")
	flag.BoolVar(&opts.pluginDryRun, "plugin-dry-run", false, "describe --run-plugin without executing it")
	flag.StringVar(&opts.bashCommand, "bash", "", "run an explicit local bash command and exit")
	flag.StringVar(&opts.bashDir, "bash-dir", "", "working directory for --bash")
	flag.Var(&opts.bashTimeout, "bash-timeout-seconds", "timeout in seconds for --bash")
	flag.StringVar(&opts.taskFilePath, "task-file", "", "JSON task-list file; defaults to .atteler/tasks.json")
	flag.StringVar(&opts.taskAddTitle, "task-add", "", "add a persistent agent task/TODO item and exit")
	flag.StringVar(&opts.taskAddID, "task-id", "", "optional stable ID for --task-add")
	flag.StringVar(&opts.taskAgent, "task-agent", "", "agent for --task-add or --task-complete")
	flag.StringVar(&opts.taskAssignSpec, "task-assign", "", "assign a task as id:agent and exit")
	flag.StringVar(&opts.taskCompleteID, "task-complete", "", "mark a task complete by ID and exit")
	flag.StringVar(&opts.memorySearch, "memory-search", "", "search local memory built from sessions, --memory-store, and --memory-index files")
	flag.StringVar(&opts.memoryStorePath, "memory-store", "", "JSON memory store path to load and/or save")
	flag.StringVar(&opts.mcpManifestPath, "mcp-manifest", "", "validate/list or invoke an MCP manifest YAML/JSON file and exit")
	flag.StringVar(&opts.mcpCapability, "mcp-capability", "", "find servers declaring this capability in --mcp-manifest")
	flag.StringVar(&opts.mcpServerName, "mcp-server", "", "server name in --mcp-manifest for --mcp-method or --mcp-tool")
	flag.StringVar(&opts.mcpMethod, "mcp-method", "", "invoke this JSON-RPC method on --mcp-server")
	flag.StringVar(&opts.mcpParamsJSON, "mcp-params", "", "JSON params for --mcp-method")
	flag.StringVar(&opts.mcpToolName, "mcp-tool", "", "invoke this MCP tool through tools/call on --mcp-server")
	flag.StringVar(&opts.mcpToolArgsJSON, "mcp-tool-args", "", "JSON object arguments for --mcp-tool")
	flag.Var(&opts.mcpTimeout, "mcp-timeout-seconds", "timeout in seconds for --mcp-method or --mcp-tool")
	flag.Var(&opts.memoryIndexFiles, "memory-index", "file to add to memory before saving/searching (repeatable or comma-separated)")
	flag.Var(&opts.memoryLimit, "memory-limit", "maximum memory search results")
	flag.StringVar(&opts.agentMemoryStorePath, "agent-memory-store", "", "JSON store for per-agent vector memory")
	flag.StringVar(&opts.agentMemoryAgent, "agent-memory-agent", "", "agent namespace for per-agent vector memory; defaults to --agent")
	flag.StringVar(&opts.agentMemorySearch, "agent-memory-search", "", "search one agent's vector memory and exit")
	flag.Var(&opts.agentMemoryIndexFiles, "agent-memory-index", "file to add to one agent's vector memory (repeatable or comma-separated)")
	flag.Var(&opts.agentMemoryLimit, "agent-memory-limit", "maximum per-agent memory search results")
	flag.StringVar(&opts.vectorSearch, "vector-search", "", "search --vector-index files with dependency-free local vector retrieval and exit")
	flag.Var(&opts.vectorIndexFiles, "vector-index", "file to add to vector search (repeatable or comma-separated)")
	flag.Var(&opts.vectorLimit, "vector-limit", "maximum vector search results")
	flag.BoolVar(&opts.lspSymbols, "lsp-symbols", false, "request document symbols from an external language server and exit")
	flag.StringVar(&opts.lspWorkspaceSymbols, "lsp-workspace-symbols", "", "query workspace symbols from an external language server and exit")
	flag.StringVar(&opts.lspCommand, "lsp-command", "", "language server command for --lsp-symbols or --lsp-workspace-symbols")
	flag.Var(&opts.lspArgs, "lsp-arg", "language server argument for LSP commands (repeatable)")
	flag.StringVar(&opts.lspFilePath, "lsp-file", "", "source file to inspect with --lsp-symbols")
	flag.StringVar(&opts.lspRootPath, "lsp-root", "", "workspace root for --lsp-symbols")
	flag.StringVar(&opts.lspLanguageID, "lsp-language", "", "language ID for --lsp-symbols; inferred from --lsp-file when omitted")
	flag.StringVar(&opts.promptCompleteInput, "prompt-complete", "", "suggest deterministic rest-of-line prompt completions and exit")
	flag.Var(&opts.promptCompleteLimit, "prompt-complete-limit", "maximum --prompt-complete suggestions")
	flag.BoolVar(&opts.asyncPlan, "async-plan", false, "print dependency-aware async task batches and exit")
	flag.BoolVar(&opts.asyncRun, "async-run", false, "execute dependency-aware async tasks by spawning Atteler sub-agents and exit")
	flag.Var(&opts.asyncTaskSpecs, "async-task", "task spec for --async-plan/--async-run: id|agent|prompt|dep1+dep2 (repeatable or comma-separated)")
	flag.Var(&opts.spawnAgentSpecs, "spawn-agent", "spawn sub-agent spec: id|agent|prompt or agent|prompt (repeatable)")
	flag.BoolVar(&opts.spawnDryRun, "spawn-dry-run", false, "print --spawn-agent invocations without executing them")
	flag.StringVar(&opts.spawnBinary, "spawn-binary", "", "atteler binary for --spawn-agent; defaults to the current executable")
	flag.Var(&opts.spawnTimeout, "spawn-timeout-seconds", "overall timeout in seconds for --spawn-agent")
	flag.Var(&opts.suggestSkillSteps, "skill-step", "observed action for skill suggestion (repeatable or comma-separated)")
	flag.StringVar(&opts.skillSaveDir, "skill-save-dir", "", "persist accepted --skill-step suggestion to this directory")
	flag.BoolVar(&opts.speculatePlan, "speculate-plan", false, "print a speculative three-round execution plan and exit")
	flag.BoolVar(&opts.speculateRun, "speculate-run", false, "execute the full speculative three-round pipeline with real LLM calls and exit")
	flag.StringVar(&opts.speculatePrompt, "speculate-prompt", "", "base task prompt for --speculate-plan prompt-cache reuse estimates")
	flag.Var(&opts.routeCandidates, "route-candidate", "model route candidate spec: provider/model,key=value... (repeatable or comma-separated)")
	flag.Var(&opts.routeInputTokens, "route-input-tokens", "estimated input tokens for model routing")
	flag.Var(&opts.routeOutputTokens, "route-output-tokens", "estimated output tokens for model routing")
	flag.Var(&opts.routeBudget, "route-budget", "maximum estimated request cost for model routing")
	flag.Var(&opts.routeCacheReuse, "route-cache-reuse", "prompt-cache reuse estimate for model routing (0..1)")
	flag.BoolVar(&opts.routeInteractive, "route-interactive", false, "rank model route candidates for low TTFT")
	flag.BoolVar(&opts.routeBatch, "route-batch", false, "rank model route candidates for batch/cost preference")
	flag.Var(&opts.speculateAgents, "speculate-agent", "agent name for --speculate-plan (repeatable or comma-separated)")
	flag.Var(&opts.speculateGates, "speculate-gate", "required gate check for --speculate-plan (repeatable or comma-separated)")
	flag.Var(&opts.reviewAgents, "review-agent", "reviewer name for --review-plan (repeatable or comma-separated)")
	flag.Var(&opts.reviewPaths, "review-path", "path for --review-plan review surface (repeatable or comma-separated)")
	flag.Var(&opts.reviewGates, "review-gate", "required gate check for --review-plan (repeatable or comma-separated)")
	flag.Var(&opts.skillMaxSteps, "skill-max-steps", "maximum repeated sequence length for --skill-step suggestions")
	flag.Var(&opts.skillMinOccurrences, "skill-min-occurrences", "minimum repeated occurrences for --skill-step suggestions")
	flag.StringVar(&opts.recordArtifact, "record-artifact", "", "record a session artifact path and exit")
	flag.StringVar(&opts.artifactKind, "artifact-kind", "", "kind for --record-artifact")
	flag.StringVar(&opts.artifactSummary, "artifact-summary", "", "summary for --record-artifact")
	flag.StringVar(&opts.mergeArtifactsPath, "merge-artifacts", "", "write selected-session text artifacts as merged Markdown; use '-' for stdout")
	flag.Var(&opts.mergeArtifactMaxBytes, "merge-artifact-max-bytes", "maximum bytes to read from each --merge-artifacts input")
	flag.StringVar(&opts.recordResponsePath, "record-response", "", "record a one-shot response to this JSON file")
	flag.StringVar(&opts.replayResponsePath, "replay-response", "", "replay a recorded one-shot response JSON file without calling an LLM")
	flag.Var(&opts.temperature, "temperature", "override request temperature")
	flag.Var(&opts.topP, "top-p", "override request nucleus sampling value (0..1)")
	flag.Var(&opts.maxTokens, "max-tokens", "override request max output tokens")
	flag.Var(&opts.seed, "seed", "best-effort deterministic seed for providers that support it")
	flag.StringVar(&opts.reasoningLevel, "reasoning-level", "", "override request reasoning level/effort")
	flag.Var(&opts.maxInputTokens, "max-input-tokens", "hard cap on estimated input tokens before an LLM call")
	flag.Var(&opts.contextPackTokens, "context-pack-tokens", "maximum estimated tokens for --context-pack-file")
	flag.Var(&opts.evaluationScore, "evaluation-score", "score for --record-evaluation")
	flag.BoolVar(&opts.listModels, "list-models", false, "list available models and exit")
	flag.BoolVar(&opts.listKnownModels, "list-known-models", false, "list built-in provider/model IDs without API calls and exit")
	flag.BoolVar(&opts.listProviders, "list-providers", false, "list built-in provider names without API calls and exit")
	flag.BoolVar(&opts.listAgents, "list-agents", false, "list configured agents and exit")
	flag.BoolVar(&opts.listCodeImports, "code-imports", false, "list Go import edges in the current repository and exit")
	flag.BoolVar(&opts.listCodeImportSummary, "code-import-summary", false, "list Go import paths with usage counts and exit")
	flag.BoolVar(&opts.listCodeImportFileSummary, "code-import-file-summary", false, "list Go files with import counts and exit")
	flag.BoolVar(&opts.listCodeLayers, "code-layers", false, "list topological Go import graph layers for the current repository and exit")
	flag.BoolVar(&opts.listCodeCycles, "code-cycles", false, "list Go import graph cycles for the current repository and exit")
	flag.BoolVar(&opts.codeSummary, "code-summary", false, "print compact Go code index and import graph counts and exit")
	flag.BoolVar(&opts.listCodeFiles, "code-files", false, "list Go files with package, import, and symbol counts and exit")
	flag.BoolVar(&opts.listCodeSymbolSummary, "code-symbol-summary", false, "list Go symbol kinds with counts and exit")
	flag.BoolVar(&opts.listCodeSymbolFileSummary, "code-symbol-file-summary", false, "list Go files with symbol counts and exit")
	flag.BoolVar(&opts.listCodePackages, "code-packages", false, "list Go packages with file and symbol counts and exit")
	flag.BoolVar(&opts.listCodePackageImportSummary, "code-package-import-summary", false, "list Go packages with import counts and exit")
	flag.BoolVar(&opts.listSessions, "list-sessions", false, "list saved sessions and exit")
	flag.BoolVar(&opts.listHeadless, "list-headless", false, "list active headless sessions and exit")
	flag.StringVar(&opts.streamHeadlessID, "stream-headless", "", "stream one headless session log by headless ID and exit when it finishes")
	flag.BoolVar(&opts.taskList, "task-list", false, "list persistent agent task/TODO items and exit")
	flag.BoolVar(&opts.listSessionTags, "list-session-tags", false, "list saved session tags with counts and exit")
	flag.BoolVar(&opts.agentPerformanceSummary, "agent-performance-summary", false, "summarize recorded agent performance across saved sessions and exit")
	flag.BoolVar(&opts.listArtifacts, "list-artifacts", false, "list artifacts recorded on the selected session and exit")
	flag.BoolVar(&opts.listEvaluations, "list-evaluations", false, "list agent evaluations recorded on the selected session and exit")
	flag.BoolVar(&opts.listFailures, "list-failures", false, "list negative-knowledge records on the selected session and exit")
	flag.BoolVar(&opts.listMessages, "list-messages", false, "list compact message records on the selected session and exit")
	flag.StringVar(&opts.listSessionsTag, "list-sessions-tag", "", "filter --list-sessions to sessions containing this exact tag")
	flag.BoolVar(&opts.listConfigPaths, "list-config-paths", false, "list config files in load order and exit")
	flag.BoolVar(&opts.listPlugins, "list-plugins", false, "list configured local plugin manifests and exit")
	flag.BoolVar(&opts.listHookEvents, "list-hook-events", false, "list supported lifecycle hook event types and exit")
	flag.BoolVar(&opts.listHookEventsJSON, "list-hook-events-json", false, "list supported lifecycle hook event types as JSON and exit")
	flag.BoolVar(&opts.watchScan, "watch-scan", false, "scan the current repository for background-agent health findings and exit")
	flag.BoolVar(&opts.watchJSON, "watch-json", false, "emit --watch-scan findings as JSON")
	flag.BoolVar(&opts.watchLoop, "watch-loop", false, "continuously scan the current repository for background-agent health findings")
	flag.Var(&opts.watchIntervalSeconds, "watch-interval-seconds", "seconds between --watch-loop scans")
	flag.Var(&opts.watchMaxIterations, "watch-max-iterations", "maximum --watch-loop scans before exiting; omit to run until interrupted")
	flag.BoolVar(&opts.reviewPlan, "review-plan", false, "print speculative review-agent plan and exit")
	flag.BoolVar(&opts.reviewScan, "review-scan", false, "scan the current repository and print a structured review report and exit")
	flag.BoolVar(&opts.feedbackProposals, "feedback-proposals", false, "derive agent improvement proposals from the selected session and exit")
	flag.StringVar(&opts.feedbackApplyConfig, "feedback-apply-config", "", "apply feedback proposals from the selected session to this agent config file")
	flag.StringVar(&opts.feedbackHistoryPath, "feedback-history", "", "append --feedback-apply-config decisions to this history log")
	flag.Var(&opts.watchLargeFileBytes, "watch-large-file-bytes", "large-file byte threshold for --watch-scan")
	flag.BoolVar(&opts.validateConfig, "validate-config", false, "validate merged YAML/JSON config and exit")
	flag.BoolVar(&opts.printConfigTemplate, "print-config-template", false, "print a starter YAML config and exit")
	flag.BoolVar(&opts.doctor, "doctor", false, "print local readiness diagnostics and exit")
	flag.BoolVar(&opts.doctorOffline, "doctor-offline", false, "print offline readiness diagnostics without provider health checks and exit")
	flag.BoolVar(&opts.readStdin, "stdin", false, "append stdin to a one-shot prompt")
	flag.BoolVar(&opts.headless, "headless", false, "run one-shot prompt without TUI output while recording headless metadata and logs")
	flag.BoolVar(&opts.showVersion, "version", false, "print version and exit")
	flag.BoolVar(&opts.useWorktree, "worktree", false, "isolate session in a git worktree")
	flag.BoolVar(&opts.listWorktrees, "list-worktrees", false, "list active atteler worktrees and exit")
	flag.BoolVar(&opts.noAutoMerge, "no-auto-merge", false, "keep worktree alive on exit instead of auto-merging")
	flag.StringVar(&opts.mergeWorktreeRef, "merge-worktree", "", "merge a session worktree back into its base branch and exit")

	flag.Usage = groupedUsage

	flag.Parse()

	if opts.oncePrompt == "" && flag.NArg() > 0 {
		opts.oncePrompt = strings.Join(flag.Args(), " ")
	}

	applyDebugEnvOptions(&opts, os.Getenv)

	return opts
}

func applyDebugEnvOptions(opts *cliOptions, getenv func(string) string) {
	if opts == nil || getenv == nil {
		return
	}

	applyDebugBool(getenv, "DEBUG_ATTELER_DOCTOR", &opts.doctor)
	applyDebugBool(getenv, "DEBUG_ATTELER_DOCTOR_OFFLINE", &opts.doctorOffline)
	applyDebugBool(getenv, "DEBUG_ATTELER_VALIDATE_CONFIG", &opts.validateConfig)
	applyDebugBool(getenv, "DEBUG_ATTELER_LIST_CONFIG_PATHS", &opts.listConfigPaths)
	applyDebugBool(getenv, "DEBUG_ATTELER_LIST_PROVIDERS", &opts.listProviders)
	applyDebugBool(getenv, "DEBUG_ATTELER_LIST_KNOWN_MODELS", &opts.listKnownModels)
	applyDebugBool(getenv, "DEBUG_ATTELER_LIST_MODELS", &opts.listModels)
	applyDebugBool(getenv, "DEBUG_ATTELER_LIST_AGENTS", &opts.listAgents)
	applyDebugBool(getenv, "DEBUG_ATTELER_LIST_PLUGINS", &opts.listPlugins)
	applyDebugBool(getenv, "DEBUG_ATTELER_LIST_HOOK_EVENTS", &opts.listHookEvents)
	applyDebugBool(getenv, "DEBUG_ATTELER_LIST_HOOK_EVENTS_JSON", &opts.listHookEventsJSON)
	applyDebugBool(getenv, "DEBUG_ATTELER_CODE_SUMMARY", &opts.codeSummary)
	applyDebugBool(getenv, "DEBUG_ATTELER_CODE_FILES", &opts.listCodeFiles)
	applyDebugBool(getenv, "DEBUG_ATTELER_CODE_IMPORTS", &opts.listCodeImports)
	applyDebugBool(getenv, "DEBUG_ATTELER_CODE_IMPORT_SUMMARY", &opts.listCodeImportSummary)
	applyDebugBool(getenv, "DEBUG_ATTELER_CODE_IMPORT_FILE_SUMMARY", &opts.listCodeImportFileSummary)
	applyDebugBool(getenv, "DEBUG_ATTELER_CODE_LAYERS", &opts.listCodeLayers)
	applyDebugBool(getenv, "DEBUG_ATTELER_CODE_CYCLES", &opts.listCodeCycles)
	applyDebugBool(getenv, "DEBUG_ATTELER_REVIEW_PLAN", &opts.reviewPlan)
	applyDebugBool(getenv, "DEBUG_ATTELER_REVIEW_SCAN", &opts.reviewScan)
	applyDebugBool(getenv, "DEBUG_ATTELER_AGENT_PERFORMANCE_SUMMARY", &opts.agentPerformanceSummary)
	applyDebugBool(getenv, "DEBUG_ATTELER_WATCH_SCAN", &opts.watchScan)
	applyDebugBool(getenv, "DEBUG_ATTELER_WATCH_JSON", &opts.watchJSON)
	applyDebugBool(getenv, "DEBUG_ATTELER_WATCH_LOOP", &opts.watchLoop)
	applyDebugBool(getenv, "DEBUG_ATTELER_LSP_SYMBOLS", &opts.lspSymbols)

	applyDebugString(getenv, "DEBUG_ATTELER_MCP_MANIFEST", &opts.mcpManifestPath)
	applyDebugString(getenv, "DEBUG_ATTELER_MCP_CAPABILITY", &opts.mcpCapability)
	applyDebugString(getenv, "DEBUG_ATTELER_MCP_SERVER", &opts.mcpServerName)
	applyDebugString(getenv, "DEBUG_ATTELER_MCP_METHOD", &opts.mcpMethod)
	applyDebugString(getenv, "DEBUG_ATTELER_MCP_PARAMS", &opts.mcpParamsJSON)
	applyDebugString(getenv, "DEBUG_ATTELER_MCP_TOOL", &opts.mcpToolName)
	applyDebugString(getenv, "DEBUG_ATTELER_MCP_TOOL_ARGS", &opts.mcpToolArgsJSON)
	applyDebugString(getenv, "DEBUG_ATTELER_LSP_COMMAND", &opts.lspCommand)
	applyDebugRawStringList(getenv, "DEBUG_ATTELER_LSP_ARGS", &opts.lspArgs)
	applyDebugString(getenv, "DEBUG_ATTELER_LSP_FILE", &opts.lspFilePath)
	applyDebugString(getenv, "DEBUG_ATTELER_LSP_ROOT", &opts.lspRootPath)
	applyDebugString(getenv, "DEBUG_ATTELER_LSP_LANGUAGE", &opts.lspLanguageID)
	applyDebugString(getenv, "DEBUG_ATTELER_LSP_WORKSPACE_SYMBOLS", &opts.lspWorkspaceSymbols)
	applyDebugString(getenv, "DEBUG_ATTELER_GIT_HISTORY_SEARCH", &opts.gitHistorySearch)
	applyDebugPositiveInt(getenv, "DEBUG_ATTELER_GIT_HISTORY_LIMIT", &opts.gitHistoryLimit)
	applyDebugPositiveInt(getenv, "DEBUG_ATTELER_MCP_TIMEOUT_SECONDS", &opts.mcpTimeout)
	applyDebugPositiveInt(getenv, "DEBUG_ATTELER_WATCH_LARGE_FILE_BYTES", &opts.watchLargeFileBytes)
	applyDebugPositiveInt(getenv, "DEBUG_ATTELER_WATCH_INTERVAL_SECONDS", &opts.watchIntervalSeconds)
	applyDebugPositiveInt(getenv, "DEBUG_ATTELER_WATCH_MAX_ITERATIONS", &opts.watchMaxIterations)
}

func applyDebugBool(getenv func(string) string, name string, target *bool) {
	if target == nil || *target {
		return
	}

	switch strings.ToLower(strings.TrimSpace(getenv(name))) {
	case "1", "true", "yes", "on":
		*target = true
	}
}

func applyDebugString(getenv func(string) string, name string, target *string) {
	if target == nil || strings.TrimSpace(*target) != "" {
		return
	}

	if value := strings.TrimSpace(getenv(name)); value != "" {
		*target = value
	}
}

func applyDebugRawStringList(getenv func(string) string, name string, target *rawStringListFlag) {
	if target == nil || len(*target) > 0 {
		return
	}

	if value := strings.TrimSpace(getenv(name)); value != "" {
		*target = append(*target, value)
	}
}

func applyDebugPositiveInt(getenv func(string) string, name string, target *positiveIntFlag) {
	if target == nil || target.set {
		return
	}

	value := strings.TrimSpace(getenv(name))
	if value == "" {
		return
	}

	if err := target.Set(value); err != nil {
		fmt.Fprintln(os.Stderr, "warning: ignoring "+name+": "+err.Error())
	}
}

func main() {
	configureSlog()

	ctx, stop := signal.NotifyContext(rootContext(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)

		stop()     // ensure signal handler is deregistered before exit
		os.Exit(1) //nolint:gocritic // defer stop() handles the normal exit path
	}
}

func rootContext() context.Context {
	return context.Background()
}

// configureSlog sets up the global slog handler. It reads the SLOG_LEVEL
// environment variable (debug, info, warn, error) and defaults to info.
// Output goes to stderr so it doesn't interfere with normal TUI output.
func configureSlog() {
	level := slog.LevelInfo

	switch strings.ToLower(os.Getenv("SLOG_LEVEL")) {
	case "debug":
		level = slog.LevelDebug
	case "warn", "warning":
		level = slog.LevelWarn
	case statusError:
		level = slog.LevelError
	}

	handler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	slog.SetDefault(slog.New(handler))
}

func versionString() string {
	return fmt.Sprintf("atteler %s (commit %s, built %s)", version, commit, date)
}

func initConfig(path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return errors.New("config path is required")
	}

	dir := filepath.Dir(path)
	if dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("create config dir %s: %w", dir, err)
		}
	}

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("config %s already exists", path)
		}

		return fmt.Errorf("create config %s: %w", path, err)
	}

	if _, err := file.WriteString(appconfig.TemplateYAML()); err != nil {
		_ = file.Close()
		return fmt.Errorf("write config %s: %w", path, err)
	}

	if err := file.Close(); err != nil {
		return fmt.Errorf("close config %s: %w", path, err)
	}

	fmt.Println("Wrote " + path)

	return nil
}

func oneShotPrompt(prompt string, readStdin bool) (string, error) {
	if readStdin {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", fmt.Errorf("read stdin: %w", err)
		}

		prompt = appendStdinContext(prompt, string(data))
	}

	if strings.TrimSpace(prompt) == "" {
		return "", errors.New("one-shot prompt is empty")
	}

	return prompt, nil
}

func normalizeOutputFormat(format string) (string, error) {
	format = strings.ToLower(strings.TrimSpace(format))
	if format == "" {
		return outputFormatText, nil
	}

	switch format {
	case outputFormatText, outputFormatJSON:
		return format, nil
	default:
		return "", fmt.Errorf("unsupported output format %q (supported: %s, %s)", format, outputFormatText, outputFormatJSON)
	}
}

func appendStdinContext(prompt, stdin string) string {
	stdin = strings.TrimRight(stdin, "\n")
	if strings.TrimSpace(stdin) == "" {
		return prompt
	}

	if strings.TrimSpace(prompt) == "" {
		return stdin
	}

	return prompt + "\n\n<stdin>\n" + stdin + "\n</stdin>"
}

func listConfigPaths() {
	for _, path := range appconfig.DefaultPaths() {
		fmt.Println(path + "\t" + configPathStatus(path))
	}
}

func validateConfig() error {
	_, loaded, err := appconfig.Load()
	if err != nil {
		return fmt.Errorf("validate config: %w", err)
	}

	if len(loaded) == 0 {
		fmt.Println("Config valid: no config files loaded.")
		return nil
	}

	fmt.Println("Config valid: " + strings.Join(loaded, ", "))

	return nil
}

func configPathStatus(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "missing"
		}

		return "error: " + err.Error()
	}

	if info.IsDir() {
		return "directory"
	}

	return "present"
}

func listKnownProviders() {
	for _, provider := range knownProvidersSorted() {
		fmt.Println(provider.Name)
	}
}

func listKnownModels() {
	for _, provider := range knownProvidersSorted() {
		sort.Strings(provider.Models)

		for _, model := range provider.Models {
			fmt.Println(provider.Name + "/" + model)
		}
	}
}

func knownProvidersSorted() []llm.ProviderInfo {
	providers := llm.KnownProviders()
	sort.Slice(providers, func(i, j int) bool {
		return providers[i].Name < providers[j].Name
	})

	return providers
}

func run(ctx context.Context) error {
	opts := parseOptions()
	if opts.configPaths != "" {
		if err := os.Setenv(appconfig.EnvPath, opts.configPaths); err != nil {
			fmt.Fprintln(os.Stderr, "warning: cannot set config path override: "+err.Error())
		}
	}

	if handled, err := runInlineCommand(ctx, opts); handled {
		return err
	}

	// Phase 1: providerless commands (no LLM registry needed).
	store := session.NewStore(opts.sessionDir)

	if handled, err := dispatchProviderless(ctx, opts, store); handled {
		return err
	}

	// Phase 2: providerless-config commands (need config/agents but no LLM).
	if providerlessConfigRequested(opts) {
		state, stateErr := providerlessState(store)
		if stateErr != nil {
			return stateErr
		}

		if handled, err := dispatchProviderlessConfig(ctx, opts, state); handled {
			return err
		}
	}

	// Phase 3: full state (LLM providers, hooks, sessions).
	state, err := loadAppState(ctx, opts)
	if err != nil {
		return err
	}

	return runWithState(ctx, opts, state)
}

// runInlineCommand handles trivial early-exit commands (version, config
// template, etc.) that need no session store or provider.
func runInlineCommand(ctx context.Context, opts cliOptions) (bool, error) {
	switch {
	case opts.printConfigTemplate:
		fmt.Print(appconfig.TemplateYAML())
		return true, nil
	case opts.showVersion:
		fmt.Println(versionString())
		return true, nil
	case opts.initConfigPath != "":
		return true, initConfig(opts.initConfigPath)
	case opts.listConfigPaths:
		listConfigPaths()
		return true, nil
	case opts.validateConfig:
		return true, validateConfig()
	case opts.doctorOffline:
		return true, doctorOffline(opts)
	case opts.listProviders:
		listKnownProviders()
		return true, nil
	case opts.listKnownModels:
		listKnownModels()
		return true, nil
	case opts.listWorktrees:
		return true, listWorktrees(ctx)
	case opts.mergeWorktreeRef != "":
		return true, mergeWorktreeBySession(ctx, opts.mergeWorktreeRef)
	default:
		return false, nil
	}
}

func providerlessState(store *session.Store) (appState, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return appState{}, fmt.Errorf("locate working directory: %w", err)
	}

	cfg, loadedConfigPaths, cfgErr := appconfig.Load()
	if cfgErr != nil {
		fmt.Fprintln(os.Stderr, "warning: "+cfgErr.Error())
	}

	return appState{
		agentRegistry:     agent.NewRegistry(cfg.Agents),
		sessionStore:      store,
		cwd:               cwd,
		loadedConfigPaths: loadedConfigPaths,
		pluginPaths:       append([]string(nil), cfg.Plugins.Paths...),
	}, nil
}

func runWithState(ctx context.Context, opts cliOptions, state appState) error {
	if handled, err := dispatchStateful(ctx, opts, state); handled {
		return err
	}

	outputFormat, err := normalizeOutputFormat(opts.outputFormat)
	if err != nil {
		return err
	}

	if opts.headless && opts.oncePrompt == "" && !opts.readStdin {
		return errors.New("headless mode requires --once, positional prompt text, or --stdin")
	}

	if opts.oncePrompt == "" && !opts.readStdin {
		return runInteractive(ctx, state)
	}

	prompt, err := oneShotPrompt(opts.oncePrompt, opts.readStdin)
	if err != nil {
		return err
	}

	runErr := runOnceWithOptions(
		ctx,
		state.registry,
		state.agentRegistry,
		state.hookRunner,
		state.sessionStore,
		state.sessionState,
		state.contextOptions,
		state.referenceContext,
		state.selectedModel,
		state.selectedAgent,
		state.fallbackModels,
		state.generationDefaults,
		state.generationOverrides,
		state.maxInputTokens,
		runOnceExecutionOptions{
			Response: responseRecordOptions{
				RecordPath: opts.recordResponsePath,
				ReplayPath: opts.replayResponsePath,
			},
			OutputFormat: outputFormat,
			Headless:     opts.headless,
		},
		state.modelLocked,
		prompt,
	)
	finalizeWorktree(ctx, &state)

	return runErr
}

func loadAppState(ctx context.Context, opts cliOptions) (appState, error) {
	cfg, loadedConfigPaths, err := appconfig.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning: "+err.Error())
	}

	agentRegistry := agent.NewRegistry(cfg.Agents)
	// Default to a stderr logger so events from utility commands (--bash,
	// --mcp, one-shot, etc.) are visible without extra configuration. Headless
	// runs stay quiet so JSON output isn't polluted; runInteractive replaces
	// this with a logger-less runner so stderr writes don't bleed onto the TUI.
	var hookLogWriter io.Writer
	if !opts.headless {
		hookLogWriter = os.Stderr
	}

	hookRunner := events.NewRunnerWithLogger(cfg.Hooks, hookLogWriter)
	store := session.NewStore(opts.sessionDir)
	stateStore := appconfig.NewStateStore("")

	persistedState, stateErr := stateStore.Load()
	if stateErr != nil {
		fmt.Fprintln(os.Stderr, "warning: "+stateErr.Error())
	}

	cwd, cwdErr := os.Getwd()
	if cwdErr != nil {
		cwd = ""
	}

	selection, err := resolveSelection(opts, cfg, persistedState.ModelForFolder(cwd), agentRegistry, store)
	if err != nil {
		return appState{}, err
	}

	reg := autoRegisterForOptions(ctx, opts, cfg, selection.selectedModel)
	contextOptions := contextOptionsFromConfig(cfg)
	referenceContext := loadConfiguredReferences(ctx, cfg.Context.References, contextOptions)
	generationDefaults := generationFromConfig(cfg)
	generationOverrides := generationOverridesFromState(opts, selection, persistedState, cwd)

	maxInputTokens := maxInputTokensFromConfigOptions(cfg, opts)

	providers := reg.ListProviders()
	if len(providers) == 0 && !opts.headless {
		fmt.Fprintln(os.Stderr, "warning: no LLM providers configured, set ANTHROPIC_API_KEY or OPENAI_API_KEY")
	}

	// Set up git worktree isolation when requested.
	var wtInfo *worktree.Info

	if opts.useWorktree && cwd != "" {
		// If continuing a session that already has a worktree, re-use it.
		if selection.sessionState.WorktreePath != "" {
			wtInfo = &worktree.Info{
				Path:       selection.sessionState.WorktreePath,
				Branch:     selection.sessionState.WorktreeBranch,
				BaseBranch: selection.sessionState.WorktreeBase,
				SessionID:  selection.sessionState.ID,
			}
			fmt.Fprintln(os.Stderr, "worktree: reusing "+wtInfo.Path)
		} else {
			wtInfo, err = worktree.CreateContext(ctx, cwd, selection.sessionState.ID)
			if err != nil {
				return appState{}, fmt.Errorf("worktree setup: %w", err)
			}

			selection.sessionState.WorktreePath = wtInfo.Path
			selection.sessionState.WorktreeBranch = wtInfo.Branch
			selection.sessionState.WorktreeBase = wtInfo.BaseBranch
			fmt.Fprintln(os.Stderr, "worktree: created "+wtInfo.Path+" (branch "+wtInfo.Branch+")")
		}

		// Update context references to point at the worktree.
		contextOptions.Root = wtInfo.Path
	}

	return appState{
		registry:            reg,
		agentRegistry:       agentRegistry,
		hookRunner:          hookRunner,
		sessionStore:        store,
		stateStore:          stateStore,
		contextOptions:      contextOptions,
		referenceContext:    referenceContext,
		sessionState:        selection.sessionState,
		worktreeInfo:        wtInfo,
		cwd:                 cwd,
		loadedConfigPaths:   loadedConfigPaths,
		providers:           providers,
		selectedModel:       selection.selectedModel,
		selectedAgent:       selection.selectedAgent,
		fallbackModels:      selection.fallbackModels,
		pluginPaths:         append([]string(nil), cfg.Plugins.Paths...),
		generationDefaults:  generationDefaults,
		generationOverrides: generationOverrides,
		maxInputTokens:      maxInputTokens,
		hookConfig:          cfg.Hooks,
		modelLocked:         selection.modelLocked,
		autoMergeWorktree:   opts.useWorktree && !opts.noAutoMerge,
	}, nil
}

func autoRegisterForOptions(ctx context.Context, opts cliOptions, cfg appconfig.Config, selectedModel string) *llm.Registry {
	regCfg := llmConfig(cfg, selectedModel)

	if opts.headless {
		regCfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	return llm.AutoRegisterWithConfigContext(ctx, regCfg)
}

func generationOverridesFromState(opts cliOptions, selection selectionState, persistedState appconfig.State, cwd string) generationSettings {
	generation := generationFromOptions(opts)
	if generation.ReasoningLevel != "" {
		return generation
	}

	if level := strings.TrimSpace(selection.sessionState.DefaultReasoningLevel); level != "" {
		generation.ReasoningLevel = level
		return generation
	}

	if level := strings.TrimSpace(persistedState.ReasoningLevelForFolder(cwd)); level != "" {
		generation.ReasoningLevel = level
	}

	return generation
}

type selectionState struct {
	sessionState   session.Session
	selectedModel  string
	selectedAgent  string
	fallbackModels []string
	modelLocked    bool
}

func resolveSelection(
	opts cliOptions,
	cfg appconfig.Config,
	persistedModel string,
	agentRegistry *agent.Registry,
	store *session.Store,
) (selectionState, error) {
	state := selectionState{
		selectedAgent:  opts.agentName,
		selectedModel:  opts.model,
		modelLocked:    opts.model != "",
		fallbackModels: append([]string(nil), cfg.FallbackModels...),
	}
	if state.modelLocked {
		state.fallbackModels = nil
	}

	state.sessionState = session.New(state.selectedModel, nil)
	if err := loadRequestedSession(opts, store, &state); err != nil {
		return selectionState{}, err
	}

	if err := applySelectedAgent(opts, agentRegistry, &state); err != nil {
		return selectionState{}, err
	}

	if err := applyRouteSelection(opts, &state); err != nil {
		return selectionState{}, err
	}

	if state.selectedModel == "" {
		state.selectedModel = persistedModel
	}

	if state.selectedModel == "" {
		state.selectedModel = cfg.DefaultModel
	}

	if state.selectedModel != "" {
		state.sessionState.DefaultModel = state.selectedModel
	}

	if opts.sessionTitle != "" {
		state.sessionState.Title = opts.sessionTitle
	}

	if len(opts.sessionTags) > 0 {
		state.sessionState.Tags = mergeTags(state.sessionState.Tags, opts.sessionTags)
	}

	return state, nil
}

func loadRequestedSession(opts cliOptions, store *session.Store, state *selectionState) error {
	if opts.sessionRef == "" && opts.replayRef == "" && opts.exportRef == "" && opts.showSessionRef == "" && opts.summarySessionRef == "" {
		return nil
	}

	ref := firstNonEmpty(opts.replayRef, opts.showSessionRef, opts.summarySessionRef, opts.exportRef, opts.sessionRef)

	loadedSession, err := store.Load(ref)
	if err != nil {
		return fmt.Errorf("load session: %w", err)
	}

	state.sessionState = loadedSession
	if state.selectedAgent == "" {
		state.selectedAgent = state.sessionState.DefaultAgent
	}

	if state.selectedModel == "" {
		state.selectedModel = state.sessionState.DefaultModel
	}

	return nil
}

func applySelectedAgent(opts cliOptions, agentRegistry *agent.Registry, state *selectionState) error {
	if state.selectedAgent == "" || opts.replayRef != "" || opts.exportRef != "" || opts.showSessionRef != "" {
		return nil
	}

	activeAgent, ok := agentRegistry.Get(state.selectedAgent)
	if !ok {
		return fmt.Errorf("unknown agent %q", state.selectedAgent)
	}

	if state.selectedModel == "" {
		state.selectedModel = activeAgent.Model
	}

	if !state.modelLocked && len(activeAgent.FallbackModels) > 0 {
		state.fallbackModels = activeAgent.FallbackModels
	}

	state.sessionState.DefaultAgent = state.selectedAgent

	return nil
}

// flagGroup assigns each flag name to a domain group for grouped --help output.
var flagGroups = map[string]string{
	// Provider & model
	"model": "Provider & Model", "list-models": "Provider & Model", "list-known-models": "Provider & Model",
	"list-providers": "Provider & Model", "temperature": "Provider & Model", "top-p": "Provider & Model",
	"max-tokens": "Provider & Model", "seed": "Provider & Model", "reasoning-level": "Provider & Model",
	"max-input-tokens": "Provider & Model",

	// Session
	"session": "Session", "session-id": "Session", "session-dir": "Session", "session-title": "Session", "session-tag": "Session",
	"list-sessions": "Session", "list-sessions-tag": "Session", "list-session-tags": "Session",
	"show-session": "Session", "session-summary": "Session", "replay": "Session",
	"export-session": "Session", "export-format": "Session", "search-sessions": "Session",
	"list-messages": "Session", "list-artifacts": "Session", "list-evaluations": "Session",
	"list-failures": "Session", "agent-performance-summary": "Session",

	// Agent
	"agent": "Agent", "list-agents": "Agent", "describe-agent": "Agent", "plan-agents": "Agent",
	"plan-agent": "Agent", "plan-max-agents": "Agent",

	// One-shot & output
	"once": "One-shot & Output", "stdin": "One-shot & Output", "output": "One-shot & Output",
	"headless": "One-shot & Output", "list-headless": "One-shot & Output", "stream-headless": "One-shot & Output",

	// Config
	"config": "Configuration", "print-config-template": "Configuration", "init-config": "Configuration",
	"list-config-paths": "Configuration", "validate-config": "Configuration",

	// Plugin
	"list-plugins": "Plugin", "describe-plugin": "Plugin", "run-plugin": "Plugin", "init-rtk-plugin": "Plugin",
	"plugin-entrypoint": "Plugin", "plugin-dry-run": "Plugin", "plugin-timeout-seconds": "Plugin",

	// Memory & RAG
	"memory-search": "Memory & RAG", "memory-store": "Memory & RAG", "memory-index": "Memory & RAG",
	"memory-limit": "Memory & RAG", "agent-memory-agent": "Memory & RAG", "agent-memory-store": "Memory & RAG",
	"agent-memory-index": "Memory & RAG", "agent-memory-search": "Memory & RAG", "agent-memory-limit": "Memory & RAG",
	"vector-search": "Memory & RAG", "vector-index": "Memory & RAG", "vector-limit": "Memory & RAG",
	"git-history-search": "Memory & RAG", "git-history-limit": "Memory & RAG",

	// Speculative execution
	"speculate-plan": "Speculative Execution", "speculate-run": "Speculative Execution",
	"speculate-agent": "Speculative Execution", "speculate-gate": "Speculative Execution",
	"speculate-prompt": "Speculative Execution",

	// Review
	"review-plan": "Review", "review-scan": "Review", "review-agent": "Review",
	"review-path": "Review", "review-gate": "Review",

	// Shell & bash
	"bash": "Shell", "bash-dir": "Shell", "bash-timeout-seconds": "Shell",

	// Worktree
	"worktree": "Worktree", "no-auto-merge": "Worktree", "list-worktrees": "Worktree",
	"merge-worktree": "Worktree",

	// Code intelligence
	"code-summary": "Code Intelligence", "code-files": "Code Intelligence",
	"code-imports": "Code Intelligence", "code-symbol": "Code Intelligence",
	"code-layers": "Code Intelligence", "code-cycles": "Code Intelligence",
	"code-impact": "Code Intelligence", "code-packages": "Code Intelligence",

	// Diagnostics
	"doctor": "Diagnostics", "doctor-offline": "Diagnostics", "version": "Diagnostics",

	// Evaluation
	"eval-output": "Evaluation", "eval-expected": "Evaluation", "eval-expected-file": "Evaluation",
	"eval-mode": "Evaluation", "record-evaluation": "Evaluation", "evaluation-outcome": "Evaluation",
	"evaluation-score": "Evaluation", "evaluation-notes": "Evaluation", "evaluation-reference": "Evaluation",

	// Hooks
	"list-hook-events": "Hooks", "list-hook-events-json": "Hooks",

	// MCP
	"mcp-manifest": "MCP", "mcp-capability": "MCP", "mcp-server": "MCP",
	"mcp-method": "MCP", "mcp-params": "MCP", "mcp-tool": "MCP",
	"mcp-tool-args": "MCP", "mcp-timeout-seconds": "MCP",

	// Recording & replay
	"record-response": "Recording & Replay", "replay-response": "Recording & Replay",
	"record-failure": "Recording & Replay", "failure-reason": "Recording & Replay",
	"failure-commit": "Recording & Replay", "record-artifact": "Recording & Replay",
	"artifact-kind": "Recording & Replay", "artifact-summary": "Recording & Replay",
	"merge-artifacts": "Recording & Replay", "merge-artifact-max-bytes": "Recording & Replay",

	// Routing
	"route-candidate": "Model Routing", "route-input-tokens": "Model Routing",
	"route-output-tokens": "Model Routing", "route-budget": "Model Routing",
	"route-cache-reuse": "Model Routing", "route-interactive": "Model Routing",
	"route-batch": "Model Routing",
}

var implicitFlagDefaults = map[string]string{
	"agent-memory-limit":       "5",
	"bash-timeout-seconds":     "120",
	"context-pack-tokens":      "unlimited",
	"git-history-limit":        "5",
	"mcp-timeout-seconds":      "none",
	"memory-limit":             "5",
	"merge-artifact-max-bytes": strconv.FormatInt(watchDefaultLargeFileBytes(), 10),
	"plan-max-agents":          "unlimited",
	"plugin-timeout-seconds":   "30",
	"prompt-complete-limit":    "5",
	"skill-max-steps":          "6",
	"skill-min-occurrences":    "2",
	"spawn-timeout-seconds":    "none",
	"vector-limit":             "5",
	"watch-interval-seconds":   "60",
	"watch-large-file-bytes":   strconv.FormatInt(watchDefaultLargeFileBytes(), 10),
	"watch-max-iterations":     "unlimited",
}

func watchDefaultLargeFileBytes() int64 {
	return 1 << 20
}

// groupedUsage prints flags organized by domain group with default values.
func groupedUsage() {
	fmt.Fprintf(os.Stderr, "Usage: atteler [flags] [prompt]\n\n")

	groups := make(map[string][]*flag.Flag)
	groupOrder := make([]string, 0)

	var ungrouped []*flag.Flag

	flag.VisitAll(func(f *flag.Flag) {
		group, ok := flagGroups[f.Name]
		if !ok {
			ungrouped = append(ungrouped, f)
			return
		}

		if _, exists := groups[group]; !exists {
			groupOrder = append(groupOrder, group)
		}

		groups[group] = append(groups[group], f)
	})

	sort.Strings(groupOrder)

	for _, group := range groupOrder {
		fmt.Fprintf(os.Stderr, "%s:\n", group)

		for _, f := range groups[group] {
			printFlagWithDefault(f)
		}

		fmt.Fprintln(os.Stderr)
	}

	if len(ungrouped) > 0 {
		fmt.Fprintf(os.Stderr, "Other:\n")

		for _, f := range ungrouped {
			printFlagWithDefault(f)
		}

		fmt.Fprintln(os.Stderr)
	}
}

func printFlagWithDefault(f *flag.Flag) {
	name := "  -" + f.Name

	usage := f.Usage + " (default: " + defaultValueForFlag(f) + ")"

	// Two-column format: flag name on the left, usage on the right.
	if len(name) < 30 {
		fmt.Fprintf(os.Stderr, "%-30s %s\n", name, usage)
	} else {
		fmt.Fprintf(os.Stderr, "%s\n%-30s %s\n", name, "", usage)
	}
}

func defaultValueForFlag(f *flag.Flag) string {
	if f == nil {
		return strconv.Quote("")
	}

	if value, ok := implicitFlagDefaults[f.Name]; ok {
		return value
	}

	if f.DefValue == "" {
		return strconv.Quote("")
	}

	return f.DefValue
}
