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
	manager    *WorkflowManager
	tracker    TrackerClient
	runner     AgentRunner
	workspaces *WorkspaceManager
	logger     *slog.Logger
	events     chan orchestratorEvent
	state      runtimeState
	wg         sync.WaitGroup
}

type runtimeState struct {
	Running             map[string]*runningEntry
	Claimed             map[string]struct{}
	RetryAttempts       map[string]*RetryEntry
	PullRequests        map[int]*pullRequestMonitorEntry
	Completed           map[string]struct{}
	CodexRateLimits     jsonRaw
	RecentEvents        []DebugEvent
	StartedAt           time.Time
	LastTickAt          time.Time
	NextTickAt          time.Time
	PollInterval        time.Duration
	MaxConcurrentAgents int
	CodexTotals         codexTotals
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
	Issue          Issue
	LastSnapshot   PullRequestCheckSnapshot
	Timer          *time.Timer
	NextCheckAt    time.Time
	LastCheckAt    time.Time
	LastReworkAt   time.Time
	LastError      string
	Branch         string
	PullRequestURL string
	ReworkAttempts int
	Number         int
	InRework       bool
	Exhausted      bool
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
		manager:    manager,
		tracker:    tracker,
		runner:     runner,
		workspaces: NewWorkspaceManager(logger),
		logger:     loggerOrDefault(logger),
		events:     make(chan orchestratorEvent, 128),
		state: runtimeState{
			Running:             map[string]*runningEntry{},
			Claimed:             map[string]struct{}{},
			RetryAttempts:       map[string]*RetryEntry{},
			PullRequests:        map[int]*pullRequestMonitorEntry{},
			Completed:           map[string]struct{}{},
			StartedAt:           time.Now().UTC(),
			PollInterval:        snapshot.Config.Polling.Interval,
			MaxConcurrentAgents: snapshot.Config.Agent.MaxConcurrentAgents,
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

	if entry.CancelReason == cancelTerminal || entry.CancelReason == cancelNonActive || entry.CancelReason == cancelShutdown {
		delete(o.state.Claimed, event.issueID)
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
