// Package permission provides Atteler's central side-effect policy gate.
package permission

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// OperationKind is the coarse side-effect class used by Atteler policy.
type OperationKind string

const (
	// OperationRead covers read-only local inspection.
	OperationRead OperationKind = "read"
	// OperationWrite covers filesystem or state writes.
	OperationWrite OperationKind = "write"
	// OperationExecute covers starting a local process or plugin entrypoint.
	OperationExecute OperationKind = "execute"
	// OperationNetwork covers network-capable commands or provider calls.
	OperationNetwork OperationKind = "network"
	// OperationCredentialAccess covers reading or propagating credentials.
	OperationCredentialAccess OperationKind = "credential_access" // #nosec G101 -- permission operation label, not a credential.
	// OperationGitMutation covers mutating git commands such as branch, add, or commit.
	OperationGitMutation OperationKind = "git_mutation"
	// OperationMergeDelete covers merge, delete, cleanup, and similarly destructive operations.
	OperationMergeDelete OperationKind = "merge_delete"
)

var operationKindOrder = []OperationKind{
	OperationRead,
	OperationWrite,
	OperationExecute,
	OperationNetwork,
	OperationCredentialAccess,
	OperationGitMutation,
	OperationMergeDelete,
}

// DecisionMode is the configured action for one operation class.
type DecisionMode string

const (
	// ModeAllow permits the operation.
	ModeAllow DecisionMode = "allow"
	// ModeAsk requires an interactive confirmation before the operation runs.
	ModeAsk DecisionMode = "ask"
	// ModeDeny blocks the operation.
	ModeDeny DecisionMode = "deny"
)

const (
	gitSubcommandBranch   = "branch"
	gitSubcommandWorktree = "worktree"
	readOnlyInspectionKey = "read_only_inspection"
)

// Policy maps operation classes to allow/ask/deny decisions.
//
// The zero value preserves legacy behavior by allowing operations unless a
// caller configures a stricter mode.
//
//nolint:recvcheck // SetMode mutates policy; read helpers intentionally also work on values.
type Policy struct {
	Modes   map[OperationKind]DecisionMode `json:"modes,omitempty" yaml:"modes,omitempty"`
	Name    string                         `json:"name,omitempty" yaml:"name,omitempty"`
	Default DecisionMode                   `json:"default,omitempty" yaml:"default,omitempty"`
	// AllowReadExecution permits execute operations only when the request was
	// classified as a known read-only inspection command. This lets read-only
	// mode run commands such as `git status` without allowing arbitrary plugin
	// entrypoints or binaries.
	AllowReadExecution bool `json:"allow_read_execution,omitempty" yaml:"allow_read_execution,omitempty"`
}

// Operation describes one pending side effect.
type Operation struct {
	Metadata map[string]string `json:"metadata,omitempty" yaml:"metadata,omitempty"`
	Kind     OperationKind     `json:"kind" yaml:"kind"`
	Action   string            `json:"action,omitempty" yaml:"action,omitempty"`
	Target   string            `json:"target,omitempty" yaml:"target,omitempty"`
	Source   string            `json:"source,omitempty" yaml:"source,omitempty"`
}

// Request groups operations that will happen as one user-visible action.
//
//nolint:govet // Field order follows action metadata readability.
type Request struct {
	Operations []Operation `json:"operations" yaml:"operations"`
	Action     string      `json:"action,omitempty" yaml:"action,omitempty"`
	Source     string      `json:"source,omitempty" yaml:"source,omitempty"`
	Target     string      `json:"target,omitempty" yaml:"target,omitempty"`
}

// Decision is the policy result for a request.
//
//nolint:govet // Field order follows user-facing decision metadata.
type Decision struct {
	Operations    []Operation   `json:"operations,omitempty" yaml:"operations,omitempty"`
	Mode          DecisionMode  `json:"mode" yaml:"mode"`
	Kind          OperationKind `json:"kind,omitempty" yaml:"kind,omitempty"`
	Rule          string        `json:"rule" yaml:"rule"`
	Reason        string        `json:"reason" yaml:"reason"`
	Allowed       bool          `json:"allowed" yaml:"allowed"`
	NeedsApproval bool          `json:"needs_approval,omitempty" yaml:"needs_approval,omitempty"`
	Confirmed     bool          `json:"confirmed,omitempty" yaml:"confirmed,omitempty"`
}

// Confirmer is called when a policy mode is ask. It should return true only
// after a human or trusted caller explicitly approves the request.
type Confirmer func(context.Context, Request, Decision) bool

type (
	policyContextKey    struct{}
	confirmerContextKey struct{}
)

// ContextWithPolicy stores policy in ctx for lower-level execution gates.
func ContextWithPolicy(ctx context.Context, policy *Policy) context.Context {
	if ctx == nil || policy == nil {
		return ctx
	}

	return context.WithValue(ctx, policyContextKey{}, policy)
}

// PolicyFromContext returns the policy stored in ctx, if any.
func PolicyFromContext(ctx context.Context) *Policy {
	if ctx == nil {
		return nil
	}

	policy, ok := ctx.Value(policyContextKey{}).(*Policy)
	if !ok {
		return nil
	}

	return policy
}

// ContextWithConfirmer stores an interactive confirmation callback in ctx.
func ContextWithConfirmer(ctx context.Context, confirmer Confirmer) context.Context {
	if ctx == nil || confirmer == nil {
		return ctx
	}

	return context.WithValue(ctx, confirmerContextKey{}, confirmer)
}

// ConfirmerFromContext returns the confirmation callback stored in ctx, if any.
func ConfirmerFromContext(ctx context.Context) Confirmer {
	if ctx == nil {
		return nil
	}

	confirmer, ok := ctx.Value(confirmerContextKey{}).(Confirmer)
	if !ok {
		return nil
	}

	return confirmer
}

// DefaultPolicy allows all operations unless callers override individual modes.
func DefaultPolicy() Policy {
	return Policy{Name: "default", Default: ModeAllow}
}

// ReadOnlyPolicy allows local read/inspection commands while blocking writes,
// network, credential access, git mutations, and merge/delete operations.
func ReadOnlyPolicy() Policy {
	policy := DefaultPolicy()
	policy.Name = "read-only"
	policy.SetMode(OperationRead, ModeAllow)
	policy.SetMode(OperationExecute, ModeDeny)
	policy.SetMode(OperationWrite, ModeDeny)
	policy.SetMode(OperationNetwork, ModeDeny)
	policy.SetMode(OperationCredentialAccess, ModeDeny)
	policy.SetMode(OperationGitMutation, ModeDeny)
	policy.SetMode(OperationMergeDelete, ModeDeny)
	policy.AllowReadExecution = true

	return policy
}

// SetMode configures one operation kind.
func (p *Policy) SetMode(kind OperationKind, mode DecisionMode) {
	if p == nil || kind == "" || mode == "" {
		return
	}

	if p.Modes == nil {
		p.Modes = make(map[OperationKind]DecisionMode)
	}

	p.Modes[kind] = mode
}

// ModeFor returns the configured decision mode for kind.
func (p Policy) ModeFor(kind OperationKind) DecisionMode {
	if mode := p.Modes[kind]; mode != "" {
		return mode
	}

	if p.Default != "" {
		return p.Default
	}

	return ModeAllow
}

// Summary returns a compact user-facing policy label.
func (p Policy) Summary() string {
	name := strings.TrimSpace(p.Name)
	if name != "" && name != "default" {
		return name
	}

	defaultMode := p.Default
	if defaultMode == "" {
		defaultMode = ModeAllow
	}

	if len(p.Modes) == 0 {
		return string(defaultMode)
	}

	parts := []string{string(defaultMode)}
	for _, kind := range operationKindOrder {
		if mode := p.Modes[kind]; mode != "" && mode != defaultMode {
			parts = append(parts, string(kind)+":"+string(mode))
		}
	}

	return strings.Join(parts, ",")
}

// KnownOperationKinds returns policy operation classes in stable display order.
func KnownOperationKinds() []OperationKind {
	return append([]OperationKind(nil), operationKindOrder...)
}

// ParseOperationKind parses one operation kind.
func ParseOperationKind(raw string) (OperationKind, error) {
	value := OperationKind(strings.ToLower(strings.TrimSpace(strings.ReplaceAll(raw, "-", "_"))))
	for _, kind := range operationKindOrder {
		if value == kind {
			return kind, nil
		}
	}

	return "", fmt.Errorf("unknown permission operation %q", raw)
}

// ParseOperationKinds parses comma-separated and repeated operation names.
func ParseOperationKinds(values []string) ([]OperationKind, error) {
	var out []OperationKind

	for _, value := range values {
		for part := range strings.SplitSeq(value, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}

			kind, err := ParseOperationKind(part)
			if err != nil {
				return nil, err
			}

			out = append(out, kind)
		}
	}

	return out, nil
}

// ParseDecisionMode parses a policy mode.
func ParseDecisionMode(raw string) (DecisionMode, error) {
	switch DecisionMode(strings.ToLower(strings.TrimSpace(raw))) {
	case "", ModeAllow:
		return ModeAllow, nil
	case ModeAsk:
		return ModeAsk, nil
	case ModeDeny:
		return ModeDeny, nil
	default:
		return "", fmt.Errorf("unknown permission mode %q", raw)
	}
}

// Evaluate applies policy to request. Deny wins over ask, and ask without a
// confirmer fails closed.
func Evaluate(ctx context.Context, policy *Policy, request Request) Decision {
	if policy == nil {
		policy = PolicyFromContext(ctx)
	}

	effective := DefaultPolicy()
	if policy != nil {
		effective = *policy
	}

	request.Operations = normalizeOperations(request)
	if len(request.Operations) == 0 {
		return Decision{Allowed: true, Mode: ModeAllow, Rule: "permission.allow.empty", Reason: "no side-effecting operation classified"}
	}

	if decision, ok := firstDecisionForMode(effective, request, ModeDeny); ok {
		if readOnlyExecutionAllowed(effective, request, decision) {
			return Decision{
				Allowed:    true,
				Mode:       ModeAllow,
				Kind:       OperationRead,
				Operations: append([]Operation(nil), request.Operations...),
				Rule:       ruleFor(OperationRead, ModeAllow),
				Reason:     "allowed read-only inspection by permission policy",
			}
		}

		return decision
	}

	if decision, ok := firstDecisionForMode(effective, request, ModeAsk); ok {
		decision.NeedsApproval = true

		confirmer := ConfirmerFromContext(ctx)
		if confirmer == nil {
			decision.Reason += "; no interactive confirmer is available"

			return decision
		}

		if confirmer(ctx, request, decision) {
			decision.Allowed = true
			decision.Confirmed = true
			decision.NeedsApproval = false
			decision.Reason = "confirmed by policy prompt"
			decision.Rule = ruleFor(decision.Kind, ModeAllow)

			return decision
		}

		decision.Reason += "; confirmation declined"

		return decision
	}

	return Decision{
		Allowed:    true,
		Mode:       ModeAllow,
		Operations: append([]Operation(nil), request.Operations...),
		Rule:       "permission.allow",
		Reason:     "allowed by permission policy",
	}
}

func firstDecisionForMode(policy Policy, request Request, mode DecisionMode) (Decision, bool) {
	for _, op := range request.Operations {
		if policy.ModeFor(op.Kind) == mode {
			return Decision{
				Mode:       mode,
				Kind:       op.Kind,
				Operations: append([]Operation(nil), request.Operations...),
				Rule:       ruleFor(op.Kind, mode),
				Reason:     reasonFor(op, mode, request),
			}, true
		}
	}

	return Decision{}, false
}

func readOnlyExecutionAllowed(policy Policy, request Request, decision Decision) bool {
	if !policy.AllowReadExecution || decision.Kind != OperationExecute || policy.ModeFor(OperationRead) != ModeAllow {
		return false
	}

	hasRead := false
	hasReadOnlyExecute := false

	for _, op := range request.Operations {
		switch op.Kind {
		case OperationRead:
			hasRead = true
		case OperationExecute:
			hasReadOnlyExecute = hasReadOnlyExecute || op.Metadata[readOnlyInspectionKey] == "true"
		default:
			return false
		}
	}

	return hasRead && hasReadOnlyExecute
}

func ruleFor(kind OperationKind, mode DecisionMode) string {
	return "permission." + string(kind) + "." + string(mode)
}

func reasonFor(op Operation, mode DecisionMode, request Request) string {
	detail := strings.TrimSpace(op.Action)
	if detail == "" {
		detail = strings.TrimSpace(request.Action)
	}

	if detail == "" {
		detail = strings.TrimSpace(request.Source)
	}

	if detail == "" {
		detail = strings.TrimSpace(op.Source)
	}

	if detail == "" {
		detail = string(op.Kind)
	}

	switch mode {
	case ModeDeny:
		return fmt.Sprintf("%s operation %q is denied by permission policy", op.Kind, detail)
	case ModeAsk:
		return fmt.Sprintf("%s operation %q requires permission confirmation", op.Kind, detail)
	default:
		return "allowed by permission policy"
	}
}

func normalizeOperations(request Request) []Operation {
	seen := make(map[OperationKind]bool)

	out := make([]Operation, 0, len(request.Operations))
	for _, op := range request.Operations {
		if op.Kind == "" || seen[op.Kind] {
			continue
		}

		if op.Action == "" {
			op.Action = request.Action
		}

		if op.Target == "" {
			op.Target = request.Target
		}

		if op.Source == "" {
			op.Source = request.Source
		}

		seen[op.Kind] = true
		out = append(out, op)
	}

	sort.SliceStable(out, func(i, j int) bool {
		return operationKindIndex(out[i].Kind) < operationKindIndex(out[j].Kind)
	})

	return out
}

func operationKindIndex(kind OperationKind) int {
	for i, candidate := range operationKindOrder {
		if kind == candidate {
			return i
		}
	}

	return len(operationKindOrder)
}

// Error reports a denied permission decision.
type Error struct {
	Decision Decision
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}

	return "permission denied: " + e.Decision.Reason
}

// ErrDenied reports whether err wraps a permission denial.
func ErrDenied(err error) bool {
	var permissionErr *Error
	return errors.As(err, &permissionErr)
}

// CommandOperations classifies a local command into central permission classes.
func CommandOperations(program string, args []string, command, cwd, source string) []Operation {
	kinds := ClassifyCommand(program, args, command)
	ops := make([]Operation, 0, len(kinds))
	action := displayCommand(program, args, command)
	readOnlyInspection := readOnlyInspectionKinds(kinds)

	for _, kind := range kinds {
		op := Operation{
			Kind:   kind,
			Action: action,
			Target: cwd,
			Source: source,
		}
		if kind == OperationExecute && readOnlyInspection {
			op.Metadata = map[string]string{readOnlyInspectionKey: "true"}
		}

		ops = append(ops, op)
	}

	return ops
}

func readOnlyInspectionKinds(kinds []OperationKind) bool {
	hasRead := false
	hasExecute := false

	for _, kind := range kinds {
		switch kind {
		case OperationRead:
			hasRead = true
		case OperationExecute:
			hasExecute = true
		default:
			return false
		}
	}

	return hasRead && hasExecute
}

// ClassifyCommand maps a command invocation to coarse permission classes. It is
// intentionally conservative for common shell/git/network/write patterns and
// leaves fine-grained command allow/deny lists to pkg/shell.
func ClassifyCommand(program string, args []string, command string) []OperationKind {
	classes := map[OperationKind]bool{OperationExecute: true}

	text := strings.TrimSpace(command)
	if text == "" {
		text = strings.TrimSpace(strings.Join(append([]string{program}, args...), " "))
	}

	if text == "" {
		return sortedKinds(classes)
	}

	if containsShellWriteRedirection(text) {
		classes[OperationWrite] = true
	}

	for _, fields := range commandSegments(text) {
		classifyFields(classes, fields)
	}

	return sortedKinds(classes)
}

func displayCommand(program string, args []string, command string) string {
	if strings.TrimSpace(command) != "" {
		return strings.TrimSpace(command)
	}

	parts := append([]string{strings.TrimSpace(program)}, args...)

	return strings.TrimSpace(strings.Join(parts, " "))
}

func commandSegments(command string) [][]string {
	parts := strings.FieldsFunc(command, func(r rune) bool {
		switch r {
		case ';', '&', '|', '\n', '\r', '(', ')':
			return true
		default:
			return false
		}
	})

	segments := make([][]string, 0, len(parts))
	for _, part := range parts {
		fields := strings.Fields(strings.TrimSpace(part))
		if len(fields) > 0 {
			segments = append(segments, fields)
		}
	}

	return segments
}

func classifyFields(classes map[OperationKind]bool, fields []string) {
	if len(fields) == 0 {
		return
	}

	for _, field := range fields {
		if credentialAssignment(field) || credentialFlag(field) {
			classes[OperationCredentialAccess] = true
		}
	}

	fields = unwrapShell(fields)
	if len(fields) == 0 {
		return
	}

	name := strings.ToLower(filepath.Base(strings.Trim(fields[0], `"'`)))
	switch name {
	case "cat", "grep", "rg", "sed", "awk", "ls", "find", "pwd", "printf", "echo", "head", "tail", "wc", "stat", "test", "true", "false":
		classes[OperationRead] = true
	case "touch", "mkdir", "tee", "cp", "mv", "chmod", "chown", "install", "truncate", "patch":
		classes[OperationWrite] = true
	case "rm", "rmdir", "unlink", "shred":
		classes[OperationWrite] = true
		classes[OperationMergeDelete] = true
	case "curl", "wget", "ssh", "scp", "sftp", "rsync", "nc", "netcat", "telnet", "ftp", "dig", "nslookup", "ping":
		classes[OperationNetwork] = true
	case "git":
		classifyGit(classes, fields[1:])
	}
}

func unwrapShell(fields []string) []string {
	if len(fields) == 0 {
		return fields
	}

	name := strings.ToLower(filepath.Base(strings.Trim(fields[0], `"'`)))
	if (name == "bash" || name == "sh" || name == "zsh") && len(fields) >= 3 {
		for i := 1; i < len(fields)-1; i++ {
			if strings.Contains(fields[i], "c") && strings.HasPrefix(fields[i], "-") {
				return strings.Fields(strings.Trim(fields[i+1], `"'`))
			}
		}
	}

	if name == "env" && len(fields) > 1 {
		for i := 1; i < len(fields); i++ {
			if strings.Contains(fields[i], "=") {
				continue
			}

			return fields[i:]
		}
	}

	return fields
}

func classifyGit(classes map[OperationKind]bool, args []string) {
	if len(args) == 0 {
		classes[OperationRead] = true
		return
	}

	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		args = args[1:]
	}

	if len(args) == 0 {
		classes[OperationRead] = true
		return
	}

	sub := strings.ToLower(strings.Trim(args[0], `"'`))
	mutates := false

	switch sub {
	case "add", "commit", "checkout", "switch", "restore", "reset", "stash", "tag", gitSubcommandBranch, gitSubcommandWorktree, "rebase", "merge", "cherry-pick", "revert", "clean", "push", "pull", "fetch":
		if gitSubcommandMutates(sub, args[1:]) {
			classes[OperationGitMutation] = true
			mutates = true
		}
	}

	mergesOrDeletes := gitSubcommandMergesOrDeletes(sub, args[1:])
	if mergesOrDeletes {
		classes[OperationMergeDelete] = true
	}

	if classes[OperationGitMutation] || classes[OperationMergeDelete] {
		classes[OperationWrite] = true
	}

	if !mutates && !mergesOrDeletes {
		switch sub {
		case "status", "diff", "log", "show", "rev-parse", "merge-base", gitSubcommandBranch, gitSubcommandWorktree, "ls-files", "remote", "config":
			classes[OperationRead] = true
		}
	}
}

func gitSubcommandMutates(sub string, args []string) bool {
	switch sub {
	case "status", "diff", "log", "show", "rev-parse", "merge-base", "ls-files", "remote":
		return false
	case gitSubcommandBranch:
		return len(args) > 0 && strings.HasPrefix(args[0], "-")
	case gitSubcommandWorktree:
		return len(args) > 0 && !readOnlyGitWorktreeSubcommand(args[0])
	case "config":
		return len(args) > 0 && !strings.HasPrefix(args[0], "--get") && args[0] != "get" && args[0] != "list" && args[0] != "--list"
	default:
		return true
	}
}

func readOnlyGitWorktreeSubcommand(sub string) bool {
	switch strings.ToLower(strings.Trim(sub, `"'`)) {
	case "list":
		return true
	default:
		return false
	}
}

func gitSubcommandMergesOrDeletes(sub string, args []string) bool {
	switch sub {
	case "merge", "rebase", "clean":
		return true
	case gitSubcommandBranch:
		return slices.Contains(args, "-d") || slices.Contains(args, "-D") || slices.Contains(args, "--delete")
	case gitSubcommandWorktree:
		return len(args) > 0 && (args[0] == "remove" || args[0] == "prune")
	case "reset":
		return slices.Contains(args, "--hard")
	}

	return false
}

func containsShellWriteRedirection(command string) bool {
	for _, marker := range []string{">", ">>", "2>", "&>"} {
		if strings.Contains(command, marker) {
			return true
		}
	}

	return false
}

func credentialAssignment(field string) bool {
	name, value, ok := strings.Cut(strings.TrimSpace(field), "=")
	return ok && value != "" && credentialName(name)
}

func credentialFlag(field string) bool {
	field = strings.ToLower(strings.TrimSpace(field))
	return strings.Contains(field, "token") || strings.Contains(field, "secret") || strings.Contains(field, "password") || strings.Contains(field, "api-key") || strings.Contains(field, "apikey") || strings.Contains(field, "credential")
}

func credentialName(name string) bool {
	upper := strings.ToUpper(strings.TrimSpace(name))
	for _, marker := range []string{"TOKEN", "SECRET", "PASSWORD", "PASSWD", "CREDENTIAL", "COOKIE", "PRIVATE_KEY", "ACCESS_KEY", "API_KEY", "APIKEY"} {
		if strings.Contains(upper, marker) {
			return true
		}
	}

	return false
}

func sortedKinds(classes map[OperationKind]bool) []OperationKind {
	out := make([]OperationKind, 0, len(classes))
	for _, kind := range operationKindOrder {
		if classes[kind] {
			out = append(out, kind)
		}
	}

	return out
}
