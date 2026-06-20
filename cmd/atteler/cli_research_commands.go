package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/tommoulard/atteler/pkg/autonomy"
	appconfig "github.com/tommoulard/atteler/pkg/config"
	"github.com/tommoulard/atteler/pkg/permission"
	"github.com/tommoulard/atteler/pkg/research"
	"github.com/tommoulard/atteler/pkg/sourcepolicy"
)

func researchCommandRequested(opts cliOptions) bool {
	return strings.TrimSpace(opts.researchRunQuestion) != ""
}

func researchOnlyAdjunctOptionsRequested(opts cliOptions) bool {
	return strings.TrimSpace(opts.researchOutputDir) != "" ||
		len(opts.researchSources) > 0 ||
		opts.researchGenerateTasks
}

func sourcePolicyAdjunctOptionsRequested(opts cliOptions) bool {
	return len(opts.trustedSources) > 0 ||
		len(opts.deniedSources) > 0 ||
		opts.warnLowTrustSources
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

	cfg, _, err := loadConfigWithPermission(
		ctx,
		"load research source policy config",
		"atteler.research",
		"load research config",
	)
	if err != nil {
		return err
	}

	result, err := research.Run(ctx, research.RunRequest{
		Question:       input.Question,
		Root:           cwd,
		OutputDir:      input.OutputDir,
		TrustedSources: input.TrustedSources,
		DeniedSources:  input.DeniedSources,
		SourcePolicy:   researchSourcePolicyFromInput(cfg, input),
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

func researchSourcePolicyFromInput(cfg appconfig.Config, input researchCommandInput) sourcepolicy.Policy {
	return sourcePolicyFromFlagInputs(
		cfg.Research.SourcePolicy,
		input.TrustedSources,
		input.DeniedSources,
		input.WarnLowTrust,
	)
}

func sourcePolicyFromFlagInputs(base sourcepolicy.Policy, trusted, denied []string, warnLowTrust bool) sourcepolicy.Policy {
	policy := sourcepolicy.Clone(base)

	trustedSources := sourcepolicy.NormalizeDomains(trusted)
	if len(trustedSources) > 0 {
		policy.DeniedDomains = sourcepolicy.RemoveDomains(policy.DeniedDomains, trustedSources)
		policy = sourcepolicy.Extend(policy, sourcepolicy.Policy{TrustedDomains: trustedSources})
	}

	deniedSources := sourcepolicy.NormalizeDomains(denied)
	if len(deniedSources) > 0 {
		policy.TrustedDomains = sourcepolicy.RemoveDomains(policy.TrustedDomains, deniedSources)
		policy = sourcepolicy.Extend(policy, sourcepolicy.Policy{DeniedDomains: deniedSources})
	}

	if warnLowTrust {
		policy = sourcepolicy.Extend(policy, sourcepolicy.Policy{WarnOnLowTrustSources: sourcepolicy.Bool(true)})
	}

	return policy
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
