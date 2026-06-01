//nolint:wsl_v5 // Memory CLI scope/policy helpers use compact guard clauses for readability.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tommoulard/atteler/pkg/agentmemory"
	appconfig "github.com/tommoulard/atteler/pkg/config"
	"github.com/tommoulard/atteler/pkg/contextpack"
	"github.com/tommoulard/atteler/pkg/githistory"
	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/lsp"
	"github.com/tommoulard/atteler/pkg/memory"
	"github.com/tommoulard/atteler/pkg/privacy"
	"github.com/tommoulard/atteler/pkg/retrieval"
	"github.com/tommoulard/atteler/pkg/session"
	"github.com/tommoulard/atteler/pkg/vector"
)

const (
	retrievalSourceAll     = "all"
	retrievalSourceSession = "session"

	sourceVectorADRIndex        = ".atteler/adr-vector-index.json"
	sourceVectorGitHistoryIndex = ".atteler/git-history-vector-index.json"
	sourceVectorSessionIndex    = ".atteler/session-vector-index.json"
)

var adrSourceDirectories = []string{
	"docs/adr",
	"docs/adrs",
	"docs/decisions",
	"docs/architecture/decisions",
	"adr",
	"adrs",
	"architecture/decisions",
}

func runLSPSymbols(ctx context.Context, input lspSymbolsCommandInput) error {
	format, err := structuredCommandOutputFormat(input.JSON, input.OutputFormat)
	if err != nil {
		return err
	}

	// Use the package managed pool so repeated lookups in a long-lived atteler
	// process reuse healthy language servers. The per-request policy still runs
	// before both new starts and healthy reuse.
	lspOptions := lsp.Options{
		Command:       strings.TrimSpace(input.Command),
		Args:          append([]string(nil), input.Args...),
		FilePath:      strings.TrimSpace(input.FilePath),
		RootPath:      strings.TrimSpace(input.RootPath),
		LanguageID:    strings.TrimSpace(input.LanguageID),
		CommandPolicy: authorizeLSPCommand,
	}

	var symbols []lsp.Symbol

	if strings.TrimSpace(input.WorkspaceSymbols) != "" {
		symbols, err = lsp.WorkspaceSymbols(ctx, lspOptions, input.WorkspaceSymbols)
	} else {
		symbols, err = lsp.DocumentSymbols(ctx, lspOptions)
	}

	if err != nil {
		return fmt.Errorf("lsp symbols: %w", err)
	}

	response := buildLSPCodeIntelResponse(input, symbols)

	return writeCodeIntelResponse(os.Stdout, response, format)
}

func authorizeLSPCommand(ctx context.Context, spec lsp.CommandSpec) error {
	command := strings.Join(append([]string{spec.Command}, spec.Args...), " ")
	decision := llm.BashToolPolicy(ctx, llm.ToolCall{
		Name:  "bash",
		Input: map[string]any{"command": command},
	}, llm.AgentLoopBudgetSnapshot{})

	switch decision.Verdict {
	case llm.ToolPolicyAllow:
		return nil
	case llm.ToolPolicyRequireConfirm:
		return fmt.Errorf("lsp command requires confirmation by local process policy (%s): %s", decision.MatchedRule, decision.Reason)
	case llm.ToolPolicyDeny:
		return fmt.Errorf("lsp command denied by local process policy (%s): %s", decision.MatchedRule, decision.Reason)
	case llm.ToolPolicyDryRun:
		return fmt.Errorf("lsp command blocked by local process policy dry-run decision (%s): %s", decision.MatchedRule, decision.Reason)
	default:
		return fmt.Errorf("lsp command blocked by unknown local process policy verdict %q (%s): %s", decision.Verdict, decision.MatchedRule, decision.Reason)
	}
}

func runContextPack(path string, maxTokens int, model string) error {
	return runContextPackWithWriters(os.Stdout, os.Stderr, path, maxTokens, model)
}

func runContextPackWithWriters(stdout, stderr io.Writer, path string, maxTokens int, model string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("context pack: read %s: %w", path, err)
	}

	messages, metadata := parseContextPackMessagesWithMetadata(string(data))

	result := contextpack.CompactWithOptions(messages, contextpack.Options{
		Model:     model,
		Metadata:  metadata,
		MaxTokens: maxTokens,
	})
	if result.Stats.HardBudgetFailure {
		if stderr != nil {
			fmt.Fprint(stderr, formatContextPackAudit(result))
		}

		return fmt.Errorf("context pack: required context does not fit token budget: %s", result.Stats.BudgetFailureReason)
	}

	if stdout != nil {
		fmt.Fprint(stdout, formatContextPackResult(result))
	}

	return nil
}

func parseContextPackMessages(text string) []llm.Message {
	messages, _ := parseContextPackMessagesWithMetadata(text)

	return messages
}

func parseContextPackMessagesWithMetadata(text string) ([]llm.Message, []contextpack.MessageMetadata) {
	var messages []llm.Message

	var metadata []contextpack.MessageMetadata

	for rawLine := range strings.SplitSeq(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		line := strings.TrimRight(rawLine, "\r")

		role, content, messageMetadata, ok := parseRoleLineWithMetadata(line)
		if ok {
			messages = append(messages, llm.Message{Role: role, Content: content})
			metadata = append(metadata, messageMetadata)

			continue
		}

		if len(messages) == 0 {
			if strings.TrimSpace(line) != "" {
				messages = append(messages, llm.Message{Role: llm.RoleUser, Content: line})
				metadata = append(metadata, contextpack.MessageMetadata{})
			}

			continue
		}

		if line != "" {
			messages[len(messages)-1].Content += "\n" + line
		}
	}

	return messages, metadata
}

func parseRoleLineWithMetadata(line string) (llm.Role, string, contextpack.MessageMetadata, bool) {
	roleText, content, ok := splitContextPackRoleLine(line)
	if !ok {
		return "", "", contextpack.MessageMetadata{}, false
	}

	roleName, metadata := parseRoleNameAndMetadata(roleText)

	switch strings.ToLower(roleName) {
	case string(llm.RoleSystem), "developer":
		return llm.RoleSystem, strings.TrimSpace(content), metadata, true
	case string(llm.RoleUser):
		return llm.RoleUser, strings.TrimSpace(content), metadata, true
	case string(llm.RoleAssistant):
		return llm.RoleAssistant, strings.TrimSpace(content), metadata, true
	case string(llm.RoleTool):
		return llm.RoleTool, strings.TrimSpace(content), metadata, true
	default:
		return "", "", contextpack.MessageMetadata{}, false
	}
}

func splitContextPackRoleLine(line string) (roleText, content string, ok bool) {
	lower := strings.ToLower(strings.TrimLeft(line, " 	"))
	trimmedPrefixBytes := len(line) - len(strings.TrimLeft(line, " 	"))

	for _, roleName := range []string{
		string(llm.RoleSystem),
		"developer",
		string(llm.RoleUser),
		string(llm.RoleAssistant),
		string(llm.RoleTool),
	} {
		if strings.HasPrefix(lower, roleName+":") {
			cut := trimmedPrefixBytes + len(roleName)

			return line[:cut], line[cut+1:], true
		}

		if strings.HasPrefix(lower, roleName+"[") {
			closeBracket := strings.Index(line[trimmedPrefixBytes:], "]:")
			if closeBracket < 0 {
				continue
			}

			cut := trimmedPrefixBytes + closeBracket + 1

			return line[:cut], line[cut+1:], true
		}
	}

	return "", "", false
}

func parseRoleNameAndMetadata(roleText string) (roleName string, metadata contextpack.MessageMetadata) {
	roleText = strings.TrimSpace(roleText)
	if !strings.HasSuffix(roleText, "]") {
		return roleText, contextpack.MessageMetadata{}
	}

	start := strings.LastIndex(roleText, "[")
	if start <= 0 {
		return roleText, contextpack.MessageMetadata{}
	}

	roleName = strings.TrimSpace(roleText[:start])

	metadata, metadataOK := parseContextPackMetadata(strings.TrimSpace(roleText[start+1 : len(roleText)-1]))
	if roleName == "" || !metadataOK {
		return roleText, contextpack.MessageMetadata{}
	}

	return roleName, metadata
}

func parseContextPackMetadata(raw string) (contextpack.MessageMetadata, bool) {
	var metadata contextpack.MessageMetadata

	var ok bool

	for part := range strings.SplitSeq(raw, ",") {
		if parseContextPackMetadataPart(&metadata, part) {
			ok = true
		}
	}

	return metadata, ok
}

func parseContextPackMetadataPart(metadata *contextpack.MessageMetadata, part string) bool {
	key, value, ok := strings.Cut(strings.TrimSpace(part), "=")
	key = strings.ToLower(strings.TrimSpace(key))
	value = strings.TrimSpace(value)

	if isPinMetadataKey(key) {
		metadata.Pinned = !ok || parseMetadataBool(value)
		return true
	}

	if ok && key == "priority" {
		if priority, err := strconv.Atoi(value); err == nil {
			metadata.Priority = priority
			return true
		}

		return false
	}

	if ok && isTimestampMetadataKey(key) {
		metadata.Timestamp = value
		return true
	}

	if !ok && metadata.Timestamp == "" && strings.TrimSpace(part) != "" {
		metadata.Timestamp = strings.TrimSpace(part)
		return true
	}

	return false
}

func isPinMetadataKey(key string) bool {
	return key == "pin" || key == "pinned"
}

func isTimestampMetadataKey(key string) bool {
	return key == "timestamp" || key == "time" || key == "ts"
}

func parseMetadataBool(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "y", "on":
		return true
	default:
		return false
	}
}

func formatContextPackResult(result contextpack.Result) string {
	return formatContextPackResultWithOutput(result, true)
}

func formatContextPackAudit(result contextpack.Result) string {
	return formatContextPackResultWithOutput(result, false)
}

func formatContextPackResultWithOutput(result contextpack.Result, includeOutput bool) string {
	var b strings.Builder

	stats := result.Stats
	fmt.Fprintf(&b, "compressed: %t\n", stats.Compressed)
	fmt.Fprintf(&b, "messages: %d/%d\n", stats.OutputCount, stats.OriginalCount)
	fmt.Fprintf(&b, "omitted: %d\n", stats.OmittedCount)
	fmt.Fprintf(&b, "tokens: %d/%d", stats.OutputEstimatedTokens, stats.OriginalEstimatedTokens)

	if stats.OutputEstimatedUpperBound > 0 || stats.OriginalEstimatedUpperBound > 0 {
		fmt.Fprintf(&b, " upper=%d/%d", stats.OutputEstimatedUpperBound, stats.OriginalEstimatedUpperBound)
	}

	if stats.OutputEstimateErrorBound > 0 || stats.OriginalEstimateErrorBound > 0 {
		fmt.Fprintf(&b, " error_bound=%d/%d", stats.OutputEstimateErrorBound, stats.OriginalEstimateErrorBound)
	}

	if stats.MaxEstimatedTokens > 0 {
		fmt.Fprintf(&b, " max=%d fits=%t", stats.MaxEstimatedTokens, stats.FitsBudget)
	}

	b.WriteString("\n")

	if stats.Estimator != "" {
		fmt.Fprintf(&b, "estimator: %s\n", stats.Estimator)
	}

	if stats.Policy != "" {
		fmt.Fprintf(&b, "policy: %s\n", stats.Policy)
	}

	if stats.HardBudgetFailure {
		writeContextPackBudgetFailure(&b, stats)
	}

	if result.Manifest.OmittedCount > 0 || len(result.Manifest.Ranges) > 0 || len(result.Manifest.Items) > 0 {
		if data, err := json.Marshal(result.Manifest); err == nil {
			fmt.Fprintf(&b, "manifest: %s\n", data)
		}
	}

	if !includeOutput {
		return b.String()
	}

	b.WriteString("output:\n")

	for _, message := range result.Messages {
		fmt.Fprintf(&b, "  %s: %s\n", message.Role, strings.ReplaceAll(message.Content, "\n", "\n    "))
	}

	return b.String()
}

func writeContextPackBudgetFailure(b *strings.Builder, stats contextpack.Stats) {
	fmt.Fprintf(b, "budget_failure: %s\n", stats.BudgetFailureReason)

	if stats.BudgetFailureReasonCode != "" {
		fmt.Fprintf(b, "budget_failure_code: %s\n", stats.BudgetFailureReasonCode)
	}
}

const agentMemoryVectorStore = "agent-memory"

type agentMemoryVectorizerRuntime struct {
	vectorizer vector.Vectorizer
	spec       vector.VectorizerSpec
	configured bool
}

func runAgentMemoryCommand(
	ctx context.Context,
	root,
	selectedAgent string,
	cfg appconfig.VectorConfig,
	input agentMemoryCommandInput,
) error {
	agentName := strings.TrimSpace(input.Agent)
	if agentName == "" {
		agentName = strings.TrimSpace(selectedAgent)
	}

	if agentName == "" && agentMemoryCommandNeedsAgent(input) {
		return errors.New("agent memory: --agent-memory-agent or --agent is required")
	}

	storePath := agentMemoryStorePath(root, agentName, input.StorePath, cfg)

	runtime, err := agentMemoryRuntimeForCommand(cfg, agentName, input)
	if err != nil {
		return err
	}

	store, compactedOnLoad, err := loadAgentMemoryStore(ctx, storePath, input.Migrate, input.Compact, runtime)
	if err != nil {
		return err
	}

	storeChanged, messages, err := mutateAgentMemoryStore(ctx, store, agentName, storePath, input, compactedOnLoad)
	if err != nil {
		return err
	}

	if storeChanged {
		if saveErr := store.Save(storePath); saveErr != nil {
			return fmt.Errorf("agent memory: save store: %w", saveErr)
		}
	}

	for _, message := range messages {
		fmt.Println(message)
	}

	if strings.TrimSpace(input.Search) == "" {
		return nil
	}

	limit := input.Limit
	if limit == 0 {
		limit = 5
	}

	results, err := store.SearchContext(ctx, agentName, input.Search, limit)
	if err != nil {
		return fmt.Errorf("agent memory: search: %w", err)
	}

	if len(results) == 0 {
		fmt.Println("No agent memory results found.")
		return nil
	}

	for i := range results {
		fmt.Println(formatAgentMemoryResult(results[i]))
	}

	return nil
}

func agentMemoryRuntimeForCommand(
	cfg appconfig.VectorConfig,
	agentName string,
	input agentMemoryCommandInput,
) (agentMemoryVectorizerRuntime, error) {
	if !agentMemoryCommandNeedsVectorizer(input) {
		return agentMemoryVectorizerRuntime{}, nil
	}

	return agentMemoryVectorizerRuntimeFromConfig(cfg, agentName)
}

func agentMemoryCommandNeedsAgent(input agentMemoryCommandInput) bool {
	return strings.TrimSpace(input.Search) != "" ||
		strings.TrimSpace(input.DeleteID) != "" ||
		len(input.IndexFiles) > 0
}

func agentMemoryCommandNeedsVectorizer(input agentMemoryCommandInput) bool {
	return strings.TrimSpace(input.Search) != "" ||
		len(input.IndexFiles) > 0 ||
		input.Migrate
}

func mutateAgentMemoryStore(
	ctx context.Context,
	store *agentmemory.Store,
	agentName string,
	storePath string,
	input agentMemoryCommandInput,
	compactedOnLoad int,
) (storeChanged bool, messages []string, err error) {
	messages = make([]string, 0, len(input.IndexFiles)+3)

	if input.Migrate {
		storeChanged = true

		messages = append(messages, "Migrated agent memory store "+memoryDisplayValue(storePath))
	}

	deleted, message := deleteAgentMemoryDocument(store, agentName, storePath, input.DeleteID)
	storeChanged = storeChanged || deleted
	messages = appendAgentMemoryMessage(messages, message)

	compacted, message := compactAgentMemoryDocuments(store, storePath, input.Compact, compactedOnLoad)
	storeChanged = storeChanged || compacted
	messages = appendAgentMemoryMessage(messages, message)

	indexedMessage, err := indexAgentMemoryFiles(ctx, store, agentName, storePath, input.IndexFiles, input.TTLSeconds)
	if err != nil {
		return false, nil, err
	}

	storeChanged = storeChanged || indexedMessage != ""
	messages = appendAgentMemoryMessage(messages, indexedMessage)

	return storeChanged, messages, nil
}

func appendAgentMemoryMessage(messages []string, message string) []string {
	if message == "" {
		return messages
	}

	return append(messages, message)
}

func deleteAgentMemoryDocument(store *agentmemory.Store, agentName, storePath, id string) (changed bool, message string) {
	id = strings.TrimSpace(id)
	if id == "" {
		return false, ""
	}

	if store.Delete(agentName, id) {
		return true, fmt.Sprintf(
			"Deleted agent memory %s for agent %s from %s",
			memoryDisplayValue(id),
			memoryDisplayValue(agentName),
			memoryDisplayValue(storePath),
		)
	}

	return false, fmt.Sprintf(
		"No agent memory %s found for agent %s in %s",
		memoryDisplayValue(id),
		memoryDisplayValue(agentName),
		memoryDisplayValue(storePath),
	)
}

func compactAgentMemoryDocuments(store *agentmemory.Store, storePath string, enabled bool, compactedOnLoad int) (changed bool, message string) {
	if !enabled {
		return false, ""
	}

	removed := compactedOnLoad + store.Compact(time.Now().UTC())
	message = fmt.Sprintf("Compacted %d expired agent memory document(s) from %s", removed, memoryDisplayValue(storePath))

	return true, message
}

func indexAgentMemoryFiles(
	ctx context.Context,
	store *agentmemory.Store,
	agentName, storePath string,
	paths []string,
	ttlSeconds int,
) (string, error) {
	opts := agentMemoryIndexOptions(ttlSeconds)
	for _, path := range paths {
		if addErr := store.AddFileWithOptionsContext(ctx, agentName, path, opts...); addErr != nil {
			return "", fmt.Errorf("agent memory: index %s: %w", path, addErr)
		}
	}

	if len(paths) == 0 {
		return "", nil
	}

	return fmt.Sprintf("Indexed %d file(s) for agent %s in %s", len(paths), memoryDisplayValue(agentName), memoryDisplayValue(storePath)), nil
}

func agentMemoryIndexOptions(ttlSeconds int) []agentmemory.AddOption {
	if ttlSeconds <= 0 {
		return nil
	}

	return []agentmemory.AddOption{agentmemory.WithTTL(time.Duration(ttlSeconds) * time.Second)}
}

func agentMemoryStorePath(root, agentName, explicitPath string, cfg appconfig.VectorConfig) string {
	if path := strings.TrimSpace(explicitPath); path != "" {
		return rootRelativePath(root, path)
	}

	storeConfig := scopedVectorizerConfig(cfg.Stores, agentMemoryVectorStore)
	agentConfig := scopedVectorizerConfig(cfg.Agents, agentName)
	if path := firstNonEmpty(agentConfig.IndexPath, storeConfig.IndexPath); path != "" {
		return rootRelativePath(root, path)
	}

	return filepath.Join(root, ".atteler", "agent-memory.json")
}

func agentMemoryVectorizerRuntimeFromConfig(
	cfg appconfig.VectorConfig,
	agentName string,
) (agentMemoryVectorizerRuntime, error) {
	settings, err := agentMemoryVectorSettings(cfg, agentName)
	if err != nil {
		return agentMemoryVectorizerRuntime{}, err
	}

	if settings.Vectorizer != vector.VectorizerKindEmbedding {
		return agentMemoryVectorizerRuntime{}, nil
	}

	if !workspaceRemoteEmbeddingAllowed(settings.BaseURL, cfg.WorkspaceAllowRemoteEmbeddings) {
		return agentMemoryVectorizerRuntime{}, fmt.Errorf(
			"agent memory: remote embedding endpoint %s is not allowed without vector.workspace_allow_remote_embeddings",
			workspaceDisplayEmbeddingEndpoint(settings.BaseURL),
		)
	}

	vectorizer, spec, err := newAgentMemoryVectorizer(settings)
	if err != nil {
		return agentMemoryVectorizerRuntime{}, err
	}

	return agentMemoryVectorizerRuntime{
		vectorizer: vectorizer,
		spec:       spec,
		configured: true,
	}, nil
}

func agentMemoryVectorSettings(cfg appconfig.VectorConfig, agentName string) (vectorSearchSettings, error) {
	resolved := cfg.ResolveVectorizerConfig(appconfig.VectorScope{
		Store: agentMemoryVectorStore,
		Agent: agentName,
	})
	settings := vectorSearchSettings{
		Vectorizer:     firstNonEmpty(resolved.Vectorizer, vector.VectorizerKindLexical),
		Provider:       firstNonEmpty(resolved.Provider, vectorDefaultProvider),
		Model:          firstNonEmpty(resolved.Model),
		BaseURL:        firstNonEmpty(resolved.BaseURL),
		FallbackPolicy: firstNonEmpty(resolved.FallbackPolicy, vectorFallbackPolicyFail),
		Chunk: vector.ChunkOptions{
			MaxRunes:     resolved.ChunkMaxRunes,
			OverlapRunes: resolved.ChunkOverlapRunes,
		},
	}

	if resolved.TimeoutSeconds > 0 {
		settings.Timeout = time.Duration(resolved.TimeoutSeconds) * time.Second
	}

	settings.Chunk = settings.Chunk.Normalize()

	var err error

	settings.Vectorizer, err = normalizeVectorizerKind(settings.Vectorizer)
	if err != nil {
		return vectorSearchSettings{}, err
	}

	settings.FallbackPolicy, err = normalizeVectorFallbackPolicy(settings.FallbackPolicy)
	if err != nil {
		return vectorSearchSettings{}, err
	}

	return settings, nil
}

func newAgentMemoryVectorizer(settings vectorSearchSettings) (vector.Vectorizer, vector.VectorizerSpec, error) {
	switch settings.Vectorizer {
	case vector.VectorizerKindLexical:
		vectorizer, err := vector.NewTextVectorizer(0)
		if err != nil {
			return nil, vector.VectorizerSpec{}, fmt.Errorf("agent memory: create lexical fallback vectorizer: %w", err)
		}

		return vectorizer, vectorizer.Spec(), nil
	case vector.VectorizerKindEmbedding:
		provider, err := normalizeEmbeddingProvider(settings.Provider)
		if err != nil {
			return nil, vector.VectorizerSpec{}, err
		}

		if provider == "" {
			provider = vectorDefaultProvider
		}

		options := []vector.EmbeddingOption{
			vector.WithEmbeddingProvider(provider),
			vector.WithEmbeddingModel(settings.Model),
			vector.WithEmbeddingBaseURL(settings.BaseURL),
		}
		if settings.Timeout > 0 {
			options = append(options, vector.WithEmbeddingTimeout(settings.Timeout))
		}

		vectorizer := vector.NewEmbeddingVectorizer(options...)
		spec := vectorizer.Spec(0)

		return vectorizer, spec, nil
	default:
		return nil, vector.VectorizerSpec{}, fmt.Errorf("agent memory: unsupported vectorizer %q", settings.Vectorizer)
	}
}

func loadAgentMemoryStore(
	ctx context.Context,
	path string,
	migrate bool,
	countCompacted bool,
	runtime agentMemoryVectorizerRuntime,
) (*agentmemory.Store, int, error) {
	if _, err := os.Stat(path); err != nil {
		store, loadErr := loadMissingAgentMemoryStore(path, migrate, runtime, err)
		return store, 0, loadErr
	}

	loadOptions := agentmemory.LoadOptions{Migrate: migrate}

	var (
		compactedOnLoad int
		store           *agentmemory.Store
		err             error
	)
	if countCompacted && !migrate {
		store, compactedOnLoad, err = agentmemory.LoadAndCompactContext(ctx, path, time.Now().UTC())
	} else {
		store, err = agentmemory.LoadWithOptionsContext(ctx, path, loadOptions)
	}
	if err != nil {
		return nil, 0, fmt.Errorf("agent memory: load store: %w", err)
	}

	if runtime.configured {
		if migrate {
			err = store.MigrateToVectorizerContext(ctx, runtime.spec, runtime.vectorizer)
		} else {
			err = store.SetVectorizer(runtime.spec, runtime.vectorizer)
		}

		if err != nil {
			return nil, 0, fmt.Errorf("agent memory: configure vectorizer: %w", err)
		}
	}

	return store, compactedOnLoad, nil
}

func loadMissingAgentMemoryStore(
	path string,
	migrate bool,
	runtime agentMemoryVectorizerRuntime,
	statErr error,
) (*agentmemory.Store, error) {
	if !errors.Is(statErr, os.ErrNotExist) {
		return nil, fmt.Errorf("agent memory: stat store %s: %w", path, statErr)
	}

	if migrate {
		return nil, fmt.Errorf("agent memory: migrate store %s: %w", path, statErr)
	}

	if runtime.configured {
		store, err := agentmemory.NewStoreWithVectorizer(runtime.spec, runtime.vectorizer)
		if err != nil {
			return nil, fmt.Errorf("agent memory: create embedding store: %w", err)
		}

		return store, nil
	}

	store, err := agentmemory.NewStore(0)
	if err != nil {
		return nil, fmt.Errorf("agent memory: create store: %w", err)
	}

	return store, nil
}

func memoryDisplayValue(value string) string {
	redactor, err := memory.NewRedactor()
	if err != nil {
		return privacy.RedactIdentifier(strings.TrimSpace(value))
	}

	redacted, _ := redactor.RedactIdentifier(strings.TrimSpace(value))

	return redacted
}

func formatAgentMemoryResult(result agentmemory.Result) string {
	parts := []string{
		result.Document.ID,
		fmt.Sprintf("score=%.4f", result.Score),
	}
	if result.Document.Path != "" {
		parts = append(parts, "path="+result.Document.Path)
	}

	if kind := result.Document.Metadata["kind"]; kind != "" {
		parts = append(parts, "kind="+kind)
	}

	return strings.Join(parts, "\t")
}

const (
	memoryPurgeAll   = helpSelectorAll
	memoryAgentKey   = "agent"
	memoryScopeStore = memory.ScopeStore
)

//nolint:govet // Field order follows the scope-resolution flow more than pointer packing.
type memoryCorpusPlan struct {
	since         time.Time
	until         time.Time
	tags          []string
	scope         string
	sessionRef    string
	repoPath      string
	agent         string
	retentionText string
	retentionDays int
	global        bool
	hasSince      bool
	hasUntil      bool
}

type memorySessionCandidate struct {
	saved session.Session
	path  string
}

func runRetrievalCommand(ctx context.Context, state appState, input retrievalCommandInput) error {
	query := strings.TrimSpace(input.Search)
	if query == "" {
		return errors.New("retrieval: --retrieval-search is required")
	}

	limit := input.Limit
	if limit == 0 {
		limit = 5
	}

	sources, err := selectedRetrievalSourcesForState(state, input)
	if err != nil {
		return err
	}

	filters, err := retrievalFilters(input.Filters)
	if err != nil {
		return err
	}

	searchers, err := retrievalSearchers(ctx, state, input, sources)
	if err != nil {
		return err
	}

	if len(searchers) == 0 {
		return errors.New("retrieval: no searchable sources selected")
	}

	results, err := retrieval.Search(ctx, retrieval.Query{
		Text:          query,
		Limit:         limit,
		Filters:       filters,
		Sources:       sources,
		Explain:       input.Explain,
		IncludeUnsafe: input.IncludeUnsafe,
	}, searchers...)
	if err != nil {
		return fmt.Errorf("retrieval: search: %w", err)
	}

	if len(results) == 0 {
		fmt.Println("No retrieval results found.")
		return nil
	}

	for i := range results {
		fmt.Println(formatRetrievalResult(results[i], input.Explain))
	}

	return nil
}

func retrievalFilters(rawFilters []string) (map[string]string, error) {
	if len(rawFilters) == 0 {
		return nil, nil
	}

	filters := make(map[string]string, len(rawFilters))
	for _, raw := range rawFilters {
		key, value, ok := strings.Cut(raw, "=")
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)

		if !ok || key == "" {
			return nil, fmt.Errorf("retrieval: invalid --retrieval-filter %q, want key=value", raw)
		}

		filters[key] = value
	}

	return filters, nil
}

func selectedRetrievalSources(input retrievalCommandInput, includeVectorSource bool) ([]retrieval.SourceType, error) {
	if len(input.Sources) == 0 {
		sources := []retrieval.SourceType{retrieval.SourceMemory, retrieval.SourceFile, retrieval.SourceSession}
		if retrievalExplicitFileVectorIndexRequested(input) {
			sources = append(sources, retrieval.SourceVector)
		}

		return sources, nil
	}

	seen := make(map[retrieval.SourceType]struct{}, len(input.Sources))

	out := make([]retrieval.SourceType, 0, len(input.Sources))
	for _, raw := range input.Sources {
		source, all, err := parseRetrievalSource(raw)
		if err != nil {
			return nil, err
		}

		if all {
			return allRetrievalSources(input, includeVectorSource), nil
		}

		if _, ok := seen[source]; ok {
			continue
		}

		seen[source] = struct{}{}
		out = append(out, source)
	}

	return out, nil
}

func selectedRetrievalSourcesForState(state appState, input retrievalCommandInput) ([]retrieval.SourceType, error) {
	includeVector := workspaceVectorEnabled(state.vectorConfig) ||
		retrievalReusableFileVectorIndexExists(state.cwd, state.vectorConfig, input)

	return selectedRetrievalSources(input, includeVector)
}

func retrievalReusableFileVectorIndexExists(root string, cfg appconfig.VectorConfig, input retrievalCommandInput) bool {
	if retrievalExplicitFileVectorIndexRequested(input) {
		return true
	}

	if !retrievalSourceAllRequested(input.Sources) {
		return false
	}

	settings, err := vectorSearchSettingsFromOptions(root, cfg, cliOptions{})
	if err != nil {
		return false
	}

	if settings.Vectorizer == vector.VectorizerKindEmbedding &&
		!workspaceRemoteEmbeddingAllowed(settings.BaseURL, cfg.WorkspaceAllowRemoteEmbeddings) {
		if settings.FallbackPolicy != vector.VectorizerKindLexical {
			return false
		}

		settings = lexicalVectorFallbackSettings(settings)
	}

	_, metadata, err := newVectorSearchVectorizer(settings)
	if err != nil {
		return false
	}

	idx, err := vector.LoadIndex(settings.IndexPath)
	if err != nil {
		return false
	}

	reuse, _, err := reusableVectorIndex(idx, metadata, settings.Chunk, settings.IndexPath, nil)

	return err == nil && reuse
}

func parseRetrievalSource(raw string) (retrieval.SourceType, bool, error) {
	switch strings.ToLower(strings.TrimSpace(strings.ReplaceAll(raw, "-", "_"))) {
	case retrievalSourceAll:
		return "", true, nil
	case "memory", "mem":
		return retrieval.SourceMemory, false, nil
	case "file", string(codeIntelTextFiles):
		return retrieval.SourceFile, false, nil
	case retrievalSourceSession, "sessions":
		return retrieval.SourceSession, false, nil
	case "git", "history", "git_history", "githistory":
		return retrieval.SourceGitHistory, false, nil
	case "adr", "adrs", "architecture_decision_record", "architecture_decision_records", "decision", "decisions":
		return retrieval.SourceADR, false, nil
	case "vector", "vectors":
		return retrieval.SourceVector, false, nil
	case memoryAgentKey, "agent_memory", "agentmemory":
		return retrieval.SourceAgentMemory, false, nil
	default:
		return "", false, fmt.Errorf("retrieval: unknown source %q", raw)
	}
}

func allRetrievalSources(input retrievalCommandInput, includeVectorSource bool) []retrieval.SourceType {
	sources := []retrieval.SourceType{
		retrieval.SourceMemory,
		retrieval.SourceFile,
		retrieval.SourceSession,
		retrieval.SourceGitHistory,
		retrieval.SourceADR,
	}
	if retrievalExplicitFileVectorIndexRequested(input) || includeVectorSource {
		sources = append(sources, retrieval.SourceVector)
	}

	if strings.TrimSpace(input.AgentMemoryAgent) != "" || strings.TrimSpace(input.AgentName) != "" || strings.TrimSpace(input.AgentMemoryStorePath) != "" {
		sources = append(sources, retrieval.SourceAgentMemory)
	}

	return sources
}

func retrievalSearchers(
	ctx context.Context,
	state appState,
	input retrievalCommandInput,
	sources []retrieval.SourceType,
) ([]retrieval.Searcher, error) {
	searchers := make([]retrieval.Searcher, 0, len(sources))

	sourceSet := retrievalSourceSet(sources)
	if sourceSet[retrieval.SourceMemory] || sourceSet[retrieval.SourceFile] {
		searcher, err := buildRetrievalMemoryStore(state.sessionStore, input, !sourceSet[retrieval.SourceSession])
		if err != nil {
			return nil, err
		}

		searchers = append(searchers, searcher)
	}

	for _, source := range sources {
		if source == retrieval.SourceMemory || source == retrieval.SourceFile {
			continue
		}

		searcher, err := retrievalSearcher(ctx, state, input, source)
		if err != nil {
			if shouldSkipRetrievalSourceError(state, input, sources, source, err) {
				continue
			}

			return nil, err
		}

		if searcher != nil {
			searchers = append(searchers, searcher)
		}
	}

	return searchers, nil
}

func shouldSkipRetrievalSourceError(
	state appState,
	input retrievalCommandInput,
	sources []retrieval.SourceType,
	source retrieval.SourceType,
	err error,
) bool {
	return shouldSkipEmptyWorkspaceVectorSource(input, sources, source, err) ||
		shouldSkipUnavailableGitHistorySource(input, sources, source, err) ||
		shouldSkipImplicitFileVectorSourceError(state, input, sources, source)
}

func shouldSkipEmptyWorkspaceVectorSource(
	input retrievalCommandInput,
	sources []retrieval.SourceType,
	source retrieval.SourceType,
	err error,
) bool {
	return source == retrieval.SourceVector &&
		len(sources) > 1 &&
		!retrievalExplicitFileVectorIndexRequested(input) &&
		retrievalSourceAllRequested(input.Sources) &&
		errors.Is(err, vector.ErrNoSources)
}

func shouldSkipImplicitFileVectorSourceError(
	state appState,
	input retrievalCommandInput,
	sources []retrieval.SourceType,
	source retrieval.SourceType,
) bool {
	return source == retrieval.SourceVector &&
		len(sources) > 1 &&
		retrievalSourceAllRequested(input.Sources) &&
		!retrievalExplicitFileVectorIndexRequested(input) &&
		!workspaceVectorEnabled(state.vectorConfig)
}

func shouldSkipUnavailableGitHistorySource(
	input retrievalCommandInput,
	sources []retrieval.SourceType,
	source retrieval.SourceType,
	err error,
) bool {
	return source == retrieval.SourceGitHistory &&
		len(sources) > 1 &&
		retrievalSourceAllRequested(input.Sources) &&
		err != nil &&
		strings.Contains(err.Error(), "git history: run git log:")
}

func retrievalSourceAllRequested(sources []string) bool {
	for _, raw := range sources {
		_, all, err := parseRetrievalSource(raw)
		if err == nil && all {
			return true
		}
	}

	return false
}

func retrievalExplicitFileVectorIndexRequested(input retrievalCommandInput) bool {
	return len(input.VectorIndexFiles) > 0 || strings.TrimSpace(input.Vector.StorePath) != ""
}

func retrievalSourceSet(sources []retrieval.SourceType) map[retrieval.SourceType]bool {
	out := make(map[retrieval.SourceType]bool, len(sources))
	for _, source := range sources {
		out[source] = true
	}

	return out
}

func retrievalSearcher(ctx context.Context, state appState, input retrievalCommandInput, source retrieval.SourceType) (retrieval.Searcher, error) {
	switch source {
	case retrieval.SourceSession:
		requested, err := sourceVectorIndexRequested(state.vectorConfig, vector.SourceKindSession)
		if err != nil {
			return nil, err
		}

		if requested {
			return buildSessionVectorRetrievalSearcher(ctx, state)
		}

		return state.sessionStore, nil
	case retrieval.SourceGitHistory:
		logText, err := gitHistoryLog(ctx, state.cwd)
		if err != nil {
			return nil, err
		}

		commits, err := githistory.ParseLog(logText)
		if err != nil {
			return nil, fmt.Errorf("git history: parse log: %w", err)
		}

		requested, err := sourceVectorIndexRequested(state.vectorConfig, vector.SourceKindGitHistory)
		if err != nil {
			return nil, err
		}

		if requested {
			return buildGitHistoryVectorRetrievalSearcher(ctx, state, commits)
		}

		return githistory.NewIndex(commits), nil
	case retrieval.SourceADR:
		return buildADRVectorRetrievalSearcher(ctx, state)
	case retrieval.SourceVector:
		return buildVectorRetrievalSearcher(ctx, state, input)
	case retrieval.SourceAgentMemory:
		return buildAgentMemoryRetrievalSearcher(ctx, state.cwd, state.selectedAgent, state.vectorConfig, input)
	default:
		return nil, fmt.Errorf("retrieval: unsupported source %q", source)
	}
}

func sourceVectorIndexRequested(cfg appconfig.VectorConfig, sourceKind string) (bool, error) {
	settings, err := sourceVectorSettings("", cfg, sourceKind, "")
	if err != nil {
		return false, err
	}

	if settings.Vectorizer == vector.VectorizerKindEmbedding {
		return true, nil
	}

	scoped := scopedVectorizerConfig(cfg.Sources, sourceKind)

	return vectorizerConfigExplicit(scoped), nil
}

func vectorizerConfigExplicit(cfg appconfig.VectorizerConfig) bool {
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

func buildSessionVectorRetrievalSearcher(ctx context.Context, state appState) (retrieval.Searcher, error) {
	settings, err := sourceVectorSettings(state.cwd, state.vectorConfig, vector.SourceKindSession, sourceVectorSessionIndex)
	if err != nil {
		return nil, err
	}

	sources, err := sessionVectorSources(state.sessionStore)
	if err != nil {
		return nil, err
	}

	if len(sources) == 0 {
		clearErr := clearEmptySourceVectorIndexes(ctx, settings, vector.SourceKindSession)
		if clearErr != nil {
			return nil, clearErr
		}

		return nil, nil
	}

	source := sessionVectorRetrievalSource(state.cwd, settings)
	if settings.Vectorizer == vector.VectorizerKindEmbedding &&
		!workspaceRemoteEmbeddingAllowed(settings.BaseURL, state.vectorConfig.WorkspaceAllowRemoteEmbeddings) {
		if settings.FallbackPolicy == vector.VectorizerKindLexical {
			return buildSourceVectorRetrievalSearcherWithLexicalFallback(ctx, state.cwd, settings, vector.SourceKindSession, source, sources)
		}

		return nil, fmt.Errorf("retrieval: remote session embedding endpoint %s is not allowed without vector.workspace_allow_remote_embeddings", workspaceDisplayEmbeddingEndpoint(settings.BaseURL))
	}

	searcher, err := buildSourceVectorRetrievalSearcher(ctx, settings, vector.SourceKindSession, source, sources)
	if err == nil ||
		settings.Vectorizer != vector.VectorizerKindEmbedding ||
		settings.FallbackPolicy != vector.VectorizerKindLexical ||
		!vectorSearchEmbeddingFailure(err) {
		return searcher, err
	}

	return buildSourceVectorRetrievalSearcherWithLexicalFallback(ctx, state.cwd, settings, vector.SourceKindSession, source, sources)
}

func buildGitHistoryVectorRetrievalSearcher(
	ctx context.Context,
	state appState,
	commits []githistory.Commit,
) (retrieval.Searcher, error) {
	settings, err := sourceVectorSettings(state.cwd, state.vectorConfig, vector.SourceKindGitHistory, sourceVectorGitHistoryIndex)
	if err != nil {
		return nil, err
	}

	sources := gitHistoryVectorSources(commits)
	if len(sources) == 0 {
		clearErr := clearEmptySourceVectorIndexes(ctx, settings, vector.SourceKindGitHistory)
		if clearErr != nil {
			return nil, clearErr
		}

		return nil, nil
	}

	source := gitHistoryVectorRetrievalSource(state.cwd, settings)
	if settings.Vectorizer == vector.VectorizerKindEmbedding &&
		!workspaceRemoteEmbeddingAllowed(settings.BaseURL, state.vectorConfig.WorkspaceAllowRemoteEmbeddings) {
		if settings.FallbackPolicy == vector.VectorizerKindLexical {
			return buildSourceVectorRetrievalSearcherWithLexicalFallback(ctx, state.cwd, settings, vector.SourceKindGitHistory, source, sources)
		}

		return nil, fmt.Errorf("retrieval: remote git-history embedding endpoint %s is not allowed without vector.workspace_allow_remote_embeddings", workspaceDisplayEmbeddingEndpoint(settings.BaseURL))
	}

	searcher, err := buildSourceVectorRetrievalSearcher(ctx, settings, vector.SourceKindGitHistory, source, sources)
	if err == nil ||
		settings.Vectorizer != vector.VectorizerKindEmbedding ||
		settings.FallbackPolicy != vector.VectorizerKindLexical ||
		!vectorSearchEmbeddingFailure(err) {
		return searcher, err
	}

	return buildSourceVectorRetrievalSearcherWithLexicalFallback(ctx, state.cwd, settings, vector.SourceKindGitHistory, source, sources)
}

func buildADRVectorRetrievalSearcher(ctx context.Context, state appState) (retrieval.Searcher, error) {
	settings, err := sourceVectorSettings(state.cwd, state.vectorConfig, vector.SourceKindADR, sourceVectorADRIndex)
	if err != nil {
		return nil, err
	}

	sources, err := adrVectorSources(ctx, state.cwd)
	if err != nil {
		return nil, err
	}

	if len(sources) == 0 {
		if clearErr := clearEmptySourceVectorIndexes(ctx, settings, vector.SourceKindADR); clearErr != nil {
			return nil, clearErr
		}

		return nil, nil
	}

	source := adrVectorRetrievalSource(state.cwd, settings)
	if settings.Vectorizer == vector.VectorizerKindEmbedding &&
		!workspaceRemoteEmbeddingAllowed(settings.BaseURL, state.vectorConfig.WorkspaceAllowRemoteEmbeddings) {
		if settings.FallbackPolicy == vector.VectorizerKindLexical {
			return buildSourceVectorRetrievalSearcherWithLexicalFallback(ctx, state.cwd, settings, vector.SourceKindADR, source, sources)
		}

		return nil, fmt.Errorf("retrieval: remote adr embedding endpoint %s is not allowed without vector.workspace_allow_remote_embeddings", workspaceDisplayEmbeddingEndpoint(settings.BaseURL))
	}

	searcher, err := buildSourceVectorRetrievalSearcher(ctx, settings, vector.SourceKindADR, source, sources)
	if err == nil ||
		settings.Vectorizer != vector.VectorizerKindEmbedding ||
		settings.FallbackPolicy != vector.VectorizerKindLexical ||
		!vectorSearchEmbeddingFailure(err) {
		return searcher, err
	}

	return buildSourceVectorRetrievalSearcherWithLexicalFallback(ctx, state.cwd, settings, vector.SourceKindADR, source, sources)
}

func sessionVectorRetrievalSource(root string, settings vectorSearchSettings) retrieval.Source {
	return retrieval.Source{
		Type: retrieval.SourceSession,
		Name: "session-vector-index",
		URI:  sourceVectorIndexURI(root, settings.IndexPath),
	}
}

func gitHistoryVectorRetrievalSource(root string, settings vectorSearchSettings) retrieval.Source {
	return retrieval.Source{
		Type: retrieval.SourceGitHistory,
		Name: "git-history-vector-index",
		URI:  sourceVectorIndexURI(root, settings.IndexPath),
	}
}

func adrVectorRetrievalSource(root string, settings vectorSearchSettings) retrieval.Source {
	return retrieval.Source{
		Type: retrieval.SourceADR,
		Name: "adr-vector-index",
		URI:  sourceVectorIndexURI(root, settings.IndexPath),
	}
}

func lexicalSourceVectorFallbackSettings(settings vectorSearchSettings) vectorSearchSettings {
	return lexicalVectorFallbackSettings(settings)
}

func sourceVectorSettings(
	cwd string,
	cfg appconfig.VectorConfig,
	sourceKind string,
	defaultIndexPath string,
) (vectorSearchSettings, error) {
	resolved := cfg.ResolveVectorizerConfig(appconfig.VectorScope{Source: sourceKind})
	sourceConfig := scopedVectorizerConfig(cfg.Sources, sourceKind)

	settings := vectorSearchSettings{
		Vectorizer:     firstNonEmpty(resolved.Vectorizer, vector.VectorizerKindLexical),
		Provider:       firstNonEmpty(resolved.Provider, vectorDefaultProvider),
		Model:          firstNonEmpty(resolved.Model),
		BaseURL:        firstNonEmpty(resolved.BaseURL),
		FallbackPolicy: firstNonEmpty(resolved.FallbackPolicy, vectorFallbackPolicyFail),
		IndexPath:      firstNonEmpty(sourceConfig.IndexPath, defaultIndexPath),
		Chunk: vector.ChunkOptions{
			MaxRunes:     resolved.ChunkMaxRunes,
			OverlapRunes: resolved.ChunkOverlapRunes,
		},
	}

	settings.IndexPath = rootRelativePath(cwd, settings.IndexPath)

	if resolved.TimeoutSeconds > 0 {
		settings.Timeout = time.Duration(resolved.TimeoutSeconds) * time.Second
	}

	settings.Chunk = settings.Chunk.Normalize()

	var err error

	settings.Vectorizer, err = normalizeVectorizerKind(settings.Vectorizer)
	if err != nil {
		return vectorSearchSettings{}, err
	}

	settings.FallbackPolicy, err = normalizeVectorFallbackPolicy(settings.FallbackPolicy)
	if err != nil {
		return vectorSearchSettings{}, err
	}

	return settings, nil
}

func clearEmptySourceVectorIndexes(ctx context.Context, settings vectorSearchSettings, sourceKind string) error {
	if err := clearEmptySourceVectorIndex(ctx, settings, sourceKind); err != nil {
		return err
	}

	if settings.Vectorizer != vector.VectorizerKindEmbedding ||
		settings.FallbackPolicy != vector.VectorizerKindLexical {
		return nil
	}

	return clearEmptySourceVectorIndex(ctx, lexicalSourceVectorFallbackSettings(settings), sourceKind)
}

func clearEmptySourceVectorIndex(ctx context.Context, settings vectorSearchSettings, sourceKind string) error {
	if err := validateEmptySourceVectorIndexMayBeCleared(settings.IndexPath, sourceKind); err != nil {
		return err
	}

	_, err := vector.RefreshSourceIndex(ctx, vector.SourceIndexOptions{
		IndexPath: settings.IndexPath,
		Sources:   nil,
		Chunk:     settings.Chunk,
	})
	if err == nil || errors.Is(err, vector.ErrNoSources) {
		return nil
	}

	return fmt.Errorf("retrieval: clear %s vector index: %w", sourceKind, err)
}

func validateEmptySourceVectorIndexMayBeCleared(indexPath, sourceKind string) error {
	return validateSourceVectorIndexSourceKind(indexPath, sourceKind, "clear")
}

func validateSourceVectorIndexMayBeRefreshed(indexPath, sourceKind string) error {
	return validateSourceVectorIndexSourceKind(indexPath, sourceKind, "refresh")
}

func validateSourceVectorIndexSourceKind(indexPath, sourceKind, action string) error {
	if strings.TrimSpace(indexPath) == "" {
		return nil
	}

	sources, ok, err := sourceVectorIndexGuardSources(indexPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}

		// Let RefreshSourceIndex decide whether a stale/corrupt managed vector
		// index can be rebuilt or removed. This guard only prevents replacing an
		// index whose persisted source metadata proves it belongs to another
		// source family, even when strict vector validation would require a
		// rebuild.
		return nil
	}

	if !ok {
		return nil
	}

	expectedKind := normalizeVectorSourceKindForCompare(sourceKind)
	for _, source := range sources {
		actualKind := normalizeVectorSourceKindForCompare(source.Kind)
		if actualKind != expectedKind {
			return fmt.Errorf(
				"source vector index: refusing to %s %s vector index %s: contains %s source %q",
				action,
				expectedKind,
				indexPath,
				actualKind,
				source.Path,
			)
		}
	}

	return nil
}

func sourceVectorIndexGuardSources(indexPath string) ([]vector.SourceMetadata, bool, error) {
	data, err := os.ReadFile(indexPath)
	if err != nil {
		return nil, false, fmt.Errorf("read source vector index guard %s: %w", indexPath, err)
	}

	var fields struct {
		Vectorizer json.RawMessage `json:"vectorizer"`
		Chunk      json.RawMessage `json:"chunk"`
		Sources    json.RawMessage `json:"sources"`
		Documents  json.RawMessage `json:"documents"`
		Version    int             `json:"version"`
	}
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, false, fmt.Errorf("decode source vector index guard %s: %w", indexPath, err)
	}

	if fields.Version <= 0 ||
		!sourceVectorIndexGuardJSONFieldIsObject(fields.Vectorizer) ||
		!sourceVectorIndexGuardJSONFieldIsObject(fields.Chunk) ||
		!sourceVectorIndexGuardJSONFieldIsArray(fields.Sources) ||
		!sourceVectorIndexGuardJSONFieldIsArray(fields.Documents) {
		return nil, false, nil
	}

	var sources []vector.SourceMetadata
	if err := json.Unmarshal(fields.Sources, &sources); err != nil {
		return nil, true, fmt.Errorf("decode source vector index guard sources %s: %w", indexPath, err)
	}

	return sources, true, nil
}

func sourceVectorIndexGuardJSONFieldIsObject(raw json.RawMessage) bool {
	var value map[string]json.RawMessage
	if err := json.Unmarshal(raw, &value); err != nil {
		return false
	}

	return value != nil
}

func sourceVectorIndexGuardJSONFieldIsArray(raw json.RawMessage) bool {
	var value []json.RawMessage

	return json.Unmarshal(raw, &value) == nil
}

func normalizeVectorSourceKindForCompare(kind string) string {
	kind = strings.ToLower(strings.TrimSpace(kind))
	kind = strings.ReplaceAll(kind, "-", "_")
	kind = strings.ReplaceAll(kind, " ", "_")
	if kind == "" {
		return vector.SourceKindFile
	}

	return kind
}

func buildSourceVectorRetrievalSearcherWithLexicalFallback(
	ctx context.Context,
	root string,
	settings vectorSearchSettings,
	sourceKind string,
	source retrieval.Source,
	sources []vector.Source,
) (retrieval.Searcher, error) {
	fallbackSettings := lexicalSourceVectorFallbackSettings(settings)
	source.URI = sourceVectorIndexURI(root, fallbackSettings.IndexPath)

	return buildSourceVectorRetrievalSearcher(ctx, fallbackSettings, sourceKind, source, sources)
}

func buildSourceVectorRetrievalSearcher(
	ctx context.Context,
	settings vectorSearchSettings,
	sourceKind string,
	source retrieval.Source,
	sources []vector.Source,
) (retrieval.Searcher, error) {
	if err := validateSourceVectorIndexMayBeRefreshed(settings.IndexPath, sourceKind); err != nil {
		return nil, err
	}

	vectorizer, metadata, err := newVectorSearchVectorizer(settings)
	if err != nil {
		return nil, err
	}

	refresh, err := vector.RefreshSourceIndex(ctx, vector.SourceIndexOptions{
		IndexPath:          settings.IndexPath,
		Sources:            sources,
		Vectorizer:         vectorizer,
		VectorizerMetadata: metadata,
		Chunk:              settings.Chunk,
	})
	if err != nil {
		return nil, fmt.Errorf("retrieval: refresh %s vector index: %w", sourceKind, err)
	}

	return newVectorIndexRetrievalSearcher(
		refresh.Index,
		vectorizer,
		source,
		settings.Vectorizer+"-"+strings.ReplaceAll(sourceKind, "_", "-")+"-ann",
	)
}

func sessionVectorSources(store *session.Store) ([]vector.Source, error) {
	if store == nil {
		return nil, nil
	}

	summaries, err := store.List()
	if err != nil {
		return nil, fmt.Errorf("retrieval: list sessions for vector index: %w", err)
	}

	sources := make([]vector.Source, 0, len(summaries))
	for i := range summaries {
		summary := &summaries[i]
		saved, err := store.Load(summary.Path)
		if err != nil {
			return nil, fmt.Errorf("retrieval: load session %s for vector index: %w", summary.ID, err)
		}

		text := sessionVectorText(saved)
		if strings.TrimSpace(text) == "" {
			continue
		}

		metadata := map[string]string{
			"session_id": saved.ID,
		}
		if !saved.CreatedAt.IsZero() {
			metadata["created_at"] = saved.CreatedAt.UTC().Format(time.RFC3339)
		}

		if !saved.UpdatedAt.IsZero() {
			updatedAt := saved.UpdatedAt.UTC()
			metadata["updated_at"] = updatedAt.Format(time.RFC3339)
			metadata[retrieval.MetadataSourceUpdatedAt] = updatedAt.Format(time.RFC3339Nano)
		}

		if saved.Title != "" {
			metadata["title"] = saved.Title
		}

		if saved.DefaultAgent != "" {
			metadata["agent"] = saved.DefaultAgent
		}

		if saved.DefaultModel != "" {
			metadata["model"] = saved.DefaultModel
		}

		if len(saved.Tags) > 0 {
			metadata["tags"] = strings.Join(saved.Tags, ",")
		}

		sources = append(sources, vector.Source{
			Kind:       vector.SourceKindSession,
			Path:       "sessions/" + saved.ID,
			Text:       text,
			Metadata:   metadata,
			Provenance: map[string]string{"session_id": saved.ID},
		})
	}

	return sources, nil
}

func sessionVectorText(saved session.Session) string {
	var b strings.Builder

	writeSourceVectorLine(&b, "session", saved.ID)
	writeSourceVectorLine(&b, "title", saved.Title)
	writeSourceVectorLine(&b, "agent", saved.DefaultAgent)
	writeSourceVectorLine(&b, "model", saved.DefaultModel)
	if len(saved.Tags) > 0 {
		writeSourceVectorLine(&b, "tags", strings.Join(saved.Tags, ", "))
	}

	for i, message := range saved.Messages {
		content := strings.TrimSpace(message.Content)
		if content == "" {
			continue
		}

		writeSourceVectorLine(&b, fmt.Sprintf("message[%d].%s", i, message.Role), content)
	}

	for i, item := range saved.NegativeKnowledge {
		writeSourceVectorLine(&b, fmt.Sprintf("negative[%d].approach", i), item.Approach)
		writeSourceVectorLine(&b, fmt.Sprintf("negative[%d].reason", i), item.Reason)
	}

	for i := range saved.Evaluations {
		evaluation := &saved.Evaluations[i]
		writeSourceVectorLine(&b, fmt.Sprintf("evaluation[%d].agent", i), evaluation.Agent)
		writeSourceVectorLine(&b, fmt.Sprintf("evaluation[%d].outcome", i), evaluation.Outcome)
		writeSourceVectorLine(&b, fmt.Sprintf("evaluation[%d].notes", i), evaluation.Notes)
	}

	for i := range saved.Artifacts {
		artifact := &saved.Artifacts[i]
		writeSourceVectorLine(&b, fmt.Sprintf("artifact[%d].kind", i), artifact.Kind)
		writeSourceVectorLine(&b, fmt.Sprintf("artifact[%d].path", i), artifact.Path)
		writeSourceVectorLine(&b, fmt.Sprintf("artifact[%d].summary", i), artifact.Summary)
	}

	return b.String()
}

func gitHistoryVectorSources(commits []githistory.Commit) []vector.Source {
	sources := make([]vector.Source, 0, len(commits))
	for i := range commits {
		commit := &commits[i]
		if strings.TrimSpace(commit.Hash) == "" {
			continue
		}

		text := gitHistoryVectorText(*commit)
		if strings.TrimSpace(text) == "" {
			continue
		}

		metadata := map[string]string{
			"commit": commit.Hash,
		}
		if commit.Subject != "" {
			metadata["subject"] = commit.Subject
		}

		if commit.AuthorName != "" {
			metadata["author_name"] = commit.AuthorName
		}

		if !commit.Date.IsZero() {
			updatedAt := commit.Date.UTC()
			metadata["date"] = updatedAt.Format(time.RFC3339)
			metadata[retrieval.MetadataSourceUpdatedAt] = updatedAt.Format(time.RFC3339Nano)
		}

		sources = append(sources, vector.Source{
			Kind:       vector.SourceKindGitHistory,
			Path:       "git/" + commit.Hash,
			Text:       text,
			Metadata:   metadata,
			Provenance: map[string]string{"commit": commit.Hash},
		})
	}

	return sources
}

func gitHistoryVectorText(commit githistory.Commit) string {
	var b strings.Builder

	writeSourceVectorLine(&b, "commit", commit.Hash)
	writeSourceVectorLine(&b, "author", strings.TrimSpace(strings.TrimSpace(commit.AuthorName)+" "+strings.TrimSpace(commit.AuthorEmail)))
	if !commit.Date.IsZero() {
		writeSourceVectorLine(&b, "date", commit.Date.UTC().Format(time.RFC3339))
	}

	writeSourceVectorLine(&b, "subject", commit.Subject)
	writeSourceVectorLine(&b, "body", commit.Body)
	if len(commit.Files) > 0 {
		writeSourceVectorLine(&b, "files", strings.Join(commit.Files, "\n"))
	}

	return b.String()
}

func adrVectorSources(ctx context.Context, root string) ([]vector.Source, error) {
	root = cleanADRSourceRoot(root)

	seen := make(map[string]struct{})
	sources := make([]vector.Source, 0)
	for _, dir := range adrSourceDirectories {
		if err := appendADRVectorSourcesFromDir(ctx, root, dir, seen, &sources); err != nil {
			return nil, err
		}
	}

	sort.SliceStable(sources, func(i, j int) bool {
		return sources[i].Path < sources[j].Path
	})

	return sources, nil
}

func cleanADRSourceRoot(root string) string {
	root = strings.TrimSpace(root)
	if root == "" {
		root = "."
	}

	return filepath.Clean(root)
}

func appendADRVectorSourcesFromDir(
	ctx context.Context,
	root string,
	dir string,
	seen map[string]struct{},
	sources *[]vector.Source,
) error {
	absoluteDir := filepath.Join(root, filepath.FromSlash(dir))
	if ok, err := adrVectorSourceDirExists(absoluteDir); err != nil || !ok {
		return err
	}

	if err := filepath.WalkDir(absoluteDir, func(path string, entry fs.DirEntry, walkErr error) error {
		return appendADRVectorSourceFromWalkEntry(ctx, root, path, entry, walkErr, seen, sources)
	}); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("retrieval: discover ADR sources canceled: %w", err)
		}

		return fmt.Errorf("retrieval: discover ADR sources in %s: %w", absoluteDir, err)
	}

	return nil
}

func adrVectorSourceDirExists(absoluteDir string) (bool, error) {
	info, err := os.Stat(absoluteDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}

		return false, fmt.Errorf("retrieval: inspect ADR directory %s: %w", absoluteDir, err)
	}

	return info.IsDir(), nil
}

func appendADRVectorSourceFromWalkEntry(
	ctx context.Context,
	root string,
	path string,
	entry fs.DirEntry,
	walkErr error,
	seen map[string]struct{},
	sources *[]vector.Source,
) error {
	if walkErr != nil {
		return walkErr
	}

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("ADR source discovery context: %w", err)
	}

	if entry.Type()&os.ModeSymlink != 0 {
		return nil
	}

	if entry.IsDir() || !adrVectorSourceFile(path) {
		return nil
	}

	return appendADRVectorSource(root, path, seen, sources)
}

func appendADRVectorSource(
	root string,
	path string,
	seen map[string]struct{},
	sources *[]vector.Source,
) error {
	relPath, err := filepath.Rel(root, path)
	if err != nil {
		return fmt.Errorf("compute ADR source path for %s: %w", path, err)
	}
	relPath = filepath.ToSlash(filepath.Clean(relPath))
	if _, ok := seen[relPath]; ok {
		return nil
	}
	seen[relPath] = struct{}{}

	source, err := vector.SourceFromFile(path)
	if err != nil {
		return fmt.Errorf("read ADR source %s: %w", relPath, err)
	}

	adrID := strings.TrimSuffix(filepath.Base(relPath), filepath.Ext(relPath))
	source.Kind = vector.SourceKindADR
	source.Path = relPath
	metadata := maps.Clone(source.Metadata)
	if metadata == nil {
		metadata = make(map[string]string, 3)
	}
	metadata["adr_id"] = adrID
	metadata["path"] = relPath
	source.Metadata = metadata
	source.Provenance = map[string]string{
		"adr_id": adrID,
	}
	*sources = append(*sources, source)

	return nil
}

func adrVectorSourceFile(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".adoc", ".md", ".markdown", ".rst", ".txt":
		return true
	default:
		return false
	}
}

func writeSourceVectorLine(b *strings.Builder, label, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}

	b.WriteString(label)
	b.WriteString(": ")
	b.WriteString(value)
	b.WriteString("\n")
}

func sourceVectorIndexURI(root, indexPath string) string {
	if display := workspaceDisplayIndexPath(root, indexPath); display != "" {
		return display
	}

	return filepath.ToSlash(privacy.RedactIdentifier(indexPath))
}

func buildRetrievalMemoryStore(store *session.Store, input retrievalCommandInput, includeSessions bool) (*memory.Store, error) {
	mem, err := loadMemoryStore(input.MemoryStorePath)
	if err != nil {
		return nil, err
	}

	if len(input.MemoryIndexFiles) > 0 {
		if err := mem.AddFiles(input.MemoryIndexFiles...); err != nil {
			return nil, fmt.Errorf("retrieval: index memory files: %w", err)
		}
	}

	if includeSessions && strings.TrimSpace(input.MemoryStorePath) == "" && len(input.MemoryIndexFiles) == 0 {
		if err := addRetrievalSessionMemory(mem, store); err != nil {
			return nil, err
		}
	}

	return mem, nil
}

func buildVectorRetrievalSearcher(ctx context.Context, state appState, input retrievalCommandInput) (retrieval.Searcher, error) {
	paths := input.VectorIndexFiles
	if !retrievalExplicitFileVectorIndexRequested(input) && workspaceVectorEnabled(state.vectorConfig) {
		return buildWorkspaceVectorRetrievalSearcher(ctx, state)
	}

	settings, err := vectorSearchSettingsFromOptions(state.cwd, state.vectorConfig, vectorSearchOptionsFromRetrievalInput(input))
	if err != nil {
		return nil, err
	}

	if settings.Vectorizer == vector.VectorizerKindEmbedding &&
		!workspaceRemoteEmbeddingAllowed(settings.BaseURL, state.vectorConfig.WorkspaceAllowRemoteEmbeddings) {
		if settings.FallbackPolicy == vector.VectorizerKindLexical {
			return buildFileVectorRetrievalSearcherOnce(
				ctx,
				state.cwd,
				lexicalVectorFallbackSettings(settings),
				input.Search,
				paths,
			)
		}

		return nil, fmt.Errorf("retrieval: remote file embedding endpoint %s is not allowed without vector.workspace_allow_remote_embeddings", workspaceDisplayEmbeddingEndpoint(settings.BaseURL))
	}

	searcher, err := buildFileVectorRetrievalSearcherOnce(ctx, state.cwd, settings, input.Search, paths)
	if err == nil ||
		settings.Vectorizer != vector.VectorizerKindEmbedding ||
		settings.FallbackPolicy != vector.VectorizerKindLexical ||
		!vectorSearchEmbeddingFailure(err) {
		return searcher, err
	}

	return buildFileVectorRetrievalSearcherOnce(
		ctx,
		state.cwd,
		lexicalVectorFallbackSettings(settings),
		input.Search,
		paths,
	)
}

func buildFileVectorRetrievalSearcherOnce(
	ctx context.Context,
	root string,
	settings vectorSearchSettings,
	query string,
	paths []string,
) (retrieval.Searcher, error) {
	vectorizer, metadata, err := newVectorSearchVectorizer(settings)
	if err != nil {
		return nil, err
	}

	idx, _, err := loadOrBuildVectorIndex(ctx, settings, paths, vectorizer, metadata)
	if err != nil {
		return nil, fmt.Errorf("retrieval: load or build vector index: %w", err)
	}

	if err := validateFileVectorRetrievalQuery(ctx, settings, idx, vectorizer, query); err != nil {
		return nil, err
	}

	return newVectorIndexRetrievalSearcher(
		idx,
		vectorizer,
		retrieval.Source{
			Type: retrieval.SourceVector,
			Name: "local-vector-index",
			URI:  sourceVectorIndexURI(root, settings.IndexPath),
		},
		settings.Vectorizer+"-file-vector-index",
	)
}

func validateFileVectorRetrievalQuery(
	ctx context.Context,
	settings vectorSearchSettings,
	idx *vector.Index,
	vectorizer vector.Vectorizer,
	query string,
) error {
	query = strings.TrimSpace(query)
	if query == "" || idx == nil {
		return nil
	}

	queryVector, err := vectorizeSearchText(ctx, vectorizer, query)
	if err != nil {
		return fmt.Errorf("retrieval: vectorize query for %s: %w", settings.IndexPath, err)
	}

	if len(queryVector) != idx.Dimensions {
		return fmt.Errorf(
			"retrieval: reusable index %s is invalid: %w: query has %d dimensions, index has %d; pass --vector-index to rebuild",
			settings.IndexPath,
			vector.ErrDimensionMismatch,
			len(queryVector),
			idx.Dimensions,
		)
	}

	return nil
}

func vectorSearchOptionsFromRetrievalInput(input retrievalCommandInput) cliOptions {
	return cliOptions{
		vectorizer:              input.Vector.Vectorizer,
		vectorProvider:          input.Vector.Provider,
		vectorModel:             input.Vector.Model,
		vectorBaseURL:           input.Vector.BaseURL,
		vectorFallbackPolicy:    input.Vector.FallbackPolicy,
		vectorStorePath:         input.Vector.StorePath,
		vectorTimeout:           positiveIntFlag{value: input.Vector.TimeoutSeconds, set: input.Vector.TimeoutSet},
		vectorChunkMaxRunes:     positiveIntFlag{value: input.Vector.ChunkMaxRunes, set: input.Vector.ChunkMaxSet},
		vectorChunkOverlapRunes: positiveIntFlag{value: input.Vector.ChunkOverlapRunes, set: input.Vector.ChunkOverlapSet},
	}
}

func buildWorkspaceVectorRetrievalSearcher(ctx context.Context, state appState) (retrieval.Searcher, error) {
	if !workspaceVectorEnabled(state.vectorConfig) {
		return nil, errors.New("retrieval: --vector-index is required for --retrieval-source vector unless vector.workspace_enabled is true")
	}

	settings, opts, err := workspaceVectorSettings(firstNonEmpty(state.contextOptions.Root, state.cwd), state.vectorConfig)
	if err != nil {
		return nil, err
	}

	if settings.Vectorizer == vector.VectorizerKindEmbedding &&
		!workspaceRemoteEmbeddingAllowed(settings.BaseURL, state.vectorConfig.WorkspaceAllowRemoteEmbeddings) {
		if settings.FallbackPolicy == vector.VectorizerKindLexical {
			return buildWorkspaceVectorRetrievalSearcherWithLexicalFallback(ctx, settings, opts)
		}

		return nil, fmt.Errorf("retrieval: remote embedding endpoint %s is not allowed without vector.workspace_allow_remote_embeddings", workspaceDisplayEmbeddingEndpoint(settings.BaseURL))
	}

	searcher, err := buildWorkspaceVectorRetrievalSearcherOnce(ctx, settings, opts)
	if err == nil ||
		settings.Vectorizer != vector.VectorizerKindEmbedding ||
		settings.FallbackPolicy != vector.VectorizerKindLexical ||
		!vectorSearchEmbeddingFailure(err) {
		return searcher, err
	}

	return buildWorkspaceVectorRetrievalSearcherWithLexicalFallback(ctx, settings, opts)
}

func buildWorkspaceVectorRetrievalSearcherWithLexicalFallback(
	ctx context.Context,
	settings vectorSearchSettings,
	opts vector.WorkspaceOptions,
) (retrieval.Searcher, error) {
	settings, opts, err := workspaceLexicalFallbackSettings(settings, opts)
	if err != nil {
		return nil, err
	}

	return buildWorkspaceVectorRetrievalSearcherOnce(ctx, settings, opts)
}

func buildWorkspaceVectorRetrievalSearcherOnce(
	ctx context.Context,
	settings vectorSearchSettings,
	opts vector.WorkspaceOptions,
) (retrieval.Searcher, error) {
	idx, _, err := refreshWorkspaceVectorIndex(ctx, opts)
	if err != nil {
		return nil, err
	}

	return newVectorIndexRetrievalSearcher(
		idx,
		opts.Vectorizer,
		retrieval.Source{
			Type: retrieval.SourceVector,
			Name: "workspace",
			URI:  workspaceVectorSourceURI(opts),
		},
		settings.Vectorizer+"-workspace-ann",
	)
}

func newVectorIndexRetrievalSearcher(
	idx *vector.Index,
	vectorizer vector.Vectorizer,
	source retrieval.Source,
	scorerName string,
) (vector.IndexSearcher, error) {
	if idx == nil {
		return vector.IndexSearcher{}, errors.New("retrieval: vector index is nil")
	}

	ann, err := vector.NewANNIndex(idx.Documents, idx.Dimensions, vector.ANNOptions{})
	if err != nil {
		return vector.IndexSearcher{}, fmt.Errorf("retrieval: build ANN index: %w", err)
	}

	return vector.IndexSearcher{
		Index:      idx,
		Vectorizer: vectorizer,
		IndexANN:   ann,
		Source:     source,
		ScorerName: scorerName,
	}, nil
}

func buildAgentMemoryRetrievalSearcher(
	ctx context.Context,
	root, selectedAgent string,
	cfg appconfig.VectorConfig,
	input retrievalCommandInput,
) (retrieval.Searcher, error) {
	agentName := strings.TrimSpace(input.AgentMemoryAgent)
	if agentName == "" {
		agentName = strings.TrimSpace(input.AgentName)
	}

	if agentName == "" {
		agentName = strings.TrimSpace(selectedAgent)
	}

	if agentName == "" {
		return nil, errors.New("retrieval: --agent-memory-agent, --agent, or a configured selected agent is required for --retrieval-source agent-memory")
	}

	storePath := agentMemoryStorePath(root, agentName, input.AgentMemoryStorePath, cfg)

	runtime, err := agentMemoryVectorizerRuntimeFromConfig(cfg, agentName)
	if err != nil {
		return nil, err
	}

	store, _, err := loadAgentMemoryStore(ctx, storePath, false, false, runtime)
	if err != nil {
		return nil, err
	}

	return agentmemory.Searcher{Store: store, Agent: agentName}, nil
}

func formatRetrievalResult(result retrieval.Result, explain bool) string {
	result = retrieval.NormalizeResult(result)

	parts := []string{
		"source=" + string(result.Source.Type),
		"document=" + result.DocumentID,
		fmt.Sprintf("score=%.4f", result.Score),
		"scorer=" + result.Scorer.Name,
	}

	if stableID := result.Metadata[retrieval.MetadataStableID]; stableID != "" {
		parts = append(parts, "stable_id="+stableID)
	}

	parts = appendRetrievalSourceParts(parts, result.Source)

	if result.Chunk.ID != "" {
		parts = append(parts, "chunk="+result.Chunk.ID)
	}

	if result.Chunk.Range.Unit != "" {
		parts = append(parts, fmt.Sprintf("range=%s:%d-%d", result.Chunk.Range.Unit, result.Chunk.Range.Start, result.Chunk.Range.End))
	}

	parts = append(parts, "inject_allowed="+strconv.FormatBool(result.Safety.InjectAllowed))
	if result.Safety.Private {
		parts = append(parts, "private=true")
	}

	if result.Safety.Sensitive {
		parts = append(parts, "sensitive=true")
	}

	if result.Safety.Redacted {
		parts = append(parts, "redacted=true")
	}

	if len(result.Safety.Reasons) > 0 {
		parts = append(parts, "safety_reasons="+strings.Join(result.Safety.Reasons, ";"))
	}

	if result.Freshness.Status != "" {
		parts = append(parts, "freshness="+result.Freshness.Status)
	}

	if result.Freshness.Deleted {
		parts = append(parts, "deleted=true")
	}

	if !result.Freshness.SourceUpdatedAt.IsZero() {
		parts = append(parts, "source_updated_at="+result.Freshness.SourceUpdatedAt.Format(time.RFC3339))
	}

	if !result.Freshness.IndexedAt.IsZero() {
		parts = append(parts, "indexed_at="+result.Freshness.IndexedAt.Format(time.RFC3339))
	}

	parts = appendRetrievalContentParts(parts, result, explain)

	return strings.Join(parts, "\t")
}

func appendRetrievalContentParts(parts []string, result retrieval.Result, explain bool) []string {
	if result.Snippet != "" {
		parts = append(parts, "snippet="+result.Snippet)
	}

	if !explain {
		return parts
	}

	parts = appendRetrievalScorerDetails(parts, result.Scorer.Details)
	if len(result.Scorer.Explanation) > 0 {
		parts = append(parts, "why="+strings.Join(result.Scorer.Explanation, " | "))
	}

	return parts
}

func appendRetrievalScorerDetails(parts []string, details map[string]float64) []string {
	if len(details) == 0 {
		return parts
	}

	keys := make([]string, 0, len(details))
	for key := range details {
		if strings.TrimSpace(key) != "" {
			keys = append(keys, key)
		}
	}

	sort.Strings(keys)

	for _, key := range keys {
		field := retrievalScorerDetailField(key)
		if field == "" {
			continue
		}

		parts = append(parts, field+"="+formatRetrievalScorerDetailValue(key, details[key]))
	}

	return parts
}

func retrievalScorerDetailField(key string) string {
	key = strings.ToLower(strings.TrimSpace(key))
	if key == "" {
		return ""
	}

	var out strings.Builder
	previousSeparator := false
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z':
			out.WriteRune(r)
			previousSeparator = false
		case r >= '0' && r <= '9':
			out.WriteRune(r)
			previousSeparator = false
		case r == '_' || r == '-' || r == '.':
			if out.Len() > 0 && !previousSeparator {
				out.WriteByte('_')
				previousSeparator = true
			}
		}
	}

	name := strings.TrimRight(out.String(), "_")
	if name == "" {
		return ""
	}

	return "detail_" + name
}

func formatRetrievalScorerDetailValue(key string, value float64) string {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "ann_exact_scan":
		return strconv.FormatBool(value != 0)
	default:
		return strconv.FormatFloat(value, 'f', -1, 64)
	}
}

func appendRetrievalSourceParts(parts []string, source retrieval.Source) []string {
	if source.Name != "" {
		parts = append(parts, "source_name="+source.Name)
	}

	if source.URI != "" {
		parts = append(parts, "source_uri="+source.URI)
	}

	return parts
}

func addRetrievalSessionMemory(mem *memory.Store, store *session.Store) error {
	if store == nil {
		return nil
	}

	summaries, err := store.List()
	if err != nil {
		return fmt.Errorf("retrieval: list sessions: %w", err)
	}

	for i := range summaries {
		summary := &summaries[i]
		saved, err := store.Load(summary.Path)
		if err != nil {
			return fmt.Errorf("retrieval: load session %s: %w", summary.ID, err)
		}
		if err := mem.AddSession(saved); err != nil {
			return fmt.Errorf("retrieval: index session %s: %w", saved.ID, err)
		}
	}

	return nil
}

//nolint:cyclop,gocognit,nestif // Coordinates purge, rebuild, list, index, retention, and search modes in CLI precedence order.
func runMemoryCommand(store *session.Store, opts cliOptions) (err error) {
	redactor, err := memory.NewRedactor(opts.memoryRedactRules...)
	if err != nil {
		fallbackRedactor, fallbackErr := memory.NewRedactor()
		if fallbackErr != nil {
			return fmt.Errorf("memory: configure redaction: %w", err)
		}

		return redactMemoryCommandError(fallbackRedactor, fmt.Errorf("memory: configure redaction: %w", err))
	}
	defer func() {
		err = redactMemoryCommandError(redactor, err)
	}()

	if memoryRetentionRequestedOnly(opts) && strings.TrimSpace(opts.memoryStorePath) == "" {
		return errors.New("memory: --memory-store is required for retention-only")
	}
	if memoryIndexRequiresStore(opts) {
		return errors.New("memory: --memory-store is required for memory indexing without --memory-search")
	}
	if strings.TrimSpace(opts.memoryPurgeSpec) != "" && opts.memoryRebuild {
		return errors.New("memory: --memory-purge cannot be combined with --memory-rebuild")
	}

	if strings.TrimSpace(opts.memoryPurgeSpec) != "" {
		if strings.TrimSpace(opts.memoryStorePath) == "" {
			return errors.New("memory: --memory-store is required for --memory-purge")
		}

		mem, purgeErr := loadMemoryStore(opts.memoryStorePath)
		if purgeErr != nil {
			return purgeErr
		}
		mem.SetRedactor(redactor)

		filter, parseErr := parseMemoryPurgeSpec(opts.memoryPurgeSpec)
		if parseErr != nil {
			return parseErr
		}

		removed := mem.Purge(filter)
		if saveErr := mem.Save(opts.memoryStorePath); saveErr != nil {
			return fmt.Errorf("memory: save store: %w", saveErr)
		}

		fmt.Printf("Purged %d memory document(s) from %s\n", removed, redactMemoryDisplay(redactor, opts.memoryStorePath))
		if memoryPurgeOnly(opts) {
			return nil
		}
	}

	var mem *memory.Store
	if opts.memoryRebuild {
		if strings.TrimSpace(opts.memoryStorePath) == "" {
			return errors.New("memory: --memory-store is required for --memory-rebuild")
		}

		mem, err = buildMemoryStoreWithRedactor(store, opts, redactor, true)
		if err != nil {
			return err
		}

		if saveErr := mem.Save(opts.memoryStorePath); saveErr != nil {
			return fmt.Errorf("memory: save store: %w", saveErr)
		}

		fmt.Printf("Rebuilt memory store at %s\n", redactMemoryDisplay(redactor, opts.memoryStorePath))
		fmt.Print(formatMemoryCorpusWithRedactor(mem, redactor))
		if strings.TrimSpace(opts.memorySearch) == "" {
			return nil
		}
	}

	if mem == nil {
		mem, err = buildMemoryStoreWithRedactor(store, opts, redactor, false)
		if err != nil {
			return err
		}
	}

	var maintenanceMem *memory.Store
	if opts.memoryStorePath != "" && opts.memoryRetentionDays.value > 0 && !opts.memoryRebuild {
		maintenanceMem, err = buildMemoryMaintenanceStore(opts, redactor)
		if err != nil {
			return err
		}
		if saveErr := maintenanceMem.Save(opts.memoryStorePath); saveErr != nil {
			return fmt.Errorf("memory: save store: %w", saveErr)
		}

		if shouldReturnAfterMemoryRetention(opts) {
			fmt.Printf("Applied %s memory retention to %s\n", memoryRetentionStatusText(maintenanceMem, opts), redactMemoryDisplay(redactor, opts.memoryStorePath))
			return nil
		}
	}

	if opts.memoryStorePath != "" && len(opts.memoryIndexFiles) > 0 && !opts.memoryRebuild {
		if maintenanceMem == nil {
			maintenanceMem, err = buildMemoryMaintenanceStore(opts, redactor)
			if err != nil {
				return err
			}
		}
		if saveErr := maintenanceMem.Save(opts.memoryStorePath); saveErr != nil {
			return fmt.Errorf("memory: save store: %w", saveErr)
		}

		if opts.memorySearch == "" && !opts.memoryListCorpus {
			fmt.Printf("Indexed %d document(s) into %s\n", len(opts.memoryIndexFiles), redactMemoryDisplay(redactor, opts.memoryStorePath))
			return nil
		}
	}

	if opts.memoryListCorpus {
		fmt.Print(formatMemoryCorpusWithRedactor(mem, redactor))
		if strings.TrimSpace(opts.memorySearch) == "" {
			return nil
		}
	}

	if strings.TrimSpace(opts.memorySearch) == "" {
		return errors.New("memory: --memory-search is required unless indexing, purging, rebuilding, retaining, or listing corpus")
	}

	searchMem, err := memorySearchStore(mem, opts, redactor)
	if err != nil {
		return err
	}

	limit := opts.memoryLimit.value
	if limit == 0 {
		limit = 5
	}

	results, err := searchMem.Search(opts.memorySearch, limit)
	if err != nil {
		return fmt.Errorf("memory: search: %w", err)
	}

	fmt.Print(formatMemoryCorpusStatement(searchMem, redactor, opts.memoryStorePath))

	if len(results) == 0 {
		fmt.Println("No memory results found.")
		return nil
	}

	for i := range results {
		fmt.Println(formatMemoryResultWithRedactor(results[i], redactor))
	}

	return nil
}

func buildMemoryMaintenanceStore(opts cliOptions, redactor *memory.Redactor) (*memory.Store, error) {
	mem, err := loadMemoryStore(opts.memoryStorePath)
	if err != nil {
		return nil, err
	}
	mem.SetRedactor(redactor)

	plan, err := memoryPlan(opts)
	if err != nil {
		return nil, err
	}

	applyMemoryRetentionPolicy(mem, plan)
	if err := addMemoryIndexFiles(mem, opts.memoryIndexFiles); err != nil {
		return nil, err
	}
	applyMemoryRetentionPolicy(mem, plan)
	if explicitMemoryScopeRequested(opts) {
		updateMemoryCorpus(mem, plan, memoryStoreSessionIDs(mem))
	}

	return mem, nil
}

func memorySearchStore(mem *memory.Store, opts cliOptions, redactor *memory.Redactor) (*memory.Store, error) {
	plan, err := memoryPlan(opts)
	if err != nil {
		return nil, err
	}
	if !implicitStoreBackedSearchShouldUseRepoScope(opts, plan) {
		return mem, nil
	}

	searchMem := cloneMemoryStore(mem)
	constrainMemoryStore(searchMem, plan, nil, redactor)
	if err := addExplicitMemoryIndexDocuments(searchMem, mem, opts.memoryIndexFiles, redactor); err != nil {
		return nil, err
	}

	return searchMem, nil
}

func addExplicitMemoryIndexDocuments(dst, src *memory.Store, paths []string, redactor *memory.Redactor) error {
	if dst == nil || src == nil || len(paths) == 0 {
		return nil
	}

	indexed := memoryIndexPathSet(paths, redactor)
	if len(indexed) == 0 {
		return nil
	}

	for i := range src.Documents {
		doc := &src.Documents[i]
		if !memoryDocumentMatchesIndexedPath(*doc, indexed) {
			continue
		}

		if err := dst.Add(cloneMemoryDocument(*doc)); err != nil {
			return fmt.Errorf("memory: include indexed file %s: %w", doc.ID, err)
		}
	}

	return nil
}

func memoryIndexPathSet(paths []string, redactor *memory.Redactor) map[string]struct{} {
	indexed := make(map[string]struct{}, len(paths)*2)
	for _, path := range paths {
		path = filepath.Clean(strings.TrimSpace(path))
		if path == "." || path == "" {
			continue
		}

		indexed[path] = struct{}{}
		if redactor != nil {
			redacted, _ := redactor.RedactIdentifier(path)
			if strings.TrimSpace(redacted) != "" {
				indexed[redacted] = struct{}{}
			}
		}
	}

	return indexed
}

func memoryDocumentMatchesIndexedPath(doc memory.Document, indexed map[string]struct{}) bool {
	for _, candidate := range []string{
		doc.ID,
		doc.Path,
		doc.Metadata["path"],
	} {
		if _, ok := indexed[filepath.Clean(strings.TrimSpace(candidate))]; ok {
			return true
		}
	}
	if doc.Provenance != nil {
		if _, ok := indexed[filepath.Clean(strings.TrimSpace(doc.Provenance.Path))]; ok {
			return true
		}
	}

	return false
}

func cloneMemoryStore(mem *memory.Store) *memory.Store {
	if mem == nil {
		return memory.NewStore()
	}

	copied := *mem
	copied.Corpus = cloneMemoryCorpus(mem.Corpus)
	copied.Documents = make([]memory.Document, len(mem.Documents))
	for i := range mem.Documents {
		copied.Documents[i] = cloneMemoryDocument(mem.Documents[i])
	}

	return &copied
}

func cloneMemoryCorpus(corpus memory.CorpusMetadata) memory.CorpusMetadata {
	copied := corpus
	copied.CreatedFrom = append([]string(nil), corpus.CreatedFrom...)
	copied.SessionIDs = append([]string(nil), corpus.SessionIDs...)
	copied.Tags = append([]string(nil), corpus.Tags...)

	return copied
}

func cloneMemoryDocument(doc memory.Document) memory.Document {
	copied := doc
	if doc.Metadata != nil {
		copied.Metadata = maps.Clone(doc.Metadata)
	}
	if doc.Provenance != nil {
		provenance := *doc.Provenance
		provenance.Tags = append([]string(nil), doc.Provenance.Tags...)
		copied.Provenance = &provenance
	}
	if doc.Policy != nil {
		policy := *doc.Policy
		policy.RedactionRules = append([]string(nil), doc.Policy.RedactionRules...)
		copied.Policy = &policy
	}

	return copied
}

func redactMemoryDisplay(redactor *memory.Redactor, value string) string {
	if redactor == nil || strings.TrimSpace(value) == "" {
		return value
	}

	redacted, _ := redactor.Redact(value)

	return redacted
}

func redactMemoryCommandError(redactor *memory.Redactor, err error) error {
	if redactor == nil || err == nil {
		return err
	}

	redacted, decision := redactor.Redact(err.Error())
	if !decision.Redacted {
		return err
	}

	return redactedMemoryCommandError{cause: err, message: redacted}
}

type redactedMemoryCommandError struct {
	cause   error
	message string
}

func (e redactedMemoryCommandError) Error() string {
	return e.message
}

func (e redactedMemoryCommandError) Unwrap() error {
	return e.cause
}

func buildMemoryStore(store *session.Store, opts cliOptions) (*memory.Store, error) {
	redactor, err := memory.NewRedactor(opts.memoryRedactRules...)
	if err != nil {
		return nil, fmt.Errorf("memory: configure redaction: %w", err)
	}

	return buildMemoryStoreWithRedactor(store, opts, redactor, false)
}

func buildMemoryStoreWithRedactor(store *session.Store, opts cliOptions, redactor *memory.Redactor, rebuild bool) (*memory.Store, error) {
	var mem *memory.Store
	if rebuild {
		mem = memory.NewStore()
	} else {
		var err error
		mem, err = loadMemoryStore(opts.memoryStorePath)
		if err != nil {
			return nil, err
		}
	}
	mem.SetRedactor(redactor)

	plan, err := memoryPlan(opts)
	if err != nil {
		return nil, err
	}
	if buildErr := validateMemoryBuildRequest(opts, plan, rebuild); buildErr != nil {
		return nil, buildErr
	}
	applyMemoryRetentionPolicy(mem, plan)

	if addErr := addMemoryIndexFiles(mem, opts.memoryIndexFiles); addErr != nil {
		return nil, addErr
	}
	var indexedFileSource *memory.Store
	if len(opts.memoryIndexFiles) > 0 {
		indexedFileSource = cloneMemoryStore(mem)
	}

	sessionIDs, err := indexMemorySessionsForPlan(mem, store, opts, plan, rebuild)
	if err != nil {
		return nil, err
	}
	sessionIDs = selectedMemorySessionIDs(plan, sessionIDs)

	applyMemoryRetentionPolicy(mem, plan)

	if shouldConstrainMemoryCorpus(opts, plan) && !purgeLeftEmptyMemoryStore(opts, mem) {
		constrainMemoryStore(mem, plan, sessionIDs, redactor)
		if addErr := addExplicitMemoryIndexDocuments(mem, indexedFileSource, opts.memoryIndexFiles, redactor); addErr != nil {
			return nil, addErr
		}
	} else if shouldApplyStoreScopePolicy(opts, plan) {
		updateMemoryCorpus(mem, plan, memoryStoreSessionIDs(mem))
	}

	return mem, nil
}

func purgeLeftEmptyMemoryStore(opts cliOptions, mem *memory.Store) bool {
	return strings.TrimSpace(opts.memoryPurgeSpec) != "" && mem != nil && len(mem.Documents) == 0
}

func validateMemoryBuildRequest(opts cliOptions, plan memoryCorpusPlan, rebuild bool) error {
	if plan.scope == memoryScopeStore && strings.TrimSpace(opts.memoryStorePath) == "" {
		return errors.New("memory: --memory-store is required for --memory-scope store")
	}
	if rebuild && plan.scope == memoryScopeStore {
		return errors.New("memory: --memory-scope store cannot be used with --memory-rebuild")
	}

	return nil
}

func addMemoryIndexFiles(mem *memory.Store, paths []string) error {
	if len(paths) == 0 {
		return nil
	}

	if err := mem.AddFiles(paths...); err != nil {
		return fmt.Errorf("memory: index files: %w", err)
	}
	mem.Corpus.FileCount = memoryFileDocumentCount(mem)

	return nil
}

func indexMemorySessionsForPlan(mem *memory.Store, store *session.Store, opts cliOptions, plan memoryCorpusPlan, rebuild bool) ([]string, error) {
	if !shouldIndexSessionMemory(opts, rebuild) {
		return nil, nil
	}

	sessionIDs, err := addSessionMemory(mem, store, plan)
	if err == nil {
		return sessionIDs, nil
	}
	if canUseStoredSessionOnly(opts, plan, err) {
		return []string{memorySessionIDFromRef(plan.sessionRef)}, nil
	}

	return nil, err
}

func selectedMemorySessionIDs(plan memoryCorpusPlan, sessionIDs []string) []string {
	if len(sessionIDs) == 0 && plan.scope == memory.ScopeSession && strings.TrimSpace(plan.sessionRef) != "" {
		return []string{memorySessionIDFromRef(plan.sessionRef)}
	}

	return sessionIDs
}

func applyMemoryRetentionPolicy(mem *memory.Store, plan memoryCorpusPlan) {
	if plan.retentionDays <= 0 {
		return
	}

	mem.ApplyRetention(plan.since)
	mem.ApplyPolicy("", plan.retentionText)
	mem.Corpus.Retention = plan.retentionText
	mem.Corpus.DateStart = plan.since.UTC().Format(time.RFC3339)
	mem.Corpus.DateEnd = ""
	mem.Corpus.Description = memoryCorpusDescription(mem.Corpus)
}

func canUseStoredSessionOnly(opts cliOptions, plan memoryCorpusPlan, err error) bool {
	return strings.TrimSpace(opts.memoryStorePath) != "" &&
		plan.scope == memory.ScopeSession &&
		errors.Is(err, os.ErrNotExist)
}

func memorySessionIDFromRef(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}

	base := filepath.Base(ref)
	if ext := filepath.Ext(base); ext != "" {
		base = strings.TrimSuffix(base, ext)
	}

	return strings.TrimSpace(base)
}

func loadMemoryStore(path string) (*memory.Store, error) {
	if strings.TrimSpace(path) == "" {
		return memory.NewStore(), nil
	}

	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return memory.NewStore(), nil
		}

		return nil, fmt.Errorf("memory: stat store %s: %w", path, err)
	}

	store, err := memory.Load(path)
	if err != nil {
		return nil, fmt.Errorf("memory: load store: %w", err)
	}

	return store, nil
}

func addSessionMemory(mem *memory.Store, store *session.Store, plan memoryCorpusPlan) ([]string, error) {
	if store == nil {
		return nil, errors.New("memory: session store is required for session-backed memory scopes")
	}

	sessions, err := memorySessionCandidatesForPlan(store, plan)
	if err != nil {
		return nil, err
	}

	sessionIDs := make([]string, 0, len(sessions))
	for i := range sessions {
		saved := sessions[i].saved
		if strings.TrimSpace(saved.WorktreePath) == "" && strings.TrimSpace(plan.repoPath) != "" &&
			memorySessionRepoMatches(saved, sessions[i].path, plan) {
			saved.WorktreePath = plan.repoPath
		}
		sessionIDs = append(sessionIDs, saved.ID)

		if err := mem.AddSessionWithOptions(saved, memory.SessionIndexOptions{
			Scope:     plan.scope,
			Agent:     plan.agent,
			Retention: plan.retentionText,
		}); err != nil {
			return nil, fmt.Errorf("memory: index session %s: %w", saved.ID, err)
		}
	}

	updateMemoryCorpus(mem, plan, memoryStoreSessionIDs(mem))

	return sessionIDs, nil
}

func memorySessionCandidatesForPlan(store *session.Store, plan memoryCorpusPlan) ([]memorySessionCandidate, error) {
	if plan.scope != memory.ScopeSession {
		return loadMemorySessions(store, plan)
	}

	ref := strings.TrimSpace(plan.sessionRef)
	if ref == "" {
		return nil, errors.New("memory: --memory-session or --session is required for session memory scope")
	}

	saved, err := store.Load(ref)
	if err != nil {
		return nil, fmt.Errorf("memory: load session %s: %w", ref, err)
	}

	sessionPath := store.Path(ref)
	if !memorySessionMatchesPlan(saved, sessionPath, plan) {
		return nil, nil
	}

	return []memorySessionCandidate{{path: sessionPath, saved: saved}}, nil
}

func loadMemorySessions(store *session.Store, plan memoryCorpusPlan) ([]memorySessionCandidate, error) {
	summaries, err := store.List()
	if err != nil {
		return nil, fmt.Errorf("memory: list sessions: %w", err)
	}

	sessions := make([]memorySessionCandidate, 0, len(summaries))
	for i := range summaries {
		summary := &summaries[i]
		if !memorySummaryMatchesPlan(*summary, plan) {
			continue
		}

		saved, err := store.Load(summary.Path)
		if err != nil {
			return nil, fmt.Errorf("memory: load session %s: %w", summary.ID, err)
		}

		if !memorySessionMatchesPlan(saved, summary.Path, plan) {
			continue
		}

		sessions = append(sessions, memorySessionCandidate{saved: saved, path: summary.Path})
	}

	return sessions, nil
}

func memorySummaryMatchesPlan(summary session.Summary, plan memoryCorpusPlan) bool {
	if strings.TrimSpace(plan.sessionRef) != "" && !memorySummarySessionMatches(summary, plan.sessionRef) {
		return false
	}

	if !memorySummaryRepoMatches(summary, plan) {
		return false
	}

	if len(plan.tags) > 0 && !summaryHasAnyMemoryTag(summary, plan.tags) {
		return false
	}

	if plan.agent != "" && !summaryHasMemoryAgent(summary, plan.agent) {
		return false
	}

	return memorySummaryDateMatches(summary, plan)
}

func memorySummarySessionMatches(summary session.Summary, sessionRef string) bool {
	return memorySessionRefMatches(summary.ID, summary.Path, sessionRef)
}

func memorySummaryRepoMatches(summary session.Summary, plan memoryCorpusPlan) bool {
	if strings.TrimSpace(plan.repoPath) == "" {
		return true
	}

	if strings.TrimSpace(summary.WorktreePath) != "" {
		return sameMemoryPath(summary.WorktreePath, plan.repoPath)
	}

	return memoryPathWithin(summary.Path, plan.repoPath)
}

func summaryHasAnyMemoryTag(summary session.Summary, tags []string) bool {
	want := make(map[string]struct{}, len(tags))
	for _, tag := range tags {
		tag = strings.ToLower(strings.TrimSpace(tag))
		if tag != "" {
			want[tag] = struct{}{}
		}
	}

	if len(want) == 0 {
		return true
	}

	for _, tag := range summary.Tags {
		if _, ok := want[strings.ToLower(strings.TrimSpace(tag))]; ok {
			return true
		}
	}

	return false
}

func summaryHasMemoryAgent(summary session.Summary, agent string) bool {
	agent = strings.ToLower(strings.TrimSpace(agent))
	if agent == "" {
		return true
	}

	if strings.ToLower(strings.TrimSpace(summary.DefaultAgent)) == agent {
		return true
	}

	for _, candidate := range summary.AgentNames {
		if strings.ToLower(strings.TrimSpace(candidate)) == agent {
			return true
		}
	}

	return false
}

func memorySummaryDateMatches(summary session.Summary, plan memoryCorpusPlan) bool {
	activity := summary.UpdatedAt
	if activity.IsZero() {
		activity = summary.CreatedAt
	}
	if plan.hasSince && (activity.IsZero() || activity.Before(plan.since)) {
		return false
	}
	if plan.hasUntil && (activity.IsZero() || activity.After(plan.until)) {
		return false
	}

	return true
}

func memoryCommandRequested(opts cliOptions) bool {
	return opts.memorySearch != "" ||
		len(opts.memoryIndexFiles) > 0 ||
		opts.memoryPurgeSpec != "" ||
		opts.memoryRebuild ||
		opts.memoryListCorpus ||
		opts.memoryRetentionDays.value > 0
}

func memoryPurgeOnly(opts cliOptions) bool {
	return strings.TrimSpace(opts.memoryPurgeSpec) != "" &&
		strings.TrimSpace(opts.memorySearch) == "" &&
		len(opts.memoryIndexFiles) == 0 &&
		!opts.memoryRebuild &&
		!opts.memoryListCorpus &&
		opts.memoryRetentionDays.value == 0
}

func shouldReturnAfterMemoryRetention(opts cliOptions) bool {
	return strings.TrimSpace(opts.memorySearch) == "" &&
		len(opts.memoryIndexFiles) == 0 &&
		!opts.memoryRebuild &&
		!opts.memoryListCorpus
}

func memoryRetentionStatusText(mem *memory.Store, opts cliOptions) string {
	if mem != nil && strings.TrimSpace(mem.Corpus.Retention) != "" {
		return mem.Corpus.Retention
	}
	if opts.memoryRetentionDays.value > 0 {
		return fmt.Sprintf("%d days", opts.memoryRetentionDays.value)
	}

	return "configured"
}

func memoryRetentionRequestedOnly(opts cliOptions) bool {
	return opts.memoryRetentionDays.value > 0 &&
		strings.TrimSpace(opts.memorySearch) == "" &&
		len(opts.memoryIndexFiles) == 0 &&
		strings.TrimSpace(opts.memoryPurgeSpec) == "" &&
		!opts.memoryRebuild &&
		!opts.memoryListCorpus
}

func memoryIndexRequiresStore(opts cliOptions) bool {
	return len(opts.memoryIndexFiles) > 0 &&
		strings.TrimSpace(opts.memoryStorePath) == "" &&
		strings.TrimSpace(opts.memorySearch) == ""
}

func shouldIndexSessionMemory(opts cliOptions, rebuild bool) bool {
	if len(opts.memoryIndexFiles) > 0 && strings.TrimSpace(opts.memorySearch) == "" && !rebuild {
		return false
	}

	if memoryRetentionRequestedOnly(opts) {
		return false
	}

	if memoryStoreMaintenanceOnly(opts) {
		return false
	}

	if strings.TrimSpace(opts.memoryPurgeSpec) != "" && !rebuild {
		return false
	}

	if memoryIndexMaintenanceOnly(opts) {
		return false
	}

	if normalizeMemoryScope(opts.memoryScope) == memoryScopeStore {
		return false
	}

	if rebuild {
		return true
	}

	if strings.TrimSpace(opts.memoryStorePath) == "" {
		return true
	}

	return explicitMemoryScopeRequested(opts)
}

func memoryStoreMaintenanceOnly(opts cliOptions) bool {
	return strings.TrimSpace(opts.memoryStorePath) != "" &&
		strings.TrimSpace(opts.memorySearch) == "" &&
		!opts.memoryRebuild &&
		(opts.memoryListCorpus ||
			opts.memoryRetentionDays.value > 0 ||
			len(opts.memoryIndexFiles) > 0 ||
			strings.TrimSpace(opts.memoryPurgeSpec) != "")
}

func memoryIndexMaintenanceOnly(opts cliOptions) bool {
	return strings.TrimSpace(opts.memoryStorePath) != "" &&
		len(opts.memoryIndexFiles) > 0 &&
		strings.TrimSpace(opts.memorySearch) == "" &&
		strings.TrimSpace(opts.memoryPurgeSpec) == "" &&
		!opts.memoryRebuild
}

func explicitMemoryScopeRequested(opts cliOptions) bool {
	return strings.TrimSpace(opts.memoryScope) != "" ||
		strings.TrimSpace(opts.memorySessionRef) != "" ||
		strings.TrimSpace(opts.sessionRef) != "" ||
		strings.TrimSpace(opts.memoryRepoPath) != "" ||
		len(opts.memoryTags) > 0 ||
		strings.TrimSpace(opts.memorySince) != "" ||
		strings.TrimSpace(opts.memoryUntil) != "" ||
		strings.TrimSpace(opts.memoryAgent) != "" ||
		opts.memoryGlobal
}

func shouldConstrainMemoryCorpus(opts cliOptions, plan memoryCorpusPlan) bool {
	if memoryRetentionRequestedOnly(opts) {
		return false
	}

	if implicitStoreBackedSearchUsesRepoScope(opts, plan) {
		return true
	}

	if !explicitMemoryScopeRequested(opts) {
		return false
	}

	switch plan.scope {
	case memory.ScopeGlobal, memoryScopeStore:
		return memoryPlanHasSecondaryFilters(plan)
	default:
		return true
	}
}

func shouldApplyStoreScopePolicy(opts cliOptions, plan memoryCorpusPlan) bool {
	return normalizeMemoryScope(opts.memoryScope) == memoryScopeStore &&
		plan.scope == memoryScopeStore
}

func implicitStoreBackedSearchUsesRepoScope(opts cliOptions, plan memoryCorpusPlan) bool {
	return implicitStoreBackedSearchShouldUseRepoScope(opts, plan) &&
		len(opts.memoryIndexFiles) == 0 &&
		opts.memoryRetentionDays.value == 0
}

func implicitStoreBackedSearchShouldUseRepoScope(opts cliOptions, plan memoryCorpusPlan) bool {
	return strings.TrimSpace(opts.memoryStorePath) != "" &&
		strings.TrimSpace(opts.memorySearch) != "" &&
		strings.TrimSpace(opts.memoryScope) == "" &&
		!opts.memoryGlobal &&
		plan.scope == memory.ScopeRepo &&
		strings.TrimSpace(plan.repoPath) != ""
}

func memoryPlanHasSecondaryFilters(plan memoryCorpusPlan) bool {
	return strings.TrimSpace(plan.repoPath) != "" ||
		strings.TrimSpace(plan.sessionRef) != "" ||
		len(plan.tags) > 0 ||
		strings.TrimSpace(plan.agent) != "" ||
		plan.hasSince ||
		plan.hasUntil
}

func memoryPlan(opts cliOptions) (memoryCorpusPlan, error) {
	scope := normalizeMemoryScope(opts.memoryScope)
	plan := memoryCorpusPlan{
		scope:         scope,
		sessionRef:    firstNonEmptyString(opts.memorySessionRef, opts.sessionRef),
		repoPath:      strings.TrimSpace(opts.memoryRepoPath),
		tags:          append([]string(nil), opts.memoryTags...),
		agent:         memoryPlanAgent(opts, scope),
		retentionDays: opts.memoryRetentionDays.value,
	}

	if opts.memoryGlobal {
		plan.scope = memory.ScopeGlobal
		plan.global = true
	}

	if plan.scope == "" {
		plan.scope = inferredMemoryScope(opts, plan)
	}

	if plan.scope == memory.ScopeGlobal {
		plan.global = true
	}

	if plan.repoPath != "" {
		plan.repoPath = normalizeExplicitMemoryRepoPath(plan.repoPath)
	} else if plan.scope == memory.ScopeRepo {
		repoPath, err := defaultMemoryRepoPath()
		if err != nil {
			return memoryCorpusPlan{}, fmt.Errorf("memory: resolve cwd for repo scope: %w", err)
		}
		plan.repoPath = repoPath
	}

	var err error
	if strings.TrimSpace(opts.memorySince) != "" {
		plan.since, err = parseMemoryTime(opts.memorySince, false)
		if err != nil {
			return memoryCorpusPlan{}, err
		}
		plan.hasSince = true
	}
	if strings.TrimSpace(opts.memoryUntil) != "" {
		plan.until, err = parseMemoryTime(opts.memoryUntil, true)
		if err != nil {
			return memoryCorpusPlan{}, err
		}
		plan.hasUntil = true
	}
	if plan.retentionDays > 0 {
		plan.retentionText = fmt.Sprintf("%d days", plan.retentionDays)
		cutoff := time.Now().UTC().AddDate(0, 0, -plan.retentionDays)
		if !plan.hasSince || cutoff.After(plan.since) {
			plan.since = cutoff
			plan.hasSince = true
		}
	}

	if err := validateMemoryPlan(plan); err != nil {
		return memoryCorpusPlan{}, err
	}

	return plan, nil
}

func memoryPlanAgent(opts cliOptions, scope string) string {
	if agent := strings.TrimSpace(opts.memoryAgent); agent != "" {
		return agent
	}
	if scope == memory.ScopeAgent {
		return strings.TrimSpace(opts.agentName)
	}

	return ""
}

func validateMemoryPlan(plan memoryCorpusPlan) error {
	if plan.hasSince && plan.hasUntil && plan.since.After(plan.until) {
		return fmt.Errorf("memory: --memory-since %s is after --memory-until %s", plan.since.Format(time.RFC3339), plan.until.Format(time.RFC3339))
	}

	switch plan.scope {
	case memory.ScopeSession:
		if strings.TrimSpace(plan.sessionRef) == "" {
			return errors.New("memory: --memory-session or --session is required for session memory scope")
		}
	case memory.ScopeRepo, memory.ScopeGlobal, memoryScopeStore:
		return nil
	case memory.ScopeTags:
		if len(plan.tags) == 0 {
			return errors.New("memory: at least one --memory-tag is required for tags memory scope")
		}
	case memory.ScopeDateRange:
		if !plan.hasSince && !plan.hasUntil {
			return errors.New("memory: --memory-since, --memory-until, or --memory-retention-days is required for date-range memory scope")
		}
	case memory.ScopeAgent:
		if strings.TrimSpace(plan.agent) == "" {
			return errors.New("memory: --memory-agent or --agent is required for agent memory scope")
		}
	default:
		return fmt.Errorf("memory: unsupported --memory-scope %q", plan.scope)
	}

	return nil
}

func normalizeMemoryScope(scope string) string {
	switch strings.ToLower(strings.ReplaceAll(strings.TrimSpace(scope), "-", "_")) {
	case "":
		return ""
	case memoryScopeStore:
		return memoryScopeStore
	case "current_session", "current_session_only", helpSelectorSession:
		return memory.ScopeSession
	case "current_repo", "current_repo_only", "repo", "repository":
		return memory.ScopeRepo
	case "tag", "tags", "tagged", "tagged_session", "tagged_sessions":
		return memory.ScopeTags
	case "date", "date_range", "date_ranges", "daterange":
		return memory.ScopeDateRange
	case memoryAgentKey, "agent_memory", "agent_specific", "agent_specific_memory":
		return memory.ScopeAgent
	case "global", "global_memory", "opt_in_global", memoryPurgeAll:
		return memory.ScopeGlobal
	default:
		return strings.TrimSpace(scope)
	}
}

func inferredMemoryScope(opts cliOptions, plan memoryCorpusPlan) string {
	switch {
	case strings.TrimSpace(plan.sessionRef) != "":
		return memory.ScopeSession
	case strings.TrimSpace(plan.agent) != "":
		return memory.ScopeAgent
	case len(plan.tags) > 0:
		return memory.ScopeTags
	case plan.hasSince || plan.hasUntil || strings.TrimSpace(opts.memorySince) != "" || strings.TrimSpace(opts.memoryUntil) != "":
		return memory.ScopeDateRange
	default:
		return memory.ScopeRepo
	}
}

func defaultMemoryRepoPath() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get cwd: %w", err)
	}

	return findMemoryRepoRoot(cwd), nil
}

func normalizeExplicitMemoryRepoPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}

	if _, err := os.Stat(path); err == nil {
		return findMemoryRepoRoot(path)
	}
	if abs, err := filepath.Abs(path); err == nil {
		if _, statErr := os.Stat(abs); statErr == nil {
			return findMemoryRepoRoot(abs)
		}
	}

	return path
}

func findMemoryRepoRoot(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	path = filepath.Clean(path)

	for dir := path; ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return path
		}
	}
}

func parseMemoryTime(raw string, endOfDay bool) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, nil
	}

	if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
		return parsed.UTC(), nil
	}

	parsed, err := time.Parse("2006-01-02", raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("memory: parse date %q as YYYY-MM-DD or RFC3339: %w", raw, err)
	}
	if endOfDay {
		parsed = parsed.Add(24*time.Hour - time.Nanosecond)
	}

	return parsed.UTC(), nil
}

func memorySessionMatchesPlan(saved session.Session, sessionPath string, plan memoryCorpusPlan) bool {
	if strings.TrimSpace(plan.sessionRef) != "" && !memorySessionRefMatches(saved.ID, sessionPath, plan.sessionRef) {
		return false
	}

	if !memorySessionRepoMatches(saved, sessionPath, plan) {
		return false
	}

	if len(plan.tags) > 0 && !sessionHasAnyMemoryTag(saved, plan.tags) {
		return false
	}

	if plan.agent != "" && !sessionHasMemoryAgent(saved, plan.agent) {
		return false
	}

	return memorySessionDateMatches(saved, plan)
}

func memorySessionRepoMatches(saved session.Session, sessionPath string, plan memoryCorpusPlan) bool {
	if strings.TrimSpace(plan.repoPath) == "" {
		return true
	}

	if strings.TrimSpace(saved.WorktreePath) != "" {
		return sameMemoryPath(saved.WorktreePath, plan.repoPath)
	}

	return memoryPathWithin(sessionPath, plan.repoPath)
}

func memorySessionRefMatches(sessionID, sessionPath, sessionRef string) bool {
	sessionRef = strings.TrimSpace(sessionRef)
	if sessionRef == "" {
		return true
	}

	if strings.TrimSpace(sessionID) == sessionRef {
		return true
	}
	if refID := memorySessionIDFromRef(sessionRef); refID != "" && strings.TrimSpace(sessionID) == refID {
		return true
	}
	if strings.TrimSpace(sessionPath) != "" && sameMemoryPath(sessionPath, sessionRef) {
		return true
	}

	return false
}

func memorySessionDateMatches(saved session.Session, plan memoryCorpusPlan) bool {
	activity := saved.UpdatedAt
	if activity.IsZero() {
		activity = saved.CreatedAt
	}
	if plan.hasSince && (activity.IsZero() || activity.Before(plan.since)) {
		return false
	}
	if plan.hasUntil && (activity.IsZero() || activity.After(plan.until)) {
		return false
	}

	return true
}

func sameMemoryPath(left, right string) bool {
	left = cleanMemoryPath(left)
	right = cleanMemoryPath(right)

	return left != "" && right != "" && left == right
}

func cleanMemoryPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}

	return evalMemoryPathSymlinks(path)
}

func evalMemoryPathSymlinks(path string) string {
	path = filepath.Clean(path)
	if evaluated, err := filepath.EvalSymlinks(path); err == nil {
		return filepath.Clean(evaluated)
	}

	parent := path
	var suffix []string
	for {
		next := filepath.Dir(parent)
		if next == parent {
			return path
		}

		suffix = append(suffix, filepath.Base(parent))
		parent = next
		if evaluated, err := filepath.EvalSymlinks(parent); err == nil {
			out := filepath.Clean(evaluated)
			for i := len(suffix) - 1; i >= 0; i-- {
				out = filepath.Join(out, suffix[i])
			}

			return filepath.Clean(out)
		}
	}
}

func memoryPathWithin(path, root string) bool {
	path = cleanMemoryPath(path)
	root = cleanMemoryPath(root)
	if path == "" || root == "" {
		return false
	}

	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}

	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)))
}

func sessionHasAnyMemoryTag(saved session.Session, tags []string) bool {
	want := make(map[string]struct{}, len(tags))
	for _, tag := range tags {
		tag = strings.ToLower(strings.TrimSpace(tag))
		if tag != "" {
			want[tag] = struct{}{}
		}
	}

	if len(want) == 0 {
		return true
	}

	for _, tag := range saved.Tags {
		if _, ok := want[strings.ToLower(strings.TrimSpace(tag))]; ok {
			return true
		}
	}

	return false
}

func sessionHasMemoryAgent(saved session.Session, agent string) bool {
	agent = strings.ToLower(strings.TrimSpace(agent))
	if agent == "" {
		return true
	}

	if strings.ToLower(strings.TrimSpace(saved.DefaultAgent)) == agent {
		return true
	}

	for _, entry := range saved.NegativeKnowledge {
		if strings.ToLower(strings.TrimSpace(entry.Agent)) == agent {
			return true
		}
	}
	for i := range saved.Evaluations {
		entry := &saved.Evaluations[i]
		if strings.ToLower(strings.TrimSpace(entry.Agent)) == agent {
			return true
		}
	}
	for i := range saved.Artifacts {
		entry := &saved.Artifacts[i]
		if strings.ToLower(strings.TrimSpace(entry.SourceAgent)) == agent {
			return true
		}
	}

	return false
}

func memoryFileDocumentCount(mem *memory.Store) int {
	count := 0
	for i := range mem.Documents {
		doc := &mem.Documents[i]
		if doc.Provenance != nil && doc.Provenance.SourceType == memory.ScopeFile {
			count++
			continue
		}
		if doc.Metadata["source_type"] == memory.ScopeFile || doc.Metadata["kind"] == memory.ScopeFile {
			count++
		}
	}

	return count
}

func constrainMemoryStore(mem *memory.Store, plan memoryCorpusPlan, selectedSessionIDs []string, redactor *memory.Redactor) {
	matchPlan := redactedMemoryMatchPlan(plan, redactor)
	selectedSessionIDs = selectedMemorySessionIDs(plan, selectedSessionIDs)
	selected := memoryStringSet(selectedSessionIDs)
	for _, sessionID := range redactedMemorySessionIDs(selectedSessionIDs, redactor) {
		if sessionID = strings.TrimSpace(sessionID); sessionID != "" {
			selected[sessionID] = struct{}{}
		}
	}

	kept := mem.Documents[:0]
	for i := range mem.Documents {
		doc := mem.Documents[i]
		if memoryDocumentMatchesPlan(doc, plan, selected) || memoryDocumentMatchesPlan(doc, matchPlan, selected) {
			kept = append(kept, doc)
		}
	}

	mem.Documents = kept
	sessionIDs := memoryStoreSessionIDs(mem)
	if plan.scope == memory.ScopeSession && len(sessionIDs) == 0 {
		sessionIDs = selectedMemorySessionIDs(plan, selectedSessionIDs)
	}
	updateMemoryCorpus(mem, plan, sessionIDs)
}

func redactedMemoryMatchPlan(plan memoryCorpusPlan, redactor *memory.Redactor) memoryCorpusPlan {
	if redactor == nil {
		return plan
	}

	plan.tags = append([]string(nil), plan.tags...)
	plan.repoPath, _ = redactor.RedactIdentifier(plan.repoPath)
	plan.sessionRef, _ = redactor.RedactIdentifier(plan.sessionRef)
	plan.agent, _ = redactor.RedactIdentifier(plan.agent)
	for i := range plan.tags {
		plan.tags[i], _ = redactor.RedactIdentifier(plan.tags[i])
	}

	return plan
}

func redactedMemorySessionIDs(sessionIDs []string, redactor *memory.Redactor) []string {
	if redactor == nil || len(sessionIDs) == 0 {
		return sessionIDs
	}

	redacted := make([]string, 0, len(sessionIDs))
	for _, sessionID := range sessionIDs {
		value, _ := redactor.RedactIdentifier(sessionID)
		redacted = append(redacted, value)
	}

	return redacted
}

func memoryDocumentMatchesPlan(doc memory.Document, plan memoryCorpusPlan, selectedSessionIDs map[string]struct{}) bool {
	if !memoryDocumentMatchesSecondaryFilters(doc, plan) {
		return false
	}

	switch plan.scope {
	case memory.ScopeSession:
		_, ok := selectedSessionIDs[memoryDocumentSessionID(doc)]

		return ok
	case memory.ScopeRepo:
		return memoryDocumentRepoMatches(doc, plan.repoPath)
	case memory.ScopeTags:
		return memoryDocumentHasAnyTag(doc, plan.tags)
	case memory.ScopeDateRange:
		return memoryDocumentInDateRange(doc, plan)
	case memory.ScopeAgent:
		return strings.EqualFold(memoryDocumentAgent(doc), plan.agent)
	default:
		return true
	}
}

func memoryDocumentMatchesSecondaryFilters(doc memory.Document, plan memoryCorpusPlan) bool {
	return memoryDocumentMatchesSecondarySessionFilter(doc, plan) &&
		memoryDocumentMatchesSecondaryRepoFilter(doc, plan) &&
		memoryDocumentMatchesSecondaryTagFilter(doc, plan) &&
		memoryDocumentMatchesSecondaryDateFilter(doc, plan) &&
		memoryDocumentMatchesSecondaryAgentFilter(doc, plan)
}

func memoryDocumentMatchesSecondarySessionFilter(doc memory.Document, plan memoryCorpusPlan) bool {
	if plan.scope == memory.ScopeSession || strings.TrimSpace(plan.sessionRef) == "" {
		return true
	}

	return memoryDocumentSessionRefMatches(doc, plan.sessionRef)
}

func memoryDocumentMatchesSecondaryRepoFilter(doc memory.Document, plan memoryCorpusPlan) bool {
	if plan.scope == memory.ScopeRepo || strings.TrimSpace(plan.repoPath) == "" {
		return true
	}

	return memoryDocumentRepoMatches(doc, plan.repoPath)
}

func memoryDocumentMatchesSecondaryTagFilter(doc memory.Document, plan memoryCorpusPlan) bool {
	if plan.scope == memory.ScopeTags || len(plan.tags) == 0 {
		return true
	}

	return memoryDocumentHasAnyTag(doc, plan.tags)
}

func memoryDocumentMatchesSecondaryDateFilter(doc memory.Document, plan memoryCorpusPlan) bool {
	if plan.scope == memory.ScopeDateRange || (!plan.hasSince && !plan.hasUntil) {
		return true
	}

	return memoryDocumentInDateRange(doc, plan)
}

func memoryDocumentMatchesSecondaryAgentFilter(doc memory.Document, plan memoryCorpusPlan) bool {
	if plan.scope == memory.ScopeAgent || strings.TrimSpace(plan.agent) == "" {
		return true
	}

	return strings.EqualFold(memoryDocumentAgent(doc), plan.agent)
}

func memoryDocumentSessionRefMatches(doc memory.Document, sessionRef string) bool {
	sessionRef = strings.TrimSpace(sessionRef)
	if sessionRef == "" {
		return true
	}

	sessionID := memoryDocumentSessionID(doc)
	return sessionID != "" && memorySessionRefMatches(sessionID, "", sessionRef)
}

func memoryStringSet(values []string) map[string]struct{} {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			set[value] = struct{}{}
		}
	}

	return set
}

func memoryStoreSessionIDs(mem *memory.Store) []string {
	seen := make(map[string]struct{})
	for i := range mem.Documents {
		if sessionID := memoryDocumentSessionID(mem.Documents[i]); sessionID != "" {
			seen[sessionID] = struct{}{}
		}
	}

	sessionIDs := make([]string, 0, len(seen))
	for sessionID := range seen {
		sessionIDs = append(sessionIDs, sessionID)
	}
	sort.Strings(sessionIDs)

	return sessionIDs
}

func memoryDocumentSessionID(doc memory.Document) string {
	if doc.Provenance != nil && strings.TrimSpace(doc.Provenance.SessionID) != "" {
		return strings.TrimSpace(doc.Provenance.SessionID)
	}
	if sessionID := strings.TrimSpace(doc.Metadata["session_id"]); sessionID != "" {
		return sessionID
	}
	if strings.HasPrefix(doc.ID, "session/") {
		parts := strings.Split(doc.ID, "/")
		if len(parts) >= 2 {
			return strings.TrimSpace(parts[1])
		}
	}

	return ""
}

func memoryDocumentRepoMatches(doc memory.Document, repoPath string) bool {
	candidates := []string{
		doc.Metadata["repo_path"],
		doc.Metadata["worktree_path"],
	}
	if doc.Provenance != nil {
		candidates = append(candidates, doc.Provenance.RepoPath)
	}
	for _, candidate := range candidates {
		if sameMemoryPath(candidate, repoPath) {
			return true
		}
	}

	for _, candidate := range memoryDocumentRepoPathCandidates(doc) {
		if memoryPathWithin(candidate, repoPath) {
			return true
		}
	}

	return false
}

func memoryDocumentRepoPathCandidates(doc memory.Document) []string {
	if memoryDocumentIsSession(doc) {
		return nil
	}

	candidates := []string{doc.Metadata["path"], doc.Path}
	candidates = appendMemoryDocumentProvenanceFilePaths(candidates, doc)
	if memoryDocumentIsFile(doc) {
		candidates = append(candidates, doc.ID, doc.Metadata["source_id"])
	}

	return candidates
}

func appendMemoryDocumentProvenanceFilePaths(candidates []string, doc memory.Document) []string {
	if doc.Provenance == nil {
		return candidates
	}

	candidates = append(candidates, doc.Provenance.Path)
	if doc.Provenance.SourceType == memory.ScopeFile {
		candidates = append(candidates, doc.Provenance.SourceID)
	}

	return candidates
}

func memoryDocumentIsSession(doc memory.Document) bool {
	if doc.Provenance != nil {
		switch doc.Provenance.SourceType {
		case memory.ScopeFile:
			return false
		case "session":
			return true
		}
	}
	switch doc.Metadata["source_type"] {
	case memory.ScopeFile:
		return false
	case "session":
		return true
	}

	return memoryDocumentSessionID(doc) != ""
}

func memoryDocumentIsFile(doc memory.Document) bool {
	if doc.Provenance != nil && doc.Provenance.SourceType == memory.ScopeFile {
		return true
	}

	return doc.Metadata["source_type"] == memory.ScopeFile || doc.Metadata["kind"] == memory.ScopeFile
}

func memoryDocumentHasAnyTag(doc memory.Document, tags []string) bool {
	want := stringSetFold(tags)
	if len(want) == 0 {
		return true
	}

	for _, tag := range memoryDocumentTags(doc) {
		if _, ok := want[strings.ToLower(strings.TrimSpace(tag))]; ok {
			return true
		}
	}

	return false
}

func stringSetFold(values []string) map[string]struct{} {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value != "" {
			set[value] = struct{}{}
		}
	}

	return set
}

func memoryDocumentTags(doc memory.Document) []string {
	var tags []string
	if doc.Provenance != nil {
		tags = append(tags, doc.Provenance.Tags...)
	}
	for tag := range strings.SplitSeq(doc.Metadata["tags"], ",") {
		if tag = strings.TrimSpace(tag); tag != "" {
			tags = append(tags, tag)
		}
	}

	return tags
}

func memoryDocumentInDateRange(doc memory.Document, plan memoryCorpusPlan) bool {
	activity, ok := memoryDocumentActivity(doc)
	if !ok {
		return false
	}
	if plan.hasSince && activity.Before(plan.since) {
		return false
	}
	if plan.hasUntil && activity.After(plan.until) {
		return false
	}

	return true
}

func memoryDocumentActivity(doc memory.Document) (time.Time, bool) {
	candidates := make([]string, 0, 4)
	if doc.Provenance != nil {
		candidates = append(candidates, doc.Provenance.UpdatedAt, doc.Provenance.CreatedAt)
	}

	candidates = append(candidates, doc.Metadata["updated_at"], doc.Metadata["created_at"])
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}

		parsed, err := time.Parse(time.RFC3339, candidate)
		if err == nil {
			return parsed.UTC(), true
		}
	}

	return time.Time{}, false
}

func memoryDocumentAgent(doc memory.Document) string {
	if doc.Provenance != nil && strings.TrimSpace(doc.Provenance.Agent) != "" {
		return strings.TrimSpace(doc.Provenance.Agent)
	}

	for _, key := range []string{memoryAgentKey, "source_agent", "default_agent"} {
		if value := strings.TrimSpace(doc.Metadata[key]); value != "" {
			return value
		}
	}

	return ""
}

func updateMemoryCorpus(mem *memory.Store, plan memoryCorpusPlan, sessionIDs []string) {
	mem.Corpus.Scope = plan.scope
	mem.Corpus.RepoPath = memoryCorpusRepoPath(plan.repoPath)
	mem.Corpus.Tags = append([]string(nil), plan.tags...)
	mem.Corpus.Agent = plan.agent
	mem.Corpus.SessionIDs = append([]string(nil), sessionIDs...)
	mem.Corpus.SessionCount = len(sessionIDs)
	mem.Corpus.FileCount = memoryFileDocumentCount(mem)
	mem.Corpus.DocumentCount = len(mem.Documents)
	mem.Corpus.Global = plan.global
	mem.Corpus.Retention = plan.retentionText
	mem.Corpus.DateStart = ""
	mem.Corpus.DateEnd = ""
	if plan.hasSince {
		mem.Corpus.DateStart = plan.since.UTC().Format(time.RFC3339)
	}
	if plan.hasUntil {
		mem.Corpus.DateEnd = plan.until.UTC().Format(time.RFC3339)
	}

	var sources []string
	if len(sessionIDs) > 0 {
		sources = append(sources, "sessions")
	}
	if mem.Corpus.FileCount > 0 {
		sources = append(sources, "files")
	}
	mem.Corpus.CreatedFrom = sources
	mem.Corpus.Description = memoryCorpusDescription(mem.Corpus)
}

func memoryCorpusRepoPath(repoPath string) string {
	repoPath = strings.TrimSpace(repoPath)
	if repoPath == "" {
		return ""
	}
	if _, err := os.Stat(repoPath); err == nil {
		return cleanMemoryPath(repoPath)
	}

	return repoPath
}

func memoryCorpusDescription(corpus memory.CorpusMetadata) string {
	parts := []string{"scope=" + fallbackMemoryValue(corpus.Scope, "manual")}
	if corpus.Global {
		parts = append(parts, "global=opt-in")
	}
	if corpus.RepoPath != "" {
		parts = append(parts, "repo="+corpus.RepoPath)
	}
	if corpus.Agent != "" {
		parts = append(parts, "agent="+corpus.Agent)
	}
	if len(corpus.Tags) > 0 {
		parts = append(parts, "tags="+strings.Join(corpus.Tags, ","))
	}
	if len(corpus.SessionIDs) > 0 {
		parts = append(parts, "session_ids="+strings.Join(corpus.SessionIDs, ","))
	}
	if corpus.DateStart != "" || corpus.DateEnd != "" {
		parts = append(parts, "date_range="+fallbackMemoryValue(corpus.DateStart, "*")+".."+fallbackMemoryValue(corpus.DateEnd, "*"))
	}
	if corpus.Retention != "" {
		parts = append(parts, "retention="+corpus.Retention)
	}

	return strings.Join(parts, " ")
}

func parseMemoryPurgeSpec(raw string) (memory.PurgeFilter, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return memory.PurgeFilter{}, errors.New("memory: --memory-purge requires all, session:<id>, tag:<tag>, or repo:<path>")
	}
	if strings.EqualFold(raw, memoryPurgeAll) {
		return memory.PurgeFilter{All: true}, nil
	}

	key, value, ok := strings.Cut(raw, ":")
	if !ok {
		key, value, ok = strings.Cut(raw, "=")
	}
	if !ok || strings.TrimSpace(value) == "" {
		return memory.PurgeFilter{}, fmt.Errorf("memory: invalid purge selector %q; use all, session:<id>, tag:<tag>, or repo:<path>", raw)
	}

	switch strings.ToLower(strings.TrimSpace(key)) {
	case helpSelectorSession:
		return memory.PurgeFilter{SessionID: strings.TrimSpace(value)}, nil
	case "tag":
		return memory.PurgeFilter{Tag: strings.TrimSpace(value)}, nil
	case "repo", "repository", "worktree":
		return memory.PurgeFilter{RepoPath: normalizeExplicitMemoryRepoPath(value)}, nil
	default:
		return memory.PurgeFilter{}, fmt.Errorf("memory: unsupported purge selector %q; use all, session:<id>, tag:<tag>, or repo:<path>", key)
	}
}

func formatMemoryCorpusStatement(mem *memory.Store, redactor *memory.Redactor, storePath string) string {
	corpus := mem.Corpus
	parts := []string{
		"Searched corpus:",
		"scope=" + fallbackMemoryValue(corpus.Scope, "manual"),
		fmt.Sprintf("documents=%d", len(mem.Documents)),
		fmt.Sprintf("sessions=%d", corpus.SessionCount),
		fmt.Sprintf("files=%d", corpus.FileCount),
	}
	if strings.TrimSpace(storePath) != "" {
		parts = append(parts, "store="+filepath.Clean(storePath))
	}
	if corpus.Global {
		parts = append(parts, "global=opt-in")
	}
	if corpus.RepoPath != "" {
		parts = append(parts, "repo="+corpus.RepoPath)
	}
	if corpus.Agent != "" {
		parts = append(parts, "agent="+corpus.Agent)
	}
	if len(corpus.Tags) > 0 {
		parts = append(parts, "tags="+strings.Join(corpus.Tags, ","))
	}
	if len(corpus.SessionIDs) > 0 {
		parts = append(parts, "session_ids="+strings.Join(corpus.SessionIDs, ","))
	}
	if corpus.DateStart != "" || corpus.DateEnd != "" {
		parts = append(parts, "date_range="+fallbackMemoryValue(corpus.DateStart, "*")+".."+fallbackMemoryValue(corpus.DateEnd, "*"))
	}
	if corpus.Retention != "" {
		parts = append(parts, "retention="+corpus.Retention)
	}

	out := strings.Join(parts, "\t") + "\n"
	if redactor != nil {
		out, _ = redactor.Redact(out)
	}

	return out
}

func formatMemoryCorpusWithRedactor(mem *memory.Store, redactor *memory.Redactor) string {
	corpus := mem.Corpus
	var b strings.Builder
	fmt.Fprintf(&b, "Memory corpus:\tschema=%d\tscope=%s\tdocuments=%d\tsessions=%d\tfiles=%d\n",
		mem.SchemaVersion,
		fallbackMemoryValue(corpus.Scope, "manual"),
		len(mem.Documents),
		corpus.SessionCount,
		corpus.FileCount,
	)
	if !mem.CreatedAt.IsZero() {
		fmt.Fprintf(&b, "  created_at=%s\n", mem.CreatedAt.UTC().Format(time.RFC3339))
	}
	if !mem.UpdatedAt.IsZero() {
		fmt.Fprintf(&b, "  updated_at=%s\n", mem.UpdatedAt.UTC().Format(time.RFC3339))
	}
	if corpus.Description != "" {
		fmt.Fprintf(&b, "  policy=%s\n", corpus.Description)
	}
	if len(corpus.CreatedFrom) > 0 {
		fmt.Fprintf(&b, "  created_from=%s\n", strings.Join(corpus.CreatedFrom, ", "))
	}
	if corpus.RepoPath != "" {
		fmt.Fprintf(&b, "  repo=%s\n", corpus.RepoPath)
	}
	if corpus.Agent != "" {
		fmt.Fprintf(&b, "  agent=%s\n", corpus.Agent)
	}
	if len(corpus.Tags) > 0 {
		fmt.Fprintf(&b, "  tags=%s\n", strings.Join(corpus.Tags, ", "))
	}
	if len(corpus.SessionIDs) > 0 {
		fmt.Fprintf(&b, "  sessions=%s\n", strings.Join(corpus.SessionIDs, ", "))
	}
	if corpus.DateStart != "" || corpus.DateEnd != "" {
		fmt.Fprintf(&b, "  date_range=%s..%s\n", fallbackMemoryValue(corpus.DateStart, "*"), fallbackMemoryValue(corpus.DateEnd, "*"))
	}
	if corpus.Retention != "" {
		fmt.Fprintf(&b, "  retention=%s\n", corpus.Retention)
	}
	if corpus.Global {
		b.WriteString("  global=opt-in\n")
	}

	out := b.String()
	if redactor != nil {
		out, _ = redactor.Redact(out)
	}

	return out
}

func fallbackMemoryValue(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}

	return value
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}

	return ""
}

func formatMemoryResult(result memory.Result) string {
	redactor, err := memory.NewRedactor()
	if err != nil {
		return formatMemoryResultWithRedactor(result, nil)
	}

	return formatMemoryResultWithRedactor(result, redactor)
}

func formatMemoryResultWithRedactor(result memory.Result, redactor *memory.Redactor) string {
	parts := []string{
		result.Document.ID,
		fmt.Sprintf("score=%.4f", result.Score),
	}
	if result.Document.Path != "" {
		parts = append(parts, "path="+result.Document.Path)
	}

	if len(result.Matches) > 0 {
		parts = append(parts, "matches="+strings.Join(result.Matches, ","))
	}

	if kind := result.Document.Metadata["kind"]; kind != "" {
		parts = append(parts, "kind="+kind)
	}

	line := strings.Join(parts, "\t")
	if result.Snippet == "" {
		if redactor != nil {
			line, _ = redactor.Redact(line)
		}

		return line
	}

	snippet := result.Snippet
	if redactor != nil {
		line, _ = redactor.Redact(line)
		snippet, _ = redactor.Redact(snippet)
	}

	return line + "\n  " + snippet
}
