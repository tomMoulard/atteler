package llm

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const osWindows = "windows"

func TestClaudeCodeProvider_Complete(t *testing.T) {
	if runtime.GOOS == osWindows {
		t.Skip("shell-script claude fake is POSIX-only")
	}

	dir := t.TempDir()
	workDir := t.TempDir()
	argsFile := filepath.Join(dir, "args.txt")

	t.Chdir(workDir)
	t.Setenv("ATTELER_ARGS_FILE", argsFile)

	bin := filepath.Join(dir, "claude")

	script := `#!/bin/sh
if [ "$1" = "auth" ] && [ "$2" = "status" ]; then
  printf '{"loggedIn":true}'
  exit 0
fi
for arg in "$@"; do
  printf '%s\n' "$arg" >> "$ATTELER_ARGS_FILE"
done
model=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --model) model="$2"; shift 2 ;;
    *) shift ;;
  esac
done
printf 'claude:%s' "$model"
`
	if err := os.WriteFile(bin, []byte(script), 0o600); err != nil {
		require.NoError(t, err)
	}
	//nolint:gosec // Test fixture must be executable so exec.Command can run it.
	if err := os.Chmod(bin, 0o700); err != nil {
		require.NoError(t, err)
	}

	provider := &ClaudeCodeProvider{
		bin:    bin,
		models: []string{"claude-opus-4-6"},
	}

	resp, err := provider.Complete(context.Background(), CompleteParams{
		Model:          "claude-opus-4-6",
		ReasoningLevel: "xhigh",
		Messages: []Message{
			{Role: RoleSystem, Content: "be brief"},
			{Role: RoleUser, Content: "say ok"},
		},
	})
	if err != nil {
		require.NoError(t, err)
	}

	if resp.Content != "claude:claude-opus-4-6" {
		assert.Failf(t, "assertion failed", "Content = %q, want claude:claude-opus-4-6", resp.Content)
	}

	if resp.Model != "claude-opus-4-6" {
		assert.Failf(t, "assertion failed", "Model = %q, want claude-opus-4-6", resp.Model)
	}

	args := readArgv(t, argsFile)
	if value, ok := flagValue(args, "--permission-mode"); !ok || value != "acceptEdits" {
		require.Failf(t, "unexpected failure", "--permission-mode = %q, %v; want acceptEdits", value, ok)
	}

	if value, ok := flagValue(args, "--tools"); !ok || value != claudeCodeTools {
		require.Failf(t, "unexpected failure", "--tools = %q, %v; want %q", value, ok, claudeCodeTools)
	}

	if value, ok := flagValue(args, "--allowed-tools"); !ok || value != claudeCodeTools {
		require.Failf(t, "unexpected failure", "--allowed-tools = %q, %v; want %q", value, ok, claudeCodeTools)
	} else {
		assert.Contains(t, strings.Split(value, ","), "Bash")
	}

	if value, ok := flagValue(args, "--add-dir"); !ok || value != workDir {
		require.Failf(t, "unexpected failure", "--add-dir = %q, %v; want %q", value, ok, workDir)
	}

	if value, ok := flagValue(args, "--effort"); !ok || value != "xhigh" {
		require.Failf(t, "unexpected failure", "--effort = %q, %v; want xhigh", value, ok)
	}
}

func TestVerifyClaudeCodeAuth(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == osWindows {
		t.Skip("shell-script claude fake is POSIX-only")
	}

	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")

	script := `#!/bin/sh
printf '{"loggedIn":false}'
`
	if err := os.WriteFile(bin, []byte(script), 0o600); err != nil {
		require.NoError(t, err)
	}
	//nolint:gosec // Test fixture must be executable so exec.Command can run it.
	if err := os.Chmod(bin, 0o700); err != nil {
		require.NoError(t, err)
	}

	err := verifyClaudeCodeAuth(context.Background(), bin)
	if err == nil || !strings.Contains(err.Error(), "no Claude Code credentials") {
		require.Failf(t, "unexpected failure", "verifyClaudeCodeAuth error = %v, want missing credentials", err)
	}
}

func readArgv(t *testing.T, path string) []string {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		require.NoError(t, err)
	}

	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}

func flagValue(args []string, flag string) (string, bool) {
	for i, arg := range args {
		if arg == flag && i+1 < len(args) {
			return args[i+1], true
		}
	}

	return "", false
}
