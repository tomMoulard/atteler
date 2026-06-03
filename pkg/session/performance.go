package session

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	performanceRecentWindowDays = 30
	performanceRecentWindow     = performanceRecentWindowDays * 24 * time.Hour
	minRoutingScoreSamples      = 10
	minRoutingRecentSamples     = 3
	maxRoutingStandardError     = 5.0
	minRoutingConfidence        = 0.70

	unspecifiedTaskType = "unspecified"
)

var studentTCritical95 = [...]float64{
	12.706, 4.303, 3.182, 2.776, 2.571,
	2.447, 2.365, 2.306, 2.262, 2.228,
	2.201, 2.179, 2.160, 2.145, 2.131,
	2.120, 2.110, 2.101, 2.093, 2.086,
	2.080, 2.074, 2.069, 2.064, 2.060,
	2.056, 2.052, 2.048, 2.045, 2.042,
}

// AgentPerformanceSummary aggregates recorded agent performance across saved sessions.
// It is diagnostic by default: callers must not use it for routing or agent
// selection unless Validity.RoutingEligible is true and they choose a compatible
// ScoreBuckets entry that matches the task being routed.
type AgentPerformanceSummary struct {
	LatestActivity             time.Time
	Agent                      string
	Outcomes                   []OutcomeCount
	ScoreBuckets               []ScoreBucketSummary
	EvaluationProvenance       []ProvenanceCount
	RubricVersions             []RubricVersionCount
	Evaluators                 []EvaluatorCount
	NegativeKnowledgeBreakdown []NegativeKnowledgeCategoryCount
	Validity                   PerformanceValidity
	RecentWindowDays           int
	AverageScore               float64
	AveragePassRate            float64
	EvaluationCount            int
	NegativeKnowledgeCount     int
	FailureCount               int
	ScoredEvaluationCount      int
	PassRateSampleCount        int
	FlakeCount                 int
	FlakyEvaluationCount       int
	InputTokens                int
	OutputTokens               int
	TotalTokens                int
	TokenSampleCount           int
	AverageDurationMillis      float64
	DurationSampleCount        int
	TotalCost                  float64
	AverageCost                float64
	CostSampleCount            int
	MinScore                   int
	MaxScore                   int
	DefaultAgentSessionCount   int
}

// OutcomeCount counts evaluations by outcome for an agent.
type OutcomeCount struct {
	Outcome string
	Count   int
}

// ScoreBucketSummary describes one compatible score cohort.
type ScoreBucketSummary struct {
	LatestScoreAt          time.Time
	RecentWindowStart      time.Time
	Source                 string
	RubricVersion          string
	TaskType               string
	Difficulty             string
	Provider               string
	Model                  string
	FixtureVersion         string
	AgentVersion           string
	Uncertainty            string
	RegressionStatus       string
	ValidityReasons        []string
	AverageScore           float64
	MinScore               int
	MaxScore               int
	StandardError          float64
	ConfidenceIntervalLow  float64
	ConfidenceIntervalHigh float64
	AverageConfidence      float64
	AveragePassRate        float64
	AverageDurationMillis  float64
	TotalCost              float64
	AverageCost            float64
	RecentAverageScore     float64
	PreviousAverageScore   float64
	RegressionDelta        float64
	SampleSize             int
	ConfidenceSampleCount  int
	PassRateSampleCount    int
	FlakeCount             int
	FlakyEvaluationCount   int
	DurationSampleCount    int
	CostSampleCount        int
	InputTokens            int
	OutputTokens           int
	TotalTokens            int
	TokenSampleCount       int
	RecentSampleSize       int
	PreviousSampleSize     int
	RoutingEligible        bool
}

// ProvenanceCount counts evaluations by source/provenance.
type ProvenanceCount struct {
	Source string
	Count  int
}

// RubricVersionCount counts evaluations by rubric version.
type RubricVersionCount struct {
	RubricVersion string
	Count         int
}

// EvaluatorCount counts evaluations by evaluator identity.
type EvaluatorCount struct {
	Evaluator string
	Count     int
}

// NegativeKnowledgeCategoryCount counts negative-knowledge incidents by task class and severity.
type NegativeKnowledgeCategoryCount struct {
	TaskType string
	Severity string
	Count    int
}

// PerformanceValidity documents whether a summary can safely inform routing.
type PerformanceValidity struct {
	Checks                []string
	Reasons               []string
	MaximumStandardError  float64
	MinimumMeanConfidence float64
	EligibleScoreBuckets  int
	MinimumSampleSize     int
	MinimumRecentSamples  int
	RecencyWindowDays     int
	RoutingEligible       bool
}

// RoutingEligibleScoreBuckets returns only score buckets that pass all routing
// validity checks. Routing callers should use this helper rather than reading
// aggregate score fields or iterating ScoreBuckets directly.
func (summary AgentPerformanceSummary) RoutingEligibleScoreBuckets() []ScoreBucketSummary {
	if !summary.Validity.RoutingEligible {
		return nil
	}

	eligible := make([]ScoreBucketSummary, 0, summary.Validity.EligibleScoreBuckets)
	for i := range summary.ScoreBuckets {
		bucket := summary.ScoreBuckets[i]
		if !scoreBucketRoutingEligible(bucket) {
			continue
		}

		bucket.RoutingEligible = true
		bucket.ValidityReasons = nil
		eligible = append(eligible, bucket)
	}

	return eligible
}

type agentPerformanceAccumulator struct {
	latestActivity           time.Time
	evaluatorCounts          map[string]int
	negativeCategoryCounts   map[string]int
	provenanceDisplay        map[string]string
	provenanceCounts         map[string]int
	rubricDisplay            map[string]string
	rubricCounts             map[string]int
	evaluatorDisplay         map[string]string
	outcomeDisplay           map[string]string
	outcomeCounts            map[string]int
	scoreBuckets             map[string]*scoreBucketAccumulator
	negativeCategoryDisplay  map[string]negativeKnowledgeCategoryKey
	agent                    string
	evalMetrics              []evaluationMetricObservation
	evaluationCount          int
	negativeKnowledgeCount   int
	failureCount             int
	defaultAgentSessionCount int
}

type scoreBucketAccumulator struct {
	key          scoreBucketKey
	observations []scoreObservation
}

type scoreObservation struct {
	createdAt     time.Time
	confidence    float64
	cost          float64
	passRate      float64
	durationMS    int64
	score         int
	flakeCount    int
	inputTokens   int
	outputTokens  int
	totalTokens   int
	hasConfidence bool
	hasDuration   bool
	hasCost       bool
	hasPassRate   bool
	hasTokens     bool
}

type evaluationMetricObservation struct {
	passRate     float64
	flakeCount   int
	inputTokens  int
	outputTokens int
	totalTokens  int
	durationMS   int64
	cost         float64
	hasPassRate  bool
	hasTokens    bool
	hasDuration  bool
	hasCost      bool
}

type scoreBucketKey struct {
	source         string
	rubricVersion  string
	taskType       string
	difficulty     string
	provider       string
	model          string
	fixtureVersion string
	agentVersion   string
}

type negativeKnowledgeCategoryKey struct {
	taskType string
	severity string
}

type numericStats struct {
	average float64
	min     int
	max     int
	stdErr  float64
	ciLow   float64
	ciHigh  float64
	sample  int
}

type validityGaps struct {
	missingVersionedRubric bool
	missingKnownProvenance bool
	missingTaskClass       bool
	missingDifficulty      bool
	missingModel           bool
	missingAgentVersion    bool
	missingConfidence      bool
	tooFewSamples          bool
	tooFewRecentSamples    bool
	wideUncertainty        bool
}

// AgentPerformanceSummary loads all saved sessions and groups performance records by agent.
func (s *Store) AgentPerformanceSummary() ([]AgentPerformanceSummary, error) {
	return s.agentPerformanceSummary(time.Now().UTC())
}

func (s *Store) agentPerformanceSummary(referenceTime time.Time) ([]AgentPerformanceSummary, error) {
	if referenceTime.IsZero() {
		referenceTime = time.Now().UTC()
	}

	sessions, err := s.loadAllSessions()
	if err != nil {
		return nil, err
	}

	byAgent := make(map[string]*agentPerformanceAccumulator)

	for i := range sessions {
		observeSessionPerformance(byAgent, &sessions[i])
	}

	summaries := make([]AgentPerformanceSummary, 0, len(byAgent))
	for _, acc := range byAgent {
		summaries = append(summaries, acc.summary(referenceTime))
	}

	sort.Slice(summaries, func(i, j int) bool {
		left := strings.ToLower(summaries[i].Agent)

		right := strings.ToLower(summaries[j].Agent)
		if left == right {
			return summaries[i].Agent < summaries[j].Agent
		}

		return left < right
	})

	return summaries, nil
}

func observeSessionPerformance(byAgent map[string]*agentPerformanceAccumulator, session *Session) {
	sessionActivity := fallbackActivity(session.UpdatedAt, session.CreatedAt)

	observeDefaultAgentSession(byAgent, session.DefaultAgent, sessionActivity)
	observeSessionEvaluations(byAgent, session, sessionActivity)
	observeSessionNegativeKnowledge(byAgent, session, sessionActivity)
}

func observeDefaultAgentSession(
	byAgent map[string]*agentPerformanceAccumulator,
	agent string,
	sessionActivity time.Time,
) {
	if agent = strings.TrimSpace(agent); agent == "" {
		return
	}

	acc := agentAccumulator(byAgent, agent)
	acc.defaultAgentSessionCount++
	acc.observeActivity(sessionActivity)
}

func observeSessionEvaluations(
	byAgent map[string]*agentPerformanceAccumulator,
	session *Session,
	sessionActivity time.Time,
) {
	for i := range session.Evaluations {
		evaluation := &session.Evaluations[i]

		agent := strings.TrimSpace(evaluation.Agent)
		if agent == "" {
			continue
		}

		acc := agentAccumulator(byAgent, agent)
		activity := fallbackActivity(evaluation.CreatedAt, sessionActivity)
		source := normalizedEvaluationSource(*evaluation)
		rubricVersion := normalizedRubricVersion(*evaluation)

		acc.observeEvaluation(*evaluation, session.DefaultModel, source, rubricVersion, activity)
	}
}

func observeSessionNegativeKnowledge(
	byAgent map[string]*agentPerformanceAccumulator,
	session *Session,
	sessionActivity time.Time,
) {
	for i := range session.NegativeKnowledge {
		knowledge := &session.NegativeKnowledge[i]

		agent := strings.TrimSpace(knowledge.Agent)
		if agent == "" {
			continue
		}

		acc := agentAccumulator(byAgent, agent)
		acc.negativeKnowledgeCount++
		acc.observeNegativeKnowledgeCategory(*knowledge)
		acc.observeActivity(fallbackActivity(knowledge.CreatedAt, sessionActivity))
	}
}

func (s *Store) loadAllSessions() ([]Session, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}

		return nil, fmt.Errorf("session: list %s: %w", s.dir, err)
	}

	sessions := make([]Session, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != sessionFileExt {
			continue
		}

		session, err := s.Load(filepath.Join(s.dir, entry.Name()))
		if err != nil {
			return nil, err
		}

		sessions = append(sessions, session)
	}

	return sessions, nil
}

func agentAccumulator(byAgent map[string]*agentPerformanceAccumulator, agent string) *agentPerformanceAccumulator {
	key := normalizeAgentKey(agent)

	acc, ok := byAgent[key]
	if ok {
		return acc
	}

	acc = &agentPerformanceAccumulator{
		agent:                   strings.TrimSpace(agent),
		outcomeDisplay:          make(map[string]string),
		outcomeCounts:           make(map[string]int),
		provenanceDisplay:       make(map[string]string),
		provenanceCounts:        make(map[string]int),
		rubricDisplay:           make(map[string]string),
		rubricCounts:            make(map[string]int),
		evaluatorDisplay:        make(map[string]string),
		evaluatorCounts:         make(map[string]int),
		negativeCategoryDisplay: make(map[string]negativeKnowledgeCategoryKey),
		negativeCategoryCounts:  make(map[string]int),
		scoreBuckets:            make(map[string]*scoreBucketAccumulator),
	}
	byAgent[key] = acc

	return acc
}

func (a *agentPerformanceAccumulator) summary(referenceTime time.Time) AgentPerformanceSummary {
	summary := AgentPerformanceSummary{
		Agent:                      a.agent,
		EvaluationCount:            a.evaluationCount,
		NegativeKnowledgeCount:     a.negativeKnowledgeCount,
		FailureCount:               a.failureCount,
		Outcomes:                   a.outcomes(),
		EvaluationProvenance:       a.provenance(),
		RubricVersions:             a.rubrics(),
		Evaluators:                 a.evaluators(),
		NegativeKnowledgeBreakdown: a.negativeKnowledgeBreakdown(),
		LatestActivity:             a.latestActivity,
		DefaultAgentSessionCount:   a.defaultAgentSessionCount,
		RecentWindowDays:           performanceRecentWindowDays,
	}

	summary.ScoreBuckets = a.scoreBucketSummaries(referenceTime)
	applyScoreBucketTotals(&summary)
	applyEvaluationMetricTotals(&summary, a.evalMetrics)

	// Backward-compatible aggregate score fields are populated only when all
	// score-bearing records are in one compatible cohort. Cross-rubric/source
	// aggregate scores are intentionally withheld.
	if len(summary.ScoreBuckets) == 1 {
		summary.AverageScore = summary.ScoreBuckets[0].AverageScore
	}

	summary.Validity = assessPerformanceValidity(summary.ScoreBuckets)

	return summary
}

func applyScoreBucketTotals(summary *AgentPerformanceSummary) {
	for i := range summary.ScoreBuckets {
		bucket := &summary.ScoreBuckets[i]

		summary.ScoredEvaluationCount += bucket.SampleSize
	}

	if len(summary.ScoreBuckets) != 1 {
		return
	}

	summary.MinScore = summary.ScoreBuckets[0].MinScore
	summary.MaxScore = summary.ScoreBuckets[0].MaxScore
}

func applyEvaluationMetricTotals(summary *AgentPerformanceSummary, observations []evaluationMetricObservation) {
	passRateAverage, passRateSample := summarizeEvaluationPassRate(observations)
	flakeCount, flakyEvaluationCount := summarizeEvaluationFlakes(observations)
	inputTokens, outputTokens, totalTokens, tokenSample := summarizeEvaluationTokens(observations)
	averageDuration, durationSample := summarizeEvaluationDuration(observations)
	totalCost, averageCost, costSample := summarizeEvaluationCost(observations)

	summary.AveragePassRate = passRateAverage
	summary.PassRateSampleCount = passRateSample
	summary.FlakeCount = flakeCount
	summary.FlakyEvaluationCount = flakyEvaluationCount
	summary.InputTokens = inputTokens
	summary.OutputTokens = outputTokens
	summary.TotalTokens = totalTokens
	summary.TokenSampleCount = tokenSample
	summary.AverageDurationMillis = averageDuration
	summary.DurationSampleCount = durationSample
	summary.TotalCost = totalCost
	summary.AverageCost = averageCost
	summary.CostSampleCount = costSample
}

func (a *agentPerformanceAccumulator) observeActivity(activity time.Time) {
	if activity.After(a.latestActivity) {
		a.latestActivity = activity
	}
}

func (a *agentPerformanceAccumulator) observeEvaluation(
	evaluation AgentEvaluation,
	sessionModel string,
	source string,
	rubricVersion string,
	activity time.Time,
) {
	a.evaluationCount++
	a.observeActivity(activity)
	a.observeOutcome(evaluation.Outcome)
	a.observeProvenance(source)
	a.observeRubric(rubricVersion)
	a.observeEvaluator(evaluation.Evaluator)
	a.observeEvaluationMetrics(evaluation)
	a.observeScore(evaluation, sessionModel, source, rubricVersion, activity)

	if failureOutcome(evaluation.Outcome) {
		a.failureCount++
	}
}

func (a *agentPerformanceAccumulator) observeOutcome(outcome string) {
	outcome = strings.TrimSpace(outcome)
	if outcome == "" {
		return
	}

	key := strings.ToLower(outcome)
	if _, ok := a.outcomeDisplay[key]; !ok {
		a.outcomeDisplay[key] = outcome
	}

	a.outcomeCounts[key]++
}

func (a *agentPerformanceAccumulator) observeProvenance(source string) {
	a.observeNamedCount(source, a.provenanceDisplay, a.provenanceCounts)
}

func (a *agentPerformanceAccumulator) observeRubric(rubricVersion string) {
	a.observeNamedCount(rubricVersion, a.rubricDisplay, a.rubricCounts)
}

func (a *agentPerformanceAccumulator) observeEvaluator(evaluator string) {
	a.observeNamedCount(normalizedDimension(evaluator), a.evaluatorDisplay, a.evaluatorCounts)
}

func (a *agentPerformanceAccumulator) observeNamedCount(value string, display map[string]string, counts map[string]int) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}

	key := strings.ToLower(value)
	if _, ok := display[key]; !ok {
		display[key] = value
	}

	counts[key]++
}

func (a *agentPerformanceAccumulator) observeNegativeKnowledgeCategory(knowledge NegativeKnowledge) {
	category := negativeKnowledgeCategoryKey{
		taskType: normalizedDimension(knowledge.TaskType),
		severity: normalizedDimension(knowledge.Severity),
	}

	key := strings.ToLower(category.taskType) + "\x00" + strings.ToLower(category.severity)
	if _, ok := a.negativeCategoryDisplay[key]; !ok {
		a.negativeCategoryDisplay[key] = category
	}

	a.negativeCategoryCounts[key]++
}

func (a *agentPerformanceAccumulator) observeEvaluationMetrics(evaluation AgentEvaluation) {
	observation := evaluationMetricObservation{}
	if evaluation.PassRateRecorded() {
		observation.passRate = evaluation.PassRate
		observation.hasPassRate = true
	}

	if evaluation.FlakeCount > 0 {
		observation.flakeCount = evaluation.FlakeCount
	}

	if evaluation.InputTokens > 0 || evaluation.OutputTokens > 0 || evaluation.TotalTokens > 0 {
		observation.inputTokens = evaluation.InputTokens
		observation.outputTokens = evaluation.OutputTokens
		observation.totalTokens = evaluation.TotalTokens

		if observation.totalTokens == 0 {
			observation.totalTokens = observation.inputTokens + observation.outputTokens
		}

		observation.hasTokens = true
	}

	if evaluation.DurationMillis > 0 {
		observation.durationMS = evaluation.DurationMillis
		observation.hasDuration = true
	}

	if evaluation.Cost > 0 {
		observation.cost = evaluation.Cost
		observation.hasCost = true
	}

	if observation.hasPassRate || observation.flakeCount > 0 || observation.hasTokens ||
		observation.hasDuration || observation.hasCost {
		a.evalMetrics = append(a.evalMetrics, observation)
	}
}

func (a *agentPerformanceAccumulator) observeScore(
	evaluation AgentEvaluation,
	sessionModel string,
	source string,
	rubricVersion string,
	createdAt time.Time,
) {
	if evaluation.Score == 0 {
		return
	}

	key := scoreBucketKey{
		source:         source,
		rubricVersion:  rubricVersion,
		taskType:       normalizedDimension(evaluation.TaskType),
		difficulty:     normalizedDimension(evaluation.Difficulty),
		provider:       normalizedOptionalDimension(evaluation.Provider),
		model:          normalizedDimension(firstNonBlank(evaluation.Model, sessionModel)),
		fixtureVersion: normalizedOptionalDimension(evaluation.FixtureVersion),
		agentVersion:   normalizedDimension(evaluation.AgentVersion),
	}
	bucketKey := key.string()

	bucket, ok := a.scoreBuckets[bucketKey]
	if !ok {
		bucket = &scoreBucketAccumulator{key: key}
		a.scoreBuckets[bucketKey] = bucket
	}

	observation := scoreObservation{
		createdAt: createdAt,
		score:     evaluation.Score,
	}
	if evaluation.Confidence > 0 {
		observation.confidence = evaluation.Confidence
		observation.hasConfidence = true
	}

	if evaluation.DurationMillis > 0 {
		observation.durationMS = evaluation.DurationMillis
		observation.hasDuration = true
	}

	if evaluation.Cost > 0 {
		observation.cost = evaluation.Cost
		observation.hasCost = true
	}

	if evaluation.PassRateRecorded() {
		observation.passRate = evaluation.PassRate
		observation.hasPassRate = true
	}

	if evaluation.FlakeCount > 0 {
		observation.flakeCount = evaluation.FlakeCount
	}

	if evaluation.InputTokens > 0 || evaluation.OutputTokens > 0 || evaluation.TotalTokens > 0 {
		observation.inputTokens = evaluation.InputTokens
		observation.outputTokens = evaluation.OutputTokens
		observation.totalTokens = evaluation.TotalTokens

		if observation.totalTokens == 0 {
			observation.totalTokens = observation.inputTokens + observation.outputTokens
		}

		observation.hasTokens = true
	}

	bucket.observations = append(bucket.observations, observation)
}

func (a *agentPerformanceAccumulator) outcomes() []OutcomeCount {
	outcomes := make([]OutcomeCount, 0, len(a.outcomeCounts))
	for key, count := range a.outcomeCounts {
		outcomes = append(outcomes, OutcomeCount{Outcome: a.outcomeDisplay[key], Count: count})
	}

	sort.Slice(outcomes, func(i, j int) bool {
		if outcomes[i].Count == outcomes[j].Count {
			left := strings.ToLower(outcomes[i].Outcome)

			right := strings.ToLower(outcomes[j].Outcome)
			if left == right {
				return outcomes[i].Outcome < outcomes[j].Outcome
			}

			return left < right
		}

		return outcomes[i].Count > outcomes[j].Count
	})

	return outcomes
}

func (a *agentPerformanceAccumulator) provenance() []ProvenanceCount {
	counts := make([]ProvenanceCount, 0, len(a.provenanceCounts))
	for key, count := range a.provenanceCounts {
		counts = append(counts, ProvenanceCount{Source: a.provenanceDisplay[key], Count: count})
	}

	sortNamedCounts(counts, func(item ProvenanceCount) (string, int) { return item.Source, item.Count })

	return counts
}

func (a *agentPerformanceAccumulator) rubrics() []RubricVersionCount {
	counts := make([]RubricVersionCount, 0, len(a.rubricCounts))
	for key, count := range a.rubricCounts {
		counts = append(counts, RubricVersionCount{RubricVersion: a.rubricDisplay[key], Count: count})
	}

	sortNamedCounts(counts, func(item RubricVersionCount) (string, int) { return item.RubricVersion, item.Count })

	return counts
}

func (a *agentPerformanceAccumulator) evaluators() []EvaluatorCount {
	counts := make([]EvaluatorCount, 0, len(a.evaluatorCounts))
	for key, count := range a.evaluatorCounts {
		counts = append(counts, EvaluatorCount{Evaluator: a.evaluatorDisplay[key], Count: count})
	}

	sortNamedCounts(counts, func(item EvaluatorCount) (string, int) { return item.Evaluator, item.Count })

	return counts
}

func (a *agentPerformanceAccumulator) negativeKnowledgeBreakdown() []NegativeKnowledgeCategoryCount {
	counts := make([]NegativeKnowledgeCategoryCount, 0, len(a.negativeCategoryCounts))
	for key, count := range a.negativeCategoryCounts {
		category := a.negativeCategoryDisplay[key]
		counts = append(counts, NegativeKnowledgeCategoryCount{
			TaskType: category.taskType,
			Severity: category.severity,
			Count:    count,
		})
	}

	sort.Slice(counts, func(i, j int) bool {
		if counts[i].Count != counts[j].Count {
			return counts[i].Count > counts[j].Count
		}

		if strings.EqualFold(counts[i].TaskType, counts[j].TaskType) {
			return strings.ToLower(counts[i].Severity) < strings.ToLower(counts[j].Severity)
		}

		return strings.ToLower(counts[i].TaskType) < strings.ToLower(counts[j].TaskType)
	})

	return counts
}

func (a *agentPerformanceAccumulator) scoreBucketSummaries(latestActivity time.Time) []ScoreBucketSummary {
	buckets := make([]ScoreBucketSummary, 0, len(a.scoreBuckets))
	cutoff := latestActivity.Add(-performanceRecentWindow)

	for _, bucket := range a.scoreBuckets {
		all := summarizeObservations(bucket.observations)
		recent := observationsSince(bucket.observations, cutoff)
		previous := observationsBefore(bucket.observations, cutoff)
		recentStats := summarizeObservations(recent)
		previousStats := summarizeObservations(previous)
		confidenceAverage, confidenceSample := summarizeObservationConfidence(bucket.observations)
		passRateAverage, passRateSample := summarizeObservationPassRate(bucket.observations)
		averageDuration, durationSample := summarizeObservationDuration(bucket.observations)
		totalCost, averageCost, costSample := summarizeObservationCost(bucket.observations)
		flakeCount, flakyEvaluationCount := summarizeObservationFlakes(bucket.observations)
		inputTokens, outputTokens, totalTokens, tokenSample := summarizeObservationTokens(bucket.observations)

		summary := ScoreBucketSummary{
			LatestScoreAt:          latestObservationActivity(bucket.observations),
			RecentWindowStart:      cutoff,
			Source:                 bucket.key.source,
			RubricVersion:          bucket.key.rubricVersion,
			TaskType:               bucket.key.taskType,
			Difficulty:             bucket.key.difficulty,
			Provider:               bucket.key.provider,
			Model:                  bucket.key.model,
			FixtureVersion:         bucket.key.fixtureVersion,
			AgentVersion:           bucket.key.agentVersion,
			SampleSize:             all.sample,
			AverageScore:           all.average,
			MinScore:               all.min,
			MaxScore:               all.max,
			StandardError:          all.stdErr,
			ConfidenceIntervalLow:  all.ciLow,
			ConfidenceIntervalHigh: all.ciHigh,
			Uncertainty:            uncertaintyLabel(all.sample, all.stdErr),
			AverageConfidence:      confidenceAverage,
			ConfidenceSampleCount:  confidenceSample,
			AveragePassRate:        passRateAverage,
			PassRateSampleCount:    passRateSample,
			FlakeCount:             flakeCount,
			FlakyEvaluationCount:   flakyEvaluationCount,
			AverageDurationMillis:  averageDuration,
			DurationSampleCount:    durationSample,
			TotalCost:              totalCost,
			AverageCost:            averageCost,
			CostSampleCount:        costSample,
			InputTokens:            inputTokens,
			OutputTokens:           outputTokens,
			TotalTokens:            totalTokens,
			TokenSampleCount:       tokenSample,
			RecentSampleSize:       recentStats.sample,
			RecentAverageScore:     recentStats.average,
			PreviousSampleSize:     previousStats.sample,
			PreviousAverageScore:   previousStats.average,
		}
		if recentStats.sample > 0 && previousStats.sample > 0 {
			summary.RegressionDelta = recentStats.average - previousStats.average
		}

		summary.RegressionStatus = regressionStatus(summary)
		summary.ValidityReasons = scoreBucketValidityReasons(summary)
		summary.RoutingEligible = len(summary.ValidityReasons) == 0
		buckets = append(buckets, summary)
	}

	sort.Slice(buckets, func(i, j int) bool {
		return buckets[i].sortKey() < buckets[j].sortKey()
	})

	return buckets
}

func assessPerformanceValidity(buckets []ScoreBucketSummary) PerformanceValidity {
	validity := PerformanceValidity{
		Checks: []string{
			"scores are evaluated within compatible source/rubric/task/difficulty/model/agent-version buckets",
			"routing requires a versioned rubric and task-normalized scored sample size",
			"routing requires difficulty/model/agent-version metadata",
			"routing requires recent evidence inside the recency window",
			"routing requires reported evaluator confidence and bounded uncertainty",
		},
		MinimumSampleSize:     minRoutingScoreSamples,
		MinimumRecentSamples:  minRoutingRecentSamples,
		MaximumStandardError:  maxRoutingStandardError,
		MinimumMeanConfidence: minRoutingConfidence,
		RecencyWindowDays:     performanceRecentWindowDays,
	}

	if len(buckets) == 0 {
		validity.Reasons = append(validity.Reasons, "no scored evaluations")
		return validity
	}

	gaps := collectValidityGaps(buckets, &validity)

	validity.RoutingEligible = validity.EligibleScoreBuckets > 0
	if validity.RoutingEligible {
		return validity
	}

	validity.Reasons = validityGapReasons(gaps)
	if len(validity.Reasons) == 0 {
		validity.Reasons = append(validity.Reasons, "no scored bucket passes every routing validity check")
	}

	return validity
}

func collectValidityGaps(buckets []ScoreBucketSummary, validity *PerformanceValidity) validityGaps {
	gaps := validityGaps{
		missingVersionedRubric: true,
		missingKnownProvenance: true,
		missingTaskClass:       true,
		missingDifficulty:      true,
		missingModel:           true,
		missingAgentVersion:    true,
		missingConfidence:      true,
		tooFewSamples:          true,
		tooFewRecentSamples:    true,
		wideUncertainty:        true,
	}

	for i := range buckets {
		bucket := &buckets[i]
		gaps.observe(*bucket)

		if scoreBucketRoutingEligible(*bucket) {
			validity.EligibleScoreBuckets++
		}
	}

	return gaps
}

func (g *validityGaps) observe(bucket ScoreBucketSummary) {
	if versionedRubricPresent(bucket.RubricVersion) {
		g.missingVersionedRubric = false
	}

	if knownEvaluationSource(bucket.Source) {
		g.missingKnownProvenance = false
	}

	if dimensionPresent(bucket.TaskType) {
		g.missingTaskClass = false
	}

	if dimensionPresent(bucket.Difficulty) {
		g.missingDifficulty = false
	}

	if dimensionPresent(bucket.Model) {
		g.missingModel = false
	}

	if dimensionPresent(bucket.AgentVersion) {
		g.missingAgentVersion = false
	}

	if bucket.ConfidenceSampleCount >= bucket.SampleSize && bucket.AverageConfidence >= minRoutingConfidence {
		g.missingConfidence = false
	}

	if bucket.SampleSize >= minRoutingScoreSamples {
		g.tooFewSamples = false
	}

	if bucket.RecentSampleSize >= minRoutingRecentSamples {
		g.tooFewRecentSamples = false
	}

	if bucket.SampleSize >= 2 && bucket.StandardError <= maxRoutingStandardError {
		g.wideUncertainty = false
	}
}

func validityGapReasons(gaps validityGaps) []string {
	reasons := make([]string, 0, 9)
	if gaps.missingVersionedRubric {
		reasons = append(reasons, "no scored bucket uses a versioned rubric")
	}

	if gaps.missingKnownProvenance {
		reasons = append(reasons, "no scored bucket declares human, harness, or CI provenance")
	}

	if gaps.missingTaskClass {
		reasons = append(reasons, "no scored bucket declares a task type")
	}

	if gaps.missingDifficulty {
		reasons = append(reasons, "no scored bucket declares difficulty")
	}

	if gaps.missingModel {
		reasons = append(reasons, "no scored bucket declares model")
	}

	if gaps.missingAgentVersion {
		reasons = append(reasons, "no scored bucket declares agent version")
	}

	if gaps.tooFewSamples {
		reasons = append(reasons, fmt.Sprintf("no compatible scored bucket has at least %d samples", minRoutingScoreSamples))
	}

	if gaps.tooFewRecentSamples {
		reasons = append(reasons, fmt.Sprintf("no compatible scored bucket has at least %d recent samples", minRoutingRecentSamples))
	}

	if gaps.missingConfidence {
		reasons = append(reasons, "no compatible scored bucket reports enough evaluator confidence")
	}

	if gaps.wideUncertainty {
		reasons = append(reasons, fmt.Sprintf("no compatible scored bucket has standard error <= %.2f", maxRoutingStandardError))
	}

	return reasons
}

func scoreBucketRoutingEligible(bucket ScoreBucketSummary) bool {
	return len(scoreBucketValidityReasons(bucket)) == 0
}

func scoreBucketValidityReasons(bucket ScoreBucketSummary) []string {
	reasons := make([]string, 0, 10)

	if !versionedRubricPresent(bucket.RubricVersion) {
		reasons = append(reasons, "rubric version is legacy or missing")
	}

	if !knownEvaluationSource(bucket.Source) {
		reasons = append(reasons, "provenance is not human, harness, or ci")
	}

	if !dimensionPresent(bucket.TaskType) {
		reasons = append(reasons, "task type is missing")
	}

	if !dimensionPresent(bucket.Difficulty) {
		reasons = append(reasons, "difficulty is missing")
	}

	if !dimensionPresent(bucket.Model) {
		reasons = append(reasons, "model is missing")
	}

	if !dimensionPresent(bucket.AgentVersion) {
		reasons = append(reasons, "agent version is missing")
	}

	if bucket.SampleSize < minRoutingScoreSamples {
		reasons = append(reasons, fmt.Sprintf("sample size %d is below required %d", bucket.SampleSize, minRoutingScoreSamples))
	}

	if bucket.RecentSampleSize < minRoutingRecentSamples {
		reasons = append(reasons, fmt.Sprintf("recent sample size %d is below required %d", bucket.RecentSampleSize, minRoutingRecentSamples))
	}

	if bucket.ConfidenceSampleCount < bucket.SampleSize {
		reasons = append(reasons, fmt.Sprintf("confidence coverage %d/%d is incomplete", bucket.ConfidenceSampleCount, bucket.SampleSize))
	} else if bucket.AverageConfidence < minRoutingConfidence {
		reasons = append(reasons, fmt.Sprintf("average confidence %.2f is below required %.2f", bucket.AverageConfidence, minRoutingConfidence))
	}

	if bucket.SampleSize >= 2 && bucket.StandardError > maxRoutingStandardError {
		reasons = append(reasons, fmt.Sprintf("standard error %.2f exceeds maximum %.2f", bucket.StandardError, maxRoutingStandardError))
	}

	return reasons
}

func versionedRubricPresent(rubricVersion string) bool {
	rubricVersion = strings.TrimSpace(rubricVersion)

	return rubricVersion != "" && !strings.EqualFold(rubricVersion, RubricVersionLegacy)
}

func dimensionPresent(value string) bool {
	value = strings.TrimSpace(value)

	return value != "" && !strings.EqualFold(value, unspecifiedTaskType)
}

func summarizeObservations(observations []scoreObservation) numericStats {
	stats := numericStats{sample: len(observations)}
	if len(observations) == 0 {
		return stats
	}

	total := 0

	for i, observation := range observations {
		score := observation.score

		total += score
		if i == 0 || score < stats.min {
			stats.min = score
		}

		if i == 0 || score > stats.max {
			stats.max = score
		}
	}

	stats.average = float64(total) / float64(len(observations))
	if len(observations) == 1 {
		stats.ciLow = stats.average
		stats.ciHigh = stats.average

		return stats
	}

	var squaredDeltaTotal float64

	for _, observation := range observations {
		delta := float64(observation.score) - stats.average
		squaredDeltaTotal += delta * delta
	}

	variance := squaredDeltaTotal / float64(len(observations)-1)
	stats.stdErr = math.Sqrt(variance / float64(len(observations)))
	margin := confidenceIntervalMultiplier95(len(observations)) * stats.stdErr
	stats.ciLow = stats.average - margin
	stats.ciHigh = stats.average + margin

	return stats
}

func confidenceIntervalMultiplier95(sampleSize int) float64 {
	degreesOfFreedom := sampleSize - 1
	if degreesOfFreedom >= 1 && degreesOfFreedom <= len(studentTCritical95) {
		return studentTCritical95[degreesOfFreedom-1]
	}

	return 1.96
}

func summarizeObservationConfidence(observations []scoreObservation) (average float64, count int) {
	var total float64

	for _, observation := range observations {
		if !observation.hasConfidence {
			continue
		}

		total += observation.confidence
		count++
	}

	if count == 0 {
		return 0, 0
	}

	return total / float64(count), count
}

func summarizeObservationPassRate(observations []scoreObservation) (average float64, count int) {
	var total float64

	for _, observation := range observations {
		if !observation.hasPassRate {
			continue
		}

		total += observation.passRate
		count++
	}

	if count == 0 {
		return 0, 0
	}

	return total / float64(count), count
}

func summarizeObservationDuration(observations []scoreObservation) (average float64, count int) {
	var total int64

	for _, observation := range observations {
		if !observation.hasDuration {
			continue
		}

		total += observation.durationMS
		count++
	}

	if count == 0 {
		return 0, 0
	}

	return float64(total) / float64(count), count
}

func summarizeObservationCost(observations []scoreObservation) (total, average float64, count int) {
	for _, observation := range observations {
		if !observation.hasCost {
			continue
		}

		total += observation.cost
		count++
	}

	if count == 0 {
		return 0, 0, 0
	}

	return total, total / float64(count), count
}

func summarizeObservationFlakes(observations []scoreObservation) (flakeCount, flakyEvaluationCount int) {
	for _, observation := range observations {
		if observation.flakeCount == 0 {
			continue
		}

		flakeCount += observation.flakeCount
		flakyEvaluationCount++
	}

	return flakeCount, flakyEvaluationCount
}

func summarizeObservationTokens(observations []scoreObservation) (inputTokens, outputTokens, totalTokens, count int) {
	for _, observation := range observations {
		if !observation.hasTokens {
			continue
		}

		inputTokens += observation.inputTokens
		outputTokens += observation.outputTokens
		totalTokens += observation.totalTokens
		count++
	}

	return inputTokens, outputTokens, totalTokens, count
}

func summarizeEvaluationPassRate(observations []evaluationMetricObservation) (average float64, count int) {
	var total float64

	for _, observation := range observations {
		if !observation.hasPassRate {
			continue
		}

		total += observation.passRate
		count++
	}

	if count == 0 {
		return 0, 0
	}

	return total / float64(count), count
}

func summarizeEvaluationFlakes(observations []evaluationMetricObservation) (flakeCount, flakyEvaluationCount int) {
	for _, observation := range observations {
		if observation.flakeCount == 0 {
			continue
		}

		flakeCount += observation.flakeCount
		flakyEvaluationCount++
	}

	return flakeCount, flakyEvaluationCount
}

func summarizeEvaluationTokens(observations []evaluationMetricObservation) (inputTokens, outputTokens, totalTokens, count int) {
	for _, observation := range observations {
		if !observation.hasTokens {
			continue
		}

		inputTokens += observation.inputTokens
		outputTokens += observation.outputTokens
		totalTokens += observation.totalTokens
		count++
	}

	return inputTokens, outputTokens, totalTokens, count
}

func summarizeEvaluationDuration(observations []evaluationMetricObservation) (average float64, count int) {
	var total int64

	for _, observation := range observations {
		if !observation.hasDuration {
			continue
		}

		total += observation.durationMS
		count++
	}

	if count == 0 {
		return 0, 0
	}

	return float64(total) / float64(count), count
}

func summarizeEvaluationCost(observations []evaluationMetricObservation) (total, average float64, count int) {
	for _, observation := range observations {
		if !observation.hasCost {
			continue
		}

		total += observation.cost
		count++
	}

	if count == 0 {
		return 0, 0, 0
	}

	return total, total / float64(count), count
}

func latestObservationActivity(observations []scoreObservation) time.Time {
	var latest time.Time

	for _, observation := range observations {
		if observation.createdAt.After(latest) {
			latest = observation.createdAt
		}
	}

	return latest
}

func observationsSince(observations []scoreObservation, cutoff time.Time) []scoreObservation {
	filtered := make([]scoreObservation, 0, len(observations))
	for _, observation := range observations {
		if !observation.createdAt.IsZero() && !observation.createdAt.Before(cutoff) {
			filtered = append(filtered, observation)
		}
	}

	return filtered
}

func observationsBefore(observations []scoreObservation, cutoff time.Time) []scoreObservation {
	filtered := make([]scoreObservation, 0, len(observations))
	for _, observation := range observations {
		if observation.createdAt.IsZero() || observation.createdAt.Before(cutoff) {
			filtered = append(filtered, observation)
		}
	}

	return filtered
}

func uncertaintyLabel(sampleSize int, standardError float64) string {
	switch {
	case sampleSize == 0:
		return "unscored"
	case sampleSize == 1:
		return "single_sample"
	case sampleSize < minRoutingScoreSamples:
		return "low_sample"
	case standardError > maxRoutingStandardError:
		return "wide_interval"
	default:
		return "bounded"
	}
}

func regressionStatus(bucket ScoreBucketSummary) string {
	if bucket.RecentSampleSize < minRoutingRecentSamples || bucket.PreviousSampleSize < minRoutingRecentSamples {
		return "insufficient_history"
	}

	switch {
	case bucket.RegressionDelta <= -maxRoutingStandardError:
		return "regressed"
	case bucket.RegressionDelta >= maxRoutingStandardError:
		return "improved"
	default:
		return "stable"
	}
}

func failureOutcome(outcome string) bool {
	switch strings.ToLower(strings.TrimSpace(outcome)) {
	case "fail", "failed", "failure", "error", "regression":
		return true
	default:
		return false
	}
}

func normalizedEvaluationSource(evaluation AgentEvaluation) string {
	if rawSource := strings.TrimSpace(evaluation.Source); rawSource != "" {
		if source := normalizeEvaluationSourceName(rawSource); source != "" {
			return source
		}

		return rawSource
	}

	if evaluation.SchemaVersion == 0 && strings.TrimSpace(evaluation.RubricVersion) == "" {
		return EvaluationSourceLegacy
	}

	return EvaluationSourceHuman
}

func normalizedRubricVersion(evaluation AgentEvaluation) string {
	if rubricVersion := strings.TrimSpace(evaluation.RubricVersion); rubricVersion != "" {
		return rubricVersion
	}

	return RubricVersionLegacy
}

func normalizedDimension(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return unspecifiedTaskType
	}

	return value
}

func normalizedOptionalDimension(value string) string {
	return strings.TrimSpace(value)
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}

	return ""
}

func fallbackActivity(primary, fallback time.Time) time.Time {
	if !primary.IsZero() {
		return primary
	}

	return fallback
}

func normalizeAgentKey(agent string) string {
	return strings.ToLower(strings.TrimSpace(agent))
}

func (key scoreBucketKey) string() string {
	return strings.Join([]string{
		strings.ToLower(key.source),
		strings.ToLower(key.rubricVersion),
		strings.ToLower(key.taskType),
		strings.ToLower(key.difficulty),
		strings.ToLower(key.provider),
		strings.ToLower(key.model),
		strings.ToLower(key.fixtureVersion),
		strings.ToLower(key.agentVersion),
	}, "\x00")
}

func (bucket ScoreBucketSummary) sortKey() string {
	return strings.Join([]string{
		strings.ToLower(bucket.Source),
		strings.ToLower(bucket.RubricVersion),
		strings.ToLower(bucket.TaskType),
		strings.ToLower(bucket.Difficulty),
		strings.ToLower(bucket.Provider),
		strings.ToLower(bucket.Model),
		strings.ToLower(bucket.FixtureVersion),
		strings.ToLower(bucket.AgentVersion),
	}, "\x00")
}

func sortNamedCounts[T any](items []T, labelAndCount func(T) (string, int)) {
	sort.Slice(items, func(i, j int) bool {
		leftLabel, leftCount := labelAndCount(items[i])

		rightLabel, rightCount := labelAndCount(items[j])
		if leftCount != rightCount {
			return leftCount > rightCount
		}

		left := strings.ToLower(leftLabel)

		right := strings.ToLower(rightLabel)
		if left == right {
			return leftLabel < rightLabel
		}

		return left < right
	})
}
