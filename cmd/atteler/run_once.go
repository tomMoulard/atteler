package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/tommoulard/atteler/pkg/agent"
	"github.com/tommoulard/atteler/pkg/contextref"
	"github.com/tommoulard/atteler/pkg/events"
	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/modelroute"
	"github.com/tommoulard/atteler/pkg/session"
	attshell "github.com/tommoulard/atteler/pkg/shell"
)

const (
	headlessHeartbeatInterval = 15 * time.Second
	headlessParentRunIDEnv    = "ATTELER_HEADLESS_PARENT_ID"
)

type responseRecordOptions struct {
	RecordPath string
	ReplayPath string
}

type runOnceExecutionOptions struct {
	OutputFormat                string
	HeadlessID                  string
	Response                    responseRecordOptions
	AgentLoopBudget             llm.AgentLoopBudget
	AgentLoopCheckpointInterval int
	Headless                    bool
	HeadlessPrivateLog          bool
}

type runOnceResult struct {
	SessionID               string     `json:"session_id"`
	SessionPath             string     `json:"session_path"`
	AgentLoopCheckpointPath string     `json:"agent_loop_checkpoint_path,omitempty"`
	HeadlessID              string     `json:"headless_id,omitempty"`
	Agent                   string     `json:"agent,omitempty"`
	Model                   string     `json:"model,omitempty"`
	Content                 string     `json:"content"`
	TokenUsage              tokenUsage `json:"token_usage"`
}

//nolint:govet // Field order follows request-preparation flow; padding is irrelevant here.
type runOncePrepared struct {
	activeAgent     agentSelection
	generation      generationSettings
	requestMessages []llm.Message
	refs            []contextref.Reference
	routeDecision   *modelroute.Decision
	fallbackModels  []string
	prompt          string
	referenceCtx    string
	requestModel    string
}

type responseRecordFile struct {
	RecordedAt time.Time             `json:"recorded_at"`
	Request    responseRecordRequest `json:"request"`
	Response   responseRecordPayload `json:"response"`
}

//nolint:govet // JSON field order is grouped for stable recording readability.
type responseRecordRequest struct {
	Temperature    *float64      `json:"temperature,omitempty"`
	TopP           *float64      `json:"top_p,omitempty"`
	Seed           *int          `json:"seed,omitempty"`
	Model          string        `json:"model,omitempty"`
	FallbackModels []string      `json:"fallback_models,omitempty"`
	Messages       []llm.Message `json:"messages"`
	MaxTokens      int           `json:"max_tokens,omitempty"`
	ReasoningLevel string        `json:"reasoning_level,omitempty"`
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

func saveRecordedResponse(path string, params llm.CompleteParams, fallbackModels []string, resp *llm.Response) error {
	if strings.TrimSpace(path) == "" || resp == nil {
		return nil
	}

	record := responseRecordFile{
		RecordedAt: time.Now().UTC(),
		Request: responseRecordRequest{
			Model:          params.Model,
			Messages:       append([]llm.Message(nil), params.Messages...),
			FallbackModels: append([]string(nil), fallbackModels...),
			MaxTokens:      params.MaxTokens,
			Temperature:    params.Temperature,
			TopP:           params.TopP,
			Seed:           params.Seed,
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

func loadRecordedResponse(path string) (*llm.Response, error) {
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

func runOnceWithOptions(
	ctx context.Context,
	reg *llm.Registry,
	agents *agent.Registry,
	hooks *events.Runner,
	store *session.Store,
	sessionState session.Session,
	contextOptions contextref.Options,
	referenceContext string,
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
	outputFormat, err := normalizeOutputFormat(executionOptions.OutputFormat)
	if err != nil {
		recordHeadlessPreflightFailure(store, executionOptions, sessionState, prompt, selectedModel, selectedAgent, err)

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
		generationDefaults,
		generationOverrides,
		modelLocked,
		prompt,
	)
	if err != nil {
		recordHeadlessPreflightFailure(store, executionOptions, sessionState, prompt, selectedModel, selectedAgent, err)
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

	emitHookWarning(ctx, hooks, events.Event{
		Type:        events.SessionStart,
		SessionID:   sessionState.ID,
		SessionPath: store.Path(sessionState.ID),
		Agent:       prepared.activeAgent.name,
		Model:       sessionState.DefaultModel,
	})
	defer emitHookWarning(ctx, hooks, events.Event{
		Type:        events.SessionEnd,
		SessionID:   sessionState.ID,
		SessionPath: store.Path(sessionState.ID),
		Agent:       prepared.activeAgent.name,
		Model:       sessionState.DefaultModel,
	})

	headlessRun, err := startHeadlessRun(store, executionOptions, sessionState, prepared.prompt, prepared.requestModel, prepared.activeAgent.name)
	if err != nil {
		return err
	}

	stopHeadlessHeartbeat := startHeadlessHeartbeat(ctx, store, headlessRun)
	defer stopHeadlessHeartbeat()

	if userSaveErr := saveRunOnceUserMessage(ctx, hooks, store, &sessionState, prepared); userSaveErr != nil {
		finishHeadlessRun(store, headlessRun, session.HeadlessStatusFailed, userSaveErr.Error())

		return userSaveErr
	}

	params := llm.CompleteParams{
		Model:    prepared.requestModel,
		Messages: append(append([]llm.Message(nil), sessionState.Messages[:len(sessionState.Messages)-1]...), prepared.requestMessages...),
	}
	if prepared.activeAgent.ok {
		params = prepared.activeAgent.agent.CompleteParams(prepared.requestModel, params.Messages)
	}

	prependReferenceContext(&params, prepared.referenceCtx)

	applyGenerationParams(&params, prepared.generation)

	ctx = events.WithEmitter(ctx, hooks, events.Event{
		SessionID:   sessionState.ID,
		SessionPath: store.Path(sessionState.ID),
		Agent:       prepared.activeAgent.name,
		Model:       prepared.requestModel,
	})

	if budgetErr := validateRequestBudget(reg, params.Model, params.Messages, maxInputTokens); budgetErr != nil {
		finishHeadlessRun(store, headlessRun, session.HeadlessStatusFailed, budgetErr.Error())

		return budgetErr
	}

	// Enable tool use for one-shot calls: the LLM can invoke bash commands.
	// Apply agent-level tool filtering when an agent is active.
	tools := llm.DefaultTools()
	if prepared.activeAgent.ok {
		tools = prepared.activeAgent.agent.FilterTools(tools)
	}

	params.Tools = tools

	if len(tools) > 0 {
		prependToolReminder(&params, tools)
	}

	slog.Debug("one-shot LLM request",
		"agent", prepared.activeAgent.name,
		"model", params.Model,
		"tools", len(params.Tools),
		"messages", len(params.Messages),
	)

	checkpointPath := agentLoopCheckpointPath(store.Path(sessionState.ID))
	if executionOptions.Response.ReplayPath != "" {
		checkpointPath = ""
	}

	resp, err := runOnceComplete(
		ctx,
		reg,
		params,
		prepared.fallbackModels,
		executionOptions.AgentLoopBudget,
		executionOptions.AgentLoopCheckpointInterval,
		executionOptions.Response,
		checkpointPath,
	)
	if err != nil {
		emitHookWarning(ctx, hooks, events.Event{
			Type:        events.Error,
			SessionID:   sessionState.ID,
			SessionPath: store.Path(sessionState.ID),
			Agent:       prepared.activeAgent.name,
			Model:       sessionState.DefaultModel,
			Error:       err.Error(),
		})
		finishHeadlessRun(store, headlessRun, session.HeadlessStatusFailed, err.Error())

		return fmt.Errorf("one-shot complete: %w", err)
	}

	return finishRunOnceSuccess(
		ctx,
		hooks,
		store,
		&sessionState,
		prepared.activeAgent.name,
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

	if err := saveRunOnceAssistantResponse(ctx, hooks, store, sessionState, agentName, resp); err != nil {
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
		AgentLoopCheckpointPath: checkpointPath,
		Agent:                   agentName,
		Model:                   resp.Model,
		Content:                 resp.Content,
		TokenUsage:              usage,
	}
	if headlessRun != nil {
		result.HeadlessID = headlessRun.ID
		if resp.Model != "" {
			headlessRun.Model = resp.Model
		}

		recordHeadlessAssistantMessage(store, headlessRun, len(resp.Content))
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
	generationDefaults generationSettings,
	generationOverrides generationSettings,
	modelLocked bool,
	prompt string,
) (runOncePrepared, error) {
	activeAgent, userPrompt, err := resolveAgent(agents, selectedAgent, prompt)
	if err != nil {
		return runOncePrepared{}, err
	}

	requestMessages, refs, err := expandReferences([]llm.Message{{Role: llm.RoleUser, Content: userPrompt}}, contextOptions)
	if err != nil {
		return runOncePrepared{}, err
	}

	generation := generationForRequest(generationDefaults, generationOverrides, activeAgent)
	requestReferenceContext := buildReferenceContext(ctx, referenceContext, activeAgent, contextOptions)
	budgetMessages := requestMessagesForBudget(selectedModel, requestMessages, activeAgent, generation, requestReferenceContext)

	requestModel, fallbackModels, routeDecision, err := requestModelAndFallbacks(
		selectedModel,
		modelLocked,
		fallbackModels,
		activeAgent,
		routeProfileForMessages(budgetMessages, generation),
		routeTelemetryFromRegistry(reg),
		routeAvailabilityFromRegistryWithRefresh(ctx, reg, effectiveRouteCandidateChain(selectedModel, fallbackModels, activeAgent, modelLocked)),
	)
	if err != nil {
		return runOncePrepared{activeAgent: activeAgent, routeDecision: routeDecision}, err
	}

	return runOncePrepared{
		activeAgent:     activeAgent,
		generation:      generation,
		requestMessages: requestMessages,
		refs:            refs,
		routeDecision:   routeDecision,
		fallbackModels:  fallbackModels,
		prompt:          userPrompt,
		referenceCtx:    requestReferenceContext,
		requestModel:    requestModel,
	}, nil
}

func saveRunOnceUserMessage(
	ctx context.Context,
	hooks *events.Runner,
	store *session.Store,
	sessionState *session.Session,
	prepared runOncePrepared,
) error {
	sessionState.Append(llm.RoleUser, prepared.prompt)

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
) error {
	sessionState.Append(llm.RoleAssistant, resp.Content)

	if resp.Model != "" {
		sessionState.DefaultModel = resp.Model
	}

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
	emitHookWarning(ctx, hooks, events.Event{
		Type:        events.AssistantMessage,
		SessionID:   sessionState.ID,
		SessionPath: store.Path(sessionState.ID),
		Agent:       agentName,
		Model:       resp.Model,
		Role:        string(llm.RoleAssistant),
		Content:     resp.Content,
	})

	return nil
}

func recordHeadlessPreflightFailure(
	store *session.Store,
	options runOnceExecutionOptions,
	sessionState session.Session,
	prompt string,
	modelName string,
	agentName string,
	failure error,
) {
	if failure == nil || !options.Headless {
		return
	}

	run, err := startHeadlessRun(store, options, sessionState, prompt, modelName, agentName)
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning: "+err.Error())

		return
	}

	finishHeadlessRun(store, run, session.HeadlessStatusFailed, failure.Error())
}

func recordHeadlessAssistantMessage(store *session.Store, run *session.HeadlessRun, contentBytes int) {
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
		Type:           session.HeadlessEventAssistantMessage,
		Status:         session.HeadlessStatusRunning,
		Role:           string(llm.RoleAssistant),
		ParentRunID:    run.ParentRunID,
		SessionID:      run.SessionID,
		SessionPath:    run.SessionPath,
		Agent:          run.Agent,
		Model:          run.Model,
		CWD:            run.CWD,
		Hostname:       run.Hostname,
		StartedCommand: run.StartedCommand,
		StartMethod:    run.StartMethod,
		TerminalReason: run.TerminalReason,
		CancelReason:   run.CancellationReason,
		StaleReason:    run.StaleReason,
		OrphanedReason: run.OrphanedReason,
		CommandArgs:    append([]string(nil), run.CommandArgs...),
		ChildRunIDs:    append([]string(nil), run.ChildRunIDs...),
		PID:            run.PID,
		ParentPID:      run.ParentPID,
		ProcessGroupID: run.ProcessGroupID,
		Metadata: map[string]string{
			"bytes": strconv.Itoa(contentBytes),
		},
	}
	if err := store.AppendHeadlessEvent(run.ID, event); err != nil {
		fmt.Fprintln(os.Stderr, "warning: "+err.Error())
	}
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
		session.HeadlessStatusStale,
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
	store *session.Store,
	options runOnceExecutionOptions,
	sessionState session.Session,
	prompt string,
	modelName string,
	agentName string,
) (*session.HeadlessRun, error) {
	if !options.Headless {
		return nil, nil
	}

	if store == nil {
		return nil, errors.New("headless mode requires a session store")
	}

	id := options.HeadlessID
	if id == "" {
		id = session.New("", nil).ID
	}

	run := session.HeadlessRun{
		ID:             id,
		ParentRunID:    os.Getenv(headlessParentRunIDEnv),
		SessionID:      sessionState.ID,
		SessionPath:    store.Path(sessionState.ID),
		Prompt:         strings.TrimSpace(prompt),
		Model:          modelName,
		Agent:          agentName,
		StartedCommand: strings.Join(os.Args, " "),
		StartMethod:    "headless",
		Status:         session.HeadlessStatusRunning,
		PrivateLogs:    options.HeadlessPrivateLog,
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
		Type:           session.HeadlessEventStarted,
		Status:         saved.Status,
		ParentRunID:    saved.ParentRunID,
		SessionID:      saved.SessionID,
		SessionPath:    saved.SessionPath,
		Agent:          saved.Agent,
		Model:          saved.Model,
		CWD:            saved.CWD,
		Hostname:       saved.Hostname,
		StartedCommand: saved.StartedCommand,
		StartMethod:    saved.StartMethod,
		TerminalReason: saved.TerminalReason,
		CancelReason:   saved.CancellationReason,
		StaleReason:    saved.StaleReason,
		OrphanedReason: saved.OrphanedReason,
		CommandArgs:    append([]string(nil), saved.CommandArgs...),
		PID:            saved.PID,
		ParentPID:      saved.ParentPID,
		ProcessGroupID: saved.ProcessGroupID,
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

	return &saved, nil
}

func appendHeadlessUserMessageEvent(store *session.Store, id string, run session.HeadlessRun) error {
	if strings.TrimSpace(run.Prompt) == "" {
		return nil
	}

	if err := store.AppendHeadlessEvent(id, session.HeadlessEvent{
		Type:           session.HeadlessEventUserMessage,
		Status:         run.Status,
		Role:           string(llm.RoleUser),
		Message:        run.Prompt,
		ParentRunID:    run.ParentRunID,
		SessionID:      run.SessionID,
		SessionPath:    run.SessionPath,
		Agent:          run.Agent,
		Model:          run.Model,
		CWD:            run.CWD,
		Hostname:       run.Hostname,
		StartedCommand: run.StartedCommand,
		StartMethod:    run.StartMethod,
		CommandArgs:    append([]string(nil), run.CommandArgs...),
		PID:            run.PID,
		ParentPID:      run.ParentPID,
		ProcessGroupID: run.ProcessGroupID,
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

func finishHeadlessRun(store *session.Store, run *session.HeadlessRun, status session.HeadlessStatus, message string) {
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
	appendFinishedHeadlessEvent(store, run, status)
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

func appendFinishedHeadlessEvent(store *session.Store, run *session.HeadlessRun, status session.HeadlessStatus) {
	eventType := session.HeadlessEventType(status)
	if status == session.HeadlessStatusCompleted {
		eventType = session.HeadlessEventCompleted
	}

	if err := store.AppendHeadlessEvent(run.ID, session.HeadlessEvent{
		Type:           eventType,
		Status:         run.Status,
		ParentRunID:    run.ParentRunID,
		SessionID:      run.SessionID,
		SessionPath:    run.SessionPath,
		Message:        run.TerminalReason,
		Error:          run.Error,
		Agent:          run.Agent,
		Model:          run.Model,
		CWD:            run.CWD,
		Hostname:       run.Hostname,
		StartedCommand: run.StartedCommand,
		StartMethod:    run.StartMethod,
		TerminalReason: run.TerminalReason,
		CancelReason:   run.CancellationReason,
		StaleReason:    run.StaleReason,
		OrphanedReason: run.OrphanedReason,
		CommandArgs:    append([]string(nil), run.CommandArgs...),
		ChildRunIDs:    append([]string(nil), run.ChildRunIDs...),
		ExitCode:       run.ExitCode,
		PID:            run.PID,
		ParentPID:      run.ParentPID,
		ProcessGroupID: run.ProcessGroupID,
	}); err != nil {
		fmt.Fprintln(os.Stderr, "warning: "+err.Error())
	}
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
			fmt.Fprintln(stderr, "session: "+result.SessionID+" ("+result.SessionPath+")")

			if result.AgentLoopCheckpointPath != "" {
				fmt.Fprintln(stderr, "agent loop checkpoint: "+result.AgentLoopCheckpointPath)
			}
		}

		return nil
	}
}

// runOnceComplete handles replay or live agent-loop completion for one-shot
// mode. If a replay path is set, it loads the recorded response; otherwise it
// runs the agentic loop with tool execution support.
func runOnceComplete(
	ctx context.Context,
	reg *llm.Registry,
	params llm.CompleteParams,
	fallbackModels []string,
	agentLoopBudget llm.AgentLoopBudget,
	agentLoopCheckpointInterval int,
	responseOptions responseRecordOptions,
	checkpointPath string,
) (*llm.Response, error) {
	if responseOptions.ReplayPath != "" {
		resp, err := loadRecordedResponse(responseOptions.ReplayPath)
		if err != nil {
			return nil, err
		}

		if err := saveRecordedResponse(responseOptions.RecordPath, params, fallbackModels, resp); err != nil {
			return nil, err
		}

		return resp, nil
	}

	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("get working directory: %w", err)
	}

	executor := newBashExecutor(cwd, os.Stderr)

	resp, _, err := llm.AgentLoop(ctx, reg, params, fallbackModels, executor, llm.AgentLoopConfig{
		ConfirmContinue:    confirmContinueStdin,
		ConfirmToolCall:    confirmToolCallStdin,
		Budget:             agentLoopBudget,
		CheckpointInterval: agentLoopCheckpointInterval,
		Policy:             llm.BashToolPolicy,
		CheckpointSink:     agentLoopCheckpointSink(checkpointPath),
	})
	if err != nil {
		return nil, agentLoopError(err, checkpointPath)
	}

	if err := saveRecordedResponse(responseOptions.RecordPath, params, fallbackModels, resp); err != nil {
		return nil, err
	}

	return resp, nil
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

// newBashExecutor creates a ToolExecutor that runs bash commands in the given
// working directory, logging output to the provided writer.
func newBashExecutor(cwd string, logw io.Writer) llm.ToolExecutor {
	return func(ctx context.Context, call llm.ToolCall) llm.ToolResult {
		if call.Name != "bash" {
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

		emitFromContextWarning(ctx, events.Event{
			Type:    events.CommandExecute,
			Content: command,
			Metadata: map[string]string{
				"command":      command,
				"cwd":          cwd,
				"input":        command,
				"source":       "llm_tool",
				"tool_call_id": call.ID,
			},
		})

		result, shellErr := attshell.RunBash(ctx, attshell.Options{
			Command:        command,
			Dir:            cwd,
			Timeout:        5 * time.Minute,
			MaxOutputBytes: agentLoopToolOutputLimit(ctx),
		})

		output := formatShellContext(shellResultMsg{
			command: command,
			stdout:  result.Stdout,
			stderr:  result.Stderr,
			err:     shellErr,
		})
		fmt.Fprintln(logw, dimStyle.Render("  "+output))
		emitFromContextWarning(ctx, commandOutputEvent(
			"", "", "", "", cwd, command, output, shellErr,
			map[string]string{
				"source":       "llm_tool",
				"tool_call_id": call.ID,
			},
		))

		if shellErr != nil {
			return llm.ToolResult{ToolCallID: call.ID, Content: output, IsError: true}
		}

		return llm.ToolResult{ToolCallID: call.ID, Content: output}
	}
}
