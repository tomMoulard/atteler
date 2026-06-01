package config

import (
	"fmt"
	"path/filepath"
	"strings"
)

const (
	vectorIndexLifecycleAgentMemory = "agent-memory"
	vectorIndexLifecycleFile        = "file"
	vectorIndexLifecycleWorkspace   = "workspace"
	vectorIndexLifecycleSession     = "session"
	vectorIndexLifecycleGitHistory  = "git-history"
	vectorIndexLifecycleADR         = "adr"
)

// ValidateVectorConfig checks vectorizer scope names and enum values that are
// resolved at runtime by local RAG stores. It is intentionally strict about
// unsupported store/source scopes because unknown names are ignored by scope
// resolution and would otherwise leave persisted indexes on unintended
// vectorizer defaults.
func ValidateVectorConfig(cfg VectorConfig) error {
	return vectorConfigError(validateVectorConfigIssues(cfg))
}

// ValidateVectorConfigWithAgents checks vector config values and verifies that
// vector.agents scopes can resolve to configured agent names. Plain
// ValidateVectorConfig leaves agent names unconstrained because callers may
// validate vector settings without loading the full agent registry.
func ValidateVectorConfigWithAgents(cfg VectorConfig, agents map[string]AgentConfig) error {
	issues := validateVectorConfigIssues(cfg)
	issues = append(issues, validateVectorAgentScopeNames(cfg.Agents, agents)...)

	return vectorConfigError(issues)
}

func validateVectorConfigIssues(cfg VectorConfig) []string {
	var issues []string

	issues = append(issues, validateVectorizerConfigValues("vector", cfg.DefaultVectorizerConfig())...)
	issues = append(issues, validateVectorStoreScopes(cfg.Stores)...)
	issues = append(issues, validateVectorAgentScopes(cfg.Agents)...)
	issues = append(issues, validateVectorSourceScopes(cfg.Sources)...)
	issues = append(issues, validateVectorIndexPathIsolation(cfg)...)

	return issues
}

func vectorConfigError(issues []string) error {
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

func validateVectorAgentScopeNames(scopes map[string]VectorizerConfig, agents map[string]AgentConfig) []string {
	var issues []string

	for _, name := range sortedMapKeys(scopes) {
		if knownVectorAgentScope(name, agents) {
			continue
		}

		field := "vector.agents." + name
		issues = append(issues, field+" unknown agent scope (configure matching agents."+name+" or remove this override)")
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

type vectorIndexPathUse struct {
	field     string
	path      string
	lifecycle string
}

func validateVectorIndexPathIsolation(cfg VectorConfig) []string {
	var issues []string

	seen := make(map[string]vectorIndexPathUse)

	for _, use := range vectorConfiguredIndexPathUses(cfg) {
		normalizedPath := normalizeVectorIndexPathForCompare(use.path)
		if normalizedPath == "" {
			continue
		}

		previous, ok := seen[normalizedPath]
		if !ok {
			seen[normalizedPath] = use
			continue
		}

		if previous.lifecycle == use.lifecycle {
			continue
		}

		issues = append(issues, fmt.Sprintf(
			"%s index_path %q conflicts with %s; %s and %s indexes must not share a persisted path",
			use.field,
			use.path,
			previous.field,
			use.lifecycle,
			previous.lifecycle,
		))
	}

	return issues
}

func vectorConfiguredIndexPathUses(cfg VectorConfig) []vectorIndexPathUse {
	uses := make([]vectorIndexPathUse, 0, len(cfg.Stores)+len(cfg.Agents)+len(cfg.Sources)+2)
	uses = appendVectorIndexPathUse(uses, "vector.index_path", cfg.IndexPath, vectorIndexLifecycleFile)
	uses = appendVectorIndexPathUse(uses, "vector.workspace_index_path", cfg.WorkspaceIndexPath, vectorIndexLifecycleWorkspace)

	for _, name := range sortedMapKeys(cfg.Stores) {
		lifecycle, ok := vectorStoreIndexLifecycle(name)
		if !ok {
			continue
		}

		uses = appendVectorIndexPathUse(uses, "vector.stores."+name, cfg.Stores[name].IndexPath, lifecycle)
	}

	for _, name := range sortedMapKeys(cfg.Agents) {
		uses = appendVectorIndexPathUse(uses, "vector.agents."+name, cfg.Agents[name].IndexPath, vectorIndexLifecycleAgentMemory)
	}

	for _, name := range sortedMapKeys(cfg.Sources) {
		lifecycle, ok := vectorSourceIndexLifecycle(name)
		if !ok {
			continue
		}

		uses = appendVectorIndexPathUse(uses, "vector.sources."+name, cfg.Sources[name].IndexPath, lifecycle)
	}

	return uses
}

func appendVectorIndexPathUse(uses []vectorIndexPathUse, field, path, lifecycle string) []vectorIndexPathUse {
	if strings.TrimSpace(path) == "" {
		return uses
	}

	return append(uses, vectorIndexPathUse{
		field:     field,
		path:      path,
		lifecycle: lifecycle,
	})
}

func vectorStoreIndexLifecycle(name string) (string, bool) {
	switch normalizeVectorizerScopeKey(name) {
	case vectorIndexLifecycleAgentMemory:
		return vectorIndexLifecycleAgentMemory, true
	case "vector-search":
		return vectorIndexLifecycleFile, true
	case vectorIndexLifecycleWorkspace:
		return vectorIndexLifecycleWorkspace, true
	default:
		return "", false
	}
}

func vectorSourceIndexLifecycle(name string) (string, bool) {
	switch normalizeVectorizerScopeKey(name) {
	case vectorIndexLifecycleFile:
		return vectorIndexLifecycleFile, true
	case vectorIndexLifecycleSession:
		return vectorIndexLifecycleSession, true
	case vectorIndexLifecycleGitHistory:
		return vectorIndexLifecycleGitHistory, true
	case vectorIndexLifecycleADR:
		return vectorIndexLifecycleADR, true
	default:
		return "", false
	}
}

func knownVectorStoreScope(name string) bool {
	switch normalizeVectorizerScopeKey(name) {
	case vectorIndexLifecycleAgentMemory, "vector-search", vectorIndexLifecycleWorkspace:
		return true
	default:
		return false
	}
}

func knownVectorSourceScope(name string) bool {
	switch normalizeVectorizerScopeKey(name) {
	case vectorIndexLifecycleFile, vectorIndexLifecycleSession, vectorIndexLifecycleGitHistory, vectorIndexLifecycleADR:
		return true
	default:
		return false
	}
}

func knownVectorAgentScope(name string, agents map[string]AgentConfig) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}

	if _, ok := agents[name]; ok {
		return true
	}

	lowerName := strings.ToLower(name)
	for configured := range agents {
		if strings.ToLower(strings.TrimSpace(configured)) == lowerName {
			return true
		}
	}

	normalizedName := normalizeVectorizerScopeKey(name)
	for configured := range agents {
		if normalizeVectorizerScopeKey(configured) == normalizedName {
			return true
		}
	}

	return false
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

func normalizeVectorIndexPathForCompare(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}

	path = filepath.Clean(path)
	if path == "." {
		return ""
	}

	return filepath.ToSlash(path)
}
