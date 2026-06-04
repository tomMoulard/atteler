package llm

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/events"
	"github.com/tommoulard/atteler/pkg/modelroute"
)

const (
	betaModel     = "b-1"
	liveOnlyModel = "live-only-model"
	alphaProvider = "alpha"
)

// fakeProvider is a minimal Provider for testing the Registry.
type fakeProvider struct {
	err            error
	fetchErr       error
	healthCheckErr error
	fetchModelsErr error
	resp           *Response
	name           string
	models         []string
	fetchedModels  []string
	warnings       []string
	calls          []CompleteParams
	fetchCalls     int
	healthCalls    int
}

func (f *fakeProvider) Name() string { return f.name }

func (f *fakeProvider) Models() []string { return f.models }

func (f *fakeProvider) FetchModels(_ context.Context) ([]string, error) {
	f.fetchCalls++

	if f.fetchModelsErr != nil {
		return nil, f.fetchModelsErr
	}

	if f.fetchErr != nil {
		return nil, f.fetchErr
	}

	if f.fetchedModels != nil {
		return f.fetchedModels, nil
	}

	return f.models, nil
}

func (f *fakeProvider) HealthCheck(_ context.Context) error {
	f.healthCalls++

	return f.healthCheckErr
}

func (f *fakeProvider) ProviderWarnings() []string {
	return append([]string(nil), f.warnings...)
}

func (f *fakeProvider) ModelContextWindow(string) int {
	return 128_000
}

func (f *fakeProvider) Complete(_ context.Context, p CompleteParams) (*Response, error) {
	f.calls = append(f.calls, p)

	if f.err != nil {
		return nil, f.err
	}

	var r Response
	if f.resp != nil {
		r = *f.resp
	}

	if r.Model == "" {
		r.Model = p.Model
	}

	return &r, nil
}

type modelOmittingProvider struct {
	fakeProvider
}

func (f *modelOmittingProvider) Complete(ctx context.Context, p CompleteParams) (*Response, error) {
	resp, err := f.fakeProvider.Complete(ctx, p)
	if resp != nil {
		resp.Model = ""
	}

	return resp, err
}

type capabilityFakeProvider struct {
	capabilities ProviderCapabilities
	fakeProvider
}

func (f *capabilityFakeProvider) Capabilities() ProviderCapabilities {
	return f.capabilities
}

type localCapabilityFakeProvider struct {
	capabilityFakeProvider
}

func (f *localCapabilityFakeProvider) Local() bool { return true }

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

func TestEstimateTokensReturnsConservativeUpperBound(t *testing.T) {
	t.Parallel()

	oldFourCharsPerTokenEstimate := 3
	got := EstimateTokens([]Message{{Role: RoleUser, Content: strings.Repeat("x", 12)}})

	assert.Greater(t, got, oldFourCharsPerTokenEstimate)
	assert.Equal(t, 15, got)
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

func TestKnownProvidersIncludesBuiltinCatalogModels(t *testing.T) {
	t.Parallel()

	providers := KnownProviders()

	byName := make(map[string][]string, len(providers))
	for _, provider := range providers {
		byName[provider.Name] = provider.Models
	}

	assert.Contains(t, byName[providerOpenAI], "gpt-5.5")
	assert.Contains(t, byName[providerAnthropic], "claude-opus-4-7")
	assert.Contains(t, byName[providerCodex], "gpt-5.3-codex")
}

func TestRegistry_RegisterIndexesBuiltinCatalogProviderModels(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	r.Register(&OpenAIProvider{})

	assert.True(t, r.ProviderHasModel(providerOpenAI, "gpt-5.5"))
	assert.False(t, r.ProviderModelsVerified(providerOpenAI))
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

func TestAutoRegisterWithConfigContextReport_ReportsDisabledAndMissingCredentialProviders(t *testing.T) {
	t.Parallel()

	r := autoRegisterWithFactoriesContext(context.Background(), AutoRegisterConfig{
		Providers: map[string]ProviderConfig{
			alphaProvider: {Disabled: true},
			"beta":        {},
		},
		DefaultProvider:        "beta",
		DisableReadinessChecks: true,
		ReadinessCheckTimeout:  time.Second,
		ReadinessCacheTTL:      time.Minute,
	}, []providerRegistration{
		{
			name:         alphaProvider,
			staticModels: []string{"a-1"},
			factory: func() (Provider, error) {
				return &fakeProvider{name: alphaProvider, models: []string{"a-1"}, resp: &Response{}}, nil
			},
		},
		{
			name:         "beta",
			staticModels: []string{betaModel},
			factory: func() (Provider, error) {
				return nil, errors.New("no beta credentials found")
			},
		},
	})

	report := r.ReadinessReport()
	alpha := requireReadinessProvider(t, report, alphaProvider)
	assert.Equal(t, ProviderStatusDisabled, alpha.Status)
	assert.False(t, alpha.Registered)
	assert.True(t, alpha.Configured)
	assert.Equal(t, []string{"a-1"}, alpha.StaticModels)

	beta := requireReadinessProvider(t, report, "beta")
	assert.Equal(t, ProviderStatusMissingCredential, beta.Status)
	assert.True(t, beta.Configured)
	assert.True(t, beta.Requested)
	assert.False(t, beta.Registered)
	assert.Contains(t, beta.Error.Error(), "no beta credentials")
}

func TestAutoRegisterWithConfigContextReport_MarksProviderRequestedByCatalogModelRole(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer

	r := autoRegisterWithFactoriesContext(context.Background(), AutoRegisterConfig{
		Logger:       slog.New(slog.NewTextHandler(&logs, nil)),
		DefaultModel: "planner",
		ModelRoles: map[string]ModelRole{
			"planner": {Preferred: "gpt-4.1-mini"},
		},
		DisableReadinessChecks: true,
	}, []providerRegistration{
		{
			name:         providerOpenAI,
			staticModels: []string{"gpt-4.1-mini"},
			factory: func() (Provider, error) {
				return nil, errors.New("no openai credentials found")
			},
		},
	})

	report := r.ReadinessReport()
	openai := requireReadinessProvider(t, report, providerOpenAI)
	assert.Equal(t, ProviderStatusMissingCredential, openai.Status)
	assert.True(t, openai.Requested)
	assert.False(t, openai.Registered)
}

func TestAutoRegisterWithConfigContextReport_DoesNotRequestBannedCatalogProviderFromModelRole(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer

	r := autoRegisterWithFactoriesContext(context.Background(), AutoRegisterConfig{
		Logger:       slog.New(slog.NewTextHandler(&logs, nil)),
		DefaultModel: "planner",
		ModelRoles: map[string]ModelRole{
			"planner": {
				Preferred:       "gpt-4.1-mini",
				BannedProviders: []string{providerOpenAI},
			},
		},
		DisableReadinessChecks: true,
	}, []providerRegistration{
		{
			name:         providerOpenAI,
			staticModels: []string{"gpt-4.1-mini"},
			factory: func() (Provider, error) {
				return nil, errors.New("no openai credentials found")
			},
		},
	})

	openai := requireReadinessProvider(t, r.ReadinessReport(), providerOpenAI)
	assert.False(t, openai.Requested)
	assert.False(t, openai.Registered)
	assert.Empty(t, logs.String())
}

func TestAutoRegisterWithConfigContextReport_DoesNotRequestBannedCatalogModelFromModelRole(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer

	r := autoRegisterWithFactoriesContext(context.Background(), AutoRegisterConfig{
		Logger:       slog.New(slog.NewTextHandler(&logs, nil)),
		DefaultModel: "planner",
		ModelRoles: map[string]ModelRole{
			"planner": {
				Preferred:    "gpt-4.1-mini",
				BannedModels: []string{"openai/gpt-4.1-mini"},
			},
		},
		DisableReadinessChecks: true,
	}, []providerRegistration{
		{
			name:         providerOpenAI,
			staticModels: []string{"gpt-4.1-mini"},
			factory: func() (Provider, error) {
				return nil, errors.New("no openai credentials found")
			},
		},
	})

	openai := requireReadinessProvider(t, r.ReadinessReport(), providerOpenAI)
	assert.False(t, openai.Requested)
	assert.False(t, openai.Registered)
	assert.Empty(t, logs.String())
}

func TestAutoRegisterWithConfigContextReport_AppliesBannedCatalogModelToNestedModelRole(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer

	r := autoRegisterWithFactoriesContext(context.Background(), AutoRegisterConfig{
		Logger:       slog.New(slog.NewTextHandler(&logs, nil)),
		DefaultModel: "planner",
		ModelRoles: map[string]ModelRole{
			"planner": {
				Preferred:    "writer",
				BannedModels: []string{"openai/gpt-4.1-mini"},
			},
			"writer": {
				Preferred: "gpt-4.1-mini",
			},
		},
		DisableReadinessChecks: true,
	}, []providerRegistration{
		{
			name:         providerOpenAI,
			staticModels: []string{"gpt-4.1-mini"},
			factory: func() (Provider, error) {
				return nil, errors.New("no openai credentials found")
			},
		},
	})

	openai := requireReadinessProvider(t, r.ReadinessReport(), providerOpenAI)
	assert.False(t, openai.Requested)
	assert.False(t, openai.Registered)
	assert.Empty(t, logs.String())
}

func TestAutoRegisterWithConfigContextReport_MarksProviderRequestedByNestedModelRole(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer

	r := autoRegisterWithFactoriesContext(context.Background(), AutoRegisterConfig{
		Logger:       slog.New(slog.NewTextHandler(&logs, nil)),
		DefaultModel: "planner",
		ModelRoles: map[string]ModelRole{
			"planner": {
				Preferred:      "openai/gpt-4.1-mini",
				FallbackModels: []string{"writer"},
			},
			"writer": {
				Preferred: "anthropic/claude-sonnet-4-20250514",
			},
		},
		DisableReadinessChecks: true,
	}, []providerRegistration{
		{
			name:         providerAnthropic,
			staticModels: []string{"claude-sonnet-4-20250514"},
			factory: func() (Provider, error) {
				return nil, errors.New("no anthropic credentials found")
			},
		},
	})

	report := r.ReadinessReport()
	anthropic := requireReadinessProvider(t, report, providerAnthropic)
	assert.Equal(t, ProviderStatusMissingCredential, anthropic.Status)
	assert.True(t, anthropic.Requested)
	assert.False(t, anthropic.Registered)
}

func TestAutoRegisterWithConfigContextReport_MarksProviderRequestedByModelRolePreferredProvider(t *testing.T) {
	t.Parallel()

	r := autoRegisterWithFactoriesContext(context.Background(), AutoRegisterConfig{
		DefaultModel: "planner",
		ModelRoles: map[string]ModelRole{
			"planner": {
				Preferred:          "llama-3.3-70b-versatile",
				PreferredProviders: []string{"groq"},
			},
		},
		DisableReadinessChecks: true,
	}, []providerRegistration{
		{
			name:         "groq",
			staticModels: []string{"llama-3.3-70b-versatile"},
			factory: func() (Provider, error) {
				return nil, errors.New("no groq credentials found")
			},
		},
	})

	groq := requireReadinessProvider(t, r.ReadinessReport(), "groq")
	assert.Equal(t, ProviderStatusMissingCredential, groq.Status)
	assert.True(t, groq.Requested)
	assert.False(t, groq.Registered)
}

func TestAutoRegisterWithConfigContextReport_MarksProviderRequestedByNestedModelRoleRoutingPolicy(t *testing.T) {
	t.Parallel()

	r := autoRegisterWithFactoriesContext(context.Background(), AutoRegisterConfig{
		DefaultModel: "planner",
		ModelRoles: map[string]ModelRole{
			"planner": {
				Preferred:      "openai/gpt-4.1-mini",
				FallbackModels: []string{"summarizer"},
			},
			"summarizer": {
				Preferred: "command-r-plus",
				RoutingPolicy: modelroute.Policy{
					PreferredProviders: []string{"cohere"},
				},
			},
		},
		DisableReadinessChecks: true,
	}, []providerRegistration{
		{
			name:         "cohere",
			staticModels: []string{"command-r-plus"},
			factory: func() (Provider, error) {
				return nil, errors.New("no cohere credentials found")
			},
		},
	})

	cohere := requireReadinessProvider(t, r.ReadinessReport(), "cohere")
	assert.Equal(t, ProviderStatusMissingCredential, cohere.Status)
	assert.True(t, cohere.Requested)
	assert.False(t, cohere.Registered)
}

func TestAutoRegisterWithConfigContextReport_LogsMissingCredentialsOnlyWhenVisible(t *testing.T) {
	t.Parallel()

	registration := providerRegistration{
		name:         alphaProvider,
		staticModels: []string{"a-1"},
		factory: func() (Provider, error) {
			return nil, errors.New("no alpha credentials found")
		},
	}

	var quiet bytes.Buffer
	autoRegisterWithFactoriesContext(context.Background(), AutoRegisterConfig{
		Logger:                 slog.New(slog.NewTextHandler(&quiet, nil)),
		DisableReadinessChecks: true,
	}, []providerRegistration{registration})
	assert.Empty(t, quiet.String())

	var visible bytes.Buffer
	autoRegisterWithFactoriesContext(context.Background(), AutoRegisterConfig{
		Logger: slog.New(slog.NewTextHandler(&visible, nil)),
		Providers: map[string]ProviderConfig{
			alphaProvider: {},
		},
		DisableReadinessChecks: true,
	}, []providerRegistration{registration})
	assert.Contains(t, visible.String(), "level=WARN")
	assert.Contains(t, visible.String(), "llm provider unavailable")
	assert.Contains(t, visible.String(), "no alpha credentials found")
}

func TestAutoRegisterWithConfigContextReport_ReportsConfiguredBrokenProviderHealth(t *testing.T) {
	t.Parallel()

	provider := &fakeProvider{
		name:           alphaProvider,
		models:         []string{"a-1"},
		healthCheckErr: errors.New("bad token"),
		resp:           &Response{},
	}
	r := autoRegisterWithFactoriesContext(context.Background(), AutoRegisterConfig{
		Providers: map[string]ProviderConfig{
			alphaProvider: {},
		},
		ReadinessCheckTimeout: time.Second,
		ReadinessCacheTTL:     time.Minute,
	}, []providerRegistration{
		{
			name:         alphaProvider,
			staticModels: provider.models,
			factory: func() (Provider, error) {
				return provider, nil
			},
		},
	})

	entry := requireReadinessProvider(t, r.ReadinessReport(), alphaProvider)
	assert.Equal(t, ProviderStatusFailedHealthCheck, entry.Status)
	assert.True(t, entry.Registered)
	assert.True(t, entry.Configured)
	assert.True(t, entry.HealthChecked)
	assert.False(t, entry.Healthy)
	assert.Equal(t, "bad token", entry.HealthError.Error())
	assert.Equal(t, 1, provider.healthCalls)
}

func TestAutoRegisterWithConfigContextReport_ReportsLiveModelFetchFailure(t *testing.T) {
	t.Parallel()

	provider := &fakeProvider{
		name:     providerOpenAI,
		models:   []string{"gpt-static"},
		fetchErr: errors.New("models unavailable"),
		resp:     &Response{},
	}
	r := autoRegisterWithFactoriesContext(context.Background(), AutoRegisterConfig{
		Providers: map[string]ProviderConfig{
			providerOpenAI: {},
		},
		ReadinessCheckTimeout: time.Second,
		ReadinessCacheTTL:     time.Minute,
	}, []providerRegistration{
		{
			name:         providerOpenAI,
			staticModels: provider.models,
			factory: func() (Provider, error) {
				return provider, nil
			},
		},
	})

	entry := requireReadinessProvider(t, r.ReadinessReport(), providerOpenAI)
	assert.Equal(t, ProviderStatusFailedHealthCheck, entry.Status)
	assert.Equal(t, ModelCatalogSourceStatic, entry.ModelCatalogSource)
	assert.Equal(t, []string{"gpt-static"}, entry.Models)
	assert.True(t, entry.ModelsStale)
	assert.Equal(t, "models unavailable", entry.ModelFetchError.Error())
	assert.Equal(t, 1, provider.fetchCalls)
}

func TestAutoRegisterWithConfigContextReport_ReportsDefaultModelProviderMismatch(t *testing.T) {
	t.Parallel()

	r := autoRegisterWithFactoriesContext(context.Background(), AutoRegisterConfig{
		DefaultProvider:        alphaProvider,
		DefaultModel:           "beta/" + betaModel,
		DisableReadinessChecks: true,
	}, []providerRegistration{
		{
			name:         alphaProvider,
			staticModels: []string{"a-1"},
			factory: func() (Provider, error) {
				return &fakeProvider{name: alphaProvider, models: []string{"a-1"}, resp: &Response{Content: "ok"}}, nil
			},
		},
	})

	report := r.ReadinessReport()
	require.Error(t, report.Default.ModelError)
	assert.Contains(t, report.Default.ModelError.Error(), "not default provider")

	resp, err := r.Complete(context.Background(), CompleteParams{})
	require.NoError(t, err)
	assert.Equal(t, "a-1", resp.Model)
}

func TestAutoRegisterWithConfigContextReport_ReportsDefaultProviderBareModelMismatch(t *testing.T) {
	t.Parallel()

	openAIProvider := &fakeProvider{
		name:   providerOpenAI,
		models: []string{"gpt-static"},
		resp:   &Response{Content: "from-openai"},
	}
	anthropicProvider := &fakeProvider{
		name:   providerAnthropic,
		models: []string{"claude-static"},
		resp:   &Response{Content: "from-anthropic"},
	}

	r := autoRegisterWithFactoriesContext(context.Background(), AutoRegisterConfig{
		DefaultProvider:        providerAnthropic,
		DefaultModel:           "gpt-static",
		DisableReadinessChecks: true,
	}, []providerRegistration{
		{
			name:         providerOpenAI,
			staticModels: openAIProvider.models,
			factory: func() (Provider, error) {
				return openAIProvider, nil
			},
		},
		{
			name:         providerAnthropic,
			staticModels: anthropicProvider.models,
			factory: func() (Provider, error) {
				return anthropicProvider, nil
			},
		},
	})

	report := r.ReadinessReport()
	require.Error(t, report.Default.ModelError)
	assert.Contains(t, report.Default.ModelError.Error(), "not default provider")

	resp, err := r.Complete(context.Background(), CompleteParams{})
	require.NoError(t, err)
	assert.Equal(t, "from-anthropic", resp.Content)
	assert.Equal(t, "claude-static", resp.Model)
}

func TestAutoRegisterWithConfigContextReport_ReportsDefaultProviderAmbiguousBareModelMismatch(t *testing.T) {
	t.Parallel()

	openAIProvider := &fakeProvider{
		name:   providerOpenAI,
		models: []string{"shared"},
		resp:   &Response{Content: "from-openai"},
	}
	anthropicProvider := &fakeProvider{
		name:   providerAnthropic,
		models: []string{"shared"},
		resp:   &Response{Content: "from-anthropic"},
	}
	codexProvider := &fakeProvider{
		name:   providerCodex,
		models: []string{"codex-static"},
		resp:   &Response{Content: "from-codex"},
	}

	r := autoRegisterWithFactoriesContext(context.Background(), AutoRegisterConfig{
		DefaultProvider:        providerCodex,
		DefaultModel:           "shared",
		DisableReadinessChecks: true,
	}, []providerRegistration{
		{
			name:         providerOpenAI,
			staticModels: openAIProvider.models,
			factory: func() (Provider, error) {
				return openAIProvider, nil
			},
		},
		{
			name:         providerAnthropic,
			staticModels: anthropicProvider.models,
			factory: func() (Provider, error) {
				return anthropicProvider, nil
			},
		},
		{
			name:         providerCodex,
			staticModels: codexProvider.models,
			factory: func() (Provider, error) {
				return codexProvider, nil
			},
		},
	})

	report := r.ReadinessReport()
	require.Error(t, report.Default.ModelError)
	assert.Contains(t, report.Default.ModelError.Error(), "not default provider")
	assert.Contains(t, report.Default.ModelError.Error(), providerAnthropic)
	assert.Contains(t, report.Default.ModelError.Error(), providerOpenAI)

	resp, err := r.Complete(context.Background(), CompleteParams{})
	require.NoError(t, err)
	assert.Equal(t, "from-codex", resp.Content)
	assert.Equal(t, "codex-static", resp.Model)
}

func TestAutoRegisterWithConfigContextReport_QualifiedModelRequestsOnlyNamedProvider(t *testing.T) {
	t.Parallel()

	r := autoRegisterWithFactoriesContext(context.Background(), AutoRegisterConfig{
		SelectedModel:          providerCodex + "/gpt-5.5",
		DisableReadinessChecks: true,
	}, []providerRegistration{
		{
			name:         providerOpenAI,
			staticModels: []string{"gpt-static"},
			factory: func() (Provider, error) {
				return nil, errors.New("no OpenAI credentials found")
			},
		},
		{
			name:         providerCodex,
			staticModels: []string{"gpt-5.5"},
			factory: func() (Provider, error) {
				return nil, errors.New("no Codex credentials found")
			},
		},
	})

	report := r.ReadinessReport()
	openai := requireReadinessProvider(t, report, providerOpenAI)
	assert.False(t, openai.Requested)

	codex := requireReadinessProvider(t, report, providerCodex)
	assert.True(t, codex.Requested)
}

func TestAutoRegisterWithConfigContextReport_AppliesConfiguredModelAlias(t *testing.T) {
	t.Parallel()

	provider := &fakeProvider{
		name:   providerOpenAI,
		models: []string{"gpt-4.1-mini"},
		resp:   &Response{Content: "ok"},
	}
	r := autoRegisterWithFactoriesContext(context.Background(), AutoRegisterConfig{
		DefaultModel:           "fast",
		ModelAliases:           map[string]string{"fast": providerOpenAI + "/gpt-4.1-mini"},
		DisableReadinessChecks: true,
	}, []providerRegistration{
		{
			name:         providerOpenAI,
			staticModels: provider.models,
			factory: func() (Provider, error) {
				return provider, nil
			},
		},
	})

	entry := requireReadinessProvider(t, r.ReadinessReport(), providerOpenAI)
	assert.True(t, entry.Requested)

	resp, err := r.Complete(context.Background(), CompleteParams{})
	require.NoError(t, err)
	assert.Equal(t, "gpt-4.1-mini", resp.Model)

	diagnostic := r.ExplainModelResolution("fast")
	require.NoError(t, diagnostic.Error)
	assert.Equal(t, providerOpenAI, diagnostic.ProviderName)
	assert.Equal(t, "gpt-4.1-mini", diagnostic.ProviderModel)
	assert.Equal(t, ModelProvenanceConfiguredAlias, diagnostic.Provenance)
}

func TestAutoRegisterWithConfigContextReport_DefaultExactSlashModelUsesCatalogClaim(t *testing.T) {
	t.Parallel()

	const model = "namespace/model"

	provider := &fakeProvider{
		name:   providerOpenAI,
		models: []string{model},
		resp:   &Response{Content: "ok"},
	}
	r := autoRegisterWithFactoriesContext(context.Background(), AutoRegisterConfig{
		DefaultProvider:        providerOpenAI,
		DefaultModel:           model,
		DisableReadinessChecks: true,
	}, []providerRegistration{
		{
			name:         providerOpenAI,
			staticModels: provider.models,
			factory: func() (Provider, error) {
				return provider, nil
			},
		},
	})

	report := r.ReadinessReport()
	require.NoError(t, report.Default.ModelError)

	resp, err := r.Complete(context.Background(), CompleteParams{})
	require.NoError(t, err)
	assert.Equal(t, model, resp.Model)

	diagnostic := r.ExplainModelResolution("")
	require.NoError(t, diagnostic.Error)
	assert.Equal(t, providerOpenAI, diagnostic.ProviderName)
	assert.Equal(t, model, diagnostic.ProviderModel)
	assert.Equal(t, ModelProvenanceStatic, diagnostic.Provenance)
}

func TestAutoRegisterWithConfigContextReport_SelectedConfiguredModelAliasRequestsProvider(t *testing.T) {
	t.Parallel()

	provider := &fakeProvider{
		name:   providerOpenAI,
		models: []string{"gpt-4.1-mini"},
		resp:   &Response{Content: "ok"},
	}
	r := autoRegisterWithFactoriesContext(context.Background(), AutoRegisterConfig{
		SelectedModel:          "fast",
		ModelAliases:           map[string]string{"fast": providerOpenAI + "/gpt-4.1-mini"},
		DisableReadinessChecks: true,
	}, []providerRegistration{
		{
			name:         providerOpenAI,
			staticModels: provider.models,
			factory: func() (Provider, error) {
				return provider, nil
			},
		},
	})

	entry := requireReadinessProvider(t, r.ReadinessReport(), providerOpenAI)
	assert.True(t, entry.Requested)

	resp, err := r.Complete(context.Background(), CompleteParams{Model: "fast"})
	require.NoError(t, err)
	assert.Equal(t, "gpt-4.1-mini", resp.Model)
}

func TestAutoRegisterWithConfigContextReport_FallbackConfiguredModelAliasRequestsProvider(t *testing.T) {
	t.Parallel()

	primaryProvider := &fakeProvider{
		err:    errors.New("primary unavailable"),
		name:   alphaProvider,
		models: []string{"a-1"},
		resp:   &Response{Content: "primary"},
	}
	fallbackProvider := &fakeProvider{
		name:   providerOpenAI,
		models: []string{"gpt-4.1-mini"},
		resp:   &Response{Content: "fallback"},
	}

	r := autoRegisterWithFactoriesContext(context.Background(), AutoRegisterConfig{
		FallbackModels:         []string{" fast "},
		ModelAliases:           map[string]string{"fast": providerOpenAI + "/gpt-4.1-mini"},
		DisableReadinessChecks: true,
	}, []providerRegistration{
		{
			name:         alphaProvider,
			staticModels: primaryProvider.models,
			factory: func() (Provider, error) {
				return primaryProvider, nil
			},
		},
		{
			name:         providerOpenAI,
			staticModels: fallbackProvider.models,
			factory: func() (Provider, error) {
				return fallbackProvider, nil
			},
		},
	})

	entry := requireReadinessProvider(t, r.ReadinessReport(), providerOpenAI)
	assert.True(t, entry.Requested)

	resp, err := r.CompleteWithFallback(context.Background(), CompleteParams{Model: "a-1"}, []string{"fast"})
	require.NoError(t, err)
	assert.Equal(t, "fallback", resp.Content)
	assert.Equal(t, "gpt-4.1-mini", resp.Model)
	require.Len(t, fallbackProvider.calls, 1)
	assert.Equal(t, "gpt-4.1-mini", fallbackProvider.calls[0].Model)
}

func TestAutoRegisterWithConfigContextReport_SelectedBareModelUsesLiveCatalog(t *testing.T) {
	t.Parallel()

	provider := &fakeProvider{
		name:          providerOpenAI,
		models:        []string{"gpt-static"},
		fetchedModels: []string{"gpt-live-only"},
		resp:          &Response{Content: "ok"},
	}
	r := autoRegisterWithFactoriesContext(context.Background(), AutoRegisterConfig{
		SelectedModel:          "gpt-live-only",
		ReadinessCheckTimeout:  time.Second,
		ReadinessCacheTTL:      time.Minute,
		DisableReadinessChecks: false,
	}, []providerRegistration{
		{
			name:         providerOpenAI,
			staticModels: provider.models,
			factory: func() (Provider, error) {
				return provider, nil
			},
		},
	})

	entry := requireReadinessProvider(t, r.ReadinessReport(), providerOpenAI)
	assert.True(t, entry.Requested)
	assert.Equal(t, ModelCatalogSourceLive, entry.ModelCatalogSource)
	assert.Equal(t, []string{"gpt-live-only"}, entry.Models)

	diagnostic := r.ExplainModelResolution("gpt-live-only")
	require.NoError(t, diagnostic.Error)
	assert.Equal(t, providerOpenAI, diagnostic.ProviderName)
	assert.Equal(t, "gpt-live-only", diagnostic.ProviderModel)
	assert.Equal(t, ModelProvenanceFetchedLive, diagnostic.Provenance)

	resp, err := r.Complete(context.Background(), CompleteParams{Model: "gpt-live-only"})
	require.NoError(t, err)
	assert.Equal(t, "gpt-live-only", resp.Model)
}

func TestAutoRegisterWithConfigContextReport_ReportsDefaultModelAliasProviderMismatch(t *testing.T) {
	t.Parallel()

	openAIProvider := &fakeProvider{
		name:   providerOpenAI,
		models: []string{"gpt-static"},
		resp:   &Response{Content: "from-openai"},
	}
	anthropicProvider := &fakeProvider{
		name:   providerAnthropic,
		models: []string{"claude-static"},
		resp:   &Response{Content: "from-anthropic"},
	}

	r := autoRegisterWithFactoriesContext(context.Background(), AutoRegisterConfig{
		DefaultProvider:        providerOpenAI,
		DefaultModel:           "fast",
		ModelAliases:           map[string]string{"fast": providerAnthropic + "/claude-static"},
		DisableReadinessChecks: true,
	}, []providerRegistration{
		{
			name:         providerOpenAI,
			staticModels: openAIProvider.models,
			factory: func() (Provider, error) {
				return openAIProvider, nil
			},
		},
		{
			name:         providerAnthropic,
			staticModels: anthropicProvider.models,
			factory: func() (Provider, error) {
				return anthropicProvider, nil
			},
		},
	})

	report := r.ReadinessReport()
	require.Error(t, report.Default.ModelError)
	assert.Contains(t, report.Default.ModelError.Error(), "not default provider")

	resp, err := r.Complete(context.Background(), CompleteParams{})
	require.NoError(t, err)
	assert.Equal(t, "from-openai", resp.Content)
	assert.Equal(t, "gpt-static", resp.Model)
}

func TestAutoRegisterWithConfigContextReport_AppliesSelectedProviderModelOverride(t *testing.T) {
	t.Parallel()

	provider := &fakeProvider{
		name:   providerOpenAI,
		models: []string{"gpt-static"},
		resp:   &Response{Content: "ok"},
	}
	r := autoRegisterWithFactoriesContext(context.Background(), AutoRegisterConfig{
		SelectedModel:          providerOpenAI + "/private-deployment",
		DisableReadinessChecks: true,
	}, []providerRegistration{
		{
			name:         providerOpenAI,
			staticModels: provider.models,
			factory: func() (Provider, error) {
				return provider, nil
			},
		},
	})

	assert.True(t, r.ProviderHasModel(providerOpenAI, "private-deployment"))

	diagnostic := r.ExplainModelResolution("private-deployment")
	require.NoError(t, diagnostic.Error)
	assert.Equal(t, providerOpenAI, diagnostic.ProviderName)
	assert.Equal(t, "private-deployment", diagnostic.ProviderModel)
	assert.Equal(t, ModelProvenanceUserOverride, diagnostic.Provenance)

	catalog, err := r.ProviderModelCatalog(context.Background(), providerOpenAI)
	require.NoError(t, err)
	assert.Contains(t, catalog.Models, "private-deployment")
	assert.Equal(t, ModelProvenanceUserOverride, catalog.ModelProvenance["private-deployment"])
}

func TestAutoRegisterWithConfigContextReport_AppliesDefaultProviderPrivateModelOverride(t *testing.T) {
	t.Parallel()

	provider := &fakeProvider{
		name:   providerOpenAI,
		models: []string{"gpt-static"},
		resp:   &Response{Content: "ok"},
	}
	r := autoRegisterWithFactoriesContext(context.Background(), AutoRegisterConfig{
		DefaultProvider:        providerOpenAI,
		DefaultModel:           "private-deployment",
		DisableReadinessChecks: true,
	}, []providerRegistration{
		{
			name:         providerOpenAI,
			staticModels: provider.models,
			factory: func() (Provider, error) {
				return provider, nil
			},
		},
	})

	report := r.ReadinessReport()
	require.NoError(t, report.Default.ModelError)
	assert.True(t, r.ProviderHasModel(providerOpenAI, "private-deployment"))

	resp, err := r.Complete(context.Background(), CompleteParams{})
	require.NoError(t, err)
	assert.Equal(t, "private-deployment", resp.Model)

	diagnostic := r.ExplainModelResolution("")
	require.NoError(t, diagnostic.Error)
	assert.Equal(t, providerOpenAI, diagnostic.ProviderName)
	assert.Equal(t, "private-deployment", diagnostic.ProviderModel)
	assert.Equal(t, ModelProvenanceUserOverride, diagnostic.Provenance)
	assert.True(t, diagnostic.DefaultProviderConfigured)
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

func TestRegistry_CompleteDoesNotGuessProviderFromLegacyPrefix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		model     string
		providers []*fakeProvider
	}{
		{
			name:  "openai",
			model: "gpt-future",
			providers: []*fakeProvider{
				{name: providerOpenAI, models: []string{"gpt-known"}, resp: &Response{Content: "from-openai"}},
			},
		},
		{
			name:  "claude code preference",
			model: "claude-future",
			providers: []*fakeProvider{
				{name: providerAnthropic, models: []string{"claude-known"}, resp: &Response{Content: "from-anthropic"}},
				{name: providerClaudeCode, models: []string{"claude-code-known"}, resp: &Response{Content: "from-claude-code"}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := NewRegistry()
			for _, provider := range tt.providers {
				r.Register(provider)
			}

			_, err := r.Complete(context.Background(), CompleteParams{Model: tt.model})
			require.Error(t, err)
			require.ErrorContains(t, err, "unknown model")

			for _, provider := range tt.providers {
				assert.Empty(t, provider.calls)
			}

			diagnostic := r.ExplainModelResolution(tt.model)
			require.Error(t, diagnostic.Error)
			assert.Equal(t, "no registered provider catalog, live fetch, configured alias, or user override claims this bare model", diagnostic.Reason)
			assert.Empty(t, diagnostic.Candidates)
		})
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

func TestRegistry_CompleteRoutesExactCatalogModelContainingSlash(t *testing.T) {
	t.Parallel()

	const model = "namespace/model"

	r := NewRegistry()
	r.Register(&fakeProvider{
		name:   alphaProvider,
		models: []string{model},
		resp:   &Response{Content: "from-alpha"},
	})

	resp, err := r.Complete(context.Background(), CompleteParams{Model: model})
	require.NoError(t, err)
	assert.Equal(t, "from-alpha", resp.Content)
	assert.Equal(t, model, resp.Model)

	diagnostic := r.ExplainModelResolution(model)
	require.NoError(t, diagnostic.Error)
	assert.Equal(t, alphaProvider, diagnostic.ProviderName)
	assert.Equal(t, model, diagnostic.ProviderModel)
	assert.Equal(t, ModelProvenanceStatic, diagnostic.Provenance)
}

func TestRegistry_ProviderQualifiedSyntaxWinsOverExactSlashModelClaim(t *testing.T) {
	t.Parallel()

	const model = "openai/private"

	r := NewRegistry()
	r.Register(&fakeProvider{
		name:   providerOpenAI,
		models: []string{"openai-static"},
		resp:   &Response{Content: "from-openai"},
	})
	r.Register(&fakeProvider{
		name:   "beta",
		models: []string{model},
		resp:   &Response{Content: "from-beta"},
	})

	resp, err := r.Complete(context.Background(), CompleteParams{Model: model})
	require.NoError(t, err)
	assert.Equal(t, "from-openai", resp.Content)
	assert.Equal(t, "private", resp.Model)

	diagnostic := r.ExplainModelResolution(model)
	require.NoError(t, diagnostic.Error)
	assert.Equal(t, providerOpenAI, diagnostic.ProviderName)
	assert.Equal(t, "private", diagnostic.ProviderModel)
	assert.Equal(t, ModelProvenanceUserOverride, diagnostic.Provenance)
	assert.Equal(t, "provider-qualified model selected provider directly", diagnostic.Reason)

	resp, err = r.Complete(context.Background(), CompleteParams{Model: "beta/" + model})
	require.NoError(t, err)
	assert.Equal(t, "from-beta", resp.Content)
	assert.Equal(t, model, resp.Model)
}

func TestRegistry_ExactSlashModelCollisionRequiresDefaultProvider(t *testing.T) {
	t.Parallel()

	const model = "namespace/model"

	r := NewRegistry()
	r.Register(&fakeProvider{name: alphaProvider, models: []string{model}, resp: &Response{Content: "from-alpha"}})
	r.Register(&fakeProvider{name: "beta", models: []string{model}, resp: &Response{Content: "from-beta"}})

	_, err := r.Complete(context.Background(), CompleteParams{Model: model})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ambiguous model")

	diagnostic := r.ExplainModelResolution(model)
	require.Error(t, diagnostic.Error)
	assert.Contains(t, diagnostic.Reason, "model ID is ambiguous")
	require.Len(t, diagnostic.Candidates, 2)
	assert.Equal(t, model, diagnostic.Candidates[0].Model)
	assert.Equal(t, model, diagnostic.Candidates[1].Model)

	require.NoError(t, r.SetDefault("beta"))

	resp, err := r.Complete(context.Background(), CompleteParams{Model: model})
	require.NoError(t, err)
	assert.Equal(t, "from-beta", resp.Content)

	diagnostic = r.ExplainModelResolution(model)
	require.NoError(t, diagnostic.Error)
	assert.Equal(t, "beta", diagnostic.ProviderName)
	assert.Contains(t, diagnostic.Reason, "model ID is claimed by multiple providers")
}

func TestRegistry_CompleteAnnotatesResponseProvider(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	r.Register(&fakeProvider{
		name:   alphaProvider,
		models: []string{"a-1"},
		resp:   &Response{Content: "from-alpha"},
	})

	resp, err := r.Complete(context.Background(), CompleteParams{Model: alphaProvider + "/a-1"})
	require.NoError(t, err)

	assert.Equal(t, alphaProvider, resp.Provider)
	assert.Equal(t, "a-1", resp.Model)
}

func TestRegistry_CompleteAnnotatesMissingResponseModel(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	r.Register(&modelOmittingProvider{fakeProvider: fakeProvider{
		name:   alphaProvider,
		models: []string{"a-1"},
		resp:   &Response{Content: "from-alpha"},
	}})

	resp, err := r.Complete(context.Background(), CompleteParams{Model: alphaProvider + "/a-1"})
	require.NoError(t, err)

	assert.Equal(t, alphaProvider, resp.Provider)
	assert.Equal(t, "a-1", resp.Model)
}

func TestRegistry_CompleteProviderQualifiedLiveOnlyModel(t *testing.T) {
	t.Parallel()

	const liveModel = "claude-opus-4-6"

	r := NewRegistry()
	r.Register(&fakeProvider{
		name:   providerAnthropic,
		models: []string{"claude-sonnet-4-20250514"},
		resp:   &Response{Content: "from-anthropic"},
	})

	resp, err := r.Complete(context.Background(), CompleteParams{Model: providerAnthropic + "/" + liveModel})
	require.NoError(t, err)

	if resp.Model != liveModel {
		assert.Failf(t, "assertion failed", "model = %q, want %s", resp.Model, liveModel)
	}

	if resp.Content != "from-anthropic" {
		assert.Failf(t, "assertion failed", "content = %q, want from-anthropic", resp.Content)
	}
}

func TestRegistry_CompleteDoesNotGuessProviderFromClaudePrefix(t *testing.T) {
	t.Parallel()

	const liveModel = "claude-future"

	r := NewRegistry()
	anthropic := &fakeProvider{
		name:   providerAnthropic,
		models: []string{"claude-sonnet-4-20250514"},
		resp:   &Response{Content: "from-anthropic"},
	}
	claudeCode := &fakeProvider{
		name:   providerClaudeCode,
		models: []string{"claude-opus-4-6"},
		resp:   &Response{Content: "from-claude-code"},
	}

	r.Register(anthropic)
	r.Register(claudeCode)

	_, err := r.Complete(context.Background(), CompleteParams{Model: liveModel})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown model")
	assert.Empty(t, anthropic.calls)
	assert.Empty(t, claudeCode.calls)
}

func TestRegistry_CompleteDoesNotGuessProviderFromOpenAIPrefixes(t *testing.T) {
	t.Parallel()

	provider := &fakeProvider{
		name:   providerOpenAI,
		models: []string{"gpt-4.1"},
		resp:   &Response{Content: "from-openai"},
	}

	r := NewRegistry()
	r.Register(provider)

	for _, model := range []string{"gpt-future", "o1-future", "o3-future", "o4-future"} {
		_, err := r.Complete(context.Background(), CompleteParams{Model: model})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unknown model")
	}

	assert.Empty(t, provider.calls)
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

func TestRegistry_CompleteRejectsUnsupportedProviderParamsBeforeDispatch(t *testing.T) {
	t.Parallel()

	for _, providerName := range protocolProviderNames() {
		capabilities, ok := BuiltInProviderCapabilities(providerName)
		require.True(t, ok)

		for field, support := range capabilities.CompleteParams {
			if support.Status != CompleteParamUnsupported {
				continue
			}

			t.Run(providerName+"/"+field, func(t *testing.T) {
				t.Parallel()

				provider := &fakeProvider{
					name:   providerName,
					models: []string{"contract-model"},
					resp:   &Response{Content: "unexpected"},
				}

				r := NewRegistry()
				r.Register(provider)

				params := paramsWithOnlyFieldSet(t, field)
				params.Model = providerName + "/contract-model"

				_, err := r.Complete(context.Background(), params)
				require.Error(t, err)
				assert.Contains(t, err.Error(), "CompleteParams."+field+" is unsupported")
				assert.Empty(t, provider.calls)
			})
		}
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

func TestRegistry_CompleteRejectsUnsupportedDeclaredProviderParams(t *testing.T) {
	t.Parallel()

	provider := &capabilityFakeProvider{
		capabilities: ProviderCapabilities{
			SupportsChatCompletions: true,
			CompleteParams: map[string]CompleteParamSupport{
				"Tools": unsupported("custom endpoint does not support tools"),
			},
		},
		fakeProvider: fakeProvider{
			name:   "compatible",
			models: []string{"coder"},
			resp:   &Response{Content: "should not be called"},
		},
	}

	r := NewRegistry()
	r.Register(provider)

	_, err := r.Complete(context.Background(), CompleteParams{
		Model:    "compatible/coder",
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
		Tools:    DefaultTools(),
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "CompleteParams.Tools is unsupported")
	assert.Empty(t, provider.calls)
}

func TestRegistry_CompleteRejectsUnsupportedDeclaredProviderCapabilities(t *testing.T) {
	t.Parallel()

	provider := &capabilityFakeProvider{
		capabilities: ProviderCapabilities{
			SupportsChatCompletions: true,
		},
		fakeProvider: fakeProvider{
			name:   "compatible",
			models: []string{"coder"},
			resp:   &Response{Content: "should not be called"},
		},
	}

	r := NewRegistry()
	r.Register(provider)

	_, err := r.Complete(context.Background(), CompleteParams{
		Model:    "compatible/coder",
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
		Tools:    DefaultTools(),
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "CompleteParams.Tools is unsupported")
	assert.Contains(t, err.Error(), "provider capability metadata does not include tools")
	assert.Empty(t, provider.calls)
}

func TestRegistry_CompleteWithFallbackSkipsUnsupportedDeclaredProviderParams(t *testing.T) {
	t.Parallel()

	primary := &capabilityFakeProvider{
		capabilities: ProviderCapabilities{
			SupportsChatCompletions: true,
			CompleteParams: map[string]CompleteParamSupport{
				"Tools": unsupported("custom endpoint does not support tools"),
			},
		},
		fakeProvider: fakeProvider{
			name:   "compatible",
			models: []string{"coder"},
			resp:   &Response{Content: "should not be called"},
		},
	}
	fallback := &fakeProvider{
		name:   "toolhost",
		models: []string{"tool-coder"},
		resp:   &Response{Content: "tool fallback"},
	}

	r := NewRegistry()
	r.Register(primary)
	r.Register(fallback)

	resp, err := r.CompleteWithFallback(context.Background(), CompleteParams{
		Model:    "compatible/coder",
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
		Tools:    DefaultTools(),
	}, []string{"toolhost/tool-coder"})
	require.NoError(t, err)

	assert.Equal(t, "tool fallback", resp.Content)
	assert.Empty(t, primary.calls)
	require.Len(t, fallback.calls, 1)
	assert.Equal(t, "tool-coder", fallback.calls[0].Model)
}

func TestRegistry_CompleteWithFallbackNormalizesTemperaturePerProvider(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	codexProvider := &fakeProvider{
		err:    errors.New("codex offline"),
		name:   providerCodex,
		models: []string{"gpt-5.5"},
		resp:   &Response{},
	}
	claudeCodeProvider := &fakeProvider{
		name:   providerClaudeCode,
		models: []string{"claude-opus-4-7"},
		resp:   &Response{Content: "ok"},
	}

	r.Register(codexProvider)
	r.Register(claudeCodeProvider)

	var log bytes.Buffer

	ctx := events.WithEmitter(context.Background(), events.NewRunnerWithLogger(nil, &log), events.Event{})

	temperature := 0.2
	resp, err := r.CompleteWithFallback(ctx, CompleteParams{
		Model:          "codex/gpt-5.5",
		ReasoningLevel: "high",
		Temperature:    &temperature,
		MaxTokens:      2048,
		Messages:       []Message{{Role: RoleUser, Content: "hi"}},
	}, []string{"claude-code/claude-opus-4-7"})
	require.NoError(t, err)

	assert.Equal(t, "ok", resp.Content)
	require.Len(t, codexProvider.calls, 1)
	assert.Nil(t, codexProvider.calls[0].Temperature)
	assert.Zero(t, codexProvider.calls[0].MaxTokens)
	require.Len(t, claudeCodeProvider.calls, 1)
	require.NotNil(t, claudeCodeProvider.calls[0].Temperature)
	assert.InEpsilon(t, 1.0, *claudeCodeProvider.calls[0].Temperature, 0.0001)
	assert.Equal(t, 2048, claudeCodeProvider.calls[0].MaxTokens)

	logOutput := log.String()
	assert.Contains(t, logOutput, "option_adjustments")
	assert.Contains(t, logOutput, "Temperature omitted")
	assert.Contains(t, logOutput, "MaxTokens omitted")
	assert.Contains(t, logOutput, "Temperature coerced")
}

func TestRegistry_CompleteOmitsAnthropicReasoningWhenMaxTokensTooSmall(t *testing.T) {
	t.Parallel()

	provider := &fakeProvider{
		name:   providerClaudeCode,
		models: []string{"claude-haiku-4-5"},
		resp:   &Response{Content: "ok"},
	}

	r := NewRegistry()
	r.Register(provider)

	var log bytes.Buffer

	ctx := events.WithEmitter(context.Background(), events.NewRunnerWithLogger(nil, &log), events.Event{})

	temperature := 0.2
	resp, err := r.Complete(ctx, CompleteParams{
		Model:          "claude-code/claude-haiku-4-5",
		ReasoningLevel: "medium",
		Temperature:    &temperature,
		MaxTokens:      16,
		Messages:       []Message{{Role: RoleUser, Content: "hi"}},
	})
	require.NoError(t, err)

	assert.Equal(t, "ok", resp.Content)
	require.Len(t, provider.calls, 1)
	assert.Empty(t, provider.calls[0].ReasoningLevel)
	require.NotNil(t, provider.calls[0].Temperature)
	assert.InEpsilon(t, 0.2, *provider.calls[0].Temperature, 0.0001)
	assert.Contains(t, log.String(), "ReasoningLevel omitted")
}

func TestRegistry_CompleteWithFallbackRecordsRateLimitTelemetry(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	r.SetRetry(retryConfig{})

	telemetry := modelroute.NewTelemetry()
	r.SetRouteTelemetry(telemetry)
	r.Register(&fakeProvider{
		err:    &ProviderError{Provider: providerOpenAI, StatusCode: 429, RetryAfter: 2 * time.Second, Message: "rate limited"},
		name:   providerOpenAI,
		models: []string{"gpt-4.1-mini"},
		resp:   &Response{},
	})
	r.Register(&fakeProvider{
		name:   providerAnthropic,
		models: []string{"claude-sonnet-4-20250514"},
		resp:   &Response{Content: "ok"},
	})

	resp, err := r.CompleteWithFallback(context.Background(), CompleteParams{
		Model: "openai/gpt-4.1-mini",
	}, []string{"anthropic/claude-sonnet-4-20250514"})
	require.NoError(t, err)
	assert.Equal(t, "claude-sonnet-4-20250514", resp.Model)

	openAIObs, ok := telemetry.Snapshot("openai/gpt-4.1-mini")
	require.True(t, ok)
	assert.Equal(t, 1, openAIObs.FailureCount)
	assert.Equal(t, 1, openAIObs.RateLimitCount)
	assert.True(t, openAIObs.LastFailureRetryable)
	assert.True(t, openAIObs.LastFailureRateLimited)
	assert.Equal(t, 2000, openAIObs.LastRetryAfterMS)
	assert.Contains(t, openAIObs.LastError, "HTTP 429")

	anthropicObs, ok := telemetry.Snapshot("anthropic/claude-sonnet-4-20250514")
	require.True(t, ok)
	assert.Equal(t, 1, anthropicObs.Count)
}

func TestRegistry_CompleteWithModelRoleRoutesByCapabilitiesAndBudget(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	openAI := &fakeProvider{
		name:   providerOpenAI,
		models: []string{"gpt-4.1", "gpt-4.1-mini"},
		resp:   &Response{Content: "ok"},
	}
	r.Register(openAI)

	require.NoError(t, r.SetModelRole("planner", ModelRole{
		Preferred:            "openai/gpt-4.1",
		FallbackModels:       []string{"openai/gpt-4.1-mini"},
		RequiredCapabilities: []string{modelroute.CapabilityTools},
		MaxCostUSD:           0.0005,
	}))

	resp, err := r.CompleteWithFallback(context.Background(), CompleteParams{
		Model:     "planner",
		Messages:  []Message{{Role: RoleUser, Content: strings.Repeat("hello ", 100)}},
		MaxTokens: 100,
	}, nil)
	require.NoError(t, err)

	assert.Equal(t, "ok", resp.Content)
	assert.Equal(t, "gpt-4.1-mini", resp.Model)
	require.Len(t, openAI.calls, 1)
	assert.Equal(t, "gpt-4.1-mini", openAI.calls[0].Model)

	resolution, ok, err := r.ResolveModelRole("planner", CompleteParams{
		Messages:  []Message{{Role: RoleUser, Content: strings.Repeat("hello ", 100)}},
		MaxTokens: 100,
	}, nil)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "openai/gpt-4.1-mini", resolution.SelectedModel)
	assert.Contains(t, resolution.Decision.Constraints, modelroute.ConstraintRequiredCapabilities)
	assert.Contains(t, resolution.Decision.Constraints, modelroute.ConstraintBudget)
	assertRejectionContains(t, resolution.Decision, "openai/gpt-4.1", modelroute.ReasonOverBudget)
}

func TestRegistry_SetModelRoleRejectsNegativeLimits(t *testing.T) {
	t.Parallel()

	assertInvalid := func(name string, role ModelRole, wantErr string) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			r := NewRegistry()

			err := r.SetModelRole("planner", role)

			require.Error(t, err)
			assert.Contains(t, err.Error(), wantErr)
		})
	}

	assertInvalid("max cost", ModelRole{
		Preferred:  "openai/gpt-4.1-mini",
		MaxCostUSD: -0.01,
	}, "max_cost_usd must be >= 0")
	assertInvalid("non finite max cost", ModelRole{
		Preferred:  "openai/gpt-4.1-mini",
		MaxCostUSD: math.NaN(),
	}, "max_cost_usd must be finite")
	assertInvalid("max latency", ModelRole{
		Preferred:    "openai/gpt-4.1-mini",
		MaxLatencyMS: -1,
	}, "max_latency_ms must be >= 0")
	assertInvalid("max ttft", ModelRole{
		Preferred: "openai/gpt-4.1-mini",
		MaxTTFTMS: -1,
	}, "max_ttft_ms must be >= 0")
	assertInvalid("routing max budget", ModelRole{
		Preferred: "openai/gpt-4.1-mini",
		RoutingPolicy: modelroute.Policy{
			MaxBudget: -0.01,
		},
	}, "routing_policy.max_budget must be >= 0")
	assertInvalid("non finite routing max budget", ModelRole{
		Preferred: "openai/gpt-4.1-mini",
		RoutingPolicy: modelroute.Policy{
			MaxBudget: math.Inf(1),
		},
	}, "routing_policy.max_budget must be finite")
	assertInvalid("routing max latency", ModelRole{
		Preferred: "openai/gpt-4.1-mini",
		RoutingPolicy: modelroute.Policy{
			MaxLatencyMS: -1,
		},
	}, "routing_policy.max_latency_ms must be >= 0")
	assertInvalid("routing max ttft", ModelRole{
		Preferred: "openai/gpt-4.1-mini",
		RoutingPolicy: modelroute.Policy{
			MaxTTFTMS: -1,
		},
	}, "routing_policy.max_ttft_ms must be >= 0")
}

func TestRegistry_SetModelRoleRejectsUnknownCapabilities(t *testing.T) {
	t.Parallel()

	assertInvalid := func(name string, role ModelRole, wantErr string) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			r := NewRegistry()

			err := r.SetModelRole("planner", role)

			require.Error(t, err)
			assert.Contains(t, err.Error(), wantErr)
			assert.Contains(t, err.Error(), "valid: text,chat,tools")
		})
	}

	assertInvalid("role required capabilities", ModelRole{
		Preferred:            "openai/gpt-4.1-mini",
		RequiredCapabilities: []string{"tools", "clairvoyance"},
	}, `required_capabilities contains unknown capability "clairvoyance"`)
	assertInvalid("routing policy required capabilities", ModelRole{
		Preferred: "openai/gpt-4.1-mini",
		RoutingPolicy: modelroute.Policy{
			RequiredCapabilities: []string{"json_schema", "teleport"},
		},
	}, `routing_policy.required_capabilities contains unknown capability "teleport"`)
}

func TestRegistry_ResolveModelRoleWithPolicyRejectsUnknownCapabilities(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	require.NoError(t, r.SetModelRole("planner", ModelRole{
		Preferred: "openai/gpt-4.1-mini",
	}))

	resolution, ok, err := r.ResolveModelRoleWithPolicy(
		"planner",
		CompleteParams{},
		nil,
		modelroute.Policy{RequiredCapabilities: []string{"teleport"}},
	)

	require.Error(t, err)
	assert.True(t, ok)
	assert.Contains(t, err.Error(), `routing_policy.required_capabilities contains unknown capability "teleport"`)
	assert.Contains(t, err.Error(), "valid: text,chat,tools")
	assert.Equal(t, "planner", resolution.Decision.ModelRole)
	assert.Contains(t, resolution.Decision.Policy.RequiredCapabilities, "teleport")
}

func TestRegistry_ResolveModelRoleWithPolicyRejectsInvalidLimits(t *testing.T) {
	t.Parallel()

	assertInvalid := func(name string, policy modelroute.Policy, wantErr string) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			r := NewRegistry()
			require.NoError(t, r.SetModelRole("planner", ModelRole{
				Preferred: "openai/gpt-4.1-mini",
			}))

			resolution, ok, err := r.ResolveModelRoleWithPolicy(
				"planner",
				CompleteParams{},
				nil,
				policy,
			)

			require.Error(t, err)
			assert.True(t, ok)
			assert.Contains(t, err.Error(), wantErr)
			assert.Equal(t, "planner", resolution.Decision.ModelRole)
		})
	}

	assertInvalid("negative max budget", modelroute.Policy{
		MaxBudget: -0.01,
	}, "routing_policy.max_budget must be >= 0")
	assertInvalid("non finite max budget", modelroute.Policy{
		MaxBudget: math.NaN(),
	}, "routing_policy.max_budget must be finite")
	assertInvalid("negative latency", modelroute.Policy{
		MaxLatencyMS: -1,
	}, "routing_policy.max_latency_ms must be >= 0")
	assertInvalid("negative ttft", modelroute.Policy{
		MaxTTFTMS: -1,
	}, "routing_policy.max_ttft_ms must be >= 0")
}

func TestRegistry_ResolveModelRoleRejectsUnknownRequestCapabilities(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	require.NoError(t, r.SetModelRole("planner", ModelRole{
		Preferred: "openai/gpt-4.1-mini",
	}))

	resolution, ok, err := r.resolveModelRoleWithCapabilities(
		"planner",
		CompleteParams{},
		nil,
		[]string{"clairvoyance"},
	)

	require.Error(t, err)
	assert.True(t, ok)
	assert.Contains(t, err.Error(), `request.required_capabilities contains unknown capability "clairvoyance"`)
	assert.Contains(t, err.Error(), "valid: text,chat,tools")
	assert.Equal(t, "planner", resolution.Decision.ModelRole)
}

func TestRegistry_CompleteWithModelRoleFallsBackOnProviderFailure(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	r.SetRetry(retryConfig{})

	openAI := &fakeProvider{
		err:    errors.New("HTTP 503: unavailable"),
		name:   providerOpenAI,
		models: []string{"gpt-4.1-mini"},
	}
	anthropic := &fakeProvider{
		name:   providerAnthropic,
		models: []string{"claude-sonnet-4-20250514"},
		resp:   &Response{Content: "fallback ok"},
	}

	r.Register(openAI)
	r.Register(anthropic)

	require.NoError(t, r.SetModelRole("planner", ModelRole{
		Preferred:      "openai/gpt-4.1-mini",
		FallbackModels: []string{"anthropic/claude-sonnet-4-20250514"},
	}))

	resp, err := r.Complete(context.Background(), CompleteParams{
		Model:    "planner",
		Messages: []Message{{Role: RoleUser, Content: "plan"}},
	})
	require.NoError(t, err)

	assert.Equal(t, "fallback ok", resp.Content)
	assert.Equal(t, "anthropic", resp.Provider)
	assert.Equal(t, "claude-sonnet-4-20250514", resp.Model)
	require.Len(t, openAI.calls, 1)
	assert.Equal(t, "gpt-4.1-mini", openAI.calls[0].Model)
	require.Len(t, anthropic.calls, 1)
	assert.Equal(t, "claude-sonnet-4-20250514", anthropic.calls[0].Model)
}

func TestRegistry_CompleteUsesConfiguredDefaultModelRole(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	provider := &fakeProvider{
		name:   providerOpenAI,
		models: []string{"gpt-4.1-mini"},
		resp:   &Response{Content: "default role ok"},
	}

	r.Register(provider)

	require.NoError(t, r.SetModelRole("planner", ModelRole{
		Preferred: "openai/gpt-4.1-mini",
	}))
	require.NoError(t, r.SetDefaultModel("planner"))

	resp, err := r.Complete(context.Background(), CompleteParams{
		Messages: []Message{{Role: RoleUser, Content: "plan"}},
	})
	require.NoError(t, err)

	assert.Equal(t, "default role ok", resp.Content)
	assert.Equal(t, providerOpenAI, resp.Provider)
	assert.Equal(t, "gpt-4.1-mini", resp.Model)
	require.Len(t, provider.calls, 1)
	assert.Equal(t, "gpt-4.1-mini", provider.calls[0].Model)
}

func TestRegistry_ModelRolePreservesPreferredBeforeCheaperFallback(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	r.Register(&fakeProvider{
		name:   providerOpenAI,
		models: []string{"gpt-4.1", "gpt-4.1-mini"},
		resp:   &Response{Content: "ok"},
	})

	require.NoError(t, r.SetModelRole("planner", ModelRole{
		Preferred:      "openai/gpt-4.1",
		FallbackModels: []string{"openai/gpt-4.1-mini"},
	}))

	resolution, ok, err := r.ResolveModelRole("planner", CompleteParams{
		Messages:  []Message{{Role: RoleUser, Content: "plan"}},
		MaxTokens: 10,
	}, nil)

	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "openai/gpt-4.1", resolution.SelectedModel)
	assert.Equal(t, []string{"openai/gpt-4.1", "openai/gpt-4.1-mini"}, resolution.Decision.FallbackOrder)
}

func TestRegistry_ModelRoleUsesCatalogMetadataAfterDefaultProviderDisambiguatesBareModel(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	openAI := &fakeProvider{
		name:   providerOpenAI,
		models: []string{"gpt-5.4-mini"},
		resp:   &Response{Content: "openai"},
	}
	codex := &fakeProvider{
		name:   providerCodex,
		models: []string{"gpt-5.4-mini"},
		resp:   &Response{Content: "codex"},
	}

	r.Register(openAI)
	r.Register(codex)
	require.NoError(t, r.SetDefault(providerOpenAI))
	require.NoError(t, r.SetModelRole("planner", ModelRole{
		Preferred:  "gpt-5.4-mini",
		MaxCostUSD: 0.01,
	}))

	resolution, ok, err := r.ResolveModelRole("planner", CompleteParams{
		Messages:  []Message{{Role: RoleUser, Content: "plan"}},
		MaxTokens: 100,
	}, nil)

	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "openai/gpt-5.4-mini", resolution.SelectedModel)
	selected := requireDecisionCandidate(t, resolution.Decision, "openai/gpt-5.4-mini")
	assert.Greater(t, selected.Candidate.InputTokenCost, 0.0)
	assert.Greater(t, selected.Candidate.OutputTokenCost, 0.0)
	assert.NotContains(t, selected.Rejected, modelroute.ReasonCostUnknown)

	resp, err := r.CompleteWithFallback(context.Background(), CompleteParams{
		Model:     "planner",
		Messages:  []Message{{Role: RoleUser, Content: "plan"}},
		MaxTokens: 100,
	}, nil)
	require.NoError(t, err)

	assert.Equal(t, "openai", resp.Content)
	require.Len(t, openAI.calls, 1)
	assert.Empty(t, codex.calls)
}

func TestRegistry_ModelRoleReportsAmbiguousBareCatalogModel(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	r.Register(&fakeProvider{
		name:   providerOpenAI,
		models: []string{"gpt-5.4-mini"},
		resp:   &Response{Content: "openai"},
	})
	r.Register(&fakeProvider{
		name:   providerCodex,
		models: []string{"gpt-5.4-mini"},
		resp:   &Response{Content: "codex"},
	})

	require.NoError(t, r.SetModelRole("planner", ModelRole{
		Preferred: "gpt-5.4-mini",
	}))

	resolution, ok, err := r.ResolveModelRole("planner", CompleteParams{
		Messages: []Message{{Role: RoleUser, Content: "plan"}},
	}, nil)

	require.Error(t, err)
	require.True(t, ok)
	assert.Empty(t, resolution.SelectedModel)
	assertRejectionContains(t, resolution.Decision, "gpt-5.4-mini", modelroute.ReasonAmbiguousMetadata)
	assertRejectionDoesNotContain(t, resolution.Decision, "gpt-5.4-mini", modelroute.ReasonModelUnavailable)
	assert.NotContains(t, formatModelRoleRejections(resolution.Decision), modelroute.ReasonUnknownMetadata)
}

func TestRegistry_ModelRolePreferredProvidersDisambiguateBareCatalogModel(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	openAI := &fakeProvider{
		name:   providerOpenAI,
		models: []string{"gpt-5.4-mini"},
		resp:   &Response{Content: "openai"},
	}
	codex := &fakeProvider{
		name:   providerCodex,
		models: []string{"gpt-5.4-mini"},
		resp:   &Response{Content: "codex"},
	}

	r.Register(openAI)
	r.Register(codex)
	require.NoError(t, r.SetModelRole("planner", ModelRole{
		Preferred:          "gpt-5.4-mini",
		PreferredProviders: []string{providerCodex},
	}))

	resp, err := r.CompleteWithFallback(context.Background(), CompleteParams{
		Model:    "planner",
		Messages: []Message{{Role: RoleUser, Content: "plan"}},
	}, nil)
	require.NoError(t, err)

	assert.Equal(t, "codex", resp.Content)
	assert.Empty(t, openAI.calls)
	require.Len(t, codex.calls, 1)
	assert.Equal(t, "gpt-5.4-mini", codex.calls[0].Model)

	resolution, ok, err := r.ResolveModelRole("planner", CompleteParams{
		Messages: []Message{{Role: RoleUser, Content: "plan"}},
	}, nil)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "codex/gpt-5.4-mini", resolution.SelectedModel)
	assert.Contains(t, resolution.Decision.Constraints, modelroute.ConstraintProviderPreference)
	assert.Empty(t, resolution.Decision.Candidates[0].Rejected)
}

func TestRegistry_ModelRolePreferredProvidersDisambiguateBareRuntimeModel(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	groq := &capabilityFakeProvider{
		capabilities: ProviderCapabilities{SupportsChatCompletions: true},
		fakeProvider: fakeProvider{
			name:   "groq",
			models: []string{"llama-3.3-70b-versatile"},
			resp:   &Response{Content: "groq"},
		},
	}
	openRouter := &capabilityFakeProvider{
		capabilities: ProviderCapabilities{SupportsChatCompletions: true},
		fakeProvider: fakeProvider{
			name:   "openrouter",
			models: []string{"llama-3.3-70b-versatile"},
			resp:   &Response{Content: "openrouter"},
		},
	}

	r.Register(groq)
	r.Register(openRouter)
	require.NoError(t, r.SetModelRole("fast_coder", ModelRole{
		Preferred:          "llama-3.3-70b-versatile",
		PreferredProviders: []string{"groq"},
	}))

	resp, err := r.CompleteWithFallback(context.Background(), CompleteParams{
		Model:    "fast_coder",
		Messages: []Message{{Role: RoleUser, Content: "implement"}},
	}, nil)
	require.NoError(t, err)

	assert.Equal(t, "groq", resp.Content)
	require.Len(t, groq.calls, 1)
	assert.Equal(t, "llama-3.3-70b-versatile", groq.calls[0].Model)
	assert.Empty(t, openRouter.calls)

	resolution, ok, err := r.ResolveModelRole("fast_coder", CompleteParams{
		Messages: []Message{{Role: RoleUser, Content: "implement"}},
	}, nil)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "groq/llama-3.3-70b-versatile", resolution.SelectedModel)
	assert.Contains(t, resolution.Decision.Constraints, modelroute.ConstraintProviderPreference)
	require.Len(t, resolution.Decision.Candidates, 1)
	assert.Equal(t, "runtime registry", resolution.Decision.Candidates[0].Candidate.MetadataSource)
}

func TestRegistry_ModelRoleReportsAmbiguousBareRuntimeModel(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	r.Register(&fakeProvider{
		name:   "groq",
		models: []string{"llama-3.3-70b-versatile"},
		resp:   &Response{Content: "groq"},
	})
	r.Register(&fakeProvider{
		name:   "openrouter",
		models: []string{"llama-3.3-70b-versatile"},
		resp:   &Response{Content: "openrouter"},
	})

	require.NoError(t, r.SetModelRole("fast_coder", ModelRole{
		Preferred: "llama-3.3-70b-versatile",
	}))

	resolution, ok, err := r.ResolveModelRole("fast_coder", CompleteParams{
		Messages: []Message{{Role: RoleUser, Content: "implement"}},
	}, nil)

	require.Error(t, err)
	require.True(t, ok)
	assert.Empty(t, resolution.SelectedModel)
	assertRejectionContains(t, resolution.Decision, "llama-3.3-70b-versatile", modelroute.ReasonAmbiguousMetadata)
	assert.NotContains(t, formatModelRoleRejections(resolution.Decision), modelroute.ReasonUnknownMetadata)
}

func TestRegistry_ModelRoleReportsBareRuntimeCollisionWithCatalogModel(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	openAI := &fakeProvider{
		name:   providerOpenAI,
		models: []string{"gpt-4.1-mini"},
		resp:   &Response{Content: "openai"},
	}
	groq := &capabilityFakeProvider{
		capabilities: ProviderCapabilities{SupportsChatCompletions: true},
		fakeProvider: fakeProvider{
			name:   "groq",
			models: []string{"gpt-4.1-mini"},
			resp:   &Response{Content: "groq"},
		},
	}

	r.Register(openAI)
	r.Register(groq)
	require.NoError(t, r.SetModelRole("planner", ModelRole{
		Preferred: "gpt-4.1-mini",
	}))

	_, err := r.CompleteWithFallback(context.Background(), CompleteParams{
		Model:    "planner",
		Messages: []Message{{Role: RoleUser, Content: "plan"}},
	}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), modelroute.ReasonAmbiguousMetadata)
	assert.Empty(t, openAI.calls)
	assert.Empty(t, groq.calls)

	resolution, ok, err := r.ResolveModelRole("planner", CompleteParams{
		Messages: []Message{{Role: RoleUser, Content: "plan"}},
	}, nil)
	require.Error(t, err)
	require.True(t, ok)
	assert.Empty(t, resolution.SelectedModel)
	assertRejectionContains(t, resolution.Decision, "gpt-4.1-mini", modelroute.ReasonAmbiguousMetadata)
}

func TestRegistry_ModelRoleBudgetRejectsAmbiguousUnpricedRuntimeModels(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	alpha := &capabilityFakeProvider{
		capabilities: ProviderCapabilities{SupportsChatCompletions: true},
		fakeProvider: fakeProvider{
			name:   alphaProvider,
			models: []string{"live-only"},
			resp:   &Response{Content: "alpha"},
		},
	}
	beta := &capabilityFakeProvider{
		capabilities: ProviderCapabilities{SupportsChatCompletions: true},
		fakeProvider: fakeProvider{
			name:   "beta",
			models: []string{"live-only"},
			resp:   &Response{Content: "beta"},
		},
	}

	r.Register(alpha)
	r.Register(beta)
	require.NoError(t, r.SetModelRole("planner", ModelRole{
		Preferred:  "live-only",
		MaxCostUSD: 0.01,
	}))

	_, err := r.CompleteWithFallback(context.Background(), CompleteParams{
		Model:    "planner",
		Messages: []Message{{Role: RoleUser, Content: "plan"}},
	}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), modelroute.ReasonCostUnknown)
	assert.Empty(t, alpha.calls)
	assert.Empty(t, beta.calls)

	resolution, ok, err := r.ResolveModelRole("planner", CompleteParams{
		Messages: []Message{{Role: RoleUser, Content: "plan"}},
	}, nil)
	require.Error(t, err)
	require.True(t, ok)
	assert.Empty(t, resolution.SelectedModel)
	assertRejectionContains(t, resolution.Decision, "alpha/live-only", modelroute.ReasonCostUnknown)
	assertRejectionContains(t, resolution.Decision, "beta/live-only", modelroute.ReasonCostUnknown)
	assert.NotContains(t, formatModelRoleRejections(resolution.Decision), modelroute.ReasonAmbiguousMetadata)
}

func TestRegistry_ModelRoleBudgetPolicyCanOverrideDefaultProviderCollision(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	openAI := &fakeProvider{
		name:   providerOpenAI,
		models: []string{"gpt-4.1-mini"},
		resp:   &Response{Content: "openai"},
	}
	ollama := &localCapabilityFakeProvider{
		capabilityFakeProvider: capabilityFakeProvider{
			capabilities: ProviderCapabilities{SupportsChatCompletions: true},
			fakeProvider: fakeProvider{
				name:   providerOllama,
				models: []string{"gpt-4.1-mini"},
				resp:   &Response{Content: "local"},
			},
		},
	}

	r.Register(openAI)
	r.Register(ollama)
	require.NoError(t, r.SetDefault(providerOpenAI))
	require.NoError(t, r.SetModelRole("fast_coder", ModelRole{
		Preferred:  "gpt-4.1-mini",
		MaxCostUSD: 0.000001,
	}))

	resp, err := r.CompleteWithFallback(context.Background(), CompleteParams{
		Model:     "fast_coder",
		Messages:  []Message{{Role: RoleUser, Content: "implement a small helper"}},
		MaxTokens: 10,
	}, nil)
	require.NoError(t, err)

	assert.Equal(t, "local", resp.Content)
	assert.Empty(t, openAI.calls)
	require.Len(t, ollama.calls, 1)
	assert.Equal(t, "gpt-4.1-mini", ollama.calls[0].Model)

	resolution, ok, err := r.ResolveModelRole("fast_coder", CompleteParams{
		Messages:  []Message{{Role: RoleUser, Content: "implement a small helper"}},
		MaxTokens: 10,
	}, nil)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "ollama/gpt-4.1-mini", resolution.SelectedModel)
	assertRejectionContains(t, resolution.Decision, "openai/gpt-4.1-mini", modelroute.ReasonOverBudget)
	assert.NotContains(t, formatModelRoleRejections(resolution.Decision), modelroute.ReasonAmbiguousMetadata)
}

func TestRegistry_ModelRoleLatencyPolicyCanOverrideDefaultProviderCollision(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	telemetry := modelroute.NewTelemetry()
	r.SetRouteTelemetry(telemetry)

	openAI := &fakeProvider{
		name:   providerOpenAI,
		models: []string{"gpt-5.4-mini"},
		resp:   &Response{Content: "openai"},
	}
	codex := &fakeProvider{
		name:   providerCodex,
		models: []string{"gpt-5.4-mini"},
		resp:   &Response{Content: "codex"},
	}

	r.Register(openAI)
	r.Register(codex)
	require.NoError(t, r.SetDefault(providerOpenAI))
	telemetry.Record(
		modelroute.Candidate{Provider: providerOpenAI, Name: "gpt-5.4-mini"},
		modelroute.ActualUsage{Latency: 900 * time.Millisecond, TTFT: 400 * time.Millisecond},
		time.Now(),
	)
	telemetry.Record(
		modelroute.Candidate{Provider: providerCodex, Name: "gpt-5.4-mini"},
		modelroute.ActualUsage{Latency: 50 * time.Millisecond, TTFT: 25 * time.Millisecond},
		time.Now(),
	)

	require.NoError(t, r.SetModelRole("planner", ModelRole{
		Preferred:    "gpt-5.4-mini",
		MaxLatencyMS: 250,
		MaxTTFTMS:    100,
	}))

	resp, err := r.CompleteWithFallback(context.Background(), CompleteParams{
		Model:    "planner",
		Messages: []Message{{Role: RoleUser, Content: "plan"}},
	}, nil)
	require.NoError(t, err)

	assert.Equal(t, "codex", resp.Content)
	assert.Empty(t, openAI.calls)
	require.Len(t, codex.calls, 1)
	assert.Equal(t, "gpt-5.4-mini", codex.calls[0].Model)

	resolution, ok, err := r.ResolveModelRole("planner", CompleteParams{
		Messages: []Message{{Role: RoleUser, Content: "plan"}},
	}, nil)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "codex/gpt-5.4-mini", resolution.SelectedModel)
	assert.Contains(t, resolution.Decision.Constraints, modelroute.ConstraintLatency)
	assert.Contains(t, resolution.Decision.Constraints, modelroute.ConstraintTTFT)
	assertRejectionContains(t, resolution.Decision, "openai/gpt-5.4-mini", modelroute.ReasonLatencyExceeded)
	assertRejectionContains(t, resolution.Decision, "openai/gpt-5.4-mini", modelroute.ReasonTTFTExceeded)
	assert.NotContains(t, formatModelRoleRejections(resolution.Decision), modelroute.ReasonAmbiguousMetadata)
}

func TestRegistry_ModelRolePreferredProvidersFallbackWhenFirstProviderUnavailable(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	openAI := &fakeProvider{
		name:   providerOpenAI,
		models: []string{"gpt-5.4-mini"},
		resp:   &Response{Content: "openai"},
	}

	r.Register(openAI)
	require.NoError(t, r.SetModelRole("planner", ModelRole{
		Preferred:          "gpt-5.4-mini",
		PreferredProviders: []string{providerCodex, providerOpenAI},
	}))

	resp, err := r.CompleteWithFallback(context.Background(), CompleteParams{
		Model:    "planner",
		Messages: []Message{{Role: RoleUser, Content: "plan"}},
	}, nil)
	require.NoError(t, err)

	assert.Equal(t, "openai", resp.Content)
	require.Len(t, openAI.calls, 1)
	assert.Equal(t, "gpt-5.4-mini", openAI.calls[0].Model)

	resolution, ok, err := r.ResolveModelRole("planner", CompleteParams{
		Messages: []Message{{Role: RoleUser, Content: "plan"}},
	}, nil)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "openai/gpt-5.4-mini", resolution.SelectedModel)
	assertRejectionContains(t, resolution.Decision, "codex/gpt-5.4-mini", modelroute.ReasonProviderUnavailable)
}

func TestRegistry_ModelRoleBannedProviderDisambiguatesBareCatalogModel(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	openAI := &fakeProvider{
		name:   providerOpenAI,
		models: []string{"gpt-5.4-mini"},
		resp:   &Response{Content: "openai"},
	}
	codex := &fakeProvider{
		name:   providerCodex,
		models: []string{"gpt-5.4-mini"},
		resp:   &Response{Content: "codex"},
	}

	r.Register(openAI)
	r.Register(codex)
	require.NoError(t, r.SetModelRole("planner", ModelRole{
		Preferred:       "gpt-5.4-mini",
		BannedProviders: []string{providerCodex},
	}))

	resp, err := r.CompleteWithFallback(context.Background(), CompleteParams{
		Model:    "planner",
		Messages: []Message{{Role: RoleUser, Content: "plan"}},
	}, nil)
	require.NoError(t, err)

	assert.Equal(t, "openai", resp.Content)
	require.Len(t, openAI.calls, 1)
	assert.Empty(t, codex.calls)

	resolution, ok, err := r.ResolveModelRole("planner", CompleteParams{
		Messages: []Message{{Role: RoleUser, Content: "plan"}},
	}, nil)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "openai/gpt-5.4-mini", resolution.SelectedModel)
	assertRejectionContains(t, resolution.Decision, "codex/gpt-5.4-mini", modelroute.ReasonProviderBanned)
}

func TestRegistry_ResolveModelRoleWithPolicyAppliesAdditionalConstraints(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	openAI := &fakeProvider{
		name:   providerOpenAI,
		models: []string{"gpt-5.4-mini"},
		resp:   &Response{Content: "openai"},
	}
	codex := &fakeProvider{
		name:   providerCodex,
		models: []string{"gpt-5.4-mini"},
		resp:   &Response{Content: "codex"},
	}

	r.Register(openAI)
	r.Register(codex)
	require.NoError(t, r.SetModelRole("planner", ModelRole{
		Preferred:      "openai/gpt-5.4-mini",
		FallbackModels: []string{"codex/gpt-5.4-mini"},
	}))

	resolution, ok, err := r.ResolveModelRoleWithPolicy(
		"planner",
		CompleteParams{Messages: []Message{{Role: RoleUser, Content: "plan"}}},
		nil,
		modelroute.Policy{BannedProviders: []string{providerOpenAI}},
	)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "codex/gpt-5.4-mini", resolution.SelectedModel)
	assert.Contains(t, resolution.Decision.Policy.BannedProviders, providerOpenAI)
	assertRejectionContains(t, resolution.Decision, "openai/gpt-5.4-mini", modelroute.ReasonProviderBanned)
}

func TestRegistry_ModelRoleHonorsProviderCapabilityMetadata(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	r.Register(&capabilityFakeProvider{
		fakeProvider: fakeProvider{
			name:   providerOpenAI,
			models: []string{"gpt-4.1-mini"},
			resp:   &Response{Content: "ok"},
		},
		capabilities: ProviderCapabilities{
			SupportsChatCompletions: true,
			SupportsTools:           true,
			SupportsCostTracking:    true,
		},
	})

	require.NoError(t, r.SetModelRole("streamer", ModelRole{
		Preferred:            "openai/gpt-4.1-mini",
		RequiredCapabilities: []string{modelroute.CapabilityStreaming},
	}))

	resolution, ok, err := r.ResolveModelRole("streamer", CompleteParams{
		Messages: []Message{{Role: RoleUser, Content: "stream this"}},
	}, nil)

	require.Error(t, err)
	require.True(t, ok)
	assert.Empty(t, resolution.SelectedModel)
	assertRejectionContains(t, resolution.Decision, "openai/gpt-4.1-mini", modelroute.ReasonMissingCapability)
	assertRejectionContains(t, resolution.Decision, "openai/gpt-4.1-mini", modelroute.CapabilityStreaming)
}

func TestRegistry_ModelRoleInfersRequiredChatCapabilityFromRequest(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	embeddingsOnly := &capabilityFakeProvider{
		capabilities: ProviderCapabilities{SupportsEmbeddings: true},
		fakeProvider: fakeProvider{
			name:   "embedder",
			models: []string{"embed-only"},
			resp:   &Response{Content: "embedding endpoint"},
		},
	}
	openAI := &fakeProvider{
		name:   providerOpenAI,
		models: []string{"gpt-4.1-mini"},
		resp:   &Response{Content: "chat endpoint"},
	}

	r.Register(embeddingsOnly)
	r.Register(openAI)

	require.NoError(t, r.SetModelRole("summarizer", ModelRole{
		Preferred:      "embedder/embed-only",
		FallbackModels: []string{"openai/gpt-4.1-mini"},
	}))

	resp, err := r.CompleteWithFallback(context.Background(), CompleteParams{
		Model:    "summarizer",
		Messages: []Message{{Role: RoleUser, Content: "summarize"}},
	}, nil)
	require.NoError(t, err)

	assert.Equal(t, "chat endpoint", resp.Content)
	assert.Empty(t, embeddingsOnly.calls)
	require.Len(t, openAI.calls, 1)
	assert.Equal(t, "gpt-4.1-mini", openAI.calls[0].Model)

	resolution, ok, err := r.ResolveModelRole("summarizer", CompleteParams{
		Messages: []Message{{Role: RoleUser, Content: "summarize"}},
	}, nil)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "openai/gpt-4.1-mini", resolution.SelectedModel)
	assert.Contains(t, resolution.Decision.Policy.RequiredCapabilities, modelroute.CapabilityChat)
	assertRejectionContains(t, resolution.Decision, "embedder/embed-only", modelroute.ReasonMissingCapability)
	assertRejectionContains(t, resolution.Decision, "embedder/embed-only", modelroute.CapabilityChat)
}

func TestRegistry_ModelRoleRequiresChatCapabilityForEmptyCompletion(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	embeddingsOnly := &capabilityFakeProvider{
		capabilities: ProviderCapabilities{SupportsEmbeddings: true},
		fakeProvider: fakeProvider{
			name:   "embedder",
			models: []string{"embed-only"},
			resp:   &Response{Content: "embedding endpoint"},
		},
	}
	openAI := &fakeProvider{
		name:   providerOpenAI,
		models: []string{"gpt-4.1-mini"},
		resp:   &Response{Content: "chat endpoint"},
	}

	r.Register(embeddingsOnly)
	r.Register(openAI)
	require.NoError(t, r.SetModelRole("summarizer", ModelRole{
		Preferred:      "embedder/embed-only",
		FallbackModels: []string{"openai/gpt-4.1-mini"},
	}))

	resp, err := r.CompleteWithFallback(context.Background(), CompleteParams{
		Model: "summarizer",
	}, nil)
	require.NoError(t, err)

	assert.Equal(t, "chat endpoint", resp.Content)
	assert.Empty(t, embeddingsOnly.calls)
	require.Len(t, openAI.calls, 1)
	assert.Equal(t, "gpt-4.1-mini", openAI.calls[0].Model)

	resolution, ok, err := r.ResolveModelRole("summarizer", CompleteParams{}, nil)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "openai/gpt-4.1-mini", resolution.SelectedModel)
	assert.Contains(t, resolution.Decision.Policy.RequiredCapabilities, modelroute.CapabilityChat)
	assertRejectionContains(t, resolution.Decision, "embedder/embed-only", modelroute.ReasonMissingCapability)
	assertRejectionContains(t, resolution.Decision, "embedder/embed-only", modelroute.CapabilityChat)
}

func TestRegistry_ModelRoleInfersRequiredReasoningCapabilityFromRequest(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	openAI := &fakeProvider{
		name:   providerOpenAI,
		models: []string{"gpt-4.1-mini", "gpt-5.4-mini"},
		resp:   &Response{Content: "ok"},
	}
	r.Register(openAI)

	require.NoError(t, r.SetModelRole("reasoner", ModelRole{
		Preferred:      "openai/gpt-4.1-mini",
		FallbackModels: []string{"openai/gpt-5.4-mini"},
	}))

	resp, err := r.CompleteWithFallback(context.Background(), CompleteParams{
		Model:          "reasoner",
		ReasoningLevel: reasoningLevelHigh,
		Messages:       []Message{{Role: RoleUser, Content: "think"}},
	}, nil)
	require.NoError(t, err)

	assert.Equal(t, "gpt-5.4-mini", resp.Model)
	require.Len(t, openAI.calls, 1)
	assert.Equal(t, "gpt-5.4-mini", openAI.calls[0].Model)

	resolution, ok, err := r.ResolveModelRole("reasoner", CompleteParams{
		ReasoningLevel: reasoningLevelHigh,
	}, nil)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "openai/gpt-5.4-mini", resolution.SelectedModel)
	assert.Contains(t, resolution.Decision.Policy.RequiredCapabilities, modelroute.CapabilityReasoning)
	assertRejectionContains(t, resolution.Decision, "openai/gpt-4.1-mini", modelroute.ReasonMissingCapability)
	assertRejectionContains(t, resolution.Decision, "openai/gpt-4.1-mini", modelroute.CapabilityReasoning)
}

func TestRegistry_ModelRoleRejectsAnthropicReasoningWhenMaxTokensTooSmall(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	claudeCode := &fakeProvider{
		name:   providerClaudeCode,
		models: []string{"claude-sonnet-4-6"},
		resp:   &Response{Content: "claude"},
	}
	openAI := &fakeProvider{
		name:   providerOpenAI,
		models: []string{"gpt-5.4-mini"},
		resp:   &Response{Content: "openai"},
	}

	r.Register(claudeCode)
	r.Register(openAI)

	require.NoError(t, r.SetModelRole("reasoner", ModelRole{
		Preferred:      "claude-code/claude-sonnet-4-6",
		FallbackModels: []string{"openai/gpt-5.4-mini"},
	}))

	params := CompleteParams{
		Model:          "reasoner",
		ReasoningLevel: reasoningLevelHigh,
		MaxTokens:      16,
		Messages:       []Message{{Role: RoleUser, Content: "think"}},
	}
	resp, err := r.Complete(context.Background(), params)
	require.NoError(t, err)

	assert.Equal(t, "openai", resp.Content)
	assert.Empty(t, claudeCode.calls)
	require.Len(t, openAI.calls, 1)
	assert.Equal(t, "gpt-5.4-mini", openAI.calls[0].Model)
	assert.Equal(t, reasoningLevelHigh, openAI.calls[0].ReasoningLevel)

	resolution, ok, err := r.ResolveModelRole("reasoner", params, nil)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "openai/gpt-5.4-mini", resolution.SelectedModel)
	assertRejectionContains(t, resolution.Decision, "claude-code/claude-sonnet-4-6", modelroute.ReasonMissingCapability)
	assertRejectionContains(t, resolution.Decision, "claude-code/claude-sonnet-4-6", modelroute.CapabilityReasoning)
}

func TestRegistry_ModelRoleDoesNotInferReasoningCapabilityForDisabledReasoning(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	r.Register(&fakeProvider{
		name:   providerOpenAI,
		models: []string{"gpt-4.1-mini", "gpt-5.4-mini"},
		resp:   &Response{Content: "ok"},
	})

	require.NoError(t, r.SetModelRole("direct", ModelRole{
		Preferred:      "openai/gpt-4.1-mini",
		FallbackModels: []string{"openai/gpt-5.4-mini"},
	}))

	resolution, ok, err := r.ResolveModelRole("direct", CompleteParams{
		ReasoningLevel: reasoningLevelNone,
	}, nil)

	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "openai/gpt-4.1-mini", resolution.SelectedModel)
	assert.NotContains(t, resolution.Decision.Policy.RequiredCapabilities, modelroute.CapabilityReasoning)
}

func TestRegistry_ModelRoleDoesNotInventProviderInfrastructureCapabilities(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	r.Register(&fakeProvider{
		name:   "custom",
		models: []string{"model-a"},
		resp:   &Response{Content: "ok"},
	})

	require.NoError(t, r.SetModelRole("needs_retries", ModelRole{
		Preferred:            "custom/model-a",
		RequiredCapabilities: []string{modelroute.CapabilityRetries},
	}))

	resolution, ok, err := r.ResolveModelRole("needs_retries", CompleteParams{}, nil)

	require.Error(t, err)
	require.True(t, ok)
	assert.Empty(t, resolution.SelectedModel)
	assertRejectionContains(t, resolution.Decision, "custom/model-a", modelroute.ReasonMissingCapability)
	assertRejectionContains(t, resolution.Decision, "custom/model-a", modelroute.CapabilityRetries)
}

func TestRegistry_ModelRoleInfersRequiredToolCapabilityFromRequest(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	openAI := &capabilityFakeProvider{
		capabilities: ProviderCapabilities{SupportsChatCompletions: true},
		fakeProvider: fakeProvider{
			name:   providerOpenAI,
			models: []string{"gpt-4.1-mini"},
			resp:   &Response{Content: "remote"},
		},
	}
	ollama := &fakeProvider{
		name:   providerOllama,
		models: []string{"llama3.2"},
		resp:   &Response{Content: "tool-capable"},
	}

	r.Register(openAI)
	r.Register(ollama)

	require.NoError(t, r.SetModelRole("tool_runner", ModelRole{
		Preferred:      "openai/gpt-4.1-mini",
		FallbackModels: []string{"ollama/llama3.2"},
	}))

	resp, err := r.CompleteWithFallback(context.Background(), CompleteParams{
		Model: "tool_runner",
		Tools: []ToolDefinition{{
			Name:        "lookup",
			Description: "lookup a value",
			Parameters:  map[string]any{"type": "object"},
		}},
	}, nil)
	require.NoError(t, err)

	assert.Equal(t, "tool-capable", resp.Content)
	assert.Empty(t, openAI.calls)
	require.Len(t, ollama.calls, 1)

	resolution, ok, err := r.ResolveModelRole("tool_runner", CompleteParams{
		Tools: []ToolDefinition{{Name: "lookup"}},
	}, nil)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "ollama/llama3.2", resolution.SelectedModel)
	assert.Contains(t, resolution.Decision.Policy.RequiredCapabilities, modelroute.CapabilityTools)
	assertRejectionContains(t, resolution.Decision, "openai/gpt-4.1-mini", modelroute.ReasonMissingCapability)
	assertRejectionContains(t, resolution.Decision, "openai/gpt-4.1-mini", modelroute.CapabilityTools)
}

func TestRegistry_ModelRoleInfersRequiredJSONSchemaCapabilityFromRequest(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	anthropic := &fakeProvider{
		name:   providerAnthropic,
		models: []string{"claude-sonnet-4-20250514"},
		resp:   &Response{Content: "text"},
	}
	openAI := &fakeProvider{
		name:   providerOpenAI,
		models: []string{"gpt-4.1-mini"},
		resp:   &Response{Content: "json"},
	}

	r.Register(anthropic)
	r.Register(openAI)

	require.NoError(t, r.SetModelRole("summarizer", ModelRole{
		Preferred:      "anthropic/claude-sonnet-4-20250514",
		FallbackModels: []string{"openai/gpt-4.1-mini"},
	}))

	resp, err := r.CompleteWithFallback(context.Background(), CompleteParams{
		Model: "summarizer",
		ResponseFormat: &ResponseFormat{
			Type:   ResponseFormatJSONSchema,
			Schema: map[string]any{"type": "object"},
		},
	}, nil)
	require.NoError(t, err)

	assert.Equal(t, "json", resp.Content)
	assert.Empty(t, anthropic.calls)
	require.Len(t, openAI.calls, 1)
	assert.Equal(t, "gpt-4.1-mini", openAI.calls[0].Model)

	resolution, ok, err := r.ResolveModelRole("summarizer", CompleteParams{
		ResponseFormat: &ResponseFormat{Schema: map[string]any{"type": "object"}},
	}, nil)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "openai/gpt-4.1-mini", resolution.SelectedModel)
	assert.Contains(t, resolution.Decision.Policy.RequiredCapabilities, modelroute.CapabilityJSONSchema)
	assertRejectionContains(t, resolution.Decision, "anthropic/claude-sonnet-4-20250514", modelroute.ReasonMissingCapability)
	assertRejectionContains(t, resolution.Decision, "anthropic/claude-sonnet-4-20250514", modelroute.CapabilityJSONSchema)
}

func TestRegistry_CompleteWithModelRoleFallsBackOnProviderError(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	r.SetRetry(retryConfig{})

	openAI := &fakeProvider{
		err:    errors.New("primary offline"),
		name:   providerOpenAI,
		models: []string{"gpt-4.1-mini"},
		resp:   &Response{},
	}
	anthropic := &fakeProvider{
		name:   providerAnthropic,
		models: []string{"claude-sonnet-4-20250514"},
		resp:   &Response{Content: "fallback ok"},
	}

	r.Register(openAI)
	r.Register(anthropic)

	require.NoError(t, r.SetModelRole("planner", ModelRole{
		Preferred:      "openai/gpt-4.1-mini",
		FallbackModels: []string{"anthropic/claude-sonnet-4-20250514"},
	}))

	resp, err := r.CompleteWithFallback(context.Background(), CompleteParams{Model: "planner"}, nil)
	require.NoError(t, err)

	assert.Equal(t, "fallback ok", resp.Content)
	assert.Equal(t, "claude-sonnet-4-20250514", resp.Model)
	require.Len(t, openAI.calls, 1)
	require.Len(t, anthropic.calls, 1)
}

func TestRegistry_CompleteWithModelRoleExpandsNestedFallbackRole(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	openAI := &fakeProvider{
		name:   providerOpenAI,
		models: []string{"gpt-4.1"},
		resp:   &Response{Content: "primary"},
	}
	anthropic := &fakeProvider{
		name:   providerAnthropic,
		models: []string{"claude-sonnet-4-20250514"},
		resp:   &Response{Content: "nested fallback"},
	}

	r.Register(openAI)
	r.Register(anthropic)

	require.NoError(t, r.SetModelRole("fallback_writer", ModelRole{
		Preferred: "anthropic/claude-sonnet-4-20250514",
	}))
	require.NoError(t, r.SetModelRole("planner", ModelRole{
		Preferred:      "openai/gpt-4.1",
		FallbackModels: []string{"fallback_writer"},
		BannedModels:   []string{"openai/gpt-4.1"},
	}))

	resp, err := r.CompleteWithFallback(context.Background(), CompleteParams{Model: "planner"}, nil)
	require.NoError(t, err)

	assert.Equal(t, "nested fallback", resp.Content)
	assert.Empty(t, openAI.calls)
	require.Len(t, anthropic.calls, 1)
	assert.Equal(t, "claude-sonnet-4-20250514", anthropic.calls[0].Model)

	resolution, ok, err := r.ResolveModelRole("planner", CompleteParams{}, nil)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "anthropic/claude-sonnet-4-20250514", resolution.SelectedModel)
	assertRejectionContains(t, resolution.Decision, "openai/gpt-4.1", modelroute.ReasonModelBanned)
}

func TestRegistry_CompleteWithModelRoleAppliesNestedPreferredProviderPolicy(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	openAI := &fakeProvider{
		name:   providerOpenAI,
		models: []string{"gpt-5.4-mini"},
		resp:   &Response{Content: "openai"},
	}
	codex := &fakeProvider{
		name:   providerCodex,
		models: []string{"gpt-5.4-mini"},
		resp:   &Response{Content: "codex"},
	}

	r.Register(openAI)
	r.Register(codex)

	require.NoError(t, r.SetModelRole("writer", ModelRole{
		Preferred:          "gpt-5.4-mini",
		PreferredProviders: []string{providerCodex},
	}))
	require.NoError(t, r.SetModelRole("planner", ModelRole{
		Preferred: "writer",
	}))

	resp, err := r.CompleteWithFallback(context.Background(), CompleteParams{Model: "planner"}, nil)
	require.NoError(t, err)

	assert.Equal(t, "codex", resp.Content)
	assert.Empty(t, openAI.calls)
	require.Len(t, codex.calls, 1)
	assert.Equal(t, "gpt-5.4-mini", codex.calls[0].Model)

	resolution, ok, err := r.ResolveModelRole("planner", CompleteParams{}, nil)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "codex/gpt-5.4-mini", resolution.SelectedModel)
	assert.NotContains(t, formatModelRoleRejections(resolution.Decision), modelroute.ReasonAmbiguousMetadata)
}

func TestRegistry_CompleteWithModelRoleAppliesNestedBannedProviderPolicy(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	openAI := &fakeProvider{
		name:   providerOpenAI,
		models: []string{"gpt-5.4-mini"},
		resp:   &Response{Content: "openai"},
	}
	codex := &fakeProvider{
		name:   providerCodex,
		models: []string{"gpt-5.4-mini"},
		resp:   &Response{Content: "codex"},
	}

	r.Register(openAI)
	r.Register(codex)

	require.NoError(t, r.SetModelRole("writer", ModelRole{
		Preferred:       "openai/gpt-5.4-mini",
		FallbackModels:  []string{"codex/gpt-5.4-mini"},
		BannedProviders: []string{providerOpenAI},
	}))
	require.NoError(t, r.SetModelRole("planner", ModelRole{
		Preferred: "writer",
	}))

	resp, err := r.CompleteWithFallback(context.Background(), CompleteParams{Model: "planner"}, nil)
	require.NoError(t, err)

	assert.Equal(t, "codex", resp.Content)
	assert.Empty(t, openAI.calls)
	require.Len(t, codex.calls, 1)

	resolution, ok, err := r.ResolveModelRole("planner", CompleteParams{}, nil)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "codex/gpt-5.4-mini", resolution.SelectedModel)
}

func TestRegistry_CompleteWithModelRoleAppliesNestedBudgetPolicy(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	openAI := &fakeProvider{
		name:   providerOpenAI,
		models: []string{"gpt-4.1", "gpt-4.1-mini"},
		resp:   &Response{Content: "within budget"},
	}

	r.Register(openAI)

	require.NoError(t, r.SetModelRole("writer", ModelRole{
		Preferred:      "openai/gpt-4.1",
		FallbackModels: []string{"openai/gpt-4.1-mini"},
		MaxCostUSD:     0.002,
	}))
	require.NoError(t, r.SetModelRole("planner", ModelRole{
		Preferred: "writer",
	}))

	resp, err := r.CompleteWithFallback(context.Background(), CompleteParams{
		Model:     "planner",
		MaxTokens: 1000,
	}, nil)
	require.NoError(t, err)

	assert.Equal(t, "within budget", resp.Content)
	require.Len(t, openAI.calls, 1)
	assert.Equal(t, "gpt-4.1-mini", openAI.calls[0].Model)

	resolution, ok, err := r.ResolveModelRole("planner", CompleteParams{
		MaxTokens: 1000,
	}, nil)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "openai/gpt-4.1-mini", resolution.SelectedModel)
}

func TestRegistry_ResolveModelRoleReportsNestedPolicyRejections(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	openAI := &fakeProvider{
		name:   providerOpenAI,
		models: []string{"gpt-4.1"},
		resp:   &Response{Content: "too expensive"},
	}

	r.Register(openAI)

	require.NoError(t, r.SetModelRole("writer", ModelRole{
		Preferred:  "openai/gpt-4.1",
		MaxCostUSD: 0.001,
	}))
	require.NoError(t, r.SetModelRole("planner", ModelRole{
		Preferred: "writer",
	}))

	resolution, ok, err := r.ResolveModelRole("planner", CompleteParams{
		MaxTokens: 1000,
	}, nil)
	require.Error(t, err)
	require.True(t, ok)
	assert.Empty(t, resolution.SelectedModel)
	assert.Contains(t, err.Error(), modelroute.ReasonOverBudget)
	assertRejectionContains(t, resolution.Decision, "openai/gpt-4.1", modelroute.ReasonOverBudget)
	assert.Empty(t, openAI.calls)
}

func TestRegistry_CompleteWithModelRoleAppliesNestedPreferLocalPolicy(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	openAI := &fakeProvider{
		name:   providerOpenAI,
		models: []string{"gpt-4.1-mini"},
		resp:   &Response{Content: "remote"},
	}
	ollama := &fakeProvider{
		name:   providerOllama,
		models: []string{"llama3.2"},
		resp:   &Response{Content: "local"},
	}

	r.Register(openAI)
	r.Register(ollama)

	require.NoError(t, r.SetModelRole("writer", ModelRole{
		Preferred:      "openai/gpt-4.1-mini",
		FallbackModels: []string{"ollama/llama3.2"},
		PreferLocal:    true,
	}))
	require.NoError(t, r.SetModelRole("planner", ModelRole{
		Preferred: "writer",
	}))

	resp, err := r.CompleteWithFallback(context.Background(), CompleteParams{Model: "planner"}, nil)
	require.NoError(t, err)

	assert.Equal(t, "local", resp.Content)
	assert.Empty(t, openAI.calls)
	require.Len(t, ollama.calls, 1)
	assert.Equal(t, "llama3.2", ollama.calls[0].Model)

	resolution, ok, err := r.ResolveModelRole("planner", CompleteParams{}, nil)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "ollama/llama3.2", resolution.SelectedModel)
}

func TestRegistry_CompleteWithModelRolePrefersLocalCandidate(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	openAI := &fakeProvider{
		name:   providerOpenAI,
		models: []string{"gpt-4.1-mini"},
		resp:   &Response{Content: "remote"},
	}
	ollama := &fakeProvider{
		name:   providerOllama,
		models: []string{"llama3.2"},
		resp:   &Response{Content: "local"},
	}

	r.Register(openAI)
	r.Register(ollama)

	require.NoError(t, r.SetModelRole("fast_coder", ModelRole{
		Preferred:      "openai/gpt-4.1-mini",
		FallbackModels: []string{"ollama/llama3.2"},
		PreferLocal:    true,
	}))

	resp, err := r.CompleteWithFallback(context.Background(), CompleteParams{Model: "fast_coder"}, nil)
	require.NoError(t, err)

	assert.Equal(t, "local", resp.Content)
	assert.Empty(t, openAI.calls)
	require.Len(t, ollama.calls, 1)
	assert.Equal(t, "llama3.2", ollama.calls[0].Model)

	resolution, ok, err := r.ResolveModelRole("fast_coder", CompleteParams{}, nil)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "ollama/llama3.2", resolution.SelectedModel)
}

func TestRegistry_CompleteWithModelRolePrefersLocalOpenAICompatibleCandidate(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	openAI := &fakeProvider{
		name:   providerOpenAI,
		models: []string{"gpt-4.1-mini"},
		resp:   &Response{Content: "remote"},
	}
	vllm := &localCapabilityFakeProvider{capabilityFakeProvider: capabilityFakeProvider{
		capabilities: ProviderCapabilities{SupportsChatCompletions: true},
		fakeProvider: fakeProvider{
			name:   "vllm",
			models: []string{"qwen2.5-coder"},
			resp:   &Response{Content: "local-compatible"},
		},
	}}

	r.Register(openAI)
	r.Register(vllm)

	require.NoError(t, r.SetModelRole("fast_coder", ModelRole{
		Preferred:      "openai/gpt-4.1-mini",
		FallbackModels: []string{"vllm/qwen2.5-coder"},
		PreferLocal:    true,
	}))

	resp, err := r.CompleteWithFallback(context.Background(), CompleteParams{Model: "fast_coder"}, nil)
	require.NoError(t, err)

	assert.Equal(t, "local-compatible", resp.Content)
	assert.Empty(t, openAI.calls)
	require.Len(t, vllm.calls, 1)
	assert.Equal(t, "qwen2.5-coder", vllm.calls[0].Model)
}

func TestRegistry_ModelRolePreferLocalDisambiguatesBareRuntimeCollision(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	openAI := &capabilityFakeProvider{
		capabilities: ProviderCapabilities{SupportsChatCompletions: true},
		fakeProvider: fakeProvider{
			name:   providerOpenAI,
			models: []string{"shared-coder"},
			resp:   &Response{Content: "remote"},
		},
	}
	vllm := &localCapabilityFakeProvider{capabilityFakeProvider: capabilityFakeProvider{
		capabilities: ProviderCapabilities{SupportsChatCompletions: true},
		fakeProvider: fakeProvider{
			name:   "vllm",
			models: []string{"shared-coder"},
			resp:   &Response{Content: "local"},
		},
	}}

	r.Register(openAI)
	r.Register(vllm)
	require.NoError(t, r.SetDefault(providerOpenAI))
	require.NoError(t, r.SetModelRole("fast_coder", ModelRole{
		Preferred:   "shared-coder",
		PreferLocal: true,
	}))

	resp, err := r.CompleteWithFallback(context.Background(), CompleteParams{
		Model:    "fast_coder",
		Messages: []Message{{Role: RoleUser, Content: "implement"}},
	}, nil)
	require.NoError(t, err)

	assert.Equal(t, "local", resp.Content)
	assert.Empty(t, openAI.calls)
	require.Len(t, vllm.calls, 1)
	assert.Equal(t, "shared-coder", vllm.calls[0].Model)

	resolution, ok, err := r.ResolveModelRole("fast_coder", CompleteParams{
		Messages: []Message{{Role: RoleUser, Content: "implement"}},
	}, nil)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "vllm/shared-coder", resolution.SelectedModel)
	assert.NotContains(t, formatModelRoleRejections(resolution.Decision), modelroute.ReasonAmbiguousMetadata)
}

func TestRegistry_ModelRolePrefersUniqueConfiguredProviderForBareRuntimeCollision(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	openAI := &fakeProvider{
		name:   providerOpenAI,
		models: []string{"gpt-4.1-mini"},
		resp:   &Response{Content: "openai"},
	}
	groq := &capabilityFakeProvider{
		capabilities: ProviderCapabilities{SupportsChatCompletions: true},
		fakeProvider: fakeProvider{
			name:   "groq",
			models: []string{"gpt-4.1-mini"},
			resp:   &Response{Content: "configured compatible"},
		},
	}

	r.Register(openAI)
	r.Register(groq)

	r.mu.Lock()
	r.upsertReadinessProviderLocked(ProviderReadiness{
		Name:       providerOpenAI,
		Status:     ProviderStatusRegistered,
		Registered: true,
	})
	r.upsertReadinessProviderLocked(ProviderReadiness{
		Name:       "groq",
		Status:     ProviderStatusRegistered,
		Registered: true,
		Configured: true,
	})
	r.mu.Unlock()

	require.NoError(t, r.SetModelRole("planner", ModelRole{
		Preferred: "gpt-4.1-mini",
	}))

	resp, err := r.CompleteWithFallback(context.Background(), CompleteParams{Model: "planner"}, nil)
	require.NoError(t, err)

	assert.Equal(t, "configured compatible", resp.Content)
	assert.Empty(t, openAI.calls)
	require.Len(t, groq.calls, 1)
	assert.Equal(t, "gpt-4.1-mini", groq.calls[0].Model)

	resolution, ok, err := r.ResolveModelRole("planner", CompleteParams{}, nil)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "groq/gpt-4.1-mini", resolution.SelectedModel)
}

func TestRegistry_ModelRoleAllowsOpenAICompatibleUnknownProviderModel(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	openAICompatible := &fakeProvider{
		name:   providerOpenAI,
		models: []string{"gpt-4.1-mini"},
		resp:   &Response{Content: "compatible"},
	}
	r.Register(openAICompatible)

	require.NoError(t, r.SetModelRole("planner", ModelRole{
		Preferred: "openai/azure-deployment",
	}))

	resp, err := r.CompleteWithFallback(context.Background(), CompleteParams{Model: "planner"}, nil)
	require.NoError(t, err)

	assert.Equal(t, "compatible", resp.Content)
	require.Len(t, openAICompatible.calls, 1)
	assert.Equal(t, "azure-deployment", openAICompatible.calls[0].Model)

	resolution, ok, err := r.ResolveModelRole("planner", CompleteParams{}, nil)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "openai/azure-deployment", resolution.SelectedModel)
	assert.Contains(t, resolution.Decision.Availability.Unverified["openai/azure-deployment"], modelroute.ReasonModelUnverified)
}

func TestRegistry_ModelRoleUsesRuntimeProviderWhenBareCatalogProviderUnavailable(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	azure := &capabilityFakeProvider{
		capabilities: ProviderCapabilities{SupportsChatCompletions: true},
		fakeProvider: fakeProvider{
			name:   "azure",
			models: []string{"gpt-4.1-mini"},
			resp:   &Response{Content: "azure compatible"},
		},
	}
	r.Register(azure)

	require.NoError(t, r.SetModelRole("planner", ModelRole{
		Preferred: "gpt-4.1-mini",
	}))

	resp, err := r.CompleteWithFallback(context.Background(), CompleteParams{Model: "planner"}, nil)
	require.NoError(t, err)

	assert.Equal(t, "azure compatible", resp.Content)
	require.Len(t, azure.calls, 1)
	assert.Equal(t, "gpt-4.1-mini", azure.calls[0].Model)

	resolution, ok, err := r.ResolveModelRole("planner", CompleteParams{}, nil)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "azure/gpt-4.1-mini", resolution.SelectedModel)
	assert.NotContains(t, resolution.Decision.Availability.Unavailable, "openai/gpt-4.1-mini")
}

func TestRegistry_ModelRoleRejectsUnpricedCandidateWithBudget(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	r.Register(&fakeProvider{
		name:   providerOpenAI,
		models: []string{"live-only"},
		resp:   &Response{Content: "should not route"},
	})

	require.NoError(t, r.SetModelRole("planner", ModelRole{
		Preferred:  "openai/live-only",
		MaxCostUSD: 0.01,
	}))

	_, err := r.CompleteWithFallback(context.Background(), CompleteParams{Model: "planner"}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), modelroute.ReasonCostUnknown)
}

func TestRegistry_CompleteRecordsRouteTelemetry(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	telemetry := modelroute.NewTelemetry()
	r.SetRouteTelemetry(telemetry)
	r.Register(&fakeProvider{
		name:   providerOpenAI,
		models: []string{"gpt-4.1-mini"},
		resp: &Response{
			Content:               "ok",
			FirstTokenLatency:     12 * time.Millisecond,
			InputTokens:           1000,
			CachedInputTokens:     200,
			CacheWriteInputTokens: 100,
			OutputTokens:          50,
		},
	})

	resp, err := r.Complete(context.Background(), CompleteParams{Model: "gpt-4.1-mini"})
	require.NoError(t, err)
	assert.Positive(t, resp.Latency)

	obs, ok := telemetry.Snapshot("openai/gpt-4.1-mini")
	require.True(t, ok)
	assert.Equal(t, 1, obs.Count)
	assert.Equal(t, 1000, obs.InputTokens)
	assert.Equal(t, 200, obs.CachedInputTokens)
	assert.Equal(t, 100, obs.CacheWriteTokens)
	assert.Equal(t, 50, obs.OutputTokens)
	assert.Positive(t, obs.LastLatencyMS)
	assert.Equal(t, 12, obs.AvgTTFTMS)
	assert.InDelta(t, 0.00042, obs.LastCost, 0.000000001)
}

func TestRegistry_CompleteRecordsTelemetryForProviderReportedAlias(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	telemetry := modelroute.NewTelemetry()
	r.SetRouteTelemetry(telemetry)
	r.Register(&fakeProvider{
		name:   providerOpenAI,
		models: []string{"gpt-4.1-mini"},
		resp: &Response{
			Model:        "gpt-4.1-mini-2025-04-14",
			InputTokens:  1000,
			OutputTokens: 50,
		},
	})

	_, err := r.Complete(context.Background(), CompleteParams{Model: "gpt-4.1-mini"})
	require.NoError(t, err)

	_, aliasObserved := telemetry.Snapshot("openai/gpt-4.1-mini-2025-04-14")
	assert.False(t, aliasObserved)

	obs, ok := telemetry.Snapshot("openai/gpt-4.1-mini")
	require.True(t, ok)
	assert.Equal(t, "openai/gpt-4.1-mini", obs.ModelID)
	assert.InDelta(t, 0.00048, obs.LastCost, 0.000000001)
}

func TestRegistry_CompleteRecordsTelemetryAgainstRequestedCatalogModelWhenProviderRevisionDrifts(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	telemetry := modelroute.NewTelemetry()
	r.SetRouteTelemetry(telemetry)
	r.Register(&fakeProvider{
		name:   providerOpenAI,
		models: []string{"gpt-4.1-mini"},
		resp: &Response{
			Model:        "gpt-4.1-mini-2026-05-22",
			InputTokens:  1000,
			OutputTokens: 50,
		},
	})

	_, err := r.Complete(context.Background(), CompleteParams{Model: "gpt-4.1-mini"})
	require.NoError(t, err)

	_, driftObserved := telemetry.Snapshot("openai/gpt-4.1-mini-2026-05-22")
	assert.False(t, driftObserved)

	obs, ok := telemetry.Snapshot("openai/gpt-4.1-mini")
	require.True(t, ok)
	assert.Equal(t, "openai/gpt-4.1-mini", obs.ModelID)
	assert.InDelta(t, 0.00048, obs.LastCost, 0.000000001)
}

func TestRegistry_ContextWindowUsesBuiltinCatalogMetadata(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	r.Register(&fakeProvider{
		name:   providerOpenAI,
		models: []string{"gpt-5.5"},
		resp:   &Response{},
	})

	assert.Equal(t, 1_050_000, r.ContextWindow("openai/gpt-5.5"))
	assert.Equal(t, 1_050_000, r.ContextWindow("gpt-5.5"))
	assert.Equal(t, 0, NewRegistry().ContextWindow("openai/gpt-5.5"))
}

func TestProviders_ModelContextWindowUsesBuiltinCatalogMetadata(t *testing.T) {
	t.Parallel()

	assert.Equal(t, 1_050_000, (&OpenAIProvider{}).ModelContextWindow("gpt-5.5"))
	assert.Equal(t, 1_000_000, (&AnthropicProvider{}).ModelContextWindow("claude-opus-4-7"))
	assert.Equal(t, 1_000_000, (&ClaudeCodeProvider{}).ModelContextWindow("claude-sonnet-4-6"))
	assert.Equal(t, 1_050_000, (&CodexProvider{}).ModelContextWindow("gpt-5.5"))
	assert.Equal(t, 128_000, (&OllamaProvider{}).ModelContextWindow("llama3.2"))
}

func TestRegistry_CanResolveModelUsesProviderQualificationIndexAndAliases(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	r.Register(&fakeProvider{
		name:   providerOpenAI,
		models: []string{"gpt-4.1-mini"},
		resp:   &Response{},
	})
	require.NoError(t, r.SetModelAlias("fast", providerOpenAI, "gpt-4.1-mini"))

	provider, ok := r.CanResolveModel("openai/gpt-future")
	require.True(t, ok)
	assert.Equal(t, providerOpenAI, provider)

	provider, ok = r.CanResolveModel("gpt-4.1-mini")
	require.True(t, ok)
	assert.Equal(t, providerOpenAI, provider)

	provider, ok = r.CanResolveModel("fast")
	require.True(t, ok)
	assert.Equal(t, providerOpenAI, provider)

	_, ok = r.CanResolveModel("gpt-future")
	assert.False(t, ok)

	_, ok = r.CanResolveModel("anthropic/claude-sonnet-4-20250514")
	assert.False(t, ok)
}

func TestRegistry_CompleteRejectsAmbiguousBareModel(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	r.Register(&fakeProvider{name: alphaProvider, models: []string{"shared"}, resp: &Response{Content: "alpha"}})
	r.Register(&fakeProvider{name: "beta", models: []string{"shared"}, resp: &Response{Content: "beta"}})

	_, err := r.Complete(context.Background(), CompleteParams{Model: "shared"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ambiguous model")
	assert.Contains(t, err.Error(), alphaProvider)
	assert.Contains(t, err.Error(), "beta")

	diagnostic := r.ExplainModelResolution("shared")
	require.Error(t, diagnostic.Error)
	assert.Equal(t, "bare model is ambiguous; use provider/model or configure a default provider", diagnostic.Reason)
	require.Len(t, diagnostic.Candidates, 2)
}

func TestRegistry_CompleteRejectsAmbiguousLiveCatalogModel(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	r.Register(&fakeProvider{
		name:          alphaProvider,
		models:        []string{"alpha-static"},
		fetchedModels: []string{"shared-live"},
		resp:          &Response{Content: "alpha"},
	})
	r.Register(&fakeProvider{
		name:          "beta",
		models:        []string{"beta-static"},
		fetchedModels: []string{"shared-live"},
		resp:          &Response{Content: "beta"},
	})

	_, err := r.ProviderModelCatalog(context.Background(), alphaProvider)
	require.NoError(t, err)
	_, err = r.ProviderModelCatalog(context.Background(), "beta")
	require.NoError(t, err)

	_, err = r.Complete(context.Background(), CompleteParams{Model: "shared-live"})
	require.Error(t, err)
	require.ErrorContains(t, err, "ambiguous model")

	diagnostic := r.ExplainModelResolution("shared-live")
	require.Error(t, diagnostic.Error)
	require.Len(t, diagnostic.Candidates, 2)
	assert.Equal(t, ModelProvenanceFetchedLive, diagnostic.Candidates[0].Provenance)
	assert.Equal(t, ModelProvenanceFetchedLive, diagnostic.Candidates[1].Provenance)
}

func TestRegistry_CompleteUsesConfiguredDefaultProviderForAmbiguousBareModel(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	r.Register(&fakeProvider{name: alphaProvider, models: []string{"shared"}, resp: &Response{Content: "alpha"}})
	r.Register(&fakeProvider{name: "beta", models: []string{"shared"}, resp: &Response{Content: "beta"}})
	require.NoError(t, r.SetDefault("beta"))

	resp, err := r.Complete(context.Background(), CompleteParams{Model: "shared"})
	require.NoError(t, err)
	assert.Equal(t, "beta", resp.Content)

	diagnostic := r.ExplainModelResolution("shared")
	require.NoError(t, diagnostic.Error)
	assert.Equal(t, "beta", diagnostic.ProviderName)
	assert.Contains(t, diagnostic.Reason, "configured default provider")
}

func TestRegistry_CompleteUsesConfiguredDefaultModelProviderForAmbiguousBareModel(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	r.Register(&fakeProvider{name: alphaProvider, models: []string{"a-1", "shared"}, resp: &Response{Content: "alpha"}})
	r.Register(&fakeProvider{name: "beta", models: []string{"b-1", "shared"}, resp: &Response{Content: "beta"}})
	require.NoError(t, r.SetDefaultModel("a-1"))

	resp, err := r.Complete(context.Background(), CompleteParams{Model: "shared"})
	require.NoError(t, err)
	assert.Equal(t, "alpha", resp.Content)

	diagnostic := r.ExplainModelResolution("shared")
	require.NoError(t, diagnostic.Error)
	assert.Equal(t, alphaProvider, diagnostic.ProviderName)
	assert.True(t, diagnostic.DefaultProviderConfigured)
}

func TestRegistry_ConfiguredAliasResolvesToProviderModel(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	provider := &fakeProvider{
		name:   providerOpenAI,
		models: []string{"gpt-4.1-mini"},
		resp:   &Response{Content: "ok"},
	}
	r.Register(provider)
	require.NoError(t, r.SetModelAlias("fast", providerOpenAI, "gpt-4.1-mini"))

	resp, err := r.Complete(context.Background(), CompleteParams{Model: "fast"})
	require.NoError(t, err)
	assert.Equal(t, "gpt-4.1-mini", resp.Model)
	require.Len(t, provider.calls, 1)
	assert.Equal(t, "gpt-4.1-mini", provider.calls[0].Model)

	diagnostic := r.ExplainModelResolution("fast")
	require.NoError(t, diagnostic.Error)
	assert.Equal(t, providerOpenAI, diagnostic.ProviderName)
	assert.Equal(t, "gpt-4.1-mini", diagnostic.ProviderModel)
	assert.Equal(t, ModelProvenanceConfiguredAlias, diagnostic.Provenance)

	catalog, err := r.ProviderModelCatalog(context.Background(), providerOpenAI)
	require.NoError(t, err)
	assert.Contains(t, catalog.Models, "fast")
	assert.Equal(t, ModelProvenanceConfiguredAlias, catalog.ModelProvenance["fast"])
}

func TestRegistry_ConfiguredAliasTargetMarksStaleStaticFallback(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	r.Register(&fakeProvider{
		name:     alphaProvider,
		models:   []string{"static-model"},
		fetchErr: errors.New("models unavailable"),
		resp:     &Response{Content: "ok"},
	})
	require.NoError(t, r.SetModelAlias("fast", alphaProvider, "static-model"))

	catalog, err := r.ProviderModelCatalog(context.Background(), alphaProvider)
	require.NoError(t, err)
	require.Error(t, catalog.Error)
	require.True(t, catalog.Stale)

	diagnostic := r.ExplainModelResolution("fast")
	require.NoError(t, diagnostic.Error)
	assert.Equal(t, ModelProvenanceConfiguredAlias, diagnostic.Provenance)
	assert.True(t, diagnostic.Stale)
	require.Len(t, diagnostic.Candidates, 1)
	assert.True(t, diagnostic.Candidates[0].Stale)
}

func TestRegistry_ConfiguredAliasTargetNotInStaticFallbackIsNotStale(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	r.Register(&fakeProvider{
		name:     alphaProvider,
		models:   []string{"static-model"},
		fetchErr: errors.New("models unavailable"),
		resp:     &Response{Content: "ok"},
	})
	require.NoError(t, r.SetModelAlias("private-model", alphaProvider, "private-model"))

	catalog, err := r.ProviderModelCatalog(context.Background(), alphaProvider)
	require.NoError(t, err)
	require.Error(t, catalog.Error)
	require.True(t, catalog.Stale)
	assert.Contains(t, catalog.Models, "private-model")
	assert.Equal(t, ModelProvenanceConfiguredAlias, catalog.ModelProvenance["private-model"])

	diagnostic := r.ExplainModelResolution("private-model")
	require.NoError(t, diagnostic.Error)
	assert.Equal(t, ModelProvenanceConfiguredAlias, diagnostic.Provenance)
	assert.False(t, diagnostic.Stale)
	require.Len(t, diagnostic.Candidates, 1)
	assert.False(t, diagnostic.Candidates[0].Stale)
}

func TestRegistry_SetModelAliasRejectsProviderQualifiedAlias(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	r.Register(&fakeProvider{
		name:   providerOpenAI,
		models: []string{"gpt-4.1-mini"},
		resp:   &Response{},
	})

	err := r.SetModelAlias(providerOpenAI+"/fast", providerOpenAI, "gpt-4.1-mini")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be a bare model name")

	err = r.SetModelAlias(providerOpenAI+"/", providerOpenAI, "gpt-4.1-mini")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be a bare model name")
}

func TestRegistry_ProviderQualifiedModelIgnoresConfiguredAlias(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	provider := &fakeProvider{
		name:   providerOpenAI,
		models: []string{"gpt-4.1-mini"},
		resp:   &Response{Content: "ok"},
	}
	r.Register(provider)
	require.NoError(t, r.SetModelAlias("fast", providerOpenAI, "gpt-4.1-mini"))

	resp, err := r.Complete(context.Background(), CompleteParams{Model: providerOpenAI + "/fast"})
	require.NoError(t, err)
	assert.Equal(t, "fast", resp.Model)
	require.Len(t, provider.calls, 1)
	assert.Equal(t, "fast", provider.calls[0].Model)

	diagnostic := r.ExplainModelResolution(providerOpenAI + "/fast")
	require.NoError(t, diagnostic.Error)
	assert.Equal(t, providerOpenAI, diagnostic.ProviderName)
	assert.Equal(t, "fast", diagnostic.ProviderModel)
	assert.Equal(t, ModelProvenanceUserOverride, diagnostic.Provenance)
}

func TestRegistry_ProviderQualifiedModelDoesNotUseSameNameConfiguredAliasProvenance(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	provider := &fakeProvider{
		name:   providerOpenAI,
		models: []string{"fast"},
		resp:   &Response{Content: "ok"},
	}
	r.Register(provider)
	require.NoError(t, r.SetModelAlias("fast", providerOpenAI, "fast"))

	bareDiagnostic := r.ExplainModelResolution("fast")
	require.NoError(t, bareDiagnostic.Error)
	assert.Equal(t, ModelProvenanceConfiguredAlias, bareDiagnostic.Provenance)

	qualifiedDiagnostic := r.ExplainModelResolution(providerOpenAI + "/fast")
	require.NoError(t, qualifiedDiagnostic.Error)
	assert.Equal(t, providerOpenAI, qualifiedDiagnostic.ProviderName)
	assert.Equal(t, "fast", qualifiedDiagnostic.ProviderModel)
	assert.Equal(t, ModelProvenanceStatic, qualifiedDiagnostic.Provenance)
	assert.Equal(t, "provider-qualified model selected provider directly", qualifiedDiagnostic.Reason)

	resp, err := r.Complete(context.Background(), CompleteParams{Model: providerOpenAI + "/fast"})
	require.NoError(t, err)
	assert.Equal(t, "fast", resp.Model)
}

func TestRegistry_ProviderQualifiedOverrideDoesNotStealBareConfiguredAlias(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	provider := &fakeProvider{
		name:   providerOpenAI,
		models: []string{"gpt-4.1-mini"},
		resp:   &Response{Content: "ok"},
	}
	r.Register(provider)
	require.NoError(t, r.SetModelAlias("fast", providerOpenAI, "gpt-4.1-mini"))
	require.NoError(t, r.SetProviderModelOverride(providerOpenAI, "fast"))

	resp, err := r.Complete(context.Background(), CompleteParams{Model: "fast"})
	require.NoError(t, err)
	assert.Equal(t, "gpt-4.1-mini", resp.Model)

	resp, err = r.Complete(context.Background(), CompleteParams{Model: providerOpenAI + "/fast"})
	require.NoError(t, err)
	assert.Equal(t, "fast", resp.Model)

	diagnostic := r.ExplainModelResolution("fast")
	require.NoError(t, diagnostic.Error)
	assert.Equal(t, "gpt-4.1-mini", diagnostic.ProviderModel)
	assert.Equal(t, ModelProvenanceConfiguredAlias, diagnostic.Provenance)
	assert.True(t, r.ProviderModelUserOverride(providerOpenAI, "fast"))
}

func TestRegistry_ConfiguredAliasSurvivesCatalogRefreshNameCollision(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	provider := &fakeProvider{
		name:          providerOpenAI,
		models:        []string{"fast"},
		fetchedModels: []string{"fast", "gpt-4.1-mini"},
		resp:          &Response{Content: "ok"},
	}
	r.Register(provider)
	require.NoError(t, r.SetModelAlias("fast", providerOpenAI, "gpt-4.1-mini"))

	catalog, err := r.ProviderModelCatalog(context.Background(), providerOpenAI)
	require.NoError(t, err)
	require.NoError(t, catalog.Error)
	assert.Equal(t, ModelProvenanceConfiguredAlias, catalog.ModelProvenance["fast"])

	provenance, ok := r.ProviderModelCatalogProvenance(providerOpenAI, "fast")
	require.True(t, ok)
	assert.Equal(t, ModelProvenanceFetchedLive, provenance)

	resp, err := r.Complete(context.Background(), CompleteParams{Model: "fast"})
	require.NoError(t, err)
	assert.Equal(t, "gpt-4.1-mini", resp.Model)

	diagnostic := r.ExplainModelResolution("fast")
	require.NoError(t, diagnostic.Error)
	assert.Equal(t, providerOpenAI, diagnostic.ProviderName)
	assert.Equal(t, "gpt-4.1-mini", diagnostic.ProviderModel)
	assert.Equal(t, ModelProvenanceConfiguredAlias, diagnostic.Provenance)

	qualifiedDiagnostic := r.ExplainModelResolution(providerOpenAI + "/fast")
	require.NoError(t, qualifiedDiagnostic.Error)
	assert.Equal(t, providerOpenAI, qualifiedDiagnostic.ProviderName)
	assert.Equal(t, "fast", qualifiedDiagnostic.ProviderModel)
	assert.Equal(t, ModelProvenanceFetchedLive, qualifiedDiagnostic.Provenance)
}

func TestRegistry_CatalogProvenanceClearsWhenLiveCatalogDropsAliasName(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	provider := &fakeProvider{
		name:          providerOpenAI,
		models:        []string{"fast"},
		fetchedModels: []string{"gpt-4.1-mini"},
		resp:          &Response{Content: "ok"},
	}
	r.Register(provider)
	require.NoError(t, r.SetModelAlias("fast", providerOpenAI, "gpt-4.1-mini"))

	catalog, err := r.ProviderModelCatalog(context.Background(), providerOpenAI)
	require.NoError(t, err)
	require.NoError(t, catalog.Error)
	assert.Contains(t, catalog.Models, "fast")
	assert.Equal(t, ModelProvenanceConfiguredAlias, catalog.ModelProvenance["fast"])

	_, ok := r.ProviderModelCatalogProvenance(providerOpenAI, "fast")
	assert.False(t, ok)

	bareDiagnostic := r.ExplainModelResolution("fast")
	require.NoError(t, bareDiagnostic.Error)
	assert.Equal(t, "gpt-4.1-mini", bareDiagnostic.ProviderModel)
	assert.Equal(t, ModelProvenanceConfiguredAlias, bareDiagnostic.Provenance)

	qualifiedDiagnostic := r.ExplainModelResolution(providerOpenAI + "/fast")
	require.NoError(t, qualifiedDiagnostic.Error)
	assert.Equal(t, "fast", qualifiedDiagnostic.ProviderModel)
	assert.Equal(t, ModelProvenanceUserOverride, qualifiedDiagnostic.Provenance)
}

func TestRegistry_DefaultProviderQualifiedModelIgnoresConfiguredAlias(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	provider := &fakeProvider{
		name:   providerOpenAI,
		models: []string{"gpt-4.1-mini"},
		resp:   &Response{Content: "ok"},
	}
	r.Register(provider)
	require.NoError(t, r.SetModelAlias("fast", providerOpenAI, "gpt-4.1-mini"))
	require.NoError(t, r.SetDefaultModel(providerOpenAI+"/fast"))

	resp, err := r.Complete(context.Background(), CompleteParams{})
	require.NoError(t, err)
	assert.Equal(t, "fast", resp.Model)
	require.Len(t, provider.calls, 1)
	assert.Equal(t, "fast", provider.calls[0].Model)

	diagnostic := r.ExplainModelResolution("")
	require.NoError(t, diagnostic.Error)
	assert.Equal(t, providerOpenAI, diagnostic.ProviderName)
	assert.Equal(t, "fast", diagnostic.ProviderModel)
	assert.Equal(t, ModelProvenanceUserOverride, diagnostic.Provenance)
}

func TestRegistry_ConfiguredAliasCollisionIsAmbiguous(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	r.Register(&fakeProvider{name: alphaProvider, models: []string{"a-1"}, resp: &Response{Content: "alpha"}})
	r.Register(&fakeProvider{name: "beta", models: []string{"b-1"}, resp: &Response{Content: "beta"}})
	require.NoError(t, r.SetModelAlias("fast", alphaProvider, "a-1"))
	require.NoError(t, r.SetModelAlias("fast", "beta", "b-1"))

	_, err := r.Complete(context.Background(), CompleteParams{Model: "fast"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ambiguous model")

	diagnostic := r.ExplainModelResolution("fast")
	require.Error(t, diagnostic.Error)
	require.Len(t, diagnostic.Candidates, 2)
	assert.Equal(t, alphaProvider, diagnostic.Candidates[0].ProviderName)
	assert.Equal(t, "a-1", diagnostic.Candidates[0].Model)
	assert.Equal(t, "beta", diagnostic.Candidates[1].ProviderName)
	assert.Equal(t, "b-1", diagnostic.Candidates[1].Model)
}

func TestRegistry_ConfiguredAliasCollidesWithStaticCatalogClaim(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	r.Register(&fakeProvider{name: alphaProvider, models: []string{"fast"}, resp: &Response{Content: "alpha"}})
	r.Register(&fakeProvider{name: "beta", models: []string{"b-1"}, resp: &Response{Content: "beta"}})
	require.NoError(t, r.SetModelAlias("fast", "beta", "b-1"))

	_, err := r.Complete(context.Background(), CompleteParams{Model: "fast"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ambiguous model")

	diagnostic := r.ExplainModelResolution("fast")
	require.Error(t, diagnostic.Error)
	require.Len(t, diagnostic.Candidates, 2)
	assert.Equal(t, alphaProvider, diagnostic.Candidates[0].ProviderName)
	assert.Equal(t, "fast", diagnostic.Candidates[0].Model)
	assert.Equal(t, ModelProvenanceStatic, diagnostic.Candidates[0].Provenance)
	assert.Equal(t, "beta", diagnostic.Candidates[1].ProviderName)
	assert.Equal(t, "b-1", diagnostic.Candidates[1].Model)
	assert.Equal(t, ModelProvenanceConfiguredAlias, diagnostic.Candidates[1].Provenance)

	require.NoError(t, r.SetDefault("beta"))

	resp, err := r.Complete(context.Background(), CompleteParams{Model: "fast"})
	require.NoError(t, err)
	assert.Equal(t, "beta", resp.Content)
	assert.Equal(t, "b-1", resp.Model)
}

func TestRegistry_ConfiguredDefaultProviderDisambiguatesAliasCollision(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	r.Register(&fakeProvider{name: alphaProvider, models: []string{"a-1"}, resp: &Response{Content: "alpha"}})
	r.Register(&fakeProvider{name: "beta", models: []string{"b-1"}, resp: &Response{Content: "beta"}})
	require.NoError(t, r.SetModelAlias("fast", alphaProvider, "a-1"))
	require.NoError(t, r.SetModelAlias("fast", "beta", "b-1"))
	require.NoError(t, r.SetDefault("beta"))

	resp, err := r.Complete(context.Background(), CompleteParams{Model: "fast"})
	require.NoError(t, err)
	assert.Equal(t, "beta", resp.Content)

	diagnostic := r.ExplainModelResolution("fast")
	require.NoError(t, diagnostic.Error)
	assert.Equal(t, "beta", diagnostic.ProviderName)
	assert.Equal(t, "b-1", diagnostic.ProviderModel)
	assert.Contains(t, diagnostic.Reason, "configured default provider")
}

func TestRegistry_DefaultConfiguredAliasUsesProviderModel(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	provider := &fakeProvider{
		name:   providerOpenAI,
		models: []string{"gpt-4.1-mini"},
		resp:   &Response{Content: "ok"},
	}
	r.Register(provider)
	require.NoError(t, r.SetModelAlias("fast", providerOpenAI, "gpt-4.1-mini"))
	require.NoError(t, r.SetDefaultModel("fast"))

	resp, err := r.Complete(context.Background(), CompleteParams{})
	require.NoError(t, err)
	assert.Equal(t, "gpt-4.1-mini", resp.Model)
	require.Len(t, provider.calls, 1)
	assert.Equal(t, "gpt-4.1-mini", provider.calls[0].Model)

	diagnostic := r.ExplainModelResolution("")
	require.NoError(t, diagnostic.Error)
	assert.Equal(t, providerOpenAI, diagnostic.ProviderName)
	assert.Equal(t, "gpt-4.1-mini", diagnostic.ProviderModel)
	assert.Equal(t, ModelProvenanceConfiguredAlias, diagnostic.Provenance)
}

func TestRegistry_UserOverrideModelProvenance(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	r.Register(&fakeProvider{name: providerCodex, models: []string{"gpt-5.5"}, resp: &Response{}})

	require.NoError(t, r.SetDefaultProviderModel(providerCodex, "custom-deployment"))

	diagnostic := r.ExplainModelResolution("custom-deployment")
	require.NoError(t, diagnostic.Error)
	assert.Equal(t, providerCodex, diagnostic.ProviderName)
	assert.Equal(t, ModelProvenanceUserOverride, diagnostic.Provenance)

	provenance, ok := r.ProviderModelProvenance(providerCodex, "custom-deployment")
	require.True(t, ok)
	assert.Equal(t, ModelProvenanceUserOverride, provenance)
}

func TestRegistry_ProviderModelCatalogIncludesUserOverrideProvenance(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	r.Register(&fakeProvider{
		name:          providerOpenAI,
		models:        []string{"gpt-static"},
		fetchedModels: []string{"gpt-live"},
		resp:          &Response{},
	})
	require.NoError(t, r.SetProviderModelOverride(providerOpenAI, "private-deployment"))

	catalog, err := r.ProviderModelCatalog(context.Background(), providerOpenAI)
	require.NoError(t, err)
	require.NoError(t, catalog.Error)
	assert.Equal(t, ModelCatalogSourceLive, catalog.Source)
	assert.Contains(t, catalog.Models, "gpt-live")
	assert.Contains(t, catalog.Models, "private-deployment")
	assert.Equal(t, ModelProvenanceFetchedLive, catalog.ModelProvenance["gpt-live"])
	assert.Equal(t, ModelProvenanceUserOverride, catalog.ModelProvenance["private-deployment"])

	models, err := r.ProviderModels(context.Background(), providerOpenAI)
	require.NoError(t, err)
	assert.Contains(t, models, "private-deployment")
}

func TestRegistry_ProviderHasModelUsesProviderSpecificIndex(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	r.Register(&fakeProvider{
		name:   providerOpenAI,
		models: []string{"shared", "gpt-4.1-mini"},
		resp:   &Response{},
	})
	r.Register(&fakeProvider{
		name:   providerAnthropic,
		models: []string{"claude-sonnet-4-20250514"},
		resp:   &Response{},
	})

	require.NoError(t, r.SetDefaultModel(providerOpenAI+"/gpt-live"))
	require.NoError(t, r.SetDefaultProviderModel(providerAnthropic, "claude-live"))

	assert.True(t, r.ProviderHasModel(providerOpenAI, "shared"))
	assert.False(t, r.ProviderHasModel(providerAnthropic, "shared"))
	assert.True(t, r.ProviderHasModel(providerOpenAI, "gpt-live"))
	assert.True(t, r.ProviderHasModel(providerAnthropic, "claude-live"))
	assert.False(t, r.ProviderHasModel(providerOpenAI, ""))

	indexed := r.IndexedProviderModels()
	assert.Contains(t, indexed[providerOpenAI], "shared")
	assert.Contains(t, indexed[providerOpenAI], "gpt-live")
	assert.Contains(t, indexed[providerAnthropic], "claude-live")
	assert.NotContains(t, indexed[providerAnthropic], "shared")
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
	require.Error(t, err)
	assert.Contains(t, err.Error(), "a-1 via alpha/a-1")
	assert.Contains(t, err.Error(), "missing via missing unresolved")
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

func TestRegistry_SetDefaultModelExactSlashModelID(t *testing.T) {
	t.Parallel()

	const model = "namespace/model"

	r := NewRegistry()
	r.Register(&fakeProvider{name: alphaProvider, models: []string{model}, resp: &Response{Content: "from-alpha"}})

	require.NoError(t, r.SetDefaultModel(model))

	resp, err := r.Complete(context.Background(), CompleteParams{})
	require.NoError(t, err)
	assert.Equal(t, "from-alpha", resp.Content)
	assert.Equal(t, model, resp.Model)

	diagnostic := r.ExplainModelResolution("")
	require.NoError(t, diagnostic.Error)
	assert.Equal(t, alphaProvider, diagnostic.ProviderName)
	assert.Equal(t, model, diagnostic.ProviderModel)
	assert.Equal(t, ModelProvenanceStatic, diagnostic.Provenance)
	assert.True(t, diagnostic.DefaultProviderConfigured)
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

func TestRegistry_ResolveModelAndContextWindowUseDefaultForEmptyModel(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	r.Register(&fakeProvider{name: alphaProvider, models: []string{"a-1"}, resp: &Response{}})

	provider, model, ok := r.ResolveModel("")
	if !ok {
		require.FailNow(t, "expected empty request model to resolve to default provider/model")
	}

	assert.Equal(t, alphaProvider, provider)
	assert.Equal(t, "a-1", model)
	assert.Equal(t, 128_000, r.ContextWindow(""))
}

func TestRegistry_EmptyModelUsesFetchedLiveCatalogDefault(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	provider := &fakeProvider{
		name:          alphaProvider,
		models:        []string{"static-model"},
		fetchedModels: []string{"live-model"},
		resp:          &Response{Content: "ok"},
	}
	r.Register(provider)

	_, err := r.ProviderModelCatalog(context.Background(), alphaProvider)
	require.NoError(t, err)

	resp, err := r.Complete(context.Background(), CompleteParams{})
	require.NoError(t, err)
	assert.Equal(t, "live-model", resp.Model)
	require.Len(t, provider.calls, 1)
	assert.Equal(t, "live-model", provider.calls[0].Model)

	diagnostic := r.ExplainModelResolution("")
	require.NoError(t, diagnostic.Error)
	assert.Equal(t, alphaProvider, diagnostic.ProviderName)
	assert.Equal(t, "live-model", diagnostic.ProviderModel)
	assert.Equal(t, ModelProvenanceFetchedLive, diagnostic.Provenance)
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

	catalog, err := r.ProviderModelCatalog(context.Background(), alphaProvider)
	require.NoError(t, err)
	assert.Equal(t, ModelProvenanceFetchedLive, catalog.ModelProvenance["live-model"])

	assert.True(t, r.ProviderHasModel(alphaProvider, "live-model"))
	assert.False(t, r.ProviderHasModel(alphaProvider, "static-model"))
	assert.True(t, r.ProviderModelsVerified(alphaProvider))

	resp, err := r.Complete(context.Background(), CompleteParams{Model: "live-model"})
	if err != nil {
		require.NoError(t, err)
	}

	if resp.Model != "live-model" {
		assert.Failf(t, "assertion failed", "expected live model routing, got %q", resp.Model)
	}

	_, err = r.Complete(context.Background(), CompleteParams{Model: "static-model"})
	require.Error(t, err)
	assert.ErrorContains(t, err, "unknown model")
}

func TestRegistry_ProviderModelsFetchFailureMarksVerifiedIndexUnverified(t *testing.T) {
	t.Parallel()

	provider := &fakeProvider{
		name:          alphaProvider,
		models:        []string{"static-model"},
		fetchedModels: []string{"live-model"},
		resp:          &Response{Content: "ok"},
	}
	r := NewRegistry()
	r.Register(provider)

	_, err := r.ProviderModels(context.Background(), alphaProvider)

	require.NoError(t, err)
	assert.True(t, r.ProviderModelsVerified(alphaProvider))

	provider.fetchModelsErr = errors.New("models endpoint unavailable")
	models, err := r.ProviderModels(context.Background(), alphaProvider)

	require.Error(t, err)
	require.ErrorContains(t, err, "using stale static fallback")
	require.ErrorContains(t, err, "models endpoint unavailable")
	assert.Equal(t, []string{"static-model"}, models)
	assert.False(t, r.ProviderModelsVerified(alphaProvider))
	assert.False(t, r.ProviderHasModel(alphaProvider, "live-model"))
	assert.True(t, r.ProviderHasModel(alphaProvider, "static-model"))
}

func TestRegistry_CheckHealthFailureReplacesLiveIndexWithStaticFallback(t *testing.T) {
	t.Parallel()

	provider := &fakeProvider{
		name:          alphaProvider,
		models:        []string{"static-model"},
		fetchedModels: []string{"live-model"},
		resp:          &Response{},
	}
	r := NewRegistry()
	r.Register(provider)

	results := r.CheckHealth(context.Background())

	require.Len(t, results, 1)
	assert.True(t, results[0].Healthy)
	assert.True(t, r.ProviderModelsVerified(alphaProvider))
	assert.True(t, r.ProviderHasModel(alphaProvider, "live-model"))
	assert.False(t, r.ProviderHasModel(alphaProvider, "static-model"))

	provider.healthCheckErr = errors.New("connection refused")
	results = r.CheckHealth(context.Background())

	require.Len(t, results, 1)
	assert.False(t, results[0].Healthy)
	assert.False(t, r.ProviderModelsVerified(alphaProvider))
	assert.False(t, r.ProviderHasModel(alphaProvider, "live-model"))
	assert.True(t, r.ProviderHasModel(alphaProvider, "static-model"))
}

func TestRegistry_ProviderModelCatalogReportsStaticFallbackOnFetchFailure(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	r.Register(&fakeProvider{
		name:     alphaProvider,
		models:   []string{"static-model"},
		fetchErr: errors.New("fetch failed"),
		resp:     &Response{Content: "ok"},
	})

	catalog, err := r.ProviderModelCatalog(context.Background(), alphaProvider)
	require.NoError(t, err)
	assert.Equal(t, ModelCatalogSourceStatic, catalog.Source)
	assert.Equal(t, []string{"static-model"}, catalog.Models)
	assert.Equal(t, []string{"static-model"}, catalog.StaticModels)
	assert.True(t, catalog.Stale)
	assert.Equal(t, ModelProvenanceStatic, catalog.ModelProvenance["static-model"])
	require.Error(t, catalog.Error)
	assert.Contains(t, catalog.Error.Error(), "fetch failed")
}

func TestProviderReadinessSummaryMarksStaleModelCatalog(t *testing.T) {
	t.Parallel()

	summary := providerReadinessSummary(&ProviderReadiness{
		Name:               alphaProvider,
		Status:             ProviderStatusRegistered,
		ModelCatalogSource: ModelCatalogSourceStatic,
		ModelsStale:        true,
	})

	assert.Contains(t, summary, "models=static")
	assert.Contains(t, summary, "stale=true")
}

func TestRegistry_ProviderModelCatalogClearsStaleFetchFailure(t *testing.T) {
	t.Parallel()

	provider := &fakeProvider{
		name:     alphaProvider,
		models:   []string{"static-model"},
		fetchErr: errors.New("first fetch failed"),
		resp:     &Response{Content: "ok"},
	}

	r := NewRegistry()
	r.Register(provider)

	catalog, err := r.ProviderModelCatalog(context.Background(), alphaProvider)
	require.NoError(t, err)
	require.Error(t, catalog.Error)

	provider.fetchErr = nil
	provider.fetchedModels = []string{"live-model"}

	catalog, err = r.ProviderModelCatalog(context.Background(), alphaProvider)
	require.NoError(t, err)
	require.NoError(t, catalog.Error)
	assert.Equal(t, ModelCatalogSourceLive, catalog.Source)
	assert.Equal(t, []string{"live-model"}, catalog.Models)

	entry := requireReadinessProvider(t, r.ReadinessReport(), alphaProvider)
	require.NoError(t, entry.Error)
	require.NoError(t, entry.ModelFetchError)
	assert.Equal(t, ModelCatalogSourceLive, entry.ModelCatalogSource)
}

func TestRegistry_ProviderModelCatalogFetchFailureReplacesPreviousLiveIndexWithStaleStaticFallback(t *testing.T) {
	t.Parallel()

	provider := &fakeProvider{
		name:          alphaProvider,
		models:        []string{"static-model"},
		fetchedModels: []string{"live-model"},
		resp:          &Response{Content: "ok"},
	}

	r := NewRegistry()
	r.Register(provider)

	catalog, err := r.ProviderModelCatalog(context.Background(), alphaProvider)
	require.NoError(t, err)
	require.NoError(t, catalog.Error)
	assert.Equal(t, []string{"live-model"}, catalog.Models)
	assert.True(t, r.ProviderHasModel(alphaProvider, "live-model"))
	assert.False(t, r.ProviderHasModel(alphaProvider, "static-model"))
	assert.True(t, r.ProviderModelsVerified(alphaProvider))

	provider.fetchedModels = nil
	provider.fetchErr = errors.New("live catalog unavailable")

	catalog, err = r.ProviderModelCatalog(context.Background(), alphaProvider)
	require.NoError(t, err)
	require.Error(t, catalog.Error)
	assert.True(t, catalog.Stale)
	assert.Equal(t, []string{"static-model"}, catalog.Models)
	assert.False(t, r.ProviderHasModel(alphaProvider, "live-model"))
	assert.True(t, r.ProviderHasModel(alphaProvider, "static-model"))
	assert.False(t, r.ProviderModelsVerified(alphaProvider))

	_, err = r.Complete(context.Background(), CompleteParams{Model: "live-model"})
	require.Error(t, err)
	assert.ErrorContains(t, err, "unknown model")
}

func TestRegistry_ProviderModelCatalogDoesNotFetchStaticOnlyProviders(t *testing.T) {
	t.Parallel()

	provider := &fakeProvider{
		name:     providerCodex,
		models:   []string{"codex-static"},
		fetchErr: errors.New("should not fetch"),
		resp:     &Response{Content: "ok"},
	}

	r := NewRegistry()
	r.Register(provider)

	catalog, err := r.ProviderModelCatalog(context.Background(), providerCodex)
	require.NoError(t, err)
	assert.Equal(t, ModelCatalogSourceStatic, catalog.Source)
	assert.Equal(t, []string{"codex-static"}, catalog.Models)
	require.NoError(t, catalog.Error)
	assert.Zero(t, provider.fetchCalls)
}

func TestRegistry_CheckHealthWithTTLUsesCachedReadiness(t *testing.T) {
	t.Parallel()

	provider := &fakeProvider{
		name:   alphaProvider,
		models: []string{"a-1"},
		resp:   &Response{Content: "ok"},
	}
	r := NewRegistry()
	r.Register(provider)

	first := r.CheckHealthWithTTL(context.Background(), time.Hour)
	require.Len(t, first, 1)
	assert.False(t, first[0].Cached)
	assert.True(t, first[0].Healthy)

	second := r.CheckHealthWithTTL(context.Background(), time.Hour)
	require.Len(t, second, 1)
	assert.True(t, second[0].Cached)
	assert.Equal(t, 1, provider.healthCalls)
}

func TestRegistry_ResolutionErrorIncludesReadinessContext(t *testing.T) {
	t.Parallel()

	r := autoRegisterWithFactoriesContext(context.Background(), AutoRegisterConfig{
		DefaultProvider:        alphaProvider,
		DisableReadinessChecks: true,
	}, []providerRegistration{
		{
			name:         alphaProvider,
			staticModels: []string{"a-1"},
			factory: func() (Provider, error) {
				return nil, errors.New("no alpha credentials found")
			},
		},
	})

	_, err := r.Complete(context.Background(), CompleteParams{Model: "a-1"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "provider readiness")
	assert.Contains(t, err.Error(), "missing_credentials")
}

func TestRegistry_ResolutionErrorHidesUnrequestedMissingCredentials(t *testing.T) {
	t.Parallel()

	r := autoRegisterWithFactoriesContext(context.Background(), AutoRegisterConfig{
		DefaultProvider:        "beta",
		DisableReadinessChecks: true,
	}, []providerRegistration{
		{
			name:         alphaProvider,
			staticModels: []string{"a-1"},
			factory: func() (Provider, error) {
				return nil, errors.New("no alpha credentials found")
			},
		},
		{
			name:         "beta",
			staticModels: []string{betaModel},
			factory: func() (Provider, error) {
				return nil, errors.New("no beta credentials found")
			},
		},
	})

	_, err := r.Complete(context.Background(), CompleteParams{Model: betaModel})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "beta=missing_credentials")
	assert.NotContains(t, err.Error(), "alpha=missing_credentials")
	assert.NotContains(t, err.Error(), "no alpha credentials")
}

func TestRegistry_ResolutionErrorIncludesDynamicallyRequestedProvider(t *testing.T) {
	t.Parallel()

	r := autoRegisterWithFactoriesContext(context.Background(), AutoRegisterConfig{
		DisableReadinessChecks: true,
	}, []providerRegistration{
		{
			name:         providerOpenAI,
			staticModels: []string{"gpt-static"},
			factory: func() (Provider, error) {
				return nil, errors.New("no OpenAI credentials found")
			},
		},
		{
			name:         providerCodex,
			staticModels: []string{"gpt-5.5"},
			factory: func() (Provider, error) {
				return nil, errors.New("no Codex credentials found")
			},
		},
	})

	_, err := r.Complete(context.Background(), CompleteParams{Model: providerCodex + "/gpt-5.5"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "codex=missing_credentials")
	assert.NotContains(t, err.Error(), "openai=missing_credentials")

	_, err = r.Complete(context.Background(), CompleteParams{Model: "gpt-static"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "openai=missing_credentials")
	assert.NotContains(t, err.Error(), "codex=missing_credentials")
}

func TestRegistry_ResolutionErrorIncludesDefaultSelectionContext(t *testing.T) {
	t.Parallel()

	r := autoRegisterWithFactoriesContext(context.Background(), AutoRegisterConfig{
		DefaultProvider:        alphaProvider,
		DefaultModel:           "beta/" + betaModel,
		DisableReadinessChecks: true,
	}, []providerRegistration{
		{
			name:         alphaProvider,
			staticModels: []string{"a-1"},
			factory: func() (Provider, error) {
				return &fakeProvider{name: alphaProvider, models: []string{"a-1"}, resp: &Response{}}, nil
			},
		},
	})

	_, err := r.Complete(context.Background(), CompleteParams{Model: "missing-model"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "default selection")
	assert.Contains(t, err.Error(), "default_model=beta/"+betaModel)
	assert.Contains(t, err.Error(), "not default provider")
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

	assert.Equal(t, defaultRetryConfig().info(), results[0].RetryPolicy)
}

func TestRegistry_CheckHealthIncludesProviderRetryPolicy(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	r.Register(&fakeProvider{
		name:   alphaProvider,
		models: []string{"a-1"},
		resp:   &Response{},
	})
	r.SetProviderRetry(alphaProvider, RetryPolicy{
		MaxAttempts:    4,
		InitialBackoff: 25 * time.Millisecond,
		MaxBackoff:     250 * time.Millisecond,
		MaxElapsedTime: 500 * time.Millisecond,
		JitterFraction: 0.3,
	})

	results := r.CheckHealth(context.Background())
	require.Len(t, results, 1)
	assert.Equal(t, RetryPolicyInfo{
		MaxAttempts:    4,
		InitialBackoff: 25 * time.Millisecond,
		MaxBackoff:     250 * time.Millisecond,
		MaxElapsedTime: 500 * time.Millisecond,
		JitterFraction: 0.3,
	}, results[0].RetryPolicy)
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

	assert.True(t, r.ProviderHasModel(alphaProvider, "live-1"))
	assert.False(t, r.ProviderHasModel(alphaProvider, "static"))
	assert.True(t, r.ProviderModelsVerified(alphaProvider))
}

func TestRegistry_StaticCatalogFetchDoesNotVerifyProviderAvailability(t *testing.T) {
	t.Parallel()

	for _, provider := range []Provider{
		&CodexProvider{models: []string{"gpt-5.5"}},
		&ClaudeCodeProvider{models: []string{"claude-sonnet-4-6"}},
	} {
		t.Run(provider.Name(), func(t *testing.T) {
			t.Parallel()

			r := NewRegistry()
			r.Register(provider)

			models, err := r.ProviderModels(context.Background(), provider.Name())

			require.NoError(t, err)
			require.NotEmpty(t, models)
			assert.False(t, r.ProviderModelsVerified(provider.Name()))
		})
	}
}

func TestRegistry_StaticCatalogFetchPreservesConfiguredProviderModel(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	r.Register(&CodexProvider{models: []string{"gpt-5.5"}})

	require.NoError(t, r.SetDefaultProviderModel(providerCodex, "custom-live"))

	_, err := r.ProviderModels(context.Background(), providerCodex)

	require.NoError(t, err)
	assert.False(t, r.ProviderModelsVerified(providerCodex))
	assert.True(t, r.ProviderHasModel(providerCodex, "custom-live"))
	providerName, ok := r.ProviderForModel("custom-live")
	assert.True(t, ok)
	assert.Equal(t, providerCodex, providerName)
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
	for i := range results {
		result := results[i]
		out[result.Name] = result
	}

	return out
}

func requireReadinessProvider(t *testing.T, report ProviderReadinessReport, providerName string) ProviderReadiness {
	t.Helper()

	for i := range report.Providers {
		provider := &report.Providers[i]
		if provider.Name == providerName {
			return *provider
		}
	}

	require.Failf(t, "provider missing", "readiness report missing provider %q: %+v", providerName, report.Providers)

	return ProviderReadiness{}
}

func requireDecisionCandidate(t *testing.T, decision modelroute.Decision, id string) modelroute.CandidateDecision {
	t.Helper()

	for i := range decision.Candidates {
		candidate := decision.Candidates[i]
		if candidate.ID == id {
			return candidate
		}
	}

	require.Failf(t, "candidate missing", "decision missing candidate %q: %+v", id, decision.Candidates)

	return modelroute.CandidateDecision{}
}

func assertRejectionContains(t *testing.T, decision modelroute.Decision, id, want string) {
	t.Helper()

	for i := range decision.Candidates {
		candidate := &decision.Candidates[i]
		if candidate.ID != id {
			continue
		}

		for _, reason := range candidate.Rejected {
			if strings.Contains(reason, want) {
				return
			}
		}

		require.Failf(t, "rejection missing", "%s rejected with %v, want substring %q", id, candidate.Rejected, want)
	}

	require.Failf(t, "candidate missing", "decision missing candidate %q: %+v", id, decision.Candidates)
}

func assertRejectionDoesNotContain(t *testing.T, decision modelroute.Decision, id, unwanted string) {
	t.Helper()

	for i := range decision.Candidates {
		candidate := &decision.Candidates[i]
		if candidate.ID != id {
			continue
		}

		for _, reason := range candidate.Rejected {
			assert.NotContains(t, reason, unwanted)
		}

		return
	}

	require.Failf(t, "candidate missing", "decision missing candidate %q: %+v", id, decision.Candidates)
}
