// Package config loads atteler configuration from layered YAML files.
package config

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// EnvPath names one or more configuration files that should override the
// default global/local configuration files. Multiple paths can be separated
// with the platform path-list separator.
const EnvPath = "ATTELER_CONFIG"

// Config is the merged application configuration.
//
//nolint:govet // Field order follows file/config grouping; padding is not performance-sensitive.
type Config struct {
	Providers       map[string]ProviderConfig `json:"providers,omitempty" yaml:"providers,omitempty"`
	Agents          map[string]AgentConfig    `json:"agents,omitempty" yaml:"agents,omitempty"`
	Hooks           map[string][]HookConfig   `json:"hooks,omitempty" yaml:"hooks,omitempty"`
	Generation      GenerationConfig          `json:"generation" yaml:"generation"`
	DefaultProvider string                    `json:"default_provider,omitempty" yaml:"default_provider,omitempty"`
	DefaultModel    string                    `json:"default_model,omitempty" yaml:"default_model,omitempty"`
	FallbackModels  []string                  `json:"fallback_models,omitempty" yaml:"fallback_models,omitempty"`
	Context         ContextConfig             `json:"context" yaml:"context"`
	Plugins         PluginConfig              `json:"plugins" yaml:"plugins"`
}

// ProviderConfig configures an individual LLM provider.
type ProviderConfig struct {
	BaseURL  string `json:"base_url,omitempty" yaml:"base_url,omitempty"`
	Disabled bool   `json:"disabled,omitempty" yaml:"disabled,omitempty"`
}

// AgentConfig configures a named agent persona.
type AgentConfig struct {
	Temperature    *float64 `json:"temperature,omitempty" yaml:"temperature,omitempty"`
	TopP           *float64 `json:"top_p,omitempty" yaml:"top_p,omitempty"`
	Seed           *int     `json:"seed,omitempty" yaml:"seed,omitempty"`
	Model          string   `json:"model,omitempty" yaml:"model,omitempty"`
	ReasoningLevel string   `json:"reasoning_level,omitempty" yaml:"reasoning_level,omitempty"`
	Description    string   `json:"description,omitempty" yaml:"description,omitempty"`
	Personality    string   `json:"personality,omitempty" yaml:"personality,omitempty"`
	SystemPrompt   string   `json:"system_prompt,omitempty" yaml:"system_prompt,omitempty"`
	FallbackModels []string `json:"fallback_models,omitempty" yaml:"fallback_models,omitempty"`
	Capabilities   []string `json:"capabilities,omitempty" yaml:"capabilities,omitempty"`
	Triggers       []string `json:"triggers,omitempty" yaml:"triggers,omitempty"`
	MaxTokens      int      `json:"max_tokens,omitempty" yaml:"max_tokens,omitempty"`
}

// HookConfig configures a local command to receive atteler lifecycle events.
type HookConfig struct {
	Env            map[string]string `json:"env,omitempty" yaml:"env,omitempty"`
	Command        []string          `json:"command,omitempty" yaml:"command,omitempty"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty" yaml:"timeout_seconds,omitempty"`
}

// GenerationConfig configures default generation parameters for all requests.
type GenerationConfig struct {
	Temperature    *float64 `json:"temperature,omitempty" yaml:"temperature,omitempty"`
	TopP           *float64 `json:"top_p,omitempty" yaml:"top_p,omitempty"`
	Seed           *int     `json:"seed,omitempty" yaml:"seed,omitempty"`
	ReasoningLevel string   `json:"reasoning_level,omitempty" yaml:"reasoning_level,omitempty"`
	MaxTokens      int      `json:"max_tokens,omitempty" yaml:"max_tokens,omitempty"`
}

// ContextConfig configures local @file prompt references.
type ContextConfig struct {
	MaxFileBytes   int `json:"max_file_bytes,omitempty" yaml:"max_file_bytes,omitempty"`
	MaxTotalBytes  int `json:"max_total_bytes,omitempty" yaml:"max_total_bytes,omitempty"`
	MaxInputTokens int `json:"max_input_tokens,omitempty" yaml:"max_input_tokens,omitempty"`
}

// PluginConfig configures local plugin manifest discovery.
type PluginConfig struct {
	Paths []string `json:"paths,omitempty" yaml:"paths,omitempty"`
}

type fileConfig struct {
	Generation      fileGenerationConfig          `json:"generation" yaml:"generation"`
	Context         fileContextConfig             `json:"context" yaml:"context"`
	Plugins         filePluginConfig              `json:"plugins" yaml:"plugins"`
	DefaultProvider *string                       `json:"default_provider" yaml:"default_provider"`
	DefaultModel    *string                       `json:"default_model" yaml:"default_model"`
	Providers       map[string]fileProviderConfig `json:"providers" yaml:"providers"`
	Agents          map[string]fileAgentConfig    `json:"agents" yaml:"agents"`
	Hooks           map[string][]HookConfig       `json:"hooks" yaml:"hooks"`
	FallbackModels  []string                      `json:"fallback_models" yaml:"fallback_models"`
}

type fileProviderConfig struct {
	Disabled *bool   `json:"disabled" yaml:"disabled"`
	BaseURL  *string `json:"base_url" yaml:"base_url"`
}

type fileAgentConfig struct {
	Temperature    *float64 `json:"temperature" yaml:"temperature"`
	TopP           *float64 `json:"top_p" yaml:"top_p"`
	Seed           *int     `json:"seed" yaml:"seed"`
	Model          *string  `json:"model" yaml:"model"`
	ReasoningLevel *string  `json:"reasoning_level" yaml:"reasoning_level"`
	Description    *string  `json:"description" yaml:"description"`
	Personality    *string  `json:"personality" yaml:"personality"`
	SystemPrompt   *string  `json:"system_prompt" yaml:"system_prompt"`
	FallbackModels []string `json:"fallback_models" yaml:"fallback_models"`
	Capabilities   []string `json:"capabilities" yaml:"capabilities"`
	MaxTokens      *int     `json:"max_tokens" yaml:"max_tokens"`
	Triggers       []string `json:"triggers" yaml:"triggers"`
}

type fileContextConfig struct {
	MaxFileBytes   *int `json:"max_file_bytes" yaml:"max_file_bytes"`
	MaxTotalBytes  *int `json:"max_total_bytes" yaml:"max_total_bytes"`
	MaxInputTokens *int `json:"max_input_tokens" yaml:"max_input_tokens"`
}

type fileGenerationConfig struct {
	Temperature    *float64 `json:"temperature" yaml:"temperature"`
	TopP           *float64 `json:"top_p" yaml:"top_p"`
	Seed           *int     `json:"seed" yaml:"seed"`
	ReasoningLevel *string  `json:"reasoning_level" yaml:"reasoning_level"`
	MaxTokens      *int     `json:"max_tokens" yaml:"max_tokens"`
}

type filePluginConfig struct {
	Paths []string `json:"paths" yaml:"paths"`
}

// Load reads the default configuration files and returns the merged result plus
// the paths that were successfully loaded. Missing files are ignored.
func Load() (Config, []string, error) {
	cfg, loaded := LoadHarnessDefaults()

	fileCfg, fileLoaded, err := LoadFiles(DefaultPaths())
	mergeConfig(&cfg, fileCfg)
	loaded = append(loaded, fileLoaded...)
	if len(cfg.Providers) == 0 {
		cfg.Providers = nil
	}
	if len(cfg.Agents) == 0 {
		cfg.Agents = nil
	}
	if len(cfg.Hooks) == 0 {
		cfg.Hooks = nil
	}

	return cfg, loaded, err
}

// DefaultPaths returns the configuration files in merge order. Later files
// override earlier files.
func DefaultPaths() []string {
	paths := make([]string, 0, 8)
	paths = append(paths, globalPaths()...)

	if cwd, err := os.Getwd(); err == nil {
		paths = append(paths,
			filepath.Join(cwd, ".atteler", "config.yaml"),
			filepath.Join(cwd, ".atteler", "config.yml"),
			filepath.Join(cwd, ".atteler", "config.json"),
			filepath.Join(cwd, ".atteler.yaml"),
			filepath.Join(cwd, ".atteler.yml"),
			filepath.Join(cwd, ".atteler.json"),
		)
	}

	if envValue := os.Getenv(EnvPath); envValue != "" {
		for _, path := range filepath.SplitList(envValue) {
			if strings.TrimSpace(path) != "" {
				paths = append(paths, path)
			}
		}
	}

	return paths
}

// LoadFiles reads and merges explicit YAML or JSON configuration files. Later
// files override earlier files. Missing files are ignored so callers can pass
// the full set of conventional paths without probing them first.
func LoadFiles(paths []string) (Config, []string, error) {
	cfg := Config{}
	loaded := make([]string, 0, len(paths))

	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}

		data, err := os.ReadFile(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return cfg, loaded, fmt.Errorf("config: read %s: %w", path, err)
		}

		var next fileConfig
		if err := yaml.Unmarshal(data, &next); err != nil {
			return cfg, loaded, fmt.Errorf("config: parse %s: %w", path, err)
		}

		merge(&cfg, next)
		loaded = append(loaded, path)
	}

	if len(cfg.Providers) == 0 {
		cfg.Providers = nil
	}
	if len(cfg.Agents) == 0 {
		cfg.Agents = nil
	}
	if len(cfg.Hooks) == 0 {
		cfg.Hooks = nil
	}

	return cfg, loaded, nil
}

func globalPaths() []string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return configPaths(dir)
	}

	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil
	}
	return configPaths(filepath.Join(home, ".config"))
}

func configPaths(configHome string) []string {
	dir := filepath.Join(configHome, "atteler")
	return []string{
		filepath.Join(dir, "config.yaml"),
		filepath.Join(dir, "config.yml"),
		filepath.Join(dir, "config.json"),
	}
}

func merge(dst *Config, src fileConfig) {
	if src.DefaultProvider != nil {
		dst.DefaultProvider = *src.DefaultProvider
	}
	if src.DefaultModel != nil {
		dst.DefaultModel = *src.DefaultModel
	}
	if src.FallbackModels != nil {
		dst.FallbackModels = append([]string(nil), src.FallbackModels...)
	}

	mergeProviders(dst, src.Providers)
	mergeAgents(dst, src.Agents)
	mergeHooks(dst, src.Hooks)
	mergeGeneration(dst, src.Generation)
	mergeContext(dst, src.Context)
	mergePlugins(dst, src.Plugins)
}

func mergeProviders(dst *Config, providers map[string]fileProviderConfig) {
	for name, provider := range providers {
		if dst.Providers == nil {
			dst.Providers = make(map[string]ProviderConfig)
		}

		current := dst.Providers[name]
		if provider.Disabled != nil {
			current.Disabled = *provider.Disabled
		}
		if provider.BaseURL != nil {
			current.BaseURL = *provider.BaseURL
		}
		dst.Providers[name] = current
	}
}

func mergeAgents(dst *Config, agents map[string]fileAgentConfig) {
	for name := range agents {
		if dst.Agents == nil {
			dst.Agents = make(map[string]AgentConfig)
		}

		current := dst.Agents[name]
		mergeFileAgent(&current, agents[name])
		dst.Agents[name] = current
	}
}

func mergeFileAgent(current *AgentConfig, agent fileAgentConfig) {
	if agent.Model != nil {
		current.Model = *agent.Model
	}
	if agent.SystemPrompt != nil {
		current.SystemPrompt = *agent.SystemPrompt
	}
	if agent.ReasoningLevel != nil {
		current.ReasoningLevel = strings.TrimSpace(*agent.ReasoningLevel)
	}
	if agent.Description != nil {
		current.Description = *agent.Description
	}
	if agent.Personality != nil {
		current.Personality = *agent.Personality
	}
	if agent.FallbackModels != nil {
		current.FallbackModels = append([]string(nil), agent.FallbackModels...)
	}
	if agent.Capabilities != nil {
		current.Capabilities = append([]string(nil), agent.Capabilities...)
	}
	if agent.Temperature != nil {
		current.Temperature = agent.Temperature
	}
	if agent.TopP != nil {
		current.TopP = agent.TopP
	}
	if agent.Seed != nil {
		current.Seed = agent.Seed
	}
	if agent.Triggers != nil {
		current.Triggers = append([]string(nil), agent.Triggers...)
	}
	if agent.MaxTokens != nil {
		current.MaxTokens = *agent.MaxTokens
	}
}

func mergeHooks(dst *Config, hooks map[string][]HookConfig) {
	for eventType, eventHooks := range hooks {
		if dst.Hooks == nil {
			dst.Hooks = make(map[string][]HookConfig)
		}

		merged := make([]HookConfig, 0, len(eventHooks))
		for _, hook := range eventHooks {
			next := HookConfig{
				Command: append([]string(nil), hook.Command...),
				Env:     cloneMap(hook.Env),
			}
			next.TimeoutSeconds = hook.TimeoutSeconds
			merged = append(merged, next)
		}
		dst.Hooks[eventType] = merged
	}
}

func mergeContext(dst *Config, contextConfig fileContextConfig) {
	if contextConfig.MaxFileBytes != nil {
		dst.Context.MaxFileBytes = *contextConfig.MaxFileBytes
	}
	if contextConfig.MaxTotalBytes != nil {
		dst.Context.MaxTotalBytes = *contextConfig.MaxTotalBytes
	}
	if contextConfig.MaxInputTokens != nil {
		dst.Context.MaxInputTokens = *contextConfig.MaxInputTokens
	}
}

func mergeGeneration(dst *Config, generation fileGenerationConfig) {
	if generation.Temperature != nil {
		dst.Generation.Temperature = generation.Temperature
	}
	if generation.TopP != nil {
		dst.Generation.TopP = generation.TopP
	}
	if generation.Seed != nil {
		dst.Generation.Seed = generation.Seed
	}
	if generation.ReasoningLevel != nil {
		dst.Generation.ReasoningLevel = strings.TrimSpace(*generation.ReasoningLevel)
	}
	if generation.MaxTokens != nil {
		dst.Generation.MaxTokens = *generation.MaxTokens
	}
}

func mergePlugins(dst *Config, plugins filePluginConfig) {
	if plugins.Paths != nil {
		dst.Plugins.Paths = append([]string(nil), plugins.Paths...)
	}
}

func mergeConfig(dst *Config, src Config) {
	if src.DefaultProvider != "" {
		dst.DefaultProvider = src.DefaultProvider
	}
	if src.DefaultModel != "" {
		dst.DefaultModel = src.DefaultModel
	}
	if src.FallbackModels != nil {
		dst.FallbackModels = append([]string(nil), src.FallbackModels...)
	}

	mergeConfigProviders(dst, src.Providers)
	mergeConfigAgents(dst, src.Agents)
	mergeConfigHooks(dst, src.Hooks)
	mergeConfigGeneration(dst, src.Generation)
	mergeConfigContext(dst, src.Context)
	mergeConfigPlugins(dst, src.Plugins)
}

func mergeConfigProviders(dst *Config, providers map[string]ProviderConfig) {
	for name, provider := range providers {
		if dst.Providers == nil {
			dst.Providers = make(map[string]ProviderConfig)
		}

		current := dst.Providers[name]
		if provider.BaseURL != "" {
			current.BaseURL = provider.BaseURL
		}
		current.Disabled = provider.Disabled
		dst.Providers[name] = current
	}
}

func mergeConfigAgents(dst *Config, agents map[string]AgentConfig) {
	for name := range agents {
		if dst.Agents == nil {
			dst.Agents = make(map[string]AgentConfig)
		}

		current := dst.Agents[name]
		mergeConfigAgent(&current, agents[name])
		dst.Agents[name] = current
	}
}

func mergeConfigAgent(current *AgentConfig, agent AgentConfig) {
	if agent.Model != "" {
		current.Model = agent.Model
	}
	if agent.SystemPrompt != "" {
		current.SystemPrompt = agent.SystemPrompt
	}
	if agent.ReasoningLevel != "" {
		current.ReasoningLevel = strings.TrimSpace(agent.ReasoningLevel)
	}
	if agent.Description != "" {
		current.Description = agent.Description
	}
	if agent.Personality != "" {
		current.Personality = agent.Personality
	}
	if agent.FallbackModels != nil {
		current.FallbackModels = append([]string(nil), agent.FallbackModels...)
	}
	if agent.Capabilities != nil {
		current.Capabilities = append([]string(nil), agent.Capabilities...)
	}
	if agent.Temperature != nil {
		current.Temperature = agent.Temperature
	}
	if agent.TopP != nil {
		current.TopP = agent.TopP
	}
	if agent.Seed != nil {
		current.Seed = agent.Seed
	}
	if agent.Triggers != nil {
		current.Triggers = append([]string(nil), agent.Triggers...)
	}
	if agent.MaxTokens > 0 {
		current.MaxTokens = agent.MaxTokens
	}
}

func mergeConfigHooks(dst *Config, hooks map[string][]HookConfig) {
	for eventType, eventHooks := range hooks {
		if dst.Hooks == nil {
			dst.Hooks = make(map[string][]HookConfig)
		}
		dst.Hooks[eventType] = cloneHooks(eventHooks)
	}
}

func mergeConfigContext(dst *Config, contextConfig ContextConfig) {
	if contextConfig.MaxFileBytes > 0 {
		dst.Context.MaxFileBytes = contextConfig.MaxFileBytes
	}
	if contextConfig.MaxTotalBytes > 0 {
		dst.Context.MaxTotalBytes = contextConfig.MaxTotalBytes
	}
	if contextConfig.MaxInputTokens > 0 {
		dst.Context.MaxInputTokens = contextConfig.MaxInputTokens
	}
}

func mergeConfigGeneration(dst *Config, generation GenerationConfig) {
	if generation.Temperature != nil {
		dst.Generation.Temperature = generation.Temperature
	}
	if generation.TopP != nil {
		dst.Generation.TopP = generation.TopP
	}
	if generation.Seed != nil {
		dst.Generation.Seed = generation.Seed
	}
	if generation.ReasoningLevel != "" {
		dst.Generation.ReasoningLevel = strings.TrimSpace(generation.ReasoningLevel)
	}
	if generation.MaxTokens > 0 {
		dst.Generation.MaxTokens = generation.MaxTokens
	}
}

func mergeConfigPlugins(dst *Config, plugins PluginConfig) {
	if plugins.Paths != nil {
		dst.Plugins.Paths = append([]string(nil), plugins.Paths...)
	}
}

func cloneHooks(hooks []HookConfig) []HookConfig {
	out := make([]HookConfig, 0, len(hooks))
	for _, hook := range hooks {
		out = append(out, HookConfig{
			Command:        append([]string(nil), hook.Command...),
			Env:            cloneMap(hook.Env),
			TimeoutSeconds: hook.TimeoutSeconds,
		})
	}
	return out
}

func cloneMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}

	out := make(map[string]string, len(in))
	maps.Copy(out, in)
	return out
}
