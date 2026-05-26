package config

import "strconv"

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
			Field:   "generation.max_tokens",
			Value:   "0",
			Message: "no config-level completion token cap is sent unless config, agent config, or CLI flags set one",
		},
		{
			Field:   "agent_loop.max_output_bytes",
			Value:   "unset/0",
			Message: "agent-loop tool output is unlimited unless this field is set to a positive byte limit",
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
			Field:   "context.references",
			Value:   "[]",
			Message: "no configured references are loaded unless this list is set",
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
			Value:   "[]",
			Message: "configured remote references are rejected unless a config source allowlists their URL scheme",
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
			Field:   "context.reference_policy.max_redirects",
			Value:   "0",
			Message: "configured remote references do not follow redirects unless this is positive",
		},
		{
			Field:   "context.reference_policy.content_types",
			Value:   "[]",
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
	}

	return append([]DefaultDiagnostic(nil), defaults...)
}
