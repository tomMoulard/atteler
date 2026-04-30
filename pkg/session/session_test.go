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

func TestDefaultDir_EnvOverride(t *testing.T) {
	t.Setenv(EnvDir, "/tmp/custom-atteler-sessions")

	if got := DefaultDir(); got != "/tmp/custom-atteler-sessions" {
		assert.Failf(t, "assertion failed", "DefaultDir = %q", got)
	}
}
