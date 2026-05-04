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

func TestCodexProvider_Complete(t *testing.T) {
	if runtime.GOOS == osWindows {
		t.Skip("shell-script codex fake is POSIX-only")
	}

	dir := t.TempDir()
	workDir := t.TempDir()
	argsFile := filepath.Join(dir, "args.txt")

	t.Chdir(workDir)
	t.Setenv("ATTELER_ARGS_FILE", argsFile)

	bin := filepath.Join(dir, "codex")

	script := `#!/bin/sh
for arg in "$@"; do
  printf '%s\n' "$arg" >> "$ATTELER_ARGS_FILE"
done
out=""
model=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o) out="$2"; shift 2 ;;
    -m) model="$2"; shift 2 ;;
    *) shift ;;
  esac
done
printf 'codex:%s' "$model" > "$out"
`
	if err := os.WriteFile(bin, []byte(script), 0o600); err != nil {
		require.NoError(t, err)
	}
	//nolint:gosec // Test fixture must be executable so exec.Command can run it.
	if err := os.Chmod(bin, 0o700); err != nil {
		require.NoError(t, err)
	}

	provider := &CodexProvider{
		bin:    bin,
		models: []string{"gpt-5.5"},
	}

	resp, err := provider.Complete(context.Background(), CompleteParams{
		Model:          "gpt-5.5",
		ReasoningLevel: "high",
		Messages: []Message{
			{Role: RoleSystem, Content: "be brief"},
			{Role: RoleUser, Content: "say ok"},
		},
	})
	if err != nil {
		require.NoError(t, err)
	}

	if resp.Content != "codex:gpt-5.5" {
		assert.Failf(t, "assertion failed", "Content = %q, want codex:gpt-5.5", resp.Content)
	}

	if resp.Model != "gpt-5.5" {
		assert.Failf(t, "assertion failed", "Model = %q, want gpt-5.5", resp.Model)
	}

	args := readArgv(t, argsFile)
	if value, ok := flagValue(args, "--sandbox"); !ok || value != "workspace-write" {
		require.Failf(t, "unexpected failure", "--sandbox = %q, %v; want workspace-write", value, ok)
	}

	if value, ok := flagValue(args, "--cd"); !ok || value != workDir {
		require.Failf(t, "unexpected failure", "--cd = %q, %v; want %q", value, ok, workDir)
	}

	if value, ok := flagValue(args, "-c"); !ok || value != `model_reasoning_effort="high"` {
		require.Failf(t, "unexpected failure", "-c = %q, %v; want model_reasoning_effort", value, ok)
	}
}

func TestCodexConfiguredModel(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	codexDir := filepath.Join(dir, ".codex")
	if err := os.MkdirAll(codexDir, 0o750); err != nil {
		require.NoError(t, err)
	}

	if err := os.WriteFile(filepath.Join(codexDir, "config.toml"), []byte(`
# comment
model = "gpt-test-codex"
`), 0o600); err != nil {
		require.NoError(t, err)
	}

	if got := codexConfiguredModel(); got != "gpt-test-codex" {
		require.Failf(t, "unexpected failure", "codexConfiguredModel = %q, want gpt-test-codex", got)
	}

	models := codexModels()
	if len(models) == 0 || models[0] != "gpt-test-codex" {
		require.Failf(t, "unexpected failure", "codexModels = %v, want configured model first", models)
	}
}

func TestCodexPrompt(t *testing.T) {
	t.Parallel()

	got := codexPrompt([]Message{
		{Role: RoleSystem, Content: "system rules"},
		{Role: RoleUser, Content: "hello"},
		{Role: RoleAssistant, Content: "hi"},
	})
	for _, want := range []string{"<system>", "system rules", "USER:\nhello", "ASSISTANT:\nhi"} {
		if !strings.Contains(got, want) {
			require.Failf(t, "unexpected failure", "prompt missing %q:\n%s", want, got)
		}
	}
}
