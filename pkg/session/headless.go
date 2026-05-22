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
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	headlessDirName = "headless"

	headlessLogChunkSuffixWidth = 6
	headlessLogChunkPrefix      = ".log."
	headlessLogRedactedValue    = "[REDACTED]"

	defaultHeadlessLogMaxChunkBytes     int64 = 1024 * 1024
	defaultHeadlessLogMaxChunks               = 8
	defaultHeadlessLogTailBytes         int64 = 64 * 1024
	defaultHeadlessLegacyMigrationBytes       = defaultHeadlessLogMaxChunkBytes * defaultHeadlessLogMaxChunks
	defaultHeadlessStaleAfter                 = 30 * time.Minute
)

var (
	headlessAssignmentSecretRE = regexp.MustCompile(`(?i)\b([a-z0-9_.-]*(?:api[_-]?key|token|secret|password|passwd|pwd|credential|authorization)[a-z0-9_.-]*\s*[:=]\s*)(["']?)([^\s"']+)(["']?)`)
	headlessBearerSecretRE     = regexp.MustCompile(`(?i)\b(Bearer\s+)([A-Za-z0-9._~+/=-]{8,})`)
	headlessOpenAISecretRE     = regexp.MustCompile(`\b(sk-[A-Za-z0-9][A-Za-z0-9_-]{12,})\b`)
	headlessGitHubSecretRE     = regexp.MustCompile(`\b(gh[pousr]_[A-Za-z0-9_]{12,})\b`)
	headlessAWSAccessKeyRE     = regexp.MustCompile(`\b(AKIA[0-9A-Z]{16})\b`)
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
	// HeadlessStatusStale indicates a previously running run no longer appears healthy.
	HeadlessStatusStale HeadlessStatus = "stale"
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

// HeadlessRun records metadata for an atteler headless execution.
//
//nolint:govet // JSON metadata is grouped by lifecycle and operator-facing fields; padding is irrelevant.
type HeadlessRun struct {
	StartedAt          time.Time      `json:"started_at"`
	UpdatedAt          time.Time      `json:"updated_at"`
	LastHeartbeatAt    time.Time      `json:"last_heartbeat_at,omitzero"`
	CompletedAt        *time.Time     `json:"completed_at,omitempty"`
	ExitCode           *int           `json:"exit_code,omitempty"`
	ID                 string         `json:"id"`
	SessionID          string         `json:"session_id"`
	SessionPath        string         `json:"session_path"`
	LogPath            string         `json:"log_path"`
	ArtifactDir        string         `json:"artifact_dir,omitempty"`
	Prompt             string         `json:"prompt"`
	Model              string         `json:"model"`
	Agent              string         `json:"agent"`
	Owner              string         `json:"owner,omitempty"`
	Hostname           string         `json:"hostname,omitempty"`
	StartedCommand     string         `json:"started_command,omitempty"`
	CancellationReason string         `json:"cancellation_reason,omitempty"`
	StaleReason        string         `json:"stale_reason,omitempty"`
	Error              string         `json:"error"`
	PID                int            `json:"pid,omitempty"`
	Status             HeadlessStatus `json:"status"`
	PrivateLogs        bool           `json:"private_logs,omitempty"`
	Stale              bool           `json:"stale,omitempty"`
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
		if entry.IsDir() || filepath.Ext(entry.Name()) != sessionFileExt {
			continue
		}

		run, err := s.LoadHeadlessRun(idFromPath(entry.Name()))
		if err != nil {
			return nil, err
		}

		annotateHeadlessStale(&run, now, defaultHeadlessStaleAfter)
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

// AppendHeadlessLog appends redacted text to the headless run log for id.
func (s *Store) AppendHeadlessLog(id, text string) error {
	return s.AppendHeadlessLogWithOptions(id, text, HeadlessLogWriteOptions{})
}

// AppendHeadlessLogWithOptions appends text to a bounded headless log.
func (s *Store) AppendHeadlessLogWithOptions(id, text string, options HeadlessLogWriteOptions) error {
	if err := validateHeadlessID(id); err != nil {
		return err
	}

	policy := options.Policy.normalized()

	return s.withHeadlessLock(id, func() error {
		run, found, err := s.loadOptionalHeadlessRunUnlocked(id)
		if err != nil {
			return err
		}

		private := options.Private || found && run.PrivateLogs
		if !private {
			text = RedactHeadlessText(text)
		}

		if err := s.appendHeadlessLogTextUnlocked(id, text, policy); err != nil {
			return err
		}

		if found && run.Status == HeadlessStatusRunning {
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

// RecoverStaleHeadlessRuns marks stale running jobs as stale and returns the recovered runs.
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
		if entry.IsDir() || filepath.Ext(entry.Name()) != sessionFileExt {
			continue
		}

		run, ok, err := s.recoverStaleHeadlessRun(idFromPath(entry.Name()), now, staleAfter)
		if err != nil {
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

		stale, reason := headlessStaleReason(run, now, staleAfter)
		if !stale {
			return nil
		}

		exitCode := 1
		run.Status = HeadlessStatusStale
		run.Stale = true
		run.StaleReason = reason
		run.CancellationReason = reason
		run.CompletedAt = &now

		run.ExitCode = &exitCode
		if err := s.saveHeadlessRunUnlocked(run); err != nil {
			return err
		}

		line := "stale\t" + now.Format(time.RFC3339) + "\treason=" + reason + "\n"
		if err := s.appendHeadlessLogTextUnlocked(id, RedactHeadlessText(line), DefaultHeadlessLogPolicy()); err != nil {
			return err
		}

		recovered = run
		ok = true

		return nil
	})
	if err != nil {
		return HeadlessRun{}, false, err
	}

	return recovered, ok, nil
}

func (s *Store) saveHeadlessRunUnlocked(run HeadlessRun) error {
	run = s.prepareHeadlessRunForSave(run)

	dir := s.headlessDir()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("session: create headless dir: %w", err)
	}

	if run.ArtifactDir != "" {
		if err := os.MkdirAll(run.ArtifactDir, 0o750); err != nil {
			return fmt.Errorf("session: create headless artifact dir: %w", err)
		}
	}

	data, err := json.MarshalIndent(run, "", "  ")
	if err != nil {
		return fmt.Errorf("session: marshal headless run: %w", err)
	}

	data = append(data, '\n')

	path := s.headlessJSONPath(run.ID)

	tmp, err := os.CreateTemp(dir, ".headless-*.json")
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

func (s *Store) prepareHeadlessRunForSave(run HeadlessRun) HeadlessRun {
	now := time.Now().UTC()
	if run.StartedAt.IsZero() {
		run.StartedAt = now
	}

	run.UpdatedAt = now
	if run.LogPath == "" {
		run.LogPath = s.headlessLogPath(run.ID)
	}

	if run.ArtifactDir == "" {
		run.ArtifactDir = s.headlessArtifactDir(run.ID)
	}

	if run.Status == HeadlessStatusRunning {
		run = prepareRunningHeadlessRun(run, now)
	}

	if !run.PrivateLogs {
		run = redactHeadlessRun(run)
	}

	return run
}

func prepareRunningHeadlessRun(run HeadlessRun, now time.Time) HeadlessRun {
	if run.LastHeartbeatAt.IsZero() {
		run.LastHeartbeatAt = now
	}

	if run.PID == 0 {
		run.PID = os.Getpid()
	}

	if run.Owner == "" {
		run.Owner = currentHeadlessOwner()
	}

	if run.Hostname == "" {
		hostname, err := os.Hostname()
		if err == nil {
			run.Hostname = hostname
		}
	}

	if run.StartedCommand == "" {
		run.StartedCommand = strings.Join(os.Args, " ")
	}

	return run
}

func redactHeadlessRun(run HeadlessRun) HeadlessRun {
	run.Prompt = RedactHeadlessText(run.Prompt)
	run.Error = RedactHeadlessText(run.Error)
	run.StartedCommand = RedactHeadlessText(run.StartedCommand)
	run.CancellationReason = RedactHeadlessText(run.CancellationReason)
	run.StaleReason = RedactHeadlessText(run.StaleReason)

	return run
}

func (s *Store) loadHeadlessRunUnlocked(id string) (HeadlessRun, error) {
	path := s.headlessJSONPath(id)

	data, err := os.ReadFile(path)
	if err != nil {
		return HeadlessRun{}, fmt.Errorf("session: read headless %s: %w", path, err)
	}

	var run HeadlessRun
	if err := json.Unmarshal(data, &run); err != nil {
		return HeadlessRun{}, fmt.Errorf("session: parse headless %s: %w", path, err)
	}

	if run.ID == "" {
		run.ID = idFromPath(path)
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

func (s *Store) appendHeadlessLogTextUnlocked(id, text string, policy HeadlessLogPolicy) error {
	if text == "" {
		return nil
	}

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

	if err := s.copyLegacyHeadlessLogTailUnlocked(id, legacyPath, info.Size(), policy); err != nil {
		return err
	}

	if err := os.Remove(legacyPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("session: remove legacy headless log %s: %w", legacyPath, err)
	}

	return nil
}

func (s *Store) copyLegacyHeadlessLogTailUnlocked(id, legacyPath string, size int64, policy HeadlessLogPolicy) error {
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

	redacted := RedactHeadlessText(string(data))
	if err := s.writeHeadlessLogChunksUnlocked(id, []byte(redacted), policy); err != nil {
		return err
	}

	return nil
}

func (s *Store) readableHeadlessLogChunksUnlocked(id string) ([]headlessLogChunk, error) {
	if err := s.migrateLegacyHeadlessLogUnlocked(id, DefaultHeadlessLogPolicy()); err != nil {
		return nil, err
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

func annotateHeadlessStale(run *HeadlessRun, now time.Time, staleAfter time.Duration) {
	stale, reason := headlessStaleReason(*run, now, staleAfter)
	if !stale {
		return
	}

	run.Status = HeadlessStatusStale
	run.Stale = true
	run.StaleReason = reason

	if run.CancellationReason == "" {
		run.CancellationReason = reason
	}
}

func headlessStaleReason(run HeadlessRun, now time.Time, staleAfter time.Duration) (stale bool, reason string) {
	if run.Status != HeadlessStatusRunning {
		return false, ""
	}

	if headlessPIDIsLocal(run) {
		if headlessProcessAlive(run.PID) {
			return false, ""
		}

		return true, fmt.Sprintf("process pid %d is not running", run.PID)
	}

	activity := headlessLastActivity(run)
	if activity.IsZero() || now.Sub(activity) <= staleAfter {
		return false, ""
	}

	return true, "no heartbeat since " + activity.UTC().Format(time.RFC3339)
}

func headlessPIDIsLocal(run HeadlessRun) bool {
	if run.PID <= 0 {
		return false
	}

	if strings.TrimSpace(run.Hostname) == "" {
		return true
	}

	hostname, err := os.Hostname()
	if err != nil || strings.TrimSpace(hostname) == "" {
		return false
	}

	return strings.EqualFold(run.Hostname, hostname)
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

	if id == "." || id == ".." || filepath.Base(id) != id {
		return errors.New("session: headless id must be a file name")
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
