// Package main is the entry point for the atteler TUI application.
package main

import (
	"context"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/tommoulard/atteler/pkg/agent"
	"github.com/tommoulard/atteler/pkg/autonomy"
	appconfig "github.com/tommoulard/atteler/pkg/config"
	"github.com/tommoulard/atteler/pkg/contextref"
	"github.com/tommoulard/atteler/pkg/events"
	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/modelroute"
	"github.com/tommoulard/atteler/pkg/permission"
	"github.com/tommoulard/atteler/pkg/session"
	"github.com/tommoulard/atteler/pkg/worktree"
)

// ---------------------------------------------------------------------------
// Styles
// ---------------------------------------------------------------------------

func rootContext() context.Context {
	return context.Background()
}

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

var runInteractiveProgram = func(m model) (tea.Model, error) {
	return tea.NewProgram(m).Run()
}

const (
	unknownLabel         = "unknown"
	executionModePlan    = "plan"
	executionModeExecute = "execute"
)

var (
	promptStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("170")).
			Bold(true)

	statusStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("241"))

	errStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("196"))

	warnStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("214"))

	assistantLabel = lipgloss.NewStyle().
			Foreground(lipgloss.Color("39")).
			Bold(true)

	userLabel = lipgloss.NewStyle().
			Foreground(lipgloss.Color("178")).
			Bold(true)

	dimStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("241"))

	pickerHeaderStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("39")).
				Bold(true)

	pickerSelectedStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("170")).
				Bold(true)

	pickerNormalStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("252"))

	pickerProviderStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("39")).
				Bold(true)
)

// Key binding constants.
const (
	keyCtrlC         = "ctrl+c"
	keyAltEnter      = "alt+enter"
	keyDown          = "down"
	keyEnter         = "enter"
	keyEsc           = "esc"
	keyShiftEnter    = "shift+enter"
	keyUp            = "up"
	outputFormatJSON = "json"
	outputFormatText = "text"
	statusError      = "error"
	statusFail       = "FAIL"
	statusWarn       = "WARN"

	maxPromptHistoryEntries = 100
	taskTickInterval        = time.Second
	idleSuggestionDelay     = time.Second
	idleSuggestionTimeout   = 8 * time.Second

	idleSuggestionStatusPending        = "pending"
	idleSuggestionStatusPendingForced  = "pending:forced"
	idleSuggestionStatusSending        = "sending"
	idleSuggestionStatusReadyModel     = "ready:model"
	idleSuggestionStatusRejectedStale  = "rejected:stale"
	idleSuggestionStatusRejectedError  = "rejected:error"
	idleSuggestionStatusRejectedEmpty  = "rejected:empty"
	idleSuggestionStatusRejectedBudget = "rejected:budget"

	idleSuggestionMaxRequestsPerSession = 20
	idleSuggestionMaxRequestsPerMinute  = 6
	idleSuggestionMaxInputTokens        = 1024
	idleSuggestionMaxOutputTokens       = 32
	idleSuggestionMaxSessionTokens      = 12_000
	idleSuggestionMaxEstimatedCostUSD   = 0.05
)

var terminalTitleSpinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// ---------------------------------------------------------------------------
// Messages (tea.Msg)
// ---------------------------------------------------------------------------

// llmResponseMsg is sent when the LLM call completes.
type llmResponseMsg struct {
	err           error
	completedAt   time.Time
	content       string
	provider      string
	model         string
	eventLines    []string
	routeDecision *modelroute.Decision
	toolLog       []string // tool call summaries (command + truncated output)
	tokenUsage    tokenUsage
	liveEvents    bool
}

type llmEventLineMsg struct {
	liveCh <-chan tea.Msg
	line   string
}

type llmToolOutputMsg struct {
	liveCh   <-chan tea.Msg
	command  string
	stream   string
	data     string
	sequence int64
}

// shellResultMsg is sent when a `!command` finishes executing.
type shellResultMsg struct {
	err         error
	completedAt time.Time
	command     string
	stdout      string
	stderr      string
	streamed    bool
}

// shellOutputMsg is sent while a `!command` is still running.
type shellOutputMsg struct {
	outputCh <-chan tea.Msg
	command  string
	stream   string
	data     string
	sequence int64
}

type idleSuggestionRequestMsg struct {
	input string
	id    int
}

type idleSuggestionMsg struct {
	err                   error
	input                 string
	suggestion            string
	provider              string
	model                 string
	contextSummary        string
	usage                 tokenUsage
	estimatedInputTokens  int
	estimatedOutputTokens int
	estimatedCostUSD      float64
	id                    int
	force                 bool
	providerCall          bool
}

type promptSuggestionConsent string

const (
	promptSuggestionConsentLocalOnly promptSuggestionConsent = promptSuggestionConsent(appconfig.PromptSuggestionPreferenceLocalOnly)
	promptSuggestionConsentSession   promptSuggestionConsent = promptSuggestionConsent(appconfig.ModelScopeSession)
	promptSuggestionConsentFolder    promptSuggestionConsent = promptSuggestionConsent(appconfig.ModelScopeFolder)
	promptSuggestionConsentGlobal    promptSuggestionConsent = promptSuggestionConsent(appconfig.ModelScopeGlobal)
)

type idleSuggestionBudget struct {
	MaxRequestsPerSession int
	MaxRequestsPerMinute  int
	MaxInputTokens        int
	MaxOutputTokens       int
	MaxSessionTokens      int
	MaxEstimatedCostUSD   float64
}

type taskTickMsg struct {
	id int
}

type agentLoopConfirmKind string

const (
	agentLoopConfirmCheckpoint agentLoopConfirmKind = "checkpoint"
	agentLoopConfirmPermission agentLoopConfirmKind = "permission"
	agentLoopConfirmToolCall   agentLoopConfirmKind = "tool_call"
)

type agentLoopConfirmRequest struct {
	kind       agentLoopConfirmKind
	prompt     string
	iterations int
}

// loopCheckpointMsg is sent from the agent loop goroutine when it reaches a
// checkpoint interval or a tool policy requires confirmation. The TUI displays
// a prompt and sends the user's answer back on responseCh.
type loopCheckpointMsg struct {
	responseCh chan<- bool
	requestCh  <-chan agentLoopConfirmRequest // kept so we can re-listen after confirming
	request    agentLoopConfirmRequest
}

type tokenUsage struct {
	InputTokens           int `json:"input_tokens"`
	CachedInputTokens     int `json:"cached_input_tokens"`
	CacheWriteInputTokens int `json:"cache_write_input_tokens"`
	OutputTokens          int `json:"output_tokens"`
	Responses             int `json:"responses"`
}

func (u *tokenUsage) addResponse(resp *llm.Response) {
	if resp == nil {
		return
	}

	u.InputTokens += resp.InputTokens
	u.CachedInputTokens += resp.CachedInputTokens
	u.CacheWriteInputTokens += resp.CacheWriteInputTokens
	u.OutputTokens += resp.OutputTokens
	u.Responses++
}

func (u *tokenUsage) add(next tokenUsage) {
	u.InputTokens += next.InputTokens
	u.CachedInputTokens += next.CachedInputTokens
	u.CacheWriteInputTokens += next.CacheWriteInputTokens
	u.OutputTokens += next.OutputTokens
	u.Responses += next.Responses
}

//nolint:govet // Field order groups request concerns; padding is not performance-sensitive.
type llmRequest struct {
	eventBase                   events.Event
	hookRunner                  *events.Runner
	generation                  generationSettings
	agentLoopBudget             llm.AgentLoopBudget
	autonomy                    autonomy.Level
	permissionPolicy            *permission.Policy
	agentLoopCheckpointInterval int
	maxInputTokens              int
	routeDecision               *modelroute.Decision
	model                       string
	agentLoopCheckpointPath     string
	referenceContext            string
	workingDir                  string
	referenceManifest           contextref.ReferenceManifest
	messages                    []llm.Message
	fallbackModels              []string
	refs                        []contextref.Reference
	inlineReferenceEvents       []contextref.ReferenceEvent
	agent                       agent.Agent
	hasAgent                    bool
	useTools                    bool

	// confirmRequestCh is used by the agent loop to ask the caller whether to
	// continue at checkpoint intervals or execute require-confirm tool calls.
	// The agent loop goroutine sends the request and blocks until it receives a
	// boolean on confirmResponseCh.
	confirmRequestCh  chan agentLoopConfirmRequest
	confirmResponseCh chan bool
	liveCh            chan tea.Msg
}

type completionCandidate struct {
	label string
	value string
	kind  string
}

// ---------------------------------------------------------------------------
// Model
// ---------------------------------------------------------------------------

//nolint:govet,recvcheck // Field order groups related TUI state; task helpers mutate the local Bubble Tea model copy before returning it.
type model struct {
	ctx                  context.Context
	textarea             textarea.Model
	registry             *llm.Registry
	agentRegistry        *agent.Registry
	hookRunner           *events.Runner
	sessionStore         *session.Store
	stateStore           *appconfig.StateStore
	cancel               context.CancelFunc
	pickerCancel         context.CancelFunc
	idleSuggestionCancel context.CancelFunc
	pendingModel         pickerItem
	selectedModel        string
	selectedAgent        string
	sessionPath          string
	cwd                  string
	selectedProvider     string
	fallbackModels       []string

	generationDefaults          generationSettings
	generationOverrides         generationSettings
	agentLoopBudget             llm.AgentLoopBudget
	autonomy                    autonomy.Level
	agentLoopCheckpointInterval int

	sessionState              session.Session
	history                   []llm.Message
	promptHistory             []string
	queuedPrompts             []string
	promptHistoryDraft        string
	pickerItems               []pickerItem
	contextOptions            contextref.Options
	configuredReferences      []string
	referenceContext          string
	referenceManifest         contextref.ReferenceManifest
	referenceContextEstimator string
	vectorConfig              appconfig.VectorConfig
	skillLearningStoreDir     string
	skillLearningSkillDir     string
	worktreeInfo              *worktree.Info
	promptContextCache        *promptContextCache
	permissionPolicy          *permission.Policy
	tokenUsage                tokenUsage
	runningTaskStarted        time.Time
	idleSuggestionInput       string
	idleSuggestionText        string
	idleSuggestionStatus      string
	idleSuggestionProvider    string
	idleSuggestionModel       string
	idleSuggestionContext     string
	idleSuggestionUsage       tokenUsage
	idleSuggestionBudget      idleSuggestionBudget
	idleSuggestionTimes       []time.Time
	idleSuggestionCostUSD     float64
	promptContextStatus       string
	pickerCursor              int
	idleSuggestionRequests    int
	idleSuggestionTokens      int
	idleSuggestionID          int
	terminalTitleFrame        int
	modelFetchID              int
	modelFetchesPending       int
	completionCursor          int
	promptHistoryCursor       int
	runningTaskID             int
	maxInputTokens            int
	width                     int
	quitting                  bool
	waiting                   bool
	pickerOpen                bool
	pickerLoading             bool
	scopePickerOpen           bool
	completionOpen            bool
	modelLocked               bool
	promptLocalOnly           bool
	promptSuggestionConsent   promptSuggestionConsent
	skillLearningEnabled      bool
	revampUndoActive          bool
	completionItems           []completionCandidate
	runningTaskLabel          string
	revampUndo                string

	// checkpointResponseCh is non-nil when the TUI is waiting for the user to
	// confirm whether to continue the agent loop or execute a require-confirm
	// tool call. The Y/N key handler sends the answer and nils this field.
	checkpointResponseCh chan<- bool
	checkpointRequestCh  <-chan agentLoopConfirmRequest
	checkpointPrompt     string
	checkpointKind       agentLoopConfirmKind
	pinnedMessages       map[int]bool
	executionMode        string
}

func initialModel(
	ctx context.Context,
	reg *llm.Registry,
	agents *agent.Registry,
	hooks *events.Runner,
	store *session.Store,
	stateStore *appconfig.StateStore,
	sessionState session.Session,
	contextOptions contextref.Options,
	configuredReferences []string,
	referenceContext string,
	referenceManifest contextref.ReferenceManifest,
	referenceContextEstimator string,
	vectorConfig appconfig.VectorConfig,
	skillLearningStoreDir string,
	skillLearningSkillDir string,
	skillLearningEnabled bool,
	sessionPath string,
	cwd string,
	selectedModel string,
	selectedAgent string,
	fallbackModels []string,
	generationDefaults generationSettings,
	generationOverrides generationSettings,
	agentLoopBudget llm.AgentLoopBudget,
	autonomyLevel autonomy.Level,
	agentLoopCheckpointInterval int,
	maxInputTokens int,
	modelLocked bool,
	promptLocalOnly bool,
	promptSuggestionConsent promptSuggestionConsent,
	idleSuggestionBudget idleSuggestionBudget,
	wtInfo *worktree.Info,
	promptContextCache *promptContextCache,
	permissionPolicy *permission.Policy,
) model {
	ta := newPromptTextarea()
	selectedProvider, _ := reg.ProviderForModel(selectedModel)
	sessionState.AgentLoopBudget = agentLoopBudget
	sessionState.Autonomy = autonomy.Normalize(autonomyLevel).String()
	idleSuggestionUsage, idleSuggestionRequests, idleSuggestionTokens, idleSuggestionCostUSD := idleSuggestionBudgetStateFromSession(sessionState)

	return model{
		ctx:                         ctx,
		registry:                    reg,
		agentRegistry:               agents,
		hookRunner:                  hooks,
		sessionStore:                store,
		stateStore:                  stateStore,
		sessionState:                sessionState,
		contextOptions:              contextOptions,
		configuredReferences:        append([]string(nil), configuredReferences...),
		referenceContext:            referenceContext,
		referenceManifest:           referenceManifest,
		referenceContextEstimator:   referenceContextEstimator,
		vectorConfig:                vectorConfig,
		skillLearningStoreDir:       skillLearningStoreDir,
		skillLearningSkillDir:       skillLearningSkillDir,
		skillLearningEnabled:        skillLearningEnabled,
		sessionPath:                 sessionPath,
		cwd:                         cwd,
		selectedModel:               selectedModel,
		selectedAgent:               selectedAgent,
		selectedProvider:            selectedProvider,
		fallbackModels:              append([]string(nil), fallbackModels...),
		generationDefaults:          generationDefaults,
		generationOverrides:         generationOverrides,
		agentLoopBudget:             agentLoopBudget,
		autonomy:                    autonomy.Normalize(autonomyLevel),
		agentLoopCheckpointInterval: agentLoopCheckpointInterval,
		maxInputTokens:              maxInputTokens,
		history:                     append([]llm.Message(nil), sessionState.Messages...),
		promptHistory:               promptHistoryFromStore(ctx, store, sessionState, maxPromptHistoryEntries),
		promptHistoryCursor:         -1,
		textarea:                    ta,
		modelLocked:                 modelLocked,
		promptLocalOnly:             promptLocalOnly,
		promptSuggestionConsent:     promptSuggestionConsent,
		idleSuggestionUsage:         idleSuggestionUsage,
		idleSuggestionBudget:        normalizeIdleSuggestionBudget(idleSuggestionBudget),
		idleSuggestionCostUSD:       idleSuggestionCostUSD,
		idleSuggestionRequests:      idleSuggestionRequests,
		idleSuggestionTokens:        idleSuggestionTokens,
		worktreeInfo:                wtInfo,
		promptContextCache:          promptContextCache,
		permissionPolicy:            permissionPolicy,
		pinnedMessages:              make(map[int]bool),
		executionMode:               executionModeExecute,
	}
}
