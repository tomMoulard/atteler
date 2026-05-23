package main

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	appconfig "github.com/tommoulard/atteler/pkg/config"
	"github.com/tommoulard/atteler/pkg/session"
)

const (
	stateDiagnosticsReasoningDefault = "default"
	stateDiagnosticsModelModeDefault = "default"
)

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
	ModelMode      statePreferenceReport `yaml:"model_mode"`
	ReasoningLevel statePreferenceReport `yaml:"reasoning_level"`
}

type statePreferenceReport struct {
	Selected string `yaml:"selected,omitempty"`
	Source   string `yaml:"source"`
	Scope    string `yaml:"scope"`
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
		ModelMode:   modelModePreferenceReport(opts, state, persisted, state.cwd, sessionPrefs),
		ReasoningLevel: reasoningPreferenceReport(
			opts,
			state,
			persisted,
			state.cwd,
			sessionPrefs,
		),
	}

	out, err := yaml.Marshal(report)
	if err != nil {
		return fmt.Errorf("state diagnostics: marshal report: %w", err)
	}

	fmt.Print(string(out))

	return nil
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
	DefaultMode   string
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
		DefaultMode:   strings.TrimSpace(loaded.DefaultModelMode),
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

func modelModePreferenceReport(
	opts cliOptions,
	state appState,
	persisted appconfig.State,
	cwd string,
	sessionPrefs stateSessionPreferences,
) statePreferenceReport {
	if mode := strings.TrimSpace(opts.modelMode); mode != "" {
		return statePreferenceReport{
			Selected: mode,
			Source:   "flag.--model-mode",
			Scope:    "flag",
		}
	}

	if sessionPrefs.DefaultMode != "" {
		return statePreferenceReport{
			Selected: sessionPrefs.DefaultMode,
			Source:   "session.default_model_mode",
			Scope:    "session",
		}
	}

	resolution := persisted.ResolveModelModePreference(cwd)
	if resolution.Source != "" {
		selected := resolution.Value
		if selected == "" {
			selected = stateDiagnosticsModelModeDefault
		}

		return statePreferenceReport{
			Selected: selected,
			Source:   resolution.Source,
			Scope:    string(resolution.Scope),
		}
	}

	agentName := diagnosticAgentName(opts, sessionPrefs)
	if agentName != "" && state.agentRegistry != nil {
		if activeAgent, ok := state.agentRegistry.Get(agentName); ok && strings.TrimSpace(activeAgent.ModelMode) != "" {
			return statePreferenceReport{
				Selected: strings.TrimSpace(activeAgent.ModelMode),
				Source:   "agent." + agentName + ".model_mode",
				Scope:    "agent",
			}
		}
	}

	if mode := strings.TrimSpace(state.config.Generation.ModelMode); mode != "" {
		return statePreferenceReport{
			Selected: mode,
			Source:   "config.generation.model_mode",
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
