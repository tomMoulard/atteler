//nolint:wsl_v5 // Session provenance helpers keep optional metadata mapping compact.
package memory

import (
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/session"
)

const sessionSourceType = "session"

var errMissingSessionID = errors.New("memory session id is required")

// SessionIndexOptions controls privacy scope metadata for session-derived memory.
type SessionIndexOptions struct {
	Scope     string
	Agent     string
	Retention string
}

// FromSessions builds a searchable memory store from saved sessions.
func FromSessions(sessions ...session.Session) (*Store, error) {
	store := NewStore()
	if err := store.AddSessions(sessions...); err != nil {
		return nil, err
	}

	return store, nil
}

// AddSessions indexes each saved session into the store.
func (s *Store) AddSessions(sessions ...session.Session) error {
	for i := range sessions {
		if err := s.AddSession(sessions[i]); err != nil {
			return err
		}
	}

	return nil
}

// AddSession indexes searchable metadata, transcript messages, negative
// knowledge, agent evaluations, and artifacts for a saved session.
func (s *Store) AddSession(saved session.Session) error {
	return s.AddSessionWithOptions(saved, SessionIndexOptions{Scope: ScopeSession})
}

// AddSessionWithOptions indexes searchable session data with explicit scope policy metadata.
func (s *Store) AddSessionWithOptions(saved session.Session, opts SessionIndexOptions) error {
	sessionID := strings.TrimSpace(saved.ID)
	if sessionID == "" {
		return errMissingSessionID
	}

	opts.Scope = normalizeSessionScope(opts.Scope)

	if text := sessionMetadataText(saved); text != "" {
		if shouldIndexSessionDocument(saved, opts, saved.DefaultAgent) {
			if err := s.Add(sessionDocument(saved, "metadata", -1, text, nil, opts)); err != nil {
				return err
			}
		}
	}

	if err := s.addSessionMessages(saved, opts); err != nil {
		return err
	}

	if err := s.addSessionNegativeKnowledge(saved, opts); err != nil {
		return err
	}

	if err := s.addSessionEvaluations(saved, opts); err != nil {
		return err
	}

	if err := s.addSessionArtifacts(saved, opts); err != nil {
		return err
	}

	s.updateSessionCorpus(saved, opts)

	return nil
}

func (s *Store) addSessionMessages(saved session.Session, opts SessionIndexOptions) error {
	if !shouldIndexSessionDocument(saved, opts, saved.DefaultAgent) {
		return nil
	}

	for i, message := range saved.Messages {
		text := messageText(message)
		if text == "" {
			continue
		}

		if err := s.Add(sessionDocument(saved, "message", i, text, map[string]string{
			"role": strings.TrimSpace(string(message.Role)),
		}, opts)); err != nil {
			return err
		}
	}

	return nil
}

func (s *Store) addSessionNegativeKnowledge(saved session.Session, opts SessionIndexOptions) error {
	for i, entry := range saved.NegativeKnowledge {
		if !shouldIndexSessionDocument(saved, opts, entry.Agent) {
			continue
		}

		text := negativeKnowledgeText(entry)
		if text == "" {
			continue
		}

		if err := s.Add(sessionDocument(saved, "negative_knowledge", i, text, map[string]string{
			"agent":  strings.TrimSpace(entry.Agent),
			"commit": strings.TrimSpace(entry.Commit),
		}, opts)); err != nil {
			return err
		}
	}

	return nil
}

func (s *Store) addSessionEvaluations(saved session.Session, opts SessionIndexOptions) error {
	for i := range saved.Evaluations {
		entry := &saved.Evaluations[i]
		if !shouldIndexSessionDocument(saved, opts, entry.Agent) {
			continue
		}

		text := evaluationText(*entry)
		if text == "" {
			continue
		}

		if err := s.Add(sessionDocument(saved, "evaluation", i, text, map[string]string{
			"agent":     strings.TrimSpace(entry.Agent),
			"outcome":   strings.TrimSpace(entry.Outcome),
			"reference": strings.TrimSpace(entry.Reference),
		}, opts)); err != nil {
			return err
		}
	}

	return nil
}

func (s *Store) addSessionArtifacts(saved session.Session, opts SessionIndexOptions) error {
	for i := range saved.Artifacts {
		entry := &saved.Artifacts[i]
		if !shouldIndexSessionDocument(saved, opts, entry.SourceAgent) {
			continue
		}

		text := artifactText(*entry)
		if text == "" {
			continue
		}

		if err := s.Add(sessionDocument(saved, "artifact", i, text, map[string]string{
			"artifact_path": strings.TrimSpace(entry.Path),
			"artifact_kind": strings.TrimSpace(entry.Kind),
			"source_agent":  strings.TrimSpace(entry.SourceAgent),
		}, opts)); err != nil {
			return err
		}
	}

	return nil
}

//nolint:cyclop // Session provenance intentionally maps several optional session fields.
func sessionDocument(saved session.Session, kind string, index int, text string, extra map[string]string, opts SessionIndexOptions) Document {
	metadata := map[string]string{
		"source_type": sessionSourceType,
		"session_id":  saved.ID,
		"kind":        kind,
	}
	if index >= 0 {
		metadata["index"] = strconv.Itoa(index)
	}

	if saved.Title != "" {
		metadata["session_title"] = saved.Title
	}

	if saved.DefaultAgent != "" {
		metadata["default_agent"] = saved.DefaultAgent
	}

	if saved.DefaultModel != "" {
		metadata["default_model"] = saved.DefaultModel
	}

	if !saved.CreatedAt.IsZero() {
		metadata["created_at"] = saved.CreatedAt.UTC().Format(time.RFC3339)
	}

	if !saved.UpdatedAt.IsZero() {
		metadata["updated_at"] = saved.UpdatedAt.UTC().Format(time.RFC3339)
	}

	if saved.WorktreePath != "" {
		metadata["worktree_path"] = saved.WorktreePath
		metadata["repo_path"] = saved.WorktreePath
	}

	if saved.WorktreeBranch != "" {
		metadata["worktree_branch"] = saved.WorktreeBranch
	}

	if saved.WorktreeBase != "" {
		metadata["worktree_base"] = saved.WorktreeBase
	}

	if len(saved.Tags) > 0 {
		metadata["tags"] = strings.Join(saved.Tags, ", ")
	}

	for key, value := range extra {
		if value = strings.TrimSpace(value); value != "" {
			metadata[key] = value
		}
	}

	agent := documentAgent(saved, extra)
	path := firstNonEmpty(extra["artifact_path"], saved.ID)
	provenance := &Provenance{
		SourceType: sessionSourceType,
		SourceID:   saved.ID,
		SessionID:  saved.ID,
		Kind:       kind,
		Agent:      agent,
		RepoPath:   saved.WorktreePath,
		Path:       path,
		Tags:       append([]string(nil), saved.Tags...),
	}
	if role := strings.TrimSpace(extra["role"]); role != "" {
		provenance.Role = role
	}
	if !saved.CreatedAt.IsZero() {
		provenance.CreatedAt = saved.CreatedAt.UTC().Format(time.RFC3339)
	}
	if !saved.UpdatedAt.IsZero() {
		provenance.UpdatedAt = saved.UpdatedAt.UTC().Format(time.RFC3339)
	}

	return Document{
		ID:         sessionDocumentID(saved.ID, kind, index),
		Path:       path,
		Text:       text,
		Metadata:   metadata,
		Provenance: provenance,
		Policy: &PolicyDecision{
			Scope:     opts.Scope,
			Retention: strings.TrimSpace(opts.Retention),
		},
	}
}

func sessionDocumentID(sessionID, kind string, index int) string {
	if index < 0 {
		return "session/" + sessionID + "/" + kind
	}

	return "session/" + sessionID + "/" + kind + "/" + strconv.Itoa(index)
}

func normalizeSessionScope(scope string) string {
	scope = strings.TrimSpace(scope)
	if scope == "" || !knownCorpusScope(scope) {
		return ScopeSession
	}

	return scope
}

func shouldIndexSessionDocument(saved session.Session, opts SessionIndexOptions, documentAgent string) bool {
	wantAgent := strings.ToLower(strings.TrimSpace(opts.Agent))
	if wantAgent == "" {
		return true
	}

	documentAgent = strings.ToLower(strings.TrimSpace(documentAgent))
	if documentAgent != "" {
		return documentAgent == wantAgent
	}

	return strings.ToLower(strings.TrimSpace(saved.DefaultAgent)) == wantAgent
}

func (s *Store) updateSessionCorpus(saved session.Session, opts SessionIndexOptions) {
	s.Corpus.Scope = opts.Scope
	if opts.Scope == ScopeGlobal {
		s.Corpus.Global = true
	}
	if strings.TrimSpace(saved.WorktreePath) != "" {
		s.Corpus.RepoPath = saved.WorktreePath
	}
	if strings.TrimSpace(opts.Agent) != "" {
		s.Corpus.Agent = strings.TrimSpace(opts.Agent)
	}
	if strings.TrimSpace(opts.Retention) != "" {
		s.Corpus.Retention = strings.TrimSpace(opts.Retention)
	}
	s.recountCorpus()
	s.normalizeCorpusMetadata()
}

func documentAgent(saved session.Session, extra map[string]string) string {
	for _, key := range []string{"agent", "source_agent"} {
		if value := strings.TrimSpace(extra[key]); value != "" {
			return value
		}
	}

	return strings.TrimSpace(saved.DefaultAgent)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}

	return ""
}

func sessionMetadataText(saved session.Session) string {
	var parts []string
	appendPart(&parts, "Session", saved.ID)
	appendPart(&parts, "Title", saved.Title)
	appendPart(&parts, "Default agent", saved.DefaultAgent)
	appendPart(&parts, "Default model", saved.DefaultModel)
	appendPart(&parts, "Worktree path", saved.WorktreePath)
	appendPart(&parts, "Worktree branch", saved.WorktreeBranch)
	appendPart(&parts, "Worktree base", saved.WorktreeBase)

	if len(saved.Tags) > 0 {
		appendPart(&parts, "Tags", strings.Join(saved.Tags, ", "))
	}

	if !saved.CreatedAt.IsZero() {
		appendPart(&parts, "Created", saved.CreatedAt.UTC().Format(time.RFC3339))
	}

	if !saved.UpdatedAt.IsZero() {
		appendPart(&parts, "Updated", saved.UpdatedAt.UTC().Format(time.RFC3339))
	}

	return strings.Join(parts, "\n")
}

func messageText(message llm.Message) string {
	var parts []string
	appendPart(&parts, "Role", string(message.Role))
	appendPart(&parts, "Content", message.Content)

	return strings.Join(parts, "\n")
}

func negativeKnowledgeText(entry session.NegativeKnowledge) string {
	var parts []string
	appendPart(&parts, "Negative knowledge", entry.Approach)
	appendPart(&parts, "Reason", entry.Reason)
	appendPart(&parts, "Commit", entry.Commit)
	appendPart(&parts, "Agent", entry.Agent)

	if !entry.CreatedAt.IsZero() {
		appendPart(&parts, "Created", entry.CreatedAt.UTC().Format(time.RFC3339))
	}

	return strings.Join(parts, "\n")
}

func evaluationText(entry session.AgentEvaluation) string {
	var parts []string
	appendPart(&parts, "Evaluation", entry.Agent)
	appendPart(&parts, "Outcome", entry.Outcome)

	if entry.Score != 0 {
		appendPart(&parts, "Score", strconv.Itoa(entry.Score))
	}

	appendPart(&parts, "Reference", entry.Reference)
	appendPart(&parts, "Notes", entry.Notes)

	if !entry.CreatedAt.IsZero() {
		appendPart(&parts, "Created", entry.CreatedAt.UTC().Format(time.RFC3339))
	}

	return strings.Join(parts, "\n")
}

func artifactText(entry session.Artifact) string {
	var parts []string
	appendPart(&parts, "Artifact", entry.Path)
	appendPart(&parts, "Kind", entry.Kind)
	appendPart(&parts, "Summary", entry.Summary)
	appendPart(&parts, "Source agent", entry.SourceAgent)

	if !entry.CreatedAt.IsZero() {
		appendPart(&parts, "Created", entry.CreatedAt.UTC().Format(time.RFC3339))
	}

	return strings.Join(parts, "\n")
}

func appendPart(parts *[]string, label, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}

	*parts = append(*parts, label+": "+value)
}
