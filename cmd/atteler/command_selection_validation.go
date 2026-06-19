package main

import "errors"

func validateCLICommandSelection(opts cliOptions) error {
	if err := validateAutoresearchCommandSelection(opts); err != nil {
		return err
	}

	if err := validateResearchCommandSelection(opts); err != nil {
		return err
	}

	if err := validateIncidentCommandSelection(opts); err != nil {
		return err
	}

	if err := validateHeadlessCommandSelection(opts); err != nil {
		return err
	}

	matches := matchingRegistryCommands(commandRegistry, tierAny, opts)
	inlineCommands := buildInlineCommandRegistry()

	matches = append(matches, matchingRegistryCommands(inlineCommands, tierInline, opts)...)
	if _, err := resolveCommandAmbiguity(matches); err != nil {
		return err
	}

	return validateGroupedCommandSelection(opts)
}

func validateHeadlessCommandSelection(opts cliOptions) error {
	if opts.headlessID != "" && !opts.headless {
		return errors.New("--headless-id requires --headless")
	}

	if opts.headlessPrivateLog && !opts.headless {
		return errors.New("--headless-private-log requires --headless")
	}

	if opts.retryHeadlessNewID != "" && opts.retryHeadlessID == "" {
		return errors.New("--retry-headless-id requires --retry-headless")
	}

	if opts.headlessStatusFilter != "" && !opts.listHeadless {
		return errors.New("--headless-status requires --list-headless")
	}

	if opts.headlessMaxAge != "" && !opts.listHeadless && !opts.cleanupHeadless {
		return errors.New("--headless-max-age requires --list-headless or --cleanup-headless")
	}

	return nil
}

func validateResearchCommandSelection(opts cliOptions) error {
	if researchAdjunctOptionsRequested(opts) && !researchCommandRequested(opts) {
		return errors.New("--trusted-source, --research-source, --research-output, and --generate-tasks require --research-run")
	}

	return nil
}

func validateGroupedCommandSelection(opts cliOptions) error {
	if _, err := selectCodeIntelCommand(codeIntelCommandInputFromOptions(opts)); err != nil {
		return err
	}

	if _, err := selectStatefulSessionReadCommand(sessionReadCommandInputFromOptions(opts)); err != nil {
		return err
	}

	if _, err := selectStatefulSessionWriteCommand(sessionWriteCommandInputFromOptions(opts)); err != nil {
		return err
	}

	return nil
}
