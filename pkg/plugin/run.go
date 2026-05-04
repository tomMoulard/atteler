package plugin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// RunResult contains the output captured while running a plugin entrypoint.
type RunResult struct {
	Stdout string
	Stderr string
}

// RunEntrypoint validates manifest, resolves entrypointName under root, and runs
// it with root as the working directory. Non-zero exits and context timeouts are
// returned as errors while still returning captured stdout and stderr.
func RunEntrypoint(
	ctx context.Context,
	root string,
	manifest Manifest,
	entrypointName string,
	timeout time.Duration,
) (RunResult, error) {
	if timeout <= 0 {
		return RunResult{}, errors.New("plugin: entrypoint timeout must be positive")
	}

	entrypointName = strings.TrimSpace(entrypointName)
	if entrypointName == "" {
		return RunResult{}, errors.New("plugin: empty entrypoint name")
	}

	if err := manifest.Validate(root); err != nil {
		return RunResult{}, fmt.Errorf("plugin: validate manifest: %w", err)
	}

	entrypoint, ok := manifest.Entrypoints[entrypointName]
	if !ok {
		return RunResult{}, fmt.Errorf("plugin: entrypoint %q not found", entrypointName)
	}

	rootAbs, targetAbs, err := resolveEntrypoint(root, entrypoint)
	if err != nil {
		return RunResult{}, fmt.Errorf("plugin: resolve entrypoint %q: %w", entrypointName, err)
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, targetAbs)
	cmd.Dir = rootAbs

	var stdout, stderr bytes.Buffer

	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()

	result := RunResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if runCtx.Err() != nil {
		return result, fmt.Errorf("plugin: run entrypoint %q: %w", entrypointName, runCtx.Err())
	}

	if runErr != nil {
		return result, fmt.Errorf("plugin: run entrypoint %q: %w", entrypointName, runErr)
	}

	return result, nil
}

func resolveEntrypoint(root, entrypoint string) (resolvedRoot, resolvedTarget string, err error) {
	if validateErr := validateEntrypoint(root, entrypoint); validateErr != nil {
		return "", "", validateErr
	}

	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", "", fmt.Errorf("resolve plugin root: %w", err)
	}

	rootResolved, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return "", "", fmt.Errorf("resolve plugin root symlinks: %w", err)
	}

	targetAbs, err := filepath.Abs(filepath.Join(rootAbs, strings.TrimSpace(entrypoint)))
	if err != nil {
		return "", "", fmt.Errorf("resolve path: %w", err)
	}

	targetResolved, err := filepath.EvalSymlinks(targetAbs)
	if err != nil {
		return "", "", fmt.Errorf("resolve path symlinks: %w", err)
	}

	rel, err := filepath.Rel(rootResolved, targetResolved)
	if err != nil {
		return "", "", fmt.Errorf("compare with plugin root: %w", err)
	}

	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", "", fmt.Errorf("path %q escapes plugin root %q", entrypoint, root)
	}

	return rootAbs, targetResolved, nil
}
