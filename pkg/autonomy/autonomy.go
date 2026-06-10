// Package autonomy defines risk-based capability levels for agent actions.
package autonomy

import (
	"fmt"
	"strings"
)

// Level is the configured risk-based autonomy level for an agent run.
type Level string

const (
	// Low is advisory-only mode: no file writes or mutating commands.
	Low Level = "low"
	// Medium permits local implementation and validation but no publish actions.
	Medium Level = "medium"
	// High permits branch, commit, push, and PR creation after validation.
	High Level = "high"
	// Full permits end-to-end issue implementation through PR creation. Merges
	// are still left to a human.
	Full Level = "full"

	// DefaultLevel preserves backwards-compatible local implementation behavior
	// without allowing publishing operations by default.
	DefaultLevel = Medium
)

// Action describes a capability that can be gated by autonomy.
type Action string

const (
	// ActionFileWrite covers edits to workspace files.
	ActionFileWrite Action = "file_write"
	// ActionMutatingShell covers shell commands that mutate local state.
	ActionMutatingShell Action = "mutating_shell"
	// ActionRemoteMutation covers mutations to remote services such as GitHub
	// issues, secrets, releases, and workflow runs.
	ActionRemoteMutation Action = "remote_mutation"
	// ActionBranch covers branch creation or checkout for publishing work.
	ActionBranch Action = "branch"
	// ActionCommit covers creating git commits.
	ActionCommit Action = "commit"
	// ActionPush covers pushing git refs to a remote.
	ActionPush Action = "push"
	// ActionPullRequestCreate covers opening pull requests.
	ActionPullRequestCreate Action = "pull_request_create"
	// ActionPullRequestMerge covers merging pull requests, which no autonomy
	// level permits.
	ActionPullRequestMerge Action = "pull_request_merge"
)

// SupportedValues returns the accepted user-facing level strings.
func SupportedValues() []string {
	return []string{string(Low), string(Medium), string(High), string(Full)}
}

// Parse validates and normalizes a user-provided autonomy level.
func Parse(raw string) (Level, error) {
	value := strings.ToLower(strings.TrimSpace(raw))
	switch Level(value) {
	case Low, Medium, High, Full:
		return Level(value), nil
	default:
		return "", fmt.Errorf("unsupported autonomy %q (supported: %s)", raw, strings.Join(SupportedValues(), ", "))
	}
}

// Normalize returns DefaultLevel when level is empty, otherwise it returns a
// valid level. Invalid values also fall back to DefaultLevel; use Parse when a
// caller must report invalid user input.
func Normalize(level Level) Level {
	switch level {
	case Low, Medium, High, Full:
		return level
	default:
		return DefaultLevel
	}
}

// FromConfig returns the configured level or DefaultLevel when unset.
func FromConfig(raw string) (Level, error) {
	if strings.TrimSpace(raw) == "" {
		return DefaultLevel, nil
	}

	return Parse(raw)
}

// String returns the stable user-facing value.
func (l Level) String() string {
	return string(Normalize(l))
}

// AllowsAgentTools reports whether the LLM should be given state-changing tools.
func (l Level) AllowsAgentTools() bool {
	return Normalize(l) != Low
}

// Allows reports whether this autonomy level permits an action without a higher
// autonomy level. Sensitive policy confirmations can still apply separately.
func (l Level) Allows(action Action) bool {
	switch action {
	case ActionPullRequestMerge:
		return false
	case ActionFileWrite, ActionMutatingShell:
		return Normalize(l) != Low
	case ActionRemoteMutation, ActionBranch, ActionCommit, ActionPush, ActionPullRequestCreate:
		return Normalize(l) == High || Normalize(l) == Full
	default:
		return false
	}
}

// MinimumLevel returns the lowest autonomy level that permits action. PR merges
// intentionally return the empty string because no autonomy level permits them.
func MinimumLevel(action Action) Level {
	switch action {
	case ActionFileWrite, ActionMutatingShell:
		return Medium
	case ActionRemoteMutation, ActionBranch, ActionCommit, ActionPush, ActionPullRequestCreate:
		return High
	default:
		return ""
	}
}

// DenialMessage returns a user-facing reason for an action blocked at level.
func DenialMessage(level Level, action Action, detail string) string {
	level = Normalize(level)
	if action == ActionPullRequestMerge {
		msg := "autonomy " + level.String() + " blocks PR merges; even full autonomy leaves merging to a human"
		if strings.TrimSpace(detail) != "" {
			msg += " (" + strings.TrimSpace(detail) + ")"
		}

		return msg
	}

	minimum := MinimumLevel(action)
	if minimum == "" {
		return "autonomy " + level.String() + " blocks " + actionLabel(action)
	}

	msg := "autonomy " + level.String() + " blocks " + actionLabel(action) + "; rerun with --autonomy " + minimum.String()
	switch minimum {
	case Medium:
		msg += " or higher"
	case High:
		msg += " or full"
	}

	if strings.TrimSpace(detail) != "" {
		msg += " (" + strings.TrimSpace(detail) + ")"
	}

	return msg
}

func actionLabel(action Action) string {
	switch action {
	case ActionFileWrite:
		return "file writes"
	case ActionMutatingShell:
		return "mutating shell commands"
	case ActionRemoteMutation:
		return "remote service mutations"
	case ActionBranch:
		return "branch creation"
	case ActionCommit:
		return "git commits"
	case ActionPush:
		return "git pushes"
	case ActionPullRequestCreate:
		return "PR creation"
	case ActionPullRequestMerge:
		return "PR merges"
	default:
		return string(action)
	}
}
