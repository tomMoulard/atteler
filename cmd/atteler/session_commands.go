package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/tommoulard/atteler/pkg/events"
	"github.com/tommoulard/atteler/pkg/session"
)

const (
	headlessStreamPollInterval    = time.Second
	headlessTerminalDrainInterval = 100 * time.Millisecond
)

func listSessions(store *session.Store, tag string) error {
	summaries, err := listSessionSummaries(store, tag)
	if err != nil {
		return fmt.Errorf("list sessions: %w", err)
	}

	if len(summaries) == 0 {
		fmt.Println("No sessions found.")
		return nil
	}

	for i := range summaries {
		fmt.Println(formatSessionSummary(summaries[i]))
	}

	return nil
}

func headlessCommandRequested(opts cliOptions) bool {
	return opts.listHeadless ||
		opts.recoverHeadless ||
		opts.statusHeadlessID != "" ||
		opts.cancelHeadlessID != "" ||
		opts.streamHeadlessID != ""
}

func runHeadlessCommand(ctx context.Context, opts cliOptions, store *session.Store) error {
	if headlessCommandCount(opts) > 1 {
		return errors.New("headless command: choose only one of list, recover, status, cancel, or stream")
	}

	switch {
	case opts.statusHeadlessID != "":
		return statusHeadlessRun(store, opts.statusHeadlessID)
	case opts.cancelHeadlessID != "":
		return cancelHeadlessRun(store, opts.cancelHeadlessID)
	case opts.streamHeadlessID != "":
		return streamHeadlessLog(ctx, store, opts.streamHeadlessID)
	case opts.recoverHeadless:
		return recoverHeadlessRuns(store)
	case opts.listHeadless:
		return listHeadlessRuns(store)
	default:
		return nil
	}
}

func headlessCommandCount(opts cliOptions) int {
	count := 0
	if opts.listHeadless {
		count++
	}

	if opts.recoverHeadless {
		count++
	}

	if opts.statusHeadlessID != "" {
		count++
	}

	if opts.cancelHeadlessID != "" {
		count++
	}

	if opts.streamHeadlessID != "" {
		count++
	}

	return count
}

func listHeadlessRuns(store *session.Store) error {
	runs, err := store.ListHeadlessRuns()
	if err != nil {
		return fmt.Errorf("list headless runs: %w", err)
	}

	active := make([]session.HeadlessRun, 0, len(runs))
	for i := range runs {
		run := &runs[i]
		if run.Status == session.HeadlessStatusRunning ||
			run.Status == session.HeadlessStatusStale ||
			run.Status == session.HeadlessStatusOrphaned ||
			run.Status == session.HeadlessStatusCorrupt {
			active = append(active, *run)
		}
	}

	if len(active) == 0 {
		fmt.Println("No active headless runs found.")
		return nil
	}

	for i := range active {
		fmt.Println(formatHeadlessRun(active[i]))
	}

	return nil
}

func recoverHeadlessRuns(store *session.Store) error {
	recovered, err := store.RecoverStaleHeadlessRuns(0)
	if err != nil {
		return fmt.Errorf("recover headless runs: %w", err)
	}

	if len(recovered) == 0 {
		fmt.Println("No recoverable stale/orphaned headless runs found.")
		return nil
	}

	for i := range recovered {
		fmt.Println(formatHeadlessRun(recovered[i]))
	}

	return nil
}

func statusHeadlessRun(store *session.Store, id string) error {
	if id == "" {
		return errors.New("status headless: id is required")
	}

	run, err := store.HeadlessRunStatus(id)
	if err != nil {
		return fmt.Errorf("status headless: %w", err)
	}

	fmt.Println(formatHeadlessRun(run))

	return nil
}

func cancelHeadlessRun(store *session.Store, id string) error {
	if id == "" {
		return errors.New("cancel headless: id is required")
	}

	run, err := store.CancelHeadlessRun(id, "canceled by atteler session cancel-headless")
	if err != nil {
		return fmt.Errorf("cancel headless: %w", err)
	}

	fmt.Println(formatHeadlessRun(run))

	return nil
}

func streamHeadlessLog(ctx context.Context, store *session.Store, id string) error {
	if id == "" {
		return errors.New("stream headless: id is required")
	}

	offset := session.HeadlessLogOffset{}

	for {
		tail, err := store.TailHeadlessLog(id, session.HeadlessLogTailOptions{Offset: offset})
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("stream headless: %w", err)
		}

		if tail.Text != "" {
			fmt.Print(tail.Text)
		}

		offset = tail.NextOffset

		run, err := store.HeadlessRunStatus(id)
		if err != nil {
			return fmt.Errorf("stream headless: %w", err)
		}

		if headlessRunRecordingIsTerminal(run.Status) {
			return drainTerminalHeadlessLogTail(ctx, store, id, offset)
		}

		timer := time.NewTimer(headlessStreamPollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("stream headless: %w", ctx.Err())
		case <-timer.C:
		}
	}
}

func drainTerminalHeadlessLogTail(ctx context.Context, store *session.Store, id string, offset session.HeadlessLogOffset) error {
	nextOffset := offset

	for {
		drainedOffset, wrote, err := drainHeadlessLogTail(store, id, nextOffset)
		if err != nil {
			return err
		}

		nextOffset = drainedOffset

		if !wrote {
			break
		}
	}

	timer := time.NewTimer(headlessTerminalDrainInterval)
	select {
	case <-ctx.Done():
		timer.Stop()
		return fmt.Errorf("stream headless: %w", ctx.Err())
	case <-timer.C:
	}

	for {
		drainedOffset, wrote, err := drainHeadlessLogTail(store, id, nextOffset)
		if err != nil {
			return err
		}

		nextOffset = drainedOffset

		if !wrote {
			return nil
		}
	}
}

func drainHeadlessLogTail(store *session.Store, id string, offset session.HeadlessLogOffset) (session.HeadlessLogOffset, bool, error) {
	tail, err := store.TailHeadlessLog(id, session.HeadlessLogTailOptions{Offset: offset})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return offset, false, fmt.Errorf("stream headless: %w", err)
	}

	if tail.Text != "" {
		fmt.Print(tail.Text)
	}

	return tail.NextOffset, tail.Text != "", nil
}

func listSessionSummaries(store *session.Store, tag string) ([]session.Summary, error) {
	tag = strings.TrimSpace(tag)
	if tag == "" {
		summaries, err := store.List()
		if err != nil {
			return nil, fmt.Errorf("list all sessions: %w", err)
		}

		return summaries, nil
	}

	summaries, err := store.ListByTag(tag)
	if err != nil {
		return nil, fmt.Errorf("list sessions by tag %q: %w", tag, err)
	}

	return summaries, nil
}

func listAgentPerformance(store *session.Store) error {
	summaries, err := store.AgentPerformanceSummary()
	if err != nil {
		return fmt.Errorf("agent performance summary: %w", err)
	}

	if len(summaries) == 0 {
		fmt.Println("No agent performance records found.")
		return nil
	}

	for i := range summaries {
		fmt.Println(formatAgentPerformanceSummary(summaries[i]))
	}

	return nil
}

func formatAgentPerformanceSummary(summary session.AgentPerformanceSummary) string {
	parts := []string{
		"agent=" + summary.Agent,
		"evaluations=" + strconv.Itoa(summary.EvaluationCount),
		"failures=" + strconv.Itoa(summary.FailureCount),
		"negative_knowledge=" + strconv.Itoa(summary.NegativeKnowledgeCount),
		"default_agent_sessions=" + strconv.Itoa(summary.DefaultAgentSessionCount),
		"routing_eligible=" + strconv.FormatBool(summary.Validity.RoutingEligible),
		"recency_window_days=" + strconv.Itoa(summary.RecentWindowDays),
	}
	if len(summary.EvaluationProvenance) > 0 {
		parts = append(parts, "provenance="+formatProvenanceCounts(summary.EvaluationProvenance))
	}

	if len(summary.RubricVersions) > 0 {
		parts = append(parts, "rubrics="+formatRubricVersionCounts(summary.RubricVersions))
	}

	if len(summary.Evaluators) > 0 {
		parts = append(parts, "evaluators="+formatEvaluatorCounts(summary.Evaluators))
	}

	if len(summary.ScoreBuckets) > 0 {
		parts = append(parts, "score_buckets="+formatScoreBuckets(summary.ScoreBuckets))
	}

	if summary.ScoredEvaluationCount > 0 && len(summary.ScoreBuckets) == 1 {
		parts = append(
			parts,
			"scored="+strconv.Itoa(summary.ScoredEvaluationCount),
			fmt.Sprintf("avg_score=%.2f", summary.AverageScore),
			"min_score="+strconv.Itoa(summary.MinScore),
			"max_score="+strconv.Itoa(summary.MaxScore),
		)
	}

	if len(summary.Outcomes) > 0 {
		parts = append(parts, "outcomes="+formatOutcomeCounts(summary.Outcomes))
	}

	if len(summary.NegativeKnowledgeBreakdown) > 0 {
		parts = append(parts, "negative_knowledge_breakdown="+formatNegativeKnowledgeBreakdown(summary.NegativeKnowledgeBreakdown))
	}

	parts = append(parts, formatPerformanceValidity(summary.Validity)...)

	if len(summary.Validity.Checks) > 0 {
		parts = append(parts, "validity_checks="+strings.Join(summary.Validity.Checks, ","))
	}

	if len(summary.Validity.Reasons) > 0 {
		parts = append(parts, "validity_reasons="+strings.Join(summary.Validity.Reasons, ","))
	}

	if !summary.LatestActivity.IsZero() {
		parts = append(parts, "latest="+summary.LatestActivity.UTC().Format(time.RFC3339))
	}

	return strings.Join(parts, "\t")
}

func formatPerformanceValidity(validity session.PerformanceValidity) []string {
	if validity.MinimumSampleSize == 0 &&
		validity.MinimumRecentSamples == 0 &&
		validity.MaximumStandardError == 0 &&
		validity.MinimumMeanConfidence == 0 {
		return nil
	}

	return []string{
		"validity_eligible_buckets=" + strconv.Itoa(validity.EligibleScoreBuckets),
		"validity_min_sample_size=" + strconv.Itoa(validity.MinimumSampleSize),
		"validity_min_recent_samples=" + strconv.Itoa(validity.MinimumRecentSamples),
		fmt.Sprintf("validity_max_stderr=%.2f", validity.MaximumStandardError),
		fmt.Sprintf("validity_min_confidence=%.2f", validity.MinimumMeanConfidence),
	}
}

func formatProvenanceCounts(counts []session.ProvenanceCount) string {
	parts := make([]string, 0, len(counts))
	for _, count := range counts {
		parts = append(parts, count.Source+":"+strconv.Itoa(count.Count))
	}

	return strings.Join(parts, ",")
}

func formatRubricVersionCounts(counts []session.RubricVersionCount) string {
	parts := make([]string, 0, len(counts))
	for _, count := range counts {
		parts = append(parts, count.RubricVersion+":"+strconv.Itoa(count.Count))
	}

	return strings.Join(parts, ",")
}

func formatEvaluatorCounts(counts []session.EvaluatorCount) string {
	parts := make([]string, 0, len(counts))
	for _, count := range counts {
		parts = append(parts, count.Evaluator+":"+strconv.Itoa(count.Count))
	}

	return strings.Join(parts, ",")
}

func formatScoreBuckets(buckets []session.ScoreBucketSummary) string {
	parts := make([]string, 0, len(buckets))
	for i := range buckets {
		bucket := &buckets[i]

		fields := []string{
			"source=" + bucket.Source,
			"rubric=" + bucket.RubricVersion,
			"task=" + bucket.TaskType,
			"difficulty=" + bucket.Difficulty,
			"model=" + bucket.Model,
			"agent_version=" + bucket.AgentVersion,
			"routing_eligible=" + strconv.FormatBool(bucket.RoutingEligible),
			"sample=" + strconv.Itoa(bucket.SampleSize),
			fmt.Sprintf("avg=%.2f", bucket.AverageScore),
			fmt.Sprintf("ci95=%.2f..%.2f", bucket.ConfidenceIntervalLow, bucket.ConfidenceIntervalHigh),
			fmt.Sprintf("stderr=%.2f", bucket.StandardError),
			"uncertainty=" + bucket.Uncertainty,
			"recent_sample=" + strconv.Itoa(bucket.RecentSampleSize),
			fmt.Sprintf("recent_avg=%.2f", bucket.RecentAverageScore),
			"previous_sample=" + strconv.Itoa(bucket.PreviousSampleSize),
			fmt.Sprintf("previous_avg=%.2f", bucket.PreviousAverageScore),
			"regression=" + bucket.RegressionStatus,
		}
		if bucket.RecentSampleSize > 0 && bucket.PreviousSampleSize > 0 {
			fields = append(fields, fmt.Sprintf("regression_delta=%.2f", bucket.RegressionDelta))
		}

		if !bucket.LatestScoreAt.IsZero() {
			fields = append(fields, "latest_score="+bucket.LatestScoreAt.UTC().Format(time.RFC3339))
		}

		if !bucket.RecentWindowStart.IsZero() {
			fields = append(fields, "recent_since="+bucket.RecentWindowStart.UTC().Format(time.RFC3339))
		}

		if bucket.ConfidenceSampleCount > 0 {
			fields = append(fields,
				"confidence_sample="+strconv.Itoa(bucket.ConfidenceSampleCount),
				fmt.Sprintf("avg_confidence=%.2f", bucket.AverageConfidence),
			)
		}

		if bucket.DurationSampleCount > 0 {
			fields = append(fields,
				"duration_sample="+strconv.Itoa(bucket.DurationSampleCount),
				fmt.Sprintf("avg_duration_ms=%.2f", bucket.AverageDurationMillis),
			)
		}

		if bucket.CostSampleCount > 0 {
			fields = append(fields,
				"cost_sample="+strconv.Itoa(bucket.CostSampleCount),
				fmt.Sprintf("total_cost=%.6f", bucket.TotalCost),
				fmt.Sprintf("avg_cost=%.6f", bucket.AverageCost),
			)
		}

		if len(bucket.ValidityReasons) > 0 {
			fields = append(fields, "validity_reasons="+strings.Join(bucket.ValidityReasons, "|"))
		}

		parts = append(parts, strings.Join(fields, "/"))
	}

	return strings.Join(parts, ";")
}

func formatNegativeKnowledgeBreakdown(counts []session.NegativeKnowledgeCategoryCount) string {
	parts := make([]string, 0, len(counts))
	for _, count := range counts {
		parts = append(parts, count.TaskType+"/"+count.Severity+":"+strconv.Itoa(count.Count))
	}

	return strings.Join(parts, ",")
}

func formatOutcomeCounts(outcomes []session.OutcomeCount) string {
	parts := make([]string, 0, len(outcomes))
	for _, outcome := range outcomes {
		parts = append(parts, outcome.Outcome+":"+strconv.Itoa(outcome.Count))
	}

	return strings.Join(parts, ",")
}

func listSessionTags(store *session.Store) error {
	tags, err := store.Tags()
	if err != nil {
		return fmt.Errorf("list session tags: %w", err)
	}

	if len(tags) == 0 {
		fmt.Println("No session tags found.")
		return nil
	}

	for _, tag := range tags {
		fmt.Println(formatTagSummary(tag))
	}

	return nil
}

func formatTagSummary(tag session.TagSummary) string {
	return fmt.Sprintf("%s\t%d sessions", tag.Tag, tag.Sessions)
}

func listHookEvents(jsonOutput bool) error {
	if jsonOutput {
		if err := json.NewEncoder(os.Stdout).Encode(events.SupportedEventTypes()); err != nil {
			return fmt.Errorf("list hook events: encode JSON: %w", err)
		}

		return nil
	}

	for _, eventType := range events.SupportedEventTypes() {
		fmt.Println(formatHookEventType(eventType))
	}

	return nil
}

func formatHookEventType(eventType events.SupportedEventType) string {
	return eventType.Type + "\t" + eventType.Description
}

func searchSessions(store *session.Store, query string) error {
	results, err := store.Search(query)
	if err != nil {
		return fmt.Errorf("search sessions: %w", err)
	}

	if len(results) == 0 {
		fmt.Println("No matching sessions found.")
		return nil
	}

	for i := range results {
		result := &results[i]
		fmt.Println(formatSessionSummary(result.Summary))

		for _, snippet := range result.Snippets {
			fmt.Println(formatSearchSnippet(snippet))
		}
	}

	return nil
}

func formatSessionSummary(summary session.Summary) string {
	updated := "-"
	if !summary.UpdatedAt.IsZero() {
		updated = summary.UpdatedAt.UTC().Format(time.RFC3339)
	}

	agentName := "-"
	if summary.DefaultAgent != "" {
		agentName = summary.DefaultAgent
	}

	modelName := "-"
	if summary.DefaultModel != "" {
		modelName = summary.DefaultModel
	}

	parts := []string{
		summary.ID,
		updated,
		fmt.Sprintf("%d messages", summary.Messages),
		"agent=" + agentName,
		"model=" + modelName,
	}
	if summary.Title != "" {
		parts = append(parts, "title="+summary.Title)
	}

	if len(summary.Tags) > 0 {
		parts = append(parts, "tags="+strings.Join(summary.Tags, ","))
	}

	parts = append(parts, summary.Path)

	return strings.Join(parts, "\t")
}

func formatHeadlessRun(run session.HeadlessRun) string {
	started := "-"
	if !run.StartedAt.IsZero() {
		started = run.StartedAt.UTC().Format(time.RFC3339)
	}

	updated := "-"
	if !run.UpdatedAt.IsZero() {
		updated = run.UpdatedAt.UTC().Format(time.RFC3339)
	}

	heartbeat := "-"
	if !run.LastHeartbeatAt.IsZero() {
		heartbeat = run.LastHeartbeatAt.UTC().Format(time.RFC3339)
	}

	agentName := fallbackDash(run.Agent)
	modelName := fallbackDash(run.Model)

	parts := []string{
		formatHeadlessFieldValue(run.ID),
		"status=" + string(run.Status),
		"session=" + fallbackDash(run.SessionID),
		"agent=" + agentName,
		"model=" + modelName,
		"started=" + started,
		"updated=" + updated,
		"heartbeat=" + heartbeat,
		"log=" + fallbackDash(run.LogPath),
	}

	parts = appendHeadlessRunTimeDetails(parts, run)
	parts = appendHeadlessRunProcessDetails(parts, run)
	parts = appendHeadlessRunStorageDetails(parts, run)
	parts = appendHeadlessRunTerminalDetails(parts, run)

	return strings.Join(parts, "\t")
}

func appendHeadlessRunTimeDetails(parts []string, run session.HeadlessRun) []string {
	if run.EventsPath != "" {
		parts = append(parts, "events="+formatHeadlessFieldValue(run.EventsPath))
	}

	if run.CompletedAt != nil {
		parts = append(parts, "completed="+run.CompletedAt.UTC().Format(time.RFC3339))
	}

	if run.CanceledAt != nil {
		parts = append(parts, "canceled="+run.CanceledAt.UTC().Format(time.RFC3339))
	}

	return parts
}

func appendHeadlessRunProcessDetails(parts []string, run session.HeadlessRun) []string {
	if run.PID != 0 {
		parts = append(parts, "pid="+strconv.Itoa(run.PID))
	}

	if run.ParentPID != 0 {
		parts = append(parts, "ppid="+strconv.Itoa(run.ParentPID))
	}

	if run.ProcessGroupID != 0 {
		parts = append(parts, "pgid="+strconv.Itoa(run.ProcessGroupID))
	}

	if run.ParentRunID != "" {
		parts = append(parts, "parent_run="+formatHeadlessFieldValue(run.ParentRunID))
	}

	if len(run.ChildRunIDs) > 0 {
		parts = append(parts, "child_runs="+formatHeadlessArgs(run.ChildRunIDs))
	}

	if run.StartMethod != "" {
		parts = append(parts, "start_method="+formatHeadlessFieldValue(run.StartMethod))
	}

	if run.StartedCommand != "" {
		parts = append(parts, "command="+formatHeadlessFieldValue(run.StartedCommand))
	}

	if len(run.CommandArgs) > 0 {
		parts = append(parts, "command_args="+formatHeadlessArgs(run.CommandArgs))
	}

	if run.Hostname != "" {
		parts = append(parts, "host="+formatHeadlessFieldValue(run.Hostname))
	}

	if run.CWD != "" {
		parts = append(parts, "cwd="+formatHeadlessFieldValue(run.CWD))
	}

	if run.ExitCode != nil {
		parts = append(parts, "exit_code="+strconv.Itoa(*run.ExitCode))
	}

	return parts
}

func appendHeadlessRunStorageDetails(parts []string, run session.HeadlessRun) []string {
	if run.LogPath != "" {
		parts = append(parts, "log_chunk_pattern="+formatHeadlessFieldValue(run.LogPath)+".NNNNNN")
	}

	if run.ArtifactDir != "" {
		parts = append(parts, "artifacts="+formatHeadlessFieldValue(run.ArtifactDir))
	}

	if run.LogMaxChunkBytes != 0 {
		parts = append(parts, "log_max_chunk_bytes="+strconv.FormatInt(run.LogMaxChunkBytes, 10))
	}

	if run.LogMaxChunks != 0 {
		parts = append(parts, "log_max_chunks="+strconv.Itoa(run.LogMaxChunks))
	}

	return parts
}

func appendHeadlessRunTerminalDetails(parts []string, run session.HeadlessRun) []string {
	if run.StaleReason != "" {
		parts = append(parts, "stale_reason="+formatHeadlessFieldValue(run.StaleReason))
	}

	if run.OrphanedReason != "" {
		parts = append(parts, "orphaned_reason="+formatHeadlessFieldValue(run.OrphanedReason))
	}

	if run.CancellationReason != "" {
		parts = append(parts, "cancellation_reason="+formatHeadlessFieldValue(run.CancellationReason))
	}

	if run.TerminalReason != "" {
		parts = append(parts, "terminal_reason="+formatHeadlessFieldValue(run.TerminalReason))
	}

	if run.Status == session.HeadlessStatusStale || run.Status == session.HeadlessStatusOrphaned {
		parts = append(parts, "recover=atteler session recover-headless")
	}

	if run.Error != "" {
		parts = append(parts, "error="+formatHeadlessFieldValue(run.Error))
	}

	return parts
}

func fallbackDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}

	return formatHeadlessFieldValue(value)
}

func formatHeadlessFieldValue(value string) string {
	return strings.NewReplacer(
		"\t", " ",
		"\r", "\\r",
		"\n", "\\n",
	).Replace(value)
}

func formatHeadlessArgs(args []string) string {
	data, err := json.Marshal(args)
	if err != nil {
		return formatHeadlessFieldValue(strings.Join(args, " "))
	}

	return formatHeadlessFieldValue(string(data))
}

func formatSearchSnippet(snippet session.SearchSnippet) string {
	role := string(snippet.Role)
	if role == "" {
		role = "message"
	}

	if snippet.Text == "" {
		return "  " + role + ":"
	}

	return "  " + role + ": " + snippet.Text
}
