//nolint:wsl_v5 // LSP lifecycle code uses compact protocol state-machine branches.
package lsp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tommoulard/atteler/pkg/shell"
)

const (
	defaultStartTimeout       = 10 * time.Second
	defaultRequestTimeout     = 30 * time.Second
	defaultShutdownTimeout    = 5 * time.Second
	defaultMaxDiagnosticBytes = 32 * 1024
	workspaceSymbolCapability = "workspace/symbol"
	documentSymbolCapability  = "textDocument/documentSymbol"
	definitionCapability      = "textDocument/definition"
	referencesCapability      = "textDocument/references"
)

var errLanguageServerStopped = errors.New("language server stopped")

// CommandSpec describes the local process a language-server pool wants to run.
// Policies can inspect it before the process is started or reused.
//
//nolint:govet // Field order groups command identity before workspace identity.
type CommandSpec struct {
	Command    string
	Args       []string
	Env        []string
	RootPath   string
	LanguageID string
}

// CommandPolicy authorizes language-server command execution. Return nil to
// allow execution; return an error to deny starting or reusing that server.
type CommandPolicy func(context.Context, CommandSpec) error

// PoolOptions configures a managed language-server pool.
type PoolOptions struct {
	CommandPolicy      CommandPolicy
	Audit              shell.AuditContext
	StartTimeout       time.Duration
	RequestTimeout     time.Duration
	ShutdownTimeout    time.Duration
	MaxDiagnosticBytes int
}

// ServerLog captures bounded language-server process diagnostics without
// writing them directly to the user's terminal.
//
//nolint:govet // Field order groups server identity before captured output.
type ServerLog struct {
	RootPath        string
	LanguageID      string
	Command         string
	Args            []string
	Stderr          string
	Stdout          string
	StderrTruncated bool
	StdoutTruncated bool
}

// UnsupportedCapabilityError reports a request the initialized server did not
// advertise support for.
type UnsupportedCapabilityError struct {
	Method     string
	Capability string
}

func (e *UnsupportedCapabilityError) Error() string {
	if e == nil {
		return "unsupported LSP capability"
	}

	return "language server does not support " + e.Capability
}

// ServerPool manages reusable language-server processes keyed by root,
// language, command, args, and environment.
//
//nolint:govet // Field order keeps the mutex guarding the server map adjacent to that map.
type ServerPool struct {
	mu      sync.Mutex
	servers map[serverKey]*serverSession
	opts    PoolOptions
}

// NewServerPool creates a managed language-server pool.
func NewServerPool(opts PoolOptions) *ServerPool {
	if opts.StartTimeout <= 0 {
		opts.StartTimeout = defaultStartTimeout
	}

	if opts.RequestTimeout <= 0 {
		opts.RequestTimeout = defaultRequestTimeout
	}

	if opts.ShutdownTimeout <= 0 {
		opts.ShutdownTimeout = defaultShutdownTimeout
	}

	if opts.MaxDiagnosticBytes <= 0 {
		opts.MaxDiagnosticBytes = defaultMaxDiagnosticBytes
	}

	return &ServerPool{servers: make(map[serverKey]*serverSession), opts: opts}
}

// DocumentSymbols opens or updates opts.FilePath on a reusable server and
// requests document symbols.
func (p *ServerPool) DocumentSymbols(ctx context.Context, opts Options) ([]Symbol, error) {
	if ctx == nil {
		return nil, errors.New("lsp symbols: nil context")
	}

	req, err := resolveDocumentRequest(opts)
	if err != nil {
		return nil, err
	}

	run := func(session *serverSession) ([]Symbol, error) {
		requestCtx, cancel := p.requestContext(ctx)
		defer cancel()

		return session.documentSymbols(requestCtx, req)
	}

	return p.withSymbolSession(ctx, opts, req.RootPath, req.LanguageID, run)
}

// WorkspaceSymbols requests workspace symbols on a reusable server.
func (p *ServerPool) WorkspaceSymbols(ctx context.Context, opts Options, query string) ([]Symbol, error) {
	if ctx == nil {
		return nil, errors.New("lsp workspace symbols: nil context")
	}

	rootPath, languageID, err := resolveWorkspaceRequest(opts)
	if err != nil {
		return nil, err
	}

	run := func(session *serverSession) ([]Symbol, error) {
		requestCtx, cancel := p.requestContext(ctx)
		defer cancel()

		return session.workspaceSymbols(requestCtx, query)
	}

	return p.withSymbolSession(ctx, opts, rootPath, languageID, run)
}

// Definitions opens or updates opts.FilePath and requests definitions at pos.
func (p *ServerPool) Definitions(ctx context.Context, opts Options, pos Position) ([]Location, error) {
	if ctx == nil {
		return nil, errors.New("lsp definitions: nil context")
	}

	req, err := resolveDocumentRequest(opts)
	if err != nil {
		return nil, err
	}

	run := func(session *serverSession) ([]Location, error) {
		requestCtx, cancel := p.requestContext(ctx)
		defer cancel()

		return session.definitions(requestCtx, req, pos)
	}

	return p.withLocationSession(ctx, opts, req.RootPath, req.LanguageID, run)
}

// References opens or updates opts.FilePath and requests references at pos.
func (p *ServerPool) References(ctx context.Context, opts Options, pos Position, includeDeclaration bool) ([]Location, error) {
	if ctx == nil {
		return nil, errors.New("lsp references: nil context")
	}

	req, err := resolveDocumentRequest(opts)
	if err != nil {
		return nil, err
	}

	run := func(session *serverSession) ([]Location, error) {
		requestCtx, cancel := p.requestContext(ctx)
		defer cancel()

		return session.references(requestCtx, req, pos, includeDeclaration)
	}

	return p.withLocationSession(ctx, opts, req.RootPath, req.LanguageID, run)
}

// Start initializes the managed language-server session for opts without
// issuing a symbol, definition, or reference request.
func (p *ServerPool) Start(ctx context.Context, opts Options) error {
	if ctx == nil {
		return errors.New("lsp start: nil context")
	}

	rootPath, languageID, err := keyPartsForOptions(opts)
	if err != nil {
		return err
	}

	_, err = p.getSession(ctx, opts, rootPath, languageID)

	return err
}

// Healthy reports whether opts currently maps to a live managed server. It
// does not start a new process.
func (p *ServerPool) Healthy(opts Options) (bool, error) {
	if p == nil {
		return false, errors.New("lsp server pool is nil")
	}

	rootPath, languageID, err := keyPartsForOptions(opts)
	if err != nil {
		return false, err
	}

	key := newServerKey(opts, rootPath, languageID)

	p.mu.Lock()
	session := p.servers[key]
	p.mu.Unlock()

	return session != nil && session.healthy(), nil
}

// Diagnostics returns the latest publishDiagnostics notifications captured for
// the server identified by opts. It does not start a server.
func (p *ServerPool) Diagnostics(opts Options) ([]Diagnostic, error) {
	if p == nil {
		return nil, errors.New("lsp server pool is nil")
	}

	rootPath, languageID, err := keyPartsForOptions(opts)
	if err != nil {
		return nil, err
	}

	key := newServerKey(opts, rootPath, languageID)

	p.mu.Lock()
	session := p.servers[key]
	p.mu.Unlock()

	if session == nil {
		return nil, nil
	}

	return session.diagnostics(), nil
}

func keyPartsForOptions(opts Options) (rootPath, languageID string, err error) {
	if strings.TrimSpace(opts.FilePath) == "" {
		return resolveWorkspaceRequest(opts)
	}

	if validationErr := validateOptions(opts); validationErr != nil {
		return "", "", validationErr
	}

	filePath, err := filepath.Abs(strings.TrimSpace(opts.FilePath))
	if err != nil {
		return "", "", fmt.Errorf("resolve file path: %w", err)
	}

	rootPath = strings.TrimSpace(opts.RootPath)
	if rootPath == "" {
		rootPath = filepath.Dir(filePath)
	}

	rootPath, err = filepath.Abs(rootPath)
	if err != nil {
		return "", "", fmt.Errorf("resolve root path: %w", err)
	}

	languageID = strings.TrimSpace(opts.LanguageID)
	if languageID == "" {
		languageID = inferLanguageID(filePath)
	}

	return rootPath, languageID, nil
}

// ServerLogs returns process diagnostics captured from all live servers.
func (p *ServerPool) ServerLogs() []ServerLog {
	if p == nil {
		return nil
	}

	p.mu.Lock()
	sessions := make([]*serverSession, 0, len(p.servers))
	for _, session := range p.servers {
		sessions = append(sessions, session)
	}
	p.mu.Unlock()

	logs := make([]ServerLog, 0, len(sessions))
	for _, session := range sessions {
		logs = append(logs, session.log())
	}

	return logs
}

// Shutdown gracefully closes all managed language-server processes.
func (p *ServerPool) Shutdown(ctx context.Context) error {
	if ctx == nil {
		return errors.New("lsp shutdown: nil context")
	}
	if p == nil {
		return errors.New("lsp server pool is nil")
	}

	p.mu.Lock()
	sessions := make([]*serverSession, 0, len(p.servers))
	for _, session := range p.servers {
		sessions = append(sessions, session)
	}
	p.servers = make(map[serverKey]*serverSession)
	p.mu.Unlock()

	var errs []error
	for _, session := range sessions {
		shutdownCtx, cancel := p.shutdownContext(ctx)
		if err := session.shutdown(shutdownCtx); err != nil {
			errs = append(errs, err)
		}
		cancel()
	}

	return errors.Join(errs...)
}

func (p *ServerPool) withSymbolSession(
	ctx context.Context,
	opts Options,
	rootPath string,
	languageID string,
	run func(*serverSession) ([]Symbol, error),
) ([]Symbol, error) {
	session, err := p.getSession(ctx, opts, rootPath, languageID)
	if err != nil {
		return nil, err
	}

	symbols, err := run(session)
	if err == nil {
		return symbols, nil
	}

	if shouldDropSession(err) {
		p.removeSession(session.key, session)
	}

	if !shouldRetrySession(ctx, err) {
		return nil, err
	}

	session, retryErr := p.getSession(ctx, opts, rootPath, languageID)
	if retryErr != nil {
		return nil, errors.Join(err, retryErr)
	}

	return run(session)
}

func (p *ServerPool) withLocationSession(
	ctx context.Context,
	opts Options,
	rootPath string,
	languageID string,
	run func(*serverSession) ([]Location, error),
) ([]Location, error) {
	session, err := p.getSession(ctx, opts, rootPath, languageID)
	if err != nil {
		return nil, err
	}

	locations, err := run(session)
	if err == nil {
		return locations, nil
	}

	if shouldDropSession(err) {
		p.removeSession(session.key, session)
	}

	if !shouldRetrySession(ctx, err) {
		return nil, err
	}

	session, retryErr := p.getSession(ctx, opts, rootPath, languageID)
	if retryErr != nil {
		return nil, errors.Join(err, retryErr)
	}

	return run(session)
}

func (p *ServerPool) getSession(ctx context.Context, opts Options, rootPath, languageID string) (*serverSession, error) {
	if p == nil {
		return nil, errors.New("lsp server pool is nil")
	}

	spec := commandSpecFromOptions(opts, rootPath, languageID)
	if err := p.authorizeCommand(ctx, opts, spec); err != nil {
		return nil, err
	}

	key := newServerKey(opts, rootPath, languageID)

	p.mu.Lock()
	if session := p.servers[key]; session != nil {
		if session.healthy() {
			p.mu.Unlock()
			return session, nil
		}

		delete(p.servers, key)
		go session.forceClose()
	}
	p.mu.Unlock()

	session, err := p.startSession(ctx, key, spec)
	if err != nil {
		return nil, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if existing := p.servers[key]; existing != nil {
		if existing.healthy() {
			go session.forceClose()
			return existing, nil
		}

		delete(p.servers, key)
		go existing.forceClose()
	}

	p.servers[key] = session

	return session, nil
}

func (p *ServerPool) startSession(ctx context.Context, key serverKey, spec CommandSpec) (*serverSession, error) {
	startCtx, cancel := context.WithTimeout(ctx, p.opts.StartTimeout)
	defer cancel()

	client, err := startClient(startCtx, spec.Command, spec.Args, spec.Env, p.opts.MaxDiagnosticBytes, p.opts.Audit)
	if err != nil {
		return nil, err
	}

	session := newServerSession(key, spec, client)
	client.setNotificationHandler(session.handleNotification)

	rootURI := fileURI(spec.RootPath)
	rootName := filepath.Base(spec.RootPath)
	params := initializeParams{
		ProcessID:    os.Getpid(),
		RootPath:     spec.RootPath,
		RootURI:      rootURI,
		Capabilities: defaultClientCapabilities(),
		WorkspaceFolders: []workspaceFolder{{
			URI:  rootURI,
			Name: rootName,
		}},
	}

	raw, err := client.request(startCtx, "initialize", params)
	if err != nil {
		session.forceClose()
		return nil, fmt.Errorf("initialize language server: %w", err)
	}

	var result initializeResult
	if len(raw) > 0 && string(raw) != jsonNull {
		if err := json.Unmarshal(raw, &result); err != nil {
			session.forceClose()
			return nil, fmt.Errorf("decode initialize result: %w", err)
		}
	}

	session.setCapabilities(result.Capabilities)

	if err := client.notify(startCtx, "initialized", map[string]any{}); err != nil {
		session.forceClose()
		return nil, fmt.Errorf("send initialized notification: %w", err)
	}

	return session, nil
}

func (p *ServerPool) authorizeCommand(ctx context.Context, opts Options, spec CommandSpec) error {
	if p.opts.CommandPolicy != nil {
		if err := p.opts.CommandPolicy(ctx, cloneCommandSpec(spec)); err != nil {
			return fmt.Errorf("authorize language server command: %w", err)
		}
	}

	if opts.CommandPolicy != nil {
		if err := opts.CommandPolicy(ctx, cloneCommandSpec(spec)); err != nil {
			return fmt.Errorf("authorize language server command: %w", err)
		}
	}

	return nil
}

func (p *ServerPool) requestContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return contextWithTimeoutCap(ctx, p.opts.RequestTimeout)
}

func (p *ServerPool) shutdownContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return contextWithTimeoutCap(ctx, p.opts.ShutdownTimeout)
}

func contextWithTimeoutCap(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return ctx, func() {}
	}

	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= timeout {
		return ctx, func() {}
	}

	return context.WithTimeout(ctx, timeout)
}

func (p *ServerPool) removeSession(key serverKey, session *serverSession) {
	p.mu.Lock()
	if p.servers[key] == session {
		delete(p.servers, key)
	}
	p.mu.Unlock()

	session.forceClose()
}

func shouldDropSession(err error) bool {
	return errors.Is(err, errLanguageServerStopped) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

func shouldRetrySession(ctx context.Context, err error) bool {
	return errors.Is(err, errLanguageServerStopped) && ctx.Err() == nil
}

func commandSpecFromOptions(opts Options, rootPath, languageID string) CommandSpec {
	return CommandSpec{
		Command:    strings.TrimSpace(opts.Command),
		Args:       append([]string(nil), opts.Args...),
		Env:        append([]string(nil), opts.Env...),
		RootPath:   rootPath,
		LanguageID: languageID,
	}
}

func cloneCommandSpec(spec CommandSpec) CommandSpec {
	spec.Args = append([]string(nil), spec.Args...)
	spec.Env = append([]string(nil), spec.Env...)

	return spec
}

type serverKey struct {
	RootPath   string
	LanguageID string
	Command    string
	ArgsKey    string
	EnvKey     string
}

func newServerKey(opts Options, rootPath, languageID string) serverKey {
	return serverKey{
		RootPath:   rootPath,
		LanguageID: languageID,
		Command:    strings.TrimSpace(opts.Command),
		ArgsKey:    strings.Join(opts.Args, "\x00"),
		EnvKey:     strings.Join(opts.Env, "\x00"),
	}
}

type openDocumentState struct {
	text    string
	version int
}

type cachedDocumentSymbols struct {
	symbols []Symbol
	version int
}

type cachedWorkspaceSymbols struct {
	symbols          []Symbol
	workspaceVersion int
}

//nolint:govet // Field order groups identity, transport, state, caches, and diagnostics.
type serverSession struct {
	key    serverKey
	spec   CommandSpec
	client *rpcClient

	mu               sync.Mutex
	capabilities     serverCapabilities
	openDocs         map[string]openDocumentState
	docCache         map[string]cachedDocumentSymbols
	workspaceCache   map[string]cachedWorkspaceSymbols
	workspaceVersion int
	diagnosticsByURI map[string][]Diagnostic
}

func newServerSession(key serverKey, spec CommandSpec, client *rpcClient) *serverSession {
	return &serverSession{
		key:              key,
		spec:             cloneCommandSpec(spec),
		client:           client,
		openDocs:         make(map[string]openDocumentState),
		docCache:         make(map[string]cachedDocumentSymbols),
		workspaceCache:   make(map[string]cachedWorkspaceSymbols),
		diagnosticsByURI: make(map[string][]Diagnostic),
	}
}

func (s *serverSession) setCapabilities(capabilities serverCapabilities) {
	s.mu.Lock()
	s.capabilities = capabilities
	s.mu.Unlock()
}

func (s *serverSession) healthy() bool {
	return s.client.isRunning()
}

func (s *serverSession) documentSymbols(ctx context.Context, req documentRequest) ([]Symbol, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.requireCapability(documentSymbolCapability, s.capabilities.supportsDocumentSymbols()); err != nil {
		return nil, err
	}

	version, err := s.openOrUpdateDocumentLocked(ctx, req)
	if err != nil {
		return nil, err
	}

	if cached, ok := s.docCache[req.URI]; ok && cached.version == version {
		return cloneSymbols(cached.symbols), nil
	}

	raw, err := s.client.request(ctx, documentSymbolCapability, documentSymbolParams{TextDocument: textDocumentIdentifier{URI: req.URI}})
	if err != nil {
		return nil, fmt.Errorf("request document symbols: %w", err)
	}

	symbols, err := normalizeSymbols(raw)
	if err != nil {
		return nil, fmt.Errorf("normalize document symbols: %w", err)
	}

	s.docCache[req.URI] = cachedDocumentSymbols{version: version, symbols: cloneSymbols(symbols)}

	return symbols, nil
}

func (s *serverSession) workspaceSymbols(ctx context.Context, query string) ([]Symbol, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.requireCapability(workspaceSymbolCapability, s.capabilities.supportsWorkspaceSymbols()); err != nil {
		return nil, err
	}

	if cached, ok := s.workspaceCache[query]; ok && cached.workspaceVersion == s.workspaceVersion {
		return cloneSymbols(cached.symbols), nil
	}

	raw, err := s.client.request(ctx, workspaceSymbolCapability, workspaceSymbolParams{Query: query})
	if err != nil {
		return nil, fmt.Errorf("request workspace symbols: %w", err)
	}

	symbols, err := normalizeSymbols(raw)
	if err != nil {
		return nil, fmt.Errorf("normalize workspace symbols: %w", err)
	}

	s.workspaceCache[query] = cachedWorkspaceSymbols{workspaceVersion: s.workspaceVersion, symbols: cloneSymbols(symbols)}

	return symbols, nil
}

func (s *serverSession) definitions(ctx context.Context, req documentRequest, pos Position) ([]Location, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.requireCapability(definitionCapability, s.capabilities.supportsDefinitions()); err != nil {
		return nil, err
	}

	if _, err := s.openOrUpdateDocumentLocked(ctx, req); err != nil {
		return nil, err
	}

	raw, err := s.client.request(ctx, definitionCapability, textDocumentPositionParams{TextDocument: textDocumentIdentifier{URI: req.URI}, Position: pos})
	if err != nil {
		return nil, fmt.Errorf("request definitions: %w", err)
	}

	locations, err := normalizeLocations(raw)
	if err != nil {
		return nil, fmt.Errorf("normalize definitions: %w", err)
	}

	return locations, nil
}

func (s *serverSession) references(ctx context.Context, req documentRequest, pos Position, includeDeclaration bool) ([]Location, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.requireCapability(referencesCapability, s.capabilities.supportsReferences()); err != nil {
		return nil, err
	}

	if _, err := s.openOrUpdateDocumentLocked(ctx, req); err != nil {
		return nil, err
	}

	params := referenceParams{
		TextDocument: textDocumentIdentifier{URI: req.URI},
		Position:     pos,
		Context:      referenceContext{IncludeDeclaration: includeDeclaration},
	}
	raw, err := s.client.request(ctx, referencesCapability, params)
	if err != nil {
		return nil, fmt.Errorf("request references: %w", err)
	}

	locations, err := normalizeLocations(raw)
	if err != nil {
		return nil, fmt.Errorf("normalize references: %w", err)
	}

	return locations, nil
}

func (s *serverSession) requireCapability(method string, ok bool) error {
	if ok {
		return nil
	}

	return &UnsupportedCapabilityError{Method: method, Capability: method}
}

func (s *serverSession) openOrUpdateDocumentLocked(ctx context.Context, req documentRequest) (int, error) {
	state, ok := s.openDocs[req.URI]
	if !ok {
		if err := s.client.notify(ctx, "textDocument/didOpen", didOpenParams{TextDocument: textDocumentItem{URI: req.URI, LanguageID: req.LanguageID, Version: 1, Text: req.Content}}); err != nil {
			return 0, fmt.Errorf("open document: %w", err)
		}

		s.openDocs[req.URI] = openDocumentState{version: 1, text: req.Content}
		s.invalidateWorkspaceSymbolsLocked()

		return 1, nil
	}

	if state.text == req.Content {
		return state.version, nil
	}

	state.version++
	switch s.capabilities.textDocumentSyncKind() {
	case textDocumentSyncFull:
		params := didChangeParams{
			TextDocument:   versionedTextDocumentIdentifier{URI: req.URI, Version: state.version},
			ContentChanges: []textDocumentContentChangeEvent{{Text: req.Content}},
		}
		if err := s.client.notify(ctx, "textDocument/didChange", params); err != nil {
			return 0, fmt.Errorf("change document: %w", err)
		}
	case textDocumentSyncIncremental:
		oldRange := fullDocumentRange(state.text)
		params := didChangeParams{
			TextDocument:   versionedTextDocumentIdentifier{URI: req.URI, Version: state.version},
			ContentChanges: []textDocumentContentChangeEvent{{Range: &oldRange, Text: req.Content}},
		}
		if err := s.client.notify(ctx, "textDocument/didChange", params); err != nil {
			return 0, fmt.Errorf("change document: %w", err)
		}
	default:
		if err := s.client.notify(ctx, "textDocument/didClose", didCloseParams{TextDocument: textDocumentIdentifier{URI: req.URI}}); err != nil {
			return 0, fmt.Errorf("close stale document: %w", err)
		}

		if err := s.client.notify(ctx, "textDocument/didOpen", didOpenParams{TextDocument: textDocumentItem{URI: req.URI, LanguageID: req.LanguageID, Version: state.version, Text: req.Content}}); err != nil {
			return 0, fmt.Errorf("reopen changed document: %w", err)
		}
	}

	state.text = req.Content
	s.openDocs[req.URI] = state
	delete(s.docCache, req.URI)
	s.invalidateWorkspaceSymbolsLocked()

	return state.version, nil
}

func fullDocumentRange(text string) Range {
	line := 0
	character := 0
	normalized := strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	for _, r := range normalized {
		if r == '\n' {
			line++
			character = 0
			continue
		}

		character += utf16CharacterLength(r)
	}

	return Range{Start: Position{}, End: Position{Line: line, Character: character}}
}

func utf16CharacterLength(r rune) int {
	if r >= 0x10000 {
		return 2
	}

	return 1
}

func (s *serverSession) invalidateWorkspaceSymbolsLocked() {
	s.workspaceCache = make(map[string]cachedWorkspaceSymbols)
	s.workspaceVersion++
}

func (s *serverSession) handleNotification(method string, params json.RawMessage) {
	if method != "textDocument/publishDiagnostics" {
		return
	}

	var payload publishDiagnosticsParams
	if err := json.Unmarshal(params, &payload); err != nil {
		return
	}

	for i := range payload.Diagnostics {
		payload.Diagnostics[i].URI = payload.URI
	}

	s.mu.Lock()
	s.diagnosticsByURI[payload.URI] = cloneDiagnostics(payload.Diagnostics)
	s.mu.Unlock()
}

func (s *serverSession) diagnostics() []Diagnostic {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []Diagnostic
	for _, diagnostics := range s.diagnosticsByURI {
		out = append(out, cloneDiagnostics(diagnostics)...)
	}

	return out
}

func (s *serverSession) log() ServerLog {
	stderr, stderrTruncated := s.client.stderrLog()
	stdout, stdoutTruncated := s.client.stdoutLog()

	return ServerLog{
		RootPath:        s.spec.RootPath,
		LanguageID:      s.spec.LanguageID,
		Command:         s.spec.Command,
		Args:            append([]string(nil), s.spec.Args...),
		Stderr:          stderr,
		Stdout:          stdout,
		StderrTruncated: stderrTruncated,
		StdoutTruncated: stdoutTruncated,
	}
}

func (s *serverSession) shutdown(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.client.isRunning() {
		s.forceCloseLocked()
		return nil
	}

	var errs []error
	for uri := range s.openDocs {
		if err := s.client.notify(ctx, "textDocument/didClose", didCloseParams{TextDocument: textDocumentIdentifier{URI: uri}}); err != nil && !isStoppedPipeError(err) {
			errs = append(errs, fmt.Errorf("close document %s: %w", uri, err))
		}
	}
	s.openDocs = make(map[string]openDocumentState)

	if _, err := s.client.request(ctx, "shutdown", nil); err != nil && !isStoppedPipeError(err) {
		errs = append(errs, fmt.Errorf("shutdown language server: %w", err))
	}

	if err := s.client.notify(ctx, "exit", nil); err != nil && !isStoppedPipeError(err) {
		errs = append(errs, fmt.Errorf("send exit notification: %w", err))
	}

	if err := s.client.closeGraceful(ctx); err != nil {
		errs = append(errs, err)
	}

	return errors.Join(errs...)
}

func (s *serverSession) forceClose() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.forceCloseLocked()
}

func (s *serverSession) forceCloseLocked() {
	s.client.kill()
}

func isStoppedPipeError(err error) bool {
	return errors.Is(err, errLanguageServerStopped) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe)
}

func cloneDiagnostics(in []Diagnostic) []Diagnostic {
	out := append([]Diagnostic(nil), in...)
	for i := range out {
		out[i].Code = append(json.RawMessage(nil), out[i].Code...)
	}

	return out
}
