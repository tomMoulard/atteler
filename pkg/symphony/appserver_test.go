//nolint:gosec,gocritic,paralleltest,wsl_v5 // The fake executable script is local to this protocol test.
package symphony

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAppServerClient_RunTurnJSONLProtocol(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-app-server.sh")
	require.NoError(t, os.WriteFile(script, []byte(`#!/usr/bin/env bash
read -r line
printf '%s\n' '{"id":1,"result":{}}'
read -r line
read -r line
printf '%s\n' '{"id":2,"result":{"thread":{"id":"thread-1"}}}'
read -r line
printf '%s\n' '{"id":3,"result":{"turn":{"id":"turn-1","status":"inProgress"}}}'
printf '%s\n' '{"method":"thread/tokenUsage/updated","params":{"threadId":"thread-1","turnId":"turn-1","tokenUsage":{"total":{"inputTokens":3,"outputTokens":4,"totalTokens":7},"last":{"inputTokens":3,"outputTokens":4,"totalTokens":7,"cachedInputTokens":0,"reasoningOutputTokens":0}}}}'
printf '%s\n' '{"method":"turn/completed","params":{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed","items":[]}}}'
sleep 2
`), 0o700))

	var events []CodexEvent
	cfg := Config{
		Codex: CodexConfig{
			Command:     script,
			ReadTimeout: 5 * time.Second,
			TurnTimeout: 5 * time.Second,
		},
	}

	client, err := StartAppServer(context.Background(), cfg.Codex, dir, func(event CodexEvent) {
		events = append(events, event)
	})
	require.NoError(t, err)
	defer client.Close()

	threadID, err := client.StartThread(context.Background(), cfg, Issue{ID: "1", Identifier: "GH-1", Title: "Fix", State: "OPEN"}, dir)
	require.NoError(t, err)
	assert.Equal(t, "thread-1", threadID)

	err = client.RunTurn(context.Background(), cfg, threadID, "do it", dir)
	require.NoError(t, err)

	assert.Contains(t, eventNames(events), "session_started")
	assert.Contains(t, eventNames(events), "turn_started")
	assert.Contains(t, eventNames(events), "turn_completed")
	assert.Contains(t, eventNames(events), "notification")
}

func eventNames(events []CodexEvent) []string {
	names := make([]string, 0, len(events))
	for _, event := range events {
		names = append(names, event.Event)
	}

	return names
}
