package llm

import (
	"context"
	"errors"
	"testing"
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

func (f *fakeProvider) Complete(_ context.Context, p CompleteParams) (*Response, error) {
	if f.err != nil {
		return nil, f.err
	}
	r := *f.resp
	r.Model = p.Model
	return &r, nil
}

func TestRegistry_RegisterAndListModels(t *testing.T) {
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
		t.Fatalf("expected 3 models, got %d: %v", len(models), models)
	}

	want := map[string]bool{"a-1": true, "a-2": true, "b-1": true}
	for _, m := range models {
		if !want[m] {
			t.Errorf("unexpected model %q", m)
		}
	}
}

func TestKnownProviders(t *testing.T) {
	providers := KnownProviders()
	if len(providers) < 2 {
		t.Fatalf("known providers len = %d, want at least 2", len(providers))
	}
	if providers[0].Name == "" || len(providers[0].Models) == 0 {
		t.Fatalf("first provider missing data: %+v", providers[0])
	}
}

func TestRegistry_CompleteRoutesToCorrectProvider(t *testing.T) {
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
		t.Fatal(err)
	}
	if resp.Model != betaModel {
		t.Errorf("expected model %s, got %q", betaModel, resp.Model)
	}
}

func TestRegistry_CompleteRoutesProviderQualifiedModel(t *testing.T) {
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
		t.Fatal(err)
	}
	if resp.Content != "from-beta" {
		t.Errorf("content = %q, want from-beta", resp.Content)
	}
	if resp.Model != "shared" {
		t.Errorf("model = %q, want shared", resp.Model)
	}
}

func TestRegistry_CompleteInfersProviderForLiveOnlyClaudeModel(t *testing.T) {
	const liveModel = "claude-opus-4-6"

	r := NewRegistry()
	r.Register(&fakeProvider{
		name:   providerAnthropic,
		models: []string{"claude-sonnet-4-20250514"},
		resp:   &Response{Content: "from-anthropic"},
	})

	resp, err := r.Complete(context.Background(), CompleteParams{Model: liveModel})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Model != liveModel {
		t.Errorf("model = %q, want %s", resp.Model, liveModel)
	}
	if resp.Content != "from-anthropic" {
		t.Errorf("content = %q, want from-anthropic", resp.Content)
	}
}

func TestRegistry_CompleteInfersClaudeCodeBeforeAnthropic(t *testing.T) {
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
		t.Fatal(err)
	}
	if resp.Content != "from-claude-code" {
		t.Errorf("content = %q, want from-claude-code", resp.Content)
	}
}

func TestRegistry_CompleteUnknownModel(t *testing.T) {
	r := NewRegistry()
	r.Register(&fakeProvider{name: "x", models: []string{"x-1"}, resp: &Response{}})

	_, err := r.Complete(context.Background(), CompleteParams{Model: "nope"})
	if err == nil {
		t.Fatal("expected error for unknown model")
	}
}

func TestRegistry_CompleteWithFallback(t *testing.T) {
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
		t.Fatal(err)
	}
	if resp.Model != betaModel {
		t.Errorf("model = %q, want %s", resp.Model, betaModel)
	}
}

func TestRegistry_CompleteWithFallbackAllFail(t *testing.T) {
	r := NewRegistry()
	r.Register(&fakeProvider{
		err:    errors.New("boom"),
		name:   alphaProvider,
		models: []string{"a-1"},
		resp:   &Response{},
	})

	_, err := r.CompleteWithFallback(context.Background(), CompleteParams{Model: "a-1"}, []string{"missing"})
	if err == nil {
		t.Fatal("expected fallback failure")
	}
}

func TestRegistry_CompleteFallsBackToDefault(t *testing.T) {
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
		t.Fatal(err)
	}
	if resp.Model != "a-1" {
		t.Errorf("expected default model a-1, got %q", resp.Model)
	}
}

func TestRegistry_SetDefault(t *testing.T) {
	r := NewRegistry()
	r.Register(&fakeProvider{name: alphaProvider, models: []string{"a-1"}, resp: &Response{}})
	r.Register(&fakeProvider{name: "beta", models: []string{betaModel}, resp: &Response{}})

	if err := r.SetDefault("beta"); err != nil {
		t.Fatal(err)
	}

	resp, err := r.Complete(context.Background(), CompleteParams{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Model != betaModel {
		t.Errorf("expected default model %s after SetDefault, got %q", betaModel, resp.Model)
	}
}

func TestRegistry_SetDefaultModel(t *testing.T) {
	r := NewRegistry()
	r.Register(&fakeProvider{name: alphaProvider, models: []string{"a-1"}, resp: &Response{}})
	r.Register(&fakeProvider{name: "beta", models: []string{betaModel}, resp: &Response{}})

	if err := r.SetDefaultModel(betaModel); err != nil {
		t.Fatal(err)
	}

	resp, err := r.Complete(context.Background(), CompleteParams{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Model != betaModel {
		t.Errorf("expected default model %s, got %q", betaModel, resp.Model)
	}
}

func TestRegistry_SetDefaultModelProviderQualified(t *testing.T) {
	r := NewRegistry()
	r.Register(&fakeProvider{name: alphaProvider, models: []string{"shared"}, resp: &Response{Content: "from-alpha"}})
	r.Register(&fakeProvider{name: "beta", models: []string{"shared"}, resp: &Response{Content: "from-beta"}})

	if err := r.SetDefaultModel("beta/" + liveOnlyModel); err != nil {
		t.Fatal(err)
	}

	resp, err := r.Complete(context.Background(), CompleteParams{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "from-beta" {
		t.Errorf("content = %q, want from-beta", resp.Content)
	}
	if resp.Model != liveOnlyModel {
		t.Errorf("model = %q, want live-only-model", resp.Model)
	}
}

func TestRegistry_SetDefaultProviderModelIndexesConfiguredModel(t *testing.T) {
	r := NewRegistry()
	r.Register(&fakeProvider{name: alphaProvider, models: []string{"a-1"}, resp: &Response{Content: "ok"}})

	if err := r.SetDefaultProviderModel(alphaProvider, liveOnlyModel); err != nil {
		t.Fatal(err)
	}

	resp, err := r.Complete(context.Background(), CompleteParams{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Model != liveOnlyModel {
		t.Errorf("expected default model live-only-model, got %q", resp.Model)
	}

	resp, err = r.Complete(context.Background(), CompleteParams{Model: liveOnlyModel})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Model != liveOnlyModel {
		t.Errorf("expected selected model live-only-model, got %q", resp.Model)
	}
}

func TestRegistry_SetDefaultUnknown(t *testing.T) {
	r := NewRegistry()
	if err := r.SetDefault("nope"); err == nil {
		t.Fatal("expected error for unknown provider")
	}
}

func TestRegistry_SetDefaultModelUnknown(t *testing.T) {
	r := NewRegistry()
	if err := r.SetDefaultModel("missing"); err == nil {
		t.Fatal("expected error for unknown model")
	}
}

func TestRegistry_CompleteNoProviders(t *testing.T) {
	r := NewRegistry()
	_, err := r.Complete(context.Background(), CompleteParams{})
	if err == nil {
		t.Fatal("expected error with empty registry")
	}
}

func TestRegistry_ProviderLookup(t *testing.T) {
	r := NewRegistry()
	fp := &fakeProvider{name: alphaProvider, models: []string{"a-1"}, resp: &Response{}}
	r.Register(fp)

	p, ok := r.Provider(alphaProvider)
	if !ok || p.Name() != alphaProvider {
		t.Fatal("expected to find provider alpha")
	}

	_, ok = r.Provider("nope")
	if ok {
		t.Fatal("expected provider nope to not be found")
	}
}

func TestRegistry_ProviderForModel(t *testing.T) {
	r := NewRegistry()
	r.Register(&fakeProvider{name: alphaProvider, models: []string{"a-1"}, resp: &Response{}})

	provider, ok := r.ProviderForModel("a-1")
	if !ok || provider != alphaProvider {
		t.Fatalf("ProviderForModel = %q/%v, want alpha/true", provider, ok)
	}

	if _, ok := r.ProviderForModel("missing"); ok {
		t.Fatal("expected missing model to not resolve")
	}
}

func TestRegistry_ProviderForModelProviderQualified(t *testing.T) {
	r := NewRegistry()
	r.Register(&fakeProvider{name: alphaProvider, models: []string{"a-1"}, resp: &Response{}})

	provider, ok := r.ProviderForModel("alpha/live-only")
	if !ok || provider != alphaProvider {
		t.Fatalf("ProviderForModel = %q/%v, want alpha/true", provider, ok)
	}

	if _, ok := r.ProviderForModel("missing/live-only"); ok {
		t.Fatal("expected unknown provider to not resolve")
	}
}

func TestRegistry_ProviderModelsIndexesFetchedModels(t *testing.T) {
	r := NewRegistry()
	r.Register(&fakeProvider{
		name:          alphaProvider,
		models:        []string{"static-model"},
		fetchedModels: []string{"live-model"},
		resp:          &Response{Content: "ok"},
	})

	models, err := r.ProviderModels(context.Background(), alphaProvider)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0] != "live-model" {
		t.Fatalf("models = %v, want [live-model]", models)
	}

	resp, err := r.Complete(context.Background(), CompleteParams{Model: "live-model"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Model != "live-model" {
		t.Errorf("expected live model routing, got %q", resp.Model)
	}
}

func TestRegistry_CheckHealth(t *testing.T) {
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
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	// Results should be sorted by name.
	if results[0].Name != alphaProvider {
		t.Errorf("first result name = %q, want alpha", results[0].Name)
	}
	if !results[0].Healthy {
		t.Error("alpha should be healthy")
	}
	if len(results[0].Models) != 2 {
		t.Errorf("alpha models = %d, want 2", len(results[0].Models))
	}

	if results[1].Name != "beta" {
		t.Errorf("second result name = %q, want beta", results[1].Name)
	}
	if results[1].Healthy {
		t.Error("beta should not be healthy")
	}
	if results[1].Error == nil || results[1].Error.Error() != "connection refused" {
		t.Errorf("beta error = %v, want connection refused", results[1].Error)
	}
	if len(results[1].Models) != 1 {
		t.Errorf("beta models = %d, want 1", len(results[1].Models))
	}
}

func TestRegistry_CheckHealthUseFetchedModels(t *testing.T) {
	r := NewRegistry()
	r.Register(&fakeProvider{
		name:          alphaProvider,
		models:        []string{"static"},
		fetchedModels: []string{"live-1", "live-2"},
		resp:          &Response{},
	})

	results := r.CheckHealth(context.Background())
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if len(results[0].Models) != 2 || results[0].Models[0] != "live-1" {
		t.Errorf("models = %v, want [live-1 live-2]", results[0].Models)
	}
}
