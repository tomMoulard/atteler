package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/tommoulard/atteler/pkg/events"
	"github.com/tommoulard/atteler/pkg/permission"
)

const (
	claudeCodeAdapterVersion    = "claude-code-oauth-messages-v1"
	claudeCodeAdapterSource     = "Claude Code OAuth credential store claudeAiOauth; source credentials are owned by the claude CLI"
	claudeCodeAdapterCLIVersion = "Claude Code claudeAiOauth schema, OAuth refresh route, and beta header set as reviewed on 2026-05-22; " +
		"no public upstream semver contract"
	claudeCodeAdapterProtocol = "Anthropic Messages POST /v1/messages with Claude Code OAuth bearer auth and beta routing headers: " +
		anthropicOAuthBetas
	claudeCodeAdapterReviewedAt  = "2026-05-22"
	claudeCodeAdapterReviewAfter = "2026-08-22"
)

// ClaudeCodeProvider calls the Anthropic Messages API directly using the
// OAuth access token Claude Code stored at login. It auto-refreshes the
// access token on 401 and persists refreshed tokens back to whichever store
// they came from (macOS keychain or ~/.claude/.credentials.json), so atteler
// and the claude CLI keep a shared session.
type ClaudeCodeProvider struct {
	client  *http.Client
	auth    *claudeCodeAuth
	baseURL string
	models  []string
}

// NewClaudeCodeProvider is kept for source compatibility only.
//
// Deprecated: use NewClaudeCodeProviderContext so keychain and credential-file
// access inherits caller cancellation and deadlines.
func NewClaudeCodeProvider() (*ClaudeCodeProvider, error) {
	return nil, ErrContextRequired
}

// NewClaudeCodeProviderContext creates a provider using ctx for credential
// discovery (keychain probe / file reads).
func NewClaudeCodeProviderContext(ctx context.Context) (*ClaudeCodeProvider, error) {
	return NewClaudeCodeProviderWithConfigContext(ctx, ProviderConfig{})
}

// NewClaudeCodeProviderWithConfigContext creates a provider using ctx for
// credential discovery and applies provider-specific configuration.
func NewClaudeCodeProviderWithConfigContext(ctx context.Context, cfg ProviderConfig) (*ClaudeCodeProvider, error) {
	if err := requireCredentialContext(ctx); err != nil {
		return nil, err
	}

	if privateAdapterDisabled(providerClaudeCode, cfg) {
		return nil, errors.New("claude code private adapter disabled")
	}

	auth, err := loadClaudeCodeAuth(ctx)
	if err != nil {
		return nil, err
	}

	return &ClaudeCodeProvider{
		client:  providerHTTPClient(cfg),
		auth:    auth,
		baseURL: configuredBaseURL("ANTHROPIC_BASE_URL", cfg.BaseURL, defaultAnthropicBase),
		models:  defaultClaudeCodeModels(),
	}, nil
}

// Name returns the provider name.
func (c *ClaudeCodeProvider) Name() string { return providerClaudeCode }

// Models returns model IDs/aliases Claude Code can serve.
func (c *ClaudeCodeProvider) Models() []string {
	if len(c.models) == 0 {
		return defaultClaudeCodeModels()
	}

	return append([]string(nil), c.models...)
}

// FetchModels returns the local Claude Code model catalog. The OAuth-mode
// /v1/models endpoint is gated separately; we keep a static list to avoid an
// extra round-trip for what is effectively a UI-only listing.
func (c *ClaudeCodeProvider) FetchModels(ctx context.Context) ([]string, error) {
	if err := requireCredentialContext(ctx); err != nil {
		return nil, err
	}

	return c.Models(), nil
}

// AdapterDiagnostics reports the private Claude Code adapter contract and readiness.
func (c *ClaudeCodeProvider) AdapterDiagnostics() AdapterDiagnostics {
	access := ""
	if c.auth != nil {
		access = c.auth.snapshot()
	}

	return AdapterDiagnostics{
		Contract: claudeCodeAdapterContract(),
		Checks: []ReadinessCheck{
			{
				Name:   "local_credentials",
				Status: readinessStatus(access != ""),
				Detail: readinessDetail(access != "", "Claude Code OAuth access token loaded", "missing Claude Code OAuth access token; run `claude` to log in"),
			},
			{
				Name:   "token_refresh",
				Status: readinessStatus(c.auth != nil && c.auth.hasRefreshToken()),
				Detail: readinessDetail(c.auth != nil && c.auth.hasRefreshToken(), "refresh_token available for one retry after HTTP 401", "missing refresh_token; adapter cannot recover from expired access tokens"),
			},
			{
				Name:   "network_reachability",
				Status: ReadinessSkipped,
				Detail: "not probed during doctor; OAuth-mode Messages access is verified only by a completion request",
			},
			{
				Name:   "model_availability",
				Status: ReadinessWarning,
				Detail: "static catalog only; OAuth model listing is not treated as a stable public contract",
			},
		},
		Warnings: []string{
			"uses borrowed Claude Code OAuth credentials, beta Anthropic routing headers, and a static non-network-verified model catalog",
		},
		Models: c.Models(),
	}
}

// ProviderModelsVerified reports whether FetchModels is an authoritative live
// provider availability check. Claude Code currently exposes a local/static
// model catalog here, so absence from the list should remain unverified.
func (c *ClaudeCodeProvider) ProviderModelsVerified() bool {
	return false
}

// HealthCheck verifies that we have an OAuth access token loaded. It does not
// hit the network — provider-level credential validity is asserted lazily on
// the next Complete call (with auto-refresh on 401).
func (c *ClaudeCodeProvider) HealthCheck(ctx context.Context) error {
	if err := requireCredentialContext(ctx); err != nil {
		return err
	}

	if err := authorizeProviderPermission(ctx, providerClaudeCode, "check Claude Code credentials", "Claude Code OAuth", permission.OperationCredentialAccess); err != nil {
		return err
	}

	emitActivity(ctx, events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command":  "claude_code.auth.check",
			"provider": providerClaudeCode,
		},
	})

	if c.auth == nil || c.auth.snapshot() == "" {
		return errors.New("no Claude Code OAuth access token: run `claude` to log in")
	}

	return nil
}

// ModelContextWindow returns the context window size for a Claude Code model.
func (c *ClaudeCodeProvider) ModelContextWindow(model string) int {
	if limit := catalogContextWindow(providerClaudeCode, model); limit > 0 {
		return limit
	}

	metadata, ok := c.ModelMetadata(model)
	if !ok {
		return 0
	}

	return metadata.ContextWindow
}

// ModelCatalog returns static Claude Code model metadata with provenance.
func (c *ClaudeCodeProvider) ModelCatalog() []ModelMetadata {
	out := make([]ModelMetadata, 0, len(c.Models()))
	for _, model := range c.Models() {
		metadata, ok := c.ModelMetadata(model)
		if !ok {
			continue
		}

		out = append(out, metadata)
	}

	return out
}

// ModelMetadata returns provenance for a Claude Code model entry.
func (c *ClaudeCodeProvider) ModelMetadata(model string) (ModelMetadata, bool) {
	if metadata, ok := builtinCatalogModelMetadata(
		providerClaudeCode,
		model,
		claudeCodeAdapterReviewedAt,
		claudeCodeAdapterReviewAfter,
		"Claude Code auth mode has no model metadata endpoint; built-in catalog metadata is used for known Claude IDs",
	); ok {
		return metadata, true
	}

	for _, entry := range claudeCodeModelCatalog() {
		if entry.ID == model {
			return entry, true
		}
	}

	return ModelMetadata{}, false
}

// Complete performs a chat completion against the Anthropic Messages API,
// refreshing the OAuth access token once on 401 and retrying transparently.
func (c *ClaudeCodeProvider) Complete(ctx context.Context, params CompleteParams) (*Response, error) {
	if err := requireCredentialContext(ctx); err != nil {
		return nil, err
	}

	model := params.Model
	if model == "" {
		models := c.Models()
		if len(models) == 0 {
			return nil, errors.New("claude code model not configured")
		}

		model = models[0]
	}

	params.Model = model

	params, adjustments, err := prepareCompleteParamsForProvider(providerClaudeCode, params)
	if err != nil {
		return nil, err
	}

	req, err := buildAnthropicRequestForProvider(providerClaudeCode, params)
	if err != nil {
		return nil, err
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("claude code: marshal: %w", err)
	}

	resp, err := c.doMessagesRequest(ctx, body, commandActivityOnce(ctx, events.Event{
		Type:     events.CommandExecute,
		Model:    model,
		Metadata: claudeCodeCommandMetadata(adjustments),
	}))
	if err != nil {
		return nil, err
	}

	resp.Model = firstNonEmptyString(resp.Model, model)

	return resp, nil
}

func claudeCodeCommandMetadata(adjustments []completeParamAdjustment) map[string]string {
	metadata := map[string]string{
		"command":  "claude_code.messages",
		"provider": providerClaudeCode,
	}
	if len(adjustments) > 0 {
		metadata["option_adjustments"] = formatCompleteParamAdjustments(adjustments)
	}

	return metadata
}

func (c *ClaudeCodeProvider) doMessagesRequest(ctx context.Context, body []byte, startCallback func()) (*Response, error) {
	access := c.auth.snapshot()

	resp, err := c.sendMessages(ctx, body, access, startCallback)
	if err == nil {
		return resp, nil
	}

	var unauthorized *claudeCodeUnauthorizedError
	if !errors.As(err, &unauthorized) {
		return nil, err
	}

	if refreshErr := c.auth.refresh(ctx, access); refreshErr != nil {
		return nil, fmt.Errorf("claude code refresh after 401: %w", refreshErr)
	}

	access = c.auth.snapshot()

	return c.sendMessages(ctx, body, access, startCallback)
}

type claudeCodeUnauthorizedError struct {
	err *ProviderError
}

func (e *claudeCodeUnauthorizedError) Error() string {
	return e.err.Error()
}

func (e *claudeCodeUnauthorizedError) Unwrap() error {
	return e.err
}

func (c *ClaudeCodeProvider) sendMessages(ctx context.Context, body []byte, access string, startCallback func()) (*Response, error) {
	if err := authorizeProviderPermission(ctx, providerClaudeCode, "call Claude Code messages", c.baseURL+"/v1/messages", permission.OperationNetwork, permission.OperationCredentialAccess); err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("claude code: new request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("anthropic-version", defaultAnthropicVersion)
	httpReq.Header.Set("anthropic-beta", anthropicOAuthBetas)
	httpReq.Header.Set("Authorization", "Bearer "+access)

	if startCallback != nil {
		startCallback()
	}

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("claude code: request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxOAuthErrorBodyBytes)) //nolint:errcheck // best-effort body capture for the error message
		return nil, &claudeCodeUnauthorizedError{err: newProviderHTTPError(providerClaudeCode, resp, raw)}
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("claude code: read body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, newProviderHTTPError(providerClaudeCode, resp, respBody)
	}

	var ar anthropicResponse
	if err := json.Unmarshal(respBody, &ar); err != nil {
		return nil, fmt.Errorf("claude code: unmarshal: %w", err)
	}

	if ar.Error != nil {
		return nil, newProviderPayloadError(providerClaudeCode, resp.StatusCode, resp.Header, ar.Error.Type, ar.Error.Message)
	}

	result := parseAnthropicResponse(ar)
	result.Provider = providerClaudeCode

	return result, nil
}

func defaultClaudeCodeModels() []string {
	return modelIDsFromMetadata(claudeCodeModelCatalog())
}

func claudeCodeModelCatalog() []ModelMetadata {
	return cloneModelMetadata([]ModelMetadata{
		claudeCodeCatalogEntry("claude-opus-4-7"),
		claudeCodeCatalogEntry("claude-opus-4-6"),
		claudeCodeCatalogEntry("claude-opus-4-5-20251101"),
		claudeCodeCatalogEntry("claude-opus-4-1-20250805"),
		claudeCodeCatalogEntry("claude-opus-4-20250514"),
		claudeCodeCatalogEntry("claude-sonnet-4-6"),
		claudeCodeCatalogEntry("claude-sonnet-4-5-20250929"),
		claudeCodeCatalogEntry("claude-sonnet-4-20250514"),
		claudeCodeCatalogEntry("claude-haiku-4-5-20251001"),
		claudeCodeAliasCatalogEntry("opus"),
		claudeCodeAliasCatalogEntry("sonnet"),
		claudeCodeAliasCatalogEntry("haiku"),
		claudeCodeAliasCatalogEntry("claude-haiku-4-5"),
	})
}

func claudeCodeCatalogEntry(id string) ModelMetadata {
	return ModelMetadata{
		ID:            id,
		ContextWindow: 200_000,
		Provenance:    "static Claude Code adapter catalog reviewed against the local provider contract; not network verified",
		ReviewedAt:    claudeCodeAdapterReviewedAt,
		ReviewAfter:   claudeCodeAdapterReviewAfter,
		Notes:         "OAuth and beta routing behavior belongs to Claude Code compatibility, not the public Anthropic API-key contract",
	}
}

func claudeCodeAliasCatalogEntry(id string) ModelMetadata {
	entry := claudeCodeCatalogEntry(id)
	entry.Provenance = "static Claude Code CLI alias reviewed against the local provider contract; not network verified"
	entry.Notes = "alias resolution is owned by Claude Code compatibility; Atteler records the assumed 200k context window instead of deriving it live"

	return entry
}

//nolint:gosec // Documents credential source paths and JSON field names, not secret values.
func claudeCodeAdapterContract() AdapterContract {
	return AdapterContract{
		Provider:         providerClaudeCode,
		AdapterVersion:   claudeCodeAdapterVersion,
		SourceCLI:        claudeCodeAdapterSource,
		SourceCLIVersion: claudeCodeAdapterCLIVersion,
		Protocol:         claudeCodeAdapterProtocol,
		Credential:       "macOS Keychain Claude Code-credentials or ~/.claude/.credentials.json claudeAiOauth",
		KillSwitches: []string{
			"providers.claude-code.disable_private_adapter",
			"ATTELER_DISABLE_CLAUDE_CODE_ADAPTER",
			"ATTELER_DISABLE_PRIVATE_ADAPTERS",
			"ATTELER_DISABLE_BORROWED_CREDENTIAL_ADAPTERS",
		},
		ReviewedAt:  claudeCodeAdapterReviewedAt,
		ReviewAfter: claudeCodeAdapterReviewAfter,
	}
}
