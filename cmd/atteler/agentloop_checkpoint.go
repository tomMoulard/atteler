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

	"github.com/tommoulard/atteler/pkg/autonomy"
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

func agentLoopCheckpointPathForAutonomy(sessionPath string, level autonomy.Level) string {
	if !autonomy.Normalize(level).Allows(autonomy.ActionFileWrite) {
		return ""
	}

	return agentLoopCheckpointPath(sessionPath)
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
		maxOutputBytes, err := nonNegativeAgentLoopInt64Budget("agent_loop.max_output_bytes", *cfg.AgentLoop.MaxOutputBytes)
		if err != nil {
			return budget, err
		}

		budget.MaxOutputBytes = maxOutputBytes
	}

	if err := applyAgentLoopTokenBudgets(&budget, cfg.AgentLoop); err != nil {
		return budget, err
	}

	if err := applyAgentLoopCountBudgets(&budget, cfg.AgentLoop); err != nil {
		return budget, err
	}

	wallTime, err := agentLoopMaxWallTimeFromConfig(cfg)
	if err != nil {
		return budget, err
	}

	budget.MaxWallTime = wallTime

	return budget, nil
}

func applyAgentLoopTokenBudgets(budget *llm.AgentLoopBudget, cfg appconfig.AgentLoopConfig) error {
	if cfg.MaxCostMicros != nil {
		maxCostMicros, err := nonNegativeAgentLoopInt64Budget("agent_loop.max_cost_micros", *cfg.MaxCostMicros)
		if err != nil {
			return err
		}

		budget.MaxCostMicros = maxCostMicros
	}

	if cfg.MaxInputTokens != nil {
		maxInputTokens, err := nonNegativeAgentLoopIntBudget("agent_loop.max_input_tokens", *cfg.MaxInputTokens)
		if err != nil {
			return err
		}

		budget.MaxInputTokens = maxInputTokens
	}

	if cfg.MaxOutputTokens != nil {
		maxOutputTokens, err := nonNegativeAgentLoopIntBudget("agent_loop.max_output_tokens", *cfg.MaxOutputTokens)
		if err != nil {
			return err
		}

		budget.MaxOutputTokens = maxOutputTokens
	}

	if cfg.MaxTotalTokens != nil {
		maxTotalTokens, err := nonNegativeAgentLoopIntBudget("agent_loop.max_total_tokens", *cfg.MaxTotalTokens)
		if err != nil {
			return err
		}

		budget.MaxTotalTokens = maxTotalTokens
	}

	return nil
}

func applyAgentLoopCountBudgets(budget *llm.AgentLoopBudget, cfg appconfig.AgentLoopConfig) error {
	if cfg.MaxIterations != nil {
		maxIterations, err := nonNegativeAgentLoopIntBudget("agent_loop.max_iterations", *cfg.MaxIterations)
		if err != nil {
			return err
		}

		budget.MaxIterations = maxIterations
	}

	if cfg.MaxModelCalls != nil {
		maxModelCalls, err := nonNegativeAgentLoopIntBudget("agent_loop.max_model_calls", *cfg.MaxModelCalls)
		if err != nil {
			return err
		}

		budget.MaxModelCalls = maxModelCalls
	}

	if cfg.MaxToolCalls != nil {
		maxToolCalls, err := nonNegativeAgentLoopIntBudget("agent_loop.max_tool_calls", *cfg.MaxToolCalls)
		if err != nil {
			return err
		}

		budget.MaxToolCalls = maxToolCalls
	}

	return nil
}

func nonNegativeAgentLoopIntBudget(name string, value int) (int, error) {
	if value < 0 {
		return 0, fmt.Errorf("%s must be >= 0", name)
	}

	return value, nil
}

func nonNegativeAgentLoopInt64Budget(name string, value int64) (int64, error) {
	if value < 0 {
		return 0, fmt.Errorf("%s must be >= 0", name)
	}

	return value, nil
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

func agentLoopBudgetModelSettingsEventMetadata(budget llm.AgentLoopBudget, reasoningLevel, modelMode string) map[string]string {
	metadata := agentLoopBudgetEventMetadata(budget)
	if metadata == nil {
		metadata = make(map[string]string)
	}

	if reasoningLevel = strings.TrimSpace(reasoningLevel); reasoningLevel != "" {
		metadata["reasoning_level"] = reasoningLevel
	}

	if modelMode = strings.TrimSpace(modelMode); modelMode != "" {
		metadata["model_mode"] = modelMode
	}

	return metadata
}

func sessionRunEventMetadata(budget llm.AgentLoopBudget, level autonomy.Level, modelSettings ...string) map[string]string {
	reasoningLevel := ""
	modelMode := ""
	if len(modelSettings) > 0 {
		reasoningLevel = modelSettings[0]
	}
	if len(modelSettings) > 1 {
		modelMode = modelSettings[1]
	}

	metadata := agentLoopBudgetModelSettingsEventMetadata(budget, reasoningLevel, modelMode)
	if metadata == nil {
		metadata = make(map[string]string, 1)
	}
	metadata["autonomy"] = autonomy.Normalize(level).String()

	return metadata
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
