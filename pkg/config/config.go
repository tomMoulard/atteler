// Package config loads atteler configuration from layered YAML files.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	attelerplugin "github.com/tommoulard/atteler/pkg/plugin"
	"github.com/tommoulard/atteler/pkg/sourcepolicy"
)

// EnvPath names one or more configuration files that should override the
// default global/local configuration files. Multiple paths can be separated
// with the platform path-list separator.
const EnvPath = "ATTELER_CONFIG"

// ConfigSchemaVersion is the current on-disk schema version for Atteler config
// files. Missing or zero versions are treated as legacy v0 and migrated in
// memory before merging.
const ConfigSchemaVersion = 1

// Config is the merged application configuration.
//
//nolint:govet // fieldalignment: field order follows config-file grouping.
type Config struct {
	Version         int                        `json:"version,omitempty" yaml:"version,omitempty"`
	Providers       map[string]ProviderConfig  `json:"providers,omitempty" yaml:"providers,omitempty"`
	Agents          map[string]AgentConfig     `json:"agents,omitempty" yaml:"agents,omitempty"`
	Hooks           map[string][]HookConfig    `json:"hooks,omitempty" yaml:"hooks,omitempty"`
	Generation      GenerationConfig           `json:"generation" yaml:"generation"`
	Research        ResearchConfig             `json:"research" yaml:"research"`
	AgentLoop       AgentLoopConfig            `json:"agent_loop" yaml:"agent_loop"`
	Autonomy        string                     `json:"autonomy,omitempty" yaml:"autonomy,omitempty"`
	Auto            string                     `json:"auto,omitempty" yaml:"auto,omitempty"`
	DefaultProvider string                     `json:"default_provider,omitempty" yaml:"default_provider,omitempty"`
	DefaultModel    string                     `json:"default_model,omitempty" yaml:"default_model,omitempty"`
	EventLedgerPath string                     `json:"event_ledger_path,omitempty" yaml:"event_ledger_path,omitempty"`
	ModelAliases    map[string]string          `json:"model_aliases,omitempty" yaml:"model_aliases,omitempty"`
	ModelRoles      map[string]ModelRoleConfig `json:"models,omitempty" yaml:"models,omitempty"`
	FallbackModels  []string                   `json:"fallback_models,omitempty" yaml:"fallback_models,omitempty"`
	Context         ContextConfig              `json:"context" yaml:"context"`
	Plugins         PluginConfig               `json:"plugins" yaml:"plugins"`
	SkillLearning   SkillLearningConfig        `json:"skill_learning" yaml:"skill_learning"`
	Vector          VectorConfig               `json:"vector" yaml:"vector"`
	Worktree        WorktreeConfig             `json:"worktree" yaml:"worktree"`
}

// ProviderConfig configures an individual LLM provider.
type ProviderConfig struct {
	Retry                 RetryConfig `json:"retry,omitzero" yaml:"retry,omitempty"`
	BaseURL               string      `json:"base_url,omitempty" yaml:"base_url,omitempty"`
	Type                  string      `json:"type,omitempty" yaml:"type,omitempty"`
	APIKeyEnv             string      `json:"api_key_env,omitempty" yaml:"api_key_env,omitempty"`
	APIKeyHeader          string      `json:"api_key_header,omitempty" yaml:"api_key_header,omitempty"`
	APIKeyScheme          string      `json:"api_key_scheme,omitempty" yaml:"api_key_scheme,omitempty"`
	ChatCompletionsPath   string      `json:"chat_completions_path,omitempty" yaml:"chat_completions_path,omitempty"`
	EmbeddingsPath        string      `json:"embeddings_path,omitempty" yaml:"embeddings_path,omitempty"`
	ModelsPath            string      `json:"models_path,omitempty" yaml:"models_path,omitempty"`
	APIVersion            string      `json:"api_version,omitempty" yaml:"api_version,omitempty"`
	Models                []string    `json:"models,omitempty" yaml:"models,omitempty"`
	Capabilities          []string    `json:"capabilities,omitempty" yaml:"capabilities,omitempty"`
	Disabled              bool        `json:"disabled,omitempty" yaml:"disabled,omitempty"`
	Local                 bool        `json:"local,omitempty" yaml:"local,omitempty"`
	AutoStart             bool        `json:"auto_start,omitempty" yaml:"auto_start,omitempty"`
	DisablePrivateAdapter bool        `json:"disable_private_adapter,omitempty" yaml:"disable_private_adapter,omitempty"`
	TimeoutSeconds        int         `json:"timeout_seconds,omitempty" yaml:"timeout_seconds,omitempty"`
}

// RetryConfig configures provider retry behavior. Nil fields inherit the
// registry defaults; max_attempts is the number of additional retry attempts.
type RetryConfig struct {
	MaxAttempts      *int     `json:"max_attempts,omitempty" yaml:"max_attempts,omitempty"`
	InitialBackoffMS *int     `json:"initial_backoff_ms,omitempty" yaml:"initial_backoff_ms,omitempty"`
	MaxBackoffMS     *int     `json:"max_backoff_ms,omitempty" yaml:"max_backoff_ms,omitempty"`
	MaxElapsedMS     *int     `json:"max_elapsed_ms,omitempty" yaml:"max_elapsed_ms,omitempty"`
	JitterFraction   *float64 `json:"jitter_fraction,omitempty" yaml:"jitter_fraction,omitempty"`
}

// ModelRoleConfig configures a task-oriented model role such as "planner" or
// "fast_coder".
//
//nolint:govet // Field order follows the public YAML shape.
type ModelRoleConfig struct {
	Preferred            string              `json:"preferred,omitempty" yaml:"preferred,omitempty"`
	FallbackModels       []string            `json:"fallback_models,omitempty" yaml:"fallback_models,omitempty"`
	RoutingPolicy        RoutingPolicyConfig `json:"routing_policy,omitzero" yaml:"routing_policy,omitempty"`
	PreferredProviders   []string            `json:"preferred_providers,omitempty" yaml:"preferred_providers,omitempty"`
	BannedProviders      []string            `json:"banned_providers,omitempty" yaml:"banned_providers,omitempty"`
	BannedModels         []string            `json:"banned_models,omitempty" yaml:"banned_models,omitempty"`
	RequiredCapabilities []string            `json:"required_capabilities,omitempty" yaml:"required_capabilities,omitempty"`
	MaxCostUSD           float64             `json:"max_cost_usd,omitempty" yaml:"max_cost_usd,omitempty"`
	MaxLatencyMS         int                 `json:"max_latency_ms,omitempty" yaml:"max_latency_ms,omitempty"`
	MaxTTFTMS            int                 `json:"max_ttft_ms,omitempty" yaml:"max_ttft_ms,omitempty"`
	RequireFreshMetadata bool                `json:"require_fresh_metadata,omitempty" yaml:"require_fresh_metadata,omitempty"`
	PreferLocal          bool                `json:"prefer_local,omitempty" yaml:"prefer_local,omitempty"`
}

// AgentConfig configures a named agent persona.
//
//nolint:govet // Field order follows config-file grouping.
type AgentConfig struct {
	Temperature *float64 `json:"temperature,omitempty" yaml:"temperature,omitempty"`
	TopP        *float64 `json:"top_p,omitempty" yaml:"top_p,omitempty"`
	Seed        *int     `json:"seed,omitempty" yaml:"seed,omitempty"`

	ToolPermissions map[string]bool `json:"tools,omitempty" yaml:"tools,omitempty"`

	RoutingPolicy    RoutingPolicyConfig `json:"routing_policy,omitzero" yaml:"routing_policy,omitempty"`
	Model            string              `json:"model,omitempty" yaml:"model,omitempty"`
	Mode             string              `json:"mode,omitempty" yaml:"mode,omitempty"`
	ModelMode        string              `json:"model_mode,omitempty" yaml:"model_mode,omitempty"`
	ReasoningLevel   string              `json:"reasoning_level,omitempty" yaml:"reasoning_level,omitempty"`
	Description      string              `json:"description,omitempty" yaml:"description,omitempty"`
	Personality      string              `json:"personality,omitempty" yaml:"personality,omitempty"`
	SystemPrompt     string              `json:"system_prompt,omitempty" yaml:"system_prompt,omitempty"`
	FallbackModels   []string            `json:"fallback_models,omitempty" yaml:"fallback_models,omitempty"`
	Capabilities     []string            `json:"capabilities,omitempty" yaml:"capabilities,omitempty"`
	Triggers         []string            `json:"triggers,omitempty" yaml:"triggers,omitempty"`
	References       []string            `json:"references,omitempty" yaml:"references,omitempty"`
	FeedbackGuidance []FeedbackGuidance  `json:"feedback_guidance,omitempty" yaml:"feedback_guidance,omitempty"`
	MaxTokens        int                 `json:"max_tokens,omitempty" yaml:"max_tokens,omitempty"`
	Hidden           bool                `json:"hidden,omitempty" yaml:"hidden,omitempty"`

	hiddenSet bool
}

// RoutingPolicyConfig configures per-agent model routing preferences.
type RoutingPolicyConfig struct {
	PreferredProviders   []string `json:"preferred_providers,omitempty" yaml:"preferred_providers,omitempty"`
	BannedProviders      []string `json:"banned_providers,omitempty" yaml:"banned_providers,omitempty"`
	BannedModels         []string `json:"banned_models,omitempty" yaml:"banned_models,omitempty"`
	RequiredCapabilities []string `json:"required_capabilities,omitempty" yaml:"required_capabilities,omitempty"`
	MaxBudget            float64  `json:"max_budget,omitempty" yaml:"max_budget,omitempty"`
	MaxLatencyMS         int      `json:"max_latency_ms,omitempty" yaml:"max_latency_ms,omitempty"`
	MaxTTFTMS            int      `json:"max_ttft_ms,omitempty" yaml:"max_ttft_ms,omitempty"`
	RequireFreshMetadata bool     `json:"require_fresh_metadata,omitempty" yaml:"require_fresh_metadata,omitempty"`
}

// FeedbackGuidance stores an auditable feedback-derived prompt instruction.
//
//nolint:govet // Field order keeps persisted YAML provenance/audit records readable.
type FeedbackGuidance struct {
	ID             string                       `json:"id,omitempty" yaml:"id,omitempty"`
	Status         string                       `json:"status,omitempty" yaml:"status,omitempty"`
	SourceRun      string                       `json:"source_run,omitempty" yaml:"source_run,omitempty"`
	Action         string                       `json:"action,omitempty" yaml:"action,omitempty"`
	Reason         string                       `json:"reason,omitempty" yaml:"reason,omitempty"`
	Evidence       []string                     `json:"evidence,omitempty" yaml:"evidence,omitempty"`
	Confidence     float64                      `json:"confidence,omitempty" yaml:"confidence,omitempty"`
	Reviewer       string                       `json:"reviewer,omitempty" yaml:"reviewer,omitempty"`
	CreatedAt      time.Time                    `json:"created_at,omitzero" yaml:"created_at,omitempty"`
	UpdatedAt      time.Time                    `json:"updated_at,omitzero" yaml:"updated_at,omitempty"`
	ExpiresAt      *time.Time                   `json:"expires_at,omitempty" yaml:"expires_at,omitempty"`
	ConflictWith   []string                     `json:"conflict_with,omitempty" yaml:"conflict_with,omitempty"`
	RollbackReason string                       `json:"rollback_reason,omitempty" yaml:"rollback_reason,omitempty"`
	Audit          []FeedbackGuidanceAuditEvent `json:"audit,omitempty" yaml:"audit,omitempty"`
}

// FeedbackGuidanceAuditEvent records a lifecycle transition for feedback guidance.
type FeedbackGuidanceAuditEvent struct {
	At     time.Time `json:"at,omitzero" yaml:"at,omitempty"`
	Actor  string    `json:"actor,omitempty" yaml:"actor,omitempty"`
	Action string    `json:"action,omitempty" yaml:"action,omitempty"`
	Reason string    `json:"reason,omitempty" yaml:"reason,omitempty"`
}

// HookConfig configures a local command to receive atteler lifecycle events.
//
//nolint:govet // fieldalignment: field order follows config-file grouping.
type HookConfig struct {
	Env                map[string]string `json:"env,omitempty" yaml:"env,omitempty"`
	Command            []string          `json:"command,omitempty" yaml:"command,omitempty"`
	Payload            string            `json:"payload,omitempty" yaml:"payload,omitempty"`
	TimeoutSeconds     int               `json:"timeout_seconds,omitempty" yaml:"timeout_seconds,omitempty"`
	MaxAttempts        int               `json:"max_attempts,omitempty" yaml:"max_attempts,omitempty"`
	RetryBackoffMillis int               `json:"retry_backoff_millis,omitempty" yaml:"retry_backoff_millis,omitempty"`
	InheritEnv         bool              `json:"inherit_env,omitempty" yaml:"inherit_env,omitempty"`
	Blocking           bool              `json:"blocking,omitempty" yaml:"blocking,omitempty"`
}

// GenerationConfig configures default generation parameters for all requests.
type GenerationConfig struct {
	Temperature    *float64 `json:"temperature,omitempty" yaml:"temperature,omitempty"`
	TopP           *float64 `json:"top_p,omitempty" yaml:"top_p,omitempty"`
	Seed           *int     `json:"seed,omitempty" yaml:"seed,omitempty"`
	ModelMode      string   `json:"model_mode,omitempty" yaml:"model_mode,omitempty"`
	ReasoningLevel string   `json:"reasoning_level,omitempty" yaml:"reasoning_level,omitempty"`
	MaxTokens      int      `json:"max_tokens,omitempty" yaml:"max_tokens,omitempty"`
}

// ResearchConfig configures research, scout, and knowledge-oriented workflows.
type ResearchConfig struct {
	SourcePolicy sourcepolicy.Policy `json:"source_policy" yaml:"source_policy"`
}

// AgentLoopConfig configures the multi-turn tool execution loop.
type AgentLoopConfig struct {
	// MaxOutputBytes caps cumulative raw tool output per agent loop. Zero or nil
	// disables the cap.
	MaxOutputBytes *int64 `json:"max_output_bytes,omitempty" yaml:"max_output_bytes,omitempty"`
	// MaxCostMicros caps cumulative estimated provider cost in micro-units of
	// currency per agent loop. Zero or nil disables the cap. Runtime callers
	// fail closed when provider/model pricing metadata is unavailable.
	MaxCostMicros *int64 `json:"max_cost_micros,omitempty" yaml:"max_cost_micros,omitempty"`
	// MaxInputTokens caps cumulative model input tokens reported by providers
	// across an agent loop. It is separate from context.max_input_tokens, which
	// preflights each request before it is sent. Runtime callers fail closed
	// when token usage metadata is unavailable or incomplete.
	MaxInputTokens *int `json:"max_input_tokens,omitempty" yaml:"max_input_tokens,omitempty"`
	// MaxOutputTokens caps cumulative model output tokens reported by providers
	// across an agent loop. Zero or nil disables the cap. Runtime callers fail
	// closed when token usage metadata is unavailable or incomplete.
	MaxOutputTokens *int `json:"max_output_tokens,omitempty" yaml:"max_output_tokens,omitempty"`
	// MaxTotalTokens caps cumulative model input plus output tokens per agent
	// loop. Zero or nil disables the cap. Runtime callers fail closed when
	// token usage metadata is unavailable or incomplete.
	MaxTotalTokens *int `json:"max_total_tokens,omitempty" yaml:"max_total_tokens,omitempty"`
	// MaxIterations caps the number of tool-use turns per agent loop. Zero or
	// nil disables the cap (the loop runs until the model returns a final
	// response or another budget — model calls, tool calls, wall time —
	// trips). Defaults to nil (unlimited).
	MaxIterations *int `json:"max_iterations,omitempty" yaml:"max_iterations,omitempty"`
	// MaxModelCalls caps the number of model completions per agent loop. Zero
	// or nil disables the cap (the loop runs until the model returns a final
	// response or another budget trips). Defaults to nil (unlimited).
	MaxModelCalls *int `json:"max_model_calls,omitempty" yaml:"max_model_calls,omitempty"`
	// MaxToolCalls caps the number of tool executions per agent loop. Zero or
	// nil disables the cap (the loop runs without a tool-call ceiling).
	// Defaults to nil (unlimited).
	MaxToolCalls *int `json:"max_tool_calls,omitempty" yaml:"max_tool_calls,omitempty"`
	// MaxWallTime caps the wall-clock duration of an agent loop, parsed via
	// time.ParseDuration (e.g. "30m", "1h30m"). An empty string, "0", or nil
	// disables the cap (the loop runs without a wall-clock ceiling).
	MaxWallTime *string `json:"max_wall_time,omitempty" yaml:"max_wall_time,omitempty"`
	// CheckpointInterval is the number of completed tool-use iterations between
	// interactive "continue?" prompts. Zero or nil disables the prompt entirely
	// — the loop runs without asking the user to confirm continuation.
	CheckpointInterval *int `json:"checkpoint_interval,omitempty" yaml:"checkpoint_interval,omitempty"`
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
	DeniedSchemes        []string `json:"denied_schemes,omitempty" yaml:"denied_schemes,omitempty"`
	AllowedHosts         []string `json:"allowed_hosts,omitempty" yaml:"allowed_hosts,omitempty"`
	DeniedHosts          []string `json:"denied_hosts,omitempty" yaml:"denied_hosts,omitempty"`
	AllowedPorts         []int    `json:"allowed_ports,omitempty" yaml:"allowed_ports,omitempty"`
	DeniedPorts          []int    `json:"denied_ports,omitempty" yaml:"denied_ports,omitempty"`
	LocalRoots           []string `json:"local_roots,omitempty" yaml:"local_roots,omitempty"`
	DeniedLocalRoots     []string `json:"denied_local_roots,omitempty" yaml:"denied_local_roots,omitempty"`
	AllowedGlobs         []string `json:"allowed_globs,omitempty" yaml:"allowed_globs,omitempty"`
	DeniedGlobs          []string `json:"denied_globs,omitempty" yaml:"denied_globs,omitempty"`
	ContentTypes         []string `json:"content_types,omitempty" yaml:"content_types,omitempty"`
	MaxRedirects         int      `json:"max_redirects,omitempty" yaml:"max_redirects,omitempty"`
	MaxFiles             int      `json:"max_files,omitempty" yaml:"max_files,omitempty"`
	AllowAbsolutePaths   bool     `json:"allow_absolute_paths,omitempty" yaml:"allow_absolute_paths,omitempty"`
	AllowPrivateNetworks bool     `json:"allow_private_networks,omitempty" yaml:"allow_private_networks,omitempty"`
}

// MarshalYAML preserves explicitly empty list fields. For reference policy,
// nil means "unset; use loader defaults" while [] can be an intentional deny-all
// policy for fields such as allowed_schemes and content_types.
func (p ReferencePolicyConfig) MarshalYAML() (any, error) {
	out := make(map[string]any, 16)

	marshalStringListYAML(out, "allowed_schemes", p.AllowedSchemes)
	marshalStringListYAML(out, "denied_schemes", p.DeniedSchemes)
	marshalStringListYAML(out, "allowed_hosts", p.AllowedHosts)
	marshalStringListYAML(out, "denied_hosts", p.DeniedHosts)
	marshalIntListYAML(out, "allowed_ports", p.AllowedPorts)
	marshalIntListYAML(out, "denied_ports", p.DeniedPorts)
	marshalStringListYAML(out, "local_roots", p.LocalRoots)
	marshalStringListYAML(out, "denied_local_roots", p.DeniedLocalRoots)
	marshalStringListYAML(out, "allowed_globs", p.AllowedGlobs)
	marshalStringListYAML(out, "denied_globs", p.DeniedGlobs)
	marshalStringListYAML(out, "content_types", p.ContentTypes)

	if p.MaxRedirects != 0 {
		out["max_redirects"] = p.MaxRedirects
	}

	if p.MaxFiles != 0 {
		out["max_files"] = p.MaxFiles
	}

	if p.AllowAbsolutePaths {
		out["allow_absolute_paths"] = p.AllowAbsolutePaths
	}

	if p.AllowPrivateNetworks {
		out["allow_private_networks"] = p.AllowPrivateNetworks
	}

	return out, nil
}

func marshalStringListYAML(out map[string]any, field string, values []string) {
	if values != nil {
		out[field] = values
	}
}

func marshalIntListYAML(out map[string]any, field string, values []int) {
	if values != nil {
		out[field] = values
	}
}

// PluginConfig configures local plugin manifest discovery.
type PluginConfig struct {
	Policy *attelerplugin.Policy `json:"policy,omitempty" yaml:"policy,omitempty"`
	Paths  []string              `json:"paths,omitempty" yaml:"paths,omitempty"`
}

// SkillLearningConfig configures automatic recurring-workflow skill learning.
// A nil Enabled value means the default behavior should be used.
type SkillLearningConfig struct {
	Enabled         *bool  `json:"enabled,omitempty" yaml:"enabled,omitempty"`
	StoreDir        string `json:"store_dir,omitempty" yaml:"store_dir,omitempty"`
	SkillDir        string `json:"skill_dir,omitempty" yaml:"skill_dir,omitempty"`
	MaxObservations int    `json:"max_observations,omitempty" yaml:"max_observations,omitempty"`
	MaxSteps        int    `json:"max_steps,omitempty" yaml:"max_steps,omitempty"`
	MinOccurrences  int    `json:"min_occurrences,omitempty" yaml:"min_occurrences,omitempty"`
}

// VectorConfig configures local vector retrieval and persisted indexes.
type VectorConfig struct {
	WorkspaceEnabled               *bool                       `json:"workspace_enabled,omitempty" yaml:"workspace_enabled,omitempty"`
	WorkspaceAllowRemoteEmbeddings *bool                       `json:"workspace_allow_remote_embeddings,omitempty" yaml:"workspace_allow_remote_embeddings,omitempty"`
	Stores                         map[string]VectorizerConfig `json:"stores,omitempty" yaml:"stores,omitempty"`
	Agents                         map[string]VectorizerConfig `json:"agents,omitempty" yaml:"agents,omitempty"`
	Sources                        map[string]VectorizerConfig `json:"sources,omitempty" yaml:"sources,omitempty"`
	Vectorizer                     string                      `json:"vectorizer,omitempty" yaml:"vectorizer,omitempty"`
	Provider                       string                      `json:"provider,omitempty" yaml:"provider,omitempty"`
	Model                          string                      `json:"model,omitempty" yaml:"model,omitempty"`
	BaseURL                        string                      `json:"base_url,omitempty" yaml:"base_url,omitempty"`
	FallbackPolicy                 string                      `json:"fallback_policy,omitempty" yaml:"fallback_policy,omitempty"`
	IndexPath                      string                      `json:"index_path,omitempty" yaml:"index_path,omitempty"`
	WorkspaceIndexPath             string                      `json:"workspace_index_path,omitempty" yaml:"workspace_index_path,omitempty"`
	WorkspaceInclude               []string                    `json:"workspace_include,omitempty" yaml:"workspace_include,omitempty"`
	WorkspaceExclude               []string                    `json:"workspace_exclude,omitempty" yaml:"workspace_exclude,omitempty"`
	TimeoutSeconds                 int                         `json:"timeout_seconds,omitempty" yaml:"timeout_seconds,omitempty"`
	ChunkMaxRunes                  int                         `json:"chunk_max_runes,omitempty" yaml:"chunk_max_runes,omitempty"`
	ChunkOverlapRunes              int                         `json:"chunk_overlap_runes,omitempty" yaml:"chunk_overlap_runes,omitempty"`
	WorkspaceLimit                 int                         `json:"workspace_limit,omitempty" yaml:"workspace_limit,omitempty"`
	WorkspaceMaxFileBytes          int                         `json:"workspace_max_file_bytes,omitempty" yaml:"workspace_max_file_bytes,omitempty"`
	WorkspaceMaxFiles              int                         `json:"workspace_max_files,omitempty" yaml:"workspace_max_files,omitempty"`
}

// VectorizerConfig configures the vectorizer identity for a store, agent, or
// source-specific retrieval index. Empty fields inherit from the parent
// VectorConfig when resolved.
type VectorizerConfig struct {
	Vectorizer        string `json:"vectorizer,omitempty" yaml:"vectorizer,omitempty"`
	Provider          string `json:"provider,omitempty" yaml:"provider,omitempty"`
	Model             string `json:"model,omitempty" yaml:"model,omitempty"`
	BaseURL           string `json:"base_url,omitempty" yaml:"base_url,omitempty"`
	FallbackPolicy    string `json:"fallback_policy,omitempty" yaml:"fallback_policy,omitempty"`
	IndexPath         string `json:"index_path,omitempty" yaml:"index_path,omitempty"`
	TimeoutSeconds    int    `json:"timeout_seconds,omitempty" yaml:"timeout_seconds,omitempty"`
	ChunkMaxRunes     int    `json:"chunk_max_runes,omitempty" yaml:"chunk_max_runes,omitempty"`
	ChunkOverlapRunes int    `json:"chunk_overlap_runes,omitempty" yaml:"chunk_overlap_runes,omitempty"`
}

// VectorScope identifies the retrieval scope whose vectorizer should be
// resolved. Source overrides agent, agent overrides store, and all three
// inherit from the top-level vector config.
type VectorScope struct {
	Store  string
	Agent  string
	Source string
}

// DefaultVectorizerConfig returns the top-level vectorizer fields without any
// store, agent, or source-specific overrides.
func (c VectorConfig) DefaultVectorizerConfig() VectorizerConfig {
	return VectorizerConfig{
		Vectorizer:        strings.TrimSpace(c.Vectorizer),
		Provider:          strings.TrimSpace(c.Provider),
		Model:             strings.TrimSpace(c.Model),
		BaseURL:           strings.TrimSpace(c.BaseURL),
		FallbackPolicy:    strings.TrimSpace(c.FallbackPolicy),
		IndexPath:         strings.TrimSpace(c.IndexPath),
		TimeoutSeconds:    c.TimeoutSeconds,
		ChunkMaxRunes:     c.ChunkMaxRunes,
		ChunkOverlapRunes: c.ChunkOverlapRunes,
	}
}

// ResolveVectorizerConfig overlays store, agent, and source-specific
// vectorizer settings on the global vector config. It lets callers select
// lexical vs embedding vectorizers independently for workspace stores,
// per-agent memory, and source families such as files, sessions, or git
// history without duplicating provider/model defaults in config files.
func (c VectorConfig) ResolveVectorizerConfig(scope VectorScope) VectorizerConfig {
	resolved := c.DefaultVectorizerConfig()
	if scoped, ok := vectorizerScopeConfig(c.Stores, scope.Store); ok {
		resolved = overlayVectorizerConfig(resolved, scoped)
	}

	if scoped, ok := vectorizerScopeConfig(c.Agents, scope.Agent); ok {
		resolved = overlayVectorizerConfig(resolved, scoped)
	}

	if scoped, ok := vectorizerScopeConfig(c.Sources, scope.Source); ok {
		resolved = overlayVectorizerConfig(resolved, scoped)
	}

	return resolved
}

// WorktreeConfig configures opt-in worktree merge-back behavior.
type WorktreeConfig struct {
	AutoMerge            *bool    `json:"auto_merge,omitempty" yaml:"auto_merge,omitempty"`
	VerificationCommands []string `json:"verification_commands,omitempty" yaml:"verification_commands,omitempty"`
	OverrideVerification bool     `json:"override_verification,omitempty" yaml:"override_verification,omitempty"`
}

// MarshalYAML keeps override_verification visible when a worktree merge policy
// is shown, even when the safe default is false.
func (w WorktreeConfig) MarshalYAML() (any, error) {
	out := make(map[string]any, 3)

	if w.AutoMerge != nil {
		out["auto_merge"] = *w.AutoMerge
	}

	marshalStringListYAML(out, "verification_commands", w.VerificationCommands)

	if w.OverrideVerification || w.AutoMerge != nil || w.VerificationCommands != nil {
		out["override_verification"] = w.OverrideVerification
	}

	return out, nil
}

// defaultAutoModeName is the orchestration mode selected when `auto` is enabled
// via a boolean/truthy value. It mirrors autopilot.DefaultMode, duplicated here
// to avoid a config -> autopilot -> agent -> config import cycle.
const defaultAutoModeName = "auto"

// AutoMode is the configured default orchestration mode for interactive runs.
// It accepts either a YAML/JSON boolean (true => the default "auto" mode) or a
// mode-name string ("auto", "bug-hunt"); falsey values disable it. The mode
// name is validated at run time, not here.
type AutoMode string

// UnmarshalYAML accepts a boolean or a string scalar.
func (a *AutoMode) UnmarshalYAML(value *yaml.Node) error {
	var asBool bool
	if err := value.Decode(&asBool); err == nil {
		*a = autoModeFromBool(asBool)

		return nil
	}

	var asString string
	if err := value.Decode(&asString); err != nil {
		return fmt.Errorf("auto: expected a boolean or mode name: %w", err)
	}

	*a = AutoMode(normalizeAutoMode(asString))

	return nil
}

// UnmarshalJSON accepts a boolean or a string.
func (a *AutoMode) UnmarshalJSON(data []byte) error {
	var asBool bool
	if err := json.Unmarshal(data, &asBool); err == nil {
		*a = autoModeFromBool(asBool)

		return nil
	}

	var asString string
	if err := json.Unmarshal(data, &asString); err != nil {
		return fmt.Errorf("auto: expected a boolean or mode name: %w", err)
	}

	*a = AutoMode(normalizeAutoMode(asString))

	return nil
}

func autoModeFromBool(enabled bool) AutoMode {
	if enabled {
		return AutoMode(defaultAutoModeName)
	}

	return ""
}

// normalizeAutoMode maps truthy/falsey words to the default mode or off, and
// passes any other value through as a mode name.
func normalizeAutoMode(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "off", "false", "no", "disabled":
		return ""
	case "on", "true", "yes", "enabled", "default":
		return defaultAutoModeName
	default:
		return strings.TrimSpace(raw)
	}
}

//nolint:govet // fieldalignment: field order follows config-file grouping; deprecated aliases stay last.
type fileConfig struct {
	Version         *int                           `json:"version" yaml:"version"`
	Generation      fileGenerationConfig           `json:"generation" yaml:"generation"`
	Research        fileResearchConfig             `json:"research" yaml:"research"`
	AgentLoop       fileAgentLoopConfig            `json:"agent_loop" yaml:"agent_loop"`
	Context         fileContextConfig              `json:"context" yaml:"context"`
	Plugins         filePluginConfig               `json:"plugins" yaml:"plugins"`
	SkillLearning   fileSkillLearningConfig        `json:"skill_learning" yaml:"skill_learning"`
	Vector          fileVectorConfig               `json:"vector" yaml:"vector"`
	Worktree        fileWorktreeConfig             `json:"worktree" yaml:"worktree"`
	Autonomy        *string                        `json:"autonomy" yaml:"autonomy"`
	Auto            *AutoMode                      `json:"auto" yaml:"auto"`
	DefaultProvider *string                        `json:"default_provider" yaml:"default_provider"`
	DefaultModel    *string                        `json:"default_model" yaml:"default_model"`
	EventLedgerPath *string                        `json:"event_ledger_path" yaml:"event_ledger_path"`
	ModelAliases    map[string]string              `json:"model_aliases" yaml:"model_aliases"`
	ModelRoles      map[string]fileModelRoleConfig `json:"models" yaml:"models"`
	Providers       map[string]fileProviderConfig  `json:"providers" yaml:"providers"`
	Agents          map[string]fileAgentConfig     `json:"agents" yaml:"agents"`
	Hooks           map[string][]HookConfig        `json:"hooks" yaml:"hooks"`
	FallbackModels  []string                       `json:"fallback_models" yaml:"fallback_models"`

	DeprecatedProvider *string `json:"provider" yaml:"provider"`
	DeprecatedModel    *string `json:"model" yaml:"model"`
}

//nolint:govet // Field order follows the public provider YAML shape.
type fileProviderConfig struct {
	Disabled              *bool           `json:"disabled" yaml:"disabled"`
	Local                 *bool           `json:"local" yaml:"local"`
	AutoStart             *bool           `json:"auto_start" yaml:"auto_start"`
	DisablePrivateAdapter *bool           `json:"disable_private_adapter" yaml:"disable_private_adapter"`
	BaseURL               *string         `json:"base_url" yaml:"base_url"`
	Type                  *string         `json:"type" yaml:"type"`
	APIKeyEnv             *string         `json:"api_key_env" yaml:"api_key_env"`
	APIKeyHeader          *string         `json:"api_key_header" yaml:"api_key_header"`
	APIKeyScheme          *string         `json:"api_key_scheme" yaml:"api_key_scheme"`
	ChatCompletionsPath   *string         `json:"chat_completions_path" yaml:"chat_completions_path"`
	EmbeddingsPath        *string         `json:"embeddings_path" yaml:"embeddings_path"`
	ModelsPath            *string         `json:"models_path" yaml:"models_path"`
	APIVersion            *string         `json:"api_version" yaml:"api_version"`
	Models                []string        `json:"models" yaml:"models"`
	Capabilities          []string        `json:"capabilities" yaml:"capabilities"`
	Retry                 fileRetryConfig `json:"retry" yaml:"retry"`
	TimeoutSeconds        *int            `json:"timeout_seconds" yaml:"timeout_seconds"`
}

type fileRetryConfig struct {
	MaxAttempts      *int     `json:"max_attempts" yaml:"max_attempts"`
	InitialBackoffMS *int     `json:"initial_backoff_ms" yaml:"initial_backoff_ms"`
	MaxBackoffMS     *int     `json:"max_backoff_ms" yaml:"max_backoff_ms"`
	MaxElapsedMS     *int     `json:"max_elapsed_ms" yaml:"max_elapsed_ms"`
	JitterFraction   *float64 `json:"jitter_fraction" yaml:"jitter_fraction"`
}

type fileModelRoleConfig struct {
	RoutingPolicy        *RoutingPolicyConfig `json:"routing_policy" yaml:"routing_policy"`
	Preferred            *string              `json:"preferred" yaml:"preferred"`
	Fallback             *string              `json:"fallback" yaml:"fallback"`
	MaxCostUSD           *float64             `json:"max_cost_usd" yaml:"max_cost_usd"`
	MaxLatencyMS         *int                 `json:"max_latency_ms" yaml:"max_latency_ms"`
	MaxTTFTMS            *int                 `json:"max_ttft_ms" yaml:"max_ttft_ms"`
	RequireFreshMetadata *bool                `json:"require_fresh_metadata" yaml:"require_fresh_metadata"`
	PreferLocal          *bool                `json:"prefer_local" yaml:"prefer_local"`
	FallbackModels       []string             `json:"fallback_models" yaml:"fallback_models"`
	Fallbacks            []string             `json:"fallbacks" yaml:"fallbacks"`
	PreferredProviders   []string             `json:"preferred_providers" yaml:"preferred_providers"`
	BannedProviders      []string             `json:"banned_providers" yaml:"banned_providers"`
	BannedModels         []string             `json:"banned_models" yaml:"banned_models"`
	RequiredCapabilities []string             `json:"required_capabilities" yaml:"required_capabilities"`
}

//nolint:govet // fieldalignment: field order follows config-file grouping; deprecated aliases stay last.
type fileAgentConfig struct {
	Personality      *string              `json:"personality" yaml:"personality"`
	TopP             *float64             `json:"top_p" yaml:"top_p"`
	Seed             *int                 `json:"seed" yaml:"seed"`
	RoutingPolicy    *RoutingPolicyConfig `json:"routing_policy" yaml:"routing_policy"`
	Model            *string              `json:"model" yaml:"model"`
	Mode             *string              `json:"mode" yaml:"mode"`
	ModelMode        *string              `json:"model_mode" yaml:"model_mode"`
	ReasoningLevel   *string              `json:"reasoning_level" yaml:"reasoning_level"`
	Description      *string              `json:"description" yaml:"description"`
	Temperature      *float64             `json:"temperature" yaml:"temperature"`
	SystemPrompt     *string              `json:"system_prompt" yaml:"system_prompt"`
	ToolPermissions  map[string]bool      `json:"tools" yaml:"tools"`
	MaxTokens        *int                 `json:"max_tokens" yaml:"max_tokens"`
	Hidden           *bool                `json:"hidden" yaml:"hidden"`
	FallbackModels   []string             `json:"fallback_models" yaml:"fallback_models"`
	Capabilities     []string             `json:"capabilities" yaml:"capabilities"`
	Triggers         []string             `json:"triggers" yaml:"triggers"`
	References       []string             `json:"references" yaml:"references"`
	FeedbackGuidance []FeedbackGuidance   `json:"feedback_guidance" yaml:"feedback_guidance"`

	DeprecatedPrompt *string `json:"prompt" yaml:"prompt"`
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
	DeniedSchemes        []string `json:"denied_schemes" yaml:"denied_schemes"`
	AllowedHosts         []string `json:"allowed_hosts" yaml:"allowed_hosts"`
	DeniedHosts          []string `json:"denied_hosts" yaml:"denied_hosts"`
	AllowedPorts         []int    `json:"allowed_ports" yaml:"allowed_ports"`
	DeniedPorts          []int    `json:"denied_ports" yaml:"denied_ports"`
	LocalRoots           []string `json:"local_roots" yaml:"local_roots"`
	DeniedLocalRoots     []string `json:"denied_local_roots" yaml:"denied_local_roots"`
	AllowedGlobs         []string `json:"allowed_globs" yaml:"allowed_globs"`
	DeniedGlobs          []string `json:"denied_globs" yaml:"denied_globs"`
	ContentTypes         []string `json:"content_types" yaml:"content_types"`
	MaxRedirects         *int     `json:"max_redirects" yaml:"max_redirects"`
	MaxFiles             *int     `json:"max_files" yaml:"max_files"`
	AllowAbsolutePaths   *bool    `json:"allow_absolute_paths" yaml:"allow_absolute_paths"`
	AllowPrivateNetworks *bool    `json:"allow_private_networks" yaml:"allow_private_networks"`
}

type fileGenerationConfig struct {
	Temperature    *float64 `json:"temperature" yaml:"temperature"`
	TopP           *float64 `json:"top_p" yaml:"top_p"`
	Seed           *int     `json:"seed" yaml:"seed"`
	ModelMode      *string  `json:"model_mode" yaml:"model_mode"`
	ReasoningLevel *string  `json:"reasoning_level" yaml:"reasoning_level"`
	MaxTokens      *int     `json:"max_tokens" yaml:"max_tokens"`

	DeprecatedReasoning *string `json:"reasoning" yaml:"reasoning"`
}

type fileResearchConfig struct {
	SourcePolicy fileSourcePolicyConfig `json:"source_policy" yaml:"source_policy"`
}

//nolint:govet // Field order mirrors the public research.source_policy config shape.
type fileSourcePolicyConfig struct {
	TrustedDomains                     []string `json:"trusted_domains" yaml:"trusted_domains"`
	DeniedDomains                      []string `json:"denied_domains" yaml:"denied_domains"`
	PreferSourceTypes                  []string `json:"prefer_source_types" yaml:"prefer_source_types"`
	AllowLowTrustSources               *bool    `json:"allow_low_trust_sources" yaml:"allow_low_trust_sources"`
	WarnOnLowTrustSources              *bool    `json:"warn_on_low_trust_sources" yaml:"warn_on_low_trust_sources"`
	RequireEvidenceForHighImpactClaims *bool    `json:"require_evidence_for_high_impact_claims" yaml:"require_evidence_for_high_impact_claims"`
}

type fileAgentLoopConfig struct {
	MaxOutputBytes     *int64  `json:"max_output_bytes" yaml:"max_output_bytes"`
	MaxCostMicros      *int64  `json:"max_cost_micros" yaml:"max_cost_micros"`
	MaxInputTokens     *int    `json:"max_input_tokens" yaml:"max_input_tokens"`
	MaxOutputTokens    *int    `json:"max_output_tokens" yaml:"max_output_tokens"`
	MaxTotalTokens     *int    `json:"max_total_tokens" yaml:"max_total_tokens"`
	MaxIterations      *int    `json:"max_iterations" yaml:"max_iterations"`
	MaxModelCalls      *int    `json:"max_model_calls" yaml:"max_model_calls"`
	MaxToolCalls       *int    `json:"max_tool_calls" yaml:"max_tool_calls"`
	MaxWallTime        *string `json:"max_wall_time" yaml:"max_wall_time"`
	CheckpointInterval *int    `json:"checkpoint_interval" yaml:"checkpoint_interval"`
}

type filePluginConfig struct {
	Policy *attelerplugin.Policy `json:"policy" yaml:"policy"`
	Paths  []string              `json:"paths" yaml:"paths"`
}

type fileSkillLearningConfig struct {
	Enabled         *bool   `json:"enabled" yaml:"enabled"`
	StoreDir        *string `json:"store_dir" yaml:"store_dir"`
	SkillDir        *string `json:"skill_dir" yaml:"skill_dir"`
	MaxObservations *int    `json:"max_observations" yaml:"max_observations"`
	MaxSteps        *int    `json:"max_steps" yaml:"max_steps"`
	MinOccurrences  *int    `json:"min_occurrences" yaml:"min_occurrences"`
}

//nolint:govet // Field order mirrors the documented vector config schema.
type fileVectorConfig struct {
	WorkspaceEnabled               *bool                           `json:"workspace_enabled" yaml:"workspace_enabled"`
	WorkspaceAllowRemoteEmbeddings *bool                           `json:"workspace_allow_remote_embeddings" yaml:"workspace_allow_remote_embeddings"`
	Stores                         map[string]fileVectorizerConfig `json:"stores" yaml:"stores"`
	Agents                         map[string]fileVectorizerConfig `json:"agents" yaml:"agents"`
	Sources                        map[string]fileVectorizerConfig `json:"sources" yaml:"sources"`
	Vectorizer                     *string                         `json:"vectorizer" yaml:"vectorizer"`
	Provider                       *string                         `json:"provider" yaml:"provider"`
	Model                          *string                         `json:"model" yaml:"model"`
	BaseURL                        *string                         `json:"base_url" yaml:"base_url"`
	FallbackPolicy                 *string                         `json:"fallback_policy" yaml:"fallback_policy"`
	IndexPath                      *string                         `json:"index_path" yaml:"index_path"`
	WorkspaceIndexPath             *string                         `json:"workspace_index_path" yaml:"workspace_index_path"`
	WorkspaceInclude               []string                        `json:"workspace_include" yaml:"workspace_include"`
	WorkspaceExclude               []string                        `json:"workspace_exclude" yaml:"workspace_exclude"`
	TimeoutSeconds                 *int                            `json:"timeout_seconds" yaml:"timeout_seconds"`
	ChunkMaxRunes                  *int                            `json:"chunk_max_runes" yaml:"chunk_max_runes"`
	ChunkOverlapRunes              *int                            `json:"chunk_overlap_runes" yaml:"chunk_overlap_runes"`
	WorkspaceLimit                 *int                            `json:"workspace_limit" yaml:"workspace_limit"`
	WorkspaceMaxFileBytes          *int                            `json:"workspace_max_file_bytes" yaml:"workspace_max_file_bytes"`
	WorkspaceMaxFiles              *int                            `json:"workspace_max_files" yaml:"workspace_max_files"`
}

type fileVectorizerConfig struct {
	Vectorizer        *string `json:"vectorizer" yaml:"vectorizer"`
	Provider          *string `json:"provider" yaml:"provider"`
	Model             *string `json:"model" yaml:"model"`
	BaseURL           *string `json:"base_url" yaml:"base_url"`
	FallbackPolicy    *string `json:"fallback_policy" yaml:"fallback_policy"`
	IndexPath         *string `json:"index_path" yaml:"index_path"`
	TimeoutSeconds    *int    `json:"timeout_seconds" yaml:"timeout_seconds"`
	ChunkMaxRunes     *int    `json:"chunk_max_runes" yaml:"chunk_max_runes"`
	ChunkOverlapRunes *int    `json:"chunk_overlap_runes" yaml:"chunk_overlap_runes"`
}

type fileWorktreeConfig struct {
	AutoMerge            *bool    `json:"auto_merge" yaml:"auto_merge"`
	OverrideVerification *bool    `json:"override_verification" yaml:"override_verification"`
	VerificationCommands []string `json:"verification_commands" yaml:"verification_commands"`
}

// Load reads the default configuration files and returns the merged result plus
// the paths that were successfully loaded. Missing files are ignored.
func Load() (Config, []string, error) {
	cfg, loaded, _, _, err := LoadWithDiagnostics()

	return cfg, loaded, err
}

// LoadWithOrigins reads the default configuration stack and returns the merged
// config, successfully loaded paths, and per-field origin chains. Missing files
// are ignored.
func LoadWithOrigins() (Config, []string, OriginMap, error) {
	cfg, loaded, origins, _, err := LoadWithDiagnostics()

	return cfg, loaded, origins, err
}

// LoadWithDiagnostics reads the default configuration stack and returns the
// merged config, successfully loaded paths, per-field origin chains, and
// non-fatal diagnostics from best-effort harness importers.
func LoadWithDiagnostics() (Config, []string, OriginMap, []Diagnostic, error) {
	cfg, loaded, origins, diagnostics := LoadHarnessDefaultsWithDiagnostics()

	fileCfg, fileLoaded, fileOrigins, err := LoadPathSources(DefaultPathSources())
	mergeConfigFromOrigins(&cfg, fileCfg, origins, fileOrigins)

	loaded = append(loaded, fileLoaded...)

	normalizeEmptyMaps(&cfg)

	return cfg, loaded, origins, diagnostics, err
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

		decodeErr := dec.Decode(&next)
		if decodeErr != nil {
			// An empty or whitespace-only file produces io.EOF; treat it
			// as a no-op rather than a parse error.
			if errors.Is(decodeErr, io.EOF) {
				continue
			}

			return cfg, loaded, origins, fmt.Errorf("config: parse %s: %w", path, decodeErr)
		}

		next, err = migrateFileConfig(next, path)
		if err != nil {
			return cfg, loaded, origins, err
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

	if len(loaded) > 0 && cfg.Version == 0 {
		cfg.Version = ConfigSchemaVersion
	}

	return cfg, loaded, origins, nil
}

func migrateFileConfig(cfg fileConfig, path string) (fileConfig, error) {
	version, err := migrateLoadedConfigVersion(cfg.Version, path)
	if err != nil {
		return fileConfig{}, err
	}

	if version != nil {
		cfg.Version = version
	}

	if cfg.DefaultProvider == nil && cfg.DeprecatedProvider != nil {
		cfg.DefaultProvider = cfg.DeprecatedProvider
	}

	if cfg.DefaultModel == nil && cfg.DeprecatedModel != nil {
		cfg.DefaultModel = cfg.DeprecatedModel
	}

	if cfg.Generation.ReasoningLevel == nil && cfg.Generation.DeprecatedReasoning != nil {
		cfg.Generation.ReasoningLevel = cfg.Generation.DeprecatedReasoning
	}

	for name := range cfg.Agents {
		agent := cfg.Agents[name]
		if agent.SystemPrompt == nil && agent.DeprecatedPrompt != nil {
			agent.SystemPrompt = agent.DeprecatedPrompt
			cfg.Agents[name] = agent
		}
	}

	return cfg, nil
}

func migrateLoadedConfigVersion(version *int, path string) (*int, error) {
	if version == nil {
		return nil, nil
	}

	if *version < 0 || *version > ConfigSchemaVersion {
		return nil, fmt.Errorf(
			"config: unsupported version %d in %s; upgrade Atteler or remove this file after backing it up",
			*version,
			path,
		)
	}

	if *version == ConfigSchemaVersion {
		return version, nil
	}

	current := ConfigSchemaVersion

	return &current, nil
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

	if len(cfg.ModelAliases) == 0 {
		cfg.ModelAliases = nil
	}

	if len(cfg.ModelRoles) == 0 {
		cfg.ModelRoles = nil
	}

	if len(cfg.Agents) == 0 {
		cfg.Agents = nil
	}

	if len(cfg.Hooks) == 0 {
		cfg.Hooks = nil
	}
}

func mergeFileConfigWithOrigins(dst *Config, src fileConfig, rec *originRecorder, source originSource) {
	if src.Version != nil {
		dst.Version = *src.Version
		rec.set("version", source, *src.Version)
	}

	if src.DefaultProvider != nil {
		dst.DefaultProvider = *src.DefaultProvider
		rec.set("default_provider", source, *src.DefaultProvider)
	}

	if src.DefaultModel != nil {
		dst.DefaultModel = *src.DefaultModel
		rec.set("default_model", source, *src.DefaultModel)
	}

	if src.EventLedgerPath != nil {
		dst.EventLedgerPath = *src.EventLedgerPath
		rec.set("event_ledger_path", source, *src.EventLedgerPath)
	}

	if src.Autonomy != nil {
		dst.Autonomy = strings.TrimSpace(*src.Autonomy)
		rec.set("autonomy", source, dst.Autonomy)
	}

	if src.Auto != nil {
		dst.Auto = string(*src.Auto)
		rec.set("auto", source, dst.Auto)
	}

	if src.FallbackModels != nil {
		dst.FallbackModels = append([]string(nil), src.FallbackModels...)
		rec.replace("fallback_models", source, dst.FallbackModels, "replaces the entire fallback model list")
	}

	mergeModelAliases(dst, src.ModelAliases, rec, source)
	mergeModelRoles(dst, src.ModelRoles, rec, source)
	mergeProviders(dst, src.Providers, rec, source)
	mergeAgents(dst, src.Agents, rec, source)
	mergeHooks(dst, src.Hooks, rec, source)
	mergeGeneration(dst, src.Generation, rec, source)
	mergeResearch(dst, src.Research, rec, source)
	mergeAgentLoop(dst, src.AgentLoop, rec, source)
	mergeContext(dst, src.Context, rec, source)
	mergePlugins(dst, src.Plugins, rec, source)
	mergeSkillLearning(dst, src.SkillLearning, rec, source)
	mergeVector(dst, src.Vector, rec, source)
	mergeWorktree(dst, src.Worktree, rec, source)
}

func mergeModelAliases(dst *Config, aliases map[string]string, rec *originRecorder, source originSource) {
	if aliases != nil {
		rec.merge("model_aliases", source, sortedMapKeys(aliases), "merges model aliases by alias")
	}

	for alias, target := range aliases {
		if dst.ModelAliases == nil {
			dst.ModelAliases = make(map[string]string)
		}

		target = strings.TrimSpace(target)
		dst.ModelAliases[alias] = target
		rec.set(modelAliasFieldPath(alias), source, target)
	}
}

func mergeModelRoles(dst *Config, roles map[string]fileModelRoleConfig, rec *originRecorder, source originSource) {
	if roles != nil {
		rec.merge("models", source, sortedMapKeys(roles), "merges model role definitions by name")
	}

	for name := range roles {
		role := roles[name]

		if dst.ModelRoles == nil {
			dst.ModelRoles = make(map[string]ModelRoleConfig)
		}

		entityPath := modelRoleFieldPath(name)
		rec.merge(entityPath, source, name, "merges model role fields by name")

		current := dst.ModelRoles[name]
		mergeFileModelRole(&current, role, rec, source, name)
		dst.ModelRoles[name] = current
	}
}

func mergeFileModelRole(
	current *ModelRoleConfig,
	role fileModelRoleConfig,
	rec *originRecorder,
	source originSource,
	name string,
) {
	if role.Preferred != nil {
		current.Preferred = strings.TrimSpace(*role.Preferred)
		rec.set(modelRoleFieldPath(name, "preferred"), source, current.Preferred)
	}

	if role.Fallback != nil {
		fallback := strings.TrimSpace(*role.Fallback)
		if fallback != "" {
			current.FallbackModels = []string{fallback}
		} else {
			current.FallbackModels = nil
		}

		rec.replace(modelRoleFieldPath(name, "fallback"), source, current.FallbackModels, "replaces the model role fallback list")
	}

	if role.Fallbacks != nil {
		current.FallbackModels = append([]string(nil), role.Fallbacks...)
		rec.replace(modelRoleFieldPath(name, "fallbacks"), source, current.FallbackModels, "replaces the model role fallback list")
	}

	if role.FallbackModels != nil {
		current.FallbackModels = append([]string(nil), role.FallbackModels...)
		rec.replace(modelRoleFieldPath(name, "fallback_models"), source, current.FallbackModels, "replaces the model role fallback list")
	}

	if role.RoutingPolicy != nil {
		current.RoutingPolicy = cloneRoutingPolicy(*role.RoutingPolicy)
		rec.replace(modelRoleFieldPath(name, "routing_policy"), source, current.RoutingPolicy, "replaces the model role routing policy")
	}

	if role.PreferredProviders != nil {
		current.PreferredProviders = append([]string(nil), role.PreferredProviders...)
		rec.replace(modelRoleFieldPath(name, "preferred_providers"), source, current.PreferredProviders, "replaces the model role preferred provider list")
	}

	if role.BannedProviders != nil {
		current.BannedProviders = append([]string(nil), role.BannedProviders...)
		rec.replace(modelRoleFieldPath(name, "banned_providers"), source, current.BannedProviders, "replaces the model role banned provider list")
	}

	if role.BannedModels != nil {
		current.BannedModels = append([]string(nil), role.BannedModels...)
		rec.replace(modelRoleFieldPath(name, "banned_models"), source, current.BannedModels, "replaces the model role banned model list")
	}

	if role.RequiredCapabilities != nil {
		current.RequiredCapabilities = append([]string(nil), role.RequiredCapabilities...)
		rec.replace(modelRoleFieldPath(name, "required_capabilities"), source, current.RequiredCapabilities, "replaces the model role required capability list")
	}

	mergeFileModelRoleLimits(current, role, rec, source, name)

	if role.RequireFreshMetadata != nil {
		current.RequireFreshMetadata = *role.RequireFreshMetadata
		rec.set(modelRoleFieldPath(name, "require_fresh_metadata"), source, current.RequireFreshMetadata)
	}

	if role.PreferLocal != nil {
		current.PreferLocal = *role.PreferLocal
		rec.set(modelRoleFieldPath(name, "prefer_local"), source, current.PreferLocal)
	}
}

func mergeFileModelRoleLimits(
	current *ModelRoleConfig,
	role fileModelRoleConfig,
	rec *originRecorder,
	source originSource,
	name string,
) {
	if role.MaxCostUSD != nil {
		current.MaxCostUSD = *role.MaxCostUSD
		rec.set(modelRoleFieldPath(name, "max_cost_usd"), source, current.MaxCostUSD)
	}

	if role.MaxLatencyMS != nil {
		current.MaxLatencyMS = *role.MaxLatencyMS
		rec.set(modelRoleFieldPath(name, "max_latency_ms"), source, current.MaxLatencyMS)
	}

	if role.MaxTTFTMS != nil {
		current.MaxTTFTMS = *role.MaxTTFTMS
		rec.set(modelRoleFieldPath(name, "max_ttft_ms"), source, current.MaxTTFTMS)
	}
}

//nolint:gocognit,cyclop // Sequential nil-guarded provider field merge mirrors the YAML shape.
func mergeProviders(dst *Config, providers map[string]fileProviderConfig, rec *originRecorder, source originSource) {
	if providers != nil {
		rec.merge("providers", source, sortedMapKeys(providers), "merges provider definitions by name")
	}

	for name := range providers {
		provider := providers[name]

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

		if provider.Local != nil {
			current.Local = *provider.Local
			rec.set(providerFieldPath(name, "local"), source, *provider.Local)
		}

		if provider.AutoStart != nil {
			current.AutoStart = *provider.AutoStart
			rec.set(providerFieldPath(name, "auto_start"), source, *provider.AutoStart)
		}

		if provider.BaseURL != nil {
			current.BaseURL = *provider.BaseURL
			rec.set(providerFieldPath(name, "base_url"), source, *provider.BaseURL)
		}

		if provider.Type != nil {
			current.Type = strings.TrimSpace(*provider.Type)
			rec.set(providerFieldPath(name, "type"), source, current.Type)
		}

		if provider.APIKeyEnv != nil {
			current.APIKeyEnv = strings.TrimSpace(*provider.APIKeyEnv)
			rec.set(providerFieldPath(name, "api_key_env"), source, current.APIKeyEnv)
		}

		if provider.APIKeyHeader != nil {
			current.APIKeyHeader = strings.TrimSpace(*provider.APIKeyHeader)
			rec.set(providerFieldPath(name, "api_key_header"), source, current.APIKeyHeader)
		}

		if provider.APIKeyScheme != nil {
			current.APIKeyScheme = strings.TrimSpace(*provider.APIKeyScheme)
			rec.set(providerFieldPath(name, "api_key_scheme"), source, current.APIKeyScheme)
		}

		if provider.ChatCompletionsPath != nil {
			current.ChatCompletionsPath = strings.TrimSpace(*provider.ChatCompletionsPath)
			rec.set(providerFieldPath(name, "chat_completions_path"), source, current.ChatCompletionsPath)
		}

		if provider.EmbeddingsPath != nil {
			current.EmbeddingsPath = strings.TrimSpace(*provider.EmbeddingsPath)
			rec.set(providerFieldPath(name, "embeddings_path"), source, current.EmbeddingsPath)
		}

		if provider.ModelsPath != nil {
			current.ModelsPath = strings.TrimSpace(*provider.ModelsPath)
			rec.set(providerFieldPath(name, "models_path"), source, current.ModelsPath)
		}

		if provider.APIVersion != nil {
			current.APIVersion = strings.TrimSpace(*provider.APIVersion)
			rec.set(providerFieldPath(name, "api_version"), source, current.APIVersion)
		}

		if provider.Models != nil {
			current.Models = append([]string(nil), provider.Models...)
			rec.replace(providerFieldPath(name, "models"), source, current.Models, "replaces the provider static model list")
		}

		if provider.Capabilities != nil {
			current.Capabilities = append([]string(nil), provider.Capabilities...)
			rec.replace(providerFieldPath(name, "capabilities"), source, current.Capabilities, "replaces the provider capability list")
		}

		if provider.DisablePrivateAdapter != nil {
			current.DisablePrivateAdapter = *provider.DisablePrivateAdapter
			rec.set(providerFieldPath(name, "disable_private_adapter"), source, *provider.DisablePrivateAdapter)
		}

		if provider.TimeoutSeconds != nil {
			current.TimeoutSeconds = *provider.TimeoutSeconds
			rec.set(providerFieldPath(name, "timeout_seconds"), source, *provider.TimeoutSeconds)
		}

		mergeFileRetryConfig(&current.Retry, provider.Retry, rec, source, providerFieldPath(name, "retry"))

		dst.Providers[name] = current
	}
}

func mergeFileRetryConfig(dst *RetryConfig, src fileRetryConfig, rec *originRecorder, source originSource, path string) {
	if !src.hasFields() {
		return
	}

	rec.merge(path, source, "retry", "merges provider retry fields")

	if src.MaxAttempts != nil {
		dst.MaxAttempts = src.MaxAttempts
		rec.set(dottedPath(path, "max_attempts"), source, *src.MaxAttempts)
	}

	if src.InitialBackoffMS != nil {
		dst.InitialBackoffMS = src.InitialBackoffMS
		rec.set(dottedPath(path, "initial_backoff_ms"), source, *src.InitialBackoffMS)
	}

	if src.MaxBackoffMS != nil {
		dst.MaxBackoffMS = src.MaxBackoffMS
		rec.set(dottedPath(path, "max_backoff_ms"), source, *src.MaxBackoffMS)
	}

	if src.MaxElapsedMS != nil {
		dst.MaxElapsedMS = src.MaxElapsedMS
		rec.set(dottedPath(path, "max_elapsed_ms"), source, *src.MaxElapsedMS)
	}

	if src.JitterFraction != nil {
		dst.JitterFraction = src.JitterFraction
		rec.set(dottedPath(path, "jitter_fraction"), source, *src.JitterFraction)
	}
}

func (c fileRetryConfig) hasFields() bool {
	return c.MaxAttempts != nil ||
		c.InitialBackoffMS != nil ||
		c.MaxBackoffMS != nil ||
		c.MaxElapsedMS != nil ||
		c.JitterFraction != nil
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

	if agent.ModelMode != nil {
		current.ModelMode = strings.TrimSpace(*agent.ModelMode)
		rec.set(agentFieldPath(name, "model_mode"), source, current.ModelMode)
	}

	if agent.ToolPermissions != nil {
		current.ToolPermissions = make(map[string]bool, len(agent.ToolPermissions))
		maps.Copy(current.ToolPermissions, agent.ToolPermissions)
		rec.replace(agentFieldPath(name, "tools"), source, current.ToolPermissions, "replaces the entire tool permissions map")
	}

	if agent.RoutingPolicy != nil {
		current.RoutingPolicy = cloneRoutingPolicy(*agent.RoutingPolicy)
		rec.replace(agentFieldPath(name, "routing_policy"), source, current.RoutingPolicy, "replaces the entire routing policy")
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

	if agent.FeedbackGuidance != nil {
		current.FeedbackGuidance = cloneFeedbackGuidance(agent.FeedbackGuidance)
		rec.replace(agentFieldPath(name, "feedback_guidance"), source, current.FeedbackGuidance, "replaces the entire feedback guidance list")
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
				Command:            append([]string(nil), hook.Command...),
				Env:                cloneMap(hook.Env),
				Payload:            hook.Payload,
				TimeoutSeconds:     hook.TimeoutSeconds,
				MaxAttempts:        hook.MaxAttempts,
				RetryBackoffMillis: hook.RetryBackoffMillis,
				InheritEnv:         hook.InheritEnv,
				Blocking:           hook.Blocking,
			}
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

	mergeReferencePolicy(&dst.Context.ReferencePolicy, contextConfig.ReferencePolicy, rec, source)
}

func mergeReferencePolicy(dst *ReferencePolicyConfig, policy fileReferencePolicyConfig, rec *originRecorder, source originSource) {
	mergeReferencePolicyLists(dst, policy, rec, source)
	mergeReferencePolicyLimits(dst, policy, rec, source)
}

func mergeReferencePolicyLists(dst *ReferencePolicyConfig, policy fileReferencePolicyConfig, rec *originRecorder, source originSource) {
	if policy.AllowedSchemes != nil {
		dst.AllowedSchemes = cloneSlicePreserveEmpty(policy.AllowedSchemes)
		rec.replace(referencePolicyFieldPath("allowed_schemes"), source, policy.AllowedSchemes, "replaces the entire allowed scheme list")
	}

	if policy.DeniedSchemes != nil {
		dst.DeniedSchemes = cloneSlicePreserveEmpty(policy.DeniedSchemes)
		rec.replace(referencePolicyFieldPath("denied_schemes"), source, policy.DeniedSchemes, "replaces the entire denied scheme list")
	}

	if policy.AllowedHosts != nil {
		dst.AllowedHosts = cloneSlicePreserveEmpty(policy.AllowedHosts)
		rec.replace(referencePolicyFieldPath("allowed_hosts"), source, policy.AllowedHosts, "replaces the entire allowed host list")
	}

	if policy.DeniedHosts != nil {
		dst.DeniedHosts = cloneSlicePreserveEmpty(policy.DeniedHosts)
		rec.replace(referencePolicyFieldPath("denied_hosts"), source, policy.DeniedHosts, "replaces the entire denied host list")
	}

	if policy.AllowedPorts != nil {
		dst.AllowedPorts = cloneSlicePreserveEmpty(policy.AllowedPorts)
		rec.replace(referencePolicyFieldPath("allowed_ports"), source, policy.AllowedPorts, "replaces the entire allowed port list")
	}

	if policy.DeniedPorts != nil {
		dst.DeniedPorts = cloneSlicePreserveEmpty(policy.DeniedPorts)
		rec.replace(referencePolicyFieldPath("denied_ports"), source, policy.DeniedPorts, "replaces the entire denied port list")
	}

	if policy.LocalRoots != nil {
		dst.LocalRoots = cloneSlicePreserveEmpty(policy.LocalRoots)
		rec.replace(referencePolicyFieldPath("local_roots"), source, policy.LocalRoots, "replaces the entire local root list")
	}

	if policy.DeniedLocalRoots != nil {
		dst.DeniedLocalRoots = cloneSlicePreserveEmpty(policy.DeniedLocalRoots)
		rec.replace(referencePolicyFieldPath("denied_local_roots"), source, policy.DeniedLocalRoots, "replaces the entire denied local root list")
	}

	if policy.AllowedGlobs != nil {
		dst.AllowedGlobs = cloneSlicePreserveEmpty(policy.AllowedGlobs)
		rec.replace(referencePolicyFieldPath("allowed_globs"), source, policy.AllowedGlobs, "replaces the entire allowed glob list")
	}

	if policy.DeniedGlobs != nil {
		dst.DeniedGlobs = cloneSlicePreserveEmpty(policy.DeniedGlobs)
		rec.replace(referencePolicyFieldPath("denied_globs"), source, policy.DeniedGlobs, "replaces the entire denied glob list")
	}

	if policy.ContentTypes != nil {
		dst.ContentTypes = cloneSlicePreserveEmpty(policy.ContentTypes)
		rec.replace(referencePolicyFieldPath("content_types"), source, policy.ContentTypes, "replaces the entire allowed content-type list")
	}
}

func mergeReferencePolicyLimits(dst *ReferencePolicyConfig, policy fileReferencePolicyConfig, rec *originRecorder, source originSource) {
	if policy.MaxRedirects != nil {
		dst.MaxRedirects = *policy.MaxRedirects
		rec.set(referencePolicyFieldPath("max_redirects"), source, *policy.MaxRedirects)
	}

	if policy.MaxFiles != nil {
		dst.MaxFiles = *policy.MaxFiles
		rec.set(referencePolicyFieldPath("max_files"), source, *policy.MaxFiles)
	}

	if policy.AllowAbsolutePaths != nil {
		dst.AllowAbsolutePaths = *policy.AllowAbsolutePaths
		rec.set(referencePolicyFieldPath("allow_absolute_paths"), source, *policy.AllowAbsolutePaths)
	}

	if policy.AllowPrivateNetworks != nil {
		dst.AllowPrivateNetworks = *policy.AllowPrivateNetworks
		rec.set(referencePolicyFieldPath("allow_private_networks"), source, *policy.AllowPrivateNetworks)
	}
}

func referencePolicyFieldPath(field string) string {
	return "context.reference_policy." + field
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

	if generation.ModelMode != nil {
		dst.Generation.ModelMode = strings.TrimSpace(*generation.ModelMode)
		rec.set("generation.model_mode", source, dst.Generation.ModelMode)
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

func mergeResearch(dst *Config, research fileResearchConfig, rec *originRecorder, source originSource) {
	mergeSourcePolicy(&dst.Research.SourcePolicy, research.SourcePolicy, rec, source)
}

func mergeSourcePolicy(dst *sourcepolicy.Policy, policy fileSourcePolicyConfig, rec *originRecorder, source originSource) {
	if policy.TrustedDomains != nil {
		dst.TrustedDomains = sourcepolicy.NormalizeDomains(policy.TrustedDomains)
		rec.replace(sourcePolicyFieldPath("trusted_domains"), source, dst.TrustedDomains, "replaces the entire trusted source domain list")
	}

	if policy.DeniedDomains != nil {
		dst.DeniedDomains = sourcepolicy.NormalizeDomains(policy.DeniedDomains)
		rec.replace(sourcePolicyFieldPath("denied_domains"), source, dst.DeniedDomains, "replaces the entire denied source domain list")
	}

	if policy.PreferSourceTypes != nil {
		dst.PreferSourceTypes = sourcepolicy.NormalizeSourceTypes(policy.PreferSourceTypes)
		rec.replace(sourcePolicyFieldPath("prefer_source_types"), source, dst.PreferSourceTypes, "replaces the entire preferred source type list")
	}

	if policy.AllowLowTrustSources != nil {
		dst.AllowLowTrustSources = cloneBoolPointer(policy.AllowLowTrustSources)
		rec.set(sourcePolicyFieldPath("allow_low_trust_sources"), source, *policy.AllowLowTrustSources)
	}

	if policy.WarnOnLowTrustSources != nil {
		dst.WarnOnLowTrustSources = cloneBoolPointer(policy.WarnOnLowTrustSources)
		rec.set(sourcePolicyFieldPath("warn_on_low_trust_sources"), source, *policy.WarnOnLowTrustSources)
	}

	if policy.RequireEvidenceForHighImpactClaims != nil {
		dst.RequireEvidenceForHighImpactClaims = cloneBoolPointer(policy.RequireEvidenceForHighImpactClaims)
		rec.set(sourcePolicyFieldPath("require_evidence_for_high_impact_claims"), source, *policy.RequireEvidenceForHighImpactClaims)
	}
}

func sourcePolicyFieldPath(field string) string {
	return "research.source_policy." + field
}

func mergeAgentLoop(dst *Config, agentLoop fileAgentLoopConfig, rec *originRecorder, source originSource) {
	if agentLoop.MaxOutputBytes != nil {
		value := *agentLoop.MaxOutputBytes
		dst.AgentLoop.MaxOutputBytes = &value
		rec.set("agent_loop.max_output_bytes", source, value)
	}

	if agentLoop.MaxCostMicros != nil {
		value := *agentLoop.MaxCostMicros
		dst.AgentLoop.MaxCostMicros = &value
		rec.set("agent_loop.max_cost_micros", source, value)
	}

	if agentLoop.MaxInputTokens != nil {
		value := *agentLoop.MaxInputTokens
		dst.AgentLoop.MaxInputTokens = &value
		rec.set("agent_loop.max_input_tokens", source, value)
	}

	if agentLoop.MaxOutputTokens != nil {
		value := *agentLoop.MaxOutputTokens
		dst.AgentLoop.MaxOutputTokens = &value
		rec.set("agent_loop.max_output_tokens", source, value)
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

	if agentLoop.MaxModelCalls != nil {
		value := *agentLoop.MaxModelCalls
		dst.AgentLoop.MaxModelCalls = &value
		rec.set("agent_loop.max_model_calls", source, value)
	}

	if agentLoop.MaxToolCalls != nil {
		value := *agentLoop.MaxToolCalls
		dst.AgentLoop.MaxToolCalls = &value
		rec.set("agent_loop.max_tool_calls", source, value)
	}

	if agentLoop.MaxWallTime != nil {
		value := *agentLoop.MaxWallTime
		dst.AgentLoop.MaxWallTime = &value
		rec.set("agent_loop.max_wall_time", source, value)
	}

	if agentLoop.CheckpointInterval != nil {
		value := *agentLoop.CheckpointInterval
		dst.AgentLoop.CheckpointInterval = &value
		rec.set("agent_loop.checkpoint_interval", source, value)
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

		rec.replace("plugins.policy", source, "configured", "replaces the plugin execution policy")
	}
}

func mergeSkillLearning(dst *Config, skillLearning fileSkillLearningConfig, rec *originRecorder, source originSource) {
	if skillLearning.Enabled != nil {
		value := *skillLearning.Enabled
		dst.SkillLearning.Enabled = &value
		rec.set("skill_learning.enabled", source, value)
	}

	if skillLearning.StoreDir != nil {
		dst.SkillLearning.StoreDir = strings.TrimSpace(*skillLearning.StoreDir)
		rec.set("skill_learning.store_dir", source, dst.SkillLearning.StoreDir)
	}

	if skillLearning.SkillDir != nil {
		dst.SkillLearning.SkillDir = strings.TrimSpace(*skillLearning.SkillDir)
		rec.set("skill_learning.skill_dir", source, dst.SkillLearning.SkillDir)
	}

	if skillLearning.MaxObservations != nil {
		dst.SkillLearning.MaxObservations = *skillLearning.MaxObservations
		rec.set("skill_learning.max_observations", source, dst.SkillLearning.MaxObservations)
	}

	if skillLearning.MaxSteps != nil {
		dst.SkillLearning.MaxSteps = *skillLearning.MaxSteps
		rec.set("skill_learning.max_steps", source, dst.SkillLearning.MaxSteps)
	}

	if skillLearning.MinOccurrences != nil {
		dst.SkillLearning.MinOccurrences = *skillLearning.MinOccurrences
		rec.set("skill_learning.min_occurrences", source, dst.SkillLearning.MinOccurrences)
	}
}

//nolint:cyclop // Merge functions intentionally mirror each schema field for origin tracking.
func mergeVector(dst *Config, vector fileVectorConfig, rec *originRecorder, source originSource) {
	if vector.WorkspaceEnabled != nil {
		value := *vector.WorkspaceEnabled
		dst.Vector.WorkspaceEnabled = &value
		rec.set("vector.workspace_enabled", source, value)
	}

	if vector.WorkspaceAllowRemoteEmbeddings != nil {
		value := *vector.WorkspaceAllowRemoteEmbeddings
		dst.Vector.WorkspaceAllowRemoteEmbeddings = &value
		rec.set("vector.workspace_allow_remote_embeddings", source, value)
	}

	dst.Vector.Stores = mergeFileVectorizerConfigMap(dst.Vector.Stores, "stores", vector.Stores, rec, source)
	dst.Vector.Agents = mergeFileVectorizerConfigMap(dst.Vector.Agents, "agents", vector.Agents, rec, source)
	dst.Vector.Sources = mergeFileVectorizerConfigMap(dst.Vector.Sources, "sources", vector.Sources, rec, source)

	if vector.Vectorizer != nil {
		value := strings.TrimSpace(*vector.Vectorizer)
		dst.Vector.Vectorizer = value
		rec.set("vector.vectorizer", source, value)
	}

	if vector.Provider != nil {
		value := strings.TrimSpace(*vector.Provider)
		dst.Vector.Provider = value
		rec.set("vector.provider", source, value)
	}

	if vector.Model != nil {
		value := strings.TrimSpace(*vector.Model)
		dst.Vector.Model = value
		rec.set("vector.model", source, value)
	}

	if vector.BaseURL != nil {
		value := strings.TrimSpace(*vector.BaseURL)
		dst.Vector.BaseURL = value
		rec.set("vector.base_url", source, value)
	}

	if vector.FallbackPolicy != nil {
		value := strings.TrimSpace(*vector.FallbackPolicy)
		dst.Vector.FallbackPolicy = value
		rec.set("vector.fallback_policy", source, value)
	}

	if vector.IndexPath != nil {
		value := strings.TrimSpace(*vector.IndexPath)
		dst.Vector.IndexPath = value
		rec.set("vector.index_path", source, value)
	}

	if vector.WorkspaceIndexPath != nil {
		value := strings.TrimSpace(*vector.WorkspaceIndexPath)
		dst.Vector.WorkspaceIndexPath = value
		rec.set("vector.workspace_index_path", source, value)
	}

	if vector.WorkspaceInclude != nil {
		dst.Vector.WorkspaceInclude = append([]string(nil), vector.WorkspaceInclude...)
		rec.replace("vector.workspace_include", source, dst.Vector.WorkspaceInclude, "replaces the workspace vector include pattern list")
	}

	if vector.WorkspaceExclude != nil {
		dst.Vector.WorkspaceExclude = append([]string(nil), vector.WorkspaceExclude...)
		rec.replace("vector.workspace_exclude", source, dst.Vector.WorkspaceExclude, "replaces the workspace vector exclude pattern list")
	}

	if vector.TimeoutSeconds != nil {
		value := *vector.TimeoutSeconds
		dst.Vector.TimeoutSeconds = value
		rec.set("vector.timeout_seconds", source, value)
	}

	if vector.ChunkMaxRunes != nil {
		value := *vector.ChunkMaxRunes
		dst.Vector.ChunkMaxRunes = value
		rec.set("vector.chunk_max_runes", source, value)
	}

	if vector.ChunkOverlapRunes != nil {
		value := *vector.ChunkOverlapRunes
		dst.Vector.ChunkOverlapRunes = value
		rec.set("vector.chunk_overlap_runes", source, value)
	}

	if vector.WorkspaceLimit != nil {
		value := *vector.WorkspaceLimit
		dst.Vector.WorkspaceLimit = value
		rec.set("vector.workspace_limit", source, value)
	}

	if vector.WorkspaceMaxFileBytes != nil {
		value := *vector.WorkspaceMaxFileBytes
		dst.Vector.WorkspaceMaxFileBytes = value
		rec.set("vector.workspace_max_file_bytes", source, value)
	}

	if vector.WorkspaceMaxFiles != nil {
		value := *vector.WorkspaceMaxFiles
		dst.Vector.WorkspaceMaxFiles = value
		rec.set("vector.workspace_max_files", source, value)
	}
}

func mergeWorktree(dst *Config, worktree fileWorktreeConfig, rec *originRecorder, source originSource) {
	if worktree.AutoMerge != nil {
		value := *worktree.AutoMerge
		dst.Worktree.AutoMerge = &value
		rec.set("worktree.auto_merge", source, value)
	}

	if worktree.VerificationCommands != nil {
		dst.Worktree.VerificationCommands = append([]string(nil), worktree.VerificationCommands...)
		rec.replace("worktree.verification_commands", source, dst.Worktree.VerificationCommands, "replaces the entire worktree verification command list")
	}

	if worktree.OverrideVerification != nil {
		dst.Worktree.OverrideVerification = *worktree.OverrideVerification
		rec.set("worktree.override_verification", source, dst.Worktree.OverrideVerification)
	}
}

func mergeConfigFromSource(dst *Config, src Config, rec *originRecorder, source originSource) {
	if src.Version > 0 {
		dst.Version = src.Version
		rec.set("version", source, src.Version)
	}

	if src.DefaultProvider != "" {
		dst.DefaultProvider = src.DefaultProvider
		rec.set("default_provider", source, src.DefaultProvider)
	}

	if src.DefaultModel != "" {
		dst.DefaultModel = src.DefaultModel
		rec.set("default_model", source, src.DefaultModel)
	}

	if src.EventLedgerPath != "" {
		dst.EventLedgerPath = src.EventLedgerPath
		rec.set("event_ledger_path", source, src.EventLedgerPath)
	}

	if strings.TrimSpace(src.Autonomy) != "" {
		dst.Autonomy = strings.TrimSpace(src.Autonomy)
		rec.set("autonomy", source, dst.Autonomy)
	}

	if src.FallbackModels != nil {
		dst.FallbackModels = append([]string(nil), src.FallbackModels...)
		rec.replace("fallback_models", source, dst.FallbackModels, "replaces the entire fallback model list")
	}

	mergeConfigModelAliases(dst, src.ModelAliases, rec, source)
	mergeConfigModelRoles(dst, src.ModelRoles, rec, source)
	mergeConfigProviders(dst, src.Providers, rec, source)
	mergeConfigAgents(dst, src.Agents, rec, source)
	mergeConfigHooks(dst, src.Hooks, rec, source)
	mergeConfigGeneration(dst, src.Generation, rec, source)
	mergeConfigResearch(dst, src.Research, rec, source)
	mergeConfigAgentLoop(dst, src.AgentLoop, rec, source)
	mergeConfigContext(dst, src.Context, rec, source)
	mergeConfigPlugins(dst, src.Plugins, rec, source)
	mergeConfigSkillLearning(dst, src.SkillLearning, rec, source)
	mergeConfigVector(dst, src.Vector, rec, source)
	mergeConfigWorktree(dst, src.Worktree, rec, source)
}

func mergeConfigModelAliases(dst *Config, aliases map[string]string, rec *originRecorder, source originSource) {
	if aliases != nil {
		rec.merge("model_aliases", source, sortedMapKeys(aliases), "merges model aliases by alias")
	}

	for alias, target := range aliases {
		if dst.ModelAliases == nil {
			dst.ModelAliases = make(map[string]string)
		}

		target = strings.TrimSpace(target)
		dst.ModelAliases[alias] = target
		rec.set(modelAliasFieldPath(alias), source, target)
	}
}

func mergeConfigModelRoles(dst *Config, roles map[string]ModelRoleConfig, rec *originRecorder, source originSource) {
	if roles != nil {
		rec.merge("models", source, sortedMapKeys(roles), "merges model role definitions by name")
	}

	for name := range roles {
		role := roles[name]

		if dst.ModelRoles == nil {
			dst.ModelRoles = make(map[string]ModelRoleConfig)
		}

		rec.merge(modelRoleFieldPath(name), source, name, "merges model role fields by name")

		current := dst.ModelRoles[name]
		mergeConfigModelRole(&current, role, rec, source, name)
		dst.ModelRoles[name] = current
	}
}

func mergeConfigModelRole(current *ModelRoleConfig, role ModelRoleConfig, rec *originRecorder, source originSource, name string) {
	if role.Preferred != "" {
		current.Preferred = strings.TrimSpace(role.Preferred)
		rec.set(modelRoleFieldPath(name, "preferred"), source, current.Preferred)
	}

	if role.FallbackModels != nil {
		current.FallbackModels = append([]string(nil), role.FallbackModels...)
		rec.replace(modelRoleFieldPath(name, "fallback_models"), source, current.FallbackModels, "replaces the model role fallback list")
	}

	if routingPolicyConfigured(role.RoutingPolicy) {
		current.RoutingPolicy = cloneRoutingPolicy(role.RoutingPolicy)
		rec.replace(modelRoleFieldPath(name, "routing_policy"), source, current.RoutingPolicy, "replaces the model role routing policy")
	}

	if role.PreferredProviders != nil {
		current.PreferredProviders = append([]string(nil), role.PreferredProviders...)
		rec.replace(modelRoleFieldPath(name, "preferred_providers"), source, current.PreferredProviders, "replaces the model role preferred provider list")
	}

	if role.BannedProviders != nil {
		current.BannedProviders = append([]string(nil), role.BannedProviders...)
		rec.replace(modelRoleFieldPath(name, "banned_providers"), source, current.BannedProviders, "replaces the model role banned provider list")
	}

	if role.BannedModels != nil {
		current.BannedModels = append([]string(nil), role.BannedModels...)
		rec.replace(modelRoleFieldPath(name, "banned_models"), source, current.BannedModels, "replaces the model role banned model list")
	}

	if role.RequiredCapabilities != nil {
		current.RequiredCapabilities = append([]string(nil), role.RequiredCapabilities...)
		rec.replace(modelRoleFieldPath(name, "required_capabilities"), source, current.RequiredCapabilities, "replaces the model role required capability list")
	}

	if role.MaxCostUSD > 0 {
		current.MaxCostUSD = role.MaxCostUSD
		rec.set(modelRoleFieldPath(name, "max_cost_usd"), source, current.MaxCostUSD)
	}

	if role.MaxLatencyMS > 0 {
		current.MaxLatencyMS = role.MaxLatencyMS
		rec.set(modelRoleFieldPath(name, "max_latency_ms"), source, current.MaxLatencyMS)
	}

	if role.MaxTTFTMS > 0 {
		current.MaxTTFTMS = role.MaxTTFTMS
		rec.set(modelRoleFieldPath(name, "max_ttft_ms"), source, current.MaxTTFTMS)
	}

	if role.RequireFreshMetadata {
		current.RequireFreshMetadata = true
		rec.set(modelRoleFieldPath(name, "require_fresh_metadata"), source, current.RequireFreshMetadata)
	}

	if role.PreferLocal {
		current.PreferLocal = true
		rec.set(modelRoleFieldPath(name, "prefer_local"), source, current.PreferLocal)
	}
}

//nolint:gocognit,cyclop // Sequential non-zero provider field merge mirrors the public config shape.
func mergeConfigProviders(dst *Config, providers map[string]ProviderConfig, rec *originRecorder, source originSource) {
	if providers != nil {
		rec.merge("providers", source, sortedMapKeys(providers), "merges provider definitions by name")
	}

	for name := range providers {
		provider := providers[name]

		if dst.Providers == nil {
			dst.Providers = make(map[string]ProviderConfig)
		}

		rec.merge(providerFieldPath(name), source, name, "merges provider fields by name")

		current := dst.Providers[name]
		if provider.BaseURL != "" {
			current.BaseURL = provider.BaseURL
			rec.set(providerFieldPath(name, "base_url"), source, provider.BaseURL)
		}

		if provider.Type != "" {
			current.Type = strings.TrimSpace(provider.Type)
			rec.set(providerFieldPath(name, "type"), source, current.Type)
		}

		if provider.APIKeyEnv != "" {
			current.APIKeyEnv = strings.TrimSpace(provider.APIKeyEnv)
			rec.set(providerFieldPath(name, "api_key_env"), source, current.APIKeyEnv)
		}

		if provider.APIKeyHeader != "" {
			current.APIKeyHeader = strings.TrimSpace(provider.APIKeyHeader)
			rec.set(providerFieldPath(name, "api_key_header"), source, current.APIKeyHeader)
		}

		if provider.APIKeyScheme != "" {
			current.APIKeyScheme = strings.TrimSpace(provider.APIKeyScheme)
			rec.set(providerFieldPath(name, "api_key_scheme"), source, current.APIKeyScheme)
		}

		if provider.ChatCompletionsPath != "" {
			current.ChatCompletionsPath = strings.TrimSpace(provider.ChatCompletionsPath)
			rec.set(providerFieldPath(name, "chat_completions_path"), source, current.ChatCompletionsPath)
		}

		if provider.EmbeddingsPath != "" {
			current.EmbeddingsPath = strings.TrimSpace(provider.EmbeddingsPath)
			rec.set(providerFieldPath(name, "embeddings_path"), source, current.EmbeddingsPath)
		}

		if provider.ModelsPath != "" {
			current.ModelsPath = strings.TrimSpace(provider.ModelsPath)
			rec.set(providerFieldPath(name, "models_path"), source, current.ModelsPath)
		}

		if provider.APIVersion != "" {
			current.APIVersion = strings.TrimSpace(provider.APIVersion)
			rec.set(providerFieldPath(name, "api_version"), source, current.APIVersion)
		}

		if provider.Models != nil {
			current.Models = append([]string(nil), provider.Models...)
			rec.replace(providerFieldPath(name, "models"), source, current.Models, "replaces the provider static model list")
		}

		if provider.Capabilities != nil {
			current.Capabilities = append([]string(nil), provider.Capabilities...)
			rec.replace(providerFieldPath(name, "capabilities"), source, current.Capabilities, "replaces the provider capability list")
		}

		if provider.Disabled {
			current.Disabled = true
			rec.set(providerFieldPath(name, "disabled"), source, provider.Disabled)
		}

		if provider.Local {
			current.Local = true
			rec.set(providerFieldPath(name, "local"), source, provider.Local)
		}

		if provider.DisablePrivateAdapter {
			current.DisablePrivateAdapter = true
			rec.set(providerFieldPath(name, "disable_private_adapter"), source, provider.DisablePrivateAdapter)
		}

		current.AutoStart = provider.AutoStart
		rec.set(providerFieldPath(name, "auto_start"), source, provider.AutoStart)

		if provider.TimeoutSeconds > 0 {
			current.TimeoutSeconds = provider.TimeoutSeconds
			rec.set(providerFieldPath(name, "timeout_seconds"), source, provider.TimeoutSeconds)
		}

		mergeRetryConfig(&current.Retry, provider.Retry, rec, source, providerFieldPath(name, "retry"))

		dst.Providers[name] = current
	}
}

func mergeRetryConfig(dst *RetryConfig, src RetryConfig, rec *originRecorder, source originSource, path string) {
	if !src.hasFields() {
		return
	}

	rec.merge(path, source, "retry", "merges provider retry fields")

	if src.MaxAttempts != nil {
		dst.MaxAttempts = src.MaxAttempts
		rec.set(dottedPath(path, "max_attempts"), source, *src.MaxAttempts)
	}

	if src.InitialBackoffMS != nil {
		dst.InitialBackoffMS = src.InitialBackoffMS
		rec.set(dottedPath(path, "initial_backoff_ms"), source, *src.InitialBackoffMS)
	}

	if src.MaxBackoffMS != nil {
		dst.MaxBackoffMS = src.MaxBackoffMS
		rec.set(dottedPath(path, "max_backoff_ms"), source, *src.MaxBackoffMS)
	}

	if src.MaxElapsedMS != nil {
		dst.MaxElapsedMS = src.MaxElapsedMS
		rec.set(dottedPath(path, "max_elapsed_ms"), source, *src.MaxElapsedMS)
	}

	if src.JitterFraction != nil {
		dst.JitterFraction = src.JitterFraction
		rec.set(dottedPath(path, "jitter_fraction"), source, *src.JitterFraction)
	}
}

func (c RetryConfig) hasFields() bool {
	return c.MaxAttempts != nil ||
		c.InitialBackoffMS != nil ||
		c.MaxBackoffMS != nil ||
		c.MaxElapsedMS != nil ||
		c.JitterFraction != nil
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

	if agent.ModelMode != "" {
		current.ModelMode = strings.TrimSpace(agent.ModelMode)
		rec.set(agentFieldPath(name, "model_mode"), source, current.ModelMode)
	}

	if agent.ToolPermissions != nil {
		current.ToolPermissions = make(map[string]bool, len(agent.ToolPermissions))
		maps.Copy(current.ToolPermissions, agent.ToolPermissions)
		rec.replace(agentFieldPath(name, "tools"), source, current.ToolPermissions, "replaces the entire tool permissions map")
	}

	if routingPolicyConfigured(agent.RoutingPolicy) {
		current.RoutingPolicy = cloneRoutingPolicy(agent.RoutingPolicy)
		rec.replace(agentFieldPath(name, "routing_policy"), source, current.RoutingPolicy, "replaces the entire routing policy")
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

	if agent.FeedbackGuidance != nil {
		current.FeedbackGuidance = cloneFeedbackGuidance(agent.FeedbackGuidance)
		rec.replace(agentFieldPath(name, "feedback_guidance"), source, current.FeedbackGuidance, "replaces the entire feedback guidance list")
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

	mergeConfigReferencePolicy(&dst.Context.ReferencePolicy, contextConfig.ReferencePolicy, rec, source)
}

func mergeConfigReferencePolicy(dst *ReferencePolicyConfig, policy ReferencePolicyConfig, rec *originRecorder, source originSource) {
	mergeConfigReferencePolicyLists(dst, policy, rec, source)
	mergeConfigReferencePolicyLimits(dst, policy, rec, source)
}

func mergeConfigReferencePolicyLists(dst *ReferencePolicyConfig, policy ReferencePolicyConfig, rec *originRecorder, source originSource) {
	if policy.AllowedSchemes != nil {
		dst.AllowedSchemes = cloneSlicePreserveEmpty(policy.AllowedSchemes)
		rec.replace(referencePolicyFieldPath("allowed_schemes"), source, policy.AllowedSchemes, "replaces the entire allowed scheme list")
	}

	if policy.DeniedSchemes != nil {
		dst.DeniedSchemes = cloneSlicePreserveEmpty(policy.DeniedSchemes)
		rec.replace(referencePolicyFieldPath("denied_schemes"), source, policy.DeniedSchemes, "replaces the entire denied scheme list")
	}

	if policy.AllowedHosts != nil {
		dst.AllowedHosts = cloneSlicePreserveEmpty(policy.AllowedHosts)
		rec.replace(referencePolicyFieldPath("allowed_hosts"), source, policy.AllowedHosts, "replaces the entire allowed host list")
	}

	if policy.DeniedHosts != nil {
		dst.DeniedHosts = cloneSlicePreserveEmpty(policy.DeniedHosts)
		rec.replace(referencePolicyFieldPath("denied_hosts"), source, policy.DeniedHosts, "replaces the entire denied host list")
	}

	if policy.AllowedPorts != nil {
		dst.AllowedPorts = cloneSlicePreserveEmpty(policy.AllowedPorts)
		rec.replace(referencePolicyFieldPath("allowed_ports"), source, policy.AllowedPorts, "replaces the entire allowed port list")
	}

	if policy.DeniedPorts != nil {
		dst.DeniedPorts = cloneSlicePreserveEmpty(policy.DeniedPorts)
		rec.replace(referencePolicyFieldPath("denied_ports"), source, policy.DeniedPorts, "replaces the entire denied port list")
	}

	if policy.LocalRoots != nil {
		dst.LocalRoots = cloneSlicePreserveEmpty(policy.LocalRoots)
		rec.replace(referencePolicyFieldPath("local_roots"), source, policy.LocalRoots, "replaces the entire local root list")
	}

	if policy.DeniedLocalRoots != nil {
		dst.DeniedLocalRoots = cloneSlicePreserveEmpty(policy.DeniedLocalRoots)
		rec.replace(referencePolicyFieldPath("denied_local_roots"), source, policy.DeniedLocalRoots, "replaces the entire denied local root list")
	}

	if policy.AllowedGlobs != nil {
		dst.AllowedGlobs = cloneSlicePreserveEmpty(policy.AllowedGlobs)
		rec.replace(referencePolicyFieldPath("allowed_globs"), source, policy.AllowedGlobs, "replaces the entire allowed glob list")
	}

	if policy.DeniedGlobs != nil {
		dst.DeniedGlobs = cloneSlicePreserveEmpty(policy.DeniedGlobs)
		rec.replace(referencePolicyFieldPath("denied_globs"), source, policy.DeniedGlobs, "replaces the entire denied glob list")
	}

	if policy.ContentTypes != nil {
		dst.ContentTypes = cloneSlicePreserveEmpty(policy.ContentTypes)
		rec.replace(referencePolicyFieldPath("content_types"), source, policy.ContentTypes, "replaces the entire allowed content-type list")
	}
}

func mergeConfigReferencePolicyLimits(dst *ReferencePolicyConfig, policy ReferencePolicyConfig, rec *originRecorder, source originSource) {
	if policy.MaxRedirects > 0 {
		dst.MaxRedirects = policy.MaxRedirects
		rec.set(referencePolicyFieldPath("max_redirects"), source, policy.MaxRedirects)
	}

	if policy.MaxFiles > 0 {
		dst.MaxFiles = policy.MaxFiles
		rec.set(referencePolicyFieldPath("max_files"), source, policy.MaxFiles)
	}

	if policy.AllowAbsolutePaths {
		dst.AllowAbsolutePaths = true

		rec.set(referencePolicyFieldPath("allow_absolute_paths"), source, policy.AllowAbsolutePaths)
	}

	if policy.AllowPrivateNetworks {
		dst.AllowPrivateNetworks = true

		rec.set(referencePolicyFieldPath("allow_private_networks"), source, policy.AllowPrivateNetworks)
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

	if generation.ModelMode != "" {
		dst.Generation.ModelMode = strings.TrimSpace(generation.ModelMode)
		rec.set("generation.model_mode", source, dst.Generation.ModelMode)
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

func mergeConfigResearch(dst *Config, research ResearchConfig, rec *originRecorder, source originSource) {
	mergeConfigSourcePolicy(&dst.Research.SourcePolicy, research.SourcePolicy, rec, source)
}

func mergeConfigSourcePolicy(dst *sourcepolicy.Policy, policy sourcepolicy.Policy, rec *originRecorder, source originSource) {
	if policy.TrustedDomains != nil {
		dst.TrustedDomains = sourcepolicy.NormalizeDomains(policy.TrustedDomains)
		rec.replace(sourcePolicyFieldPath("trusted_domains"), source, dst.TrustedDomains, "replaces the entire trusted source domain list")
	}

	if policy.DeniedDomains != nil {
		dst.DeniedDomains = sourcepolicy.NormalizeDomains(policy.DeniedDomains)
		rec.replace(sourcePolicyFieldPath("denied_domains"), source, dst.DeniedDomains, "replaces the entire denied source domain list")
	}

	if policy.PreferSourceTypes != nil {
		dst.PreferSourceTypes = sourcepolicy.NormalizeSourceTypes(policy.PreferSourceTypes)
		rec.replace(sourcePolicyFieldPath("prefer_source_types"), source, dst.PreferSourceTypes, "replaces the entire preferred source type list")
	}

	if policy.AllowLowTrustSources != nil {
		dst.AllowLowTrustSources = cloneBoolPointer(policy.AllowLowTrustSources)
		rec.set(sourcePolicyFieldPath("allow_low_trust_sources"), source, *policy.AllowLowTrustSources)
	}

	if policy.WarnOnLowTrustSources != nil {
		dst.WarnOnLowTrustSources = cloneBoolPointer(policy.WarnOnLowTrustSources)
		rec.set(sourcePolicyFieldPath("warn_on_low_trust_sources"), source, *policy.WarnOnLowTrustSources)
	}

	if policy.RequireEvidenceForHighImpactClaims != nil {
		dst.RequireEvidenceForHighImpactClaims = cloneBoolPointer(policy.RequireEvidenceForHighImpactClaims)
		rec.set(sourcePolicyFieldPath("require_evidence_for_high_impact_claims"), source, *policy.RequireEvidenceForHighImpactClaims)
	}
}

func mergeConfigAgentLoop(dst *Config, agentLoop AgentLoopConfig, rec *originRecorder, source originSource) {
	if agentLoop.MaxOutputBytes != nil {
		value := *agentLoop.MaxOutputBytes
		dst.AgentLoop.MaxOutputBytes = &value
		rec.set("agent_loop.max_output_bytes", source, value)
	}

	if agentLoop.MaxCostMicros != nil {
		value := *agentLoop.MaxCostMicros
		dst.AgentLoop.MaxCostMicros = &value
		rec.set("agent_loop.max_cost_micros", source, value)
	}

	if agentLoop.MaxInputTokens != nil {
		value := *agentLoop.MaxInputTokens
		dst.AgentLoop.MaxInputTokens = &value
		rec.set("agent_loop.max_input_tokens", source, value)
	}

	if agentLoop.MaxOutputTokens != nil {
		value := *agentLoop.MaxOutputTokens
		dst.AgentLoop.MaxOutputTokens = &value
		rec.set("agent_loop.max_output_tokens", source, value)
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

	if agentLoop.MaxModelCalls != nil {
		value := *agentLoop.MaxModelCalls
		dst.AgentLoop.MaxModelCalls = &value
		rec.set("agent_loop.max_model_calls", source, value)
	}

	if agentLoop.MaxToolCalls != nil {
		value := *agentLoop.MaxToolCalls
		dst.AgentLoop.MaxToolCalls = &value
		rec.set("agent_loop.max_tool_calls", source, value)
	}

	if agentLoop.MaxWallTime != nil {
		value := *agentLoop.MaxWallTime
		dst.AgentLoop.MaxWallTime = &value
		rec.set("agent_loop.max_wall_time", source, value)
	}

	if agentLoop.CheckpointInterval != nil {
		value := *agentLoop.CheckpointInterval
		dst.AgentLoop.CheckpointInterval = &value
		rec.set("agent_loop.checkpoint_interval", source, value)
	}
}

func mergeConfigPlugins(dst *Config, plugins PluginConfig, rec *originRecorder, source originSource) {
	if plugins.Paths != nil {
		dst.Plugins.Paths = append([]string(nil), plugins.Paths...)
		rec.replace("plugins.paths", source, dst.Plugins.Paths, "replaces the entire plugin path list")
	}

	if plugins.Policy != nil {
		policy := attelerplugin.ClonePolicy(*plugins.Policy)
		dst.Plugins.Policy = &policy

		rec.replace("plugins.policy", source, "configured", "replaces the plugin execution policy")
	}
}

func mergeConfigSkillLearning(dst *Config, skillLearning SkillLearningConfig, rec *originRecorder, source originSource) {
	if skillLearning.Enabled != nil {
		value := *skillLearning.Enabled
		dst.SkillLearning.Enabled = &value
		rec.set("skill_learning.enabled", source, value)
	}

	if skillLearning.StoreDir != "" {
		dst.SkillLearning.StoreDir = skillLearning.StoreDir
		rec.set("skill_learning.store_dir", source, skillLearning.StoreDir)
	}

	if skillLearning.SkillDir != "" {
		dst.SkillLearning.SkillDir = skillLearning.SkillDir
		rec.set("skill_learning.skill_dir", source, skillLearning.SkillDir)
	}

	if skillLearning.MaxObservations > 0 {
		dst.SkillLearning.MaxObservations = skillLearning.MaxObservations
		rec.set("skill_learning.max_observations", source, skillLearning.MaxObservations)
	}

	if skillLearning.MaxSteps > 0 {
		dst.SkillLearning.MaxSteps = skillLearning.MaxSteps
		rec.set("skill_learning.max_steps", source, skillLearning.MaxSteps)
	}

	if skillLearning.MinOccurrences > 0 {
		dst.SkillLearning.MinOccurrences = skillLearning.MinOccurrences
		rec.set("skill_learning.min_occurrences", source, skillLearning.MinOccurrences)
	}
}

func mergeConfigFromOrigins(dst *Config, src Config, dstOrigins, srcOrigins OriginMap) {
	if src.Version > 0 {
		dst.Version = src.Version

		appendOriginChain(dstOrigins, "version", srcOrigins, false)
	}

	if src.DefaultProvider != "" {
		dst.DefaultProvider = src.DefaultProvider

		appendOriginChain(dstOrigins, "default_provider", srcOrigins, false)
	}

	if src.DefaultModel != "" {
		dst.DefaultModel = src.DefaultModel

		appendOriginChain(dstOrigins, "default_model", srcOrigins, false)
	}

	if src.EventLedgerPath != "" {
		dst.EventLedgerPath = src.EventLedgerPath

		appendOriginChain(dstOrigins, "event_ledger_path", srcOrigins, false)
	}

	if strings.TrimSpace(src.Autonomy) != "" {
		dst.Autonomy = strings.TrimSpace(src.Autonomy)

		appendOriginChain(dstOrigins, "autonomy", srcOrigins, false)
	}

	if src.FallbackModels != nil {
		dst.FallbackModels = append([]string(nil), src.FallbackModels...)

		appendOriginChain(dstOrigins, "fallback_models", srcOrigins, true)
	}

	mergeConfigModelAliasesFromOrigins(dst, src.ModelAliases, dstOrigins, srcOrigins)
	mergeConfigModelRolesFromOrigins(dst, src.ModelRoles, dstOrigins, srcOrigins)
	mergeConfigProvidersFromOrigins(dst, src.Providers, dstOrigins, srcOrigins)
	mergeConfigAgentsFromOrigins(dst, src.Agents, dstOrigins, srcOrigins)
	mergeConfigHooksFromOrigins(dst, src.Hooks, dstOrigins, srcOrigins)
	mergeConfigGenerationFromOrigins(dst, src.Generation, dstOrigins, srcOrigins)
	mergeConfigResearchFromOrigins(dst, src.Research, dstOrigins, srcOrigins)
	mergeConfigAgentLoopFromOrigins(dst, src.AgentLoop, dstOrigins, srcOrigins)
	mergeConfigContextFromOrigins(dst, src.Context, dstOrigins, srcOrigins)
	mergeConfigPluginsFromOrigins(dst, src.Plugins, dstOrigins, srcOrigins)
	mergeConfigSkillLearningFromOrigins(dst, src.SkillLearning, dstOrigins, srcOrigins)
	mergeConfigVectorFromOrigins(dst, src.Vector, dstOrigins, srcOrigins)
	mergeConfigWorktreeFromOrigins(dst, src.Worktree, dstOrigins, srcOrigins)
}

func mergeConfigModelAliasesFromOrigins(dst *Config, aliases map[string]string, dstOrigins, srcOrigins OriginMap) {
	if aliases != nil {
		appendOriginChain(dstOrigins, "model_aliases", srcOrigins, false)
	}

	for alias, target := range aliases {
		if dst.ModelAliases == nil {
			dst.ModelAliases = make(map[string]string)
		}

		dst.ModelAliases[alias] = strings.TrimSpace(target)
		appendOriginChain(dstOrigins, modelAliasFieldPath(alias), srcOrigins, false)
	}
}

func mergeConfigModelRolesFromOrigins(dst *Config, roles map[string]ModelRoleConfig, dstOrigins, srcOrigins OriginMap) {
	if roles != nil {
		appendOriginChain(dstOrigins, "models", srcOrigins, false)
	}

	for name := range roles {
		role := roles[name]

		if dst.ModelRoles == nil {
			dst.ModelRoles = make(map[string]ModelRoleConfig)
		}

		appendOriginChain(dstOrigins, modelRoleFieldPath(name), srcOrigins, false)

		current := dst.ModelRoles[name]
		mergeConfigModelRoleFromOrigins(&current, role, dstOrigins, srcOrigins, name)
		dst.ModelRoles[name] = current
	}
}

func mergeConfigModelRoleFromOrigins(
	current *ModelRoleConfig,
	role ModelRoleConfig,
	dstOrigins OriginMap,
	srcOrigins OriginMap,
	name string,
) {
	preferredPath := modelRoleFieldPath(name, "preferred")
	if originPathExists(srcOrigins, preferredPath) {
		current.Preferred = strings.TrimSpace(role.Preferred)

		appendOriginChain(dstOrigins, preferredPath, srcOrigins, false)
	}

	fallbackModelsPath := firstExistingOriginPath(
		srcOrigins,
		modelRoleFieldPath(name, "fallback_models"),
		modelRoleFieldPath(name, "fallbacks"),
		modelRoleFieldPath(name, "fallback"),
	)
	if fallbackModelsPath != "" {
		current.FallbackModels = append([]string(nil), role.FallbackModels...)

		appendOriginChain(dstOrigins, fallbackModelsPath, srcOrigins, true)
	}

	routingPolicyPath := modelRoleFieldPath(name, "routing_policy")
	if originPathExists(srcOrigins, routingPolicyPath) {
		current.RoutingPolicy = cloneRoutingPolicy(role.RoutingPolicy)

		appendOriginChain(dstOrigins, routingPolicyPath, srcOrigins, true)
	}

	preferredProvidersPath := modelRoleFieldPath(name, "preferred_providers")
	if originPathExists(srcOrigins, preferredProvidersPath) {
		current.PreferredProviders = append([]string(nil), role.PreferredProviders...)

		appendOriginChain(dstOrigins, preferredProvidersPath, srcOrigins, true)
	}

	bannedProvidersPath := modelRoleFieldPath(name, "banned_providers")
	if originPathExists(srcOrigins, bannedProvidersPath) {
		current.BannedProviders = append([]string(nil), role.BannedProviders...)

		appendOriginChain(dstOrigins, bannedProvidersPath, srcOrigins, true)
	}

	bannedModelsPath := modelRoleFieldPath(name, "banned_models")
	if originPathExists(srcOrigins, bannedModelsPath) {
		current.BannedModels = append([]string(nil), role.BannedModels...)

		appendOriginChain(dstOrigins, bannedModelsPath, srcOrigins, true)
	}

	requiredCapabilitiesPath := modelRoleFieldPath(name, "required_capabilities")
	if originPathExists(srcOrigins, requiredCapabilitiesPath) {
		current.RequiredCapabilities = append([]string(nil), role.RequiredCapabilities...)

		appendOriginChain(dstOrigins, requiredCapabilitiesPath, srcOrigins, true)
	}

	maxCostPath := modelRoleFieldPath(name, "max_cost_usd")
	if originPathExists(srcOrigins, maxCostPath) {
		current.MaxCostUSD = role.MaxCostUSD

		appendOriginChain(dstOrigins, maxCostPath, srcOrigins, false)
	}

	maxLatencyPath := modelRoleFieldPath(name, "max_latency_ms")
	if originPathExists(srcOrigins, maxLatencyPath) {
		current.MaxLatencyMS = role.MaxLatencyMS

		appendOriginChain(dstOrigins, maxLatencyPath, srcOrigins, false)
	}

	maxTTFTPath := modelRoleFieldPath(name, "max_ttft_ms")
	if originPathExists(srcOrigins, maxTTFTPath) {
		current.MaxTTFTMS = role.MaxTTFTMS

		appendOriginChain(dstOrigins, maxTTFTPath, srcOrigins, false)
	}

	requireFreshMetadataPath := modelRoleFieldPath(name, "require_fresh_metadata")
	if originPathExists(srcOrigins, requireFreshMetadataPath) {
		current.RequireFreshMetadata = role.RequireFreshMetadata

		appendOriginChain(dstOrigins, requireFreshMetadataPath, srcOrigins, false)
	}

	preferLocalPath := modelRoleFieldPath(name, "prefer_local")
	if originPathExists(srcOrigins, preferLocalPath) {
		current.PreferLocal = role.PreferLocal

		appendOriginChain(dstOrigins, preferLocalPath, srcOrigins, false)
	}
}

//nolint:gocognit,cyclop // Sequential origin-aware provider field merge keeps provenance explicit.
func mergeConfigProvidersFromOrigins(dst *Config, providers map[string]ProviderConfig, dstOrigins, srcOrigins OriginMap) {
	if providers != nil {
		appendOriginChain(dstOrigins, "providers", srcOrigins, false)
	}

	for name := range providers {
		provider := providers[name]

		if dst.Providers == nil {
			dst.Providers = make(map[string]ProviderConfig)
		}

		appendOriginChain(dstOrigins, providerFieldPath(name), srcOrigins, false)

		current := dst.Providers[name]
		if provider.BaseURL != "" {
			current.BaseURL = provider.BaseURL

			appendOriginChain(dstOrigins, providerFieldPath(name, "base_url"), srcOrigins, false)
		}

		if provider.Type != "" {
			current.Type = strings.TrimSpace(provider.Type)

			appendOriginChain(dstOrigins, providerFieldPath(name, "type"), srcOrigins, false)
		}

		if provider.APIKeyEnv != "" {
			current.APIKeyEnv = strings.TrimSpace(provider.APIKeyEnv)

			appendOriginChain(dstOrigins, providerFieldPath(name, "api_key_env"), srcOrigins, false)
		}

		if provider.APIKeyHeader != "" {
			current.APIKeyHeader = strings.TrimSpace(provider.APIKeyHeader)

			appendOriginChain(dstOrigins, providerFieldPath(name, "api_key_header"), srcOrigins, false)
		}

		if provider.APIKeyScheme != "" {
			current.APIKeyScheme = strings.TrimSpace(provider.APIKeyScheme)

			appendOriginChain(dstOrigins, providerFieldPath(name, "api_key_scheme"), srcOrigins, false)
		}

		if provider.ChatCompletionsPath != "" {
			current.ChatCompletionsPath = strings.TrimSpace(provider.ChatCompletionsPath)

			appendOriginChain(dstOrigins, providerFieldPath(name, "chat_completions_path"), srcOrigins, false)
		}

		if provider.EmbeddingsPath != "" {
			current.EmbeddingsPath = strings.TrimSpace(provider.EmbeddingsPath)

			appendOriginChain(dstOrigins, providerFieldPath(name, "embeddings_path"), srcOrigins, false)
		}

		if provider.ModelsPath != "" {
			current.ModelsPath = strings.TrimSpace(provider.ModelsPath)

			appendOriginChain(dstOrigins, providerFieldPath(name, "models_path"), srcOrigins, false)
		}

		if provider.APIVersion != "" {
			current.APIVersion = strings.TrimSpace(provider.APIVersion)

			appendOriginChain(dstOrigins, providerFieldPath(name, "api_version"), srcOrigins, false)
		}

		if provider.Models != nil {
			current.Models = append([]string(nil), provider.Models...)

			appendOriginChain(dstOrigins, providerFieldPath(name, "models"), srcOrigins, true)
		}

		if provider.Capabilities != nil {
			current.Capabilities = append([]string(nil), provider.Capabilities...)

			appendOriginChain(dstOrigins, providerFieldPath(name, "capabilities"), srcOrigins, true)
		}

		disabledPath := providerFieldPath(name, "disabled")
		if originPathExists(srcOrigins, disabledPath) {
			current.Disabled = provider.Disabled

			appendOriginChain(dstOrigins, disabledPath, srcOrigins, false)
		}

		localPath := providerFieldPath(name, "local")
		if originPathExists(srcOrigins, localPath) {
			current.Local = provider.Local

			appendOriginChain(dstOrigins, localPath, srcOrigins, false)
		}

		disablePrivateAdapterPath := providerFieldPath(name, "disable_private_adapter")
		if originPathExists(srcOrigins, disablePrivateAdapterPath) {
			current.DisablePrivateAdapter = provider.DisablePrivateAdapter

			appendOriginChain(dstOrigins, disablePrivateAdapterPath, srcOrigins, false)
		}

		current.AutoStart = provider.AutoStart

		appendOriginChain(dstOrigins, providerFieldPath(name, "auto_start"), srcOrigins, false)

		if provider.TimeoutSeconds > 0 {
			current.TimeoutSeconds = provider.TimeoutSeconds

			appendOriginChain(dstOrigins, providerFieldPath(name, "timeout_seconds"), srcOrigins, false)
		}

		if provider.Retry.hasFields() {
			current.Retry = mergeRetryConfigFromOrigins(
				current.Retry,
				provider.Retry,
				dstOrigins,
				srcOrigins,
				providerFieldPath(name, "retry"),
			)
		}

		dst.Providers[name] = current
	}
}

func originPathExists(origins OriginMap, path string) bool {
	_, ok := origins[path]

	return ok
}

func mergeRetryConfigFromOrigins(
	current RetryConfig,
	provider RetryConfig,
	dstOrigins OriginMap,
	srcOrigins OriginMap,
	path string,
) RetryConfig {
	appendOriginChain(dstOrigins, path, srcOrigins, false)

	if provider.MaxAttempts != nil {
		current.MaxAttempts = provider.MaxAttempts

		appendOriginChain(dstOrigins, dottedPath(path, "max_attempts"), srcOrigins, false)
	}

	if provider.InitialBackoffMS != nil {
		current.InitialBackoffMS = provider.InitialBackoffMS

		appendOriginChain(dstOrigins, dottedPath(path, "initial_backoff_ms"), srcOrigins, false)
	}

	if provider.MaxBackoffMS != nil {
		current.MaxBackoffMS = provider.MaxBackoffMS

		appendOriginChain(dstOrigins, dottedPath(path, "max_backoff_ms"), srcOrigins, false)
	}

	if provider.MaxElapsedMS != nil {
		current.MaxElapsedMS = provider.MaxElapsedMS

		appendOriginChain(dstOrigins, dottedPath(path, "max_elapsed_ms"), srcOrigins, false)
	}

	if provider.JitterFraction != nil {
		current.JitterFraction = provider.JitterFraction

		appendOriginChain(dstOrigins, dottedPath(path, "jitter_fraction"), srcOrigins, false)
	}

	return current
}

func firstExistingOriginPath(origins OriginMap, paths ...string) string {
	for _, path := range paths {
		if originPathExists(origins, path) {
			return path
		}
	}

	return ""
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

	if agent.ModelMode != "" {
		current.ModelMode = strings.TrimSpace(agent.ModelMode)

		appendOriginChain(dstOrigins, agentFieldPath(name, "model_mode"), srcOrigins, false)
	}

	if agent.ToolPermissions != nil {
		current.ToolPermissions = make(map[string]bool, len(agent.ToolPermissions))
		maps.Copy(current.ToolPermissions, agent.ToolPermissions)

		appendOriginChain(dstOrigins, agentFieldPath(name, "tools"), srcOrigins, true)
	}

	if routingPolicyConfigured(agent.RoutingPolicy) {
		current.RoutingPolicy = cloneRoutingPolicy(agent.RoutingPolicy)

		appendOriginChain(dstOrigins, agentFieldPath(name, "routing_policy"), srcOrigins, true)
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

	if agent.FeedbackGuidance != nil {
		current.FeedbackGuidance = cloneFeedbackGuidance(agent.FeedbackGuidance)

		appendOriginChain(dstOrigins, agentFieldPath(name, "feedback_guidance"), srcOrigins, true)
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

	mergeConfigReferencePolicyFromOrigins(&dst.Context.ReferencePolicy, contextConfig.ReferencePolicy, dstOrigins, srcOrigins)
}

func mergeConfigReferencePolicyFromOrigins(dst *ReferencePolicyConfig, policy ReferencePolicyConfig, dstOrigins, srcOrigins OriginMap) {
	mergeConfigReferencePolicyListsFromOrigins(dst, policy, dstOrigins, srcOrigins)
	mergeConfigReferencePolicyLimitsFromOrigins(dst, policy, dstOrigins, srcOrigins)
}

func mergeConfigReferencePolicyListsFromOrigins(dst *ReferencePolicyConfig, policy ReferencePolicyConfig, dstOrigins, srcOrigins OriginMap) {
	mergePolicyListFromOrigins(&dst.AllowedSchemes, policy.AllowedSchemes, "allowed_schemes", dstOrigins, srcOrigins)
	mergePolicyListFromOrigins(&dst.DeniedSchemes, policy.DeniedSchemes, "denied_schemes", dstOrigins, srcOrigins)
	mergePolicyListFromOrigins(&dst.AllowedHosts, policy.AllowedHosts, "allowed_hosts", dstOrigins, srcOrigins)
	mergePolicyListFromOrigins(&dst.DeniedHosts, policy.DeniedHosts, "denied_hosts", dstOrigins, srcOrigins)
	mergePolicyListFromOrigins(&dst.AllowedPorts, policy.AllowedPorts, "allowed_ports", dstOrigins, srcOrigins)
	mergePolicyListFromOrigins(&dst.DeniedPorts, policy.DeniedPorts, "denied_ports", dstOrigins, srcOrigins)
	mergePolicyListFromOrigins(&dst.LocalRoots, policy.LocalRoots, "local_roots", dstOrigins, srcOrigins)
	mergePolicyListFromOrigins(&dst.DeniedLocalRoots, policy.DeniedLocalRoots, "denied_local_roots", dstOrigins, srcOrigins)
	mergePolicyListFromOrigins(&dst.AllowedGlobs, policy.AllowedGlobs, "allowed_globs", dstOrigins, srcOrigins)
	mergePolicyListFromOrigins(&dst.DeniedGlobs, policy.DeniedGlobs, "denied_globs", dstOrigins, srcOrigins)
	mergePolicyListFromOrigins(&dst.ContentTypes, policy.ContentTypes, "content_types", dstOrigins, srcOrigins)
}

func mergePolicyListFromOrigins[T any](dst *[]T, policy []T, field string, dstOrigins, srcOrigins OriginMap) {
	path := referencePolicyFieldPath(field)
	if policy == nil && !originPresent(srcOrigins, path) {
		return
	}

	*dst = cloneSlicePreserveEmpty(policy)

	appendOriginChain(dstOrigins, path, srcOrigins, true)
}

func mergeConfigReferencePolicyLimitsFromOrigins(dst *ReferencePolicyConfig, policy ReferencePolicyConfig, dstOrigins, srcOrigins OriginMap) {
	if policy.MaxRedirects > 0 || originPresent(srcOrigins, referencePolicyFieldPath("max_redirects")) {
		dst.MaxRedirects = policy.MaxRedirects

		appendOriginChain(dstOrigins, referencePolicyFieldPath("max_redirects"), srcOrigins, false)
	}

	if policy.MaxFiles > 0 || originPresent(srcOrigins, referencePolicyFieldPath("max_files")) {
		dst.MaxFiles = policy.MaxFiles

		appendOriginChain(dstOrigins, referencePolicyFieldPath("max_files"), srcOrigins, false)
	}

	if policy.AllowAbsolutePaths || originPresent(srcOrigins, referencePolicyFieldPath("allow_absolute_paths")) {
		dst.AllowAbsolutePaths = policy.AllowAbsolutePaths

		appendOriginChain(dstOrigins, referencePolicyFieldPath("allow_absolute_paths"), srcOrigins, false)
	}

	if policy.AllowPrivateNetworks || originPresent(srcOrigins, referencePolicyFieldPath("allow_private_networks")) {
		dst.AllowPrivateNetworks = policy.AllowPrivateNetworks

		appendOriginChain(dstOrigins, referencePolicyFieldPath("allow_private_networks"), srcOrigins, false)
	}
}

func originPresent(origins OriginMap, path string) bool {
	if origins == nil {
		return false
	}

	origin, ok := origins[path]

	return ok && len(origin.Chain) > 0
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

	if generation.ModelMode != "" {
		dst.Generation.ModelMode = strings.TrimSpace(generation.ModelMode)

		appendOriginChain(dstOrigins, "generation.model_mode", srcOrigins, false)
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

func mergeConfigResearchFromOrigins(dst *Config, research ResearchConfig, dstOrigins, srcOrigins OriginMap) {
	mergeConfigSourcePolicyFromOrigins(&dst.Research.SourcePolicy, research.SourcePolicy, dstOrigins, srcOrigins)
}

func mergeConfigSourcePolicyFromOrigins(dst *sourcepolicy.Policy, policy sourcepolicy.Policy, dstOrigins, srcOrigins OriginMap) {
	mergeSourcePolicyStringListFromOrigins(&dst.TrustedDomains, policy.TrustedDomains, "trusted_domains", dstOrigins, srcOrigins)
	mergeSourcePolicyStringListFromOrigins(&dst.DeniedDomains, policy.DeniedDomains, "denied_domains", dstOrigins, srcOrigins)
	mergeSourcePolicyStringListFromOrigins(&dst.PreferSourceTypes, policy.PreferSourceTypes, "prefer_source_types", dstOrigins, srcOrigins)
	mergeSourcePolicyBoolFromOrigins(&dst.AllowLowTrustSources, policy.AllowLowTrustSources, "allow_low_trust_sources", dstOrigins, srcOrigins)
	mergeSourcePolicyBoolFromOrigins(&dst.WarnOnLowTrustSources, policy.WarnOnLowTrustSources, "warn_on_low_trust_sources", dstOrigins, srcOrigins)
	mergeSourcePolicyBoolFromOrigins(&dst.RequireEvidenceForHighImpactClaims, policy.RequireEvidenceForHighImpactClaims, "require_evidence_for_high_impact_claims", dstOrigins, srcOrigins)
}

func mergeSourcePolicyStringListFromOrigins(dst *[]string, policy []string, field string, dstOrigins, srcOrigins OriginMap) {
	path := sourcePolicyFieldPath(field)
	if policy == nil && !originPresent(srcOrigins, path) {
		return
	}

	switch field {
	case "trusted_domains", "denied_domains":
		*dst = sourcepolicy.NormalizeDomains(policy)
	case "prefer_source_types":
		*dst = sourcepolicy.NormalizeSourceTypes(policy)
	default:
		*dst = cloneSlicePreserveEmpty(policy)
	}

	appendOriginChain(dstOrigins, path, srcOrigins, true)
}

func mergeSourcePolicyBoolFromOrigins(dst **bool, policy *bool, field string, dstOrigins, srcOrigins OriginMap) {
	path := sourcePolicyFieldPath(field)
	if policy == nil && !originPresent(srcOrigins, path) {
		return
	}

	*dst = cloneBoolPointer(policy)

	appendOriginChain(dstOrigins, path, srcOrigins, false)
}

func mergeConfigAgentLoopFromOrigins(dst *Config, agentLoop AgentLoopConfig, dstOrigins, srcOrigins OriginMap) {
	if agentLoop.MaxOutputBytes != nil {
		value := *agentLoop.MaxOutputBytes
		dst.AgentLoop.MaxOutputBytes = &value

		appendOriginChain(dstOrigins, "agent_loop.max_output_bytes", srcOrigins, false)
	}

	if agentLoop.MaxCostMicros != nil {
		value := *agentLoop.MaxCostMicros
		dst.AgentLoop.MaxCostMicros = &value

		appendOriginChain(dstOrigins, "agent_loop.max_cost_micros", srcOrigins, false)
	}

	if agentLoop.MaxInputTokens != nil {
		value := *agentLoop.MaxInputTokens
		dst.AgentLoop.MaxInputTokens = &value

		appendOriginChain(dstOrigins, "agent_loop.max_input_tokens", srcOrigins, false)
	}

	if agentLoop.MaxOutputTokens != nil {
		value := *agentLoop.MaxOutputTokens
		dst.AgentLoop.MaxOutputTokens = &value

		appendOriginChain(dstOrigins, "agent_loop.max_output_tokens", srcOrigins, false)
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

	if agentLoop.MaxModelCalls != nil {
		value := *agentLoop.MaxModelCalls
		dst.AgentLoop.MaxModelCalls = &value

		appendOriginChain(dstOrigins, "agent_loop.max_model_calls", srcOrigins, false)
	}

	if agentLoop.MaxToolCalls != nil {
		value := *agentLoop.MaxToolCalls
		dst.AgentLoop.MaxToolCalls = &value

		appendOriginChain(dstOrigins, "agent_loop.max_tool_calls", srcOrigins, false)
	}

	if agentLoop.MaxWallTime != nil {
		value := *agentLoop.MaxWallTime
		dst.AgentLoop.MaxWallTime = &value

		appendOriginChain(dstOrigins, "agent_loop.max_wall_time", srcOrigins, false)
	}

	if agentLoop.CheckpointInterval != nil {
		value := *agentLoop.CheckpointInterval
		dst.AgentLoop.CheckpointInterval = &value

		appendOriginChain(dstOrigins, "agent_loop.checkpoint_interval", srcOrigins, false)
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

		appendOriginChain(dstOrigins, "plugins.policy", srcOrigins, true)
	}
}

func mergeConfigSkillLearningFromOrigins(dst *Config, skillLearning SkillLearningConfig, dstOrigins, srcOrigins OriginMap) {
	if skillLearning.Enabled != nil {
		value := *skillLearning.Enabled
		dst.SkillLearning.Enabled = &value

		appendOriginChain(dstOrigins, "skill_learning.enabled", srcOrigins, false)
	}

	if skillLearning.StoreDir != "" {
		dst.SkillLearning.StoreDir = skillLearning.StoreDir

		appendOriginChain(dstOrigins, "skill_learning.store_dir", srcOrigins, false)
	}

	if skillLearning.SkillDir != "" {
		dst.SkillLearning.SkillDir = skillLearning.SkillDir

		appendOriginChain(dstOrigins, "skill_learning.skill_dir", srcOrigins, false)
	}

	if skillLearning.MaxObservations > 0 {
		dst.SkillLearning.MaxObservations = skillLearning.MaxObservations

		appendOriginChain(dstOrigins, "skill_learning.max_observations", srcOrigins, false)
	}

	if skillLearning.MaxSteps > 0 {
		dst.SkillLearning.MaxSteps = skillLearning.MaxSteps

		appendOriginChain(dstOrigins, "skill_learning.max_steps", srcOrigins, false)
	}

	if skillLearning.MinOccurrences > 0 {
		dst.SkillLearning.MinOccurrences = skillLearning.MinOccurrences

		appendOriginChain(dstOrigins, "skill_learning.min_occurrences", srcOrigins, false)
	}
}

//nolint:cyclop // Merge functions intentionally mirror each schema field for origin tracking.
func mergeConfigVectorFromOrigins(dst *Config, vector VectorConfig, dstOrigins, srcOrigins OriginMap) {
	if vector.WorkspaceEnabled != nil {
		value := *vector.WorkspaceEnabled
		dst.Vector.WorkspaceEnabled = &value

		appendOriginChain(dstOrigins, "vector.workspace_enabled", srcOrigins, false)
	}

	if vector.WorkspaceAllowRemoteEmbeddings != nil {
		value := *vector.WorkspaceAllowRemoteEmbeddings
		dst.Vector.WorkspaceAllowRemoteEmbeddings = &value

		appendOriginChain(dstOrigins, "vector.workspace_allow_remote_embeddings", srcOrigins, false)
	}

	dst.Vector.Stores = mergeConfigVectorizerConfigMapFromOrigins(dst.Vector.Stores, "stores", vector.Stores, dstOrigins, srcOrigins)
	dst.Vector.Agents = mergeConfigVectorizerConfigMapFromOrigins(dst.Vector.Agents, "agents", vector.Agents, dstOrigins, srcOrigins)
	dst.Vector.Sources = mergeConfigVectorizerConfigMapFromOrigins(dst.Vector.Sources, "sources", vector.Sources, dstOrigins, srcOrigins)

	if vector.Vectorizer != "" {
		dst.Vector.Vectorizer = strings.TrimSpace(vector.Vectorizer)

		appendOriginChain(dstOrigins, "vector.vectorizer", srcOrigins, false)
	}

	if vector.Provider != "" {
		dst.Vector.Provider = strings.TrimSpace(vector.Provider)

		appendOriginChain(dstOrigins, "vector.provider", srcOrigins, false)
	}

	if vector.Model != "" {
		dst.Vector.Model = strings.TrimSpace(vector.Model)

		appendOriginChain(dstOrigins, "vector.model", srcOrigins, false)
	}

	if vector.BaseURL != "" {
		dst.Vector.BaseURL = strings.TrimSpace(vector.BaseURL)

		appendOriginChain(dstOrigins, "vector.base_url", srcOrigins, false)
	}

	if vector.FallbackPolicy != "" {
		dst.Vector.FallbackPolicy = strings.TrimSpace(vector.FallbackPolicy)

		appendOriginChain(dstOrigins, "vector.fallback_policy", srcOrigins, false)
	}

	if vector.IndexPath != "" {
		dst.Vector.IndexPath = strings.TrimSpace(vector.IndexPath)

		appendOriginChain(dstOrigins, "vector.index_path", srcOrigins, false)
	}

	if vector.WorkspaceIndexPath != "" {
		dst.Vector.WorkspaceIndexPath = strings.TrimSpace(vector.WorkspaceIndexPath)

		appendOriginChain(dstOrigins, "vector.workspace_index_path", srcOrigins, false)
	}

	if vector.WorkspaceInclude != nil {
		dst.Vector.WorkspaceInclude = append([]string(nil), vector.WorkspaceInclude...)

		appendOriginChain(dstOrigins, "vector.workspace_include", srcOrigins, true)
	}

	if vector.WorkspaceExclude != nil {
		dst.Vector.WorkspaceExclude = append([]string(nil), vector.WorkspaceExclude...)

		appendOriginChain(dstOrigins, "vector.workspace_exclude", srcOrigins, true)
	}

	if vector.TimeoutSeconds > 0 {
		dst.Vector.TimeoutSeconds = vector.TimeoutSeconds

		appendOriginChain(dstOrigins, "vector.timeout_seconds", srcOrigins, false)
	}

	if vector.ChunkMaxRunes > 0 {
		dst.Vector.ChunkMaxRunes = vector.ChunkMaxRunes

		appendOriginChain(dstOrigins, "vector.chunk_max_runes", srcOrigins, false)
	}

	if vector.ChunkOverlapRunes > 0 {
		dst.Vector.ChunkOverlapRunes = vector.ChunkOverlapRunes

		appendOriginChain(dstOrigins, "vector.chunk_overlap_runes", srcOrigins, false)
	}

	if vector.WorkspaceLimit > 0 {
		dst.Vector.WorkspaceLimit = vector.WorkspaceLimit

		appendOriginChain(dstOrigins, "vector.workspace_limit", srcOrigins, false)
	}

	if vector.WorkspaceMaxFileBytes > 0 {
		dst.Vector.WorkspaceMaxFileBytes = vector.WorkspaceMaxFileBytes

		appendOriginChain(dstOrigins, "vector.workspace_max_file_bytes", srcOrigins, false)
	}

	if vector.WorkspaceMaxFiles > 0 {
		dst.Vector.WorkspaceMaxFiles = vector.WorkspaceMaxFiles

		appendOriginChain(dstOrigins, "vector.workspace_max_files", srcOrigins, false)
	}
}

func mergeConfigWorktreeFromOrigins(dst *Config, worktree WorktreeConfig, dstOrigins, srcOrigins OriginMap) {
	if worktree.AutoMerge != nil {
		value := *worktree.AutoMerge
		dst.Worktree.AutoMerge = &value

		appendOriginChain(dstOrigins, "worktree.auto_merge", srcOrigins, false)
	}

	if worktree.VerificationCommands != nil {
		dst.Worktree.VerificationCommands = append([]string(nil), worktree.VerificationCommands...)

		appendOriginChain(dstOrigins, "worktree.verification_commands", srcOrigins, true)
	}

	if originPathExists(srcOrigins, "worktree.override_verification") {
		dst.Worktree.OverrideVerification = worktree.OverrideVerification

		appendOriginChain(dstOrigins, "worktree.override_verification", srcOrigins, false)
	}
}

//nolint:cyclop // Merge functions intentionally mirror each schema field for origin tracking.
func mergeConfigVector(dst *Config, vector VectorConfig, rec *originRecorder, source originSource) {
	if vector.WorkspaceEnabled != nil {
		value := *vector.WorkspaceEnabled
		dst.Vector.WorkspaceEnabled = &value
		rec.set("vector.workspace_enabled", source, value)
	}

	if vector.WorkspaceAllowRemoteEmbeddings != nil {
		value := *vector.WorkspaceAllowRemoteEmbeddings
		dst.Vector.WorkspaceAllowRemoteEmbeddings = &value
		rec.set("vector.workspace_allow_remote_embeddings", source, value)
	}

	dst.Vector.Stores = mergeConfigVectorizerConfigMap(dst.Vector.Stores, "stores", vector.Stores, rec, source)
	dst.Vector.Agents = mergeConfigVectorizerConfigMap(dst.Vector.Agents, "agents", vector.Agents, rec, source)
	dst.Vector.Sources = mergeConfigVectorizerConfigMap(dst.Vector.Sources, "sources", vector.Sources, rec, source)

	if vector.Vectorizer != "" {
		value := strings.TrimSpace(vector.Vectorizer)
		dst.Vector.Vectorizer = value
		rec.set("vector.vectorizer", source, value)
	}

	if vector.Provider != "" {
		value := strings.TrimSpace(vector.Provider)
		dst.Vector.Provider = value
		rec.set("vector.provider", source, value)
	}

	if vector.Model != "" {
		value := strings.TrimSpace(vector.Model)
		dst.Vector.Model = value
		rec.set("vector.model", source, value)
	}

	if vector.BaseURL != "" {
		value := strings.TrimSpace(vector.BaseURL)
		dst.Vector.BaseURL = value
		rec.set("vector.base_url", source, value)
	}

	if vector.FallbackPolicy != "" {
		value := strings.TrimSpace(vector.FallbackPolicy)
		dst.Vector.FallbackPolicy = value
		rec.set("vector.fallback_policy", source, value)
	}

	if vector.IndexPath != "" {
		value := strings.TrimSpace(vector.IndexPath)
		dst.Vector.IndexPath = value
		rec.set("vector.index_path", source, value)
	}

	if vector.WorkspaceIndexPath != "" {
		value := strings.TrimSpace(vector.WorkspaceIndexPath)
		dst.Vector.WorkspaceIndexPath = value
		rec.set("vector.workspace_index_path", source, value)
	}

	if vector.WorkspaceInclude != nil {
		dst.Vector.WorkspaceInclude = append([]string(nil), vector.WorkspaceInclude...)
		rec.replace("vector.workspace_include", source, dst.Vector.WorkspaceInclude, "replaces the workspace vector include pattern list")
	}

	if vector.WorkspaceExclude != nil {
		dst.Vector.WorkspaceExclude = append([]string(nil), vector.WorkspaceExclude...)
		rec.replace("vector.workspace_exclude", source, dst.Vector.WorkspaceExclude, "replaces the workspace vector exclude pattern list")
	}

	if vector.TimeoutSeconds > 0 {
		dst.Vector.TimeoutSeconds = vector.TimeoutSeconds
		rec.set("vector.timeout_seconds", source, vector.TimeoutSeconds)
	}

	if vector.ChunkMaxRunes > 0 {
		dst.Vector.ChunkMaxRunes = vector.ChunkMaxRunes
		rec.set("vector.chunk_max_runes", source, vector.ChunkMaxRunes)
	}

	if vector.ChunkOverlapRunes > 0 {
		dst.Vector.ChunkOverlapRunes = vector.ChunkOverlapRunes
		rec.set("vector.chunk_overlap_runes", source, vector.ChunkOverlapRunes)
	}

	if vector.WorkspaceLimit > 0 {
		dst.Vector.WorkspaceLimit = vector.WorkspaceLimit
		rec.set("vector.workspace_limit", source, vector.WorkspaceLimit)
	}

	if vector.WorkspaceMaxFileBytes > 0 {
		dst.Vector.WorkspaceMaxFileBytes = vector.WorkspaceMaxFileBytes
		rec.set("vector.workspace_max_file_bytes", source, vector.WorkspaceMaxFileBytes)
	}

	if vector.WorkspaceMaxFiles > 0 {
		dst.Vector.WorkspaceMaxFiles = vector.WorkspaceMaxFiles
		rec.set("vector.workspace_max_files", source, vector.WorkspaceMaxFiles)
	}
}

func mergeConfigWorktree(dst *Config, worktree WorktreeConfig, rec *originRecorder, source originSource) {
	if worktree.AutoMerge != nil {
		value := *worktree.AutoMerge
		dst.Worktree.AutoMerge = &value
		rec.set("worktree.auto_merge", source, value)
	}

	if worktree.VerificationCommands != nil {
		dst.Worktree.VerificationCommands = append([]string(nil), worktree.VerificationCommands...)
		rec.replace("worktree.verification_commands", source, dst.Worktree.VerificationCommands, "replaces the entire worktree verification command list")
	}

	if worktree.OverrideVerification {
		dst.Worktree.OverrideVerification = true

		rec.set("worktree.override_verification", source, true)
	}
}

func mergeFileVectorizerConfigMap(
	dst map[string]VectorizerConfig,
	scopeName string,
	scopes map[string]fileVectorizerConfig,
	rec *originRecorder,
	source originSource,
) map[string]VectorizerConfig {
	if scopes == nil {
		return dst
	}

	if dst == nil {
		dst = make(map[string]VectorizerConfig, len(scopes))
	}

	rec.merge(vectorScopeGroupFieldPath(scopeName), source, sortedMapKeys(scopes), "merges vectorizer configs by scope name")

	for name, scoped := range scopes {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}

		current := dst[name]
		fieldPath := vectorScopeFieldPath(scopeName, name)
		rec.merge(fieldPath, source, name, "merges vectorizer config fields by scope name")
		mergeFileVectorizerConfig(&current, scoped, rec, source, fieldPath)
		dst[name] = current
	}

	return dst
}

func mergeFileVectorizerConfig(
	dst *VectorizerConfig,
	src fileVectorizerConfig,
	rec *originRecorder,
	source originSource,
	fieldPath string,
) {
	if src.Vectorizer != nil {
		value := strings.TrimSpace(*src.Vectorizer)
		dst.Vectorizer = value
		rec.set(dottedPath(fieldPath, "vectorizer"), source, value)
	}

	if src.Provider != nil {
		value := strings.TrimSpace(*src.Provider)
		dst.Provider = value
		rec.set(dottedPath(fieldPath, "provider"), source, value)
	}

	if src.Model != nil {
		value := strings.TrimSpace(*src.Model)
		dst.Model = value
		rec.set(dottedPath(fieldPath, "model"), source, value)
	}

	if src.BaseURL != nil {
		value := strings.TrimSpace(*src.BaseURL)
		dst.BaseURL = value
		rec.set(dottedPath(fieldPath, "base_url"), source, value)
	}

	if src.FallbackPolicy != nil {
		value := strings.TrimSpace(*src.FallbackPolicy)
		dst.FallbackPolicy = value
		rec.set(dottedPath(fieldPath, "fallback_policy"), source, value)
	}

	if src.IndexPath != nil {
		value := strings.TrimSpace(*src.IndexPath)
		dst.IndexPath = value
		rec.set(dottedPath(fieldPath, "index_path"), source, value)
	}

	if src.TimeoutSeconds != nil {
		dst.TimeoutSeconds = *src.TimeoutSeconds
		rec.set(dottedPath(fieldPath, "timeout_seconds"), source, *src.TimeoutSeconds)
	}

	if src.ChunkMaxRunes != nil {
		dst.ChunkMaxRunes = *src.ChunkMaxRunes
		rec.set(dottedPath(fieldPath, "chunk_max_runes"), source, *src.ChunkMaxRunes)
	}

	if src.ChunkOverlapRunes != nil {
		dst.ChunkOverlapRunes = *src.ChunkOverlapRunes
		rec.set(dottedPath(fieldPath, "chunk_overlap_runes"), source, *src.ChunkOverlapRunes)
	}
}

func mergeConfigVectorizerConfigMap(
	dst map[string]VectorizerConfig,
	scopeName string,
	scopes map[string]VectorizerConfig,
	rec *originRecorder,
	source originSource,
) map[string]VectorizerConfig {
	if scopes == nil {
		return dst
	}

	if dst == nil {
		dst = make(map[string]VectorizerConfig, len(scopes))
	}

	rec.merge(vectorScopeGroupFieldPath(scopeName), source, sortedMapKeys(scopes), "merges vectorizer configs by scope name")

	for name, scoped := range scopes {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}

		current := dst[name]
		fieldPath := vectorScopeFieldPath(scopeName, name)
		rec.merge(fieldPath, source, name, "merges vectorizer config fields by scope name")
		mergeConfigVectorizerConfig(&current, scoped, rec, source, fieldPath)
		dst[name] = current
	}

	return dst
}

func mergeConfigVectorizerConfig(
	dst *VectorizerConfig,
	src VectorizerConfig,
	rec *originRecorder,
	source originSource,
	fieldPath string,
) {
	if src.Vectorizer != "" {
		value := strings.TrimSpace(src.Vectorizer)
		dst.Vectorizer = value
		rec.set(dottedPath(fieldPath, "vectorizer"), source, value)
	}

	if src.Provider != "" {
		value := strings.TrimSpace(src.Provider)
		dst.Provider = value
		rec.set(dottedPath(fieldPath, "provider"), source, value)
	}

	if src.Model != "" {
		value := strings.TrimSpace(src.Model)
		dst.Model = value
		rec.set(dottedPath(fieldPath, "model"), source, value)
	}

	if src.BaseURL != "" {
		value := strings.TrimSpace(src.BaseURL)
		dst.BaseURL = value
		rec.set(dottedPath(fieldPath, "base_url"), source, value)
	}

	if src.FallbackPolicy != "" {
		value := strings.TrimSpace(src.FallbackPolicy)
		dst.FallbackPolicy = value
		rec.set(dottedPath(fieldPath, "fallback_policy"), source, value)
	}

	if src.IndexPath != "" {
		value := strings.TrimSpace(src.IndexPath)
		dst.IndexPath = value
		rec.set(dottedPath(fieldPath, "index_path"), source, value)
	}

	if src.TimeoutSeconds > 0 {
		dst.TimeoutSeconds = src.TimeoutSeconds
		rec.set(dottedPath(fieldPath, "timeout_seconds"), source, src.TimeoutSeconds)
	}

	if src.ChunkMaxRunes > 0 {
		dst.ChunkMaxRunes = src.ChunkMaxRunes
		rec.set(dottedPath(fieldPath, "chunk_max_runes"), source, src.ChunkMaxRunes)
	}

	if src.ChunkOverlapRunes > 0 {
		dst.ChunkOverlapRunes = src.ChunkOverlapRunes
		rec.set(dottedPath(fieldPath, "chunk_overlap_runes"), source, src.ChunkOverlapRunes)
	}
}

func mergeConfigVectorizerConfigMapFromOrigins(
	dst map[string]VectorizerConfig,
	scopeName string,
	scopes map[string]VectorizerConfig,
	dstOrigins, srcOrigins OriginMap,
) map[string]VectorizerConfig {
	if scopes == nil {
		return dst
	}

	if dst == nil {
		dst = make(map[string]VectorizerConfig, len(scopes))
	}

	appendOriginChain(dstOrigins, vectorScopeGroupFieldPath(scopeName), srcOrigins, false)

	for name, scoped := range scopes {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}

		fieldPath := vectorScopeFieldPath(scopeName, name)
		current := dst[name]

		appendOriginChain(dstOrigins, fieldPath, srcOrigins, false)
		mergeConfigVectorizerConfigFromOrigins(&current, scoped, fieldPath, dstOrigins, srcOrigins)
		dst[name] = current
	}

	return dst
}

func mergeConfigVectorizerConfigFromOrigins(
	dst *VectorizerConfig,
	src VectorizerConfig,
	fieldPath string,
	dstOrigins, srcOrigins OriginMap,
) {
	mergeConfigVectorizerStringFromOrigins(&dst.Vectorizer, src.Vectorizer, fieldPath, "vectorizer", dstOrigins, srcOrigins)
	mergeConfigVectorizerStringFromOrigins(&dst.Provider, src.Provider, fieldPath, "provider", dstOrigins, srcOrigins)
	mergeConfigVectorizerStringFromOrigins(&dst.Model, src.Model, fieldPath, "model", dstOrigins, srcOrigins)
	mergeConfigVectorizerStringFromOrigins(&dst.BaseURL, src.BaseURL, fieldPath, "base_url", dstOrigins, srcOrigins)
	mergeConfigVectorizerStringFromOrigins(&dst.FallbackPolicy, src.FallbackPolicy, fieldPath, "fallback_policy", dstOrigins, srcOrigins)
	mergeConfigVectorizerStringFromOrigins(&dst.IndexPath, src.IndexPath, fieldPath, "index_path", dstOrigins, srcOrigins)
	mergeConfigVectorizerIntFromOrigins(&dst.TimeoutSeconds, src.TimeoutSeconds, fieldPath, "timeout_seconds", dstOrigins, srcOrigins)
	mergeConfigVectorizerIntFromOrigins(&dst.ChunkMaxRunes, src.ChunkMaxRunes, fieldPath, "chunk_max_runes", dstOrigins, srcOrigins)
	mergeConfigVectorizerIntFromOrigins(&dst.ChunkOverlapRunes, src.ChunkOverlapRunes, fieldPath, "chunk_overlap_runes", dstOrigins, srcOrigins)
}

func mergeConfigVectorizerStringFromOrigins(
	dst *string,
	value string,
	fieldPath, name string,
	dstOrigins, srcOrigins OriginMap,
) {
	path := dottedPath(fieldPath, name)
	if !originPathExists(srcOrigins, path) && strings.TrimSpace(value) == "" {
		return
	}

	*dst = strings.TrimSpace(value)

	appendOriginChain(dstOrigins, path, srcOrigins, false)
}

func mergeConfigVectorizerIntFromOrigins(
	dst *int,
	value int,
	fieldPath, name string,
	dstOrigins, srcOrigins OriginMap,
) {
	path := dottedPath(fieldPath, name)
	if !originPathExists(srcOrigins, path) && value == 0 {
		return
	}

	*dst = value

	appendOriginChain(dstOrigins, path, srcOrigins, false)
}

func vectorizerScopeConfig(scopes map[string]VectorizerConfig, key string) (VectorizerConfig, bool) {
	key = strings.TrimSpace(key)
	if key == "" || len(scopes) == 0 {
		return VectorizerConfig{}, false
	}

	if scoped, ok := scopes[key]; ok {
		return scoped, true
	}

	lowerKey := strings.ToLower(key)
	for name, scoped := range scopes {
		if strings.ToLower(strings.TrimSpace(name)) == lowerKey {
			return scoped, true
		}
	}

	normalizedKey := normalizeVectorizerScopeKey(key)
	for name, scoped := range scopes {
		if normalizeVectorizerScopeKey(name) == normalizedKey {
			return scoped, true
		}
	}

	return VectorizerConfig{}, false
}

func normalizeVectorizerScopeKey(key string) string {
	key = strings.ToLower(strings.TrimSpace(key))
	key = strings.ReplaceAll(key, "_", "-")
	key = strings.ReplaceAll(key, " ", "-")

	return key
}

func overlayVectorizerConfig(base, override VectorizerConfig) VectorizerConfig {
	if value := strings.TrimSpace(override.Vectorizer); value != "" {
		base.Vectorizer = value
	}

	if value := strings.TrimSpace(override.Provider); value != "" {
		base.Provider = value
	}

	if value := strings.TrimSpace(override.Model); value != "" {
		base.Model = value
	}

	if value := strings.TrimSpace(override.BaseURL); value != "" {
		base.BaseURL = value
	}

	if value := strings.TrimSpace(override.FallbackPolicy); value != "" {
		base.FallbackPolicy = value
	}

	if value := strings.TrimSpace(override.IndexPath); value != "" {
		base.IndexPath = value
	}

	if override.TimeoutSeconds > 0 {
		base.TimeoutSeconds = override.TimeoutSeconds
	}

	if override.ChunkMaxRunes > 0 {
		base.ChunkMaxRunes = override.ChunkMaxRunes
	}

	if override.ChunkOverlapRunes > 0 {
		base.ChunkOverlapRunes = override.ChunkOverlapRunes
	}

	return base
}

func vectorScopeGroupFieldPath(scopeName string) string {
	return dottedPath("vector", scopeName)
}

func vectorScopeFieldPath(scopeName, name string, fields ...string) string {
	return dottedPath(append([]string{"vector", scopeName, name}, fields...)...)
}

func cloneHooks(hooks []HookConfig) []HookConfig {
	out := make([]HookConfig, 0, len(hooks))
	for _, hook := range hooks {
		out = append(out, HookConfig{
			Command:            append([]string(nil), hook.Command...),
			Env:                cloneMap(hook.Env),
			Payload:            hook.Payload,
			TimeoutSeconds:     hook.TimeoutSeconds,
			MaxAttempts:        hook.MaxAttempts,
			RetryBackoffMillis: hook.RetryBackoffMillis,
			InheritEnv:         hook.InheritEnv,
			Blocking:           hook.Blocking,
		})
	}

	return out
}

func cloneSlicePreserveEmpty[T any](in []T) []T {
	if in == nil {
		return nil
	}

	out := make([]T, len(in))
	copy(out, in)

	return out
}

func cloneBoolPointer(in *bool) *bool {
	if in == nil {
		return nil
	}

	out := *in

	return &out
}

func cloneFeedbackGuidance(records []FeedbackGuidance) []FeedbackGuidance {
	if len(records) == 0 {
		return nil
	}

	out := make([]FeedbackGuidance, 0, len(records))
	for i := range records {
		record := &records[i]
		next := *record
		next.Evidence = append([]string(nil), record.Evidence...)
		next.ConflictWith = append([]string(nil), record.ConflictWith...)
		next.Audit = append([]FeedbackGuidanceAuditEvent(nil), record.Audit...)

		if record.ExpiresAt != nil {
			expiresAt := *record.ExpiresAt
			next.ExpiresAt = &expiresAt
		}

		out = append(out, next)
	}

	return out
}

func cloneRoutingPolicy(policy RoutingPolicyConfig) RoutingPolicyConfig {
	return RoutingPolicyConfig{
		PreferredProviders:   append([]string(nil), policy.PreferredProviders...),
		BannedProviders:      append([]string(nil), policy.BannedProviders...),
		BannedModels:         append([]string(nil), policy.BannedModels...),
		RequiredCapabilities: append([]string(nil), policy.RequiredCapabilities...),
		MaxBudget:            policy.MaxBudget,
		MaxLatencyMS:         policy.MaxLatencyMS,
		MaxTTFTMS:            policy.MaxTTFTMS,
		RequireFreshMetadata: policy.RequireFreshMetadata,
	}
}

func routingPolicyConfigured(policy RoutingPolicyConfig) bool {
	return len(policy.PreferredProviders) > 0 ||
		len(policy.BannedProviders) > 0 ||
		len(policy.BannedModels) > 0 ||
		len(policy.RequiredCapabilities) > 0 ||
		policy.MaxBudget != 0 ||
		policy.MaxLatencyMS != 0 ||
		policy.MaxTTFTMS != 0 ||
		policy.RequireFreshMetadata
}

func cloneMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}

	out := make(map[string]string, len(in))
	maps.Copy(out, in)

	return out
}
