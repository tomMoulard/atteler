package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/tommoulard/atteler/pkg/autonomy"
)

func applyAutoresearchShortcutOptions(opts *cliOptions) {
	if opts == nil || !opts.autoresearch {
		return
	}

	if !opts.auto.set {
		opts.auto = autoFlag{value: "autoresearch", set: true}
	}

	if !opts.autonomy.set {
		opts.autonomy = autonomyFlag{value: autonomy.High, set: true}
	}

	opts.headless = true
	opts.useWorktree = true
}

func validateAutoresearchCommandSelection(opts cliOptions) error {
	if !opts.autoresearch {
		return nil
	}

	if opts.auto.set && strings.TrimSpace(opts.auto.value) != "autoresearch" {
		return errors.New("--autoresearch cannot be combined with a different --auto mode")
	}

	if opts.oncePrompt == "" && !opts.readStdin {
		return errors.New("--autoresearch requires a mission prompt or --stdin")
	}

	return nil
}

func autoresearchPromptWithTournament(prompt string, opts cliOptions) string {
	if !opts.autoresearch || (!opts.tournament && !opts.variants.set) {
		return prompt
	}

	variants := opts.variants.value
	if variants <= 0 {
		variants = 3
	}

	instruction := fmt.Sprintf(
		"Autoresearch tournament mode: before committing to one edit path, generate %d independent implementation or research %s, evaluate them under the same validation criteria, compare them with the shared pkg/tournament-style ranking primitive, and record why the kept hypothesis won and why alternatives were discarded.",
		variants,
		hypothesisNoun(variants),
	)

	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return instruction
	}

	return prompt + "\n\n" + instruction
}

func hypothesisNoun(count int) string {
	if count == 1 {
		return "hypothesis"
	}

	return "hypotheses"
}
