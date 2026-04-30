// Package session persists atteler chat sessions for replay and continuation.
package session

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tommoulard/atteler/pkg/llm"
)

// EnvDir overrides the default session storage directory.
const EnvDir = "ATTELER_SESSION_DIR"

// Session is a durable chat transcript.
type Session struct {
	CreatedAt      time.Time     `json:"created_at"`
	UpdatedAt      time.Time     `json:"updated_at"`
	ID             string        `json:"id"`
	Title          string        `json:"title,omitempty"`
	DefaultModel   string        `json:"default_model,omitempty"`
	DefaultAgent   string        `json:"default_agent,omitempty"`
	WorktreePath   string        `json:"worktree_path,omitempty"`
	WorktreeBranch string        `json:"worktree_branch,omitempty"`
	WorktreeBase   string        `json:"worktree_base,omitempty"`
	Tags           []string      `json:"tags,omitempty"`
	Messages       []llm.Message `json:"messages"`
}

// Store reads and writes sessions under a directory.
type Store struct {
	dir string
}

// Summary is lightweight session metadata for listing.
type Summary struct {
	UpdatedAt    time.Time
	CreatedAt    time.Time
	Path         string
	ID           string
	Title        string
	DefaultModel string
	DefaultAgent string
	Tags         []string
	Messages     int
}

// TagSummary counts how many saved sessions use a tag.
type TagSummary struct {
	Tag      string
	Sessions int
}

// NewStore creates a session store. If dir is empty, DefaultDir is used.
func NewStore(dir string) *Store {
	if dir == "" {
		dir = DefaultDir()
	}
	return &Store{dir: dir}
}

// DefaultDir returns the default session storage directory.
func DefaultDir() string {
	if dir := os.Getenv(EnvDir); dir != "" {
		return dir
	}
	if cwd, err := os.Getwd(); err == nil {
		return filepath.Join(cwd, ".atteler", "sessions")
	}
	return filepath.Join(os.TempDir(), "atteler", "sessions")
}

// Dir returns the store directory.
func (s *Store) Dir() string {
	return s.dir
}

// New creates a new unsaved session.
func New(defaultModel string, messages []llm.Message) Session {
	now := time.Now().UTC()
	copied := append([]llm.Message(nil), messages...)
	return Session{
		ID:           newID(now),
		CreatedAt:    now,
		UpdatedAt:    now,
		DefaultModel: defaultModel,
		Messages:     copied,
	}
}

// Load reads a session by ID or path.
func (s *Store) Load(ref string) (Session, error) {
	path := s.path(ref)
	data, err := os.ReadFile(path)
	if err != nil {
		return Session{}, fmt.Errorf("session: read %s: %w", path, err)
	}

	var session Session
	if err := json.Unmarshal(data, &session); err != nil {
		return Session{}, fmt.Errorf("session: parse %s: %w", path, err)
	}
	if session.ID == "" {
		session.ID = idFromPath(path)
	}
	return session, nil
}

// Save writes a session atomically enough for local CLI use.
func (s *Store) Save(session Session) error {
	if session.ID == "" {
		return errors.New("session: id is required")
	}

	now := time.Now().UTC()
	if session.CreatedAt.IsZero() {
		session.CreatedAt = now
	}
	session.UpdatedAt = now

	if err := os.MkdirAll(s.dir, 0o750); err != nil {
		return fmt.Errorf("session: create dir: %w", err)
	}

	data, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		return fmt.Errorf("session: marshal: %w", err)
	}
	data = append(data, '\n')

	path := s.path(session.ID)
	tmp, err := os.CreateTemp(s.dir, ".session-*.json")
	if err != nil {
		return fmt.Errorf("session: create temp: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("session: write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("session: close temp: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("session: replace %s: %w", path, err)
	}

	return nil
}

// List returns saved sessions sorted by most recently updated first.
func (s *Store) List() ([]Summary, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("session: list %s: %w", s.dir, err)
	}

	summaries := make([]Summary, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}

		path := filepath.Join(s.dir, entry.Name())
		session, err := s.Load(path)
		if err != nil {
			return nil, err
		}
		summaries = append(summaries, summarize(path, session))
	}

	sort.Slice(summaries, func(i, j int) bool {
		return summaries[i].UpdatedAt.After(summaries[j].UpdatedAt)
	})
	return summaries, nil
}

// Tags returns saved session tags sorted by descending use count, then name.
func (s *Store) Tags() ([]TagSummary, error) {
	summaries, err := s.List()
	if err != nil {
		return nil, err
	}

	counts := make(map[string]int)
	display := make(map[string]string)
	for i := range summaries {
		summary := &summaries[i]
		seen := make(map[string]bool, len(summary.Tags))
		for _, tag := range summary.Tags {
			key := strings.ToLower(strings.TrimSpace(tag))
			if key == "" || seen[key] {
				continue
			}
			seen[key] = true
			counts[key]++
			if _, ok := display[key]; !ok {
				display[key] = strings.TrimSpace(tag)
			}
		}
	}

	tags := make([]TagSummary, 0, len(counts))
	for key, count := range counts {
		tags = append(tags, TagSummary{Tag: display[key], Sessions: count})
	}
	sort.Slice(tags, func(i, j int) bool {
		if tags[i].Sessions == tags[j].Sessions {
			return strings.ToLower(tags[i].Tag) < strings.ToLower(tags[j].Tag)
		}
		return tags[i].Sessions > tags[j].Sessions
	})
	return tags, nil
}

// Path returns the path for a session reference.
func (s *Store) Path(ref string) string {
	return s.path(ref)
}

// Append adds a message to the session.
func (s *Session) Append(role llm.Role, content string) {
	s.Messages = append(s.Messages, llm.Message{Role: role, Content: content})
}

func (s *Store) path(ref string) string {
	if ref == "" {
		return ""
	}

	if filepath.IsAbs(ref) || strings.ContainsRune(ref, rune(os.PathSeparator)) {
		return ref
	}
	if strings.HasSuffix(ref, ".json") {
		return filepath.Join(s.dir, ref)
	}
	return filepath.Join(s.dir, ref+".json")
}

func newID(now time.Time) string {
	var suffix [4]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return now.Format("20060102-150405")
	}
	return now.Format("20060102-150405") + "-" + hex.EncodeToString(suffix[:])
}

func idFromPath(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

func summarize(path string, session Session) Summary {
	return Summary{
		ID:           session.ID,
		Title:        session.Title,
		Path:         path,
		CreatedAt:    session.CreatedAt,
		UpdatedAt:    session.UpdatedAt,
		DefaultModel: session.DefaultModel,
		DefaultAgent: session.DefaultAgent,
		Tags:         append([]string(nil), session.Tags...),
		Messages:     len(session.Messages),
	}
}
