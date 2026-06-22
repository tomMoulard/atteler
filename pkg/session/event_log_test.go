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
	sessionState.WorktreePath = "/repo/atteler"
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
	sessionState.WorktreePath = "/repo/atteler"
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
