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
	sessionID := strings.TrimSpace(saved.ID)
	if sessionID == "" {
		return errMissingSessionID
	}

	if text := sessionMetadataText(saved); text != "" {
		if err := s.Add(sessionDocument(saved, "metadata", -1, text, nil)); err != nil {
			return err
		}
	}
	if err := s.addSessionMessages(saved); err != nil {
		return err
	}
	if err := s.addSessionNegativeKnowledge(saved); err != nil {
		return err
	}
	if err := s.addSessionEvaluations(saved); err != nil {
		return err
	}
	return s.addSessionArtifacts(saved)
}

func (s *Store) addSessionMessages(saved session.Session) error {
	for i, message := range saved.Messages {
		text := messageText(message)
		if text == "" {
			continue
		}
		if err := s.Add(sessionDocument(saved, "message", i, text, map[string]string{
			"role": strings.TrimSpace(string(message.Role)),
		})); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) addSessionNegativeKnowledge(saved session.Session) error {
	for i, entry := range saved.NegativeKnowledge {
		text := negativeKnowledgeText(entry)
		if text == "" {
			continue
		}
		if err := s.Add(sessionDocument(saved, "negative_knowledge", i, text, map[string]string{
			"agent":  strings.TrimSpace(entry.Agent),
			"commit": strings.TrimSpace(entry.Commit),
		})); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) addSessionEvaluations(saved session.Session) error {
	for i, entry := range saved.Evaluations {
		text := evaluationText(entry)
		if text == "" {
			continue
		}
		if err := s.Add(sessionDocument(saved, "evaluation", i, text, map[string]string{
			"agent":     strings.TrimSpace(entry.Agent),
			"outcome":   strings.TrimSpace(entry.Outcome),
			"reference": strings.TrimSpace(entry.Reference),
		})); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) addSessionArtifacts(saved session.Session) error {
	for i, entry := range saved.Artifacts {
		text := artifactText(entry)
		if text == "" {
			continue
		}
		if err := s.Add(sessionDocument(saved, "artifact", i, text, map[string]string{
			"artifact_path": strings.TrimSpace(entry.Path),
			"artifact_kind": strings.TrimSpace(entry.Kind),
			"source_agent":  strings.TrimSpace(entry.SourceAgent),
		})); err != nil {
			return err
		}
	}
	return nil
}

func sessionDocument(saved session.Session, kind string, index int, text string, extra map[string]string) Document {
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
	for key, value := range extra {
		if value = strings.TrimSpace(value); value != "" {
			metadata[key] = value
		}
	}

	return Document{
		ID:       sessionDocumentID(saved.ID, kind, index),
		Path:     saved.ID,
		Text:     text,
		Metadata: metadata,
	}
}

func sessionDocumentID(sessionID, kind string, index int) string {
	if index < 0 {
		return "session/" + sessionID + "/" + kind
	}
	return "session/" + sessionID + "/" + kind + "/" + strconv.Itoa(index)
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
