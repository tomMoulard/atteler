package main

import (
	"context"
	"fmt"

	"gopkg.in/yaml.v3"

	appconfig "github.com/tommoulard/atteler/pkg/config"
)

func printConfigReport(ctx context.Context) error {
	if err := authorizeConfigStackRead(ctx, "build config diagnostics report", "atteler.config.report"); err != nil {
		return fmt.Errorf("config report: %w", err)
	}

	statePath := appconfig.DefaultStatePath()
	if err := authorizeStateFileRead(ctx, "inspect state diagnostics", "atteler.config.report", statePath); err != nil {
		return fmt.Errorf("config report: %w", err)
	}

	report := appconfig.NewDefaultDiagnosticsReport()

	out, err := yaml.Marshal(report)
	if err != nil {
		return fmt.Errorf("config report: marshal diagnostics: %w", err)
	}

	fmt.Print(string(out))

	return nil
}
