//nolint:wsl_v5,modernize // Tests intentionally group setup/assertions and legacy atomic checks for clarity.
package subagent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	partialOutput   = "partial"
	failingID       = "fail"
	recoveredOutput = "recovered"
	slowID          = "slow"
)

func TestSpawnAll_RunsRequestsConcurrently(t *testing.T) {
	t.Parallel()

	requests := []Request{
		{ID: "a", Agent: "executor", Prompt: "first"},
		{ID: "b", Agent: "reviewer", Prompt: "second"},
		{ID: "c", Agent: "writer", Prompt: "third"},
	}
	started := make(chan string, len(requests))
	release := make(chan struct{})
	done := make(chan error, 1)

	var results []Result

	go func() {
		var err error

		results, err = SpawnAll(context.Background(), requests, func(ctx context.Context, request Request) (string, error) {
			started <- request.ID

			select {
			case <-release:
				return request.ID + "-output", nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		})
		done <- err
	}()

	gotStarted := make([]string, 0, len(requests))
	for range requests {
		select {
		case id := <-started:
			gotStarted = append(gotStarted, id)
		case <-time.After(500 * time.Millisecond):
			t.Fatalf("SpawnAll did not start every request concurrently; started %v", gotStarted)
		}
	}

	close(release)

	select {
	case err := <-done:
		require.NoError(t, err)
		require.Len(t, results, len(requests))
		assert.Equal(t, []string{"a", "b", "c"}, resultIDs(results))

		for i := range results {
			assert.Equal(t, requests[i], results[i].Request)
			assert.Equal(t, requests[i].ID+"-output", results[i].Output)
			assert.Empty(t, results[i].Error)
			assert.False(t, results[i].StartedAt.IsZero())
			assert.Positive(t, results[i].Duration)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("SpawnAll did not finish after requests were released")
	}
}

func TestSpawnAll_PreservesInputOrderDespiteCompletionOrder(t *testing.T) {
	t.Parallel()

	requests := []Request{
		{ID: slowID, Agent: "executor", Prompt: slowID},
		{ID: "fast", Agent: "executor", Prompt: "fast"},
		{ID: "middle", Agent: "executor", Prompt: "middle"},
	}
	results, err := SpawnAll(context.Background(), requests, func(_ context.Context, request Request) (string, error) {
		if request.ID == slowID {
			time.Sleep(25 * time.Millisecond)
		}

		return request.ID + "-done", nil
	})

	require.NoError(t, err)
	require.Len(t, results, len(requests))
	assert.Equal(t, []string{slowID, "fast", "middle"}, resultIDs(results))
	assert.Equal(t, []string{"slow-done", "fast-done", "middle-done"}, resultOutputs(results))
}

func TestSpawnAll_RecordsErrorsAndReturnsWrappedFailure(t *testing.T) {
	t.Parallel()

	requests := []Request{
		{ID: "ok", Agent: "executor", Prompt: "ok"},
		{ID: failingID, Agent: "executor", Prompt: failingID},
	}
	results, err := SpawnAll(context.Background(), requests, func(_ context.Context, request Request) (string, error) {
		if request.ID == failingID {
			return partialOutput, errors.New("boom")
		}

		return "done", nil
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), `subagent: request "fail" failed: boom`)
	require.Len(t, results, len(requests))
	assert.Empty(t, results[0].Error)
	assert.Equal(t, "done", results[0].Output)
	assert.Equal(t, "boom", results[1].Error)
	assert.Equal(t, partialOutput, results[1].Output)
}

func TestSpawnAll_ValidatesInputs(t *testing.T) {
	t.Parallel()

	runner := func(context.Context, Request) (string, error) { return "", nil }
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	//nolint:govet // Test case readability is more useful than padding optimization.
	type validationCase struct {
		name     string
		ctx      context.Context
		requests []Request
		runner   Runner
		want     string
	}

	tests := []validationCase{
		{name: "nil context", ctx: nil, requests: []Request{{ID: "a", Agent: "executor", Prompt: "prompt"}}, runner: runner, want: "context is required"},
		{name: "canceled context", ctx: canceledCtx, requests: []Request{{ID: "a", Agent: "executor", Prompt: "prompt"}}, runner: runner, want: "context canceled"},
		{name: "nil runner", ctx: context.Background(), requests: []Request{{ID: "a", Agent: "executor", Prompt: "prompt"}}, runner: nil, want: "runner is required"},
		{name: "missing id", ctx: context.Background(), requests: []Request{{Agent: "executor", Prompt: "prompt"}}, runner: runner, want: "ID is required"},
		{name: "missing agent", ctx: context.Background(), requests: []Request{{ID: "a", Prompt: "prompt"}}, runner: runner, want: "agent is required"},
		{name: "missing prompt", ctx: context.Background(), requests: []Request{{ID: "a", Agent: "executor"}}, runner: runner, want: "prompt is required"},
		{name: "duplicate id", ctx: context.Background(), requests: []Request{{ID: "a", Agent: "executor", Prompt: "one"}, {ID: "a", Agent: "writer", Prompt: "two"}}, runner: runner, want: `duplicate request ID "a"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			results, err := SpawnAll(tt.ctx, tt.requests, tt.runner)
			require.Error(t, err)
			assert.Nil(t, results)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

func TestAttelerCommand_RequiresActiveContext(t *testing.T) {
	t.Parallel()

	runner := AttelerCommandWithOptions(CommandOptions{Binary: "atteler"})

	_, err := runner(nil, Request{ID: "child", Agent: "architect", Prompt: "draft plan"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "context is required")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = runner(ctx, Request{ID: "child", Agent: "architect", Prompt: "draft plan"})
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
}

func TestAttelerCommand_ConstructsExpectedArguments(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args.txt")
	fake := filepath.Join(dir, "fake-atteler")
	writeFakeCommand(t, fake, `#!/bin/sh
printf '%s\n' "$@" > "$ATTELER_ARGS_FILE"
printf 'ran:%s:%s' "$2" "$4"
`)

	runner := AttelerCommandWithOptions(CommandOptions{
		Binary: fake,
		Env:    map[string]string{"ATTELER_ARGS_FILE": argsFile},
	})

	output, err := runner(context.Background(), Request{ID: "child", Agent: "architect", Prompt: "draft plan"})

	require.NoError(t, err)
	assert.Equal(t, "ran:architect:draft plan", output)

	contents, err := os.ReadFile(argsFile)
	require.NoError(t, err)
	assert.Equal(t, []string{"--agent", "architect", "--once", "draft plan"}, strings.Split(strings.TrimSpace(string(contents)), "\n"))
}

func TestAttelerCommand_PrependsConfiguredArguments(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args.txt")
	fake := filepath.Join(dir, "fake-atteler")
	writeFakeCommand(t, fake, `#!/bin/sh
printf '%s\n' "$@" > "$ATTELER_ARGS_FILE"
`)

	runner := AttelerCommandWithOptions(CommandOptions{
		Args:   []string{"--model", "codex/gpt-5.5", "--session-dir", dir},
		Binary: fake,
		Env:    map[string]string{"ATTELER_ARGS_FILE": argsFile},
	})

	_, err := runner(context.Background(), Request{ID: "child", Agent: "architect", Prompt: "draft plan"})

	require.NoError(t, err)

	contents, err := os.ReadFile(argsFile)
	require.NoError(t, err)
	assert.Equal(t, []string{
		"--model",
		"codex/gpt-5.5",
		"--session-dir",
		dir,
		"--agent",
		"architect",
		"--once",
		"draft plan",
	}, strings.Split(strings.TrimSpace(string(contents)), "\n"))
}

func TestAttelerCommand_PassesChildIdentityEnvironment(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	envFile := filepath.Join(dir, "env.txt")
	fake := filepath.Join(dir, "fake-atteler")
	writeFakeCommand(t, fake, `#!/bin/sh
{
printf 'id=%s\n' "$ATTELER_CHILD_ID"
printf 'agent=%s\n' "$ATTELER_CHILD_AGENT"
printf 'workspace=%s\n' "$ATTELER_CHILD_WORKSPACE_ID"
printf 'scope=%s\n' "$ATTELER_ALLOWED_WRITE_SCOPE"
printf 'model=%s\n' "$ATTELER_CHILD_MODEL"
printf 'provider=%s\n' "$ATTELER_CHILD_PROVIDER"
} > "$ATTELER_ENV_FILE"
`)

	runner := AttelerCommandWithOptions(CommandOptions{
		Binary: fake,
		Env:    map[string]string{"ATTELER_ENV_FILE": envFile},
	})
	_, err := runner(context.Background(), Request{
		ID:                "child",
		Agent:             "architect",
		Prompt:            "draft plan",
		WorkspaceID:       "session/child",
		AllowedWriteScope: dir,
		Model:             "codex/gpt-test",
		Provider:          "codex",
	})

	require.NoError(t, err)

	contents, err := os.ReadFile(envFile)
	require.NoError(t, err)
	assert.Equal(t, []string{
		"id=child",
		"agent=architect",
		"workspace=session/child",
		"scope=" + dir,
		"model=codex/gpt-test",
		"provider=codex",
	}, strings.Split(strings.TrimSpace(string(contents)), "\n"))
}

func TestChildEnv_OverridesParentAndExtraIdentity(t *testing.T) {
	t.Setenv("ATTELER_CHILD_ID", "parent")
	t.Setenv("ATTELER_CHILD_AGENT", "parent-agent")

	env := childEnv(map[string]string{
		"ATTELER_CHILD_AGENT": "extra-agent",
		"CUSTOM":              "value",
	}, Request{ID: "child", Agent: "architect"})

	assert.Equal(t, []string{"child"}, envValuesForKey(env, "ATTELER_CHILD_ID"))
	assert.Equal(t, []string{"architect"}, envValuesForKey(env, "ATTELER_CHILD_AGENT"))
	assert.Equal(t, []string{"value"}, envValuesForKey(env, "CUSTOM"))
}

func TestAttelerCommand_ReturnsOutputAndWrappedCommandError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	fake := filepath.Join(dir, "fake-atteler")
	writeFakeCommand(t, fake, `#!/bin/sh
printf partial
printf 'bad request' >&2
exit 7
`)

	output, err := AttelerCommand(fake)(context.Background(), Request{ID: "child", Agent: "architect", Prompt: "draft plan"})

	require.Error(t, err)
	assert.Equal(t, partialOutput, output)
	assert.Contains(t, err.Error(), "atteler command failed: bad request")
}

func TestAttelerCommand_EnforcesOutputLimitDuringExecution(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	fake := filepath.Join(dir, "fake-atteler")
	writeFakeCommand(t, fake, `#!/bin/sh
printf abcdef
`)

	runner := AttelerCommandDetailedWithOptions(CommandOptions{Binary: fake, MaxOutputBytes: 4})
	out, err := runner(context.Background(), Request{ID: "child", Agent: "architect", Prompt: "draft plan"})

	require.Error(t, err)
	assert.Equal(t, "abcd", out.Stdout)
	assert.True(t, out.BudgetExhausted)
	assert.Contains(t, err.Error(), "output exceeded 4 byte limit")
}

func TestAttelerCommand_OutputLimitCancelsLongRunningCommand(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	fake := filepath.Join(dir, "fake-atteler")
	writeFakeCommand(t, fake, `#!/bin/sh
printf abcdef
while :; do
  :
done
`)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	started := time.Now()
	runner := AttelerCommandDetailedWithOptions(CommandOptions{Binary: fake, MaxOutputBytes: 4})
	out, err := runner(ctx, Request{ID: "child", Agent: "architect", Prompt: "draft plan"})

	require.Error(t, err)
	assert.Equal(t, "abcd", out.Stdout)
	assert.True(t, out.BudgetExhausted)
	assert.Contains(t, err.Error(), "output exceeded 4 byte limit")
	assert.Less(t, time.Since(started), 4*time.Second)
}

func writeFakeCommand(t *testing.T, path, contents string) {
	t.Helper()

	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*")
	require.NoError(t, err)

	tmpPath := tmp.Name()

	t.Cleanup(func() {
		_ = os.Remove(tmpPath)
	})

	_, writeErr := tmp.WriteString(contents)
	closeErr := tmp.Close()

	require.NoError(t, writeErr)
	require.NoError(t, closeErr)

	//nolint:gosec // Test fixtures must be executable by the spawned process.
	require.NoError(t, os.Chmod(tmpPath, 0o755))
	require.NoError(t, os.Rename(tmpPath, path))
}

func resultIDs(results []Result) []string {
	ids := make([]string, len(results))
	for i := range results {
		ids[i] = results[i].Request.ID
	}

	return ids
}

func resultOutputs(results []Result) []string {
	outputs := make([]string, len(results))
	for i := range results {
		outputs[i] = results[i].Output
	}

	return outputs
}

func envValuesForKey(env []string, key string) []string {
	prefix := key + "="
	values := make([]string, 0)
	for _, entry := range env {
		value, ok := strings.CutPrefix(entry, prefix)
		if ok {
			values = append(values, value)
		}
	}

	return values
}

func TestSpawnAllWithOptions_LimitsConcurrencyAndWritesLedger(t *testing.T) {
	t.Parallel()

	requests := []Request{
		{ID: "a", Agent: "executor", Prompt: "first"},
		{ID: "b", Agent: "executor", Prompt: "second"},
		{ID: "c", Agent: "executor", Prompt: "third"},
		{ID: "d", Agent: "executor", Prompt: "fourth"},
	}
	ledgerPath := filepath.Join(t.TempDir(), "spawn-ledger.json")

	var current int32
	var maxSeen int32

	results, err := SpawnAllDetailed(t.Context(), requests, func(_ context.Context, request Request) (RunOutput, error) {
		active := atomic.AddInt32(&current, 1)
		for {
			seen := atomic.LoadInt32(&maxSeen)
			if active <= seen || atomic.CompareAndSwapInt32(&maxSeen, seen, active) {
				break
			}
		}

		time.Sleep(20 * time.Millisecond)
		atomic.AddInt32(&current, -1)

		return RunOutput{Stdout: request.ID + "-out", Stderr: request.ID + "-err"}, nil
	}, Options{MaxConcurrency: 2, LedgerPath: ledgerPath, AllowedWriteScope: t.TempDir(), Model: "codex/gpt-test"})

	require.NoError(t, err)
	require.Len(t, results, len(requests))
	assert.LessOrEqual(t, atomic.LoadInt32(&maxSeen), int32(2))
	for _, result := range results {
		assert.Equal(t, StatusSucceeded, result.Status)
		assert.NotEmpty(t, result.LedgerPath)
		assert.NotEmpty(t, result.TranscriptPath)
		transcript, readErr := os.ReadFile(result.TranscriptPath)
		require.NoError(t, readErr)
		assert.Contains(t, string(transcript), result.Request.ID+"-out")
	}

	var ledger Ledger
	data, err := os.ReadFile(ledgerPath)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &ledger))
	require.Len(t, ledger.Attempts, len(requests))
	require.Len(t, ledger.Results, len(requests))
	assert.Equal(t, "codex/gpt-test", ledger.Requests[0].Model)
	assert.NotEmpty(t, ledger.Requests[0].WorkspaceID)
	assert.NotEmpty(t, ledger.Requests[0].AllowedWriteScope)
	assert.NotEqual(t, ledger.Requests[0].WorkspaceID, ledger.Requests[1].WorkspaceID)
}

func TestSpawnAllWithOptions_RetriesAndResumesLedger(t *testing.T) {
	t.Parallel()

	requests := []Request{
		{ID: "ok", Agent: "executor", Prompt: "ok"},
		{ID: "flaky", Agent: "executor", Prompt: "flaky"},
	}
	ledgerPath := filepath.Join(t.TempDir(), "spawn-ledger.json")

	var mu sync.Mutex
	calls := map[string]int{}
	runner := func(_ context.Context, request Request) (string, error) {
		mu.Lock()
		calls[request.ID]++
		call := calls[request.ID]
		mu.Unlock()

		if request.ID == "flaky" && call == 1 {
			return partialOutput, errors.New("try again")
		}

		return request.ID + "-done", nil
	}

	results, err := SpawnAllWithOptions(t.Context(), requests, runner, Options{
		LedgerPath:  ledgerPath,
		RetryPolicy: RetryPolicy{MaxAttempts: 2},
	})
	require.NoError(t, err)
	require.Len(t, results, 2)
	assert.Equal(t, 2, results[1].Attempts)
	assert.Equal(t, StatusSucceeded, results[1].Status)

	mu.Lock()
	calls = map[string]int{}
	mu.Unlock()

	resumed, err := SpawnAllWithOptions(t.Context(), requests, runner, Options{LedgerPath: ledgerPath, Resume: true})
	require.NoError(t, err)
	require.Len(t, resumed, 2)
	assert.Equal(t, StatusSkipped, resumed[0].Status)
	assert.True(t, resumed[0].Resumed)
	assert.Equal(t, "ok-done", resumed[0].Output)
	assert.Equal(t, StatusSkipped, resumed[1].Status)
	assert.True(t, resumed[1].Resumed)

	mu.Lock()
	assert.Empty(t, calls)
	calls = map[string]int{}
	mu.Unlock()

	changedRequests := []Request{
		{ID: "ok", Agent: "executor", Prompt: "changed"},
		{ID: "flaky", Agent: "executor", Prompt: "flaky"},
	}
	changed, err := SpawnAllWithOptions(t.Context(), changedRequests, runner, Options{LedgerPath: ledgerPath, Resume: true})
	require.NoError(t, err)
	require.Len(t, changed, 2)
	assert.Equal(t, StatusSucceeded, changed[0].Status)
	assert.False(t, changed[0].Resumed)
	assert.Equal(t, StatusSkipped, changed[1].Status)
	assert.True(t, changed[1].Resumed)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 1, calls["ok"])
	assert.Zero(t, calls["flaky"])
}

func TestSpawnAllWithOptions_ResumesAfterFailedRun(t *testing.T) {
	t.Parallel()

	requests := []Request{
		{ID: "ok", Agent: "executor", Prompt: "ok"},
		{ID: failingID, Agent: "executor", Prompt: failingID},
	}
	ledgerPath := filepath.Join(t.TempDir(), "spawn-ledger.json")
	var failing atomic.Bool
	failing.Store(true)
	var mu sync.Mutex
	calls := map[string]int{}
	runner := func(_ context.Context, request Request) (string, error) {
		mu.Lock()
		calls[request.ID]++
		mu.Unlock()

		if request.ID == failingID && failing.Load() {
			return partialOutput, errors.New("boom")
		}

		return request.ID + "-done", nil
	}

	results, err := SpawnAllWithOptions(t.Context(), requests, runner, Options{LedgerPath: ledgerPath})
	require.Error(t, err)
	require.Len(t, results, 2)
	assert.Equal(t, StatusSucceeded, results[0].Status)
	assert.Equal(t, StatusFailed, results[1].Status)

	failing.Store(false)
	mu.Lock()
	calls = map[string]int{}
	mu.Unlock()

	resumed, err := SpawnAllWithOptions(t.Context(), requests, runner, Options{LedgerPath: ledgerPath, Resume: true})
	require.NoError(t, err)
	require.Len(t, resumed, 2)
	assert.Equal(t, StatusSkipped, resumed[0].Status)
	assert.True(t, resumed[0].Resumed)
	assert.Equal(t, StatusSucceeded, resumed[1].Status)
	assert.False(t, resumed[1].Resumed)

	mu.Lock()
	defer mu.Unlock()
	assert.Zero(t, calls["ok"])
	assert.Equal(t, 1, calls[failingID])
}

func TestSpawnAllWithOptions_ResumeRerunsAfterLatestMatchingFailure(t *testing.T) {
	t.Parallel()

	requests := []Request{{ID: "child", Agent: "executor", Prompt: "child"}}
	ledgerPath := filepath.Join(t.TempDir(), "spawn-ledger.json")
	results, err := SpawnAllWithOptions(t.Context(), requests, func(context.Context, Request) (string, error) {
		return "first success", nil
	}, Options{LedgerPath: ledgerPath})
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, StatusSucceeded, results[0].Status)

	results, err = SpawnAllWithOptions(t.Context(), requests, func(context.Context, Request) (string, error) {
		return partialOutput, errors.New("latest failure")
	}, Options{LedgerPath: ledgerPath})
	require.Error(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, StatusFailed, results[0].Status)

	var calls atomic.Int32
	resumed, err := SpawnAllWithOptions(t.Context(), requests, func(context.Context, Request) (string, error) {
		calls.Add(1)
		return recoveredOutput, nil
	}, Options{LedgerPath: ledgerPath, Resume: true})
	require.NoError(t, err)
	require.Len(t, resumed, 1)
	assert.Equal(t, StatusSucceeded, resumed[0].Status)
	assert.False(t, resumed[0].Resumed)
	assert.Equal(t, recoveredOutput, resumed[0].Output)
	assert.Equal(t, int32(1), calls.Load())
}

func TestSpawnAllWithOptions_ResumeRerunsAfterCanceledResultWithoutAttempt(t *testing.T) {
	t.Parallel()

	ledgerPath := filepath.Join(t.TempDir(), "spawn-ledger.json")
	initial := []Request{{ID: "child", Agent: "executor", Prompt: "child"}}
	results, err := SpawnAllWithOptions(t.Context(), initial, func(context.Context, Request) (string, error) {
		return "first success", nil
	}, Options{LedgerPath: ledgerPath})
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, StatusSucceeded, results[0].Status)

	cancelRun := []Request{
		{ID: failingID, Agent: "executor", Prompt: failingID},
		{ID: "child", Agent: "executor", Prompt: "child"},
	}
	results, err = SpawnAllWithOptions(t.Context(), cancelRun, func(context.Context, Request) (string, error) {
		return partialOutput, errors.New("cancel sibling")
	}, Options{LedgerPath: ledgerPath, MaxConcurrency: 1, CancelOnFailure: true})
	require.Error(t, err)
	require.Len(t, results, 2)
	assert.Equal(t, StatusFailed, results[0].Status)
	assert.Equal(t, StatusCanceled, results[1].Status)
	assert.Zero(t, results[1].Attempts)

	var calls atomic.Int32
	resumed, err := SpawnAllWithOptions(t.Context(), initial, func(context.Context, Request) (string, error) {
		calls.Add(1)
		return recoveredOutput, nil
	}, Options{LedgerPath: ledgerPath, Resume: true})
	require.NoError(t, err)
	require.Len(t, resumed, 1)
	assert.Equal(t, StatusSucceeded, resumed[0].Status)
	assert.False(t, resumed[0].Resumed)
	assert.Equal(t, recoveredOutput, resumed[0].Output)
	assert.Equal(t, int32(1), calls.Load())
}

func TestSpawnAllWithOptions_ResumesFromSuccessfulAttemptWhenResultMissing(t *testing.T) {
	t.Parallel()

	requests := []Request{{ID: "ok", Agent: "executor", Prompt: "ok"}}
	ledgerPath := filepath.Join(t.TempDir(), "spawn-ledger.json")

	var calls atomic.Int32
	results, err := SpawnAllWithOptions(t.Context(), requests, func(context.Context, Request) (string, error) {
		calls.Add(1)
		return "ok-done", nil
	}, Options{LedgerPath: ledgerPath})
	require.NoError(t, err)
	require.Len(t, results, 1)

	var ledger Ledger
	data, err := os.ReadFile(ledgerPath)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &ledger))
	require.NotEmpty(t, ledger.Attempts)
	ledger.Results = nil
	data, err = json.MarshalIndent(ledger, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(ledgerPath, append(data, '\n'), 0o600))

	calls.Store(0)
	resumed, err := SpawnAllWithOptions(t.Context(), requests, func(context.Context, Request) (string, error) {
		calls.Add(1)
		return "rerun", nil
	}, Options{LedgerPath: ledgerPath, Resume: true})

	require.NoError(t, err)
	require.Len(t, resumed, 1)
	assert.Equal(t, StatusSkipped, resumed[0].Status)
	assert.True(t, resumed[0].Resumed)
	assert.Equal(t, "ok-done", resumed[0].Output)
	assert.Equal(t, int32(0), calls.Load())

	data, err = os.ReadFile(ledgerPath)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &ledger))
	require.Len(t, ledger.Results, 1)
	assert.Equal(t, StatusSkipped, ledger.Results[0].Status)
	assert.True(t, ledger.Results[0].Resumed)
}

func TestSpawnAllWithOptions_ResumePreservesResultOnlySuccessForChangedRequestID(t *testing.T) {
	t.Parallel()

	ledgerPath := filepath.Join(t.TempDir(), "spawn-ledger.json")
	original := []Request{{ID: "child", Agent: "executor", Prompt: "child"}}
	results, err := SpawnAllWithOptions(t.Context(), original, func(context.Context, Request) (string, error) {
		return "original success", nil
	}, Options{LedgerPath: ledgerPath})
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, StatusSucceeded, results[0].Status)

	var ledger Ledger
	data, err := os.ReadFile(ledgerPath)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &ledger))
	ledger.Attempts = nil
	data, err = json.MarshalIndent(ledger, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(ledgerPath, append(data, '\n'), 0o600))

	changedCanceled := []Request{
		{ID: failingID, Agent: "executor", Prompt: failingID},
		{ID: "child", Agent: "executor", Prompt: "changed"},
	}
	results, err = SpawnAllWithOptions(t.Context(), changedCanceled, func(context.Context, Request) (string, error) {
		return partialOutput, errors.New("cancel sibling")
	}, Options{LedgerPath: ledgerPath, Resume: true, MaxConcurrency: 1, CancelOnFailure: true})
	require.Error(t, err)
	require.Len(t, results, 2)
	assert.Equal(t, StatusCanceled, results[1].Status)
	assert.Zero(t, results[1].Attempts)

	var calls atomic.Int32
	resumed, err := SpawnAllWithOptions(t.Context(), original, func(context.Context, Request) (string, error) {
		calls.Add(1)
		return recoveredOutput, nil
	}, Options{LedgerPath: ledgerPath, Resume: true})
	require.NoError(t, err)
	require.Len(t, resumed, 1)
	assert.Equal(t, StatusSkipped, resumed[0].Status)
	assert.True(t, resumed[0].Resumed)
	assert.Equal(t, "original success", resumed[0].Output)
	assert.Equal(t, int32(0), calls.Load())

	data, err = os.ReadFile(ledgerPath)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &ledger))
	assert.Len(t, ledger.Results, 3)
}

func TestSpawnAllWithOptions_ResumeRequiresExistingLedger(t *testing.T) {
	t.Parallel()

	requests := []Request{{ID: "ok", Agent: "executor", Prompt: "ok"}}
	var calls atomic.Int32
	runner := func(context.Context, Request) (string, error) {
		calls.Add(1)
		return "should not run", nil
	}

	results, err := SpawnAllWithOptions(t.Context(), requests, runner, Options{Resume: true})
	require.Error(t, err)
	assert.Nil(t, results)
	assert.Contains(t, err.Error(), "resume requires ledger path")

	missing := filepath.Join(t.TempDir(), "missing-ledger.json")
	results, err = SpawnAllWithOptions(t.Context(), requests, runner, Options{LedgerPath: missing, Resume: true})
	require.Error(t, err)
	assert.Nil(t, results)
	assert.Contains(t, err.Error(), "resume ledger")
	assert.Equal(t, int32(0), calls.Load())
}

func TestSpawnAllWithOptions_ResumeRecoversRunningAttempt(t *testing.T) {
	t.Parallel()

	request := Request{ID: "child", Agent: "executor", Prompt: "child"}
	started := time.Now().UTC().Add(-time.Minute)
	ledgerPath := filepath.Join(t.TempDir(), "spawn-ledger.json")
	data, err := json.MarshalIndent(Ledger{
		StartedAt: started,
		UpdatedAt: started,
		RunID:     "run-before-crash",
		Requests:  []Request{request},
		Attempts: []Attempt{{
			StartedAt: started,
			Request:   request,
			Attempt:   1,
			Status:    StatusRunning,
			Usage:     Usage{PromptTokens: 1},
		}},
	}, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(ledgerPath, append(data, '\n'), 0o600))

	var calls atomic.Int32
	results, err := SpawnAllWithOptions(t.Context(), []Request{request}, func(context.Context, Request) (string, error) {
		calls.Add(1)
		return recoveredOutput, nil
	}, Options{LedgerPath: ledgerPath, Resume: true})

	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, StatusSucceeded, results[0].Status)
	assert.False(t, results[0].Resumed)
	assert.Equal(t, recoveredOutput, results[0].Output)
	assert.Equal(t, int32(1), calls.Load())

	var ledger Ledger
	data, err = os.ReadFile(ledgerPath)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &ledger))
	require.Len(t, ledger.Attempts, 2)
	assert.Equal(t, StatusCanceled, ledger.Attempts[0].Status)
	assert.Contains(t, ledger.Attempts[0].Error, "recovered running attempt")
	assert.False(t, ledger.Attempts[0].FinishedAt.IsZero())
	assert.Equal(t, StatusSucceeded, ledger.Attempts[1].Status)
}

func TestSpawnAllWithOptions_CancelOnFailureCancelsSibling(t *testing.T) {
	t.Parallel()

	requests := []Request{
		{ID: failingID, Agent: "executor", Prompt: failingID},
		{ID: slowID, Agent: "executor", Prompt: slowID},
	}
	slowStarted := make(chan struct{})
	releaseFailure := make(chan struct{})

	results, err := SpawnAllWithOptions(t.Context(), requests, func(ctx context.Context, request Request) (string, error) {
		switch request.ID {
		case failingID:
			<-slowStarted
			close(releaseFailure)
			return partialOutput, errors.New("boom")
		case slowID:
			close(slowStarted)
			<-releaseFailure
			<-ctx.Done()
			return "", ctx.Err()
		default:
			return "", nil
		}
	}, Options{MaxConcurrency: 2, CancelOnFailure: true})

	require.Error(t, err)
	require.Len(t, results, 2)
	assert.Equal(t, StatusFailed, results[0].Status)
	assert.Equal(t, StatusCanceled, results[1].Status)
}

func TestSpawnAllWithOptions_CancelOnFailureMarksSiblingCanceledWhenRunnerIgnoresContext(t *testing.T) {
	t.Parallel()

	requests := []Request{
		{ID: failingID, Agent: "executor", Prompt: failingID},
		{ID: slowID, Agent: "executor", Prompt: slowID},
	}
	slowStarted := make(chan struct{})
	releaseFailure := make(chan struct{})

	results, err := SpawnAllWithOptions(t.Context(), requests, func(ctx context.Context, request Request) (string, error) {
		switch request.ID {
		case failingID:
			<-slowStarted
			close(releaseFailure)
			return partialOutput, errors.New("boom")
		case slowID:
			close(slowStarted)
			<-releaseFailure
			<-ctx.Done()
			return "late success", nil
		default:
			return "", nil
		}
	}, Options{MaxConcurrency: 2, CancelOnFailure: true})

	require.Error(t, err)
	require.Len(t, results, 2)
	assert.Equal(t, StatusFailed, results[0].Status)
	assert.Equal(t, StatusCanceled, results[1].Status)
	assert.Contains(t, results[1].Error, context.Canceled.Error())
}

func TestSpawnAllWithOptions_TimeoutAndBudgetExhaustion(t *testing.T) {
	t.Parallel()

	t.Run("timeout", func(t *testing.T) {
		t.Parallel()

		results, err := SpawnAllWithOptions(t.Context(), []Request{{ID: slowID, Agent: "executor", Prompt: slowID}}, func(ctx context.Context, _ Request) (string, error) {
			<-ctx.Done()
			return "", ctx.Err()
		}, Options{Timeout: 10 * time.Millisecond})

		require.Error(t, err)
		require.Len(t, results, 1)
		assert.Equal(t, StatusTimedOut, results[0].Status)
	})

	t.Run("timeout even when runner returns success after deadline", func(t *testing.T) {
		t.Parallel()

		results, err := SpawnAllWithOptions(t.Context(), []Request{{ID: slowID, Agent: "executor", Prompt: slowID}}, func(context.Context, Request) (string, error) {
			time.Sleep(20 * time.Millisecond)
			return "late success", nil
		}, Options{Timeout: 5 * time.Millisecond})

		require.Error(t, err)
		require.Len(t, results, 1)
		assert.Equal(t, StatusTimedOut, results[0].Status)
		assert.Contains(t, results[0].Error, context.DeadlineExceeded.Error())
	})

	t.Run("budget", func(t *testing.T) {
		t.Parallel()

		var called atomic.Bool
		results, err := SpawnAllWithOptions(t.Context(), []Request{{ID: "expensive", Agent: "executor", Prompt: "expensive", EstimatedPromptTokens: 10}}, func(context.Context, Request) (string, error) {
			called.Store(true)
			return "", nil
		}, Options{Budget: Budget{MaxPromptTokens: 5}})

		require.Error(t, err)
		require.Len(t, results, 1)
		assert.Equal(t, StatusBudgetExhausted, results[0].Status)
		assert.False(t, called.Load())
	})

	t.Run("output budget", func(t *testing.T) {
		t.Parallel()

		requests := []Request{
			{ID: "chatty", Agent: "executor", Prompt: "chatty"},
			{ID: "queued", Agent: "executor", Prompt: "queued"},
		}

		var calls atomic.Int32
		results, err := SpawnAllWithOptions(t.Context(), requests, func(context.Context, Request) (string, error) {
			calls.Add(1)
			return "too much output", nil
		}, Options{MaxConcurrency: 1, Budget: Budget{MaxOutputBytes: 4}})

		require.Error(t, err)
		require.Len(t, results, 2)
		assert.Equal(t, StatusBudgetExhausted, results[0].Status)
		assert.Equal(t, StatusCanceled, results[1].Status)
		assert.Contains(t, results[0].Error, "output byte budget exceeded")
		assert.Equal(t, int32(1), calls.Load())
	})

	t.Run("output budget serializes accounting", func(t *testing.T) {
		t.Parallel()

		requests := []Request{
			{ID: "first", Agent: "executor", Prompt: "first"},
			{ID: "second", Agent: "executor", Prompt: "second"},
		}

		var current atomic.Int32
		var maxSeen atomic.Int32
		results, err := SpawnAllWithOptions(t.Context(), requests, func(context.Context, Request) (string, error) {
			active := current.Add(1)
			for {
				seen := maxSeen.Load()
				if active <= seen || maxSeen.CompareAndSwap(seen, active) {
					break
				}
			}

			time.Sleep(10 * time.Millisecond)
			current.Add(-1)

			return "ok", nil
		}, Options{MaxConcurrency: 2, Budget: Budget{MaxOutputBytes: 10}})

		require.NoError(t, err)
		require.Len(t, results, 2)
		assert.LessOrEqual(t, maxSeen.Load(), int32(1))
	})

	t.Run("command output cap is budget exhaustion", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		fake := filepath.Join(dir, "fake-atteler")
		runLog := filepath.Join(dir, "children.log")
		writeFakeCommand(t, fake, `#!/bin/sh
printf '%s\n' "$ATTELER_CHILD_ID" >> "$RUN_LOG"
printf abcdef
`)

		requests := []Request{
			{ID: "chatty", Agent: "executor", Prompt: "chatty"},
			{ID: "queued", Agent: "executor", Prompt: "queued"},
		}
		runner := AttelerCommandDetailedWithOptions(CommandOptions{
			Env:            map[string]string{"RUN_LOG": runLog},
			Binary:         fake,
			MaxOutputBytes: 4,
		})

		results, err := SpawnAllDetailed(t.Context(), requests, runner, Options{MaxConcurrency: 1})

		require.Error(t, err)
		require.Len(t, results, 2)
		assert.Equal(t, StatusBudgetExhausted, results[0].Status)
		assert.Equal(t, StatusCanceled, results[1].Status)
		assert.Equal(t, "abcd", results[0].Output)
		assert.Contains(t, results[0].Error, "output exceeded 4 byte limit")

		logContents, readErr := os.ReadFile(runLog)
		require.NoError(t, readErr)
		assert.Equal(t, "chatty\n", string(logContents))
	})

	t.Run("command output cap uses remaining aggregate budget", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		fake := filepath.Join(dir, "fake-atteler")
		runLog := filepath.Join(dir, "children.log")
		writeFakeCommand(t, fake, `#!/bin/sh
printf '%s\n' "$ATTELER_CHILD_ID" >> "$RUN_LOG"
printf abcdef
`)

		requests := []Request{
			{ID: "first", Agent: "executor", Prompt: "first"},
			{ID: "second", Agent: "executor", Prompt: "second"},
		}
		runner := AttelerCommandDetailedWithOptions(CommandOptions{
			Env:    map[string]string{"RUN_LOG": runLog},
			Binary: fake,
		})

		results, err := SpawnAllDetailed(t.Context(), requests, runner, Options{MaxConcurrency: 2, Budget: Budget{MaxOutputBytes: 10}})

		require.Error(t, err)
		require.Len(t, results, 2)
		assert.Equal(t, StatusSucceeded, results[0].Status)
		assert.Equal(t, "abcdef", results[0].Output)
		assert.Equal(t, StatusBudgetExhausted, results[1].Status)
		assert.Equal(t, "abcd", results[1].Output)
		assert.Contains(t, results[1].Error, "output exceeded 4 byte limit")

		logContents, readErr := os.ReadFile(runLog)
		require.NoError(t, readErr)
		assert.Equal(t, "first\nsecond\n", string(logContents))
	})

	t.Run("resume keeps exhausted output budget closed", func(t *testing.T) {
		t.Parallel()

		requests := []Request{{ID: "chatty", Agent: "executor", Prompt: "chatty"}}
		ledgerPath := filepath.Join(t.TempDir(), "spawn-ledger.json")

		var calls atomic.Int32
		results, err := SpawnAllWithOptions(t.Context(), requests, func(context.Context, Request) (string, error) {
			calls.Add(1)
			return "too much output", nil
		}, Options{LedgerPath: ledgerPath, Budget: Budget{MaxOutputBytes: 4}})

		require.Error(t, err)
		require.Len(t, results, 1)
		assert.Equal(t, StatusBudgetExhausted, results[0].Status)
		assert.Equal(t, int32(1), calls.Load())

		calls.Store(0)
		resumed, err := SpawnAllWithOptions(t.Context(), requests, func(context.Context, Request) (string, error) {
			calls.Add(1)
			return "should not run", nil
		}, Options{LedgerPath: ledgerPath, Resume: true, Budget: Budget{MaxOutputBytes: 4}})

		require.Error(t, err)
		require.Len(t, resumed, 1)
		assert.Equal(t, StatusBudgetExhausted, resumed[0].Status)
		assert.Contains(t, resumed[0].Error, "output byte budget exhausted")
		assert.Equal(t, int32(0), calls.Load())
	})

	t.Run("actual token and cost budget", func(t *testing.T) {
		t.Parallel()

		results, err := SpawnAllDetailed(t.Context(), []Request{{ID: "actual", Agent: "executor", Prompt: "actual", EstimatedPromptTokens: 1, EstimatedCostMicros: 1}}, func(context.Context, Request) (RunOutput, error) {
			return RunOutput{Stdout: "ok", PromptTokens: 10, EstimatedCostMicros: 10}, nil
		}, Options{Budget: Budget{MaxPromptTokens: 5, MaxCostMicros: 5}})

		require.Error(t, err)
		require.Len(t, results, 1)
		assert.Equal(t, StatusBudgetExhausted, results[0].Status)
		assert.Contains(t, results[0].Error, "prompt token budget exceeded")
		assert.Contains(t, results[0].Error, "cost budget exceeded")
		assert.Equal(t, 10, results[0].Usage.PromptTokens)
		assert.Equal(t, int64(10), results[0].Usage.EstimatedCostMicros)
	})
}
