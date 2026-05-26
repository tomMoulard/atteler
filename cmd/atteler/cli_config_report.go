package main

import (
	"fmt"

	"gopkg.in/yaml.v3"

	appconfig "github.com/tommoulard/atteler/pkg/config"
)

func printConfigReport() error {
	report := appconfig.NewDefaultDiagnosticsReport()

	out, err := yaml.Marshal(report)
	if err != nil {
		return fmt.Errorf("config report: marshal diagnostics: %w", err)
	}

	fmt.Print(string(out))

	return nil
}
