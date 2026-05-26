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
			{Agent: "writer", Approach: "cache", Reason: "stale", TaskType: "docs", Severity: "low", CreatedAt: base.Add(5 * time.Minute)},
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
			{Agent: "reviewer", Approach: "rewrite", Reason: "regressed", Severity: "high", CreatedAt: base.Add(time.Hour + 4*time.Minute)},
		},
	})

	summaries, err := store.agentPerformanceSummary(base.Add(2 * time.Hour))
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
	assert.Equal(t, []NegativeKnowledgeCategoryCount{{TaskType: unspecifiedTaskType, Severity: "high", Count: 1}}, reviewer.NegativeKnowledgeBreakdown)
	assert.False(t, reviewer.Validity.RoutingEligible)

	writer := summaries[1]
	assert.Equal(t, 3, writer.EvaluationCount)
	assert.Equal(t, 1, writer.NegativeKnowledgeCount)
	assert.Equal(t, 0, writer.FailureCount)
	assert.Equal(t, 2, writer.DefaultAgentSessionCount)
	assert.Equal(t, base.Add(time.Hour+3*time.Minute), writer.LatestActivity)
	assert.Equal(t, 3, writer.ScoredEvaluationCount)
	assert.Equal(t, []NegativeKnowledgeCategoryCount{{TaskType: "docs", Severity: "low", Count: 1}}, writer.NegativeKnowledgeBreakdown)
	require.Len(t, writer.ScoreBuckets, 1)
	assert.Equal(t, EvaluationSourceLegacy, writer.ScoreBuckets[0].Source)
	assert.Equal(t, RubricVersionLegacy, writer.ScoreBuckets[0].RubricVersion)
	assert.Equal(t, unspecifiedTaskType, writer.ScoreBuckets[0].TaskType)
	assert.Equal(t, "low_sample", writer.ScoreBuckets[0].Uncertainty)
}

func TestStore_AgentPerformanceSummarySeparatesIncompatibleRubrics(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	writeSessionSnapshot(t, store, Session{
		ID:           "scores",
		CreatedAt:    base,
		UpdatedAt:    base,
		DefaultModel: "gpt-test",
		Evaluations: []AgentEvaluation{
			{
				Agent:           "tester",
				Outcome:         "pass",
				Source:          EvaluationSourceHuman,
				Evaluator:       "alice",
				RubricVersion:   "rubric/v1",
				TaskType:        "code-review",
				Difficulty:      "medium",
				Model:           "gpt-a",
				AgentVersion:    "tester@1",
				Score:           90,
				Confidence:      0.8,
				SchemaVersion:   AgentEvaluationSchemaVersion,
				DurationMillis:  1000,
				Cost:            0.01,
				ExpectedOutcome: "catch regression",
				CreatedAt:       base.Add(time.Minute),
			},
			{
				Agent:         "tester",
				Outcome:       "pass",
				Source:        EvaluationSourceHuman,
				Evaluator:     "alice",
				RubricVersion: "rubric/v1",
				TaskType:      "code-review",
				Difficulty:    "medium",
				Model:         "gpt-a",
				AgentVersion:  "tester@1",
				Score:         70,
				Confidence:    0.9,
				SchemaVersion: AgentEvaluationSchemaVersion,
				CreatedAt:     base.Add(2 * time.Minute),
			},
			{
				Agent:         "tester",
				Outcome:       "pass",
				Source:        EvaluationSourceHarness,
				Evaluator:     "eval-bot",
				RubricVersion: "rubric/v2",
				TaskType:      "code-review",
				Difficulty:    "medium",
				Model:         "gpt-a",
				AgentVersion:  "tester@1",
				Score:         30,
				Confidence:    0.95,
				SchemaVersion: AgentEvaluationSchemaVersion,
				CreatedAt:     base.Add(3 * time.Minute),
			},
		},
	})

	summaries, err := store.agentPerformanceSummary(base.Add(time.Hour))
	require.NoError(t, err)
	require.Len(t, summaries, 1)

	summary := summaries[0]
	assert.Equal(t, 3, summary.EvaluationCount)
	assert.Equal(t, 3, summary.ScoredEvaluationCount)
	assert.Zero(t, summary.AverageScore, "top-level average must be withheld across incompatible rubrics/provenance")
	assert.Zero(t, summary.MinScore, "top-level min must be withheld across incompatible rubrics/provenance")
	assert.Zero(t, summary.MaxScore, "top-level max must be withheld across incompatible rubrics/provenance")
	assert.Equal(t, []ProvenanceCount{
		{Source: EvaluationSourceHuman, Count: 2},
		{Source: EvaluationSourceHarness, Count: 1},
	}, summary.EvaluationProvenance)
	assert.Equal(t, []RubricVersionCount{
		{RubricVersion: "rubric/v1", Count: 2},
		{RubricVersion: "rubric/v2", Count: 1},
	}, summary.RubricVersions)
	assert.Equal(t, []EvaluatorCount{
		{Evaluator: "alice", Count: 2},
		{Evaluator: "eval-bot", Count: 1},
	}, summary.Evaluators)
	require.Len(t, summary.ScoreBuckets, 2)
	humanBucket := findScoreBucket(t, summary.ScoreBuckets, EvaluationSourceHuman, "rubric/v1")
	assert.Equal(t, 2, humanBucket.SampleSize)
	assert.InEpsilon(t, 80.0, humanBucket.AverageScore, 0.0001)
	assert.InEpsilon(t, 0.85, humanBucket.AverageConfidence, 0.0001)
	assert.Equal(t, 1, humanBucket.DurationSampleCount)
	assert.InEpsilon(t, 1000.0, humanBucket.AverageDurationMillis, 0.0001)
	assert.Equal(t, 1, humanBucket.CostSampleCount)
	assert.InEpsilon(t, 0.01, humanBucket.TotalCost, 0.0001)
	assert.InEpsilon(t, 0.01, humanBucket.AverageCost, 0.0001)
	assert.Equal(t, "low_sample", humanBucket.Uncertainty)
	harnessBucket := findScoreBucket(t, summary.ScoreBuckets, EvaluationSourceHarness, "rubric/v2")
	assert.Equal(t, 1, harnessBucket.SampleSize)
	assert.False(t, summary.Validity.RoutingEligible)
	assert.Contains(t, summary.Validity.Reasons, "no compatible scored bucket has at least 10 samples")
}

func TestStore_AgentPerformanceSummarySeparatesTaskNormalizedBuckets(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	writeSessionSnapshot(t, store, Session{
		ID:           "task-normalized",
		CreatedAt:    base,
		UpdatedAt:    base,
		DefaultModel: "gpt-a",
		Evaluations: []AgentEvaluation{
			{
				Agent:         "tester",
				Outcome:       "pass",
				Source:        EvaluationSourceHuman,
				Evaluator:     "alice",
				RubricVersion: "rubric/v1",
				TaskType:      "migration",
				Difficulty:    "hard",
				Model:         "gpt-a",
				AgentVersion:  "tester@1",
				Score:         90,
				Confidence:    0.8,
				SchemaVersion: AgentEvaluationSchemaVersion,
				CreatedAt:     base.Add(time.Minute),
			},
			{
				Agent:         "tester",
				Outcome:       "pass",
				Source:        EvaluationSourceHuman,
				Evaluator:     "alice",
				RubricVersion: "rubric/v1",
				TaskType:      "copy-edit",
				Difficulty:    "easy",
				Model:         "gpt-a",
				AgentVersion:  "tester@1",
				Score:         50,
				Confidence:    0.8,
				SchemaVersion: AgentEvaluationSchemaVersion,
				CreatedAt:     base.Add(2 * time.Minute),
			},
		},
	})

	summaries, err := store.agentPerformanceSummary(base.Add(time.Hour))
	require.NoError(t, err)
	require.Len(t, summaries, 1)

	summary := summaries[0]
	assert.Equal(t, 2, summary.ScoredEvaluationCount)
	assert.Zero(t, summary.AverageScore, "scores from different task/difficulty cohorts must not be averaged together")
	require.Len(t, summary.ScoreBuckets, 2)

	bucketsByTask := make(map[string]ScoreBucketSummary, len(summary.ScoreBuckets))
	for _, bucket := range summary.ScoreBuckets {
		bucketsByTask[bucket.TaskType] = bucket
	}

	migrationBucket := bucketsByTask["migration"]
	assert.Equal(t, "hard", migrationBucket.Difficulty)
	assert.Equal(t, 1, migrationBucket.SampleSize)
	assert.InEpsilon(t, 90.0, migrationBucket.AverageScore, 0.0001)

	copyEditBucket := bucketsByTask["copy-edit"]
	assert.Equal(t, "easy", copyEditBucket.Difficulty)
	assert.Equal(t, 1, copyEditBucket.SampleSize)
	assert.InEpsilon(t, 50.0, copyEditBucket.AverageScore, 0.0001)
}

func TestStore_AgentPerformanceSummaryMarksRoutingEligibleWithValidEvidence(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	evaluations := make([]AgentEvaluation, 0, 12)

	for i := range 12 {
		createdAt := base.Add(time.Duration(i) * 24 * time.Hour)
		evaluations = append(evaluations, AgentEvaluation{
			Agent:         "planner",
			Outcome:       "pass",
			Source:        EvaluationSourceCI,
			Evaluator:     "ci-eval",
			RubricVersion: "planning/v3",
			TaskType:      "planning",
			Difficulty:    "medium",
			Model:         "gpt-plan",
			AgentVersion:  "planner@2",
			Score:         90,
			Confidence:    0.9,
			SchemaVersion: AgentEvaluationSchemaVersion,
			CreatedAt:     createdAt,
		})
	}

	writeSessionSnapshot(t, store, Session{
		ID:           "valid",
		CreatedAt:    base,
		UpdatedAt:    base.Add(11 * 24 * time.Hour),
		DefaultModel: "gpt-plan",
		Evaluations:  evaluations,
	})

	summaries, err := store.agentPerformanceSummary(base.Add(12 * 24 * time.Hour))
	require.NoError(t, err)
	require.Len(t, summaries, 1)
	summary := summaries[0]
	require.Len(t, summary.ScoreBuckets, 1)
	bucket := summary.ScoreBuckets[0]
	assert.Equal(t, 12, bucket.SampleSize)
	assert.Equal(t, 12, bucket.RecentSampleSize)
	assert.Equal(t, "bounded", bucket.Uncertainty)
	assert.Equal(t, "insufficient_history", bucket.RegressionStatus)
	assert.True(t, bucket.RoutingEligible)
	assert.Empty(t, bucket.ValidityReasons)
	assert.True(t, summary.Validity.RoutingEligible)
	assert.Equal(t, 1, summary.Validity.EligibleScoreBuckets)
	assert.Empty(t, summary.Validity.Reasons)
}

func TestStore_AgentPerformanceSummaryDoesNotTreatSummaryEligibilityAsBlanketBucketEligibility(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	evaluations := make([]AgentEvaluation, 0, 13)

	for i := range 12 {
		evaluations = append(evaluations, versionedScore("planner", 90, base.Add(time.Duration(i)*24*time.Hour)))
	}

	evaluations = append(evaluations, AgentEvaluation{
		Agent:     "planner",
		Outcome:   "pass",
		Score:     10,
		CreatedAt: base.Add(13 * 24 * time.Hour),
	})

	writeSessionSnapshot(t, store, Session{
		ID:          "mixed-validity",
		CreatedAt:   base,
		UpdatedAt:   base.Add(13 * 24 * time.Hour),
		Evaluations: evaluations,
	})

	summaries, err := store.agentPerformanceSummary(base.Add(14 * 24 * time.Hour))
	require.NoError(t, err)
	require.Len(t, summaries, 1)

	summary := summaries[0]
	assert.True(t, summary.Validity.RoutingEligible)
	assert.Equal(t, 1, summary.Validity.EligibleScoreBuckets)
	require.Len(t, summary.ScoreBuckets, 2)

	eligibleBucket := findScoreBucket(t, summary.ScoreBuckets, EvaluationSourceHarness, "rubric/v1")
	assert.True(t, eligibleBucket.RoutingEligible)
	assert.Empty(t, eligibleBucket.ValidityReasons)

	legacyBucket := findScoreBucket(t, summary.ScoreBuckets, EvaluationSourceLegacy, RubricVersionLegacy)
	assert.False(t, legacyBucket.RoutingEligible)
	assert.Contains(t, legacyBucket.ValidityReasons, "rubric version is legacy or missing")
	assert.Contains(t, legacyBucket.ValidityReasons, "sample size 1 is below required 10")

	eligibleBuckets := summary.RoutingEligibleScoreBuckets()
	require.Len(t, eligibleBuckets, 1)
	assert.Equal(t, eligibleBucket.sortKey(), eligibleBuckets[0].sortKey())
	assert.True(t, eligibleBuckets[0].RoutingEligible)
	assert.Empty(t, eligibleBuckets[0].ValidityReasons)
}

func TestStore_AgentPerformanceSummaryRequiresModelVersionMetadataForRouting(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	evaluations := make([]AgentEvaluation, 0, 12)

	for i := range 12 {
		evaluation := versionedScore("planner", 90, base.Add(time.Duration(i)*24*time.Hour))
		evaluation.AgentVersion = ""
		evaluations = append(evaluations, evaluation)
	}

	writeSessionSnapshot(t, store, Session{
		ID:          "missing-version",
		CreatedAt:   base,
		UpdatedAt:   base.Add(11 * 24 * time.Hour),
		Evaluations: evaluations,
	})

	summaries, err := store.agentPerformanceSummary(base.Add(12 * 24 * time.Hour))
	require.NoError(t, err)
	require.Len(t, summaries, 1)
	require.Len(t, summaries[0].ScoreBuckets, 1)

	bucket := summaries[0].ScoreBuckets[0]
	assert.False(t, bucket.RoutingEligible)
	assert.Contains(t, bucket.ValidityReasons, "agent version is missing")
	assert.False(t, summaries[0].Validity.RoutingEligible)
	assert.Contains(t, summaries[0].Validity.Reasons, "no scored bucket declares agent version")
}

func TestStore_AgentPerformanceSummaryTracksRegressionWindow(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

	evaluations := make([]AgentEvaluation, 0, 12)
	for i := range 6 {
		evaluations = append(evaluations, versionedScore("critic", 95, base.Add(time.Duration(i)*24*time.Hour)))
	}

	for i := range 6 {
		evaluations = append(evaluations, versionedScore("critic", 70, base.Add((40+time.Duration(i))*24*time.Hour)))
	}

	writeSessionSnapshot(t, store, Session{
		ID:          "regression",
		CreatedAt:   base,
		UpdatedAt:   base.Add(45 * 24 * time.Hour),
		Evaluations: evaluations,
	})

	summaries, err := store.agentPerformanceSummary(base.Add(45 * 24 * time.Hour))
	require.NoError(t, err)
	require.Len(t, summaries, 1)
	require.Len(t, summaries[0].ScoreBuckets, 1)
	bucket := summaries[0].ScoreBuckets[0]
	assert.Equal(t, 6, bucket.PreviousSampleSize)
	assert.Equal(t, 6, bucket.RecentSampleSize)
	assert.Equal(t, base.Add(45*24*time.Hour), bucket.LatestScoreAt)
	assert.Equal(t, base.Add(15*24*time.Hour), bucket.RecentWindowStart)
	assert.InEpsilon(t, 95, bucket.PreviousAverageScore, 0.0001)
	assert.InEpsilon(t, 70, bucket.RecentAverageScore, 0.0001)
	assert.InEpsilon(t, -25, bucket.RegressionDelta, 0.0001)
	assert.Equal(t, "regressed", bucket.RegressionStatus)
}

func TestStore_AgentPerformanceSummaryRequiresCurrentRecentEvidence(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	evaluations := make([]AgentEvaluation, 0, 12)
	for i := range 12 {
		evaluations = append(evaluations, versionedScore("planner", 90, base.Add(time.Duration(i)*24*time.Hour)))
	}

	writeSessionSnapshot(t, store, Session{
		ID:          "stale",
		CreatedAt:   base,
		UpdatedAt:   base.Add(11 * 24 * time.Hour),
		Evaluations: evaluations,
	})

	referenceTime := base.Add(90 * 24 * time.Hour)
	summaries, err := store.agentPerformanceSummary(referenceTime)
	require.NoError(t, err)
	require.Len(t, summaries, 1)
	require.Len(t, summaries[0].ScoreBuckets, 1)

	bucket := summaries[0].ScoreBuckets[0]
	assert.Equal(t, 0, bucket.RecentSampleSize)
	assert.Equal(t, referenceTime.Add(-performanceRecentWindow), bucket.RecentWindowStart)
	assert.False(t, bucket.RoutingEligible)
	assert.Contains(t, bucket.ValidityReasons, "recent sample size 0 is below required 3")
	assert.False(t, summaries[0].Validity.RoutingEligible)
	assert.Contains(t, summaries[0].Validity.Reasons, "no compatible scored bucket has at least 3 recent samples")
}

func TestAssessPerformanceValidityExplainsSplitEvidenceGaps(t *testing.T) {
	t.Parallel()

	validity := assessPerformanceValidity([]ScoreBucketSummary{
		{
			Source:           EvaluationSourceHuman,
			RubricVersion:    "rubric/v1",
			TaskType:         "implementation",
			Difficulty:       "medium",
			Model:            "gpt-test",
			AgentVersion:     "planner@1",
			SampleSize:       minRoutingScoreSamples,
			RecentSampleSize: minRoutingRecentSamples,
			StandardError:    0,
		},
		{
			Source:                EvaluationSourceHuman,
			RubricVersion:         RubricVersionLegacy,
			TaskType:              "implementation",
			Difficulty:            "medium",
			Model:                 "gpt-test",
			AgentVersion:          "planner@1",
			SampleSize:            minRoutingScoreSamples,
			RecentSampleSize:      minRoutingRecentSamples,
			StandardError:         0,
			ConfidenceSampleCount: minRoutingScoreSamples,
			AverageConfidence:     minRoutingConfidence,
		},
	})

	assert.False(t, validity.RoutingEligible)
	assert.Equal(t, []string{"no scored bucket passes every routing validity check"}, validity.Reasons)
}

func TestAssessPerformanceValidityRejectsBlankBucketMetadata(t *testing.T) {
	t.Parallel()

	bucket := ScoreBucketSummary{
		Source:                EvaluationSourceHuman,
		SampleSize:            minRoutingScoreSamples,
		RecentSampleSize:      minRoutingRecentSamples,
		ConfidenceSampleCount: minRoutingScoreSamples,
		AverageConfidence:     minRoutingConfidence,
		StandardError:         0,
	}

	validity := assessPerformanceValidity([]ScoreBucketSummary{bucket})

	assert.False(t, validity.RoutingEligible)
	assert.Contains(t, validity.Reasons, "no scored bucket uses a versioned rubric")
	assert.Contains(t, validity.Reasons, "no scored bucket declares a task type")
	assert.Contains(t, validity.Reasons, "no scored bucket declares difficulty")
	assert.Contains(t, validity.Reasons, "no scored bucket declares model")
	assert.Contains(t, validity.Reasons, "no scored bucket declares agent version")
	assert.Contains(t, scoreBucketValidityReasons(bucket), "rubric version is legacy or missing")
	assert.Contains(t, scoreBucketValidityReasons(bucket), "task type is missing")
	assert.Contains(t, scoreBucketValidityReasons(bucket), "difficulty is missing")
	assert.Contains(t, scoreBucketValidityReasons(bucket), "model is missing")
	assert.Contains(t, scoreBucketValidityReasons(bucket), "agent version is missing")
}

func TestAgentPerformanceSummary_RoutingEligibleScoreBucketsRequiresSummaryValidity(t *testing.T) {
	t.Parallel()

	summary := AgentPerformanceSummary{
		Validity: PerformanceValidity{RoutingEligible: false, EligibleScoreBuckets: 1},
		ScoreBuckets: []ScoreBucketSummary{
			{
				Source:                EvaluationSourceHarness,
				RubricVersion:         "rubric/v1",
				TaskType:              "implementation",
				Difficulty:            "hard",
				Model:                 "gpt-test",
				AgentVersion:          "planner@1",
				SampleSize:            minRoutingScoreSamples,
				RecentSampleSize:      minRoutingRecentSamples,
				ConfidenceSampleCount: minRoutingScoreSamples,
				AverageConfidence:     minRoutingConfidence,
				StandardError:         0,
			},
		},
	}

	assert.Empty(t, summary.RoutingEligibleScoreBuckets())
}

func TestSummarizeObservationsUsesSmallSampleAdjustedInterval(t *testing.T) {
	t.Parallel()

	stats := summarizeObservations([]scoreObservation{
		{score: 70},
		{score: 90},
	})

	assert.Equal(t, 2, stats.sample)
	assert.InEpsilon(t, 80, stats.average, 0.0001)
	assert.InEpsilon(t, 10, stats.stdErr, 0.0001)
	assert.InEpsilon(t, -47.06, stats.ciLow, 0.0001)
	assert.InEpsilon(t, 207.06, stats.ciHigh, 0.0001)
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

	summaries, err := store.agentPerformanceSummary(base.Add(time.Hour))
	require.NoError(t, err)
	require.Len(t, summaries, 1)
	assert.Equal(t, []OutcomeCount{
		{Outcome: "pass", Count: 2},
		{Outcome: "warn", Count: 2},
		{Outcome: "fail", Count: 1},
	}, summaries[0].Outcomes)
	assert.Equal(t, 1, summaries[0].FailureCount)
}

func versionedScore(agent string, score int, createdAt time.Time) AgentEvaluation {
	return AgentEvaluation{
		Agent:         agent,
		Outcome:       "pass",
		Source:        EvaluationSourceHarness,
		Evaluator:     "eval-bot",
		RubricVersion: "rubric/v1",
		TaskType:      "implementation",
		Difficulty:    "hard",
		Model:         "gpt-test",
		AgentVersion:  agent + "@1",
		Score:         score,
		Confidence:    0.9,
		SchemaVersion: AgentEvaluationSchemaVersion,
		CreatedAt:     createdAt,
	}
}

func findScoreBucket(t *testing.T, buckets []ScoreBucketSummary, source, rubricVersion string) ScoreBucketSummary {
	t.Helper()

	for i := range buckets {
		bucket := &buckets[i]
		if bucket.Source == source && bucket.RubricVersion == rubricVersion {
			return *bucket
		}
	}

	require.Failf(t, "missing score bucket", "source=%s rubric=%s buckets=%+v", source, rubricVersion, buckets)

	return ScoreBucketSummary{}
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
