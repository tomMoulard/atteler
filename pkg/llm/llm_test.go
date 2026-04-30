package llm

import (
	"context"
	"errors"
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
		name:   alphaProvider,
		models: []string{"a-1", "a-2"},
		resp:   &Response{},
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
