package symphony

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRenderPrompt_StrictVariablesAndLoops(t *testing.T) {
	t.Parallel()

	attempt := 2
	body := `Issue {{ issue.identifier }}: {{ issue.title }}
{% if attempt %}Attempt {{ attempt }}{% endif %}
Labels:{% for label in issue.labels %} {{ label }}{% endfor %}`

	rendered, err := RenderPrompt(body, Issue{
		ID:         "1",
		Identifier: "GH-1",
		Title:      "Fix it",
		State:      "OPEN",
		Labels:     []string{"bug", "p1"},
	}, &attempt)
	require.NoError(t, err)

	assert.Contains(t, rendered, "Issue GH-1: Fix it")
	assert.Contains(t, rendered, "Attempt 2")
	assert.Contains(t, rendered, "Labels: bug p1")
}

func TestRenderPrompt_ExposesIssueComments(t *testing.T) {
	t.Parallel()

	body := `Comments:{% for comment in issue.comments %}
- {{ comment.author }}: {{ comment.body }}{% endfor %}`
	rendered, err := RenderPrompt(body, Issue{
		ID:         "1",
		Identifier: "GH-32",
		Title:      "Persist admission records",
		State:      "OPEN",
		Comments: []IssueComment{
			{Author: "maintainer", Body: "Add denied-before-spawn fixture."},
			{Author: "reviewer", Body: "Add admitted-then-halted fixture."},
		},
	}, nil)
	require.NoError(t, err)

	assert.Contains(t, rendered, "maintainer: Add denied-before-spawn fixture.")
	assert.Contains(t, rendered, "reviewer: Add admitted-then-halted fixture.")
}

func TestRenderPrompt_UnknownVariableFails(t *testing.T) {
	t.Parallel()

	_, err := RenderPrompt("{{ issue.nope }}", Issue{ID: "1", Identifier: "GH-1", Title: "Fix", State: "OPEN"}, nil)
	require.Error(t, err)

	var classed *ClassedError
	require.ErrorAs(t, err, &classed)
	assert.Equal(t, ErrTemplateRender, classed.Class)
}

func TestRenderPrompt_UnknownFilterFails(t *testing.T) {
	t.Parallel()

	_, err := RenderPrompt("{{ issue.title | upcase }}", Issue{ID: "1", Identifier: "GH-1", Title: "Fix", State: "OPEN"}, nil)
	require.Error(t, err)

	var classed *ClassedError
	require.ErrorAs(t, err, &classed)
	assert.Equal(t, ErrTemplateRender, classed.Class)
}

func TestTurnPrompt_IncludesPullRequestReworkContext(t *testing.T) {
	t.Parallel()

	prompt, err := turnPrompt(
		WorkflowDefinition{PromptTemplate: "Work on {{ issue.identifier }}"},
		Issue{ID: "1", Identifier: "GH-2", Title: "Fix CI", State: "OPEN"},
		nil,
		&RunContext{
			Kind: RunKindPullRequestRework,
			PullRequest: &PullRequestReworkContext{
				URL:                  "https://github.com/owner/repo/pull/31",
				Branch:               "symphony/GH-2",
				HeadSHA:              "abc123",
				Summary:              "failing required checks: test",
				FailedChecks:         []string{"test"},
				RequiredFailedChecks: []string{"test"},
				OptionalFailedChecks: []string{"optional-lint"},
				Number:               31,
				ReworkAttempt:        2,
			},
		},
		1,
		8,
	)
	require.NoError(t, err)

	assert.Contains(t, prompt, "Work on GH-2")
	assert.Contains(t, prompt, "Symphony PR rework context")
	assert.Contains(t, prompt, "Pull request: #31 https://github.com/owner/repo/pull/31")
	assert.Contains(t, prompt, "Required failing checks")
	assert.Contains(t, prompt, "Optional failing checks")
	assert.Contains(t, prompt, "test")
	assert.Contains(t, prompt, "optional-lint")
	assert.Contains(t, prompt, "same branch")
	assert.Contains(t, prompt, "git rebase --continue")
}

func TestTurnPrompt_LabelsOptionalCheckReworkAsOptional(t *testing.T) {
	t.Parallel()

	prompt, err := turnPrompt(
		WorkflowDefinition{PromptTemplate: "Work on {{ issue.identifier }}"},
		Issue{ID: "1", Identifier: "GH-2", Title: "Fix CI", State: "OPEN"},
		nil,
		&RunContext{
			Kind: RunKindPullRequestRework,
			PullRequest: &PullRequestReworkContext{
				URL:                  "https://github.com/owner/repo/pull/31",
				Branch:               "symphony/GH-2",
				Summary:              "failing optional checks configured for rework: optional-lint",
				FailedChecks:         []string{"optional-lint"},
				OptionalFailedChecks: []string{"optional-lint"},
				Number:               31,
				ReworkAttempt:        1,
			},
		},
		1,
		8,
	)
	require.NoError(t, err)

	assert.Contains(t, prompt, "Optional failing checks selected for rework by workflow config")
	assert.NotContains(t, prompt, "Required failing checks")
}

func TestTurnPrompt_LabelsOptionalFailuresAsContextForNoChecksPolicyFailure(t *testing.T) {
	t.Parallel()

	prompt, err := turnPrompt(
		WorkflowDefinition{PromptTemplate: "Work on {{ issue.identifier }}"},
		Issue{ID: "1", Identifier: "GH-2", Title: "Fix CI", State: "OPEN"},
		nil,
		&RunContext{
			Kind: RunKindPullRequestRework,
			PullRequest: &PullRequestReworkContext{
				URL:                  "https://github.com/owner/repo/pull/31",
				Branch:               "symphony/GH-2",
				Summary:              "reported checks are optional and no-check policy is fail",
				FailedChecks:         []string{"no required checks"},
				OptionalFailedChecks: []string{"optional-lint"},
				Number:               31,
				ReworkAttempt:        1,
			},
		},
		1,
		8,
	)
	require.NoError(t, err)

	assert.Contains(t, prompt, "Failing checks")
	assert.Contains(t, prompt, "no required checks")
	assert.Contains(t, prompt, "Optional failing checks (reported for context")
	assert.NotContains(t, prompt, "Optional failing checks selected for rework by workflow config")
}

func TestTurnPrompt_AppendsIssueCommentsWhenWorkflowOmitsThem(t *testing.T) {
	t.Parallel()

	createdAt := time.Date(2026, 5, 26, 8, 52, 52, 0, time.UTC)
	commentURL := "https://github.com/owner/repo/issues/32#issuecomment-1"
	prompt, err := turnPrompt(
		WorkflowDefinition{PromptTemplate: "Work on {{ issue.identifier }}"},
		Issue{
			ID:         "1",
			Identifier: "GH-32",
			Title:      "Persist admission records",
			State:      "OPEN",
			Comments: []IssueComment{{
				Author:    "maintainer",
				Body:      "Add a fixture where a child is denied before spawn.",
				URL:       &commentURL,
				CreatedAt: &createdAt,
			}},
		},
		nil,
		nil,
		1,
		8,
	)
	require.NoError(t, err)

	assert.Contains(t, prompt, "Work on GH-32")
	assert.Contains(t, prompt, "Issue discussion comments")
	assert.Contains(t, prompt, "denied before spawn")
	assert.Contains(t, prompt, commentURL)
}

func TestTurnPrompt_DoesNotAppendIssueCommentsWhenWorkflowRendersThem(t *testing.T) {
	t.Parallel()

	prompt, err := turnPrompt(
		WorkflowDefinition{PromptTemplate: `{% for comment in issue.comments %}{{ comment.body }}{% endfor %}`},
		Issue{
			ID:         "1",
			Identifier: "GH-32",
			Title:      "Persist admission records",
			State:      "OPEN",
			Comments:   []IssueComment{{Author: "maintainer", Body: "custom discussion block"}},
		},
		nil,
		nil,
		1,
		8,
	)
	require.NoError(t, err)

	assert.Contains(t, prompt, "custom discussion block")
	assert.NotContains(t, prompt, "Issue discussion comments")
}
