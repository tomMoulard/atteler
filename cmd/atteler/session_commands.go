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

	"gopkg.in/yaml.v3"

	"github.com/tommoulard/atteler/pkg/artifactmerge"
	"github.com/tommoulard/atteler/pkg/events"
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
	if !sessionState.RecordNegativeKnowledge(approach, reason, commit, agentName) {
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
	if !sessionState.RecordEvaluation(agentName, outcome, notes, reference, score) {
		return errors.New("record evaluation: agent and outcome are required")
	}

	if err := store.Save(sessionState); err != nil {
		return fmt.Errorf("record evaluation: save session: %w", err)
	}

	fmt.Println("Recorded evaluation on session " + sessionState.ID)

	return nil
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

	if evaluation.Score != 0 {
		parts = append(parts, "score="+strconv.Itoa(evaluation.Score))
	}

	if evaluation.Reference != "" {
		parts = append(parts, "reference="+evaluation.Reference)
	}

	if evaluation.Notes != "" {
		parts = append(parts, "notes="+evaluation.Notes)
	}

	return strings.Join(parts, "	")
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

type sessionDetails struct {
	CreatedAt         time.Time                   `yaml:"created_at"`
	UpdatedAt         time.Time                   `yaml:"updated_at"`
	ID                string                      `yaml:"id"`
	Path              string                      `yaml:"path"`
	Title             string                      `yaml:"title,omitempty"`
	DefaultAgent      string                      `yaml:"default_agent,omitempty"`
	DefaultModel      string                      `yaml:"default_model,omitempty"`
	DefaultReasoning  string                      `yaml:"default_reasoning_level,omitempty"`
	WorktreePath      string                      `yaml:"worktree_path,omitempty"`
	WorktreeBranch    string                      `yaml:"worktree_branch,omitempty"`
	WorktreeBase      string                      `yaml:"worktree_base,omitempty"`
	Tags              []string                    `yaml:"tags,omitempty"`
	Messages          []yamlMessage               `yaml:"messages,omitempty"`
	NegativeKnowledge []session.NegativeKnowledge `yaml:"negative_knowledge,omitempty"`
	Evaluations       []session.AgentEvaluation   `yaml:"evaluations,omitempty"`
	Artifacts         []session.Artifact          `yaml:"artifacts,omitempty"`
}

type yamlMessage struct {
	Role    llm.Role `yaml:"role"`
	Content string   `yaml:"content"`
}

func printSessionSummary(sessionState session.Session, path string) {
	fmt.Println(formatSessionDetailsSummary(sessionState, path))
}

func formatSessionDetailsSummary(sessionState session.Session, path string) string {
	parts := []string{
		"id=" + sessionState.ID,
		"path=" + path,
		"messages=" + strconv.Itoa(len(sessionState.Messages)),
		"failures=" + strconv.Itoa(len(sessionState.NegativeKnowledge)),
		"evaluations=" + strconv.Itoa(len(sessionState.Evaluations)),
		"artifacts=" + strconv.Itoa(len(sessionState.Artifacts)),
	}
	if !sessionState.CreatedAt.IsZero() {
		parts = append(parts, "created_at="+sessionState.CreatedAt.Format(time.RFC3339))
	}

	if !sessionState.UpdatedAt.IsZero() {
		parts = append(parts, "updated_at="+sessionState.UpdatedAt.Format(time.RFC3339))
	}

	if sessionState.Title != "" {
		parts = append(parts, "title="+sessionState.Title)
	}

	if sessionState.DefaultAgent != "" {
		parts = append(parts, "agent="+sessionState.DefaultAgent)
	}

	if sessionState.DefaultModel != "" {
		parts = append(parts, "model="+sessionState.DefaultModel)
	}

	if sessionState.DefaultReasoningLevel != "" {
		parts = append(parts, "effort="+sessionState.DefaultReasoningLevel)
	}

	if len(sessionState.Tags) > 0 {
		parts = append(parts, "tags="+strings.Join(sessionState.Tags, ","))
	}

	return strings.Join(parts, "	")
}

func showSession(sessionState session.Session, path string) error {
	out, err := formatSessionDetails(sessionState, path)
	if err != nil {
		return fmt.Errorf("format session %q: %w", sessionState.ID, err)
	}

	fmt.Print(out)

	return nil
}

func formatSessionDetails(sessionState session.Session, path string) (string, error) {
	out, err := yaml.Marshal(sessionDetails{
		ID:               sessionState.ID,
		Path:             path,
		Title:            sessionState.Title,
		CreatedAt:        sessionState.CreatedAt,
		UpdatedAt:        sessionState.UpdatedAt,
		DefaultAgent:     sessionState.DefaultAgent,
		DefaultModel:     sessionState.DefaultModel,
		DefaultReasoning: sessionState.DefaultReasoningLevel,
		WorktreePath:     sessionState.WorktreePath,
		WorktreeBranch:   sessionState.WorktreeBranch,
		WorktreeBase:     sessionState.WorktreeBase,
		Tags:             sessionState.Tags,
		Messages:         yamlMessages(sessionState.Messages),
		NegativeKnowledge: append(
			[]session.NegativeKnowledge(nil),
			sessionState.NegativeKnowledge...,
		),
		Evaluations: append([]session.AgentEvaluation(nil), sessionState.Evaluations...),
		Artifacts:   append([]session.Artifact(nil), sessionState.Artifacts...),
	})
	if err != nil {
		return "", fmt.Errorf("marshal session details: %w", err)
	}

	return string(out), nil
}

func yamlMessages(messages []llm.Message) []yamlMessage {
	out := make([]yamlMessage, 0, len(messages))
	for _, message := range messages {
		out = append(out, yamlMessage{
			Role:    message.Role,
			Content: message.Content,
		})
	}

	return out
}

func exportSession(sessionState session.Session, format string) error {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "", "markdown", "md":
		fmt.Print(session.Markdown(sessionState))
	case "json":
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")

		if err := encoder.Encode(sessionState); err != nil {
			return fmt.Errorf("encode session json: %w", err)
		}
	default:
		return fmt.Errorf("unsupported export format %q (supported: markdown, json)", format)
	}

	return nil
}

func printTranscript(sessionState session.Session) {
	for _, msg := range sessionState.Messages {
		switch msg.Role {
		case llm.RoleUser:
			fmt.Println(userLabel.Render("You") + " " + msg.Content)
		case llm.RoleAssistant:
			fmt.Println(assistantLabel.Render("Assistant") + " " + msg.Content)
		default:
			fmt.Println(dimStyle.Render(string(msg.Role)) + " " + msg.Content)
		}
	}
}
