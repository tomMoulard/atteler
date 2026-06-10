//nolint:wsl_v5 // The one-shot issue implementation flow is intentionally linear.
package symphony

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
)

// IssueImplementOptions configures a single issue-to-PR run outside the
// polling orchestrator.
type IssueImplementOptions struct {
	Logger          *slog.Logger
	WorkflowPath    string
	WorkDir         string
	IssueRef        string
	BaseBranch      string
	OpenPullRequest bool
	RunTests        bool
	RunLint         bool
	UpdateDocs      bool
	UpdateChangelog bool
}

// ImplementIssue runs one issue through the same workspace, agent, verification,
// and publication path used by the Symphony scheduler.
func ImplementIssue(ctx context.Context, opts IssueImplementOptions) (RunResult, error) {
	if ctx == nil {
		return RunResult{}, errors.New("symphony issue implement: context is required")
	}

	issueRef := strings.TrimSpace(opts.IssueRef)
	if issueRef == "" {
		return RunResult{}, errors.New("symphony issue implement: issue reference is required")
	}

	logger := loggerOrDefault(opts.Logger)
	snapshot, err := loadIssueImplementWorkflow(ctx, opts)
	if err != nil {
		return RunResult{}, err
	}

	applyIssueImplementOverrides(&snapshot.Config, opts)
	if validateErr := snapshot.Config.validateIssueImplementPublishConfig(); validateErr != nil {
		return RunResult{}, validateErr
	}

	tracker, err := NewTrackerClient(snapshot.Config)
	if err != nil {
		return RunResult{}, err
	}
	tracker = issueImplementTracker(snapshot.Config, tracker)

	issue, err := fetchIssueForImplementation(ctx, tracker, snapshot.Config, issueRef)
	if err != nil {
		return RunResult{}, err
	}
	issue = issueWithImplementationFlags(issue, opts)

	runner := NewDefaultAgentRunner(tracker, logger)
	result, runErr := runner.Run(ctx, RunRequest{
		Config:   snapshot.Config,
		Workflow: snapshot.Definition,
		Issue:    issue,
		Context:  &RunContext{Kind: RunKindIssue},
	}, nil)
	if runErr != nil {
		return publishFailureDraftForRunError(ctx, snapshot.Config, issue, result, runErr, logger)
	}

	result, err = publishNoChangeDraftIfNeeded(ctx, snapshot.Config, issue, result, logger)
	if err != nil {
		return result, err
	}

	return failIssueImplementationValidationDraftIfNeeded(result)
}

func loadIssueImplementWorkflow(ctx context.Context, opts IssueImplementOptions) (WorkflowSnapshot, error) {
	manager, err := NewWorkflowManager(opts.WorkDir, opts.WorkflowPath)
	if err != nil {
		return WorkflowSnapshot{}, err
	}

	data, info, err := readWorkflowFile(manager.Path())
	if err != nil {
		return WorkflowSnapshot{}, err
	}

	def, err := ParseWorkflow(data)
	if err != nil {
		return WorkflowSnapshot{}, err
	}

	config, err := issueImplementPreflightConfig(def.Config)
	if err != nil {
		return WorkflowSnapshot{}, err
	}

	cfg, err := ResolveConfig(ctx, config, manager.Path())
	if err != nil {
		return WorkflowSnapshot{}, err
	}

	return WorkflowSnapshot{
		Definition: def,
		Config:     cfg,
		ModTime:    info.ModTime(),
		Size:       info.Size(),
	}, nil
}

func issueImplementPreflightConfig(config map[string]any) (map[string]any, error) {
	out := cloneConfigMap(config)
	publish, err := issueImplementPublishConfig(out)
	if err != nil {
		return nil, err
	}

	if publish == nil {
		publish = map[string]any{}
		out["publish"] = publish
	}

	// Load one-shot runs with scheduler publication disabled, then validate the
	// requested --open-pr publication mode after applying CLI overrides. This
	// keeps long-lived scheduler safeguards such as required remove_labels out
	// of the one-shot path, where there may be no dispatch label to remove.
	publish["enabled"] = false

	return out, nil
}

func issueImplementPublishConfig(config map[string]any) (map[string]any, error) {
	value, ok := config["publish"]
	if !ok || value == nil {
		return nil, nil
	}

	publish, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("symphony issue implement: publish config must be a map, got %T", value)
	}

	return publish, nil
}

func cloneConfigMap(config map[string]any) map[string]any {
	out := make(map[string]any, len(config))
	for key, value := range config {
		out[key] = cloneConfigValue(value)
	}

	return out
}

func cloneConfigValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneConfigMap(typed)
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = cloneConfigValue(item)
		}

		return out
	default:
		return value
	}
}

func publishFailureDraftForRunError(ctx context.Context, cfg Config, issue Issue, result RunResult, runErr error, logger *slog.Logger) (RunResult, error) {
	if canPublishIssueImplementationFailure(cfg, result, runErr) {
		publishResult, publishErr := PublishFailureDraft(ctx, cfg, issue, Workspace{
			Path:         result.WorkspacePath,
			WorkspaceKey: SanitizeWorkspaceKey(issue.Identifier),
		}, runErr, logger)
		if publishResult != nil {
			result.Publish = publishResult
		}
		if publishErr != nil {
			return result, errors.Join(runErr, fmt.Errorf("publish failure draft: %w", publishErr))
		}
	}

	return result, runErr
}

func publishNoChangeDraftIfNeeded(ctx context.Context, cfg Config, issue Issue, result RunResult, logger *slog.Logger) (RunResult, error) {
	if !isIssueImplementationNoChange(cfg, result) {
		return result, nil
	}

	noChangeErr := noChangeIssueImplementationError(result)
	if !canPublishIssueImplementationNoChangeDraft(cfg, result) {
		result.Status = AttemptFailed
		result.Error = noChangeErr.Error()
		return result, noChangeErr
	}

	publishResult, publishErr := PublishFailureDraft(ctx, cfg, issue, Workspace{
		Path:         result.WorkspacePath,
		WorkspaceKey: SanitizeWorkspaceKey(issue.Identifier),
	}, noChangeErr, logger)
	if publishResult != nil {
		result.Publish = publishResult
	}
	if publishErr != nil {
		result.Status = AttemptFailed
		result.Error = noChangeErr.Error()
		return result, errors.Join(noChangeErr, fmt.Errorf("publish no-change draft: %w", publishErr))
	}

	result.Status = AttemptFailed
	result.Error = noChangeErr.Error()

	return result, noChangeErr
}

func failIssueImplementationValidationDraftIfNeeded(result RunResult) (RunResult, error) {
	if result.Publish == nil || !result.Publish.DraftDueToFailedVerification {
		return result, nil
	}

	validationErr := failedValidationDraftIssueImplementationError(result.Publish.Verification)
	result.Status = AttemptFailed
	result.Error = validationErr.Error()

	return result, validationErr
}

func failedValidationDraftIssueImplementationError(report *VerificationReport) error {
	if report == nil {
		return errors.New("symphony issue implement: draft pull request opened after required verification failed")
	}

	return fmt.Errorf(
		"symphony issue implement: draft pull request opened after failed verification: %w",
		&VerificationGateError{Report: *report},
	)
}

func noChangeIssueImplementationError(result RunResult) error {
	if result.Publish != nil && strings.TrimSpace(result.Publish.SkippedReason) != "" {
		return fmt.Errorf("symphony issue implement: %s", result.Publish.SkippedReason)
	}

	return errors.New("symphony issue implement: worker completed without publishable changes")
}

func canPublishIssueImplementationNoChangeDraft(cfg Config, result RunResult) bool {
	return cfg.Publish.DraftOnFailedValidation && isIssueImplementationNoChange(cfg, result)
}

func isIssueImplementationNoChange(cfg Config, result RunResult) bool {
	if !cfg.Publish.Enabled || result.Publish == nil {
		return false
	}

	return !result.Publish.Published &&
		strings.TrimSpace(result.Publish.SkippedReason) != "" &&
		strings.TrimSpace(result.WorkspacePath) != ""
}

func canPublishIssueImplementationFailure(cfg Config, result RunResult, runErr error) bool {
	if runErr == nil || !cfg.Publish.Enabled || !cfg.Publish.DraftOnFailedValidation {
		return false
	}

	if errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) {
		return false
	}

	var gateErr *VerificationGateError
	if errors.As(runErr, &gateErr) {
		return false
	}

	return result.Publish == nil && strings.TrimSpace(result.WorkspacePath) != ""
}

func applyIssueImplementOverrides(cfg *Config, opts IssueImplementOptions) {
	if cfg == nil {
		return
	}

	// One-shot issue implementation is safe by default: it only publishes when
	// the caller explicitly passes --open-pr. Long-lived Symphony scheduler runs
	// still honor publish.enabled from WORKFLOW.md because they do not go through
	// this override path.
	cfg.Publish.Enabled = opts.OpenPullRequest

	if strings.TrimSpace(opts.BaseBranch) != "" {
		cfg.Publish.BaseBranch = strings.TrimSpace(opts.BaseBranch)
	}

	seedFlagAllowCommands := len(cfg.Publish.VerificationAllowCommands) > 0 || len(cfg.Publish.VerificationGates) == 0
	if opts.RunTests {
		appendVerificationGateIfMissing(&cfg.Publish.VerificationGates, verificationGateFromString("go_test"))
		appendVerificationAllowedCommandForIssueFlag(&cfg.Publish.VerificationAllowCommands, "go", seedFlagAllowCommands)
	}

	if opts.RunLint {
		appendVerificationGateIfMissing(&cfg.Publish.VerificationGates, verificationGateFromString("golangci_lint"))
		appendVerificationAllowedCommandForIssueFlag(&cfg.Publish.VerificationAllowCommands, "make", seedFlagAllowCommands)
	}
}

func appendVerificationGateIfMissing(gates *[]VerificationGateConfig, gate VerificationGateConfig) {
	if gates == nil || strings.TrimSpace(gate.Name) == "" {
		return
	}

	for i := range *gates {
		if sameVerificationGate((*gates)[i], gate) {
			mergeVerificationGate(&(*gates)[i], gate)
			return
		}
	}

	*gates = append(*gates, gate)
}

func mergeVerificationGate(existing *VerificationGateConfig, requested VerificationGateConfig) {
	if existing == nil {
		return
	}

	if requested.Required {
		existing.Required = true
	}

	if existing.Timeout <= 0 {
		existing.Timeout = requested.Timeout
	}

	if strings.TrimSpace(existing.Command) == "" {
		existing.Command = requested.Command
	}

	if strings.TrimSpace(existing.Name) == "" {
		existing.Name = requested.Name
	}
}

func sameVerificationGate(a, b VerificationGateConfig) bool {
	if strings.EqualFold(strings.TrimSpace(a.Name), strings.TrimSpace(b.Name)) {
		return true
	}

	aCommand := normalizedVerificationCommand(a.Command)
	bCommand := normalizedVerificationCommand(b.Command)

	return aCommand != "" && bCommand != "" && aCommand == bCommand
}

func normalizedVerificationCommand(command string) string {
	return strings.Join(strings.Fields(command), " ")
}

func appendVerificationAllowedCommandForIssueFlag(commands *[]string, command string, enabled bool) {
	if !enabled || commands == nil {
		return
	}

	command = strings.TrimSpace(command)
	if command == "" {
		return
	}

	for _, existing := range *commands {
		if strings.EqualFold(strings.TrimSpace(existing), command) {
			return
		}
	}

	*commands = append(*commands, command)
}

func fetchIssueForImplementation(ctx context.Context, tracker TrackerClient, cfg Config, ref string) (Issue, error) {
	if tracker == nil {
		return Issue{}, errors.New("symphony issue implement: tracker is required")
	}

	candidates := issueReferenceCandidates(ref)
	var referenceLookupErr error
	if len(candidates) > 0 {
		issues, err := tracker.FetchIssueStatesByIDs(ctx, candidates)
		if err != nil {
			referenceLookupErr = fmt.Errorf("fetch issue by reference: %w", err)
		} else if issue, ok := firstMatchingIssueReference(issues, candidates); ok {
			return issueForImplementationRunner(cfg, issue), nil
		}

		// Some tracker adapters only support opaque node IDs in
		// FetchIssueStatesByIDs. Keep the direct lookup error for reporting, but
		// still try the configured state scan so human-facing references such as
		// ENG-123 can be resolved when the tracker lists them by identifier.
	}

	states := append([]string{}, cfg.Tracker.ActiveStates...)
	states = append(states, cfg.Tracker.TerminalStates...)
	issues, err := tracker.FetchIssuesByStates(ctx, states)
	if err != nil {
		if referenceLookupErr != nil {
			return Issue{}, errors.Join(referenceLookupErr, fmt.Errorf("fetch issues by state: %w", err))
		}

		return Issue{}, fmt.Errorf("fetch issues by state: %w", err)
	}

	if issue, ok := firstMatchingIssueReference(issues, append(candidates, ref)); ok {
		return issueForImplementationRunner(cfg, issue), nil
	}

	if referenceLookupErr != nil {
		return Issue{}, referenceLookupErr
	}

	return Issue{}, fmt.Errorf("symphony issue implement: issue %q not found", ref)
}

func issueImplementTracker(cfg Config, tracker TrackerClient) TrackerClient {
	if tracker == nil || normalizeState(cfg.Tracker.Kind) != trackerKindGitHub {
		return tracker
	}

	return issueImplementGitHubTracker{inner: tracker, cfg: cfg}
}

type issueImplementGitHubTracker struct {
	inner TrackerClient
	cfg   Config
}

func (t issueImplementGitHubTracker) FetchCandidateIssues(ctx context.Context) ([]Issue, error) {
	issues, err := t.inner.FetchCandidateIssues(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetch issue implementation candidates: %w", err)
	}

	return issuesForImplementationRunner(t.cfg, issues), nil
}

func (t issueImplementGitHubTracker) FetchIssuesByStates(ctx context.Context, states []string) ([]Issue, error) {
	issues, err := t.inner.FetchIssuesByStates(ctx, states)
	if err != nil {
		return nil, fmt.Errorf("fetch issue implementation states: %w", err)
	}

	return issuesForImplementationRunner(t.cfg, issues), nil
}

func (t issueImplementGitHubTracker) FetchIssueStatesByIDs(ctx context.Context, ids []string) ([]Issue, error) {
	issues, err := t.inner.FetchIssueStatesByIDs(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("fetch issue implementation IDs: %w", err)
	}

	return issuesForImplementationRunner(t.cfg, issues), nil
}

func issuesForImplementationRunner(cfg Config, issues []Issue) []Issue {
	if len(issues) == 0 || normalizeState(cfg.Tracker.Kind) != trackerKindGitHub {
		return issues
	}

	out := append([]Issue(nil), issues...)
	for i := range out {
		out[i] = issueForImplementationRunner(cfg, out[i])
	}

	return out
}

func issueForImplementationRunner(cfg Config, issue Issue) Issue {
	if normalizeState(cfg.Tracker.Kind) != trackerKindGitHub {
		return issue
	}

	// One-shot GitHub issue implementation may target issues that do not carry
	// the scheduler dispatch labels. Keep the refresh ID in a direct lookup
	// shape so the runner's post-turn refresh does not fall back to
	// label-filtered issue lists and miss the explicitly requested issue.
	if number, err := githubIssueNumber(issue); err == nil {
		issue.ID = "GH-" + strconv.Itoa(number)
	}

	return issue
}

func issueReferenceCandidates(ref string) []string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil
	}

	seen := map[string]struct{}{}
	add := func(value string, out *[]string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}

		key := strings.ToUpper(value)
		if _, ok := seen[key]; ok {
			return
		}

		seen[key] = struct{}{}
		*out = append(*out, value)
	}

	var out []string
	add(ref, &out)

	number := strings.TrimPrefix(ref, "#")
	number = strings.TrimPrefix(strings.ToUpper(number), "GH-")
	if parsed, err := strconv.Atoi(number); err == nil && parsed > 0 {
		add("GH-"+number, &out)
		add("GH-"+strconv.Itoa(parsed), &out)
	} else if parsed, ok := githubIssueNumberReference(ref); ok {
		add("GH-"+strconv.Itoa(parsed), &out)
	}

	return out
}

func firstMatchingIssueReference(issues []Issue, refs []string) (Issue, bool) {
	wanted := issueReferenceWantedSet(refs)
	for i := range issues {
		issue := issues[i]
		if issueMatchesReference(issue, wanted) {
			return issue, true
		}
	}

	return Issue{}, false
}

func issueReferenceWantedSet(refs []string) map[string]struct{} {
	wanted := make(map[string]struct{}, len(refs)*2)
	for _, ref := range refs {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			continue
		}

		wanted[strings.ToUpper(ref)] = struct{}{}
		if withoutTrailingSlash := strings.TrimRight(ref, "/"); withoutTrailingSlash != "" && withoutTrailingSlash != ref {
			wanted[strings.ToUpper(withoutTrailingSlash)] = struct{}{}
		}
		if number, ok := strings.CutPrefix(ref, "#"); ok {
			wanted["GH-"+number] = struct{}{}
		}
	}

	return wanted
}

func issueMatchesReference(issue Issue, wanted map[string]struct{}) bool {
	if referenceWanted(issue.Identifier, wanted) || referenceWanted(issue.ID, wanted) {
		return true
	}

	if issue.URL == nil {
		return false
	}

	issueURL := strings.TrimSpace(*issue.URL)
	normalizedIssueURL := strings.TrimRight(issueURL, "/")

	return referenceWanted(issueURL, wanted) || referenceWanted(normalizedIssueURL, wanted)
}

func referenceWanted(value string, wanted map[string]struct{}) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}

	_, ok := wanted[strings.ToUpper(value)]

	return ok
}

func issueWithImplementationFlags(issue Issue, opts IssueImplementOptions) Issue {
	var notes []string
	if opts.UpdateDocs {
		notes = append(notes, "Update documentation when relevant.")
	}

	if opts.UpdateChangelog {
		notes = append(notes, "Update the changelog when relevant.")
	}

	if len(notes) == 0 {
		return issue
	}

	description := ""
	if issue.Description != nil {
		description = strings.TrimSpace(*issue.Description)
	}

	extra := "Autonomous PR agent options:\n- " + strings.Join(notes, "\n- ")
	if description == "" {
		description = extra
	} else {
		description += "\n\n" + extra
	}
	issue.Description = &description

	return issue
}
