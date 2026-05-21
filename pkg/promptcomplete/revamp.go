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
	if hasEnoughPromptStructure(input) {
		return input
	}

	sections := []string{input}
	lower := strings.ToLower(input)

	if !hasAny(lower, "goal:", "objective:", "task:") {
		sections = append(sections, "Goal: "+sentenceFromPrompt(input))
	}

	if !hasAny(lower, "context:", "background:") {
		sections = append(sections, "Context to add: relevant files, errors, prior attempts, or session state.")
	}

	if !hasAny(lower, "constraint:", "constraints:", "requirements:", "must ", "avoid ") {
		sections = append(sections, "Constraints to preserve: scope, safety, and behavior that must not change.")
	}

	if !hasAny(lower, "output format:", "format:", "respond with", "return ") {
		sections = append(sections, "Output: the concrete answer, patch, or verification evidence expected.")
	}

	return strings.Join(sections, "\n")
}

func hasEnoughPromptStructure(input string) bool {
	lower := strings.ToLower(input)
	sections := 0

	if hasAny(lower, "goal:", "objective:", "task:") {
		sections++
	}

	if hasAny(lower, "context:", "background:") {
		sections++
	}

	if hasAny(lower, "constraint:", "constraints:", "requirements:", "must ", "avoid ") {
		sections++
	}

	if hasAny(lower, "output format:", "format:", "respond with", "return ", "deliverable:") {
		sections++
	}

	return sections >= 2 || bulletLineCount(input) >= 2
}

func bulletLineCount(input string) int {
	count := 0

	for line := range strings.SplitSeq(input, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "- ") || strings.HasPrefix(line, "* ") {
			count++
		}
	}

	return count
}

func sentenceFromPrompt(input string) string {
	sentence := compactWhitespace(input)

	sentence = strings.TrimRight(sentence, ".!?")
	if sentence == "" {
		return "state the desired outcome."
	}

	return sentence + "."
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
