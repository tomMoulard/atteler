package skill

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var validSlug = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// Review describes the generated files for an accepted skill before they are
// written. Callers should show Diff to the user as the review/approval artifact
// before calling PersistReview.
type Review struct {
	Root           string
	SkillPath      string
	Diff           string
	Files          []ReviewFile
	TriggerResults []TriggerEvalResult
}

// ReviewFile is one generated file in a synthesized skill directory.
type ReviewFile struct {
	RelativePath string
	Content      string
	Mode         os.FileMode
}

// BuildReview renders the skill directory that would be written for suggestion
// and returns a diff suitable for review before approval.
func BuildReview(dir string, suggestion Suggestion) (Review, error) {
	if err := validatePersistInput(dir, suggestion); err != nil {
		return Review{}, err
	}

	triggerResults, err := ValidateTriggerEvals(suggestion)
	if err != nil {
		return Review{}, err
	}

	root := filepath.Join(strings.TrimSpace(dir), suggestion.Slug)
	files := []ReviewFile{
		{
			RelativePath: "SKILL.md",
			Content:      formatPersistedSuggestion(suggestion),
			Mode:         0o600,
		},
		{
			RelativePath: filepath.Join("evals", "triggers.yaml"),
			Content:      formatTriggerEvals(suggestion),
			Mode:         0o600,
		},
	}

	review := Review{
		Root:           root,
		SkillPath:      filepath.Join(root, "SKILL.md"),
		Files:          files,
		TriggerResults: triggerResults,
	}
	review.Diff = formatReviewDiff(review)

	return review, nil
}

// PersistSuggestion writes an accepted skill suggestion as a valid skill
// directory under dir. Existing directories/files are left untouched so
// acceptance remains a safe, explicit action.
func PersistSuggestion(dir string, suggestion Suggestion) (string, error) {
	review, err := BuildReview(dir, suggestion)
	if err != nil {
		return "", err
	}

	return PersistReview(review)
}

// PersistReview writes a previously reviewed generated skill to disk. Persisting
// the reviewed artifact ensures the saved files match the diff the caller
// presented for approval.
func PersistReview(review Review) (string, error) {
	if err := validateReview(review); err != nil {
		return "", err
	}

	dir := filepath.Dir(review.Root)
	if mkdirErr := os.MkdirAll(dir, 0o750); mkdirErr != nil {
		return "", fmt.Errorf("skill: create save directory: %w", mkdirErr)
	}

	if _, statErr := os.Stat(review.Root); statErr == nil {
		return "", fmt.Errorf("skill: create skill directory %s: %w", review.Root, os.ErrExist)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return "", fmt.Errorf("skill: inspect skill directory %s: %w", review.Root, statErr)
	}

	tmpRoot, err := os.MkdirTemp(dir, "."+filepath.Base(review.Root)+"-*.tmp")
	if err != nil {
		return "", fmt.Errorf("skill: create temp skill directory: %w", err)
	}
	defer os.RemoveAll(tmpRoot)

	for _, file := range review.Files {
		path := filepath.Join(tmpRoot, file.RelativePath)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return "", fmt.Errorf("skill: create skill subdirectory %s: %w", filepath.Dir(path), err)
		}

		handle, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, file.Mode)
		if err != nil {
			return "", fmt.Errorf("skill: save generated file %s: %w", path, err)
		}

		if _, err := handle.WriteString(file.Content); err != nil {
			_ = handle.Close()
			return "", fmt.Errorf("skill: write generated file %s: %w", path, err)
		}

		if err := handle.Close(); err != nil {
			return "", fmt.Errorf("skill: close generated file %s: %w", path, err)
		}
	}

	if err := os.Rename(tmpRoot, review.Root); err != nil {
		return "", fmt.Errorf("skill: publish skill directory %s: %w", review.Root, err)
	}

	return review.SkillPath, nil
}

func validateReview(review Review) error {
	if strings.TrimSpace(review.Root) == "" {
		return errors.New("skill: review root is required")
	}

	if strings.TrimSpace(review.SkillPath) == "" {
		return errors.New("skill: review skill path is required")
	}

	if !pathWithin(review.Root, review.SkillPath) {
		return fmt.Errorf("skill: review skill path %q must be under %q", review.SkillPath, review.Root)
	}

	if len(review.Files) == 0 {
		return errors.New("skill: review files are required")
	}

	hasSkillFile := false

	for _, file := range review.Files {
		if err := validateReviewFile(file); err != nil {
			return err
		}

		if filepath.ToSlash(filepath.Clean(file.RelativePath)) == "SKILL.md" {
			hasSkillFile = true
		}
	}

	if !hasSkillFile {
		return errors.New("skill: review must include SKILL.md")
	}

	return nil
}

func validateReviewFile(file ReviewFile) error {
	relativePath := filepath.Clean(file.RelativePath)
	if relativePath == "." ||
		filepath.IsAbs(relativePath) ||
		relativePath == ".." ||
		strings.HasPrefix(relativePath, ".."+string(filepath.Separator)) {
		return fmt.Errorf("skill: invalid review file path %q", file.RelativePath)
	}

	if file.Content == "" {
		return fmt.Errorf("skill: review file %s content is required", file.RelativePath)
	}

	return nil
}

func pathWithin(root, path string) bool {
	root = filepath.Clean(root)
	path = filepath.Clean(path)

	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}

	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func validatePersistInput(dir string, suggestion Suggestion) error {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return errors.New("skill: save directory is required")
	}

	slug := strings.TrimSpace(suggestion.Slug)
	if slug == "" {
		return errors.New("skill: suggestion slug is required")
	}

	if filepath.Base(slug) != slug ||
		strings.Contains(slug, string(filepath.Separator)) ||
		!validSlug.MatchString(slug) {
		return fmt.Errorf("skill: invalid suggestion slug %q", slug)
	}

	if len(suggestion.Steps) == 0 {
		return errors.New("skill: suggestion steps are required")
	}

	return nil
}

func formatPersistedSuggestion(suggestion Suggestion) string {
	workflow := suggestion.Workflow
	if len(workflow) == 0 {
		workflow = workflowFromSteps(suggestion.Steps)
	}

	triggerEvals := suggestion.TriggerEvals
	if len(triggerEvals) == 0 {
		triggerEvals = BuildTriggerEvals(suggestion)
	}

	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "name: %s\n", yamlString(suggestion.Slug))
	fmt.Fprintf(&b, "description: %s\n", yamlString(skillDescription(suggestion)))
	b.WriteString("---\n\n")

	fmt.Fprintf(&b, "# %s\n\n", suggestion.Name)
	b.WriteString("Use this skill only when the task calls for the complete repeated workflow below.\n")
	b.WriteString("Do not trigger it for one isolated step or unrelated requests.\n\n")

	writeParameters(&b, suggestion.Parameters)
	writeWorkflow(&b, workflow)
	writeExamples(&b, triggerEvals)
	writeToolBoundaries(&b, workflow)
	writeFailureModes(&b)
	writeVerification(&b, workflow)
	writeTriggerGuidance(&b, triggerEvals)
	writeProvenance(&b, suggestion, workflow)

	return b.String()
}

func skillDescription(suggestion Suggestion) string {
	return fmt.Sprintf(
		"Use when the user asks for the full %s workflow: %s. Do not use for unrelated tasks or a single isolated step.",
		suggestion.Slug,
		humanWorkflow(suggestion.Steps),
	)
}

func writeParameters(b *strings.Builder, parameters []Parameter) {
	b.WriteString("## Parameters\n\n")

	if len(parameters) == 0 {
		b.WriteString("- No variable values were detected in the repeated action labels; use the current task context as input.\n\n")
		return
	}

	for _, parameter := range parameters {
		fmt.Fprintf(b, "- `%s` (`%s`): %s", parameter.Name, parameter.Placeholder, parameter.Description)

		if len(parameter.Examples) > 0 {
			fmt.Fprintf(b, " Observed examples: %s.", strings.Join(parameter.Examples, ", "))
		}

		b.WriteByte('\n')
	}

	b.WriteByte('\n')
}

func writeWorkflow(b *strings.Builder, workflow []WorkflowStep) {
	b.WriteString("## Workflow\n\n")

	for i := range workflow {
		step := &workflow[i]
		fmt.Fprintf(b, "%d. **%s**\n", i+1, humanStep(step.Action))
		writeNestedList(b, "Prompts", step.Prompts, "Use the current user request and previous step output.")
		writeNestedList(b, "Inputs", step.Inputs, "Current task context plus any parameters above.")
		writeNestedList(b, "Outputs", step.Outputs, "A completed step result, explicit blocker, or handoff note.")
		writeNestedList(b, "Tool classes", step.ToolClasses, "agent-guidance")
		writeNestedList(b, "Verify", step.VerificationCommands, "Use the verification section after completing the workflow.")
		writeNestedList(b, "Stop when", step.StopConditions, "Required inputs are missing, a tool is unavailable, or verification fails.")
		b.WriteByte('\n')
	}
}

func writeExamples(b *strings.Builder, evals []TriggerEval) {
	b.WriteString("## Examples\n\n")
	b.WriteString("Good fits:\n")

	wroteTrigger := false

	for _, eval := range evals {
		if !eval.ShouldTrigger {
			continue
		}

		wroteTrigger = true

		fmt.Fprintf(b, "- %s\n", eval.Prompt)
	}

	if !wroteTrigger {
		b.WriteString("- Any request that needs the full workflow above.\n")
	}

	b.WriteString("\nDo not use for:\n")

	wroteReject := false

	for _, eval := range evals {
		if eval.ShouldTrigger {
			continue
		}

		wroteReject = true

		fmt.Fprintf(b, "- %s\n", eval.Prompt)
	}

	if !wroteReject {
		b.WriteString("- Unrelated requests or isolated single-step tasks.\n")
	}

	b.WriteByte('\n')
}

func writeToolBoundaries(b *strings.Builder, workflow []WorkflowStep) {
	b.WriteString("## Tool boundaries\n\n")

	classes := uniqueToolClasses(workflow)
	if len(classes) == 0 {
		b.WriteString("- Use only the tools already appropriate for the current task context.\n")
	} else {
		for _, class := range classes {
			fmt.Fprintf(b, "- `%s`: use only when the corresponding workflow step requires it.\n", class)
		}
	}

	b.WriteString("- Do not introduce new dependencies or external side effects unless the user explicitly requests them.\n\n")
}

func writeFailureModes(b *strings.Builder) {
	b.WriteString("## Failure modes and recovery\n\n")
	b.WriteString("- If a parameter value is missing, ask one concise clarifying question before executing the workflow.\n")
	b.WriteString("- If a tool boundary blocks progress, stop and report the unavailable tool plus the next safe alternative.\n")
	b.WriteString("- If verification fails, iterate on the failing step once with the failure output, then report remaining blockers.\n\n")
}

func writeVerification(b *strings.Builder, workflow []WorkflowStep) {
	b.WriteString("## Verification\n\n")

	commands := uniqueVerificationCommands(workflow)
	if len(commands) == 0 {
		b.WriteString("- Run the smallest relevant check for the changed artifact before claiming completion.\n")
		b.WriteString("- Confirm the workflow's stop conditions are satisfied or explicitly reported.\n\n")

		return
	}

	for _, command := range commands {
		fmt.Fprintf(b, "- `%s`\n", command)
	}

	b.WriteByte('\n')
}

func writeTriggerGuidance(b *strings.Builder, evals []TriggerEval) {
	b.WriteString("## Trigger guidance\n\n")
	b.WriteString("Trigger only for prompts that require the full workflow shape. Use `evals/triggers.yaml` as the regression fixture.\n\n")

	for _, eval := range evals {
		label := "reject"
		if eval.ShouldTrigger {
			label = "trigger"
		}

		fmt.Fprintf(b, "- `%s`: %s — %s\n", label, eval.Prompt, eval.Reason)
	}

	b.WriteByte('\n')
}

func writeProvenance(b *strings.Builder, suggestion Suggestion, workflow []WorkflowStep) {
	b.WriteString("## Provenance\n\n")
	fmt.Fprintf(b, "- Slug: `%s`\n", suggestion.Slug)
	fmt.Fprintf(b, "- Occurrences: %d\n", suggestion.Occurrences)

	if strings.TrimSpace(suggestion.Rationale) != "" {
		fmt.Fprintf(b, "- Rationale: %s\n", suggestion.Rationale)
	}

	for i := range workflow {
		step := &workflow[i]
		if len(step.SourceActions) == 0 {
			continue
		}

		fmt.Fprintf(b, "- Step %d source actions: %s\n", i+1, strings.Join(step.SourceActions, "; "))
	}
}

func workflowFromSteps(steps []string) []WorkflowStep {
	workflow := make([]WorkflowStep, 0, len(steps))
	for _, step := range steps {
		workflow = append(workflow, WorkflowStep{
			Action:      step,
			ToolClasses: []string{inferToolClass(step)},
		})
	}

	return workflow
}

func writeNestedList(b *strings.Builder, label string, values []string, fallback string) {
	if len(values) == 0 {
		fmt.Fprintf(b, "   - %s: %s\n", label, fallback)
		return
	}

	fmt.Fprintf(b, "   - %s:\n", label)

	for _, value := range values {
		fmt.Fprintf(b, "     - %s\n", value)
	}
}

func uniqueToolClasses(workflow []WorkflowStep) []string {
	var classes []string

	for i := range workflow {
		step := &workflow[i]
		classes = appendUniqueAll(classes, step.ToolClasses)
	}

	return classes
}

func uniqueVerificationCommands(workflow []WorkflowStep) []string {
	var commands []string

	for i := range workflow {
		step := &workflow[i]
		commands = appendUniqueAll(commands, step.VerificationCommands)
	}

	return commands
}

func formatTriggerEvals(suggestion Suggestion) string {
	evals := suggestion.TriggerEvals

	if len(evals) == 0 {
		evals = BuildTriggerEvals(suggestion)
	}

	var b strings.Builder
	b.WriteString("version: 1\n")
	fmt.Fprintf(&b, "skill: %s\n", yamlString(suggestion.Slug))
	b.WriteString("cases:\n")

	for _, eval := range evals {
		fmt.Fprintf(&b, "  - prompt: %s\n", yamlString(eval.Prompt))
		fmt.Fprintf(&b, "    should_trigger: %t\n", eval.ShouldTrigger)
		fmt.Fprintf(&b, "    reason: %s\n", yamlString(eval.Reason))
	}

	return b.String()
}

func formatReviewDiff(review Review) string {
	var b strings.Builder

	for _, file := range review.Files {
		rel := filepath.ToSlash(filepath.Join(filepath.Base(review.Root), file.RelativePath))
		fmt.Fprintf(&b, "diff --git a/%s b/%s\n", rel, rel)
		b.WriteString("new file mode 100644\n")
		b.WriteString("--- /dev/null\n")
		fmt.Fprintf(&b, "+++ b/%s\n", rel)
		b.WriteString("@@\n")

		for line := range strings.SplitSeq(strings.TrimSuffix(file.Content, "\n"), "\n") {
			fmt.Fprintf(&b, "+%s\n", line)
		}
	}

	return b.String()
}

func yamlString(value string) string {
	return strconv.Quote(value)
}
