package main

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	attasync "github.com/tommoulard/atteler/pkg/async"
	"github.com/tommoulard/atteler/pkg/review"
	"github.com/tommoulard/atteler/pkg/session"
	"github.com/tommoulard/atteler/pkg/speculate"
	"github.com/tommoulard/atteler/pkg/tasklist"
	"github.com/tommoulard/atteler/pkg/watch"
)

func TestFormatWatchFinding(t *testing.T) {
	t.Parallel()

	got := formatWatchFinding(watch.Finding{
		Path:     "pkg/example/example.go",
		Kind:     watch.KindMissingTest,
		Severity: watch.SeverityInfo,
		Message:  "missing _test.go companion",
	})

	want := strings.Join([]string{
		"path=pkg/example/example.go",
		"kind=missing_test",
		"severity=info",
		"message=missing _test.go companion",
	}, "\t")
	if got != want {
		require.Failf(t, "unexpected watch finding format", "got %q, want %q", got, want)
	}
}

func TestFormatWatchIteration(t *testing.T) {
	t.Parallel()

	started := time.Date(2026, 5, 2, 9, 30, 0, 0, time.UTC)
	got := formatWatchIteration(watch.IterationResult{
		Iteration: 1,
		StartedAt: started,
		Duration:  2 * time.Second,
		Findings: []watch.Finding{
			{Path: "TODO.md", Kind: watch.KindStaleTODO},
			{Path: "pkg/example/example.go", Kind: watch.KindMissingTest},
		},
	})

	want := "iteration=1\tfindings=2\tstarted=2026-05-02T09:30:00Z\tduration=2s"
	if got != want {
		require.Failf(t, "unexpected watch iteration format", "got %q, want %q", got, want)
	}
}

func TestParseAndFormatAsyncPlan(t *testing.T) {
	t.Parallel()

	task, err := parseAsyncTaskSpec("code|coder|implement feature|plan+review")
	if err != nil {
		require.NoError(t, err)
	}

	if task.ID != "code" || task.Agent != "coder" || task.Prompt != "implement feature" {
		require.Failf(t, "unexpected parsed async task", "task = %+v", task)
	}

	if !reflect.DeepEqual(task.DependsOn, []string{"plan", "review"}) {
		require.Failf(t, "unexpected parsed dependencies", "deps = %#v", task.DependsOn)
	}

	plan, err := attasync.NewPlan([]attasync.Task{
		{ID: "plan", Agent: "planner", Prompt: "draft plan"},
		{ID: "review", Agent: "reviewer", Prompt: "review plan", DependsOn: []string{"plan"}},
		{ID: "code", Agent: "coder", Prompt: "implement feature", DependsOn: []string{"plan", "review"}},
	})
	if err != nil {
		require.NoError(t, err)
	}

	got := formatAsyncPlanBatches(plan.ReadyBatches())
	for _, want := range []string{
		"wave 1:\n",
		"id=plan\tagent=planner\tprompt=draft plan",
		"wave 2:\n",
		"id=review\tagent=reviewer\tdepends=plan\tprompt=review plan",
		"wave 3:\n",
		"id=code\tagent=coder\tdepends=plan+review\tprompt=implement feature",
	} {
		if !strings.Contains(got, want) {
			require.Failf(t, "formatted async plan missing content", "missing %q in:\n%s", want, got)
		}
	}
}

func TestValidateAndFormatAsyncRun(t *testing.T) {
	t.Parallel()

	err := validateAsyncRunTasks([]attasync.Task{{ID: "plan", Prompt: "draft"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "agent is required")

	err = validateAsyncRunTasks([]attasync.Task{{ID: "plan", Agent: "planner"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "prompt is required")

	err = validateAsyncRunTasks([]attasync.Task{{ID: "plan", Agent: "planner", Prompt: "draft"}})
	require.NoError(t, err)

	got := formatAsyncRunResults([]attasync.TaskResult{{
		Wave:     0,
		Order:    0,
		Task:     attasync.Task{ID: "plan", Agent: "planner"},
		Output:   "done\n",
		Duration: 1500 * time.Millisecond,
	}, {
		Wave:     1,
		Order:    0,
		Task:     attasync.Task{ID: "code", Agent: "coder"},
		Error:    "boom",
		Duration: time.Millisecond,
	}})

	assert.Contains(t, got, "wave=1\torder=1\tid=plan\tagent=planner\tstatus=ok\tduration=1.5s")
	assert.Contains(t, got, "output=done")
	assert.Contains(t, got, "wave=2\torder=1\tid=code\tagent=coder\tstatus=error\tduration=1ms")
	assert.Contains(t, got, "error=boom")
}

func TestTaskListHelpers(t *testing.T) {
	t.Parallel()

	assert.Equal(t, taskCommandInput{
		FilePath:   "tasks.json",
		AddTitle:   "write contract tests",
		AddID:      "todo-1",
		Agent:      "planner",
		AssignSpec: "todo-1:executor",
		CompleteID: "todo-1",
		List:       true,
	}, taskCommandInputFromOptions(cliOptions{
		taskFilePath:   "tasks.json",
		taskAddTitle:   "write contract tests",
		taskAddID:      "todo-1",
		taskAgent:      "planner",
		taskAssignSpec: "todo-1:executor",
		taskCompleteID: "todo-1",
		taskList:       true,
	}))

	id, agentName, err := parseTaskAssignmentSpec("todo-1:reviewer")
	require.NoError(t, err)
	assert.Equal(t, "todo-1", id)
	assert.Equal(t, "reviewer", agentName)

	_, _, err = parseTaskAssignmentSpec("todo-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "expected id:agent")

	completedAt := time.Date(2026, 5, 5, 12, 30, 0, 0, time.UTC)
	got := formatTaskListItem(tasklist.Task{
		ID:          "todo-1",
		Title:       "wire CLI",
		Status:      tasklist.StatusCompleted,
		Agent:       "reviewer",
		CreatedAt:   completedAt.Add(-time.Hour),
		UpdatedAt:   completedAt,
		CompletedAt: &completedAt,
		Metadata:    map[string]string{"priority": "high", "scope": "cmd"},
	})

	for _, want := range []string{
		"id=todo-1",
		"status=completed",
		"title=wire CLI",
		"agent=reviewer",
		"created_at=2026-05-05T11:30:00Z",
		"updated_at=2026-05-05T12:30:00Z",
		"completed_at=2026-05-05T12:30:00Z",
		"metadata=priority:high,scope:cmd",
	} {
		assert.Contains(t, got, want)
	}
}

func TestRunTaskListCommandPersistsTaskLifecycle(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	taskFile := filepath.Join(t.TempDir(), "tasks.json")
	store := session.NewStore(filepath.Join(t.TempDir(), "sessions"))

	err := runTaskListCommand(ctx, store, taskCommandInput{
		FilePath: taskFile,
		AddID:    "todo-1",
		AddTitle: "draft task package",
		Agent:    "planner",
	})
	require.NoError(t, err)

	err = runTaskListCommand(ctx, store, taskCommandInput{
		FilePath:   taskFile,
		AssignSpec: "todo-1:executor",
	})
	require.NoError(t, err)

	err = runTaskListCommand(ctx, store, taskCommandInput{
		FilePath:   taskFile,
		CompleteID: "todo-1",
		Agent:      "verifier",
	})
	require.NoError(t, err)

	tasks, err := tasklist.NewStore(taskFile).List(ctx)
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	assert.Equal(t, tasklist.StatusCompleted, tasks[0].Status)
	assert.Equal(t, "verifier", tasks[0].Agent)

	history, err := tasklist.NewStore(taskFile).History(ctx)
	require.NoError(t, err)
	assert.Len(t, history, 3)

	err = runTaskListCommand(ctx, store, taskCommandInput{FilePath: taskFile, AddTitle: "new", List: true})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "choose only one")
}

func TestFormatSpeculatePlan(t *testing.T) {
	t.Parallel()

	plan, err := speculate.NewPlan([]string{"alpha", "beta"}, []string{"tests pass"})
	if err != nil {
		require.NoError(t, err)
	}

	got := formatSpeculatePlan(plan)
	for _, want := range []string{
		"agents: alpha,beta\n",
		"rounds:\n",
		"1\tindependent proposals",
		"cross_reviews:\n",
		"alpha -> beta",
		"beta -> alpha",
		"gates:\n  - tests pass\n",
	} {
		if !strings.Contains(got, want) {
			require.Failf(t, "formatted speculate plan missing content", "missing %q in:\n%s", want, got)
		}
	}
}

func TestFormatReviewPlan(t *testing.T) {
	t.Parallel()

	plan, err := review.NewPlan(
		[]review.Reviewer{{Name: "alpha"}, {Name: "beta"}},
		[]string{"pkg/auth.go"},
		[]string{"tests pass"},
	)
	if err != nil {
		require.NoError(t, err)
	}

	got := formatReviewPlan(plan)
	for _, want := range []string{
		"reviewers:\n",
		"  - alpha\n",
		"paths:\n  - pkg/auth.go\n",
		"rounds:\n",
		"1\tindependent-review\tIndependent review\treviewers=alpha,beta",
		"cross_reviews:\n",
		"alpha -> beta",
		"beta -> alpha",
		"gates:\n  - tests pass\n",
	} {
		if !strings.Contains(got, want) {
			require.Failf(t, "formatted review plan missing content", "missing %q in:\n%s", want, got)
		}
	}
}

func TestReviewPlanDefaults(t *testing.T) {
	t.Parallel()

	plan, err := review.NewPlan(reviewPlanReviewers(nil), reviewPlanPaths(nil), nil)
	if err != nil {
		require.NoError(t, err)
	}

	got := formatReviewPlan(plan)
	for _, want := range []string{
		"quality-reviewer\tcategories=correctness,maintainability",
		"test-engineer\tcategories=tests",
		"paths:\n  - .\n",
		"behavioral diff reviewed",
	} {
		if !strings.Contains(got, want) {
			require.Failf(t, "default review plan missing content", "missing %q in:\n%s", want, got)
		}
	}
}

func TestFormatSpeculatePromptCacheEstimate(t *testing.T) {
	t.Parallel()

	plan, err := speculate.NewPlan([]string{"alpha", "beta"}, []string{"tests pass"})
	if err != nil {
		require.NoError(t, err)
	}

	estimate, err := speculate.EstimatePromptCacheReuse(speculateBranchPrompts(plan, "implement auth flow"))
	if err != nil {
		require.NoError(t, err)
	}

	got := formatSpeculatePromptCacheEstimate(estimate)
	for _, want := range []string{
		"prompt_cache:\n",
		"shared_prefix_bytes:",
		"reusable_prompt_bytes:",
		"reuse_ratio:",
		"alpha\tprompt_bytes=",
		"beta\tprompt_bytes=",
	} {
		if !strings.Contains(got, want) {
			require.Failf(t, "formatted speculate prompt cache missing content", "missing %q in:\n%s", want, got)
		}
	}

	if estimate.SharedPrefixBytes == 0 {
		require.FailNow(t, "expected shared branch prompt prefix")
	}
}
