package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/tommoulard/atteler/pkg/agentmemory"
	"github.com/tommoulard/atteler/pkg/contextpack"
	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/lsp"
	"github.com/tommoulard/atteler/pkg/memory"
	"github.com/tommoulard/atteler/pkg/session"
	"github.com/tommoulard/atteler/pkg/vector"
)

func runLSPSymbols(ctx context.Context, input lspSymbolsCommandInput) error {
	lspOptions := lsp.Options{
		Command:    strings.TrimSpace(input.Command),
		Args:       append([]string(nil), input.Args...),
		FilePath:   strings.TrimSpace(input.FilePath),
		RootPath:   strings.TrimSpace(input.RootPath),
		LanguageID: strings.TrimSpace(input.LanguageID),
	}

	var (
		symbols []lsp.Symbol
		err     error
	)
	if strings.TrimSpace(input.WorkspaceSymbols) != "" {
		symbols, err = lsp.WorkspaceSymbols(ctx, lspOptions, input.WorkspaceSymbols)
	} else {
		symbols, err = lsp.DocumentSymbols(ctx, lspOptions)
	}

	if err != nil {
		return fmt.Errorf("lsp symbols: %w", err)
	}

	fmt.Print(formatLSPSymbols(symbols))

	return nil
}

func formatLSPSymbols(symbols []lsp.Symbol) string {
	if len(symbols) == 0 {
		return "No LSP symbols found.\n"
	}

	var b strings.Builder
	writeLSPSymbols(&b, symbols, 0)

	return b.String()
}

func writeLSPSymbols(b *strings.Builder, symbols []lsp.Symbol, depth int) {
	indent := strings.Repeat("  ", depth)

	for i := range symbols {
		symbol := symbols[i]

		parts := []string{
			indent + symbol.Name,
			"kind=" + strconv.Itoa(symbol.Kind),
			"range=" + formatLSPRange(symbol.Range),
		}
		if symbol.Detail != "" {
			parts = append(parts, "detail="+symbol.Detail)
		}

		if symbol.ContainerName != "" {
			parts = append(parts, "container="+symbol.ContainerName)
		}

		if symbol.URI != "" {
			parts = append(parts, "uri="+symbol.URI)
		}

		b.WriteString(strings.Join(parts, "\t"))
		b.WriteString("\n")
		writeLSPSymbols(b, symbol.Children, depth+1)
	}
}

func formatLSPRange(r lsp.Range) string {
	return fmt.Sprintf("%d:%d-%d:%d", r.Start.Line, r.Start.Character, r.End.Line, r.End.Character)
}

func runContextPack(path string, maxTokens int, model string) error {
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
	fmt.Print(formatContextPackResult(result))

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

	roleName, timestamp := parseRoleNameAndTimestamp(roleText)
	metadata := contextpack.MessageMetadata{Timestamp: timestamp}

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

func parseRoleNameAndTimestamp(roleText string) (roleName, timestamp string) {
	roleText = strings.TrimSpace(roleText)
	if !strings.HasSuffix(roleText, "]") {
		return roleText, ""
	}

	start := strings.LastIndex(roleText, "[")
	if start <= 0 {
		return roleText, ""
	}

	roleName = strings.TrimSpace(roleText[:start])

	timestamp = strings.TrimSpace(roleText[start+1 : len(roleText)-1])
	if roleName == "" || timestamp == "" {
		return roleText, ""
	}

	return roleName, timestamp
}

func formatContextPackResult(result contextpack.Result) string {
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
		fmt.Fprintf(&b, "budget_failure: %s\n", stats.BudgetFailureReason)
	}

	b.WriteString("output:\n")

	for _, message := range result.Messages {
		fmt.Fprintf(&b, "  %s: %s\n", message.Role, strings.ReplaceAll(message.Content, "\n", "\n    "))
	}

	return b.String()
}

func runVectorSearch(input vectorSearchCommandInput) error {
	if strings.TrimSpace(input.Query) == "" {
		return errors.New("vector search: --vector-search is required")
	}

	if len(input.IndexFiles) == 0 {
		return errors.New("vector search: at least one --vector-index file is required")
	}

	limit := input.Limit
	if limit == 0 {
		limit = 5
	}

	vectorizer, err := vector.NewTextVectorizer(0)
	if err != nil {
		return fmt.Errorf("vector search: create vectorizer: %w", err)
	}

	store, err := vector.NewStore(vectorizer.Dimensions)
	if err != nil {
		return fmt.Errorf("vector search: create store: %w", err)
	}

	for _, path := range input.IndexFiles {
		addErr := addVectorFile(store, vectorizer, path)
		if addErr != nil {
			return addErr
		}
	}

	queryVector, err := vectorizer.Vectorize(input.Query)
	if err != nil {
		return fmt.Errorf("vector search: vectorize query: %w", err)
	}

	results, err := store.Search(queryVector, limit)
	if err != nil {
		return fmt.Errorf("vector search failed: %w", err)
	}

	if len(results) == 0 {
		fmt.Println("No vector results found.")
		return nil
	}

	for i := range results {
		fmt.Println(formatVectorResult(results[i]))
	}

	return nil
}

func addVectorFile(store *vector.Store, vectorizer *vector.TextVectorizer, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("vector search: read %s: %w", path, err)
	}

	if !utf8.Valid(data) {
		return fmt.Errorf("vector search: %s is not valid UTF-8", path)
	}

	vec, err := vectorizer.Vectorize(string(data))
	if err != nil {
		return fmt.Errorf("vector search: vectorize %s: %w", path, err)
	}

	clean := filepath.Clean(path)
	if err := store.Add(vector.Document{ID: clean, Text: string(data), Vector: vec, Metadata: map[string]string{"path": clean}}); err != nil {
		return fmt.Errorf("vector search: index %s: %w", path, err)
	}

	return nil
}

func formatVectorResult(result vector.Result) string {
	parts := []string{
		result.Document.ID,
		fmt.Sprintf("score=%.4f", result.Score),
	}
	if path := result.Document.Metadata["path"]; path != "" {
		parts = append(parts, "path="+path)
	}

	return strings.Join(parts, "\t")
}

func runAgentMemoryCommand(root, selectedAgent string, input agentMemoryCommandInput) error {
	agentName := strings.TrimSpace(input.Agent)
	if agentName == "" {
		agentName = strings.TrimSpace(selectedAgent)
	}

	if agentName == "" {
		return errors.New("agent memory: --agent-memory-agent or --agent is required")
	}

	storePath := strings.TrimSpace(input.StorePath)
	if storePath == "" {
		storePath = filepath.Join(root, ".atteler", "agent-memory.json")
	}

	store, err := loadAgentMemoryStore(storePath)
	if err != nil {
		return err
	}

	for _, path := range input.IndexFiles {
		if addErr := store.AddFile(agentName, path); addErr != nil {
			return fmt.Errorf("agent memory: index %s: %w", path, addErr)
		}
	}

	if len(input.IndexFiles) > 0 {
		if saveErr := store.Save(storePath); saveErr != nil {
			return fmt.Errorf("agent memory: save store: %w", saveErr)
		}

		fmt.Printf("Indexed %d file(s) for agent %s in %s\n", len(input.IndexFiles), agentName, storePath)
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

func loadAgentMemoryStore(path string) (*agentmemory.Store, error) {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			store, newErr := agentmemory.NewStore(0)
			if newErr != nil {
				return nil, fmt.Errorf("agent memory: create store: %w", newErr)
			}

			return store, nil
		}

		return nil, fmt.Errorf("agent memory: stat store %s: %w", path, err)
	}

	store, err := agentmemory.Load(path)
	if err != nil {
		return nil, fmt.Errorf("agent memory: load store: %w", err)
	}

	return store, nil
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

func runMemoryCommand(store *session.Store, input memoryCommandInput) error {
	mem, err := buildMemoryStore(store, input)
	if err != nil {
		return err
	}

	if input.StorePath != "" && len(input.IndexFiles) > 0 {
		if saveErr := mem.Save(input.StorePath); saveErr != nil {
			return fmt.Errorf("memory: save store: %w", saveErr)
		}

		if input.Search == "" {
			fmt.Printf("Indexed %d document(s) into %s\n", len(mem.Documents), input.StorePath)
			return nil
		}
	}

	if input.Search == "" {
		return errors.New("memory: --memory-search is required unless indexing into --memory-store")
	}

	limit := input.Limit
	if limit == 0 {
		limit = 5
	}

	results, err := mem.Search(input.Search, limit)
	if err != nil {
		return fmt.Errorf("memory: search: %w", err)
	}

	if len(results) == 0 {
		fmt.Println("No memory results found.")
		return nil
	}

	for i := range results {
		fmt.Println(formatMemoryResult(results[i]))
	}

	return nil
}

func buildMemoryStore(store *session.Store, input memoryCommandInput) (*memory.Store, error) {
	mem, err := loadMemoryStore(input.StorePath)
	if err != nil {
		return nil, err
	}

	if len(input.IndexFiles) > 0 {
		if err := mem.AddFiles(input.IndexFiles...); err != nil {
			return nil, fmt.Errorf("memory: index files: %w", err)
		}
	}

	if input.StorePath == "" || len(mem.Documents) == 0 {
		if err := addSessionMemory(mem, store); err != nil {
			return nil, err
		}
	}

	return mem, nil
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

func addSessionMemory(mem *memory.Store, store *session.Store) error {
	summaries, err := store.List()
	if err != nil {
		return fmt.Errorf("memory: list sessions: %w", err)
	}

	for i := range summaries {
		summary := &summaries[i]

		saved, err := store.Load(summary.Path)
		if err != nil {
			return fmt.Errorf("memory: load session %s: %w", summary.ID, err)
		}

		if err := mem.AddSession(saved); err != nil {
			return fmt.Errorf("memory: index session %s: %w", summary.ID, err)
		}
	}

	return nil
}

func formatMemoryResult(result memory.Result) string {
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
		return line
	}

	return line + "\n  " + result.Snippet
}
