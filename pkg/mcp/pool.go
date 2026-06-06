//nolint:wsl_v5 // Session pool lifecycle code keeps related lock/start/cleanup branches together.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// SessionPool manages persistent MCP sessions keyed by server process settings.
// It is safe for concurrent use.
//
//nolint:govet // Mutex and guarded session map stay adjacent for readability.
type SessionPool struct {
	mu       sync.Mutex
	sessions map[string]*Session
	opts     SessionOptions
}

// NewSessionPool creates a reusable MCP session pool.
func NewSessionPool(opts SessionOptions) *SessionPool {
	return &SessionPool{sessions: make(map[string]*Session), opts: opts}
}

// Session returns a started and initialized reusable session for server.
func (p *SessionPool) Session(ctx context.Context, server Server) (*Session, error) {
	if p == nil {
		return nil, errors.New("mcp session pool: nil pool")
	}

	if err := requireInvokeContext(ctx); err != nil {
		return nil, err
	}

	key, err := serverSessionKey(server)
	if err != nil {
		return nil, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sessions == nil {
		p.sessions = make(map[string]*Session)
	}

	if session := p.sessions[key]; session != nil {
		if session.busyButReusable() {
			return session, nil
		}

		if session.Health(ctx).Healthy {
			return session, nil
		}

		delete(p.sessions, key)
		_ = session.Close(newShutdownContext(ctx))
	}

	session := NewSession(server, p.opts)
	if err := session.Start(ctx); err != nil {
		return nil, err
	}

	p.sessions[key] = session

	return session, nil
}

// WithSession runs fn with a started reusable session.
func (p *SessionPool) WithSession(ctx context.Context, server Server, fn func(*Session) error) error {
	if fn == nil {
		return errors.New("mcp session pool: nil session callback")
	}

	session, err := p.Session(ctx, server)
	if err != nil {
		return err
	}

	return fn(session)
}

// Invoke sends one request on a pooled initialized MCP session.
func (p *SessionPool) Invoke(ctx context.Context, server Server, request Request) (*Response, error) {
	session, err := p.Session(ctx, server)
	if err != nil {
		return nil, err
	}

	return session.Invoke(ctx, request)
}

// CallTool invokes a discovered tool on a pooled initialized MCP session.
func (p *SessionPool) CallTool(ctx context.Context, server Server, toolName string, arguments map[string]any) (*Response, error) {
	session, err := p.Session(ctx, server)
	if err != nil {
		return nil, err
	}

	return session.CallTool(ctx, toolName, arguments)
}

// ListTools discovers tools on a pooled initialized MCP session.
func (p *SessionPool) ListTools(ctx context.Context, server Server) ([]Tool, error) {
	session, err := p.Session(ctx, server)
	if err != nil {
		return nil, err
	}

	return session.ListTools(ctx)
}

// ListResources discovers resources on a pooled initialized MCP session.
func (p *SessionPool) ListResources(ctx context.Context, server Server) ([]Resource, error) {
	session, err := p.Session(ctx, server)
	if err != nil {
		return nil, err
	}

	return session.ListResources(ctx)
}

// ListPrompts discovers prompts on a pooled initialized MCP session.
func (p *SessionPool) ListPrompts(ctx context.Context, server Server) ([]Prompt, error) {
	session, err := p.Session(ctx, server)
	if err != nil {
		return nil, err
	}

	return session.ListPrompts(ctx)
}

// CloseAll cleanly shuts down all sessions managed by the pool.
func (p *SessionPool) CloseAll(ctx context.Context) error {
	if p == nil {
		return nil
	}

	if ctx == nil {
		return errors.New("mcp session pool: context is required")
	}

	p.mu.Lock()
	sessions := p.sessions
	p.sessions = make(map[string]*Session)
	p.mu.Unlock()

	var closeErr error
	for _, session := range sessions {
		if err := session.Close(ctx); err != nil {
			closeErr = errors.Join(closeErr, err)
		}
	}

	return closeErr
}

func serverSessionKey(server Server) (string, error) {
	server.Name = strings.TrimSpace(server.Name)
	server.Command = strings.TrimSpace(server.Command)
	server.CWD = strings.TrimSpace(server.CWD)
	if err := server.Validate(); err != nil {
		return "", err
	}

	encoded, err := json.Marshal(struct {
		Env          map[string]string `json:"env,omitempty"`
		Name         string            `json:"name"`
		Command      string            `json:"command"`
		CWD          string            `json:"cwd,omitempty"`
		Args         []string          `json:"args,omitempty"`
		Capabilities []string          `json:"capabilities,omitempty"`
	}{
		Name:         server.Name,
		Command:      server.Command,
		Args:         server.Args,
		Env:          server.Env,
		CWD:          server.CWD,
		Capabilities: normalizedCapabilities(server.Capabilities),
	})
	if err != nil {
		return "", fmt.Errorf("encode mcp session key: %w", err)
	}

	return string(encoded), nil
}

func normalizedCapabilities(capabilities []string) []string {
	if len(capabilities) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(capabilities))
	normalized := make([]string, 0, len(capabilities))
	for _, capability := range capabilities {
		capability = strings.TrimSpace(capability)
		if capability == "" {
			continue
		}

		if _, ok := seen[capability]; ok {
			continue
		}

		seen[capability] = struct{}{}
		normalized = append(normalized, capability)
	}

	if len(normalized) == 0 {
		return nil
	}

	sort.Strings(normalized)

	return normalized
}
