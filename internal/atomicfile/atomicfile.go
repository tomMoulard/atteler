// Package atomicfile provides small local-file persistence helpers.
package atomicfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const defaultTempPattern = ".atteler-*.tmp"

// WriteFile writes data to path through a same-directory temporary file and
// rename. The target directory is created with 0750 permissions when needed and
// the final file is chmod'd to mode after replacement.
func WriteFile(path string, data []byte, mode os.FileMode, tempPattern string) error {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "" || path == "." {
		return errors.New("atomic write: path is required")
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("atomic write: create dir %s: %w", dir, err)
	}

	if strings.TrimSpace(tempPattern) == "" {
		tempPattern = defaultTempPattern
	}

	tmp, err := os.CreateTemp(dir, tempPattern)
	if err != nil {
		return fmt.Errorf("atomic write: create temp in %s: %w", dir, err)
	}

	tmpPath := tmp.Name()
	cleanup := true

	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	if err := os.Chmod(tmpPath, mode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("atomic write: chmod temp %s: %w", tmpPath, err)
	}

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("atomic write: write temp %s: %w", tmpPath, err)
	}

	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("atomic write: sync temp %s: %w", tmpPath, err)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("atomic write: close temp %s: %w", tmpPath, err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("atomic write: replace %s: %w", path, err)
	}

	cleanup = false

	if err := os.Chmod(path, mode); err != nil {
		return fmt.Errorf("atomic write: chmod %s: %w", path, err)
	}

	return nil
}
