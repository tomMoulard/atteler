package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/tommoulard/atteler/pkg/autonomy"
	"github.com/tommoulard/atteler/pkg/session"
	"github.com/tommoulard/atteler/pkg/shell"
	"github.com/tommoulard/atteler/pkg/worktree"
)

const worktreeVerificationStatusPass = "PASS"

func listWorktrees(ctx context.Context, level autonomy.Level) error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("list worktrees: %w", err)
	}

	ctx = worktree.WithAuditContext(ctx, worktreeShellAuditContext(session.Session{}, level))
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

func mergeWorktreeBySession(ctx context.Context, sessionRef string, policy cliWorktreeMergePolicy, level autonomy.Level) error {
	if !autonomy.Normalize(level).Allows(autonomy.ActionCommit) {
		return fmt.Errorf("%s", autonomy.DenialMessage(level, autonomy.ActionCommit, "--merge-worktree"))
	}

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("merge worktree: %w", err)
	}

	ctx = worktree.WithAuditContext(ctx, worktreeShellAuditContext(session.Session{}, level))
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

	ctx = worktree.WithAuditContext(ctx, worktreeShellAuditContext(sess, level))
	result, err := worktree.MergeWithResultContext(ctx, cwd, info, worktree.MergeOptions{
		AutoMerge:               true,
		Strategy:                worktree.MergeStrategyMerge,
		OverrideVerification:    policy.OverrideVerification,
		VerificationCommands:    policy.VerificationCommands,
		AllowBaseBranchMismatch: policy.AllowBaseMismatch,
		Provenance:              worktreeMergeProvenance(sess),
	})
	if err != nil {
		return fmt.Errorf("merge worktree: %w", err)
	}

	// Clear worktree metadata from the session.
	sess.WorktreePath = ""
	sess.WorktreeBranch = ""

	sess.WorktreeBase = ""
	if err := store.Save(sess); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not update session after merge: %v\n", err)
	}

	printWorktreeMergeResult(os.Stderr, result)
	fmt.Fprintf(os.Stderr, "worktree: merged and cleaned up session %s\n", sess.ID)

	return nil
}

func printWorktreeMergeResult(w io.Writer, result worktree.MergeResult) {
	if strings.TrimSpace(result.DiffSummary) != "" {
		fmt.Fprintln(w, "worktree: diff summary:")
		printIndentedLines(w, result.DiffSummary)
	}

	fmt.Fprintln(w, "worktree: tests run:")

	if len(result.Verification) == 0 {
		if result.VerificationOverridden {
			fmt.Fprintln(w, "  - verification override: no commands run")
		} else {
			fmt.Fprintln(w, "  - none recorded")
		}
	} else {
		for _, verification := range result.Verification {
			status := worktreeVerificationStatusPass
			if !verification.Passed {
				status = "FAIL"
			}

			fmt.Fprintf(w, "  - %s %s\n", status, verification.Command)
		}
	}

	if result.CommitSHA != "" {
		fmt.Fprintln(w, "worktree: commit SHA: "+result.CommitSHA)
	}

	if result.TransactionLog != "" {
		fmt.Fprintln(w, "worktree: transaction log: "+result.TransactionLog)
	}

	if len(result.RollbackCommands) > 0 {
		fmt.Fprintln(w, "worktree: rollback instructions:")

		for _, command := range result.RollbackCommands {
			fmt.Fprintln(w, "  - "+command)
		}
	}
}

func printIndentedLines(w io.Writer, text string) {
	for line := range strings.SplitSeq(strings.TrimSpace(text), "\n") {
		fmt.Fprintln(w, "  "+line)
	}
}

func worktreeShellAuditContext(sess session.Session, level autonomy.Level) shell.AuditContext {
	return shell.AuditContext{
		Caller:    "atteler.worktree.git",
		SessionID: sess.ID,
		Autonomy:  autonomy.Normalize(level).String(),
	}
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
