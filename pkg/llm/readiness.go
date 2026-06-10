package llm

import (
	"context"
	"fmt"
	"maps"
	"sort"
	"strings"
	"time"
)

const (
	// DefaultReadinessCacheTTL is the default in-process cache TTL for provider
	// health checks performed during auto-registration and diagnostics.
	DefaultReadinessCacheTTL = 5 * time.Minute

	// DefaultReadinessCheckTimeout bounds auto-registration readiness probes so
	// startup reports provider state without turning every launch into a long
	// network wait.
	DefaultReadinessCheckTimeout = 3 * time.Second
)

// ProviderReadinessStatus describes provider availability after
// auto-registration and optional health checks.
type ProviderReadinessStatus string

// Provider readiness states.
const (
	ProviderStatusRegistered        ProviderReadinessStatus = "registered"
	ProviderStatusDisabled          ProviderReadinessStatus = "disabled"
	ProviderStatusMissingCredential ProviderReadinessStatus = "missing_credentials"
	ProviderStatusFailed            ProviderReadinessStatus = "failed"
	ProviderStatusFailedHealthCheck ProviderReadinessStatus = "failed_health_check"
)

// ModelCatalogSource tells whether the registry is using a live provider model
// list or a static fallback catalog.
type ModelCatalogSource string

// Model catalog sources.
const (
	ModelCatalogSourceStatic ModelCatalogSource = "static"
	ModelCatalogSourceLive   ModelCatalogSource = "live"
)

// ProviderModelCatalog separates a provider's static fallback model catalog
// from the live model list fetched from the provider API.
//
//nolint:govet // Field order keeps provenance and model slices grouped for callers.
type ProviderModelCatalog struct {
	Error           error
	ProviderName    string
	Models          []string
	StaticModels    []string
	LiveModels      []string
	ModelProvenance map[string]ModelProvenance
	Source          ModelCatalogSource
	Stale           bool
}

// ProviderReadiness is the product-facing provider availability state recorded
// during auto-registration and provider health diagnostics.
//
//nolint:govet // Field order keeps related readiness fields together for reports.
type ProviderReadiness struct {
	Error              error
	HealthError        error
	ModelFetchError    error
	CheckedAt          time.Time
	CacheTTL           time.Duration
	Name               string
	Status             ProviderReadinessStatus
	ModelCatalogSource ModelCatalogSource
	Models             []string
	StaticModels       []string
	LiveModels         []string
	RetryPolicy        RetryPolicyInfo
	Registered         bool
	Configured         bool
	Requested          bool
	HealthChecked      bool
	HealthCached       bool
	Healthy            bool
	ModelsStale        bool
}

// DefaultSelectionReport records how configured default provider/model
// selection was applied after auto-registration.
type DefaultSelectionReport struct {
	ProviderError error
	ModelError    error
	Provider      string
	Model         string
}

// ProviderReadinessReport is returned from auto-registration and can be reused
// by CLI diagnostics or completion errors to explain provider availability.
//
//nolint:govet // Field order keeps provider entries before default-selection metadata.
type ProviderReadinessReport struct {
	Providers []ProviderReadiness
	Default   DefaultSelectionReport
}

//nolint:govet // Cache entry mirrors ProviderHealth plus its cache timestamp.
type providerHealthCacheEntry struct {
	health    ProviderHealth
	checkedAt time.Time
}

func cloneProviderModelCatalog(c ProviderModelCatalog) ProviderModelCatalog {
	c.Models = append([]string(nil), c.Models...)
	c.StaticModels = append([]string(nil), c.StaticModels...)

	c.LiveModels = append([]string(nil), c.LiveModels...)
	if c.ModelProvenance != nil {
		c.ModelProvenance = maps.Clone(c.ModelProvenance)
	}

	return c
}

func cloneProviderReadiness(p ProviderReadiness) ProviderReadiness {
	p.Models = append([]string(nil), p.Models...)
	p.StaticModels = append([]string(nil), p.StaticModels...)
	p.LiveModels = append([]string(nil), p.LiveModels...)

	return p
}

func cloneProviderReadinessReport(report ProviderReadinessReport) ProviderReadinessReport {
	report.Providers = append([]ProviderReadiness(nil), report.Providers...)
	for i := range report.Providers {
		report.Providers[i] = cloneProviderReadiness(report.Providers[i])
	}

	return report
}

func (r *Registry) setStaticProviderCatalogLocked(providerName string, models []string) {
	if r.catalogs == nil {
		r.catalogs = make(map[string]ProviderModelCatalog)
	}

	staticModels := cleanModelList(models)
	r.catalogs[providerName] = ProviderModelCatalog{
		ProviderName:    providerName,
		Models:          append([]string(nil), staticModels...),
		StaticModels:    append([]string(nil), staticModels...),
		ModelProvenance: modelProvenanceForModels(staticModels, ModelProvenanceStatic),
		Source:          ModelCatalogSourceStatic,
	}
}

func (r *Registry) setProviderCatalogLocked(providerName string, catalog ProviderModelCatalog) {
	if r.catalogs == nil {
		r.catalogs = make(map[string]ProviderModelCatalog)
	}

	catalog.ProviderName = providerName
	catalog.Models = cleanModelList(catalog.Models)
	catalog.StaticModels = cleanModelList(catalog.StaticModels)

	catalog.LiveModels = cleanModelList(catalog.LiveModels)
	if catalog.ModelProvenance == nil {
		catalog.ModelProvenance = modelProvenanceForModels(catalog.Models, catalogProvenance(catalog.Source))
	}

	r.catalogs[providerName] = cloneProviderModelCatalog(catalog)
	r.addProviderModelOverridesToCatalogLocked(providerName)
}

func (r *Registry) providerCatalogLocked(providerName string) (ProviderModelCatalog, bool) {
	if r.catalogs == nil {
		return ProviderModelCatalog{}, false
	}

	catalog, ok := r.catalogs[providerName]

	return cloneProviderModelCatalog(catalog), ok
}

func (r *Registry) markProviderRegisteredLocked(providerName string, models []string) {
	entry := ProviderReadiness{
		Name:               providerName,
		Status:             ProviderStatusRegistered,
		Registered:         true,
		ModelCatalogSource: ModelCatalogSourceStatic,
		Models:             cleanModelList(models),
		StaticModels:       cleanModelList(models),
	}

	if existing, ok := r.readinessProviderLocked(providerName); ok {
		entry.Configured = existing.Configured
		entry.Requested = existing.Requested
	}

	r.upsertReadinessProviderLocked(entry)
}

// ReadinessReport returns the latest provider readiness snapshot known to the
// registry. Registries built manually synthesize registered provider entries.
func (r *Registry) ReadinessReport() ProviderReadinessReport {
	r.mu.RLock()
	defer r.mu.RUnlock()

	report := r.readinessReportLocked()

	return cloneProviderReadinessReport(report)
}

func (r *Registry) readinessReportLocked() ProviderReadinessReport {
	report := cloneProviderReadinessReport(r.readiness)

	for name, provider := range r.providers {
		if _, ok := findReadinessProvider(report, name); ok {
			continue
		}

		models := provider.Models()
		if catalog, ok := r.providerCatalogLocked(name); ok {
			models = catalog.Models
		}

		report.Providers = append(report.Providers, ProviderReadiness{
			Name:               name,
			Status:             ProviderStatusRegistered,
			Registered:         true,
			ModelCatalogSource: ModelCatalogSourceStatic,
			Models:             cleanModelList(models),
			StaticModels:       cleanModelList(provider.Models()),
		})
	}

	sortReadinessProviders(report.Providers)

	return report
}

func (r *Registry) readinessProviderLocked(providerName string) (ProviderReadiness, bool) {
	entry, ok := findReadinessProvider(r.readiness, providerName)

	return cloneProviderReadiness(entry), ok
}

func (r *Registry) upsertReadinessProviderLocked(entry ProviderReadiness) {
	entry = cloneProviderReadiness(entry)
	if entry.Status == "" {
		entry.Status = ProviderStatusRegistered
	}

	for i := range r.readiness.Providers {
		if r.readiness.Providers[i].Name == entry.Name {
			r.readiness.Providers[i] = entry
			sortReadinessProviders(r.readiness.Providers)

			return
		}
	}

	r.readiness.Providers = append(r.readiness.Providers, entry)
	sortReadinessProviders(r.readiness.Providers)
}

func findReadinessProvider(report ProviderReadinessReport, providerName string) (ProviderReadiness, bool) {
	for i := range report.Providers {
		entry := &report.Providers[i]
		if entry.Name == providerName {
			return cloneProviderReadiness(*entry), true
		}
	}

	return ProviderReadiness{}, false
}

func sortReadinessProviders(providers []ProviderReadiness) {
	sort.Slice(providers, func(i, j int) bool {
		return providers[i].Name < providers[j].Name
	})
}

func cleanModelList(models []string) []string {
	out := make([]string, 0, len(models))
	seen := make(map[string]bool, len(models))

	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" || seen[model] {
			continue
		}

		seen[model] = true
		out = append(out, model)
	}

	return out
}

func modelProvenanceForModels(models []string, provenance ModelProvenance) map[string]ModelProvenance {
	out := make(map[string]ModelProvenance, len(models))
	for _, model := range cleanModelList(models) {
		out[model] = provenance
	}

	return out
}

func catalogProvenance(source ModelCatalogSource) ModelProvenance {
	switch source {
	case ModelCatalogSourceLive:
		return ModelProvenanceFetchedLive
	default:
		return ModelProvenanceStatic
	}
}

// ProviderModelCatalog fetches a provider's live model list when supported and
// returns explicit provenance for live-vs-static model availability.
func (r *Registry) ProviderModelCatalog(ctx context.Context, providerName string) (ProviderModelCatalog, error) {
	if err := requireCredentialContext(ctx); err != nil {
		return ProviderModelCatalog{}, err
	}

	r.mu.RLock()
	p, ok := r.providers[providerName]
	r.mu.RUnlock()

	if !ok {
		return ProviderModelCatalog{}, fmt.Errorf("llm: unknown provider %q", providerName)
	}

	catalog := fetchProviderModelCatalog(ctx, p)

	r.mu.Lock()
	r.setProviderCatalogLocked(providerName, catalog)

	if len(catalog.LiveModels) > 0 {
		if providerModelsVerified(p) {
			r.replaceProviderModelsLocked(providerName, catalog.LiveModels, ModelProvenanceFetchedLive, true)
		} else {
			r.indexProviderModelsLocked(providerName, catalog.LiveModels, ModelProvenanceFetchedLive)
			r.markProviderModelsUnverifiedLocked(providerName)
		}
	} else {
		r.replaceProviderModelsLocked(providerName, catalog.StaticModels, ModelProvenanceStatic, false)
	}

	storedCatalog, ok := r.providerCatalogLocked(providerName)
	if ok {
		catalog = storedCatalog
	}

	r.updateReadinessCatalogLocked(providerName, catalog)
	r.mu.Unlock()

	return catalog, nil
}

func fetchProviderModelCatalog(ctx context.Context, p Provider) ProviderModelCatalog {
	staticModels := cleanModelList(p.Models())
	catalog := ProviderModelCatalog{
		ProviderName:    p.Name(),
		Models:          append([]string(nil), staticModels...),
		StaticModels:    append([]string(nil), staticModels...),
		ModelProvenance: modelProvenanceForModels(staticModels, ModelProvenanceStatic),
		Source:          ModelCatalogSourceStatic,
	}

	if !providerSupportsLiveModels(p.Name()) {
		return catalog
	}

	liveModels, err := p.FetchModels(ctx)
	if err != nil {
		catalog.Error = err
		catalog.Stale = true

		return catalog
	}

	liveModels = cleanModelList(liveModels)
	if len(liveModels) == 0 {
		return catalog
	}

	catalog.Models = append([]string(nil), liveModels...)
	catalog.LiveModels = append([]string(nil), liveModels...)
	catalog.ModelProvenance = modelProvenanceForModels(liveModels, ModelProvenanceFetchedLive)
	catalog.Source = ModelCatalogSourceLive

	return catalog
}

func (r *Registry) updateReadinessCatalogLocked(providerName string, catalog ProviderModelCatalog) {
	entry, ok := r.readinessProviderLocked(providerName)
	if !ok {
		entry = ProviderReadiness{Name: providerName, Status: ProviderStatusRegistered, Registered: true}
	}

	entry.Models = append([]string(nil), catalog.Models...)
	entry.StaticModels = append([]string(nil), catalog.StaticModels...)
	entry.LiveModels = append([]string(nil), catalog.LiveModels...)
	entry.ModelCatalogSource = catalog.Source
	entry.ModelFetchError = catalog.Error
	entry.ModelsStale = catalog.Stale

	if entry.HealthError != nil {
		entry.Error = entry.HealthError
	} else {
		entry.Error = catalog.Error
	}

	r.upsertReadinessProviderLocked(entry)
}

func providerSupportsLiveModels(providerName string) bool {
	switch providerName {
	case providerClaudeCode, providerCodex:
		return false
	default:
		return true
	}
}

func providerFetchModelsChecksHealth(providerName string) bool {
	switch providerName {
	case providerAnthropic, providerOpenAI, providerOllama:
		return true
	default:
		return false
	}
}

// CheckHealthWithTTL pings every registered provider and fetches live models
// when supported. ttl controls the in-process health-check cache; pass 0 to
// force fresh checks.
func (r *Registry) CheckHealthWithTTL(ctx context.Context, ttl time.Duration) []ProviderHealth {
	r.mu.RLock()

	names := make([]string, 0, len(r.providers))
	for name := range r.providers {
		names = append(names, name)
	}

	r.mu.RUnlock()

	sort.Strings(names)

	results := make([]ProviderHealth, 0, len(names))
	for _, name := range names {
		results = append(results, r.checkProviderHealth(ctx, name, ttl))
	}

	return results
}

// CheckReadiness returns the full provider readiness report, including
// disabled and skipped providers from auto-registration plus health results for
// registered providers.
func (r *Registry) CheckReadiness(ctx context.Context, ttl time.Duration) ProviderReadinessReport {
	r.CheckHealthWithTTL(ctx, ttl)

	return r.ReadinessReport()
}

func (r *Registry) checkProviderHealth(ctx context.Context, providerName string, ttl time.Duration) ProviderHealth {
	now := time.Now()

	r.mu.RLock()
	retryPolicy := r.retryPolicyForProviderLocked(providerName).info()

	if cached, ok := r.cachedProviderHealthLocked(providerName, ttl, now); ok {
		cached.RetryPolicy = retryPolicy

		r.mu.RUnlock()
		r.mu.Lock()
		r.applyHealthToReadinessLocked(providerName, cached)
		r.mu.Unlock()

		return cached
	}

	ctxErr := requireCredentialContext(ctx)

	p, ok := r.providers[providerName]
	r.mu.RUnlock()

	if !ok {
		return ProviderHealth{
			Name:        providerName,
			RetryPolicy: retryPolicy,
			Error:       fmt.Errorf("llm: unknown provider %q", providerName),
		}
	}

	health := providerHealthFresh(ctx, providerName, p, now, ttl, ctxErr)
	health.RetryPolicy = retryPolicy

	r.mu.Lock()
	r.cacheProviderHealthLocked(providerName, health, now)
	r.applyHealthToReadinessLocked(providerName, health)
	r.mu.Unlock()

	return cloneProviderHealth(health)
}

func providerHealthFresh(
	ctx context.Context,
	providerName string,
	p Provider,
	now time.Time,
	ttl time.Duration,
	ctxErr error,
) ProviderHealth {
	if ctxErr != nil {
		health := providerHealthBase(providerName, p, now, ttl)
		health.Error = ctxErr
		health.Models = cleanModelList(p.Models())
		health.ModelSource = ModelCatalogSourceStatic

		return health
	}

	if diagnosticProvider, ok := p.(DiagnosticsProvider); ok {
		return providerDiagnosticHealth(providerName, p, diagnosticProvider, now, ttl)
	}

	if providerFetchModelsChecksHealth(providerName) {
		return providerCatalogHealth(ctx, providerName, p, now, ttl)
	}

	return providerExplicitHealthCheck(ctx, providerName, p, now, ttl)
}

func providerHealthBase(providerName string, p Provider, now time.Time, ttl time.Duration) ProviderHealth {
	return ProviderHealth{
		Name:         providerName,
		CheckedAt:    now,
		CacheTTL:     ttl,
		StaticModels: cleanModelList(p.Models()),
		Warnings:     appendProviderWarnings(nil, p),
	}
}

func providerDiagnosticHealth(
	providerName string,
	p Provider,
	diagnosticProvider DiagnosticsProvider,
	now time.Time,
	ttl time.Duration,
) ProviderHealth {
	health := providerHealthFromDiagnostics(providerName, diagnosticProvider.AdapterDiagnostics())
	health.CheckedAt = now
	health.CacheTTL = ttl
	health.StaticModels = cleanModelList(p.Models())
	health.ModelSource = ModelCatalogSourceStatic
	health.Warnings = appendProviderWarnings(health.Warnings, p)

	return providerHealthWithDefaults(health, p)
}

func providerCatalogHealth(ctx context.Context, providerName string, p Provider, now time.Time, ttl time.Duration) ProviderHealth {
	health := providerHealthBase(providerName, p, now, ttl)
	catalog := fetchProviderModelCatalog(ctx, p)

	health.Models = append([]string(nil), catalog.Models...)
	health.StaticModels = append([]string(nil), catalog.StaticModels...)
	health.LiveModels = append([]string(nil), catalog.LiveModels...)
	health.ModelSource = catalog.Source
	health.ModelFetchError = catalog.Error
	health.Error = catalog.Error
	health.Healthy = catalog.Error == nil
	health.ModelsStale = catalog.Stale

	return providerHealthWithDefaults(health, p)
}

func providerExplicitHealthCheck(ctx context.Context, providerName string, p Provider, now time.Time, ttl time.Duration) ProviderHealth {
	health := providerHealthBase(providerName, p, now, ttl)

	if err := p.HealthCheck(ctx); err != nil {
		health.Error = err
		health.Models = cleanModelList(p.Models())
		health.ModelSource = ModelCatalogSourceStatic

		return providerHealthWithDefaults(health, p)
	}

	catalog := fetchProviderModelCatalog(ctx, p)
	health.Healthy = true

	health.Models = append([]string(nil), catalog.Models...)
	health.StaticModels = append([]string(nil), catalog.StaticModels...)
	health.LiveModels = append([]string(nil), catalog.LiveModels...)
	health.ModelSource = catalog.Source
	health.ModelFetchError = catalog.Error
	health.ModelsStale = catalog.Stale

	if catalog.Error != nil {
		health.Models = cleanModelList(p.Models())
		health.ModelSource = ModelCatalogSourceStatic
		health.ModelsStale = true
	}

	return providerHealthWithDefaults(health, p)
}

func providerHealthWithDefaults(health ProviderHealth, p Provider) ProviderHealth {
	if len(health.Models) == 0 {
		health.Models = cleanModelList(p.Models())
	}

	if health.ModelSource == "" {
		health.ModelSource = ModelCatalogSourceStatic
	}

	return health
}

func (r *Registry) cachedProviderHealthLocked(providerName string, ttl time.Duration, now time.Time) (ProviderHealth, bool) {
	if ttl <= 0 || r.healthCache == nil {
		return ProviderHealth{}, false
	}

	cached, ok := r.healthCache[providerName]
	if !ok || now.Sub(cached.checkedAt) >= ttl {
		return ProviderHealth{}, false
	}

	health := cloneProviderHealth(cached.health)
	health.Cached = true
	health.CheckedAt = cached.checkedAt
	health.CacheTTL = ttl

	return health, true
}

func (r *Registry) cacheProviderHealthLocked(providerName string, health ProviderHealth, checkedAt time.Time) {
	if r.healthCache == nil {
		r.healthCache = make(map[string]providerHealthCacheEntry)
	}

	health.Cached = false
	health.CheckedAt = checkedAt
	r.healthCache[providerName] = providerHealthCacheEntry{
		health:    cloneProviderHealth(health),
		checkedAt: checkedAt,
	}
}

func (r *Registry) applyHealthToReadinessLocked(providerName string, health ProviderHealth) {
	entry, ok := r.readinessProviderLocked(providerName)
	if !ok {
		entry = ProviderReadiness{Name: providerName, Registered: true}
	}

	entry.Status = ProviderStatusRegistered
	if !health.Healthy {
		entry.Status = ProviderStatusFailedHealthCheck
	}

	entry.Registered = true
	entry.HealthChecked = true
	entry.HealthCached = health.Cached
	entry.Healthy = health.Healthy
	entry.CheckedAt = health.CheckedAt
	entry.CacheTTL = health.CacheTTL
	entry.HealthError = health.Error
	entry.Error = health.Error
	entry.ModelFetchError = health.ModelFetchError
	entry.Models = append([]string(nil), health.Models...)
	entry.StaticModels = append([]string(nil), health.StaticModels...)
	entry.LiveModels = append([]string(nil), health.LiveModels...)
	entry.RetryPolicy = health.RetryPolicy
	entry.ModelCatalogSource = health.ModelSource
	entry.ModelsStale = health.ModelsStale

	if health.Healthy && health.ModelFetchError != nil {
		entry.Error = health.ModelFetchError
	}

	if len(entry.StaticModels) == 0 {
		entry.StaticModels = append([]string(nil), health.Models...)
	}

	r.upsertReadinessProviderLocked(entry)
	r.setProviderCatalogLocked(providerName, ProviderModelCatalog{
		ProviderName:    providerName,
		Models:          health.Models,
		StaticModels:    health.StaticModels,
		LiveModels:      health.LiveModels,
		ModelProvenance: modelProvenanceForModels(health.Models, catalogProvenance(health.ModelSource)),
		Source:          health.ModelSource,
		Error:           health.ModelFetchError,
		Stale:           health.ModelsStale,
	})

	if len(health.LiveModels) > 0 {
		if provider, ok := r.providers[providerName]; ok && providerModelsVerified(provider) {
			r.replaceProviderModelsLocked(providerName, health.LiveModels, ModelProvenanceFetchedLive, true)
		} else {
			r.indexProviderModelsLocked(providerName, health.LiveModels, ModelProvenanceFetchedLive)
			r.markProviderModelsUnverifiedLocked(providerName)
		}
	} else {
		r.replaceProviderModelsLocked(providerName, health.StaticModels, ModelProvenanceStatic, false)
	}
}

func cloneProviderHealth(health ProviderHealth) ProviderHealth {
	health.Models = append([]string(nil), health.Models...)
	health.StaticModels = append([]string(nil), health.StaticModels...)
	health.LiveModels = append([]string(nil), health.LiveModels...)
	health.Checks = append([]ReadinessCheck(nil), health.Checks...)
	health.Warnings = append([]string(nil), health.Warnings...)

	if health.Contract != nil {
		contract := *health.Contract
		health.Contract = &contract
	}

	return health
}

func (r *Registry) readinessContextLocked() string {
	return r.readinessContextForModelLocked("")
}

func (r *Registry) readinessContextForModelLocked(model string) string {
	report := r.readinessReportLocked()
	relevantProviders := r.relevantProviderNamesForModelLocked(model)

	segments := make([]string, 0, 2)
	providers := make([]string, 0, len(report.Providers))

	for i := range report.Providers {
		provider := &report.Providers[i]
		if !includeProviderInResolutionContext(provider, relevantProviders) {
			continue
		}

		providers = append(providers, providerReadinessSummary(provider))
	}

	if len(providers) > 0 {
		segments = append(segments, "providers: "+strings.Join(providers, "; "))
	}

	if defaults := defaultSelectionReadinessSummary(report.Default); defaults != "" {
		segments = append(segments, defaults)
	}

	if len(segments) == 0 {
		return ""
	}

	return "provider readiness: " + strings.Join(segments, "; ")
}

func (r *Registry) relevantProviderNamesForModelLocked(model string) map[string]bool {
	model = strings.TrimSpace(model)
	if model == "" {
		return nil
	}

	if explicitProvider, _, ok := splitProviderModel(model); ok {
		return map[string]bool{explicitProvider: true}
	}

	candidates := r.modelResolutionCandidatesLocked(model)

	providers := make(map[string]bool, len(candidates))
	for _, candidate := range candidates {
		providers[candidate.ProviderName] = true
	}

	entries := r.readinessReportLocked().Providers
	for i := range entries {
		if providerReadinessMentionsModel(&entries[i], model) {
			providers[entries[i].Name] = true
		}
	}

	if len(providers) == 0 {
		return nil
	}

	return providers
}

func providerReadinessMentionsModel(entry *ProviderReadiness, model string) bool {
	return containsModel(entry.Models, model) ||
		containsModel(entry.StaticModels, model) ||
		containsModel(entry.LiveModels, model)
}

func includeProviderInResolutionContext(entry *ProviderReadiness, relevantProviders map[string]bool) bool {
	if entry == nil {
		return false
	}

	return entry.Registered || entry.Configured || entry.Requested || relevantProviders[entry.Name]
}

func providerReadinessSummary(entry *ProviderReadiness) string {
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

	if classification, ok := providerReadinessClassification(*entry); ok {
		parts = append(parts, "classification="+string(classification.Kind))
		if err := providerReadinessError(*entry); err != nil {
			parts = append(parts, "error="+classifiedProviderError(err))
		}
	}

	return strings.Join(parts, " ")
}

func providerReadinessError(entry ProviderReadiness) error {
	switch {
	case entry.Error != nil:
		return entry.Error
	case entry.HealthError != nil:
		return entry.HealthError
	case entry.ModelFetchError != nil:
		return entry.ModelFetchError
	default:
		return nil
	}
}

func providerReadinessClassification(entry ProviderReadiness) (providerFailureClassification, bool) {
	if err := providerReadinessError(entry); err != nil {
		return classifyProviderFailure(err), true
	}

	switch {
	case entry.Status == ProviderStatusMissingCredential:
		return providerFailureClassification{
			Kind:    providerFailureAuthentication,
			Summary: "provider authentication/configuration error",
		}, true
	case entry.ModelsStale ||
		entry.Status == ProviderStatusFailed ||
		entry.Status == ProviderStatusFailedHealthCheck:
		return providerFailureClassification{
			Kind:    providerFailureNotReady,
			Summary: "provider readiness is stale or unavailable",
		}, true
	default:
		return providerFailureClassification{}, false
	}
}

func defaultSelectionReadinessSummary(report DefaultSelectionReport) string {
	var parts []string

	if report.ProviderError != nil {
		value := strings.TrimSpace(report.Provider)
		if value == "" {
			value = "<empty>"
		}

		parts = append(parts, "default_provider="+value+" error="+shortError(report.ProviderError))
	}

	if report.ModelError != nil {
		value := strings.TrimSpace(report.Model)
		if value == "" {
			value = "<empty>"
		}

		parts = append(parts, "default_model="+value+" error="+shortError(report.ModelError))
	}

	if len(parts) == 0 {
		return ""
	}

	return "default selection: " + strings.Join(parts, "; ")
}

func shortError(err error) string {
	if err == nil {
		return ""
	}

	msg := strings.TrimSpace(err.Error())

	const maxErrorLen = 160
	if len(msg) > maxErrorLen {
		return msg[:maxErrorLen] + "…"
	}

	return msg
}

func (r *Registry) resolutionErrorLocked(err error) error {
	if err == nil {
		return nil
	}

	readiness := r.readinessContextLocked()
	if readiness == "" {
		return err
	}

	return fmt.Errorf("%w (%s)", err, readiness)
}

func (r *Registry) resolutionErrorForModelLocked(err error, model string) error {
	if err == nil {
		return nil
	}

	readiness := r.readinessContextForModelLocked(model)
	if readiness == "" {
		return err
	}

	return fmt.Errorf("%w (%s)", err, readiness)
}

func (r *Registry) fallbackAttemptTarget(model string) fallbackAttemptTarget {
	r.mu.RLock()
	defer r.mu.RUnlock()

	diagnostic := r.explainModelResolutionLocked(model)
	if diagnostic.Error != nil {
		providerName := ""

		if explicitProvider, _, ok := splitProviderModel(model); ok {
			providerName = explicitProvider
		}

		if strings.TrimSpace(model) == "" {
			return fallbackAttemptTarget{
				label:        "unresolved",
				providerName: providerName,
				resolved:     false,
			}
		}

		return fallbackAttemptTarget{
			label:        strings.TrimSpace(model) + " unresolved",
			providerName: providerName,
			resolved:     false,
		}
	}

	label := diagnostic.ProviderName + "/" + diagnostic.ProviderModel
	if diagnostic.Provenance != "" {
		label += " (" + string(diagnostic.Provenance) + ")"
	}

	return fallbackAttemptTarget{
		label:         label,
		providerName:  diagnostic.ProviderName,
		providerModel: diagnostic.ProviderModel,
		resolved:      true,
	}
}
