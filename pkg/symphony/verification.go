package symphony

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tommoulard/atteler/pkg/privacy"
	"github.com/tommoulard/atteler/pkg/shell"
)

// VerificationGateError reports local verification failures that are configured
// to block publication instead of opening a draft pull request.
type VerificationGateError struct {
	Report VerificationReport
}

func (e *VerificationGateError) Error() string {
	if e == nil {
		return ""
	}

	failed := trimNonEmptyStrings(e.Report.FailedRequired)
	if len(failed) == 0 {
		return "publish: verification gates failed"
	}

	return "publish: required verification gate(s) failed: " + strings.Join(redactVerificationNames(failed), ", ")
}

func runVerificationGates(ctx context.Context, cfg Config, issue Issue, workspace Workspace) (report VerificationReport, err error) {
	if ctx == nil {
		return VerificationReport{}, errors.New("publish verification: context is required")
	}

	report = VerificationReport{
		StartedAt:  time.Now().UTC(),
		Configured: len(cfg.Publish.VerificationGates) > 0,
		Passed:     true,
	}

	defer func() {
		report.CompletedAt = time.Now().UTC()
	}()

	if len(cfg.Publish.VerificationGates) == 0 {
		return report, nil
	}

	if strings.TrimSpace(workspace.Path) == "" {
		return report, errors.New("publish verification: workspace path is required")
	}

	for _, gate := range cfg.Publish.VerificationGates {
		if err := ctx.Err(); err != nil {
			return report, fmt.Errorf("publish verification canceled: %w", err)
		}

		result := runVerificationGate(ctx, cfg, issue, workspace, gate)
		report.Gates = append(report.Gates, result)

		if result.Required && result.Status != VerificationPassed {
			report.FailedRequired = append(report.FailedRequired, result.Name)
			report.Passed = false
		}

		if err := ctx.Err(); err != nil {
			return report, fmt.Errorf("publish verification canceled: %w", err)
		}
	}

	return report, nil
}

func runVerificationGate(ctx context.Context, cfg Config, issue Issue, workspace Workspace, gate VerificationGateConfig) VerificationGateResult {
	started := time.Now().UTC()
	command := strings.TrimSpace(gate.Command)
	result := VerificationGateResult{
		StartedAt: started,
		Name:      privacy.RedactText(strings.TrimSpace(gate.Name)),
		Command:   privacy.RedactText(command),
		Required:  gate.Required,
		Status:    VerificationPassed,
	}

	maxOutputBytes := cfg.Publish.VerificationOutputMaxBytes
	if maxOutputBytes <= 0 {
		maxOutputBytes = defaultPRGateOutputBytes
	}

	runResult, err := shell.RunBash(ctx, shell.Options{
		Command:        command,
		Dir:            workspace.Path,
		Timeout:        verificationGateTimeout(gate),
		MaxOutputBytes: maxOutputBytes,
		Policy: &shell.Policy{
			AllowCommands: append([]string(nil), cfg.Publish.VerificationAllowCommands...),
			DenyCommands:  append([]string(nil), cfg.Publish.VerificationDenyCommands...),
			DenyNetwork:   true,
		},
		Audit: symphonyIssueAudit("symphony.verification", issue, cfg.Autonomy),
	})

	result.CompletedAt = time.Now().UTC()
	result.Duration = result.CompletedAt.Sub(started)
	result.Stdout = strings.TrimSpace(privacy.RedactText(runResult.Stdout))
	result.Stderr = strings.TrimSpace(privacy.RedactText(runResult.Stderr))
	result.OutputTruncated = runResult.OutputTruncated

	if err != nil {
		result.Status = VerificationFailed
		result.Error = privacy.RedactText(err.Error())

		if runResult.Duration > 0 {
			result.Duration = runResult.Duration
		}
	}

	return result
}

func verificationGateTimeout(gate VerificationGateConfig) time.Duration {
	if gate.Timeout > 0 {
		return gate.Timeout
	}

	return defaultPRGateTimeout
}
