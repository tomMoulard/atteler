package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/tommoulard/atteler/pkg/githistory"
	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/speculate"
	"github.com/tommoulard/atteler/pkg/watch"
)

func runSpeculatePlan(agents, gates []string, prompt string) error {
	if len(gates) == 0 {
		gates = []string{"tests pass", "lint pass", "types pass"}
	}

	plan, err := speculate.NewPlan(agents, gates)
	if err != nil {
		return fmt.Errorf("speculate plan: %w", err)
	}

	fmt.Print(formatSpeculatePlan(plan))

	if strings.TrimSpace(prompt) != "" {
		estimate, estimateErr := speculate.EstimatePromptCacheReuse(speculateBranchPrompts(plan, prompt))
		if estimateErr != nil {
			return fmt.Errorf("speculate prompt cache: %w", estimateErr)
		}

		fmt.Print(formatSpeculatePromptCacheEstimate(estimate))
	}

	return nil
}

// registryCompleter adapts the llm.Registry to the speculate.LLMCompleter
// interface so the speculative execution pipeline can make real LLM calls.
type registryCompleter struct {
	registry       *llm.Registry
	fallbackModels []string
	generation     generationSettings
}

func (rc *registryCompleter) Complete(ctx context.Context, model, systemPrompt, userPrompt string) (string, error) {
	params := llm.CompleteParams{
		Model: model,
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: systemPrompt},
			{Role: llm.RoleUser, Content: userPrompt},
		},
	}

	applyGenerationParams(&params, rc.generation)

	resp, err := rc.registry.CompleteWithFallback(ctx, params, rc.fallbackModels)
	if err != nil {
		return "", fmt.Errorf("speculate LLM complete: %w", err)
	}

	return resp.Content, nil
}

func runSpeculateExecution(ctx context.Context, state appState, opts cliOptions) error {
	prompt := strings.TrimSpace(opts.speculatePrompt)
	if prompt == "" {
		return errors.New("speculate-run requires --speculate-prompt")
	}

	agents := []string(opts.speculateAgents)
	if len(agents) == 0 {
		return errors.New("speculate-run requires at least one --speculate-agent")
	}

	gates := []string(opts.speculateGates)
	if len(gates) == 0 {
		gates = []string{"tests pass", "lint pass", "types pass"}
	}

	plan, err := speculate.NewPlan(agents, gates)
	if err != nil {
		return fmt.Errorf("speculate-run: %w", err)
	}

	completer := &registryCompleter{
		registry:       state.registry,
		fallbackModels: state.fallbackModels,
		generation:     mergeGenerationSettings(state.generationDefaults, state.generationOverrides),
	}

	fmt.Fprintln(os.Stderr, "speculate: running three-round pipeline with "+strings.Join(agents, ", ")+"...")

	result, err := speculate.RunWithLLM(ctx, plan, completer, prompt)
	if err != nil {
		// Print partial results even on error.
		if len(result.Session.Proposals) > 0 {
			fmt.Println(formatSpeculateResult(result))
		}

		return fmt.Errorf("speculate-run: %w", err)
	}

	fmt.Print(formatSpeculateResult(result))

	return nil
}

func formatSpeculateResult(result speculate.Result) string {
	var b strings.Builder

	b.WriteString("winner: " + result.Winner + "\n")
	b.WriteString("reason: " + result.Reason + "\n")

	if len(result.Session.Proposals) > 0 {
		b.WriteString("proposals:\n")

		for _, p := range result.Session.Proposals {
			fmt.Fprintf(&b, "  - agent: %s\n    content: %s\n", p.Agent, truncatePreview(p.Content, 200))
		}
	}

	if len(result.Session.Reviews) > 0 {
		b.WriteString("reviews:\n")

		for _, r := range result.Session.Reviews {
			fmt.Fprintf(&b, "  - reviewer: %s -> %s\n    notes: %s\n", r.Reviewer, r.TargetAgent, truncatePreview(r.Notes, 200))
		}
	}

	if len(result.Session.Verdict.GateChecks) > 0 {
		b.WriteString("gates:\n")

		for _, gc := range result.Session.Verdict.GateChecks {
			status := "FAIL"
			if gc.Passed {
				status = "PASS"
			}

			fmt.Fprintf(&b, "  - %s: %s %s\n", gc.Name, status, gc.Notes)
		}
	}

	return b.String()
}

func truncatePreview(s string, maxLen int) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "\n", " ")

	if len(s) <= maxLen {
		return s
	}

	return s[:maxLen] + "..."
}

func formatSpeculatePlan(plan speculate.Plan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "agents: %s\n", strings.Join(plan.Agents, ","))
	b.WriteString("rounds:\n")

	for _, round := range plan.Rounds {
		fmt.Fprintf(&b, "  - %d\t%s\t%s\n", round.Number, round.Name, round.Purpose)
	}

	proposals := make([]speculate.Proposal, 0, len(plan.Agents))
	for _, name := range plan.Agents {
		proposals = append(proposals, speculate.Proposal{Agent: name, Round: speculate.RoundProposal})
	}

	reviews, err := speculate.CrossReviews(proposals)
	if err == nil && len(reviews) > 0 {
		b.WriteString("cross_reviews:\n")

		for _, review := range reviews {
			fmt.Fprintf(&b, "  - %s -> %s\n", review.Reviewer, review.TargetAgent)
		}
	}

	b.WriteString("gates:\n")

	for _, gate := range plan.GateChecks {
		fmt.Fprintf(&b, "  - %s\n", gate)
	}

	return b.String()
}

func speculateBranchPrompts(plan speculate.Plan, prompt string) []speculate.BranchPrompt {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return nil
	}

	var shared strings.Builder
	shared.WriteString("Task:\n")
	shared.WriteString(prompt)
	shared.WriteString("\n\nRequired gate checks:\n")

	for _, gate := range plan.GateChecks {
		shared.WriteString("- ")
		shared.WriteString(gate)
		shared.WriteByte('\n')
	}

	shared.WriteString("\nSpeculative round: independent proposal\n")

	branches := make([]speculate.BranchPrompt, 0, len(plan.Agents))
	for _, name := range plan.Agents {
		branches = append(branches, speculate.BranchPrompt{
			Branch: name,
			Prompt: shared.String() +
				"Branch agent: " + name + "\n" +
				"Produce a self-contained proposal that can be cross-reviewed.\n",
		})
	}

	return branches
}

func formatSpeculatePromptCacheEstimate(estimate speculate.PromptCacheReuseEstimate) string {
	var b strings.Builder
	b.WriteString("prompt_cache:\n")
	fmt.Fprintf(&b, "  shared_prefix_bytes: %d\n", estimate.SharedPrefixBytes)
	fmt.Fprintf(&b, "  reusable_prompt_bytes: %d\n", estimate.ReusablePromptBytes)
	fmt.Fprintf(&b, "  total_prompt_bytes: %d\n", estimate.TotalPromptBytes)
	fmt.Fprintf(&b, "  reuse_ratio: %.4f\n", estimate.ReuseRatio)
	b.WriteString("  branches:\n")

	for _, branch := range estimate.Branches {
		fmt.Fprintf(
			&b, "    - %s\tprompt_bytes=%d\tshared_prefix_bytes=%d\treuse_ratio=%.4f\n",
			branch.Branch,
			branch.PromptBytes,
			branch.SharedPrefixBytes,
			branch.ReuseRatio,
		)
	}

	return b.String()
}

func runWatchScan(root string, largeFileBytes int, jsonOutput bool) error {
	findings, err := watch.ScanWithOptions(root, watch.Options{LargeFileBytes: int64(largeFileBytes)})
	if err != nil {
		return fmt.Errorf("watch scan: %w", err)
	}

	if jsonOutput {
		if findings == nil {
			findings = []watch.Finding{}
		}

		if err := json.NewEncoder(os.Stdout).Encode(struct {
			Findings []watch.Finding `json:"findings"`
		}{Findings: findings}); err != nil {
			return fmt.Errorf("watch scan: encode JSON: %w", err)
		}

		return nil
	}

	if len(findings) == 0 {
		fmt.Println("No watch findings found.")
		return nil
	}

	for i := range findings {
		fmt.Println(formatWatchFinding(findings[i]))
	}

	return nil
}

func runWatchLoop(ctx context.Context, root string, largeFileBytes, intervalSeconds, maxIterations int) error {
	interval := time.Duration(intervalSeconds) * time.Second

	results, err := watch.Run(ctx, root, watch.RunOptions{
		ScanOptions:   watch.Options{LargeFileBytes: int64(largeFileBytes)},
		Interval:      interval,
		MaxIterations: maxIterations,
	})
	for i := range results {
		fmt.Println(formatWatchIteration(results[i]))

		if len(results[i].Findings) == 0 {
			fmt.Println("No watch findings found.")
			continue
		}

		for j := range results[i].Findings {
			fmt.Println(formatWatchFinding(results[i].Findings[j]))
		}
	}

	if err != nil {
		return fmt.Errorf("watch loop: %w", err)
	}

	return nil
}

func formatWatchIteration(result watch.IterationResult) string {
	parts := []string{
		"iteration=" + strconv.Itoa(result.Iteration),
		"findings=" + strconv.Itoa(len(result.Findings)),
	}
	if !result.StartedAt.IsZero() {
		parts = append(parts, "started="+result.StartedAt.Format(time.RFC3339))
	}

	if result.Duration > 0 {
		parts = append(parts, "duration="+result.Duration.String())
	}

	return strings.Join(parts, "\t")
}

func formatWatchFinding(finding watch.Finding) string {
	parts := []string{
		"path=" + finding.Path,
		"kind=" + finding.Kind,
		"severity=" + finding.Severity,
	}
	if finding.Message != "" {
		parts = append(parts, "message="+finding.Message)
	}

	if finding.RuleID != "" {
		parts = append(parts, "rule_id="+finding.RuleID)
	}

	if finding.Help != "" {
		parts = append(parts, "help="+finding.Help)
	}

	return strings.Join(parts, "\t")
}

func runGitHistorySearch(ctx context.Context, root, query string, limit int) error {
	if limit == 0 {
		limit = 5
	}

	logText, err := gitHistoryLog(ctx, root)
	if err != nil {
		return err
	}

	commits, err := githistory.ParseLog(logText)
	if err != nil {
		return fmt.Errorf("git history: parse log: %w", err)
	}

	results := githistory.NewIndex(commits).Search(query, limit)
	if len(results) == 0 {
		fmt.Println("No git history results found.")
		return nil
	}

	for i := range results {
		fmt.Println(formatGitHistoryResult(results[i]))
	}

	return nil
}

func gitHistoryLog(ctx context.Context, root string) (string, error) {
	cmd := exec.CommandContext(
		ctx,
		"git",
		"log",
		"--name-only",
		"--date=iso-strict",
		"--pretty=format:%H%x1f%an%x1f%ae%x1f%aI%x1f%s%x1e",
		"--",
	)
	cmd.Dir = root

	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git history: run git log: %w", err)
	}

	return string(out), nil
}

func formatGitHistoryResult(result githistory.Result) string {
	commit := result.Commit

	parts := []string{
		shortCommitHash(commit.Hash),
		fmt.Sprintf("score=%d", result.Score),
	}
	if !commit.Date.IsZero() {
		parts = append(parts, "date="+commit.Date.Format(time.RFC3339))
	}

	if commit.AuthorName != "" {
		parts = append(parts, "author="+commit.AuthorName)
	}

	if commit.Subject != "" {
		parts = append(parts, "subject="+commit.Subject)
	}

	for _, snippet := range result.Snippets {
		if snippet.Text != "" {
			parts = append(parts, snippet.Field+"="+snippet.Text)
			break
		}
	}

	return strings.Join(parts, "\t")
}

func shortCommitHash(hash string) string {
	if len(hash) <= 12 {
		return hash
	}

	return hash[:12]
}
