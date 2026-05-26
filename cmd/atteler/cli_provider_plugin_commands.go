package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"

	"github.com/tommoulard/atteler/pkg/agent"
	"github.com/tommoulard/atteler/pkg/llm"
)

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
			fmt.Println(provider + "/" + model)
		}
	}

	return nil
}

func listAgents(agents *agent.Registry) {
	for _, name := range agents.List() {
		fmt.Println(name)
	}
}
