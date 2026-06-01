package permission

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClassifyCommand_CoversCentralOperationKinds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		program string
		args    []string
		command string
		want    []OperationKind
	}{
		{name: "read inspection", command: "git status --short", want: []OperationKind{OperationRead, OperationExecute}},
		{name: "write shell", command: "touch generated.txt", want: []OperationKind{OperationWrite, OperationExecute}},
		{name: "network", command: "curl https://example.com", want: []OperationKind{OperationExecute, OperationNetwork}},
		{name: "credential assignment", command: "API_TOKEN=secret go test ./...", want: []OperationKind{OperationExecute, OperationCredentialAccess}},
		{name: "git mutation", program: "git", args: []string{"commit", "-m", "msg"}, want: []OperationKind{OperationWrite, OperationExecute, OperationGitMutation}},
		{name: "merge delete", program: "git", args: []string{"branch", "-D", "atteler/session"}, want: []OperationKind{OperationWrite, OperationExecute, OperationGitMutation, OperationMergeDelete}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := ClassifyCommand(tt.program, tt.args, tt.command)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestEvaluate_DenyAskAndAllow(t *testing.T) {
	t.Parallel()

	policy := DefaultPolicy()
	policy.SetMode(OperationWrite, ModeDeny)
	policy.SetMode(OperationNetwork, ModeAsk)

	denied := Evaluate(context.Background(), &policy, Request{
		Action:     "touch generated.txt",
		Operations: []Operation{{Kind: OperationExecute}, {Kind: OperationWrite}},
	})
	require.False(t, denied.Allowed)
	assert.Equal(t, OperationWrite, denied.Kind)
	assert.Equal(t, "permission.write.deny", denied.Rule)

	askNoConfirmer := Evaluate(context.Background(), &policy, Request{
		Action:     "curl https://example.com",
		Operations: []Operation{{Kind: OperationExecute}, {Kind: OperationNetwork}},
	})
	require.False(t, askNoConfirmer.Allowed)
	assert.True(t, askNoConfirmer.NeedsApproval)
	assert.Contains(t, askNoConfirmer.Reason, "no interactive confirmer")

	ctx := ContextWithConfirmer(context.Background(), func(context.Context, Request, Decision) bool {
		return true
	})
	asked := Evaluate(ctx, &policy, Request{
		Action:     "curl https://example.com",
		Operations: []Operation{{Kind: OperationExecute}, {Kind: OperationNetwork}},
	})
	require.True(t, asked.Allowed)
	assert.True(t, asked.Confirmed)
	assert.Equal(t, "permission.network.allow", asked.Rule)
}

func TestReadOnlyPolicy_AllowsInspectionAndDeniesWrites(t *testing.T) {
	t.Parallel()

	policy := ReadOnlyPolicy()

	readDecision := Evaluate(context.Background(), &policy, Request{
		Action:     "git status --short",
		Operations: CommandOperations("git", []string{"status", "--short"}, "", ".", "test"),
	})
	require.True(t, readDecision.Allowed)

	writeDecision := Evaluate(context.Background(), &policy, Request{
		Action:     "touch generated.txt",
		Operations: CommandOperations("bash", []string{"-lc", "touch generated.txt"}, "touch generated.txt", ".", "test"),
	})
	require.False(t, writeDecision.Allowed)
	assert.Equal(t, OperationWrite, writeDecision.Kind)

	unknownExecuteDecision := Evaluate(context.Background(), &policy, Request{
		Action:     "custom-tool --version",
		Operations: CommandOperations("custom-tool", []string{"--version"}, "", ".", "test"),
	})
	require.False(t, unknownExecuteDecision.Allowed)
	assert.Equal(t, OperationExecute, unknownExecuteDecision.Kind)

	pluginExecuteDecision := Evaluate(context.Background(), &policy, Request{
		Action:     "plugin entrypoint",
		Operations: []Operation{{Kind: OperationRead}, {Kind: OperationExecute}},
	})
	require.False(t, pluginExecuteDecision.Allowed)
	assert.Equal(t, OperationExecute, pluginExecuteDecision.Kind)
}

func TestParseOperationKinds_AcceptsCommaSeparatedValues(t *testing.T) {
	t.Parallel()

	got, err := ParseOperationKinds([]string{"read,write", "credential-access"})
	require.NoError(t, err)
	assert.Equal(t, []OperationKind{OperationRead, OperationWrite, OperationCredentialAccess}, got)

	_, err = ParseOperationKinds([]string{"unknown"})
	require.Error(t, err)
}
