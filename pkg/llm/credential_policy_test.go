package llm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/permission"
)

func TestCredentialSourcePolicyErrorRedactsSecretLocationAndAudit(t *testing.T) {
	t.Parallel()

	auditDir := t.TempDir()
	ctx := permission.ContextWithAuditDir(context.Background(), auditDir)

	secretPath := filepath.Join(t.TempDir(), "api_key=super-secret-token", "auth.json")
	// An explicit deny-all policy still rejects the borrowed Codex store even
	// though the default policy now borrows it; the denial path is what redacts.
	denyAll := ProviderConfig{CredentialPolicy: CredentialSourcePolicy{Configured: true, AllowedStoresSet: true}}
	err := authorizeCredentialSourcePolicy(ctx, denyAll, credentialSource{
		Provider:   providerCodex,
		Store:      CredentialStoreCodexAuthJSON,
		Location:   secretPath,
		Identifier: "acct-super-secret-token",
	}, credentialActionRead)
	require.Error(t, err)

	message := err.Error()
	assert.Contains(t, message, "api_key=[REDACTED]")
	assert.NotContains(t, message, "super-secret-token")

	audit, readErr := os.ReadFile(filepath.Join(auditDir, credentialAuditLedgerFileName))
	require.NoError(t, readErr)
	assert.Contains(t, string(audit), "api_key=[REDACTED]")
	assert.NotContains(t, string(audit), "super-secret-token")
	assert.Contains(t, string(audit), "sha256:")
	assert.NotContains(t, string(audit), "acct")
}

func TestCredentialAuditFailureReasonRedactsSecrets(t *testing.T) {
	t.Parallel()

	auditDir := t.TempDir()
	ctx := permission.ContextWithAuditDir(context.Background(), auditDir)
	source := credentialSource{
		Provider:      providerCodex,
		Store:         CredentialStoreCodexAuthJSON,
		Description:   "Codex ChatGPT auth.json",
		Location:      filepath.Join(t.TempDir(), "auth.json"),
		Identifier:    "acct-secret",
		BorrowedOAuth: true,
	}

	auditCredentialRefreshFailure(ctx, source, errors.New("refresh failed: refresh_token=refresh-secret Authorization: Bearer access-secret"))
	auditCredentialWriteBackFailure(ctx, source, errors.New("write failed: access_token=access-secret"))

	audit, readErr := os.ReadFile(filepath.Join(auditDir, credentialAuditLedgerFileName))
	require.NoError(t, readErr)

	body := string(audit)
	assert.Contains(t, body, credentialAuditEventRefresh)
	assert.Contains(t, body, credentialAuditEventWriteBack)
	assert.Contains(t, body, "refresh_token=[REDACTED]")
	assert.Contains(t, body, "Authorization: [REDACTED]")
	assert.Contains(t, body, "access_token=[REDACTED]")
	assert.Contains(t, body, "sha256:")
	assert.NotContains(t, body, "refresh-secret")
	assert.NotContains(t, body, "access-secret")
	assert.NotContains(t, body, "acct-secret")
}

func TestRedactCredentialPathErrorRedactsPathErrorLocation(t *testing.T) {
	t.Parallel()

	secretPath := filepath.Join(t.TempDir(), "api_key=super-secret-token", "auth.json")
	message := redactCredentialPathError(&os.PathError{Op: "open", Path: secretPath, Err: os.ErrNotExist})

	assert.Contains(t, message, "api_key=[REDACTED]")
	assert.NotContains(t, message, "super-secret-token")
}

func TestLoadCodexChatGPTAuthRedactsMissingCredentialPath(t *testing.T) {
	t.Parallel()

	codexHome := filepath.Join(t.TempDir(), "api_key=super-secret-token")
	_, err := loadCodexChatGPTAuthWithConfigContext(context.Background(), codexHome, ProviderConfig{
		CredentialPolicy: CredentialSourcePolicy{
			AllowedStores:      []string{CredentialStoreCodexAuthJSON},
			AllowBorrowedOAuth: true,
		},
	})
	require.Error(t, err)

	message := err.Error()
	assert.Contains(t, message, "api_key=[REDACTED]")
	assert.NotContains(t, message, "super-secret-token")
}

func TestConfiguredCredentialPolicyOverridesBorrowedTrustEnv(t *testing.T) {
	t.Setenv(envTrustBorrowedCredentials, "1")

	source := credentialSource{
		Provider:      providerAnthropic,
		Store:         CredentialStoreEnv,
		Description:   "CLAUDE_CODE_OAUTH_TOKEN",
		BorrowedOAuth: true,
	}

	require.NoError(t, authorizeCredentialSourcePolicy(t.Context(), ProviderConfig{}, source, credentialActionUse))

	err := authorizeCredentialSourcePolicy(t.Context(), ProviderConfig{
		CredentialPolicy: CredentialSourcePolicy{Configured: true},
	}, source, credentialActionUse)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "allow_borrowed_oauth")
}

func TestCredentialSourcePolicyExplicitEmptyListsDenyAll(t *testing.T) {
	t.Setenv(envTrustBorrowedCredentials, "1")

	source := credentialSource{
		Provider: providerOpenAI,
		Store:    CredentialStoreEnv,
	}

	err := authorizeCredentialSourcePolicy(t.Context(), ProviderConfig{
		CredentialPolicy: CredentialSourcePolicy{
			Configured:          true,
			AllowedProvidersSet: true,
			AllowedStoresSet:    true,
		},
	}, source, credentialActionUse)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "allowed_providers")

	err = authorizeCredentialSourcePolicy(t.Context(), ProviderConfig{
		CredentialPolicy: CredentialSourcePolicy{
			Configured:          true,
			AllowedProviders:    []string{providerOpenAI},
			AllowedProvidersSet: true,
			AllowedStoresSet:    true,
		},
	}, source, credentialActionUse)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "allowed_stores")
}

func TestCredentialSourcePolicyAllowedProvidersNormalizesHyphenatedNames(t *testing.T) {
	t.Parallel()

	source := credentialSource{
		Provider: providerClaudeCode,
		Store:    CredentialStoreClaudeCodeFile,
	}

	for _, allowedProvider := range []string{"claude-code", "claude_code"} {
		t.Run(allowedProvider, func(t *testing.T) {
			t.Parallel()

			err := authorizeCredentialSourcePolicy(t.Context(), ProviderConfig{
				CredentialPolicy: CredentialSourcePolicy{
					AllowedProviders:    []string{allowedProvider},
					AllowedProvidersSet: true,
					AllowedStores:       []string{CredentialStoreClaudeCodeFile},
					AllowedStoresSet:    true,
				},
			}, source, credentialActionRead)
			require.NoError(t, err)
		})
	}
}

func TestCredentialPolicySummaryPreservesExplicitEmptyLists(t *testing.T) {
	t.Parallel()

	summary := credentialPolicySummary(CredentialSourcePolicy{
		Configured:          true,
		AllowedProvidersSet: true,
		AllowedStoresSet:    true,
	})

	assert.Contains(t, summary, "allowed_providers=[]")
	assert.Contains(t, summary, "allowed_stores=[]")
	assert.NotContains(t, summary, "allowed_providers=*")
	assert.NotContains(t, summary, "allowed_stores=env")
}

func TestCredentialPolicySummaryDefaultsOnlyWhenListsUnset(t *testing.T) {
	t.Parallel()

	summary := credentialPolicySummary(CredentialSourcePolicy{})

	// The default borrows the known local stores out of the box.
	assert.Contains(t, summary, "allowed_providers=*")
	assert.Contains(t, summary, "allowed_stores="+strings.Join(defaultAllowedCredentialStores(), ","))
	assert.Contains(t, summary, "allow_borrowed_oauth=true")
	assert.Contains(t, summary, "allow_refresh=true")
	assert.Contains(t, summary, "allow_write_back=true")
}
