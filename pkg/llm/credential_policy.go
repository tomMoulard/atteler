package llm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tommoulard/atteler/pkg/permission"
)

// Credential store identifiers used by credential-source policy.
const (
	CredentialStoreEnv                = "env"
	CredentialStoreForgeCredentials   = "forge_credentials" // #nosec G101 -- credential store label, not a credential value.
	CredentialStoreClaudeCodeKeychain = "claude_code_keychain"
	CredentialStoreClaudeCodeFile     = "claude_code_file"
	CredentialStoreCodexAuthJSON      = "codex_auth_json" // #nosec G101 -- credential store label, not a credential value.
)

// keychainService is the macOS Keychain "service" attribute under which the
// claude CLI stores its OAuth credentials.
const keychainService = "Claude Code-credentials" // #nosec G101 -- keychain service label, not a credential value.

const (
	credentialActionUse       = "use"
	credentialActionRead      = "read"
	credentialActionRefresh   = "refresh"
	credentialActionWriteBack = "write_back"
)

const (
	envCredentialAllowedProviders = "ATTELER_CREDENTIAL_ALLOWED_PROVIDERS" // #nosec G101 -- environment variable name, not a credential value.
	envCredentialAllowedStores    = "ATTELER_CREDENTIAL_ALLOWED_STORES"    // #nosec G101 -- environment variable name, not a credential value.
	envTrustBorrowedCredentials   = "ATTELER_TRUST_BORROWED_CREDENTIALS"   // #nosec G101 -- environment variable name, not a credential value.
	envAllowBorrowedOAuth         = "ATTELER_ALLOW_BORROWED_OAUTH"
	envAllowCredentialRefresh     = "ATTELER_ALLOW_CREDENTIAL_REFRESH"    // #nosec G101 -- environment variable name, not a credential value.
	envAllowCredentialWriteBack   = "ATTELER_ALLOW_CREDENTIAL_WRITE_BACK" // #nosec G101 -- environment variable name, not a credential value.

	credentialAuditLedgerFileName = "credential_events.jsonl"  // #nosec G101 -- audit file name, not a credential value.
	credentialAuditEventDenied    = "credential.policy_denied" // #nosec G101 -- audit event label, not a credential value.
	credentialAuditEventRefresh   = "credential.refresh"       // #nosec G101 -- audit event label, not a credential value.
	credentialAuditEventWriteBack = "credential.write_back"    // #nosec G101 -- audit event label, not a credential value.
	credentialAuditEventCAS       = "credential.cas_conflict"  // #nosec G101 -- audit event label, not a credential value.
)

// claudeCodeKeychainSource is the diagnostic location string used when
// credentials originate from the macOS keychain.
const claudeCodeKeychainSource = "keychain:" + keychainService

// CredentialSourcePolicy declares which credential sources Atteler may use and
// whether borrowed OAuth sessions may be refreshed or written back to their
// owning CLI's credential store.
//
// When a provider has no explicit credential_policy configured,
// defaultCredentialSourcePolicy applies: Atteler borrows the well-known local
// credential stores out of the box (see defaultAllowedCredentialStores) and may
// refresh/write them back. Unknown stores and the "*" wildcard are still not
// auto-allowed. The struct zero value, by contrast, is intentionally
// conservative — it is what an explicitly configured policy that omits fields
// denies: borrowed OAuth, refresh, and write-back are all off.
type CredentialSourcePolicy struct {
	AllowedProviders    []string `json:"allowed_providers,omitempty" yaml:"allowed_providers,omitempty"`
	AllowedStores       []string `json:"allowed_stores,omitempty" yaml:"allowed_stores,omitempty"`
	Configured          bool     `json:"-" yaml:"-"`
	AllowedProvidersSet bool     `json:"-" yaml:"-"`
	AllowedStoresSet    bool     `json:"-" yaml:"-"`
	AllowBorrowedOAuth  bool     `json:"allow_borrowed_oauth,omitempty" yaml:"allow_borrowed_oauth,omitempty"`
	AllowRefresh        bool     `json:"allow_refresh,omitempty" yaml:"allow_refresh,omitempty"`
	AllowWriteBack      bool     `json:"allow_write_back,omitempty" yaml:"allow_write_back,omitempty"`
}

type credentialSourcePolicyContextKey struct{}

// ContextWithCredentialSourcePolicy stores a credential-source policy in ctx
// for credential helpers that are not constructed from ProviderConfig.
func ContextWithCredentialSourcePolicy(ctx context.Context, policy CredentialSourcePolicy) context.Context {
	if ctx == nil {
		return nil
	}

	return context.WithValue(ctx, credentialSourcePolicyContextKey{}, policy)
}

func credentialSourcePolicyFromContext(ctx context.Context) (CredentialSourcePolicy, bool) {
	if ctx == nil {
		return CredentialSourcePolicy{}, false
	}

	policy, ok := ctx.Value(credentialSourcePolicyContextKey{}).(CredentialSourcePolicy)

	return policy, ok
}

type credentialSource struct {
	Provider      string
	Store         string
	Description   string
	Location      string
	Identifier    string
	BorrowedOAuth bool
}

type normalizedCredentialSourcePolicy struct {
	allowedProviders   map[string]bool
	allowedStores      map[string]bool
	restrictProviders  bool
	allowAllProviders  bool
	allowAllStores     bool
	allowBorrowedOAuth bool
	allowRefresh       bool
	allowWriteBack     bool
}

// CredentialSourcePolicyError is returned when credential-source policy blocks
// a credential read, use, refresh, or write-back.
type CredentialSourcePolicyError struct {
	Provider string
	Store    string
	Action   string
	Location string
	Reason   string
}

func (e *CredentialSourcePolicyError) Error() string {
	if e == nil {
		return ""
	}

	parts := []string{
		"credential source policy denied",
		"provider=" + e.Provider,
		"store=" + e.Store,
		"action=" + e.Action,
	}
	if e.Location != "" {
		parts = append(parts, "location="+redactCredentialLocation(e.Location))
	}

	message := strings.Join(parts, " ")
	if e.Reason != "" {
		message += ": " + e.Reason
	}

	return message
}

func isBlockingCredentialSourcePolicyError(err error) bool {
	var policyErr *CredentialSourcePolicyError
	if !errors.As(err, &policyErr) {
		return false
	}

	return policyErr.Action != credentialActionRead
}

func containsCredentialSourcePolicyError(err error) bool {
	var policyErr *CredentialSourcePolicyError

	return errors.As(err, &policyErr)
}

// defaultAllowedCredentialStores are the well-known local credential stores
// Atteler borrows from sibling CLIs out of the box when no credential_policy is
// configured. The "*" wildcard and any store not listed here still require an
// explicit opt-in (config credential_policy or ATTELER_TRUST_BORROWED_CREDENTIALS).
func defaultAllowedCredentialStores() []string {
	return []string{
		CredentialStoreEnv,
		CredentialStoreCodexAuthJSON,
		CredentialStoreClaudeCodeKeychain,
		CredentialStoreClaudeCodeFile,
		CredentialStoreForgeCredentials,
	}
}

func defaultCredentialSourcePolicy() CredentialSourcePolicy {
	// By default Atteler reuses the local CLI subscriptions it can find: it may
	// read, use, refresh, and write back the known borrowed stores. Lock this
	// down with an explicit providers.<name>.credential_policy (for example
	// allowed_stores: [env]) or disable_private_adapter.
	policy := CredentialSourcePolicy{
		AllowedStores:      defaultAllowedCredentialStores(),
		AllowBorrowedOAuth: true,
		AllowRefresh:       true,
		AllowWriteBack:     true,
	}

	if providers := splitCredentialPolicyList(os.Getenv(envCredentialAllowedProviders)); len(providers) > 0 {
		policy.AllowedProviders = providers
	}

	if stores := splitCredentialPolicyList(os.Getenv(envCredentialAllowedStores)); len(stores) > 0 {
		policy.AllowedStores = stores
	}

	if envBool(envTrustBorrowedCredentials) {
		policy.AllowedStores = []string{"*"}
		policy.AllowBorrowedOAuth = true
		policy.AllowRefresh = true
		policy.AllowWriteBack = true
	}

	if envBool(envAllowBorrowedOAuth) {
		policy.AllowBorrowedOAuth = true
	}

	if envBool(envAllowCredentialRefresh) {
		policy.AllowRefresh = true
	}

	if envBool(envAllowCredentialWriteBack) {
		policy.AllowWriteBack = true
	}

	return policy
}

func credentialPolicyForProvider(ctx context.Context, cfg ProviderConfig) CredentialSourcePolicy {
	policy := defaultCredentialSourcePolicy()

	if contextPolicy, ok := credentialSourcePolicyFromContext(ctx); ok {
		policy = contextPolicy
	}

	if !cfg.CredentialPolicy.empty() {
		policy = cfg.CredentialPolicy
	}

	if len(policy.AllowedStores) == 0 && !policy.AllowedStoresSet && policy.AllowedStores == nil {
		policy.AllowedStores = defaultAllowedCredentialStores()
	}

	return policy
}

func (p CredentialSourcePolicy) empty() bool {
	return !p.Configured &&
		!p.AllowedProvidersSet &&
		!p.AllowedStoresSet &&
		len(p.AllowedProviders) == 0 &&
		len(p.AllowedStores) == 0 &&
		!p.AllowBorrowedOAuth &&
		!p.AllowRefresh &&
		!p.AllowWriteBack
}

func permissiveCredentialSourcePolicy() CredentialSourcePolicy {
	return CredentialSourcePolicy{
		AllowedStores:      []string{"*"},
		Configured:         true,
		AllowedStoresSet:   true,
		AllowBorrowedOAuth: true,
		AllowRefresh:       true,
		AllowWriteBack:     true,
	}
}

func normalizeCredentialSourcePolicy(policy CredentialSourcePolicy) normalizedCredentialSourcePolicy {
	out := normalizedCredentialSourcePolicy{
		allowedProviders:   make(map[string]bool),
		allowedStores:      make(map[string]bool),
		restrictProviders:  policy.AllowedProvidersSet || policy.AllowedProviders != nil,
		allowBorrowedOAuth: policy.AllowBorrowedOAuth,
		allowRefresh:       policy.AllowRefresh,
		allowWriteBack:     policy.AllowWriteBack,
	}

	for _, provider := range policy.AllowedProviders {
		value := normalizeCredentialPolicyValue(provider)
		if value == "" {
			continue
		}

		if value == "*" {
			out.allowAllProviders = true
			continue
		}

		out.allowedProviders[value] = true
	}

	stores := policy.AllowedStores
	if len(stores) == 0 && !policy.AllowedStoresSet && stores == nil {
		stores = defaultAllowedCredentialStores()
	}

	for _, store := range stores {
		value := normalizeCredentialPolicyValue(store)
		if value == "" {
			continue
		}

		if value == "*" {
			out.allowAllStores = true
			continue
		}

		out.allowedStores[value] = true
	}

	return out
}

func normalizeCredentialPolicyValue(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, "-", "_")

	return value
}

//nolint:wsl_v5 // Compact splitter keeps list parsing local and readable.
func splitCredentialPolicyList(raw string) []string {
	var out []string
	for _, part := range strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	}) {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}

	return out
}

//nolint:wsl_v5 // Policy normalization and denial closure are easier to audit in one block.
func authorizeCredentialSourcePolicy(ctx context.Context, cfg ProviderConfig, source credentialSource, action string) error {
	policy := normalizeCredentialSourcePolicy(credentialPolicyForProvider(ctx, cfg))
	source.Provider = normalizeProviderName(source.Provider)
	source.Store = normalizeCredentialPolicyValue(source.Store)
	action = strings.TrimSpace(action)
	if action == "" {
		action = credentialActionUse
	}

	deny := func(reason string) error {
		err := &CredentialSourcePolicyError{
			Provider: source.Provider,
			Store:    source.Store,
			Action:   action,
			Location: source.Location,
			Reason:   reason,
		}
		auditCredentialEvent(ctx, credentialAuditEvent{
			Event:         credentialAuditEventDenied,
			Provider:      source.Provider,
			Store:         source.Store,
			Action:        action,
			Source:        source.Description,
			Location:      source.Location,
			Identifier:    source.Identifier,
			BorrowedOAuth: source.BorrowedOAuth,
			Decision:      "denied",
			Reason:        reason,
		})

		return err
	}

	if source.Provider == "" {
		return deny("credential source is missing a provider")
	}

	if source.Store == "" {
		return deny("credential source is missing a store")
	}

	if !policy.allowAllProviders && policy.restrictProviders && !policy.allowedProviders[source.Provider] {
		return deny("provider is not in credential_policy.allowed_providers")
	}

	if !policy.allowAllStores && !policy.allowedStores[source.Store] {
		return deny("store is not in credential_policy.allowed_stores")
	}

	if source.BorrowedOAuth && !policy.allowBorrowedOAuth {
		return deny("borrowed OAuth sessions require credential_policy.allow_borrowed_oauth")
	}

	switch action {
	case credentialActionRefresh:
		if !policy.allowRefresh {
			return deny("OAuth refresh requires credential_policy.allow_refresh")
		}
	case credentialActionWriteBack:
		if !policy.allowWriteBack {
			return deny("credential write-back requires credential_policy.allow_write_back")
		}
	}

	return nil
}

func normalizeProviderName(provider string) string {
	return normalizeCredentialPolicyValue(provider)
}

type credentialProvenance struct {
	Provider      string
	Store         string
	Location      string
	Identifier    string
	BorrowedOAuth bool
}

//nolint:wsl_v5 // Compact rendering avoids spreading simple provenance formatting.
func (p credentialProvenance) detail() string {
	var parts []string
	if p.Store != "" {
		parts = append(parts, "store="+p.Store)
	}
	if p.Location != "" {
		parts = append(parts, "location="+redactCredentialLocation(p.Location))
	}
	if p.Identifier != "" {
		parts = append(parts, "identifier="+redactCredentialIdentifier(p.Identifier))
	}
	if p.BorrowedOAuth {
		parts = append(parts, "borrowed_oauth=true")
	}
	if len(parts) == 0 {
		return "credential source unavailable"
	}

	return strings.Join(parts, " ")
}

func credentialSourceFromProvenance(p credentialProvenance, description string) credentialSource {
	return credentialSource{
		Provider:      p.Provider,
		Store:         p.Store,
		Description:   description,
		Location:      p.Location,
		Identifier:    p.Identifier,
		BorrowedOAuth: p.BorrowedOAuth,
	}
}

func redactCredentialLocation(location string) string {
	location = strings.TrimSpace(location)
	if location == "" {
		return ""
	}

	if strings.HasPrefix(location, "keychain:") {
		return redactProviderErrorMessage(location)
	}

	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if location == home {
			return "~"
		}

		if rel, err := filepath.Rel(home, location); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
			return redactProviderErrorMessage(filepath.Join("~", rel))
		}
	}

	return redactProviderErrorMessage(location)
}

func redactCredentialPathError(err error) string {
	if err == nil {
		return ""
	}

	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		op := strings.TrimSpace(pathErr.Op)
		if op == "" {
			op = "file"
		}

		return fmt.Sprintf("%s %s: %v", op, redactCredentialLocation(pathErr.Path), pathErr.Err)
	}

	var linkErr *os.LinkError
	if errors.As(err, &linkErr) {
		op := strings.TrimSpace(linkErr.Op)
		if op == "" {
			op = "link"
		}

		oldPath := redactCredentialLocation(linkErr.Old)
		newPath := redactCredentialLocation(linkErr.New)

		if oldPath != "" && newPath != "" {
			return fmt.Sprintf("%s %s -> %s: %v", op, oldPath, newPath, linkErr.Err)
		}

		return fmt.Sprintf("%s %s%s: %v", op, oldPath, newPath, linkErr.Err)
	}

	return redactProviderErrorMessage(err.Error())
}

func redactCredentialIdentifier(identifier string) string {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return ""
	}

	sum := sha256.Sum256([]byte(identifier))

	return "sha256:" + hex.EncodeToString(sum[:])[:12]
}

//nolint:govet // JSON field order follows audit-reading order.
type credentialAuditEvent struct {
	Timestamp     time.Time `json:"timestamp,omitzero"`
	Event         string    `json:"event"`
	Provider      string    `json:"provider,omitempty"`
	Store         string    `json:"store,omitempty"`
	Action        string    `json:"action,omitempty"`
	Source        string    `json:"source,omitempty"`
	Location      string    `json:"location,omitempty"`
	Identifier    string    `json:"identifier,omitempty"`
	BorrowedOAuth bool      `json:"borrowed_oauth,omitempty"`
	Decision      string    `json:"decision,omitempty"`
	Reason        string    `json:"reason,omitempty"`
}

var credentialAuditMu sync.Mutex

func auditCredentialEvent(ctx context.Context, event credentialAuditEvent) {
	event.Event = strings.TrimSpace(event.Event)
	if event.Event == "" {
		return
	}

	event.Timestamp = time.Now().UTC()
	event.Provider = normalizeProviderName(event.Provider)
	event.Store = normalizeCredentialPolicyValue(event.Store)
	event.Location = redactCredentialLocation(event.Location)
	event.Identifier = redactCredentialIdentifier(event.Identifier)
	event.Reason = RedactDiagnosticMessage(event.Reason)

	line, err := json.Marshal(event)
	if err != nil {
		return
	}

	dir := credentialAuditDir(ctx)
	if mkdirErr := os.MkdirAll(dir, 0o700); mkdirErr != nil {
		return
	}

	credentialAuditMu.Lock()
	defer credentialAuditMu.Unlock()

	file, err := os.OpenFile(filepath.Join(dir, credentialAuditLedgerFileName), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer file.Close()

	if _, err := file.Write(append(line, '\n')); err != nil {
		return
	}
}

func auditCredentialRefreshFailure(ctx context.Context, source credentialSource, err error) {
	if err == nil {
		return
	}

	auditCredentialEvent(ctx, credentialAuditEvent{
		Event:         credentialAuditEventRefresh,
		Provider:      source.Provider,
		Store:         source.Store,
		Action:        credentialActionRefresh,
		Source:        source.Description,
		Location:      source.Location,
		Identifier:    source.Identifier,
		BorrowedOAuth: source.BorrowedOAuth,
		Decision:      "failed",
		Reason:        err.Error(),
	})
}

func auditCredentialWriteBackFailure(ctx context.Context, source credentialSource, err error) {
	if err == nil {
		return
	}

	auditCredentialEvent(ctx, credentialAuditEvent{
		Event:         credentialAuditEventWriteBack,
		Provider:      source.Provider,
		Store:         source.Store,
		Action:        credentialActionWriteBack,
		Source:        source.Description,
		Location:      source.Location,
		Identifier:    source.Identifier,
		BorrowedOAuth: source.BorrowedOAuth,
		Decision:      "failed",
		Reason:        err.Error(),
	})
}

func credentialAuditDir(ctx context.Context) string {
	if dir := permission.AuditDirFromContext(ctx); dir != "" {
		return dir
	}

	if dir := strings.TrimSpace(os.Getenv(permission.EnvAuditDir)); dir != "" {
		return dir
	}

	return filepath.Join(os.TempDir(), "atteler", "audit")
}

func credentialPolicySummary(policy CredentialSourcePolicy) string {
	if policy.empty() {
		policy = defaultCredentialSourcePolicy()
	}

	return fmt.Sprintf(
		"allowed_providers=%s allowed_stores=%s allow_borrowed_oauth=%t allow_refresh=%t allow_write_back=%t",
		credentialPolicyListSummary(policy.AllowedProviders, policy.AllowedProvidersSet, "*"),
		credentialPolicyListSummary(policy.AllowedStores, policy.AllowedStoresSet, strings.Join(defaultAllowedCredentialStores(), ",")),
		policy.AllowBorrowedOAuth,
		policy.AllowRefresh,
		policy.AllowWriteBack,
	)
}

//nolint:wsl_v5 // Compact rendering avoids spreading simple summary formatting.
func credentialPolicyListSummary(values []string, explicitlySet bool, defaultValue string) string {
	if len(values) == 0 {
		if explicitlySet {
			return "[]"
		}

		return defaultValue
	}

	cleaned := make([]string, 0, len(values))
	for _, value := range values {
		value = normalizeCredentialPolicyValue(value)
		if value != "" {
			cleaned = append(cleaned, value)
		}
	}
	if len(cleaned) == 0 {
		if explicitlySet {
			return "[]"
		}

		return defaultValue
	}

	return strings.Join(cleaned, ",")
}
