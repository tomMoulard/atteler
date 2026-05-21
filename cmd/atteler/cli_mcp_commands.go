package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/tommoulard/atteler/pkg/mcp"
)

func runMCPManifest(path, capability string) error {
	manifest, err := loadMCPManifest(path)
	if err != nil {
		return err
	}

	if err := manifest.Validate(); err != nil {
		return fmt.Errorf("mcp manifest: validate: %w", err)
	}

	if strings.TrimSpace(capability) != "" {
		servers := manifest.Find(capability)
		if len(servers) == 0 {
			fmt.Println("No MCP servers found.")
			return nil
		}

		for i := range servers {
			fmt.Println(formatMCPServer(servers[i]))
		}

		return nil
	}

	for _, name := range manifest.List() {
		fmt.Println(name)
	}

	return nil
}

func runMCPInvoke(ctx context.Context, opts cliOptions) error {
	if strings.TrimSpace(opts.mcpMethod) != "" && strings.TrimSpace(opts.mcpToolName) != "" {
		return errors.New("mcp invoke: use either --mcp-method or --mcp-tool, not both")
	}

	if strings.TrimSpace(opts.mcpManifestPath) == "" {
		return errors.New("mcp invoke: --mcp-manifest is required; run `atteler help plugins`")
	}

	if strings.TrimSpace(opts.mcpServerName) == "" {
		return errors.New("mcp invoke: --mcp-server is required")
	}

	manifest, err := loadMCPManifest(opts.mcpManifestPath)
	if err != nil {
		return err
	}

	if validateErr := manifest.Validate(); validateErr != nil {
		return fmt.Errorf("mcp invoke: validate manifest: %w", validateErr)
	}

	server, ok := findMCPServer(manifest, opts.mcpServerName)
	if !ok {
		return fmt.Errorf("mcp invoke: server %q not found", strings.TrimSpace(opts.mcpServerName))
	}

	timeout := time.Duration(opts.mcpTimeout.value) * time.Second

	var response *mcp.Response

	if strings.TrimSpace(opts.mcpToolName) != "" {
		args, parseErr := parseMCPToolArgs(opts.mcpToolArgsJSON)
		if parseErr != nil {
			return parseErr
		}

		response, err = mcp.CallTool(ctx, server, opts.mcpToolName, args, timeout)
	} else {
		params, parseErr := parseJSONParam(opts.mcpParamsJSON, "mcp params")
		if parseErr != nil {
			return parseErr
		}

		response, err = mcp.Invoke(ctx, server, mcp.Request{Method: opts.mcpMethod, Params: params}, timeout)
	}

	if response != nil {
		fmt.Println(formatMCPResponse(response))
	}

	if err != nil {
		return fmt.Errorf("mcp invoke: %w", err)
	}

	return nil
}

func loadMCPManifest(path string) (mcp.Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return mcp.Manifest{}, fmt.Errorf("mcp manifest: read %s: %w", path, err)
	}

	var manifest mcp.Manifest
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		return mcp.Manifest{}, fmt.Errorf("mcp manifest: parse %s: %w", path, err)
	}

	return manifest, nil
}

func findMCPServer(manifest mcp.Manifest, name string) (mcp.Server, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return mcp.Server{}, false
	}

	for _, server := range manifest.Servers {
		if strings.TrimSpace(server.Name) == name {
			return server, true
		}
	}

	return mcp.Server{}, false
}

func formatMCPServer(server mcp.Server) string {
	parts := []string{server.Name, "command=" + server.Command}
	if len(server.Args) > 0 {
		parts = append(parts, "args="+strings.Join(server.Args, ","))
	}

	if strings.TrimSpace(server.CWD) != "" {
		parts = append(parts, "cwd="+strings.TrimSpace(server.CWD))
	}

	if len(server.Capabilities) > 0 {
		capabilities := append([]string(nil), server.Capabilities...)
		sort.Strings(capabilities)
		parts = append(parts, "capabilities="+strings.Join(capabilities, ","))
	}

	return strings.Join(parts, "\t")
}

func parseMCPToolArgs(raw string) (map[string]any, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}

	var args map[string]any
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return nil, fmt.Errorf("mcp tool args: parse JSON object: %w", err)
	}

	if args == nil {
		return nil, errors.New("mcp tool args: expected JSON object")
	}

	return args, nil
}

func parseJSONParam(raw, label string) (any, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}

	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return nil, fmt.Errorf("%s: parse JSON: %w", label, err)
	}

	return value, nil
}

func formatMCPResponse(response *mcp.Response) string {
	if response == nil {
		return ""
	}

	if response.Error != nil {
		data, err := json.MarshalIndent(response.Error, "", "  ")
		if err == nil {
			return string(data)
		}

		return response.Error.Message
	}

	if len(response.Result) == 0 {
		return "{}"
	}

	var value any
	if err := json.Unmarshal(response.Result, &value); err != nil {
		return string(response.Result)
	}

	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return string(response.Result)
	}

	return string(data)
}
