package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStore_AgentPerformanceSummaryEmptyStore(t *testing.T) {
	t.Parallel()

	summaries, err := NewStore(t.TempDir()).AgentPerformanceSummary()
	require.NoError(t, err)
	assert.Empty(t, summaries)
}

func TestStore_AgentPerformanceSummaryGroupsByAgent(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	base := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	writeSessionSnapshot(t, store, Session{
		ID:           "one",
		CreatedAt:    base,
		UpdatedAt:    base.Add(time.Minute),
		DefaultAgent: "writer",
		Evaluations: []AgentEvaluation{
			{Agent: "writer", Outcome: "pass", Score: 80, CreatedAt: base.Add(2 * time.Minute)},
			{Agent: "reviewer", Outcome: "fail", CreatedAt: base.Add(3 * time.Minute)},
			{Agent: " ", Outcome: "ignored", Score: 100, CreatedAt: base.Add(4 * time.Minute)},
		},
		NegativeKnowledge: []NegativeKnowledge{
			{Agent: "writer", Approach: "cache", Reason: "stale", CreatedAt: base.Add(5 * time.Minute)},
			{Agent: "", Approach: "ignored", Reason: "blank agent", CreatedAt: base.Add(6 * time.Minute)},
		},
	})
	writeSessionSnapshot(t, store, Session{
		ID:           "two",
		CreatedAt:    base.Add(time.Hour),
		UpdatedAt:    base.Add(time.Hour + time.Minute),
		DefaultAgent: " Writer ",
		Evaluations: []AgentEvaluation{
			{Agent: " Writer ", Outcome: "pass", Score: 100, CreatedAt: base.Add(time.Hour + 2*time.Minute)},
			{Agent: "writer", Outcome: "partial", Score: 60, CreatedAt: base.Add(time.Hour + 3*time.Minute)},
		},
		NegativeKnowledge: []NegativeKnowledge{
			{Agent: "reviewer", Approach: "rewrite", Reason: "regressed", CreatedAt: base.Add(time.Hour + 4*time.Minute)},
		},
	})

	summaries, err := store.AgentPerformanceSummary()
	require.NoError(t, err)
	require.Len(t, summaries, 2)
	assert.Equal(t, []string{"reviewer", "writer"}, []string{summaries[0].Agent, summaries[1].Agent})

	reviewer := summaries[0]
	assert.Equal(t, 1, reviewer.EvaluationCount)
	assert.Equal(t, 1, reviewer.NegativeKnowledgeCount)
	assert.Equal(t, 1, reviewer.FailureCount)
	assert.Equal(t, 0, reviewer.ScoredEvaluationCount)
	assert.Equal(t, 0, reviewer.DefaultAgentSessionCount)
	assert.Equal(t, base.Add(time.Hour+4*time.Minute), reviewer.LatestActivity)
	assert.Equal(t, []OutcomeCount{{Outcome: "fail", Count: 1}}, reviewer.Outcomes)

	writer := summaries[1]
	assert.Equal(t, 3, writer.EvaluationCount)
	assert.Equal(t, 1, writer.NegativeKnowledgeCount)
	assert.Equal(t, 1, writer.FailureCount)
	assert.Equal(t, 2, writer.DefaultAgentSessionCount)
	assert.Equal(t, base.Add(time.Hour+3*time.Minute), writer.LatestActivity)
}

func TestStore_AgentPerformanceSummaryAggregatesScores(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	writeSessionSnapshot(t, store, Session{
		ID:        "scores",
		CreatedAt: base,
		UpdatedAt: base,
		Evaluations: []AgentEvaluation{
			{Agent: "tester", Outcome: "pass", Score: 95, CreatedAt: base.Add(time.Minute)},
			{Agent: "tester", Outcome: "fail", Score: 65, CreatedAt: base.Add(2 * time.Minute)},
			{Agent: "tester", Outcome: "skipped", Score: 0, CreatedAt: base.Add(3 * time.Minute)},
		},
	})

	summaries, err := store.AgentPerformanceSummary()
	require.NoError(t, err)
	require.Len(t, summaries, 1)

	summary := summaries[0]
	assert.Equal(t, 3, summary.EvaluationCount)
	assert.Equal(t, 2, summary.ScoredEvaluationCount)
	assert.InEpsilon(t, 80.0, summary.AverageScore, 0.0001)
	assert.Equal(t, 65, summary.MinScore)
	assert.Equal(t, 95, summary.MaxScore)
}

func TestStore_AgentPerformanceSummarySortsOutcomes(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	base := time.Date(2026, 5, 1, 13, 0, 0, 0, time.UTC)
	writeSessionSnapshot(t, store, Session{
		ID:        "outcomes",
		CreatedAt: base,
		UpdatedAt: base,
		Evaluations: []AgentEvaluation{
			{Agent: "agent", Outcome: "warn", CreatedAt: base.Add(time.Minute)},
			{Agent: "agent", Outcome: "pass", CreatedAt: base.Add(2 * time.Minute)},
			{Agent: "agent", Outcome: "fail", CreatedAt: base.Add(3 * time.Minute)},
			{Agent: "agent", Outcome: "pass", CreatedAt: base.Add(4 * time.Minute)},
			{Agent: "agent", Outcome: "Warn", CreatedAt: base.Add(5 * time.Minute)},
		},
	})

	summaries, err := store.AgentPerformanceSummary()
	require.NoError(t, err)
	require.Len(t, summaries, 1)
	assert.Equal(t, []OutcomeCount{
		{Outcome: "pass", Count: 2},
		{Outcome: "warn", Count: 2},
		{Outcome: "fail", Count: 1},
	}, summaries[0].Outcomes)
}

func writeSessionSnapshot(t *testing.T, store *Store, session Session) {
	t.Helper()
	require.NotEmpty(t, session.ID)
	require.NoError(t, os.MkdirAll(store.Dir(), 0o750))

	data, err := json.MarshalIndent(session, "", "  ")
	require.NoError(t, err)

	data = append(data, '\n')
	require.NoError(t, os.WriteFile(filepath.Join(store.Dir(), session.ID+sessionFileExt), data, 0o600))
}
