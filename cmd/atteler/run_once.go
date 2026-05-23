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
	"strings"
	"time"

	"github.com/tommoulard/atteler/pkg/agent"
	"github.com/tommoulard/atteler/pkg/contextref"
	"github.com/tommoulard/atteler/pkg/events"
	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/session"
	attshell "github.com/tommoulard/atteler/pkg/shell"
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
	ModelMode               string     `json:"model_mode,omitempty"`
	Content                 string     `json:"content"`
	TokenUsage              tokenUsage `json:"token_usage"`
}

//nolint:govet // Field order follows request-preparation flow; padding is irrelevant here.
type runOncePrepared struct {
	activeAgent     agentSelection
	generation      generationSettings
	requestMessages []llm.Message
	refs            []contextref.Reference
	fallbackModels  []string
	prompt          string
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
	ModelMode      string        `json:"model_mode,omitempty"`
	FallbackModels []string      `json:"fallback_models,omitempty"`
	Messages       []llm.Message `json:"messages"`
	MaxTokens      int           `json:"max_tokens,omitempty"`
	ReasoningLevel string        `json:"reasoning_level,omitempty"`
}

type responseRecordPayload struct {
	Content           string `json:"content"`
	Model             string `json:"model,omitempty"`
	InputTokens       int    `json:"input_tokens,omitempty"`
	CachedInputTokens int    `json:"cached_input_tokens,omitempty"`
	OutputTokens      int    `json:"output_tokens,omitempty"`
}

func saveRecordedResponse(path string, params llm.CompleteParams, fallbackModels []string, resp *llm.Response) error {
	if strings.TrimSpace(path) == "" || resp == nil {
		return nil
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
			ReasoningLevel: params.ReasoningLevel,
		},
		Response: responseRecordPayload{
			Content:           resp.Content,
			Model:             resp.Model,
			InputTokens:       resp.InputTokens,
			CachedInputTokens: resp.CachedInputTokens,
			OutputTokens:      resp.OutputTokens,
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
		Content:           record.Response.Content,
		Model:             record.Response.Model,
		InputTokens:       record.Response.InputTokens,
		CachedInputTokens: record.Response.CachedInputTokens,
		OutputTokens:      record.Response.OutputTokens,
	}, nil
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
		return err
	}

	prepared, err := prepareRunOnceRequest(
		agents,
		contextOptions,
		selectedModel,
		selectedAgent,
		fallbackModels,
		generationDefaults,
		generationOverrides,
		modelLocked,
		prompt,
	)
	if err != nil {
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

	if userSaveErr := saveRunOnceUserMessage(ctx, hooks, store, &sessionState, prepared); userSaveErr != nil {
		return userSaveErr
	}

	params := llm.CompleteParams{
		Model:    prepared.requestModel,
		Messages: append(append([]llm.Message(nil), sessionState.Messages[:len(sessionState.Messages)-1]...), prepared.requestMessages...),
	}
	if prepared.activeAgent.ok {
		params = prepared.activeAgent.agent.CompleteParams(prepared.requestModel, params.Messages)
	}

	refCtx := buildReferenceContext(ctx, referenceContext, prepared.activeAgent, contextOptions)
	prependReferenceContext(&params, refCtx)

	applyGenerationParams(&params, prepared.generation)

	if budgetErr := validateRequestBudget(reg, params.Model, params.Messages, maxInputTokens); budgetErr != nil {
		return budgetErr
	}

	ctx = events.WithEmitter(ctx, hooks, events.Event{
		SessionID:   sessionState.ID,
		SessionPath: store.Path(sessionState.ID),
		Agent:       prepared.activeAgent.name,
		Model:       prepared.requestModel,
	})

	headlessRun, err := startHeadlessRun(
		store,
		executionOptions,
		sessionState,
		prepared.prompt,
		prepared.requestModel,
		prepared.generation.ModelMode,
		prepared.activeAgent.name,
	)
	if err != nil {
		return err
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
		"model_mode", params.ModelMode,
		"reasoning_level", params.ReasoningLevel,
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

	if err := saveRunOnceAssistantResponse(ctx, hooks, store, &sessionState, prepared.activeAgent.name, resp); err != nil {
		finishHeadlessRun(store, headlessRun, session.HeadlessStatusFailed, err.Error())
		return err
	}

	var usage tokenUsage
	usage.addResponse(resp)

	result := runOnceResult{
		SessionID:               sessionState.ID,
		SessionPath:             store.Path(sessionState.ID),
		AgentLoopCheckpointPath: checkpointPath,
		Agent:                   prepared.activeAgent.name,
		Model:                   resp.Model,
		ModelMode:               prepared.generation.ModelMode,
		Content:                 resp.Content,
		TokenUsage:              usage,
	}
	if headlessRun != nil {
		result.HeadlessID = headlessRun.ID
		if resp.Model != "" {
			headlessRun.Model = resp.Model
		}

		if err := store.AppendHeadlessLog(headlessRun.ID, fmt.Sprintf("assistant_message\t%s\tbytes=%d\n", time.Now().UTC().Format(time.RFC3339), len(resp.Content))); err != nil {
			fmt.Fprintln(os.Stderr, "warning: "+err.Error())
		}

		finishHeadlessRun(store, headlessRun, session.HeadlessStatusCompleted, "")
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

func prepareRunOnceRequest(
	agents *agent.Registry,
	contextOptions contextref.Options,
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

	requestModel, fallbackModels := requestModelAndFallbacks(selectedModel, modelLocked, fallbackModels, activeAgent)

	return runOncePrepared{
		activeAgent:     activeAgent,
		generation:      generationForRequest(generationDefaults, generationOverrides, activeAgent),
		requestMessages: requestMessages,
		refs:            refs,
		fallbackModels:  fallbackModels,
		prompt:          userPrompt,
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

func startHeadlessRun(
	store *session.Store,
	options runOnceExecutionOptions,
	sessionState session.Session,
	prompt string,
	modelName string,
	modelMode string,
	agentName string,
) (*session.HeadlessRun, error) {
	if !options.Headless {
		return nil, nil
	}

	if store == nil {
		return nil, errors.New("headless mode requires a session store")
	}

	id := strings.TrimSpace(options.HeadlessID)
	if id == "" {
		id = session.New("", nil).ID
	}

	run := session.HeadlessRun{
		ID:             id,
		SessionID:      sessionState.ID,
		SessionPath:    store.Path(sessionState.ID),
		Prompt:         strings.TrimSpace(prompt),
		Model:          modelName,
		ModelMode:      strings.TrimSpace(modelMode),
		Agent:          agentName,
		StartedCommand: strings.Join(os.Args, " "),
		Status:         session.HeadlessStatusRunning,
		PrivateLogs:    options.HeadlessPrivateLog,
	}
	if err := store.SaveHeadlessRun(run); err != nil {
		return nil, fmt.Errorf("start headless run: %w", err)
	}

	saved, err := store.LoadHeadlessRun(id)
	if err != nil {
		return nil, fmt.Errorf("load started headless run: %w", err)
	}

	if err := store.AppendHeadlessLog(id, "started\t"+time.Now().UTC().Format(time.RFC3339)+"\tsession="+sessionState.ID+"\n"); err != nil {
		return nil, fmt.Errorf("write headless start log: %w", err)
	}

	return &saved, nil
}

func finishHeadlessRun(store *session.Store, run *session.HeadlessRun, status session.HeadlessStatus, message string) {
	if store == nil || run == nil {
		return
	}

	now := time.Now().UTC()
	run.Status = status
	run.CompletedAt = &now

	run.Error = strings.TrimSpace(message)
	if run.CancellationReason == "" && isCancellationMessage(run.Error) {
		run.CancellationReason = run.Error
	}

	exitCode := 0
	if status != session.HeadlessStatusCompleted {
		exitCode = 1
	}

	run.ExitCode = &exitCode
	if err := store.SaveHeadlessRun(*run); err != nil {
		fmt.Fprintln(os.Stderr, "warning: "+err.Error())
	}

	logLine := string(status) + "\t" + now.Format(time.RFC3339)
	if run.Error != "" {
		logLine += "\terror=" + run.Error
	}

	if err := store.AppendHeadlessLog(run.ID, logLine+"\n"); err != nil {
		fmt.Fprintln(os.Stderr, "warning: "+err.Error())
	}
}

func isCancellationMessage(message string) bool {
	message = strings.ToLower(strings.TrimSpace(message))
	if message == "" {
		return false
	}

	return strings.Contains(message, "context canceled") ||
		strings.Contains(message, "context deadline exceeded") ||
		strings.Contains(message, "canceled")
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
