package llm

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProviders_RequireActiveContextForBlockingMethods(t *testing.T) {
	t.Parallel()

	providers := []Provider{
		&AnthropicProvider{client: &http.Client{}, apiKey: "key", baseURL: "http://127.0.0.1:1"},
		&OpenAIProvider{client: &http.Client{}, apiKey: "key", baseURL: "http://127.0.0.1:1"},
		&OllamaProvider{client: &http.Client{}, baseURL: "http://127.0.0.1:1"},
		&ClaudeCodeProvider{
			client:  &http.Client{},
			auth:    &claudeCodeAuth{accessToken: "access"},
			baseURL: "http://127.0.0.1:1",
			models:  []string{"test-model"},
		},
		&CodexProvider{
			client:  &http.Client{},
			auth:    &codexChatGPTAuth{accessToken: "access"},
			baseURL: "http://127.0.0.1:1",
			models:  []string{"test-model"},
		},
	}

	for _, provider := range providers {
		t.Run(provider.Name(), func(t *testing.T) {
			t.Parallel()

			params := CompleteParams{
				Model:    "test-model",
				Messages: []Message{{Role: RoleUser, Content: "hi"}},
			}

			_, err := provider.Complete(nil, params) //nolint:staticcheck // Verify nil contexts are rejected instead of panicking.
			require.Error(t, err)
			require.ErrorIs(t, err, ErrContextRequired)

			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			_, err = provider.Complete(ctx, params)
			require.Error(t, err)
			require.ErrorIs(t, err, context.Canceled)

			if streamProvider, ok := provider.(StreamProvider); ok {
				_, err = streamProvider.CompleteStream(nil, params) //nolint:staticcheck // Verify nil contexts are rejected instead of panicking.
				require.Error(t, err)
				require.ErrorIs(t, err, ErrContextRequired)

				_, err = streamProvider.CompleteStream(ctx, params)
				require.Error(t, err)
				require.ErrorIs(t, err, context.Canceled)
			}

			_, err = provider.FetchModels(nil) //nolint:staticcheck // Verify nil contexts are rejected instead of panicking.
			require.Error(t, err)
			require.ErrorIs(t, err, ErrContextRequired)

			err = provider.HealthCheck(nil) //nolint:staticcheck // Verify nil contexts are rejected instead of panicking.
			require.Error(t, err)
			require.ErrorIs(t, err, ErrContextRequired)

			err = provider.HealthCheck(ctx)
			require.Error(t, err)
			assert.ErrorIs(t, err, context.Canceled)
		})
	}
}
