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
