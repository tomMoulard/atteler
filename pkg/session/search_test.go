package session

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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

	results, err = store.Search("assistant")
	if err != nil {
		require.NoError(t, err)
	}

	if len(results) != 1 || results[0].Snippets[0].Label != "messages[2].role" {
		require.Failf(t, "unexpected failure", "role results = %+v, want assistant message role", results)
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

func TestStore_SearchMissingDirectoryDoesNotCreateIndex(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "missing")
	store := NewStore(dir)

	results, err := store.Search("anything")
	require.NoError(t, err)
	assert.Empty(t, results)

	_, err = os.Stat(dir)
	assert.ErrorIs(t, err, os.ErrNotExist, "search should not create missing session dir")
}

func TestStore_SearchUsesIndexWithoutLoadingSessionFiles(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	session := New("gpt-review", []llm.Message{{Role: llm.RoleUser, Content: "indexed callback evidence"}})
	require.NoError(t, store.Save(session))

	indexData, err := os.ReadFile(store.searchIndexPath())
	require.NoError(t, err)
	assert.NotContains(t, string(indexData), store.Dir())

	sessionPath := store.Path(session.ID)
	sessionData, err := os.ReadFile(sessionPath)
	require.NoError(t, err)

	sessionInfo, err := os.Stat(sessionPath)
	require.NoError(t, err)

	corruptData := make([]byte, len(sessionData))
	for i := range corruptData {
		corruptData[i] = ' '
	}

	copy(corruptData, "{broken json")
	require.NoError(t, os.WriteFile(sessionPath, corruptData, 0o600))
	require.NoError(t, os.Chtimes(sessionPath, sessionInfo.ModTime(), sessionInfo.ModTime()))

	results, err := store.Search("callback evidence")
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, session.ID, results[0].Summary.ID)
	assert.Equal(t, sessionPath, results[0].Summary.Path)
}

func TestStore_SearchRebuildsIndexWhenSessionFilesChangeWithoutSave(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	session := New("gpt-review", []llm.Message{{Role: llm.RoleUser, Content: "original indexed needle"}})
	require.NoError(t, store.Save(session))

	added := Session{
		ID:        "manual",
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: "manual addition needle"}},
	}
	writeSessionSnapshot(t, store, added)

	results, err := store.Search("manual addition needle")
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "manual", results[0].Summary.ID)

	require.NoError(t, os.Remove(store.Path(session.ID)))

	results, err = store.Search("original indexed needle")
	require.NoError(t, err)
	assert.Empty(t, results)
}

func TestStore_SearchIndexIgnoresSaveTempFiles(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	session := New("gpt-review", []llm.Message{{Role: llm.RoleUser, Content: "durable session needle"}})
	require.NoError(t, store.Save(session))

	require.NoError(t, os.WriteFile(filepath.Join(store.Dir(), ".session-leftover.json"), []byte(`{not valid`), 0o600))

	results, err := store.Search("durable session needle")
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, session.ID, results[0].Summary.ID)
}

func TestStore_SearchIndexSurvivesConcurrentSaves(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())

	const sessions = 12

	var wg sync.WaitGroup

	errs := make(chan error, sessions)
	for i := range sessions {
		wg.Add(1)

		go func(index int) {
			defer wg.Done()

			session := New("gpt-review", []llm.Message{{Role: llm.RoleUser, Content: "shared concurrent needle"}})
			session.ID = fmt.Sprintf("concurrent-%02d", index)
			session.Title = fmt.Sprintf("Concurrent %02d", index)

			errs <- store.Save(session)
		}(i)
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		require.NoError(t, err)
	}

	results, err := store.Search("shared concurrent needle")
	require.NoError(t, err)
	require.Len(t, results, sessions)
}

func TestStore_SearchWithOptionsFiltersScopes(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	base := time.Now().UTC().Add(-4 * time.Hour)

	writeSessionSnapshot(t, store, Session{
		ID:                    "reviewer-repo-a",
		CreatedAt:             base,
		UpdatedAt:             base.Add(time.Hour),
		Title:                 "Repo A deploy",
		DefaultAgent:          "reviewer",
		DefaultModel:          "gpt-5",
		DefaultReasoningLevel: "high",
		WorktreePath:          "/workspace/repo-a",
		Tags:                  []string{"deploy", "backend"},
		Messages:              []llm.Message{{Role: llm.RoleUser, Content: "deploy rollback plan"}},
	})
	writeSessionSnapshot(t, store, Session{
		ID:           "writer-repo-a",
		CreatedAt:    base,
		UpdatedAt:    base.Add(2 * time.Hour),
		Title:        "Repo A writer",
		DefaultAgent: "writer",
		DefaultModel: "gpt-5",
		WorktreePath: "/workspace/repo-a",
		Tags:         []string{"deploy"},
		Messages:     []llm.Message{{Role: llm.RoleUser, Content: "deploy copy plan"}},
	})
	writeSessionSnapshot(t, store, Session{
		ID:           "reviewer-repo-b",
		CreatedAt:    base.Add(-48 * time.Hour),
		UpdatedAt:    base.Add(-47 * time.Hour),
		Title:        "Repo B deploy",
		DefaultAgent: "reviewer",
		DefaultModel: "gpt-5",
		WorktreePath: "/workspace/repo-b",
		Tags:         []string{"deploy"},
		Messages:     []llm.Message{{Role: llm.RoleUser, Content: "deploy repo b"}},
	})
	writeSessionSnapshot(t, store, Session{
		ID:             "branch-shadow",
		CreatedAt:      base,
		UpdatedAt:      base.Add(3 * time.Hour),
		Title:          "Branch shadow deploy",
		DefaultAgent:   "reviewer",
		DefaultModel:   "gpt-5",
		WorktreePath:   "/workspace/repo-c",
		WorktreeBranch: "repo-a",
		Tags:           []string{"deploy"},
		Messages:       []llm.Message{{Role: llm.RoleUser, Content: "deploy branch shadow"}},
	})
	writeSessionSnapshot(t, store, Session{
		ID:           "repo-name-shadow",
		CreatedAt:    base,
		UpdatedAt:    base.Add(4 * time.Hour),
		Title:        "Repo name shadow deploy",
		DefaultAgent: "reviewer",
		DefaultModel: "gpt-5",
		WorktreePath: "/workspace/repo-a-old",
		Tags:         []string{"deploy"},
		Messages:     []llm.Message{{Role: llm.RoleUser, Content: "deploy repo name shadow"}},
	})

	results, err := store.SearchWithOptions("deploy", SearchOptions{
		Agent:      "reviewer",
		Model:      "gpt-5",
		Repo:       "/workspace/repo-a",
		Tags:       []string{"backend"},
		SessionIDs: []string{"reviewer-repo-a", "writer-repo-a"},
		DateFrom:   base.Add(-time.Minute),
		DateTo:     base.Add(90 * time.Minute),
	})
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "reviewer-repo-a", results[0].Summary.ID)

	results, err = store.SearchWithOptions("deploy", SearchOptions{Repo: "repo-a"})
	require.NoError(t, err)
	require.Len(t, results, 2)
	assert.ElementsMatch(t, []string{"reviewer-repo-a", "writer-repo-a"}, searchResultIDs(results))

	results, err = store.SearchWithOptions("deploy", SearchOptions{Model: "high"})
	require.NoError(t, err)
	assert.Empty(t, results)

	results, err = store.Search("high")
	require.NoError(t, err)
	assert.Empty(t, results)

	results, err = store.SearchWithOptions("deploy", SearchOptions{SessionIDs: []string{" "}})
	require.NoError(t, err)
	require.Len(t, results, 5)
}

func TestStore_SearchFieldFiltersAndStableOffsets(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	session := New("gpt-review", []llm.Message{{Role: llm.RoleUser, Content: "alpha auth beta"}})
	session.ID = "session-field-needle"
	session.Title = "Auth title"
	session.WorktreePath = "/workspace/repo-field-needle"
	require.NoError(t, store.Save(session))

	results, err := store.SearchWithOptions("auth", SearchOptions{Fields: []SearchField{SearchFieldTranscript}})
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.NotEmpty(t, results[0].Matches)
	assert.Equal(t, SearchFieldTranscript, results[0].Matches[0].Field)
	assert.Equal(t, "messages[1].content", results[0].Matches[0].Label)
	assert.Equal(t, 6, results[0].Matches[0].Offset)
	assert.Equal(t, 10, results[0].Matches[0].End)

	results, err = store.SearchWithOptions("auth", SearchOptions{Fields: []SearchField{SearchFieldArtifacts}})
	require.NoError(t, err)
	assert.Empty(t, results)

	results, err = store.SearchWithOptions("session-field-needle", SearchOptions{Fields: []SearchField{SearchFieldSession}})
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, SearchFieldSession, results[0].Matches[0].Field)

	results, err = store.SearchWithOptions("repo-field-needle", SearchOptions{Fields: []SearchField{SearchFieldRepo}})
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, SearchFieldRepo, results[0].Matches[0].Field)

	agentSession := New("gpt-review", nil)
	require.True(t, agentSession.RecordNegativeKnowledge("cache patch", "missed invalidation", "", "failure-agent"))
	require.True(t, agentSession.RecordEvaluation("evaluation-agent", "pass", "", "", 0))
	require.True(t, agentSession.RecordArtifact("docs/research.md", "note", "", "artifact-agent"))
	require.NoError(t, store.Save(agentSession))

	for _, tc := range []struct {
		query string
		label string
	}{
		{query: "failure-agent", label: "negative_knowledge[1].agent"},
		{query: "evaluation-agent", label: "evaluations[1].agent"},
		{query: "artifact-agent", label: "artifacts[1].source_agent"},
	} {
		results, err = store.SearchWithOptions(tc.query, SearchOptions{Fields: []SearchField{SearchFieldAgent}})
		require.NoError(t, err)
		require.Len(t, results, 1)
		require.NotEmpty(t, results[0].Matches)
		assert.Equal(t, SearchFieldAgent, results[0].Matches[0].Field)
		assert.Equal(t, tc.label, results[0].Matches[0].Label)
	}
}

func TestStore_SearchFieldFiltersCoverIndexedFamilies(t *testing.T) {
	t.Parallel()

	store := NewStoreWithSearchIndexPolicy(t.TempDir(), SearchIndexPolicy{MaxTranscriptAge: -1})
	now := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	session := Session{
		ID:           "session-scope-needle",
		Title:        "title-scope-needle",
		CreatedAt:    now,
		UpdatedAt:    now,
		DefaultAgent: "agent-scope-needle",
		DefaultModel: "model-scope-needle",
		WorktreePath: "/workspace/repo-scope-needle",
		Tags:         []string{"tag-scope-needle"},
		Messages:     []llm.Message{{Role: llm.RoleUser, Content: "transcript-scope-needle"}},
		NegativeKnowledge: []NegativeKnowledge{{
			CreatedAt: now,
			Approach:  "failure-scope-needle",
			Reason:    "failed reason",
			Agent:     "failure-agent",
		}},
		Evaluations: []AgentEvaluation{{
			CreatedAt: now,
			Agent:     "evaluation-agent",
			Outcome:   "evaluation-scope-needle",
		}},
		Artifacts: []Artifact{{
			CreatedAt:   now,
			Path:        "docs/artifact.md",
			Kind:        "note",
			Summary:     "artifact-scope-needle",
			SourceAgent: "artifact-agent",
		}},
	}
	writeSessionSnapshot(t, store, session)

	for _, tc := range []struct {
		field SearchField
		query string
	}{
		{field: SearchFieldTranscript, query: "transcript-scope-needle"},
		{field: SearchFieldTags, query: "tag-scope-needle"},
		{field: SearchFieldEvaluations, query: "evaluation-scope-needle"},
		{field: SearchFieldFailures, query: "failure-scope-needle"},
		{field: SearchFieldArtifacts, query: "artifact-scope-needle"},
		{field: SearchFieldAgent, query: "agent-scope-needle"},
		{field: SearchFieldModel, query: "model-scope-needle"},
		{field: SearchFieldDate, query: "2026-02-03"},
		{field: SearchFieldRepo, query: "repo-scope-needle"},
		{field: SearchFieldSession, query: "session-scope-needle"},
		{field: SearchFieldTitle, query: "title-scope-needle"},
	} {
		results, err := store.SearchWithOptions(tc.query, SearchOptions{Fields: []SearchField{tc.field}})
		require.NoError(t, err)
		require.Len(t, results, 1, "query %q field %s", tc.query, tc.field)
		require.NotEmpty(t, results[0].Matches)
		assert.Equal(t, tc.field, results[0].Matches[0].Field, "query %q", tc.query)
	}
}

func TestStore_SearchOffsetsAreRuneBased(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	session := New("gpt-review", []llm.Message{{Role: llm.RoleUser, Content: "🙂 alpha café beta"}})
	require.NoError(t, store.Save(session))

	results, err := store.Search("café")
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.NotEmpty(t, results[0].Matches)
	assert.Equal(t, 8, results[0].Matches[0].Offset)
	assert.Equal(t, 12, results[0].Matches[0].End)
}

func TestStore_SearchDateFieldScope(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	session := New("gpt-review", []llm.Message{{Role: llm.RoleUser, Content: "date scope transcript"}})
	session.CreatedAt = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	session.UpdatedAt = time.Date(2026, 1, 2, 4, 5, 6, 0, time.UTC)
	require.NoError(t, store.Save(session))

	results, err := store.SearchWithOptions("2026-01-02", SearchOptions{Fields: []SearchField{SearchFieldDate}})
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.NotEmpty(t, results[0].Matches)
	assert.Equal(t, SearchFieldDate, results[0].Matches[0].Field)
	assert.Contains(t, results[0].Matches[0].Text, "2026-01-02")

	results, err = store.SearchWithOptions("2026-01-02", SearchOptions{Fields: []SearchField{SearchFieldTranscript}})
	require.NoError(t, err)
	assert.Empty(t, results)
}

func TestStore_SearchMatchesTermsAcrossAllowedFields(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	session := New("gpt-cross-field", []llm.Message{{Role: llm.RoleUser, Content: "transcript only"}})
	session.DefaultAgent = "reviewer-cross-field"
	session.Title = "Cross-field session"
	require.NoError(t, store.Save(session))

	results, err := store.Search("reviewer-cross-field gpt-cross-field")
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, session.ID, results[0].Summary.ID)
	assert.GreaterOrEqual(t, len(results[0].Matches), 2)

	results, err = store.SearchWithOptions("reviewer-cross-field gpt-cross-field", SearchOptions{
		Fields: []SearchField{SearchFieldAgent},
	})
	require.NoError(t, err)
	assert.Empty(t, results)
}

func TestStore_SearchRanksFailuresAboveNewerTranscriptEchoes(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	base := time.Now().UTC().Add(-24 * time.Hour)

	writeSessionSnapshot(t, store, Session{
		ID:        "failure",
		CreatedAt: base,
		UpdatedAt: base,
		NegativeKnowledge: []NegativeKnowledge{{
			CreatedAt: base,
			Approach:  "retry storm patch",
			Reason:    "retry storm exhausted workers",
			Agent:     "reviewer",
		}},
	})
	writeSessionSnapshot(t, store, Session{
		ID:        "transcript",
		CreatedAt: base.Add(12 * time.Hour),
		UpdatedAt: base.Add(12 * time.Hour),
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: "retry storm status update"}},
	})

	results, err := store.Search("retry storm")
	require.NoError(t, err)
	require.Len(t, results, 2)
	assert.Equal(t, "failure", results[0].Summary.ID)
	assert.Equal(t, SearchFieldFailures, results[0].Matches[0].Field)
}

func TestStore_SearchRankingUsesExactPhraseAndRecency(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	exactStore := NewStore(t.TempDir())
	writeSessionSnapshot(t, exactStore, Session{
		ID:        "exact",
		CreatedAt: now.Add(-30 * 24 * time.Hour),
		UpdatedAt: now.Add(-30 * 24 * time.Hour),
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: "ranking alpha beta phrase"}},
	})
	writeSessionSnapshot(t, exactStore, Session{
		ID:        "scattered",
		CreatedAt: now.Add(-time.Hour),
		UpdatedAt: now.Add(-time.Hour),
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: "ranking alpha filler beta phrase"}},
	})

	results, err := exactStore.Search("alpha beta")
	require.NoError(t, err)
	require.Len(t, results, 2)
	assert.Equal(t, "exact", results[0].Summary.ID)
	require.NotEmpty(t, results[0].Matches)
	assert.True(t, results[0].Matches[0].ExactPhrase)

	recencyStore := NewStore(t.TempDir())
	writeSessionSnapshot(t, recencyStore, Session{
		ID:        "older",
		CreatedAt: now.Add(-24 * time.Hour),
		UpdatedAt: now.Add(-24 * time.Hour),
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: "recency ranking needle"}},
	})
	writeSessionSnapshot(t, recencyStore, Session{
		ID:        "newer",
		CreatedAt: now.Add(-time.Hour),
		UpdatedAt: now.Add(-time.Hour),
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: "recency ranking needle"}},
	})

	results, err = recencyStore.SearchWithOptions("recency ranking needle", SearchOptions{Limit: 1})
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "newer", results[0].Summary.ID)
}

func TestStore_SearchPrivacyPolicyExcludesTranscriptAndRedactsSecrets(t *testing.T) {
	t.Parallel()

	excludedStore := NewStoreWithSearchIndexPolicy(t.TempDir(), SearchIndexPolicy{
		ExcludedFields:   []SearchField{SearchFieldTranscript},
		MaxTranscriptAge: -1,
	})
	privateSession := New("gpt-review", []llm.Message{{Role: llm.RoleUser, Content: "private transcript needle"}})
	privateSession.Title = "Policy searchable title"
	require.NoError(t, excludedStore.Save(privateSession))

	results, err := excludedStore.Search("private transcript needle")
	require.NoError(t, err)
	assert.Empty(t, results)

	results, err = excludedStore.Search("Policy searchable title")
	require.NoError(t, err)
	require.Len(t, results, 1)

	redactedStore := NewStoreWithSearchIndexPolicy(t.TempDir(), SearchIndexPolicy{MaxTranscriptAge: -1})
	secretSession := New("gpt-review", []llm.Message{{Role: llm.RoleUser, Content: "token=supersecretvalue"}})
	require.NoError(t, redactedStore.Save(secretSession))

	results, err = redactedStore.Search("supersecretvalue")
	require.NoError(t, err)
	assert.Empty(t, results)

	results, err = redactedStore.Search("token")
	require.NoError(t, err)
	require.Len(t, results, 1)
}

func TestStore_SearchIndexRedactsSensitiveSummaryAndFilterValues(t *testing.T) {
	t.Parallel()

	const secret = "ultra-private-value"

	store := NewStoreWithSearchIndexPolicy(t.TempDir(), SearchIndexPolicy{
		MaxTranscriptAge: -1,
		SensitiveFields:  []string{"tenant_secret"},
	})

	session := New("tenant_secret="+secret, []llm.Message{{
		Role:    llm.RoleUser,
		Content: "tenant_secret=" + secret,
	}})
	session.ID = "tenant_secret=" + secret
	session.Title = "path=/Users/tom/private/repo"
	session.DefaultModel = "tenant_secret=" + secret
	session.DefaultAgent = "tenant_secret=" + secret
	session.WorktreePath = "/Users/tom/private/repo"
	session.Tags = []string{"tenant_secret=" + secret}
	require.True(t, session.RecordNegativeKnowledge("tenant_secret="+secret, "tenant_secret="+secret, "", "tenant_secret="+secret))
	require.True(t, session.RecordEvaluation("tenant_secret="+secret, "pass", "tenant_secret="+secret, "eval.md", 1))
	require.True(t, session.RecordArtifact("/Users/tom/private/report.md", "note", "tenant_secret="+secret, "tenant_secret="+secret))
	require.NoError(t, store.Save(session))

	indexData, err := os.ReadFile(store.searchIndexPath())
	require.NoError(t, err)
	assert.NotContains(t, string(indexData), secret)
	assert.NotContains(t, string(indexData), "/Users/tom")

	results, err := store.Search(secret)
	require.NoError(t, err)
	assert.Empty(t, results)

	results, err = store.Search("tenant_secret")
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.NotContains(t, results[0].Summary.Title, "/Users/tom")
	assert.NotContains(t, results[0].Summary.ID, secret)
	assert.NotContains(t, results[0].Summary.Path, secret)
	assert.NotContains(t, results[0].Summary.DefaultModel, secret)
	assert.NotContains(t, results[0].Summary.DefaultAgent, secret)
	assert.NotContains(t, results[0].Summary.WorktreePath, "/Users/tom")
	assert.NotContains(t, strings.Join(results[0].Summary.Tags, ","), secret)

	for _, match := range results[0].Matches {
		assert.NotContains(t, match.Text, secret)
		assert.NotContains(t, match.Text, "/Users/tom")
	}
}

func TestStore_SearchIndexSensitiveFieldNamesRedactWholeValues(t *testing.T) {
	t.Parallel()

	const (
		rawModel    = "raw-model-field-secret"
		rawAgent    = "raw-agent-field-secret"
		rawTag      = "raw-tag-field-secret"
		rawApproach = "raw-approach-field-secret"
		rawNotes    = "raw-notes-field-secret"
		rawArtifact = "raw-artifact-field-secret"
	)

	store := NewStoreWithSearchIndexPolicy(t.TempDir(), SearchIndexPolicy{
		MaxTranscriptAge: -1,
		SensitiveFields: []string{
			"default_model",
			"default_agent",
			"tags",
			"approach",
			"notes",
			"summary",
		},
	})

	session := New(rawModel, []llm.Message{{Role: llm.RoleUser, Content: "field-name policy allowed transcript"}})
	session.DefaultAgent = rawAgent
	session.Tags = []string{rawTag}
	require.True(t, session.RecordNegativeKnowledge(rawApproach, "field-name policy failure", "", "failure-agent"))
	require.True(t, session.RecordEvaluation("reviewer", "pass", rawNotes, "eval.md", 1))
	require.True(t, session.RecordArtifact("docs/artifact.md", "note", rawArtifact, "artifact-agent"))
	require.NoError(t, store.Save(session))

	indexData, err := os.ReadFile(store.searchIndexPath())
	require.NoError(t, err)

	for _, secret := range []string{rawModel, rawAgent, rawTag, rawApproach, rawNotes, rawArtifact} {
		assert.NotContains(t, string(indexData), secret)

		secretResults, searchErr := store.Search(secret)
		require.NoError(t, searchErr)
		assert.Empty(t, secretResults, "sensitive field-name policy leaked %q", secret)
	}

	results, err := store.Search("field-name policy allowed transcript")
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "[REDACTED]", results[0].Summary.DefaultModel)
	assert.Equal(t, "[REDACTED]", results[0].Summary.DefaultAgent)
	assert.Equal(t, []string{"[REDACTED]"}, results[0].Summary.Tags)

	results, err = store.SearchWithOptions("field-name policy allowed transcript", SearchOptions{Model: rawModel})
	require.NoError(t, err)
	assert.Empty(t, results)

	results, err = store.SearchWithOptions("field-name policy allowed transcript", SearchOptions{Agent: rawAgent})
	require.NoError(t, err)
	assert.Empty(t, results)

	results, err = store.SearchWithOptions("field-name policy allowed transcript", SearchOptions{Tags: []string{rawTag}})
	require.NoError(t, err)
	assert.Empty(t, results)
}

func TestStore_SearchIndexPolicyExcludesMetadataScopes(t *testing.T) {
	t.Parallel()

	store := NewStoreWithSearchIndexPolicy(t.TempDir(), SearchIndexPolicy{
		ExcludedFields:   []SearchField{SearchFieldAgent, SearchFieldTags, SearchFieldDate},
		MaxTranscriptAge: -1,
	})

	session := New("gpt-review", []llm.Message{{Role: llm.RoleUser, Content: "metadata policy needle"}})
	session.CreatedAt = time.Date(1999, 2, 3, 4, 5, 6, 0, time.UTC)
	session.UpdatedAt = time.Date(1999, 2, 3, 5, 6, 7, 0, time.UTC)
	session.DefaultAgent = "reviewer"
	session.Tags = []string{"auth"}
	require.True(t, session.RecordNegativeKnowledge("failed approach", "bad reason", "", "reviewer"))
	require.True(t, session.RecordEvaluation("reviewer", "pass", "solid notes", "eval.md", 5))
	require.True(t, session.RecordArtifact("docs/notes.md", "research", "artifact summary", "reviewer"))
	require.NoError(t, store.Save(session))

	results, err := store.SearchWithOptions("metadata policy needle", SearchOptions{Agent: "reviewer"})
	require.NoError(t, err)
	assert.Empty(t, results)

	results, err = store.SearchWithOptions("metadata policy needle", SearchOptions{Tags: []string{"auth"}})
	require.NoError(t, err)
	assert.Empty(t, results)

	results, err = store.Search("reviewer")
	require.NoError(t, err)
	assert.Empty(t, results)

	results, err = store.SearchWithOptions("metadata policy needle", SearchOptions{
		DateFrom: time.Date(1999, 2, 3, 0, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)
	assert.Empty(t, results)

	results, err = store.Search("metadata policy needle")
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Empty(t, results[0].Summary.DefaultAgent)
	assert.Empty(t, results[0].Summary.Tags)
	assert.True(t, results[0].Summary.CreatedAt.IsZero())
	assert.True(t, results[0].Summary.UpdatedAt.IsZero())

	indexData, err := os.ReadFile(store.searchIndexPath())
	require.NoError(t, err)
	assert.NotContains(t, string(indexData), "reviewer")
	assert.NotContains(t, string(indexData), "1999-02-03")
}

func TestStore_SearchIndexPolicyExcludesRecordFamilyNestedAgents(t *testing.T) {
	t.Parallel()

	store := NewStoreWithSearchIndexPolicy(t.TempDir(), SearchIndexPolicy{
		ExcludedFields: []SearchField{
			SearchFieldFailures,
			SearchFieldEvaluations,
			SearchFieldArtifacts,
		},
		MaxTranscriptAge: -1,
	})

	session := New("gpt-review", []llm.Message{{Role: llm.RoleUser, Content: "record policy allowed transcript"}})
	require.True(t, session.RecordNegativeKnowledge("failure family needle", "failure reason needle", "", "failure-agent-needle"))
	require.True(t, session.RecordEvaluation("evaluation-agent-needle", "evaluation outcome needle", "evaluation notes needle", "", 1))
	require.True(t, session.RecordArtifact("docs/artifact-family-needle.md", "artifact kind needle", "artifact summary needle", "artifact-agent-needle"))
	require.NoError(t, store.Save(session))

	for _, query := range []string{
		"failure family needle",
		"failure-agent-needle",
		"evaluation outcome needle",
		"evaluation-agent-needle",
		"artifact summary needle",
		"artifact-agent-needle",
	} {
		results, err := store.Search(query)
		require.NoError(t, err)
		assert.Empty(t, results, "record-family exclusion leaked query %q", query)
	}

	results, err := store.SearchWithOptions("record policy allowed transcript", SearchOptions{Agent: "failure-agent-needle"})
	require.NoError(t, err)
	assert.Empty(t, results)

	results, err = store.Search("record policy allowed transcript")
	require.NoError(t, err)
	require.Len(t, results, 1)

	indexData, err := os.ReadFile(store.searchIndexPath())
	require.NoError(t, err)
	assert.NotContains(t, string(indexData), "failure-agent-needle")
	assert.NotContains(t, string(indexData), "evaluation-agent-needle")
	assert.NotContains(t, string(indexData), "artifact-agent-needle")
	assert.NotContains(t, string(indexData), "artifact-family-needle")
}

func TestStore_SearchIndexPolicyExcludesDateRetentionMetadata(t *testing.T) {
	t.Parallel()

	store := NewStoreWithSearchIndexPolicy(t.TempDir(), SearchIndexPolicy{
		ExcludedFields:   []SearchField{SearchFieldDate},
		MaxTranscriptAge: 10 * 365 * 24 * time.Hour,
	})

	session := New("gpt-review", []llm.Message{{Role: llm.RoleUser, Content: "date policy retention needle"}})
	session.CreatedAt = time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	session.UpdatedAt = time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	require.NoError(t, store.Save(session))

	indexData, err := os.ReadFile(store.searchIndexPath())
	require.NoError(t, err)
	assert.NotContains(t, string(indexData), `"built_at"`)
	assert.NotContains(t, string(indexData), `"transcript_expires_at"`)
	assert.NotContains(t, string(indexData), `"next_transcript_expiry"`)
	assert.NotContains(t, string(indexData), `"mod_time_unix_nano"`)
	assert.NotContains(t, string(indexData), "2034-")

	var index sessionSearchIndex
	require.NoError(t, json.Unmarshal(indexData, &index))
	require.NotEmpty(t, index.Files)
	assert.Zero(t, index.Files[0].ModTimeUnixNano)

	results, err := store.Search("date policy retention needle")
	require.NoError(t, err)
	assert.Empty(t, results)

	unboundedStore := NewStoreWithSearchIndexPolicy(t.TempDir(), SearchIndexPolicy{
		ExcludedFields:   []SearchField{SearchFieldDate},
		MaxTranscriptAge: -1,
	})
	require.NoError(t, unboundedStore.Save(session))

	indexData, err = os.ReadFile(unboundedStore.searchIndexPath())
	require.NoError(t, err)
	assert.NotContains(t, string(indexData), `"built_at"`)
	assert.NotContains(t, string(indexData), `"transcript_expires_at"`)
	assert.NotContains(t, string(indexData), `"next_transcript_expiry"`)

	results, err = unboundedStore.Search("date policy retention needle")
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.True(t, results[0].Summary.CreatedAt.IsZero())
	assert.True(t, results[0].Summary.UpdatedAt.IsZero())
}

func TestStore_SearchIndexPolicyExcludesSearchableMetadataFields(t *testing.T) {
	t.Parallel()

	store := NewStoreWithSearchIndexPolicy(t.TempDir(), SearchIndexPolicy{
		ExcludedFields:   []SearchField{SearchFieldTitle, SearchFieldModel, SearchFieldRepo},
		MaxTranscriptAge: -1,
	})

	session := New("sensitive-model-needle", []llm.Message{{Role: llm.RoleUser, Content: "allowed transcript needle"}})
	session.Title = "sensitive title needle"
	session.WorktreePath = "/Users/tom/repo-secret-needle"
	require.NoError(t, store.Save(session))

	results, err := store.Search("sensitive title needle")
	require.NoError(t, err)
	assert.Empty(t, results)

	results, err = store.Search("sensitive-model-needle")
	require.NoError(t, err)
	assert.Empty(t, results)

	results, err = store.SearchWithOptions("allowed transcript needle", SearchOptions{Repo: "repo-secret-needle"})
	require.NoError(t, err)
	assert.Empty(t, results)

	results, err = store.Search("allowed transcript needle")
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Empty(t, results[0].Summary.Title)
	assert.Empty(t, results[0].Summary.DefaultModel)
	assert.Empty(t, results[0].Summary.WorktreePath)

	indexData, err := os.ReadFile(store.searchIndexPath())
	require.NoError(t, err)
	assert.NotContains(t, string(indexData), "sensitive title needle")
	assert.NotContains(t, string(indexData), "sensitive-model-needle")
	assert.NotContains(t, string(indexData), "repo-secret-needle")
	assert.NotContains(t, string(indexData), "/Users/tom")
}

func TestStore_SearchIndexPolicyExcludesSessionIdentity(t *testing.T) {
	t.Parallel()

	const sessionID = "private-session-id-needle"

	store := NewStoreWithSearchIndexPolicy(t.TempDir(), SearchIndexPolicy{
		ExcludedFields:   []SearchField{SearchFieldSession},
		MaxTranscriptAge: -1,
	})
	session := New("gpt-review", []llm.Message{{Role: llm.RoleUser, Content: "session identity allowed transcript"}})
	session.ID = sessionID
	require.NoError(t, store.Save(session))

	results, err := store.Search(sessionID)
	require.NoError(t, err)
	assert.Empty(t, results)

	results, err = store.SearchWithOptions("session identity allowed transcript", SearchOptions{SessionIDs: []string{sessionID}})
	require.NoError(t, err)
	assert.Empty(t, results)

	results, err = store.Search("session identity allowed transcript")
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Empty(t, results[0].Summary.ID)
	assert.Empty(t, results[0].Summary.Path)

	indexData, err := os.ReadFile(store.searchIndexPath())
	require.NoError(t, err)
	assert.NotContains(t, string(indexData), sessionID)
	assert.NotContains(t, string(indexData), sessionID+sessionFileExt)
}

func TestStore_SearchRebuildsWhenIndexPolicyChanges(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	defaultStore := NewStoreWithSearchIndexPolicy(dir, SearchIndexPolicy{MaxTranscriptAge: -1})
	session := New("gpt-review", []llm.Message{{Role: llm.RoleUser, Content: "policy switch private needle"}})
	require.NoError(t, defaultStore.Save(session))

	results, err := defaultStore.Search("policy switch private needle")
	require.NoError(t, err)
	require.Len(t, results, 1)

	privateStore := NewStoreWithSearchIndexPolicy(dir, SearchIndexPolicy{
		ExcludedFields:   []SearchField{SearchFieldTranscript},
		MaxTranscriptAge: -1,
	})
	results, err = privateStore.Search("policy switch private needle")
	require.NoError(t, err)
	assert.Empty(t, results)

	indexData, err := os.ReadFile(privateStore.searchIndexPath())
	require.NoError(t, err)
	assert.NotContains(t, string(indexData), "policy switch private needle")
}

func TestStore_SearchIndexUsesPersistedSnapshotWhenUpdating(t *testing.T) {
	t.Parallel()

	store := NewStoreWithSearchIndexPolicy(t.TempDir(), SearchIndexPolicy{MaxTranscriptAge: -1})
	now := time.Now().UTC()
	initial := Session{
		ID:        "same-session",
		CreatedAt: now,
		UpdatedAt: now,
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: "initial indexed needle"}},
	}
	writeSessionSnapshot(t, store, initial)

	results, err := store.Search("initial indexed needle")
	require.NoError(t, err)
	require.Len(t, results, 1)

	persisted := initial
	persisted.Messages = []llm.Message{{Role: llm.RoleUser, Content: "persisted latest needle"}}
	writeSessionSnapshot(t, store, persisted)

	require.NoError(t, store.indexSavedSession(store.Path(initial.ID)))

	results, err = store.Search("persisted latest needle")
	require.NoError(t, err)
	require.Len(t, results, 1)

	results, err = store.Search("initial indexed needle")
	require.NoError(t, err)
	assert.Empty(t, results)
}

func TestStore_SearchIndexUpdateRemovesPreviousDocumentForSameFile(t *testing.T) {
	t.Parallel()

	store := NewStoreWithSearchIndexPolicy(t.TempDir(), SearchIndexPolicy{MaxTranscriptAge: -1})
	now := time.Now().UTC()
	path := filepath.Join(store.Dir(), "shared.json")

	writeSessionSnapshotFile(t, path, Session{
		ID:        "old-id",
		CreatedAt: now,
		UpdatedAt: now,
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: "old same file needle"}},
	})

	results, err := store.Search("old same file needle")
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "old-id", results[0].Summary.ID)

	writeSessionSnapshotFile(t, path, Session{
		ID:        "new-id",
		CreatedAt: now,
		UpdatedAt: now.Add(time.Minute),
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: "new same file needle"}},
	})

	require.NoError(t, store.indexSavedSession(path))

	results, err = store.Search("new same file needle")
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "new-id", results[0].Summary.ID)

	results, err = store.Search("old same file needle")
	require.NoError(t, err)
	assert.Empty(t, results)
}

func TestStore_SearchIndexSessionIdentityExcludedUpdateUsesFileKey(t *testing.T) {
	t.Parallel()

	store := NewStoreWithSearchIndexPolicy(t.TempDir(), SearchIndexPolicy{
		ExcludedFields:   []SearchField{SearchFieldSession},
		MaxTranscriptAge: -1,
	})
	now := time.Now().UTC()
	path := filepath.Join(store.Dir(), "shared-private.json")

	writeSessionSnapshotFile(t, path, Session{
		ID:        "old-private-id",
		CreatedAt: now,
		UpdatedAt: now,
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: "old private same file needle"}},
	})

	results, err := store.Search("old private same file needle")
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Empty(t, results[0].Summary.ID)

	writeSessionSnapshotFile(t, path, Session{
		ID:        "new-private-id",
		CreatedAt: now,
		UpdatedAt: now.Add(time.Minute),
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: "new private same file needle"}},
	})

	require.NoError(t, store.indexSavedSession(path))

	results, err = store.Search("new private same file needle")
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Empty(t, results[0].Summary.ID)

	results, err = store.Search("old private same file needle")
	require.NoError(t, err)
	assert.Empty(t, results)

	indexData, err := os.ReadFile(store.searchIndexPath())
	require.NoError(t, err)
	assert.NotContains(t, string(indexData), "old-private-id")
	assert.NotContains(t, string(indexData), "new-private-id")
	assert.NotContains(t, string(indexData), "shared-private.json")
}

func TestStore_SearchDefaultPolicyExpiresOldTranscriptContent(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	old := time.Now().UTC().Add(-defaultTranscriptRetention - 24*time.Hour)
	writeSessionSnapshot(t, store, Session{
		ID:        "old",
		CreatedAt: old,
		UpdatedAt: old,
		Title:     "old title needle",
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: "expired transcript needle"}},
	})

	results, err := store.Search("expired transcript needle")
	require.NoError(t, err)
	assert.Empty(t, results)

	results, err = store.Search("old title needle")
	require.NoError(t, err)
	require.Len(t, results, 1)
}

func TestStore_SearchRebuildsWhenTranscriptRetentionExpires(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	now := time.Now().UTC()
	expired := now.Add(-defaultTranscriptRetention - time.Hour)
	path := store.Path("retention")

	writeSessionSnapshot(t, store, Session{
		ID:        "retention",
		CreatedAt: expired,
		UpdatedAt: expired,
		Title:     "retention title needle",
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: "retention rollover needle"}},
	})

	indexedSession := Session{
		ID:        "retention",
		CreatedAt: now.Add(-defaultTranscriptRetention + time.Hour),
		UpdatedAt: now.Add(-defaultTranscriptRetention + time.Hour),
		Title:     "retention title needle",
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: "retention rollover needle"}},
	}
	index := newSearchIndex(normalizeSearchIndexPolicy(SearchIndexPolicy{}))
	files, err := currentSessionFiles(store.Dir())
	require.NoError(t, err)

	index.Files = files
	index.addSession(path, indexedSession, normalizeSearchIndexPolicy(SearchIndexPolicy{}))
	index.finish()
	require.False(t, index.NextTranscriptExpiry.IsZero())
	index.NextTranscriptExpiry = now.Add(-time.Minute)
	require.NoError(t, store.writeSearchIndex(index))

	results, err := store.Search("retention rollover needle")
	require.NoError(t, err)
	assert.Empty(t, results)

	results, err = store.Search("retention title needle")
	require.NoError(t, err)
	require.Len(t, results, 1)
}

func TestStore_SearchDefaultPolicyExcludesUndatedTranscriptContent(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	writeSessionSnapshot(t, store, Session{
		ID:       "undated",
		Title:    "undated title needle",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "undated transcript needle"}},
	})

	results, err := store.Search("undated transcript needle")
	require.NoError(t, err)
	assert.Empty(t, results)

	results, err = store.Search("undated title needle")
	require.NoError(t, err)
	require.Len(t, results, 1)

	unboundedStore := NewStoreWithSearchIndexPolicy(t.TempDir(), SearchIndexPolicy{MaxTranscriptAge: -1})
	writeSessionSnapshot(t, unboundedStore, Session{
		ID:       "undated",
		Title:    "undated title needle",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "undated transcript needle"}},
	})

	results, err = unboundedStore.Search("undated transcript needle")
	require.NoError(t, err)
	require.Len(t, results, 1)
}

func TestStore_SearchRebuildsStaleAndCorruptIndexes(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	session := Session{
		ID:        "rebuild",
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: "migration recovery needle"}},
	}
	writeSessionSnapshot(t, store, session)

	stale := sessionSearchIndex{Version: searchIndexVersion - 1, SchemaVersion: searchIndexSchemaVersion}
	data, err := json.Marshal(stale)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(store.searchIndexPath(), data, 0o600))

	results, err := store.Search("migration recovery")
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "rebuild", results[0].Summary.ID)

	var rebuilt sessionSearchIndex

	data, err = os.ReadFile(store.searchIndexPath())
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &rebuilt))
	assert.Equal(t, searchIndexVersion, rebuilt.Version)
	assert.Equal(t, searchIndexSchemaVersion, rebuilt.SchemaVersion)
	assert.Equal(t, sessionSchemaVersion, rebuilt.SessionSchema)

	rebuilt.SchemaVersion = searchIndexSchemaVersion - 1
	data, err = json.Marshal(rebuilt)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(store.searchIndexPath(), data, 0o600))

	results, err = store.Search("migration recovery")
	require.NoError(t, err)
	require.Len(t, results, 1)

	require.NoError(t, os.WriteFile(store.searchIndexPath(), []byte(`not-json`), 0o600))

	results, err = store.Search("migration recovery")
	require.NoError(t, err)
	require.Len(t, results, 1)

	data, err = os.ReadFile(store.searchIndexPath())
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &rebuilt))

	rebuilt.Terms = map[string][]string{}
	data, err = json.Marshal(rebuilt)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(store.searchIndexPath(), data, 0o600))

	results, err = store.Search("migration recovery")
	require.NoError(t, err)
	require.Len(t, results, 1)

	data, err = os.ReadFile(store.searchIndexPath())
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &rebuilt))
	require.NotEmpty(t, rebuilt.Sessions)

	rebuilt.Terms = map[string][]string{"wrong": {rebuilt.Sessions[0].Key}}
	rebuilt.Integrity = searchIndexIntegrityDigest(rebuilt)
	data, err = json.Marshal(rebuilt)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(store.searchIndexPath(), data, 0o600))

	results, err = store.Search("migration recovery")
	require.NoError(t, err)
	require.Len(t, results, 1)
}

func TestStore_SaveRebuildsCorruptSearchIndex(t *testing.T) {
	t.Parallel()

	store := NewStoreWithSearchIndexPolicy(t.TempDir(), SearchIndexPolicy{MaxTranscriptAge: -1})
	initial := New("gpt-review", []llm.Message{{Role: llm.RoleUser, Content: "initial save recovery needle"}})
	initial.ID = "initial-save-recovery"
	require.NoError(t, store.Save(initial))

	require.NoError(t, os.WriteFile(store.searchIndexPath(), []byte(`not-json`), 0o600))

	savedAfterCorruption := New("gpt-review", []llm.Message{{Role: llm.RoleUser, Content: "saved after corrupt index needle"}})
	savedAfterCorruption.ID = "saved-after-corruption"
	require.NoError(t, store.Save(savedAfterCorruption))

	results, err := store.Search("initial save recovery needle")
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, initial.ID, results[0].Summary.ID)

	results, err = store.Search("saved after corrupt index needle")
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, savedAfterCorruption.ID, results[0].Summary.ID)
}

func TestStore_SearchRebuildSkipsCorruptSessionFiles(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	valid := Session{
		ID:        "valid",
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: "valid rebuild needle"}},
	}
	writeSessionSnapshot(t, store, valid)

	require.NoError(t, os.WriteFile(filepath.Join(store.Dir(), "broken.json"), []byte(`{broken`), 0o600))

	results, err := store.Search("valid rebuild needle")
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "valid", results[0].Summary.ID)

	var index sessionSearchIndex

	data, err := os.ReadFile(store.searchIndexPath())
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &index))
	require.Len(t, index.Files, 2)
	require.Len(t, index.Sessions, 2)

	foundBrokenPlaceholder := false

	for i := range index.Sessions {
		document := index.Sessions[i]
		if document.Key == searchIndexFileKey("broken.json") {
			foundBrokenPlaceholder = true

			assert.Empty(t, document.Fields)
			assert.Empty(t, document.Summary.ID)

			break
		}
	}

	assert.True(t, foundBrokenPlaceholder)

	repaired := Session{
		ID:        "broken",
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: "repaired rebuild needle"}},
	}
	writeSessionSnapshotFile(t, filepath.Join(store.Dir(), "broken.json"), repaired)

	results, err = store.Search("repaired rebuild needle")
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "broken", results[0].Summary.ID)
}

func TestStore_SearchRebuildsWhenSessionSchemaVersionChanges(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	session := Session{
		ID:        "schema",
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: "session schema needle"}},
	}
	writeSessionSnapshot(t, store, session)

	stale := newSearchIndex(normalizeSearchIndexPolicy(SearchIndexPolicy{}))
	stale.Version = searchIndexVersion
	stale.SchemaVersion = searchIndexSchemaVersion
	stale.SessionSchema = sessionSchemaVersion - 1
	files, err := currentSessionFiles(store.Dir())
	require.NoError(t, err)

	stale.Files = files
	stale.finish()

	data, err := json.Marshal(stale)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(store.searchIndexPath(), data, 0o600))

	results, err := store.Search("session schema needle")
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "schema", results[0].Summary.ID)
}

func searchResultIDs(results []SearchResult) []string {
	ids := make([]string, 0, len(results))
	for i := range results {
		ids = append(ids, results[i].Summary.ID)
	}

	return ids
}

func writeSessionSnapshotFile(t *testing.T, path string, session Session) {
	t.Helper()
	require.NotEmpty(t, session.ID)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))

	data, err := json.MarshalIndent(session, "", "  ")
	require.NoError(t, err)

	data = append(data, '\n')
	require.NoError(t, os.WriteFile(path, data, 0o600))
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
