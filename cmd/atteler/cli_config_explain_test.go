package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	appconfig "github.com/tommoulard/atteler/pkg/config"
	"github.com/tommoulard/atteler/pkg/llm"
)

func TestWriteConfigExplanation_IncludesProviderModelAndRuntimeProvenance(t *testing.T) {
	t.Parallel()

	origins := appconfig.OriginMap{
		"default_model": {
			Chain: []appconfig.OriginEvent{{
				Kind:      appconfig.OriginProjectFile,
				Operation: appconfig.OriginSet,
				Source:    ".atteler/config.yaml",
				Value:     "config-model",
			}},
		},
		"default_provider": {
			Chain: []appconfig.OriginEvent{{
				Kind:      appconfig.OriginProjectFile,
				Operation: appconfig.OriginSet,
				Source:    ".atteler/config.yaml",
				Value:     "openai",
			}},
		},
		"generation.reasoning_level": {
			Chain: []appconfig.OriginEvent{{
				Kind:      appconfig.OriginProjectFile,
				Operation: appconfig.OriginSet,
				Source:    ".atteler/config.yaml",
				Value:     "medium",
			}},
		},
		"generation.model_mode": {
			Chain: []appconfig.OriginEvent{{
				Kind:      appconfig.OriginProjectFile,
				Operation: appconfig.OriginSet,
				Source:    ".atteler/config.yaml",
				Value:     "default",
			}},
		},
		"providers.openai.base_url": {
			Chain: []appconfig.OriginEvent{{
				Kind:      appconfig.OriginProjectFile,
				Operation: appconfig.OriginSet,
				Source:    ".atteler/config.yaml",
				Value:     "https://openai.project",
			}},
		},
	}
	cfg := appconfig.Config{
		DefaultProvider: "openai",
		DefaultModel:    "config-model",
		Generation: appconfig.GenerationConfig{
			ReasoningLevel: "medium",
			ModelMode:      "default",
		},
	}

	addRuntimeConfigOrigins(
		origins,
		cfg,
		cliOptions{model: "cli-model", reasoningLevel: "high", modelMode: "fast"},
		appconfig.State{DefaultReasoningLevel: "low", DefaultModelMode: "default"},
		"/repo",
		"state.yaml",
	)

	var out strings.Builder
	writeConfigExplanation(&out, []string{".atteler/config.yaml"}, origins, "")
	got := out.String()

	assert.Contains(t, got, "Precedence (lowest to highest):")
	assert.Contains(t, got, "providers.openai.base_url: https://openai.project")
	assert.Contains(t, got, "runtime.selected_model: cli-model")
	assert.Contains(t, got, "--model [cli-flag]")
	assert.Contains(t, got, "runtime.selected_provider: openai")
	assert.Contains(t, got, "runtime.generation.reasoning_level: high")
	assert.Contains(t, got, "state.yaml global [state-override]")
	assert.Contains(t, got, "--reasoning-level [cli-flag]")
	assert.Contains(t, got, "runtime.generation.model_mode: fast")
	assert.Contains(t, got, "--model-mode [cli-flag]")
}

func TestAddRuntimeConfigOrigins_UsesProviderQualifiedModelPrefix(t *testing.T) {
	t.Parallel()

	origins := appconfig.OriginMap{}
	addRuntimeConfigOrigins(
		origins,
		appconfig.Config{DefaultProvider: "anthropic"},
		cliOptions{model: "openai/gpt-test"},
		appconfig.State{},
		"/repo",
		"state.yaml",
	)

	final, ok := origins.Final("runtime.selected_provider")
	require.True(t, ok)
	assert.Equal(t, "openai", final.Value)
	assert.Equal(t, appconfig.OriginRuntimeSelection, final.Kind)
	assert.Contains(t, final.Note, "provider-qualified")
}

func TestAddRuntimeConfigOrigins_ModelModeDefaultSentinelFromState(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	state := appconfig.State{DefaultModelMode: llm.ModelModeFast}
	state.SetModelMode(appconfig.ModelScopeFolder, dir, llm.ModelModeDefault)

	origins := appconfig.OriginMap{
		"generation.model_mode": {
			Chain: []appconfig.OriginEvent{{
				Kind:      appconfig.OriginProjectFile,
				Operation: appconfig.OriginSet,
				Source:    ".atteler/config.yaml",
				Value:     llm.ModelModeFast,
			}},
		},
	}
	addRuntimeConfigOrigins(
		origins,
		appconfig.Config{Generation: appconfig.GenerationConfig{ModelMode: llm.ModelModeFast}},
		cliOptions{},
		state,
		dir,
		"state.yaml",
	)

	final, ok := origins.Final("runtime.generation.model_mode")
	require.True(t, ok)
	assert.Equal(t, llm.ModelModeDefault, final.Value)
	assert.Equal(t, appconfig.OriginStateOverride, final.Kind)
	assert.Contains(t, final.Source, "state.yaml folder ")
}

func TestWriteConfigExplanation_FiltersFieldPrefixes(t *testing.T) {
	t.Parallel()

	origins := appconfig.OriginMap{
		"default_model": {
			Chain: []appconfig.OriginEvent{{
				Kind:      appconfig.OriginProjectFile,
				Operation: appconfig.OriginSet,
				Source:    ".atteler/config.yaml",
				Value:     "config-model",
			}},
		},
		"providers.openai.base_url": {
			Chain: []appconfig.OriginEvent{{
				Kind:      appconfig.OriginProjectFile,
				Operation: appconfig.OriginSet,
				Source:    ".atteler/config.yaml",
				Value:     "https://openai.project",
			}},
		},
		"providers.anthropic.base_url": {
			Chain: []appconfig.OriginEvent{{
				Kind:      appconfig.OriginProjectFile,
				Operation: appconfig.OriginSet,
				Source:    ".atteler/config.yaml",
				Value:     "https://anthropic.project",
			}},
		},
	}

	var out strings.Builder
	writeConfigExplanation(&out, []string{".atteler/config.yaml"}, origins, "providers.openai")
	got := out.String()

	assert.Contains(t, got, `Field origins matching "providers.openai":`)
	assert.Contains(t, got, "providers.openai.base_url: https://openai.project")
	assert.NotContains(t, got, "providers.anthropic.base_url:")
	assert.NotContains(t, got, "default_model:")
}
