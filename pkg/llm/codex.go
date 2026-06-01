package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/tommoulard/atteler/pkg/events"
)

// codexChatGPTAPIBase is the base URL the codex CLI uses when authenticated
// via ChatGPT login. The token is scoped for this proxied endpoint, not for
// api.openai.com directly.
const codexChatGPTAPIBase = "https://chatgpt.com/backend-api/codex"

// codexOriginatorHeader identifies us to the codex backend. We borrow codex's
// own originator value; impersonation here is at the user's invitation, since
// they want their codex login to authorize atteler.
const codexOriginatorHeader = "codex_cli_rs"

const (
	codexAdapterVersion    = "codex-chatgpt-responses-v1"
	codexAdapterSource     = "Codex CLI auth.json auth_mode=chatgpt; source credentials are owned by the codex CLI"
	codexAdapterCLIVersion = "Codex CLI ChatGPT auth schema and originator header as reviewed on 2026-05-22; " +
		"no public upstream semver contract"
	codexAdapterProtocol = "ChatGPT Codex backend POST /backend-api/codex/responses with Responses-style SSE, " +
		"originator=codex_cli_rs, OpenAI-Beta=responses=experimental"
	codexAdapterReviewedAt  = "2026-05-22"
	codexAdapterReviewAfter = "2026-08-22"
)

// CodexProvider calls the OpenAI Responses API directly using credentials
// stored by the codex CLI in ~/.codex/auth.json (auth_mode=chatgpt). It
// reuses the user's ChatGPT subscription so atteler doesn't burn separate
// Platform API quota.
type CodexProvider struct {
	client  *http.Client
	auth    *codexChatGPTAuth
	baseURL string
	models  []string
}

// NewCodexProvider is kept for source compatibility only.
//
// Deprecated: use NewCodexProviderContext so auth-file reads inherit caller
// cancellation checks.
func NewCodexProvider() (*CodexProvider, error) {
	return nil, ErrContextRequired
}

// NewCodexProviderContext creates a provider backed by ~/.codex/auth.json.
// It returns an error when no chatgpt-mode credentials are present.
func NewCodexProviderContext(ctx context.Context) (*CodexProvider, error) {
	return NewCodexProviderWithConfigContext(ctx, ProviderConfig{})
}

// NewCodexProviderWithConfig is kept for source compatibility only.
//
// Deprecated: use NewCodexProviderWithConfigContext so auth-file reads inherit
// caller cancellation checks.
func NewCodexProviderWithConfig(_ ProviderConfig) (*CodexProvider, error) {
	return nil, ErrContextRequired
}

// NewCodexProviderWithConfigContext creates a provider backed by
// ~/.codex/auth.json and applies provider-specific configuration.
func NewCodexProviderWithConfigContext(ctx context.Context, cfg ProviderConfig) (*CodexProvider, error) {
	if err := requireCredentialContext(ctx); err != nil {
		return nil, err
	}

	if privateAdapterDisabled(providerCodex, cfg) {
		return nil, errors.New("codex private adapter disabled")
	}

	auth, err := loadCodexChatGPTAuthContext(ctx, codexConfigDir())
	if err != nil {
		return nil, fmt.Errorf("no Codex credentials found: %w (run `codex login`)", err)
	}

	return &CodexProvider{
		client:  providerHTTPClient(cfg),
		auth:    auth,
		baseURL: configuredBaseURL("CODEX_BASE_URL", cfg.BaseURL, codexChatGPTAPIBase),
		models:  codexModels(),
	}, nil
}

// Name returns the provider name.
func (c *CodexProvider) Name() string { return providerCodex }

// Models returns Codex model IDs.
func (c *CodexProvider) Models() []string {
	if len(c.models) == 0 {
		return defaultCodexModels()
	}

	return append([]string(nil), c.models...)
}

// FetchModels returns the local Codex model catalog. The chatgpt backend does
// not expose a /models endpoint for this auth mode.
func (c *CodexProvider) FetchModels(ctx context.Context) ([]string, error) {
	if err := requireCredentialContext(ctx); err != nil {
		return nil, err
	}

	return c.Models(), nil
}

// AdapterDiagnostics reports the private Codex adapter contract and readiness.
func (c *CodexProvider) AdapterDiagnostics() AdapterDiagnostics {
	access, accountID := "", ""
	if c.auth != nil {
		access, accountID = c.auth.snapshot()
	}

	checks := []ReadinessCheck{
		{
			Name:   "local_credentials",
			Status: readinessStatus(access != ""),
			Detail: readinessDetail(access != "", "chatgpt access_token loaded from Codex auth.json", "missing chatgpt access_token; run `codex login`"),
		},
		{
			Name:   "token_refresh",
			Status: readinessStatus(c.auth != nil && c.auth.hasRefreshToken()),
			Detail: readinessDetail(c.auth != nil && c.auth.hasRefreshToken(), "refresh_token available for one retry after HTTP 401", "missing refresh_token; adapter cannot recover from expired access tokens"),
		},
		{
			Name:   "network_reachability",
			Status: ReadinessSkipped,
			Detail: "not probed during doctor; private ChatGPT Codex backend is verified only by a completion request",
		},
		{
			Name:   "model_availability",
			Status: ReadinessWarning,
			Detail: "static catalog only; this auth mode has no supported /models endpoint",
		},
	}

	if accountID == "" {
		checks = append(checks, ReadinessCheck{
			Name:   "account_scope",
			Status: ReadinessWarning,
			Detail: "auth.json has no ChatGPT account id; backend may choose the default account",
		})
	}

	return AdapterDiagnostics{
		Contract: codexAdapterContract(),
		Checks:   checks,
		Warnings: []string{
			"uses a private ChatGPT/Codex backend, borrowed Codex CLI credentials, and a static non-network-verified model catalog",
		},
		Models: c.Models(),
	}
}

// ProviderModelsVerified reports whether FetchModels is an authoritative live
// provider availability check. Codex currently exposes only a local/static model
// catalog for this auth mode, so absence from the list should remain unverified.
func (c *CodexProvider) ProviderModelsVerified() bool {
	return false
}

// HealthCheck verifies that auth.json parses and contains a chatgpt-mode token.
// It does not contact the network.
func (c *CodexProvider) HealthCheck(ctx context.Context) error {
	if err := requireCredentialContext(ctx); err != nil {
		return err
	}

	emitActivity(ctx, events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command":  "codex.auth.check",
			"provider": providerCodex,
		},
	})

	if c.auth == nil {
		return errors.New("no Codex chatgpt access_token in auth.json: run `codex login`")
	}

	access, _ := c.auth.snapshot()
	if access == "" {
		return errors.New("no Codex chatgpt access_token in auth.json: run `codex login`")
	}

	return nil
}

// ModelContextWindow returns the context window size for a Codex model.
func (c *CodexProvider) ModelContextWindow(model string) int {
	if limit := catalogContextWindow(providerCodex, model); limit > 0 {
		return limit
	}

	metadata, ok := c.ModelMetadata(model)
	if !ok {
		return 0
	}

	return metadata.ContextWindow
}

// ModelCatalog returns static Codex model metadata with provenance.
func (c *CodexProvider) ModelCatalog() []ModelMetadata {
	out := make([]ModelMetadata, 0, len(c.Models()))
	for _, model := range c.Models() {
		metadata, ok := c.ModelMetadata(model)
		if !ok {
			continue
		}

		out = append(out, metadata)
	}

	return out
}

// ModelMetadata returns provenance for a Codex static model entry.
func (c *CodexProvider) ModelMetadata(model string) (ModelMetadata, bool) {
	for _, entry := range codexDefaultModelCatalog() {
		if entry.ID == model {
			return entry, true
		}
	}

	if slices.Contains(c.Models(), model) || model == codexConfiguredModel() {
		return ModelMetadata{
			ID:          model,
			Provenance:  "user Codex config.toml model override; no Codex backend model-metadata endpoint exists for this auth mode",
			ReviewedAt:  codexAdapterReviewedAt,
			ReviewAfter: codexAdapterReviewAfter,
			Notes:       "context window unknown; Atteler intentionally returns 0 rather than guessing for configured Codex models",
		}, true
	}

	return ModelMetadata{}, false
}

// codexResponsesRequest mirrors the subset of the OpenAI Responses API that
// the codex backend accepts.
type codexResponsesRequest struct {
	Reasoning    *codexRequestReasoning `json:"reasoning,omitempty"`
	Tools        []codexTool            `json:"tools,omitempty"`
	Model        string                 `json:"model"`
	ServiceTier  string                 `json:"service_tier,omitempty"`
	Instructions string                 `json:"instructions,omitempty"`
	Input        []codexInputItem       `json:"input"`
	Stream       bool                   `json:"stream"`
	Store        bool                   `json:"store"`
}

type codexRequestReasoning struct {
	Effort string `json:"effort,omitempty"`
}

// codexTool is the Responses API tool definition format.
type codexTool struct {
	Parameters  map[string]any `json:"parameters,omitempty"`
	Type        string         `json:"type"`
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
}

type codexInputItem struct {
	Type      string              `json:"type"`
	Role      string              `json:"role,omitempty"`
	CallID    string              `json:"call_id,omitempty"`
	Name      string              `json:"name,omitempty"`
	Arguments string              `json:"arguments,omitempty"`
	Output    string              `json:"output,omitempty"`
	Content   []codexInputContent `json:"content,omitempty"`
}

type codexInputContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Complete runs an OpenAI Responses API request against the chatgpt codex
// backend, refreshing the access token once on a 401.
func (c *CodexProvider) Complete(ctx context.Context, params CompleteParams) (*Response, error) {
	if err := requireCredentialContext(ctx); err != nil {
		return nil, err
	}

	model, body, err := c.buildResponsesBody(ctx, params)
	if err != nil {
		return nil, err
	}

	resp, err := c.doResponsesRequest(ctx, body)
	if err != nil {
		return nil, err
	}

	resp.Model = firstNonEmptyString(resp.Model, model)

	return resp, nil
}

// CompleteStream runs a streaming OpenAI Responses API request against the
// chatgpt codex backend, refreshing the access token once on a setup-time 401.
func (c *CodexProvider) CompleteStream(ctx context.Context, params CompleteParams) (<-chan Chunk, error) {
	if err := requireCredentialContext(ctx); err != nil {
		return nil, err
	}

	model, body, err := c.buildResponsesBody(ctx, params)
	if err != nil {
		return nil, err
	}

	return c.doResponsesStream(ctx, body, model)
}

func (c *CodexProvider) buildResponsesBody(ctx context.Context, params CompleteParams) (model string, body []byte, err error) {
	model = params.Model
	if model == "" {
		models := c.Models()
		if len(models) == 0 {
			return "", nil, errors.New("codex model not configured")
		}

		model = models[0]
	}

	params.Model = model

	preparedParams, adjustments, err := prepareCompleteParamsForProvider(providerCodex, params)
	if err != nil {
		return "", nil, err
	}

	params = preparedParams

	req, err := buildCodexResponsesRequest(params)
	if err != nil {
		return "", nil, err
	}

	body, err = json.Marshal(req)
	if err != nil {
		return "", nil, fmt.Errorf("codex marshal: %w", err)
	}

	metadata := map[string]string{
		"command":  "codex.responses",
		"provider": providerCodex,
	}

	if mode := normalizeModelMode(params.ModelMode); mode != "" {
		metadata["model_mode"] = mode
		if tier := openAIServiceTierForModelMode(mode); tier != "" {
			metadata["service_tier"] = tier
		}
	}

	if len(adjustments) > 0 {
		metadata["option_adjustments"] = formatCompleteParamAdjustments(adjustments)
	}

	emitActivity(ctx, events.Event{
		Type:     events.CommandExecute,
		Model:    model,
		Metadata: metadata,
	})

	return model, body, nil
}

func buildCodexResponsesRequest(params CompleteParams) (codexResponsesRequest, error) {
	preparedParams, _, err := prepareCompleteParamsForProvider(providerCodex, params)
	if err != nil {
		return codexResponsesRequest{}, err
	}

	params = preparedParams

	if validateErr := validateCompleteParamsSupported(providerCodex, params); validateErr != nil {
		return codexResponsesRequest{}, validateErr
	}

	if params.Model == "" {
		return codexResponsesRequest{}, errors.New("codex model not configured")
	}

	req := codexResponsesRequest{
		Model:        params.Model,
		Instructions: codexInstructions(params.Messages),
		Input:        codexBuildInput(params.Messages),
		Tools:        codexBuildTools(params.Tools),
		Stream:       true,
	}

	if effort := openAIReasoningEffort(params.ReasoningLevel); effort != "" {
		req.Reasoning = &codexRequestReasoning{Effort: effort}
	}

	if tier := openAIServiceTierForModelMode(params.ModelMode); tier != "" {
		req.ServiceTier = tier
	}

	return req, nil
}

func (c *CodexProvider) doResponsesRequest(ctx context.Context, body []byte) (*Response, error) {
	access, accountID := c.auth.snapshot()

	resp, err := c.sendResponses(ctx, body, access, accountID)
	if err == nil {
		return resp, nil
	}

	var unauthorized *codexUnauthorizedError
	if !errors.As(err, &unauthorized) {
		return nil, err
	}

	if refreshErr := c.auth.refresh(ctx, access); refreshErr != nil {
		return nil, fmt.Errorf("codex refresh after 401: %w", refreshErr)
	}

	access, accountID = c.auth.snapshot()

	resp, err = c.sendResponses(ctx, body, access, accountID)
	if err != nil {
		return nil, err
	}

	return resp, nil
}

func (c *CodexProvider) doResponsesStream(ctx context.Context, body []byte, model string) (<-chan Chunk, error) {
	access, accountID := c.auth.snapshot()

	startedAt := time.Now()

	bodyStream, err := c.sendResponsesStream(ctx, body, access, accountID)
	if err == nil {
		return streamCodexResponseBody(ctx, bodyStream, model, startedAt, time.Now), nil
	}

	var unauthorized *codexUnauthorizedError
	if !errors.As(err, &unauthorized) {
		return nil, err
	}

	if refreshErr := c.auth.refresh(ctx, access); refreshErr != nil {
		return nil, fmt.Errorf("codex refresh after 401: %w", refreshErr)
	}

	access, accountID = c.auth.snapshot()

	startedAt = time.Now()

	bodyStream, err = c.sendResponsesStream(ctx, body, access, accountID)
	if err != nil {
		return nil, err
	}

	return streamCodexResponseBody(ctx, bodyStream, model, startedAt, time.Now), nil
}

type codexUnauthorizedError struct {
	err *ProviderError
}

func (e *codexUnauthorizedError) Error() string {
	return e.err.Error()
}

func (e *codexUnauthorizedError) Unwrap() error {
	return e.err
}

func (c *CodexProvider) sendResponses(ctx context.Context, body []byte, access, accountID string) (*Response, error) {
	startedAt := time.Now()

	bodyStream, err := c.sendResponsesStream(ctx, body, access, accountID)
	if err != nil {
		return nil, err
	}
	defer bodyStream.Close()

	return parseCodexSSEWithClock(ctx, bodyStream, startedAt, time.Now)
}

func (c *CodexProvider) sendResponsesStream(ctx context.Context, body []byte, access, accountID string) (io.ReadCloser, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/responses", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("codex new request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("Authorization", "Bearer "+access)

	if accountID != "" {
		httpReq.Header.Set("ChatGPT-Account-ID", accountID)
	}

	httpReq.Header.Set("OpenAI-Beta", "responses=experimental")
	httpReq.Header.Set("originator", codexOriginatorHeader)

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("codex request: %w", err)
	}

	if resp.StatusCode == http.StatusUnauthorized {
		defer resp.Body.Close()

		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxOAuthErrorBodyBytes)) //nolint:errcheck // best-effort body capture for the error message

		return nil, &codexUnauthorizedError{err: newProviderHTTPError(providerCodex, resp, raw)}
	}

	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()

		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxOAuthErrorBodyBytes)) //nolint:errcheck // best-effort body capture for the error message

		return nil, newProviderHTTPError(providerCodex, resp, raw)
	}

	return resp.Body, nil
}

// codexStreamState accumulates partial state from an SSE response stream.
type codexStreamState struct {
	now                func() time.Time
	startedAt          time.Time
	toolCalls          []ToolCall
	finalText          string
	deltaBuf           strings.Builder
	header             http.Header
	out                Response
	finished           bool
	firstTokenRecorded bool
}

type codexStreamAction struct {
	chunk    Chunk
	emit     bool
	terminal bool
}

// parseCodexSSE consumes an OpenAI Responses-API SSE stream and returns the
// final assistant message and usage. It prefers the explicit message item when
// available, falling back to accumulated text deltas otherwise.
func parseCodexSSE(ctx context.Context, r io.Reader) (*Response, error) {
	return parseCodexSSEWithHeader(ctx, r, nil)
}

func parseCodexSSEWithHeader(ctx context.Context, r io.Reader, header http.Header) (*Response, error) {
	return parseCodexSSEWithClockAndHeader(ctx, r, time.Time{}, nil, header)
}

func parseCodexSSEWithClock(ctx context.Context, r io.Reader, startedAt time.Time, now func() time.Time) (*Response, error) {
	return parseCodexSSEWithClockAndHeader(ctx, r, startedAt, now, nil)
}

func parseCodexSSEWithClockAndHeader(ctx context.Context, r io.Reader, startedAt time.Time, now func() time.Time, header http.Header) (*Response, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	state := &codexStreamState{
		startedAt: startedAt,
		now:       now,
		header:    header,
	}

	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("codex stream canceled: %w", err)
		}

		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}

		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}

		if _, err := state.handleEventPayload(payload); err != nil {
			return nil, err
		}
	}

	if err := scanner.Err(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("codex stream canceled: %w", ctxErr)
		}

		return nil, fmt.Errorf("codex stream read: %w", err)
	}

	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("codex stream canceled: %w", err)
	}

	return state.finalize()
}

func streamCodexResponseBody(ctx context.Context, body io.ReadCloser, model string, startedAt time.Time, now func() time.Time) <-chan Chunk {
	ch := make(chan Chunk, DefaultStreamBuffer)

	go func() {
		defer close(ch)
		defer body.Close()

		streamCodexSSEWithClock(ctx, body, ch, model, startedAt, now)
	}()

	return ch
}

func streamCodexSSE(ctx context.Context, r io.Reader, ch chan<- Chunk, model string) {
	streamCodexSSEWithClock(ctx, r, ch, model, time.Time{}, nil)
}

func streamCodexSSEWithClock(ctx context.Context, r io.Reader, ch chan<- Chunk, model string, startedAt time.Time, now func() time.Time) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	state := &codexStreamState{
		startedAt: startedAt,
		now:       now,
		out:       Response{Model: model},
	}

	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			sendCodexTerminalError(ctx, ch, fmt.Errorf("codex stream canceled: %w", err))

			return
		}

		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}

		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}

		action, err := state.handleEventPayload(payload)
		if err != nil {
			sendCodexTerminalError(ctx, ch, err)

			return
		}

		if action.emit && !sendCodexChunk(ctx, ch, action.chunk) {
			return
		}

		if action.terminal {
			return
		}
	}

	if err := scanner.Err(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			sendCodexTerminalError(ctx, ch, fmt.Errorf("codex stream canceled: %w", ctxErr))

			return
		}

		sendCodexTerminalError(ctx, ch, fmt.Errorf("codex stream read: %w", err))

		return
	}

	if err := ctx.Err(); err != nil {
		sendCodexTerminalError(ctx, ch, fmt.Errorf("codex stream canceled: %w", err))

		return
	}

	if !state.finished {
		sendCodexTerminalError(ctx, ch, fmt.Errorf("codex stream incomplete: %w", ErrStreamIncomplete))
	}
}

func sendCodexChunk(ctx context.Context, ch chan<- Chunk, chunk Chunk) bool {
	select {
	case ch <- chunk:
		return true
	case <-ctx.Done():
		return false
	}
}

func sendCodexTerminalError(ctx context.Context, ch chan<- Chunk, err error) {
	if err == nil {
		return
	}

	chunk := Chunk{Err: err}

	select {
	case ch <- chunk:
		return
	default:
	}

	select {
	case ch <- chunk:
	case <-ctx.Done():
	}
}

func (s *codexStreamState) handleEventPayload(payload string) (codexStreamAction, error) {
	var event codexStreamEvent
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		// Tolerate malformed events; some backends interleave keep-alive
		// pings or partial frames with JSON data lines.
		return codexStreamAction{}, nil //nolint:nilerr // intentional: skip unparseable lines
	}

	switch event.Type {
	case "response.output_text.delta":
		if event.Delta != "" {
			s.recordFirstToken()
		}

		s.deltaBuf.WriteString(event.Delta)

		if event.Delta != "" {
			return codexStreamAction{
				chunk: Chunk{
					Content:           event.Delta,
					FirstTokenLatency: s.out.FirstTokenLatency,
				},
				emit: true,
			}, nil
		}
	case "response.output_item.done":
		if tc, ok := codexExtractFunctionCall(event.Item); ok {
			s.toolCalls = append(s.toolCalls, tc)
		} else if text := codexExtractMessageText(event.Item); text != "" {
			s.recordFirstToken()
			s.finalText = text
		}
	case "response.completed":
		s.finished = true
		s.applyCompleted(event.Response)

		chunk, err := s.completedChunk()
		if err != nil {
			return codexStreamAction{}, err
		}

		return codexStreamAction{chunk: chunk, emit: true, terminal: true}, nil
	case "response.failed", "response.incomplete", "error":
		return codexStreamAction{}, event.providerError(s.header)
	}

	return codexStreamAction{}, nil
}

func (s *codexStreamState) recordFirstToken() {
	if s.firstTokenRecorded || s.startedAt.IsZero() || s.now == nil {
		return
	}

	s.firstTokenRecorded = true

	if d := s.now().Sub(s.startedAt); d > 0 {
		s.out.FirstTokenLatency = d
	}
}

func (s *codexStreamState) applyCompleted(resp *codexEventPayload) {
	if resp == nil {
		return
	}

	if resp.Model != "" {
		s.out.Model = resp.Model
	}

	if stopReason := codexStopReason(resp); stopReason != StopUnknown {
		s.out.StopReason = stopReason
	}

	if u := resp.Usage; u != nil {
		s.out.InputTokens = u.InputTokens
		s.out.OutputTokens = u.OutputTokens

		if u.InputTokensDetails != nil {
			s.out.CachedInputTokens = u.InputTokensDetails.CachedTokens
		}
	}
}

func (s *codexStreamState) finalize() (*Response, error) {
	if s.finalText != "" {
		s.out.Content = s.finalText
	} else {
		s.out.Content = s.deltaBuf.String()
	}

	if !s.finished {
		return nil, fmt.Errorf("codex stream incomplete: %w", ErrStreamIncomplete)
	}

	// If the model returned tool calls, set them on the response and mark
	// the stop reason so the AgentLoop knows to execute them.
	if len(s.toolCalls) > 0 {
		s.out.ToolCalls = s.toolCalls
		s.out.StopReason = StopToolUse

		return &s.out, nil
	}

	if s.out.Content == "" {
		return nil, errors.New("codex stream completed with empty assistant message")
	}

	if s.out.StopReason == StopUnknown {
		s.out.StopReason = StopEndTurn
	}

	return &s.out, nil
}

func (s *codexStreamState) completedChunk() (Chunk, error) {
	streamedContent := s.deltaBuf.String()
	completeContent := streamedContent
	chunkContent := ""

	if completeContent == "" {
		completeContent = s.finalText
		chunkContent = s.finalText
	}

	chunk := Chunk{
		Content:               chunkContent,
		Model:                 s.out.Model,
		Done:                  true,
		FirstTokenLatency:     s.out.FirstTokenLatency,
		InputTokens:           s.out.InputTokens,
		CachedInputTokens:     s.out.CachedInputTokens,
		CacheWriteInputTokens: s.out.CacheWriteInputTokens,
		OutputTokens:          s.out.OutputTokens,
	}

	if len(s.toolCalls) > 0 {
		chunk.StopReason = StopToolUse

		chunk.ToolCalls = append([]ToolCall(nil), s.toolCalls...)

		return chunk, nil
	}

	if completeContent == "" {
		return Chunk{}, errors.New("codex stream completed with empty assistant message")
	}

	chunk.StopReason = s.out.StopReason
	if chunk.StopReason == StopUnknown {
		chunk.StopReason = StopEndTurn
	}

	return chunk, nil
}

type codexStreamEvent struct {
	Item     *codexEventItem    `json:"item,omitempty"`
	Response *codexEventPayload `json:"response,omitempty"`
	Error    *codexEventError   `json:"error,omitempty"`
	Type     string             `json:"type"`
	Code     string             `json:"code,omitempty"`
	Delta    string             `json:"delta,omitempty"`
	Message  string             `json:"message,omitempty"`
}

type codexEventItem struct {
	Arguments string                  `json:"arguments"`
	Type      string                  `json:"type"`
	Role      string                  `json:"role"`
	ID        string                  `json:"id"`
	CallID    string                  `json:"call_id"`
	Name      string                  `json:"name"`
	Content   []codexEventItemContent `json:"content"`
}

type codexEventItemContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type codexEventPayload struct {
	IncompleteDetails *codexIncompleteDetails `json:"incomplete_details,omitempty"`
	Error             *codexEventError        `json:"error,omitempty"`
	Usage             *codexEventUsage        `json:"usage,omitempty"`
	Model             string                  `json:"model,omitempty"`
	Status            string                  `json:"status,omitempty"`
}

type codexIncompleteDetails struct {
	Reason string `json:"reason,omitempty"`
}

type codexEventError struct {
	Message string `json:"message,omitempty"`
	Type    string `json:"type,omitempty"`
	Code    string `json:"code,omitempty"`
}

const codexStreamError = "codex stream error"

func (e *codexEventError) UnmarshalJSON(data []byte) error {
	var message string
	if err := json.Unmarshal(data, &message); err == nil {
		e.Message = message

		return nil
	}

	type object codexEventError

	var value object
	if err := json.Unmarshal(data, &value); err != nil {
		return fmt.Errorf("decode codex event error: %w", err)
	}

	*e = codexEventError(value)

	return nil
}

func (e codexStreamEvent) providerError(header http.Header) *ProviderError {
	eventError := e.Error
	if eventError == nil && e.Response != nil {
		eventError = e.Response.Error
	}

	if eventError != nil {
		errorType := firstNonEmptyString(eventError.Code, eventError.Type, e.Code)
		if errorType == "" {
			errorType = codexStreamError
		}

		message := firstNonEmptyString(eventError.Message, codexStreamError)
		if errorType == codexStreamError && e.Type != "" && message != e.Type {
			message = e.Type + ": " + message
		}

		return newProviderPayloadError(
			providerCodex,
			http.StatusOK,
			header,
			errorType,
			message,
		)
	}

	errorType := firstNonEmptyString(e.Code, e.Type)
	if errorType == "" {
		errorType = codexStreamError
	}

	message := firstNonEmptyString(e.Message, codexStreamError)
	if errorType == codexStreamError && e.Type != "" && message != e.Type {
		message = e.Type + ": " + message
	}

	return newProviderPayloadError(
		providerCodex,
		http.StatusOK,
		header,
		errorType,
		message,
	)
}

type codexEventUsage struct {
	InputTokensDetails *codexEventTokensDetails `json:"input_tokens_details,omitempty"`
	InputTokens        int                      `json:"input_tokens"`
	OutputTokens       int                      `json:"output_tokens"`
	TotalTokens        int                      `json:"total_tokens"`
}

type codexEventTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

func codexStopReason(resp *codexEventPayload) StopReason {
	if resp == nil {
		return StopUnknown
	}

	if resp.IncompleteDetails != nil {
		switch resp.IncompleteDetails.Reason {
		case "max_output_tokens", "max_tokens":
			return StopMaxToks
		}
	}

	switch resp.Status {
	case "completed":
		return StopEndTurn
	case "incomplete":
		return StopUnknown
	default:
		return StopUnknown
	}
}

func codexExtractMessageText(item *codexEventItem) string {
	if item == nil || item.Type != "message" {
		return ""
	}

	var b strings.Builder

	for _, c := range item.Content {
		if c.Type == "output_text" || c.Type == "text" {
			b.WriteString(c.Text)
		}
	}

	return b.String()
}

// codexExtractFunctionCall detects a function_call output item and converts it
// to a ToolCall. Returns false when the item is not a function_call.
func codexExtractFunctionCall(item *codexEventItem) (ToolCall, bool) {
	if item == nil || item.Type != "function_call" {
		return ToolCall{}, false
	}

	var input map[string]any
	if err := json.Unmarshal([]byte(item.Arguments), &input); err != nil {
		input = map[string]any{"raw": item.Arguments}
	}

	// The Responses API uses call_id, but some backends populate id instead.
	callID := item.CallID
	if callID == "" {
		callID = item.ID
	}

	return ToolCall{
		ID:    callID,
		Name:  item.Name,
		Input: input,
	}, true
}

// codexDefaultInstructions is sent when the caller provided no system prompt.
// The chatgpt codex backend rejects requests with empty `instructions`.
const codexDefaultInstructions = "You are a helpful assistant."

func codexInstructions(messages []Message) string {
	var system []string

	for _, msg := range messages {
		if msg.Role == RoleSystem && msg.Content != "" {
			system = append(system, msg.Content)
		}
	}

	if len(system) == 0 {
		return codexDefaultInstructions
	}

	return strings.Join(system, "\n\n")
}

func codexBuildInput(messages []Message) []codexInputItem {
	out := make([]codexInputItem, 0, len(messages))

	for _, msg := range messages {
		switch {
		case msg.Role == RoleSystem:
			continue
		case msg.Role == RoleAssistant && len(msg.ToolCalls) > 0:
			out = appendCodexToolCallItems(out, msg)
		case msg.Role == RoleTool && msg.ToolResult != nil:
			out = append(out, codexInputItem{
				Type:   "function_call_output",
				CallID: msg.ToolResult.ToolCallID,
				Output: msg.ToolResult.Content,
			})
		default:
			out = appendCodexMessageItem(out, msg)
		}
	}

	return out
}

// appendCodexToolCallItems appends the assistant text (if any) and each tool
// call as separate Responses API items.
func appendCodexToolCallItems(out []codexInputItem, msg Message) []codexInputItem {
	if msg.Content != "" {
		out = append(out, codexInputItem{
			Type: "message",
			Role: string(RoleAssistant),
			Content: []codexInputContent{{
				Type: "output_text",
				Text: msg.Content,
			}},
		})
	}

	for _, tc := range msg.ToolCalls {
		args, err := json.Marshal(tc.Input)
		if err != nil {
			args = []byte("{}")
		}

		out = append(out, codexInputItem{
			Type:      "function_call",
			CallID:    tc.ID,
			Name:      tc.Name,
			Arguments: string(args),
		})
	}

	return out
}

// appendCodexMessageItem appends a plain user or assistant message.
func appendCodexMessageItem(out []codexInputItem, msg Message) []codexInputItem {
	contentType := "input_text"
	if msg.Role == RoleAssistant {
		contentType = "output_text"
	}

	return append(out, codexInputItem{
		Type: "message",
		Role: string(msg.Role),
		Content: []codexInputContent{{
			Type: contentType,
			Text: msg.Content,
		}},
	})
}

// codexBuildTools converts internal tool definitions to the Responses API
// format (type=function with a name, description, and JSON Schema parameters).
func codexBuildTools(tools []ToolDefinition) []codexTool {
	if len(tools) == 0 {
		return nil
	}

	out := make([]codexTool, 0, len(tools))
	for _, td := range tools {
		out = append(out, codexTool{
			Type:        "function",
			Name:        td.Name,
			Description: td.Description,
			Parameters:  td.Parameters,
		})
	}

	return out
}

func codexModels() []string {
	models := defaultCodexModels()
	if model := codexConfiguredModel(); model != "" {
		models = append([]string{model}, models...)
	}

	return dedupeStrings(models)
}

func defaultCodexModels() []string {
	return modelIDsFromMetadata(codexDefaultModelCatalog())
}

func codexDefaultModelCatalog() []ModelMetadata {
	return cloneModelMetadata([]ModelMetadata{
		{
			ID:            "gpt-5.5",
			ContextWindow: 400_000,
			Provenance:    "static Codex adapter catalog reviewed against the local provider contract; not network verified",
			ReviewedAt:    codexAdapterReviewedAt,
			ReviewAfter:   codexAdapterReviewAfter,
			Notes:         "private Codex backend has no supported /models endpoint for ChatGPT auth",
		},
		{
			ID:            "gpt-5.4",
			ContextWindow: 400_000,
			Provenance:    "static Codex adapter catalog reviewed against the local provider contract; not network verified",
			ReviewedAt:    codexAdapterReviewedAt,
			ReviewAfter:   codexAdapterReviewAfter,
			Notes:         "private Codex backend has no supported /models endpoint for ChatGPT auth",
		},
		{
			ID:            "gpt-5.4-mini",
			ContextWindow: 200_000,
			Provenance:    "static Codex adapter catalog reviewed against the local provider contract; not network verified",
			ReviewedAt:    codexAdapterReviewedAt,
			ReviewAfter:   codexAdapterReviewAfter,
			Notes:         "private Codex backend has no supported /models endpoint for ChatGPT auth",
		},
		{
			ID:            "gpt-5.3-codex",
			ContextWindow: 200_000,
			Provenance:    "static Codex adapter catalog reviewed against the local provider contract; not network verified",
			ReviewedAt:    codexAdapterReviewedAt,
			ReviewAfter:   codexAdapterReviewAfter,
			Notes:         "private Codex backend has no supported /models endpoint for ChatGPT auth",
		},
		{
			ID:            "gpt-5.3-codex-spark",
			ContextWindow: 200_000,
			Provenance:    "static Codex adapter catalog reviewed against the local provider contract; not network verified",
			ReviewedAt:    codexAdapterReviewedAt,
			ReviewAfter:   codexAdapterReviewAfter,
			Notes:         "private Codex backend has no supported /models endpoint for ChatGPT auth",
		},
	})
}

//nolint:gosec // Documents credential source paths and JSON field names, not secret values.
func codexAdapterContract() AdapterContract {
	return AdapterContract{
		Provider:         providerCodex,
		AdapterVersion:   codexAdapterVersion,
		SourceCLI:        codexAdapterSource,
		SourceCLIVersion: codexAdapterCLIVersion,
		Protocol:         codexAdapterProtocol,
		Credential:       "CODEX_HOME/auth.json or ~/.codex/auth.json tokens.access_token/refresh_token/account_id",
		KillSwitches: []string{
			"providers.codex.disable_private_adapter",
			"ATTELER_DISABLE_CODEX_ADAPTER",
			"ATTELER_DISABLE_PRIVATE_ADAPTERS",
			"ATTELER_DISABLE_BORROWED_CREDENTIAL_ADAPTERS",
		},
		ReviewedAt:  codexAdapterReviewedAt,
		ReviewAfter: codexAdapterReviewAfter,
	}
}

func codexConfiguredModel() string {
	data, err := os.ReadFile(filepath.Join(codexConfigDir(), "config.toml"))
	if err != nil {
		return ""
	}

	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(stripInlineComment(line))
		if !strings.HasPrefix(line, "model") {
			continue
		}

		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != "model" {
			continue
		}

		return strings.Trim(strings.TrimSpace(value), `"'`)
	}

	return ""
}

func codexConfigDir() string {
	if dir := os.Getenv("CODEX_HOME"); strings.TrimSpace(dir) != "" {
		return dir
	}

	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}

	return filepath.Join(home, ".codex")
}

func stripInlineComment(line string) string {
	before, _, found := strings.Cut(line, "#")
	if found {
		return before
	}

	return line
}

func dedupeStrings(values []string) []string {
	out := make([]string, 0, len(values))

	seen := make(map[string]bool, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}

		seen[value] = true
		out = append(out, value)
	}

	return out
}
