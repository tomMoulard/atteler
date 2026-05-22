//nolint:gosec,gocritic,wsl_v5 // The fake executable script is local to this protocol test.
package symphony

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/shell"
)

func TestAppServerClient_RunTurnJSONLProtocol(t *testing.T) {
	dir := t.TempDir()
	auditDir := filepath.Join(t.TempDir(), "audit")
	t.Setenv(shell.EnvAuditDir, auditDir)

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

	issue := Issue{ID: "issue-node-1", Identifier: "GH-1", Title: "Fix", State: "OPEN"}

	client, err := StartAppServerForIssue(context.Background(), cfg.Codex, issue, dir, func(event CodexEvent) {
		events = append(events, event)
	})
	require.NoError(t, err)
	defer client.Close()

	records := readAppServerAuditRecords(t, auditDir)
	require.NotEmpty(t, records)
	assert.Equal(t, "symphony.codex_app_server", records[0].Caller)
	assert.Equal(t, issue.ID, records[0].IssueID)
	assert.Equal(t, issue.Identifier, records[0].IssueIdentifier)

	threadID, err := client.StartThread(context.Background(), cfg, issue, dir)
	require.NoError(t, err)
	assert.Equal(t, "thread-1", threadID)

	err = client.RunTurn(context.Background(), cfg, threadID, "do it", dir)
	require.NoError(t, err)

	assert.Contains(t, eventNames(events), "session_started")
	assert.Contains(t, eventNames(events), "turn_started")
	assert.Contains(t, eventNames(events), "turn_completed")
	assert.Contains(t, eventNames(events), "notification")
}

func TestAppServerClient_RequiresActiveContext(t *testing.T) {
	t.Parallel()

	cfg := Config{Codex: CodexConfig{Command: "unused"}}

	_, err := StartAppServer(nil, cfg.Codex, t.TempDir(), nil) //nolint:staticcheck // Verify nil contexts are rejected instead of panicking.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "context is required")

	client := &AppServerClient{}

	_, err = client.StartThread(nil, cfg, Issue{}, t.TempDir()) //nolint:staticcheck // Verify nil contexts are rejected instead of panicking.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "context is required")

	err = client.RunTurn(nil, cfg, "thread-1", "prompt", t.TempDir()) //nolint:staticcheck // Verify nil contexts are rejected instead of panicking.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "context is required")

	err = client.respond(nil, 1, map[string]any{"ok": true}) //nolint:staticcheck // Verify nil contexts are rejected instead of panicking.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "context is required")
}

func TestAppServerClient_RejectsCanceledContextBeforeProtocolWrite(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	client := &AppServerClient{}
	cfg := Config{Codex: CodexConfig{Command: "unused"}}

	_, err := StartAppServer(ctx, cfg.Codex, t.TempDir(), nil)
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)

	_, err = client.StartThread(ctx, cfg, Issue{}, t.TempDir())
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)

	err = client.RunTurn(ctx, cfg, "thread-1", "prompt", t.TempDir())
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)

	err = client.respond(ctx, 1, map[string]any{"ok": true})
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
}

func readAppServerAuditRecords(t *testing.T, auditDir string) []shell.AuditRecord {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(auditDir, "commands.jsonl"))
	require.NoError(t, err)

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	records := make([]shell.AuditRecord, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}

		var record shell.AuditRecord
		require.NoError(t, json.Unmarshal([]byte(line), &record))
		records = append(records, record)
	}

	return records
}

func eventNames(events []CodexEvent) []string {
	names := make([]string, 0, len(events))
	for _, event := range events {
		names = append(names, event.Event)
	}

	return names
}
