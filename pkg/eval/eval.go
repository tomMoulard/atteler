// Package eval provides text, structured, and workflow helpers for checking
// agent outputs and side effects.
package eval

import (
	"fmt"
	"strings"
)

// MatchMode controls how an actual agent output is compared to expected text.
type MatchMode string

const (
	// ModeExact requires byte-for-byte equality.
	ModeExact MatchMode = "exact"
	// ModeContains requires the actual output to contain the expected text.
	ModeContains MatchMode = "contains"
	// ModeNormalized compares text after lowercasing and collapsing whitespace.
	ModeNormalized MatchMode = "normalized"
)

// Result describes the outcome of one output comparison.
type Result struct {
	Mode     MatchMode
	Expected string
	Actual   string
	Summary  string
	Diff     string
	Passed   bool
}

// Failure returns a compact human-readable failure report. It returns an empty
// string when the comparison passed.
func (r Result) Failure() string {
	if r.Passed {
		return ""
	}

	if r.Diff == "" {
		return r.Summary
	}

	return r.Summary + "\n" + r.Diff
}

// Check compares actual output to expected text using mode.
func Check(actual, expected string, mode MatchMode) Result {
	result := Result{Mode: mode, Expected: expected, Actual: actual}

	switch mode {
	case ModeExact:
		result.Passed = actual == expected
		if !result.Passed {
			result.Summary = fmt.Sprintf("expected exact match (%d chars), got %d chars", len(expected), len(actual))
			result.Diff = diffSnippet(expected, actual)
		}
	case ModeContains:
		result.Passed = strings.Contains(actual, expected)
		if !result.Passed {
			result.Summary = fmt.Sprintf("expected output to contain %q", compact(Redact(expected), 80))
			result.Diff = containsSnippet(expected, actual)
		}
	case ModeNormalized:
		normalizedExpected := Normalize(expected)
		normalizedActual := Normalize(actual)

		result.Passed = normalizedActual == normalizedExpected
		if !result.Passed {
			result.Summary = "expected normalized match"
			result.Diff = diffSnippet(normalizedExpected, normalizedActual)
		}
	default:
		result.Summary = fmt.Sprintf("unsupported match mode %q", mode)
	}

	return result
}

// Match reports whether actual output satisfies expected text using mode.
func Match(actual, expected string, mode MatchMode) bool {
	return Check(actual, expected, mode).Passed
}

// Normalize lowercases text, trims leading and trailing whitespace, and
// collapses all whitespace runs to a single ASCII space.
func Normalize(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

func containsSnippet(expected, actual string) string {
	return fmt.Sprintf("missing: %q\nactual:  %q", compact(Redact(expected), 120), compact(Redact(actual), 160))
}

func diffSnippet(expected, actual string) string {
	expected = Redact(expected)
	actual = Redact(actual)

	expectedRunes := []rune(expected)
	actualRunes := []rune(actual)
	prefix := commonPrefix(expectedRunes, actualRunes)
	suffix := commonSuffix(expectedRunes[prefix:], actualRunes[prefix:])

	const context = 40

	expectedStart := max(prefix-context, 0)
	actualStart := max(prefix-context, 0)
	expectedEnd := min(len(expectedRunes)-suffix, prefix+context)
	actualEnd := min(len(actualRunes)-suffix, prefix+context)

	if expectedStart > expectedEnd {
		expectedEnd = expectedStart
	}

	if actualStart > actualEnd {
		actualEnd = actualStart
	}

	return fmt.Sprintf(
		"first difference at rune %d\nexpected: %q\nactual:   %q",
		prefix,
		withEllipsis(expectedRunes, expectedStart, expectedEnd),
		withEllipsis(actualRunes, actualStart, actualEnd),
	)
}

func commonPrefix(a, b []rune) int {
	limit := min(len(a), len(b))
	for i := range limit {
		if a[i] != b[i] {
			return i
		}
	}

	return limit
}

func commonSuffix(a, b []rune) int {
	limit := min(len(a), len(b))
	for i := range limit {
		if a[len(a)-1-i] != b[len(b)-1-i] {
			return i
		}
	}

	return limit
}

func withEllipsis(runes []rune, start, end int) string {
	var builder strings.Builder
	if start > 0 {
		builder.WriteString("…")
	}

	builder.WriteString(string(runes[start:end]))

	if end < len(runes) {
		builder.WriteString("…")
	}

	return builder.String()
}

func compact(s string, limit int) string {
	s = strings.Join(strings.Fields(s), " ")

	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}

	if limit <= 1 {
		return "…"
	}

	return string(runes[:limit-1]) + "…"
}
