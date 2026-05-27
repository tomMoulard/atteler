package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

const defaultOpenAIBase = "https://api.openai.com"

// OpenAIProvider calls the OpenAI Chat Completions API.
type OpenAIProvider struct {
	client  *http.Client
	apiKey  string
	baseURL string
	bearer  bool
}

// NewOpenAIProvider is kept for source compatibility only.
//
// Deprecated: use NewOpenAIProviderContext so credential reads inherit caller
// cancellation checks.
func NewOpenAIProvider() (*OpenAIProvider, error) {
	return nil, ErrContextRequired
}

// NewOpenAIProviderContext creates a provider using ResolveOpenAIKeyContext.
// The base URL can be overridden with OPENAI_BASE_URL.
func NewOpenAIProviderContext(ctx context.Context) (*OpenAIProvider, error) {
	return NewOpenAIProviderWithConfigContext(ctx, ProviderConfig{})
}

// NewOpenAIProviderWithConfig is kept for source compatibility only.
//
// Deprecated: use NewOpenAIProviderWithConfigContext so credential reads
// inherit caller cancellation checks.
func NewOpenAIProviderWithConfig(_ ProviderConfig) (*OpenAIProvider, error) {
	return nil, ErrContextRequired
}

// NewOpenAIProviderWithConfigContext creates a provider using
// ResolveOpenAIKeyContext and optional config values. OPENAI_BASE_URL overrides
// cfg.BaseURL.
func NewOpenAIProviderWithConfigContext(ctx context.Context, cfg ProviderConfig) (*OpenAIProvider, error) {
	key, bearer, err := ResolveOpenAIKeyContext(ctx)
	if err != nil {
		return nil, err
	}

	return &OpenAIProvider{
		apiKey:  key,
		bearer:  bearer,
		baseURL: configuredBaseURL("OPENAI_BASE_URL", cfg.BaseURL, defaultOpenAIBase),
		client:  providerHTTPClient(cfg),
	}, nil
}

// Name returns the provider name.
func (o *OpenAIProvider) Name() string { return providerOpenAI }

// Models returns the static list of supported models (fallback).
func (o *OpenAIProvider) Models() []string {
	static := []string{
		"gpt-4.1",
		"gpt-4.1-mini",
		"gpt-4.1-nano",
		"o4-mini",
	}

	return mergeModelLists(static, catalogModelsByProvider()[providerOpenAI])
}

// ---------------------------------------------------------------------------
// OpenAI Models API
// ---------------------------------------------------------------------------

type openaiModelsResponse struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

// FetchModels queries GET /v1/models to discover available models.
func (o *OpenAIProvider) FetchModels(ctx context.Context) ([]string, error) {
	if err := requireCredentialContext(ctx); err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, o.baseURL+"/v1/models", http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("openai: new request: %w", err)
	}

	httpReq.Header.Set("Authorization", "Bearer "+o.apiKey)

	resp, err := o.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openai: models request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("openai: read models body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, newProviderHTTPError(providerOpenAI, resp, body)
	}

	var mr openaiModelsResponse
	if err := json.Unmarshal(body, &mr); err != nil {
		return nil, fmt.Errorf("openai: unmarshal models: %w", err)
	}

	out := make([]string, 0, len(mr.Data))
	for _, m := range mr.Data {
		out = append(out, m.ID)
	}

	return out, nil
}

// HealthCheck verifies that the OpenAI API is reachable and the credentials
// are valid by issuing a lightweight GET /v1/models request.
func (o *OpenAIProvider) HealthCheck(ctx context.Context) error {
	_, err := o.FetchModels(ctx)
	return err
}

// ---------------------------------------------------------------------------
// OpenAI Chat Completions request / response shapes
// ---------------------------------------------------------------------------

type openaiRequest struct {
	Temperature     *float64        `json:"temperature,omitempty"`
	TopP            *float64        `json:"top_p,omitempty"`
	Seed            *int            `json:"seed,omitempty"`
	Model           string          `json:"model"`
	ReasoningEffort string          `json:"reasoning_effort,omitempty"`
	Messages        []openaiMessage `json:"messages"`
	Stop            []string        `json:"stop,omitempty"`
	Tools           []openaiTool    `json:"tools,omitempty"`
	MaxTokens       int             `json:"max_tokens,omitempty"`
}

type openaiTool struct {
	Function openaiToolFunction `json:"function"`
	Type     string             `json:"type"`
}

type openaiToolFunction struct {
	Parameters  map[string]any `json:"parameters"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
}

type openaiMessage struct {
	Role       string           `json:"role"`
	Content    string           `json:"content,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	ToolCalls  []openaiToolCall `json:"tool_calls,omitempty"`
}

type openaiToolCall struct {
	ID       string                 `json:"id"`
	Type     string                 `json:"type"`
	Function openaiToolCallFunction `json:"function"`
}

type openaiToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON string.
}

type openaiResponse struct {
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
	Model   string `json:"model"`
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Content   string           `json:"content"`
			ToolCalls []openaiToolCall `json:"tool_calls,omitempty"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		PromptTokensDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
}

// Complete performs a chat completion using the OpenAI Chat Completions API.
func (o *OpenAIProvider) Complete(ctx context.Context, params CompleteParams) (*Response, error) {
	if err := requireCredentialContext(ctx); err != nil {
		return nil, err
	}

	return o.complete(ctx, params)
}

func (o *OpenAIProvider) complete(ctx context.Context, params CompleteParams) (*Response, error) {
	req, err := buildOpenAIRequest(params)
	if err != nil {
		return nil, err
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("openai: marshal: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("openai: new request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+o.apiKey)

	resp, err := o.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openai: request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("openai: read body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, newProviderHTTPError(providerOpenAI, resp, respBody)
	}

	var or openaiResponse
	if err := json.Unmarshal(respBody, &or); err != nil {
		return nil, fmt.Errorf("openai: unmarshal: %w", err)
	}

	if or.Error != nil {
		return nil, newProviderPayloadError(
			providerOpenAI,
			resp.StatusCode,
			resp.Header,
			firstNonEmptyString(or.Error.Code, or.Error.Type),
			or.Error.Message,
		)
	}

	return parseOpenAIResponse(or), nil
}

func buildOpenAIRequest(params CompleteParams) (openaiRequest, error) {
	if err := validateCompleteParamsSupported(providerOpenAI, params); err != nil {
		return openaiRequest{}, err
	}

	req := openaiRequest{
		Model:    params.Model,
		Messages: buildOpenAIMessages(params.Messages),
		Stop:     params.Stop,
	}
	if params.MaxTokens > 0 {
		req.MaxTokens = params.MaxTokens
	}

	if params.Temperature != nil {
		req.Temperature = params.Temperature
	}

	if params.TopP != nil {
		req.TopP = params.TopP
	}

	if params.Seed != nil {
		req.Seed = params.Seed
	}

	if effort := openAIReasoningEffort(params.ReasoningLevel); effort != "" {
		req.ReasoningEffort = effort
	}

	for _, tool := range params.Tools {
		req.Tools = append(req.Tools, openaiTool{
			Type:     "function",
			Function: openaiToolFunction(tool),
		})
	}

	return req, nil
}

func parseOpenAIResponse(or openaiResponse) *Response {
	result := &Response{
		Provider:          providerOpenAI,
		Model:             or.Model,
		InputTokens:       or.Usage.PromptTokens,
		CachedInputTokens: or.Usage.PromptTokensDetails.CachedTokens,
		OutputTokens:      or.Usage.CompletionTokens,
	}

	if len(or.Choices) > 0 {
		choice := or.Choices[0]
		result.Content = choice.Message.Content
		result.StopReason = openaiStopReason(choice.FinishReason)
		result.ToolCalls = parseOpenAIToolCalls(choice.Message.ToolCalls)
	}

	return result
}

func buildOpenAIMessages(messages []Message) []openaiMessage {
	msgs := make([]openaiMessage, 0, len(messages))

	for _, m := range messages {
		omsg := openaiMessage{Role: string(m.Role), Content: m.Content}

		// Marshal assistant messages with tool calls.
		if m.Role == RoleAssistant && len(m.ToolCalls) > 0 {
			for _, tc := range m.ToolCalls {
				args, err := json.Marshal(tc.Input)
				if err != nil {
					args = []byte("{}")
				}

				omsg.ToolCalls = append(omsg.ToolCalls, openaiToolCall{
					ID:   tc.ID,
					Type: "function",
					Function: openaiToolCallFunction{
						Name:      tc.Name,
						Arguments: string(args),
					},
				})
			}
		}

		// Marshal tool result messages.
		if m.Role == RoleTool && m.ToolResult != nil {
			omsg.ToolCallID = m.ToolResult.ToolCallID
			omsg.Content = m.ToolResult.Content
		}

		msgs = append(msgs, omsg)
	}

	return msgs
}

func parseOpenAIToolCalls(calls []openaiToolCall) []ToolCall {
	if len(calls) == 0 {
		return nil
	}

	out := make([]ToolCall, 0, len(calls))

	for _, tc := range calls {
		var input map[string]any

		if err := json.Unmarshal([]byte(tc.Function.Arguments), &input); err != nil {
			input = map[string]any{"raw": tc.Function.Arguments}
		}

		out = append(out, ToolCall{
			ID:    tc.ID,
			Name:  tc.Function.Name,
			Input: input,
		})
	}

	return out
}

func openaiStopReason(reason string) StopReason {
	switch reason {
	case "stop":
		return StopEndTurn
	case "tool_calls":
		return StopToolUse
	case "length":
		return StopMaxToks
	default:
		return StopUnknown
	}
}

func configuredBaseURL(envKey, configured, fallback string) string {
	if v := os.Getenv(envKey); v != "" {
		return v
	}

	if configured != "" {
		return configured
	}

	return fallback
}

// ModelContextWindow returns the context window size for an OpenAI model.
func (o *OpenAIProvider) ModelContextWindow(model string) int {
	if limit := catalogContextWindow(providerOpenAI, model); limit > 0 {
		return limit
	}

	return openaiContextWindow(model)
}

//nolint:cyclop // Flat model lookup table is clearer as a switch.
func openaiContextWindow(model string) int {
	switch model {
	case "gpt-4.1":
		return 1_047_576
	case "gpt-4.1-mini":
		return 1_047_576
	case "gpt-4.1-nano":
		return 1_047_576
	case "o4-mini":
		return 200_000
	case "o3", "o3-pro":
		return 200_000
	case "o3-mini":
		return 200_000
	case "o1", "o1-pro":
		return 200_000
	case "o1-mini":
		return 128_000
	case "gpt-4o", "gpt-4o-mini":
		return 128_000
	case "gpt-4-turbo":
		return 128_000
	case "gpt-4":
		return 8_192
	default:
		if strings.HasPrefix(model, "gpt-4.1") {
			return 1_047_576
		}

		if strings.HasPrefix(model, "gpt-4o") || strings.HasPrefix(model, "gpt-4-turbo") {
			return 128_000
		}

		if strings.HasPrefix(model, "o1") || strings.HasPrefix(model, "o3") || strings.HasPrefix(model, "o4") {
			return 200_000
		}

		return 0
	}
}
