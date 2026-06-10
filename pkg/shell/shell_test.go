package shell

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/permission"
)

func TestRunBash_CapturesStdoutAndEnv(t *testing.T) {
	t.Parallel()

	result, err := RunBash(context.Background(), Options{
		Command: `printf "hello $ATTELER_TEST_VALUE"`,
		Env:     map[string]string{"ATTELER_TEST_VALUE": "world"},
		Audit:   testAuditContext(t),
	})

	require.NoError(t, err)
	require.Equal(t, "hello world", result.Stdout)
	require.Empty(t, result.Stderr)
	require.Positive(t, result.Duration)
}

func TestRunBash_ReturnsStderrAndExitError(t *testing.T) {
	t.Parallel()

	result, err := RunBash(context.Background(), Options{
		Command: `printf problem >&2; exit 7`,
		Audit:   testAuditContext(t),
	})

	require.Error(t, err)
	require.Contains(t, err.Error(), "bash command failed")
	require.Equal(t, "problem", result.Stderr)
	require.NotEmpty(t, result.ExitError)
}

func TestRunBash_TimesOut(t *testing.T) {
	t.Parallel()

	_, err := RunBash(context.Background(), Options{
		Command: `sleep 1`,
		Timeout: 10 * time.Millisecond,
		Audit:   testAuditContext(t),
	})

	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "timed out") || strings.Contains(err.Error(), "killed"))
}

func TestRunBash_RejectsBlankCommand(t *testing.T) {
	t.Parallel()

	_, err := RunBash(context.Background(), Options{Command: " \t"})
	require.Error(t, err)
}

func TestRunBash_RequiresActiveContext(t *testing.T) {
	t.Parallel()

	_, err := RunBash(nil, Options{Command: "echo hello"}) //nolint:staticcheck // Verify nil contexts are rejected instead of panicking.
	require.Error(t, err)
	require.Contains(t, err.Error(), "context is required")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = RunBash(ctx, Options{Command: " \t"})
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
}

func TestRunBash_LimitsCapturedOutputBytes(t *testing.T) {
	t.Parallel()

	var chunks []OutputChunk

	result, err := RunBash(context.Background(), Options{
		Command:        `printf abcdef`,
		MaxOutputBytes: 3,
		Audit:          testAuditContext(t),
		OutputCallback: func(chunk OutputChunk) {
			chunks = append(chunks, chunk)
		},
	})

	require.Error(t, err)
	require.Contains(t, err.Error(), "output exceeded 3 bytes")
	require.Equal(t, "abc", result.Stdout)
	require.Empty(t, result.Stderr)
	require.True(t, result.OutputTruncated)
	require.Len(t, chunks, 1)
	require.Equal(t, OutputStreamStdout, chunks[0].Stream)
	require.Equal(t, "abc", string(chunks[0].Data))
}

func TestRunBash_StreamsStdoutBeforeCommandCompletes(t *testing.T) {
	t.Parallel()

	chunks := make(chan OutputChunk, 2)
	done := make(chan error, 1)

	go func() {
		_, err := RunBash(context.Background(), Options{
			Command: `printf first; sleep 0.4; printf second`,
			OutputCallback: func(chunk OutputChunk) {
				chunks <- chunk
			},
		})
		done <- err
	}()

	var chunk OutputChunk
	select {
	case chunk = <-chunks:
	case <-time.After(300 * time.Millisecond):
		require.FailNow(t, "timed out waiting for streamed stdout")
	}

	require.Equal(t, OutputStreamStdout, chunk.Stream)
	require.Equal(t, "first", string(chunk.Data))
	require.Equal(t, int64(1), chunk.Sequence)
	require.False(t, chunk.Timestamp.IsZero())

	select {
	case err := <-done:
		require.Failf(t, "command completed before delayed output", "err=%v", err)
	default:
	}

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		require.FailNow(t, "timed out waiting for command completion")
	}
}

func TestRunBash_StreamsStderrBeforeCommandCompletes(t *testing.T) {
	t.Parallel()

	chunks := make(chan OutputChunk, 2)
	done := make(chan error, 1)

	go func() {
		_, err := RunBash(context.Background(), Options{
			Command: `printf warn >&2; sleep 0.4; printf done >&2`,
			OutputCallback: func(chunk OutputChunk) {
				chunks <- chunk
			},
		})
		done <- err
	}()

	var chunk OutputChunk
	select {
	case chunk = <-chunks:
	case <-time.After(300 * time.Millisecond):
		require.FailNow(t, "timed out waiting for streamed stderr")
	}

	require.Equal(t, OutputStreamStderr, chunk.Stream)
	require.Equal(t, "warn", string(chunk.Data))

	select {
	case err := <-done:
		require.Failf(t, "command completed before delayed output", "err=%v", err)
	default:
	}

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		require.FailNow(t, "timed out waiting for command completion")
	}
}

func TestOutputCallback_DeliveryFollowsObservedSequence(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex

	var got []int64

	firstStarted := make(chan struct{})

	stdout, stderr, _ := commandOutputWriters(0, func(chunk OutputChunk) {
		if chunk.Sequence == 1 {
			close(firstStarted)
			time.Sleep(50 * time.Millisecond)
		}

		mu.Lock()
		defer mu.Unlock()

		got = append(got, chunk.Sequence)
	})

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()

		_, _ = stdout.Write([]byte("stdout"))
	}()

	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		require.FailNow(t, "timed out waiting for first callback")
	}

	go func() {
		defer wg.Done()

		_, _ = stderr.Write([]byte("stderr"))
	}()

	wg.Wait()

	require.Equal(t, []int64{1, 2}, got)
}

func TestRunInteractive_RejectsBlankCommand(t *testing.T) {
	t.Parallel()

	_, err := RunInteractive(context.Background(), Options{Command: ""})
	require.Error(t, err)
	require.Contains(t, err.Error(), "command is required")
}

func TestRunInteractive_RejectsNilContext(t *testing.T) {
	t.Parallel()

	_, err := RunInteractive(nil, Options{Command: "echo hello"}) //nolint:staticcheck // intentional nil context for test
	require.Error(t, err)
	require.Contains(t, err.Error(), "context is required")
}

func TestRunInteractive_RejectsCanceledContextBeforeCommandValidation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := RunInteractive(ctx, Options{Command: " \t"})
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
}

func TestRunInteractive_RunsSimpleCommand(t *testing.T) {
	t.Parallel()

	result, err := RunInteractive(context.Background(), Options{Command: "true", Audit: testAuditContext(t)})
	require.NoError(t, err)
	require.Positive(t, result.Duration)
	require.Empty(t, result.ExitError)
}

func TestRunInteractive_ReportsNonZeroExit(t *testing.T) {
	t.Parallel()

	result, err := RunInteractive(context.Background(), Options{Command: "exit 42", Audit: testAuditContext(t)})
	require.Error(t, err)
	require.Contains(t, err.Error(), "interactive command failed")
	require.NotEmpty(t, result.ExitError)
}

func TestRunInteractive_RespectsCancel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err := RunInteractive(ctx, Options{Command: "sleep 10", Audit: testAuditContext(t)})
	require.Error(t, err)
}

func TestRunBash_PolicyDenialHappensBeforeProcessStart(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	auditDir := filepath.Join(tmp, "audit")

	_, err := RunBash(context.Background(), Options{
		Command: "touch denied-started",
		Dir:     tmp,
		Policy:  &Policy{DenyCommands: []string{"bash"}},
		Audit: AuditContext{
			Caller:   "test-denial",
			AuditDir: auditDir,
		},
	})

	require.Error(t, err)
	require.ErrorAs(t, err, new(*PolicyError))
	require.NoFileExists(t, filepath.Join(tmp, "denied-started"))

	records := readAuditRecords(t, auditDir)
	require.Len(t, records, 1)
	require.Equal(t, "denied", records[0].Phase)
	require.Equal(t, "denied", records[0].Decision)
	require.Equal(t, "command.deny", records[0].DecisionRule)
	require.Equal(t, "test-denial", records[0].Caller)
	require.False(t, records[0].StartedAt.IsZero())
	require.False(t, records[0].EndedAt.IsZero())
}

func TestRunBash_CentralPermissionDenialHappensBeforeProcessStart(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	auditDir := filepath.Join(tmp, "audit")
	policy := permission.ReadOnlyPolicy()
	ctx := permission.ContextWithPolicy(context.Background(), &policy)

	_, err := RunBash(ctx, Options{
		Command: "touch denied-by-permission",
		Dir:     tmp,
		Audit: AuditContext{
			Caller:   "test-permission-denial",
			AuditDir: auditDir,
		},
	})

	require.Error(t, err)
	require.True(t, permission.ErrDenied(err))
	require.ErrorAs(t, err, new(*PolicyError))
	require.NoFileExists(t, filepath.Join(tmp, "denied-by-permission"))

	records := readAuditRecords(t, auditDir)
	require.Len(t, records, 1)
	require.Equal(t, "denied", records[0].Phase)
	require.Equal(t, "denied", records[0].Decision)
	require.Equal(t, "permission.write.deny", records[0].DecisionRule)
	require.Contains(t, records[0].OperationKinds, "execute")
	require.Contains(t, records[0].OperationKinds, "write")
}

func TestRunBash_CentralPermissionDenialRedactsCredentialAssignments(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	auditDir := filepath.Join(tmp, "audit")
	policy := DefaultPolicy()
	permissionPolicy := permission.DefaultPolicy()
	permissionPolicy.SetMode(permission.OperationCredentialAccess, permission.ModeDeny)
	ctx := permission.ContextWithPolicy(context.Background(), &permissionPolicy)

	_, err := RunBash(ctx, Options{
		Command: `API_TOKEN=super-secret-token printf 'blocked\n'`,
		Dir:     tmp,
		Policy:  &policy,
		Audit: AuditContext{
			Caller:   "test-permission-redaction",
			AuditDir: auditDir,
		},
	})

	require.Error(t, err)
	require.True(t, permission.ErrDenied(err))
	require.NotContains(t, err.Error(), "super-secret-token")
	require.Contains(t, err.Error(), "API_TOKEN=<redacted:API_TOKEN>")

	records := readAuditRecords(t, auditDir)
	require.Len(t, records, 1)
	require.Equal(t, "permission.credential_access.deny", records[0].DecisionRule)
	require.NotContains(t, records[0].DecisionReason, "super-secret-token")
	require.Contains(t, records[0].DecisionReason, "API_TOKEN=<redacted:API_TOKEN>")
}

func TestRunBash_CommandDenyRuleInspectsShellCommandString(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	auditDir := filepath.Join(tmp, "audit")

	_, err := RunBash(context.Background(), Options{
		Command: "touch denied-by-inner-command",
		Dir:     tmp,
		Policy:  &Policy{DenyCommands: []string{"touch"}},
		Audit:   AuditContext{AuditDir: auditDir},
	})

	require.Error(t, err)
	require.ErrorAs(t, err, new(*PolicyError))
	require.NoFileExists(t, filepath.Join(tmp, "denied-by-inner-command"))

	records := readAuditRecords(t, auditDir)
	require.Len(t, records, 1)
	require.Equal(t, "command.deny", records[0].DecisionRule)
	require.Contains(t, records[0].DecisionReason, "touch")
}

func TestRunBash_CommandDenyRuleInspectsCommandWithAttachedRedirection(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	auditDir := filepath.Join(tmp, "audit")

	_, err := RunBash(context.Background(), Options{
		Command: "touch>denied-by-attached-redirection",
		Dir:     tmp,
		Policy:  &Policy{DenyCommands: []string{"touch"}},
		Audit:   AuditContext{AuditDir: auditDir},
	})

	require.Error(t, err)
	require.ErrorAs(t, err, new(*PolicyError))
	require.NoFileExists(t, filepath.Join(tmp, "denied-by-attached-redirection"))

	records := readAuditRecords(t, auditDir)
	require.Len(t, records, 1)
	require.Equal(t, "command.deny", records[0].DecisionRule)
	require.Contains(t, records[0].DecisionReason, "touch")
}

func TestRunBash_CommandDenyRuleInspectsNestedShellCommand(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	auditDir := filepath.Join(tmp, "audit")

	_, err := RunBash(context.Background(), Options{
		Command: `bash -lc 'touch denied-by-nested-shell'`,
		Dir:     tmp,
		Policy:  &Policy{DenyCommands: []string{"touch"}},
		Audit:   AuditContext{AuditDir: auditDir},
	})

	require.Error(t, err)
	require.ErrorAs(t, err, new(*PolicyError))
	require.NoFileExists(t, filepath.Join(tmp, "denied-by-nested-shell"))

	records := readAuditRecords(t, auditDir)
	require.Len(t, records, 1)
	require.Equal(t, "command.deny", records[0].DecisionRule)
	require.Contains(t, records[0].DecisionReason, "touch")
}

func TestRunBash_CommandDenyRuleInspectsEvalCommand(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	auditDir := filepath.Join(tmp, "audit")

	_, err := RunBash(context.Background(), Options{
		Command: `eval 'touch denied-by-eval'`,
		Dir:     tmp,
		Policy:  &Policy{DenyCommands: []string{"touch"}},
		Audit:   AuditContext{AuditDir: auditDir},
	})

	require.Error(t, err)
	require.ErrorAs(t, err, new(*PolicyError))
	require.NoFileExists(t, filepath.Join(tmp, "denied-by-eval"))

	records := readAuditRecords(t, auditDir)
	require.Len(t, records, 1)
	require.Equal(t, "command.deny", records[0].DecisionRule)
	require.Contains(t, records[0].DecisionReason, "touch")
}

func TestRunBash_CommandDenyRuleInspectsBacktickCommandSubstitution(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	auditDir := filepath.Join(tmp, "audit")

	_, err := RunBash(context.Background(), Options{
		Command: "printf before `touch denied-by-backtick`",
		Dir:     tmp,
		Policy:  &Policy{DenyCommands: []string{"touch"}},
		Audit:   AuditContext{AuditDir: auditDir},
	})

	require.Error(t, err)
	require.ErrorAs(t, err, new(*PolicyError))
	require.NoFileExists(t, filepath.Join(tmp, "denied-by-backtick"))

	records := readAuditRecords(t, auditDir)
	require.Len(t, records, 1)
	require.Equal(t, "command.deny", records[0].DecisionRule)
	require.Contains(t, records[0].DecisionReason, "touch")
}

func TestRunBash_CommandDenyRuleInspectsDoubleQuotedCommandSubstitution(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	auditDir := filepath.Join(tmp, "audit")

	_, err := RunBash(context.Background(), Options{
		Command: `printf "%s" "$(touch denied-by-quoted-substitution)"`,
		Dir:     tmp,
		Policy:  &Policy{DenyCommands: []string{"touch"}},
		Audit:   AuditContext{AuditDir: auditDir},
	})

	require.Error(t, err)
	require.ErrorAs(t, err, new(*PolicyError))
	require.NoFileExists(t, filepath.Join(tmp, "denied-by-quoted-substitution"))

	records := readAuditRecords(t, auditDir)
	require.Len(t, records, 1)
	require.Equal(t, "command.deny", records[0].DecisionRule)
	require.Contains(t, records[0].DecisionReason, "touch")
}

func TestRunBash_CommandDenyRuleIgnoresSingleQuotedCommandSubstitution(t *testing.T) {
	t.Parallel()

	auditDir := filepath.Join(t.TempDir(), "audit")

	result, err := RunBash(context.Background(), Options{
		Command: `printf "%s" '$(touch single-quoted-literal)'`,
		Policy:  &Policy{DenyCommands: []string{"touch"}},
		Audit:   AuditContext{AuditDir: auditDir},
	})

	require.NoError(t, err)
	require.Equal(t, "$(touch single-quoted-literal)", result.Stdout)
}

func TestRunBash_CommandDenyRuleDoesNotTreatKeywordArgumentAsCommandReset(t *testing.T) {
	t.Parallel()

	auditDir := filepath.Join(t.TempDir(), "audit")

	result, err := RunBash(context.Background(), Options{
		Command: `printf '%s' then touch`,
		Policy:  &Policy{DenyCommands: []string{"touch"}},
		Audit:   AuditContext{AuditDir: auditDir},
	})

	require.NoError(t, err)
	require.Equal(t, "thentouch", result.Stdout)

	records := readAuditRecords(t, auditDir)
	require.Len(t, records, 2)
	require.Equal(t, "allowed", records[0].Decision)
}

func TestRunBash_CommandDenyRuleInspectsCommandAfterNegationKeyword(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	auditDir := filepath.Join(tmp, "audit")

	_, err := RunBash(context.Background(), Options{
		Command: `! touch denied-after-negation`,
		Dir:     tmp,
		Policy:  &Policy{DenyCommands: []string{"touch"}},
		Audit:   AuditContext{AuditDir: auditDir},
	})

	require.Error(t, err)
	require.ErrorAs(t, err, new(*PolicyError))
	require.NoFileExists(t, filepath.Join(tmp, "denied-after-negation"))

	records := readAuditRecords(t, auditDir)
	require.Len(t, records, 1)
	require.Equal(t, "command.deny", records[0].DecisionRule)
	require.Contains(t, records[0].DecisionReason, "touch")
}

func TestRunBash_CommandAllowRuleInspectsShellCommandString(t *testing.T) {
	t.Parallel()

	auditDir := filepath.Join(t.TempDir(), "audit")

	result, err := RunBash(context.Background(), Options{
		Command: "printf allowed",
		Policy:  &Policy{AllowCommands: []string{"printf"}},
		Audit:   AuditContext{AuditDir: auditDir},
	})

	require.NoError(t, err)
	require.Equal(t, "allowed", result.Stdout)
}

func TestRunBash_CommandAllowRuleDeniesUnlistedShellCommand(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	auditDir := filepath.Join(tmp, "audit")

	_, err := RunBash(context.Background(), Options{
		Command: "printf allowed; touch denied-by-allow-list",
		Dir:     tmp,
		Policy:  &Policy{AllowCommands: []string{"printf"}},
		Audit:   AuditContext{AuditDir: auditDir},
	})

	require.Error(t, err)
	require.ErrorAs(t, err, new(*PolicyError))
	require.NoFileExists(t, filepath.Join(tmp, "denied-by-allow-list"))

	records := readAuditRecords(t, auditDir)
	require.Len(t, records, 1)
	require.Equal(t, "command.allow", records[0].DecisionRule)
}

func TestRunBash_CommandAllowRuleDoesNotTrustOnlyShellWrapper(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	auditDir := filepath.Join(tmp, "audit")

	_, err := RunBash(context.Background(), Options{
		Command: "touch denied-by-bash-wrapper",
		Dir:     tmp,
		Policy:  &Policy{AllowCommands: []string{"bash"}},
		Audit:   AuditContext{AuditDir: auditDir},
	})

	require.Error(t, err)
	require.ErrorAs(t, err, new(*PolicyError))
	require.NoFileExists(t, filepath.Join(tmp, "denied-by-bash-wrapper"))

	records := readAuditRecords(t, auditDir)
	require.Len(t, records, 1)
	require.Equal(t, "command.allow", records[0].DecisionRule)
	require.Contains(t, records[0].DecisionReason, "touch")
}

func TestRunBash_CommandDenyRuleCanDenyShellWrapper(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	auditDir := filepath.Join(tmp, "audit")

	_, err := RunBash(context.Background(), Options{
		Command: "env touch denied-by-env-wrapper",
		Dir:     tmp,
		Policy:  &Policy{DenyCommands: []string{"env"}},
		Audit:   AuditContext{AuditDir: auditDir},
	})

	require.Error(t, err)
	require.ErrorAs(t, err, new(*PolicyError))
	require.NoFileExists(t, filepath.Join(tmp, "denied-by-env-wrapper"))

	records := readAuditRecords(t, auditDir)
	require.Len(t, records, 1)
	require.Equal(t, "command.deny", records[0].DecisionRule)
	require.Contains(t, records[0].DecisionReason, "env")
}

func TestRunBash_CommandAllowRuleIncludesShellWrapper(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	auditDir := filepath.Join(tmp, "audit")

	_, err := RunBash(context.Background(), Options{
		Command: "env touch denied-by-unlisted-env-wrapper",
		Dir:     tmp,
		Policy:  &Policy{AllowCommands: []string{"touch"}},
		Audit:   AuditContext{AuditDir: auditDir},
	})

	require.Error(t, err)
	require.ErrorAs(t, err, new(*PolicyError))
	require.NoFileExists(t, filepath.Join(tmp, "denied-by-unlisted-env-wrapper"))

	records := readAuditRecords(t, auditDir)
	require.Len(t, records, 1)
	require.Equal(t, "command.allow", records[0].DecisionRule)
	require.Contains(t, records[0].DecisionReason, "env")
}

func TestCommandContext_DirectWrapperAllowRuleInspectsWrappedCommand(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	auditDir := filepath.Join(tmp, "audit")
	target := filepath.Join(tmp, "direct-wrapper-allow-started")

	_, _, err := CommandContext(context.Background(), CommandOptions{
		Program: "env",
		Args:    []string{"touch", target},
		Policy:  &Policy{AllowCommands: []string{"env"}},
		Audit:   AuditContext{AuditDir: auditDir},
	})

	require.Error(t, err)
	require.ErrorAs(t, err, new(*PolicyError))
	require.NoFileExists(t, target)

	records := readAuditRecords(t, auditDir)
	require.Len(t, records, 1)
	require.Equal(t, "command.allow", records[0].DecisionRule)
	require.Contains(t, records[0].DecisionReason, "touch")
}

func TestCommandContext_DirectWrapperAllowRuleAllowsWrapperAndWrappedCommand(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	auditDir := filepath.Join(tmp, "audit")
	target := filepath.Join(tmp, "direct-wrapper-allowed")

	cmd, invocation, err := CommandContext(context.Background(), CommandOptions{
		Program: "env",
		Args:    []string{"touch", target},
		Policy:  &Policy{AllowCommands: []string{"env", "touch"}},
		Audit:   AuditContext{AuditDir: auditDir},
	})
	require.NoError(t, err)

	runErr := cmd.Run()
	require.NoError(t, invocation.Finish(FinishOptions{Error: runErr, OutputCapture: OutputCaptured}))
	require.NoError(t, runErr)
	require.FileExists(t, target)
}

func TestRunBash_CommandDenyRuleInspectsCommandAfterWrapperOptions(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	auditDir := filepath.Join(tmp, "audit")

	_, err := RunBash(context.Background(), Options{
		Command: `sudo -u root touch denied-after-wrapper-option`,
		Dir:     tmp,
		Policy:  &Policy{DenyCommands: []string{"touch"}},
		Audit:   AuditContext{AuditDir: auditDir},
	})

	require.Error(t, err)
	require.ErrorAs(t, err, new(*PolicyError))
	require.NoFileExists(t, filepath.Join(tmp, "denied-after-wrapper-option"))

	records := readAuditRecords(t, auditDir)
	require.Len(t, records, 1)
	require.Equal(t, "command.deny", records[0].DecisionRule)
	require.Contains(t, records[0].DecisionReason, "touch")
}

func TestRunBash_DefaultPolicyDeniesDestructivePattern(t *testing.T) {
	t.Parallel()

	_, err := RunBash(context.Background(), Options{
		Command: "rm -rf /",
		Audit:   AuditContext{AuditDir: filepath.Join(t.TempDir(), "audit")},
	})

	require.Error(t, err)
	require.ErrorAs(t, err, new(*PolicyError))
	require.Contains(t, err.Error(), "destructive")
}

func TestRunBash_DefaultPolicyDeniesDestructiveRMFlagOrder(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	auditDir := filepath.Join(tmp, "audit")

	_, err := RunBash(context.Background(), Options{
		Command: "rm -fr /",
		Dir:     tmp,
		Audit:   AuditContext{AuditDir: auditDir},
	})

	require.Error(t, err)
	require.ErrorAs(t, err, new(*PolicyError))

	records := readAuditRecords(t, auditDir)
	require.Len(t, records, 1)
	require.Equal(t, "destructive.deny", records[0].DecisionRule)
}

func TestRunBash_DefaultPolicyDeniesDestructiveRMHomeExpansion(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	auditDir := filepath.Join(tmp, "audit")

	_, err := RunBash(context.Background(), Options{
		Command: `rm -rf "$HOME"`,
		Dir:     tmp,
		Audit:   AuditContext{AuditDir: auditDir},
	})

	require.Error(t, err)
	require.ErrorAs(t, err, new(*PolicyError))

	records := readAuditRecords(t, auditDir)
	require.Len(t, records, 1)
	require.Equal(t, "destructive.deny", records[0].DecisionRule)
}

func TestRunBash_DefaultPolicyDeniesDestructiveRMPWDExpansion(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	auditDir := filepath.Join(tmp, "audit")

	_, err := RunBash(context.Background(), Options{
		Command: `rm -rf "${PWD}"`,
		Dir:     tmp,
		Audit:   AuditContext{AuditDir: auditDir},
	})

	require.Error(t, err)
	require.ErrorAs(t, err, new(*PolicyError))

	records := readAuditRecords(t, auditDir)
	require.Len(t, records, 1)
	require.Equal(t, "destructive.deny", records[0].DecisionRule)
}

func TestRunBash_DefaultPolicyDeniesDestructiveEvalCommand(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	auditDir := filepath.Join(tmp, "audit")

	_, err := RunBash(context.Background(), Options{
		Command: `eval 'rm -fr /'`,
		Dir:     tmp,
		Audit:   AuditContext{AuditDir: auditDir},
	})

	require.Error(t, err)
	require.ErrorAs(t, err, new(*PolicyError))

	records := readAuditRecords(t, auditDir)
	require.Len(t, records, 1)
	require.Equal(t, "destructive.deny", records[0].DecisionRule)
}

func TestRunBash_DeniesNetworkCommandBeforeStart(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	auditDir := filepath.Join(tmp, "audit")

	_, err := RunBash(context.Background(), Options{
		Command: `curl https://example.invalid > network-started`,
		Dir:     tmp,
		Policy:  &Policy{DenyNetwork: true},
		Audit:   AuditContext{AuditDir: auditDir},
	})

	require.Error(t, err)
	require.ErrorAs(t, err, new(*PolicyError))
	require.NoFileExists(t, filepath.Join(tmp, "network-started"))

	records := readAuditRecords(t, auditDir)
	require.Len(t, records, 1)
	require.Equal(t, "network.deny", records[0].DecisionRule)
}

func TestRunBash_DeniesNestedNetworkCommandBeforeStart(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	auditDir := filepath.Join(tmp, "audit")

	_, err := RunBash(context.Background(), Options{
		Command: `bash -lc 'curl https://example.invalid > nested-network-started'`,
		Dir:     tmp,
		Policy:  &Policy{DenyNetwork: true},
		Audit:   AuditContext{AuditDir: auditDir},
	})

	require.Error(t, err)
	require.ErrorAs(t, err, new(*PolicyError))
	require.NoFileExists(t, filepath.Join(tmp, "nested-network-started"))

	records := readAuditRecords(t, auditDir)
	require.Len(t, records, 1)
	require.Equal(t, "network.deny", records[0].DecisionRule)
}

func TestRunBash_DeniesEvalNetworkCommandBeforeStart(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	auditDir := filepath.Join(tmp, "audit")

	_, err := RunBash(context.Background(), Options{
		Command: `eval 'curl https://example.invalid > eval-network-started'`,
		Dir:     tmp,
		Policy:  &Policy{DenyNetwork: true},
		Audit:   AuditContext{AuditDir: auditDir},
	})

	require.Error(t, err)
	require.ErrorAs(t, err, new(*PolicyError))
	require.NoFileExists(t, filepath.Join(tmp, "eval-network-started"))

	records := readAuditRecords(t, auditDir)
	require.Len(t, records, 1)
	require.Equal(t, "network.deny", records[0].DecisionRule)
}

func TestRunBash_DeniesGitNetworkShellCommandBeforeStart(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	auditDir := filepath.Join(tmp, "audit")

	_, err := RunBash(context.Background(), Options{
		Command: `git -C . fetch origin > git-network-started`,
		Dir:     tmp,
		Policy:  &Policy{DenyNetwork: true},
		Audit:   AuditContext{AuditDir: auditDir},
	})

	require.Error(t, err)
	require.ErrorAs(t, err, new(*PolicyError))
	require.NoFileExists(t, filepath.Join(tmp, "git-network-started"))

	records := readAuditRecords(t, auditDir)
	require.Len(t, records, 1)
	require.Equal(t, "network.deny", records[0].DecisionRule)
}

func TestRunBash_DeniesGitNetworkShellCommandWithAttachedRedirectionBeforeStart(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	auditDir := filepath.Join(tmp, "audit")

	_, err := RunBash(context.Background(), Options{
		Command: `git -C . fetch>git-network-attached-redirection`,
		Dir:     tmp,
		Policy:  &Policy{DenyNetwork: true},
		Audit:   AuditContext{AuditDir: auditDir},
	})

	require.Error(t, err)
	require.ErrorAs(t, err, new(*PolicyError))
	require.NoFileExists(t, filepath.Join(tmp, "git-network-attached-redirection"))

	records := readAuditRecords(t, auditDir)
	require.Len(t, records, 1)
	require.Equal(t, "network.deny", records[0].DecisionRule)
}

func TestRunBash_DenyNetworkDoesNotTreatArgumentAsCommand(t *testing.T) {
	t.Parallel()

	auditDir := filepath.Join(t.TempDir(), "audit")

	result, err := RunBash(context.Background(), Options{
		Command: `printf "%s" curl`,
		Policy:  &Policy{DenyNetwork: true},
		Audit:   AuditContext{AuditDir: auditDir},
	})

	require.NoError(t, err)
	require.Equal(t, "curl", result.Stdout)

	records := readAuditRecords(t, auditDir)
	require.Len(t, records, 2)
	require.Equal(t, "allowed", records[0].Decision)
}

func TestRunBash_DeniesNetworkCommandAfterNegationKeywordBeforeStart(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	auditDir := filepath.Join(tmp, "audit")

	_, err := RunBash(context.Background(), Options{
		Command: `! curl --version; touch network-negation-started`,
		Dir:     tmp,
		Policy:  &Policy{DenyNetwork: true},
		Audit:   AuditContext{AuditDir: auditDir},
	})

	require.Error(t, err)
	require.ErrorAs(t, err, new(*PolicyError))
	require.NoFileExists(t, filepath.Join(tmp, "network-negation-started"))

	records := readAuditRecords(t, auditDir)
	require.Len(t, records, 1)
	require.Equal(t, "network.deny", records[0].DecisionRule)
}

func TestRunBash_DeniesNetworkCommandAfterWrapperOptionsBeforeStart(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	auditDir := filepath.Join(tmp, "audit")

	_, err := RunBash(context.Background(), Options{
		Command: `env -u FOO curl --version; touch network-wrapper-option-started`,
		Dir:     tmp,
		Policy:  &Policy{DenyNetwork: true},
		Audit:   AuditContext{AuditDir: auditDir},
	})

	require.Error(t, err)
	require.ErrorAs(t, err, new(*PolicyError))
	require.NoFileExists(t, filepath.Join(tmp, "network-wrapper-option-started"))

	records := readAuditRecords(t, auditDir)
	require.Len(t, records, 1)
	require.Equal(t, "network.deny", records[0].DecisionRule)
}

func TestCommandContext_DeniesGitNetworkSubcommandAfterGlobalFlags(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	auditDir := filepath.Join(tmp, "audit")

	_, _, err := CommandContext(context.Background(), CommandOptions{
		Program: "git",
		Args:    []string{"-C", tmp, "-c", "credential.helper=", "fetch", "origin"},
		Policy:  &Policy{DenyNetwork: true},
		Audit:   AuditContext{AuditDir: auditDir},
	})

	require.Error(t, err)
	require.ErrorAs(t, err, new(*PolicyError))

	records := readAuditRecords(t, auditDir)
	require.Len(t, records, 1)
	require.Equal(t, "network.deny", records[0].DecisionRule)
}

func TestRunBash_DeniesCWDGlobBeforeStart(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	auditDir := filepath.Join(tmp, "audit")

	_, err := RunBash(context.Background(), Options{
		Command: `touch path-started`,
		Dir:     tmp,
		Policy:  &Policy{DenyPathGlobs: []string{tmp}},
		Audit:   AuditContext{AuditDir: auditDir},
	})

	require.Error(t, err)
	require.ErrorAs(t, err, new(*PolicyError))
	require.NoFileExists(t, filepath.Join(tmp, "path-started"))

	records := readAuditRecords(t, auditDir)
	require.Len(t, records, 1)
	require.Equal(t, "path.deny", records[0].DecisionRule)
}

func TestCommandContext_RecordsEffectiveCWDWhenDirEmpty(t *testing.T) {
	t.Parallel()

	auditDir := filepath.Join(t.TempDir(), "audit")
	cwd, err := os.Getwd()
	require.NoError(t, err)

	result, err := RunBash(context.Background(), Options{
		Command: "pwd",
		Audit:   AuditContext{AuditDir: auditDir},
	})

	require.NoError(t, err)
	require.Equal(t, filepath.Clean(cwd), strings.TrimSpace(result.Stdout))

	records := readAuditRecords(t, auditDir)
	require.Len(t, records, 2)
	require.Equal(t, filepath.Clean(cwd), records[0].CWD)
	require.Equal(t, filepath.Clean(cwd), records[1].CWD)
}

func TestCommandContext_AllowPathGlobChecksEffectiveCWDWhenDirEmpty(t *testing.T) {
	t.Parallel()

	auditDir := filepath.Join(t.TempDir(), "audit")

	_, _, err := CommandContext(context.Background(), CommandOptions{
		Program: "true",
		Policy:  &Policy{AllowPathGlobs: []string{filepath.Join(t.TempDir(), "*")}},
		Audit:   AuditContext{AuditDir: auditDir},
	})

	require.Error(t, err)
	require.ErrorAs(t, err, new(*PolicyError))

	records := readAuditRecords(t, auditDir)
	require.Len(t, records, 1)
	require.Equal(t, "path.allow", records[0].DecisionRule)
	require.NotEmpty(t, records[0].CWD)
}

func TestCommandContext_DeniesPathArgumentGlobBeforeStart(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	auditDir := filepath.Join(tmp, "audit")
	target := filepath.Join(tmp, "path-argument-started")

	_, _, err := CommandContext(context.Background(), CommandOptions{
		Program: "touch",
		Args:    []string{target},
		Policy:  &Policy{DenyPathGlobs: []string{target}},
		Audit:   AuditContext{AuditDir: auditDir},
	})

	require.Error(t, err)
	require.ErrorAs(t, err, new(*PolicyError))
	require.NoFileExists(t, target)

	records := readAuditRecords(t, auditDir)
	require.Len(t, records, 1)
	require.Equal(t, "path.deny", records[0].DecisionRule)
}

func TestCommandContext_DeniesLongOptionPathArgumentGlobBeforeStart(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	auditDir := filepath.Join(tmp, "audit")
	blocked := filepath.Join(tmp, "blocked")

	_, _, err := CommandContext(context.Background(), CommandOptions{
		Program: "git",
		Args:    []string{"--work-tree=" + blocked, "status"},
		Policy:  &Policy{DenyPathGlobs: []string{blocked}},
		Audit:   AuditContext{AuditDir: auditDir},
	})

	require.Error(t, err)
	require.ErrorAs(t, err, new(*PolicyError))

	records := readAuditRecords(t, auditDir)
	require.Len(t, records, 1)
	require.Equal(t, "path.deny", records[0].DecisionRule)
	require.Contains(t, records[0].DecisionReason, blocked)
}

func TestCommandContext_DeniesAttachedShortOptionPathArgumentGlobBeforeStart(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	auditDir := filepath.Join(tmp, "audit")
	blocked := filepath.Join(tmp, "blocked")

	_, _, err := CommandContext(context.Background(), CommandOptions{
		Program: "tar",
		Args:    []string{"-C" + blocked, "-cf", "archive.tar", "."},
		Policy:  &Policy{DenyPathGlobs: []string{blocked}},
		Audit:   AuditContext{AuditDir: auditDir},
	})

	require.Error(t, err)
	require.ErrorAs(t, err, new(*PolicyError))

	records := readAuditRecords(t, auditDir)
	require.Len(t, records, 1)
	require.Equal(t, "path.deny", records[0].DecisionRule)
	require.Contains(t, records[0].DecisionReason, blocked)
}

func TestCommandContext_DeniesProgramPathGlobBeforeStart(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	auditDir := filepath.Join(tmp, "audit")
	blockedProgram := filepath.Join(tmp, "blocked", "tool")

	_, _, err := CommandContext(context.Background(), CommandOptions{
		Program: blockedProgram,
		Policy:  &Policy{DenyPathGlobs: []string{filepath.Join(tmp, "blocked", "*")}},
		Audit:   AuditContext{AuditDir: auditDir},
	})

	require.Error(t, err)
	require.ErrorAs(t, err, new(*PolicyError))

	records := readAuditRecords(t, auditDir)
	require.Len(t, records, 1)
	require.Equal(t, "path.deny", records[0].DecisionRule)
	require.Contains(t, records[0].DecisionReason, blockedProgram)
}

func TestCommandContext_ShellArgsDenyRuleInspectsDashCArgumentBeforeStart(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	auditDir := filepath.Join(tmp, "audit")

	_, _, err := CommandContext(context.Background(), CommandOptions{
		Program: "bash",
		Args:    []string{"--noprofile", "--norc", "-lc", "touch denied-shell-arg-started"},
		Dir:     tmp,
		Policy:  &Policy{DenyCommands: []string{"touch"}},
		Audit:   AuditContext{AuditDir: auditDir},
	})

	require.Error(t, err)
	require.ErrorAs(t, err, new(*PolicyError))
	require.NoFileExists(t, filepath.Join(tmp, "denied-shell-arg-started"))

	records := readAuditRecords(t, auditDir)
	require.Len(t, records, 1)
	require.Equal(t, "command.deny", records[0].DecisionRule)
	require.Contains(t, records[0].DecisionReason, "touch")
}

func TestRunBash_DeniesShellPathArgumentGlobBeforeStart(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	auditDir := filepath.Join(tmp, "audit")
	target := filepath.Join(tmp, "shell-path-argument-started")

	_, err := RunBash(context.Background(), Options{
		Command: "touch " + shellQuote(target),
		Dir:     tmp,
		Policy:  &Policy{DenyPathGlobs: []string{target}},
		Audit:   AuditContext{AuditDir: auditDir},
	})

	require.Error(t, err)
	require.ErrorAs(t, err, new(*PolicyError))
	require.NoFileExists(t, target)

	records := readAuditRecords(t, auditDir)
	require.Len(t, records, 1)
	require.Equal(t, "path.deny", records[0].DecisionRule)
	require.Contains(t, records[0].DecisionReason, target)
}

func TestRunBash_DeniesNestedShellPathArgumentGlobBeforeStart(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	auditDir := filepath.Join(tmp, "audit")
	blocked := filepath.Join(tmp, "blocked")
	require.NoError(t, os.Mkdir(blocked, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(blocked, "secret.txt"), []byte("secret"), 0o600))

	_, err := RunBash(context.Background(), Options{
		Command: `bash -lc 'cat blocked/secret.txt >/dev/null; touch nested-shell-path-started'`,
		Dir:     tmp,
		Policy:  &Policy{DenyPathGlobs: []string{filepath.Join(blocked, "*")}},
		Audit:   AuditContext{AuditDir: auditDir},
	})

	require.Error(t, err)
	require.ErrorAs(t, err, new(*PolicyError))
	require.NoFileExists(t, filepath.Join(tmp, "nested-shell-path-started"))

	records := readAuditRecords(t, auditDir)
	require.Len(t, records, 1)
	require.Equal(t, "path.deny", records[0].DecisionRule)
	require.Contains(t, records[0].DecisionReason, filepath.Join(blocked, "secret.txt"))
}

func TestRunBash_DeniesEvalPathArgumentGlobBeforeStart(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	auditDir := filepath.Join(tmp, "audit")
	target := filepath.Join(tmp, "eval-path-started")

	_, err := RunBash(context.Background(), Options{
		Command: "eval 'touch " + shellQuote(target) + "'",
		Dir:     tmp,
		Policy:  &Policy{DenyPathGlobs: []string{target}},
		Audit:   AuditContext{AuditDir: auditDir},
	})

	require.Error(t, err)
	require.ErrorAs(t, err, new(*PolicyError))
	require.NoFileExists(t, target)

	records := readAuditRecords(t, auditDir)
	require.Len(t, records, 1)
	require.Equal(t, "path.deny", records[0].DecisionRule)
	require.Contains(t, records[0].DecisionReason, target)
}

func TestRunBash_DeniesShellRedirectionPathGlobBeforeStart(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	auditDir := filepath.Join(tmp, "audit")
	target := filepath.Join(tmp, "shell-redirection-started")

	_, err := RunBash(context.Background(), Options{
		Command: "printf nope >" + shellQuote(target),
		Dir:     tmp,
		Policy:  &Policy{DenyPathGlobs: []string{target}},
		Audit:   AuditContext{AuditDir: auditDir},
	})

	require.Error(t, err)
	require.ErrorAs(t, err, new(*PolicyError))
	require.NoFileExists(t, target)

	records := readAuditRecords(t, auditDir)
	require.Len(t, records, 1)
	require.Equal(t, "path.deny", records[0].DecisionRule)
	require.Contains(t, records[0].DecisionReason, target)
}

func TestRunBash_DeniesAttachedShellRedirectionPathGlobBeforeStart(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	auditDir := filepath.Join(tmp, "audit")
	target := filepath.Join(tmp, "attached-shell-redirection-started")

	_, err := RunBash(context.Background(), Options{
		Command: "printf nope>" + shellQuote(target),
		Dir:     tmp,
		Policy:  &Policy{DenyPathGlobs: []string{target}},
		Audit:   AuditContext{AuditDir: auditDir},
	})

	require.Error(t, err)
	require.ErrorAs(t, err, new(*PolicyError))
	require.NoFileExists(t, target)

	records := readAuditRecords(t, auditDir)
	require.Len(t, records, 1)
	require.Equal(t, "path.deny", records[0].DecisionRule)
	require.Contains(t, records[0].DecisionReason, target)
}

func TestRunBash_DeniesDoubleQuotedCommandSubstitutionPathGlobBeforeStart(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	auditDir := filepath.Join(tmp, "audit")
	target := filepath.Join(tmp, "quoted-substitution-path-started")

	_, err := RunBash(context.Background(), Options{
		Command: `printf "%s" "$(cat ` + shellQuote(target) + `)"`,
		Dir:     tmp,
		Policy:  &Policy{DenyPathGlobs: []string{target}},
		Audit:   AuditContext{AuditDir: auditDir},
	})

	require.Error(t, err)
	require.ErrorAs(t, err, new(*PolicyError))

	records := readAuditRecords(t, auditDir)
	require.Len(t, records, 1)
	require.Equal(t, "path.deny", records[0].DecisionRule)
	require.Contains(t, records[0].DecisionReason, target)
}

func TestRunBash_AllowPathGlobInspectsShellTokensNotWholeScript(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	auditDir := filepath.Join(tmp, "audit")
	target := filepath.Join(tmp, "allowed-shell-path")

	result, err := RunBash(context.Background(), Options{
		Command: "touch " + shellQuote(target),
		Dir:     tmp,
		Policy: &Policy{AllowPathGlobs: []string{
			tmp,
			filepath.Join(tmp, "*"),
		}},
		Audit: AuditContext{AuditDir: auditDir},
	})

	require.NoError(t, err)
	require.Empty(t, result.Stderr)
	require.FileExists(t, target)
}

func TestRunBash_AllowPathGlobInspectsNestedShellScriptNotWholeArgument(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	auditDir := filepath.Join(tmp, "audit")
	target := filepath.Join(tmp, "allowed-nested-shell-path")

	result, err := RunBash(context.Background(), Options{
		Command: "bash --noprofile --norc -lc 'touch " + shellQuote(target) + "'",
		Dir:     tmp,
		Policy: &Policy{AllowPathGlobs: []string{
			tmp,
			filepath.Join(tmp, "*"),
		}},
		Audit: AuditContext{AuditDir: auditDir},
	})

	require.NoError(t, err)
	require.Empty(t, result.Stderr)
	require.FileExists(t, target)
}

func TestRunBash_AllowPathGlobInspectsEvalScriptNotWholeArgument(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	auditDir := filepath.Join(tmp, "audit")
	target := filepath.Join(tmp, "allowed-eval-shell-path")

	result, err := RunBash(context.Background(), Options{
		Command: "eval 'touch " + shellQuote(target) + "'",
		Dir:     tmp,
		Policy: &Policy{AllowPathGlobs: []string{
			tmp,
			filepath.Join(tmp, "*"),
		}},
		Audit: AuditContext{AuditDir: auditDir},
	})

	require.NoError(t, err)
	require.Empty(t, result.Stderr)
	require.FileExists(t, target)
}

func TestRunBash_StripsCredentialEnvAndRedactsAuditOutput(t *testing.T) {
	t.Parallel()

	auditDir := filepath.Join(t.TempDir(), "audit")

	result, err := RunBash(context.Background(), Options{
		Command: `printf "%s" "${ATTELER_SECRET_TOKEN-unset}"`,
		Env:     map[string]string{"ATTELER_SECRET_TOKEN": "super-secret-value"},
		Audit: AuditContext{
			Caller:    "test-secret",
			SessionID: "session-1",
			AuditDir:  auditDir,
		},
	})

	require.NoError(t, err)
	require.Equal(t, "unset", result.Stdout)

	ledgerBytes, err := os.ReadFile(filepath.Join(auditDir, ledgerFileName))
	require.NoError(t, err)
	require.NotContains(t, string(ledgerBytes), "super-secret-value")
	require.Contains(t, string(ledgerBytes), "ATTELER_SECRET_TOKEN")
	require.Contains(t, string(ledgerBytes), "redacted")

	records := readAuditRecords(t, auditDir)
	finish := records[len(records)-1]
	require.Equal(t, "finish", finish.Phase)
	require.Equal(t, "captured", finish.OutputCapture)
	require.NotEmpty(t, finish.OutputPath)

	outputBytes, err := os.ReadFile(finish.OutputPath)
	require.NoError(t, err)
	require.NotContains(t, string(outputBytes), "super-secret-value")
}

func TestRunBash_StripsPATCredentialEnv(t *testing.T) {
	t.Parallel()

	auditDir := filepath.Join(t.TempDir(), "audit")
	secret := strings.Repeat("p", 16)

	result, err := RunBash(context.Background(), Options{
		Command: `printf "%s" "${GH_PAT-unset}"`,
		Env:     map[string]string{"GH_PAT": secret},
		Audit:   AuditContext{AuditDir: auditDir},
	})

	require.NoError(t, err)
	require.Equal(t, "unset", result.Stdout)

	ledgerBytes, err := os.ReadFile(filepath.Join(auditDir, ledgerFileName))
	require.NoError(t, err)
	require.NotContains(t, string(ledgerBytes), secret)
	require.Contains(t, string(ledgerBytes), "GH_PAT")
	require.Contains(t, string(ledgerBytes), "redacted")
}

func TestRunBash_StripsAccessKeyCredentialEnv(t *testing.T) {
	t.Parallel()

	auditDir := filepath.Join(t.TempDir(), "audit")
	secret := strings.Repeat("a", 16)

	result, err := RunBash(context.Background(), Options{
		Command: `printf "%s" "${AWS_ACCESS_KEY_ID-unset}"`,
		Env:     map[string]string{"AWS_ACCESS_KEY_ID": secret},
		Audit:   AuditContext{AuditDir: auditDir},
	})

	require.NoError(t, err)
	require.Equal(t, "unset", result.Stdout)

	ledgerBytes, err := os.ReadFile(filepath.Join(auditDir, ledgerFileName))
	require.NoError(t, err)
	require.NotContains(t, string(ledgerBytes), secret)
	require.Contains(t, string(ledgerBytes), "AWS_ACCESS_KEY_ID")
	require.Contains(t, string(ledgerBytes), "redacted")
}

func TestRunBash_DoesNotStripAuthorEnvAsCredential(t *testing.T) {
	t.Parallel()

	auditDir := filepath.Join(t.TempDir(), "audit")

	result, err := RunBash(context.Background(), Options{
		Command: `printf "%s" "$GIT_AUTHOR_NAME"`,
		Env:     map[string]string{"GIT_AUTHOR_NAME": "Ada"},
		Audit:   AuditContext{AuditDir: auditDir},
	})

	require.NoError(t, err)
	require.Equal(t, "Ada", result.Stdout)

	records := readAuditRecords(t, auditDir)
	require.NotEmpty(t, records)

	for _, change := range records[0].EnvDiff {
		if change.Name == "GIT_AUTHOR_NAME" {
			require.NotEqual(t, "redacted", change.Action)
		}
	}
}

func TestRunBash_AllowedCredentialEnvPropagatesButAuditRedacts(t *testing.T) {
	t.Parallel()

	auditDir := filepath.Join(t.TempDir(), "audit")
	secret := "allowed-secret-value"

	result, err := RunBash(context.Background(), Options{
		Command: `printf "%s" "$ATTELER_SECRET_TOKEN"`,
		Env:     map[string]string{"ATTELER_SECRET_TOKEN": secret},
		Policy:  &Policy{AllowCredentialEnv: []string{"ATTELER_SECRET_TOKEN"}},
		Audit:   AuditContext{AuditDir: auditDir},
	})

	require.NoError(t, err)
	require.Equal(t, secret, result.Stdout)

	records := readAuditRecords(t, auditDir)
	finish := records[len(records)-1]
	require.Equal(t, "finish", finish.Phase)
	require.NotEmpty(t, finish.OutputPath)

	ledgerBytes, err := os.ReadFile(filepath.Join(auditDir, ledgerFileName))
	require.NoError(t, err)
	require.NotContains(t, string(ledgerBytes), secret)

	outputBytes, err := os.ReadFile(finish.OutputPath)
	require.NoError(t, err)
	require.NotContains(t, string(outputBytes), secret)
	require.Contains(t, string(outputBytes), "redacted:ATTELER_SECRET_TOKEN")
}

func TestRunBash_AllowedCredentialEnvGlobPropagatesButAuditRedacts(t *testing.T) {
	t.Parallel()

	auditDir := filepath.Join(t.TempDir(), "audit")
	envValue := strings.Repeat("x", 16)

	result, err := RunBash(context.Background(), Options{
		Command: `printf "%s" "$ATTELER_SECRET_TOKEN"`,
		Env:     map[string]string{"ATTELER_SECRET_TOKEN": envValue},
		Policy:  &Policy{AllowCredentialEnv: []string{"ATTELER_*_TOKEN"}},
		Audit:   AuditContext{AuditDir: auditDir},
	})

	require.NoError(t, err)
	require.Equal(t, envValue, result.Stdout)

	ledgerBytes, err := os.ReadFile(filepath.Join(auditDir, ledgerFileName))
	require.NoError(t, err)
	require.NotContains(t, string(ledgerBytes), envValue)

	records := readAuditRecords(t, auditDir)
	finish := records[len(records)-1]
	outputBytes, err := os.ReadFile(finish.OutputPath)
	require.NoError(t, err)
	require.NotContains(t, string(outputBytes), envValue)
	require.Contains(t, string(outputBytes), "redacted:ATTELER_SECRET_TOKEN")
}

func TestCommandContext_FullAmbientStillStripsCredentialEnvUnlessAllowed(t *testing.T) {
	auditDir := filepath.Join(t.TempDir(), "audit")
	envValue := strings.Repeat("z", 16)
	t.Setenv("ATTELER_SECRET_TOKEN", envValue)

	result, err := RunCommand(context.Background(), CommandOptions{
		Program: "bash",
		Args:    []string{"--noprofile", "--norc", "-lc", `printf "%s" "${ATTELER_SECRET_TOKEN-unset}"`},
		EnvMode: EnvModeFullAmbient,
		Audit:   AuditContext{AuditDir: auditDir},
	})

	require.NoError(t, err)
	require.Equal(t, "unset", result.Stdout)

	ledgerBytes, err := os.ReadFile(filepath.Join(auditDir, ledgerFileName))
	require.NoError(t, err)
	require.NotContains(t, string(ledgerBytes), envValue)
	require.Contains(t, string(ledgerBytes), "ATTELER_SECRET_TOKEN")
	require.Contains(t, string(ledgerBytes), "redacted")
}

func TestCommandContext_FullAmbientAllowedCredentialEnvPropagatesButAuditRedacts(t *testing.T) {
	auditDir := filepath.Join(t.TempDir(), "audit")
	envValue := strings.Repeat("q", 16)
	t.Setenv("ATTELER_SECRET_TOKEN", envValue)

	result, err := RunCommand(context.Background(), CommandOptions{
		Program: "bash",
		Args:    []string{"--noprofile", "--norc", "-lc", `printf "%s" "$ATTELER_SECRET_TOKEN"`},
		EnvMode: EnvModeFullAmbient,
		Policy:  &Policy{AllowCredentialEnv: []string{"ATTELER_SECRET_TOKEN"}},
		Audit:   AuditContext{AuditDir: auditDir},
	})

	require.NoError(t, err)
	require.Equal(t, envValue, result.Stdout)

	ledgerBytes, err := os.ReadFile(filepath.Join(auditDir, ledgerFileName))
	require.NoError(t, err)
	require.NotContains(t, string(ledgerBytes), envValue)

	records := readAuditRecords(t, auditDir)
	finish := records[len(records)-1]
	outputBytes, err := os.ReadFile(finish.OutputPath)
	require.NoError(t, err)
	require.NotContains(t, string(outputBytes), envValue)
	require.Contains(t, string(outputBytes), "redacted:ATTELER_SECRET_TOKEN")
}

func TestRunBash_AuditRecordsExitStatus(t *testing.T) {
	t.Parallel()

	auditDir := filepath.Join(t.TempDir(), "audit")

	_, err := RunBash(context.Background(), Options{
		Command: `exit 7`,
		Audit:   AuditContext{AuditDir: auditDir},
	})

	require.Error(t, err)
	records := readAuditRecords(t, auditDir)
	finish := records[len(records)-1]
	require.NotNil(t, finish.ExitStatus)
	require.Equal(t, 7, *finish.ExitStatus)
}

func TestRunInteractive_RecordsNotCapturedAudit(t *testing.T) {
	t.Parallel()

	auditDir := filepath.Join(t.TempDir(), "audit")

	_, err := RunInteractive(context.Background(), Options{
		Command: "true",
		Audit: AuditContext{
			Caller:   "test-interactive",
			AuditDir: auditDir,
		},
	})

	require.NoError(t, err)
	records := readAuditRecords(t, auditDir)
	finish := records[len(records)-1]
	require.Equal(t, "finish", finish.Phase)
	require.Equal(t, string(ModeInteractive), finish.Mode)
	require.Equal(t, "not_captured", finish.OutputCapture)
	require.Contains(t, finish.OutputNote, "not captured")
}

func TestRunBash_RecordsFinishWhenOutputCaptureFails(t *testing.T) {
	t.Parallel()

	auditDir := filepath.Join(t.TempDir(), "audit")
	require.NoError(t, os.MkdirAll(auditDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(auditDir, "outputs"), []byte("not a directory"), 0o600))

	result, err := RunBash(context.Background(), Options{
		Command: "printf captured",
		Audit:   AuditContext{AuditDir: auditDir},
	})

	require.Error(t, err)
	require.Contains(t, err.Error(), "create audit output directory")
	require.Equal(t, "captured", result.Stdout)

	records := readAuditRecords(t, auditDir)
	require.Len(t, records, 2)

	finish := records[1]
	require.Equal(t, "finish", finish.Phase)
	require.Equal(t, "allowed", finish.Decision)
	require.Equal(t, "not_captured", finish.OutputCapture)
	require.Empty(t, finish.OutputPath)
	require.Contains(t, finish.OutputNote, "redacted output capture failed")
	require.NotNil(t, finish.ExitStatus)
	require.Equal(t, 0, *finish.ExitStatus)
}

func readAuditRecords(t *testing.T, auditDir string) []AuditRecord {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(auditDir, ledgerFileName))
	require.NoError(t, err)

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")

	records := make([]AuditRecord, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}

		var record AuditRecord
		require.NoError(t, json.Unmarshal([]byte(line), &record))
		records = append(records, record)
	}

	return records
}

func testAuditContext(t *testing.T) AuditContext {
	t.Helper()

	return AuditContext{AuditDir: filepath.Join(t.TempDir(), "audit")}
}

func TestCommandContext_RedactsSensitiveArgsInDeniedAudit(t *testing.T) {
	t.Parallel()

	auditDir := filepath.Join(t.TempDir(), "audit")
	secret := "secret-command-arg"

	_, _, err := CommandContext(context.Background(), CommandOptions{
		Program:      "printf",
		Args:         []string{secret},
		SecretValues: []string{secret},
		Policy:       &Policy{DenyCommands: []string{"printf"}},
		Audit:        AuditContext{Caller: "test-sensitive-args", AuditDir: auditDir},
	})

	require.Error(t, err)
	ledgerBytes, err := os.ReadFile(filepath.Join(auditDir, ledgerFileName))
	require.NoError(t, err)
	require.NotContains(t, string(ledgerBytes), secret)
	require.Contains(t, string(ledgerBytes), "redacted:command_arg")
}

func TestRunBash_RedactsAuthorizationHeaderInDeniedAudit(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	auditDir := filepath.Join(tmp, "audit")
	secret := strings.Repeat("b", 16)

	_, err := RunBash(context.Background(), Options{
		Command: `curl -H "Authorization: Bearer ` + secret + `" https://example.invalid > header-secret-started`,
		Dir:     tmp,
		Policy:  &Policy{DenyNetwork: true},
		Audit:   AuditContext{AuditDir: auditDir},
	})

	require.Error(t, err)
	require.ErrorAs(t, err, new(*PolicyError))
	require.NoFileExists(t, filepath.Join(tmp, "header-secret-started"))

	ledgerBytes, err := os.ReadFile(filepath.Join(auditDir, ledgerFileName))
	require.NoError(t, err)
	require.NotContains(t, string(ledgerBytes), secret)

	records := readAuditRecords(t, auditDir)
	require.Len(t, records, 1)
	require.Contains(t, records[0].Command, "Authorization: Bearer <redacted:authorization>")
}

func TestRunBash_RedactsQuotedTokenArgumentInDeniedAudit(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	auditDir := filepath.Join(tmp, "audit")
	secret := strings.Repeat("q", 16)

	_, err := RunBash(context.Background(), Options{
		Command: `curl --token "` + secret + `" https://example.invalid > quoted-token-started`,
		Dir:     tmp,
		Policy:  &Policy{DenyNetwork: true},
		Audit:   AuditContext{AuditDir: auditDir},
	})

	require.Error(t, err)
	require.ErrorAs(t, err, new(*PolicyError))
	require.NoFileExists(t, filepath.Join(tmp, "quoted-token-started"))

	ledgerBytes, err := os.ReadFile(filepath.Join(auditDir, ledgerFileName))
	require.NoError(t, err)
	require.NotContains(t, string(ledgerBytes), secret)

	records := readAuditRecords(t, auditDir)
	require.Len(t, records, 1)
	require.Contains(t, records[0].Command, `--token "<redacted:command_arg>"`)
}

func TestRunBash_RedactsCommonSecretTextInAuditOutput(t *testing.T) {
	t.Parallel()

	auditDir := filepath.Join(t.TempDir(), "audit")
	secret := strings.Repeat("s", 16)

	result, err := RunBash(context.Background(), Options{
		Command: `printf '%s' 'token=` + secret + `'`,
		Audit:   AuditContext{AuditDir: auditDir},
	})

	require.NoError(t, err)
	require.Contains(t, result.Stdout, secret)

	records := readAuditRecords(t, auditDir)
	finish := records[len(records)-1]
	outputBytes, err := os.ReadFile(finish.OutputPath)
	require.NoError(t, err)
	require.NotContains(t, string(outputBytes), secret)

	var output struct {
		Stdout string `json:"stdout"`
	}
	require.NoError(t, json.Unmarshal(outputBytes, &output))
	require.Equal(t, "token=<redacted:command_arg>", output.Stdout)
}

func TestRunBash_RedactsQuotedCommonSecretTextInAuditOutput(t *testing.T) {
	t.Parallel()

	auditDir := filepath.Join(t.TempDir(), "audit")
	secret := strings.Repeat("w", 16)

	result, err := RunBash(context.Background(), Options{
		Command: `printf '%s' "password: '` + secret + `'"`,
		Audit:   AuditContext{AuditDir: auditDir},
	})

	require.NoError(t, err)
	require.Contains(t, result.Stdout, secret)

	records := readAuditRecords(t, auditDir)
	finish := records[len(records)-1]
	outputBytes, err := os.ReadFile(finish.OutputPath)
	require.NoError(t, err)
	require.NotContains(t, string(outputBytes), secret)

	var output struct {
		Stdout string `json:"stdout"`
	}
	require.NoError(t, json.Unmarshal(outputBytes, &output))
	require.Equal(t, "password: '<redacted:command_arg>'", output.Stdout)
}

func TestRunBash_DeniesInlineCredentialAssignmentBeforeStart(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	auditDir := filepath.Join(tmp, "audit")
	target := filepath.Join(tmp, "inline-credential-started")
	secret := strings.Repeat("i", 16)

	_, err := RunBash(context.Background(), Options{
		Command: "ATTELER_SECRET_TOKEN=" + secret + " touch " + shellQuote(target),
		Dir:     tmp,
		Audit:   AuditContext{AuditDir: auditDir},
	})

	require.Error(t, err)
	require.ErrorAs(t, err, new(*PolicyError))
	require.NoFileExists(t, target)

	ledgerBytes, err := os.ReadFile(filepath.Join(auditDir, ledgerFileName))
	require.NoError(t, err)
	require.NotContains(t, string(ledgerBytes), secret)

	records := readAuditRecords(t, auditDir)
	require.Len(t, records, 1)
	require.Equal(t, "env.deny", records[0].DecisionRule)
	require.Contains(t, records[0].DecisionReason, "ATTELER_SECRET_TOKEN")
}

func TestRunBash_AllowsInlineCredentialAssignmentWhenExplicitlyAllowed(t *testing.T) {
	t.Parallel()

	auditDir := filepath.Join(t.TempDir(), "audit")
	secret := strings.Repeat("j", 16)

	result, err := RunBash(context.Background(), Options{
		Command: "ATTELER_SECRET_TOKEN=" + secret + ` sh -c 'printf "%s" "$ATTELER_SECRET_TOKEN"'`,
		Policy:  &Policy{AllowCredentialEnv: []string{"ATTELER_SECRET_TOKEN"}},
		Audit:   AuditContext{AuditDir: auditDir},
	})

	require.NoError(t, err)
	require.Equal(t, secret, result.Stdout)

	ledgerBytes, err := os.ReadFile(filepath.Join(auditDir, ledgerFileName))
	require.NoError(t, err)
	require.NotContains(t, string(ledgerBytes), secret)

	records := readAuditRecords(t, auditDir)
	finish := records[len(records)-1]
	outputBytes, err := os.ReadFile(finish.OutputPath)
	require.NoError(t, err)
	require.NotContains(t, string(outputBytes), secret)
	require.Contains(t, string(outputBytes), "redacted:ATTELER_SECRET_TOKEN")
}

func TestCommandContext_DeniesEnvWrapperCredentialAssignmentBeforeStart(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	auditDir := filepath.Join(tmp, "audit")
	target := filepath.Join(tmp, "env-wrapper-credential-started")
	secret := strings.Repeat("k", 16)

	_, _, err := CommandContext(context.Background(), CommandOptions{
		Program: "env",
		Args:    []string{"ATTELER_SECRET_TOKEN=" + secret, "touch", target},
		Audit:   AuditContext{AuditDir: auditDir},
	})

	require.Error(t, err)
	require.ErrorAs(t, err, new(*PolicyError))
	require.NoFileExists(t, target)

	ledgerBytes, err := os.ReadFile(filepath.Join(auditDir, ledgerFileName))
	require.NoError(t, err)
	require.NotContains(t, string(ledgerBytes), secret)

	records := readAuditRecords(t, auditDir)
	require.Len(t, records, 1)
	require.Equal(t, "env.deny", records[0].DecisionRule)
}

func TestRunBash_CredentialAssignmentArgumentAfterCommandWordIsNotEnvironment(t *testing.T) {
	t.Parallel()

	auditDir := filepath.Join(t.TempDir(), "audit")

	result, err := RunBash(context.Background(), Options{
		Command: `printf '%s' ATTELER_SECRET_TOKEN=literal`,
		Audit:   AuditContext{AuditDir: auditDir},
	})

	require.NoError(t, err)
	require.Equal(t, "ATTELER_SECRET_TOKEN=literal", result.Stdout)
}

func TestRunBash_RedactsInlineCredentialAssignmentInDeniedAudit(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	auditDir := filepath.Join(tmp, "audit")
	secret := "inline-secret-value"

	_, err := RunBash(context.Background(), Options{
		Command: "ATTELER_SECRET_TOKEN=" + secret + " touch inline-secret-started",
		Dir:     tmp,
		Policy:  &Policy{DenyCommands: []string{"touch"}},
		Audit:   AuditContext{AuditDir: auditDir},
	})

	require.Error(t, err)
	require.ErrorAs(t, err, new(*PolicyError))
	require.NoFileExists(t, filepath.Join(tmp, "inline-secret-started"))

	ledgerBytes, err := os.ReadFile(filepath.Join(auditDir, ledgerFileName))
	require.NoError(t, err)
	require.NotContains(t, string(ledgerBytes), secret)
	require.Contains(t, string(ledgerBytes), "redacted:ATTELER_SECRET_TOKEN")
}

func TestRunBash_PolicyUsesUnredactedCommandForDenial(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	auditDir := filepath.Join(tmp, "audit")

	_, err := RunBash(context.Background(), Options{
		Command: "ATTELER_SECRET_TOKEN=touch touch denied-unredacted-policy",
		Dir:     tmp,
		Policy:  &Policy{DenyCommands: []string{"touch"}},
		Audit:   AuditContext{AuditDir: auditDir},
	})

	require.Error(t, err)
	require.ErrorAs(t, err, new(*PolicyError))
	require.NoFileExists(t, filepath.Join(tmp, "denied-unredacted-policy"))

	ledgerBytes, err := os.ReadFile(filepath.Join(auditDir, ledgerFileName))
	require.NoError(t, err)
	require.NotContains(t, string(ledgerBytes), "ATTELER_SECRET_TOKEN=touch touch")
	require.Contains(t, string(ledgerBytes), "redacted:ATTELER_SECRET_TOKEN")

	records := readAuditRecords(t, auditDir)
	require.Len(t, records, 1)
	require.Equal(t, "command.deny", records[0].DecisionRule)
}

func TestBuildCommandEnvironment_SanitizedAmbientKeepsNonSecretVarsAndStripsSecrets(t *testing.T) {
	tests := []struct {
		name     string
		envName  string
		stripped bool
	}{
		{name: "ssh agent socket survives", envName: "SSH_AUTH_SOCK", stripped: false},
		{name: "gpg agent info survives", envName: "GPG_AGENT_INFO", stripped: false},
		{name: "forwarded agent socket survives", envName: "FORWARDED_SSH_AUTH_SOCK", stripped: false},
		{name: "lesskey survives", envName: "LESSKEY", stripped: false},
		{name: "aws secret access key is stripped", envName: "AWS_SECRET_ACCESS_KEY", stripped: true},
		{name: "openai api key is stripped", envName: "OPENAI_API_KEY", stripped: true},
		{name: "github token is stripped", envName: "GITHUB_TOKEN", stripped: true},
		{name: "custom auth token is stripped", envName: "MY_AUTH_TOKEN", stripped: true},
		{name: "underscore key suffix is stripped", envName: "DEPLOY_KEY", stripped: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := "value-for-" + test.envName
			t.Setenv(test.envName, value)

			env, diff, _ := buildCommandEnvironment(CommandOptions{EnvMode: EnvModeSanitizedAmbient}, DefaultPolicy())

			child := envMap(env)
			redacted := slices.ContainsFunc(diff, func(change EnvChange) bool {
				return change.Name == test.envName && change.Action == "redacted"
			})

			if test.stripped {
				require.NotContains(t, child, test.envName)
				require.True(t, redacted, "expected %s to be audited as redacted", test.envName)

				return
			}

			require.Equal(t, value, child[test.envName])
			require.False(t, redacted, "expected %s to survive sanitization", test.envName)
		})
	}
}

func TestCredentialEnv_AllowsKnownNonSecretsAndKeepsSecretPatterns(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		envName    string
		credential bool
	}{
		{name: "ssh agent socket", envName: "SSH_AUTH_SOCK", credential: false},
		{name: "gpg agent info", envName: "GPG_AGENT_INFO", credential: false},
		{name: "generic agent socket suffix", envName: "REMOTE_GIT_AUTH_SOCK", credential: false},
		{name: "lesskey", envName: "LESSKEY", credential: false},
		{name: "bare key word", envName: "MONKEY", credential: false},
		{name: "api key", envName: "OPENAI_API_KEY", credential: true},
		{name: "access key", envName: "AWS_SECRET_ACCESS_KEY", credential: true},
		{name: "private key", envName: "TLS_PRIVATE_KEY", credential: true},
		{name: "underscore key suffix", envName: "SIGNING_KEY", credential: true},
		{name: "auth infix", envName: "MY_AUTH_TOKEN", credential: true},
		{name: "token suffix", envName: "GITHUB_TOKEN", credential: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(t, test.credential, credentialEnv(test.envName))
		})
	}
}
