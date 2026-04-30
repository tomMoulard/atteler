package llm

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
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
		t.Fatal(err)
	}
	//nolint:gosec // Test fixture must be executable so exec.Command can run it.
	if err := os.Chmod(bin, 0o700); err != nil {
		t.Fatal(err)
	}

	provider := &CodexProvider{
		bin:    bin,
		models: []string{"gpt-5.5"},
	}
	resp, err := provider.Complete(context.Background(), CompleteParams{
		Model: "gpt-5.5",
		Messages: []Message{
			{Role: RoleSystem, Content: "be brief"},
			{Role: RoleUser, Content: "say ok"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "codex:gpt-5.5" {
		t.Errorf("Content = %q, want codex:gpt-5.5", resp.Content)
	}
	if resp.Model != "gpt-5.5" {
		t.Errorf("Model = %q, want gpt-5.5", resp.Model)
	}

	args := readArgv(t, argsFile)
	if value, ok := flagValue(args, "--sandbox"); !ok || value != "workspace-write" {
		t.Fatalf("--sandbox = %q, %v; want workspace-write", value, ok)
	}
	if value, ok := flagValue(args, "--cd"); !ok || value != workDir {
		t.Fatalf("--cd = %q, %v; want %q", value, ok, workDir)
	}
}

func TestCodexConfiguredModel(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	codexDir := filepath.Join(dir, ".codex")
	if err := os.MkdirAll(codexDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexDir, "config.toml"), []byte(`
# comment
model = "gpt-test-codex"
`), 0o600); err != nil {
		t.Fatal(err)
	}

	if got := codexConfiguredModel(); got != "gpt-test-codex" {
		t.Fatalf("codexConfiguredModel = %q, want gpt-test-codex", got)
	}
	models := codexModels()
	if len(models) == 0 || models[0] != "gpt-test-codex" {
		t.Fatalf("codexModels = %v, want configured model first", models)
	}
}

func TestCodexPrompt(t *testing.T) {
	got := codexPrompt([]Message{
		{Role: RoleSystem, Content: "system rules"},
		{Role: RoleUser, Content: "hello"},
		{Role: RoleAssistant, Content: "hi"},
	})
	for _, want := range []string{"<system>", "system rules", "USER:\nhello", "ASSISTANT:\nhi"} {
		if !strings.Contains(got, want) {
			t.Fatalf("prompt missing %q:\n%s", want, got)
		}
	}
}
