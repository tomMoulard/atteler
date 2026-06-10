package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/tommoulard/atteler/pkg/permission"
	attshell "github.com/tommoulard/atteler/pkg/shell"
)

// RunResult contains the output captured while running a plugin entrypoint.
type RunResult struct {
	Structured any
	Stdout     string
	Stderr     string
}

// RunOptions controls a plugin entrypoint execution.
//
//nolint:govet // Field order follows execution flow readability.
type RunOptions struct {
	Policy         *Policy
	Permission     *permission.Policy
	Env            map[string]string
	Autonomy       string
	AuditDir       string
	Timeout        time.Duration
	Args           []string
	AttelerVersion string
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
//
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
	entrypointName, validationErr := validateRunEntrypointRequest(ctx, entrypointName, options)
	if validationErr != nil {
		return RunResult{}, validationErr
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
	if err := authorizeRun(root, manifest, entrypointName, policy, options.AttelerVersion); err != nil {
		return RunResult{}, fmt.Errorf("plugin: authorize entrypoint %q: %w", entrypointName, err)
	}

	argSchema, _ := entrypointArgsFor(manifest, entrypointName)

	args, err := validateRunArgs(entrypointName, argSchema, options.Args)
	if err != nil {
		return RunResult{}, fmt.Errorf("plugin: authorize entrypoint %q: %w", entrypointName, err)
	}

	env, secrets, err := buildPluginEnvironment(manifest, options.Env)
	if err != nil {
		return RunResult{}, fmt.Errorf("plugin: authorize entrypoint %q: %w", entrypointName, err)
	}

	autonomyLevel := strings.TrimSpace(options.Autonomy)
	if autonomyLevel != "" {
		env = append(env, "ATTELER_AUTONOMY="+autonomyLevel)
	}

	rootAbs, targetAbs, err := resolveEntrypoint(root, entrypoint)
	if err != nil {
		return RunResult{}, fmt.Errorf("plugin: resolve entrypoint %q: %w", entrypointName, err)
	}

	if shapeErr := authorizeEntrypointRuntimeShape(targetAbs, manifest); shapeErr != nil {
		return RunResult{}, fmt.Errorf("plugin: authorize entrypoint %q: %w", entrypointName, shapeErr)
	}

	runCtx, cancel := context.WithTimeout(ctx, options.Timeout)
	defer cancel()

	stdout := newBoundedBuffer(manifest.Output.StdoutMaxBytes)
	stderr := newBoundedBuffer(manifest.Output.StderrMaxBytes)

	cmd, invocation, err := attshell.CommandContext(runCtx, attshell.CommandOptions{
		Program:    targetAbs,
		Args:       args,
		Dir:        rootAbs,
		EnvList:    env,
		EnvMode:    attshell.EnvModeExplicitOnly,
		Mode:       attshell.ModeCaptured,
		Stdout:     stdout,
		Stderr:     stderr,
		Permission: options.Permission,
		PermissionOperations: pluginPermissionOperations(
			manifest,
			entrypointName,
			targetAbs,
		),
		Policy: shellPolicyForPlugin(targetAbs, manifest, secrets),
		Audit: attshell.AuditContext{
			Caller:   "atteler.plugin." + entrypointName,
			Autonomy: autonomyLevel,
			AuditDir: strings.TrimSpace(options.AuditDir),
		},
	})
	if err != nil {
		return RunResult{}, fmt.Errorf("plugin: authorize entrypoint %q: %w", entrypointName, err)
	}

	runErr := cmd.Run()

	return finishPluginRun(runCtx, entrypointName, manifest, runErr, invocation, stdout, stderr, secrets)
}

func finishPluginRun(
	runCtx context.Context,
	entrypointName string,
	manifest Manifest,
	runErr error,
	invocation *attshell.Invocation,
	stdout *boundedBuffer,
	stderr *boundedBuffer,
	secrets []secretValue,
) (RunResult, error) {
	redactor := outputRedactor{secrets: secrets}

	result := RunResult{
		Stdout: stdout.String(redactor),
		Stderr: stderr.String(redactor),
	}

	executionErr := runErr
	if runCtx.Err() != nil {
		executionErr = runCtx.Err()
	}

	var structuredErr error
	if executionErr == nil {
		structuredErr = adaptStructuredOutput(entrypointName, manifest, &result)
		if structuredErr != nil {
			executionErr = structuredErr
		}
	}

	finishErr := invocation.Finish(attshell.FinishOptions{
		Stdout:        result.Stdout,
		Stderr:        result.Stderr,
		Error:         executionErr,
		OutputCapture: attshell.OutputCaptured,
	})
	if runCtx.Err() != nil {
		return result, fmt.Errorf("plugin: run entrypoint %q: %w", entrypointName, runCtx.Err())
	}

	if runErr != nil {
		return result, fmt.Errorf("plugin: run entrypoint %q: %w", entrypointName, runErr)
	}

	if structuredErr != nil {
		return result, fmt.Errorf("plugin: structured output entrypoint %q: %w", entrypointName, structuredErr)
	}

	if finishErr != nil {
		return result, fmt.Errorf("plugin: audit entrypoint %q: %w", entrypointName, finishErr)
	}

	return result, nil
}

func shellPolicyForPlugin(targetAbs string, manifest Manifest, secrets []secretValue) *attshell.Policy {
	return &attshell.Policy{
		AllowCommands:      []string{targetAbs},
		AllowCredentialEnv: secretNames(secrets),
		DenyNetwork:        !manifest.Permissions.Network.Allow,
	}
}

func authorizeEntrypointRuntimeShape(targetAbs string, manifest Manifest) error {
	usesShell, err := entrypointUsesShell(targetAbs)
	if err != nil {
		return err
	}

	if usesShell && !manifest.Permissions.Shell.Allow {
		return errors.New("shell access must be declared in permissions")
	}

	return nil
}

func pluginPermissionOperations(
	manifest Manifest,
	entrypointName string,
	executable string,
) []permission.Operation {
	action := pluginPermissionAction(manifest.Name, entrypointName)
	source := pluginPermissionSource(manifest.Name, entrypointName)
	metadata := pluginPermissionMetadata(manifest.Name, entrypointName)
	operations := []permission.Operation{
		pluginPermissionOperation(permission.OperationExecute, action, executable, source, metadata),
	}

	if manifest.Permissions == nil {
		return operations
	}

	permissions := *manifest.Permissions
	if len(permissions.Filesystem.Read) > 0 {
		operations = append(operations, pluginPermissionOperation(
			permission.OperationRead,
			action,
			strings.Join(permissions.Filesystem.Read, ","),
			source,
			metadata,
		))
	}

	if len(permissions.Filesystem.Write) > 0 {
		operations = append(operations, pluginPermissionOperation(
			permission.OperationWrite,
			action,
			strings.Join(permissions.Filesystem.Write, ","),
			source,
			metadata,
		))
	}

	if permissions.Network.Allow {
		operations = append(operations, pluginPermissionOperation(
			permission.OperationNetwork,
			action,
			strings.Join(permissions.Network.Hosts, ","),
			source,
			metadata,
		))
	}

	if len(permissions.Secrets) > 0 {
		operations = append(operations, pluginPermissionOperation(
			permission.OperationCredentialAccess,
			action,
			strings.Join(permissions.Secrets, ","),
			source,
			metadata,
		))
	}

	return operations
}

func pluginPermissionAction(pluginName, entrypointName string) string {
	pluginName = strings.TrimSpace(pluginName)
	entrypointName = strings.TrimSpace(entrypointName)

	if pluginName != "" && entrypointName != "" {
		return "plugin entrypoint " + pluginName + "/" + entrypointName
	}

	if entrypointName != "" {
		return "plugin entrypoint " + entrypointName
	}

	if pluginName != "" {
		return "plugin " + pluginName
	}

	return "plugin entrypoint"
}

func pluginPermissionSource(pluginName, entrypointName string) string {
	parts := []string{"atteler", "plugin"}

	if pluginName = strings.TrimSpace(pluginName); pluginName != "" {
		parts = append(parts, pluginName)
	}

	if entrypointName = strings.TrimSpace(entrypointName); entrypointName != "" {
		parts = append(parts, entrypointName)
	}

	return strings.Join(parts, ".")
}

func pluginPermissionMetadata(pluginName, entrypointName string) map[string]string {
	metadata := map[string]string{
		"plugin":     strings.TrimSpace(pluginName),
		"entrypoint": strings.TrimSpace(entrypointName),
	}

	for key, value := range metadata {
		if value == "" {
			delete(metadata, key)
		}
	}

	return metadata
}

func pluginPermissionOperation(
	kind permission.OperationKind,
	action, target, source string,
	metadata map[string]string,
) permission.Operation {
	return permission.Operation{
		Metadata: cloneStringMap(metadata),
		Kind:     kind,
		Action:   action,
		Target:   target,
		Source:   source,
	}
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}

	out := make(map[string]string, len(in))
	maps.Copy(out, in)

	return out
}

func adaptStructuredOutput(entrypointName string, manifest Manifest, result *RunResult) error {
	output, ok := entrypointOutputContractFor(manifest, entrypointName)
	if !ok {
		return nil
	}

	if normalizedOutputFormat(output) == OutputFormatText {
		return nil
	}

	payload := strings.TrimSpace(result.Stdout)
	if payload == "" {
		return errors.New("expected JSON stdout, got empty output")
	}

	decoder := json.NewDecoder(strings.NewReader(payload))
	decoder.UseNumber()

	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return fmt.Errorf("parse JSON stdout: %w", err)
	}

	var extra any
	if err := decoder.Decode(&extra); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("parse JSON stdout: trailing data: %w", err)
	} else if err == nil {
		return errors.New("parse JSON stdout: trailing JSON values")
	}

	if output.Schema != nil {
		if err := validateJSONValue("$", decoded, *output.Schema); err != nil {
			return err
		}
	}

	result.Structured = decoded

	return nil
}

func validateJSONValue(path string, value any, schema JSONSchema) error {
	schemaType := normalizedJSONType(schema.Type)
	switch schemaType {
	case jsonTypeAny:
		return nil
	case jsonTypeObject:
		object, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: expected object", path)
		}

		for _, name := range schema.Required {
			if _, ok := object[name]; !ok {
				return fmt.Errorf("%s: missing required property %q", path, name)
			}
		}

		for name, property := range schema.Properties {
			propertyValue, ok := object[name]
			if !ok {
				continue
			}

			if err := validateJSONTypeValue(path+"."+name, propertyValue, normalizedJSONPropertyType(property.Type)); err != nil {
				return err
			}
		}

		return nil
	default:
		return validateJSONTypeValue(path, value, schemaType)
	}
}

func validateJSONTypeValue(path string, value any, schemaType string) error {
	switch schemaType {
	case jsonTypeAny:
		return nil
	case jsonTypeObject:
		if _, ok := value.(map[string]any); !ok {
			return fmt.Errorf("%s: expected object", path)
		}
	case jsonTypeArray:
		if _, ok := value.([]any); !ok {
			return fmt.Errorf("%s: expected array", path)
		}
	case jsonTypeString:
		if _, ok := value.(string); !ok {
			return fmt.Errorf("%s: expected string", path)
		}
	case jsonTypeNumber:
		if _, ok := value.(json.Number); !ok {
			return fmt.Errorf("%s: expected number", path)
		}
	case jsonTypeInteger:
		number, ok := value.(json.Number)
		if !ok {
			return fmt.Errorf("%s: expected integer", path)
		}

		if _, err := number.Int64(); err != nil {
			return fmt.Errorf("%s: expected integer", path)
		}
	case jsonTypeBoolean:
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("%s: expected boolean", path)
		}
	}

	return nil
}

func validateRunEntrypointRequest(ctx context.Context, entrypointName string, options RunOptions) (string, error) {
	if err := requireRunContext(ctx); err != nil {
		return "", err
	}

	if options.Timeout <= 0 {
		return "", errors.New("plugin: entrypoint timeout must be positive")
	}

	entrypointName = strings.TrimSpace(entrypointName)
	if entrypointName == "" {
		return "", errors.New("plugin: empty entrypoint name")
	}

	return entrypointName, nil
}

func requireRunContext(ctx context.Context) error {
	if ctx == nil {
		return errors.New("plugin: context is required")
	}

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("plugin: context already done: %w", err)
	}

	return nil
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

func secretNames(secrets []secretValue) []string {
	names := make([]string, 0, len(secrets))
	for _, secret := range secrets {
		names = append(names, secret.name)
	}

	return names
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
