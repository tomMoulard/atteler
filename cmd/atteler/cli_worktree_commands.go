package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/tommoulard/atteler/pkg/session"
	"github.com/tommoulard/atteler/pkg/worktree"
)

func listWorktrees(ctx context.Context) error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("list worktrees: %w", err)
	}

	if !worktree.IsGitRepoContext(ctx, cwd) {
		return errors.New("list worktrees: not inside a git repository")
	}

	infos, err := worktree.ListContext(ctx, cwd)
	if err != nil {
		return fmt.Errorf("list worktrees: %w", err)
	}

	if len(infos) == 0 {
		fmt.Println("No active atteler worktrees.")
		return nil
	}

	for i := range infos {
		info := &infos[i]
		fmt.Printf("%s\tbranch=%s\tbase=%s\tsession=%s\n",
			info.Path, info.Branch, info.BaseBranch, info.SessionID)
	}

	return nil
}

func mergeWorktreeBySession(ctx context.Context, sessionRef string, allowBaseMismatch bool) error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("merge worktree: %w", err)
	}

	if !worktree.IsGitRepoContext(ctx, cwd) {
		return errors.New("merge worktree: not inside a git repository")
	}

	store := session.NewStore("")

	sess, err := store.Load(sessionRef)
	if err != nil {
		return fmt.Errorf("merge worktree: load session: %w", err)
	}

	if sess.WorktreePath == "" {
		return fmt.Errorf("merge worktree: session %s has no worktree", sess.ID)
	}

	info := &worktree.Info{
		Path:       sess.WorktreePath,
		Branch:     sess.WorktreeBranch,
		BaseBranch: sess.WorktreeBase,
		SessionID:  sess.ID,
	}

	fmt.Fprintf(os.Stderr, "worktree: merging %s into %s...\n", info.Branch, info.BaseBranch)

	if err := worktree.MergeWithOptionsContext(ctx, cwd, info, worktree.MergeOptions{
		AutoCommit:              true,
		AutoMerge:               true,
		AllowBaseBranchMismatch: allowBaseMismatch,
		Provenance:              worktreeMergeProvenance(sess),
	}); err != nil {
		return fmt.Errorf("merge worktree: %w", err)
	}

	// Clear worktree metadata from the session.
	sess.WorktreePath = ""
	sess.WorktreeBranch = ""

	sess.WorktreeBase = ""
	if err := store.Save(sess); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not update session after merge: %v\n", err)
	}

	fmt.Fprintf(os.Stderr, "worktree: merged and cleaned up session %s\n", sess.ID)

	return nil
}

func worktreeMergeProvenance(sess session.Session) []string {
	provenance := []string{"session=" + sess.ID}

	if sess.Title != "" {
		provenance = append(provenance, "title="+sess.Title)
	}

	if len(sess.Tags) > 0 {
		provenance = append(provenance, "tags="+strings.Join(sess.Tags, ","))
	}

	return provenance
}
