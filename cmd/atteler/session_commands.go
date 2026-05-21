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

func listHeadlessRuns(store *session.Store) error {
	runs, err := store.ListHeadlessRuns()
	if err != nil {
		return fmt.Errorf("list headless sessions: %w", err)
	}

	active := make([]session.HeadlessRun, 0, len(runs))
	for i := range runs {
		run := &runs[i]
		if run.Status == session.HeadlessStatusRunning {
			active = append(active, *run)
		}
	}

	if len(active) == 0 {
		fmt.Println("No active headless sessions found.")
		return nil
	}

	for i := range active {
		fmt.Println(formatHeadlessRun(active[i]))
	}

	return nil
}

func streamHeadlessLog(ctx context.Context, store *session.Store, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("stream headless: id is required")
	}

	offset := 0

	for {
		text, err := store.ReadHeadlessLog(id)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("stream headless: %w", err)
		}

		if len(text) > offset {
			fmt.Print(text[offset:])
			offset = len(text)
		}

		run, err := store.LoadHeadlessRun(id)
		if err != nil {
			return fmt.Errorf("stream headless: %w", err)
		}

		if run.Status != session.HeadlessStatusRunning {
			return nil
		}

		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("stream headless: %w", ctx.Err())
		case <-timer.C:
		}
	}
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
	}
	if summary.ScoredEvaluationCount > 0 {
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

	if !summary.LatestActivity.IsZero() {
		parts = append(parts, "latest="+summary.LatestActivity.UTC().Format(time.RFC3339))
	}

	return strings.Join(parts, "\t")
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

	agentName := fallbackDash(run.Agent)
	modelName := fallbackDash(run.Model)

	parts := []string{
		run.ID,
		"status=" + string(run.Status),
		"session=" + fallbackDash(run.SessionID),
		"agent=" + agentName,
		"model=" + modelName,
		"started=" + started,
		"updated=" + updated,
		"log=" + fallbackDash(run.LogPath),
	}
	if run.Error != "" {
		parts = append(parts, "error="+run.Error)
	}

	return strings.Join(parts, "\t")
}

func fallbackDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}

	return value
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
