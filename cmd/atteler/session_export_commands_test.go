package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/permission"
	"github.com/tommoulard/atteler/pkg/session"
)

func TestExportSession_JSONUsesRedactedMachineExport(t *testing.T) { //nolint:paralleltest // captures process-global stdout.
	const secret = "sk-proj-abcdefghijklmnopqrstuvwx1234567890"

	sessionState := session.Session{
		ID:       "abc",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "key=" + secret + " path=/Users/tom/project"}},
	}

	var err error

	out := captureStdoutForExport(t, func() {
		err = exportSession(sessionState, "json")
	})
	require.NoError(t, err)

	assert.Contains(t, out, `"manifest"`)
	assert.Contains(t, out, `"redaction_profile": "redacted-shareable"`)
	assert.Contains(t, out, "[REDACTED_PATH]")
	assert.NotContains(t, out, secret)
	assert.NotContains(t, out, "/Users/tom")
}

func TestExportSession_PrivateJSONIsExplicit(t *testing.T) { //nolint:paralleltest // captures process-global stdout.
	const secret = "sk-proj-abcdefghijklmnopqrstuvwx1234567890"

	sessionState := session.Session{
		ID:       "abc",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "key=" + secret + " path=/Users/tom/project"}},
	}

	var err error

	out := captureStdoutForExport(t, func() {
		err = exportSession(sessionState, "private-json")
	})
	require.NoError(t, err)

	assert.Contains(t, out, `"redaction_profile": "private-full"`)
	assert.Contains(t, out, `"privacy_notice": "Private full-fidelity export.`)
	assert.Contains(t, out, secret)
	assert.Contains(t, out, "/Users/tom/project")
}

func TestWriteSessionExport_JSONUsesRedactedMachineExport(t *testing.T) {
	t.Parallel()

	secret := "sk-" + "proj-fileexportabcdefghijklmnop"
	sessionState := session.Session{
		ID:       "abc",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "key=" + secret + " path=/Users/tom/project"}},
	}
	path := filepath.Join(t.TempDir(), "session.json")

	require.NoError(t, writeSessionExport(context.Background(), sessionState, path))

	data, err := os.ReadFile(path)
	require.NoError(t, err)

	out := string(data)

	assert.Contains(t, out, `"manifest"`)
	assert.Contains(t, out, `"redaction_profile": "redacted-shareable"`)
	assert.Contains(t, out, "[REDACTED_PATH]")
	assert.NotContains(t, out, secret)
	assert.NotContains(t, out, "/Users/tom")
}

func TestWriteSessionExportPermissionPolicyDeniesWriteBeforeCreatingDir(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	path := filepath.Join(root, "nested", "session.md")

	policy := permission.DefaultPolicy()
	policy.SetMode(permission.OperationWrite, permission.ModeDeny)

	ctx := permission.ContextWithAuditDir(t.Context(), filepath.Join(root, "audit"))
	ctx = permission.ContextWithPolicy(ctx, &policy)

	err := writeSessionExport(ctx, session.New("gpt-test", nil), path)
	require.Error(t, err)
	assert.True(t, permission.ErrDenied(err))
	assert.Contains(t, err.Error(), "permission.write.deny")
	assert.NoDirExists(t, filepath.Dir(path))
}

func TestFormatTranscriptProvenanceIncludesReplayInputs(t *testing.T) {
	t.Parallel()

	sessionState := session.New("openai/gpt-test", []llm.Message{{Role: llm.RoleUser, Content: "summarize"}})
	sessionState.EventLog = &session.EventLogMetadata{
		LastHash:   "sha256:event-log",
		EventCount: 12,
	}
	sessionState.ProviderCalls = []session.ProviderCall{{
		ID:             "call-1",
		Source:         "run_once",
		Provider:       "openai",
		RequestedModel: "openai/gpt-test",
		ResponseModel:  "openai/gpt-test",
		PromptHash:     "sha256:prompt",
		ConfigHash:     "sha256:call-config",
		RequestMessages: []llm.Message{{
			Role: llm.RoleAssistant,
			ToolCalls: []llm.ToolCall{{
				ID:   "tool-1",
				Name: "read_file",
			}},
		}, {
			Role: llm.RoleTool,
			ToolResult: &llm.ToolResult{
				ToolCallID: "tool-1",
				Content:    "ok",
			},
		}},
	}}
	require.True(t, sessionState.AddArtifact(session.Artifact{
		Path:      "reports/replay.md",
		Kind:      "report",
		CreatedAt: sessionState.CreatedAt,
	}))
	require.True(t, sessionState.UpsertMultiAgentRun(session.MultiAgentRun{
		ID:     "run-1",
		Kind:   session.MultiAgentRunKindReview,
		Status: session.MultiAgentRunStatusCompleted,
		Model:  "openai/gpt-test",
		Usage:  session.MultiAgentRunUsage{ModelCalls: 1, InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
		Gates:  []session.MultiAgentRunGate{{Name: "tests", Passed: true}},
	}))

	line := formatTranscriptProvenance(sessionState)

	assert.Contains(t, line, "Provenance\t")
	assert.Contains(t, line, "config_hash=sha256:")
	assert.Contains(t, line, "providers=openai")
	assert.Contains(t, line, "models=openai/gpt-test")
	assert.Contains(t, line, "event_hash=sha256:event-log")
	assert.Contains(t, line, "events=12")
	assert.Contains(t, line, "provider_calls=1")
	assert.Contains(t, line, "prompt_hash=sha256:prompt")
	assert.Contains(t, line, "call_config_hash=sha256:call-config")
	assert.Contains(t, line, "tool_calls=1")
	assert.Contains(t, line, "tool_results=1")
	assert.Contains(t, line, "total_tokens=15")
	assert.Contains(t, line, "files=1")
	assert.Contains(t, line, "file_refs=report:reports/replay.md")
	assert.Contains(t, line, "gates=1")
	assert.Contains(t, line, "gate_checks=run-1/tests:pass")
}

func captureStdoutForExport(t *testing.T, fn func()) string {
	t.Helper()

	oldStdout := os.Stdout
	reader, writer, err := os.Pipe()
	require.NoError(t, err)

	os.Stdout = writer

	defer func() {
		os.Stdout = oldStdout
	}()

	fn()

	os.Stdout = oldStdout

	require.NoError(t, writer.Close())

	out, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.NoError(t, reader.Close())

	return string(out)
}
