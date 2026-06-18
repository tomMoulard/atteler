package main

import "errors"

func validateCLICommandSelection(opts cliOptions) error {
	if err := validateAutoresearchCommandSelection(opts); err != nil {
		return err
	}

	if opts.headlessID != "" && !opts.headless {
		return errors.New("--headless-id requires --headless")
	}

	if opts.headlessPrivateLog && !opts.headless {
		return errors.New("--headless-private-log requires --headless")
	}

	if err := validateIncidentCommandSelection(opts); err != nil {
		return err
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

	matches := matchingRegistryCommands(commandRegistry, tierAny, opts)
	inlineCommands := buildInlineCommandRegistry()

	matches = append(matches, matchingRegistryCommands(inlineCommands, tierInline, opts)...)
	if _, err := resolveCommandAmbiguity(matches); err != nil {
		return err
	}

	return validateGroupedCommandSelection(opts)
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
