package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// AgentPerformanceSummary aggregates recorded agent performance across saved sessions.
type AgentPerformanceSummary struct {
	LatestActivity           time.Time
	Agent                    string
	Outcomes                 []OutcomeCount
	AverageScore             float64
	EvaluationCount          int
	NegativeKnowledgeCount   int
	FailureCount             int
	ScoredEvaluationCount    int
	MinScore                 int
	MaxScore                 int
	DefaultAgentSessionCount int
}

// OutcomeCount counts evaluations by outcome for an agent.
type OutcomeCount struct {
	Outcome string
	Count   int
}

type agentPerformanceAccumulator struct {
	latestActivity           time.Time
	outcomeDisplay           map[string]string
	outcomeCounts            map[string]int
	agent                    string
	evaluationCount          int
	negativeKnowledgeCount   int
	failureCount             int
	scoredEvaluationCount    int
	scoreTotal               int
	minScore                 int
	maxScore                 int
	defaultAgentSessionCount int
}

// AgentPerformanceSummary loads all saved sessions and groups performance records by agent.
func (s *Store) AgentPerformanceSummary() ([]AgentPerformanceSummary, error) {
	sessions, err := s.loadAllSessions()
	if err != nil {
		return nil, err
	}

	byAgent := make(map[string]*agentPerformanceAccumulator)
	for i := range sessions {
		session := &sessions[i]
		sessionActivity := fallbackActivity(session.UpdatedAt, session.CreatedAt)

		if agent := strings.TrimSpace(session.DefaultAgent); agent != "" {
			acc := agentAccumulator(byAgent, agent)
			acc.defaultAgentSessionCount++
			acc.observeActivity(sessionActivity)
		}

		for _, evaluation := range session.Evaluations {
			agent := strings.TrimSpace(evaluation.Agent)
			if agent == "" {
				continue
			}
			acc := agentAccumulator(byAgent, agent)
			acc.evaluationCount++
			acc.observeActivity(fallbackActivity(evaluation.CreatedAt, sessionActivity))
			acc.observeOutcome(evaluation.Outcome)
			acc.observeScore(evaluation.Score)
		}

		for _, knowledge := range session.NegativeKnowledge {
			agent := strings.TrimSpace(knowledge.Agent)
			if agent == "" {
				continue
			}
			acc := agentAccumulator(byAgent, agent)
			acc.negativeKnowledgeCount++
			acc.failureCount++
			acc.observeActivity(fallbackActivity(knowledge.CreatedAt, sessionActivity))
		}
	}

	summaries := make([]AgentPerformanceSummary, 0, len(byAgent))
	for _, acc := range byAgent {
		summary := AgentPerformanceSummary{
			Agent:                    acc.agent,
			EvaluationCount:          acc.evaluationCount,
			NegativeKnowledgeCount:   acc.negativeKnowledgeCount,
			FailureCount:             acc.failureCount,
			Outcomes:                 acc.outcomes(),
			ScoredEvaluationCount:    acc.scoredEvaluationCount,
			MinScore:                 acc.minScore,
			MaxScore:                 acc.maxScore,
			LatestActivity:           acc.latestActivity,
			DefaultAgentSessionCount: acc.defaultAgentSessionCount,
		}
		if acc.scoredEvaluationCount > 0 {
			summary.AverageScore = float64(acc.scoreTotal) / float64(acc.scoredEvaluationCount)
		}
		summaries = append(summaries, summary)
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
		agent:          strings.TrimSpace(agent),
		outcomeDisplay: make(map[string]string),
		outcomeCounts:  make(map[string]int),
	}
	byAgent[key] = acc
	return acc
}

func (a *agentPerformanceAccumulator) observeActivity(activity time.Time) {
	if activity.After(a.latestActivity) {
		a.latestActivity = activity
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

func (a *agentPerformanceAccumulator) observeScore(score int) {
	if score == 0 {
		return
	}
	a.scoredEvaluationCount++
	a.scoreTotal += score
	if a.scoredEvaluationCount == 1 || score < a.minScore {
		a.minScore = score
	}
	if a.scoredEvaluationCount == 1 || score > a.maxScore {
		a.maxScore = score
	}
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

func fallbackActivity(primary, fallback time.Time) time.Time {
	if !primary.IsZero() {
		return primary
	}
	return fallback
}

func normalizeAgentKey(agent string) string {
	return strings.ToLower(strings.TrimSpace(agent))
}
