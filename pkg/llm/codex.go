package llm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/tommoulard/atteler/pkg/events"
)

// CodexProvider shells out to Codex CLI so Atteler can reuse a ChatGPT/Codex
// login without consuming OpenAI Platform API quota.
type CodexProvider struct {
	bin    string
	models []string
}

// NewCodexProvider creates a provider backed by the local codex executable.
func NewCodexProvider() (*CodexProvider, error) {
	if !hasCodexAuth() {
		return nil, errors.New("no Codex credentials found: run `codex login`")
	}

	bin, err := exec.LookPath("codex")
	if err != nil {
		return nil, fmt.Errorf("codex executable not found: %w", err)
	}

	return &CodexProvider{bin: bin, models: codexModels()}, nil
}

// Name returns the provider name.
func (c *CodexProvider) Name() string { return providerCodex }

// Models returns Codex CLI model IDs.
func (c *CodexProvider) Models() []string {
	if len(c.models) == 0 {
		return defaultCodexModels()
	}

	return append([]string(nil), c.models...)
}

// FetchModels returns the local Codex model catalog. Codex CLI currently owns
// model availability, so there is no separate Atteler network discovery call.
func (c *CodexProvider) FetchModels(_ context.Context) ([]string, error) {
	return c.Models(), nil
}

// HealthCheck verifies that the codex CLI is reachable and that valid
// credentials exist locally.
func (c *CodexProvider) HealthCheck(ctx context.Context) error {
	emitActivity(ctx, events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command":  "codex --version",
			"provider": providerCodex,
		},
	})
	//nolint:gosec // c.bin comes from exec.LookPath; argument is static.
	cmd := exec.CommandContext(ctx, c.bin, "--version")
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("codex --version: %w: %s", err, output)
	}

	if !hasCodexAuth() {
		return errors.New("no Codex credentials found: run `codex login`")
	}

	return nil
}

// ModelContextWindow returns the context window size for a Codex model.
func (c *CodexProvider) ModelContextWindow(model string) int {
	switch model {
	case "gpt-5.5", "gpt-5.4":
		return 400_000
	case "gpt-5.4-mini", "gpt-5.3-codex", "gpt-5.3-codex-spark":
		return 200_000
	default:
		if strings.HasPrefix(model, "gpt-") {
			return 200_000
		}

		return 0
	}
}

// Complete runs `codex exec` and returns the final assistant message.
func (c *CodexProvider) Complete(ctx context.Context, params CompleteParams) (*Response, error) {
	if c.bin == "" {
		return nil, errors.New("codex executable not configured")
	}

	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("codex working directory: %w", err)
	}

	model := params.Model
	if model == "" {
		models := c.Models()
		if len(models) == 0 {
			return nil, errors.New("codex model not configured")
		}

		model = models[0]
	}

	outFile, err := os.CreateTemp("", "atteler-codex-last-*.txt")
	if err != nil {
		return nil, fmt.Errorf("codex output file: %w", err)
	}

	outPath := outFile.Name()
	if closeErr := outFile.Close(); closeErr != nil {
		_ = os.Remove(outPath)
		return nil, fmt.Errorf("codex output file: %w", closeErr)
	}
	defer os.Remove(outPath)

	args := []string{
		"exec",
		"--ephemeral",
		"--skip-git-repo-check",
		"--sandbox", "workspace-write",
		"--cd", cwd,
		"-m", model,
	}
	if effort := cliReasoningEffort(params.ReasoningLevel); effort != "" {
		args = append(args, "-c", fmt.Sprintf("model_reasoning_effort=%q", effort))
	}

	args = append(args,
		"-o", outPath,
		codexPrompt(params.Messages),
	)

	emitActivity(ctx, events.Event{
		Type:  events.CommandExecute,
		Model: model,
		Metadata: map[string]string{
			"command":  "codex exec",
			"cwd":      cwd,
			"provider": providerCodex,
		},
	})
	//nolint:gosec // c.bin comes from exec.LookPath or tests; args are passed without a shell.
	cmd := exec.CommandContext(ctx, c.bin, args...)
	cmd.Dir = cwd

	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("codex exec: %w: %s", err, output)
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		return nil, fmt.Errorf("codex read final message: %w", err)
	}

	content := strings.TrimSpace(string(data))
	if content == "" {
		return nil, errors.New("codex returned an empty final message")
	}

	return &Response{Content: content, Model: model}, nil
}

func codexPrompt(messages []Message) string {
	var (
		system []string
		b      strings.Builder
	)

	for _, msg := range messages {
		if msg.Role == RoleSystem {
			system = append(system, msg.Content)
		}
	}

	if len(system) > 0 {
		b.WriteString("<system>\n")
		b.WriteString(strings.Join(system, "\n\n"))
		b.WriteString("\n</system>\n\n")
	}

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

func codexModels() []string {
	models := defaultCodexModels()
	if model := codexConfiguredModel(); model != "" {
		models = append([]string{model}, models...)
	}

	return dedupeStrings(models)
}

func defaultCodexModels() []string {
	return []string{
		"gpt-5.5",
		"gpt-5.4",
		"gpt-5.4-mini",
		"gpt-5.3-codex",
		"gpt-5.3-codex-spark",
	}
}

func codexConfiguredModel() string {
	data, err := os.ReadFile(filepath.Join(codexConfigDir(), "config.toml"))
	if err != nil {
		return ""
	}

	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(stripInlineComment(line))
		if !strings.HasPrefix(line, "model") {
			continue
		}

		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != "model" {
			continue
		}

		return strings.Trim(strings.TrimSpace(value), `"'`)
	}

	return ""
}

func codexConfigDir() string {
	if dir := os.Getenv("CODEX_HOME"); strings.TrimSpace(dir) != "" {
		return dir
	}

	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}

	return filepath.Join(home, ".codex")
}

func stripInlineComment(line string) string {
	before, _, found := strings.Cut(line, "#")
	if found {
		return before
	}

	return line
}

func dedupeStrings(values []string) []string {
	out := make([]string, 0, len(values))

	seen := make(map[string]bool, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}

		seen[value] = true
		out = append(out, value)
	}

	return out
}
