package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/tommoulard/atteler/pkg/agent"
	appconfig "github.com/tommoulard/atteler/pkg/config"
	atteval "github.com/tommoulard/atteler/pkg/eval"
	"github.com/tommoulard/atteler/pkg/feedback"
	"github.com/tommoulard/atteler/pkg/modelroute"
	"github.com/tommoulard/atteler/pkg/promptcomplete"
	"github.com/tommoulard/atteler/pkg/session"
	attskill "github.com/tommoulard/atteler/pkg/skill"
)

func planAgents(registry *agent.Registry, prompt string, requested []string, maxAgents int) error {
	plan, err := registry.PlanAgents(prompt, requested, maxAgents)
	if err != nil {
		return fmt.Errorf("plan agents: %w", err)
	}

	if len(plan.Participants) == 0 {
		fmt.Println("No agents matched.")
		return nil
	}

	for i := range plan.Participants {
		fmt.Println(formatAgentPlanParticipant(&plan.Participants[i]))
	}

	return nil
}

func formatAgentPlanParticipant(participant *agent.Participant) string {
	parts := []string{participant.Agent.Name, "source=" + participant.Source}
	if participant.Pattern != "" {
		parts = append(parts, "match="+participant.Pattern)
	}

	if len(participant.Agent.Capabilities) > 0 {
		parts = append(parts, "capabilities="+strings.Join(participant.Agent.Capabilities, ","))
	}

	if participant.Agent.Model != "" {
		parts = append(parts, "model="+participant.Agent.Model)
	}

	return strings.Join(parts, "\t")
}

func evalOutput(actualPath, expectedText, expectedPath string, mode atteval.MatchMode) error {
	actual, err := os.ReadFile(actualPath)
	if err != nil {
		return fmt.Errorf("eval output: read actual %s: %w", actualPath, err)
	}

	expected, err := expectedEvalText(expectedText, expectedPath)
	if err != nil {
		return err
	}

	result := atteval.Check(string(actual), expected, mode)
	if result.Passed {
		fmt.Printf("PASS\tmode=%s\tactual=%s\n", result.Mode, actualPath)
		return nil
	}

	report := result.Failure()
	if report == "" {
		report = result.Summary
	}

	fmt.Printf("FAIL\tmode=%s\tactual=%s\n%s\n", result.Mode, actualPath, report)

	return errors.New("eval output failed")
}

func expectedEvalText(expectedText, expectedPath string) (string, error) {
	switch {
	case expectedText != "" && expectedPath != "":
		return "", errors.New("eval output: pass either --eval-expected or --eval-expected-file, not both")
	case expectedText != "":
		return expectedText, nil
	case expectedPath != "":
		data, err := os.ReadFile(expectedPath)
		if err != nil {
			return "", fmt.Errorf("eval output: read expected %s: %w", expectedPath, err)
		}

		return string(data), nil
	default:
		return "", errors.New("eval output: expected text is required")
	}
}

func suggestSkill(steps []string, maxSteps, minOccurrences int, saveDir string, reviewOnly bool) error {
	suggestion, ok := attskill.SuggestWithOptions(steps, attskill.Options{
		MaxSteps:       maxSteps,
		MinOccurrences: minOccurrences,
	})
	if !ok {
		fmt.Println("No repeated multi-step skill candidate found.")
		return nil
	}

	fmt.Print(formatSkillSuggestion(suggestion))

	if strings.TrimSpace(saveDir) == "" {
		if reviewOnly {
			return errors.New("skill review: --skill-review-only requires --skill-save-dir")
		}

		return nil
	}

	review, err := attskill.BuildReview(saveDir, suggestion)
	if err != nil {
		return fmt.Errorf("review skill suggestion: %w", err)
	}

	fmt.Print(review.Diff)
	fmt.Print(formatSkillTriggerResults(review.TriggerResults))

	if reviewOnly {
		fmt.Println("review-only: no files written")
		return nil
	}

	path, err := attskill.PersistReview(review)
	if err != nil {
		return fmt.Errorf("save skill suggestion: %w", err)
	}

	fmt.Println("saved: " + path)

	return nil
}

func formatSkillSuggestion(suggestion attskill.Suggestion) string {
	var b strings.Builder
	fmt.Fprintf(&b, "name: %s\n", suggestion.Name)
	fmt.Fprintf(&b, "slug: %s\n", suggestion.Slug)
	fmt.Fprintf(&b, "occurrences: %d\n", suggestion.Occurrences)
	b.WriteString("steps:\n")

	for _, step := range suggestion.Steps {
		fmt.Fprintf(&b, "  - %s\n", step)
	}

	if len(suggestion.Parameters) > 0 {
		b.WriteString("parameters:\n")

		for _, parameter := range suggestion.Parameters {
			fmt.Fprintf(&b, "  - %s=%s", parameter.Name, parameter.Placeholder)

			if len(parameter.Examples) > 0 {
				fmt.Fprintf(&b, " examples=%s", strings.Join(parameter.Examples, ","))
			}

			b.WriteByte('\n')
		}
	}

	if suggestion.Rationale != "" {
		fmt.Fprintf(&b, "rationale: %s\n", suggestion.Rationale)
	}

	return b.String()
}

func formatSkillTriggerResults(results []attskill.TriggerEvalResult) string {
	if len(results) == 0 {
		return ""
	}

	var b strings.Builder
	fmt.Fprintf(&b, "trigger-evals: pass %d cases\n", len(results))

	for _, result := range results {
		label := "reject"
		if result.Expected {
			label = "trigger"
		}

		fmt.Fprintf(&b, "  - %s: %s\n", label, result.Prompt)
	}

	return b.String()
}

func promptComplete(ctx context.Context, state appState, input string, limit int) {
	suggestions := promptcomplete.SuggestAll(promptCompletionContext(ctx, state, input, true), promptcomplete.Options{Limit: limit})
	if len(suggestions) == 0 {
		fmt.Println("No prompt completion found.")
		return
	}

	fmt.Print(formatPromptSuggestions(suggestions))
}

func promptAgentCandidates(registry *agent.Registry) []promptcomplete.Candidate {
	if registry == nil {
		return nil
	}

	names := registry.List()

	out := make([]promptcomplete.Candidate, 0, len(names))
	for _, name := range names {
		configuredAgent, _ := registry.Get(name)
		out = append(out, promptcomplete.Candidate{
			Text:        name,
			Kind:        "agent",
			Description: configuredAgent.Description,
			Tokens:      append([]string(nil), configuredAgent.Capabilities...),
		})
	}

	return out
}

func promptToolCandidates() []promptcomplete.Candidate {
	return []promptcomplete.Candidate{
		{Text: "memory-search", Kind: "tool", Description: "search local memory and saved sessions"},
		{Text: "plan-agents", Kind: "tool", Description: "preview agent orchestration"},
		{Text: "review", Kind: "tool", Description: "run a structured code review"},
		{Text: "test", Kind: "tool", Description: "run verification tests"},
	}
}

func promptTemplateCandidates() []promptcomplete.Candidate {
	return []promptcomplete.Candidate{
		{
			Text:        "review this change for correctness, tests, and regressions",
			Kind:        "template",
			Description: "code review prompt",
		},
		{
			Text:        "summarize this session with changed files and verification evidence",
			Kind:        "template",
			Description: "session summary prompt",
		},
		{
			Text:        "plan agents for this task and list the verification gates",
			Kind:        "template",
			Description: "agent orchestration prompt",
		},
	}
}

func formatPromptSuggestions(suggestions []promptcomplete.Suggestion) string {
	var b strings.Builder

	for i := range suggestions {
		suggestion := &suggestions[i]
		fmt.Fprintf(&b, "text: %s\n", suggestion.Text)
		fmt.Fprintf(&b, "suffix: %s\n", suggestion.Suffix)
		fmt.Fprintf(&b, "kind: %s\n", suggestion.Candidate.Kind)

		if suggestion.Source != "" {
			fmt.Fprintf(&b, "source: %s\n", suggestion.Source)
		}

		fmt.Fprintf(&b, "score: %d\n", suggestion.Score)
		fmt.Fprintf(&b, "replace: %d:%d\n", suggestion.ReplacementStart, suggestion.ReplacementEnd)

		if suggestion.Explanation != "" {
			fmt.Fprintf(&b, "explanation: %s\n", suggestion.Explanation)
		}

		if len(suggestion.RankSignals) > 0 {
			b.WriteString("rank:\n")

			for _, signal := range suggestion.RankSignals {
				fmt.Fprintf(&b, "  - %s %+d", signal.Name, signal.Score)

				if signal.Detail != "" {
					fmt.Fprintf(&b, ": %s", signal.Detail)
				}

				b.WriteByte('\n')
			}
		}

		b.WriteByte('\n')
	}

	return b.String()
}

func printFeedbackProposals(saved session.Session) {
	proposals := feedback.FromSession(saved)
	if len(proposals) == 0 {
		fmt.Println("No feedback proposals found.")
		return
	}

	for i := range proposals {
		fmt.Print(formatFeedbackProposal(proposals[i]))
	}
}

func formatFeedbackProposal(proposal feedback.Proposal) string {
	var b strings.Builder
	fmt.Fprintf(&b, "id: %s\n", feedback.ProposalID(proposal))
	writeFeedbackProposalHeader(&b, proposal)

	if sourceRun := strings.TrimSpace(proposal.SourceRun); sourceRun != "" {
		fmt.Fprintf(&b, "source_run: %s\n", sourceRun)
	}

	if reviewer := strings.TrimSpace(proposal.Reviewer); reviewer != "" {
		fmt.Fprintf(&b, "reviewer: %s\n", reviewer)
	}

	if proposal.ExpiresAt != nil {
		fmt.Fprintf(&b, "expires_at: %s\n", proposal.ExpiresAt.UTC().Format(time.RFC3339))
	}

	writeFeedbackRejectedAlternatives(&b, proposal.RejectedAlternatives)
	writeFeedbackEvidence(&b, proposal.Evidence)
	writeFeedbackLinkedEvidence(&b, proposal.LinkedEvidence)
	writeFeedbackVerification(&b, proposal.Verification)

	b.WriteByte('\n')

	return b.String()
}

func writeFeedbackProposalHeader(b *strings.Builder, proposal feedback.Proposal) {
	fmt.Fprintf(b, "agent: %s\n", proposal.Agent)
	fmt.Fprintf(b, "confidence: %.2f\n", proposal.Confidence)

	if rootCause := formatFeedbackRootCause(proposal.RootCause); rootCause != "" {
		fmt.Fprintf(b, "root_cause: %s\n", rootCause)
	}

	if target := strings.TrimSpace(proposal.TargetBehavior); target != "" {
		fmt.Fprintf(b, "target_behavior: %s\n", target)
	}

	fmt.Fprintf(b, "action: %s\n", proposal.Action)
	fmt.Fprintf(b, "reason: %s\n", proposal.Reason)
}

func writeFeedbackRejectedAlternatives(b *strings.Builder, rejected []feedback.RejectedAlternative) {
	if len(rejected) == 0 {
		return
	}

	b.WriteString("rejected_alternatives:\n")

	for _, item := range rejected {
		if alternative := strings.TrimSpace(item.Alternative); alternative != "" {
			fmt.Fprintf(b, "  - alternative: %s\n", alternative)
		}

		if reason := strings.TrimSpace(item.Reason); reason != "" {
			fmt.Fprintf(b, "    reason: %s\n", reason)
		}
	}
}

func writeFeedbackEvidence(b *strings.Builder, evidence []string) {
	if len(evidence) == 0 {
		return
	}

	b.WriteString("evidence:\n")

	for _, item := range evidence {
		fmt.Fprintf(b, "  - %s\n", item)
	}
}

func writeFeedbackLinkedEvidence(b *strings.Builder, links []feedback.EvidenceLink) {
	if len(links) == 0 {
		return
	}

	b.WriteString("linked_evidence:\n")

	for _, link := range links {
		fmt.Fprintf(b, "  - kind=%s\tref=%s", link.Kind, link.Reference)

		if description := strings.TrimSpace(link.Description); description != "" {
			fmt.Fprintf(b, "\tdescription=%s", description)
		}

		b.WriteByte('\n')
	}
}

func writeFeedbackVerification(b *strings.Builder, records []feedback.VerificationRecord) {
	if len(records) == 0 {
		return
	}

	b.WriteString("verification:\n")

	for _, record := range records {
		fmt.Fprintf(b, "  - phase=%s\tkind=%s\tpassed=%t", record.Phase, record.Kind, record.Passed)

		if record.Outcome != "" {
			fmt.Fprintf(b, "\toutcome=%s", record.Outcome)
		}

		if record.Score != 0 {
			fmt.Fprintf(b, "\tscore=%d", record.Score)
		}

		if record.Reference != "" {
			fmt.Fprintf(b, "\tref=%s", record.Reference)
		}

		b.WriteByte('\n')
	}
}

func formatFeedbackRootCause(rootCause feedback.RootCauseClassification) string {
	category := strings.TrimSpace(rootCause.Category)
	summary := strings.TrimSpace(rootCause.Summary)

	signals := cleanFeedbackStrings(rootCause.Signals)
	switch {
	case category == "" && summary == "":
		return ""
	case category == "":
		return summary
	case summary == "":
		return category
	case len(signals) == 0:
		return category + " — " + summary
	default:
		return fmt.Sprintf("%s — %s (signals: %s)", category, summary, strings.Join(signals, ", "))
	}
}

func cleanFeedbackStrings(values []string) []string {
	cleaned := make([]string, 0, len(values))
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			cleaned = append(cleaned, trimmed)
		}
	}

	return cleaned
}

func applyFeedbackProposals(saved session.Session, configPath, historyPath string) error {
	configPath = strings.TrimSpace(configPath)
	if configPath == "" {
		return errors.New("feedback apply: config path is required")
	}

	cfg, loaded, err := appconfig.LoadFiles([]string{configPath})
	if err != nil {
		return fmt.Errorf("feedback apply: load config: %w", err)
	}

	if len(loaded) == 0 {
		return fmt.Errorf("feedback apply: config %s not found", configPath)
	}

	originalCfg := cfg
	appliedAt := time.Now().UTC()

	updatedAgents, history := feedback.ApplyProposalsWithOptions(cfg.Agents, feedback.FromSession(saved), feedback.ApplyOptions{
		SourceRun: saved.ID,
		Reviewer:  "feedback-apply",
		Status:    feedback.GuidanceStatusPending,
		Now:       appliedAt,
	})
	if len(history) == 0 {
		fmt.Println("No feedback proposals recorded.")
		return nil
	}

	cfg.Agents = updatedAgents
	if err := writeConfigFile(configPath, cfg); err != nil {
		return fmt.Errorf("feedback apply: %w", err)
	}

	historyPath = feedbackHistoryDefault(configPath, historyPath)
	if err := appendFeedbackHistory(historyPath, history, appliedAt); err != nil {
		if restoreErr := writeConfigFile(configPath, originalCfg); restoreErr != nil {
			return fmt.Errorf("feedback apply: append history failed and restore config failed: %w", errors.Join(err, restoreErr))
		}

		return fmt.Errorf("feedback apply: %w", err)
	}

	fmt.Printf("Recorded %d pending feedback guidance decision(s).\n", len(history))
	fmt.Println("config: " + configPath)
	fmt.Println("history: " + historyPath)

	return nil
}

func approveFeedbackGuidance(configPath, historyPath, agentName, guidanceID string) error {
	configPath = strings.TrimSpace(configPath)
	if configPath == "" {
		return errors.New("feedback approve: config path is required")
	}

	agentName = strings.TrimSpace(agentName)
	if agentName == "" {
		return errors.New("feedback approve: agent is required")
	}

	guidanceID = strings.TrimSpace(guidanceID)
	if guidanceID == "" {
		return errors.New("feedback approve: guidance id is required")
	}

	cfg, loaded, err := appconfig.LoadFiles([]string{configPath})
	if err != nil {
		return fmt.Errorf("feedback approve: load config: %w", err)
	}

	if len(loaded) == 0 {
		return fmt.Errorf("feedback approve: config %s not found", configPath)
	}

	approvedAt := time.Now().UTC()

	updatedAgents, history, ok := feedback.ApproveGuidance(
		cfg.Agents,
		agentName,
		guidanceID,
		"feedback-approve",
		approvedAt,
	)
	if !ok {
		return fmt.Errorf("feedback approve: guidance %s for agent %s was not found, pending, or approvable", guidanceID, agentName)
	}

	cfg.Agents = updatedAgents
	if err := writeConfigFile(configPath, cfg); err != nil {
		return fmt.Errorf("feedback approve: %w", err)
	}

	historyPath = feedbackHistoryDefault(configPath, historyPath)
	if err := appendFeedbackHistory(historyPath, []feedback.HistoryEntry{history}, approvedAt); err != nil {
		return fmt.Errorf("feedback approve: %w", err)
	}

	switch history.Status {
	case feedback.GuidanceStatusApproved:
		fmt.Printf("Approved feedback guidance %s for agent %s.\n", guidanceID, agentName)
	case feedback.GuidanceStatusQuarantined:
		fmt.Printf("Quarantined feedback guidance %s for agent %s.\n", guidanceID, agentName)
	default:
		fmt.Printf("Recorded feedback guidance %s for agent %s with status %s.\n", guidanceID, agentName, history.Status)
	}

	fmt.Println("config: " + configPath)
	fmt.Println("history: " + historyPath)

	return nil
}

func rollbackFeedbackGuidance(configPath, historyPath, agentName, guidanceID, reason string) error {
	configPath = strings.TrimSpace(configPath)
	if configPath == "" {
		return errors.New("feedback rollback: config path is required")
	}

	agentName = strings.TrimSpace(agentName)
	if agentName == "" {
		return errors.New("feedback rollback: agent is required")
	}

	guidanceID = strings.TrimSpace(guidanceID)
	if guidanceID == "" {
		return errors.New("feedback rollback: guidance id is required")
	}

	cfg, loaded, err := appconfig.LoadFiles([]string{configPath})
	if err != nil {
		return fmt.Errorf("feedback rollback: load config: %w", err)
	}

	if len(loaded) == 0 {
		return fmt.Errorf("feedback rollback: config %s not found", configPath)
	}

	rolledBackAt := time.Now().UTC()

	updatedAgents, history, ok := feedback.RollbackGuidance(
		cfg.Agents,
		agentName,
		guidanceID,
		"feedback-rollback",
		reason,
		rolledBackAt,
	)
	if !ok {
		return fmt.Errorf("feedback rollback: guidance %s for agent %s was not found or is already rolled back", guidanceID, agentName)
	}

	cfg.Agents = updatedAgents
	if err := writeConfigFile(configPath, cfg); err != nil {
		return fmt.Errorf("feedback rollback: %w", err)
	}

	historyPath = feedbackHistoryDefault(configPath, historyPath)
	if err := appendFeedbackHistory(historyPath, []feedback.HistoryEntry{history}, rolledBackAt); err != nil {
		return fmt.Errorf("feedback rollback: %w", err)
	}

	fmt.Printf("Rolled back feedback guidance %s for agent %s.\n", guidanceID, agentName)
	fmt.Println("config: " + configPath)
	fmt.Println("history: " + historyPath)

	return nil
}

func feedbackHistoryDefault(configPath, historyPath string) string {
	historyPath = strings.TrimSpace(historyPath)
	if historyPath != "" {
		return historyPath
	}

	return configPath + ".feedback.md"
}

func writeConfigFile(path string, cfg appconfig.Config) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write config %s: %w", path, err)
	}

	return nil
}

func appendFeedbackHistory(path string, entries []feedback.HistoryEntry, appliedAt time.Time) error {
	dir := filepath.Dir(path)
	if dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("create feedback history dir %s: %w", dir, err)
		}
	}

	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open feedback history %s: %w", path, err)
	}

	if _, err := file.WriteString(formatFeedbackHistory(entries, appliedAt)); err != nil {
		_ = file.Close()
		return fmt.Errorf("write feedback history %s: %w", path, err)
	}

	if err := file.Close(); err != nil {
		return fmt.Errorf("close feedback history %s: %w", path, err)
	}

	return nil
}

func formatFeedbackHistory(entries []feedback.HistoryEntry, appliedAt time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Feedback guidance decisions %s\n\n", appliedAt.Format(time.RFC3339))

	for i := range entries {
		b.WriteString(feedback.FormatHistoryEntry(entries[i]))
		b.WriteByte('\n')
	}

	return b.String()
}

func runRouteModels(opts cliOptions) error {
	if len(opts.routeCandidates) == 0 {
		return errors.New("model route: at least one --route-candidate is required; run `atteler help providers`")
	}

	candidates, profile, err := routeCandidatesAndProfile(opts)
	if err != nil {
		return err
	}

	chain := modelroute.FallbackChain(candidates, profile)
	if len(chain) == 0 {
		fmt.Println("No model route candidates fit.")
		return nil
	}

	for i := range chain {
		fmt.Println(formatRouteCandidate(chain[i], profile))
	}

	return nil
}

func routeCandidatesAndProfile(opts cliOptions) ([]modelroute.Candidate, modelroute.RequestProfile, error) {
	candidates := make([]modelroute.Candidate, 0, len(opts.routeCandidates))
	for _, raw := range opts.routeCandidates {
		candidate, err := parseRouteCandidate(raw)
		if err != nil {
			return nil, modelroute.RequestProfile{}, err
		}

		candidates = append(candidates, candidate)
	}

	profile := modelroute.RequestProfile{
		EstimatedInputTokens:  opts.routeInputTokens.value,
		EstimatedOutputTokens: opts.routeOutputTokens.value,
		Interactive:           opts.routeInteractive,
		Batch:                 opts.routeBatch,
	}
	if opts.routeBudget.set {
		profile.Budget = opts.routeBudget.value
	}

	if opts.routeCacheReuse.set {
		profile.PromptCacheReuseEstimate = opts.routeCacheReuse.value
	}

	return candidates, profile, nil
}

func applyRouteSelection(opts cliOptions, state *selectionState) error {
	if len(opts.routeCandidates) == 0 {
		return nil
	}

	candidates, profile, err := routeCandidatesAndProfile(opts)
	if err != nil {
		return err
	}

	chain := modelroute.FallbackChain(candidates, profile)
	if len(chain) == 0 {
		return errors.New("model route: no candidates fit request budget/context")
	}

	state.selectedModel = chain[0].ID()
	state.fallbackModels = routeFallbackIDs(chain[1:])
	state.modelLocked = true
	state.sessionState.DefaultModel = state.selectedModel

	return nil
}

func routeFallbackIDs(candidates []modelroute.Candidate) []string {
	if len(candidates) == 0 {
		return nil
	}

	out := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		out = append(out, candidate.ID())
	}

	return out
}

func parseRouteCandidate(raw string) (modelroute.Candidate, error) {
	parts := strings.Split(raw, ",")
	if len(parts) == 0 || strings.TrimSpace(parts[0]) == "" {
		return modelroute.Candidate{}, errors.New("route candidate: model id is required")
	}

	candidate := modelroute.Candidate{}

	id := strings.TrimSpace(parts[0])
	if provider, name, ok := strings.Cut(id, "/"); ok {
		candidate.Provider = strings.TrimSpace(provider)
		candidate.Name = strings.TrimSpace(name)
	} else {
		candidate.Name = id
	}

	if candidate.Name == "" && candidate.Provider == "" {
		return modelroute.Candidate{}, errors.New("route candidate: model id is required")
	}

	for _, part := range parts[1:] {
		field, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			return modelroute.Candidate{}, fmt.Errorf("route candidate %q: expected key=value", raw)
		}

		if err := applyRouteCandidateField(&candidate, strings.TrimSpace(field), strings.TrimSpace(value)); err != nil {
			return modelroute.Candidate{}, err
		}
	}

	return candidate, nil
}

func applyRouteCandidateField(candidate *modelroute.Candidate, field, value string) error {
	switch field {
	case "input", "input_cost":
		parsed, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return fmt.Errorf("route candidate input cost: %w", err)
		}

		candidate.InputTokenCost = parsed
	case "output", "output_cost":
		parsed, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return fmt.Errorf("route candidate output cost: %w", err)
		}

		candidate.OutputTokenCost = parsed
	case "priority":
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("route candidate priority: %w", err)
		}

		candidate.Priority = parsed
	case "max", "max_input":
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("route candidate max input: %w", err)
		}

		candidate.MaxInputTokens = parsed
	case "latency":
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("route candidate latency: %w", err)
		}

		candidate.ExpectedLatencyMS = parsed
	case "ttft":
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("route candidate ttft: %w", err)
		}

		candidate.ExpectedTTFTMS = parsed
	default:
		return fmt.Errorf("route candidate: unknown field %q", field)
	}

	return nil
}

func formatRouteCandidate(candidate modelroute.Candidate, profile modelroute.RequestProfile) string {
	parts := []string{
		candidate.ID(),
		fmt.Sprintf("cost=%.6f", modelroute.EstimateCost(candidate, profile)),
	}
	if candidate.Priority != 0 {
		parts = append(parts, "priority="+strconv.Itoa(candidate.Priority))
	}

	if candidate.MaxInputTokens > 0 {
		parts = append(parts, "max_input="+strconv.Itoa(candidate.MaxInputTokens))
	}

	if candidate.ExpectedLatencyMS > 0 {
		parts = append(parts, "latency_ms="+strconv.Itoa(candidate.ExpectedLatencyMS))
	}

	if candidate.ExpectedTTFTMS > 0 {
		parts = append(parts, "ttft_ms="+strconv.Itoa(candidate.ExpectedTTFTMS))
	}

	return strings.Join(parts, "\t")
}
