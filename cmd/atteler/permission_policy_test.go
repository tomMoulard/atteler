package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/permission"
	"github.com/tommoulard/atteler/pkg/session"
)

func TestPermissionPolicyFromOptions_ReadOnlyWithOverrides(t *testing.T) {
	t.Parallel()

	opts := cliOptions{permissionMode: "read-only"}
	require.NoError(t, opts.allowOperations.Set("network"))
	require.NoError(t, opts.denyOperations.Set("execute"))

	policy, err := permissionPolicyFromOptions(opts)
	require.NoError(t, err)
	assert.Equal(t, permission.ModeAllow, policy.ModeFor(permission.OperationRead))
	assert.Equal(t, permission.ModeAllow, policy.ModeFor(permission.OperationNetwork))
	assert.Equal(t, permission.ModeDeny, policy.ModeFor(permission.OperationExecute))
	assert.Equal(t, permission.ModeDeny, policy.ModeFor(permission.OperationWrite))

	decision := permission.Evaluate(t.Context(), policy, permission.Request{
		Action:     "printf inspected",
		Operations: permission.CommandOperations("printf", []string{"inspected"}, "", ".", "test"),
	})
	require.False(t, decision.Allowed)
	assert.Equal(t, permission.OperationExecute, decision.Kind)
}

func TestPermissionPolicyFromOptions_CIHeadlessDenyExternalDestructiveWork(t *testing.T) {
	t.Parallel()

	opts := cliOptions{
		permissionMode: "allow",
		headless:       true,
	}
	require.NoError(t, opts.denyOperations.Set("network,credential_access,git_mutation,merge_delete"))

	policy, err := permissionPolicyFromOptions(opts)
	require.NoError(t, err)

	tests := []struct {
		wantKind permission.OperationKind
		name     string
		ops      []permission.Operation
		allowed  bool
	}{
		{
			name:    "local session writes remain available",
			ops:     []permission.Operation{{Kind: permission.OperationWrite}},
			allowed: true,
		},
		{
			name:    "local command execution remains available",
			ops:     []permission.Operation{{Kind: permission.OperationExecute}},
			allowed: true,
		},
		{
			name:     "network is denied",
			ops:      permission.CommandOperations("bash", []string{"-lc", "curl https://example.invalid"}, "curl https://example.invalid", "", "test"),
			allowed:  false,
			wantKind: permission.OperationNetwork,
		},
		{
			name:     "credential access is denied",
			ops:      []permission.Operation{{Kind: permission.OperationCredentialAccess}},
			allowed:  false,
			wantKind: permission.OperationCredentialAccess,
		},
		{
			name:     "git mutation is denied",
			ops:      []permission.Operation{{Kind: permission.OperationGitMutation}},
			allowed:  false,
			wantKind: permission.OperationGitMutation,
		},
		{
			name:     "merge delete is denied",
			ops:      permission.CommandOperations("bash", []string{"-lc", "git branch -D stale"}, "git branch -D stale", "", "test"),
			allowed:  false,
			wantKind: permission.OperationMergeDelete,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			decision := permission.Evaluate(t.Context(), policy, permission.Request{
				Action:     tt.name,
				Operations: tt.ops,
			})

			assert.Equal(t, tt.allowed, decision.Allowed)

			if !tt.allowed {
				assert.Equal(t, tt.wantKind, decision.Kind)
				assert.Equal(t, "permission."+string(tt.wantKind)+".deny", decision.Rule)
			}
		})
	}
}

func TestPermissionPolicyCommandArgs_RoundTripsSubagentPolicy(t *testing.T) {
	t.Parallel()

	opts := cliOptions{permissionMode: "read-only"}
	require.NoError(t, opts.allowOperations.Set("network"))
	require.NoError(t, opts.denyOperations.Set("execute"))

	policy, err := permissionPolicyFromOptions(opts)
	require.NoError(t, err)

	args := permissionPolicyCommandArgs(policy)
	assert.Equal(t, []string{
		"--permission-mode", "read-only",
		"--allow-operation", "network",
		"--deny-operation", "execute",
	}, args)

	var childOpts cliOptions

	for i := 0; i < len(args); i += 2 {
		switch args[i] {
		case "--permission-mode":
			childOpts.permissionMode = args[i+1]
		case "--allow-operation":
			require.NoError(t, childOpts.allowOperations.Set(args[i+1]))
		case "--deny-operation":
			require.NoError(t, childOpts.denyOperations.Set(args[i+1]))
		}
	}

	childPolicy, err := permissionPolicyFromOptions(childOpts)
	require.NoError(t, err)
	assert.Equal(t, policy.Summary(), childPolicy.Summary())
	assert.Equal(t, policy.AllowReadExecution, childPolicy.AllowReadExecution)
}

func TestSubagentCommandArgs_IncludesPermissionPolicy(t *testing.T) {
	t.Parallel()

	opts := cliOptions{permissionMode: "allow"}
	require.NoError(t, opts.denyOperations.Set("network"))

	policy, err := permissionPolicyFromOptions(opts)
	require.NoError(t, err)

	store := session.NewStore(t.TempDir())
	got := subagentCommandArgs(appState{
		selectedModel:    "gpt-test",
		sessionStore:     store,
		permissionPolicy: policy,
	})

	assert.Equal(t, []string{
		"--model", "gpt-test",
		"--session-dir", store.Dir(),
		"--permission-mode", "allow",
		"--deny-operation", "network",
	}, got)
}

func TestPermissionPolicyFromOptions_RejectsUnknownOperation(t *testing.T) {
	t.Parallel()

	opts := cliOptions{}
	require.NoError(t, opts.denyOperations.Set("not-real"))

	_, err := permissionPolicyFromOptions(opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse --deny-operation")
}

func TestContextWithPermissionPolicyForOptions_HeadlessAskFailsClosed(t *testing.T) {
	t.Parallel()

	opts := cliOptions{permissionMode: "ask", headless: true}
	policy, err := permissionPolicyFromOptions(opts)
	require.NoError(t, err)

	ctx := contextWithPermissionPolicyForOptions(context.Background(), opts, policy)
	decision := permission.Evaluate(ctx, nil, permission.Request{
		Action: "touch generated.txt",
		Operations: []permission.Operation{
			{Kind: permission.OperationExecute},
			{Kind: permission.OperationWrite},
		},
	})

	require.False(t, decision.Allowed)
	assert.True(t, decision.NeedsApproval)
	assert.Equal(t, permission.OperationWrite, decision.Kind)
	assert.Contains(t, decision.Reason, "no interactive confirmer")
}

func TestContextWithPermissionAuditMetadataAddsSessionFields(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	sessionState := session.New("gpt-test", nil)
	ctx := contextWithPermissionAuditMetadata(context.Background(), store, sessionState, "agent-test", "model-test")

	decision := permission.Evaluate(ctx, nil, permission.Request{
		Action:     "cat README.md",
		Operations: []permission.Operation{{Kind: permission.OperationRead}},
	})

	require.True(t, decision.Allowed)
	require.Len(t, decision.Operations, 1)
	assert.Equal(t, sessionState.ID, decision.Operations[0].Metadata["session_id"])
	assert.Equal(t, store.Path(sessionState.ID), decision.Operations[0].Metadata["session_path"])
	assert.Equal(t, "agent-test", decision.Operations[0].Metadata["agent"])
	assert.Equal(t, "model-test", decision.Operations[0].Metadata["model"])

	auditPath := filepath.Join(permissionAuditDirForSessionPath(store.Path(sessionState.ID), sessionState.ID), "side_effects.jsonl")
	_, err := os.Stat(auditPath)
	require.NoError(t, err)
}
