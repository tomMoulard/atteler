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
	if line := formatTranscriptProvenance(sessionState); line != "" {
		fmt.Println(line)
	}

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

func formatTranscriptProvenance(sessionState session.Session) string {
	provenance := session.BuildMachineReadableExport(sessionState, session.ExportOptions{
		Profile: session.ExportProfileIssue,
	}).Provenance

	parts := []string{"Provenance", "config_hash=" + provenance.ConfigHash}
	if len(provenance.Providers) > 0 {
		parts = append(parts, "providers="+strings.Join(provenance.Providers, ","))
	}

	if len(provenance.Models) > 0 {
		parts = append(parts, "models="+strings.Join(provenance.Models, ","))
	}

	if provenance.EventLog != nil {
		if provenance.EventLog.LastHash != "" {
			parts = append(parts, "event_hash="+provenance.EventLog.LastHash)
		}

		if provenance.EventLog.EventCount > 0 {
			parts = append(parts, "events="+strconv.Itoa(provenance.EventLog.EventCount))
		}
	}

	if provenance.TokenUsage.TotalTokens > 0 {
		parts = append(parts, "total_tokens="+strconv.Itoa(provenance.TokenUsage.TotalTokens))
	}

	appendProviderCallTranscriptProvenance(&parts, provenance.ProviderCalls)

	if len(provenance.ReferencedFiles) > 0 {
		parts = append(parts, "files="+strconv.Itoa(len(provenance.ReferencedFiles)))
		if refs := formatTranscriptFileReferences(provenance.ReferencedFiles); refs != "" {
			parts = append(parts, "file_refs="+refs)
		}
	}

	if len(provenance.VerificationGates) > 0 {
		parts = append(parts, "gates="+strconv.Itoa(len(provenance.VerificationGates)))
		if gates := formatTranscriptVerificationGates(provenance.VerificationGates); gates != "" {
			parts = append(parts, "gate_checks="+gates)
		}
	}

	return strings.Join(parts, "\t")
}

func formatTranscriptFileReferences(files []session.ExportFileReference) string {
	if len(files) == 0 {
		return ""
	}

	refs := make([]string, 0, len(files))
	for index := range files {
		file := files[index]

		label := strings.TrimSpace(file.LogicalPath)
		if label == "" {
			label = strings.TrimSpace(file.Path)
		}

		if label == "" {
			continue
		}

		if kind := strings.TrimSpace(file.Kind); kind != "" {
			label = kind + ":" + label
		}

		if hash := strings.TrimSpace(file.SHA256); hash != "" {
			label += "@" + hash
		}

		refs = append(refs, transcriptProvenanceToken(label))
	}

	return strings.Join(refs, ",")
}

func formatTranscriptVerificationGates(gates []session.ExportVerificationGate) string {
	if len(gates) == 0 {
		return ""
	}

	refs := make([]string, 0, len(gates))
	for index := range gates {
		gate := gates[index]

		label := strings.TrimSpace(gate.Name)
		if label == "" {
			continue
		}

		if phase := strings.TrimSpace(gate.Phase); phase != "" {
			label = phase + "/" + label
		}

		if runID := strings.TrimSpace(gate.RunID); runID != "" {
			label = runID + "/" + label
		}

		if agent := strings.TrimSpace(gate.Agent); agent != "" {
			label += "@" + agent
		}

		status := strings.ToLower(gateStatusFail)
		if gate.Passed {
			status = strings.ToLower(gateStatusPass)
		}

		refs = append(refs, transcriptProvenanceToken(label+":"+status))
	}

	return strings.Join(refs, ",")
}

func transcriptProvenanceToken(value string) string {
	value = strings.TrimSpace(strings.Join(strings.Fields(value), " "))
	if value == "" {
		return ""
	}

	if strings.ContainsAny(value, "\t\n\r ,") {
		return strconv.Quote(value)
	}

	return value
}

func appendProviderCallTranscriptProvenance(parts *[]string, calls []session.ExportProviderCall) {
	if len(calls) == 0 {
		return
	}

	*parts = append(*parts, "provider_calls="+strconv.Itoa(len(calls)))

	promptHashes := providerCallHashes(calls, func(call session.ExportProviderCall) string {
		return call.PromptHash
	})
	appendHashList(parts, "prompt_hash", "prompt_hashes", promptHashes)

	configHashes := providerCallHashes(calls, func(call session.ExportProviderCall) string {
		return call.ConfigHash
	})
	appendHashList(parts, "call_config_hash", "call_config_hashes", configHashes)

	toolCalls := 0
	toolResults := 0

	for index := range calls {
		toolCalls += calls[index].RequestToolCallCount
		toolResults += calls[index].RequestToolResultCount
	}

	if toolCalls > 0 {
		*parts = append(*parts, "tool_calls="+strconv.Itoa(toolCalls))
	}

	if toolResults > 0 {
		*parts = append(*parts, "tool_results="+strconv.Itoa(toolResults))
	}
}

func providerCallHashes(
	calls []session.ExportProviderCall,
	selectHash func(session.ExportProviderCall) string,
) []string {
	seen := make(map[string]struct{}, len(calls))
	hashes := make([]string, 0, len(calls))

	for index := range calls {
		hash := strings.TrimSpace(selectHash(calls[index]))
		if hash == "" {
			continue
		}

		if _, ok := seen[hash]; ok {
			continue
		}

		seen[hash] = struct{}{}
		hashes = append(hashes, hash)
	}

	return hashes
}

func appendHashList(parts *[]string, singular, plural string, hashes []string) {
	switch len(hashes) {
	case 0:
		return
	case 1:
		*parts = append(*parts, singular+"="+hashes[0])
	default:
		*parts = append(*parts, plural+"="+strings.Join(hashes, ","))
	}
}
