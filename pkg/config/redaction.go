package config

import (
	"encoding/json"
	"maps"
	"net/url"
	"strings"

	attelerplugin "github.com/tommoulard/atteler/pkg/plugin"
)

// RedactedValue is the placeholder used when a config value may contain a
// secret or private prompt content.
const RedactedValue = "<redacted>"

// RedactedConfig returns a deep copy of cfg with secret-bearing values replaced
// by placeholders so the result can be attached to issue reports.
func RedactedConfig(cfg Config) Config {
	out := cfg
	out.Generation = redactedGenerationConfig(cfg.Generation)
	out.AgentLoop = redactedAgentLoopConfig(cfg.AgentLoop)
	out.SkillLearning.Enabled = clonePtr(cfg.SkillLearning.Enabled)
	out.Vector = redactedVectorConfig(cfg.Vector)

	out.Providers = make(map[string]ProviderConfig, len(cfg.Providers))
	for name, provider := range cfg.Providers {
		provider.BaseURL = redactURL(provider.BaseURL)
		out.Providers[name] = provider
	}

	if len(out.Providers) == 0 {
		out.Providers = nil
	}

	out.Agents = make(map[string]AgentConfig, len(cfg.Agents))
	for name := range cfg.Agents {
		agent := cfg.Agents[name]
		agent.Temperature = clonePtr(agent.Temperature)
		agent.TopP = clonePtr(agent.TopP)
		agent.Seed = clonePtr(agent.Seed)
		agent.RoutingPolicy = cloneRoutingPolicy(agent.RoutingPolicy)
		agent.FallbackModels = append([]string(nil), agent.FallbackModels...)
		agent.Capabilities = append([]string(nil), agent.Capabilities...)
		agent.Triggers = append([]string(nil), agent.Triggers...)
		agent.References = redactStringSlice("references", agent.References)
		agent.ToolPermissions = cloneBoolMap(agent.ToolPermissions)
		agent.FeedbackGuidance = nil

		if strings.TrimSpace(agent.Description) != "" {
			agent.Description = RedactedValue
		}

		if strings.TrimSpace(agent.Personality) != "" {
			agent.Personality = RedactedValue
		}

		if strings.TrimSpace(agent.SystemPrompt) != "" {
			agent.SystemPrompt = RedactedValue
		}

		out.Agents[name] = agent
	}

	if len(out.Agents) == 0 {
		out.Agents = nil
	}

	out.Hooks = make(map[string][]HookConfig, len(cfg.Hooks))
	for event, hooks := range cfg.Hooks {
		out.Hooks[event] = redactHooks(hooks)
	}

	if len(out.Hooks) == 0 {
		out.Hooks = nil
	}

	out.FallbackModels = append([]string(nil), cfg.FallbackModels...)
	out.ModelAliases = maps.Clone(cfg.ModelAliases)
	out.Context.References = redactStringSlice("references", cfg.Context.References)
	out.Context.ReferencePolicy = redactedReferencePolicyConfig(cfg.Context.ReferencePolicy)

	out.Plugins.Paths = append([]string(nil), cfg.Plugins.Paths...)
	if cfg.Plugins.Policy != nil {
		policy := attelerplugin.ClonePolicy(*cfg.Plugins.Policy)
		out.Plugins.Policy = &policy
	}

	return out
}

func redactedGenerationConfig(cfg GenerationConfig) GenerationConfig {
	cfg.Temperature = clonePtr(cfg.Temperature)
	cfg.TopP = clonePtr(cfg.TopP)
	cfg.Seed = clonePtr(cfg.Seed)

	return cfg
}

func redactedAgentLoopConfig(cfg AgentLoopConfig) AgentLoopConfig {
	cfg.MaxOutputBytes = clonePtr(cfg.MaxOutputBytes)
	cfg.MaxCostMicros = clonePtr(cfg.MaxCostMicros)
	cfg.MaxInputTokens = clonePtr(cfg.MaxInputTokens)
	cfg.MaxOutputTokens = clonePtr(cfg.MaxOutputTokens)
	cfg.MaxTotalTokens = clonePtr(cfg.MaxTotalTokens)
	cfg.MaxIterations = clonePtr(cfg.MaxIterations)
	cfg.MaxModelCalls = clonePtr(cfg.MaxModelCalls)
	cfg.MaxToolCalls = clonePtr(cfg.MaxToolCalls)
	cfg.MaxWallTime = clonePtr(cfg.MaxWallTime)
	cfg.CheckpointInterval = clonePtr(cfg.CheckpointInterval)

	return cfg
}

func redactedReferencePolicyConfig(policy ReferencePolicyConfig) ReferencePolicyConfig {
	policy.AllowedSchemes = append([]string(nil), policy.AllowedSchemes...)
	policy.DeniedSchemes = append([]string(nil), policy.DeniedSchemes...)
	policy.AllowedHosts = append([]string(nil), policy.AllowedHosts...)
	policy.DeniedHosts = append([]string(nil), policy.DeniedHosts...)
	policy.AllowedPorts = append([]int(nil), policy.AllowedPorts...)
	policy.DeniedPorts = append([]int(nil), policy.DeniedPorts...)
	policy.LocalRoots = append([]string(nil), policy.LocalRoots...)
	policy.DeniedLocalRoots = append([]string(nil), policy.DeniedLocalRoots...)
	policy.AllowedGlobs = append([]string(nil), policy.AllowedGlobs...)
	policy.DeniedGlobs = append([]string(nil), policy.DeniedGlobs...)
	policy.ContentTypes = append([]string(nil), policy.ContentTypes...)

	return policy
}

func redactedVectorConfig(cfg VectorConfig) VectorConfig {
	cfg.WorkspaceEnabled = clonePtr(cfg.WorkspaceEnabled)
	cfg.WorkspaceAllowRemoteEmbeddings = clonePtr(cfg.WorkspaceAllowRemoteEmbeddings)
	cfg.Provider = redactPotentialSecretString(cfg.Provider)
	cfg.Model = redactPotentialSecretString(cfg.Model)
	cfg.BaseURL = redactURL(cfg.BaseURL)
	cfg.IndexPath = redactPotentialSecretString(cfg.IndexPath)
	cfg.WorkspaceIndexPath = redactPotentialSecretString(cfg.WorkspaceIndexPath)
	cfg.WorkspaceInclude = redactStringSlice("workspace_include", cfg.WorkspaceInclude)
	cfg.WorkspaceExclude = redactStringSlice("workspace_exclude", cfg.WorkspaceExclude)

	return cfg
}

func clonePtr[T any](value *T) *T {
	if value == nil {
		return nil
	}

	cloned := *value

	return &cloned
}

// RedactedOriginMap returns a deep copy of origins with origin values redacted
// using the same path-aware rules as RedactedConfig.
func RedactedOriginMap(origins OriginMap) OriginMap {
	out := make(OriginMap, len(origins))
	for path, origin := range origins {
		next := FieldOrigin{Chain: make([]OriginEvent, 0, len(origin.Chain))}
		for _, event := range origin.Chain {
			event.Value = RedactOriginValue(path, event.Value)
			next.Chain = append(next.Chain, event)
		}

		out[path] = next
	}

	return out
}

// RedactOriginValue redacts a string value from a field-origin event based on
// its dotted config path.
func RedactOriginValue(path, value string) string {
	path = strings.ToLower(path)
	switch {
	case strings.HasPrefix(path, "hooks."):
		return redactSerializedHooks(value)
	case path == "context.references" || strings.HasSuffix(path, ".references"):
		return redactSerializedStringSlice("references", value)
	case path == "vector.workspace_include" || path == "vector.workspace_exclude":
		return redactSerializedStringSlice(path, value)
	case strings.HasSuffix(path, ".base_url"):
		return redactURL(value)
	case strings.Contains(path, ".env"), strings.Contains(path, ".command"),
		strings.HasSuffix(path, ".description"), strings.HasSuffix(path, ".personality"),
		strings.HasSuffix(path, ".system_prompt"), strings.Contains(path, ".feedback_guidance"):
		if strings.TrimSpace(value) == "" {
			return value
		}

		return RedactedValue
	default:
		return redactPotentialSecretString(value)
	}
}

func redactHooks(hooks []HookConfig) []HookConfig {
	out := make([]HookConfig, 0, len(hooks))
	for _, hook := range hooks {
		next := HookConfig{
			Command:        redactCommandArgs(hook.Command),
			Env:            make(map[string]string, len(hook.Env)),
			TimeoutSeconds: hook.TimeoutSeconds,
		}
		for key, value := range hook.Env {
			next.Env[key] = redactValueForKey(key, value)
		}

		if len(next.Env) == 0 {
			next.Env = nil
		}

		out = append(out, next)
	}

	return out
}

func cloneBoolMap(in map[string]bool) map[string]bool {
	if len(in) == 0 {
		return nil
	}

	return maps.Clone(in)
}

func redactCommandArgs(args []string) []string {
	out := append([]string(nil), args...)
	for i, arg := range out {
		if i > 0 && isSensitiveKey(strings.TrimLeft(out[i-1], "-")) {
			out[i] = RedactedValue

			continue
		}

		out[i] = redactPotentialSecretString(arg)
	}

	return out
}

func redactStringSlice(key string, in []string) []string {
	out := append([]string(nil), in...)
	for i := range out {
		out[i] = redactValueForKey(key, out[i])
	}

	return out
}

func redactValueForKey(key, value string) string {
	if value == "" {
		return ""
	}

	if isSensitiveKey(key) {
		return RedactedValue
	}

	return redactPotentialSecretString(value)
}

func redactURL(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return raw
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		return redactInlineSecret(raw)
	}

	if parsed.User != nil {
		if _, hasPassword := parsed.User.Password(); hasPassword {
			parsed.User = url.UserPassword(RedactedValue, RedactedValue)
		} else {
			parsed.User = url.User(RedactedValue)
		}
	}

	query := parsed.Query()
	changed := false

	for key := range query {
		if isSensitiveKey(key) {
			query.Set(key, RedactedValue)

			changed = true
		}
	}

	if changed {
		parsed.RawQuery = query.Encode()
	}

	return redactInlineSecret(parsed.String())
}

func redactInlineSecret(value string) string {
	if strings.TrimSpace(value) == "" {
		return value
	}

	if key, rest, ok := strings.Cut(value, "="); ok && isSensitiveKey(key) && rest != "" {
		return key + "=" + RedactedValue
	}

	if key, rest, ok := strings.Cut(value, ":"); ok && isSensitiveKey(key) && strings.TrimSpace(rest) != "" {
		return key + ": " + RedactedValue
	}

	return value
}

func redactPotentialSecretString(value string) string {
	if strings.TrimSpace(value) == "" {
		return value
	}

	return redactURL(value)
}

func redactSerializedHooks(value string) string {
	if strings.TrimSpace(value) == "" {
		return value
	}

	var hooks []HookConfig
	if err := json.Unmarshal([]byte(value), &hooks); err != nil {
		return RedactedValue
	}

	data, err := json.Marshal(redactHooks(hooks))
	if err != nil {
		return RedactedValue
	}

	return string(data)
}

func redactSerializedStringSlice(key, value string) string {
	if strings.TrimSpace(value) == "" {
		return value
	}

	var values []string
	if err := json.Unmarshal([]byte(value), &values); err != nil {
		return redactPotentialSecretString(value)
	}

	data, err := json.Marshal(redactStringSlice(key, values))
	if err != nil {
		return RedactedValue
	}

	return string(data)
}

func isSensitiveKey(key string) bool {
	normalized := strings.ToLower(strings.TrimSpace(key))
	normalized = strings.Trim(normalized, "-_ .")
	normalized = strings.NewReplacer("-", "_", ".", "_").Replace(normalized)

	if normalized == "" {
		return false
	}

	for _, marker := range []string{
		"secret",
		"token",
		"api_key",
		"apikey",
		"access_token",
		"refresh_token",
		"password",
		"passwd",
		"credential",
		"client_secret",
		"private_key",
		"bearer",
		"authorization",
		"auth",
	} {
		if strings.Contains(normalized, marker) {
			return true
		}
	}

	return false
}
