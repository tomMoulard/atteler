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
	"strings"

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

// NewCodexProvider creates a provider backed by ~/.codex/auth.json.
// It returns an error when no chatgpt-mode credentials are present.
func NewCodexProvider() (*CodexProvider, error) {
	auth, err := loadCodexChatGPTAuth(codexConfigDir())
	if err != nil {
		return nil, fmt.Errorf("no Codex credentials found: %w (run `codex login`)", err)
	}

	return &CodexProvider{
		client:  providerHTTPClient(ProviderConfig{}),
		auth:    auth,
		baseURL: configuredBaseURL("CODEX_BASE_URL", "", codexChatGPTAPIBase),
		models:  codexModels(),
	}, nil
}

// Name returns the provider name.
func (c *CodexProvider) Name() string { return providerCodex }

// Models returns Codex CLI model IDs.
func (c *CodexProvider) Models() []string {
	if len(c.models) == 0 {
		return defaultCodexModels()
	}

	return append([]string(nil), c.models...)
}

// FetchModels returns the local Codex model catalog. The chatgpt backend does
// not expose a /models endpoint for this auth mode.
func (c *CodexProvider) FetchModels(_ context.Context) ([]string, error) {
	return c.Models(), nil
}

// HealthCheck verifies that auth.json parses and contains a chatgpt-mode token.
// It does not contact the network.
func (c *CodexProvider) HealthCheck(ctx context.Context) error {
	emitActivity(ctx, events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command":  "codex.auth.check",
			"provider": providerCodex,
		},
	})

	access, _ := c.auth.snapshot()
	if access == "" {
		return errors.New("no Codex chatgpt access_token in auth.json: run `codex login`")
	}

	return nil
}

// ModelContextWindow returns the context window size for a Codex model.
func (c *CodexProvider) ModelContextWindow(model string) int {
	switch model {
	case "gpt-5.5", "gpt-5.4":
		return 400_000
	case "gpt-5.4-mini", "gpt-5.3-codex", "gpt-5.3-codex-spark":
		return 200_000
	default:
		if strings.HasPrefix(model, "gpt-") {
			return 200_000
		}

		return 0
	}
}

// codexResponsesRequest mirrors the subset of the OpenAI Responses API that
// the codex backend accepts.
type codexResponsesRequest struct {
	Reasoning    *codexRequestReasoning `json:"reasoning,omitempty"`
	Model        string                 `json:"model"`
	Instructions string                 `json:"instructions,omitempty"`
	Input        []codexInputItem       `json:"input"`
	Stream       bool                   `json:"stream"`
	Store        bool                   `json:"store"`
}

type codexRequestReasoning struct {
	Effort string `json:"effort,omitempty"`
}

type codexInputItem struct {
	Type    string              `json:"type"`
	Role    string              `json:"role"`
	Content []codexInputContent `json:"content"`
}

type codexInputContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Complete runs an OpenAI Responses API request against the chatgpt codex
// backend, refreshing the access token once on a 401.
func (c *CodexProvider) Complete(ctx context.Context, params CompleteParams) (*Response, error) {
	model := params.Model
	if model == "" {
		models := c.Models()
		if len(models) == 0 {
			return nil, errors.New("codex model not configured")
		}

		model = models[0]
	}

	req := codexResponsesRequest{
		Model:        model,
		Instructions: codexInstructions(params.Messages),
		Input:        codexBuildInput(params.Messages),
		Stream:       true,
	}

	if effort := openAIReasoningEffort(params.ReasoningLevel); effort != "" {
		req.Reasoning = &codexRequestReasoning{Effort: effort}
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("codex marshal: %w", err)
	}

	emitActivity(ctx, events.Event{
		Type:  events.CommandExecute,
		Model: model,
		Metadata: map[string]string{
			"command":  "codex.responses",
			"provider": providerCodex,
		},
	})

	resp, err := c.doResponsesRequest(ctx, body)
	if err != nil {
		return nil, err
	}

	resp.Model = firstNonEmptyString(resp.Model, model)

	return resp, nil
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

type codexUnauthorizedError struct {
	body string
}

func (e *codexUnauthorizedError) Error() string {
	return "codex: HTTP 401: " + e.body
}

func (c *CodexProvider) sendResponses(ctx context.Context, body []byte, access, accountID string) (*Response, error) {
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
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxOAuthErrorBodyBytes)) //nolint:errcheck // best-effort body capture for the error message
		return nil, &codexUnauthorizedError{body: string(raw)}
	}

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxOAuthErrorBodyBytes)) //nolint:errcheck // best-effort body capture for the error message
		return nil, fmt.Errorf("codex HTTP %d: %s", resp.StatusCode, raw)
	}

	return parseCodexSSE(resp.Body)
}

// codexStreamState accumulates partial state from an SSE response stream.
type codexStreamState struct {
	finalText string
	deltaBuf  strings.Builder
	out       Response
	finished  bool
}

// parseCodexSSE consumes an OpenAI Responses-API SSE stream and returns the
// final assistant message and usage. It prefers the explicit message item
// when available, falling back to accumulated text deltas otherwise.
func parseCodexSSE(r io.Reader) (*Response, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	state := &codexStreamState{}

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}

		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}

		if err := state.handleEventPayload(payload); err != nil {
			return nil, err
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("codex stream read: %w", err)
	}

	return state.finalize()
}

func (s *codexStreamState) handleEventPayload(payload string) error {
	var event codexStreamEvent
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		// Tolerate malformed events; some backends interleave keep-alive
		// pings or partial frames with JSON data lines.
		return nil //nolint:nilerr // intentional: skip unparseable lines
	}

	switch event.Type {
	case "response.output_text.delta":
		s.deltaBuf.WriteString(event.Delta)
	case "response.output_item.done":
		if text := codexExtractMessageText(event.Item); text != "" {
			s.finalText = text
		}
	case "response.completed":
		s.finished = true
		s.applyCompleted(event.Response)
	case "response.failed", "error":
		return fmt.Errorf("codex stream error: %s", payload)
	}

	return nil
}

func (s *codexStreamState) applyCompleted(resp *codexEventPayload) {
	if resp == nil {
		return
	}

	if resp.Model != "" {
		s.out.Model = resp.Model
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

	if s.out.Content == "" && !s.finished {
		return nil, errors.New("codex stream ended before response.completed")
	}

	if s.out.Content == "" {
		return nil, errors.New("codex stream completed with empty assistant message")
	}

	return &s.out, nil
}

type codexStreamEvent struct {
	Item     *codexEventItem    `json:"item,omitempty"`
	Response *codexEventPayload `json:"response,omitempty"`
	Type     string             `json:"type"`
	Delta    string             `json:"delta,omitempty"`
}

type codexEventItem struct {
	Type    string                  `json:"type"`
	Role    string                  `json:"role"`
	Content []codexEventItemContent `json:"content"`
}

type codexEventItemContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type codexEventPayload struct {
	Usage *codexEventUsage `json:"usage,omitempty"`
	Model string           `json:"model,omitempty"`
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
		if msg.Role == RoleSystem {
			continue
		}

		contentType := "input_text"
		if msg.Role == RoleAssistant {
			contentType = "output_text"
		}

		out = append(out, codexInputItem{
			Type: "message",
			Role: string(msg.Role),
			Content: []codexInputContent{{
				Type: contentType,
				Text: msg.Content,
			}},
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
	return []string{
		"gpt-5.5",
		"gpt-5.4",
		"gpt-5.4-mini",
		"gpt-5.3-codex",
		"gpt-5.3-codex-spark",
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
