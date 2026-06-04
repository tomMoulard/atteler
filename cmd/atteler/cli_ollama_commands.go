package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	appconfig "github.com/tommoulard/atteler/pkg/config"
	"github.com/tommoulard/atteler/pkg/llm"
)

const (
	ollamaProviderName    = "ollama"
	commandOllamaStatus   = "ollama-status"
	commandOllamaStop     = "ollama-stop"
	ollamaStopCommandHint = "atteler providers " + commandOllamaStop
)

func printOllamaStatus(ctx context.Context) error {
	cfg, _, err := loadConfigWithPermission(
		ctx,
		"load config for Ollama status",
		"atteler.provider.ollama.status",
		"ollama status",
	)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	status := llm.CheckOllamaStatus(ctx, ollamaProviderConfig(cfg))
	printOllamaStatusReport(status)

	return nil
}

func stopOllamaDaemon(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	result, err := llm.StopOwnedOllamaDaemon(ctx, "")
	if err != nil {
		return fmt.Errorf("stop ollama daemon: %w", err)
	}

	fmt.Println("Ollama stop")
	fmt.Println("ownership_path: " + result.OwnershipPath)
	fmt.Println("result: " + result.Message)
	fmt.Printf("stopped: %t\n", result.Stopped)
	fmt.Printf("cleaned: %t\n", result.Cleaned)

	if result.Ownership != nil {
		fmt.Printf("pid: %d\n", result.Ownership.PID)
		fmt.Println("base_url: " + result.Ownership.BaseURL)
	}

	return nil
}

func printOllamaStatusReport(status llm.OllamaStatus) {
	fmt.Println("Ollama status")
	fmt.Println("state: " + string(status.State))
	fmt.Println("base_url: " + status.BaseURL)
	fmt.Printf("local: %t\n", status.Local)
	fmt.Printf("auto_start: %s (%s)\n", enabledDisabled(status.AutoStart.Enabled), status.AutoStart.Source)

	if status.AutoStart.Error != "" {
		fmt.Println("auto_start_error: " + status.AutoStart.Error)
	}

	fmt.Println("ownership_path: " + status.OwnershipPath)
	fmt.Println("ownership: " + ollamaOwnershipSummary(status))

	if status.Ownership != nil {
		printOllamaOwnershipReport(*status.Ownership)
	}

	fmt.Println("stop: " + ollamaStopHint(status))

	if status.Error != "" {
		fmt.Println("error: " + status.Error)
	}
}

func printOllamaOwnershipReport(ownership llm.OllamaDaemonOwnership) {
	if ownership.Owner != "" {
		fmt.Println("owner: " + ownership.Owner)
	}

	fmt.Printf("pid: %d\n", ownership.PID)
	fmt.Println("daemon_command: " + formatOllamaCommand(ownership.Command))
	fmt.Println("started_at: " + ownership.StartedAt.Format(time.RFC3339))

	if ownership.SessionID != "" {
		fmt.Println("session_id: " + ownership.SessionID)
	}

	if len(ownership.AttelerCommand) > 0 {
		fmt.Println("atteler_command: " + formatOllamaCommand(ownership.AttelerCommand))
	}

	if ownership.AutoStartSource != "" {
		fmt.Println("auto_start_source: " + ownership.AutoStartSource)
	}

	if len(ownership.Environment) > 0 {
		fmt.Println("daemon_environment: " + formatOllamaEnvironment(ownership.Environment))
	}

	if ownership.LogPath != "" {
		fmt.Println("startup_log: " + ownership.LogPath)
	}
}

func formatOllamaDoctorLine(ctx context.Context, cfg appconfig.Config) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	status := llm.CheckOllamaStatus(ctx, ollamaProviderConfig(cfg))
	parts := []string{
		"ollama: " + string(status.State),
		"base_url=" + status.BaseURL,
		"auto_start=" + enabledDisabled(status.AutoStart.Enabled),
		"ownership=" + ollamaOwnershipSummary(status),
	}

	if status.Error != "" {
		parts = append(parts, "error="+status.Error)
	}

	if status.AutoStart.Error != "" {
		parts = append(parts, "auto_start_error="+status.AutoStart.Error)
	}

	return strings.Join(parts, " ")
}

func formatOllamaCommand(command []string) string {
	if len(command) == 0 {
		return unknownLabel
	}

	return strings.Join(command, " ")
}

func formatOllamaEnvironment(environment map[string]string) string {
	keys := make([]string, 0, len(environment))
	for key := range environment {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+environment[key])
	}

	return strings.Join(parts, " ")
}

func ollamaProviderConfig(cfg appconfig.Config) llm.ProviderConfig {
	provider := cfg.Providers[ollamaProviderName]

	return llm.ProviderConfig{
		BaseURL:        provider.BaseURL,
		Disabled:       provider.Disabled,
		AutoStart:      provider.AutoStart,
		TimeoutSeconds: provider.TimeoutSeconds,
	}
}

func enabledDisabled(enabled bool) string {
	if enabled {
		return "enabled"
	}

	return "disabled"
}

func ollamaOwnershipSummary(status llm.OllamaStatus) string {
	if status.OwnershipStatus != "" {
		return status.OwnershipStatus
	}

	if status.Ownership == nil {
		return "none"
	}

	return "recorded"
}

func ollamaStopHint(status llm.OllamaStatus) string {
	if status.Ownership == nil {
		return "no Atteler-owned daemon recorded"
	}

	if status.State == llm.OllamaStatusStartedByAtteler || status.OwnershipStatus == "owned-running" {
		return "run `" + ollamaStopCommandHint + "` or `atteler --ollama-stop`"
	}

	if status.OwnershipStatus == "owned-stale" {
		return "run `" + ollamaStopCommandHint + "` or `atteler --ollama-stop` to remove the stale ownership record"
	}

	return "record exists, but this status is not an Atteler-owned running daemon for the selected base URL"
}
