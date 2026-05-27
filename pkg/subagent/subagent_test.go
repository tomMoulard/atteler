//nolint:wsl_v5,modernize // Tests intentionally group setup/assertions and legacy atomic checks for clarity.
package subagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	doneOutput         = "done"
	partialOutput      = "partial"
	failingID          = "fail"
	recoveredOutput    = "recovered"
	shouldNotRunOutput = "should not run"
	slowID             = "slow"
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

func TestSpawnAll_UsesDefaultBoundedConcurrency(t *testing.T) {
	t.Parallel()

	requests := make([]Request, defaultMaxConcurrency*3)
	for i := range requests {
		id := fmt.Sprintf("child-%02d", i)
		requests[i] = Request{ID: id, Agent: "executor", Prompt: id}
	}

	var current atomic.Int32
	var maxSeen atomic.Int32
	results, err := SpawnAll(t.Context(), requests, func(context.Context, Request) (string, error) {
		active := current.Add(1)
		for {
			seen := maxSeen.Load()
			if active <= seen || maxSeen.CompareAndSwap(seen, active) {
				break
			}
		}

		time.Sleep(10 * time.Millisecond)
		current.Add(-1)

		return doneOutput, nil
	})

	require.NoError(t, err)
	require.Len(t, results, len(requests))
	assert.LessOrEqual(t, maxSeen.Load(), int32(defaultMaxConcurrency))
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

		return doneOutput, nil
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), `subagent: request "fail" failed: boom`)
	require.Len(t, results, len(requests))
	assert.Empty(t, results[0].Error)
	assert.Equal(t, doneOutput, results[0].Output)
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

func TestOutputByteLimit(t *testing.T) {
	t.Parallel()

	assert.Zero(t, OutputByteLimit(context.Background()))
	assert.Zero(t, OutputByteLimit(WithOutputByteLimit(context.Background(), 0)))
	assert.Equal(t, int64(7), OutputByteLimit(WithOutputByteLimit(context.Background(), 7)))
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

		return RunOutput{Stdout: request.ID + "-out", Stderr: request.ID + "-err", ExitStatus: 7, Artifacts: []string{"artifact.txt"}}, nil
	}, Options{MaxConcurrency: 2, LedgerPath: ledgerPath, AllowedWriteScope: t.TempDir(), Model: "codex/gpt-test"})

	require.NoError(t, err)
	require.Len(t, results, len(requests))
	assert.LessOrEqual(t, atomic.LoadInt32(&maxSeen), int32(2))
	for _, result := range results {
		assert.Equal(t, StatusSucceeded, result.Status)
		assert.Equal(t, 7, result.ExitStatus)
		assert.Equal(t, []string{"artifact.txt"}, result.Artifacts)
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
	assert.Equal(t, 7, ledger.Attempts[0].ExitStatus)
	assert.Equal(t, []string{"artifact.txt"}, ledger.Attempts[0].Artifacts)
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
		WorkspaceID: "parent-run-a",
	})
	require.NoError(t, err)
	require.Len(t, results, 2)
	assert.Equal(t, 2, results[1].Attempts)
	assert.Equal(t, StatusSucceeded, results[1].Status)

	mu.Lock()
	calls = map[string]int{}
	mu.Unlock()

	resumed, err := SpawnAllWithOptions(t.Context(), requests, runner, Options{
		LedgerPath:  ledgerPath,
		Resume:      true,
		WorkspaceID: "parent-run-b",
	})
	require.NoError(t, err)
	require.Len(t, resumed, 2)
	assert.Equal(t, StatusSkipped, resumed[0].Status)
	assert.True(t, resumed[0].Resumed)
	assert.Equal(t, "parent-run-b/ok", resumed[0].Request.WorkspaceID)
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
	changed, err := SpawnAllWithOptions(t.Context(), changedRequests, runner, Options{
		LedgerPath:  ledgerPath,
		Resume:      true,
		WorkspaceID: "parent-run-b",
	})
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

func TestSpawnAllWithOptions_RecordsAdmissionDenialWhenRetryBackoffCanceled(t *testing.T) {
	t.Parallel()

	requests := []Request{{ID: "flaky", Agent: "executor", Prompt: "flaky"}}
	ledgerPath := filepath.Join(t.TempDir(), "spawn-ledger.json")
	parentCtx, stopTimeout := context.WithTimeout(t.Context(), 2*time.Second)
	defer stopTimeout()
	ctx, cancel := context.WithCancelCause(parentCtx)
	defer cancel(nil)
	cancelCause := errors.New("operator stopped spawn retry")
	go cancelAfterFailedAttempt(ctx, func() { cancel(cancelCause) }, ledgerPath)

	var calls atomic.Int32
	results, err := SpawnAllWithOptions(ctx, requests, func(context.Context, Request) (string, error) {
		if calls.Add(1) == 1 {
			return partialOutput, errors.New("try again")
		}

		return shouldNotRunOutput, nil
	}, Options{
		LedgerPath: ledgerPath,
		RetryPolicy: RetryPolicy{
			MaxAttempts: 2,
			Backoff:     time.Hour,
		},
	})

	require.Error(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, StatusCanceled, results[0].Status)
	assert.Equal(t, 1, results[0].Attempts)
	assert.Contains(t, results[0].Error, cancelCause.Error())
	assert.Equal(t, int32(1), calls.Load())

	var ledger Ledger
	data, readErr := os.ReadFile(ledgerPath)
	require.NoError(t, readErr)
	require.NoError(t, json.Unmarshal(data, &ledger))
	require.Len(t, ledger.Admissions, 2)
	assert.True(t, ledger.Admissions[0].Admitted)
	assert.Equal(t, 1, ledger.Admissions[0].Attempt)
	assert.False(t, ledger.Admissions[1].Admitted)
	assert.Equal(t, 2, ledger.Admissions[1].Attempt)
	assert.Contains(t, ledger.Admissions[1].DenyReason, "retry backoff canceled")
	assert.Contains(t, ledger.Admissions[1].DenyReason, cancelCause.Error())
	assert.Equal(t, ledger.Admissions[1].AdmissionID, results[0].AdmissionID)
	require.Len(t, ledger.Attempts, 1)
	assert.Equal(t, StatusFailed, ledger.Attempts[0].Status)
	assert.Equal(t, ledger.Admissions[0].AdmissionID, ledger.Attempts[0].AdmissionID)
	require.Len(t, ledger.Results, 1)
	assert.Equal(t, StatusCanceled, ledger.Results[0].Status)
	assert.Equal(t, ledger.Admissions[1].AdmissionID, ledger.Results[0].AdmissionID)
}

func cancelAfterFailedAttempt(ctx context.Context, cancel context.CancelFunc, ledgerPath string) {
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			data, err := os.ReadFile(ledgerPath)
			if err != nil {
				continue
			}

			var ledger Ledger
			if err := json.Unmarshal(data, &ledger); err != nil {
				continue
			}

			for i := range ledger.Attempts {
				if ledger.Attempts[i].Status == StatusFailed {
					cancel()
					return
				}
			}
		}
	}
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
		return shouldNotRunOutput, nil
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
	admissionID := "admission-before-crash"
	ledgerPath := filepath.Join(t.TempDir(), "spawn-ledger.json")
	data, err := json.MarshalIndent(Ledger{
		StartedAt: started,
		UpdatedAt: started,
		RunID:     "run-before-crash",
		Requests:  []Request{request},
		Admissions: []Admission{{
			RecordedAt:  started,
			AdmissionID: admissionID,
			ChildID:     request.ID,
			ParentRunID: "run-before-crash",
			Admitted:    true,
			Attempt:     1,
		}},
		Attempts: []Attempt{{
			StartedAt:   started,
			Request:     request,
			Attempt:     1,
			Status:      StatusRunning,
			AdmissionID: admissionID,
			Usage:       Usage{PromptTokens: 1},
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
	require.Len(t, ledger.StopReceipts, 1)
	assert.Equal(t, ledger.Attempts[0].StopID, ledger.StopReceipts[0].StopID)
	assert.Equal(t, admissionID, ledger.StopReceipts[0].AdmissionID)
	assert.Equal(t, request.ID, ledger.StopReceipts[0].ChildID)
	assert.Equal(t, StatusCanceled, ledger.StopReceipts[0].Status)
	assert.Contains(t, ledger.StopReceipts[0].Reason, "recovered running attempt")
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
	assert.Contains(t, results[1].Error, `sibling cancellation after request "fail" failed`)
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
	assert.Contains(t, results[1].Error, `sibling cancellation after request "fail" failed`)
}

func TestCanceledBeforeStartResult_PreservesCancelCause(t *testing.T) {
	t.Parallel()

	result := canceledBeforeStartResult(
		Request{ID: "queued", Agent: "executor", Prompt: "queued"},
		nil,
		"admission-queued",
		context.DeadlineExceeded,
	)

	assert.Equal(t, StatusCanceled, result.Status)
	assert.Equal(t, "admission-queued", result.AdmissionID)
	assert.Contains(t, result.Error, context.DeadlineExceeded.Error())
}

func TestSpawnAllWithOptions_TimeoutAndBudgetExhaustion(t *testing.T) {
	t.Parallel()

	t.Run("timeout", func(t *testing.T) {
		t.Parallel()

		ledgerPath := filepath.Join(t.TempDir(), "spawn-ledger.json")
		results, err := SpawnAllWithOptions(t.Context(), []Request{{ID: slowID, Agent: "executor", Prompt: slowID}}, func(ctx context.Context, _ Request) (string, error) {
			<-ctx.Done()
			return "", ctx.Err()
		}, Options{LedgerPath: ledgerPath, Timeout: 10 * time.Millisecond})

		require.Error(t, err)
		require.Len(t, results, 1)
		assert.Contains(t, err.Error(), `subagent: request "slow" timed out`)
		assert.Equal(t, StatusTimedOut, results[0].Status)
		assert.NotEmpty(t, results[0].AdmissionID)
		assert.NotEmpty(t, results[0].StopID)

		var ledger Ledger
		data, readErr := os.ReadFile(ledgerPath)
		require.NoError(t, readErr)
		require.NoError(t, json.Unmarshal(data, &ledger))
		require.Len(t, ledger.StopReceipts, 1)
		assert.Equal(t, results[0].AdmissionID, ledger.StopReceipts[0].AdmissionID)
		assert.Equal(t, results[0].StopID, ledger.StopReceipts[0].StopID)
		assert.Equal(t, slowID, ledger.StopReceipts[0].ChildID)
		assert.Equal(t, StatusTimedOut, ledger.StopReceipts[0].Status)
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

	t.Run("timeout records deadline when runner returns custom error after deadline", func(t *testing.T) {
		t.Parallel()

		ledgerPath := filepath.Join(t.TempDir(), "spawn-ledger.json")
		results, err := SpawnAllWithOptions(t.Context(), []Request{{ID: slowID, Agent: "executor", Prompt: slowID}}, func(context.Context, Request) (string, error) {
			time.Sleep(20 * time.Millisecond)
			return "", errors.New("late cleanup failed")
		}, Options{LedgerPath: ledgerPath, Timeout: 5 * time.Millisecond})

		require.Error(t, err)
		require.Len(t, results, 1)
		assert.Equal(t, StatusTimedOut, results[0].Status)
		assert.Contains(t, results[0].Error, "late cleanup failed")
		assert.Contains(t, results[0].Error, context.DeadlineExceeded.Error())

		var ledger Ledger
		data, readErr := os.ReadFile(ledgerPath)
		require.NoError(t, readErr)
		require.NoError(t, json.Unmarshal(data, &ledger))
		require.Len(t, ledger.Attempts, 1)
		assert.Contains(t, ledger.Attempts[0].Error, "late cleanup failed")
		assert.Contains(t, ledger.Attempts[0].Error, context.DeadlineExceeded.Error())
		require.Len(t, ledger.StopReceipts, 1)
		assert.Contains(t, ledger.StopReceipts[0].Reason, "late cleanup failed")
		assert.Contains(t, ledger.StopReceipts[0].Reason, context.DeadlineExceeded.Error())
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
		assert.Contains(t, err.Error(), `subagent: request "chatty" budget exhausted`)
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

		ledgerPath := filepath.Join(t.TempDir(), "spawn-ledger.json")
		results, err := SpawnAllDetailed(t.Context(), requests, runner, Options{LedgerPath: ledgerPath, MaxConcurrency: 1})

		require.Error(t, err)
		require.Len(t, results, 2)
		assert.Equal(t, StatusBudgetExhausted, results[0].Status)
		assert.Equal(t, StatusCanceled, results[1].Status)
		assert.Equal(t, "abcd", results[0].Output)
		assert.Contains(t, results[0].Error, "output exceeded 4 byte limit")

		logContents, readErr := os.ReadFile(runLog)
		require.NoError(t, readErr)
		assert.Equal(t, "chatty\n", string(logContents))

		var ledger Ledger
		data, readErr := os.ReadFile(ledgerPath)
		require.NoError(t, readErr)
		require.NoError(t, json.Unmarshal(data, &ledger))
		require.Len(t, ledger.Admissions, 2)
		admissions := map[string]Admission{}
		for _, admission := range ledger.Admissions {
			admissions[admission.ChildID] = admission
		}
		assert.True(t, admissions["chatty"].Admitted)
		assert.False(t, admissions["queued"].Admitted)
		assert.Contains(t, admissions["queued"].DenyReason, context.Canceled.Error())
		assert.Equal(t, admissions["queued"].AdmissionID, results[1].AdmissionID)
		require.Len(t, ledger.StopReceipts, 1)
		assert.Equal(t, results[0].AdmissionID, ledger.StopReceipts[0].AdmissionID)
		assert.Equal(t, results[0].StopID, ledger.StopReceipts[0].StopID)
		assert.Equal(t, "chatty", ledger.StopReceipts[0].ChildID)
		assert.Equal(t, StatusBudgetExhausted, ledger.StopReceipts[0].Status)
		assert.Contains(t, ledger.StopReceipts[0].Reason, "output exceeded 4 byte limit")
		require.Len(t, ledger.Attempts, 1)
		assert.Equal(t, "chatty", ledger.Attempts[0].Request.ID)
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
			return shouldNotRunOutput, nil
		}, Options{LedgerPath: ledgerPath, Resume: true, Budget: Budget{MaxOutputBytes: 4}})

		require.Error(t, err)
		require.Len(t, resumed, 1)
		assert.Equal(t, StatusBudgetExhausted, resumed[0].Status)
		assert.Contains(t, resumed[0].Error, "output byte budget exhausted")
		assert.Equal(t, int32(0), calls.Load())
	})

	t.Run("resume keeps exhausted token and cost budget closed", func(t *testing.T) {
		t.Parallel()

		requests := []Request{{ID: "actual", Agent: "executor", Prompt: "actual", EstimatedPromptTokens: 1, EstimatedCostMicros: 1}}
		ledgerPath := filepath.Join(t.TempDir(), "spawn-ledger.json")
		var calls atomic.Int32
		results, err := SpawnAllDetailed(t.Context(), requests, func(context.Context, Request) (RunOutput, error) {
			calls.Add(1)

			return RunOutput{Stdout: "ok", PromptTokens: 10, EstimatedCostMicros: 10}, nil
		}, Options{LedgerPath: ledgerPath, Budget: Budget{MaxPromptTokens: 5, MaxCostMicros: 5}})

		require.Error(t, err)
		require.Len(t, results, 1)
		assert.Equal(t, StatusBudgetExhausted, results[0].Status)
		assert.Equal(t, int32(1), calls.Load())

		calls.Store(0)
		resumed, err := SpawnAllDetailed(t.Context(), requests, func(context.Context, Request) (RunOutput, error) {
			calls.Add(1)

			return RunOutput{Stdout: shouldNotRunOutput}, nil
		}, Options{LedgerPath: ledgerPath, Resume: true, Budget: Budget{MaxPromptTokens: 5, MaxCostMicros: 5}})

		require.Error(t, err)
		require.Len(t, resumed, 1)
		assert.Equal(t, StatusBudgetExhausted, resumed[0].Status)
		assert.Contains(t, resumed[0].Error, "prompt token budget exceeded")
		assert.Contains(t, resumed[0].Error, "cost budget exceeded")
		assert.Equal(t, int32(0), calls.Load())
	})

	t.Run("actual token and cost budget", func(t *testing.T) {
		t.Parallel()

		results, err := SpawnAllDetailed(t.Context(), []Request{{ID: "actual", Agent: "executor", Prompt: "actual", EstimatedPromptTokens: 1, EstimatedCostMicros: 1}}, func(context.Context, Request) (RunOutput, error) {
			return RunOutput{Stdout: "ok", PromptTokens: 10, EstimatedCostMicros: 10}, nil
		}, Options{Budget: Budget{MaxPromptTokens: 5, MaxCostMicros: 5}})

		require.Error(t, err)
		require.Len(t, results, 1)
		assert.Contains(t, err.Error(), `subagent: request "actual" budget exhausted`)
		assert.Equal(t, StatusBudgetExhausted, results[0].Status)
		assert.Contains(t, results[0].Error, "prompt token budget exceeded")
		assert.Contains(t, results[0].Error, "cost budget exceeded")
		assert.Equal(t, 10, results[0].Usage.PromptTokens)
		assert.Equal(t, int64(10), results[0].Usage.EstimatedCostMicros)
	})
}

func TestSpawnAllWithOptions_PersistsAdmissionDenialBeforeSpawn(t *testing.T) {
	t.Parallel()

	requests := []Request{{ID: "expensive", Agent: "executor", Prompt: "expensive", EstimatedPromptTokens: 10}}
	ledgerPath := filepath.Join(t.TempDir(), "spawn-ledger.json")

	var called atomic.Bool
	results, err := SpawnAllWithOptions(t.Context(), requests, func(context.Context, Request) (string, error) {
		called.Store(true)

		return shouldNotRunOutput, nil
	}, Options{
		LedgerPath:        ledgerPath,
		WorkspaceID:       "parent-session",
		AllowedWriteScope: t.TempDir(),
		Model:             "codex/gpt-test",
		Provider:          "codex",
		Timeout:           time.Second,
		RetryPolicy:       RetryPolicy{MaxAttempts: 3},
		Budget:            Budget{MaxPromptTokens: 5},
	})

	require.Error(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, StatusBudgetExhausted, results[0].Status)
	assert.False(t, called.Load())

	var ledger Ledger
	data, readErr := os.ReadFile(ledgerPath)
	require.NoError(t, readErr)
	require.NoError(t, json.Unmarshal(data, &ledger))
	require.Len(t, ledger.Admissions, 1)
	admission := ledger.Admissions[0]
	assert.NotEmpty(t, admission.AdmissionID)
	assert.Equal(t, "expensive", admission.ChildID)
	assert.Equal(t, ledger.RunID, admission.ParentRunID)
	assert.Equal(t, "parent-session/expensive", admission.WorkspaceID)
	assert.Equal(t, "codex/gpt-test", admission.Model)
	assert.Equal(t, "codex", admission.Provider)
	assert.Equal(t, time.Second, admission.Timeout)
	assert.Equal(t, Budget{MaxPromptTokens: 5}, admission.Budget)
	assert.Equal(t, RetryPolicy{MaxAttempts: 3}, admission.RetryPolicy)
	assert.False(t, admission.Admitted)
	assert.Contains(t, admission.DenyReason, "prompt token budget exceeded")
	assert.Equal(t, admission.AdmissionID, results[0].AdmissionID)
	require.Len(t, ledger.Results, 1)
	assert.Equal(t, admission.AdmissionID, ledger.Results[0].AdmissionID)
	assert.Empty(t, ledger.Attempts)
}

func TestSpawnAllWithOptions_PersistsAdmissionBeforeRunner(t *testing.T) {
	t.Parallel()

	requests := []Request{{ID: "preflight", Agent: "executor", Prompt: "preflight"}}
	ledgerPath := filepath.Join(t.TempDir(), "spawn-ledger.json")
	results, err := SpawnAllWithOptions(t.Context(), requests, func(context.Context, Request) (string, error) {
		data, readErr := os.ReadFile(ledgerPath)
		if readErr != nil {
			return "", fmt.Errorf("read ledger before runner body: %w", readErr)
		}

		var ledger Ledger
		if unmarshalErr := json.Unmarshal(data, &ledger); unmarshalErr != nil {
			return "", fmt.Errorf("unmarshal ledger before runner body: %w", unmarshalErr)
		}

		if len(ledger.Admissions) != 1 {
			return "", fmt.Errorf("expected one admission before runner body, got %d", len(ledger.Admissions))
		}

		admission := ledger.Admissions[0]
		if !admission.Admitted || admission.AdmissionID == "" || admission.ChildID != "preflight" || admission.ParentRunID == "" {
			return "", fmt.Errorf("unexpected admission before runner body: %+v", admission)
		}

		if len(ledger.Attempts) != 1 {
			return "", fmt.Errorf("expected one running attempt before runner body, got %d", len(ledger.Attempts))
		}

		attempt := ledger.Attempts[0]
		if attempt.Status != StatusRunning || attempt.AdmissionID != admission.AdmissionID {
			return "", fmt.Errorf("unexpected attempt before runner body: %+v", attempt)
		}

		return doneOutput, nil
	}, Options{
		LedgerPath:        ledgerPath,
		WorkspaceID:       "parent-run",
		AllowedWriteScope: t.TempDir(),
		Timeout:           time.Second,
		Budget:            Budget{MaxPromptTokens: 10},
	})

	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, StatusSucceeded, results[0].Status)
	assert.NotEmpty(t, results[0].AdmissionID)
}

func TestSpawnAllWithOptions_DeniesScopeViolationBeforeSpawn(t *testing.T) {
	t.Parallel()

	parentScope := t.TempDir()
	childScope := filepath.Join(parentScope, "..", "outside-scope")
	requests := []Request{{
		ID:                "escape",
		Agent:             "executor",
		Prompt:            "escape",
		AllowedWriteScope: childScope,
	}}
	ledgerPath := filepath.Join(t.TempDir(), "spawn-ledger.json")

	var called atomic.Bool
	results, err := SpawnAllWithOptions(t.Context(), requests, func(context.Context, Request) (string, error) {
		called.Store(true)

		return shouldNotRunOutput, nil
	}, Options{LedgerPath: ledgerPath, AllowedWriteScope: parentScope})

	require.Error(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, StatusDenied, results[0].Status)
	assert.Contains(t, err.Error(), `subagent: request "escape" denied`)
	assert.False(t, called.Load())

	var ledger Ledger
	data, readErr := os.ReadFile(ledgerPath)
	require.NoError(t, readErr)
	require.NoError(t, json.Unmarshal(data, &ledger))
	require.Len(t, ledger.Admissions, 1)
	admission := ledger.Admissions[0]
	assert.False(t, admission.Admitted)
	assert.Equal(t, childScope, admission.AllowedWriteScope)
	assert.Contains(t, admission.DenyReason, "escapes parent scope")
	assert.Equal(t, admission.AdmissionID, results[0].AdmissionID)
	require.Len(t, ledger.Results, 1)
	assert.Equal(t, StatusDenied, ledger.Results[0].Status)
	assert.Equal(t, admission.AdmissionID, ledger.Results[0].AdmissionID)
	assert.Empty(t, ledger.Attempts)
}

func TestSpawnAllWithOptions_DeniesSymlinkScopeEscapeBeforeSpawn(t *testing.T) {
	t.Parallel()

	parentScope := t.TempDir()
	outsideScope := t.TempDir()
	childScope := filepath.Join(parentScope, "linked-outside")
	requireSymlinkOrSkip(t, outsideScope, childScope)

	requests := []Request{{
		ID:                "escape",
		Agent:             "executor",
		Prompt:            "escape",
		AllowedWriteScope: childScope,
	}}
	ledgerPath := filepath.Join(t.TempDir(), "spawn-ledger.json")

	var called atomic.Bool
	results, err := SpawnAllWithOptions(t.Context(), requests, func(context.Context, Request) (string, error) {
		called.Store(true)

		return shouldNotRunOutput, nil
	}, Options{LedgerPath: ledgerPath, AllowedWriteScope: parentScope})

	require.Error(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, StatusDenied, results[0].Status)
	assert.Contains(t, err.Error(), `subagent: request "escape" denied`)
	assert.False(t, called.Load())

	var ledger Ledger
	data, readErr := os.ReadFile(ledgerPath)
	require.NoError(t, readErr)
	require.NoError(t, json.Unmarshal(data, &ledger))
	require.Len(t, ledger.Admissions, 1)
	admission := ledger.Admissions[0]
	assert.False(t, admission.Admitted)
	assert.Equal(t, childScope, admission.AllowedWriteScope)
	assert.Contains(t, admission.DenyReason, "escapes parent scope")
	assert.Equal(t, admission.AdmissionID, results[0].AdmissionID)
	assert.Empty(t, ledger.Attempts)
}

func TestSpawnAllWithOptions_PersistsAdmissionForHaltedSibling(t *testing.T) {
	t.Parallel()

	requests := []Request{
		{ID: failingID, Agent: "executor", Prompt: failingID},
		{ID: slowID, Agent: "executor", Prompt: slowID},
	}
	ledgerPath := filepath.Join(t.TempDir(), "spawn-ledger.json")
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
	}, Options{LedgerPath: ledgerPath, MaxConcurrency: 2, CancelOnFailure: true})

	require.Error(t, err)
	require.Len(t, results, 2)
	assert.Equal(t, StatusFailed, results[0].Status)
	assert.Equal(t, StatusCanceled, results[1].Status)

	var ledger Ledger
	data, readErr := os.ReadFile(ledgerPath)
	require.NoError(t, readErr)
	require.NoError(t, json.Unmarshal(data, &ledger))
	require.Len(t, ledger.Admissions, 2)
	admissions := map[string]Admission{}
	for _, admission := range ledger.Admissions {
		admissions[admission.ChildID] = admission
	}
	require.Contains(t, admissions, failingID)
	require.Contains(t, admissions, slowID)
	assert.NotEmpty(t, admissions[failingID].AdmissionID)
	assert.NotEmpty(t, admissions[slowID].AdmissionID)
	assert.NotEqual(t, admissions[failingID].AdmissionID, admissions[slowID].AdmissionID)
	assert.True(t, admissions[failingID].Admitted)
	assert.True(t, admissions[slowID].Admitted)
	assert.Empty(t, admissions[slowID].DenyReason)
	assert.Equal(t, ledger.RunID, admissions[slowID].ParentRunID)
	resultStatuses := map[string]string{}
	resultAdmissions := map[string]string{}
	resultStops := map[string]string{}
	resultErrors := map[string]string{}
	for _, result := range ledger.Results {
		resultStatuses[result.Request.ID] = result.Status
		resultAdmissions[result.Request.ID] = result.AdmissionID
		resultStops[result.Request.ID] = result.StopID
		resultErrors[result.Request.ID] = result.Error
	}
	attemptStatuses := map[string]string{}
	attemptAdmissions := map[string]string{}
	attemptStops := map[string]string{}
	attemptErrors := map[string]string{}
	for _, attempt := range ledger.Attempts {
		attemptStatuses[attempt.Request.ID] = attempt.Status
		attemptAdmissions[attempt.Request.ID] = attempt.AdmissionID
		attemptStops[attempt.Request.ID] = attempt.StopID
		attemptErrors[attempt.Request.ID] = attempt.Error
	}
	require.Len(t, ledger.StopReceipts, 1)
	stopReceipt := ledger.StopReceipts[0]
	assert.NotEmpty(t, stopReceipt.StopID)
	assert.Equal(t, slowID, stopReceipt.ChildID)
	assert.Equal(t, StatusCanceled, stopReceipt.Status)
	assert.Contains(t, stopReceipt.Reason, `sibling cancellation after request "fail" failed`)
	assert.Equal(t, admissions[slowID].AdmissionID, stopReceipt.AdmissionID)
	assert.Equal(t, StatusCanceled, resultStatuses[slowID])
	assert.Equal(t, StatusCanceled, attemptStatuses[slowID])
	assert.Contains(t, resultErrors[slowID], `sibling cancellation after request "fail" failed`)
	assert.Contains(t, attemptErrors[slowID], `sibling cancellation after request "fail" failed`)
	assert.Equal(t, admissions[slowID].AdmissionID, resultAdmissions[slowID])
	assert.Equal(t, admissions[slowID].AdmissionID, attemptAdmissions[slowID])
	assert.Equal(t, stopReceipt.StopID, resultStops[slowID])
	assert.Equal(t, stopReceipt.StopID, attemptStops[slowID])
}

func TestSpawnAllWithOptions_PreCanceledContextPersistsAdmissionDenials(t *testing.T) {
	t.Parallel()

	requests := []Request{
		{ID: "plan", Agent: "planner", Prompt: "plan"},
		{ID: "build", Agent: "executor", Prompt: "build"},
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	ledgerPath := filepath.Join(t.TempDir(), "spawn-ledger.json")
	var ran atomic.Bool
	results, err := SpawnAllWithOptions(ctx, requests, func(context.Context, Request) (string, error) {
		ran.Store(true)

		return shouldNotRunOutput, nil
	}, Options{LedgerPath: ledgerPath, MaxConcurrency: 2})

	require.Error(t, err)
	assert.False(t, ran.Load())
	require.Len(t, results, 2)
	assert.Equal(t, StatusCanceled, results[0].Status)
	assert.Equal(t, StatusCanceled, results[1].Status)
	assert.NotEmpty(t, results[0].AdmissionID)
	assert.NotEmpty(t, results[1].AdmissionID)

	var ledger Ledger
	data, readErr := os.ReadFile(ledgerPath)
	require.NoError(t, readErr)
	require.NoError(t, json.Unmarshal(data, &ledger))
	require.Len(t, ledger.Admissions, 2)
	require.Len(t, ledger.Results, 2)
	assert.Empty(t, ledger.Attempts)
	for _, admission := range ledger.Admissions {
		assert.False(t, admission.Admitted)
		assert.Contains(t, admission.DenyReason, context.Canceled.Error())
		assert.Equal(t, ledger.RunID, admission.ParentRunID)
	}
}

func requireSymlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()

	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink not available in this environment: %v", err)
	}
}
