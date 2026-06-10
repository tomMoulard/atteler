package llm

import (
	"errors"
	"strings"

	"github.com/tommoulard/atteler/pkg/modelroute"
)

// ReadinessStatus is the normalized result of one provider readiness check.
type ReadinessStatus string

// Readiness statuses used by provider diagnostics.
const (
	ReadinessOK      ReadinessStatus = "ok"
	ReadinessWarning ReadinessStatus = "warn"
	ReadinessFailed  ReadinessStatus = "fail"
	ReadinessSkipped ReadinessStatus = "skip"
)

// ReadinessCheck describes one independently reportable provider readiness
// dimension, such as local credentials, refresh capability, network reachability,
// or model availability.
type ReadinessCheck struct {
	Status ReadinessStatus
	Name   string
	Detail string
}

// AdapterContract records the compatibility assumptions for adapters that
// depend on private, beta, CLI-owned, or otherwise non-public provider behavior.
//
//nolint:govet // field order keeps doctor output grouped by contract semantics.
type AdapterContract struct {
	Provider         string
	AdapterVersion   string
	SourceCLI        string
	SourceCLIVersion string
	Protocol         string
	Credential       string
	KillSwitches     []string
	ReviewedAt       string
	ReviewAfter      string
}

// AdapterDiagnostics is a provider-supplied readiness report used by doctor.
type AdapterDiagnostics struct {
	Contract AdapterContract
	Checks   []ReadinessCheck
	Warnings []string
	Models   []string
}

// Healthy reports whether the diagnostics contain any failing check.
func (d AdapterDiagnostics) Healthy() bool {
	if len(d.Checks) == 0 {
		return false
	}

	for _, check := range d.Checks {
		if check.Status == ReadinessFailed {
			return false
		}
	}

	return true
}

// Error summarizes failing checks, returning nil when the adapter is healthy.
func (d AdapterDiagnostics) Error() error {
	if len(d.Checks) == 0 {
		return errors.New("no readiness checks reported")
	}

	failures := make([]string, 0)

	for _, check := range d.Checks {
		if check.Status != ReadinessFailed {
			continue
		}

		if check.Detail == "" {
			failures = append(failures, check.Name)
		} else {
			failures = append(failures, check.Name+": "+check.Detail)
		}
	}

	if len(failures) == 0 {
		return nil
	}

	return errors.New(strings.Join(failures, "; "))
}

// DiagnosticsProvider is implemented by providers that can report a structured
// adapter contract and multi-dimensional readiness.
type DiagnosticsProvider interface {
	AdapterDiagnostics() AdapterDiagnostics
}

// ModelMetadata describes the provenance of a static model/context entry.
type ModelMetadata struct {
	ID            string
	Provenance    string
	ReviewedAt    string
	ReviewAfter   string
	Notes         string
	ContextWindow int
}

// ModelMetadataProvider is implemented by providers with auditable static
// model metadata.
type ModelMetadataProvider interface {
	ModelCatalog() []ModelMetadata
	ModelMetadata(model string) (ModelMetadata, bool)
}

// WarningProvider is implemented by providers that need to surface advisory
// warnings even when their normal network health check succeeds.
type WarningProvider interface {
	ProviderWarnings() []string
}

func modelIDsFromMetadata(catalog []ModelMetadata) []string {
	out := make([]string, 0, len(catalog))
	for _, entry := range catalog {
		if entry.ID != "" {
			out = append(out, entry.ID)
		}
	}

	return out
}

func cloneModelMetadata(catalog []ModelMetadata) []ModelMetadata {
	return append([]ModelMetadata(nil), catalog...)
}

func builtinCatalogModelMetadata(providerName, model, reviewedAt, reviewAfter, notes string) (ModelMetadata, bool) {
	metadata, ok := modelroute.BuiltinCatalog().Lookup(providerName, model)
	if !ok {
		return ModelMetadata{}, false
	}

	return ModelMetadata{
		ID:            model,
		ContextWindow: metadata.ContextWindow,
		Provenance:    "built-in provider/model catalog: " + metadata.Source,
		ReviewedAt:    reviewedAt,
		ReviewAfter:   reviewAfter,
		Notes:         notes,
	}, true
}

func readinessStatus(ok bool) ReadinessStatus {
	if ok {
		return ReadinessOK
	}

	return ReadinessFailed
}

func readinessDetail(ok bool, pass, fail string) string {
	if ok {
		return pass
	}

	return fail
}
