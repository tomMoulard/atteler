//nolint:goconst,misspell,wsl_v5 // MCP protocol method names are literal and the session lifecycle is branch-heavy.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tommoulard/atteler/pkg/permission"
	"github.com/tommoulard/atteler/pkg/shell"
)

const (
	// DefaultProtocolVersion is the newest MCP protocol revision this client sends.
	DefaultProtocolVersion = "2025-11-25"

	defaultShutdownTimeout = 500 * time.Millisecond
	defaultHealthTimeout   = 500 * time.Millisecond
	defaultStderrBytes     = 64 * 1024
	defaultDiscoveryPages  = 1000

	// maxIncomingMessageBytes caps one newline-delimited JSON-RPC message read
	// from server stdout. It matches the 4 MiB line caps used by the codex and
	// ollama stream readers in pkg/llm.
	maxIncomingMessageBytes = 4 * 1024 * 1024
)

// SupportedProtocolVersions lists MCP protocol revisions this client can use.
var SupportedProtocolVersions = []string{
	"2025-11-25",
	"2025-06-18",
	"2025-03-26",
	"2024-11-05",
}

// Request is a JSON-RPC 2.0 request sent to an MCP server over stdio.
//
//nolint:govet // JSON field order mirrors the JSON-RPC envelope for readability.
type Request struct {
	JSONRPC string `json:"jsonrpc"`
	ID      any    `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// Response is a JSON-RPC 2.0 response received from an MCP server over stdio.
//
//nolint:govet // JSON field order mirrors the JSON-RPC envelope for readability.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *ResponseError  `json:"error,omitempty"`
}

// ResponseError is a JSON-RPC 2.0 error object.
//
//nolint:govet // JSON field order mirrors JSON-RPC error objects.
type ResponseError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// CallToolParams are the MCP tools/call request parameters.
//
//nolint:govet // JSON field order keeps name before arguments.
type CallToolParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments,omitempty"`
}

// Implementation describes an MCP client or server implementation.
type Implementation struct {
	Name        string `json:"name"`
	Title       string `json:"title,omitempty"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
	WebsiteURL  string `json:"websiteUrl,omitempty"`
}

// ClientCapabilities declares optional MCP capabilities supported by Atteler.
type ClientCapabilities struct {
	Experimental map[string]any `json:"experimental,omitempty"`
	Roots        map[string]any `json:"roots,omitempty"`
	Sampling     map[string]any `json:"sampling,omitempty"`
	Elicitation  map[string]any `json:"elicitation,omitempty"`
	Tasks        map[string]any `json:"tasks,omitempty"`
}

// InitializeRequestParams are sent in the first MCP initialize request.
type InitializeRequestParams struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    ClientCapabilities `json:"capabilities"`
	ClientInfo      Implementation     `json:"clientInfo"`
}

// InitializeResult is returned by an MCP server after capability negotiation.
type InitializeResult struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    ServerCapabilities `json:"capabilities"`
	ServerInfo      Implementation     `json:"serverInfo"`
	Instructions    string             `json:"instructions,omitempty"`
}

// ListChangedCapability is used by prompts/tools server capabilities.
type ListChangedCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

// ResourcesCapability describes server resource support.
type ResourcesCapability struct {
	Subscribe   bool `json:"subscribe,omitempty"`
	ListChanged bool `json:"listChanged,omitempty"`
}

// ServerCapabilities captures the MCP server capabilities Atteler knows how to gate.
// Unknown capability extensions are intentionally ignored instead of rejected.
type ServerCapabilities struct {
	Experimental map[string]any         `json:"experimental,omitempty"`
	Logging      map[string]any         `json:"logging,omitempty"`
	Completions  map[string]any         `json:"completions,omitempty"`
	Prompts      *ListChangedCapability `json:"prompts,omitempty"`
	Resources    *ResourcesCapability   `json:"resources,omitempty"`
	Tools        *ListChangedCapability `json:"tools,omitempty"`
	Tasks        map[string]any         `json:"tasks,omitempty"`
}

// Has reports whether a known server capability was negotiated.
func (c ServerCapabilities) Has(name string) bool {
	switch strings.TrimSpace(name) {
	case "tools":
		return c.Tools != nil
	case "resources":
		return c.Resources != nil
	case "prompts":
		return c.Prompts != nil
	case "logging":
		return c.Logging != nil
	case "completions":
		return c.Completions != nil
	case "tasks":
		return c.Tasks != nil
	case "experimental":
		return c.Experimental != nil
	default:
		return false
	}
}

// Tool describes a tool discovered through tools/list.
//
//nolint:govet // JSON field grouping keeps schemas and descriptive metadata together.
type Tool struct {
	InputSchema  json.RawMessage `json:"inputSchema,omitempty"`
	OutputSchema json.RawMessage `json:"outputSchema,omitempty"`
	Annotations  map[string]any  `json:"annotations,omitempty"`
	Meta         map[string]any  `json:"_meta,omitempty"`
	Name         string          `json:"name"`
	Title        string          `json:"title,omitempty"`
	Description  string          `json:"description,omitempty"`
	Execution    map[string]any  `json:"execution,omitempty"`
}

// Resource describes a resource discovered through resources/list.
//
//nolint:govet // JSON field grouping keeps identifiers before metadata.
type Resource struct {
	Annotations map[string]any `json:"annotations,omitempty"`
	Meta        map[string]any `json:"_meta,omitempty"`
	URI         string         `json:"uri"`
	Name        string         `json:"name"`
	Title       string         `json:"title,omitempty"`
	Description string         `json:"description,omitempty"`
	MimeType    string         `json:"mimeType,omitempty"`
	Size        *int64         `json:"size,omitempty"`
}

// PromptArgument describes an argument accepted by a discovered prompt.
type PromptArgument struct {
	Name        string `json:"name"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
}

// Prompt describes a prompt discovered through prompts/list.
//
//nolint:govet // JSON field grouping keeps arguments near prompt metadata.
type Prompt struct {
	Arguments   []PromptArgument `json:"arguments,omitempty"`
	Meta        map[string]any   `json:"_meta,omitempty"`
	Name        string           `json:"name"`
	Title       string           `json:"title,omitempty"`
	Description string           `json:"description,omitempty"`
}

// ListToolsResult is returned by the MCP tools/list method.
//
//nolint:govet // JSON field order mirrors MCP pagination shape.
type ListToolsResult struct {
	Tools      []Tool `json:"tools"`
	NextCursor string `json:"nextCursor,omitempty"`
}

// ListResourcesResult is returned by the MCP resources/list method.
//
//nolint:govet // JSON field order mirrors MCP pagination shape.
type ListResourcesResult struct {
	Resources  []Resource `json:"resources"`
	NextCursor string     `json:"nextCursor,omitempty"`
}

// ListPromptsResult is returned by the MCP prompts/list method.
//
//nolint:govet // JSON field order mirrors MCP pagination shape.
type ListPromptsResult struct {
	Prompts    []Prompt `json:"prompts"`
	NextCursor string   `json:"nextCursor,omitempty"`
}

// SessionOptions controls a long-lived MCP stdio server session.
type SessionOptions struct {
	Policy             *shell.Policy
	Permission         *permission.Policy
	Audit              shell.AuditContext
	ClientInfo         Implementation
	ClientCapabilities ClientCapabilities
	ProtocolVersion    string
	InitializeTimeout  time.Duration
	ShutdownTimeout    time.Duration
	HealthTimeout      time.Duration
	MaxStderrBytes     int
	MaxDiscoveryPages  int
}

// HealthStatus summarizes an MCP session health check.
type HealthStatus struct {
	Server      string
	Stderr      string
	Error       string
	Running     bool
	Initialized bool
	Healthy     bool
}

// Session is a persistent MCP stdio client connection to one server process.
//
//nolint:govet // Lifecycle state is grouped by lock ownership instead of memory layout.
type Session struct {
	server Server
	opts   SessionOptions

	startMu     sync.Mutex
	closeMu     sync.Mutex
	stateMu     sync.Mutex
	started     bool
	closing     bool
	closed      bool
	initialized bool
	cmd         *exec.Cmd
	invocation  *shell.Invocation
	stdin       io.WriteCloser
	stdoutPipe  io.Closer
	stderrPipe  io.Closer
	cancelProc  context.CancelFunc

	writeMu sync.Mutex

	pendingMu sync.Mutex
	pending   map[string]chan responseResult
	retired   map[string]struct{}

	waitMu   sync.Mutex
	waitDone chan struct{}
	waitErr  error

	readMu   sync.Mutex
	readDone chan struct{}
	readErr  error

	stderr     *stderrBuffer
	stderrDone chan struct{}
	secrets    []string

	requestSeq int64

	initMu           sync.RWMutex
	initializeResult InitializeResult
	tools            []Tool
	resources        []Resource
	prompts          []Prompt
	toolIndex        map[string]Tool
}

// NewSession constructs a reusable MCP server session. Call Start before use and
// Close when finished.
func NewSession(server Server, opts SessionOptions) *Session {
	if strings.TrimSpace(opts.ProtocolVersion) == "" {
		opts.ProtocolVersion = DefaultProtocolVersion
	}

	if strings.TrimSpace(opts.ClientInfo.Name) == "" {
		opts.ClientInfo.Name = "atteler"
	}

	if strings.TrimSpace(opts.ClientInfo.Version) == "" {
		opts.ClientInfo.Version = "dev"
	}

	if opts.ShutdownTimeout <= 0 {
		opts.ShutdownTimeout = defaultShutdownTimeout
	}

	if opts.HealthTimeout <= 0 {
		opts.HealthTimeout = defaultHealthTimeout
	}

	if opts.MaxStderrBytes <= 0 {
		opts.MaxStderrBytes = defaultStderrBytes
	}

	if opts.MaxDiscoveryPages <= 0 {
		opts.MaxDiscoveryPages = defaultDiscoveryPages
	}

	return &Session{
		server:     server,
		opts:       opts,
		pending:    make(map[string]chan responseResult),
		retired:    make(map[string]struct{}),
		waitDone:   make(chan struct{}),
		readDone:   make(chan struct{}),
		stderr:     newStderrBuffer(opts.MaxStderrBytes),
		stderrDone: make(chan struct{}),
		secrets:    explicitServerSecretValues(server.Env),
		toolIndex:  make(map[string]Tool),
	}
}

// Start launches the configured server, performs MCP initialize negotiation,
// sends notifications/initialized, and discovers tools when supported.
//
//nolint:cyclop // Process setup, protocol negotiation, and cleanup have distinct error exits.
func (s *Session) Start(ctx context.Context) error {
	if s == nil {
		return errors.New("start mcp session: nil session")
	}

	if err := requireInvokeContext(ctx); err != nil {
		return err
	}

	s.startMu.Lock()
	defer s.startMu.Unlock()

	if alreadyStarted, err := s.startState(); err != nil || alreadyStarted {
		if err != nil {
			return err
		}
		return nil
	}

	if err := s.validateStartInputs(); err != nil {
		return err
	}

	// Keep the server process lifetime owned by the session, not by the
	// caller's start/request cancellation. Start still uses ctx for policy setup
	// and initialize/discovery deadlines; if those fail, Close tears the process
	// down while procCtx preserves caller values for command setup.
	procCtx, cancel := newSessionProcessContext(ctx)
	cmd, invocation, err := shell.CommandContext(procCtx, shell.CommandOptions{
		Program:              strings.TrimSpace(s.server.Command),
		Args:                 s.server.Args,
		Dir:                  s.server.CWD,
		Env:                  mcpServerEnv(s.server.Env, s.opts.Audit.Autonomy),
		Policy:               mcpShellPolicy(s.opts.Policy, s.server.Env),
		Permission:           s.opts.Permission,
		PermissionOperations: mcpPermissionOperations(s.server),
		Mode:                 shell.ModeStreaming,
		SecretValues:         s.secrets,
		Audit:                s.auditContext(),
	})
	if err != nil {
		cancel()
		return fmt.Errorf("authorize mcp server %q: %w", strings.TrimSpace(s.server.Name), err)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return fmt.Errorf("open stdin for mcp server %q: %w", strings.TrimSpace(s.server.Name), finishMCPSetupError(invocation, err))
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return fmt.Errorf("open stdout for mcp server %q: %w", strings.TrimSpace(s.server.Name), finishMCPSetupError(invocation, err))
	}

	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return fmt.Errorf("open stderr for mcp server %q: %w", strings.TrimSpace(s.server.Name), finishMCPSetupError(invocation, err))
	}

	// Run the server in its own process group so shutdown signals reach
	// children of wrapper commands (sh, npx, uvx), not only the direct child.
	configureProcessGroup(cmd)

	startErr := cmd.Start()
	if startErr != nil {
		cancel()
		return finishMCPStartError(ctx, invocation, strings.TrimSpace(s.server.Name), startErr)
	}

	s.stateMu.Lock()
	s.started = true
	s.cmd = cmd
	s.invocation = invocation
	s.stdin = stdin
	s.stdoutPipe = stdout
	s.stderrPipe = stderrPipe
	s.cancelProc = cancel
	s.stateMu.Unlock()

	go s.captureStderr(stderrPipe)
	go s.waitForProcess(cmd)
	go s.readLoop(stdout)

	initCtx, cancelInit := s.initializeContext(ctx)
	defer cancelInit()

	response, err := s.invoke(initCtx, Request{
		Method: "initialize",
		Params: InitializeRequestParams{
			ProtocolVersion: s.opts.ProtocolVersion,
			Capabilities:    s.opts.ClientCapabilities,
			ClientInfo:      s.opts.ClientInfo,
		},
	}, false)
	if err != nil {
		_ = s.close(newShutdownContext(ctx))
		return withProcessOutput(fmt.Errorf("initialize mcp server %q: %w", strings.TrimSpace(s.server.Name), err), s.Stderr())
	}

	initResult, err := parseInitializeResult(response.Result)
	if err != nil {
		_ = s.close(newShutdownContext(ctx))
		return withProcessOutput(fmt.Errorf("initialize mcp server %q: %w", strings.TrimSpace(s.server.Name), err), s.Stderr())
	}

	if !supportedProtocolVersion(initResult.ProtocolVersion) {
		_ = s.close(newShutdownContext(ctx))
		return withProcessOutput(fmt.Errorf("initialize mcp server %q: unsupported protocol version %q", strings.TrimSpace(s.server.Name), initResult.ProtocolVersion), s.Stderr())
	}

	s.initMu.Lock()
	s.initializeResult = initResult
	s.initMu.Unlock()

	if err := s.validateDeclaredCapabilities(initResult.Capabilities); err != nil {
		_ = s.close(newShutdownContext(ctx))
		return err
	}

	if err := s.notify(initCtx, "notifications/initialized", nil); err != nil {
		_ = s.close(newShutdownContext(ctx))
		return withProcessOutput(fmt.Errorf("initialize mcp server %q: send initialized notification: %w", strings.TrimSpace(s.server.Name), err), s.Stderr())
	}

	s.stateMu.Lock()
	s.initialized = true
	s.stateMu.Unlock()

	if initResult.Capabilities.Has("tools") {
		if _, err := s.ListTools(initCtx); err != nil {
			_ = s.close(newShutdownContext(ctx))
			return fmt.Errorf("discover tools for mcp server %q: %w", strings.TrimSpace(s.server.Name), err)
		}
	}

	return nil
}

func (s *Session) validateStartInputs() error {
	if err := s.server.Validate(); err != nil {
		return fmt.Errorf("start mcp server: %w", err)
	}

	if err := validateClientCapabilities(s.opts.ClientCapabilities); err != nil {
		return fmt.Errorf("start mcp server %q: %w", strings.TrimSpace(s.server.Name), err)
	}

	return nil
}

func (s *Session) startState() (bool, error) {
	s.stateMu.Lock()
	started := s.started
	closed := s.closed
	s.stateMu.Unlock()

	if started && !closed {
		if !s.running() {
			return true, fmt.Errorf("start mcp server %q: session is not running; close it and create a new session", strings.TrimSpace(s.server.Name))
		}

		return true, nil
	}

	if closed {
		return false, fmt.Errorf("start mcp server %q: session is closed", strings.TrimSpace(s.server.Name))
	}

	return false, nil
}

// Invoke sends one JSON-RPC request over an initialized session and returns the
// matching response. The session process remains alive for future calls.
func (s *Session) Invoke(ctx context.Context, request Request) (*Response, error) {
	return s.invoke(ctx, request, true)
}

// CallTool invokes a discovered MCP tool through tools/call.
func (s *Session) CallTool(ctx context.Context, toolName string, arguments map[string]any) (*Response, error) {
	toolName = strings.TrimSpace(toolName)
	if toolName == "" {
		return nil, errors.New("call mcp tool: missing tool name")
	}

	return s.Invoke(ctx, Request{
		Method: "tools/call",
		Params: CallToolParams{
			Name:      toolName,
			Arguments: arguments,
		},
	})
}

// ListTools refreshes and returns tools discovered from the server.
func (s *Session) ListTools(ctx context.Context) ([]Tool, error) {
	if err := s.requireInitialized(); err != nil {
		return nil, err
	}

	if err := s.requireCapability("tools", "tools/list"); err != nil {
		return nil, err
	}

	var all []Tool
	cursor := ""
	pageCount := 0
	seen := make(map[string]struct{})
	seenCursors := make(map[string]struct{})

	for {
		pageCount++
		if err := checkDiscoveryPageLimit("tools/list", pageCount, s.opts.MaxDiscoveryPages); err != nil {
			return nil, err
		}

		var params any
		if cursor != "" {
			params = map[string]any{"cursor": cursor}
		}

		response, err := s.invoke(ctx, Request{Method: "tools/list", Params: params}, false)
		if err != nil {
			return nil, err
		}

		page, err := parseListToolsResult(response.Result)
		if err != nil {
			return nil, err
		}

		all, err = appendListToolsPage(all, page, seen)
		if err != nil {
			return nil, err
		}

		nextCursor, err := nextPaginationCursor("tools/list", page.NextCursor, seenCursors)
		if err != nil {
			return nil, err
		}

		if nextCursor == "" {
			break
		}

		cursor = nextCursor
	}

	index := make(map[string]Tool, len(all))
	for _, tool := range all {
		index[tool.Name] = tool
	}

	s.initMu.Lock()
	s.tools = append([]Tool(nil), all...)
	s.toolIndex = index
	s.initMu.Unlock()

	return append([]Tool(nil), all...), nil
}

func appendListToolsPage(all []Tool, page ListToolsResult, seen map[string]struct{}) ([]Tool, error) {
	for i, tool := range page.Tools {
		name := strings.TrimSpace(tool.Name)
		if name == "" {
			return nil, fmt.Errorf("tools/list returned tool %d with empty name", i)
		}

		if _, ok := seen[name]; ok {
			return nil, fmt.Errorf("tools/list returned duplicate tool %q", name)
		}

		tool.Name = name
		if err := validateDiscoveredToolSchema(tool); err != nil {
			return nil, fmt.Errorf("tools/list returned tool %q: %w", name, err)
		}

		seen[name] = struct{}{}
		all = append(all, tool)
	}

	return all, nil
}

func nextPaginationCursor(method, raw string, seen map[string]struct{}) (string, error) {
	cursor := strings.TrimSpace(raw)
	if cursor == "" {
		return "", nil
	}

	if _, ok := seen[cursor]; ok {
		return "", fmt.Errorf("%s returned repeated nextCursor %q", method, cursor)
	}

	seen[cursor] = struct{}{}

	return cursor, nil
}

func checkDiscoveryPageLimit(method string, page, limit int) error {
	if limit <= 0 || page <= limit {
		return nil
	}

	return fmt.Errorf("%s exceeded discovery page limit %d", method, limit)
}

// ListResources refreshes and returns resources discovered from the server.
func (s *Session) ListResources(ctx context.Context) ([]Resource, error) {
	if err := s.requireInitialized(); err != nil {
		return nil, err
	}

	if err := s.requireCapability("resources", "resources/list"); err != nil {
		return nil, err
	}

	var all []Resource
	cursor := ""
	pageCount := 0
	seen := make(map[string]struct{})
	seenCursors := make(map[string]struct{})

	for {
		pageCount++
		if err := checkDiscoveryPageLimit("resources/list", pageCount, s.opts.MaxDiscoveryPages); err != nil {
			return nil, err
		}

		var params any
		if cursor != "" {
			params = map[string]any{"cursor": cursor}
		}

		response, err := s.invoke(ctx, Request{Method: "resources/list", Params: params}, false)
		if err != nil {
			return nil, err
		}

		page, err := parseListResourcesResult(response.Result)
		if err != nil {
			return nil, err
		}

		all, err = appendListResourcesPage(all, page, seen)
		if err != nil {
			return nil, err
		}

		nextCursor, err := nextPaginationCursor("resources/list", page.NextCursor, seenCursors)
		if err != nil {
			return nil, err
		}

		if nextCursor == "" {
			break
		}

		cursor = nextCursor
	}

	s.initMu.Lock()
	s.resources = append([]Resource(nil), all...)
	s.initMu.Unlock()

	return append([]Resource(nil), all...), nil
}

func appendListResourcesPage(all []Resource, page ListResourcesResult, seen map[string]struct{}) ([]Resource, error) {
	for i, resource := range page.Resources {
		uri := strings.TrimSpace(resource.URI)
		if uri == "" {
			return nil, fmt.Errorf("resources/list returned resource %d with empty uri", i)
		}

		if strings.TrimSpace(resource.Name) == "" {
			return nil, fmt.Errorf("resources/list returned resource %q with empty name", uri)
		}

		if _, ok := seen[uri]; ok {
			return nil, fmt.Errorf("resources/list returned duplicate resource %q", uri)
		}

		resource.URI = uri
		resource.Name = strings.TrimSpace(resource.Name)
		seen[uri] = struct{}{}
		all = append(all, resource)
	}

	return all, nil
}

// ListPrompts refreshes and returns prompts discovered from the server.
func (s *Session) ListPrompts(ctx context.Context) ([]Prompt, error) {
	if err := s.requireInitialized(); err != nil {
		return nil, err
	}

	if err := s.requireCapability("prompts", "prompts/list"); err != nil {
		return nil, err
	}

	var all []Prompt
	cursor := ""
	pageCount := 0
	seen := make(map[string]struct{})
	seenCursors := make(map[string]struct{})

	for {
		pageCount++
		if err := checkDiscoveryPageLimit("prompts/list", pageCount, s.opts.MaxDiscoveryPages); err != nil {
			return nil, err
		}

		var params any
		if cursor != "" {
			params = map[string]any{"cursor": cursor}
		}

		response, err := s.invoke(ctx, Request{Method: "prompts/list", Params: params}, false)
		if err != nil {
			return nil, err
		}

		page, err := parseListPromptsResult(response.Result)
		if err != nil {
			return nil, err
		}

		all, err = appendListPromptsPage(all, page, seen)
		if err != nil {
			return nil, err
		}

		nextCursor, err := nextPaginationCursor("prompts/list", page.NextCursor, seenCursors)
		if err != nil {
			return nil, err
		}

		if nextCursor == "" {
			break
		}

		cursor = nextCursor
	}

	s.initMu.Lock()
	s.prompts = append([]Prompt(nil), all...)
	s.initMu.Unlock()

	return append([]Prompt(nil), all...), nil
}

func appendListPromptsPage(all []Prompt, page ListPromptsResult, seen map[string]struct{}) ([]Prompt, error) {
	for i, prompt := range page.Prompts {
		name := strings.TrimSpace(prompt.Name)
		if name == "" {
			return nil, fmt.Errorf("prompts/list returned prompt %d with empty name", i)
		}

		if _, ok := seen[name]; ok {
			return nil, fmt.Errorf("prompts/list returned duplicate prompt %q", name)
		}

		for j := range prompt.Arguments {
			argumentName := strings.TrimSpace(prompt.Arguments[j].Name)
			if argumentName == "" {
				return nil, fmt.Errorf("prompts/list returned prompt %q argument %d with empty name", name, j)
			}

			prompt.Arguments[j].Name = argumentName
		}

		prompt.Name = name
		seen[name] = struct{}{}
		all = append(all, prompt)
	}

	return all, nil
}

// Tools returns the last discovered tool list.
func (s *Session) Tools() []Tool {
	if s == nil {
		return nil
	}

	s.initMu.RLock()
	defer s.initMu.RUnlock()

	return append([]Tool(nil), s.tools...)
}

// Resources returns the last discovered resource list.
func (s *Session) Resources() []Resource {
	if s == nil {
		return nil
	}

	s.initMu.RLock()
	defer s.initMu.RUnlock()

	return append([]Resource(nil), s.resources...)
}

// Prompts returns the last discovered prompt list.
func (s *Session) Prompts() []Prompt {
	if s == nil {
		return nil
	}

	s.initMu.RLock()
	defer s.initMu.RUnlock()

	return append([]Prompt(nil), s.prompts...)
}

// InitializeResult returns the negotiated initialize result.
func (s *Session) InitializeResult() InitializeResult {
	if s == nil {
		return InitializeResult{}
	}

	s.initMu.RLock()
	defer s.initMu.RUnlock()

	return s.initializeResult
}

// Ping checks that the initialized server is still responsive.
func (s *Session) Ping(ctx context.Context) error {
	_, err := s.Invoke(ctx, Request{Method: "ping"})
	return err
}

// Health performs a ping-based health check and includes recent stderr for diagnostics.
func (s *Session) Health(ctx context.Context) HealthStatus {
	if s == nil {
		return HealthStatus{Error: "nil session"}
	}

	status := HealthStatus{Server: strings.TrimSpace(s.server.Name)}
	status.Stderr = s.Stderr()
	if ctx == nil {
		status.Error = "invoke mcp server: context is required"
		return status
	}

	status.Running = s.running()
	status.Initialized = s.isInitialized()
	if !status.Running {
		status.Error = "server process is not running"
		return status
	}

	if !status.Initialized {
		status.Error = "session is not initialized"
		return status
	}

	if err := s.requireTransportOpen(); err != nil {
		status.Error = err.Error()
		status.Stderr = s.Stderr()
		return status
	}

	healthCtx, cancelHealth := s.healthContext(ctx)
	defer cancelHealth()

	if err := s.Ping(healthCtx); err != nil {
		status.Error = err.Error()
		status.Stderr = s.Stderr()
		return status
	}

	status.Healthy = true
	status.Stderr = s.Stderr()
	return status
}

// Stderr returns captured server stderr diagnostics, bounded by MaxStderrBytes.
func (s *Session) Stderr() string {
	if s == nil || s.stderr == nil {
		return ""
	}

	return strings.TrimSpace(redactServerSecretValues(s.stderr.String(), s.secrets))
}

// Close gracefully shuts down the MCP stdio transport. MCP stdio does not define
// a shutdown RPC; closing server stdin is the portable shutdown signal, followed
// by process termination if the server does not stop in time.
func (s *Session) Close(ctx context.Context) error {
	if s == nil {
		return nil
	}

	if ctx == nil {
		return errors.New("close mcp session: context is required")
	}

	s.startMu.Lock()
	defer s.startMu.Unlock()

	return s.close(ctx)
}

func (s *Session) close(ctx context.Context) error {
	s.closeMu.Lock()
	defer s.closeMu.Unlock()

	s.stateMu.Lock()
	if s.closed {
		s.stateMu.Unlock()
		return nil
	}

	if !s.started && s.cmd == nil && s.invocation == nil {
		s.closed = true
		s.stateMu.Unlock()
		return nil
	}

	s.closing = true
	stdin := s.stdin
	cmd := s.cmd
	invocation := s.invocation
	cancelProc := s.cancelProc
	shutdownTimeout := s.opts.ShutdownTimeout
	s.stateMu.Unlock()

	var closeErr error
	if stdin != nil {
		s.writeMu.Lock()
		closeErr = stdin.Close()
		s.stateMu.Lock()
		if s.stdin == stdin {
			s.stdin = nil
		}
		s.stateMu.Unlock()
		s.writeMu.Unlock()
	}

	forced, waitErr, stopErr := s.waitForShutdown(ctx, cmd, shutdownTimeout)
	if stopErr != nil {
		closeErr = errors.Join(closeErr, stopErr)
	}

	if cancelProc != nil {
		cancelProc()
	}

	s.failPending(errors.New("mcp session closed"))

	s.stateMu.Lock()
	s.closed = true
	s.started = false
	s.stateMu.Unlock()

	stderrText := s.Stderr()
	var finishErr error
	if invocation != nil {
		finishErr = invocation.Finish(shell.FinishOptions{
			Stderr:        stderrText,
			Error:         waitErr,
			OutputCapture: shell.OutputNotCaptured,
			OutputNote:    outputNoteForShutdown(forced),
		})
	}

	if closeErr != nil && !errors.Is(closeErr, os.ErrClosed) {
		return withProcessOutput(fmt.Errorf("close mcp server %q: %w", strings.TrimSpace(s.server.Name), closeErr), stderrText)
	}

	if finishErr != nil {
		return fmt.Errorf("audit mcp server %q: %w", strings.TrimSpace(s.server.Name), finishErr)
	}

	if waitErr != nil && !forced {
		return withProcessOutput(fmt.Errorf("mcp server %q exited: %w", strings.TrimSpace(s.server.Name), waitErr), stderrText)
	}

	return nil
}

// Invoke starts a short-lived session, performs the MCP lifecycle, sends one
// request, and then cleanly shuts the process down. A positive timeout applies
// to initialization and request execution; pass 0 to rely on ctx only.
func Invoke(ctx context.Context, server Server, request Request, timeout time.Duration) (*Response, error) {
	return InvokeWithOptions(ctx, server, request, timeout, SessionOptions{})
}

// InvokeWithOptions starts a short-lived session using opts, performs the MCP
// lifecycle, sends one request, and then cleanly shuts the process down. A
// positive timeout applies to initialization and request execution; pass 0 to
// rely on ctx only.
func InvokeWithOptions(ctx context.Context, server Server, request Request, timeout time.Duration, opts SessionOptions) (*Response, error) {
	if err := requireInvokeContext(ctx); err != nil {
		return nil, err
	}

	closeCtx := newShutdownContext(ctx)
	if err := server.Validate(); err != nil {
		return nil, fmt.Errorf("invoke mcp server: %w", err)
	}

	if err := request.Validate(); err != nil {
		return nil, fmt.Errorf("invoke mcp server %q: %w", strings.TrimSpace(server.Name), err)
	}

	if timeout > 0 {
		var cancel context.CancelFunc

		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	session := NewSession(server, opts)
	if err := session.Start(ctx); err != nil {
		return nil, fmt.Errorf("invoke mcp server: %w", err)
	}

	response, err := session.Invoke(ctx, request)
	closeErr := session.Close(closeCtx)
	if err != nil {
		return response, err
	}

	if closeErr != nil {
		return nil, closeErr
	}

	return response, nil
}

// CallTool starts a short-lived session and invokes the MCP tools/call method for toolName.
func CallTool(ctx context.Context, server Server, toolName string, arguments map[string]any, timeout time.Duration) (*Response, error) {
	return CallToolWithOptions(ctx, server, toolName, arguments, timeout, SessionOptions{})
}

// CallToolWithOptions starts a short-lived session using opts and invokes the
// MCP tools/call method for toolName.
func CallToolWithOptions(
	ctx context.Context,
	server Server,
	toolName string,
	arguments map[string]any,
	timeout time.Duration,
	opts SessionOptions,
) (*Response, error) {
	toolName = strings.TrimSpace(toolName)
	if toolName == "" {
		return nil, errors.New("call mcp tool: missing tool name")
	}

	if err := requireInvokeContext(ctx); err != nil {
		return nil, err
	}

	closeCtx := newShutdownContext(ctx)
	if err := server.Validate(); err != nil {
		return nil, fmt.Errorf("call mcp tool: %w", err)
	}

	if timeout > 0 {
		var cancel context.CancelFunc

		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	session := NewSession(server, opts)
	if err := session.Start(ctx); err != nil {
		return nil, fmt.Errorf("call mcp tool: %w", err)
	}

	response, err := session.CallTool(ctx, toolName, arguments)
	closeErr := session.Close(closeCtx)
	if err != nil {
		return response, err
	}

	if closeErr != nil {
		return nil, closeErr
	}

	return response, nil
}

//nolint:cyclop,gocognit // Request validation, write, response, and cancellation paths stay together.
func (s *Session) invoke(ctx context.Context, request Request, validate bool) (*Response, error) {
	if s == nil {
		return nil, errors.New("invoke mcp server: nil session")
	}

	if err := requireInvokeContext(ctx); err != nil {
		return nil, err
	}

	if err := request.Validate(); err != nil {
		return nil, fmt.Errorf("invoke mcp server %q: %w", strings.TrimSpace(s.server.Name), err)
	}

	if validate {
		if err := s.requireInitialized(); err != nil {
			return nil, err
		}

		if err := s.validateRequest(ctx, request); err != nil {
			return nil, err
		}
	}

	request.JSONRPC = "2.0"
	if request.ID == nil {
		request.ID = s.nextRequestID()
	}

	key, err := jsonIDKey(request.ID)
	if err != nil {
		return nil, fmt.Errorf("invoke mcp server %q: invalid request id: %w", strings.TrimSpace(s.server.Name), err)
	}

	responseCh := make(chan responseResult, 1)
	if err := s.registerPending(key, responseCh); err != nil {
		return nil, err
	}

	if err := s.writeMessage(ctx, request); err != nil {
		s.unregisterPending(key)
		return nil, withProcessOutput(fmt.Errorf("write request to mcp server %q: %w", strings.TrimSpace(s.server.Name), err), s.Stderr())
	}

	select {
	case result := <-responseCh:
		if result.err != nil {
			return nil, withProcessOutput(fmt.Errorf("read response from mcp server %q: %w", strings.TrimSpace(s.server.Name), result.err), s.Stderr())
		}

		response := result.response
		if response.Error != nil {
			response.Error.Message = redactServerSecretValues(response.Error.Message, s.secrets)
			return response, withProcessOutput(fmt.Errorf("mcp server %q returned error %d: %s", strings.TrimSpace(s.server.Name), response.Error.Code, response.Error.Message), s.Stderr())
		}

		return response, nil
	case <-ctx.Done():
		s.retirePending(key)
		if request.Method != "initialize" {
			if err := s.sendCancelled(request.ID, ctx.Err()); err != nil {
				// The caller's context is already done; cancellation notification
				// delivery is best-effort and must not mask the original error.
				_ = err.Error()
			}
		}

		return nil, withProcessOutput(fmt.Errorf("mcp server %q timed out or was canceled: %w", strings.TrimSpace(s.server.Name), ctx.Err()), s.Stderr())
	}
}

func (s *Session) notify(ctx context.Context, method string, params any) error {
	if strings.TrimSpace(method) == "" {
		return errors.New("missing notification method")
	}

	//nolint:govet // JSON-RPC notification envelope field order is intentional.
	notification := struct {
		JSONRPC string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  any    `json:"params,omitempty"`
	}{
		JSONRPC: "2.0",
		Method:  strings.TrimSpace(method),
		Params:  params,
	}

	return s.writeMessage(ctx, notification)
}

func (s *Session) sendCancelled(requestID any, reason error) error {
	params := map[string]any{"requestId": requestID}
	if reason != nil {
		params["reason"] = reason.Error()
	}

	//nolint:govet // JSON-RPC notification envelope field order is intentional.
	notification := struct {
		JSONRPC string         `json:"jsonrpc"`
		Method  string         `json:"method"`
		Params  map[string]any `json:"params"`
	}{
		JSONRPC: "2.0",
		Method:  "notifications/cancelled",
		Params:  params,
	}

	return s.writeMessageNoContext(notification)
}

func (s *Session) validateRequest(ctx context.Context, request Request) error {
	method := strings.TrimSpace(request.Method)
	if managedLifecycleMethod(method) {
		return fmt.Errorf("mcp lifecycle method %q is managed by the session", method)
	}

	if notificationMethod(method) {
		return fmt.Errorf("mcp notification method %q cannot be invoked as a request", method)
	}

	if clientSideMethod(method) {
		return fmt.Errorf("mcp client-side method %q cannot be invoked against a server", method)
	}

	if capability := requiredCapabilityForMethod(method); capability != "" {
		if err := s.requireCapability(capability, method); err != nil {
			return err
		}
	}

	if method != "tools/call" {
		return nil
	}

	params, err := decodeCallToolParams(request.Params)
	if err != nil {
		return err
	}

	if err := s.validateToolCall(params); err != nil {
		return err
	}

	return s.authorizeToolCall(ctx, params)
}

func managedLifecycleMethod(method string) bool {
	switch method {
	case "initialize", "notifications/initialized":
		return true
	default:
		return false
	}
}

func notificationMethod(method string) bool {
	return strings.HasPrefix(method, "notifications/")
}

func clientSideMethod(method string) bool {
	switch {
	case strings.HasPrefix(method, "roots/"):
		return true
	case strings.HasPrefix(method, "sampling/"):
		return true
	case strings.HasPrefix(method, "elicitation/"):
		return true
	default:
		return false
	}
}

func requiredCapabilityForMethod(method string) string {
	switch {
	case strings.HasPrefix(method, "tools/"):
		return "tools"
	case strings.HasPrefix(method, "resources/"):
		return "resources"
	case strings.HasPrefix(method, "prompts/"):
		return "prompts"
	case strings.HasPrefix(method, "logging/"):
		return "logging"
	case strings.HasPrefix(method, "completion/"):
		return "completions"
	case strings.HasPrefix(method, "tasks/"):
		return "tasks"
	default:
		return ""
	}
}

func (s *Session) validateToolCall(params CallToolParams) error {
	name := strings.TrimSpace(params.Name)
	if name == "" {
		return errors.New("call mcp tool: missing tool name")
	}

	s.initMu.RLock()
	tool, ok := s.toolIndex[name]
	s.initMu.RUnlock()
	if !ok {
		return fmt.Errorf("call mcp tool %q: tool was not discovered by tools/list", name)
	}

	if err := validateToolArguments(tool, params.Arguments); err != nil {
		return fmt.Errorf("call mcp tool %q: %w", name, err)
	}

	return nil
}

func (s *Session) authorizeToolCall(ctx context.Context, params CallToolParams) error {
	toolName := strings.TrimSpace(params.Name)
	decision := permission.Evaluate(ctx, s.opts.Permission, permission.Request{
		Operations: []permission.Operation{mcpToolPermissionOperation(s.server, toolName)},
		Action:     mcpToolPermissionAction(s.server.Name, toolName),
		Source:     mcpPermissionSource(s.server.Name),
		Target:     toolName,
	})
	if decision.Allowed {
		return nil
	}

	return &permission.Error{Decision: decision}
}

func (s *Session) requireCapability(capability, method string) error {
	s.initMu.RLock()
	caps := s.initializeResult.Capabilities
	s.initMu.RUnlock()

	if !caps.Has(capability) {
		return fmt.Errorf("mcp server %q does not advertise %q capability required for %s", strings.TrimSpace(s.server.Name), capability, method)
	}

	return nil
}

func (s *Session) validateDeclaredCapabilities(caps ServerCapabilities) error {
	for _, capability := range s.server.Capabilities {
		capability = strings.TrimSpace(capability)
		if !knownProtocolCapability(capability) {
			continue
		}

		if !caps.Has(capability) {
			return fmt.Errorf("mcp server %q declares %q capability but initialize negotiated capabilities %s", strings.TrimSpace(s.server.Name), capability, formatNegotiatedCapabilities(caps))
		}
	}

	return nil
}

func (s *Session) requireInitialized() error {
	s.stateMu.Lock()
	started := s.started
	closed := s.closed
	initialized := s.initialized
	s.stateMu.Unlock()

	if !started || closed {
		return fmt.Errorf("mcp server %q session is not started", strings.TrimSpace(s.server.Name))
	}

	if !initialized {
		return fmt.Errorf("mcp server %q session is not initialized", strings.TrimSpace(s.server.Name))
	}

	select {
	case <-s.waitDone:
		return fmt.Errorf("mcp server %q session is not running", strings.TrimSpace(s.server.Name))
	default:
	}

	if err := s.requireTransportOpen(); err != nil {
		return err
	}

	return nil
}

func (s *Session) isInitialized() bool {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()

	return s.initialized && s.started && !s.closed
}

func (s *Session) running() bool {
	if s == nil {
		return false
	}

	s.stateMu.Lock()
	started := s.started && !s.closed
	s.stateMu.Unlock()
	if !started {
		return false
	}

	select {
	case <-s.waitDone:
		return false
	default:
		return true
	}
}

func (s *Session) initializeContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if s.opts.InitializeTimeout <= 0 {
		return ctx, func() {}
	}

	return context.WithTimeout(ctx, s.opts.InitializeTimeout)
}

func (s *Session) healthContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if s.opts.HealthTimeout <= 0 {
		return ctx, func() {}
	}

	return context.WithTimeout(ctx, s.opts.HealthTimeout)
}

func (s *Session) auditContext() shell.AuditContext {
	audit := s.opts.Audit
	if strings.TrimSpace(audit.Caller) == "" {
		audit.Caller = "atteler.mcp." + strings.TrimSpace(s.server.Name)
	}

	return audit
}

func mcpServerEnv(env map[string]string, autonomy string) map[string]string {
	autonomy = strings.TrimSpace(autonomy)
	if autonomy == "" {
		return env
	}

	merged := maps.Clone(env)
	if merged == nil {
		merged = make(map[string]string, 1)
	}

	merged["ATTELER_AUTONOMY"] = autonomy

	return merged
}

func mcpShellPolicy(policy *shell.Policy, env map[string]string) *shell.Policy {
	allowedEnv := explicitServerEnvNames(env)
	if len(allowedEnv) == 0 {
		return policy
	}

	effective := shell.DefaultPolicy()
	if policy != nil {
		effective = *policy
	}

	effective.AllowCredentialEnv = append(append([]string(nil), effective.AllowCredentialEnv...), allowedEnv...)

	return &effective
}

func explicitServerEnvNames(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}

	names := make([]string, 0, len(env))
	for name := range env {
		if strings.TrimSpace(name) != "" {
			names = append(names, name)
		}
	}

	return names
}

func explicitServerSecretValues(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}

	values := make([]string, 0, len(env))
	for name, value := range env {
		value = strings.TrimSpace(value)
		if value != "" && mcpCredentialEnvName(name) {
			values = append(values, value)
		}
	}

	return values
}

func mcpCredentialEnvName(name string) bool {
	name = strings.ToUpper(strings.TrimSpace(name))
	for _, marker := range []string{"TOKEN", "SECRET", "KEY", "AUTH", "PASSWORD", "PASSWD", "COOKIE", "CREDENTIAL", "PRIVATE"} {
		if strings.Contains(name, marker) {
			return true
		}
	}

	return false
}

func redactServerSecretValues(text string, secrets []string) string {
	for _, secret := range secrets {
		secret = strings.TrimSpace(secret)
		if secret == "" {
			continue
		}

		text = strings.ReplaceAll(text, secret, "<redacted:mcp_server_env>")
	}

	return text
}

func mcpPermissionOperations(server Server) []permission.Operation {
	return []permission.Operation{mcpServerPermissionOperation(server)}
}

func mcpServerPermissionOperation(server Server) permission.Operation {
	name := strings.TrimSpace(server.Name)

	metadata := map[string]string{"server": name}
	if name == "" {
		metadata = nil
	}

	return permission.Operation{
		Kind:     permission.OperationExecute,
		Action:   mcpServerPermissionAction(name),
		Target:   strings.TrimSpace(server.Command),
		Source:   mcpPermissionSource(name),
		Metadata: metadata,
	}
}

func mcpToolPermissionOperation(server Server, toolName string) permission.Operation {
	serverName := strings.TrimSpace(server.Name)
	toolName = strings.TrimSpace(toolName)
	metadata := map[string]string{"server": serverName, "tool": toolName}
	for key, value := range metadata {
		if value == "" {
			delete(metadata, key)
		}
	}

	return permission.Operation{
		Kind:     permission.OperationExecute,
		Action:   mcpToolPermissionAction(serverName, toolName),
		Target:   toolName,
		Source:   mcpPermissionSource(serverName),
		Metadata: metadata,
	}
}

func mcpServerPermissionAction(serverName string) string {
	serverName = strings.TrimSpace(serverName)
	if serverName != "" {
		return "mcp server " + serverName
	}

	return "mcp server"
}

func mcpToolPermissionAction(serverName, toolName string) string {
	serverName = strings.TrimSpace(serverName)
	toolName = strings.TrimSpace(toolName)
	switch {
	case serverName != "" && toolName != "":
		return "mcp tool " + serverName + "/" + toolName
	case toolName != "":
		return "mcp tool " + toolName
	case serverName != "":
		return "mcp tool " + serverName
	default:
		return "mcp tool"
	}
}

func mcpPermissionSource(serverName string) string {
	source := "atteler.mcp"
	if serverName = strings.TrimSpace(serverName); serverName != "" {
		source += "." + serverName
	}

	return source
}

func (s *Session) nextRequestID() int64 {
	return atomic.AddInt64(&s.requestSeq, 1)
}

func (s *Session) registerPending(key string, ch chan responseResult) error {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()

	select {
	case <-s.readDone:
		return s.transportStoppedError()
	default:
	}

	if _, ok := s.pending[key]; ok {
		return fmt.Errorf("duplicate mcp request id %s", key)
	}

	if _, ok := s.retired[key]; ok {
		return fmt.Errorf("mcp request id %s was canceled and cannot be reused until its stale response is discarded", key)
	}

	s.pending[key] = ch
	return nil
}

func (s *Session) requireTransportOpen() error {
	select {
	case <-s.readDone:
		return s.transportStoppedError()
	default:
		return nil
	}
}

func (s *Session) transportStoppedError() error {
	s.readMu.Lock()
	err := s.readErr
	s.readMu.Unlock()

	if err != nil {
		return fmt.Errorf("mcp server %q transport stopped: %w", strings.TrimSpace(s.server.Name), err)
	}

	return fmt.Errorf("mcp server %q transport stopped", strings.TrimSpace(s.server.Name))
}

func (s *Session) unregisterPending(key string) {
	s.pendingMu.Lock()
	delete(s.pending, key)
	s.pendingMu.Unlock()
}

func (s *Session) retirePending(key string) {
	s.pendingMu.Lock()
	delete(s.pending, key)
	s.retired[key] = struct{}{}
	s.pendingMu.Unlock()
}

func (s *Session) deliverResponse(response *Response) {
	key, err := jsonIDKey(response.ID)
	if err != nil {
		return
	}

	s.pendingMu.Lock()
	ch, ok := s.pending[key]
	if ok {
		delete(s.pending, key)
	} else {
		delete(s.retired, key)
	}
	s.pendingMu.Unlock()

	if ok {
		ch <- responseResult{response: response}
	}
}

func (s *Session) failPending(err error) {
	if err == nil {
		return
	}

	s.pendingMu.Lock()
	pending := s.pending
	s.pending = make(map[string]chan responseResult)
	s.pendingMu.Unlock()

	for _, ch := range pending {
		ch <- responseResult{err: err}
	}
}

func (s *Session) writeMessage(ctx context.Context, message any) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("message context: %w", err)
	}

	return s.writeMessageNoContext(message)
}

func (s *Session) writeMessageNoContext(message any) error {
	encoded, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("marshal json-rpc message: %w", err)
	}

	encoded = append(encoded, '\n')

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	s.stateMu.Lock()
	stdin := s.stdin
	closed := s.closed || s.closing
	s.stateMu.Unlock()

	if stdin == nil || closed {
		return errors.New("mcp server stdin is closed")
	}

	if _, err := stdin.Write(encoded); err != nil {
		return fmt.Errorf("write newline-delimited json: %w", err)
	}

	return nil
}

func (s *Session) readLoop(r io.Reader) {
	err := s.scanResponses(r)
	s.readMu.Lock()
	s.readErr = err
	close(s.readDone)
	s.readMu.Unlock()

	if err != nil {
		s.failPending(err)
	}
}

func (s *Session) scanResponses(r io.Reader) error {
	reader := bufio.NewReaderSize(r, 64*1024)

	for {
		line, oversized, readErr := readIncomingLine(reader)
		if oversized {
			s.discardOversizedMessage()
		} else if err := s.handleIncomingLine(bytes.TrimSpace(line)); err != nil {
			return err
		}

		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}

			return fmt.Errorf("scan response: %w", readErr)
		}
	}

	if s.hasPending() {
		return io.ErrUnexpectedEOF
	}

	return nil
}

// readIncomingLine reads one newline-terminated message, accumulating at most
// maxIncomingMessageBytes. Oversized lines are drained and reported via the
// second return value instead of failing the reader: bufio.Scanner cannot
// continue after bufio.ErrTooLong, while a plain reader keeps the transport
// alive for subsequent messages.
func readIncomingLine(reader *bufio.Reader) (line []byte, oversized bool, err error) {
	for {
		chunk, readErr := reader.ReadSlice('\n')
		if !oversized {
			line = append(line, chunk...)
			if len(line) > maxIncomingMessageBytes {
				line = nil
				oversized = true
			}
		}

		if readErr == nil {
			return line, oversized, nil
		}

		if errors.Is(readErr, bufio.ErrBufferFull) {
			continue
		}

		if errors.Is(readErr, io.EOF) {
			return line, oversized, io.EOF
		}

		return line, oversized, fmt.Errorf("read newline-delimited json: %w", readErr)
	}
}

// handleIncomingLine dispatches one stdout line. Lines that are not JSON
// objects (stray prints from the server or its children) are recorded and
// skipped so one junk line does not destroy a healthy long-lived transport;
// teardown is reserved for malformed JSON-RPC envelopes, I/O errors, and EOF.
func (s *Session) handleIncomingLine(line []byte) error {
	if len(line) == 0 {
		return nil
	}

	if line[0] != '{' || !json.Valid(line) {
		s.recordSkippedStdoutLine(line)
		return nil
	}

	message, err := decodeIncomingMessage(line)
	if err != nil {
		return err
	}

	if message.Method != "" {
		if len(message.Result) > 0 || message.Error != nil {
			return fmt.Errorf("malformed json-rpc message: method %q cannot appear on a response", message.Method)
		}

		s.handleServerMessage(message)
		return nil
	}

	response := Response{
		JSONRPC: message.JSONRPC,
		ID:      message.ID,
		Result:  message.Result,
		Error:   message.Error,
	}
	if err := validateResponse(response); err != nil {
		return err
	}

	s.deliverResponse(&response)

	return nil
}

// recordSkippedStdoutLine keeps a bounded diagnostic for a skipped stdout line
// so the junk surfaces through Stderr() and error annotations.
func (s *Session) recordSkippedStdoutLine(line []byte) {
	const previewBytes = 256

	preview := string(line)
	if len(preview) > previewBytes {
		preview = preview[:previewBytes] + "..."
	}

	_, _ = fmt.Fprintf(s.stderr, "atteler: skipped malformed mcp stdout line (%d bytes): %s\n", len(line), preview)
}

// discardOversizedMessage drops a single JSON-RPC message larger than
// maxIncomingMessageBytes and fails the requests currently awaiting a
// response, since the discarded message may have carried one of their
// results. The transport itself stays open for subsequent calls.
func (s *Session) discardOversizedMessage() {
	err := fmt.Errorf(
		"mcp server %q sent a json-rpc message larger than %d bytes; the message was discarded and the session remains usable",
		strings.TrimSpace(s.server.Name), maxIncomingMessageBytes,
	)
	_, _ = fmt.Fprintf(s.stderr, "atteler: %v\n", err)
	s.failPending(err)
}

func (s *Session) handleServerMessage(message incomingMessage) {
	if message.ID == nil {
		return
	}

	method := strings.TrimSpace(message.Method)
	response := Response{JSONRPC: "2.0", ID: message.ID}
	if method == "ping" {
		response.Result = json.RawMessage(`{}`)
	} else {
		response.Error = &ResponseError{Code: -32601, Message: "method not found"}
	}

	if err := s.writeMessageNoContext(response); err != nil {
		return
	}
}

func (s *Session) hasPending() bool {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()

	return len(s.pending) > 0
}

func (s *Session) busyButReusable() bool {
	return s != nil && s.pendingCount() > 0 && s.running() && s.isInitialized() && s.requireTransportOpen() == nil
}

func (s *Session) pendingCount() int {
	if s == nil {
		return 0
	}

	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()

	return len(s.pending)
}

func (s *Session) captureStderr(r io.Reader) {
	defer close(s.stderrDone)

	if _, err := io.Copy(s.stderr, r); err != nil {
		return
	}
}

func (s *Session) waitForProcess(cmd *exec.Cmd) {
	<-s.readDone
	<-s.stderrDone

	var err error
	if cmd != nil {
		err = cmd.Wait()
	}

	s.waitMu.Lock()
	s.waitErr = err
	close(s.waitDone)
	s.waitMu.Unlock()
}

func (s *Session) waitForShutdown(ctx context.Context, cmd *exec.Cmd, timeout time.Duration) (bool, error, error) {
	if cmd == nil {
		return false, nil, nil
	}

	select {
	case <-s.waitDone:
		return false, s.processWaitErr(), nil
	default:
	}

	if waitForDone(ctx, s.waitDone, timeout) {
		return false, s.processWaitErr(), nil
	}

	forced := true
	var stopErr error
	if cmd.Process != nil {
		if err := terminateProcess(cmd.Process); err != nil && !errors.Is(err, os.ErrProcessDone) {
			stopErr = errors.Join(stopErr, fmt.Errorf("terminate mcp server process: %w", err))
		}
	}

	if waitForDone(ctx, s.waitDone, timeout) {
		return forced, s.processWaitErr(), stopErr
	}

	if cmd.Process != nil {
		if err := killProcess(cmd.Process); err != nil && !errors.Is(err, os.ErrProcessDone) {
			stopErr = errors.Join(stopErr, fmt.Errorf("kill mcp server process: %w", err))
		}
	}

	if waitForDone(ctx, s.waitDone, timeout) {
		return forced, s.processWaitErr(), stopErr
	}

	// A surviving grandchild can still hold the inherited stdout/stderr write
	// ends even after SIGKILL. Abandon the parent's read ends so the reader
	// goroutines unblock and waitForProcess can finish.
	s.abandonPipes()

	if waitForDone(ctx, s.waitDone, timeout) {
		return forced, s.processWaitErr(), stopErr
	}

	stopErr = errors.Join(stopErr, fmt.Errorf(
		"mcp server %q did not stop within shutdown timeout %s after kill; abandoning process wait",
		strings.TrimSpace(s.server.Name), timeout,
	))

	return forced, nil, stopErr
}

// abandonPipes closes the parent's stdout/stderr pipe read ends. Close must
// never block forever on a grandchild that inherited the server's pipes and
// outlived it, so shutdown gives up on draining them once the process has
// been signaled and the shutdown timeout has elapsed.
func (s *Session) abandonPipes() {
	s.stateMu.Lock()
	stdout := s.stdoutPipe
	stderr := s.stderrPipe
	s.stdoutPipe = nil
	s.stderrPipe = nil
	s.stateMu.Unlock()

	if stdout != nil {
		_ = stdout.Close()
	}

	if stderr != nil {
		_ = stderr.Close()
	}
}

func (s *Session) processWaitErr() error {
	s.waitMu.Lock()
	defer s.waitMu.Unlock()

	return s.waitErr
}

func waitForDone(ctx context.Context, done <-chan struct{}, timeout time.Duration) bool {
	if timeout <= 0 {
		select {
		case <-done:
			return true
		default:
			return false
		}
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	case <-timer.C:
		return false
	}
}

func outputNoteForShutdown(forced bool) string {
	if forced {
		return "MCP JSON-RPC protocol output was not captured; server required termination during shutdown"
	}

	return "MCP JSON-RPC protocol output was not captured"
}

// Validate checks the server fields required to invoke a configured MCP server.
func (s Server) Validate() error {
	name := strings.TrimSpace(s.Name)
	if name == "" {
		return errors.New("missing name")
	}

	if strings.TrimSpace(s.Command) == "" {
		return fmt.Errorf("server %q: missing command", name)
	}

	if err := validateCapabilities(name, s.Capabilities); err != nil {
		return err
	}

	return nil
}

// Validate checks the request fields required for JSON-RPC invocation.
func (r Request) Validate() error {
	if strings.TrimSpace(r.Method) == "" {
		return errors.New("missing method")
	}

	return nil
}

func parseInitializeResult(raw json.RawMessage) (InitializeResult, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return InitializeResult{}, fmt.Errorf("decode initialize result: %w", err)
	}

	capabilitiesRaw, ok := fields["capabilities"]
	if !ok {
		return InitializeResult{}, errors.New("initialize result missing capabilities")
	}

	if !jsonValueIsObject(capabilitiesRaw) {
		return InitializeResult{}, errors.New("initialize result capabilities must be an object")
	}

	var result InitializeResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return InitializeResult{}, fmt.Errorf("decode initialize result: %w", err)
	}

	if strings.TrimSpace(result.ProtocolVersion) == "" {
		return InitializeResult{}, errors.New("initialize result missing protocolVersion")
	}

	if strings.TrimSpace(result.ServerInfo.Name) == "" {
		return InitializeResult{}, errors.New("initialize result missing serverInfo.name")
	}

	if strings.TrimSpace(result.ServerInfo.Version) == "" {
		return InitializeResult{}, errors.New("initialize result missing serverInfo.version")
	}

	return result, nil
}

func parseListToolsResult(raw json.RawMessage) (ListToolsResult, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return ListToolsResult{}, fmt.Errorf("decode tools/list result: %w", err)
	}

	toolsRaw, ok := fields["tools"]
	if !ok {
		return ListToolsResult{}, errors.New("tools/list result missing tools")
	}

	var result ListToolsResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return ListToolsResult{}, fmt.Errorf("decode tools/list result: %w", err)
	}

	if !jsonValueIsArray(toolsRaw) {
		return ListToolsResult{}, errors.New("tools/list result tools must be an array")
	}

	return result, nil
}

func parseListResourcesResult(raw json.RawMessage) (ListResourcesResult, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return ListResourcesResult{}, fmt.Errorf("decode resources/list result: %w", err)
	}

	resourcesRaw, ok := fields["resources"]
	if !ok {
		return ListResourcesResult{}, errors.New("resources/list result missing resources")
	}

	var result ListResourcesResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return ListResourcesResult{}, fmt.Errorf("decode resources/list result: %w", err)
	}

	if !jsonValueIsArray(resourcesRaw) {
		return ListResourcesResult{}, errors.New("resources/list result resources must be an array")
	}

	return result, nil
}

func parseListPromptsResult(raw json.RawMessage) (ListPromptsResult, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return ListPromptsResult{}, fmt.Errorf("decode prompts/list result: %w", err)
	}

	promptsRaw, ok := fields["prompts"]
	if !ok {
		return ListPromptsResult{}, errors.New("prompts/list result missing prompts")
	}

	var result ListPromptsResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return ListPromptsResult{}, fmt.Errorf("decode prompts/list result: %w", err)
	}

	if !jsonValueIsArray(promptsRaw) {
		return ListPromptsResult{}, errors.New("prompts/list result prompts must be an array")
	}

	return result, nil
}

func jsonValueIsArray(raw json.RawMessage) bool {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return false
	}

	_, ok := value.([]any)
	return ok
}

func jsonValueIsObject(raw json.RawMessage) bool {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return false
	}

	_, ok := value.(map[string]any)
	return ok
}

func supportedProtocolVersion(version string) bool {
	return slices.Contains(SupportedProtocolVersions, strings.TrimSpace(version))
}

func knownProtocolCapability(capability string) bool {
	switch capability {
	case "tools", "resources", "prompts", "logging", "completions", "tasks", "experimental":
		return true
	default:
		return false
	}
}

func validateClientCapabilities(caps ClientCapabilities) error {
	for _, capability := range []struct {
		name    string
		enabled bool
	}{
		{name: "experimental", enabled: len(caps.Experimental) > 0},
		{name: "roots", enabled: len(caps.Roots) > 0},
		{name: "sampling", enabled: len(caps.Sampling) > 0},
		{name: "elicitation", enabled: len(caps.Elicitation) > 0},
		{name: "tasks", enabled: len(caps.Tasks) > 0},
	} {
		if capability.enabled {
			return fmt.Errorf("client capability %q is not implemented", capability.name)
		}
	}

	return nil
}

func formatNegotiatedCapabilities(caps ServerCapabilities) string {
	var names []string
	for _, name := range []string{"tools", "resources", "prompts", "logging", "completions", "tasks", "experimental"} {
		if caps.Has(name) {
			names = append(names, name)
		}
	}

	if len(names) == 0 {
		return "[]"
	}

	return "[" + strings.Join(names, ",") + "]"
}

func decodeCallToolParams(params any) (CallToolParams, error) {
	if params == nil {
		return CallToolParams{}, errors.New("tools/call params are required")
	}

	data, err := json.Marshal(params)
	if err != nil {
		return CallToolParams{}, fmt.Errorf("marshal tools/call params: %w", err)
	}

	var decoded CallToolParams
	if err := decodeJSONUseNumber(data, &decoded); err != nil {
		return CallToolParams{}, fmt.Errorf("decode tools/call params: %w", err)
	}

	return decoded, nil
}

func validateDiscoveredToolSchema(tool Tool) error {
	if len(tool.InputSchema) == 0 {
		return errors.New("inputSchema is required")
	}

	for _, field := range []struct {
		name string
		raw  json.RawMessage
	}{
		{name: "inputSchema", raw: tool.InputSchema},
		{name: "outputSchema", raw: tool.OutputSchema},
	} {
		if len(field.raw) == 0 {
			continue
		}

		if !jsonValueIsObject(field.raw) {
			return fmt.Errorf("%s must be a JSON object", field.name)
		}
	}

	inputSchema, err := decodeToolInputSchema(tool.InputSchema)
	if err != nil {
		return err
	}

	if err := validateToolInputSchema(inputSchema); err != nil {
		return err
	}

	return nil
}

func validateToolArguments(tool Tool, arguments map[string]any) error {
	schema, err := decodeToolInputSchema(tool.InputSchema)
	if err != nil {
		return err
	}

	if len(schema) == 0 {
		return nil
	}

	if schemaErr := validateToolInputSchema(schema); schemaErr != nil {
		return schemaErr
	}

	required, err := toolInputSchemaRequired(schema)
	if err != nil {
		return err
	}

	if requiredErr := validateRequiredProperties(required, arguments); requiredErr != nil {
		return requiredErr
	}

	properties, propertiesErr := toolInputSchemaProperties(schema)
	if propertiesErr != nil {
		return propertiesErr
	}

	additional, err := toolInputSchemaAdditionalProperties(schema)
	if err != nil {
		return err
	}

	if err := validateAdditionalProperties(additional, properties, arguments); err != nil {
		return err
	}

	return validateDeclaredProperties(properties, arguments)
}

func decodeToolInputSchema(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 {
		return nil, nil
	}

	var schema map[string]any
	if err := decodeJSONUseNumber(raw, &schema); err != nil {
		return nil, fmt.Errorf("decode input schema: %w", err)
	}

	return schema, nil
}

func validateToolInputSchema(schema map[string]any) error {
	if len(schema) == 0 {
		return nil
	}

	if err := validateToolInputSchemaType(schema); err != nil {
		return err
	}

	if _, err := toolInputSchemaProperties(schema); err != nil {
		return err
	}

	if _, err := toolInputSchemaRequired(schema); err != nil {
		return err
	}

	if _, err := toolInputSchemaAdditionalProperties(schema); err != nil {
		return err
	}

	return nil
}

func validateToolInputSchemaType(schema map[string]any) error {
	raw, exists := schema["type"]
	if !exists || raw == nil {
		return nil
	}

	allowed := schemaTypes(raw)
	if len(allowed) == 0 {
		return errors.New("input schema type must be a string or array of strings")
	}

	if !slices.Contains(allowed, "object") {
		return fmt.Errorf("unsupported input schema type %q", strings.Join(allowed, " or "))
	}

	return nil
}

func toolInputSchemaProperties(schema map[string]any) (map[string]any, error) {
	properties, ok := schema["properties"].(map[string]any)
	if rawProperties, exists := schema["properties"]; exists && !ok && rawProperties != nil {
		return nil, errors.New("input schema properties must be an object")
	}

	return properties, nil
}

func toolInputSchemaRequired(schema map[string]any) ([]string, error) {
	raw, ok := schema["required"]
	if !ok {
		return nil, nil
	}

	items, ok := raw.([]any)
	if !ok {
		return nil, errors.New("input schema required must be an array")
	}

	required := make([]string, 0, len(items))
	seen := make(map[string]struct{}, len(items))
	for i, item := range items {
		name, ok := item.(string)
		name = strings.TrimSpace(name)
		if !ok || name == "" {
			return nil, fmt.Errorf("input schema required item %d must be a non-empty string", i)
		}

		if _, exists := seen[name]; exists {
			return nil, fmt.Errorf("input schema required item %d duplicates %q", i, name)
		}

		seen[name] = struct{}{}
		required = append(required, name)
	}

	return required, nil
}

func toolInputSchemaAdditionalProperties(schema map[string]any) (any, error) {
	additional, ok := schema["additionalProperties"]
	if !ok || additional == nil {
		return nil, nil
	}

	switch additional.(type) {
	case bool, map[string]any:
		return additional, nil
	default:
		return nil, errors.New("input schema additionalProperties must be a boolean or object")
	}
}

func validateDeclaredProperties(properties, arguments map[string]any) error {
	if len(properties) == 0 {
		return nil
	}

	for name, value := range arguments {
		propertySchema, ok := properties[name].(map[string]any)
		if !ok {
			continue
		}

		if err := validateJSONSchemaValue(name, value, propertySchema); err != nil {
			return err
		}
	}

	return nil
}

func validateAdditionalProperties(additional any, properties, arguments map[string]any) error {
	if additional == nil {
		return nil
	}

	for name, value := range arguments {
		if _, declared := properties[name]; declared {
			continue
		}

		switch typed := additional.(type) {
		case bool:
			if !typed {
				return fmt.Errorf("unexpected argument %q", name)
			}
		case map[string]any:
			if err := validateJSONSchemaValue(name, value, typed); err != nil {
				return err
			}
		}
	}

	return nil
}

func validateRequiredProperties(required []string, arguments map[string]any) error {
	for _, name := range required {
		if _, ok := arguments[name]; !ok {
			return fmt.Errorf("missing required argument %q", name)
		}
	}

	return nil
}

func validateJSONSchemaValue(name string, value any, schema map[string]any) error {
	allowed := schemaTypes(schema["type"])
	if len(allowed) == 0 {
		return nil
	}

	for _, schemaType := range allowed {
		if valueMatchesSchemaType(value, schemaType) {
			return nil
		}
	}

	return fmt.Errorf("argument %q has type %s, want %s", name, jsonValueType(value), strings.Join(allowed, " or "))
}

func schemaTypes(raw any) []string {
	switch typed := raw.(type) {
	case string:
		if strings.TrimSpace(typed) == "" {
			return nil
		}

		return []string{strings.TrimSpace(typed)}
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			if text, ok := item.(string); ok && strings.TrimSpace(text) != "" {
				out = append(out, strings.TrimSpace(text))
			}
		}

		return out
	default:
		return nil
	}
}

func valueMatchesSchemaType(value any, schemaType string) bool {
	switch schemaType {
	case "null":
		return value == nil
	case "string":
		_, ok := value.(string)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "number":
		switch value.(type) {
		case float64, float32, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, json.Number:
			return true
		default:
			return false
		}
	case "integer":
		return valueIsInteger(value)
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	default:
		return true
	}
}

func valueIsInteger(value any) bool {
	switch typed := value.(type) {
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return true
	case float64:
		return typed == float64(int64(typed))
	case float32:
		return typed == float32(int64(typed))
	case json.Number:
		_, err := typed.Int64()
		return err == nil
	default:
		return false
	}
}

func jsonValueType(value any) string {
	if value == nil {
		return "null"
	}

	switch value.(type) {
	case string:
		return "string"
	case bool:
		return "boolean"
	case float64, float32, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, json.Number:
		return "number"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	default:
		return fmt.Sprintf("%T", value)
	}
}

func requireInvokeContext(ctx context.Context) error {
	if ctx == nil {
		return errors.New("invoke mcp server: context is required")
	}

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("invoke mcp server: context already done: %w", err)
	}

	return nil
}

func finishMCPSetupError(invocation *shell.Invocation, err error) error {
	if finishErr := invocation.Finish(shell.FinishOptions{
		Error:         err,
		OutputCapture: shell.OutputNotCaptured,
		OutputNote:    "MCP server failed before JSON-RPC streaming",
	}); finishErr != nil {
		return errors.Join(err, finishErr)
	}

	return err
}

func finishMCPStartError(ctx context.Context, invocation *shell.Invocation, serverName string, err error) error {
	startErr := fmt.Errorf("start mcp server %q: %w", serverName, err)
	if ctxErr := mcpStartContextError(ctx, err); ctxErr != nil {
		startErr = fmt.Errorf("mcp server %q timed out or was canceled: %w", serverName, ctxErr)
	}

	if finishErr := invocation.Finish(shell.FinishOptions{Error: err, OutputCapture: shell.OutputNotCaptured, OutputNote: "MCP server failed before JSON-RPC streaming"}); finishErr != nil {
		return errors.Join(startErr, finishErr)
	}

	return startErr
}

func mcpStartContextError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return errors.Join(ctxErr, err)
	}

	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}

	return nil
}

func decodeIncomingMessage(line []byte) (incomingMessage, error) {
	var fields map[string]json.RawMessage
	if err := decodeJSONUseNumber(line, &fields); err != nil {
		return incomingMessage{}, fmt.Errorf("decode newline-delimited json response: %w", err)
	}

	if rawError, ok := fields["error"]; ok {
		if err := validateResponseErrorObject(rawError); err != nil {
			return incomingMessage{}, err
		}
	}

	var message incomingMessage
	if err := decodeJSONUseNumber(line, &message); err != nil {
		return incomingMessage{}, fmt.Errorf("decode newline-delimited json response: %w", err)
	}

	if strings.TrimSpace(message.JSONRPC) != "2.0" {
		return incomingMessage{}, fmt.Errorf("malformed json-rpc message: jsonrpc=%q", message.JSONRPC)
	}

	return message, nil
}

func decodeJSONUseNumber(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode JSON: %w", err)
	}

	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values")
		}

		return fmt.Errorf("decode trailing JSON: %w", err)
	}

	return nil
}

func validateResponseErrorObject(raw json.RawMessage) error {
	if !jsonValueIsObject(raw) {
		return errors.New("malformed json-rpc response: error must be an object")
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return fmt.Errorf("decode json-rpc error object: %w", err)
	}

	codeRaw, ok := fields["code"]
	if !ok {
		return errors.New("malformed json-rpc response: error missing code")
	}

	var code int
	if err := json.Unmarshal(codeRaw, &code); err != nil {
		return fmt.Errorf("malformed json-rpc response: error code must be an integer: %w", err)
	}

	messageRaw, ok := fields["message"]
	if !ok {
		return errors.New("malformed json-rpc response: error missing message")
	}

	var message string
	if err := json.Unmarshal(messageRaw, &message); err != nil {
		return fmt.Errorf("malformed json-rpc response: error message must be a string: %w", err)
	}

	return nil
}

func validateResponse(response Response) error {
	if strings.TrimSpace(response.JSONRPC) != "2.0" {
		return fmt.Errorf("malformed json-rpc response: jsonrpc=%q", response.JSONRPC)
	}

	if _, err := jsonIDKey(response.ID); err != nil {
		return fmt.Errorf("malformed json-rpc response: invalid id: %w", err)
	}

	if response.Error != nil && len(response.Result) > 0 {
		return errors.New("malformed json-rpc response: contains both result and error")
	}

	if response.Error == nil && len(response.Result) == 0 {
		return errors.New("malformed json-rpc response: missing result or error")
	}

	return nil
}

func jsonIDKey(id any) (string, error) {
	if id == nil {
		return "", errors.New("missing id")
	}

	if !validJSONRPCIDType(id) {
		return "", fmt.Errorf("id must be a string or number, got %s", jsonValueType(id))
	}

	encoded, err := json.Marshal(id)
	if err != nil {
		return "", fmt.Errorf("marshal id: %w", err)
	}

	key := strings.TrimSpace(string(encoded))
	if key == "" || key == "null" {
		return "", errors.New("missing id")
	}

	return key, nil
}

func validJSONRPCIDType(id any) bool {
	switch id.(type) {
	case string, json.Number,
		float64, float32,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64:
		return true
	default:
		return false
	}
}

func withProcessOutput(err error, stderr string) error {
	stderr = strings.TrimSpace(stderr)
	if stderr == "" {
		return err
	}

	return fmt.Errorf("%w: stderr: %s", err, stderr)
}

//nolint:govet // JSON-RPC envelope grouping follows wire format.
type incomingMessage struct {
	ID      any             `json:"id,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *ResponseError  `json:"error,omitempty"`
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method,omitempty"`
}

type responseResult struct {
	response *Response
	err      error
}

//nolint:govet // Mutex stays first by convention; size is irrelevant here.
type stderrBuffer struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	limit int
}

func newStderrBuffer(limit int) *stderrBuffer {
	if limit <= 0 {
		limit = defaultStderrBytes
	}

	return &stderrBuffer{limit: limit}
}

func (b *stderrBuffer) Write(p []byte) (int, error) {
	if b == nil {
		return len(p), nil
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	combined := append(append([]byte(nil), b.buf.Bytes()...), p...)
	if len(combined) > b.limit {
		combined = combined[len(combined)-b.limit:]
	}

	b.buf.Reset()
	_, _ = b.buf.Write(combined)

	return len(p), nil
}

func (b *stderrBuffer) String() string {
	if b == nil {
		return ""
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

// shutdownContext preserves caller values for audit/log correlation while
// making MCP process cleanup independent of a canceled request context. The
// session ShutdownTimeout still bounds graceful shutdown before termination.
type shutdownContext struct {
	context.Context
}

func newShutdownContext(parent context.Context) context.Context {
	return shutdownContext{Context: parent}
}

func (c shutdownContext) Deadline() (time.Time, bool) {
	return time.Time{}, false
}

func (c shutdownContext) Done() <-chan struct{} {
	return nil
}

func (c shutdownContext) Err() error {
	return nil
}

// sessionProcessContext preserves caller values for process creation while
// making the process lifetime explicit: only Session.Close or failed startup
// cancels the command context. Per-request contexts still bound JSON-RPC calls.
type sessionProcessContext struct {
	context.Context
	done chan struct{}
	once sync.Once
}

func newSessionProcessContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx := &sessionProcessContext{Context: parent, done: make(chan struct{})}

	return ctx, ctx.cancel
}

func (c *sessionProcessContext) Deadline() (time.Time, bool) {
	return time.Time{}, false
}

func (c *sessionProcessContext) Done() <-chan struct{} {
	return c.done
}

func (c *sessionProcessContext) Err() error {
	select {
	case <-c.done:
		return context.Canceled
	default:
		return nil
	}
}

func (c *sessionProcessContext) cancel() {
	c.once.Do(func() {
		close(c.done)
	})
}
