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

	vectorFileDefaultIndexPath        = ".atteler/vector-index.json"
	vectorWorkspaceDefaultIndexPath   = ".atteler/workspace-vector-index.json"
	vectorAgentMemoryDefaultIndexPath = ".atteler/agent-memory.json"
	vectorSessionDefaultIndexPath     = ".atteler/session-vector-index.json"
	vectorGitHistoryDefaultIndexPath  = ".atteler/git-history-vector-index.json"
	vectorADRDefaultIndexPath         = ".atteler/adr-vector-index.json"

	vectorValidationDefaultEmbeddingProvider = "ollama"
	vectorValidationDefaultEmbeddingModel    = "nomic-embed-text"
	vectorValidationDefaultEmbeddingBaseURL  = "http://127.0.0.1:11434"
	vectorValidationKindLexical              = "lexical"
	vectorValidationKindEmbedding            = "embedding"
	vectorValidationFallbackFail             = "fail"
	vectorValidationAliasLexicalFallback     = "lexical-fallback"
	vectorValidationAliasFallback            = "fallback"
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
	issues = append(issues, validateVectorAgentMemorySharing(cfg, agents)...)

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

		issues = append(issues, vectorIndexPathConflictMessage(use, previous))
	}

	return issues
}

func vectorIndexPathConflictMessage(use, previous vectorIndexPathUse) string {
	if vectorIndexPathUseIsDefault(use) && !vectorIndexPathUseIsDefault(previous) {
		use, previous = previous, use
	}

	return fmt.Sprintf(
		"%s index_path %q conflicts with %s; %s and %s indexes must not share a persisted path",
		use.field,
		use.path,
		previous.field,
		use.lifecycle,
		previous.lifecycle,
	)
}

func vectorIndexPathUseIsDefault(use vectorIndexPathUse) bool {
	return strings.Contains(use.field, " default")
}

type vectorAgentMemoryUse struct {
	identity vectorAgentMemoryIdentity
	field    string
	path     string
}

type vectorAgentMemoryIdentity struct {
	kind     string
	provider string
	model    string
	baseURL  string
}

func validateVectorAgentMemorySharing(cfg VectorConfig, agents map[string]AgentConfig) []string {
	var issues []string

	seen := make(map[string]vectorAgentMemoryUse, len(agents))

	for _, agentName := range sortedMapKeys(agents) {
		use := vectorAgentMemoryUseForAgent(cfg, agentName)
		if use.path == "" {
			continue
		}

		previous, ok := seen[use.path]
		if !ok {
			seen[use.path] = use
			continue
		}

		if previous.identity == use.identity {
			continue
		}

		issues = append(issues, fmt.Sprintf(
			"%s shares agent-memory index_path %q with %s but resolves a different vectorizer identity; give agents distinct index_path values or use one shared vectorizer",
			use.field,
			use.path,
			previous.field,
		))
	}

	return issues
}

func vectorAgentMemoryUseForAgent(cfg VectorConfig, agentName string) vectorAgentMemoryUse {
	resolved := cfg.ResolveVectorizerConfig(VectorScope{
		Store: vectorIndexLifecycleAgentMemory,
		Agent: agentName,
	})

	return vectorAgentMemoryUse{
		identity: vectorAgentMemoryIdentityForConfig(resolved),
		field:    vectorAgentMemoryFieldForAgent(cfg, agentName),
		path:     normalizeVectorIndexPathForCompare(vectorAgentMemoryPathForAgent(cfg, agentName)),
	}
}

func vectorAgentMemoryFieldForAgent(cfg VectorConfig, agentName string) string {
	if scoped, ok := vectorizerScopeConfig(cfg.Agents, agentName); ok && vectorizerConfigHasValue(scoped) {
		return "vector.agents." + agentName
	}

	if scoped, ok := vectorizerScopeConfig(cfg.Stores, vectorIndexLifecycleAgentMemory); ok && vectorizerConfigHasValue(scoped) {
		return "vector.stores." + vectorIndexLifecycleAgentMemory
	}

	return "agents." + agentName
}

func vectorizerConfigHasValue(cfg VectorizerConfig) bool {
	return strings.TrimSpace(cfg.Vectorizer) != "" ||
		strings.TrimSpace(cfg.Provider) != "" ||
		strings.TrimSpace(cfg.Model) != "" ||
		strings.TrimSpace(cfg.BaseURL) != "" ||
		strings.TrimSpace(cfg.FallbackPolicy) != "" ||
		strings.TrimSpace(cfg.IndexPath) != "" ||
		cfg.TimeoutSeconds > 0 ||
		cfg.ChunkMaxRunes > 0 ||
		cfg.ChunkOverlapRunes > 0
}

func vectorAgentMemoryPathForAgent(cfg VectorConfig, agentName string) string {
	storeConfig, _ := vectorizerScopeConfig(cfg.Stores, vectorIndexLifecycleAgentMemory)
	agentConfig, _ := vectorizerScopeConfig(cfg.Agents, agentName)

	return firstTrimmedNonEmpty(agentConfig.IndexPath, storeConfig.IndexPath, vectorAgentMemoryDefaultIndexPath)
}

func vectorAgentMemoryIdentityForConfig(cfg VectorizerConfig) vectorAgentMemoryIdentity {
	kind := canonicalVectorizerKindForValidation(cfg.Vectorizer)
	if kind != vectorValidationKindEmbedding {
		return vectorAgentMemoryIdentity{kind: vectorValidationKindLexical}
	}

	return vectorAgentMemoryIdentity{
		kind:     kind,
		provider: canonicalVectorProviderForValidation(cfg.Provider),
		model:    firstTrimmedNonEmpty(cfg.Model, vectorValidationDefaultEmbeddingModel),
		baseURL:  firstTrimmedNonEmpty(cfg.BaseURL, vectorValidationDefaultEmbeddingBaseURL),
	}
}

func canonicalVectorizerKindForValidation(kind string) string {
	switch normalizeVectorConfigValue(kind) {
	case "", vectorValidationKindLexical, vectorValidationAliasLexicalFallback, vectorValidationAliasFallback, "text", "hashed", "hashed-token-frequency":
		return vectorValidationKindLexical
	case vectorValidationKindEmbedding, "embed", "embeddings":
		return vectorValidationKindEmbedding
	default:
		return normalizeVectorConfigValue(kind)
	}
}

func canonicalVectorProviderForValidation(provider string) string {
	switch normalizeVectorConfigValue(provider) {
	case "", "ollama", "ollama-compatible":
		return vectorValidationDefaultEmbeddingProvider
	default:
		return normalizeVectorConfigValue(provider)
	}
}

func canonicalVectorFallbackPolicyForValidation(policy string) string {
	switch normalizeVectorConfigValue(policy) {
	case "", vectorValidationFallbackFail, "none":
		return vectorValidationFallbackFail
	case vectorValidationKindLexical, vectorValidationAliasLexicalFallback, vectorValidationAliasFallback:
		return vectorValidationKindLexical
	default:
		return normalizeVectorConfigValue(policy)
	}
}

func vectorConfiguredIndexPathUses(cfg VectorConfig) []vectorIndexPathUse {
	uses := make([]vectorIndexPathUse, 0, len(cfg.Stores)+len(cfg.Agents)+len(cfg.Sources)+11)
	uses = appendVectorIndexPathUse(
		uses,
		vectorIndexPathField("vector.index_path", cfg.IndexPath),
		firstTrimmedNonEmpty(cfg.IndexPath, vectorFileDefaultIndexPath),
		vectorIndexLifecycleFile,
	)
	uses = appendVectorIndexPathUse(
		uses,
		vectorIndexPathField("vector.workspace_index_path", cfg.WorkspaceIndexPath),
		firstTrimmedNonEmpty(cfg.WorkspaceIndexPath, vectorWorkspaceDefaultIndexPath),
		vectorIndexLifecycleWorkspace,
	)
	uses = appendVectorIndexPathUse(uses, "vector.stores.agent-memory default", vectorAgentMemoryDefaultIndexPath, vectorIndexLifecycleAgentMemory)
	uses = appendVectorIndexPathUse(uses, "vector.sources.session default", vectorSessionDefaultIndexPath, vectorIndexLifecycleSession)
	uses = appendVectorIndexPathUse(uses, "vector.sources.git_history default", vectorGitHistoryDefaultIndexPath, vectorIndexLifecycleGitHistory)
	uses = appendVectorIndexPathUse(uses, "vector.sources.adr default", vectorADRDefaultIndexPath, vectorIndexLifecycleADR)

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

	uses = appendVectorLexicalFallbackIndexPathUses(uses, cfg)

	return uses
}

func appendVectorLexicalFallbackIndexPathUses(uses []vectorIndexPathUse, cfg VectorConfig) []vectorIndexPathUse {
	fileField, filePath := vectorEffectiveFileIndexPath(cfg)
	uses = appendVectorLexicalFallbackIndexPathUse(
		uses,
		fileField,
		filePath,
		vectorIndexLifecycleFile,
		cfg.ResolveVectorizerConfig(VectorScope{
			Store:  "vector-search",
			Source: vectorIndexLifecycleFile,
		}),
	)

	workspaceField, workspacePath := vectorEffectiveWorkspaceIndexPath(cfg)
	uses = appendVectorLexicalFallbackIndexPathUse(
		uses,
		workspaceField,
		workspacePath,
		vectorIndexLifecycleWorkspace,
		cfg.ResolveVectorizerConfig(VectorScope{
			Store:  vectorIndexLifecycleWorkspace,
			Source: vectorIndexLifecycleFile,
		}),
	)

	sessionField, sessionPath := vectorEffectiveSourceIndexPath(cfg, vectorIndexLifecycleSession, vectorSessionDefaultIndexPath)
	uses = appendVectorLexicalFallbackIndexPathUse(
		uses,
		sessionField,
		sessionPath,
		vectorIndexLifecycleSession,
		cfg.ResolveVectorizerConfig(VectorScope{Source: vectorIndexLifecycleSession}),
	)

	gitHistoryField, gitHistoryPath := vectorEffectiveSourceIndexPath(cfg, vectorIndexLifecycleGitHistory, vectorGitHistoryDefaultIndexPath)
	uses = appendVectorLexicalFallbackIndexPathUse(
		uses,
		gitHistoryField,
		gitHistoryPath,
		vectorIndexLifecycleGitHistory,
		cfg.ResolveVectorizerConfig(VectorScope{Source: vectorIndexLifecycleGitHistory}),
	)

	adrField, adrPath := vectorEffectiveSourceIndexPath(cfg, vectorIndexLifecycleADR, vectorADRDefaultIndexPath)
	uses = appendVectorLexicalFallbackIndexPathUse(
		uses,
		adrField,
		adrPath,
		vectorIndexLifecycleADR,
		cfg.ResolveVectorizerConfig(VectorScope{Source: vectorIndexLifecycleADR}),
	)

	return uses
}

func appendVectorLexicalFallbackIndexPathUse(
	uses []vectorIndexPathUse,
	field string,
	path string,
	lifecycle string,
	resolved VectorizerConfig,
) []vectorIndexPathUse {
	if !vectorizerConfigUsesLexicalFallback(resolved) {
		return uses
	}

	fallbackPath := vectorLexicalFallbackIndexPath(path)
	if normalizeVectorIndexPathForCompare(fallbackPath) == normalizeVectorIndexPathForCompare(path) {
		return uses
	}

	return appendVectorIndexPathUse(uses, field+" lexical fallback", fallbackPath, lifecycle)
}

func vectorizerConfigUsesLexicalFallback(cfg VectorizerConfig) bool {
	return canonicalVectorizerKindForValidation(cfg.Vectorizer) == vectorValidationKindEmbedding &&
		canonicalVectorFallbackPolicyForValidation(cfg.FallbackPolicy) == vectorValidationKindLexical
}

func vectorEffectiveFileIndexPath(cfg VectorConfig) (field, path string) {
	if sourceConfig, ok := vectorizerScopeConfig(cfg.Sources, vectorIndexLifecycleFile); ok &&
		strings.TrimSpace(sourceConfig.IndexPath) != "" {
		return "vector.sources.file", sourceConfig.IndexPath
	}

	if storeConfig, ok := vectorizerScopeConfig(cfg.Stores, "vector-search"); ok &&
		strings.TrimSpace(storeConfig.IndexPath) != "" {
		return "vector.stores.vector-search", storeConfig.IndexPath
	}

	if strings.TrimSpace(cfg.IndexPath) != "" {
		return "vector.index_path", cfg.IndexPath
	}

	return "vector.index_path default", vectorFileDefaultIndexPath
}

func vectorEffectiveWorkspaceIndexPath(cfg VectorConfig) (field, path string) {
	if storeConfig, ok := vectorizerScopeConfig(cfg.Stores, vectorIndexLifecycleWorkspace); ok &&
		strings.TrimSpace(storeConfig.IndexPath) != "" {
		return "vector.stores.workspace", storeConfig.IndexPath
	}

	if strings.TrimSpace(cfg.WorkspaceIndexPath) != "" {
		return "vector.workspace_index_path", cfg.WorkspaceIndexPath
	}

	return "vector.workspace_index_path default", vectorWorkspaceDefaultIndexPath
}

func vectorEffectiveSourceIndexPath(cfg VectorConfig, sourceKind, defaultIndexPath string) (field, path string) {
	field = "vector.sources." + vectorSourceConfigFieldName(sourceKind)
	if sourceConfig, ok := vectorizerScopeConfig(cfg.Sources, sourceKind); ok &&
		strings.TrimSpace(sourceConfig.IndexPath) != "" {
		return field, sourceConfig.IndexPath
	}

	return field + " default", defaultIndexPath
}

func vectorSourceConfigFieldName(sourceKind string) string {
	if sourceKind == vectorIndexLifecycleGitHistory {
		return "git_history"
	}

	return sourceKind
}

func vectorLexicalFallbackIndexPath(indexPath string) string {
	indexPath = strings.TrimSpace(indexPath)
	if indexPath == "" {
		return "vector-index.lexical.json"
	}

	if strings.HasSuffix(indexPath, ".lexical") {
		return indexPath
	}

	extension := filepath.Ext(indexPath)
	if extension == "" {
		return indexPath + ".lexical"
	}

	stem := strings.TrimSuffix(indexPath, extension)
	if strings.HasSuffix(stem, ".lexical") {
		return indexPath
	}

	return stem + ".lexical" + extension
}

func vectorIndexPathField(field, path string) string {
	if strings.TrimSpace(path) == "" {
		return field + " default"
	}

	return field
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
	case "", vectorValidationKindLexical, vectorValidationAliasLexicalFallback, vectorValidationAliasFallback, "text", "hashed", "hashed-token-frequency",
		vectorValidationKindEmbedding, "embed", "embeddings":
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
	case "", vectorValidationFallbackFail, "none", vectorValidationKindLexical, vectorValidationAliasLexicalFallback, vectorValidationAliasFallback:
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

func firstTrimmedNonEmpty(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}

	return ""
}
