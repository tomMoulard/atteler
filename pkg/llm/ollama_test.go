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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOllamaProvider_Complete(t *testing.T) {
	t.Parallel()
	var gotReq ollamaChatRequest
	var gotPath string
	var gotContentType string

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
		Model:       "llama3.2",
		MaxTokens:   128,
		Temperature: &temperature,
		TopP:        &topP,
		Seed:        &seed,
		Stop:        []string{"</stop>"},
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

	assert.Equal(t, "hello from ollama", resp.Content)
	assert.Equal(t, "llama3.2", resp.Model)
	assert.Equal(t, 7, resp.InputTokens)
	assert.Equal(t, 4, resp.OutputTokens)
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
	t.Setenv(envOllamaAutoStart, "")

	baseURL := unusedLocalOllamaURL(t)
	var calls int
	var srv *http.Server
	withOllamaServeStarter(t, func(gotBaseURL string) error {
		calls++
		assert.Equal(t, baseURL, gotBaseURL)

		var err error
		srv, err = startOllamaTagsServer(t, gotBaseURL)
		return err
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
	t.Setenv(envOllamaAutoStart, "")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/tags", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(`{"models":[]}`))
		assert.NoError(t, err)
	}))
	defer srv.Close()

	var calls int
	withOllamaServeStarter(t, func(string) error {
		calls++
		return errors.New("unexpected starter call")
	})

	p, err := NewOllamaProviderWithConfigContext(context.Background(), ProviderConfig{BaseURL: srv.URL, AutoStart: true})
	require.NoError(t, err)
	assert.Equal(t, srv.URL, p.baseURL)
	assert.False(t, p.startAttempted)
	assert.Equal(t, 0, calls)
}

func TestOllamaProvider_AutoStartReturnsStarterError(t *testing.T) {
	t.Setenv("OLLAMA_BASE_URL", "")
	t.Setenv(envOllamaAutoStart, "")

	baseURL := unusedLocalOllamaURL(t)
	withOllamaServeStarter(t, func(string) error {
		return errors.New("ollama: start daemon: binary not found")
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
	withOllamaServeStarter(t, func(string) error {
		calls++
		return errors.New("unexpected starter call")
	})

	disabled, err := NewOllamaProviderWithConfigContext(context.Background(), ProviderConfig{BaseURL: "http://127.0.0.1:1", AutoStart: true})
	require.NoError(t, err)
	assert.Equal(t, "http://127.0.0.1:1", disabled.baseURL)

	t.Setenv(envOllamaAutoStart, "")
	remote, err := NewOllamaProviderWithConfigContext(context.Background(), ProviderConfig{BaseURL: "http://ollama.example", AutoStart: true})
	require.NoError(t, err)
	assert.Equal(t, "http://ollama.example", remote.baseURL)
	assert.Equal(t, 0, calls)
}

func TestAutoRegisterWithConfigContext_RegistersConfiguredOllama(t *testing.T) {
	t.Setenv("OLLAMA_BASE_URL", "")

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
			providerOllama:     {BaseURL: srv.URL},
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
}

func TestAutoRegisterWithConfigContext_StartsLocalOllamaForDefaultProvider(t *testing.T) {
	t.Setenv("OLLAMA_BASE_URL", "")
	t.Setenv(envOllamaAutoStart, "")

	baseURL := unusedLocalOllamaURL(t)
	var srv *http.Server
	withOllamaServeStarter(t, func(gotBaseURL string) error {
		assert.Equal(t, baseURL, gotBaseURL)
		var err error
		srv, err = startOllamaTagsServer(t, gotBaseURL)
		return err
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
			providerOllama:     {BaseURL: baseURL},
		},
		DefaultProvider: providerOllama,
		DefaultModel:    "llama3.2",
	})

	p, ok := r.Provider(providerOllama)
	require.True(t, ok)
	assert.Equal(t, providerOllama, p.Name())
}

func TestShouldAutoStartOllama(t *testing.T) {
	t.Parallel()

	tests := []struct {
		cfg  AutoRegisterConfig
		name string
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

func unusedLocalOllamaURL(t *testing.T) string {
	t.Helper()
	ln, err := new(net.ListenConfig).Listen(context.Background(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())
	return "http://" + addr
}

func startOllamaTagsServer(t *testing.T, baseURL string) (*http.Server, error) {
	t.Helper()

	parsed, err := url.Parse(baseURL)
	if err != nil {
		return nil, err
	}
	ln, err := new(net.ListenConfig).Listen(context.Background(), "tcp", parsed.Host)
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
