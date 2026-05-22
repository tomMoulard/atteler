//nolint:govet,wsl_v5 // Debug snapshots intentionally flatten scheduler state into operator-friendly JSON.
package symphony

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"
)

// DebugSnapshotter is implemented by the orchestrator and kept small for HTTP
// handler tests.
type DebugSnapshotter interface {
	Snapshot(context.Context) (DebugSnapshot, error)
}

// DebugSnapshot is the JSON payload returned by GET /debug/status.
type DebugSnapshot struct {
	Now               time.Time             `json:"now"`
	Service           DebugServiceSnapshot  `json:"service"`
	Workflow          DebugWorkflowSnapshot `json:"workflow"`
	Config            DebugConfigSnapshot   `json:"config"`
	Counts            DebugCounts           `json:"counts"`
	Codex             DebugCodexSnapshot    `json:"codex"`
	Summary           DebugSummary          `json:"summary"`
	Running           []DebugRunningIssue   `json:"running"`
	Retries           []DebugRetry          `json:"retries"`
	PullRequests      []DebugPullRequest    `json:"pull_requests"`
	RecentEvents      []DebugEvent          `json:"recent_events"`
	ClaimedIssueIDs   []string              `json:"claimed_issue_ids"`
	CompletedIssueIDs []string              `json:"completed_issue_ids"`
}

// DebugServiceSnapshot summarizes service lifecycle timing.
type DebugServiceSnapshot struct {
	StartedAt     time.Time `json:"started_at"`
	LastTickAt    time.Time `json:"last_tick_at"`
	NextTickAt    time.Time `json:"next_tick_at"`
	UptimeSeconds int64     `json:"uptime_seconds"`
}

// DebugWorkflowSnapshot summarizes the active workflow.
type DebugWorkflowSnapshot struct {
	Path       string    `json:"path"`
	Tracker    string    `json:"tracker"`
	ModTime    time.Time `json:"mod_time"`
	Size       int64     `json:"size,omitempty"`
	PromptSize int       `json:"prompt_size"`
	Loaded     bool      `json:"loaded"`
}

// DebugConfigSnapshot reports non-secret operational config.
type DebugConfigSnapshot struct {
	WorkflowPath                  string   `json:"workflow_path"`
	WorkspaceRoot                 string   `json:"workspace_root"`
	TrackerKind                   string   `json:"tracker_kind"`
	TrackerRepository             string   `json:"tracker_repository,omitempty"`
	TrackerActiveStates           []string `json:"tracker_active_states,omitempty"`
	TrackerLabels                 []string `json:"tracker_labels,omitempty"`
	PollIntervalMS                int64    `json:"poll_interval_ms"`
	MaxConcurrentAgents           int      `json:"max_concurrent_agents"`
	MaxTurns                      int      `json:"max_turns"`
	StallTimeoutMS                int64    `json:"stall_timeout_ms"`
	PublishEnabled                bool     `json:"publish_enabled"`
	PublishBaseBranch             string   `json:"publish_base_branch,omitempty"`
	PublishBranchPrefix           string   `json:"publish_branch_prefix,omitempty"`
	PublishRemoveLabels           []string `json:"publish_remove_labels,omitempty"`
	PublishMonitorChecks          bool     `json:"publish_monitor_checks"`
	PublishCheckIntervalMS        int64    `json:"publish_check_interval_ms,omitempty"`
	PublishMaxCheckReworkAttempts int      `json:"publish_max_check_rework_attempts,omitempty"`
	DebugEnabled                  bool     `json:"debug_enabled"`
	DebugAddress                  string   `json:"debug_address,omitempty"`
}

// DebugCounts summarizes scheduler queues.
type DebugCounts struct {
	Running             int `json:"running"`
	Claimed             int `json:"claimed"`
	Retries             int `json:"retries"`
	PullRequests        int `json:"pull_requests"`
	Completed           int `json:"completed"`
	MaxConcurrentAgents int `json:"max_concurrent_agents"`
	AvailableSlots      int `json:"available_slots"`
}

// DebugCodexSnapshot summarizes Codex runtime totals.
type DebugCodexSnapshot struct {
	RateLimits     jsonRaw `json:"rate_limits,omitempty"`
	RuntimeSeconds int64   `json:"runtime_seconds"`
	InputTokens    int64   `json:"input_tokens"`
	OutputTokens   int64   `json:"output_tokens"`
	TotalTokens    int64   `json:"total_tokens"`
}

// DebugSummary is intentionally phrased around the operator questions the
// debug API exists to answer.
type DebugSummary struct {
	WhatHappened  []string `json:"what_happened"`
	WhatIsGoingOn []string `json:"what_is_going_on"`
	WhatWillDo    []string `json:"what_will_do"`
}

// DebugRunningIssue describes one active worker.
type DebugRunningIssue struct {
	Issue          Issue     `json:"issue"`
	StartedAt      time.Time `json:"started_at"`
	LastCodexTime  time.Time `json:"last_codex_time"`
	StallDeadline  time.Time `json:"stall_deadline"`
	ElapsedMS      int64     `json:"elapsed_ms"`
	IdleMS         int64     `json:"idle_ms"`
	Attempt        int       `json:"attempt"`
	WorkspacePath  string    `json:"workspace_path,omitempty"`
	LastCodexEvent string    `json:"last_codex_event,omitempty"`
	LastMessage    string    `json:"last_message,omitempty"`
	SessionID      string    `json:"session_id,omitempty"`
	ThreadID       string    `json:"thread_id,omitempty"`
	TurnID         string    `json:"turn_id,omitempty"`
	AppServerPID   string    `json:"app_server_pid,omitempty"`
	CancelReason   string    `json:"cancel_reason,omitempty"`
	InputTokens    int64     `json:"input_tokens"`
	OutputTokens   int64     `json:"output_tokens"`
	TotalTokens    int64     `json:"total_tokens"`
}

// DebugRetry describes one queued retry.
type DebugRetry struct {
	DueAt      time.Time `json:"due_at"`
	DelayMS    int64     `json:"delay_ms"`
	IssueID    string    `json:"issue_id"`
	Identifier string    `json:"identifier"`
	Error      string    `json:"error,omitempty"`
	Attempt    int       `json:"attempt"`
}

// DebugPullRequest describes one PR check monitor.
type DebugPullRequest struct {
	LastSnapshot   PullRequestCheckSnapshot  `json:"last_snapshot,omitzero"`
	PendingRework  *PullRequestCheckSnapshot `json:"pending_rework,omitempty"`
	NextCheckAt    time.Time                 `json:"next_check_at,omitzero"`
	LastCheckAt    time.Time                 `json:"last_check_at,omitzero"`
	LastReworkAt   time.Time                 `json:"last_rework_at,omitzero"`
	DelayMS        int64                     `json:"delay_ms"`
	Issue          Issue                     `json:"issue"`
	LastError      string                    `json:"last_error,omitempty"`
	Branch         string                    `json:"branch,omitempty"`
	PullRequestURL string                    `json:"pull_request_url,omitempty"`
	ReworkAttempts int                       `json:"rework_attempts"`
	Number         int                       `json:"number"`
	InRework       bool                      `json:"in_rework"`
	Exhausted      bool                      `json:"exhausted"`
}

// DebugEvent is an append-only recent event entry for operator history.
type DebugEvent struct {
	Timestamp time.Time      `json:"timestamp"`
	Kind      string         `json:"kind"`
	Message   string         `json:"message,omitempty"`
	IssueID   string         `json:"issue_id,omitempty"`
	Issue     string         `json:"issue_identifier,omitempty"`
	Fields    map[string]any `json:"fields,omitempty"`
}

// DebugServer exposes local status endpoints for inspecting a running Symphony
// process.
type DebugServer struct {
	server   *http.Server
	listener net.Listener
	logger   *slog.Logger
}

// StartDebugServer starts the local debug HTTP API when enabled.
func StartDebugServer(ctx context.Context, cfg DebugConfig, source DebugSnapshotter, logger *slog.Logger) (*DebugServer, error) {
	if !cfg.Enabled {
		return nil, nil
	}

	if ctx == nil {
		return nil, errors.New("debug server: context is required")
	}

	if source == nil {
		return nil, errors.New("debug server: snapshot source is required")
	}

	address := firstNonEmpty(cfg.Address, defaultDebugAddress)
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(ctx, "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("debug server: listen %s: %w", address, err)
	}

	debugServer := &DebugServer{
		listener: listener,
		logger:   loggerOrDefault(logger),
	}
	debugServer.server = &http.Server{
		Handler:           debugMux(source),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go debugServer.serve()
	go debugServer.shutdownOnContext(ctx)

	debugServer.logger.Info("symphony debug server listening", "address", listener.Addr().String())
	return debugServer, nil
}

// Address returns the actual listen address. It is useful when address port 0
// is used in tests.
func (s *DebugServer) Address() string {
	if s == nil || s.listener == nil {
		return ""
	}

	return s.listener.Addr().String()
}

// Shutdown stops the debug HTTP server.
func (s *DebugServer) Shutdown(ctx context.Context) error {
	if s == nil || s.server == nil {
		return nil
	}

	if ctx == nil {
		return errors.New("debug server shutdown: context is required")
	}

	if err := s.server.Shutdown(ctx); err != nil {
		return fmt.Errorf("debug server shutdown: %w", err)
	}

	return nil
}

// Close immediately stops the debug HTTP server without waiting for active
// requests. It is used after caller cancellation, when a graceful Shutdown
// would be rejected by the already-canceled context.
func (s *DebugServer) Close() error {
	if s == nil || s.server == nil {
		return nil
	}

	if err := s.server.Close(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("debug server close: %w", err)
	}

	return nil
}

func (s *DebugServer) stop(ctx context.Context, timeout time.Duration) error {
	if ctx == nil || ctx.Err() != nil {
		return s.Close()
	}

	if timeout <= 0 {
		return s.Shutdown(ctx)
	}

	shutdownCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	return s.Shutdown(shutdownCtx)
}

func (s *DebugServer) serve() {
	err := s.server.Serve(s.listener)
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		s.logger.Warn("symphony debug server stopped unexpectedly", "error", err)
	}
}

func (s *DebugServer) shutdownOnContext(ctx context.Context) {
	<-ctx.Done()

	if err := s.Close(); err != nil {
		s.logger.Warn("symphony debug server shutdown failed", "error", err)
	}
}

func debugMux(source DebugSnapshotter) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug", handleDebugIndex)
	mux.HandleFunc("/debug/healthz", handleDebugHealthz)
	mux.HandleFunc("/debug/status", handleDebugStatus(source))
	mux.HandleFunc("/debug/events", handleDebugEvents(source))

	return mux
}

func handleDebugIndex(w http.ResponseWriter, _ *http.Request) {
	writeDebugJSON(w, http.StatusOK, map[string]any{
		"service": "atteler-symphony",
		"endpoints": []string{
			"GET /debug/healthz",
			"GET /debug/status",
			"GET /debug/events",
		},
	})
}

func handleDebugHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write([]byte("ok\n")); err != nil {
		slog.Default().Warn("symphony debug health response failed", "error", err)
	}
}

func handleDebugStatus(source DebugSnapshotter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		snapshot, err := source.Snapshot(r.Context())
		if err != nil {
			writeDebugError(w, http.StatusServiceUnavailable, err)
			return
		}

		writeDebugJSON(w, http.StatusOK, snapshot)
	}
}

func handleDebugEvents(source DebugSnapshotter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		snapshot, err := source.Snapshot(r.Context())
		if err != nil {
			writeDebugError(w, http.StatusServiceUnavailable, err)
			return
		}

		writeDebugJSON(w, http.StatusOK, map[string]any{
			"now":           snapshot.Now,
			"recent_events": snapshot.RecentEvents,
		})
	}
}

func writeDebugError(w http.ResponseWriter, status int, err error) {
	writeDebugJSON(w, status, map[string]any{"error": err.Error()})
}

func writeDebugJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		slog.Default().Warn("symphony debug response encode failed", "error", err)
	}
}

// Snapshot returns an event-loop-consistent scheduler snapshot.
func (o *Orchestrator) Snapshot(ctx context.Context) (DebugSnapshot, error) {
	if o == nil {
		return DebugSnapshot{}, errors.New("orchestrator is nil")
	}

	if ctx == nil {
		return DebugSnapshot{}, errors.New("debug snapshot: context is required")
	}

	reply := make(chan debugSnapshotResponse, 1)
	event := debugSnapshotEvent{reply: reply}

	select {
	case o.events <- event:
	case <-ctx.Done():
		return DebugSnapshot{}, fmt.Errorf("debug snapshot enqueue: %w", ctx.Err())
	}

	select {
	case response := <-reply:
		return response.snapshot, response.err
	case <-ctx.Done():
		return DebugSnapshot{}, fmt.Errorf("debug snapshot wait: %w", ctx.Err())
	}
}

func (o *Orchestrator) handleDebugSnapshot(event debugSnapshotEvent) {
	event.reply <- debugSnapshotResponse{snapshot: o.buildDebugSnapshot(time.Now().UTC())}
}

func (o *Orchestrator) buildDebugSnapshot(now time.Time) DebugSnapshot {
	snapshot, workflowLoaded := o.manager.Current()
	cfg := snapshot.Config
	running := o.debugRunning(now, cfg)
	retries := o.debugRetries(now)
	pullRequests := o.debugPullRequests(now)
	claimed := sortedKeys(o.state.Claimed)
	completed := sortedKeys(o.state.Completed)
	recentEvents := append([]DebugEvent(nil), o.state.RecentEvents...)
	counts := DebugCounts{
		Running:             len(running),
		Claimed:             len(claimed),
		Retries:             len(retries),
		PullRequests:        len(pullRequests),
		Completed:           len(completed),
		MaxConcurrentAgents: cfg.Agent.MaxConcurrentAgents,
		AvailableSlots:      o.availableSlots(cfg),
	}

	return DebugSnapshot{
		Now: now,
		Service: DebugServiceSnapshot{
			StartedAt:     o.state.StartedAt,
			LastTickAt:    o.state.LastTickAt,
			NextTickAt:    o.state.NextTickAt,
			UptimeSeconds: int64(now.Sub(o.state.StartedAt).Seconds()),
		},
		Workflow: DebugWorkflowSnapshot{
			Path:       cfg.WorkflowPath,
			Tracker:    cfg.Tracker.Kind,
			Loaded:     workflowLoaded,
			ModTime:    snapshot.ModTime,
			Size:       snapshot.Size,
			PromptSize: len(snapshot.Definition.PromptTemplate),
		},
		Config:            debugConfigSnapshot(cfg),
		Counts:            counts,
		Codex:             debugCodexSnapshot(o.state.CodexTotals, o.state.CodexRateLimits),
		Summary:           debugSummary(now, running, retries, pullRequests, recentEvents, cfg, counts),
		Running:           running,
		Retries:           retries,
		PullRequests:      pullRequests,
		RecentEvents:      recentEvents,
		ClaimedIssueIDs:   claimed,
		CompletedIssueIDs: completed,
	}
}

func (o *Orchestrator) debugRunning(now time.Time, cfg Config) []DebugRunningIssue {
	running := make([]DebugRunningIssue, 0, len(o.state.Running))
	for _, entry := range o.state.Running {
		lastActivity := entry.StartedAt
		if !entry.LastCodexTime.IsZero() {
			lastActivity = entry.LastCodexTime
		}

		debugEntry := DebugRunningIssue{
			Issue:          entry.Issue,
			StartedAt:      entry.StartedAt,
			LastCodexTime:  entry.LastCodexTime,
			ElapsedMS:      now.Sub(entry.StartedAt).Milliseconds(),
			IdleMS:         now.Sub(lastActivity).Milliseconds(),
			Attempt:        entry.Attempt,
			WorkspacePath:  entry.WorkspacePath,
			LastCodexEvent: entry.LastCodexEvent,
			LastMessage:    entry.LastMessage,
			SessionID:      entry.SessionID,
			ThreadID:       entry.ThreadID,
			TurnID:         entry.TurnID,
			AppServerPID:   entry.AppServerPID,
			CancelReason:   string(entry.CancelReason),
			InputTokens:    entry.InputTokens,
			OutputTokens:   entry.OutputTokens,
			TotalTokens:    entry.TotalTokens,
		}
		if cfg.Codex.StallTimeout > 0 {
			debugEntry.StallDeadline = lastActivity.Add(cfg.Codex.StallTimeout)
		}

		running = append(running, debugEntry)
	}

	sort.Slice(running, func(i, j int) bool {
		return running[i].Issue.Identifier < running[j].Issue.Identifier
	})

	return running
}

func (o *Orchestrator) debugRetries(now time.Time) []DebugRetry {
	retries := make([]DebugRetry, 0, len(o.state.RetryAttempts))
	for _, retry := range o.state.RetryAttempts {
		retries = append(retries, DebugRetry{
			DueAt:      retry.DueAt,
			DelayMS:    max(retry.DueAt.Sub(now).Milliseconds(), 0),
			IssueID:    retry.IssueID,
			Identifier: retry.Identifier,
			Error:      retry.Error,
			Attempt:    retry.Attempt,
		})
	}

	sort.Slice(retries, func(i, j int) bool {
		return retries[i].DueAt.Before(retries[j].DueAt)
	})

	return retries
}

func (o *Orchestrator) debugPullRequests(now time.Time) []DebugPullRequest {
	pullRequests := make([]DebugPullRequest, 0, len(o.state.PullRequests))
	for _, monitor := range o.state.PullRequests {
		entry := DebugPullRequest{
			LastSnapshot:   monitor.LastSnapshot,
			PendingRework:  monitor.PendingRework,
			NextCheckAt:    monitor.NextCheckAt,
			LastCheckAt:    monitor.LastCheckAt,
			LastReworkAt:   monitor.LastReworkAt,
			DelayMS:        max(monitor.NextCheckAt.Sub(now).Milliseconds(), 0),
			Issue:          monitor.Issue,
			LastError:      monitor.LastError,
			Branch:         monitor.Branch,
			PullRequestURL: monitor.PullRequestURL,
			ReworkAttempts: monitor.ReworkAttempts,
			Number:         monitor.Number,
			InRework:       monitor.InRework,
			Exhausted:      monitor.Exhausted,
		}
		pullRequests = append(pullRequests, entry)
	}

	sort.Slice(pullRequests, func(i, j int) bool {
		return pullRequests[i].Number < pullRequests[j].Number
	})

	return pullRequests
}

func debugConfigSnapshot(cfg Config) DebugConfigSnapshot {
	return DebugConfigSnapshot{
		WorkflowPath:                  cfg.WorkflowPath,
		WorkspaceRoot:                 cfg.Workspace.Root,
		TrackerKind:                   cfg.Tracker.Kind,
		TrackerRepository:             firstNonEmpty(cfg.Tracker.Repository, cfg.Tracker.Owner+"/"+cfg.Tracker.Repo),
		TrackerActiveStates:           append([]string(nil), cfg.Tracker.ActiveStates...),
		TrackerLabels:                 append([]string(nil), cfg.Tracker.Labels...),
		PollIntervalMS:                cfg.Polling.Interval.Milliseconds(),
		MaxConcurrentAgents:           cfg.Agent.MaxConcurrentAgents,
		MaxTurns:                      cfg.Agent.MaxTurns,
		StallTimeoutMS:                cfg.Codex.StallTimeout.Milliseconds(),
		PublishEnabled:                cfg.Publish.Enabled,
		PublishBaseBranch:             cfg.Publish.BaseBranch,
		PublishBranchPrefix:           cfg.Publish.BranchPrefix,
		PublishRemoveLabels:           append([]string(nil), cfg.Publish.RemoveLabels...),
		PublishMonitorChecks:          cfg.Publish.MonitorChecks,
		PublishCheckIntervalMS:        cfg.Publish.CheckInterval.Milliseconds(),
		PublishMaxCheckReworkAttempts: cfg.Publish.MaxCheckReworkAttempts,
		DebugEnabled:                  cfg.Debug.Enabled,
		DebugAddress:                  cfg.Debug.Address,
	}
}

func debugCodexSnapshot(totals codexTotals, rateLimits jsonRaw) DebugCodexSnapshot {
	return DebugCodexSnapshot{
		RateLimits:     append(jsonRaw(nil), rateLimits...),
		RuntimeSeconds: totals.RuntimeSeconds,
		InputTokens:    totals.InputTokens,
		OutputTokens:   totals.OutputTokens,
		TotalTokens:    totals.TotalTokens,
	}
}

func debugSummary(now time.Time, running []DebugRunningIssue, retries []DebugRetry, pullRequests []DebugPullRequest, events []DebugEvent, cfg Config, counts DebugCounts) DebugSummary {
	return DebugSummary{
		WhatHappened:  recentEventSummaries(events),
		WhatIsGoingOn: currentActivitySummaries(running, retries, pullRequests),
		WhatWillDo:    nextActionSummaries(now, running, retries, pullRequests, cfg, counts),
	}
}

func recentEventSummaries(events []DebugEvent) []string {
	if len(events) == 0 {
		return []string{"No recent scheduler events have been recorded yet."}
	}

	start := max(len(events)-8, 0)

	out := make([]string, 0, len(events)-start)
	for _, event := range events[start:] {
		subject := firstNonEmpty(event.Issue, event.IssueID)
		if subject != "" {
			out = append(out, fmt.Sprintf("%s: %s (%s)", event.Kind, event.Message, subject))
			continue
		}

		out = append(out, strings.TrimSpace(event.Kind+": "+event.Message))
	}

	return out
}

func currentActivitySummaries(running []DebugRunningIssue, retries []DebugRetry, pullRequests []DebugPullRequest) []string {
	var out []string
	for index := range running {
		entry := &running[index]
		idle := (time.Duration(entry.IdleMS) * time.Millisecond).Round(time.Second)
		out = append(out, fmt.Sprintf("%s is running attempt %d; last event=%s; idle=%s", entry.Issue.Identifier, entry.Attempt, firstNonEmpty(entry.LastCodexEvent, "none"), idle))
	}

	for _, retry := range retries {
		out = append(out, fmt.Sprintf("%s retry attempt %d is queued for %s", retry.Identifier, retry.Attempt, retry.DueAt.Format(time.RFC3339)))
	}

	for index := range pullRequests {
		pr := &pullRequests[index]
		state := string(pr.LastSnapshot.State)
		if state == "" {
			state = "unknown"
		}

		switch {
		case pr.InRework:
			out = append(out, fmt.Sprintf("PR #%d is being reworked on %s after %d attempt(s).", pr.Number, firstNonEmpty(pr.Branch, "its branch"), pr.ReworkAttempts))
		case pr.Exhausted:
			out = append(out, fmt.Sprintf("PR #%d checks are %s and rework is exhausted: %s", pr.Number, state, pr.LastError))
		default:
			out = append(out, fmt.Sprintf("PR #%d checks are %s; next check at %s.", pr.Number, state, pr.NextCheckAt.Format(time.RFC3339)))
		}
	}

	if len(out) == 0 {
		return []string{"No workers are running and no retries are queued."}
	}

	return out
}

func nextActionSummaries(now time.Time, running []DebugRunningIssue, retries []DebugRetry, pullRequests []DebugPullRequest, cfg Config, counts DebugCounts) []string {
	var out []string
	for index := range running {
		entry := &running[index]
		if !entry.StallDeadline.IsZero() {
			out = append(out, fmt.Sprintf("Monitor %s for stall until %s.", entry.Issue.Identifier, entry.StallDeadline.Format(time.RFC3339)))
		}
	}

	for _, retry := range retries {
		delay := max(retry.DueAt.Sub(now).Round(time.Second), 0)

		out = append(out, fmt.Sprintf("Retry %s attempt %d in %s.", retry.Identifier, retry.Attempt, delay))
	}

	for index := range pullRequests {
		pr := &pullRequests[index]
		if pr.Exhausted {
			out = append(out, fmt.Sprintf("PR #%d will wait for human attention because the check rework budget is exhausted.", pr.Number))
			continue
		}

		if pr.InRework {
			out = append(out, fmt.Sprintf("PR #%d will be published back to the same branch when the rework worker succeeds.", pr.Number))
			continue
		}

		delay := max(pr.NextCheckAt.Sub(now).Round(time.Second), 0)
		out = append(out, fmt.Sprintf("Check PR #%d again in %s; if checks fail, dispatch a PR rework worker.", pr.Number, delay))
	}

	if counts.AvailableSlots > 0 {
		out = append(out, fmt.Sprintf("On the next poll, fetch %s issues with labels %s and dispatch up to %d worker(s).", strings.Join(cfg.Tracker.ActiveStates, ","), strings.Join(cfg.Tracker.Labels, ","), counts.AvailableSlots))
	}

	if cfg.Publish.Enabled {
		out = append(out, fmt.Sprintf("Successful GitHub workers will publish PRs to %s and remove labels %s.", cfg.Publish.BaseBranch, strings.Join(cfg.Publish.RemoveLabels, ",")))
	}

	if cfg.Publish.MonitorChecks {
		out = append(out, fmt.Sprintf("Published PRs are monitored every %s and can be reworked up to %d time(s).", cfg.Publish.CheckInterval.Round(time.Second), cfg.Publish.MaxCheckReworkAttempts))
	}

	if len(out) == 0 {
		return []string{"Wait for running workers or queued retries to produce the next scheduler event."}
	}

	return out
}

func (o *Orchestrator) recordIssueEvent(kind string, issue Issue, message string, fields ...any) {
	fields = append([]any{
		"issue_id", issue.ID,
		"issue_identifier", issue.Identifier,
	}, fields...)
	o.recordEvent(kind, message, fields...)
}

func (o *Orchestrator) recordEvent(kind, message string, fields ...any) {
	if o == nil {
		return
	}

	event := DebugEvent{
		Timestamp: time.Now().UTC(),
		Kind:      strings.TrimSpace(kind),
		Message:   strings.TrimSpace(message),
		Fields:    debugFields(fields...),
	}
	if issueID, ok := event.Fields["issue_id"].(string); ok {
		event.IssueID = issueID
		delete(event.Fields, "issue_id")
	}

	if issueIdentifier, ok := event.Fields["issue_identifier"].(string); ok {
		event.Issue = issueIdentifier
		delete(event.Fields, "issue_identifier")
	}

	if len(event.Fields) == 0 {
		event.Fields = nil
	}

	o.state.RecentEvents = append(o.state.RecentEvents, event)
	limit := defaultDebugEventLimit
	if snapshot, ok := o.manager.Current(); ok && snapshot.Config.Debug.EventLimit > 0 {
		limit = snapshot.Config.Debug.EventLimit
	}

	if len(o.state.RecentEvents) > limit {
		o.state.RecentEvents = append([]DebugEvent(nil), o.state.RecentEvents[len(o.state.RecentEvents)-limit:]...)
	}
}

func debugFields(fields ...any) map[string]any {
	out := make(map[string]any, len(fields)/2)
	for index := 0; index+1 < len(fields); index += 2 {
		key, ok := fields[index].(string)
		if !ok || strings.TrimSpace(key) == "" {
			continue
		}

		out[strings.TrimSpace(key)] = fields[index+1]
	}

	return out
}

func sortedKeys[T any](values map[string]T) []string {
	out := make([]string, 0, len(values))
	for key := range values {
		out = append(out, key)
	}

	sort.Strings(out)

	return out
}
