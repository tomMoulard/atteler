//nolint:paralleltest // These CLI tests capture process-global stdout/stderr.
package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/events"
	"github.com/tommoulard/atteler/pkg/llm"
)

func TestRunSpeculateExecution_ReturnsErrorForInvalidJudgeGates(t *testing.T) {
	tests := []struct {
		name        string
		judgeOutput string
		wantErr     string
	}{
		{
			name: "missing gates",
			judgeOutput: "WINNER: planner\n" +
				"REASON: omitted required gates",
			wantErr: `missing gate checks: "tests pass", "lint pass"`,
		},
		{
			name: "malformed gate",
			judgeOutput: "WINNER: planner\n" +
				"REASON: malformed gate evidence\n" +
				"GATE tests pass: PASSING not exact\n" +
				"GATE lint pass: PASS clean",
			wantErr: `gate check "tests pass" failed: malformed gate status`,
		},
		{
			name: "malformed gate syntax",
			judgeOutput: "WINNER: planner\n" +
				"REASON: malformed gate syntax\n" +
				"GATE tests pass PASS no colon\n" +
				"GATE lint pass: PASS clean",
			wantErr: `malformed gate check: expected "<name>: PASS|FAIL <notes>"`,
		},
		{
			name: "duplicate gate",
			judgeOutput: "WINNER: planner\n" +
				"REASON: duplicate gate evidence\n" +
				"GATE tests pass: PASS first\n" +
				"GATE tests pass: PASS second\n" +
				"GATE lint pass: PASS clean",
			wantErr: `duplicate gate check "tests pass"`,
		},
		{
			name: "unknown gate",
			judgeOutput: "WINNER: planner\n" +
				"REASON: unknown gate evidence\n" +
				"GATE tests pass: PASS covered\n" +
				"GATE lint pass: PASS clean\n" +
				"GATE deploy pass: PASS shipped",
			wantErr: `unknown gate check "deploy pass"`,
		},
		{
			name: "explicit failure",
			judgeOutput: "WINNER: planner\n" +
				"REASON: explicit failure\n" +
				"GATE tests pass: FAIL tests red\n" +
				"GATE lint pass: PASS clean",
			wantErr: `gate check "tests pass" failed: tests red`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stdout := captureSpeculateRunStdout(t, func() {
				err := runSpeculateExecution(t.Context(), appState{
					registry: speculateRunTestRegistry(tt.judgeOutput),
				}, speculateRunCommandInput{
					Prompt: "ship safely",
					Agents: []string{"planner"},
					Gates:  []string{"tests pass", "lint pass"},
				})

				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
			})

			assert.Contains(t, stdout, "error: "+tt.wantErr)
			assert.Contains(t, stdout, "winner: planner")
		})
	}
}

func TestRunSpeculateExecution_SucceedsWithExplicitPassingGates(t *testing.T) {
	stdout := captureSpeculateRunStdout(t, func() {
		err := runSpeculateExecution(t.Context(), appState{
			registry: speculateRunTestRegistry("WINNER: planner\n" +
				"REASON: all gates explicit\n" +
				"GATE tests pass: PASS covered\n" +
				"GATE lint pass: PASS clean"),
		}, speculateRunCommandInput{
			Prompt: "ship safely",
			Agents: []string{"planner"},
			Gates:  []string{"tests pass", "lint pass"},
		})

		require.NoError(t, err)
	})

	assert.Contains(t, stdout, "winner: planner")
	assert.Contains(t, stdout, "gates:\n  - tests pass: PASS covered\n  - lint pass: PASS clean")
	assert.NotContains(t, stdout, "error:")
}

func TestRegistryCompleterEmitsRouteDecisionForModelRole(t *testing.T) {
	t.Parallel()

	provider := &capturingIdleSuggestionProvider{
		providerName: "openai",
		model:        "gpt-4.1-mini",
		response:     "proposal ok",
	}
	registry := llm.NewRegistry()
	registry.Register(provider)
	require.NoError(t, registry.SetModelRole("planner", llm.ModelRole{
		Preferred: "openai/gpt-4.1-mini",
	}))

	var eventLog bytes.Buffer

	completer := registryCompleter{
		registry:       registry,
		hookRunner:     events.NewRunnerWithLogger(nil, &eventLog),
		maxInputTokens: 10_000,
	}

	got, err := completer.Complete(t.Context(), "planner", "system", "propose this")

	require.NoError(t, err)
	assert.Equal(t, "proposal ok", got)
	require.NotNil(t, provider.params)
	assert.Equal(t, "gpt-4.1-mini", provider.params.Model)

	log := eventLog.String()
	assert.Contains(t, log, "event:route_decision")
	assert.Contains(t, log, "agent=planner")
	assert.Contains(t, log, "model_role=planner")
	assert.Contains(t, log, "phase=estimated")
	assert.Contains(t, log, "phase=actual")
	assert.Contains(t, log, "selected=openai/gpt-4.1-mini")
	assert.Contains(t, log, "fallback_order=openai/gpt-4.1-mini")
	assert.Contains(t, log, "actual_selected=openai/gpt-4.1-mini")
}

func speculateRunTestRegistry(judgeOutput string) *llm.Registry {
	registry := llm.NewRegistry()
	registry.Register(&speculateRunTestProvider{
		responses: map[string]string{
			"planner": "proposal from planner",
			"judge":   judgeOutput,
		},
	})

	return registry
}

type speculateRunTestProvider struct {
	responses map[string]string
}

func (p *speculateRunTestProvider) Name() string {
	return "speculate-test"
}

func (p *speculateRunTestProvider) Models() []string {
	return []string{"planner", "judge"}
}

func (p *speculateRunTestProvider) FetchModels(context.Context) ([]string, error) {
	return p.Models(), nil
}

func (p *speculateRunTestProvider) HealthCheck(context.Context) error {
	return nil
}

func (p *speculateRunTestProvider) Complete(_ context.Context, params llm.CompleteParams) (*llm.Response, error) {
	content := p.responses[params.Model]
	if content == "" {
		content = "response from " + params.Model
	}

	return &llm.Response{Content: content, Model: params.Model}, nil
}

func (p *speculateRunTestProvider) ModelContextWindow(string) int {
	return 0
}

func captureSpeculateRunStdout(t *testing.T, fn func()) string {
	t.Helper()

	oldStdout := os.Stdout
	oldStderr := os.Stderr

	stdoutReader, stdoutWriter, err := os.Pipe()
	require.NoError(t, err)

	stderrReader, stderrWriter, err := os.Pipe()
	require.NoError(t, err)

	os.Stdout = stdoutWriter
	os.Stderr = stderrWriter

	defer func() {
		os.Stdout = oldStdout
		os.Stderr = oldStderr
	}()

	fn()

	require.NoError(t, stdoutWriter.Close())
	require.NoError(t, stderrWriter.Close())

	var stdoutBuffer bytes.Buffer

	_, err = io.Copy(&stdoutBuffer, stdoutReader)
	require.NoError(t, err)

	var stderrBuffer bytes.Buffer

	_, err = io.Copy(&stderrBuffer, stderrReader)
	require.NoError(t, err)

	return stdoutBuffer.String()
}
