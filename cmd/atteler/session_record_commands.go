package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tommoulard/atteler/pkg/artifactmerge"
	"github.com/tommoulard/atteler/pkg/autonomy"
	atteval "github.com/tommoulard/atteler/pkg/eval"
	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/permission"
	"github.com/tommoulard/atteler/pkg/session"
	"github.com/tommoulard/atteler/pkg/watch"
)

func recordFailure(
	ctx context.Context,
	store *session.Store,
	sessionState session.Session,
	approach string,
	reason string,
	commit string,
	agentName string,
) error {
	return recordFailureDetails(ctx, store, sessionState, session.NegativeKnowledge{
		Approach: approach,
		Reason:   reason,
		Commit:   commit,
		Agent:    agentName,
	})
}

func recordFailureDetails(
	ctx context.Context,
	store *session.Store,
	sessionState session.Session,
	failure session.NegativeKnowledge,
) error {
	if !sessionState.RecordNegativeKnowledgeDetails(failure) {
		return errors.New("record failure: approach and reason are required, or this failure is already recorded")
	}

	if err := authorizeSessionStoreWrite(ctx, store, sessionState, "record session failure"); err != nil {
		return fmt.Errorf("record failure: %w", err)
	}

	if err := store.Save(sessionState); err != nil {
		return fmt.Errorf("record failure: save session: %w", err)
	}

	fmt.Println("Recorded failure on session " + sessionState.ID)

	return nil
}

func recordEvaluation(
	ctx context.Context,
	store *session.Store,
	sessionState session.Session,
	agentName string,
	outcome string,
	notes string,
	reference string,
	score int,
) error {
	return recordEvaluationDetails(ctx, store, sessionState, session.AgentEvaluation{
		Agent:     agentName,
		Outcome:   outcome,
		Notes:     notes,
		Reference: reference,
		Score:     score,
	})
}

func recordEvaluationDetails(
	ctx context.Context,
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

	if err := authorizeSessionStoreWrite(ctx, store, sessionState, "record session evaluation"); err != nil {
		return fmt.Errorf("record evaluation: %w", err)
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

func evaluationFromSessionWriteInput(input sessionWriteCommandInput, state appState) (session.AgentEvaluation, error) {
	evaluation := session.AgentEvaluation{
		Agent:           input.RecordEvaluation,
		Outcome:         input.EvaluationOutcome,
		Notes:           input.EvaluationNotes,
		Reference:       input.EvaluationReference,
		Source:          input.EvaluationSource,
		Evaluator:       input.EvaluationEvaluator,
		RubricVersion:   input.EvaluationRubricVersion,
		TaskType:        input.EvaluationTaskType,
		Difficulty:      input.EvaluationDifficulty,
		ExpectedOutcome: input.EvaluationExpectedOutcome,
		Provider:        input.EvaluationProvider,
		Model:           strings.TrimSpace(input.EvaluationModel),
		FixtureVersion:  input.EvaluationFixtureVersion,
		AgentVersion:    input.EvaluationAgentVersion,
		Score:           input.EvaluationScore,
		PassRate:        input.EvaluationPassRate,
		HasPassRate:     input.evaluationPassRateSet,
		FlakeCount:      input.EvaluationFlakeCount,
		DurationMillis:  int64(input.EvaluationDurationMillis),
		InputTokens:     input.EvaluationInputTokens,
		OutputTokens:    input.EvaluationOutputTokens,
		TotalTokens:     input.EvaluationTotalTokens,
		Cost:            input.EvaluationCost,
		Confidence:      input.EvaluationConfidence,
	}

	if strings.TrimSpace(input.EvaluationReportPath) != "" {
		report, err := readEvaluationReport(input.EvaluationReportPath)
		if err != nil {
			return session.AgentEvaluation{}, err
		}

		applyEvaluationReport(&evaluation, report, input.evaluationPassRateSet)

		if strings.TrimSpace(evaluation.Reference) == "" {
			evaluation.Reference = input.EvaluationReportPath
		}
	}

	if strings.TrimSpace(evaluation.Model) == "" {
		evaluation.Model = evaluationModelForRecord("", state)
	}

	return evaluation, nil
}

func readEvaluationReport(path string) (atteval.Report, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return atteval.Report{}, fmt.Errorf("record evaluation: read eval report %s: %w", path, err)
	}

	var report atteval.Report
	if err := json.Unmarshal(data, &report); err != nil {
		return atteval.Report{}, fmt.Errorf("record evaluation: parse eval report %s: %w", path, err)
	}

	return report, nil
}

func applyEvaluationReport(evaluation *session.AgentEvaluation, report atteval.Report, passRateExplicit bool) {
	applyEvaluationReportProvenance(evaluation)
	applyEvaluationReportOutcomeAndMetadata(evaluation, report, passRateExplicit)
	applyEvaluationReportMetrics(evaluation, report)
}

func applyEvaluationReportProvenance(evaluation *session.AgentEvaluation) {
	if strings.TrimSpace(evaluation.Source) == "" {
		evaluation.Source = session.EvaluationSourceHarness
	}

	if strings.TrimSpace(evaluation.Evaluator) == "" {
		evaluation.Evaluator = "atteler-eval"
	}
}

func applyEvaluationReportOutcomeAndMetadata(
	evaluation *session.AgentEvaluation,
	report atteval.Report,
	passRateExplicit bool,
) {
	if strings.TrimSpace(evaluation.Outcome) == "" {
		evaluation.Outcome = "fail"
		if report.Passed {
			evaluation.Outcome = "pass"
		}
	}

	if strings.TrimSpace(evaluation.Reference) == "" {
		evaluation.Reference = evaluationReportReference(report)
	}

	if strings.TrimSpace(evaluation.Provider) == "" {
		evaluation.Provider = report.Metadata.Provider
	}

	if strings.TrimSpace(evaluation.Model) == "" {
		evaluation.Model = report.Metadata.Model
	}

	if strings.TrimSpace(evaluation.FixtureVersion) == "" {
		evaluation.FixtureVersion = report.Metadata.FixtureVersion
	}

	if !passRateExplicit && report.Summary.Total > 0 {
		evaluation.PassRate = report.Summary.PassRate
		evaluation.HasPassRate = true
	}
}

func applyEvaluationReportMetrics(evaluation *session.AgentEvaluation, report atteval.Report) {
	if evaluation.FlakeCount == 0 {
		evaluation.FlakeCount = report.Summary.FlakeCount
	}

	if evaluation.DurationMillis == 0 {
		evaluation.DurationMillis = report.Metrics.LatencyMillis
	}

	if evaluation.InputTokens == 0 {
		evaluation.InputTokens = report.Metrics.InputTokens
	}

	if evaluation.OutputTokens == 0 {
		evaluation.OutputTokens = report.Metrics.OutputTokens
	}

	if evaluation.TotalTokens == 0 {
		evaluation.TotalTokens = report.Metrics.TotalTokens
	}

	if evaluation.Cost == 0 {
		evaluation.Cost = report.Metrics.Cost
	}
}

func evaluationReportReference(report atteval.Report) string {
	reference := firstNonBlankString(report.Suite, report.ActualRef)
	if reference != "" {
		return reference
	}

	for i := range report.Suites {
		if reference = firstNonBlankString(report.Suites[i].Path, report.Suites[i].ActualRef); reference != "" {
			return reference
		}
	}

	return ""
}

func firstNonBlankString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
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

	if evaluation.PassRateRecorded() {
		parts = append(parts, fmt.Sprintf("pass_rate=%.2f", evaluation.PassRate))
	}

	if evaluation.FlakeCount != 0 {
		parts = append(parts, "flake_count="+strconv.Itoa(evaluation.FlakeCount))
	}

	if evaluation.TotalTokens != 0 {
		parts = append(parts, "total_tokens="+strconv.Itoa(evaluation.TotalTokens))
	}

	if evaluation.InputTokens != 0 || evaluation.OutputTokens != 0 {
		parts = append(parts, fmt.Sprintf("tokens=input:%d,output:%d", evaluation.InputTokens, evaluation.OutputTokens))
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
		{key: "provider", value: evaluation.Provider},
		{key: "model", value: evaluation.Model},
		{key: "fixture_version", value: evaluation.FixtureVersion},
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

	if artifact.LogicalPath != "" && artifact.LogicalPath != artifact.Path {
		parts = append(parts, "logical_path="+artifact.LogicalPath)
	}

	if artifact.SourceSessionID != "" {
		parts = append(parts, "session="+artifact.SourceSessionID)
	}

	if artifact.SourceTurn != 0 {
		parts = append(parts, "turn="+strconv.Itoa(artifact.SourceTurn))
	}

	if artifact.SourceCommand != "" {
		parts = append(parts, "command="+artifact.SourceCommand)
	}

	if artifact.SourceCommit != "" {
		parts = append(parts, "commit="+artifact.SourceCommit)
	}

	if artifact.SHA256 != "" {
		parts = append(parts, "sha256="+artifact.SHA256)
	}

	if artifact.SizeBytes != 0 {
		parts = append(parts, "size="+strconv.FormatInt(artifact.SizeBytes, 10))
	}

	if artifact.ReviewStatus != "" {
		parts = append(parts, "review="+artifact.ReviewStatus)
	}

	if artifact.Summary != "" {
		parts = append(parts, "summary="+artifact.Summary)
	}

	return strings.Join(parts, "	")
}

func mergeArtifacts(ctx context.Context, state appState, outputPath, outputFormat string, maxBytes int) error {
	if maxBytes == 0 {
		maxBytes = int(watch.DefaultLargeFileBytes)
	}

	if err := authorizeReadPermission(ctx, "read merged artifacts", "atteler.artifacts.merge", state.cwd); err != nil {
		return fmt.Errorf("merge artifacts: %w", err)
	}

	result, err := artifactmerge.Merge(state.cwd, state.sessionState.Artifacts, int64(maxBytes))
	if err != nil {
		return fmt.Errorf("merge artifacts: %w", err)
	}

	consumedAt := time.Now().UTC()
	result.MarkConsumedAt(consumedAt)

	for i := range result.Warnings {
		warning := &result.Warnings[i]
		fmt.Fprintf(os.Stderr, "warning: artifact %s %s: %s\n", warning.Path, warning.Code, warning.Reason)
	}

	data, err := renderMergedArtifactOutput(result, outputFormat)
	if err != nil {
		return err
	}

	if strings.TrimSpace(outputPath) == "-" {
		if err := persistMergedArtifactConsumption(ctx, state.sessionStore, &state.sessionState, result.Entries, consumedAt); err != nil {
			return err
		}

		fmt.Print(string(data))

		return nil
	}

	if err := authorizeWritePermission(ctx, "write merged artifacts", "atteler.artifacts.merge", outputPath); err != nil {
		return fmt.Errorf("merge artifacts: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(outputPath), 0o750); err != nil {
		return fmt.Errorf("merge artifacts: create output dir: %w", err)
	}

	if err := os.WriteFile(outputPath, data, 0o600); err != nil {
		return fmt.Errorf("merge artifacts: write %s: %w", outputPath, err)
	}

	emitFileWriteWarning(ctx, state.hookRunner, state.sessionState, outputPath, state.selectedAgent, "merged-artifacts")

	if err := persistMergedArtifactConsumption(ctx, state.sessionStore, &state.sessionState, result.Entries, consumedAt); err != nil {
		return err
	}

	fmt.Println("Merged artifacts into " + outputPath)
	printAttelerArtifactPrivacyHint(outputPath)

	return nil
}

func persistMergedArtifactConsumption(
	ctx context.Context,
	store *session.Store,
	sessionState *session.Session,
	entries []artifactmerge.Entry,
	consumedAt time.Time,
) error {
	if store == nil || sessionState == nil || len(entries) == 0 {
		return nil
	}

	if err := authorizeSessionStoreWrite(ctx, store, *sessionState, "mark merged artifacts consumed"); err != nil {
		return fmt.Errorf("merge artifacts: save consumed artifacts: %w", err)
	}

	paths := make([]string, 0, len(entries))
	for i := range entries {
		paths = append(paths, entries[i].Path)
	}

	if sessionState.MarkArtifactsConsumed(paths, consumedAt) == 0 {
		return nil
	}

	if err := store.Save(*sessionState); err != nil {
		return fmt.Errorf("merge artifacts: save consumed artifacts: %w", err)
	}

	return nil
}

func renderMergedArtifactOutput(result artifactmerge.Result, outputFormat string) ([]byte, error) {
	switch strings.ToLower(strings.TrimSpace(outputFormat)) {
	case "", "markdown", "md":
		return []byte(result.Markdown), nil
	case "json":
		data, err := result.JSON()
		if err != nil {
			return nil, fmt.Errorf("merge artifacts: render json: %w", err)
		}

		return data, nil
	default:
		return nil, fmt.Errorf("merge artifacts: unsupported format %q", outputFormat)
	}
}

func authorizeSessionStoreWrite(ctx context.Context, store *session.Store, sessionState session.Session, action string) error {
	return authorizeWritePermission(ctx, action, "atteler.session", sessionWriteTarget(store, sessionState))
}

func authorizeSessionStoreRead(ctx context.Context, store *session.Store, ref, action string) error {
	return authorizeReadPermission(ctx, action, "atteler.session", sessionReadTarget(store, ref))
}

func sessionReadTarget(store *session.Store, ref string) string {
	ref = strings.TrimSpace(ref)
	if store != nil {
		if ref != "" {
			return store.Path(ref)
		}

		return store.Dir()
	}

	if ref != "" {
		return ref
	}

	return "session store"
}

func sessionWriteTarget(store *session.Store, sessionState session.Session) string {
	if store != nil && strings.TrimSpace(sessionState.ID) != "" {
		return store.Path(sessionState.ID)
	}

	if strings.TrimSpace(sessionState.ID) != "" {
		return sessionState.ID
	}

	return "session store"
}

func authorizeRecordArtifactSideEffects(ctx context.Context, store *session.Store, sessionState session.Session, artifactPath string) error {
	writeTarget := sessionWriteTarget(store, sessionState)
	action := "record session artifact"

	decision := permission.Evaluate(ctx, nil, permission.Request{
		Action: action,
		Source: "atteler.artifacts.record",
		Target: artifactPath,
		Operations: []permission.Operation{
			{
				Kind:   permission.OperationRead,
				Action: "read session artifact",
				Source: "atteler.artifacts.record",
				Target: artifactPath,
			},
			{
				Kind:   permission.OperationWrite,
				Action: action,
				Source: "atteler.session",
				Target: writeTarget,
			},
		},
	})
	if decision.Allowed {
		return nil
	}

	return &permission.Error{Decision: decision}
}

func recordArtifact(
	ctx context.Context,
	store *session.Store,
	sessionState session.Session,
	cwd string,
	path string,
	kind string,
	logicalPath string,
	reviewStatus string,
	summary string,
	sourceAgent string,
	level autonomy.Level,
) error {
	if err := authorizeRecordArtifactSideEffects(ctx, store, sessionState, path); err != nil {
		return fmt.Errorf("record artifact: %w", err)
	}

	artifact, err := artifactmerge.CaptureArtifact(ctx, cwd, sessionState, path, kind, summary, sourceAgent, artifactmerge.CaptureOptions{
		MaxBytes:      watch.DefaultLargeFileBytes,
		LogicalPath:   logicalPath,
		SourceCommand: "record-artifact",
		SourceTool:    "atteler",
		Autonomy:      autonomy.Normalize(level).String(),
	})
	if err != nil {
		return fmt.Errorf("record artifact: %w", err)
	}

	artifact.ReviewStatus = strings.TrimSpace(reviewStatus)

	if !sessionState.AddArtifact(artifact) {
		return errors.New("record artifact: path and kind are required")
	}

	if err := store.Save(sessionState); err != nil {
		return fmt.Errorf("record artifact: save session: %w", err)
	}

	fmt.Println("Recorded artifact on session " + sessionState.ID)

	return nil
}
