package main

import (
	"fmt"

	appconfig "github.com/tommoulard/atteler/pkg/config"
)

func migrateConfigAndState() error {
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
