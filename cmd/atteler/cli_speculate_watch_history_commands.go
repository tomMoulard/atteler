//nolint:wsl_v5 // Command flow keeps related render/format branches close for readability.
package main

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tommoulard/atteler/pkg/agent"
	"github.com/tommoulard/atteler/pkg/autonomy"
	"github.com/tommoulard/atteler/pkg/contextref"
	"github.com/tommoulard/atteler/pkg/events"
	"github.com/tommoulard/atteler/pkg/githistory"
	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/permission"
	"github.com/tommoulard/atteler/pkg/session"
	"github.com/tommoulard/atteler/pkg/shell"
	"github.com/tommoulard/atteler/pkg/speculate"
	"github.com/tommoulard/atteler/pkg/symphony"
	"github.com/tommoulard/atteler/pkg/watch"
)

const defaultGitHubAPIEndpoint = "https://api.github.com"

func runSpeculatePlan(input speculatePlanCommandInput) error {
	gates := input.Gates
	if len(gates) == 0 {
		gates = []string{"tests pass", "lint pass", "types pass"}
	}

	plan, err := speculate.NewPlan(input.Agents, gates)
	if err != nil {
		return fmt.Errorf("speculate plan: %w", err)
	}

	fmt.Print(formatSpeculatePlan(plan))

	if strings.TrimSpace(input.Prompt) != "" {
		estimate, estimateErr := speculate.EstimatePromptCacheReuse(speculateBranchPrompts(plan, input.Prompt))
		if estimateErr != nil {
			return fmt.Errorf("speculate prompt cache: %w", estimateErr)
		}

		fmt.Print(formatSpeculatePromptCacheEstimate(estimate))
	}

	return nil
}

// registryCompleter adapts the llm.Registry to the speculate.LLMCompleter
// interface so the speculative execution pipeline can make real LLM calls.
//
//nolint:govet // Field order groups LLM routing, generation, and autonomy settings for readability.
type registryCompleter struct {
	registry       *llm.Registry
	agents         *agent.Registry
	recorder       *multiAgentRunRecorder
	hookRunner     *events.Runner
	selectedModel  string
	fallbackModels []string
	generationBase generationSettings
	generationOver generationSettings
	autonomy       autonomy.Level
	maxInputTokens int
	modelLocked    bool
}

func (rc *registryCompleter) Complete(ctx context.Context, branch, systemPrompt, userPrompt string) (string, error) {
	activeAgent := agentSelection{name: branch}
	if rc.agents != nil {
		if configuredAgent, ok := rc.agents.Get(branch); ok {
			activeAgent = agentSelection{name: branch, agent: configuredAgent, ok: true}
		}
	}

	generation := generationForRequest(rc.generationBase, rc.generationOver, activeAgent)

	selectedModel := rc.selectedModel
	if strings.TrimSpace(selectedModel) == "" && !activeAgent.ok {
		selectedModel = branch
	}

	messages := []llm.Message{
		{Role: llm.RoleSystem, Content: systemPrompt},
		{Role: llm.RoleUser, Content: userPrompt},
	}

	requestModel := selectedModel

	fallbackModels := append([]string(nil), rc.fallbackModels...)
	if activeAgent.ok && !rc.modelLocked {
		requestModel, fallbackModels = effectiveAgentModelSelection(selectedModel, rc.fallbackModels, activeAgent)
	}

	budgetMessages := requestMessagesForBudget(requestModel, messages, activeAgent, generation, "", false)

	requestModel, fallbackModels, routeDecision, err := requestModelRoutingAndFallbacks(
		ctx,
		rc.registry,
		selectedModel,
		rc.modelLocked,
		rc.fallbackModels,
		activeAgent,
		requestModel,
		fallbackModels,
		routeCompleteParamsForRequest(requestModel, budgetMessages, generation, activeAgent, false),
		routeProfileForMessages(budgetMessages, generation),
		routeTelemetryFromRegistry(rc.registry),
		routeAvailabilityFromRegistryWithRefresh(ctx, rc.registry, effectiveRouteCandidateChain(selectedModel, rc.fallbackModels, activeAgent, rc.modelLocked)),
	)
	if err != nil {
		emitRouteDecisionWarning(ctx, rc.hookRunner, "", "", branch, requestModel, routeDecision)

		return "", fmt.Errorf("speculate LLM route: %w", err)
	}

	params := llm.CompleteParams{
		Model:    requestModel,
		Messages: messages,
	}
	if activeAgent.ok {
		params = activeAgent.agent.CompleteParams(requestModel, messages)
	}

	applyGenerationParams(&params, generation)
	prependAutonomyInstructions(&params, rc.autonomy)

	manifestEvent := requestContextManifestEvent(newRequestContextManifestForModels(
		rc.registry,
		params.Model,
		fallbackModels,
		params.Messages,
		rc.maxInputTokens,
		contextref.ReferenceManifest{},
	))
	manifestEvent.Agent = branch
	setExplicitContextManifestEventModel(&manifestEvent, params.Model)
	emitHookWarning(ctx, rc.hookRunner, manifestEvent)
	emitRouteDecisionWarning(ctx, rc.hookRunner, "", "", branch, params.Model, routeDecision)

	if budgetErr := validateRequestBudgetWithFallbacks(rc.registry, params.Model, fallbackModels, params.Messages, rc.maxInputTokens); budgetErr != nil {
		return "", fmt.Errorf("speculate LLM budget: %w", budgetErr)
	}

	resp, err := completeMultiAgentRegistryCall(
		ctx,
		rc.recorder,
		rc.registry,
		speculateCallInfo(branch, systemPrompt, userPrompt),
		params,
		fallbackModels,
	)
	if err != nil {
		return "", fmt.Errorf("speculate LLM complete: %w", err)
	}

	emitRouteDecisionWarning(
		ctx,
		rc.hookRunner,
		"",
		"",
		branch,
		routeResponseModelID(resp.Provider, resp.Model),
		routeDecisionWithResponse(routeDecision, resp, routeTelemetryFromRegistry(rc.registry)),
	)

	return resp.Content, nil
}

func speculateCallInfo(agentName, systemPrompt, userPrompt string) multiAgentCallInfo {
	info := multiAgentCallInfo{Agent: agentName, Phase: multiAgentPhaseProposal}

	switch {
	case strings.Contains(systemPrompt, "aggregating speculative execution results"):
		info.Phase = multiAgentPhaseAggregateVerdict
	case strings.Contains(systemPrompt, "reviewing a proposal"):
		info.Phase = multiAgentPhaseCrossReview
		info.TargetAgent = targetAfter(userPrompt, "Proposal from ", ":")
	}

	return info
}

func runSpeculateExecution(ctx context.Context, state appState, input speculateRunCommandInput) error {
	prompt := strings.TrimSpace(input.Prompt)
	if prompt == "" {
		return errors.New("speculate-run requires --speculate-prompt")
	}

	agents := input.Agents
	if len(agents) == 0 {
		return errors.New("speculate-run requires at least one --speculate-agent")
	}

	gates := input.Gates
	if len(gates) == 0 {
		gates = []string{"tests pass", "lint pass", "types pass"}
	}

	plan, err := speculate.NewPlan(agents, gates)
	if err != nil {
		return fmt.Errorf("speculate-run: %w", err)
	}

	sessionState := state.sessionState
	budget := multiAgentBudgetFromState(state)
	recorder := newMultiAgentRunRecorder(
		state.sessionStore,
		&sessionState,
		session.MultiAgentRunKindSpeculation,
		prompt,
		state.selectedModel,
		state.fallbackModels,
		budget,
		state.registry.ContextWindow,
		nil,
	)

	if startErr := recorder.start(); startErr != nil {
		return startErr
	}

	completer := &registryCompleter{
		registry:       state.registry,
		agents:         state.agentRegistry,
		recorder:       recorder,
		hookRunner:     state.hookRunner,
		selectedModel:  state.selectedModel,
		fallbackModels: state.fallbackModels,
		generationBase: state.generationDefaults,
		generationOver: state.generationOverrides,
		autonomy:       state.autonomy,
		maxInputTokens: state.maxInputTokens,
		modelLocked:    state.modelLocked,
	}

	fmt.Fprintln(os.Stderr, "speculate: running three-round pipeline with "+strings.Join(agents, ", ")+"...")

	runCtx, cancelRun := contextWithMultiAgentRunBudget(ctx, budget)
	defer cancelRun()

	result, err := speculate.RunWithLLM(runCtx, plan, completer, prompt)

	err = multiAgentRunErrorForBudgetContext(ctx, runCtx, err, budget, recorder.run.StartedAt)
	if recordErr := recorder.recordSpeculateSession(result.Session); recordErr != nil {
		return multiAgentPersistenceError(err, recordErr)
	}

	if finishErr := recorder.finish(err); finishErr != nil {
		return multiAgentPersistenceError(err, finishErr)
	}

	if err != nil {
		// Print partial results even on error.
		if len(result.Session.Proposals) > 0 {
			fmt.Print(formatSpeculateResult(result))
			fmt.Println("error: " + err.Error())
		}

		return fmt.Errorf("speculate-run: %w", err)
	}

	fmt.Print(formatSpeculateResult(result))

	return nil
}

func formatSpeculateResult(result speculate.Result) string {
	var b strings.Builder

	winner := strings.TrimSpace(result.Winner)
	if winner == "" {
		winner = strings.TrimSpace(result.Session.Verdict.Winner)
	}

	reason := strings.TrimSpace(result.Reason)
	if reason == "" {
		reason = strings.TrimSpace(result.Session.Verdict.Reason)
	}

	b.WriteString("winner: " + winner + "\n")
	b.WriteString("reason: " + reason + "\n")

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
			status := gateStatusFail
			if gc.Passed {
				status = gateStatusPass
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

type watchCLIOptions struct {
	BaselinePath     string
	BaselineRef      string
	RulesPath        string
	SuppressionsPath string
	GateMinSeverity  string
	IssueMinSeverity string
	IssueRepository  string
	GitHubEndpoint   string
	GitHubToken      string
	Autonomy         autonomy.Level
	IssueLabels      []string
	LargeFileBytes   int
	JSONOutput       bool
	GateEnabled      bool
	IssueUpsert      bool
}

type watchScanOutput struct {
	Comparison *watch.Comparison         `json:"comparison,omitempty"`
	Gate       *watch.GateResult         `json:"gate,omitempty"`
	Baseline   *watchBaselineInfo        `json:"baseline,omitempty"`
	Issues     []watch.IssueUpsertResult `json:"issues,omitempty"`
	Findings   []watch.Finding           `json:"findings"`
}

type watchBaselineInfo struct {
	Source   string `json:"source"`
	Path     string `json:"path,omitempty"`
	Ref      string `json:"ref,omitempty"`
	Commit   string `json:"commit,omitempty"`
	Findings int    `json:"findings"`
}

func watchCLIOptionsFrom(options cliOptions, levels ...autonomy.Level) watchCLIOptions {
	level := autonomy.DefaultLevel
	if len(levels) > 0 {
		level = autonomy.Normalize(levels[0])
	}

	return watchCLIOptions{
		BaselinePath:     options.watchBaselinePath,
		BaselineRef:      options.watchBaselineRef,
		RulesPath:        options.watchRulesPath,
		SuppressionsPath: options.watchSuppressionsPath,
		GateMinSeverity:  options.watchGateMinSeverity,
		IssueMinSeverity: options.watchIssueMinSeverity,
		IssueRepository:  options.watchIssueRepository,
		GitHubEndpoint:   options.watchGitHubEndpoint,
		GitHubToken:      options.watchGitHubToken,
		IssueLabels:      []string(options.watchIssueLabels),
		Autonomy:         level,
		LargeFileBytes:   options.watchLargeFileBytes.value,
		JSONOutput:       options.watchJSON,
		GateEnabled:      options.watchGate,
		IssueUpsert:      options.watchIssueUpsert,
	}
}

func runWatchScan(ctx context.Context, root string, options watchCLIOptions) error {
	return runWatchScanWithIssueTracker(ctx, root, options, nil)
}

func runWatchScanWithIssueTracker(ctx context.Context, root string, options watchCLIOptions, issueTracker watch.IssueTracker) error {
	scanOptions, baseline, baselineInfo, gateOptions, err := watchQualityInputs(ctx, root, options)
	if err != nil {
		return err
	}

	if authErr := authorizeWatchScanRead(ctx, root); authErr != nil {
		return authErr
	}

	findings, err := watch.ScanWithOptions(root, scanOptions)
	if err != nil {
		return fmt.Errorf("watch scan: %w", err)
	}

	output := buildWatchScanOutput(findings, baseline, baselineInfo, gateOptions)
	if err := upsertWatchScanIssues(ctx, options, issueTracker, &output); err != nil {
		return err
	}

	if options.JSONOutput {
		if findings == nil {
			findings = []watch.Finding{}
		}

		output.Findings = findings
		if err := json.NewEncoder(os.Stdout).Encode(output); err != nil {
			return fmt.Errorf("watch scan: encode JSON: %w", err)
		}

		return watchGateError(output.Gate)
	}

	printWatchScanOutput(output)

	return watchGateError(output.Gate)
}

func printWatchScanOutput(output watchScanOutput) {
	if output.Comparison != nil {
		fmt.Println(formatWatchComparison(*output.Comparison))
	}

	if output.Baseline != nil {
		fmt.Println(formatWatchBaseline(*output.Baseline))
	}

	if output.Gate != nil {
		fmt.Println(formatWatchGate(*output.Gate))
	}

	printWatchIssueUpserts(output.Issues)

	if output.Comparison != nil && printWatchComparisonFindings(*output.Comparison) {
		return
	}

	findings := output.Findings
	if len(findings) == 0 {
		fmt.Println("No watch findings found.")
		return
	}

	for i := range findings {
		fmt.Println(formatWatchFinding(findings[i]))
	}
}

func runWatchLoop(ctx context.Context, root string, options watchCLIOptions, intervalSeconds, maxIterations int) error {
	scanOptions, baseline, baselineInfo, gateOptions, err := watchQualityInputs(ctx, root, options)
	if err != nil {
		return err
	}

	if authErr := authorizeWatchScanRead(ctx, root); authErr != nil {
		return authErr
	}

	issueTracker, issueOptions, err := watchIssueInputs(ctx, options, nil)
	if err != nil {
		return err
	}

	interval := time.Duration(intervalSeconds) * time.Second

	results, err := watch.Run(ctx, root, watch.RunOptions{
		ScanOptions:       scanOptions,
		Baseline:          baseline,
		Gate:              gateOptions,
		IssueTracker:      issueTracker,
		IssueOptions:      issueOptions,
		StopOnGateFailure: gateOptions.Enabled,
		Interval:          interval,
		MaxIterations:     maxIterations,
	})

	if baselineInfo != nil {
		fmt.Println(formatWatchBaseline(*baselineInfo))
	}

	for i := range results {
		fmt.Println(formatWatchIteration(results[i]))
		printWatchIssueUpserts(results[i].Issues)

		if results[i].Comparison != nil && printWatchComparisonFindings(*results[i].Comparison) {
			continue
		}

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

func watchQualityInputs(ctx context.Context, root string, options watchCLIOptions) (watch.Options, *watch.Baseline, *watchBaselineInfo, watch.GateOptions, error) {
	scanOptions := watch.Options{LargeFileBytes: int64(options.LargeFileBytes)}

	if options.RulesPath != "" {
		config, err := readWatchRulesConfig(ctx, options.RulesPath)
		if err != nil {
			return watch.Options{}, nil, nil, watch.GateOptions{}, err
		}

		scanOptions.Rules = config.Rules
		scanOptions.IgnorePaths = append(scanOptions.IgnorePaths, config.IgnorePaths...)
	}

	if options.SuppressionsPath != "" {
		suppressions, err := readWatchSuppressions(ctx, options.SuppressionsPath)
		if err != nil {
			return watch.Options{}, nil, nil, watch.GateOptions{}, err
		}

		scanOptions.Suppressions = suppressions
	}

	var (
		baseline     *watch.Baseline
		baselineInfo *watchBaselineInfo
	)

	if options.BaselinePath != "" && options.BaselineRef != "" {
		return watch.Options{}, nil, nil, watch.GateOptions{}, errors.New("watch baseline: use only one of --watch-baseline or --watch-baseline-ref")
	}

	if options.BaselinePath != "" {
		loaded, err := readWatchBaseline(ctx, options.BaselinePath)
		if err != nil {
			return watch.Options{}, nil, nil, watch.GateOptions{}, err
		}

		baseline = &loaded
		baselineInfo = &watchBaselineInfo{
			Source:   "file",
			Path:     options.BaselinePath,
			Findings: len(loaded.Findings),
		}
	}

	if options.BaselineRef != "" {
		if !autonomy.Normalize(options.Autonomy).Allows(autonomy.ActionFileWrite) {
			return watch.Options{}, nil, nil, watch.GateOptions{}, fmt.Errorf("%s", autonomy.DenialMessage(options.Autonomy, autonomy.ActionFileWrite, "--watch-baseline-ref"))
		}

		loaded, commit, err := readWatchBaselineRef(ctx, root, scanOptions, options.BaselineRef, options.Autonomy)
		if err != nil {
			return watch.Options{}, nil, nil, watch.GateOptions{}, err
		}

		baseline = &loaded
		baselineInfo = &watchBaselineInfo{
			Source:   "git_merge_base",
			Ref:      options.BaselineRef,
			Commit:   commit,
			Findings: len(loaded.Findings),
		}
	}

	gateOptions := watch.GateOptions{
		Enabled:     options.GateEnabled || options.GateMinSeverity != "",
		MinSeverity: strings.TrimSpace(options.GateMinSeverity),
	}
	if gateOptions.MinSeverity != "" && !validWatchGateSeverity(gateOptions.MinSeverity) {
		return watch.Options{}, nil, nil, watch.GateOptions{}, fmt.Errorf("watch gate min severity must be one of high, warning, maintenance, info: %q", gateOptions.MinSeverity)
	}

	return scanOptions, baseline, baselineInfo, gateOptions, nil
}

func upsertWatchScanIssues(ctx context.Context, options watchCLIOptions, tracker watch.IssueTracker, output *watchScanOutput) error {
	tracker, issueOptions, err := watchIssueInputs(ctx, options, tracker)
	if err != nil {
		return err
	}

	if tracker == nil {
		return nil
	}

	if output.Comparison == nil {
		comparison := watch.CompareFindings(nil, output.Findings)
		output.Comparison = &comparison
	}

	issues, err := watch.UpsertIssues(ctx, tracker, *output.Comparison, issueOptions)
	output.Issues = append([]watch.IssueUpsertResult(nil), issues...)

	if err != nil {
		return fmt.Errorf("watch issue upsert: %w", err)
	}

	return nil
}

func authorizeWatchScanRead(ctx context.Context, root string) error {
	if err := authorizeReadPermission(ctx, "scan watch root", "atteler.watch", root); err != nil {
		return fmt.Errorf("watch scan: authorize read: %w", err)
	}

	return nil
}

func watchIssueInputs(ctx context.Context, options watchCLIOptions, tracker watch.IssueTracker) (watch.IssueTracker, watch.IssueOptions, error) {
	issueOptions, err := watchIssueOptions(options)
	if err != nil {
		return nil, watch.IssueOptions{}, err
	}

	if !options.IssueUpsert {
		return nil, issueOptions, nil
	}

	if !autonomy.Normalize(options.Autonomy).Allows(autonomy.ActionRemoteMutation) {
		return nil, watch.IssueOptions{}, fmt.Errorf("%s", autonomy.DenialMessage(options.Autonomy, autonomy.ActionRemoteMutation, "--watch-issue-upsert"))
	}

	if tracker != nil {
		return tracker, issueOptions, nil
	}

	config, err := watchGitHubTrackerConfig(ctx, options)
	if err != nil {
		return nil, watch.IssueOptions{}, err
	}

	return symphony.NewGitHubClient(config), issueOptions, nil
}

func watchIssueOptions(options watchCLIOptions) (watch.IssueOptions, error) {
	minSeverity := strings.TrimSpace(options.IssueMinSeverity)
	if minSeverity != "" && !validWatchGateSeverity(minSeverity) {
		return watch.IssueOptions{}, fmt.Errorf("watch issue min severity must be one of high, warning, maintenance, info: %q", minSeverity)
	}

	labels := append([]string(nil), options.IssueLabels...)
	if len(labels) == 0 {
		labels = []string{"quality", "watch"}
	}

	return watch.IssueOptions{
		MinSeverity: minSeverity,
		Labels:      labels,
	}, nil
}

func watchGitHubTrackerConfig(ctx context.Context, options watchCLIOptions) (symphony.TrackerConfig, error) {
	owner, repo := splitGitHubRepository(options.IssueRepository)
	if owner == "" || repo == "" {
		return symphony.TrackerConfig{}, errors.New("watch issue upsert requires --watch-github-repository owner/repo")
	}

	if err := authorizeWatchGitHubTokenAccess(ctx); err != nil {
		return symphony.TrackerConfig{}, err
	}

	token := firstNonEmpty(strings.TrimSpace(options.GitHubToken), os.Getenv("GITHUB_TOKEN"), os.Getenv("GH_TOKEN"))
	if token == "" {
		return symphony.TrackerConfig{}, errors.New("watch issue upsert requires --watch-github-token, GITHUB_TOKEN, or GH_TOKEN")
	}

	endpoint := strings.TrimSpace(options.GitHubEndpoint)
	if endpoint == "" {
		endpoint = defaultGitHubAPIEndpoint
	}

	return symphony.TrackerConfig{
		Endpoint: endpoint,
		APIKey:   token,
		Owner:    owner,
		Repo:     repo,
	}, nil
}

func authorizeWatchGitHubTokenAccess(ctx context.Context) error {
	decision := permission.Evaluate(ctx, nil, permission.Request{
		Action: "resolve watch GitHub token",
		Source: "atteler.watch.github",
		Target: "--watch-github-token/GITHUB_TOKEN/GH_TOKEN",
		Operations: []permission.Operation{{
			Kind:   permission.OperationCredentialAccess,
			Action: "resolve watch GitHub token",
			Source: "atteler.watch.github",
			Target: "--watch-github-token/GITHUB_TOKEN/GH_TOKEN",
		}},
	})
	if decision.Allowed {
		return nil
	}

	return &permission.Error{Decision: decision}
}

func splitGitHubRepository(repository string) (owner, repo string) {
	parts := strings.Split(strings.TrimSpace(repository), "/")
	if len(parts) != 2 {
		return "", ""
	}

	return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
}

func validWatchGateSeverity(severity string) bool {
	switch severity {
	case watch.SeverityHigh, watch.SeverityWarning, watch.SeverityMaintenance, watch.SeverityInfo:
		return true
	default:
		return false
	}
}

func readWatchBaseline(ctx context.Context, path string) (watch.Baseline, error) {
	if err := authorizeReadPermission(ctx, "read watch baseline", "atteler.watch", path); err != nil {
		return watch.Baseline{}, fmt.Errorf("watch baseline: authorize read: %w", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return watch.Baseline{}, fmt.Errorf("watch baseline: read %s: %w", path, err)
	}

	var baseline watch.Baseline
	if err := decodeWatchJSON(data, &baseline, &baseline.Findings); err != nil {
		return watch.Baseline{}, fmt.Errorf("watch baseline: decode %s: %w", path, err)
	}

	return baseline, nil
}

func readWatchBaselineRef(ctx context.Context, root string, scanOptions watch.Options, ref string, level autonomy.Level) (watch.Baseline, string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return watch.Baseline{}, "", errors.New("watch baseline ref is required")
	}

	mergeBase, err := gitMergeBase(ctx, root, ref, level)
	if err != nil {
		return watch.Baseline{}, "", err
	}

	if authErr := authorizeWatchBaselineRefMaterialization(ctx, ref, mergeBase); authErr != nil {
		return watch.Baseline{}, "", fmt.Errorf("watch baseline ref %s: %w", ref, authErr)
	}

	tmp, err := os.MkdirTemp("", "atteler-watch-baseline-*")
	if err != nil {
		return watch.Baseline{}, "", fmt.Errorf("watch baseline ref: create temp dir: %w", err)
	}
	defer os.RemoveAll(tmp)

	if archiveErr := gitArchiveToDir(ctx, root, mergeBase, tmp, level); archiveErr != nil {
		return watch.Baseline{}, "", archiveErr
	}

	findings, err := watch.ScanWithOptions(tmp, scanOptions)
	if err != nil {
		return watch.Baseline{}, "", fmt.Errorf("watch baseline ref %s: scan merge-base %s: %w", ref, mergeBase, err)
	}

	return watch.Baseline{Findings: findings}, mergeBase, nil
}

func authorizeWatchBaselineRefMaterialization(ctx context.Context, ref, mergeBase string) error {
	target := strings.TrimSpace(ref)
	if strings.TrimSpace(mergeBase) != "" {
		target += "@" + strings.TrimSpace(mergeBase)
	}

	if target == "" {
		target = "watch baseline ref"
	}

	action := "materialize watch baseline ref"
	operations := []permission.Operation{
		{
			Kind:   permission.OperationWrite,
			Action: action,
			Source: "atteler.watch.baseline_ref",
			Target: target,
		},
		{
			Kind:   permission.OperationMergeDelete,
			Action: action,
			Source: "atteler.watch.baseline_ref",
			Target: target,
		},
	}

	decision := permission.Evaluate(ctx, nil, permission.Request{
		Action:     action,
		Source:     "atteler.watch.baseline_ref",
		Target:     target,
		Operations: operations,
	})
	if decision.Allowed {
		return nil
	}

	return &permission.Error{Decision: decision}
}

func gitMergeBase(ctx context.Context, root, ref string, level autonomy.Level) (string, error) {
	output, err := runGitOutput(ctx, root, level, "merge-base", "HEAD", ref)
	if err != nil {
		return "", fmt.Errorf("watch baseline ref %s: find merge-base: %w", ref, err)
	}

	mergeBase := strings.TrimSpace(string(output))
	if mergeBase == "" {
		return "", fmt.Errorf("watch baseline ref %s: empty merge-base", ref)
	}

	return mergeBase, nil
}

func gitArchiveToDir(ctx context.Context, root, ref, dir string, level autonomy.Level) error {
	data, err := runGitOutput(ctx, root, level, "archive", "--format=tar", ref)
	if err != nil {
		return fmt.Errorf("watch baseline ref %s: archive: %w", ref, err)
	}

	reader := tar.NewReader(bytes.NewReader(data))
	for {
		header, nextErr := reader.Next()
		if errors.Is(nextErr, io.EOF) {
			return nil
		}

		if nextErr != nil {
			return fmt.Errorf("watch baseline ref %s: read archive: %w", ref, nextErr)
		}

		if err := extractTarEntry(reader, header, dir); err != nil {
			return fmt.Errorf("watch baseline ref %s: extract %s: %w", ref, header.Name, err)
		}
	}
}

func extractTarEntry(reader io.Reader, header *tar.Header, dir string) error {
	target, err := safeArchivePath(dir, header.Name)
	if err != nil {
		return err
	}

	switch header.Typeflag {
	case tar.TypeDir:
		if err := os.MkdirAll(target, 0o750); err != nil {
			return fmt.Errorf("create archive directory: %w", err)
		}

		return nil
	case tar.TypeReg:
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return fmt.Errorf("create archive parent directory: %w", err)
		}

		file, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, header.FileInfo().Mode().Perm())
		if err != nil {
			return fmt.Errorf("create archive file: %w", err)
		}
		defer file.Close()

		_, err = io.Copy(file, reader)
		if err != nil {
			return fmt.Errorf("copy archive file: %w", err)
		}

		return nil
	default:
		return nil
	}
}

func safeArchivePath(dir, name string) (string, error) {
	cleanName := filepath.Clean(name)
	if filepath.IsAbs(cleanName) || cleanName == ".." || strings.HasPrefix(cleanName, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe archive path %q", name)
	}

	target := filepath.Join(dir, cleanName)

	relative, err := filepath.Rel(dir, target)
	if err != nil {
		return "", fmt.Errorf("check archive path: %w", err)
	}

	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("archive path escapes destination: %q", name)
	}

	return target, nil
}

func runGitOutput(ctx context.Context, root string, level autonomy.Level, args ...string) ([]byte, error) {
	fullArgs := append([]string{"-C", root}, args...)

	result, err := shell.RunCommand(ctx, shell.CommandOptions{
		Program: "git",
		Args:    fullArgs,
		Mode:    shell.ModeCaptured,
		Audit:   watchGitAuditContext(level),
	})
	if err != nil {
		output := strings.TrimSpace(result.Stdout + result.Stderr)
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, output)
	}

	return []byte(result.Stdout), nil
}

func watchGitAuditContext(level autonomy.Level) shell.AuditContext {
	return shell.AuditContext{
		Caller:   "atteler.watch.git",
		Autonomy: autonomy.Normalize(level).String(),
	}
}

type watchRulesConfig struct {
	Rules       []watch.RuleConfig `json:"rules"`
	IgnorePaths []string           `json:"ignore_paths"`
}

func readWatchRules(ctx context.Context, path string) ([]watch.RuleConfig, error) {
	config, err := readWatchRulesConfig(ctx, path)
	if err != nil {
		return nil, err
	}

	return config.Rules, nil
}

func readWatchRulesConfig(ctx context.Context, path string) (watchRulesConfig, error) {
	if err := authorizeReadPermission(ctx, "read watch rules", "atteler.watch", path); err != nil {
		return watchRulesConfig{}, fmt.Errorf("watch rules: authorize read: %w", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return watchRulesConfig{}, fmt.Errorf("watch rules: read %s: %w", path, err)
	}

	var payload watchRulesConfig
	if err := decodeWatchJSON(data, &payload, &payload.Rules); err != nil {
		return watchRulesConfig{}, fmt.Errorf("watch rules: decode %s: %w", path, err)
	}

	payload.IgnorePaths = trimNonEmptyStringSlice(payload.IgnorePaths)

	return payload, nil
}

func readWatchSuppressions(ctx context.Context, path string) ([]watch.Suppression, error) {
	if err := authorizeReadPermission(ctx, "read watch suppressions", "atteler.watch", path); err != nil {
		return nil, fmt.Errorf("watch suppressions: authorize read: %w", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("watch suppressions: read %s: %w", path, err)
	}

	var payload struct {
		Suppressions []watch.Suppression `json:"suppressions"`
	}
	if err := decodeWatchJSON(data, &payload, &payload.Suppressions); err != nil {
		return nil, fmt.Errorf("watch suppressions: decode %s: %w", path, err)
	}

	return payload.Suppressions, nil
}

func decodeWatchJSON(data []byte, object, array any) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		if err := json.Unmarshal(trimmed, array); err != nil {
			return fmt.Errorf("decode array payload: %w", err)
		}

		return nil
	}

	if err := json.Unmarshal(trimmed, object); err != nil {
		return fmt.Errorf("decode object payload: %w", err)
	}

	return nil
}

func trimNonEmptyStringSlice(values []string) []string {
	trimmed := make([]string, 0, len(values))
	for i := range values {
		value := strings.TrimSpace(values[i])
		if value != "" {
			trimmed = append(trimmed, value)
		}
	}

	return trimmed
}

func buildWatchScanOutput(findings []watch.Finding, baseline *watch.Baseline, baselineInfo *watchBaselineInfo, gateOptions watch.GateOptions) watchScanOutput {
	output := watchScanOutput{
		Baseline: cloneWatchBaselineInfo(baselineInfo),
		Findings: append([]watch.Finding(nil), findings...),
	}
	if baseline == nil && !gateOptions.Enabled {
		return output
	}

	var baselineFindings []watch.Finding
	if baseline != nil {
		baselineFindings = baseline.Findings
	}

	comparison := watch.CompareFindings(baselineFindings, findings)
	output.Comparison = &comparison

	if gateOptions.Enabled {
		gate := watch.EvaluateGate(comparison, gateOptions)
		output.Gate = &gate
	}

	return output
}

func cloneWatchBaselineInfo(info *watchBaselineInfo) *watchBaselineInfo {
	if info == nil {
		return nil
	}

	clone := *info

	return &clone
}

func watchGateError(gate *watch.GateResult) error {
	if gate == nil || gate.Passed {
		return nil
	}

	return fmt.Errorf("watch gate %q failed: %s", gate.Name, gate.Reason)
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

	if result.Comparison != nil {
		parts = append(parts,
			"new="+strconv.Itoa(result.Comparison.Metrics.New),
			"fixed="+strconv.Itoa(result.Comparison.Metrics.Fixed),
			"unchanged="+strconv.Itoa(result.Comparison.Metrics.Unchanged),
			"suppressed="+strconv.Itoa(result.Comparison.Metrics.Suppressed),
			"unstable="+strconv.Itoa(result.Comparison.Metrics.Unstable),
		)
	}

	if result.Gate != nil {
		parts = append(parts,
			"gate="+result.Gate.Name,
			"gate_passed="+strconv.FormatBool(result.Gate.Passed),
		)
	}

	if len(result.Issues) > 0 {
		parts = append(parts, "issues="+strconv.Itoa(len(result.Issues)))
	}

	return strings.Join(parts, "\t")
}

func formatWatchComparison(comparison watch.Comparison) string {
	return strings.Join([]string{
		"watch_comparison",
		"new=" + strconv.Itoa(comparison.Metrics.New),
		"fixed=" + strconv.Itoa(comparison.Metrics.Fixed),
		"unchanged=" + strconv.Itoa(comparison.Metrics.Unchanged),
		"suppressed=" + strconv.Itoa(comparison.Metrics.Suppressed),
		"unstable=" + strconv.Itoa(comparison.Metrics.Unstable),
	}, "\t")
}

func formatWatchBaseline(info watchBaselineInfo) string {
	parts := []string{
		"watch_baseline",
		"source=" + info.Source,
		"findings=" + strconv.Itoa(info.Findings),
	}
	if info.Path != "" {
		parts = append(parts, "path="+info.Path)
	}

	if info.Ref != "" {
		parts = append(parts, "ref="+info.Ref)
	}

	if info.Commit != "" {
		parts = append(parts, "commit="+info.Commit)
	}

	return strings.Join(parts, "\t")
}

func formatWatchGate(gate watch.GateResult) string {
	parts := []string{
		"watch_gate",
		"name=" + gate.Name,
		"passed=" + strconv.FormatBool(gate.Passed),
	}
	if gate.Reason != "" {
		parts = append(parts, "reason="+gate.Reason)
	}

	if len(gate.BlockingFindings) > 0 {
		parts = append(parts, "blocking_findings="+strconv.Itoa(len(gate.BlockingFindings)))
	}

	return strings.Join(parts, "\t")
}

func printWatchIssueUpserts(results []watch.IssueUpsertResult) {
	for i := range results {
		fmt.Println(formatWatchIssueUpsert(results[i]))
	}
}

func formatWatchIssueUpsert(result watch.IssueUpsertResult) string {
	parts := []string{
		"watch_issue",
		"action=" + result.Action,
	}
	if result.Issue.Number > 0 {
		parts = append(parts, "number="+strconv.Itoa(result.Issue.Number))
	}

	if result.Issue.URL != "" {
		parts = append(parts, "url="+result.Issue.URL)
	}

	if result.Issue.Fingerprint != "" {
		parts = append(parts, "fingerprint="+result.Issue.Fingerprint)
	}

	if result.Finding.ID != "" {
		parts = append(parts, "finding_id="+result.Finding.ID)
	}

	return strings.Join(parts, "\t")
}

func printWatchComparisonFindings(comparison watch.Comparison) bool {
	printed := false

	for _, group := range []struct {
		status   string
		findings []watch.Finding
	}{
		{status: "new", findings: comparison.NewFindings},
		{status: "fixed", findings: comparison.FixedFindings},
		{status: "unchanged", findings: comparison.UnchangedFindings},
		{status: "suppressed", findings: comparison.SuppressedFindings},
		{status: "unstable", findings: comparison.UnstableFindings},
	} {
		for i := range group.findings {
			fmt.Println(formatWatchFindingWithStatus(group.status, group.findings[i]))

			printed = true
		}
	}

	return printed
}

func formatWatchFindingWithStatus(status string, finding watch.Finding) string {
	status = strings.TrimSpace(status)
	if status == "" {
		return formatWatchFinding(finding)
	}

	return "status=" + status + "\t" + formatWatchFinding(finding)
}

func formatWatchFinding(finding watch.Finding) string {
	parts := []string{
		"path=" + finding.Path,
		"kind=" + finding.Kind,
		"severity=" + finding.Severity,
	}
	if finding.ID != "" {
		parts = append(parts, "id="+finding.ID)
	}

	if finding.Fingerprint != "" {
		parts = append(parts, "fingerprint="+finding.Fingerprint)
	}

	if finding.Message != "" {
		parts = append(parts, "message="+finding.Message)
	}

	if finding.RuleID != "" {
		parts = append(parts, "rule_id="+finding.RuleID)
	}

	if finding.RuleDescription != "" {
		parts = append(parts, "rule_description="+finding.RuleDescription)
	}

	if finding.Help != "" {
		parts = append(parts, "help="+finding.Help)
	}

	if finding.Owner != "" {
		parts = append(parts, "owner="+finding.Owner)
	}

	if finding.Suppressed {
		parts = append(parts, "suppressed=true")
	}

	if finding.SuppressionReason != "" {
		parts = append(parts, "suppression_reason="+finding.SuppressionReason)
	}

	return strings.Join(parts, "\t")
}

func runGitHistorySearch(ctx context.Context, root string, input gitHistorySearchCommandInput, level autonomy.Level) error {
	if input.Limit == 0 {
		input.Limit = 5
	}

	query, err := gitHistoryQuery(input)
	if err != nil {
		return err
	}

	commits, err := gitHistoryCommits(ctx, root, level, gitHistoryCollectOptions{
		Query:        query,
		IncludeHunks: input.IncludeHunks,
		MaxHunkBytes: input.MaxHunkBytes,
	})
	if err != nil {
		return err
	}

	results := githistory.NewIndex(commits).Search(input.Query, input.Limit)
	if len(results) == 0 {
		fmt.Println("No git history results found.")
		return nil
	}

	for i := range results {
		fmt.Println(formatGitHistoryResult(results[i]))
	}

	return nil
}

//nolint:govet // Field order follows collector option grouping.
type gitHistoryCollectOptions struct {
	Query        githistory.Query
	Audit        shell.AuditContext
	MaxHunkBytes int
	IncludeHunks bool
	RedactOutput func(string) string
	OutputNote   string
}

func gitHistoryQuery(input gitHistorySearchCommandInput) (githistory.Query, error) {
	if input.NoMerges && input.MergesOnly {
		return githistory.Query{}, errors.New("git-history-no-merges and git-history-merges cannot both be set")
	}

	since, err := parseGitHistoryDateBound(input.Since, "git-history-since")
	if err != nil {
		return githistory.Query{}, err
	}

	until, err := parseGitHistoryDateBound(input.Until, "git-history-until")
	if err != nil {
		return githistory.Query{}, err
	}

	return githistory.Query{
		Range:       strings.TrimSpace(input.Range),
		Refs:        append([]string(nil), input.Refs...),
		Paths:       append([]string(nil), input.Paths...),
		Authors:     append([]string(nil), input.Authors...),
		Since:       since,
		Until:       until,
		All:         input.All,
		FirstParent: input.FirstParent,
		NoMerges:    input.NoMerges,
		MergesOnly:  input.MergesOnly,
	}, nil
}

func parseGitHistoryDateBound(raw, name string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, nil
	}

	if value, err := time.Parse(time.RFC3339, raw); err == nil {
		return value, nil
	}

	value, err := time.Parse("2006-01-02", raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s must be RFC3339 or YYYY-MM-DD: %w", name, err)
	}

	return value, nil
}

func gitHistoryCommits(ctx context.Context, root string, level autonomy.Level, opts gitHistoryCollectOptions) ([]githistory.Commit, error) {
	level = autonomy.Normalize(level)
	if level == autonomy.Low {
		return nil, errors.New("git history: autonomy low is advisory-only and blocks git history shell execution; rerun with --autonomy medium or higher")
	}

	audit := opts.Audit
	if strings.TrimSpace(audit.Caller) == "" {
		audit = gitHistoryAuditContext(level)
	} else if strings.TrimSpace(audit.Autonomy) == "" {
		audit.Autonomy = level.String()
	}

	commits, err := githistory.Collect(ctx, githistory.CollectorOptions{
		RepoDir:      root,
		Query:        opts.Query,
		Audit:        audit,
		MaxHunkBytes: opts.MaxHunkBytes,
		IncludeHunks: opts.IncludeHunks,
		RedactOutput: opts.RedactOutput,
		OutputNote:   opts.OutputNote,
	})
	if err != nil {
		if strings.Contains(err.Error(), "githistory: git log:") {
			return nil, fmt.Errorf("git history: run git log: %w", err)
		}

		return nil, fmt.Errorf("git history: collect: %w", err)
	}

	return commits, nil
}

func gitHistoryAuditContext(level autonomy.Level) shell.AuditContext {
	return shell.AuditContext{
		Caller:   "atteler.git_history",
		Autonomy: autonomy.Normalize(level).String(),
	}
}

func formatGitHistoryResult(result githistory.Result) string {
	commit := result.Commit

	parts := []string{
		shortCommitHash(commit.Hash),
		fmt.Sprintf("score=%d", result.Score),
	}
	if result.Confidence > 0 {
		parts = append(parts, fmt.Sprintf("confidence=%.2f", result.Confidence))
	}
	if fields := gitHistoryMatchedFields(result.Matches); fields != "" {
		parts = append(parts, "matched_fields="+fields)
	}
	if result.RangeContext != "" {
		parts = append(parts, "range="+result.RangeContext)
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
	if changes := gitHistoryChangedFilesSummary(commit.Changes); changes != "" {
		parts = append(parts, "changes="+changes)
	}
	if commit.DiffTruncated {
		parts = append(parts, "diff_truncated=true")
	}

	for _, snippet := range result.Snippets {
		if snippet.Text != "" {
			parts = append(parts, snippet.Field+"="+snippet.Text)
			break
		}
	}

	return strings.Join(parts, "\t")
}

func gitHistoryMatchedFields(matches []githistory.MatchEvidence) string {
	seen := make(map[string]struct{})
	fields := make([]string, 0)
	for _, match := range matches {
		field := strings.TrimSpace(match.Field)
		if field == "" {
			continue
		}
		if _, ok := seen[field]; ok {
			continue
		}

		seen[field] = struct{}{}
		fields = append(fields, field)
	}

	slices.Sort(fields)
	return strings.Join(fields, ",")
}

func gitHistoryChangedFilesSummary(changes []githistory.ChangedFile) string {
	if len(changes) == 0 {
		return ""
	}

	limit := min(len(changes), 3)
	parts := make([]string, 0, limit+1)
	for i := range limit {
		change := changes[i]
		entry := change.Path
		var details []string
		if change.Status != "" {
			details = append(details, change.Status)
		}
		if change.OldPath != "" {
			details = append(details, "from="+change.OldPath)
		}
		if change.Binary {
			details = append(details, "binary")
		} else if change.Added != 0 || change.Deleted != 0 {
			details = append(details, fmt.Sprintf("+%d/-%d", change.Added, change.Deleted))
		}
		if len(details) > 0 {
			entry += "(" + strings.Join(details, ",") + ")"
		}
		parts = append(parts, entry)
	}
	if len(changes) > limit {
		parts = append(parts, fmt.Sprintf("+%d more", len(changes)-limit))
	}

	return strings.Join(parts, ";")
}

func shortCommitHash(hash string) string {
	if len(hash) <= 12 {
		return hash
	}

	return hash[:12]
}
