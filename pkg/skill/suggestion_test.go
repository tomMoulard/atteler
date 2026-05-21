package skill

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

type triggerFixture struct {
	Skill string               `yaml:"skill"`
	Cases []triggerFixtureCase `yaml:"cases"`
}

type triggerFixtureCase struct {
	Prompt        string `yaml:"prompt"`
	Reason        string `yaml:"reason"`
	ShouldTrigger bool   `yaml:"should_trigger"`
}

func TestSuggest_RepeatedSequence(t *testing.T) {
	t.Parallel()

	got, ok := Suggest([]string{
		"Open Issue", "Edit Fix", "Run Tests",
		"Open Issue", "Edit Fix", "Run Tests",
	})
	if !ok {
		t.Fatal("Suggest returned ok=false, want a suggestion")
	}

	wantSteps := []string{"open issue", "edit fix", "run tests"}
	if !reflect.DeepEqual(got.Steps, wantSteps) {
		t.Fatalf("Steps = %#v, want %#v", got.Steps, wantSteps)
	}

	if got.Slug != "open-issue-edit-fix-run-tests" {
		t.Fatalf("Slug = %q, want open-issue-edit-fix-run-tests", got.Slug)
	}

	if got.Name != "Open Issue Edit Fix Run Tests Skill" {
		t.Fatalf("Name = %q", got.Name)
	}

	if got.Occurrences != 2 {
		t.Fatalf("Occurrences = %d, want 2", got.Occurrences)
	}

	if !strings.Contains(got.Rationale, "3-step sequence") || !strings.Contains(got.Rationale, "repeat 2 times") {
		t.Fatalf("Rationale = %q, want sequence length and occurrences", got.Rationale)
	}
}

func TestSuggestWithOptions_SelectsBestFocusedWorkflow(t *testing.T) {
	t.Parallel()

	got, ok := SuggestWithOptions([]string{
		"plan", "code", "test",
		"plan", "code", "test",
		"plan", "code", "test",
	}, Options{MaxSteps: 4})
	if !ok {
		t.Fatal("SuggestWithOptions returned ok=false, want a suggestion")
	}

	wantSteps := []string{"plan", "code", "test"}
	if !reflect.DeepEqual(got.Steps, wantSteps) {
		t.Fatalf("Steps = %#v, want %#v", got.Steps, wantSteps)
	}

	if got.Occurrences != 3 {
		t.Fatalf("Occurrences = %d, want 3", got.Occurrences)
	}
}

func TestSuggest_IgnoresEmptyActionsAndNormalizesWhitespace(t *testing.T) {
	t.Parallel()

	got, ok := Suggest([]string{
		"  Review   Logs ", "", "Patch-Code",
		"review logs", "   ", "patch-code",
	})
	if !ok {
		t.Fatal("Suggest returned ok=false, want a suggestion")
	}

	wantSteps := []string{"review logs", "patch-code"}
	if !reflect.DeepEqual(got.Steps, wantSteps) {
		t.Fatalf("Steps = %#v, want %#v", got.Steps, wantSteps)
	}

	if got.Slug != "review-logs-patch-code" {
		t.Fatalf("Slug = %q, want review-logs-patch-code", got.Slug)
	}
}

func TestParseObservationSpec_CapturesInlineProvenance(t *testing.T) {
	t.Parallel()

	got := ParseObservationSpec("Open GH-15 | prompt=Fix the issue | tool=github | input=GH-15 | output=issue context | verify=go test ./pkg/skill | stop=issue is inaccessible")

	require.Equal(t, "Open GH-15", got.Action)
	require.Equal(t, "Fix the issue", got.Prompt)
	require.Equal(t, "github", got.ToolClass)
	require.Equal(t, []string{"GH-15"}, got.Inputs)
	require.Equal(t, []string{"issue context"}, got.Outputs)
	require.Equal(t, []string{"go test ./pkg/skill"}, got.VerificationCommands)
	require.Equal(t, []string{"issue is inaccessible"}, got.StopConditions)
}

func TestParseObservationSpec_KeepsUnknownPipesAsLiteralAction(t *testing.T) {
	t.Parallel()

	got := ParseObservationSpec("plan | code")

	require.Equal(t, Observation{Action: "plan | code"}, got)
}

func TestSuggestWithOptions_ParsesInlineProvenance(t *testing.T) {
	t.Parallel()

	got, ok := SuggestWithOptions([]string{
		"Open GH-15 | prompt=Fix GH-15 | tool=github | input=GH-15 | output=issue context",
		"Run go test ./pkg/skill | tool=shell | verify=go test ./pkg/skill",
		"Open GH-16 | prompt=Fix GH-16 | tool=github | input=GH-16 | output=issue context",
		"Run go test ./pkg/skill | tool=shell | verify=go test ./pkg/skill",
	}, Options{})
	require.True(t, ok)

	require.Equal(t, []string{"open {{issue}}", "run go test {{path}}"}, got.Steps)
	require.Equal(t, []string{"github"}, got.Workflow[0].ToolClasses)
	require.Contains(t, got.Workflow[0].Prompts, "Fix GH-15")
	require.Contains(t, got.Workflow[0].Inputs, "GH-15")
	require.Contains(t, got.Workflow[1].VerificationCommands, "go test ./pkg/skill")
}

func TestSuggestFromObservations_ParameterizesValuesAndPreservesProvenance(t *testing.T) {
	t.Parallel()

	got, ok := SuggestFromObservations([]Observation{
		{
			Action:               "Open GH-12",
			Prompt:               "Fix GH-12",
			ToolClass:            "github",
			Inputs:               []string{"issue GH-12"},
			Outputs:              []string{"issue context"},
			VerificationCommands: []string{"go test ./pkg/auth"},
			StopConditions:       []string{"issue is inaccessible"},
		},
		{Action: "Edit pkg/auth/auth.go", ToolClass: "file-edit", Inputs: []string{"pkg/auth/auth.go"}},
		{Action: "Run go test ./pkg/auth", ToolClass: "shell", VerificationCommands: []string{"go test ./pkg/auth"}},
		{Action: "Open GH-13", Prompt: "Fix GH-13", ToolClass: "github", Inputs: []string{"issue GH-13"}, Outputs: []string{"issue context"}},
		{Action: "Edit pkg/llm/llm.go", ToolClass: "file-edit", Inputs: []string{"pkg/llm/llm.go"}},
		{Action: "Run go test ./pkg/llm", ToolClass: "shell", VerificationCommands: []string{"go test ./pkg/llm"}},
	}, Options{})
	require.True(t, ok)

	require.Equal(t, []string{"open {{issue}}", "edit {{path}}", "run go test {{path}}"}, got.Steps)
	require.Equal(t, "open-issue-edit-path-run-go-test-path", got.Slug)
	require.Len(t, got.Parameters, 2)
	require.Equal(t, "issue", got.Parameters[0].Name)
	require.Equal(t, []string{"gh-12", "gh-13"}, got.Parameters[0].Examples)
	require.Equal(t, "path", got.Parameters[1].Name)
	require.Contains(t, got.Parameters[1].Examples, "pkg/auth/auth.go")
	require.Contains(t, got.Parameters[1].Examples, "./pkg/auth")

	require.Len(t, got.Workflow, 3)
	require.Equal(t, []string{"github"}, got.Workflow[0].ToolClasses)
	require.Contains(t, got.Workflow[0].Prompts, "Fix GH-12")
	require.Contains(t, got.Workflow[0].StopConditions, "issue is inaccessible")
	require.Equal(t, []string{"file-edit"}, got.Workflow[1].ToolClasses)
	require.Equal(t, []string{"shell"}, got.Workflow[2].ToolClasses)
	require.Contains(t, got.Workflow[2].VerificationCommands, "go test ./pkg/auth")

	require.True(t, PromptTriggers(got, "Run the open issue edit path run go test path workflow"))
	require.True(t, PromptTriggers(got, "Open GH-12, edit pkg/auth/auth.go, then run go test ./pkg/auth."))
	require.False(t, PromptTriggers(got, "Open GH-12, edit pkg/auth/auth.go, then run a command."))
	require.False(t, PromptTriggers(got, "Only open GH-12"))
	require.False(t, PromptTriggers(got, "Summarize the project README"))
}

func TestSuggest_NoRepeatedMultiStepSequence(t *testing.T) {
	t.Parallel()

	if got, ok := Suggest([]string{"plan", "code", "test"}); ok {
		t.Fatalf("Suggest returned %#v, true; want no suggestion", got)
	}
}

func TestPromptTriggers_UsesTokenBoundaries(t *testing.T) {
	t.Parallel()

	suggestion, ok := Suggest([]string{"plan", "code", "plan", "code"})
	require.True(t, ok)

	require.True(t, PromptTriggers(suggestion, "please plan and code the fix"))
	require.False(t, PromptTriggers(suggestion, "planarian codependent trivia"))
}

func TestValidateTriggerEvals_RejectsInconsistentFixture(t *testing.T) {
	t.Parallel()

	suggestion, ok := Suggest([]string{"plan", "code", "plan", "code"})
	require.True(t, ok)

	suggestion.TriggerEvals = []TriggerEval{
		{Prompt: "Only plan.", ShouldTrigger: true, Reason: "bad positive"},
		{Prompt: "Summarize the project README.", ShouldTrigger: false, Reason: "unrelated"},
	}

	results, err := ValidateTriggerEvals(suggestion)
	require.Error(t, err)
	require.ErrorContains(t, err, "expected should_trigger=true, got false")
	require.Len(t, results, 1)
	require.Equal(t, "Only plan.", results[0].Prompt)
	require.True(t, results[0].Expected)
	require.False(t, results[0].Actual)

	_, err = BuildReview(t.TempDir(), suggestion)
	require.ErrorContains(t, err, "expected should_trigger=true, got false")
}

func TestValidateTriggerEvals_RequiresPositiveAndNegativeCases(t *testing.T) {
	t.Parallel()

	suggestion, ok := Suggest([]string{"plan", "code", "plan", "code"})
	require.True(t, ok)

	suggestion.TriggerEvals = []TriggerEval{
		{Prompt: "Use the Plan Code skill for this task.", ShouldTrigger: true, Reason: "explicit"},
	}

	_, err := ValidateTriggerEvals(suggestion)
	require.ErrorContains(t, err, "negative trigger eval")
}

func TestBuildReview_RendersSkillDirectoryDiff(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	suggestion, ok := Suggest([]string{"plan", "code", "plan", "code"})
	require.True(t, ok)

	review, err := BuildReview(dir, suggestion)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(dir, "plan-code", "SKILL.md"), review.SkillPath)
	require.Len(t, review.Files, 2)
	require.Len(t, review.TriggerResults, 4)
	require.Contains(t, review.Diff, "diff --git a/plan-code/SKILL.md b/plan-code/SKILL.md")
	require.Contains(t, review.Diff, "+---")
	require.Contains(t, review.Diff, "+name: \"plan-code\"")
	require.Contains(t, review.Diff, "+## Workflow")
	require.Contains(t, review.Diff, "diff --git a/plan-code/evals/triggers.yaml b/plan-code/evals/triggers.yaml")
	require.Contains(t, review.Diff, "+    should_trigger: true")
	require.Contains(t, review.Diff, "+    should_trigger: false")
}

func TestPersistSuggestion_WritesSkillDirectoryWithoutOverwrite(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	suggestion, ok := Suggest([]string{"plan", "code", "plan", "code"})
	require.True(t, ok)

	path, err := PersistSuggestion(dir, suggestion)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(dir, "plan-code", "SKILL.md"), path)

	data, err := os.ReadFile(path)
	require.NoError(t, err)

	content := string(data)
	for _, want := range []string{
		"---\nname: \"plan-code\"",
		"description:",
		"# Plan Code Skill",
		"## Parameters",
		"## Workflow",
		"1. **plan**",
		"2. **code**",
		"## Examples",
		"Good fits:",
		"Do not use for:",
		"## Tool boundaries",
		"## Verification",
		"## Trigger guidance",
		"## Provenance",
		"- Slug: `plan-code`",
	} {
		require.Contains(t, content, want)
	}

	frontMatter := strings.SplitN(content, "---", 3)
	require.Len(t, frontMatter, 3)

	var metadata map[string]string
	require.NoError(t, yaml.Unmarshal([]byte(frontMatter[1]), &metadata))
	require.Equal(t, "plan-code", metadata["name"])
	require.Contains(t, metadata["description"], "full plan-code workflow")

	evals, err := os.ReadFile(filepath.Join(dir, "plan-code", "evals", "triggers.yaml"))
	require.NoError(t, err)

	for _, want := range []string{
		`skill: "plan-code"`,
		`prompt: "Run the plan-code workflow: plan → code."`,
		"should_trigger: true",
		"should_trigger: false",
	} {
		require.Contains(t, string(evals), want)
	}

	var fixture triggerFixture
	require.NoError(t, yaml.Unmarshal(evals, &fixture))
	require.Equal(t, "plan-code", fixture.Skill)
	require.NotEmpty(t, fixture.Cases)

	for _, tc := range fixture.Cases {
		require.Equal(t, tc.ShouldTrigger, PromptTriggers(suggestion, tc.Prompt), tc.Prompt)
	}

	_, err = PersistSuggestion(dir, suggestion)
	require.Error(t, err)
}

func TestPersistReview_WritesReviewedFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	suggestion, ok := Suggest([]string{"plan", "code", "plan", "code"})
	require.True(t, ok)

	review, err := BuildReview(dir, suggestion)
	require.NoError(t, err)

	path, err := PersistReview(review)
	require.NoError(t, err)
	require.Equal(t, review.SkillPath, path)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, review.Files[0].Content, string(data))
}

func TestPersistReview_RejectsUnsafeRelativePaths(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "bad")
	review := Review{
		Root:      dir,
		SkillPath: filepath.Join(dir, "SKILL.md"),
		Files: []ReviewFile{
			{RelativePath: "../SKILL.md", Content: "bad", Mode: 0o600},
		},
	}

	_, err := PersistReview(review)
	require.ErrorContains(t, err, "invalid review file path")
}

func TestPersistReview_RequiresSkillFileAndPathUnderRoot(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "bad")
	review := Review{
		Root:      root,
		SkillPath: filepath.Join(filepath.Dir(root), "SKILL.md"),
		Files: []ReviewFile{
			{RelativePath: "notes.md", Content: "notes", Mode: 0o600},
		},
	}

	_, err := PersistReview(review)
	require.ErrorContains(t, err, "must be under")

	review.SkillPath = filepath.Join(root, "SKILL.md")
	_, err = PersistReview(review)
	require.ErrorContains(t, err, "must include SKILL.md")
}

func TestPersistSuggestion_WritesParameterizedProvenance(t *testing.T) {
	t.Parallel()

	suggestion, ok := SuggestFromObservations([]Observation{
		{
			Action:               "Open GH-12",
			Prompt:               "Fix GH-12",
			ToolClass:            "github",
			Inputs:               []string{"issue GH-12"},
			Outputs:              []string{"issue context"},
			VerificationCommands: []string{"go test ./pkg/auth"},
			StopConditions:       []string{"issue is inaccessible"},
		},
		{Action: "Run go test ./pkg/auth", ToolClass: "shell", VerificationCommands: []string{"go test ./pkg/auth"}},
		{
			Action:               "Open GH-13",
			Prompt:               "Fix GH-13",
			ToolClass:            "github",
			Inputs:               []string{"issue GH-13"},
			Outputs:              []string{"issue context"},
			VerificationCommands: []string{"go test ./pkg/llm"},
			StopConditions:       []string{"issue is inaccessible"},
		},
		{Action: "Run go test ./pkg/llm", ToolClass: "shell", VerificationCommands: []string{"go test ./pkg/llm"}},
	}, Options{})
	require.True(t, ok)

	path, err := PersistSuggestion(t.TempDir(), suggestion)
	require.NoError(t, err)

	data, err := os.ReadFile(path)
	require.NoError(t, err)

	content := string(data)
	for _, want := range []string{
		"`issue` (`{{issue}}`)",
		"Observed examples: gh-12, gh-13.",
		"1. **open <issue>**",
		"- github",
		"- Fix GH-12",
		"- issue GH-12",
		"- issue context",
		"- issue is inaccessible",
		"- `go test ./pkg/auth`",
		"- `go test ./pkg/llm`",
		"Step 1 source actions: open gh-12; open gh-13",
	} {
		require.Contains(t, content, want)
	}
}
