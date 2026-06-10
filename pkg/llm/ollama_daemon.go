package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/tommoulard/atteler/pkg/permission"
)

const (
	envOllamaOwnershipPath = "ATTELER_OLLAMA_OWNERSHIP_PATH"
	envAttelerStatePath    = "ATTELER_STATE"

	ollamaOwnershipStatusError = "error"
	ollamaOwnershipFilename    = "ollama-daemon.json"
	ollamaOwnershipOwner       = "atteler"
	ollamaStartupLogBytes      = 16 * 1024

	ollamaProcessInspectionAction = "inspect Ollama process ownership"
)

// OllamaStatusState is the user-facing lifecycle state for a configured
// Ollama endpoint.
type OllamaStatusState string

// Ollama lifecycle states reported by --ollama-status and doctor.
const (
	OllamaStatusAlreadyRunning   OllamaStatusState = "already-running"
	OllamaStatusStartedByAtteler OllamaStatusState = "started-by-atteler"
	OllamaStatusRemote           OllamaStatusState = "remote"
	OllamaStatusUnavailable      OllamaStatusState = "unavailable"
	OllamaStatusMisconfigured    OllamaStatusState = "misconfigured"
)

// OllamaAutoStartPolicy reports whether Atteler is allowed to launch
// "ollama serve" and where that decision came from.
type OllamaAutoStartPolicy struct {
	Source  string
	Error   string
	Enabled bool
}

// OllamaDaemonOwnership records the local daemon process Atteler started so it
// can be diagnosed and stopped later without guessing process ownership.
//
// Environment intentionally stores only daemon-specific overrides, not the
// whole process environment, to avoid persisting API keys or other secrets.
//
//nolint:govet // Field order keeps the JSON metadata grouped for humans.
type OllamaDaemonOwnership struct {
	StartedAt       time.Time         `json:"started_at"`
	Environment     map[string]string `json:"environment,omitempty"`
	Command         []string          `json:"command"`
	AttelerCommand  []string          `json:"atteler_command,omitempty"`
	Owner           string            `json:"owner"`
	BaseURL         string            `json:"base_url"`
	SessionID       string            `json:"session_id,omitempty"`
	AutoStartSource string            `json:"auto_start_source,omitempty"`
	LogPath         string            `json:"log_path,omitempty"`
	PID             int               `json:"pid"`
	AttelerPID      int               `json:"atteler_pid"`
	ParentPID       int               `json:"parent_pid"`
}

// OllamaStatus describes the current endpoint and any Atteler ownership
// metadata found on disk.
//
//nolint:govet // Field order follows the user-facing status report.
type OllamaStatus struct {
	Ownership       *OllamaDaemonOwnership
	AutoStart       OllamaAutoStartPolicy
	State           OllamaStatusState
	BaseURL         string
	OwnershipPath   string
	OwnershipStatus string
	Error           string
	Local           bool
}

// OllamaStopResult describes the outcome of a stop/cleanup request.
type OllamaStopResult struct {
	Ownership     *OllamaDaemonOwnership
	OwnershipPath string
	Message       string
	Stopped       bool
	Cleaned       bool
}

var (
	ollamaProcessHooksMu    sync.Mutex
	ollamaProcessAlive      = defaultOllamaProcessAlive
	ollamaTerminateProcess  = defaultOllamaTerminateProcess
	ollamaKillProcess       = defaultOllamaKillProcess
	ollamaProcessMatches    = defaultOllamaProcessMatchesOwnership
	ollamaProcessPollPeriod = 50 * time.Millisecond
)

func ollamaAutoStartPolicy(configured bool) OllamaAutoStartPolicy {
	if raw, ok := os.LookupEnv(envOllamaAutoStart); ok {
		enabled, valid := parseOllamaBool(raw)
		if !valid {
			return OllamaAutoStartPolicy{
				Source:  "env." + envOllamaAutoStart,
				Error:   envOllamaAutoStart + " must be one of true/false, 1/0, yes/no, or on/off",
				Enabled: false,
			}
		}

		return OllamaAutoStartPolicy{
			Source:  "env." + envOllamaAutoStart,
			Enabled: enabled,
		}
	}

	if configured {
		return OllamaAutoStartPolicy{Source: "config.providers.ollama.auto_start", Enabled: true}
	}

	return OllamaAutoStartPolicy{Source: "default", Enabled: false}
}

func parseOllamaBool(raw string) (enabled, valid bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true, true
	case "0", "false", "no", "off", "":
		return false, true
	default:
		return false, false
	}
}

// CheckOllamaStatus inspects the configured Ollama endpoint without starting a
// daemon.
func CheckOllamaStatus(ctx context.Context, cfg ProviderConfig) OllamaStatus {
	baseURL := strings.TrimRight(configuredBaseURL("OLLAMA_BASE_URL", cfg.BaseURL, defaultOllamaBase), "/")

	status := OllamaStatus{
		State:         OllamaStatusUnavailable,
		BaseURL:       baseURL,
		AutoStart:     ollamaAutoStartPolicy(cfg.AutoStart),
		OwnershipPath: ollamaOwnershipPath(cfg.OwnershipPath),
	}
	if err := requireCredentialContext(ctx); err != nil {
		status.Error = err.Error()

		return status
	}

	parsed, err := parseOllamaBaseURL(baseURL)
	if err != nil {
		status.State = OllamaStatusMisconfigured
		status.Error = err.Error()

		return status
	}

	status.Local = isLocalOllamaParsedURL(parsed)
	if !status.Local {
		status.State = OllamaStatusRemote

		return status
	}

	if err := authorizeOllamaOwnershipReadPermission(ctx, status.OwnershipPath); err != nil {
		status.OwnershipStatus = ollamaOwnershipStatusError
		status.Error = err.Error()

		return status
	}

	ownership, ownershipErr := readOllamaOwnership(status.OwnershipPath)
	if ownershipErr == nil {
		status.Ownership = ownership
		ownershipStatus, statusErr := ollamaOwnershipStatusContext(ctx, baseURL, ownership)
		status.OwnershipStatus = ownershipStatus

		if statusErr != nil {
			status.OwnershipStatus = ollamaOwnershipStatusError
			status.Error = statusErr.Error()

			return status
		}
	} else if !errors.Is(ownershipErr, os.ErrNotExist) {
		status.OwnershipStatus = ollamaOwnershipStatusError
		status.Error = ownershipErr.Error()
	}

	provider := &OllamaProvider{
		baseURL: baseURL,
		client:  providerHTTPClient(cfg),
	}
	if err := provider.HealthCheck(ctx); err != nil {
		status.State = OllamaStatusUnavailable
		if status.Error == "" {
			status.Error = err.Error()
		}

		return status
	}

	if status.Ownership != nil {
		if status.OwnershipStatus == "owned-running" {
			status.State = OllamaStatusStartedByAtteler

			return status
		}

		status.State = OllamaStatusAlreadyRunning

		return status
	}

	status.State = OllamaStatusAlreadyRunning

	return status
}

// StopOwnedOllamaDaemon stops the daemon recorded in Atteler's ownership file.
// It refuses to act when there is no Atteler ownership record.
func StopOwnedOllamaDaemon(ctx context.Context, ownershipPath string) (OllamaStopResult, error) {
	path := ollamaOwnershipPath(ownershipPath)

	result := OllamaStopResult{OwnershipPath: path}
	if err := requireCredentialContext(ctx); err != nil {
		return result, err
	}

	ownership, result, err := readOllamaOwnershipForStop(ctx, path, result)
	if err != nil || ownership == nil {
		return result, err
	}

	result.Ownership = ownership
	if ownership.Owner != ollamaOwnershipOwner {
		return result, fmt.Errorf("ollama: ownership record %s is owned by %q, not atteler", path, ownership.Owner)
	}

	if ownership.PID <= 0 {
		return cleanStaleOllamaOwnership(ctx, path, ownership, result)
	}

	alive, aliveErr := ollamaPIDAliveContext(ctx, ownership)
	if aliveErr != nil {
		return result, aliveErr
	}

	if !alive {
		return cleanStaleOllamaOwnership(ctx, path, ownership, result)
	}

	return stopRunningOllamaDaemonAfterInspection(ctx, path, ownership, result)
}

func stopRunningOllamaDaemonAfterInspection(
	ctx context.Context,
	path string,
	ownership *OllamaDaemonOwnership,
	result OllamaStopResult,
) (OllamaStopResult, error) {
	if err := validateOllamaOwnershipForStop(path, ownership); err != nil {
		return result, err
	}

	// StopOwnedOllamaDaemon already authorized process inspection before the
	// liveness probe. Reuse that approval for the ownership match so an
	// interactive policy does not prompt twice for the same PID inspection.
	matchesOwnership := ollamaPIDMatchesOwnership(ownership)
	if !matchesOwnership {
		return result, fmt.Errorf("ollama: PID %d no longer matches Atteler Ollama ownership record %s; refusing to stop", ownership.PID, path)
	}

	if err := authorizeOllamaStopPermission(ctx, "stop Atteler-owned Ollama daemon", path, ownership, true); err != nil {
		return result, err
	}

	if err := ollamaTerminatePID(ownership.PID); err != nil {
		return result, err
	}

	if err := waitForOllamaPIDExit(ctx, ownership.PID); err != nil {
		if killErr := ollamaKillPID(ownership.PID); killErr != nil {
			return result, errors.Join(err, killErr)
		}

		if waitErr := waitForOllamaPIDExit(ctx, ownership.PID); waitErr != nil {
			return result, waitErr
		}
	}

	if err := removeOllamaOwnership(path); err != nil {
		return result, err
	}

	result.Stopped = true
	result.Cleaned = true
	result.Message = "stopped Atteler-owned Ollama daemon"

	return result, nil
}

func readOllamaOwnershipForStop(ctx context.Context, path string, result OllamaStopResult) (*OllamaDaemonOwnership, OllamaStopResult, error) {
	if err := authorizeOllamaOwnershipReadPermission(ctx, path); err != nil {
		return nil, result, err
	}

	ownership, err := readOllamaOwnership(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			result.Message = "no Atteler-owned Ollama daemon record found"

			return nil, result, nil
		}

		return nil, result, err
	}

	return ownership, result, nil
}

func cleanStaleOllamaOwnership(
	ctx context.Context,
	path string,
	ownership *OllamaDaemonOwnership,
	result OllamaStopResult,
) (OllamaStopResult, error) {
	if err := authorizeOllamaStopPermission(ctx, "clean stale Ollama ownership record", path, ownership, false); err != nil {
		return result, err
	}

	if err := removeOllamaOwnership(path); err != nil {
		return result, err
	}

	result.Cleaned = true
	result.Message = "removed stale Atteler Ollama ownership record"

	return result, nil
}

func authorizeOllamaOwnershipReadPermission(ctx context.Context, ownershipPath string) error {
	return authorizeProviderPermission(ctx, providerOllama, "read Ollama ownership state", ownershipPath, permission.OperationRead)
}

func authorizeOllamaStateWritePermission(ctx context.Context, action, path string) error {
	return authorizeProviderPermission(ctx, providerOllama, action, path, permission.OperationWrite)
}

func authorizeOllamaStateDeletePermission(ctx context.Context, action, path string) error {
	return authorizeProviderPermission(
		ctx,
		providerOllama,
		action,
		path,
		permission.OperationWrite,
		permission.OperationMergeDelete,
	)
}

func authorizeOllamaProcessInspectionPermission(ctx context.Context, ownership *OllamaDaemonOwnership) error {
	if ownership == nil || ownership.PID <= 0 {
		return nil
	}

	target := fmt.Sprintf("pid=%d", ownership.PID)
	if runtime.GOOS == "linux" {
		target = fmt.Sprintf("/proc/%d/cmdline", ownership.PID)
	}

	return authorizeProviderPermission(ctx, providerOllama, ollamaProcessInspectionAction, target, permission.OperationRead)
}

func authorizeOllamaStopPermission(ctx context.Context, action, ownershipPath string, ownership *OllamaDaemonOwnership, stopsProcess bool) error {
	kinds := []permission.OperationKind{
		permission.OperationWrite,
		permission.OperationMergeDelete,
	}
	if stopsProcess {
		kinds = append([]permission.OperationKind{permission.OperationExecute}, kinds...)
	}

	target := ownershipPath
	metadata := map[string]string{}

	if ownership != nil {
		if ownership.PID > 0 {
			target = fmt.Sprintf("%s pid=%d", ownershipPath, ownership.PID)
		}

		if ownership.SessionID != "" {
			metadata["session_id"] = ownership.SessionID
		}
	}

	ops := make([]permission.Operation, 0, len(kinds))
	for _, kind := range kinds {
		op := permission.Operation{
			Kind:   kind,
			Action: action,
			Target: target,
			Source: "atteler.provider.ollama",
		}
		if len(metadata) > 0 {
			op.Metadata = metadata
		}

		ops = append(ops, op)
	}

	decision := permission.Evaluate(ctx, nil, permission.Request{
		Operations: ops,
		Action:     action,
		Source:     "atteler.provider.ollama",
		Target:     target,
	})
	if decision.Allowed {
		return nil
	}

	return fmt.Errorf("ollama: %s: %w", action, &permission.Error{Decision: decision})
}

func validateOllamaOwnershipForStop(path string, ownership *OllamaDaemonOwnership) error {
	if ownership == nil {
		return fmt.Errorf("ollama: ownership record %s is empty", path)
	}

	if ownership.PID <= 0 {
		return fmt.Errorf("ollama: ownership record %s has invalid PID %d", path, ownership.PID)
	}

	if !ollamaServeCommandRecorded(ownership.Command) {
		return fmt.Errorf("ollama: ownership record %s command is %q, not ollama serve", path, strings.Join(ownership.Command, " "))
	}

	return nil
}

func ollamaServeCommandRecorded(command []string) bool {
	if len(command) < 2 {
		return false
	}

	executable := strings.TrimSuffix(strings.ToLower(filepath.Base(command[0])), ".exe")

	return executable == ollamaServeCommand && command[1] == "serve"
}

func waitForOllamaPIDExit(ctx context.Context, pid int) error {
	ticker := time.NewTicker(ollamaProcessPollPeriod)
	defer ticker.Stop()

	for {
		if !ollamaPIDAlive(pid) {
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("ollama: wait for PID %d to exit: %w", pid, ctx.Err())
		case <-ticker.C:
		}
	}
}

func ollamaPIDAlive(pid int) bool {
	ollamaProcessHooksMu.Lock()
	alive := ollamaProcessAlive
	ollamaProcessHooksMu.Unlock()

	return alive(pid)
}

func ollamaPIDAliveContext(ctx context.Context, ownership *OllamaDaemonOwnership) (bool, error) {
	if err := authorizeOllamaProcessInspectionPermission(ctx, ownership); err != nil {
		return false, err
	}

	if ownership == nil {
		return false, nil
	}

	return ollamaPIDAlive(ownership.PID), nil
}

func ollamaTerminatePID(pid int) error {
	ollamaProcessHooksMu.Lock()
	terminate := ollamaTerminateProcess
	ollamaProcessHooksMu.Unlock()

	return terminate(pid)
}

func ollamaKillPID(pid int) error {
	ollamaProcessHooksMu.Lock()
	killProcess := ollamaKillProcess
	ollamaProcessHooksMu.Unlock()

	return killProcess(pid)
}

func ollamaPIDMatchesOwnership(ownership *OllamaDaemonOwnership) bool {
	ollamaProcessHooksMu.Lock()
	matches := ollamaProcessMatches
	ollamaProcessHooksMu.Unlock()

	return matches(ownership)
}

func parseOllamaBaseURL(baseURL string) (*url.URL, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("ollama: invalid base URL %q: %w", baseURL, err)
	}

	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("ollama: invalid base URL %q: scheme must be http or https", baseURL)
	}

	if parsed.Hostname() == "" {
		return nil, fmt.Errorf("ollama: invalid base URL %q: host is required", baseURL)
	}

	return parsed, nil
}

func isLocalOllamaParsedURL(parsed *url.URL) bool {
	if parsed == nil {
		return false
	}

	return isLocalOllamaHost(parsed.Hostname())
}

func isLocalOllamaHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if host == "localhost" {
		return true
	}

	ip, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}

	return ip.IsLoopback()
}

func ollamaOwnershipStatusContext(ctx context.Context, baseURL string, ownership *OllamaDaemonOwnership) (string, error) {
	switch {
	case ownership == nil:
		return "none", nil
	case ownership.Owner != "atteler":
		return "recorded-untrusted-owner", nil
	case !ollamaServeCommandRecorded(ownership.Command):
		return "recorded-invalid-command", nil
	case ownership.BaseURL != baseURL:
		return "recorded-for-different-base-url", nil
	default:
		alive, err := ollamaPIDAliveContext(ctx, ownership)
		if err != nil {
			return ollamaOwnershipStatusError, err
		}

		if !alive {
			return "owned-stale", nil
		}

		if !ollamaPIDMatchesOwnership(ownership) {
			return "owned-pid-mismatch", nil
		}

		return "owned-running", nil
	}
}

func readOllamaOwnership(path string) (*OllamaDaemonOwnership, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("ollama: read ownership %s: %w", path, err)
	}

	var ownership OllamaDaemonOwnership
	if err := json.Unmarshal(data, &ownership); err != nil {
		return nil, fmt.Errorf("ollama: parse ownership %s: %w", path, err)
	}

	return &ownership, nil
}

func recordOllamaOwnership(path string, ownership OllamaDaemonOwnership) error {
	// Refuse to silently overwrite a record for another live atteler-owned
	// daemon: doing so would orphan that daemon and make it unstoppable via
	// `atteler --ollama-stop`.
	if existing, err := readOllamaOwnership(path); err == nil &&
		existing.Owner == ollamaOwnershipOwner &&
		existing.PID > 0 &&
		existing.PID != ownership.PID &&
		ollamaPIDAlive(existing.PID) &&
		ollamaPIDMatchesOwnership(existing) {
		return fmt.Errorf("ownership file %s already records live daemon PID %d", path, existing.PID)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create ownership dir %s: %w", dir, err)
	}

	data, err := json.MarshalIndent(ownership, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal ownership: %w", err)
	}

	data = append(data, '\n')

	tmp, err := os.CreateTemp(dir, ".ollama-daemon-*.tmp")
	if err != nil {
		return fmt.Errorf("create ownership temp file: %w", err)
	}

	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)

		return fmt.Errorf("write ownership temp file: %w", err)
	}

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)

		return fmt.Errorf("chmod ownership temp file: %w", err)
	}

	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)

		return fmt.Errorf("close ownership temp file: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)

		return fmt.Errorf("replace ownership file %s: %w", path, err)
	}

	return nil
}

func recordOllamaOwnershipContext(ctx context.Context, path string, ownership OllamaDaemonOwnership) error {
	if err := authorizeOllamaStateWritePermission(ctx, "write Ollama ownership state", path); err != nil {
		return err
	}

	return recordOllamaOwnership(path, ownership)
}

func removeOllamaOwnership(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove ownership file %s: %w", path, err)
	}

	return nil
}

func ollamaOwnershipPath(override string) string {
	if path := strings.TrimSpace(override); path != "" {
		return path
	}

	if path := strings.TrimSpace(os.Getenv(envOllamaOwnershipPath)); path != "" {
		return path
	}

	return filepath.Join(ollamaStateDir(""), ollamaOwnershipFilename)
}

func ollamaStateDir(ownershipPath string) string {
	if path := strings.TrimSpace(ownershipPath); path != "" {
		return filepath.Dir(path)
	}

	if path := strings.TrimSpace(os.Getenv(envOllamaOwnershipPath)); path != "" {
		return filepath.Dir(path)
	}

	if path := strings.TrimSpace(os.Getenv(envAttelerStatePath)); path != "" {
		return filepath.Dir(path)
	}

	if dir := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); dir != "" {
		return filepath.Join(dir, "atteler")
	}

	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(os.TempDir(), "atteler")
	}

	return filepath.Join(home, ".local", "state", "atteler")
}

type boundedLogBuffer struct {
	buf   []byte
	limit int
	mu    sync.Mutex
}

func newBoundedLogBuffer(limit int) *boundedLogBuffer {
	return &boundedLogBuffer{limit: limit}
}

func (b *boundedLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.limit <= 0 {
		return len(p), nil
	}

	b.buf = append(b.buf, p...)
	if len(b.buf) > b.limit {
		b.buf = append([]byte(nil), b.buf[len(b.buf)-b.limit:]...)
	}

	return len(p), nil
}

func (b *boundedLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return string(append([]byte(nil), b.buf...))
}

type lockedWriter struct {
	writer io.Writer
	mu     sync.Mutex
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	n, err := w.writer.Write(p)
	if err != nil {
		return n, fmt.Errorf("write locked writer: %w", err)
	}

	return n, nil
}

type cappedLogFileWriter struct {
	writer    io.Writer
	remaining int
	truncated bool
}

func newCappedLogFileWriter(writer io.Writer, limit int) *cappedLogFileWriter {
	return &cappedLogFileWriter{writer: writer, remaining: limit}
}

func (w *cappedLogFileWriter) Write(p []byte) (int, error) {
	if w == nil || w.writer == nil {
		return len(p), nil
	}

	if w.remaining <= 0 {
		if len(p) > 0 {
			if err := w.writeTruncationMarker(); err != nil {
				return 0, err
			}
		}

		return len(p), nil
	}

	toWrite := p
	if len(toWrite) > w.remaining {
		toWrite = toWrite[:w.remaining]
	}

	written, err := w.writer.Write(toWrite)

	w.remaining -= written
	if err != nil {
		return written, fmt.Errorf("write startup log: %w", err)
	}

	if written != len(toWrite) {
		return written, io.ErrShortWrite
	}

	if len(p) > len(toWrite) && !w.truncated {
		if err := w.writeTruncationMarker(); err != nil {
			return written, err
		}
	}

	return len(p), nil
}

func (w *cappedLogFileWriter) writeTruncationMarker() error {
	if w.truncated {
		return nil
	}

	w.truncated = true
	if _, err := w.writer.Write([]byte("\n[atteler: startup log truncated]\n")); err != nil {
		return fmt.Errorf("write startup log truncation marker: %w", err)
	}

	return nil
}
