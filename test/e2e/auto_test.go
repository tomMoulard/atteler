package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestE2EAutoModeSelectsOrchestratorAndRespectsDepthCap verifies that --auto
// force-selects the auto-mode orchestrator persona, and that once the recursion
// depth budget is exhausted the run downgrades to a single ordinary agent.
//
// This exercises wiring only (the replayed response never forks); the full
// self-fork loop is covered by TestE2EAutoModeForksChildViaBash.
func TestE2EAutoModeSelectsOrchestratorAndRespectsDepthCap(t *testing.T) {
	t.Parallel()

	workDir := t.TempDir()
	replayPath := writeReplayResponse(t, workDir)

	// --auto force-selects the orchestrator persona (named "auto").
	result := runOK(t, runSpec{dir: workDir},
		"--auto", "--auto-max-depth", "2",
		"--headless", "--autonomy", "high",
		"--replay-response", replayPath,
		"--output", "json",
		"--once", "implement the feature",
	)

	var report struct {
		Agent   string `json:"agent"`
		Content string `json:"content"`
	}
	require.NoError(t, json.Unmarshal([]byte(result.stdout), &report))
	require.Equal(t, "auto", report.Agent)

	// At the depth cap (ATTELER_AUTO_DEPTH >= --auto-max-depth) auto mode is
	// suppressed: the run continues as a single agent and warns on stderr.
	capped := runOK(t, runSpec{dir: workDir, env: []string{"ATTELER_AUTO_DEPTH=2"}},
		"--auto", "--auto-max-depth", "2",
		"--headless", "--autonomy", "high",
		"--replay-response", replayPath,
		"--output", "json",
		"--once", "implement the feature",
	)

	var cappedReport struct {
		Agent string `json:"agent"`
	}
	require.NoError(t, json.Unmarshal([]byte(capped.stdout), &cappedReport))
	require.NotEqual(t, "auto", cappedReport.Agent)
	assertContains(t, capped.stderr, "suppressed at recursion depth")
}

// TestE2EAutoModeRejectsUnknownMode verifies that --auto=<unknown> fails fast.
func TestE2EAutoModeRejectsUnknownMode(t *testing.T) {
	t.Parallel()

	workDir := t.TempDir()
	replayPath := writeReplayResponse(t, workDir)

	result, err := runAtteler(t, runSpec{dir: workDir},
		"--auto=does-not-exist",
		"--headless", "--autonomy", "high",
		"--replay-response", replayPath,
		"--output", "json",
		"--once", "implement the feature",
	)
	require.Error(t, err)
	assertContains(t, result.stderr, "unknown auto mode")
}

// TestE2EAutoConfigDefaultIgnoredInHeadless verifies that the `auto:` config
// default applies to interactive runs only: a headless one-shot must stay an
// ordinary single agent unless --auto is passed explicitly.
func TestE2EAutoConfigDefaultIgnoredInHeadless(t *testing.T) {
	t.Parallel()

	workDir := t.TempDir()
	configPath := filepath.Join(workDir, "atteler.yaml")
	writeFile(t, configPath, "auto: auto\n")
	replayPath := writeReplayResponse(t, workDir)

	result := runOK(t, runSpec{dir: workDir},
		"--config", configPath,
		"--headless", "--autonomy", "high",
		"--replay-response", replayPath,
		"--output", "json",
		"--once", "implement the feature",
	)

	var report struct {
		Agent string `json:"agent"`
	}
	require.NoError(t, json.Unmarshal([]byte(result.stdout), &report))
	require.NotEqual(t, "auto", report.Agent, "config auto default must not activate headless runs")
}

// TestE2EAutoModeForksChildViaBash is the end-to-end eval for the self-fork
// feature. A scripted mock Anthropic endpoint makes the orchestrator issue a
// bash tool call that spawns a real child atteler process (running the explorer
// worker). The child inherits ANTHROPIC_BASE_URL and hits the same mock, so the
// whole loop — orchestrator -> bash -> child atteler -> tool result ->
// orchestrator synthesis — runs deterministically with no live provider.
func TestE2EAutoModeForksChildViaBash(t *testing.T) {
	t.Parallel()

	workDir := t.TempDir()
	homeDir := filepath.Join(workDir, "home")
	configPath := filepath.Join(workDir, "atteler.yaml")

	// Auto mode forks children through the bash tool, whose sandbox redacts
	// credential env vars (e.g. ANTHROPIC_API_KEY). Borrowed file credentials
	// survive because HOME is inherited and read from disk — which is atteler's
	// primary auth model. Use the claude-code provider to exercise that path.
	credPath := filepath.Join(homeDir, ".claude", ".credentials.json")
	writeFile(t, credPath, `{"claudeAiOauth":{"accessToken":"test-access","refreshToken":"test-refresh","expiresAt":9999999999999}}`)

	writeFile(t, configPath, `default_provider: claude-code
default_model: claude-code/claude-opus-4-7
providers:
  anthropic:
    disabled: true
  claude-code:
    credential_policy:
      allowed_stores: [claude_code_file]
      allow_borrowed_oauth: true
  codex:
    disabled: true
  ollama:
    disabled: true
  openai:
    disabled: true
generation:
  temperature: 0
  max_tokens: 64
`)

	// The command the orchestrator is scripted to run: spawn a child atteler as
	// the explorer worker. The child reuses the same config and inherits HOME
	// (credentials) and the mock endpoint via the environment.
	childCommand := fmt.Sprintf(
		"%q --config %q --headless --once explore --agent explorer --output json --autonomy high",
		e2eBinary, configPath,
	)

	server := startScriptedAutoAnthropic(t, childCommand)
	defer server.Close()

	result := runOK(t, runSpec{
		dir: workDir,
		env: []string{
			"HOME=" + homeDir,
			"ANTHROPIC_BASE_URL=" + server.URL,
			"ATTELER_CLAUDE_CODE_SKIP_KEYCHAIN=1",
			"ATTELER_OLLAMA_AUTO_START=false",
		},
	},
		"--config", configPath,
		"--auto", "--auto-max-depth", "2",
		"--headless", "--autonomy", "high",
		"--output", "json",
		"--once", "implement the feature",
	)

	// The orchestrator's final synthesized answer is surfaced to the user.
	var report struct {
		Agent   string `json:"agent"`
		Content string `json:"content"`
	}
	require.NoError(t, json.Unmarshal([]byte(result.stdout), &report))
	require.Equal(t, "auto", report.Agent)
	assertContains(t, report.Content, orchestratorDoneMarker)

	// The orchestrator took two turns (issue tool call, then synthesize) and a
	// real child explorer process was spawned and reached the provider.
	require.GreaterOrEqual(t, server.orchestratorToolTurns.Load(), int32(1), "orchestrator should issue a bash tool call")
	require.GreaterOrEqual(t, server.orchestratorFinalTurns.Load(), int32(1), "orchestrator should synthesize a final answer")
	require.GreaterOrEqual(t, server.childTurns.Load(), int32(1), "a child worker should reach the provider")
	require.True(t, server.sawExplorerWorker.Load(), "the spawned child should run as the explorer worker persona")
}

// TestLiveAutoModeOneShot runs --auto against a real Anthropic model. It is
// gated behind ATTELER_E2E_LIVE and skips when credentials are unavailable. It
// asserts the run completes and produces output; it does not assert that the
// model chose to fork (that is non-deterministic and covered deterministically
// by TestE2EAutoModeForksChildViaBash).
//
//nolint:paralleltest // reads live provider environment and may consume quota.
func TestLiveAutoModeOneShot(t *testing.T) {
	requireLive(t)
	apiKey := requireAnthropic(t)
	model := envOrDefault("ATTELER_E2E_ANTHROPIC_MODEL", liveAnthropicDefaultModel)

	workDir := t.TempDir()
	configPath := filepath.Join(workDir, "atteler.yaml")
	writeFile(t, configPath, `default_provider: anthropic
providers:
  claude-code:
    disabled: true
  codex:
    disabled: true
  openai:
    disabled: true
  ollama:
    disabled: true
generation:
  temperature: 0
  max_tokens: 256
`)

	result := runOK(t, runSpec{
		dir:     workDir,
		timeout: liveTimeout,
		env: []string{
			"ANTHROPIC_API_KEY=" + apiKey,
			"ATTELER_OLLAMA_AUTO_START=false",
		},
	},
		"--config", configPath, "--model", model,
		"--auto", "--auto-max-depth", "1",
		"--headless", "--autonomy", "high",
		"--output", "json",
		"--once", "Briefly summarize what this repository does in one sentence.",
	)

	var report struct {
		Agent   string `json:"agent"`
		Content string `json:"content"`
	}
	require.NoError(t, json.Unmarshal([]byte(result.stdout), &report))
	require.Equal(t, "auto", report.Agent)
	require.NotEmpty(t, strings.TrimSpace(report.Content))
}

const (
	orchestratorDoneMarker = "ORCHESTRATION_COMPLETE"
	childResultMarker      = "CHILD_EXPLORER_RESULT"
)

// scriptedAutoServer is a mock Anthropic /v1/messages endpoint that drives the
// self-fork loop and records what it observed.
type scriptedAutoServer struct {
	*httptest.Server
	orchestratorToolTurns  atomic.Int32
	orchestratorFinalTurns atomic.Int32
	childTurns             atomic.Int32
	sawExplorerWorker      atomic.Bool
}

// startScriptedAutoAnthropic returns a mock provider that:
//   - tells the orchestrator (system prompt contains the self-fork manual) to
//     run childCommand via bash on its first turn;
//   - returns a final synthesized answer to the orchestrator once a tool result
//     is present;
//   - returns a worker answer to any other caller (the spawned child).
func startScriptedAutoAnthropic(t *testing.T, childCommand string) *scriptedAutoServer {
	t.Helper()

	server := &scriptedAutoServer{}
	server.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/messages") {
			http.NotFound(w, r)

			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)

			return
		}

		request := string(body)

		w.Header().Set("Content-Type", "application/json")

		isOrchestrator := strings.Contains(request, "Self-Fork Orchestration")
		hasToolResult := strings.Contains(request, "tool_result")

		switch {
		case isOrchestrator && !hasToolResult:
			server.orchestratorToolTurns.Add(1)
			fmt.Fprintf(w, `{"model":"claude-opus-4-7","stop_reason":"tool_use","content":[{"type":"tool_use","id":"call_1","name":"bash","input":{"command":%q}}],"usage":{"input_tokens":1,"output_tokens":1}}`, childCommand)
		case isOrchestrator && hasToolResult:
			server.orchestratorFinalTurns.Add(1)
			fmt.Fprintf(w, `{"model":"claude-opus-4-7","stop_reason":"end_turn","content":[{"type":"text","text":%q}],"usage":{"input_tokens":1,"output_tokens":1}}`, orchestratorDoneMarker+": spawned explorer and synthesized its findings")
		default:
			server.childTurns.Add(1)

			if strings.Contains(request, "exploration worker") {
				server.sawExplorerWorker.Store(true)
			}

			fmt.Fprintf(w, `{"model":"claude-opus-4-7","stop_reason":"end_turn","content":[{"type":"text","text":%q}],"usage":{"input_tokens":1,"output_tokens":1}}`, childResultMarker+": mapped the relevant code")
		}
	}))

	return server
}
