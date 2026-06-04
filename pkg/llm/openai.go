package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"

	"github.com/tommoulard/atteler/pkg/modelroute"
	"github.com/tommoulard/atteler/pkg/permission"
)

const defaultOpenAIBase = "https://api.openai.com"

const (
	openAICompatibleType             = "openai_compatible"
	azureOpenAIType                  = "azure_openai"
	defaultOpenAIModelsPath          = "/v1/models"
	defaultOpenAIChatPath            = "/v1/chat/completions"
	defaultOpenAIEmbeddingsPath      = "/v1/embeddings"
	defaultAzureOpenAIAPIEnv         = "AZURE_OPENAI_API_KEY"
	defaultAzureOpenAIChatPath       = "/openai/deployments/{model}/chat/completions"
	defaultAzureOpenAIEmbeddingsPath = "/openai/deployments/{model}/embeddings"
	apiKeySchemeNone                 = "none"
)

// OpenAIProvider calls the OpenAI Chat Completions API.
type OpenAIProvider struct {
	client         *http.Client
	apiKey         string
	baseURL        string
	providerName   string
	authHeader     string
	authScheme     string
	chatPath       string
	embeddingsPath string
	modelsPath     string
	apiVersion     string
	staticModels   []string
	capabilities   []string
	bearer         bool
	local          bool
}

// NewOpenAIProvider is kept for source compatibility only.
//
// Deprecated: use NewOpenAIProviderContext so credential reads inherit caller
// cancellation checks.
func NewOpenAIProvider() (*OpenAIProvider, error) {
	return nil, ErrContextRequired
}

// NewOpenAIProviderContext creates a provider using ResolveOpenAIKeyContext.
// The base URL can be overridden with OPENAI_BASE_URL.
func NewOpenAIProviderContext(ctx context.Context) (*OpenAIProvider, error) {
	return NewOpenAIProviderWithConfigContext(ctx, ProviderConfig{})
}

// NewOpenAIProviderWithConfig is kept for source compatibility only.
//
// Deprecated: use NewOpenAIProviderWithConfigContext so credential reads
// inherit caller cancellation checks.
func NewOpenAIProviderWithConfig(_ ProviderConfig) (*OpenAIProvider, error) {
	return nil, ErrContextRequired
}

// NewOpenAIProviderWithConfigContext creates a provider using
// ResolveOpenAIKeyContext and optional config values. OPENAI_BASE_URL overrides
// cfg.BaseURL.
func NewOpenAIProviderWithConfigContext(ctx context.Context, cfg ProviderConfig) (*OpenAIProvider, error) {
	key, bearer, err := ResolveOpenAIKeyContext(ctx)
	if err != nil {
		return nil, err
	}

	baseURL := configuredBaseURL("OPENAI_BASE_URL", cfg.BaseURL, defaultOpenAIBase)

	return &OpenAIProvider{
		apiKey:         key,
		bearer:         bearer,
		baseURL:        baseURL,
		providerName:   providerOpenAI,
		authHeader:     "Authorization",
		authScheme:     "Bearer",
		chatPath:       defaultOpenAIChatPath,
		embeddingsPath: defaultOpenAIEmbeddingsPath,
		modelsPath:     defaultOpenAIModelsPath,
		client:         providerHTTPClient(cfg),
		local:          cfg.Local || isLocalEndpoint(baseURL),
	}, nil
}

// NewOpenAICompatibleProviderWithConfigContext creates a provider for endpoints
// that implement OpenAI's /v1/models and /v1/chat/completions wire shape. When
// APIKeyEnv is empty, no Authorization header is sent so local vLLM/TGI-style
// endpoints can be configured without fake secrets.
func NewOpenAICompatibleProviderWithConfigContext(ctx context.Context, name string, cfg ProviderConfig) (*OpenAIProvider, error) {
	if err := requireCredentialContext(ctx); err != nil {
		return nil, err
	}

	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("openai-compatible: provider name cannot be empty")
	}

	if strings.Contains(name, "/") {
		return nil, fmt.Errorf("openai-compatible: provider name %q must not contain /", name)
	}

	baseURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if baseURL == "" {
		return nil, fmt.Errorf("openai-compatible: provider %q requires base_url", name)
	}

	if err := validateKnownRouteCapabilities(
		fmt.Sprintf("openai-compatible provider %q capabilities", name),
		cfg.Capabilities,
	); err != nil {
		return nil, err
	}

	cfg = openAICompatibleDefaults(cfg)
	if err := validateOpenAICompatibleEndpointPaths(name, cfg); err != nil {
		return nil, err
	}

	apiKey, err := openAICompatibleAPIKey(ctx, name, cfg.APIKeyEnv)
	if err != nil {
		return nil, err
	}

	return &OpenAIProvider{
		apiKey:         apiKey,
		baseURL:        baseURL,
		providerName:   name,
		authHeader:     cfg.APIKeyHeader,
		authScheme:     cfg.APIKeyScheme,
		chatPath:       cfg.ChatCompletionsPath,
		embeddingsPath: cfg.EmbeddingsPath,
		modelsPath:     cfg.ModelsPath,
		apiVersion:     cfg.APIVersion,
		staticModels:   cleanModelList(cfg.Models),
		capabilities:   cleanCapabilityList(cfg.Capabilities),
		client:         providerHTTPClient(cfg),
		local:          cfg.Local || capabilityListContains(cfg.Capabilities, modelroute.CapabilityLocal) || isLocalEndpoint(baseURL),
	}, nil
}

func openAICompatibleDefaults(cfg ProviderConfig) ProviderConfig {
	providerType := normalizeOpenAIProviderType(cfg.Type)
	if providerType == azureOpenAIType {
		return trimOpenAICompatibleConfig(azureOpenAICompatibleDefaults(cfg))
	}

	return trimOpenAICompatibleConfig(standardOpenAICompatibleDefaults(cfg))
}

func azureOpenAICompatibleDefaults(cfg ProviderConfig) ProviderConfig {
	if strings.TrimSpace(cfg.APIKeyEnv) == "" {
		cfg.APIKeyEnv = defaultAzureOpenAIAPIEnv
	}

	if strings.TrimSpace(cfg.APIKeyHeader) == "" {
		cfg.APIKeyHeader = "api-key"
	}

	if strings.TrimSpace(cfg.APIKeyScheme) == "" {
		cfg.APIKeyScheme = apiKeySchemeNone
	}

	if strings.TrimSpace(cfg.ChatCompletionsPath) == "" {
		cfg.ChatCompletionsPath = defaultAzureOpenAIChatPath
	}

	if strings.TrimSpace(cfg.EmbeddingsPath) == "" {
		cfg.EmbeddingsPath = defaultAzureOpenAIEmbeddingsPath
	}

	return cfg
}

func standardOpenAICompatibleDefaults(cfg ProviderConfig) ProviderConfig {
	if strings.TrimSpace(cfg.APIKeyHeader) == "" {
		cfg.APIKeyHeader = "Authorization"
	}

	if strings.TrimSpace(cfg.APIKeyScheme) == "" {
		cfg.APIKeyScheme = "Bearer"
	}

	if strings.TrimSpace(cfg.ChatCompletionsPath) == "" {
		cfg.ChatCompletionsPath = defaultOpenAIChatPath
	}

	if strings.TrimSpace(cfg.EmbeddingsPath) == "" {
		cfg.EmbeddingsPath = defaultOpenAIEmbeddingsPath
	}

	if strings.TrimSpace(cfg.ModelsPath) == "" {
		cfg.ModelsPath = defaultOpenAIModelsPath
	}

	return cfg
}

func trimOpenAICompatibleConfig(cfg ProviderConfig) ProviderConfig {
	cfg.Type = strings.TrimSpace(cfg.Type)
	cfg.APIKeyEnv = strings.TrimSpace(cfg.APIKeyEnv)
	cfg.APIKeyHeader = strings.TrimSpace(cfg.APIKeyHeader)
	cfg.APIKeyScheme = strings.TrimSpace(cfg.APIKeyScheme)
	cfg.ChatCompletionsPath = strings.TrimSpace(cfg.ChatCompletionsPath)
	cfg.EmbeddingsPath = strings.TrimSpace(cfg.EmbeddingsPath)
	cfg.ModelsPath = strings.TrimSpace(cfg.ModelsPath)
	cfg.APIVersion = strings.TrimSpace(cfg.APIVersion)

	return cfg
}

func validateOpenAICompatibleEndpointPaths(providerName string, cfg ProviderConfig) error {
	checks := []struct {
		field string
		path  string
	}{
		{field: "chat_completions_path", path: cfg.ChatCompletionsPath},
		{field: "embeddings_path", path: cfg.EmbeddingsPath},
		{field: "models_path", path: cfg.ModelsPath},
	}

	for _, check := range checks {
		path := strings.TrimSpace(check.path)
		if path == "" || strings.HasPrefix(path, "/") {
			continue
		}

		return fmt.Errorf(
			"openai-compatible provider %q %s %q must start with /",
			providerName,
			check.field,
			path,
		)
	}

	return nil
}

func normalizeOpenAIProviderType(providerType string) string {
	switch strings.ToLower(strings.TrimSpace(providerType)) {
	case "azure_openai", "azure-openai", "azure":
		return azureOpenAIType
	case "openai_compatible", "openai-compatible", "openai",
		"groq", "mistral", "cohere",
		"gemini", "google", "google_gemini", "google-gemini",
		"google_ai", "google-ai", "google_ai_studio", "google-ai-studio",
		"ai_studio", "ai-studio",
		"vertex", "vertex_ai", "vertex-ai",
		"bedrock", "aws_bedrock", "aws-bedrock", "amazon_bedrock", "amazon-bedrock",
		"vllm", "tgi", "text_generation_inference", "text-generation-inference",
		"self_hosted", "self-hosted", "selfhosted",
		"litellm", "lite_llm", "lite-llm",
		"openrouter", "open_router", "open-router":
		return openAICompatibleType
	default:
		return strings.ToLower(strings.TrimSpace(providerType))
	}
}

func openAICompatibleAPIKey(ctx context.Context, providerName, envName string) (string, error) {
	if err := requireCredentialContext(ctx); err != nil {
		return "", err
	}

	envName = strings.TrimSpace(envName)
	if envName == "" {
		return "", nil
	}

	value := strings.TrimSpace(os.Getenv(envName))
	if value == "" {
		return "", fmt.Errorf("openai-compatible: provider %q has no credentials: set %s", providerName, envName)
	}

	return value, nil
}

func isLocalEndpoint(rawURL string) bool {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return false
	}

	host := strings.ToLower(strings.Trim(parsed.Hostname(), "[]"))
	switch host {
	case "localhost", "127.0.0.1", "::1", "0.0.0.0":
		return true
	default:
		return strings.HasPrefix(host, "127.")
	}
}

// Name returns the provider name.
func (o *OpenAIProvider) Name() string {
	if o.providerName != "" {
		return o.providerName
	}

	return providerOpenAI
}

// Local reports whether this OpenAI-compatible provider is self-hosted/local.
func (o *OpenAIProvider) Local() bool {
	return o != nil && o.local
}

// Models returns the static list of supported models (fallback).
func (o *OpenAIProvider) Models() []string {
	if o.Name() != providerOpenAI {
		return append([]string(nil), o.staticModels...)
	}

	static := []string{
		"gpt-4.1",
		"gpt-4.1-mini",
		"gpt-4.1-nano",
		"o4-mini",
		"text-embedding-3-small",
		"text-embedding-3-large",
	}

	return mergeModelLists(static, catalogModelsByProvider()[providerOpenAI])
}

// ---------------------------------------------------------------------------
// OpenAI Models API
// ---------------------------------------------------------------------------

type openaiModelsResponse struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

// FetchModels queries GET /v1/models to discover available models.
func (o *OpenAIProvider) FetchModels(ctx context.Context) ([]string, error) {
	if err := requireCredentialContext(ctx); err != nil {
		return nil, err
	}

	endpoint, err := o.endpointURL(o.effectiveModelsPath(), "")
	if err != nil {
		return nil, err
	}

	if endpoint == "" {
		if len(o.staticModels) > 0 {
			return append([]string(nil), o.staticModels...), nil
		}

		return nil, fmt.Errorf("%s: models endpoint is not configured", o.Name())
	}

	if policyErr := authorizeProviderPermission(ctx, o.Name(), "fetch OpenAI models", endpoint, permission.OperationNetwork, permission.OperationCredentialAccess); policyErr != nil {
		return nil, policyErr
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("%s: new request: %w", o.Name(), err)
	}

	o.setAuthHeader(httpReq)

	resp, err := o.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("%s: models request: %w", o.Name(), err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%s: read models body: %w", o.Name(), err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, newProviderHTTPError(o.Name(), resp, body)
	}

	var mr openaiModelsResponse
	if err := json.Unmarshal(body, &mr); err != nil {
		return nil, fmt.Errorf("%s: unmarshal models: %w", o.Name(), err)
	}

	out := make([]string, 0, len(mr.Data))
	for _, m := range mr.Data {
		out = append(out, m.ID)
	}

	return out, nil
}

// HealthCheck verifies that the OpenAI API is reachable and the credentials
// are valid by issuing a lightweight GET /v1/models request.
func (o *OpenAIProvider) HealthCheck(ctx context.Context) error {
	_, err := o.FetchModels(ctx)
	return err
}

// ---------------------------------------------------------------------------
// OpenAI Chat Completions request / response shapes
// ---------------------------------------------------------------------------

type openaiRequest struct {
	Temperature     *float64              `json:"temperature,omitempty"`
	TopP            *float64              `json:"top_p,omitempty"`
	Seed            *int                  `json:"seed,omitempty"`
	ResponseFormat  *openaiResponseFormat `json:"response_format,omitempty"`
	Model           string                `json:"model,omitempty"`
	ServiceTier     string                `json:"service_tier,omitempty"`
	ReasoningEffort string                `json:"reasoning_effort,omitempty"`
	Messages        []openaiMessage       `json:"messages"`
	Stop            []string              `json:"stop,omitempty"`
	Tools           []openaiTool          `json:"tools,omitempty"`
	MaxTokens       int                   `json:"max_tokens,omitempty"`
}

type openaiResponseFormat struct {
	JSONSchema *openaiJSONSchema `json:"json_schema,omitempty"`
	Type       string            `json:"type"`
}

type openaiJSONSchema struct {
	Schema map[string]any `json:"schema"`
	Name   string         `json:"name"`
	Strict bool           `json:"strict,omitempty"`
}

type openaiTool struct {
	Function openaiToolFunction `json:"function"`
	Type     string             `json:"type"`
}

type openaiToolFunction struct {
	Parameters  map[string]any `json:"parameters"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
}

type openaiMessage struct {
	Role       string           `json:"role"`
	Content    string           `json:"content,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	ToolCalls  []openaiToolCall `json:"tool_calls,omitempty"`
}

type openaiToolCall struct {
	ID       string                 `json:"id"`
	Type     string                 `json:"type"`
	Function openaiToolCallFunction `json:"function"`
}

type openaiToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON string.
}

type openaiResponse struct {
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
	Model   string `json:"model"`
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Content   string           `json:"content"`
			ToolCalls []openaiToolCall `json:"tool_calls,omitempty"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		PromptTokensDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
}

type openaiEmbeddingRequest struct {
	Input      any    `json:"input"`
	Model      string `json:"model,omitempty"`
	Dimensions int    `json:"dimensions,omitempty"`
}

type openaiEmbeddingResponse struct {
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
	Model string                `json:"model,omitempty"`
	Data  []openaiEmbeddingData `json:"data"`
	Usage struct {
		PromptTokens int `json:"prompt_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage"`
}

type openaiEmbeddingData struct {
	Embedding []float64 `json:"embedding"`
	Index     int       `json:"index"`
}

// Complete performs a chat completion using the OpenAI Chat Completions API.
func (o *OpenAIProvider) Complete(ctx context.Context, params CompleteParams) (*Response, error) {
	if err := requireCredentialContext(ctx); err != nil {
		return nil, err
	}

	return o.complete(ctx, params)
}

func (o *OpenAIProvider) complete(ctx context.Context, params CompleteParams) (*Response, error) {
	req, err := o.buildRequest(params)
	if err != nil {
		return nil, err
	}

	if o.omitModelInChatBody() {
		req.Model = ""
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("%s: marshal: %w", o.Name(), err)
	}

	endpoint, err := o.endpointURL(o.effectiveChatPath(), params.Model)
	if err != nil {
		return nil, err
	}

	if policyErr := authorizeProviderPermission(ctx, o.Name(), "call OpenAI chat completions", endpoint, permission.OperationNetwork, permission.OperationCredentialAccess); policyErr != nil {
		return nil, policyErr
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%s: new request: %w", o.Name(), err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	o.setAuthHeader(httpReq)

	resp, err := o.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("%s: request: %w", o.Name(), err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%s: read body: %w", o.Name(), err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, newProviderHTTPError(o.Name(), resp, respBody)
	}

	var or openaiResponse
	if err := json.Unmarshal(respBody, &or); err != nil {
		return nil, fmt.Errorf("%s: unmarshal: %w", o.Name(), err)
	}

	if or.Error != nil {
		return nil, newProviderPayloadError(
			o.Name(),
			resp.StatusCode,
			resp.Header,
			firstNonEmptyString(or.Error.Code, or.Error.Type),
			or.Error.Message,
		)
	}

	result := parseOpenAIResponse(or)
	result.Provider = o.Name()

	return result, nil
}

// Embed performs a vector embedding request using OpenAI's embeddings wire
// shape. OpenAI-compatible providers use the same endpoint by default.
func (o *OpenAIProvider) Embed(ctx context.Context, params EmbeddingParams) (*EmbeddingResponse, error) {
	if err := requireCredentialContext(ctx); err != nil {
		return nil, err
	}

	if providerDeclaresEmbeddingsUnsupported(o) {
		return nil, fmt.Errorf("%w: %s capabilities do not include embeddings", ErrEmbeddingsUnsupported, o.Name())
	}

	if len(params.Input) == 0 {
		return nil, errors.New("openai: embedding input cannot be empty")
	}

	respBody, err := o.sendEmbeddingRequest(ctx, params)
	if err != nil {
		return nil, err
	}

	return o.parseEmbeddingResponse(respBody, params.Model)
}

func (o *OpenAIProvider) sendEmbeddingRequest(ctx context.Context, params EmbeddingParams) ([]byte, error) {
	req := openaiEmbeddingRequest{
		Input:      append([]string(nil), params.Input...),
		Model:      params.Model,
		Dimensions: params.Dimensions,
	}
	if o.omitModelInEmbeddingBody() {
		req.Model = ""
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("%s: marshal embeddings: %w", o.Name(), err)
	}

	endpoint, err := o.endpointURL(o.effectiveEmbeddingsPath(), params.Model)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%s: new embeddings request: %w", o.Name(), err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	o.setAuthHeader(httpReq)

	resp, err := o.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("%s: embeddings request: %w", o.Name(), err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%s: read embeddings body: %w", o.Name(), err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, newProviderHTTPError(o.Name(), resp, respBody)
	}

	return respBody, nil
}

func (o *OpenAIProvider) parseEmbeddingResponse(respBody []byte, requestedModel string) (*EmbeddingResponse, error) {
	var embeddingResp openaiEmbeddingResponse
	if err := json.Unmarshal(respBody, &embeddingResp); err != nil {
		return nil, fmt.Errorf("%s: unmarshal embeddings: %w", o.Name(), err)
	}

	if embeddingResp.Error != nil {
		return nil, fmt.Errorf("%s: embeddings %s: %s", o.Name(), embeddingResp.Error.Type, embeddingResp.Error.Message)
	}

	if len(embeddingResp.Data) == 0 {
		return nil, fmt.Errorf("%s: embeddings empty response from %s", o.Name(), requestedModel)
	}

	vectors, err := o.openAIEmbeddingVectors(embeddingResp.Data)
	if err != nil {
		return nil, err
	}

	model := strings.TrimSpace(embeddingResp.Model)
	if model == "" {
		model = requestedModel
	}

	return &EmbeddingResponse{
		Provider:    o.Name(),
		Model:       model,
		Embeddings:  vectors,
		InputTokens: embeddingResp.Usage.PromptTokens,
	}, nil
}

func (o *OpenAIProvider) openAIEmbeddingVectors(data []openaiEmbeddingData) ([][]float64, error) {
	vectors := make([][]float64, len(data))
	for _, item := range data {
		if item.Index < 0 || item.Index >= len(vectors) {
			return nil, fmt.Errorf("%s: embeddings response index %d out of range", o.Name(), item.Index)
		}

		if len(item.Embedding) == 0 {
			return nil, fmt.Errorf("%s: embeddings empty vector at index %d", o.Name(), item.Index)
		}

		vectors[item.Index] = append([]float64(nil), item.Embedding...)
	}

	for i := range vectors {
		if len(vectors[i]) == 0 {
			return nil, fmt.Errorf("%s: embeddings missing vector at index %d", o.Name(), i)
		}
	}

	return vectors, nil
}

func (o *OpenAIProvider) buildRequest(params CompleteParams) (openaiRequest, error) {
	return buildOpenAIRequestWithCapabilities(o.Name(), o.Capabilities(), params)
}

func (o *OpenAIProvider) omitModelInChatBody() bool {
	return strings.Contains(o.effectiveChatPath(), "{model}")
}

func (o *OpenAIProvider) omitModelInEmbeddingBody() bool {
	return strings.Contains(o.effectiveEmbeddingsPath(), "{model}")
}

func (o *OpenAIProvider) setAuthHeader(req *http.Request) {
	if o.apiKey == "" {
		return
	}

	header := strings.TrimSpace(o.authHeader)
	if header == "" {
		header = "Authorization"
	}

	req.Header.Set(header, apiKeyHeaderValue(o.apiKey, o.effectiveAuthScheme()))
}

func (o *OpenAIProvider) effectiveAuthScheme() string {
	if strings.TrimSpace(o.authScheme) != "" {
		return o.authScheme
	}

	if o.Name() == providerOpenAI {
		return "Bearer"
	}

	return ""
}

func apiKeyHeaderValue(apiKey, scheme string) string {
	scheme = strings.TrimSpace(scheme)
	switch strings.ToLower(scheme) {
	case "", apiKeySchemeNone, "raw", "-":
		return apiKey
	default:
		return scheme + " " + apiKey
	}
}

func (o *OpenAIProvider) effectiveModelsPath() string {
	if strings.TrimSpace(o.modelsPath) != "" {
		return o.modelsPath
	}

	if o.Name() == providerOpenAI {
		return defaultOpenAIModelsPath
	}

	return ""
}

func (o *OpenAIProvider) effectiveChatPath() string {
	if strings.TrimSpace(o.chatPath) != "" {
		return o.chatPath
	}

	return defaultOpenAIChatPath
}

func (o *OpenAIProvider) effectiveEmbeddingsPath() string {
	if strings.TrimSpace(o.embeddingsPath) != "" {
		return o.embeddingsPath
	}

	return defaultOpenAIEmbeddingsPath
}

func (o *OpenAIProvider) endpointURL(pathTemplate, model string) (string, error) {
	pathTemplate = strings.TrimSpace(pathTemplate)
	if pathTemplate == "" {
		return "", nil
	}

	if !strings.HasPrefix(pathTemplate, "/") {
		return "", fmt.Errorf("%s: endpoint path %q must start with /", o.Name(), pathTemplate)
	}

	path := strings.ReplaceAll(pathTemplate, "{model}", url.PathEscape(model))
	path = o.endpointPath(path)
	endpoint := o.baseURL + path

	apiVersion := strings.TrimSpace(o.apiVersion)
	if apiVersion == "" {
		return endpoint, nil
	}

	separator := "?"
	if strings.Contains(endpoint, "?") {
		separator = "&"
	}

	return endpoint + separator + "api-version=" + url.QueryEscape(apiVersion), nil
}

func (o *OpenAIProvider) endpointPath(path string) string {
	basePath := ""
	if parsed, err := url.Parse(o.baseURL); err == nil {
		basePath = strings.TrimRight(parsed.EscapedPath(), "/")
	}

	// OpenAI-compatible services and SDK examples commonly configure base_url
	// with the API version prefix (for example http://localhost:8000/v1) or with
	// a provider-specific OpenAI-compatible root that already embeds the version
	// (for example Google's /v1beta/openai). Keep repository defaults rooted at
	// /v1 for api.openai.com and unversioned roots, but avoid producing
	// /v1/v1/... or /v1beta/openai/v1/... when users provide those common forms.
	if strings.HasSuffix(basePath, "/v1") && strings.HasPrefix(path, "/v1/") {
		return strings.TrimPrefix(path, "/v1")
	}

	if openAICompatibleRootEmbedsVersion(basePath) && strings.HasPrefix(path, "/v1/") {
		return strings.TrimPrefix(path, "/v1")
	}

	return path
}

func openAICompatibleRootEmbedsVersion(basePath string) bool {
	segments := strings.Split(strings.Trim(basePath, "/"), "/")
	if len(segments) < 2 || segments[len(segments)-1] != "openai" {
		return false
	}

	return slices.ContainsFunc(segments[:len(segments)-1], pathSegmentLooksLikeAPIVersion)
}

func pathSegmentLooksLikeAPIVersion(segment string) bool {
	segment = strings.ToLower(strings.TrimSpace(segment))

	return len(segment) > 1 && segment[0] == 'v' && segment[1] >= '0' && segment[1] <= '9'
}

func buildOpenAIRequest(params CompleteParams) (openaiRequest, error) {
	capabilities, _ := BuiltInProviderCapabilities(providerOpenAI)

	return buildOpenAIRequestWithCapabilities(providerOpenAI, capabilities, params)
}

func buildOpenAIRequestWithCapabilities(
	providerName string,
	capabilities ProviderCapabilities,
	params CompleteParams,
) (openaiRequest, error) {
	if err := validateCompleteParamsAgainstCapabilities(providerName, capabilities, params); err != nil {
		return openaiRequest{}, err
	}

	req := openaiRequest{
		Model:    params.Model,
		Messages: buildOpenAIMessages(params.Messages),
		Stop:     params.Stop,
	}
	if params.MaxTokens > 0 {
		req.MaxTokens = params.MaxTokens
	}

	if params.Temperature != nil {
		req.Temperature = params.Temperature
	}

	if params.TopP != nil {
		req.TopP = params.TopP
	}

	if params.Seed != nil {
		req.Seed = params.Seed
	}

	if effort := openAIReasoningEffort(params.ReasoningLevel); effort != "" && capabilities.SupportsReasoning {
		req.ReasoningEffort = effort
	}

	if tier := openAIServiceTierForModelMode(params.ModelMode); tier != "" {
		req.ServiceTier = tier
	}

	if params.ResponseFormat != nil {
		responseFormat, err := buildOpenAIResponseFormat(params.ResponseFormat)
		if err != nil {
			return openaiRequest{}, err
		}

		req.ResponseFormat = responseFormat
	}

	for _, tool := range params.Tools {
		req.Tools = append(req.Tools, openaiTool{
			Type:     "function",
			Function: openaiToolFunction(tool),
		})
	}

	return req, nil
}

func buildOpenAIResponseFormat(format *ResponseFormat) (*openaiResponseFormat, error) {
	normalized, err := normalizeResponseFormat(format)
	if err != nil {
		return nil, fmt.Errorf("openai: CompleteParams.ResponseFormat: %w", err)
	}

	switch normalized.Type {
	case "":
		return nil, nil
	case ResponseFormatJSONObject:
		return &openaiResponseFormat{Type: ResponseFormatJSONObject}, nil
	case ResponseFormatJSONSchema:
		return &openaiResponseFormat{
			Type: ResponseFormatJSONSchema,
			JSONSchema: &openaiJSONSchema{
				Name:   normalized.Name,
				Schema: normalized.Schema,
				Strict: normalized.Strict,
			},
		}, nil
	default:
		return nil, fmt.Errorf("openai: CompleteParams.ResponseFormat: unsupported type %q", normalized.Type)
	}
}

func parseOpenAIResponse(or openaiResponse) *Response {
	result := &Response{
		Provider:          providerOpenAI,
		Model:             or.Model,
		InputTokens:       or.Usage.PromptTokens,
		CachedInputTokens: or.Usage.PromptTokensDetails.CachedTokens,
		OutputTokens:      or.Usage.CompletionTokens,
	}

	if len(or.Choices) > 0 {
		choice := or.Choices[0]
		result.Content = choice.Message.Content
		result.StopReason = openaiStopReason(choice.FinishReason)
		result.ToolCalls = parseOpenAIToolCalls(choice.Message.ToolCalls)
	}

	return result
}

func buildOpenAIMessages(messages []Message) []openaiMessage {
	msgs := make([]openaiMessage, 0, len(messages))

	for _, m := range messages {
		omsg := openaiMessage{Role: string(m.Role), Content: m.Content}

		// Marshal assistant messages with tool calls.
		if m.Role == RoleAssistant && len(m.ToolCalls) > 0 {
			for _, tc := range m.ToolCalls {
				args, err := json.Marshal(tc.Input)
				if err != nil {
					args = []byte("{}")
				}

				omsg.ToolCalls = append(omsg.ToolCalls, openaiToolCall{
					ID:   tc.ID,
					Type: "function",
					Function: openaiToolCallFunction{
						Name:      tc.Name,
						Arguments: string(args),
					},
				})
			}
		}

		// Marshal tool result messages.
		if m.Role == RoleTool && m.ToolResult != nil {
			omsg.ToolCallID = m.ToolResult.ToolCallID
			omsg.Content = m.ToolResult.Content
		}

		msgs = append(msgs, omsg)
	}

	return msgs
}

func parseOpenAIToolCalls(calls []openaiToolCall) []ToolCall {
	if len(calls) == 0 {
		return nil
	}

	out := make([]ToolCall, 0, len(calls))

	for _, tc := range calls {
		var input map[string]any

		if err := json.Unmarshal([]byte(tc.Function.Arguments), &input); err != nil {
			input = map[string]any{"raw": tc.Function.Arguments}
		}

		out = append(out, ToolCall{
			ID:    tc.ID,
			Name:  tc.Function.Name,
			Input: input,
		})
	}

	return out
}

func openaiStopReason(reason string) StopReason {
	switch reason {
	case "stop":
		return StopEndTurn
	case "tool_calls":
		return StopToolUse
	case "length":
		return StopMaxToks
	default:
		return StopUnknown
	}
}

func configuredBaseURL(envKey, configured, fallback string) string {
	if v := os.Getenv(envKey); v != "" {
		return v
	}

	if configured != "" {
		return configured
	}

	return fallback
}

// ModelContextWindow returns the context window size for an OpenAI model.
func (o *OpenAIProvider) ModelContextWindow(model string) int {
	if limit := catalogContextWindow(o.Name(), model); limit > 0 {
		return limit
	}

	return openaiContextWindow(model)
}

//nolint:cyclop // Flat model lookup table is clearer as a switch.
func openaiContextWindow(model string) int {
	switch model {
	case "gpt-4.1":
		return 1_047_576
	case "gpt-4.1-mini":
		return 1_047_576
	case "gpt-4.1-nano":
		return 1_047_576
	case "o4-mini":
		return 200_000
	case "o3", "o3-pro":
		return 200_000
	case "o3-mini":
		return 200_000
	case "o1", "o1-pro":
		return 200_000
	case "o1-mini":
		return 128_000
	case "gpt-4o", "gpt-4o-mini":
		return 128_000
	case "gpt-4-turbo":
		return 128_000
	case "gpt-4":
		return 8_192
	default:
		if strings.HasPrefix(model, "gpt-4.1") {
			return 1_047_576
		}

		if strings.HasPrefix(model, "gpt-4o") || strings.HasPrefix(model, "gpt-4-turbo") {
			return 128_000
		}

		if strings.HasPrefix(model, "o1") || strings.HasPrefix(model, "o3") || strings.HasPrefix(model, "o4") {
			return 200_000
		}

		return 0
	}
}
