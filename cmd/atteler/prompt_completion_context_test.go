package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/agent"
	"github.com/tommoulard/atteler/pkg/config"
	"github.com/tommoulard/atteler/pkg/promptcomplete"
	"github.com/tommoulard/atteler/pkg/session"
	"github.com/tommoulard/atteler/pkg/tasklist"
)

func TestPromptCompletionContext_UsesRepoSessionAndTaskSources(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "live.go"), []byte("package live\n\nfunc LiveSymbol() {}\n"), 0o600))

	store := session.NewStore(filepath.Join(dir, ".atteler", "sessions"))
	taskStore := tasklist.NewStore(taskListPath(store, ""))
	_, err := taskStore.Add(context.Background(), tasklist.AddRequest{
		ID:    "GH-27",
		Title: "Make prompt completion context-aware",
	})
	require.NoError(t, err)

	state := appState{
		cwd:          dir,
		sessionStore: store,
		sessionState: session.Session{
			Title: "Prompt completion work for GH-27 and #15",
			Artifacts: []session.Artifact{{
				Path:    "docs/notes.md",
				Kind:    "notes",
				Summary: "context source notes",
			}},
		},
	}

	completionContext := promptCompletionContext(context.Background(), state, "fix symbol Live", true)
	suggestions := promptcomplete.SuggestAll(completionContext, promptcomplete.Options{})

	require.NotEmpty(t, suggestions)
	assert.Equal(t, "LiveSymbol", suggestions[0].Text)
	assert.Equal(t, "project-symbol", suggestions[0].Candidate.Kind)
	assert.NotEmpty(t, completionContext.Tasks)
	assert.Contains(t, candidateTexts(completionContext.Tasks), "GH-27")
	assert.Contains(t, candidateTexts(completionContext.Issues), "#15")
	assert.Contains(t, candidateTexts(completionContext.Issues), "GH-27")
	assert.Equal(t, 1, countCandidateText(completionContext.Issues, "GH-27"))
	assert.Contains(t, candidateTexts(completionContext.RecentFiles), "docs/notes.md")
}

func TestParseGitStatusPath_RenameUsesNewPath(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "new/name.go", parseGitStatusPath("R  old/name.go -> new/name.go"))
	assert.Equal(t, "pkg/file.go", parseGitStatusPath(" M pkg/file.go"))
	assert.Empty(t, parseGitStatusPath(""))
}

func TestPromptCompletionContext_ExcludesHiddenAgents(t *testing.T) {
	t.Parallel()

	registry := agent.NewRegistry(map[string]config.AgentConfig{
		"visible":  {Description: "shown to users"},
		"internal": {Description: "hidden helper", Hidden: true},
	})

	completionContext := promptCompletionContext(context.Background(), appState{agentRegistry: registry}, "ask vis", false)

	assert.Contains(t, candidateTexts(completionContext.Agents), "visible")
	assert.NotContains(t, candidateTexts(completionContext.Agents), "internal")
}

func candidateTexts(candidates []promptcomplete.Candidate) []string {
	out := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		out = append(out, candidate.Text)
	}

	return out
}

func countCandidateText(candidates []promptcomplete.Candidate, text string) int {
	count := 0

	for _, candidate := range candidates {
		if candidate.Text == text {
			count++
		}
	}

	return count
}
