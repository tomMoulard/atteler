package config

import (
	"strconv"

	"github.com/tommoulard/atteler/pkg/autonomy"
)

// DefaultDiagnostic describes an implicit config behavior that applies when a
// higher-precedence source does not set the field explicitly.
type DefaultDiagnostic struct {
	Field   string `json:"field" yaml:"field"`
	Value   string `json:"value" yaml:"value"`
	Message string `json:"message" yaml:"message"`
}

// DefaultDiagnostics returns the built-in config defaults and zero-value
// semantics that are otherwise easy to miss when inspecting only loaded files.
func DefaultDiagnostics() []DefaultDiagnostic {
	defaults := []DefaultDiagnostic{
		{
			Field:   "version",
			Value:   strconv.Itoa(ConfigSchemaVersion),
			Message: "missing config versions are treated as legacy version 0 and migrated in memory to the current schema",
		},
		{
			Field:   "default_provider",
			Value:   "unset",
			Message: "when unset, provider selection comes from harness imports, CLI/runtime model selection, or registered provider order",
		},
		{
			Field:   "default_model",
			Value:   "unset",
			Message: "when unset, model selection comes from agent config, persisted state, CLI flags, or provider defaults",
		},
		{
			Field:   "fallback_models",
			Value:   "[]",
			Message: "no response-level fallback models are configured unless a source sets this list",
		},
		{
			Field:   "models",
			Value:   "{}",
			Message: "no task-oriented model roles are configured unless a source defines entries such as planner or fast_coder",
		},
		{
			Field:   "autonomy",
			Value:   autonomy.DefaultLevel.String(),
			Message: "when unset, agent runs default to local implementation autonomy: file edits and tests are allowed, but commits, pushes, PRs, and merges are blocked",
		},
		{
			Field:   "generation.temperature",
			Value:   "unset",
			Message: "provider/model default temperature is used unless config, agent config, or CLI flags set it",
		},
		{
			Field:   "generation.top_p",
			Value:   "unset",
			Message: "provider/model default top_p is used unless config, agent config, or CLI flags set it",
		},
		{
			Field:   "generation.seed",
			Value:   "unset",
			Message: "requests are not seeded unless config, agent config, or CLI flags set a seed",
		},
		{
			Field:   "generation.reasoning_level",
			Value:   "unset",
			Message: "provider/model default reasoning effort is used unless config, state, agent config, or CLI flags set it",
		},
		{
			Field:   "generation.model_mode",
			Value:   "unset",
			Message: "provider/model default mode is used unless config, state, agent config, or CLI flags set it",
		},
		{
			Field:   "generation.max_tokens",
			Value:   "0",
			Message: "no config-level completion token cap is sent unless config, agent config, or CLI flags set one",
		},
		{
			Field:   "research.source_policy.trusted_domains",
			Value:   "[]",
			Message: "research and retrieval do not trust specific domains unless project config, harness guidance, or CLI flags add them",
		},
		{
			Field:   "research.source_policy.denied_domains",
			Value:   "[]",
			Message: "source policy denies no domains unless project config, harness guidance, or CLI flags add them",
		},
		{
			Field:   "research.source_policy.prefer_source_types",
			Value:   "[official_docs, source_code, standard_or_spec]",
			Message: "source scoring prefers official documentation, source code, and standards/specifications unless config sets a different list",
		},
		{
			Field:   "research.source_policy.allow_low_trust_sources",
			Value:   "true",
			Message: "low-trust sources are allowed by default and marked for audit instead of being blocked",
		},
		{
			Field:   "research.source_policy.warn_on_low_trust_sources",
			Value:   "true",
			Message: "reports warn when included sources are weak evidence unless config disables the warning",
		},
		{
			Field:   "research.source_policy.require_evidence_for_high_impact_claims",
			Value:   "false",
			Message: "evidence is recommended for high-impact claims but not mandatory unless policy or harness guidance requires it",
		},
		{
			Field:   "agent_loop.max_output_bytes",
			Value:   "unset/0",
			Message: "agent-loop tool output is unlimited unless this field is set to a positive byte limit",
		},
		{
			Field:   "agent_loop.max_cost_micros",
			Value:   "unset/0",
			Message: "agent-loop estimated provider cost is unlimited unless this field is set to a positive micro-unit limit; priced model metadata is required when enabled",
		},
		{
			Field:   "agent_loop.max_input_tokens",
			Value:   "unset/0",
			Message: "agent-loop cumulative provider-reported input tokens are unlimited unless this field is set to a positive token limit; context.max_input_tokens remains a separate per-request preflight cap",
		},
		{
			Field:   "agent_loop.max_output_tokens",
			Value:   "unset/0",
			Message: "agent-loop cumulative provider-reported output tokens are unlimited unless this field is set to a positive token limit",
		},
		{
			Field:   "agent_loop.max_total_tokens",
			Value:   "unset/0",
			Message: "agent-loop cumulative tokens are unlimited unless this field is set to a positive token limit",
		},
		{
			Field:   "agent_loop.max_iterations",
			Value:   "unset/0",
			Message: "agent-loop turns are unlimited unless this field is set to a positive iteration limit",
		},
		{
			Field:   "agent_loop.max_model_calls",
			Value:   "unset/0",
			Message: "agent-loop model calls are unlimited unless this field is set to a positive call limit",
		},
		{
			Field:   "agent_loop.max_tool_calls",
			Value:   "unset/0",
			Message: "agent-loop tool executions are unlimited unless this field is set to a positive call limit",
		},
		{
			Field:   "agent_loop.max_wall_time",
			Value:   "unset/0",
			Message: "agent-loop wall-clock runtime is unlimited unless this field is set to a positive duration",
		},
		{
			Field:   "agent_loop.checkpoint_interval",
			Value:   "unset/0",
			Message: "interactive continuation checkpoints are disabled unless this field is set to a positive iteration interval",
		},
		{
			Field:   "context.references",
			Value:   "[]",
			Message: "no configured references are loaded unless this list is set",
		},
		{
			Field:   "context.project_instructions.enabled",
			Value:   "true",
			Message: "AGENTS.md files, or CLAUDE.md fallback files, are auto-loaded from the repository root down to the working directory unless disabled",
		},
		{
			Field:   "context.project_instructions.max_tokens",
			Value:   strconv.Itoa(DefaultProjectInstructionsMaxTokens),
			Message: "auto-loaded project instructions are compressed with contextpack to this token budget before being pinned into requests",
		},
		{
			Field:   "context.max_file_bytes",
			Value:   "0",
			Message: "the context loader applies no config-level per-file byte cap unless this is positive",
		},
		{
			Field:   "context.max_total_bytes",
			Value:   "0",
			Message: "the context loader applies no config-level total byte cap unless this is positive",
		},
		{
			Field:   "context.max_input_tokens",
			Value:   "0",
			Message: "the request path applies no config-level input token cap unless this is positive or overridden by CLI flags",
		},
		{
			Field:   "context.reference_policy.allowed_schemes",
			Value:   "[https]",
			Message: "configured remote references are HTTPS-only unless a config source narrows or expands the allowed URL schemes",
		},
		{
			Field:   "context.reference_policy.allowed_hosts",
			Value:   "[]",
			Message: "configured remote references are rejected unless a config source allowlists their host",
		},
		{
			Field:   "context.reference_policy.local_roots",
			Value:   "[]",
			Message: "configured local references are limited to the working directory unless extra local roots are set",
		},
		{
			Field:   "context.reference_policy.allow_absolute_paths",
			Value:   "false",
			Message: "configured absolute local references are rejected unless explicitly allowed",
		},
		{
			Field:   "context.reference_policy.max_redirects",
			Value:   "0",
			Message: "configured remote references do not follow redirects unless this is positive",
		},
		{
			Field:   "context.reference_policy.max_files",
			Value:   "200",
			Message: "configured reference loading stops after the built-in file count limit unless config sets max_files",
		},
		{
			Field:   "context.reference_policy.content_types",
			Value:   "[text/*, application/json, application/xml, application/x-yaml, application/yaml, application/toml]",
			Message: "configured remote references accept the loader's built-in safe content types unless this list narrows them",
		},
		{
			Field:   "context.reference_policy.allow_private_networks",
			Value:   "false",
			Message: "configured remote references to private networks are blocked unless explicitly allowed",
		},
		{
			Field:   "plugins.paths",
			Value:   "[]",
			Message: "no local plugin manifests are loaded unless this list is set",
		},
		{
			Field:   "worktree.auto_merge",
			Value:   "false",
			Message: "worktree sessions are preserved on exit unless config or CLI explicitly enables reviewed auto-merge",
		},
		{
			Field:   "worktree.verification_commands",
			Value:   "[]",
			Message: "auto-merge has no verification gate unless config or CLI supplies commands; an explicit override is required to merge without them",
		},
		{
			Field:   "worktree.override_verification",
			Value:   "false",
			Message: "auto-merge cannot skip verification commands unless config or CLI deliberately sets an override",
		},
		{
			Field:   "providers.*.disable_private_adapter",
			Value:   "false",
			Message: "private provider adapters remain eligible unless a provider config or environment kill switch disables them",
		},
		{
			Field:   "providers.*.credential_policy.allowed_providers",
			Value:   "[]",
			Message: "credential-source policy accepts any resolved provider name unless this list narrows it",
		},
		{
			Field:   "providers.*.credential_policy.allowed_stores",
			Value:   "[env]",
			Message: "credential-source policy only permits environment variables unless additional stores are explicitly allowed",
		},
		{
			Field:   "providers.*.credential_policy.allow_borrowed_oauth",
			Value:   "false",
			Message: "borrowed OAuth sessions from other tools are denied unless explicitly allowed",
		},
		{
			Field:   "providers.*.credential_policy.allow_refresh",
			Value:   "false",
			Message: "borrowed OAuth refresh is denied unless explicitly allowed",
		},
		{
			Field:   "providers.*.credential_policy.allow_write_back",
			Value:   "false",
			Message: "credential write-back to external CLI stores is denied unless explicitly allowed",
		},
		{
			Field:   "providers.*.local",
			Value:   "false",
			Message: "custom providers are treated as remote unless local is true or the provider can infer loopback/self-hosted execution",
		},
		{
			Field:   "skill_learning.enabled",
			Value:   "unset",
			Message: "automatic recurring-workflow skill learning uses its built-in default unless config or environment overrides it",
		},
		{
			Field:   "skill_learning.store_dir",
			Value:   "unset",
			Message: "skill-learning observations use the runtime default store directory unless config or environment overrides it",
		},
		{
			Field:   "skill_learning.skill_dir",
			Value:   "unset",
			Message: "generated skills use the runtime default skill directory unless config or environment overrides it",
		},
		{
			Field:   "skill_learning.min_occurrences",
			Value:   "0",
			Message: "the skill-learning runtime uses its built-in minimum occurrence threshold unless config sets this field",
		},
		{
			Field:   "skill_learning.max_steps",
			Value:   "0",
			Message: "the skill-learning runtime uses its built-in maximum step threshold unless config sets this field",
		},
		{
			Field:   "skill_learning.max_observations",
			Value:   "0",
			Message: "the skill-learning runtime uses its built-in observation retention limit unless config sets this field",
		},
		{
			Field:   "vector.workspace_enabled",
			Value:   "false",
			Message: "workspace vector indexing is disabled unless explicitly enabled; no workspace files are embedded or indexed by default",
		},
		{
			Field:   "vector.workspace_allow_remote_embeddings",
			Value:   "false",
			Message: "non-loopback embedding endpoints are blocked for workspace indexing unless explicitly allowed",
		},
		{
			Field:   "vector.vectorizer",
			Value:   "lexical",
			Message: "workspace vector context uses the local lexical hash vectorizer unless config selects model-backed embeddings",
		},
		{
			Field:   "vector.workspace_index_path",
			Value:   ".atteler/workspace-vector-index.json",
			Message: "enabled workspace vector indexes are stored under the launched workspace by default",
		},
		{
			Field:   "vector.fallback_policy",
			Value:   "fail",
			Message: "embedding failures fail closed unless config explicitly selects lexical fallback",
		},
		{
			Field:   "vector.workspace_max_file_bytes",
			Value:   "262144",
			Message: "workspace indexing skips files larger than the built-in per-file safety cap unless config sets a different positive limit",
		},
		{
			Field:   "vector.workspace_max_files",
			Value:   "5000",
			Message: "workspace indexing stops after the built-in file-count safety cap unless config sets a different positive limit",
		},
	}

	return append([]DefaultDiagnostic(nil), defaults...)
}
