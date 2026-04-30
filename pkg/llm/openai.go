package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
)

const defaultOpenAIBase = "https://api.openai.com"

// OpenAIProvider calls the OpenAI Chat Completions API.
type OpenAIProvider struct {
	client  *http.Client
	apiKey  string
	baseURL string
	bearer  bool
}

// NewOpenAIProvider creates a provider using ResolveOpenAIKey.
// The base URL can be overridden with OPENAI_BASE_URL.
func NewOpenAIProvider() (*OpenAIProvider, error) {
	return NewOpenAIProviderWithConfig(ProviderConfig{})
}

// NewOpenAIProviderWithConfig creates a provider using ResolveOpenAIKey and
// optional config values. OPENAI_BASE_URL overrides cfg.BaseURL.
func NewOpenAIProviderWithConfig(cfg ProviderConfig) (*OpenAIProvider, error) {
	key, bearer, err := ResolveOpenAIKey()
	if err != nil {
		return nil, err
	}

	return &OpenAIProvider{
		apiKey:  key,
		bearer:  bearer,
		baseURL: configuredBaseURL("OPENAI_BASE_URL", cfg.BaseURL, defaultOpenAIBase),
		client:  &http.Client{},
	}, nil
}

// Name returns the provider name.
func (o *OpenAIProvider) Name() string { return providerOpenAI }

// Models returns the static list of supported models (fallback).
func (o *OpenAIProvider) Models() []string {
	return []string{
		"gpt-4.1",
		"gpt-4.1-mini",
		"gpt-4.1-nano",
		"o4-mini",
	}
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
		return nil, fmt.Errorf("openai: models HTTP %d: %s", resp.StatusCode, body)
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
	Temperature *float64        `json:"temperature,omitempty"`
	TopP        *float64        `json:"top_p,omitempty"`
	Model       string          `json:"model"`
	Messages    []openaiMessage `json:"messages"`
	Stop        []string        `json:"stop,omitempty"`
	MaxTokens   int             `json:"max_tokens,omitempty"`
}

type openaiMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openaiResponse struct {
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// Complete performs a chat completion using the OpenAI Chat Completions API.
func (o *OpenAIProvider) Complete(ctx context.Context, params CompleteParams) (*Response, error) {
	msgs := make([]openaiMessage, 0, len(params.Messages))
	for _, m := range params.Messages {
		msgs = append(msgs, openaiMessage{Role: string(m.Role), Content: m.Content})
	}

	req := openaiRequest{
		Model:    params.Model,
		Messages: msgs,
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
		return nil, fmt.Errorf("openai: HTTP %d: %s", resp.StatusCode, respBody)
	}

	var or openaiResponse
	if err := json.Unmarshal(respBody, &or); err != nil {
		return nil, fmt.Errorf("openai: unmarshal: %w", err)
	}

	if or.Error != nil {
		return nil, fmt.Errorf("openai: %s: %s", or.Error.Type, or.Error.Message)
	}

	var text string
	if len(or.Choices) > 0 {
		text = or.Choices[0].Message.Content
	}

	return &Response{
		Content:      text,
		Model:        or.Model,
		InputTokens:  or.Usage.PromptTokens,
		OutputTokens: or.Usage.CompletionTokens,
	}, nil
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
