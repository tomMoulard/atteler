package skill

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

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

func TestSuggest_NoRepeatedMultiStepSequence(t *testing.T) {
	t.Parallel()

	if got, ok := Suggest([]string{"plan", "code", "test"}); ok {
		t.Fatalf("Suggest returned %#v, true; want no suggestion", got)
	}
}

func TestPersistSuggestion_WritesMarkdownWithoutOverwrite(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	suggestion, ok := Suggest([]string{"plan", "code", "plan", "code"})
	require.True(t, ok)

	path, err := PersistSuggestion(dir, suggestion)
	require.NoError(t, err)
	require.Equal(t, "plan-code.md", filepath.Base(path))

	data, err := os.ReadFile(path)
	require.NoError(t, err)

	content := string(data)
	for _, want := range []string{"# Plan Code Skill", "slug: plan-code", "## Steps", "- plan", "- code"} {
		require.Contains(t, content, want)
	}

	_, err = PersistSuggestion(dir, suggestion)
	require.Error(t, err)
}
