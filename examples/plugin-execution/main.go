// Package main demonstrates executing a local plugin entrypoint through the SDK.
package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/tommoulard/atteler/pkg/plugin"
	"github.com/tommoulard/atteler/pkg/sdk"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	root, err := os.MkdirTemp("", "atteler-plugin-example-*")
	if err != nil {
		return fmt.Errorf("create plugin example dir: %w", err)
	}

	defer func() {
		if cleanupErr := os.RemoveAll(root); cleanupErr != nil {
			log.Printf("cleanup plugin example: %v", cleanupErr)
		}
	}()

	if scriptErr := writeScript(filepath.Join(root, "bin", "hello"), "#!/bin/sh\nprintf 'hello %s\\n' \"$1\"\n"); scriptErr != nil {
		return scriptErr
	}

	manifest := plugin.Manifest{
		Name:                  "hello",
		Version:               "1.0.0",
		MinimumAttelerVersion: "0.1.0",
		Entrypoints:           map[string]string{"run": "bin/hello"},
		EntrypointContracts: map[string]plugin.EntrypointContract{
			"run": {
				Inputs: plugin.EntrypointInputs{Args: []plugin.ArgumentSpec{{Name: "subject", Required: true}}},
				Output: &plugin.StructuredOutputContract{Format: plugin.OutputFormatText},
			},
		},
		Permissions: &plugin.PermissionSet{
			Filesystem: plugin.FilesystemPermissions{Read: []string{"."}},
			Shell:      plugin.ShellPermissions{Allow: true},
		},
		Output: &plugin.OutputLimits{StdoutMaxBytes: 4096, StderrMaxBytes: 4096},
		Trust: &plugin.Trust{
			Enabled:       true,
			InstallSource: "example",
			Checksum:      "sha256:example",
			Audit:         []plugin.TrustAudit{{Action: "accepted", Actor: "example", At: "2026-06-22T00:00:00Z"}},
		},
	}
	policy := plugin.AcceptManifestPolicy(manifest)

	result, err := sdk.RunPlugin(exampleContext{}, sdk.PluginRunOptions{
		Policy:     &policy,
		Manifest:   manifest,
		Root:       root,
		Entrypoint: "run",
		Args:       []string{"SDK"},
		Timeout:    5 * time.Second,
	})
	if err != nil {
		return fmt.Errorf("run example plugin: %w", err)
	}

	fmt.Print(result.Stdout)

	return nil
}

func writeScript(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create script dir: %w", err)
	}

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return fmt.Errorf("write script: %w", err)
	}

	//nolint:gosec // SDK example intentionally creates a local executable plugin script.
	if err := os.Chmod(path, 0o700); err != nil {
		return fmt.Errorf("make script executable: %w", err)
	}

	return nil
}

// exampleContext keeps examples free of process-root context creation; real applications should pass their request or command context.
type exampleContext struct{}

func (exampleContext) Deadline() (time.Time, bool) { return time.Time{}, false }

func (exampleContext) Done() <-chan struct{} { return nil }

func (exampleContext) Err() error { return nil }

func (exampleContext) Value(_ any) any { return nil }
