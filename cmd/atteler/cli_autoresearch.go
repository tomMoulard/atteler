package main

import (
	"errors"
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
