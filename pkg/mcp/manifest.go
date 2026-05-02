// Package mcp defines dependency-free MCP manifest configuration primitives.
package mcp

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Server describes a configured MCP server process.
type Server struct {
	Name         string            `json:"name" yaml:"name"`
	Command      string            `json:"command" yaml:"command"`
	Args         []string          `json:"args,omitempty" yaml:"args,omitempty"`
	Env          map[string]string `json:"env,omitempty" yaml:"env,omitempty"`
	CWD          string            `json:"cwd,omitempty" yaml:"cwd,omitempty"`
	Capabilities []string          `json:"capabilities,omitempty" yaml:"capabilities,omitempty"`
}

// Manifest lists configured MCP servers.
type Manifest struct {
	Servers []Server `json:"servers" yaml:"servers"`
}

// Validate checks server required fields and duplicate server names.
func (m Manifest) Validate() error {
	seen := make(map[string]struct{}, len(m.Servers))
	for i, server := range m.Servers {
		name := strings.TrimSpace(server.Name)
		if name == "" {
			return fmt.Errorf("server %d: missing name", i)
		}
		if _, ok := seen[name]; ok {
			return fmt.Errorf("duplicate server name %q", name)
		}
		seen[name] = struct{}{}

		if strings.TrimSpace(server.Command) == "" {
			return fmt.Errorf("server %q: missing command", name)
		}
		if err := validateCapabilities(name, server.Capabilities); err != nil {
			return err
		}
	}
	return nil
}

// List returns server names sorted lexicographically.
func (m Manifest) List() []string {
	if len(m.Servers) == 0 {
		return nil
	}

	names := make([]string, 0, len(m.Servers))
	for _, server := range m.Servers {
		name := strings.TrimSpace(server.Name)
		if name != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// Find returns servers declaring capability, sorted by server name.
func (m Manifest) Find(capability string) []Server {
	capability = strings.TrimSpace(capability)
	if capability == "" || len(m.Servers) == 0 {
		return nil
	}

	matches := make([]Server, 0)
	for _, server := range m.Servers {
		if hasCapability(server.Capabilities, capability) {
			matches = append(matches, server)
		}
	}
	if len(matches) == 0 {
		return nil
	}
	sort.Slice(matches, func(i, j int) bool {
		return strings.TrimSpace(matches[i].Name) < strings.TrimSpace(matches[j].Name)
	})
	return matches
}

func validateCapabilities(serverName string, capabilities []string) error {
	for i, capability := range capabilities {
		if strings.TrimSpace(capability) == "" {
			return fmt.Errorf("server %q: capability %d: %w", serverName, i, errors.New("empty capability"))
		}
	}
	return nil
}

func hasCapability(capabilities []string, want string) bool {
	for _, capability := range capabilities {
		if strings.TrimSpace(capability) == want {
			return true
		}
	}
	return false
}
