//nolint:gocritic,gosec,govet,revive,wsl_v5,perfsprint,wastedassign,modernize // The orchestrator intentionally keeps the spec state machine explicit.
package symphony

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"
)

// Orchestrator owns Symphony scheduling state and state transitions.
type Orchestrator struct {
	manager                 *WorkflowManager
	tracker                 TrackerClient
	runner                  AgentRunner
	workspaces              *WorkspaceManager
	logger                  *slog.Logger
	events                  chan orchestratorEvent
	state                   runtimeState
	updatePullRequestBranch pullRequestBranchUpdater
	wg                      sync.WaitGroup
}

type pullRequestBranchUpdater func(context.Context, Config, Issue, string, *slog.Logger) (string, error)

type runtimeState struct {
	Running               map[string]*runningEntry
	Claimed               map[string]struct{}
	RetryAttempts         map[string]*RetryEntry
	PullRequests          map[int]*pullRequestMonitorEntry
	Completed             map[string]struct{}
	CompletedPullRequests map[int]struct{}
	CodexRateLimits       jsonRaw
	RecentEvents          []DebugEvent
	StartedAt             time.Time
	LastTickAt            time.Time
	NextTickAt            time.Time
	PollInterval          time.Duration
	MaxConcurrentAgents   int
	CodexTotals           codexTotals
}

type jsonRaw []byte

type codexTotals struct {
	RuntimeSeconds int64
	InputTokens    int64
	OutputTokens   int64
	TotalTokens    int64
}

type runningEntry struct {
	Issue          Issue
	Cancel         context.CancelFunc
	CancelReason   cancelReason
	WorkspacePath  string
	StartedAt      time.Time
	LastCodexTime  time.Time
	LastCodexEvent string
	LastMessage    string
	SessionID      string
	ThreadID       string
	TurnID         string
	AppServerPID   string
	State          string
	Attempt        int
	RunKind        RunKind
	PullRequest    *PullRequestReworkContext
	InputTokens    int64
	OutputTokens   int64
	TotalTokens    int64
}

type RetryEntry struct {
	Timer      *time.Timer
	DueAt      time.Time
	IssueID    string
	Identifier string
	Error      string
	Attempt    int
}

type pullRequestMonitorEntry struct {
	PendingRework    *PullRequestCheckSnapshot
	Issue            Issue
	LastSnapshot     PullRequestCheckSnapshot
	Timer            *time.Timer
	NextCheckAt      time.Time
	LastCheckAt      time.Time
	LastReworkAt     time.Time
	LastError        string
	Branch           string
	PullRequestURL   string
	PendingReworkKey string
	ReworkAttempts   int
	Number           int
	InRework         bool
	Exhausted        bool
}

type cancelReason string

const (
	cancelNone      cancelReason = ""
	cancelTerminal  cancelReason = "terminal"
	cancelNonActive cancelReason = "non_active"
	cancelStalled   cancelReason = "stalled"
	cancelShutdown  cancelReason = "shutdown"
)

type orchestratorEvent interface{ isOrchestratorEvent() }

type workerExitEvent struct {
	result  RunResult
	err     error
	issueID string
}

type codexUpdateEvent struct {
	event   CodexEvent
	issueID string
}

type retryDueEvent struct {
	issueID string
}

type pullRequestCheckDueEvent struct {
	number int
}

type debugSnapshotEvent struct {
	reply chan debugSnapshotResponse
}

type debugSnapshotResponse struct {
	snapshot DebugSnapshot
	err      error
}

func (workerExitEvent) isOrchestratorEvent()          {}
func (codexUpdateEvent) isOrchestratorEvent()         {}
func (retryDueEvent) isOrchestratorEvent()            {}
func (pullRequestCheckDueEvent) isOrchestratorEvent() {}
func (debugSnapshotEvent) isOrchestratorEvent()       {}

// NewOrchestrator creates an orchestrator using the current workflow snapshot.
func NewOrchestrator(manager *WorkflowManager, tracker TrackerClient, runner AgentRunner, logger *slog.Logger) (*Orchestrator, error) {
	snapshot, ok := manager.Current()
	if !ok {
		return nil, fmt.Errorf("symphony orchestrator: workflow has not been loaded")
	}

	return &Orchestrator{
		manager:                 manager,
		tracker:                 tracker,
		runner:                  runner,
		workspaces:              NewWorkspaceManager(logger),
		logger:                  loggerOrDefault(logger),
		events:                  make(chan orchestratorEvent, 128),
		updatePullRequestBranch: UpdatePullRequestBranch,
		state: runtimeState{
			Running:               map[string]*runningEntry{},
			Claimed:               map[string]struct{}{},
			RetryAttempts:         map[string]*RetryEntry{},
			PullRequests:          map[int]*pullRequestMonitorEntry{},
			Completed:             map[string]struct{}{},
			CompletedPullRequests: map[int]struct{}{},
			StartedAt:             time.Now().UTC(),
			PollInterval:          snapshot.Config.Polling.Interval,
			MaxConcurrentAgents:   snapshot.Config.Agent.MaxConcurrentAgents,
		},
	}, nil
}

// Run starts the poll loop and blocks until ctx is canceled or a startup error
// occurs.
func (o *Orchestrator) Run(ctx context.Context) error {
	snapshot, ok := o.manager.Current()
	if !ok {
		return fmt.Errorf("symphony run: workflow has not been loaded")
	}

	o.applyConfig(snapshot.Config)
	o.startupCleanup(ctx, snapshot.Config)

	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			o.recordEvent("shutdown", "context canceled; shutting down workers")
			o.shutdown()
			return nil
		case <-timer.C:
			now := time.Now().UTC()
			o.state.LastTickAt = now
			o.tick(ctx)
			o.state.NextTickAt = time.Now().UTC().Add(o.state.PollInterval)
			timer.Reset(o.state.PollInterval)
		case event := <-o.events:
			o.handleEvent(ctx, event)
		}
	}
}

func (o *Orchestrator) tick(ctx context.Context) {
	snapshot, reloaded, err := o.manager.ReloadIfChanged(ctx)
	if err != nil {
		o.logger.Error("symphony workflow reload failed; keeping last known good config", "error", err)
		if current, ok := o.manager.Current(); ok {
			snapshot = current
		}
	}

	if reloaded {
		o.recordEvent("workflow_reloaded", "workflow file reloaded")
		o.logger.Info("symphony workflow reloaded", "workflow_path", snapshot.Config.WorkflowPath)
	}

	o.applyConfig(snapshot.Config)
	o.reconcile(ctx, snapshot.Config)

	if err := snapshot.Config.ValidatePreflight(); err != nil {
		o.recordEvent("dispatch_validation_failed", err.Error())
		o.logger.Error("symphony dispatch validation failed", "error", err)
		return
	}

	o.discoverPullRequestMonitors(ctx, snapshot.Config)
	o.recoverPullRequestMonitorTimers(snapshot.Config)

	issues, err := o.tracker.FetchCandidateIssues(ctx)
	if err != nil {
		o.recordEvent("candidate_fetch_failed", err.Error())
		o.logger.Error("symphony candidate fetch failed", "error", err)
		return
	}

	o.recordEvent(
		"poll_completed", "candidate issue poll completed",
		"candidate_count", len(issues),
		"available_slots", o.availableSlots(snapshot.Config),
	)

	sortIssuesForDispatch(issues)
	for _, issue := range issues {
		if !o.canDispatch(issue, snapshot.Config) {
			continue
		}

		o.dispatch(ctx, snapshot, issue, nil, nil)
		if o.availableSlots(snapshot.Config) == 0 {
			break
		}
	}
}

func (o *Orchestrator) applyConfig(cfg Config) {
	o.state.PollInterval = cfg.Polling.Interval
	o.state.MaxConcurrentAgents = cfg.Agent.MaxConcurrentAgents
}

func (o *Orchestrator) startupCleanup(ctx context.Context, cfg Config) {
	issues, err := o.tracker.FetchIssuesByStates(ctx, cfg.Tracker.TerminalStates)
	if err != nil {
		o.logger.Warn("symphony startup terminal cleanup fetch failed", "error", err)
		return
	}

	for _, issue := range issues {
		if err := o.workspaces.Remove(ctx, cfg, issue); err != nil {
			o.logger.Warn(
				"symphony startup terminal cleanup failed",
				"issue_id", issue.ID,
				"issue_identifier", issue.Identifier,
				"error", err,
			)
		}
	}
}

func (o *Orchestrator) reconcile(ctx context.Context, cfg Config) {
	now := time.Now().UTC()
	var ids []string
	for issueID, entry := range o.state.Running {
		if cfg.Codex.StallTimeout > 0 {
			since := entry.StartedAt
			if !entry.LastCodexTime.IsZero() {
				since = entry.LastCodexTime
			}

			if now.Sub(since) > cfg.Codex.StallTimeout {
				entry.CancelReason = cancelStalled
				entry.Cancel()
				o.logger.Warn(
					"symphony worker stalled; canceling",
					"issue_id", entry.Issue.ID,
					"issue_identifier", entry.Issue.Identifier,
					"elapsed_ms", now.Sub(since).Milliseconds(),
				)
				continue
			}
		}

		ids = append(ids, issueID)
	}

	if len(ids) == 0 {
		return
	}

	refreshed, err := o.tracker.FetchIssueStatesByIDs(ctx, ids)
	if err != nil {
		o.logger.Warn("symphony running state refresh failed", "error", err)
		return
	}

	byID := make(map[string]Issue, len(refreshed))
	for _, issue := range refreshed {
		byID[issue.ID] = issue
	}

	for issueID, entry := range o.state.Running {
		issue, ok := byID[issueID]
		if !ok {
			continue
		}

		entry.Issue = issue
		entry.State = issue.State
		switch {
		case isTerminalState(issue.State, cfg):
			entry.CancelReason = cancelTerminal
			entry.Cancel()
			if err := o.workspaces.Remove(ctx, cfg, issue); err != nil {
				o.logger.Warn(
					"symphony terminal workspace cleanup failed",
					"issue_id", issue.ID,
					"issue_identifier", issue.Identifier,
					"error", err,
				)
			}
		case isActiveState(issue.State, cfg):
			continue
		default:
			entry.CancelReason = cancelNonActive
			entry.Cancel()
		}
	}
}

func (o *Orchestrator) discoverPullRequestMonitors(ctx context.Context, cfg Config) {
	if !cfg.Publish.Enabled || !cfg.Publish.MonitorChecks || normalizeState(cfg.Tracker.Kind) != trackerKindGitHub {
		return
	}
	o.ensurePullRequestState()

	prClient, ok := o.tracker.(interface {
		FetchOpenPullRequestsByHeadPrefix(context.Context, string) ([]MonitoredPullRequest, error)
	})
	if !ok {
		return
	}

	pullRequests, err := prClient.FetchOpenPullRequestsByHeadPrefix(ctx, cfg.Publish.BranchPrefix)
	if err != nil {
		o.recordEvent("pr_discovery_failed", "pull request monitor discovery failed", "error", err.Error())
		o.logger.Warn("symphony pull request monitor discovery failed", "error", err)
		return
	}

	for _, pr := range pullRequests {
		number := pr.PullRequest.Number
		if number <= 0 {
			continue
		}
		if _, completed := o.state.CompletedPullRequests[number]; completed {
			continue
		}

		monitor := o.state.PullRequests[number]
		if monitor == nil {
			monitor = &pullRequestMonitorEntry{Number: number}
			o.state.PullRequests[number] = monitor
			monitor.Issue = pr.Issue
			monitor.Branch = firstNonEmpty(pr.Branch, pr.PullRequest.Head.Ref)
			monitor.PullRequestURL = pr.PullRequest.HTMLURL
			o.reschedulePullRequestCheck(monitor, minPositiveDuration(cfg.Publish.CheckInterval, time.Second), "")
			o.recordIssueEvent(
				"pr_monitor_discovered", pr.Issue, "open Symphony pull request monitor discovered",
				"pull_request_number", number,
				"pull_request_url", pr.PullRequest.HTMLURL,
				"branch", monitor.Branch,
			)
			continue
		}

		monitor.Issue = pr.Issue
		monitor.Branch = firstNonEmpty(pr.Branch, pr.PullRequest.Head.Ref, monitor.Branch)
		monitor.PullRequestURL = firstNonEmpty(pr.PullRequest.HTMLURL, monitor.PullRequestURL)
		if monitor.Timer == nil && !monitor.InRework && !monitor.Exhausted && monitor.NextCheckAt.IsZero() {
			o.reschedulePullRequestCheck(monitor, cfg.Publish.CheckInterval, "")
		}
	}
}

func (o *Orchestrator) recoverPullRequestMonitorTimers(cfg Config) {
	if !cfg.Publish.Enabled || !cfg.Publish.MonitorChecks {
		return
	}

	o.ensurePullRequestState()
	now := time.Now().UTC()
	for _, monitor := range o.state.PullRequests {
		if monitor == nil || monitor.Number <= 0 || monitor.Timer != nil || monitor.InRework || monitor.Exhausted {
			continue
		}

		delay := time.Millisecond
		if !monitor.NextCheckAt.IsZero() && monitor.NextCheckAt.After(now) {
			delay = monitor.NextCheckAt.Sub(now)
		}

		o.reschedulePullRequestCheck(monitor, delay, "monitor timer recovered")
		o.recordIssueEvent(
			"pr_monitor_timer_recovered", monitor.Issue, "pull request monitor timer recovered",
			"pull_request_number", monitor.Number,
			"pull_request_url", monitor.PullRequestURL,
			"next_check_at", monitor.NextCheckAt,
		)
	}
}

func (o *Orchestrator) canDispatch(issue Issue, cfg Config) bool {
	if strings.TrimSpace(issue.ID) == "" || strings.TrimSpace(issue.Identifier) == "" || strings.TrimSpace(issue.Title) == "" || strings.TrimSpace(issue.State) == "" {
		return false
	}

	if !isActiveState(issue.State, cfg) || isTerminalState(issue.State, cfg) {
		return false
	}

	if _, ok := o.state.Running[issue.ID]; ok {
		return false
	}

	if _, ok := o.state.Claimed[issue.ID]; ok {
		return false
	}

	if o.availableSlots(cfg) <= 0 {
		return false
	}

	if !o.hasStateSlot(issue.State, cfg) {
		return false
	}

	if normalizeState(issue.State) == "todo" && hasNonTerminalBlocker(issue, cfg) {
		return false
	}

	return true
}

func (o *Orchestrator) dispatch(ctx context.Context, snapshot WorkflowSnapshot, issue Issue, attempt *int, runContext *RunContext) {
	runCtx, cancel := context.WithCancel(ctx)
	attemptValue := 0
	if attempt != nil {
		attemptValue = *attempt
	}

	runKind := RunKindIssue
	var pullRequest *PullRequestReworkContext
	if runContext != nil && runContext.Kind != "" {
		runKind = runContext.Kind
		pullRequest = runContext.PullRequest
	}

	entry := &runningEntry{
		Issue:       issue,
		Cancel:      cancel,
		StartedAt:   time.Now().UTC(),
		State:       issue.State,
		Attempt:     attemptValue,
		RunKind:     runKind,
		PullRequest: pullRequest,
	}

	o.state.Running[issue.ID] = entry
	o.state.Claimed[issue.ID] = struct{}{}

	o.logger.Info(
		"symphony dispatching issue",
		"issue_id", issue.ID,
		"issue_identifier", issue.Identifier,
		"state", issue.State,
		"attempt", attemptValue,
		"run_kind", runKind,
	)
	o.recordIssueEvent(
		"issue_dispatched", issue, "worker dispatched",
		"state", issue.State,
		"attempt", attemptValue,
		"run_kind", runKind,
	)

	req := RunRequest{
		Config:   snapshot.Config,
		Workflow: snapshot.Definition,
		Issue:    issue,
		Attempt:  attempt,
		Context:  runContext,
	}

	o.wg.Add(1)
	go func() {
		defer o.wg.Done()
		result, err := o.runner.Run(runCtx, req, func(event CodexEvent) {
			o.enqueueEvent(ctx, codexUpdateEvent{issueID: issue.ID, event: event})
		})

		o.enqueueEvent(ctx, workerExitEvent{issueID: issue.ID, result: result, err: err})
	}()
}

func (o *Orchestrator) enqueueEvent(ctx context.Context, event orchestratorEvent) {
	select {
	case o.events <- event:
		return
	case <-ctx.Done():
	}

	select {
	case o.events <- event:
	default:
	}
}

func (o *Orchestrator) handleEvent(ctx context.Context, event orchestratorEvent) {
	switch typed := event.(type) {
	case codexUpdateEvent:
		o.handleCodexUpdate(typed)
	case workerExitEvent:
		o.handleWorkerExit(typed)
	case retryDueEvent:
		o.handleRetryDue(ctx, typed.issueID)
	case pullRequestCheckDueEvent:
		o.handlePullRequestCheckDue(ctx, typed.number)
	case debugSnapshotEvent:
		o.handleDebugSnapshot(typed)
	}
}

func (o *Orchestrator) handleCodexUpdate(event codexUpdateEvent) {
	entry, ok := o.state.Running[event.issueID]
	if !ok {
		return
	}

	update := event.event
	if update.Timestamp.IsZero() {
		update.Timestamp = time.Now().UTC()
	}

	entry.LastCodexTime = update.Timestamp
	entry.LastCodexEvent = update.Event
	entry.LastMessage = update.Message
	entry.AppServerPID = firstNonEmpty(update.AppServerPID, entry.AppServerPID)
	entry.ThreadID = firstNonEmpty(update.ThreadID, entry.ThreadID)
	entry.TurnID = firstNonEmpty(update.TurnID, entry.TurnID)
	entry.SessionID = firstNonEmpty(update.SessionID, entry.SessionID)

	if update.Usage != nil {
		prevInput := entry.InputTokens
		prevOutput := entry.OutputTokens
		prevTotal := entry.TotalTokens
		entry.InputTokens = update.Usage.InputTokens
		entry.OutputTokens = update.Usage.OutputTokens
		entry.TotalTokens = update.Usage.TotalTokens
		o.state.CodexTotals.InputTokens += maxInt64(update.Usage.InputTokens-prevInput, 0)
		o.state.CodexTotals.OutputTokens += maxInt64(update.Usage.OutputTokens-prevOutput, 0)
		o.state.CodexTotals.TotalTokens += maxInt64(update.Usage.TotalTokens-prevTotal, 0)
	}

	if len(update.RateLimits) > 0 {
		o.state.CodexRateLimits = append(o.state.CodexRateLimits[:0], update.RateLimits...)
	}

	o.logCodexUpdate(entry, update)
	o.recordIssueEvent(
		"codex_event", entry.Issue, "codex event received",
		"event", update.Event,
		"message", update.Message,
		"session_id", entry.SessionID,
		"thread_id", entry.ThreadID,
		"turn_id", entry.TurnID,
	)
}

func (o *Orchestrator) logCodexUpdate(entry *runningEntry, update CodexEvent) {
	attrs := []any{
		"issue_id", entry.Issue.ID,
		"issue_identifier", entry.Issue.Identifier,
		"session_id", entry.SessionID,
		"thread_id", entry.ThreadID,
		"turn_id", entry.TurnID,
		"event", update.Event,
	}
	if update.Message != "" {
		attrs = append(attrs, "message", update.Message)
	}

	if update.TotalTokens > 0 {
		attrs = append(
			attrs,
			"input_tokens", update.InputTokens,
			"output_tokens", update.OutputTokens,
			"total_tokens", update.TotalTokens,
		)
	}

	switch update.Event {
	case "session_started":
		o.logger.Info("symphony codex session started", attrs...)
	case "turn_started":
		o.logger.Info("symphony codex turn started", attrs...)
	case "turn_completed":
		o.logger.Info("symphony codex turn completed", attrs...)
	case "turn_failed", "turn_input_required", "unsupported_tool_call":
		o.logger.Warn("symphony codex turn needs attention", attrs...)
	default:
		o.logger.Debug("symphony codex event", attrs...)
	}
}

func (o *Orchestrator) handleWorkerExit(event workerExitEvent) {
	entry, ok := o.state.Running[event.issueID]
	if !ok {
		return
	}

	delete(o.state.Running, event.issueID)
	o.state.CodexTotals.RuntimeSeconds += int64(time.Since(entry.StartedAt).Seconds())
	if entry.PullRequest != nil {
		o.markPullRequestReworkFinished(entry.PullRequest.Number)
	}

	if o.releaseCanceledWorker(event.issueID, entry) {
		return
	}

	if entry.CancelReason == cancelStalled {
		if entry.RunKind == RunKindPullRequestRework {
			delete(o.state.Claimed, event.issueID)
			o.recordIssueEvent(
				"pr_rework_stalled", entry.Issue, "pull request rework stalled; check retry scheduled",
				"pull_request_number", pullRequestNumber(entry),
			)
			o.schedulePullRequestCheckRetry(entry.PullRequest, "rework stalled")
			return
		}

		o.recordIssueEvent("worker_stalled", entry.Issue, "worker stalled; retry scheduled")
		o.scheduleRetry(event.issueID, entry.Issue.Identifier, nextAttempt(entry.Attempt), "stalled", o.currentRetryBackoffCap())
		return
	}

	if event.err == nil && event.result.Status == AttemptSucceeded {
		o.state.Completed[event.issueID] = struct{}{}
		delete(o.state.Claimed, event.issueID)
		if event.result.Publish != nil && event.result.Publish.Published {
			o.schedulePullRequestMonitor(entry.Issue, event.result.Publish, pullRequestReworkAttempt(entry), "worker published pull request")
			o.recordIssueEvent(
				"worker_published", entry.Issue, "worker published pull request and finalized issue",
				"branch", event.result.Publish.Branch,
				"pull_request_url", event.result.Publish.PullRequestURL,
				"pull_request_number", event.result.Publish.PullRequestNumber,
			)
			o.logger.Info(
				"symphony worker published pull request; issue finalized",
				"issue_id", entry.Issue.ID,
				"issue_identifier", entry.Issue.Identifier,
				"branch", event.result.Publish.Branch,
				"pull_request_url", event.result.Publish.PullRequestURL,
			)
			return
		}

		if entry.RunKind == RunKindPullRequestRework {
			o.schedulePullRequestCheckRetry(entry.PullRequest, "rework finished without a publish result")
			return
		}

		o.scheduleRetry(event.issueID, entry.Issue.Identifier, 1, "", continuationRetryDelay)
		o.recordIssueEvent("worker_completed", entry.Issue, "worker completed; continuation retry queued")
		o.logger.Info(
			"symphony worker completed; continuation retry queued",
			"issue_id", entry.Issue.ID,
			"issue_identifier", entry.Issue.Identifier,
		)
		return
	}

	errText := ""
	if event.err != nil {
		errText = event.err.Error()
	} else {
		errText = event.result.Error
	}

	if entry.RunKind == RunKindPullRequestRework {
		delete(o.state.Claimed, event.issueID)
		o.recordIssueEvent(
			"pr_rework_failed", entry.Issue, "pull request rework failed; check retry scheduled",
			"pull_request_number", pullRequestNumber(entry),
			"error", errText,
		)
		o.logger.Warn(
			"symphony pull request rework failed; check retry scheduled",
			"issue_id", entry.Issue.ID,
			"issue_identifier", entry.Issue.Identifier,
			"pull_request_number", pullRequestNumber(entry),
			"error", errText,
		)
		o.schedulePullRequestCheckRetry(entry.PullRequest, errText)
		return
	}

	o.scheduleRetry(event.issueID, entry.Issue.Identifier, nextAttempt(entry.Attempt), errText, o.currentRetryBackoffCap())
	o.recordIssueEvent(
		"worker_failed", entry.Issue, "worker failed; retry queued",
		"error", errText,
	)
	o.logger.Warn(
		"symphony worker failed; retry queued",
		"issue_id", entry.Issue.ID,
		"issue_identifier", entry.Issue.Identifier,
		"error", errText,
	)
}

func (o *Orchestrator) releaseCanceledWorker(issueID string, entry *runningEntry) bool {
	if entry.CancelReason != cancelTerminal && entry.CancelReason != cancelNonActive && entry.CancelReason != cancelShutdown {
		return false
	}

	delete(o.state.Claimed, issueID)
	if entry.RunKind == RunKindPullRequestRework && entry.CancelReason != cancelShutdown {
		o.schedulePullRequestCheckRetry(entry.PullRequest, "rework canceled because issue became "+string(entry.CancelReason))
	}

	o.recordIssueEvent(
		"worker_released", entry.Issue, "worker released",
		"reason", string(entry.CancelReason),
	)
	o.logger.Info(
		"symphony worker released",
		"issue_id", entry.Issue.ID,
		"issue_identifier", entry.Issue.Identifier,
		"reason", entry.CancelReason,
	)

	return true
}

func (o *Orchestrator) handleRetryDue(ctx context.Context, issueID string) {
	retry, ok := o.state.RetryAttempts[issueID]
	if !ok {
		return
	}

	delete(o.state.RetryAttempts, issueID)
	o.recordEvent(
		"retry_due", "retry became due",
		"issue_id", issueID,
		"issue_identifier", retry.Identifier,
		"attempt", retry.Attempt,
	)

	snapshot, ok := o.manager.Current()
	if !ok {
		delete(o.state.Claimed, issueID)
		o.recordEvent(
			"retry_released", "retry released because workflow snapshot is unavailable",
			"issue_id", issueID,
			"issue_identifier", retry.Identifier,
		)
		return
	}

	issues, err := o.tracker.FetchCandidateIssues(ctx)
	if err != nil {
		o.scheduleRetry(issueID, retry.Identifier, retry.Attempt+1, "retry poll failed: "+err.Error(), snapshot.Config.Agent.MaxRetryBackoff)
		o.recordEvent(
			"retry_poll_failed", "retry candidate poll failed",
			"issue_id", issueID,
			"issue_identifier", retry.Identifier,
			"error", err.Error(),
		)
		return
	}

	var found *Issue
	for i := range issues {
		if issues[i].ID == issueID {
			found = &issues[i]
			break
		}
	}

	if found == nil {
		delete(o.state.Claimed, issueID)
		o.recordEvent(
			"retry_released", "retry released because issue is no longer a candidate",
			"issue_id", issueID,
			"issue_identifier", retry.Identifier,
		)
		return
	}

	delete(o.state.Claimed, issueID)
	if !o.canDispatch(*found, snapshot.Config) {
		o.state.Claimed[issueID] = struct{}{}
		o.scheduleRetry(issueID, found.Identifier, retry.Attempt+1, "no available orchestrator slots", snapshot.Config.Agent.MaxRetryBackoff)
		o.recordIssueEvent(
			"retry_deferred", *found, "retry deferred; no available orchestrator slots",
			"attempt", retry.Attempt+1,
		)
		return
	}

	attempt := retry.Attempt
	o.dispatch(ctx, snapshot, *found, &attempt, nil)
}

func (o *Orchestrator) handlePullRequestCheckDue(ctx context.Context, number int) {
	monitor, ok := o.state.PullRequests[number]
	if !ok {
		return
	}

	monitor.Timer = nil
	monitor.NextCheckAt = time.Time{}
	monitor.LastError = ""

	snapshot, ok := o.manager.Current()
	if !ok {
		o.reschedulePullRequestCheck(monitor, defaultPRCheckInterval, "workflow snapshot is unavailable")
		return
	}

	cfg := snapshot.Config
	if !cfg.Publish.Enabled || !cfg.Publish.MonitorChecks {
		delete(o.state.PullRequests, number)
		o.recordIssueEvent(
			"pr_monitor_disabled", monitor.Issue, "pull request monitor disabled",
			"pull_request_number", number,
		)
		return
	}

	checkClient, ok := o.tracker.(interface {
		FetchPullRequestChecks(context.Context, int) (PullRequestCheckSnapshot, error)
	})
	if !ok {
		monitor.Exhausted = true
		monitor.LastError = "tracker does not support pull request checks"
		o.recordIssueEvent(
			"pr_monitor_failed", monitor.Issue, monitor.LastError,
			"pull_request_number", number,
		)
		return
	}

	checks, err := checkClient.FetchPullRequestChecks(ctx, number)
	if err != nil {
		o.reschedulePullRequestCheck(monitor, cfg.Publish.CheckInterval, err.Error())
		o.recordIssueEvent(
			"pr_check_failed", monitor.Issue, "pull request check polling failed",
			"pull_request_number", number,
			"error", err.Error(),
		)
		return
	}

	monitor.LastSnapshot = checks
	monitor.LastCheckAt = checks.CheckedAt
	monitor.PullRequestURL = firstNonEmpty(checks.PullRequestURL, monitor.PullRequestURL)
	monitor.Branch = firstNonEmpty(checks.HeadRef, monitor.Branch)
	o.clearStalePendingRework(monitor, checks)

	o.recordIssueEvent(
		"pr_checks_polled", monitor.Issue, "pull request checks polled",
		"pull_request_number", number,
		"state", checks.State,
		"summary", checks.Summary,
		"needs_branch_update", checks.NeedsBranchUpdate,
		"branch_update_reason", checks.BranchUpdateReason,
	)

	if checks.NeedsBranchUpdate {
		o.handlePullRequestBranchUpdate(ctx, snapshot, monitor, checks)
		return
	}

	switch checks.State {
	case PullRequestChecksPassed:
		delete(o.state.PullRequests, number)
		o.ensurePullRequestState()
		o.state.CompletedPullRequests[number] = struct{}{}
		o.recordIssueEvent(
			"pr_checks_passed", monitor.Issue, "pull request checks passed; monitor complete",
			"pull_request_number", number,
			"pull_request_url", monitor.PullRequestURL,
		)
		o.logger.Info(
			"symphony pull request checks passed; monitor complete",
			"issue_id", monitor.Issue.ID,
			"issue_identifier", monitor.Issue.Identifier,
			"pull_request_number", number,
			"pull_request_url", monitor.PullRequestURL,
		)
	case PullRequestChecksFailed:
		o.handleFailedPullRequestChecks(ctx, snapshot, monitor, checks)
	default:
		o.reschedulePullRequestCheck(monitor, cfg.Publish.CheckInterval, "")
	}
}

func (o *Orchestrator) handlePullRequestBranchUpdate(ctx context.Context, snapshot WorkflowSnapshot, monitor *pullRequestMonitorEntry, checks PullRequestCheckSnapshot) {
	branch := firstNonEmpty(checks.HeadRef, monitor.Branch)
	if strings.TrimSpace(branch) == "" {
		o.reschedulePullRequestCheck(monitor, snapshot.Config.Publish.CheckInterval, "pull request head branch is not available yet")
		return
	}

	if monitor.InRework {
		o.reschedulePullRequestCheck(monitor, snapshot.Config.Publish.CheckInterval, "rework already running")
		return
	}

	if monitor.PendingRework != nil && monitor.PendingReworkKey == pullRequestPendingReworkKey(checks) {
		o.recordIssueEvent(
			"pr_branch_update_rework_pending", monitor.Issue, "pull request branch update already failed; waiting for rework capacity",
			"pull_request_number", monitor.Number,
			"branch", branch,
			"reason", monitor.PendingRework.Summary,
		)
		o.handleFailedPullRequestChecks(ctx, snapshot, monitor, *monitor.PendingRework)
		return
	}

	update := o.updatePullRequestBranch
	if update == nil {
		update = UpdatePullRequestBranch
	}

	o.recordIssueEvent(
		"pr_branch_update_needed", monitor.Issue, "pull request branch update needed",
		"pull_request_number", monitor.Number,
		"branch", branch,
		"reason", checks.BranchUpdateReason,
	)
	o.logger.Info(
		"symphony pull request branch update needed",
		"issue_id", monitor.Issue.ID,
		"issue_identifier", monitor.Issue.Identifier,
		"pull_request_number", monitor.Number,
		"branch", branch,
		"reason", checks.BranchUpdateReason,
	)

	commitSHA, err := update(ctx, snapshot.Config, monitor.Issue, branch, o.logger)
	if err != nil {
		failed := checks
		failed.State = PullRequestChecksFailed
		failed.Summary = "branch update failed: " + err.Error()
		failed.FailedCheckNames = []string{"branch update"}
		monitor.PendingRework = clonePullRequestCheckSnapshot(failed)
		monitor.PendingReworkKey = pullRequestPendingReworkKey(checks)
		o.recordIssueEvent(
			"pr_branch_update_failed", monitor.Issue, "pull request branch update failed; rework will be attempted",
			"pull_request_number", monitor.Number,
			"branch", branch,
			"error", err.Error(),
		)
		o.logger.Warn(
			"symphony pull request branch update failed; rework will be attempted",
			"issue_id", monitor.Issue.ID,
			"issue_identifier", monitor.Issue.Identifier,
			"pull_request_number", monitor.Number,
			"branch", branch,
			"error", err,
		)
		o.handleFailedPullRequestChecks(ctx, snapshot, monitor, failed)
		return
	}

	monitor.Branch = branch
	monitor.PendingRework = nil
	monitor.PendingReworkKey = ""
	o.reschedulePullRequestCheck(monitor, snapshot.Config.Publish.CheckInterval, "branch update pushed; waiting for fresh checks")
	o.recordIssueEvent(
		"pr_branch_updated", monitor.Issue, "pull request branch updated",
		"pull_request_number", monitor.Number,
		"branch", branch,
		"head_sha", commitSHA,
	)
	o.logger.Info(
		"symphony pull request branch updated",
		"issue_id", monitor.Issue.ID,
		"issue_identifier", monitor.Issue.Identifier,
		"pull_request_number", monitor.Number,
		"branch", branch,
		"head_sha", commitSHA,
	)
}

func (o *Orchestrator) clearStalePendingRework(monitor *pullRequestMonitorEntry, checks PullRequestCheckSnapshot) {
	if monitor == nil || monitor.PendingRework == nil {
		return
	}

	if monitor.PendingReworkKey == pullRequestPendingReworkKey(checks) {
		return
	}

	monitor.PendingRework = nil
	monitor.PendingReworkKey = ""
}

func pullRequestPendingReworkKey(checks PullRequestCheckSnapshot) string {
	return strings.Join([]string{
		checks.HeadSHA,
		checks.BaseSHA,
		checks.BranchUpdateReason,
	}, "\x00")
}

func clonePullRequestCheckSnapshot(snapshot PullRequestCheckSnapshot) *PullRequestCheckSnapshot {
	clone := snapshot
	clone.FailedCheckNames = append([]string(nil), snapshot.FailedCheckNames...)
	clone.CheckRuns = append([]PullRequestCheckRun(nil), snapshot.CheckRuns...)
	clone.StatusContexts = append([]PullRequestStatus(nil), snapshot.StatusContexts...)
	return &clone
}

func (o *Orchestrator) handleFailedPullRequestChecks(ctx context.Context, snapshot WorkflowSnapshot, monitor *pullRequestMonitorEntry, checks PullRequestCheckSnapshot) {
	cfg := snapshot.Config
	if monitor.ReworkAttempts >= cfg.Publish.MaxCheckReworkAttempts {
		monitor.Exhausted = true
		monitor.LastError = fmt.Sprintf("max PR rework attempts reached (%d)", cfg.Publish.MaxCheckReworkAttempts)
		o.recordIssueEvent(
			"pr_checks_failed_exhausted", monitor.Issue, "pull request checks still fail; max rework attempts reached",
			"pull_request_number", monitor.Number,
			"rework_attempts", monitor.ReworkAttempts,
			"summary", checks.Summary,
		)
		o.logger.Warn(
			"symphony pull request checks failed; max rework attempts reached",
			"issue_id", monitor.Issue.ID,
			"issue_identifier", monitor.Issue.Identifier,
			"pull_request_number", monitor.Number,
			"rework_attempts", monitor.ReworkAttempts,
			"summary", checks.Summary,
		)
		return
	}

	if monitor.InRework {
		o.reschedulePullRequestCheck(monitor, cfg.Publish.CheckInterval, "rework already running")
		return
	}

	if _, running := o.state.Running[monitor.Issue.ID]; running || o.availableSlots(cfg) <= 0 {
		o.reschedulePullRequestCheck(monitor, cfg.Publish.CheckInterval, "no available orchestrator slots")
		o.recordIssueEvent(
			"pr_rework_deferred", monitor.Issue, "pull request rework deferred; no available slots",
			"pull_request_number", monitor.Number,
		)
		return
	}

	monitor.ReworkAttempts++
	monitor.InRework = true
	monitor.LastReworkAt = time.Now().UTC()
	attempt := monitor.ReworkAttempts
	runContext := &RunContext{
		Kind: RunKindPullRequestRework,
		PullRequest: &PullRequestReworkContext{
			URL:           monitor.PullRequestURL,
			Branch:        monitor.Branch,
			HeadSHA:       checks.HeadSHA,
			Summary:       checks.Summary,
			FailedChecks:  append([]string(nil), checks.FailedCheckNames...),
			Number:        monitor.Number,
			ReworkAttempt: attempt,
		},
	}

	o.recordIssueEvent(
		"pr_rework_dispatched", monitor.Issue, "pull request rework dispatched",
		"pull_request_number", monitor.Number,
		"rework_attempt", attempt,
		"summary", checks.Summary,
	)
	o.logger.Info(
		"symphony dispatching pull request rework",
		"issue_id", monitor.Issue.ID,
		"issue_identifier", monitor.Issue.Identifier,
		"pull_request_number", monitor.Number,
		"rework_attempt", attempt,
		"summary", checks.Summary,
	)

	o.dispatch(ctx, snapshot, monitor.Issue, &attempt, runContext)
}

func (o *Orchestrator) schedulePullRequestMonitor(issue Issue, result *PublishResult, reworkAttempts int, reason string) {
	if result == nil || result.PullRequestNumber <= 0 {
		return
	}
	o.ensurePullRequestState()

	snapshot, ok := o.manager.Current()
	if !ok || !snapshot.Config.Publish.MonitorChecks {
		return
	}

	monitor := o.state.PullRequests[result.PullRequestNumber]
	if monitor == nil {
		monitor = &pullRequestMonitorEntry{Number: result.PullRequestNumber}
		o.state.PullRequests[result.PullRequestNumber] = monitor
	}
	delete(o.state.CompletedPullRequests, result.PullRequestNumber)

	if monitor.Timer != nil {
		monitor.Timer.Stop()
	}

	monitor.Issue = issue
	monitor.Branch = firstNonEmpty(result.Branch, monitor.Branch)
	monitor.PullRequestURL = firstNonEmpty(result.PullRequestURL, monitor.PullRequestURL)
	monitor.ReworkAttempts = max(monitor.ReworkAttempts, reworkAttempts)
	monitor.Exhausted = false
	monitor.InRework = false
	monitor.PendingRework = nil
	monitor.PendingReworkKey = ""
	o.reschedulePullRequestCheck(monitor, snapshot.Config.Publish.CheckInterval, "")
	o.recordIssueEvent(
		"pr_monitor_scheduled", issue, "pull request monitor scheduled",
		"pull_request_number", result.PullRequestNumber,
		"pull_request_url", result.PullRequestURL,
		"reason", reason,
		"next_check_at", monitor.NextCheckAt,
	)
}

func (o *Orchestrator) schedulePullRequestCheckRetry(runContext *PullRequestReworkContext, reason string) {
	if runContext == nil || runContext.Number <= 0 {
		return
	}

	monitor := o.state.PullRequests[runContext.Number]
	if monitor == nil {
		return
	}

	delay := defaultPRCheckInterval
	if snapshot, ok := o.manager.Current(); ok && snapshot.Config.Publish.CheckInterval > 0 {
		delay = snapshot.Config.Publish.CheckInterval
	}

	o.reschedulePullRequestCheck(monitor, delay, reason)
}

func (o *Orchestrator) reschedulePullRequestCheck(monitor *pullRequestMonitorEntry, delay time.Duration, reason string) {
	if monitor == nil || monitor.Number <= 0 {
		return
	}

	if delay <= 0 {
		delay = defaultPRCheckInterval
	}

	if monitor.Timer != nil {
		monitor.Timer.Stop()
	}

	monitor.LastError = strings.TrimSpace(reason)
	monitor.Exhausted = false
	monitor.NextCheckAt = time.Now().UTC().Add(delay)
	number := monitor.Number
	monitor.Timer = time.AfterFunc(delay, func() {
		o.events <- pullRequestCheckDueEvent{number: number}
	})
}

func (o *Orchestrator) markPullRequestReworkFinished(number int) {
	if number <= 0 {
		return
	}

	if monitor := o.state.PullRequests[number]; monitor != nil {
		monitor.InRework = false
	}
}

func (o *Orchestrator) ensurePullRequestState() {
	if o.state.PullRequests == nil {
		o.state.PullRequests = map[int]*pullRequestMonitorEntry{}
	}
	if o.state.CompletedPullRequests == nil {
		o.state.CompletedPullRequests = map[int]struct{}{}
	}
}

func pullRequestNumber(entry *runningEntry) int {
	if entry == nil || entry.PullRequest == nil {
		return 0
	}

	return entry.PullRequest.Number
}

func pullRequestReworkAttempt(entry *runningEntry) int {
	if entry == nil || entry.PullRequest == nil {
		return 0
	}

	return entry.PullRequest.ReworkAttempt
}

func (o *Orchestrator) scheduleRetry(issueID, identifier string, attempt int, errorText string, delayOrCap time.Duration) {
	if existing := o.state.RetryAttempts[issueID]; existing != nil && existing.Timer != nil {
		existing.Timer.Stop()
	}

	delay := delayOrCap
	if delay != continuationRetryDelay {
		delay = retryBackoff(attempt, delayOrCap)
	}

	dueAt := time.Now().Add(delay)
	timer := time.AfterFunc(delay, func() {
		o.events <- retryDueEvent{issueID: issueID}
	})

	o.state.RetryAttempts[issueID] = &RetryEntry{
		IssueID:    issueID,
		Identifier: identifier,
		Attempt:    attempt,
		Error:      errorText,
		DueAt:      dueAt,
		Timer:      timer,
	}
	o.state.Claimed[issueID] = struct{}{}
}

func (o *Orchestrator) currentRetryBackoffCap() time.Duration {
	if snapshot, ok := o.manager.Current(); ok && snapshot.Config.Agent.MaxRetryBackoff > 0 {
		return snapshot.Config.Agent.MaxRetryBackoff
	}

	return defaultMaxRetryBackoff
}

func (o *Orchestrator) availableSlots(cfg Config) int {
	return max(cfg.Agent.MaxConcurrentAgents-len(o.state.Running), 0)
}

func (o *Orchestrator) hasStateSlot(state string, cfg Config) bool {
	key := normalizeState(state)
	limit := cfg.Agent.MaxConcurrentAgents
	if cfg.Agent.MaxConcurrentAgentsByState != nil {
		if stateLimit, ok := cfg.Agent.MaxConcurrentAgentsByState[key]; ok {
			limit = stateLimit
		}
	}

	count := 0
	for _, entry := range o.state.Running {
		if normalizeState(entry.State) == key {
			count++
		}
	}

	return count < limit
}

func (o *Orchestrator) shutdown() {
	for _, retry := range o.state.RetryAttempts {
		if retry.Timer != nil {
			retry.Timer.Stop()
		}
	}

	for _, monitor := range o.state.PullRequests {
		if monitor.Timer != nil {
			monitor.Timer.Stop()
		}
	}

	for _, entry := range o.state.Running {
		entry.CancelReason = cancelShutdown
		entry.Cancel()
	}

	o.wg.Wait()
}

func sortIssuesForDispatch(issues []Issue) {
	sort.SliceStable(issues, func(i, j int) bool {
		left := issues[i]
		right := issues[j]

		leftPriority, rightPriority := prioritySortValue(left), prioritySortValue(right)
		if leftPriority != rightPriority {
			return leftPriority < rightPriority
		}

		if !sameTimePointer(left.CreatedAt, right.CreatedAt) {
			return timeSortValue(left.CreatedAt).Before(timeSortValue(right.CreatedAt))
		}

		return left.Identifier < right.Identifier
	})
}

func prioritySortValue(issue Issue) int {
	if issue.Priority == nil {
		return 1_000_000
	}

	return *issue.Priority
}

func timeSortValue(value *time.Time) time.Time {
	if value == nil {
		return time.Unix(1<<62, 0)
	}

	return *value
}

func sameTimePointer(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}

	return a.Equal(*b)
}

func retryBackoff(attempt int, maxDelay time.Duration) time.Duration {
	if attempt <= 0 {
		attempt = 1
	}

	delay := 10 * time.Second
	for i := 1; i < attempt; i++ {
		delay *= 2
		if delay >= maxDelay {
			return maxDelay
		}
	}

	if delay > maxDelay {
		return maxDelay
	}

	return delay
}

func minPositiveDuration(value, fallback time.Duration) time.Duration {
	if value <= 0 {
		return fallback
	}

	if fallback <= 0 || value < fallback {
		return value
	}

	return fallback
}

func nextAttempt(current int) int {
	if current <= 0 {
		return 1
	}

	return current + 1
}

func isActiveState(state string, cfg Config) bool {
	return stateIn(state, cfg.Tracker.ActiveStates)
}

func isTerminalState(state string, cfg Config) bool {
	return stateIn(state, cfg.Tracker.TerminalStates)
}

func stateIn(state string, states []string) bool {
	state = normalizeState(state)
	for _, candidate := range states {
		if normalizeState(candidate) == state {
			return true
		}
	}

	return false
}

func hasNonTerminalBlocker(issue Issue, cfg Config) bool {
	for _, blocker := range issue.BlockedBy {
		if blocker.State == nil || !isTerminalState(*blocker.State, cfg) {
			return true
		}
	}

	return false
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}

	return b
}
