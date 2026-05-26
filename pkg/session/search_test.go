package session

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/retrieval"
)

func TestStore_SearchMessagesAndMetadata(t *testing.T) {
	t.Parallel()

	const (
		authQuery = "auth"
		reviewer  = "reviewer"
		writer    = "writer"
	)

	store := NewStore(t.TempDir())

	reviewerSession := New("gpt-review", []llm.Message{
		{Role: llm.RoleUser, Content: "Please review the auth flow"},
		{Role: llm.RoleAssistant, Content: "The auth flow needs tests."},
	})

	reviewerSession.DefaultAgent = reviewer
	if err := store.Save(reviewerSession); err != nil {
		require.NoError(t, err)
	}

	writerSession := New("gpt-write", []llm.Message{
		{Role: llm.RoleUser, Content: "Draft release notes"},
	})
	writerSession.DefaultAgent = writer
	writerSession.Title = "Release planning"

	writerSession.Tags = []string{"docs", "release"}
	if err := store.Save(writerSession); err != nil {
		require.NoError(t, err)
	}

	results, err := store.Search(authQuery)
	if err != nil {
		require.NoError(t, err)
	}

	if len(results) != 1 {
		require.Failf(t, "unexpected failure", "results len = %d, want 1: %+v", len(results), results)
	}

	if results[0].Summary.DefaultAgent != reviewer {
		require.Failf(t, "unexpected failure", "agent = %q, want reviewer", results[0].Summary.DefaultAgent)
	}

	if len(results[0].Snippets) == 0 || !strings.Contains(results[0].Snippets[0].Text, authQuery) {
		require.Failf(t, "unexpected failure", "snippet = %+v, want auth excerpt", results[0].Snippets)
	}

	assert.Equal(t, "message", results[0].Snippets[0].Kind)
	assert.Equal(t, 0, results[0].Snippets[0].Index)
	assert.Equal(t, retrieval.RangeUnitRuneOffset, results[0].Snippets[0].Range.Unit)
	assert.Greater(t, results[0].Snippets[0].Range.End, results[0].Snippets[0].Range.Start)

	results, err = store.Search("gpt-write")
	if err != nil {
		require.NoError(t, err)
	}

	if len(results) != 1 || results[0].Summary.DefaultAgent != writer {
		require.Failf(t, "unexpected failure", "metadata results = %+v, want writer session", results)
	}

	results, err = store.Search("release planning")
	if err != nil {
		require.NoError(t, err)
	}

	if len(results) != 1 || results[0].Summary.Title != writerSession.Title {
		require.Failf(t, "unexpected failure", "title results = %+v, want writer session title", results)
	}

	results, err = store.Search("docs")
	if err != nil {
		require.NoError(t, err)
	}

	if len(results) != 1 || results[0].Summary.DefaultAgent != writer {
		require.Failf(t, "unexpected failure", "tag results = %+v, want writer session", results)
	}
}

func TestStore_SearchEmptyQuery(t *testing.T) {
	t.Parallel()

	_, err := NewStore(t.TempDir()).Search(" ")
	if err == nil {
		require.FailNow(t, "expected empty query error")
	}
}

func TestStore_SearchNegativeKnowledge(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	session := New("gpt-review", []llm.Message{{Role: llm.RoleUser, Content: "Try something safe"}})
	session.Title = "Auth repair"
	require.True(t, session.RecordNegativeKnowledgeDetails(NegativeKnowledge{
		Approach: "Patch token refresh timer",
		Reason:   "Created retry storms",
		Commit:   "abc123",
		Agent:    "reviewer",
		TaskType: "migration",
		Severity: "critical",
	}))
	require.NoError(t, store.Save(session))

	results, err := store.Search("retry storms")
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Len(t, results[0].Snippets, 1)
	assert.Equal(t, llm.Role("negative_knowledge"), results[0].Snippets[0].Role)
	assert.Equal(t, "negative_knowledge", results[0].Snippets[0].Kind)
	assert.Equal(t, 0, results[0].Snippets[0].Index)
	assert.Contains(t, results[0].Snippets[0].Text, "Failed attempt: Patch token refresh timer")
	assert.Contains(t, results[0].Snippets[0].Text, "Reason: Created retry storms")

	results, err = store.Search("abc123")
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Contains(t, results[0].Snippets[0].Text, "Commit: abc123")

	results, err = store.Search("critical")
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Contains(t, results[0].Snippets[0].Text, "Severity: critical")
}

func TestStore_SearchEvaluationsAndArtifacts(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	session := New("gpt-review", nil)
	require.True(t, session.RecordEvaluationDetails(AgentEvaluation{
		Agent:          "reviewer",
		Outcome:        "pass",
		Notes:          "Caught OAuth bug",
		Reference:      "eval.md",
		Source:         EvaluationSourceHarness,
		RubricVersion:  "review/v2",
		TaskType:       "auth",
		Model:          "gpt-review",
		DurationMillis: 1200,
		Confidence:     0.91,
		Score:          90,
	}))
	require.True(t, session.RecordArtifact("docs/oauth.md", "research", "OAuth findings", "researcher"))
	require.NoError(t, store.Save(session))

	results, err := store.Search("oauth bug")
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Len(t, results[0].Snippets, 1)
	assert.Equal(t, llm.Role("evaluation"), results[0].Snippets[0].Role)
	assert.Equal(t, "evaluation", results[0].Snippets[0].Kind)
	assert.Equal(t, 0, results[0].Snippets[0].Index)
	assert.Contains(t, results[0].Snippets[0].Text, "Caught OAuth bug")

	results, err = store.Search("review/v2")
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, llm.Role("evaluation"), results[0].Snippets[0].Role)
	assert.Contains(t, results[0].Snippets[0].Text, "Rubric Version: review/v2")

	results, err = store.Search("0.91")
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, llm.Role("evaluation"), results[0].Snippets[0].Role)
	assert.Contains(t, results[0].Snippets[0].Text, "Confidence: 0.91")

	results, err = store.Search("findings")
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, llm.Role("artifact"), results[0].Snippets[0].Role)
	assert.Equal(t, "artifact", results[0].Snippets[0].Kind)
	assert.Equal(t, 0, results[0].Snippets[0].Index)
	assert.Contains(t, results[0].Snippets[0].Text, "OAuth findings")
}

func TestStore_SearchRetrievalMarksSessionsPrivateAndCitable(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	session := New("gpt-review", []llm.Message{{Role: llm.RoleUser, Content: "OAuth callback api_key=super-secret-token"}})
	session.Title = "Auth repair"
	require.NoError(t, store.Save(session))

	results, err := store.SearchRetrieval(context.Background(), retrieval.Query{Text: "OAuth callback", Limit: 1, IncludeUnsafe: true, Explain: true})
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, retrieval.SourceSession, results[0].Source.Type)
	assert.Equal(t, "session/"+session.ID, results[0].DocumentID)
	assert.NotEmpty(t, results[0].Chunk.ID)
	assert.Equal(t, "message", results[0].Metadata["kind"])
	assert.Equal(t, "0", results[0].Metadata["index"])
	assert.Equal(t, string(llm.RoleUser), results[0].Metadata["role"])
	assert.Equal(t, retrieval.RangeUnitRuneOffset, results[0].Chunk.Range.Unit)
	assert.Greater(t, results[0].Chunk.Range.End, results[0].Chunk.Range.Start)
	assert.NotEmpty(t, results[0].Metadata[retrieval.MetadataStableID])
	assert.NotEmpty(t, results[0].Metadata[retrieval.MetadataContentHash])
	assert.True(t, results[0].Safety.Private)
	assert.True(t, results[0].Safety.Redacted)
	assert.False(t, results[0].Safety.InjectAllowed)
	assert.NotContains(t, results[0].Snippet, "super-secret-token")
	assert.NotEmpty(t, results[0].Scorer.Explanation)
}

func TestStore_SearchRetrievalRequiresUnsafeOptInForSessionSnippets(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	saved := New("gpt-private", []llm.Message{{Role: llm.RoleUser, Content: "OAuth callback retry notes"}})
	require.NoError(t, store.Save(saved))

	results, err := store.SearchRetrieval(context.Background(), retrieval.Query{Text: "OAuth callback", Limit: 1})
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.True(t, results[0].Safety.Private)
	assert.False(t, results[0].Safety.InjectAllowed)
	assert.Contains(t, results[0].Safety.Reasons, "private session transcript")

	filtered, err := retrieval.Search(context.Background(), retrieval.Query{Text: "OAuth callback", Limit: 1}, store)
	require.NoError(t, err)
	assert.Empty(t, filtered)

	included, err := retrieval.Search(context.Background(), retrieval.Query{Text: "OAuth callback", Limit: 1, IncludeUnsafe: true}, store)
	require.NoError(t, err)
	require.Len(t, included, 1)
	assert.Equal(t, "session/"+saved.ID, included[0].DocumentID)
}

func TestStore_SearchRetrievalRedactsSensitiveSessionMetadata(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	saved := New("gpt-metadata", nil)
	saved.Title = "OAuth api_key=metadata-secret-token"
	require.NoError(t, store.Save(saved))

	results, err := store.SearchRetrieval(context.Background(), retrieval.Query{
		Text:          "OAuth",
		Limit:         1,
		IncludeUnsafe: true,
	})
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.True(t, results[0].Safety.Redacted)
	assert.Equal(t, "metadata", results[0].Metadata["kind"])
	assert.NotContains(t, results[0].Metadata["session_title"], "metadata-secret-token")
	assert.Contains(t, results[0].Metadata["session_title"], "[REDACTED]")
}
