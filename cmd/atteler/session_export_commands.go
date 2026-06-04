package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/session"
)

type sessionDetails struct {
	CreatedAt         time.Time                   `yaml:"created_at"`
	UpdatedAt         time.Time                   `yaml:"updated_at"`
	ID                string                      `yaml:"id"`
	Path              string                      `yaml:"path"`
	Title             string                      `yaml:"title,omitempty"`
	DefaultAgent      string                      `yaml:"default_agent,omitempty"`
	DefaultModel      string                      `yaml:"default_model,omitempty"`
	DefaultReasoning  string                      `yaml:"default_reasoning_level,omitempty"`
	DefaultModelMode  string                      `yaml:"default_model_mode,omitempty"`
	Autonomy          string                      `yaml:"autonomy,omitempty"`
	AgentLoopBudget   *agentLoopBudgetDetails     `yaml:"agent_loop_budget,omitempty"`
	WorktreePath      string                      `yaml:"worktree_path,omitempty"`
	WorktreeBranch    string                      `yaml:"worktree_branch,omitempty"`
	WorktreeBase      string                      `yaml:"worktree_base,omitempty"`
	Tags              []string                    `yaml:"tags,omitempty"`
	Messages          []yamlMessage               `yaml:"messages,omitempty"`
	NegativeKnowledge []session.NegativeKnowledge `yaml:"negative_knowledge,omitempty"`
	Evaluations       []session.AgentEvaluation   `yaml:"evaluations,omitempty"`
	Artifacts         []session.Artifact          `yaml:"artifacts,omitempty"`
	MultiAgentRuns    []session.MultiAgentRun     `yaml:"multi_agent_runs,omitempty"`
}

//nolint:govet // YAML output order mirrors the documented agent_loop config schema.
type agentLoopBudgetDetails struct {
	MaxOutputBytes  int64  `yaml:"max_output_bytes"`
	MaxCostMicros   int64  `yaml:"max_cost_micros"`
	MaxInputTokens  int    `yaml:"max_input_tokens"`
	MaxOutputTokens int    `yaml:"max_output_tokens"`
	MaxTotalTokens  int    `yaml:"max_total_tokens"`
	MaxIterations   int    `yaml:"max_iterations"`
	MaxModelCalls   int    `yaml:"max_model_calls"`
	MaxToolCalls    int    `yaml:"max_tool_calls"`
	MaxWallTime     string `yaml:"max_wall_time"`
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
		"multi_agent_runs=" + strconv.Itoa(len(sessionState.MultiAgentRuns)),
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

	if sessionState.DefaultModelMode != "" {
		parts = append(parts, "mode="+sessionState.DefaultModelMode)
	}

	if sessionState.Autonomy != "" {
		parts = append(parts, "autonomy="+sessionState.Autonomy)
	}

	if budget := formatAgentLoopBudgetCompact(sessionState.AgentLoopBudget); budget != "" {
		parts = append(parts, "budget="+budget)
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
		DefaultModelMode: sessionState.DefaultModelMode,
		Autonomy:         sessionState.Autonomy,
		AgentLoopBudget:  sessionAgentLoopBudgetDetails(sessionState.AgentLoopBudget),
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
		MultiAgentRuns: append(
			[]session.MultiAgentRun(nil),
			sessionState.MultiAgentRuns...,
		),
	})
	if err != nil {
		return "", fmt.Errorf("marshal session details: %w", err)
	}

	return string(out), nil
}

func sessionAgentLoopBudgetDetails(budget llm.AgentLoopBudget) *agentLoopBudgetDetails {
	if budget.IsZero() {
		return nil
	}

	return &agentLoopBudgetDetails{
		MaxOutputBytes:  budget.MaxOutputBytes,
		MaxCostMicros:   budget.MaxCostMicros,
		MaxInputTokens:  budget.MaxInputTokens,
		MaxOutputTokens: budget.MaxOutputTokens,
		MaxTotalTokens:  budget.MaxTotalTokens,
		MaxIterations:   budget.MaxIterations,
		MaxModelCalls:   budget.MaxModelCalls,
		MaxToolCalls:    budget.MaxToolCalls,
		MaxWallTime:     budget.MaxWallTime.String(),
	}
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
	case "", "markdown", "md", "shareable", "redacted", "redacted-markdown":
		fmt.Print(session.Markdown(sessionState))
	case "json", "machine-json", "shareable-json", "redacted-json":
		data, err := session.JSON(sessionState)
		if err != nil {
			return fmt.Errorf("encode session json: %w", err)
		}

		fmt.Print(string(data))
	case "private", "private-markdown", "private-full", "raw", "raw-markdown", "full":
		fmt.Print(session.PrivateMarkdown(sessionState))
	case "private-json", "raw-json", "full-json":
		data, err := session.JSONWithOptions(sessionState, session.ExportOptions{Profile: session.ExportProfilePrivate})
		if err != nil {
			return fmt.Errorf("encode private session json: %w", err)
		}

		fmt.Print(string(data))
	case "issue", "pr", "summary", "issue-markdown", "pr-markdown", "summary-markdown":
		fmt.Print(session.IssueMarkdown(sessionState))
	case "issue-json", "pr-json", "summary-json":
		data, err := session.JSONWithOptions(sessionState, session.ExportOptions{Profile: session.ExportProfileIssue})
		if err != nil {
			return fmt.Errorf("encode issue session json: %w", err)
		}

		fmt.Print(string(data))
	default:
		return fmt.Errorf("unsupported export format %q (supported: markdown, json, private-markdown, private-json, issue, issue-json)", format)
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
