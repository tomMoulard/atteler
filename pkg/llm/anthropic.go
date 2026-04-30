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
	key, bearer, err := ResolveAnthropicKey()
	if err != nil {
		return nil, err
	}

	base := defaultAnthropicBase
	if v := envOr("ANTHROPIC_BASE_URL", ""); v != "" {
		base = v
	}

	return &AnthropicProvider{
		apiKey:  key,
		bearer:  bearer,
		baseURL: base,
		client:  &http.Client{},
	}, nil
}

// Name returns the provider name.
func (a *AnthropicProvider) Name() string { return "anthropic" }

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
	if a.bearer {
		httpReq.Header.Set("Authorization", "Bearer "+a.apiKey)
	} else {
		httpReq.Header.Set("X-Api-Key", a.apiKey)
	}

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

// ---------------------------------------------------------------------------
// Anthropic Messages API request / response shapes
// ---------------------------------------------------------------------------

type anthropicRequest struct {
	Temperature *float64           `json:"temperature,omitempty"`
	TopP        *float64           `json:"top_p,omitempty"`
	Model       string             `json:"model"`
	System      string             `json:"system,omitempty"`
	Messages    []anthropicMessage `json:"messages"`
	Stop        []string           `json:"stop_sequences,omitempty"`
	MaxTokens   int                `json:"max_tokens"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicResponse struct {
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
	Model   string `json:"model"`
	Content []struct {
		Text string `json:"text"`
	} `json:"content"`
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// Complete performs a chat completion using the Anthropic Messages API.
func (a *AnthropicProvider) Complete(ctx context.Context, params CompleteParams) (*Response, error) {
	// Separate system message from the rest.
	var system string
	msgs := make([]anthropicMessage, 0, len(params.Messages))
	for _, m := range params.Messages {
		if m.Role == RoleSystem {
			system = m.Content
			continue
		}
		msgs = append(msgs, anthropicMessage{Role: string(m.Role), Content: m.Content})
	}

	maxTok := params.MaxTokens
	if maxTok <= 0 {
		maxTok = 4096
	}

	req := anthropicRequest{
		Model:     params.Model,
		MaxTokens: maxTok,
		Messages:  msgs,
		System:    system,
		Stop:      params.Stop,
	}
	if params.Temperature >= 0 {
		req.Temperature = &params.Temperature
	}
	if params.TopP >= 0 {
		req.TopP = &params.TopP
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
	if a.bearer {
		httpReq.Header.Set("Authorization", "Bearer "+a.apiKey)
	} else {
		httpReq.Header.Set("X-Api-Key", a.apiKey)
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
		return nil, fmt.Errorf("anthropic: HTTP %d: %s", resp.StatusCode, respBody)
	}

	var ar anthropicResponse
	if err := json.Unmarshal(respBody, &ar); err != nil {
		return nil, fmt.Errorf("anthropic: unmarshal: %w", err)
	}

	if ar.Error != nil {
		return nil, fmt.Errorf("anthropic: %s: %s", ar.Error.Type, ar.Error.Message)
	}

	var b strings.Builder
	for _, c := range ar.Content {
		b.WriteString(c.Text)
	}

	return &Response{
		Content:      b.String(),
		Model:        ar.Model,
		InputTokens:  ar.Usage.InputTokens,
		OutputTokens: ar.Usage.OutputTokens,
	}, nil
}
