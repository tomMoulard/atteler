package plugin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

// RunResult contains the output captured while running a plugin entrypoint.
type RunResult struct {
	Stdout string
	Stderr string
}

// RunOptions controls a plugin entrypoint execution.
//
//nolint:govet // Field order follows execution flow readability.
type RunOptions struct {
	Policy  *Policy
	Env     map[string]string
	Timeout time.Duration
	Args    []string
}

// RunEntrypoint preserves the legacy signature but refuses to execute without
// an accepted policy. Use RunEntrypointWithOptions for new code.
func RunEntrypoint(
	ctx context.Context,
	root string,
	manifest Manifest,
	entrypointName string,
	timeout time.Duration,
) (RunResult, error) {
	return RunEntrypointWithOptions(ctx, root, manifest, entrypointName, RunOptions{Timeout: timeout})
}

// RunEntrypointWithOptions validates manifest, authorizes declared permissions
// against policy, builds a scrubbed allowlisted environment, validates
// positional args against the entrypoint schema, and runs the entrypoint with
// root as the working directory.
func RunEntrypointWithOptions(
	ctx context.Context,
	root string,
	manifest Manifest,
	entrypointName string,
	options RunOptions,
) (RunResult, error) {
	if options.Timeout <= 0 {
		return RunResult{}, errors.New("plugin: entrypoint timeout must be positive")
	}

	entrypointName = strings.TrimSpace(entrypointName)
	if entrypointName == "" {
		return RunResult{}, errors.New("plugin: empty entrypoint name")
	}

	if err := manifest.Validate(root); err != nil {
		return RunResult{}, fmt.Errorf("plugin: validate manifest: %w", err)
	}

	entrypoint, ok := manifest.Entrypoints[entrypointName]
	if !ok {
		return RunResult{}, fmt.Errorf("plugin: entrypoint %q not found", entrypointName)
	}

	if options.Policy == nil {
		return RunResult{}, fmt.Errorf("plugin: authorize entrypoint %q: accepted policy must be provided", entrypointName)
	}

	policy := ClonePolicy(*options.Policy)
	if err := authorizeRun(root, manifest, entrypointName, policy); err != nil {
		return RunResult{}, fmt.Errorf("plugin: authorize entrypoint %q: %w", entrypointName, err)
	}

	args, err := validateRunArgs(entrypointName, manifest.EntrypointArgs[entrypointName], options.Args)
	if err != nil {
		return RunResult{}, fmt.Errorf("plugin: authorize entrypoint %q: %w", entrypointName, err)
	}

	env, secrets, err := buildPluginEnvironment(manifest, options.Env)
	if err != nil {
		return RunResult{}, fmt.Errorf("plugin: authorize entrypoint %q: %w", entrypointName, err)
	}

	rootAbs, targetAbs, err := resolveEntrypoint(root, entrypoint)
	if err != nil {
		return RunResult{}, fmt.Errorf("plugin: resolve entrypoint %q: %w", entrypointName, err)
	}

	usesShell, err := entrypointUsesShell(targetAbs)
	if err != nil {
		return RunResult{}, fmt.Errorf("plugin: authorize entrypoint %q: %w", entrypointName, err)
	}

	if usesShell && !manifest.Permissions.Shell.Allow {
		return RunResult{}, fmt.Errorf("plugin: authorize entrypoint %q: shell access must be declared in permissions", entrypointName)
	}

	runCtx, cancel := context.WithTimeout(ctx, options.Timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, targetAbs, args...)
	cmd.Dir = rootAbs
	cmd.Env = env

	stdout := newBoundedBuffer(manifest.Output.StdoutMaxBytes)
	stderr := newBoundedBuffer(manifest.Output.StderrMaxBytes)

	cmd.Stdout = stdout
	cmd.Stderr = stderr

	runErr := cmd.Run()

	redactor := outputRedactor{secrets: secrets}

	result := RunResult{
		Stdout: stdout.String(redactor),
		Stderr: stderr.String(redactor),
	}
	if runCtx.Err() != nil {
		return result, fmt.Errorf("plugin: run entrypoint %q: %w", entrypointName, runCtx.Err())
	}

	if runErr != nil {
		return result, fmt.Errorf("plugin: run entrypoint %q: %w", entrypointName, runErr)
	}

	return result, nil
}

func validateRunArgs(entrypointName string, schema []ArgumentSpec, args []string) ([]string, error) {
	if len(args) > len(schema) {
		return nil, fmt.Errorf("entrypoint %q accepts at most %d args", entrypointName, len(schema))
	}

	for i, spec := range schema {
		if spec.Required && i >= len(args) {
			return nil, fmt.Errorf("entrypoint %q missing required arg %q", entrypointName, spec.Name)
		}
	}

	copied := append([]string(nil), args...)
	for i, arg := range copied {
		spec := schema[i]
		if len(spec.Allowed) > 0 && !slices.Contains(spec.Allowed, arg) {
			return nil, fmt.Errorf("arg %q value %q is not allowed", spec.Name, arg)
		}

		if strings.TrimSpace(spec.Pattern) == "" {
			continue
		}

		matched, err := regexp.MatchString(spec.Pattern, arg)
		if err != nil {
			return nil, fmt.Errorf("arg %q pattern: %w", spec.Name, err)
		}

		if !matched {
			return nil, fmt.Errorf("arg %q value %q does not match pattern", spec.Name, arg)
		}
	}

	return copied, nil
}

func buildPluginEnvironment(manifest Manifest, explicit map[string]string) ([]string, []secretValue, error) {
	allowed := make(map[string]struct{}, len(manifest.Permissions.Env)+len(manifest.Permissions.Secrets))
	for _, name := range manifest.Permissions.Env {
		allowed[name] = struct{}{}
	}

	secretNames := make(map[string]struct{}, len(manifest.Permissions.Secrets))
	for _, name := range manifest.Permissions.Secrets {
		allowed[name] = struct{}{}
		secretNames[name] = struct{}{}
	}

	for name := range explicit {
		if _, ok := allowed[name]; !ok {
			return nil, nil, fmt.Errorf("env %q was not declared in permissions", name)
		}
	}

	names := make([]string, 0, len(allowed))
	for name := range allowed {
		names = append(names, name)
	}

	sort.Strings(names)

	env := make([]string, 0, len(names))
	secrets := make([]secretValue, 0, len(secretNames))

	for _, name := range names {
		value, ok := explicit[name]
		if !ok {
			value, ok = os.LookupEnv(name)
		}

		if !ok {
			continue
		}

		env = append(env, name+"="+value)
		if _, isSecret := secretNames[name]; isSecret {
			secrets = append(secrets, secretValue{name: name, value: value})
		}
	}

	return env, secrets, nil
}

type boundedBuffer struct {
	buf       bytes.Buffer
	maxBytes  int
	total     int
	truncated bool
}

func newBoundedBuffer(maxBytes int) *boundedBuffer {
	return &boundedBuffer{maxBytes: maxBytes}
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.total += len(p)
	if b.maxBytes <= 0 {
		b.truncated = true

		return len(p), nil
	}

	remaining := b.maxBytes - b.buf.Len()
	if remaining <= 0 {
		b.truncated = true

		return len(p), nil
	}

	if len(p) > remaining {
		_, _ = b.buf.Write(p[:remaining])
		b.truncated = true

		return len(p), nil
	}

	_, _ = b.buf.Write(p)

	return len(p), nil
}

func (b *boundedBuffer) String(redactor outputRedactor) string {
	output := redactor.Redact(b.buf.String())
	if !b.truncated {
		return output
	}

	return output + fmt.Sprintf("\n[atteler: output truncated after %d bytes; process wrote %d bytes]\n", b.maxBytes, b.total)
}

func entrypointUsesShell(path string) (bool, error) {
	if strings.EqualFold(filepath.Ext(path), ".sh") {
		return true, nil
	}

	file, err := os.Open(path)
	if err != nil {
		return false, fmt.Errorf("inspect entrypoint: %w", err)
	}
	defer file.Close()

	buf := make([]byte, 256)

	n, err := file.Read(buf)
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("read entrypoint header: %w", err)
	}

	header := string(buf[:n])
	if !strings.HasPrefix(header, "#!") {
		return false, nil
	}

	firstLine, _, _ := strings.Cut(header, "\n")

	return shebangUsesShell(firstLine), nil
}

func shebangUsesShell(line string) bool {
	for field := range strings.FieldsSeq(strings.TrimPrefix(line, "#!")) {
		if isShellName(filepath.Base(field)) {
			return true
		}
	}

	return false
}

func isShellName(name string) bool {
	switch name {
	case "sh", "bash", "dash", "zsh", "fish", "ksh", "mksh", "csh", "tcsh":
		return true
	default:
		return false
	}
}

func resolveEntrypoint(root, entrypoint string) (resolvedRoot, resolvedTarget string, err error) {
	if validateErr := validateEntrypoint(root, entrypoint); validateErr != nil {
		return "", "", validateErr
	}

	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", "", fmt.Errorf("resolve plugin root: %w", err)
	}

	rootResolved, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return "", "", fmt.Errorf("resolve plugin root symlinks: %w", err)
	}

	targetAbs, err := filepath.Abs(filepath.Join(rootAbs, strings.TrimSpace(entrypoint)))
	if err != nil {
		return "", "", fmt.Errorf("resolve path: %w", err)
	}

	targetResolved, err := filepath.EvalSymlinks(targetAbs)
	if err != nil {
		return "", "", fmt.Errorf("resolve path symlinks: %w", err)
	}

	rel, err := filepath.Rel(rootResolved, targetResolved)
	if err != nil {
		return "", "", fmt.Errorf("compare with plugin root: %w", err)
	}

	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", "", fmt.Errorf("path %q escapes plugin root %q", entrypoint, root)
	}

	return rootAbs, targetResolved, nil
}
