package main

import (
	"strings"

	"github.com/tommoulard/atteler/pkg/autonomy"
	"github.com/tommoulard/atteler/pkg/llm"
)

func prependAutonomyInstructions(params *llm.CompleteParams, level autonomy.Level) {
	if params == nil {
		return
	}

	level = autonomy.Normalize(level)

	instruction := autonomyInstruction(level)
	if strings.TrimSpace(instruction) == "" {
		return
	}

	params.Messages = append([]llm.Message{{Role: llm.RoleSystem, Content: instruction}}, params.Messages...)
}

func autonomyInstruction(level autonomy.Level) string {
	switch autonomy.Normalize(level) {
	case autonomy.Low:
		return "Autonomy: low. Advisory-only mode. Produce a concise plan or guidance only. Do not modify files, run mutating commands, commit, push, open PRs, or claim that those actions were performed. If implementation is required, say that --autonomy medium or higher is required for local edits/tests, and --autonomy high or full is required for branch/commit/push/PR creation."
	case autonomy.Medium:
		return "Autonomy: medium. Local implementation mode. You may edit local files and run validation commands, but must not create branches, commit, push, open PRs, or merge PRs. If publishing is required, explain that --autonomy high or full is required."
	case autonomy.High:
		return "Autonomy: high. PR preparation mode. You may create branches, edit files, run validation, commit, push, and open PRs when needed. Do not merge PRs; final merge remains a human action. Sensitive operations can still require confirmation."
	case autonomy.Full:
		return "Autonomy: full. End-to-end issue implementation mode through PR creation and validation. Do not merge PRs; final merge remains a human action. Sensitive operations can still require confirmation or policy approval."
	default:
		return ""
	}
}
