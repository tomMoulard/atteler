package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/tommoulard/atteler/pkg/agent"
	"github.com/tommoulard/atteler/pkg/llm"
)

const emptyResolutionValue = "<empty>"

func listModels(ctx context.Context, reg *llm.Registry) error {
	providers := reg.ListProviders()
	sort.Strings(providers)

	if len(providers) == 0 {
		return errors.New("no providers registered")
	}

	for _, provider := range providers {
		catalog, err := reg.ProviderModelCatalog(ctx, provider)
		if err != nil {
			return fmt.Errorf("list %s models: %w", provider, err)
		}

		if catalog.Error != nil && catalog.Source == llm.ModelCatalogSourceStatic {
			fmt.Fprintf(os.Stderr, "warning: %s live model fetch failed; using static fallback: %v\n", provider, catalog.Error)
		}

		models := append([]string(nil), catalog.Models...)
		sort.Strings(models)

		for _, model := range models {
			fmt.Println(providerModelListLine(reg, provider, model, catalog.ModelProvenance[model]))
		}
	}

	return nil
}

func providerModelListLine(reg *llm.Registry, provider, model string, provenance llm.ModelProvenance) string {
	if provenance != llm.ModelProvenanceConfiguredAlias {
		return provider + "/" + model
	}

	diagnostic := reg.ExplainModelResolution(model)
	for _, candidate := range diagnostic.Candidates {
		if candidate.ProviderName != provider || candidate.Provenance != llm.ModelProvenanceConfiguredAlias {
			continue
		}

		return model + " -> " + provider + "/" + candidate.Model
	}

	return model + " -> " + provider + "/<unknown>"
}

func explainModelResolution(ctx context.Context, model string, reg *llm.Registry) error {
	if reg == nil {
		return errors.New("model resolution: no registry available")
	}

	if err := refreshModelResolutionCatalogs(ctx, reg); err != nil {
		return err
	}

	diagnostic := reg.ExplainModelResolution(model)
	printModelResolutionDiagnostic(diagnostic)

	if diagnostic.Error != nil {
		return diagnostic.Error
	}

	return nil
}

func printModelResolutionDiagnostic(diagnostic llm.ModelResolutionDiagnostic) {
	fmt.Println("Model resolution")
	fmt.Printf("  requested: %s\n", modelResolutionValue(diagnostic.RequestedModel))

	if diagnostic.Resolved() {
		printResolvedModelResolution(diagnostic)
	} else {
		fmt.Println("  status: unresolved")

		if diagnostic.Error != nil {
			fmt.Printf("  error: %v\n", diagnostic.Error)
		}
	}

	printModelResolutionDefault(diagnostic)

	if diagnostic.Reason != "" {
		fmt.Printf("  reason: %s\n", diagnostic.Reason)
	}

	printModelResolutionCandidates(diagnostic.Candidates)
}

func printResolvedModelResolution(diagnostic llm.ModelResolutionDiagnostic) {
	fmt.Printf("  provider: %s\n", diagnostic.ProviderName)
	fmt.Printf("  provider_model: %s\n", modelResolutionValue(diagnostic.ProviderModel))

	if diagnostic.Provenance != "" {
		fmt.Printf("  provenance: %s\n", diagnostic.Provenance)
	}

	if diagnostic.Stale {
		fmt.Println("  stale: true")
	}
}

func printModelResolutionDefault(diagnostic llm.ModelResolutionDiagnostic) {
	if diagnostic.DefaultProvider != "" {
		defaultKind := "auto"
		if diagnostic.DefaultProviderConfigured {
			defaultKind = "configured"
		}

		fmt.Printf("  default_provider: %s (%s)\n", diagnostic.DefaultProvider, defaultKind)
	}
}

func printModelResolutionCandidates(candidates []llm.ModelResolutionCandidate) {
	if len(candidates) == 0 {
		return
	}

	fmt.Println("  candidates:")

	for _, candidate := range candidates {
		fmt.Printf("    - %s/%s%s\n", candidate.ProviderName, candidate.Model, modelResolutionCandidateSuffix(candidate))
	}
}

func modelResolutionCandidateSuffix(candidate llm.ModelResolutionCandidate) string {
	var parts []string
	if candidate.Provenance != "" {
		parts = append(parts, string(candidate.Provenance))
	}

	if candidate.Stale {
		parts = append(parts, "stale")
	}

	if len(parts) == 0 {
		return ""
	}

	return " [" + strings.Join(parts, " ") + "]"
}

func refreshModelResolutionCatalogs(ctx context.Context, reg *llm.Registry) error {
	providers := reg.ListProviders()
	sort.Strings(providers)

	for _, provider := range providers {
		catalog, err := reg.ProviderModelCatalog(ctx, provider)
		if err != nil {
			return fmt.Errorf("refresh %s models for resolution: %w", provider, err)
		}

		if catalog.Error != nil && catalog.Source == llm.ModelCatalogSourceStatic {
			fmt.Fprintf(os.Stderr, "warning: %s live model fetch failed; using static fallback: %v\n", provider, catalog.Error)
		}
	}

	return nil
}

func modelResolutionValue(value string) string {
	if value == "" {
		return emptyResolutionValue
	}

	return value
}

func listAgents(agents *agent.Registry) {
	for _, name := range agents.List() {
		fmt.Println(name)
	}
}
