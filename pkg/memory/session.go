package memory

import (
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/privacy"
	"github.com/tommoulard/atteler/pkg/retrieval"
	"github.com/tommoulard/atteler/pkg/session"
)

const sessionSourceType = "session"

var errMissingSessionID = errors.New("memory session id is required")

// SessionIndexPolicy controls which session fields are copied into searchable
// memory. Raw transcript messages are excluded by default because they can
// contain secrets or unrelated private content; callers must opt in explicitly.
type SessionIndexPolicy struct {
	IncludeMessages          bool
	IncludeNegativeKnowledge bool
	IncludeEvaluations       bool
	IncludeArtifacts         bool
	IncludeWorktreeMetadata  bool
}

// DefaultSessionIndexPolicy returns the privacy-preserving default session
// memory policy.
func DefaultSessionIndexPolicy() SessionIndexPolicy {
	return SessionIndexPolicy{
		IncludeNegativeKnowledge: true,
		IncludeEvaluations:       true,
		IncludeArtifacts:         true,
	}
}

// FromSessions builds a searchable memory store from saved sessions.
func FromSessions(sessions ...session.Session) (*Store, error) {
	store := NewStore()
	if err := store.AddSessions(sessions...); err != nil {
		return nil, err
	}

	return store, nil
}

// FromSessionsWithPolicy builds a searchable memory store using policy.
func FromSessionsWithPolicy(policy SessionIndexPolicy, sessions ...session.Session) (*Store, error) {
	store := NewStore()
	if err := store.AddSessionsWithPolicy(policy, sessions...); err != nil {
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

// AddSessionsWithPolicy indexes each saved session according to policy.
func (s *Store) AddSessionsWithPolicy(policy SessionIndexPolicy, sessions ...session.Session) error {
	for i := range sessions {
		if err := s.AddSessionWithPolicy(sessions[i], policy); err != nil {
			return err
		}
	}

	return nil
}

// AddSession indexes privacy-safe metadata, negative knowledge, agent
// evaluations, and artifacts for a saved session. Raw transcript messages and
// worktree metadata require AddSessionWithPolicy opt-in.
func (s *Store) AddSession(saved session.Session) error {
	return s.AddSessionWithPolicy(saved, DefaultSessionIndexPolicy())
}

// AddSessionWithPolicy indexes a saved session according to policy.
func (s *Store) AddSessionWithPolicy(saved session.Session, policy SessionIndexPolicy) error {
	sessionID := strings.TrimSpace(saved.ID)
	if sessionID == "" {
		return errMissingSessionID
	}

	if text := sessionMetadataText(saved, policy); text != "" {
		if err := s.Add(sessionDocument(saved, "metadata", -1, text, nil)); err != nil {
			return err
		}
	}

	if policy.IncludeMessages {
		if err := s.addSessionMessages(saved); err != nil {
			return err
		}
	}

	if policy.IncludeNegativeKnowledge {
		if err := s.addSessionNegativeKnowledge(saved); err != nil {
			return err
		}
	}

	if policy.IncludeEvaluations {
		if err := s.addSessionEvaluations(saved); err != nil {
			return err
		}
	}

	if policy.IncludeArtifacts {
		return s.addSessionArtifacts(saved)
	}

	return nil
}

func (s *Store) addSessionMessages(saved session.Session) error {
	for i, message := range saved.Messages {
		text := messageText(message)
		if text == "" {
			continue
		}

		if err := s.Add(sessionDocument(saved, "message", i, text, sessionMessageMetadata(message, text))); err != nil {
			return err
		}
	}

	return nil
}

func sessionMessageMetadata(message llm.Message, text string) map[string]string {
	metadata := map[string]string{
		"role":                                strings.TrimSpace(string(message.Role)),
		retrieval.MetadataSafetyInjectAllowed: "false",
		retrieval.MetadataSafetyPrivate:       "true",
	}

	if privacy.RedactText(text) != text {
		metadata[retrieval.MetadataSafetySensitive] = "true"
		metadata[retrieval.MetadataSafetyRedacted] = "true"
	}

	return metadata
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
	for i := range saved.Evaluations {
		entry := &saved.Evaluations[i]

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
		ID:         sessionDocumentID(saved.ID, kind, index),
		Path:       saved.ID,
		Text:       text,
		Metadata:   metadata,
		Provenance: sessionProvenance(saved.ID, kind),
	}
}

func sessionProvenance(sessionID, kind string) map[string]string {
	return map[string]string{
		"source_type": sessionSourceType,
		"session_id":  sessionID,
		"kind":        kind,
	}
}

func sessionDocumentID(sessionID, kind string, index int) string {
	if index < 0 {
		return "session/" + sessionID + "/" + kind
	}

	return "session/" + sessionID + "/" + kind + "/" + strconv.Itoa(index)
}

func sessionMetadataText(saved session.Session, policy SessionIndexPolicy) string {
	var parts []string
	appendPart(&parts, "Session", saved.ID)
	appendPart(&parts, "Title", saved.Title)
	appendPart(&parts, "Default agent", saved.DefaultAgent)
	appendPart(&parts, "Default model", saved.DefaultModel)

	if policy.IncludeWorktreeMetadata {
		appendPart(&parts, "Worktree path", saved.WorktreePath)
		appendPart(&parts, "Worktree branch", saved.WorktreeBranch)
		appendPart(&parts, "Worktree base", saved.WorktreeBase)
	}

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
	appendPart(&parts, "Task type", entry.TaskType)
	appendPart(&parts, "Severity", entry.Severity)

	if !entry.CreatedAt.IsZero() {
		appendPart(&parts, "Created", entry.CreatedAt.UTC().Format(time.RFC3339))
	}

	return strings.Join(parts, "\n")
}

func evaluationText(entry *session.AgentEvaluation) string {
	var parts []string
	appendPart(&parts, "Evaluation", entry.Agent)
	appendPart(&parts, "Outcome", entry.Outcome)

	if entry.Score != 0 {
		appendPart(&parts, "Score", strconv.Itoa(entry.Score))
	}

	appendPart(&parts, "Source", entry.Source)
	appendPart(&parts, "Evaluator", entry.Evaluator)
	appendPart(&parts, "Rubric version", entry.RubricVersion)
	appendPart(&parts, "Task type", entry.TaskType)
	appendPart(&parts, "Difficulty", entry.Difficulty)
	appendPart(&parts, "Expected outcome", entry.ExpectedOutcome)
	appendPart(&parts, "Model", entry.Model)
	appendPart(&parts, "Agent version", entry.AgentVersion)

	if entry.SchemaVersion != 0 {
		appendPart(&parts, "Schema version", strconv.Itoa(entry.SchemaVersion))
	}

	if entry.DurationMillis != 0 {
		appendPart(&parts, "Duration millis", strconv.FormatInt(entry.DurationMillis, 10))
	}

	if entry.Cost != 0 {
		appendPart(&parts, "Cost", strconv.FormatFloat(entry.Cost, 'f', 6, 64))
	}

	if entry.Confidence != 0 {
		appendPart(&parts, "Confidence", strconv.FormatFloat(entry.Confidence, 'f', 2, 64))
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
