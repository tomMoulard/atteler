package main

import "errors"

func incidentSupplementalFlagsSet(opts cliOptions) bool {
	return firstNonEmpty(
		opts.sentryIssue,
		opts.incidentReference,
		opts.incidentFilePath,
		opts.incidentSentryOrg,
		opts.incidentSentryBaseURL,
		opts.incidentSentryTokenEnv,
		opts.incidentSentryEventID,
		opts.incidentMCPManifestPath,
		opts.incidentMCPServerName,
		opts.incidentMCPToolName,
		opts.incidentMCPToolArgsJSON,
		opts.incidentReproCommand,
		opts.incidentReportPath,
		opts.incidentPRBodyPath,
	) != "" ||
		len(opts.incidentValidationCommands) > 0 ||
		opts.incidentTimeout.set ||
		opts.incidentApplyFix ||
		opts.incidentOpenPR
}

func validateIncidentCommandSelection(opts cliOptions) error {
	if incidentSupplementalFlagsSet(opts) && !opts.incidentDiagnose {
		return errors.New("incident flags require --incident-diagnose or `atteler incident diagnose`")
	}

	if opts.incidentDiagnose && opts.incidentOpenPR && !opts.incidentApplyFix {
		return errors.New("--incident-open-pr requires --incident-apply-fix so the PR is created only after a repair attempt; use --incident-pr-body for a diagnose-only PR template")
	}

	if opts.incidentDiagnose && opts.incidentOpenPR && len(opts.incidentValidationCommands) == 0 {
		return errors.New("--incident-open-pr requires at least one --incident-validation-command so the PR includes harness-captured validation evidence")
	}

	if opts.incidentDiagnose && opts.incidentApplyFix {
		if _, err := incidentDiagnosisOutputFormat(incidentDiagnoseCommandInputFromOptions(opts)); err != nil {
			return err
		}
	}

	return nil
}
