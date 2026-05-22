package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const (
	defaultOllamaBase       = "http://127.0.0.1:11434"
	envOllamaAutoStart      = "ATTELER_OLLAMA_AUTO_START"
	ollamaServeCommand      = "ollama"
	ollamaStartupTimeout    = 5 * time.Second
	ollamaStartupPollPeriod = 100 * time.Millisecond
)

// OllamaProvider calls a local or configured Ollama server.
type OllamaProvider struct {
	client         *http.Client
	baseURL        string
	autoStart      bool
	startAttempted bool
}

// NewOllamaProvider creates a provider using OLLAMA_BASE_URL or the local
// Ollama default. The provider is created only when Ollama is reachable unless
// the base URL is explicitly configured.
func NewOllamaProvider(ctx context.Context) (*OllamaProvider, error) {
	return NewOllamaProviderWithConfigContext(ctx, ProviderConfig{})
}

// NewOllamaProviderWithConfigContext creates a provider using OLLAMA_BASE_URL,
// cfg.BaseURL, or the local Ollama default. OLLAMA_BASE_URL overrides cfg.BaseURL.
func NewOllamaProviderWithConfigContext(ctx context.Context, cfg ProviderConfig) (*OllamaProvider, error) {
	if err := requireCredentialContext(ctx); err != nil {
		return nil, err
	}

	baseURL := strings.TrimRight(configuredBaseURL("OLLAMA_BASE_URL", cfg.BaseURL, defaultOllamaBase), "/")
	p := &OllamaProvider{
		baseURL:   baseURL,
		client:    providerHTTPClient(cfg),
		autoStart: cfg.AutoStart && ollamaAutoStartEnabled() && isLocalOllamaBaseURL(baseURL),
	}

	if ollamaExplicitlyConfigured(cfg) && !p.autoStart {
		return p, nil
	}

	healthErr := p.HealthCheck(ctx)
	if healthErr == nil {
		return p, nil
	}

	if p.autoStart && isOllamaDaemonUnavailable(healthErr) {
		startErr := p.startDaemonAndWait(ctx)
		if startErr == nil {
			return p, nil
		}

		return nil, errors.Join(healthErr, startErr)
	}

	if ollamaExplicitlyConfigured(cfg) {
		return p, nil
	}

	return nil, healthErr
}

func ollamaExplicitlyConfigured(cfg ProviderConfig) bool {
	return os.Getenv("OLLAMA_BASE_URL") != "" || cfg.BaseURL != ""
}

type ollamaServeStarter func(ctx context.Context, baseURL string) error

var (
	ollamaServeStarterMu sync.Mutex
	startOllamaServe     ollamaServeStarter = startOllamaServeProcess
)

func (o *OllamaProvider) startDaemonAndWait(ctx context.Context) error {
	if o.startAttempted {
		return errors.New("ollama: daemon start already attempted")
	}

	o.startAttempted = true

	if err := callOllamaServeStarter(ctx, o.baseURL); err != nil {
		return err
	}

	return o.waitForDaemon(ctx)
}

func callOllamaServeStarter(ctx context.Context, baseURL string) error {
	ollamaServeStarterMu.Lock()
	starter := startOllamaServe
	ollamaServeStarterMu.Unlock()

	return starter(ctx, baseURL)
}

func startOllamaServeProcess(ctx context.Context, baseURL string) error {
	cmd := exec.CommandContext(ctx, ollamaServeCommand, "serve")
	cmd.Stdout = io.Discard

	cmd.Stderr = io.Discard
	if host := ollamaHostForBaseURL(baseURL); host != "" {
		cmd.Env = append(os.Environ(), "OLLAMA_HOST="+host)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("ollama: start daemon: %w", err)
	}

	go func() {
		if err := cmd.Wait(); err != nil {
			slog.Warn("ollama daemon exited", "error", err)
		}
	}()

	return nil
}

func (o *OllamaProvider) waitForDaemon(ctx context.Context) error {
	waitCtx, cancel := context.WithTimeout(ctx, ollamaStartupTimeout)
	defer cancel()

	ticker := time.NewTicker(ollamaStartupPollPeriod)
	defer ticker.Stop()

	var lastErr error

	for {
		err := o.HealthCheck(waitCtx)
		if err == nil {
			return nil
		}

		lastErr = err

		select {
		case <-waitCtx.Done():
			return fmt.Errorf("ollama: daemon did not become ready: %w", lastErr)
		case <-ticker.C:
		}
	}
}

func ollamaAutoStartEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(envOllamaAutoStart))) {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

func isLocalOllamaBaseURL(baseURL string) bool {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return false
	}

	host := strings.ToLower(parsed.Hostname())

	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func ollamaHostForBaseURL(baseURL string) string {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return ""
	}

	return parsed.Host
}

func isOllamaDaemonUnavailable(err error) bool {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return true
	}

	var netErr net.Error

	return errors.As(err, &netErr)
}

// Name returns the provider name.
func (o *OllamaProvider) Name() string { return providerOllama }

// Models returns useful built-in Ollama model names. Availability depends on
// what has been pulled into the target Ollama server; use FetchModels for live
// discovery.
func (o *OllamaProvider) Models() []string {
	return []string{
		"llama3.2",
		"llama3.1",
		"qwen2.5",
		"mistral",
		"gemma3",
		"deepseek-r1",
	}
}

type ollamaTagsResponse struct {
	Models []struct {
		Name string `json:"name"`
	} `json:"models"`
}

// FetchModels queries GET /api/tags to discover locally available Ollama models.
func (o *OllamaProvider) FetchModels(ctx context.Context) ([]string, error) {
	if err := requireCredentialContext(ctx); err != nil {
		return nil, err
	}

	if o.client == nil {
		o.client = providerHTTPClient(ProviderConfig{})
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, o.baseURL+"/api/tags", http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("ollama: new models request: %w", err)
	}

	resp, err := o.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("ollama: models request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("ollama: read models body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ollama: models HTTP %d: %s", resp.StatusCode, body)
	}

	var tr ollamaTagsResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return nil, fmt.Errorf("ollama: unmarshal models: %w", err)
	}

	out := make([]string, 0, len(tr.Models))
	for _, model := range tr.Models {
		if model.Name != "" {
			out = append(out, model.Name)
		}
	}

	return out, nil
}

// HealthCheck verifies that the Ollama server is reachable by listing tags.
func (o *OllamaProvider) HealthCheck(ctx context.Context) error {
	_, err := o.FetchModels(ctx)
	return err
}

type ollamaChatRequest struct {
	Think    any             `json:"think,omitempty"`
	Model    string          `json:"model"`
	Messages []ollamaMessage `json:"messages"`
	Tools    []ollamaTool    `json:"tools,omitempty"`
	Options  ollamaOptions   `json:"options"`
	Stream   bool            `json:"stream"`
}

type ollamaOptions struct {
	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`
	Seed        *int     `json:"seed,omitempty"`
	Stop        []string `json:"stop,omitempty"`
	NumPredict  int      `json:"num_predict,omitempty"`
}

type ollamaTool struct {
	Function ollamaToolFunction `json:"function"`
	Type     string             `json:"type"`
}

type ollamaToolFunction struct {
	Parameters  map[string]any `json:"parameters"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
}

type ollamaMessage struct {
	Role      string           `json:"role"`
	Content   string           `json:"content"`
	ToolCalls []ollamaToolCall `json:"tool_calls,omitempty"`
}

type ollamaToolCall struct {
	Function ollamaToolCallFunction `json:"function"`
}

type ollamaToolCallFunction struct {
	Arguments map[string]any `json:"arguments"`
	Name      string         `json:"name"`
}

type ollamaChatResponse struct {
	Error           string        `json:"error"`
	Model           string        `json:"model"`
	Message         ollamaMessage `json:"message"`
	PromptEvalCount int           `json:"prompt_eval_count"`
	EvalCount       int           `json:"eval_count"`
}

// Complete performs a non-streaming chat completion using Ollama's /api/chat endpoint.
func (o *OllamaProvider) Complete(ctx context.Context, params CompleteParams) (*Response, error) {
	if err := requireCredentialContext(ctx); err != nil {
		return nil, err
	}

	return o.complete(ctx, params)
}

func (o *OllamaProvider) complete(ctx context.Context, params CompleteParams) (*Response, error) {
	if params.Model == "" {
		return nil, errors.New("ollama: model is required")
	}

	if o.client == nil {
		o.client = providerHTTPClient(ProviderConfig{})
	}

	msgs := buildOllamaMessages(params.Messages)

	req := ollamaChatRequest{
		Model:    params.Model,
		Messages: msgs,
		Stream:   false,
		Options: ollamaOptions{
			Temperature: params.Temperature,
			TopP:        params.TopP,
			Seed:        params.Seed,
			Stop:        params.Stop,
		},
	}
	if params.MaxTokens > 0 {
		req.Options.NumPredict = params.MaxTokens
	}

	if think, ok := ollamaThink(params.ReasoningLevel); ok {
		req.Think = think
	}

	// Add tool definitions.
	for _, tool := range params.Tools {
		req.Tools = append(req.Tools, ollamaTool{
			Type:     "function",
			Function: ollamaToolFunction(tool),
		})
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("ollama: marshal: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("ollama: new request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := o.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("ollama: request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("ollama: read body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ollama: HTTP %d: %s", resp.StatusCode, respBody)
	}

	var or ollamaChatResponse
	if err := json.Unmarshal(respBody, &or); err != nil {
		return nil, fmt.Errorf("ollama: unmarshal: %w", err)
	}

	if or.Error != "" {
		return nil, fmt.Errorf("ollama: %s", or.Error)
	}

	model := or.Model
	if model == "" {
		model = params.Model
	}

	result := &Response{
		Content:      or.Message.Content,
		Model:        model,
		InputTokens:  or.PromptEvalCount,
		OutputTokens: or.EvalCount,
	}

	// Parse tool calls from response.
	result.ToolCalls = parseOllamaToolCalls(or.Message.ToolCalls)
	if len(result.ToolCalls) > 0 {
		result.StopReason = StopToolUse
	}

	return result, nil
}

func buildOllamaMessages(messages []Message) []ollamaMessage {
	msgs := make([]ollamaMessage, 0, len(messages))

	for _, m := range messages {
		omsg := ollamaMessage{Role: string(m.Role), Content: m.Content}

		// Marshal assistant messages with tool calls.
		if m.Role == RoleAssistant && len(m.ToolCalls) > 0 {
			for _, tc := range m.ToolCalls {
				omsg.ToolCalls = append(omsg.ToolCalls, ollamaToolCall{
					Function: ollamaToolCallFunction{
						Name:      tc.Name,
						Arguments: tc.Input,
					},
				})
			}
		}

		// Tool result messages use the "tool" role in Ollama.
		if m.Role == RoleTool && m.ToolResult != nil {
			omsg.Content = m.ToolResult.Content
		}

		msgs = append(msgs, omsg)
	}

	return msgs
}

func parseOllamaToolCalls(calls []ollamaToolCall) []ToolCall {
	if len(calls) == 0 {
		return nil
	}

	out := make([]ToolCall, 0, len(calls))

	for i, tc := range calls {
		out = append(out, ToolCall{
			ID:    fmt.Sprintf("ollama_%d", i),
			Name:  tc.Function.Name,
			Input: tc.Function.Arguments,
		})
	}

	return out
}

// ModelContextWindow returns known default context windows for common Ollama models.
func (o *OllamaProvider) ModelContextWindow(model string) int {
	model = strings.ToLower(strings.TrimSpace(model))

	model, _, _ = strings.Cut(model, ":")
	switch model {
	case "llama3.2", "llama3.1", "qwen2.5", "mistral", "gemma3", "deepseek-r1":
		return 128_000
	default:
		return 0
	}
}
