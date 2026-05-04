// Package skill detects repeated action sequences that can be promoted into
// reusable skill suggestions.
package skill

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

const (
	defaultMinSteps       = 2
	defaultMaxSteps       = 6
	defaultMinOccurrences = 2
)

var separators = regexp.MustCompile(`[^a-z0-9]+`)

// Options controls how repeated action sequences are detected.
type Options struct {
	// MinSteps is the shortest multi-step sequence to consider. Values below 2
	// are treated as 2 because a skill suggestion must contain multiple steps.
	MinSteps int
	// MaxSteps is the longest sequence to consider. If unset, a small default is
	// used so suggestions stay focused and explainable.
	MaxSteps int
	// MinOccurrences is the minimum number of non-overlapping repetitions needed.
	// Values below 2 are treated as 2.
	MinOccurrences int
}

// Suggestion describes a repeated workflow that could become a named skill.
//
//nolint:govet // Public field order follows readability and output semantics.
type Suggestion struct {
	// Name is a human-readable skill name derived from the detected steps.
	Name string
	// Slug is a stable lowercase identifier derived from the detected steps.
	Slug string
	// Steps is the normalized command/action sequence that repeated.
	Steps []string
	// Occurrences is the number of non-overlapping times Steps appeared.
	Occurrences int
	// Rationale explains why this sequence was suggested.
	Rationale string
}

type candidate struct {
	steps       []string
	occurrences int
	firstIndex  int
}

// Suggest returns the strongest repeated multi-step sequence in observed.
// Empty action names are ignored. The boolean return is false when there is no
// sequence that satisfies the default thresholds.
func Suggest(observed []string) (Suggestion, bool) {
	return SuggestWithOptions(observed, Options{})
}

// SuggestWithOptions returns the strongest repeated multi-step sequence in
// observed using opts. Strength favors workflows with more repeated work
// (steps × occurrences), then longer step sequences, then earlier appearance.
func SuggestWithOptions(observed []string, opts Options) (Suggestion, bool) {
	opts = normalizeOptions(opts)

	actions := normalizeActions(observed)
	if len(actions) < opts.MinSteps*opts.MinOccurrences {
		return Suggestion{}, false
	}

	if opts.MaxSteps > len(actions)/opts.MinOccurrences {
		opts.MaxSteps = len(actions) / opts.MinOccurrences
	}

	var best candidate

	found := false

	for size := opts.MinSteps; size <= opts.MaxSteps; size++ {
		startsByKey := make(map[string][]int)
		stepsByKey := make(map[string][]string)

		for start := 0; start+size <= len(actions); start++ {
			steps := actions[start : start+size]
			key := strings.Join(steps, "\x00")

			startsByKey[key] = append(startsByKey[key], start)
			if _, ok := stepsByKey[key]; !ok {
				stepsByKey[key] = append([]string(nil), steps...)
			}
		}

		for key, starts := range startsByKey {
			occurrences := countNonOverlapping(starts, size)
			if occurrences < opts.MinOccurrences {
				continue
			}

			cand := candidate{steps: stepsByKey[key], occurrences: occurrences, firstIndex: starts[0]}
			if !found || better(cand, best) {
				best = cand
				found = true
			}
		}
	}

	if !found {
		return Suggestion{}, false
	}

	return buildSuggestion(best), true
}

func normalizeOptions(opts Options) Options {
	if opts.MinSteps < defaultMinSteps {
		opts.MinSteps = defaultMinSteps
	}

	if opts.MaxSteps <= 0 {
		opts.MaxSteps = defaultMaxSteps
	}

	if opts.MaxSteps < opts.MinSteps {
		opts.MaxSteps = opts.MinSteps
	}

	if opts.MinOccurrences < defaultMinOccurrences {
		opts.MinOccurrences = defaultMinOccurrences
	}

	return opts
}

func normalizeActions(observed []string) []string {
	actions := make([]string, 0, len(observed))
	for _, action := range observed {
		normalized := normalizeStep(action)
		if normalized == "" {
			continue
		}

		actions = append(actions, normalized)
	}

	return actions
}

func normalizeStep(action string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(action))), " ")
}

func countNonOverlapping(starts []int, size int) int {
	count := 0

	nextAllowed := 0
	for _, start := range starts {
		if start < nextAllowed {
			continue
		}

		count++
		nextAllowed = start + size
	}

	return count
}

func better(cand, best candidate) bool {
	candScore := len(cand.steps) * cand.occurrences

	bestScore := len(best.steps) * best.occurrences
	if candScore != bestScore {
		return candScore > bestScore
	}

	if len(cand.steps) != len(best.steps) {
		return len(cand.steps) > len(best.steps)
	}

	if cand.occurrences != best.occurrences {
		return cand.occurrences > best.occurrences
	}

	return cand.firstIndex < best.firstIndex
}

func buildSuggestion(c candidate) Suggestion {
	slug := slugForSteps(c.steps)

	return Suggestion{
		Name:        nameFromSlug(slug),
		Slug:        slug,
		Steps:       append([]string(nil), c.steps...),
		Occurrences: c.occurrences,
		Rationale: fmt.Sprintf(
			"Observed the %d-step sequence %q repeat %d times, making it a good candidate for a reusable skill.",
			len(c.steps), strings.Join(c.steps, " → "), c.occurrences,
		),
	}
}

func slugForSteps(steps []string) string {
	parts := make([]string, 0, len(steps))
	for _, step := range steps {
		part := separators.ReplaceAllString(strings.ToLower(step), "-")

		part = strings.Trim(part, "-")
		if part != "" {
			parts = append(parts, part)
		}
	}

	return strings.Join(parts, "-")
}

func nameFromSlug(slug string) string {
	words := strings.Split(slug, "-")
	for i, word := range words {
		if word == "" {
			continue
		}

		letters := []rune(word)
		letters[0] = unicode.ToUpper(letters[0])
		words[i] = string(letters)
	}

	return strings.Join(words, " ") + " Skill"
}
