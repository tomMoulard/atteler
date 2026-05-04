package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const headlessDirName = "headless"

// HeadlessStatus is the lifecycle state for a headless run.
type HeadlessStatus string

const (
	// HeadlessStatusRunning indicates a run is still active.
	HeadlessStatusRunning HeadlessStatus = "running"
	// HeadlessStatusCompleted indicates a run finished successfully.
	HeadlessStatusCompleted HeadlessStatus = "completed"
	// HeadlessStatusFailed indicates a run ended with an error.
	HeadlessStatusFailed HeadlessStatus = "failed"
)

// HeadlessRun records metadata for an atteler headless execution.
type HeadlessRun struct {
	StartedAt   time.Time      `json:"started_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
	CompletedAt *time.Time     `json:"completed_at,omitempty"`
	ID          string         `json:"id"`
	SessionID   string         `json:"session_id"`
	SessionPath string         `json:"session_path"`
	LogPath     string         `json:"log_path"`
	Prompt      string         `json:"prompt"`
	Model       string         `json:"model"`
	Agent       string         `json:"agent"`
	Status      HeadlessStatus `json:"status"`
	Error       string         `json:"error"`
}

// SaveHeadlessRun writes headless run metadata atomically enough for local CLI use.
func (s *Store) SaveHeadlessRun(run HeadlessRun) error {
	if err := validateHeadlessID(run.ID); err != nil {
		return err
	}

	now := time.Now().UTC()
	if run.StartedAt.IsZero() {
		run.StartedAt = now
	}

	run.UpdatedAt = now
	if run.LogPath == "" {
		run.LogPath = s.headlessLogPath(run.ID)
	}

	dir := s.headlessDir()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("session: create headless dir: %w", err)
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

// LoadHeadlessRun reads headless run metadata by ID.
func (s *Store) LoadHeadlessRun(id string) (HeadlessRun, error) {
	if err := validateHeadlessID(id); err != nil {
		return HeadlessRun{}, err
	}

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
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != sessionFileExt {
			continue
		}

		run, err := s.LoadHeadlessRun(idFromPath(entry.Name()))
		if err != nil {
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

// AppendHeadlessLog appends text to the headless run log for id.
func (s *Store) AppendHeadlessLog(id, text string) error {
	if err := validateHeadlessID(id); err != nil {
		return err
	}

	dir := s.headlessDir()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("session: create headless dir: %w", err)
	}

	path := s.headlessLogPath(id)

	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("session: open headless log %s: %w", path, err)
	}
	defer file.Close()

	if _, err := file.WriteString(text); err != nil {
		return fmt.Errorf("session: append headless log %s: %w", path, err)
	}

	return nil
}

// ReadHeadlessLog reads the headless run log for id.
func (s *Store) ReadHeadlessLog(id string) (string, error) {
	if err := validateHeadlessID(id); err != nil {
		return "", err
	}

	path := s.headlessLogPath(id)

	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("session: read headless log %s: %w", path, err)
	}

	return string(data), nil
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

func (s *Store) headlessDir() string {
	return filepath.Join(s.dir, headlessDirName)
}

func (s *Store) headlessJSONPath(id string) string {
	return filepath.Join(s.headlessDir(), id+sessionFileExt)
}

func (s *Store) headlessLogPath(id string) string {
	return filepath.Join(s.headlessDir(), id+".log")
}
