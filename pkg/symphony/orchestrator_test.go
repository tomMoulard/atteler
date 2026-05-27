package symphony

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type noopTracker struct{}

func (noopTracker) FetchCandidateIssues(context.Context) ([]Issue, error) {
	return nil, nil
}

func (noopTracker) FetchIssuesByStates(context.Context, []string) ([]Issue, error) {
	return nil, nil
}

func (noopTracker) FetchIssueStatesByIDs(context.Context, []string) ([]Issue, error) {
	return nil, nil
}

type checkTracker struct {
	noopTracker
	checks PullRequestCheckSnapshot
}

func (t checkTracker) FetchPullRequestChecks(context.Context, int) (PullRequestCheckSnapshot, error) {
	return t.checks, nil
}

type policyCheckTracker struct {
	noopTracker
	checks PullRequestCheckSnapshot
	policy PullRequestCheckPolicy
	number int
	calls  int
}

func (t *policyCheckTracker) FetchPullRequestChecksWithPolicy(_ context.Context, number int, policy PullRequestCheckPolicy) (PullRequestCheckSnapshot, error) {
	t.calls++
	t.number = number
	t.policy = policy

	return t.checks, nil
}

type captureRunner struct {
	requests chan RunRequest
}

func (r captureRunner) Run(ctx context.Context, req RunRequest, _ func(CodexEvent)) (RunResult, error) {
	r.requests <- req

	<-ctx.Done()

	return RunResult{Status: AttemptCanceled}, ctx.Err()
}

func TestHandleWorkerExit_PublishedPRSchedulesMonitorAndReleasesClaim(t *testing.T) {
	t.Parallel()

	issue := Issue{ID: "issue-node", Identifier: "GH-2", Title: "Fix CI", State: "OPEN"}
	cfg := Config{
		Tracker: TrackerConfig{
			Kind: trackerKindGitHub,
		},
		Publish: PublishConfig{
			Enabled:                true,
			MonitorChecks:          true,
			CheckInterval:          time.Hour,
			MaxCheckReworkAttempts: 3,
		},
		Agent: AgentConfig{
			MaxConcurrentAgents: 2,
		},
	}
	orchestrator := &Orchestrator{
		manager: &WorkflowManager{
			current: WorkflowSnapshot{Config: cfg},
			loaded:  true,
		},
		tracker: noopTracker{},
		logger:  slog.Default(),
		events:  make(chan orchestratorEvent, 4),
		state: runtimeState{
			Running: map[string]*runningEntry{
				issue.ID: {
					Issue:     issue,
					StartedAt: time.Now().Add(-time.Second),
					State:     issue.State,
				},
			},
			Claimed:               map[string]struct{}{issue.ID: {}},
			RetryAttempts:         map[string]*RetryEntry{},
			PullRequests:          map[int]*pullRequestMonitorEntry{},
			Completed:             map[string]struct{}{},
			CompletedPullRequests: map[int]struct{}{},
			StartedAt:             time.Now(),
		},
	}

	orchestrator.handleWorkerExit(workerExitEvent{
		issueID: issue.ID,
		result: RunResult{
			Status: AttemptSucceeded,
			Publish: &PublishResult{
				Branch:            "symphony/GH-2",
				PullRequestNumber: 31,
				PullRequestURL:    "https://github.com/owner/repo/pull/31",
				Published:         true,
			},
		},
	})

	_, claimed := orchestrator.state.Claimed[issue.ID]
	assert.False(t, claimed)
	assert.Contains(t, orchestrator.state.Completed, issue.ID)
	require.Contains(t, orchestrator.state.PullRequests, 31)
	monitor := orchestrator.state.PullRequests[31]
	assert.Equal(t, "symphony/GH-2", monitor.Branch)
	assert.Equal(t, "https://github.com/owner/repo/pull/31", monitor.PullRequestURL)
	assert.False(t, monitor.NextCheckAt.IsZero())
	require.NotNil(t, monitor.Timer)
	monitor.Timer.Stop()
}

func TestHandleCodexUpdateRecordsCommandOutputMetadata(t *testing.T) {
	t.Parallel()

	issue := Issue{ID: "issue-node", Identifier: "GH-2", Title: "Stream output", State: "OPEN"}
	longOutput := strings.Repeat("x", 1200)
	orchestrator := &Orchestrator{
		manager: &WorkflowManager{
			current: WorkflowSnapshot{Config: Config{}},
			loaded:  true,
		},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		state: runtimeState{
			Running: map[string]*runningEntry{
				issue.ID: {
					Issue:     issue,
					StartedAt: time.Now().Add(-time.Second),
					State:     issue.State,
				},
			},
		},
	}

	orchestrator.handleCodexUpdate(codexUpdateEvent{
		issueID: issue.ID,
		event: CodexEvent{
			Event:        codexEventExecCommandOutputDelta,
			ThreadID:     "thread-1",
			TurnID:       "turn-1",
			SessionID:    "thread-1-turn-1",
			CommandID:    "cmd-1",
			ProcessID:    "proc-1",
			Command:      "make test",
			OutputStream: "stdout",
			OutputChunk:  longOutput,
		},
	})

	require.Len(t, orchestrator.state.RecentEvents, 1)
	event := orchestrator.state.RecentEvents[0]
	assert.Equal(t, "codex_event", event.Kind)
	assert.Equal(t, issue.Identifier, event.Issue)
	assert.Equal(t, codexEventExecCommandOutputDelta, event.Fields["event"])
	assert.Equal(t, "cmd-1", event.Fields["command_id"])
	assert.Equal(t, "proc-1", event.Fields["process_id"])
	assert.Equal(t, "make test", event.Fields["command"])
	assert.Equal(t, "stdout", event.Fields["stream"])
	assert.Equal(t, commandOutputPreview(longOutput), event.Fields["output"])
	assert.Equal(t, len(longOutput), event.Fields["output_bytes"])
	assert.Equal(t, true, event.Fields["output_truncated"])
}

func TestHandlePullRequestCheckDue_DispatchesReworkForFailedChecks(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	issue := Issue{ID: "issue-node", Identifier: "GH-2", Title: "Fix CI", State: "OPEN"}
	cfg := Config{
		Tracker: TrackerConfig{
			Kind: trackerKindGitHub,
		},
		Publish: PublishConfig{
			Enabled:                true,
			MonitorChecks:          true,
			CheckInterval:          time.Hour,
			MaxCheckReworkAttempts: 3,
		},
		Agent: AgentConfig{
			MaxConcurrentAgents: 2,
		},
	}
	runner := captureRunner{requests: make(chan RunRequest, 1)}
	orchestrator := &Orchestrator{
		manager: &WorkflowManager{
			current: WorkflowSnapshot{Config: cfg},
			loaded:  true,
		},
		tracker: checkTracker{checks: PullRequestCheckSnapshot{
			CheckedAt:                time.Now().UTC(),
			PullRequestURL:           "https://github.com/owner/repo/pull/31",
			HeadRef:                  "symphony/GH-2",
			HeadSHA:                  "abc123",
			Summary:                  "failing required checks: test",
			FailedCheckNames:         []string{"test"},
			RequiredFailedCheckNames: []string{"test"},
			OptionalFailedCheckNames: []string{"optional-lint"},
			State:                    PullRequestChecksFailed,
		}},
		runner: runner,
		logger: slog.Default(),
		events: make(chan orchestratorEvent, 4),
		state: runtimeState{
			Running:       map[string]*runningEntry{},
			Claimed:       map[string]struct{}{},
			RetryAttempts: map[string]*RetryEntry{},
			PullRequests: map[int]*pullRequestMonitorEntry{
				31: {
					Issue:          issue,
					Branch:         "symphony/GH-2",
					PullRequestURL: "https://github.com/owner/repo/pull/31",
					Number:         31,
				},
			},
			Completed:             map[string]struct{}{},
			CompletedPullRequests: map[int]struct{}{},
			StartedAt:             time.Now(),
		},
	}

	orchestrator.handlePullRequestCheckDue(ctx, 31)

	req := <-runner.requests

	require.NotNil(t, req.Context)
	require.NotNil(t, req.Context.PullRequest)
	assert.Equal(t, RunKindPullRequestRework, req.Context.Kind)
	assert.Equal(t, 31, req.Context.PullRequest.Number)
	assert.Equal(t, []string{"test"}, req.Context.PullRequest.FailedChecks)
	assert.Equal(t, []string{"test"}, req.Context.PullRequest.RequiredFailedChecks)
	assert.Equal(t, []string{"optional-lint"}, req.Context.PullRequest.OptionalFailedChecks)
	assert.Equal(t, 1, req.Context.PullRequest.ReworkAttempt)
	assert.Contains(t, orchestrator.state.Running, issue.ID)
	assert.Contains(t, orchestrator.state.Claimed, issue.ID)

	cancel()
	orchestrator.wg.Wait()
}

func TestHandlePullRequestCheckDue_CompletesWhenOnlyOptionalChecksFail(t *testing.T) {
	t.Parallel()

	issue := Issue{ID: "issue-node", Identifier: "GH-2", Title: "Fix CI", State: "OPEN"}
	cfg := Config{
		Tracker: TrackerConfig{
			Kind: trackerKindGitHub,
		},
		Publish: PublishConfig{
			Enabled:                true,
			MonitorChecks:          true,
			CheckInterval:          time.Hour,
			MaxCheckReworkAttempts: 3,
		},
		Agent: AgentConfig{
			MaxConcurrentAgents: 2,
		},
	}
	runner := captureRunner{requests: make(chan RunRequest, 1)}
	orchestrator := &Orchestrator{
		manager: &WorkflowManager{
			current: WorkflowSnapshot{Config: cfg},
			loaded:  true,
		},
		tracker: checkTracker{checks: PullRequestCheckSnapshot{
			CheckedAt:                time.Now().UTC(),
			PullRequestURL:           "https://github.com/owner/repo/pull/31",
			HeadRef:                  "symphony/GH-2",
			HeadSHA:                  "abc123",
			Summary:                  "required checks passed; optional failing checks: optional-lint",
			OptionalFailedCheckNames: []string{"optional-lint"},
			State:                    PullRequestChecksPassed,
		}},
		runner: runner,
		logger: slog.Default(),
		events: make(chan orchestratorEvent, 4),
		state: runtimeState{
			Running:       map[string]*runningEntry{},
			Claimed:       map[string]struct{}{},
			RetryAttempts: map[string]*RetryEntry{},
			PullRequests: map[int]*pullRequestMonitorEntry{
				31: {
					Issue:          issue,
					Branch:         "symphony/GH-2",
					PullRequestURL: "https://github.com/owner/repo/pull/31",
					Number:         31,
				},
			},
			Completed:             map[string]struct{}{},
			CompletedPullRequests: map[int]struct{}{},
			StartedAt:             time.Now(),
		},
	}

	orchestrator.handlePullRequestCheckDue(t.Context(), 31)

	assert.NotContains(t, orchestrator.state.PullRequests, 31)
	assert.Contains(t, orchestrator.state.CompletedPullRequests, 31)

	select {
	case req := <-runner.requests:
		t.Fatalf("optional failure unexpectedly dispatched rework: %#v", req.Context)
	default:
	}
}

func TestHandlePullRequestCheckDue_PassesPublishCheckPolicyToTracker(t *testing.T) {
	t.Parallel()

	issue := Issue{ID: "issue-node", Identifier: "GH-2", Title: "Fix CI", State: "OPEN"}
	cfg := Config{
		Tracker: TrackerConfig{
			Kind: trackerKindGitHub,
		},
		Publish: PublishConfig{
			Enabled:                true,
			MonitorChecks:          true,
			CheckInterval:          time.Hour,
			MaxCheckReworkAttempts: 3,
			NoChecksPolicy:         PullRequestNoChecksFail,
			RequiredCheckNames:     []string{"test", "build"},
			RequiredCheckPatterns:  []string{"ci/*"},
			DiscoverRequiredChecks: true,
			ReworkOptionalChecks:   true,
		},
		Agent: AgentConfig{
			MaxConcurrentAgents: 2,
		},
	}
	tracker := &policyCheckTracker{checks: PullRequestCheckSnapshot{
		CheckedAt:      time.Now().UTC(),
		PullRequestURL: "https://github.com/owner/repo/pull/31",
		HeadRef:        "symphony/GH-2",
		HeadSHA:        "abc123",
		Summary:        "required checks passed",
		State:          PullRequestChecksPassed,
	}}
	orchestrator := &Orchestrator{
		manager: &WorkflowManager{
			current: WorkflowSnapshot{Config: cfg},
			loaded:  true,
		},
		tracker: tracker,
		logger:  slog.Default(),
		events:  make(chan orchestratorEvent, 4),
		state: runtimeState{
			Running:       map[string]*runningEntry{},
			Claimed:       map[string]struct{}{},
			RetryAttempts: map[string]*RetryEntry{},
			PullRequests: map[int]*pullRequestMonitorEntry{
				31: {
					Issue:          issue,
					Branch:         "symphony/GH-2",
					PullRequestURL: "https://github.com/owner/repo/pull/31",
					Number:         31,
				},
			},
			Completed:             map[string]struct{}{},
			CompletedPullRequests: map[int]struct{}{},
			StartedAt:             time.Now(),
		},
	}

	orchestrator.handlePullRequestCheckDue(t.Context(), 31)

	assert.Equal(t, 1, tracker.calls)
	assert.Equal(t, 31, tracker.number)
	assert.Equal(t, PullRequestNoChecksFail, tracker.policy.NoChecksPolicy)
	assert.Equal(t, []string{"test", "build"}, tracker.policy.RequiredCheckNames)
	assert.Equal(t, []string{"ci/*"}, tracker.policy.RequiredCheckPatterns)
	assert.True(t, tracker.policy.DiscoverRequiredChecks)
	assert.True(t, tracker.policy.ReworkOptionalChecks)
	assert.False(t, tracker.policy.TreatAllReportedAsRequired)
}

func TestHandlePullRequestCheckDue_UpdatesBranchBeforeCompletingChecks(t *testing.T) {
	t.Parallel()

	issue := Issue{ID: "issue-node", Identifier: "GH-2", Title: "Fix CI", State: "OPEN"}
	cfg := Config{
		Tracker: TrackerConfig{
			Kind: trackerKindGitHub,
		},
		Publish: PublishConfig{
			Enabled:                true,
			MonitorChecks:          true,
			CheckInterval:          time.Hour,
			MaxCheckReworkAttempts: 3,
		},
		Agent: AgentConfig{
			MaxConcurrentAgents: 2,
		},
	}

	var updatedBranch string

	orchestrator := &Orchestrator{
		manager: &WorkflowManager{
			current: WorkflowSnapshot{Config: cfg},
			loaded:  true,
		},
		tracker: checkTracker{checks: PullRequestCheckSnapshot{
			CheckedAt:          time.Now().UTC(),
			PullRequestURL:     "https://github.com/owner/repo/pull/31",
			HeadRef:            "symphony/GH-2",
			HeadSHA:            "abc123",
			Summary:            "all reported checks have passed",
			State:              PullRequestChecksPassed,
			NeedsBranchUpdate:  true,
			BranchUpdateReason: "pull request branch is behind main",
		}},
		logger: slog.Default(),
		events: make(chan orchestratorEvent, 4),
		updatePullRequestBranch: func(_ context.Context, _ Config, _ Issue, branch string, _ *slog.Logger) (string, error) {
			updatedBranch = branch
			return "def456", nil
		},
		state: runtimeState{
			Running:       map[string]*runningEntry{},
			Claimed:       map[string]struct{}{},
			RetryAttempts: map[string]*RetryEntry{},
			PullRequests: map[int]*pullRequestMonitorEntry{
				31: {
					Issue:          issue,
					Branch:         "symphony/GH-2",
					PullRequestURL: "https://github.com/owner/repo/pull/31",
					Number:         31,
				},
			},
			Completed:             map[string]struct{}{},
			CompletedPullRequests: map[int]struct{}{},
			StartedAt:             time.Now(),
		},
	}

	orchestrator.handlePullRequestCheckDue(t.Context(), 31)

	assert.Equal(t, "symphony/GH-2", updatedBranch)
	require.Contains(t, orchestrator.state.PullRequests, 31)
	assert.NotContains(t, orchestrator.state.CompletedPullRequests, 31)
	monitor := orchestrator.state.PullRequests[31]
	assert.Contains(t, monitor.LastError, "branch update pushed")
	assert.False(t, monitor.NextCheckAt.IsZero())
	require.NotNil(t, monitor.Timer)
	monitor.Timer.Stop()
}

func TestHandlePullRequestCheckDue_DispatchesReworkWhenBranchUpdateFails(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	issue := Issue{ID: "issue-node", Identifier: "GH-2", Title: "Fix CI", State: "OPEN"}
	cfg := Config{
		Tracker: TrackerConfig{
			Kind: trackerKindGitHub,
		},
		Publish: PublishConfig{
			Enabled:                true,
			MonitorChecks:          true,
			CheckInterval:          time.Hour,
			MaxCheckReworkAttempts: 3,
		},
		Agent: AgentConfig{
			MaxConcurrentAgents: 2,
		},
	}
	runner := captureRunner{requests: make(chan RunRequest, 1)}
	orchestrator := &Orchestrator{
		manager: &WorkflowManager{
			current: WorkflowSnapshot{Config: cfg},
			loaded:  true,
		},
		tracker: checkTracker{checks: PullRequestCheckSnapshot{
			CheckedAt:          time.Now().UTC(),
			PullRequestURL:     "https://github.com/owner/repo/pull/31",
			HeadRef:            "symphony/GH-2",
			HeadSHA:            "abc123",
			Summary:            "all reported checks have passed",
			State:              PullRequestChecksPassed,
			NeedsBranchUpdate:  true,
			BranchUpdateReason: "pull request branch has merge conflicts with main",
		}},
		runner: runner,
		logger: slog.Default(),
		events: make(chan orchestratorEvent, 4),
		updatePullRequestBranch: func(context.Context, Config, Issue, string, *slog.Logger) (string, error) {
			return "", errors.New("rebase conflict")
		},
		state: runtimeState{
			Running:       map[string]*runningEntry{},
			Claimed:       map[string]struct{}{},
			RetryAttempts: map[string]*RetryEntry{},
			PullRequests: map[int]*pullRequestMonitorEntry{
				31: {
					Issue:          issue,
					Branch:         "symphony/GH-2",
					PullRequestURL: "https://github.com/owner/repo/pull/31",
					Number:         31,
				},
			},
			Completed:             map[string]struct{}{},
			CompletedPullRequests: map[int]struct{}{},
			StartedAt:             time.Now(),
		},
	}

	orchestrator.handlePullRequestCheckDue(ctx, 31)

	req := <-runner.requests

	require.NotNil(t, req.Context)
	require.NotNil(t, req.Context.PullRequest)
	assert.Equal(t, RunKindPullRequestRework, req.Context.Kind)
	assert.Equal(t, []string{"branch update"}, req.Context.PullRequest.FailedChecks)
	assert.Contains(t, req.Context.PullRequest.Summary, "rebase conflict")

	cancel()
	orchestrator.wg.Wait()
}

func TestHandlePullRequestCheckDue_DoesNotRepeatFailedBranchUpdateWhileReworkQueued(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	issue := Issue{ID: "issue-node", Identifier: "GH-2", Title: "Fix CI", State: "OPEN"}
	cfg := Config{
		Tracker: TrackerConfig{
			Kind: trackerKindGitHub,
		},
		Publish: PublishConfig{
			Enabled:                true,
			MonitorChecks:          true,
			CheckInterval:          time.Hour,
			MaxCheckReworkAttempts: 3,
		},
		Agent: AgentConfig{
			MaxConcurrentAgents: 1,
		},
	}
	checks := PullRequestCheckSnapshot{
		CheckedAt:          time.Now().UTC(),
		PullRequestURL:     "https://github.com/owner/repo/pull/31",
		HeadRef:            "symphony/GH-2",
		HeadSHA:            "abc123",
		BaseSHA:            "base123",
		Summary:            "pull request branch has merge conflicts with main",
		State:              PullRequestChecksPending,
		NeedsBranchUpdate:  true,
		BranchUpdateReason: "pull request branch has merge conflicts with main",
	}

	updateCalls := 0
	runner := captureRunner{requests: make(chan RunRequest, 1)}
	orchestrator := &Orchestrator{
		manager: &WorkflowManager{
			current: WorkflowSnapshot{Config: cfg},
			loaded:  true,
		},
		tracker: checkTracker{checks: checks},
		runner:  runner,
		logger:  slog.Default(),
		events:  make(chan orchestratorEvent, 4),
		updatePullRequestBranch: func(context.Context, Config, Issue, string, *slog.Logger) (string, error) {
			updateCalls++
			return "", errors.New("rebase conflict")
		},
		state: runtimeState{
			Running: map[string]*runningEntry{
				"other-issue": {
					Issue: Issue{ID: "other-issue", Identifier: "GH-3", Title: "Busy", State: "OPEN"},
					State: "OPEN",
				},
			},
			Claimed:       map[string]struct{}{},
			RetryAttempts: map[string]*RetryEntry{},
			PullRequests: map[int]*pullRequestMonitorEntry{
				31: {
					Issue:          issue,
					Branch:         "symphony/GH-2",
					PullRequestURL: "https://github.com/owner/repo/pull/31",
					Number:         31,
				},
			},
			Completed:             map[string]struct{}{},
			CompletedPullRequests: map[int]struct{}{},
			StartedAt:             time.Now(),
		},
	}

	orchestrator.handlePullRequestCheckDue(ctx, 31)

	require.Equal(t, 1, updateCalls)

	monitor := orchestrator.state.PullRequests[31]
	require.NotNil(t, monitor.PendingRework)
	assert.Contains(t, monitor.LastError, "no available orchestrator slots")
	require.NotNil(t, monitor.Timer)
	monitor.Timer.Stop()

	orchestrator.state.Running = map[string]*runningEntry{}
	monitor.Timer = nil

	orchestrator.handlePullRequestCheckDue(ctx, 31)

	req := <-runner.requests

	assert.Equal(t, 1, updateCalls)
	require.NotNil(t, req.Context)
	require.NotNil(t, req.Context.PullRequest)
	assert.Equal(t, RunKindPullRequestRework, req.Context.Kind)
	assert.Contains(t, req.Context.PullRequest.Summary, "rebase conflict")

	cancel()
	orchestrator.wg.Wait()
}

func TestHandleWorkerExit_CanceledPullRequestReworkSchedulesFinalCheck(t *testing.T) {
	t.Parallel()

	issue := Issue{ID: "issue-node", Identifier: "GH-2", Title: "Fix CI", State: "OPEN"}
	cfg := Config{
		Publish: PublishConfig{
			Enabled:       true,
			MonitorChecks: true,
			CheckInterval: time.Hour,
		},
		Agent: AgentConfig{
			MaxConcurrentAgents: 2,
		},
	}
	orchestrator := &Orchestrator{
		manager: &WorkflowManager{
			current: WorkflowSnapshot{Config: cfg},
			loaded:  true,
		},
		logger: slog.Default(),
		events: make(chan orchestratorEvent, 4),
		state: runtimeState{
			Running: map[string]*runningEntry{
				issue.ID: {
					Issue:        issue,
					StartedAt:    time.Now().Add(-time.Second),
					State:        issue.State,
					RunKind:      RunKindPullRequestRework,
					CancelReason: cancelTerminal,
					PullRequest: &PullRequestReworkContext{
						Number: 31,
					},
				},
			},
			Claimed:       map[string]struct{}{issue.ID: {}},
			RetryAttempts: map[string]*RetryEntry{},
			PullRequests: map[int]*pullRequestMonitorEntry{
				31: {
					Issue:    issue,
					Number:   31,
					InRework: true,
				},
			},
			Completed:             map[string]struct{}{},
			CompletedPullRequests: map[int]struct{}{},
			StartedAt:             time.Now(),
		},
	}

	orchestrator.handleWorkerExit(workerExitEvent{issueID: issue.ID})

	_, claimed := orchestrator.state.Claimed[issue.ID]
	assert.False(t, claimed)

	monitor := orchestrator.state.PullRequests[31]
	require.NotNil(t, monitor)
	assert.False(t, monitor.InRework)
	assert.Contains(t, monitor.LastError, "terminal")
	assert.False(t, monitor.NextCheckAt.IsZero())
	require.NotNil(t, monitor.Timer)
	monitor.Timer.Stop()
}

func TestRecoverPullRequestMonitorTimersRearmsStaleMonitor(t *testing.T) {
	t.Parallel()

	issue := Issue{ID: "issue-node", Identifier: "GH-2", Title: "Fix CI", State: "OPEN"}
	orchestrator := &Orchestrator{
		logger: slog.Default(),
		events: make(chan orchestratorEvent, 4),
		state: runtimeState{
			PullRequests: map[int]*pullRequestMonitorEntry{
				31: {
					Issue:          issue,
					Branch:         "symphony/GH-2",
					PullRequestURL: "https://github.com/owner/repo/pull/31",
					Number:         31,
				},
			},
			RecentEvents:          []DebugEvent{},
			CompletedPullRequests: map[int]struct{}{},
		},
	}

	orchestrator.recoverPullRequestMonitorTimers(Config{
		Publish: PublishConfig{
			Enabled:       true,
			MonitorChecks: true,
		},
	})

	monitor := orchestrator.state.PullRequests[31]
	assert.False(t, monitor.NextCheckAt.IsZero())
	assert.Contains(t, monitor.LastError, "recovered")
	require.NotNil(t, monitor.Timer)
	monitor.Timer.Stop()
}

func TestDebugSnapshotIncludesPullRequestCheckClassification(t *testing.T) {
	t.Parallel()

	issue := Issue{ID: "issue-node", Identifier: "GH-2", Title: "Fix CI", State: "OPEN"}
	checks := PullRequestCheckSnapshot{
		CheckedAt:                 time.Now().UTC(),
		Summary:                   "failing required checks: test",
		RequirementSource:         "config",
		NoChecksPolicy:            string(PullRequestNoChecksPass),
		BranchProtectionError:     "branch protection required status checks: HTTP 403",
		RulesetError:              "ruleset required status checks: HTTP 403",
		RequiredCheckNames:        []string{"test"},
		RequiredFailedCheckNames:  []string{"test"},
		OptionalFailedCheckNames:  []string{"optional-lint"},
		PendingRequiredCheckNames: []string{"build"},
		State:                     PullRequestChecksFailed,
	}
	orchestrator := &Orchestrator{
		manager: &WorkflowManager{
			current: WorkflowSnapshot{Config: Config{
				Publish: PublishConfig{
					NoChecksPolicy:         PullRequestNoChecksPass,
					RequiredCheckNames:     []string{"test"},
					RequiredCheckPatterns:  []string{"ci/*"},
					DiscoverRequiredChecks: true,
					ReworkOptionalChecks:   false,
				},
			}},
			loaded: true,
		},
		state: runtimeState{
			PullRequests: map[int]*pullRequestMonitorEntry{31: {
				Issue:        issue,
				LastSnapshot: checks,
				Number:       31,
			}},
			Claimed:       map[string]struct{}{},
			Completed:     map[string]struct{}{},
			RetryAttempts: map[string]*RetryEntry{},
			Running:       map[string]*runningEntry{},
			StartedAt:     time.Now(),
		},
	}

	snapshot := orchestrator.buildDebugSnapshot(time.Now().UTC())

	assert.Equal(t, []string{"test"}, snapshot.Config.PublishRequiredChecks)
	assert.Equal(t, []string{"ci/*"}, snapshot.Config.PublishRequiredCheckPatterns)
	assert.Equal(t, string(PullRequestNoChecksPass), snapshot.Config.PublishNoChecksPolicy)
	require.Len(t, snapshot.PullRequests, 1)
	assert.Equal(t, checks.RequiredFailedCheckNames, snapshot.PullRequests[0].LastSnapshot.RequiredFailedCheckNames)
	assert.Equal(t, checks.OptionalFailedCheckNames, snapshot.PullRequests[0].LastSnapshot.OptionalFailedCheckNames)
	assert.Equal(t, checks.RequirementSource, snapshot.PullRequests[0].LastSnapshot.RequirementSource)
	assert.Equal(t, checks.BranchProtectionError, snapshot.PullRequests[0].LastSnapshot.BranchProtectionError)
	assert.Equal(t, checks.RulesetError, snapshot.PullRequests[0].LastSnapshot.RulesetError)
	statusSummary := strings.Join(snapshot.Summary.WhatIsGoingOn, "\n")
	assert.Contains(t, statusSummary, "requirement source: config")
	assert.Contains(t, statusSummary, "no-check policy: pass")
	assert.Contains(t, statusSummary, "required failed: test")
	assert.Contains(t, statusSummary, "required pending: build")
	assert.Contains(t, statusSummary, "optional failed: optional-lint")
	assert.Contains(t, statusSummary, "branch protection lookup error")
	assert.Contains(t, statusSummary, "ruleset lookup error")
}

func TestHandlePullRequestCheckDue_CompletesClosedPullRequest(t *testing.T) {
	t.Parallel()

	issue := Issue{ID: "issue-node", Identifier: "GH-2", Title: "Fix CI", State: "OPEN"}
	cfg := Config{
		Publish: PublishConfig{
			Enabled:                true,
			MonitorChecks:          true,
			CheckInterval:          time.Hour,
			MaxCheckReworkAttempts: 3,
		},
		Agent: AgentConfig{
			MaxConcurrentAgents: 2,
		},
	}
	orchestrator := &Orchestrator{
		manager: &WorkflowManager{
			current: WorkflowSnapshot{Config: cfg},
			loaded:  true,
		},
		tracker: checkTracker{checks: PullRequestCheckSnapshot{
			CheckedAt:         time.Now().UTC(),
			PullRequestURL:    "https://github.com/owner/repo/pull/31",
			HeadRef:           "symphony/GH-2",
			State:             PullRequestChecksPassed,
			Summary:           "pull request is closed; no rework will be scheduled",
			PullRequestClosed: true,
			PullRequestNumber: 31,
		}},
		logger: slog.Default(),
		events: make(chan orchestratorEvent, 4),
		state: runtimeState{
			Running:       map[string]*runningEntry{},
			Claimed:       map[string]struct{}{},
			RetryAttempts: map[string]*RetryEntry{},
			PullRequests: map[int]*pullRequestMonitorEntry{
				31: {
					Issue:          issue,
					Branch:         "symphony/GH-2",
					PullRequestURL: "https://github.com/owner/repo/pull/31",
					Number:         31,
				},
			},
			Completed:             map[string]struct{}{},
			CompletedPullRequests: map[int]struct{}{},
			StartedAt:             time.Now(),
		},
	}

	orchestrator.handlePullRequestCheckDue(t.Context(), 31)

	assert.NotContains(t, orchestrator.state.PullRequests, 31)
	assert.Contains(t, orchestrator.state.CompletedPullRequests, 31)
}
