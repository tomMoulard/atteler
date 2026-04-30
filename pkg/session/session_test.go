package session

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tommoulard/atteler/pkg/llm"
)

func TestStore_SaveLoadByID(t *testing.T) {
	store := NewStore(t.TempDir())
	session := New("gpt-4.1", nil)
	session.Append(llm.RoleUser, "hello")
	session.Append(llm.RoleAssistant, "hi")

	if err := store.Save(session); err != nil {
		t.Fatal(err)
	}

	loaded, err := store.Load(session.ID)
	if err != nil {
		t.Fatal(err)
	}

	if loaded.ID != session.ID {
		t.Errorf("ID = %q, want %q", loaded.ID, session.ID)
	}
	if loaded.DefaultModel != "gpt-4.1" {
		t.Errorf("DefaultModel = %q", loaded.DefaultModel)
	}
	if len(loaded.Messages) != 2 {
		t.Fatalf("messages len = %d, want 2", len(loaded.Messages))
	}
	if loaded.Messages[0].Role != llm.RoleUser || loaded.Messages[0].Content != "hello" {
		t.Errorf("first message = %+v", loaded.Messages[0])
	}
}

func TestStore_LoadByPathInfersID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manual.json")
	if err := os.WriteFile(path, []byte(`{"messages":[{"role":"user","content":"hi"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	loaded, err := NewStore(dir).Load(path)
	if err != nil {
		t.Fatal(err)
	}

	if loaded.ID != "manual" {
		t.Errorf("ID = %q, want manual", loaded.ID)
	}
}

func TestStore_Path(t *testing.T) {
	store := NewStore("/tmp/atteler-sessions")

	if got := store.Path("abc"); got != "/tmp/atteler-sessions/abc.json" {
		t.Errorf("Path(id) = %q", got)
	}
	if got := store.Path("abc.json"); got != "/tmp/atteler-sessions/abc.json" {
		t.Errorf("Path(json) = %q", got)
	}
}

func TestStore_List(t *testing.T) {
	const defaultAgent = "writer"

	store := NewStore(t.TempDir())

	first := New("gpt-4.1", []llm.Message{{Role: llm.RoleUser, Content: "one"}})
	first.DefaultAgent = defaultAgent
	first.Title = "Review auth flow"
	first.Tags = []string{"auth", "review"}
	if err := store.Save(first); err != nil {
		t.Fatal(err)
	}

	second := New("gpt-4.1-mini", []llm.Message{{Role: llm.RoleUser, Content: "two"}})
	if err := store.Save(second); err != nil {
		t.Fatal(err)
	}

	summaries, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 2 {
		t.Fatalf("summaries len = %d, want 2", len(summaries))
	}
	if summaries[0].Messages != 1 {
		t.Errorf("Messages = %d", summaries[0].Messages)
	}
	if summaries[1].DefaultAgent != defaultAgent && summaries[0].DefaultAgent != defaultAgent {
		t.Fatalf("%s agent not found in summaries: %+v", defaultAgent, summaries)
	}
	if summaries[1].Title != first.Title && summaries[0].Title != first.Title {
		t.Fatalf("title not found in summaries: %+v", summaries)
	}
	if len(summaries[1].Tags) == 0 && len(summaries[0].Tags) == 0 {
		t.Fatalf("tags not found in summaries: %+v", summaries)
	}
}

func TestStore_Tags(t *testing.T) {
	store := NewStore(t.TempDir())

	first := New("gpt-4.1", nil)
	first.Tags = []string{"auth", "review", "Auth"}
	if err := store.Save(first); err != nil {
		t.Fatal(err)
	}

	second := New("gpt-4.1", nil)
	second.Tags = []string{"auth", "docs"}
	if err := store.Save(second); err != nil {
		t.Fatal(err)
	}

	tags, err := store.Tags()
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 3 {
		t.Fatalf("tags len = %d, want 3: %+v", len(tags), tags)
	}
	if tags[0].Tag != "auth" || tags[0].Sessions != 2 {
		t.Fatalf("first tag = %+v, want auth count 2", tags[0])
	}
}

func TestDefaultDir_EnvOverride(t *testing.T) {
	t.Setenv(EnvDir, "/tmp/custom-atteler-sessions")

	if got := DefaultDir(); got != "/tmp/custom-atteler-sessions" {
		t.Errorf("DefaultDir = %q", got)
	}
}
