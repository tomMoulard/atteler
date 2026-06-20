// Package tournament provides dependency-free primitives for comparing
// independent candidate ideas, roadmaps, hypotheses, or proposals.
//
//nolint:wsl_v5 // Scoring and ranking logic keeps related statements adjacent for auditability.
package tournament

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"unicode"
)

// Candidate is one proposal produced by a variant, agent, or hypothesis lane.
//
// The type is intentionally generic so product-discovery workflows such as
// scout and implementation-discovery workflows such as autoresearch can share
// the same comparison step without depending on each other's artifact schemas.
type Candidate struct {
	ID            string
	Title         string
	Summary       string
	Variant       string
	Fit           int
	Feasibility   int
	Evidence      int
	RiskPenalty   int
	OriginalIndex int
}

// Score returns the deterministic tournament score for the candidate.
func (c Candidate) Score() int {
	return c.Fit + c.Feasibility + c.Evidence - c.RiskPenalty
}

// Variant groups independently produced candidates under one lens.
type Variant struct {
	ID         string
	Name       string
	Lens       string
	Candidates []Candidate
}

// RankedCandidate is a merged, ranked candidate with provenance from all
// variants that proposed a semantically matching title.
//
//nolint:govet // Public field order keeps rank/score metadata adjacent to candidate content.
type RankedCandidate struct {
	Candidate
	SourceVariants []string
	Decision       string
	Rationale      string
	Rank           int
	Score          int
}

// Result is the comparison output across all variants.
type Result struct {
	Ranked    []RankedCandidate
	Kept      []RankedCandidate
	Discarded []RankedCandidate
	Variants  []Variant
}

// Options controls tournament comparison.
type Options struct {
	// KeepTop is the number of top-ranked merged candidates to mark as kept.
	// A zero or negative value keeps every ranked candidate.
	KeepTop int
}

// Merge deduplicates, scores, ranks, and marks candidate decisions.
//
//nolint:cyclop,gocognit // Merge intentionally keeps the scoring and dedupe algorithm in one auditable pass.
func Merge(variants []Variant, opts Options) (Result, error) {
	if len(variants) == 0 {
		return Result{}, errors.New("tournament: at least one variant is required")
	}

	working := cloneVariants(variants)
	merged := make(map[string]RankedCandidate)
	order := make(map[string]int)

	for variantIndex := range working {
		variant := normalizeVariant(working[variantIndex], variantIndex)
		if len(variant.Candidates) == 0 {
			return Result{}, fmt.Errorf("tournament: variant %q has no candidates", variant.ID)
		}
		working[variantIndex] = variant

		for candidateIndex := range variant.Candidates {
			candidate := normalizeCandidate(variant.Candidates[candidateIndex], variant, candidateIndex)
			if candidate.Title == "" {
				return Result{}, fmt.Errorf("tournament: variant %q candidate %d has empty title", variant.ID, candidateIndex+1)
			}
			variant.Candidates[candidateIndex] = candidate

			key := candidateKey(candidate.Title)
			current, exists := merged[key]
			if !exists {
				current = RankedCandidate{
					Candidate:      candidate,
					Score:          candidate.Score(),
					SourceVariants: []string{variant.ID},
				}
				order[key] = len(order)
				merged[key] = current

				continue
			}

			current.Score += candidate.Score()
			current.Fit = max(current.Fit, candidate.Fit)
			current.Feasibility = max(current.Feasibility, candidate.Feasibility)
			current.Evidence += candidate.Evidence
			current.RiskPenalty = minPositive(current.RiskPenalty, candidate.RiskPenalty)
			current.SourceVariants = appendUnique(current.SourceVariants, variant.ID)
			if len(candidate.Summary) > len(current.Summary) {
				current.Summary = candidate.Summary
			}
			merged[key] = current
		}
		working[variantIndex] = variant
	}

	ranked := make([]RankedCandidate, 0, len(merged))
	for key := range merged {
		candidate := merged[key]
		candidate.Score += len(candidate.SourceVariants) - 1
		candidate.Rationale = rationaleFor(candidate)
		ranked = append(ranked, candidate)
	}

	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].Score != ranked[j].Score {
			return ranked[i].Score > ranked[j].Score
		}

		if len(ranked[i].SourceVariants) != len(ranked[j].SourceVariants) {
			return len(ranked[i].SourceVariants) > len(ranked[j].SourceVariants)
		}

		left := order[candidateKey(ranked[i].Title)]
		right := order[candidateKey(ranked[j].Title)]
		if left != right {
			return left < right
		}

		return ranked[i].Title < ranked[j].Title
	})

	keepTop := opts.KeepTop
	if keepTop <= 0 || keepTop > len(ranked) {
		keepTop = len(ranked)
	}

	kept := make([]RankedCandidate, 0, keepTop)
	discarded := make([]RankedCandidate, 0, max(len(ranked)-keepTop, 0))
	for i := range ranked {
		ranked[i].Rank = i + 1
		if i < keepTop {
			ranked[i].Decision = "kept"
			kept = append(kept, ranked[i])
			continue
		}

		ranked[i].Decision = "discarded"
		ranked[i].Rationale = "lower aggregate score than the kept recommendations"
		discarded = append(discarded, ranked[i])
	}

	return Result{
		Variants:  cloneVariants(working),
		Ranked:    ranked,
		Kept:      kept,
		Discarded: discarded,
	}, nil
}

func normalizeVariant(variant Variant, index int) Variant {
	variant.ID = strings.TrimSpace(variant.ID)
	if variant.ID == "" {
		variant.ID = fmt.Sprintf("variant-%d", index+1)
	}

	variant.Name = strings.TrimSpace(variant.Name)
	if variant.Name == "" {
		variant.Name = variant.ID
	}

	variant.Lens = strings.TrimSpace(variant.Lens)
	if variant.Lens == "" {
		variant.Lens = "independent proposal lane"
	}

	return variant
}

func normalizeCandidate(candidate Candidate, variant Variant, index int) Candidate {
	candidate.ID = strings.TrimSpace(candidate.ID)
	if candidate.ID == "" {
		candidate.ID = fmt.Sprintf("%s-%d", variant.ID, index+1)
	}

	candidate.Title = strings.TrimSpace(candidate.Title)
	candidate.Summary = strings.TrimSpace(candidate.Summary)
	candidate.Variant = variant.ID
	candidate.OriginalIndex = index

	return candidate
}

func candidateKey(title string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(title)) {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r):
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}

	return strings.Trim(b.String(), "-")
}

func appendUnique(values []string, value string) []string {
	if slices.Contains(values, value) {
		return values
	}

	return append(values, value)
}

func minPositive(left, right int) int {
	switch {
	case left <= 0:
		return right
	case right <= 0:
		return left
	default:
		return min(left, right)
	}
}

func rationaleFor(candidate RankedCandidate) string {
	signals := []string{
		fmt.Sprintf("score=%d", candidate.Score),
		fmt.Sprintf("fit=%d", candidate.Fit),
		fmt.Sprintf("feasibility=%d", candidate.Feasibility),
	}
	if candidate.Evidence > 0 {
		signals = append(signals, fmt.Sprintf("evidence=%d", candidate.Evidence))
	}
	if len(candidate.SourceVariants) > 1 {
		signals = append(signals, fmt.Sprintf("supported_by=%d_variants", len(candidate.SourceVariants)))
	}

	return strings.Join(signals, ", ")
}

func cloneVariants(variants []Variant) []Variant {
	out := make([]Variant, len(variants))
	for i := range variants {
		out[i] = variants[i]
		out[i].Candidates = append([]Candidate(nil), variants[i].Candidates...)
	}

	return out
}
