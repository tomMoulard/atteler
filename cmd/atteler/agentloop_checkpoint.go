package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	appconfig "github.com/tommoulard/atteler/pkg/config"
	"github.com/tommoulard/atteler/pkg/llm"
)

func agentLoopCheckpointPath(sessionPath string) string {
	sessionPath = strings.TrimSpace(sessionPath)
	if sessionPath == "" {
		return ""
	}

	ext := filepath.Ext(sessionPath)
	if ext == "" {
		return sessionPath + ".agentloop.jsonl"
	}

	return strings.TrimSuffix(sessionPath, ext) + ".agentloop.jsonl"
}

func agentLoopCheckpointSink(path string) llm.AgentLoopCheckpointSink {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}

	return llm.NewAgentLoopJSONLCheckpoint(path)
}

func agentLoopBudgetFromConfig(cfg appconfig.Config) (llm.AgentLoopBudget, error) {
	var budget llm.AgentLoopBudget

	if cfg.AgentLoop.MaxOutputBytes != nil {
		maxOutputBytes := *cfg.AgentLoop.MaxOutputBytes
		if maxOutputBytes < 0 {
			return budget, errors.New("agent_loop.max_output_bytes must be >= 0")
		}

		budget.MaxOutputBytes = maxOutputBytes
	}

	if cfg.AgentLoop.MaxTotalTokens != nil {
		maxTotalTokens := *cfg.AgentLoop.MaxTotalTokens
		if maxTotalTokens < 0 {
			return budget, errors.New("agent_loop.max_total_tokens must be >= 0")
		}

		budget.MaxTotalTokens = maxTotalTokens
	}

	if cfg.AgentLoop.MaxIterations != nil {
		maxIterations := *cfg.AgentLoop.MaxIterations
		if maxIterations < 0 {
			return budget, errors.New("agent_loop.max_iterations must be >= 0")
		}

		budget.MaxIterations = maxIterations
	}

	return budget, nil
}

func agentLoopToolOutputLimit(ctx context.Context) int64 {
	snapshot, ok := llm.AgentLoopBudgetSnapshotFromContext(ctx)
	if !ok {
		return 0
	}

	return snapshot.RemainingOutputBytes
}

func agentLoopError(err error, checkpointPath string) error {
	if err == nil {
		return nil
	}

	checkpointPath = strings.TrimSpace(checkpointPath)
	if checkpointPath == "" {
		return err
	}

	return fmt.Errorf("agent loop (checkpoint %s): %w", checkpointPath, err)
}
