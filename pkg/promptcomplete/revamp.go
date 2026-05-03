package promptcomplete

import "strings"

// RevampStyle selects the deterministic local rewrite applied by Revamp.
type RevampStyle string

const (
	// RevampStyleDetailed expands a prompt with compact planning guidance.
	RevampStyleDetailed RevampStyle = "detailed"
	// RevampStyleConcise removes safe filler and compresses whitespace.
	RevampStyleConcise RevampStyle = "concise"
)

// Revamp improves a prompt locally without model calls.
//
// Blank input returns ok=false. Unknown styles use the detailed fallback so the
// TUI can keep one deterministic behavior for unsupported style values.
func Revamp(input string, style RevampStyle) (string, bool) {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return "", false
	}

	if style == RevampStyleConcise {
		return conciseRevamp(trimmed), true
	}

	return detailedRevamp(trimmed), true
}

func conciseRevamp(input string) string {
	compact := compactWhitespace(input)
	lower := strings.ToLower(compact)

	for _, prefix := range []string{"please ", "can you ", "could you "} {
		if strings.HasPrefix(lower, prefix) {
			return strings.TrimLeft(compact[len(prefix):], " ,:;-\t")
		}
	}

	return compact
}

func detailedRevamp(input string) string {
	sections := []string{input}
	lower := strings.ToLower(input)

	if !hasAny(lower, "goal:", "objective:", "task:") {
		sections = append(sections, "Goal: clarify the desired outcome.")
	}
	if !hasAny(lower, "context:", "background:") {
		sections = append(sections, "Context: include relevant background or inputs.")
	}
	if !hasAny(lower, "constraint:", "constraints:", "requirements:", "must ", "avoid ") {
		sections = append(sections, "Constraints: note limits, preferences, and must-haves.")
	}
	if !hasAny(lower, "output format:", "format:", "respond with", "return ") {
		sections = append(sections, "Output format: specify the expected structure.")
	}

	return strings.Join(sections, "\n")
}

func compactWhitespace(input string) string {
	return strings.Join(strings.Fields(input), " ")
}

func hasAny(input string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(input, needle) {
			return true
		}
	}
	return false
}
