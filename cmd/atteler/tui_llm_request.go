package main

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/tommoulard/atteler/pkg/contextpack"
	"github.com/tommoulard/atteler/pkg/contextref"
	"github.com/tommoulard/atteler/pkg/events"
	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/permission"
	"github.com/tommoulard/atteler/pkg/session"
	attshell "github.com/tommoulard/atteler/pkg/shell"
)

func callLLM(ctx context.Context, reg *llm.Registry, request llmRequest) tea.Cmd {
	return func() tea.Msg {
		if request.liveCh != nil {
			defer close(request.liveCh)
		}

		if request.confirmRequestCh != nil {
			defer close(request.confirmRequestCh)
		}

		eventLines := newEventLineBuffer(request.liveCh)
		ctx = events.WithEmitter(
			ctx,
			request.hookRunner.WithLogger(eventLines),
			request.eventBase,
		)
		ctx = contextWithPermissionPolicyPrompt(ctx, request)

		params := llm.CompleteParams{
			Model:    request.model,
			Messages: request.messages,
		}
		if request.hasAgent {
			params = request.agent.CompleteParams(request.model, request.messages)
		}

		prependReferenceContext(&params, request.referenceContext)
		prependAutonomyInstructions(&params, request.autonomy)

		applyGenerationParams(&params, request.generation)

		// When tools are enabled, run the agentic loop.
		if request.useTools {
			return finishLLMResponse(request.liveCh, callLLMWithTools(ctx, reg, params, request, eventLines))
		}

		emitRequestContextManifest(ctx, reg, params.Model, params.Messages, request)

		if err := validateRequestBudgetWithFallbacks(reg, params.Model, request.fallbackModels, params.Messages, request.maxInputTokens); err != nil {
			return finishLLMResponse(request.liveCh, llmResponseMsg{
				err:         err,
				completedAt: time.Now(),
				eventLines:  eventLines.Lines(),
			})
		}

		resp, err := reg.CompleteWithFallback(ctx, params, request.fallbackModels)
		if err != nil {
			return finishLLMResponse(request.liveCh, llmResponseMsg{
				err:         err,
				completedAt: time.Now(),
				eventLines:  eventLines.Lines(),
			})
		}

		var usage tokenUsage
		usage.addResponse(resp)

		completedAt := time.Now()

		return finishLLMResponse(request.liveCh, llmResponseMsg{
			completedAt:             completedAt,
			content:                 resp.Content,
			provider:                resp.Provider,
			model:                   resp.Model,
			eventLines:              eventLines.Lines(),
			providerFailureMetadata: resp.ProviderFailureMetadata(),
			routeDecision:           routeDecisionWithResponse(request.routeDecision, resp, routeTelemetryFromRegistry(reg)),
			providerCall: session.NewProviderCall(session.ProviderCallRecord{
				CompletedAt:     completedAt,
				Source:          "tui",
				Params:          params,
				Response:        resp,
				FallbackModels:  request.fallbackModels,
				ReferencedFiles: sessionFileReferencesFromLLMRequest(request),
			}),
			tokenUsage: usage,
		})
	}
}

func finishLLMResponse(liveCh chan<- tea.Msg, msg llmResponseMsg) tea.Msg {
	if liveCh == nil {
		return msg
	}

	msg.liveEvents = true
	liveCh <- msg

	return nil
}

func listenForLLMLiveMessage(liveCh <-chan tea.Msg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-liveCh
		if !ok {
			return nil
		}

		switch typed := msg.(type) {
		case llmEventLineMsg:
			typed.liveCh = liveCh

			return typed
		case llmToolOutputMsg:
			typed.liveCh = liveCh

			return typed
		default:
			return msg
		}
	}
}

// callLLMWithTools runs an agent loop where the LLM can execute built-in tools.
func callLLMWithTools(
	ctx context.Context,
	reg *llm.Registry,
	params llm.CompleteParams,
	request llmRequest,
	eventLines *eventLineBuffer,
) llmResponseMsg {
	// request.confirmRequestCh is closed by callLLM's deferred close; closing
	// it here too would double-close and panic.
	tools := defaultToolsForAgent(agentSelection{agent: request.agent, ok: request.hasAgent})

	params.Tools = tools

	// Inject a tool-availability reminder so the model knows it can (and
	// should) use the bash tool, even when the agent's system prompt
	// mentions other tools that are not wired up in this environment.
	if len(tools) > 0 {
		prependToolReminder(&params, tools)
	}

	emitRequestContextManifest(ctx, reg, params.Model, params.Messages, request)

	if err := validateRequestBudgetWithFallbacks(reg, params.Model, request.fallbackModels, params.Messages, request.maxInputTokens); err != nil {
		return llmResponseMsg{err: err, completedAt: time.Now(), eventLines: eventLines.Lines()}
	}

	toolNames := make([]string, 0, len(tools))
	for _, t := range tools {
		toolNames = append(toolNames, t.Name)
	}

	slog.Debug("callLLMWithTools",
		"agent", request.agent.Name,
		"hasAgent", request.hasAgent,
		"model", params.Model,
		"model_mode", params.ModelMode,
		"reasoning_level", params.ReasoningLevel,
		"tools", toolNames,
		"messages", len(params.Messages),
	)

	var toolLog []string

	executor := func(ctx context.Context, call llm.ToolCall) llm.ToolResult {
		if llm.IsFileToolName(call.Name) {
			result := executeFileTool(ctx, call, fileToolExecutorOptions{
				WorkingDir: request.workingDir,
				Autonomy:   request.autonomy,
			})
			toolLog = append(toolLog, result.Content)

			return result
		}

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

		commandEvent := events.Event{
			Type:    events.CommandExecute,
			Content: command,
			Metadata: map[string]string{
				"command":      command,
				"cwd":          request.workingDir,
				"input":        command,
				"source":       "llm_tool",
				"tool_call_id": call.ID,
				"autonomy":     request.autonomy.String(),
			},
		}

		result, err := attshell.RunBash(ctx, attshell.Options{
			Command:        command,
			Dir:            request.workingDir,
			Timeout:        5 * time.Minute, // Generous timeout for tool calls.
			MaxOutputBytes: agentLoopToolOutputLimit(ctx),
			Audit: attshell.AuditContext{
				Caller:      "atteler.tui.llm_tool",
				SessionID:   request.eventBase.SessionID,
				SessionPath: request.eventBase.SessionPath,
				Autonomy:    request.autonomy.String(),
			},
			Permission: request.permissionPolicy,
			StartCallback: func() {
				emitFromContextWarning(ctx, commandEvent)
			},
			OutputCallback: func(chunk attshell.OutputChunk) {
				sendLiveLLMToolOutput(request.liveCh, command, chunk)
				emitFromContextWarning(ctx, events.Event{
					Type:    events.CommandOutput,
					Content: string(chunk.Data),
					Metadata: map[string]string{
						"command":      command,
						"cwd":          request.workingDir,
						"partial":      "true",
						"sequence":     strconv.FormatInt(chunk.Sequence, 10),
						"source":       "llm_tool",
						"stream":       string(chunk.Stream),
						"tool_call_id": call.ID,
						"autonomy":     request.autonomy.String(),
					},
				})
			},
		})

		output := formatShellContext(shellResultMsg{
			command: command,
			stdout:  result.Stdout,
			stderr:  result.Stderr,
			err:     err,
		})
		toolLog = append(toolLog, output)
		emitFromContextWarning(ctx, commandOutputEvent(
			"", "", "", "", request.workingDir, command, output, err,
			map[string]string{
				"source":       "llm_tool",
				"tool_call_id": call.ID,
				"autonomy":     request.autonomy.String(),
			},
		))

		if err != nil {
			content := output
			// When the command timed out, append a recovery hint so the LLM
			// can decide to retry with a smaller scope or take corrective
			// action autonomously.
			if strings.Contains(err.Error(), "timed out") {
				content += "\n\n[TIMEOUT RECOVERY] The command timed out after the configured limit. " +
					"Consider: (1) retrying with a smaller scope or simpler command, " +
					"(2) splitting the work into smaller steps, " +
					"(3) checking if the command is hanging on user input, or " +
					"(4) increasing the timeout if the operation legitimately requires more time."
			}

			return llm.ToolResult{
				ToolCallID: call.ID,
				Content:    content,
				IsError:    true,
			}
		}

		return llm.ToolResult{
			ToolCallID: call.ID,
			Content:    output,
		}
	}

	confirmContinueFn, confirmToolFn := agentLoopConfirmCallbacks(ctx, request)

	costEstimator, err := agentLoopCostEstimatorForBudget(reg, params.Model, request.fallbackModels, request.agentLoopBudget)
	if err != nil {
		return llmResponseMsg{
			err:         err,
			completedAt: time.Now(),
			eventLines:  eventLines.Lines(),
			toolLog:     toolLog,
			liveEvents:  request.liveCh != nil,
		}
	}

	resp, messages, err := llm.AgentLoop(ctx, reg, params, request.fallbackModels, executor, llm.AgentLoopConfig{
		ConfirmContinue:    confirmContinueFn,
		ConfirmToolCall:    confirmToolFn,
		BeforeModelCall:    agentLoopManifestPreflight(ctx, reg, request),
		Budget:             request.agentLoopBudget,
		Autonomy:           request.autonomy,
		EstimateCostMicros: costEstimator,
		CheckpointInterval: request.agentLoopCheckpointInterval,
		Policy:             llm.DefaultToolPolicyForAutonomy(request.autonomy),
		CheckpointSink:     agentLoopCheckpointSink(request.agentLoopCheckpointPath),
	})
	if err != nil {
		return llmResponseMsg{
			err:         agentLoopError(err, request.agentLoopCheckpointPath),
			completedAt: time.Now(),
			eventLines:  eventLines.Lines(),
			toolLog:     toolLog,
			liveEvents:  request.liveCh != nil,
		}
	}

	var usage tokenUsage
	usage.addResponse(resp)

	completedAt := time.Now()

	providerParams := params
	if len(messages) > 0 {
		providerParams.Messages = append([]llm.Message(nil), messages...)
	}

	return llmResponseMsg{
		completedAt:             completedAt,
		content:                 resp.Content,
		provider:                resp.Provider,
		model:                   resp.Model,
		eventLines:              eventLines.Lines(),
		providerFailureMetadata: resp.ProviderFailureMetadata(),
		routeDecision:           routeDecisionWithResponse(request.routeDecision, resp, routeTelemetryFromRegistry(reg)),
		providerCall: session.NewProviderCall(session.ProviderCallRecord{
			CompletedAt:     completedAt,
			Source:          "tui_agent_loop",
			Params:          providerParams,
			Response:        resp,
			FallbackModels:  request.fallbackModels,
			ReferencedFiles: sessionFileReferencesFromLLMRequest(request),
		}),
		toolLog:    toolLog,
		tokenUsage: usage,
		liveEvents: request.liveCh != nil,
	}
}

func sessionFileReferencesFromLLMRequest(request llmRequest) []session.FileReference {
	if len(request.inlineReferenceEvents) == 0 {
		return sessionFileReferencesFromManifest(request.referenceManifest, request.eventBase.Agent)
	}

	referenceEvents := append([]contextref.ReferenceEvent(nil), request.referenceManifest.Entries...)
	referenceEvents = append(referenceEvents, request.inlineReferenceEvents...)

	manifest := contextref.BuildReferenceManifest(referenceEvents)
	if manifest.TokenEstimator == "" {
		manifest.TokenEstimator = request.referenceManifest.TokenEstimator
	}

	return sessionFileReferencesFromManifest(manifest, request.eventBase.Agent)
}

func contextWithPermissionPolicyPrompt(ctx context.Context, request llmRequest) context.Context {
	return contextWithTUIPermissionPrompt(ctx, request.permissionPolicy, request.confirmRequestCh, request.confirmResponseCh)
}

func sendLiveLLMToolOutput(liveCh chan<- tea.Msg, command string, chunk attshell.OutputChunk) {
	if liveCh == nil {
		return
	}

	liveCh <- llmToolOutputMsg{
		command:  command,
		stream:   string(chunk.Stream),
		data:     string(chunk.Data),
		sequence: chunk.Sequence,
	}
}

// agentLoopConfirmCallbacks builds TUI-backed callbacks for both legacy loop
// continuation checkpoints and require-confirm tool policy decisions.
func agentLoopConfirmCallbacks(ctx context.Context, request llmRequest) (func(int) bool, llm.ConfirmToolCallFunc) {
	if request.confirmRequestCh == nil || request.confirmResponseCh == nil {
		return nil, nil
	}

	confirmContinue := func(iterations int) bool {
		return sendAgentLoopConfirmation(ctx, request.confirmRequestCh, request.confirmResponseCh, agentLoopConfirmRequest{
			kind:       agentLoopConfirmCheckpoint,
			iterations: iterations,
			prompt:     fmt.Sprintf("Agent loop reached %d iterations. Continue? [Y/n] ", iterations),
		})
	}

	confirmTool := func(ctx context.Context, call llm.ToolCall, decision llm.ToolPolicyDecision) bool {
		return sendAgentLoopConfirmation(ctx, request.confirmRequestCh, request.confirmResponseCh, agentLoopConfirmRequest{
			kind:   agentLoopConfirmToolCall,
			prompt: agentLoopToolConfirmPrompt(call, decision),
		})
	}

	return confirmContinue, confirmTool
}

func sendAgentLoopConfirmation(
	ctx context.Context,
	requestCh chan<- agentLoopConfirmRequest,
	responseCh <-chan bool,
	request agentLoopConfirmRequest,
) bool {
	select {
	case requestCh <- request:
	case <-ctx.Done():
		return false
	}

	select {
	case answer := <-responseCh:
		return answer
	case <-ctx.Done():
		return false
	}
}

func agentLoopToolConfirmPrompt(call llm.ToolCall, decision llm.ToolPolicyDecision) string {
	command, ok := call.Input["command"].(string)
	if !ok {
		command = "<missing command>"
	}

	return fmt.Sprintf(
		"Agent tool call requires confirmation (%s): %s\n$ %s\nExecute? [y/N] ",
		decision.MatchedRule,
		decision.Reason,
		command,
	)
}

func permissionConfirmPrompt(request permission.Request, decision permission.Decision) string {
	action := strings.TrimSpace(request.Action)
	if action == "" {
		action = strings.TrimSpace(decision.Reason)
	}

	return fmt.Sprintf(
		"Permission policy requires confirmation (%s, policy: %s): %s\n%s\nAllow? [y/N] ",
		decision.Rule,
		permissionPromptPolicy(decision),
		decision.Reason,
		action,
	)
}

func permissionPromptPolicy(decision permission.Decision) string {
	policy := strings.TrimSpace(decision.Policy)
	if policy == "" {
		return unknownLabel
	}

	return policy
}

// prependReferenceContext injects pre-rendered reference content as a system
// message at the beginning of the messages array. This makes configured
// repository paths, documentation links, and other reference material available
// to the LLM for every request.
func prependReferenceContext(params *llm.CompleteParams, refCtx string) {
	if refCtx == "" {
		return
	}

	params.Messages = append(
		[]llm.Message{{Role: llm.RoleSystem, Content: refCtx}},
		params.Messages...,
	)
}

func requestMessagesForBudget(
	modelName string,
	messages []llm.Message,
	activeAgent agentSelection,
	generation generationSettings,
	referenceContext string,
	useTools bool,
) []llm.Message {
	params := llm.CompleteParams{
		Model:    modelName,
		Messages: messages,
	}
	if activeAgent.ok {
		params = activeAgent.agent.CompleteParams(modelName, messages)
	}

	prependReferenceContext(&params, referenceContext)
	applyGenerationParams(&params, generation)

	if useTools {
		tools := defaultToolsForAgent(activeAgent)

		if len(tools) > 0 {
			prependToolReminder(&params, tools)
		}
	}

	return params.Messages
}

func emitRequestContextManifest(ctx context.Context, reg *llm.Registry, modelName string, messages []llm.Message, request llmRequest) {
	emitFromContextWarning(ctx, requestContextManifestEvent(newRequestContextManifestForModelsWithInlineEvents(
		reg,
		modelName,
		request.fallbackModels,
		messages,
		request.maxInputTokens,
		request.inlineReferenceEvents,
		request.referenceManifest,
	)))
}

func agentLoopManifestPreflight(ctx context.Context, reg *llm.Registry, request llmRequest) func(iteration int, params llm.CompleteParams) error {
	return func(iteration int, params llm.CompleteParams) error {
		if iteration > 0 {
			emitRequestContextManifest(ctx, reg, params.Model, params.Messages, request)
		}

		return validateRequestBudgetWithFallbacks(reg, params.Model, request.fallbackModels, params.Messages, request.maxInputTokens)
	}
}

func newRequestContextManifestForModels(
	reg *llm.Registry,
	modelName string,
	fallbackModels []string,
	messages []llm.Message,
	maxInputTokens int,
	configuredManifest contextref.ReferenceManifest,
) requestContextManifest {
	return newRequestContextManifestForModelsWithInlineEvents(
		reg,
		modelName,
		fallbackModels,
		messages,
		maxInputTokens,
		nil,
		configuredManifest,
	)
}

func newRequestContextManifestForModelsWithInlineEvents(
	reg *llm.Registry,
	modelName string,
	fallbackModels []string,
	messages []llm.Message,
	maxInputTokens int,
	inlineEvents []contextref.ReferenceEvent,
	configuredManifest contextref.ReferenceManifest,
) requestContextManifest {
	primaryProvider, primaryModel := requestManifestModelIdentity(reg, modelName, fallbackModels)
	manifest := newRequestContextManifestWithInlineEvents(
		primaryProvider,
		primaryModel,
		messages,
		maxInputTokens,
		contextWindowForModel(reg, primaryModel),
		inlineEvents,
		configuredManifest,
	)

	seen := map[string]bool{strings.TrimSpace(primaryModel): true}
	for _, fallbackModel := range fallbackModels {
		fallbackModel = strings.TrimSpace(fallbackModel)
		if fallbackModel == "" || seen[fallbackModel] {
			continue
		}

		seen[fallbackModel] = true
		manifest.FallbackModelEstimates = append(manifest.FallbackModelEstimates, requestModelEstimate(
			providerNameForModel(reg, fallbackModel),
			fallbackModel,
			messages,
			maxInputTokens,
			contextWindowForModel(reg, fallbackModel),
		))
	}

	return manifest
}

func requestManifestModelIdentity(reg *llm.Registry, modelName string, fallbackModels []string) (providerName, manifestModel string) {
	providerName = providerNameForModel(reg, modelName)

	manifestModel = modelName

	if strings.TrimSpace(modelName) != "" {
		return providerName, manifestModel
	}

	for _, fallbackModel := range fallbackModels {
		fallbackModel = strings.TrimSpace(fallbackModel)
		if fallbackModel != "" {
			return providerNameForModel(reg, fallbackModel), fallbackModel
		}
	}

	resolvedProvider, resolvedModel, ok := resolveRegistryModel(reg, "")
	if !ok {
		return providerName, manifestModel
	}

	return resolvedProvider, resolvedModel
}

func validateRequestBudget(reg *llm.Registry, modelName string, messages []llm.Message, maxInputTokens int) error {
	estimate, estimatorSummary := estimateMessagesForModel(reg, modelName, messages)
	used := estimate.UpperBoundTokens

	if maxInputTokens > 0 && used > maxInputTokens {
		return fmt.Errorf("estimated input tokens upper bound %s (point=%s error_bound=%s estimator=%s) exceeds configured max_input_tokens %s", formatTokenCount(used), formatTokenCount(estimate.Tokens), formatTokenCount(estimate.ErrorBoundTokens), estimatorSummary, formatTokenCount(maxInputTokens))
	}

	if limit := contextWindowForModel(reg, modelName); limit > 0 && used > limit {
		displayModel := modelName
		if strings.TrimSpace(displayModel) == "" {
			_, resolvedModel, ok := resolveRegistryModel(reg, "")
			if ok {
				displayModel = resolvedModel
			}
		}

		return fmt.Errorf("estimated input tokens upper bound %s (point=%s error_bound=%s estimator=%s) exceeds %s context window %s", formatTokenCount(used), formatTokenCount(estimate.Tokens), formatTokenCount(estimate.ErrorBoundTokens), estimatorSummary, displayModel, formatTokenCount(limit))
	}

	return nil
}

func validateRequestBudgetWithFallbacks(reg *llm.Registry, modelName string, fallbackModels []string, messages []llm.Message, maxInputTokens int) error {
	models := requestBudgetModels(modelName, fallbackModels)
	if len(models) == 0 {
		return validateRequestBudget(reg, "", messages, maxInputTokens)
	}

	for _, model := range models {
		if err := validateRequestBudget(reg, model, messages, maxInputTokens); err != nil {
			return fmt.Errorf("model %s budget: %w", model, err)
		}
	}

	return nil
}

func requestBudgetModels(modelName string, fallbackModels []string) []string {
	seen := make(map[string]bool, len(fallbackModels)+1)
	models := make([]string, 0, len(fallbackModels)+1)

	for _, model := range append([]string{modelName}, fallbackModels...) {
		model = strings.TrimSpace(model)
		if model == "" || seen[model] {
			continue
		}

		seen[model] = true
		models = append(models, model)
	}

	return models
}

func estimateMessagesForModel(reg *llm.Registry, modelName string, messages []llm.Message) (estimate contextpack.TokenEstimate, estimatorSummary string) {
	providerName := providerNameForModel(reg, modelName)

	estimatorModel := modelName

	if strings.TrimSpace(modelName) == "" {
		resolvedProvider, resolvedModel, ok := resolveRegistryModel(reg, "")
		if ok {
			providerName = resolvedProvider
			estimatorModel = resolvedModel
		}
	}

	estimator := contextpack.NewEstimator(providerName, estimatorModel)

	return estimator.EstimateMessages(messages), contextEstimatorSummary(estimator.Profile())
}

func providerNameForModel(reg *llm.Registry, modelName string) string {
	if reg == nil {
		return ""
	}

	resolvedProvider, ok := reg.ProviderForModel(modelName)
	if !ok {
		return ""
	}

	return resolvedProvider
}

func resolveRegistryModel(reg *llm.Registry, modelName string) (providerName, providerModel string, ok bool) {
	if reg == nil {
		return "", "", false
	}

	return reg.ResolveModel(modelName)
}

func contextWindowForModel(reg *llm.Registry, modelName string) int {
	if reg != nil {
		if limit := reg.ContextWindow(modelName); limit > 0 {
			return limit
		}
	}

	return contextpack.ModelContextWindow(providerNameForModel(reg, modelName), modelName)
}

func contextEstimatorSummary(profile contextpack.EstimatorProfile) string {
	parts := []string{
		profile.Name,
		"provider=" + profile.Provider,
		fmt.Sprintf("cpt=%d", profile.CharsPerToken),
		fmt.Sprintf("overhead=%d", profile.MessageOverheadTokens),
		fmt.Sprintf("err=%d%%", profile.ErrorBoundPercent),
	}
	if strings.TrimSpace(profile.Calibration) != "" {
		parts = append(parts, "calibration="+strings.TrimSpace(profile.Calibration))
	}

	if strings.TrimSpace(profile.Model) != "" {
		parts = append(parts, "model="+strings.TrimSpace(profile.Model))
	}

	return strings.Join(parts, ";")
}

func expandReferences(messages []llm.Message, opts contextref.Options) ([]llm.Message, []contextref.Reference, []contextref.ReferenceEvent, error) {
	if len(messages) == 0 {
		return nil, nil, nil, nil
	}

	out := append([]llm.Message(nil), messages...)
	for i := len(out) - 1; i >= 0; i-- {
		if out[i].Role != llm.RoleUser {
			continue
		}

		result, inlineEvents, err := contextref.ExpandWithReport(out[i].Content, opts)
		if err != nil {
			inlineEvents = omitLoadedConfiguredReferenceEvents(inlineEvents, "inline reference block omitted because expansion failed")
			return nil, nil, inlineEvents, fmt.Errorf("expand context references: %w", err)
		}

		out[i].Content = result.Prompt

		return out, result.References, inlineEvents, nil
	}

	return out, nil, nil, nil
}

func referenceSummary(refs []contextref.Reference) string {
	parts := make([]string, 0, len(refs))
	for _, ref := range refs {
		path := ref.Path
		if ref.Kind != "" && ref.Kind != "file" {
			path = ref.Kind + ":" + path
		}

		label := fmt.Sprintf("%s (%d bytes", path, ref.Bytes)
		if ref.Truncated {
			label += ", truncated"
		}

		label += ")"
		parts = append(parts, label)
	}

	return strings.Join(parts, ", ")
}

func referenceMetadata(refs []contextref.Reference) map[string]string {
	if len(refs) == 0 {
		return nil
	}

	return map[string]string{
		"context_references": referenceSummary(refs),
	}
}
