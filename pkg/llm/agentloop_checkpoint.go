package llm

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// AgentLoopStepKind identifies what a checkpoint record describes.
type AgentLoopStepKind string

const (
	// AgentLoopStepModelResponse records one model response and request summary.
	AgentLoopStepModelResponse AgentLoopStepKind = "model_response"
	// AgentLoopStepToolCall records one tool policy decision and result.
	AgentLoopStepToolCall AgentLoopStepKind = "tool_call"
	// AgentLoopStepStop records the reason the loop stopped.
	AgentLoopStepStop AgentLoopStepKind = "stop"
)

// AgentLoopStopKind classifies why the loop stopped.
type AgentLoopStopKind string

const (
	// AgentLoopStopFinalResponse means the model returned a non-tool final answer.
	AgentLoopStopFinalResponse AgentLoopStopKind = "final_response"
	// AgentLoopStopMaxIterations means the iteration budget was exhausted.
	AgentLoopStopMaxIterations AgentLoopStopKind = "max_iterations"
	// AgentLoopStopMaxModelCalls means the model-call budget was exhausted.
	AgentLoopStopMaxModelCalls AgentLoopStopKind = "max_model_calls"
	// AgentLoopStopMaxToolCalls means the tool-call budget was exhausted.
	AgentLoopStopMaxToolCalls AgentLoopStopKind = "max_tool_calls"
	// AgentLoopStopWallTime means the wall-clock budget was exhausted.
	AgentLoopStopWallTime AgentLoopStopKind = "wall_time"
	// AgentLoopStopTokenBudget means a token budget was exceeded.
	AgentLoopStopTokenBudget AgentLoopStopKind = "token_budget"
	// AgentLoopStopCostBudget means an estimated cost budget was exceeded.
	AgentLoopStopCostBudget AgentLoopStopKind = "cost_budget"
	// AgentLoopStopOutputBytes means the tool-output byte budget was exceeded.
	AgentLoopStopOutputBytes AgentLoopStopKind = "output_bytes"
	// AgentLoopStopPolicyDenied means policy denied a requested tool.
	AgentLoopStopPolicyDenied AgentLoopStopKind = "policy_denied"
	// AgentLoopStopConfirmationRequired means required confirmation was unavailable or declined.
	AgentLoopStopConfirmationRequired AgentLoopStopKind = "confirmation_required"
	// AgentLoopStopDryRun means policy selected dry-run behavior.
	AgentLoopStopDryRun AgentLoopStopKind = "dry_run"
	// AgentLoopStopUserCheckpoint means the legacy continuation callback stopped the loop.
	AgentLoopStopUserCheckpoint AgentLoopStopKind = "user_checkpoint"
	// AgentLoopStopCanceled means context cancellation stopped the loop.
	AgentLoopStopCanceled AgentLoopStopKind = "canceled"
	// AgentLoopStopModelError means a model call failed.
	AgentLoopStopModelError AgentLoopStopKind = "model_error"
	// AgentLoopStopCheckpointError means checkpoint persistence failed.
	AgentLoopStopCheckpointError AgentLoopStopKind = "checkpoint_error"
)

// AgentLoopStopCondition is a durable, structured explanation for loop exit.
type AgentLoopStopCondition struct {
	Kind        AgentLoopStopKind `json:"kind"`
	Reason      string            `json:"reason"`
	MatchedRule string            `json:"matched_rule,omitempty"`
}

// AgentLoopModelRequestSummary records the request shape without copying the
// whole transcript into every checkpoint.
type AgentLoopModelRequestSummary struct {
	Model          string   `json:"model"`
	FallbackModels []string `json:"fallback_models,omitempty"`
	ToolNames      []string `json:"tool_names,omitempty"`
	MessageCount   int      `json:"message_count"`
	MessageBytes   int      `json:"message_bytes"`
	MaxTokens      int      `json:"max_tokens,omitempty"`
}

// AgentLoopModelResponseSummary records enough of a model response to audit or
// replay a tool-use turn.
type AgentLoopModelResponseSummary struct {
	StopReason        StopReason `json:"stop_reason"`
	Model             string     `json:"model"`
	Content           string     `json:"content,omitempty"`
	ToolCalls         []ToolCall `json:"tool_calls,omitempty"`
	ContentBytes      int        `json:"content_bytes"`
	InputTokens       int        `json:"input_tokens"`
	CachedInputTokens int        `json:"cached_input_tokens"`
	CacheWriteTokens  int        `json:"cache_write_tokens,omitempty"`
	OutputTokens      int        `json:"output_tokens"`
}

// AgentLoopStep is one durable checkpoint record in the tool loop ledger.
//
//nolint:govet // Field order follows JSON/audit readability rather than padding minimization.
type AgentLoopStep struct {
	ToolCallIndex int                            `json:"tool_call_index,omitempty"`
	Sequence      int                            `json:"sequence"`
	Iteration     int                            `json:"iteration"`
	At            time.Time                      `json:"at"`
	Kind          AgentLoopStepKind              `json:"kind"`
	ModelRequest  *AgentLoopModelRequestSummary  `json:"model_request,omitempty"`
	ModelResponse *AgentLoopModelResponseSummary `json:"model_response,omitempty"`
	ToolCall      *ToolCall                      `json:"tool_call,omitempty"`
	Policy        *ToolPolicyDecision            `json:"policy,omitempty"`
	ToolBudget    *AgentLoopBudgetSnapshot       `json:"tool_budget,omitempty"`
	ToolResult    *ToolResult                    `json:"tool_result,omitempty"`
	PromptResult  *ToolResult                    `json:"prompt_result,omitempty"`
	Usage         AgentLoopUsage                 `json:"usage"`
	StopCondition *AgentLoopStopCondition        `json:"stop_condition,omitempty"`
}

// AgentLoopCheckpointSink persists checkpoint records as the loop progresses.
type AgentLoopCheckpointSink interface {
	SaveAgentLoopStep(ctx context.Context, step AgentLoopStep) error
}

// AgentLoopCheckpointFunc adapts a function into an AgentLoopCheckpointSink.
type AgentLoopCheckpointFunc func(ctx context.Context, step AgentLoopStep) error

// SaveAgentLoopStep implements AgentLoopCheckpointSink.
func (f AgentLoopCheckpointFunc) SaveAgentLoopStep(ctx context.Context, step AgentLoopStep) error {
	return f(ctx, step)
}

// AgentLoopLedger is an in-memory checkpoint sink useful for tests and callers
// that want structured inspection without parsing chat transcripts.
type AgentLoopLedger struct {
	Steps []AgentLoopStep `json:"steps"`
}

// SaveAgentLoopStep implements AgentLoopCheckpointSink.
func (l *AgentLoopLedger) SaveAgentLoopStep(ctx context.Context, step AgentLoopStep) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("llm: save in-memory checkpoint canceled: %w", err)
	}

	if l == nil {
		return nil
	}

	l.Steps = append(l.Steps, step)

	return nil
}

// AgentLoopJSONLCheckpoint appends checkpoint records to a JSON Lines file.
type AgentLoopJSONLCheckpoint struct {
	path string
}

// NewAgentLoopJSONLCheckpoint creates a JSONL checkpoint sink at path.
func NewAgentLoopJSONLCheckpoint(path string) *AgentLoopJSONLCheckpoint {
	return &AgentLoopJSONLCheckpoint{path: path}
}

// Path returns the checkpoint file path.
func (c *AgentLoopJSONLCheckpoint) Path() string {
	if c == nil {
		return ""
	}

	return c.path
}

// SaveAgentLoopStep implements AgentLoopCheckpointSink.
func (c *AgentLoopJSONLCheckpoint) SaveAgentLoopStep(ctx context.Context, step AgentLoopStep) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("llm: save checkpoint canceled: %w", err)
	}

	if c == nil || strings.TrimSpace(c.path) == "" {
		return errors.New("llm: agent loop checkpoint path is required")
	}

	dir := filepath.Dir(c.path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("llm: create checkpoint dir: %w", err)
	}

	file, err := os.OpenFile(c.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("llm: open checkpoint %s: %w", c.path, err)
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	if err := encoder.Encode(step); err != nil {
		return fmt.Errorf("llm: write checkpoint %s: %w", c.path, err)
	}

	return nil
}

// LoadAgentLoopLedger reads a JSONL checkpoint file produced by
// AgentLoopJSONLCheckpoint.
func LoadAgentLoopLedger(path string) (*AgentLoopLedger, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("llm: open checkpoint %s: %w", path, err)
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	ledger := &AgentLoopLedger{}
	lineNo := 0

	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			lineNo++

			line = []byte(strings.TrimSpace(string(line)))
			if len(line) > 0 {
				var step AgentLoopStep
				if decodeErr := json.Unmarshal(line, &step); decodeErr != nil {
					return nil, fmt.Errorf("llm: parse checkpoint %s line %d: %w", path, lineNo, decodeErr)
				}

				ledger.Steps = append(ledger.Steps, step)
			}
		}

		if err == nil {
			continue
		}

		if errors.Is(err, io.EOF) {
			break
		}

		return nil, fmt.Errorf("llm: read checkpoint %s: %w", path, err)
	}

	return ledger, nil
}
