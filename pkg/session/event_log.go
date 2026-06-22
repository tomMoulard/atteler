package session

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/tommoulard/atteler/pkg/llm"
)

const (
	sessionEventLogFileExt = ".events.jsonl"
	sessionLockFileExt     = ".lock"
)

// EventType identifies one append-only session audit event.
type EventType string

const (
	// EventSessionCreated records the first schema-versioned identity/config event for a session.
	EventSessionCreated EventType = "session.created"
	// EventSessionMetadataUpdated records mutable session metadata as an append-only event.
	EventSessionMetadataUpdated EventType = "session.metadata_updated"
	// EventMessageRecorded records one transcript message.
	EventMessageRecorded EventType = "message.recorded"
	// EventProviderCallRecorded records one provider/model call and token usage.
	EventProviderCallRecorded EventType = "provider_call.recorded"
	// EventToolCallRecorded records one requested tool call.
	EventToolCallRecorded EventType = "tool_call.recorded"
	// EventToolResultRecorded records one tool result.
	EventToolResultRecorded EventType = "tool_result.recorded"
	// EventFileReferenceRecorded records one replay-relevant file reference.
	EventFileReferenceRecorded EventType = "file_reference.recorded"
	// EventArtifactRecorded records one session artifact.
	EventArtifactRecorded EventType = "artifact.recorded"
	// EventFailureRecorded records one negative-knowledge/failure entry.
	EventFailureRecorded EventType = "failure.recorded"
	// EventEvaluationRecorded records one agent evaluation.
	EventEvaluationRecorded EventType = "evaluation.recorded"
	// EventWorktreeActionRecorded records worktree metadata that influenced replay.
	EventWorktreeActionRecorded EventType = "worktree_action.recorded"
	// EventVerificationGateRecorded records one verification gate decision.
	EventVerificationGateRecorded EventType = "verification_gate.recorded"
	// EventMultiAgentRunRecorded records one durable multi-agent run receipt.
	EventMultiAgentRunRecorded EventType = "multi_agent_run.recorded"
	// EventBackgroundUsageRecorded records background suggestion provider usage.
	EventBackgroundUsageRecorded EventType = "background_suggestion.recorded"
)

// ErrCorruptEventLog is returned when an audit log has a broken non-tail event
// or an invalid hash chain. A truncated final write is tolerated by Load so the
// previous durable prefix remains replayable after a crash.
var ErrCorruptEventLog = errors.New("session: corrupt event log")

// Event is one hash-chained JSONL record in a session audit log.
//
//nolint:govet // Field order mirrors the on-disk audit envelope.
type Event struct {
	At            time.Time       `json:"at"`
	Payload       json.RawMessage `json:"payload,omitempty"`
	PrevHash      string          `json:"prev_hash,omitempty"`
	Hash          string          `json:"hash,omitempty"`
	SessionID     string          `json:"session_id"`
	Type          EventType       `json:"type"`
	SchemaVersion int             `json:"schema_version"`
	Sequence      int64           `json:"sequence"`
}

//nolint:govet // Field order keeps event envelope construction readable.
type pendingEvent struct {
	payload json.RawMessage
	at      time.Time
	typ     EventType
}

type eventLogReadResult struct {
	events    []Event
	truncated bool
}

// sessionMetadataEvent carries the session-level fields needed to replay the
// JSON projection without storing transcript bodies in one mutable blob.
//
//nolint:govet // Serialized field order keeps identity, config, and repo metadata grouped.
type sessionMetadataEvent struct {
	CreatedAt             time.Time           `json:"created_at,omitzero"`
	UpdatedAt             time.Time           `json:"updated_at,omitzero"`
	ID                    string              `json:"id"`
	Title                 string              `json:"title,omitempty"`
	DefaultModel          string              `json:"default_model,omitempty"`
	DefaultReasoningLevel string              `json:"default_reasoning_level,omitempty"`
	DefaultModelMode      string              `json:"default_model_mode,omitempty"`
	DefaultAgent          string              `json:"default_agent,omitempty"`
	Autonomy              string              `json:"autonomy,omitempty"`
	AgentLoopBudget       llm.AgentLoopBudget `json:"agent_loop_budget,omitzero"`
	PromptSuggestions     string              `json:"prompt_suggestions,omitempty"`
	WorktreePath          string              `json:"worktree_path,omitempty"`
	WorktreeBranch        string              `json:"worktree_branch,omitempty"`
	WorktreeBase          string              `json:"worktree_base,omitempty"`
	Tags                  []string            `json:"tags,omitempty"`
	SchemaVersion         int                 `json:"schema_version"`
}

type messageEvent struct {
	Message llm.Message `json:"message"`
	Index   int         `json:"index"`
}

type providerCallEvent struct {
	SessionCall *ProviderCall      `json:"session_call,omitempty"`
	Call        *MultiAgentRunCall `json:"call,omitempty"`
	RunID       string             `json:"run_id,omitempty"`
	RunKind     string             `json:"run_kind,omitempty"`
	Provider    string             `json:"provider,omitempty"`
	Model       string             `json:"model,omitempty"`
	TokenUsage  tokenUsageEvent    `json:"token_usage,omitzero"`
}

type tokenUsageEvent struct {
	InputTokens           int   `json:"input_tokens,omitempty"`
	CachedInputTokens     int   `json:"cached_input_tokens,omitempty"`
	CacheWriteInputTokens int   `json:"cache_write_input_tokens,omitempty"`
	OutputTokens          int   `json:"output_tokens,omitempty"`
	TotalTokens           int   `json:"total_tokens,omitempty"`
	EstimatedCostMicros   int64 `json:"estimated_cost_micros,omitempty"`
}

type toolCallEvent struct {
	Call         llm.ToolCall `json:"call"`
	MessageIndex int          `json:"message_index"`
}

type toolResultEvent struct {
	Result       llm.ToolResult `json:"result"`
	MessageIndex int            `json:"message_index"`
}

//nolint:govet // Serialized file-reference fields are grouped by provenance meaning.
type fileReferenceEvent struct {
	Path            string `json:"path"`
	LogicalPath     string `json:"logical_path,omitempty"`
	Kind            string `json:"kind,omitempty"`
	Source          string `json:"source,omitempty"`
	SourceAgent     string `json:"source_agent,omitempty"`
	SourceSessionID string `json:"source_session_id,omitempty"`
	SHA256          string `json:"sha256,omitempty"`
	SizeBytes       int64  `json:"size_bytes,omitempty"`
	WorktreeBranch  string `json:"worktree_branch,omitempty"`
	WorktreeBase    string `json:"worktree_base,omitempty"`
}

func fileReferenceEventFromReference(ref FileReference) fileReferenceEvent {
	return fileReferenceEvent(ref)
}

type artifactEvent struct {
	Artifact Artifact `json:"artifact"`
	Index    int      `json:"index"`
}

type failureEvent struct {
	Failure NegativeKnowledge `json:"failure"`
	Index   int               `json:"index"`
}

type evaluationEvent struct {
	Evaluation AgentEvaluation `json:"evaluation"`
	Index      int             `json:"index"`
}

type worktreeActionEvent struct {
	Action string `json:"action"`
	Path   string `json:"path,omitempty"`
	Branch string `json:"branch,omitempty"`
	Base   string `json:"base,omitempty"`
}

//nolint:govet // Serialized gate fields are grouped by replay meaning.
type verificationGateEvent struct {
	Gate  MultiAgentRunGate `json:"gate"`
	RunID string            `json:"run_id,omitempty"`
	Index int               `json:"index"`
}

type multiAgentRunEvent struct {
	Run   MultiAgentRun `json:"run"`
	Index int           `json:"index"`
}

type backgroundSuggestionEvent struct {
	Usage BackgroundSuggestionUsage `json:"usage"`
}

func (s *Store) appendSnapshotEventsLocked(sessionState Session) error {
	path := s.eventLogPath(sessionState.ID)

	readResult, err := readEventLog(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	if readResult.truncated {
		return fmt.Errorf("%w: %s has a truncated tail; refusing to append until repaired", ErrCorruptEventLog, path)
	}

	events := pendingEventsForSession(sessionState)
	if len(events) == 0 {
		return nil
	}

	needsNewline, err := eventLogNeedsTrailingNewline(path)
	if err != nil {
		return err
	}

	existingKeys := existingEventKeys(readResult.events)
	existingTypes := existingEventTypes(readResult.events)

	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("session: open event log %s: %w", path, err)
	}
	defer file.Close()

	cursor := nextEventCursor(readResult.events)

	if needsNewline {
		if _, err := file.WriteString("\n"); err != nil {
			return fmt.Errorf("session: repair event log newline %s: %w", path, err)
		}
	}

	if err := appendPendingEvents(file, path, sessionState.ID, events, existingKeys, existingTypes, cursor); err != nil {
		return err
	}

	if err := file.Sync(); err != nil {
		return fmt.Errorf("session: sync event log %s: %w", path, err)
	}

	return nil
}

type eventCursor struct {
	prevHash     string
	nextSequence int64
}

func nextEventCursor(events []Event) eventCursor {
	if len(events) == 0 {
		return eventCursor{nextSequence: 1}
	}

	last := events[len(events)-1]

	return eventCursor{
		nextSequence: last.Sequence + 1,
		prevHash:     last.Hash,
	}
}

func appendPendingEvents(
	file *os.File,
	path string,
	sessionID string,
	events []pendingEvent,
	existingKeys map[string]struct{},
	existingTypes map[EventType]struct{},
	cursor eventCursor,
) error {
	for _, pending := range events {
		if pending.typ == EventSessionCreated {
			if _, ok := existingTypes[pending.typ]; ok {
				continue
			}
		} else if _, ok := existingKeys[pendingEventKey(pending)]; ok {
			continue
		}

		event := Event{
			At:            pending.at.UTC(),
			Payload:       pending.payload,
			PrevHash:      cursor.prevHash,
			SessionID:     sessionID,
			Type:          pending.typ,
			SchemaVersion: SessionEventSchemaVersion,
			Sequence:      cursor.nextSequence,
		}

		hash, err := eventDigest(event)
		if err != nil {
			return err
		}

		event.Hash = hash

		data, err := json.Marshal(event)
		if err != nil {
			return fmt.Errorf("session: marshal event: %w", err)
		}

		if _, err := file.Write(append(data, '\n')); err != nil {
			return fmt.Errorf("session: append event log %s: %w", path, err)
		}

		existingKeys[pendingEventKey(pending)] = struct{}{}
		existingTypes[pending.typ] = struct{}{}
		cursor.prevHash = event.Hash
		cursor.nextSequence++
	}

	return nil
}

func eventLogNeedsTrailingNewline(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}

		return false, fmt.Errorf("session: stat event log %s: %w", path, err)
	}

	if info.Size() == 0 {
		return false, nil
	}

	file, err := os.Open(path)
	if err != nil {
		return false, fmt.Errorf("session: open event log %s: %w", path, err)
	}
	defer file.Close()

	if _, err := file.Seek(-1, io.SeekEnd); err != nil {
		return false, fmt.Errorf("session: seek event log %s: %w", path, err)
	}

	var last [1]byte
	if _, err := file.Read(last[:]); err != nil {
		return false, fmt.Errorf("session: read event log %s: %w", path, err)
	}

	return last[0] != '\n', nil
}

func existingEventKeys(events []Event) map[string]struct{} {
	keys := make(map[string]struct{}, len(events))
	for index := range events {
		event := &events[index]
		keys[eventPayloadKey(event.Type, event.Payload)] = struct{}{}
	}

	return keys
}

func existingEventTypes(events []Event) map[EventType]struct{} {
	types := make(map[EventType]struct{}, len(events))
	for index := range events {
		types[events[index].Type] = struct{}{}
	}

	return types
}

func pendingEventKey(event pendingEvent) string {
	return eventPayloadKey(event.typ, event.payload)
}

//nolint:cyclop // This is the single schema fan-out point from a Session projection to audit events.
func pendingEventsForSession(sessionState Session) []pendingEvent {
	now := sessionState.UpdatedAt
	if now.IsZero() {
		now = time.Now().UTC()
	}

	events := []pendingEvent{
		mustPendingEvent(now, EventSessionCreated, metadataEventFromSession(sessionState)),
		mustPendingEvent(now, EventSessionMetadataUpdated, metadataEventFromSession(sessionState)),
	}

	if worktree := worktreeEventFromSession(sessionState); worktree.Path != "" || worktree.Branch != "" || worktree.Base != "" {
		events = append(events, mustPendingEvent(now, EventWorktreeActionRecorded, worktree))
	}

	for index, message := range sessionState.Messages {
		events = append(events, mustPendingEvent(now, EventMessageRecorded, messageEvent{
			Index:   index,
			Message: message,
		}))

		for _, call := range message.ToolCalls {
			events = append(events, mustPendingEvent(now, EventToolCallRecorded, toolCallEvent{
				MessageIndex: index,
				Call:         call,
			}))
		}

		if message.ToolResult != nil {
			events = append(events, mustPendingEvent(now, EventToolResultRecorded, toolResultEvent{
				MessageIndex: index,
				Result:       *message.ToolResult,
			}))
		}
	}

	events = append(events, providerCallEventsForSession(now, sessionState.ProviderCalls)...)

	for index, failure := range sessionState.NegativeKnowledge {
		events = append(events, mustPendingEvent(now, EventFailureRecorded, failureEvent{
			Index:   index,
			Failure: failure,
		}))
	}

	for index := range sessionState.Evaluations {
		evaluation := &sessionState.Evaluations[index]
		events = append(events, mustPendingEvent(now, EventEvaluationRecorded, evaluationEvent{
			Index:      index,
			Evaluation: *evaluation,
		}))

		if strings.TrimSpace(evaluation.Reference) != "" {
			events = append(events, mustPendingEvent(now, EventFileReferenceRecorded, fileReferenceEvent{
				Path:        evaluation.Reference,
				Kind:        "evaluation",
				Source:      "evaluation.reference",
				SourceAgent: evaluation.Agent,
			}))
		}
	}

	for index := range sessionState.Artifacts {
		artifact := &sessionState.Artifacts[index]
		events = append(
			events,
			mustPendingEvent(now, EventArtifactRecorded, artifactEvent{
				Index:    index,
				Artifact: *artifact,
			}),
			mustPendingEvent(now, EventFileReferenceRecorded, fileReferenceFromArtifact(*artifact)),
		)
	}

	for index := range sessionState.MultiAgentRuns {
		run := &sessionState.MultiAgentRuns[index]
		events = append(events, mustPendingEvent(now, EventMultiAgentRunRecorded, multiAgentRunEvent{
			Index: index,
			Run:   *run,
		}))

		for callIndex := range run.Calls {
			call := &run.Calls[callIndex]
			events = append(events, mustPendingEvent(now, EventProviderCallRecorded, providerCallFromRun(*run, *call)))
		}

		for gateIndex, gate := range run.Gates {
			events = append(events, mustPendingEvent(now, EventVerificationGateRecorded, verificationGateEvent{
				RunID: run.ID,
				Index: gateIndex,
				Gate:  gate,
			}))
		}
	}

	if sessionState.BackgroundSuggestions != nil {
		events = append(events, mustPendingEvent(now, EventBackgroundUsageRecorded, backgroundSuggestionEvent{
			Usage: *sessionState.BackgroundSuggestions,
		}))
	}

	return events
}

func providerCallEventsForSession(at time.Time, calls []ProviderCall) []pendingEvent {
	if len(calls) == 0 {
		return nil
	}

	events := make([]pendingEvent, 0, len(calls))
	for index := range calls {
		call := normalizeProviderCall(calls[index])
		events = append(events, mustPendingEvent(at, EventProviderCallRecorded, providerCallFromSession(call)))

		for refIndex := range call.ReferencedFiles {
			events = append(events, mustPendingEvent(
				at,
				EventFileReferenceRecorded,
				fileReferenceEventFromReference(call.ReferencedFiles[refIndex]),
			))
		}
	}

	return events
}

func mustPendingEvent(at time.Time, typ EventType, payload any) pendingEvent {
	data, err := json.Marshal(payload)
	if err != nil {
		panic(fmt.Sprintf("session: marshal event payload %s: %v", typ, err))
	}

	return pendingEvent{
		at:      at,
		typ:     typ,
		payload: compactJSON(data),
	}
}

func metadataEventFromSession(sessionState Session) sessionMetadataEvent {
	return sessionMetadataEvent{
		ID:                    sessionState.ID,
		CreatedAt:             sessionState.CreatedAt,
		UpdatedAt:             sessionState.UpdatedAt,
		Title:                 sessionState.Title,
		DefaultModel:          sessionState.DefaultModel,
		DefaultReasoningLevel: sessionState.DefaultReasoningLevel,
		DefaultModelMode:      sessionState.DefaultModelMode,
		DefaultAgent:          sessionState.DefaultAgent,
		Autonomy:              sessionState.Autonomy,
		AgentLoopBudget:       sessionState.AgentLoopBudget,
		PromptSuggestions:     sessionState.PromptSuggestions,
		WorktreePath:          sessionState.WorktreePath,
		WorktreeBranch:        sessionState.WorktreeBranch,
		WorktreeBase:          sessionState.WorktreeBase,
		Tags:                  append([]string(nil), sessionState.Tags...),
		SchemaVersion:         SessionSchemaVersion,
	}
}

func worktreeEventFromSession(sessionState Session) worktreeActionEvent {
	return worktreeActionEvent{
		Action: "metadata.recorded",
		Path:   sessionState.WorktreePath,
		Branch: sessionState.WorktreeBranch,
		Base:   sessionState.WorktreeBase,
	}
}

func fileReferenceFromArtifact(artifact Artifact) fileReferenceEvent {
	return fileReferenceEvent{
		Path:            artifact.Path,
		LogicalPath:     artifact.LogicalPath,
		Kind:            artifact.Kind,
		Source:          "artifact",
		SourceAgent:     artifact.SourceAgent,
		SourceSessionID: artifact.SourceSessionID,
		SHA256:          artifact.SHA256,
		SizeBytes:       artifact.SizeBytes,
		WorktreeBranch:  artifact.WorktreeBranch,
		WorktreeBase:    artifact.WorktreeBase,
	}
}

func providerCallFromRun(run MultiAgentRun, call MultiAgentRunCall) providerCallEvent {
	model := firstNonEmptyString(call.ResponseModel, call.RequestedModel, run.Model)

	return providerCallEvent{
		RunID:    run.ID,
		RunKind:  run.Kind,
		Call:     &call,
		Model:    model,
		Provider: providerFromModel(model),
		TokenUsage: tokenUsageEvent{
			InputTokens:         call.InputTokens,
			CachedInputTokens:   call.CachedInputTokens,
			OutputTokens:        call.OutputTokens,
			TotalTokens:         call.TotalTokens,
			EstimatedCostMicros: call.EstimatedCostMicros,
		},
	}
}

func providerCallFromSession(call ProviderCall) providerCallEvent {
	model := firstNonEmptyString(call.ResponseModel, call.RequestedModel)

	return providerCallEvent{
		SessionCall: &call,
		Model:       model,
		Provider:    firstNonEmptyString(call.Provider, providerFromModel(model)),
		TokenUsage: tokenUsageEvent{
			InputTokens:           call.InputTokens,
			CachedInputTokens:     call.CachedInputTokens,
			CacheWriteInputTokens: call.CacheWriteInputTokens,
			OutputTokens:          call.OutputTokens,
			TotalTokens:           call.TotalTokens,
			EstimatedCostMicros:   0,
		},
	}
}

func (s *Store) loadEventLogSession(path string) (Session, error) {
	readResult, err := readEventLog(path)
	if err != nil {
		return Session{}, err
	}

	sessionState, err := replayEvents(readResult.events)
	if err != nil {
		return Session{}, err
	}

	if sessionState.ID == "" {
		sessionState.ID = idFromEventLogPath(path)
	}

	sessionState.SchemaVersion = SessionSchemaVersion
	sessionState.EventLog = eventLogMetadata(path, readResult)

	return sessionState, nil
}

//nolint:cyclop,gocognit // Replay centralizes event-version dispatch so migration behavior stays auditable.
func replayEvents(events []Event) (Session, error) {
	var sessionState Session

	seenMessages := make(map[string]struct{})
	seenProviderCalls := make(map[string]struct{})
	seenFailures := make(map[string]struct{})
	seenEvaluations := make(map[string]struct{})
	artifactIndexes := make(map[string]int)

	for index := range events {
		event := &events[index]
		if event.SessionID != "" && sessionState.ID == "" {
			sessionState.ID = event.SessionID
		}

		if event.At.After(sessionState.UpdatedAt) {
			sessionState.UpdatedAt = event.At
		}

		switch event.Type {
		case EventSessionCreated, EventSessionMetadataUpdated:
			var metadata sessionMetadataEvent
			if err := unmarshalEventPayload(event, &metadata); err != nil {
				return Session{}, err
			}

			applyMetadataEvent(&sessionState, metadata)
		case EventMessageRecorded:
			var payload messageEvent
			if err := unmarshalEventPayload(event, &payload); err != nil {
				return Session{}, err
			}

			key := eventPayloadKey(event.Type, event.Payload)
			if _, ok := seenMessages[key]; ok {
				continue
			}

			seenMessages[key] = struct{}{}

			sessionState.Messages = append(sessionState.Messages, payload.Message)
		case EventProviderCallRecorded:
			var payload providerCallEvent
			if err := unmarshalEventPayload(event, &payload); err != nil {
				return Session{}, err
			}

			if payload.SessionCall == nil {
				continue
			}

			call := normalizeProviderCall(*payload.SessionCall)

			key := providerCallIdentity(call)
			if _, ok := seenProviderCalls[key]; ok {
				continue
			}

			seenProviderCalls[key] = struct{}{}

			sessionState.ProviderCalls = append(sessionState.ProviderCalls, call)
		case EventFailureRecorded:
			var payload failureEvent
			if err := unmarshalEventPayload(event, &payload); err != nil {
				return Session{}, err
			}

			key := eventPayloadKey(event.Type, event.Payload)
			if _, ok := seenFailures[key]; ok {
				continue
			}

			seenFailures[key] = struct{}{}

			sessionState.NegativeKnowledge = append(sessionState.NegativeKnowledge, payload.Failure)
		case EventEvaluationRecorded:
			var payload evaluationEvent
			if err := unmarshalEventPayload(event, &payload); err != nil {
				return Session{}, err
			}

			key := eventPayloadKey(event.Type, event.Payload)
			if _, ok := seenEvaluations[key]; ok {
				continue
			}

			seenEvaluations[key] = struct{}{}

			sessionState.Evaluations = append(sessionState.Evaluations, payload.Evaluation)
		case EventArtifactRecorded:
			var payload artifactEvent
			if err := unmarshalEventPayload(event, &payload); err != nil {
				return Session{}, err
			}

			key := artifactEventKey(payload)
			if existing, ok := artifactIndexes[key]; ok {
				sessionState.Artifacts[existing] = payload.Artifact
				continue
			}

			artifactIndexes[key] = len(sessionState.Artifacts)
			sessionState.Artifacts = append(sessionState.Artifacts, payload.Artifact)
		case EventMultiAgentRunRecorded:
			var payload multiAgentRunEvent
			if err := unmarshalEventPayload(event, &payload); err != nil {
				return Session{}, err
			}

			upsertMultiAgentRunProjection(&sessionState, payload.Run)
		case EventBackgroundUsageRecorded:
			var payload backgroundSuggestionEvent
			if err := unmarshalEventPayload(event, &payload); err != nil {
				return Session{}, err
			}

			usage := payload.Usage
			sessionState.BackgroundSuggestions = &usage
		case EventToolCallRecorded,
			EventToolResultRecorded,
			EventFileReferenceRecorded,
			EventWorktreeActionRecorded,
			EventVerificationGateRecorded:
			// These audit events are intentionally redundant with message,
			// artifact, worktree and run payloads. They make replay provenance
			// inspectable without changing the projected Session shape.
			continue
		default:
			if event.SchemaVersion > SessionEventSchemaVersion {
				return Session{}, fmt.Errorf(
					"%w: event %d uses unsupported schema %d",
					ErrCorruptEventLog,
					event.Sequence,
					event.SchemaVersion,
				)
			}
		}
	}

	return sessionState, nil
}

func upsertMultiAgentRunProjection(sessionState *Session, run MultiAgentRun) {
	if strings.TrimSpace(run.ID) == "" {
		return
	}

	if strings.TrimSpace(run.ReceiptID) == "" {
		run.ReceiptID = run.ID
	}

	for i := range sessionState.MultiAgentRuns {
		if sessionState.MultiAgentRuns[i].ID == run.ID {
			sessionState.MultiAgentRuns[i] = run
			return
		}
	}

	sessionState.MultiAgentRuns = append(sessionState.MultiAgentRuns, run)
}

func applyMetadataEvent(sessionState *Session, metadata sessionMetadataEvent) {
	if metadata.ID != "" {
		sessionState.ID = metadata.ID
	}

	if !metadata.CreatedAt.IsZero() &&
		(sessionState.CreatedAt.IsZero() || metadata.CreatedAt.Before(sessionState.CreatedAt)) {
		sessionState.CreatedAt = metadata.CreatedAt
	}

	if !metadata.UpdatedAt.IsZero() && metadata.UpdatedAt.After(sessionState.UpdatedAt) {
		sessionState.UpdatedAt = metadata.UpdatedAt
	}

	sessionState.Title = metadata.Title
	sessionState.DefaultModel = metadata.DefaultModel
	sessionState.DefaultReasoningLevel = metadata.DefaultReasoningLevel
	sessionState.DefaultModelMode = metadata.DefaultModelMode
	sessionState.DefaultAgent = metadata.DefaultAgent
	sessionState.Autonomy = metadata.Autonomy
	sessionState.AgentLoopBudget = metadata.AgentLoopBudget
	sessionState.PromptSuggestions = metadata.PromptSuggestions
	sessionState.WorktreePath = metadata.WorktreePath
	sessionState.WorktreeBranch = metadata.WorktreeBranch
	sessionState.WorktreeBase = metadata.WorktreeBase
	sessionState.Tags = append([]string(nil), metadata.Tags...)
	sessionState.SchemaVersion = SessionSchemaVersion
}

//nolint:gocognit // Log recovery keeps valid-prefix, corrupt-tail, and hash-chain decisions together.
func readEventLog(path string) (eventLogReadResult, error) {
	file, err := os.Open(path)
	if err != nil {
		return eventLogReadResult{}, fmt.Errorf("session: read event log %s: %w", path, err)
	}
	defer file.Close()

	var result eventLogReadResult

	reader := bufio.NewReader(file)
	lineNumber := 0
	prevHash := ""
	expectedSequence := int64(1)

	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) == 0 && errors.Is(readErr, io.EOF) {
			break
		}

		lineNumber++
		completeLine := bytes.HasSuffix(line, []byte{'\n'})

		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			if readErr != nil && !errors.Is(readErr, io.EOF) {
				return eventLogReadResult{}, fmt.Errorf("session: read event log %s: %w", path, readErr)
			}

			if errors.Is(readErr, io.EOF) {
				break
			}

			continue
		}

		event, parseErr := parseEventLogLine(line, path, lineNumber, prevHash, expectedSequence)
		if parseErr != nil {
			if !completeLine && errors.Is(readErr, io.EOF) {
				result.truncated = true
				break
			}

			return eventLogReadResult{}, parseErr
		}

		result.events = append(result.events, event)
		prevHash = event.Hash
		expectedSequence++

		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}

			return eventLogReadResult{}, fmt.Errorf("session: read event log %s: %w", path, readErr)
		}
	}

	return result, nil
}

func parseEventLogLine(line []byte, path string, lineNumber int, prevHash string, expectedSequence int64) (Event, error) {
	var event Event
	if err := json.Unmarshal(line, &event); err != nil {
		return Event{}, fmt.Errorf("%w: parse %s line %d: %w", ErrCorruptEventLog, path, lineNumber, err)
	}

	if event.SchemaVersion != SessionEventSchemaVersion {
		return Event{}, fmt.Errorf(
			"%w: %s line %d has schema %d",
			ErrCorruptEventLog,
			path,
			lineNumber,
			event.SchemaVersion,
		)
	}

	if event.Sequence != expectedSequence {
		return Event{}, fmt.Errorf(
			"%w: %s line %d sequence %d after %d",
			ErrCorruptEventLog,
			path,
			lineNumber,
			event.Sequence,
			expectedSequence-1,
		)
	}

	if event.PrevHash != prevHash {
		return Event{}, fmt.Errorf("%w: %s line %d prev_hash mismatch", ErrCorruptEventLog, path, lineNumber)
	}

	hash, err := eventDigest(event)
	if err != nil {
		return Event{}, err
	}

	if event.Hash != hash {
		return Event{}, fmt.Errorf("%w: %s line %d hash mismatch", ErrCorruptEventLog, path, lineNumber)
	}

	event.Payload = compactJSON(event.Payload)

	return event, nil
}

func eventDigest(event Event) (string, error) {
	//nolint:govet // Digest field order mirrors the persisted envelope.
	payload := struct {
		At            time.Time       `json:"at"`
		Payload       json.RawMessage `json:"payload,omitempty"`
		PrevHash      string          `json:"prev_hash,omitempty"`
		SessionID     string          `json:"session_id"`
		Type          EventType       `json:"type"`
		SchemaVersion int             `json:"schema_version"`
		Sequence      int64           `json:"sequence"`
	}{
		At:            event.At.UTC(),
		Payload:       compactJSON(event.Payload),
		PrevHash:      event.PrevHash,
		SessionID:     event.SessionID,
		Type:          event.Type,
		SchemaVersion: event.SchemaVersion,
		Sequence:      event.Sequence,
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("session: marshal event digest: %w", err)
	}

	sum := sha256.Sum256(data)

	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func compactJSON(data []byte) json.RawMessage {
	if len(data) == 0 {
		return nil
	}

	var out bytes.Buffer
	if err := json.Compact(&out, data); err != nil {
		return append(json.RawMessage(nil), data...)
	}

	return append(json.RawMessage(nil), out.Bytes()...)
}

func unmarshalEventPayload(event *Event, target any) error {
	if err := json.Unmarshal(event.Payload, target); err != nil {
		return fmt.Errorf("%w: decode %s event %d: %w", ErrCorruptEventLog, event.Type, event.Sequence, err)
	}

	return nil
}

func eventPayloadKey(typ EventType, payload json.RawMessage) string {
	return string(typ) + ":" + hashJSON(compactJSON(payload))
}

func artifactEventKey(payload artifactEvent) string {
	parts := []string{
		strconvFromInt(payload.Index),
		payload.Artifact.Path,
		payload.Artifact.Kind,
		payload.Artifact.SourceSessionID,
		strconvFromInt(payload.Artifact.SourceTurn),
		payload.Artifact.CreatedAt.UTC().Format(time.RFC3339Nano),
	}

	return strings.Join(parts, "\x00")
}

func eventLogMetadata(path string, result eventLogReadResult) *EventLogMetadata {
	metadata := &EventLogMetadata{
		Path:          path,
		SchemaVersion: SessionEventSchemaVersion,
		EventCount:    len(result.events),
		TruncatedTail: result.truncated,
	}

	if len(result.events) > 0 {
		last := result.events[len(result.events)-1]
		metadata.LastSequence = last.Sequence
		metadata.LastHash = last.Hash
	}

	return metadata
}

func (s *Store) writeSessionProjectionLocked(sessionState Session) error {
	path := s.path(sessionState.ID)

	data, err := json.MarshalIndent(sessionState, "", "  ")
	if err != nil {
		return fmt.Errorf("session: marshal projection: %w", err)
	}

	data = append(data, '\n')

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

func readLegacyJSONSession(path string) (Session, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Session{}, fmt.Errorf("session: read %s: %w", path, err)
	}

	var sessionState Session
	if err := json.Unmarshal(data, &sessionState); err != nil {
		return Session{}, fmt.Errorf("session: parse %s: %w", path, err)
	}

	if sessionState.ID == "" {
		sessionState.ID = idFromPath(path)
	}

	if sessionState.SchemaVersion == 0 {
		sessionState.SchemaVersion = 1
	}

	return sessionState, nil
}

func summarizeSession(path string, sessionState Session) Summary {
	return Summary{
		ID:              sessionState.ID,
		Title:           sessionState.Title,
		Path:            path,
		CreatedAt:       sessionState.CreatedAt,
		UpdatedAt:       sessionState.UpdatedAt,
		AgentNames:      appendSummaryAgentNames([]string{sessionState.DefaultAgent}, sessionAgentNames(sessionState)...),
		DefaultModel:    sessionState.DefaultModel,
		DefaultAgent:    sessionState.DefaultAgent,
		Autonomy:        sessionState.Autonomy,
		AgentLoopBudget: sessionState.AgentLoopBudget,
		WorktreePath:    sessionState.WorktreePath,
		WorktreeBranch:  sessionState.WorktreeBranch,
		WorktreeBase:    sessionState.WorktreeBase,
		Tags:            append([]string(nil), sessionState.Tags...),
		Messages:        len(sessionState.Messages),
	}
}

func sessionAgentNames(sessionState Session) []string {
	var names []string
	for _, failure := range sessionState.NegativeKnowledge {
		names = append(names, failure.Agent)
	}

	for index := range sessionState.Evaluations {
		names = append(names, sessionState.Evaluations[index].Agent)
	}

	for index := range sessionState.Artifacts {
		names = append(names, sessionState.Artifacts[index].SourceAgent)
	}

	return names
}

func (s *Store) withSessionLock(id string, fn func() error) (lockErr error) {
	if err := os.MkdirAll(s.dir, 0o750); err != nil {
		return fmt.Errorf("session: create dir: %w", err)
	}

	file, err := os.OpenFile(s.sessionLockPath(id), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("session: open lock: %w", err)
	}
	defer file.Close()

	if err := lockSessionFile(file, "session lock"); err != nil {
		return err
	}

	defer func() {
		if unlockErr := unlockSessionFile(file, "session lock"); lockErr == nil && unlockErr != nil {
			lockErr = unlockErr
		}
	}()

	return fn()
}

func (s *Store) sessionLockPath(id string) string {
	base := filepath.Base(s.path(id))
	if base == "." || base == string(os.PathSeparator) || strings.TrimSpace(base) == "" {
		base = strings.TrimSpace(id)
	}

	return filepath.Join(s.dir, "."+base+sessionLockFileExt)
}

func (s *Store) eventLogPath(ref string) string {
	if ref == "" {
		return ""
	}

	if strings.HasSuffix(ref, sessionEventLogFileExt) {
		if filepath.IsAbs(ref) || strings.ContainsRune(ref, rune(os.PathSeparator)) {
			return ref
		}

		return filepath.Join(s.dir, ref)
	}

	path := s.path(ref)
	if base, ok := strings.CutSuffix(path, sessionFileExt); ok {
		return base + sessionEventLogFileExt
	}

	return path + sessionEventLogFileExt
}

func (s *Store) eventProjectionPathForID(id string) string {
	return filepath.Join(s.dir, id+sessionFileExt)
}

func isSessionEventLogFileName(name string) bool {
	return strings.HasSuffix(name, sessionEventLogFileExt) && !strings.HasPrefix(name, ".")
}

func idFromEventLogPath(path string) string {
	base := filepath.Base(path)
	id, _ := strings.CutSuffix(base, sessionEventLogFileExt)

	return id
}

func providerFromModel(model string) string {
	provider, _, ok := strings.Cut(strings.TrimSpace(model), "/")
	if !ok {
		return ""
	}

	return strings.TrimSpace(provider)
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}

	return ""
}

func strconvFromInt(value int) string {
	return strconv.Itoa(value)
}
