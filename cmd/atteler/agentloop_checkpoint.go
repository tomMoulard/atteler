package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

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

	if cfg.AgentLoop.MaxModelCalls != nil {
		maxModelCalls := *cfg.AgentLoop.MaxModelCalls
		if maxModelCalls < 0 {
			return budget, errors.New("agent_loop.max_model_calls must be >= 0")
		}

		budget.MaxModelCalls = maxModelCalls
	}

	wallTime, err := agentLoopMaxWallTimeFromConfig(cfg)
	if err != nil {
		return budget, err
	}

	budget.MaxWallTime = wallTime

	return budget, nil
}

// agentLoopMaxWallTimeFromConfig parses the configured wall-time ceiling.
// Zero means no cap (the default when the field is unset or empty).
func agentLoopMaxWallTimeFromConfig(cfg appconfig.Config) (time.Duration, error) {
	if cfg.AgentLoop.MaxWallTime == nil {
		return 0, nil
	}

	raw := strings.TrimSpace(*cfg.AgentLoop.MaxWallTime)
	if raw == "" {
		return 0, nil
	}

	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("agent_loop.max_wall_time: %w", err)
	}

	if parsed < 0 {
		return 0, errors.New("agent_loop.max_wall_time must be >= 0")
	}

	return parsed, nil
}

// agentLoopCheckpointIntervalFromConfig returns the configured checkpoint
// interval. Zero (the default when unset) disables the interactive
// "continue?" prompt entirely.
func agentLoopCheckpointIntervalFromConfig(cfg appconfig.Config) (int, error) {
	if cfg.AgentLoop.CheckpointInterval == nil {
		return 0, nil
	}

	interval := *cfg.AgentLoop.CheckpointInterval
	if interval < 0 {
		return 0, errors.New("agent_loop.checkpoint_interval must be >= 0")
	}

	return interval, nil
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
