package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tommoulard/atteler/pkg/permission"
	"github.com/tommoulard/atteler/pkg/session"
)

const (
	permissionModeAllow    = "allow"
	permissionModeAsk      = "ask"
	permissionModeDeny     = "deny"
	permissionModeReadOnly = "read-only"
)

func permissionPolicyFromOptions(opts cliOptions) (*permission.Policy, error) {
	mode := strings.ToLower(strings.TrimSpace(opts.permissionMode))

	var policy permission.Policy

	switch mode {
	case "", permissionModeAllow:
		policy = permission.DefaultPolicy()
		policy.Name = permissionModeAllow
	case permissionModeAsk:
		policy = permission.DefaultPolicy()
		policy.Name = permissionModeAsk
		policy.Default = permission.ModeAsk
		policy.SetMode(permission.OperationRead, permission.ModeAllow)
	case permissionModeDeny:
		policy = permission.DefaultPolicy()
		policy.Name = permissionModeDeny
		policy.Default = permission.ModeDeny
	case permissionModeReadOnly, "readonly":
		policy = permission.ReadOnlyPolicy()
	default:
		return nil, fmt.Errorf("unsupported --permission-mode %q (supported: allow, ask, deny, read-only)", opts.permissionMode)
	}

	allowed, err := permission.ParseOperationKinds([]string(opts.allowOperations))
	if err != nil {
		return nil, fmt.Errorf("parse --allow-operation: %w", err)
	}

	denied, err := permission.ParseOperationKinds([]string(opts.denyOperations))
	if err != nil {
		return nil, fmt.Errorf("parse --deny-operation: %w", err)
	}

	for _, kind := range allowed {
		policy.SetMode(kind, permission.ModeAllow)
	}

	for _, kind := range denied {
		policy.SetMode(kind, permission.ModeDeny)

		if kind == permission.OperationExecute {
			policy.AllowReadExecution = false
		}
	}

	return &policy, nil
}

func permissionPolicySummary(policy *permission.Policy) string {
	if policy == nil {
		return permission.DefaultPolicy().Summary()
	}

	return policy.Summary()
}

func permissionPolicyCommandArgs(policy *permission.Policy) []string {
	if policy == nil {
		return nil
	}

	mode := permissionPolicyCommandMode(policy)

	baseline, err := permissionPolicyFromOptions(cliOptions{permissionMode: mode})
	if err != nil {
		return nil
	}

	args := []string{"--permission-mode", mode}

	for _, kind := range permission.KnownOperationKinds() {
		policyMode := policy.ModeFor(kind)
		if policyMode == baseline.ModeFor(kind) {
			continue
		}

		switch policyMode {
		case permission.ModeAllow:
			args = append(args, "--allow-operation", string(kind))
		case permission.ModeDeny:
			args = append(args, "--deny-operation", string(kind))
		}
	}

	if !policy.AllowReadExecution && baseline.AllowReadExecution && policy.ModeFor(permission.OperationExecute) == baseline.ModeFor(permission.OperationExecute) {
		args = append(args, "--deny-operation", string(permission.OperationExecute))
	}

	return args
}

func permissionPolicyCommandMode(policy *permission.Policy) string {
	if policy == nil {
		return "allow"
	}

	switch strings.ToLower(strings.TrimSpace(policy.Name)) {
	case permissionModeAsk:
		return permissionModeAsk
	case permissionModeDeny:
		return permissionModeDeny
	case "read-only", "readonly":
		return permissionModeReadOnly
	case permissionModeAllow, "default":
		return permissionModeAllow
	}

	switch policy.Default {
	case permission.ModeAsk:
		return permissionModeAsk
	case permission.ModeDeny:
		return permissionModeDeny
	default:
		return permissionModeAllow
	}
}

func contextWithPermissionPolicyForOptions(ctx context.Context, opts cliOptions, policy *permission.Policy) context.Context {
	ctx = permission.ContextWithPolicy(ctx, policy)
	if policy == nil || opts.headless {
		return ctx
	}

	return permission.ContextWithConfirmer(ctx, confirmPermissionStdin)
}

func contextWithPermissionPolicyFromOptions(ctx context.Context, opts cliOptions) (context.Context, *permission.Policy, error) {
	if !permissionOptionsSpecified(opts) {
		if policy := permission.PolicyFromContext(ctx); policy != nil {
			return ctx, policy, nil
		}
	}

	policy, err := permissionPolicyFromOptions(opts)
	if err != nil {
		return ctx, nil, err
	}

	return contextWithPermissionPolicyForOptions(ctx, opts, policy), policy, nil
}

func permissionOptionsSpecified(opts cliOptions) bool {
	return strings.TrimSpace(opts.permissionMode) != "" || len(opts.allowOperations) > 0 || len(opts.denyOperations) > 0
}

func contextWithPermissionAuditMetadata(ctx context.Context, store *session.Store, sessionState session.Session, agentName, modelName string) context.Context {
	metadata := map[string]string{
		"session_id": sessionState.ID,
		"agent":      agentName,
		"model":      modelName,
	}

	if store != nil && strings.TrimSpace(sessionState.ID) != "" {
		sessionPath := store.Path(sessionState.ID)

		metadata["session_path"] = sessionPath
		if strings.TrimSpace(os.Getenv(permission.EnvAuditDir)) == "" {
			ctx = permission.ContextWithAuditDir(ctx, permissionAuditDirForSessionPath(sessionPath, sessionState.ID))
		}
	}

	return permission.ContextWithAuditMetadata(ctx, metadata)
}

func permissionAuditDirForSessionPath(sessionPath, sessionID string) string {
	sessionPath = strings.TrimSpace(sessionPath)
	if sessionPath == "" {
		return ""
	}

	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return filepath.Join(filepath.Dir(sessionPath), "audit")
	}

	return filepath.Join(filepath.Dir(sessionPath), "audit", sessionID)
}

func authorizeWritePermission(ctx context.Context, action, source, target string) error {
	return authorizePermissionOperation(ctx, action, source, target, permission.OperationWrite)
}

func authorizeReadPermission(ctx context.Context, action, source, target string) error {
	return authorizePermissionOperation(ctx, action, source, target, permission.OperationRead)
}

func authorizePermissionOperation(ctx context.Context, action, source, target string, kind permission.OperationKind) error {
	decision := permission.Evaluate(ctx, nil, permission.Request{
		Action: action,
		Source: source,
		Target: target,
		Operations: []permission.Operation{{
			Kind:   kind,
			Action: action,
			Source: source,
			Target: target,
		}},
	})
	if decision.Allowed {
		return nil
	}

	return &permission.Error{Decision: decision}
}
