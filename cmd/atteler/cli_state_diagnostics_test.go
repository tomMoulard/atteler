package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/tommoulard/atteler/pkg/agent"
	appconfig "github.com/tommoulard/atteler/pkg/config"
	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/permission"
	"github.com/tommoulard/atteler/pkg/session"
)

const testReasoningLow = "low"

func TestPrintStateDiagnostics_PrintsPathRevisionAndResolvedSources(t *testing.T) {
	dir := t.TempDir()
	cwd := filepath.Join(dir, "project")
	require.NoError(t, os.Mkdir(cwd, 0o750))

	statePath := filepath.Join(dir, "state.yaml")
	t.Setenv(appconfig.EnvStatePath, statePath)

	store := appconfig.NewStateStore("")
	_, err := store.Update(func(state *appconfig.State) error {
		state.SetModel(appconfig.ModelScopeFolder, cwd, "folder-model")
		state.SetModelMode(appconfig.ModelScopeFolder, cwd, "fast")
		state.SetReasoningLevel(appconfig.ModelScopeFolder, cwd, "xhigh")

		return nil
	})
	require.NoError(t, err)

	app := stateDiagnosticsTestApp(appconfig.Config{})
	app.cwd = cwd
	app.sessionStore = session.NewStore(filepath.Join(dir, "sessions"))

	var printErr error

	out := captureStdoutForStateDiagnostics(t, func() {
		printErr = printStateDiagnostics(t.Context(), cliOptions{}, app)
	})
	require.NoError(t, printErr)

	var report stateDiagnosticsReport
	require.NoError(t, yaml.Unmarshal([]byte(out), &report))

	assert.Equal(t, statePath, report.StatePath)
	assert.Equal(t, "present", report.StateStatus)
	assert.Equal(t, cwd, report.CWD)
	assert.Equal(t, appconfig.FolderKey(cwd), report.FolderKey)
	assert.Equal(t, appconfig.StateSchemaVersion, report.Version)
	assert.Equal(t, int64(1), report.Revision)
	assert.Equal(t, "folder-model", report.Model.Selected)
	assert.Equal(t, "state.folder", report.Model.Source)
	assert.Equal(t, "folder", report.Model.Scope)
	assert.Equal(t, "xhigh", report.ReasoningLevel.Selected)
	assert.Equal(t, "state.folder", report.ReasoningLevel.Source)
	assert.Equal(t, "folder", report.ReasoningLevel.Scope)
	assert.Equal(t, "fast", report.ModelMode.Selected)
	assert.Equal(t, "state.folder", report.ModelMode.Source)
	assert.Equal(t, "folder", report.ModelMode.Scope)
}

func TestPrintStateDiagnosticsPermissionPolicyDeniesStateRead(t *testing.T) {
	t.Parallel()

	policy := permission.DefaultPolicy()
	policy.SetMode(permission.OperationRead, permission.ModeDeny)
	ctx := permission.ContextWithPolicy(t.Context(), &policy)

	err := printStateDiagnostics(ctx, cliOptions{}, stateDiagnosticsTestApp(appconfig.Config{}))

	require.Error(t, err)
	require.True(t, permission.ErrDenied(err))
	assert.Contains(t, err.Error(), "permission.read.deny")
}

func TestStateDiagnostics_PreferenceSources(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	state := appconfig.State{DefaultModel: "global-model", DefaultReasoningLevel: "medium"}
	state.SetModel(appconfig.ModelScopeFolder, dir, "folder-model")
	state.SetReasoningLevel(appconfig.ModelScopeFolder, dir, "high")

	cfg := appconfig.Config{DefaultModel: "config-model"}
	cfg.Generation.ReasoningLevel = testReasoningLow
	app := stateDiagnosticsTestApp(cfg)

	model := modelPreferenceReport(cliOptions{}, app, state, dir, stateSessionPreferences{})
	assert.Equal(t, "folder-model", model.Selected)
	assert.Equal(t, "state.folder", model.Source)
	assert.Equal(t, "folder", model.Scope)

	reasoning := reasoningPreferenceReport(cliOptions{}, app, state, dir, stateSessionPreferences{})
	assert.Equal(t, "high", reasoning.Selected)
	assert.Equal(t, "state.folder", reasoning.Source)
	assert.Equal(t, "folder", reasoning.Scope)
}

func TestStateDiagnostics_ReasoningDefaultSentinelNamesPersistedSource(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	state := appconfig.State{DefaultReasoningLevel: "high"}
	state.SetReasoningLevel(appconfig.ModelScopeFolder, dir, "default")

	cfg := appconfig.Config{}
	cfg.Generation.ReasoningLevel = testReasoningLow
	app := stateDiagnosticsTestApp(cfg)

	reasoning := reasoningPreferenceReport(cliOptions{}, app, state, dir, stateSessionPreferences{})
	assert.Equal(t, stateDiagnosticsReasoningDefault, reasoning.Selected)
	assert.Equal(t, "state.folder", reasoning.Source)
	assert.Equal(t, "folder", reasoning.Scope)
}

func TestStateDiagnostics_FlagsOverridePersistedSources(t *testing.T) {
	t.Parallel()

	state := appconfig.State{DefaultModel: "global-model", DefaultReasoningLevel: "medium"}

	cfg := appconfig.Config{DefaultModel: "config-model"}
	cfg.Generation.ReasoningLevel = testReasoningLow
	app := stateDiagnosticsTestApp(cfg)
	opts := cliOptions{model: "flag-model", reasoningLevel: "xhigh"}

	model := modelPreferenceReport(opts, app, state, t.TempDir(), stateSessionPreferences{})
	assert.Equal(t, "flag-model", model.Selected)
	assert.Equal(t, "flag.--model", model.Source)
	assert.Equal(t, "flag", model.Scope)

	reasoning := reasoningPreferenceReport(opts, app, state, t.TempDir(), stateSessionPreferences{})
	assert.Equal(t, "xhigh", reasoning.Selected)
	assert.Equal(t, "flag.--reasoning-level", reasoning.Source)
	assert.Equal(t, "flag", reasoning.Scope)
}

func TestStateDiagnostics_AgentSources(t *testing.T) {
	t.Parallel()

	cfg := appconfig.Config{
		DefaultModel: "config-model",
		Agents: map[string]appconfig.AgentConfig{
			"reviewer": {
				Model:          "agent-model",
				ReasoningLevel: "high",
			},
		},
	}
	cfg.Generation.ReasoningLevel = testReasoningLow
	app := stateDiagnosticsTestApp(cfg)
	opts := cliOptions{agentName: "reviewer"}

	model := modelPreferenceReport(opts, app, appconfig.State{}, t.TempDir(), stateSessionPreferences{})
	assert.Equal(t, "agent-model", model.Selected)
	assert.Equal(t, "agent.reviewer.model", model.Source)
	assert.Equal(t, "agent", model.Scope)

	reasoning := reasoningPreferenceReport(opts, app, appconfig.State{}, t.TempDir(), stateSessionPreferences{})
	assert.Equal(t, "high", reasoning.Selected)
	assert.Equal(t, "agent.reviewer.reasoning_level", reasoning.Source)
	assert.Equal(t, "agent", reasoning.Scope)
}

func TestStateDiagnostics_SessionSources(t *testing.T) {
	t.Parallel()

	cfg := appconfig.Config{
		DefaultModel: "config-model",
		Agents: map[string]appconfig.AgentConfig{
			"reviewer": {
				Model:          "agent-model",
				ReasoningLevel: "high",
			},
		},
	}
	cfg.Generation.ReasoningLevel = testReasoningLow
	app := stateDiagnosticsTestApp(cfg)
	sessionPrefs := stateSessionPreferences{
		ID:            "demo",
		DefaultModel:  "session-model",
		DefaultReason: "medium",
	}

	model := modelPreferenceReport(cliOptions{}, app, appconfig.State{}, t.TempDir(), sessionPrefs)
	assert.Equal(t, "session-model", model.Selected)
	assert.Equal(t, "session.default_model", model.Source)
	assert.Equal(t, "session", model.Scope)

	reasoning := reasoningPreferenceReport(cliOptions{}, app, appconfig.State{}, t.TempDir(), sessionPrefs)
	assert.Equal(t, "medium", reasoning.Selected)
	assert.Equal(t, "session.default_reasoning_level", reasoning.Source)
	assert.Equal(t, "session", reasoning.Scope)
}

func TestStateDiagnostics_LoadsSessionPreferences(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	saved := session.New("session-model", nil)
	saved.DefaultReasoningLevel = "medium"
	saved.DefaultAgent = "reviewer"
	require.NoError(t, store.Save(saved))

	got, err := stateDiagnosticSession(cliOptions{sessionRef: saved.ID}, store)
	require.NoError(t, err)
	assert.Equal(t, saved.ID, got.ID)
	assert.Equal(t, "session-model", got.DefaultModel)
	assert.Equal(t, "medium", got.DefaultReason)
	assert.Equal(t, "reviewer", got.DefaultAgent)
}

func TestStateDiagnostics_SessionAgentSources(t *testing.T) {
	t.Parallel()

	cfg := appconfig.Config{
		DefaultModel: "config-model",
		Agents: map[string]appconfig.AgentConfig{
			"reviewer": {
				Model:          "agent-model",
				ReasoningLevel: "high",
			},
		},
	}
	cfg.Generation.ReasoningLevel = testReasoningLow
	app := stateDiagnosticsTestApp(cfg)
	sessionPrefs := stateSessionPreferences{
		ID:           "demo",
		DefaultAgent: "reviewer",
	}

	model := modelPreferenceReport(cliOptions{}, app, appconfig.State{}, t.TempDir(), sessionPrefs)
	assert.Equal(t, "agent-model", model.Selected)
	assert.Equal(t, "agent.reviewer.model", model.Source)
	assert.Equal(t, "agent", model.Scope)

	reasoning := reasoningPreferenceReport(cliOptions{}, app, appconfig.State{}, t.TempDir(), sessionPrefs)
	assert.Equal(t, "high", reasoning.Selected)
	assert.Equal(t, "agent.reviewer.reasoning_level", reasoning.Source)
	assert.Equal(t, "agent", reasoning.Scope)
}

func TestStateDiagnostics_AgentModelOverridesSessionModel(t *testing.T) {
	t.Parallel()

	cfg := appconfig.Config{
		DefaultModel: "config-model",
		Agents: map[string]appconfig.AgentConfig{
			"reviewer": {
				Model: "agent-model",
			},
		},
	}
	app := stateDiagnosticsTestApp(cfg)
	sessionPrefs := stateSessionPreferences{
		ID:           "demo",
		DefaultModel: "session-model",
		DefaultAgent: "reviewer",
	}

	model := modelPreferenceReport(cliOptions{}, app, appconfig.State{}, t.TempDir(), sessionPrefs)
	assert.Equal(t, "agent-model", model.Selected)
	assert.Equal(t, "agent.reviewer.model", model.Source)
	assert.Equal(t, "agent", model.Scope)
}

func TestStateDiagnostics_FallsBackToConfigSources(t *testing.T) {
	t.Parallel()

	cfg := appconfig.Config{DefaultModel: "config-model"}
	cfg.Generation.ReasoningLevel = testReasoningLow
	app := stateDiagnosticsTestApp(cfg)

	model := modelPreferenceReport(cliOptions{}, app, appconfig.State{}, t.TempDir(), stateSessionPreferences{})
	assert.Equal(t, "config-model", model.Selected)
	assert.Equal(t, "config.default_model", model.Source)
	assert.Equal(t, "config", model.Scope)

	reasoning := reasoningPreferenceReport(cliOptions{}, app, appconfig.State{}, t.TempDir(), stateSessionPreferences{})
	assert.Equal(t, testReasoningLow, reasoning.Selected)
	assert.Equal(t, "config.generation.reasoning_level", reasoning.Source)
	assert.Equal(t, "config", reasoning.Scope)
}

func TestProviderReportsExposeReadinessReasons(t *testing.T) {
	t.Parallel()

	reports := providerReports(llm.ProviderReadinessReport{
		Providers: []llm.ProviderReadiness{
			{
				Name:               "openai",
				Status:             llm.ProviderStatusFailedHealthCheck,
				Registered:         true,
				Configured:         true,
				Requested:          true,
				HealthChecked:      true,
				ModelCatalogSource: llm.ModelCatalogSourceStatic,
				Models:             []string{"gpt-static"},
				StaticModels:       []string{"gpt-static"},
				ModelsStale:        true,
				Error:              assert.AnError,
			},
		},
	})

	require.Len(t, reports, 1)
	assert.Equal(t, "openai", reports[0].Name)
	assert.Equal(t, string(llm.ProviderStatusFailedHealthCheck), reports[0].Status)
	assert.True(t, reports[0].Configured)
	assert.True(t, reports[0].Requested)
	assert.True(t, reports[0].ModelsStale)
	assert.Equal(t, string(llm.ModelCatalogSourceStatic), reports[0].ModelCatalogSource)
	assert.Equal(t, []string{"gpt-static"}, reports[0].Models)
	assert.Contains(t, reports[0].Error, "assert")
}

func TestPrintProviderReadinessReport_ShowsStatusReasonAndModelSource(t *testing.T) { //nolint:paralleltest // Captures process stdout.
	report := llm.ProviderReadinessReport{
		Default: llm.DefaultSelectionReport{
			Provider: "openai",
			Model:    "gpt-test",
		},
		Providers: []llm.ProviderReadiness{
			{
				Name:               "openai",
				Status:             llm.ProviderStatusFailedHealthCheck,
				Registered:         true,
				Configured:         true,
				Requested:          true,
				HealthChecked:      true,
				ModelCatalogSource: llm.ModelCatalogSourceStatic,
				Models:             []string{"gpt-static"},
				StaticModels:       []string{"gpt-static"},
				Error:              errors.New("models HTTP 401"),
			},
			{
				Name:               "ollama",
				Status:             llm.ProviderStatusRegistered,
				Registered:         true,
				Healthy:            true,
				HealthChecked:      true,
				ModelCatalogSource: llm.ModelCatalogSourceLive,
				Models:             []string{"llama3.2"},
				LiveModels:         []string{"llama3.2"},
			},
			{
				Name:               "custom",
				Status:             llm.ProviderStatusRegistered,
				Registered:         true,
				Healthy:            true,
				HealthChecked:      true,
				ModelCatalogSource: llm.ModelCatalogSourceStatic,
				Models:             []string{"custom-static"},
				StaticModels:       []string{"custom-static"},
				ModelFetchError:    errors.New("live models timeout"),
				ModelsStale:        true,
			},
		},
	}

	var healthy int

	out := captureStdoutForStateDiagnostics(t, func() {
		healthy = printProviderReadinessReport(report)
	})

	assert.Equal(t, 2, healthy)
	assert.Contains(t, out, "default_selection:")
	assert.Contains(t, out, "[FAIL] openai configured requested")
	assert.Contains(t, out, "reason: models HTTP 401")
	assert.Contains(t, out, "models: static fallback")
	assert.Contains(t, out, "[ok] ollama")
	assert.Contains(t, out, "models: live")
	assert.Contains(t, out, "[WARN] custom")
	assert.Contains(t, out, "model_fetch: live models timeout")
	assert.Contains(t, out, "models: static fallback (stale)")
}

func TestStartupProviderReadinessSummary_FiltersUnrequestedProviders(t *testing.T) {
	t.Parallel()

	summary := startupProviderReadinessSummary(llm.ProviderReadinessReport{
		Providers: []llm.ProviderReadiness{
			{
				Name:       "openai",
				Status:     llm.ProviderStatusMissingCredential,
				Configured: true,
				Error:      errors.New("no OpenAI credentials found"),
			},
			{
				Name:   "anthropic",
				Status: llm.ProviderStatusMissingCredential,
				Error:  errors.New("no Anthropic credentials found"),
			},
		},
	})

	assert.Contains(t, summary, "openai missing credentials")
	assert.NotContains(t, summary, "anthropic")
}

func TestStartupProviderReadinessSummary_IncludesDefaultSelectionWarnings(t *testing.T) {
	t.Parallel()

	summary := startupProviderReadinessSummary(llm.ProviderReadinessReport{
		Default: llm.DefaultSelectionReport{
			Provider:      "missing-provider",
			Model:         "other/model",
			ProviderError: errors.New("unknown provider"),
			ModelError:    errors.New("model belongs to another provider"),
		},
	})

	assert.Contains(t, summary, "default provider missing-provider ignored: unknown provider")
	assert.Contains(t, summary, "default model other/model ignored: model belongs to another provider")
}

func stateDiagnosticsTestApp(cfg appconfig.Config) appState {
	return appState{
		config:        cfg,
		agentRegistry: agent.NewRegistry(cfg.Agents),
	}
}

func captureStdoutForStateDiagnostics(t *testing.T, fn func()) string {
	t.Helper()

	oldStdout := os.Stdout
	reader, writer, err := os.Pipe()
	require.NoError(t, err)

	os.Stdout = writer

	defer func() {
		os.Stdout = oldStdout
	}()

	fn()

	os.Stdout = oldStdout

	require.NoError(t, writer.Close())

	out, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.NoError(t, reader.Close())

	return string(out)
}
