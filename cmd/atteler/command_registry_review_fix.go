package main

import "errors"

const reviewFixValidateFlagName = "validate"

func reviewFixSupplementalFlagsSet(opts cliOptions) bool {
	return opts.reviewFixFrom != "" ||
		opts.reviewFixPR != "" ||
		len(opts.reviewFixValidationCommands) > 0
}

func validateReviewFixCommandSelection(opts cliOptions) error {
	if reviewFixSupplementalFlagsSet(opts) && !opts.reviewFix {
		return errors.New("review fix flags require --review-fix or `atteler review fix`")
	}

	if opts.reviewFix && opts.reviewFixFrom == "" && opts.reviewFixPR == "" {
		return errors.New("review fix requires --from for Atteler-native review findings or --pr for a future GitHub source")
	}

	if opts.reviewFix && opts.reviewFixFrom != "" && opts.reviewFixPR != "" {
		return errors.New("review fix accepts only one input source: choose --from or --pr")
	}

	return nil
}
