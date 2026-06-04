// Package permission provides Atteler's central side-effect policy gate.
package permission

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
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
	// EnvAuditDir overrides the directory used for side-effect audit ledgers.
	// It intentionally matches pkg/shell's audit directory env var so command
	// and non-command side-effect decisions land in the same review location.
	EnvAuditDir = "ATTELER_COMMAND_AUDIT_DIR"

	sideEffectLedgerFileName = "side_effects.jsonl"

	gitSubcommandBranch    = "branch"
	gitSubcommandApply     = "apply"
	gitSubcommandWorktree  = "worktree"
	gitSubcommandConfig    = "config"
	gitSubcommandRemote    = "remote"
	gitSubcommandShow      = "show"
	gitSubcommandPush      = "push"
	gitSubcommandStatus    = "status"
	gitSubcommandSubmodule = "submodule"
	shellNameBash          = "bash"
	shellNameSh            = "sh"
	shellNameZsh           = "zsh"
	commandNameChmod       = "chmod"
	commandNameChown       = "chown"
	commandNameInstall     = "install"
	commandNamePatch       = "patch"
	commandNameSed         = "sed"
	readOnlyInspectionKey  = "read_only_inspection"

	metadataSessionID       = "session_id"
	metadataSessionPath     = "session_path"
	metadataIssueID         = "issue_id"
	metadataIssueIdentifier = "issue_identifier"
	metadataAgent           = "agent"
	metadataModel           = "model"
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
	// AllowReadExecution permits execute operations only when they are part of
	// a request classified as read-only inspection. This lets read-only mode
	// run known inspection commands without allowing arbitrary executables.
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
	Policy        string        `json:"policy,omitempty" yaml:"policy,omitempty"`
	Mode          DecisionMode  `json:"mode" yaml:"mode"`
	Kind          OperationKind `json:"kind,omitempty" yaml:"kind,omitempty"`
	Rule          string        `json:"rule" yaml:"rule"`
	Reason        string        `json:"reason" yaml:"reason"`
	Allowed       bool          `json:"allowed" yaml:"allowed"`
	NeedsApproval bool          `json:"needs_approval,omitempty" yaml:"needs_approval,omitempty"`
	Confirmed     bool          `json:"confirmed,omitempty" yaml:"confirmed,omitempty"`
}

// AuditRecord is one append-only side-effect permission decision.
//
//nolint:govet // JSON field order follows audit-reading order.
type AuditRecord struct {
	Timestamp       time.Time     `json:"timestamp,omitzero"`
	Operations      []Operation   `json:"operations,omitempty"`
	OperationKinds  []string      `json:"operation_kinds,omitempty"`
	Action          string        `json:"action,omitempty"`
	Source          string        `json:"source,omitempty"`
	Target          string        `json:"target,omitempty"`
	SessionID       string        `json:"session_id,omitempty"`
	SessionPath     string        `json:"session_path,omitempty"`
	IssueID         string        `json:"issue_id,omitempty"`
	IssueIdentifier string        `json:"issue_identifier,omitempty"`
	Agent           string        `json:"agent,omitempty"`
	Model           string        `json:"model,omitempty"`
	Decision        string        `json:"decision"`
	Policy          string        `json:"policy,omitempty"`
	Mode            DecisionMode  `json:"mode"`
	Kind            OperationKind `json:"kind,omitempty"`
	Rule            string        `json:"rule"`
	Reason          string        `json:"reason"`
	NeedsApproval   bool          `json:"needs_approval,omitempty"`
	Confirmed       bool          `json:"confirmed,omitempty"`
}

// Confirmer is called when a policy mode is ask. It should return true only
// after a human or trusted caller explicitly approves the request.
type Confirmer func(context.Context, Request, Decision) bool

type (
	policyContextKey        struct{}
	confirmerContextKey     struct{}
	auditMetadataContextKey struct{}
	auditDirContextKey      struct{}
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

// ContextWithAuditMetadata stores stable session/audit fields in ctx so every
// permission decision can be tied back to the session log that triggered it.
func ContextWithAuditMetadata(ctx context.Context, metadata map[string]string) context.Context {
	if ctx == nil || len(metadata) == 0 {
		return ctx
	}

	merged := auditMetadataFromContext(ctx)

	for key, value := range metadata {
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if key == "" || value == "" {
			continue
		}

		merged[key] = value
	}

	if len(merged) == 0 {
		return ctx
	}

	return context.WithValue(ctx, auditMetadataContextKey{}, merged)
}

// ContextWithAuditDir stores the directory for side-effect audit records.
func ContextWithAuditDir(ctx context.Context, dir string) context.Context {
	dir = strings.TrimSpace(dir)
	if ctx == nil || dir == "" {
		return ctx
	}

	return context.WithValue(ctx, auditDirContextKey{}, dir)
}

// AuditDirFromContext returns the explicit audit directory stored in ctx.
func AuditDirFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}

	dir, ok := ctx.Value(auditDirContextKey{}).(string)
	if !ok {
		return ""
	}

	return strings.TrimSpace(dir)
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
		if overrides := namedPolicySummaryOverrides(name, p); len(overrides) > 0 {
			return name + "," + strings.Join(overrides, ",")
		}

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

func namedPolicySummaryOverrides(name string, policy Policy) []string {
	baseline, ok := namedPolicyBaseline(name)
	if !ok {
		return nil
	}

	overrides := make([]string, 0)

	for _, kind := range operationKindOrder {
		if mode := policy.ModeFor(kind); mode != baseline.ModeFor(kind) {
			overrides = append(overrides, string(kind)+":"+string(mode))
		}
	}

	if policy.AllowReadExecution != baseline.AllowReadExecution {
		mode := ModeDeny
		if policy.AllowReadExecution {
			mode = ModeAllow
		}

		overrides = append(overrides, "read_execution:"+string(mode))
	}

	return overrides
}

func namedPolicyBaseline(name string) (Policy, bool) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "allow":
		policy := DefaultPolicy()
		policy.Name = "allow"

		return policy, true
	case "ask":
		policy := DefaultPolicy()
		policy.Name = "ask"
		policy.Default = ModeAsk
		policy.SetMode(OperationRead, ModeAllow)

		return policy, true
	case "deny":
		policy := DefaultPolicy()
		policy.Name = "deny"
		policy.Default = ModeDeny

		return policy, true
	case "read-only", "readonly":
		return ReadOnlyPolicy(), true
	default:
		return Policy{}, false
	}
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

	policySummary := effective.Summary()

	request.Operations = operationsWithAuditMetadata(request.Operations, auditMetadataFromContext(ctx))

	request.Operations = normalizeOperations(request)
	if len(request.Operations) == 0 {
		return Decision{Allowed: true, Policy: policySummary, Mode: ModeAllow, Rule: "permission.allow.empty", Reason: "no side-effecting operation classified"}
	}

	if decision, ok := firstDecisionForMode(effective, request, ModeDeny); ok {
		decision.Policy = policySummary
		if readOnlyExecutionAllowed(effective, request, decision) {
			return auditDecision(ctx, Decision{
				Allowed:    true,
				Policy:     policySummary,
				Mode:       ModeAllow,
				Kind:       OperationRead,
				Operations: append([]Operation(nil), request.Operations...),
				Rule:       ruleFor(OperationRead, ModeAllow),
				Reason:     "allowed read-only inspection by permission policy",
			})
		}

		return auditDecision(ctx, decision)
	}

	if decision, ok := firstDecisionForMode(effective, request, ModeAsk); ok {
		decision.Policy = policySummary
		decision.NeedsApproval = true

		confirmer := ConfirmerFromContext(ctx)
		if confirmer == nil {
			decision.Reason += "; no interactive confirmer is available"

			return auditDecision(ctx, decision)
		}

		if confirmer(ctx, request, decision) {
			decision.Allowed = true
			decision.Confirmed = true
			decision.NeedsApproval = false
			decision.Reason = "confirmed by policy prompt"
			decision.Rule = ruleFor(decision.Kind, ModeAllow)

			return auditDecision(ctx, decision)
		}

		decision.Reason += "; confirmation declined"

		return auditDecision(ctx, decision)
	}

	return auditDecision(ctx, Decision{
		Allowed:    true,
		Policy:     policySummary,
		Mode:       ModeAllow,
		Operations: append([]Operation(nil), request.Operations...),
		Rule:       "permission.allow",
		Reason:     "allowed by permission policy",
	})
}

func firstDecisionForMode(policy Policy, request Request, mode DecisionMode) (Decision, bool) {
	for _, op := range operationsByDecisionPriority(request.Operations) {
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

func operationsByDecisionPriority(ops []Operation) []Operation {
	if len(ops) <= 1 {
		return ops
	}

	out := append([]Operation(nil), ops...)
	sort.SliceStable(out, func(i, j int) bool {
		return operationDecisionPriority(out[i].Kind) < operationDecisionPriority(out[j].Kind)
	})

	return out
}

func operationDecisionPriority(kind OperationKind) int {
	switch kind {
	case OperationCredentialAccess:
		return 0
	case OperationMergeDelete:
		return 1
	case OperationNetwork:
		return 2
	case OperationGitMutation:
		return 3
	case OperationWrite:
		return 4
	case OperationExecute:
		return 5
	case OperationRead:
		return 6
	default:
		return 7
	}
}

func auditDecision(ctx context.Context, decision Decision) Decision {
	appendAuditRecord(ctx, decision)

	return decision
}

func readOnlyExecutionAllowed(policy Policy, request Request, decision Decision) bool {
	return decision.Kind == OperationExecute && AllowsReadOnlyExecution(policy, request.Operations)
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
	out := make([]Operation, 0, len(request.Operations))
	for _, op := range request.Operations {
		if op.Kind == "" {
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

		out = append(out, op)
	}

	sort.SliceStable(out, func(i, j int) bool {
		return operationKindIndex(out[i].Kind) < operationKindIndex(out[j].Kind)
	})

	return out
}

func auditMetadataFromContext(ctx context.Context) map[string]string {
	if ctx == nil {
		return map[string]string{}
	}

	metadata, ok := ctx.Value(auditMetadataContextKey{}).(map[string]string)
	if !ok || len(metadata) == 0 {
		return map[string]string{}
	}

	out := make(map[string]string, len(metadata))
	for key, value := range metadata {
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if key != "" && value != "" {
			out[key] = value
		}
	}

	return out
}

func operationsWithAuditMetadata(ops []Operation, metadata map[string]string) []Operation {
	if len(ops) == 0 || len(metadata) == 0 {
		return ops
	}

	out := make([]Operation, 0, len(ops))
	for _, op := range ops {
		if len(metadata) > 0 {
			op.Metadata = mergeOperationMetadata(op.Metadata, metadata)
		}

		out = append(out, op)
	}

	return out
}

func mergeOperationMetadata(existing, defaults map[string]string) map[string]string {
	if len(defaults) == 0 {
		return existing
	}

	merged := make(map[string]string, len(existing)+len(defaults))
	for key, value := range defaults {
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if key != "" && value != "" {
			merged[key] = value
		}
	}

	for key, value := range existing {
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if key != "" && value != "" {
			merged[key] = value
		}
	}

	if len(merged) == 0 {
		return nil
	}

	return merged
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

	if e.Decision.Rule != "" {
		return "permission denied by policy (" + e.Decision.Rule + "): " + e.Decision.Reason
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
	classification := classifyCommand(program, args, command)
	kinds := sortedKinds(classification.classes)
	ops := make([]Operation, 0, len(kinds))
	action := displayCommand(program, args, command)
	readOnlyInspection := classification.readOnlyInspection && readOnlyInspectionKinds(kinds)

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

func sideEffectingClassifiedOperations(classes map[OperationKind]bool) bool {
	return classes[OperationWrite] ||
		classes[OperationNetwork] ||
		classes[OperationCredentialAccess] ||
		classes[OperationGitMutation] ||
		classes[OperationMergeDelete]
}

// AllowsReadOnlyExecution reports whether ops are a classified read-only
// inspection that may execute even when execute is otherwise denied. It is used
// by provider/tool preflight policies that need to mirror Evaluate's read-only
// exception without recording an extra allowed audit entry before the actual
// execution gate runs.
func AllowsReadOnlyExecution(policy Policy, ops []Operation) bool {
	if !policy.AllowReadExecution || policy.ModeFor(OperationRead) != ModeAllow {
		return false
	}

	hasRead := false
	hasReadOnlyExecute := false

	for _, op := range ops {
		switch op.Kind {
		case OperationRead:
			hasRead = true
		case OperationExecute:
			if op.Metadata[readOnlyInspectionKey] != "true" {
				return false
			}

			hasReadOnlyExecute = true

			continue
		default:
			return false
		}
	}

	return hasRead && hasReadOnlyExecute
}

// ClassifyCommand maps a command invocation to coarse permission classes. It is
// intentionally conservative for common shell/git/network/write patterns and
// leaves fine-grained command allow/deny lists to pkg/shell.
func ClassifyCommand(program string, args []string, command string) []OperationKind {
	return sortedKinds(classifyCommand(program, args, command).classes)
}

type commandClassification struct {
	classes            map[OperationKind]bool
	readOnlyInspection bool
}

func classifyCommand(program string, args []string, command string) commandClassification {
	classes := map[OperationKind]bool{OperationExecute: true}

	text := strings.TrimSpace(command)
	if text == "" {
		if shellCommand, ok := shellCommandFromProgramArgs(program, args); ok {
			text = shellCommand
		} else {
			text = strings.TrimSpace(strings.Join(append([]string{program}, args...), " "))
		}
	}

	if text == "" {
		return commandClassification{classes: classes}
	}

	if containsShellWriteRedirection(text) {
		classes[OperationWrite] = true
	}

	readOnlyInspection := true
	seenSegment := false

	for _, fields := range commandSegments(text) {
		seenSegment = true

		if !classifyFields(classes, fields) {
			readOnlyInspection = false
		}
	}

	if !seenSegment || sideEffectingClassifiedOperations(classes) {
		readOnlyInspection = false
	}

	return commandClassification{classes: classes, readOnlyInspection: readOnlyInspection}
}

func shellCommandFromProgramArgs(program string, args []string) (string, bool) {
	name := strings.ToLower(filepath.Base(strings.Trim(program, `"'`)))
	if !policyShellName(name) || len(args) < 2 {
		return "", false
	}

	for i := range len(args) - 1 {
		if shellCommandOptionInvokesCommand(args[i]) {
			return strings.TrimSpace(args[i+1]), strings.TrimSpace(args[i+1]) != ""
		}
	}

	return "", false
}

func displayCommand(program string, args []string, command string) string {
	if strings.TrimSpace(command) != "" {
		return strings.TrimSpace(command)
	}

	parts := append([]string{strings.TrimSpace(program)}, args...)

	return strings.TrimSpace(strings.Join(parts, " "))
}

func commandSegments(command string) [][]string {
	parts := commandSegmentTexts(command)
	segments := make([][]string, 0, len(parts))

	for _, part := range parts {
		fields := shellFields(strings.TrimSpace(part))
		if len(fields) > 0 {
			segments = append(segments, fields)
		}
	}

	for _, script := range commandSubstitutionScripts(command) {
		for _, part := range commandSegmentTexts(script) {
			fields := shellFields(strings.TrimSpace(part))
			if len(fields) > 0 {
				segments = append(segments, fields)
			}
		}
	}

	return segments
}

//nolint:gocognit,cyclop // This scanner intentionally keeps shell quote state local and explicit.
func commandSegmentTexts(command string) []string {
	var segments []string

	var current strings.Builder

	inSingle := false
	inDouble := false
	inBacktick := false
	escaped := false

	flush := func() {
		if segment := strings.TrimSpace(current.String()); segment != "" {
			segments = append(segments, segment)
		}

		current.Reset()
	}

	runes := []rune(command)
	for i, r := range runes {
		if escaped {
			current.WriteRune(r)

			escaped = false

			continue
		}

		if r == '\\' && !inSingle {
			current.WriteRune(r)

			escaped = true

			continue
		}

		switch r {
		case '\'':
			if !inDouble && !inBacktick {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle && !inBacktick {
				inDouble = !inDouble
			}
		case '`':
			if !inSingle {
				inBacktick = !inBacktick
			}
		}

		if !inSingle && !inDouble && !inBacktick && commandSegmentDelimiterAt(runes, i) {
			flush()

			continue
		}

		current.WriteRune(r)
	}

	flush()

	return segments
}

func commandSegmentDelimiterAt(runes []rune, index int) bool {
	r := runes[index]
	if r == '&' && shellAmpersandRedirection(runes, index) {
		return false
	}

	return commandSegmentDelimiter(r)
}

func shellAmpersandRedirection(runes []rune, index int) bool {
	return (index > 0 && runes[index-1] == '>') || (index+1 < len(runes) && runes[index+1] == '>')
}

func commandSegmentDelimiter(r rune) bool {
	switch r {
	case ';', '&', '|', '\n', '\r', '(', ')':
		return true
	default:
		return false
	}
}

//nolint:gocognit,cyclop // Shell word splitting needs explicit quote, escape, and redirection state.
func shellFields(text string) []string {
	var (
		fields  []string
		current strings.Builder
	)

	runes := []rune(text)

	inSingle := false
	inDouble := false
	inBacktick := false
	escaped := false
	hasField := false

	emit := func() {
		if hasField || current.Len() > 0 {
			fields = append(fields, current.String())
		}

		current.Reset()

		hasField = false
	}

	emitRedirection := func(redirection string) {
		emit()

		fields = append(fields, redirection)
	}

	for i := 0; i < len(runes); i++ {
		r := runes[i]

		if escaped {
			current.WriteRune(r)

			hasField = true
			escaped = false

			continue
		}

		if r == '\\' && !inSingle {
			hasField = true
			escaped = true

			continue
		}

		if !inSingle && !inDouble && !inBacktick {
			if r == '&' && i+1 < len(runes) && runes[i+1] == '>' {
				emitRedirection("&>")

				i++

				continue
			}

			if localShellRedirectionRune(runes, i) {
				fdPrefix := ""
				if currentDigits := current.String(); decimalDigits(currentDigits) {
					fdPrefix = currentDigits

					current.Reset()

					hasField = false
				}

				redirection, consumed := readShellRedirectionToken(runes, i)
				emitRedirection(fdPrefix + redirection)

				i += consumed - 1

				continue
			}
		}

		switch r {
		case '\'':
			if !inDouble && !inBacktick {
				inSingle = !inSingle
				hasField = true

				continue
			}
		case '"':
			if !inSingle && !inBacktick {
				inDouble = !inDouble
				hasField = true

				continue
			}
		case '`':
			if !inSingle {
				inBacktick = !inBacktick
			}
		case ' ', '\t', '\n', '\r':
			if !inSingle && !inDouble && !inBacktick {
				emit()

				continue
			}
		}

		current.WriteRune(r)

		hasField = true
	}

	if escaped {
		current.WriteRune('\\')
	}

	emit()

	return fields
}

func shellRedirectionRune(r rune) bool {
	return r == '<' || r == '>'
}

func localShellRedirectionRune(runes []rune, index int) bool {
	r := runes[index]

	return shellRedirectionRune(r) && (r != '<' || index+1 >= len(runes) || runes[index+1] != '(')
}

func decimalDigits(value string) bool {
	if value == "" {
		return false
	}

	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}

	return true
}

func readShellRedirectionToken(runes []rune, start int) (token string, consumed int) {
	if start < 0 || start >= len(runes) {
		return "", 0
	}

	first := runes[start]
	if !shellRedirectionRune(first) {
		return string(first), 1
	}

	var out strings.Builder
	out.WriteRune(first)

	i := start + 1
	for i < len(runes) && runes[i] == first {
		out.WriteRune(runes[i])
		i++
	}

	if first == '>' && i < len(runes) && runes[i] == '|' {
		out.WriteRune(runes[i])
		i++
	}

	if first == '<' && i < len(runes) && runes[i] == '>' {
		out.WriteRune(runes[i])
		i++
	}

	return out.String(), i - start
}

func commandSubstitutionScripts(command string) []string {
	scripts := backtickSubstitutionScripts(command)

	return append(scripts, dollarSubstitutionScripts(command)...)
}

//nolint:gocognit // Shell quoting state is easier to audit when kept as one small scanner.
func backtickSubstitutionScripts(command string) []string {
	var scripts []string

	inSingle := false
	inDouble := false
	inBacktick := false
	escaped := false
	start := -1

	for i, r := range command {
		if escaped {
			escaped = false

			continue
		}

		if r == '\\' && !inSingle {
			escaped = true

			continue
		}

		if inBacktick {
			if r == '`' {
				if script := strings.TrimSpace(command[start:i]); script != "" {
					scripts = append(scripts, script)
				}

				inBacktick = false
				start = -1
			}

			continue
		}

		switch r {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		case '`':
			if !inSingle {
				inBacktick = true
				start = i + 1
			}
		}
	}

	return scripts
}

//nolint:gocognit // Command substitution parsing is intentionally local and conservative.
func dollarSubstitutionScripts(command string) []string {
	var scripts []string

	runes := []rune(command)
	inSingle := false
	inDouble := false
	escaped := false

	for i := 0; i < len(runes); i++ {
		r := runes[i]

		if escaped {
			escaped = false

			continue
		}

		if r == '\\' && !inSingle {
			escaped = true

			continue
		}

		switch r {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		case '$':
			if inSingle || i+1 >= len(runes) || runes[i+1] != '(' {
				continue
			}

			script, end, ok := readDollarSubstitutionScript(runes, i+2)
			if ok {
				if script = strings.TrimSpace(script); script != "" {
					scripts = append(scripts, script)
				}

				i = end
			}
		}
	}

	return scripts
}

//nolint:gocognit,cyclop // Nested shell substitution state is easier to audit in one scanner.
func readDollarSubstitutionScript(runes []rune, start int) (script string, end int, ok bool) {
	var current strings.Builder

	depth := 1
	inSingle := false
	inDouble := false
	escaped := false

	for i := start; i < len(runes); i++ {
		r := runes[i]
		if escaped {
			current.WriteRune(r)

			escaped = false

			continue
		}

		if r == '\\' && !inSingle {
			current.WriteRune(r)

			escaped = true

			continue
		}

		switch r {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		case '(':
			if !inSingle && !inDouble {
				depth++
			}
		case ')':
			if !inSingle && !inDouble {
				depth--
				if depth == 0 {
					return current.String(), i, true
				}
			}
		}

		current.WriteRune(r)
	}

	return "", len(runes), false
}

func classifyFields(classes map[OperationKind]bool, fields []string) bool {
	if len(fields) == 0 {
		return false
	}

	credentialAccessed := classifyCredentialFields(classes, fields)
	credentialAccessed = classifyInputRedirectionFields(classes, fields) || credentialAccessed

	if script, ok := nestedShellCommandScript(fields); ok {
		readOnlyInspection := true

		for _, nestedFields := range commandSegments(script) {
			if !classifyFields(classes, nestedFields) {
				readOnlyInspection = false
			}
		}

		return readOnlyInspection && !credentialAccessed
	}

	fields = normalizeCommandFields(fields)

	if len(fields) == 0 {
		return false
	}

	credentialAccessed = classifyCredentialFields(classes, fields) || credentialAccessed
	credentialAccessed = classifyInputRedirectionFields(classes, fields) || credentialAccessed
	credentialAccessed = classifyOutputRedirectionFields(classes, fields) || credentialAccessed

	fields = stripShellRedirectionFields(fields)
	if len(fields) == 0 {
		return !credentialAccessed
	}

	return classifyCommandFields(classes, fields, credentialAccessed)
}

func classifyCommandFields(classes map[OperationKind]bool, fields []string, credentialAccessed bool) bool {
	name := strings.ToLower(filepath.Base(strings.Trim(fields[0], `"'`)))
	if commandAccessesCredentialPath(name, fields[1:]) {
		classes[OperationCredentialAccess] = true
		credentialAccessed = true
	}

	write, credentialWrite := commandWritesLocalOutput(name, fields[1:])
	if write {
		classes[OperationWrite] = true
	}

	if credentialWrite {
		classes[OperationCredentialAccess] = true
		credentialAccessed = true
	}

	if handled, readOnlyInspection := classifySimpleCommandFields(classes, name); handled {
		return readOnlyInspection && !credentialAccessed
	}

	if handled, readOnlyInspection := classifyStructuredCommandFields(classes, name, fields); handled {
		return readOnlyInspection && !credentialAccessed
	}

	return false
}

func commandWritesLocalOutput(name string, args []string) (write, credential bool) {
	switch name {
	case "curl":
		return curlWritesLocalOutput(args)
	case "wget":
		// wget writes response bodies to the current directory by default. Keep
		// this conservative so deny-write policies do not allow network downloads
		// just because no shell redirection was used.
		return true, false
	default:
		return false, false
	}
}

func curlWritesLocalOutput(args []string) (write, credential bool) {
	for i, arg := range args {
		arg = strings.Trim(strings.TrimSpace(arg), `"'`)
		if arg == "" {
			continue
		}

		next := ""
		if i+1 < len(args) {
			next = strings.Trim(args[i+1], `"'`)
		}

		argWrites, argCredential := curlOutputEffect(arg, next)
		write = write || argWrites
		credential = credential || argCredential
	}

	return write, credential
}

func curlOutputEffect(arg, next string) (write, credential bool) {
	if arg == "-O" || arg == "--remote-name" || curlShortOptionsContainRemoteName(arg) {
		return true, false
	}

	if target, ok := curlFileOutputTarget(arg, next); ok {
		return outputTargetEffect(target, false)
	}

	if target, ok := curlCredentialOutputTarget(arg, next); ok {
		return outputTargetEffect(target, true)
	}

	return false, false
}

func curlFileOutputTarget(arg, next string) (string, bool) {
	switch {
	case arg == "-o" || arg == "--output" || arg == "-D" || arg == "--dump-header":
		return next, true
	case strings.HasPrefix(arg, "--output="):
		return strings.TrimPrefix(arg, "--output="), true
	case strings.HasPrefix(arg, "-o"):
		return strings.TrimPrefix(arg, "-o"), true
	case curlShortOptionsContainOutputWithNextValue(arg):
		return next, true
	case strings.HasPrefix(arg, "--dump-header="):
		return strings.TrimPrefix(arg, "--dump-header="), true
	default:
		return "", false
	}
}

func curlCredentialOutputTarget(arg, next string) (string, bool) {
	switch {
	case arg == "-c" || arg == "--cookie-jar":
		return next, true
	case strings.HasPrefix(arg, "--cookie-jar="):
		return strings.TrimPrefix(arg, "--cookie-jar="), true
	default:
		return "", false
	}
}

func outputTargetEffect(target string, alwaysCredential bool) (write, credential bool) {
	if !targetWritesLocalFile(target) {
		return false, false
	}

	return true, alwaysCredential || credentialPathArgument(target)
}

func curlShortOptionsContainRemoteName(arg string) bool {
	if strings.HasPrefix(arg, "--") || !strings.HasPrefix(arg, "-") || arg == "-" {
		return false
	}

	return strings.Contains(arg[1:], "O")
}

func curlShortOptionsContainOutputWithNextValue(arg string) bool {
	if strings.HasPrefix(arg, "--") || !strings.HasPrefix(arg, "-") || len(arg) < 3 {
		return false
	}

	return strings.HasSuffix(arg, "o")
}

func targetWritesLocalFile(target string) bool {
	target = strings.Trim(strings.TrimSpace(target), `"'`)

	return target != "" && target != "-"
}

func classifyStructuredCommandFields(classes map[OperationKind]bool, name string, fields []string) (handled, readOnlyInspection bool) {
	switch name {
	case "eval":
		return true, classifyEval(classes, fields[1:])
	case "find":
		return true, classifyFind(classes, fields[1:])
	case "xargs":
		return true, classifyXargs(classes, fields[1:])
	case commandNameSed:
		return true, classifySed(classes, fields[1:])
	case "source", ".":
		// Sourcing reads a local file and executes arbitrary shell content in
		// the current shell. Treat it as write-capable so read-only or
		// write-deny policies fail closed before hidden side effects can run.
		classes[OperationRead] = true
		classes[OperationWrite] = true

		return true, false
	case "sudo":
		return true, classifyCredentialWrapper(classes, fields[1:], sudoOptionNeedsValue)
	case "security":
		classifySecurity(classes, fields[1:])

		return true, false
	case "git":
		return true, classifyGit(classes, fields[1:])
	case "go", "npm", "pnpm", "yarn", "pip", "pip3", "poetry", "cargo", "brew":
		network, write := dependencyCommandSideEffects(name, fields[1:])
		if network {
			classes[OperationNetwork] = true
		}

		if write {
			classes[OperationWrite] = true
		}

		return true, false
	default:
		return false, false
	}
}

func classifySed(classes map[OperationKind]bool, args []string) bool {
	if sedCommandMutates(args) {
		classes[OperationWrite] = true

		return false
	}

	classes[OperationRead] = true

	return true
}

func classifySimpleCommandFields(classes map[OperationKind]bool, name string) (handled, readOnlyInspection bool) {
	switch name {
	case "cat", "grep", "rg", "awk", "ls", "pwd", "printf", "echo", "head", "tail", "wc", "stat", "test", "true", "false", "less", "more":
		classes[OperationRead] = true

		return true, true
	case "touch", "mkdir", "tee", "cp", "mv", commandNameChmod, commandNameChown, commandNameInstall, "truncate", commandNamePatch:
		classes[OperationWrite] = true

		return true, false
	case "rm", "rmdir", "unlink", "shred":
		classes[OperationWrite] = true
		classes[OperationMergeDelete] = true

		return true, false
	case "curl", "wget", "ssh", "scp", "sftp", "rsync", "nc", "ncat", "netcat", "telnet", "ftp", "dig", "nslookup", "ping",
		"gh", "glab", "hub", "kubectl", "aws", "gcloud", "az":
		classes[OperationNetwork] = true
		if networkCommandAccessesCredentials(name) {
			classes[OperationCredentialAccess] = true
		}

		return true, false
	default:
		return false, false
	}
}

func networkCommandAccessesCredentials(name string) bool {
	switch name {
	case "ssh", "scp", "sftp", "rsync", "gh", "glab", "hub", "kubectl", "aws", "gcloud", "az":
		return true
	default:
		return false
	}
}

func nestedShellCommandScript(fields []string) (string, bool) {
	for {
		fields = trimLeadingAssignmentFields(fields)
		if len(fields) == 0 {
			return "", false
		}

		name := strings.ToLower(filepath.Base(strings.Trim(fields[0], `"'`)))
		if name == "env" && len(fields) > 1 {
			next := unwrapEnv(fields)
			if slices.Equal(fields, next) {
				return "", false
			}

			fields = next

			continue
		}

		if !policyShellName(name) || len(fields) < 3 {
			return "", false
		}

		for i := 1; i < len(fields)-1; i++ {
			if shellCommandOptionInvokesCommand(fields[i]) {
				script := strings.TrimSpace(fields[i+1])

				return script, script != ""
			}
		}

		return "", false
	}
}

func classifyCredentialFields(classes map[OperationKind]bool, fields []string) bool {
	accessed := false

	for _, field := range fields {
		if credentialAssignment(field) || credentialFlag(field) {
			classes[OperationCredentialAccess] = true
			accessed = true
		}
	}

	return accessed
}

func classifyInputRedirectionFields(classes map[OperationKind]bool, fields []string) bool {
	accessedCredential := false

	for i, field := range fields {
		nextValue := ""
		if i+1 < len(fields) {
			nextValue = fields[i+1]
		}

		value, reads := inputRedirectionValue(field, nextValue)
		if !reads {
			continue
		}

		classes[OperationRead] = true
		if credentialPathArgument(value) {
			classes[OperationCredentialAccess] = true
			accessedCredential = true
		}
	}

	return accessedCredential
}

func classifyOutputRedirectionFields(classes map[OperationKind]bool, fields []string) bool {
	accessedCredential := false

	for i, field := range fields {
		nextValue := ""
		if i+1 < len(fields) {
			nextValue = fields[i+1]
		}

		value, writes := outputRedirectionValue(field, nextValue)
		if !writes {
			continue
		}

		classes[OperationWrite] = true
		if credentialPathArgument(value) {
			classes[OperationCredentialAccess] = true
			accessedCredential = true
		}
	}

	return accessedCredential
}

func inputRedirectionValue(field, next string) (string, bool) {
	field = strings.Trim(strings.TrimSpace(field), `"'`)
	if field == "" {
		return "", false
	}

	field = stripLeadingFileDescriptor(field)

	switch {
	case field == "<" || field == "<<" || field == "<<<" || field == "<>":
		return next, true
	case strings.HasPrefix(field, "<("):
		// Process substitution is parsed as its own command segment elsewhere,
		// not as a local file redirection target.
		return "", false
	case strings.HasPrefix(field, "<<<"):
		return strings.TrimPrefix(field, "<<<"), true
	case strings.HasPrefix(field, "<<"):
		return strings.TrimPrefix(field, "<<"), true
	case strings.HasPrefix(field, "<>"):
		return strings.TrimPrefix(field, "<>"), true
	case strings.HasPrefix(field, "<"):
		return strings.TrimPrefix(field, "<"), true
	default:
		return "", false
	}
}

func outputRedirectionValue(field, next string) (string, bool) {
	field = strings.Trim(strings.TrimSpace(field), `"'`)
	if field == "" {
		return "", false
	}

	if inline, ok := strings.CutPrefix(field, "&>"); ok {
		return nonEmptyRedirectionTarget(outputRedirectionTarget(inline, next))
	}

	field = stripLeadingFileDescriptor(field)

	switch {
	case field == ">" || field == ">>" || field == ">|":
		return nonEmptyRedirectionTarget(outputRedirectionTarget("", next))
	case strings.HasPrefix(field, ">>"):
		return nonEmptyRedirectionTarget(outputRedirectionTarget(strings.TrimPrefix(field, ">>"), next))
	case strings.HasPrefix(field, ">|"):
		return nonEmptyRedirectionTarget(outputRedirectionTarget(strings.TrimPrefix(field, ">|"), next))
	case strings.HasPrefix(field, ">"):
		return nonEmptyRedirectionTarget(outputRedirectionTarget(strings.TrimPrefix(field, ">"), next))
	default:
		return "", false
	}
}

func nonEmptyRedirectionTarget(target string) (string, bool) {
	return target, strings.TrimSpace(target) != ""
}

func outputRedirectionTarget(inline, next string) string {
	target := strings.TrimSpace(inline)
	if target == "" {
		target = strings.TrimSpace(next)
	}

	target = strings.Trim(target, `"'`)
	if strings.HasPrefix(target, "&") || target == "-" {
		return ""
	}

	return target
}

func stripShellRedirectionFields(fields []string) []string {
	if len(fields) == 0 {
		return fields
	}

	out := make([]string, 0, len(fields))

	for i := 0; i < len(fields); i++ {
		field := fields[i]
		if shellRedirectionField(field) {
			if redirectionConsumesNextField(field) && i+1 < len(fields) {
				i++
			}

			continue
		}

		out = append(out, field)
	}

	return out
}

func shellRedirectionField(field string) bool {
	field = strings.Trim(strings.TrimSpace(field), `"'`)
	if field == "" {
		return false
	}

	if strings.HasPrefix(field, "&>") {
		return true
	}

	field = stripLeadingFileDescriptor(field)

	return strings.HasPrefix(field, ">") || strings.HasPrefix(field, "<")
}

func redirectionConsumesNextField(field string) bool {
	field = strings.Trim(strings.TrimSpace(field), `"'`)
	if field == "" {
		return false
	}

	if inline, ok := strings.CutPrefix(field, "&>"); ok {
		return strings.TrimSpace(inline) == ""
	}

	field = stripLeadingFileDescriptor(field)

	for _, op := range []string{">>", ">|", "<<<", "<<", "<>", ">", "<"} {
		if inline, ok := strings.CutPrefix(field, op); ok {
			return strings.TrimSpace(inline) == ""
		}
	}

	return false
}

func stripLeadingFileDescriptor(field string) string {
	for field != "" && field[0] >= '0' && field[0] <= '9' {
		field = field[1:]
	}

	return field
}

func normalizeCommandFields(fields []string) []string {
	for {
		next := trimLeadingAssignmentFields(fields)
		next = unwrapShell(next)
		next = unwrapCommandWrapper(next)

		if slices.Equal(fields, next) {
			return next
		}

		fields = next
	}
}

func unwrapCommandWrapper(fields []string) []string {
	if len(fields) == 0 {
		return fields
	}

	name := strings.ToLower(filepath.Base(strings.Trim(fields[0], `"'`)))

	switch name {
	case "command", "builtin", "exec", "noglob", "nohup":
		return fields[1:]
	case "time":
		return unwrapOptionWrapper(fields[1:], timeOptionNeedsValue)
	case "nice":
		return unwrapOptionWrapper(fields[1:], niceOptionNeedsValue)
	default:
		return fields
	}
}

func unwrapOptionWrapper(fields []string, optionNeedsValue func(string) bool) []string {
	for i := 0; i < len(fields); i++ {
		arg := strings.Trim(fields[i], `"'`)
		if arg == "" {
			continue
		}

		if arg == "--" {
			return fields[i+1:]
		}

		if !strings.HasPrefix(arg, "-") || arg == "-" {
			return fields[i:]
		}

		if optionNeedsValue(arg) && !strings.Contains(arg, "=") {
			i++
		}
	}

	return nil
}

func timeOptionNeedsValue(arg string) bool {
	return arg == "-f" || arg == "--format" || strings.HasPrefix(arg, "--format=")
}

func niceOptionNeedsValue(arg string) bool {
	return arg == "-n" || arg == "--adjustment" || strings.HasPrefix(arg, "--adjustment=")
}

func sudoOptionNeedsValue(arg string) bool {
	switch arg {
	case "-a", "-b", "-C", "-c", "-D", "-g", "-h", "-p", "-R", "-r", "-t", "-T", "-U", "-u":
		return true
	default:
		for _, option := range []string{
			"--askpass",
			"--close-from",
			"--chdir",
			"--group",
			"--host",
			"--prompt",
			"--role",
			"--type",
			"--user",
		} {
			if arg == option || strings.HasPrefix(arg, option+"=") {
				return true
			}
		}

		return false
	}
}

func trimLeadingAssignmentFields(fields []string) []string {
	for len(fields) > 0 && shellAssignmentField(fields[0]) {
		fields = fields[1:]
	}

	return fields
}

func shellAssignmentField(field string) bool {
	name, _, ok := strings.Cut(strings.TrimSpace(field), "=")
	if !ok || name == "" || strings.HasPrefix(name, "-") || strings.ContainsAny(name, `/\`) {
		return false
	}

	for i, r := range name {
		switch {
		case r == '_':
			continue
		case r >= 'A' && r <= 'Z':
			continue
		case r >= 'a' && r <= 'z':
			continue
		case i > 0 && r >= '0' && r <= '9':
			continue
		default:
			return false
		}
	}

	return true
}

func classifyFind(classes map[OperationKind]bool, args []string) bool {
	classes[OperationRead] = true
	readOnlyInspection := true

	for i, arg := range args {
		arg = strings.ToLower(strings.Trim(arg, `"'`))
		switch arg {
		case "-delete":
			classes[OperationWrite] = true
			classes[OperationMergeDelete] = true
			readOnlyInspection = false
		case "-exec", "-execdir", "-ok", "-okdir":
			if !classifyFindExec(classes, args[i+1:]) {
				readOnlyInspection = false
			}
		}
	}

	return readOnlyInspection
}

func classifyFindExec(classes map[OperationKind]bool, args []string) bool {
	fields := findExecFields(args)
	if len(fields) == 0 {
		return false
	}

	nested := map[OperationKind]bool{OperationExecute: true}
	readOnlyInspection := classifyFields(nested, fields)

	if len(nested) == 1 && nested[OperationExecute] {
		// Unknown find -exec targets may perform arbitrary side effects. Mark
		// them as write-capable so read-only mode does not treat the parent find
		// command as harmless inspection.
		classes[OperationWrite] = true

		return false
	}

	for kind := range nested {
		classes[kind] = true
	}

	return readOnlyInspection && !sideEffectingClassifiedOperations(nested)
}

func findExecFields(args []string) []string {
	out := make([]string, 0, len(args))
	for _, arg := range args {
		trimmed := strings.Trim(arg, `"'`)
		if trimmed == ";" || trimmed == "+" {
			break
		}

		out = append(out, arg)
	}

	return out
}

func classifyXargs(classes map[OperationKind]bool, args []string) bool {
	classes[OperationRead] = true

	fields := xargsCommandFields(args)
	if len(fields) == 0 {
		return true
	}

	nested := map[OperationKind]bool{OperationExecute: true}
	readOnlyInspection := classifyFields(nested, fields)

	if len(nested) == 1 && nested[OperationExecute] {
		// Unknown xargs targets are arbitrary commands. Treat them as write
		// capable so deny-write policies do not permit hidden side effects just
		// because the immediate program was xargs.
		classes[OperationWrite] = true

		return false
	}

	for kind := range nested {
		classes[kind] = true
	}

	return readOnlyInspection && !sideEffectingClassifiedOperations(nested)
}

func classifyEval(classes map[OperationKind]bool, args []string) bool {
	script := strings.TrimSpace(strings.Join(args, " "))
	if script == "" {
		return false
	}

	nested := map[OperationKind]bool{OperationExecute: true}
	readOnlyInspection := true

	for _, fields := range commandSegments(script) {
		if !classifyFields(nested, fields) {
			readOnlyInspection = false
		}
	}

	if len(nested) == 1 && nested[OperationExecute] {
		// Dynamic eval payloads may perform arbitrary side effects. Mark them
		// as write-capable so deny-write policies do not permit hidden writes.
		classes[OperationWrite] = true

		return false
	}

	for kind := range nested {
		classes[kind] = true
	}

	return readOnlyInspection && !sideEffectingClassifiedOperations(nested)
}

func classifyCredentialWrapper(classes map[OperationKind]bool, args []string, optionNeedsValue func(string) bool) bool {
	classes[OperationCredentialAccess] = true

	fields := unwrapOptionWrapper(args, optionNeedsValue)
	if len(fields) == 0 {
		return false
	}

	nested := map[OperationKind]bool{OperationExecute: true}
	readOnlyInspection := classifyFields(nested, fields)

	if len(nested) == 1 && nested[OperationExecute] {
		classes[OperationWrite] = true

		return false
	}

	for kind := range nested {
		classes[kind] = true
	}

	return readOnlyInspection && !sideEffectingClassifiedOperations(nested)
}

func classifySecurity(classes map[OperationKind]bool, args []string) {
	classes[OperationCredentialAccess] = true

	action := securityAction(args)
	if action == "" {
		classes[OperationRead] = true

		return
	}

	if securityActionMutates(action) {
		classes[OperationWrite] = true
	}
}

func securityAction(args []string) string {
	for i, arg := range args {
		arg = strings.TrimSpace(strings.Trim(arg, `"'`))
		if arg == "" {
			continue
		}

		if arg == "--" {
			if i+1 >= len(args) {
				return ""
			}

			return strings.ToLower(strings.Trim(args[i+1], `"'`))
		}

		if strings.HasPrefix(arg, "-") {
			continue
		}

		return strings.ToLower(arg)
	}

	return ""
}

func securityActionMutates(action string) bool {
	return strings.HasPrefix(action, "add-") ||
		strings.HasPrefix(action, "delete-") ||
		strings.HasPrefix(action, "set-") ||
		strings.HasPrefix(action, "create-") ||
		strings.HasPrefix(action, "import") ||
		strings.HasPrefix(action, "export") ||
		action == "lock-keychain" ||
		action == "unlock-keychain"
}

func xargsCommandFields(args []string) []string {
	for i := 0; i < len(args); i++ {
		arg := strings.Trim(args[i], `"'`)
		if arg == "" {
			continue
		}

		if arg == "--" {
			return args[i+1:]
		}

		if !strings.HasPrefix(arg, "-") || arg == "-" {
			return args[i:]
		}

		if xargsOptionNeedsValue(arg) && !strings.Contains(arg, "=") && i+1 < len(args) {
			i++
		}
	}

	return nil
}

func xargsOptionNeedsValue(arg string) bool {
	for _, option := range []string{
		"-a", "--arg-file",
		"-d", "--delimiter",
		"-E", "--eof",
		"-I", "--replace",
		"-L", "--max-lines",
		"-n", "--max-args",
		"-P", "--max-procs",
		"-s", "--max-chars",
	} {
		if arg == option || strings.HasPrefix(arg, option+"=") {
			return true
		}
	}

	return false
}

func sedCommandMutates(args []string) bool {
	for _, arg := range args {
		arg = strings.ToLower(strings.Trim(arg, `"'`))
		if strings.HasPrefix(arg, "-i") ||
			arg == "--in-place" ||
			strings.HasPrefix(arg, "--in-place=") {
			return true
		}
	}

	return false
}

func dependencyCommandSideEffects(name string, args []string) (network, write bool) {
	action, rest := dependencyActionAndRest(args)

	switch name {
	case "go":
		if action == "get" || action == "install" {
			return true, true
		}

		if action == "mod" {
			next, _ := dependencyActionAndRest(rest)
			changes := next == "download" || next == "tidy"

			return changes, changes
		}
	case "npm", "pnpm":
		return dependencyNetworkWrite(action, "install", "i", "add", "update", "up", "ci")
	case "yarn":
		return dependencyNetworkWrite(action, "install", "add", "upgrade", "up")
	case "pip", "pip3":
		return dependencyNetworkWrite(action, "install", "download", "wheel")
	case "poetry":
		return dependencyNetworkWrite(action, "add", "install", "update")
	case "cargo":
		return dependencyNetworkWrite(action, "install", "add", "update", "fetch")
	case "brew":
		return dependencyNetworkWrite(action, "install", "update", "upgrade", "tap")
	}

	return false, false
}

func dependencyActionAndRest(args []string) (action string, rest []string) {
	for i := 0; i < len(args); i++ {
		arg := strings.ToLower(strings.Trim(strings.TrimSpace(args[i]), `"'`))
		if arg == "" || arg == "--" {
			continue
		}

		if dependencyOptionWithValue(arg) {
			if !strings.Contains(arg, "=") {
				i++
			}

			continue
		}

		if strings.HasPrefix(arg, "-") {
			continue
		}

		return arg, args[i+1:]
	}

	return "", nil
}

func dependencyOptionWithValue(arg string) bool {
	switch {
	case arg == "-c", arg == "-m", arg == "--cwd", arg == "--prefix", arg == "--cache", arg == "--directory":
		return true
	case strings.HasPrefix(arg, "--cwd="),
		strings.HasPrefix(arg, "--prefix="),
		strings.HasPrefix(arg, "--cache="),
		strings.HasPrefix(arg, "--directory="):
		return true
	default:
		return false
	}
}

func dependencyActionIn(action string, candidates ...string) bool {
	return slices.Contains(candidates, action)
}

func dependencyNetworkWrite(action string, candidates ...string) (network, write bool) {
	changes := dependencyActionIn(action, candidates...)

	return changes, changes
}

func unwrapShell(fields []string) []string {
	if len(fields) == 0 {
		return fields
	}

	name := strings.ToLower(filepath.Base(strings.Trim(fields[0], `"'`)))
	if policyShellName(name) && len(fields) >= 3 {
		for i := 1; i < len(fields)-1; i++ {
			if shellCommandOptionInvokesCommand(fields[i]) {
				return strings.Fields(strings.Trim(fields[i+1], `"'`))
			}
		}
	}

	if name == "env" && len(fields) > 1 {
		return unwrapEnv(fields)
	}

	return fields
}

func unwrapEnv(fields []string) []string {
	for i := 1; i < len(fields); i++ {
		arg := strings.Trim(fields[i], `"'`)

		switch {
		case arg == "--":
			return fields[i+1:]
		case envSplitStringOption(arg):
			if i+1 >= len(fields) {
				return nil
			}

			return shellFields(fields[i+1])
		case envSplitStringInlineValue(arg) != "":
			return shellFields(envSplitStringInlineValue(arg))
		case envOptionNeedsValue(arg):
			i++
		case envOption(arg):
			continue
		case shellAssignmentField(fields[i]):
			continue
		default:
			return fields[i:]
		}
	}

	return fields
}

func envSplitStringOption(arg string) bool {
	return arg == "-S" || arg == "--split-string"
}

func envSplitStringInlineValue(arg string) string {
	if !strings.HasPrefix(arg, "--split-string=") {
		return ""
	}

	return strings.TrimPrefix(arg, "--split-string=")
}

func envOptionNeedsValue(arg string) bool {
	switch arg {
	case "-u", "--unset", "-C", "--chdir", "-S", "--split-string":
		return true
	default:
		return false
	}
}

func envOption(arg string) bool {
	return arg == "-" ||
		strings.HasPrefix(arg, "--") ||
		(strings.HasPrefix(arg, "-") && arg != "-")
}

func policyShellName(name string) bool {
	switch name {
	case shellNameBash, shellNameSh, shellNameZsh:
		return true
	default:
		return false
	}
}

func shellCommandOptionInvokesCommand(arg string) bool {
	arg = strings.Trim(arg, `"'`)
	if arg == "-c" {
		return true
	}

	return strings.HasPrefix(arg, "-") &&
		!strings.HasPrefix(arg, "--") &&
		strings.Contains(arg[1:], "c")
}

func classifyGit(classes map[OperationKind]bool, args []string) bool {
	if len(args) == 0 {
		classes[OperationRead] = true
		return true
	}

	args = trimGitGlobalOptions(args)

	if len(args) == 0 {
		classes[OperationRead] = true
		return true
	}

	sub := strings.ToLower(strings.Trim(args[0], `"'`))
	mutates := false
	readOnly := false

	usesNetwork := gitSubcommandUsesNetwork(sub, args[1:])
	if usesNetwork {
		classes[OperationNetwork] = true
		classes[OperationCredentialAccess] = true
	}

	switch sub {
	case "add", gitSubcommandApply, "commit", "checkout", "switch", "restore", "reset", "stash", "tag", "rm", "mv", "init", gitSubcommandBranch, gitSubcommandWorktree, "rebase", "merge", "cherry-pick", "revert", "clean", gitSubcommandPush, "pull", "fetch", "clone", gitSubcommandConfig, gitSubcommandRemote, gitSubcommandSubmodule:
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
		case gitSubcommandStatus, "diff", "log", gitSubcommandShow, "rev-parse", "merge-base", gitSubcommandBranch, gitSubcommandWorktree, "ls-files", gitSubcommandRemote, gitSubcommandConfig, gitSubcommandApply, gitSubcommandSubmodule:
			classes[OperationRead] = true
			readOnly = true
		}
	}

	return readOnly && !mutates && !mergesOrDeletes && !usesNetwork
}

func trimGitGlobalOptions(args []string) []string {
	for len(args) > 0 {
		arg := strings.Trim(args[0], `"'`)
		if arg == "" {
			args = args[1:]
			continue
		}

		switch {
		case arg == "-C" || arg == "-c" || arg == "--git-dir" || arg == "--work-tree" || arg == "--namespace":
			if len(args) < 2 {
				return nil
			}

			args = args[2:]
		case strings.HasPrefix(arg, "--git-dir=") ||
			strings.HasPrefix(arg, "--work-tree=") ||
			strings.HasPrefix(arg, "--namespace="):
			args = args[1:]
		case strings.HasPrefix(arg, "--"):
			args = args[1:]
		case strings.HasPrefix(arg, "-"):
			args = args[1:]
		default:
			return args
		}
	}

	return args
}

func gitSubcommandUsesNetwork(sub string, args []string) bool {
	switch sub {
	case "clone", "fetch", "pull", gitSubcommandPush, "ls-remote":
		return true
	case gitSubcommandRemote:
		return len(args) > 0 && strings.ToLower(strings.Trim(args[0], `"'`)) == gitSubcommandShow
	case gitSubcommandSubmodule:
		return gitSubmoduleUsesNetwork(args)
	default:
		return false
	}
}

func gitSubcommandMutates(sub string, args []string) bool {
	switch sub {
	case gitSubcommandStatus, "diff", "log", gitSubcommandShow, "rev-parse", "merge-base", "ls-files":
		return false
	case gitSubcommandBranch:
		return gitBranchSubcommandMutates(args)
	case gitSubcommandWorktree:
		return len(args) > 0 && !readOnlyGitWorktreeSubcommand(args[0])
	case gitSubcommandRemote:
		return len(args) > 0 && !readOnlyGitRemoteSubcommand(args[0])
	case gitSubcommandConfig:
		return len(args) > 0 && !strings.HasPrefix(args[0], "--get") && args[0] != "get" && args[0] != "list" && args[0] != "--list"
	case gitSubcommandApply:
		return !gitApplyCheckOnly(args)
	case gitSubcommandSubmodule:
		return gitSubmoduleMutates(args)
	default:
		return true
	}
}

func gitSubmoduleUsesNetwork(args []string) bool {
	switch gitSubmoduleSubcommand(args) {
	case "add", "update":
		return true
	default:
		return false
	}
}

func gitSubmoduleMutates(args []string) bool {
	switch gitSubmoduleSubcommand(args) {
	case "", gitSubcommandStatus, "summary":
		return false
	default:
		return true
	}
}

func gitSubmoduleSubcommand(args []string) string {
	for i := 0; i < len(args); i++ {
		arg := normalizedGitArg(args[i])
		if arg == "" || arg == "--" {
			continue
		}

		if gitSubmoduleOptionWithValue(arg) {
			if !strings.Contains(arg, "=") {
				i++
			}

			continue
		}

		if strings.HasPrefix(arg, "-") {
			continue
		}

		return arg
	}

	return ""
}

func gitSubmoduleOptionWithValue(arg string) bool {
	switch {
	case arg == "--jobs", arg == "-j":
		return true
	case strings.HasPrefix(arg, "--jobs="):
		return true
	default:
		return false
	}
}

func gitBranchSubcommandMutates(args []string) bool {
	if len(args) == 0 {
		return false
	}

	readSelector := false
	readOptionNeedsValue := false

	for _, arg := range args {
		arg = strings.ToLower(strings.Trim(arg, `"'`))

		if readOptionNeedsValue {
			readOptionNeedsValue = false
			continue
		}

		switch {
		case gitBranchReadFlag(arg):
			readSelector = true

			continue
		case gitBranchReadFlagWithValue(arg):
			readSelector = true
			readOptionNeedsValue = true

			continue
		case gitBranchReadFlagWithInlineValue(arg):
			readSelector = true

			continue
		}

		if strings.HasPrefix(arg, "-") {
			return true
		}

		return !readSelector
	}

	return false
}

func gitBranchReadFlag(arg string) bool {
	switch arg {
	case "--list", "-l", "--show-current", "--no-color", "--no-column",
		"--ignore-case", "-i", "-r", "--remotes", "-a", "--all", "-v", "-vv", "-q", "--quiet":
		return true
	default:
		return false
	}
}

func gitBranchReadFlagWithValue(arg string) bool {
	switch arg {
	case "--contains", "--no-contains", "--merged", "--no-merged", "--points-at", "--format", "--sort", "--color", "--column":
		return true
	default:
		return false
	}
}

func gitBranchReadFlagWithInlineValue(arg string) bool {
	for _, prefix := range []string{
		"--list=",
		"--contains=",
		"--no-contains=",
		"--merged=",
		"--no-merged=",
		"--points-at=",
		"--format=",
		"--sort=",
		"--color=",
		"--column=",
	} {
		if strings.HasPrefix(arg, prefix) {
			return true
		}
	}

	return false
}

func gitApplyCheckOnly(args []string) bool {
	if len(args) == 0 {
		return false
	}

	for _, arg := range args {
		arg = strings.ToLower(strings.Trim(arg, `"'`))
		switch arg {
		case "--check", "--stat", "--numstat", "--summary", "--cached", "--index", "-":
			continue
		}

		if strings.HasPrefix(arg, "-p") ||
			strings.HasPrefix(arg, "--directory=") ||
			strings.HasPrefix(arg, "--include=") ||
			strings.HasPrefix(arg, "--exclude=") {
			continue
		}

		if strings.HasPrefix(arg, "-") {
			return false
		}
	}

	return slices.Contains(args, "--check") ||
		slices.Contains(args, "--stat") ||
		slices.Contains(args, "--numstat") ||
		slices.Contains(args, "--summary")
}

func readOnlyGitWorktreeSubcommand(sub string) bool {
	switch strings.ToLower(strings.Trim(sub, `"'`)) {
	case "list":
		return true
	default:
		return false
	}
}

func readOnlyGitRemoteSubcommand(sub string) bool {
	switch strings.ToLower(strings.Trim(sub, `"'`)) {
	case "", "-v", "--verbose", gitSubcommandShow, "get-url":
		return true
	default:
		return false
	}
}

func gitSubcommandMergesOrDeletes(sub string, args []string) bool {
	switch sub {
	case "merge", "rebase", "clean", "rm":
		return true
	case gitSubcommandBranch:
		return gitArgsContainDeleteFlag(args)
	case "tag":
		return gitArgsContainDeleteFlag(args)
	case gitSubcommandWorktree:
		if len(args) == 0 {
			return false
		}

		sub = normalizedGitArg(args[0])

		return sub == "remove" || sub == "prune"
	case gitSubcommandRemote:
		if len(args) == 0 {
			return false
		}

		sub = normalizedGitArg(args[0])

		return sub == "remove" || sub == "rm" || sub == "prune"
	case gitSubcommandSubmodule:
		return gitSubmoduleSubcommand(args) == "deinit"
	case gitSubcommandPush:
		return gitPushIsDestructive(args)
	case "reset":
		return gitArgsContain(args, "--hard")
	}

	return false
}

func normalizedGitArg(arg string) string {
	return strings.ToLower(strings.Trim(arg, `"'`))
}

func gitArgsContain(args []string, want string) bool {
	for _, arg := range args {
		if normalizedGitArg(arg) == want {
			return true
		}
	}

	return false
}

func gitArgsContainDeleteFlag(args []string) bool {
	for _, arg := range args {
		arg = normalizedGitArg(arg)
		if arg == "-d" || arg == "--delete" || strings.HasPrefix(arg, "--delete=") {
			return true
		}
	}

	return false
}

func gitPushIsDestructive(args []string) bool {
	for _, arg := range args {
		arg = normalizedGitArg(arg)
		if arg == "--prune" || gitDeleteRefspec(arg) || gitForcePushRefspec(arg) || gitForcePushFlag(arg) {
			return true
		}
	}

	return gitArgsContainDeleteFlag(args)
}

func gitDeleteRefspec(arg string) bool {
	return strings.HasPrefix(arg, ":") || strings.HasPrefix(arg, "+:")
}

func gitForcePushRefspec(arg string) bool {
	return strings.HasPrefix(arg, "+") && !strings.HasPrefix(arg, "+:")
}

func gitForcePushFlag(arg string) bool {
	return arg == "-f" ||
		arg == "--force" ||
		strings.HasPrefix(arg, "--force=") ||
		arg == "--force-with-lease" ||
		strings.HasPrefix(arg, "--force-with-lease=")
}

func containsShellWriteRedirection(command string) bool {
	for _, part := range commandSegmentTexts(command) {
		fields := shellFields(part)
		for i, field := range fields {
			nextValue := ""
			if i+1 < len(fields) {
				nextValue = fields[i+1]
			}

			if _, writes := outputRedirectionValue(field, nextValue); writes {
				return true
			}
		}
	}

	return false
}

func credentialAssignment(field string) bool {
	name, value, ok := strings.Cut(strings.TrimSpace(field), "=")
	return ok && value != "" && credentialName(name)
}

func credentialFlag(field string) bool {
	field = strings.TrimSpace(field)
	if field == "" {
		return false
	}

	if strings.HasPrefix(field, "-") {
		if _, value, ok := strings.Cut(field, "="); ok {
			field = value
		}
	}

	field = strings.Trim(strings.TrimSpace(field), `"'{}$`)
	if field == "" {
		return false
	}

	normalized := strings.ReplaceAll(strings.ReplaceAll(strings.ToLower(field), "-", "_"), ".", "_")
	if credentialName(normalized) {
		return true
	}

	for _, marker := range []string{"token", "secret", "password", "passwd", "credential", "cookie", "private_key", "access_key", "api_key", "apikey", "authorization"} {
		if strings.Contains(normalized, marker) {
			return true
		}
	}

	return normalized == "auth" ||
		normalized == "pat" ||
		strings.Contains(normalized, "_auth_") ||
		strings.Contains(normalized, "_pat_") ||
		strings.HasPrefix(normalized, "auth_") ||
		strings.HasPrefix(normalized, "pat_") ||
		strings.HasSuffix(normalized, "_auth") ||
		strings.HasSuffix(normalized, "_pat")
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

// CredentialName reports whether name looks like it carries credential material.
func CredentialName(name string) bool {
	return credentialName(name)
}

func commandAccessesCredentialPath(name string, args []string) bool {
	if len(args) == 0 || !credentialPathCommand(name) {
		return false
	}

	return slices.ContainsFunc(args, credentialPathArgument)
}

func credentialPathCommand(name string) bool {
	switch name {
	case "cat", "grep", "rg", "awk", commandNameSed, "head", "tail", "wc", "stat", "test",
		"less", "more", "cp", "mv", commandNameChmod, commandNameChown, commandNameInstall, commandNamePatch,
		"source", ".":
		return true
	default:
		return false
	}
}

func credentialPathArgument(raw string) bool {
	normalized, base, ok := normalizedCredentialPathArgument(raw)
	if !ok {
		return false
	}

	return credentialPathBaseMatches(base) ||
		credentialPathLocationMatches(normalized, base) ||
		credentialPathSensitiveName(normalized, base)
}

func normalizedCredentialPathArgument(raw string) (normalized, base string, ok bool) {
	value := strings.Trim(strings.TrimSpace(raw), `"'`)
	if value == "" || value == "-" {
		return "", "", false
	}

	if strings.HasPrefix(value, "-") {
		_, flagValue, hasValue := strings.Cut(value, "=")
		if !hasValue || strings.TrimSpace(flagValue) == "" {
			return "", "", false
		}

		value = flagValue
	}

	value = strings.Trim(strings.TrimSpace(value), `"'`)
	if value == "" || value == "-" {
		return "", "", false
	}

	normalized = strings.ToLower(filepath.ToSlash(value))
	base = strings.ToLower(filepath.Base(normalized))

	return normalized, base, true
}

func credentialPathBaseMatches(base string) bool {
	switch {
	case base == ".env" || strings.HasPrefix(base, ".env."):
		return true
	case base == ".netrc" || base == ".npmrc" || base == ".pypirc":
		return true
	case strings.HasPrefix(base, "id_") && !strings.HasSuffix(base, ".pub"):
		return true
	default:
		return false
	}
}

func credentialPathLocationMatches(normalized, base string) bool {
	switch {
	case strings.HasSuffix(normalized, "/.aws/credentials") || strings.Contains(normalized, "/.aws/credentials."):
		return true
	case strings.HasSuffix(normalized, "/.claude/.credentials.json"):
		return true
	case strings.HasSuffix(normalized, "/.codex/auth.json"):
		return true
	case strings.HasSuffix(normalized, "/.docker/config.json"):
		return true
	case strings.HasSuffix(normalized, "/.kube/config"):
		return true
	case base == "credentials" && (strings.Contains(normalized, "/.aws/") || strings.Contains(normalized, "/.config/") || strings.Contains(normalized, "/gcloud/")):
		return true
	case base == "auth.json" && strings.Contains(normalized, "/.codex/"):
		return true
	default:
		return false
	}
}

func credentialPathSensitiveName(normalized, base string) bool {
	pathLike := strings.Contains(normalized, "/") || strings.HasPrefix(base, ".") || strings.Contains(base, ".")
	if !pathLike {
		return false
	}

	return strings.Contains(base, "secret") ||
		strings.Contains(base, "token") ||
		(strings.Contains(base, "private") && strings.Contains(base, "key"))
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

var auditWriteMu sync.Mutex

func appendAuditRecord(ctx context.Context, decision Decision) {
	if len(decision.Operations) == 0 {
		return
	}

	record := auditRecord(decision)

	line, marshalErr := json.Marshal(record)
	if marshalErr != nil {
		return
	}

	dir := auditDir(ctx)
	if mkdirErr := os.MkdirAll(dir, 0o700); mkdirErr != nil {
		return
	}

	auditWriteMu.Lock()
	defer auditWriteMu.Unlock()

	file, err := os.OpenFile(filepath.Join(dir, sideEffectLedgerFileName), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer file.Close()

	if _, err := file.Write(append(line, '\n')); err != nil {
		return
	}
}

func auditRecord(decision Decision) AuditRecord {
	return AuditRecord{
		Timestamp:       time.Now().UTC(),
		Operations:      append([]Operation(nil), decision.Operations...),
		OperationKinds:  operationKindStrings(decision.Operations),
		Action:          firstOperationField(decision.Operations, func(op Operation) string { return op.Action }),
		Source:          firstOperationField(decision.Operations, func(op Operation) string { return op.Source }),
		Target:          firstOperationField(decision.Operations, func(op Operation) string { return op.Target }),
		SessionID:       firstOperationMetadata(decision.Operations, metadataSessionID),
		SessionPath:     firstOperationMetadata(decision.Operations, metadataSessionPath),
		IssueID:         firstOperationMetadata(decision.Operations, metadataIssueID),
		IssueIdentifier: firstOperationMetadata(decision.Operations, metadataIssueIdentifier),
		Agent:           firstOperationMetadata(decision.Operations, metadataAgent),
		Model:           firstOperationMetadata(decision.Operations, metadataModel),
		Decision:        auditDecisionString(decision.Allowed),
		Policy:          decision.Policy,
		Mode:            decision.Mode,
		Kind:            decision.Kind,
		Rule:            decision.Rule,
		Reason:          decision.Reason,
		NeedsApproval:   decision.NeedsApproval,
		Confirmed:       decision.Confirmed,
	}
}

func operationKindStrings(ops []Operation) []string {
	if len(ops) == 0 {
		return nil
	}

	seen := make(map[OperationKind]bool)
	out := make([]string, 0, len(operationKindOrder))

	for _, op := range ops {
		if op.Kind != "" && !seen[op.Kind] {
			seen[op.Kind] = true
			out = append(out, string(op.Kind))
		}
	}

	return out
}

func firstOperationField(ops []Operation, value func(Operation) string) string {
	for _, op := range ops {
		if out := strings.TrimSpace(value(op)); out != "" {
			return out
		}
	}

	return ""
}

func firstOperationMetadata(ops []Operation, key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}

	for _, op := range ops {
		if value := strings.TrimSpace(op.Metadata[key]); value != "" {
			return value
		}
	}

	return ""
}

func auditDecisionString(allowed bool) string {
	if allowed {
		return "allowed"
	}

	return "denied"
}

func auditDir(ctx context.Context) string {
	if ctx != nil {
		if dir, ok := ctx.Value(auditDirContextKey{}).(string); ok {
			if dir = strings.TrimSpace(dir); dir != "" {
				return dir
			}
		}
	}

	if dir := strings.TrimSpace(os.Getenv(EnvAuditDir)); dir != "" {
		return dir
	}

	return filepath.Join(os.TempDir(), "atteler", "audit")
}
