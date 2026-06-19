package main

import (
	"errors"
	"strings"

	"github.com/tommoulard/atteler/pkg/autonomy"
	"github.com/tommoulard/atteler/pkg/tournament"
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

	if tournamentOptionsRequested(*opts) && (strings.TrimSpace(opts.oncePrompt) != "" || opts.readStdin) {
		options := tournament.Normalize(opts.tournament, opts.variants.value)
		opts.oncePrompt = strings.TrimSpace(opts.oncePrompt + tournament.AutoresearchInstruction(options))
	}
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
