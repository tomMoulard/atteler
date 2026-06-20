package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/tommoulard/atteler/pkg/events"
	"github.com/tommoulard/atteler/pkg/permission"
)

const (
	defaultAnthropicBase    = "https://api.anthropic.com"
	defaultAnthropicVersion = "2023-06-01"
	anthropicOAuthBetas     = "claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14,structured-outputs-2025-11-13"

	anthropicCacheControlTypeEphemeral = "ephemeral"
	anthropicContentTypeText           = "text"
	anthropicContentTypeToolResult     = "tool_result"
	anthropicContentTypeToolUse        = "tool_use"
)

// AnthropicProvider calls the Anthropic Messages API.
type AnthropicProvider struct {
	client  *http.Client
	apiKey  string
	baseURL string
	bearer  bool
}

// NewAnthropicProvider is kept for source compatibility only.
//
// Deprecated: use NewAnthropicProviderContext so credential discovery inherits
// caller cancellation and deadlines.
func NewAnthropicProvider() (*AnthropicProvider, error) {
	return nil, ErrContextRequired
}

// NewAnthropicProviderContext creates a provider using ResolveAnthropicKeyContext.
// The base URL can be overridden with ANTHROPIC_BASE_URL.
func NewAnthropicProviderContext(ctx context.Context) (*AnthropicProvider, error) {
	return NewAnthropicProviderWithConfigContext(ctx, ProviderConfig{})
}

// NewAnthropicProviderWithConfig is kept for source compatibility only.
//
// Deprecated: use NewAnthropicProviderWithConfigContext so credential
// discovery inherits caller cancellation and deadlines.
func NewAnthropicProviderWithConfig(_ ProviderConfig) (*AnthropicProvider, error) {
	return nil, ErrContextRequired
}

// NewAnthropicProviderWithConfigContext creates a provider using
// ResolveAnthropicKeyWithConfigContext and optional config values.
// ANTHROPIC_BASE_URL overrides cfg.BaseURL.
func NewAnthropicProviderWithConfigContext(ctx context.Context, cfg ProviderConfig) (*AnthropicProvider, error) {
	key, bearer, err := ResolveAnthropicKeyWithConfigContext(ctx, cfg)
	if err != nil {
		return nil, err
	}

	return &AnthropicProvider{
		apiKey:  key,
		bearer:  bearer,
		baseURL: configuredBaseURL("ANTHROPIC_BASE_URL", cfg.BaseURL, defaultAnthropicBase),
		client:  providerHTTPClient(cfg),
	}, nil
}

// Name returns the provider name.
func (a *AnthropicProvider) Name() string { return providerAnthropic }

// Models returns the static list of supported models (fallback).
func (a *AnthropicProvider) Models() []string {
	static := []string{
		"claude-sonnet-4-20250514",
		"claude-haiku-4-5-20251001",
		"claude-opus-4-20250514",
	}

	return mergeModelLists(static, catalogModelsByProvider()[providerAnthropic])
}

// ---------------------------------------------------------------------------
// Anthropic Models API
// ---------------------------------------------------------------------------

type anthropicModelsResponse struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

// FetchModels queries GET /v1/models to discover available models.
func (a *AnthropicProvider) FetchModels(ctx context.Context) ([]string, error) {
	if err := requireCredentialContext(ctx); err != nil {
		return nil, err
	}

	if policyErr := authorizeProviderPermission(ctx, providerAnthropic, "fetch Anthropic models", a.baseURL+"/v1/models", permission.OperationNetwork, permission.OperationCredentialAccess); policyErr != nil {
		return nil, policyErr
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, a.baseURL+"/v1/models", http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("anthropic: new request: %w", err)
	}

	httpReq.Header.Set("anthropic-version", defaultAnthropicVersion)
	a.setAuthHeaders(httpReq)

	resp, err := a.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("anthropic: models request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("anthropic: read models body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, newProviderHTTPError(providerAnthropic, resp, body)
	}

	var mr anthropicModelsResponse
	if err := json.Unmarshal(body, &mr); err != nil {
		return nil, fmt.Errorf("anthropic: unmarshal models: %w", err)
	}

	out := make([]string, 0, len(mr.Data))
	for _, m := range mr.Data {
		out = append(out, m.ID)
	}

	return out, nil
}

// ProviderWarnings reports when the Anthropic provider is using bearer-token
// routing that requires Anthropic beta headers. This can happen with explicit
// bearer tokens or borrowed Claude Code/Forge credentials.
func (a *AnthropicProvider) ProviderWarnings() []string {
	if !a.bearer {
		return nil
	}

	return []string{
		"uses Anthropic bearer-token auth with beta routing headers; set providers.anthropic.disable_private_adapter, " +
			"ATTELER_DISABLE_CLAUDE_CODE_ADAPTER=1, or ATTELER_DISABLE_BORROWED_CREDENTIAL_ADAPTERS=1 " +
			"to prevent borrowed Claude Code/Forge fallback",
	}
}

// HealthCheck verifies that the Anthropic API is reachable and the credentials
// are valid by issuing a lightweight GET /v1/models request.
func (a *AnthropicProvider) HealthCheck(ctx context.Context) error {
	_, err := a.FetchModels(ctx)
	return err
}

type anthropicRequest struct {
	Temperature *float64                `json:"temperature,omitempty"`
	TopP        *float64                `json:"top_p,omitempty"`
	Model       string                  `json:"model"`
	System      []anthropicContentBlock `json:"system,omitempty"`
	Thinking    *anthropicThinking      `json:"thinking,omitempty"`
	Messages    []anthropicMessage      `json:"messages"`
	Stop        []string                `json:"stop_sequences,omitempty"`
	Tools       []anthropicTool         `json:"tools,omitempty"`
	MaxTokens   int                     `json:"max_tokens"`
	Stream      bool                    `json:"stream,omitempty"`
}

type anthropicThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens"`
}

//nolint:govet // Field order keeps Anthropic wire JSON readable in fixtures.
type anthropicTool struct {
	InputSchema  map[string]any         `json:"input_schema"`
	Name         string                 `json:"name"`
	Description  string                 `json:"description"`
	CacheControl *anthropicCacheControl `json:"cache_control,omitempty"`
}

type anthropicCacheControl struct {
	Type string `json:"type"`
}

// anthropicMessage uses json.RawMessage for Content so it can be either
// a plain string (user text) or an array of content blocks (tool results,
// assistant tool_use responses).
type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// anthropicContentBlock is a single block in an Anthropic message content array.
//
//nolint:govet // Field order keeps Anthropic wire JSON readable in fixtures.
type anthropicContentBlock struct {
	Type         string                 `json:"type"`
	Text         string                 `json:"text,omitempty"`
	ID           string                 `json:"id,omitempty"`
	Name         string                 `json:"name,omitempty"`
	Input        map[string]any         `json:"input,omitempty"`
	ToolUseID    string                 `json:"tool_use_id,omitempty"`
	Content      string                 `json:"content,omitempty"`
	IsError      bool                   `json:"is_error,omitempty"`
	CacheControl *anthropicCacheControl `json:"cache_control,omitempty"`
}

type anthropicResponse struct {
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
	Model      string                  `json:"model"`
	StopReason string                  `json:"stop_reason"`
	Content    []anthropicContentBlock `json:"content"`
	Usage      struct {
		InputTokens              int `json:"input_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		OutputTokens             int `json:"output_tokens"`
	} `json:"usage"`
}

// Complete performs a chat completion using the Anthropic Messages API.
func (a *AnthropicProvider) Complete(ctx context.Context, params CompleteParams) (*Response, error) {
	if err := requireCredentialContext(ctx); err != nil {
		return nil, err
	}

	params, adjustments, err := prepareCompleteParamsForProvider(providerAnthropic, params)
	if err != nil {
		return nil, err
	}

	req, err := buildAnthropicRequestForProvider(providerAnthropic, params)
	if err != nil {
		return nil, err
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("anthropic: marshal: %w", err)
	}

	if policyErr := authorizeProviderPermission(ctx, providerAnthropic, "call Anthropic messages", a.baseURL+"/v1/messages", permission.OperationNetwork, permission.OperationCredentialAccess); policyErr != nil {
		return nil, policyErr
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("anthropic: new request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("anthropic-version", defaultAnthropicVersion)
	a.setAuthHeaders(httpReq)

	if len(adjustments) > 0 {
		emitActivity(ctx, events.Event{
			Type:     events.CommandExecute,
			Model:    params.Model,
			Metadata: anthropicCommandMetadata(adjustments),
		})
	}

	resp, err := a.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("anthropic: request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("anthropic: read body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, newProviderHTTPError(providerAnthropic, resp, respBody)
	}

	var ar anthropicResponse
	if err := json.Unmarshal(respBody, &ar); err != nil {
		return nil, fmt.Errorf("anthropic: unmarshal: %w", err)
	}

	if ar.Error != nil {
		return nil, newProviderPayloadError(providerAnthropic, resp.StatusCode, resp.Header, ar.Error.Type, ar.Error.Message)
	}

	result := parseAnthropicResponse(ar)
	result.Provider = providerAnthropic

	return result, nil
}

// CompleteStream performs a streaming completion using Anthropic's Messages
// SSE protocol. Setup failures are returned directly; once a channel is
// returned, read/provider failures are delivered as terminal error chunks.
func (a *AnthropicProvider) CompleteStream(ctx context.Context, params CompleteParams) (<-chan Chunk, error) {
	if err := requireCredentialContext(ctx); err != nil {
		return nil, err
	}

	params, adjustments, err := prepareCompleteParamsForProvider(providerAnthropic, params)
	if err != nil {
		return nil, err
	}

	req, err := buildAnthropicRequestForProvider(providerAnthropic, params)
	if err != nil {
		return nil, err
	}

	req.Stream = true

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("anthropic: marshal stream: %w", err)
	}

	if policyErr := authorizeProviderPermission(ctx, providerAnthropic, "call Anthropic messages stream", a.baseURL+"/v1/messages", permission.OperationNetwork, permission.OperationCredentialAccess); policyErr != nil {
		return nil, policyErr
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("anthropic: new stream request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("anthropic-version", defaultAnthropicVersion)
	a.setAuthHeaders(httpReq)

	if len(adjustments) > 0 {
		emitActivity(ctx, events.Event{
			Type:     events.CommandExecute,
			Model:    params.Model,
			Metadata: anthropicCommandMetadata(adjustments),
		})
	}

	startedAt := time.Now()

	resp, err := a.client.Do(httpReq) //nolint:bodyclose // Successful streaming responses are closed by the goroutine below.
	if err != nil {
		return nil, fmt.Errorf("anthropic: stream request: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()

		respBody, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return nil, fmt.Errorf("anthropic: read stream error body: %w", readErr)
		}

		return nil, newProviderHTTPError(providerAnthropic, resp, respBody)
	}

	ch := make(chan Chunk, DefaultStreamBuffer)

	go func() {
		defer close(ch)
		defer resp.Body.Close()

		streamAnthropicMessages(ctx, resp.Body, ch, anthropicStreamConfig{
			providerName:   providerAnthropic,
			requestedModel: params.Model,
			header:         resp.Header,
			startedAt:      startedAt,
			now:            time.Now,
		})
	}()

	return ch, nil
}

func anthropicCommandMetadata(adjustments []completeParamAdjustment) map[string]string {
	metadata := map[string]string{
		"command":  "anthropic.messages",
		"provider": providerAnthropic,
	}
	if len(adjustments) > 0 {
		metadata["option_adjustments"] = formatCompleteParamAdjustments(adjustments)
	}

	return metadata
}

func parseAnthropicResponse(ar anthropicResponse) *Response {
	result := &Response{
		Model:                 ar.Model,
		StopReason:            anthropicStopReason(ar.StopReason),
		InputTokens:           ar.Usage.InputTokens + ar.Usage.CacheCreationInputTokens + ar.Usage.CacheReadInputTokens,
		CachedInputTokens:     ar.Usage.CacheReadInputTokens,
		CacheWriteInputTokens: ar.Usage.CacheCreationInputTokens,
		OutputTokens:          ar.Usage.OutputTokens,
	}

	var textParts strings.Builder

	for _, block := range ar.Content {
		switch block.Type {
		case anthropicContentTypeText:
			textParts.WriteString(block.Text)
		case anthropicContentTypeToolUse:
			result.ToolCalls = append(result.ToolCalls, ToolCall{
				ID:    block.ID,
				Name:  block.Name,
				Input: block.Input,
			})
		}
	}

	result.Content = textParts.String()

	return result
}

func anthropicStopReason(reason string) StopReason {
	switch reason {
	case "end_turn", "stop_sequence":
		return StopEndTurn
	case anthropicContentTypeToolUse:
		return StopToolUse
	case "max_tokens":
		return StopMaxToks
	default:
		return StopUnknown
	}
}

type anthropicStreamConfig struct {
	header         http.Header
	now            func() time.Time
	startedAt      time.Time
	providerName   string
	requestedModel string
}

type anthropicStreamState struct {
	toolBlocks         map[int]*anthropicStreamToolBlock
	now                func() time.Time
	startedAt          time.Time
	out                Response
	firstTokenRecorded bool
	finished           bool
	sawContent         bool
}

type anthropicStreamToolBlock struct {
	id        string
	name      string
	input     map[string]any
	inputJSON strings.Builder
	seen      bool
}

type anthropicStreamEvent struct {
	Message      *anthropicStreamMessage      `json:"message,omitempty"`
	ContentBlock *anthropicContentBlock       `json:"content_block,omitempty"`
	Delta        *anthropicStreamDelta        `json:"delta,omitempty"`
	Error        *anthropicStreamErrorPayload `json:"error,omitempty"`
	Usage        *anthropicStreamUsage        `json:"usage,omitempty"`
	Type         string                       `json:"type"`
	Index        int                          `json:"index,omitempty"`
}

type anthropicStreamMessage struct {
	Model string               `json:"model"`
	Usage anthropicStreamUsage `json:"usage"`
}

type anthropicStreamDelta struct {
	Type        string `json:"type"`
	Text        string `json:"text,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
	StopReason  string `json:"stop_reason,omitempty"`
}

type anthropicStreamErrorPayload struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type anthropicStreamUsage struct {
	InputTokens              int `json:"input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	OutputTokens             int `json:"output_tokens"`
}

//nolint:govet // Transient parser action; field order keeps the emitted chunk first for readability.
type anthropicStreamAction struct {
	chunk    Chunk
	err      error
	emit     bool
	terminal bool
}

func (s *anthropicStreamState) handlePayload(
	payload string,
	providerName string,
	header http.Header,
) anthropicStreamAction {
	var event anthropicStreamEvent
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		return anthropicStreamAction{err: fmt.Errorf("%s: stream unmarshal: %w", providerName, err)}
	}

	switch event.Type {
	case "message_start":
		s.recordMessageStart(event.Message)
	case "content_block_start":
		s.recordContentBlockStart(event.Index, event.ContentBlock)
	case "content_block_delta":
		return s.handleContentBlockDelta(event.Index, event.Delta)
	case "content_block_stop":
		s.finishToolBlock(event.Index)
	case "message_delta":
		s.recordMessageDelta(event.Delta, event.Usage)
	case "message_stop":
		s.finished = true

		return anthropicStreamAction{chunk: s.finalChunk(), emit: true, terminal: true}
	case "error":
		return anthropicStreamError(providerName, header, event.Error)
	}

	return anthropicStreamAction{}
}

func (s *anthropicStreamState) recordMessageStart(message *anthropicStreamMessage) {
	if message == nil {
		return
	}

	if message.Model != "" {
		s.out.Model = message.Model
	}

	s.applyUsage(message.Usage)
}

func (s *anthropicStreamState) handleContentBlockDelta(index int, delta *anthropicStreamDelta) anthropicStreamAction {
	if delta == nil {
		return anthropicStreamAction{}
	}

	switch delta.Type {
	case "text_delta":
		return s.textDeltaChunk(delta.Text)
	case "input_json_delta":
		s.recordToolInputDelta(index, delta.PartialJSON)
	}

	return anthropicStreamAction{}
}

func (s *anthropicStreamState) textDeltaChunk(text string) anthropicStreamAction {
	if text == "" {
		return anthropicStreamAction{}
	}

	s.recordFirstToken()
	s.sawContent = true

	return anthropicStreamAction{
		chunk: Chunk{
			Content:           text,
			Provider:          s.out.Provider,
			Model:             s.out.Model,
			FirstTokenLatency: s.out.FirstTokenLatency,
		},
		emit: true,
	}
}

func (s *anthropicStreamState) recordMessageDelta(delta *anthropicStreamDelta, usage *anthropicStreamUsage) {
	if delta != nil && delta.StopReason != "" {
		s.out.StopReason = anthropicStopReason(delta.StopReason)
	}

	if usage != nil {
		s.applyUsage(*usage)
	}
}

func anthropicStreamError(providerName string, header http.Header, payload *anthropicStreamErrorPayload) anthropicStreamAction {
	if payload == nil {
		return anthropicStreamAction{err: fmt.Errorf("%s: stream error", providerName)}
	}

	return anthropicStreamAction{
		err: newProviderPayloadError(providerName, http.StatusOK, header, payload.Type, payload.Message),
	}
}

func streamAnthropicMessages(ctx context.Context, r io.Reader, ch chan<- Chunk, cfg anthropicStreamConfig) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	state := &anthropicStreamState{
		startedAt: cfg.startedAt,
		now:       cfg.now,
		out: Response{
			Provider: cfg.providerName,
			Model:    cfg.requestedModel,
		},
	}

	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			sendStreamTerminalError(ctx, ch, fmt.Errorf("%s: stream canceled: %w", cfg.providerName, err))

			return
		}

		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}

		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}

		action := state.handlePayload(payload, cfg.providerName, cfg.header)
		if action.err != nil {
			sendStreamTerminalError(ctx, ch, action.err)

			return
		}

		if action.emit && !sendStreamChunk(ctx, ch, action.chunk) {
			return
		}

		if action.terminal {
			return
		}
	}

	if err := scanner.Err(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			sendStreamTerminalError(ctx, ch, fmt.Errorf("%s: stream canceled: %w", cfg.providerName, ctxErr))

			return
		}

		sendStreamTerminalError(ctx, ch, fmt.Errorf("%s: stream read: %w", cfg.providerName, err))

		return
	}

	if err := ctx.Err(); err != nil {
		sendStreamTerminalError(ctx, ch, fmt.Errorf("%s: stream canceled: %w", cfg.providerName, err))

		return
	}

	if state.finished {
		sendStreamChunk(ctx, ch, state.finalChunk())

		return
	}

	sendStreamTerminalError(ctx, ch, fmt.Errorf("%s: stream incomplete: %w", cfg.providerName, ErrStreamIncomplete))
}

func (s *anthropicStreamState) recordContentBlockStart(index int, block *anthropicContentBlock) {
	if block == nil || block.Type != string(StopToolUse) {
		return
	}

	if s.toolBlocks == nil {
		s.toolBlocks = make(map[int]*anthropicStreamToolBlock)
	}

	s.toolBlocks[index] = &anthropicStreamToolBlock{
		id:    block.ID,
		name:  block.Name,
		input: cloneAnyMap(block.Input),
		seen:  true,
	}
}

func (s *anthropicStreamState) recordToolInputDelta(index int, partial string) {
	if partial == "" {
		return
	}

	if s.toolBlocks == nil {
		s.toolBlocks = make(map[int]*anthropicStreamToolBlock)
	}

	block := s.toolBlocks[index]
	if block == nil {
		block = &anthropicStreamToolBlock{seen: true}
		s.toolBlocks[index] = block
	}

	block.inputJSON.WriteString(partial)
}

func (s *anthropicStreamState) finishToolBlock(index int) {
	if s.toolBlocks == nil {
		return
	}

	block := s.toolBlocks[index]
	if block == nil || block.inputJSON.Len() == 0 {
		return
	}

	var input map[string]any
	if err := json.Unmarshal([]byte(block.inputJSON.String()), &input); err != nil {
		input = map[string]any{"raw": block.inputJSON.String()}
	}

	block.input = input
}

func (s *anthropicStreamState) finalChunk() Chunk {
	toolCalls := s.finalToolCalls()
	if len(toolCalls) > 0 {
		s.out.ToolCalls = toolCalls
		s.out.StopReason = StopToolUse
	} else if s.out.StopReason == StopUnknown && s.sawContent {
		s.out.StopReason = StopEndTurn
	}

	return Chunk{
		Provider:              s.out.Provider,
		Model:                 s.out.Model,
		Done:                  true,
		StopReason:            s.out.StopReason,
		ToolCalls:             append([]ToolCall(nil), s.out.ToolCalls...),
		FirstTokenLatency:     s.out.FirstTokenLatency,
		InputTokens:           s.out.InputTokens,
		CachedInputTokens:     s.out.CachedInputTokens,
		CacheWriteInputTokens: s.out.CacheWriteInputTokens,
		OutputTokens:          s.out.OutputTokens,
	}
}

func (s *anthropicStreamState) finalToolCalls() []ToolCall {
	if len(s.toolBlocks) == 0 {
		return nil
	}

	indexes := make([]int, 0, len(s.toolBlocks))
	for index, block := range s.toolBlocks {
		if block != nil && block.seen {
			indexes = append(indexes, index)
		}
	}

	slices.Sort(indexes)

	out := make([]ToolCall, 0, len(indexes))
	for _, index := range indexes {
		block := s.toolBlocks[index]
		out = append(out, ToolCall{
			ID:    block.id,
			Name:  block.name,
			Input: cloneAnyMap(block.input),
		})
	}

	return out
}

func (s *anthropicStreamState) applyUsage(usage anthropicStreamUsage) {
	if usage.InputTokens != 0 || usage.CacheCreationInputTokens != 0 || usage.CacheReadInputTokens != 0 {
		s.out.InputTokens = usage.InputTokens + usage.CacheCreationInputTokens + usage.CacheReadInputTokens
		s.out.CacheWriteInputTokens = usage.CacheCreationInputTokens
		s.out.CachedInputTokens = usage.CacheReadInputTokens
	}

	if usage.OutputTokens != 0 {
		s.out.OutputTokens = usage.OutputTokens
	}
}

func (s *anthropicStreamState) recordFirstToken() {
	if s.firstTokenRecorded || s.startedAt.IsZero() || s.now == nil {
		return
	}

	s.firstTokenRecorded = true

	if d := s.now().Sub(s.startedAt); d > 0 {
		s.out.FirstTokenLatency = d
	}
}

func buildAnthropicRequestForProvider(providerName string, params CompleteParams) (anthropicRequest, error) {
	preparedParams, _, err := prepareCompleteParamsForProvider(providerName, params)
	if err != nil {
		return anthropicRequest{}, err
	}

	params = preparedParams

	if validateErr := validateCompleteParamsSupported(providerName, params); validateErr != nil {
		return anthropicRequest{}, validateErr
	}

	var system string

	msgs := make([]anthropicMessage, 0, len(params.Messages))
	for _, m := range params.Messages {
		if m.Role == RoleSystem {
			system = m.Content
			continue
		}

		msgs = append(msgs, buildAnthropicMessage(m))
	}

	maxTok := params.MaxTokens
	if maxTok <= 0 {
		maxTok = 4096
	}

	req := anthropicRequest{
		Model:       params.Model,
		MaxTokens:   maxTok,
		Messages:    msgs,
		Stop:        params.Stop,
		Temperature: params.Temperature,
		TopP:        params.TopP,
	}
	if system != "" {
		req.System = []anthropicContentBlock{{
			Type:         anthropicContentTypeText,
			Text:         system,
			CacheControl: anthropicEphemeralCacheControl(),
		}}
	}

	// Add tool definitions.
	for _, tool := range params.Tools {
		req.Tools = append(req.Tools, anthropicTool{
			Name:        tool.Name,
			Description: tool.Description,
			InputSchema: tool.Parameters,
		})
	}

	applyAnthropicPromptCacheBreakpoints(&req)

	budget, ok, err := anthropicThinkingBudget(params.ReasoningLevel, maxTok)
	if err != nil {
		return anthropicRequest{}, fmt.Errorf("%s: %w", providerName, err)
	}

	if ok {
		req.Thinking = &anthropicThinking{Type: "enabled", BudgetTokens: budget}
	}

	return req, nil
}

func anthropicEphemeralCacheControl() *anthropicCacheControl {
	return &anthropicCacheControl{Type: anthropicCacheControlTypeEphemeral}
}

func applyAnthropicPromptCacheBreakpoints(req *anthropicRequest) {
	if req == nil {
		return
	}

	if len(req.Tools) > 0 {
		req.Tools[len(req.Tools)-1].CacheControl = anthropicEphemeralCacheControl()
	}

	for i := len(req.Messages) - 1; i >= 0; i-- {
		content, ok := anthropicContentWithTailCacheControl(req.Messages[i].Content)
		if !ok {
			continue
		}

		req.Messages[i].Content = content

		return
	}
}

func anthropicContentWithTailCacheControl(content json.RawMessage) (json.RawMessage, bool) {
	trimmed := bytes.TrimSpace(content)
	if len(trimmed) == 0 {
		return nil, false
	}

	if trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil || text == "" {
			return nil, false
		}

		return marshalAnthropicCachedContentBlocks([]anthropicContentBlock{{
			Type:         anthropicContentTypeText,
			Text:         text,
			CacheControl: anthropicEphemeralCacheControl(),
		}})
	}

	if trimmed[0] != '[' {
		return nil, false
	}

	var blocks []anthropicContentBlock
	if err := json.Unmarshal(trimmed, &blocks); err != nil {
		return nil, false
	}

	for i := len(blocks) - 1; i >= 0; i-- {
		if !anthropicContentBlockCacheable(blocks[i]) {
			continue
		}

		blocks[i].CacheControl = anthropicEphemeralCacheControl()

		return marshalAnthropicCachedContentBlocks(blocks)
	}

	return nil, false
}

func marshalAnthropicCachedContentBlocks(blocks []anthropicContentBlock) (json.RawMessage, bool) {
	content, err := json.Marshal(blocks)
	if err != nil {
		return nil, false
	}

	return content, true
}

func anthropicContentBlockCacheable(block anthropicContentBlock) bool {
	switch block.Type {
	case anthropicContentTypeText:
		return block.Text != ""
	case anthropicContentTypeToolResult, anthropicContentTypeToolUse:
		return true
	default:
		return false
	}
}

// buildAnthropicMessage converts an llm.Message to the Anthropic wire format.
// Plain user/assistant text starts as a JSON string; prompt caching may later
// promote the tail message to a text content-block array so it can carry
// cache_control. Tool-use and tool-result messages use content-block arrays.
func buildAnthropicMessage(m Message) anthropicMessage {
	// Assistant message with tool calls -> content block array.
	if m.Role == RoleAssistant && len(m.ToolCalls) > 0 {
		var blocks []anthropicContentBlock
		if m.Content != "" {
			blocks = append(blocks, anthropicContentBlock{Type: anthropicContentTypeText, Text: m.Content})
		}

		for _, tc := range m.ToolCalls {
			blocks = append(blocks, anthropicContentBlock{
				Type:  anthropicContentTypeToolUse,
				ID:    tc.ID,
				Name:  tc.Name,
				Input: tc.Input,
			})
		}

		content, err := json.Marshal(blocks)
		if err != nil {
			// Fallback: serialize text-only as plain string.
			content, _ = json.Marshal(m.Content) //nolint:errcheck,errchkjson // string marshal cannot fail.
		}

		return anthropicMessage{Role: "assistant", Content: content}
	}

	// Tool result message -> user role with tool_result content blocks.
	if m.Role == RoleTool && m.ToolResult != nil {
		blocks := []anthropicContentBlock{{
			Type:      anthropicContentTypeToolResult,
			ToolUseID: m.ToolResult.ToolCallID,
			Content:   m.ToolResult.Content,
			IsError:   m.ToolResult.IsError,
		}}

		content, err := json.Marshal(blocks)
		if err != nil {
			content, _ = json.Marshal(m.ToolResult.Content) //nolint:errcheck,errchkjson // string marshal cannot fail.
		}

		return anthropicMessage{Role: "user", Content: content}
	}

	// Plain text message.
	content, _ := json.Marshal(m.Content) //nolint:errcheck,errchkjson // string marshal cannot fail.

	return anthropicMessage{Role: string(m.Role), Content: content}
}

func (a *AnthropicProvider) setAuthHeaders(httpReq *http.Request) {
	if a.bearer {
		httpReq.Header.Set("Authorization", "Bearer "+a.apiKey)
		// Claude Code and ForgeCode OAuth tokens require Anthropic beta routing
		// headers in addition to Authorization bearer auth. Keeping the header
		// on all Anthropic bearer tokens is safer than silently sending OAuth
		// tokens to the API-key-only path.
		httpReq.Header.Set("anthropic-beta", anthropicOAuthBetas)
	} else {
		httpReq.Header.Set("X-Api-Key", a.apiKey)
	}
}

// ModelContextWindow returns the context window size for an Anthropic model.
func (a *AnthropicProvider) ModelContextWindow(model string) int {
	if limit := catalogContextWindow(providerAnthropic, model); limit > 0 {
		return limit
	}

	return anthropicContextWindow(model)
}

func anthropicContextWindow(model string) int {
	switch model {
	case "claude-opus-4-20250514", "claude-sonnet-4-20250514",
		"claude-haiku-4-5-20251001":
		return 200_000
	default:
		// Newer models default to 200k; fall back for unknowns.
		if strings.HasPrefix(model, "claude") {
			return 200_000
		}

		return 0
	}
}
