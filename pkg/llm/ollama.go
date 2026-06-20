package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/tommoulard/atteler/pkg/permission"
	attshell "github.com/tommoulard/atteler/pkg/shell"
)

const (
	defaultOllamaBase       = "http://127.0.0.1:11434"
	envOllamaAutoStart      = "ATTELER_OLLAMA_AUTO_START"
	ollamaServeCommand      = "ollama"
	ollamaStartupTimeout    = 5 * time.Second
	ollamaStartupPollPeriod = 100 * time.Millisecond
)

// OllamaProvider calls a local or configured Ollama server.
//
//nolint:govet // Field order groups provider lifecycle state for readability.
type OllamaProvider struct {
	mu              sync.Mutex // guards client and startAttempted
	client          *http.Client
	baseURL         string
	sessionID       string
	ownershipPath   string
	commandLine     []string
	autoStartSource string
	autoStart       bool
	startAttempted  bool
}

// NewOllamaProvider creates a provider using OLLAMA_BASE_URL or the local
// Ollama default. The provider is created only when Ollama is reachable unless
// the base URL is explicitly configured.
func NewOllamaProvider(ctx context.Context) (*OllamaProvider, error) {
	return NewOllamaProviderWithConfigContext(ctx, ProviderConfig{})
}

// NewOllamaProviderWithConfigContext creates a provider using OLLAMA_BASE_URL,
// cfg.BaseURL, or the local Ollama default. OLLAMA_BASE_URL overrides cfg.BaseURL.
func NewOllamaProviderWithConfigContext(ctx context.Context, cfg ProviderConfig) (*OllamaProvider, error) {
	if err := requireCredentialContext(ctx); err != nil {
		return nil, err
	}

	baseURL := strings.TrimRight(configuredBaseURL("OLLAMA_BASE_URL", cfg.BaseURL, defaultOllamaBase), "/")

	autoStartPolicy := ollamaAutoStartPolicy(cfg.AutoStart)
	if cfg.autoStartBlocked {
		autoStartPolicy.Enabled = false
	}

	p := &OllamaProvider{
		baseURL:         baseURL,
		client:          providerHTTPClient(cfg),
		sessionID:       cfg.SessionID,
		ownershipPath:   cfg.OwnershipPath,
		commandLine:     append([]string(nil), cfg.CommandLine...),
		autoStartSource: autoStartPolicy.Source,
		autoStart:       autoStartPolicy.Enabled && isLocalOllamaBaseURL(baseURL),
	}

	if ollamaExplicitlyConfigured(cfg) && !p.autoStart {
		return p, nil
	}

	healthErr := p.HealthCheck(ctx)
	if healthErr == nil {
		return p, nil
	}

	if p.autoStart && isOllamaDaemonUnavailable(healthErr) {
		startErr := p.startDaemonAndWait(ctx)
		if startErr == nil {
			return p, nil
		}

		return nil, errors.Join(healthErr, startErr)
	}

	if ollamaExplicitlyConfigured(cfg) {
		return p, nil
	}

	return nil, healthErr
}

func ollamaExplicitlyConfigured(cfg ProviderConfig) bool {
	return os.Getenv("OLLAMA_BASE_URL") != "" || cfg.BaseURL != ""
}

//nolint:govet // Field order mirrors the ownership metadata recorded at start.
type ollamaStartRequest struct {
	BaseURL       string
	SessionID     string
	OwnershipPath string
	CommandLine   []string
	PolicySource  string
}

//nolint:govet // Field order keeps ownership metadata before transient log buffer.
type ollamaDaemonStart struct {
	ownership     OllamaDaemonOwnership
	logs          *boundedLogBuffer
	ownershipPath string
}

func (s *ollamaDaemonStart) startupLogs() string {
	if s == nil || s.logs == nil {
		return ""
	}

	return s.logs.String()
}

type ollamaServeStarter func(ctx context.Context, req ollamaStartRequest) (*ollamaDaemonStart, error)

var (
	ollamaServeStarterMu sync.Mutex
	startOllamaServe     ollamaServeStarter = startOllamaServeProcess
)

func (o *OllamaProvider) startDaemonAndWait(ctx context.Context) error {
	o.mu.Lock()
	alreadyAttempted := o.startAttempted
	o.startAttempted = true
	o.mu.Unlock()

	if alreadyAttempted {
		return errors.New("ollama: daemon start already attempted")
	}

	start, err := callOllamaServeStarter(ctx, ollamaStartRequest{
		BaseURL:       o.baseURL,
		SessionID:     o.sessionID,
		OwnershipPath: o.ownershipPath,
		CommandLine:   o.commandLine,
		PolicySource:  o.autoStartSource,
	})
	if err != nil {
		return err
	}

	if err := o.waitForDaemon(ctx); err != nil {
		return ollamaStartupError(err, start)
	}

	return nil
}

func callOllamaServeStarter(ctx context.Context, req ollamaStartRequest) (*ollamaDaemonStart, error) {
	ollamaServeStarterMu.Lock()
	starter := startOllamaServe
	ollamaServeStarterMu.Unlock()

	return starter(ctx, req)
}

//nolint:cyclop,gocognit // Ollama startup has a linear authorize/start/record/cleanup lifecycle that is easier to audit in one place.
func startOllamaServeProcess(ctx context.Context, req ollamaStartRequest) (*ollamaDaemonStart, error) {
	if err := requireCredentialContext(ctx); err != nil {
		return nil, fmt.Errorf("ollama: start daemon: %w", err)
	}

	logs := newBoundedLogBuffer(ollamaStartupLogBytes)

	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("ollama: start daemon: %w", err)
	}

	environment := map[string]string{}
	if host := ollamaHostForBaseURL(req.BaseURL); host != "" {
		environment["OLLAMA_HOST"] = host
	}

	cmd, invocation, err := attshell.CommandContext(ctx, attshell.CommandOptions{
		Program:              ollamaServeCommand,
		Args:                 []string{"serve"},
		Env:                  environment,
		Mode:                 attshell.ModeStreaming,
		Detach:               true,
		Stdout:               logs,
		Stderr:               logs,
		PermissionOperations: ollamaServePermissionOperations(req),
		Audit: attshell.AuditContext{
			Caller:    "atteler.provider.ollama",
			SessionID: req.SessionID,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("ollama: start daemon: %w", err)
	}

	stateDir := ollamaStateDir(req.OwnershipPath)
	if permissionErr := authorizeOllamaStateWritePermission(ctx, "prepare Ollama daemon state directory", stateDir); permissionErr != nil {
		if finishErr := invocation.Finish(attshell.FinishOptions{
			Error:         permissionErr,
			OutputCapture: attshell.OutputNotCaptured,
			OutputNote:    "ollama daemon did not start; state directory preparation was denied by policy",
		}); finishErr != nil {
			permissionErr = errors.Join(permissionErr, finishErr)
		}

		return nil, fmt.Errorf("ollama: prepare daemon state dir: %w", permissionErr)
	}

	if mkdirErr := os.MkdirAll(stateDir, 0o750); mkdirErr != nil {
		if finishErr := invocation.Finish(attshell.FinishOptions{
			Error:         mkdirErr,
			OutputCapture: attshell.OutputNotCaptured,
			OutputNote:    "ollama daemon did not start; state directory preparation failed",
		}); finishErr != nil {
			mkdirErr = errors.Join(mkdirErr, finishErr)
		}

		return nil, fmt.Errorf("ollama: prepare daemon state dir: %w", mkdirErr)
	}

	if permissionErr := authorizeOllamaStateWritePermission(ctx, "write Ollama startup log", stateDir); permissionErr != nil {
		if finishErr := invocation.Finish(attshell.FinishOptions{
			Error:         permissionErr,
			OutputCapture: attshell.OutputNotCaptured,
			OutputNote:    "ollama daemon did not start; startup log creation was denied by policy",
		}); finishErr != nil {
			permissionErr = errors.Join(permissionErr, finishErr)
		}

		return nil, fmt.Errorf("ollama: create startup log: %w", permissionErr)
	}

	logFile, logErr := os.CreateTemp(stateDir, "ollama-startup-*.log")
	if logErr != nil {
		if finishErr := invocation.Finish(attshell.FinishOptions{
			Error:         logErr,
			OutputCapture: attshell.OutputNotCaptured,
			OutputNote:    "ollama daemon did not start; startup log creation failed",
		}); finishErr != nil {
			logErr = errors.Join(logErr, finishErr)
		}

		return nil, fmt.Errorf("ollama: create startup log: %w", logErr)
	}

	logPath := logFile.Name()
	logWriter := &lockedWriter{writer: io.MultiWriter(newCappedLogFileWriter(logFile, ollamaStartupLogBytes), logs)}
	cmd.Stdout = logWriter
	cmd.Stderr = logWriter

	if err := cmd.Start(); err != nil {
		_ = logFile.Close()

		if permissionErr := authorizeOllamaStateDeletePermission(ctx, "delete failed Ollama startup log", logPath); permissionErr != nil {
			err = errors.Join(err, permissionErr)
		} else if removeErr := os.Remove(logPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, removeErr)
		}

		if finishErr := invocation.Finish(attshell.FinishOptions{
			Stderr:        logs.String(),
			Error:         err,
			OutputCapture: attshell.OutputCaptured,
			OutputNote:    "ollama daemon failed before startup completed",
		}); finishErr != nil {
			err = errors.Join(err, finishErr)
		}

		return nil, fmt.Errorf("ollama: start daemon: %w", err)
	}

	ownership := OllamaDaemonOwnership{
		Owner:           ollamaOwnershipOwner,
		PID:             cmd.Process.Pid,
		AttelerPID:      os.Getpid(),
		ParentPID:       os.Getppid(),
		Command:         []string{ollamaServeCommand, "serve"},
		Environment:     environment,
		StartedAt:       time.Now().UTC(),
		BaseURL:         req.BaseURL,
		SessionID:       req.SessionID,
		AttelerCommand:  ollamaCommandLine(req.CommandLine),
		AutoStartSource: req.PolicySource,
		LogPath:         logPath,
	}

	ownershipPath := ollamaOwnershipPath(req.OwnershipPath)
	if err := recordOllamaOwnershipContext(ctx, ownershipPath, ownership); err != nil {
		err = cleanupUntrackedOllamaDaemonAfterOwnershipFailure(ctx, cmd, ownershipPath, ownership, err)

		if finishErr := invocation.Finish(attshell.FinishOptions{
			Stderr:        logs.String(),
			Error:         err,
			OutputCapture: attshell.OutputCaptured,
			OutputNote:    "ollama daemon stopped after ownership write failure",
		}); finishErr != nil {
			slog.Warn("ollama daemon audit after ownership failure failed", "pid", cmd.Process.Pid, "error", finishErr)
		}

		_ = logFile.Close()

		return nil, fmt.Errorf("ollama: record daemon ownership: %w", err)
	}

	slog.Info("ollama daemon started by atteler",
		"pid", cmd.Process.Pid,
		"base_url", req.BaseURL,
		"auto_start_source", req.PolicySource,
		"ownership_path", ownershipPath,
		"log_path", logPath,
		"stop_command", "atteler --ollama-stop",
	)

	go func() {
		defer func() {
			if err := logFile.Close(); err != nil {
				slog.Warn("ollama startup log close failed", "path", logPath, "error", err)
			}
		}()

		waitErr := cmd.Wait()
		if finishErr := invocation.Finish(attshell.FinishOptions{
			Stderr:        logs.String(),
			Error:         waitErr,
			OutputCapture: attshell.OutputCaptured,
			OutputNote:    "ollama daemon startup log snapshot",
		}); finishErr != nil {
			slog.Warn("ollama daemon audit failed", "pid", cmd.Process.Pid, "log_path", logPath, "error", finishErr)
		}

		if waitErr != nil {
			slog.Warn("ollama daemon exited", "pid", cmd.Process.Pid, "log_path", logPath, "error", waitErr)
		}
	}()

	return &ollamaDaemonStart{ownership: ownership, ownershipPath: ownershipPath, logs: logs}, nil
}

func cleanupUntrackedOllamaDaemonAfterOwnershipFailure(
	ctx context.Context,
	cmd *exec.Cmd,
	ownershipPath string,
	ownership OllamaDaemonOwnership,
	err error,
) error {
	cleanupErr := authorizeOllamaStopPermission(
		ctx,
		"stop untracked Ollama daemon after ownership write failure",
		ownershipPath,
		&ownership,
		true,
	)
	if cleanupErr != nil {
		slog.Warn("ollama daemon left running after ownership failure cleanup was denied", "pid", cmd.Process.Pid, "error", cleanupErr)

		return errors.Join(err, cleanupErr)
	}

	if killErr := cmd.Process.Kill(); killErr != nil {
		slog.Warn("ollama daemon cleanup after ownership failure failed", "pid", cmd.Process.Pid, "error", killErr)
	}

	// Reap the child after the forced cleanup so a failed ownership write does
	// not leave a short-lived zombie process behind.
	if waitErr := cmd.Wait(); waitErr != nil {
		slog.Debug("ollama daemon cleanup wait completed", "pid", cmd.Process.Pid, "error", waitErr)
	}

	return err
}

func ollamaServePermissionOperations(req ollamaStartRequest) []permission.Operation {
	return []permission.Operation{
		{
			Kind:   permission.OperationExecute,
			Action: "ollama serve",
			Target: req.BaseURL,
			Source: "atteler.provider.ollama",
		},
		{
			Kind:   permission.OperationWrite,
			Action: "ollama daemon state",
			Target: ollamaStateDir(req.OwnershipPath),
			Source: "atteler.provider.ollama",
		},
		{
			Kind:   permission.OperationNetwork,
			Action: "ollama serve listener",
			Target: req.BaseURL,
			Source: "atteler.provider.ollama",
		},
	}
}

func ollamaCommandLine(commandLine []string) []string {
	if len(commandLine) > 0 {
		return append([]string(nil), commandLine...)
	}

	return append([]string(nil), os.Args...)
}

func ollamaStartupError(err error, start *ollamaDaemonStart) error {
	if start == nil {
		return err
	}

	err = fmt.Errorf("%w; %s", err, start.stopHint())

	logs := strings.TrimSpace(start.startupLogs())
	if logs == "" {
		if start.ownership.LogPath != "" {
			return fmt.Errorf("%w; startup log: %s", err, start.ownership.LogPath)
		}

		return err
	}

	if start.ownership.LogPath != "" {
		return fmt.Errorf("%w; startup log %s: %s", err, start.ownership.LogPath, logs)
	}

	return fmt.Errorf("%w; startup log: %s", err, logs)
}

func (s *ollamaDaemonStart) stopHint() string {
	if s == nil || s.ownership.PID <= 0 {
		return "check `atteler --ollama-status` for daemon status"
	}

	if s.ownershipPath != "" {
		return fmt.Sprintf("Atteler started Ollama PID %d; ownership record: %s; stop with `atteler --ollama-stop`", s.ownership.PID, s.ownershipPath)
	}

	return fmt.Sprintf("Atteler started Ollama PID %d; stop with `atteler --ollama-stop`", s.ownership.PID)
}

func (o *OllamaProvider) waitForDaemon(ctx context.Context) error {
	waitCtx, cancel := context.WithTimeout(ctx, ollamaStartupTimeout)
	defer cancel()

	ticker := time.NewTicker(ollamaStartupPollPeriod)
	defer ticker.Stop()

	var lastErr error

	for {
		err := o.HealthCheck(waitCtx)
		if err == nil {
			return nil
		}

		lastErr = err

		select {
		case <-waitCtx.Done():
			return fmt.Errorf("ollama: daemon did not become ready: %w", lastErr)
		case <-ticker.C:
		}
	}
}

func isLocalOllamaBaseURL(baseURL string) bool {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return false
	}

	return isLocalOllamaHost(parsed.Hostname())
}

func ollamaHostForBaseURL(baseURL string) string {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return ""
	}

	return parsed.Host
}

func isOllamaDaemonUnavailable(err error) bool {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return true
	}

	var netErr net.Error

	return errors.As(err, &netErr)
}

// httpClient returns the provider HTTP client, lazily initializing it for
// zero-value providers. It is safe for concurrent use.
func (o *OllamaProvider) httpClient() *http.Client {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.client == nil {
		o.client = providerHTTPClient(ProviderConfig{})
	}

	return o.client
}

// Name returns the provider name.
func (o *OllamaProvider) Name() string { return providerOllama }

// Models returns useful built-in Ollama model names. Availability depends on
// what has been pulled into the target Ollama server; use FetchModels for live
// discovery.
func (o *OllamaProvider) Models() []string {
	return []string{
		"llama3.2",
		"llama3.1",
		"qwen2.5",
		"mistral",
		"gemma3",
		"deepseek-r1",
		"nomic-embed-text",
	}
}

type ollamaTagsResponse struct {
	Models []struct {
		Name string `json:"name"`
	} `json:"models"`
}

// FetchModels queries GET /api/tags to discover locally available Ollama models.
func (o *OllamaProvider) FetchModels(ctx context.Context) ([]string, error) {
	if err := requireCredentialContext(ctx); err != nil {
		return nil, err
	}

	if policyErr := authorizeProviderPermission(ctx, providerOllama, "fetch Ollama models", o.baseURL+"/api/tags", permission.OperationNetwork); policyErr != nil {
		return nil, policyErr
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, o.baseURL+"/api/tags", http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("ollama: new models request: %w", err)
	}

	resp, err := o.httpClient().Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("ollama: models request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("ollama: read models body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, newProviderHTTPError(providerOllama, resp, body)
	}

	var tr ollamaTagsResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return nil, fmt.Errorf("ollama: unmarshal models: %w", err)
	}

	out := make([]string, 0, len(tr.Models))
	for _, model := range tr.Models {
		if model.Name != "" {
			out = append(out, model.Name)
		}
	}

	return out, nil
}

// HealthCheck verifies that the Ollama server is reachable by listing tags.
func (o *OllamaProvider) HealthCheck(ctx context.Context) error {
	_, err := o.FetchModels(ctx)
	return err
}

type ollamaChatRequest struct {
	Think    any             `json:"think,omitempty"`
	Format   any             `json:"format,omitempty"`
	Model    string          `json:"model"`
	Messages []ollamaMessage `json:"messages"`
	Tools    []ollamaTool    `json:"tools,omitempty"`
	Options  ollamaOptions   `json:"options"`
	Stream   bool            `json:"stream"`
}

type ollamaOptions struct {
	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`
	Seed        *int     `json:"seed,omitempty"`
	Stop        []string `json:"stop,omitempty"`
	NumPredict  int      `json:"num_predict,omitempty"`
}

type ollamaTool struct {
	Function ollamaToolFunction `json:"function"`
	Type     string             `json:"type"`
}

type ollamaToolFunction struct {
	Parameters  map[string]any `json:"parameters"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
}

type ollamaMessage struct {
	Role      string           `json:"role"`
	Content   string           `json:"content"`
	Images    []string         `json:"images,omitempty"`
	ToolCalls []ollamaToolCall `json:"tool_calls,omitempty"`
}

type ollamaToolCall struct {
	Function ollamaToolCallFunction `json:"function"`
}

type ollamaToolCallFunction struct {
	Arguments map[string]any `json:"arguments"`
	Name      string         `json:"name"`
}

type ollamaChatResponse struct {
	Error           string        `json:"error"`
	Model           string        `json:"model"`
	DoneReason      string        `json:"done_reason,omitempty"`
	Message         ollamaMessage `json:"message"`
	PromptEvalCount int           `json:"prompt_eval_count"`
	EvalCount       int           `json:"eval_count"`
	Done            bool          `json:"done"`
}

type ollamaEmbedRequest struct {
	Input any    `json:"input"`
	Model string `json:"model"`
}

type ollamaEmbedResponse struct {
	Error      string      `json:"error"`
	Model      string      `json:"model,omitempty"`
	Embeddings [][]float64 `json:"embeddings"`
}

// Complete performs a non-streaming chat completion using Ollama's /api/chat endpoint.
func (o *OllamaProvider) Complete(ctx context.Context, params CompleteParams) (*Response, error) {
	if err := requireCredentialContext(ctx); err != nil {
		return nil, err
	}

	return o.complete(ctx, params)
}

func (o *OllamaProvider) complete(ctx context.Context, params CompleteParams) (*Response, error) {
	body, err := buildOllamaChatRequestBody(params, false)
	if err != nil {
		return nil, err
	}

	if policyErr := authorizeProviderPermission(ctx, providerOllama, "call Ollama chat", o.baseURL+"/api/chat", permission.OperationNetwork); policyErr != nil {
		return nil, policyErr
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("ollama: new request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := o.httpClient().Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("ollama: request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("ollama: read body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, newProviderHTTPError(providerOllama, resp, respBody)
	}

	var or ollamaChatResponse
	if err := json.Unmarshal(respBody, &or); err != nil {
		return nil, fmt.Errorf("ollama: unmarshal: %w", err)
	}

	if or.Error != "" {
		return nil, newProviderPayloadError(providerOllama, resp.StatusCode, resp.Header, "", or.Error)
	}

	return parseOllamaChatResponse(or, params.Model), nil
}

// Embed performs a vector embedding request using Ollama's /api/embed endpoint.
func (o *OllamaProvider) Embed(ctx context.Context, params EmbeddingParams) (*EmbeddingResponse, error) {
	if err := requireCredentialContext(ctx); err != nil {
		return nil, err
	}

	if len(params.Input) == 0 {
		return nil, errors.New("ollama: embedding input cannot be empty")
	}

	if params.Dimensions > 0 {
		return nil, errors.New("ollama: EmbeddingParams.Dimensions is unsupported")
	}

	respBody, err := o.sendEmbeddingRequest(ctx, params)
	if err != nil {
		return nil, err
	}

	return parseOllamaEmbeddingResponse(respBody, params.Model)
}

func (o *OllamaProvider) sendEmbeddingRequest(ctx context.Context, params EmbeddingParams) ([]byte, error) {
	body, err := json.Marshal(ollamaEmbedRequest{
		Model: params.Model,
		Input: append([]string(nil), params.Input...),
	})
	if err != nil {
		return nil, fmt.Errorf("ollama: marshal embeddings request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("ollama: new embeddings request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := o.httpClient().Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("ollama: embeddings request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("ollama: read embeddings body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, retryableHTTPStatusError(
			fmt.Errorf("ollama: embeddings HTTP %d: %s", resp.StatusCode, respBody),
			resp.StatusCode,
			resp.Header.Get("Retry-After"),
		)
	}

	return respBody, nil
}

func parseOllamaEmbeddingResponse(respBody []byte, requestedModel string) (*EmbeddingResponse, error) {
	var embedResp ollamaEmbedResponse
	if err := json.Unmarshal(respBody, &embedResp); err != nil {
		return nil, fmt.Errorf("ollama: unmarshal embeddings: %w", err)
	}

	if embedResp.Error != "" {
		return nil, fmt.Errorf("ollama: embeddings: %s", embedResp.Error)
	}

	if len(embedResp.Embeddings) == 0 {
		return nil, fmt.Errorf("ollama: embeddings empty response from %s", requestedModel)
	}

	vectors, err := ollamaEmbeddingVectors(embedResp.Embeddings)
	if err != nil {
		return nil, err
	}

	model := strings.TrimSpace(embedResp.Model)
	if model == "" {
		model = requestedModel
	}

	return &EmbeddingResponse{
		Provider:   providerOllama,
		Model:      model,
		Embeddings: vectors,
	}, nil
}

func ollamaEmbeddingVectors(in [][]float64) ([][]float64, error) {
	vectors := make([][]float64, len(in))
	for i := range in {
		if len(in[i]) == 0 {
			return nil, fmt.Errorf("ollama: embeddings empty vector at index %d", i)
		}

		vectors[i] = append([]float64(nil), in[i]...)
	}

	return vectors, nil
}

func buildOllamaChatRequest(params CompleteParams) (ollamaChatRequest, error) {
	return buildOllamaChatRequestForStream(params, false)
}

func buildOllamaChatRequestForStream(params CompleteParams, stream bool) (ollamaChatRequest, error) {
	if params.Model == "" {
		return ollamaChatRequest{}, errors.New("ollama: model is required")
	}

	if err := validateCompleteParamsSupported(providerOllama, params); err != nil {
		return ollamaChatRequest{}, err
	}

	req := ollamaChatRequest{
		Model:    params.Model,
		Messages: buildOllamaMessages(params.Messages),
		Stream:   stream,
		Options: ollamaOptions{
			Temperature: params.Temperature,
			TopP:        params.TopP,
			Seed:        params.Seed,
			Stop:        params.Stop,
		},
	}
	if params.MaxTokens > 0 {
		req.Options.NumPredict = params.MaxTokens
	}

	if think, ok := ollamaThink(params.ReasoningLevel); ok {
		req.Think = think
	}

	if params.ResponseFormat != nil {
		format, err := buildOllamaResponseFormat(params.ResponseFormat)
		if err != nil {
			return ollamaChatRequest{}, err
		}

		req.Format = format
	}

	for _, tool := range params.Tools {
		req.Tools = append(req.Tools, ollamaTool{
			Type:     "function",
			Function: ollamaToolFunction(tool),
		})
	}

	return req, nil
}

func buildOllamaResponseFormat(format *ResponseFormat) (any, error) {
	normalized, err := normalizeResponseFormat(format)
	if err != nil {
		return nil, fmt.Errorf("ollama: CompleteParams.ResponseFormat: %w", err)
	}

	switch normalized.Type {
	case "":
		return nil, nil
	case ResponseFormatJSONObject:
		return "json", nil
	case ResponseFormatJSONSchema:
		return normalized.Schema, nil
	default:
		return nil, fmt.Errorf("ollama: CompleteParams.ResponseFormat: unsupported type %q", normalized.Type)
	}
}

func parseOllamaChatResponse(or ollamaChatResponse, fallbackModel string) *Response {
	model := or.Model
	if model == "" {
		model = fallbackModel
	}

	result := &Response{
		Content:      or.Message.Content,
		Model:        model,
		StopReason:   ollamaCompletionStopReason(or.DoneReason),
		InputTokens:  or.PromptEvalCount,
		OutputTokens: or.EvalCount,
	}

	// Parse tool calls from response.
	result.ToolCalls = parseOllamaToolCalls(or.Message.ToolCalls)
	if len(result.ToolCalls) > 0 {
		result.StopReason = StopToolUse
	}

	return result
}

// CompleteStream performs a streaming chat completion using Ollama's /api/chat
// endpoint. Setup failures are returned directly; once a channel is returned,
// provider/read/cancellation failures are delivered as terminal error chunks.
func (o *OllamaProvider) CompleteStream(ctx context.Context, params CompleteParams) (<-chan Chunk, error) {
	if err := requireCredentialContext(ctx); err != nil {
		return nil, err
	}

	body, err := buildOllamaChatRequestBody(params, true)
	if err != nil {
		return nil, err
	}

	if policyErr := authorizeProviderPermission(ctx, providerOllama, "call Ollama chat stream", o.baseURL+"/api/chat", permission.OperationNetwork); policyErr != nil {
		return nil, policyErr
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("ollama: new request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := o.httpClient().Do(httpReq) //nolint:bodyclose // Successful streaming responses are closed by the goroutine below.
	if err != nil {
		return nil, fmt.Errorf("ollama: request: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()

		respBody, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return nil, fmt.Errorf("ollama: read error body: %w", readErr)
		}

		return nil, newProviderHTTPError(providerOllama, resp, respBody)
	}

	ch := make(chan Chunk, DefaultStreamBuffer)

	go func() {
		defer close(ch)
		defer resp.Body.Close()

		streamOllamaChatResponse(ctx, resp.Body, ch, params.Model)
	}()

	return ch, nil
}

func buildOllamaChatRequestBody(params CompleteParams, stream bool) ([]byte, error) {
	req, err := buildOllamaChatRequestForStream(params, stream)
	if err != nil {
		return nil, err
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("ollama: marshal: %w", err)
	}

	return body, nil
}

func streamOllamaChatResponse(ctx context.Context, r io.Reader, ch chan<- Chunk, fallbackModel string) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			sendOllamaTerminalError(ctx, ch, fmt.Errorf("ollama: stream canceled: %w", err))

			return
		}

		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var resp ollamaChatResponse
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			sendOllamaTerminalError(ctx, ch, fmt.Errorf("ollama: stream unmarshal: %w", err))

			return
		}

		if resp.Error != "" {
			sendOllamaTerminalError(ctx, ch, fmt.Errorf("ollama: %s", resp.Error))

			return
		}

		model := firstNonEmptyString(resp.Model, fallbackModel)

		if resp.Message.Content != "" && !sendOllamaChunk(ctx, ch, Chunk{Content: resp.Message.Content, Model: model}) {
			return
		}

		if resp.Done {
			sendOllamaChunk(ctx, ch, ollamaFinalChunk(resp, model))

			return
		}
	}

	if err := scanner.Err(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			sendOllamaTerminalError(ctx, ch, fmt.Errorf("ollama: stream canceled: %w", ctxErr))

			return
		}

		sendOllamaTerminalError(ctx, ch, fmt.Errorf("ollama: stream read: %w", err))

		return
	}

	if err := ctx.Err(); err != nil {
		sendOllamaTerminalError(ctx, ch, fmt.Errorf("ollama: stream canceled: %w", err))

		return
	}

	sendOllamaTerminalError(ctx, ch, fmt.Errorf("ollama: stream incomplete: %w", ErrStreamIncomplete))
}

func ollamaFinalChunk(resp ollamaChatResponse, model string) Chunk {
	toolCalls := parseOllamaToolCalls(resp.Message.ToolCalls)

	stopReason := ollamaCompletionStopReason(resp.DoneReason)
	if len(toolCalls) > 0 {
		stopReason = StopToolUse
	}

	return Chunk{
		Model:        model,
		Done:         true,
		StopReason:   stopReason,
		ToolCalls:    toolCalls,
		InputTokens:  resp.PromptEvalCount,
		OutputTokens: resp.EvalCount,
	}
}

func sendOllamaChunk(ctx context.Context, ch chan<- Chunk, chunk Chunk) bool {
	select {
	case ch <- chunk:
		return true
	case <-ctx.Done():
		return false
	}
}

func sendOllamaTerminalError(ctx context.Context, ch chan<- Chunk, err error) {
	if err == nil {
		return
	}

	chunk := Chunk{Err: err}

	select {
	case ch <- chunk:
		return
	default:
	}

	select {
	case ch <- chunk:
	case <-ctx.Done():
	}
}

func buildOllamaMessages(messages []Message) []ollamaMessage {
	msgs := make([]ollamaMessage, 0, len(messages))

	for _, m := range messages {
		omsg := ollamaMessage{Role: string(m.Role), Content: messageTextContent(m)}
		for _, part := range m.ContentParts {
			if part.Type == MessageContentPartImage && part.Image != nil {
				omsg.Images = append(omsg.Images, part.Image.DataBase64)
			}
		}

		// Marshal assistant messages with tool calls.
		if m.Role == RoleAssistant && len(m.ToolCalls) > 0 {
			for _, tc := range m.ToolCalls {
				omsg.ToolCalls = append(omsg.ToolCalls, ollamaToolCall{
					Function: ollamaToolCallFunction{
						Name:      tc.Name,
						Arguments: tc.Input,
					},
				})
			}
		}

		// Tool result messages use the "tool" role in Ollama.
		if m.Role == RoleTool && m.ToolResult != nil {
			omsg.Content = m.ToolResult.Content
		}

		msgs = append(msgs, omsg)
	}

	return msgs
}

func parseOllamaToolCalls(calls []ollamaToolCall) []ToolCall {
	if len(calls) == 0 {
		return nil
	}

	out := make([]ToolCall, 0, len(calls))

	for i, tc := range calls {
		out = append(out, ToolCall{
			ID:    fmt.Sprintf("ollama_%d", i),
			Name:  tc.Function.Name,
			Input: tc.Function.Arguments,
		})
	}

	return out
}

func ollamaStopReason(reason string) StopReason {
	switch reason {
	case "stop":
		return StopEndTurn
	case "length":
		return StopMaxToks
	default:
		return StopUnknown
	}
}

func ollamaCompletionStopReason(reason string) StopReason {
	if strings.TrimSpace(reason) == "" {
		return StopEndTurn
	}

	return ollamaStopReason(reason)
}

// ModelContextWindow returns known default context windows for common Ollama models.
func (o *OllamaProvider) ModelContextWindow(model string) int {
	if limit := catalogContextWindow(providerOllama, model); limit > 0 {
		return limit
	}

	model = strings.ToLower(strings.TrimSpace(model))

	model, _, _ = strings.Cut(model, ":")
	switch model {
	case "llama3.2", "llama3.1", "qwen2.5", "mistral", "gemma3", "deepseek-r1":
		return 128_000
	default:
		return 0
	}
}
