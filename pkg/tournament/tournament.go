// Package tournament contains small shared primitives for running multiple
// independent proposal or hypothesis variants and then adjudicating them.
package tournament

import (
	"fmt"
	"strings"
)

const (
	// DefaultVariants is the count used when tournament mode is requested
	// without an explicit --variants value.
	DefaultVariants = 3
	// MaxVariants caps accidental fan-out from CLI flags before a provider-backed
	// implementation exists.
	MaxVariants = 12
)

// Options describes a variant/tournament request in a workflow-neutral shape.
type Options struct {
	Variants int  `json:"variants"`
	Enabled  bool `json:"enabled"`
}

// Normalize returns bounded tournament options. Supplying more than one variant
// activates the tournament even when Enabled is false.
func Normalize(enabled bool, variants int) Options {
	if variants < 0 {
		variants = 0
	}

	active := enabled || variants > 1
	if active && variants == 0 {
		variants = DefaultVariants
	}

	if !active && variants == 0 {
		variants = 1
	}

	if variants > MaxVariants {
		variants = MaxVariants
	}

	return Options{Enabled: active, Variants: variants}
}

// Active reports whether multiple independent candidates should be generated or
// adjudicated.
func (o Options) Active() bool {
	return o.Enabled || o.Variants > 1
}

// Count returns the normalized variant count, defaulting to one outside active
// tournament mode.
func (o Options) Count() int {
	if o.Variants <= 0 {
		if o.Active() {
			return DefaultVariants
		}

		return 1
	}

	if o.Variants > MaxVariants {
		return MaxVariants
	}

	return o.Variants
}

// AutoresearchInstruction renders a reusable prompt appendix for the existing
// autoresearch loop. Scout uses the same Options type for roadmap variants, so
// tournament behavior stays a shared capability instead of a one-off command.
func AutoresearchInstruction(options Options) string {
	options = Normalize(options.Enabled, options.Variants)
	if !options.Active() {
		return ""
	}

	return fmt.Sprintf(`

Tournament mode requested: before choosing implementation edits, form %d independent research or implementation hypotheses under the same evaluator. Compare them with the baseline criteria, record why each hypothesis is kept or discarded, then advance only the strongest validated hypothesis. Keep the normal autoresearch safety rules: small changes, fixed validation commands, durable ledger entries, and reset discarded regressions.`, options.Count())
}

// VariantID returns a stable one-based identifier such as V1.
func VariantID(index int) string {
	if index < 1 {
		index = 1
	}

	return fmt.Sprintf("V%d", index)
}

// NormalizeLens trims a human-readable lens and falls back to the variant ID.
func NormalizeLens(lens string, index int) string {
	lens = strings.TrimSpace(lens)
	if lens != "" {
		return lens
	}

	return "Variant " + VariantID(index)
}
