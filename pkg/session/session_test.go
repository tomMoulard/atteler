package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/llm"
)

func TestStore_SaveLoadByID(t *testing.T) {
	t.Parallel()
	store := NewStore(t.TempDir())
	session := New("gpt-4.1", nil)
	session.DefaultReasoningLevel = "high"
	session.AgentLoopBudget = llm.AgentLoopBudget{
		MaxWallTime:     time.Minute,
		MaxOutputBytes:  4096,
		MaxCostMicros:   25_000,
		MaxIterations:   3,
		MaxModelCalls:   4,
		MaxToolCalls:    5,
		MaxInputTokens:  100,
		MaxOutputTokens: 50,
		MaxTotalTokens:  150,
	}
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

	if loaded.DefaultReasoningLevel != "high" {
		assert.Failf(t, "assertion failed", "DefaultReasoningLevel = %q", loaded.DefaultReasoningLevel)
	}

	assert.Equal(t, session.AgentLoopBudget, loaded.AgentLoopBudget)

	if len(loaded.Messages) != 2 {
		require.Failf(t, "unexpected failure", "messages len = %d, want 2", len(loaded.Messages))
	}

	if loaded.Messages[0].Role != llm.RoleUser || loaded.Messages[0].Content != "hello" {
		assert.Failf(t, "assertion failed", "first message = %+v", loaded.Messages[0])
	}
}

func TestHeadlessEventForRunIncludesAgentLoopBudget(t *testing.T) {
	t.Parallel()

	budget := llm.AgentLoopBudget{
		MaxWallTime:     time.Minute,
		MaxOutputBytes:  4096,
		MaxCostMicros:   25_000,
		MaxIterations:   3,
		MaxModelCalls:   4,
		MaxToolCalls:    5,
		MaxInputTokens:  100,
		MaxOutputTokens: 50,
		MaxTotalTokens:  150,
	}

	event := headlessEventForRun(HeadlessRun{
		ID:              "run-budget",
		SessionID:       "session-budget",
		Status:          HeadlessStatusCanceled,
		AgentLoopBudget: budget,
	}, HeadlessEventCanceled, "canceled")

	assert.Equal(t, budget, event.AgentLoopBudget)
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

func TestSession_RecordBackgroundSuggestionUsage(t *testing.T) {
	t.Parallel()

	var session Session
	session.RecordBackgroundSuggestionUsage(BackgroundSuggestionRecord{
		Provider:              "openai",
		Model:                 "gpt-5.4-mini",
		Status:                "ready:model",
		ContextSummary:        "agent=1,file/task/issue=omitted-private",
		EstimatedCostUSD:      0.0002,
		ProviderCall:          true,
		Response:              true,
		InputTokens:           12,
		CachedInputTokens:     3,
		CacheWriteInputTokens: 1,
		OutputTokens:          4,
		EstimatedInputTokens:  20,
		EstimatedOutputTokens: 8,
	})

	require.NotNil(t, session.BackgroundSuggestions)
	assert.Equal(t, 1, session.BackgroundSuggestions.Requests)
	assert.Equal(t, 1, session.BackgroundSuggestions.ProviderCalls)
	assert.Equal(t, 1, session.BackgroundSuggestions.Responses)
	assert.Equal(t, "ready:model", session.BackgroundSuggestions.LastStatus)
	assert.Equal(t, "agent=1,file/task/issue=omitted-private", session.BackgroundSuggestions.LastContextSummary)
	assert.Equal(t, 12, session.BackgroundSuggestions.InputTokens)
	assert.Equal(t, 4, session.BackgroundSuggestions.OutputTokens)
	assert.Equal(t, 20, session.BackgroundSuggestions.EstimatedInputTokens)
	assert.InDelta(t, 0.0002, session.BackgroundSuggestions.EstimatedCostUSD, 0.0000001)
}

func TestStore_SaveLoadPromptSuggestionMetadata(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	session := New("gpt-test", nil)
	session.PromptSuggestions = "model-backed"
	session.RecordBackgroundSuggestionUsage(BackgroundSuggestionRecord{
		Provider:              "openai",
		Model:                 "gpt-5.4-mini",
		Status:                "ready:model",
		ContextSummary:        "slash=1,file/task/issue=omitted-private",
		EstimatedCostUSD:      0.0001,
		ProviderCall:          true,
		Response:              true,
		InputTokens:           7,
		OutputTokens:          2,
		EstimatedInputTokens:  12,
		EstimatedOutputTokens: 4,
	})

	require.NoError(t, store.Save(session))

	loaded, err := store.Load(session.ID)
	require.NoError(t, err)
	assert.Equal(t, "model-backed", loaded.PromptSuggestions)
	require.NotNil(t, loaded.BackgroundSuggestions)
	assert.Equal(t, 1, loaded.BackgroundSuggestions.Requests)
	assert.Equal(t, 1, loaded.BackgroundSuggestions.ProviderCalls)
	assert.Equal(t, "ready:model", loaded.BackgroundSuggestions.LastStatus)
	assert.Equal(t, "slash=1,file/task/issue=omitted-private", loaded.BackgroundSuggestions.LastContextSummary)
	assert.Equal(t, 7, loaded.BackgroundSuggestions.InputTokens)
	assert.Equal(t, 2, loaded.BackgroundSuggestions.OutputTokens)
	assert.Equal(t, 12, loaded.BackgroundSuggestions.EstimatedInputTokens)
	assert.Equal(t, 4, loaded.BackgroundSuggestions.EstimatedOutputTokens)
	assert.InDelta(t, 0.0001, loaded.BackgroundSuggestions.EstimatedCostUSD, 0.0000001)
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
	first.WorktreePath = "/repo/auth"
	first.AgentLoopBudget = llm.AgentLoopBudget{MaxInputTokens: 100, MaxOutputTokens: 50, MaxCostMicros: 25_000}

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

	if summaries[1].WorktreePath != first.WorktreePath && summaries[0].WorktreePath != first.WorktreePath {
		require.Failf(t, "unexpected failure", "worktree path not found in summaries: %+v", summaries)
	}

	if summaries[1].AgentLoopBudget != first.AgentLoopBudget && summaries[0].AgentLoopBudget != first.AgentLoopBudget {
		require.Failf(t, "unexpected failure", "agent loop budget not found in summaries: %+v", summaries)
	}

	if len(summaries[1].Tags) == 0 && len(summaries[0].Tags) == 0 {
		require.Failf(t, "unexpected failure", "tags not found in summaries: %+v", summaries)
	}
}

func TestStore_ListDoesNotDecodeMessageContent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.json")
	data := `{
  "id": "legacy",
  "title": "Legacy transcript",
  "default_agent": "reviewer",
  "worktree_path": "/repo/auth",
  "negative_knowledge": [123, {"agent": 123, "approach": 123}, {"agent": "critic", "approach": 123}],
  "evaluations": [{"agent": ["bad"], "notes": 123}, {"agent": "reviewer", "notes": 123}],
  "artifacts": [{"source_agent": false, "summary": 123}, {"source_agent": "writer", "summary": 123}],
  "messages": [123, {"role": "user", "content": 123}]
}`
	require.NoError(t, os.WriteFile(path, []byte(data), 0o600))

	summaries, err := NewStore(dir).List()
	require.NoError(t, err)
	require.Len(t, summaries, 1)
	assert.Equal(t, "legacy", summaries[0].ID)
	assert.Equal(t, "/repo/auth", summaries[0].WorktreePath)
	assert.ElementsMatch(t, []string{"critic", "reviewer", "writer"}, summaries[0].AgentNames)
	assert.Equal(t, 2, summaries[0].Messages)

	_, err = NewStore(dir).Load("legacy")
	require.Error(t, err)
}

func TestStore_ListRejectsTrailingSessionJSON(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "trailing.json")
	data := `{"id":"trailing","messages":[]}{"id":"extra"}`
	require.NoError(t, os.WriteFile(path, []byte(data), 0o600))

	_, err := NewStore(dir).List()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "trailing data")
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

func TestStore_ListByTagFiltersExactNormalizedTag(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	first := New("gpt-4.1", nil)
	first.Title = "Auth one"
	first.Tags = []string{"auth", "review"}
	require.NoError(t, store.Save(first))

	second := New("gpt-4.1", nil)
	second.Title = "Auth two"
	second.Tags = []string{" Auth "}
	require.NoError(t, store.Save(second))

	third := New("gpt-4.1", nil)
	third.Title = "Docs"
	third.Tags = []string{"docs", "authoring"}
	require.NoError(t, store.Save(third))

	summaries, err := store.ListByTag(" AUTH ")
	require.NoError(t, err)
	require.Len(t, summaries, 2)
	assert.ElementsMatch(t, []string{"Auth one", "Auth two"}, []string{summaries[0].Title, summaries[1].Title})

	docs, err := store.ListByTag("docs")
	require.NoError(t, err)
	require.Len(t, docs, 1)
	assert.Equal(t, "Docs", docs[0].Title)
}

func TestStore_ListByTagRejectsEmptyTag(t *testing.T) {
	t.Parallel()

	_, err := NewStore(t.TempDir()).ListByTag(" \t ")
	require.Error(t, err)
	assert.ErrorContains(t, err, "tag is required")
}

func TestSession_RecordNegativeKnowledgeDeduplicatesNormalizedApproachAndReason(t *testing.T) {
	t.Parallel()

	session := New("gpt-4.1", nil)
	if ok := session.RecordNegativeKnowledge(" Try cache bust ", " Broke auth flow ", " abc123 ", " reviewer "); !ok {
		require.FailNow(t, "expected first negative knowledge entry to be recorded")
	}

	if ok := session.RecordNegativeKnowledge("try   CACHE bust", "broke   auth flow", "def456", "writer"); ok {
		require.FailNow(t, "expected duplicate negative knowledge entry to be skipped")
	}

	require.Len(t, session.NegativeKnowledge, 1)
	entry := session.NegativeKnowledge[0]
	assert.Equal(t, "Try cache bust", entry.Approach)
	assert.Equal(t, "Broke auth flow", entry.Reason)
	assert.Equal(t, "abc123", entry.Commit)
	assert.Equal(t, "reviewer", entry.Agent)
	assert.False(t, entry.CreatedAt.IsZero())
}

func TestSession_RecordNegativeKnowledgeDetailsCategorizesIncident(t *testing.T) {
	t.Parallel()

	session := New("gpt-4.1", nil)
	ok := session.RecordNegativeKnowledgeDetails(NegativeKnowledge{
		Approach: " rewrite router ",
		Reason:   " hid regression ",
		Agent:    " reviewer ",
		TaskType: " migration ",
		Severity: " high ",
	})
	require.True(t, ok)

	require.Len(t, session.NegativeKnowledge, 1)
	entry := session.NegativeKnowledge[0]
	assert.Equal(t, "migration", entry.TaskType)
	assert.Equal(t, "high", entry.Severity)
}

func TestSession_RecordNegativeKnowledgeRejectsMissingApproachOrReason(t *testing.T) {
	t.Parallel()

	session := New("gpt-4.1", nil)
	assert.False(t, session.RecordNegativeKnowledge("", "reason", "", ""))
	assert.False(t, session.RecordNegativeKnowledge("approach", " ", "", ""))
	assert.Empty(t, session.NegativeKnowledge)
}

func TestSession_RecordEvaluationAndArtifact(t *testing.T) {
	t.Parallel()

	session := New("gpt-4.1", nil)
	assert.True(t, session.RecordEvaluation(" reviewer ", " pass ", " solid ", " ref.md ", 95))
	assert.False(t, session.RecordEvaluation("", "pass", "", "", 0))
	assert.True(t, session.RecordArtifact("./plans/../plan.md", " research ", " useful ", " reviewer "))
	assert.False(t, session.RecordArtifact("", "research", "", ""))

	require.Len(t, session.Evaluations, 1)
	assert.Equal(t, "reviewer", session.Evaluations[0].Agent)
	assert.Equal(t, "pass", session.Evaluations[0].Outcome)
	assert.Equal(t, 95, session.Evaluations[0].Score)
	assert.Equal(t, EvaluationSourceHuman, session.Evaluations[0].Source)
	assert.Equal(t, RubricVersionLegacy, session.Evaluations[0].RubricVersion)
	assert.Equal(t, AgentEvaluationSchemaVersion, session.Evaluations[0].SchemaVersion)
	assert.False(t, session.Evaluations[0].CreatedAt.IsZero())

	require.Len(t, session.Artifacts, 1)
	assert.Equal(t, "plan.md", session.Artifacts[0].Path)
	assert.Equal(t, "plan.md", session.Artifacts[0].LogicalPath)
	assert.Equal(t, "research", session.Artifacts[0].Kind)
	assert.Equal(t, "reviewer", session.Artifacts[0].SourceAgent)
	assert.Equal(t, session.ID, session.Artifacts[0].SourceSessionID)
	assert.False(t, session.Artifacts[0].CreatedAt.IsZero())
}

func TestSession_RecordEvaluationDetailsStoresVersionedMetadata(t *testing.T) {
	t.Parallel()

	session := New("gpt-4.1", nil)
	ok := session.RecordEvaluationDetails(AgentEvaluation{
		Agent:           " reviewer ",
		Outcome:         " pass ",
		Source:          EvaluationSourceHarness,
		Evaluator:       " eval-bot ",
		RubricVersion:   "review-rubric/v2",
		TaskType:        "code-review",
		Difficulty:      "medium",
		ExpectedOutcome: "find regression",
		Model:           "gpt-test",
		AgentVersion:    "reviewer@abc123",
		Score:           88,
		DurationMillis:  1200,
		Cost:            0.0123,
		Confidence:      0.91,
	})
	require.True(t, ok)

	require.Len(t, session.Evaluations, 1)
	entry := session.Evaluations[0]
	assert.Equal(t, EvaluationSourceHarness, entry.Source)
	assert.Equal(t, "eval-bot", entry.Evaluator)
	assert.Equal(t, "review-rubric/v2", entry.RubricVersion)
	assert.Equal(t, "code-review", entry.TaskType)
	assert.Equal(t, "medium", entry.Difficulty)
	assert.Equal(t, "find regression", entry.ExpectedOutcome)
	assert.Equal(t, "gpt-test", entry.Model)
	assert.Equal(t, "reviewer@abc123", entry.AgentVersion)
	assert.Equal(t, AgentEvaluationSchemaVersion, entry.SchemaVersion)
	assert.Equal(t, int64(1200), entry.DurationMillis)
	assert.InEpsilon(t, 0.0123, entry.Cost, 0.0001)
	assert.InEpsilon(t, 0.91, entry.Confidence, 0.0001)
}

func TestSession_RecordEvaluationDetailsRejectsInvalidCalibration(t *testing.T) {
	t.Parallel()

	session := New("gpt-4.1", nil)
	assert.False(t, session.RecordEvaluationDetails(AgentEvaluation{
		Agent:      "reviewer",
		Outcome:    "pass",
		Confidence: 1.1,
	}))
	assert.False(t, session.RecordEvaluationDetails(AgentEvaluation{
		Agent:   "reviewer",
		Outcome: "pass",
		Score:   -1,
	}))
	assert.False(t, session.RecordEvaluationDetails(AgentEvaluation{
		Agent:          "reviewer",
		Outcome:        "pass",
		DurationMillis: -1,
	}))
	assert.False(t, session.RecordEvaluationDetails(AgentEvaluation{
		Agent:   "reviewer",
		Outcome: "pass",
		Cost:    -0.01,
	}))
	assert.False(t, session.RecordEvaluationDetails(AgentEvaluation{
		Agent:         "reviewer",
		Outcome:       "pass",
		SchemaVersion: -1,
	}))
	assert.False(t, session.RecordEvaluationDetails(AgentEvaluation{
		Agent:         "reviewer",
		Outcome:       "pass",
		SchemaVersion: AgentEvaluationSchemaVersion + 1,
	}))
	assert.False(t, session.RecordEvaluationDetails(AgentEvaluation{
		Agent:   "reviewer",
		Outcome: "pass",
		Source:  "spreadsheet",
	}))
	assert.Empty(t, session.Evaluations)
}

func TestSession_AddArtifactPreservesProvenance(t *testing.T) {
	t.Parallel()

	session := New("gpt-4.1", []llm.Message{{Role: llm.RoleUser, Content: "hello"}})
	artifact := Artifact{
		Path:           " ./reports/../report.md ",
		LogicalPath:    " decisions/auth.md ",
		Kind:           " report ",
		Summary:        " done ",
		SourceAgent:    " reviewer ",
		SourceCommand:  " record-artifact ",
		SourceTool:     " atteler ",
		SourceCommit:   " abc123 ",
		WorktreePath:   " /repo ",
		WorktreeBranch: " feature ",
		WorktreeBase:   " main ",
		SHA256:         " ABCDEF ",
		SizeBytes:      42,
		ReviewStatus:   " approved ",
	}

	require.True(t, session.AddArtifact(artifact))

	require.Len(t, session.Artifacts, 1)
	got := session.Artifacts[0]
	assert.Equal(t, "report.md", got.Path)
	assert.Equal(t, "decisions/auth.md", got.LogicalPath)
	assert.Equal(t, "report", got.Kind)
	assert.Equal(t, "done", got.Summary)
	assert.Equal(t, "reviewer", got.SourceAgent)
	assert.Equal(t, session.ID, got.SourceSessionID)
	assert.Equal(t, 1, got.SourceTurn)
	assert.Equal(t, "record-artifact", got.SourceCommand)
	assert.Equal(t, "atteler", got.SourceTool)
	assert.Equal(t, "abc123", got.SourceCommit)
	assert.Equal(t, "/repo", got.WorktreePath)
	assert.Equal(t, "feature", got.WorktreeBranch)
	assert.Equal(t, "main", got.WorktreeBase)
	assert.Equal(t, "abcdef", got.SHA256)
	assert.Equal(t, int64(42), got.SizeBytes)
	assert.Equal(t, "approved", got.ReviewStatus)
	assert.False(t, got.CreatedAt.IsZero())
}

func TestSession_MarkArtifactsConsumedPreservesFirstConsumption(t *testing.T) {
	t.Parallel()

	session := New("gpt-4.1", nil)
	require.True(t, session.RecordArtifact("./plans/../plan.md", "research", "useful", "reviewer"))
	require.True(t, session.RecordArtifact("notes.md", "research", "notes", "reviewer"))

	consumedAt := mustParseSessionTestTime(t, "2026-05-01T10:00:00Z")
	assert.Equal(t, 1, session.MarkArtifactsConsumed([]string{"plan.md"}, consumedAt))
	require.NotNil(t, session.Artifacts[0].ConsumedAt)
	assert.Equal(t, consumedAt, *session.Artifacts[0].ConsumedAt)
	assert.Nil(t, session.Artifacts[1].ConsumedAt)

	later := consumedAt.Add(time.Hour)
	assert.Equal(t, 0, session.MarkArtifactsConsumed([]string{"./plan.md"}, later))
	assert.Equal(t, consumedAt, *session.Artifacts[0].ConsumedAt)
}

func TestStore_LoadExistingJSONWithoutNegativeKnowledge(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	path := filepath.Join(dir, "legacy.json")
	if err := os.WriteFile(path, []byte(`{"messages":[{"role":"user","content":"hi"}]}`), 0o600); err != nil {
		require.NoError(t, err)
	}

	loaded, err := NewStore(dir).Load(path)
	require.NoError(t, err)

	assert.Equal(t, "legacy", loaded.ID)
	assert.Len(t, loaded.Messages, 1)
	assert.Empty(t, loaded.NegativeKnowledge)
	assert.Empty(t, loaded.Evaluations)
	assert.Empty(t, loaded.Artifacts)
}

func TestDefaultDir_EnvOverride(t *testing.T) {
	t.Setenv(EnvDir, "/tmp/custom-atteler-sessions")

	if got := DefaultDir(); got != "/tmp/custom-atteler-sessions" {
		assert.Failf(t, "assertion failed", "DefaultDir = %q", got)
	}
}

func mustParseSessionTestTime(t *testing.T, value string) time.Time {
	t.Helper()

	parsed, err := time.Parse(time.RFC3339, value)
	require.NoError(t, err)

	return parsed
}

func TestSession_UpsertAndFindMultiAgentRun(t *testing.T) {
	t.Parallel()

	session := New("gpt-4.1", nil)
	specRun := NewMultiAgentRun(MultiAgentRunKindSpeculation, "ship it", "gpt-4.1", []string{"backup"}, MultiAgentRunBudget{
		MaxModelCalls: 3,
	})
	assert.Equal(t, specRun.ID, specRun.ReceiptID)

	reviewRun := NewMultiAgentRun(MultiAgentRunKindReview, "audit it", "gpt-4.1", nil, MultiAgentRunBudget{})
	reviewRun.ID = "review-run"
	reviewRun.ReceiptID = "receipt-review"
	reviewRun.Status = MultiAgentRunStatusCompleted

	require.True(t, session.UpsertMultiAgentRun(specRun))
	require.True(t, session.UpsertMultiAgentRun(reviewRun))

	reviewRun.Status = MultiAgentRunStatusFailed
	require.True(t, session.UpsertMultiAgentRun(reviewRun))
	require.Len(t, session.MultiAgentRuns, 2)

	found, ok := session.FindMultiAgentRun("review-run")
	require.True(t, ok)
	assert.Equal(t, MultiAgentRunStatusFailed, found.Status)
	assert.Equal(t, "receipt-review", found.ReceiptID)

	byReceipt, ok := session.FindMultiAgentRun("receipt-review")
	require.True(t, ok)
	assert.Equal(t, "review-run", byReceipt.ID)

	latest, ok := session.FindMultiAgentRun("latest")
	require.True(t, ok)
	assert.Equal(t, "review-run", latest.ID)

	byKind, ok := session.FindMultiAgentRun(MultiAgentRunKindSpeculation)
	require.True(t, ok)
	assert.Equal(t, specRun.ID, byKind.ID)

	_, ok = session.FindMultiAgentRun("missing")
	assert.False(t, ok)
	assert.False(t, session.UpsertMultiAgentRun(MultiAgentRun{}))
}

func TestAcceptedMultiAgentRunArtifactPrefersIndexedDecisionMatch(t *testing.T) {
	t.Parallel()

	run := MultiAgentRun{
		Status: MultiAgentRunStatusCompleted,
		Artifacts: []MultiAgentRunArtifact{
			{
				Kind:    "verdict",
				Phase:   "aggregate-verdict",
				Agent:   "judge",
				Content: "legacy stale verdict",
			},
			{
				Kind:    "verdict",
				Phase:   "aggregate-verdict",
				Agent:   "judge",
				Content: "accepted verdict",
				Index:   2,
			},
		},
		Decisions: []MultiAgentRunDecision{{
			Kind:    "verdict",
			Phase:   "aggregate-verdict",
			Agent:   "judge",
			Outcome: "accepted",
			Index:   2,
		}},
	}

	artifact, ok := acceptedMultiAgentRunArtifact(run)
	require.True(t, ok)
	assert.Equal(t, "accepted verdict", artifact.Content)
	assert.True(t, multiAgentRunHasAcceptedOutput(run))
}
