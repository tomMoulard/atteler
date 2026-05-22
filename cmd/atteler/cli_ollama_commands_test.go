package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	appconfig "github.com/tommoulard/atteler/pkg/config"
	"github.com/tommoulard/atteler/pkg/llm"
)

func TestPrintOllamaStatusReportsRemoteWithoutStarting(t *testing.T) {
	t.Setenv("OLLAMA_BASE_URL", "https://ollama.example")
	t.Setenv("ATTELER_OLLAMA_AUTO_START", "true")
	t.Setenv("ATTELER_OLLAMA_OWNERSHIP_PATH", filepath.Join(t.TempDir(), "ollama-daemon.json"))

	out := captureStdoutForOllamaCommands(t, func() {
		require.NoError(t, printOllamaStatus(context.Background()))
	})

	assert.Contains(t, out, "Ollama status")
	assert.Contains(t, out, "state: remote")
	assert.Contains(t, out, "base_url: https://ollama.example")
	assert.Contains(t, out, "local: false")
	assert.Contains(t, out, "auto_start: enabled (env.ATTELER_OLLAMA_AUTO_START)")
	assert.Contains(t, out, "ownership: none")
	assert.Contains(t, out, "stop: no Atteler-owned daemon recorded")
}

func TestStopOllamaDaemonReportsNoRecord(t *testing.T) {
	ownershipPath := filepath.Join(t.TempDir(), "ollama-daemon.json")
	t.Setenv("ATTELER_OLLAMA_OWNERSHIP_PATH", ownershipPath)

	out := captureStdoutForOllamaCommands(t, func() {
		require.NoError(t, stopOllamaDaemon(context.Background()))
	})

	assert.Contains(t, out, "Ollama stop")
	assert.Contains(t, out, "ownership_path: "+ownershipPath)
	assert.Contains(t, out, "result: no Atteler-owned Ollama daemon record found")
	assert.Contains(t, out, "stopped: false")
	assert.Contains(t, out, "cleaned: false")
}

func TestFormatOllamaDoctorLineReportsRemotePolicyAndOwnership(t *testing.T) {
	t.Setenv("OLLAMA_BASE_URL", "")
	t.Setenv("ATTELER_OLLAMA_OWNERSHIP_PATH", filepath.Join(t.TempDir(), "ollama-daemon.json"))
	unsetEnvForOllamaCommandTest(t, "ATTELER_OLLAMA_AUTO_START")

	line := formatOllamaDoctorLine(context.Background(), appconfig.Config{
		Providers: map[string]appconfig.ProviderConfig{
			ollamaProviderName: {
				BaseURL:   "https://ollama.example",
				AutoStart: true,
			},
		},
	})

	assert.Contains(t, line, "ollama: remote")
	assert.Contains(t, line, "base_url=https://ollama.example")
	assert.Contains(t, line, "auto_start=enabled")
	assert.Contains(t, line, "ownership=none")
	assert.NotContains(t, line, "error=")
}

func TestFormatOllamaDoctorLineReportsInvalidAutoStartPolicy(t *testing.T) {
	t.Setenv("OLLAMA_BASE_URL", "https://ollama.example")
	t.Setenv("ATTELER_OLLAMA_AUTO_START", "maybe")
	t.Setenv("ATTELER_OLLAMA_OWNERSHIP_PATH", filepath.Join(t.TempDir(), "ollama-daemon.json"))

	line := formatOllamaDoctorLine(context.Background(), appconfig.Config{})

	assert.Contains(t, line, "ollama: remote")
	assert.Contains(t, line, "auto_start=disabled")
	assert.Contains(t, line, "auto_start_error=ATTELER_OLLAMA_AUTO_START must be one of")
}

func TestOllamaStopHintSuggestsStopForOwnedUnhealthyDaemon(t *testing.T) {
	t.Parallel()

	hint := ollamaStopHint(llm.OllamaStatus{
		State:           llm.OllamaStatusUnavailable,
		OwnershipStatus: "owned-running",
		Ownership: &llm.OllamaDaemonOwnership{
			PID:     1234,
			BaseURL: "http://127.0.0.1:11434",
		},
	})

	assert.Contains(t, hint, "atteler providers ollama-stop")
	assert.Contains(t, hint, "atteler --ollama-stop")
}

func TestPrintOllamaStatusReportIncludesOwnershipMetadata(t *testing.T) { //nolint:paralleltest // Captures process-global stdout.
	startedAt := time.Date(2026, 5, 22, 9, 30, 0, 0, time.UTC)
	out := captureStdoutForOllamaCommands(t, func() {
		printOllamaStatusReport(llm.OllamaStatus{
			State:           llm.OllamaStatusStartedByAtteler,
			BaseURL:         "http://127.0.0.1:11434",
			Local:           true,
			OwnershipPath:   "/tmp/ollama-daemon.json",
			OwnershipStatus: "owned-running",
			Ownership: &llm.OllamaDaemonOwnership{
				Owner:           "atteler",
				PID:             4242,
				Command:         []string{"ollama", "serve"},
				StartedAt:       startedAt,
				BaseURL:         "http://127.0.0.1:11434",
				SessionID:       "session-123",
				AttelerCommand:  []string{"atteler", "--once", "hi"},
				AutoStartSource: "config.providers.ollama.auto_start",
				Environment:     map[string]string{"OLLAMA_HOST": "127.0.0.1:11434"},
				LogPath:         "/tmp/ollama-startup.log",
			},
		})
	})

	assert.Contains(t, out, "state: started-by-atteler")
	assert.Contains(t, out, "ownership: owned-running")
	assert.Contains(t, out, "owner: atteler")
	assert.Contains(t, out, "pid: 4242")
	assert.Contains(t, out, "daemon_command: ollama serve")
	assert.Contains(t, out, "started_at: 2026-05-22T09:30:00Z")
	assert.Contains(t, out, "session_id: session-123")
	assert.Contains(t, out, "atteler_command: atteler --once hi")
	assert.Contains(t, out, "auto_start_source: config.providers.ollama.auto_start")
	assert.Contains(t, out, "daemon_environment: OLLAMA_HOST=127.0.0.1:11434")
	assert.Contains(t, out, "startup_log: /tmp/ollama-startup.log")
}

func captureStdoutForOllamaCommands(t *testing.T, fn func()) string {
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

//nolint:usetesting // This helper intentionally restores an unset-vs-empty distinction.
func unsetEnvForOllamaCommandTest(t *testing.T, key string) {
	t.Helper()

	previous, ok := os.LookupEnv(key)
	require.NoError(t, os.Unsetenv(key))

	t.Cleanup(func() {
		if ok {
			assert.NoError(t, os.Setenv(key, previous))
			return
		}

		assert.NoError(t, os.Unsetenv(key))
	})
}
