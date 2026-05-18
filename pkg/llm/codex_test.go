package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCodexProvider_Complete(t *testing.T) {
	t.Parallel()

	var (
		gotReq     codexResponsesRequest
		gotHeaders http.Header
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()

		body, err := io.ReadAll(r.Body)
		if !assert.NoError(t, err) {
			return
		}

		if !assert.NoError(t, json.Unmarshal(body, &gotReq)) {
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		writeCodexSSE(t, w, codexFakeSuccess("hello back", "gpt-5.5", 12, 4, 3))
	}))
	defer srv.Close()

	auth := newTestCodexAuth(t, "access-1", "refresh-1", "acct-42")
	p := &CodexProvider{
		client:  srv.Client(),
		auth:    auth,
		baseURL: srv.URL,
		models:  []string{"gpt-5.5"},
	}

	resp, err := p.Complete(context.Background(), CompleteParams{
		Model:          "gpt-5.5",
		ReasoningLevel: "high",
		Messages: []Message{
			{Role: RoleSystem, Content: "be brief"},
			{Role: RoleUser, Content: "say ok"},
		},
	})
	require.NoError(t, err)

	assert.Equal(t, "hello back", resp.Content)
	assert.Equal(t, "gpt-5.5", resp.Model)
	assert.Equal(t, 12, resp.InputTokens)
	assert.Equal(t, 4, resp.CachedInputTokens)
	assert.Equal(t, 3, resp.OutputTokens)

	// Request shape: system → instructions, user → input.
	assert.Equal(t, "be brief", gotReq.Instructions)
	require.Len(t, gotReq.Input, 1)
	assert.Equal(t, "message", gotReq.Input[0].Type)
	assert.Equal(t, "user", gotReq.Input[0].Role)
	require.Len(t, gotReq.Input[0].Content, 1)
	assert.Equal(t, "input_text", gotReq.Input[0].Content[0].Type)
	assert.Equal(t, "say ok", gotReq.Input[0].Content[0].Text)
	assert.True(t, gotReq.Stream)

	if assert.NotNil(t, gotReq.Reasoning) {
		assert.Equal(t, "high", gotReq.Reasoning.Effort)
	}

	// Auth headers carry the chatgpt access token + account id.
	assert.Equal(t, "Bearer access-1", gotHeaders.Get("Authorization"))
	assert.Equal(t, "acct-42", gotHeaders.Get("ChatGPT-Account-ID"))
}

func TestCodexProvider_RefreshOn401(t *testing.T) {
	t.Parallel()

	var refreshHits atomic.Int32

	refreshSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshHits.Add(1)

		var req map[string]string
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&req)) {
			return
		}

		assert.Equal(t, "refresh_token", req["grant_type"])
		assert.Equal(t, "refresh-1", req["refresh_token"])
		assert.Equal(t, codexChatGPTOAuthClientID, req["client_id"])

		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]string{
			"access_token":  "access-2",
			"refresh_token": "refresh-2",
			"id_token":      "ignored",
		}))
	}))
	defer refreshSrv.Close()

	var apiCalls atomic.Int32

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := apiCalls.Add(1)
		token := r.Header.Get("Authorization")

		if call == 1 {
			assert.Equal(t, "Bearer access-1", token)
			w.WriteHeader(http.StatusUnauthorized)
			_, err := w.Write([]byte(`{"error":"invalid_token"}`))
			assert.NoError(t, err)

			return
		}

		assert.Equal(t, "Bearer access-2", token)

		w.Header().Set("Content-Type", "text/event-stream")
		writeCodexSSE(t, w, codexFakeSuccess("ok-after-refresh", "gpt-5.5", 1, 0, 1))
	}))
	defer apiSrv.Close()

	authPath := writeCodexAuthFile(t, "access-1", "refresh-1", "acct-42")

	auth, err := loadCodexChatGPTAuth(filepath.Dir(authPath))
	require.NoError(t, err)

	auth.refreshURL = refreshSrv.URL
	auth.httpClient = refreshSrv.Client()

	p := &CodexProvider{
		client:  apiSrv.Client(),
		auth:    auth,
		baseURL: apiSrv.URL,
		models:  []string{"gpt-5.5"},
	}

	resp, err := p.Complete(context.Background(), CompleteParams{
		Model:    "gpt-5.5",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	require.NoError(t, err)

	assert.Equal(t, "ok-after-refresh", resp.Content)
	assert.EqualValues(t, 1, refreshHits.Load())
	assert.EqualValues(t, 2, apiCalls.Load())

	// Persisted access token now matches the refreshed value.
	persisted, err := os.ReadFile(authPath)
	require.NoError(t, err)

	var diskAuth codexAuth
	require.NoError(t, json.Unmarshal(persisted, &diskAuth))
	assert.Equal(t, "access-2", diskAuth.Tokens.AccessToken)
	assert.Equal(t, "refresh-2", diskAuth.Tokens.RefreshToken)
	assert.Equal(t, "acct-42", diskAuth.Tokens.AccountID)
}

func TestCodexProvider_RefreshFailureSurfaced(t *testing.T) {
	t.Parallel()

	refreshSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, err := w.Write([]byte(`{"error":{"code":"refresh_token_expired"}}`))
		assert.NoError(t, err)
	}))
	defer refreshSrv.Close()

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, err := w.Write([]byte(`{"error":"invalid_token"}`))
		assert.NoError(t, err)
	}))
	defer apiSrv.Close()

	authPath := writeCodexAuthFile(t, "access-1", "refresh-1", "acct-42")

	auth, err := loadCodexChatGPTAuth(filepath.Dir(authPath))
	require.NoError(t, err)

	auth.refreshURL = refreshSrv.URL
	auth.httpClient = refreshSrv.Client()

	p := &CodexProvider{client: apiSrv.Client(), auth: auth, baseURL: apiSrv.URL, models: []string{"gpt-5.5"}}

	_, err = p.Complete(context.Background(), CompleteParams{
		Model:    "gpt-5.5",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "refresh after 401")
}

func TestLoadCodexChatGPTAuth(t *testing.T) {
	t.Parallel()

	authPath := writeCodexAuthFile(t, "access-1", "refresh-1", "acct-42")

	auth, err := loadCodexChatGPTAuth(filepath.Dir(authPath))
	require.NoError(t, err)

	access, accountID := auth.snapshot()
	assert.Equal(t, "access-1", access)
	assert.Equal(t, "acct-42", accountID)
}

func TestLoadCodexChatGPTAuth_RejectsAPIKeyMode(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	authJSON := `{"auth_mode":"apikey","OPENAI_API_KEY":"sk-test","tokens":{"access_token":"","refresh_token":""}}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "auth.json"), []byte(authJSON), 0o600))

	_, err := loadCodexChatGPTAuth(dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "auth_mode")
}

func TestPersistRefreshedCodexAuth_AtomicAndPreservesUnknown(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	authPath := filepath.Join(dir, "auth.json")
	original := `{
		"auth_mode": "chatgpt",
		"OPENAI_API_KEY": null,
		"custom_field": "preserved",
		"tokens": {
			"id_token": "old-id",
			"access_token": "old-access",
			"refresh_token": "old-refresh",
			"account_id": "acct-9"
		},
		"last_refresh": "2025-01-01T00:00:00Z"
	}`
	require.NoError(t, os.WriteFile(authPath, []byte(original), 0o600))

	require.NoError(t, persistRefreshedCodexAuth(authPath, "new-access", "new-refresh", "new-id"))

	updated, err := os.ReadFile(authPath)
	require.NoError(t, err)

	var raw map[string]any
	require.NoError(t, json.Unmarshal(updated, &raw))

	assert.Equal(t, "preserved", raw["custom_field"])
	assert.NotEqual(t, "2025-01-01T00:00:00Z", raw["last_refresh"], "last_refresh should be updated")

	tokens, ok := raw["tokens"].(map[string]any)
	require.True(t, ok, "tokens field must be an object")
	assert.Equal(t, "new-access", tokens["access_token"])
	assert.Equal(t, "new-refresh", tokens["refresh_token"])
	assert.Equal(t, "new-id", tokens["id_token"])
	assert.Equal(t, "acct-9", tokens["account_id"], "account_id should be preserved")
}

func TestCodexConfiguredModel(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("CODEX_HOME", "")

	codexDir := filepath.Join(dir, ".codex")
	require.NoError(t, os.MkdirAll(codexDir, 0o750))

	require.NoError(t, os.WriteFile(filepath.Join(codexDir, "config.toml"), []byte(`
# comment
model = "gpt-test-codex"
`), 0o600))

	if got := codexConfiguredModel(); got != "gpt-test-codex" {
		require.Failf(t, "unexpected failure", "codexConfiguredModel = %q, want gpt-test-codex", got)
	}

	models := codexModels()
	if len(models) == 0 || models[0] != "gpt-test-codex" {
		require.Failf(t, "unexpected failure", "codexModels = %v, want configured model first", models)
	}
}

func TestCodexBuildInput_SkipsSystemAndPicksContentType(t *testing.T) {
	t.Parallel()

	got := codexBuildInput([]Message{
		{Role: RoleSystem, Content: "system rules"},
		{Role: RoleUser, Content: "hello"},
		{Role: RoleAssistant, Content: "hi"},
	})

	require.Len(t, got, 2)
	assert.Equal(t, "user", got[0].Role)
	assert.Equal(t, "input_text", got[0].Content[0].Type)
	assert.Equal(t, "assistant", got[1].Role)
	assert.Equal(t, "output_text", got[1].Content[0].Type)
}

func TestCodexBuildInput_ToolCallAndResultMessages(t *testing.T) {
	t.Parallel()

	got := codexBuildInput([]Message{
		{Role: RoleUser, Content: "run ls"},
		{
			Role:    RoleAssistant,
			Content: "",
			ToolCalls: []ToolCall{{
				ID:    "call-1",
				Name:  "bash",
				Input: map[string]any{"command": "ls"},
			}},
		},
		{
			Role: RoleTool,
			ToolResult: &ToolResult{
				ToolCallID: "call-1",
				Content:    "file1.go\nfile2.go",
			},
		},
		{Role: RoleAssistant, Content: "Here are the files."},
	})

	require.Len(t, got, 4)

	// 1) user message
	assert.Equal(t, "message", got[0].Type)
	assert.Equal(t, "user", got[0].Role)

	// 2) function_call
	assert.Equal(t, "function_call", got[1].Type)
	assert.Equal(t, "call-1", got[1].CallID)
	assert.Equal(t, "bash", got[1].Name)
	assert.Contains(t, got[1].Arguments, `"command"`)

	// 3) function_call_output
	assert.Equal(t, "function_call_output", got[2].Type)
	assert.Equal(t, "call-1", got[2].CallID)
	assert.Equal(t, "file1.go\nfile2.go", got[2].Output)

	// 4) final assistant message
	assert.Equal(t, "message", got[3].Type)
	assert.Equal(t, "assistant", got[3].Role)
}

func TestCodexBuildTools(t *testing.T) {
	t.Parallel()

	tools := codexBuildTools([]ToolDefinition{BashTool()})
	require.Len(t, tools, 1)
	assert.Equal(t, "function", tools[0].Type)
	assert.Equal(t, "bash", tools[0].Name)
	assert.NotEmpty(t, tools[0].Description)
	assert.NotNil(t, tools[0].Parameters)
}

func TestCodexBuildTools_NilReturnsNil(t *testing.T) {
	t.Parallel()

	assert.Nil(t, codexBuildTools(nil))
}

func TestCodexProvider_Complete_WithToolUse(t *testing.T) {
	t.Parallel()

	var gotReq codexResponsesRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if !assert.NoError(t, err) {
			return
		}

		if !assert.NoError(t, json.Unmarshal(body, &gotReq)) {
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		// Return a function_call response.
		events := []map[string]any{
			{
				"type": "response.output_item.done",
				"item": map[string]any{
					"type":      "function_call",
					"call_id":   "call-abc",
					"name":      "bash",
					"arguments": `{"command":"ls -la"}`,
				},
			},
			{
				"type": "response.completed",
				"response": map[string]any{
					"model": "gpt-5.5",
					"usage": map[string]any{
						"input_tokens":  10,
						"output_tokens": 5,
					},
				},
			},
		}
		writeCodexSSE(t, w, events)
	}))
	defer srv.Close()

	auth := newTestCodexAuth(t, "access-1", "refresh-1", "acct-42")
	p := &CodexProvider{
		client:  srv.Client(),
		auth:    auth,
		baseURL: srv.URL,
		models:  []string{"gpt-5.5"},
	}

	resp, err := p.Complete(context.Background(), CompleteParams{
		Model:    "gpt-5.5",
		Messages: []Message{{Role: RoleUser, Content: "list files"}},
		Tools:    DefaultTools(),
	})
	require.NoError(t, err)

	// Tools should be sent in the request.
	require.Len(t, gotReq.Tools, 1)
	assert.Equal(t, "function", gotReq.Tools[0].Type)
	assert.Equal(t, "bash", gotReq.Tools[0].Name)

	// Response should contain tool calls.
	assert.Equal(t, StopToolUse, resp.StopReason)
	require.Len(t, resp.ToolCalls, 1)
	assert.Equal(t, "call-abc", resp.ToolCalls[0].ID)
	assert.Equal(t, "bash", resp.ToolCalls[0].Name)
	assert.Equal(t, "ls -la", resp.ToolCalls[0].Input["command"])
}

func TestCodexExtractFunctionCall(t *testing.T) {
	t.Parallel()

	tc, ok := codexExtractFunctionCall(&codexEventItem{
		Type:      "function_call",
		CallID:    "call-99",
		Name:      "bash",
		Arguments: `{"command":"pwd"}`,
	})
	require.True(t, ok)
	assert.Equal(t, "call-99", tc.ID)
	assert.Equal(t, "bash", tc.Name)
	assert.Equal(t, "pwd", tc.Input["command"])
}

func TestCodexExtractFunctionCall_NilItem(t *testing.T) {
	t.Parallel()

	_, ok := codexExtractFunctionCall(nil)
	assert.False(t, ok)
}

func TestCodexExtractFunctionCall_NonFunctionCallType(t *testing.T) {
	t.Parallel()

	_, ok := codexExtractFunctionCall(&codexEventItem{Type: "message"})
	assert.False(t, ok)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func newTestCodexAuth(t *testing.T, access, refresh, accountID string) *codexChatGPTAuth {
	t.Helper()

	authPath := writeCodexAuthFile(t, access, refresh, accountID)

	auth, err := loadCodexChatGPTAuth(filepath.Dir(authPath))
	require.NoError(t, err)

	return auth
}

func writeCodexAuthFile(t *testing.T, access, refresh, accountID string) string {
	t.Helper()

	dir := t.TempDir()
	authPath := filepath.Join(dir, "auth.json")

	body := fmt.Sprintf(
		`{"auth_mode":"chatgpt","OPENAI_API_KEY":null,"tokens":{"access_token":%q,"refresh_token":%q,"account_id":%q}}`,
		access, refresh, accountID,
	)
	require.NoError(t, os.WriteFile(authPath, []byte(body), 0o600))

	return authPath
}

// codexFakeSuccess returns SSE event JSON values for a successful completion.
func codexFakeSuccess(text, model string, in, cached, out int) []map[string]any {
	return []map[string]any{
		{"type": "response.output_text.delta", "delta": text[:1]},
		{"type": "response.output_text.delta", "delta": text[1:]},
		{
			"type": "response.output_item.done",
			"item": map[string]any{
				"type": "message",
				"role": "assistant",
				"content": []map[string]any{
					{"type": "output_text", "text": text},
				},
			},
		},
		{
			"type": "response.completed",
			"response": map[string]any{
				"model": model,
				"usage": map[string]any{
					"input_tokens":         in,
					"output_tokens":        out,
					"input_tokens_details": map[string]any{"cached_tokens": cached},
				},
			},
		},
	}
}

func writeCodexSSE(t *testing.T, w http.ResponseWriter, events []map[string]any) {
	t.Helper()

	flusher, hasFlusher := w.(http.Flusher)

	for _, ev := range events {
		payload, err := json.Marshal(ev)
		require.NoError(t, err)

		_, err = w.Write([]byte("event: " + asString(ev["type"]) + "\n"))
		require.NoError(t, err)
		_, err = w.Write([]byte("data: "))
		require.NoError(t, err)
		_, err = w.Write(payload)
		require.NoError(t, err)
		_, err = w.Write([]byte("\n\n"))
		require.NoError(t, err)

		if hasFlusher {
			flusher.Flush()
		}
	}
}

func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}

	return ""
}

func init() {
	// Ensure import compatibility for strings.* used elsewhere in tests.
	_ = strings.EqualFold
}
