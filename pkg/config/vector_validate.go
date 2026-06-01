package config

import (
	"fmt"
	"strings"
)

// ValidateVectorConfig checks vectorizer scope names and enum values that are
// resolved at runtime by local RAG stores. It is intentionally strict about
// unsupported store/source scopes because unknown names are ignored by scope
// resolution and would otherwise leave persisted indexes on unintended
// vectorizer defaults.
func ValidateVectorConfig(cfg VectorConfig) error {
	var issues []string

	issues = append(issues, validateVectorizerConfigValues("vector", cfg.DefaultVectorizerConfig())...)
	issues = append(issues, validateVectorStoreScopes(cfg.Stores)...)
	issues = append(issues, validateVectorAgentScopes(cfg.Agents)...)
	issues = append(issues, validateVectorSourceScopes(cfg.Sources)...)

	if len(issues) == 0 {
		return nil
	}

	return fmt.Errorf("vector config: %s", strings.Join(issues, "; "))
}

func validateVectorStoreScopes(stores map[string]VectorizerConfig) []string {
	var issues []string

	for _, name := range sortedMapKeys(stores) {
		field := "vector.stores." + name
		if !knownVectorStoreScope(name) {
			issues = append(issues, field+" unknown store scope (supported: agent-memory, vector-search, workspace)")
		}

		issues = append(issues, validateVectorizerConfigValues(field, stores[name])...)
	}

	return issues
}

func validateVectorAgentScopes(agents map[string]VectorizerConfig) []string {
	var issues []string
	for _, name := range sortedMapKeys(agents) {
		issues = append(issues, validateVectorizerConfigValues("vector.agents."+name, agents[name])...)
	}

	return issues
}

func validateVectorSourceScopes(sources map[string]VectorizerConfig) []string {
	var issues []string

	for _, name := range sortedMapKeys(sources) {
		field := "vector.sources." + name
		if !knownVectorSourceScope(name) {
			issues = append(issues, field+" unknown source scope (supported: file, session, git_history, adr)")
		}

		issues = append(issues, validateVectorizerConfigValues(field, sources[name])...)
	}

	return issues
}

func validateVectorizerConfigValues(field string, cfg VectorizerConfig) []string {
	var issues []string
	if !knownVectorizerKind(cfg.Vectorizer) {
		issues = append(issues, fmt.Sprintf("%s.vectorizer unsupported value %q (supported: lexical, embedding)", field, cfg.Vectorizer))
	}

	if !knownVectorProvider(cfg.Provider) {
		issues = append(issues, fmt.Sprintf("%s.provider unsupported value %q (supported: ollama-compatible)", field, cfg.Provider))
	}

	if !knownVectorFallbackPolicy(cfg.FallbackPolicy) {
		issues = append(issues, fmt.Sprintf("%s.fallback_policy unsupported value %q (supported: fail, lexical)", field, cfg.FallbackPolicy))
	}

	return issues
}

func knownVectorStoreScope(name string) bool {
	switch normalizeVectorizerScopeKey(name) {
	case "agent-memory", "vector-search", "workspace":
		return true
	default:
		return false
	}
}

func knownVectorSourceScope(name string) bool {
	switch normalizeVectorizerScopeKey(name) {
	case "file", "session", "git-history", "adr":
		return true
	default:
		return false
	}
}

func knownVectorizerKind(kind string) bool {
	switch normalizeVectorConfigValue(kind) {
	case "", "lexical", "lexical-fallback", "fallback", "text", "hashed", "hashed-token-frequency",
		"embedding", "embed", "embeddings":
		return true
	default:
		return false
	}
}

func knownVectorProvider(provider string) bool {
	switch normalizeVectorConfigValue(provider) {
	case "", "ollama", "ollama-compatible":
		return true
	default:
		return false
	}
}

func knownVectorFallbackPolicy(policy string) bool {
	switch normalizeVectorConfigValue(policy) {
	case "", "fail", "none", "lexical", "lexical-fallback", "fallback":
		return true
	default:
		return false
	}
}

func normalizeVectorConfigValue(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, "_", "-")

	return value
}
