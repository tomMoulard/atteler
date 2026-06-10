package main

import (
	"context"
	"fmt"

	appconfig "github.com/tommoulard/atteler/pkg/config"
	"github.com/tommoulard/atteler/pkg/permission"
)

func migrateConfigAndState(ctx context.Context) error {
	if err := authorizeConfigMigration(ctx); err != nil {
		return err
	}

	configResults, err := appconfig.MigratePathSources(appconfig.DefaultPathSources())
	if err != nil {
		return fmt.Errorf("config migrate: %w", err)
	}

	stateStore := appconfig.NewStateStore("")

	stateChanged, state, err := stateStore.Migrate()
	if err != nil {
		return fmt.Errorf("state migrate %s: %w", stateStore.Path(), err)
	}

	fmt.Println("Config/state migration")

	if len(configResults) == 0 {
		fmt.Println("config: no config files found")
	} else {
		for _, result := range configResults {
			status := configPathStatusUpToDate
			if result.Changed {
				status = "migrated"
			}

			fmt.Printf("config: %s (%s)\n", result.Path, status)
		}
	}

	stateStatus := configPathStatusMissing
	if state.Version > 0 {
		stateStatus = configPathStatusUpToDate
	}

	if stateChanged {
		stateStatus = "migrated"
	}

	fmt.Printf("state: %s (%s)\n", stateStore.Path(), stateStatus)

	return nil
}

func authorizeConfigMigration(ctx context.Context) error {
	const (
		action = "migrate Atteler config/state"
		source = "atteler.config.migrate"
		target = "config/state"
	)

	decision := permission.Evaluate(ctx, nil, permission.Request{
		Action: action,
		Source: source,
		Target: target,
		Operations: []permission.Operation{
			{
				Kind:   permission.OperationRead,
				Action: "inspect Atteler config/state before migration",
				Source: source,
				Target: target,
			},
			{
				Kind:   permission.OperationWrite,
				Action: action,
				Source: source,
				Target: target,
			},
		},
	})
	if decision.Allowed {
		return nil
	}

	return &permission.Error{Decision: decision}
}
