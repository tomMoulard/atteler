package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/tommoulard/atteler/pkg/events"
)

// ClaudeCodeProvider calls the Anthropic Messages API directly using the
// OAuth access token Claude Code stored at login. It auto-refreshes the
// access token on 401 and persists refreshed tokens back to whichever store
// they came from (macOS keychain or ~/.claude/.credentials.json), so atteler
// and the claude CLI keep a shared session.
type ClaudeCodeProvider struct {
	client  *http.Client
	auth    *claudeCodeAuth
	baseURL string
	models  []string
}

// NewClaudeCodeProvider creates a provider backed by the Claude Code OAuth
// credentials discovered on the local machine.
func NewClaudeCodeProvider() (*ClaudeCodeProvider, error) {
	return NewClaudeCodeProviderContext(defaultCredentialContext())
}

// NewClaudeCodeProviderContext creates a provider using ctx for credential
// discovery (keychain probe / file reads).
func NewClaudeCodeProviderContext(ctx context.Context) (*ClaudeCodeProvider, error) {
	ctx = nonNilCredentialContext(ctx)

	auth, err := loadClaudeCodeAuth(ctx)
	if err != nil {
		return nil, err
	}

	return &ClaudeCodeProvider{
		client:  providerHTTPClient(ProviderConfig{}),
		auth:    auth,
		baseURL: configuredBaseURL("ANTHROPIC_BASE_URL", "", defaultAnthropicBase),
		models:  defaultClaudeCodeModels(),
	}, nil
}

// Name returns the provider name.
func (c *ClaudeCodeProvider) Name() string { return providerClaudeCode }

// Models returns model IDs/aliases Claude Code can serve.
func (c *ClaudeCodeProvider) Models() []string {
	if len(c.models) == 0 {
		return defaultClaudeCodeModels()
	}

	return append([]string(nil), c.models...)
}

// FetchModels returns the local Claude Code model catalog. The OAuth-mode
// /v1/models endpoint is gated separately; we keep a static list to avoid an
// extra round-trip for what is effectively a UI-only listing.
func (c *ClaudeCodeProvider) FetchModels(_ context.Context) ([]string, error) {
	return c.Models(), nil
}

// HealthCheck verifies that we have an OAuth access token loaded. It does not
// hit the network — provider-level credential validity is asserted lazily on
// the next Complete call (with auto-refresh on 401).
func (c *ClaudeCodeProvider) HealthCheck(ctx context.Context) error {
	emitActivity(ctx, events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command":  "claude_code.auth.check",
			"provider": providerClaudeCode,
		},
	})

	if c.auth == nil || c.auth.snapshot() == "" {
		return errors.New("no Claude Code OAuth access token: run `claude` to log in")
	}

	return nil
}

// ModelContextWindow returns the context window size for a Claude Code model.
func (c *ClaudeCodeProvider) ModelContextWindow(model string) int {
	return anthropicContextWindow(model)
}

// Complete performs a chat completion against the Anthropic Messages API,
// refreshing the OAuth access token once on 401 and retrying transparently.
func (c *ClaudeCodeProvider) Complete(ctx context.Context, params CompleteParams) (*Response, error) {
	model := params.Model
	if model == "" {
		models := c.Models()
		if len(models) == 0 {
			return nil, errors.New("claude code model not configured")
		}

		model = models[0]
	}

	params.Model = model

	req, err := buildAnthropicRequest(params)
	if err != nil {
		return nil, err
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("claude code: marshal: %w", err)
	}

	emitActivity(ctx, events.Event{
		Type:  events.CommandExecute,
		Model: model,
		Metadata: map[string]string{
			"command":  "claude_code.messages",
			"provider": providerClaudeCode,
		},
	})

	resp, err := c.doMessagesRequest(ctx, body)
	if err != nil {
		return nil, err
	}

	resp.Model = firstNonEmptyString(resp.Model, model)

	return resp, nil
}

func (c *ClaudeCodeProvider) doMessagesRequest(ctx context.Context, body []byte) (*Response, error) {
	access := c.auth.snapshot()

	resp, err := c.sendMessages(ctx, body, access)
	if err == nil {
		return resp, nil
	}

	var unauthorized *claudeCodeUnauthorizedError
	if !errors.As(err, &unauthorized) {
		return nil, err
	}

	if refreshErr := c.auth.refresh(ctx, access); refreshErr != nil {
		return nil, fmt.Errorf("claude code refresh after 401: %w", refreshErr)
	}

	access = c.auth.snapshot()

	return c.sendMessages(ctx, body, access)
}

type claudeCodeUnauthorizedError struct {
	body string
}

func (e *claudeCodeUnauthorizedError) Error() string {
	return "claude code: HTTP 401: " + e.body
}

func (c *ClaudeCodeProvider) sendMessages(ctx context.Context, body []byte, access string) (*Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("claude code: new request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("anthropic-version", defaultAnthropicVersion)
	httpReq.Header.Set("anthropic-beta", anthropicOAuthBetas)
	httpReq.Header.Set("Authorization", "Bearer "+access)

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("claude code: request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxOAuthErrorBodyBytes)) //nolint:errcheck // best-effort body capture for the error message
		return nil, &claudeCodeUnauthorizedError{body: string(raw)}
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("claude code: read body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("claude code: HTTP %d: %s", resp.StatusCode, respBody)
	}

	var ar anthropicResponse
	if err := json.Unmarshal(respBody, &ar); err != nil {
		return nil, fmt.Errorf("claude code: unmarshal: %w", err)
	}

	if ar.Error != nil {
		return nil, fmt.Errorf("claude code: %s: %s", ar.Error.Type, ar.Error.Message)
	}

	var b strings.Builder
	for _, c := range ar.Content {
		b.WriteString(c.Text)
	}

	return &Response{
		Content:           b.String(),
		Model:             ar.Model,
		InputTokens:       ar.Usage.InputTokens + ar.Usage.CacheCreationInputTokens + ar.Usage.CacheReadInputTokens,
		CachedInputTokens: ar.Usage.CacheReadInputTokens,
		OutputTokens:      ar.Usage.OutputTokens,
	}, nil
}

func defaultClaudeCodeModels() []string {
	return []string{
		"claude-opus-4-7",
		"claude-opus-4-6",
		"claude-opus-4-5-20251101",
		"claude-opus-4-1-20250805",
		"claude-opus-4-20250514",
		"claude-sonnet-4-6",
		"claude-sonnet-4-5-20250929",
		"claude-sonnet-4-20250514",
		"claude-haiku-4-5-20251001",
		"opus",
		"sonnet",
		"haiku",
	}
}
