package llm

import (
	"context"
	"testing"
)

// fakeProvider is a minimal Provider for testing the Registry.
type fakeProvider struct {
	err    error
	resp   *Response
	name   string
	models []string
}

func (f *fakeProvider) Name() string { return f.name }

func (f *fakeProvider) Models() []string { return f.models }

func (f *fakeProvider) FetchModels(_ context.Context) ([]string, error) {
	return f.models, nil
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
		name:   "alpha",
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

func TestRegistry_CompleteRoutesToCorrectProvider(t *testing.T) {
	r := NewRegistry()
	r.Register(&fakeProvider{
		name:   "alpha",
		models: []string{"a-1"},
		resp:   &Response{Content: "from-alpha"},
	})
	r.Register(&fakeProvider{
		name:   "beta",
		models: []string{"b-1"},
		resp:   &Response{Content: "from-beta"},
	})

	resp, err := r.Complete(context.Background(), CompleteParams{Model: "b-1"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Model != "b-1" {
		t.Errorf("expected model b-1, got %q", resp.Model)
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

func TestRegistry_CompleteFallsBackToDefault(t *testing.T) {
	r := NewRegistry()
	r.Register(&fakeProvider{
		name:   "alpha",
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
	r.Register(&fakeProvider{name: "alpha", models: []string{"a-1"}, resp: &Response{}})
	r.Register(&fakeProvider{name: "beta", models: []string{"b-1"}, resp: &Response{}})

	if err := r.SetDefault("beta"); err != nil {
		t.Fatal(err)
	}

	resp, err := r.Complete(context.Background(), CompleteParams{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Model != "b-1" {
		t.Errorf("expected default model b-1 after SetDefault, got %q", resp.Model)
	}
}

func TestRegistry_SetDefaultUnknown(t *testing.T) {
	r := NewRegistry()
	if err := r.SetDefault("nope"); err == nil {
		t.Fatal("expected error for unknown provider")
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
	fp := &fakeProvider{name: "alpha", models: []string{"a-1"}, resp: &Response{}}
	r.Register(fp)

	p, ok := r.Provider("alpha")
	if !ok || p.Name() != "alpha" {
		t.Fatal("expected to find provider alpha")
	}

	_, ok = r.Provider("nope")
	if ok {
		t.Fatal("expected provider nope to not be found")
	}
}
