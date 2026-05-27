package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOllamaProvider_Complete(t *testing.T) {
	t.Parallel()

	var (
		gotReq         ollamaChatRequest
		gotPath        string
		gotContentType string
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotContentType = r.Header.Get("Content-Type")

		body, err := io.ReadAll(r.Body)
		if !assert.NoError(t, err) {
			return
		}

		if !assert.NoError(t, json.Unmarshal(body, &gotReq)) {
			return
		}

		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(ollamaChatResponse{
			Model:           gotReq.Model,
			Message:         ollamaMessage{Role: "assistant", Content: "hello from ollama"},
			PromptEvalCount: 7,
			EvalCount:       4,
		}))
	}))
	defer srv.Close()

	p := &OllamaProvider{baseURL: srv.URL, client: srv.Client()}
	temperature := 0.2
	topP := 0.9
	seed := 42

	resp, err := p.Complete(context.Background(), CompleteParams{
		Model:          "llama3.2",
		MaxTokens:      128,
		Temperature:    &temperature,
		TopP:           &topP,
		Seed:           &seed,
		Stop:           []string{"</stop>"},
		ReasoningLevel: "xhigh",
		Messages: []Message{
			{Role: RoleSystem, Content: "be brief"},
			{Role: RoleUser, Content: "hi"},
		},
	})
	require.NoError(t, err)

	assert.Equal(t, "/api/chat", gotPath)
	assert.Equal(t, "application/json", gotContentType)
	assert.False(t, gotReq.Stream)
	assert.Equal(t, "llama3.2", gotReq.Model)
	require.Len(t, gotReq.Messages, 2)
	assert.Equal(t, "system", gotReq.Messages[0].Role)
	assert.Equal(t, "user", gotReq.Messages[1].Role)
	assert.Equal(t, 128, gotReq.Options.NumPredict)
	require.NotNil(t, gotReq.Options.Temperature)
	assert.InEpsilon(t, 0.2, *gotReq.Options.Temperature, 0.0001)
	require.NotNil(t, gotReq.Options.TopP)
	assert.InEpsilon(t, 0.9, *gotReq.Options.TopP, 0.0001)
	require.NotNil(t, gotReq.Options.Seed)
	assert.Equal(t, 42, *gotReq.Options.Seed)
	assert.Equal(t, []string{"</stop>"}, gotReq.Options.Stop)
	assert.Equal(t, "high", gotReq.Think)

	assert.Equal(t, "hello from ollama", resp.Content)
	assert.Equal(t, "llama3.2", resp.Model)
	assert.Equal(t, StopEndTurn, resp.StopReason)
	assert.Equal(t, 7, resp.InputTokens)
	assert.Equal(t, 4, resp.OutputTokens)
}

func TestOllamaProvider_CompleteStream_Success(t *testing.T) {
	t.Parallel()

	var gotReq ollamaChatRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if !assert.NoError(t, err) {
			return
		}

		if !assert.NoError(t, json.Unmarshal(body, &gotReq)) {
			return
		}

		w.Header().Set("Content-Type", "application/x-ndjson")
		writeOllamaStream(t, w,
			ollamaChatResponse{Model: gotReq.Model, Message: ollamaMessage{Content: "hello "}},
			ollamaChatResponse{Model: gotReq.Model, Message: ollamaMessage{Content: "stream"}},
			ollamaChatResponse{
				Model:           gotReq.Model,
				PromptEvalCount: 7,
				EvalCount:       2,
				Done:            true,
			},
		)
	}))
	defer srv.Close()

	p := &OllamaProvider{baseURL: srv.URL, client: srv.Client()}

	ch, err := p.CompleteStream(context.Background(), CompleteParams{
		Model:    "llama3.2",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	require.NoError(t, err)

	resp, err := CollectStream(ch)
	require.NoError(t, err)
	assert.True(t, gotReq.Stream)
	assert.Equal(t, "hello stream", resp.Content)
	assert.Equal(t, "llama3.2", resp.Model)
	assert.Equal(t, StopEndTurn, resp.StopReason)
	assert.Equal(t, 7, resp.InputTokens)
	assert.Equal(t, 2, resp.OutputTokens)
}

func TestOllamaProvider_CompleteStream_ToolUseStopReason(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		writeOllamaStream(t, w,
			ollamaChatResponse{
				Model: "llama3.2",
				Message: ollamaMessage{
					ToolCalls: []ollamaToolCall{{
						Function: ollamaToolCallFunction{
							Name:      "bash",
							Arguments: map[string]any{"command": "pwd"},
						},
					}},
				},
				PromptEvalCount: 4,
				EvalCount:       1,
				Done:            true,
			},
		)
	}))
	defer srv.Close()

	p := &OllamaProvider{baseURL: srv.URL, client: srv.Client()}

	ch, err := p.CompleteStream(context.Background(), CompleteParams{
		Model:    "llama3.2",
		Messages: []Message{{Role: RoleUser, Content: "run pwd"}},
		Tools:    DefaultTools(),
	})
	require.NoError(t, err)

	resp, err := CollectStream(ch)
	require.NoError(t, err)
	assert.Equal(t, "llama3.2", resp.Model)
	assert.Equal(t, StopToolUse, resp.StopReason)
	require.Len(t, resp.ToolCalls, 1)
	assert.Equal(t, "bash", resp.ToolCalls[0].Name)
	assert.Equal(t, "pwd", resp.ToolCalls[0].Input["command"])
	assert.Equal(t, 4, resp.InputTokens)
	assert.Equal(t, 1, resp.OutputTokens)
}

func TestOllamaProvider_CompleteStream_MidStreamErrorReturnsPartial(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		writeOllamaStream(t, w,
			ollamaChatResponse{Model: "llama3.2", Message: ollamaMessage{Content: "partial "}},
			ollamaChatResponse{Error: "provider failed"},
		)
	}))
	defer srv.Close()

	p := &OllamaProvider{baseURL: srv.URL, client: srv.Client()}

	ch, err := p.CompleteStream(context.Background(), CompleteParams{
		Model:    "llama3.2",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	require.NoError(t, err)

	resp, err := CollectStream(ch)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "provider failed")
	assert.Equal(t, "partial ", resp.Content)
}

func TestOllamaProvider_CompleteStream_MissingFinalChunkIsIncomplete(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		writeOllamaStream(t, w,
			ollamaChatResponse{Model: "llama3.2", Message: ollamaMessage{Content: "partial"}},
		)
	}))
	defer srv.Close()

	p := &OllamaProvider{baseURL: srv.URL, client: srv.Client()}

	ch, err := p.CompleteStream(context.Background(), CompleteParams{
		Model:    "llama3.2",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	require.NoError(t, err)

	resp, err := CollectStream(ch)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrStreamIncomplete)
	assert.Equal(t, "partial", resp.Content)
}

func TestOllamaStream_CancelUnblocksBackpressuredTerminalError(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := make(chan Chunk, DefaultStreamBuffer)
	done := make(chan struct{})
	payload := ollamaStreamString(t,
		ollamaChatResponse{Model: "llama3.2", Message: ollamaMessage{Content: "one"}},
		ollamaChatResponse{Model: "llama3.2", Message: ollamaMessage{Content: "two"}},
		ollamaChatResponse{Error: "provider failed"},
	)

	go func() {
		defer close(done)

		streamOllamaChatResponse(ctx, strings.NewReader(payload), ch, "llama3.2")
	}()

	waitForBufferedChunks(t, ch, DefaultStreamBuffer)
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		require.Fail(t, "ollama stream goroutine stayed blocked on terminal error after cancellation")
	}
}

func TestOllamaProvider_FetchModels(t *testing.T) {
	t.Parallel()

	var gotPath string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path

		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(`{"models":[{"name":"llama3.2:latest"},{"name":"qwen2.5:7b"}]}`))
		assert.NoError(t, err)
	}))
	defer srv.Close()

	p := &OllamaProvider{baseURL: srv.URL, client: srv.Client()}
	models, err := p.FetchModels(context.Background())
	require.NoError(t, err)

	assert.Equal(t, "/api/tags", gotPath)
	assert.Equal(t, []string{"llama3.2:latest", "qwen2.5:7b"}, models)
}

func TestOllamaProvider_HTTPErrorIsTyped(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, err := w.Write([]byte(`{"error":"daemon busy"}`))
		assert.NoError(t, err)
	}))
	defer srv.Close()

	p := &OllamaProvider{baseURL: srv.URL, client: srv.Client()}

	_, err := p.Complete(context.Background(), CompleteParams{
		Model:    "llama3.2",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	require.Error(t, err)

	var providerErr *ProviderError
	require.ErrorAs(t, err, &providerErr)
	assert.Equal(t, providerOllama, providerErr.Provider)
	assert.Equal(t, http.StatusServiceUnavailable, providerErr.StatusCode)
	assert.Equal(t, RetryabilityRetryable, providerErr.Retryability)
	assert.Equal(t, "daemon busy", providerErr.Message)
}

func TestOllamaProvider_FetchModelsHTTPErrorIsTyped(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("cf-ray", "cf-ollama-models")
		w.WriteHeader(http.StatusBadGateway)
		_, err := w.Write([]byte(`{"error":"tag list unavailable"}`))
		assert.NoError(t, err)
	}))
	defer srv.Close()

	p := &OllamaProvider{baseURL: srv.URL, client: srv.Client()}

	_, err := p.FetchModels(context.Background())
	require.Error(t, err)

	var providerErr *ProviderError
	require.ErrorAs(t, err, &providerErr)
	assert.Equal(t, providerOllama, providerErr.Provider)
	assert.Equal(t, http.StatusBadGateway, providerErr.StatusCode)
	assert.Equal(t, "cf-ollama-models", providerErr.RequestID)
	assert.Equal(t, RetryabilityRetryable, providerErr.Retryability)
	assert.Equal(t, "tag list unavailable", providerErr.Message)
}

func TestOllamaProvider_ErrorPayloadIsTyped(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "8")
		w.Header().Set("cf-ray", "cf-ollama-payload")
		w.Header().Set("Content-Type", "application/json")

		assert.NoError(t, json.NewEncoder(w).Encode(ollamaChatResponse{
			Error: "service unavailable: daemon overloaded",
		}))
	}))
	defer srv.Close()

	p := &OllamaProvider{baseURL: srv.URL, client: srv.Client()}

	_, err := p.Complete(context.Background(), CompleteParams{
		Model:    "llama3.2",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	require.Error(t, err)

	var providerErr *ProviderError
	require.ErrorAs(t, err, &providerErr)
	assert.Equal(t, providerOllama, providerErr.Provider)
	assert.Equal(t, http.StatusOK, providerErr.StatusCode)
	assert.Equal(t, 8*time.Second, providerErr.RetryAfter)
	assert.Equal(t, "cf-ollama-payload", providerErr.RequestID)
	assert.Equal(t, RetryabilityRetryable, providerErr.Retryability)
	assert.Equal(t, "service unavailable: daemon overloaded", providerErr.Message)
}

func TestOllamaProvider_ConfigBaseURL(t *testing.T) {
	t.Setenv("OLLAMA_BASE_URL", "")

	p, err := NewOllamaProviderWithConfigContext(context.Background(), ProviderConfig{BaseURL: "http://ollama.config/"})
	require.NoError(t, err)
	assert.Equal(t, "http://ollama.config", p.baseURL)
}

func TestOllamaProvider_EnvBaseURLOverridesConfig(t *testing.T) {
	t.Setenv("OLLAMA_BASE_URL", "http://ollama.env/")

	p, err := NewOllamaProviderWithConfigContext(context.Background(), ProviderConfig{BaseURL: "http://ollama.config"})
	require.NoError(t, err)
	assert.Equal(t, "http://ollama.env", p.baseURL)
}

func TestOllamaProvider_ConfiguredBaseURLDoesNotRequireReachability(t *testing.T) {
	t.Setenv("OLLAMA_BASE_URL", "")

	p, err := NewOllamaProviderWithConfigContext(context.Background(), ProviderConfig{BaseURL: "http://ollama.config"})
	require.NoError(t, err)
	assert.Equal(t, "http://ollama.config", p.baseURL)
}

func TestOllamaProvider_AutoStartStartsDaemonWhenUnavailable(t *testing.T) {
	t.Setenv("OLLAMA_BASE_URL", "")
	t.Setenv(envOllamaAutoStart, "true")

	baseURL := unusedLocalOllamaURL(t)

	var (
		calls int
		srv   *http.Server
	)

	withOllamaServeStarter(t, func(ctx context.Context, req ollamaStartRequest) (*ollamaDaemonStart, error) {
		calls++

		assert.Equal(t, baseURL, req.BaseURL)
		assert.Equal(t, "env."+envOllamaAutoStart, req.PolicySource)

		var err error

		srv, err = startOllamaTagsServer(ctx, t, req.BaseURL)

		return &ollamaDaemonStart{ownership: OllamaDaemonOwnership{BaseURL: req.BaseURL}}, err
	})
	t.Cleanup(func() {
		if srv != nil {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()

			assert.NoError(t, srv.Shutdown(ctx))
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	p, err := NewOllamaProviderWithConfigContext(ctx, ProviderConfig{BaseURL: baseURL, AutoStart: true})
	require.NoError(t, err)
	assert.Equal(t, baseURL, p.baseURL)
	assert.True(t, p.startAttempted)
	assert.Equal(t, 1, calls)
}

func TestOllamaProvider_AutoStartDoesNotStartWhenReachable(t *testing.T) {
	t.Setenv("OLLAMA_BASE_URL", "")
	t.Setenv(envOllamaAutoStart, "true")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/tags", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(`{"models":[]}`))
		assert.NoError(t, err)
	}))
	defer srv.Close()

	var calls int

	withOllamaServeStarter(t, func(context.Context, ollamaStartRequest) (*ollamaDaemonStart, error) {
		calls++
		return nil, errors.New("unexpected starter call")
	})

	p, err := NewOllamaProviderWithConfigContext(context.Background(), ProviderConfig{BaseURL: srv.URL, AutoStart: true})
	require.NoError(t, err)
	assert.Equal(t, srv.URL, p.baseURL)
	assert.False(t, p.startAttempted)
	assert.Equal(t, 0, calls)
}

func TestOllamaProvider_AutoStartReturnsStarterError(t *testing.T) {
	t.Setenv("OLLAMA_BASE_URL", "")
	t.Setenv(envOllamaAutoStart, "true")

	baseURL := unusedLocalOllamaURL(t)
	withOllamaServeStarter(t, func(context.Context, ollamaStartRequest) (*ollamaDaemonStart, error) {
		return nil, errors.New("ollama: start daemon: binary not found")
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := NewOllamaProviderWithConfigContext(ctx, ProviderConfig{BaseURL: baseURL, AutoStart: true})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ollama: start daemon: binary not found")
}

func TestOllamaProvider_AutoStartSkipsRemoteAndDisabled(t *testing.T) {
	t.Setenv("OLLAMA_BASE_URL", "")
	t.Setenv(envOllamaAutoStart, "false")

	var calls int

	withOllamaServeStarter(t, func(context.Context, ollamaStartRequest) (*ollamaDaemonStart, error) {
		calls++
		return nil, errors.New("unexpected starter call")
	})

	disabled, err := NewOllamaProviderWithConfigContext(context.Background(), ProviderConfig{BaseURL: "http://127.0.0.1:1", AutoStart: true})
	require.NoError(t, err)
	assert.Equal(t, "http://127.0.0.1:1", disabled.baseURL)

	t.Setenv(envOllamaAutoStart, "true")

	remote, err := NewOllamaProviderWithConfigContext(context.Background(), ProviderConfig{BaseURL: "http://ollama.example", AutoStart: true})
	require.NoError(t, err)
	assert.Equal(t, "http://ollama.example", remote.baseURL)
	assert.Equal(t, 0, calls)
}

func TestIsLocalOllamaBaseURLRecognizesLoopbackAddresses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		baseURL string
		want    bool
	}{
		{name: "localhost", baseURL: "http://localhost:11434", want: true},
		{name: "localhost trailing dot", baseURL: "http://localhost.:11434", want: true},
		{name: "ipv4 loopback range", baseURL: "http://127.0.0.2:11434", want: true},
		{name: "ipv6 loopback", baseURL: "http://[::1]:11434", want: true},
		{name: "unspecified bind address is not a client-local endpoint", baseURL: "http://0.0.0.0:11434", want: false},
		{name: "remote", baseURL: "https://ollama.example", want: false},
		{name: "invalid", baseURL: "://bad-url", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, isLocalOllamaBaseURL(tt.baseURL))
		})
	}
}

func TestOllamaProvider_AutoStartWaitErrorIncludesStartupLogs(t *testing.T) {
	t.Setenv("OLLAMA_BASE_URL", "")
	t.Setenv(envOllamaAutoStart, "true")

	baseURL := unusedLocalOllamaURL(t)
	logs := newBoundedLogBuffer(1024)
	_, err := logs.Write([]byte("listen tcp 127.0.0.1:11434: bind: address already in use"))
	require.NoError(t, err)

	withOllamaServeStarter(t, func(context.Context, ollamaStartRequest) (*ollamaDaemonStart, error) {
		return &ollamaDaemonStart{
			ownership:     OllamaDaemonOwnership{PID: 4242, BaseURL: baseURL, LogPath: filepath.Join(t.TempDir(), "ollama.log")},
			ownershipPath: filepath.Join(t.TempDir(), "ollama-daemon.json"),
			logs:          logs,
		}, nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err = NewOllamaProviderWithConfigContext(ctx, ProviderConfig{BaseURL: baseURL, AutoStart: true})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "startup log")
	assert.Contains(t, err.Error(), "address already in use")
	assert.Contains(t, err.Error(), "Atteler started Ollama PID 4242")
	assert.Contains(t, err.Error(), "atteler --ollama-stop")
}

func TestCappedLogFileWriterBoundsPersistedStartupLogs(t *testing.T) {
	t.Parallel()

	logPath := filepath.Join(t.TempDir(), "ollama-startup.log")
	logFile, err := os.Create(logPath)
	require.NoError(t, err)

	writer := newCappedLogFileWriter(logFile, 8)
	n, err := writer.Write([]byte("1234567890abcdef"))
	require.NoError(t, err)
	assert.Equal(t, 16, n)

	require.NoError(t, logFile.Close())

	data, err := os.ReadFile(logPath)
	require.NoError(t, err)

	logs := string(data)
	assert.Contains(t, logs, "12345678")
	assert.NotContains(t, logs, "90abcdef")
	assert.Contains(t, logs, "startup log truncated")
}

func TestCappedLogFileWriterMarksTruncationAfterLimitReached(t *testing.T) {
	t.Parallel()

	logPath := filepath.Join(t.TempDir(), "ollama-startup.log")
	logFile, err := os.Create(logPath)
	require.NoError(t, err)

	writer := newCappedLogFileWriter(logFile, 4)
	n, err := writer.Write([]byte("1234"))
	require.NoError(t, err)
	assert.Equal(t, 4, n)

	n, err = writer.Write([]byte("5678"))
	require.NoError(t, err)
	assert.Equal(t, 4, n)

	require.NoError(t, logFile.Close())

	data, err := os.ReadFile(logPath)
	require.NoError(t, err)

	logs := string(data)
	assert.Contains(t, logs, "1234")
	assert.NotContains(t, logs, "5678")
	assert.Contains(t, logs, "startup log truncated")
}

func TestCheckOllamaStatus_DistinguishesLocalRemoteMisconfiguredAndOwned(t *testing.T) {
	t.Setenv("OLLAMA_BASE_URL", "")
	unsetOllamaAutoStartEnvForTest(t)

	ownershipPath := filepath.Join(t.TempDir(), "ollama-daemon.json")
	t.Setenv(envOllamaOwnershipPath, ownershipPath)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/tags", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(`{"models":[]}`))
		assert.NoError(t, err)
	}))
	defer srv.Close()

	alreadyRunning := CheckOllamaStatus(context.Background(), ProviderConfig{BaseURL: srv.URL})
	assert.Equal(t, OllamaStatusAlreadyRunning, alreadyRunning.State)
	assert.True(t, alreadyRunning.Local)

	ownership := OllamaDaemonOwnership{
		Owner:     "atteler",
		PID:       os.Getpid(),
		Command:   []string{"ollama", "serve"},
		StartedAt: time.Now().UTC(),
		BaseURL:   srv.URL,
		SessionID: "session-123",
		LogPath:   filepath.Join(t.TempDir(), "ollama.log"),
	}
	require.NoError(t, recordOllamaOwnership(ownershipPath, ownership))

	withOllamaProcessMatchHook(t, func(*OllamaDaemonOwnership) bool { return true })

	owned := CheckOllamaStatus(context.Background(), ProviderConfig{BaseURL: srv.URL})
	assert.Equal(t, OllamaStatusStartedByAtteler, owned.State)
	require.NotNil(t, owned.Ownership)
	assert.Equal(t, "session-123", owned.Ownership.SessionID)
	assert.Equal(t, "owned-running", owned.OwnershipStatus)

	remote := CheckOllamaStatus(context.Background(), ProviderConfig{BaseURL: "https://ollama.example"})
	assert.Equal(t, OllamaStatusRemote, remote.State)
	assert.False(t, remote.Local)

	unavailableURL := unusedLocalOllamaURL(t)
	unavailable := CheckOllamaStatus(context.Background(), ProviderConfig{BaseURL: unavailableURL})
	assert.Equal(t, OllamaStatusUnavailable, unavailable.State)
	assert.True(t, unavailable.Local)

	misconfigured := CheckOllamaStatus(context.Background(), ProviderConfig{BaseURL: "not-a-url"})
	assert.Equal(t, OllamaStatusMisconfigured, misconfigured.State)
	assert.Contains(t, misconfigured.Error, "scheme")
}

func TestCheckOllamaStatus_DoesNotTrustNonAttelerOwnershipRecord(t *testing.T) {
	t.Setenv("OLLAMA_BASE_URL", "")
	unsetOllamaAutoStartEnvForTest(t)

	ownershipPath := filepath.Join(t.TempDir(), "ollama-daemon.json")
	t.Setenv(envOllamaOwnershipPath, ownershipPath)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/tags", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(`{"models":[]}`))
		assert.NoError(t, err)
	}))
	defer srv.Close()

	require.NoError(t, recordOllamaOwnership(ownershipPath, OllamaDaemonOwnership{
		Owner:     "other-tool",
		PID:       os.Getpid(),
		Command:   []string{"ollama", "serve"},
		StartedAt: time.Now().UTC(),
		BaseURL:   srv.URL,
	}))

	status := CheckOllamaStatus(context.Background(), ProviderConfig{BaseURL: srv.URL})
	assert.Equal(t, OllamaStatusAlreadyRunning, status.State)
	assert.Equal(t, "recorded-untrusted-owner", status.OwnershipStatus)

	require.NoError(t, recordOllamaOwnership(ownershipPath, OllamaDaemonOwnership{
		Owner:     "atteler",
		PID:       os.Getpid(),
		Command:   []string{"sleep", "1000"},
		StartedAt: time.Now().UTC(),
		BaseURL:   srv.URL,
	}))

	status = CheckOllamaStatus(context.Background(), ProviderConfig{BaseURL: srv.URL})
	assert.Equal(t, OllamaStatusAlreadyRunning, status.State)
	assert.Equal(t, "recorded-invalid-command", status.OwnershipStatus)
}

func TestOllamaOwnershipMetadataRoundTripAndStopCleanup(t *testing.T) {
	ownershipPath := filepath.Join(t.TempDir(), "ollama-daemon.json")
	t.Setenv(envOllamaOwnershipPath, ownershipPath)

	ownership := OllamaDaemonOwnership{
		Owner:           "atteler",
		PID:             4242,
		AttelerPID:      100,
		ParentPID:       99,
		Command:         []string{"ollama", "serve"},
		Environment:     map[string]string{"OLLAMA_HOST": "127.0.0.1:11434"},
		StartedAt:       time.Now().UTC(),
		BaseURL:         defaultOllamaBase,
		SessionID:       "session-123",
		AttelerCommand:  []string{"atteler", "chat", "once"},
		AutoStartSource: "config.providers.ollama.auto_start",
		LogPath:         filepath.Join(t.TempDir(), "ollama.log"),
	}
	require.NoError(t, recordOllamaOwnership(ownershipPath, ownership))

	got, err := readOllamaOwnership(ownershipPath)
	require.NoError(t, err)
	assert.Equal(t, ownership.PID, got.PID)
	assert.Equal(t, ownership.Command, got.Command)
	assert.Equal(t, ownership.Environment, got.Environment)
	assert.Equal(t, ownership.BaseURL, got.BaseURL)
	assert.Equal(t, ownership.SessionID, got.SessionID)
	assert.Equal(t, ownership.AttelerCommand, got.AttelerCommand)
	assert.Equal(t, ownership.AutoStartSource, got.AutoStartSource)

	alive := true
	terminated := false

	withOllamaProcessHooks(t,
		func(int) bool { return alive },
		func(int) error {
			terminated = true
			alive = false

			return nil
		},
		func(int) error {
			alive = false

			return nil
		},
	)

	result, err := StopOwnedOllamaDaemon(context.Background(), ownershipPath)
	require.NoError(t, err)
	assert.True(t, result.Stopped)
	assert.True(t, result.Cleaned)
	assert.True(t, terminated)
	assert.NoFileExists(t, ownershipPath)
}

func TestStopOwnedOllamaDaemonRejectsUnexpectedOwnershipCommand(t *testing.T) { //nolint:paralleltest // Mutates global Ollama process hooks.
	ownershipPath := filepath.Join(t.TempDir(), "ollama-daemon.json")
	ownership := OllamaDaemonOwnership{
		Owner:     "atteler",
		PID:       4242,
		Command:   []string{"sleep", "1000"},
		StartedAt: time.Now().UTC(),
		BaseURL:   defaultOllamaBase,
	}
	require.NoError(t, recordOllamaOwnership(ownershipPath, ownership))

	withOllamaProcessHooks(t,
		func(int) bool { return true },
		func(int) error {
			require.FailNow(t, "unexpected terminate call")

			return nil
		},
		func(int) error {
			require.FailNow(t, "unexpected kill call")

			return nil
		},
	)

	result, err := StopOwnedOllamaDaemon(context.Background(), ownershipPath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not ollama serve")
	assert.False(t, result.Stopped)
	assert.False(t, result.Cleaned)
	assert.FileExists(t, ownershipPath)
}

func TestStopOwnedOllamaDaemonRejectsMissingOwner(t *testing.T) { //nolint:paralleltest // Mutates global Ollama process hooks.
	ownershipPath := filepath.Join(t.TempDir(), "ollama-daemon.json")
	ownership := OllamaDaemonOwnership{
		PID:       4242,
		Command:   []string{"ollama", "serve"},
		StartedAt: time.Now().UTC(),
		BaseURL:   defaultOllamaBase,
	}
	require.NoError(t, recordOllamaOwnership(ownershipPath, ownership))

	withOllamaProcessHooks(t,
		func(int) bool { return true },
		func(int) error {
			require.FailNow(t, "unexpected terminate call")

			return nil
		},
		func(int) error {
			require.FailNow(t, "unexpected kill call")

			return nil
		},
	)

	assert.Equal(t, "recorded-untrusted-owner", ollamaOwnershipStatus(defaultOllamaBase, &ownership))

	result, err := StopOwnedOllamaDaemon(context.Background(), ownershipPath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not atteler")
	assert.False(t, result.Stopped)
	assert.False(t, result.Cleaned)
	assert.FileExists(t, ownershipPath)
}

func TestStopOwnedOllamaDaemonCleansStaleMalformedRecord(t *testing.T) { //nolint:paralleltest // Mutates global Ollama process hooks.
	ownershipPath := filepath.Join(t.TempDir(), "ollama-daemon.json")
	ownership := OllamaDaemonOwnership{
		Owner:     "atteler",
		PID:       4242,
		Command:   []string{"not-ollama"},
		StartedAt: time.Now().UTC(),
		BaseURL:   defaultOllamaBase,
	}
	require.NoError(t, recordOllamaOwnership(ownershipPath, ownership))

	withOllamaProcessHooks(t,
		func(int) bool { return false },
		func(int) error {
			require.FailNow(t, "unexpected terminate call")

			return nil
		},
		func(int) error {
			require.FailNow(t, "unexpected kill call")

			return nil
		},
	)

	result, err := StopOwnedOllamaDaemon(context.Background(), ownershipPath)
	require.NoError(t, err)
	assert.False(t, result.Stopped)
	assert.True(t, result.Cleaned)
	assert.Contains(t, result.Message, "removed stale")
	assert.NoFileExists(t, ownershipPath)
}

func TestStopOwnedOllamaDaemonRefusesPIDMismatch(t *testing.T) { //nolint:paralleltest // Mutates global Ollama process hooks.
	ownershipPath := filepath.Join(t.TempDir(), "ollama-daemon.json")
	ownership := OllamaDaemonOwnership{
		Owner:     "atteler",
		PID:       4242,
		Command:   []string{"ollama", "serve"},
		StartedAt: time.Now().UTC(),
		BaseURL:   defaultOllamaBase,
	}
	require.NoError(t, recordOllamaOwnership(ownershipPath, ownership))

	withOllamaProcessHooks(t,
		func(int) bool { return true },
		func(int) error {
			require.FailNow(t, "unexpected terminate call")

			return nil
		},
		func(int) error {
			require.FailNow(t, "unexpected kill call")

			return nil
		},
	)
	withOllamaProcessMatchHook(t, func(*OllamaDaemonOwnership) bool { return false })

	assert.Equal(t, "owned-pid-mismatch", ollamaOwnershipStatus(defaultOllamaBase, &ownership))

	result, err := StopOwnedOllamaDaemon(context.Background(), ownershipPath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no longer matches")
	assert.False(t, result.Stopped)
	assert.False(t, result.Cleaned)
	assert.FileExists(t, ownershipPath)
}

func TestAutoRegisterWithConfigContext_RegistersConfiguredOllama(t *testing.T) {
	t.Setenv("OLLAMA_BASE_URL", "")

	maxAttempts := 4
	initialBackoffMS := 25
	maxBackoffMS := 250
	maxElapsedMS := 500
	jitterFraction := 0.3

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/tags", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(`{"models":[{"name":"llama3.2"}]}`))
		assert.NoError(t, err)
	}))
	defer srv.Close()

	r := AutoRegisterWithConfigContext(context.Background(), AutoRegisterConfig{
		Providers: map[string]ProviderConfig{
			providerAnthropic:  {Disabled: true},
			providerOpenAI:     {Disabled: true},
			providerClaudeCode: {Disabled: true},
			providerCodex:      {Disabled: true},
			providerOllama: {
				BaseURL: srv.URL,
				Retry: RetryPolicyConfig{
					MaxAttempts:      &maxAttempts,
					InitialBackoffMS: &initialBackoffMS,
					MaxBackoffMS:     &maxBackoffMS,
					MaxElapsedMS:     &maxElapsedMS,
					JitterFraction:   &jitterFraction,
				},
			},
		},
		DefaultProvider: providerOllama,
		DefaultModel:    "llama3.2",
	})

	p, ok := r.Provider(providerOllama)
	require.True(t, ok)
	assert.Equal(t, providerOllama, p.Name())

	providerName, ok := r.ProviderForModel("llama3.2")
	require.True(t, ok)
	assert.Equal(t, providerOllama, providerName)

	retry := r.RetryPolicyForProvider(providerOllama)
	assert.Equal(t, maxAttempts, retry.MaxAttempts)
	assert.Equal(t, time.Duration(initialBackoffMS)*time.Millisecond, retry.InitialBackoff)
	assert.Equal(t, time.Duration(maxBackoffMS)*time.Millisecond, retry.MaxBackoff)
	assert.Equal(t, time.Duration(maxElapsedMS)*time.Millisecond, retry.MaxElapsedTime)
	assert.InEpsilon(t, jitterFraction, retry.JitterFraction, 0.0001)
}

func TestAutoRegisterWithConfigContext_StartsLocalOllamaForDefaultProvider(t *testing.T) {
	t.Setenv("OLLAMA_BASE_URL", "")
	unsetOllamaAutoStartEnvForTest(t)

	baseURL := unusedLocalOllamaURL(t)

	var srv *http.Server

	withOllamaServeStarter(t, func(ctx context.Context, req ollamaStartRequest) (*ollamaDaemonStart, error) {
		assert.Equal(t, baseURL, req.BaseURL)
		assert.Equal(t, "config.providers.ollama.auto_start", req.PolicySource)

		var err error

		srv, err = startOllamaTagsServer(ctx, t, req.BaseURL)

		return &ollamaDaemonStart{ownership: OllamaDaemonOwnership{BaseURL: req.BaseURL}}, err
	})
	t.Cleanup(func() {
		if srv != nil {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()

			assert.NoError(t, srv.Shutdown(ctx))
		}
	})

	r := AutoRegisterWithConfigContext(context.Background(), AutoRegisterConfig{
		Providers: map[string]ProviderConfig{
			providerAnthropic:  {Disabled: true},
			providerOpenAI:     {Disabled: true},
			providerClaudeCode: {Disabled: true},
			providerCodex:      {Disabled: true},
			providerOllama:     {BaseURL: baseURL, AutoStart: true},
		},
		DefaultProvider: providerOllama,
		DefaultModel:    "llama3.2",
	})

	p, ok := r.Provider(providerOllama)
	require.True(t, ok)
	assert.Equal(t, providerOllama, p.Name())
}

func TestAutoRegisterWithConfigContext_DoesNotAutoStartWithoutPolicyOptIn(t *testing.T) {
	t.Setenv("OLLAMA_BASE_URL", "")
	unsetOllamaAutoStartEnvForTest(t)

	baseURL := unusedLocalOllamaURL(t)

	var calls int

	withOllamaServeStarter(t, func(context.Context, ollamaStartRequest) (*ollamaDaemonStart, error) {
		calls++
		return nil, errors.New("unexpected starter call")
	})

	r := AutoRegisterWithConfigContext(context.Background(), AutoRegisterConfig{
		Providers: map[string]ProviderConfig{
			providerAnthropic:  {Disabled: true},
			providerOpenAI:     {Disabled: true},
			providerClaudeCode: {Disabled: true},
			providerCodex:      {Disabled: true},
			providerOllama:     {BaseURL: baseURL},
		},
		DefaultProvider: providerOllama,
		DefaultModel:    "llama3.2",
	})

	_, ok := r.Provider(providerOllama)
	require.True(t, ok)
	assert.Equal(t, 0, calls)
}

func TestAutoRegisterWithConfigContext_DoesNotAutoStartWhenOptedInButNotSelected(t *testing.T) {
	t.Setenv("OLLAMA_BASE_URL", "")
	t.Setenv(envOllamaAutoStart, "true")

	baseURL := unusedLocalOllamaURL(t)

	var calls int

	withOllamaServeStarter(t, func(context.Context, ollamaStartRequest) (*ollamaDaemonStart, error) {
		calls++
		return nil, errors.New("unexpected starter call")
	})

	r := AutoRegisterWithConfigContext(context.Background(), AutoRegisterConfig{
		Providers: map[string]ProviderConfig{
			providerAnthropic:  {Disabled: true},
			providerOpenAI:     {Disabled: true},
			providerClaudeCode: {Disabled: true},
			providerCodex:      {Disabled: true},
			providerOllama:     {BaseURL: baseURL, AutoStart: true},
		},
		DefaultProvider: providerOpenAI,
		DefaultModel:    "gpt-4.1",
	})

	_, ok := r.Provider(providerOllama)
	require.True(t, ok)
	assert.Equal(t, 0, calls)
}

func TestAutoRegisterWithConfigContext_DisableAutoStartBlocksSelectedOllama(t *testing.T) {
	t.Setenv("OLLAMA_BASE_URL", "")
	t.Setenv(envOllamaAutoStart, "true")

	baseURL := unusedLocalOllamaURL(t)

	var calls int

	withOllamaServeStarter(t, func(context.Context, ollamaStartRequest) (*ollamaDaemonStart, error) {
		calls++
		return nil, errors.New("unexpected starter call")
	})

	r := AutoRegisterWithConfigContext(context.Background(), AutoRegisterConfig{
		Providers: map[string]ProviderConfig{
			providerAnthropic:  {Disabled: true},
			providerOpenAI:     {Disabled: true},
			providerClaudeCode: {Disabled: true},
			providerCodex:      {Disabled: true},
			providerOllama:     {BaseURL: baseURL, AutoStart: true},
		},
		DefaultProvider:  providerOllama,
		DefaultModel:     "llama3.2",
		DisableAutoStart: true,
	})

	_, ok := r.Provider(providerOllama)
	require.True(t, ok)
	assert.Equal(t, 0, calls)
}

func TestShouldAutoStartOllama(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  AutoRegisterConfig
		want bool
	}{
		{name: "default provider", cfg: AutoRegisterConfig{DefaultProvider: providerOllama}, want: true},
		{name: "provider model", cfg: AutoRegisterConfig{SelectedModel: "ollama/llama3.2"}, want: true},
		{name: "known local model", cfg: AutoRegisterConfig{DefaultModel: "llama3.2"}, want: true},
		{name: "non ollama", cfg: AutoRegisterConfig{DefaultProvider: providerOpenAI, DefaultModel: "gpt-4.1"}, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, shouldAutoStartOllama(tt.cfg))
		})
	}
}

func TestOllamaAutoStartPolicy_EnvAndConfigOptInOut(t *testing.T) {
	unsetOllamaAutoStartEnvForTest(t)

	fromDefault := ollamaAutoStartPolicy(false)
	assert.False(t, fromDefault.Enabled)
	assert.Equal(t, "default", fromDefault.Source)

	fromConfig := ollamaAutoStartPolicy(true)
	assert.True(t, fromConfig.Enabled)
	assert.Equal(t, "config.providers.ollama.auto_start", fromConfig.Source)

	t.Setenv(envOllamaAutoStart, "false")

	envFalse := ollamaAutoStartPolicy(true)
	assert.False(t, envFalse.Enabled)
	assert.Equal(t, "env."+envOllamaAutoStart, envFalse.Source)

	t.Setenv(envOllamaAutoStart, "true")

	envTrue := ollamaAutoStartPolicy(false)
	assert.True(t, envTrue.Enabled)
	assert.Equal(t, "env."+envOllamaAutoStart, envTrue.Source)
}

func TestOllamaAutoStartPolicy_InvalidEnvDisablesAutoStartWithError(t *testing.T) {
	t.Setenv(envOllamaAutoStart, "maybe")

	policy := ollamaAutoStartPolicy(true)
	assert.False(t, policy.Enabled)
	assert.Equal(t, "env."+envOllamaAutoStart, policy.Source)
	assert.Contains(t, policy.Error, envOllamaAutoStart)
}

func TestKnownProvidersIncludesOllama(t *testing.T) {
	t.Parallel()

	providers := KnownProviders()
	for _, provider := range providers {
		if provider.Name == providerOllama {
			assert.Contains(t, provider.Models, "llama3.2")
			return
		}
	}

	require.Fail(t, "KnownProviders missing ollama")
}

func writeOllamaStream(t *testing.T, w http.ResponseWriter, events ...ollamaChatResponse) {
	t.Helper()

	payload := ollamaStreamString(t, events...)

	_, err := w.Write([]byte(payload))
	require.NoError(t, err)

	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func ollamaStreamString(t *testing.T, events ...ollamaChatResponse) string {
	t.Helper()

	var b strings.Builder

	for i := range events {
		payload, err := json.Marshal(events[i])
		require.NoError(t, err)

		b.Write(payload)
		b.WriteByte('\n')
	}

	return b.String()
}

func withOllamaServeStarter(t *testing.T, starter ollamaServeStarter) {
	t.Helper()
	ollamaServeStarterMu.Lock()
	previous := startOllamaServe
	startOllamaServe = starter
	ollamaServeStarterMu.Unlock()

	t.Cleanup(func() {
		ollamaServeStarterMu.Lock()
		startOllamaServe = previous
		ollamaServeStarterMu.Unlock()
	})
}

func withOllamaProcessHooks(
	t *testing.T,
	alive func(int) bool,
	terminate func(int) error,
	kill func(int) error,
) {
	t.Helper()
	ollamaProcessHooksMu.Lock()
	previousAlive := ollamaProcessAlive
	previousTerminate := ollamaTerminateProcess
	previousKill := ollamaKillProcess
	previousMatches := ollamaProcessMatches
	ollamaProcessAlive = alive
	ollamaTerminateProcess = terminate
	ollamaKillProcess = kill
	ollamaProcessMatches = func(*OllamaDaemonOwnership) bool { return true }
	ollamaProcessHooksMu.Unlock()

	t.Cleanup(func() {
		ollamaProcessHooksMu.Lock()
		ollamaProcessAlive = previousAlive
		ollamaTerminateProcess = previousTerminate
		ollamaKillProcess = previousKill
		ollamaProcessMatches = previousMatches
		ollamaProcessHooksMu.Unlock()
	})
}

func withOllamaProcessMatchHook(t *testing.T, matches func(*OllamaDaemonOwnership) bool) {
	t.Helper()
	ollamaProcessHooksMu.Lock()
	previousMatches := ollamaProcessMatches
	ollamaProcessMatches = matches
	ollamaProcessHooksMu.Unlock()

	t.Cleanup(func() {
		ollamaProcessHooksMu.Lock()
		ollamaProcessMatches = previousMatches
		ollamaProcessHooksMu.Unlock()
	})
}

//nolint:usetesting // This helper intentionally restores an unset-vs-empty distinction.
func unsetOllamaAutoStartEnvForTest(t *testing.T) {
	t.Helper()

	key := envOllamaAutoStart
	previous, ok := os.LookupEnv(key)
	require.NoError(t, os.Unsetenv(key))

	t.Cleanup(func() {
		if ok {
			assert.NoError(t, os.Setenv(key, previous))
		} else {
			assert.NoError(t, os.Unsetenv(key))
		}
	})
}

func unusedLocalOllamaURL(t *testing.T) string {
	t.Helper()

	ln, err := new(net.ListenConfig).Listen(context.Background(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)

	addr := ln.Addr().String()
	require.NoError(t, ln.Close())

	return "http://" + addr
}

func startOllamaTagsServer(ctx context.Context, t *testing.T, baseURL string) (*http.Server, error) {
	t.Helper()

	parsed, err := url.Parse(baseURL)
	if err != nil {
		return nil, err
	}

	ln, err := new(net.ListenConfig).Listen(ctx, "tcp", parsed.Host)
	if err != nil {
		return nil, err
	}

	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "/api/tags", r.URL.Path)
			w.Header().Set("Content-Type", "application/json")
			_, err := w.Write([]byte(`{"models":[{"name":"llama3.2"}]}`))
			assert.NoError(t, err)
		}),
		ReadHeaderTimeout: time.Second,
	}

	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("serve fake ollama tags: %v", err)
		}
	}()

	return srv, nil
}
