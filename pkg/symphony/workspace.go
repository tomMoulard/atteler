//nolint:wsl_v5 // Workspace lifecycle branches are kept explicit around create/reuse/remove hooks.
package symphony

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/tommoulard/atteler/pkg/shell"
)

var workspaceUnsafeCharPattern = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// WorkspaceManager creates, reuses, and removes per-issue workspaces.
type WorkspaceManager struct {
	logger *slog.Logger
}

// NewWorkspaceManager creates a workspace manager.
func NewWorkspaceManager(logger *slog.Logger) *WorkspaceManager {
	return &WorkspaceManager{logger: loggerOrDefault(logger)}
}

// Ensure creates or reuses the workspace for issue and runs after_create only
// when the directory is first created.
func (m *WorkspaceManager) Ensure(ctx context.Context, cfg Config, issue Issue) (Workspace, error) {
	key := SanitizeWorkspaceKey(issue.Identifier)
	if key == "" {
		return Workspace{}, errors.New("workspace: issue identifier is empty after sanitization")
	}

	root := filepath.Clean(cfg.Workspace.Root)
	if err := os.MkdirAll(root, workspaceDirPermissions); err != nil {
		return Workspace{}, fmt.Errorf("workspace: create root %s: %w", root, err)
	}

	path := filepath.Join(root, key)
	if err := ensureUnderRoot(root, path); err != nil {
		return Workspace{}, err
	}

	var createdNow bool
	info, statErr := os.Stat(path)
	switch {
	case statErr == nil && !info.IsDir():
		return Workspace{}, fmt.Errorf("workspace: %s exists and is not a directory", path)
	case statErr == nil:
	case errors.Is(statErr, os.ErrNotExist):
		if err := os.Mkdir(path, workspaceDirPermissions); err != nil {
			return Workspace{}, fmt.Errorf("workspace: create %s: %w", path, err)
		}

		createdNow = true
	default:
		return Workspace{}, fmt.Errorf("workspace: stat %s: %w", path, statErr)
	}

	workspace := Workspace{Path: path, WorkspaceKey: key, CreatedNow: createdNow}
	if createdNow && strings.TrimSpace(cfg.Hooks.AfterCreate) != "" {
		if err := RunHook(ctx, cfg, issue, workspace, "after_create", cfg.Hooks.AfterCreate); err != nil {
			return workspace, fmt.Errorf("workspace: after_create hook failed: %w", err)
		}
	}

	return workspace, nil
}

// Remove runs before_remove best-effort and deletes the issue workspace.
func (m *WorkspaceManager) Remove(ctx context.Context, cfg Config, issue Issue) error {
	key := SanitizeWorkspaceKey(issue.Identifier)
	if key == "" {
		return nil
	}

	workspace := Workspace{
		Path:         filepath.Join(filepath.Clean(cfg.Workspace.Root), key),
		WorkspaceKey: key,
	}

	if err := ensureUnderRoot(cfg.Workspace.Root, workspace.Path); err != nil {
		return err
	}

	if _, err := os.Stat(workspace.Path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}

		return fmt.Errorf("workspace: stat %s: %w", workspace.Path, err)
	}

	if strings.TrimSpace(cfg.Hooks.BeforeRemove) != "" {
		if err := RunHook(ctx, cfg, issue, workspace, "before_remove", cfg.Hooks.BeforeRemove); err != nil {
			m.logger.Warn(
				"symphony hook failed; continuing cleanup",
				"hook", "before_remove",
				"issue_id", issue.ID,
				"issue_identifier", issue.Identifier,
				"error", err,
			)
		}
	}

	if err := os.RemoveAll(workspace.Path); err != nil {
		return fmt.Errorf("workspace: remove %s: %w", workspace.Path, err)
	}

	return nil
}

// SanitizeWorkspaceKey converts an issue identifier to a safe directory name.
func SanitizeWorkspaceKey(identifier string) string {
	return workspaceUnsafeCharPattern.ReplaceAllString(strings.TrimSpace(identifier), "_")
}

// RunHook executes a configured workspace hook in the workspace directory.
func RunHook(ctx context.Context, cfg Config, issue Issue, workspace Workspace, hookName, script string) error {
	if ctx == nil {
		return errors.New("hook: context is required")
	}

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("hook: context already done: %w", err)
	}

	script = strings.TrimSpace(script)
	if script == "" {
		return nil
	}

	workspace.WorkspaceKey = hookWorkspaceKey(issue, workspace)
	if workspace.WorkspaceKey == "" {
		return fmt.Errorf("hook %s workspace key is empty for issue %q", hookName, issue.Identifier)
	}

	timeout := cfg.Hooks.Timeout
	if timeout <= 0 {
		timeout = defaultHookTimeout
	}

	hookCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var stdout, stderr bytes.Buffer
	cmd, invocation, err := shell.CommandContext(hookCtx, shell.CommandOptions{
		Program: "bash",
		Args:    []string{"--noprofile", "--norc", "-lc", script},
		Command: script,
		Dir:     workspace.Path,
		EnvList: hookEnv(issue, workspace, hookName),
		Policy:  symphonyHookPolicy(),
		Mode:    shell.ModeCaptured,
		Stdout:  &stdout,
		Stderr:  &stderr,
		Audit: shell.AuditContext{
			Caller:          "symphony.hook." + hookName,
			IssueID:         issue.ID,
			IssueIdentifier: issue.Identifier,
		},
	})
	if err != nil {
		return fmt.Errorf("hook %s authorize: %w", hookName, err)
	}

	started := time.Now()
	err = cmd.Run()
	if finishErr := invocation.Finish(shell.FinishOptions{
		Stdout:        stdout.String(),
		Stderr:        stderr.String(),
		Error:         err,
		OutputCapture: shell.OutputCaptured,
	}); finishErr != nil && err == nil {
		return fmt.Errorf("hook %s audit failed: %w", hookName, finishErr)
	}
	if hookCtx.Err() != nil {
		return fmt.Errorf("hook %s timed out after %s: %w", hookName, timeout, hookCtx.Err())
	}

	if err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = strings.TrimSpace(stdout.String())
		}

		if message == "" {
			message = err.Error()
		}

		return fmt.Errorf("hook %s failed after %s: %s: %w", hookName, time.Since(started).Round(time.Millisecond), message, err)
	}

	return nil
}

func symphonyHookPolicy() *shell.Policy {
	policy := shell.DefaultPolicy()
	policy.AllowCredentialEnv = append(policy.AllowCredentialEnv, "SYMPHONY_WORKSPACE_KEY")

	return &policy
}

func hookWorkspaceKey(issue Issue, workspace Workspace) string {
	key := strings.TrimSpace(workspace.WorkspaceKey)
	if key != "" {
		return key
	}

	return SanitizeWorkspaceKey(issue.Identifier)
}

func hookEnv(issue Issue, workspace Workspace, hookName string) []string {
	workspaceKey := hookWorkspaceKey(issue, workspace)

	return []string{
		"SYMPHONY_HOOK=" + hookName,
		"SYMPHONY_WORKSPACE_PATH=" + workspace.Path,
		"SYMPHONY_WORKSPACE_KEY=" + workspaceKey,
		"SYMPHONY_ISSUE_ID=" + issue.ID,
		"SYMPHONY_ISSUE_IDENTIFIER=" + issue.Identifier,
		"SYMPHONY_ISSUE_TITLE=" + issue.Title,
		"SYMPHONY_ISSUE_STATE=" + issue.State,
	}
}

func ensureUnderRoot(root, path string) error {
	absRoot, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return fmt.Errorf("workspace: resolve root %s: %w", root, err)
	}

	absPath, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return fmt.Errorf("workspace: resolve path %s: %w", path, err)
	}

	rel, err := filepath.Rel(absRoot, absPath)
	if err != nil {
		return fmt.Errorf("workspace: compare root %s and path %s: %w", absRoot, absPath, err)
	}

	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("workspace: path %s escapes root %s", absPath, absRoot)
	}

	return nil
}

func loggerOrDefault(logger *slog.Logger) *slog.Logger {
	if logger != nil {
		return logger
	}

	return slog.Default()
}
