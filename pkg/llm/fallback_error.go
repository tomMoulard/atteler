package llm

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/tommoulard/atteler/pkg/modelroute"
)

type fallbackAttemptTarget struct {
	label         string
	providerName  string
	providerModel string
	resolved      bool
}

//nolint:govet // Grouped by semantics; padding is negligible for fallback error paths.
type fallbackAttemptFailure struct {
	err            error
	classification providerFailureClassification
	model          string
	providerName   string
	label          string
	rateLimitScope string
	skipped        bool
}

type fallbackError struct {
	readiness ProviderReadinessReport
	attempts  []fallbackAttemptFailure
}

func newFallbackError(attempts []fallbackAttemptFailure, readiness ProviderReadinessReport) error {
	if len(attempts) == 0 {
		return errors.New("llm: all fallback models failed: no fallback attempts were made")
	}

	return &fallbackError{
		attempts:  append([]fallbackAttemptFailure(nil), attempts...),
		readiness: cloneProviderReadinessReport(readiness),
	}
}

func (e *fallbackError) Error() string {
	if e == nil || len(e.attempts) == 0 {
		return "llm: all fallback models failed: no fallback attempts were made"
	}

	parts := make([]string, 0, len(e.attempts)+1)
	for _, provider := range e.providerOrder() {
		parts = append(parts, e.providerSummary(provider))
	}

	if readiness := fallbackReadinessIssues(e.readiness, e.providerSet()); readiness != "" {
		parts = append(parts, readiness)
	}

	return "llm: all fallback models failed: " + strings.Join(parts, "; ")
}

func (e *fallbackError) Unwrap() []error {
	if e == nil {
		return nil
	}

	errs := make([]error, 0, len(e.attempts))
	for i := range e.attempts {
		attempt := &e.attempts[i]
		if attempt.err != nil {
			errs = append(errs, attempt.err)
		}
	}

	return errs
}

func (e *fallbackError) providerOrder() []string {
	seen := make(map[string]bool, len(e.attempts))
	order := make([]string, 0, len(e.attempts))

	for i := range e.attempts {
		provider := fallbackProviderGroup(e.attempts[i].providerName)
		if seen[provider] {
			continue
		}

		seen[provider] = true
		order = append(order, provider)
	}

	return order
}

func (e *fallbackError) providerSet() map[string]bool {
	if e == nil {
		return nil
	}

	providers := make(map[string]bool, len(e.attempts))
	for i := range e.attempts {
		provider := strings.TrimSpace(e.attempts[i].providerName)
		if provider == "" {
			continue
		}

		providers[provider] = true
	}

	return providers
}

func (e *fallbackError) providerSummary(provider string) string {
	grouped := make(map[providerFailureKind][]string)

	for i := range e.attempts {
		attempt := &e.attempts[i]
		if fallbackProviderGroup(attempt.providerName) != provider {
			continue
		}

		grouped[attempt.classification.Kind] = append(grouped[attempt.classification.Kind], attemptSummary(*attempt))
	}

	categories := make([]providerFailureKind, 0, len(grouped))
	for category := range grouped {
		categories = append(categories, category)
	}

	sort.SliceStable(categories, func(i, j int) bool {
		return providerFailureKindRank(categories[i]) < providerFailureKindRank(categories[j])
	})

	parts := make([]string, 0, len(categories))
	for _, category := range categories {
		parts = append(parts, string(category)+": "+strings.Join(grouped[category], ", "))
	}

	return provider + ": " + strings.Join(parts, "; ")
}

func providerFailureKindRank(kind providerFailureKind) int {
	switch kind {
	case providerFailureConfiguration:
		return 0
	case providerFailureAuthentication:
		return 1
	case providerFailureNotReady:
		return 2
	case providerFailureRateLimit:
		return 3
	case providerFailureTransient:
		return 4
	case providerFailureRouteExhausted:
		return 5
	case providerFailurePermanent:
		return 6
	default:
		return 7
	}
}

func attemptSummary(attempt fallbackAttemptFailure) string {
	prefix := strings.TrimSpace(attempt.model)
	if prefix == "" {
		prefix = "<default>"
	}

	if attempt.label != "" {
		prefix += " via " + attempt.label
	}

	if attempt.skipped {
		return prefix + " skipped: " + fallbackAttemptMessage(attempt)
	}

	return prefix + ": " + fallbackAttemptMessage(attempt)
}

func fallbackAttemptMessage(attempt fallbackAttemptFailure) string {
	classification := attempt.classification

	message := classification.Summary
	if message == "" {
		message = string(classification.Kind)
	}

	if classification.Remediation != "" {
		message += "; " + classification.Remediation
	}

	if attempt.err != nil && includeFallbackAttemptErrorDetail(classification) {
		message += ": " + shortError(attempt.err)
	}

	return message
}

func includeFallbackAttemptErrorDetail(classification providerFailureClassification) bool {
	if classification.Remediation != "" {
		return false
	}

	return classification.Kind != providerFailureRateLimit
}

func fallbackProviderGroup(providerName string) string {
	providerName = strings.TrimSpace(providerName)
	if providerName == "" {
		return "unresolved"
	}

	return providerName
}

func fallbackReadinessIssues(report ProviderReadinessReport, relevantProviders map[string]bool) string {
	issues := make([]string, 0, len(report.Providers))
	for i := range report.Providers {
		entry := report.Providers[i]
		if !includeFallbackReadinessIssue(entry, relevantProviders) {
			continue
		}

		if !providerReadinessHasIssue(entry) {
			continue
		}

		issues = append(issues, fallbackReadinessIssue(entry))
	}

	if len(issues) == 0 {
		return ""
	}

	return "provider readiness issues: " + strings.Join(issues, "; ")
}

func includeFallbackReadinessIssue(entry ProviderReadiness, relevantProviders map[string]bool) bool {
	if len(relevantProviders) == 0 || relevantProviders[entry.Name] {
		return true
	}

	classification, ok := providerReadinessClassification(entry)

	return ok && classification.Kind == providerFailureConfiguration
}

func providerReadinessHasIssue(entry ProviderReadiness) bool {
	if providerReadinessError(entry) != nil || entry.ModelsStale {
		return true
	}

	return entry.Status != "" &&
		entry.Status != ProviderStatusRegistered &&
		entry.Status != ProviderStatusDisabled
}

func fallbackReadinessIssue(entry ProviderReadiness) string {
	status := string(entry.Status)
	if status == "" {
		status = string(ProviderStatusRegistered)
	}

	parts := []string{entry.Name + "=" + status}
	if entry.ModelCatalogSource != "" {
		parts = append(parts, "models="+string(entry.ModelCatalogSource))
	}

	if entry.ModelsStale {
		parts = append(parts, "stale=true")
	}

	if classification, ok := providerReadinessClassification(entry); ok {
		parts = append(parts, "classification="+string(classification.Kind))
		if err := providerReadinessError(entry); err != nil {
			parts = append(parts, "error="+classifiedProviderError(err))
		}
	}

	return strings.Join(parts, " ")
}

func fallbackFailureMetadata(err error) map[string]string {
	var fb *fallbackError
	if !errors.As(err, &fb) || fb == nil {
		return nil
	}

	return fb.metadata()
}

func fallbackMetadataForAttempts(attempts []fallbackAttemptFailure, readiness ProviderReadinessReport) map[string]string {
	if len(attempts) == 0 {
		return nil
	}

	fb := &fallbackError{
		attempts:  append([]fallbackAttemptFailure(nil), attempts...),
		readiness: cloneProviderReadinessReport(readiness),
	}

	return fb.metadata()
}

func (e *fallbackError) metadata() map[string]string {
	metadata := map[string]string{
		"fallback_failure_classifications": e.classificationMetadata(),
		"fallback_attempts":                e.attemptsMetadata(),
	}

	if scopes := e.rateLimitScopesMetadata(); scopes != "" {
		metadata["fallback_rate_limit_scopes"] = scopes
	}

	if rateLimited := e.providersForKindIncludingReadiness(providerFailureRateLimit); rateLimited != "" {
		metadata["rate_limited_providers"] = rateLimited
	}

	if transient := e.providersForKind(providerFailureTransient); transient != "" {
		metadata["transient_error_providers"] = transient
	}

	if configErrors := e.providersForKindIncludingReadiness(providerFailureConfiguration); configErrors != "" {
		metadata["configuration_error_providers"] = configErrors
	}

	if authErrors := e.providersForKindIncludingReadiness(providerFailureAuthentication); authErrors != "" {
		metadata["authentication_error_providers"] = authErrors
	}

	if notReady := e.providersForKindIncludingReadiness(providerFailureNotReady); notReady != "" {
		metadata["provider_not_ready_providers"] = notReady
	}

	if exhausted := e.providersForKind(providerFailureRouteExhausted); exhausted != "" {
		metadata["exhausted_fallback_route_providers"] = exhausted
	}

	if permanent := e.providersForKind(providerFailurePermanent); permanent != "" {
		metadata["permanent_error_providers"] = permanent
	}

	if readiness := fallbackReadinessMetadata(e.readiness, e.providerSet()); readiness != "" {
		metadata["provider_readiness"] = readiness
	}

	return metadata
}

// ProviderFailureMetadata returns structured provider failure classifications
// suitable for lifecycle events, checkpoints, or session logs. It returns nil
// when err is not a classified fallback routing error.
func ProviderFailureMetadata(err error) map[string]string {
	return fallbackFailureMetadata(err)
}

func (e *fallbackError) classificationMetadata() string {
	if e == nil {
		return ""
	}

	seen := make(map[string]bool, len(e.attempts))

	parts := make([]string, 0, len(e.attempts))
	addClassification := func(provider string, kind providerFailureKind) {
		part := fallbackProviderGroup(provider) + "=" + string(kind)
		if seen[part] {
			return
		}

		seen[part] = true
		parts = append(parts, part)
	}

	for i := range e.attempts {
		attempt := &e.attempts[i]

		addClassification(attempt.providerName, attempt.classification.Kind)
	}

	relevantProviders := e.providerSet()
	for i := range e.readiness.Providers {
		entry := e.readiness.Providers[i]
		if !includeFallbackReadinessIssue(entry, relevantProviders) {
			continue
		}

		if classification, ok := providerReadinessClassification(entry); ok {
			addClassification(entry.Name, classification.Kind)
		}
	}

	return strings.Join(parts, ",")
}

func (e *fallbackError) attemptsMetadata() string {
	if e == nil {
		return ""
	}

	parts := make([]string, 0, len(e.attempts))
	for i := range e.attempts {
		attempt := &e.attempts[i]
		route := fallbackAttemptMetadataRoute(*attempt)

		status := string(attempt.classification.Kind)
		if attempt.skipped {
			status += ":skipped"
		}

		parts = append(parts, route+"="+status)
	}

	return strings.Join(parts, ",")
}

func (e *fallbackError) rateLimitScopesMetadata() string {
	if e == nil {
		return ""
	}

	parts := make([]string, 0, len(e.attempts))
	for i := range e.attempts {
		attempt := &e.attempts[i]
		if !attempt.classification.RateLimited || attempt.rateLimitScope == "" {
			continue
		}

		parts = append(parts, fallbackAttemptMetadataRoute(*attempt)+"="+attempt.rateLimitScope)
	}

	return strings.Join(parts, ",")
}

func fallbackAttemptMetadataRoute(attempt fallbackAttemptFailure) string {
	if strings.TrimSpace(attempt.label) != "" {
		return strings.TrimSpace(attempt.label)
	}

	if strings.TrimSpace(attempt.model) != "" {
		return strings.TrimSpace(attempt.model)
	}

	return "<default>"
}

func (e *fallbackError) providersForKind(kind providerFailureKind) string {
	if e == nil {
		return ""
	}

	seen := make(map[string]bool, len(e.attempts))

	providers := make([]string, 0, len(e.attempts))
	for i := range e.attempts {
		attempt := &e.attempts[i]
		if attempt.classification.Kind != kind {
			continue
		}

		provider := fallbackProviderGroup(attempt.providerName)
		if seen[provider] {
			continue
		}

		seen[provider] = true
		providers = append(providers, provider)
	}

	return strings.Join(providers, ",")
}

func (e *fallbackError) providersForKindIncludingReadiness(kind providerFailureKind) string {
	if e == nil {
		return ""
	}

	seen := make(map[string]bool, len(e.attempts)+len(e.readiness.Providers))
	providers := make([]string, 0, len(e.attempts))

	addProvider := func(provider string) {
		provider = fallbackProviderGroup(provider)
		if seen[provider] {
			return
		}

		seen[provider] = true
		providers = append(providers, provider)
	}

	for i := range e.attempts {
		attempt := &e.attempts[i]
		if attempt.classification.Kind == kind {
			addProvider(attempt.providerName)
		}
	}

	relevantProviders := e.providerSet()
	for i := range e.readiness.Providers {
		entry := e.readiness.Providers[i]
		if !includeFallbackReadinessIssue(entry, relevantProviders) {
			continue
		}

		if classification, ok := providerReadinessClassification(entry); ok && classification.Kind == kind {
			addProvider(entry.Name)
		}
	}

	return strings.Join(providers, ",")
}

func fallbackReadinessMetadata(report ProviderReadinessReport, relevantProviders map[string]bool) string {
	providers := make([]string, 0, len(report.Providers))
	for i := range report.Providers {
		entry := report.Providers[i]
		if !includeFallbackReadinessIssue(entry, relevantProviders) {
			continue
		}

		providers = append(providers, fallbackReadinessIssue(entry))
	}

	if len(providers) == 0 {
		return ""
	}

	return "provider readiness: " + strings.Join(providers, "; ")
}

func skippedRateLimitFailure(model string, target fallbackAttemptTarget, prior fallbackAttemptFailure) fallbackAttemptFailure {
	err := fmt.Errorf("provider %s temporarily rate limited after %s: %w", target.providerName, prior.model, prior.err)

	return fallbackAttemptFailure{
		err: err,
		classification: providerFailureClassification{
			Kind:        providerFailureRateLimit,
			Summary:     "provider temporarily rate-limited; skipped remaining route for this provider",
			RateLimited: true,
		},
		model:          model,
		providerName:   target.providerName,
		label:          target.label,
		rateLimitScope: modelroute.RateLimitScopeProvider,
		skipped:        true,
	}
}

func skippedProviderCooldownFailure(
	model string,
	target fallbackAttemptTarget,
	until time.Time,
	lastError string,
	rateLimitScope string,
) fallbackAttemptFailure {
	scopeName, scopeLabel := cooldownRateLimitScope(model, target, rateLimitScope)
	detail := scopeName + " " + scopeLabel + " temporarily rate limited"

	if !until.IsZero() {
		detail += " until " + until.Format(time.RFC3339)
	}

	if strings.TrimSpace(lastError) != "" {
		detail += ": " + shortError(errors.New(lastError))
	}

	return fallbackAttemptFailure{
		err: providerCooldownAttemptError(errors.New(detail), until),
		classification: providerFailureClassification{
			Kind:        providerFailureRateLimit,
			Summary:     scopeName + " temporarily rate-limited; skipped route during cooldown",
			RateLimited: true,
		},
		model:          model,
		providerName:   target.providerName,
		label:          target.label,
		rateLimitScope: rateLimitScope,
		skipped:        true,
	}
}

func cooldownRateLimitScope(
	model string,
	target fallbackAttemptTarget,
	rateLimitScope string,
) (scopeName, scopeLabel string) {
	if rateLimitScope != modelroute.RateLimitScopeModel {
		return modelroute.RateLimitScopeProvider, target.providerName
	}

	if target.providerName != "" && target.providerModel != "" {
		return modelroute.RateLimitScopeModel, target.providerName + "/" + target.providerModel
	}

	if target.label != "" {
		return modelroute.RateLimitScopeModel, target.label
	}

	return modelroute.RateLimitScopeModel, strings.TrimSpace(model)
}

func providerCooldownAttemptError(err error, until time.Time) error {
	if err == nil {
		return nil
	}

	if retryAfter := time.Until(until); retryAfter > 0 {
		return &ProviderError{
			StatusCode:   http.StatusTooManyRequests,
			RetryAfter:   retryAfter,
			Message:      err.Error(),
			Retryability: RetryabilityRetryable,
		}
	}

	return err
}
