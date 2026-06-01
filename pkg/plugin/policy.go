package plugin

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

const acceptedAuditAction = "accepted"

var secretAssignmentPattern = regexp.MustCompile(`(?i)\b(api[_-]?key|password|secret|token)=[^\s[]+`)

// Policy describes the local permissions and provenance an atteler process has
// accepted for a plugin run.
//
//nolint:govet // Field order mirrors manifest security concepts.
type Policy struct {
	Permissions           PermissionSet `json:"permissions" yaml:"permissions"`
	Output                OutputLimits  `json:"output" yaml:"output"`
	TrustedInstallSources []string      `json:"trusted_install_sources,omitempty" yaml:"trusted_install_sources,omitempty"`
	RequireSignature      bool          `json:"require_signature,omitempty" yaml:"require_signature,omitempty"`
}

// AcceptManifestPolicy creates a policy that accepts exactly the permissions
// and output limits declared by manifest. Callers that have a stricter local
// policy should pass that policy through RunOptions instead.
func AcceptManifestPolicy(manifest Manifest) Policy {
	policy := Policy{}
	if manifest.Permissions != nil {
		policy.Permissions = clonePermissionSet(*manifest.Permissions)
	}

	if manifest.Output != nil {
		policy.Output = *manifest.Output
	}

	if manifest.Trust != nil && strings.TrimSpace(manifest.Trust.InstallSource) != "" {
		policy.TrustedInstallSources = []string{manifest.Trust.InstallSource}
	}

	return policy
}

// ClonePolicy returns a deep copy of policy so callers can store accepted
// policy independently from mutable manifest/config data.
func ClonePolicy(policy Policy) Policy {
	out := policy
	out.Permissions = clonePermissionSet(policy.Permissions)
	out.TrustedInstallSources = append([]string(nil), policy.TrustedInstallSources...)

	return out
}

func clonePermissionSet(in PermissionSet) PermissionSet {
	out := in
	out.Filesystem.Read = append([]string(nil), in.Filesystem.Read...)
	out.Filesystem.Write = append([]string(nil), in.Filesystem.Write...)
	out.Network.Hosts = append([]string(nil), in.Network.Hosts...)
	out.Env = append([]string(nil), in.Env...)
	out.Secrets = append([]string(nil), in.Secrets...)
	out.Tools = append([]string(nil), in.Tools...)

	return out
}

func authorizeRun(root string, manifest Manifest, entrypointName string, policy Policy, attelerVersion string) error {
	if manifest.Permissions == nil {
		return errors.New("permissions must be declared before running plugin")
	}

	if manifest.Output == nil {
		return errors.New("output limits must be declared before running plugin")
	}

	if manifest.Trust == nil {
		return errors.New("trust provenance must be declared before running plugin")
	}

	if strings.TrimSpace(manifest.MinimumAttelerVersion) == "" {
		return errors.New("min_atteler_version must be declared before running plugin")
	}

	if _, ok := entrypointArgsFor(manifest, entrypointName); !ok {
		return fmt.Errorf("entrypoint %q args must be declared before running plugin", entrypointName)
	}

	if _, ok := entrypointOutputContractFor(manifest, entrypointName); !ok {
		return fmt.Errorf("entrypoint %q output contract must be declared before running plugin", entrypointName)
	}

	if err := authorizeCompatibility(manifest.MinimumAttelerVersion, attelerVersion); err != nil {
		return fmt.Errorf("compatibility policy: %w", err)
	}

	if err := validatePermissions(root, policy.Permissions); err != nil {
		return fmt.Errorf("policy permissions: %w", err)
	}

	if err := validateOutputLimits(policy.Output); err != nil {
		return fmt.Errorf("policy output: %w", err)
	}

	if err := authorizeTrust(*manifest.Trust, policy); err != nil {
		return fmt.Errorf("trust policy: %w", err)
	}

	if err := authorizePermissions(root, *manifest.Permissions, policy.Permissions); err != nil {
		return fmt.Errorf("permissions policy: %w", err)
	}

	if err := authorizeOutput(*manifest.Output, policy.Output); err != nil {
		return fmt.Errorf("output policy: %w", err)
	}

	entrypointPath := manifest.Entrypoints[entrypointName]
	if !scopesCover(root, manifest.Permissions.Filesystem.Read, entrypointPath) {
		return fmt.Errorf("permissions policy: filesystem.read does not include entrypoint %q", entrypointPath)
	}

	return nil
}

func entrypointArgsFor(manifest Manifest, entrypointName string) ([]ArgumentSpec, bool) {
	if contract, ok := manifest.EntrypointContracts[entrypointName]; ok && contract.Inputs.Args != nil {
		return contract.Inputs.Args, true
	}

	args, ok := manifest.EntrypointArgs[entrypointName]

	return args, ok
}

func entrypointOutputContractFor(manifest Manifest, entrypointName string) (StructuredOutputContract, bool) {
	contract, ok := manifest.EntrypointContracts[entrypointName]
	if !ok || contract.Output == nil {
		return StructuredOutputContract{}, false
	}

	return *contract.Output, true
}

func authorizeCompatibility(minimumAttelerVersion, attelerVersion string) error {
	minimumAttelerVersion = strings.TrimSpace(minimumAttelerVersion)
	if minimumAttelerVersion == "" {
		return nil
	}

	attelerVersion = strings.TrimSpace(attelerVersion)
	if attelerVersion == "" || strings.EqualFold(attelerVersion, "dev") {
		return nil
	}

	cmp, ok := compareVersionCore(attelerVersion, minimumAttelerVersion)
	if !ok {
		return fmt.Errorf("atteler version %q cannot be compared with minimum %q", attelerVersion, minimumAttelerVersion)
	}

	if cmp < 0 {
		return fmt.Errorf("plugin requires atteler >= %s; current version is %s", minimumAttelerVersion, attelerVersion)
	}

	return nil
}

func compareVersionCore(left, right string) (int, bool) {
	leftParts, ok := parseVersionCore(left)
	if !ok {
		return 0, false
	}

	rightParts, ok := parseVersionCore(right)
	if !ok {
		return 0, false
	}

	for i := range leftParts {
		switch {
		case leftParts[i] > rightParts[i]:
			return 1, true
		case leftParts[i] < rightParts[i]:
			return -1, true
		}
	}

	return 0, true
}

func parseVersionCore(version string) ([3]int, bool) {
	var out [3]int

	version = strings.TrimPrefix(strings.TrimSpace(version), "v")
	version, _, _ = strings.Cut(version, "-")

	version, _, _ = strings.Cut(version, "+")
	if version == "" {
		return out, false
	}

	parts := strings.Split(version, ".")
	if len(parts) > len(out) {
		parts = parts[:len(out)]
	}

	for i, part := range parts {
		if part == "" {
			return out, false
		}

		n, err := strconv.Atoi(part)
		if err != nil {
			return out, false
		}

		out[i] = n
	}

	return out, true
}

func authorizeTrust(trust Trust, policy Policy) error {
	if !trust.Enabled {
		return errors.New("plugin is disabled")
	}

	if trust.Revoked {
		return errors.New("plugin trust is revoked")
	}

	if strings.TrimSpace(trust.InstallSource) == "" {
		return errors.New("install_source is required")
	}

	if strings.TrimSpace(trust.Checksum) == "" && strings.TrimSpace(trust.Signature) == "" {
		return errors.New("checksum or signature is required")
	}

	if policy.RequireSignature && strings.TrimSpace(trust.Signature) == "" {
		return errors.New("signature is required by policy")
	}

	if len(policy.TrustedInstallSources) == 0 {
		return errors.New("trusted_install_sources must include install_source")
	}

	if !slices.Contains(policy.TrustedInstallSources, trust.InstallSource) {
		return fmt.Errorf("install_source %q is not trusted by policy", trust.InstallSource)
	}

	if !hasTrustAuditAction(trust.Audit, acceptedAuditAction) {
		return errors.New("accepted audit event is required")
	}

	return nil
}

func hasTrustAuditAction(audit []TrustAudit, action string) bool {
	for _, event := range audit {
		if strings.EqualFold(strings.TrimSpace(event.Action), action) {
			return true
		}
	}

	return false
}

func authorizePermissions(root string, requested, accepted PermissionSet) error {
	if err := authorizeScopes(root, "filesystem.read", requested.Filesystem.Read, accepted.Filesystem.Read); err != nil {
		return err
	}

	if err := authorizeScopes(root, "filesystem.write", requested.Filesystem.Write, accepted.Filesystem.Write); err != nil {
		return err
	}

	if requested.Network.Allow {
		if !accepted.Network.Allow {
			return errors.New("network access was not accepted")
		}

		if !stringSetAllows(requested.Network.Hosts, accepted.Network.Hosts, true) {
			return errors.New("network hosts exceed accepted policy")
		}
	}

	if requested.Shell.Allow && !accepted.Shell.Allow {
		return errors.New("shell access was not accepted")
	}

	if !stringSetAllows(requested.Env, accepted.Env, false) {
		return errors.New("environment variables exceed accepted policy")
	}

	if !stringSetAllows(requested.Secrets, accepted.Secrets, false) {
		return errors.New("secret variables exceed accepted policy")
	}

	if !stringSetAllows(requested.Tools, accepted.Tools, false) {
		return errors.New("tool capabilities exceed accepted policy")
	}

	return nil
}

func authorizeScopes(root, field string, requested, accepted []string) error {
	for _, scope := range requested {
		if !scopesCover(root, accepted, scope) {
			return fmt.Errorf("%s scope %q was not accepted", field, scope)
		}
	}

	return nil
}

func authorizeOutput(requested, accepted OutputLimits) error {
	if requested.StdoutMaxBytes > accepted.StdoutMaxBytes {
		return fmt.Errorf("stdout_max_bytes %d exceeds accepted %d", requested.StdoutMaxBytes, accepted.StdoutMaxBytes)
	}

	if requested.StderrMaxBytes > accepted.StderrMaxBytes {
		return fmt.Errorf("stderr_max_bytes %d exceeds accepted %d", requested.StderrMaxBytes, accepted.StderrMaxBytes)
	}

	return nil
}

func scopesCover(root string, scopes []string, candidate string) bool {
	for _, scope := range scopes {
		ok, err := scopeCovers(root, scope, candidate)
		if err == nil && ok {
			return true
		}
	}

	return false
}

func scopeCovers(root, scope, candidate string) (bool, error) {
	scope = strings.TrimSpace(scope)
	candidate = strings.TrimSpace(candidate)

	if scope == "" || candidate == "" {
		return false, nil
	}

	if filepath.IsAbs(scope) || filepath.IsAbs(candidate) {
		return false, nil
	}

	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return false, fmt.Errorf("resolve plugin root: %w", err)
	}

	scopeAbs, err := filepath.Abs(filepath.Join(rootAbs, scope))
	if err != nil {
		return false, fmt.Errorf("resolve scope: %w", err)
	}

	candidateAbs, err := filepath.Abs(filepath.Join(rootAbs, candidate))
	if err != nil {
		return false, fmt.Errorf("resolve candidate: %w", err)
	}

	rel, err := filepath.Rel(scopeAbs, candidateAbs)
	if err != nil {
		return false, fmt.Errorf("compare scope: %w", err)
	}

	return rel == "." || (!strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != ".." && !filepath.IsAbs(rel)), nil
}

func stringSetAllows(requested, accepted []string, allowWildcard bool) bool {
	if allowWildcard && slices.Contains(accepted, "*") {
		return true
	}

	for _, value := range requested {
		if !slices.Contains(accepted, value) {
			return false
		}
	}

	return true
}

type secretValue struct {
	name  string
	value string
}

type outputRedactor struct {
	secrets []secretValue
}

func (r outputRedactor) Redact(output string) string {
	redacted := output
	for _, secret := range r.secrets {
		redacted = redactSecretValue(redacted, secret)
	}

	return secretAssignmentPattern.ReplaceAllString(redacted, "$1=[REDACTED]")
}

func redactSecretValue(output string, secret secretValue) string {
	value := secret.value
	if len(value) < 4 {
		return output
	}

	redaction := "[REDACTED:" + secret.name + "]"

	redacted := strings.ReplaceAll(output, value, redaction)
	for prefixLen := min(len(value)-1, len(output)); prefixLen >= 4; prefixLen-- {
		redacted = strings.ReplaceAll(redacted, value[:prefixLen], redaction)
	}

	return redacted
}
