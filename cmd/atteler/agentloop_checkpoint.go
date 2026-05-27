package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
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
	var (
		budget llm.AgentLoopBudget
		err    error
	)

	budget.MaxOutputBytes, err = nonNegativeAgentLoopInt64("agent_loop.max_output_bytes", cfg.AgentLoop.MaxOutputBytes)
	if err != nil {
		return budget, err
	}

	budget.MaxCostMicros, err = nonNegativeAgentLoopInt64("agent_loop.max_cost_micros", cfg.AgentLoop.MaxCostMicros)
	if err != nil {
		return budget, err
	}

	budget.MaxInputTokens, err = nonNegativeAgentLoopInt("agent_loop.max_input_tokens", cfg.AgentLoop.MaxInputTokens)
	if err != nil {
		return budget, err
	}

	budget.MaxOutputTokens, err = nonNegativeAgentLoopInt("agent_loop.max_output_tokens", cfg.AgentLoop.MaxOutputTokens)
	if err != nil {
		return budget, err
	}

	budget.MaxTotalTokens, err = nonNegativeAgentLoopInt("agent_loop.max_total_tokens", cfg.AgentLoop.MaxTotalTokens)
	if err != nil {
		return budget, err
	}

	budget.MaxIterations, err = nonNegativeAgentLoopInt("agent_loop.max_iterations", cfg.AgentLoop.MaxIterations)
	if err != nil {
		return budget, err
	}

	budget.MaxModelCalls, err = nonNegativeAgentLoopInt("agent_loop.max_model_calls", cfg.AgentLoop.MaxModelCalls)
	if err != nil {
		return budget, err
	}

	budget.MaxToolCalls, err = nonNegativeAgentLoopInt("agent_loop.max_tool_calls", cfg.AgentLoop.MaxToolCalls)
	if err != nil {
		return budget, err
	}

	wallTime, err := agentLoopMaxWallTimeFromConfig(cfg)
	if err != nil {
		return budget, err
	}

	budget.MaxWallTime = wallTime

	return budget, nil
}

func nonNegativeAgentLoopInt(name string, value *int) (int, error) {
	if value == nil {
		return 0, nil
	}

	if *value < 0 {
		return 0, fmt.Errorf("%s must be >= 0", name)
	}

	return *value, nil
}

func nonNegativeAgentLoopInt64(name string, value *int64) (int64, error) {
	if value == nil {
		return 0, nil
	}

	if *value < 0 {
		return 0, fmt.Errorf("%s must be >= 0", name)
	}

	return *value, nil
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

func agentLoopBudgetEventMetadata(budget llm.AgentLoopBudget) map[string]string {
	data, err := json.Marshal(budget)
	if err != nil {
		return nil
	}

	return map[string]string{
		"agent_loop_budget": string(data),
	}
}

func agentLoopCostEstimatorForBudget(
	reg *llm.Registry,
	model string,
	fallbackModels []string,
	budget llm.AgentLoopBudget,
) (llm.AgentLoopCostEstimator, error) {
	if budget.MaxCostMicros <= 0 {
		return nil, nil
	}

	estimator, err := reg.AgentLoopCostEstimator(model, fallbackModels)
	if err != nil {
		return nil, fmt.Errorf("agent_loop.max_cost_micros: %w", err)
	}

	return estimator, nil
}

func formatAgentLoopBudgetCompact(budget llm.AgentLoopBudget) string {
	if budget.IsZero() {
		return ""
	}

	parts := make([]string, 0, 9)
	if budget.MaxIterations > 0 {
		parts = append(parts, "iter="+strconv.Itoa(budget.MaxIterations))
	}

	if budget.MaxModelCalls > 0 {
		parts = append(parts, "model="+strconv.Itoa(budget.MaxModelCalls))
	}

	if budget.MaxToolCalls > 0 {
		parts = append(parts, "tool="+strconv.Itoa(budget.MaxToolCalls))
	}

	if budget.MaxWallTime > 0 {
		parts = append(parts, "wall="+budget.MaxWallTime.String())
	}

	if budget.MaxInputTokens > 0 {
		parts = append(parts, "in="+formatTokenCount(budget.MaxInputTokens))
	}

	if budget.MaxOutputTokens > 0 {
		parts = append(parts, "out="+formatTokenCount(budget.MaxOutputTokens))
	}

	if budget.MaxTotalTokens > 0 {
		parts = append(parts, "total="+formatTokenCount(budget.MaxTotalTokens))
	}

	if budget.MaxOutputBytes > 0 {
		parts = append(parts, "bytes="+strconv.FormatInt(budget.MaxOutputBytes, 10))
	}

	if budget.MaxCostMicros > 0 {
		parts = append(parts, "costµ="+strconv.FormatInt(budget.MaxCostMicros, 10))
	}

	return strings.Join(parts, ",")
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
