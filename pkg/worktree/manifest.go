package worktree

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tommoulard/atteler/pkg/permission"
)

const worktreeManifestVersion = 1

const (
	manifestStateCreating      = "creating"
	manifestStateBranchCreated = "branch-created"
	manifestStateActive        = "active"
	manifestStateMerging       = "merging"
	manifestStateMerged        = "merged"
	manifestStateRemoved       = "removed"
	manifestStateFailed        = "failed"
)

type worktreeManifest struct {
	SessionID       string `json:"session_id"`
	Branch          string `json:"branch"`
	BaseBranch      string `json:"base_branch"`
	BaseHEAD        string `json:"base_head"`
	WorktreePath    string `json:"worktree_path"`
	RepoRoot        string `json:"repo_root"`
	State           string `json:"state"`
	CreatedAt       string `json:"created_at"`
	UpdatedAt       string `json:"updated_at"`
	LastTransaction string `json:"last_transaction,omitempty"`
	LastError       string `json:"last_error,omitempty"`
	SchemaVersion   int    `json:"schema_version"`
}

func newWorktreeManifest(sessionID, branch, baseBranch, baseHEAD, repoRoot, wtDir string) worktreeManifest {
	now := time.Now().UTC().Format(time.RFC3339Nano)

	return worktreeManifest{
		SchemaVersion: worktreeManifestVersion,
		SessionID:     sessionID,
		Branch:        branch,
		BaseBranch:    baseBranch,
		BaseHEAD:      baseHEAD,
		WorktreePath:  wtDir,
		RepoRoot:      repoRoot,
		State:         manifestStateCreating,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
}

func manifestInfo(manifest *worktreeManifest) *Info {
	if manifest == nil {
		return nil
	}

	return &Info{
		Path:       manifest.WorktreePath,
		Branch:     manifest.Branch,
		BaseBranch: manifest.BaseBranch,
		SessionID:  manifest.SessionID,
	}
}

func loadWorktreeManifest(ctx context.Context, repoRoot, sessionID string) (*worktreeManifest, bool, error) {
	path, err := worktreeManifestPath(ctx, repoRoot, sessionID)
	if err != nil {
		return nil, false, err
	}

	if permissionErr := authorizeWorktreePermission(ctx, "read worktree ownership manifest", path, sessionID, []permission.OperationKind{
		permission.OperationRead,
	}); permissionErr != nil {
		return nil, false, permissionErr
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}

		return nil, false, fmt.Errorf("read manifest %s: %w", path, err)
	}

	var manifest worktreeManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, false, fmt.Errorf("parse manifest %s: %w", path, err)
	}

	return &manifest, true, nil
}

func writeWorktreeManifest(ctx context.Context, repoRoot string, manifest *worktreeManifest, event, detail string) error {
	if manifest == nil {
		return nil
	}

	manifest.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)

	path, err := worktreeManifestPath(ctx, repoRoot, manifest.SessionID)
	if err != nil {
		return err
	}

	if permissionErr := authorizeWorktreePermission(ctx, "write worktree ownership manifest", path, manifest.SessionID, []permission.OperationKind{
		permission.OperationWrite,
	}); permissionErr != nil {
		return permissionErr
	}

	mkdirErr := os.MkdirAll(filepath.Dir(path), 0o700)
	if mkdirErr != nil {
		return fmt.Errorf("create manifest dir %s: %w", filepath.Dir(path), mkdirErr)
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create manifest temp for %s: %w", path, err)
	}

	tmpPath := tmp.Name()
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")

	if err := enc.Encode(manifest); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)

		return fmt.Errorf("encode manifest %s: %w", path, err)
	}

	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)

		return fmt.Errorf("close manifest temp %s: %w", tmpPath, err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)

		return fmt.Errorf("write manifest %s: %w", path, err)
	}

	if event != "" {
		if err := appendWorktreeLedger(ctx, repoRoot, manifest.SessionID, event, detail); err != nil {
			return err
		}
	}

	return nil
}

func appendWorktreeLedger(ctx context.Context, repoRoot, sessionID, event, detail string) error {
	path, err := worktreeLedgerPath(ctx, repoRoot, sessionID)
	if err != nil {
		return err
	}

	if permissionErr := authorizeWorktreePermission(ctx, "write worktree ownership ledger", path, sessionID, []permission.OperationKind{
		permission.OperationWrite,
	}); permissionErr != nil {
		return permissionErr
	}

	mkdirErr := os.MkdirAll(filepath.Dir(path), 0o700)
	if mkdirErr != nil {
		return fmt.Errorf("create ledger dir %s: %w", filepath.Dir(path), mkdirErr)
	}

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open ledger %s: %w", path, err)
	}
	defer file.Close()

	detail = strings.ReplaceAll(detail, "\n", "\\n")
	if _, err := fmt.Fprintf(file, "%s\t%s\t%s\n", time.Now().UTC().Format(time.RFC3339Nano), event, detail); err != nil {
		return fmt.Errorf("append ledger %s: %w", path, err)
	}

	return nil
}

func worktreeManifestPath(ctx context.Context, repoRoot, sessionID string) (string, error) {
	return gitPath(ctx, repoRoot, worktreeManifestDir+"/"+sessionID+".json")
}

func worktreeLedgerPath(ctx context.Context, repoRoot, sessionID string) (string, error) {
	return gitPath(ctx, repoRoot, worktreeManifestDir+"/"+sessionID+".ledger")
}

func validateManifestOwnership(repoRoot string, manifest *worktreeManifest, sessionID, branch, wtDir string) error {
	if manifest == nil {
		return errorsNewOwnership("missing ownership manifest")
	}

	if manifest.SchemaVersion != worktreeManifestVersion {
		return fmt.Errorf("ownership manifest has unsupported schema version %d", manifest.SchemaVersion)
	}

	if manifest.SessionID != sessionID {
		return fmt.Errorf("ownership manifest session %s does not match %s", manifest.SessionID, sessionID)
	}

	if manifest.Branch != branch {
		return fmt.Errorf("ownership manifest branch %s does not match %s", manifest.Branch, branch)
	}

	if !samePath(manifest.WorktreePath, wtDir) {
		return fmt.Errorf("ownership manifest worktree path %s does not match %s", manifest.WorktreePath, wtDir)
	}

	if manifest.RepoRoot != "" && !samePath(manifest.RepoRoot, repoRoot) {
		return fmt.Errorf("ownership manifest repo root %s does not match %s", manifest.RepoRoot, repoRoot)
	}

	if strings.TrimSpace(manifest.BaseBranch) == "" {
		return errorsNewOwnership("ownership manifest is missing base branch")
	}

	if strings.TrimSpace(manifest.BaseHEAD) == "" {
		return errorsNewOwnership("ownership manifest is missing base HEAD")
	}

	return nil
}

func errorsNewOwnership(message string) error {
	return fmt.Errorf("ownership metadata: %s", message)
}
