package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/tommoulard/atteler/pkg/agent"
	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/promptcomplete"
	"github.com/tommoulard/atteler/pkg/session"
)

func completionCandidates(input string, agents *agent.Registry, root string, limit int) ([]completionCandidate, bool) {
	_, prefix, ok := activeAtToken(input)
	if !ok {
		return nil, false
	}

	if limit <= 0 {
		limit = 8
	}

	var out []completionCandidate

	prefixLower := strings.ToLower(prefix)
	if !strings.ContainsAny(prefix, `/\.`) {
		for _, name := range agents.List() {
			if strings.HasPrefix(strings.ToLower(name), prefixLower) {
				out = append(out, completionCandidate{
					kind:  "agent",
					label: "@" + name,
					value: "@" + name + " ",
				})
				if len(out) >= limit {
					return out, true
				}
			}
		}
	}

	fileCandidates := pathCompletionCandidates(root, prefix, limit-len(out))
	out = append(out, fileCandidates...)

	return out, true
}

func activeAtToken(input string) (start int, prefix string, ok bool) {
	if input == "" {
		return 0, "", false
	}

	end := len(input)

	start = end
	for start > 0 {
		r, size := lastRune(input[:start])
		if r == 0 || r == '\n' || r == '\t' || r == ' ' {
			break
		}

		start -= size
	}

	token := input[start:end]
	if !strings.HasPrefix(token, "@") {
		return 0, "", false
	}

	return start, strings.TrimPrefix(token, "@"), true
}

func lastRune(value string) (r rune, size int) {
	if value == "" {
		return 0, 0
	}

	r = rune(value[len(value)-1])
	if r < utf8.RuneSelf {
		return r, 1
	}

	r, size = utf8.DecodeLastRuneInString(value)

	return r, size
}

func pathCompletionCandidates(root, prefix string, limit int) []completionCandidate {
	if limit <= 0 || filepath.IsAbs(prefix) {
		return nil
	}

	if root == "" {
		var err error

		root, err = os.Getwd()
		if err != nil {
			return nil
		}
	}

	dirPart, base := pathCompletionParts(prefix)

	dir := filepath.Join(root, dirPart)
	if !pathInsideRoot(root, dir) {
		return nil
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	out := make([]completionCandidate, 0, min(limit, len(entries)))
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") && !strings.HasPrefix(base, ".") {
			continue
		}

		if base != "" && !strings.HasPrefix(strings.ToLower(name), strings.ToLower(base)) {
			continue
		}

		rel := name
		if dirPart != "." {
			rel = filepath.Join(dirPart, name)
		}

		value := "@" + filepath.ToSlash(rel)
		if entry.IsDir() {
			value += "/"
		}

		out = append(out, completionCandidate{
			kind:  "path",
			label: value,
			value: value,
		})
		if len(out) >= limit {
			break
		}
	}

	return out
}

func pathCompletionParts(prefix string) (dirPart, base string) {
	cleanPrefix := filepath.Clean(filepath.FromSlash(prefix))
	if cleanPrefix == "." {
		cleanPrefix = ""
	}

	dirPart = filepath.Dir(cleanPrefix)

	base = filepath.Base(cleanPrefix)
	if prefix == "" || !strings.ContainsAny(prefix, `/\`) {
		return ".", cleanPrefix
	}

	return dirPart, base
}

func pathInsideRoot(root, path string) bool {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return false
	}

	pathAbs, err := filepath.Abs(path)
	if err != nil {
		return false
	}

	rel, err := filepath.Rel(rootAbs, pathAbs)
	if err != nil {
		return false
	}

	return rel == "." || (!strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != ".." && !filepath.IsAbs(rel))
}

func applyCompletionCandidate(input, value string) string {
	start, _, ok := activeAtToken(input)
	if !ok {
		return input
	}

	return input[:start] + value
}

func applyPromptSuggestion(input string, suggestion promptcomplete.Suggestion) string {
	if suggestion.ReplacementStart < 0 ||
		suggestion.ReplacementEnd < suggestion.ReplacementStart ||
		suggestion.ReplacementEnd > len(input) {
		return input + suggestion.Suffix
	}

	return input[:suggestion.ReplacementStart] + suggestion.Text + input[suggestion.ReplacementEnd:]
}

func promptHistoryFromStore(store *session.Store, current session.Session, limit int) []string {
	if limit <= 0 {
		return nil
	}

	seen := make(map[string]bool)

	out := appendUserPromptsNewestFirst(nil, seen, current.Messages, limit)
	if len(out) >= limit || store == nil {
		return out
	}

	summaries, err := store.List()
	if err != nil {
		return out
	}

	// Bound the number of sessions loaded from disk. The list is already
	// sorted by UpdatedAt descending, so we scan only the most recent ones.
	const maxSessionsToScan = 20

	scanned := 0
	for i := range summaries {
		if scanned >= maxSessionsToScan || len(out) >= limit {
			break
		}

		summary := &summaries[i]
		if summary.ID == current.ID {
			continue
		}

		saved, err := store.Load(summary.ID)
		if err != nil {
			continue
		}

		scanned++

		out = appendUserPromptsNewestFirst(out, seen, saved.Messages, limit)
	}

	return out
}

func appendUserPromptsNewestFirst(out []string, seen map[string]bool, messages []llm.Message, limit int) []string {
	for i := len(messages) - 1; i >= 0 && len(out) < limit; i-- {
		if messages[i].Role != llm.RoleUser {
			continue
		}

		prompt := strings.TrimSpace(messages[i].Content)

		promptKey := normalizePromptHistoryKey(prompt)
		if promptKey == "" || seen[promptKey] {
			continue
		}

		seen[promptKey] = true

		out = append(out, prompt)
	}

	return out
}

func prependPromptHistory(prompt string, history []string, limit int) []string {
	if limit <= 0 {
		return nil
	}

	prompt = strings.TrimSpace(prompt)

	promptKey := normalizePromptHistoryKey(prompt)
	if promptKey == "" {
		return append([]string(nil), history...)
	}

	out := []string{prompt}

	for _, item := range history {
		if normalizePromptHistoryKey(item) == promptKey {
			continue
		}

		out = append(out, item)
		if len(out) >= limit {
			break
		}
	}

	return out
}

func normalizePromptHistoryKey(prompt string) string {
	return strings.ToLower(strings.Join(strings.Fields(prompt), " "))
}

// callLLM sends the messages to the selected LLM and returns a command that
// resolves with an llmResponseMsg. If no model is selected it uses the
// registry default. When useTools is true, the call runs an agentic loop
// that lets the LLM invoke tools (bash commands) iteratively.
