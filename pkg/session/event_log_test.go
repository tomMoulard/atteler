package session

import (
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/llm"
)

const eventLogTestWorktreePath = "/repo/atteler"

func TestStore_SaveWritesAppendOnlyEventLogAndLoadsWithoutProjection(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	sessionState := New("openai/gpt-test", []llm.Message{{
		Role:    llm.RoleUser,
		Content: "inspect artifact",
	}, {
		Role:    llm.RoleAssistant,
		Content: "calling tool",
		ToolCalls: []llm.ToolCall{{
			ID:    "tool-1",
			Name:  "read_file",
			Input: map[string]any{"path": "README.md"},
		}},
	}, {
		Role:    llm.RoleTool,
		Content: "README contents",
		ToolResult: &llm.ToolResult{
			ToolCallID: "tool-1",
			Content:    "ok",
		},
	}})
	sessionState.WorktreePath = eventLogTestWorktreePath
	sessionState.WorktreeBranch = "feature/audit-log"
	sessionState.WorktreeBase = "main"
	require.True(t, sessionState.RecordNegativeKnowledge("rewrite log", "lost provenance", "abc123", "critic"))
	require.True(t, sessionState.AddArtifact(Artifact{
		Path:        "reports/replay.md",
		Kind:        "report",
		Summary:     "Replay report",
		SourceAgent: "verifier",
		SHA256:      "abcdef",
		SizeBytes:   42,
		CreatedAt:   time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
	}))
	require.True(t, sessionState.RecordEvaluationDetails(AgentEvaluation{
		CreatedAt:     time.Date(2026, 5, 1, 13, 0, 0, 0, time.UTC),
		Agent:         "verifier",
		Outcome:       "pass",
		Provider:      "openai",
		Model:         "openai/gpt-test",
		Reference:     "eval/report.json",
		InputTokens:   7,
		OutputTokens:  5,
		RubricVersion: "audit/v1",
	}))

	run := NewMultiAgentRun(MultiAgentRunKindReview, "audit", "openai/gpt-test", nil, MultiAgentRunBudget{})
	run.ID = "run-1"
	run.ReceiptID = "receipt-1"
	run.Status = MultiAgentRunStatusCompleted
	run.Usage = MultiAgentRunUsage{ModelCalls: 1, InputTokens: 10, OutputTokens: 4, TotalTokens: 14}
	run.Calls = []MultiAgentRunCall{{
		ID:             "call-1",
		Phase:          "review",
		Status:         MultiAgentRunStatusCompleted,
		RequestedModel: "openai/gpt-test",
		ResponseModel:  "openai/gpt-test",
		InputTokens:    10,
		OutputTokens:   4,
		TotalTokens:    14,
	}}
	run.Gates = []MultiAgentRunGate{{Name: "tests", Phase: "verify", Agent: "verifier", Passed: true}}
	require.True(t, sessionState.UpsertMultiAgentRun(run))

	require.NoError(t, store.Save(sessionState))

	eventPath := store.EventLogPath(sessionState.ID)
	data, err := os.ReadFile(eventPath)
	require.NoError(t, err)

	for _, want := range []string{
		string(EventMessageRecorded),
		string(EventProviderCallRecorded),
		string(EventToolCallRecorded),
		string(EventToolResultRecorded),
		string(EventFileReferenceRecorded),
		string(EventArtifactRecorded),
		string(EventFailureRecorded),
		string(EventEvaluationRecorded),
		string(EventWorktreeActionRecorded),
		string(EventVerificationGateRecorded),
		string(EventMultiAgentRunRecorded),
		`"schema_version":1`,
		`"hash":"sha256:`,
	} {
		assert.Contains(t, string(data), want)
	}

	require.NoError(t, os.Remove(store.Path(sessionState.ID)))
	loaded, err := store.Load(sessionState.ID)
	require.NoError(t, err)
	require.NotNil(t, loaded.EventLog)
	assert.Equal(t, SessionSchemaVersion, loaded.SchemaVersion)
	assert.Equal(t, eventPath, loaded.EventLog.Path)
	assert.Len(t, loaded.Messages, 3)
	assert.Len(t, loaded.Artifacts, 1)
	assert.Len(t, loaded.Evaluations, 1)
	assert.Len(t, loaded.NegativeKnowledge, 1)
	assert.Len(t, loaded.MultiAgentRuns, 1)
}

func TestStore_MigrateLegacyJSONSessionCreatesVersionedEventLog(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	require.NoError(t, os.WriteFile(
		store.Path("legacy"),
		[]byte(`{"id":"legacy","default_model":"gpt-legacy","messages":[{"role":"user","content":"hi"}]}`),
		0o600,
	))

	require.NoError(t, store.Migrate("legacy"))

	loaded, err := store.Load("legacy")
	require.NoError(t, err)
	require.NotNil(t, loaded.EventLog)
	assert.Equal(t, SessionSchemaVersion, loaded.SchemaVersion)
	assert.Equal(t, "gpt-legacy", loaded.DefaultModel)
	require.Len(t, loaded.Messages, 1)

	var first Event

	lines := strings.Split(strings.TrimSpace(readSessionTestFile(t, store.EventLogPath("legacy"))), "\n")
	require.NotEmpty(t, lines)
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &first))
	assert.Equal(t, SessionEventSchemaVersion, first.SchemaVersion)
	assert.Equal(t, int64(1), first.Sequence)
}

func TestStore_LoadLegacyJSONSessionWithoutEventLogPreservesBackwardCompatibility(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	require.NoError(t, os.WriteFile(
		store.Path("legacy-json"),
		[]byte(`{
  "title":"Legacy JSON transcript",
  "default_model":"openai/gpt-legacy",
  "messages":[{"role":"user","content":"legacy hi"}],
  "negative_knowledge":[{"approach":"rewrite everything","reason":"lost audit trail","agent":"critic"}],
  "evaluations":[{"agent":"verifier","outcome":"pass","reference":"eval/legacy.json"}],
  "artifacts":[{"path":"notes/legacy.md","kind":"note","summary":"legacy artifact"}]
}`),
		0o600,
	))

	loaded, err := store.Load("legacy-json")
	require.NoError(t, err)

	assert.Equal(t, "legacy-json", loaded.ID)
	assert.Equal(t, 1, loaded.SchemaVersion)
	assert.Nil(t, loaded.EventLog)
	assert.Equal(t, "openai/gpt-legacy", loaded.DefaultModel)
	require.Len(t, loaded.Messages, 1)
	assert.Equal(t, "legacy hi", loaded.Messages[0].Content)
	require.Len(t, loaded.NegativeKnowledge, 1)
	require.Len(t, loaded.Evaluations, 1)
	require.Len(t, loaded.Artifacts, 1)

	_, err = os.Stat(store.EventLogPath("legacy-json"))
	require.ErrorIs(t, err, os.ErrNotExist, "loading legacy JSON should not require or create an event log")

	loaded.Append(llm.RoleAssistant, "legacy response")
	require.NoError(t, store.Save(loaded))

	replayed, err := store.Load("legacy-json")
	require.NoError(t, err)
	require.NotNil(t, replayed.EventLog)
	assert.Equal(t, SessionSchemaVersion, replayed.SchemaVersion)
	require.Len(t, replayed.Messages, 2)
	assert.Equal(t, "legacy response", replayed.Messages[1].Content)
	assert.Len(t, replayed.NegativeKnowledge, 1)
	assert.Len(t, replayed.Evaluations, 1)
	assert.Len(t, replayed.Artifacts, 1)

	data := readSessionTestFile(t, store.EventLogPath("legacy-json"))
	assert.Contains(t, data, string(EventMessageRecorded))
	assert.Contains(t, data, string(EventFailureRecorded))
	assert.Contains(t, data, string(EventEvaluationRecorded))
	assert.Contains(t, data, string(EventArtifactRecorded))
}

func TestStore_MigrateExistingEventLogValidatesAndRefreshesProjection(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	sessionState := New("gpt-test", []llm.Message{{Role: llm.RoleUser, Content: "event-only migration"}})
	sessionState.ID = "event-only"
	require.NoError(t, store.Save(sessionState))
	require.NoError(t, os.Remove(store.Path(sessionState.ID)))

	require.NoError(t, store.Migrate(sessionState.ID))

	projection, err := readLegacyJSONSession(store.Path(sessionState.ID))
	require.NoError(t, err)
	require.NotNil(t, projection.EventLog)
	assert.Equal(t, sessionState.ID, projection.ID)
	assert.Equal(t, SessionSchemaVersion, projection.SchemaVersion)
	require.Len(t, projection.Messages, 1)
	assert.Equal(t, "event-only migration", projection.Messages[0].Content)
}

func TestStore_MigrateExistingCorruptEventLogReturnsError(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	sessionState := New("gpt-test", []llm.Message{{Role: llm.RoleUser, Content: "safe prefix"}})
	require.NoError(t, store.Save(sessionState))

	file, err := os.OpenFile(store.EventLogPath(sessionState.ID), os.O_WRONLY|os.O_APPEND, 0)
	require.NoError(t, err)
	_, err = file.WriteString("{not-json}\n")
	require.NoError(t, err)
	require.NoError(t, file.Close())

	err = store.Migrate(sessionState.ID)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrCorruptEventLog)
}

func TestStore_ConcurrentSavesMergeMessagesThroughEventLog(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	base := New("gpt-test", []llm.Message{{Role: llm.RoleUser, Content: "base"}})
	base.ID = "shared"
	require.NoError(t, store.Save(base))

	left, err := store.Load(base.ID)
	require.NoError(t, err)
	right, err := store.Load(base.ID)
	require.NoError(t, err)
	left.Append(llm.RoleAssistant, "left")
	right.Append(llm.RoleAssistant, "right")

	var wg sync.WaitGroup

	errs := make(chan error, 2)

	for _, next := range []Session{left, right} {
		wg.Add(1)

		go func(sessionState Session) {
			defer wg.Done()

			errs <- store.Save(sessionState)
		}(next)
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		require.NoError(t, err)
	}

	loaded, err := store.Load(base.ID)
	require.NoError(t, err)

	var contents []string
	for _, message := range loaded.Messages {
		contents = append(contents, message.Content)
	}

	assert.Contains(t, contents, "base")
	assert.Contains(t, contents, "left")
	assert.Contains(t, contents, "right")
}

func TestStore_SaveRecordsOrdinaryProviderCallProvenance(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	sessionState := New("openai/gpt-test", []llm.Message{{Role: llm.RoleUser, Content: "summarize @README.md"}})
	call := NewProviderCall(ProviderCallRecord{
		CompletedAt: time.Date(2026, 5, 1, 14, 0, 0, 0, time.UTC),
		Source:      "run_once",
		Params: llm.CompleteParams{
			Model:          "openai/gpt-test",
			ModelMode:      "fast",
			ReasoningLevel: "high",
			Messages: []llm.Message{
				{Role: llm.RoleSystem, Content: "reference context"},
				{Role: llm.RoleUser, Content: "summarize README"},
				{
					Role:    llm.RoleAssistant,
					Content: "reading file",
					ToolCalls: []llm.ToolCall{{
						ID:    "tool-1",
						Name:  "read_file",
						Input: map[string]any{"path": "README.md"},
					}},
				},
				{
					Role:    llm.RoleTool,
					Content: "README contents",
					ToolResult: &llm.ToolResult{
						ToolCallID: "tool-1",
						Content:    "README contents",
					},
				},
			},
			MaxTokens: 256,
		},
		Response: &llm.Response{
			Content:      "summary",
			Provider:     "openai",
			Model:        "openai/gpt-test",
			InputTokens:  8,
			OutputTokens: 4,
			StopReason:   llm.StopEndTurn,
		},
		FallbackModels: []string{"openai/gpt-backup"},
		ReferencedFiles: []FileReference{{
			Path:      "README.md",
			Kind:      "file",
			Source:    "context_reference",
			SHA256:    "abc123",
			SizeBytes: 512,
		}},
	})
	require.True(t, sessionState.RecordProviderCall(call))
	require.NoError(t, store.Save(sessionState))

	data := readSessionTestFile(t, store.EventLogPath(sessionState.ID))
	assert.Contains(t, data, string(EventProviderCallRecorded))
	assert.Contains(t, data, `"request_messages"`)
	assert.Contains(t, data, string(EventToolCallRecorded))
	assert.Contains(t, data, string(EventToolResultRecorded))
	assert.Contains(t, data, string(EventFileReferenceRecorded))

	require.NoError(t, os.Remove(store.Path(sessionState.ID)))
	loaded, err := store.Load(sessionState.ID)
	require.NoError(t, err)
	require.Len(t, loaded.ProviderCalls, 1)
	assert.Equal(t, call.PromptHash, loaded.ProviderCalls[0].PromptHash)
	assert.Equal(t, "openai", loaded.ProviderCalls[0].Provider)
	assert.Equal(t, 12, loaded.ProviderCalls[0].TotalTokens)
	require.Len(t, loaded.ProviderCalls[0].RequestMessages, 4)

	export := BuildMachineReadableExport(loaded, ExportOptions{Profile: ExportProfilePrivate})
	assert.Contains(t, export.Provenance.Providers, "openai")
	assert.Contains(t, export.Provenance.Models, "openai/gpt-test")
	assert.Contains(t, export.Provenance.Models, "openai/gpt-backup")
	assert.Equal(t, 1, export.Provenance.TokenUsage.ModelCalls)
	assert.Equal(t, 12, export.Provenance.TokenUsage.TotalTokens)
	require.Len(t, export.Provenance.ProviderCalls, 1)
	exportedCall := export.Provenance.ProviderCalls[0]
	assert.Equal(t, call.PromptHash, exportedCall.PromptHash)
	assert.Equal(t, call.ConfigHash, exportedCall.ConfigHash)
	assert.Equal(t, "openai/gpt-test", exportedCall.RequestedModel)
	assert.Equal(t, "openai/gpt-test", exportedCall.ResponseModel)
	require.Len(t, exportedCall.FallbackModels, 1)
	assert.Equal(t, "openai/gpt-backup", exportedCall.FallbackModels[0])
	assert.Equal(t, 12, exportedCall.TotalTokens)
	assert.Equal(t, 1, exportedCall.RequestToolCallCount)
	assert.Equal(t, 1, exportedCall.RequestToolResultCount)
	require.Len(t, exportedCall.RequestMessages, 4)
	require.NotNil(t, exportedCall.RequestMessages[3].ToolResult)
	assert.Equal(t, "README contents", exportedCall.RequestMessages[3].ToolResult.Content)
	require.Len(t, exportedCall.ReferencedFiles, 1)
	assert.Equal(t, "README.md", exportedCall.ReferencedFiles[0].Path)

	shareableExport := BuildMachineReadableExport(loaded, ExportOptions{Profile: ExportProfileShareable})
	require.Len(t, shareableExport.Provenance.ProviderCalls, 1)
	assert.Empty(t, shareableExport.Provenance.ProviderCalls[0].RequestMessages)
	assert.Equal(t, call.PromptHash, shareableExport.Provenance.ProviderCalls[0].PromptHash)

	markdown := MarkdownWithOptions(loaded, ExportOptions{Profile: ExportProfilePrivate, ExportedAt: fixedExportedAt})
	assert.Contains(t, markdown, "- **Provider calls:**")
	assert.Contains(t, markdown, "prompt_hash="+call.PromptHash)
	assert.Contains(t, markdown, "config_hash="+call.ConfigHash)

	var referencedPaths []string
	for _, file := range export.Provenance.ReferencedFiles {
		referencedPaths = append(referencedPaths, file.Path)
	}

	assert.Contains(t, referencedPaths, "README.md")
}

func TestStore_ReplayFromEventLogIsDeterministic(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	sessionState := New("openai/gpt-test", []llm.Message{
		{Role: llm.RoleUser, Content: "summarize @README.md"},
		{Role: llm.RoleAssistant, Content: "summary"},
	})
	sessionState.ID = "deterministic"
	sessionState.WorktreePath = eventLogTestWorktreePath
	sessionState.WorktreeBranch = "feature/audit-log"
	sessionState.WorktreeBase = "main"
	require.True(t, sessionState.RecordProviderCall(NewProviderCall(ProviderCallRecord{
		CompletedAt: time.Date(2026, 5, 1, 14, 0, 0, 0, time.UTC),
		Source:      "run_once",
		Params: llm.CompleteParams{
			Model:          "openai/gpt-test",
			ModelMode:      "fast",
			ReasoningLevel: "high",
			Messages: []llm.Message{
				{Role: llm.RoleSystem, Content: "reference context"},
				{Role: llm.RoleUser, Content: "summarize README"},
				{
					Role:    llm.RoleAssistant,
					Content: "reading file",
					ToolCalls: []llm.ToolCall{{
						ID:    "tool-1",
						Name:  "read_file",
						Input: map[string]any{"path": "README.md"},
					}},
				},
				{
					Role:    llm.RoleTool,
					Content: "README contents",
					ToolResult: &llm.ToolResult{
						ToolCallID: "tool-1",
						Content:    "README contents",
					},
				},
			},
			MaxTokens: 256,
		},
		Response: &llm.Response{
			Content:      "summary",
			Provider:     "openai",
			Model:        "openai/gpt-test",
			InputTokens:  8,
			OutputTokens: 4,
			StopReason:   llm.StopEndTurn,
		},
		FallbackModels: []string{"openai/gpt-backup"},
		ReferencedFiles: []FileReference{{
			Path:      "README.md",
			Kind:      "file",
			Source:    "context_reference",
			SHA256:    "abc123",
			SizeBytes: 512,
		}},
	})))

	run := NewMultiAgentRun(MultiAgentRunKindReview, "audit", "openai/gpt-test", nil, MultiAgentRunBudget{})
	run.ID = "run-1"
	run.Status = MultiAgentRunStatusCompleted
	run.Usage = MultiAgentRunUsage{ModelCalls: 1, InputTokens: 10, OutputTokens: 5, TotalTokens: 15}
	run.Gates = []MultiAgentRunGate{{Name: "tests", Phase: "verify", Agent: "verifier", Passed: true}}
	require.True(t, sessionState.UpsertMultiAgentRun(run))

	require.NoError(t, store.Save(sessionState))

	projection, err := readLegacyJSONSession(store.Path(sessionState.ID))
	require.NoError(t, err)
	firstReplay, err := store.Load(sessionState.ID)
	require.NoError(t, err)
	require.NoError(t, os.Remove(store.Path(sessionState.ID)))
	secondReplay, err := store.Load(sessionState.ID)
	require.NoError(t, err)

	options := ExportOptions{Profile: ExportProfilePrivate, ExportedAt: fixedExportedAt}
	assert.Equal(t, stableSessionExportJSON(t, projection, options), stableSessionExportJSON(t, firstReplay, options))
	assert.Equal(t, stableSessionExportJSON(t, firstReplay, options), stableSessionExportJSON(t, secondReplay, options))
	require.NotNil(t, firstReplay.EventLog)
	require.NotNil(t, secondReplay.EventLog)
	assert.Equal(t, firstReplay.EventLog.LastHash, secondReplay.EventLog.LastHash)
	require.Len(t, firstReplay.ProviderCalls, 1)
	require.Len(t, secondReplay.ProviderCalls, 1)
	assert.Equal(t, firstReplay.ProviderCalls[0].RequestMessages, secondReplay.ProviderCalls[0].RequestMessages)
}

func TestStore_LoadEventLogIgnoresTrailingPartialLine(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	sessionState := New("gpt-test", []llm.Message{{Role: llm.RoleUser, Content: "safe prefix"}})
	require.NoError(t, store.Save(sessionState))
	require.NoError(t, os.Remove(store.Path(sessionState.ID)))

	file, err := os.OpenFile(store.EventLogPath(sessionState.ID), os.O_WRONLY|os.O_APPEND, 0)
	require.NoError(t, err)
	_, err = file.WriteString(`{"schema_version":1,"sequence":`)
	require.NoError(t, err)
	require.NoError(t, file.Close())

	loaded, err := store.Load(sessionState.ID)
	require.NoError(t, err)
	require.NotNil(t, loaded.EventLog)
	assert.True(t, loaded.EventLog.TruncatedTail)
	require.Len(t, loaded.Messages, 1)
	assert.Equal(t, "safe prefix", loaded.Messages[0].Content)
}

func TestStore_LoadEventLogRejectsCompleteCorruptLine(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	sessionState := New("gpt-test", []llm.Message{{Role: llm.RoleUser, Content: "safe prefix"}})
	require.NoError(t, store.Save(sessionState))

	file, err := os.OpenFile(store.EventLogPath(sessionState.ID), os.O_WRONLY|os.O_APPEND, 0)
	require.NoError(t, err)
	_, err = file.WriteString("{not-json}\n")
	require.NoError(t, err)
	require.NoError(t, file.Close())

	_, err = store.Load(sessionState.ID)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrCorruptEventLog)
}

func TestStore_LoadEventLogRejectsTamperedHashChain(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	sessionState := New("gpt-test", []llm.Message{{Role: llm.RoleUser, Content: "safe prefix"}})
	require.NoError(t, store.Save(sessionState))

	lines := strings.Split(strings.TrimSpace(readSessionTestFile(t, store.EventLogPath(sessionState.ID))), "\n")
	require.NotEmpty(t, lines)

	var event Event
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &event))
	event.Payload = json.RawMessage(`{"id":"` + sessionState.ID + `","title":"tampered","schema_version":2}`)

	tampered, err := json.Marshal(event)
	require.NoError(t, err)

	lines[0] = string(tampered)

	require.NoError(t, os.WriteFile(store.EventLogPath(sessionState.ID), []byte(strings.Join(lines, "\n")+"\n"), 0o600))

	_, err = store.Load(sessionState.ID)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrCorruptEventLog)
}

func TestStore_LoadEventLogRejectsValidJSONCorruptFinalLineWithoutNewline(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	sessionState := New("gpt-test", []llm.Message{{Role: llm.RoleUser, Content: "safe prefix"}})
	require.NoError(t, store.Save(sessionState))

	lines := strings.Split(strings.TrimSpace(readSessionTestFile(t, store.EventLogPath(sessionState.ID))), "\n")
	require.NotEmpty(t, lines)

	var event Event
	require.NoError(t, json.Unmarshal([]byte(lines[len(lines)-1]), &event))
	event.Payload = json.RawMessage(`{"index":999,"message":{"role":"user","content":"tampered final event"}}`)

	tampered, err := json.Marshal(event)
	require.NoError(t, err)

	lines[len(lines)-1] = string(tampered)

	require.NoError(t, os.WriteFile(store.EventLogPath(sessionState.ID), []byte(strings.Join(lines, "\n")), 0o600))

	_, err = store.Load(sessionState.ID)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrCorruptEventLog)
}

func TestStore_LoadEventLogRejectsMixedSessionIDs(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	sessionState := New("gpt-test", []llm.Message{{Role: llm.RoleUser, Content: "safe prefix"}})
	require.NoError(t, store.Save(sessionState))

	events := readEventLogTestEvents(t, store.EventLogPath(sessionState.ID))
	require.GreaterOrEqual(t, len(events), 2)

	events[len(events)-1].SessionID = "other-session"
	rewriteEventLogTestEvents(t, store.EventLogPath(sessionState.ID), events)

	_, err := store.Load(sessionState.ID)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrCorruptEventLog)
	assert.Contains(t, err.Error(), "session_id")
}

func TestStore_SaveRepairsValidEventLogMissingTrailingNewlineBeforeAppend(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	sessionState := New("gpt-test", []llm.Message{{Role: llm.RoleUser, Content: "safe prefix"}})
	require.NoError(t, store.Save(sessionState))

	eventPath := store.EventLogPath(sessionState.ID)
	data := strings.TrimRight(readSessionTestFile(t, eventPath), "\n")
	require.NoError(t, os.WriteFile(eventPath, []byte(data), 0o600))

	loaded, err := store.Load(sessionState.ID)
	require.NoError(t, err)
	loaded.Append(llm.RoleAssistant, "safe append")
	require.NoError(t, store.Save(loaded))

	replayed, err := store.Load(sessionState.ID)
	require.NoError(t, err)

	var contents []string
	for _, message := range replayed.Messages {
		contents = append(contents, message.Content)
	}

	assert.Contains(t, contents, "safe prefix")
	assert.Contains(t, contents, "safe append")
}

func TestStore_SearchIndexesEventOnlySession(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	sessionState := New("gpt-test", []llm.Message{{Role: llm.RoleUser, Content: "event only searchable needle"}})
	require.NoError(t, store.Save(sessionState))
	require.NoError(t, os.Remove(store.Path(sessionState.ID)))

	results, err := store.Search("needle")
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, sessionState.ID, results[0].Summary.ID)
	assert.Equal(t, store.EventLogPath(sessionState.ID), results[0].Summary.Path)
}

func TestBuildMachineReadableExport_IncludesReplayProvenance(t *testing.T) {
	t.Parallel()

	sessionState := New("openai/gpt-test", nil)
	sessionState.EventLog = &EventLogMetadata{
		Path:          "session.events.jsonl",
		LastHash:      "sha256:last",
		SchemaVersion: SessionEventSchemaVersion,
		EventCount:    4,
		LastSequence:  4,
	}
	sessionState.WorktreePath = eventLogTestWorktreePath
	require.True(t, sessionState.AddArtifact(Artifact{
		Path:      "reports/replay.md",
		Kind:      "report",
		SHA256:    "abcdef",
		CreatedAt: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
	}))
	run := MultiAgentRun{
		ID:     "run-1",
		Kind:   MultiAgentRunKindReview,
		Status: MultiAgentRunStatusCompleted,
		Model:  "openai/gpt-test",
		Usage:  MultiAgentRunUsage{ModelCalls: 1, InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
		Calls: []MultiAgentRunCall{{
			ID:             "call-1",
			Phase:          "review",
			Status:         MultiAgentRunStatusCompleted,
			RequestedModel: "openai/gpt-test",
			ResponseModel:  "openai/gpt-test",
			InputTokens:    10,
			OutputTokens:   5,
			TotalTokens:    15,
		}},
		Gates: []MultiAgentRunGate{{Name: "tests", Phase: "verify", Agent: "verifier", Passed: true}},
	}
	require.True(t, sessionState.UpsertMultiAgentRun(run))

	privateOptions := ExportOptions{Profile: ExportProfilePrivate, ExportedAt: fixedExportedAt}
	export := BuildMachineReadableExport(sessionState, privateOptions)
	assert.Equal(t, SessionSchemaVersion, export.Session.SchemaVersion)
	assert.Equal(t, SessionEventSchemaVersion, export.Session.EventSchemaVersion)
	assert.Equal(t, "sha256:last", export.Provenance.EventLog.LastHash)
	assert.NotEmpty(t, export.Provenance.ConfigHash)
	assert.Contains(t, export.Provenance.Providers, "openai")
	assert.Contains(t, export.Provenance.Models, "openai/gpt-test")
	assert.Equal(t, 1, export.Provenance.TokenUsage.ModelCalls)
	assert.Equal(t, 15, export.Provenance.TokenUsage.TotalTokens)
	require.NotEmpty(t, export.Provenance.ReferencedFiles)

	var referencedPaths []string
	for _, file := range export.Provenance.ReferencedFiles {
		referencedPaths = append(referencedPaths, file.Path)
	}

	assert.Contains(t, referencedPaths, "reports/replay.md")
	require.Len(t, export.Provenance.VerificationGates, 1)
	assert.Equal(t, "tests", export.Provenance.VerificationGates[0].Name)

	markdown := MarkdownWithOptions(sessionState, privateOptions)
	assert.Contains(t, markdown, "## Provenance")
	assert.Contains(t, markdown, "- **Config hash:** sha256:")
	assert.Contains(t, markdown, "- **Event log hash:** sha256:last")
	assert.Contains(t, markdown, "reports/replay.md")
	assert.Contains(t, markdown, "tests status=pass")
}

func readSessionTestFile(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path)
	require.NoError(t, err)

	return string(data)
}

func stableSessionExportJSON(t *testing.T, sessionState Session, options ExportOptions) string {
	t.Helper()

	data, err := JSONWithOptions(sessionState, options)
	require.NoError(t, err)

	return string(data)
}

func readEventLogTestEvents(t *testing.T, path string) []Event {
	t.Helper()

	lines := strings.Split(strings.TrimSpace(readSessionTestFile(t, path)), "\n")
	events := make([]Event, 0, len(lines))

	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}

		var event Event
		require.NoError(t, json.Unmarshal([]byte(line), &event))

		events = append(events, event)
	}

	return events
}

func rewriteEventLogTestEvents(t *testing.T, path string, events []Event) {
	t.Helper()

	lines := make([]string, 0, len(events))
	prevHash := ""

	for index := range events {
		events[index].Sequence = int64(index + 1)
		events[index].PrevHash = prevHash
		events[index].Hash = ""

		hash, err := eventDigest(events[index])
		require.NoError(t, err)

		events[index].Hash = hash
		prevHash = hash

		data, err := json.Marshal(events[index])
		require.NoError(t, err)

		lines = append(lines, string(data))
	}

	require.NoError(t, os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600))
}
