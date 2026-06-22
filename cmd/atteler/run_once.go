package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/tommoulard/atteler/pkg/agent"
	"github.com/tommoulard/atteler/pkg/autonomy"
	appconfig "github.com/tommoulard/atteler/pkg/config"
	"github.com/tommoulard/atteler/pkg/contextref"
	"github.com/tommoulard/atteler/pkg/events"
	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/modelroute"
	"github.com/tommoulard/atteler/pkg/permission"
	"github.com/tommoulard/atteler/pkg/session"
	attshell "github.com/tommoulard/atteler/pkg/shell"
)

const (
	headlessHeartbeatInterval = 15 * time.Second
	headlessParentRunIDEnv    = "ATTELER_HEADLESS_PARENT_ID"
	headlessRetryOfRunIDEnv   = "ATTELER_HEADLESS_RETRY_OF_ID"
	headlessRetryCountEnv     = "ATTELER_HEADLESS_RETRY_COUNT"

	agentLoopConfigFieldMaxInputTokens  = "agent_loop.max_input_tokens"  // #nosec G101 -- config field path, not a credential.
	agentLoopConfigFieldMaxOutputTokens = "agent_loop.max_output_tokens" // #nosec G101 -- config field path, not a credential.
	agentLoopConfigFieldMaxTotalTokens  = "agent_loop.max_total_tokens"  // #nosec G101 -- config field path, not a credential.
)

type responseRecordOptions struct {
	RecordPath string
	ReplayPath string
}

//nolint:govet // Field order follows CLI execution concerns, not memory packing.
type runOnceExecutionOptions struct {
	OutputFormat                string
	HeadlessID                  string
	SkillLearningStoreDir       string
	SkillLearningSkillDir       string
	VectorConfig                appconfig.VectorConfig
	PermissionPolicy            *permission.Policy
	Response                    responseRecordOptions
	AgentLoopBudget             llm.AgentLoopBudget
	Autonomy                    autonomy.Level
	AgentLoopCheckpointInterval int
	Headless                    bool
	HeadlessPrivateLog          bool
	SkillLearningEnabled        bool
}

//nolint:govet // JSON output order favors user-facing readability over pointer packing.
type runOnceResult struct {
	SessionID               string              `json:"session_id"`
	SessionPath             string              `json:"session_path"`
	SessionPersisted        bool                `json:"session_persisted"`
	AgentLoopCheckpointPath string              `json:"agent_loop_checkpoint_path,omitempty"`
	AgentLoopBudget         llm.AgentLoopBudget `json:"agent_loop_budget,omitzero"`
	Autonomy                string              `json:"autonomy"`
	HeadlessID              string              `json:"headless_id,omitempty"`
	Agent                   string              `json:"agent,omitempty"`
	Model                   string              `json:"model,omitempty"`
	ModelMode               string              `json:"model_mode,omitempty"`
	Content                 string              `json:"content"`
	TokenUsage              tokenUsage          `json:"token_usage"`
}

//nolint:govet // Field order follows request-preparation flow; padding is irrelevant here.
type runOncePrepared struct {
	activeAgent           agentSelection
	generation            generationSettings
	requestMessages       []llm.Message
	refs                  []contextref.Reference
	inlineReferenceEvents []contextref.ReferenceEvent
	routeDecision         *modelroute.Decision
	fallbackModels        []string
	prompt                string
	requestModel          string
}

type responseRecordFile struct {
	RecordedAt time.Time             `json:"recorded_at"`
	Request    responseRecordRequest `json:"request"`
	Response   responseRecordPayload `json:"response"`
}

//nolint:govet // JSON field order is grouped for stable recording readability.
type responseRecordRequest struct {
	Temperature    *float64            `json:"temperature,omitempty"`
	TopP           *float64            `json:"top_p,omitempty"`
	Seed           *int                `json:"seed,omitempty"`
	ResponseFormat *llm.ResponseFormat `json:"response_format,omitempty"`
	Model          string              `json:"model,omitempty"`
	ModelMode      string              `json:"model_mode,omitempty"`
	FallbackModels []string            `json:"fallback_models,omitempty"`
	Messages       []llm.Message       `json:"messages"`
	MaxTokens      int                 `json:"max_tokens,omitempty"`
	ReasoningLevel string              `json:"reasoning_level,omitempty"`
}

type responseRecordPayload struct {
	Content               string `json:"content"`
	Provider              string `json:"provider,omitempty"`
	Model                 string `json:"model,omitempty"`
	LatencyMS             int    `json:"latency_ms,omitempty"`
	FirstTokenLatencyMS   int    `json:"first_token_latency_ms,omitempty"`
	InputTokens           int    `json:"input_tokens,omitempty"`
	CachedInputTokens     int    `json:"cached_input_tokens,omitempty"`
	CacheWriteInputTokens int    `json:"cache_write_input_tokens,omitempty"`
	OutputTokens          int    `json:"output_tokens,omitempty"`
}

func saveRecordedResponse(ctx context.Context, path string, params llm.CompleteParams, fallbackModels []string, resp *llm.Response) error {
	if strings.TrimSpace(path) == "" || resp == nil {
		return nil
	}

	if err := authorizeWritePermission(ctx, "record one-shot response", "atteler.eval.record_response", path); err != nil {
		return fmt.Errorf("record response: %w", err)
	}

	record := responseRecordFile{
		RecordedAt: time.Now().UTC(),
		Request: responseRecordRequest{
			Model:          params.Model,
			ModelMode:      params.ModelMode,
			Messages:       append([]llm.Message(nil), params.Messages...),
			FallbackModels: append([]string(nil), fallbackModels...),
			MaxTokens:      params.MaxTokens,
			Temperature:    params.Temperature,
			TopP:           params.TopP,
			Seed:           params.Seed,
			ResponseFormat: cloneResponseFormat(params.ResponseFormat),
			ReasoningLevel: params.ReasoningLevel,
		},
		Response: responseRecordPayload{
			Content:               resp.Content,
			Provider:              resp.Provider,
			Model:                 resp.Model,
			LatencyMS:             responseRecordDurationMS(resp.Latency),
			FirstTokenLatencyMS:   responseRecordDurationMS(resp.FirstTokenLatency),
			InputTokens:           resp.InputTokens,
			CachedInputTokens:     resp.CachedInputTokens,
			CacheWriteInputTokens: resp.CacheWriteInputTokens,
			OutputTokens:          resp.OutputTokens,
		},
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil && filepath.Dir(path) != "." {
		return fmt.Errorf("record response: create dir: %w", err)
	}

	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("record response: marshal: %w", err)
	}

	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("record response: write %s: %w", path, err)
	}

	return nil
}

func cloneResponseFormat(format *llm.ResponseFormat) *llm.ResponseFormat {
	if format == nil {
		return nil
	}

	clone := *format
	if format.Schema != nil {
		clone.Schema = make(map[string]any, len(format.Schema))
		maps.Copy(clone.Schema, format.Schema)
	}

	return &clone
}

func loadRecordedResponse(ctx context.Context, path string) (*llm.Response, error) {
	if err := authorizeReadPermission(ctx, "replay one-shot response", "atteler.eval.replay_response", path); err != nil {
		return nil, fmt.Errorf("replay response: %w", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("replay response: read %s: %w", path, err)
	}

	var record responseRecordFile
	if err := json.Unmarshal(data, &record); err != nil {
		return nil, fmt.Errorf("replay response: parse %s: %w", path, err)
	}

	if strings.TrimSpace(record.Response.Content) == "" {
		return nil, fmt.Errorf("replay response: %s has empty response content", path)
	}

	return &llm.Response{
		Content:               record.Response.Content,
		Provider:              record.Response.Provider,
		Model:                 record.Response.Model,
		Latency:               responseRecordDuration(record.Response.LatencyMS),
		FirstTokenLatency:     responseRecordDuration(record.Response.FirstTokenLatencyMS),
		InputTokens:           record.Response.InputTokens,
		CachedInputTokens:     record.Response.CachedInputTokens,
		CacheWriteInputTokens: record.Response.CacheWriteInputTokens,
		OutputTokens:          record.Response.OutputTokens,
	}, nil
}

func responseRecordDurationMS(duration time.Duration) int {
	if duration <= 0 {
		return 0
	}

	return int(duration / time.Millisecond)
}

func responseRecordDuration(ms int) time.Duration {
	if ms <= 0 {
		return 0
	}

	return time.Duration(ms) * time.Millisecond
}

func runOnce(
	ctx context.Context,
	reg *llm.Registry,
	agents *agent.Registry,
	hooks *events.Runner,
	store *session.Store,
	sessionState session.Session,
	contextOptions contextref.Options,
	referenceContext string,
	referenceManifest contextref.ReferenceManifest,
	referenceContextEstimator string,
	configuredReferences []string,
	selectedModel string,
	selectedAgent string,
	fallbackModels []string,
	generationDefaults generationSettings,
	generationOverrides generationSettings,
	maxInputTokens int,
	responseOptions responseRecordOptions,
	modelLocked bool,
	prompt string,
) error {
	return runOnceWithOptions(
		ctx,
		reg,
		agents,
		hooks,
		store,
		sessionState,
		contextOptions,
		referenceContext,
		referenceManifest,
		referenceContextEstimator,
		configuredReferences,
		selectedModel,
		selectedAgent,
		fallbackModels,
		generationDefaults,
		generationOverrides,
		maxInputTokens,
		runOnceExecutionOptions{Response: responseOptions},
		modelLocked,
		prompt,
	)
}

func prepareRunOnceExecutionOptions(
	ctx context.Context,
	store *session.Store,
	options runOnceExecutionOptions,
	sessionState session.Session,
	prompt string,
	selectedModel string,
	selectedAgent string,
	agents *agent.Registry,
	generationDefaults generationSettings,
	generationOverrides generationSettings,
) (runOnceExecutionOptions, string, error) {
	options.Autonomy = autonomy.Normalize(options.Autonomy)
	modelMode := headlessPreflightModelMode(generationDefaults, generationOverrides, selectedAgent, agents)

	if err := validateRunOnceAutonomyOptions(options); err != nil {
		recordHeadlessPreflightFailure(ctx, store, options, sessionState, prompt, selectedModel, modelMode, selectedAgent, err)

		return options, "", err
	}

	outputFormat, err := normalizeOutputFormat(options.OutputFormat)
	if err != nil {
		recordHeadlessPreflightFailure(ctx, store, options, sessionState, prompt, selectedModel, modelMode, selectedAgent, err)

		return options, "", err
	}

	return options, outputFormat, nil
}

func runOnceWithOptions(
	ctx context.Context,
	reg *llm.Registry,
	agents *agent.Registry,
	hooks *events.Runner,
	store *session.Store,
	sessionState session.Session,
	contextOptions contextref.Options,
	referenceContext string,
	referenceManifest contextref.ReferenceManifest,
	referenceContextEstimator string,
	configuredReferences []string,
	selectedModel string,
	selectedAgent string,
	fallbackModels []string,
	generationDefaults generationSettings,
	generationOverrides generationSettings,
	maxInputTokens int,
	executionOptions runOnceExecutionOptions,
	modelLocked bool,
	prompt string,
) error {
	executionOptions, outputFormat, err := prepareRunOnceExecutionOptions(
		ctx,
		store,
		executionOptions,
		sessionState,
		prompt,
		selectedModel,
		selectedAgent,
		agents,
		generationDefaults,
		generationOverrides,
	)
	if err != nil {
		recordHeadlessPreflightFailure(
			ctx,
			store,
			executionOptions,
			sessionState,
			prompt,
			selectedModel,
			headlessPreflightModelMode(generationDefaults, generationOverrides, selectedAgent, agents),
			selectedAgent,
			err,
		)

		return err
	}

	prepared, err := prepareRunOnceRequest(
		ctx,
		reg,
		agents,
		contextOptions,
		referenceContext,
		selectedModel,
		selectedAgent,
		fallbackModels,
		recentAgentNamesForSelection(selectedAgent, sessionState),
		generationDefaults,
		generationOverrides,
		modelLocked,
		executionOptions.Autonomy.AllowsAgentTools(),
		prompt,
	)
	if err != nil {
		if len(prepared.inlineReferenceEvents) > 0 {
			return handleRunOncePrepareError(ctx, hooks, reg, store, sessionState, prepared, referenceManifest, maxInputTokens, executionOptions, err)
		}

		modelMode := prepared.generation.ModelMode
		if modelMode == "" {
			modelMode = headlessPreflightModelMode(generationDefaults, generationOverrides, selectedAgent, agents)
		}

		recordHeadlessPreflightFailure(ctx, store, executionOptions, sessionState, prompt, selectedModel, modelMode, selectedAgent, err)
		emitRouteDecisionWarning(
			ctx,
			hooks,
			sessionState.ID,
			store.Path(sessionState.ID),
			prepared.activeAgent.name,
			"",
			prepared.routeDecision,
		)

		return err
	}

	applyRunOnceSessionDefaults(&sessionState, prepared, generationOverrides)
	sessionState.DefaultAgent = prepared.activeAgent.name
	sessionState.AgentLoopBudget = executionOptions.AgentLoopBudget
	sessionState.Autonomy = executionOptions.Autonomy.String()

	runMetadata := sessionRunEventMetadata(
		executionOptions.AgentLoopBudget,
		executionOptions.Autonomy,
		prepared.generation.ReasoningLevel,
		prepared.generation.ModelMode,
	)

	emitHookWarning(ctx, hooks, events.Event{
		Type:        events.SessionStart,
		SessionID:   sessionState.ID,
		SessionPath: store.Path(sessionState.ID),
		Agent:       prepared.activeAgent.name,
		Model:       sessionState.DefaultModel,
		Metadata:    runMetadata,
	})
	defer emitHookWarning(ctx, hooks, events.Event{
		Type:        events.SessionEnd,
		SessionID:   sessionState.ID,
		SessionPath: store.Path(sessionState.ID),
		Agent:       prepared.activeAgent.name,
		Model:       sessionState.DefaultModel,
		Metadata:    runMetadata,
	})

	params := llm.CompleteParams{
		Model:    prepared.requestModel,
		Messages: append(append([]llm.Message(nil), sessionState.Messages...), prepared.requestMessages...),
	}
	if prepared.activeAgent.ok {
		params = prepared.activeAgent.agent.CompleteParams(prepared.requestModel, params.Messages)
	}

	requestContextOptions := contextOptionsForRequestModels(contextOptions, reg, prepared.requestModel, prepared.fallbackModels)
	globalRefCtx := configuredReferenceContextForRunOnce(ctx, configuredReferences, referenceContext, referenceManifest, referenceContextEstimator, requestContextOptions)
	refCtx := buildReferenceContextWithManifest(ctx, globalRefCtx, prepared.activeAgent, requestContextOptions)
	generatedSkillRefCtx := generatedSkillReferenceContextWithManifest(
		prepared.prompt,
		executionOptions.SkillLearningStoreDir,
		executionOptions.SkillLearningSkillDir,
		executionOptions.SkillLearningEnabled,
		requestContextOptions,
	)
	refCtx.Content = appendReferenceContext(refCtx.Content, generatedSkillRefCtx.Content)

	refCtx.Manifest = mergeReferenceManifests(refCtx.Manifest, generatedSkillRefCtx.Manifest)
	if refCtx.Estimator == "" {
		refCtx.Estimator = generatedSkillRefCtx.Estimator
	}

	refCtx = appendWorkspaceVectorReferenceContextForAutonomy(
		ctx,
		refCtx,
		executionOptions.Autonomy,
		contextOptions.Root,
		executionOptions.VectorConfig,
		prepared.prompt,
		true,
		requestContextOptions,
	)

	prependReferenceContext(&params, refCtx.Content)
	prependAutonomyInstructions(&params, executionOptions.Autonomy)

	applyGenerationParams(&params, prepared.generation)

	// Enable tool use for one-shot calls: the LLM can invoke built-in tools.
	// Apply agent-level tool filtering when an agent is active.
	var tools []llm.ToolDefinition
	if executionOptions.Autonomy.AllowsAgentTools() {
		tools = defaultToolsForAgent(prepared.activeAgent)
		params.Tools = tools
	} else {
		params.Tools = nil
	}

	if len(params.Tools) > 0 {
		prependToolReminder(&params, tools)
	}

	manifestEvent := requestContextManifestEvent(newRequestContextManifestForModelsWithInlineEvents(reg, params.Model, prepared.fallbackModels, params.Messages, maxInputTokens, prepared.inlineReferenceEvents, refCtx.Manifest))
	manifestEvent.SessionID = sessionState.ID
	manifestEvent.SessionPath = store.Path(sessionState.ID)
	manifestEvent.Agent = prepared.activeAgent.name
	setExplicitContextManifestEventModel(&manifestEvent, params.Model)
	emitHookWarning(ctx, hooks, manifestEvent)

	ctx = events.WithEmitter(ctx, hooks, events.Event{
		SessionID:   sessionState.ID,
		SessionPath: store.Path(sessionState.ID),
		Agent:       prepared.activeAgent.name,
		Model:       prepared.requestModel,
	})
	ctx = contextWithPermissionAuditMetadata(ctx, store, sessionState, prepared.activeAgent.name, prepared.requestModel)

	headlessRun, err := startHeadlessRun(
		ctx,
		store,
		executionOptions,
		sessionState,
		prepared.prompt,
		prepared.requestModel,
		params.ModelMode,
		prepared.activeAgent.name,
		manifestEvent.Metadata["context_manifest"],
	)
	if err != nil {
		return err
	}

	stopHeadlessHeartbeat := startHeadlessHeartbeat(ctx, store, headlessRun)
	defer stopHeadlessHeartbeat()

	if userSaveErr := saveRunOnceUserMessage(ctx, hooks, store, &sessionState, prepared); userSaveErr != nil {
		finishHeadlessRun(store, headlessRun, session.HeadlessStatusFailed, userSaveErr.Error())

		return userSaveErr
	}

	if budgetErr := validateRequestBudgetWithFallbacks(reg, params.Model, prepared.fallbackModels, params.Messages, maxInputTokens); budgetErr != nil {
		emitHookWarning(ctx, hooks, events.Event{
			Type:        events.Error,
			SessionID:   sessionState.ID,
			SessionPath: store.Path(sessionState.ID),
			Agent:       prepared.activeAgent.name,
			Model:       sessionState.DefaultModel,
			Error:       budgetErr.Error(),
		})
		finishHeadlessRun(store, headlessRun, session.HeadlessStatusFailed, budgetErr.Error())

		return budgetErr
	}

	slog.Debug("one-shot LLM request",
		"agent", prepared.activeAgent.name,
		"model", params.Model,
		"model_mode", params.ModelMode,
		"reasoning_level", params.ReasoningLevel,
		"autonomy", executionOptions.Autonomy.String(),
		"tools", len(params.Tools),
		"messages", len(params.Messages),
	)

	checkpointPath := agentLoopCheckpointPathForAutonomy(store.Path(sessionState.ID), executionOptions.Autonomy)
	if executionOptions.Response.ReplayPath != "" {
		checkpointPath = ""
	}

	agentLoopPreflight := runOnceAgentLoopManifestPreflight(ctx, hooks, reg, store, headlessRun, store.Path(sessionState.ID), sessionState.ID, prepared.activeAgent.name, prepared.fallbackModels, prepared.inlineReferenceEvents, refCtx.Manifest, maxInputTokens)

	resp, providerRequestMessages, err := runOnceCompleteWithAutonomyTranscript(
		ctx,
		reg,
		params,
		prepared.fallbackModels,
		executionOptions.AgentLoopBudget,
		executionOptions.Autonomy,
		executionOptions.AgentLoopCheckpointInterval,
		executionOptions.Response,
		executionOptions.PermissionPolicy,
		!executionOptions.Headless,
		checkpointPath,
		agentLoopPreflight,
		attshell.AuditContext{
			Caller:      "atteler.once.llm_tool",
			SessionID:   sessionState.ID,
			SessionPath: store.Path(sessionState.ID),
			Autonomy:    executionOptions.Autonomy.String(),
		},
	)
	if err != nil {
		providerFailureMetadata := llm.ProviderFailureMetadata(err)

		emitHookWarning(ctx, hooks, events.Event{
			Type:        events.Error,
			SessionID:   sessionState.ID,
			SessionPath: store.Path(sessionState.ID),
			Agent:       prepared.activeAgent.name,
			Model:       sessionState.DefaultModel,
			Error:       err.Error(),
			Metadata:    providerFailureMetadata,
		})
		finishHeadlessRun(store, headlessRun, session.HeadlessStatusFailed, err.Error(), providerFailureMetadata)

		return fmt.Errorf("one-shot complete: %w", err)
	}

	providerReferenceManifest := refCtx.Manifest
	if len(prepared.inlineReferenceEvents) > 0 {
		providerReferenceManifest = mergeReferenceManifests(
			refCtx.Manifest,
			contextref.BuildReferenceManifest(prepared.inlineReferenceEvents),
		)
	}

	return finishRunOnceSuccess(
		ctx,
		hooks,
		store,
		&sessionState,
		prepared.activeAgent.name,
		params.ModelMode,
		params,
		prepared.fallbackModels,
		providerRequestMessages,
		providerReferenceManifest,
		resp,
		checkpointPath,
		outputFormat,
		executionOptions,
		headlessRun,
		prepared.routeDecision,
		routeTelemetryFromRegistry(reg),
	)
}

func finishRunOnceSuccess(
	ctx context.Context,
	hooks *events.Runner,
	store *session.Store,
	sessionState *session.Session,
	agentName string,
	modelMode string,
	params llm.CompleteParams,
	fallbackModels []string,
	providerRequestMessages []llm.Message,
	referenceManifest contextref.ReferenceManifest,
	resp *llm.Response,
	checkpointPath string,
	outputFormat string,
	executionOptions runOnceExecutionOptions,
	headlessRun *session.HeadlessRun,
	routeDecision *modelroute.Decision,
	routeTelemetry *modelroute.Telemetry,
) error {
	if err := ensureHeadlessRunCanRecordResponse(store, headlessRun); err != nil {
		return err
	}

	providerParams := params
	if len(providerRequestMessages) > 0 {
		providerParams.Messages = append([]llm.Message(nil), providerRequestMessages...)
	}

	providerCall := session.NewProviderCall(session.ProviderCallRecord{
		CompletedAt:     time.Now().UTC(),
		Source:          "run_once",
		Params:          providerParams,
		Response:        resp,
		FallbackModels:  fallbackModels,
		ReferencedFiles: sessionFileReferencesFromManifest(referenceManifest, agentName),
	})

	if err := saveRunOnceAssistantResponse(ctx, hooks, store, sessionState, agentName, resp, providerCall); err != nil {
		finishHeadlessRun(store, headlessRun, session.HeadlessStatusFailed, err.Error())
		return err
	}

	emitRouteDecisionWarning(
		ctx,
		hooks,
		sessionState.ID,
		store.Path(sessionState.ID),
		agentName,
		routeResponseModelID(resp.Provider, resp.Model),
		routeDecisionWithResponse(routeDecision, resp, routeTelemetry),
	)

	var usage tokenUsage
	usage.addResponse(resp)

	result := runOnceResult{
		SessionID:               sessionState.ID,
		SessionPath:             store.Path(sessionState.ID),
		SessionPersisted:        sessionPersistenceAllowed(*sessionState),
		AgentLoopCheckpointPath: checkpointPath,
		AgentLoopBudget:         executionOptions.AgentLoopBudget,
		Autonomy:                executionOptions.Autonomy.String(),
		Agent:                   agentName,
		Model:                   resp.Model,
		ModelMode:               modelMode,
		Content:                 resp.Content,
		TokenUsage:              usage,
	}
	if headlessRun != nil {
		result.HeadlessID = headlessRun.ID
		if resp.Model != "" {
			headlessRun.Model = resp.Model
		}

		recordHeadlessAssistantMessage(store, headlessRun, len(resp.Content), resp.ProviderFailureMetadata())
		finishHeadlessRun(store, headlessRun, session.HeadlessStatusCompleted, "")

		if err := headlessCompletionError(headlessRun); err != nil {
			return err
		}
	}

	return writeRunOnceResult(os.Stdout, os.Stderr, result, outputFormat, executionOptions.Headless)
}

func applyRunOnceSessionDefaults(sessionState *session.Session, prepared runOncePrepared, generationOverrides generationSettings) {
	if prepared.requestModel != "" {
		sessionState.DefaultModel = prepared.requestModel
	}

	if level := strings.TrimSpace(generationOverrides.ReasoningLevel); level != "" {
		sessionState.DefaultReasoningLevel = level
	}

	if mode := strings.TrimSpace(generationOverrides.ModelMode); mode != "" {
		sessionState.DefaultModelMode = mode
	}
}

func headlessPreflightModelMode(
	generationDefaults generationSettings,
	generationOverrides generationSettings,
	selectedAgent string,
	agents *agent.Registry,
) string {
	var activeAgent agentSelection

	if selectedAgent != "" && agents != nil {
		if configuredAgent, ok := agents.Get(selectedAgent); ok {
			activeAgent = agentSelection{name: selectedAgent, agent: configuredAgent, ok: true}
		}
	}

	return generationForRequest(generationDefaults, generationOverrides, activeAgent).ModelMode
}

func handleRunOncePrepareError(
	ctx context.Context,
	hooks *events.Runner,
	reg *llm.Registry,
	store *session.Store,
	sessionState session.Session,
	prepared runOncePrepared,
	referenceManifest contextref.ReferenceManifest,
	maxInputTokens int,
	executionOptions runOnceExecutionOptions,
	prepareErr error,
) error {
	manifestEvent, ok := runOncePrepareContextManifestEvent(reg, store, sessionState, prepared, referenceManifest, maxInputTokens)
	if !ok {
		return prepareErr
	}

	emitHookWarning(ctx, hooks, manifestEvent)

	headlessRun, headlessErr := startHeadlessRun(
		ctx,
		store,
		executionOptions,
		sessionState,
		prepared.prompt,
		prepared.requestModel,
		prepared.generation.ModelMode,
		prepared.activeAgent.name,
		manifestEvent.Metadata["context_manifest"],
	)
	if headlessErr != nil {
		return errors.Join(prepareErr, headlessErr)
	}

	finishHeadlessRun(store, headlessRun, session.HeadlessStatusFailed, prepareErr.Error())

	return prepareErr
}

func runOncePrepareContextManifestEvent(
	reg *llm.Registry,
	store *session.Store,
	sessionState session.Session,
	prepared runOncePrepared,
	referenceManifest contextref.ReferenceManifest,
	maxInputTokens int,
) (events.Event, bool) {
	if len(prepared.inlineReferenceEvents) == 0 {
		return events.Event{}, false
	}

	referenceManifest = omitIncludedReferenceManifestEntries(referenceManifest, "request assembly aborted before configured reference context was sent")

	prompt := prepared.prompt
	if strings.TrimSpace(prompt) == "" && len(prepared.requestMessages) > 0 {
		prompt = prepared.requestMessages[len(prepared.requestMessages)-1].Content
	}

	manifestEvent := requestContextManifestEvent(newRequestContextManifestForModelsWithInlineEvents(
		reg,
		prepared.requestModel,
		prepared.fallbackModels,
		[]llm.Message{{Role: llm.RoleUser, Content: prompt}},
		maxInputTokens,
		prepared.inlineReferenceEvents,
		referenceManifest,
	))
	manifestEvent.SessionID = sessionState.ID

	if store != nil {
		manifestEvent.SessionPath = store.Path(sessionState.ID)
	}

	manifestEvent.Agent = prepared.activeAgent.name
	setExplicitContextManifestEventModel(&manifestEvent, prepared.requestModel)

	return manifestEvent, true
}

func configuredReferenceContextForRunOnce(
	ctx context.Context,
	configuredReferences []string,
	referenceContext string,
	referenceManifest contextref.ReferenceManifest,
	referenceContextEstimator string,
	contextOptions contextref.Options,
) configuredReferenceContext {
	if referenceContextEstimator == "" {
		referenceContextEstimator = referenceManifest.TokenEstimator
	}

	return configuredReferenceContextForRequest(ctx, configuredReferences, configuredReferenceContext{
		Content:   referenceContext,
		Manifest:  referenceManifest,
		Estimator: referenceContextEstimator,
	}, contextOptions)
}

func sessionFileReferencesFromManifest(manifest contextref.ReferenceManifest, agentName string) []session.FileReference {
	if len(manifest.Entries) == 0 {
		return nil
	}

	refs := make([]session.FileReference, 0, manifest.IncludedCount)
	for index := range manifest.Entries {
		entry := &manifest.Entries[index]

		switch entry.PolicyDecision {
		case contextref.ReferenceDecisionLoaded, contextref.ReferenceDecisionTruncated:
		default:
			continue
		}

		path := strings.TrimSpace(entry.ResolvedSource)
		if path == "" {
			path = strings.TrimSpace(entry.Source)
		}

		if path == "" {
			continue
		}

		refs = append(refs, session.FileReference{
			Path:        path,
			LogicalPath: strings.TrimSpace(entry.Source),
			Kind:        strings.TrimSpace(entry.Kind),
			Source:      "context_reference",
			SourceAgent: strings.TrimSpace(agentName),
			SHA256:      strings.TrimSpace(entry.DigestSHA256),
			SizeBytes:   int64(entry.Bytes),
		})
	}

	return refs
}

func prepareRunOnceRequest(
	ctx context.Context,
	reg *llm.Registry,
	agents *agent.Registry,
	contextOptions contextref.Options,
	referenceContext string,
	selectedModel string,
	selectedAgent string,
	fallbackModels []string,
	recentAgentNames []string,
	generationDefaults generationSettings,
	generationOverrides generationSettings,
	modelLocked bool,
	useTools bool,
	prompt string,
) (runOncePrepared, error) {
	activeAgent, userPrompt, err := resolveAgent(agents, selectedAgent, prompt, recentAgentNames)
	if err != nil {
		return runOncePrepared{}, err
	}

	if activeAgent.notice != "" {
		fmt.Fprintln(os.Stderr, "warning: "+activeAgent.notice)
	}

	requestModel := selectedModel

	selectedFallbackModels := append([]string(nil), fallbackModels...)
	if activeAgent.ok && !modelLocked {
		requestModel, fallbackModels = effectiveAgentModelSelection(selectedModel, fallbackModels, activeAgent)
	}

	contextOptions = contextOptionsForRequestModels(contextOptions, reg, requestModel, fallbackModels)

	requestMessages, refs, inlineEvents, err := expandReferences([]llm.Message{{Role: llm.RoleUser, Content: userPrompt}}, contextOptions)
	if err != nil {
		return runOncePrepared{
			activeAgent:           activeAgent,
			generation:            generationForRequest(generationDefaults, generationOverrides, activeAgent),
			inlineReferenceEvents: inlineEvents,
			fallbackModels:        fallbackModels,
			prompt:                userPrompt,
			requestModel:          requestModel,
		}, err
	}

	generation := generationForRequest(generationDefaults, generationOverrides, activeAgent)
	requestReferenceContext := buildReferenceContextWithManifest(ctx, configuredReferenceContext{
		Content:   referenceContext,
		Estimator: estimatorSummaryForContextOptions(contextOptions),
	}, activeAgent, contextOptions)
	budgetMessages := requestMessagesForBudget(requestModel, requestMessages, activeAgent, generation, requestReferenceContext.Content, useTools)

	requestModel, fallbackModels, routeDecision, err := requestModelRoutingAndFallbacks(
		ctx,
		reg,
		selectedModel,
		modelLocked,
		selectedFallbackModels,
		activeAgent,
		requestModel,
		fallbackModels,
		routeCompleteParamsForRequest(requestModel, budgetMessages, generation, activeAgent, useTools),
		routeProfileForMessages(budgetMessages, generation),
		routeTelemetryFromRegistry(reg),
		routeAvailabilityFromRegistryWithRefresh(ctx, reg, effectiveRouteCandidateChain(selectedModel, selectedFallbackModels, activeAgent, modelLocked)),
	)
	if err != nil {
		return runOncePrepared{
			activeAgent:           activeAgent,
			generation:            generation,
			inlineReferenceEvents: inlineEvents,
			routeDecision:         routeDecision,
			fallbackModels:        fallbackModels,
			prompt:                userPrompt,
			requestModel:          requestModel,
		}, err
	}

	return runOncePrepared{
		activeAgent:           activeAgent,
		generation:            generation,
		requestMessages:       requestMessages,
		refs:                  refs,
		inlineReferenceEvents: inlineEvents,
		routeDecision:         routeDecision,
		fallbackModels:        fallbackModels,
		prompt:                userPrompt,
		requestModel:          requestModel,
	}, nil
}

func saveRunOnceUserMessage(
	ctx context.Context,
	hooks *events.Runner,
	store *session.Store,
	sessionState *session.Session,
	prepared runOncePrepared,
) error {
	if err := authorizeSessionStoreWrite(ctx, store, *sessionState, "save user session message"); err != nil {
		return fmt.Errorf("save session before request: %w", err)
	}

	sessionState.Append(llm.RoleUser, prepared.prompt)

	if sessionPersistenceAllowed(*sessionState) {
		if err := store.Save(*sessionState); err != nil {
			emitHookWarning(ctx, hooks, events.Event{
				Type:        events.Error,
				SessionID:   sessionState.ID,
				SessionPath: store.Path(sessionState.ID),
				Agent:       prepared.activeAgent.name,
				Model:       sessionState.DefaultModel,
				Error:       err.Error(),
			})

			return fmt.Errorf("save session before request: %w", err)
		}

		emitFileWriteWarning(ctx, hooks, *sessionState, store.Path(sessionState.ID), prepared.activeAgent.name, "session")
	}

	for _, ref := range prepared.refs {
		emitHookWarning(ctx, hooks, fileReadEvent(sessionState.ID, store.Path(sessionState.ID), prepared.activeAgent.name, sessionState.DefaultModel, ref))
		emitHookWarning(ctx, hooks, contextAddEvent(sessionState.ID, store.Path(sessionState.ID), prepared.activeAgent.name, sessionState.DefaultModel, ref))
	}

	if prepared.activeAgent.ok {
		emitHookWarning(ctx, hooks, events.Event{
			Type:        events.AgentExecute,
			SessionID:   sessionState.ID,
			SessionPath: store.Path(sessionState.ID),
			Agent:       prepared.activeAgent.name,
			Model:       sessionState.DefaultModel,
			Metadata: map[string]string{
				"agent": prepared.activeAgent.name,
			},
		})
	}

	emitRouteDecisionWarning(ctx, hooks, sessionState.ID, store.Path(sessionState.ID), prepared.activeAgent.name, sessionState.DefaultModel, prepared.routeDecision)

	emitHookWarning(ctx, hooks, events.Event{
		Type:        events.UserMessage,
		SessionID:   sessionState.ID,
		SessionPath: store.Path(sessionState.ID),
		Agent:       prepared.activeAgent.name,
		Model:       sessionState.DefaultModel,
		Role:        string(llm.RoleUser),
		Content:     prepared.prompt,
		Metadata:    referenceMetadata(prepared.refs),
	})

	if len(prepared.refs) > 0 {
		fmt.Fprintln(os.Stderr, "context: "+referenceSummary(prepared.refs))
	}

	return nil
}

func saveRunOnceAssistantResponse(
	ctx context.Context,
	hooks *events.Runner,
	store *session.Store,
	sessionState *session.Session,
	agentName string,
	resp *llm.Response,
	providerCall session.ProviderCall,
) error {
	if err := authorizeSessionStoreWrite(ctx, store, *sessionState, "save assistant session response"); err != nil {
		return fmt.Errorf("save session after response: %w", err)
	}

	sessionState.Append(llm.RoleAssistant, resp.Content)
	sessionState.RecordProviderCall(providerCall)

	if resp.Model != "" {
		sessionState.DefaultModel = resp.Model
	}

	if sessionPersistenceAllowed(*sessionState) {
		if err := store.Save(*sessionState); err != nil {
			emitHookWarning(ctx, hooks, events.Event{
				Type:        events.Error,
				SessionID:   sessionState.ID,
				SessionPath: store.Path(sessionState.ID),
				Agent:       agentName,
				Model:       sessionState.DefaultModel,
				Error:       err.Error(),
			})

			return fmt.Errorf("save session after response: %w", err)
		}

		emitFileWriteWarning(ctx, hooks, *sessionState, store.Path(sessionState.ID), agentName, "session")
	}

	emitHookWarning(ctx, hooks, events.Event{
		Type:        events.AssistantMessage,
		SessionID:   sessionState.ID,
		SessionPath: store.Path(sessionState.ID),
		Agent:       agentName,
		Model:       resp.Model,
		Role:        string(llm.RoleAssistant),
		Content:     resp.Content,
		Metadata:    resp.ProviderFailureMetadata(),
	})

	return nil
}

func validateRunOnceAutonomyOptions(options runOnceExecutionOptions) error {
	if options.Headless &&
		!autonomy.Normalize(options.Autonomy).Allows(autonomy.ActionFileWrite) {
		return fmt.Errorf("%s", autonomy.DenialMessage(options.Autonomy, autonomy.ActionFileWrite, "--headless run metadata"))
	}

	if strings.TrimSpace(options.Response.RecordPath) != "" &&
		!autonomy.Normalize(options.Autonomy).Allows(autonomy.ActionFileWrite) {
		return fmt.Errorf("%s", autonomy.DenialMessage(options.Autonomy, autonomy.ActionFileWrite, "--record-response"))
	}

	return nil
}

func recordHeadlessPreflightFailure(
	ctx context.Context,
	store *session.Store,
	options runOnceExecutionOptions,
	sessionState session.Session,
	prompt string,
	modelName string,
	modelMode string,
	agentName string,
	failure error,
) {
	if failure == nil || !options.Headless {
		return
	}

	run, err := startHeadlessRun(ctx, store, options, sessionState, prompt, modelName, modelMode, agentName)
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning: "+err.Error())

		return
	}

	finishHeadlessRun(store, run, session.HeadlessStatusFailed, failure.Error())
}

func recordHeadlessAssistantMessage(
	store *session.Store,
	run *session.HeadlessRun,
	contentBytes int,
	providerFailureMetadata ...map[string]string,
) {
	if store == nil || run == nil {
		return
	}

	current, err := store.LoadHeadlessRun(run.ID)
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning: load headless run before assistant message: "+err.Error())

		return
	}

	if headlessRunRecordingIsTerminal(current.Status) {
		*run = current

		return
	}

	if current.Status == session.HeadlessStatusRunning || current.Status == session.HeadlessStatusOrphaned {
		if heartbeatErr := store.HeartbeatHeadlessRun(run.ID); heartbeatErr != nil {
			fmt.Fprintln(os.Stderr, "warning: heartbeat headless run before assistant message: "+heartbeatErr.Error())

			return
		}

		current, err = store.LoadHeadlessRun(run.ID)
		if err != nil {
			fmt.Fprintln(os.Stderr, "warning: reload headless run before assistant message: "+err.Error())

			return
		}

		if headlessRunRecordingIsTerminal(current.Status) {
			*run = current

			return
		}
	}

	mergeHeadlessRunForAssistantRecording(run, current)

	if err := store.AppendHeadlessLog(run.ID, fmt.Sprintf("assistant_message\t%s\tbytes=%d\n", time.Now().UTC().Format(time.RFC3339), contentBytes)); err != nil {
		fmt.Fprintln(os.Stderr, "warning: "+err.Error())
	}

	event := session.HeadlessEvent{
		Type:              session.HeadlessEventAssistantMessage,
		Status:            session.HeadlessStatusRunning,
		Role:              string(llm.RoleAssistant),
		ParentRunID:       run.ParentRunID,
		RetryOfRunID:      run.RetryOfRunID,
		SupersededByRunID: run.SupersededByRunID,
		SessionID:         run.SessionID,
		SessionPath:       run.SessionPath,
		Agent:             run.Agent,
		Model:             run.Model,
		Autonomy:          run.Autonomy,
		AgentLoopBudget:   run.AgentLoopBudget,
		CWD:               run.CWD,
		Executable:        run.Executable,
		Version:           run.Version,
		Hostname:          run.Hostname,
		StartedCommand:    run.StartedCommand,
		StartMethod:       run.StartMethod,
		TerminalReason:    run.TerminalReason,
		CancelReason:      run.CancellationReason,
		StaleReason:       run.StaleReason,
		OrphanedReason:    run.OrphanedReason,
		CommandArgs:       append([]string(nil), run.CommandArgs...),
		ChildRunIDs:       append([]string(nil), run.ChildRunIDs...),
		PID:               run.PID,
		ParentPID:         run.ParentPID,
		ProcessGroupID:    run.ProcessGroupID,
		RetryCount:        run.RetryCount,
		Metadata:          headlessAssistantMetadata(contentBytes, providerFailureMetadata...),
	}
	if err := store.AppendHeadlessEvent(run.ID, event); err != nil {
		fmt.Fprintln(os.Stderr, "warning: "+err.Error())
	}
}

func headlessAssistantMetadata(contentBytes int, providerFailureMetadata ...map[string]string) map[string]string {
	metadata := map[string]string{
		"bytes": strconv.Itoa(contentBytes),
	}

	for _, next := range providerFailureMetadata {
		maps.Copy(metadata, next)
	}

	metadata["bytes"] = strconv.Itoa(contentBytes)

	return metadata
}

func mergeHeadlessRunForAssistantRecording(run *session.HeadlessRun, current session.HeadlessRun) {
	responseModel := run.Model
	*run = current

	if strings.TrimSpace(responseModel) != "" {
		run.Model = responseModel
	}
}

func headlessRunRecordingIsTerminal(status session.HeadlessStatus) bool {
	switch status {
	case session.HeadlessStatusCompleted,
		session.HeadlessStatusFailed,
		session.HeadlessStatusCanceled,
		session.HeadlessStatusTimedOut,
		session.HeadlessStatusExpired,
		session.HeadlessStatusStale,
		session.HeadlessStatusRetried,
		session.HeadlessStatusSuperseded,
		session.HeadlessStatusCorrupt:
		return true
	default:
		return false
	}
}

func startHeadlessHeartbeat(ctx context.Context, store *session.Store, run *session.HeadlessRun) func() {
	if store == nil || run == nil {
		return func() {}
	}

	id := run.ID
	heartbeatCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	go func() {
		defer close(done)

		ticker := time.NewTicker(headlessHeartbeatInterval)
		defer ticker.Stop()

		for {
			select {
			case <-heartbeatCtx.Done():
				return
			case <-ticker.C:
				if err := store.HeartbeatHeadlessRun(id); err != nil {
					slog.Debug("headless heartbeat failed", "id", id, "error", err)
				}
			}
		}
	}()

	return func() {
		cancel()
		<-done
	}
}

func startHeadlessRun(
	ctx context.Context,
	store *session.Store,
	options runOnceExecutionOptions,
	sessionState session.Session,
	prompt string,
	modelName string,
	modelMode string,
	agentName string,
	contextManifestJSON ...string,
) (*session.HeadlessRun, error) {
	if !options.Headless {
		return nil, nil
	}

	if !autonomy.Normalize(options.Autonomy).Allows(autonomy.ActionFileWrite) {
		return nil, fmt.Errorf("%s", autonomy.DenialMessage(options.Autonomy, autonomy.ActionFileWrite, "--headless run metadata"))
	}

	if store == nil {
		return nil, errors.New("headless mode requires a session store")
	}

	id := options.HeadlessID
	if id == "" {
		id = session.New("", nil).ID
	}

	if err := authorizeHeadlessPermission(ctx, "start headless run", store, id, permission.OperationWrite); err != nil {
		return nil, fmt.Errorf("start headless run: %w", err)
	}

	run := session.HeadlessRun{
		ID:              id,
		ParentRunID:     os.Getenv(headlessParentRunIDEnv),
		RetryOfRunID:    os.Getenv(headlessRetryOfRunIDEnv),
		SessionID:       sessionState.ID,
		SessionPath:     store.Path(sessionState.ID),
		Prompt:          strings.TrimSpace(prompt),
		Model:           modelName,
		ModelMode:       strings.TrimSpace(modelMode),
		Autonomy:        autonomy.Normalize(options.Autonomy).String(),
		AgentLoopBudget: options.AgentLoopBudget,
		Agent:           agentName,
		Executable:      currentExecutablePath(),
		Version:         versionString(),
		StartedCommand:  strings.Join(os.Args, " "),
		StartMethod:     "headless",
		RetryCount:      headlessRetryCountFromEnv(),
		Status:          session.HeadlessStatusRunning,
		PrivateLogs:     options.HeadlessPrivateLog,
	}

	if err := saveStartedHeadlessRun(store, run); err != nil {
		return nil, fmt.Errorf("start headless run: %w", err)
	}

	saved, err := store.LoadHeadlessRun(id)
	if err != nil {
		loadErr := fmt.Errorf("load started headless run: %w", err)
		failStartedHeadlessRun(store, &run, loadErr)

		return nil, loadErr
	}

	if err := store.AppendHeadlessLog(id, "started\t"+time.Now().UTC().Format(time.RFC3339)+"\tsession="+sessionState.ID+"\n"); err != nil {
		logErr := fmt.Errorf("write headless start log: %w", err)
		failStartedHeadlessRun(store, &saved, logErr)

		return nil, logErr
	}

	if err := store.AppendHeadlessEvent(id, session.HeadlessEvent{
		Type:              session.HeadlessEventStarted,
		Status:            saved.Status,
		ParentRunID:       saved.ParentRunID,
		RetryOfRunID:      saved.RetryOfRunID,
		SupersededByRunID: saved.SupersededByRunID,
		SessionID:         saved.SessionID,
		SessionPath:       saved.SessionPath,
		Agent:             saved.Agent,
		Model:             saved.Model,
		Autonomy:          saved.Autonomy,
		AgentLoopBudget:   saved.AgentLoopBudget,
		CWD:               saved.CWD,
		Executable:        saved.Executable,
		Version:           saved.Version,
		Hostname:          saved.Hostname,
		StartedCommand:    saved.StartedCommand,
		StartMethod:       saved.StartMethod,
		TerminalReason:    saved.TerminalReason,
		CancelReason:      saved.CancellationReason,
		StaleReason:       saved.StaleReason,
		OrphanedReason:    saved.OrphanedReason,
		CommandArgs:       append([]string(nil), saved.CommandArgs...),
		PID:               saved.PID,
		ParentPID:         saved.ParentPID,
		ProcessGroupID:    saved.ProcessGroupID,
		RetryCount:        saved.RetryCount,
	}); err != nil {
		eventErr := fmt.Errorf("write headless start event: %w", err)
		failStartedHeadlessRun(store, &saved, eventErr)

		return nil, eventErr
	}

	if err := appendHeadlessUserMessageEvent(store, id, saved); err != nil {
		eventErr := fmt.Errorf("write headless user message event: %w", err)
		failStartedHeadlessRun(store, &saved, eventErr)

		return nil, eventErr
	}

	if saved.ParentRunID != "" {
		if err := store.LinkHeadlessChildRun(saved.ParentRunID, saved.ID); err != nil {
			fmt.Fprintln(os.Stderr, "warning: link headless child run: "+err.Error())
		}
	}

	manifestJSON := ""
	if len(contextManifestJSON) > 0 {
		manifestJSON = contextManifestJSON[0]
	}

	if err := appendHeadlessContextManifestLog(store, &saved, manifestJSON); err != nil {
		failStartedHeadlessRun(store, &saved, err)

		return nil, err
	}

	return &saved, nil
}

func currentExecutablePath() string {
	executable, err := os.Executable()
	if err != nil {
		return ""
	}

	return executable
}

func headlessRetryCountFromEnv() int {
	raw := strings.TrimSpace(os.Getenv(headlessRetryCountEnv))
	if raw == "" {
		return 0
	}

	count, err := strconv.Atoi(raw)
	if err != nil || count < 0 {
		return 0
	}

	return count
}

func appendHeadlessUserMessageEvent(store *session.Store, id string, run session.HeadlessRun) error {
	if strings.TrimSpace(run.Prompt) == "" {
		return nil
	}

	if err := store.AppendHeadlessEvent(id, session.HeadlessEvent{
		Type:              session.HeadlessEventUserMessage,
		Status:            run.Status,
		Role:              string(llm.RoleUser),
		Message:           run.Prompt,
		ParentRunID:       run.ParentRunID,
		RetryOfRunID:      run.RetryOfRunID,
		SupersededByRunID: run.SupersededByRunID,
		SessionID:         run.SessionID,
		SessionPath:       run.SessionPath,
		Agent:             run.Agent,
		Model:             run.Model,
		Autonomy:          run.Autonomy,
		AgentLoopBudget:   run.AgentLoopBudget,
		CWD:               run.CWD,
		Executable:        run.Executable,
		Version:           run.Version,
		Hostname:          run.Hostname,
		StartedCommand:    run.StartedCommand,
		StartMethod:       run.StartMethod,
		CommandArgs:       append([]string(nil), run.CommandArgs...),
		PID:               run.PID,
		ParentPID:         run.ParentPID,
		ProcessGroupID:    run.ProcessGroupID,
		RetryCount:        run.RetryCount,
		Metadata: map[string]string{
			"bytes": strconv.Itoa(len(run.Prompt)),
		},
	}); err != nil {
		return fmt.Errorf("append headless user message event: %w", err)
	}

	return nil
}

func failStartedHeadlessRun(store *session.Store, run *session.HeadlessRun, err error) {
	if err == nil {
		return
	}

	finishHeadlessRun(store, run, session.HeadlessStatusFailed, err.Error())
}

func saveStartedHeadlessRun(store *session.Store, run session.HeadlessRun) error {
	if err := store.SaveNewHeadlessRun(run); err != nil {
		return fmt.Errorf("save new headless run: %w", err)
	}

	return nil
}

func appendHeadlessContextManifestLog(store *session.Store, run *session.HeadlessRun, manifestJSON string) error {
	if store == nil || run == nil || strings.TrimSpace(manifestJSON) == "" {
		return nil
	}

	line := "context_manifest\t" + time.Now().UTC().Format(time.RFC3339) + "\tjson=" + manifestJSON + "\n"
	if err := store.AppendHeadlessLog(run.ID, line); err != nil {
		return fmt.Errorf("write headless context manifest log: %w", err)
	}

	return nil
}

func finishHeadlessRun(store *session.Store, run *session.HeadlessRun, status session.HeadlessStatus, message string, metadata ...map[string]string) {
	if store == nil || run == nil {
		return
	}

	now := time.Now().UTC()
	message = strings.TrimSpace(message)

	status = finishHeadlessStatus(status, message)
	applyFinishedHeadlessRun(run, status, message, now)

	saved, wrote, err := store.SaveFinishedHeadlessRun(*run)
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning: "+err.Error())
		return
	}

	*run = saved

	if !wrote {
		return
	}

	appendFinishedHeadlessLog(store, run, status, now)
	appendFinishedHeadlessEvent(store, run, status, headlessFinishMetadata(metadata...))
}

func finishHeadlessStatus(status session.HeadlessStatus, message string) session.HeadlessStatus {
	if status != session.HeadlessStatusFailed {
		return status
	}

	if isTimeoutMessage(message) {
		return session.HeadlessStatusTimedOut
	}

	if isCancellationMessage(message) {
		return session.HeadlessStatusCanceled
	}

	return status
}

func applyFinishedHeadlessRun(run *session.HeadlessRun, status session.HeadlessStatus, message string, now time.Time) {
	run.Status = status
	run.CompletedAt = &now
	run.Error = message

	if run.CancellationReason == "" && isCancellationMessage(run.Error) {
		run.CancellationReason = run.Error
	}

	if run.TerminalReason == "" {
		run.TerminalReason = run.Error
		if run.TerminalReason == "" {
			run.TerminalReason = string(status)
		}
	}

	exitCode := headlessExitCode(status)
	run.ExitCode = &exitCode

	if status == session.HeadlessStatusCanceled {
		run.CanceledAt = &now

		if run.CancellationReason == "" {
			run.CancellationReason = "canceled"
		}

		if run.TerminalReason == "" {
			run.TerminalReason = run.CancellationReason
		}
	}
}

func headlessExitCode(status session.HeadlessStatus) int {
	switch status {
	case session.HeadlessStatusCompleted:
		return 0
	case session.HeadlessStatusCanceled:
		return 130
	case session.HeadlessStatusTimedOut:
		return 124
	default:
		return 1
	}
}

func appendFinishedHeadlessLog(store *session.Store, run *session.HeadlessRun, status session.HeadlessStatus, now time.Time) {
	logLine := string(status) + "\t" + now.Format(time.RFC3339)
	if run.Error != "" {
		logLine += "\terror=" + run.Error
	}

	if err := store.AppendHeadlessLog(run.ID, logLine+"\n"); err != nil {
		fmt.Fprintln(os.Stderr, "warning: "+err.Error())
	}
}

func appendFinishedHeadlessEvent(store *session.Store, run *session.HeadlessRun, status session.HeadlessStatus, metadata map[string]string) {
	eventType := session.HeadlessEventType(status)
	if status == session.HeadlessStatusCompleted {
		eventType = session.HeadlessEventCompleted
	}

	if err := store.AppendHeadlessEvent(run.ID, session.HeadlessEvent{
		Type:              eventType,
		Status:            run.Status,
		ParentRunID:       run.ParentRunID,
		RetryOfRunID:      run.RetryOfRunID,
		SupersededByRunID: run.SupersededByRunID,
		SessionID:         run.SessionID,
		SessionPath:       run.SessionPath,
		Message:           run.TerminalReason,
		Error:             run.Error,
		Agent:             run.Agent,
		Model:             run.Model,
		Autonomy:          run.Autonomy,
		AgentLoopBudget:   run.AgentLoopBudget,
		CWD:               run.CWD,
		Executable:        run.Executable,
		Version:           run.Version,
		Hostname:          run.Hostname,
		StartedCommand:    run.StartedCommand,
		StartMethod:       run.StartMethod,
		TerminalReason:    run.TerminalReason,
		CancelReason:      run.CancellationReason,
		StaleReason:       run.StaleReason,
		OrphanedReason:    run.OrphanedReason,
		CommandArgs:       append([]string(nil), run.CommandArgs...),
		ChildRunIDs:       append([]string(nil), run.ChildRunIDs...),
		ExitCode:          run.ExitCode,
		PID:               run.PID,
		ParentPID:         run.ParentPID,
		ProcessGroupID:    run.ProcessGroupID,
		RetryCount:        run.RetryCount,
		Metadata:          metadata,
	}); err != nil {
		fmt.Fprintln(os.Stderr, "warning: "+err.Error())
	}
}

func headlessFinishMetadata(metadata ...map[string]string) map[string]string {
	if len(metadata) == 0 {
		return nil
	}

	var out map[string]string

	for _, values := range metadata {
		for key, value := range values {
			key = strings.TrimSpace(key)
			if key == "" {
				continue
			}

			if out == nil {
				out = make(map[string]string, len(values))
			}

			out[key] = value
		}
	}

	if len(out) == 0 {
		return nil
	}

	return out
}

func ensureHeadlessRunCanRecordResponse(store *session.Store, run *session.HeadlessRun) error {
	if store == nil || run == nil {
		return nil
	}

	current, err := store.HeadlessRunStatus(run.ID)
	if err != nil {
		return fmt.Errorf("check headless run status: %w", err)
	}

	*run = current

	return headlessCompletionError(run)
}

func headlessCompletionError(run *session.HeadlessRun) error {
	if run == nil || run.Status == "" || run.Status == session.HeadlessStatusCompleted {
		return nil
	}

	if !headlessRunRecordingIsTerminal(run.Status) {
		return nil
	}

	reason := strings.TrimSpace(run.TerminalReason)
	if reason == "" {
		reason = strings.TrimSpace(run.Error)
	}

	if reason == "" {
		reason = string(run.Status)
	}

	return fmt.Errorf("headless run %s ended with status %s: %s", run.ID, run.Status, reason)
}

func isCancellationMessage(message string) bool {
	message = strings.ToLower(strings.TrimSpace(message))
	if message == "" {
		return false
	}

	return strings.Contains(message, "cancel")
}

func isTimeoutMessage(message string) bool {
	message = strings.ToLower(strings.TrimSpace(message))
	if message == "" {
		return false
	}

	return strings.Contains(message, "context deadline exceeded") ||
		strings.Contains(message, "deadline exceeded") ||
		strings.Contains(message, "timed out") ||
		strings.Contains(message, "timeout")
}

func writeRunOnceResult(stdout, stderr io.Writer, result runOnceResult, outputFormat string, headless bool) error {
	switch outputFormat {
	case outputFormatJSON:
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")

		if err := encoder.Encode(result); err != nil {
			return fmt.Errorf("encode one-shot result: %w", err)
		}

		return nil
	default:
		if !headless {
			fmt.Fprintln(stdout, result.Content)
			printTokenUsageSummary(stderr, result.TokenUsage)
			fmt.Fprintln(stderr, formatRunOnceSessionSummary(result))

			if result.AgentLoopCheckpointPath != "" {
				fmt.Fprintln(stderr, "agent loop checkpoint: "+result.AgentLoopCheckpointPath)
			}

			if budget := formatAgentLoopBudgetCompact(result.AgentLoopBudget); budget != "" {
				fmt.Fprintln(stderr, "agent loop budget: "+budget)
			}

			if strings.TrimSpace(result.Autonomy) != "" {
				fmt.Fprintln(stderr, "autonomy: "+result.Autonomy)
			}
		}

		return nil
	}
}

func formatRunOnceSessionSummary(result runOnceResult) string {
	return "session: " + formatSessionLocation(result.SessionID, result.SessionPath, result.SessionPersisted, result.Autonomy)
}

func formatSessionLocation(sessionID, sessionPath string, persisted bool, autonomyLabel string) string {
	line := sessionID

	if persisted {
		if strings.TrimSpace(sessionPath) != "" {
			line += " (" + sessionPath + ")"
		}

		return line
	}

	reason := "not persisted"
	if strings.EqualFold(strings.TrimSpace(autonomyLabel), autonomy.Low.String()) {
		reason = "not persisted: autonomy low"
	}

	return line + " (" + reason + ")"
}

// runOnceAgentLoopManifestPreflight emits follow-up context manifests before
// model calls after the first agent-loop turn, then validates the request
// against the configured per-request input budget.
func runOnceAgentLoopManifestPreflight(
	ctx context.Context,
	hooks *events.Runner,
	reg *llm.Registry,
	store *session.Store,
	headlessRun *session.HeadlessRun,
	sessionPath string,
	sessionID string,
	agentName string,
	fallbackModels []string,
	inlineEvents []contextref.ReferenceEvent,
	referenceManifest contextref.ReferenceManifest,
	maxInputTokens int,
) func(iteration int, params llm.CompleteParams) error {
	return func(iteration int, params llm.CompleteParams) error {
		if iteration > 0 {
			manifestEvent := requestContextManifestEvent(newRequestContextManifestForModelsWithInlineEvents(
				reg,
				params.Model,
				fallbackModels,
				params.Messages,
				maxInputTokens,
				inlineEvents,
				referenceManifest,
			))
			manifestEvent.SessionID = sessionID
			manifestEvent.SessionPath = sessionPath
			manifestEvent.Agent = agentName
			setExplicitContextManifestEventModel(&manifestEvent, params.Model)
			emitHookWarning(ctx, hooks, manifestEvent)

			if err := appendHeadlessContextManifestLog(store, headlessRun, manifestEvent.Metadata["context_manifest"]); err != nil {
				return err
			}
		}

		return validateRequestBudgetWithFallbacks(reg, params.Model, fallbackModels, params.Messages, maxInputTokens)
	}
}

// runOnceComplete handles replay or live agent-loop completion for one-shot
// mode. If a replay path is set, it loads the recorded response; otherwise it
// runs the agentic loop with tool execution support.
//
//nolint:unparam // Compatibility wrapper kept for existing focused tests that exercise default-autonomy completion.
func runOnceComplete(
	ctx context.Context,
	reg *llm.Registry,
	params llm.CompleteParams,
	fallbackModels []string,
	agentLoopBudget llm.AgentLoopBudget,
	agentLoopCheckpointInterval int,
	responseOptions responseRecordOptions,
	permissionPolicy *permission.Policy,
	allowPermissionPrompt bool,
	checkpointPath string,
	beforeModelCall func(iteration int, params llm.CompleteParams) error,
	audit attshell.AuditContext,
) (*llm.Response, error) {
	return runOnceCompleteWithAutonomy(
		ctx,
		reg,
		params,
		fallbackModels,
		agentLoopBudget,
		autonomy.DefaultLevel,
		agentLoopCheckpointInterval,
		responseOptions,
		permissionPolicy,
		allowPermissionPrompt,
		checkpointPath,
		beforeModelCall,
		audit,
	)
}

func runOnceCompleteWithAutonomy(
	ctx context.Context,
	reg *llm.Registry,
	params llm.CompleteParams,
	fallbackModels []string,
	agentLoopBudget llm.AgentLoopBudget,
	autonomyLevel autonomy.Level,
	agentLoopCheckpointInterval int,
	responseOptions responseRecordOptions,
	permissionPolicy *permission.Policy,
	allowPermissionPrompt bool,
	checkpointPath string,
	beforeModelCall func(iteration int, params llm.CompleteParams) error,
	audit attshell.AuditContext,
) (*llm.Response, error) {
	resp, _, err := runOnceCompleteWithAutonomyTranscript(
		ctx,
		reg,
		params,
		fallbackModels,
		agentLoopBudget,
		autonomyLevel,
		agentLoopCheckpointInterval,
		responseOptions,
		permissionPolicy,
		allowPermissionPrompt,
		checkpointPath,
		beforeModelCall,
		audit,
	)

	return resp, err
}

func runOnceCompleteWithAutonomyTranscript(
	ctx context.Context,
	reg *llm.Registry,
	params llm.CompleteParams,
	fallbackModels []string,
	agentLoopBudget llm.AgentLoopBudget,
	autonomyLevel autonomy.Level,
	agentLoopCheckpointInterval int,
	responseOptions responseRecordOptions,
	permissionPolicy *permission.Policy,
	allowPermissionPrompt bool,
	checkpointPath string,
	beforeModelCall func(iteration int, params llm.CompleteParams) error,
	audit attshell.AuditContext,
) (*llm.Response, []llm.Message, error) {
	autonomyLevel = autonomy.Normalize(autonomyLevel)

	ctx = permission.ContextWithPolicy(ctx, permissionPolicy)
	if permissionPolicy != nil && allowPermissionPrompt {
		ctx = permission.ContextWithConfirmer(ctx, confirmPermissionStdin)
	}

	if responseOptions.ReplayPath != "" {
		resp, err := loadRecordedResponse(ctx, responseOptions.ReplayPath)
		if err != nil {
			return nil, nil, err
		}

		if err := enforceReplayAgentLoopBudget(reg, params.Model, fallbackModels, agentLoopBudget, resp); err != nil {
			return nil, nil, err
		}

		if err := saveRecordedResponse(ctx, responseOptions.RecordPath, params, fallbackModels, resp); err != nil {
			return nil, nil, err
		}

		return resp, append([]llm.Message(nil), params.Messages...), nil
	}

	cwd, err := os.Getwd()
	if err != nil {
		return nil, nil, fmt.Errorf("get working directory: %w", err)
	}

	executor := newBashExecutor(cwd, os.Stderr, audit, permissionPolicy)

	costEstimator, err := agentLoopCostEstimatorForBudget(reg, params.Model, fallbackModels, agentLoopBudget)
	if err != nil {
		return nil, nil, err
	}

	resp, messages, err := llm.AgentLoop(ctx, reg, params, fallbackModels, executor, llm.AgentLoopConfig{
		ConfirmContinue:    confirmContinueStdin,
		ConfirmToolCall:    confirmToolCallStdin,
		BeforeModelCall:    beforeModelCall,
		Budget:             agentLoopBudget,
		Autonomy:           autonomyLevel,
		EstimateCostMicros: costEstimator,
		CheckpointInterval: agentLoopCheckpointInterval,
		Policy:             llm.DefaultToolPolicyForAutonomy(autonomyLevel),
		CheckpointSink:     agentLoopCheckpointSink(checkpointPath),
	})
	if err != nil {
		return nil, messages, agentLoopError(err, checkpointPath)
	}

	if err := saveRecordedResponse(ctx, responseOptions.RecordPath, params, fallbackModels, resp); err != nil {
		return nil, messages, err
	}

	return resp, messages, nil
}

func enforceReplayAgentLoopBudget(
	reg *llm.Registry,
	model string,
	fallbackModels []string,
	budget llm.AgentLoopBudget,
	resp *llm.Response,
) error {
	if budget.IsZero() {
		return nil
	}

	usage, err := replayAgentLoopUsage(resp, budget)
	if err != nil {
		return err
	}

	costMicros, err := replayAgentLoopCostMicros(reg, model, fallbackModels, budget, resp)
	if err != nil {
		return err
	}

	usage.EstimatedCostMicros = costMicros

	return enforceReplayAgentLoopUsageBudget(budget, usage)
}

func replayAgentLoopCostMicros(
	reg *llm.Registry,
	model string,
	fallbackModels []string,
	budget llm.AgentLoopBudget,
	resp *llm.Response,
) (int64, error) {
	if budget.MaxCostMicros <= 0 {
		return 0, nil
	}

	estimator, err := agentLoopCostEstimatorForBudget(reg, model, fallbackModels, budget)
	if err != nil {
		return 0, err
	}

	costMicros, err := estimator(resp)
	if err != nil {
		return 0, fmt.Errorf("agent_loop.max_cost_micros: %w", err)
	}

	if costMicros < 0 {
		return 0, fmt.Errorf("agent_loop.max_cost_micros: negative cost estimate: %d micros", costMicros)
	}

	return costMicros, nil
}

func enforceReplayAgentLoopUsageBudget(budget llm.AgentLoopBudget, usage llm.AgentLoopUsage) error {
	if budget.MaxModelCalls > 0 && usage.ModelCalls > budget.MaxModelCalls {
		return fmt.Errorf("agent_loop.max_model_calls: model call budget exhausted: used %d of %d", usage.ModelCalls, budget.MaxModelCalls)
	}

	if budget.MaxInputTokens > 0 && usage.InputTokens > budget.MaxInputTokens {
		return fmt.Errorf("agent_loop.max_input_tokens: input token budget exceeded: used %d of %d", usage.InputTokens, budget.MaxInputTokens)
	}

	if budget.MaxOutputTokens > 0 && usage.OutputTokens > budget.MaxOutputTokens {
		return fmt.Errorf("agent_loop.max_output_tokens: output token budget exceeded: used %d of %d", usage.OutputTokens, budget.MaxOutputTokens)
	}

	if budget.MaxTotalTokens > 0 && usage.TotalTokens > budget.MaxTotalTokens {
		return fmt.Errorf("agent_loop.max_total_tokens: total token budget exceeded: used %d of %d", usage.TotalTokens, budget.MaxTotalTokens)
	}

	if budget.MaxCostMicros > 0 && usage.EstimatedCostMicros > budget.MaxCostMicros {
		return fmt.Errorf("agent_loop.max_cost_micros: cost budget exceeded: used %d micros of %d", usage.EstimatedCostMicros, budget.MaxCostMicros)
	}

	return nil
}

func replayAgentLoopUsage(resp *llm.Response, budget llm.AgentLoopBudget) (llm.AgentLoopUsage, error) {
	if resp == nil {
		return llm.AgentLoopUsage{}, nil
	}

	usage := llm.AgentLoopUsage{ModelCalls: 1}
	if err := replayAgentLoopTokenUsageError(resp, budget); err != nil {
		return usage, err
	}

	usage.InputTokens = resp.InputTokens
	usage.CachedInputTokens = resp.CachedInputTokens
	usage.CacheWriteTokens = resp.CacheWriteInputTokens
	usage.OutputTokens = resp.OutputTokens
	usage.TotalTokens = resp.InputTokens + resp.OutputTokens

	return usage, nil
}

func replayAgentLoopTokenUsageError(resp *llm.Response, budget llm.AgentLoopBudget) error {
	if budget.MaxInputTokens <= 0 && budget.MaxOutputTokens <= 0 && budget.MaxTotalTokens <= 0 {
		return nil
	}

	if err := replayAgentLoopInvalidTokenUsageError(resp, budget); err != nil {
		return err
	}

	return replayAgentLoopMissingTokenUsageError(resp, budget)
}

func replayAgentLoopInvalidTokenUsageError(resp *llm.Response, budget llm.AgentLoopBudget) error {
	if resp.InputTokens < 0 ||
		resp.CachedInputTokens < 0 ||
		resp.CacheWriteInputTokens < 0 ||
		resp.OutputTokens < 0 {
		return fmt.Errorf("%s: token budget could not be enforced: %w: negative token usage",
			replayAgentLoopTokenBudgetField(budget),
			llm.ErrAgentLoopTokenUsageUnavailable,
		)
	}

	return nil
}

func replayAgentLoopMissingTokenUsageError(resp *llm.Response, budget llm.AgentLoopBudget) error {
	requireInput := budget.MaxInputTokens > 0 || budget.MaxTotalTokens > 0
	requireOutput := (budget.MaxOutputTokens > 0 || budget.MaxTotalTokens > 0) && replayAgentLoopResponseHasVisibleOutput(resp)

	switch {
	case requireInput && resp.InputTokens <= 0:
		return replayAgentLoopTokenUsageUnavailableError(
			replayAgentLoopInputUsageBudgetField(budget),
			"input token usage unavailable",
		)
	case requireOutput && resp.OutputTokens <= 0:
		return replayAgentLoopTokenUsageUnavailableError(
			replayAgentLoopOutputUsageBudgetField(budget),
			"output token usage unavailable",
		)
	}

	if resp.CachedInputTokens+resp.CacheWriteInputTokens > resp.InputTokens {
		return replayAgentLoopTokenUsageUnavailableError(
			replayAgentLoopInputUsageBudgetField(budget),
			"cache token usage exceeds input tokens",
		)
	}

	return nil
}

func replayAgentLoopTokenUsageUnavailableError(field, detail string) error {
	return fmt.Errorf("%s: token budget could not be enforced: %w: %s",
		field,
		llm.ErrAgentLoopTokenUsageUnavailable,
		detail,
	)
}

func replayAgentLoopResponseHasVisibleOutput(resp *llm.Response) bool {
	return resp != nil && (resp.Content != "" || len(resp.ToolCalls) > 0)
}

func replayAgentLoopTokenBudgetField(budget llm.AgentLoopBudget) string {
	switch {
	case budget.MaxInputTokens > 0:
		return agentLoopConfigFieldMaxInputTokens
	case budget.MaxOutputTokens > 0:
		return agentLoopConfigFieldMaxOutputTokens
	case budget.MaxTotalTokens > 0:
		return agentLoopConfigFieldMaxTotalTokens
	default:
		return agentLoopConfigFieldMaxTotalTokens
	}
}

func replayAgentLoopInputUsageBudgetField(budget llm.AgentLoopBudget) string {
	if budget.MaxInputTokens > 0 {
		return agentLoopConfigFieldMaxInputTokens
	}

	return agentLoopConfigFieldMaxTotalTokens
}

func replayAgentLoopOutputUsageBudgetField(budget llm.AgentLoopBudget) string {
	if budget.MaxOutputTokens > 0 {
		return agentLoopConfigFieldMaxOutputTokens
	}

	return agentLoopConfigFieldMaxTotalTokens
}

// confirmContinueStdin prompts the user on stdin/stderr when the agent loop
// reaches a checkpoint. Used in one-shot (non-TUI) mode.
func confirmContinueStdin(iterations int) bool {
	fmt.Fprintf(os.Stderr, "\nAgent loop reached %d iterations. Continue? [Y/n] ", iterations)

	var answer string
	if _, err := fmt.Scanln(&answer); err != nil {
		// On EOF or error, treat as "yes" to avoid blocking headless runs.
		return true
	}

	answer = strings.TrimSpace(strings.ToLower(answer))

	return answer == "" || answer == "y" || answer == affirmativeYes
}

// confirmToolCallStdin prompts before commands that the built-in tool policy
// marks as require-confirm in one-shot mode.
func confirmToolCallStdin(_ context.Context, call llm.ToolCall, decision llm.ToolPolicyDecision) bool {
	command, ok := call.Input["command"].(string)
	if !ok {
		command = "<missing command>"
	}

	fmt.Fprintf(
		os.Stderr,
		"\nAgent tool call requires confirmation (%s): %s\n$ %s\nExecute? [y/N] ",
		decision.MatchedRule,
		decision.Reason,
		command,
	)

	var answer string
	if _, err := fmt.Scanln(&answer); err != nil {
		return false
	}

	answer = strings.TrimSpace(strings.ToLower(answer))

	return answer == "y" || answer == affirmativeYes
}

func confirmPermissionStdin(_ context.Context, request permission.Request, decision permission.Decision) bool {
	action := strings.TrimSpace(request.Action)
	if action == "" {
		action = strings.TrimSpace(decision.Reason)
	}

	fmt.Fprintf(
		os.Stderr,
		"\nPermission policy requires confirmation (%s, policy: %s): %s\n%s\nAllow? [y/N] ",
		decision.Rule,
		permissionPromptPolicy(decision),
		decision.Reason,
		action,
	)

	var answer string
	if _, err := fmt.Scanln(&answer); err != nil {
		return false
	}

	answer = strings.TrimSpace(strings.ToLower(answer))

	return answer == "y" || answer == affirmativeYes
}

// newBashExecutor creates a ToolExecutor for built-in file and bash tools in
// the given working directory, logging human-readable tool output to writer.
func newBashExecutor(cwd string, logw io.Writer, audit attshell.AuditContext, permissionPolicy *permission.Policy) llm.ToolExecutor {
	if strings.TrimSpace(audit.Autonomy) == "" {
		audit.Autonomy = autonomy.DefaultLevel.String()
	}

	return func(ctx context.Context, call llm.ToolCall) llm.ToolResult {
		if permissionPolicy != nil && permission.PolicyFromContext(ctx) == nil {
			ctx = permission.ContextWithPolicy(ctx, permissionPolicy)
		}

		if llm.IsFileToolName(call.Name) {
			result := executeFileTool(ctx, call, fileToolExecutorOptions{
				WorkingDir: cwd,
				Autonomy:   autonomy.Level(audit.Autonomy),
			})
			logFileToolResult(logw, call.Name, result)

			return result
		}

		return executeRunOnceBashTool(ctx, call, cwd, logw, audit, permissionPolicy)
	}
}

func logFileToolResult(logw io.Writer, name string, result llm.ToolResult) {
	if logw == nil {
		return
	}

	line := result.Content
	if result.IsError {
		line = "[error] " + line
	}

	fmt.Fprintln(logw, dimStyle.Render("  "+name+" tool: "+line))
}

func executeRunOnceBashTool(
	ctx context.Context,
	call llm.ToolCall,
	cwd string,
	logw io.Writer,
	audit attshell.AuditContext,
	permissionPolicy *permission.Policy,
) llm.ToolResult {
	if call.Name != llm.ToolNameBash {
		return llm.ToolResult{
			ToolCallID: call.ID,
			Content:    "unknown tool: " + call.Name,
			IsError:    true,
		}
	}

	command, ok := call.Input["command"].(string)
	if !ok || command == "" {
		return llm.ToolResult{
			ToolCallID: call.ID,
			Content:    "error: empty command",
			IsError:    true,
		}
	}

	if logw != nil {
		fmt.Fprintln(logw, dimStyle.Render("  $ "+command))
	}

	result, shellErr := runOnceBashCommand(ctx, cwd, command, logw, audit, permissionPolicy, call.ID)

	output := formatShellContext(shellResultMsg{
		command: command,
		stdout:  result.Stdout,
		stderr:  result.Stderr,
		err:     shellErr,
	})
	if shellErr != nil && logw != nil {
		fmt.Fprintln(logw, dimStyle.Render("[error] "+shellErr.Error()))
	}

	emitFromContextWarning(ctx, commandOutputEvent(
		"", "", "", "", cwd, command, output, shellErr,
		map[string]string{
			"source":       "llm_tool",
			"tool_call_id": call.ID,
			"autonomy":     audit.Autonomy,
		},
	))

	if shellErr != nil {
		return llm.ToolResult{ToolCallID: call.ID, Content: output, IsError: true}
	}

	return llm.ToolResult{ToolCallID: call.ID, Content: output}
}

func runOnceBashCommand(
	ctx context.Context,
	cwd string,
	command string,
	logw io.Writer,
	audit attshell.AuditContext,
	permissionPolicy *permission.Policy,
	toolCallID string,
) (attshell.Result, error) {
	commandEvent := events.Event{
		Type:    events.CommandExecute,
		Content: command,
		Metadata: map[string]string{
			"command":      command,
			"cwd":          cwd,
			"input":        command,
			"source":       "llm_tool",
			"tool_call_id": toolCallID,
			"autonomy":     audit.Autonomy,
		},
	}

	result, err := attshell.RunBash(ctx, attshell.Options{
		Command:        command,
		Dir:            cwd,
		Timeout:        5 * time.Minute,
		MaxOutputBytes: agentLoopToolOutputLimit(ctx),
		Audit:          audit,
		Permission:     permissionPolicy,
		StartCallback: func() {
			emitFromContextWarning(ctx, commandEvent)
		},
		OutputCallback: func(chunk attshell.OutputChunk) {
			if logw != nil {
				fmt.Fprint(logw, dimStyle.Render(string(chunk.Data)))
			}

			emitFromContextWarning(ctx, events.Event{
				Type:    events.CommandOutput,
				Content: string(chunk.Data),
				Metadata: map[string]string{
					"command":      command,
					"cwd":          cwd,
					"partial":      "true",
					"sequence":     strconv.FormatInt(chunk.Sequence, 10),
					"source":       "llm_tool",
					"stream":       string(chunk.Stream),
					"tool_call_id": toolCallID,
					"autonomy":     audit.Autonomy,
				},
			})
		},
	})
	if err != nil {
		return result, fmt.Errorf("run bash tool: %w", err)
	}

	return result, nil
}
