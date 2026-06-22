//nolint:wsl_v5 // Review-fix command flow keeps load/plan/apply/validate/report stages visually grouped.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/tommoulard/atteler/pkg/autonomy"
	"github.com/tommoulard/atteler/pkg/events"
	"github.com/tommoulard/atteler/pkg/reviewfix"
	attshell "github.com/tommoulard/atteler/pkg/shell"
)

const (
	reviewFixApplyModeAgent         = "agent"
	reviewFixApplyModeSuggestedDiff = "suggested-diff"
	reviewFixApplyModeNone          = "none"
	reviewFixValidationPassed       = "passed"
	reviewFixValidationFailed       = "failed"
	reviewFixValidationNotRun       = "not_run"
	reviewFixDefaultTimeout         = 10 * time.Minute
	reviewFixMaxOutputBytes         = 128 * 1024
)

func runReviewFix(ctx context.Context, state appState, input reviewFixCommandInput) error {
	if err := validateReviewFixRunInput(input); err != nil {
		return err
	}
	if !autonomy.Normalize(state.autonomy).Allows(autonomy.ActionFileWrite) {
		return fmt.Errorf("%s", autonomy.DenialMessage(state.autonomy, autonomy.ActionFileWrite, "review fix"))
	}
	if !autonomy.Normalize(state.autonomy).Allows(autonomy.ActionMutatingShell) {
		return fmt.Errorf("%s", autonomy.DenialMessage(state.autonomy, autonomy.ActionMutatingShell, "review fix"))
	}

	root := reviewFixWorkspaceRoot(state)
	inputPath := resolveWorkspacePath(state.cwd, input.From)
	if err := authorizeReadPermission(ctx, "read review fix findings", "atteler.review_fix", inputPath); err != nil {
		return fmt.Errorf("review fix: %w", err)
	}
	if err := authorizeReadPermission(ctx, "read review fix harness guidance", "atteler.review_fix", root); err != nil {
		return fmt.Errorf("review fix: %w", err)
	}

	rawInput, findings, err := reviewfix.LoadFindingsFile(ctx, inputPath)
	if err != nil {
		return fmt.Errorf("review fix: load findings: %w", err)
	}

	guidance, err := reviewfix.DiscoverGuidanceForFindings(ctx, root, findings)
	if err != nil {
		return fmt.Errorf("review fix: discover guidance: %w", err)
	}

	startedAt := time.Now().UTC()
	worktreeRequested := input.Worktree || state.worktreeInfo != nil
	plan := reviewfix.BuildPlan(inputPath, findings, guidance, input.ValidationCommands, worktreeRequested, startedAt)
	runID := reviewfix.NewRunID(startedAt)
	paths := reviewfix.ArtifactPathsFor(root, runID)
	if err := authorizeWritePermission(ctx, "write review fix artifacts", "atteler.review_fix", paths.RunDir); err != nil {
		return fmt.Errorf("review fix: %w", err)
	}
	if err := reviewfix.WriteInitialArtifacts(ctx, paths, rawInput, plan); err != nil {
		return fmt.Errorf("review fix: write initial artifacts: %w", err)
	}

	fmt.Fprintln(os.Stderr, "review fix: wrote plan "+paths.FixPlan)

	applyMode, applyErr := applyReviewFixPlan(ctx, state, root, plan)
	validation := runReviewFixValidation(ctx, state, root, input.ValidationCommands)

	patch, patchErr := reviewFixPatchDiff(ctx, root, paths.RunDir, inputPath)
	changedFiles, changesErr := reviewFixChangedFiles(ctx, root, paths.RunDir, inputPath)
	artifactApplyError := reviewFixArtifactError(applyErr, patchErr, changesErr)

	record := reviewfix.NewRunRecord(
		startedAt,
		time.Now().UTC(),
		paths,
		plan,
		changedFiles,
		validation,
		applyMode,
		artifactApplyError,
		patch,
	)
	if err := reviewfix.WriteFinalArtifacts(ctx, paths, record, patch); err != nil {
		return fmt.Errorf("review fix: write final artifacts: %w", err)
	}

	fmt.Print(formatReviewFixResult(record))

	return errors.Join(applyErr, reviewFixValidationError(validation))
}

func runReviewFixStateful(ctx context.Context, state appState, input reviewFixCommandInput) error {
	runErr := runReviewFix(ctx, state, input)
	if runErr != nil && state.worktreeInfo != nil && state.autoMergeWorktree {
		fmt.Fprintln(os.Stderr, "worktree: auto-merge skipped because review fix failed: "+runErr.Error())

		state.autoMergeWorktree = false
	}

	metadataErr := persistReviewFixWorktreeMetadata(ctx, &state)

	return errors.Join(runErr, metadataErr, finalizeWorktree(ctx, &state))
}

func persistReviewFixWorktreeMetadata(ctx context.Context, state *appState) error {
	if state == nil || state.worktreeInfo == nil || state.sessionStore == nil {
		return nil
	}

	state.sessionState.WorktreePath = state.worktreeInfo.Path
	state.sessionState.WorktreeBranch = state.worktreeInfo.Branch
	state.sessionState.WorktreeBase = state.worktreeInfo.BaseBranch
	if err := authorizeSessionStoreWrite(ctx, state.sessionStore, state.sessionState, "record review fix worktree metadata"); err != nil {
		return fmt.Errorf("review fix: %w", err)
	}
	if err := state.sessionStore.Save(state.sessionState); err != nil {
		return fmt.Errorf("review fix: save worktree metadata: %w", err)
	}

	return nil
}

func validateReviewFixRunInput(input reviewFixCommandInput) error {
	if strings.TrimSpace(input.PR) != "" {
		return errors.New("review fix: --pr input is not supported in the MVP; export review findings to JSON and use --from")
	}
	if strings.TrimSpace(input.From) == "" {
		return errors.New("review fix: --from is required for Atteler-native review finding input")
	}

	return nil
}

func reviewFixWorkspaceRoot(state appState) string {
	if state.worktreeInfo != nil && strings.TrimSpace(state.worktreeInfo.Path) != "" {
		return state.worktreeInfo.Path
	}
	if strings.TrimSpace(state.cwd) != "" {
		return state.cwd
	}

	cwd, err := os.Getwd()
	if err != nil {
		return "."
	}

	return cwd
}

func applyReviewFixPlan(ctx context.Context, state appState, root string, plan reviewfix.Plan) (string, error) {
	if len(plan.Findings) == 0 {
		return reviewFixApplyModeNone, nil
	}

	if diff, ok := reviewFixSuggestedDiffForPlan(plan.Findings); ok {
		return reviewFixApplyModeSuggestedDiff, applyReviewFixSuggestedDiff(ctx, state, root, diff)
	}

	return reviewFixApplyModeAgent, runReviewFixAgent(ctx, state, root, plan)
}

func reviewFixSuggestedDiffForPlan(findings []reviewfix.Finding) (string, bool) {
	diff, ok := reviewfix.SuggestedUnifiedDiff(findings)
	if !ok {
		return "", false
	}

	for i := range findings {
		if !reviewfix.LooksLikeUnifiedDiff(findings[i].SuggestedFix) {
			return "", false
		}
	}

	return diff, true
}

func applyReviewFixSuggestedDiff(ctx context.Context, state appState, root, diff string) error {
	file, err := os.CreateTemp("", "atteler-review-fix-*.diff")
	if err != nil {
		return fmt.Errorf("review fix suggested diff: create temp patch: %w", err)
	}
	patchPath := file.Name()
	defer func() { _ = os.Remove(patchPath) }()

	if _, err := file.WriteString(diff); err != nil {
		_ = file.Close()
		return fmt.Errorf("review fix suggested diff: write temp patch: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("review fix suggested diff: close temp patch: %w", err)
	}

	command := "git apply --whitespace=nowarn " + shellQuote(patchPath)
	result, runErr := attshell.RunBash(ctx, attshell.Options{
		Command:        command,
		Dir:            root,
		Timeout:        reviewFixDefaultTimeout,
		MaxOutputBytes: reviewFixMaxOutputBytes,
		Permission:     state.permissionPolicy,
		Audit: attshell.AuditContext{
			Caller:      "atteler.review_fix.apply_suggested_diff",
			SessionID:   state.sessionState.ID,
			SessionPath: reviewFixSessionPath(state),
			Autonomy:    state.autonomy.String(),
		},
	})
	if runErr != nil {
		return fmt.Errorf("review fix suggested diff: %w%s", runErr, reviewFixCommandOutputSuffix(result))
	}

	return nil
}

func runReviewFixAgent(ctx context.Context, state appState, root string, plan reviewfix.Plan) error {
	if state.registry == nil {
		return errors.New("review fix: LLM registry is required when findings do not contain unified diffs")
	}
	if state.agentRegistry == nil {
		return errors.New("review fix: agent registry is required when findings do not contain unified diffs")
	}
	if state.sessionStore == nil {
		return errors.New("review fix: session store is required when findings do not contain unified diffs")
	}

	fmt.Fprintln(os.Stderr, "review fix: running selected Atteler agent against normalized findings")

	localAutonomy := reviewFixLocalAutonomy(state.autonomy)

	return withReviewFixWorkingDirectory(root, func() error {
		return runOnceWithOptions(
			ctx,
			state.registry,
			state.agentRegistry,
			state.hookRunner,
			state.sessionStore,
			state.sessionState,
			state.contextOptions,
			state.referenceContext,
			state.referenceManifest,
			state.referenceContextEstimator,
			state.configuredReferences,
			state.selectedModel,
			state.selectedAgent,
			state.fallbackModels,
			state.generationDefaults,
			state.generationOverrides,
			state.maxInputTokens,
			runOnceExecutionOptions{
				OutputFormat:                outputFormatText,
				AgentLoopBudget:             state.agentLoopBudget,
				AgentLoopCheckpointInterval: state.agentLoopCheckpointInterval,
				SkillLearningStoreDir:       state.skillLearningStoreDir,
				SkillLearningSkillDir:       state.skillLearningSkillDir,
				SkillLearningEnabled:        state.skillLearningEnabled,
				VectorConfig:                state.vectorConfig,
				PermissionPolicy:            state.permissionPolicy,
				Autonomy:                    localAutonomy,
			},
			state.modelLocked,
			reviewfix.BuildAgentPrompt(plan),
		)
	})
}

func reviewFixLocalAutonomy(level autonomy.Level) autonomy.Level {
	normalized := autonomy.Normalize(level)
	switch normalized {
	case autonomy.High, autonomy.Full:
		return autonomy.Medium
	default:
		return normalized
	}
}

func withReviewFixWorkingDirectory(root string, fn func() error) (err error) {
	if strings.TrimSpace(root) == "" {
		return fn()
	}

	current, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("review fix: get working directory: %w", err)
	}
	if filepath.Clean(current) == filepath.Clean(root) {
		return fn()
	}
	if chdirErr := os.Chdir(root); chdirErr != nil {
		return fmt.Errorf("review fix: chdir %s: %w", root, chdirErr)
	}
	defer func() {
		if restoreErr := os.Chdir(current); restoreErr != nil && err == nil {
			err = fmt.Errorf("review fix: restore working directory %s: %w", current, restoreErr)
		}
	}()

	return fn()
}

func reviewFixSessionPath(state appState) string {
	if state.sessionStore == nil {
		return ""
	}

	return state.sessionStore.Path(state.sessionState.ID)
}

func runReviewFixValidation(ctx context.Context, state appState, root string, commands []string) []reviewfix.ValidationResult {
	commands = cleanCLIStrings(commands)
	if len(commands) == 0 {
		return nil
	}

	results := make([]reviewfix.ValidationResult, 0, len(commands))
	for _, command := range commands {
		fmt.Fprintln(os.Stderr, "review fix: validating: "+command)
		startedAt := time.Now().UTC()
		result, err := attshell.RunBash(ctx, attshell.Options{
			Command:        command,
			Dir:            root,
			Timeout:        reviewFixDefaultTimeout,
			MaxOutputBytes: reviewFixMaxOutputBytes,
			Policy:         &attshell.Policy{DenyNetwork: true},
			Permission:     state.permissionPolicy,
			Audit: attshell.AuditContext{
				Caller:      "atteler.review_fix.validate",
				SessionID:   state.sessionState.ID,
				SessionPath: reviewFixSessionPath(state),
				Autonomy:    state.autonomy.String(),
			},
			StartCallback: func() {
				emitHookWarning(ctx, state.hookRunner, events.Event{
					Type:        events.CommandExecute,
					SessionID:   state.sessionState.ID,
					SessionPath: reviewFixSessionPath(state),
					Agent:       state.selectedAgent,
					Model:       state.selectedModel,
					Content:     command,
					Metadata: map[string]string{
						"command":  command,
						"cwd":      root,
						"source":   "review_fix_validation",
						"autonomy": state.autonomy.String(),
					},
				})
			},
		})

		status := reviewFixValidationPassed
		errorText := ""
		if err != nil {
			status = reviewFixValidationFailed
			errorText = err.Error()
		}

		results = append(results, reviewfix.ValidationResult{
			StartedAt: startedAt,
			Duration:  result.Duration,
			Command:   command,
			Status:    status,
			Stdout:    result.Stdout,
			Stderr:    result.Stderr,
			Error:     errorText,
		})
	}

	return results
}

func reviewFixPatchDiff(ctx context.Context, root string, excludePaths ...string) (string, error) {
	var parts []string
	var errs []error

	trackedArgs := []string{"diff", "--no-ext-diff", "--binary"}
	if reviewFixGitHasHead(ctx, root) {
		trackedArgs = append(trackedArgs, "HEAD")
	}
	trackedArgs = append(trackedArgs, reviewFixDiffPathspecs(root, excludePaths...)...)
	tracked, trackedErr := reviewFixGitOutput(ctx, root, trackedArgs...)
	if trackedErr != nil {
		errs = append(errs, trackedErr)
	} else if strings.TrimSpace(tracked) != "" {
		parts = append(parts, tracked)
	}

	untrackedPaths, untrackedErr := reviewFixUntrackedPaths(ctx, root, excludePaths...)
	if untrackedErr != nil {
		errs = append(errs, untrackedErr)
	}
	for _, untrackedPath := range untrackedPaths {
		diff, diffErr := reviewFixNoIndexDiff(ctx, root, untrackedPath)
		if diffErr != nil {
			errs = append(errs, diffErr)
			continue
		}
		if strings.TrimSpace(diff) != "" {
			parts = append(parts, diff)
		}
	}

	return strings.Join(parts, "\n"), errors.Join(errs...)
}

func reviewFixDiffPathspecs(root string, excludePaths ...string) []string {
	if len(excludePaths) == 0 {
		return nil
	}

	pathspecs := []string{"--", "."}
	for _, excludePath := range excludePaths {
		excludeRel := reviewFixRelativePath(root, excludePath)
		if excludeRel == "" {
			continue
		}

		pathspecs = append(pathspecs, ":(exclude)"+excludeRel)
	}

	return pathspecs
}

func reviewFixGitHasHead(ctx context.Context, root string) bool {
	cmd := exec.CommandContext(ctx, "git", "-C", root, "rev-parse", "--verify", "--quiet", "HEAD")
	return cmd.Run() == nil
}

func reviewFixGitOutput(ctx context.Context, root string, args ...string) (string, error) {
	cmdArgs := append([]string{"-C", root}, args...)
	cmd := exec.CommandContext(ctx, "git", cmdArgs...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("review fix git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}

	return string(out), nil
}

func reviewFixUntrackedPaths(ctx context.Context, root string, excludePaths ...string) ([]string, error) {
	out, err := reviewFixGitOutput(ctx, root, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, err
	}

	var paths []string
	for pathValue := range strings.SplitSeq(out, "\x00") {
		pathValue = strings.TrimSpace(pathValue)
		if pathValue == "" || reviewFixPathExcluded(root, pathValue, excludePaths...) {
			continue
		}

		paths = append(paths, pathValue)
	}

	return paths, nil
}

func reviewFixNoIndexDiff(ctx context.Context, root, pathValue string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", root, "diff", "--no-ext-diff", "--binary", "--no-index", "--", "/dev/null", pathValue)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), nil
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 && len(out) > 0 {
		return string(out), nil
	}

	return "", fmt.Errorf("review fix git diff --no-index %s: %w: %s", pathValue, err, strings.TrimSpace(string(out)))
}

func reviewFixChangedFiles(ctx context.Context, root string, excludePaths ...string) ([]reviewfix.ChangedFile, error) {
	out, err := reviewFixGitOutput(ctx, root, "status", "--short", "--untracked-files=all")
	if err != nil {
		return nil, err
	}

	var files []reviewfix.ChangedFile
	for line := range strings.Lines(out) {
		line = strings.TrimRight(line, "\n")
		if strings.TrimSpace(line) == "" {
			continue
		}

		status := strings.TrimSpace(line[:min(len(line), 2)])
		pathValue := ""
		if len(line) > 3 {
			pathValue = strings.TrimSpace(line[3:])
		}
		if pathValue == "" {
			pathValue = strings.TrimSpace(line)
		}
		if reviewFixPathExcluded(root, pathValue, excludePaths...) {
			continue
		}

		files = append(files, reviewfix.ChangedFile{Status: status, Path: pathValue})
	}

	return files, nil
}

func reviewFixPathExcluded(root, pathValue string, excludePaths ...string) bool {
	rel := reviewFixRelativePath(root, pathValue)
	if rel == "" {
		return false
	}

	for _, excludePath := range excludePaths {
		excludeRel := reviewFixRelativePath(root, excludePath)
		if excludeRel == "" {
			continue
		}
		if rel == excludeRel || strings.HasPrefix(rel, strings.TrimSuffix(excludeRel, "/")+"/") {
			return true
		}
	}

	return false
}

func reviewFixRelativePath(root, pathValue string) string {
	pathValue = strings.TrimSpace(pathValue)
	if pathValue == "" {
		return ""
	}

	if filepath.IsAbs(pathValue) {
		rel, err := filepath.Rel(root, pathValue)
		if err == nil {
			pathValue = rel
		}
	}

	cleaned := filepath.Clean(pathValue)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return ""
	}

	return strings.TrimPrefix(filepath.ToSlash(cleaned), "./")
}

func reviewFixArtifactError(errs ...error) string {
	parts := make([]string, 0, len(errs))
	for _, err := range errs {
		if err != nil {
			parts = append(parts, err.Error())
		}
	}

	return strings.Join(parts, "; ")
}

func reviewFixValidationError(results []reviewfix.ValidationResult) error {
	failed := make([]string, 0)
	for i := range results {
		result := results[i]
		if result.Status == reviewFixValidationFailed {
			failed = append(failed, result.Command)
		}
	}
	if len(failed) == 0 {
		return nil
	}

	return fmt.Errorf("review fix validation failed: %s", strings.Join(failed, ", "))
}

func reviewFixCommandOutputSuffix(result attshell.Result) string {
	var b strings.Builder
	if strings.TrimSpace(result.Stdout) != "" {
		b.WriteString("\nstdout:\n")
		b.WriteString(result.Stdout)
	}
	if strings.TrimSpace(result.Stderr) != "" {
		b.WriteString("\nstderr:\n")
		b.WriteString(result.Stderr)
	}

	return b.String()
}

func formatReviewFixResult(record reviewfix.RunRecord) string {
	validationStatus := reviewFixValidationNotRun
	if len(record.Validation) > 0 {
		validationStatus = reviewFixValidationPassed
		for i := range record.Validation {
			result := record.Validation[i]
			if result.Status == reviewFixValidationFailed {
				validationStatus = reviewFixValidationFailed
				break
			}
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "review_fix_run: %s\n", record.Artifacts.RunDir)
	fmt.Fprintf(&b, "findings: %d\n", record.FindingCount)
	fmt.Fprintf(&b, "groups: %d\n", record.GroupCount)
	fmt.Fprintf(&b, "changed_files: %d\n", len(record.ChangedFiles))
	fmt.Fprintf(&b, "validation: %s\n", validationStatus)
	fmt.Fprintf(&b, "patch: %s\n", record.Artifacts.PatchDiff)
	fmt.Fprintf(&b, "report: %s\n", record.Artifacts.Changes)
	fmt.Fprintf(&b, "run_json: %s\n", record.Artifacts.RunJSON)
	b.WriteString("remote_publishing: false\n")
	if record.ApplyError != "" {
		fmt.Fprintf(&b, "apply_error: %s\n", record.ApplyError)
	}

	return b.String()
}
