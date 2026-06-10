package llm

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/permission"
)

func TestSecurityCommandPermissionOperationsClassifiesCredentialRead(t *testing.T) {
	t.Parallel()

	ops := securityCommandPermissionOperations([]string{"find-generic-password", "-s", keychainService, "-w"})

	require.Len(t, ops, 1)
	assert.Equal(t, permission.OperationCredentialAccess, ops[0].Kind)
	assert.Equal(t, "security find-generic-password", ops[0].Action)
	assert.Equal(t, keychainService, ops[0].Target)
	assert.Equal(t, "atteler.keychain.security", ops[0].Source)
}

func TestSecurityCommandPermissionOperationsClassifiesCredentialWrite(t *testing.T) {
	t.Parallel()

	ops := securityCommandPermissionOperations(
		[]string{"add-generic-password", "-U", "-s", keychainService, "-w", "secret-value"},
	)

	require.Len(t, ops, 2)
	assert.Equal(t, permission.OperationCredentialAccess, ops[0].Kind)
	assert.Equal(t, permission.OperationWrite, ops[1].Kind)
	assert.Equal(t, "security add-generic-password", ops[0].Action)
	assert.Equal(t, "security add-generic-password", ops[1].Action)
}

func TestRunSecurityCommandPermissionPolicyDeniesCredentialAccessBeforeExecution(t *testing.T) {
	t.Parallel()

	policy := permission.DefaultPolicy()
	policy.SetMode(permission.OperationCredentialAccess, permission.ModeDeny)
	ctx := permission.ContextWithPolicy(context.Background(), &policy)
	ctx = permission.ContextWithAuditDir(ctx, t.TempDir())

	_, err := runSecurityCommand(
		ctx,
		[]string{"find-generic-password", "-s", keychainService, "-w"},
		nil,
	)

	require.Error(t, err)
	require.True(t, permission.ErrDenied(err))
	assert.Contains(t, err.Error(), "permission.credential_access.deny")
}

func TestRunSecurityCommandPermissionPolicyDeniesCredentialWriteBeforeExecution(t *testing.T) {
	t.Parallel()

	policy := permission.DefaultPolicy()
	policy.SetMode(permission.OperationWrite, permission.ModeDeny)
	ctx := permission.ContextWithPolicy(context.Background(), &policy)
	ctx = permission.ContextWithAuditDir(ctx, t.TempDir())

	_, err := runSecurityCommand(
		ctx,
		[]string{"add-generic-password", "-U", "-s", keychainService, "-w", "secret-value"},
		[]string{"secret-value"},
	)

	require.Error(t, err)
	require.True(t, permission.ErrDenied(err))
	assert.Contains(t, err.Error(), "permission.write.deny")
}
