package main

import (
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	appconfig "github.com/tommoulard/atteler/pkg/config"
	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/session"
)

const stateDiagnosticsReasoningDefault = "default"

//nolint:govet // field order follows the user-facing YAML report.
type stateDiagnosticsReport struct {
	StatePath      string                `yaml:"state_path"`
	StateStatus    string                `yaml:"state_status"`
	CWD            string                `yaml:"cwd"`
	FolderKey      string                `yaml:"folder_key,omitempty"`
	SessionID      string                `yaml:"session_id,omitempty"`
	AgentName      string                `yaml:"agent,omitempty"`
	Version        int                   `yaml:"version"`
	Revision       int64                 `yaml:"revision"`
	Model          statePreferenceReport `yaml:"model"`
	ReasoningLevel statePreferenceReport `yaml:"reasoning_level"`
	Providers      []stateProviderReport `yaml:"providers,omitempty"`
}

type statePreferenceReport struct {
	Selected string `yaml:"selected,omitempty"`
	Source   string `yaml:"source"`
	Scope    string `yaml:"scope"`
}

type stateProviderReport struct {
	Error              string   `yaml:"error,omitempty"`
	HealthError        string   `yaml:"health_error,omitempty"`
	ModelFetchError    string   `yaml:"model_fetch_error,omitempty"`
	CheckedAt          string   `yaml:"checked_at,omitempty"`
	Name               string   `yaml:"name"`
	Status             string   `yaml:"status"`
	ModelCatalogSource string   `yaml:"model_catalog_source,omitempty"`
	Models             []string `yaml:"models,omitempty"`
	StaticModels       []string `yaml:"static_models,omitempty"`
	LiveModels         []string `yaml:"live_models,omitempty"`
	Registered         bool     `yaml:"registered"`
	Configured         bool     `yaml:"configured,omitempty"`
	Requested          bool     `yaml:"requested,omitempty"`
	HealthChecked      bool     `yaml:"health_checked,omitempty"`
	HealthCached       bool     `yaml:"health_cached,omitempty"`
	Healthy            bool     `yaml:"healthy,omitempty"`
}

func printStateDiagnostics(opts cliOptions, state appState) error {
	store := appconfig.NewStateStore("")

	persisted, err := store.Load()
	if err != nil {
		return fmt.Errorf("state diagnostics: load %s: %w", store.Path(), err)
	}

	sessionPrefs, err := stateDiagnosticSession(opts, state.sessionStore)
	if err != nil {
		return err
	}

	report := stateDiagnosticsReport{
		StatePath:   store.Path(),
		StateStatus: configPathStatus(store.Path()),
		CWD:         state.cwd,
		FolderKey:   appconfig.FolderKey(state.cwd),
		SessionID:   sessionPrefs.ID,
		AgentName:   diagnosticAgentName(opts, sessionPrefs),
		Version:     stateDiagnosticVersion(persisted),
		Revision:    persisted.Revision,
		Model:       modelPreferenceReport(opts, state, persisted, state.cwd, sessionPrefs),
		ReasoningLevel: reasoningPreferenceReport(
			opts,
			state,
			persisted,
			state.cwd,
			sessionPrefs,
		),
		Providers: providerReports(state.providerReadiness),
	}

	out, err := yaml.Marshal(report)
	if err != nil {
		return fmt.Errorf("state diagnostics: marshal report: %w", err)
	}

	fmt.Print(string(out))

	return nil
}

func providerReports(report llm.ProviderReadinessReport) []stateProviderReport {
	out := make([]stateProviderReport, 0, len(report.Providers))
	for i := range report.Providers {
		provider := &report.Providers[i]
		out = append(out, stateProviderReport{
			Name:               provider.Name,
			Status:             string(provider.Status),
			Registered:         provider.Registered,
			Configured:         provider.Configured,
			Requested:          provider.Requested,
			HealthChecked:      provider.HealthChecked,
			HealthCached:       provider.HealthCached,
			Healthy:            provider.Healthy,
			CheckedAt:          formatProviderCheckedAt(provider.CheckedAt),
			ModelCatalogSource: string(provider.ModelCatalogSource),
			Models:             append([]string(nil), provider.Models...),
			StaticModels:       append([]string(nil), provider.StaticModels...),
			LiveModels:         append([]string(nil), provider.LiveModels...),
			Error:              errorString(provider.Error),
			HealthError:        errorString(provider.HealthError),
			ModelFetchError:    errorString(provider.ModelFetchError),
		})
	}

	return out
}

func formatProviderCheckedAt(checkedAt time.Time) string {
	if checkedAt.IsZero() {
		return ""
	}

	return checkedAt.UTC().Format(time.RFC3339)
}

func stateDiagnosticVersion(state appconfig.State) int {
	if state.Version > 0 {
		return state.Version
	}

	return appconfig.StateSchemaVersion
}

type stateSessionPreferences struct {
	ID            string
	DefaultModel  string
	DefaultReason string
	DefaultAgent  string
}

func stateDiagnosticSession(opts cliOptions, store *session.Store) (stateSessionPreferences, error) {
	ref := strings.TrimSpace(opts.sessionRef)
	if ref == "" {
		return stateSessionPreferences{}, nil
	}

	if store == nil {
		return stateSessionPreferences{}, fmt.Errorf("state diagnostics: session %s requested but no session store is available", ref)
	}

	loaded, err := store.Load(ref)
	if err != nil {
		return stateSessionPreferences{}, fmt.Errorf("state diagnostics: load session %s: %w", ref, err)
	}

	return stateSessionPreferences{
		ID:            loaded.ID,
		DefaultModel:  strings.TrimSpace(loaded.DefaultModel),
		DefaultReason: strings.TrimSpace(loaded.DefaultReasoningLevel),
		DefaultAgent:  strings.TrimSpace(loaded.DefaultAgent),
	}, nil
}

func modelPreferenceReport(
	opts cliOptions,
	state appState,
	persisted appconfig.State,
	cwd string,
	sessionPrefs stateSessionPreferences,
) statePreferenceReport {
	if model := strings.TrimSpace(opts.model); model != "" {
		return statePreferenceReport{
			Selected: model,
			Source:   "flag.--model",
			Scope:    "flag",
		}
	}

	agentName := diagnosticAgentName(opts, sessionPrefs)
	if agentName != "" && state.agentRegistry != nil {
		if activeAgent, ok := state.agentRegistry.Get(agentName); ok && strings.TrimSpace(activeAgent.Model) != "" {
			return statePreferenceReport{
				Selected: strings.TrimSpace(activeAgent.Model),
				Source:   "agent." + agentName + ".model",
				Scope:    "agent",
			}
		}
	}

	if sessionPrefs.DefaultModel != "" {
		return statePreferenceReport{
			Selected: sessionPrefs.DefaultModel,
			Source:   "session.default_model",
			Scope:    "session",
		}
	}

	resolution := persisted.ResolveModelPreference(cwd)
	if resolution.Source != "" {
		return statePreferenceReport{
			Selected: resolution.Value,
			Source:   resolution.Source,
			Scope:    string(resolution.Scope),
		}
	}

	if model := strings.TrimSpace(state.config.DefaultModel); model != "" {
		return statePreferenceReport{
			Selected: model,
			Source:   "config.default_model",
			Scope:    "config",
		}
	}

	return statePreferenceReport{Source: "none", Scope: "none"}
}

func reasoningPreferenceReport(
	opts cliOptions,
	state appState,
	persisted appconfig.State,
	cwd string,
	sessionPrefs stateSessionPreferences,
) statePreferenceReport {
	if level := strings.TrimSpace(opts.reasoningLevel); level != "" {
		return statePreferenceReport{
			Selected: level,
			Source:   "flag.--reasoning-level",
			Scope:    "flag",
		}
	}

	if sessionPrefs.DefaultReason != "" {
		return statePreferenceReport{
			Selected: sessionPrefs.DefaultReason,
			Source:   "session.default_reasoning_level",
			Scope:    "session",
		}
	}

	resolution := persisted.ResolveReasoningPreference(cwd)
	if resolution.Source != "" {
		selected := resolution.Value
		if selected == "" {
			selected = stateDiagnosticsReasoningDefault
		}

		return statePreferenceReport{
			Selected: selected,
			Source:   resolution.Source,
			Scope:    string(resolution.Scope),
		}
	}

	agentName := diagnosticAgentName(opts, sessionPrefs)
	if agentName != "" && state.agentRegistry != nil {
		if activeAgent, ok := state.agentRegistry.Get(agentName); ok && strings.TrimSpace(activeAgent.ReasoningLevel) != "" {
			return statePreferenceReport{
				Selected: strings.TrimSpace(activeAgent.ReasoningLevel),
				Source:   "agent." + agentName + ".reasoning_level",
				Scope:    "agent",
			}
		}
	}

	if level := strings.TrimSpace(state.config.Generation.ReasoningLevel); level != "" {
		return statePreferenceReport{
			Selected: level,
			Source:   "config.generation.reasoning_level",
			Scope:    "config",
		}
	}

	return statePreferenceReport{Source: "none", Scope: "none"}
}

func diagnosticAgentName(opts cliOptions, sessionPrefs stateSessionPreferences) string {
	if agentName := strings.TrimSpace(opts.agentName); agentName != "" {
		return agentName
	}

	return sessionPrefs.DefaultAgent
}
