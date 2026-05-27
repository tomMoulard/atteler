//nolint:wsl_v5 // Memory CLI scope/policy helpers use compact guard clauses for readability.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tommoulard/atteler/pkg/agentmemory"
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
)

func runLSPSymbols(ctx context.Context, input lspSymbolsCommandInput) error {
	format, err := structuredCommandOutputFormat(input.JSON, input.OutputFormat)
	if err != nil {
		return err
	}

	pool := lsp.NewServerPool(lsp.PoolOptions{CommandPolicy: authorizeLSPCommand})

	defer func() {
		if shutdownErr := pool.Shutdown(ctx); shutdownErr != nil {
			_, _ = fmt.Fprintf(os.Stderr, "lsp shutdown: %v\n", shutdownErr)
		}
	}()

	lspOptions := lsp.Options{
		Command:    strings.TrimSpace(input.Command),
		Args:       append([]string(nil), input.Args...),
		FilePath:   strings.TrimSpace(input.FilePath),
		RootPath:   strings.TrimSpace(input.RootPath),
		LanguageID: strings.TrimSpace(input.LanguageID),
		Pool:       pool,
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

func addVectorFile(store *vector.Store, vectorizer *vector.TextVectorizer, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("vector search: read %s: %w", path, err)
	}

	if !utf8.Valid(data) {
		return fmt.Errorf("vector search: %s is not valid UTF-8", path)
	}

	clean := filepath.Clean(path)
	text, safety := retrieval.Sanitize(privacy.RedactText(string(data)), retrieval.PolicyContext{
		Source:     retrieval.Source{Type: retrieval.SourceVector, Name: clean, URI: clean},
		DocumentID: clean,
		Path:       clean,
	})

	vec, err := vectorizer.Vectorize(text)
	if err != nil {
		return fmt.Errorf("vector search: vectorize %s: %w", path, err)
	}

	metadata := map[string]string{"path": clean}
	if info, statErr := os.Stat(path); statErr == nil && !info.ModTime().IsZero() {
		metadata[retrieval.MetadataSourceUpdatedAt] = info.ModTime().UTC().Format(time.RFC3339Nano)
	}

	if !retrieval.IsDefaultSafety(safety) {
		metadata = retrieval.MergeSafetyMetadata(metadata, safety)
	}

	if err := store.Add(vector.Document{
		ID:         clean,
		Text:       text,
		Vector:     vec,
		Vectorizer: vectorizer.Spec(),
		Metadata:   metadata,
		Provenance: map[string]string{"source_type": "file", "path": clean},
	}); err != nil {
		return fmt.Errorf("vector search: index %s: %w", path, err)
	}

	return nil
}

func runAgentMemoryCommand(root, selectedAgent string, input agentMemoryCommandInput) error {
	agentName := strings.TrimSpace(input.Agent)
	if agentName == "" {
		agentName = strings.TrimSpace(selectedAgent)
	}

	if agentName == "" && agentMemoryCommandNeedsAgent(input) {
		return errors.New("agent memory: --agent-memory-agent or --agent is required")
	}

	storePath := strings.TrimSpace(input.StorePath)
	if storePath == "" {
		storePath = filepath.Join(root, ".atteler", "agent-memory.json")
	}

	store, err := loadAgentMemoryStore(storePath, input.Migrate)
	if err != nil {
		return err
	}

	storeChanged, messages, err := mutateAgentMemoryStore(store, agentName, storePath, input)
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

	results, err := store.Search(agentName, input.Search, limit)
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

func agentMemoryCommandNeedsAgent(input agentMemoryCommandInput) bool {
	return strings.TrimSpace(input.Search) != "" ||
		strings.TrimSpace(input.DeleteID) != "" ||
		len(input.IndexFiles) > 0
}

func mutateAgentMemoryStore(
	store *agentmemory.Store,
	agentName string,
	storePath string,
	input agentMemoryCommandInput,
) (storeChanged bool, messages []string, err error) {
	messages = make([]string, 0, len(input.IndexFiles)+3)

	if input.Migrate {
		storeChanged = true

		messages = append(messages, "Migrated agent memory store "+memoryDisplayValue(storePath))
	}

	deleted, message := deleteAgentMemoryDocument(store, agentName, storePath, input.DeleteID)
	storeChanged = storeChanged || deleted
	messages = appendAgentMemoryMessage(messages, message)

	compacted, message := compactAgentMemoryDocuments(store, storePath, input.Compact)
	storeChanged = storeChanged || compacted
	messages = appendAgentMemoryMessage(messages, message)

	indexedMessage, err := indexAgentMemoryFiles(store, agentName, storePath, input.IndexFiles, input.TTLSeconds)
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

func compactAgentMemoryDocuments(store *agentmemory.Store, storePath string, enabled bool) (changed bool, message string) {
	if !enabled {
		return false, ""
	}

	removed := store.Compact(time.Now().UTC())
	message = fmt.Sprintf("Compacted %d expired agent memory document(s) from %s", removed, memoryDisplayValue(storePath))

	return true, message
}

func indexAgentMemoryFiles(store *agentmemory.Store, agentName, storePath string, paths []string, ttlSeconds int) (string, error) {
	opts := agentMemoryIndexOptions(ttlSeconds)
	for _, path := range paths {
		if addErr := store.AddFileWithOptions(agentName, path, opts...); addErr != nil {
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

func loadAgentMemoryStore(path string, migrate bool) (*agentmemory.Store, error) {
	if _, err := os.Stat(path); err != nil {
		return loadMissingAgentMemoryStore(path, migrate, err)
	}

	loadOptions := agentmemory.LoadOptions{Migrate: migrate}

	store, err := agentmemory.LoadWithOptions(path, loadOptions)
	if err != nil {
		return nil, fmt.Errorf("agent memory: load store: %w", err)
	}

	return store, nil
}

func loadMissingAgentMemoryStore(path string, migrate bool, statErr error) (*agentmemory.Store, error) {
	if !errors.Is(statErr, os.ErrNotExist) {
		return nil, fmt.Errorf("agent memory: stat store %s: %w", path, statErr)
	}

	if migrate {
		return nil, fmt.Errorf("agent memory: migrate store %s: %w", path, statErr)
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
	path  string
	saved session.Session
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

	sources, err := selectedRetrievalSources(input, workspaceVectorEnabled(state.vectorConfig))
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

func selectedRetrievalSources(input retrievalCommandInput, includeWorkspaceVector bool) ([]retrieval.SourceType, error) {
	if len(input.Sources) == 0 {
		return []retrieval.SourceType{retrieval.SourceMemory, retrieval.SourceFile, retrieval.SourceSession}, nil
	}

	seen := make(map[retrieval.SourceType]struct{}, len(input.Sources))

	out := make([]retrieval.SourceType, 0, len(input.Sources))
	for _, raw := range input.Sources {
		source, all, err := parseRetrievalSource(raw)
		if err != nil {
			return nil, err
		}

		if all {
			return allRetrievalSources(input, includeWorkspaceVector), nil
		}

		if _, ok := seen[source]; ok {
			continue
		}

		seen[source] = struct{}{}
		out = append(out, source)
	}

	return out, nil
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
	case "vector", "vectors":
		return retrieval.SourceVector, false, nil
	case memoryAgentKey, "agent_memory", "agentmemory":
		return retrieval.SourceAgentMemory, false, nil
	default:
		return "", false, fmt.Errorf("retrieval: unknown source %q", raw)
	}
}

func allRetrievalSources(input retrievalCommandInput, includeWorkspaceVector bool) []retrieval.SourceType {
	sources := []retrieval.SourceType{
		retrieval.SourceMemory,
		retrieval.SourceFile,
		retrieval.SourceSession,
		retrieval.SourceGitHistory,
	}
	if len(input.VectorIndexFiles) > 0 || includeWorkspaceVector {
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
			if shouldSkipEmptyWorkspaceVectorSource(input, sources, source, err) {
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

func shouldSkipEmptyWorkspaceVectorSource(
	input retrievalCommandInput,
	sources []retrieval.SourceType,
	source retrieval.SourceType,
	err error,
) bool {
	return source == retrieval.SourceVector &&
		len(sources) > 1 &&
		len(input.VectorIndexFiles) == 0 &&
		retrievalSourceAllRequested(input.Sources) &&
		errors.Is(err, vector.ErrNoSources)
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

		return githistory.NewIndex(commits), nil
	case retrieval.SourceVector:
		return buildVectorRetrievalSearcher(ctx, state, input.VectorIndexFiles)
	case retrieval.SourceAgentMemory:
		return buildAgentMemoryRetrievalSearcher(state.cwd, state.selectedAgent, input)
	default:
		return nil, fmt.Errorf("retrieval: unsupported source %q", source)
	}
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

func buildVectorRetrievalSearcher(ctx context.Context, state appState, paths []string) (retrieval.Searcher, error) {
	if len(paths) == 0 {
		return buildWorkspaceVectorRetrievalSearcher(ctx, state)
	}

	vectorizer, err := vector.NewTextVectorizer(0)
	if err != nil {
		return nil, fmt.Errorf("retrieval: create vectorizer: %w", err)
	}

	store, err := vector.NewStoreWithVectorizer(vectorizer.Spec())
	if err != nil {
		return nil, fmt.Errorf("retrieval: create vector store: %w", err)
	}

	for _, path := range paths {
		if err := addVectorFile(store, vectorizer, path); err != nil {
			return nil, err
		}
	}

	return vector.Searcher{
		Store:      store,
		Vectorizer: vectorizer,
		Source:     retrieval.Source{Type: retrieval.SourceVector, Name: "local-vector-index"},
	}, nil
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

	return vector.IndexSearcher{
		Index:      idx,
		Vectorizer: opts.Vectorizer,
		Source: retrieval.Source{
			Type: retrieval.SourceVector,
			Name: "workspace",
			URI:  workspaceVectorSourceURI(opts),
		},
		ScorerName: settings.Vectorizer + "-workspace-ann",
	}, nil
}

func buildAgentMemoryRetrievalSearcher(root, selectedAgent string, input retrievalCommandInput) (retrieval.Searcher, error) {
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

	storePath := strings.TrimSpace(input.AgentMemoryStorePath)
	if storePath == "" {
		storePath = filepath.Join(root, ".atteler", "agent-memory.json")
	}

	store, err := loadAgentMemoryStore(storePath, false)
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

	if result.Snippet != "" {
		parts = append(parts, "snippet="+result.Snippet)
	}

	if explain && len(result.Scorer.Explanation) > 0 {
		parts = append(parts, "why="+strings.Join(result.Scorer.Explanation, " | "))
	}

	return strings.Join(parts, "\t")
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
