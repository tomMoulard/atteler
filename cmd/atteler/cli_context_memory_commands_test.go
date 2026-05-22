package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/agentmemory"
	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/vector"
)

func TestFormatVectorResult(t *testing.T) {
	t.Parallel()

	got := formatVectorResult(vector.Result{
		Document: vector.Document{
			ID:       "docs/research.md",
			Metadata: map[string]string{"path": "docs/research.md"},
		},
		Score: 0.75,
	})

	want := "docs/research.md\tscore=0.7500\tpath=docs/research.md"
	if got != want {
		require.Failf(t, "unexpected vector result format", "got %q, want %q", got, want)
	}
}

func TestParseContextPackMessagesMapsDeveloperAndToolRoles(t *testing.T) {
	t.Parallel()

	messages, metadata := parseContextPackMessagesWithMetadata("developer[2026-05-22T10:00:00Z]: keep secret policy\ntool: grep output\nuser: continue\n")

	require.Len(t, messages, 3)
	require.Len(t, metadata, 3)
	assert.Equal(t, llm.RoleSystem, messages[0].Role)
	assert.Equal(t, "keep secret policy", messages[0].Content)
	assert.Equal(t, "2026-05-22T10:00:00Z", metadata[0].Timestamp)
	assert.Equal(t, llm.RoleTool, messages[1].Role)
	assert.Equal(t, "grep output", messages[1].Content)
	assert.Empty(t, metadata[1].Timestamp)
	assert.Equal(t, llm.RoleUser, messages[2].Role)
}

func TestParseContextPackMessagesDoesNotMistakeContentBracketForTimestamp(t *testing.T) {
	t.Parallel()

	messages, metadata := parseContextPackMessagesWithMetadata("user: keep literal ]: marker in content\n")

	require.Len(t, messages, 1)
	require.Len(t, metadata, 1)
	assert.Equal(t, llm.RoleUser, messages[0].Role)
	assert.Equal(t, "keep literal ]: marker in content", messages[0].Content)
	assert.Empty(t, metadata[0].Timestamp)
}

func TestParseContextPackTimestampWithoutSpaceKeepsContent(t *testing.T) {
	t.Parallel()

	messages, metadata := parseContextPackMessagesWithMetadata("user[2026-05-22T10:00:00Z]:continue without losing first byte\n")

	require.Len(t, messages, 1)
	require.Len(t, metadata, 1)
	assert.Equal(t, llm.RoleUser, messages[0].Role)
	assert.Equal(t, "continue without losing first byte", messages[0].Content)
	assert.Equal(t, "2026-05-22T10:00:00Z", metadata[0].Timestamp)
}

func TestRunAgentMemoryCommandIndexesAndSearchesSelectedAgent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	note := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(note, []byte("OAuth callback retry memory"), 0o600); err != nil {
		require.NoError(t, err)
	}

	storePath := filepath.Join(dir, "agent-memory.json")

	err := runAgentMemoryCommand(dir, "reviewer", agentMemoryCommandInput{
		StorePath:  storePath,
		Search:     "callback retry",
		IndexFiles: []string{note},
		Limit:      1,
	})

	require.NoError(t, err)
	loaded, err := agentmemory.Load(storePath)
	require.NoError(t, err)
	results, err := loaded.Search("reviewer", "callback", 1)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, filepath.Clean(note), results[0].Document.ID)
}

func TestFormatAgentMemoryResult(t *testing.T) {
	t.Parallel()

	got := formatAgentMemoryResult(agentmemory.Result{
		Document: agentmemory.Document{
			ID:       "docs/memory.md",
			Path:     "docs/memory.md",
			Metadata: map[string]string{"kind": "note"},
		},
		Score: 0.5,
	})

	want := "docs/memory.md\tscore=0.5000\tpath=docs/memory.md\tkind=note"
	if got != want {
		require.Failf(t, "unexpected agent memory result format", "got %q, want %q", got, want)
	}
}
