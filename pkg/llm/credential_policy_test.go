package llm

import (
	"context"
	"os"
	"path/filepath"
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
	err := authorizeCredentialSourcePolicy(ctx, ProviderConfig{}, credentialSource{
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

	assert.Contains(t, summary, "allowed_providers=*")
	assert.Contains(t, summary, "allowed_stores=env")
}
