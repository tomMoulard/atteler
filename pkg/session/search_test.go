package session

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/llm"
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
