package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/tommoulard/atteler/pkg/events"
)

const claudeCodeAuthTimeout = 5 * time.Second

const claudeCodeFileTools = "Read,Write,Edit,MultiEdit,LS,Glob,Grep"

// ClaudeCodeProvider shells out to Claude Code so Atteler can reuse a working
// Claude subscription login without consuming direct Anthropic API quota.
type ClaudeCodeProvider struct {
	bin    string
	models []string
}

// NewClaudeCodeProvider creates a provider backed by the local claude
// executable and verifies that Claude Code is logged in.
func NewClaudeCodeProvider() (*ClaudeCodeProvider, error) {
	bin, err := exec.LookPath("claude")
	if err != nil {
		return nil, fmt.Errorf("claude executable not found: %w", err)
	}
	if err := verifyClaudeCodeAuth(context.Background(), bin); err != nil {
		return nil, err
	}
	return &ClaudeCodeProvider{bin: bin, models: defaultClaudeCodeModels()}, nil
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

// FetchModels returns the local Claude Code model catalog. Claude Code owns
// model availability, so Atteler does not make a separate Anthropic API call.
func (c *ClaudeCodeProvider) FetchModels(_ context.Context) ([]string, error) {
	return c.Models(), nil
}

// HealthCheck verifies that the claude CLI is reachable and the user is
// authenticated by running `claude auth status`.
func (c *ClaudeCodeProvider) HealthCheck(ctx context.Context) error {
	emitActivity(ctx, events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command":  "claude auth status",
			"provider": providerClaudeCode,
		},
	})
	return verifyClaudeCodeAuth(ctx, c.bin)
}

// ModelContextWindow returns the context window size for a Claude Code model.
func (c *ClaudeCodeProvider) ModelContextWindow(model string) int {
	return anthropicContextWindow(model)
}

// Complete runs `claude --print` and returns its text output.
func (c *ClaudeCodeProvider) Complete(ctx context.Context, params CompleteParams) (*Response, error) {
	if c.bin == "" {
		return nil, errors.New("claude executable not configured")
	}

	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("claude code working directory: %w", err)
	}

	model := params.Model
	if model == "" {
		models := c.Models()
		if len(models) == 0 {
			return nil, errors.New("claude code model not configured")
		}
		model = models[0]
	}

	args := []string{
		"--print",
		"--no-session-persistence",
		"--permission-mode", "acceptEdits",
		"--tools", claudeCodeFileTools,
		"--allowed-tools", claudeCodeFileTools,
		"--add-dir", cwd,
		"--model", model,
		"--output-format", "text",
	}
	if system := systemPrompt(params.Messages); system != "" {
		args = append(args, "--system-prompt", system)
	}
	args = append(args, conversationPrompt(params.Messages))

	emitActivity(ctx, events.Event{
		Type:  events.CommandExecute,
		Model: model,
		Metadata: map[string]string{
			"command":  "claude --print",
			"cwd":      cwd,
			"provider": providerClaudeCode,
		},
	})
	//nolint:gosec // c.bin comes from exec.LookPath or tests; args are passed without a shell.
	cmd := exec.CommandContext(ctx, c.bin, args...)
	cmd.Dir = cwd
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("claude code: %w: %s", err, stderr.String())
	}

	content := strings.TrimSpace(stdout.String())
	if content == "" {
		return nil, errors.New("claude code returned an empty response")
	}
	return &Response{Content: content, Model: model}, nil
}

func verifyClaudeCodeAuth(ctx context.Context, bin string) error {
	ctx, cancel := context.WithTimeout(ctx, claudeCodeAuthTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "auth", "status")
	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("claude auth status failed: %w", err)
	}

	var status struct {
		LoggedIn bool `json:"loggedIn"`
	}
	if err := json.Unmarshal(output, &status); err != nil {
		return fmt.Errorf("claude auth status parse: %w", err)
	}
	if !status.LoggedIn {
		return errors.New("no Claude Code credentials found: run `claude auth login`")
	}
	return nil
}

func defaultClaudeCodeModels() []string {
	return []string{
		"claude-opus-4-7",
		"claude-opus-4-6",
		"claude-opus-4-5-20251101",
		"claude-opus-4-1-20250805",
		"claude-opus-4-20250514",
		"claude-sonnet-4-6",
		"claude-sonnet-4-5-20250929",
		"claude-sonnet-4-20250514",
		"claude-haiku-4-5-20251001",
		"opus",
		"sonnet",
		"haiku",
	}
}

func systemPrompt(messages []Message) string {
	var system []string
	for _, msg := range messages {
		if msg.Role == RoleSystem {
			system = append(system, msg.Content)
		}
	}
	return strings.Join(system, "\n\n")
}

func conversationPrompt(messages []Message) string {
	var b strings.Builder
	for _, msg := range messages {
		if msg.Role == RoleSystem {
			continue
		}
		b.WriteString(strings.ToUpper(string(msg.Role)))
		b.WriteString(":\n")
		b.WriteString(msg.Content)
		b.WriteString("\n\n")
	}
	return strings.TrimSpace(b.String())
}
