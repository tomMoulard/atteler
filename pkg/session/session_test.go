package session

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/llm"
)

func TestStore_SaveLoadByID(t *testing.T) {
	t.Parallel()
	store := NewStore(t.TempDir())
	session := New("gpt-4.1", nil)
	session.Append(llm.RoleUser, "hello")
	session.Append(llm.RoleAssistant, "hi")

	if err := store.Save(session); err != nil {
		require.NoError(t, err)
	}

	loaded, err := store.Load(session.ID)
	if err != nil {
		require.NoError(t, err)
	}

	if loaded.ID != session.ID {
		assert.Failf(t, "assertion failed", "ID = %q, want %q", loaded.ID, session.ID)
	}

	if loaded.DefaultModel != "gpt-4.1" {
		assert.Failf(t, "assertion failed", "DefaultModel = %q", loaded.DefaultModel)
	}

	if len(loaded.Messages) != 2 {
		require.Failf(t, "unexpected failure", "messages len = %d, want 2", len(loaded.Messages))
	}

	if loaded.Messages[0].Role != llm.RoleUser || loaded.Messages[0].Content != "hello" {
		assert.Failf(t, "assertion failed", "first message = %+v", loaded.Messages[0])
	}
}

func TestStore_LoadByPathInfersID(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	path := filepath.Join(dir, "manual.json")
	if err := os.WriteFile(path, []byte(`{"messages":[{"role":"user","content":"hi"}]}`), 0o600); err != nil {
		require.NoError(t, err)
	}

	loaded, err := NewStore(dir).Load(path)
	if err != nil {
		require.NoError(t, err)
	}

	if loaded.ID != "manual" {
		assert.Failf(t, "assertion failed", "ID = %q, want manual", loaded.ID)
	}
}

func TestStore_Path(t *testing.T) {
	t.Parallel()

	store := NewStore("/tmp/atteler-sessions")

	if got := store.Path("abc"); got != "/tmp/atteler-sessions/abc.json" {
		assert.Failf(t, "assertion failed", "Path(id) = %q", got)
	}

	if got := store.Path("abc.json"); got != "/tmp/atteler-sessions/abc.json" {
		assert.Failf(t, "assertion failed", "Path(json) = %q", got)
	}
}

func TestStore_List(t *testing.T) {
	t.Parallel()

	const defaultAgent = "writer"

	store := NewStore(t.TempDir())

	first := New("gpt-4.1", []llm.Message{{Role: llm.RoleUser, Content: "one"}})
	first.DefaultAgent = defaultAgent
	first.Title = "Review auth flow"

	first.Tags = []string{"auth", "review"}
	if err := store.Save(first); err != nil {
		require.NoError(t, err)
	}

	second := New("gpt-4.1-mini", []llm.Message{{Role: llm.RoleUser, Content: "two"}})
	if err := store.Save(second); err != nil {
		require.NoError(t, err)
	}

	summaries, err := store.List()
	if err != nil {
		require.NoError(t, err)
	}

	if len(summaries) != 2 {
		require.Failf(t, "unexpected failure", "summaries len = %d, want 2", len(summaries))
	}

	if summaries[0].Messages != 1 {
		assert.Failf(t, "assertion failed", "Messages = %d", summaries[0].Messages)
	}

	if summaries[1].DefaultAgent != defaultAgent && summaries[0].DefaultAgent != defaultAgent {
		require.Failf(t, "unexpected failure", "%s agent not found in summaries: %+v", defaultAgent, summaries)
	}

	if summaries[1].Title != first.Title && summaries[0].Title != first.Title {
		require.Failf(t, "unexpected failure", "title not found in summaries: %+v", summaries)
	}

	if len(summaries[1].Tags) == 0 && len(summaries[0].Tags) == 0 {
		require.Failf(t, "unexpected failure", "tags not found in summaries: %+v", summaries)
	}
}

func TestStore_Tags(t *testing.T) {
	t.Parallel()
	store := NewStore(t.TempDir())

	first := New("gpt-4.1", nil)

	first.Tags = []string{"auth", "review", "Auth"}
	if err := store.Save(first); err != nil {
		require.NoError(t, err)
	}

	second := New("gpt-4.1", nil)

	second.Tags = []string{"auth", "docs"}
	if err := store.Save(second); err != nil {
		require.NoError(t, err)
	}

	tags, err := store.Tags()
	if err != nil {
		require.NoError(t, err)
	}

	if len(tags) != 3 {
		require.Failf(t, "unexpected failure", "tags len = %d, want 3: %+v", len(tags), tags)
	}

	if tags[0].Tag != "auth" || tags[0].Sessions != 2 {
		require.Failf(t, "unexpected failure", "first tag = %+v, want auth count 2", tags[0])
	}
}

func TestStore_ListByTagFiltersExactNormalizedTag(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	first := New("gpt-4.1", nil)
	first.Title = "Auth one"
	first.Tags = []string{"auth", "review"}
	require.NoError(t, store.Save(first))

	second := New("gpt-4.1", nil)
	second.Title = "Auth two"
	second.Tags = []string{" Auth "}
	require.NoError(t, store.Save(second))

	third := New("gpt-4.1", nil)
	third.Title = "Docs"
	third.Tags = []string{"docs", "authoring"}
	require.NoError(t, store.Save(third))

	summaries, err := store.ListByTag(" AUTH ")
	require.NoError(t, err)
	require.Len(t, summaries, 2)
	assert.ElementsMatch(t, []string{"Auth one", "Auth two"}, []string{summaries[0].Title, summaries[1].Title})

	docs, err := store.ListByTag("docs")
	require.NoError(t, err)
	require.Len(t, docs, 1)
	assert.Equal(t, "Docs", docs[0].Title)
}

func TestStore_ListByTagRejectsEmptyTag(t *testing.T) {
	t.Parallel()

	_, err := NewStore(t.TempDir()).ListByTag(" \t ")
	require.Error(t, err)
	assert.ErrorContains(t, err, "tag is required")
}

func TestSession_RecordNegativeKnowledgeDeduplicatesNormalizedApproachAndReason(t *testing.T) {
	t.Parallel()

	session := New("gpt-4.1", nil)
	if ok := session.RecordNegativeKnowledge(" Try cache bust ", " Broke auth flow ", " abc123 ", " reviewer "); !ok {
		require.FailNow(t, "expected first negative knowledge entry to be recorded")
	}

	if ok := session.RecordNegativeKnowledge("try   CACHE bust", "broke   auth flow", "def456", "writer"); ok {
		require.FailNow(t, "expected duplicate negative knowledge entry to be skipped")
	}

	require.Len(t, session.NegativeKnowledge, 1)
	entry := session.NegativeKnowledge[0]
	assert.Equal(t, "Try cache bust", entry.Approach)
	assert.Equal(t, "Broke auth flow", entry.Reason)
	assert.Equal(t, "abc123", entry.Commit)
	assert.Equal(t, "reviewer", entry.Agent)
	assert.False(t, entry.CreatedAt.IsZero())
}

func TestSession_RecordNegativeKnowledgeRejectsMissingApproachOrReason(t *testing.T) {
	t.Parallel()

	session := New("gpt-4.1", nil)
	assert.False(t, session.RecordNegativeKnowledge("", "reason", "", ""))
	assert.False(t, session.RecordNegativeKnowledge("approach", " ", "", ""))
	assert.Empty(t, session.NegativeKnowledge)
}

func TestSession_RecordEvaluationAndArtifact(t *testing.T) {
	t.Parallel()

	session := New("gpt-4.1", nil)
	assert.True(t, session.RecordEvaluation(" reviewer ", " pass ", " solid ", " ref.md ", 95))
	assert.False(t, session.RecordEvaluation("", "pass", "", "", 0))
	assert.True(t, session.RecordArtifact("./plans/../plan.md", " research ", " useful ", " reviewer "))
	assert.False(t, session.RecordArtifact("", "research", "", ""))

	require.Len(t, session.Evaluations, 1)
	assert.Equal(t, "reviewer", session.Evaluations[0].Agent)
	assert.Equal(t, "pass", session.Evaluations[0].Outcome)
	assert.Equal(t, 95, session.Evaluations[0].Score)
	assert.False(t, session.Evaluations[0].CreatedAt.IsZero())

	require.Len(t, session.Artifacts, 1)
	assert.Equal(t, "plan.md", session.Artifacts[0].Path)
	assert.Equal(t, "research", session.Artifacts[0].Kind)
	assert.Equal(t, "reviewer", session.Artifacts[0].SourceAgent)
	assert.False(t, session.Artifacts[0].CreatedAt.IsZero())
}

func TestStore_LoadExistingJSONWithoutNegativeKnowledge(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	path := filepath.Join(dir, "legacy.json")
	if err := os.WriteFile(path, []byte(`{"messages":[{"role":"user","content":"hi"}]}`), 0o600); err != nil {
		require.NoError(t, err)
	}

	loaded, err := NewStore(dir).Load(path)
	require.NoError(t, err)

	assert.Equal(t, "legacy", loaded.ID)
	assert.Len(t, loaded.Messages, 1)
	assert.Empty(t, loaded.NegativeKnowledge)
	assert.Empty(t, loaded.Evaluations)
	assert.Empty(t, loaded.Artifacts)
}

func TestDefaultDir_EnvOverride(t *testing.T) {
	t.Setenv(EnvDir, "/tmp/custom-atteler-sessions")

	if got := DefaultDir(); got != "/tmp/custom-atteler-sessions" {
		assert.Failf(t, "assertion failed", "DefaultDir = %q", got)
	}
}
