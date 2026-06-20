package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/autonomy"
	"github.com/tommoulard/atteler/pkg/events"
	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/permission"
)

type recordingFileToolObserver struct {
	events []events.Event
	mu     sync.Mutex
}

func (o *recordingFileToolObserver) ObserveEvent(_ context.Context, event events.Event) error {
	o.mu.Lock()
	defer o.mu.Unlock()

	o.events = append(o.events, event)

	return nil
}

func (o *recordingFileToolObserver) snapshot() []events.Event {
	o.mu.Lock()
	defer o.mu.Unlock()

	return append([]events.Event(nil), o.events...)
}

func TestExecuteFileToolReadEmitsFileRead(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "note.txt"), []byte("alpha\nbeta\ngamma\n"), 0o600))

	observer := &recordingFileToolObserver{}
	ctx := events.WithEmitter(
		context.Background(),
		events.NewRunnerWithLoggerAndObservers(nil, nil, observer),
		events.Event{SessionID: "session-1", Agent: "coder", Model: "model"},
	)

	result := executeFileTool(ctx, llm.ToolCall{
		ID:   "read-1",
		Name: llm.ToolNameRead,
		Input: map[string]any{
			"path":   "note.txt",
			"offset": float64(2),
			"limit":  float64(1),
		},
	}, fileToolExecutorOptions{WorkingDir: root, Autonomy: autonomy.Medium})

	require.False(t, result.IsError, result.Content)
	assert.Equal(t, "read-1", result.ToolCallID)
	assert.Contains(t, result.Content, "note.txt")
	assert.Contains(t, result.Content, "2 | beta")

	recorded := observer.snapshot()
	require.Len(t, recorded, 1)
	assert.Equal(t, events.FileRead, recorded[0].Type)
	assert.Equal(t, "session-1", recorded[0].SessionID)
	assert.Equal(t, "coder", recorded[0].Agent)
	assert.Equal(t, "note.txt", recorded[0].Metadata["path"])
	assert.Equal(t, "read", recorded[0].Metadata["tool"])
	assert.Equal(t, "read-1", recorded[0].Metadata["tool_call_id"])
	assert.Equal(t, "17", recorded[0].Metadata["bytes"])
	assert.True(t, strings.HasPrefix(recorded[0].Metadata["digest_sha256"], "sha256:"))
}

func TestExecuteFileToolWriteEditGlobAndGrep(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, "pkg"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, "pkg", "main.go"), []byte("package pkg\n\nfunc Hello() string { return \"hello\" }\n"), 0o600))

	observer := &recordingFileToolObserver{}
	ctx := events.WithEmitter(
		context.Background(),
		events.NewRunnerWithLoggerAndObservers(nil, nil, observer),
		events.Event{SessionID: "session-2", Agent: "coder", Model: "model"},
	)
	opts := fileToolExecutorOptions{WorkingDir: root, Autonomy: autonomy.Medium}

	writeResult := executeFileTool(ctx, llm.ToolCall{
		ID:   "write-1",
		Name: llm.ToolNameWrite,
		Input: map[string]any{
			"path":    "pkg/extra.go",
			"content": "package pkg\n\nconst Extra = true\n",
		},
	}, opts)
	require.False(t, writeResult.IsError, writeResult.Content)

	editResult := executeFileTool(ctx, llm.ToolCall{
		ID:   "edit-1",
		Name: llm.ToolNameEdit,
		Input: map[string]any{
			"path":       "pkg/main.go",
			"old_string": "hello",
			"new_string": "hi",
		},
	}, opts)
	require.False(t, editResult.IsError, editResult.Content)

	data, err := os.ReadFile(filepath.Join(root, "pkg", "main.go"))
	require.NoError(t, err)
	assert.Contains(t, string(data), `"hi"`)

	globResult := executeFileTool(ctx, llm.ToolCall{
		ID:   "glob-1",
		Name: llm.ToolNameGlob,
		Input: map[string]any{
			"pattern": "**/*.go",
		},
	}, opts)
	require.False(t, globResult.IsError, globResult.Content)
	assert.Contains(t, globResult.Content, "pkg/extra.go")
	assert.Contains(t, globResult.Content, "pkg/main.go")

	nestedGlobResult := executeFileTool(ctx, llm.ToolCall{
		ID:   "glob-2",
		Name: llm.ToolNameGlob,
		Input: map[string]any{
			"path":    "pkg",
			"pattern": "*.go",
		},
	}, opts)
	require.False(t, nestedGlobResult.IsError, nestedGlobResult.Content)
	assert.Contains(t, nestedGlobResult.Content, "pkg/extra.go")
	assert.NotContains(t, nestedGlobResult.Content, "\nextra.go")

	grepResult := executeFileTool(ctx, llm.ToolCall{
		ID:   "grep-1",
		Name: llm.ToolNameGrep,
		Input: map[string]any{
			"pattern": "Extra",
			"include": "**/*.go",
		},
	}, opts)
	require.False(t, grepResult.IsError, grepResult.Content)
	assert.Contains(t, grepResult.Content, "pkg/extra.go:3:const Extra = true")

	nestedGrepResult := executeFileTool(ctx, llm.ToolCall{
		ID:   "grep-2",
		Name: llm.ToolNameGrep,
		Input: map[string]any{
			"path":    "pkg",
			"pattern": "Extra",
			"include": "*.go",
		},
	}, opts)
	require.False(t, nestedGrepResult.IsError, nestedGrepResult.Content)
	assert.Contains(t, nestedGrepResult.Content, "pkg/extra.go:3:const Extra = true")
	assert.NotContains(t, nestedGrepResult.Content, "\nextra.go:3")

	recorded := observer.snapshot()
	eventTypes := make([]string, 0, len(recorded))

	for _, event := range recorded {
		eventTypes = append(eventTypes, event.Type)
		if event.Type == events.FileWrite && event.Metadata["tool"] == "edit" {
			assert.True(t, strings.HasPrefix(event.Metadata["digest_sha256"], "sha256:"))
			assert.True(t, strings.HasPrefix(event.Metadata["old_digest_sha256"], "sha256:"))
			assert.Equal(t, "edit-1", event.Metadata["tool_call_id"])
		}
	}

	assert.Contains(t, eventTypes, events.FileRead)
	assert.Contains(t, eventTypes, events.FileWrite)
}

func TestExecuteFileToolPreservesExistingFileMode(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("POSIX file mode preservation is not meaningful on Windows")
	}

	root := t.TempDir()
	path := filepath.Join(root, "script.sh")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\necho old\n"), 0o600))
	require.NoError(t, os.Chmod(path, 0o755)) // #nosec G302 -- test verifies executable mode preservation.

	opts := fileToolExecutorOptions{WorkingDir: root, Autonomy: autonomy.Medium}

	writeResult := executeFileTool(context.Background(), llm.ToolCall{
		ID:   "write-mode",
		Name: llm.ToolNameWrite,
		Input: map[string]any{
			"path":    "script.sh",
			"content": "#!/bin/sh\necho new\n",
		},
	}, opts)
	require.False(t, writeResult.IsError, writeResult.Content)

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, 0o755, int(info.Mode().Perm()))

	editResult := executeFileTool(context.Background(), llm.ToolCall{
		ID:   "edit-mode",
		Name: llm.ToolNameEdit,
		Input: map[string]any{
			"path":       "script.sh",
			"old_string": "new",
			"new_string": "edited",
		},
	}, opts)
	require.False(t, editResult.IsError, editResult.Content)

	info, err = os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, 0o755, int(info.Mode().Perm()))
}

func TestExecuteFileToolRejectsInvalidGlobPatterns(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	globResult := executeFileTool(context.Background(), llm.ToolCall{
		ID:   "glob-invalid",
		Name: llm.ToolNameGlob,
		Input: map[string]any{
			"pattern": "[",
		},
	}, fileToolExecutorOptions{WorkingDir: root, Autonomy: autonomy.Medium})
	require.True(t, globResult.IsError)
	assert.Contains(t, globResult.Content, "glob pattern")
	assert.Contains(t, globResult.Content, "invalid")

	grepResult := executeFileTool(context.Background(), llm.ToolCall{
		ID:   "grep-invalid",
		Name: llm.ToolNameGrep,
		Input: map[string]any{
			"pattern": "needle",
			"include": "[",
		},
	}, fileToolExecutorOptions{WorkingDir: root, Autonomy: autonomy.Medium})
	require.True(t, grepResult.IsError)
	assert.Contains(t, grepResult.Content, "include")
	assert.Contains(t, grepResult.Content, "invalid")
}

func TestExecuteFileToolRejectsInvalidOptionalStringInputs(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	globResult := executeFileTool(context.Background(), llm.ToolCall{
		ID:   "glob-invalid-path",
		Name: llm.ToolNameGlob,
		Input: map[string]any{
			"pattern": "*.go",
			"path":    float64(42),
		},
	}, fileToolExecutorOptions{WorkingDir: root, Autonomy: autonomy.Medium})
	require.True(t, globResult.IsError)
	assert.Contains(t, globResult.Content, "path must be a string")

	grepResult := executeFileTool(context.Background(), llm.ToolCall{
		ID:   "grep-invalid-include",
		Name: llm.ToolNameGrep,
		Input: map[string]any{
			"pattern": "needle",
			"include": true,
		},
	}, fileToolExecutorOptions{WorkingDir: root, Autonomy: autonomy.Medium})
	require.True(t, grepResult.IsError)
	assert.Contains(t, grepResult.Content, "include must be a string")
}

func TestExecuteFileToolHonorsCanceledContext(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("package main\n"), 0o600))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result := executeFileTool(ctx, llm.ToolCall{
		ID:   "glob-canceled",
		Name: llm.ToolNameGlob,
		Input: map[string]any{
			"pattern": "*.go",
		},
	}, fileToolExecutorOptions{WorkingDir: root, Autonomy: autonomy.Medium})

	require.True(t, result.IsError)
	assert.Contains(t, result.Content, context.Canceled.Error())
}

func TestExecuteFileToolRejectsExcessiveMaxResults(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	excessive := llm.FileToolMaxResults + 1

	globResult := executeFileTool(context.Background(), llm.ToolCall{
		ID:   "glob-excessive",
		Name: llm.ToolNameGlob,
		Input: map[string]any{
			"pattern":     "*.go",
			"max_results": excessive,
		},
	}, fileToolExecutorOptions{WorkingDir: root, Autonomy: autonomy.Medium})
	require.True(t, globResult.IsError)
	assert.Contains(t, globResult.Content, "max_results")
	assert.Contains(t, globResult.Content, "less than or equal")

	grepResult := executeFileTool(context.Background(), llm.ToolCall{
		ID:   "grep-excessive",
		Name: llm.ToolNameGrep,
		Input: map[string]any{
			"pattern":     "needle",
			"max_results": excessive,
		},
	}, fileToolExecutorOptions{WorkingDir: root, Autonomy: autonomy.Medium})
	require.True(t, grepResult.IsError)
	assert.Contains(t, grepResult.Content, "max_results")
	assert.Contains(t, grepResult.Content, "less than or equal")
}

func TestExecuteFileToolReportsTruncationOnlyWhenResultsExceedLimit(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.txt"), []byte("needle\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "b.txt"), []byte("plain\n"), 0o600))

	opts := fileToolExecutorOptions{WorkingDir: root, Autonomy: autonomy.Medium}

	globExact := executeFileTool(context.Background(), llm.ToolCall{
		ID:   "glob-exact",
		Name: llm.ToolNameGlob,
		Input: map[string]any{
			"pattern":     "*.txt",
			"max_results": float64(2),
		},
	}, opts)
	require.False(t, globExact.IsError, globExact.Content)
	assert.NotContains(t, globExact.Content, "truncated")

	require.NoError(t, os.WriteFile(filepath.Join(root, "c.txt"), []byte("needle\n"), 0o600))

	globTruncated := executeFileTool(context.Background(), llm.ToolCall{
		ID:   "glob-truncated",
		Name: llm.ToolNameGlob,
		Input: map[string]any{
			"pattern":     "*.txt",
			"max_results": float64(2),
		},
	}, opts)
	require.False(t, globTruncated.IsError, globTruncated.Content)
	assert.Contains(t, globTruncated.Content, "truncated at 2")

	grepExact := executeFileTool(context.Background(), llm.ToolCall{
		ID:   "grep-exact",
		Name: llm.ToolNameGrep,
		Input: map[string]any{
			"pattern":     "needle",
			"include":     "*.txt",
			"max_results": float64(2),
		},
	}, opts)
	require.False(t, grepExact.IsError, grepExact.Content)
	assert.NotContains(t, grepExact.Content, "truncated")

	require.NoError(t, os.WriteFile(filepath.Join(root, "d.txt"), []byte("needle\n"), 0o600))

	grepTruncated := executeFileTool(context.Background(), llm.ToolCall{
		ID:   "grep-truncated",
		Name: llm.ToolNameGrep,
		Input: map[string]any{
			"pattern":     "needle",
			"include":     "*.txt",
			"max_results": float64(2),
		},
	}, opts)
	require.False(t, grepTruncated.IsError, grepTruncated.Content)
	assert.Contains(t, grepTruncated.Content, "truncated at 2")
}

func TestExecuteFileToolGrepHandlesOverlongTextLines(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "long.txt"), []byte("needle "+strings.Repeat("x", 1024*1024+10)+"\n"), 0o600))

	result := executeFileTool(context.Background(), llm.ToolCall{
		ID:   "grep-long",
		Name: llm.ToolNameGrep,
		Input: map[string]any{
			"pattern": "needle",
			"include": "*.txt",
		},
	}, fileToolExecutorOptions{WorkingDir: root, Autonomy: autonomy.Medium})

	require.False(t, result.IsError, result.Content)
	assert.Contains(t, result.Content, "long.txt:1:")
	assert.Contains(t, result.Content, "needle")
}

func TestExecuteFileToolWriteDeniedByPermissionPolicy(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	readOnly := permission.ReadOnlyPolicy()
	ctx := permission.ContextWithPolicy(context.Background(), &readOnly)

	result := executeFileTool(ctx, llm.ToolCall{
		ID:   "write-denied",
		Name: llm.ToolNameWrite,
		Input: map[string]any{
			"path":    "blocked.txt",
			"content": "nope",
		},
	}, fileToolExecutorOptions{WorkingDir: root, Autonomy: autonomy.Medium})

	require.True(t, result.IsError)
	assert.Contains(t, result.Content, "denied by permission policy")
	assert.NoFileExists(t, filepath.Join(root, "blocked.txt"))
}

func TestExecuteFileToolWriteRejectsBinaryContent(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	result := executeFileTool(context.Background(), llm.ToolCall{
		ID:   "write-binary",
		Name: llm.ToolNameWrite,
		Input: map[string]any{
			"path":    "binary.txt",
			"content": "alpha\x00beta",
		},
	}, fileToolExecutorOptions{WorkingDir: root, Autonomy: autonomy.Medium})

	require.True(t, result.IsError)
	assert.Contains(t, result.Content, "appears to be binary")
	assert.NoFileExists(t, filepath.Join(root, "binary.txt"))
}

func TestExecuteFileToolRejectsPathEscape(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	require.NoError(t, os.WriteFile(outside, []byte("secret"), 0o600))

	result := executeFileTool(context.Background(), llm.ToolCall{
		ID:   "read-outside",
		Name: llm.ToolNameRead,
		Input: map[string]any{
			"path": outside,
		},
	}, fileToolExecutorOptions{WorkingDir: root, Autonomy: autonomy.Medium})

	require.True(t, result.IsError)
	assert.Contains(t, result.Content, "outside workspace root")
}

func TestExecuteFileToolRejectsSymlinkEscapeOnWrite(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	outside := t.TempDir()

	linkPath := filepath.Join(root, "link")
	if err := os.Symlink(outside, linkPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	result := executeFileTool(context.Background(), llm.ToolCall{
		ID:   "write-symlink",
		Name: llm.ToolNameWrite,
		Input: map[string]any{
			"path":    "link/secret.txt",
			"content": "secret",
		},
	}, fileToolExecutorOptions{WorkingDir: root, Autonomy: autonomy.Medium})

	require.True(t, result.IsError)
	assert.Contains(t, result.Content, "outside workspace root")
	assert.NoFileExists(t, filepath.Join(outside, "secret.txt"))
}
