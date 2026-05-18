package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	defaultAnthropicBase    = "https://api.anthropic.com"
	defaultAnthropicVersion = "2023-06-01"
	anthropicOAuthBetas     = "claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14,structured-outputs-2025-11-13"
)

// AnthropicProvider calls the Anthropic Messages API.
type AnthropicProvider struct {
	client  *http.Client
	apiKey  string
	baseURL string
	bearer  bool
}

// NewAnthropicProvider creates a provider using ResolveAnthropicKey.
// The base URL can be overridden with ANTHROPIC_BASE_URL.
func NewAnthropicProvider() (*AnthropicProvider, error) {
	return NewAnthropicProviderContext(defaultCredentialContext())
}

// NewAnthropicProviderContext creates a provider using ResolveAnthropicKeyContext.
// The base URL can be overridden with ANTHROPIC_BASE_URL.
func NewAnthropicProviderContext(ctx context.Context) (*AnthropicProvider, error) {
	return NewAnthropicProviderWithConfigContext(ctx, ProviderConfig{})
}

// NewAnthropicProviderWithConfig creates a provider using ResolveAnthropicKey
// and optional config values. ANTHROPIC_BASE_URL overrides cfg.BaseURL.
func NewAnthropicProviderWithConfig(cfg ProviderConfig) (*AnthropicProvider, error) {
	return NewAnthropicProviderWithConfigContext(defaultCredentialContext(), cfg)
}

// NewAnthropicProviderWithConfigContext creates a provider using
// ResolveAnthropicKeyContext and optional config values. ANTHROPIC_BASE_URL
// overrides cfg.BaseURL.
func NewAnthropicProviderWithConfigContext(ctx context.Context, cfg ProviderConfig) (*AnthropicProvider, error) {
	key, bearer, err := ResolveAnthropicKeyContext(ctx)
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
	return []string{
		"claude-sonnet-4-20250514",
		"claude-haiku-4-20250414",
		"claude-opus-4-20250514",
	}
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
		return nil, fmt.Errorf("anthropic: models HTTP %d: %s", resp.StatusCode, body)
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

// HealthCheck verifies that the Anthropic API is reachable and the credentials
// are valid by issuing a lightweight GET /v1/models request.
func (a *AnthropicProvider) HealthCheck(ctx context.Context) error {
	_, err := a.FetchModels(ctx)
	return err
}

type anthropicRequest struct {
	Temperature *float64           `json:"temperature,omitempty"`
	TopP        *float64           `json:"top_p,omitempty"`
	Model       string             `json:"model"`
	System      string             `json:"system,omitempty"`
	Thinking    *anthropicThinking `json:"thinking,omitempty"`
	Messages    []anthropicMessage `json:"messages"`
	Stop        []string           `json:"stop_sequences,omitempty"`
	Tools       []anthropicTool    `json:"tools,omitempty"`
	MaxTokens   int                `json:"max_tokens"`
}

type anthropicThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens"`
}

type anthropicTool struct {
	InputSchema map[string]any `json:"input_schema"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
}

// anthropicMessage uses json.RawMessage for Content so it can be either
// a plain string (user text) or an array of content blocks (tool results,
// assistant tool_use responses).
type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// anthropicContentBlock is a single block in an Anthropic message content array.
type anthropicContentBlock struct {
	Type      string         `json:"type"`
	Text      string         `json:"text,omitempty"`
	ID        string         `json:"id,omitempty"`
	Name      string         `json:"name,omitempty"`
	Input     map[string]any `json:"input,omitempty"`
	ToolUseID string         `json:"tool_use_id,omitempty"`
	Content   string         `json:"content,omitempty"`
	IsError   bool           `json:"is_error,omitempty"`
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
	req, err := buildAnthropicRequest(params)
	if err != nil {
		return nil, err
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("anthropic: marshal: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("anthropic: new request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("anthropic-version", defaultAnthropicVersion)
	a.setAuthHeaders(httpReq)

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
		return nil, fmt.Errorf("anthropic: HTTP %d: %s", resp.StatusCode, respBody)
	}

	var ar anthropicResponse
	if err := json.Unmarshal(respBody, &ar); err != nil {
		return nil, fmt.Errorf("anthropic: unmarshal: %w", err)
	}

	if ar.Error != nil {
		return nil, fmt.Errorf("anthropic: %s: %s", ar.Error.Type, ar.Error.Message)
	}

	return parseAnthropicResponse(ar), nil
}

func parseAnthropicResponse(ar anthropicResponse) *Response {
	result := &Response{
		Model:             ar.Model,
		StopReason:        anthropicStopReason(ar.StopReason),
		InputTokens:       ar.Usage.InputTokens + ar.Usage.CacheCreationInputTokens + ar.Usage.CacheReadInputTokens,
		CachedInputTokens: ar.Usage.CacheReadInputTokens,
		OutputTokens:      ar.Usage.OutputTokens,
	}

	var textParts strings.Builder

	for _, block := range ar.Content {
		switch block.Type {
		case "text":
			textParts.WriteString(block.Text)
		case "tool_use":
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
	case "end_turn":
		return StopEndTurn
	case "tool_use":
		return StopToolUse
	case "max_tokens":
		return StopMaxToks
	default:
		return StopUnknown
	}
}

func buildAnthropicRequest(params CompleteParams) (anthropicRequest, error) {
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
		System:      system,
		Stop:        params.Stop,
		Temperature: params.Temperature,
		TopP:        params.TopP,
	}

	// Add tool definitions.
	for _, tool := range params.Tools {
		req.Tools = append(req.Tools, anthropicTool{
			Name:        tool.Name,
			Description: tool.Description,
			InputSchema: tool.Parameters,
		})
	}

	budget, ok, err := anthropicThinkingBudget(params.ReasoningLevel, maxTok)
	if err != nil {
		return anthropicRequest{}, fmt.Errorf("anthropic: %w", err)
	}

	if ok {
		req.Thinking = &anthropicThinking{Type: "enabled", BudgetTokens: budget}
	}

	return req, nil
}

// buildAnthropicMessage converts an llm.Message to the Anthropic wire format.
// Plain user/assistant text is sent as a JSON string; tool-use and tool-result
// messages use the content-block array format.
func buildAnthropicMessage(m Message) anthropicMessage {
	// Assistant message with tool calls -> content block array.
	if m.Role == RoleAssistant && len(m.ToolCalls) > 0 {
		var blocks []anthropicContentBlock
		if m.Content != "" {
			blocks = append(blocks, anthropicContentBlock{Type: "text", Text: m.Content})
		}

		for _, tc := range m.ToolCalls {
			blocks = append(blocks, anthropicContentBlock{
				Type:  "tool_use",
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
			Type:      "tool_result",
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
	return anthropicContextWindow(model)
}

func anthropicContextWindow(model string) int {
	switch model {
	case "claude-opus-4-20250514", "claude-sonnet-4-20250514",
		"claude-haiku-4-20250414":
		return 200_000
	default:
		// Newer models default to 200k; fall back for unknowns.
		if strings.HasPrefix(model, "claude") {
			return 200_000
		}

		return 0
	}
}
