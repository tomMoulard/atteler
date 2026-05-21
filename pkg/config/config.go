// Package config loads atteler configuration from layered YAML files.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	attelerplugin "github.com/tommoulard/atteler/pkg/plugin"
)

// EnvPath names one or more configuration files that should override the
// default global/local configuration files. Multiple paths can be separated
// with the platform path-list separator.
const EnvPath = "ATTELER_CONFIG"

// Config is the merged application configuration.
//
//nolint:govet // fieldalignment: field order follows config-file grouping.
type Config struct {
	Providers       map[string]ProviderConfig `json:"providers,omitempty" yaml:"providers,omitempty"`
	Agents          map[string]AgentConfig    `json:"agents,omitempty" yaml:"agents,omitempty"`
	Hooks           map[string][]HookConfig   `json:"hooks,omitempty" yaml:"hooks,omitempty"`
	Generation      GenerationConfig          `json:"generation" yaml:"generation"`
	AgentLoop       AgentLoopConfig           `json:"agent_loop" yaml:"agent_loop"`
	DefaultProvider string                    `json:"default_provider,omitempty" yaml:"default_provider,omitempty"`
	DefaultModel    string                    `json:"default_model,omitempty" yaml:"default_model,omitempty"`
	FallbackModels  []string                  `json:"fallback_models,omitempty" yaml:"fallback_models,omitempty"`
	Context         ContextConfig             `json:"context" yaml:"context"`
	Plugins         PluginConfig              `json:"plugins" yaml:"plugins"`
}

// ProviderConfig configures an individual LLM provider.
type ProviderConfig struct {
	BaseURL        string `json:"base_url,omitempty" yaml:"base_url,omitempty"`
	Disabled       bool   `json:"disabled,omitempty" yaml:"disabled,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" yaml:"timeout_seconds,omitempty"`
}

// AgentConfig configures a named agent persona.
type AgentConfig struct {
	Temperature *float64 `json:"temperature,omitempty" yaml:"temperature,omitempty"`
	TopP        *float64 `json:"top_p,omitempty" yaml:"top_p,omitempty"`
	Seed        *int     `json:"seed,omitempty" yaml:"seed,omitempty"`

	ToolPermissions map[string]bool `json:"tools,omitempty" yaml:"tools,omitempty"`

	Model          string   `json:"model,omitempty" yaml:"model,omitempty"`
	Mode           string   `json:"mode,omitempty" yaml:"mode,omitempty"`
	ReasoningLevel string   `json:"reasoning_level,omitempty" yaml:"reasoning_level,omitempty"`
	Description    string   `json:"description,omitempty" yaml:"description,omitempty"`
	Personality    string   `json:"personality,omitempty" yaml:"personality,omitempty"`
	SystemPrompt   string   `json:"system_prompt,omitempty" yaml:"system_prompt,omitempty"`
	FallbackModels []string `json:"fallback_models,omitempty" yaml:"fallback_models,omitempty"`
	Capabilities   []string `json:"capabilities,omitempty" yaml:"capabilities,omitempty"`
	Triggers       []string `json:"triggers,omitempty" yaml:"triggers,omitempty"`
	References     []string `json:"references,omitempty" yaml:"references,omitempty"`
	MaxTokens      int      `json:"max_tokens,omitempty" yaml:"max_tokens,omitempty"`
	Hidden         bool     `json:"hidden,omitempty" yaml:"hidden,omitempty"`

	hiddenSet bool
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

// AgentLoopConfig configures the multi-turn tool execution loop.
type AgentLoopConfig struct {
	// MaxOutputBytes caps cumulative raw tool output per agent loop. Zero or nil
	// disables the cap.
	MaxOutputBytes *int64 `json:"max_output_bytes,omitempty" yaml:"max_output_bytes,omitempty"`
	// MaxTotalTokens caps cumulative model input plus output tokens per agent
	// loop. Zero or nil disables the cap.
	MaxTotalTokens *int `json:"max_total_tokens,omitempty" yaml:"max_total_tokens,omitempty"`
	// MaxIterations caps the number of tool-use turns per agent loop. Zero or
	// nil disables the cap (the loop runs until the model returns a final
	// response or another budget — model calls, tool calls, wall time —
	// trips). Defaults to nil (unlimited).
	MaxIterations *int `json:"max_iterations,omitempty" yaml:"max_iterations,omitempty"`
}

// ContextConfig configures local @file prompt references and configured
// references that are loaded at startup or per-agent.
//
//nolint:govet // field order follows config-file grouping.
type ContextConfig struct {
	References      []string              `json:"references,omitempty" yaml:"references,omitempty"`
	MaxFileBytes    int                   `json:"max_file_bytes,omitempty" yaml:"max_file_bytes,omitempty"`
	MaxTotalBytes   int                   `json:"max_total_bytes,omitempty" yaml:"max_total_bytes,omitempty"`
	MaxInputTokens  int                   `json:"max_input_tokens,omitempty" yaml:"max_input_tokens,omitempty"`
	ReferencePolicy ReferencePolicyConfig `json:"reference_policy" yaml:"reference_policy"`
}

// ReferencePolicyConfig controls which configured references may be ingested.
type ReferencePolicyConfig struct {
	AllowedSchemes       []string `json:"allowed_schemes,omitempty" yaml:"allowed_schemes,omitempty"`
	AllowedHosts         []string `json:"allowed_hosts,omitempty" yaml:"allowed_hosts,omitempty"`
	LocalRoots           []string `json:"local_roots,omitempty" yaml:"local_roots,omitempty"`
	ContentTypes         []string `json:"content_types,omitempty" yaml:"content_types,omitempty"`
	MaxRedirects         int      `json:"max_redirects,omitempty" yaml:"max_redirects,omitempty"`
	AllowPrivateNetworks bool     `json:"allow_private_networks,omitempty" yaml:"allow_private_networks,omitempty"`
}

// PluginConfig configures local plugin manifest discovery.
type PluginConfig struct {
	Policy *attelerplugin.Policy `json:"policy,omitempty" yaml:"policy,omitempty"`
	Paths  []string              `json:"paths,omitempty" yaml:"paths,omitempty"`
}

type fileConfig struct {
	Generation      fileGenerationConfig          `json:"generation" yaml:"generation"`
	AgentLoop       fileAgentLoopConfig           `json:"agent_loop" yaml:"agent_loop"`
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
	Disabled       *bool   `json:"disabled" yaml:"disabled"`
	BaseURL        *string `json:"base_url" yaml:"base_url"`
	TimeoutSeconds *int    `json:"timeout_seconds" yaml:"timeout_seconds"`
}

type fileAgentConfig struct {
	Personality     *string         `json:"personality" yaml:"personality"`
	TopP            *float64        `json:"top_p" yaml:"top_p"`
	Seed            *int            `json:"seed" yaml:"seed"`
	Model           *string         `json:"model" yaml:"model"`
	Mode            *string         `json:"mode" yaml:"mode"`
	ReasoningLevel  *string         `json:"reasoning_level" yaml:"reasoning_level"`
	Description     *string         `json:"description" yaml:"description"`
	Temperature     *float64        `json:"temperature" yaml:"temperature"`
	SystemPrompt    *string         `json:"system_prompt" yaml:"system_prompt"`
	ToolPermissions map[string]bool `json:"tools" yaml:"tools"`
	MaxTokens       *int            `json:"max_tokens" yaml:"max_tokens"`
	Hidden          *bool           `json:"hidden" yaml:"hidden"`
	FallbackModels  []string        `json:"fallback_models" yaml:"fallback_models"`
	Capabilities    []string        `json:"capabilities" yaml:"capabilities"`
	Triggers        []string        `json:"triggers" yaml:"triggers"`
	References      []string        `json:"references" yaml:"references"`
}

//nolint:govet // field order follows config-file grouping.
type fileContextConfig struct {
	MaxFileBytes    *int                      `json:"max_file_bytes" yaml:"max_file_bytes"`
	MaxTotalBytes   *int                      `json:"max_total_bytes" yaml:"max_total_bytes"`
	MaxInputTokens  *int                      `json:"max_input_tokens" yaml:"max_input_tokens"`
	References      []string                  `json:"references" yaml:"references"`
	ReferencePolicy fileReferencePolicyConfig `json:"reference_policy" yaml:"reference_policy"`
}

//nolint:govet // field order follows config-file grouping.
type fileReferencePolicyConfig struct {
	AllowedSchemes       []string `json:"allowed_schemes" yaml:"allowed_schemes"`
	AllowedHosts         []string `json:"allowed_hosts" yaml:"allowed_hosts"`
	LocalRoots           []string `json:"local_roots" yaml:"local_roots"`
	ContentTypes         []string `json:"content_types" yaml:"content_types"`
	MaxRedirects         *int     `json:"max_redirects" yaml:"max_redirects"`
	AllowPrivateNetworks *bool    `json:"allow_private_networks" yaml:"allow_private_networks"`
}

type fileGenerationConfig struct {
	Temperature    *float64 `json:"temperature" yaml:"temperature"`
	TopP           *float64 `json:"top_p" yaml:"top_p"`
	Seed           *int     `json:"seed" yaml:"seed"`
	ReasoningLevel *string  `json:"reasoning_level" yaml:"reasoning_level"`
	MaxTokens      *int     `json:"max_tokens" yaml:"max_tokens"`
}

type fileAgentLoopConfig struct {
	MaxOutputBytes *int64 `json:"max_output_bytes" yaml:"max_output_bytes"`
	MaxTotalTokens *int   `json:"max_total_tokens" yaml:"max_total_tokens"`
	MaxIterations  *int   `json:"max_iterations" yaml:"max_iterations"`
}

type filePluginConfig struct {
	Policy *attelerplugin.Policy `json:"policy" yaml:"policy"`
	Paths  []string              `json:"paths" yaml:"paths"`
}

// Load reads the default configuration files and returns the merged result plus
// the paths that were successfully loaded. Missing files are ignored.
func Load() (Config, []string, error) {
	cfg, loaded, _, err := LoadWithOrigins()

	return cfg, loaded, err
}

// LoadWithOrigins reads the default configuration stack and returns the merged
// config, successfully loaded paths, and per-field origin chains. Missing files
// are ignored.
func LoadWithOrigins() (Config, []string, OriginMap, error) {
	cfg, loaded, origins := LoadHarnessDefaultsWithOrigins()

	fileCfg, fileLoaded, fileOrigins, err := LoadPathSources(DefaultPathSources())
	mergeConfigFromOrigins(&cfg, fileCfg, origins, fileOrigins)

	loaded = append(loaded, fileLoaded...)

	normalizeEmptyMaps(&cfg)

	return cfg, loaded, origins, err
}

// DefaultPaths returns the configuration files in merge order. Later files
// override earlier files.
func DefaultPaths() []string {
	sources := DefaultPathSources()

	paths := make([]string, 0, len(sources))
	for _, source := range sources {
		paths = append(paths, source.Path)
	}

	return paths
}

// DefaultPathSources returns the default configuration files in merge order
// with the layer each path belongs to. Later sources override earlier sources.
func DefaultPathSources() []PathSource {
	paths := make([]string, 0, 8)
	paths = append(paths, globalPaths()...)

	sources := make([]PathSource, 0, len(paths)+8)
	for _, path := range paths {
		sources = append(sources, PathSource{Path: path, Kind: OriginGlobalFile})
	}

	if cwd, err := os.Getwd(); err == nil {
		for _, path := range []string{
			filepath.Join(cwd, ".atteler", "config.yaml"),
			filepath.Join(cwd, ".atteler", "config.yml"),
			filepath.Join(cwd, ".atteler", "config.json"),
			filepath.Join(cwd, ".atteler.yaml"),
			filepath.Join(cwd, ".atteler.yml"),
			filepath.Join(cwd, ".atteler.json"),
		} {
			sources = append(sources, PathSource{Path: path, Kind: OriginProjectFile})
		}
	}

	if envValue := os.Getenv(EnvPath); envValue != "" {
		for _, path := range filepath.SplitList(envValue) {
			if strings.TrimSpace(path) != "" {
				sources = append(sources, PathSource{Path: path, Kind: OriginEnvFile})
			}
		}
	}

	return sources
}

// LoadFiles reads and merges explicit YAML or JSON configuration files. Later
// files override earlier files. Missing files are ignored so callers can pass
// the full set of conventional paths without probing them first.
func LoadFiles(paths []string) (Config, []string, error) {
	cfg, loaded, _, err := LoadFilesWithOrigins(paths)

	return cfg, loaded, err
}

// LoadFilesWithOrigins reads and merges explicit YAML or JSON configuration
// files and records per-field origin chains using OriginExplicitFile.
func LoadFilesWithOrigins(paths []string) (Config, []string, OriginMap, error) {
	sources := make([]PathSource, 0, len(paths))
	for _, path := range paths {
		sources = append(sources, PathSource{Path: path, Kind: OriginExplicitFile})
	}

	return LoadPathSources(sources)
}

// LoadPathSources reads and merges YAML or JSON configuration files with
// caller-provided source kinds. It is useful for tests and diagnostics that
// need to distinguish global, project, environment, or explicit config files.
func LoadPathSources(sources []PathSource) (Config, []string, OriginMap, error) {
	cfg := Config{}
	loaded := make([]string, 0, len(sources))
	origins := OriginMap{}

	for _, source := range sources {
		path := strings.TrimSpace(source.Path)
		if path == "" {
			continue
		}

		data, err := os.ReadFile(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}

			return cfg, loaded, origins, fmt.Errorf("config: read %s: %w", path, err)
		}

		var next fileConfig

		dec := yaml.NewDecoder(bytes.NewReader(data))
		dec.KnownFields(true)

		if err := dec.Decode(&next); err != nil {
			// An empty or whitespace-only file produces io.EOF; treat it
			// as a no-op rather than a parse error.
			if errors.Is(err, io.EOF) {
				continue
			}

			return cfg, loaded, origins, fmt.Errorf("config: parse %s: %w", path, err)
		}

		if source.Kind == "" {
			source.Kind = OriginExplicitFile
		}

		mergeFileConfigWithOrigins(&cfg, next, newOriginRecorder(origins), originSource{
			kind:   source.Kind,
			source: path,
		})

		loaded = append(loaded, path)
	}

	normalizeEmptyMaps(&cfg)

	return cfg, loaded, origins, nil
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

func normalizeEmptyMaps(cfg *Config) {
	if len(cfg.Providers) == 0 {
		cfg.Providers = nil
	}

	if len(cfg.Agents) == 0 {
		cfg.Agents = nil
	}

	if len(cfg.Hooks) == 0 {
		cfg.Hooks = nil
	}
}

func mergeFileConfigWithOrigins(dst *Config, src fileConfig, rec *originRecorder, source originSource) {
	if src.DefaultProvider != nil {
		dst.DefaultProvider = *src.DefaultProvider
		rec.set("default_provider", source, *src.DefaultProvider)
	}

	if src.DefaultModel != nil {
		dst.DefaultModel = *src.DefaultModel
		rec.set("default_model", source, *src.DefaultModel)
	}

	if src.FallbackModels != nil {
		dst.FallbackModels = append([]string(nil), src.FallbackModels...)
		rec.replace("fallback_models", source, dst.FallbackModels, "replaces the entire fallback model list")
	}

	mergeProviders(dst, src.Providers, rec, source)
	mergeAgents(dst, src.Agents, rec, source)
	mergeHooks(dst, src.Hooks, rec, source)
	mergeGeneration(dst, src.Generation, rec, source)
	mergeAgentLoop(dst, src.AgentLoop, rec, source)
	mergeContext(dst, src.Context, rec, source)
	mergePlugins(dst, src.Plugins, rec, source)
}

func mergeProviders(dst *Config, providers map[string]fileProviderConfig, rec *originRecorder, source originSource) {
	if providers != nil {
		rec.merge("providers", source, sortedMapKeys(providers), "merges provider definitions by name")
	}

	for name, provider := range providers {
		if dst.Providers == nil {
			dst.Providers = make(map[string]ProviderConfig)
		}

		entityPath := providerFieldPath(name)
		rec.merge(entityPath, source, name, "merges provider fields by name")

		current := dst.Providers[name]
		if provider.Disabled != nil {
			current.Disabled = *provider.Disabled
			rec.set(providerFieldPath(name, "disabled"), source, *provider.Disabled)
		}

		if provider.BaseURL != nil {
			current.BaseURL = *provider.BaseURL
			rec.set(providerFieldPath(name, "base_url"), source, *provider.BaseURL)
		}

		if provider.TimeoutSeconds != nil {
			current.TimeoutSeconds = *provider.TimeoutSeconds
			rec.set(providerFieldPath(name, "timeout_seconds"), source, *provider.TimeoutSeconds)
		}

		dst.Providers[name] = current
	}
}

func mergeAgents(dst *Config, agents map[string]fileAgentConfig, rec *originRecorder, source originSource) {
	if agents != nil {
		rec.merge("agents", source, sortedMapKeys(agents), "merges agent definitions by name")
	}

	for name := range agents {
		if dst.Agents == nil {
			dst.Agents = make(map[string]AgentConfig)
		}

		rec.merge(agentFieldPath(name), source, name, "merges agent fields by name")

		current := dst.Agents[name]
		mergeFileAgent(&current, agents[name], rec, source, name)
		dst.Agents[name] = current
	}
}

//nolint:cyclop // Sequential nil-guarded field copies; splitting would reduce clarity.
func mergeFileAgent(current *AgentConfig, agent fileAgentConfig, rec *originRecorder, source originSource, name string) {
	if agent.Model != nil {
		current.Model = *agent.Model
		rec.set(agentFieldPath(name, "model"), source, *agent.Model)
	}

	if agent.Mode != nil {
		current.Mode = strings.TrimSpace(*agent.Mode)
		rec.set(agentFieldPath(name, "mode"), source, current.Mode)
	}

	if agent.ToolPermissions != nil {
		current.ToolPermissions = make(map[string]bool, len(agent.ToolPermissions))
		maps.Copy(current.ToolPermissions, agent.ToolPermissions)
		rec.replace(agentFieldPath(name, "tools"), source, current.ToolPermissions, "replaces the entire tool permissions map")
	}

	if agent.SystemPrompt != nil {
		current.SystemPrompt = *agent.SystemPrompt
		rec.set(agentFieldPath(name, "system_prompt"), source, *agent.SystemPrompt)
	}

	if agent.ReasoningLevel != nil {
		current.ReasoningLevel = strings.TrimSpace(*agent.ReasoningLevel)
		rec.set(agentFieldPath(name, "reasoning_level"), source, current.ReasoningLevel)
	}

	if agent.Description != nil {
		current.Description = *agent.Description
		rec.set(agentFieldPath(name, "description"), source, *agent.Description)
	}

	if agent.Personality != nil {
		current.Personality = *agent.Personality
		rec.set(agentFieldPath(name, "personality"), source, *agent.Personality)
	}

	if agent.FallbackModels != nil {
		current.FallbackModels = append([]string(nil), agent.FallbackModels...)
		rec.replace(agentFieldPath(name, "fallback_models"), source, current.FallbackModels, "replaces the entire agent fallback model list")
	}

	if agent.Capabilities != nil {
		current.Capabilities = append([]string(nil), agent.Capabilities...)
		rec.replace(agentFieldPath(name, "capabilities"), source, current.Capabilities, "replaces the entire capabilities list")
	}

	if agent.Temperature != nil {
		current.Temperature = agent.Temperature
		rec.set(agentFieldPath(name, "temperature"), source, *agent.Temperature)
	}

	if agent.TopP != nil {
		current.TopP = agent.TopP
		rec.set(agentFieldPath(name, "top_p"), source, *agent.TopP)
	}

	if agent.Seed != nil {
		current.Seed = agent.Seed
		rec.set(agentFieldPath(name, "seed"), source, *agent.Seed)
	}

	if agent.Triggers != nil {
		current.Triggers = append([]string(nil), agent.Triggers...)
		rec.replace(agentFieldPath(name, "triggers"), source, current.Triggers, "replaces the entire trigger list")
	}

	if agent.References != nil {
		current.References = append([]string(nil), agent.References...)
		rec.replace(agentFieldPath(name, "references"), source, current.References, "replaces the entire reference list")
	}

	if agent.MaxTokens != nil {
		current.MaxTokens = *agent.MaxTokens
		rec.set(agentFieldPath(name, "max_tokens"), source, *agent.MaxTokens)
	}

	if agent.Hidden != nil {
		current.Hidden = *agent.Hidden
		current.hiddenSet = true

		rec.set(agentFieldPath(name, "hidden"), source, *agent.Hidden)
	}
}

func mergeHooks(dst *Config, hooks map[string][]HookConfig, rec *originRecorder, source originSource) {
	if hooks != nil {
		rec.merge("hooks", source, sortedMapKeys(hooks), "merges hook lists by event type")
	}

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
		rec.replace(hookFieldPath(eventType), source, merged, "replaces the entire hook list for this event")
	}
}

func mergeContext(dst *Config, contextConfig fileContextConfig, rec *originRecorder, source originSource) {
	if contextConfig.MaxFileBytes != nil {
		dst.Context.MaxFileBytes = *contextConfig.MaxFileBytes
		rec.set("context.max_file_bytes", source, *contextConfig.MaxFileBytes)
	}

	if contextConfig.MaxTotalBytes != nil {
		dst.Context.MaxTotalBytes = *contextConfig.MaxTotalBytes
		rec.set("context.max_total_bytes", source, *contextConfig.MaxTotalBytes)
	}

	if contextConfig.MaxInputTokens != nil {
		dst.Context.MaxInputTokens = *contextConfig.MaxInputTokens
		rec.set("context.max_input_tokens", source, *contextConfig.MaxInputTokens)
	}

	if contextConfig.References != nil {
		dst.Context.References = append([]string(nil), contextConfig.References...)
		rec.replace("context.references", source, dst.Context.References, "replaces the entire configured reference list")
	}

	mergeReferencePolicy(&dst.Context.ReferencePolicy, contextConfig.ReferencePolicy)
}

func mergeReferencePolicy(dst *ReferencePolicyConfig, policy fileReferencePolicyConfig) {
	if policy.AllowedSchemes != nil {
		dst.AllowedSchemes = append([]string(nil), policy.AllowedSchemes...)
	}

	if policy.AllowedHosts != nil {
		dst.AllowedHosts = append([]string(nil), policy.AllowedHosts...)
	}

	if policy.LocalRoots != nil {
		dst.LocalRoots = append([]string(nil), policy.LocalRoots...)
	}

	if policy.MaxRedirects != nil {
		dst.MaxRedirects = *policy.MaxRedirects
	}

	if policy.ContentTypes != nil {
		dst.ContentTypes = append([]string(nil), policy.ContentTypes...)
	}

	if policy.AllowPrivateNetworks != nil {
		dst.AllowPrivateNetworks = *policy.AllowPrivateNetworks
	}
}

func mergeGeneration(dst *Config, generation fileGenerationConfig, rec *originRecorder, source originSource) {
	if generation.Temperature != nil {
		dst.Generation.Temperature = generation.Temperature
		rec.set("generation.temperature", source, *generation.Temperature)
	}

	if generation.TopP != nil {
		dst.Generation.TopP = generation.TopP
		rec.set("generation.top_p", source, *generation.TopP)
	}

	if generation.Seed != nil {
		dst.Generation.Seed = generation.Seed
		rec.set("generation.seed", source, *generation.Seed)
	}

	if generation.ReasoningLevel != nil {
		dst.Generation.ReasoningLevel = strings.TrimSpace(*generation.ReasoningLevel)
		rec.set("generation.reasoning_level", source, dst.Generation.ReasoningLevel)
	}

	if generation.MaxTokens != nil {
		dst.Generation.MaxTokens = *generation.MaxTokens
		rec.set("generation.max_tokens", source, *generation.MaxTokens)
	}
}

func mergeAgentLoop(dst *Config, agentLoop fileAgentLoopConfig, rec *originRecorder, source originSource) {
	if agentLoop.MaxOutputBytes != nil {
		value := *agentLoop.MaxOutputBytes
		dst.AgentLoop.MaxOutputBytes = &value
		rec.set("agent_loop.max_output_bytes", source, value)
	}

	if agentLoop.MaxTotalTokens != nil {
		value := *agentLoop.MaxTotalTokens
		dst.AgentLoop.MaxTotalTokens = &value
		rec.set("agent_loop.max_total_tokens", source, value)
	}

	if agentLoop.MaxIterations != nil {
		value := *agentLoop.MaxIterations
		dst.AgentLoop.MaxIterations = &value
		rec.set("agent_loop.max_iterations", source, value)
	}
}

func mergePlugins(dst *Config, plugins filePluginConfig, rec *originRecorder, source originSource) {
	if plugins.Paths != nil {
		dst.Plugins.Paths = append([]string(nil), plugins.Paths...)
		rec.replace("plugins.paths", source, dst.Plugins.Paths, "replaces the entire plugin path list")
	}

	if plugins.Policy != nil {
		policy := attelerplugin.ClonePolicy(*plugins.Policy)
		dst.Plugins.Policy = &policy
	}
}

func mergeConfig(dst *Config, src Config) {
	mergeConfigFromSource(dst, src, nil, originSource{})
}

func mergeConfigFromSource(dst *Config, src Config, rec *originRecorder, source originSource) {
	if src.DefaultProvider != "" {
		dst.DefaultProvider = src.DefaultProvider
		rec.set("default_provider", source, src.DefaultProvider)
	}

	if src.DefaultModel != "" {
		dst.DefaultModel = src.DefaultModel
		rec.set("default_model", source, src.DefaultModel)
	}

	if src.FallbackModels != nil {
		dst.FallbackModels = append([]string(nil), src.FallbackModels...)
		rec.replace("fallback_models", source, dst.FallbackModels, "replaces the entire fallback model list")
	}

	mergeConfigProviders(dst, src.Providers, rec, source)
	mergeConfigAgents(dst, src.Agents, rec, source)
	mergeConfigHooks(dst, src.Hooks, rec, source)
	mergeConfigGeneration(dst, src.Generation, rec, source)
	mergeConfigAgentLoop(dst, src.AgentLoop, rec, source)
	mergeConfigContext(dst, src.Context, rec, source)
	mergeConfigPlugins(dst, src.Plugins, rec, source)
}

func mergeConfigProviders(dst *Config, providers map[string]ProviderConfig, rec *originRecorder, source originSource) {
	if providers != nil {
		rec.merge("providers", source, sortedMapKeys(providers), "merges provider definitions by name")
	}

	for name, provider := range providers {
		if dst.Providers == nil {
			dst.Providers = make(map[string]ProviderConfig)
		}

		rec.merge(providerFieldPath(name), source, name, "merges provider fields by name")

		current := dst.Providers[name]
		if provider.BaseURL != "" {
			current.BaseURL = provider.BaseURL
			rec.set(providerFieldPath(name, "base_url"), source, provider.BaseURL)
		}

		current.Disabled = provider.Disabled
		rec.set(providerFieldPath(name, "disabled"), source, provider.Disabled)

		if provider.TimeoutSeconds > 0 {
			current.TimeoutSeconds = provider.TimeoutSeconds
			rec.set(providerFieldPath(name, "timeout_seconds"), source, provider.TimeoutSeconds)
		}

		dst.Providers[name] = current
	}
}

func mergeConfigAgents(dst *Config, agents map[string]AgentConfig, rec *originRecorder, source originSource) {
	if agents != nil {
		rec.merge("agents", source, sortedMapKeys(agents), "merges agent definitions by name")
	}

	for name := range agents {
		if dst.Agents == nil {
			dst.Agents = make(map[string]AgentConfig)
		}

		rec.merge(agentFieldPath(name), source, name, "merges agent fields by name")

		current := dst.Agents[name]
		mergeConfigAgent(&current, agents[name], rec, source, name)
		dst.Agents[name] = current
	}
}

func mergeConfigAgent(current *AgentConfig, agent AgentConfig, rec *originRecorder, source originSource, name string) {
	if agent.Model != "" {
		current.Model = agent.Model
		rec.set(agentFieldPath(name, "model"), source, agent.Model)
	}

	if agent.Mode != "" {
		current.Mode = agent.Mode
		rec.set(agentFieldPath(name, "mode"), source, agent.Mode)
	}

	if agent.ToolPermissions != nil {
		current.ToolPermissions = make(map[string]bool, len(agent.ToolPermissions))
		maps.Copy(current.ToolPermissions, agent.ToolPermissions)
		rec.replace(agentFieldPath(name, "tools"), source, current.ToolPermissions, "replaces the entire tool permissions map")
	}

	if agent.SystemPrompt != "" {
		current.SystemPrompt = agent.SystemPrompt
		rec.set(agentFieldPath(name, "system_prompt"), source, agent.SystemPrompt)
	}

	if agent.ReasoningLevel != "" {
		current.ReasoningLevel = strings.TrimSpace(agent.ReasoningLevel)
		rec.set(agentFieldPath(name, "reasoning_level"), source, current.ReasoningLevel)
	}

	if agent.Description != "" {
		current.Description = agent.Description
		rec.set(agentFieldPath(name, "description"), source, agent.Description)
	}

	if agent.Personality != "" {
		current.Personality = agent.Personality
		rec.set(agentFieldPath(name, "personality"), source, agent.Personality)
	}

	mergeConfigAgentSlicesAndPointers(current, agent, rec, source, name)

	if agent.MaxTokens > 0 {
		current.MaxTokens = agent.MaxTokens
		rec.set(agentFieldPath(name, "max_tokens"), source, agent.MaxTokens)
	}

	if agent.hiddenSet || agent.Hidden {
		current.Hidden = agent.Hidden
		current.hiddenSet = true

		rec.set(agentFieldPath(name, "hidden"), source, agent.Hidden)
	}
}

func mergeConfigAgentSlicesAndPointers(current *AgentConfig, agent AgentConfig, rec *originRecorder, source originSource, name string) {
	if agent.FallbackModels != nil {
		current.FallbackModels = append([]string(nil), agent.FallbackModels...)
		rec.replace(agentFieldPath(name, "fallback_models"), source, current.FallbackModels, "replaces the entire agent fallback model list")
	}

	if agent.Capabilities != nil {
		current.Capabilities = append([]string(nil), agent.Capabilities...)
		rec.replace(agentFieldPath(name, "capabilities"), source, current.Capabilities, "replaces the entire capabilities list")
	}

	if agent.Temperature != nil {
		current.Temperature = agent.Temperature
		rec.set(agentFieldPath(name, "temperature"), source, *agent.Temperature)
	}

	if agent.TopP != nil {
		current.TopP = agent.TopP
		rec.set(agentFieldPath(name, "top_p"), source, *agent.TopP)
	}

	if agent.Seed != nil {
		current.Seed = agent.Seed
		rec.set(agentFieldPath(name, "seed"), source, *agent.Seed)
	}

	if agent.Triggers != nil {
		current.Triggers = append([]string(nil), agent.Triggers...)
		rec.replace(agentFieldPath(name, "triggers"), source, current.Triggers, "replaces the entire trigger list")
	}

	if agent.References != nil {
		current.References = append([]string(nil), agent.References...)
		rec.replace(agentFieldPath(name, "references"), source, current.References, "replaces the entire reference list")
	}
}

func mergeConfigHooks(dst *Config, hooks map[string][]HookConfig, rec *originRecorder, source originSource) {
	if hooks != nil {
		rec.merge("hooks", source, sortedMapKeys(hooks), "merges hook lists by event type")
	}

	for eventType, eventHooks := range hooks {
		if dst.Hooks == nil {
			dst.Hooks = make(map[string][]HookConfig)
		}

		dst.Hooks[eventType] = cloneHooks(eventHooks)
		rec.replace(hookFieldPath(eventType), source, dst.Hooks[eventType], "replaces the entire hook list for this event")
	}
}

func mergeConfigContext(dst *Config, contextConfig ContextConfig, rec *originRecorder, source originSource) {
	if contextConfig.MaxFileBytes > 0 {
		dst.Context.MaxFileBytes = contextConfig.MaxFileBytes
		rec.set("context.max_file_bytes", source, contextConfig.MaxFileBytes)
	}

	if contextConfig.MaxTotalBytes > 0 {
		dst.Context.MaxTotalBytes = contextConfig.MaxTotalBytes
		rec.set("context.max_total_bytes", source, contextConfig.MaxTotalBytes)
	}

	if contextConfig.MaxInputTokens > 0 {
		dst.Context.MaxInputTokens = contextConfig.MaxInputTokens
		rec.set("context.max_input_tokens", source, contextConfig.MaxInputTokens)
	}

	if contextConfig.References != nil {
		dst.Context.References = append([]string(nil), contextConfig.References...)
		rec.replace("context.references", source, dst.Context.References, "replaces the entire configured reference list")
	}

	mergeConfigReferencePolicy(&dst.Context.ReferencePolicy, contextConfig.ReferencePolicy)
}

func mergeConfigReferencePolicy(dst *ReferencePolicyConfig, policy ReferencePolicyConfig) {
	if policy.AllowedSchemes != nil {
		dst.AllowedSchemes = append([]string(nil), policy.AllowedSchemes...)
	}

	if policy.AllowedHosts != nil {
		dst.AllowedHosts = append([]string(nil), policy.AllowedHosts...)
	}

	if policy.LocalRoots != nil {
		dst.LocalRoots = append([]string(nil), policy.LocalRoots...)
	}

	if policy.MaxRedirects > 0 {
		dst.MaxRedirects = policy.MaxRedirects
	}

	if policy.ContentTypes != nil {
		dst.ContentTypes = append([]string(nil), policy.ContentTypes...)
	}

	if policy.AllowPrivateNetworks {
		dst.AllowPrivateNetworks = true
	}
}

func mergeConfigGeneration(dst *Config, generation GenerationConfig, rec *originRecorder, source originSource) {
	if generation.Temperature != nil {
		dst.Generation.Temperature = generation.Temperature
		rec.set("generation.temperature", source, *generation.Temperature)
	}

	if generation.TopP != nil {
		dst.Generation.TopP = generation.TopP
		rec.set("generation.top_p", source, *generation.TopP)
	}

	if generation.Seed != nil {
		dst.Generation.Seed = generation.Seed
		rec.set("generation.seed", source, *generation.Seed)
	}

	if generation.ReasoningLevel != "" {
		dst.Generation.ReasoningLevel = strings.TrimSpace(generation.ReasoningLevel)
		rec.set("generation.reasoning_level", source, dst.Generation.ReasoningLevel)
	}

	if generation.MaxTokens > 0 {
		dst.Generation.MaxTokens = generation.MaxTokens
		rec.set("generation.max_tokens", source, generation.MaxTokens)
	}
}

func mergeConfigAgentLoop(dst *Config, agentLoop AgentLoopConfig, rec *originRecorder, source originSource) {
	if agentLoop.MaxOutputBytes != nil {
		value := *agentLoop.MaxOutputBytes
		dst.AgentLoop.MaxOutputBytes = &value
		rec.set("agent_loop.max_output_bytes", source, value)
	}

	if agentLoop.MaxTotalTokens != nil {
		value := *agentLoop.MaxTotalTokens
		dst.AgentLoop.MaxTotalTokens = &value
		rec.set("agent_loop.max_total_tokens", source, value)
	}

	if agentLoop.MaxIterations != nil {
		value := *agentLoop.MaxIterations
		dst.AgentLoop.MaxIterations = &value
		rec.set("agent_loop.max_iterations", source, value)
	}
}

func mergeConfigPlugins(dst *Config, plugins PluginConfig, rec *originRecorder, source originSource) {
	if plugins.Paths != nil {
		dst.Plugins.Paths = append([]string(nil), plugins.Paths...)
		rec.replace("plugins.paths", source, dst.Plugins.Paths, "replaces the entire plugin path list")
	}
}

func mergeConfigFromOrigins(dst *Config, src Config, dstOrigins, srcOrigins OriginMap) {
	if src.DefaultProvider != "" {
		dst.DefaultProvider = src.DefaultProvider

		appendOriginChain(dstOrigins, "default_provider", srcOrigins, false)
	}

	if src.DefaultModel != "" {
		dst.DefaultModel = src.DefaultModel

		appendOriginChain(dstOrigins, "default_model", srcOrigins, false)
	}

	if src.FallbackModels != nil {
		dst.FallbackModels = append([]string(nil), src.FallbackModels...)

		appendOriginChain(dstOrigins, "fallback_models", srcOrigins, true)
	}

	mergeConfigProvidersFromOrigins(dst, src.Providers, dstOrigins, srcOrigins)
	mergeConfigAgentsFromOrigins(dst, src.Agents, dstOrigins, srcOrigins)
	mergeConfigHooksFromOrigins(dst, src.Hooks, dstOrigins, srcOrigins)
	mergeConfigGenerationFromOrigins(dst, src.Generation, dstOrigins, srcOrigins)
	mergeConfigAgentLoopFromOrigins(dst, src.AgentLoop, dstOrigins, srcOrigins)
	mergeConfigContextFromOrigins(dst, src.Context, dstOrigins, srcOrigins)
	mergeConfigPluginsFromOrigins(dst, src.Plugins, dstOrigins, srcOrigins)
}

func mergeConfigProvidersFromOrigins(dst *Config, providers map[string]ProviderConfig, dstOrigins, srcOrigins OriginMap) {
	if providers != nil {
		appendOriginChain(dstOrigins, "providers", srcOrigins, false)
	}

	for name, provider := range providers {
		if dst.Providers == nil {
			dst.Providers = make(map[string]ProviderConfig)
		}

		appendOriginChain(dstOrigins, providerFieldPath(name), srcOrigins, false)

		current := dst.Providers[name]
		if provider.BaseURL != "" {
			current.BaseURL = provider.BaseURL

			appendOriginChain(dstOrigins, providerFieldPath(name, "base_url"), srcOrigins, false)
		}

		current.Disabled = provider.Disabled

		appendOriginChain(dstOrigins, providerFieldPath(name, "disabled"), srcOrigins, false)

		if provider.TimeoutSeconds > 0 {
			current.TimeoutSeconds = provider.TimeoutSeconds

			appendOriginChain(dstOrigins, providerFieldPath(name, "timeout_seconds"), srcOrigins, false)
		}

		dst.Providers[name] = current
	}
}

func mergeConfigAgentsFromOrigins(dst *Config, agents map[string]AgentConfig, dstOrigins, srcOrigins OriginMap) {
	if agents != nil {
		appendOriginChain(dstOrigins, "agents", srcOrigins, false)
	}

	for name := range agents {
		if dst.Agents == nil {
			dst.Agents = make(map[string]AgentConfig)
		}

		appendOriginChain(dstOrigins, agentFieldPath(name), srcOrigins, false)

		current := dst.Agents[name]
		mergeConfigAgentFromOrigins(&current, agents[name], dstOrigins, srcOrigins, name)
		dst.Agents[name] = current
	}
}

func mergeConfigAgentFromOrigins(current *AgentConfig, agent AgentConfig, dstOrigins, srcOrigins OriginMap, name string) {
	if agent.Model != "" {
		current.Model = agent.Model

		appendOriginChain(dstOrigins, agentFieldPath(name, "model"), srcOrigins, false)
	}

	if agent.Mode != "" {
		current.Mode = agent.Mode

		appendOriginChain(dstOrigins, agentFieldPath(name, "mode"), srcOrigins, false)
	}

	if agent.ToolPermissions != nil {
		current.ToolPermissions = make(map[string]bool, len(agent.ToolPermissions))
		maps.Copy(current.ToolPermissions, agent.ToolPermissions)

		appendOriginChain(dstOrigins, agentFieldPath(name, "tools"), srcOrigins, true)
	}

	if agent.SystemPrompt != "" {
		current.SystemPrompt = agent.SystemPrompt

		appendOriginChain(dstOrigins, agentFieldPath(name, "system_prompt"), srcOrigins, false)
	}

	if agent.ReasoningLevel != "" {
		current.ReasoningLevel = strings.TrimSpace(agent.ReasoningLevel)

		appendOriginChain(dstOrigins, agentFieldPath(name, "reasoning_level"), srcOrigins, false)
	}

	if agent.Description != "" {
		current.Description = agent.Description

		appendOriginChain(dstOrigins, agentFieldPath(name, "description"), srcOrigins, false)
	}

	if agent.Personality != "" {
		current.Personality = agent.Personality

		appendOriginChain(dstOrigins, agentFieldPath(name, "personality"), srcOrigins, false)
	}

	mergeConfigAgentSlicesAndPointersFromOrigins(current, agent, dstOrigins, srcOrigins, name)

	if agent.MaxTokens > 0 {
		current.MaxTokens = agent.MaxTokens

		appendOriginChain(dstOrigins, agentFieldPath(name, "max_tokens"), srcOrigins, false)
	}

	if agent.hiddenSet || agent.Hidden {
		current.Hidden = agent.Hidden
		current.hiddenSet = true

		appendOriginChain(dstOrigins, agentFieldPath(name, "hidden"), srcOrigins, false)
	}
}

func mergeConfigAgentSlicesAndPointersFromOrigins(
	current *AgentConfig,
	agent AgentConfig,
	dstOrigins OriginMap,
	srcOrigins OriginMap,
	name string,
) {
	if agent.FallbackModels != nil {
		current.FallbackModels = append([]string(nil), agent.FallbackModels...)

		appendOriginChain(dstOrigins, agentFieldPath(name, "fallback_models"), srcOrigins, true)
	}

	if agent.Capabilities != nil {
		current.Capabilities = append([]string(nil), agent.Capabilities...)

		appendOriginChain(dstOrigins, agentFieldPath(name, "capabilities"), srcOrigins, true)
	}

	if agent.Temperature != nil {
		current.Temperature = agent.Temperature

		appendOriginChain(dstOrigins, agentFieldPath(name, "temperature"), srcOrigins, false)
	}

	if agent.TopP != nil {
		current.TopP = agent.TopP

		appendOriginChain(dstOrigins, agentFieldPath(name, "top_p"), srcOrigins, false)
	}

	if agent.Seed != nil {
		current.Seed = agent.Seed

		appendOriginChain(dstOrigins, agentFieldPath(name, "seed"), srcOrigins, false)
	}

	if agent.Triggers != nil {
		current.Triggers = append([]string(nil), agent.Triggers...)

		appendOriginChain(dstOrigins, agentFieldPath(name, "triggers"), srcOrigins, true)
	}

	if agent.References != nil {
		current.References = append([]string(nil), agent.References...)

		appendOriginChain(dstOrigins, agentFieldPath(name, "references"), srcOrigins, true)
	}
}

func mergeConfigHooksFromOrigins(dst *Config, hooks map[string][]HookConfig, dstOrigins, srcOrigins OriginMap) {
	if hooks != nil {
		appendOriginChain(dstOrigins, "hooks", srcOrigins, false)
	}

	for eventType, eventHooks := range hooks {
		if dst.Hooks == nil {
			dst.Hooks = make(map[string][]HookConfig)
		}

		dst.Hooks[eventType] = cloneHooks(eventHooks)

		appendOriginChain(dstOrigins, hookFieldPath(eventType), srcOrigins, true)
	}
}

func mergeConfigContextFromOrigins(dst *Config, contextConfig ContextConfig, dstOrigins, srcOrigins OriginMap) {
	if contextConfig.MaxFileBytes > 0 {
		dst.Context.MaxFileBytes = contextConfig.MaxFileBytes

		appendOriginChain(dstOrigins, "context.max_file_bytes", srcOrigins, false)
	}

	if contextConfig.MaxTotalBytes > 0 {
		dst.Context.MaxTotalBytes = contextConfig.MaxTotalBytes

		appendOriginChain(dstOrigins, "context.max_total_bytes", srcOrigins, false)
	}

	if contextConfig.MaxInputTokens > 0 {
		dst.Context.MaxInputTokens = contextConfig.MaxInputTokens

		appendOriginChain(dstOrigins, "context.max_input_tokens", srcOrigins, false)
	}

	if contextConfig.References != nil {
		dst.Context.References = append([]string(nil), contextConfig.References...)

		appendOriginChain(dstOrigins, "context.references", srcOrigins, true)
	}
}

func mergeConfigGenerationFromOrigins(dst *Config, generation GenerationConfig, dstOrigins, srcOrigins OriginMap) {
	if generation.Temperature != nil {
		dst.Generation.Temperature = generation.Temperature

		appendOriginChain(dstOrigins, "generation.temperature", srcOrigins, false)
	}

	if generation.TopP != nil {
		dst.Generation.TopP = generation.TopP

		appendOriginChain(dstOrigins, "generation.top_p", srcOrigins, false)
	}

	if generation.Seed != nil {
		dst.Generation.Seed = generation.Seed

		appendOriginChain(dstOrigins, "generation.seed", srcOrigins, false)
	}

	if generation.ReasoningLevel != "" {
		dst.Generation.ReasoningLevel = strings.TrimSpace(generation.ReasoningLevel)

		appendOriginChain(dstOrigins, "generation.reasoning_level", srcOrigins, false)
	}

	if generation.MaxTokens > 0 {
		dst.Generation.MaxTokens = generation.MaxTokens

		appendOriginChain(dstOrigins, "generation.max_tokens", srcOrigins, false)
	}
}

func mergeConfigAgentLoopFromOrigins(dst *Config, agentLoop AgentLoopConfig, dstOrigins, srcOrigins OriginMap) {
	if agentLoop.MaxOutputBytes != nil {
		value := *agentLoop.MaxOutputBytes
		dst.AgentLoop.MaxOutputBytes = &value

		appendOriginChain(dstOrigins, "agent_loop.max_output_bytes", srcOrigins, false)
	}

	if agentLoop.MaxTotalTokens != nil {
		value := *agentLoop.MaxTotalTokens
		dst.AgentLoop.MaxTotalTokens = &value

		appendOriginChain(dstOrigins, "agent_loop.max_total_tokens", srcOrigins, false)
	}

	if agentLoop.MaxIterations != nil {
		value := *agentLoop.MaxIterations
		dst.AgentLoop.MaxIterations = &value

		appendOriginChain(dstOrigins, "agent_loop.max_iterations", srcOrigins, false)
	}
}

func mergeConfigPluginsFromOrigins(dst *Config, plugins PluginConfig, dstOrigins, srcOrigins OriginMap) {
	if plugins.Paths != nil {
		dst.Plugins.Paths = append([]string(nil), plugins.Paths...)

		appendOriginChain(dstOrigins, "plugins.paths", srcOrigins, true)
	}

	if plugins.Policy != nil {
		policy := attelerplugin.ClonePolicy(*plugins.Policy)
		dst.Plugins.Policy = &policy
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
