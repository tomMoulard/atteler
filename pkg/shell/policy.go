//nolint:wsl_v5 // Command policy code keeps closely related audit/setup branches together.
package shell

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// EnvAuditDir overrides the directory used for command audit ledgers and
	// redacted output captures.
	EnvAuditDir = "ATTELER_COMMAND_AUDIT_DIR"

	ledgerFileName = "commands.jsonl"
)

// EnvMode controls which ambient environment variables are visible to a child
// process. The zero value keeps non-secret ambient variables and strips common
// credential-bearing names.
type EnvMode int

const (
	// EnvModeSanitizedAmbient inherits the ambient environment after removing
	// credential-like variables unless the policy explicitly allows them.
	EnvModeSanitizedAmbient EnvMode = iota
	// EnvModeExplicitOnly passes only variables supplied in CommandOptions.Env or
	// CommandOptions.EnvList.
	EnvModeExplicitOnly
	// EnvModeFullAmbient inherits the full non-credential ambient environment.
	// Credential-like names still require AllowCredentialEnv so secrets are never
	// propagated by accident.
	EnvModeFullAmbient
)

// ExecutionMode describes how command IO is handled for audit records.
type ExecutionMode string

const (
	// ModeCaptured records stdout/stderr through the audit output writer.
	ModeCaptured ExecutionMode = "captured"
	// ModeInteractive attaches the process to a terminal; stdout/stderr are not captured.
	ModeInteractive ExecutionMode = "interactive"
	// ModeStreaming covers long-lived or pipe-driven commands whose output is
	// consumed by another subsystem instead of the audit layer.
	ModeStreaming ExecutionMode = "streaming"
)

// OutputCapture describes whether command output is persisted by the audit ledger.
type OutputCapture string

const (
	// OutputCaptured means redacted stdout/stderr were persisted to OutputPath.
	OutputCaptured OutputCapture = "captured"
	// OutputNotCaptured means the command ran without captured output.
	OutputNotCaptured OutputCapture = "not_captured"
	// OutputSensitive means output was intentionally omitted because it may contain secrets.
	OutputSensitive OutputCapture = "sensitive_not_captured"
)

// AuditContext ties a command decision to the caller and current unit of work.
type AuditContext struct {
	Caller          string
	SessionID       string
	SessionPath     string
	IssueID         string
	IssueIdentifier string
	AuditDir        string
}

// Policy is the single allow/deny policy applied before every command start.
//
// Empty allow lists mean "allow unless denied". Credential-like environment
// variables are stripped by default unless listed in AllowCredentialEnv.
//
//nolint:govet // Field order keeps policy groups readable in docs and tests.
type Policy struct {
	AllowCommands      []string
	DenyCommands       []string
	AllowPathGlobs     []string
	DenyPathGlobs      []string
	AllowCredentialEnv []string
	DenyCredentialEnv  []string
	DenyNetwork        bool
	AllowDestructive   bool
	AuditDir           string
}

// DefaultPolicy returns Atteler's default local command policy.
func DefaultPolicy() Policy {
	return Policy{}
}

// PolicyError reports a command denied before process start.
type PolicyError struct {
	Command string
	Reason  string
	Rule    string
}

func (e *PolicyError) Error() string {
	if e == nil {
		return ""
	}

	if strings.TrimSpace(e.Rule) == "" {
		return "shell: command denied by policy: " + e.Reason
	}

	return "shell: command denied by policy (" + e.Rule + "): " + e.Reason
}

// EnvChange records how the policy changed the child process environment.
type EnvChange struct {
	Name   string `json:"name"`
	Action string `json:"action"`
	Value  string `json:"value,omitempty"`
}

// AuditRecord is one append-only command ledger event.
//
//nolint:govet // JSON field order follows audit-reading order.
type AuditRecord struct {
	StartedAt       time.Time   `json:"started_at,omitzero"`
	EndedAt         time.Time   `json:"ended_at,omitzero"`
	EnvDiff         []EnvChange `json:"env_diff,omitempty"`
	Args            []string    `json:"args,omitempty"`
	ID              string      `json:"id"`
	Phase           string      `json:"phase"`
	Program         string      `json:"program"`
	Command         string      `json:"command"`
	CWD             string      `json:"cwd,omitempty"`
	Caller          string      `json:"caller,omitempty"`
	SessionID       string      `json:"session_id,omitempty"`
	SessionPath     string      `json:"session_path,omitempty"`
	IssueID         string      `json:"issue_id,omitempty"`
	IssueIdentifier string      `json:"issue_identifier,omitempty"`
	Mode            string      `json:"mode"`
	Decision        string      `json:"decision"`
	DecisionReason  string      `json:"decision_reason,omitempty"`
	DecisionRule    string      `json:"decision_rule,omitempty"`
	OutputCapture   string      `json:"output_capture,omitempty"`
	OutputPath      string      `json:"output_path,omitempty"`
	OutputNote      string      `json:"output_note,omitempty"`
	Error           string      `json:"error,omitempty"`
	ExitStatus      *int        `json:"exit_status,omitempty"`
	DurationMillis  int64       `json:"duration_ms,omitempty"`
}

// CommandOptions describes one process launch through the policy/audit gate.
//
//nolint:govet // Field order keeps execution inputs before metadata.
type CommandOptions struct {
	Stdin        io.Reader
	Stdout       io.Writer
	Stderr       io.Writer
	Policy       *Policy
	Env          map[string]string
	Audit        AuditContext
	Mode         ExecutionMode
	EnvMode      EnvMode
	Program      string
	Command      string
	Dir          string
	Args         []string
	EnvList      []string
	SecretValues []string
}

// FinishOptions records the result of an already-authorized command.
type FinishOptions struct {
	Stdout        string
	Stderr        string
	Error         error
	OutputCapture OutputCapture
	OutputNote    string
}

// Invocation tracks an authorized command until the caller records completion.
//
//nolint:govet // Field order follows lifecycle metadata, not memory layout.
type Invocation struct {
	auditDir   string
	secrets    []secretValue
	record     AuditRecord
	startTime  time.Time
	finishOnce sync.Once
	finishErr  error
}

// CommandContext authorizes a command, records the decision, and constructs an
// exec.Cmd only after policy allows the launch. Callers that do not use
// RunCommand must call Invocation.Finish after cmd.Run/Wait returns.
func CommandContext(ctx context.Context, opts CommandOptions) (*exec.Cmd, *Invocation, error) {
	if ctx == nil {
		return nil, nil, errors.New("shell: context is required")
	}

	program := strings.TrimSpace(opts.Program)
	if program == "" {
		return nil, nil, errors.New("shell: program is required")
	}

	cwd, err := commandCWD(opts.Dir)
	if err != nil {
		return nil, nil, err
	}

	opts.Dir = cwd

	policy := effectivePolicy(opts.Policy)
	if strings.TrimSpace(opts.Audit.AuditDir) != "" {
		policy.AuditDir = opts.Audit.AuditDir
	}

	env, diff, secrets := buildCommandEnvironment(opts, policy)
	secrets = appendCommandSecrets(secrets, opts.SecretValues)
	secrets = appendCredentialAssignmentSecrets(secrets, opts.Command)
	secrets = appendCredentialAssignmentSecrets(secrets, opts.Args...)
	rawCommand := policyCommand(program, opts.Args, opts.Command)
	auditArgs := redactArgValues(opts.Args, opts.SecretValues)
	auditArgs = redactArgs(auditArgs, secrets)
	command := redactText(displayCommand(program, auditArgs, opts.Command), secrets)
	mode := opts.Mode
	if mode == "" {
		mode = ModeCaptured
	}

	inv := &Invocation{
		auditDir:  auditDir(policy),
		secrets:   secrets,
		startTime: time.Now().UTC(),
		record: AuditRecord{
			ID:              nextAuditID(),
			Program:         program,
			Args:            auditArgs,
			Command:         command,
			CWD:             cwd,
			Caller:          strings.TrimSpace(opts.Audit.Caller),
			SessionID:       strings.TrimSpace(opts.Audit.SessionID),
			SessionPath:     strings.TrimSpace(opts.Audit.SessionPath),
			IssueID:         strings.TrimSpace(opts.Audit.IssueID),
			IssueIdentifier: strings.TrimSpace(opts.Audit.IssueIdentifier),
			Mode:            string(mode),
			EnvDiff:         diff,
		},
	}

	decision := authorizeCommand(opts, policy, rawCommand)
	if !decision.allowed {
		inv.record.Phase = "denied"
		inv.record.Decision = "denied"
		inv.record.DecisionReason = decision.reason
		inv.record.DecisionRule = decision.rule
		inv.record.StartedAt = inv.startTime
		inv.record.EndedAt = time.Now().UTC()
		inv.record.DurationMillis = inv.record.EndedAt.Sub(inv.startTime).Milliseconds()
		if err := inv.appendRecord(inv.record); err != nil {
			return nil, nil, err
		}

		return nil, nil, &PolicyError{Command: command, Reason: decision.reason, Rule: decision.rule}
	}

	inv.record.Phase = "start"
	inv.record.Decision = "allowed"
	inv.record.DecisionReason = decision.reason
	inv.record.DecisionRule = decision.rule
	inv.record.StartedAt = inv.startTime
	if err := inv.appendRecord(inv.record); err != nil {
		return nil, nil, err
	}

	cmd := exec.CommandContext(ctx, program, opts.Args...)
	cmd.Dir = cwd
	cmd.Env = env
	cmd.Stdin = opts.Stdin
	cmd.Stdout = opts.Stdout
	cmd.Stderr = opts.Stderr

	return cmd, inv, nil
}

func commandCWD(dir string) (string, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("shell: resolve current working directory: %w", err)
		}

		return filepath.Clean(cwd), nil
	}

	if filepath.IsAbs(dir) {
		return filepath.Clean(dir), nil
	}

	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("shell: resolve working directory %q: %w", dir, err)
	}

	return filepath.Clean(abs), nil
}

// RunCommand runs a command with captured stdout/stderr through the policy gate.
func RunCommand(ctx context.Context, opts CommandOptions) (Result, error) {
	stdout, stderr, outputLimit := commandOutputWriters(0, nil)
	opts.Stdout = stdout
	opts.Stderr = stderr

	cmd, inv, err := CommandContext(ctx, opts)
	if err != nil {
		return Result{}, err
	}

	runErr := cmd.Run()
	result := Result{
		StartedAt:       inv.startTime,
		Duration:        time.Since(inv.startTime),
		Stdout:          stdout.String(),
		Stderr:          stderr.String(),
		OutputTruncated: outputLimit.truncatedOutput(),
	}
	if runErr != nil {
		result.ExitError = runErr.Error()
	}

	if finishErr := inv.Finish(FinishOptions{
		Stdout:        result.Stdout,
		Stderr:        result.Stderr,
		Error:         runErr,
		OutputCapture: OutputCaptured,
	}); finishErr != nil && runErr == nil {
		return result, finishErr
	}

	if runErr != nil {
		return result, fmt.Errorf("shell: command failed: %w", runErr)
	}

	return result, nil
}

// Finish appends the final command audit record. It is safe to call multiple
// times; only the first call is recorded.
func (i *Invocation) Finish(opts FinishOptions) error {
	if i == nil {
		return nil
	}

	i.finishOnce.Do(func() {
		record := i.record
		record.Phase = "finish"
		record.Decision = "allowed"
		record.StartedAt = i.startTime
		record.EndedAt = time.Now().UTC()
		record.DurationMillis = record.EndedAt.Sub(i.startTime).Milliseconds()
		record.Error = errorString(opts.Error)
		record.ExitStatus = exitStatus(opts.Error)

		capture := opts.OutputCapture
		if capture == "" {
			capture = OutputCaptured
		}

		record.OutputCapture = string(capture)
		record.OutputNote = strings.TrimSpace(opts.OutputNote)
		if capture == OutputCaptured {
			path, err := i.writeOutput(opts.Stdout, opts.Stderr)
			if err != nil {
				record.OutputCapture = string(OutputNotCaptured)
				record.OutputNote = appendOutputNote(record.OutputNote, "redacted output capture failed: "+err.Error())
				i.finishErr = errors.Join(err, i.appendRecord(record))
				return
			}

			record.OutputPath = path
		}

		i.finishErr = i.appendRecord(record)
	})

	return i.finishErr
}

func appendOutputNote(existing, note string) string {
	existing = strings.TrimSpace(existing)
	note = strings.TrimSpace(note)
	if existing == "" {
		return note
	}

	if note == "" {
		return existing
	}

	return existing + "; " + note
}

//nolint:govet // Field order keeps the allow flag before human-readable detail.
type policyDecision struct {
	allowed bool
	reason  string
	rule    string
}

//nolint:cyclop // Authorization is intentionally linear so rule ordering stays auditable.
func authorizeCommand(opts CommandOptions, policy Policy, command string) policyDecision {
	program := strings.TrimSpace(opts.Program)
	base := filepath.Base(program)

	if denied, ok := commandOutsideAllowList(program, command, policy.AllowCommands); ok {
		return policyDecision{reason: fmt.Sprintf("command %q is not in allow list", denied), rule: "command.allow"}
	}

	if matchesAnyCommand(program, policy.DenyCommands) {
		return policyDecision{reason: fmt.Sprintf("command %q is denied", base), rule: "command.deny"}
	}

	if denied, ok := deniedShellCommand(command, policy.DenyCommands); ok {
		return policyDecision{reason: fmt.Sprintf("command %q is denied", denied), rule: "command.deny"}
	}

	if deniedEnv, ok := deniedCredentialAssignment(command, policy); ok {
		return policyDecision{reason: fmt.Sprintf("credential environment assignment %q requires explicit policy allowance", deniedEnv), rule: "env.deny"}
	}

	if decision, ok := authorizeProgramPath(program, policy); ok {
		return decision
	}

	if dir := strings.TrimSpace(opts.Dir); dir != "" {
		if matchesAnyGlob(dir, policy.DenyPathGlobs) {
			return policyDecision{reason: fmt.Sprintf("cwd %q matches a denied path", dir), rule: "path.deny"}
		}

		if len(policy.AllowPathGlobs) > 0 && !matchesAnyGlob(dir, policy.AllowPathGlobs) {
			return policyDecision{reason: fmt.Sprintf("cwd %q is outside allowed paths", dir), rule: "path.allow"}
		}
	}

	if deniedPath, ok := deniedPathArgument(opts.Dir, opts.Args, command, policy.DenyPathGlobs); ok {
		return policyDecision{reason: fmt.Sprintf("path argument %q matches a denied path", deniedPath), rule: "path.deny"}
	}

	if allowedPath, ok := disallowedPathArgument(opts.Dir, opts.Args, command, policy.AllowPathGlobs); ok {
		return policyDecision{reason: fmt.Sprintf("path argument %q is outside allowed paths", allowedPath), rule: "path.allow"}
	}

	if policy.DenyNetwork && isNetworkCommand(program, opts.Args, command) {
		return policyDecision{reason: "network-like command requires explicit policy allowance", rule: "network.deny"}
	}

	if !policy.AllowDestructive && isDestructiveCommand(program, opts.Args, command) {
		return policyDecision{reason: "destructive command pattern requires explicit policy allowance", rule: "destructive.deny"}
	}

	return policyDecision{allowed: true, reason: "allowed by policy", rule: "policy.allow"}
}

func authorizeProgramPath(program string, policy Policy) (policyDecision, bool) {
	if deniedPath, ok := deniedProgramPath(program, policy.DenyPathGlobs); ok {
		return policyDecision{reason: fmt.Sprintf("program path %q matches a denied path", deniedPath), rule: "path.deny"}, true
	}

	if disallowedPath, ok := disallowedProgramPath(program, policy.AllowPathGlobs); ok {
		return policyDecision{reason: fmt.Sprintf("program path %q is outside allowed paths", disallowedPath), rule: "path.allow"}, true
	}

	return policyDecision{}, false
}

func effectivePolicy(policy *Policy) Policy {
	if policy == nil {
		return DefaultPolicy()
	}

	return *policy
}

//nolint:gocognit // Environment sanitization is easier to audit when the diff and secret tracking stay together.
func buildCommandEnvironment(opts CommandOptions, policy Policy) ([]string, []EnvChange, []secretValue) {
	ambient := envMap(os.Environ())
	child := make(map[string]string)
	diff := make([]EnvChange, 0)
	secrets := make([]secretValue, 0)

	if opts.EnvMode != EnvModeExplicitOnly {
		for key, value := range ambient {
			if envDenied(key, policy) || (credentialEnv(key) && !envAllowed(key, policy)) {
				diff = append(diff, EnvChange{Name: key, Action: "redacted"})
				if value != "" {
					secrets = append(secrets, secretValue{name: key, value: value})
				}

				continue
			}

			child[key] = value
			if credentialEnv(key) && value != "" {
				secrets = append(secrets, secretValue{name: key, value: value})
			}
		}
	}

	applyExplicitEnv(child, ambient, opts.EnvList, policy, &diff, &secrets)
	if len(opts.Env) > 0 {
		keys := make([]string, 0, len(opts.Env))
		for key := range opts.Env {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		pairs := make([]string, 0, len(keys))
		for _, key := range keys {
			pairs = append(pairs, key+"="+opts.Env[key])
		}
		applyExplicitEnv(child, ambient, pairs, policy, &diff, &secrets)
	}

	return flattenEnv(child), diff, secrets
}

func applyExplicitEnv(child, ambient map[string]string, pairs []string, policy Policy, diff *[]EnvChange, secrets *[]secretValue) {
	for _, pair := range pairs {
		key, value, ok := strings.Cut(pair, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			continue
		}

		if envDenied(key, policy) || (credentialEnv(key) && !envAllowed(key, policy)) {
			*diff = append(*diff, EnvChange{Name: key, Action: "redacted"})
			if value != "" {
				*secrets = append(*secrets, secretValue{name: key, value: value})
			}

			delete(child, key)
			continue
		}

		if old, exists := ambient[key]; !exists {
			*diff = append(*diff, EnvChange{Name: key, Action: "added", Value: envAuditValue(key)})
		} else if old != value {
			*diff = append(*diff, EnvChange{Name: key, Action: "changed", Value: envAuditValue(key)})
		}

		child[key] = value
		if credentialEnv(key) && value != "" {
			*secrets = append(*secrets, secretValue{name: key, value: value})
		}
	}
}

func appendCommandSecrets(secrets []secretValue, values []string) []secretValue {
	for _, value := range values {
		if value == "" {
			continue
		}

		secrets = append(secrets, secretValue{name: "command_arg", value: value})
	}

	return secrets
}

func appendCredentialAssignmentSecrets(secrets []secretValue, texts ...string) []secretValue {
	for _, text := range texts {
		for _, token := range shellTokens(text) {
			if token.operator || !shellAssignment(token.value) {
				continue
			}

			name, value, _ := strings.Cut(token.value, "=")
			if strings.TrimSpace(value) == "" || !credentialEnv(name) {
				continue
			}

			secrets = append(secrets, secretValue{name: strings.TrimSpace(name), value: value})
		}
	}

	return secrets
}

func redactArgValues(args, values []string) []string {
	redacted := append([]string(nil), args...)
	for i, arg := range redacted {
		for _, value := range values {
			if value == "" {
				continue
			}

			arg = strings.ReplaceAll(arg, value, "<redacted:command_arg>")
		}

		redacted[i] = arg
	}

	return redacted
}

func redactArgs(args []string, secrets []secretValue) []string {
	redacted := append([]string(nil), args...)
	for i, arg := range redacted {
		redacted[i] = redactText(arg, secrets)
	}

	return redacted
}

func envMap(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, pair := range env {
		key, value, ok := strings.Cut(pair, "=")
		if !ok || strings.TrimSpace(key) == "" {
			continue
		}

		out[key] = value
	}

	return out
}

func flattenEnv(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}

	slices.Sort(keys)

	env := make([]string, 0, len(keys))
	for _, key := range keys {
		env = append(env, key+"="+values[key])
	}

	return env
}

func envDenied(key string, policy Policy) bool {
	return slices.ContainsFunc(policy.DenyCredentialEnv, func(denied string) bool {
		return envNameMatches(denied, key)
	})
}

func envAllowed(key string, policy Policy) bool {
	return slices.ContainsFunc(policy.AllowCredentialEnv, func(allowed string) bool {
		return envNameMatches(allowed, key)
	})
}

func envNameMatches(pattern, key string) bool {
	pattern = strings.TrimSpace(pattern)
	key = strings.TrimSpace(key)
	if pattern == "" || key == "" {
		return false
	}

	if strings.EqualFold(pattern, key) {
		return true
	}

	ok, err := filepath.Match(strings.ToUpper(pattern), strings.ToUpper(key))

	return err == nil && ok
}

func envAuditValue(key string) string {
	if credentialEnv(key) {
		return "<redacted>"
	}

	return "<set>"
}

func credentialEnv(key string) bool {
	upper := strings.ToUpper(strings.TrimSpace(key))
	if upper == "" {
		return false
	}

	markers := []string{"TOKEN", "SECRET", "PASSWORD", "PASSWD", "CREDENTIAL", "COOKIE", "PRIVATE_KEY", "ACCESS_KEY", "API_KEY", "APIKEY"}
	for _, marker := range markers {
		if strings.Contains(upper, marker) {
			return true
		}
	}

	if upper == "AUTH" ||
		upper == "AUTHORIZATION" ||
		upper == "PAT" ||
		strings.Contains(upper, "AUTHORIZATION") ||
		strings.Contains(upper, "_AUTH_") ||
		strings.Contains(upper, "_PAT_") ||
		strings.HasPrefix(upper, "AUTH_") ||
		strings.HasPrefix(upper, "PAT_") ||
		strings.HasSuffix(upper, "_AUTH") ||
		strings.HasSuffix(upper, "_PAT") {
		return true
	}

	return strings.HasSuffix(upper, "_KEY") || strings.HasSuffix(upper, "KEY")
}

func credentialAssignmentDenied(key string, policy Policy) bool {
	return envDenied(key, policy) || (credentialEnv(key) && !envAllowed(key, policy))
}

func matchesAnyCommand(program string, patterns []string) bool {
	program = strings.TrimSpace(program)
	base := filepath.Base(program)
	for _, pattern := range patterns {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}

		if pattern == program || pattern == base {
			return true
		}

		if ok, err := filepath.Match(pattern, program); err == nil && ok {
			return true
		}

		if ok, err := filepath.Match(pattern, base); err == nil && ok {
			return true
		}
	}

	return false
}

func commandOutsideAllowList(program, command string, patterns []string) (string, bool) {
	if len(patterns) == 0 {
		return "", false
	}

	if commandAllowListShouldInspectWords(program) && strings.TrimSpace(command) != "" {
		names := shellCommandNames(command)
		for _, name := range names {
			if !matchesAnyCommand(name, patterns) {
				return name, true
			}
		}

		return "", false
	}

	if !matchesAnyCommand(program, patterns) {
		return filepath.Base(program), true
	}

	return "", false
}

func commandAllowListShouldInspectWords(program string) bool {
	return shellProgram(program) || shellWrapper(program)
}

func shellProgram(program string) bool {
	base := strings.TrimSuffix(strings.ToLower(filepath.Base(strings.TrimSpace(program))), ".exe")
	switch base {
	case "bash", "sh", "zsh", "fish":
		return true
	default:
		return false
	}
}

func shellEval(program string) bool {
	base := strings.TrimSuffix(strings.ToLower(filepath.Base(strings.TrimSpace(program))), ".exe")

	return base == "eval"
}

func deniedShellCommand(command string, patterns []string) (string, bool) {
	for _, name := range shellCommandNames(command) {
		if matchesAnyCommand(name, patterns) {
			return name, true
		}
	}

	return "", false
}

func deniedCredentialAssignment(command string, policy Policy) (string, bool) {
	if name, ok := shellTokensDeniedCredentialAssignment(shellTokens(command), policy, 0); ok {
		return name, true
	}

	return shellSubstitutionsDeniedCredentialAssignment(command, policy, 0)
}

//nolint:cyclop,gocognit,nestif // Mirrors command scanning so env assignment policy sees wrappers and nested scripts.
func shellTokensDeniedCredentialAssignment(tokens []shellToken, policy Policy, depth int) (string, bool) {
	expectCommand := true
	activeWrapper := ""
	skipWrapperValue := false

	for i, token := range tokens {
		if token.operator {
			expectCommand = true
			activeWrapper = ""
			skipWrapperValue = false
			continue
		}

		if expectCommand {
			if skipWrapperValue {
				skipWrapperValue = false
				continue
			}

			if activeWrapper != "" && strings.HasPrefix(token.value, "-") {
				skipWrapperValue = shellWrapperOptionTakesValue(activeWrapper, token.value)
				continue
			}

			if shellAssignment(token.value) {
				name, _, _ := strings.Cut(token.value, "=")
				name = strings.TrimSpace(name)
				if credentialAssignmentDenied(name, policy) {
					return name, true
				}

				continue
			}

			if wrapper, ok := shellWrapperCommand(token.value); ok {
				activeWrapper = wrapper
				continue
			}
		}

		name, resetCommand, ok := shellCommandCandidate(token.value, expectCommand)
		if resetCommand {
			expectCommand = true
			activeWrapper = ""
			skipWrapperValue = false
			continue
		}
		if !ok {
			continue
		}

		if shellEval(name) && depth < maxNestedShellInspectionDepth {
			if script := evalScriptArgumentFromTokens(tokens[i+1:]); script != "" {
				if denied, ok := shellTokensDeniedCredentialAssignment(shellTokens(script), policy, depth+1); ok {
					return denied, true
				}

				if denied, ok := shellSubstitutionsDeniedCredentialAssignment(script, policy, depth+1); ok {
					return denied, true
				}
			}
		}

		if shellProgram(name) && depth < maxNestedShellInspectionDepth {
			if script, ok := shellScriptArgumentFromTokens(tokens[i+1:]); ok {
				if denied, ok := shellTokensDeniedCredentialAssignment(shellTokens(script), policy, depth+1); ok {
					return denied, true
				}

				if denied, ok := shellSubstitutionsDeniedCredentialAssignment(script, policy, depth+1); ok {
					return denied, true
				}
			}
		}

		expectCommand = false
		activeWrapper = ""
	}

	return "", false
}

func shellSubstitutionsDeniedCredentialAssignment(command string, policy Policy, depth int) (string, bool) {
	if depth >= maxNestedShellInspectionDepth {
		return "", false
	}

	for _, script := range shellCommandSubstitutions(command) {
		if denied, ok := shellTokensDeniedCredentialAssignment(shellTokens(script), policy, depth+1); ok {
			return denied, true
		}

		if denied, ok := shellSubstitutionsDeniedCredentialAssignment(script, policy, depth+1); ok {
			return denied, true
		}
	}

	return "", false
}

func matchesAnyGlob(path string, globs []string) bool {
	path = filepath.Clean(path)
	for _, glob := range globs {
		glob = strings.TrimSpace(glob)
		if glob == "" {
			continue
		}

		if ok, err := filepath.Match(glob, path); err == nil && ok {
			return true
		}

		if ok, err := filepath.Match(glob, filepath.ToSlash(path)); err == nil && ok {
			return true
		}
	}

	return false
}

func deniedPathArgument(dir string, args []string, command string, globs []string) (string, bool) {
	for _, path := range pathArguments(dir, args, command) {
		if matchesAnyGlob(path, globs) {
			return path, true
		}
	}

	return "", false
}

func disallowedPathArgument(dir string, args []string, command string, globs []string) (string, bool) {
	if len(globs) == 0 {
		return "", false
	}

	for _, path := range pathArguments(dir, args, command) {
		if !matchesAnyGlob(path, globs) {
			return path, true
		}
	}

	return "", false
}

func deniedProgramPath(program string, globs []string) (string, bool) {
	path, ok := programPathArgument(program)
	if !ok {
		return "", false
	}

	if matchesAnyGlob(path, globs) {
		return path, true
	}

	return "", false
}

func disallowedProgramPath(program string, globs []string) (string, bool) {
	if len(globs) == 0 {
		return "", false
	}

	path, ok := programPathArgument(program)
	if !ok {
		return "", false
	}

	if !matchesAnyGlob(path, globs) {
		return path, true
	}

	return "", false
}

func programPathArgument(program string) (string, bool) {
	program = strings.TrimSpace(program)
	if !pathLikeArgument(program) {
		return "", false
	}

	return filepath.Clean(program), true
}

func pathArguments(dir string, args []string, command string) []string {
	paths := make([]string, 0)
	seen := make(map[string]struct{})
	for _, arg := range args {
		if strings.TrimSpace(command) != "" && strings.TrimSpace(arg) == strings.TrimSpace(command) {
			continue
		}

		addPathArgument(dir, arg, &paths, seen)
	}

	appendShellPathArguments(dir, shellTokens(command), &paths, seen, 0)
	appendShellSubstitutionPathArguments(dir, command, &paths, seen, 0)

	return paths
}

//nolint:gocognit // Path scanning handles wrappers plus nested/eval shell scripts in one auditable pass.
func appendShellPathArguments(dir string, tokens []shellToken, paths *[]string, seen map[string]struct{}, depth int) {
	expectCommand := true
	skipPathArgument := make(map[int]struct{})

	for i, token := range tokens {
		if token.operator {
			expectCommand = true
			continue
		}

		if _, skip := skipPathArgument[i]; !skip {
			addPathArgument(dir, token.value, paths, seen)
			addEmbeddedRedirectionPathArguments(dir, token.value, paths, seen)
		}

		name, resetCommand, ok := shellCommandCandidate(token.value, expectCommand)
		if resetCommand {
			expectCommand = true
			continue
		}
		if !ok {
			continue
		}

		if shellEval(name) && depth < maxNestedShellInspectionDepth {
			if scriptIndexes, script := evalScriptArgumentIndexesFromTokens(tokens[i+1:]); script != "" {
				for _, scriptIndex := range scriptIndexes {
					skipPathArgument[i+1+scriptIndex] = struct{}{}
				}

				appendShellPathArguments(dir, shellTokens(script), paths, seen, depth+1)
				appendShellSubstitutionPathArguments(dir, script, paths, seen, depth+1)
			}
		}

		if shellProgram(name) && depth < maxNestedShellInspectionDepth {
			if scriptIndex, script, ok := shellScriptArgumentIndexFromTokens(tokens[i+1:]); ok {
				skipPathArgument[i+1+scriptIndex] = struct{}{}
				appendShellPathArguments(dir, shellTokens(script), paths, seen, depth+1)
			}
		}

		expectCommand = false
	}
}

func appendShellSubstitutionPathArguments(dir, command string, paths *[]string, seen map[string]struct{}, depth int) {
	if depth >= maxNestedShellInspectionDepth {
		return
	}

	for _, script := range shellCommandSubstitutions(command) {
		appendShellPathArguments(dir, shellTokens(script), paths, seen, depth+1)
		appendShellSubstitutionPathArguments(dir, script, paths, seen, depth+1)
	}
}

func addPathArgument(dir, arg string, paths *[]string, seen map[string]struct{}) {
	arg = normalizePathArgument(arg)
	if !pathLikeArgument(arg) {
		return
	}

	addUniquePath(paths, seen, arg)
	if !filepath.IsAbs(arg) && strings.TrimSpace(dir) != "" && !strings.HasPrefix(arg, "~") {
		addUniquePath(paths, seen, filepath.Join(dir, arg))
	}
}

func addEmbeddedRedirectionPathArguments(dir, arg string, paths *[]string, seen map[string]struct{}) {
	for _, fragment := range embeddedRedirectionFragments(arg) {
		addPathArgument(dir, fragment, paths, seen)
	}
}

func normalizePathArgument(arg string) string {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return ""
	}

	arg = stripLongOptionValue(arg)
	arg = stripShortOptionPathValue(arg)

	if rest, ok := strings.CutPrefix(arg, "&>>"); ok {
		arg = rest
	} else if rest, ok := strings.CutPrefix(arg, "&>"); ok {
		arg = rest
	}

	if index := firstNonDigitIndex(arg); index > 0 && index < len(arg) && (arg[index] == '>' || arg[index] == '<') {
		arg = arg[index:]
	}

	if strings.HasPrefix(arg, "<<") || strings.HasPrefix(arg, "<(") || strings.HasPrefix(arg, ">&") {
		return ""
	}

	for strings.HasPrefix(arg, ">") || strings.HasPrefix(arg, "<") {
		arg = strings.TrimPrefix(strings.TrimPrefix(arg, ">"), "<")
	}

	if strings.HasPrefix(arg, "&") {
		return ""
	}

	return strings.TrimSpace(arg)
}

func stripLongOptionValue(arg string) string {
	if !strings.HasPrefix(arg, "--") {
		return arg
	}

	_, value, ok := strings.Cut(arg, "=")
	if !ok {
		return arg
	}

	return value
}

func stripShortOptionPathValue(arg string) string {
	if strings.HasPrefix(arg, "--") || !strings.HasPrefix(arg, "-") || len(arg) <= 2 {
		return arg
	}

	for index := 2; index < len(arg); index++ {
		suffix := arg[index:]
		if pathLikeArgument(suffix) {
			return suffix
		}
	}

	return arg
}

func embeddedRedirectionFragments(arg string) []string {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return nil
	}

	fragments := make([]string, 0)
	for i := 1; i < len(arg); i++ {
		if arg[i] != '>' && arg[i] != '<' {
			continue
		}

		fragments = append(fragments, arg[i:])
	}

	return fragments
}

func firstNonDigitIndex(value string) int {
	for i, r := range value {
		if r < '0' || r > '9' {
			return i
		}
	}

	return len(value)
}

func addUniquePath(paths *[]string, seen map[string]struct{}, path string) {
	path = filepath.Clean(path)
	if _, ok := seen[path]; ok {
		return
	}

	seen[path] = struct{}{}
	*paths = append(*paths, path)
}

func pathLikeArgument(arg string) bool {
	if arg == "" || strings.HasPrefix(arg, "-") {
		return false
	}

	if filepath.IsAbs(arg) || strings.HasPrefix(arg, "~/") || strings.HasPrefix(arg, `~\`) {
		return true
	}

	return strings.HasPrefix(arg, "./") ||
		strings.HasPrefix(arg, "../") ||
		strings.HasPrefix(arg, `.\`) ||
		strings.HasPrefix(arg, `..\`) ||
		strings.Contains(arg, "/") ||
		strings.Contains(arg, `\`)
}

type shellToken struct {
	value    string
	operator bool
}

const maxNestedShellInspectionDepth = 3

func shellCommandNames(command string) []string {
	names := make([]string, 0)
	seen := make(map[string]struct{})

	appendShellCommandNames(&names, seen, shellTokens(command), 0)
	appendShellSubstitutionCommandNames(&names, seen, command, 0)

	return names
}

//nolint:cyclop,gocognit // Wrapper-aware shell scanning is branchy but intentionally local and auditable.
func appendShellCommandNames(names *[]string, seen map[string]struct{}, tokens []shellToken, depth int) {
	expectCommand := true
	activeWrapper := ""
	skipWrapperValue := false

	for i, token := range tokens {
		if token.operator {
			expectCommand = true
			activeWrapper = ""
			skipWrapperValue = false
			continue
		}

		if expectCommand {
			if skipWrapperValue {
				skipWrapperValue = false
				continue
			}

			if activeWrapper != "" && strings.HasPrefix(token.value, "-") {
				skipWrapperValue = shellWrapperOptionTakesValue(activeWrapper, token.value)
				continue
			}

			if wrapper, ok := shellWrapperCommand(token.value); ok {
				addShellCommandName(names, seen, wrapper)
				activeWrapper = wrapper
				continue
			}
		}

		name, resetCommand, ok := shellCommandCandidate(token.value, expectCommand)
		if resetCommand {
			expectCommand = true
			activeWrapper = ""
			skipWrapperValue = false
			continue
		}
		if !ok {
			continue
		}

		addShellCommandName(names, seen, name)
		if shellEval(name) && depth < maxNestedShellInspectionDepth {
			if script := evalScriptArgumentFromTokens(tokens[i+1:]); script != "" {
				appendShellCommandNames(names, seen, shellTokens(script), depth+1)
				appendShellSubstitutionCommandNames(names, seen, script, depth+1)
			}
		}

		if shellProgram(name) && depth < maxNestedShellInspectionDepth {
			if script, ok := shellScriptArgumentFromTokens(tokens[i+1:]); ok {
				appendShellCommandNames(names, seen, shellTokens(script), depth+1)
			}
		}

		expectCommand = false
		activeWrapper = ""
	}
}

func appendShellSubstitutionCommandNames(names *[]string, seen map[string]struct{}, command string, depth int) {
	if depth >= maxNestedShellInspectionDepth {
		return
	}

	for _, script := range shellCommandSubstitutions(command) {
		appendShellCommandNames(names, seen, shellTokens(script), depth+1)
		appendShellSubstitutionCommandNames(names, seen, script, depth+1)
	}
}

func shellCommandCandidate(value string, expectCommand bool) (name string, resetCommand, ok bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", false, false
	}

	if !expectCommand {
		return "", false, false
	}

	if shellRedirection(value) {
		return "", false, false
	}

	if commandValue, ok := shellWordBeforeRedirection(value); ok {
		value = commandValue
	}

	lower := strings.ToLower(value)
	if shellKeyword(lower) {
		return "", true, false
	}

	if shellAssignment(value) || strings.HasPrefix(value, "-") || shellWrapper(lower) {
		return "", false, false
	}

	return filepath.Base(value), false, true
}

func addShellCommandName(names *[]string, seen map[string]struct{}, name string) {
	if strings.TrimSpace(name) == "" {
		return
	}

	if _, ok := seen[name]; ok {
		return
	}

	seen[name] = struct{}{}
	*names = append(*names, name)
}

func shellScriptArgumentFromTokens(tokens []shellToken) (string, bool) {
	_, script, ok := shellScriptArgumentIndexFromTokens(tokens)

	return script, ok
}

func evalScriptArgumentFromTokens(tokens []shellToken) string {
	_, script := evalScriptArgumentIndexesFromTokens(tokens)

	return script
}

func evalScriptArgumentIndexesFromTokens(tokens []shellToken) (indexes []int, script string) {
	args := make([]string, 0, len(tokens))
	for i, token := range tokens {
		if token.operator {
			break
		}

		args = append(args, token.value)
		indexes = append(indexes, i)
	}

	return indexes, strings.TrimSpace(strings.Join(args, " "))
}

func shellScriptArgumentIndexFromTokens(tokens []shellToken) (index int, script string, ok bool) {
	args := make([]string, 0, len(tokens))
	indexes := make([]int, 0, len(tokens))
	for i, token := range tokens {
		if token.operator {
			break
		}

		args = append(args, token.value)
		indexes = append(indexes, i)
	}

	argIndex, script, ok := shellScriptArgumentIndex(args)
	if !ok || argIndex >= len(indexes) {
		return 0, "", false
	}

	return indexes[argIndex], script, true
}

func shellTokens(command string) []shellToken {
	tokens := make([]shellToken, 0)
	var current strings.Builder
	var quote rune
	escaped := false

	flush := func() {
		if current.Len() == 0 {
			return
		}

		tokens = append(tokens, shellToken{value: current.String()})
		current.Reset()
	}

	for _, r := range command {
		if escaped {
			current.WriteRune(r)
			escaped = false
			continue
		}

		if r == '\\' && quote != '\'' {
			escaped = true
			continue
		}

		if quote != 0 {
			if r == quote {
				quote = 0
				continue
			}

			current.WriteRune(r)
			continue
		}

		switch r {
		case '\'', '"':
			quote = r
		case ' ', '\t', '\r':
			flush()
		case '\n', ';', '|', '&', '(', ')', '`':
			flush()
			tokens = append(tokens, shellToken{value: string(r), operator: true})
		default:
			current.WriteRune(r)
		}
	}

	flush()

	return tokens
}

//nolint:cyclop,gocognit // Quote-aware command-substitution scanning is branchy but intentionally local and auditable.
func shellCommandSubstitutions(command string) []string {
	scripts := make([]string, 0)
	var quote byte
	escaped := false

	for i := 0; i < len(command); i++ {
		c := command[i]
		if escaped {
			escaped = false
			continue
		}

		if c == '\\' && quote != '\'' {
			escaped = true
			continue
		}

		if quote != 0 {
			if c == quote {
				quote = 0
				continue
			}

			if quote == '\'' {
				continue
			}
		} else if c == '\'' || c == '"' {
			quote = c
			continue
		}

		if c == '`' {
			end, ok := closingBacktick(command, i+1)
			if !ok {
				continue
			}

			scripts = append(scripts, command[i+1:end])
			i = end

			continue
		}

		if c == '$' && i+1 < len(command) && command[i+1] == '(' {
			end, ok := closingDollarParen(command, i+2)
			if !ok {
				continue
			}

			scripts = append(scripts, command[i+2:end])
			i = end
		}
	}

	return scripts
}

func closingBacktick(command string, start int) (int, bool) {
	escaped := false
	for i := start; i < len(command); i++ {
		c := command[i]
		if escaped {
			escaped = false
			continue
		}

		if c == '\\' {
			escaped = true
			continue
		}

		if c == '`' {
			return i, true
		}
	}

	return 0, false
}

func closingDollarParen(command string, start int) (int, bool) {
	depth := 1
	var quote byte
	escaped := false

	for i := start; i < len(command); i++ {
		c := command[i]
		if escaped {
			escaped = false
			continue
		}

		if c == '\\' && quote != '\'' {
			escaped = true
			continue
		}

		if quote != 0 {
			if c == quote {
				quote = 0
			}

			continue
		}

		switch c {
		case '\'', '"':
			quote = c
		case '$':
			if i+1 < len(command) && command[i+1] == '(' {
				depth++
				i++
			}
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i, true
			}
		}
	}

	return 0, false
}

func shellKeyword(value string) bool {
	switch value {
	case "if", "then", "else", "elif", "fi", "for", "while", "until", "do", "done", "case", "esac", "{", "}", "!":
		return true
	default:
		return false
	}
}

func shellAssignment(value string) bool {
	name, _, ok := strings.Cut(value, "=")
	if !ok || name == "" {
		return false
	}

	for _, r := range name {
		if r != '_' && (r < '0' || r > '9') && (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') {
			return false
		}
	}

	return true
}

func shellRedirection(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}

	if strings.HasPrefix(value, ">") ||
		strings.HasPrefix(value, "<") ||
		strings.HasPrefix(value, "&>") {
		return true
	}

	index := firstNonDigitIndex(value)

	return index > 0 && index < len(value) && (value[index] == '>' || value[index] == '<')
}

func shellWordBeforeRedirection(value string) (string, bool) {
	value = strings.TrimSpace(value)
	for i := 1; i < len(value); i++ {
		if value[i] == '>' || value[i] == '<' {
			end := i
			if value[i] == '>' && i > 0 && value[i-1] == '&' {
				end = i - 1
			}

			command := strings.TrimSpace(value[:end])

			return command, command != ""
		}
	}

	return "", false
}

func shellWrapper(value string) bool {
	_, ok := shellWrapperCommand(value)

	return ok
}

func shellWrapperCommand(value string) (string, bool) {
	name := strings.TrimSuffix(strings.ToLower(filepath.Base(strings.TrimSpace(value))), ".exe")
	switch name {
	case "builtin", "command", "exec", "env", "nohup", "sudo", "time":
		return name, true
	default:
		return "", false
	}
}

var shellWrapperOptionsWithValues = map[string]map[string]struct{}{
	"env": {
		"-C": {}, "-S": {}, "-u": {}, "--chdir": {}, "--split-string": {}, "--unset": {},
	},
	"sudo": {
		"-C": {}, "-D": {}, "-g": {}, "-h": {}, "-p": {}, "-R": {}, "-r": {}, "-t": {}, "-T": {}, "-u": {},
		"--chdir": {}, "--close-from": {}, "--group": {}, "--host": {}, "--login-class": {}, "--prompt": {},
		"--role": {}, "--type": {}, "--user": {},
	},
	"time": {
		"-f": {}, "-o": {}, "--format": {}, "--output": {},
	},
}

func shellWrapperOptionTakesValue(wrapper, option string) bool {
	option = strings.TrimSpace(option)
	if strings.Contains(option, "=") {
		return false
	}

	options, ok := shellWrapperOptionsWithValues[wrapper]
	if !ok {
		return false
	}

	_, ok = options[option]

	return ok
}

var networkCommandNames = map[string]struct{}{
	"curl": {}, "wget": {}, "nc": {}, "ncat": {}, "netcat": {}, "ssh": {}, "scp": {}, "sftp": {},
	"ftp": {}, "rsync": {}, "telnet": {}, "gh": {},
}

func isNetworkCommand(program string, args []string, command string) bool {
	base := filepath.Base(strings.TrimSpace(program))
	if _, ok := networkCommandNames[base]; ok {
		return true
	}

	if base == "git" {
		return gitNetworkSubcommand(gitSubcommand(args))
	}

	return shellHasNetworkCommand(command)
}

func gitNetworkSubcommand(subcommand string) bool {
	switch strings.TrimSpace(subcommand) {
	case "clone", "fetch", "pull", "push", "ls-remote":
		return true
	default:
		return false
	}
}

func shellHasNetworkCommand(command string) bool {
	return shellTokensHaveNetworkCommand(shellTokens(command), 0) || shellSubstitutionsHaveNetworkCommand(command, 0)
}

//nolint:cyclop,gocognit // Shell command scanning keeps wrapper, git subcommand, and nested shell handling together for auditability.
func shellTokensHaveNetworkCommand(tokens []shellToken, depth int) bool {
	expectCommand := true
	activeWrapper := ""
	skipWrapperValue := false

	for i, token := range tokens {
		if token.operator {
			expectCommand = true
			activeWrapper = ""
			skipWrapperValue = false
			continue
		}

		if expectCommand {
			if skipWrapperValue {
				skipWrapperValue = false
				continue
			}

			if activeWrapper != "" && strings.HasPrefix(token.value, "-") {
				skipWrapperValue = shellWrapperOptionTakesValue(activeWrapper, token.value)
				continue
			}

			if wrapper, ok := shellWrapperCommand(token.value); ok {
				activeWrapper = wrapper
				continue
			}
		}

		name, resetCommand, ok := shellCommandCandidate(token.value, expectCommand)
		if resetCommand {
			expectCommand = true
			activeWrapper = ""
			skipWrapperValue = false
			continue
		}
		if !ok {
			continue
		}

		if _, ok := networkCommandNames[name]; ok {
			return true
		}

		if name == "git" && gitNetworkSubcommand(gitSubcommand(shellArgsFromTokens(tokens[i+1:]))) {
			return true
		}

		if shellEval(name) && depth < maxNestedShellInspectionDepth {
			if script := evalScriptArgumentFromTokens(tokens[i+1:]); script != "" {
				if shellTokensHaveNetworkCommand(shellTokens(script), depth+1) || shellSubstitutionsHaveNetworkCommand(script, depth+1) {
					return true
				}
			}
		}

		if shellProgram(name) && depth < maxNestedShellInspectionDepth {
			if script, ok := shellScriptArgumentFromTokens(tokens[i+1:]); ok {
				if shellTokensHaveNetworkCommand(shellTokens(script), depth+1) || shellSubstitutionsHaveNetworkCommand(script, depth+1) {
					return true
				}
			}
		}

		expectCommand = false
		activeWrapper = ""
	}

	return false
}

func shellSubstitutionsHaveNetworkCommand(command string, depth int) bool {
	if depth >= maxNestedShellInspectionDepth {
		return false
	}

	for _, script := range shellCommandSubstitutions(command) {
		if shellTokensHaveNetworkCommand(shellTokens(script), depth+1) || shellSubstitutionsHaveNetworkCommand(script, depth+1) {
			return true
		}
	}

	return false
}

func shellArgsFromTokens(tokens []shellToken) []string {
	args := make([]string, 0, len(tokens))
	for _, token := range tokens {
		if token.operator {
			break
		}

		args = append(args, token.value)
	}

	return args
}

func gitSubcommand(args []string) string {
	for i := 0; i < len(args); i++ {
		arg := strings.TrimSpace(args[i])
		if arg == "" {
			continue
		}

		if arg == "--" {
			continue
		}

		if gitGlobalFlagWithValue(arg) {
			if !strings.Contains(arg, "=") {
				i++
			}

			continue
		}

		if gitGlobalFlag(arg) || strings.HasPrefix(arg, "-") {
			continue
		}

		if commandValue, ok := shellWordBeforeRedirection(arg); ok {
			return commandValue
		}

		return arg
	}

	return ""
}

func gitGlobalFlagWithValue(arg string) bool {
	switch {
	case arg == "-C", arg == "-c":
		return true
	case arg == "--git-dir", arg == "--work-tree", arg == "--namespace", arg == "--config-env", arg == "--exec-path":
		return true
	case strings.HasPrefix(arg, "--git-dir="),
		strings.HasPrefix(arg, "--work-tree="),
		strings.HasPrefix(arg, "--namespace="),
		strings.HasPrefix(arg, "--config-env="),
		strings.HasPrefix(arg, "--exec-path="):
		return true
	default:
		return false
	}
}

func gitGlobalFlag(arg string) bool {
	switch arg {
	case "--bare", "--no-replace-objects", "--literal-pathspecs", "--glob-pathspecs",
		"--noglob-pathspecs", "--icase-pathspecs", "--no-optional-locks",
		"--paginate", "--no-pager", "--version", "--help":
		return true
	default:
		return false
	}
}

func isDestructiveCommand(program string, args []string, command string) bool {
	base := filepath.Base(strings.TrimSpace(program))
	if base == "rm" && rmArgsDangerous(args) {
		return true
	}

	if base == "mkfs" || strings.HasPrefix(base, "mkfs.") || base == "shutdown" || base == "reboot" {
		return true
	}

	if base == "dd" && argsContainOutputDevice(args) {
		return true
	}

	if shellHasDestructiveCommand(command) {
		return true
	}

	lower := strings.ToLower(command)
	destructivePatterns := []*regexp.Regexp{
		regexp.MustCompile(`\brm\s+-[^\n;&|]*r[^\n;&|]*f[^\n;&|]*(/|\*|~|\$home|\.\.?)(\s|$|/)`),
		regexp.MustCompile(`\bmkfs(\.[a-z0-9]+)?\b`),
		regexp.MustCompile(`\bdd\b[^\n;&|]*\bof=/dev/`),
		regexp.MustCompile(`:\(\)\s*\{\s*:\|:`),
		regexp.MustCompile(`\b(shutdown|reboot)\b`),
	}
	for _, pattern := range destructivePatterns {
		if pattern.MatchString(lower) {
			return true
		}
	}

	return false
}

func shellHasDestructiveCommand(command string) bool {
	return shellTokensHaveDestructiveCommand(shellTokens(command), 0) || shellSubstitutionsHaveDestructiveCommand(command, 0)
}

//nolint:cyclop,gocognit // Mirrors network scanning so destructive checks see wrappers and nested shell scripts.
func shellTokensHaveDestructiveCommand(tokens []shellToken, depth int) bool {
	expectCommand := true
	activeWrapper := ""
	skipWrapperValue := false

	for i, token := range tokens {
		if token.operator {
			expectCommand = true
			activeWrapper = ""
			skipWrapperValue = false
			continue
		}

		if expectCommand {
			if skipWrapperValue {
				skipWrapperValue = false
				continue
			}

			if activeWrapper != "" && strings.HasPrefix(token.value, "-") {
				skipWrapperValue = shellWrapperOptionTakesValue(activeWrapper, token.value)
				continue
			}

			if wrapper, ok := shellWrapperCommand(token.value); ok {
				activeWrapper = wrapper
				continue
			}
		}

		name, resetCommand, ok := shellCommandCandidate(token.value, expectCommand)
		if resetCommand {
			expectCommand = true
			activeWrapper = ""
			skipWrapperValue = false
			continue
		}
		if !ok {
			continue
		}

		args := shellArgsFromTokens(tokens[i+1:])
		if name == "rm" && rmArgsDangerous(args) {
			return true
		}

		if name == "mkfs" || strings.HasPrefix(name, "mkfs.") || name == "shutdown" || name == "reboot" {
			return true
		}

		if name == "dd" && argsContainOutputDevice(args) {
			return true
		}

		if shellEval(name) && depth < maxNestedShellInspectionDepth {
			if script := evalScriptArgumentFromTokens(tokens[i+1:]); script != "" {
				if shellTokensHaveDestructiveCommand(shellTokens(script), depth+1) ||
					shellSubstitutionsHaveDestructiveCommand(script, depth+1) {
					return true
				}
			}
		}

		if shellProgram(name) && depth < maxNestedShellInspectionDepth {
			if script, ok := shellScriptArgumentFromTokens(tokens[i+1:]); ok {
				if shellTokensHaveDestructiveCommand(shellTokens(script), depth+1) ||
					shellSubstitutionsHaveDestructiveCommand(script, depth+1) {
					return true
				}
			}
		}

		expectCommand = false
		activeWrapper = ""
	}

	return false
}

func shellSubstitutionsHaveDestructiveCommand(command string, depth int) bool {
	if depth >= maxNestedShellInspectionDepth {
		return false
	}

	for _, script := range shellCommandSubstitutions(command) {
		if shellTokensHaveDestructiveCommand(shellTokens(script), depth+1) ||
			shellSubstitutionsHaveDestructiveCommand(script, depth+1) {
			return true
		}
	}

	return false
}

func rmArgsDangerous(args []string) bool {
	recursive := false
	force := false
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			recursive = recursive || strings.Contains(arg, "r") || strings.Contains(arg, "R")
			force = force || strings.Contains(arg, "f")
			continue
		}

		clean := filepath.Clean(strings.TrimSpace(arg))
		if recursive && force && dangerousRMTarget(clean) {
			return true
		}
	}

	return false
}

func dangerousRMTarget(target string) bool {
	target = strings.TrimSpace(target)
	if target == "" {
		return false
	}

	normalized := strings.ToUpper(filepath.ToSlash(filepath.Clean(target)))
	if normalized == "/" || normalized == "." || normalized == ".." || normalized == "~" || strings.Contains(normalized, "*") {
		return true
	}

	switch normalized {
	case "$HOME", "${HOME}", "$PWD", "${PWD}", "$OLDPWD", "${OLDPWD}":
		return true
	default:
		return false
	}
}

func argsContainOutputDevice(args []string) bool {
	for _, arg := range args {
		if strings.HasPrefix(strings.ToLower(arg), "of=/dev/") {
			return true
		}
	}

	return false
}

var (
	auditCounter atomic.Uint64
	auditWriteMu sync.Mutex
)

func nextAuditID() string {
	return strconv.FormatInt(time.Now().UTC().UnixNano(), 36) + "-" + strconv.Itoa(os.Getpid()) + "-" + strconv.FormatUint(auditCounter.Add(1), 36)
}

func auditDir(policy Policy) string {
	if dir := strings.TrimSpace(policy.AuditDir); dir != "" {
		return dir
	}

	if dir := strings.TrimSpace(os.Getenv(EnvAuditDir)); dir != "" {
		return dir
	}

	// Use the process temp directory by default so the audit gate remains usable
	// in sandboxed runs where a home cache directory may not be writable. Users
	// that need a long-lived ledger can set ATTELER_COMMAND_AUDIT_DIR.
	return filepath.Join(os.TempDir(), "atteler", "audit")
}

func (i *Invocation) appendRecord(record AuditRecord) error {
	dir := strings.TrimSpace(i.auditDir)
	if dir == "" {
		return errors.New("shell: audit directory is required")
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("shell: create audit directory: %w", err)
	}

	line, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("shell: encode audit record: %w", err)
	}

	auditWriteMu.Lock()
	defer auditWriteMu.Unlock()

	file, err := os.OpenFile(filepath.Join(dir, ledgerFileName), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("shell: open audit ledger: %w", err)
	}
	defer file.Close()

	if _, err := file.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("shell: write audit ledger: %w", err)
	}

	return nil
}

func (i *Invocation) writeOutput(stdout, stderr string) (string, error) {
	dir := filepath.Join(i.auditDir, "outputs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("shell: create audit output directory: %w", err)
	}

	path := filepath.Join(dir, i.record.ID+".json")
	payload := struct {
		Stdout string `json:"stdout"`
		Stderr string `json:"stderr"`
	}{
		Stdout: redactText(stdout, i.secrets),
		Stderr: redactText(stderr, i.secrets),
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(payload); err != nil {
		return "", fmt.Errorf("shell: encode audit output: %w", err)
	}

	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		return "", fmt.Errorf("shell: write audit output: %w", err)
	}

	return path, nil
}

type secretValue struct {
	name  string
	value string
}

func redactText(text string, secrets []secretValue) string {
	redacted := text
	for _, secret := range secrets {
		if secret.value == "" {
			continue
		}

		redacted = strings.ReplaceAll(redacted, secret.value, "<redacted:"+secret.name+">")
	}

	return redactCommonSensitiveText(redacted)
}

var commonSensitiveTextPatterns = []struct {
	pattern     *regexp.Regexp
	name        string
	replacement string
}{
	{
		pattern:     regexp.MustCompile(`(?i)(authorization\s*:\s*bearer\s+)([^\s'"\\]+)`),
		name:        "authorization",
		replacement: "${1}<redacted:authorization>",
	},
	{
		pattern:     regexp.MustCompile(`(?i)(\bbearer\s+)([A-Za-z0-9._~+/=-]{8,})`),
		name:        "bearer",
		replacement: "${1}<redacted:bearer>",
	},
	{
		pattern:     regexp.MustCompile(`(?i)(--?(?:api-key|apikey|token|secret|password|passwd)(?:=|\s+)(["']))([^"'\\]+)(["'])`),
		name:        "command_arg",
		replacement: "${1}<redacted:command_arg>${4}",
	},
	{
		pattern:     regexp.MustCompile(`(?i)\b((?:api[_-]?key|apikey|token|secret|password|passwd)\s*[:=]\s*(["']))([^"'\\]+)(["'])`),
		name:        "command_arg",
		replacement: "${1}<redacted:command_arg>${4}",
	},
	{
		pattern:     regexp.MustCompile(`(?i)(--?(?:api-key|apikey|token|secret|password|passwd)(?:=|\s+))([^\s'"\\]+)`),
		name:        "command_arg",
		replacement: "${1}<redacted:command_arg>",
	},
	{
		pattern:     regexp.MustCompile(`(?i)\b((?:api[_-]?key|apikey|token|secret|password|passwd)\s*[:=]\s*)([^\s'"\\]+)`),
		name:        "command_arg",
		replacement: "${1}<redacted:command_arg>",
	},
}

func redactCommonSensitiveText(text string) string {
	redacted := text
	for _, secretPattern := range commonSensitiveTextPatterns {
		replacement := secretPattern.replacement
		if replacement == "" {
			replacement = "${1}<redacted:" + secretPattern.name + ">"
		}

		redacted = secretPattern.pattern.ReplaceAllString(redacted, replacement)
	}

	return redacted
}

func exitStatus(err error) *int {
	if err == nil {
		code := 0
		return &code
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code := exitErr.ExitCode()
		return &code
	}

	return nil
}

func errorString(err error) string {
	if err == nil {
		return ""
	}

	return err.Error()
}

func displayCommand(program string, args []string, command string) string {
	command = strings.TrimSpace(command)
	if command != "" {
		return command
	}

	parts := []string{program}
	for _, arg := range args {
		parts = append(parts, shellQuote(arg))
	}

	return strings.Join(parts, " ")
}

func policyCommand(program string, args []string, command string) string {
	command = strings.TrimSpace(command)
	if command != "" {
		return command
	}

	if shellProgram(program) {
		if script, ok := shellScriptArgument(args); ok {
			return script
		}
	}

	return displayCommand(program, args, "")
}

func shellScriptArgument(args []string) (string, bool) {
	_, script, ok := shellScriptArgumentIndex(args)

	return script, ok
}

func shellScriptArgumentIndex(args []string) (index int, script string, ok bool) {
	for i, arg := range args {
		arg = strings.TrimSpace(arg)
		if arg == "--" {
			return 0, "", false
		}

		if arg == "-c" {
			return nextShellArgument(args, i)
		}

		if strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--") && strings.Contains(strings.TrimLeft(arg, "-"), "c") {
			return nextShellArgument(args, i)
		}
	}

	return 0, "", false
}

func nextShellArgument(args []string, index int) (scriptIndex int, script string, ok bool) {
	if index+1 >= len(args) {
		return 0, "", false
	}

	script = strings.TrimSpace(args[index+1])

	return index + 1, script, script != ""
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}

	if strings.IndexFunc(value, func(r rune) bool { return !shellQuoteSafeRune(r) }) == -1 {
		return value
	}

	return strconv.Quote(value)
}

func shellQuoteSafeRune(r rune) bool {
	return r == '_' || r == '-' || r == '.' || r == '/' || r == ':' || r == '=' ||
		(r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z')
}
