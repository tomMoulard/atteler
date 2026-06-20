package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tommoulard/atteler/pkg/autonomy"
	"github.com/tommoulard/atteler/pkg/permission"
	"github.com/tommoulard/atteler/pkg/scout"
)

func scoutCommandRequested(opts cliOptions) bool {
	return strings.TrimSpace(opts.scoutRunPrompt) != ""
}

func scoutSpecificAdjunctOptionsRequested(opts cliOptions) bool {
	return strings.TrimSpace(opts.scoutOutputDir) != "" ||
		strings.TrimSpace(opts.scoutArea) != "" ||
		len(opts.scoutCompetitors) > 0
}

func tournamentOptionsRequested(opts cliOptions) bool {
	return opts.tournament || opts.variants.set
}

func runScoutCommandWithAutonomy(ctx context.Context, cwd string, input scoutCommandInput, level autonomy.Level) error {
	if strings.TrimSpace(input.Prompt) == "" {
		return errors.New("scout run: prompt is required")
	}

	if !autonomy.Normalize(level).Allows(autonomy.ActionFileWrite) {
		return fmt.Errorf("%s", autonomy.DenialMessage(level, autonomy.ActionFileWrite, "scout run"))
	}

	if err := authorizeScoutPermission(ctx, "read scout context", cwd, permission.OperationRead); err != nil {
		return fmt.Errorf("scout run: %w", err)
	}

	writeTarget := scoutWriteTarget(cwd, input.OutputDir)
	if err := authorizeScoutPermission(ctx, "write scout artifacts", writeTarget, permission.OperationWrite); err != nil {
		return fmt.Errorf("scout run: %w", err)
	}

	result, err := scout.Run(ctx, scout.RunRequest{
		Prompt:        input.Prompt,
		Root:          cwd,
		OutputDir:     input.OutputDir,
		Area:          input.Area,
		Competitors:   input.Competitors,
		GenerateTasks: input.GenerateTasks,
		Tournament:    input.Tournament,
		VariantCount:  input.Variants,
	})
	if err != nil {
		return fmt.Errorf("scout run: %w", err)
	}

	fmt.Printf("Scout run %s written to %s\n", result.RunID, result.Dir)

	for _, file := range result.Files {
		fmt.Println(filepath.Join(result.Dir, file))
	}

	return nil
}

func runScoutCommandFromOptions(ctx context.Context, opts cliOptions) error {
	level, err := autonomyForEarlyCommand(opts)
	if err != nil {
		return err
	}

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("locate working directory: %w", err)
	}

	return runScoutCommandWithAutonomy(ctx, cwd, scoutCommandInputFromOptions(opts), level)
}

func scoutWriteTarget(cwd, outputDir string) string {
	outputDir = strings.TrimSpace(outputDir)
	if outputDir == "" {
		return filepath.Join(cwd, ".atteler", "runs", "scout")
	}

	if filepath.IsAbs(outputDir) {
		return filepath.Clean(outputDir)
	}

	return filepath.Join(cwd, filepath.Clean(outputDir))
}

func authorizeScoutPermission(ctx context.Context, action, target string, kinds ...permission.OperationKind) error {
	operations := make([]permission.Operation, 0, len(kinds))
	for _, kind := range kinds {
		operations = append(operations, permission.Operation{
			Kind:   kind,
			Action: action,
			Source: "atteler.scout",
			Target: target,
		})
	}

	decision := permission.Evaluate(ctx, nil, permission.Request{
		Action:     action,
		Source:     "atteler.scout",
		Target:     target,
		Operations: operations,
	})
	if decision.Allowed {
		return nil
	}

	return &permission.Error{Decision: decision}
}
