//nolint:gosec,gocritic,wsl_v5 // The fake executable script is local to this protocol test.
package symphony

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/autonomy"
	"github.com/tommoulard/atteler/pkg/permission"
	"github.com/tommoulard/atteler/pkg/shell"
)

const testStderrStream = "stderr"

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
	assert.Contains(t, records[0].OperationKinds, string(permission.OperationWrite))
	assert.Contains(t, records[0].OperationKinds, string(permission.OperationNetwork))
	assert.Contains(t, records[0].OperationKinds, string(permission.OperationCredentialAccess))

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

func TestStartAppServer_PermissionPolicyDeniesWorkspaceWrite(t *testing.T) {
	dir := t.TempDir()
	auditDir := filepath.Join(t.TempDir(), "audit")
	t.Setenv(shell.EnvAuditDir, auditDir)

	marker := filepath.Join(dir, "started")
	script := filepath.Join(dir, "fake-app-server.sh")
	require.NoError(t, os.WriteFile(script, []byte(`#!/usr/bin/env bash
printf started > started
sleep 2
`), 0o700))

	policy := permission.DefaultPolicy()
	policy.SetMode(permission.OperationWrite, permission.ModeDeny)
	ctx := permission.ContextWithPolicy(context.Background(), &policy)

	client, err := StartAppServer(ctx, CodexConfig{Command: script}, dir, nil)
	require.Error(t, err)
	require.Nil(t, client)
	require.True(t, permission.ErrDenied(err))
	require.Contains(t, err.Error(), "permission.write.deny")
	require.NoFileExists(t, marker)

	records := readAppServerAuditRecords(t, auditDir)
	require.NotEmpty(t, records)
	assert.Equal(t, "denied", records[0].Decision)
	assert.Equal(t, "permission.write.deny", records[0].PermissionRule)
	assert.Contains(t, records[0].OperationKinds, string(permission.OperationWrite))
}

func TestAppServerClient_ApprovalRequestsUsePermissionPolicy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		method string
		params string
		deny   permission.OperationKind
		rule   string
	}{
		{
			name:   "command network",
			method: "item/commandExecution/requestApproval",
			params: `{"item":{"command":"curl https://example.invalid > out"}}`,
			deny:   permission.OperationNetwork,
			rule:   "permission.network.deny",
		},
		{
			name:   "file change write",
			method: "applyPatchApproval",
			params: `{"changes":[{"path":"file.txt","kind":"modify"}]}`,
			deny:   permission.OperationWrite,
			rule:   "permission.write.deny",
		},
		{
			name:   "file change delete",
			method: "applyPatchApproval",
			params: `{"changes":[{"path":"file.txt","kind":"delete"}]}`,
			deny:   permission.OperationMergeDelete,
			rule:   "permission.merge_delete.deny",
		},
		{
			name:   "session network permission",
			method: "item/permissions/requestApproval",
			params: `{"permissions":{"network":{"enabled":true}}}`,
			deny:   permission.OperationNetwork,
			rule:   "permission.network.deny",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			policy := permission.DefaultPolicy()
			policy.SetMode(tt.deny, permission.ModeDeny)

			auditDir := t.TempDir()
			ctx := permission.ContextWithPolicy(t.Context(), &policy)
			ctx = permission.ContextWithAuditDir(ctx, auditDir)

			stdin := &appServerTestStdin{}
			var events []CodexEvent
			client := &AppServerClient{
				stdin:         stdin,
				workspacePath: t.TempDir(),
				emit: func(event CodexEvent) {
					events = append(events, event)
				},
			}

			err := client.handleServerRequest(ctx, appServerMessage{
				ID:     1,
				Method: tt.method,
				Params: rawTestJSON(tt.params),
			})

			require.Error(t, err)
			require.True(t, permission.ErrDenied(err))
			assert.Contains(t, err.Error(), tt.rule)
			require.Len(t, events, 1)
			assert.Equal(t, "approval_denied", events[0].Event)
			assert.Contains(t, events[0].Message, tt.rule)

			var response map[string]any
			require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(stdin.String())), &response))
			require.Contains(t, response, "error")
			assert.Contains(t, stdin.String(), tt.rule)

			auditData, readErr := os.ReadFile(filepath.Join(auditDir, "side_effects.jsonl"))
			require.NoError(t, readErr)
			assert.Contains(t, string(auditData), tt.rule)
			assert.Contains(t, string(auditData), "symphony.codex_app_server")
		})
	}
}

func TestAppServerClient_MediumAutonomyDeniesGitPushApproval(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	approvalPath := filepath.Join(dir, "approval-response.json")
	script := filepath.Join(dir, "fake-app-server.sh")
	require.NoError(t, os.WriteFile(script, []byte(`#!/usr/bin/env bash
read -r line
printf '%s\n' '{"id":1,"result":{}}'
read -r line
read -r line
printf '%s\n' '{"id":2,"result":{"thread":{"id":"thread-1"}}}'
read -r line
printf '%s\n' '{"id":3,"result":{"turn":{"id":"turn-1","status":"inProgress"}}}'
printf '%s\n' '{"id":4,"method":"item/commandExecution/requestApproval","params":{"command":"git push --force origin feature"}}'
read -r approval
printf '%s\n' "$approval" > "$APPROVAL_PATH"
printf '%s\n' '{"method":"turn/completed","params":{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed","items":[]}}}'
sleep 2
`), 0o700))

	var events []CodexEvent
	cfg := Config{
		Autonomy: autonomy.Medium,
		Codex: CodexConfig{
			Command:     "env APPROVAL_PATH=" + strconv.Quote(approvalPath) + " " + strconv.Quote(script),
			ReadTimeout: 5 * time.Second,
			TurnTimeout: 5 * time.Second,
		},
	}

	client, err := StartAppServer(context.Background(), cfg.Codex, dir, func(event CodexEvent) {
		events = append(events, event)
	})
	require.NoError(t, err)
	defer client.Close()

	issue := Issue{ID: "1", Identifier: "GH-1", Title: "Fix", State: "OPEN"}
	threadID, err := client.StartThread(context.Background(), cfg, issue, dir)
	require.NoError(t, err)
	require.NoError(t, client.RunTurn(context.Background(), cfg, threadID, "do it", dir))

	data, err := os.ReadFile(approvalPath)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"decision":"deny"`)
	assert.Contains(t, string(data), "autonomy medium blocks git pushes")
	assert.Contains(t, eventNames(events), "approval_denied")
}

func TestAppServerClient_LowAutonomyDeniesCommandApproval(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	approvalPath := filepath.Join(dir, "approval-response.json")
	script := filepath.Join(dir, "fake-app-server.sh")
	require.NoError(t, os.WriteFile(script, []byte(`#!/usr/bin/env bash
read -r line
printf '%s\n' '{"id":1,"result":{}}'
read -r line
read -r line
printf '%s\n' '{"id":2,"result":{"thread":{"id":"thread-1"}}}'
read -r line
printf '%s\n' '{"id":3,"result":{"turn":{"id":"turn-1","status":"inProgress"}}}'
printf '%s\n' '{"id":4,"method":"item/commandExecution/requestApproval","params":{"command":"git status --short"}}'
read -r approval
printf '%s\n' "$approval" > "$APPROVAL_PATH"
printf '%s\n' '{"method":"turn/completed","params":{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed","items":[]}}}'
sleep 2
`), 0o700))

	var events []CodexEvent
	cfg := Config{
		Autonomy: autonomy.Low,
		Codex: CodexConfig{
			Command:     "env APPROVAL_PATH=" + strconv.Quote(approvalPath) + " " + strconv.Quote(script),
			ReadTimeout: 5 * time.Second,
			TurnTimeout: 5 * time.Second,
		},
	}

	client, err := StartAppServer(context.Background(), cfg.Codex, dir, func(event CodexEvent) {
		events = append(events, event)
	})
	require.NoError(t, err)
	defer client.Close()

	issue := Issue{ID: "1", Identifier: "GH-1", Title: "Fix", State: "OPEN"}
	threadID, err := client.StartThread(context.Background(), cfg, issue, dir)
	require.NoError(t, err)
	require.NoError(t, client.RunTurn(context.Background(), cfg, threadID, "do it", dir))

	data, err := os.ReadFile(approvalPath)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"decision":"deny"`)
	assert.Contains(t, string(data), "autonomy low is advisory-only")
	assert.Contains(t, eventNames(events), "approval_denied")
}

func TestAppServerClient_DeniesSensitiveCommandApprovalEvenAtHighAutonomy(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	approvalPath := filepath.Join(dir, "approval-response.json")
	script := filepath.Join(dir, "fake-app-server.sh")
	require.NoError(t, os.WriteFile(script, []byte(`#!/usr/bin/env bash
read -r line
printf '%s\n' '{"id":1,"result":{}}'
read -r line
read -r line
printf '%s\n' '{"id":2,"result":{"thread":{"id":"thread-1"}}}'
read -r line
printf '%s\n' '{"id":3,"result":{"turn":{"id":"turn-1","status":"inProgress"}}}'
printf '%s\n' '{"id":4,"method":"item/commandExecution/requestApproval","params":{"command":"sudo make install"}}'
read -r approval
printf '%s\n' "$approval" > "$APPROVAL_PATH"
printf '%s\n' '{"method":"turn/completed","params":{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed","items":[]}}}'
sleep 2
`), 0o700))

	var events []CodexEvent
	cfg := Config{
		Autonomy: autonomy.High,
		Codex: CodexConfig{
			Command:     "env APPROVAL_PATH=" + strconv.Quote(approvalPath) + " " + strconv.Quote(script),
			ReadTimeout: 5 * time.Second,
			TurnTimeout: 5 * time.Second,
		},
	}

	client, err := StartAppServer(context.Background(), cfg.Codex, dir, func(event CodexEvent) {
		events = append(events, event)
	})
	require.NoError(t, err)
	defer client.Close()

	issue := Issue{ID: "1", Identifier: "GH-1", Title: "Fix", State: "OPEN"}
	threadID, err := client.StartThread(context.Background(), cfg, issue, dir)
	require.NoError(t, err)
	require.NoError(t, client.RunTurn(context.Background(), cfg, threadID, "do it", dir))

	data, err := os.ReadFile(approvalPath)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"decision":"deny"`)
	assert.Contains(t, string(data), "privileged command requires confirmation")
	assert.Contains(t, string(data), "cannot auto-approve sensitive commands")
	assert.Contains(t, eventNames(events), "approval_denied")
}

func TestAppServerClient_DeniesCommandApprovalWithoutCommand(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	approvalPath := filepath.Join(dir, "approval-response.json")
	script := filepath.Join(dir, "fake-app-server.sh")
	require.NoError(t, os.WriteFile(script, []byte(`#!/usr/bin/env bash
read -r line
printf '%s\n' '{"id":1,"result":{}}'
read -r line
read -r line
printf '%s\n' '{"id":2,"result":{"thread":{"id":"thread-1"}}}'
read -r line
printf '%s\n' '{"id":3,"result":{"turn":{"id":"turn-1","status":"inProgress"}}}'
printf '%s\n' '{"id":4,"method":"item/commandExecution/requestApproval","params":{"summary":"run a command"}}'
read -r approval
printf '%s\n' "$approval" > "$APPROVAL_PATH"
printf '%s\n' '{"method":"turn/completed","params":{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed","items":[]}}}'
sleep 2
`), 0o700))

	var events []CodexEvent
	cfg := Config{
		Autonomy: autonomy.High,
		Codex: CodexConfig{
			Command:     "env APPROVAL_PATH=" + strconv.Quote(approvalPath) + " " + strconv.Quote(script),
			ReadTimeout: 5 * time.Second,
			TurnTimeout: 5 * time.Second,
		},
	}

	client, err := StartAppServer(context.Background(), cfg.Codex, dir, func(event CodexEvent) {
		events = append(events, event)
	})
	require.NoError(t, err)
	defer client.Close()

	issue := Issue{ID: "1", Identifier: "GH-1", Title: "Fix", State: "OPEN"}
	threadID, err := client.StartThread(context.Background(), cfg, issue, dir)
	require.NoError(t, err)
	require.NoError(t, client.RunTurn(context.Background(), cfg, threadID, "do it", dir))

	data, err := os.ReadFile(approvalPath)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"decision":"deny"`)
	assert.Contains(t, string(data), "cannot evaluate against selected autonomy")
	assert.Contains(t, eventNames(events), "approval_denied")
}

func TestAppServerClient_MediumAutonomyDeniesNetworkPermissionApproval(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	approvalPath := filepath.Join(dir, "approval-response.json")
	script := filepath.Join(dir, "fake-app-server.sh")
	require.NoError(t, os.WriteFile(script, []byte(`#!/usr/bin/env bash
read -r line
printf '%s\n' '{"id":1,"result":{}}'
read -r line
read -r line
printf '%s\n' '{"id":2,"result":{"thread":{"id":"thread-1"}}}'
read -r line
printf '%s\n' '{"id":3,"result":{"turn":{"id":"turn-1","status":"inProgress"}}}'
printf '%s\n' '{"id":4,"method":"item/permissions/requestApproval","params":{"permissions":{"network":{"enabled":true}}}}'
read -r approval
printf '%s\n' "$approval" > "$APPROVAL_PATH"
printf '%s\n' '{"method":"turn/completed","params":{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed","items":[]}}}'
sleep 2
`), 0o700))

	var events []CodexEvent
	cfg := Config{
		Autonomy: autonomy.Medium,
		Codex: CodexConfig{
			Command:     "env APPROVAL_PATH=" + strconv.Quote(approvalPath) + " " + strconv.Quote(script),
			ReadTimeout: 5 * time.Second,
			TurnTimeout: 5 * time.Second,
		},
	}

	client, err := StartAppServer(context.Background(), cfg.Codex, dir, func(event CodexEvent) {
		events = append(events, event)
	})
	require.NoError(t, err)
	defer client.Close()

	issue := Issue{ID: "1", Identifier: "GH-1", Title: "Fix", State: "OPEN"}
	threadID, err := client.StartThread(context.Background(), cfg, issue, dir)
	require.NoError(t, err)
	require.NoError(t, client.RunTurn(context.Background(), cfg, threadID, "do it", dir))

	data, err := os.ReadFile(approvalPath)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"decision":"deny"`)
	assert.Contains(t, string(data), "autonomy medium blocks remote service mutations")
	assert.Contains(t, string(data), "network permission escalation")
	assert.Contains(t, eventNames(events), "approval_denied")
}

func TestAppServerClient_FullAutonomyApprovesNetworkPermissionByPolicy(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	approvalPath := filepath.Join(dir, "approval-response.json")
	script := filepath.Join(dir, "fake-app-server.sh")
	require.NoError(t, os.WriteFile(script, []byte(`#!/usr/bin/env bash
read -r line
printf '%s\n' '{"id":1,"result":{}}'
read -r line
read -r line
printf '%s\n' '{"id":2,"result":{"thread":{"id":"thread-1"}}}'
read -r line
printf '%s\n' '{"id":3,"result":{"turn":{"id":"turn-1","status":"inProgress"}}}'
printf '%s\n' '{"id":4,"method":"item/permissions/requestApproval","params":{"permissions":{"network":{"enabled":true}}}}'
read -r approval
printf '%s\n' "$approval" > "$APPROVAL_PATH"
printf '%s\n' '{"method":"turn/completed","params":{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed","items":[]}}}'
sleep 2
`), 0o700))

	var events []CodexEvent
	cfg := Config{
		Autonomy: autonomy.Full,
		Codex: CodexConfig{
			Command:     "env APPROVAL_PATH=" + strconv.Quote(approvalPath) + " " + strconv.Quote(script),
			ReadTimeout: 5 * time.Second,
			TurnTimeout: 5 * time.Second,
		},
	}

	client, err := StartAppServer(context.Background(), cfg.Codex, dir, func(event CodexEvent) {
		events = append(events, event)
	})
	require.NoError(t, err)
	defer client.Close()

	issue := Issue{ID: "1", Identifier: "GH-1", Title: "Fix", State: "OPEN"}
	threadID, err := client.StartThread(context.Background(), cfg, issue, dir)
	require.NoError(t, err)
	require.NoError(t, client.RunTurn(context.Background(), cfg, threadID, "do it", dir))

	data, err := os.ReadFile(approvalPath)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"network":{"enabled":true}`)
	assert.Contains(t, string(data), `"scope":"session"`)
	assert.Contains(t, eventNames(events), "approval_auto_approved")
}

//nolint:paralleltest // Shares fake app-server pipe state; parallel race runs can close it before all readers drain.
func TestAppServerClient_RunTurnStreamsCommandOutputNotifications(t *testing.T) {
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
printf '%s\n' '{"method":"item/started","params":{"threadId":"thread-1","turnId":"turn-1","item":{"type":"commandExecution","id":"cmd-1","command":"printf first","cwd":"/tmp","processId":"proc-1","source":"agent","status":"running","commandActions":[],"aggregatedOutput":null,"exitCode":null,"durationMs":null},"startedAtMs":1}}'
printf '%s\n' '{"method":"item/commandExecution/outputDelta","params":{"threadId":"thread-1","turnId":"turn-1","itemId":"cmd-1","delta":"first\n"}}'
printf '%s\n' '{"method":"process/outputDelta","params":{"processHandle":"proc-1","stream":"stderr","deltaBase64":"d2Fybgo=","capReached":false}}'
sleep 0.4
printf '%s\n' '{"method":"item/completed","params":{"threadId":"thread-1","turnId":"turn-1","item":{"type":"commandExecution","id":"cmd-1","command":"printf first","cwd":"/tmp","processId":"proc-1","source":"agent","status":"completed","commandActions":[],"aggregatedOutput":"first\nwarn\n","exitCode":0,"durationMs":400},"completedAtMs":2}}'
printf '%s\n' '{"method":"turn/completed","params":{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed","items":[]}}}'
sleep 2
`), 0o700))

	events := make(chan CodexEvent, 16)
	cfg := Config{
		Codex: CodexConfig{
			Command:     script,
			ReadTimeout: 5 * time.Second,
			TurnTimeout: 5 * time.Second,
		},
	}

	client, err := StartAppServer(context.Background(), cfg.Codex, dir, func(event CodexEvent) {
		events <- event
	})
	require.NoError(t, err)
	defer client.Close()

	threadID, err := client.StartThread(context.Background(), cfg, Issue{ID: "1", Identifier: "GH-1", Title: "Fix", State: "OPEN"}, dir)
	require.NoError(t, err)

	runErr := make(chan error, 1)
	go func() {
		runErr <- client.RunTurn(context.Background(), cfg, threadID, "do it", dir)
	}()

	var stdoutEvent, stderrEvent CodexEvent
	var commandEvents []CodexEvent
	deadline := time.After(time.Second)
	for stdoutEvent.Event == "" || stderrEvent.Event == "" {
		select {
		case event := <-events:
			if isCommandEventName(event.Event) {
				commandEvents = append(commandEvents, event)
			}
			if event.Event == codexEventExecCommandOutputDelta && event.CommandID == "cmd-1" && event.OutputStream != testStderrStream {
				stdoutEvent = event
			}
			if event.Event == codexEventExecCommandOutputDelta && event.OutputStream == testStderrStream {
				stderrEvent = event
			}
		case err := <-runErr:
			require.Failf(t, "turn completed before command output streamed", "err=%v", err)
		case <-deadline:
			require.FailNow(t, "timed out waiting for command output events")
		}
	}

	require.NotEmpty(t, commandEvents)
	assert.Equal(t, codexEventExecCommandBegin, commandEvents[0].Event)
	assert.Equal(t, "cmd-1", stdoutEvent.CommandID)
	assert.Equal(t, "printf first", stdoutEvent.Command)
	assert.Equal(t, "first\n", stdoutEvent.OutputChunk)
	assert.Equal(t, "stdout", stdoutEvent.OutputStream)
	assert.Equal(t, "thread-1-turn-1", stdoutEvent.SessionID)
	assert.Equal(t, "cmd-1", stderrEvent.CommandID)
	assert.Equal(t, "proc-1", stderrEvent.ProcessID)
	assert.Equal(t, "printf first", stderrEvent.Command)
	assert.Equal(t, "warn\n", stderrEvent.OutputChunk)
	assert.Equal(t, testStderrStream, stderrEvent.OutputStream)

	select {
	case err := <-runErr:
		require.Failf(t, "turn completed before delayed completion notification", "err=%v", err)
	default:
	}

	select {
	case err := <-runErr:
		require.NoError(t, err)
	case <-time.After(time.Second):
		require.FailNow(t, "timed out waiting for turn completion")
	}

drain:
	for {
		select {
		case event := <-events:
			if isCommandEventName(event.Event) {
				commandEvents = append(commandEvents, event)
			}
		default:
			break drain
		}
	}

	assert.Equal(t, []string{
		codexEventExecCommandBegin,
		codexEventExecCommandOutputDelta,
		codexEventExecCommandOutputDelta,
		codexEventExecCommandEnd,
	}, eventNames(commandEvents))
}

func TestAppServerClient_CloseUnblocksReadLoopWhenLinesChannelIsFull(t *testing.T) {
	t.Parallel()

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
printf '%s\n' '{"method":"turn/completed","params":{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed","items":[]}}}'
for _ in $(seq 1 100); do
  printf '%s\n' '{"method":"thread/tokenUsage/updated","params":{"threadId":"thread-1","turnId":"turn-1","tokenUsage":{"total":{"inputTokens":1,"outputTokens":1,"totalTokens":2}}}}'
done
sleep 60
`), 0o700))

	cfg := Config{
		Codex: CodexConfig{
			Command:     script,
			ReadTimeout: 5 * time.Second,
			TurnTimeout: 5 * time.Second,
		},
	}

	client, err := StartAppServer(context.Background(), cfg.Codex, dir, nil)
	require.NoError(t, err)

	threadID, err := client.StartThread(context.Background(), cfg, Issue{ID: "1", Identifier: "GH-1", Title: "Fix", State: "OPEN"}, dir)
	require.NoError(t, err)
	require.NoError(t, client.RunTurn(context.Background(), cfg, threadID, "do it", dir))

	// After the final turn nobody drains c.lines; wait until the post-turn
	// notifications have filled the buffer so readLoop parks on its send.
	require.Eventually(t, func() bool {
		return len(client.lines) == cap(client.lines)
	}, 2*time.Second, 10*time.Millisecond, "post-turn notifications never filled the lines buffer")

	closeErr := make(chan error, 1)
	go func() { closeErr <- client.Close() }()

	select {
	case err := <-closeErr:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		require.FailNow(t, "Close did not return promptly while readLoop was parked on a full lines channel")
	}

	select {
	case <-client.readDone:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "readLoop goroutine did not exit after Close")
	}
}

func TestParseCommandNotification_DecodesCommandExecOutputDelta(t *testing.T) {
	t.Parallel()

	notification := parseCommandNotification(
		"command/exec/outputDelta",
		rawTestJSON(`{"processId":"proc-1","stream":"stdout","deltaBase64":"Zmlyc3QK","capReached":false}`),
	)

	require.True(t, notification.ok)
	assert.Equal(t, codexEventExecCommandOutputDelta, notification.event)
	assert.Equal(t, "proc-1", notification.commandID)
	assert.Equal(t, "proc-1", notification.processID)
	assert.Equal(t, "stdout", notification.stream)
	assert.Equal(t, "first\n", notification.chunk)
}

func TestParseCommandNotification_NormalizesNumericOutputFD(t *testing.T) {
	t.Parallel()

	notification := parseCommandNotification(
		"process/outputDelta",
		rawTestJSON(`{"processId":"proc-1","fd":2,"deltaBase64":"ZXJyCg==","capReached":false}`),
	)

	require.True(t, notification.ok)
	assert.Equal(t, codexEventExecCommandOutputDelta, notification.event)
	assert.Equal(t, "proc-1", notification.processID)
	assert.Equal(t, "stderr", notification.stream)
	assert.Equal(t, "err\n", notification.chunk)
}

func TestParseCommandNotification_AcceptsSnakeCaseCommandProtocol(t *testing.T) {
	t.Parallel()

	begin := parseCommandNotification(
		"item/started",
		rawTestJSON(`{"item":{"type":"command_execution","id":"cmd-1","command":"make test","process_id":"proc-1"}}`),
	)
	output := parseCommandNotification(
		"exec_command/output_delta",
		rawTestJSON(`{"process_id":"proc-1","stream":"out","delta":"ok\n"}`),
	)

	require.True(t, begin.ok)
	assert.Equal(t, codexEventExecCommandBegin, begin.event)
	assert.Equal(t, "cmd-1", begin.commandID)
	assert.Equal(t, "proc-1", begin.processID)
	assert.Equal(t, "make test", begin.command)
	require.True(t, output.ok)
	assert.Equal(t, codexEventExecCommandOutputDelta, output.event)
	assert.Equal(t, "stdout", output.stream)
	assert.Equal(t, "ok\n", output.chunk)
}

func TestParseCommandNotification_AcceptsExecCommandItemType(t *testing.T) {
	t.Parallel()

	notification := parseCommandNotification(
		"item/started",
		rawTestJSON(`{"item":{"type":"exec_command","id":"cmd-1","command":"make test","process_id":"proc-1"}}`),
	)

	require.True(t, notification.ok)
	assert.Equal(t, codexEventExecCommandBegin, notification.event)
	assert.Equal(t, "cmd-1", notification.commandID)
	assert.Equal(t, "proc-1", notification.processID)
	assert.Equal(t, "make test", notification.command)
}

func TestAppServerClient_EnrichCommandNotificationSupportsZeroValueClient(t *testing.T) {
	t.Parallel()

	client := &AppServerClient{}
	begin := client.enrichCommandNotification(commandNotification{
		ok:        true,
		event:     codexEventExecCommandBegin,
		commandID: "cmd-1",
		processID: "proc-1",
		command:   "make test",
	})
	output := client.enrichCommandNotification(commandNotification{
		ok:        true,
		event:     codexEventExecCommandOutputDelta,
		commandID: "cmd-1",
		chunk:     "ok\n",
	})

	assert.Equal(t, "make test", begin.command)
	assert.Equal(t, "make test", output.command)
}

func TestParseCommandNotification_TerminalInteraction(t *testing.T) {
	t.Parallel()

	notification := parseCommandNotification(
		"item/commandExecution/terminalInteraction",
		rawTestJSON(`{"threadId":"thread-1","turnId":"turn-1","itemId":"cmd-1","processId":"proc-1","stdin":"q"}`),
	)

	require.True(t, notification.ok)
	assert.Equal(t, codexEventTerminalInteraction, notification.event)
	assert.Equal(t, "cmd-1", notification.commandID)
	assert.Equal(t, "proc-1", notification.processID)
	assert.Equal(t, "q", notification.message)
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

func rawTestJSON(value string) json.RawMessage {
	return json.RawMessage(value)
}

type appServerTestStdin struct {
	strings.Builder
}

func (w *appServerTestStdin) Close() error {
	return nil
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

func isCommandEventName(name string) bool {
	switch name {
	case codexEventExecCommandBegin, codexEventExecCommandOutputDelta, codexEventExecCommandEnd:
		return true
	default:
		return false
	}
}
