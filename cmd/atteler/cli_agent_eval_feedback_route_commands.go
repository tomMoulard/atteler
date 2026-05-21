package main

import (
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

func suggestSkill(steps []string, maxSteps, minOccurrences int, saveDir string) error {
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
		return nil
	}

	path, err := attskill.PersistSuggestion(saveDir, suggestion)
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

	if suggestion.Rationale != "" {
		fmt.Fprintf(&b, "rationale: %s\n", suggestion.Rationale)
	}

	return b.String()
}

func promptComplete(registry *agent.Registry, input string, limit int) {
	suggestions := promptcomplete.SuggestAll(promptcomplete.Context{
		Input:     input,
		Cursor:    len(input),
		Agents:    promptAgentCandidates(registry),
		Tools:     promptToolCandidates(),
		Templates: promptTemplateCandidates(),
	}, promptcomplete.Options{Limit: limit})
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
		fmt.Fprintf(&b, "score: %d\n", suggestion.Score)
		fmt.Fprintf(&b, "replace: %d:%d\n", suggestion.ReplacementStart, suggestion.ReplacementEnd)

		if suggestion.Explanation != "" {
			fmt.Fprintf(&b, "explanation: %s\n", suggestion.Explanation)
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
	fmt.Fprintf(&b, "agent: %s\n", proposal.Agent)
	fmt.Fprintf(&b, "confidence: %.2f\n", proposal.Confidence)
	fmt.Fprintf(&b, "action: %s\n", proposal.Action)
	fmt.Fprintf(&b, "reason: %s\n", proposal.Reason)

	if len(proposal.Evidence) > 0 {
		b.WriteString("evidence:\n")

		for _, evidence := range proposal.Evidence {
			fmt.Fprintf(&b, "  - %s\n", evidence)
		}
	}

	b.WriteByte('\n')

	return b.String()
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

	updatedAgents, history := feedback.ApplyProposals(cfg.Agents, feedback.FromSession(saved))
	if len(history) == 0 {
		fmt.Println("No feedback proposals applied.")
		return nil
	}

	cfg.Agents = updatedAgents
	if err := writeConfigFile(configPath, cfg); err != nil {
		return fmt.Errorf("feedback apply: %w", err)
	}

	historyPath = feedbackHistoryDefault(configPath, historyPath)
	if err := appendFeedbackHistory(historyPath, history, time.Now().UTC()); err != nil {
		return fmt.Errorf("feedback apply: %w", err)
	}

	fmt.Printf("Applied %d feedback proposal(s).\n", len(history))
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
	fmt.Fprintf(&b, "## Applied feedback %s\n\n", appliedAt.Format(time.RFC3339))

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
