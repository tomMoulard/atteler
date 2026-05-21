package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

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
