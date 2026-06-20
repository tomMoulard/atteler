package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/tommoulard/atteler/pkg/autonomy"
	"github.com/tommoulard/atteler/pkg/permission"
	"github.com/tommoulard/atteler/pkg/research"
)

func researchCommandRequested(opts cliOptions) bool {
	return strings.TrimSpace(opts.researchRunQuestion) != ""
}

func researchAdjunctOptionsRequested(opts cliOptions) bool {
	return strings.TrimSpace(opts.researchOutputDir) != "" ||
		len(opts.trustedSources) > 0 ||
		len(opts.researchSources) > 0
}

func runResearchCommandWithAutonomy(ctx context.Context, cwd string, input researchCommandInput, level autonomy.Level) error {
	if strings.TrimSpace(input.Question) == "" {
		return errors.New("research run: question is required")
	}

	if !autonomy.Normalize(level).Allows(autonomy.ActionFileWrite) {
		return fmt.Errorf("%s", autonomy.DenialMessage(level, autonomy.ActionFileWrite, "research run"))
	}

	if err := authorizeResearchPermission(ctx, "read research context", cwd, permission.OperationRead); err != nil {
		return fmt.Errorf("research run: %w", err)
	}

	writeTarget := researchWriteTarget(cwd, input.OutputDir)
	if err := authorizeResearchPermission(ctx, "write research artifacts", writeTarget, permission.OperationWrite); err != nil {
		return fmt.Errorf("research run: %w", err)
	}

	result, err := research.Run(ctx, research.RunRequest{
		Question:       input.Question,
		Root:           cwd,
		OutputDir:      input.OutputDir,
		TrustedSources: input.TrustedSources,
		Sources:        input.Sources,
		GenerateTasks:  input.GenerateTasks,
	})
	if err != nil {
		return fmt.Errorf("research run: %w", err)
	}

	fmt.Printf("Research run %s written to %s\n", result.RunID, result.Dir)

	for _, file := range result.Files {
		fmt.Println(filepath.Join(result.Dir, file))
	}

	return nil
}

func researchWriteTarget(cwd, outputDir string) string {
	outputDir = strings.TrimSpace(outputDir)
	if outputDir == "" {
		return filepath.Join(cwd, ".atteler", "runs", "research")
	}

	if filepath.IsAbs(outputDir) {
		return filepath.Clean(outputDir)
	}

	return filepath.Join(cwd, filepath.Clean(outputDir))
}

func authorizeResearchPermission(ctx context.Context, action, target string, kinds ...permission.OperationKind) error {
	operations := make([]permission.Operation, 0, len(kinds))
	for _, kind := range kinds {
		operations = append(operations, permission.Operation{
			Kind:   kind,
			Action: action,
			Source: "atteler.research",
			Target: target,
		})
	}

	decision := permission.Evaluate(ctx, nil, permission.Request{
		Action:     action,
		Source:     "atteler.research",
		Target:     target,
		Operations: operations,
	})
	if decision.Allowed {
		return nil
	}

	return &permission.Error{Decision: decision}
}
