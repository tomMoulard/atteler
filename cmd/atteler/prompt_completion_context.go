package main

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/tommoulard/atteler/pkg/agent"
	"github.com/tommoulard/atteler/pkg/codeintel"
	"github.com/tommoulard/atteler/pkg/promptcomplete"
	"github.com/tommoulard/atteler/pkg/session"
	"github.com/tommoulard/atteler/pkg/tasklist"
)

const maxPromptContextCandidates = 300

var issueRefPattern = regexp.MustCompile(`(?i)(?:\bGH-\d+\b|#\d+\b)`)

func promptCompletionContext(ctx context.Context, state appState, input string, includeRepo bool) promptcomplete.Context {
	completionContext := promptcomplete.Context{
		Input:         input,
		Cursor:        len(input),
		Agents:        promptAgentCandidates(state.agentRegistry),
		Tools:         promptToolCandidates(),
		Templates:     promptTemplateCandidates(),
		SlashCommands: promptSlashCommandCandidates(),
		RecentFiles:   promptSessionFileCandidates(state.sessionState),
		Issues:        promptIssueCandidatesFromSession(state.sessionState),
		Permissions:   promptPermissionCandidates(state.agentRegistry, state.selectedAgent),
	}

	tasks := promptTaskCandidates(ctx, state.sessionStore, state.cwd)
	completionContext.Tasks = tasks
	completionContext.Issues = append(completionContext.Issues, promptIssueCandidatesFromTasks(tasks)...)

	if includeRepo {
		completionContext.Issues = append(completionContext.Issues, promptGitIssueCandidates(ctx, state.cwd)...)
		completionContext.ProjectSymbols = promptProjectSymbolCandidates(state.cwd, input)
		completionContext.RecentFiles = append(completionContext.RecentFiles, promptGitRecentFileCandidates(ctx, state.cwd)...)
	}

	completionContext.Issues = dedupePromptCandidates(completionContext.Issues)
	completionContext.RecentFiles = dedupePromptCandidates(completionContext.RecentFiles)

	return completionContext
}

func dedupePromptCandidates(candidates []promptcomplete.Candidate) []promptcomplete.Candidate {
	if len(candidates) < 2 {
		return candidates
	}

	seen := make(map[string]struct{}, len(candidates))

	out := make([]promptcomplete.Candidate, 0, len(candidates))
	for _, candidate := range candidates {
		key := candidate.Kind + "\x00" + candidate.Text
		if _, ok := seen[key]; ok {
			continue
		}

		seen[key] = struct{}{}

		out = append(out, candidate)
	}

	return out
}

func promptSlashCommandCandidates() []promptcomplete.Candidate {
	commands := []struct {
		name        string
		description string
		tokens      []string
	}{
		{name: "/help", description: "show interactive slash command help"},
		{name: "/model", description: "show or change the active model", tokens: []string{"provider"}},
		{name: "/profile", description: "switch to a configured agent profile", tokens: []string{"agent"}},
		{name: "/save", description: "save the current session"},
		{name: "/export", description: "export the current session transcript", tokens: []string{"session"}},
		{name: "/clear", description: "clear the visible conversation"},
		{name: "/retry", description: "regenerate the last user prompt", tokens: []string{"regenerate"}},
		{name: "/edit", description: "edit the last user prompt"},
		{name: "/fork", description: "fork the current session at a message index", tokens: []string{"session"}},
		{name: "/tokens", description: "show token usage", tokens: []string{"cost"}},
		{name: "/cost", description: "show token cost summary", tokens: []string{"tokens"}},
		{name: "/search", description: "search the current conversation", tokens: []string{"session"}},
		{name: "/pin", description: "pin a message before pruning context", tokens: []string{"context"}},
		{name: "/unpin", description: "unpin a message", tokens: []string{"context"}},
		{name: "/context", description: "show or prune conversation context"},
		{name: "/mode", description: "switch between plan and execute modes", tokens: []string{"plan", "execute"}},
		{name: "/template", description: "insert a local prompt template"},
		{name: "/codeblocks", description: "list code blocks from the last assistant response", tokens: []string{"code"}},
		{name: "/save-code", description: "save a code block from the last assistant response", tokens: []string{"code"}},
		{name: "/copy", description: "copy the last answer or full session"},
		{name: "/copy-code", description: "copy a code block"},
		{name: "/apply-patch", description: "apply the last assistant patch", tokens: []string{"patch"}},
		{name: "/eval", description: "run local evaluation commands", tokens: []string{"test"}},
	}

	out := make([]promptcomplete.Candidate, 0, len(commands))
	for _, command := range commands {
		out = append(out, promptcomplete.Candidate{
			Text:        command.name,
			Kind:        "slash-command",
			Source:      "interactive slash commands",
			Description: command.description,
			Tokens:      command.tokens,
		})
	}

	return out
}

func promptProjectSymbolCandidates(root, input string) []promptcomplete.Candidate {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil
	}

	index, err := codeintel.IndexDir(root)
	if err != nil {
		return nil
	}

	prefix := strings.ToLower(promptCompletionPrefix(input))
	limit := min(len(index.Symbols), maxPromptContextCandidates)

	out := make([]promptcomplete.Candidate, 0, limit)
	for _, symbol := range index.Symbols {
		if len(out) >= limit {
			break
		}

		if prefix != "" && !symbolNameMatchesPrefix(symbol.Name, prefix) {
			continue
		}

		rel := relPath(root, symbol.File)
		out = append(out, promptcomplete.Candidate{
			Text:        symbol.Name,
			Kind:        "project-symbol",
			Source:      "project symbol index",
			Description: fmt.Sprintf("%s in %s:%d", symbol.Kind, filepath.ToSlash(rel), symbol.Line),
			Tokens:      []string{symbol.Kind, rel, filepath.Base(rel)},
		})
	}

	return out
}

func promptCompletionPrefix(input string) string {
	cursor := len(input)
	start := cursor

	for start > 0 && isPromptCompletionTokenByte(input[start-1]) {
		start--
	}

	return input[start:cursor]
}

func isPromptCompletionTokenByte(b byte) bool {
	return (b >= 'A' && b <= 'Z') ||
		(b >= 'a' && b <= 'z') ||
		(b >= '0' && b <= '9') ||
		strings.ContainsRune("_@$/.\\-:#", rune(b))
}

func symbolNameMatchesPrefix(name, prefix string) bool {
	name = strings.ToLower(name)

	return strings.HasPrefix(name, prefix) || (len(prefix) >= 3 && strings.Contains(name, prefix))
}

func promptGitRecentFileCandidates(ctx context.Context, root string) []promptcomplete.Candidate {
	root = strings.TrimSpace(root)
	if root == "" || ctx == nil {
		return nil
	}

	cmd := exec.CommandContext(ctx, "git", "-C", root, "status", "--short")

	output, err := cmd.Output()
	if err != nil {
		return nil
	}

	seen := make(map[string]struct{})
	files := make([]string, 0)

	for line := range strings.SplitSeq(string(output), "\n") {
		file := parseGitStatusPath(line)
		if file == "" {
			continue
		}

		if _, ok := seen[file]; ok {
			continue
		}

		seen[file] = struct{}{}
		files = append(files, file)
	}

	sort.Strings(files)

	out := make([]promptcomplete.Candidate, 0, min(len(files), maxPromptContextCandidates))
	for _, file := range files {
		if len(out) >= maxPromptContextCandidates {
			break
		}

		out = append(out, promptcomplete.Candidate{
			Text:        filepath.ToSlash(file),
			Kind:        "recent-file",
			Source:      "git status",
			Description: "recently touched file",
			Tokens:      []string{"git", "status", filepath.Base(file)},
		})
	}

	return out
}

func promptGitIssueCandidates(ctx context.Context, root string) []promptcomplete.Candidate {
	root = strings.TrimSpace(root)
	if root == "" || ctx == nil {
		return nil
	}

	cmd := exec.CommandContext(ctx, "git", "-C", root, "branch", "--show-current")

	output, err := cmd.Output()
	if err != nil {
		return nil
	}

	return issueCandidatesFromText("git branch", string(output))
}

func parseGitStatusPath(line string) string {
	line = strings.TrimSpace(line)
	if len(line) < 3 {
		return ""
	}

	path := strings.TrimSpace(line[2:])
	if path == "" {
		return ""
	}

	if _, after, ok := strings.Cut(path, " -> "); ok {
		path = strings.TrimSpace(after)
	}

	return path
}

func promptSessionFileCandidates(sessionState session.Session) []promptcomplete.Candidate {
	if len(sessionState.Artifacts) == 0 {
		return nil
	}

	out := make([]promptcomplete.Candidate, 0, len(sessionState.Artifacts))
	for _, artifact := range sessionState.Artifacts {
		path := strings.TrimSpace(artifact.Path)
		if path == "" {
			continue
		}

		out = append(out, promptcomplete.Candidate{
			Text:        filepath.ToSlash(path),
			Kind:        "recent-file",
			Source:      "session artifacts",
			Description: artifactDescription(artifact),
			Tokens:      []string{artifact.Kind, artifact.SourceAgent, filepath.Base(path)},
		})
	}

	return out
}

func artifactDescription(artifact session.Artifact) string {
	parts := []string{"session artifact"}
	if artifact.Kind != "" {
		parts = append(parts, "kind="+artifact.Kind)
	}

	if artifact.Summary != "" {
		parts = append(parts, artifact.Summary)
	}

	return strings.Join(parts, "; ")
}

func promptTaskCandidates(ctx context.Context, store *session.Store, root string) []promptcomplete.Candidate {
	if ctx == nil {
		return nil
	}

	taskStore := tasklist.NewStore(taskListPath(store, ""))

	tasks, err := taskStore.List(ctx)
	if err != nil {
		return nil
	}

	out := make([]promptcomplete.Candidate, 0, len(tasks))
	for i := range tasks {
		task := &tasks[i]

		description := string(task.Status)
		if task.Title != "" {
			description += ": " + task.Title
		}

		out = append(out, promptcomplete.Candidate{
			Text:        task.ID,
			Kind:        "task",
			Source:      "task list",
			Description: description,
			Tokens:      []string{string(task.Status), task.Agent, root},
		})
	}

	return out
}

func promptIssueCandidatesFromSession(sessionState session.Session) []promptcomplete.Candidate {
	texts := []string{sessionState.ID, sessionState.Title}
	texts = append(texts, sessionState.Tags...)

	for _, message := range sessionState.Messages {
		texts = append(texts, message.Content)
	}

	for _, artifact := range sessionState.Artifacts {
		texts = append(texts, artifact.Path, artifact.Kind, artifact.Summary)
	}

	return issueCandidatesFromText("session state", texts...)
}

func promptIssueCandidatesFromTasks(tasks []promptcomplete.Candidate) []promptcomplete.Candidate {
	texts := make([]string, 0, len(tasks)*2)
	for i := range tasks {
		task := &tasks[i]
		texts = append(texts, task.Text, task.Description)
	}

	return issueCandidatesFromText("task list", texts...)
}

func issueCandidatesFromText(source string, texts ...string) []promptcomplete.Candidate {
	seen := make(map[string]struct{})
	refs := make([]string, 0)

	for _, text := range texts {
		for _, ref := range issueRefPattern.FindAllString(text, -1) {
			ref = normalizeIssueRef(ref)
			if ref == "" {
				continue
			}

			if _, ok := seen[ref]; ok {
				continue
			}

			seen[ref] = struct{}{}
			refs = append(refs, ref)
		}
	}

	sort.Strings(refs)

	out := make([]promptcomplete.Candidate, 0, len(refs))
	for _, ref := range refs {
		out = append(out, promptcomplete.Candidate{
			Text:        ref,
			Kind:        "issue",
			Source:      source,
			Description: "issue reference from " + source,
			Tokens:      []string{"issue", "github"},
		})
	}

	return out
}

func normalizeIssueRef(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}

	if strings.HasPrefix(ref, "#") {
		return ref
	}

	return strings.ToUpper(ref[:2]) + ref[2:]
}

func promptPermissionCandidates(registry *agent.Registry, selectedAgent string) []promptcomplete.Candidate {
	out := []promptcomplete.Candidate{{
		Text:        "local-only",
		Kind:        "permission",
		Source:      "completion modes",
		Description: "use deterministic no-network completion context",
		Tokens:      []string{"local", "no-network", "offline"},
	}}

	if strings.TrimSpace(selectedAgent) == "" || registry == nil {
		return out
	}

	activeAgent, ok := registry.Get(selectedAgent)
	if !ok {
		return out
	}

	permissions := activeAgent.ToolPermissions
	if permissions == nil {
		out = append(out, promptcomplete.Candidate{
			Text:        "bash",
			Kind:        "permission",
			Source:      "agent permissions",
			Description: selectedAgent + " uses the default tool policy; bash is available with safety checks",
			Tokens:      []string{"tool", "allowed", selectedAgent},
		})

		return out
	}

	names := make([]string, 0, len(permissions))
	for name, allowed := range permissions {
		if allowed {
			names = append(names, name)
		}
	}

	sort.Strings(names)

	for _, name := range names {
		out = append(out, promptcomplete.Candidate{
			Text:        name,
			Kind:        "permission",
			Source:      "agent permissions",
			Description: selectedAgent + " is allowed to use tool " + name,
			Tokens:      []string{"tool", "allowed", selectedAgent},
		})
	}

	return out
}

func relPath(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}

	return rel
}
