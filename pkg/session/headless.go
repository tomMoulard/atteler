package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/tommoulard/atteler/pkg/llm"
)

const (
	headlessDirName = "headless"

	headlessLogChunkSuffixWidth = 6
	headlessLogChunkPrefix      = ".log."
	headlessLogRedactedValue    = "[REDACTED]"
	headlessTempFilePrefix      = ".headless-"

	defaultHeadlessLogMaxChunkBytes     int64 = 1024 * 1024
	defaultHeadlessLogMaxChunks               = 8
	defaultHeadlessLogTailBytes         int64 = 64 * 1024
	defaultHeadlessLegacyMigrationBytes       = defaultHeadlessLogMaxChunkBytes * defaultHeadlessLogMaxChunks
	defaultHeadlessStaleAfter                 = 30 * time.Minute
	defaultHeadlessCancelKillGrace            = 200 * time.Millisecond
)

var (
	headlessAssignmentSecretRE = regexp.MustCompile(`(?i)\b([a-z0-9_.-]*(?:api[_-]?key|token|secret|password|passwd|pwd|credential|authorization)[a-z0-9_.-]*\s*[:=]\s*)(["']?)([^\s"']+)(["']?)`)
	headlessFlagSecretRE       = regexp.MustCompile(`(?i)(^|\s)(--?[a-z0-9_.-]*(?:api[_-]?key|token|secret|password|passwd|pwd|credential|authorization)[a-z0-9_.-]*\s+)(["']?)([^\s"']+)(["']?)`)
	headlessBearerSecretRE     = regexp.MustCompile(`(?i)\b(Bearer\s+)([A-Za-z0-9._~+/=-]{8,})`)
	headlessOpenAISecretRE     = regexp.MustCompile(`\b(sk-[A-Za-z0-9][A-Za-z0-9_-]{12,})\b`)
	headlessGitHubSecretRE     = regexp.MustCompile(`\b(gh[pousr]_[A-Za-z0-9_]{12,})\b`)
	headlessAWSAccessKeyRE     = regexp.MustCompile(`\b(AKIA[0-9A-Z]{16})\b`)

	// ErrCorruptHeadlessRun is returned when a headless metadata file exists
	// but cannot be decoded. Listing and status commands surface this as a
	// durable corrupt run record instead of hiding every other run.
	ErrCorruptHeadlessRun = errors.New("session: corrupt headless metadata")
)

// HeadlessStatus is the lifecycle state for a headless run.
type HeadlessStatus string

const (
	// HeadlessStatusRunning indicates a run is still active.
	HeadlessStatusRunning HeadlessStatus = "running"
	// HeadlessStatusCompleted indicates a run finished successfully.
	HeadlessStatusCompleted HeadlessStatus = "completed"
	// HeadlessStatusFailed indicates a run ended with an error.
	HeadlessStatusFailed HeadlessStatus = "failed"
	// HeadlessStatusCanceled indicates a run was canceled by an operator.
	HeadlessStatusCanceled HeadlessStatus = "canceled"
	// HeadlessStatusTimedOut indicates a run exceeded its execution deadline.
	HeadlessStatusTimedOut HeadlessStatus = "timed_out"
	// HeadlessStatusStale indicates a previously running run no longer appears healthy.
	HeadlessStatusStale HeadlessStatus = "stale"
	// HeadlessStatusOrphaned indicates a local process still exists but no longer heartbeats.
	HeadlessStatusOrphaned HeadlessStatus = "orphaned"
	// HeadlessStatusSuperseded indicates a run was intentionally replaced by another run.
	HeadlessStatusSuperseded HeadlessStatus = "superseded"
	// HeadlessStatusCorrupt indicates metadata exists but could not be parsed.
	HeadlessStatusCorrupt HeadlessStatus = "corrupt"
)

// HeadlessEventType identifies a structured summary event for automation.
type HeadlessEventType string

const (
	// HeadlessEventStarted records the beginning of a headless process.
	HeadlessEventStarted HeadlessEventType = "started"
	// HeadlessEventUserMessage records the submitted user prompt summary.
	HeadlessEventUserMessage HeadlessEventType = "user_message"
	// HeadlessEventAssistantMessage records an assistant message summary.
	HeadlessEventAssistantMessage HeadlessEventType = "assistant_message"
	// HeadlessEventCompleted records successful completion.
	HeadlessEventCompleted HeadlessEventType = "completed"
	// HeadlessEventFailed records failed completion.
	HeadlessEventFailed HeadlessEventType = "failed"
	// HeadlessEventCanceled records operator cancellation.
	HeadlessEventCanceled HeadlessEventType = "canceled"
	// HeadlessEventTimedOut records deadline-based termination.
	HeadlessEventTimedOut HeadlessEventType = "timed_out"
	// HeadlessEventStale records stale-run reconciliation.
	HeadlessEventStale HeadlessEventType = "stale"
	// HeadlessEventOrphaned records a live local process with stale metadata.
	HeadlessEventOrphaned HeadlessEventType = "orphaned"
	// HeadlessEventSuperseded records that a run was intentionally replaced.
	HeadlessEventSuperseded HeadlessEventType = "superseded"
)

const (
	headlessEventRoleAssistant = "assistant"
	headlessEventRoleUser      = "user"
)

// HeadlessLogPolicy bounds retained log data for one headless run.
type HeadlessLogPolicy struct {
	MaxAge        time.Duration
	MaxChunkBytes int64
	MaxChunks     int
}

// HeadlessLogWriteOptions controls one headless log append.
type HeadlessLogWriteOptions struct {
	Policy  HeadlessLogPolicy
	Private bool
}

// HeadlessLogOffset points at the next byte to read from a retained log chunk.
type HeadlessLogOffset struct {
	Byte  int64 `json:"byte"`
	Chunk int   `json:"chunk"`
}

// HeadlessLogTailOptions controls bounded log tail reads.
type HeadlessLogTailOptions struct {
	Offset   HeadlessLogOffset
	MaxBytes int64
}

// HeadlessLogTail is a bounded slice of log text and the offset for the next read.
type HeadlessLogTail struct {
	Text           string            `json:"text"`
	NextOffset     HeadlessLogOffset `json:"next_offset"`
	RetainedOffset HeadlessLogOffset `json:"retained_offset"`
	Truncated      bool              `json:"truncated"`
}

// HeadlessEvent is a structured lifecycle summary stored separately from the
// raw bounded log chunks. It intentionally mirrors the fields automation needs
// to compare headless runs with interactive lifecycle hooks.
//
//nolint:govet // JSON summary fields are grouped by lifecycle and metadata readability.
type HeadlessEvent struct {
	At              time.Time           `json:"at"`
	RunID           string              `json:"run_id"`
	ParentRunID     string              `json:"parent_run_id,omitempty"`
	SessionID       string              `json:"session_id,omitempty"`
	SessionPath     string              `json:"session_path,omitempty"`
	Type            HeadlessEventType   `json:"type"`
	Status          HeadlessStatus      `json:"status,omitempty"`
	Role            string              `json:"role,omitempty"`
	Message         string              `json:"message,omitempty"`
	Error           string              `json:"error,omitempty"`
	Agent           string              `json:"agent,omitempty"`
	Model           string              `json:"model,omitempty"`
	AgentLoopBudget llm.AgentLoopBudget `json:"agent_loop_budget,omitzero"`
	CWD             string              `json:"cwd,omitempty"`
	Hostname        string              `json:"hostname,omitempty"`
	StartedCommand  string              `json:"started_command,omitempty"`
	StartMethod     string              `json:"start_method,omitempty"`
	TerminalReason  string              `json:"terminal_reason,omitempty"`
	CancelReason    string              `json:"cancellation_reason,omitempty"`
	StaleReason     string              `json:"stale_reason,omitempty"`
	OrphanedReason  string              `json:"orphaned_reason,omitempty"`
	ExitCode        *int                `json:"exit_code,omitempty"`
	CommandArgs     []string            `json:"command_args,omitempty"`
	ChildRunIDs     []string            `json:"child_run_ids,omitempty"`
	PID             int                 `json:"pid,omitempty"`
	ParentPID       int                 `json:"parent_pid,omitempty"`
	ProcessGroupID  int                 `json:"process_group_id,omitempty"`
	Metadata        map[string]string   `json:"metadata,omitempty"`
}

// HeadlessRun records metadata for an atteler headless execution.
//
//nolint:govet // JSON metadata is grouped by lifecycle and operator-facing fields; padding is irrelevant.
type HeadlessRun struct {
	StartedAt          time.Time           `json:"started_at"`
	UpdatedAt          time.Time           `json:"updated_at"`
	LastHeartbeatAt    time.Time           `json:"last_heartbeat_at,omitzero"`
	CompletedAt        *time.Time          `json:"completed_at,omitempty"`
	CanceledAt         *time.Time          `json:"canceled_at,omitempty"`
	ExitCode           *int                `json:"exit_code,omitempty"`
	ID                 string              `json:"id"`
	ParentRunID        string              `json:"parent_run_id,omitempty"`
	SessionID          string              `json:"session_id"`
	SessionPath        string              `json:"session_path"`
	LogPath            string              `json:"log_path"`
	EventsPath         string              `json:"events_path,omitempty"`
	ArtifactDir        string              `json:"artifact_dir,omitempty"`
	CWD                string              `json:"cwd,omitempty"`
	Prompt             string              `json:"prompt"`
	Model              string              `json:"model"`
	ModelMode          string              `json:"model_mode,omitempty"`
	AgentLoopBudget    llm.AgentLoopBudget `json:"agent_loop_budget,omitzero"`
	Agent              string              `json:"agent"`
	Owner              string              `json:"owner,omitempty"`
	Hostname           string              `json:"hostname,omitempty"`
	StartedCommand     string              `json:"started_command,omitempty"`
	StartMethod        string              `json:"start_method,omitempty"`
	TerminalReason     string              `json:"terminal_reason,omitempty"`
	CancellationReason string              `json:"cancellation_reason,omitempty"`
	StaleReason        string              `json:"stale_reason,omitempty"`
	OrphanedReason     string              `json:"orphaned_reason,omitempty"`
	Error              string              `json:"error"`
	CommandArgs        []string            `json:"command_args,omitempty"`
	ChildRunIDs        []string            `json:"child_run_ids,omitempty"`
	PID                int                 `json:"pid,omitempty"`
	ParentPID          int                 `json:"parent_pid,omitempty"`
	ProcessGroupID     int                 `json:"process_group_id,omitempty"`
	LogMaxChunkBytes   int64               `json:"log_max_chunk_bytes,omitempty"`
	LogMaxChunks       int                 `json:"log_max_chunks,omitempty"`
	Status             HeadlessStatus      `json:"status"`
	PrivateLogs        bool                `json:"private_logs,omitempty"`
	Stale              bool                `json:"stale,omitempty"`
}

type headlessLogChunk struct {
	modTime time.Time
	path    string
	index   int
	size    int64
}

// DefaultHeadlessLogPolicy returns the default bounded retention policy for headless logs.
func DefaultHeadlessLogPolicy() HeadlessLogPolicy {
	return HeadlessLogPolicy{
		MaxChunkBytes: defaultHeadlessLogMaxChunkBytes,
		MaxChunks:     defaultHeadlessLogMaxChunks,
	}
}

// RedactHeadlessText removes common secret values from headless metadata and logs.
func RedactHeadlessText(text string) string {
	if text == "" {
		return ""
	}

	redacted := redactPatternSecrets(text)
	for _, value := range headlessSecretEnvValues() {
		redacted = strings.ReplaceAll(redacted, value, headlessLogRedactedValue)
	}

	return redacted
}

// SaveHeadlessRun writes headless run metadata atomically enough for local CLI use.
func (s *Store) SaveHeadlessRun(run HeadlessRun) error {
	if err := validateHeadlessID(run.ID); err != nil {
		return err
	}

	return s.withHeadlessLock(run.ID, func() error {
		return s.saveHeadlessRunUnlocked(run)
	})
}

// SaveNewHeadlessRun writes headless run metadata only when no record, log,
// event stream, or artifact directory exists for the same ID. This protects
// explicit automation-provided IDs from overwriting a previous durable
// lifecycle record or appending to old retained logs.
func (s *Store) SaveNewHeadlessRun(run HeadlessRun) error {
	if err := validateHeadlessID(run.ID); err != nil {
		return err
	}

	return s.withHeadlessLock(run.ID, func() error {
		if err := s.ensureHeadlessRunArtifactsAbsentUnlocked(run.ID); err != nil {
			return err
		}

		return s.saveHeadlessRunUnlocked(run)
	})
}

// LoadHeadlessRun reads headless run metadata by ID.
func (s *Store) LoadHeadlessRun(id string) (HeadlessRun, error) {
	if err := validateHeadlessID(id); err != nil {
		return HeadlessRun{}, err
	}

	var run HeadlessRun

	err := s.withHeadlessLock(id, func() error {
		loaded, err := s.loadHeadlessRunUnlocked(id)
		if err != nil {
			return err
		}

		run = loaded

		return nil
	})
	if err != nil {
		return HeadlessRun{}, err
	}

	return run, nil
}

// ListHeadlessRuns returns saved headless runs sorted by most recently updated first.
// Running runs are reconciled as part of listing so crashed local processes do
// not stay forever "running" in automation.
func (s *Store) ListHeadlessRuns() ([]HeadlessRun, error) {
	dir := s.headlessDir()

	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}

		return nil, fmt.Errorf("session: list headless %s: %w", dir, err)
	}

	runs := make([]HeadlessRun, 0, len(entries))
	now := time.Now().UTC()

	for _, entry := range entries {
		id, ok := headlessMetadataEntryID(entry)
		if !ok {
			continue
		}

		run, err := s.reconciledHeadlessRun(id, now, defaultHeadlessStaleAfter)
		if err != nil {
			if errors.Is(err, ErrCorruptHeadlessRun) {
				runs = append(runs, s.corruptHeadlessRun(id, err))
				continue
			}

			return nil, err
		}

		runs = append(runs, run)
	}

	sort.Slice(runs, func(i, j int) bool {
		if runs[i].UpdatedAt.Equal(runs[j].UpdatedAt) {
			return runs[i].StartedAt.After(runs[j].StartedAt)
		}

		return runs[i].UpdatedAt.After(runs[j].UpdatedAt)
	})

	return runs, nil
}

// HeadlessRunStatus returns one headless run after stale reconciliation. If the
// metadata is corrupt, a synthetic corrupt record is returned with no error.
func (s *Store) HeadlessRunStatus(id string) (HeadlessRun, error) {
	if err := validateHeadlessID(id); err != nil {
		return HeadlessRun{}, err
	}

	run, err := s.reconciledHeadlessRun(id, time.Now().UTC(), defaultHeadlessStaleAfter)
	if err != nil {
		if errors.Is(err, ErrCorruptHeadlessRun) {
			return s.corruptHeadlessRun(id, err), nil
		}

		return HeadlessRun{}, err
	}

	return run, nil
}

// CancelHeadlessRun marks a running headless run canceled and best-effort
// signals its recorded local process or process group. The canceled status is
// saved before the process is trusted to clean up, making cancellation durable
// even if the process exits abruptly.
func (s *Store) CancelHeadlessRun(id, reason string) (HeadlessRun, error) {
	if err := validateHeadlessID(id); err != nil {
		return HeadlessRun{}, err
	}

	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "canceled by user"
	}

	now := time.Now().UTC()

	var canceled HeadlessRun

	err := s.withHeadlessLock(id, func() error {
		run, err := s.cancelHeadlessRunUnlocked(id, reason, now)
		if err != nil {
			return err
		}

		canceled = run

		return nil
	})
	if err != nil {
		return HeadlessRun{}, err
	}

	return canceled, nil
}

// HeartbeatHeadlessRun records liveness for a running headless run. Terminal
// runs are left untouched so late heartbeat goroutines cannot move completed or
// canceled runs back to a running lifecycle.
func (s *Store) HeartbeatHeadlessRun(id string) error {
	if err := validateHeadlessID(id); err != nil {
		return err
	}

	now := time.Now().UTC()

	return s.withHeadlessLock(id, func() error {
		run, err := s.loadHeadlessRunUnlocked(id)
		if err != nil {
			return err
		}

		if run.Status != HeadlessStatusRunning && run.Status != HeadlessStatusOrphaned {
			return nil
		}

		run.Status = HeadlessStatusRunning
		run.Stale = false
		run.OrphanedReason = ""
		run.LastHeartbeatAt = now

		return s.saveHeadlessRunUnlocked(run)
	})
}

// SaveFinishedHeadlessRun writes terminal metadata unless another controller
// has already recorded a durable terminal state. It returns false when the
// incoming terminal update was skipped to preserve that state.
func (s *Store) SaveFinishedHeadlessRun(run HeadlessRun) (HeadlessRun, bool, error) {
	if err := validateHeadlessID(run.ID); err != nil {
		return HeadlessRun{}, false, err
	}

	if !isTerminalHeadlessStatus(run.Status) || run.Status == HeadlessStatusCorrupt {
		return HeadlessRun{}, false, fmt.Errorf("session: finished headless run %q must have a terminal status, got %q", run.ID, run.Status)
	}

	var (
		saved HeadlessRun
		wrote = true
	)

	err := s.withHeadlessLock(run.ID, func() error {
		current, found, err := s.loadOptionalHeadlessRunUnlocked(run.ID)
		if err != nil {
			return err
		}

		if found && isTerminalHeadlessStatus(current.Status) {
			saved = current
			wrote = false

			return nil
		}

		if found && !finishedHeadlessRunMatchesCurrent(current, run) {
			saved = current
			wrote = false

			return nil
		}

		if found {
			run = mergeFinishedHeadlessRun(current, run)
		}

		if saveErr := s.saveHeadlessRunUnlocked(run); saveErr != nil {
			return saveErr
		}

		loaded, loadErr := s.loadHeadlessRunUnlocked(run.ID)
		if loadErr != nil {
			return loadErr
		}

		saved = loaded

		return nil
	})
	if err != nil {
		return HeadlessRun{}, false, err
	}

	return saved, wrote, nil
}

func (s *Store) ensureHeadlessRunArtifactsAbsentUnlocked(id string) error {
	for _, path := range []string{
		s.headlessJSONPath(id),
		s.headlessEventsPath(id),
		s.headlessLogPath(id),
		s.headlessArtifactDir(id),
	} {
		if err := ensureHeadlessPathAbsent(id, path); err != nil {
			return err
		}
	}

	chunks, err := s.headlessLogChunksUnlocked(id)
	if err != nil {
		return err
	}

	if len(chunks) > 0 {
		return fmt.Errorf("session: headless run %q already exists: retained log chunk %s", id, chunks[0].path)
	}

	return nil
}

func ensureHeadlessPathAbsent(id, path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("session: headless run %q already exists: %s", id, path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("session: stat headless %s: %w", path, err)
	}

	return nil
}

func finishedHeadlessRunMatchesCurrent(current, finished HeadlessRun) bool {
	if current.ID == "" || finished.ID == "" || current.ID != finished.ID {
		return false
	}

	return headlessTimesMatch(current.StartedAt, finished.StartedAt) &&
		headlessIntsMatch(current.PID, finished.PID) &&
		headlessIntsMatch(current.ProcessGroupID, finished.ProcessGroupID) &&
		headlessStringsMatch(current.SessionID, finished.SessionID) &&
		headlessHostnamesMatch(current.Hostname, finished.Hostname)
}

func headlessTimesMatch(current, finished time.Time) bool {
	return current.IsZero() || finished.IsZero() || current.Equal(finished)
}

func headlessIntsMatch(current, finished int) bool {
	return current <= 0 || finished <= 0 || current == finished
}

func headlessStringsMatch(current, finished string) bool {
	return current == "" || finished == "" || current == finished
}

func headlessHostnamesMatch(current, finished string) bool {
	return current == "" || finished == "" || strings.EqualFold(current, finished)
}

func mergeFinishedHeadlessRun(current, finished HeadlessRun) HeadlessRun {
	run := finished

	run = mergeFinishedHeadlessTiming(current, run)
	run = mergeFinishedHeadlessIdentity(current, run)
	run = mergeFinishedHeadlessStorage(current, run)
	run = mergeFinishedHeadlessProcess(current, run)
	run.ChildRunIDs = mergeHeadlessChildRunIDs(current.ChildRunIDs, run.ChildRunIDs)
	run.PrivateLogs = run.PrivateLogs || current.PrivateLogs

	return run
}

func mergeFinishedHeadlessTiming(current, run HeadlessRun) HeadlessRun {
	if !current.StartedAt.IsZero() {
		run.StartedAt = current.StartedAt
	}

	if current.LastHeartbeatAt.After(run.LastHeartbeatAt) {
		run.LastHeartbeatAt = current.LastHeartbeatAt
	}

	return run
}

func mergeFinishedHeadlessIdentity(current, run HeadlessRun) HeadlessRun {
	if run.ParentRunID == "" {
		run.ParentRunID = current.ParentRunID
	}

	if run.SessionID == "" {
		run.SessionID = current.SessionID
	}

	if run.SessionPath == "" {
		run.SessionPath = current.SessionPath
	}

	if run.CWD == "" {
		run.CWD = current.CWD
	}

	if run.Prompt == "" {
		run.Prompt = current.Prompt
	}

	if run.Model == "" {
		run.Model = current.Model
	}

	if run.AgentLoopBudget.IsZero() {
		run.AgentLoopBudget = current.AgentLoopBudget
	}

	if run.Agent == "" {
		run.Agent = current.Agent
	}

	if run.Owner == "" {
		run.Owner = current.Owner
	}

	if run.Hostname == "" {
		run.Hostname = current.Hostname
	}

	if run.StartedCommand == "" {
		run.StartedCommand = current.StartedCommand
	}

	if run.StartMethod == "" {
		run.StartMethod = current.StartMethod
	}

	if len(run.CommandArgs) == 0 {
		run.CommandArgs = append([]string(nil), current.CommandArgs...)
	}

	return run
}

func mergeFinishedHeadlessProcess(current, run HeadlessRun) HeadlessRun {
	if run.PID == 0 {
		run.PID = current.PID
	}

	if run.ParentPID == 0 {
		run.ParentPID = current.ParentPID
	}

	if run.ProcessGroupID == 0 {
		run.ProcessGroupID = current.ProcessGroupID
	}

	return run
}

func mergeFinishedHeadlessStorage(current, run HeadlessRun) HeadlessRun {
	if run.LogPath == "" {
		run.LogPath = current.LogPath
	}

	if run.EventsPath == "" {
		run.EventsPath = current.EventsPath
	}

	if run.ArtifactDir == "" {
		run.ArtifactDir = current.ArtifactDir
	}

	if run.LogMaxChunkBytes == 0 {
		run.LogMaxChunkBytes = current.LogMaxChunkBytes
	}

	if run.LogMaxChunks == 0 {
		run.LogMaxChunks = current.LogMaxChunks
	}

	return run
}

// LinkHeadlessChildRun records a parent/child relationship between two
// headless runs. Missing parents are ignored so child starts are not blocked by
// races with older metadata cleanup.
func (s *Store) LinkHeadlessChildRun(parentID, childID string) error {
	if parentID == "" || childID == "" || parentID == childID {
		return nil
	}

	if err := validateHeadlessID(parentID); err != nil {
		return err
	}

	if err := validateHeadlessID(childID); err != nil {
		return err
	}

	return s.withHeadlessLock(parentID, func() error {
		parent, found, err := s.loadOptionalHeadlessRunUnlocked(parentID)
		if err != nil {
			if errors.Is(err, ErrCorruptHeadlessRun) {
				return nil
			}

			return err
		}

		if !found {
			return nil
		}

		if stringSliceContains(parent.ChildRunIDs, childID) {
			return nil
		}

		parent.ChildRunIDs = append(parent.ChildRunIDs, childID)

		return s.saveHeadlessRunMetadataOnlyUnlocked(parent)
	})
}

func (s *Store) cancelHeadlessRunUnlocked(id, reason string, now time.Time) (HeadlessRun, error) {
	run, err := s.loadHeadlessRunUnlocked(id)
	if err != nil {
		return HeadlessRun{}, err
	}

	if run.Status == HeadlessStatusOrphaned {
		if unavailable, staleReason := headlessProcessUnavailableReason(run); unavailable {
			return s.markHeadlessRunStaleUnlocked(run, now, staleReason)
		}

		return s.cancelRunningHeadlessRunUnlocked(run, reason, now)
	}

	if run.Status != HeadlessStatusRunning {
		return run, nil
	}

	if unavailable, staleReason := headlessProcessUnavailableReason(run); unavailable {
		return s.markHeadlessRunStaleUnlocked(run, now, staleReason)
	}

	return s.cancelRunningHeadlessRunUnlocked(run, reason, now)
}

func (s *Store) cancelRunningHeadlessRunUnlocked(run HeadlessRun, reason string, now time.Time) (HeadlessRun, error) {
	run = markHeadlessRunCanceled(run, now, reason)

	saved, err := s.saveAndLoadHeadlessRunUnlocked(run)
	if err != nil {
		return HeadlessRun{}, err
	}

	run = saved

	signalErr := terminateHeadlessRunProcess(run)
	if signalErr != nil {
		run.TerminalReason += "; signal: " + signalErr.Error()

		saved, err := s.saveAndLoadHeadlessRunUnlocked(run)
		if err != nil {
			return HeadlessRun{}, err
		}

		run = saved
	}

	if err := s.appendHeadlessCancelLogUnlocked(run, reason, now, signalErr); err != nil {
		return HeadlessRun{}, err
	}

	if err := s.appendHeadlessCancelEventUnlocked(run, reason, signalErr); err != nil {
		return HeadlessRun{}, err
	}

	return run, nil
}

func (s *Store) appendHeadlessCancelLogUnlocked(run HeadlessRun, reason string, now time.Time, signalErr error) error {
	line := "canceled\t" + now.Format(time.RFC3339) + "\treason=" + reason
	if signalErr != nil {
		line += "\tsignal_error=" + signalErr.Error()
	}

	return s.appendHeadlessLogTextUnlocked(run.ID, headlessLifecycleLogLine(run, line+"\n"), headlessLogPolicyForRun(run))
}

func (s *Store) appendHeadlessCancelEventUnlocked(run HeadlessRun, reason string, signalErr error) error {
	event := headlessEventForRun(run, HeadlessEventCanceled, reason)
	if signalErr != nil {
		event.Error = signalErr.Error()
	}

	return s.appendHeadlessEventUnlocked(run.ID, event)
}

func headlessLifecycleLogLine(run HeadlessRun, line string) string {
	if run.PrivateLogs {
		return line
	}

	return RedactHeadlessText(line)
}

// AppendHeadlessLog appends redacted text to the headless run log for id.
func (s *Store) AppendHeadlessLog(id, text string) error {
	return s.AppendHeadlessLogWithOptions(id, text, HeadlessLogWriteOptions{})
}

// AppendHeadlessLogWithOptions appends text to a bounded headless log.
func (s *Store) AppendHeadlessLogWithOptions(id, text string, options HeadlessLogWriteOptions) error {
	if err := validateHeadlessID(id); err != nil {
		return err
	}

	return s.withHeadlessLock(id, func() error {
		run, found, err := s.loadOptionalHeadlessRunUnlocked(id)
		if err != nil {
			return err
		}

		policy := headlessLogPolicyForWrite(options.Policy, run, found)

		private := options.Private || found && run.PrivateLogs
		if !private {
			text = RedactHeadlessText(text)
		}

		if err := s.appendHeadlessLogTextUnlocked(id, text, policy); err != nil {
			return err
		}

		if found && (run.Status == HeadlessStatusRunning || run.Status == HeadlessStatusOrphaned) {
			run.Status = HeadlessStatusRunning
			run.Stale = false
			run.OrphanedReason = ""
			run.LastHeartbeatAt = time.Now().UTC()

			return s.saveHeadlessRunUnlocked(run)
		}

		return nil
	})
}

// ReadHeadlessLog reads retained bounded log chunks for id.
func (s *Store) ReadHeadlessLog(id string) (string, error) {
	if err := validateHeadlessID(id); err != nil {
		return "", err
	}

	var text string

	err := s.withHeadlessLock(id, func() error {
		chunks, err := s.readableHeadlessLogChunksUnlocked(id)
		if err != nil {
			return err
		}

		if len(chunks) == 0 {
			return fmt.Errorf("session: read headless log %s: %w", s.headlessLogPath(id), os.ErrNotExist)
		}

		text, err = readHeadlessLogChunks(chunks)

		return err
	})
	if err != nil {
		return "", err
	}

	return text, nil
}

// TailHeadlessLog reads at most MaxBytes from retained logs beginning at Offset.
func (s *Store) TailHeadlessLog(id string, options HeadlessLogTailOptions) (HeadlessLogTail, error) {
	if err := validateHeadlessID(id); err != nil {
		return HeadlessLogTail{}, err
	}

	maxBytes := options.MaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultHeadlessLogTailBytes
	}

	var tail HeadlessLogTail

	err := s.withHeadlessLock(id, func() error {
		chunks, err := s.readableHeadlessLogChunksUnlocked(id)
		if err != nil {
			return err
		}

		tail, err = tailHeadlessLogChunks(chunks, options.Offset, maxBytes)

		return err
	})
	if err != nil {
		return HeadlessLogTail{}, err
	}

	return tail, nil
}

// AppendHeadlessEvent appends one structured lifecycle summary event for id.
func (s *Store) AppendHeadlessEvent(id string, event HeadlessEvent) error {
	if err := validateHeadlessID(id); err != nil {
		return err
	}

	return s.withHeadlessLock(id, func() error {
		return s.appendHeadlessEventUnlocked(id, event)
	})
}

// ReadHeadlessEvents reads structured lifecycle summary events for id.
func (s *Store) ReadHeadlessEvents(id string) ([]HeadlessEvent, error) {
	if err := validateHeadlessID(id); err != nil {
		return nil, err
	}

	var events []HeadlessEvent

	err := s.withHeadlessLock(id, func() error {
		data, err := os.ReadFile(s.headlessEventsPath(id))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				events = nil
				return nil
			}

			return fmt.Errorf("session: read headless events %s: %w", s.headlessEventsPath(id), err)
		}

		lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
		events = make([]HeadlessEvent, 0, len(lines))

		for i, line := range lines {
			if strings.TrimSpace(line) == "" {
				continue
			}

			var event HeadlessEvent
			if err := json.Unmarshal([]byte(line), &event); err != nil {
				return fmt.Errorf("session: parse headless event %s:%d: %w", s.headlessEventsPath(id), i+1, err)
			}

			if err := validateHeadlessEvent(id, event); err != nil {
				return fmt.Errorf("session: parse headless event %s:%d: %w", s.headlessEventsPath(id), i+1, err)
			}

			events = append(events, event)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return events, nil
}

// CleanupHeadlessLogs applies policy retention to a headless run's log chunks.
func (s *Store) CleanupHeadlessLogs(id string, policy HeadlessLogPolicy) error {
	if err := validateHeadlessID(id); err != nil {
		return err
	}

	policy = policy.normalized()

	return s.withHeadlessLock(id, func() error {
		if err := s.migrateLegacyHeadlessLogUnlocked(id, policy); err != nil {
			return err
		}

		return s.cleanupHeadlessLogChunksUnlocked(id, policy, time.Now().UTC())
	})
}

// RecoverStaleHeadlessRuns reconciles stale or orphaned running jobs and
// returns the records that changed.
func (s *Store) RecoverStaleHeadlessRuns(staleAfter time.Duration) ([]HeadlessRun, error) {
	if staleAfter <= 0 {
		staleAfter = defaultHeadlessStaleAfter
	}

	entries, err := os.ReadDir(s.headlessDir())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}

		return nil, fmt.Errorf("session: list headless %s: %w", s.headlessDir(), err)
	}

	recovered := make([]HeadlessRun, 0)
	now := time.Now().UTC()

	for _, entry := range entries {
		id, ok := headlessMetadataEntryID(entry)
		if !ok {
			continue
		}

		run, ok, err := s.recoverStaleHeadlessRun(id, now, staleAfter)
		if err != nil {
			if errors.Is(err, ErrCorruptHeadlessRun) {
				continue
			}

			return nil, err
		}

		if ok {
			recovered = append(recovered, run)
		}
	}

	return recovered, nil
}

func (s *Store) recoverStaleHeadlessRun(id string, now time.Time, staleAfter time.Duration) (HeadlessRun, bool, error) {
	var (
		recovered HeadlessRun
		ok        bool
	)

	err := s.withHeadlessLock(id, func() error {
		run, err := s.loadHeadlessRunUnlocked(id)
		if err != nil {
			return err
		}

		var reconcileErr error

		recovered, ok, reconcileErr = s.reconcileHeadlessRunUnlocked(run, now, staleAfter)
		if reconcileErr != nil {
			return reconcileErr
		}

		if !ok {
			return nil
		}

		return nil
	})
	if err != nil {
		return HeadlessRun{}, false, err
	}

	return recovered, ok, nil
}

func (s *Store) reconciledHeadlessRun(id string, now time.Time, staleAfter time.Duration) (HeadlessRun, error) {
	var run HeadlessRun

	err := s.withHeadlessLock(id, func() error {
		loaded, err := s.loadHeadlessRunUnlocked(id)
		if err != nil {
			return err
		}

		reconciled, _, err := s.reconcileHeadlessRunUnlocked(loaded, now, staleAfter)
		if err != nil {
			return err
		}

		run = reconciled

		return nil
	})
	if err != nil {
		return HeadlessRun{}, err
	}

	return run, nil
}

func (s *Store) reconcileHeadlessRunUnlocked(run HeadlessRun, now time.Time, staleAfter time.Duration) (HeadlessRun, bool, error) {
	status, reason, reconcile := headlessReconcileStatus(run, now, staleAfter)
	if !reconcile {
		return run, false, nil
	}

	var err error

	switch status {
	case HeadlessStatusOrphaned:
		run, err = s.markHeadlessRunOrphanedUnlocked(run, now, reason)
	default:
		run, err = s.markHeadlessRunStaleUnlocked(run, now, reason)
	}

	if err != nil {
		return HeadlessRun{}, false, err
	}

	return run, true, nil
}

func (s *Store) markHeadlessRunStaleUnlocked(run HeadlessRun, now time.Time, reason string) (HeadlessRun, error) {
	exitCode := 1
	run.Status = HeadlessStatusStale
	run.Stale = true
	run.StaleReason = reason
	run.OrphanedReason = ""
	run.CancellationReason = ""
	run.TerminalReason = reason
	run.CompletedAt = &now
	run.ExitCode = &exitCode

	saved, err := s.saveAndLoadHeadlessRunUnlocked(run)
	if err != nil {
		return HeadlessRun{}, err
	}

	run = saved

	line := "stale\t" + now.Format(time.RFC3339) + "\treason=" + reason + "\n"
	if err := s.appendHeadlessLogTextUnlocked(run.ID, headlessLifecycleLogLine(run, line), headlessLogPolicyForRun(run)); err != nil {
		return HeadlessRun{}, err
	}

	if err := s.appendHeadlessEventUnlocked(run.ID, headlessEventForRun(run, HeadlessEventStale, reason)); err != nil {
		return HeadlessRun{}, err
	}

	return run, nil
}

func (s *Store) markHeadlessRunOrphanedUnlocked(run HeadlessRun, now time.Time, reason string) (HeadlessRun, error) {
	run.Status = HeadlessStatusOrphaned
	run.Stale = true
	run.OrphanedReason = reason

	saved, err := s.saveAndLoadHeadlessRunUnlocked(run)
	if err != nil {
		return HeadlessRun{}, err
	}

	run = saved

	line := "orphaned\t" + now.Format(time.RFC3339) + "\treason=" + reason + "\n"
	if err := s.appendHeadlessLogTextUnlocked(run.ID, headlessLifecycleLogLine(run, line), headlessLogPolicyForRun(run)); err != nil {
		return HeadlessRun{}, err
	}

	if err := s.appendHeadlessEventUnlocked(run.ID, headlessEventForRun(run, HeadlessEventOrphaned, reason)); err != nil {
		return HeadlessRun{}, err
	}

	return run, nil
}

func (s *Store) saveAndLoadHeadlessRunUnlocked(run HeadlessRun) (HeadlessRun, error) {
	if err := s.saveHeadlessRunUnlocked(run); err != nil {
		return HeadlessRun{}, err
	}

	return s.loadHeadlessRunUnlocked(run.ID)
}

func (s *Store) corruptHeadlessRun(id string, err error) HeadlessRun {
	updated := time.Now().UTC()
	if info, statErr := os.Stat(s.headlessJSONPath(id)); statErr == nil {
		updated = info.ModTime().UTC()
	}

	return HeadlessRun{
		ID:         id,
		UpdatedAt:  updated,
		Status:     HeadlessStatusCorrupt,
		Error:      err.Error(),
		LogPath:    s.headlessLogPath(id),
		EventsPath: s.headlessEventsPath(id),
	}
}

func (s *Store) saveHeadlessRunUnlocked(run HeadlessRun) error {
	if err := validateHeadlessRunForSave(run); err != nil {
		return err
	}

	run, artifactDir := s.prepareHeadlessRunForSave(run)

	return s.writeHeadlessRunUnlocked(run, artifactDir)
}

func (s *Store) saveHeadlessRunMetadataOnlyUnlocked(run HeadlessRun) error {
	if err := validateHeadlessRunForSave(run); err != nil {
		return err
	}

	run, artifactDir := s.prepareHeadlessRunMetadataOnlyForSave(run)

	return s.writeHeadlessRunUnlocked(run, artifactDir)
}

func validateHeadlessRunForSave(run HeadlessRun) error {
	if !isPersistableHeadlessStatus(run.Status) {
		return fmt.Errorf("session: invalid headless status %q", run.Status)
	}

	return validateHeadlessRunRelationships(run)
}

func (s *Store) writeHeadlessRunUnlocked(run HeadlessRun, artifactDir string) error {
	dir := s.headlessDir()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("session: create headless dir: %w", err)
	}

	if artifactDir != "" {
		if err := os.MkdirAll(artifactDir, 0o750); err != nil {
			return fmt.Errorf("session: create headless artifact dir: %w", err)
		}
	}

	data, err := json.MarshalIndent(run, "", "  ")
	if err != nil {
		return fmt.Errorf("session: marshal headless run: %w", err)
	}

	data = append(data, '\n')

	path := s.headlessJSONPath(run.ID)

	tmp, err := os.CreateTemp(dir, headlessTempFilePrefix+"*.json")
	if err != nil {
		return fmt.Errorf("session: create headless temp: %w", err)
	}

	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("session: write headless temp: %w", err)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("session: close headless temp: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("session: replace headless %s: %w", path, err)
	}

	return nil
}

func (s *Store) prepareHeadlessRunForSave(run HeadlessRun) (prepared HeadlessRun, artifactDir string) {
	now := time.Now().UTC()
	run = s.prepareHeadlessRunMetadataOnlyForSaveAt(run, now)

	if run.Status == HeadlessStatusRunning {
		run = prepareRunningHeadlessRun(run, now)
	}

	artifactDir = run.ArtifactDir
	if !run.PrivateLogs {
		run = redactHeadlessRun(run)
	}

	return run, artifactDir
}

func (s *Store) prepareHeadlessRunMetadataOnlyForSave(run HeadlessRun) (prepared HeadlessRun, artifactDir string) {
	now := time.Now().UTC()
	run = s.prepareHeadlessRunMetadataOnlyForSaveAt(run, now)

	artifactDir = run.ArtifactDir
	if !run.PrivateLogs {
		run = redactHeadlessRun(run)
	}

	return run, artifactDir
}

func (s *Store) prepareHeadlessRunMetadataOnlyForSaveAt(run HeadlessRun, now time.Time) HeadlessRun {
	if run.StartedAt.IsZero() {
		run.StartedAt = now
	}

	run.UpdatedAt = now
	if run.LogPath == "" {
		run.LogPath = s.headlessLogPath(run.ID)
	}

	if run.EventsPath == "" {
		run.EventsPath = s.headlessEventsPath(run.ID)
	}

	if run.ArtifactDir == "" {
		run.ArtifactDir = s.headlessArtifactDir(run.ID)
	}

	policy := DefaultHeadlessLogPolicy()
	if run.LogMaxChunkBytes <= 0 {
		run.LogMaxChunkBytes = policy.MaxChunkBytes
	}

	if run.LogMaxChunks <= 0 {
		run.LogMaxChunks = policy.MaxChunks
	}

	run = normalizeHeadlessRunLifecycleFields(run)

	return run
}

func prepareRunningHeadlessRun(run HeadlessRun, now time.Time) HeadlessRun {
	if run.LastHeartbeatAt.IsZero() {
		run.LastHeartbeatAt = now
	}

	run = prepareRunningHeadlessHost(run)
	localHost := headlessHostnameIsLocal(run.Hostname)

	if localHost && run.Owner == "" {
		run.Owner = currentHeadlessOwner()
	}

	run = prepareRunningHeadlessProcess(run)

	if localHost {
		run = prepareRunningHeadlessCommand(run)
	}

	if localHost && run.CWD == "" {
		cwd, err := os.Getwd()
		if err == nil {
			run.CWD = cwd
		}
	}

	if localHost && run.StartMethod == "" {
		run.StartMethod = "foreground"
	}

	return run
}

func prepareRunningHeadlessHost(run HeadlessRun) HeadlessRun {
	if run.Hostname == "" {
		hostname, err := os.Hostname()
		if err == nil {
			run.Hostname = hostname
		}
	}

	return run
}

func prepareRunningHeadlessProcess(run HeadlessRun) HeadlessRun {
	localHost := headlessHostnameIsLocal(run.Hostname)
	defaultedPID := false

	if run.PID == 0 && localHost {
		run.PID = os.Getpid()
		defaultedPID = true
	}

	if run.ParentPID == 0 && defaultedPID {
		run.ParentPID = os.Getppid()
	}

	if run.ProcessGroupID == 0 && run.PID > 0 && localHost {
		run.ProcessGroupID = headlessProcessGroupID(run.PID)
	}

	return run
}

func prepareRunningHeadlessCommand(run HeadlessRun) HeadlessRun {
	if run.StartedCommand == "" {
		run.StartedCommand = strings.Join(os.Args, " ")
	}

	if len(run.CommandArgs) == 0 {
		run.CommandArgs = append([]string(nil), os.Args...)
	}

	return run
}

func normalizeHeadlessRunLifecycleFields(run HeadlessRun) HeadlessRun {
	if run.Status != HeadlessStatusCanceled {
		run.CanceledAt = nil
		run.CancellationReason = ""
	}

	switch run.Status {
	case HeadlessStatusRunning:
		run = clearHeadlessTerminalFields(run)
		run.Stale = false
		run.StaleReason = ""
		run.OrphanedReason = ""
	case HeadlessStatusOrphaned:
		run = clearHeadlessTerminalFields(run)
		run.Stale = true
		run.StaleReason = ""
	case HeadlessStatusStale:
		run.Stale = true
		run.OrphanedReason = ""
	default:
		run.Stale = false
		run.StaleReason = ""
		run.OrphanedReason = ""
	}

	return run
}

func validateHeadlessRunRelationships(run HeadlessRun) error {
	if run.ParentRunID != "" {
		if err := validateHeadlessID(run.ParentRunID); err != nil {
			return fmt.Errorf("session: invalid parent headless id %q: %w", run.ParentRunID, err)
		}

		if run.ParentRunID == run.ID {
			return fmt.Errorf("session: headless run %q cannot be its own parent", run.ID)
		}
	}

	seenChildren := make(map[string]struct{}, len(run.ChildRunIDs))
	for _, childID := range run.ChildRunIDs {
		if err := validateHeadlessID(childID); err != nil {
			return fmt.Errorf("session: invalid child headless id %q: %w", childID, err)
		}

		if childID == run.ID {
			return fmt.Errorf("session: headless run %q cannot be its own child", run.ID)
		}

		if _, ok := seenChildren[childID]; ok {
			return fmt.Errorf("session: duplicate child headless id %q", childID)
		}

		seenChildren[childID] = struct{}{}
	}

	return nil
}

func clearHeadlessTerminalFields(run HeadlessRun) HeadlessRun {
	run.CompletedAt = nil
	run.CanceledAt = nil
	run.ExitCode = nil
	run.Error = ""
	run.TerminalReason = ""

	return run
}

func redactHeadlessRun(run HeadlessRun) HeadlessRun {
	run.Prompt = RedactHeadlessText(run.Prompt)
	run.Error = RedactHeadlessText(run.Error)
	run.SessionPath = RedactHeadlessText(run.SessionPath)
	run.LogPath = RedactHeadlessText(run.LogPath)
	run.EventsPath = RedactHeadlessText(run.EventsPath)
	run.ArtifactDir = RedactHeadlessText(run.ArtifactDir)
	run.CWD = RedactHeadlessText(run.CWD)
	run.CancellationReason = RedactHeadlessText(run.CancellationReason)
	run.StaleReason = RedactHeadlessText(run.StaleReason)
	run.OrphanedReason = RedactHeadlessText(run.OrphanedReason)
	run.TerminalReason = RedactHeadlessText(run.TerminalReason)

	run.CommandArgs = redactHeadlessArgs(run.CommandArgs)
	run.StartedCommand = redactHeadlessStartedCommand(run.StartedCommand, run.CommandArgs)

	return run
}

func redactHeadlessStartedCommand(command string, args []string) string {
	if len(args) > 0 {
		return strings.Join(args, " ")
	}

	return RedactHeadlessText(command)
}

func redactHeadlessArgs(args []string) []string {
	if len(args) == 0 {
		return nil
	}

	redacted := make([]string, len(args))
	redactNext := false

	for i, arg := range args {
		if redactNext {
			redacted[i] = headlessLogRedactedValue
			redactNext = false

			continue
		}

		if sensitive := sensitiveHeadlessArg(arg); sensitive.ok {
			redacted[i] = sensitive.value
			redactNext = sensitive.redactFollowing

			continue
		}

		redacted[i] = RedactHeadlessText(arg)
	}

	return redacted
}

type headlessArgRedaction struct {
	value           string
	ok              bool
	redactFollowing bool
}

func sensitiveHeadlessArg(arg string) headlessArgRedaction {
	name := arg
	value := ""
	separator := ""

	if before, after, found := strings.Cut(arg, "="); found {
		name = before
		value = after
		separator = "="
	}

	name = strings.TrimLeft(name, "-")
	name = strings.ReplaceAll(name, "-", "_")

	if name == "" || !isSensitiveEnvName(name) {
		return headlessArgRedaction{}
	}

	if separator == "" {
		return headlessArgRedaction{ok: true, value: RedactHeadlessText(arg), redactFollowing: true}
	}

	return headlessArgRedaction{ok: true, value: strings.TrimSuffix(arg, separator+value) + separator + headlessLogRedactedValue}
}

func (s *Store) loadHeadlessRunUnlocked(id string) (HeadlessRun, error) {
	path := s.headlessJSONPath(id)

	data, err := os.ReadFile(path)
	if err != nil {
		return HeadlessRun{}, fmt.Errorf("session: read headless %s: %w", path, err)
	}

	var run HeadlessRun
	if err := json.Unmarshal(data, &run); err != nil {
		return HeadlessRun{}, fmt.Errorf("%w: parse headless %s: %w", ErrCorruptHeadlessRun, path, err)
	}

	if run.ID == "" {
		run.ID = idFromPath(path)
	} else if run.ID != id {
		return HeadlessRun{}, fmt.Errorf("%w: parse headless %s: metadata id %q does not match requested id %q", ErrCorruptHeadlessRun, path, run.ID, id)
	}

	if err := validateHeadlessID(run.ID); err != nil {
		return HeadlessRun{}, fmt.Errorf("%w: parse headless %s: invalid metadata id: %w", ErrCorruptHeadlessRun, path, err)
	}

	if !isPersistableHeadlessStatus(run.Status) {
		return HeadlessRun{}, fmt.Errorf("%w: parse headless %s: invalid status %q", ErrCorruptHeadlessRun, path, run.Status)
	}

	if err := validateHeadlessRunRelationships(run); err != nil {
		return HeadlessRun{}, fmt.Errorf("%w: parse headless %s: %w", ErrCorruptHeadlessRun, path, err)
	}

	return run, nil
}

func (s *Store) loadOptionalHeadlessRunUnlocked(id string) (HeadlessRun, bool, error) {
	run, err := s.loadHeadlessRunUnlocked(id)
	if err == nil {
		return run, true, nil
	}

	if errors.Is(err, os.ErrNotExist) {
		return HeadlessRun{}, false, nil
	}

	return HeadlessRun{}, false, err
}

func (s *Store) appendHeadlessEventUnlocked(id string, event HeadlessEvent) error {
	if err := os.MkdirAll(s.headlessDir(), 0o750); err != nil {
		return fmt.Errorf("session: create headless dir: %w", err)
	}

	private, err := s.headlessEventsArePrivateUnlocked(id)
	if err != nil {
		return err
	}

	event = prepareHeadlessEvent(id, event, private)
	if validateErr := validateHeadlessEvent(id, event); validateErr != nil {
		return validateErr
	}

	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("session: marshal headless event: %w", err)
	}

	data = append(data, '\n')

	path := s.headlessEventsPath(id)

	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("session: open headless events %s: %w", path, err)
	}
	defer file.Close()

	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("session: append headless events %s: %w", path, err)
	}

	return nil
}

func (s *Store) headlessEventsArePrivateUnlocked(id string) (bool, error) {
	return s.headlessPrivateLogsUnlocked(id)
}

func (s *Store) headlessPrivateLogsUnlocked(id string) (bool, error) {
	run, err := s.loadHeadlessRunUnlocked(id)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, ErrCorruptHeadlessRun) {
			return false, nil
		}

		return false, err
	}

	return run.PrivateLogs, nil
}

func prepareHeadlessEvent(id string, event HeadlessEvent, private bool) HeadlessEvent {
	if event.At.IsZero() {
		event.At = time.Now().UTC()
	}

	if event.RunID == "" {
		event.RunID = id
	}

	if private {
		return event
	}

	event.Message = RedactHeadlessText(event.Message)
	event.Error = RedactHeadlessText(event.Error)
	event.SessionPath = RedactHeadlessText(event.SessionPath)
	event.CWD = RedactHeadlessText(event.CWD)
	event.TerminalReason = RedactHeadlessText(event.TerminalReason)
	event.CancelReason = RedactHeadlessText(event.CancelReason)
	event.StaleReason = RedactHeadlessText(event.StaleReason)
	event.OrphanedReason = RedactHeadlessText(event.OrphanedReason)
	event.CommandArgs = redactHeadlessArgs(event.CommandArgs)
	event.StartedCommand = redactHeadlessStartedCommand(event.StartedCommand, event.CommandArgs)

	if event.Metadata != nil {
		metadata := make(map[string]string, len(event.Metadata))
		for key, value := range event.Metadata {
			metadata[key] = RedactHeadlessText(value)
		}

		event.Metadata = metadata
	}

	return event
}

func validateHeadlessEvent(id string, event HeadlessEvent) error {
	if !isKnownHeadlessEventType(event.Type) {
		return fmt.Errorf("session: invalid headless event type %q", event.Type)
	}

	if event.RunID == "" {
		return errors.New("session: headless event run_id is required")
	}

	if err := validateHeadlessID(event.RunID); err != nil {
		return fmt.Errorf("session: invalid headless event run_id %q: %w", event.RunID, err)
	}

	if event.RunID != id {
		return fmt.Errorf("session: headless event run_id %q does not match run %q", event.RunID, id)
	}

	if err := validateHeadlessEventLifecycle(event); err != nil {
		return err
	}

	if event.ParentRunID != "" {
		if err := validateHeadlessID(event.ParentRunID); err != nil {
			return fmt.Errorf("session: invalid headless event parent_run_id %q: %w", event.ParentRunID, err)
		}

		if event.ParentRunID == id {
			return fmt.Errorf("session: headless event run %q cannot be its own parent", id)
		}
	}

	seenChildren := make(map[string]struct{}, len(event.ChildRunIDs))
	for _, childID := range event.ChildRunIDs {
		if err := validateHeadlessID(childID); err != nil {
			return fmt.Errorf("session: invalid headless event child_run_id %q: %w", childID, err)
		}

		if childID == id {
			return fmt.Errorf("session: headless event run %q cannot be its own child", id)
		}

		if _, ok := seenChildren[childID]; ok {
			return fmt.Errorf("session: duplicate headless event child_run_id %q", childID)
		}

		seenChildren[childID] = struct{}{}
	}

	return nil
}

func validateHeadlessEventLifecycle(event HeadlessEvent) error {
	if event.Status != "" && !isKnownHeadlessStatus(event.Status) {
		return fmt.Errorf("session: invalid headless event status %q", event.Status)
	}

	if !headlessEventStatusMatchesType(event.Type, event.Status) {
		return fmt.Errorf("session: headless event type %q does not match status %q", event.Type, event.Status)
	}

	if !headlessEventRoleMatchesType(event.Type, event.Role) {
		return fmt.Errorf("session: headless event type %q does not match role %q", event.Type, event.Role)
	}

	return nil
}

func headlessEventForRun(run HeadlessRun, eventType HeadlessEventType, message string) HeadlessEvent {
	return HeadlessEvent{
		At:              time.Now().UTC(),
		RunID:           run.ID,
		ParentRunID:     run.ParentRunID,
		SessionID:       run.SessionID,
		SessionPath:     run.SessionPath,
		Type:            eventType,
		Status:          run.Status,
		Message:         message,
		Error:           run.Error,
		Agent:           run.Agent,
		Model:           run.Model,
		AgentLoopBudget: run.AgentLoopBudget,
		CWD:             run.CWD,
		Hostname:        run.Hostname,
		StartedCommand:  run.StartedCommand,
		StartMethod:     run.StartMethod,
		TerminalReason:  run.TerminalReason,
		CancelReason:    run.CancellationReason,
		StaleReason:     run.StaleReason,
		OrphanedReason:  run.OrphanedReason,
		ExitCode:        run.ExitCode,
		CommandArgs:     append([]string(nil), run.CommandArgs...),
		ChildRunIDs:     append([]string(nil), run.ChildRunIDs...),
		PID:             run.PID,
		ParentPID:       run.ParentPID,
		ProcessGroupID:  run.ProcessGroupID,
	}
}

func (s *Store) appendHeadlessLogTextUnlocked(id, text string, policy HeadlessLogPolicy) error {
	if text == "" {
		return nil
	}

	policy = policy.normalized()

	if err := os.MkdirAll(s.headlessDir(), 0o750); err != nil {
		return fmt.Errorf("session: create headless dir: %w", err)
	}

	if err := s.migrateLegacyHeadlessLogUnlocked(id, policy); err != nil {
		return err
	}

	if err := s.writeHeadlessLogChunksUnlocked(id, []byte(text), policy); err != nil {
		return err
	}

	return s.cleanupHeadlessLogChunksUnlocked(id, policy, time.Now().UTC())
}

func (s *Store) writeHeadlessLogChunksUnlocked(id string, data []byte, policy HeadlessLogPolicy) error {
	policy = policy.normalized()

	for len(data) > 0 {
		chunk, err := s.activeHeadlessLogChunkUnlocked(id)
		if err != nil {
			return err
		}

		if chunk.size >= policy.MaxChunkBytes {
			chunk = headlessLogChunk{index: chunk.index + 1, path: s.headlessLogChunkPath(id, chunk.index+1)}
		}

		space := policy.MaxChunkBytes - chunk.size
		if space <= 0 {
			continue
		}

		writeBytes := int(minInt64(space, int64(len(data))))
		if err := appendHeadlessLogChunk(chunk.path, data[:writeBytes]); err != nil {
			return err
		}

		if err := s.cleanupHeadlessLogChunksUnlocked(id, policy, time.Now().UTC()); err != nil {
			return err
		}

		data = data[writeBytes:]
	}

	return nil
}

func appendHeadlessLogChunk(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("session: open headless log %s: %w", path, err)
	}
	defer file.Close()

	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("session: append headless log %s: %w", path, err)
	}

	return nil
}

func (s *Store) activeHeadlessLogChunkUnlocked(id string) (headlessLogChunk, error) {
	chunks, err := s.headlessLogChunksUnlocked(id)
	if err != nil {
		return headlessLogChunk{}, err
	}

	if len(chunks) == 0 {
		return headlessLogChunk{index: 1, path: s.headlessLogChunkPath(id, 1)}, nil
	}

	return chunks[len(chunks)-1], nil
}

func (s *Store) cleanupHeadlessLogChunksUnlocked(id string, policy HeadlessLogPolicy, now time.Time) error {
	chunks, err := s.headlessLogChunksUnlocked(id)
	if err != nil {
		return err
	}

	remove := headlessLogChunksToRemove(chunks, policy, now)
	for _, chunk := range remove {
		if err := os.Remove(chunk.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("session: remove headless log chunk %s: %w", chunk.path, err)
		}
	}

	return nil
}

func headlessLogChunksToRemove(chunks []headlessLogChunk, policy HeadlessLogPolicy, now time.Time) []headlessLogChunk {
	if len(chunks) == 0 || len(chunks) == 1 && policy.MaxAge <= 0 {
		return nil
	}

	remove := make(map[int]struct{})

	if policy.MaxAge > 0 {
		cutoff := now.Add(-policy.MaxAge)
		for _, chunk := range chunks {
			if chunk.modTime.Before(cutoff) {
				remove[chunk.index] = struct{}{}
			}
		}
	}

	kept := make([]headlessLogChunk, 0, len(chunks))
	for _, chunk := range chunks {
		if _, ok := remove[chunk.index]; !ok {
			kept = append(kept, chunk)
		}
	}

	if len(kept) > policy.MaxChunks {
		excess := len(kept) - policy.MaxChunks
		for _, chunk := range kept[:excess] {
			remove[chunk.index] = struct{}{}
		}
	}

	removed := make([]headlessLogChunk, 0, len(remove))
	for _, chunk := range chunks {
		if _, ok := remove[chunk.index]; ok {
			removed = append(removed, chunk)
		}
	}

	return removed
}

func headlessMetadataEntryID(entry os.DirEntry) (string, bool) {
	name := entry.Name()
	if entry.IsDir() || filepath.Ext(name) != sessionFileExt {
		return "", false
	}

	if strings.HasPrefix(name, headlessTempFilePrefix) {
		return "", false
	}

	id := idFromPath(name)
	if err := validateHeadlessID(id); err != nil {
		return "", false
	}

	return id, true
}

func (s *Store) migrateLegacyHeadlessLogUnlocked(id string, policy HeadlessLogPolicy) error {
	policy = policy.normalized()
	legacyPath := s.headlessLogPath(id)

	info, err := os.Stat(legacyPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}

		return fmt.Errorf("session: stat legacy headless log %s: %w", legacyPath, err)
	}

	if info.IsDir() {
		return fmt.Errorf("session: legacy headless log %s is a directory", legacyPath)
	}

	private, err := s.headlessPrivateLogsUnlocked(id)
	if err != nil {
		return err
	}

	if err := s.copyLegacyHeadlessLogTailUnlocked(id, legacyPath, info.Size(), policy, private); err != nil {
		return err
	}

	if err := os.Remove(legacyPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("session: remove legacy headless log %s: %w", legacyPath, err)
	}

	return nil
}

func (s *Store) copyLegacyHeadlessLogTailUnlocked(id, legacyPath string, size int64, policy HeadlessLogPolicy, private bool) error {
	if size <= 0 {
		return nil
	}

	limit := headlessLegacyMigrationLimit(policy)

	start := int64(0)
	if size > limit {
		start = size - limit
	}

	file, err := os.Open(legacyPath)
	if err != nil {
		return fmt.Errorf("session: open legacy headless log %s: %w", legacyPath, err)
	}
	defer file.Close()

	if _, seekErr := file.Seek(start, io.SeekStart); seekErr != nil {
		return fmt.Errorf("session: seek legacy headless log %s: %w", legacyPath, seekErr)
	}

	data, err := io.ReadAll(io.LimitReader(file, limit))
	if err != nil {
		return fmt.Errorf("session: read legacy headless log %s: %w", legacyPath, err)
	}

	text := string(data)
	if !private {
		text = RedactHeadlessText(text)
	}

	if err := s.writeHeadlessLogChunksUnlocked(id, []byte(text), policy); err != nil {
		return err
	}

	return nil
}

func (s *Store) readableHeadlessLogChunksUnlocked(id string) ([]headlessLogChunk, error) {
	policy, found, err := s.headlessLogPolicyForIDUnlocked(id)
	if err != nil {
		return nil, err
	}

	if migrateErr := s.migrateLegacyHeadlessLogUnlocked(id, policy); migrateErr != nil {
		return nil, migrateErr
	}

	if found {
		if cleanupErr := s.cleanupHeadlessLogChunksUnlocked(id, policy, time.Now().UTC()); cleanupErr != nil {
			return nil, cleanupErr
		}
	}

	chunks, err := s.headlessLogChunksUnlocked(id)
	if err != nil || len(chunks) > 0 {
		return chunks, err
	}

	info, err := os.Stat(s.headlessLogPath(id))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}

		return nil, fmt.Errorf("session: stat headless log %s: %w", s.headlessLogPath(id), err)
	}

	if info.IsDir() {
		return nil, fmt.Errorf("session: headless log %s is a directory", s.headlessLogPath(id))
	}

	return []headlessLogChunk{{index: 1, path: s.headlessLogPath(id), size: info.Size(), modTime: info.ModTime()}}, nil
}

func (s *Store) headlessLogChunksUnlocked(id string) ([]headlessLogChunk, error) {
	entries, err := os.ReadDir(s.headlessDir())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}

		return nil, fmt.Errorf("session: list headless logs %s: %w", s.headlessDir(), err)
	}

	prefix := id + headlessLogChunkPrefix
	chunks := make([]headlessLogChunk, 0)

	for _, entry := range entries {
		chunk, ok, err := s.headlessLogChunkFromDirEntry(prefix, entry)
		if err != nil {
			return nil, err
		}

		if ok {
			chunks = append(chunks, chunk)
		}
	}

	sort.Slice(chunks, func(i, j int) bool {
		return chunks[i].index < chunks[j].index
	})

	return chunks, nil
}

func (s *Store) headlessLogChunkFromDirEntry(prefix string, entry os.DirEntry) (headlessLogChunk, bool, error) {
	name := entry.Name()
	if entry.IsDir() || !strings.HasPrefix(name, prefix) {
		return headlessLogChunk{}, false, nil
	}

	suffix := strings.TrimPrefix(name, prefix)
	if len(suffix) != headlessLogChunkSuffixWidth || strings.Trim(suffix, "0123456789") != "" {
		return headlessLogChunk{}, false, nil
	}

	index, err := strconv.Atoi(suffix)
	if err != nil {
		return headlessLogChunk{}, false, fmt.Errorf("session: parse headless log chunk %s: %w", name, err)
	}

	info, err := entry.Info()
	if err != nil {
		return headlessLogChunk{}, false, fmt.Errorf("session: stat headless log chunk %s: %w", name, err)
	}

	return headlessLogChunk{
		index:   index,
		path:    filepath.Join(s.headlessDir(), name),
		size:    info.Size(),
		modTime: info.ModTime(),
	}, true, nil
}

func readHeadlessLogChunks(chunks []headlessLogChunk) (string, error) {
	var builder strings.Builder

	for _, chunk := range chunks {
		data, err := os.ReadFile(chunk.path)
		if err != nil {
			return "", fmt.Errorf("session: read headless log chunk %s: %w", chunk.path, err)
		}

		builder.Write(data)
	}

	return builder.String(), nil
}

func tailHeadlessLogChunks(chunks []headlessLogChunk, offset HeadlessLogOffset, maxBytes int64) (HeadlessLogTail, error) {
	tail := HeadlessLogTail{NextOffset: offset}
	if len(chunks) == 0 || maxBytes <= 0 {
		return tail, nil
	}

	tail.RetainedOffset = HeadlessLogOffset{Chunk: chunks[0].index}
	start := normalizeTailStart(chunks, offset)
	tail.Truncated = start.truncated
	tail.NextOffset = HeadlessLogOffset{Chunk: start.chunk, Byte: start.byte}

	var builder strings.Builder

	remaining := maxBytes

	for _, chunk := range chunks {
		if chunk.index < start.chunk || remaining <= 0 {
			continue
		}

		chunkOffset := int64(0)
		if chunk.index == start.chunk {
			chunkOffset = start.byte
		}

		data, err := readHeadlessLogChunkRange(chunk, chunkOffset, remaining)
		if err != nil {
			return HeadlessLogTail{}, err
		}

		builder.Write(data)
		remaining -= int64(len(data))
		tail.NextOffset = HeadlessLogOffset{Chunk: chunk.index, Byte: chunkOffset + int64(len(data))}
	}

	tail.Text = builder.String()

	return tail, nil
}

type headlessTailStart struct {
	chunk     int
	byte      int64
	truncated bool
}

func normalizeTailStart(chunks []headlessLogChunk, offset HeadlessLogOffset) headlessTailStart {
	start := headlessTailStart{chunk: offset.Chunk, byte: offset.Byte}
	if start.chunk <= 0 {
		start.chunk = chunks[0].index
		start.byte = 0
	}

	if start.byte < 0 {
		start.byte = 0
	}

	if start.chunk < chunks[0].index {
		start.chunk = chunks[0].index
		start.byte = 0
		start.truncated = true
	}

	return start
}

func readHeadlessLogChunkRange(chunk headlessLogChunk, offset, maxBytes int64) ([]byte, error) {
	if offset >= chunk.size || maxBytes <= 0 {
		return nil, nil
	}

	readBytes := minInt64(chunk.size-offset, maxBytes)

	file, err := os.Open(chunk.path)
	if err != nil {
		return nil, fmt.Errorf("session: open headless log chunk %s: %w", chunk.path, err)
	}
	defer file.Close()

	buffer := make([]byte, int(readBytes))

	read, err := file.ReadAt(buffer, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("session: read headless log chunk %s: %w", chunk.path, err)
	}

	return buffer[:read], nil
}

func markHeadlessRunCanceled(run HeadlessRun, now time.Time, reason string) HeadlessRun {
	exitCode := 130
	run.Status = HeadlessStatusCanceled
	run.CanceledAt = &now
	run.CompletedAt = &now
	run.CancellationReason = reason
	run.TerminalReason = reason
	run.Error = reason
	run.ExitCode = &exitCode

	return run
}

func terminateHeadlessRunProcess(run HeadlessRun) error {
	if run.PID <= 0 {
		return nil
	}

	if !headlessPIDIsLocal(run) {
		return fmt.Errorf("process pid %d is on host %q", run.PID, run.Hostname)
	}

	if run.PID == os.Getpid() {
		return fmt.Errorf("refusing to signal current process pid %d", run.PID)
	}

	if !headlessProcessAlive(run.PID) {
		if headlessRecordedProcessGroupIsLiveLocal(run) {
			return signalHeadlessProcessGroup(run.PID, run.ProcessGroupID)
		}

		return nil
	}

	if run.ProcessGroupID > 0 {
		processGroupID := headlessProcessGroupID(run.PID)
		if processGroupID > 0 && processGroupID != run.ProcessGroupID {
			return fmt.Errorf("refusing to signal process pid %d: process group changed from %d to %d", run.PID, run.ProcessGroupID, processGroupID)
		}

		if headlessCanSignalRecordedProcessGroup(run) {
			return signalHeadlessProcessGroup(run.PID, run.ProcessGroupID)
		}
	}

	return signalHeadlessProcess(run.PID)
}

func headlessReconcileStatus(run HeadlessRun, now time.Time, staleAfter time.Duration) (status HeadlessStatus, reason string, reconcile bool) {
	if run.Status == HeadlessStatusOrphaned {
		if unavailable, unavailableReason := headlessProcessUnavailableReason(run); unavailable {
			return HeadlessStatusStale, unavailableReason, true
		}

		return "", "", false
	}

	if run.Status != HeadlessStatusRunning {
		return "", "", false
	}

	if headlessRecordedProcessExitedButGroupIsLiveLocal(run) {
		return HeadlessStatusOrphaned, fmt.Sprintf("process pid %d exited but process group %d is still running", run.PID, run.ProcessGroupID), true
	}

	if unavailable, unavailableReason := headlessProcessUnavailableReason(run); unavailable {
		return HeadlessStatusStale, unavailableReason, true
	}

	stale, reason := headlessHeartbeatStaleReason(headlessLastActivity(run), now, staleAfter)
	if !stale {
		return "", "", false
	}

	if headlessRecordedProcessIsLiveLocal(run) {
		return HeadlessStatusOrphaned, reason, true
	}

	return HeadlessStatusStale, reason, true
}

func headlessHeartbeatStaleReason(activity, now time.Time, staleAfter time.Duration) (stale bool, reason string) {
	if activity.IsZero() {
		return true, "no heartbeat recorded"
	}

	if now.Sub(activity) <= staleAfter {
		return false, ""
	}

	return true, "no heartbeat since " + activity.UTC().Format(time.RFC3339)
}

func headlessProcessUnavailableReason(run HeadlessRun) (unavailable bool, reason string) {
	if run.PID <= 0 {
		return true, "no process pid recorded"
	}

	if !headlessPIDIsLocal(run) {
		return false, ""
	}

	if !headlessProcessAlive(run.PID) {
		if headlessRecordedProcessGroupIsLiveLocal(run) {
			return false, ""
		}

		return true, fmt.Sprintf("process pid %d is not running", run.PID)
	}

	if run.ProcessGroupID > 0 {
		processGroupID := headlessProcessGroupID(run.PID)
		if processGroupID > 0 && processGroupID != run.ProcessGroupID {
			return true, fmt.Sprintf("process pid %d moved from process group %d to %d", run.PID, run.ProcessGroupID, processGroupID)
		}
	}

	return false, ""
}

func headlessRecordedProcessIsLiveLocal(run HeadlessRun) bool {
	if run.PID <= 0 || !headlessPIDIsLocal(run) {
		return false
	}

	unavailable, _ := headlessProcessUnavailableReason(run)

	return !unavailable
}

func headlessRecordedProcessGroupIsLiveLocal(run HeadlessRun) bool {
	if !headlessCanSignalRecordedProcessGroup(run) || !headlessPIDIsLocal(run) {
		return false
	}

	return headlessProcessGroupAlive(run.ProcessGroupID)
}

func headlessRecordedProcessExitedButGroupIsLiveLocal(run HeadlessRun) bool {
	if run.PID <= 0 || !headlessPIDIsLocal(run) {
		return false
	}

	return !headlessProcessAlive(run.PID) && headlessRecordedProcessGroupIsLiveLocal(run)
}

func headlessCanSignalRecordedProcessGroup(run HeadlessRun) bool {
	if run.ProcessGroupID <= 0 || run.ProcessGroupID != run.PID {
		return false
	}

	currentProcessGroupID := headlessProcessGroupID(os.Getpid())

	return currentProcessGroupID > 0 && currentProcessGroupID != run.ProcessGroupID
}

func isTerminalHeadlessStatus(status HeadlessStatus) bool {
	switch status {
	case HeadlessStatusCompleted, HeadlessStatusFailed, HeadlessStatusCanceled, HeadlessStatusTimedOut, HeadlessStatusStale, HeadlessStatusSuperseded, HeadlessStatusCorrupt:
		return true
	default:
		return false
	}
}

func isKnownHeadlessStatus(status HeadlessStatus) bool {
	switch status {
	case HeadlessStatusRunning,
		HeadlessStatusCompleted,
		HeadlessStatusFailed,
		HeadlessStatusCanceled,
		HeadlessStatusTimedOut,
		HeadlessStatusStale,
		HeadlessStatusOrphaned,
		HeadlessStatusSuperseded,
		HeadlessStatusCorrupt:
		return true
	default:
		return false
	}
}

func isPersistableHeadlessStatus(status HeadlessStatus) bool {
	return isKnownHeadlessStatus(status) && status != HeadlessStatusCorrupt
}

func isKnownHeadlessEventType(eventType HeadlessEventType) bool {
	switch eventType {
	case HeadlessEventStarted,
		HeadlessEventUserMessage,
		HeadlessEventAssistantMessage,
		HeadlessEventCompleted,
		HeadlessEventFailed,
		HeadlessEventCanceled,
		HeadlessEventTimedOut,
		HeadlessEventStale,
		HeadlessEventOrphaned,
		HeadlessEventSuperseded:
		return true
	default:
		return false
	}
}

func headlessEventStatusMatchesType(eventType HeadlessEventType, status HeadlessStatus) bool {
	if status == "" {
		return true
	}

	switch eventType {
	case HeadlessEventStarted, HeadlessEventUserMessage, HeadlessEventAssistantMessage:
		return status == HeadlessStatusRunning || status == HeadlessStatusOrphaned
	case HeadlessEventCompleted:
		return status == HeadlessStatusCompleted
	case HeadlessEventFailed:
		return status == HeadlessStatusFailed
	case HeadlessEventCanceled:
		return status == HeadlessStatusCanceled
	case HeadlessEventTimedOut:
		return status == HeadlessStatusTimedOut
	case HeadlessEventStale:
		return status == HeadlessStatusStale
	case HeadlessEventOrphaned:
		return status == HeadlessStatusOrphaned
	case HeadlessEventSuperseded:
		return status == HeadlessStatusSuperseded
	default:
		return false
	}
}

func headlessEventRoleMatchesType(eventType HeadlessEventType, role string) bool {
	if role == "" {
		return true
	}

	switch eventType {
	case HeadlessEventUserMessage:
		return role == headlessEventRoleUser
	case HeadlessEventAssistantMessage:
		return role == headlessEventRoleAssistant
	default:
		return false
	}
}

func headlessPIDIsLocal(run HeadlessRun) bool {
	if run.PID <= 0 {
		return false
	}

	return headlessHostnameIsLocal(run.Hostname)
}

func headlessHostnameIsLocal(hostname string) bool {
	if strings.TrimSpace(hostname) == "" {
		return true
	}

	localHostname, err := os.Hostname()
	if err != nil || strings.TrimSpace(localHostname) == "" {
		return false
	}

	return strings.EqualFold(hostname, localHostname)
}

func headlessLastActivity(run HeadlessRun) time.Time {
	if !run.LastHeartbeatAt.IsZero() {
		return run.LastHeartbeatAt
	}

	if !run.UpdatedAt.IsZero() {
		return run.UpdatedAt
	}

	return run.StartedAt
}

func headlessLegacyMigrationLimit(policy HeadlessLogPolicy) int64 {
	policy = policy.normalized()
	if policy.MaxChunkBytes <= 0 || policy.MaxChunks <= 0 {
		return defaultHeadlessLegacyMigrationBytes
	}

	if policy.MaxChunkBytes > defaultHeadlessLegacyMigrationBytes {
		return defaultHeadlessLegacyMigrationBytes
	}

	maxChunks := int64(policy.MaxChunks)
	if maxChunks > defaultHeadlessLegacyMigrationBytes/policy.MaxChunkBytes {
		return defaultHeadlessLegacyMigrationBytes
	}

	limit := policy.MaxChunkBytes * maxChunks
	if limit <= 0 || limit > defaultHeadlessLegacyMigrationBytes {
		return defaultHeadlessLegacyMigrationBytes
	}

	return limit
}

func headlessLogPolicyForWrite(policy HeadlessLogPolicy, run HeadlessRun, found bool) HeadlessLogPolicy {
	if !found {
		return policy.normalized()
	}

	if policy.MaxChunkBytes <= 0 {
		policy.MaxChunkBytes = run.LogMaxChunkBytes
	}

	if policy.MaxChunks <= 0 {
		policy.MaxChunks = run.LogMaxChunks
	}

	return policy.normalized()
}

func headlessLogPolicyForRun(run HeadlessRun) HeadlessLogPolicy {
	return headlessLogPolicyForWrite(HeadlessLogPolicy{}, run, true)
}

func (s *Store) headlessLogPolicyForIDUnlocked(id string) (HeadlessLogPolicy, bool, error) {
	run, found, err := s.loadOptionalHeadlessRunUnlocked(id)
	if err != nil {
		if errors.Is(err, ErrCorruptHeadlessRun) {
			return DefaultHeadlessLogPolicy(), false, nil
		}

		return HeadlessLogPolicy{}, false, err
	}

	if !found {
		return DefaultHeadlessLogPolicy(), false, nil
	}

	return headlessLogPolicyForRun(run), true, nil
}

func (p HeadlessLogPolicy) normalized() HeadlessLogPolicy {
	defaults := DefaultHeadlessLogPolicy()
	if p.MaxChunkBytes <= 0 {
		p.MaxChunkBytes = defaults.MaxChunkBytes
	}

	if p.MaxChunks <= 0 {
		p.MaxChunks = defaults.MaxChunks
	}

	if p.MaxAge < 0 {
		p.MaxAge = 0
	}

	return p
}

func redactPatternSecrets(text string) string {
	redacted := headlessBearerSecretRE.ReplaceAllString(text, `${1}`+headlessLogRedactedValue)
	redacted = headlessAssignmentSecretRE.ReplaceAllString(redacted, `${1}${2}`+headlessLogRedactedValue+`${4}`)
	redacted = headlessFlagSecretRE.ReplaceAllString(redacted, `${1}${2}${3}`+headlessLogRedactedValue+`${5}`)
	redacted = headlessOpenAISecretRE.ReplaceAllString(redacted, headlessLogRedactedValue)
	redacted = headlessGitHubSecretRE.ReplaceAllString(redacted, headlessLogRedactedValue)
	redacted = headlessAWSAccessKeyRE.ReplaceAllString(redacted, headlessLogRedactedValue)

	return redacted
}

func headlessSecretEnvValues() []string {
	values := make([]string, 0)

	for _, env := range os.Environ() {
		key, value, found := strings.Cut(env, "=")
		if !found || !isSensitiveEnvName(key) || len(value) < 8 {
			continue
		}

		values = append(values, value)
	}

	sort.Slice(values, func(i, j int) bool {
		return len(values[i]) > len(values[j])
	})

	return values
}

func isSensitiveEnvName(name string) bool {
	name = strings.ToUpper(name)

	sensitiveParts := []string{"KEY", "TOKEN", "SECRET", "PASSWORD", "PASS", "CREDENTIAL", "AUTH"}
	for _, part := range sensitiveParts {
		if strings.Contains(name, part) {
			return true
		}
	}

	return false
}

func currentHeadlessOwner() string {
	current, err := user.Current()
	if err != nil {
		return ""
	}

	if current.Username != "" {
		return current.Username
	}

	if current.Uid != "" {
		return "uid:" + current.Uid
	}

	return ""
}

func validateHeadlessID(id string) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("session: headless id is required")
	}

	if strings.TrimSpace(id) != id {
		return errors.New("session: headless id must not have leading or trailing whitespace")
	}

	if strings.ContainsAny(id, `/\<>:"|?*`) {
		return errors.New("session: headless id must be a file name")
	}

	if id == "." || id == ".." || filepath.Base(id) != id {
		return errors.New("session: headless id must be a file name")
	}

	if strings.ContainsFunc(id, unicode.IsControl) {
		return errors.New("session: headless id must be a file name")
	}

	if strings.HasPrefix(id, headlessTempFilePrefix) {
		return fmt.Errorf("session: headless id cannot start with reserved prefix %q", headlessTempFilePrefix)
	}

	return nil
}

func (s *Store) withHeadlessLock(id string, fn func() error) (lockErr error) {
	if err := os.MkdirAll(s.headlessDir(), 0o750); err != nil {
		return fmt.Errorf("session: create headless dir: %w", err)
	}

	file, err := os.OpenFile(s.headlessLockPath(id), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("session: open headless lock: %w", err)
	}
	defer file.Close()

	if err := lockHeadlessFile(file); err != nil {
		return err
	}

	defer func() {
		if unlockErr := unlockHeadlessFile(file); lockErr == nil && unlockErr != nil {
			lockErr = unlockErr
		}
	}()

	return fn()
}

func (s *Store) headlessDir() string {
	return filepath.Join(s.dir, headlessDirName)
}

func (s *Store) headlessJSONPath(id string) string {
	return filepath.Join(s.headlessDir(), id+sessionFileExt)
}

func (s *Store) headlessLogPath(id string) string {
	return filepath.Join(s.headlessDir(), id+".log")
}

func (s *Store) headlessEventsPath(id string) string {
	return filepath.Join(s.headlessDir(), id+".events.jsonl")
}

func (s *Store) headlessLogChunkPath(id string, index int) string {
	return fmt.Sprintf("%s.%0*d", s.headlessLogPath(id), headlessLogChunkSuffixWidth, index)
}

func (s *Store) headlessLockPath(id string) string {
	return filepath.Join(s.headlessDir(), id+".lock")
}

func (s *Store) headlessArtifactDir(id string) string {
	return filepath.Join(s.headlessDir(), id+"-artifacts")
}

func minInt64(left, right int64) int64 {
	if left < right {
		return left
	}

	return right
}

func stringSliceContains(values []string, needle string) bool {
	return slices.Contains(values, needle)
}

func mergeHeadlessChildRunIDs(current, incoming []string) []string {
	if len(current) == 0 && len(incoming) == 0 {
		return nil
	}

	merged := make([]string, 0, len(current)+len(incoming))
	for _, id := range append(append([]string(nil), current...), incoming...) {
		if id == "" || stringSliceContains(merged, id) {
			continue
		}

		merged = append(merged, id)
	}

	return merged
}
