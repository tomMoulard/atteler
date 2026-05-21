package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tommoulard/atteler/pkg/artifactmerge"
	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/session"
	"github.com/tommoulard/atteler/pkg/watch"
)

func recordFailure(
	store *session.Store,
	sessionState session.Session,
	approach string,
	reason string,
	commit string,
	agentName string,
) error {
	return recordFailureDetails(store, sessionState, session.NegativeKnowledge{
		Approach: approach,
		Reason:   reason,
		Commit:   commit,
		Agent:    agentName,
	})
}

func recordFailureDetails(
	store *session.Store,
	sessionState session.Session,
	failure session.NegativeKnowledge,
) error {
	if !sessionState.RecordNegativeKnowledgeDetails(failure) {
		return errors.New("record failure: approach and reason are required, or this failure is already recorded")
	}

	if err := store.Save(sessionState); err != nil {
		return fmt.Errorf("record failure: save session: %w", err)
	}

	fmt.Println("Recorded failure on session " + sessionState.ID)

	return nil
}

func recordEvaluation(
	store *session.Store,
	sessionState session.Session,
	agentName string,
	outcome string,
	notes string,
	reference string,
	score int,
) error {
	return recordEvaluationDetails(store, sessionState, session.AgentEvaluation{
		Agent:     agentName,
		Outcome:   outcome,
		Notes:     notes,
		Reference: reference,
		Score:     score,
	})
}

func recordEvaluationDetails(
	store *session.Store,
	sessionState session.Session,
	evaluation session.AgentEvaluation,
) error {
	if strings.TrimSpace(evaluation.Model) == "" {
		evaluation.Model = strings.TrimSpace(sessionState.DefaultModel)
	}

	if !sessionState.RecordEvaluationDetails(evaluation) {
		return errors.New("record evaluation: agent, outcome, and valid evaluation metadata are required")
	}

	if err := store.Save(sessionState); err != nil {
		return fmt.Errorf("record evaluation: save session: %w", err)
	}

	fmt.Println("Recorded evaluation on session " + sessionState.ID)

	return nil
}

func evaluationModelForRecord(modelOverride string, state appState) string {
	for _, candidate := range []string{
		modelOverride,
		state.selectedModel,
		state.sessionState.DefaultModel,
	} {
		if value := strings.TrimSpace(candidate); value != "" {
			return value
		}
	}

	return ""
}

const messagePreviewRunes = 120

func listMessages(sessionState session.Session) {
	if len(sessionState.Messages) == 0 {
		fmt.Println("No messages recorded.")
		return
	}

	for i := range sessionState.Messages {
		fmt.Println(formatMessageSummary(i+1, sessionState.Messages[i]))
	}
}

func formatMessageSummary(index int, message llm.Message) string {
	content := compactMessageWhitespace(message.Content)

	parts := []string{
		"index=" + strconv.Itoa(index),
		"role=" + string(message.Role),
		"chars=" + strconv.Itoa(len([]rune(message.Content))),
	}
	if content != "" {
		parts = append(parts, "preview="+truncateRunes(content, messagePreviewRunes))
	}

	return strings.Join(parts, "	")
}

func compactMessageWhitespace(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func truncateRunes(value string, limit int) string {
	if limit <= 0 {
		return ""
	}

	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}

	if limit == 1 {
		return "…"
	}

	return string(runes[:limit-1]) + "…"
}

func listFailures(sessionState session.Session) {
	if len(sessionState.NegativeKnowledge) == 0 {
		fmt.Println("No failures recorded.")
		return
	}

	failures := append([]session.NegativeKnowledge(nil), sessionState.NegativeKnowledge...)
	sort.SliceStable(failures, func(i, j int) bool {
		return failures[i].CreatedAt.Before(failures[j].CreatedAt)
	})

	for i := range failures {
		fmt.Println(formatFailure(failures[i]))
	}
}

func formatFailure(failure session.NegativeKnowledge) string {
	parts := []string{"approach=" + failure.Approach, "reason=" + failure.Reason}
	if !failure.CreatedAt.IsZero() {
		parts = append(parts, "created_at="+failure.CreatedAt.Format(time.RFC3339))
	}

	if failure.Agent != "" {
		parts = append(parts, "agent="+failure.Agent)
	}

	if failure.Commit != "" {
		parts = append(parts, "commit="+failure.Commit)
	}

	if failure.TaskType != "" {
		parts = append(parts, "task_type="+failure.TaskType)
	}

	if failure.Severity != "" {
		parts = append(parts, "severity="+failure.Severity)
	}

	return strings.Join(parts, "	")
}

func listEvaluations(sessionState session.Session) {
	if len(sessionState.Evaluations) == 0 {
		fmt.Println("No evaluations recorded.")
		return
	}

	evaluations := append([]session.AgentEvaluation(nil), sessionState.Evaluations...)
	sort.SliceStable(evaluations, func(i, j int) bool {
		return evaluations[i].CreatedAt.Before(evaluations[j].CreatedAt)
	})

	for i := range evaluations {
		fmt.Println(formatEvaluation(evaluations[i]))
	}
}

func formatEvaluation(evaluation session.AgentEvaluation) string {
	parts := []string{"agent=" + evaluation.Agent, "outcome=" + evaluation.Outcome}
	if !evaluation.CreatedAt.IsZero() {
		parts = append(parts, "created_at="+evaluation.CreatedAt.Format(time.RFC3339))
	}

	parts = appendEvaluationNumbers(parts, evaluation)
	parts = appendEvaluationStrings(parts, evaluation)

	return strings.Join(parts, "	")
}

func appendEvaluationNumbers(parts []string, evaluation session.AgentEvaluation) []string {
	if evaluation.Score != 0 {
		parts = append(parts, "score="+strconv.Itoa(evaluation.Score))
	}

	if evaluation.SchemaVersion != 0 {
		parts = append(parts, "schema_version="+strconv.Itoa(evaluation.SchemaVersion))
	}

	if evaluation.DurationMillis != 0 {
		parts = append(parts, "duration_millis="+strconv.FormatInt(evaluation.DurationMillis, 10))
	}

	if evaluation.Cost != 0 {
		parts = append(parts, fmt.Sprintf("cost=%.6f", evaluation.Cost))
	}

	if evaluation.Confidence != 0 {
		parts = append(parts, fmt.Sprintf("confidence=%.2f", evaluation.Confidence))
	}

	return parts
}

func appendEvaluationStrings(parts []string, evaluation session.AgentEvaluation) []string {
	fields := []struct {
		key   string
		value string
	}{
		{key: "source", value: evaluation.Source},
		{key: "evaluator", value: evaluation.Evaluator},
		{key: "rubric_version", value: evaluation.RubricVersion},
		{key: "task_type", value: evaluation.TaskType},
		{key: "difficulty", value: evaluation.Difficulty},
		{key: "expected_outcome", value: evaluation.ExpectedOutcome},
		{key: "model", value: evaluation.Model},
		{key: "agent_version", value: evaluation.AgentVersion},
		{key: "reference", value: evaluation.Reference},
		{key: "notes", value: evaluation.Notes},
	}

	for _, field := range fields {
		if field.value != "" {
			parts = append(parts, field.key+"="+field.value)
		}
	}

	return parts
}

func listArtifacts(sessionState session.Session) {
	if len(sessionState.Artifacts) == 0 {
		fmt.Println("No artifacts recorded.")
		return
	}

	artifacts := append([]session.Artifact(nil), sessionState.Artifacts...)
	sort.SliceStable(artifacts, func(i, j int) bool {
		return artifacts[i].CreatedAt.Before(artifacts[j].CreatedAt)
	})

	for i := range artifacts {
		fmt.Println(formatArtifact(artifacts[i]))
	}
}

func formatArtifact(artifact session.Artifact) string {
	parts := []string{"path=" + artifact.Path, "kind=" + artifact.Kind}
	if !artifact.CreatedAt.IsZero() {
		parts = append(parts, "created_at="+artifact.CreatedAt.Format(time.RFC3339))
	}

	if artifact.SourceAgent != "" {
		parts = append(parts, "agent="+artifact.SourceAgent)
	}

	if artifact.Summary != "" {
		parts = append(parts, "summary="+artifact.Summary)
	}

	return strings.Join(parts, "	")
}

func mergeArtifacts(ctx context.Context, state appState, outputPath string, maxBytes int) error {
	if maxBytes == 0 {
		maxBytes = int(watch.DefaultLargeFileBytes)
	}

	result, err := artifactmerge.Merge(state.cwd, state.sessionState.Artifacts, int64(maxBytes))
	if err != nil {
		return fmt.Errorf("merge artifacts: %w", err)
	}

	for _, warning := range result.Warnings {
		fmt.Fprintf(os.Stderr, "warning: artifact %s skipped: %s\n", warning.Path, warning.Reason)
	}

	if strings.TrimSpace(outputPath) == "-" {
		fmt.Print(result.Markdown)
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(outputPath), 0o750); err != nil {
		return fmt.Errorf("merge artifacts: create output dir: %w", err)
	}

	if err := os.WriteFile(outputPath, []byte(result.Markdown), 0o600); err != nil {
		return fmt.Errorf("merge artifacts: write %s: %w", outputPath, err)
	}

	emitFileWriteWarning(ctx, state.hookRunner, state.sessionState, outputPath, state.selectedAgent, "merged-artifacts")
	fmt.Println("Merged artifacts into " + outputPath)

	return nil
}

func recordArtifact(
	store *session.Store,
	sessionState session.Session,
	path string,
	kind string,
	summary string,
	sourceAgent string,
) error {
	if !sessionState.RecordArtifact(path, kind, summary, sourceAgent) {
		return errors.New("record artifact: path and kind are required")
	}

	if err := store.Save(sessionState); err != nil {
		return fmt.Errorf("record artifact: save session: %w", err)
	}

	fmt.Println("Recorded artifact on session " + sessionState.ID)

	return nil
}
