package llm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	betaModel     = "b-1"
	liveOnlyModel = "live-only-model"
	alphaProvider = "alpha"
)

// fakeProvider is a minimal Provider for testing the Registry.
type fakeProvider struct {
	err            error
	healthCheckErr error
	resp           *Response
	name           string
	models         []string
	fetchedModels  []string
	warnings       []string
}

func (f *fakeProvider) Name() string { return f.name }

func (f *fakeProvider) Models() []string { return f.models }

func (f *fakeProvider) FetchModels(_ context.Context) ([]string, error) {
	if f.fetchedModels != nil {
		return f.fetchedModels, nil
	}

	return f.models, nil
}

func (f *fakeProvider) HealthCheck(_ context.Context) error {
	return f.healthCheckErr
}

func (f *fakeProvider) ProviderWarnings() []string {
	return append([]string(nil), f.warnings...)
}

func (f *fakeProvider) ModelContextWindow(string) int {
	return 128_000
}

func (f *fakeProvider) Complete(_ context.Context, p CompleteParams) (*Response, error) {
	if f.err != nil {
		return nil, f.err
	}

	r := *f.resp
	r.Model = p.Model

	return &r, nil
}

func TestRegistry_RegisterAndListModels(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	r.Register(&fakeProvider{
		name:   alphaProvider,
		models: []string{"a-1", "a-2"},
		resp:   &Response{Content: "ok"},
	})
	r.Register(&fakeProvider{
		name:   "beta",
		models: []string{"b-1"},
		resp:   &Response{Content: "ok"},
	})

	models := r.ListModels()
	if len(models) != 3 {
		require.Failf(t, "unexpected failure", "expected 3 models, got %d: %v", len(models), models)
	}

	want := map[string]bool{"a-1": true, "a-2": true, "b-1": true}
	for _, m := range models {
		if !want[m] {
			assert.Failf(t, "assertion failed", "unexpected model %q", m)
		}
	}
}

func TestKnownProviders(t *testing.T) {
	t.Parallel()

	providers := KnownProviders()
	if len(providers) < 2 {
		require.Failf(t, "unexpected failure", "known providers len = %d, want at least 2", len(providers))
	}

	if providers[0].Name == "" || len(providers[0].Models) == 0 {
		require.Failf(t, "unexpected failure", "first provider missing data: %+v", providers[0])
	}
}

func TestRegistry_RequiresActiveContextForBlockingMethods(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	r.Register(&fakeProvider{
		name:   alphaProvider,
		models: []string{"a-1"},
		resp:   &Response{Content: "ok"},
	})

	_, err := r.Complete(nil, CompleteParams{Model: "a-1"}) //nolint:staticcheck // Verify nil contexts are rejected instead of panicking.
	require.Error(t, err)
	require.ErrorIs(t, err, ErrContextRequired)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = r.CompleteWithFallback(ctx, CompleteParams{Model: "a-1"}, nil)
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)

	_, err = r.ProviderModels(nil, alphaProvider) //nolint:staticcheck // Verify nil contexts are rejected instead of panicking.
	require.Error(t, err)
	require.ErrorIs(t, err, ErrContextRequired)

	health := r.CheckHealth(nil) //nolint:staticcheck // Verify nil contexts are reported without provider calls.
	require.Len(t, health, 1)
	require.ErrorIs(t, health[0].Error, ErrContextRequired)
	assert.False(t, health[0].Healthy)
}

func TestRegistry_CompleteRoutesToCorrectProvider(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	r.Register(&fakeProvider{
		name:   alphaProvider,
		models: []string{"a-1"},
		resp:   &Response{Content: "from-alpha"},
	})
	r.Register(&fakeProvider{
		name:   "beta",
		models: []string{betaModel},
		resp:   &Response{Content: "from-beta"},
	})

	resp, err := r.Complete(context.Background(), CompleteParams{Model: betaModel})
	if err != nil {
		require.NoError(t, err)
	}

	if resp.Model != betaModel {
		assert.Failf(t, "assertion failed", "expected model %s, got %q", betaModel, resp.Model)
	}
}

func TestRegistry_CompleteRoutesProviderQualifiedModel(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	r.Register(&fakeProvider{
		name:   alphaProvider,
		models: []string{"shared"},
		resp:   &Response{Content: "from-alpha"},
	})
	r.Register(&fakeProvider{
		name:   "beta",
		models: []string{"shared"},
		resp:   &Response{Content: "from-beta"},
	})

	resp, err := r.Complete(context.Background(), CompleteParams{Model: "beta/shared"})
	if err != nil {
		require.NoError(t, err)
	}

	if resp.Content != "from-beta" {
		assert.Failf(t, "assertion failed", "content = %q, want from-beta", resp.Content)
	}

	if resp.Model != "shared" {
		assert.Failf(t, "assertion failed", "model = %q, want shared", resp.Model)
	}
}

func TestRegistry_CompleteInfersProviderForLiveOnlyClaudeModel(t *testing.T) {
	t.Parallel()

	const liveModel = "claude-opus-4-6"

	r := NewRegistry()
	r.Register(&fakeProvider{
		name:   providerAnthropic,
		models: []string{"claude-sonnet-4-20250514"},
		resp:   &Response{Content: "from-anthropic"},
	})

	resp, err := r.Complete(context.Background(), CompleteParams{Model: liveModel})
	if err != nil {
		require.NoError(t, err)
	}

	if resp.Model != liveModel {
		assert.Failf(t, "assertion failed", "model = %q, want %s", resp.Model, liveModel)
	}

	if resp.Content != "from-anthropic" {
		assert.Failf(t, "assertion failed", "content = %q, want from-anthropic", resp.Content)
	}
}

func TestRegistry_CompleteInfersClaudeCodeBeforeAnthropic(t *testing.T) {
	t.Parallel()

	const liveModel = "claude-future"

	r := NewRegistry()
	r.Register(&fakeProvider{
		name:   providerAnthropic,
		models: []string{"claude-sonnet-4-20250514"},
		resp:   &Response{Content: "from-anthropic"},
	})
	r.Register(&fakeProvider{
		name:   providerClaudeCode,
		models: []string{"claude-opus-4-6"},
		resp:   &Response{Content: "from-claude-code"},
	})

	resp, err := r.Complete(context.Background(), CompleteParams{Model: liveModel})
	if err != nil {
		require.NoError(t, err)
	}

	if resp.Content != "from-claude-code" {
		assert.Failf(t, "assertion failed", "content = %q, want from-claude-code", resp.Content)
	}
}

func TestRegistry_CompleteUnknownModel(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	r.Register(&fakeProvider{name: "x", models: []string{"x-1"}, resp: &Response{}})

	_, err := r.Complete(context.Background(), CompleteParams{Model: "nope"})
	if err == nil {
		require.FailNow(t, "expected error for unknown model")
	}
}

func TestRegistry_CompleteWithFallback(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	r.Register(&fakeProvider{
		err:    errors.New("primary failed"),
		name:   alphaProvider,
		models: []string{"a-1"},
		resp:   &Response{},
	})
	r.Register(&fakeProvider{
		name:   "beta",
		models: []string{betaModel},
		resp:   &Response{Content: "ok"},
	})

	resp, err := r.CompleteWithFallback(context.Background(), CompleteParams{
		Model: "a-1",
	}, []string{betaModel})
	if err != nil {
		require.NoError(t, err)
	}

	if resp.Model != betaModel {
		assert.Failf(t, "assertion failed", "model = %q, want %s", resp.Model, betaModel)
	}
}

func TestRegistry_CompleteWithFallbackAllFail(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	r.Register(&fakeProvider{
		err:    errors.New("boom"),
		name:   alphaProvider,
		models: []string{"a-1"},
		resp:   &Response{},
	})

	_, err := r.CompleteWithFallback(context.Background(), CompleteParams{Model: "a-1"}, []string{"missing"})
	if err == nil {
		require.FailNow(t, "expected fallback failure")
	}
}

func TestRegistry_CompleteFallsBackToDefault(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	r.Register(&fakeProvider{
		name:   alphaProvider,
		models: []string{"a-1", "a-2"},
		resp:   &Response{Content: "default"},
	})

	resp, err := r.Complete(context.Background(), CompleteParams{
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		require.NoError(t, err)
	}

	if resp.Model != "a-1" {
		assert.Failf(t, "assertion failed", "expected default model a-1, got %q", resp.Model)
	}
}

func TestRegistry_SetDefault(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	r.Register(&fakeProvider{name: alphaProvider, models: []string{"a-1"}, resp: &Response{}})
	r.Register(&fakeProvider{name: "beta", models: []string{betaModel}, resp: &Response{}})

	if err := r.SetDefault("beta"); err != nil {
		require.NoError(t, err)
	}

	resp, err := r.Complete(context.Background(), CompleteParams{})
	if err != nil {
		require.NoError(t, err)
	}

	if resp.Model != betaModel {
		assert.Failf(t, "assertion failed", "expected default model %s after SetDefault, got %q", betaModel, resp.Model)
	}
}

func TestRegistry_SetDefaultModel(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	r.Register(&fakeProvider{name: alphaProvider, models: []string{"a-1"}, resp: &Response{}})
	r.Register(&fakeProvider{name: "beta", models: []string{betaModel}, resp: &Response{}})

	if err := r.SetDefaultModel(betaModel); err != nil {
		require.NoError(t, err)
	}

	resp, err := r.Complete(context.Background(), CompleteParams{})
	if err != nil {
		require.NoError(t, err)
	}

	if resp.Model != betaModel {
		assert.Failf(t, "assertion failed", "expected default model %s, got %q", betaModel, resp.Model)
	}
}

func TestRegistry_SetDefaultModelProviderQualified(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	r.Register(&fakeProvider{name: alphaProvider, models: []string{"shared"}, resp: &Response{Content: "from-alpha"}})
	r.Register(&fakeProvider{name: "beta", models: []string{"shared"}, resp: &Response{Content: "from-beta"}})

	if err := r.SetDefaultModel("beta/" + liveOnlyModel); err != nil {
		require.NoError(t, err)
	}

	resp, err := r.Complete(context.Background(), CompleteParams{})
	if err != nil {
		require.NoError(t, err)
	}

	if resp.Content != "from-beta" {
		assert.Failf(t, "assertion failed", "content = %q, want from-beta", resp.Content)
	}

	if resp.Model != liveOnlyModel {
		assert.Failf(t, "assertion failed", "model = %q, want live-only-model", resp.Model)
	}
}

func TestRegistry_SetDefaultProviderModelIndexesConfiguredModel(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	r.Register(&fakeProvider{name: alphaProvider, models: []string{"a-1"}, resp: &Response{Content: "ok"}})

	if err := r.SetDefaultProviderModel(alphaProvider, liveOnlyModel); err != nil {
		require.NoError(t, err)
	}

	resp, err := r.Complete(context.Background(), CompleteParams{})
	if err != nil {
		require.NoError(t, err)
	}

	if resp.Model != liveOnlyModel {
		assert.Failf(t, "assertion failed", "expected default model live-only-model, got %q", resp.Model)
	}

	resp, err = r.Complete(context.Background(), CompleteParams{Model: liveOnlyModel})
	if err != nil {
		require.NoError(t, err)
	}

	if resp.Model != liveOnlyModel {
		assert.Failf(t, "assertion failed", "expected selected model live-only-model, got %q", resp.Model)
	}
}

func TestRegistry_SetDefaultUnknown(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	if err := r.SetDefault("nope"); err == nil {
		require.FailNow(t, "expected error for unknown provider")
	}
}

func TestRegistry_SetDefaultModelUnknown(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	if err := r.SetDefaultModel("missing"); err == nil {
		require.FailNow(t, "expected error for unknown model")
	}
}

func TestRegistry_CompleteNoProviders(t *testing.T) {
	t.Parallel()

	r := NewRegistry()

	_, err := r.Complete(context.Background(), CompleteParams{})
	if err == nil {
		require.FailNow(t, "expected error with empty registry")
	}
}

func TestRegistry_ProviderLookup(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	fp := &fakeProvider{name: alphaProvider, models: []string{"a-1"}, resp: &Response{}}
	r.Register(fp)

	p, ok := r.Provider(alphaProvider)
	if !ok || p.Name() != alphaProvider {
		require.FailNow(t, "expected to find provider alpha")
	}

	_, ok = r.Provider("nope")
	if ok {
		require.FailNow(t, "expected provider nope to not be found")
	}
}

func TestRegistry_ProviderForModel(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	r.Register(&fakeProvider{name: alphaProvider, models: []string{"a-1"}, resp: &Response{}})

	provider, ok := r.ProviderForModel("a-1")
	if !ok || provider != alphaProvider {
		require.Failf(t, "unexpected failure", "ProviderForModel = %q/%v, want alpha/true", provider, ok)
	}

	if _, ok := r.ProviderForModel("missing"); ok {
		require.FailNow(t, "expected missing model to not resolve")
	}
}

func TestRegistry_ProviderForModelProviderQualified(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	r.Register(&fakeProvider{name: alphaProvider, models: []string{"a-1"}, resp: &Response{}})

	provider, ok := r.ProviderForModel("alpha/live-only")
	if !ok || provider != alphaProvider {
		require.Failf(t, "unexpected failure", "ProviderForModel = %q/%v, want alpha/true", provider, ok)
	}

	if _, ok := r.ProviderForModel("missing/live-only"); ok {
		require.FailNow(t, "expected unknown provider to not resolve")
	}
}

func TestRegistry_ProviderModelsIndexesFetchedModels(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	r.Register(&fakeProvider{
		name:          alphaProvider,
		models:        []string{"static-model"},
		fetchedModels: []string{"live-model"},
		resp:          &Response{Content: "ok"},
	})

	models, err := r.ProviderModels(context.Background(), alphaProvider)
	if err != nil {
		require.NoError(t, err)
	}

	if len(models) != 1 || models[0] != "live-model" {
		require.Failf(t, "unexpected failure", "models = %v, want [live-model]", models)
	}

	resp, err := r.Complete(context.Background(), CompleteParams{Model: "live-model"})
	if err != nil {
		require.NoError(t, err)
	}

	if resp.Model != "live-model" {
		assert.Failf(t, "assertion failed", "expected live model routing, got %q", resp.Model)
	}
}

func TestRegistry_CheckHealth(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	r.Register(&fakeProvider{
		name:     alphaProvider,
		models:   []string{"a-1", "a-2"},
		resp:     &Response{},
		warnings: []string{"uses advisory path"},
	})
	r.Register(&fakeProvider{
		name:           "beta",
		models:         []string{betaModel},
		healthCheckErr: errors.New("connection refused"),
		resp:           &Response{},
	})

	results := r.CheckHealth(context.Background())
	if len(results) != 2 {
		require.Failf(t, "unexpected failure", "expected 2 results, got %d", len(results))
	}

	// Results should be sorted by name.
	if results[0].Name != alphaProvider {
		assert.Failf(t, "assertion failed", "first result name = %q, want alpha", results[0].Name)
	}

	if !results[0].Healthy {
		assert.Fail(t, "alpha should be healthy")
	}

	if len(results[0].Models) != 2 {
		assert.Failf(t, "assertion failed", "alpha models = %d, want 2", len(results[0].Models))
	}

	assert.Equal(t, []string{"uses advisory path"}, results[0].Warnings)

	if results[1].Name != "beta" {
		assert.Failf(t, "assertion failed", "second result name = %q, want beta", results[1].Name)
	}

	if results[1].Healthy {
		assert.Fail(t, "beta should not be healthy")
	}

	if results[1].Error == nil || results[1].Error.Error() != "connection refused" {
		assert.Failf(t, "assertion failed", "beta error = %v, want connection refused", results[1].Error)
	}

	if len(results[1].Models) != 1 {
		assert.Failf(t, "assertion failed", "beta models = %d, want 1", len(results[1].Models))
	}
}

func TestRegistry_CheckHealthUseFetchedModels(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	r.Register(&fakeProvider{
		name:          alphaProvider,
		models:        []string{"static"},
		fetchedModels: []string{"live-1", "live-2"},
		resp:          &Response{},
	})

	results := r.CheckHealth(context.Background())
	if len(results) != 1 {
		require.Failf(t, "unexpected failure", "expected 1 result, got %d", len(results))
	}

	if len(results[0].Models) != 2 || results[0].Models[0] != "live-1" {
		assert.Failf(t, "assertion failed", "models = %v, want [live-1 live-2]", results[0].Models)
	}
}

func TestRegistry_CheckHealthUsesAdapterDiagnostics(t *testing.T) {
	t.Parallel()

	auth := &codexChatGPTAuth{
		accessToken:  "access",
		refreshToken: "refresh",
		accountID:    "acct",
	}
	r := NewRegistry()
	r.Register(&CodexProvider{
		auth:   auth,
		models: []string{"gpt-5.5"},
	})

	results := r.CheckHealth(context.Background())
	require.Len(t, results, 1)

	assert.True(t, results[0].Healthy)
	require.NotNil(t, results[0].Contract)
	assert.Equal(t, codexAdapterVersion, results[0].Contract.AdapterVersion)
	assert.NotEmpty(t, results[0].Checks)
	assert.Contains(t, results[0].Warnings[0], "private")
}

func TestPrivateAdapterDiagnosticsReportsMissingCredentialContracts(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("CODEX_HOME", filepath.Join(tempDir, "codex"))
	t.Setenv("HOME", tempDir)
	t.Setenv("ATTELER_CLAUDE_CODE_SKIP_KEYCHAIN", "1")

	results := PrivateAdapterDiagnostics(context.Background(), AutoRegisterConfig{})
	require.Len(t, results, 2)

	byName := providerHealthByName(results)
	for _, providerName := range []string{providerClaudeCode, providerCodex} {
		result := byName[providerName]
		assert.False(t, result.Healthy)
		require.NotNil(t, result.Contract)
		assert.NotEmpty(t, result.Contract.AdapterVersion)

		checks := readinessChecksByName(result.Checks)
		assert.Equal(t, ReadinessFailed, checks["local_credentials"].Status)
		assert.Equal(t, ReadinessSkipped, checks["token_refresh"].Status)
		assert.Equal(t, ReadinessSkipped, checks["network_reachability"].Status)
		assert.Equal(t, ReadinessWarning, checks["model_availability"].Status)
	}
}

func TestPrivateAdapterDiagnosticsReportsCodexConfiguredModelWithoutCredentials(t *testing.T) {
	tempDir := t.TempDir()
	codexDir := filepath.Join(tempDir, "codex")
	require.NoError(t, os.MkdirAll(codexDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(codexDir, "config.toml"), []byte(`model = "gpt-test-codex"`), 0o600))

	t.Setenv("CODEX_HOME", codexDir)
	t.Setenv("HOME", tempDir)
	t.Setenv("ATTELER_CLAUDE_CODE_SKIP_KEYCHAIN", "1")
	t.Setenv("ATTELER_DISABLE_CLAUDE_CODE_ADAPTER", "1")

	results := PrivateAdapterDiagnostics(context.Background(), AutoRegisterConfig{})
	require.Len(t, results, 1)
	assert.Equal(t, providerCodex, results[0].Name)
	require.NotEmpty(t, results[0].Models)
	assert.Equal(t, "gpt-test-codex", results[0].Models[0])

	metadata, ok := (&CodexProvider{}).ModelMetadata("gpt-test-codex")
	require.True(t, ok)
	assert.Zero(t, metadata.ContextWindow)
	assert.Contains(t, metadata.Provenance, "config.toml")
}

func TestPrivateAdapterDiagnosticsSkipsDisabledAdapters(t *testing.T) {
	t.Setenv("ATTELER_DISABLE_PRIVATE_ADAPTERS", "1")

	results := PrivateAdapterDiagnostics(context.Background(), AutoRegisterConfig{})

	assert.Empty(t, results)
}

func TestPrivateAdapterDiagnosticsHonorsProviderKillSwitch(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("CODEX_HOME", filepath.Join(tempDir, "codex"))
	t.Setenv("HOME", tempDir)
	t.Setenv("ATTELER_CLAUDE_CODE_SKIP_KEYCHAIN", "1")
	t.Setenv("ATTELER_DISABLE_CODEX_ADAPTER", "1")

	results := PrivateAdapterDiagnostics(context.Background(), AutoRegisterConfig{})
	require.Len(t, results, 1)

	assert.Equal(t, providerClaudeCode, results[0].Name)
}

func TestAutoRegisterWithConfigContext_DisablesPrivateAdaptersOnly(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-test")
	t.Setenv("ANTHROPIC_API_KEY", "anthropic-test")
	t.Setenv("ATTELER_DISABLE_PRIVATE_ADAPTERS", "1")

	r := AutoRegisterWithConfigContext(context.Background(), AutoRegisterConfig{
		Providers: map[string]ProviderConfig{
			providerOllama: {Disabled: true},
		},
	})

	_, ok := r.Provider(providerOpenAI)
	assert.True(t, ok, "normal OpenAI provider should remain available")

	_, ok = r.Provider(providerAnthropic)
	assert.True(t, ok, "normal Anthropic provider should remain available")

	_, ok = r.Provider(providerCodex)
	assert.False(t, ok, "Codex private adapter should be kill-switched")

	_, ok = r.Provider(providerClaudeCode)
	assert.False(t, ok, "Claude Code private adapter should be kill-switched")
}

func TestAutoRegisterWithConfigContext_DisablesPrivateAdapterByProviderConfig(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-test")
	t.Setenv("CODEX_HOME", filepath.Dir(writeCodexAuthFile(t, "access", "refresh", "acct")))

	r := AutoRegisterWithConfigContext(context.Background(), AutoRegisterConfig{
		Providers: map[string]ProviderConfig{
			providerAnthropic:  {Disabled: true},
			providerClaudeCode: {Disabled: true},
			providerOllama:     {Disabled: true},
			providerCodex:      {DisablePrivateAdapter: true},
		},
	})

	_, ok := r.Provider(providerOpenAI)
	assert.True(t, ok, "normal OpenAI provider should remain available")

	_, ok = r.Provider(providerCodex)
	assert.False(t, ok, "Codex private adapter should honor disable_private_adapter config")
}

func TestAutoRegisterWithConfigContext_DisablesAnthropicBorrowedCredentialsOnly(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	t.Setenv("FORGE_CONFIG", "")
	t.Setenv("ATTELER_CLAUDE_CODE_SKIP_KEYCHAIN", "1")

	tempDir := t.TempDir()
	t.Setenv("HOME", tempDir)

	claudeDir := filepath.Join(tempDir, ".claude")
	require.NoError(t, os.MkdirAll(claudeDir, 0o750))

	body := `{"claudeAiOauth":{"accessToken":"borrowed-access","refreshToken":"refresh","expiresAt":9999999999999}}`
	require.NoError(t, os.WriteFile(filepath.Join(claudeDir, ".credentials.json"), []byte(body), 0o600))

	r := AutoRegisterWithConfigContext(context.Background(), AutoRegisterConfig{
		Providers: map[string]ProviderConfig{
			providerAnthropic:  {DisablePrivateAdapter: true},
			providerClaudeCode: {Disabled: true},
			providerCodex:      {Disabled: true},
			providerOllama:     {Disabled: true},
			providerOpenAI:     {Disabled: true},
		},
	})

	_, ok := r.Provider(providerAnthropic)
	assert.False(t, ok, "normal Anthropic provider should not borrow Claude Code credentials when disabled")
}

func providerHealthByName(results []ProviderHealth) map[string]ProviderHealth {
	out := make(map[string]ProviderHealth, len(results))
	for _, result := range results {
		out[result.Name] = result
	}

	return out
}
